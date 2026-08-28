package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
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
	// M3a admits a string-keyed map OUTPUT (the native carrier generates it); a map
	// INPUT is still declined (no map in the input-value profile).
	if !admitted["MapOutput"] {
		t.Errorf("MapOutput (string-keyed map output) should be admitted (M3a); declines=%v", declines)
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
		"MapInput":    "unsupported_input_shape",  // input map -> INPUT shape (still declined)
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

// ---------------------------------------------------------------------------
// M3b: real introspect -> classifier -> codegen pipeline coverage (scope §5A).
// These drive the REAL parseBamlSourceDir pipeline (not source-grep / not a
// hand-built descriptor), assert the exact admission/decline map for the M3b
// vocabulary (multi-arm unions, string/int/bool literals, @alias metadata), and
// then EMIT the admitted method, compile it in a temp module, and EXECUTE
// behavioral JSON tests. Descriptor presence alone is not admission.
// ---------------------------------------------------------------------------

// pipelineProject runs the REAL introspect pipeline and returns the whole
// Project (so a test can pull an admitted method and emit it).
func pipelineProject(t *testing.T, sources map[string]string) projectdescriptor.Project {
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
	return proj
}

// admittedMethodByName returns the admitted method of the given name, failing if
// it was not admitted.
func admittedMethodByName(t *testing.T, proj projectdescriptor.Project, name string) projectdescriptor.Method {
	t.Helper()
	for i := range proj.Methods {
		if proj.Methods[i].Name == name {
			return proj.Methods[i]
		}
	}
	t.Fatalf("method %q was not admitted (methods: %v)", name, methodNames(proj))
	return projectdescriptor.Method{}
}

func methodNames(proj projectdescriptor.Project) []string {
	out := make([]string, 0, len(proj.Methods))
	for _, m := range proj.Methods {
		out = append(out, m.Name)
	}
	return out
}

// compileAndRunEmittedCarrier writes the emitted carrier + a behavioral test into
// a throwaway module (bamlutils resolved via a local replace, repo go.sum reused,
// fully offline) and runs `go test`. Mirrors the proven cmd/introspect Gate-A
// temp-module recipe; it respects module boundaries (no adapters/common internal
// testharness import from the root module).
func compileAndRunEmittedCarrier(t *testing.T, packageName, emitted, behavioralTest string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping emitted-carrier compile+execute")
	}
	repoRoot := gateARepoRoot(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "carrier.go"), []byte(emitted), 0o644); err != nil {
		t.Fatalf("write carrier.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "carrier_test.go"), []byte(behavioralTest), 0o644); err != nil {
		t.Fatalf("write carrier_test.go: %v", err)
	}
	const bamlutilsPkg = "github.com/invakid404/baml-rest/bamlutils"
	bamlutilsAbs := filepath.Join(repoRoot, "bamlutils")
	goMod := fmt.Sprintf("module m3bcarrier\n\ngo %s\n\nrequire %s v0.0.0\n\nreplace %s => %s\n",
		gateAGoVersion(t, repoRoot), bamlutilsPkg, bamlutilsPkg, filepath.ToSlash(bamlutilsAbs))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("write go.sum: %v", err)
		}
	}
	// Import-graph gate on the emitted carrier: no baml_client / BAML / CFFI.
	assertEmittedCarrierNoCFFI(t, dir)

	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOWORK=off",
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
	)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emitted carrier behavioral test failed: %v\n%s", err, outBytes)
	}
}

// assertEmittedCarrierNoCFFI runs `go list -deps` on the carrier package (NON-test
// graph) and rejects any baml_client / BAML runtime / CFFI / patched-client path
// (scope §5C).
func assertEmittedCarrierNoCFFI(t *testing.T, dir string) {
	t.Helper()
	forbidden := []string{"baml_client", "github.com/boundaryml/baml", "dynclient/baml-patched", "language_client_go", "cffi"}
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps on emitted carrier: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Errorf("emitted carrier depends on %q (matches forbidden %q)", dep, bad)
			}
		}
	}
}

