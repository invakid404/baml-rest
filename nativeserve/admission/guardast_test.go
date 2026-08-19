package admission

// De-BAML serving cutover S1 — the AST substrate the structural guards run on.
//
// The guards in this package assert things about the package's own SOURCE: that every
// exported admission entry point is cohort-gated, that no untagged exported API can
// hand in a gate, that every exported recorder is driven with hostile input, that the
// bounded-label allow-list contains only declared constants, and that the input
// structs carry no forgeable Surface field.
//
// They were originally written as regexes over source text, and a bot review found the
// predictable consequence: each regex encoded an incidental detail of how the code
// happens to be written today — the literal receiver `a *Admitter`, the receiver name
// `m`, a `[^)]*` parameter list that nested function types slip through, and a scan
// that could not tell code from prose. None of those was a live bypass, but each was a
// guard that a routine refactor could quietly turn false-green, which for a guard is
// the whole failure mode.
//
// So discovery is done on the parsed AST instead. A receiver rename, a nested
// parameter type, or an example written in a comment cannot change what these guards
// see, because they see declarations rather than characters.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// astSource is one parsed non-test source file of this package.
type astSource struct {
	name string
	file *ast.File
	// tagged reports whether the file carries the `nanollm_integration` opt-in build
	// constraint, i.e. whether a released consumer can link it. The untagged set is
	// the public surface the no-bypass guards care about.
	tagged bool
}

// packageAST parses every non-test .go file in this package.
//
// It fails if it finds none: a guard that silently inspects an empty file set is worse
// than no guard, so non-vacuity is checked here once for every caller.
func packageAST(t *testing.T) []astSource {
	t.Helper()
	out, err := parsePackageAST()
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no package sources parsed; every structural guard would be vacuous")
	}
	return out
}

// parsePackageAST is packageAST without a *testing.T, for the one caller that builds
// the bounded-label allow-list from a plain function. A failure here surfaces as an
// EMPTY declared-enum set, which TestDeclaredStagesAndReasonsScanIsNotVacuous fails
// on — the scan cannot go quiet.
func parsePackageAST() ([]astSource, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []astSource
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, name, raw, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		out = append(out, astSource{
			name:   name,
			file:   f,
			tagged: isGatedByTheOptInTag(raw),
		})
	}
	return out, nil
}

// isGatedByTheOptInTag reports whether a source file is behind the `nanollm_integration`
// opt-in constraint — i.e. whether a released consumer CANNOT link it.
//
// It requires the constraint to be exactly that tag, not merely to start with it. A prefix
// test was permissive in the direction that matters: `//go:build nanollm_integration ||
// something_else` starts with the same text but IS linkable without the tag, and treating
// such a file as gated would have excluded it from the no-public-bypass scans. Anything
// this function cannot positively recognise as gated is treated as untagged, which is the
// strict direction — the guards then inspect it.
func isGatedByTheOptInTag(raw []byte) bool {
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first) == "//go:build nanollm_integration"
}

// structDecl is one `type <Name> struct{…}` declaration and the file it came from.
// Guards that inspect a struct collect ALL of its declarations rather than the first,
// because "exactly one" is itself part of what they assert: an absent declaration
// makes a structural proof vacuous, and two of them (say, one per build tag) mean the
// guard inspected an arbitrary half of the truth.
type structDecl struct {
	file string
	spec *ast.StructType
}

// structTypeDecls returns every declaration of the named struct type in the package.
func structTypeDecls(sources []astSource, name string) []structDecl {
	var out []structDecl
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				out = append(out, structDecl{file: src.name, spec: st})
			}
		}
	}
	return out
}

// soleStructDecl applies the "exactly one declaration, then inspect it" rule that the
// lane-input guard rests on, and returns the reason it could not when it could not.
//
// Guard and bite both call this. An ABSENT declaration means a structural proof lost its
// subject; a DUPLICATED one means it would have inspected an arbitrary half of the truth.
// Both are false-green, so both are reported rather than skipped.
func soleStructDecl(sources []astSource, name string) (structDecl, string) {
	decls := structTypeDecls(sources, name)
	switch len(decls) {
	case 1:
		return decls[0], ""
	case 0:
		return structDecl{}, "no declaration of struct " + name + " was found"
	default:
		files := make([]string, 0, len(decls))
		for _, d := range decls {
			files = append(files, d.file)
		}
		return structDecl{}, "struct " + name + " is declared " +
			strconv.Itoa(len(decls)) + " times (" + strings.Join(files, ", ") + ")"
	}
}

