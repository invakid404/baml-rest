//go:build integration && !subprocess

package integration

// inProcessBuild reports whether the test binary was compiled without
// the `subprocess` tag. Direct in-process builds set it to true so
// tests that depend on subprocess-only semantics (kill-worker process
// death, independent worker restart) can skip themselves.
const inProcessBuild = true
