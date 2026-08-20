package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// De-BAML serving cutover S2 — the HOST startup profile/mode signal.
//
// These run in the DEFAULT (untagged) build, so hostEmbeddedWorkerNativeCapable
// is false here and the host attests the BAML-only rollback profile. The
// native-capable half is asserted by the tag-split sibling
// (artifact_profile_native_test.go), which CI builds with
// -tags=nativeworkerartifact.

// logLines decodes the JSON log lines zerolog wrote to buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON (%q): %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findLog returns the first log line whose "message" contains substr.
func findLog(lines []map[string]any, substr string) map[string]any {
	for _, l := range lines {
		if msg, ok := l["message"].(string); ok && strings.Contains(msg, substr) {
			return l
		}
	}
	return nil
}

// TestHostAttestsItsOwnProfile pins the host half of the S2 signal: the serve
// binary reports the artifact identity it DERIVES from its own build tag, not
// something the worker told it over the plugin channel, and publishes it on the
// registry the combined /metrics gatherer already merges with each worker's.
func TestHostAttestsItsOwnProfile(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	reg := prometheus.NewRegistry()

	att, err := attestArtifactProfile(logger, reg, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("attestArtifactProfile: %v", err)
	}
	if want := artifactprofile.DeriveProfile(hostEmbeddedWorkerNativeCapable); att.Profile != want {
		t.Errorf("Profile = %q, want %q", att.Profile, want)
	}
	if att.ExpectationViolated() {
		t.Errorf("expectation violated with no expectation configured")
	}

	line := findLog(logLines(t, &buf), "de-BAML serve artifact profile")
	if line == nil {
		t.Fatal("no artifact profile line on the startup log")
	}
	for _, field := range []string{"artifact_profile", "artifact_id", "artifact_stamped", "expected_artifact_profile"} {
		if _, ok := line[field]; !ok {
			t.Errorf("startup profile signal is missing field %q: %v", field, line)
		}
	}
	if got := line["artifact_profile"]; got != string(att.Profile) {
		t.Errorf("logged artifact_profile = %v, want %q", got, att.Profile)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var names []string
	for _, mf := range families {
		names = append(names, mf.GetName())
	}
	for _, want := range []string{artifactprofile.ArtifactInfoMetric, artifactprofile.ExpectationMetric} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("metric %q was not registered by the host; gathered %v", want, names)
		}
	}
}

// TestHostExpectationMismatchAlertsAndStillBoots is the scope's required alert,
// on the host side: a BAML-only artifact in a slot that expects the standard
// profile must LOG AN ALERT and keep booting. Turning this into a startup
// failure would break the rollback lane, which is the one thing
// BAML_REST_USE_DEBAML=false's "total revert" promise depends on.
func TestHostExpectationMismatchAlertsAndStillBoots(t *testing.T) {
	expected := string(artifactprofile.ProfileNativeCapable)
	if hostEmbeddedWorkerNativeCapable {
		expected = string(artifactprofile.ProfileBAMLOnly)
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	att, err := attestArtifactProfile(logger, prometheus.NewRegistry(), func(k string) (string, bool) {
		if k == artifactprofile.ExpectedProfileEnv {
			return expected, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("attestArtifactProfile refused to boot on an expectation mismatch: %v", err)
	}
	if !att.ExpectationViolated() {
		t.Fatalf("expectation %q against profile %q did not violate", expected, att.Profile)
	}

	alert := findLog(logLines(t, &buf), "does not match the expected deployment profile")
	if alert == nil {
		t.Fatal("no alert line for an artifact/expectation mismatch")
	}
	if alert["level"] != "error" {
		t.Errorf("alert level = %v, want error (it must page)", alert["level"])
	}
	if alert["alert_reason"] != att.AlertReason {
		t.Errorf("alert_reason = %v, want %q", alert["alert_reason"], att.AlertReason)
	}
}

// TestHostAttestationFailsClosed pins that attestation errors are RETURNED, not
// logged-and-ignored: the production wrapper turns the returned error into a
// Fatal before the pool starts, so an artifact with an unprovable identity never
// accepts traffic.
func TestHostAttestationFailsClosed(t *testing.T) {
	logger := zerolog.New(&bytes.Buffer{})

	// A malformed operator expectation is a configuration error, not a silently
	// ignored string.
	if _, err := attestArtifactProfile(logger, prometheus.NewRegistry(), func(k string) (string, bool) {
		if k == artifactprofile.ExpectedProfileEnv {
			return "native", true
		}
		return "", false
	}); err == nil {
		t.Error("attestArtifactProfile accepted a malformed expectation")
	}

	// A registration failure (here: a registry that already carries the
	// collectors) must surface too — a startup signal that silently failed to
	// register is a false green.
	reg := prometheus.NewRegistry()
	noEnv := func(string) (string, bool) { return "", false }
	if _, err := attestArtifactProfile(logger, reg, noEnv); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := attestArtifactProfile(logger, reg, noEnv); err == nil {
		t.Error("attestArtifactProfile swallowed a duplicate metric registration")
	}
}
