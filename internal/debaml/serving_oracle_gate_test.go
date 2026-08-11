package debaml

// The BOUNDARY LOCK at the gate functions themselves, and the structural proof
// that the serving oracle is test-only.
//
// This file carries NO build tag on purpose. The integration oracle needs CGO and
// the stock BAML CFFI; the invariant it rests on must not. Everything here runs in
// the ordinary `go test ./internal/debaml/...` gate, so a change that widened
// admission would fail before anyone reached for a tagged run.
//
// It drives checkSupported, checkSupportedFields and checkSupportedType BY NAME,
// which the integration oracle cannot do as directly: those are unexported and the
// oracle reaches them through Parse / SupportsNativeFinalBundle / ParseStaticBundle.
// Both directions matter — the exported entry points are what production actually
// calls, and the three internal functions are where the decision is made — so the
// invariant is asserted at both.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// servingOracleGateFamilies is the shared vocabulary of shape families.
//
// It is declared HERE, in the untagged half, and consumed by the integration
// corpus: TestServingOracleCorpusIsWellFormed requires every family below to have
// at least one oracle fixture AND every fixture's family to appear below. Neither
// list can drift away from the other without a failure.
var servingOracleGateFamilies = []string{
	"scalar", "enum", "class", "list", "map", "union",
	"alias", "target", "asymmetry", "guard", "decline", "error", "control",
}

// servingOracleGateRow is one shape the three gate functions are driven over.
type servingOracleGateRow struct {
	Family string
	What   string
	Bundle *schema.Bundle
	// TypeNode is the type node checkSupportedType must decline on its own, when
	// the constraint lives on a TYPE rather than on a class/enum declaration. Nil
	// where the constraint is declared on a ClassDef/EnumDef, because
	// checkSupportedType never sees those — claiming otherwise would be asserting a
	// decline the function is not responsible for.
	TypeNode *schema.Type
}

// servingOracleGateRows covers every family in servingOracleGateFamilies.
func servingOracleGateRows() []servingOracleGateRow {
	label := func(s string) *string { return &s }
	check := func(t schema.Type, l, expr string) schema.Type {
		t.Meta.Constraints = append(t.Meta.Constraints,
			schema.Constraint{Level: schema.ConstraintCheck, Expression: expr, Label: label(l)})
		return t
	}
	field := func(name string, t schema.Type) schema.ClassField {
		return schema.ClassField{Name: schema.Name{Name: name}, Type: t}
	}
	aliased := func(name, alias string, t schema.Type) schema.ClassField {
		a := alias
		return schema.ClassField{Name: schema.Name{Name: name, Alias: &a}, Type: t}
	}
	indexed := func(b *schema.Bundle) *schema.Bundle {
		if err := b.RebuildIndexes(); err != nil {
			panic("serving oracle gate: " + err.Error())
		}
		return b
	}
	cls := func(name string, fields []schema.ClassField, cs ...schema.Constraint) schema.ClassDef {
		return schema.ClassDef{Name: schema.Name{Name: name}, Mode: schema.NonStreaming,
			Fields: fields, Constraints: cs}
	}
	rootOf := func(name string) schema.Type {
		return schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming}
	}
	oneField := func(name string, t schema.Type, cs ...schema.Constraint) *schema.Bundle {
		return indexed(&schema.Bundle{Target: rootOf(name),
			Classes: []schema.ClassDef{cls(name, []schema.ClassField{field("v", t)}, cs...)}})
	}

	scalarT := check(intType(), "gt", "this > 0")
	listElemT := schema.Type{Kind: schema.TypeList, Elem: ptr(check(intType(), "pos", "this > 0"))}
	mapValT := schema.Type{Kind: schema.TypeMap, Key: ptr(stringType()),
		Value: ptr(check(intType(), "pos", "this > 0"))}
	mapKeyT := schema.Type{Kind: schema.TypeMap, Key: ptr(check(stringType(), "k", "this|length > 0")),
		Value: ptr(intType())}
	unionT := schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{
		Variants: []schema.Type{check(intType(), "pos", "this > 0"), stringType()}}}
	guardT := check(intType(), "big", "this > 9007199254740992")
	declineT := check(stringType(), "fmt", `"{:,}".format(1) == "1"`)
	errT := check(intType(), "uf", "this|nosuchfilter")
	dupT := intType()
	dupT.Meta.Constraints = []schema.Constraint{
		{Level: schema.ConstraintCheck, Expression: "this > 0", Label: label("dup")},
		{Level: schema.ConstraintCheck, Expression: "this > 100", Label: label("dup")},
	}
	targetT := check(stringType(), "eq", `this == "expected"`)
	targetListT := schema.Type{Kind: schema.TypeList, Elem: ptr(check(intType(), "pos", "this > 0"))}

	enumBundle := indexed(&schema.Bundle{
		Target:  rootOf("GateEnumCls"),
		Classes: []schema.ClassDef{cls("GateEnumCls", []schema.ClassField{field("c", schema.Type{Kind: schema.TypeEnum, Name: "GateSuit"})})},
		Enums: []schema.EnumDef{{
			Name:   schema.Name{Name: "GateSuit"},
			Values: []schema.EnumValue{{Name: schema.Name{Name: "Hearts"}}, {Name: schema.Name{Name: "Spades"}}},
			Constraints: []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: `this != "Hearts"`, Label: label("not_hearts")}},
		}},
	})
	aliasBundle := indexed(&schema.Bundle{
		Target: rootOf("GateAliasCls"),
		Classes: []schema.ClassDef{cls("GateAliasCls",
			[]schema.ClassField{aliased("amount", "qty", intType())},
			schema.Constraint{Level: schema.ConstraintCheck, Expression: "this.amount == 3", Label: label("amt")})},
	})
	classLevelBundle := indexed(&schema.Bundle{
		Target: rootOf("GateClsLevel"),
		Classes: []schema.ClassDef{cls("GateClsLevel",
			[]schema.ClassField{field("s", stringType())},
			schema.Constraint{Level: schema.ConstraintCheck, Expression: "this.s|length > 0", Label: label("has_s")})},
	})

	return []servingOracleGateRow{
		{"scalar", "@check on a scalar field", oneField("GateScalar", scalarT), &scalarT},
		{"enum", "enum-level @check", enumBundle, nil},
		{"class", "class-level @check", classLevelBundle, nil},
		{"list", "@check on a list ELEMENT", oneField("GateList", listElemT), &listElemT},
		{"map", "@check on a map VALUE", oneField("GateMapVal", mapValT), &mapValT},
		{"map", "@check on a map KEY", oneField("GateMapKey", mapKeyT), &mapKeyT},
		{"union", "@check on a union ARM", oneField("GateUnion", unionT), &unionT},
		{"alias", "class-level @check over an ALIASED field", aliasBundle, nil},
		{"target", "@check on the RETURN TYPE itself", indexed(&schema.Bundle{Target: targetT}), &targetT},
		{"target", "@check on a target LIST ELEMENT", indexed(&schema.Bundle{Target: targetListT}), &targetListT},
		{"asymmetry", "two @check attributes under ONE label", oneField("GateDup", dupT), &dupT},
		{"guard", "a large-number predicate", oneField("GateGuard", guardT), &guardT},
		{"decline", "a predicate native refuses to evaluate", oneField("GateDecline", declineT), &declineT},
		{"error", "a predicate whose evaluation fails", oneField("GateError", errT), &errT},
	}
}

// TestServingOracleGateDeclinesEveryFamily drives the three gate functions by name
// over every constraint-bearing family.
//
// It is the UNNARROWED invariant: nothing is carved out, target-level included.
func TestServingOracleGateDeclinesEveryFamily(t *testing.T) {
	rows := servingOracleGateRows()
	if len(rows) == 0 {
		t.Fatal("no gate rows; the boundary lock would be vacuous")
	}
	covered := map[string]int{}
	typeChecked := 0
	for _, r := range rows {
		covered[r.Family]++
		t.Run(r.Family+"/"+r.What, func(t *testing.T) {
			if err := checkSupported(r.Bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("checkSupported returned %v, want ErrDeBAMLParseUnsupported for %s", err, r.What)
			}
			if err := checkSupportedFields(r.Bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("checkSupportedFields returned %v, want ErrDeBAMLParseUnsupported for %s", err, r.What)
			}
			if err := SupportsNativeFinalBundle(r.Bundle); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("SupportsNativeFinalBundle returned %v, want ErrDeBAMLParseUnsupported for %s", err, r.What)
			}
			if r.TypeNode == nil {
				return
			}
			typeChecked++
			if err := checkSupportedType(r.Bundle, *r.TypeNode); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("checkSupportedType returned %v, want ErrDeBAMLParseUnsupported for %s", err, r.What)
			}
		})
	}
	if typeChecked == 0 {
		t.Fatal("no row drove checkSupportedType; that function would be unasserted")
	}
	// Every family except the control must be represented, and the control family is
	// asserted in the opposite direction by TestServingOracleGateAdmitsUnconstrained.
	for _, fam := range servingOracleGateFamilies {
		if fam == "control" {
			continue
		}
		if covered[fam] == 0 {
			t.Errorf("family %q has no gate row", fam)
		}
	}
}

