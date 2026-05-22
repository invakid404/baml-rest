package testharness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// RunGoBuild shells out to `go build ./...` in `dir`. The matrix is
// rendered into a single file, so a single build invocation type-
// checks every cell in one shot. Any failure (undeclared identifier,
// arg-count mismatch, type mismatch) surfaces as a build error and
// fails the outer test with the captured subprocess output.
func RunGoBuild(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("matrix temp module failed to compile (the rendered codegen output type-checks against bamlutils + fixtures):\n%s\nerror: %v", out, err)
	}
}

// RunGoTest shells out to `go test -count=1` in `dir`, narrowing the
// run to tests matching `runPattern` when non-empty. Returns the
// combined stdout/stderr plus the subprocess error (nil on green
// run). Callers — the pool-lifecycle harness in particular — decide
// whether a non-nil error is expected (regression-seed paths expect
// the inner test to FAIL).
//
// For richer invocations (build tags, fuzz, custom env, -race), use
// RunGoTestArgs.
func RunGoTest(t *testing.T, dir, runPattern string) (string, error) {
	t.Helper()
	return RunGoTestArgs(t, dir, RunGoTestOptions{Run: runPattern})
}

// RunGoTestOptions captures the knobs the broader fuzz harness needs
// when invoking `go test` against a temp module: build tags, fuzz
// target selection, additional environment variables, race detector
// toggling. Empty fields drop their flags entirely so the default
// invocation collapses back to `go test -count=1 ./...`.
type RunGoTestOptions struct {
	// Run is passed as `-run <pattern>`. Empty omits the flag.
	Run string
	// Fuzz is passed as `-fuzz <pattern>`. Empty omits the flag.
	Fuzz string
	// Tags is passed as `-tags <list>`. Empty omits the flag.
	Tags string
	// Race toggles `-race`.
	Race bool
	// Count overrides `-count`. Zero (the default) becomes 1.
	Count int
	// Env are extra environment entries appended after os.Environ()
	// + GOWORK=off. Any GOWORK= entry is silently dropped so the
	// harness's GOWORK=off contract is non-overridable.
	Env []string
}

// RunGoTestArgs invokes `go test` in `dir` with the options encoded
// by `opts`. Returns combined stdout/stderr + the subprocess error.
// GOWORK=off is always set so the temp module's go.mod is the sole
// resolution source.
func RunGoTestArgs(t *testing.T, dir string, opts RunGoTestOptions) (string, error) {
	t.Helper()
	count := opts.Count
	if count == 0 {
		count = 1
	}
	args := []string{"test", fmt.Sprintf("-count=%d", count)}
	if opts.Race {
		args = append(args, "-race")
	}
	if opts.Tags != "" {
		args = append(args, "-tags", opts.Tags)
	}
	if opts.Run != "" {
		args = append(args, "-run", opts.Run)
	}
	if opts.Fuzz != "" {
		args = append(args, "-fuzz", opts.Fuzz)
		// `go test -fuzz` accepts exactly one package; "./..."
		// would fail with multi-package matches. Pin the target
		// to the temp module root.
		args = append(args, ".")
	} else {
		args = append(args, "./...")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GOWORK=off")
	// Drop any caller-supplied GOWORK= entry so the harness's
	// "always GOWORK=off" contract holds.
	for _, e := range opts.Env {
		if strings.HasPrefix(e, "GOWORK=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
