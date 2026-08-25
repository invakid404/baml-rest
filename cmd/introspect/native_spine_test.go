package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// writeFixture writes the shared M1 fixture .baml corpus into a temp dir and
// returns it.
func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range nativespine.M1FixtureSources {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

// TestNativeSpineFlagEquivalence proves the introspect CLI pipeline
// (parseBamlSourceDir → BuildProjectDescriptor) produces exactly the descriptor
// the test-support pipeline does, and that the admitted method's return schema is
// the introspect fact verbatim (composition, not re-derivation).
func TestNativeSpineFlagEquivalence(t *testing.T) {
	dir := writeFixture(t)
	bc := parseBamlSourceDir(dir)

	proj := nativespine.BuildProjectDescriptor(bc.nativeSourceFacts())
	if err := proj.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}

	want, err := nativespine.BuildFromSource(nativespine.M1FixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	if !reflect.DeepEqual(proj, want) {
		t.Fatalf("CLI-pipeline descriptor != test-support descriptor:\n cli:  %+v\n want: %+v", proj, want)
	}

	// Golden equivalence to the already-emitted introspection facts: the admitted
	// method's return Bundle is the exact Function.Return introspect computed.
	var greet *projectdescriptor.Method
	for i := range proj.Methods {
		if proj.Methods[i].Name == "Greet" {
			greet = &proj.Methods[i]
		}
	}
	if greet == nil {
		t.Fatal("Greet not admitted")
	}
	fact, ok := bc.staticPromptDescriptors["Greet"]
	if !ok {
		t.Fatal("introspect facts missing Greet")
	}
	if !reflect.DeepEqual(greet.Return, fact.Return) {
		t.Fatal("descriptor return schema is not the introspect fact verbatim")
	}
	if greet.Prompt != fact.Prompt {
		t.Fatalf("descriptor prompt %q != introspect fact %q", greet.Prompt, fact.Prompt)
	}
}

// pipelineDeclines runs the REAL introspect pipeline (parseBamlSourceDir, which
// canonicalizes providers via enrichShorthandClientProviders) on a temp dir of
// .baml sources and returns the admitted method names and the decline map. The
// source-only helper (nativespine.BuildFromSource) does NOT canonicalize, so the
// discriminating classifier tests must use this path.
func pipelineDeclines(t *testing.T, sources map[string]string) (admitted map[string]bool, declines map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, src := range sources {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	bc := parseBamlSourceDir(dir)
	proj := nativespine.BuildProjectDescriptor(bc.nativeSourceFacts())
	if err := proj.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	admitted = map[string]bool{}
	for _, m := range proj.Methods {
		admitted[m.Name] = true
	}
	declines = map[string]string{}
	for _, d := range proj.Diagnostics {
		declines[d.Method] = string(d.Code)
	}
	return admitted, declines
}

const goodClient = `client<llm> GPT4 {
  provider openai
  options { model "gpt-4o" api_key env.K }
}
`

// TestNativeSpineClassifierDiscriminating drives the REAL pipeline and asserts
// each method-class failure earns its specific stable decline code — including
// round-robin canonicalization (P2-1), body-affecting config (P1-1), escaped
// model literals (P1-1), null output (P1-3), and media (P2-2) — while a clean
// method is admitted.
func TestNativeSpineClassifierDiscriminating(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient + `
client<llm> GPT4b {
  provider openai
  options { model "gpt-4.1" api_key env.K }
}
client<llm> RR {
  provider round-robin
  options { strategy [GPT4, GPT4b] }
}
client<llm> Temp {
  provider openai
  options { model "gpt-4o" api_key env.K temperature 0.7 }
}
client<llm> Esc {
  provider openai
  options { model "gpt\t4" api_key env.K }
}
`,
		"types.baml": `class Reply {
  foo_bar string
  fooBar string
}
`,
		"functions.baml": `
function GoodGreet(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }
function RRGreet(name: string) -> string { client RR prompt #"Hi {{ name }}"# }
function TempGreet(name: string) -> string { client Temp prompt #"Hi {{ name }}"# }
function EscGreet(name: string) -> string { client Esc prompt #"Hi {{ name }}"# }
function ToolGreet(name: string) -> string { client GPT4 prompt #"{{ _.role("tool") }} {{ name }}"# }
function NullReturn() -> null { client GPT4 prompt #"x"# }
function ImgReturn() -> image { client GPT4 prompt #"x"# }
function ImgInput(img: image) -> string { client GPT4 prompt #"{{ img }}"# }
function MapInput(m: map<string, string>) -> string { client GPT4 prompt #"x"# }
function MapOutput() -> map<string, string> { client GPT4 prompt #"x"# }
function TupleOutput() -> (string, int) { client GPT4 prompt #"x"# }
function CollideFields() -> Reply { client GPT4 prompt #"x"# }
`,
	}
	admitted, declines := pipelineDeclines(t, sources)

	if !admitted["GoodGreet"] {
		t.Errorf("GoodGreet should be admitted; declines=%v", declines)
	}
	want := map[string]string{
		"RRGreet":    "strategy_round_robin",     // P2-1: canonical baml-roundrobin
		"TempGreet":  "request_body_option",      // P1-1: temperature body option
		"EscGreet":   "model_escape",             // P1-1: escaped regular model literal
		"ToolGreet":  "role_unsupported",         // non-standard role
		"NullReturn": "unsupported_output_shape", // P1-3: null output declined in classifier
		// Media pre-declines in the static-schema builder, which M1 does not yet
		// instrument to carry its causative node; named by reliable context, not an
		// unreliable re-walk (fix #7 finding 1). Precise media_part is M2.
		"ImgReturn": "unsupported_output_shape", // media return -> OUTPUT context
		"ImgInput":  "unsupported_input_shape",  // media input  -> INPUT context
		// P2-2 (fix #2): input-graph vs return-bundle context decides the shape code.
		"MapInput":    "unsupported_input_shape",  // input map -> INPUT shape (was wrongly output)
		"MapOutput":   "unsupported_output_shape", // output map -> OUTPUT shape (classifier)
		"TupleOutput": "unsupported_output_shape", // output tuple -> OUTPUT shape (pre-decline)
		// P1-5 (fix #2): lossy Go normalization collision -> stable decline, not
		// an uncompilable carrier (foo_bar and fooBar both normalize to FooBar).
		"CollideFields": "name_collision",
	}
	for method, code := range want {
		if declines[method] != code {
			t.Errorf("%s decline = %q, want %q", method, declines[method], code)
		}
	}
}

// TestNativeSpinePromptEnumGlobalDiscriminates proves the enum-dependency gate is
// PER-PROMPT (P1-2, fix #2): in a project that defines an enum, a function that
// ACTUALLY references it declines prompt_dependency, while a plain function that
// does not is admitted. Removing the reference flips the verdict.
func TestNativeSpinePromptEnumGlobalDiscriminates(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml":   "enum Color { RED\n BLUE }\n",
		"functions.baml": `
function EnumUser() -> string { client GPT4 prompt #"Pick {{ Color.RED }}"# }
function EnumUnused(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }
`,
	}
	admitted, declines := pipelineDeclines(t, sources)
	if declines["EnumUser"] != "prompt_dependency" {
		t.Errorf("EnumUser decline = %q, want prompt_dependency", declines["EnumUser"])
	}
	if !admitted["EnumUnused"] {
		t.Errorf("EnumUnused should be admitted (does not reference the enum); declines=%v", declines)
	}
}