// mentionsIdent reports whether a parsed file USES the named identifier anywhere in
// its code. Comments are not part of the AST, so prose that merely names a symbol —
// in a line comment or a block comment — is invisible here by construction.
func mentionsIdent(src astSource, name string) bool {
	found := false
	ast.Inspect(src.file, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// syntheticSource parses an in-test source file into the SAME astSource the package scan
// produces.
//
// This is what lets a bite drive the REAL discovery function over receiver shapes (or
// signatures, or const blocks) that this package does not currently contain. A bot review
// found the first version of those bites re-implementing the discovery predicate instead:
// they agreed with the guard by construction, so narrowing the guard would have left the
// guard AND its bite green. Now there is one predicate and two inputs.
func syntheticSource(t *testing.T, name, src string) astSource {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse synthetic %s: %v", name, err)
	}
	return astSource{name: name, file: f}
}

// discoveredFunc is one function declaration a discovery predicate selected.
type discoveredFunc struct {
	name     string
	file     string
	receiver string // the receiver TYPE, or "" for a package-level function
	tagged   bool   // declared behind the `nanollm_integration` opt-in constraint
	decl     *ast.FuncDecl
}

// discoverFuncs applies one predicate across a set of sources. Every structural guard
// that asks "which functions in this package look like X" goes through here, so a guard
// and its bite cannot answer that question differently.
func discoverFuncs(sources []astSource, match func(*ast.FuncDecl) bool) []discoveredFunc {
	var out []discoveredFunc
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !match(fn) {
				continue
			}
			out = append(out, discoveredFunc{
				name:     fn.Name.Name,
				file:     src.name,
				receiver: receiverTypeName(fn),
				tagged:   src.tagged,
				decl:     fn,
			})
		}
	}
	return out
}

// receiverTypeName returns the (unqualified) type name of a FuncDecl's receiver, with
// any pointer indirection stripped, or "" for a package-level function. It is
// deliberately indifferent to the receiver's VARIABLE name — that name is the detail
// two of the original regexes accidentally depended on.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	// A generic receiver — take the base identifier. IndexExpr is the single-parameter
	// form (Type[T]); IndexListExpr is the multi-parameter one (Type[T, U]).
	//
	// Omitting the second form was a PERMISSIVE gap, which is why it is closed here even
	// though this package declares no generic recorder today: receiverTypeName returning
	// "" makes isExportedMetricsRecorder answer false, so a generic `Metrics[T]` recorder
	// would have dropped out of hostile-input coverage silently.
	switch idx := t.(type) {
	case *ast.IndexExpr:
		if id, ok := idx.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := idx.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// signatureMentions reports whether a function's parameters or results mention any of
// the named types ANYWHERE in their type expressions — including inside a nested
// function type, a slice, a map, a channel or a pointer. That nesting is exactly what
// a `[^)]*` signature regex cannot see.
func signatureMentions(fn *ast.FuncDecl, names ...string) string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := ""
	inspect := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			ast.Inspect(f.Type, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && want[id.Name] {
					found = id.Name
					return false
				}
				// A qualified type (pkg.Name) — match on the selector.
				if sel, ok := n.(*ast.SelectorExpr); ok && want[sel.Sel.Name] {
					found = sel.Sel.Name
					return false
				}
				return true
			})
		}
	}
	inspect(fn.Type.Params)
	inspect(fn.Type.Results)
	return found
}

// resultsMention is signatureMentions restricted to a function's RESULTS.
//
// The distinction matters for identity types: a parameter can only ever receive what the
// caller could already construct, whereas a result hands back whatever the package built —
// including state behind unexported fields.
func resultsMention(fn *ast.FuncDecl, names ...string) string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := ""
	if fn.Type.Results == nil {
		return ""
	}
	for _, f := range fn.Type.Results.List {
		ast.Inspect(f.Type, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if want[v.Sel.Name] {
					found = v.Sel.Name
					return false
				}
			case *ast.Ident:
				if want[v.Name] {
					found = v.Name
					return false
				}
			}
			return true
		})
	}
	return found
}

