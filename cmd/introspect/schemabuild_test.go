package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// TestBuildStaticSchemasIntegrationCorpus runs the full production
// parseBamlSourceDir pipeline over the real integration/testdata/baml_src
// fixtures and asserts the expected supported/declined split, proving the
// native static-schema builder is wired end-to-end on the production introspect
// path (parseBamlSourceDir -> nativeschema.BuildStaticSchemas -> cfg.staticSchemas).
// The builder's own unit corpus lives with the code in
// internal/nativeschema/build_test.go.
func TestBuildStaticSchemasIntegrationCorpus(t *testing.T) {
	dir := filepath.Join("..", "..", "integration", "testdata", "baml_src")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("integration corpus not present at %s: %v", dir, err)
	}

	cfg := parseBamlSourceDir(dir)

	// Supported: plain classes/enums/lists, plus media-INPUT functions whose
	// OUTPUT is a bare string (media is an input, not part of the output graph).
	// Recursion is now lowered (slice 5): a recursive class (ParseTree/TreeNode)
	// and a structural recursive alias reached via a class field
	// (ParseJson/JsonContainer -> JsonValue) both build.
	supported := []string{
		"GetGreeting", "GetSimple", "GetPerson", "GetPersonWithAddress",
		"GetPeople", "GetCategory", "GetComprehensive",
		"DescribeImage", "DescribeImages", "DescribeImageWithCaption",
		"ParseTree", "ParseJson",
	}
	for _, name := range supported {
		if _, ok := cfg.staticSchemas[name]; !ok {
			t.Errorf("function %q expected supported, decline=%q", name, cfg.staticSchemaDeclines[name])
		}
	}

	// Declined: @@dynamic class/enum, and a recursive class whose output graph
	// reaches MEDIA (MediaTreeNode) — a legal recursive class, but media output
	// is rejected by ValidateOutput.
	declined := map[string]string{
		"GetDynamic":     "block attribute",
		"GetDynamicEnum": "block attribute",
		"ParseMediaTree": "media is not usable as an output type",
	}
	for name, sub := range declined {
		if _, ok := cfg.staticSchemas[name]; ok {
			t.Errorf("function %q expected declined, but a descriptor was built", name)
		}
		reason, ok := cfg.staticSchemaDeclines[name]
		if !ok {
			t.Errorf("function %q expected a decline reason, got none", name)
			continue
		}
		if !strings.Contains(reason, sub) {
			t.Errorf("function %q decline reason %q does not contain %q", name, reason, sub)
		}
	}
}

// TestBuildPromptDescriptorsIntegrationCorpus runs the full production
// parseBamlSourceDir pipeline over the real integration/testdata/baml_src
// fixtures and asserts the native PROMPT descriptor sidecar is wired end-to-end
// (parseBamlSourceDir -> BuildStaticSchemas + enrichShorthandClientProviders ->
// BuildPromptDescriptors -> cfg.staticPromptDescriptors/staticPromptDeclines).
// The builder's own unit corpus lives with the code in
// internal/nativeschema/prompt_test.go; this is a wiring + invariants check.
func TestBuildPromptDescriptorsIntegrationCorpus(t *testing.T) {
	dir := filepath.Join("..", "..", "integration", "testdata", "baml_src")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("integration corpus not present at %s: %v", dir, err)
	}

	cfg := parseBamlSourceDir(dir)

	if cfg.staticPromptDescriptors == nil || cfg.staticPromptDeclines == nil {
		t.Fatalf("prompt sidecar maps must be non-nil: descriptors=%v declines=%v",
			cfg.staticPromptDescriptors == nil, cfg.staticPromptDeclines == nil)
	}

	// Mutual exclusivity: a method is a descriptor OR a decline, never both —
	// exactly like staticSchemas vs staticSchemaDeclines.
	for name := range cfg.staticPromptDescriptors {
		if _, both := cfg.staticPromptDeclines[name]; both {
			t.Errorf("function %q appears in BOTH staticPromptDescriptors and staticPromptDeclines", name)
		}
	}

	// De-BAML Slice 7.1b: this corpus declares `enum DynamicCategory { ... @@dynamic }`.
	// BAML installs one Jinja namespace global per PROJECT enum, and it builds
	// that IR with the request's type_builder overlay applied — so a @@dynamic
	// enum's member set is NOT a build-time fact. A V3 descriptor may not describe
	// a PARTIAL enum environment (that would be a render context BAML never has),
	// so the whole project declines V3 and therefore has NO prompt descriptor at
	// all. Every function here is a #583 ledger entry, not an accepted permanent
	// fallback: removing @@dynamic from the corpus (or proving dynamic enums)
	// restores descriptors.
	//
	// This asserts the GLOBAL shape deliberately — a regression that silently
	// re-admitted a partial enum universe would otherwise pass unnoticed.
	if len(cfg.staticPromptDescriptors) != 0 {
		t.Errorf("expected zero prompt descriptors while the corpus declares a @@dynamic enum, got %d", len(cfg.staticPromptDescriptors))
	}
	for _, name := range []string{"GetGreeting", "GetSimple", "GetCategory"} {
		reason, ok := cfg.staticPromptDeclines[name]
		if !ok {
			t.Errorf("%s expected a V3 input-value decline, got none", name)
			continue
		}
		if !strings.Contains(reason, "input value graph cannot be resolved faithfully") ||
			!strings.Contains(reason, "DynamicCategory") {
			t.Errorf("%s decline reason %q should name the unresolvable @@dynamic project enum", name, reason)
		}
	}

	// The @@dynamic-OUTPUT function is declined for the same global reason now;
	// its own return-bundle decline (a) is proven independently by the static
	// schema half above and by internal/nativeschema/prompt_test.go.
	if _, ok := cfg.staticPromptDescriptors["GetDynamic"]; ok {
		t.Errorf("GetDynamic (@@dynamic output) should be declined, not a descriptor")
	}
}