// TestServingOracleGateAdmitsUnconstrained is the control direction: the SAME
// shapes without constraints are admitted by all three functions.
//
// Without it, every assertion above would be satisfied by a gate that declined
// everything, which is not the invariant — the invariant is that CONSTRAINTS cause
// the decline.
func TestServingOracleGateAdmitsUnconstrained(t *testing.T) {
	rows := servingOracleGateRows()
	admitted := 0
	for _, r := range rows {
		stripped := servingOracleGateStrip(r.Bundle)
		t.Run(r.Family+"/"+r.What, func(t *testing.T) {
			if err := checkSupported(stripped); err != nil {
				t.Fatalf("the constraint-stripped twin of %s was DECLINED by checkSupported: %v", r.What, err)
			}
			if err := checkSupportedFields(stripped); err != nil {
				t.Fatalf("the constraint-stripped twin of %s was DECLINED by checkSupportedFields: %v", r.What, err)
			}
			if r.TypeNode != nil {
				if err := checkSupportedType(stripped, servingOracleGateStripType(*r.TypeNode)); err != nil {
					t.Fatalf("the constraint-stripped node of %s was DECLINED by checkSupportedType: %v",
						r.What, err)
				}
			}
			// SupportsNativeFinalBundle adds a STREAM cut-line on top of the constraint
			// cut-line, and it legitimately declines some unconstrained shapes (a single
			// string-absorbing-field root class, a scalar map value). Those are allowed
			// here — but only after their reason is checked NOT to be a constraint, so
			// the allowance cannot absorb a constraint decline.
			if err := SupportsNativeFinalBundle(stripped); err != nil {
				if strings.Contains(err.Error(), "constraint") {
					t.Fatalf("the constraint-stripped twin of %s was declined by SupportsNativeFinalBundle for "+
						"a CONSTRAINT reason (%v), but it carries none", r.What, err)
				}
				t.Logf("%s: the stripped twin is declined by the stream cut-line (%v); admission is still "+
					"proven by checkSupported and checkSupportedFields", r.What, err)
			}
		})
		admitted++
	}
	if admitted == 0 {
		t.Fatal("no control was checked; the constraint-specificity claim would be vacuous")
	}
}

// servingOracleGateStrip removes every constraint from a bundle.
//
// It PRESERVES every non-constraint fact, RecursiveClasses and the structural
// recursive aliases included. Rebuilding without them made the stripped twin a
// different bundle rather than a constraint-free clone: a fixture whose recursion
// independently causes a decline would have had that fact erased, and its
// "constraints caused the decline" attribution would have been about a shape the
// gate never saw.
func servingOracleGateStrip(b *schema.Bundle) *schema.Bundle {
	out := &schema.Bundle{
		Target:           servingOracleGateStripType(b.Target),
		RecursiveClasses: append([]string(nil), b.RecursiveClasses...),
	}
	for _, a := range b.StructuralRecursiveAliases {
		out.StructuralRecursiveAliases = append(out.StructuralRecursiveAliases,
			schema.RecursiveAliasDef{Name: a.Name, Target: servingOracleGateStripType(a.Target)})
	}
	for _, c := range b.Classes {
		nc := c
		nc.Constraints = nil
		nc.Fields = nil
		for _, f := range c.Fields {
			nf := f
			nf.Type = servingOracleGateStripType(f.Type)
			nc.Fields = append(nc.Fields, nf)
		}
		out.Classes = append(out.Classes, nc)
	}
	for _, e := range b.Enums {
		ne := e
		ne.Constraints = nil
		out.Enums = append(out.Enums, ne)
	}
	if err := out.RebuildIndexes(); err != nil {
		panic("serving oracle gate strip: " + err.Error())
	}
	return out
}

// servingOracleGateStripType removes the constraints from a type and EVERY type
// nested inside it — Items included, which was previously skipped, leaving a tuple
// element's constraint in the "stripped" twin.
func servingOracleGateStripType(t schema.Type) schema.Type {
	out := t
	out.Meta.Constraints = nil
	if t.Elem != nil {
		out.Elem = ptr(servingOracleGateStripType(*t.Elem))
	}
	if t.Key != nil {
		out.Key = ptr(servingOracleGateStripType(*t.Key))
	}
	if t.Value != nil {
		out.Value = ptr(servingOracleGateStripType(*t.Value))
	}
	if len(t.Items) > 0 {
		out.Items = nil
		for _, it := range t.Items {
			out.Items = append(out.Items, servingOracleGateStripType(it))
		}
	}
	if t.Arrow != nil {
		a := *t.Arrow
		a.Return = servingOracleGateStripType(t.Arrow.Return)
		a.Params = nil
		for _, p := range t.Arrow.Params {
			a.Params = append(a.Params, servingOracleGateStripType(p))
		}
		out.Arrow = &a
	}
	if t.Union != nil {
		u := *t.Union
		u.Variants = nil
		for _, v := range t.Union.Variants {
			u.Variants = append(u.Variants, servingOracleGateStripType(v))
		}
		out.Union = &u
	}
	return out
}

// ---------------------------------------------------------------------------
// Structural proofs
// ---------------------------------------------------------------------------

// servingOracleSourceGlob matches the oracle's own files.
const servingOracleSourceGlob = "serving_oracle_*.go"

// servingOracleAnchors must EXIST, and may only be declared in a _test.go file.
//
// Named anchors rather than a filename check: renaming a file must not be able to
// disable the guard, and a missing anchor means the oracle was gutted rather than
// that the rule passed.
var servingOracleAnchors = []string{
	"servingOracleFixtures", "servingOracleFixture", "servingOracleGateRows",
	"soRenderProject", "soRunNative", "soCompare", "soDriveStock", "soReadStockValue",
	"soClassObserver", "soChecked", "soStockEnvelope", "soNativeEnvelope",
	"soAlignNativePath", "soBuildTypeMap", "soEnsureRuntime", "soStripConstraints",
}

// servingOracleReservedPrefixes is the namespace the oracle declares into. Every
// identifier the oracle declares must match one, and no production file may
// declare or reference one of the oracle's identifiers.
var servingOracleReservedPrefixes = []string{"so", "servingOracle", "TestServingOracle"}

// TestServingOracleIsTestOnly is the STRUCTURAL proof — a go/ast walk, not a
// source-text grep — that nothing in this slice can be linked into a production
// binary.
//
// Three rules, each with a direction that can fail:
//
//  1. every anchor exists, and only in _test.go files;
//  2. every identifier the oracle declares is inside the reserved namespace, so
//     rule 3's scan cannot be evaded by naming something outside it;
//  3. no non-test .go file anywhere in the repository declares or references any
//     identifier the oracle declares.
func TestServingOracleIsTestOnly(t *testing.T) {
	repo := servingOracleRepoRoot(t)
	declaredHere, err := servingOracleOwnDeclarations(repo)
	if err != nil {
		t.Fatalf("index the oracle's own declarations: %v", err)
	}
	if len(declaredHere) == 0 {
		t.Fatal("the oracle declares no identifiers; the scan below would be vacuous")
	}

	// Rule 1: anchors exist, in _test.go only.
	if len(servingOracleAnchors) == 0 {
		t.Fatal("servingOracleAnchors is empty; rule 1 would be satisfied trivially")
	}
	index, err := servingOracleDeclarationIndex(repo)
	if err != nil {
		t.Fatalf("index repository declarations: %v", err)
	}
	for _, a := range servingOracleAnchors {
		files := index[a]
		if len(files) == 0 {
			t.Errorf("anchor %q is declared nowhere; the oracle was renamed or removed and this guard no "+
				"longer describes it", a)
			continue
		}
		for _, rel := range files {
			if !strings.HasSuffix(rel, "_test.go") {
				t.Errorf("anchor %q is declared in the PRODUCTION file %s; the serving oracle must be "+
					"test-only", a, rel)
			}
		}
	}

	// Rule 2: the oracle's own namespace is real.
	if len(servingOracleReservedPrefixes) == 0 {
		t.Fatal("servingOracleReservedPrefixes is empty; rule 2 would be satisfied trivially")
	}
	for name := range declaredHere {
		if !servingOracleHasReservedPrefix(name) {
			t.Errorf("the oracle declares %q, which is outside its reserved namespace %v; move it inside so "+
				"the no-production-caller scan cannot be evaded", name, servingOracleReservedPrefixes)
		}
	}

	// Rule 3: no production file declares or references any of them.
	hits, err := servingOracleProductionReferences(repo, declaredHere)
	if err != nil {
		t.Fatalf("scan for production references: %v", err)
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		t.Fatalf("the serving oracle is reachable from PRODUCTION code:\n  %s", strings.Join(hits, "\n  "))
	}
	t.Logf("test-only: %d oracle identifiers, %d anchors, no production declaration or reference",
		len(declaredHere), len(servingOracleAnchors))
}

