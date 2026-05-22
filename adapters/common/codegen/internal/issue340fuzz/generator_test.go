package issue340fuzz

import (
	"encoding/json"
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// TestDynamicSafeNeverSelfRefs asserts the dynamic-safe schema
// generator never produces a class graph that allows any class to
// reach itself (including transitive paths through optional / list /
// map wrappers). This is the contract the dynamic emitter relies on
// to avoid the upstream BAML TypeBuilder self-reference bug
// (TODO(upstream-self-ref)).
func TestDynamicSafeNeverSelfRefs(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := DynamicSafeSchemaGen().Draw(rt, "schema")
		fresh := AnalyzeGraph(schema)
		if fresh.HasSelfRef {
			rt.Fatalf("dynamic-safe schema reached self via reachability:\n%s", schemaDump(schema))
		}
		if fresh.HasMutualCycle {
			rt.Fatalf("dynamic-safe schema has mutual cycle:\n%s", schemaDump(schema))
		}
		if fresh.RequiresDynamicSkip {
			rt.Fatalf("dynamic-safe schema flagged RequiresDynamicSkip:\n%s", schemaDump(schema))
		}
		// The schema's stamped flags must match a re-derivation
		// from AnalyzeGraph — otherwise the generator's stamping
		// drifted from the analyzer.
		if schema.HasSelfRef != fresh.HasSelfRef ||
			schema.HasMutualCycle != fresh.HasMutualCycle ||
			schema.RequiresDynamicSkip != fresh.RequiresDynamicSkip {
			rt.Fatalf("stamped flags drift from AnalyzeGraph\nstamped: self=%v mutual=%v skip=%v\nfresh:   self=%v mutual=%v skip=%v",
				schema.HasSelfRef, schema.HasMutualCycle, schema.RequiresDynamicSkip,
				fresh.HasSelfRef, fresh.HasMutualCycle, fresh.RequiresDynamicSkip)
		}
	})
}

// TestStaticGeneratorMayEmitSelfRef asserts the static generator,
// when configured with a maximum self-ref probability, eventually
// emits at least one self-ref schema across the rapid check budget.
// Sanity check that the injection path fires.
func TestStaticGeneratorMayEmitSelfRef(t *testing.T) {
	gen := SchemaGen(SchemaGenOptions{
		AllowSelfRef:       true,
		SelfRefProbability: 1.0,
	})
	saw := false
	for i := 0; i < 32 && !saw; i++ {
		schema := gen.Example(i + 1)
		if schema.HasSelfRef {
			saw = true
		}
		if schema.HasSelfRef != schema.RequiresDynamicSkip {
			t.Fatalf("self-ref must imply RequiresDynamicSkip\nschema: %s", schemaDump(schema))
		}
	}
	if !saw {
		t.Fatal("static generator with selfRefProbability=1.0 never produced a self-ref schema across 32 examples")
	}
}

// TestStaticValueTerminates asserts the value walker terminates for
// static schemas — even those with self-ref. Concretely: for any
// generated (schema, value), Walk returns without exceeding the
// per-class recursion cap. The walker's metadata reports peak depth
// per class; if the value generator obeyed the cap, no value
// exceeds MaxValueRecursion.
func TestStaticValueTerminates(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := SchemaGen(SchemaGenOptions{
			AllowSelfRef:       true,
			SelfRefProbability: 0.5,
		}).Draw(rt, "schema")
		value := ValueGen(schema).Draw(rt, "value")
		res, err := Walk(schema, value)
		if err != nil {
			rt.Fatalf("walk error: %v\nschema: %s\nvalue: %s",
				err, schemaDump(schema), valueDump(value))
		}
		for cls, depth := range res.Metadata.RecursionDepths {
			if depth > MaxValueRecursion {
				rt.Fatalf("class %q recursion depth %d exceeds cap %d",
					cls, depth, MaxValueRecursion)
			}
		}
	})
}

