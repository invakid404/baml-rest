package nativeprompt

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	mj "github.com/mitsuhiko/minijinja/minijinja-go/v2"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/value"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// RenderStatic renders a function's static prompt from its retained
// [promptdescriptor.Function] and a map of primitive argument values, producing
// the native [RenderedPrompt].
//
// It is the test-only narrow static candidate of the de-BAML front-end arc: no
// serving path calls it. It claims only the deliberately small surface proven
// (in slice 2) byte-exact against BAML v0.223 — literal text with direct
// primitive interpolation, fixed text-only role blocks, and bare
// ctx.output_format — and fails closed on everything else.
//
// RenderStatic and [SupportsStatic] share the exact same internal preparer
// ([prepareStatic]), so RenderStatic never interprets a shape SupportsStatic
// accepted. On any decline it returns a wrapped [ErrUnsupported] and a nil
// prompt. After the preparer returns nil, a compile/render/lower failure is an
// invariant/parity failure (surfaced as a plain error), not a new decline
// category.
func RenderStatic(fn promptdescriptor.Function, args map[string]any) (*RenderedPrompt, error) {
	plan, err := prepareStatic(fn, args)
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
func SupportsStatic(fn promptdescriptor.Function, args map[string]any) error {
	_, err := prepareStatic(fn, args)
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
// RenderStatic. It runs the load-bearing preparation order from the Phase 3
// scope and returns either a ready plan or the first decline encountered:
//
//  1. descriptor envelope (version/method/return + structurally valid bundle);
//  2. macro gate (any project macro declines FeatureTemplateString);
//  3. argument-declaration gate (attribute-free primitive args only);
//  4. argument-value gate + explicit primitive binding (exact Go scalar types);
//  5. template feature analysis (the closed-allowlist segment scanner);
//  6. dedent+trim of the raw prompt;
//  7. output-format render for bare ctx.output_format only;
//  8. value-aware chat-layout validation;
//  9. RENDER the exact bytes (dedentTrim -> buildEnv -> renderToString) that
//     lower will consume, then validate their reserved-marker structure.
//
// Rendering + reserved-marker validation happen HERE, in the shared preparer, so
// SupportsStatic really does compile/render (then discards the bytes) and
// RenderStatic re-runs the same preparer and only lowers the validated plan.
// This is load-bearing: the reserved-marker fence is byte-faithful because it
// inspects the real rendered output, and a nil SupportsStatic therefore
// guarantees RenderStatic lowers cleanly.
func prepareStatic(fn promptdescriptor.Function, args map[string]any) (*staticPlan, error) {
	// (1) Descriptor envelope. A mismatch is never normalized; it declines.
	bundle, err := checkStaticEnvelope(fn)
	if err != nil {
		return nil, err
	}

	// (2) Macro gate. BAML injects every project template string into every
	// function, so a non-empty macro set makes every function in that project a
	// Phase 3 decline. Do not concatenate/dedent bodies or inspect call sites.
	if len(fn.Macros) != 0 {
		return nil, decline(FeatureTemplateString,
			fmt.Sprintf("function carries %d project template_string macro(s); BAML injects them into every function", len(fn.Macros)))
	}

	// (3) Argument-declaration gate.
	decls, err := checkArgDeclarations(fn.Args)
	if err != nil {
		return nil, err
	}

	// (4) Argument-value gate + explicit primitive binding. argNonWS records,
	// per argument, whether its bound value renders a non-whitespace string —
	// used by the value-aware chat-layout check below.
	ctx, argNonWS, err := bindArgs(decls, args)
	if err != nil {
		return nil, err
	}

	// (5) Template feature analysis (closed allowlist). It defines support; a
	// successful MiniJinja compile does not. Chat layout is validated after
	// output-format rendering (step 8) because emptiness is value-dependent.
	plan, err := analyzeTemplate(fn.Prompt, decls)
	if err != nil {
		return nil, err
	}

	// (6) Preprocess: BAML's dedent-by-minimum-leading-whitespace + trim.
	template := dedentTrim(fn.Prompt)

	// (7) Output format for bare ctx.output_format only. The bundle is always
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

	// (8) Value-aware chat-layout validation. A content event contributes only
	// when it renders a non-whitespace string, so an interpolated "" / "  \n\t"
	// argument or an empty output-format block cannot masquerade as message
	// content (which the lowerer would then drop, producing an empty message).
	contentful := func(ev event) bool {
		switch ev.kind {
		case evText:
			return true
		case evInterp:
			return argNonWS[ev.arg]
		case evOutputFormat:
			return outputFormatNonWS
		default:
			return false
		}
	}
	if err := validateChatLayout(plan.events, contentful); err != nil {
		return nil, err
	}

	// (9) Render the exact bytes lower will consume, then (10) fence reserved
	// markers on THAT output. Rendering in the shared preparer (not just
	// RenderStatic) is what makes the reserved-marker check byte-faithful: it sees
	// the real post-dedent/post-render/post-whitespace-control text, so it cannot
	// miss a delimiter synthesized by BAML dedent (form-feed/NBSP/other Unicode
	// indentation) or a {{- -}} join, and cannot invent one from ordinary literal
	// whitespace that MiniJinja preserves. A render error for an analyzed-allowed
	// shape is an invariant failure surfaced loudly (not a decline category).
	env := buildEnv(outputFormat)
	rendered, err := renderToString(env, template, "static", ctx)
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

// argDecl is one validated primitive argument declaration: its name and the
// exact BAML primitive ("string"/"int"/"float"/"bool") it must bind.
type argDecl struct {
	name string
	prim string
}

// reservedGlobalNames are the MiniJinja globals the environment installs. An
// argument may not shadow them; such a declaration declines rather than risk
// silently masking ctx.output_format or the role helper at render time.
var reservedGlobalNames = map[string]bool{"ctx": true, "_": true}

// checkArgDeclarations runs the argument-declaration gate (step 3): each
// argument must have a unique, non-empty, non-reserved name and an
// attribute-free primitive TypeExpr in {string,int,float,bool}. Every other
// shape (bare/untyped, null, optional, literal, container, named type, media)
// declines with the matching feature key.
func checkArgDeclarations(args []promptdescriptor.Argument) ([]argDecl, error) {
	decls := make([]argDecl, 0, len(args))
	seen := make(map[string]bool, len(args))
	for _, a := range args {
		if a.Name == "" {
			return nil, decline(FeatureStaticArgType, "argument has an empty name")
		}
		if reservedGlobalNames[a.Name] {
			return nil, decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q shadows a reserved global", a.Name))
		}
		if seen[a.Name] {
			return nil, decline(FeatureStaticArgValue,
				fmt.Sprintf("duplicate argument declaration %q", a.Name))
		}
		seen[a.Name] = true

		prim, err := primitiveOfDecl(a)
		if err != nil {
			return nil, err
		}
		decls = append(decls, argDecl{name: a.Name, prim: prim})
	}
	return decls, nil
}

// primitiveOfDecl classifies one argument's retained type, returning its
// primitive name on success or the matching decline.
func primitiveOfDecl(a promptdescriptor.Argument) (string, error) {
	t := a.Type
	if t == nil {
		return "", decline(FeatureStaticArgType,
			fmt.Sprintf("argument %q is bare/untyped", a.Name))
	}
	if len(t.Attributes) != 0 {
		return "", decline(FeatureStaticArgType,
			fmt.Sprintf("argument %q carries attributes; only attribute-free primitives are supported", a.Name))
	}
	switch t.Kind {
	case bamlparser.KindPrimitive:
		switch t.Primitive {
		case "string", "int", "float", "bool":
			return t.Primitive, nil
		case "null":
			return "", decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q is null; only non-null string/int/float/bool are supported", a.Name))
		default:
			return "", decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q has unsupported primitive %q", a.Name, t.Primitive))
		}
	case bamlparser.KindMedia:
		return "", decline(FeatureUnsupportedMediaKind,
			fmt.Sprintf("argument %q is media (%q); static media is not supported, including image URL", a.Name, t.Media))
	case bamlparser.KindNameRef:
		return "", decline(FeatureEnumClassValue,
			fmt.Sprintf("argument %q references a named class/enum/alias %q", a.Name, t.Name))
	default:
		return "", decline(FeatureStaticArgType,
			fmt.Sprintf("argument %q has an unsupported type (optional/literal/list/map/tuple/union/group)", a.Name))
	}
}

// bindArgs runs the argument-value gate (step 4) and the explicit primitive
// binder (step 8's value construction). It requires an EXACT key set — every
// declared argument present and no extra input key — and binds each value to
// its declared primitive with the exact Go scalar type (string/int64/float64/
// bool). No coercion: int, json.Number, integral-float-for-int, pointers, and
// aliases all decline. Invalid UTF-8 strings, non-finite floats, and reserved
// marker strings decline too.
//
// It returns two maps keyed by argument name: ctx holds the explicitly
// constructed value.Values, and nonWS records whether each argument renders to a
// non-whitespace string (numbers and bools always do; a string does iff it is
// not whitespace-only), which the value-aware chat-layout check consumes.
func bindArgs(decls []argDecl, args map[string]any) (ctx map[string]any, nonWS map[string]bool, err error) {
	declared := make(map[string]bool, len(decls))
	for _, d := range decls {
		declared[d.name] = true
	}
	for k := range args {
		if !declared[k] {
			return nil, nil, decline(FeatureStaticArgValue,
				fmt.Sprintf("unexpected argument %q not in the function signature", k))
		}
	}

	ctx = make(map[string]any, len(decls))
	nonWS = make(map[string]bool, len(decls))
	for _, d := range decls {
		raw, ok := args[d.name]
		if !ok {
			return nil, nil, decline(FeatureStaticArgValue,
				fmt.Sprintf("missing value for declared argument %q", d.name))
		}
		v, err := bindPrimitive(d.name, d.prim, raw)
		if err != nil {
			return nil, nil, err
		}
		ctx[d.name] = v
		nonWS[d.name] = renderedNonWhitespace(d.prim, raw)
	}
	return ctx, nonWS, nil
}

// renderedNonWhitespace reports whether the bound primitive renders to a string
// with at least one non-whitespace rune. int/float/bool always do (digits or
// letters); a string does iff it is not whitespace-only. The type assertions
// mirror bindPrimitive, which has already validated them.
func renderedNonWhitespace(prim string, raw any) bool {
	if prim == "string" {
		s, _ := raw.(string)
		return strings.TrimSpace(s) != ""
	}
	return true
}

// bindPrimitive constructs a MiniJinja value for one primitive argument using
// the explicit value.From* constructors only — never value.FromAny, reflection,
// JSON, or fmt coercion. The Go type must match the declared primitive exactly.
func bindPrimitive(name, prim string, raw any) (value.Value, error) {
	switch prim {
	case "string":
		s, ok := raw.(string)
		if !ok {
			return value.Value{}, argTypeMismatch(name, "string", raw)
		}
		if !utf8.ValidString(s) {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("argument %q string is not valid UTF-8", name))
		}
		// A reserved marker inside a value (whole or synthesized with neighbours)
		// is fenced byte-faithfully by validateRenderedMarkers on the rendered
		// output, not per-piece here — so a value with a marker-shaped substring
		// that renders into a harmless completion is not falsely declined.
		return value.FromString(s), nil
	case "int":
		i, ok := raw.(int64)
		if !ok {
			return value.Value{}, argTypeMismatch(name, "int64", raw)
		}
		return value.FromInt(i), nil
	case "float":
		f, ok := raw.(float64)
		if !ok {
			return value.Value{}, argTypeMismatch(name, "float64", raw)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("argument %q float is non-finite (NaN/Inf)", name))
		}
		return value.FromFloat(f), nil
	case "bool":
		b, ok := raw.(bool)
		if !ok {
			return value.Value{}, argTypeMismatch(name, "bool", raw)
		}
		return value.FromBool(b), nil
	default:
		// Unreachable: checkArgDeclarations restricts prim to the four above.
		return value.Value{}, decline(FeatureStaticArgType,
			fmt.Sprintf("argument %q has unsupported primitive %q", name, prim))
	}
}

// argTypeMismatch builds the value-gate decline for a wrong Go scalar type,
// naming both the expected Go type and what was actually supplied.
func argTypeMismatch(name, wantGo string, raw any) error {
	return decline(FeatureStaticArgValue,
		fmt.Sprintf("argument %q requires Go %s, got %T (no coercion)", name, wantGo, raw))
}

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

// renderToString compiles src in env and renders it with ctx, returning the raw
// rendered string (the exact bytes lower will consume). what names the template
// in error messages ("dynamic"/"static").
func renderToString(env *mj.Environment, src, what string, ctx map[string]any) (string, error) {
	tmpl, err := env.TemplateFromString(src)
	if err != nil {
		return "", fmt.Errorf("nativeprompt: compile %s template: %w", what, err)
	}
	rendered, err := tmpl.Render(ctx)
	if err != nil {
		return "", fmt.Errorf("nativeprompt: render %s template: %w", what, err)
	}
	return rendered, nil
}

// renderTemplate compiles src in env, renders it with ctx, and lowers the
// result. It is the single MiniJinja boundary the dynamic Render path uses;
// behavior and error strings are unchanged.
func renderTemplate(env *mj.Environment, src, what string, ctx map[string]any) (*RenderedPrompt, error) {
	rendered, err := renderToString(env, src, what, ctx)
	if err != nil {
		return nil, err
	}
	return lower(rendered)
}
