package bamlfuzz

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSchemaJSONRoundTrip pins the IR's stable serialization. The
// failure-replay envelope and the seed-corpus files both round-trip
// FuzzSchema through JSON; if the field set or naming drifts, this
// test catches it before downstream consumers write stale artifacts.
func TestSchemaJSONRoundTrip(t *testing.T) {
	original := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "FuzzClass0",
				Properties: []FuzzProperty{
					{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindString}},
					{Name: "Fuzz_field_1", Type: FuzzType{
						Kind: KindOptional,
						Inner: &FuzzType{
							Kind: KindClassRef,
							Ref:  "FuzzClass0",
						},
					}},
				},
			},
		},
		Enums: []FuzzEnum{{
			Name:   "FuzzEnum0",
			Values: []string{"FuzzEnum0_V0", "FuzzEnum0_V1"},
		}},
		RootClass:           "FuzzClass0",
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

// TestAnalyzeGraphDetectsSelfRef wires a manual schema where the
// root class's own property tree references itself directly (via an
// optional). HasSelfRef must fire; HasMutualCycle must not (no
// other class is involved); RequiresDynamicSkip must fire because
// direct self-ref is not realizable through dynamic TypeBuilder.
func TestAnalyzeGraphDetectsSelfRef(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	optSelf := FuzzType{Kind: KindOptional, Inner: &self}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindInt}},
				{Name: "Fuzz_field_1", Type: optSelf},
			},
		}},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasSelfRef {
		t.Errorf("HasSelfRef = false, want true")
	}
	if got.HasMutualCycle {
		t.Errorf("HasMutualCycle = true, want false (no other-class hop)")
	}
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip = false, want true")
	}
}

// TestAnalyzeGraphDetectsMutualCycle wires an A↔B mutual cycle
// through OTHER classes (neither A nor B references itself
// directly). HasMutualCycle must fire; HasSelfRef must not fire;
// RequiresDynamicSkip must be true because mutual cycles are gated by
// TODO(upstream-mutual-rec-dynamic-crash) until BAML's cgo
// TypeBuilder stops aborting on mutual-cycle dynamic schemas.
func TestAnalyzeGraphDetectsMutualCycle(t *testing.T) {
	refA := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	refB := FuzzType{Kind: KindClassRef, Ref: "FuzzClass1"}
	optA := FuzzType{Kind: KindOptional, Inner: &refA}
	optB := FuzzType{Kind: KindOptional, Inner: &refB}
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "FuzzClass0",
				Properties: []FuzzProperty{
					{Name: "Fuzz_field_0", Type: optB},
				},
			},
			{
				Name: "FuzzClass1",
				Properties: []FuzzProperty{
					{Name: "Fuzz_field_0", Type: optA},
				},
			},
		},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if got.HasSelfRef {
		t.Errorf("HasSelfRef = true, want false (no class references itself directly)")
	}
	if !got.HasMutualCycle {
		t.Errorf("HasMutualCycle = false, want true")
	}
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip = false, want true (mutual cycle gated by TODO(upstream-mutual-rec-dynamic-crash))")
	}
}

// TestAnalyzeGraphSelfRefThroughList asserts a self-ref reached
// through a list wrapper is still flagged as HasSelfRef (the
// analyzer descends into list / map / optional inners equally).
func TestAnalyzeGraphSelfRefThroughList(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	listSelf := FuzzType{Kind: KindList, Inner: &self}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: listSelf},
			},
		}},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasSelfRef {
		t.Errorf("HasSelfRef should fire for self-ref through list, got %+v", got)
	}
	if !got.RequiresDynamicSkip {
		t.Errorf("RequiresDynamicSkip should fire for self-ref through list")
	}
}

// TestAnalyzeGraphSelfRefThroughMap asserts a self-ref reached
// through a map value wrapper is also flagged as HasSelfRef.
func TestAnalyzeGraphSelfRefThroughMap(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	mapSelf := FuzzType{
		Kind:  KindMap,
		Key:   &FuzzType{Kind: KindString},
		Inner: &self,
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: mapSelf},
			},
		}},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if !got.HasSelfRef {
		t.Errorf("HasSelfRef should fire for self-ref through map value, got %+v", got)
	}
}