// constStringsOfType collects the STRING values of every declared constant whose type
// is one of the named types. It reads declarations only: a value that appears in a
// line comment, a block comment or a doc example is not a declaration and cannot widen
// anything derived from this.
//
// TYPE CONTINUATION, per the Go spec rather than per intuition. Inside a parenthesized
// const block, "the expression list may be omitted from any but the first ConstSpec.
// Such an empty list is equivalent textually to the substitution of the first preceding
// non-empty expression list and its type if any." So a spec inherits the previous type
// ONLY when it omits the values too. A spec that supplies values but no type is an
// independent, untyped constant — it does NOT take the block's earlier type.
//
// The first version of this helper carried the type across that second case, which a bot
// review caught. It failed permissively: in a mixed block, an untyped constant would have
// been read as a declared Stage/Reason and would have WIDENED the bounded-label
// allow-list. TestMixedConstBlockDoesNotInheritTypeAcrossValues is the bite.
func constStringsOfType(sources []astSource, typeNames ...string) []string {
	want := map[string]bool{}
	for _, n := range typeNames {
		want[n] = true
	}
	var out []string
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			carried := "" // the type continued from an earlier spec in this block
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				switch {
				case vs.Type != nil:
					// An explicit type: this spec's own, and the one later value-less
					// specs continue.
					carried = ""
					if id, ok := vs.Type.(*ast.Ident); ok {
						carried = id.Name
					}
				case len(vs.Values) > 0:
					// Values but no type: an independent constant. It continues nothing,
					// and it ends the continuation for the specs after it too — the Go
					// rule substitutes the "first preceding non-empty expression list AND
					// ITS TYPE IF ANY", which from here on is this untyped one.
					carried = ""
				default:
					// Neither type nor values: this spec repeats the previous expression
					// list and its type, so `carried` stands. There is nothing to collect
					// here — the repeated value was already collected from the spec it
					// repeats.
				}
				if !want[carried] {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// blockCommentContaining reports the file whose BLOCK comment contains the given token,
// or "" if no block comment in the package does.
//
// Comments are not part of the AST's declaration graph, but the parser does record them
// (packageAST asks for ParseComments), which is what lets a proof assert that a token
// exists in this package's shipped source AS PROSE AND ONLY AS PROSE.
func blockCommentContaining(sources []astSource, token string) string {
	for _, src := range sources {
		for _, group := range src.file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, "/*") && strings.Contains(c.Text, token) {
					return src.name
				}
			}
		}
	}
	return ""
}

// packageRawSources returns the package's non-test sources as TEXT.
//
// Nothing that guards a property reads this: the guards all moved to the AST precisely
// so prose could not reach them. It exists for the opposite purpose — to build the
// prose-permissive MUTANT of the allow-list derivation, so a proof can show what the
// old text scan would have accepted and that the current derivation does not.
func packageRawSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no package sources read")
	}
	return out
}

// TestMixedConstBlockDoesNotInheritTypeAcrossValues is the bite for the type-continuation
// rule above. A const block that mixes a typed Stage spec with a following spec that
// supplies VALUES BUT NO TYPE must contribute only the typed one: the untyped constant is
// its own thing, and admitting it would silently widen the bounded-label allow-list.
//
// It also pins the legitimate continuation (a spec with neither type nor values) and the
// re-typing case, so the fix cannot be "return nothing and pass".
func TestMixedConstBlockDoesNotInheritTypeAcrossValues(t *testing.T) {
	const mixed = `package admission

const (
	RealStage      Stage = "typed_stage"
	UntypedFollows       = "untyped_must_not_inherit"
	AlsoUntyped          = "still_must_not_inherit"
	RetypedStage   Stage = "retyped_stage"
	ContinuesStage
	AfterUntyped = "untyped_again"
)

const (
	Lonely Reason = "typed_reason"
)
`
	got := constStringsOfType([]astSource{syntheticSource(t, "mixed.go", mixed)}, "Stage", "Reason")
	want := map[string]bool{"typed_stage": true, "retyped_stage": true, "typed_reason": true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("constStringsOfType admitted %q, which is not a declared Stage/Reason constant: "+
				"a valued-but-untyped spec must not inherit the block's earlier type", v)
		}
		delete(want, v)
	}
	for v := range want {
		t.Errorf("constStringsOfType missed the genuinely typed constant %q", v)
	}
}

// TestConstExtractionIgnoresProse is the bite for constStringsOfType: a value that
// exists only in a comment — line or block — must not be collected. This is what makes
// the bounded-label allow-list unwidenable by prose, and it is checked against a
// synthetic file so the proof does not depend on this package happening to contain
// such a comment today.
func TestConstExtractionIgnoresProse(t *testing.T) {
	const synthetic = `package admission

// A doc comment showing an example: DocStage Stage = "prose_only_line_comment"
/*
   A block comment with another: BlockStage Stage = "prose_only_block_comment"
*/
const RealStage Stage = "real_declared_stage"

// A constant of an unrelated type must also be ignored.
const Unrelated Mode = "unrelated_mode"
`
	got := constStringsOfType([]astSource{syntheticSource(t, "synthetic.go", synthetic)}, "Stage", "Reason")
	if len(got) != 1 || got[0] != "real_declared_stage" {
		t.Fatalf("constStringsOfType(synthetic) = %v, want exactly [real_declared_stage]: "+
			"prose must not widen the allow-list and an unrelated type must not enter it", got)
	}
}

