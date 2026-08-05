//go:build integration

package guardledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/internal/debaml"
	bamlclient "github.com/invakid404/baml-rest/internal/debaml/testdata/guard_ledger/baml_client"
)

// The rows stock cannot be ASKED in-process.
//
// Every other row in this package is driven in-process, because stock BAML
// either answers or returns an error. Two cannot be:
//
//	this is divisibleby(0)     BAML v0.223 evaluates the test in Rust and panics
//	                           with "attempt to calculate the remainder with a
//	                           divisor of zero" on the CFFI callback thread. A Go
//	                           `recover` in the caller cannot intercept it, so the
//	                           process either aborts or is left waiting forever.
//	range(10^12)|length        a resource-risk row: stock's `range` is asked for a
//	                           trillion elements. Whatever it does — error, refuse,
//	                           or allocate — must not happen inside a binary that
//	                           still has 200+ rows to drive.
//
// Both are driven from an ISOLATED SUBPROCESS under a deadline. What the parent
// then records is decided by the child's OWN report, not by its exit status:
//
//	the deadline expired, or the child died on a SIGNAL   -> envProcessFatal
//	the child printed a structured "returned an error"    -> envEvaluatorError,
//	                                                         and the message is
//	                                                         pinned
//	the child printed a structured "returned a value"     -> the row's premise is
//	                                                         gone; FAIL
//	anything else                                          -> FAIL
//
// The distinction is load-bearing. Accepting "the child exited nonzero" as
// process-fatal would file an ordinary returned error — or a broken fixture, or
// a stale client — as an abort, which is exactly the error-class-versus-fatal
// confusion the envelope vocabulary exists to prevent. A boolean is never
// fabricated for either row.
//
// The subprocess is this same test binary re-executed against a helper test —
// the standard os/exec pattern — gated on an environment variable so the child
// is inert during a normal run.

const (
	fatalChildEnv = "GUARD_LEDGER_FATAL_CHILD"
	// childDeadline bounds each subprocess. Generous enough that a loaded
	// machine cannot produce a false "hang", short enough to keep the suite
	// usable; the child does nothing but load the CFFI and parse once.
	childDeadline = 45 * time.Second

	// The child's structured report. A prefix rather than an exit code, because
	// an exit code cannot distinguish "stock returned an error" from "the fixture
	// is stale" or "the client no longer has this method".
	//
	// The payload is ESCAPED onto one line. A stock coercion error is routinely
	// multi-line (BAML's ParsingError nests a `\n  - <root>: Failed: …` cause
	// list), and a raw newline would silently truncate the message at the first
	// break — before the parent compares it for equality, which is the whole
	// point of the returned-error branch.
	childErrorPrefix = "GUARD_LEDGER_CHILD returned-error: "
	childValuePrefix = "GUARD_LEDGER_CHILD returned-value: "
	// childReachedMarker is announced BEFORE the parse, and it is what turns a
	// hang into an observation.
	//
	// Without it, any stall inside the child satisfies the process-fatal arm: a
	// cold BAML cache making the CFFI load exceed the deadline, a stall in the
	// generated client's init, a future deadlock with nothing to do with the
	// predicate. Each would confirm the load-bearing claim without measuring it —
	// absence of a result read as evidence about the expression. A hang BEFORE
	// this marker is a harness failure; only a hang after it says anything about
	// stock.
	childReachedMarker = "GUARD_LEDGER_CHILD reached-parse"

	// hugeRangeStockError is the WHOLE engine message stock BAML v0.223 returns
	// for range(10^12), compared for EQUALITY rather than containment: a
	// substring match would accept a changed or re-wrapped error class, which is
	// the opposite of the exact-envelope discrimination this row exists for.
	hugeRangeStockError = "invalid operation: range has too many elements"
)

// childOutcome is a subprocess run, classified.
type childOutcome struct {
	// Kind is one of "timeout", "signal", "returned-error", "returned-value" or
	// "unreported" — never inferred from the exit code alone.
	Kind string
	// Reached reports whether the child announced that it got as far as the
	// stock parse. It is what separates "stock did not come back" from "the
	// child never asked stock anything".
	Reached bool
	// Detail is the child's reported message, or the runner error for a
	// signal/timeout.
	Detail string
	Output string
}

