// Package nativespine builds the neutral projectdescriptor.Project from the
// facts cmd/introspect already computes for the current .baml project, and
// classifies each method into the one native class M1 admits (unary final-call +
// final-parse) or a stable decline. It is the "grow cmd/introspect's pass"
// half of M1 (decision D10): introspect owns the .baml walk and hands its
// already-ordered prompt descriptors here; this package neither re-parses nor
// re-derives them.
//
// The governing rule (codegen-spine M1 review): the classifier admits a method
// ONLY when the emitter can reproduce it faithfully. Anything the Project
// projection would drop, or the emitter could not render byte-for-byte, is
// DECLINED with a stable manifest code and left on generated BAML — never
// admitted-then-failed, never emitted with wrong carriers.
//
// The decline codes are opaque manifest codes (internal/codegenspine/
// manifest.json declines.codegen_admission_declines); DeclineCodes() enumerates
// exactly the set this build can emit, and a test validates every one is in the
// manifest catalogue. This keeps the D1 boundary intact — the descriptor package
// carries codes opaquely and the catalogue lives in the manifest.
//
// For a pre-decline (a function that never reached a promptdescriptor.Function),
// the sub-code comes from a STRUCTURAL verdict nativeschema stamps on the decline
// (PreDeclineFeature) — never from words in the upstream reason string. This
// package is a pure lookup on that verdict; it reads no reason text, so no help
// prose and no user-controlled declaration name can influence a code.
//
// M1 is a dark slice: nativeschema stamps a precise feature (schema_dynamic_class)
// only where the WINNING producer already knows it exactly (the eligibility scan /
// project enum / macro-argument verdict). Every other pre-decline is named by its
// reliable input-vs-return CONTEXT (unsupported_input_shape / unsupported_output_
// shape). The static-schema builder is not yet instrumented to carry its causative
// node, so media/checks/asserts on a schema-builder pre-decline resolve to the
// context code rather than an independent, non-equivalent re-walk that could pick
// an incidental earlier feature over the real later cause. Precise sub-codes for
// those are M2 (full producer instrumentation). checks, asserts, and the media
// codes stay emittable via the admitted classifier path (classifyOutputSchema), so
// DeclineCodes()/the manifest catalogue are unchanged. See docs/codegen-spine/04.
package nativespine

