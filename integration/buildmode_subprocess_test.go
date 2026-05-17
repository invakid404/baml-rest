//go:build integration && subprocess

package integration

// inProcessBuild reports whether the test binary was compiled without
// the `subprocess` tag. Subprocess builds set it to false so tests
// that assert OS-level worker isolation (process death, signal
// delivery, gRPC Unavailable on worker kill) run normally.
const inProcessBuild = false