// TestServingOracleTestOnlyGuardBites is the guard's own bite check.
//
// It proves the scan CLASSIFIES rather than merely returns nothing: a synthetic
// production file that references an anchor is detected, and a real production
// identifier is not mistaken for one of the oracle's.
func TestServingOracleTestOnlyGuardBites(t *testing.T) {
	dir := t.TempDir()
	synthetic := filepath.Join(dir, "fake_production.go")
	src := "package fake\n\nfunc use() { _ = servingOracleFixtures }\n"
	if err := os.WriteFile(synthetic, []byte(src), 0o644); err != nil {
		t.Fatalf("write synthetic production file: %v", err)
	}
	hits, err := servingOracleProductionReferences(dir, map[string]struct{}{"servingOracleFixtures": {}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("the scan did not detect a production file referencing servingOracleFixtures; it cannot be " +
			"relied on to detect a real one")
	}
	// NEGATIVE control: a production identifier that is genuinely production must
	// not be swallowed by the reserved namespace.
	for _, name := range []string{"EvaluateConstraint", "SupportsNativeFinalBundle", "ParseStaticBundle"} {
		if servingOracleHasReservedPrefix(name) {
			t.Errorf("the reserved namespace swallows the PRODUCTION identifier %q", name)
		}
		repo := servingOracleRepoRoot(t)
		index, err := servingOracleDeclarationIndex(repo)
		if err != nil {
			t.Fatalf("index: %v", err)
		}
		if !servingOracleDeclaredByProduction(index, name) {
			t.Errorf("%q has no PRODUCTION declaration; the negative control is stale", name)
		}
	}
}

// TestServingOracleGateReadsNoEnvironment is the STRUCTURAL half of "the flag does
// not alter the decision".
//
// The integration boundary lock sets BAML_REST_USE_DEBAML both ways around every
// gate call, which shows the outcome does not change. This shows WHY it cannot: no
// production file under internal/debaml or internal/schema reads the process
// environment at all, so no gate decision can be conditioned on any flag.
//
// It walks RECURSIVELY and resolves the import, because the earlier version could
// have stayed green through a real regression: it globbed only each directory root
// (missing nested production packages such as bamlunicode and outputformat), it
// matched an identifier literally spelled `os` rather than whatever name the file
// imports "os" under, and it knew only three of the environment-read APIs.
func TestServingOracleGateReadsNoEnvironment(t *testing.T) {
	pkgs := servingOracleProductionPackages(t, ".", "../schema")
	if len(pkgs) < 3 {
		t.Fatalf("the scan covered %d package director(ies) %v; it is meant to reach NESTED production "+
			"packages (internal/debaml/bamlunicode, internal/schema/outputformat) and not only the two roots",
			len(pkgs), servingOraclePackageDirs(pkgs))
	}
	if len(servingOracleEnvAPIs) == 0 {
		t.Fatal("servingOracleEnvAPIs is empty; the scan would look for nothing")
	}
	for pkg, api := range servingOracleEnvAPIs {
		if len(api) == 0 {
			t.Fatalf("the environment API set for %q is empty; that package would be watched for nothing", pkg)
		}
	}

	scanned := 0
	var hits []string
	for dir, pkg := range pkgs {
		uses := servingOracleProductionBindings(t, pkg, dir)
		for path, file := range pkg.Files {
			scanned++
			hits = append(hits, servingOracleEnvHits(path, file, uses)...)
		}
	}
	if scanned == 0 {
		t.Fatal("no production file was scanned; the claim would be vacuous")
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		t.Fatalf("production code behind the native gate reads the environment, so a flag COULD alter the "+
			"admission decision:\n  %s", strings.Join(hits, "\n  "))
	}
	t.Logf("no environment read in %d production files across %d packages under internal/debaml and "+
		"internal/schema (bindings resolved with go/types)", scanned, len(pkgs))
}

// servingOraclePackage is one production package's parsed non-test files.
type servingOraclePackage struct {
	Fset  *token.FileSet
	Files map[string]*ast.File
}

// ---------------------------------------------------------------------------
// Memoized fixtures
//
// Parsing 49 production files, type-checking six packages, and walking every .go
// file in the repository are the three expensive things this file does — and all
// three are functions of the SOURCE TREE, which does not change while a test binary
// runs. Recomputing them per iteration made `go test -race -count=100` dominate the
// unit-tests lane's per-package budget (see the diagnosis in the Slice 7.2a-3
// report).
//
// They are therefore computed ONCE per process and shared. Nothing is weakened: the
// assertions still run in full on every iteration, over the same trees; only the
// derived, read-only inputs are reused. Every consumer treats them as immutable —
// ast.Inspect and types.Info lookups are reads — and the package's tests do not run
// in parallel.
// ---------------------------------------------------------------------------

var (
	servingOracleFixtureMu sync.Mutex

	// Keyed by the ROOTS scanned, not a single flag. A second caller naming
	// different roots would otherwise be handed the first caller's packages and its
	// scan would prove nothing about the trees it asked for — the same cache-key
	// unsoundness that made a re-parsed probe read a stale binding map.
	servingOracleProdPkgsCache = map[string]servingOracleProdPkgsEntry{}

	// Bindings are keyed by *ast.Ident POINTERS, so a binding map is only meaningful
	// for the exact AST it was computed from. Both caches below therefore hold the
	// PARSED FILES together with their bindings; caching bindings alone against a
	// name that gets re-parsed would hand the detector a map none of the new
	// identifiers appear in, and it would (correctly, but uselessly) fail closed on
	// every selector.
	//
	// For production packages that means keying on the PARSE ITSELF rather than on the
	// directory: now that the parse cache above is keyed by roots, one directory can
	// legitimately have several distinct *servingOraclePackage values live at once
	// (`.` parsed under {"."} is a different AST from `.` parsed under {".",
	// "../schema"}). A directory-keyed binding cache would hand the second one the
	// first one's map, in which none of its identifiers appear.
	servingOracleProdBindings = map[servingOracleBindKey]servingOracleBindings{}
	servingOracleProbeCache   = map[string]*servingOracleProbeFixture{}

	servingOracleTreeCache = map[string][]servingOracleParsedFile{}
	servingOracleTreeErr   = map[string]error{}
)

// servingOracleProdPkgsEntry is one memoized production-package parse.
type servingOracleProdPkgsEntry struct {
	Pkgs map[string]*servingOraclePackage
	Err  error
}

// servingOracleBindKey identifies the bindings of one PARSE of one package.
//
// Pkg is the identity that matters — a binding map's keys are identifiers inside
// that exact AST — and it alone makes the key sound. Dir rides along because it is
// the package path go/types was configured with, so a hypothetical caller checking
// one AST under two package paths also gets its own entry rather than the other's.
type servingOracleBindKey struct {
	Pkg *servingOraclePackage
	Dir string
}

// servingOracleBindings is one memoized type-check result: the resolved uses and
// whether go/types produced any bindings at all.
type servingOracleBindings struct {
	Uses map[*ast.Ident]types.Object
	OK   bool
}

// servingOracleParsedFile is one parsed file of a scanned tree.
type servingOracleParsedFile struct {
	// Rel is the slash-separated path relative to the scanned root.
	Rel  string
	File *ast.File
}

// servingOracleProbeFixture is one synthetic probe: the AST and the bindings that
// belong to it, kept together for the reason above.
type servingOracleProbeFixture struct {
	File *ast.File
	Uses map[*ast.Ident]types.Object
	OK   bool
}

// servingOracleProductionPackages parses every production .go file under the given
// roots, grouped by directory so each package can be type-checked as a unit.
//
// testdata is skipped: it is never built, and a generated oracle client under it is
// not production code.
func servingOracleProductionPackages(t *testing.T, roots ...string) map[string]*servingOraclePackage {
	t.Helper()
	servingOracleFixtureMu.Lock()
	defer servingOracleFixtureMu.Unlock()
	key := strings.Join(roots, "\x00")
	if entry, done := servingOracleProdPkgsCache[key]; done {
		if entry.Err != nil {
			t.Fatalf("parse the production packages %v: %v", roots, entry.Err)
		}
		return entry.Pkgs
	}
	out, err := servingOracleParseProductionPackages(roots...)
	servingOracleProdPkgsCache[key] = servingOracleProdPkgsEntry{Pkgs: out, Err: err}
	if err != nil {
		t.Fatalf("parse the production packages %v: %v", roots, err)
	}
	return out
}

// servingOracleParseProductionPackages does the parsing itself.
func servingOracleParseProductionPackages(roots ...string) (map[string]*servingOraclePackage, error) {
	out := map[string]*servingOraclePackage{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			dir := filepath.Dir(path)
			pkg := out[dir]
			if pkg == nil {
				pkg = &servingOraclePackage{Fset: token.NewFileSet(), Files: map[string]*ast.File{}}
				out[dir] = pkg
			}
			file, perr := parser.ParseFile(pkg.Fset, path, nil, 0)
			if perr != nil {
				// Fail closed: an unreadable file is not evidence that it reads nothing.
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			pkg.Files[path] = file
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", root, err)
		}
	}
	return out, nil
}

func servingOraclePackageDirs(pkgs map[string]*servingOraclePackage) []string {
	out := make([]string, 0, len(pkgs))
	for dir := range pkgs {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// servingOracleResolveBindings type-checks one package and returns the resolved
// identifier bindings.
//
// WHY TYPE-CHECKING RATHER THAN SPELLING. The question the scan has to answer is
// whether the `X` in `X.Getenv(...)` still DENOTES the imported os package at that
// point in the source. Comparing X's spelling to the import's name gets that wrong
// for ordinary, compilable Go:
//
//	import env "os"
//	var _ = env.Args                       // genuinely the import
//	func f() string { var env local; return env.Getenv("X") }   // NOT the import
//
// go/types answers it exactly: the shadowed use resolves to a *types.Var, the real
// one to a *types.PkgName whose imported path is "os".
//
// WHAT THE IMPORTER DOES, AND WHY A STUB IS ENOUGH. Every import resolves to a
// STUB package carrying the real import PATH — this module's own dependencies
// cannot be loaded by the stdlib importers in module mode, and they do not need to
// be. The question is only which OBJECT a qualifier denotes: go/types creates the
// *types.PkgName from the import declaration itself, so `os` (or an alias of it)
// binds to a PkgName whose Imported().Path() is "os" whether or not that package's
// contents were loaded, while a shadowing local binds to a *types.Var. Selector
// lookups INTO a stub fail, and those errors are irrelevant here — but the detector
// never relies on them, and it FAILS CLOSED on any selector whose binding it could
// not resolve.
//
// The returned bool is false when type-checking produced no bindings at all, which
// the caller must treat as a hard failure rather than a clean package.
func servingOracleResolveBindings(pkg *servingOraclePackage, dir string) (map[*ast.Ident]types.Object, bool) {
	files := make([]*ast.File, 0, len(pkg.Files))
	for _, f := range pkg.Files {
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	cfg := &types.Config{
		Importer: servingOracleImporter{},
		// Errors are collected rather than fatal: a stubbed dependency makes every
		// selector into it undefined, and none of that bears on identifier binding.
		Error: func(error) {},
	}
	_, _ = cfg.Check(dir, pkg.Fset, files, info)
	return info.Uses, len(info.Uses) > 0
}

// servingOracleProductionBindings memoizes the bindings of a PRODUCTION package
// against the exact parse they were computed from, so the pointers always match:
// the map returned for a package is always the one go/types produced for THAT
// package's files. See servingOracleBindKey.
func servingOracleProductionBindings(t *testing.T, pkg *servingOraclePackage, dir string) map[*ast.Ident]types.Object {
	t.Helper()
	key := servingOracleBindKey{Pkg: pkg, Dir: dir}
	servingOracleFixtureMu.Lock()
	entry, done := servingOracleProdBindings[key]
	servingOracleFixtureMu.Unlock()
	uses, ok := entry.Uses, entry.OK
	if !done {
		uses, ok = servingOracleResolveBindings(pkg, dir)
		servingOracleFixtureMu.Lock()
		servingOracleProdBindings[key] = servingOracleBindings{Uses: uses, OK: ok}
		servingOracleFixtureMu.Unlock()
	}
	if !ok {
		t.Fatalf("go/types resolved no identifier bindings for %s (%d file(s)); the environment guard "+
			"cannot answer what any selector denotes and must not report the package clean",
			dir, len(pkg.Files))
	}
	return uses
}

// servingOracleImporter resolves every import to a complete, empty stub carrying
// the real path. See servingOracleResolveBindings for why the contents are not
// needed.
type servingOracleImporter struct{}

func (servingOracleImporter) Import(path string) (*types.Package, error) {
	seg := path
	if idx := strings.LastIndex(seg, "/"); idx >= 0 {
		seg = seg[idx+1:]
	}
	p := types.NewPackage(path, seg)
	p.MarkComplete()
	return p, nil
}

// servingOracleEnvAPIs is the COMPLETE set of standard-library entry points, per
// package, through which code can reach the process environment.
//
// TWO PACKAGES, because one is not enough: `os`'s environment functions are thin
// wrappers over `syscall`'s, and a gate that called syscall.Getenv directly would
// read the very same environment while an os-only proof stayed green. That is a
// false green, not an alternate spelling.
//
// MUTATION IS INCLUDED alongside reading. A gate that WRITES the environment is
// conditioning behaviour on it just as surely, and including Setenv/Unsetenv/
// Clearenv costs nothing here — no production file behind the gate touches either.
//
// The sets are pinned by TestServingOracleEnvAPISetIsPinned, so a stdlib addition
// is a conscious edit rather than a silent gap.
// A SET, NOT A BOOLEAN MAP, and that is a proof property rather than a style
// choice. While the detector decided from a stored `bool`, flipping one entry to
// false silently disabled that API — and a key-only pin stayed green, because the
// name was still there. Membership has no value to flip.
var servingOracleEnvAPIs = map[string]map[string]struct{}{
	"os": {
		"Getenv": {}, "LookupEnv": {}, "Environ": {}, "ExpandEnv": {},
		"Setenv": {}, "Unsetenv": {}, "Clearenv": {},
	},
	"syscall": {
		"Getenv": {}, "Environ": {},
		"Setenv": {}, "Unsetenv": {}, "Clearenv": {},
	},
}

// servingOracleExpectedEnvAPIs is the INDEPENDENT expectation the pin compares
// against, and the source the per-entry positive probes are generated from.
//
// It is written out by hand on purpose. Generating the probes from the live
// allowlist would be self-referential — deleting an entry would delete its own
// probe and stay green — so both the exactness check and the "every entry is
// detectable" check are driven from here instead.
var servingOracleExpectedEnvAPIs = map[string][]string{
	"os":      {"Clearenv", "Environ", "ExpandEnv", "Getenv", "LookupEnv", "Setenv", "Unsetenv"},
	"syscall": {"Clearenv", "Environ", "Getenv", "Setenv", "Unsetenv"},
}

// servingOracleEnvPkgName is the identifier a DEFAULT import of path is qualified
// by: its last segment.
//
// It is the single source of truth for that derivation. The detector and the
// per-entry probe generator both call it, so they cannot disagree about how a
// watched package is spelled at a call site — they did, before this existed.
func servingOracleEnvPkgName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// servingOracleIsEnvAPI reports whether fn is an environment entry point of pkg.
func servingOracleIsEnvAPI(pkgPath, fn string) bool {
	_, ok := servingOracleEnvAPIs[pkgPath][fn]
	return ok
}

// servingOracleEnvImports resolves how a file imports each ENVIRONMENT package: the
// local names it may be qualified by, and which of them are DOT-imported.
//
// A dot import is reported separately because it cannot be scanned the same way —
// `Getenv("X")` is then a bare identifier indistinguishable from a call to any
// local function of that name, so the caller fails closed on it rather than
// concluding the file reads nothing. A blank import cannot be called at all.
func servingOracleEnvImports(file *ast.File) (names map[string]struct{}, dotImported []string) {
	names = map[string]struct{}{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if _, watched := servingOracleEnvAPIs[path]; !watched {
			continue
		}
		switch {
		case imp.Name == nil:
			// The package's own name qualifies it, and that is the LAST path segment
			// rather than the whole path. Registering the full path worked only
			// because "os" and "syscall" contain no slash; a watched multi-segment
			// path would never have matched its default-imported selectors while the
			// pin stayed green — the exact false-green shape this file exists to
			// prevent.
			names[servingOracleEnvPkgName(path)] = struct{}{}
		case imp.Name.Name == ".":
			dotImported = append(dotImported, path)
		case imp.Name.Name == "_":
			// Imported for its side effects only; it cannot be called.
		default:
			names[imp.Name.Name] = struct{}{}
		}
	}
	sort.Strings(dotImported)
	return names, dotImported
}

// servingOracleEnvHits reports every environment read this scan can see in one
// file, and every construct it CANNOT see through.
//
// It is the single detector the live scan and TestServingOracleEnvironmentScanBites
// both drive, so the bite test exercises the real code path rather than a copy that
// can drift away from it.
//
// A qualified selector is a hit only when its qualifier's RESOLVED BINDING is the
// imported os package — not when it merely shares the import's spelling. `uses` is
// go/types' Uses map for the file's package; a selector whose qualifier is missing
// from it cannot be judged, so it fails closed.
func servingOracleEnvHits(path string, file *ast.File, uses map[*ast.Ident]types.Object) []string {
	var hits []string
	qualifiers, dotImported := servingOracleEnvImports(file)
	if len(qualifiers) > 0 {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			// Only identifiers spelled like an environment-package import can BE one;
			// anything else is some other package or value and is not this guard's
			// business. The binding check below is what decides whether it actually is.
			if _, isEnvQualifier := qualifiers[qualifier.Name]; !isEnvQualifier {
				return true
			}
			obj, resolved := uses[qualifier]
			if !resolved {
				if !servingOracleAnyEnvAPI(sel.Sel.Name) {
					return true
				}
				hits = append(hits, fmt.Sprintf("%s: %s.%s — the qualifier's binding could not be "+
					"resolved, so this guard fails closed rather than assume it is not an environment "+
					"package", path, qualifier.Name, sel.Sel.Name))
				return true
			}
			pkgName, isPkg := obj.(*types.PkgName)
			if !isPkg {
				// SHADOWED: the identifier denotes a local value, not the import. This
				// is ordinary, compilable Go and must not be reported.
				return true
			}
			imported := pkgName.Imported().Path()
			if !servingOracleIsEnvAPI(imported, sel.Sel.Name) {
				return true
			}
			hits = append(hits, fmt.Sprintf("%s: %s.%s (package %q imported as %q)",
				path, qualifier.Name, sel.Sel.Name, imported, qualifier.Name))
			return true
		})
	}
	if len(dotImported) == 0 {
		return hits
	}
	// FAIL CLOSED on a dot import. An unqualified Getenv(...) is syntactically
	// identical to a call to any local function of that name, and there is no
	// qualifier whose binding could settle it, so this scan cannot prove the file
	// reads no environment — and "cannot prove" must not read as "proved it does
	// not". The bare call is named when one is present, so the report says what was
	// found rather than only that the shape is unscannable.
	detail := ""
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !servingOracleAnyEnvAPI(id.Name) {
			return true
		}
		detail = fmt.Sprintf(" — it calls %s(...) unqualified", id.Name)
		return false
	})
	hits = append(hits, fmt.Sprintf("%s: DOT-IMPORTS %v%s. An unqualified environment call is "+
		"indistinguishable from a call to any local function of the same name, so this guard fails "+
		"closed rather than claim the file reads nothing.", path, dotImported, detail))
	return hits
}