import (
	"errors"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

// M1 decline codes — every value is a code in internal/codegenspine/manifest.json
// (capabilities[].code, declines.representative_reasons[], or the codegen-native
// codes catalogued in declines.codegen_admission_declines). Validated by a test
// in internal/codegenspine.
const (
	DeclineMediaImage         projectdescriptor.CapabilityCode = "media_image"
	DeclineMediaAudio         projectdescriptor.CapabilityCode = "media_audio"
	DeclineMediaPDF           projectdescriptor.CapabilityCode = "media_pdf"
	DeclineMediaVideo         projectdescriptor.CapabilityCode = "media_video"
	DeclineMediaPart          projectdescriptor.CapabilityCode = "media_part"
	DeclineSchemaDynamicClass projectdescriptor.CapabilityCode = "schema_dynamic_class"
	DeclineChecks             projectdescriptor.CapabilityCode = "checks"
	DeclineAsserts            projectdescriptor.CapabilityCode = "asserts"
	DeclineStrategyFallback   projectdescriptor.CapabilityCode = "strategy_fallback"
	DeclineStrategyRoundRobin projectdescriptor.CapabilityCode = "strategy_round_robin"
	DeclineProviderNotOpenAI  projectdescriptor.CapabilityCode = "provider_not_openai"
	DeclineModelNotLiteral    projectdescriptor.CapabilityCode = "model_not_literal"
	DeclineModelEscape        projectdescriptor.CapabilityCode = "model_escape"
	DeclineClientBodyOption   projectdescriptor.CapabilityCode = "request_body_option"
	DeclinePromptDependency   projectdescriptor.CapabilityCode = "prompt_dependency"
	DeclineRoleUnsupported    projectdescriptor.CapabilityCode = "role_unsupported"
	DeclineNameCollision      projectdescriptor.CapabilityCode = "name_collision"

	DeclineUnsupportedOutputShape projectdescriptor.CapabilityCode = "unsupported_output_shape"
	DeclineUnsupportedInputShape  projectdescriptor.CapabilityCode = "unsupported_input_shape"
)

// DeclineCodes returns every decline code this build can emit, for catalogue
// validation. Order is stable.
func DeclineCodes() []projectdescriptor.CapabilityCode {
	return []projectdescriptor.CapabilityCode{
		DeclineMediaImage, DeclineMediaAudio, DeclineMediaPDF, DeclineMediaVideo, DeclineMediaPart,
		DeclineSchemaDynamicClass, DeclineChecks, DeclineAsserts,
		DeclineStrategyFallback, DeclineStrategyRoundRobin,
		DeclineProviderNotOpenAI, DeclineModelNotLiteral, DeclineModelEscape, DeclineClientBodyOption,
		DeclinePromptDependency, DeclineRoleUnsupported, DeclineNameCollision,
		DeclineUnsupportedOutputShape, DeclineUnsupportedInputShape,
	}
}

// admittedCapabilities are the manifest capability codes a ClassStaticUnary
// method satisfies.
var admittedCapabilities = []projectdescriptor.CapabilityCode{
	"static_method", "final_call", "provider_openai", "single_leaf_client",
}

// Provider strings. Strategy providers are matched in BOTH their canonical
// (cmd/introspect canonicaliseProvider, main.go) and raw source spellings,
// because the source-only test helper does not canonicalize.
const (
	providerOpenAI = "openai"
)

var (
	fallbackProviders   = map[string]bool{"baml-fallback": true, "fallback": true}
	roundRobinProviders = map[string]bool{"baml-roundrobin": true, "baml-round-robin": true, "round-robin": true}
)

// SourceFacts is the whole-project input to [BuildProjectDescriptor], assembled
// from the SAME parsed .baml walk by both the introspect pass and the fixture:
//
//   - Funcs / PreDeclines / PreDeclineFeatures are nativeschema's per-function
//     prompt-descriptor outputs (a V3 descriptor, a decline reason, and the
//     structural pre-decline feature) — the M1 inputs, unchanged.
//   - Clients / RetryPolicies / Strategies are nativeschema.BuildClientGraph's
//     ordered whole-project client graph.
//   - Templates is nativeschema.BuildProjectTemplates' ordered macro set.
//
// It carries no serving state and no Go-map order escapes the builder.
type SourceFacts struct {
	Funcs              map[string]promptdescriptor.Function
	PreDeclines        map[string]string
	PreDeclineFeatures map[string]nativeschema.PreDeclineFeature
	Clients            []projectdescriptor.Client
	RetryPolicies      []projectdescriptor.RetryPolicy
	Strategies         []projectdescriptor.Strategy
	Templates          []promptdescriptor.TemplateString
}

// BuildProjectDescriptor classifies the whole-project source facts into a Project
// (descriptor Version 2). Every retained method is admitted as one native class
// or recorded as a stable structural decline; the project's client graph,
// templates, and a per-method capability manifest are carried alongside. Method,
// diagnostic, and capability order is deterministic (sorted by name), independent
// of file walk order; the client-graph slices arrive already name-ordered.
func BuildProjectDescriptor(facts SourceFacts) projectdescriptor.Project {
	p := projectdescriptor.Project{
		Version:                 projectdescriptor.Version,
		PromptDescriptorVersion: promptdescriptor.Version,
		SchemaVersion:           schemadescriptor.Version,
		Clients:                 facts.Clients,
		RetryPolicies:           facts.RetryPolicies,
		Strategies:              facts.Strategies,
		Templates:               projectTemplates(facts.Templates),
	}

	// caps accumulates one MethodCapability for EVERY retained method (admitted or
	// declined) so M4 can read a complete per-method manifest; it is sorted by
	// method name at the end.
	var caps []projectdescriptor.MethodCapability

	names := make([]string, 0, len(facts.Funcs))
	for name := range facts.Funcs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn := facts.Funcs[name]
		if code, detail := classify(fn); code != "" {
			p.Diagnostics = append(p.Diagnostics, projectdescriptor.Decline{Method: name, Code: code, Detail: detail})
			caps = append(caps, projectdescriptor.MethodCapability{Method: name, Admitted: false, Blocked: code})
			continue
		}
		m := admitMethod(name, fn)
		p.Methods = append(p.Methods, m)
		caps = append(caps, projectdescriptor.MethodCapability{
			Method:   name,
			Admitted: true,
			Class:    m.Class,
			Required: append([]projectdescriptor.CapabilityCode(nil), m.RequiredCapabilities...),
		})
	}

	// Functions that never got a V3 prompt descriptor are declined too; the
	// native lane must record them, never silently drop a retained method (D3).
	// Their specific upstream reason is mapped to the most precise stable code so
	// diagnostic tallies stay trustworthy.
	preNames := make([]string, 0, len(facts.PreDeclines))
	for name := range facts.PreDeclines {
		if _, described := facts.Funcs[name]; described {
			continue // already handled above
		}
		preNames = append(preNames, name)
	}
	sort.Strings(preNames)
	for _, name := range preNames {
		code := preDeclineCode(facts.PreDeclineFeatures[name])
		p.Diagnostics = append(p.Diagnostics, projectdescriptor.Decline{
			Method: name, Code: code, Detail: facts.PreDeclines[name],
		})
		caps = append(caps, projectdescriptor.MethodCapability{Method: name, Admitted: false, Blocked: code})
	}

	// Diagnostics are appended in two passes (classifier declines, then
	// pre-declines), each name-sorted internally; sort the COMBINED slice by method
	// name so the whole-project ordering contract holds regardless of which pass a
	// decline came from.
	sort.Slice(p.Diagnostics, func(i, j int) bool { return p.Diagnostics[i].Method < p.Diagnostics[j].Method })
	sort.Slice(caps, func(i, j int) bool { return caps[i].Method < caps[j].Method })
	p.Capabilities = caps
	return p
}

