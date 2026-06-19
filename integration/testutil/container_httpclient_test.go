//go:build integration

package testutil

import "testing"

// TestForwardHostHTTPClientSelector is the regression guard for the
// `http-client` matrix axis. The axis was a proven no-op: the host go-test
// process set BAML_REST_HTTP_CLIENT, but the integration setup never forwarded
// it into the container, so every arm ran as the container default (auto). This
// asserts the inverse of that no-op — when the host sets the selector, the env
// constructed for the container now CONTAINS it — and that "unset → auto" is
// preserved (no entry injected when the host has nothing set). No docker/BAML
// needed: it exercises the pure host-env → RuntimeEnv → container-env plumbing.
func TestForwardHostHTTPClientSelector(t *testing.T) {
	t.Run("host set is forwarded into container env", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "fasthttp")

		opts := SetupOptions{}
		opts.ForwardHostHTTPClientSelector()

		if got := opts.RuntimeEnv[HTTPClientSelectorEnvVar]; got != "fasthttp" {
			t.Fatalf("RuntimeEnv[%s] = %q, want %q", HTTPClientSelectorEnvVar, got, "fasthttp")
		}

		env := buildContainerEnv(opts)
		if got := env[HTTPClientSelectorEnvVar]; got != "fasthttp" {
			t.Fatalf("container env[%s] = %q, want %q (selector did not reach the container)",
				HTTPClientSelectorEnvVar, got, "fasthttp")
		}
	})

	t.Run("host unset leaves container env without the selector", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "")

		opts := SetupOptions{}
		opts.ForwardHostHTTPClientSelector()

		if _, ok := opts.RuntimeEnv[HTTPClientSelectorEnvVar]; ok {
			t.Fatalf("RuntimeEnv unexpectedly contains %s when host is unset (breaks unset → auto)",
				HTTPClientSelectorEnvVar)
		}

		env := buildContainerEnv(opts)
		if _, ok := env[HTTPClientSelectorEnvVar]; ok {
			t.Fatalf("container env unexpectedly contains %s when host is unset (breaks unset → auto)",
				HTTPClientSelectorEnvVar)
		}
	})

	t.Run("explicit RuntimeEnv pin is not clobbered by host", func(t *testing.T) {
		t.Setenv(HTTPClientSelectorEnvVar, "fasthttp")

		opts := SetupOptions{RuntimeEnv: map[string]string{HTTPClientSelectorEnvVar: "nethttp"}}
		opts.ForwardHostHTTPClientSelector()

		if got := opts.RuntimeEnv[HTTPClientSelectorEnvVar]; got != "nethttp" {
			t.Fatalf("RuntimeEnv[%s] = %q, want %q (explicit pin overridden)",
				HTTPClientSelectorEnvVar, got, "nethttp")
		}
	})
}
