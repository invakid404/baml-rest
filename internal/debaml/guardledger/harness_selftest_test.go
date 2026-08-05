//go:build integration

package guardledger

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// SELF-TESTS for the harness's own machinery.
//
// The rows prove things about BAML; these prove things about the code that
// reads BAML's answers. Each case below FAILED before the fix it pins and
// passes after, and each is a defect that would otherwise have shown up as a
// green run over a mis-read observation — the worst failure mode a proof
// harness has, because the ledger would carry a number nobody could tell was
// wrong.

// TestChildRunClassificationPrefersAnEmittedReport pins the ordering rule in
// [classifyChildRun].
//
// The runner used to consult the DEADLINE first, so a child that reported a
// returned stock error and then overran — or was still being torn down when the
// context expired — was classified as a timeout. That is a false green in the
// exact proof the fatal rows exist for: `is divisibleby(0)` must be shown
// UNOBSERVABLE, and the parent treats "timeout" as confirmation of that while
// treating "returned-error" as a refutation. The two must never be confusable.
func TestChildRunClassificationPrefersAnEmittedReport(t *testing.T) {
	const reported = "invalid operation: range has too many elements"
	withReport := "some framework noise\n" + childErrorPrefix + reported + "\nmore noise\n"

	// REAL process errors, not fabricated ones. classifyChildRun distinguishes a
	// child that EXITED non-zero from one that died on a SIGNAL by asking
	// (*exec.ExitError).Exited(), and only a genuine ProcessState answers that;
	// a hand-rolled error would exercise the fallback arm instead of the one
	// under test.
	exitErr := runForError(t, "exit 3")
	signalErr := runForError(t, "kill -ABRT $$")
	if ee := (*exec.ExitError)(nil); !errors.As(exitErr, &ee) || !ee.Exited() {
		t.Fatalf("the exit-status fixture is not a clean non-zero EXIT: %v", exitErr)
	}
	if ee := (*exec.ExitError)(nil); !errors.As(signalErr, &ee) || ee.Exited() {
		t.Fatalf("the signal fixture did not die on a SIGNAL: %v", signalErr)
	}

	for _, tc := range []struct {
		name            string
		out             string
		runErr          error
		deadlineExpired bool
		wantKind        string
		wantDetail      string
	}{
		{
			// THE REGRESSION. A report exists AND the deadline expired: the
			// observation was already made, so it wins.
			name: "report survives an expired deadline", out: withReport, deadlineExpired: true,
			wantKind: "returned-error", wantDetail: reported,
		},
		{
			name: "report with no deadline pressure", out: withReport,
			wantKind: "returned-error", wantDetail: reported,
		},
		{
			// A report also outranks a NON-ZERO EXIT. The child prints its
			// observation and then lets the test framework fail it — which is
			// exactly what the divisibleby(0) child does — so an exit-status-first
			// classifier would file a returned stock error as "unreported" and the
			// parent would never see the refutation it exists to catch.
			name: "report survives a non-zero exit", out: withReport, runErr: exitErr,
			wantKind: "returned-error", wantDetail: reported,
		},
		{
			// And over a SIGNAL death, which is the arm that means process-fatal.
			// A child can report and still abort during teardown; the observation
			// was already made.
			name: "report survives a signal death", out: withReport, runErr: signalErr,
			wantKind: "returned-error", wantDetail: reported,
		},
		{
			// All three at once: report, signal, expired deadline. Report wins.
			name: "report survives a signal death and an expired deadline",
			out:  withReport, runErr: signalErr, deadlineExpired: true,
			wantKind: "returned-error", wantDetail: reported,
		},
		{
			// The converse, so the rule is not "always returned-error": a
			// non-zero exit with NO report is a child that failed before it could
			// observe anything, which is a harness or fixture problem rather than
			// a statement about stock.
			name: "a non-zero exit with no report is not an emitted error",
			out:  "nothing structured here\n", runErr: exitErr,
			wantKind: "unreported",
		},
		{
			// A signal death with no report IS the abort case the fatal rows
			// record as process-fatal.
			name: "a signal death with no report is an abort",
			out:  "nothing structured here\n", runErr: signalErr,
			wantKind: "signal",
		},
		{
			name: "a returned value is its own outcome", out: childValuePrefix + "42\n",
			wantKind: "returned-value", wantDetail: "42",
		},
		{
			// No report at all is the only thing a timeout may be inferred from.
			name: "no report and an expired deadline", out: "nothing structured here\n", deadlineExpired: true,
			wantKind: "timeout",
		},
		{
			name: "no report and a plain exit", out: "nothing structured here\n",
			wantKind: "unreported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyChildRun(tc.out, tc.runErr, tc.deadlineExpired)
			if got.Kind != tc.wantKind {
				t.Fatalf("classified as %q, want %q (detail %q)", got.Kind, tc.wantKind, got.Detail)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("detail %q, want %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// TestChildPayloadSurvivesTheLineProtocol pins that a MULTI-LINE stock error
// crosses the child boundary intact.
//
// BAML's coercion errors routinely carry embedded newlines — an assertion
// failure nests `\n  - <root>: Failed: …` — and the report is a single line, so
// an unescaped payload was truncated at the first break. The parent then
// compared a PREFIX of the message for equality and could not tell the
// difference between "stock said something else" and "the channel ate the rest".
func TestChildPayloadSurvivesTheLineProtocol(t *testing.T) {
	for _, payload := range []string{
		"invalid operation: range has too many elements",
		"Failed to coerce value: ParsingError {\n  reason: \"Assertions failed.\"\n  - <root>: Failed: l this == 2\n}",
		`a backslash \ and a \n literal and a "quote"`,
		"trailing carriage\r\nreturn",
		// OUTER WHITESPACE. The transport escapes backslash, CR and newline and
		// deliberately leaves spaces and tabs alone, so a reader that normalised
		// them would drop real bytes from a message the parent then compares for
		// EQUALITY — accepting a changed stock error as if it matched.
		" \t outer-space error \t ",
		"\tleading tab only",
		"trailing spaces only   ",
	} {
		line := childErrorPrefix + escapeChildPayload(payload)
		if strings.ContainsAny(line, "\n\r") {
			t.Fatalf("the escaped report still breaks the line framing: %q", line)
		}
		got, ok := reportedLine("noise\n"+line+"\nmore\n", childErrorPrefix)
		if !ok {
			t.Fatalf("the escaped report was not found in the child output: %q", line)
		}
		if got != payload {
			t.Errorf("payload did not survive the line protocol:\n  sent %q\n  got  %q", payload, got)
		}
	}
}

// TestStockInnerErrorStopsAtTheRealQuote pins the backslash-run PARITY rule.
//
// Rust's Debug formatting escapes a backslash as `\\`, so a quote preceded by
// an EVEN-length run of backslashes is not escaped at all — it closes the
// string. Treating any preceding backslash as an escape read straight past that
// boundary and folded BAML's surrounding scaffolding into the inner error the
// row pins, which would then be compared for equality against a longer string
// than stock actually produced.
func TestStockInnerErrorStopsAtTheRealQuote(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{
			name: "plain message",
			text: `reason: "Failed to evaluate constraints: unknown filter: filter urlencode is unknown (in <string>:1)", causes: []`,
			want: "unknown filter: filter urlencode is unknown",
		},
		{
			// THE REGRESSION: two backslashes escape EACH OTHER, so the quote
			// that follows really does close the reason string.
			name: "even-length backslash run leaves the quote unescaped",
			text: `reason: "Failed to evaluate constraints: bad path C:\\", causes: [{"reason": "not part of the message"}]`,
			want: `bad path C:\\`,
		},
		{
			// An ODD run does escape the quote, so the message continues.
			name: "odd-length backslash run escapes the quote",
			text: `reason: "Failed to evaluate constraints: he said \"hi\" loudly", causes: []`,
			want: `he said \"hi\" loudly`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stockInnerError(tc.text)
			if !ok {
				t.Fatalf("no inner error recovered from %q", tc.text)
			}
			if got != tc.want {
				t.Errorf("inner error:\n  got  %q\n  want %q", got, tc.want)
			}
		})
	}
}

// TestLedgerLoadersRejectTrailingData pins that the canonical ledger must be
// exactly ONE complete JSON document.
//
// encoding/json stops at the end of the first value, so a file holding a valid
// ledger followed by anything else decoded cleanly and the remainder was never
// read. That is proof material nobody looks at, presented as if it had been
// checked.
func TestLedgerLoadersRejectTrailingData(t *testing.T) {
	good, err := os.ReadFile(ledgerJSONPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerJSONPath, err)
	}
	// A second, self-consistent document appended after the first: the shape a
	// bad merge or a truncated rewrite actually produces.
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, append(append([]byte(nil), good...), []byte("\n{\"records\":[]}\n")...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	if _, err := loadLedger(path); err == nil {
		t.Error("loadLedger accepted a file carrying a second JSON document after the ledger")
	} else if !strings.Contains(err.Error(), "after the first JSON document") {
		t.Errorf("loadLedger rejected the trailing data for the wrong reason: %v", err)
	}
	if _, err := loadLedgerCallableRows(path); err == nil {
		t.Error("loadLedgerCallableRows accepted a file carrying a second JSON document after the ledger")
	} else if !strings.Contains(err.Error(), "after the first JSON document") {
		// Pinned for the same reason as its sibling above: a decoder that started
		// failing for an unrelated reason would otherwise look like a working
		// trailing-data check.
		t.Errorf("loadLedgerCallableRows rejected the trailing data for the wrong reason: %v", err)
	}

	// The real file must still load, so the check is specific rather than a
	// blanket refusal.
	if _, err := loadLedger(ledgerJSONPath); err != nil {
		t.Errorf("the checked-in ledger no longer loads: %v", err)
	}
}

// TestStockModulePinCatchesAReplacementFromAnySource pins the check that the
// stock-CFFI authority claim actually rests on.
//
// The pin used to read the root go.mod only. That manifest is not where module
// resolution ends: the repository builds inside a WORKSPACE, and a `replace` in
// go.work overrides go.mod while leaving every version string reading v0.223.0.
// A same-version fork introduced that way would satisfy the CFFI runtime string
// AND the require pin, and every envelope in this package would then have been
// recorded against something that is not stock.
//
// So the regression is end-to-end: it builds a hostile workspace that replaces
// the module with a local stub and asks the go command to resolve it. The
// effective check sees the replacement; the manifest checks name the file.
func TestStockModulePinCatchesAReplacementFromAnySource(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("locate the repo root: %v", err)
	}

	// (1) THE EFFECTIVE RESOLUTION, end to end. A go.work replacement is exactly
	// what the manifest-only check could not see.
	t.Run("go.work replacement is visible to the effective check", func(t *testing.T) {
		dir := t.TempDir()
		stub := filepath.Join(dir, "notstock")
		if err := os.MkdirAll(stub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stub, "go.mod"),
			[]byte("module "+bamlModulePath+"\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatalf("write stub go.mod: %v", err)
		}
		// The `go` directive is READ FROM THE REPOSITORY rather than hardcoded.
		// The go command rejects a workspace whose directive is lower than a
		// module in its `use` list requires, so a pinned literal would start
		// failing the moment the repo raised its Go version — and it would fail
		// as "resolve under the hostile workspace", i.e. as a broken fixture
		// wearing the costume of a real finding, instead of testing replacement
		// visibility at all.
		goDirective := repoWorkspaceGoDirective(t, repoRoot)
		work := filepath.Join(dir, "go.work")
		if err := os.WriteFile(work, fmt.Appendf(nil,
			"go %s\n\nuse %s\n\nreplace %s => %s\n",
			goDirective, repoRoot, bamlModulePath, stub), 0o644); err != nil {
			t.Fatalf("write hostile go.work: %v", err)
		}

		hostile, err := resolveStockModule(repoRoot, work)
		if err != nil {
			t.Fatalf("resolve under the hostile workspace: %v", err)
		}
		if hostile.ReplacedBy == "" {
			t.Fatal("a go.work `replace` of the stock module did not surface as a replacement; the effective " +
				"check cannot see workspace overrides and the pin is back to reading one manifest")
		}
		// And the version is UNCHANGED, which is the whole danger: every other
		// signal still says v0.223.0.
		if hostile.Version != wantBAMLModuleVersion {
			t.Logf("note: the hostile workspace also changed the version to %q", hostile.Version)
		}

		// The clean tip must resolve unreplaced through the same code path, so
		// the check is specific rather than a blanket refusal.
		clean, err := resolveStockModule(repoRoot, "")
		if err != nil {
			t.Fatalf("resolve the clean tip: %v", err)
		}
		if clean.ReplacedBy != "" {
			t.Errorf("the checked-in tree resolves %s through %q; it must link the stock module",
				bamlModulePath, clean.ReplacedBy)
		}
		if clean.Version != wantBAMLModuleVersion {
			t.Errorf("the checked-in tree resolves %s to %q, want %q", bamlModulePath, clean.Version, wantBAMLModuleVersion)
		}
	})

	// (2) THE MANIFEST SCANS, so a red run names the file to change. Both
	// positions and both forms are covered, in go.mod and in go.work alike.
	for _, tc := range []struct {
		name    string
		content string
		scan    func(string, []byte) ([]string, error)
		file    string
		want    bool
	}{
		{
			name: "go.mod replaces the module as the source", file: "go.mod", scan: bamlReplacementsInGoMod,
			content: "module x\n\ngo 1.21\n\nreplace " + bamlModulePath + " => ./fork\n", want: true,
		},
		{
			name: "go.mod replaces some other module WITH it", file: "go.mod", scan: bamlReplacementsInGoMod,
			content: "module x\n\ngo 1.21\n\nreplace example.com/other => " + bamlModulePath + " v0.223.0\n", want: true,
		},
		{
			name: "go.mod block form", file: "go.mod", scan: bamlReplacementsInGoMod,
			content: "module x\n\ngo 1.21\n\nreplace (\n\texample.com/a => ./a\n\t" + bamlModulePath + " => ./fork\n)\n", want: true,
		},
		{
			name: "clean go.mod", file: "go.mod", scan: bamlReplacementsInGoMod,
			content: "module x\n\ngo 1.21\n\nrequire " + bamlModulePath + " v0.223.0\n", want: false,
		},
		{
			name: "go.work replaces the module", file: "go.work", scan: bamlReplacementsInGoWork,
			content: "go 1.26.5\n\nuse .\n\nreplace " + bamlModulePath + " => ./fork\n", want: true,
		},
		{
			name: "go.work block form", file: "go.work", scan: bamlReplacementsInGoWork,
			content: "go 1.26.5\n\nuse .\n\nreplace (\n\texample.com/a => ./a\n\t" + bamlModulePath + " => ./fork\n)\n", want: true,
		},
		{
			name: "clean go.work", file: "go.work", scan: bamlReplacementsInGoWork,
			content: "go 1.26.5\n\nuse (\n\t.\n\t./bamlutils\n)\n", want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := tc.scan(tc.file, []byte(tc.content))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(found) > 0; got != tc.want {
				t.Errorf("replacement detected = %v (%v), want %v", got, found, tc.want)
			}
		})
	}

	// (3) The CHECKED-IN manifests are clean, which is what the live pin test
	// asserts and what this whole slice's authority rests on.
	for _, tc := range []struct {
		path string
		scan func(string, []byte) ([]string, error)
	}{
		{filepath.Join(repoRoot, "go.mod"), bamlReplacementsInGoMod},
		{filepath.Join(repoRoot, "go.work"), bamlReplacementsInGoWork},
	} {
		content, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		found, err := tc.scan(tc.path, content)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.path, err)
		}
		if len(found) > 0 {
			t.Errorf("%s replaces %s (%v)", tc.path, bamlModulePath, found)
		}
	}
}