// TestNativeSpinePromptTemplateStringDiscriminates proves the macro-dependency
// gate is PER-PROMPT (P1-2, fix #2): in a project that defines a template_string,
// a function that CALLS it declines, while a plain function that does not is
// admitted — even though the project-wide macro slice is populated for both.
func TestNativeSpinePromptTemplateStringDiscriminates(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"functions.baml": "template_string TS(n: string) #\"Hi {{ n }}\"#\n" +
			`function TmplCaller(name: string) -> string { client GPT4 prompt #"{{ TS(name) }}"# }` + "\n" +
			`function TmplUnused(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }` + "\n",
	}
	admitted, declines := pipelineDeclines(t, sources)
	if declines["TmplCaller"] != "prompt_dependency" {
		t.Errorf("TmplCaller decline = %q, want prompt_dependency (macro call not carried)", declines["TmplCaller"])
	}
	if !admitted["TmplUnused"] {
		t.Errorf("TmplUnused should be admitted (does not call the macro); declines=%v", declines)
	}
}

// TestNativeSpineRoleGate proves the role gate uses the STRUCTURED parse (P1-2,
// fix #2), both directions: standard roles in positional, role= kwarg, spaced,
// and _.chat spellings are admitted; a non-standard role in either _.role or
// _.chat is declined role_unsupported.
func TestNativeSpineRoleGate(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"functions.baml": `
function PlainText(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }
function StdChat(name: string) -> string { client GPT4 prompt #"{{ _.role("system") }}You help.{{ _.role("user") }}Hi {{ name }}"# }
function KwargRole(name: string) -> string { client GPT4 prompt #"{{ _.role(role="user") }}Hi {{ name }}"# }
function SpacedRole(name: string) -> string { client GPT4 prompt #"{{ _.role ("user") }}Hi {{ name }}"# }
function ToolRole(name: string) -> string { client GPT4 prompt #"{{ _.role("tool") }}Hi {{ name }}"# }
function ChatTool(name: string) -> string { client GPT4 prompt #"{{ _.chat("tool") }}Hi {{ name }}"# }
`,
	}
	admitted, declines := pipelineDeclines(t, sources)
	for _, ok := range []string{"PlainText", "StdChat", "KwargRole", "SpacedRole"} {
		if !admitted[ok] {
			t.Errorf("%s should be admitted (supported role surface); decline=%q", ok, declines[ok])
		}
	}
	for _, bad := range []string{"ToolRole", "ChatTool"} {
		if declines[bad] != "role_unsupported" {
			t.Errorf("%s decline = %q, want role_unsupported", bad, declines[bad])
		}
	}
}