// projectTemplates projects the passive macro set into the JSON-clean descriptor
// [projectdescriptor.Template] (argument NAMES only — a macro argument is never
// bound to a resolved value type, and its bamlparser AST type does not round-trip).
func projectTemplates(macros []promptdescriptor.TemplateString) []projectdescriptor.Template {
	if len(macros) == 0 {
		return nil
	}
	out := make([]projectdescriptor.Template, 0, len(macros))
	for _, m := range macros {
		var argNames []string
		if len(m.Args) > 0 {
			argNames = make([]string, 0, len(m.Args))
			for _, a := range m.Args {
				argNames = append(argNames, a.Name)
			}
		}
		out = append(out, projectdescriptor.Template{
			Name: m.Name,
			Args: argNames,
			Body: m.Body,
		})
	}
	return out
}

// admitMethod projects a promptdescriptor.Function into an admitted
// ClassStaticUnary Method, composing the JSON-clean sub-descriptors.
func admitMethod(name string, fn promptdescriptor.Function) projectdescriptor.Method {
	args := make([]projectdescriptor.Argument, 0, len(fn.Args))
	for _, a := range fn.Args {
		var vt promptdescriptor.ResolvedValueType
		if a.ValueType != nil {
			vt = *a.ValueType
		}
		args = append(args, projectdescriptor.Argument{Name: a.Name, Type: vt})
	}
	return projectdescriptor.Method{
		Name:                 name,
		Class:                projectdescriptor.ClassStaticUnary,
		Prompt:               fn.Prompt,
		Args:                 args,
		Client:               fn.Client,
		Provider:             effectiveProvider(fn),
		Model:                projectdescriptor.Model{Value: fn.ClientConfig.Model.Value, Provenance: fn.ClientConfig.Model.Provenance},
		Return:               fn.Return,
		RequiredCapabilities: append([]projectdescriptor.CapabilityCode(nil), admittedCapabilities...),
	}
}

func effectiveProvider(fn promptdescriptor.Function) string {
	if fn.ClientConfig.Present && fn.ClientConfig.Provider != "" {
		return fn.ClientConfig.Provider
	}
	return fn.Provider
}