// TestAnalyzeGraphDAGStaysSafe asserts a class graph with no cycles
// is stamped clean.
func TestAnalyzeGraphDAGStaysSafe(t *testing.T) {
	refC1 := FuzzType{Kind: KindClassRef, Ref: "FuzzClass1"}
	schema := FuzzSchema{
		Classes: []FuzzClass{
			{
				Name: "FuzzClass0",
				Properties: []FuzzProperty{
					{Name: "Fuzz_field_0", Type: refC1},
				},
			},
			{
				Name: "FuzzClass1",
				Properties: []FuzzProperty{
					{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindString}},
				},
			},
		},
		RootClass: "FuzzClass0",
	}
	got := AnalyzeGraph(schema)
	if got.HasSelfRef || got.HasMutualCycle || got.RequiresDynamicSkip {
		t.Errorf("expected clean DAG flags, got %+v", got)
	}
}

// TestWalkOptionalContract pins the absent-vs-null contract: walker
// expected JSON always includes absent optionals as JSON null
// (matching both static and dynamic paths), while the mock JSON
// omits absent keys (absent omits the key, null emits explicit null).
func TestWalkOptionalContract(t *testing.T) {
	optStr := FuzzType{Kind: KindOptional, Inner: &FuzzType{Kind: KindString}}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: optStr},
				{Name: "Fuzz_field_1", Type: optStr},
				{Name: "Fuzz_field_2", Type: optStr},
			},
		}},
		RootClass: "FuzzClass0",
	}
	value := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "FuzzClass0",
		Fields: []FuzzFieldValue{
			{Name: "Fuzz_field_0", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalPresent,
				Inner: &FuzzValue{Kind: KindString, String: "hello"},
			}},
			{Name: "Fuzz_field_1", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalAbsent,
			}},
			{Name: "Fuzz_field_2", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalNull,
			}},
		},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	wantMock := `{"Fuzz_field_0":"hello","Fuzz_field_2":null}`
	wantExpected := `{"Fuzz_field_0":"hello","Fuzz_field_1":null,"Fuzz_field_2":null}`
	if string(res.MockLLMContent) != wantMock {
		t.Errorf("mock mismatch\nwant: %s\ngot:  %s", wantMock, string(res.MockLLMContent))
	}
	if string(res.Expected) != wantExpected {
		t.Errorf("expected mismatch\nwant: %s\ngot:  %s", wantExpected, string(res.Expected))
	}

	normalized, err := NormalizeMockToExpected(schema, res.MockLLMContent, "FuzzClass0")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if string(normalized) != string(res.Expected) {
		t.Errorf("normalize round-trip\nwant: %s\ngot:  %s", string(res.Expected), string(normalized))
	}
}

// TestWalkKindNull renders a required-null leaf in both the mock
// and expected paths to confirm KindNull emits JSON null on both
// sides without going through the optional path.
func TestWalkKindNull(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: FuzzType{Kind: KindNull}},
				{Name: "Fuzz_field_1", Type: FuzzType{Kind: KindString}},
			},
		}},
		RootClass: "FuzzClass0",
	}
	value := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "FuzzClass0",
		Fields: []FuzzFieldValue{
			{Name: "Fuzz_field_0", Value: FuzzValue{Kind: KindNull}},
			{Name: "Fuzz_field_1", Value: FuzzValue{Kind: KindString, String: "hi"}},
		},
	}
	res, err := Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := `{"Fuzz_field_0":null,"Fuzz_field_1":"hi"}`
	if string(res.MockLLMContent) != want {
		t.Errorf("KindNull mock mismatch\nwant: %s\ngot:  %s", want, string(res.MockLLMContent))
	}
	if string(res.Expected) != want {
		t.Errorf("KindNull expected mismatch\nwant: %s\ngot:  %s", want, string(res.Expected))
	}
	normalized, err := NormalizeMockToExpected(schema, res.MockLLMContent, "FuzzClass0")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if string(normalized) != want {
		t.Errorf("KindNull normalize mismatch\nwant: %s\ngot:  %s", want, string(normalized))
	}
}

