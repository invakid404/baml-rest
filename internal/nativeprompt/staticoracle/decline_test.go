// This file is the de-BAML Phase 3 slice-2 END-TO-END decline corpus. Unlike
// the slice-1 decline matrix (internal/nativeprompt/static_support_test.go),
// which drives SupportsStatic/RenderStatic with HAND-CONSTRUCTED descriptors,
// this corpus parses REAL inline .baml mini-projects through the exact native
// build pipeline the claimed corpus uses — bamlparser.ParseBytes ->
// nativeschema.BuildStaticSchemas -> nativeschema.BuildPromptDescriptors — and
// then proves the decline. It closes the pipeline gap between Phase 1 (which
// populates promptdescriptor.Function, e.g. its Macros set and argument
// TypeExprs, from source) and the Phase 3 static gate.
//
// It is PURE GO (no build tag, no CGO, no BAML): BAML is authoritative but does
// not need to be invoked for a surface native REFUSES to claim — the required
// proof is SupportsStatic/RenderStatic returning the exact *Decline (unwrapping
// to ErrUnsupported) and a nil render. Each mini-project is macro-free UNLESS it
// is specifically testing template_string, so the claimed macro-free fixture is
// never poisoned.
//
// Two decline tiers are proven, matching the scope's contract:
//
//   - Descriptor-producing declines: the builder emits a descriptor (a real
//     Macros set / argument type / prompt source), and the Phase 3 gate then
//     declines with the exact feature key.
//   - Builder-layer declines (@skip / @@dynamic reachable): the builder emits NO
//     descriptor at all and records a reason, so the Phase 3 gate is never
//     reached. These are asserted at the BuildPromptDescriptors layer against a
//     REAL descriptor absence — never a fabricated nativeprompt descriptor. The
//     authoritative descriptor-layer proofs live in
//     internal/nativeschema/prompt_test.go; this re-confirms them through the
//     same builder the native oracle leg uses.

package staticoracle

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

// strArgs builds a single-string projected argument vector, the shape a
// generated projector produces for a one-`string` signature.
func strArgs(name, v string) []promptdescriptor.ArgumentValue {
	return []promptdescriptor.ArgumentValue{{
		Name:  name,
		Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticString, String: v},
	}}
}

// enumArgs / classArgs / listArgs build the projected vectors for the enum,
// class and list rows, exactly as a generated projector would: canonical member
// and field names as literals, source field order, input item order.
func enumArgs(name, enumName, canonical string) []promptdescriptor.ArgumentValue {
	return []promptdescriptor.ArgumentValue{{
		Name: name,
		Value: promptdescriptor.StaticValue{
			Kind: promptdescriptor.StaticEnum, TypeName: enumName, Canonical: canonical,
		},
	}}
}

func classArgs(name, className, field, value string) []promptdescriptor.ArgumentValue {
	return []promptdescriptor.ArgumentValue{{
		Name: name,
		Value: promptdescriptor.StaticValue{
			Kind: promptdescriptor.StaticClass, TypeName: className,
			Fields: []promptdescriptor.StaticFieldValue{{
				Canonical: field,
				Value:     promptdescriptor.StaticValue{Kind: promptdescriptor.StaticString, String: value},
			}},
		},
	}}
}

func listArgs(name string, items ...string) []promptdescriptor.ArgumentValue {
	vs := make([]promptdescriptor.StaticValue, 0, len(items))
	for _, it := range items {
		vs = append(vs, promptdescriptor.StaticValue{Kind: promptdescriptor.StaticString, String: it})
	}
	return []promptdescriptor.ArgumentValue{{
		Name:  name,
		Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticList, Items: vs},
	}}
}

