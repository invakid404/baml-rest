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
	supported := []string{
		"GetGreeting", "GetSimple", "GetPerson", "GetPersonWithAddress",
		"GetPeople", "GetCategory", "GetComprehensive",
		"DescribeImage", "DescribeImages", "DescribeImageWithCaption",
	}
	for _, name := range supported {
		if _, ok := cfg.staticSchemas[name]; !ok {
			t.Errorf("function %q expected supported, decline=%q", name, cfg.staticSchemaDeclines[name])
		}
	}

	// Declined: @@dynamic class/enum, recursive class, recursive+media, and the
	// recursive JSON alias.
	declined := map[string]string{
		"GetDynamic":     "block attribute",
		"GetDynamicEnum": "block attribute",
		"ParseTree":      "recursive",
		"ParseMediaTree": "recursive",
		"ParseJson":      "recursive type alias",
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
