//go:build integration

package debaml

// COMPOSING THE 719-CASE CONSTRAINT CORPUS AS A REGRESSION SOURCE.
//
// internal/debaml/constraintoracle enumerates 719 (expression, `this`) cases and
// pins both legs' outcomes. Those cases are EVALUATOR-shaped: the native leg there
// is handed a hand-written ConstraintValue and the expression, with no coercion in
// between. This file re-drives every one of them SERVING-shaped instead: the
// group's assistant text is coerced by each engine from raw bytes, and the
// predicate runs over whatever that coercion produced.
//
// That is what makes them a regression source here rather than a separate suite —
// a case whose outcome depends on the coerced value, not just on the expression,
// is now covered end to end, and any case that stops reproducing its pinned outcome
// through the serving path fails here.
//
// HOW THE CORPUS CROSSES THE PACKAGE BOUNDARY. Both corpora live in _test.go files
// in different packages, and Go cannot import test code across packages; promoting
// either into a non-test package would add shared production-visible code, which
// this slice forbids. The carrier is a checked-in JSON artifact written by
// constraintoracle's TestConstraintCorpusBridgeExport, which re-renders and
// byte-compares it on every run, so it cannot drift from the live 719 cases.
//
// STRICT DECODING. The artifact is the oracle's one encoded-evidence boundary, and
// it is decoded with DisallowUnknownFields and an explicit duplicate-key rejection:
// a field the producer added but the consumer does not understand, or a case object
// carrying the same key twice, is a hard failure rather than a silently dropped
// half of the evidence.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/internal/schema"
)

const (
	// soBridgePath is the artifact constraintoracle exports.
	soBridgePath = "testdata/serving_oracle/constraint_corpus.json"
	// soBridgeVersion is the artifact SHAPE this consumer understands. A producer
	// that bumps it without this being updated fails loudly.
	soBridgeVersion = 1
	// soBridgePrefix namespaces the bridge's generated declarations and functions so
	// they cannot collide with the serving corpus's own.
	soBridgePrefix = "CO_"
)

// ---------------------------------------------------------------------------
// The artifact
// ---------------------------------------------------------------------------

type soBridgeExport struct {
	Version int             `json:"version"`
	Prelude string          `json:"prelude"`
	Groups  []soBridgeGroup `json:"groups"`
	Cases   []soBridgeCase  `json:"cases"`
}

type soBridgeGroup struct {
	Name     string `json:"name"`
	BAMLType string `json:"baml_type"`
	Input    string `json:"input"`
}

type soBridgeCase struct {
	Label    string `json:"label"`
	Group    string `json:"group"`
	Expr     string `json:"expr"`
	Retained string `json:"retained,omitempty"`
	Stock    string `json:"stock"`
	Native   string `json:"native"`
}

// retainedExpr is the source the engines actually evaluate: BAML's attribute lexer
// doubles backslashes, so a row that carries a Retained spelling is compared as the
// bytes stock reports back rather than as the bytes the .baml was written with.
func (c soBridgeCase) retainedExpr() string {
	if c.Retained != "" {
		return c.Retained
	}
	return c.Expr
}

// isolated mirrors constraintoracle's batching rule: a case whose stock outcome is
// not a real check status gets its own function, because an evaluator error rejects
// the WHOLE node and would take every other case in its batch down with it.
func (c soBridgeCase) isolated() bool { return c.Stock == "error" || c.Stock == "no-checks" }

// soLoadBridgeExport reads the artifact with a STRICT decoder.
func soLoadBridgeExport(path string) (soBridgeExport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return soBridgeExport{}, err
	}
	if err := soRejectDuplicateJSONKeys(raw); err != nil {
		return soBridgeExport{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A field the producer added that this consumer does not model would otherwise
	// be silently dropped, and half the evidence would go unnoticed.
	dec.DisallowUnknownFields()
	var out soBridgeExport
	if err := dec.Decode(&out); err != nil {
		return soBridgeExport{}, fmt.Errorf("decode %s: %w", path, err)
	}
	// Exactly one document: trailing content would mean the artifact is not what it
	// claims to be.
	if _, err := dec.Token(); err != io.EOF {
		return soBridgeExport{}, fmt.Errorf("decode %s: trailing content after the document (%v)", path, err)
	}
	if out.Version != soBridgeVersion {
		return soBridgeExport{}, fmt.Errorf("%s carries version %d, this consumer understands %d",
			path, out.Version, soBridgeVersion)
	}
	return out, nil
}

// soRejectDuplicateJSONKeys walks the raw document and refuses any object that
// declares the same key twice.
//
// encoding/json keeps the LAST occurrence silently, so a duplicated key would drop
// evidence without any decoder error. DisallowUnknownFields does not catch it.
func soRejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyTok.(string)
				if !ok {
					return fmt.Errorf("%s: non-string object key %v", path, keyTok)
				}
				if seen[key] {
					return fmt.Errorf("%s: duplicate object key %q; encoding/json would silently keep the "+
						"last one and drop the evidence in the first", path, key)
				}
				seen[key] = true
				if err := walk(path + "." + key); err != nil {
					return err
				}
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		// Consume the closing delimiter.
		_, err = dec.Token()
		return err
	}
	if err := walk("$"); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// The bridge project
// ---------------------------------------------------------------------------

// soBridgePrelude is the schema form of constraintoracle's shared declarations.
//
// It is hand-written here and then PROVEN equivalent: TestServingOracleBridgeCorpus
// renders it back with the oracle's own renderer and byte-compares against the
// prelude text the artifact carries, so a change on the producer's side cannot pass
// unnoticed.
func soBridgePrelude() ([]schema.ClassDef, []schema.EnumDef) {
	rouge := "rouge"
	classes := []schema.ClassDef{
		soClassOf("Probe", []schema.ClassField{
			soField("b", intType()),
			soField("a", stringType()),
			soField("c", soListOf(intType())),
		}),
		soClassOf("NestInner", []schema.ClassField{
			soField("name", intType()),
			soField("tags", soListOf(stringType())),
		}),
		soClassOf("Nest", []schema.ClassField{
			soField("a", soClassType("NestInner")),
			soField("name", stringType()),
			soField("rows", soListOf(soClassType("NestInner"))),
		}),
	}
	enums := []schema.EnumDef{{
		Name: schema.Name{Name: "Hue"},
		Values: []schema.EnumValue{
			{Name: schema.Name{Name: "RED", Alias: &rouge}},
			{Name: schema.Name{Name: "GREEN"}},
		},
	}}
	return classes, enums
}

// soBridgePreludeOrder is the order the producer declares the prelude in, so the
// equivalence render can be compared verbatim.
var soBridgePreludeOrder = []string{"Probe", "Hue", "NestInner", "Nest"}