// TestNativeSpineOptionalArgAdmitted proves an M1-supported optional (nullable)
// argument no longer over-declines a plain prompt (fix #3, P1): the probe clears
// the nullable flag so the analyzer evaluates the prompt's actual dependency, not
// the nullable declaration.
func TestNativeSpineOptionalArgAdmitted(t *testing.T) {
	sources := map[string]string{
		"clients.baml":   goodClient,
		"functions.baml": `function Plain(unused: string?) -> string { client GPT4 prompt #"hello"# }`,
	}
	admitted, declines := pipelineDeclines(t, sources)
	if !admitted["Plain"] {
		t.Errorf("Plain(unused: string?) should be admitted; decline=%q", declines["Plain"])
	}
}

// TestNativeSpineEnumShadowDeclined proves an argument whose name shadows a
// project enum namespace is declined (fix #3, P1) — the real analyzer rejects the
// shadowed binding, and the Project carries no enum universe. An unrelated
// argument in the same project stays admitted.
func TestNativeSpineEnumShadowDeclined(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml":   "enum Color { RED\n BLUE }\n",
		"functions.baml": `
function Echo(Color: string) -> string { client GPT4 prompt #"{{ Color }}"# }
function PlainArg(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }
`,
	}
	admitted, declines := pipelineDeclines(t, sources)
	if declines["Echo"] != "prompt_dependency" {
		t.Errorf("Echo(Color:...) decline = %q, want prompt_dependency (enum-namespace shadow)", declines["Echo"])
	}
	if !admitted["PlainArg"] {
		t.Errorf("PlainArg should be admitted (no shadow); decline=%q", declines["PlainArg"])
	}
}

