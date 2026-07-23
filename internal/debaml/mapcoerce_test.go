package debaml

import (
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// Map coercion coverage (#581 map re-enable): Parse and parseStream now CLAIM a
// map in a proven position — clean object input, an exact string/enum/string-
// literal(-union) key match, and an in-scope value — emitting entries in parsed
// INPUT key order, which the bamlfuzz parse-recovery differential proves equals
// BAML's observable map-key order. Shapes the differential cannot prove decline
// at COERCE time (a missed enum/literal key, a duplicate key, a case-fold-
// uncertain key/value, a deferred Mcoerce-d value, a non-object input) and fall
// back to BAML. The coerceViaBundle / coerceStreamViaBundle bypasses below drive
// the map coercers directly (skipping only the checkSupported gate) so per-shape
// coerceMap behavior stays covered independent of the extract/gate path.

// coerceViaBundle drives the native FINAL-parse pipeline MINUS the
// checkSupported gate: lower → validate → extract → coerce. It mirrors Parse's
// body exactly (a lower/validate failure or a missing JSON candidate still
// surfaces as the unsupported sentinel) and lets the coerceMap tests exercise
// per-shape map coercion (partial-value skips, key-miss declines, dual ordering)
// directly, without threading each case through checkSupported and extraction.
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
	out, err := coerce(bundle, bundle.Target, parsed, nil, nil)
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
// coerceMap-level decline (or a lower/validate/extract decline), NOT a
// checkSupported gate decline (which the bypass skips).
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
// checkSupported gate, for the same reason as coerceViaBundle: it lets the
// coerceStreamMap tests exercise per-shape stream map coercion directly, without
// threading each case through checkSupported and the streaming extractor.
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
// list element, which checkSupportedType reaches through its TypeList arm
// (walking Elem into the map arm).
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
// union variant, admitted by checkUnionMapVariant (string key) and scored by the
// two-phase union coercer against the string arm.
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
// reachable only through a NAMED class ref, so this exercises checkSupported's
// per-class walk (each reachable class validated as its own bundle entry).
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

// TestParse_MapContainingSchemasClaim pins the #581 re-enable end-to-end through
// Parse (gate + extract + coerce, not the coerce bypass): a map in EVERY proven
// reachable position now CLAIMS, emitting entries in INPUT key order — a
// top-level field, a map value, a list element, a union arm, a nested class
// field, and — crucially — the non-null arm of an OPTIONAL map that the
// checkSupportedType nullable fast path waves through (null input claims null;
// a non-null object claims the map). The keys are deliberately out of lexical
// order so a byte-exact assertion catches any reordering regression.
func TestParse_MapContainingSchemasClaim(t *testing.T) {
	// Top-level map field — byte-exact input key order.
	mustParseExact(t, mapStringIntSchema(), `{"scores":{"z":1,"a":2}}`, `{"scores":{"z":1,"a":2}}`)

	// Map VALUE is a class: map keys stay in INPUT order, class fields in SCHEMA
	// order (dual-ordering). Both entries are complete, so both are kept.
	mustParseExact(t, mapStringItemSchema(),
		`{"by_id":{"z":{"label":"zed","id":"3"},"a":{"label":"ay","id":"1"}}}`,
		`{"by_id":{"z":{"id":"3","label":"zed"},"a":{"id":"1","label":"ay"}}}`)

	// Map nested as a LIST element.
	mustParseExact(t, listOfMapSchema(), `{"u":[{"b":1,"a":2}]}`, `{"u":[{"b":1,"a":2}]}`)

	// Map as a UNION arm: the object input picks the map arm (ObjectToMap) over
	// the string arm (JsonToString), so the map is claimed.
	mustParseExact(t, unionWithMapArmSchema(), `{"u":{"b":1,"a":2}}`, `{"u":{"b":1,"a":2}}`)

	// Map reached only through a NAMED class ref (per-class field walk).
	mustParseExact(t, classWithMapFieldSchema(), `{"u":{"scores":{"b":1,"a":2}}}`, `{"u":{"scores":{"b":1,"a":2}}}`)

	// OPTIONAL map: null input claims null; a non-null object claims the map.
	mustParse(t, optionalMapStrIntSchema(), `{"u":null}`, `{"u":null}`)
	mustParseExact(t, optionalMapStrIntSchema(), `{"u":{"b":1,"a":2}}`, `{"u":{"b":1,"a":2}}`)

	// The STREAM path shares the checkSupported gate and coerceStreamMap claims
	// a completed string-keyed map in input order.
	mustStream(t, mapStringIntSchema(), `{"scores":{"z":1,"a":2}}`, `{"scores":{"z":1,"a":2}}`)
}

// TestParse_OptionalMapUnsupportedValueDeclines pins the #596 review fix: an
// OPTIONAL map whose value is a gate-only-declined tree must DECLINE on non-null
// input, not over-claim. The canonical case is a direct list<multi-arm-union>
// under the map value — its isMultiArmUnion guard lives only in
// checkSupportedType's TypeList arm (array union_variant_hint), with no
// coerce-time equivalent. The gate's nullable fast path (`if u.Nullable { return
// nil }`) accepts the schema without walking the map value, so coerceUnionSafe
// re-proves the lone optional arm before claiming non-null input. Null input and
// a SUPPORTED optional map must still CLAIM (no regression).
func TestParse_OptionalMapUnsupportedValueDeclines(t *testing.T) {
	// optional map<string, list<int|string>>: the multi-arm-union list element is
	// out of scope, so non-null input DECLINES even though the map is optional.
	badValMap := oneField(&bamlutils.DynamicProperty{
		Type: "optional",
		Inner: &bamlutils.DynamicTypeSpec{
			Type: "map",
			Keys: &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{
				Type: "list",
				Items: &bamlutils.DynamicTypeSpec{
					Type:  "union",
					OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "string"}},
				},
			},
		},
	})
	requireUnsupported(t, badValMap, `{"u":{"a":[1,"x"]}}`)
	// Null input still claims null — the reproof only gates NON-null input.
	mustParse(t, badValMap, `{"u":null}`, `{"u":null}`)

	// A SUPPORTED optional map still claims on non-null input (no regression),
	// keeping INPUT key order; null still claims null.
	okMap := oneField(&bamlutils.DynamicProperty{
		Type: "optional",
		Inner: &bamlutils.DynamicTypeSpec{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Type: "string"},
		},
	})
	mustParseExact(t, okMap, `{"u":{"b":"y","a":"x"}}`, `{"u":{"b":"y","a":"x"}}`)
	mustParse(t, okMap, `{"u":null}`, `{"u":null}`)
}