// TestRequiresDynamicSkipMatchesDetection asserts that for ANY
// generated schema (dynamic-safe or static), the
// RequiresDynamicSkip flag matches a fresh AnalyzeGraph derivation.
// This is the cross-check that the generator's stamping logic stays
// in sync with the analyzer used by the rest of the harness.
func TestRequiresDynamicSkipMatchesDetection(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Toggle a coin per check whether we draw from the
		// dynamic-safe or static generator. Rapid biases coverage
		// across both paths.
		allowSelfRef := rapid.Bool().Draw(rt, "allow_self_ref")
		opts := SchemaGenOptions{AllowSelfRef: allowSelfRef}
		if allowSelfRef {
			opts.SelfRefProbability = 0.5
		}
		schema := SchemaGen(opts).Draw(rt, "schema")
		fresh := AnalyzeGraph(schema)
		if schema.RequiresDynamicSkip != fresh.RequiresDynamicSkip {
			rt.Fatalf("flag drift: stamped=%v fresh=%v\nschema: %s",
				schema.RequiresDynamicSkip, fresh.RequiresDynamicSkip,
				schemaDump(schema))
		}
	})
}

// TestSchemaBoundsRespected asserts the generator never exceeds the
// scope-doc D9 budget: max 4 classes, max 3 enums, 1–5 fields per
// class plus a possible self-ref-injected extra optional, type
// depth ≤ 4.
func TestSchemaBoundsRespected(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := SchemaGen(SchemaGenOptions{
			AllowSelfRef:       true,
			SelfRefProbability: 0.5,
		}).Draw(rt, "schema")
		if len(schema.Classes) < 1 || len(schema.Classes) > MaxClasses {
			rt.Fatalf("class count %d outside [1, %d]", len(schema.Classes), MaxClasses)
		}
		if len(schema.Enums) > MaxEnums {
			rt.Fatalf("enum count %d > cap %d", len(schema.Enums), MaxEnums)
		}
		for _, cls := range schema.Classes {
			// +1 leeway: injectSelfRef appends one optional at
			// the back when AllowSelfRef + SelfRefProbability
			// trigger.
			if len(cls.Properties) < MinFieldsPerClass || len(cls.Properties) > MaxFieldsPerClass+1 {
				rt.Fatalf("class %q has %d fields, outside [%d, %d]",
					cls.Name, len(cls.Properties), MinFieldsPerClass, MaxFieldsPerClass+1)
			}
			for _, prop := range cls.Properties {
				if d := typeDepth(prop.Type); d > MaxTypeDepth {
					rt.Fatalf("class %q field %q type depth %d > cap %d",
						cls.Name, prop.Name, d, MaxTypeDepth)
				}
			}
		}
	})
}

// TestValueBoundsRespected asserts list / map values stay within
// the scope-doc length bounds.
func TestValueBoundsRespected(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := DynamicSafeSchemaGen().Draw(rt, "schema")
		value := ValueGen(schema).Draw(rt, "value")
		if err := checkValueBounds(value); err != nil {
			rt.Fatalf("value bound violation: %v\nschema: %s\nvalue: %s",
				err, schemaDump(schema), valueDump(value))
		}
	})
}

// TestWalkRoundTripNormalizesAbsent asserts the central walker
// contract: parsing the mock LLM output and normalizing it against
// the schema produces a byte-identical match to the walker's
// expected output. This is the property the three-way oracle
// (added in PR-B/PR-C) relies on.
func TestWalkRoundTripNormalizesAbsent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		schema := SchemaGen(SchemaGenOptions{
			AllowSelfRef:       true,
			SelfRefProbability: 0.5,
		}).Draw(rt, "schema")
		value := ValueGen(schema).Draw(rt, "value")
		res, err := Walk(schema, value)
		if err != nil {
			rt.Fatalf("walk: %v", err)
		}
		normalized, err := NormalizeMockToExpected(schema, res.MockLLMContent, schema.RootClass)
		if err != nil {
			rt.Fatalf("normalize: %v\nmock: %s\nschema: %s",
				err, string(res.MockLLMContent), schemaDump(schema))
		}
		if string(normalized) != string(res.Expected) {
			rt.Fatalf("round-trip mismatch\nmock:       %s\nexpected:   %s\nnormalized: %s\nschema: %s",
				string(res.MockLLMContent), string(res.Expected), string(normalized),
				schemaDump(schema))
		}
	})
}