// TestNativeSpineNameCollisionShapes proves both normalization-collision shapes
// the earlier preflight missed decline name_collision (fix #3, P1): the input
// carrier vs an output type, and an enum CONSTANT vs a class type.
func TestNativeSpineNameCollisionShapes(t *testing.T) {
	// Input carrier vs output type: OutputFoo() -> FooInput both emit OutputFooInput.
	inCarrier := map[string]string{
		"clients.baml":   goodClient,
		"types.baml":     "class FooInput {\n value string\n}\n",
		"functions.baml": `function OutputFoo() -> FooInput { client GPT4 prompt #"x"# }`,
	}
	_, d1 := pipelineDeclines(t, inCarrier)
	if d1["OutputFoo"] != "name_collision" {
		t.Errorf("OutputFoo()->FooInput decline = %q, want name_collision (input carrier vs output type)", d1["OutputFoo"])
	}
	// Enum constant vs class type: enum Color{RED} const + class ColorRed type
	// both emit OutputColorRed.
	enumConst := map[string]string{
		"clients.baml":   goodClient,
		"types.baml":     "enum Color { RED }\nclass ColorRed {\n value string\n}\nclass Wrap {\n a Color\n b ColorRed\n}\n",
		"functions.baml": `function W() -> Wrap { client GPT4 prompt #"x"# }`,
	}
	_, d2 := pipelineDeclines(t, enumConst)
	if d2["W"] != "name_collision" {
		t.Errorf("enum-const/class-type collision decline = %q, want name_collision", d2["W"])
	}
}

// TestNativeSpinePreDeclineArgNameLeak proves the pre-decline context parser does
// not let a user-controlled argument name leak into the code (fix #3/#4, P2): a
// map INPUT argument named "output", "media", or "check" still maps to
// unsupported_input_shape.
func TestNativeSpinePreDeclineArgNameLeak(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"functions.baml": `
function BadOutput(output: map<string, string>) -> string { client GPT4 prompt #"x"# }
function BadMedia(media: map<string, string>) -> string { client GPT4 prompt #"x"# }
function BadCheck(check: map<string, string>) -> string { client GPT4 prompt #"x"# }
`,
	}
	_, declines := pipelineDeclines(t, sources)
	for _, m := range []string{"BadOutput", "BadMedia", "BadCheck"} {
		if declines[m] != "unsupported_input_shape" {
			t.Errorf("%s decline = %q, want unsupported_input_shape (arg name must not leak)", m, declines[m])
		}
	}
}

// TestNativeSpinePreDeclineReturnContext proves a @skip reachable from the return
// type is named by its OUTPUT context (unsupported_output_shape) — carried from
// the scanRoot("return") verdict, not parsed from the reason string.
func TestNativeSpinePreDeclineReturnContext(t *testing.T) {
	skip := map[string]string{
		"clients.baml":   goodClient,
		"types.baml":     "class SkipOut {\n keep string\n drop string? @skip\n}\n",
		"functions.baml": `function SkipFn() -> SkipOut { client GPT4 prompt #"x"# }`,
	}
	_, ds := pipelineDeclines(t, skip)
	if ds["SkipFn"] != "unsupported_output_shape" {
		t.Errorf("@skip-in-return-type decline = %q, want unsupported_output_shape", ds["SkipFn"])
	}
}

// TestNativeSpineChecksViaAdmittedPath proves `checks`/`asserts` remain emittable
// after fix #7 (which stopped naming features from schema-builder pre-declines):
// a VALID @check/@assert LOWERS into the return bundle, so the function is admitted
// to the classifier, and classifyOutputSchema declines it with the precise code.
// This is the reliable producer for those codes; the malformed-@check pre-decline
// no longer guesses them (see TestNativeSpinePreDeclineStructural/CheckNoParens).
func TestNativeSpineChecksViaAdmittedPath(t *testing.T) {
	for _, tc := range []struct{ attr, want string }{
		{"@check(pos, {{ this > 0 }})", "checks"},
		{"@assert(pos, {{ this > 0 }})", "asserts"},
	} {
		_, declines := pipelineDeclines(t, map[string]string{
			"clients.baml":   goodClient,
			"types.baml":     "class VC {\n score int " + tc.attr + "\n}\n",
			"functions.baml": `function VCFn() -> VC { client GPT4 prompt #"x"# }`,
		})
		if declines["VCFn"] != tc.want {
			t.Errorf("valid %s decline = %q, want %q (admitted-classifier path)", tc.attr, declines["VCFn"], tc.want)
		}
	}
}