// servingOracleAnyEnvAPI reports whether a bare function name matches an
// environment entry point of ANY watched package. It is used only where the
// package cannot be established — a dot import, or an unresolvable qualifier —
// which is precisely where this guard fails closed.
func servingOracleAnyEnvAPI(fn string) bool {
	for _, api := range servingOracleEnvAPIs {
		if _, ok := api[fn]; ok {
			return true
		}
	}
	return false
}

// TestServingOracleEnvironmentScanBites proves the scan CLASSIFIES, in BOTH
// directions, over the shapes that distinguish a resolved binding from a spelling
// match — and over BOTH environment packages.
//
// It drives servingOracleEnvHits through the same type-checking path the live scan
// uses, so the two cannot drift apart.
func TestServingOracleEnvironmentScanBites(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		// -- os, positive ---------------------------------------------------
		{"a plain os.Getenv", "package p\n\nimport \"os\"\n\nfunc f() string { return os.Getenv(\"X\") }\n", true},
		{"os.LookupEnv", "package p\n\nimport \"os\"\n\nfunc f() bool { _, ok := os.LookupEnv(\"X\"); return ok }\n", true},
		{"os.Environ", "package p\n\nimport \"os\"\n\nfunc f() []string { return os.Environ() }\n", true},
		{"os.ExpandEnv", "package p\n\nimport \"os\"\n\nfunc f() string { return os.ExpandEnv(\"$X\") }\n", true},
		{"os.Setenv (mutation counts too)", "package p\n\nimport \"os\"\n\nfunc f() error { return os.Setenv(\"X\", \"1\") }\n", true},
		{"an ALIASED os import that really calls Getenv", "package p\n\nimport env \"os\"\n\nfunc f() string { return env.Getenv(\"X\") }\n", true},

		// -- syscall, positive: the package an os-only proof missed entirely --
		{"syscall.Getenv", "package p\n\nimport \"syscall\"\n\nfunc f() string { v, _ := syscall.Getenv(\"X\"); return v }\n", true},
		{"syscall.Environ", "package p\n\nimport \"syscall\"\n\nfunc f() []string { return syscall.Environ() }\n", true},
		{"syscall.Setenv", "package p\n\nimport \"syscall\"\n\nfunc f() error { return syscall.Setenv(\"X\", \"1\") }\n", true},
		{"an ALIASED syscall import that really calls Getenv", "package p\n\nimport sc \"syscall\"\n\nfunc f() string { v, _ := sc.Getenv(\"X\"); return v }\n", true},

		// -- dot imports of either package: unscannable, so fail closed ------
		{"a DOT import of os with a bare Getenv", "package p\n\nimport . \"os\"\n\nfunc f() string { return Getenv(\"BAML_REST_USE_DEBAML\") }\n", true},
		{"a DOT import of os with no visible env call", "package p\n\nimport . \"os\"\n\nfunc f() error { _, err := Open(\"x\"); return err }\n", true},
		{"a DOT import of syscall", "package p\n\nimport . \"syscall\"\n\nfunc f() []string { return Environ() }\n", true},

		// -- shadows: ordinary compilable Go that must NOT be reported -------
		{"a DEFAULT os import shadowed by a local value", "package p\n\nimport \"os\"\n\n" +
			"var _ = os.Args\n\ntype local struct{}\n\nfunc (local) Getenv(string) string { return \"x\" }\n\n" +
			"func f() string { var os local; return os.Getenv(\"X\") }\n", false},
		{"an ALIASED os import shadowed by a local value", "package p\n\nimport env \"os\"\n\n" +
			"var _ = env.Args\n\ntype local struct{}\n\nfunc (local) Getenv(string) string { return \"x\" }\n\n" +
			"func f() string { var env local; return env.Getenv(\"X\") }\n", false},
		{"a DEFAULT syscall import shadowed by a local value", "package p\n\nimport \"syscall\"\n\n" +
			"var _ = syscall.Stdin\n\ntype local struct{}\n\nfunc (local) Environ() []string { return nil }\n\n" +
			"func f() []string { var syscall local; return syscall.Environ() }\n", false},
		{"an ALIASED syscall import shadowed by a local value", "package p\n\nimport sc \"syscall\"\n\n" +
			"var _ = sc.Stdin\n\ntype local struct{}\n\nfunc (local) Getenv(string) (string, bool) { return \"\", false }\n\n" +
			"func f() string { var sc local; v, _ := sc.Getenv(\"X\"); return v }\n", false},

		// -- non-environment calls into the same packages --------------------
		{"os.Open is not an environment read", "package p\n\nimport \"os\"\n\nfunc f() error { _, err := os.Open(\"x\"); return err }\n", false},
		{"syscall.Kill is not an environment read", "package p\n\nimport \"syscall\"\n\nfunc f() error { return syscall.Kill(1, 0) }\n", false},

		{"a local value called os in a file with NO os import", "package p\n\ntype t struct{ Getenv func(string) string }\n\nfunc f() string { var os t; return os.Getenv(\"X\") }\n", false},
		{"a BLANK import cannot be called", "package p\n\nimport _ \"os\"\n\nfunc f() string { return \"\" }\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := servingOracleProbeEnvHits(t, tc.src)
			if (len(hits) > 0) != tc.want {
				t.Fatalf("detected=%v (%v), want %v", len(hits) > 0, hits, tc.want)
			}
		})
	}
	// The dot-import report must NAME the bare call when there is one, so the failure
	// tells its reader what was found rather than only that the shape is unscannable.
	hits := servingOracleProbeEnvHits(t,
		"package p\n\nimport . \"os\"\n\nfunc f() string { return Getenv(\"X\") }\n")
	if len(hits) != 1 || !strings.Contains(hits[0], "Getenv(...) unqualified") {
		t.Fatalf("the dot-import report does not name the bare call: %v", hits)
	}
	// And an UNRESOLVABLE qualifier fails closed rather than being read as "not the
	// import": the detector is handed an empty binding map for a file that really
	// does call the import.
	for _, src := range []string{
		"package p\n\nimport \"os\"\n\nfunc f() string { return os.Getenv(\"X\") }\n",
		"package p\n\nimport \"syscall\"\n\nfunc f() []string { return syscall.Environ() }\n",
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "probe.go", src, 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		unresolved := servingOracleEnvHits("probe.go", file, map[*ast.Ident]types.Object{})
		if len(unresolved) != 1 || !strings.Contains(unresolved[0], "could not be resolved") {
			t.Fatalf("an unresolvable qualifier must FAIL CLOSED; got %v", unresolved)
		}
	}
}