// soParseBAMLType turns one of the corpus's BAML type expressions into a
// schema.Type.
//
// It is deliberately small and total: an expression it does not understand is an
// error, never a guess. Every parse is checked by rendering it back and requiring
// the original spelling, so a mis-parse cannot silently change what a case is
// driven against.
func soParseBAMLType(expr string) (schema.Type, error) {
	e := strings.TrimSpace(expr)
	switch {
	case e == "":
		return schema.Type{}, fmt.Errorf("empty type expression")
	case strings.HasSuffix(e, "?"):
		inner, err := soParseBAMLType(strings.TrimSuffix(e, "?"))
		if err != nil {
			return schema.Type{}, err
		}
		return soOptional(inner), nil
	case strings.HasSuffix(e, "[]"):
		inner, err := soParseBAMLType(strings.TrimSuffix(e, "[]"))
		if err != nil {
			return schema.Type{}, err
		}
		return soListOf(inner), nil
	case strings.HasPrefix(e, "map<") && strings.HasSuffix(e, ">"):
		body := e[len("map<") : len(e)-1]
		comma := strings.Index(body, ",")
		if comma < 0 {
			return schema.Type{}, fmt.Errorf("map type %q has no key/value separator", expr)
		}
		key, err := soParseBAMLType(body[:comma])
		if err != nil {
			return schema.Type{}, err
		}
		val, err := soParseBAMLType(body[comma+1:])
		if err != nil {
			return schema.Type{}, err
		}
		return soMapOf(key, val), nil
	case e == "int":
		return intType(), nil
	case e == "float":
		return soFloatType(), nil
	case e == "string":
		return stringType(), nil
	case e == "bool":
		return soBoolType(), nil
	case e == "Hue":
		return soEnumType("Hue"), nil
	case e == "Probe", e == "Nest", e == "NestInner":
		return soClassType(e), nil
	}
	return schema.Type{}, fmt.Errorf("unmodelled BAML type expression %q; the bridge refuses to guess", expr)
}

// soBridgeUnit is one driveable unit: a generated class carrying one or more of the
// corpus's checks over a group's value.
//
// TWO BUNDLES, and the difference is load-bearing. BAML's attribute lexer REWRITES
// the expression text for five of the 719 cases (it doubles backslashes, so a BAML
// constraint cannot express a regex escape at all). The .baml project must be
// rendered from the SOURCE spelling — that is what the lexer consumes — while the
// native evaluator must be given the spelling stock actually evaluates and reports
// back in Check.Expression, or the differential would be comparing BAML's attribute
// lexer against Go's string literals rather than two engines over one expression.
//
// The original constraint oracle makes the same distinction (it feeds native
// c.retainedExpr()); the bridge previously did not, and ran its native leg on the
// pre-lexer spelling for those five rows.
type soBridgeUnit struct {
	// Method is the BAML function name.
	Method string
	Group  soBridgeGroup
	Cases  []soBridgeCase
	// Bundle carries the SOURCE spelling and is what the .baml project is rendered
	// from, and what the stock readback is interpreted against.
	Bundle *schema.Bundle
	// NativeBundle carries the STOCK-RETAINED spelling and is what the native leg
	// coerces and evaluates. For a case whose spelling survives the lexer unchanged
	// the two are identical.
	NativeBundle *schema.Bundle
}

// usesRetainedSpelling reports whether any of this unit's cases is one the BAML
// attribute lexer rewrote.
func (u soBridgeUnit) usesRetainedSpelling() bool {
	for _, c := range u.Cases {
		if c.Retained != "" && c.Retained != c.Expr {
			return true
		}
	}
	return false
}

