// This file is hand-written and is NOT emitted by
// cmd/regenerate-dynclient. It implements the native "de-BAML"
// ctx.output_format pre-substitution wired into the generated dynamic
// BuildRequest closures (adapter.go) via the codegen-emitted call to
// maybeApplyDeBAMLOutputFormat. See GitHub #536.
//
// Mechanism (typed pre-substitution before BAML BuildRequest): when the
// BAML_REST_USE_DEBAML umbrella flag is on, render the native
// output-format block via the injected render callback
// (bamlutils.DeBAMLRenderFunc, wired by the root module — this separate
// dynclient module cannot import baml-rest's internal/schema +
// outputformat), then replace every structured output_format part and
// every literal "{output_format}" string placeholder in a COPY of the
// converted message slice with that block — before BAML renders the
// provider request. BAML then renders the block as ordinary text and
// never invokes ctx.output_format, because no marker remains.
//
// The rewrite is returned as a separate copy used ONLY by the BuildRequest
// Request/StreamRequest/Bedrock closures; the legacy fallback children
// keep the original converted slice so a mixed-mode fallback to a legacy
// child stays BAML-as-today (route coupling tracked in #537).
//
// Fallback is render-first / rewrite-after: a nil callback or a genuine
// render error returns the original slice so BAML renders
// ctx.output_format exactly as today. A schema carrying metadata BAML's
// dynamic TypeBuilder drops (class/field descriptions, field aliases, enum
// aliases) is NOT a fallback case — native ON intentionally includes that
// metadata (#536 "option C"). Class-level aliases are intentionally NOT
// included: BAML's static ctx.output_format does not render them, so the
// native renderer's canonical class names are correct.
package generated

import (
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
	types "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client/types"
)

// deBAMLRendererGetter is the narrow optional interface the adapter
// implements to expose the native ctx.output_format render callback. The
// root module wires the callback (it owns internal/schema + outputformat);
// this separate dynclient module only ever calls it through this public-
// typed seam, so it never imports those root internal packages.
type deBAMLRendererGetter interface {
	DeBAMLRenderer() bamlutils.DeBAMLRenderFunc
}

// outputFormatPlaceholder is the literal string placeholder the dynamic
// BAML template replaces with ctx.output_format inside plain-string
// message content (cmd/build/dynamic.baml). The native path replaces the
// same token so string-content messages match BAML's behaviour.
const outputFormatPlaceholder = "{output_format}"

// maybeApplyDeBAMLOutputFormat returns the message slice the dynamic
// BuildRequest closures should hand to BAML's Request/StreamRequest. When
// the de-BAML flag is on and the carried schema renders, it returns a
// COPY of msgs with the output-format markers replaced by the natively-
// rendered block; otherwise it returns msgs unchanged.
//
// It NEVER mutates the input slice or its backing parts/content. The
// caller keeps the original slice for the legacy fallback children
// (legacyStreamChildFn / legacyCallChildFn), which must stay BAML-as-
// today — the native seam lives on the BuildRequest route only (route
// coupling tracked in #537). Returning the input unchanged when nothing
// is rewritten avoids any allocation on the flag-off / no-schema / render-
// error paths.
//
// Fallback is render-first / return-after: a genuine native lowering or
// rendering error returns the original msgs so BAML renders
// ctx.output_format as today.
func maybeApplyDeBAMLOutputFormat(adapter bamlutils.Adapter, msgs []types.Baml_Rest_Message) []types.Baml_Rest_Message {
	if !adapter.DeBAMLConfig().Enabled {
		return msgs
	}
	outputSchema := adapter.DeBAMLOutputSchema()
	if outputSchema == nil {
		// No schema carried (e.g. a caller that bypassed
		// DynamicInput.ToWorkerInput). Leave BAML to render.
		return msgs
	}

	// The render callback is injected by the root module (cmd/serve,
	// cmd/worker, or a dynclient caller via WithDeBAMLRenderer). A nil
	// callback means this build has no native renderer wired, so fall
	// back to BAML-as-today.
	getter, ok := adapter.(deBAMLRendererGetter)
	if !ok {
		return msgs
	}
	render := getter.DeBAMLRenderer()
	if render == nil {
		return msgs
	}
	block, err := render(outputSchema)
	if err != nil {
		logDeBAMLFallback(adapter, "render native output_format", err)
		return msgs
	}

	// Rewrite into a copy only after a successful render so a fallback
	// never leaves half-substituted messages and the original slice the
	// legacy children share is never touched.
	return rewriteOutputFormat(msgs, block)
}

// rewriteOutputFormat returns a copy of msgs with every output-format
// marker replaced by block, mirroring the two insertion forms the dynamic
// BAML template supports:
//
//   - a structured output_format content part becomes a plain text part
//     carrying the block (BAML then renders {{ p.text }} instead of
//     {{ ctx.output_format }});
//   - a literal "{output_format}" token inside string content is replaced
//     by the block (BAML's own replace() over the rewritten content is
//     then a no-op).
//
// Every occurrence is replaced — multiple parts, multiple messages, and
// multiple placeholders within one string all match — because BAML's
// template renders ctx.output_format at each occurrence. The input slice
// and its backing parts/content are never mutated: only-touched messages
// get a fresh parts slice / content pointer in the returned copy, while
// untouched messages share the original (read-only) pointers.
func rewriteOutputFormat(msgs []types.Baml_Rest_Message, block string) []types.Baml_Rest_Message {
	out := make([]types.Baml_Rest_Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		m := &out[i]
		switch {
		case m.Parts != nil:
			if !hasOutputFormatPart(*m.Parts) {
				continue
			}
			newParts := make([]types.Baml_Rest_ContentPart, len(*m.Parts))
			copy(newParts, *m.Parts)
			for j := range newParts {
				p := &newParts[j]
				if p.Output_format != nil && *p.Output_format {
					// Distinct backing string per part so the *string
					// pointers never alias.
					text := block
					p.Text = &text
					p.Output_format = nil
				}
			}
			m.Parts = &newParts
		case m.Content != nil:
			if strings.Contains(*m.Content, outputFormatPlaceholder) {
				replaced := strings.ReplaceAll(*m.Content, outputFormatPlaceholder, block)
				m.Content = &replaced
			}
		}
	}
	return out
}

// hasOutputFormatPart reports whether any part is an output_format marker.
func hasOutputFormatPart(parts []types.Baml_Rest_ContentPart) bool {
	for i := range parts {
		if parts[i].Output_format != nil && *parts[i].Output_format {
			return true
		}
	}
	return false
}

// logDeBAMLFallback records a native-render fallback so the BAML-as-today
// path is observable without introducing a user-facing flag. Best-effort:
// no logger installed means no line, never an error to the caller.
func logDeBAMLFallback(adapter bamlutils.Adapter, stage string, err error) {
	if logger := adapter.Logger(); logger != nil {
		logger.Warn("de-BAML output_format fallback to BAML render", "stage", stage, "err", err.Error())
	}
}