// TestNativeSpinePreDeclineUnknownAttribute is the NEGATIVE control (fix #5, P2):
// an UNKNOWN attribute must fall to the context code, never a feature code guessed
// from the generic help text (which lists @check/@assert/@@dynamic in prose). It
// also proves whole-token matching (@checkmate is not @check).
func TestNativeSpinePreDeclineUnknownAttribute(t *testing.T) {
	cases := []struct {
		name, types, fn string
	}{
		{"AtFoo", "class Out { value string @foo }\n", "function AtFoo() -> Out { client GPT4 prompt #\"x\"# }"},
		{"AtAtFoo", "class Out { value string\n @@foo }\n", "function AtAtFoo() -> Out { client GPT4 prompt #\"x\"# }"},
		{"Checkmate", "class Out { value string @checkmate }\n", "function Checkmate() -> Out { client GPT4 prompt #\"x\"# }"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, declines := pipelineDeclines(t, map[string]string{
				"clients.baml": goodClient, "types.baml": tc.types, "functions.baml": tc.fn,
			})
			if declines[tc.name] != "unsupported_output_shape" {
				t.Errorf("%s decline = %q, want unsupported_output_shape (unknown attribute must not guess a feature code)", tc.name, declines[tc.name])
			}
		})
	}
}

// TestNativeSpinePreDeclineStructural proves pre-decline codes come from a
// STRUCTURAL verdict the winning producer carries, never from the reason string.
//
// Precise stamping is kept ONLY where the producer knows it exactly (fix #7):
//   - an input class carrying @@dynamic → schema_dynamic_class (scanRoot verdict);
//   - an otherwise-unused project enum carrying @@dynamic → schema_dynamic_class
//     (the project-wide enum decline, poisoning a clean function);
//   - an @@dynamic macro argument → schema_dynamic_class (macro-arg scan verdict).
//
// Everything the static-SCHEMA builder declines is named by its reliable CONTEXT,
// NOT by an independent re-walk that could pick an incidental feature (fix #7,
// finding 1). So the mixed, nullable-media, dotted, and single-@ cases all resolve
// to the input/return shape context — never a guessed media/checks/dynamic code.
// Precise media/checks sub-codes for these are M2.
func TestNativeSpinePreDeclineStructural(t *testing.T) {
	cases := []struct {
		name, types, fn, want string
	}{
		{
			name:  "DynInputClass",
			types: "class DynInput {\n field string\n @@dynamic\n}\n",
			fn:    `function DynInputClass(inp: DynInput) -> string { client GPT4 prompt #"Hi {{ inp }}"# }`,
			want:  "schema_dynamic_class",
		},
		{
			name:  "DynEnumUnused",
			types: "enum DynEnum {\n RED\n @@dynamic\n}\n",
			fn:    `function DynEnumUnused(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }`,
			want:  "schema_dynamic_class",
		},
		{
			name:  "DynMacroArg",
			types: "class DynMac {\n f string\n @@dynamic\n}\n",
			fn:    "template_string Mac(x: DynMac) #\"{{ x }}\"#\nfunction DynMacroArg(name: string) -> string { client GPT4 prompt #\"Hi {{ name }}\"# }",
			want:  "schema_dynamic_class",
		},
		{
			// fix #7 finding 1: a valid @check on an EARLIER field lowers fine; the
			// real cause is the LATER media field. The old re-walk stamped `checks`;
			// context correctly yields output shape.
			name:  "MixedCheckThenMedia",
			types: "class Out {\n score int @check(pos, {{ this > 0 }})\n attachment image\n}\n",
			fn:    `function MixedCheckThenMedia() -> Out { client GPT4 prompt #"x"# }`,
			want:  "unsupported_output_shape",
		},
		{
			// fix #7 finding 1: the old walk treated a union as featureless, so `image?`
			// missed media. Context names it regardless.
			name:  "NullableImageOut",
			types: "",
			fn:    `function NullableImageOut() -> image? { client GPT4 prompt #"x"# }`,
			want:  "unsupported_output_shape",
		},
		{
			name:  "NullableImageIn",
			types: "",
			fn:    `function NullableImageIn(x: image?) -> string { client GPT4 prompt #"{{ x }}"# }`,
			want:  "unsupported_input_shape",
		},
		{
			// fix #7 finding 1: only @@dynamic (block) is schema-dynamic. A single-@
			// `@dynamic` field is an unsupported attribute, NOT schema_dynamic_class.
			name:  "DynFieldSingleAt",
			types: "class DF {\n f string @dynamic\n}\n",
			fn:    `function DynFieldSingleAt() -> DF { client GPT4 prompt #"x"# }`,
			want:  "unsupported_output_shape",
		},
		{
			// malformed @check pre-declines in the schema builder; named by context.
			name:  "CheckNoParens",
			types: "class ChkBare {\n confidence int @check\n}\n",
			fn:    `function CheckNoParens() -> ChkBare { client GPT4 prompt #"x"# }`,
			want:  "unsupported_output_shape",
		},
		{
			name:  "CheckDotted",
			types: "class DotOut {\n value string @check.foo\n}\n",
			fn:    `function CheckDotted() -> DotOut { client GPT4 prompt #"x"# }`,
			want:  "unsupported_output_shape",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string]string{"clients.baml": goodClient, "functions.baml": tc.fn}
			if tc.types != "" {
				sources["types.baml"] = tc.types
			}
			_, declines := pipelineDeclines(t, sources)
			if declines[tc.name] != tc.want {
				t.Errorf("%s decline = %q, want %q (code must come from a carried structural verdict, not the reason string)", tc.name, declines[tc.name], tc.want)
			}
		})
	}
}