// soBuildBridgeUnits turns the artifact into driveable units, batching exactly as
// constraintoracle does.
func soBuildBridgeUnits(exp soBridgeExport) ([]soBridgeUnit, error) {
	classes, enums := soBridgePrelude()
	byName := map[string]soBridgeGroup{}
	for _, g := range exp.Groups {
		if _, dup := byName[g.Name]; dup {
			return nil, fmt.Errorf("duplicate group %q in the bridge artifact", g.Name)
		}
		byName[g.Name] = g
	}
	// DUPLICATE CASE LABELS ARE REJECTED HERE, at the artifact boundary.
	//
	// Rejecting duplicate JSON keys is not enough: two distinct case OBJECTS may
	// reuse the same Label, and the label is what four downstream operations select
	// by — the @check attribute in the generated .baml, the per-site assertion, the
	// stock outcome lookup, and the native outcome lookup. All 719 cases would still
	// be counted while one case's evidence was read for another, which is a silent
	// mis-attribution rather than a loud failure.
	seenLabel := map[string]string{}
	for _, c := range exp.Cases {
		if prev, dup := seenLabel[c.Label]; dup {
			return nil, fmt.Errorf("duplicate case label %q in the bridge artifact (groups %q and %q); the "+
				"label is what the .baml attribute, the per-site assertion and both outcome lookups select "+
				"by, so one case's evidence would be read for another while the count still came to %d",
				c.Label, prev, c.Group, len(exp.Cases))
		}
		seenLabel[c.Label] = c.Group
	}

	unit := func(name string, g soBridgeGroup, cases []soBridgeCase) (soBridgeUnit, error) {
		base, err := soParseBAMLType(g.BAMLType)
		if err != nil {
			return soBridgeUnit{}, fmt.Errorf("group %q: %w", g.Name, err)
		}
		if got := soBaseTypeExpr(base); got != g.BAMLType {
			return soBridgeUnit{}, fmt.Errorf("group %q: the parsed type renders as %q but the corpus "+
				"declares %q; the bridge would drive a DIFFERENT shape", g.Name, got, g.BAMLType)
		}
		// Two field types from the same base: the SOURCE spelling for the .baml, the
		// STOCK-RETAINED spelling for the native evaluator. See soBridgeUnit.
		sourceType, nativeType := base, base
		for _, c := range cases {
			sourceType = soWith(sourceType, soCheck(c.Label, c.Expr))
			nativeType = soWith(nativeType, soCheck(c.Label, c.retainedExpr()))
		}
		build := func(t schema.Type) (*schema.Bundle, error) {
			cls := soClassOf(name, []schema.ClassField{soField("v", t)})
			b := &schema.Bundle{
				Target:  soClassType(name),
				Classes: append(append([]schema.ClassDef(nil), classes...), cls),
				Enums:   enums,
			}
			if err := b.RebuildIndexes(); err != nil {
				return nil, fmt.Errorf("group %q: %w", g.Name, err)
			}
			return b, nil
		}
		src, err := build(sourceType)
		if err != nil {
			return soBridgeUnit{}, err
		}
		nat, err := build(nativeType)
		if err != nil {
			return soBridgeUnit{}, err
		}
		return soBridgeUnit{Method: soFunctionName(name), Group: g, Cases: cases,
			Bundle: src, NativeBundle: nat}, nil
	}

	var out []soBridgeUnit
	for _, g := range exp.Groups {
		var batched []soBridgeCase
		for _, c := range exp.Cases {
			if c.Group == g.Name && !c.isolated() {
				batched = append(batched, c)
			}
		}
		if len(batched) == 0 {
			continue
		}
		u, err := unit(soBridgePrefix+"B_"+g.Name, g, batched)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	for _, c := range exp.Cases {
		if !c.isolated() {
			continue
		}
		g, ok := byName[c.Group]
		if !ok {
			return nil, fmt.Errorf("case %q references unknown group %q", c.Label, c.Group)
		}
		u, err := unit(soBridgePrefix+"I_"+c.Label, g, []soBridgeCase{c})
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// soRenderBridgeProject renders the bridge's own in-memory project.
func soRenderBridgeProject(units []soBridgeUnit) string {
	var b strings.Builder
	b.WriteString(soPrelude)
	classes, enums := soBridgePrelude()
	for _, name := range soBridgePreludeOrder {
		for _, e := range enums {
			if e.Name.Name == name {
				b.WriteString("\n")
				b.WriteString(soRenderEnum(e))
			}
		}
		for _, c := range classes {
			if c.Name.Name == name {
				b.WriteString("\n")
				b.WriteString(soRenderClass(c))
			}
		}
	}
	for _, u := range units {
		cls := u.Bundle.Classes[len(u.Bundle.Classes)-1]
		b.WriteString("\n")
		b.WriteString(soRenderClass(cls))
		b.WriteString("\n")
		b.WriteString(soRenderFunction(strings.TrimPrefix(u.Method, soFunctionPrefix), u.Bundle.Target))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// soWantBridgeCases pins how many of the 719 cases the serving-shaped harness
// drives, and how they land. A case moving between buckets has to be acknowledged.
const (
	soWantBridgeCases = 719
	// soWantBridgeGroups is the number of `this` values behind them.
	soWantBridgeGroups = 28
)

// soBridgeCollectorRefusals are the cases the serving-shaped harness cannot carry
// all the way to the evaluator, because the TEST-ONLY #662 collector refuses the
// COERCION first.
//
// Every one of them is the same limitation, and it is a limitation of the witness
// rather than of either engine: the collector re-serializes a float leaf as its
// shortest round-trip decimal (9223372036854776000) while production coerce keeps
// the source token (9223372036854775808.0), and the exact big.Rat divergence check
// compares those two DECIMALS rather than the float64 they both denote. The same
// finding is recorded by the guard_float_2p63 fixture.
//
// This is a PINNED SET, not a skip. Every label here must be observed refusing, and
// every observed refusal must be listed — so a 13th case joining them, or one of
// these starting to work, fails until it is acknowledged. And each one's pinned
// NATIVE outcome must already be "unsupported", which is what makes the refusal
// safe: the collector can only be hiding a decline native was going to make anyway,
// never an answer.
var soBridgeCollectorRefusals = map[string]string{
	"asint_float_abs":         "2^63 float leaf: collector decimal round-trip",
	"asint_float_compare":     "2^63 float leaf: collector decimal round-trip",
	"asint_float_divisibleby": "2^63 float leaf: collector decimal round-trip",
	"asint_float_even":        "2^63 float leaf: collector decimal round-trip",
	"asint_float_int":         "2^63 float leaf: collector decimal round-trip",
	"asint_float_integer":     "2^63 float leaf: collector decimal round-trip",
	"asint_float_odd":         "2^63 float leaf: collector decimal round-trip",
	"asint_float_string":      "2^63 float leaf: collector decimal round-trip",
	"asint_list_elem_even":    "2^63 float element: collector decimal round-trip",
	"asint_list_sum":          "2^63 float element: collector decimal round-trip",
	"asint_reject_odd":        "2^63 float element: collector decimal round-trip",
	"asint_select_even":       "2^63 float element: collector decimal round-trip",
}

// TestServingOracleBridgeCorpus drives every case of the 719-case constraint
// corpus through the SERVING-shaped harness and requires the fail-closed contract
// to hold over all of them.
//
// Three things are asserted per case, and none of them is an aggregate:
//
//  1. the STOCK outcome the serving path observes reproduces the outcome
//     constraintoracle pinned for it — so the two harnesses agree about BAML;
//  2. the NATIVE outcome, evaluated over the value THIS harness coerced (rather
//     than a hand-written ConstraintValue), reproduces the pinned native outcome;
//     and
//  3. native never produces a boolean stock did not, nor a different one.
func TestServingOracleBridgeCorpus(t *testing.T) {
	soEnsureRuntime(t)
	exp := soBridgeCorpus(t)

	// The prelude this file models must be the prelude the producer declares.
	var rendered strings.Builder
	classes, enums := soBridgePrelude()
	for _, name := range soBridgePreludeOrder {
		for _, e := range enums {
			if e.Name.Name == name {
				rendered.WriteString("\n" + soRenderEnum(e))
			}
		}
		for _, c := range classes {
			if c.Name.Name == name {
				rendered.WriteString("\n" + soRenderClass(c))
			}
		}
	}
	if got, want := soNormaliseBAML(rendered.String()), soNormaliseBAML(exp.Prelude); got != want {
		t.Fatalf("the bridge's schema model of the shared prelude is not what constraintoracle declares, so "+
			"every case would be driven against a different shape:\n  modelled %q\n  declared %q", got, want)
	}

	if len(exp.Cases) != soWantBridgeCases || len(exp.Groups) != soWantBridgeGroups {
		t.Fatalf("the bridge artifact carries %d cases over %d groups, want %d over %d. The serving oracle "+
			"consumes the constraint corpus as a regression source, so a change in its size has to be "+
			"acknowledged here.", len(exp.Cases), len(exp.Groups), soWantBridgeCases, soWantBridgeGroups)
	}

	units := soBridgeUnits
	if len(units) == 0 {
		t.Fatal("the bridge built no units; every case below would be silently skipped")
	}
	rt, env, err := soBridgeRuntime(units)
	if err != nil {
		t.Fatalf("create the bridge runtime: %v", err)
	}

	driven, violations := 0, []string{}
	stockAgree, nativeAgree := 0, 0
	refused := map[string]bool{}
	for _, u := range units {
		stock, serr := soBridgeDriveStock(rt, env, u)
		violations = append(violations, soBridgeAssertSites(u, stock, serr)...)
		for _, c := range u.Cases {
			driven++
			gotStock := soBridgeStockOutcome(stock, serr, c.Label)
			if gotStock != c.Stock {
				violations = append(violations, fmt.Sprintf(
					"%s: the SERVING path observes stock %q where constraintoracle pinned %q "+
						"(method %s, raw %q, expr %q)",
					c.Label, gotStock, c.Stock, u.Method, u.Group.Input, c.retainedExpr()))
				continue
			}
			stockAgree++

			gotNative, nerr := soBridgeNativeOutcome(u, c)
			if strings.HasPrefix(gotNative, "coercion:") {
				// The collector refused the coercion, so the evaluator was never
				// reached. Allowed ONLY for the pinned set, and only where native was
				// already declining — a refusal that hid an ANSWER would be a
				// fail-closed hole.
				refused[c.Label] = true
				if why := soBridgeRefusalViolation(c.Label, c.Native); why != "" {
					violations = append(violations, fmt.Sprintf("%s: %s (%s: %v)",
						c.Label, why, gotNative, nerr))
				}
				continue
			}
			if gotNative != c.Native {
				violations = append(violations, fmt.Sprintf(
					"%s: the SERVING path observes native %q where constraintoracle pinned %q "+
						"(raw %q, expr %q, err %v)",
					c.Label, gotNative, c.Native, u.Group.Input, c.retainedExpr(), nerr))
				continue
			}
			nativeAgree++

			// The contract itself, over the LIVE outcomes.
			if gotNative == "unsupported" {
				continue
			}
			if gotStock != gotNative {
				violations = append(violations, fmt.Sprintf(
					"%s: native answered %q where stock produced %q — expr %q",
					c.Label, gotNative, gotStock, c.retainedExpr()))
			}
		}
	}
	if driven != soWantBridgeCases {
		t.Fatalf("drove %d cases, want %d; a silently skipped case is a silently dropped regression source",
			driven, soWantBridgeCases)
	}
	// The pinned refusal set must be exact in BOTH directions.
	for label := range soBridgeCollectorRefusals {
		if !refused[label] {
			violations = append(violations, fmt.Sprintf(
				"%s is pinned as a collector refusal but the serving harness drove it successfully; the "+
					"#662 limitation it records has been fixed and the entry must be removed", label))
		}
	}
	if len(soBridgeCollectorRefusals) == 0 && len(refused) > 0 {
		violations = append(violations, "the refusal set is empty but refusals were observed")
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		shown := violations
		if len(shown) > 20 {
			shown = shown[:20]
		}
		t.Fatalf("the serving-shaped harness does not reproduce the 719-case corpus; %d disagreement(s):\n  %s",
			len(violations), strings.Join(shown, "\n  "))
	}
	t.Logf("719-case corpus re-driven serving-shaped: %d cases over %d units; stock reproduced %d/%d, "+
		"native reproduced %d/%d, %d blocked by the #662 collector's float divergence check (all of them "+
		"already pinned native=unsupported)", driven, len(units), stockAgree, driven, nativeAgree, driven,
		len(refused))
}

// soBridgeRefusalViolation decides whether a collector refusal is ACCEPTABLE for a
// case, returning the reason it is not.
//
// It is a function rather than inline code so both of its refusal reasons can be
// driven directly (TestServingOracleBridgeRefusalPolicy): in a normal run every
// observed refusal is listed and already declining, so neither branch would ever
// execute and neither could be shown to work.
func soBridgeRefusalViolation(label, pinnedNative string) string {
	if _, listed := soBridgeCollectorRefusals[label]; !listed {
		return "the collector refused the coercion and the case is not in soBridgeCollectorRefusals; a " +
			"case cannot drop out of the regression source without being acknowledged"
	}
	if pinnedNative != "unsupported" {
		return fmt.Sprintf("the collector refused the coercion, but constraintoracle pins native=%q for it "+
			"— the refusal would be HIDING an answer rather than a decline", pinnedNative)
	}
	return ""
}

// soBridgeCorpus loads and strictly decodes the artifact.
func soBridgeCorpus(t *testing.T) soBridgeExport {
	t.Helper()
	exp, err := soLoadBridgeExport(soBridgePath)
	if err != nil {
		t.Fatalf("load %s: %v\nRegenerate it with BAML_CONSTRAINT_BRIDGE_WRITE=1 go test -tags integration "+
			"./internal/debaml/constraintoracle -run TestConstraintCorpusBridgeExport", soBridgePath, err)
	}
	return exp
}

// soNormaliseBAML collapses blank lines and trailing space so two renderings of the
// same declarations compare equal without pinning whitespace conventions.
func soNormaliseBAML(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// soBridgeUnits is the bridge's driveable units, built once during runtime setup
// so their declarations can be registered in the process-global type map.
var soBridgeUnits []soBridgeUnit

// soBridgeUnitsFromArtifact loads the artifact and builds the units.
func soBridgeUnitsFromArtifact() ([]soBridgeUnit, error) {
	exp, err := soLoadBridgeExport(soBridgePath)
	if err != nil {
		return nil, fmt.Errorf("load the 719-case bridge artifact: %w (regenerate it with "+
			"BAML_CONSTRAINT_BRIDGE_WRITE=1 go test -tags integration ./internal/debaml/constraintoracle "+
			"-run TestConstraintCorpusBridgeExport)", err)
	}
	return soBuildBridgeUnits(exp)
}

// soBridgeRuntimeState memoizes the bridge runtime: one project, one compile.
var (
	soBridgeRT      baml.BamlRuntime
	soBridgeEnv     map[string]string
	soBridgeErr     error
	soBridgeStarted bool
)

// soBridgeRuntime compiles the bridge project once.
func soBridgeRuntime(units []soBridgeUnit) (baml.BamlRuntime, map[string]string, error) {
	if soBridgeStarted {
		return soBridgeRT, soBridgeEnv, soBridgeErr
	}
	soBridgeStarted = true
	src := soRenderBridgeProject(units)
	soBridgeEnv = soEnvSnapshot()
	soBridgeRT, soBridgeErr = baml.CreateRuntime("./baml_src",
		map[string]string{"constraint_bridge.baml": src}, soBridgeEnv)
	return soBridgeRT, soBridgeEnv, soBridgeErr
}

// soBridgeDriveStock parses one unit through the stock CFFI.
func soBridgeDriveStock(rt baml.BamlRuntime, env map[string]string, u soBridgeUnit) (soStockEnvelope, error) {
	fmt.Fprintf(os.Stderr, "serving-oracle: bridge BEGIN %s\n", u.Method)
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": u.Group.Input, "stream": false},
		Env:    env,
	}
	encoded, err := args.Encode()
	if err != nil {
		return soStockEnvelope{}, err
	}
	res, callErr := rt.CallFunctionParse(context.Background(), u.Method, encoded)
	fmt.Fprintf(os.Stderr, "serving-oracle: bridge END   %s\n", u.Method)
	if callErr != nil {
		return soClassifyStockError(callErr), callErr
	}
	var sites []soStockSite
	identity, doc, rerr := soReadStockResult(u.Bundle, res, "$", &sites)
	if rerr != nil {
		return soStockEnvelope{}, rerr
	}
	return soStockEnvelope{Kind: soStockValue, Identity: identity, JSON: doc, Sites: sites}, nil
}

// soBridgeAssertSites checks the WHOLE observed check collection of one unit
// against the artifact: cardinality, declaration ORDER, label, the exact
// CFFI-retained EXPRESSION text, and the node each check ran at.
//
// Checking only the status of a label-selected site (which is what this did) leaves
// the expression contract unasserted: the five rows whose spelling the attribute
// lexer rewrites would stay green if the retained spelling changed while their
// boolean did not. All five currently decline natively, which masks the gap rather
// than closing it.
func soBridgeAssertSites(u soBridgeUnit, env soStockEnvelope, callErr error) []string {
	return soBridgeAssertSitesWith(u, env, callErr, soAllBridgeSiteChecks)
}

// soBridgeSiteChecks names the individual comparisons soBridgeAssertSitesWith
// makes.
//
// Every real caller uses [soAllBridgeSiteChecks]; the set exists so
// TestServingOracleBridgeRetainedExpressionGuardBites can turn exactly ONE
// comparison off and show that it, and nothing else, is what catches a drifted
// retained expression. That test also asserts every field of the default set is
// true, so a comparison cannot be quietly disabled for the real path.
type soBridgeSiteChecks struct {
	Cardinality bool
	Order       bool
	Expression  bool
	Path        bool
	Certified   bool
}

// soAllBridgeSiteChecks is the set every production caller of the bridge uses.
var soAllBridgeSiteChecks = soBridgeSiteChecks{
	Cardinality: true, Order: true, Expression: true, Path: true, Certified: true,
}

// soBridgeAssertSitesWith is soBridgeAssertSites with the comparison set made
// explicit. Only the counterfactual test passes anything but the full set.
func soBridgeAssertSitesWith(u soBridgeUnit, env soStockEnvelope, callErr error,
	checks soBridgeSiteChecks) []string {
	if callErr != nil {
		// Stock rejected the node, so there is no collection to compare. The per-case
		// outcome check covers the rejection itself.
		return nil
	}
	// Every case whose pinned stock outcome is a real check status must appear, in
	// declaration order. A "no-checks" case is emitted with no entry at all.
	var want []soBridgeCase
	for _, c := range u.Cases {
		if c.Stock != "no-checks" {
			want = append(want, c)
		}
	}
	var out []string
	if checks.Cardinality && len(env.Sites) != len(want) {
		return []string{fmt.Sprintf(
			"%s: stock reported %d check(s) where the artifact declares %d:\n      observed %s",
			u.Method, len(env.Sites), len(want), soRenderStockSites(env.Sites))}
	}
	if len(env.Sites) != len(want) {
		// Cardinality checking is off and the lengths differ: compare what overlaps
		// rather than indexing past the end.
		return out
	}
	for i, c := range want {
		got := env.Sites[i]
		if checks.Order && got.Label != c.Label {
			out = append(out, fmt.Sprintf("%s: check %d is labelled %q, the artifact declares %q at that "+
				"position — the collection is not in declaration order", u.Method, i, got.Label, c.Label))
			continue
		}
		if checks.Expression && got.Expression != c.retainedExpr() {
			out = append(out, fmt.Sprintf("%s: check %d (%s) — stock RETAINED the expression\n"+
				"      observed %q\n      artifact %q\n"+
				"    The retained spelling is what the native evaluator is fed, so a change here means the "+
				"two engines are being compared over different source.",
				u.Method, i, c.Label, got.Expression, c.retainedExpr()))
		}
		if checks.Path && got.Path != "$.v" {
			out = append(out, fmt.Sprintf("%s: check %d (%s) ran at %s, want $.v",
				u.Method, i, c.Label, got.Path))
		}
		if checks.Certified && !got.Certified {
			out = append(out, fmt.Sprintf("%s: check %d (%s) is an UNCERTIFIED root site; every bridge check "+
				"is a nested field and must come from the raw CFFI tree", u.Method, i, c.Label))
		}
	}
	return out
}

// soBridgeStockOutcome maps one case's stock observation onto constraintoracle's
// vocabulary, so the two harnesses are compared in the same terms.
func soBridgeStockOutcome(env soStockEnvelope, callErr error, label string) string {
	if callErr != nil {
		return "error"
	}
	for _, s := range env.Sites {
		if s.Label != label {
			continue
		}
		switch s.Status {
		case "succeeded":
			return "true"
		case "failed":
			return "false"
		default:
			return s.Status
		}
	}
	return "no-checks"
}

// soBridgeNativeOutcome evaluates one case through the SERVING path: the group's
// raw text is extracted and coerced by production, and the predicate then runs over
// the value that coercion produced.
//
// That is the difference from constraintoracle's own native leg, which is handed a
// hand-written ConstraintValue: here the value is derived, so a case whose outcome
// depends on coercion is covered end to end.
func soBridgeNativeOutcome(u soBridgeUnit, c soBridgeCase) (string, error) {
	// The NATIVE bundle: the spelling stock reports back, not the spelling the .baml
	// was written with.
	f := servingOracleFixture{Name: c.Label, Bundle: u.NativeBundle, Raw: u.Group.Input}
	env := soRunNative(f)
	switch env.Kind {
	case soNativeCoercionError, soNativeNoCandidate, soNativeUnmodelled, soNativeCollectorDiverged:
		return "coercion:" + string(env.Kind), fmt.Errorf("%s", env.Message)
	}
	for _, s := range env.Sites {
		if s.Label != c.Label {
			continue
		}
		switch s.Outcome {
		case constraintOutcomeTrue:
			return "true", nil
		case constraintOutcomeFalse:
			return "false", nil
		case constraintOutcomeUnsupported:
			return "unsupported", nil
		}
	}
	return "no-checks", nil
}

// TestServingOracleBridgeStrictDecoding proves the artifact boundary is STRICT, in
// every direction that could silently drop evidence.
//
// The bridge is the oracle's only encoded-evidence boundary. A decoder that ignored
// an unknown field, kept the last of two duplicate keys, or accepted a second
// document would let half the corpus go missing with no error at all.
func TestServingOracleBridgeStrictDecoding(t *testing.T) {
	// The REAL artifact must load, or every negative below would pass trivially.
	if _, err := soLoadBridgeExport(soBridgePath); err != nil {
		t.Fatalf("the checked-in artifact does not load: %v", err)
	}
	base := `{"version":1,"prelude":"","groups":[{"name":"g","baml_type":"int","input":"{}"}],` +
		`"cases":[{"label":"l","group":"g","expr":"this","stock":"true","native":"true"}]}`
	if err := soWriteTemp(t, base); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown field at the top level",
			`{"version":1,"prelude":"","groups":[],"cases":[],"surprise":1}`, "surprise"},
		{"unknown field inside a case",
			`{"version":1,"prelude":"","groups":[],"cases":[{"label":"l","group":"g","expr":"e",` +
				`"stock":"true","native":"true","weight":3}]}`, "weight"},
		{"duplicate key inside a case",
			`{"version":1,"prelude":"","groups":[],"cases":[{"label":"a","label":"b","group":"g",` +
				`"expr":"e","stock":"true","native":"true"}]}`, "duplicate object key"},
		{"duplicate key at the top level",
			`{"version":1,"version":2,"prelude":"","groups":[],"cases":[]}`, "duplicate object key"},
		{"trailing content after the document",
			`{"version":1,"prelude":"","groups":[],"cases":[]} {"version":1}`, "trailing content"},
		{"a version this consumer does not understand",
			`{"version":99,"prelude":"","groups":[],"cases":[]}`, "version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corpus.json")
			if err := os.WriteFile(path, []byte(tc.doc), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := soLoadBridgeExport(path)
			if err == nil {
				t.Fatal("the strict decoder ACCEPTED a document that drops or contradicts evidence")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name the problem (%q): %v", tc.want, err)
			}
		})
	}
	// CONTROL: the well-formed document loads, so the refusals above are about the
	// defects rather than about the decoder rejecting everything.
	path := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(path, []byte(base), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := soLoadBridgeExport(path)
	if err != nil {
		t.Fatalf("the well-formed control does not load: %v", err)
	}
	if len(got.Cases) != 1 || got.Cases[0].Label != "l" {
		t.Fatalf("the control decoded to %+v", got)
	}
}

// soWriteTemp is a sanity check that the base document used above is itself valid,
// so a typo in it cannot make every negative case pass for the wrong reason.
func soWriteTemp(t *testing.T, doc string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return err
	}
	if _, err := soLoadBridgeExport(path); err != nil {
		return fmt.Errorf("the base document for the strict-decoding table is itself invalid: %w", err)
	}
	return nil
}

// TestServingOracleBridgeTypeParserRoundTrips proves the bridge drives each group
// against the type the corpus DECLARES.
//
// The parser is small on purpose, and it is not trusted: every group's expression
// is parsed and rendered back, and an expression the parser does not model is an
// error rather than a guess.
func TestServingOracleBridgeTypeParserRoundTrips(t *testing.T) {
	exp := soBridgeCorpus(t)
	if len(exp.Groups) == 0 {
		t.Fatal("no groups; the round-trip claim would be vacuous")
	}
	seen := map[string]bool{}
	for _, g := range exp.Groups {
		parsed, err := soParseBAMLType(g.BAMLType)
		if err != nil {
			t.Errorf("group %q: %v", g.Name, err)
			continue
		}
		if got := soBaseTypeExpr(parsed); got != g.BAMLType {
			t.Errorf("group %q: %q parsed and rendered back as %q", g.Name, g.BAMLType, got)
		}
		seen[g.BAMLType] = true
	}
	// The corpus must exercise more than one shape, or the round trip proves nothing
	// about the parser.
	if len(seen) < 5 {
		t.Errorf("the groups cover only %d distinct type expressions: %v", len(seen), seen)
	}
	// An unmodelled expression is refused rather than guessed at.
	for _, bad := range []string{"", "Widget", "map<int>", "tuple<int, int>"} {
		if _, err := soParseBAMLType(bad); err == nil {
			t.Errorf("the parser accepted the unmodelled expression %q; it must refuse rather than guess", bad)
		}
	}
}

// TestServingOracleBridgeRefusalPolicy drives the refusal policy directly, because
// a normal run never reaches either of its rejecting branches.
func TestServingOracleBridgeRefusalPolicy(t *testing.T) {
	if len(soBridgeCollectorRefusals) == 0 {
		t.Fatal("the pinned refusal set is empty; the policy would accept nothing and prove nothing")
	}
	var listed string
	for label := range soBridgeCollectorRefusals {
		listed = label
		break
	}
	if why := soBridgeRefusalViolation(listed, "unsupported"); why != "" {
		t.Errorf("a LISTED refusal whose case already declines must be accepted; got %q", why)
	}
	if why := soBridgeRefusalViolation(listed, "true"); why == "" {
		t.Error("a listed refusal whose case is pinned native=true must be REJECTED: the refusal would be " +
			"hiding an answer rather than a decline")
	}
	if why := soBridgeRefusalViolation("a_case_that_is_not_listed", "unsupported"); why == "" {
		t.Error("an UNLISTED refusal must be rejected, or a case could drop out of the regression source " +
			"unnoticed")
	}
}

// soWantRetainedRewrites is how many of the 719 cases BAML's attribute lexer
// rewrites. Pinned, because everything below is a claim about them: if the count
// went to zero the retained-spelling machinery would be exercising nothing.
const soWantRetainedRewrites = 5

// TestServingOracleBridgeRetainedSpellingIsWired proves the two spellings go to the
// two places they belong, for the cases where they actually differ.
//
// BAML's attribute lexer DOUBLES backslashes, so for five of the 719 expressions the
// text stock evaluates is not the text the .baml was written with. The project must
// be rendered from the SOURCE spelling (that is what the lexer consumes) and the
// native evaluator must be fed the RETAINED spelling (that is what stock evaluated),
// or the differential compares BAML's lexer against Go's string literals.
//
// All five currently decline natively, so a boolean comparison alone cannot see the
// difference — which is exactly why the wiring is asserted structurally here and the
// observed expression is asserted per site by soBridgeAssertSites.
func TestServingOracleBridgeRetainedSpellingIsWired(t *testing.T) {
	soEnsureRuntime(t)
	exp := soBridgeCorpus(t)

	rewritten := map[string]soBridgeCase{}
	for _, c := range exp.Cases {
		if c.Retained != "" && c.Retained != c.Expr {
			rewritten[c.Label] = c
		}
	}
	if len(rewritten) != soWantRetainedRewrites {
		t.Fatalf("the artifact carries %d lexer-rewritten expressions, want %d. Every claim below is about "+
			"them, so a change in that population has to be acknowledged.", len(rewritten), soWantRetainedRewrites)
	}

	// The rendered PROJECT must carry the source spelling, and never the retained one.
	project := soRenderBridgeProject(soBridgeUnits)
	for label, c := range rewritten {
		if !strings.Contains(project, c.Expr) {
			t.Errorf("%s: the .baml project does not carry the SOURCE spelling %q, which is what the "+
				"attribute lexer consumes", label, c.Expr)
		}
		if strings.Contains(project, c.Retained) {
			t.Errorf("%s: the .baml project carries the RETAINED spelling %q; that is stock's OUTPUT, not "+
				"its input, and writing it back would double the backslashes again", label, c.Retained)
		}
	}

	// The NATIVE bundle must carry the retained spelling, and the source bundle the
	// source spelling — checked on the unit that actually holds them.
	found := map[string]bool{}
	for _, u := range soBridgeUnits {
		if !u.usesRetainedSpelling() {
			continue
		}
		src := soBridgeConstraintsOf(t, u.Bundle)
		nat := soBridgeConstraintsOf(t, u.NativeBundle)
		for label, c := range rewritten {
			gotSrc, okSrc := src[label]
			gotNat, okNat := nat[label]
			if !okSrc || !okNat {
				continue
			}
			found[label] = true
			if gotSrc != c.Expr {
				t.Errorf("%s: the project bundle carries %q, want the SOURCE spelling %q", label, gotSrc, c.Expr)
			}
			if gotNat != c.Retained {
				t.Errorf("%s: the NATIVE bundle carries %q, want the STOCK-RETAINED spelling %q",
					label, gotNat, c.Retained)
			}
		}
	}
	if len(found) != len(rewritten) {
		t.Fatalf("only %d of %d rewritten cases were located in a unit; the wiring claim would cover less "+
			"than it says", len(found), len(rewritten))
	}

	// And the LIVE observation: stock reports the retained spelling back for each of
	// them. This is what soBridgeAssertSites compares per site on every run; asserting
	// it here as well names the five rows the contract exists for.
	rt, env, err := soBridgeRuntime(soBridgeUnits)
	if err != nil {
		t.Fatalf("bridge runtime: %v", err)
	}
	seen := 0
	for _, u := range soBridgeUnits {
		if !u.usesRetainedSpelling() {
			continue
		}
		stock, callErr := soBridgeDriveStock(rt, env, u)
		if callErr != nil {
			t.Fatalf("%s: stock rejected the unit carrying the rewritten expressions: %v", u.Method, callErr)
		}
		for _, s := range stock.Sites {
			c, ok := rewritten[s.Label]
			if !ok {
				continue
			}
			seen++
			if s.Expression != c.Retained {
				t.Errorf("%s: stock retained %q, the artifact records %q", s.Label, s.Expression, c.Retained)
			}
			if s.Expression == c.Expr {
				t.Errorf("%s: stock retained the SOURCE spelling unchanged, so this case no longer witnesses "+
					"the lexer rewrite and must be re-derived", s.Label)
			}
		}
	}
	if seen != len(rewritten) {
		t.Fatalf("observed %d of %d rewritten expressions live; the rest were never driven", seen, len(rewritten))
	}
	t.Logf("retained spelling wired for %d lexer-rewritten expressions: project=source, native=retained, "+
		"stock observed to report the retained form", seen)
}

// soBridgeConstraintsOf maps label -> expression for the `v` field of a unit's
// generated class.
func soBridgeConstraintsOf(t *testing.T, b *schema.Bundle) map[string]string {
	t.Helper()
	out := map[string]string{}
	cls := b.Classes[len(b.Classes)-1]
	for _, f := range cls.Fields {
		for _, c := range f.Type.Meta.Constraints {
			if c.Label == nil {
				continue
			}
			if _, dup := out[*c.Label]; dup {
				t.Fatalf("class %s declares the label %q twice; the lookup would hide one", cls.Name.Name, *c.Label)
			}
			out[*c.Label] = c.Expression
		}
	}
	return out
}

// soRetainedGuardWitness is the row the counterfactual below drifts. It is one of
// the five expressions BAML's attribute lexer rewrites, so its retained spelling is
// genuinely different from its source spelling — which is the whole point.
const soRetainedGuardWitness = "f_lines"

// soWantRetainedWitnessSites is the EXACT size of the check collection the witness
// unit reports: every non-isolated case of the `const` group, which is where
// f_lines rides.
//
// Pinned exactly rather than bounded, and that is the point. A lower bound would
// leave the counterfactual technically satisfied after a batching or corpus change
// that shrank this witness to a two-site collection: the drift would still be
// caught, but no longer against a collection large enough for "order and
// cardinality were held stable while only the expression moved" to mean anything —
// the whole-collection half of the proof would have retired silently while the
// 719-wide bridge stayed green.
//
// IF THE CORPUS REALLY CHANGES this constant must be updated DELIBERATELY, as a
// conscious edit alongside the regenerated artifact, not adjusted to whatever the
// run happened to produce.
const soWantRetainedWitnessSites = 410

// TestServingOracleBridgeRetainedExpressionGuardBites proves the per-site
// EXPRESSION comparison is the UNIQUE catcher of a drifted retained spelling.
//
// The five lexer-rewritten rows all decline natively, so no boolean comparison can
// see their expression change; and the whole-collection assertion has four other
// comparisons that could mask which one fired. This holds label, `$.v` path,
// declaration order, cardinality and boolean status STABLE, changes ONLY the
// retained expression text, and shows the pair the proof needs:
//
//	drifted + every comparison enabled          -> FAILS   (the guard catches it)
//	drifted + only the expression check disabled -> PASSES  (nothing else catches it)
//
// It is hermetic and deterministic: it reads the checked-in artifact and builds both
// envelopes in memory, with no CFFI call and no file mutation. The LIVE half — that
// stock really does report the retained spelling back — is
// TestServingOracleBridgeRetainedSpellingIsWired.
func TestServingOracleBridgeRetainedExpressionGuardBites(t *testing.T) {
	units, err := soBridgeUnitsFromArtifact()
	if err != nil {
		t.Fatalf("build the bridge units: %v", err)
	}
	// Every comparison must be on for the real path, or the counterfactual below
	// would be turning off something already off.
	all := reflect.ValueOf(soAllBridgeSiteChecks)
	for i := 0; i < all.NumField(); i++ {
		if !all.Field(i).Bool() {
			t.Fatalf("soAllBridgeSiteChecks.%s is false; the bridge's real path is not running every "+
				"comparison", all.Type().Field(i).Name)
		}
	}

	unit, witness, ok := soFindRetainedWitness(units, soRetainedGuardWitness)
	if !ok {
		t.Fatalf("no bridge unit carries the retained-expression row %q; the counterfactual would drift "+
			"nothing", soRetainedGuardWitness)
	}
	if witness.Retained == "" || witness.Retained == witness.Expr {
		t.Fatalf("%s no longer carries a retained spelling that differs from its source spelling "+
			"(expr %q, retained %q); pick another of the lexer-rewritten rows",
			witness.Label, witness.Expr, witness.Retained)
	}

	faithful := soBridgeFaithfulSites(t, unit)
	if len(faithful) != soWantRetainedWitnessSites {
		t.Fatalf("the %s witness unit reports %d check(s), want exactly %d.\n"+
			"The counterfactual below holds ORDER and CARDINALITY stable across the WHOLE collection while "+
			"moving one expression, so the collection's size is part of what it proves — a shrunk witness "+
			"would still catch the drift but would no longer demonstrate that. If the corpus or the "+
			"batching genuinely changed, update soWantRetainedWitnessSites deliberately.",
			witness.Label, len(faithful), soWantRetainedWitnessSites)
	}
	env := func(sites []soStockSite) soStockEnvelope {
		return soStockEnvelope{Kind: soStockValue, Identity: "class:x{}", JSON: "{}", Sites: sites}
	}

	// BASELINE. The faithful collection must be accepted, or "the drifted one fails"
	// would say nothing about the drift.
	if got := soBridgeAssertSites(unit, env(faithful), nil); len(got) > 0 {
		t.Fatalf("the FAITHFUL collection was rejected, so the counterfactual below would prove nothing:\n  %s",
			strings.Join(got, "\n  "))
	}

	// DRIFT: one character of one retained expression, and nothing else.
	drifted := append([]soStockSite(nil), faithful...)
	idx := -1
	for i := range drifted {
		if drifted[i].Label == witness.Label {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("%s is not in the witness unit's reported collection", witness.Label)
	}
	drifted[idx].Expression = witness.Retained + " "

	// Everything else is held stable, asserted rather than assumed.
	if len(drifted) != len(faithful) {
		t.Fatal("the drift changed the cardinality")
	}
	for i := range faithful {
		if drifted[i].Label != faithful[i].Label {
			t.Fatalf("the drift changed the label at position %d", i)
		}
		if drifted[i].Path != faithful[i].Path {
			t.Fatalf("the drift changed the path at position %d", i)
		}
		if drifted[i].Status != faithful[i].Status {
			t.Fatalf("the drift changed the boolean status at position %d", i)
		}
		if drifted[i].Certified != faithful[i].Certified {
			t.Fatalf("the drift changed the certification at position %d", i)
		}
		changed := drifted[i].Expression != faithful[i].Expression
		switch {
		case i == idx && !changed:
			t.Fatalf("the drift did NOT change the expression at position %d, so the counterfactual would "+
				"be comparing a collection against itself", i)
		case i != idx && changed:
			t.Fatalf("the drift also changed the expression at position %d; only %d may move", i, idx)
		}
	}

	// V7: the guard catches it.
	withGuard := soBridgeAssertSites(unit, env(drifted), nil)
	if len(withGuard) == 0 {
		t.Fatal("a drifted RETAINED expression was accepted with every comparison enabled; the native leg " +
			"would be evaluating a spelling stock never reported")
	}
	joined := strings.Join(withGuard, "\n  ")
	if !strings.Contains(joined, "RETAINED the expression") {
		t.Fatalf("the failure does not name the retained expression:\n  %s", joined)
	}

	// V8: nothing ELSE catches it. Exactly one comparison is turned off.
	noExpr := soAllBridgeSiteChecks
	noExpr.Expression = false
	if diff := soBridgeChecksDiff(soAllBridgeSiteChecks, noExpr); diff != []string{"Expression"}[0] {
		t.Fatalf("the counterfactual disables %q, want only Expression", diff)
	}
	if got := soBridgeAssertSitesWith(unit, env(drifted), nil, noExpr); len(got) > 0 {
		t.Fatalf("with ONLY the expression comparison disabled the drift was still caught, so the "+
			"expression assertion is not the unique catcher and this proof is not the one it claims:\n  %s",
			strings.Join(got, "\n  "))
	}
	t.Logf("retained-expression guard is the unique catcher: drift caught with every comparison enabled, "+
		"accepted with only the expression comparison disabled (witness %s, %d sites held stable)",
		witness.Label, len(faithful))
}

// soFindRetainedWitness locates the unit carrying a named case.
func soFindRetainedWitness(units []soBridgeUnit, label string) (soBridgeUnit, soBridgeCase, bool) {
	for _, u := range units {
		for _, c := range u.Cases {
			if c.Label == label {
				return u, c, true
			}
		}
	}
	return soBridgeUnit{}, soBridgeCase{}, false
}

// soBridgeFaithfulSites builds the check collection stock reports for a unit, from
// the artifact alone.
//
// It is deterministic and needs no CFFI: the artifact pins each case's outcome, and
// the retained spelling is what stock echoes back in Check.Expression. That stock
// really behaves this way is asserted live elsewhere
// (TestServingOracleBridgeRetainedSpellingIsWired and every bridge run); here the
// point is only that the two envelopes differ in exactly one field.
func soBridgeFaithfulSites(t *testing.T, u soBridgeUnit) []soStockSite {
	t.Helper()
	var out []soStockSite
	for _, c := range u.Cases {
		var status string
		switch c.Stock {
		case "true":
			status = "succeeded"
		case "false":
			status = "failed"
		case "no-checks":
			continue
		default:
			t.Fatalf("%s: the witness unit carries the outcome %q, which stock does not report as a check "+
				"status; pick a unit whose cases are all real statuses", c.Label, c.Stock)
		}
		out = append(out, soStockSite{
			Path: "$.v", Label: c.Label, Expression: c.retainedExpr(), Status: status, Certified: true,
		})
	}
	return out
}

// soBridgeChecksDiff names the fields that differ between two comparison sets, so
// the counterfactual can prove it disabled exactly one.
func soBridgeChecksDiff(a, b soBridgeSiteChecks) string {
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	var diff []string
	for i := 0; i < av.NumField(); i++ {
		if av.Field(i).Bool() != bv.Field(i).Bool() {
			diff = append(diff, av.Type().Field(i).Name)
		}
	}
	return strings.Join(diff, ",")
}

// TestServingOracleBridgeRejectsDuplicateCaseLabels pins the artifact-boundary
// check that duplicate JSON-key rejection cannot make.
//
// Two distinct case objects may reuse a Label without repeating any JSON key. The
// label is what the generated @check attribute, the per-site assertion and both
// outcome lookups select by, so a duplicate silently reads one case's evidence for
// another while the total still comes to 719.
func TestServingOracleBridgeRejectsDuplicateCaseLabels(t *testing.T) {
	base := soBridgeExport{
		Version: soBridgeVersion,
		Groups:  []soBridgeGroup{{Name: "g", BAMLType: "int", Input: `{"v":1}`}},
		Cases: []soBridgeCase{
			{Label: "dup", Group: "g", Expr: "this > 0", Stock: "true", Native: "true"},
			{Label: "other", Group: "g", Expr: "this > 1", Stock: "false", Native: "false"},
		},
	}
	// CONTROL first: unique labels build, so the rejection below is about the
	// duplication rather than about the fixture being malformed.
	if _, err := soBuildBridgeUnits(base); err != nil {
		t.Fatalf("the unique-label control does not build: %v", err)
	}

	dup := base
	dup.Cases = append([]soBridgeCase(nil), base.Cases...)
	// A DIFFERENT case object reusing the label: no JSON key is repeated, so the
	// strict decoder cannot see it.
	dup.Cases[1].Label = "dup"
	_, err := soBuildBridgeUnits(dup)
	if err == nil {
		t.Fatal("two case objects sharing a Label were ACCEPTED; all 719 cases would still be counted while " +
			"one case's evidence was read for another")
	}
	if !strings.Contains(err.Error(), "duplicate case label") {
		t.Fatalf("the refusal does not name the duplicate label: %v", err)
	}

	// And the live artifact is clean, so the check is not merely latent.
	exp := soBridgeCorpus(t)
	seen := map[string]bool{}
	for _, c := range exp.Cases {
		if seen[c.Label] {
			t.Fatalf("the checked-in artifact carries the duplicate label %q", c.Label)
		}
		seen[c.Label] = true
	}
	if len(seen) != len(exp.Cases) {
		t.Fatalf("the artifact has %d cases under %d distinct labels", len(exp.Cases), len(seen))
	}
}