// runForError runs a shell fragment and returns the error the go runtime
// reports for it, so the classifier's table can be driven with REAL
// *exec.ExitError values rather than stand-ins.
func runForError(t *testing.T, script string) error {
	t.Helper()
	err := exec.Command("sh", "-c", script).Run()
	if err == nil {
		t.Fatalf("%q was expected to fail, but it succeeded", script)
	}
	return err
}

// TestLedgerLoadersRejectDuplicateEntries pins that a duplicated key is REJECTED
// rather than overwritten.
//
// `loadLedgerCallableRows` returns a map keyed by callable id, and a plain
// assignment silently dropped the FIRST entry's witnesses on a collision — proof
// material disappearing inside the reader whose whole job is to surface it, in a
// document nobody would look at twice because it decoded cleanly.
//
// The same shape is covered for a duplicated RECORD key, which the harness's
// coverage test builds a map from as well.
func TestLedgerLoadersRejectDuplicateEntries(t *testing.T) {
	good, err := os.ReadFile(ledgerJSONPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerJSONPath, err)
	}
	var doc ledgerDocument
	if err := stdjson.Unmarshal(good, &doc); err != nil {
		t.Fatalf("decode %s: %v", ledgerJSONPath, err)
	}
	if len(doc.Callables) == 0 {
		t.Fatal("the ledger carries no callable inventory to duplicate")
	}

	// Duplicate the FIRST callable with a DIFFERENT witness list, so an
	// overwrite is observable rather than idempotent.
	dup := doc
	dup.Callables = append(append([]ledgerCallable(nil), doc.Callables...), doc.Callables[0])
	dup.Callables[len(dup.Callables)-1].WitnessRows = []string{"THIS_ROW_DOES_NOT_EXIST"}

	raw, err := stdjson.Marshal(dup)
	if err != nil {
		t.Fatalf("marshal the duplicated ledger: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	got, err := loadLedgerCallableRows(path)
	if err == nil {
		t.Fatalf("loadLedgerCallableRows accepted a ledger inventorying %q twice; it returned %v for that key, "+
			"so the other entry's cited witnesses were dropped without a word",
			doc.Callables[0].Callable, got[doc.Callables[0].Callable])
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("loadLedgerCallableRows rejected the duplicate for the wrong reason: %v", err)
	}
	if !strings.Contains(err.Error(), doc.Callables[0].Callable) {
		t.Errorf("the duplicate error does not name the colliding callable: %v", err)
	}

	// The real file must still load, so the check is specific rather than a
	// blanket refusal.
	if _, err := loadLedgerCallableRows(ledgerJSONPath); err != nil {
		t.Errorf("the checked-in ledger no longer loads: %v", err)
	}
}

// repoWorkspaceGoDirective is the `go` version the repository's own workspace
// declares, so a fixture workspace can mirror it instead of pinning a literal
// that goes stale on the next toolchain bump.
func repoWorkspaceGoDirective(t *testing.T, repoRoot string) string {
	t.Helper()
	path := filepath.Join(repoRoot, "go.work")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	wf, err := modfile.ParseWork(path, raw, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if wf.Go == nil || wf.Go.Version == "" {
		t.Fatalf("%s declares no go directive; the hostile fixture cannot mirror it", path)
	}
	return wf.Go.Version
}

// TestHangIsOnlyProofAfterTheChildReachedTheParse pins the rule that turns a
// subprocess hang into an observation.
//
// The process-fatal arm used to accept ANY hang. A cold CFFI cache pushing the
// load past the deadline, a stall in the generated client's init, or a future
// deadlock with nothing to do with `is divisibleby(0)` would each have confirmed
// the load-bearing claim without measuring it — absence of a result standing in
// for evidence about the expression.
func TestHangIsOnlyProofAfterTheChildReachedTheParse(t *testing.T) {
	const noise = "some framework noise\n"
	reached := noise + childReachedMarker + "\n"

	for _, tc := range []struct {
		name            string
		out             string
		runErr          error
		deadlineExpired bool
		wantReached     bool
		wantProof       bool
	}{
		{
			// THE REGRESSION. A hang with no marker says nothing about stock.
			name: "timeout before the parse is not proof", out: noise, deadlineExpired: true,
			wantReached: false, wantProof: false,
		},
		{
			name: "timeout after the parse IS proof", out: reached, deadlineExpired: true,
			wantReached: true, wantProof: true,
		},
		{
			name: "signal before the parse is not proof", out: noise, runErr: runForError(t, "kill -ABRT $$"),
			wantReached: false, wantProof: false,
		},
		{
			name: "signal after the parse IS proof", out: reached, runErr: runForError(t, "kill -ABRT $$"),
			wantReached: true, wantProof: true,
		},
		{
			// A child that RETURNED is not "unobservable" however it exited: the
			// observation was made, and belongs to the returned-error arm.
			name: "a returned error is never a hang", out: reached + childErrorPrefix + "boom\n",
			deadlineExpired: true, wantReached: true, wantProof: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyChildRun(tc.out, tc.runErr, tc.deadlineExpired)
			if c.Reached != tc.wantReached {
				t.Errorf("Reached = %v, want %v (kind %q)", c.Reached, tc.wantReached, c.Kind)
			}
			if got := provesUnobservable(c); got != tc.wantProof {
				t.Errorf("provesUnobservable = %v, want %v (kind %q, reached %v)",
					got, tc.wantProof, c.Kind, c.Reached)
			}
		})
	}

}

// TestChildEmitsTheReachedMarkerBeforeCallingParse is the PRODUCER half, and it
// is structural on purpose.
//
// The rule the parent now enforces — a hang only counts once the child has
// announced it reached the parse — is worth nothing if the child stops
// announcing, or announces too late. Both failures are silent: the marker never
// arrives, every honest hang is rejected as a harness failure, and the
// divisibleby(0) row quietly stops being measured.
//
// A text search cannot see either. The marker string survives in its own
// constant, in the comments and in the consumer, so deleting the sole
// `fmt.Println(childReachedMarker)` leaves a grep green — and a grep says
// nothing about ORDER. So this reads reportChild's SYNTAX TREE and requires the
// emitting call to exist and to sit before the call to the parse parameter.
func TestChildEmitsTheReachedMarkerBeforeCallingParse(t *testing.T) {
	fn, parseParam, _ := reportChildDecl(t)

	emit := findMarkerEmit(fn.Body, "childReachedMarker")
	if emit == token.NoPos {
		t.Fatal("reportChild does not EMIT childReachedMarker. The parent rejects any non-returning child that " +
			"never announced it reached the parse, so without this call every hang — including the real " +
			"divisibleby(0) one — is reported as a harness failure and the row stops being measured.\n" +
			"Expected a write of the marker: fmt.Print/Println/Printf/Fprint*, or a Write/WriteString call.")
	}
	call := findParamCall(fn.Body, parseParam)
	if call == token.NoPos {
		t.Fatalf("reportChild never calls its %q parameter; this test can no longer locate the parse it must "+
			"order the marker against", parseParam)
	}
	if emit >= call {
		t.Fatalf("reportChild emits childReachedMarker AFTER calling %s(). A child killed mid-parse would then "+
			"never have announced anything, so the marker could only ever appear for runs that already "+
			"returned — the rule it feeds would reject exactly the hangs it exists to qualify.", parseParam)
	}
}

// TestChildEscapesEveryReportedPayload is the same structural check for the
// other half of the protocol.
//
// TestChildPayloadSurvivesTheLineProtocol proves the CODEC round-trips, but it
// escapes the payload itself — so it stays green if the producer stops escaping,
// and a multi-line stock error would then be truncated before the parent's exact
// comparison. This requires every report the child writes to pass its payload
// through escapeChildPayload.
func TestChildEscapesEveryReportedPayload(t *testing.T) {
	fn, _, fset := reportChildDecl(t)

	reports := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isWriteCall(call.Fun) {
			return true
		}
		if !mentionsIdent(call, "childErrorPrefix") && !mentionsIdent(call, "childValuePrefix") {
			return true
		}
		reports++
		if !callsFunc(call, "escapeChildPayload") {
			t.Errorf("reportChild writes a report at %s:%d without passing its payload through escapeChildPayload; "+
				"a multi-line stock error would be truncated at the first newline, before the parent compares "+
				"it for equality", childProtocolFile, fset.Position(call.Pos()).Line)
		}
		return true
	})
	if reports < 2 {
		t.Fatalf("expected reportChild to write both a returned-error and a returned-value report; found %d", reports)
	}
}