// classify returns the first blocking decline code (and detail) for fn, or "" if
// fn is admissible as ClassStaticUnary. Checks run in a fixed, deterministic
// priority: output schema, input args, name collisions, prompt profile, then
// client.
func classify(fn promptdescriptor.Function) (projectdescriptor.CapabilityCode, string) {
	if code, detail := classifyOutputSchema(fn.Return); code != "" {
		return code, detail
	}
	if code, detail := classifyInputArgs(fn.Args); code != "" {
		return code, detail
	}
	if code, detail := classifyNameCollisions(fn); code != "" {
		return code, detail
	}
	if code, detail := classifyPrompt(fn); code != "" {
		return code, detail
	}
	return classifyClient(fn)
}

// classifyNameCollisions declines a method whose emitted Go identifiers would
// collide after the emitter's lossy strcase normalization — producing an
// uncompilable carrier. It DELEGATES to the emitter's CheckNativeNameCollision so
// the classifier declines EXACTLY the identifier set the emitter would reject
// (input carrier vs output types, enum type/constant collisions, and per-struct
// fields) — one source of truth, no drift.
func classifyNameCollisions(fn promptdescriptor.Function) (projectdescriptor.CapabilityCode, string) {
	argNames := make([]string, len(fn.Args))
	for i, a := range fn.Args {
		argNames[i] = a.Name
	}
	if err := codegen.CheckNativeNameCollision(fn.Method, argNames, fn.Return); err != nil {
		return DeclineNameCollision, err.Error()
	}
	return "", ""
}

// classifyClient gates the default client. Strategy providers are checked first
// (so a fallback/round-robin client reports the strategy code, not
// provider_not_openai), then provider, then model provenance, then escaped
// regular model literals, then body-affecting configuration.
func classifyClient(fn promptdescriptor.Function) (projectdescriptor.CapabilityCode, string) {
	provider := effectiveProvider(fn)
	switch {
	case roundRobinProviders[provider]:
		return DeclineStrategyRoundRobin, "client uses a round-robin strategy provider (" + provider + ")"
	case fallbackProviders[provider]:
		return DeclineStrategyFallback, "client uses a fallback strategy provider (" + provider + ")"
	case provider == providerOpenAI:
		// proven provider; continue
	default:
		return DeclineProviderNotOpenAI, "provider is " + provider + ", only openai is proven"
	}

	model := fn.ClientConfig.Model
	if model.Provenance != promptdescriptor.ModelProvenanceLiteral {
		return DeclineModelNotLiteral, "model provenance is " + string(model.Provenance) + ", only a literal model is admissible"
	}
	// Mirror internal/nativebody FeatureModelEscape (support.go:463-469): a
	// regular (non-raw) string literal bearing a backslash carries an escape BAML
	// decodes and native cannot prove byte-for-byte.
	if !model.RawString && strings.ContainsRune(model.Value, '\\') {
		return DeclineModelEscape, "regular string model literal contains an escape sequence"
	}

	// Body-affecting client configuration (temperature / tools / request_body ...)
	// would change the request, but the Project projection carries only the model.
	// Decline rather than silently drop it. Transport options (base_url/api_key)
	// are fine.
	if fn.ClientConfig.RequestBodyPresent {
		return DeclineClientBodyOption, "client declares a request_body block the descriptor does not carry"
	}
	if len(fn.ClientConfig.BodyAffectingOptions) > 0 {
		return DeclineClientBodyOption, "client declares body-affecting options (e.g. temperature/tools) the descriptor does not carry"
	}
	return "", ""
}

