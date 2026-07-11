package nativeprompt

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// Render renders the dynamic Baml_Rest_Dynamic prompt for the given messages
// and dynamic output schema, producing the native RenderedPrompt equivalent of
// BAML's runtime output.
//
// It fails closed: if [Supports] declines the input (an unproven media kind,
// say), Render returns a wrapped [ErrUnsupported] and renders nothing, so a
// caller can route the request to the authoritative BAML path. Any genuine
// output_format or template error is returned as-is.
//
// outputSchema may be nil ONLY when the input cannot reach ctx.output_format
// (no output_format part and no {output_format} placeholder). If the input can
// reach ctx.output_format but no schema is supplied, Render fails closed with
// [ErrUnsupported] rather than render a weakened (empty) output_format block, so
// the caller falls back to BAML. When present, ctx.output_format is rendered via
// the native internal/schema/outputformat renderer with default options — BAML
// RenderOptions::default, matching the template's bare {{ ctx.output_format }}.
func Render(msgs []Message, outputSchema *bamlutils.DynamicOutputSchema) (*RenderedPrompt, error) {
	if err := Supports(rawDynamicPrompt, msgs); err != nil {
		return nil, err
	}

	// Fail closed: a nil/absent output schema that could reach ctx.output_format
	// would otherwise render an empty block and silently weaken the prompt.
	if outputSchema == nil && inputReachesOutputFormat(msgs) {
		return nil, decline(FeatureNilOutputSchema,
			"input can reach ctx.output_format but no output schema was supplied; declining rather than render a weakened (empty) output_format")
	}

	outputFormat := ""
	if outputSchema != nil {
		block, err := renderOutputFormat(outputSchema)
		if err != nil {
			return nil, fmt.Errorf("nativeprompt: render ctx.output_format: %w", err)
		}
		outputFormat = block
	}

	env := buildEnv(outputFormat)
	return renderTemplate(env, dynamicTemplate, "dynamic", map[string]any{"messages": messagesToValue(msgs)})
}

// renderOutputFormat lowers the dynamic output schema and renders the native
// ctx.output_format block with zero-value options, mirroring
// internal/debaml.Render (the wiring the production seam already uses).
func renderOutputFormat(s *bamlutils.DynamicOutputSchema) (string, error) {
	bundle, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		return "", err
	}
	return outputformat.Render(bundle, outputformat.Options{})
}