// TestBuildPromptDescriptorsPositiveInvariants exercises the PER-DESCRIPTOR
// invariants through the same production parseBamlSourceDir pipeline.
//
// It exists because the integration corpus above declares a @@dynamic enum and
// therefore correctly yields ZERO descriptors — which makes every per-descriptor
// loop vacuous there. Those invariants (key == Method, resolved Client/Provider,
// Return == the exact static bundle, and the descriptor/decline partition) are
// real contracts, so they are asserted here against a small NON-dynamic project
// that genuinely produces descriptors. Both halves are needed: the corpus test
// pins the global decline, this one pins the positive shape.
func TestBuildPromptDescriptorsPositiveInvariants(t *testing.T) {
	dir := t.TempDir()
	// A deliberately ordinary project: a named client, an aliased enum, a nested
	// input class, and one function per admitted argument shape — plus ONE
	// function the builder must decline (a map argument), so the partition and
	// the decline ledger are both non-empty.
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("clients.baml", `client<llm> PositiveClient {
  provider openai
  options { model "m" api_key "k" }
}
`)
	write("types.baml", `enum Hue {
  RED @alias("rouge")
  BLUE
}

class Swatch {
  hue Hue
  label string @alias("etiquette")
}
`)
	write("functions.baml", `function PlainText(topic: string) -> string {
  client PositiveClient
  prompt #"About {{ topic }}"#
}

function EnumArg(hue: Hue) -> string {
  client PositiveClient
  prompt #"{{ hue }}"#
}

function ClassArg(s: Swatch) -> string {
  client PositiveClient
  prompt #"{{ s }}"#
}

function MapArg(m: map<string, string>) -> string {
  client PositiveClient
  prompt #"{{ m }}"#
}
`)

	cfg := parseBamlSourceDir(dir)

	wantDescriptors := []string{"PlainText", "EnumArg", "ClassArg"}
	if len(cfg.staticPromptDescriptors) != len(wantDescriptors) {
		t.Fatalf("built %d descriptors (%v), want exactly %v",
			len(cfg.staticPromptDescriptors), descriptorNames(cfg), wantDescriptors)
	}

	for _, name := range wantDescriptors {
		d, ok := cfg.staticPromptDescriptors[name]
		if !ok {
			t.Errorf("%s has no descriptor (decline: %q)", name, cfg.staticPromptDeclines[name])
			continue
		}
		if d.Method != name {
			t.Errorf("descriptor keyed %q has Method %q", name, d.Method)
		}
		if d.Version != promptdescriptor.Version {
			t.Errorf("descriptor %q version = %d, want %d", name, d.Version, promptdescriptor.Version)
		}
		if d.Client != "PositiveClient" || d.Provider != "openai" {
			t.Errorf("descriptor %q Client/Provider = %q/%q, want PositiveClient/openai", name, d.Client, d.Provider)
		}
		if !reflect.DeepEqual(d.Return, cfg.staticSchemas[name]) {
			t.Errorf("descriptor %q Return does not equal the static bundle", name)
		}
		// V3: the project enum set is installed WHOLE on every function, and every
		// argument carries a resolved value type.
		if len(d.InputValues.ProjectEnums) != 1 || d.InputValues.ProjectEnums[0].Name != "Hue" {
			t.Errorf("descriptor %q project enums = %+v, want exactly the source's Hue", name, d.InputValues.ProjectEnums)
		}
		for _, a := range d.Args {
			if a.ValueType == nil {
				t.Errorf("descriptor %q argument %q has no V3 ValueType", name, a.Name)
			}
		}
		if _, both := cfg.staticPromptDeclines[name]; both {
			t.Errorf("%s appears in BOTH descriptors and declines", name)
		}
	}

	// The class closure really is resolved from source (aliases + field order),
	// so the positive assertions above are about real content.
	class := cfg.staticPromptDescriptors["ClassArg"].InputValues.Classes
	if len(class) != 1 || class[0].Name != "Swatch" || len(class[0].Fields) != 2 ||
		class[0].Fields[0].Canonical != "hue" || class[0].Fields[1].Canonical != "label" ||
		class[0].Fields[1].Alias == nil || *class[0].Fields[1].Alias != "etiquette" {
		t.Errorf("ClassArg class closure = %+v, want the source-resolved Swatch", class)
	}

	// The partition is non-vacuous in BOTH directions: the map argument declines.
	reason, ok := cfg.staticPromptDeclines["MapArg"]
	if !ok {
		t.Fatal("MapArg expected a decline (map is not a V3 value node), got none")
	}
	if !strings.Contains(reason, "map types are not supported") {
		t.Errorf("MapArg decline reason %q should name the unsupported map", reason)
	}
	if _, ok := cfg.staticPromptDescriptors["MapArg"]; ok {
		t.Error("MapArg must not have a descriptor")
	}
}

// descriptorNames returns the built descriptor method names, sorted, for
// diagnostics only.
func descriptorNames(cfg *bamlConfig) []string {
	out := make([]string, 0, len(cfg.staticPromptDescriptors))
	for k := range cfg.staticPromptDescriptors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