// classifyPrompt delegates the entire prompt-profile decision to the STRUCTURED
// nativeprompt static analyzer (a real MiniJinja parse), rather than scanning raw
// bytes. It asks the faithful question: is THIS prompt reproducible from what the
// M1 Project actually carries — the raw prompt + scalar args, with NO project
// macro set and NO project enum/class universe? It answers by calling
// SupportsStatic on a copy of the function with Macros and InputValues cleared
// and a synthesized type-valid value vector. nil means a supported text /
// standard-role-chat prompt (standard roles incl. the role= kwarg and _.chat
// spellings, correct spacing, no per-prompt enum/macro/filter/ctx dependency); a
// *Decline maps to a stable code.
//
// Clearing the universe is principled, not a hack: because the M1 descriptor
// carries no enum universe or macro bodies, a prompt that resolves ONLY with them
// is not reproducible from the descriptor and must decline. This makes an UNUSED
// project enum/macro leave a plain prompt admitted (removing a reference flips the
// verdict), while an actual enum reference or macro call declines.
func classifyPrompt(fn promptdescriptor.Function) (projectdescriptor.CapabilityCode, string) {
	// Enum-namespace shadow: the real analyzer (nativeprompt static_bind.go:246)
	// declines an argument whose name shadows a project enum global, because the
	// bound variable would silently reinterpret `Enum.MEMBER`. Clearing the
	// universe below would hide that fact, so check it here WITH the universe. This
	// mirrors the enum-only fence; class names are not installed as globals.
	if code, detail := classifyEnumShadow(fn); code != "" {
		return code, detail
	}

	// Build the probe as what the M1 Project actually carries: no project macro
	// set, no enum/class universe, and — since the analyzer refuses a nullable
	// DECLARATION before it evaluates the prompt (static_bind.go:352) while M1
	// emits a valid nullable carrier — arguments with the nullable flag cleared so
	// a present value binds and the prompt's ACTUAL dependency is evaluated.
	probe := fn
	probe.Macros = nil
	probe.InputValues = promptdescriptor.InputValueUniverse{}
	probe.Args = nonNullableArgs(fn.Args)

	err := nativeprompt.SupportsStatic(probe, synthesizeArgumentValues(probe.Args))
	if err == nil {
		return "", ""
	}
	var d *nativeprompt.Decline
	if !errors.As(err, &d) {
		// A non-Decline invariant error: fail closed to a prompt decline.
		return DeclinePromptDependency, "prompt could not be statically validated: " + err.Error()
	}
	switch d.Feature {
	case nativeprompt.FeatureRoleCallShape, nativeprompt.FeatureChatLayout:
		return DeclineRoleUnsupported, d.Detail
	default:
		// enum/class reference, macro CALL, {% macro %} statement, unknown filter,
		// unsupported ctx, callable output_format, reserved delimiter, unrecognized
		// prompt, etc. — the prompt depends on something the descriptor cannot carry.
		return DeclinePromptDependency, d.Detail
	}
}

// synthesizeArgumentValues builds a type-valid, non-empty projected value vector
// for the scalar / list-of-scalar M1 argument profile — enough for the static
// analyzer to bind and render. Non-empty payloads avoid a spurious empty-message
// chat-layout decline. Class/enum inputs are declined upstream, so only scalar
// and list-of-scalar kinds occur here.
func synthesizeArgumentValues(args []promptdescriptor.Argument) []promptdescriptor.ArgumentValue {
	out := make([]promptdescriptor.ArgumentValue, 0, len(args))
	for _, a := range args {
		var vt promptdescriptor.ResolvedValueType
		if a.ValueType != nil {
			vt = *a.ValueType
		}
		out = append(out, promptdescriptor.ArgumentValue{Name: a.Name, Value: synthesizeStaticValue(vt)})
	}
	return out
}

func synthesizeStaticValue(vt promptdescriptor.ResolvedValueType) promptdescriptor.StaticValue {
	switch vt.Kind {
	case promptdescriptor.ValueString:
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticString, String: "x"}
	case promptdescriptor.ValueInt:
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticInt, Int: 1}
	case promptdescriptor.ValueFloat:
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticFloat, Float: 1}
	case promptdescriptor.ValueBool:
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticBool, Bool: true}
	case promptdescriptor.ValueList:
		if vt.Elem != nil {
			return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticList, Items: []promptdescriptor.StaticValue{synthesizeStaticValue(*vt.Elem)}}
		}
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticList}
	default:
		return promptdescriptor.StaticValue{Kind: promptdescriptor.StaticNull}
	}
}

