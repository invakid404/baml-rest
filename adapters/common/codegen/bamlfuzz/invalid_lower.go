package bamlfuzz

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// LowerInvalidToDynamicSchema lowers c.Schema to a DynamicOutputSchema
// and, for InvalidDynamicSchema (Axis B), applies c.Mutation to the
// lowered shape's first root property. For InvalidJSONCoercion (Axis C)
// the schema lowers cleanly — the perturbation lives in c.MockLLMContent
// and reaches the surfaces through the mockllm scenario; the schema
// itself stays untouched.
//
// Errors from the base lowering (ErrDynamicSchemaUnsupported and
// friends) bubble up unchanged so the caller can apply the same
// skip-vs-fail dispatch the dynamic oracle uses (corpus/fuzz cases
// skip, rapid cases fail — rapid draws from DynamicSafeSchemaGen which
// excludes unsupported shapes by construction).
func LowerInvalidToDynamicSchema(c InvalidOracleCase) (bamlutils.DynamicOutputSchema, error) {
	lowered, err := LowerToDynamicSchema(c.Schema)
	if err != nil {
		return bamlutils.DynamicOutputSchema{}, err
	}
	switch c.Mode {
	case InvalidJSONCoercion:
		return lowered, nil
	case InvalidDynamicSchema:
		if err := applyInvalidSchemaMutation(&lowered, c.Mutation); err != nil {
			return bamlutils.DynamicOutputSchema{}, fmt.Errorf("apply mutation %q: %w", c.Mutation, err)
		}
		return lowered, nil
	}
	return bamlutils.DynamicOutputSchema{}, fmt.Errorf("unknown invalid oracle mode %q", c.Mode)
}
