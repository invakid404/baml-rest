package debaml

import (
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// Map parity-decline (checkNoMap): Parse and parseStream now return the
// unsupported sentinel for a map in ANY reachable position, because native
// coerceMap emits entries in parsed INPUT key order and cannot PROVE that order
// equals BAML's observable map-key order under preserve-order. The generated
// seam falls back to BAML CFFI for those finals. coerceMap itself is UNCHANGED;
// the tests that used to drive it through Parse now reach it through the
// coerceViaBundle / coerceStreamViaBundle bypasses below (which skip only the
// checkSupported gate), so the map coercion behavior stays fully covered for the
// future PR that re-enables maps behind a proven map-order fixture.

// coerceViaBundle drives the native FINAL-parse pipeline MINUS the
// checkSupported gate: lower → validate → extract → coerce. It mirrors Parse's
// body exactly (a lower/validate failure or a missing JSON candidate still
// surfaces as the unsupported sentinel) and exists only so the coerceMap tests
// can exercise the unchanged map coercion now that Parse declines every
// map-containing schema at checkNoMap.
func coerceViaBundle(s *bamlutils.DynamicOutputSchema, raw string) (string, error) {
	bundle, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		return "", unsupportedErr("lower schema", err)
	}
	if err := bundle.ValidateOutput(); err != nil {
		return "", unsupportedErr("validate schema", err)
	}
	parsed, ok := extractCandidate(raw)
	if !ok {
		return "", unsupported("no cleanly-claimable JSON candidate")
	}
	out, err := coerce(bundle, bundle.Target, parsed, nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mustCoerce asserts coerceViaBundle succeeds and its output is SEMANTICALLY
// equal to want (the coerce-bypass twin of mustParse).
func mustCoerce(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := coerceViaBundle(s, raw)
	if err != nil {
		t.Fatalf("coerceViaBundle(%q) unexpected error: %v", raw, err)
	}
	if !jsonValueEqual(t, got, want) {
		t.Errorf("coerceViaBundle(%q):\n got %s\nwant %s", raw, got, want)
	}
}

// mustCoerceExact asserts coerceViaBundle succeeds and its output matches want
// BYTE-FOR-BYTE (the coerce-bypass twin of mustParseExact; reserved for the
// map-key/field emission-order contract).
func mustCoerceExact(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := coerceViaBundle(s, raw)
	if err != nil {
		t.Fatalf("coerceViaBundle(%q) unexpected error: %v", raw, err)
	}
	if got != want {
		t.Errorf("coerceViaBundle(%q): byte-exact mismatch\n got %s\nwant %s", raw, got, want)
	}
}

// requireCoerceUnsupported asserts coerceViaBundle declined (sentinel) — a
// coerceMap-level decline (or a lower/validate/extract decline), NOT the
// checkNoMap gate decline (which the bypass skips).
func requireCoerceUnsupported(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) {
	t.Helper()
	_, err := coerceViaBundle(s, raw)
	if err == nil {
		t.Fatalf("coerceViaBundle(%q): expected ErrDeBAMLParseUnsupported, got success", raw)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("coerceViaBundle(%q): expected ErrDeBAMLParseUnsupported, got %v", raw, err)
	}
}

// coerceStreamViaBundle drives the native STREAM-parse pipeline MINUS the
// checkSupported gate, for the same reason as coerceViaBundle: map-containing
// stream schemas now decline at checkNoMap, so the coerceStream map tests reach
// coerceStreamMap through this bypass.
func coerceStreamViaBundle(s *bamlutils.DynamicOutputSchema, raw string) (string, error) {
	bundle, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		return "", unsupportedErr("lower schema", err)
	}
	if err := bundle.ValidateOutput(); err != nil {
		return "", unsupportedErr("validate schema", err)
	}
	if err := checkNoStreamAnnotations(bundle); err != nil {
		return "", err
	}
	v, ok := streamExtractCandidate(raw)
	if !ok {
		return "", unsupported("stream: no cleanly-claimable JSON candidate")
	}
	out, err := coerceStream(bundle, bundle.Target, v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mustStreamCoerce asserts coerceStreamViaBundle succeeds and its output is
// semantically equal to want (the stream-bypass twin of mustStream).
func mustStreamCoerce(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := coerceStreamViaBundle(s, raw)
	if err != nil {
		t.Fatalf("coerceStreamViaBundle(%q): unexpected error: %v", raw, err)
	}
	if !jsonValueEqual(t, got, want) {
		t.Errorf("coerceStreamViaBundle(%q):\n got %s\nwant %s", raw, got, want)
	}
}

// listOfMapSchema is Root{ u: list<map<string,int>> } — a map nested inside a
// list element, which checkNoMap DECLINES by recursing into the list element
// (its TypeList arm walks Elem).
func listOfMapSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Type: "int"},
		},
	})
}