// classifyEnumShadow declines a method whose argument name shadows a project enum
// namespace global — the load-bearing fence the analyzer applies WITH the
// universe (static_bind.go:246). It is detected here explicitly because the probe
// clears the universe. Mirrors the enum-only rule: class names are not enum
// globals, so they do not shadow.
func classifyEnumShadow(fn promptdescriptor.Function) (projectdescriptor.CapabilityCode, string) {
	if len(fn.InputValues.ProjectEnums) == 0 {
		return "", ""
	}
	enumNames := make(map[string]bool, len(fn.InputValues.ProjectEnums))
	for _, e := range fn.InputValues.ProjectEnums {
		enumNames[e.Name] = true
	}
	for _, a := range fn.Args {
		if enumNames[a.Name] {
			return DeclinePromptDependency, "argument " + a.Name + " shadows the project enum namespace global of the same name"
		}
	}
	return "", ""
}

// nonNullableArgs returns a copy of args with each argument's resolved value type
// made non-nullable (recursively for lists). The M1 emitter carries an optional
// as a pointer; the static analyzer refuses a nullable declaration outright, so
// the probe presents a present value to evaluate the prompt's real dependency.
func nonNullableArgs(args []promptdescriptor.Argument) []promptdescriptor.Argument {
	out := make([]promptdescriptor.Argument, len(args))
	for i, a := range args {
		out[i] = a
		if a.ValueType != nil {
			vt := clearNullable(*a.ValueType)
			out[i].ValueType = &vt
		}
	}
	return out
}

func clearNullable(vt promptdescriptor.ResolvedValueType) promptdescriptor.ResolvedValueType {
	vt.Nullable = false
	if vt.Elem != nil {
		e := clearNullable(*vt.Elem)
		vt.Elem = &e
	}
	return vt
}

// classifyInputArgs declines any argument outside the M1 input profile — scalar
// (string/int/float/bool) or a list of scalars, optionally nullable. Class/enum
// inputs are declined for M1 because the emitter does not yet generate input
// carriers for them; the classifier and the emitter must admit exactly the same
// shapes.
func classifyInputArgs(args []promptdescriptor.Argument) (projectdescriptor.CapabilityCode, string) {
	for _, a := range args {
		if a.ValueType == nil {
			return DeclineUnsupportedInputShape, "argument " + a.Name + " has no resolved value type"
		}
		if !inputWithinM1Profile(*a.ValueType) {
			return DeclineUnsupportedInputShape, "argument " + a.Name + " uses an input kind outside the M1 scalar profile"
		}
	}
	return "", ""
}

func inputWithinM1Profile(vt promptdescriptor.ResolvedValueType) bool {
	switch vt.Kind {
	case promptdescriptor.ValueString, promptdescriptor.ValueInt, promptdescriptor.ValueFloat, promptdescriptor.ValueBool:
		return true
	case promptdescriptor.ValueList:
		return vt.Elem != nil && inputWithinM1Profile(*vt.Elem)
	default:
		// enum, class, null: outside the M1 input profile.
		return false
	}
}

// classifyOutputSchema walks the return Bundle for the first feature that blocks
// ClassStaticUnary. Recursion, then per-type features (dynamic, media,
// constraints, null, unsupported kinds) in a deterministic walk order.
func classifyOutputSchema(b schemadescriptor.Bundle) (projectdescriptor.CapabilityCode, string) {
	if len(b.RecursiveClasses) > 0 || len(b.StructuralRecursiveAliases) > 0 {
		return DeclineUnsupportedOutputShape, "return schema is recursive"
	}
	seen := map[*schemadescriptor.Type]bool{}
	if code, detail := walkType(&b.Target, seen); code != "" {
		return code, detail
	}
	for i := range b.Classes {
		c := &b.Classes[i]
		if code, detail := constraintCode(c.Constraints); code != "" {
			return code, detail
		}
		for j := range c.Fields {
			if code, detail := walkType(&c.Fields[j].Type, seen); code != "" {
				return code, detail
			}
		}
	}
	for i := range b.Enums {
		if code, detail := constraintCode(b.Enums[i].Constraints); code != "" {
			return code, detail
		}
	}
	return "", ""
}