// TestRealDescriptorAdmitsBoundHostValues is the POSITIVE half of the same
// pipeline: real .baml source whose enum/class/list argument the V3 resolver
// describes exactly, bound through the production binder and RENDERED. It is
// what makes the decline rows above meaningful — without it they could all be
// passing because the pipeline declines everything.
//
// The byte-exact answers are owned by the stock differential oracle; this proves
// the SOURCE -> descriptor -> binder -> render path is closed end to end.
func TestRealDescriptorAdmitsBoundHostValues(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		values []promptdescriptor.ArgumentValue
		want   string
	}{
		{
			name: "enum_argument_renders_its_alias",
			src: `enum Color { RED @alias("rouge") GREEN }
client<llm> C { provider openai options { model "m" } }
function F(c: Color) -> string { client C prompt #"{{ c }}"# }`,
			values: enumArgs("c", "Color", "RED"),
			want:   "rouge",
		},
		{
			name: "enum_argument_canonical_equality",
			src: `enum Color { RED @alias("rouge") GREEN }
client<llm> C { provider openai options { model "m" } }
function F(c: Color) -> string { client C prompt #"{{ c == 'RED' }}"# }`,
			values: enumArgs("c", "Color", "RED"),
			want:   "true",
		},
		{
			name: "literal_only_enum_comparison_needs_no_argument",
			src: `enum Color { RED @alias("rouge") GREEN }
client<llm> C { provider openai options { model "m" } }
function F() -> string { client C prompt #"{{ Color.RED == 'RED' }}"# }`,
			values: nil,
			want:   "true",
		},
		{
			name: "class_argument_renders_alias_keys_in_source_order",
			src: `class Item { name string @alias("nom") }
client<llm> C { provider openai options { model "m" } }
function F(it: Item) -> string { client C prompt #"{{ it }}"# }`,
			values: classArgs("it", "Item", "name", "n"),
			want:   "{\n    \"nom\": \"n\",\n}",
		},
		{
			name: "list_argument_renders_in_input_order",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topics: string[]) -> string { client C prompt #"{{ topics }}"# }`,
			values: listArgs("topics", "b", "a"),
			want:   `["b", "a"]`,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			descriptors, promptDeclines := buildOne(t, c.name, c.src)
			fn, ok := descriptors["F"]
			if !ok {
				t.Fatalf("expected a built descriptor for F; builder decline = %q", promptDeclines["F"])
			}
			if err := nativeprompt.SupportsStatic(fn, c.values); err != nil {
				t.Fatalf("SupportsStatic declined an admitted host-value row: %v", err)
			}
			rp, err := nativeprompt.RenderStatic(fn, c.values)
			if err != nil {
				t.Fatalf("RenderStatic: %v", err)
			}
			if rp == nil || rp.Kind != nativeprompt.KindCompletion {
				t.Fatalf("expected a completion, got %+v", rp)
			}
			if rp.Completion != c.want {
				t.Errorf("rendered %q, want %q", rp.Completion, c.want)
			}
		})
	}
}

// declineClientProvider is the client->provider map shared by every mini-project
// below (each declares `client<llm> C { provider openai ... }`).
var declineClientProvider = map[string]string{"C": "openai"}

// buildOne parses a single-file inline .baml mini-project and runs the native
// build pipeline, returning the descriptors and prompt declines keyed by
// function name.
func buildOne(t *testing.T, name, src string) (map[string]promptdescriptor.Function, map[string]string) {
	t.Helper()
	f, err := bamlparser.ParseBytes(name+".baml", []byte(src))
	if err != nil {
		t.Fatalf("%s: ParseBytes: %v", name, err)
	}
	files := []nativeschema.SourceFile{{File: f, Path: name + ".baml"}}
	schemas, schemaDeclines := nativeschema.BuildStaticSchemas(files)
	descriptors, promptDeclines := nativeschema.BuildPromptDescriptors(files, schemas, schemaDeclines, declineClientProvider, nativeschema.BuildClientConfigs(files))
	return descriptors, promptDeclines
}