// TestNativeSpinePreDeclineNoNameLeak is the fix #7 finding 2 proof: a
// user-controlled declaration NAME can never influence the stable code. A poisoned
// macro reason embeds the macro name and has no colon; the old preDeclineContext
// cut at `:` and substring-tested `schema`, so a macro named `schemaHelper` leaked
// to output while `helper` stayed input. Context is now carried structurally
// (input-side for a macro decline), so both yield the SAME code.
func TestNativeSpinePreDeclineNoNameLeak(t *testing.T) {
	code := func(macroName string) string {
		_, declines := pipelineDeclines(t, map[string]string{
			"clients.baml":   goodClient,
			"functions.baml": "template_string " + macroName + "() { some brace body }\nfunction LeakFn() -> string { client GPT4 prompt #\"x\"# }",
		})
		return declines["LeakFn"]
	}
	withSchema, withPlain := code("schemaHelper"), code("helper")
	if withSchema != "unsupported_input_shape" || withPlain != "unsupported_input_shape" {
		t.Errorf("macro-name leak: schemaHelper=%q helper=%q, want both unsupported_input_shape", withSchema, withPlain)
	}
	if withSchema != withPlain {
		t.Errorf("macro NAME changed the stable code (%q vs %q); no user name may affect it", withSchema, withPlain)
	}
}

// TestNativeSpineCodecMethodCollision proves an output field that normalizes onto
// a generated codec METHOD name declines name_collision (fix #4, P1) — a Go type
// cannot have a field and method of the same name.
func TestNativeSpineCodecMethodCollision(t *testing.T) {
	sources := map[string]string{
		"clients.baml":   goodClient,
		"types.baml":     "class Reply {\n marshal_J_S_O_N string\n}\n",
		"functions.baml": `function CodecFn() -> Reply { client GPT4 prompt #"x"# }`,
	}
	_, declines := pipelineDeclines(t, sources)
	if declines["CodecFn"] != "name_collision" {
		t.Errorf("field colliding with MarshalJSON decline = %q, want name_collision", declines["CodecFn"])
	}
}