// TestServingOracleEnvAPISetIsPinned pins the watched surface exactly, so a
// standard-library addition — or a quiet deletion — is a conscious edit.
//
// It compares against [servingOracleExpectedEnvAPIs], which is written out
// independently of the live set, and it checks both directions: a package or a
// function present in one and not the other fails.
func TestServingOracleEnvAPISetIsPinned(t *testing.T) {
	if len(servingOracleExpectedEnvAPIs) != len(servingOracleEnvAPIs) {
		t.Fatalf("the scan watches %d package(s), the pin names %d",
			len(servingOracleEnvAPIs), len(servingOracleExpectedEnvAPIs))
	}
	for pkg, names := range servingOracleExpectedEnvAPIs {
		api, ok := servingOracleEnvAPIs[pkg]
		if !ok {
			t.Errorf("package %q is pinned but not watched", pkg)
			continue
		}
		got := make([]string, 0, len(api))
		for name := range api {
			got = append(got, name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("the %q environment API set is %v, want %v", pkg, got, names)
		}
	}
	for pkg := range servingOracleEnvAPIs {
		if _, ok := servingOracleExpectedEnvAPIs[pkg]; !ok {
			t.Errorf("package %q is watched but not pinned", pkg)
		}
	}
	// The membership helper must agree with the set in both directions, so the
	// detector's own predicate cannot drift from the table the pin checks.
	for pkg, names := range servingOracleExpectedEnvAPIs {
		for _, fn := range names {
			if !servingOracleIsEnvAPI(pkg, fn) {
				t.Errorf("servingOracleIsEnvAPI(%q, %q) is false for a pinned entry", pkg, fn)
			}
		}
	}
	for _, fn := range []string{"Open", "Kill", "Getpid", "Exit"} {
		for pkg := range servingOracleExpectedEnvAPIs {
			if servingOracleIsEnvAPI(pkg, fn) {
				t.Errorf("servingOracleIsEnvAPI(%q, %q) is true for a NON-environment function", pkg, fn)
			}
		}
	}
}

// TestServingOracleEveryEnvAPIEntryIsDetected proves EVERY pinned entry is
// load-bearing: each one is driven through the real go/types resolver and
// servingOracleEnvHits, and each must be a HIT.
//
// Before this, four of the twelve (os.Unsetenv/Clearenv, syscall.Unsetenv/Clearenv)
// were exercised by nothing at all, so disabling them was invisible to the whole
// suite. The probes are generated from the INDEPENDENT expectation rather than from
// the live allowlist: generating them from the allowlist would delete a deleted
// entry's own probe and stay green.
func TestServingOracleEveryEnvAPIEntryIsDetected(t *testing.T) {
	total := 0
	for pkg, names := range servingOracleExpectedEnvAPIs {
		if len(names) == 0 {
			t.Fatalf("package %q has no pinned entries; this test would prove nothing about it", pkg)
		}
		// The SAME derivation the detector uses, so the probe and the thing it
		// probes cannot disagree about the qualifier.
		qualifier := servingOracleEnvPkgName(pkg)
		for _, fn := range names {
			total++
			t.Run(pkg+"."+fn, func(t *testing.T) {
				// A bare selector rather than a call: the stub importer means the
				// function's signature is unknown either way, and the detector matches
				// the selector, so this is uniform across all twelve entries.
				src := fmt.Sprintf("package p\n\nimport %q\n\nvar _ = %s.%s\n", pkg, qualifier, fn)
				hits := servingOracleProbeEnvHits(t, src)
				if len(hits) != 1 {
					t.Fatalf("%s.%s produced %d hit(s) %v, want exactly 1; the allowlist entry is not "+
						"load-bearing and could be removed or disabled unnoticed", qualifier, fn, len(hits), hits)
				}
				if !strings.Contains(hits[0], qualifier+"."+fn) || !strings.Contains(hits[0], pkg) {
					t.Fatalf("the hit does not name %s.%s from package %q: %s", qualifier, fn, pkg, hits[0])
				}
			})
		}
	}
	if total != 12 {
		t.Fatalf("drove %d allowlist entries, want 12; the pinned surface changed without this count "+
			"being acknowledged", total)
	}
	// CONTROL: a NON-environment selector from the same packages is not a hit, so
	// the probes above discriminate rather than flagging every selector.
	for _, tc := range []struct{ pkg, fn string }{{"os", "Open"}, {"syscall", "Kill"}} {
		src := fmt.Sprintf("package p\n\nimport %q\n\nvar _ = %s.%s\n", tc.pkg, tc.pkg, tc.fn)
		if hits := servingOracleProbeEnvHits(t, src); len(hits) != 0 {
			t.Errorf("%s.%s was reported as an environment read: %v", tc.pkg, tc.fn, hits)
		}
	}
}

// servingOracleProbeEnvHits type-checks one synthetic source file exactly as the
// live scan type-checks a production package, then runs the detector over it.
//
// Going through the real binding resolution is the point: a probe scanned without
// it would prove nothing about the shadowing cases.
func servingOracleProbeEnvHits(t *testing.T, src string) []string {
	t.Helper()
	servingOracleFixtureMu.Lock()
	fixture, done := servingOracleProbeCache[src]
	servingOracleFixtureMu.Unlock()
	if !done {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "probe.go", src, 0)
		if err != nil {
			t.Fatalf("parse the probe: %v", err)
		}
		pkg := &servingOraclePackage{Fset: fset, Files: map[string]*ast.File{"probe.go": file}}
		// The package path is the probe's own SOURCE, so two different probes can
		// never be type-checked as one another.
		uses, ok := servingOracleResolveBindings(pkg, "probe:"+src)
		fixture = &servingOracleProbeFixture{File: file, Uses: uses, OK: ok}
		servingOracleFixtureMu.Lock()
		servingOracleProbeCache[src] = fixture
		servingOracleFixtureMu.Unlock()
	}
	if !fixture.OK {
		t.Fatalf("go/types resolved no identifier bindings for the probe; the detector could not judge a "+
			"single selector:\n%s", src)
	}
	// The CACHED file, not a fresh parse: the binding map is keyed by that AST's
	// identifier pointers.
	return servingOracleEnvHits("probe.go", fixture.File, fixture.Uses)
}

