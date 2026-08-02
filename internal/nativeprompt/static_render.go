package nativeprompt

import (
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// RenderStatic renders a function's static prompt from its retained
// [promptdescriptor.Function] and the ORDERED, already-typed argument vector a
// generated projector produced, producing the native [RenderedPrompt].
//
// It claims only the deliberately small surface proven byte-exact against BAML
// v0.223 — literal text with direct scalar/enum/list interpolation, the
// exact canonical enum equality and one-element membership forms, fixed
// text-only role blocks, and bare ctx.output_format — and fails closed on
// everything else.
//
// values is the neutral [promptdescriptor.ArgumentValue] vector, in declared
// argument order. There is deliberately NO map-based overload (de-BAML Slice
// 7.1b): a raw `map[string]any` cannot state nested order, canonical field
// names, or enum identity, so a call site holding only that is ineligible by
// construction rather than by discipline.
//
// RenderStatic and [SupportsStatic] share the exact same internal preparer
// ([prepareStatic]), so RenderStatic never interprets a shape SupportsStatic
// accepted. On any decline it returns a wrapped [ErrUnsupported] and a nil
// prompt. After the preparer returns nil, a compile/render/lower failure is an
// invariant/parity failure (surfaced as a plain error), not a new decline
// category.
func RenderStatic(fn promptdescriptor.Function, values []promptdescriptor.ArgumentValue) (*RenderedPrompt, error) {
	plan, err := prepareStatic(fn, values)
	if err != nil {
		return nil, err
	}
	// prepareStatic has already produced (and reserved-marker-validated) the exact
	// rendered bytes; RenderStatic only lowers them. Lowering cannot introduce a
	// new decline here — the shared preparer guarantees a clean structure.
	return lower(plan.rendered)
}

// SupportsStatic is the single fail-closed static claim predicate. It returns
// nil when [RenderStatic] is proven to reproduce this function+args and a
// *Decline (unwrapping to [ErrUnsupported]) otherwise. It shares the whole
// preparer with RenderStatic, so a nil result guarantees RenderStatic would not
// decline.
func SupportsStatic(fn promptdescriptor.Function, values []promptdescriptor.ArgumentValue) error {
	_, err := prepareStatic(fn, values)
	return err
}

// staticPlan is the fully-prepared output of [prepareStatic]: the exact rendered
// template bytes (post dedent/trim, MiniJinja render, and whitespace-control),
// already validated to carry only the intentional reserved markers. RenderStatic
// lowers these bytes; SupportsStatic discards them.
type staticPlan struct {
	rendered string
}

// prepareStatic is the shared analyzer/preparer behind both SupportsStatic and
// RenderStatic. It runs the load-bearing preparation order and returns either a
// ready plan or the first decline encountered:
//
//  1. descriptor envelope (version/method/return + structurally valid bundle);
//  2. macro gate (any project macro declines FeatureTemplateString);
//  3. V3 universe validation (unique enum/class names, unique canonical members
//     and fields, every named edge resolvable, no malformed list/scalar edge);
//  4. argument-declaration gate (a unique, non-shadowing name and a well-formed,
//     acyclic V3 value type per argument);
//  5. V3 value binding: the ORDERED projected vector is matched against the
//     declaration by count/order/name and bound into bamlprofile host values;
//  6. template feature analysis (the closed-allowlist segment scanner, now with
//     the V3 type gate for enum expressions);
//  7. dedent+trim of the raw prompt;
//  8. output-format render for bare ctx.output_format only;
//  9. value-aware chat-layout validation;
//  10. RENDER the exact bytes (dedentTrim -> profile renderContext -> render)
//     that lower will consume, then validate their reserved-marker structure.
//
// Order is load-bearing in two places. The universe and the values are validated
// BEFORE any template is considered, so a malformed descriptor can never become
// a support claim; and binding happens BEFORE analysis, so "a directly declared
// V3 class with an acyclic, fully bindable closure" is a fact the analyzer reads
// rather than a property it re-derives.
//
// Rendering + reserved-marker validation happen HERE, in the shared preparer, so
// SupportsStatic really does compile/render (then discards the bytes) and
// RenderStatic re-runs the same preparer and only lowers the validated plan.
// This is load-bearing: the reserved-marker fence is byte-faithful because it
// inspects the real rendered output, and a nil SupportsStatic therefore
// guarantees RenderStatic lowers cleanly.
func prepareStatic(fn promptdescriptor.Function, values []promptdescriptor.ArgumentValue) (*staticPlan, error) {
	// (1) Descriptor envelope. A mismatch is never normalized; it declines.
	bundle, err := checkStaticEnvelope(fn)
	if err != nil {
		return nil, err
	}

	// (2) Macro gate. BAML injects every project template string into every
	// function, so a non-empty macro set makes every function in that project a
	// decline. Do not concatenate/dedent bodies or inspect call sites.
	if len(fn.Macros) != 0 {
		return nil, decline(FeatureTemplateString,
			fmt.Sprintf("function carries %d project template_string macro(s); BAML injects them into every function", len(fn.Macros)))
	}

	// (3) V3 universe validation, before a template or a value is considered.
	universe, err := validateUniverse(fn.InputValues)
	if err != nil {
		return nil, err
	}

	// (4) Argument-declaration gate.
	decls, err := checkV3ArgDeclarations(fn.Args, universe)
	if err != nil {
		return nil, err
	}

	// (5) V3 value binding. Each binding records whether its value renders a
	// non-whitespace string, which the value-aware chat-layout check consumes.
	bindings, err := bindV3Args(decls, values, universe)
	if err != nil {
		return nil, err
	}
	gate := newTypeGate(bindings, universe)

	// (6) Template feature analysis (closed allowlist). It defines support; a
	// successful MiniJinja compile does not. Chat layout is validated after
	// output-format rendering (step 9) because emptiness is value-dependent.
	plan, err := analyzeTemplate(fn.Prompt, gate)
	if err != nil {
		return nil, err
	}

	// (7) Preprocess: BAML's dedent-by-minimum-leading-whitespace + trim.
	template := dedentTrim(fn.Prompt)

	// (8) Output format for bare ctx.output_format only. The bundle is always
	// lowered (step 1); it is rendered only when the prompt reaches the global.
	outputFormat := ""
	outputFormatNonWS := false
	if plan.usesOutputFormat {
		block, rerr := outputformat.Render(bundle, outputformat.Options{})
		if rerr != nil {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("return bundle does not render a valid ctx.output_format: %v", rerr))
		}
		outputFormat = block
		outputFormatNonWS = strings.TrimSpace(block) != ""
	}

	// (9) Value-aware chat-layout validation. A content event contributes only
	// when it renders a non-whitespace string, so an interpolated "" / "  \n\t"
	// argument, an enum whose display alias is whitespace, or an empty
	// output-format block cannot masquerade as message content (which the lowerer
	// would then drop, producing an empty message).
	argNonWS := make(map[string]bool, len(bindings))
	for i := range bindings {
		argNonWS[bindings[i].name] = bindings[i].nonWS
	}
	contentful := func(ev event) bool {
		switch ev.kind {
		case evText:
			return true
		case evInterp:
			return argNonWS[ev.arg]
		case evOutputFormat:
			return outputFormatNonWS
		case evEnumPredicate:
			// A predicate renders MiniJinja's "true"/"false" — always content.
			return true
		default:
			return false
		}
	}
	if err := validateChatLayout(plan.events, contentful); err != nil {
		return nil, err
	}

	// (10) Render the exact bytes lower will consume, then (11) fence reserved
	// markers on THAT output. Rendering in the shared preparer (not just
	// RenderStatic) is what makes the reserved-marker check byte-faithful: it sees
	// the real post-dedent/post-render/post-whitespace-control text, so it cannot
	// miss a delimiter synthesized by BAML dedent (form-feed/NBSP/other Unicode
	// indentation) or a {{- -}} join, and cannot invent one from ordinary literal
	// whitespace that MiniJinja preserves. A render error for an analyzed-allowed
	// shape is an invariant failure surfaced loudly (not a decline category).
	//
	// The render context installs the descriptor's WHOLE project enum set as
	// namespace globals, reproducing stock v0.223's render_prompt: `Color.RED`
	// resolves even in a function that takes no Color argument.
	rc, err := newRenderContext(outputFormat, universe.defs)
	if err != nil {
		return nil, err
	}
	for i := range bindings {
		rc.bind(bindings[i].name, bindings[i].value)
	}
	rendered, err := rc.renderToString(template, "static")
	if err != nil {
		return nil, err
	}
	if err := validateRenderedMarkers(rendered, roleSequence(plan.events)); err != nil {
		return nil, err
	}
	return &staticPlan{rendered: rendered}, nil
}