// TestRealDescriptorDeclineMatrix proves that real .baml source whose descriptor
// the builder DOES emit is then declined by the Phase 3 static gate with the
// exact feature key, and that RenderStatic returns a nil prompt. Every row
// asserts all four required properties: non-nil error, errors.Is(ErrUnsupported),
// errors.As(*Decline) with the exact Feature, and a nil RenderStatic result.
func TestRealDescriptorDeclineMatrix(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// values is the PROJECTED argument vector (de-BAML Slice 7.1b). A row
		// states it explicitly because the vector — its order, names, and per-value
		// kinds — is what the binder validates against the descriptor; deriving it
		// from a raw map would test a translation the production path lacks.
		values  []promptdescriptor.ArgumentValue
		feature string
	}{
		// template_string, UNCALLED: BAML injects every project macro into every
		// function, so the descriptor carries Macros and the gate declines even
		// though the prompt never references the macro.
		{
			name: "template_string_uncalled",
			src: `template_string Greet(n: string) #"hi {{ n }}"#
client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"About {{ topic }}."# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureTemplateString,
		},
		// template_string, CALLED: same decline; the macro is invoked in the prompt.
		{
			name: "template_string_called",
			src: `template_string Greet(n: string) #"hi {{ n }}"#
client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"{{ Greet(topic) }}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureTemplateString,
		},
		// De-BAML Slice 7.1b: a bare enum/class/list ARGUMENT render is now
		// ADMITTED (see TestRealDescriptorAdmitsBoundHostValues). What stays
		// declined is TRAVERSING one — source field knowledge is not permission to
		// walk it.
		{
			name: "enum_arg_attribute",
			src: `enum Color { RED GREEN }
client<llm> C { provider openai options { model "m" } }
function F(c: Color) -> string { client C prompt #"pick {{ c.value }}"# }`,
			values:  enumArgs("c", "Color", "RED"),
			feature: nativeprompt.FeatureEnumClassValue,
		},
		{
			name: "class_arg_field_access",
			src: `class Item { name string }
client<llm> C { provider openai options { model "m" } }
function F(it: Item) -> string { client C prompt #"item {{ it.name }}"# }`,
			values:  classArgs("it", "Item", "name", "n"),
			feature: nativeprompt.FeatureEnumClassValue,
		},
		{
			name: "enum_arg_display_alias_equality",
			src: `enum Color { RED @alias("rouge") GREEN }
client<llm> C { provider openai options { model "m" } }
function F(c: Color) -> string { client C prompt #"{{ c == 'rouge' }}"# }`,
			values:  enumArgs("c", "Color", "RED"),
			feature: nativeprompt.FeatureEnumComparison,
		},
		// Optional primitive argument: declined as an unsupported arg type.
		{
			name: "optional_primitive_arg",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topic: string?) -> string { client C prompt #"About {{ topic }}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureStaticArgType,
		},
		// A list argument renders bare (admitted below); INDEXING it does not.
		{
			name: "list_arg_index",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topics: string[]) -> string { client C prompt #"About {{ topics[0] }}"# }`,
			values:  listArgs("topics", "a", "b"),
			feature: nativeprompt.FeatureUnrecognizedPrompt,
		},
		// Callable ctx.output_format (render_null_as): declined; only bare is ok.
		{
			name: "callable_output_format",
			src: `class O { answer string }
client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> O { client C prompt #"{{ ctx.output_format(render_null_as="null") }} {{ topic }}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureCallableOutputFmt,
		},
		// A custom BAML filter: declined (static Phase 3 reproduces no filters).
		{
			name: "filter_format",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"{{ topic | format(type="yaml") }}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureUnknownFilter,
		},
		// ctx.client is an unsupported ctx member.
		{
			name: "ctx_client",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"{{ ctx.client }} {{ topic }}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureUnsupportedCtx,
		},
		// A {% if %} statement (control flow) is outside the allowlist.
		{
			name: "if_statement",
			src: `client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"{% if topic %}{{ topic }}{% endif %}"# }`,
			values:  strArgs("topic", "x"),
			feature: nativeprompt.FeatureUnrecognizedPrompt,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			descriptors, promptDeclines := buildOne(t, c.name, c.src)
			fn, ok := descriptors["F"]
			if !ok {
				t.Fatalf("expected a built descriptor for F (this row must decline at the STATIC GATE, not the builder); builder decline = %q", promptDeclines["F"])
			}

			err := nativeprompt.SupportsStatic(fn, c.values)
			assertDecline(t, err, c.feature)

			rp, rerr := nativeprompt.RenderStatic(fn, c.values)
			if rp != nil {
				t.Errorf("RenderStatic returned a non-nil prompt on a declined case: %+v", rp)
			}
			assertDecline(t, rerr, c.feature)
		})
	}
}

