package issue340fuzz

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSchemaJSONRoundTrip pins the IR's stable serialization. The
// failure-replay envelope and the seed-corpus files both round-trip
// FuzzSchema through JSON; if the field set or naming drifts, this
// test catches it before downstream PRs (#340 PR-B / PR-C) write
// stale artifacts.
func TestSchemaJSONRoundTrip(t *testing.T) {
	original := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "F340_C0",
				Properties: []FuzzProperty{
					{Name: "F340_field_0", Type: FuzzType{Kind: KindString}},
					{Name: "F340_field_1", Type: FuzzType{
						Kind: KindOptional,
						Inner: &FuzzType{
							Kind: KindClassRef,
							Ref:  "F340_C0",
						},
					}},
				},
			},
		},
		Enums: []FuzzEnum{{
			Name:   "F340_E0",
			Values: []string{"F340_E0_V0", "F340_E0_V1"},
		}},
		RootClass:           "F340_C0",
		HasSelfRef:          true,
		HasMutualCycle:      false,
		HasUnion:            false,
		RequiresDynamicSkip: true,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded FuzzSchema
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip mismatch\noriginal: %#v\ndecoded: %#v", original, decoded)
	}
}

// TestAnalyzeGraphDetectsSelfRef wires a manual schema with a
// direct self-ref and asserts the analyzer flags it correctly.
func TestAnalyzeGraphDetectsSelfRef(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "F340_C0"}
	optSelf := FuzzType{Kind: KindOptional, Inner: &self}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "F340_C0",
			Properties: []FuzzProperty{
				{Name: "F340_field_0", Type: FuzzType{Kind: KindInt}},
				{Name: "F340_field_1", Type: optSelf},
			},
		}},
		RootClass: "F340_C0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasSelfRef {
		t.Errorf("HasSelfRef = false, want true")
	}
	if got.HasMutualCycle {
		t.Errorf("HasMutualCycle = true, want false (only direct self-ref)")
	}
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip = false, want true")
	}
}

// TestAnalyzeGraphDetectsMutualCycle wires a mutual A<->B cycle and
// asserts both HasMutualCycle and RequiresDynamicSkip fire.
func TestAnalyzeGraphDetectsMutualCycle(t *testing.T) {
	refA := FuzzType{Kind: KindClassRef, Ref: "F340_C0"}
	refB := FuzzType{Kind: KindClassRef, Ref: "F340_C1"}
	optA := FuzzType{Kind: KindOptional, Inner: &refA}
	optB := FuzzType{Kind: KindOptional, Inner: &refB}
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "F340_C0",
				Properties: []FuzzProperty{
					{Name: "F340_field_0", Type: optB},
				},
			},
			{
				Name: "F340_C1",
				Properties: []FuzzProperty{
					{Name: "F340_field_0", Type: optA},
				},
			},
		},
		RootClass: "F340_C0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasMutualCycle {
		t.Errorf("HasMutualCycle = false, want true")
	}
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip = false, want true")
	}
}

// TestAnalyzeGraphDAGStaysSafe asserts a class graph with no cycles
// is stamped clean.
func TestAnalyzeGraphDAGStaysSafe(t *testing.T) {
	refC1 := FuzzType{Kind: KindClassRef, Ref: "F340_C1"}
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "F340_C0",
				Properties: []FuzzProperty{
					{Name: "F340_field_0", Type: refC1},
				},
			},
			{
				Name: "F340_C1",
				Properties: []FuzzProperty{
					{Name: "F340_field_0", Type: FuzzType{Kind: KindString}},
				},
			},
		},
		RootClass: "F340_C0",
	}
	got := AnalyzeGraph(schema)
	if got.HasSelfRef || got.HasMutualCycle || got.RequiresDynamicSkip {
		t.Errorf("expected clean DAG flags, got %+v", got)
	}
}

// TestWalkOptionalContract pins the absent-vs-null contract: walker
// expected JSON renders absent and null the same way (both null),
// while the mock JSON differs (absent omits the key, null emits an
// explicit null).
func TestWalkOptionalContract(t *testing.T) {
	optStr := FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindString}}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "F340_C0",
			Properties: []FuzzProperty{
				{Name: "F340_field_0", Type: optStr},
				{Name: "F340_field_1", Type: optStr},
				{Name: "F340_field_2", Type: optStr},
			},
		}},
		RootClass: "F340_C0",
	}
	value := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "F340_C0",
		Fields: []FuzzFieldValue{
			{Name: "F340_field_0", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalPresent,
				Inner: &FuzzValue{Kind: KindString, String: "hello"},
			}},
			{Name: "F340_field_1", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalAbsent,
			}},
			{Name: "F340_field_2", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalNull,
			}},
		},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	wantMock := `{"F340_field_0":"hello","F340_field_2":null}`
	wantExpected := `{"F340_field_0":"hello","F340_field_1":null,"F340_field_2":null}`
	if string(res.MockLLMContent) != wantMock {
		t.Errorf("mock mismatch\nwant: %s\ngot:  %s", wantMock, string(res.MockLLMContent))
	}
	if string(res.Expected) != wantExpected {
		t.Errorf("expected mismatch\nwant: %s\ngot:  %s", wantExpected, string(res.Expected))
	}

	// Round trip: parse mock, normalize against schema, compare
	// byte-equal to the walker's expected.
	normalized, err := NormalizeMockToExpected(schema, res.MockLLMContent, "F340_C0")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if string(normalized) != string(res.Expected) {
		t.Errorf("normalize round-trip\nwant: %s\ngot:  %s", string(res.Expected), string(normalized))
	}
}

// TestWalkMapKeysSortLexically asserts the walker emits map entries
// in lexicographic order regardless of FuzzMapEntries input order.
// This is the canonical-form invariant the round-trip relies on.
func TestWalkMapKeysSortLexically(t *testing.T) {
	mapType := FuzzType{
		Kind:  KindMap,
		Key:   &FuzzType{Kind: KindString},
		Inner: &FuzzType{Kind: KindInt},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "F340_C0",
			Properties: []FuzzProperty{
				{Name: "F340_field_0", Type: mapType},
			},
		}},
		RootClass: "F340_C0",
	}
	value := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "F340_C0",
		Fields: []FuzzFieldValue{{
			Name: "F340_field_0", Value: FuzzValue{
				Kind: KindMap,
				MapEntries: []FuzzMapEntry{
					{Key: "kZ", Value: FuzzValue{Kind: KindInt, Int: 1}},
					{Key: "kA", Value: FuzzValue{Kind: KindInt, Int: 2}},
					{Key: "kM", Value: FuzzValue{Kind: KindInt, Int: 3}},
				},
			},
		}},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := `{"F340_field_0":{"kA":2,"kM":3,"kZ":1}}`
	if string(res.MockLLMContent) != want {
		t.Errorf("mock keys not lexicographic\nwant: %s\ngot:  %s", want, string(res.MockLLMContent))
	}
}
