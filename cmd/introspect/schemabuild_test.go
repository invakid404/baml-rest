package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

	// Per-descriptor invariants: the Return bundle is the EXACT static bundle,
	// the method name matches its key, and Client/Provider are resolved.
	for name, d := range cfg.staticPromptDescriptors {
		if d.Method != name {
			t.Errorf("descriptor keyed %q has Method %q", name, d.Method)
		}
		if d.Client == "" || d.Provider == "" {
			t.Errorf("descriptor %q has empty Client %q / Provider %q", name, d.Client, d.Provider)
		}
		if !reflect.DeepEqual(d.Return, cfg.staticSchemas[name]) {
			t.Errorf("descriptor %q Return does not equal the static bundle", name)
		}
	}

	// A known-eligible function (string output, named client) builds; a known
	// @@dynamic-output function inherits the static return-bundle decline (a).
	if _, ok := cfg.staticPromptDescriptors["GetGreeting"]; !ok {
		t.Errorf("GetGreeting expected an eligible prompt descriptor, decline=%q",
			cfg.staticPromptDeclines["GetGreeting"])
	}
	if _, ok := cfg.staticPromptDescriptors["GetDynamic"]; ok {
		t.Errorf("GetDynamic (@@dynamic output) should be declined, not a descriptor")
	}
	if reason := cfg.staticPromptDeclines["GetDynamic"]; !strings.Contains(reason, "return bundle unavailable") {
		t.Errorf("GetDynamic decline reason %q should mention the unavailable return bundle", reason)
	}
}