// assertDecline checks the four required decline properties: non-nil, unwraps to
// ErrUnsupported, is a *Decline, and carries the exact feature key.
func assertDecline(t *testing.T, err error, wantFeature string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a decline (%s), got nil error", wantFeature)
	}
	if !errors.Is(err, nativeprompt.ErrUnsupported) {
		t.Errorf("decline does not wrap ErrUnsupported: %v", err)
	}
	var d *nativeprompt.Decline
	if !errors.As(err, &d) {
		t.Fatalf("decline is not a *nativeprompt.Decline: %v", err)
	}
	if d.Feature != wantFeature {
		t.Errorf("decline feature = %q, want %q (detail: %s)", d.Feature, wantFeature, d.Detail)
	}
}

// TestBuilderLayerDeclineAbsence proves that reachable @skip / @@dynamic decline
// at the BuildPromptDescriptors layer: the builder emits NO descriptor for the
// function and records a reason. These never reach the Phase 3 renderer, so the
// proof is a REAL descriptor ABSENCE plus a builder reason — NOT a fabricated
// nativeprompt descriptor. (Authoritative coverage: nativeschema/prompt_test.go;
// this re-confirms it end-to-end through the same builder the oracle leg uses.)
func TestBuilderLayerDeclineAbsence(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		reasonSubstr string
	}{
		{
			name: "skip_reachable_from_input",
			src: `class In { keep string drop string @skip }
client<llm> C { provider openai options { model "m" } }
function F(x: In) -> string { client C prompt #"{{ x }}"# }`,
			reasonSubstr: "@skip",
		},
		{
			name: "dynamic_reachable_from_input",
			src: `class In { base string @@dynamic }
client<llm> C { provider openai options { model "m" } }
function F(x: In) -> string { client C prompt #"{{ x }}"# }`,
			reasonSubstr: "@@dynamic",
		},
		{
			name: "dynamic_reachable_from_return",
			src: `class Out { @@dynamic }
client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> Out { client C prompt #"{{ ctx.output_format }} {{ topic }}"# }`,
			reasonSubstr: "return bundle unavailable",
		},
		// De-BAML Slice 7.1b: a MEDIA argument moved DOWN a tier. Media is not a
		// V3 input value node, so the descriptor is no longer built at all and the
		// static gate is never reached — the media decline is now structural
		// (build-time) rather than a render-time feature key.
		{
			name: "media_image_arg",
			src: `client<llm> C { provider openai options { model "m" } }
function F(img: image) -> string { client C prompt #"see {{ img }}"# }`,
			reasonSubstr: "media types are not supported",
		},
		// A MAP argument: V3 has no map node, so it too declines pre-descriptor.
		{
			name: "map_arg",
			src: `client<llm> C { provider openai options { model "m" } }
function F(m: map<string, string>) -> string { client C prompt #"{{ m }}"# }`,
			reasonSubstr: "map types are not supported",
		},
		// A RECURSIVE input class graph: representable as a name reference, but
		// unproven for rendering, so it is a build-time decline (the binder
		// declines a recursive universe independently).
		{
			name: "recursive_input_class",
			src: `class NodeIn { value string next NodeIn? }
client<llm> C { provider openai options { model "m" } }
function F(n: NodeIn) -> string { client C prompt #"{{ n }}"# }`,
			reasonSubstr: "recursive input class graph",
		},
		// An unresolvable PROJECT enum alias poisons V3 for every function, even
		// one that never mentions the enum: BAML installs the namespace globals as
		// a complete set, so a partial environment would be a lie.
		{
			name: "project_enum_dynamic_alias",
			src: `enum Color { RED @alias(x) GREEN }
client<llm> C { provider openai options { model "m" } }
function F(topic: string) -> string { client C prompt #"About {{ topic }}"# }`,
			reasonSubstr: "project enum \"Color\"",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			descriptors, promptDeclines := buildOne(t, c.name, c.src)
			if _, ok := descriptors["F"]; ok {
				t.Fatalf("expected NO descriptor for F (builder-layer decline), but one was built")
			}
			reason, ok := promptDeclines["F"]
			if !ok {
				t.Fatalf("expected a builder decline reason for F, got none")
			}
			if !strings.Contains(reason, c.reasonSubstr) {
				t.Errorf("builder decline reason = %q, want it to contain %q", reason, c.reasonSubstr)
			}
		})
	}
}
