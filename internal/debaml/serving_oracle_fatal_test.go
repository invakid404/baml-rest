//go:build integration

package debaml

// The row stock cannot be ASKED in-process.
//
// Every other row is driven in-process because stock either answers or returns an
// error. `this is divisibleby(0)` does neither: BAML v0.223 evaluates the test in
// Rust and panics with "attempt to calculate the remainder with a divisor of zero"
// on the CFFI callback thread, which a Go `recover` in the caller cannot
// intercept, so the process either aborts or is left waiting for a result that
// never arrives. Driving it in the main binary would take the rest of the corpus
// down with it.
//
// It is therefore driven from an ISOLATED SUBPROCESS under a deadline, and what
// the parent records is decided by the child's OWN report rather than by its exit
// status:
//
//	the deadline expired, or the child died on a SIGNAL   -> process-fatal
//	the child printed a structured "returned an error"    -> FAIL; that is an
//	                                                         evaluator-error
//	                                                         envelope, and the row
//	                                                         and the native guard
//	                                                         behind it have to be
//	                                                         re-derived
//	the child printed a structured "returned a value"     -> FAIL; the premise is
//	                                                         gone
//	anything else                                          -> FAIL, as a harness
//	                                                         failure
//
// A NON-RETURN ONLY COUNTS AFTER THE REACHED MARKER. Without it, any stall
// satisfies the process-fatal arm: a cold BAML cache making the CFFI load exceed
// the deadline, a stall while the project compiles, a future deadlock with nothing
// to do with the predicate. Each would confirm the claim without measuring it —
// absence of a result standing in for evidence about the expression. The child
// announces that it reached the parse BEFORE calling it, on unbuffered stdout, so
// a hang before that point is a harness failure and only a hang after it says
// anything about stock.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	// soFatalChildEnv gates the child body so it is inert during a normal run.
	soFatalChildEnv = "BAML_SERVING_ORACLE_FATAL_CHILD"
	// soChildDeadline bounds the subprocess: generous enough that a loaded machine
	// cannot produce a false "hang", short enough to keep the suite usable.
	soChildDeadline = 45 * time.Second

	// The child's structured report. A prefix rather than an exit code, because an
	// exit code cannot distinguish "stock returned an error" from "the fixture is
	// stale" or "the project failed to compile". The payload is escaped onto one
	// line because a BAML coercion error is routinely multi-line.
	soChildErrorPrefix = "SERVING_ORACLE_CHILD returned-error: "
	soChildValuePrefix = "SERVING_ORACLE_CHILD returned-value: "
	// soChildReachedMarker is announced BEFORE the parse and is what turns a hang
	// into an observation.
	soChildReachedMarker = "SERVING_ORACLE_CHILD reached-parse"
)

// soChildOutcome is a subprocess run, classified. The kind is never inferred from
// the exit code alone.
type soChildOutcome struct {
	Kind    string // timeout | signal | returned-error | returned-value | unreported
	Reached bool
	Detail  string
	Output  string
}

// soClassifyChildRun is the classification itself, kept pure so it can be tested
// without spawning anything.
//
// ORDER MATTERS, and it is report-first. A structured report means the child
// REACHED the CFFI and came back with an answer; that observation is already made
// and cannot be un-made by what the deadline did afterwards. Checking the deadline
// first would let a returned stock error be recorded as a timeout — a false green
// in exactly the proof this row exists for.
func soClassifyChildRun(out string, runErr error, deadlineExpired bool) soChildOutcome {
	reached := strings.Contains(out, soChildReachedMarker)
	if line, ok := soReportedLine(out, soChildErrorPrefix); ok {
		return soChildOutcome{Kind: "returned-error", Reached: reached, Detail: line, Output: out}
	}
	if line, ok := soReportedLine(out, soChildValuePrefix); ok {
		return soChildOutcome{Kind: "returned-value", Reached: reached, Detail: line, Output: out}
	}
	if deadlineExpired {
		return soChildOutcome{Kind: "timeout", Reached: reached,
			Detail: "deadline exceeded before the child reported", Output: out}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && !exitErr.Exited() {
		return soChildOutcome{Kind: "signal", Reached: reached, Detail: exitErr.String(), Output: out}
	}
	return soChildOutcome{Kind: "unreported", Reached: reached, Detail: fmt.Sprint(runErr), Output: out}
}

// soProvesUnobservable reports whether a run that never returned says anything
// about the EXPRESSION: it must have failed to return AND have announced that it
// reached the parse.
func soProvesUnobservable(c soChildOutcome) bool {
	return c.Reached && (c.Kind == "timeout" || c.Kind == "signal")
}

// soReportedLine finds the child's structured report and unescapes the payload.
func soReportedLine(out, prefix string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			return soUnescapeChildPayload(strings.TrimSuffix(line[i+len(prefix):], "\r")), true
		}
	}
	return "", false
}