func walkType(t *schemadescriptor.Type, seen map[*schemadescriptor.Type]bool) (projectdescriptor.CapabilityCode, string) {
	if t == nil || seen[t] {
		return "", ""
	}
	seen[t] = true

	if t.Dynamic {
		return DeclineSchemaDynamicClass, "return schema references a @@dynamic type"
	}
	if code, detail := constraintCode(t.Meta.Constraints); code != "" {
		return code, detail
	}

	switch t.Kind {
	case schemadescriptor.TypePrimitive:
		switch t.Primitive {
		case schemadescriptor.PrimitiveString, schemadescriptor.PrimitiveInt, schemadescriptor.PrimitiveFloat, schemadescriptor.PrimitiveBool:
			return "", ""
		case schemadescriptor.PrimitiveMedia:
			return mediaCode(t.Media), "return schema contains media " + string(t.Media)
		default:
			// null and any future primitive: the emitter cannot generate a
			// carrier, so decline rather than admit-then-fail.
			return DeclineUnsupportedOutputShape, "return schema contains unsupported primitive " + string(t.Primitive)
		}
	case schemadescriptor.TypeEnum, schemadescriptor.TypeClass:
		return "", "" // named ref; the def is walked at the Bundle level
	case schemadescriptor.TypeList:
		return walkType(t.Elem, seen)
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return "", ""
		}
		if len(t.Union.Variants) > 1 {
			return DeclineUnsupportedOutputShape, "return schema contains a multi-variant union"
		}
		for i := range t.Union.Variants {
			if code, detail := walkType(&t.Union.Variants[i], seen); code != "" {
				return code, detail
			}
		}
		return "", ""
	default:
		// top, literal, map, tuple, arrow, recursive_alias: outside the M1 class.
		return DeclineUnsupportedOutputShape, "return schema contains an unsupported type kind " + string(t.Kind)
	}
}

func mediaCode(k schemadescriptor.MediaKind) projectdescriptor.CapabilityCode {
	switch k {
	case schemadescriptor.MediaImage:
		return DeclineMediaImage
	case schemadescriptor.MediaAudio:
		return DeclineMediaAudio
	case schemadescriptor.MediaPDF:
		return DeclineMediaPDF
	case schemadescriptor.MediaVideo:
		return DeclineMediaVideo
	default:
		return DeclineMediaPart
	}
}

func constraintCode(cs []schemadescriptor.Constraint) (projectdescriptor.CapabilityCode, string) {
	for _, c := range cs {
		switch c.Level {
		case schemadescriptor.ConstraintCheck:
			return DeclineChecks, "return schema carries a @check constraint"
		case schemadescriptor.ConstraintAssert:
			return DeclineAsserts, "return schema carries an @assert constraint"
		}
	}
	return "", ""
}

// preDeclineCode maps the STRUCTURAL feature nativeschema stamped for a
// pre-decline onto a stable capability code. It is a pure lookup on a verdict the
// producing path carried — no reason string is read here, so no help prose and no
// user-controlled declaration name (a class, field, or macro name) can ever
// influence the code. The full reason is preserved separately in Decline.Detail.
//
// M1 stamps a precise feature (FeatureDynamic) only where the winning producer
// knows it exactly; every other decline is named by its reliable input/return
// context. FeatureNone is a defensive default for any decline that reaches here
// without a carried feature. Precise media/checks sub-codes for schema-builder
// pre-declines are M2 (see the package doc + docs/codegen-spine/04). checks,
// asserts, and the media codes remain emittable via the admitted classifier path
// (classifyOutputSchema), so DeclineCodes()/the catalogue are unchanged.
func preDeclineCode(feature nativeschema.PreDeclineFeature) projectdescriptor.CapabilityCode {
	switch feature {
	case nativeschema.FeatureDynamic:
		return DeclineSchemaDynamicClass
	case nativeschema.FeatureOutputShape:
		return DeclineUnsupportedOutputShape
	default:
		// FeatureInputShape and the defensive FeatureNone.
		return DeclineUnsupportedInputShape
	}
}
