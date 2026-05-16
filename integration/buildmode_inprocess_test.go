//go:build integration && inprocess

package integration

// inProcessBuild reports whether the test binary was compiled with the
// `inprocess` tag. Inprocess builds set it to true so tests that
// depend on subprocess-only semantics (kill-worker process death,
// independent worker restart) can skip themselves.
const inProcessBuild = true