// reportChildDecl parses the sibling fatal_test.go and returns reportChild's
// declaration plus the name of its function-typed parse parameter.
//
// The parameter name is read rather than assumed, so renaming it is a refactor
// rather than a silent hole in the ordering check above.
func reportChildDecl(t *testing.T) (*ast.FuncDecl, string, *token.FileSet) {
	t.Helper()
	const path = childProtocolFile
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "reportChild" || fn.Body == nil {
			continue
		}
		for _, field := range fn.Type.Params.List {
			if _, isFunc := field.Type.(*ast.FuncType); !isFunc || len(field.Names) == 0 {
				continue
			}
			return fn, field.Names[0].Name, fset
		}
		t.Fatalf("%s: reportChild takes no function parameter to call", path)
	}
	t.Fatalf("%s declares no reportChild; the child protocol has moved and this check must follow it", path)
	return nil, "", nil
}

// childProtocolFile is where the subprocess side of the report protocol lives.
const childProtocolFile = "fatal_test.go"

// findMarkerEmit is the position of the first call that WRITES the named
// constant, or NoPos.
func findMarkerEmit(body *ast.BlockStmt, marker string) token.Pos {
	found := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found != token.NoPos {
			return found == token.NoPos
		}
		if isWriteCall(call.Fun) && mentionsIdent(call, marker) {
			found = call.Pos()
			return false
		}
		return true
	})
	return found
}