// ---------------------------------------------------------------------------
// AST helpers
// ---------------------------------------------------------------------------

func servingOracleRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func servingOracleHasReservedPrefix(name string) bool {
	for _, p := range servingOracleReservedPrefixes {
		if !strings.HasPrefix(name, p) {
			continue
		}
		rest := name[len(p):]
		// A prefix match only counts when the next rune starts a new word, so `sort`
		// is not read as the `so` namespace while `soRenderProject` is.
		if rest == "" {
			continue
		}
		if p == "so" && !(rest[0] >= 'A' && rest[0] <= 'Z') {
			continue
		}
		return true
	}
	return false
}

// servingOracleOwnDeclarations returns every top-level identifier the oracle's own
// files declare.
func servingOracleOwnDeclarations(repo string) (map[string]struct{}, error) {
	paths, err := filepath.Glob(filepath.Join(repo, "internal", "debaml", servingOracleSourceGlob))
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for _, path := range paths {
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil, perr
		}
		for _, name := range servingOracleTopLevelNames(file) {
			out[name] = struct{}{}
		}
	}
	return out, nil
}

// servingOracleTopLevelNames lists the top-level declarations of one file,
// including methods (whose receiver type is what matters for the scan).
func servingOracleTopLevelNames(file *ast.File) []string {
	var out []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				out = append(out, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out = append(out, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							out = append(out, n.Name)
						}
					}
				}
			}
		}
	}
	return out
}