// checkStaticEnvelope validates the descriptor envelope (step 1) and returns
// the lowered return bundle. The bundle is lowered even when the prompt never
// reaches ctx.output_format, so a hand-constructed malformed descriptor can
// never become a support claim.
func checkStaticEnvelope(fn promptdescriptor.Function) (*schema.Bundle, error) {
	if fn.Version != promptdescriptor.Version {
		return nil, decline(FeatureStaticDescriptor,
			fmt.Sprintf("function descriptor version %d != %d", fn.Version, promptdescriptor.Version))
	}
	if fn.Method == "" {
		return nil, decline(FeatureStaticDescriptor, "function descriptor has empty method")
	}
	if fn.Client == "" || fn.Provider == "" {
		return nil, decline(FeatureStaticDescriptor, "function descriptor is missing a resolved client/provider")
	}
	if fn.Return.Version != schemadescriptor.Version {
		return nil, decline(FeatureStaticDescriptor,
			fmt.Sprintf("return descriptor version %d != %d", fn.Return.Version, schemadescriptor.Version))
	}
	if fn.Return.Method != fn.Method {
		return nil, decline(FeatureStaticDescriptor,
			fmt.Sprintf("return descriptor method %q != function method %q", fn.Return.Method, fn.Method))
	}
	bundle, err := schema.FromStaticDescriptor(fn.Return)
	if err != nil {
		return nil, decline(FeatureStaticDescriptor,
			fmt.Sprintf("return bundle is not structurally valid: %v", err))
	}
	return bundle, nil
}

