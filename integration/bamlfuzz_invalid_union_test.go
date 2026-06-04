//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
)

// unionClassOrMapSchema builds the boundaryml/baml#3690 shape: a
// top-level union of `FuzzClass1 | map<string,int>`, where FuzzClass1
// carries an optional float field BAML's codegen can leak as a null key
// into the map arm's serialization.
func unionClassOrMapSchema() bamlfuzz.FuzzSchema {
	floatT := bamlfuzz.FuzzType{Kind: bamlfuzz.KindFloat}
	intT := bamlfuzz.FuzzType{Kind: bamlfuzz.KindInt}
	strT := bamlfuzz.FuzzType{Kind: bamlfuzz.KindString}
	return bamlfuzz.FuzzSchema{
		Classes: []bamlfuzz.FuzzClass{{
			Name: "FuzzClass1",
			Properties: []bamlfuzz.FuzzProperty{
				{Name: "Fuzz_field_0", Type: bamlfuzz.FuzzType{Kind: bamlfuzz.KindOptional, Inner: &floatT}},
			},
		}},
		RootType: &bamlfuzz.FuzzType{
			Kind: bamlfuzz.KindUnion,
			Variants: []bamlfuzz.FuzzType{
				{Kind: bamlfuzz.KindClassRef, Ref: "FuzzClass1"},
				{Kind: bamlfuzz.KindMap, Key: &strT, Inner: &intT},
			},
		},
	}
}

// TestDeriveUnionChoices_LeakedNullPicksMapArm pins the boundaryml/baml#3690
// arm-derivation fix: a map-arm payload carrying a leaked null key from
// the sibling class arm still resolves to the map arm. Before the fix the
// class arm rejected the unknown `k0` and the map arm rejected the null
// `Fuzz_field_0`, so pickUnionArm returned no choice and the order walker
// hard-failed with ErrSchemaOrderUnsupported.
func TestDeriveUnionChoices_LeakedNullPicksMapArm(t *testing.T) {
	schema := unionClassOrMapSchema()
	dyn := json.RawMessage(`{"Fuzz_field_0":null,"k0":-26}`)
	choices, err := deriveUnionChoicesFromParsed(schema, dyn)
	if err != nil {
		t.Fatalf("deriveUnionChoicesFromParsed: %v", err)
	}
	choice, ok := choices[""]
	if !ok {
		t.Fatalf("expected a union choice at root path, got none (%v)", choices)
	}
	if choice.Kind != bamlfuzz.KindMap || choice.Index != 1 {
		t.Errorf("expected map arm (index 1), got %+v", choice)
	}
}

// TestDeriveUnionChoices_RealClassStillPicksClassArm guards the fix's
// scope: a genuine class-instance payload still resolves to the (narrower)
// class arm, so the null tolerance does not slacken arm selection for
// real values.
func TestDeriveUnionChoices_RealClassStillPicksClassArm(t *testing.T) {
	schema := unionClassOrMapSchema()
	dyn := json.RawMessage(`{"Fuzz_field_0":1.5}`)
	choices, err := deriveUnionChoicesFromParsed(schema, dyn)
	if err != nil {
		t.Fatalf("deriveUnionChoicesFromParsed: %v", err)
	}
	choice, ok := choices[""]
	if !ok {
		t.Fatalf("expected a union choice at root path, got none (%v)", choices)
	}
	if choice.Kind != bamlfuzz.KindClassRef || choice.Index != 0 {
		t.Errorf("expected class arm (index 0), got %+v", choice)
	}
}

// TestCheckInvalidOrderC_LeakedNullDoesNotHardFail exercises the full axis-C
// order-parity path end to end: dynclient leaked the null key, REST returned
// the clean map. Derivation resolves the map arm and the null-tolerant parity
// walker strips the leak from both sides, so the key order agrees and the
// check neither reports a diagnostic nor hard-fails.
func TestCheckInvalidOrderC_LeakedNullDoesNotHardFail(t *testing.T) {
	c := bamlfuzz.InvalidOracleCase{Schema: unionClassOrMapSchema(), PreserveSchemaOrder: true}
	dyn := json.RawMessage(`{"Fuzz_field_0":null,"k0":-26}`)
	rest := json.RawMessage(`{"k0":-26}`)
	msg, fail := checkInvalidOrderC(c, dyn, rest)
	if fail {
		t.Errorf("expected no hard fail, got fail=true msg=%q", msg)
	}
	if msg != "" {
		t.Errorf("expected no diagnostic, got %q", msg)
	}
}

// TestCheckInvalidOrderC_RealOrderMismatchStillFails confirms the path still
// catches a genuine map insertion-order swap once the leaked null is stripped.
func TestCheckInvalidOrderC_RealOrderMismatchStillFails(t *testing.T) {
	c := bamlfuzz.InvalidOracleCase{Schema: unionClassOrMapSchema(), PreserveSchemaOrder: true}
	dyn := json.RawMessage(`{"Fuzz_field_0":null,"k0":1,"k1":2}`)
	rest := json.RawMessage(`{"k1":2,"k0":1}`)
	msg, fail := checkInvalidOrderC(c, dyn, rest)
	if !fail {
		t.Errorf("expected a hard fail for the key-order swap, got fail=false msg=%q", msg)
	}
}