// TestReceiverDiscoveryIsNameIndependent is the bite for receiverTypeName: the guards must
// key on the receiver's TYPE, never on the variable name the author happened to pick. A
// rename is the refactor that silently disarmed the original regexes.
//
// The generic rows are here because returning "" for an unrecognised receiver shape is
// PERMISSIVE — a discovery predicate that filters on the receiver type would simply not
// see such a method.
func TestReceiverDiscoveryIsNameIndependent(t *testing.T) {
	const synthetic = `package admission

func (m *Metrics) RecordA()                {}
func (renamed *Metrics) RecordB()          {}
func (Metrics) RecordC()                   {}
func (g *Metrics[T]) RecordGeneric()       {}
func (g *Metrics[T, U]) RecordGeneric2()   {}
func (v Metrics[T]) RecordGenericValue()   {}
func (a *Admitter) AdmitX()                {}
func (differently *Admitter) AdmitY()      {}
func AdmitZ()                              {}
func (o *Other) RecordD()                  {}
`
	got := map[string]string{}
	for _, fn := range discoverFuncs(
		[]astSource{syntheticSource(t, "synthetic.go", synthetic)},
		func(*ast.FuncDecl) bool { return true },
	) {
		got[fn.name] = fn.receiver
	}
	for name, want := range map[string]string{
		"RecordA": "Metrics", "RecordB": "Metrics", "RecordC": "Metrics",
		"RecordGeneric": "Metrics", "RecordGeneric2": "Metrics", "RecordGenericValue": "Metrics",
		"AdmitX": "Admitter", "AdmitY": "Admitter", "AdmitZ": "", "RecordD": "Other",
	} {
		if got[name] != want {
			t.Errorf("receiver type of %s = %q, want %q", name, got[name], want)
		}
	}
	// And the consequence, stated on the predicate the recorder guard actually uses.
	recorders := map[string]bool{}
	for _, r := range exportedMetricsRecorders([]astSource{syntheticSource(t, "synthetic.go", synthetic)}) {
		recorders[r.name] = true
	}
	for _, want := range []string{"RecordA", "RecordB", "RecordC", "RecordGeneric", "RecordGeneric2", "RecordGenericValue"} {
		if !recorders[want] {
			t.Errorf("%s is an exported *Metrics recorder that the discovery predicate MISSED", want)
		}
	}
	if recorders["RecordD"] || recorders["AdmitX"] {
		t.Error("the recorder predicate selected a method on another type")
	}
}

// TestOptInTagDetectionIsExact is the bite for isGatedByTheOptInTag. Everything the
// no-public-bypass guards SKIP rests on it, so the permissive direction — calling a file
// gated when a released consumer can still link it — must be closed.
func TestOptInTagDetectionIsExact(t *testing.T) {
	gated := []string{
		"//go:build nanollm_integration\n\npackage admission\n",
		"//go:build nanollm_integration  \n\npackage admission\n",
	}
	for _, src := range gated {
		if !isGatedByTheOptInTag([]byte(src)) {
			t.Errorf("a genuinely gated file was read as untagged: %q", src)
		}
	}
	notGated := []string{
		// Linkable WITHOUT the tag: the old prefix test called this gated and skipped it.
		"//go:build nanollm_integration || something_else\n\npackage admission\n",
		"//go:build nanollm_integration_extra\n\npackage admission\n",
		"// a comment first\n//go:build nanollm_integration\n\npackage admission\n",
		"package admission\n",
	}
	for _, src := range notGated {
		if isGatedByTheOptInTag([]byte(src)) {
			t.Errorf("a file a released consumer can link was treated as gated (and would be "+
				"skipped by the no-bypass scans): %q", src)
		}
	}
}

// TestSignatureMentionsSeesNestedTypes is the bite for signatureMentions: a type
// buried inside a function parameter, a slice, a map or a variadic must still be seen.
// The original `[^)]*` regex could not see past the first `)`.
func TestSignatureMentionsSeesNestedTypes(t *testing.T) {
	const synthetic = `package admission

func Direct(in CohortInput)                        {}
func Nested(cb func(CohortInput) error)            {}
func Sliced(in []CohortInput)                      {}
func Mapped(in map[string]*CohortGate)             {}
func Variadic(in ...CohortInput)                   {}
func Returned() *CohortGate                        { return nil }
func ReturnedNested() func() *CohortGate           { return nil }
func Qualified(in admission.CohortInput)           {}
func Clean(in string) error                        { return nil }
`
	for _, fn := range discoverFuncs(
		[]astSource{syntheticSource(t, "synthetic.go", synthetic)},
		func(*ast.FuncDecl) bool { return true },
	) {
		got := signatureMentions(fn.decl, "CohortGate", "CohortInput")
		if fn.name == "Clean" {
			if got != "" {
				t.Errorf("signatureMentions(Clean) = %q, want none", got)
			}
			continue
		}
		if got == "" {
			t.Errorf("signatureMentions(%s) found nothing; a nested identity/gate type escaped the guard", fn.name)
		}
	}
}