// TestEdgeValuesAreReachable spins the value generator a fixed
// number of times against a primitive-only schema and asserts the
// curated edge values (empty string, 0, negative integers, both
// booleans) all appear in the sample. Catches a future change to
// drawString / drawInt that accidentally turns off the edge-bias
// path.
func TestEdgeValuesAreReachable(t *testing.T) {
	schema := FuzzSchema{
		Classes: []FuzzClass{{
			Name: "F340_C0",
			Properties: []FuzzProperty{
				{Name: "F340_field_0", Type: FuzzType{Kind: KindString}},
				{Name: "F340_field_1", Type: FuzzType{Kind: KindInt}},
				{Name: "F340_field_2", Type: FuzzType{Kind: KindBool}},
			},
		}},
		RootClass: "F340_C0",
	}
	gen := ValueGen(schema)
	var (
		sawEmptyString bool
		sawZero        bool
		sawNeg         bool
		sawTrue        bool
		sawFalse       bool
	)
	// 256 samples keeps the test fast (< 100ms) while making the
	// probability of missing any biased edge negligible.
	for i := 0; i < 256; i++ {
		v := gen.Example(i + 1)
		s := v.Fields[0].Value.String
		n := v.Fields[1].Value.Int
		b := v.Fields[2].Value.Bool
		if s == "" {
			sawEmptyString = true
		}
		if n == 0 {
			sawZero = true
		}
		if n < 0 {
			sawNeg = true
		}
		if b {
			sawTrue = true
		} else {
			sawFalse = true
		}
		if sawEmptyString && sawZero && sawNeg && sawTrue && sawFalse {
			return
		}
	}
	t.Fatalf("edge coverage incomplete after 256 draws: empty=%v zero=%v neg=%v true=%v false=%v",
		sawEmptyString, sawZero, sawNeg, sawTrue, sawFalse)
}

// typeDepth computes the maximum wrapper nesting depth in a type.
// Class refs are atomic from a depth perspective (they don't
// contain nested types in the structural sense).
func typeDepth(t FuzzType) int {
	switch t.Kind {
	case KindOptional, KindList, KindMap:
		if t.Inner == nil {
			return 1
		}
		return 1 + typeDepth(*t.Inner)
	default:
		return 1
	}
}

// checkValueBounds walks a FuzzValue tree and verifies every list /
// map length lives inside the v1 budget.
func checkValueBounds(v FuzzValue) error {
	switch v.Kind {
	case KindList:
		if len(v.Items) > MaxListLen {
			return fmt.Errorf("list len %d > cap %d", len(v.Items), MaxListLen)
		}
		for _, item := range v.Items {
			if err := checkValueBounds(item); err != nil {
				return err
			}
		}
	case KindMap:
		if len(v.MapEntries) > MaxMapLen {
			return fmt.Errorf("map len %d > cap %d", len(v.MapEntries), MaxMapLen)
		}
		for _, e := range v.MapEntries {
			if err := checkValueBounds(e.Value); err != nil {
				return err
			}
		}
	case KindOptional:
		if v.OptionalShape == OptionalPresent && v.Inner != nil {
			return checkValueBounds(*v.Inner)
		}
	case KindClassRef:
		for _, fv := range v.Fields {
			if err := checkValueBounds(fv.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaDump(s FuzzSchema) string {
	b, _ := json.MarshalIndent(s, "", "  ")
	return string(b)
}

func valueDump(v FuzzValue) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
