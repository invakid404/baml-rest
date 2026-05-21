package testharness

import (
	"os"
	"os/exec"
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
func RunGoTest(t *testing.T, dir, runPattern string) (string, error) {
	t.Helper()
	args := []string{"test", "-count=1"}
	if runPattern != "" {
		args = append(args, "-run", runPattern)
	}
	args = append(args, "./...")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
