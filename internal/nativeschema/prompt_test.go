package nativeschema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// promptFile is one named .baml source for a multi-file prompt-descriptor test.
type promptFile struct {
	path string
	body string
}

// buildPrompts parses files in order, runs the static-schema builder, then the
// prompt descriptor builder with the given resolved clientProvider map. It
// returns the descriptors, the declines, and the static schemas (so a test can
// assert Return equals the exact static bundle).
func buildPrompts(t *testing.T, clientProvider map[string]string, files ...promptFile) (
	map[string]promptdescriptor.Function, map[string]string, map[string]sd.Bundle,
) {
	t.Helper()
	var sfs []SourceFile
	for _, pf := range files {
		f, err := bamlparser.ParseString(pf.path, pf.body)
		if err != nil {
			t.Fatalf("parse %s: %v", pf.path, err)
		}
		sfs = append(sfs, SourceFile{File: f, Path: pf.path})
	}
	schemas, schemaDeclines := BuildStaticSchemas(sfs)
	descs, declines := BuildPromptDescriptors(sfs, schemas, schemaDeclines, clientProvider)
	return descs, declines, schemas
}

// macroNames projects a macro slice onto its ordered names for order assertions.
func macroNames(ms []promptdescriptor.TemplateString) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// assertDeclined asserts method has a decline (with reason substring sub) and no
// descriptor.
func assertDeclined(t *testing.T, descs map[string]promptdescriptor.Function, declines map[string]string, method, sub string) {
	t.Helper()
	if _, ok := descs[method]; ok {
		t.Errorf("%s: expected decline, but a descriptor was built", method)
	}
	reason, ok := declines[method]
	if !ok {
		t.Fatalf("%s: expected a decline reason, got none", method)
	}
	if !strings.Contains(reason, sub) {
		t.Errorf("%s: decline reason %q does not contain %q", method, reason, sub)
	}
}

// TestBuildPromptDescriptorsComplete proves the happy path: a small project with
// a named client, a shorthand client, a supported return type, two ordered
// macros across two files, and two eligible functions. It asserts every
// descriptor field is complete, macro order is SourceFile-then-File.Items (never
// lexical), and Return is the EXACT static bundle.
func TestBuildPromptDescriptorsComplete(t *testing.T) {
	fileA := promptFile{path: "a.baml", body: `
class Person {
    name string
    age int
}

template_string GreetHeader(name: string) #"Hello {{ name }}!"#

template_string Footer() #"-- sent by baml-rest --"#

function GetPerson(who: string) -> Person {
    client MyClient
    prompt #"{{ GreetHeader(who) }}
Return a person.
{{ ctx.output_format }}"#
}
`}
	fileB := promptFile{path: "b.baml", body: `
template_string Signature(x: int) #"sig-{{ x }}"#

function GetName(id: int) -> string {
    client "openai/gpt-4o"
    prompt #"Give me a name for id {{ id }}"#
}
`}

	cp := map[string]string{"MyClient": "anthropic", "openai/gpt-4o": "openai"}
	descs, declines, schemas := buildPrompts(t, cp, fileA, fileB)

	if len(declines) != 0 {
		t.Fatalf("expected no declines, got %v", declines)
	}

	// Macro order is source-file order then within-file item order:
	// GreetHeader, Footer (a.baml), then Signature (b.baml). Lexical order would
	// be Footer, GreetHeader, Signature — so this order proves non-lexical.
	wantMacros := []string{"GreetHeader", "Footer", "Signature"}
	wantPaths := map[string]string{"GreetHeader": "a.baml", "Footer": "a.baml", "Signature": "b.baml"}

	gp, ok := descs["GetPerson"]
	if !ok {
		t.Fatalf("GetPerson expected a descriptor; declines=%v", declines)
	}
	if gp.Version != promptdescriptor.Version {
		t.Errorf("GetPerson Version = %d, want %d", gp.Version, promptdescriptor.Version)
	}
	if gp.Method != "GetPerson" {
		t.Errorf("GetPerson Method = %q", gp.Method)
	}
	if gp.Client != "MyClient" {
		t.Errorf("GetPerson Client = %q, want MyClient", gp.Client)
	}
	if gp.Provider != "anthropic" {
		t.Errorf("GetPerson Provider = %q, want anthropic", gp.Provider)
	}
	wantPrompt := "{{ GreetHeader(who) }}\nReturn a person.\n{{ ctx.output_format }}"
	if gp.Prompt != wantPrompt {
		t.Errorf("GetPerson Prompt = %q, want %q", gp.Prompt, wantPrompt)
	}
	if len(gp.Args) != 1 || gp.Args[0].Name != "who" || gp.Args[0].Type == nil ||
		gp.Args[0].Type.Kind != bamlparser.KindPrimitive || gp.Args[0].Type.Primitive != "string" {
		t.Errorf("GetPerson Args = %+v, want [{who string}]", gp.Args)
	}
	if got := macroNames(gp.Macros); !reflect.DeepEqual(got, wantMacros) {
		t.Errorf("GetPerson macro order = %v, want %v", got, wantMacros)
	}
	for _, m := range gp.Macros {
		if wantPaths[m.Name] != m.SourcePath {
			t.Errorf("macro %q SourcePath = %q, want %q", m.Name, m.SourcePath, wantPaths[m.Name])
		}
	}
	// Return is the EXACT ordered static bundle for the method.
	if !reflect.DeepEqual(gp.Return, schemas["GetPerson"]) {
		t.Errorf("GetPerson Return does not equal the static bundle")
	}
	// Spot-check the macro bodies are the raw delimiter-stripped source.
	if gp.Macros[0].Body != "Hello {{ name }}!" {
		t.Errorf("GreetHeader body = %q", gp.Macros[0].Body)
	}
	if len(gp.Macros[0].Args) != 1 || gp.Macros[0].Args[0].Name != "name" {
		t.Errorf("GreetHeader args = %+v", gp.Macros[0].Args)
	}

	gn, ok := descs["GetName"]
	if !ok {
		t.Fatalf("GetName expected a descriptor; declines=%v", declines)
	}
	if gn.Client != "openai/gpt-4o" || gn.Provider != "openai" {
		t.Errorf("GetName Client/Provider = %q/%q, want openai/gpt-4o / openai", gn.Client, gn.Provider)
	}
	if len(gn.Args) != 1 || gn.Args[0].Name != "id" || gn.Args[0].Type == nil ||
		gn.Args[0].Type.Primitive != "int" {
		t.Errorf("GetName Args = %+v, want [{id int}]", gn.Args)
	}
	if got := macroNames(gn.Macros); !reflect.DeepEqual(got, wantMacros) {
		t.Errorf("GetName macro order = %v, want %v (every eligible function carries the whole macro set)", got, wantMacros)
	}
	if !reflect.DeepEqual(gn.Return, schemas["GetName"]) {
		t.Errorf("GetName Return does not equal the static bundle")
	}
}