// servingOracleDeclarationIndex maps every top-level identifier in the repository
// to the files declaring it, relative to repo. Test files are INCLUDED: rule 1
// needs both, because that is how a test-only anchor is distinguished from one
// that leaked into production.
func servingOracleDeclarationIndex(repo string) (map[string][]string, error) {
	out := map[string][]string{}
	err := servingOracleWalkGo(repo, func(path, rel string, file *ast.File) {
		for _, name := range servingOracleTopLevelNames(file) {
			out[name] = append(out[name], rel)
		}
	})
	return out, err
}

// servingOracleDeclaredByProduction reports whether name has at least one
// declaration in a NON-test file. A presence-only check would keep the negative
// control green after the name moved into test-only code, at which point it would
// no longer name a production seam at all.
func servingOracleDeclaredByProduction(index map[string][]string, name string) bool {
	for _, rel := range index[name] {
		if !strings.HasSuffix(rel, "_test.go") {
			return true
		}
	}
	return false
}

// servingOracleProductionReferences finds every non-test .go file that declares or
// mentions one of the given identifiers.
func servingOracleProductionReferences(repo string, names map[string]struct{}) ([]string, error) {
	var hits []string
	err := servingOracleWalkGo(repo, func(path, rel string, file *ast.File) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if _, watched := names[id.Name]; !watched {
				return true
			}
			hits = append(hits, rel+": "+id.Name)
			return true
		})
	})
	return hits, err
}

// servingOracleWalkGo yields every .go file under repo, parsed.
//
// The parse is MEMOIZED per root (see the fixture block above): the tree does not
// change while the binary runs, and re-parsing every file in the repository on each
// of 100 iterations is what put this package over the unit-tests budget. The walk
// itself still visits every file on every call, so no assertion is skipped.
func servingOracleWalkGo(repo string, fn func(path, rel string, file *ast.File)) error {
	files, err := servingOracleParsedTree(repo)
	if err != nil {
		return err
	}
	for _, f := range files {
		fn(filepath.Join(repo, f.Rel), f.Rel, f.File)
	}
	return nil
}

// servingOracleParsedTree parses every .go file under repo, once per root.
func servingOracleParsedTree(repo string) ([]servingOracleParsedFile, error) {
	// ONLY the repository root is memoized. It is fixed for the life of the binary,
	// which is what makes reuse sound. A temp directory is not: the AST-error bite
	// test deliberately rewrites one between two scans, and serving it a cached tree
	// would make that test pass by looking at the wrong bytes.
	cacheable := repo == servingOracleRepoRootPath()
	if cacheable {
		servingOracleFixtureMu.Lock()
		if files, done := servingOracleTreeCache[repo]; done {
			err := servingOracleTreeErr[repo]
			servingOracleFixtureMu.Unlock()
			return files, err
		}
		servingOracleFixtureMu.Unlock()
	}

	var out []servingOracleParsedFile
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".jj", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(repo, path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file the parser cannot read is NOT evidence of absence. Swallowing it
			// would make an unparseable production .go read as "no oracle reference",
			// which is the one answer this scan must never give by accident.
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		out = append(out, servingOracleParsedFile{Rel: filepath.ToSlash(rel), File: file})
		return nil
	})

	if cacheable {
		servingOracleFixtureMu.Lock()
		servingOracleTreeCache[repo], servingOracleTreeErr[repo] = out, err
		servingOracleFixtureMu.Unlock()
	}
	return out, err
}

// servingOracleRepoRootPath resolves the repository root once, for the cache
// predicate above. It deliberately does not take a *testing.T: an unresolvable root
// simply makes nothing cacheable rather than failing a test that is not about it.
var servingOracleRepoRootOnce sync.Once
var servingOracleRepoRootAbs string

func servingOracleRepoRootPath() string {
	servingOracleRepoRootOnce.Do(func() {
		if abs, err := filepath.Abs("../.."); err == nil {
			servingOracleRepoRootAbs = abs
		}
	})
	return servingOracleRepoRootAbs
}

