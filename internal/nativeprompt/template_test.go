package nativeprompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// TestDynamicTemplateDriftGuard parses the real cmd/build/dynamic.baml and
// asserts the prompt body still matches rawDynamicPrompt verbatim. If the
// generated template ever changes, this fails loudly rather than letting the
// parity proof silently render a stale template.
func TestDynamicTemplateDriftGuard(t *testing.T) {
	path := filepath.Join("..", "..", "cmd", "build", "dynamic.baml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	file, err := bamlparser.ParseBytes(path, data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var promptRaw *string
	for _, item := range file.Items {
		if item.Function == nil || item.Function.Name != "Baml_Rest_Dynamic" {
			continue
		}
		// Read the dedicated PromptRaw projection (P1 slice 1, #586) rather than
		// reaching through the generic ordered Fields. PromptRaw is the final
		// source-ordered prompt field's raw body with only the raw-string
		// delimiters removed (no dedent/trim), which is exactly what this drift
		// guard needs.
		promptRaw = item.Function.PromptRaw
	}

	if promptRaw == nil {
		t.Fatal("Baml_Rest_Dynamic prompt (PromptRaw) not found in cmd/build/dynamic.baml")
	}
	if *promptRaw != rawDynamicPrompt {
		t.Errorf("dynamic prompt drift: the checked-in template no longer matches rawDynamicPrompt.\n--- parsed ---\n%q\n--- constant ---\n%q", *promptRaw, rawDynamicPrompt)
	}
}

// TestDedentTrimMatchesBAML pins BAML's dedent+trim preprocessing on a small
// fixture: minimum-leading-whitespace dedent over non-blank lines, then trim.
func TestDedentTrimMatchesBAML(t *testing.T) {
	in := "\n" +
		"    line one\n" +
		"      indented two\n" +
		"    line three\n" +
		"  "
	want := "line one\n  indented two\nline three"
	if got := dedentTrim(in); got != want {
		t.Errorf("dedentTrim:\n got %q\nwant %q", got, want)
	}
}