// TestBuildPromptDescriptorsDeclineReturnBundle covers decline (a): a function
// whose return type static-schema declines (an @@dynamic output) inherits the
// decline under a prompt-descriptor prefix.
func TestBuildPromptDescriptorsDeclineReturnBundle(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Dyn {
    @@dynamic
}

function GetDyn(x: string) -> Dyn {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, schemas := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	if _, ok := schemas["GetDyn"]; ok {
		t.Fatalf("precondition: GetDyn should be static-declined, but a bundle exists")
	}
	assertDeclined(t, descs, declines, "GetDyn", "return bundle unavailable")
}

// TestBuildPromptDescriptorsDeclineShape covers decline (b) in three sub-shapes:
// an unexpected function field, a non-raw final prompt, and a client that does
// not resolve to a provider.
func TestBuildPromptDescriptorsDeclineShape(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

function ExtraField(x: string) -> Person {
    client MyClient
    prompt #"p"#
    temperature 0.7
}

function NonRawPrompt(x: string) -> Person {
    client MyClient
    prompt "not a raw string"
}

function UnknownClient(x: string) -> Person {
    client NotRegistered
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "ExtraField", "no usable LLM function shape")
	assertDeclined(t, descs, declines, "NonRawPrompt", "no usable LLM function shape")
	assertDeclined(t, descs, declines, "UnknownClient", "no usable LLM function shape")
	// The unexpected-field and non-raw-prompt reasons are specific.
	if !strings.Contains(declines["ExtraField"], "other than client/prompt") {
		t.Errorf("ExtraField reason = %q", declines["ExtraField"])
	}
	if !strings.Contains(declines["UnknownClient"], "does not resolve to a provider") {
		t.Errorf("UnknownClient reason = %q", declines["UnknownClient"])
	}
}

// TestBuildPromptDescriptorsDeclineSkipInput covers decline (c): a @skip
// reachable from a function INPUT type declines, even though @skip on an OUTPUT
// field is DROPPED by the static-schema builder (D11).
func TestBuildPromptDescriptorsDeclineSkipInput(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

class SkipInput {
    keep string
    drop string @skip
}

function SkipFn(inp: SkipInput) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "SkipFn", "@skip")
}

// TestBuildPromptDescriptorsDeclineSkipReturnStricter proves the prompt scan is
// STRICTER than the static-schema builder for @skip on the RETURN type: the
// static builder DROPS the skipped output field (so a bundle exists), but the
// prompt descriptor still declines because native prompt rendering has not
// proven BAML's skip semantics.
func TestBuildPromptDescriptorsDeclineSkipReturnStricter(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class SkipOut {
    keep string
    drop string @skip
}

function SkipReturn(x: string) -> SkipOut {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, schemas := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	if _, ok := schemas["SkipReturn"]; !ok {
		t.Fatalf("precondition: static builder should DROP @skip and SUPPORT SkipReturn, but no bundle exists")
	}
	assertDeclined(t, descs, declines, "SkipReturn", "@skip")
	if !strings.Contains(declines["SkipReturn"], "return type") {
		t.Errorf("SkipReturn decline should name the return root: %q", declines["SkipReturn"])
	}
}

// TestBuildPromptDescriptorsDeclineDynamicInput covers decline (d): @@dynamic
// reachable from a function INPUT type declines.
func TestBuildPromptDescriptorsDeclineDynamicInput(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

class DynInput {
    @@dynamic
}

function DynFn(inp: DynInput) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "DynFn", "@@dynamic")
}

