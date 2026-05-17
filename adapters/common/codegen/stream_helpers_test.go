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
	generateStreamHelpers(out, DefaultPackageConfig())

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
	generateStreamHelpers(out, DefaultPackageConfig())
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

// TestRunFullOrchestration_NewErrorCarriesRaw pins the codegen
// contract behind #256's details.raw forwarding on the legacy path:
// runFullOrchestration's newError parameter widens to (error, string)
// so the per-method closure (in codegen_legacy_stream.go) can stamp the
// accumulator's raw text onto the error StreamResult.
// The contract has three structural assertions:
//
//  1. The newError parameter type appears with the widened signature.
//  2. Both error-emit sites in the stream-drain goroutine (fatalErrCopy,
//     lastErr) thread the accumulator-derived raw via computeErrorRaw().
//  3. The panic-recovery path passes "" (defensive: the extractor mutex
//     may be held by the panicking goroutine — see the doc comment at
//     the emit site).
//
// All three are textual checks against the rendered source. Brittle to
// minor rewrites but cheap; the alternative (parse + AST inspection)
// is over-engineered for a contract this narrow.
func TestRunFullOrchestration_NewErrorCarriesRaw(t *testing.T) {
	t.Parallel()

	out := common.MakeFile()
	generateStreamHelpers(out, DefaultPackageConfig())
	rendered := out.GoString()

	// 1. Widened signature on runFullOrchestration. Use a substring that
	// only matches the full path's newError (the noRaw path keeps the
	// old func(error) shape).
	if !strings.Contains(rendered, "newError func(error, string) bamlutils.StreamResult") {
		t.Errorf("runFullOrchestration must accept newError func(error, string) bamlutils.StreamResult; rendered:\n%s",
			snippet(rendered, 4000))
	}

	// 2. Both terminal error sites thread the accumulator via
	// computeErrorRaw(). Search bounded to runFullOrchestration so a
	// future addition of a newError call site in runNoRawOrchestration
	// (which keeps the single-arg signature) cannot satisfy these
	// assertions by accident.
	fnStart := strings.Index(rendered, "func runFullOrchestration(")
	if fnStart < 0 {
		t.Fatal("could not locate runFullOrchestration in generated source")
	}
	nextFn := strings.Index(rendered[fnStart+1:], "\nfunc ")
	end := len(rendered)
	if nextFn >= 0 {
		end = fnStart + 1 + nextFn
	}
	fullBody := rendered[fnStart:end]

	if !strings.Contains(fullBody, "newError(fatalErrCopy, computeErrorRaw())") {
		t.Errorf("fatalErrCopy emit must pass computeErrorRaw(); runFullOrchestration body:\n%s",
			snippet(fullBody, 4000))
	}
	if !strings.Contains(fullBody, "newError(lastErr, computeErrorRaw())") {
		t.Errorf("lastErr emit must pass computeErrorRaw(); runFullOrchestration body:\n%s",
			snippet(fullBody, 4000))
	}

	// 3. The panic recovery handler emits newError(err, "") because the
	// extractor mutex may be poisoned by the panicking goroutine.
	if !strings.Contains(fullBody, `newError(err, "")`) {
		t.Errorf("panic recovery must pass empty raw to newError; runFullOrchestration body:\n%s",
			snippet(fullBody, 4000))
	}

	// reconcileRaw closure must be present so error frames pick up the
	// authoritative RawLLMResponse splice (matches the success-final
	// reconciliation). Without this, error raw could be truncated under
	// the same stale-SSE-view race the success path defends against.
	if !strings.Contains(fullBody, "reconcileRaw :=") {
		t.Errorf("runFullOrchestration must declare a reconcileRaw closure; body:\n%s",
			snippet(fullBody, 4000))
	}
	// reconcileRaw must still be used by the success-final path so a
	// future refactor doesn't accidentally split the reconciliation
	// between two codepaths and let error/success drift apart.
	if !strings.Contains(fullBody, "finalRaw = reconcileRaw(finalRaw, parseableFull)") {
		t.Errorf("success-final path must reuse reconcileRaw; body:\n%s",
			snippet(fullBody, 4000))
	}
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