// unionWithMapArmSchema is Root{ u: string | map<string,int> } — a map as a
// union variant (reached via checkNoMap's union recursion, not the
// checkSupportedType/checkUnionMapVariant gate).
func unionWithMapArmSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "string"},
			{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
		},
	})
}

// classWithMapFieldSchema is Root{ u: C }, C{ scores: map<string,int> } — a map
// reachable only through a NAMED class ref (checkNoMap treats the ref as a leaf,
// so this proves checkSupported's per-class walk catches it).
func classWithMapFieldSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{Ref: "C"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
				Properties: props(kv("scores", &bamlutils.DynamicProperty{
					Type:   "map",
					Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
					Values: &bamlutils.DynamicTypeSpec{Type: "int"},
				})),
			}),
		),
	}
}

// TestParse_MapContainingSchemasDecline pins the checkNoMap parity-decline
// boundary: Parse returns the unsupported sentinel (so the seam falls back to
// BAML CFFI) for a map in EVERY reachable position — a top-level field, a map
// value, a list element, a union arm, a nested class field, and — crucially —
// the non-null arm of an OPTIONAL map, which the checkSupportedType nullable
// fast path would otherwise wave through (claiming null on null input). The
// unchanged coerceMap is exercised separately via coerceViaBundle.
func TestParse_MapContainingSchemasDecline(t *testing.T) {
	// Top-level map field.
	requireUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1,"b":2}}`)

	// Map nested as a map VALUE.
	requireUnsupported(t, mapStringItemSchema(), `{"by_id":{"a":{"id":"1","label":"x"}}}`)

	// Map nested as a LIST element.
	requireUnsupported(t, listOfMapSchema(), `{"u":[{"a":1}]}`)

	// Map as a UNION arm (checkNoMap's union recursion).
	requireUnsupported(t, unionWithMapArmSchema(), `{"u":{"a":1}}`)

	// Map reached only through a NAMED class ref (per-class field walk).
	requireUnsupported(t, classWithMapFieldSchema(), `{"u":{"scores":{"a":1}}}`)

	// OPTIONAL map: declines for BOTH null and non-null input. This is the
	// nested-position guarantee checkNoMap adds over checkSupportedType's
	// `if u.Nullable { return nil }` fast path — a null input would otherwise
	// claim null and a non-null object would claim the map.
	requireUnsupported(t, optionalMapStrIntSchema(), `{"u":null}`)
	requireUnsupported(t, optionalMapStrIntSchema(), `{"u":{"a":1}}`)

	// The STREAM path shares the checkSupported gate, so it declines maps too.
	requireStreamUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1`)
	requireStreamUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1}}`)
}

// TestCheckNoMap_Positions unit-checks checkNoMap directly across the type
// positions it must reject, independent of the extract/coerce pipeline.
func TestCheckNoMap_Positions(t *testing.T) {
	strT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	intT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	mapT := schema.Type{Kind: schema.TypeMap, Key: &strT, Value: &intT}

	rejects := []struct {
		name string
		t    schema.Type
	}{
		{"bare map", mapT},
		{"list of map", schema.Type{Kind: schema.TypeList, Elem: &mapT}},
		{"map value in map", schema.Type{Kind: schema.TypeMap, Key: &strT, Value: &mapT}},
		{"union arm map", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{strT, mapT}}}},
		{"nullable optional map", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Nullable: true, Variants: []schema.Type{mapT}}}},
		{"tuple item map", schema.Type{Kind: schema.TypeTuple, Items: []schema.Type{intT, mapT}}},
	}
	for _, c := range rejects {
		if err := checkNoMap(c.t); err == nil || !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: expected ErrDeBAMLParseUnsupported, got %v", c.name, err)
		}
	}

	// A map-free tree passes (named class/enum refs are leaves — their bodies
	// are walked per-class by checkSupported, not here).
	accepts := []struct {
		name string
		t    schema.Type
	}{
		{"primitive", strT},
		{"list of int", schema.Type{Kind: schema.TypeList, Elem: &intT}},
		{"scalar union", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{strT, intT}}}},
		{"class ref leaf", schema.Type{Kind: schema.TypeClass, Name: "C"}},
	}
	for _, c := range accepts {
		if err := checkNoMap(c.t); err != nil {
			t.Errorf("%s: expected nil, got %v", c.name, err)
		}
	}
}
