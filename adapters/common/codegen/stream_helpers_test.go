package codegen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common"
)

// TestGenerateStreamHelpers_ParsesAsValidGo emits the orchestration helpers
// (runNoRawOrchestration / runFullOrchestration / runLegacyChildStream) into
// a synthetic file and confirms the output parses as valid Go syntax. The
// adapter package is generated at user build time against a real baml_client,
// so this is the closest in-repo guard against codegen syntax regressions.
func TestGenerateStreamHelpers_ParsesAsValidGo(t *testing.T) {
	t.Parallel()

	out := common.MakeFile()
	generateStreamHelpers(out)

	rendered := out.GoString()
	if rendered == "" {
		t.Fatal("generateStreamHelpers produced empty output")
	}

	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "stream_helpers_synth.go", rendered, parser.AllErrors); err != nil {
		// Print a few lines of context around the failure to make the error
		// readable; jennifer output can be long.
		t.Fatalf("generated stream helpers do not parse as valid Go: %v\n--- snippet ---\n%s", err, snippet(rendered, 3000))
	}
}

// TestGenerateStreamHelpers_ContainsExpectedSymbols asserts the new
// orchestration contract's call sites appear in the generated source. This
// is a low-cost guard against accidentally dropping the outcome-emission
// changes from runNoRaw / runFull during future refactors.
func TestGenerateStreamHelpers_ContainsExpectedSymbols(t *testing.T) {
	t.Parallel()

	out := common.MakeFile()
	generateStreamHelpers(out)
	rendered := out.GoString()

	wantSubstrings := []string{
		"runNoRawOrchestration",
		"runFullOrchestration",
		"BuildLegacyOutcome",
		"plannedMetadata",
		"newMetadataResult",
		"beforeFinal",
		"lastFuncLog",
		"SelectedCall",
		"startTime",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(rendered, want) {
			t.Errorf("generated source missing expected symbol %q", want)
		}
	}
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
