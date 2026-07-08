//go:build integration

package testutil

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestBuildContainerEnvDeBAMLSelector is the regression guard for the
// integration-tests-debaml-off CI arm. De-BAML is now default-ON, so that arm
// sets BAML_REST_USE_DEBAML=false on the host go-test process to flip the WHOLE
// shared TestEnv OFF the de-BAML native dynamic response-coercion path,
// validating the disable path. The harness starts baml-rest via testcontainers
// with an explicit Env map, so unless the flag is forwarded into that map the
// arm is inert: the shared suite keeps running the server default (de-BAML ON)
// and the arm is a false signal.
//
// Forwarding lives in buildContainerEnv — the single chokepoint every
// baml-rest container's env passes through — with the SAME precedence as the
// http-client selector: the host value is copied before the opts.RuntimeEnv
// loop so a dedicated test (dynamic_debaml_rest_test.go) that pins the flag in
// RuntimeEnv still wins. These cases pin that contract. No docker/BAML needed.
func TestBuildContainerEnvDeBAMLSelector(t *testing.T) {
	t.Run("host set reaches the container env", func(t *testing.T) {
		t.Setenv(bamlutils.EnvUseDeBAML, "true")

		env := buildContainerEnv(SetupOptions{})

		if got := env[bamlutils.EnvUseDeBAML]; got != "true" {
			t.Fatalf("container env[%s] = %q, want %q (de-BAML flag did not reach the container)",
				bamlutils.EnvUseDeBAML, got, "true")
		}
	})

	t.Run("host set reaches the container env even with a fresh RuntimeEnv map", func(t *testing.T) {
		// Simulates the dedicated tests that REPLACE opts.RuntimeEnv with a
		// fresh map lacking the flag: the setup-path value would be clobbered,
		// but the chokepoint forwarding survives.
		t.Setenv(bamlutils.EnvUseDeBAML, "true")

		opts := SetupOptions{RuntimeEnv: map[string]string{"BAML_REST_CLIENT_DEFAULTS": "{}"}}
		env := buildContainerEnv(opts)

		if got := env[bamlutils.EnvUseDeBAML]; got != "true" {
			t.Fatalf("container env[%s] = %q, want %q (flag dropped by a replaced RuntimeEnv map)",
				bamlutils.EnvUseDeBAML, got, "true")
		}
		// The test's own RuntimeEnv entries must still be forwarded too.
		if got := env["BAML_REST_CLIENT_DEFAULTS"]; got != "{}" {
			t.Fatalf("container env[BAML_REST_CLIENT_DEFAULTS] = %q, want %q", got, "{}")
		}
	})

	t.Run("explicit RuntimeEnv pin wins over the host value", func(t *testing.T) {
		// The dedicated de-BAML differential (dynamic_debaml_rest_test.go) pins
		// the flag explicitly via RuntimeEnv for its own containers (both the ON
		// and OFF legs); that pin must win even under a global host
		// BAML_REST_USE_DEBAML, in either direction. Here a host ON is overridden
		// by an OFF pin.
		t.Setenv(bamlutils.EnvUseDeBAML, "true")

		opts := SetupOptions{RuntimeEnv: map[string]string{bamlutils.EnvUseDeBAML: "false"}}
		env := buildContainerEnv(opts)

		if got := env[bamlutils.EnvUseDeBAML]; got != "false" {
			t.Fatalf("container env[%s] = %q, want %q (explicit pin overridden by host env)",
				bamlutils.EnvUseDeBAML, got, "false")
		}
	})

	t.Run("host unset leaves the flag absent (container inherits server default, now de-BAML ON)", func(t *testing.T) {
		// With the host var unset/empty the forwarding must NOT inject the key,
		// so the container inherits the server default resolved by
		// DeBAMLConfigFromEnv — which is now de-BAML ON (default-on). Injecting a
		// value here would override that default and defeat the disable-only
		// contract.
		t.Setenv(bamlutils.EnvUseDeBAML, "")

		env := buildContainerEnv(SetupOptions{})

		if _, ok := env[bamlutils.EnvUseDeBAML]; ok {
			t.Fatalf("container env unexpectedly contains %s when host is unset — the container must inherit the server default (now de-BAML ON), not a host-pinned value",
				bamlutils.EnvUseDeBAML)
		}
	})
}