// TestServingOracleASTWalkPropagatesParseErrors proves the repo scan FAILS CLOSED
// on a file it cannot parse.
//
// Returning nil there would make an unparseable production .go read as "no oracle
// reference" — absence of evidence standing in for evidence, in the one scan whose
// whole job is to find a reference.
func TestServingOracleASTWalkPropagatesParseErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.go")
	if err := os.WriteFile(good, []byte("package fake\n\nfunc ok() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// CONTROL first: a parseable tree scans cleanly, so the failure below is about
	// the broken file rather than about the walk refusing everything.
	if _, err := servingOracleProductionReferences(dir, map[string]struct{}{"servingOracleFixtures": {}}); err != nil {
		t.Fatalf("a parseable tree must scan without error; got %v", err)
	}
	broken := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(broken, []byte("package fake\n\nfunc ( ... this is not Go\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := servingOracleProductionReferences(dir, map[string]struct{}{"servingOracleFixtures": {}})
	if err == nil {
		t.Fatal("an UNPARSEABLE .go file was silently skipped; the scan would report 'no production " +
			"reference' for a file it never read")
	}
	if !strings.Contains(err.Error(), "broken.go") {
		t.Fatalf("the error does not name the file it could not read: %v", err)
	}
}

// TestServingOracleBindingResolutionRefusesAnEmptyResult witnesses the guard that
// stops a package whose bindings could not be resolved from being reported clean.
//
// Every real package and every probe resolves bindings, so the guard is satisfied
// in normal operation and its removal cannot be seen from those runs. Driving a
// package that genuinely produces none is what makes it load-bearing.
func TestServingOracleBindingResolutionRefusesAnEmptyResult(t *testing.T) {
	fset := token.NewFileSet()
	empty, err := parser.ParseFile(fset, "empty.go", "package p\n", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := servingOracleResolveBindings(
		&servingOraclePackage{Fset: fset, Files: map[string]*ast.File{"empty.go": empty}},
		"probe:binding-resolution/empty"); ok {
		t.Fatal("a package that resolves NO identifier bindings was reported usable; the scan would then " +
			"report it clean without having been able to judge a single selector")
	}
	// CONTROL: a package that DOES resolve bindings is usable, so the guard is about
	// the empty result rather than about refusing everything.
	fset2 := token.NewFileSet()
	real, err := parser.ParseFile(fset2, "real.go", "package p\n\nimport \"os\"\n\nvar _ = os.Args\n", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	uses, ok := servingOracleResolveBindings(
		&servingOraclePackage{Fset: fset2, Files: map[string]*ast.File{"real.go": real}},
		"probe:binding-resolution/real")
	if !ok || len(uses) == 0 {
		t.Fatalf("the control package resolved no bindings (ok=%v, uses=%d)", ok, len(uses))
	}
}

// TestServingOracleEnvHitsRequiresTheOsPackage witnesses the last predicate in the
// binding check: a qualifier that resolves to a PACKAGE must resolve to `os`
// specifically.
//
// Within a single file that check cannot fire — an identifier cannot be both an os
// import name and a binding to some other package — so it is unreachable from the
// live scan and from the source-level bite table. It is asserted directly rather
// than left as an unexercised branch: the detector is handed a file that imports os
// as `env`, and a binding map in which `env` denotes a DIFFERENT package.
func TestServingOracleEnvHitsRequiresTheOsPackage(t *testing.T) {
	const src = "package p\n\nimport env \"os\"\n\nfunc f() string { return env.Getenv(\"X\") }\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var qualifier *ast.Ident
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Getenv" {
			qualifier, _ = sel.X.(*ast.Ident)
		}
		return true
	})
	if qualifier == nil {
		t.Fatal("the probe has no env.Getenv selector")
	}
	self := types.NewPackage("probe", "p")

	// Bound to a package that is NOT os: not a hit.
	other := types.NewPkgName(token.NoPos, self, "env", types.NewPackage("example.com/other", "env"))
	if hits := servingOracleEnvHits("probe.go", file,
		map[*ast.Ident]types.Object{qualifier: other}); len(hits) > 0 {
		t.Errorf("a qualifier bound to %q was reported as an os read: %v",
			other.Imported().Path(), hits)
	}
	// CONTROL: bound to os, it IS a hit — so the check discriminates rather than
	// rejecting every package binding.
	osPkg := types.NewPkgName(token.NoPos, self, "env", types.NewPackage("os", "os"))
	hits := servingOracleEnvHits("probe.go", file, map[*ast.Ident]types.Object{qualifier: osPkg})
	if len(hits) != 1 || !strings.Contains(hits[0], "env.Getenv") {
		t.Errorf("a qualifier bound to os was not reported: %v", hits)
	}
}

// TestServingOracleEnvPkgNameIsTheSharedDerivation pins the qualifier derivation a
// default import gets, including the multi-segment case no watched package
// currently has.
//
// The detector and the per-entry probe generator both call this helper. They used
// to derive it independently — the detector registered the WHOLE import path — which
// agreed only because "os" and "syscall" contain no slash. A watched multi-segment
// path would then never have matched its default-imported selectors while the pinned
// set stayed green, which is the false-green shape this file exists to prevent.
func TestServingOracleEnvPkgNameIsTheSharedDerivation(t *testing.T) {
	for path, want := range map[string]string{
		"os":                    "os",
		"syscall":               "syscall",
		"example.com/x/env":     "env",
		"golang.org/x/sys/unix": "unix",
	} {
		if got := servingOracleEnvPkgName(path); got != want {
			t.Errorf("servingOracleEnvPkgName(%q) = %q, want %q", path, got, want)
		}
	}
	// And the detector really uses it: a file DEFAULT-importing a multi-segment
	// watched path must register the last segment as a qualifier. The path is
	// injected into the watched set for the length of this test so the case can be
	// driven without moving the pinned production surface.
	//
	// The injection SWAPS A WHOLE MAP rather than writing into the live one. Every
	// reader of servingOracleEnvAPIs (servingOracleIsEnvAPI, servingOracleEnvImports,
	// servingOracleAnyEnvAPI) reads it WITHOUT the mutex, because a pinned lookup
	// table is otherwise immutable for the life of the process — so an in-place write
	// under a write-side-only lock buys nothing, and the moment any test in this file
	// grows a t.Parallel it becomes a concurrent map read/write. Copy-on-write keeps
	// every map value immutable once published: a reader holds a complete, consistent
	// table either way, and the restore below hands back the ORIGINAL map untouched.
	const multi = "example.com/x/env"
	servingOracleFixtureMu.Lock()
	pinned := servingOracleEnvAPIs
	widened := make(map[string]map[string]struct{}, len(pinned)+1)
	for path, api := range pinned {
		widened[path] = api
	}
	widened[multi] = map[string]struct{}{"Getenv": {}}
	servingOracleEnvAPIs = widened
	servingOracleFixtureMu.Unlock()
	defer func() {
		servingOracleFixtureMu.Lock()
		servingOracleEnvAPIs = pinned
		servingOracleFixtureMu.Unlock()
	}()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go",
		"package p\n\nimport \""+multi+"\"\n\nvar _ = env.Getenv\n", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names, dot := servingOracleEnvImports(file)
	if dot != nil {
		t.Fatalf("a default import was read as a dot import: %v", dot)
	}
	if _, ok := names["env"]; !ok {
		t.Fatalf("a default import of %q registered %v; the qualifier must be its last segment %q, or the "+
			"detector would never match its selectors", multi, names, "env")
	}
	if _, ok := names[multi]; ok {
		t.Fatalf("a default import of %q registered the whole PATH as a qualifier; no selector is ever "+
			"spelled that way", multi)
	}
}

// TestServingOracleProductionPackageCacheIsKeyedByRoots proves the memoized
// package parse is keyed by the roots it scanned.
//
// The live scan is the only caller today and always names the same two roots, so a
// cache that ignored the argument would be correct by accident and unobservable —
// and the next caller to name different roots would silently receive the first
// call's packages and prove nothing about the trees it asked for. Two calls with
// DIFFERENT root sets must return different results.
//
// It costs one extra parse of one root per process: the result is memoized like
// every other fixture here, so -count re-runs are free.
func TestServingOracleProductionPackageCacheIsKeyedByRoots(t *testing.T) {
	oneRoot := servingOracleProductionPackages(t, ".")
	twoRoots := servingOracleProductionPackages(t, ".", "../schema")
	if len(oneRoot) == 0 || len(twoRoots) == 0 {
		t.Fatalf("a root scan returned nothing (%d, %d); the comparison would be vacuous",
			len(oneRoot), len(twoRoots))
	}
	if len(twoRoots) <= len(oneRoot) {
		t.Fatalf("scanning %d root(s) returned %d package(s) and scanning 1 returned %d; the cache is "+
			"serving one caller's result to another that named different roots", 2, len(twoRoots), len(oneRoot))
	}
	// The narrower scan must not contain a package from the root it did not name.
	for dir := range oneRoot {
		if strings.Contains(filepath.ToSlash(dir), "schema") {
			t.Errorf("the single-root scan of %q returned the package %q, which is under a root it never "+
				"named", ".", dir)
		}
	}
	// …and the wider one must, so the difference is the second root rather than noise.
	found := false
	for dir := range twoRoots {
		if strings.Contains(filepath.ToSlash(dir), "schema") {
			found = true
		}
	}
	if !found {
		t.Error("the two-root scan returned no package under ../schema; the roots argument had no effect")
	}
}

// TestServingOracleProductionBindingsBelongToTheirOwnParse proves the memoized
// go/types bindings are associated with the AST they were computed from, and not
// merely with the directory that AST came from.
//
// This is the second half of the roots-keyed cache above, and the reason it is
// needed: once one directory can have several distinct parses live at once, a
// binding cache keyed by directory alone hands the second parse the first parse's
// map. Nothing about that fails loudly — the map is a valid map, and lookups into
// it simply miss — so the environment detector would fail closed on every selector
// of a package it believes it scanned. Binding maps are keyed by *ast.Ident
// POINTERS, so "the wrong package's map" and "an empty map" are the same thing.
//
// The assertion is exact rather than statistical: every identifier the returned map
// binds must be an identifier of the AST that was asked about, and there must be at
// least one. Serving the other root set's map makes the overlap ZERO while the map
// itself stays non-empty, which trips both halves.
func TestServingOracleProductionBindingsBelongToTheirOwnParse(t *testing.T) {
	const dir = "."
	oneRoot := servingOracleProductionPackages(t, dir)
	twoRoots := servingOracleProductionPackages(t, dir, "../schema")

	// Precondition: the SAME directory, parsed under two root sets, is two distinct
	// ASTs. Without this the test would pass by comparing a parse to itself.
	narrow, wide := oneRoot[dir], twoRoots[dir]
	if narrow == nil || wide == nil {
		t.Fatalf("directory %q is missing from a root scan (1-root: %v, 2-root: %v); the comparison "+
			"would be vacuous", dir, narrow != nil, wide != nil)
	}
	if narrow == wide {
		t.Fatalf("both root scans returned the SAME *servingOraclePackage for %q; the binding cache is "+
			"never asked to distinguish two parses of one directory and this witness proves nothing", dir)
	}
	if len(narrow.Files) == 0 || len(wide.Files) == 0 {
		t.Fatalf("a parse of %q has no files (1-root: %d, 2-root: %d)", dir, len(narrow.Files), len(wide.Files))
	}

	// Both directions, because either parse may reach the cache first: whichever one
	// a directory-keyed cache stored, the OTHER one is served a foreign map.
	for _, parse := range []struct {
		what string
		pkg  *servingOraclePackage
	}{
		{"the 1-root parse", narrow},
		{"the 2-root parse", wide},
	} {
		own := map[*ast.Ident]struct{}{}
		for _, file := range parse.pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				if id, isIdent := n.(*ast.Ident); isIdent {
					own[id] = struct{}{}
				}
				return true
			})
		}

		uses := servingOracleProductionBindings(t, parse.pkg, dir)
		matched := 0
		for id := range uses {
			if _, mine := own[id]; mine {
				matched++
			}
		}
		switch {
		case matched == 0:
			t.Errorf("the bindings returned for %s of %q contain ZERO of its %d identifiers (the map "+
				"binds %d identifier(s), all of them from some OTHER parse); the binding cache is keyed "+
				"by directory rather than by the parse it belongs to, so the environment scan would "+
				"resolve nothing in this package", parse.what, dir, len(own), len(uses))
		case matched != len(uses):
			t.Errorf("the bindings returned for %s of %q bind %d identifier(s) but only %d belong to "+
				"that parse; the map is a mixture of parses", parse.what, dir, len(uses), matched)
		}
	}
}