// reservedGlobalNames are the MiniJinja globals the profile environment
// installs unconditionally. An argument may not shadow them; such a declaration
// declines rather than risk silently masking ctx.output_format or the role
// helper at render time. (A project ENUM namespace is the other shadowing
// hazard; checkV3ArgDeclarations fences that against the descriptor's universe.)
var reservedGlobalNames = map[string]bool{"ctx": true, "_": true}

// roleSequence returns the ordered roles of the intentional _.role/_.chat
// markers the scanner accepted — the exact role markers the rendered output
// must contain, used by validateRenderedMarkers.
func roleSequence(events []event) []string {
	var roles []string
	for _, ev := range events {
		if ev.kind == evRole {
			roles = append(roles, ev.role)
		}
	}
	return roles
}

// validateRenderedMarkers is the byte-faithful reserved-marker fence. It runs in
// the shared preparer on the ACTUAL rendered bytes (post dedent/trim, MiniJinja
// render, and whitespace-control) — the same bytes lower consumes — and requires
// they carry EXACTLY the intentional markers:
//
//   - NO media marker of any form. This slice binds no media, so there are no
//     intentional media markers; lower.parseBody recognizes a
//     mediaMarkerPrefix..mediaMarkerSuffix body segment as a MediaPart even
//     WITHOUT the media delimiter, so the media delimiter AND both media marker
//     affixes are fenced on any occurrence.
//   - a completion (no intentional roles) must contain no role delimiter and no
//     role marker affix;
//   - a chat must split on the role delimiter into exactly preamble + (marker,
//     body)×N, with the role markers at the intentional (odd) positions parsing
//     back to expectedRoles, and NO role marker affix in any other chunk.
//
// Any deviation is a delimiter/marker synthesized by untrusted text across a
// boundary (dedent of Unicode indentation, a {{- -}} join, interpolation, a
// comment, or multiple arguments) and declines FeatureReservedDelimiter. Because
// it inspects the real output, it cannot miss such a synthesis nor invent a
// marker from ordinary literal whitespace MiniJinja preserves.
func validateRenderedMarkers(rendered string, expectedRoles []string) error {
	// No intentional static media markers exist, so any media magic string
	// (delimiter or affix) in the rendered output is synthesized by untrusted
	// content and would drive lower.parseBody -> isMediaMarker.
	for _, m := range []string{mediaDelim, mediaMarkerPrefix, mediaMarkerSuffix} {
		if strings.Contains(rendered, m) {
			return decline(FeatureReservedDelimiter,
				"a media marker appears in the rendered output")
		}
	}

	n := len(expectedRoles)
	if n == 0 {
		// Completion: lower returns the whole string. A role delimiter or role
		// marker affix would make lower interpret chat structure instead.
		if strings.Contains(rendered, roleDelim) ||
			strings.Contains(rendered, roleMarkerPrefix) ||
			strings.Contains(rendered, roleMarkerSuffix) {
			return decline(FeatureReservedDelimiter,
				"a role marker is synthesized in a completion prompt")
		}
		return nil
	}

	// Chat: each intentional _.role/_.chat marker wraps the role delimiter twice,
	// so the split is exactly preamble + N*(marker, body) = 2N+1 chunks, with the
	// intentional role markers at the odd positions.
	chunks := strings.Split(rendered, roleDelim)
	if len(chunks) != 2*n+1 {
		return decline(FeatureReservedDelimiter,
			"an unexpected number of role delimiters appears in the rendered output")
	}
	for i, chunk := range chunks {
		if i%2 == 1 {
			// Intentional role marker: must be well-formed for the expected role.
			if !isRoleMarker(chunk) {
				return decline(FeatureReservedDelimiter,
					"a role marker is missing or misaligned in the rendered output")
			}
			role, allowDupe, meta, err := parseRoleMarker(chunk)
			if err != nil || allowDupe || meta != nil || role != expectedRoles[(i-1)/2] {
				return decline(FeatureReservedDelimiter,
					"a role marker diverges from the intentional emission")
			}
			continue
		}
		// Preamble/body chunk: only the intentional odd chunks may carry a role
		// marker affix. A whole-chunk affix wrap would be a fake role marker; any
		// affix here is synthesized, so decline it outright.
		if strings.Contains(chunk, roleMarkerPrefix) || strings.Contains(chunk, roleMarkerSuffix) {
			return decline(FeatureReservedDelimiter,
				"a role marker affix is synthesized in a non-role chunk")
		}
	}
	return nil
}