// TestNativeSpineFlagWritesJSON exercises the flag's file-output path end to end.
func TestNativeSpineFlagWritesJSON(t *testing.T) {
	dir := writeFixture(t)
	out := filepath.Join(t.TempDir(), "descriptor.json")

	if err := emitNativeSpineDescriptors(&config{BAMLSourceDir: dir, NativeSpineDescriptors: out}); err != nil {
		t.Fatalf("emitNativeSpineDescriptors: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var proj projectdescriptor.Project
	if err := json.Unmarshal(data, &proj); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if err := proj.Validate(); err != nil {
		t.Fatalf("written descriptor invalid: %v", err)
	}
	if len(proj.Methods) != 1 || proj.Methods[0].Name != "Greet" {
		t.Fatalf("methods = %+v", proj.Methods)
	}
	if len(proj.Diagnostics) != 4 {
		t.Fatalf("diagnostics = %d, want 4", len(proj.Diagnostics))
	}
}

// TestNativeSpineStrictDiagnostics proves the native-spine descriptor pass (unlike
// the best-effort generated lane) FAILS generation on invalid retained source: a
// duplicate declaration or an unresolved type reference (M2, §1.3). A clean
// project succeeds.
func TestNativeSpineStrictDiagnostics(t *testing.T) {
	run := func(sources map[string]string) error {
		dir := t.TempDir()
		for name, src := range sources {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		out := filepath.Join(t.TempDir(), "d.json")
		return emitNativeSpineDescriptors(&config{BAMLSourceDir: dir, NativeSpineDescriptors: out})
	}

	if err := run(map[string]string{
		"clients.baml":   goodClient,
		"functions.baml": `function Greet(name: string) -> string { client GPT4 prompt #"Hi {{ name }}"# }`,
	}); err != nil {
		t.Errorf("clean project failed strict generation: %v", err)
	}

	for _, tc := range []struct {
		name    string
		sources map[string]string
		want    string
	}{
		{
			name: "duplicate function",
			sources: map[string]string{
				"clients.baml": goodClient,
				"functions.baml": "function Greet(name: string) -> string { client GPT4 prompt #\"a\"# }\n" +
					"function Greet(name: string) -> string { client GPT4 prompt #\"b\"# }\n",
			},
			want: "declared more than once",
		},
		{
			name: "unresolved reference",
			sources: map[string]string{
				"clients.baml":   goodClient,
				"functions.baml": `function Greet(x: Missing) -> string { client GPT4 prompt #"x"# }`,
			},
			want: "resolves to no declared",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.sources)
			if err == nil {
				t.Fatalf("want strict error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestNativeSpineStrategyMirrorsProduction proves the descriptor's fallback/
// round-robin strategy graph matches the PRODUCTION introspect semantics for the
// pinned depth-gating and empty-list shapes (fix P1-4): nested `strategy`
// overrides the outer one (last-write-wins), an empty `strategy []` yields no
// strategy, and `start` is depth-1-gated and round-robin-only.
func TestNativeSpineStrategyMirrorsProduction(t *testing.T) {
	sources := map[string]string{
		"clients.baml": `
client<llm> NestedDepth {
    provider baml-fallback
    options {
        strategy [A, B]
        start 1
        custom_subblock {
            start 99
            endpoint_url "x"
            strategy [C, D]
        }
        region "us-east-1"
    }
}
client<llm> EmptyStrat {
    provider baml-fallback
    options { strategy [] }
}
client<llm> RRStart {
    provider round-robin
    options { strategy [A, B] start 2 }
}
client<llm> MultiKeep {
    provider round-robin
    options { strategy [A, B] start 2 }
    options { model "ignored" }
}
client<llm> MultiNew {
    provider round-robin
    options { strategy [A, B] start 2 }
    options { strategy [C, D] }
}
`,
	}
	dir := t.TempDir()
	for name, src := range sources {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	bc := parseBamlSourceDir(dir)
	proj := nativespine.BuildProjectDescriptor(bc.nativeSourceFacts())
	strat := map[string]projectdescriptor.Strategy{}
	for _, s := range proj.Strategies {
		strat[s.Name] = s
	}

	// NestedDepth: nested [C,D] wins; descriptor agrees with production fallbackChains.
	if got := strat["NestedDepth"].Children; !reflect.DeepEqual(got, []string{"C", "D"}) {
		t.Errorf("NestedDepth children = %v, want [C D] (nested override)", got)
	}
	if got := bc.fallbackChains["NestedDepth"]; !reflect.DeepEqual(got, []string{"C", "D"}) {
		t.Errorf("production fallbackChains[NestedDepth] = %v, want [C D]", got)
	}
	if strat["NestedDepth"].Start != nil {
		t.Errorf("NestedDepth (fallback) start = %v, want nil (round-robin-only)", *strat["NestedDepth"].Start)
	}

	// EmptyStrat: empty list yields no Strategy; production records no chain either.
	if _, ok := strat["EmptyStrat"]; ok {
		t.Errorf("empty strategy list produced a Strategy: %+v", strat["EmptyStrat"])
	}
	if _, ok := bc.fallbackChains["EmptyStrat"]; ok {
		t.Errorf("production recorded a fallback chain for an empty strategy list")
	}

	// RRStart: round-robin start is carried and matches production.
	rr := strat["RRStart"]
	if rr.Kind != projectdescriptor.StrategyRoundRobin || !reflect.DeepEqual(rr.Children, []string{"A", "B"}) || rr.Start == nil || *rr.Start != 2 {
		t.Errorf("RRStart = %+v, want round_robin children [A B] start 2", rr)
	}
	if bc.roundRobinStart["RRStart"] != 2 {
		t.Errorf("production roundRobinStart[RRStart] = %d, want 2", bc.roundRobinStart["RRStart"])
	}

	// MultiKeep: a later `options { model … }` block must NOT wipe the earlier
	// strategy/start — the descriptor and production both retain [A,B] / start 2.
	mk := strat["MultiKeep"]
	if !reflect.DeepEqual(mk.Children, []string{"A", "B"}) || mk.Start == nil || *mk.Start != 2 {
		t.Errorf("MultiKeep = %+v, want children [A B] start 2 (later options{model} must not wipe)", mk)
	}
	if !reflect.DeepEqual(bc.fallbackChains["MultiKeep"], []string{"A", "B"}) || bc.roundRobinStart["MultiKeep"] != 2 {
		t.Errorf("production MultiKeep chain=%v start=%d, want [A B] / 2", bc.fallbackChains["MultiKeep"], bc.roundRobinStart["MultiKeep"])
	}

	// MultiNew: a later block with a NEW non-empty strategy overwrites the chain,
	// but the earlier start (absent from the new block) is preserved.
	mn := strat["MultiNew"]
	if !reflect.DeepEqual(mn.Children, []string{"C", "D"}) || mn.Start == nil || *mn.Start != 2 {
		t.Errorf("MultiNew = %+v, want children [C D] start 2 (new chain, earlier start kept)", mn)
	}
	if !reflect.DeepEqual(bc.fallbackChains["MultiNew"], []string{"C", "D"}) || bc.roundRobinStart["MultiNew"] != 2 {
		t.Errorf("production MultiNew chain=%v start=%d, want [C D] / 2", bc.fallbackChains["MultiNew"], bc.roundRobinStart["MultiNew"])
	}
}

// TestNativeSpineStrictMissingDir proves strict mode fails on a missing/unreadable
// --baml-src-dir rather than emitting an empty descriptor and exiting 0 (fix P1-1).
func TestNativeSpineStrictMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out := filepath.Join(t.TempDir(), "d.json")
	err := emitNativeSpineDescriptors(&config{BAMLSourceDir: missing, NativeSpineDescriptors: out})
	if err == nil {
		t.Fatal("strict pass on a missing source dir returned nil, want a walk error")
	}
	if !strings.Contains(err.Error(), "strict source diagnostics") {
		t.Errorf("error = %q, want a strict source-diagnostics failure", err.Error())
	}
}
