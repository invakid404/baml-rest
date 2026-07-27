//go:build integration

package constraintoracle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/internal/debaml"
	bamlclient "github.com/invakid404/baml-rest/internal/debaml/testdata/constraint_oracle/baml_client"
)

// `is divisibleby(0)` — the one expression the corpus cannot hold.
//
// Every other case in this package is driven in-process, because stock BAML
// either answers or returns an error. This one takes the PROCESS DOWN: BAML
// v0.223 evaluates the test in Rust and panics with "attempt to calculate the
// remainder with a divisor of zero" on the CFFI callback thread, which a Go
// `recover` in the caller cannot intercept. Observed here, the caller is then
// left waiting for a result that never arrives, so the process hangs rather
// than exiting. Putting it in the corpus would take the other 320 cases down
// with it.
//
// Leaving it merely undocumented would be worse: the expression is
// syntactically reachable, so "every reachable expression feature" has to
// account for it. It is therefore handled in three places:
//
//  1. the evaluator REFUSES it (constraint_profile.go's divisibleby guard), so
//     native never answers where the oracle cannot be observed;
//  2. TestNativeDivisibleByZeroIsRefused pins that refusal here, next to the
//     evidence; and
//  3. TestStockDivisibleByZeroIsUnobservable actually runs the stock leg — in a
//     SUBPROCESS — and proves the claim rather than asserting it.
//
// The subprocess is this same test binary re-executed against a helper test,
// the standard os/exec pattern: the child is gated on an environment variable
// so it is inert during a normal run. It runs under a DEADLINE because the
// observed failure mode is not always a clean abort: the Rust panic fires on
// the CFFI callback thread while the Go caller is still waiting for a result,
// so the child sometimes HANGS instead of exiting. Either way stock cannot be
// observed, which is what justifies the guard; only a clean successful return
// would falsify it.

const (
	divByZeroChildEnv = "BAML_CONSTRAINT_ORACLE_DIVZERO_CHILD"
	// childDeadline bounds the subprocess. Generous enough that a loaded machine
	// cannot produce a false "hang", short enough to keep the suite usable; the
	// child does nothing but load the CFFI and evaluate once.
	childDeadline = 45 * time.Second
)

// TestNativeDivisibleByZeroIsRefused pins native's side: refusal, not an answer.
func TestNativeDivisibleByZeroIsRefused(t *testing.T) {
	for _, expr := range []string{
		"this is divisibleby(0)",
		"1 is divisibleby(0)",
		"(this + 1) is divisibleby(0)",
	} {
		if _, err := debaml.EvaluateConstraint(debaml.IntValue(4), expr); !errors.Is(err, debaml.ErrConstraintUnsupported) {
			t.Errorf("%q must be refused (stock aborts the process on it); got err=%v", expr, err)
		}
	}
	// A non-zero divisor is inside the profile and must still decide, so the
	// guard is specific rather than a blanket removal of the test.
	ok, err := debaml.EvaluateConstraint(debaml.IntValue(4), "this is divisibleby(2)")
	if err != nil || !ok {
		t.Errorf("divisibleby(2) = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestStockDivisibleByZeroIsUnobservable runs the stock leg in a subprocess
// under a deadline and requires it NOT to return an answer. If a future BAML
// makes this survivable, this test fails and the guard above can be
// reconsidered on evidence.
func TestStockDivisibleByZeroIsUnobservable(t *testing.T) {
	if os.Getenv(divByZeroChildEnv) != "" {
		t.Skip("child process: driven by the parent")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), childDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run", "^TestStockDivisibleByZeroChild$", "-test.v")
	cmd.Env = append(os.Environ(), divByZeroChildEnv+"=1")
	out, runErr := cmd.CombinedOutput()
	text := string(out)

	switch {
	case ctx.Err() != nil:
		// Hung: the Rust panic fired on the callback thread and the Go caller is
		// still waiting for a result that will never arrive. Unobservable, which
		// is the point.
		t.Logf("confirmed: stock BAML v0.223 HANGS on `is divisibleby(0)` (killed after %s)", childDeadline)
	case runErr == nil:
		t.Fatalf("stock BAML answered `is divisibleby(0)`; the profile guard is no longer justified.\nchild output:\n%s", text)
	case strings.Contains(text, "divisor of zero"), strings.Contains(text, "remainder"):
		t.Logf("confirmed: stock BAML v0.223 aborts the process on `is divisibleby(0)` (%v)", runErr)
	default:
		t.Fatalf("child failed (%v) but not with the expected divisor-of-zero abort:\n%s", runErr, text)
	}
}

// TestStockDivisibleByZeroChild is the subprocess body. It is inert unless the
// parent sets the gate, so `go test` never trips it by accident.
func TestStockDivisibleByZeroChild(t *testing.T) {
	if os.Getenv(divByZeroChildEnv) == "" {
		t.Skip("helper for TestStockDivisibleByZeroIsUnobservable")
	}
	// Expected to abort or hang inside the CFFI before returning.
	v, err := bamlclient.Parse.Iso_divzeroFn(`{"v":4}`)
	t.Fatalf("stock returned (%v, %v) instead of aborting", v, err)
}