// TestWalkMapKeysPreserveInsertionOrder asserts the walker emits map
// entries in FuzzValue.MapEntries slice order, byte-identical for both
// MockLLMContent and Expected. This is the property the strict
// map key-order assertion in order.go's walkType(KindMap) relies on:
// without it, the gate is materially unverified because every walked
// map already shares the same canonical (sorted) shape.
func TestWalkMapKeysPreserveInsertionOrder(t *testing.T) {
	mapType := FuzzType{
		Kind:  KindMap,
		Key:   &FuzzType{Kind: KindString},
		Inner: &FuzzType{Kind: KindInt},
	}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: mapType},
			},
		}},
		RootClass: "FuzzClass0",
	}
	mkValue := func(entries []FuzzMapEntry) FuzzValue {
		return FuzzValue{
			Kind:      KindClassRef,
			ClassName: "FuzzClass0",
			Fields: []FuzzFieldValue{{
				Name: "Fuzz_field_0", Value: FuzzValue{Kind: KindMap, MapEntries: entries},
			}},
		}
	}
	cases := []struct {
		name    string
		entries []FuzzMapEntry
		want    string
	}{
		{
			name: "z_a_m",
			entries: []FuzzMapEntry{
				{Key: "kZ", Value: FuzzValue{Kind: KindInt, Int: 1}},
				{Key: "kA", Value: FuzzValue{Kind: KindInt, Int: 2}},
				{Key: "kM", Value: FuzzValue{Kind: KindInt, Int: 3}},
			},
			want: `{"Fuzz_field_0":{"kZ":1,"kA":2,"kM":3}}`,
		},
		{
			name: "m_a_z",
			entries: []FuzzMapEntry{
				{Key: "kM", Value: FuzzValue{Kind: KindInt, Int: 1}},
				{Key: "kA", Value: FuzzValue{Kind: KindInt, Int: 2}},
				{Key: "kZ", Value: FuzzValue{Kind: KindInt, Int: 3}},
			},
			want: `{"Fuzz_field_0":{"kM":1,"kA":2,"kZ":3}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Walk(schema, mkValue(tc.entries))
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if string(res.MockLLMContent) != tc.want {
				t.Errorf("mock keys not in MapEntries order\nwant: %s\ngot:  %s", tc.want, string(res.MockLLMContent))
			}
			if string(res.Expected) != tc.want {
				t.Errorf("expected keys not in MapEntries order\nwant: %s\ngot:  %s", tc.want, string(res.Expected))
			}
			normalized, err := NormalizeMockToExpected(schema, res.MockLLMContent, "FuzzClass0")
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if string(normalized) != tc.want {
				t.Errorf("normalize round-trip not in MapEntries order\nwant: %s\ngot:  %s", tc.want, string(normalized))
			}
		})
	}
}

// TestWalkRecordsPeakRecursionDepth pins the M3 fix: the walker
// must record the peak per-class depth observed, not zero. The
// previous implementation deferred decrement and then copied the
// final (always-zero) depths into metadata.
func TestWalkRecordsPeakRecursionDepth(t *testing.T) {
	self := FuzzType{Kind: KindClassRef, Ref: "FuzzClass0"}
	optSelf := FuzzType{Kind: KindOptional, Inner: &self}
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "FuzzClass0",
			Properties: []FuzzProperty{
				{Name: "Fuzz_field_0", Type: optSelf},
			},
		}},
		RootClass: "FuzzClass0",
	}
	// Two nested instances: outer + inner via the present
	// optional. Peak depth must be 2.
	inner := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "FuzzClass0",
		Fields: []FuzzFieldValue{{
			Name: "Fuzz_field_0", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalAbsent,
			},
		}},
	}
	outer := FuzzValue{
		Kind:      KindClassRef,
		ClassName: "FuzzClass0",
		Fields: []FuzzFieldValue{{
			Name: "Fuzz_field_0", Value: FuzzValue{
				Kind: KindOptional, OptionalShape: OptionalPresent,
				Inner: &inner,
			},
		}},
	}
	res, err := Walk(schema, outer)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if got := res.Metadata.RecursionDepths["FuzzClass0"]; got != 2 {
		t.Errorf("peak depth for FuzzClass0 = %d, want 2", got)
	}
}
