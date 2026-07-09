package main

import (
	"os"
	"path/filepath"
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