// soEscapeChildPayload / soUnescapeChildPayload carry an arbitrary message over a
// line-oriented channel without losing any of it: only the bytes that would break
// the framing are touched.
func soEscapeChildPayload(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func soUnescapeChildPayload(s string) string {
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
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TestServingOracleFatalRowIsUnobservable drives every Fatal row in a subprocess
// and requires stock to be UNOBSERVABLE there.
func TestServingOracleFatalRowIsUnobservable(t *testing.T) {
	if os.Getenv(soFatalChildEnv) != "" {
		t.Skip("child process: driven by the parent")
	}
	rows := 0
	for _, f := range servingOracleFixtures {
		if !f.Fatal {
			continue
		}
		rows++
		t.Run(f.Name, func(t *testing.T) {
			c := soRunFatalChild(t, f.Name)
			if (c.Kind == "timeout" || c.Kind == "signal") && !soProvesUnobservable(c) {
				t.Fatalf("the child did not return (%s: %s) but never announced that it REACHED the parse, so "+
					"this says nothing about %s — it is a harness failure (a cold CFFI cache, a stall while the "+
					"in-memory project compiled, or an unrelated deadlock):\n%s", c.Kind, c.Detail, f.Name, c.Output)
			}
			switch c.Kind {
			case "timeout":
				t.Logf("recorded envelope %s: stock BAML v0.223 HANGS on this row — the child reached the parse "+
					"and never came back (killed after %s)", soStockProcessFatal, soChildDeadline)
			case "signal":
				if !strings.Contains(c.Output, "divisor of zero") && !strings.Contains(c.Output, "remainder") {
					t.Fatalf("the child died on a signal, but not with the expected divisor-of-zero abort (%s):\n%s",
						c.Detail, c.Output)
				}
				t.Logf("recorded envelope %s: stock BAML v0.223 aborts the process on this row (%s)",
					soStockProcessFatal, c.Detail)
			case "returned-error":
				t.Fatalf("stock RETURNED an error for %s instead of aborting: %q.\nThat is an %s envelope, not "+
					"%s — the row's classification and the native guard that rests on unobservability must be "+
					"re-derived.", f.Name, c.Detail, soStockEvaluatorError, soStockProcessFatal)
			case "returned-value":
				t.Fatalf("stock ANSWERED %s: %q; the native guard is no longer justified.", f.Name, c.Detail)
			default:
				t.Fatalf("the child neither reported nor died on a signal (%s: %s); this is a harness or fixture "+
					"failure, not an observation about stock:\n%s", c.Kind, c.Detail, c.Output)
			}

			// Native's side, pinned next to the evidence: a refusal, never an answer.
			native := soRunNative(f)
			if len(native.Sites) == 0 {
				t.Fatalf("native evaluated no predicate for %s, so its refusal is not observable: %s",
					f.Name, native.render())
			}
			for _, s := range native.Sites {
				if s.Outcome != constraintOutcomeUnsupported {
					t.Fatalf("native DECIDED %s for %s where stock cannot be observed at all; no boolean may be "+
						"fabricated there", s.Outcome, s.render())
				}
			}
		})
	}
	if rows == 0 {
		t.Fatal("no Fatal row in the corpus; this test would assert nothing")
	}
}

// soRunFatalChild re-executes this test binary against the child body under a
// deadline and classifies what happened.
func soRunFatalChild(t *testing.T, fixture string) soChildOutcome {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), soChildDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run", "^TestServingOracleFatalChild$", "-test.v")
	cmd.Env = append(os.Environ(), soFatalChildEnv+"="+fixture)
	raw, runErr := cmd.CombinedOutput()
	return soClassifyChildRun(string(raw), runErr, ctx.Err() != nil)
}

// TestServingOracleFatalChild is the subprocess body. It is inert unless the
// parent names a fixture, so `go test` never trips it by accident.
func TestServingOracleFatalChild(t *testing.T) {
	name := os.Getenv(soFatalChildEnv)
	if name == "" {
		t.Skip("helper for TestServingOracleFatalRowIsUnobservable")
	}
	var target servingOracleFixture
	for _, f := range servingOracleFixtures {
		if f.Name == name {
			target = f
		}
	}
	if target.Name == "" {
		t.Fatalf("no fixture named %q", name)
	}
	soEnsureRuntime(t)

	// ANNOUNCED FIRST, deliberately after the project has compiled and before
	// anything that can block. os.Stdout is unbuffered here, so the marker is in the
	// pipe before the parse starts; if the process is killed mid-parse the parent
	// still reads it, which is what distinguishes a hang INSIDE stock from a hang on
	// the way to it.
	fmt.Println(soChildReachedMarker)
	env, err := soDriveStock(target)
	if err != nil {
		fmt.Printf("%s%s\n", soChildErrorPrefix, soEscapeChildPayload(err.Error()))
		return
	}
	if env.Kind != soStockValue {
		fmt.Printf("%s%s\n", soChildErrorPrefix, soEscapeChildPayload(env.render()))
		return
	}
	fmt.Printf("%s%s\n", soChildValuePrefix, soEscapeChildPayload(env.render()))
}

// TestServingOracleChildClassification proves the classifier CLASSIFIES, over
// synthetic transcripts, so the live arms above rest on a function whose behaviour
// is pinned rather than on one that has only ever been exercised one way.
func TestServingOracleChildClassification(t *testing.T) {
	// A REAL signal death rather than a hand-built ExitError: the classifier reads
	// ProcessState, so a zero value would test a shape that cannot occur.
	fatalErr := soSignalDeathError(t)
	cases := []struct {
		name       string
		out        string
		runErr     error
		deadline   bool
		wantKind   string
		wantProves bool
	}{
		{"reached then killed", soChildReachedMarker + "\n", nil, true, "timeout", true},
		{"killed before reaching", "loading\n", nil, true, "timeout", false},
		{"reported an error even though the deadline expired",
			soChildReachedMarker + "\n" + soChildErrorPrefix + "boom\n", nil, true, "returned-error", false},
		{"reported a value", soChildReachedMarker + "\n" + soChildValuePrefix + "v\n", nil, false,
			"returned-value", false},
		{"nothing reported and no signal", "", errors.New("exit status 1"), false, "unreported", false},
		{"signal death after reaching", soChildReachedMarker + "\n", fatalErr, false, "signal", true},
		{"signal death before reaching", "loading\n", fatalErr, false, "signal", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := soClassifyChildRun(tc.out, tc.runErr, tc.deadline)
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if soProvesUnobservable(got) != tc.wantProves {
				t.Fatalf("provesUnobservable = %v, want %v (reached=%v kind=%q)",
					soProvesUnobservable(got), tc.wantProves, got.Reached, got.Kind)
			}
		})
	}
	// The escaping round-trips a payload that contains both framing bytes, so a
	// multi-line stock error is compared as the bytes stock produced.
	const payload = "a\nb\\c\rd"
	if got := soUnescapeChildPayload(soEscapeChildPayload(payload)); got != payload {
		t.Fatalf("payload round-trip = %q, want %q", got, payload)
	}
}

// soSignalDeathError produces a genuine *exec.ExitError for a process killed by a
// signal, so the classifier's signal arm is exercised against the real thing.
func soSignalDeathError(t *testing.T) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError from a signal death, got %v", err)
	}
	if exitErr.Exited() {
		t.Fatalf("the helper process exited normally (%v); it must die on a signal for this control to "+
			"mean anything", exitErr)
	}
	return err
}