// TestNativeSpineM3bAdmissionAndDeclines drives the real pipeline and asserts the
// exact M3b admission/decline map: multi-arm unions (cross-kind, class/enum arms,
// nested list/map arms, repeated shapes, nullable), string/int/bool literals and
// same-base literal unions, and aliased fields/enum members (incl. exotic
// aliases) are ADMITTED; recursion, media-under-union, tuple, null-only, dynamic,
// non-string-key map, and unsupported input union/map are DECLINED. One clean
// M3a method proves no global poisoning.
func TestNativeSpineM3bAdmissionAndDeclines(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml": `
enum Suit { Hearts @alias("hearts_alias")
  Spades
}
class Aliased { qty int @alias("amount")
  empt string @alias("")
  comma string @alias("a,b")
  uni string @alias("naïve")
  suit Suit
}
class Holder { pick string | int
  items (string | int)[]
  lut map<string, string | int>
}
class RecA { child RecB }
class RecB { back RecA }
class DynClass { f string
  @@dynamic
}
`,
		"functions.baml": `
function CrossUnion() -> string | int { client GPT4 prompt #"x"# }
function StrLitUnion() -> "a" | "b" { client GPT4 prompt #"x"# }
function IntLitUnion() -> 1 | 2 { client GPT4 prompt #"x"# }
function BoolLitUnion() -> true | false { client GPT4 prompt #"x"# }
function StandaloneStr() -> "active" { client GPT4 prompt #"x"# }
function StandaloneInt() -> 200 { client GPT4 prompt #"x"# }
function NullableUnion() -> (string | int)? { client GPT4 prompt #"x"# }
function EnumClassUnion() -> Suit | Aliased { client GPT4 prompt #"x"# }
function RepeatedUnions() -> Holder { client GPT4 prompt #"x"# }
function AliasProbe() -> Aliased { client GPT4 prompt #"x"# }
function CleanM3a() -> string { client GPT4 prompt #"x"# }

function RecReturn() -> RecA { client GPT4 prompt #"x"# }
function UnionMedia() -> string | image { client GPT4 prompt #"x"# }
function NullReturn() -> null { client GPT4 prompt #"x"# }
function TupleReturn() -> (string, int) { client GPT4 prompt #"x"# }
function DynReturn() -> DynClass { client GPT4 prompt #"x"# }
function IntKeyMap() -> map<int, string> { client GPT4 prompt #"x"# }
function UnionInput(x: string | int) -> string { client GPT4 prompt #"{{ x }}"# }
function MapInput(m: map<string, string>) -> string { client GPT4 prompt #"x"# }
`,
	}
	admitted, declines := pipelineDeclines(t, sources)

	wantAdmitted := []string{
		"CrossUnion", "StrLitUnion", "IntLitUnion", "BoolLitUnion",
		"StandaloneStr", "StandaloneInt", "NullableUnion", "EnumClassUnion",
		"RepeatedUnions", "AliasProbe", "CleanM3a",
	}
	for _, name := range wantAdmitted {
		if !admitted[name] {
			t.Errorf("%s should be admitted (M3b vocabulary); decline=%q", name, declines[name])
		}
	}

	wantDeclines := map[string]string{
		"RecReturn":   "unsupported_output_shape", // recursive class graph
		"UnionMedia":  "unsupported_output_shape", // media child under a union (pre-decline context)
		"NullReturn":  "unsupported_output_shape", // null-only
		"TupleReturn": "unsupported_output_shape", // tuple
		// A @@dynamic RETURN class pre-declines in the static-schema builder, named
		// by reliable OUTPUT context (precise schema_dynamic_class is stamped only
		// where the winning producer knows it — input class / unused enum / macro
		// arg; see TestNativeSpinePreDeclineStructural). Either way, dynamic is declined.
		"DynReturn": "unsupported_output_shape",
		"IntKeyMap": "unsupported_output_shape", // non-string-key map
		"UnionInput":  "unsupported_input_shape",  // union INPUT stays declined (output-only slice)
		"MapInput":    "unsupported_input_shape",  // map input stays declined
	}
	for name, code := range wantDeclines {
		if declines[name] != code {
			t.Errorf("%s decline = %q, want %q", name, declines[name], code)
		}
	}
}

// TestNativeSpineM3bAliasCanonicalPipeline is the scope §2 real-pipeline
// deliverable: a `.baml` class with an aliased int field + an enum with an
// aliased member is admitted, EMITTED, compiled, and executed to prove the
// carrier serves CANONICAL keys/values, an alias is never an output token, an
// alias key does not populate the field on direct unmarshal, and an alias enum
// value is REJECTED on direct unmarshal.
func TestNativeSpineM3bAliasCanonicalPipeline(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml": `
enum Suit { Hearts @alias("hearts_alias")
  Spades
}
class Aliased { qty int @alias("amount")
  empt string @alias("")
  comma string @alias("a,b")
  uni string @alias("naïve")
  suit Suit
}
`,
		"functions.baml": `function AliasProbe() -> Aliased { client GPT4 prompt #"x"# }`,
	}
	proj := pipelineProject(t, sources)
	m := admittedMethodByName(t, proj, "AliasProbe")

	src, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: "carrier"})
	if err != nil {
		t.Fatalf("emit AliasProbe: %v", err)
	}
	got := string(src)
	// Emitted codec keys canonically; no alias string is a codec wire key.
	for _, want := range []string{`{"qty", v.Qty}`, `OutputSuitHearts OutputSuit = "Hearts"`} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted carrier missing canonical token %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{`"amount"`, `"hearts_alias"`, `"naïve"`, `"a,b"`} {
		if strings.Contains(got, bad) {
			t.Errorf("alias %s leaked into emitted carrier source", bad)
		}
	}

	compileAndRunEmittedCarrier(t, "carrier", got, aliasBehavioralTest)
}

const aliasBehavioralTest = `package carrier

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAliasCanonicalBehavior(t *testing.T) {
	v := OutputAliased{Qty: 7, Empt: "E", Comma: "C", Uni: "U", Suit: OutputSuitHearts}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{` + "`\"qty\":7`" + `, ` + "`\"suit\":\"Hearts\"`" + `} {
		if !strings.Contains(s, want) {
			t.Fatalf("canonical token %s missing from %s", want, s)
		}
	}
	for _, bad := range []string{"amount", "hearts_alias"} {
		if strings.Contains(s, bad) {
			t.Fatalf("alias %q leaked into output bytes %s", bad, s)
		}
	}
	// Canonical direct unmarshal populates the fields.
	var back OutputAliased
	if err := json.Unmarshal([]byte(` + "`{\"qty\":9,\"empt\":\"z\",\"comma\":\"c\",\"uni\":\"u\",\"suit\":\"Hearts\"}`" + `), &back); err != nil {
		t.Fatal(err)
	}
	if back.Qty != 9 || back.Suit != OutputSuitHearts {
		t.Fatalf("canonical unmarshal = %+v", back)
	}
	// A class alias key does NOT populate the field (generated struct tags canonical).
	var viaAlias OutputAliased
	if err := json.Unmarshal([]byte(` + "`{\"amount\":9}`" + `), &viaAlias); err != nil {
		t.Fatal(err)
	}
	if viaAlias.Qty != 0 {
		t.Fatalf("alias key \"amount\" populated the canonical field: qty=%d", viaAlias.Qty)
	}
	// An enum ALIAS value is rejected on direct unmarshal; canonical is accepted.
	var suit OutputSuit
	if err := json.Unmarshal([]byte(` + "`\"hearts_alias\"`" + `), &suit); err == nil {
		t.Fatal("enum accepted the alias value \"hearts_alias\"")
	}
	if err := json.Unmarshal([]byte(` + "`\"Spades\"`" + `), &suit); err != nil {
		t.Fatalf("enum rejected canonical \"Spades\": %v", err)
	}
}
`

// TestNativeSpineM3bUnionLiteralPipeline emits a real-pipeline class carrying
// multi-arm unions + standalone literals, compiles it, and executes behavioral
// arm-selection / same-base-ambiguity / no-literal-validation proofs.
func TestNativeSpineM3bUnionLiteralPipeline(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml": `class UBox { cross string | int
  choice "a" | "b"
  flag true | false
  status "active"
  code 200
}
`,
		"functions.baml": `function UnionBox() -> UBox { client GPT4 prompt #"x"# }`,
	}
	proj := pipelineProject(t, sources)
	m := admittedMethodByName(t, proj, "UnionBox")

	src, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: "carrier"})
	if err != nil {
		t.Fatalf("emit UnionBox: %v", err)
	}
	got := string(src)
	for _, want := range []string{"type OutputUnion1 struct", "type OutputUnion2 struct", "type OutputUnion3 struct"} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted carrier missing %q", want)
		}
	}

	compileAndRunEmittedCarrier(t, "carrier", got, unionLiteralBehavioralTest)
}

const unionLiteralBehavioralTest = `package carrier

import (
	"encoding/json"
	"testing"
)

func TestUnionLiteralBehavior(t *testing.T) {
	// Cross-kind arm selection (cross = string | int -> OutputUnion1).
	var cross OutputUnion1
	if err := json.Unmarshal([]byte("7"), &cross); err != nil {
		t.Fatal(err)
	}
	if !cross.IsVariant1() {
		t.Fatalf("JSON 7 should bind the int arm (v1)")
	}
	if err := json.Unmarshal([]byte(` + "`\"hi\"`" + `), &cross); err != nil {
		t.Fatal(err)
	}
	if !cross.IsVariant0() || cross.AsVariant0() == nil || *cross.AsVariant0() != "hi" {
		t.Fatalf("JSON \"hi\" should bind the string arm (v0)")
	}

	// Same-base string-literal ambiguity (choice = "a" | "b" -> OutputUnion2):
	// any string binds the FIRST arm, no value check.
	var choice OutputUnion2
	if err := json.Unmarshal([]byte(` + "`\"b\"`" + `), &choice); err != nil {
		t.Fatal(err)
	}
	if !choice.IsVariant0() {
		t.Fatalf("string \"b\" should bind the FIRST literal arm (v0)")
	}

	// Same-base bool-literal ambiguity (flag = true | false -> OutputUnion3):
	// JSON false binds the FIRST (true-declared) arm.
	var flag OutputUnion3
	if err := json.Unmarshal([]byte("false"), &flag); err != nil {
		t.Fatal(err)
	}
	if !flag.IsVariant0() || flag.AsVariant0() == nil || *flag.AsVariant0() != false {
		t.Fatalf("JSON false should bind the FIRST bool arm (v0) holding false")
	}

	// Unset union marshal errors; a constructed arm marshals to its bare value.
	var unset OutputUnion1
	if _, err := json.Marshal(unset); err == nil {
		t.Fatal("unset union marshaled without error")
	}
	if b, err := json.Marshal(OutputUnion1NewVariant0("z")); err != nil || string(b) != ` + "`\"z\"`" + ` {
		t.Fatalf("constructor marshal = %s, %v", b, err)
	}

	// Standalone literals are plain bases with NO value validation.
	var box OutputUBox
	if err := json.Unmarshal([]byte(` + "`{\"cross\":1,\"choice\":\"a\",\"flag\":true,\"status\":\"anything\",\"code\":999}`" + `), &box); err != nil {
		t.Fatalf("standalone literal rejected an out-of-literal value: %v", err)
	}
	if box.Status != "anything" || box.Code != 999 {
		t.Fatalf("standalone literals not plain bases: status=%q code=%d", box.Status, box.Code)
	}
}
`

// TestNativeSpineM3bEmitsAdmittedUnionShapes emits (no compile) the admitted
// union shapes that the behavioral tests do not exercise directly — an enum|class
// union arm, and a union reachable via a class field, a list element, AND a map
// value that all DEDUPE to one carrier — proving the plan resolves every reach
// site and emission succeeds for the full admitted vocabulary.
func TestNativeSpineM3bEmitsAdmittedUnionShapes(t *testing.T) {
	sources := map[string]string{
		"clients.baml": goodClient,
		"types.baml": `
enum Suit { Hearts
  Spades
}
class Aliased { qty int @alias("amount")
  suit Suit
}
class Holder { pick string | int
  items (string | int)[]
  lut map<string, string | int>
}
`,
		"functions.baml": `
function EnumClassUnion() -> Suit | Aliased { client GPT4 prompt #"x"# }
function RepeatedUnions() -> Holder { client GPT4 prompt #"x"# }
`,
	}
	proj := pipelineProject(t, sources)

	// enum|class union at the target: emits OutputUnion1 with enum + class arms.
	ec := admittedMethodByName(t, proj, "EnumClassUnion")
	ecSrc, err := codegen.EmitNativeStaticUnary(ec, codegen.NativeSpineOptions{PackageName: "carrier"})
	if err != nil {
		t.Fatalf("emit EnumClassUnion: %v", err)
	}
	for _, want := range []string{"type OutputUnion1 struct", "type OutputSuit string", "type OutputAliased struct"} {
		if !strings.Contains(string(ecSrc), want) {
			t.Errorf("EnumClassUnion carrier missing %q", want)
		}
	}

	// One union shape reached via a class field, a list element, and a map value —
	// all must DEDUPE to a single OutputUnion1 (no OutputUnion2).
	ru := admittedMethodByName(t, proj, "RepeatedUnions")
	ruSrc, err := codegen.EmitNativeStaticUnary(ru, codegen.NativeSpineOptions{PackageName: "carrier"})
	if err != nil {
		t.Fatalf("emit RepeatedUnions: %v", err)
	}
	s := string(ruSrc)
	if !strings.Contains(s, "type OutputUnion1 struct") {
		t.Error("RepeatedUnions carrier missing OutputUnion1")
	}
	if strings.Contains(s, "type OutputUnion2 struct") {
		t.Error("union dedupe failed: field/list/map reaches of one shape emitted more than one carrier")
	}
	if !strings.Contains(s, "[]OutputUnion1") || !strings.Contains(s, "map[string]OutputUnion1") {
		t.Errorf("RepeatedUnions carrier missing list/map union bindings:\n%s", s)
	}
}
