package bamlfuzz

import (
	"fmt"

	"pgregory.net/rapid"
)

// invalidSchemaMutationKinds enumerates the Axis B mutation catalogue
// the generator picks from. Kept as a stable ordered slice so the
// rapid bit that selects an index has predictable shrink behaviour
// (lower indices = earlier kinds, which rapid biases toward).
var invalidSchemaMutationKinds = []string{
	MutationBothTypeAndRef,
	MutationMisspelledType,
	MutationDropItems,
	MutationDropInner,
	MutationDropKeysValues,
	MutationDropOneOf,
	MutationOverDepth,
}

// invalidJSONMutationKinds enumerates the Axis C JSON mutation
// catalogue. Same ordering rationale as invalidSchemaMutationKinds.
var invalidJSONMutationKinds = []string{
	JSONMutationDropKey,
	JSONMutationRetypeScalar,
	JSONMutationAddUnknownKey,
	JSONMutationSwapEnum,
	JSONMutationWrapInArray,
}

// InvalidDynamicSchemaGen returns a rapid generator for InvalidOracleCase
// values that drive Axis B: a valid base schema drawn from
// DynamicSafeSchemaGen, paired with a Mutation descriptor describing
// one targeted edit the lowering helper applies to the first root
// property. ExpectedOutcome is OutcomeBothReject — both dynclient and
// REST surfaces must error on the resulting dynamic output_schema.
//
// The schema mutation lives on the lowered DynamicOutputSchema, not on
// the FuzzSchema IR: the IR's pointer-fielded composites can't express
// shapes like "list with no items" cleanly, while the lowered form's
// raw nullable fields can. The case carries the valid base schema +
// the mutation kind; LowerInvalidToDynamicSchema rebuilds the
// post-mutation lowered form deterministically.
func InvalidDynamicSchemaGen() *rapid.Generator[InvalidOracleCase] {
	return rapid.Custom(func(t *rapid.T) InvalidOracleCase {
		cc := CoupledCaseGen(DynamicSafeSchemaGen()).Draw(t, "coupled_case")
		idx := rapid.IntRange(0, len(invalidSchemaMutationKinds)-1).Draw(t, "schema_mutation_kind")
		kind := invalidSchemaMutationKinds[idx]
		return InvalidOracleCase{
			Name:            fmt.Sprintf("invalid_dyn_%s", kind),
			Mode:            InvalidDynamicSchema,
			Mutation:        kind,
			ExpectedOutcome: OutcomeBothReject,
			Schema:          cc.Schema,
			Value:           cc.Value,
			MockLLMContent:  cc.Walk.MockLLMContent,
			Metadata:        cc.Walk.Metadata,
		}
	})
}

// InvalidJSONCoercionGen returns a rapid generator for InvalidOracleCase
// values that drive Axis C: a valid base schema + valid value, with the
// walker-emitted mock LLM JSON perturbed via one targeted JSON-aware
// edit. ExpectedOutcome is OutcomeConditional — when both surfaces
// succeed they must agree on semantic equality; when at least one
// errors both must error.
//
// When the chosen mutation can't apply to the drawn mock (e.g.,
// drop_key on a zero-key object), the generator falls back to
// add_unknown_key — a universal mutation that always perturbs the
// payload meaningfully. Without the fallback the case would
// degenerate to a no-op pass that the oracle silently accepts.
func InvalidJSONCoercionGen() *rapid.Generator[InvalidOracleCase] {
	return rapid.Custom(func(t *rapid.T) InvalidOracleCase {
		cc := CoupledCaseGen(DynamicSafeSchemaGen()).Draw(t, "coupled_case")
		idx := rapid.IntRange(0, len(invalidJSONMutationKinds)-1).Draw(t, "json_mutation_kind")
		kind := invalidJSONMutationKinds[idx]
		mutated, err := applyJSONMutation(cc.Walk.MockLLMContent, kind)
		if err != nil {
			fallback, ferr := applyJSONMutation(cc.Walk.MockLLMContent, JSONMutationAddUnknownKey)
			if ferr != nil {
				t.Fatalf("bamlfuzz: fallback JSON mutation failed: %v (original kind=%q err=%v)", ferr, kind, err)
			}
			mutated = fallback
			kind = JSONMutationAddUnknownKey
		}
		return InvalidOracleCase{
			Name:            fmt.Sprintf("invalid_json_%s", kind),
			Mode:            InvalidJSONCoercion,
			Mutation:        kind,
			ExpectedOutcome: OutcomeConditional,
			Schema:          cc.Schema,
			Value:           cc.Value,
			MockLLMContent:  mutated,
			Metadata:        cc.Walk.Metadata,
		}
	})
}