// TestBuildPromptDescriptorsDeclineUnresolvableInput covers decline (e): an
// input type graph that cannot be resolved faithfully (an unresolved name).
func TestBuildPromptDescriptorsDeclineUnresolvableInput(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

function UnresolvedFn(inp: NoSuchType) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "UnresolvedFn", "cannot be resolved faithfully")
}

// TestBuildPromptDescriptorsDeclineBadMacroBody covers decline (f): a brace-
// tolerated (non-raw) template body poisons every function (global decline),
// because BAML injects every template string into every prompt.
func TestBuildPromptDescriptorsDeclineBadMacroBody(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

template_string Braced() { some brace body }

function AnyFn(x: string) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "AnyFn", "template string")
	if !strings.Contains(declines["AnyFn"], "raw body") {
		t.Errorf("AnyFn reason = %q, want a raw-body reason", declines["AnyFn"])
	}
}

// TestBuildPromptDescriptorsDeclineDuplicateMacro covers decline (f): duplicate
// template string names are a global decline.
func TestBuildPromptDescriptorsDeclineDuplicateMacro(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

template_string Dup(x: string) #"a"#
template_string Dup(y: string) #"b"#

function AnyFn(x: string) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "AnyFn", "duplicate template string name")
}

// TestBuildPromptDescriptorsDeclineDoesNotRemoveEligible proves a declined
// method does not remove an unrelated eligible method (per-function fail-closed).
func TestBuildPromptDescriptorsDeclineDoesNotRemoveEligible(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

class Dyn {
    @@dynamic
}

function Declined(x: string) -> Dyn {
    client MyClient
    prompt #"p"#
}

function Eligible(x: string) -> Person {
    client MyClient
    prompt #"p"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)
	assertDeclined(t, descs, declines, "Declined", "return bundle unavailable")
	if _, ok := descs["Eligible"]; !ok {
		t.Errorf("Eligible should still have a descriptor despite Declined's decline; declines=%v", declines)
	}
	if _, ok := declines["Eligible"]; ok {
		t.Errorf("Eligible should NOT be in declines")
	}
}

// TestBuildPromptDescriptorsDuplicateFunctionLastWins proves duplicate function
// names are last-wins in BOTH directions (a later decline supersedes an earlier
// descriptor, and a later descriptor supersedes an earlier decline).
func TestBuildPromptDescriptorsDuplicateFunctionLastWins(t *testing.T) {
	src := promptFile{path: "a.baml", body: `
class Person {
    name string
}

function EligibleThenDeclined(x: string) -> Person {
    client MyClient
    prompt #"first"#
}

function EligibleThenDeclined(x: string) -> Person {
    client MyClient
    prompt "not raw"
}

function DeclinedThenEligible(x: string) -> Person {
    client MyClient
    prompt "not raw"
}

function DeclinedThenEligible(x: string) -> Person {
    client MyClient
    prompt #"second"#
}
`}
	descs, declines, _ := buildPrompts(t, map[string]string{"MyClient": "anthropic"}, src)

	if _, ok := descs["EligibleThenDeclined"]; ok {
		t.Errorf("EligibleThenDeclined: later decline should win, but a descriptor exists")
	}
	if _, ok := declines["EligibleThenDeclined"]; !ok {
		t.Errorf("EligibleThenDeclined: expected a decline (last-wins)")
	}

	d, ok := descs["DeclinedThenEligible"]
	if !ok {
		t.Fatalf("DeclinedThenEligible: later descriptor should win; declines=%v", declines)
	}
	if d.Prompt != "second" {
		t.Errorf("DeclinedThenEligible Prompt = %q, want the LAST declaration's body", d.Prompt)
	}
	if _, ok := declines["DeclinedThenEligible"]; ok {
		t.Errorf("DeclinedThenEligible: should NOT remain in declines after last-wins")
	}
}

// TestBuildPromptDescriptorsNonNilMaps proves the builder always returns non-nil
// maps, even with no input.
func TestBuildPromptDescriptorsNonNilMaps(t *testing.T) {
	descs, declines := BuildPromptDescriptors(nil, nil, nil, nil)
	if descs == nil || declines == nil {
		t.Fatalf("BuildPromptDescriptors must return non-nil maps, got descs=%v declines=%v", descs, declines)
	}
}
