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
		// Planned metadata must be emitted upfront from the goroutine so
		// it does not depend on BAML firing onTick.
		"emitPlanned",
		"plannedSent",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(rendered, want) {
			t.Errorf("generated source missing expected symbol %q", want)
		}
	}

	// Structural check: emitPlanned must be invoked inside both
	// run*Orchestration goroutines BEFORE body() runs. The upfront
	// emission is the whole point — gating planned metadata on onTick
	// lost the event whenever BAML completed without firing it.
	// We assert the textual ordering "emitPlanned()" appears before
	// "body(beforeFinal, onTick)" in each function. Brittle to minor
	// codegen reformatting but cheap; adjust if the rendering changes.
	for _, fn := range []string{"runNoRawOrchestration", "runFullOrchestration"} {
		fnStart := strings.Index(rendered, "func "+fn+"(")
		if fnStart < 0 {
			t.Errorf("could not locate %s() in generated source", fn)
			continue
		}
		// Bound the search to this function's body. A naive `}` won't
		// work because of nested closures; instead search forward for
		// the next top-level `func ` after the start, treating that as
		// the function's end. Sufficient for a contains-order check.
		nextFn := strings.Index(rendered[fnStart+len("func "+fn+"("):], "\nfunc ")
		end := len(rendered)
		if nextFn >= 0 {
			end = fnStart + len("func "+fn+"(") + nextFn
		}
		body := rendered[fnStart:end]
		emitIdx := strings.Index(body, "emitPlanned()")
		bodyCallIdx := strings.Index(body, "body(beforeFinal, onTick)")
		if emitIdx < 0 {
			t.Errorf("%s: missing emitPlanned() call site", fn)
			continue
		}
		if bodyCallIdx < 0 {
			// runFullOrchestration uses driveStream(opts), not body().
			// Try that instead.
			bodyCallIdx = strings.Index(body, "driveStream(opts)")
		}
		if bodyCallIdx < 0 {
			t.Errorf("%s: missing body/driveStream call site", fn)
			continue
		}
		if emitIdx >= bodyCallIdx {
			t.Errorf("%s: emitPlanned() must be called before body/driveStream; got emit@%d, call@%d", fn, emitIdx, bodyCallIdx)
		}
	}
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