// runFatalChild re-executes this test binary against one helper test under a
// deadline and classifies what happened. It never collapses two outcomes into
// one: the caller decides which are acceptable for its row.
func runFatalChild(t *testing.T, run string) childOutcome {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), childDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run", run, "-test.v")
	cmd.Env = append(os.Environ(), fatalChildEnv+"=1")
	raw, runErr := cmd.CombinedOutput()
	return classifyChildRun(string(raw), runErr, ctx.Err() != nil)
}

// classifyChildRun is the classification itself, kept pure so it can be tested
// without spawning anything (see TestChildRunClassification).
//
// ORDER MATTERS, and it is report-first. A structured report means the child
// REACHED the CFFI and came back with an answer; that observation is already
// made, and it cannot be un-made by what the process or the deadline did
// afterwards. Checking the deadline first — as this did — turned a returned
// stock error into a tolerated "timeout", which is a false green in exactly the
// proof these rows exist for: `is divisibleby(0)` must be shown UNOBSERVABLE,
// and a returned error being reported as a timeout would satisfy that test while
// meaning the opposite.
func classifyChildRun(out string, runErr error, deadlineExpired bool) childOutcome {
	reached := strings.Contains(out, childReachedMarker)
	if line, ok := reportedLine(out, childErrorPrefix); ok {
		return childOutcome{Kind: "returned-error", Reached: reached, Detail: line, Output: out}
	}
	if line, ok := reportedLine(out, childValuePrefix); ok {
		return childOutcome{Kind: "returned-value", Reached: reached, Detail: line, Output: out}
	}
	// Nothing was reported, so the child never came back from the CFFI.
	if deadlineExpired {
		return childOutcome{Kind: "timeout", Reached: reached,
			Detail: "deadline exceeded before the child reported", Output: out}
	}
	// Death by SIGNAL is the abort case; every other exit means the child failed
	// before it could report, which is a harness/fixture problem rather than an
	// observation about stock.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && !exitErr.Exited() {
		return childOutcome{Kind: "signal", Reached: reached, Detail: exitErr.String(), Output: out}
	}
	return childOutcome{Kind: "unreported", Reached: reached, Detail: fmt.Sprint(runErr), Output: out}
}

// provesUnobservable reports whether a run that never returned says anything
// about the EXPRESSION.
//
// It requires two things, and the second is the one the round-6 review added:
// the child must have failed to return (a timeout or a signal death), AND it
// must have announced that it reached the parse. A stall before that point is a
// fact about the harness — a cold cache, a slow load, an unrelated deadlock —
// and reading it as "stock is unobservable" would be absence of evidence
// standing in for evidence.
func provesUnobservable(c childOutcome) bool {
	return c.Reached && (c.Kind == "timeout" || c.Kind == "signal")
}

// reportedLine finds the child's structured report and returns the UNESCAPED
// payload that follows the prefix.
//
// The trim is FRAMING-ONLY, and that is the whole point. The split above has
// already consumed the `\n` the child wrote, so the only byte that can still be
// framing is a `\r` from a CRLF writer — and a CR the CHILD meant is escaped as
// the two characters `\r` by [escapeChildPayload], so a literal CR here is never
// payload. Trimming whitespace generally, as this used to, silently dropped
// leading and trailing spaces and tabs from the message before the parent
// compared it for EQUALITY, which is exactly the byte-for-byte contract the
// escaping exists to keep.
func reportedLine(out, prefix string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			return unescapeChildPayload(strings.TrimSuffix(line[i+len(prefix):], "\r")), true
		}
	}
	return "", false
}

