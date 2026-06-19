//go:build integration

package testutil

import "testing"

// TestBuildContainerEnvHTTPClientSelector is the regression guard for the
// `http-client` matrix axis. The axis was a proven no-op: the host go-test
// process set BAML_REST_HTTP_CLIENT, but the integration setup never forwarded
// it into the container, so every arm ran as the container default (auto).
//
// Forwarding lives in buildContainerEnv — the single chokepoint every
// baml-rest container's env passes through — rather than in matrixSetupOptions,
// because dedicated tests REPLACE opts.RuntimeEnv with a fresh map after
// calling matrixSetupOptions and would silently drop a setup-path injection
// back to auto. These cases pin that contract. No docker/BAML needed.
func TestBuildContainerEnvHTTPClientSelector(t *testing.T) {
	t.Run("host set reaches the container env", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "fasthttp")

		env := buildContainerEnv(SetupOptions{})

		if got := env[HTTPClientSelectorEnvVar]; got != "fasthttp" {
			t.Fatalf("container env[%s] = %q, want %q (selector did not reach the container)",
				HTTPClientSelectorEnvVar, got, "fasthttp")
		}
	})

	t.Run("host set reaches the container env even with a fresh RuntimeEnv map", func(t *testing.T) {
		// Simulates the dedicated tests (client_defaults_test.go,
		// bridge_test.go, dynamic_output_format_preserve_order_test.go) that
		// REPLACE opts.RuntimeEnv with a fresh map lacking the selector. This is
		// the exact regression Codex flagged: a setup-path injection would be
		// clobbered here, but the chokepoint forwarding survives.
		t.Setenv(HTTPClientSelectorEnvVar, "nethttp")

		opts := SetupOptions{RuntimeEnv: map[string]string{"BAML_REST_CLIENT_DEFAULTS": "{}"}}
		env := buildContainerEnv(opts)

		if got := env[HTTPClientSelectorEnvVar]; got != "nethttp" {
			t.Fatalf("container env[%s] = %q, want %q (selector dropped by a replaced RuntimeEnv map)",
				HTTPClientSelectorEnvVar, got, "nethttp")
		}
		// The test's own RuntimeEnv entries must still be forwarded too.
		if got := env["BAML_REST_CLIENT_DEFAULTS"]; got != "{}" {
			t.Fatalf("container env[BAML_REST_CLIENT_DEFAULTS] = %q, want %q", got, "{}")
		}
	})

	t.Run("explicit RuntimeEnv pin wins over the host value", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "fasthttp")

		opts := SetupOptions{RuntimeEnv: map[string]string{HTTPClientSelectorEnvVar: "nethttp"}}
		env := buildContainerEnv(opts)

		if got := env[HTTPClientSelectorEnvVar]; got != "nethttp" {
			t.Fatalf("container env[%s] = %q, want %q (explicit pin overridden by host env)",
				HTTPClientSelectorEnvVar, got, "nethttp")
		}
	})

	t.Run("host unset leaves the selector absent (unset → auto)", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "")

		env := buildContainerEnv(SetupOptions{})

		if _, ok := env[HTTPClientSelectorEnvVar]; ok {
			t.Fatalf("container env unexpectedly contains %s when host is unset (breaks unset → auto)",
				HTTPClientSelectorEnvVar)
		}
	})
}