// findParamCall is the position of the first call to the named parameter.
func findParamCall(body *ast.BlockStmt, name string) token.Pos {
	found := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found != token.NoPos {
			return found == token.NoPos
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = call.Pos()
			return false
		}
		return true
	})
	return found
}

// isWriteCall reports whether a callee actually puts bytes somewhere the parent
// can read. Anything else could mention the marker without emitting it.
func isWriteCall(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Print", "Println", "Printf", "Fprint", "Fprintln", "Fprintf", "Write", "WriteString":
		return true
	}
	return false
}

// mentionsIdent reports whether a call references the named identifier anywhere
// in its arguments.
func mentionsIdent(call *ast.CallExpr, name string) bool {
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return !found
		})
	}
	return found
}

// callsFunc reports whether a call has an argument that is itself a call to the
// named function.
func callsFunc(call *ast.CallExpr, name string) bool {
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			inner, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return !found
		})
	}
	return found
}

// TestDispositionObligationsAreNotVacuous drives every ledger disposition with a
// record that satisfies its non-row obligations and cites NOTHING, and requires
// each one that makes a claim about rows to reject it.
//
// The obligations are mostly of the form "every cited row looks like X", and a
// check of that shape passes over an empty list while proving nothing. Two arms
// were hardened against exactly that — kept-inert in an earlier lap, and
// kept-unprovable here — and this test is what keeps a future disposition from
// reintroducing it.
func TestDispositionObligationsAreNotVacuous(t *testing.T) {
	const link = "https://example.invalid/583"
	for _, tc := range []struct {
		disposition string
		rec         ledgerRecord
		wantProblem string
	}{
		{"removed", ledgerRecord{Key: "r", Disposition: "removed", SubsumedBy: "other"}, "cites no witness row"},
		{"kept-inert", ledgerRecord{Key: "r", Disposition: "kept-inert", Notes: "n"}, "cites no witness row"},
		{"kept-unprovable", ledgerRecord{Key: "r", Disposition: "kept-unprovable",
			DeferralRecord: link, LivenessProof: "TestX"}, "cites no witness row"},
		{"kept-over-decline", ledgerRecord{Key: "r", Disposition: "kept-over-decline",
			DeferralRecord: link}, "none of its witnesses"},
	} {
		t.Run(tc.disposition, func(t *testing.T) {
			problems := dispositionProblems([]ledgerRecord{tc.rec},
				func(string) (guardRow, bool) { return guardRow{}, false },
				map[string][]agreement{})
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.wantProblem) {
				t.Errorf("a %s record citing no row was accepted (or rejected for another reason).\n"+
					"  want a problem containing %q\n  got: %s", tc.disposition, tc.wantProblem, joined)
			}
		})
	}

	// kept-unwitnessable is the ONE disposition for which an empty list is the
	// point, so it must NOT be rejected for that — the check is specific rather
	// than a blanket "records must cite rows".
	problems := dispositionProblems([]ledgerRecord{{
		Key: "u", Disposition: "kept-unwitnessable", DeferralRecord: link, SubprocessWitness: "TestY",
	}}, func(string) (guardRow, bool) { return guardRow{}, false }, map[string][]agreement{})
	if len(problems) != 0 {
		t.Errorf("a kept-unwitnessable record citing no row must be accepted; got %v", problems)
	}
}

// TestGeneratorVersionIsPinnedToTheLinkedRuntime is the CGO-free half of the
// generator-provenance check: the fixture project must generate with the same
// BAML the harness links, or the checked-in client and the runtime driving it
// came from different versions.
func TestGeneratorVersionIsPinnedToTheLinkedRuntime(t *testing.T) {
	assertGeneratorVersionPinned(t)

	// The pattern must actually match something in the real file, or the
	// assertion above would be satisfied by a file it cannot read.
	raw, err := os.ReadFile(filepath.Join(fixtureSrcDir, "generators.baml"))
	if err != nil {
		t.Fatalf("read generators.baml: %v", err)
	}
	if got := generatorVersionPattern.FindAllStringSubmatch(string(raw), -1); len(got) != 1 {
		t.Fatalf("expected exactly one generator version declaration, found %d", len(got))
	}
}