// escapeChildPayload / unescapeChildPayload carry an arbitrary message over a
// line-oriented channel without losing any of it. Only the two characters that
// would break the framing are touched, so a payload that contains neither
// round-trips byte for byte.
func escapeChildPayload(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func unescapeChildPayload(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		default:
			// Not one of ours: keep both bytes, so an escape the child never
			// wrote cannot silently eat a character.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TestGuardLedgerDivisibleByZeroIsUnobservable drives the divisibleby(0) row in a
// subprocess and requires stock to be UNOBSERVABLE there — a hang or an abort,
// not a returned error.
//
// A returned error would be an ordinary evaluator-error envelope, which native
// could be compared against; the U classification and the guard that rests on it
// would then need re-deriving. That is why this fails rather than passes on it.
func TestGuardLedgerDivisibleByZeroIsUnobservable(t *testing.T) {
	if os.Getenv(fatalChildEnv) != "" {
		t.Skip("child process: driven by the parent")
	}
	c := runFatalChild(t, "^TestGuardLedgerDivisibleByZeroChild$")
	if (c.Kind == "timeout" || c.Kind == "signal") && !provesUnobservable(c) {
		t.Fatalf("the child did not return (%s: %s) but never announced that it REACHED the parse, so this says "+
			"nothing about `is divisibleby(0)` — it is a harness failure (a cold CFFI cache, a stall in the "+
			"generated client's init, or an unrelated deadlock):\n%s", c.Kind, c.Detail, c.Output)
	}
	switch c.Kind {
	case "timeout":
		// The Rust panic fired on the callback thread and the Go caller is still
		// waiting for a result that will never arrive.
		t.Logf("recorded envelope %s: stock BAML v0.223 HANGS on `is divisibleby(0)` — the child reached the "+
			"parse and never came back (killed after %s)", envProcessFatal, childDeadline)
	case "signal":
		if !strings.Contains(c.Output, "divisor of zero") && !strings.Contains(c.Output, "remainder") {
			t.Fatalf("the child died on a signal, but not with the expected divisor-of-zero abort (%s):\n%s",
				c.Detail, c.Output)
		}
		t.Logf("recorded envelope %s: stock BAML v0.223 aborts the process on `is divisibleby(0)` (%s)",
			envProcessFatal, c.Detail)
	case "returned-error":
		t.Fatalf("stock BAML RETURNED an error for `is divisibleby(0)` instead of aborting: %q.\n"+
			"That is an %s envelope, not %s — the U classification and the profile guard both rest on "+
			"unobservability and must be re-derived.", c.Detail, envEvaluatorError, envProcessFatal)
	case "returned-value":
		t.Fatalf("stock BAML ANSWERED `is divisibleby(0)`: %q; the guard is no longer justified.", c.Detail)
	default:
		t.Fatalf("the child neither reported nor died on a signal (%s: %s); this is a harness or fixture "+
			"failure, not an observation about stock:\n%s", c.Kind, c.Detail, c.Output)
	}
}

// TestGuardLedgerNativeRefusesDivisibleByZero pins native's side next to the
// evidence: a refusal, never an answer — and a SPECIFIC one, so a future change
// that refused it for an unrelated reason would not silently look the same.
func TestGuardLedgerNativeRefusesDivisibleByZero(t *testing.T) {
	for _, expr := range []string{
		"this is divisibleby(0)",
		"1 is divisibleby(0)",
	} {
		_, err := debaml.EvaluateConstraint(debaml.IntValue(4), expr)
		if !errors.Is(err, debaml.ErrConstraintUnsupported) {
			t.Errorf("%q must be refused (stock aborts the process on it); got err=%v", expr, err)
			continue
		}
		if got := attributeNativeGuard(err); got != "divisibleByZero" {
			t.Errorf("%q was refused by %q, want the divisibleByZero guard — err=%v", expr, got, err)
		}
	}
	// A non-zero divisor is inside the profile and must still DECIDE, so the
	// guard is specific rather than a blanket withdrawal of the test.
	ok, err := debaml.EvaluateConstraint(debaml.IntValue(4), "this is divisibleby(2)")
	if err != nil || !ok {
		t.Errorf("divisibleby(2) = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestGuardLedgerHugeRangeIsIsolated drives the oversized-range row in a
// subprocess so its resource behaviour cannot affect the rows around it, and
// pins the SPECIFIC error stock returns.
//
// An unpinned "it failed somehow" would be satisfied by a stale client or a
// broken fixture just as well as by stock's own limit, which is why the message
// is asserted.
func TestGuardLedgerHugeRangeIsIsolated(t *testing.T) {
	if os.Getenv(fatalChildEnv) != "" {
		t.Skip("child process: driven by the parent")
	}
	c := runFatalChild(t, "^TestGuardLedgerHugeRangeChild$")
	if (c.Kind == "timeout" || c.Kind == "signal") && !provesUnobservable(c) {
		t.Fatalf("the child did not return (%s: %s) but never announced that it REACHED the parse; that is a "+
			"harness failure rather than anything about range(10^12):\n%s", c.Kind, c.Detail, c.Output)
	}
	switch c.Kind {
	case "returned-error":
		inner, ok := stockInnerError(c.Detail)
		if !ok {
			t.Fatalf("stock returned an error for range(10^12) that carries no engine message: %q", c.Detail)
		}
		if inner != hugeRangeStockError {
			t.Fatalf("stock returned a DIFFERENT error for range(10^12):\n  got  %q\n  want %q (exactly).\n"+
				"The row records stock's own limit, and the whole message is the observation — a changed or "+
				"re-wrapped error class would mean the fixture, the client or the runtime is at fault.",
				inner, hugeRangeStockError)
		}
		t.Logf("recorded envelope %s: stock BAML v0.223 rejects range(10^12) with %q", envEvaluatorError, inner)
	case "timeout", "signal":
		t.Fatalf("stock did not RETURN on range(10^12) (%s: %s). That is a %s envelope rather than the "+
			"%s this row records, and the `range` withdrawal's rationale changes with it:\n%s",
			c.Kind, c.Detail, envProcessFatal, envEvaluatorError, c.Output)
	case "returned-value":
		t.Fatalf("stock BAML ANSWERED range(10^12): %q; the withdrawal's unbounded-allocation rationale "+
			"needs re-deriving.", c.Detail)
	default:
		t.Fatalf("the child neither reported nor died on a signal (%s: %s); this is a harness or fixture "+
			"failure, not an observation about stock:\n%s", c.Kind, c.Detail, c.Output)
	}

	// Native must refuse it either way, and refuse it for a NAMED reason rather
	// than incidentally.
	//
	// The reason measured here is the OPERATOR GATE, not the `range` withdrawal:
	// no global callable parses in the closed predicate grammar, so the whole
	// expression is declined before the withdrawn global is ever reached. The
	// withdrawal sits behind that gate and is witnessed by rows RANGE_LIST,
	// RANGE_LAST and RANGE_STEP. Pinning the attribution keeps this from
	// silently becoming "something refused" — which an unrelated future guard
	// would satisfy just as well.
	_, err := debaml.EvaluateConstraint(debaml.IntValue(1), "range(1000000000000)|length == 1000000000000")
	if !errors.Is(err, debaml.ErrConstraintUnsupported) {
		t.Fatalf("native must refuse the oversized range; got err=%v", err)
	}
	if got := attributeNativeGuard(err); got != "operatorShapeIsProven" {
		t.Errorf("the oversized range was refused by %q, want operatorShapeIsProven — the gate that declines "+
			"every global callable before the withdrawal behind it is reached; err=%v", got, err)
	}
}

// TestGuardLedgerDivisibleByZeroChild is a subprocess body. It is inert unless
// the parent sets the gate, so `go test` never trips it by accident.
//
// It REPORTS what stock did on a single line the parent parses, so "stock
// returned an error" can never be confused with "the child failed to run".
func TestGuardLedgerDivisibleByZeroChild(t *testing.T) {
	if os.Getenv(fatalChildEnv) == "" {
		t.Skip("helper for TestGuardLedgerDivisibleByZeroIsUnobservable")
	}
	reportChild(t, func() (any, error) { return bamlclient.Parse.GLF_divzeroFn(`{"v":4}`) })
}

// TestGuardLedgerHugeRangeChild is the other subprocess body.
func TestGuardLedgerHugeRangeChild(t *testing.T) {
	if os.Getenv(fatalChildEnv) == "" {
		t.Skip("helper for TestGuardLedgerHugeRangeIsIsolated")
	}
	reportChild(t, func() (any, error) { return bamlclient.Parse.GLF_hugerangeFn(`{"v":1}`) })
}

// reportChild drives one stock parse and prints the structured report the parent
// classifies on. It deliberately does NOT fail the test for a returned error —
// the parent decides what a returned error means for its row.
func reportChild(t *testing.T, parse func() (any, error)) {
	t.Helper()
	// ANNOUNCED FIRST, and deliberately before anything that can block. os.Stdout
	// is unbuffered here, so the marker is in the pipe before the parse starts;
	// if the process is killed mid-parse the parent still reads it, which is the
	// whole point — it is what distinguishes a hang INSIDE stock from a hang on
	// the way to it.
	fmt.Println(childReachedMarker)
	v, err := parse()
	if err != nil {
		fmt.Printf("%s%s\n", childErrorPrefix, escapeChildPayload(err.Error()))
		return
	}
	fmt.Printf("%s%s\n", childValuePrefix, escapeChildPayload(fmt.Sprint(v)))
}
