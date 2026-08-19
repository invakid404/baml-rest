//go:build nativeartifactproof

package workerboot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// De-BAML serving cutover S2 — the NATIVE-CAPABLE ARTIFACT proof.
//
// This lane boots the artifacts S2 actually promotes: the STANDARD deployable
// native-capable workers, built from the out-of-go.work nanollmprepare module
// with CGO and a linked nanollm archive, stamped exactly as cmd/build/build.sh
// stamps them. It asserts, per entrypoint:
//
//   - the startup profile signal and the RELEASE ARTIFACT ID the build computed
//     (not merely a well-formed one — the exact expected value);
//   - flag-ON with the shipped empty cohort policy: the artifact boots, serves,
//     and has enrolled NOTHING and claimed NOTHING, so BAML owns all traffic;
//   - flag-OFF: zero native runtime init and zero native factories — observable
//     as the total absence of every de-BAML admission collector, because each of
//     them is registered by a factory that only exists in the flag-on branch.
//
// WHY THIS IS ITS OWN TAG AND ITS OWN LANE, AND WHY IT NEVER SKIPS.
//
// The artifacts cannot be built here: this module is pure Go and the ordinary CI
// lane deliberately keeps nanollm out. So the binaries are supplied by the gated
// nanollm lane, which builds them and exports the env below.
//
// They are REQUIRED, not optional. An earlier version of this proof skipped when
// the env was unset, and the CI step that was supposed to supply it did not — so
// the whole native-artifact proof ran as a green skip, and a real flag-off
// kill-switch failure on cmd/worker-shadow shipped underneath it. Missing env is
// now a FAILURE: this file cannot report success without having booted something.

// The artifacts under proof, and where the lane hands them over.
const (
	serveWorkerBinEnv         = "BAML_REST_S2_NATIVE_WORKER_BIN"
	serveWorkerArtifactIDEnv  = "BAML_REST_S2_NATIVE_WORKER_ARTIFACT_ID"
	shadowWorkerBinEnv        = "BAML_REST_S2_NATIVE_SHADOW_WORKER_BIN"
	shadowWorkerArtifactIDEnv = "BAML_REST_S2_NATIVE_SHADOW_WORKER_ARTIFACT_ID"
)

// nativeArtifact is one native-capable artifact to prove.
type nativeArtifact struct {
	// name is the entrypoint this artifact was built from.
	name string
	// binEnv / idEnv name the env vars carrying the binary and the release
	// artifact ID the build computed for it.
	binEnv, idEnv string
	// flagOnRolloutMode is the rollout mode this profile reports with the
	// umbrella flag on. Both are native-capable ARTIFACTS; they differ in which
	// native lane they wire.
	flagOnRolloutMode string
	// flagOnNativeServing is the native_serving label with the flag on. Only the
	// serve profile is ever "eligible" — and even then it serves nothing while the
	// cohort policy is empty.
	flagOnNativeServing string
}

// nativeArtifacts is every entrypoint cmd/build/build.sh can ship as a
// native_capable artifact. It is deliberately the same set the source-level
// entrypoint guard enumerates: the P0 this lane exists to catch was a second
// entrypoint nobody booted.
func nativeArtifacts() []nativeArtifact {
	return []nativeArtifact{
		{
			name:                "cmd/worker",
			binEnv:              serveWorkerBinEnv,
			idEnv:               serveWorkerArtifactIDEnv,
			flagOnRolloutMode:   "serve",
			flagOnNativeServing: "eligible",
		},
		{
			name:                "cmd/worker-shadow",
			binEnv:              shadowWorkerBinEnv,
			idEnv:               shadowWorkerArtifactIDEnv,
			flagOnRolloutMode:   "shadow",
			flagOnNativeServing: "off",
		},
	}
}

// require returns the value of a required env var, failing the test when it is
// absent. It is the anti-skip: this lane cannot pass without its inputs.
func require(t *testing.T, key string) string {
	t.Helper()
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		t.Fatalf("%s is not set: this lane must BOOT the native-capable artifact, and a missing artifact is a lane misconfiguration, not a reason to report success", key)
	}
	return value
}

// binary returns the artifact's binary path, failing when it is missing.
func (a nativeArtifact) binary(t *testing.T) string {
	t.Helper()
	bin := require(t, a.binEnv)
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s=%q is not usable: %v", a.binEnv, bin, err)
	}
	return bin
}

// expectedArtifactID returns the release artifact ID the build computed for this
// artifact, validated so a lane that exported junk fails here rather than making
// the assertion below vacuous.
func (a nativeArtifact) expectedArtifactID(t *testing.T) string {
	t.Helper()
	id := strings.TrimSpace(require(t, a.idEnv))
	if err := artifactprofile.ValidateArtifactID(id); err != nil {
		t.Fatalf("%s=%q is not a release artifact ID: %v", a.idEnv, id, err)
	}
	return id
}

// TestNativeCapableArtifactAttestsItselfAndServesNothingNatively is the flag-ON
// proof for every shippable native-capable artifact.
func TestNativeCapableArtifactAttestsItselfAndServesNothingNatively(t *testing.T) {
	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			bin := a.binary(t)
			wantID := a.expectedArtifactID(t)

			booted, err := bootWorker(t, bin, nil)
			if err != nil {
				t.Fatalf("the native-capable standard artifact failed to boot: %v\n%s", err, stderrSettled(booted))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if healthy, err := booted.worker.Health(ctx); err != nil || !healthy {
				t.Fatalf("the native-capable standard artifact is not serving (healthy=%v, err=%v)", healthy, err)
			}

			line := startupSignal(t, booted)
			for field, want := range map[string]any{
				"artifact_profile":           string(artifactprofile.ProfileNativeCapable),
				"artifact_id":                wantID,
				"artifact_stamped":           true,
				"native_build_capable":       true,
				"native_runtime_initialized": true,
				"debaml_flag_enabled":        true,
				"rollout_mode":               a.flagOnRolloutMode,
				"native_serving":             a.flagOnNativeServing,
			} {
				if got := line[field]; got != want {
					t.Errorf("flag-on startup signal %s = %v, want %v", field, got, want)
				}
			}
			// Provenance reaches the startup log: an operator can join the running
			// process back to the source it was built from.
			for _, field := range []string{"artifact_source_revision", "artifact_source_bundle_digest", "artifact_native_worker_tar_digest"} {
				value, ok := line[field].(string)
				if !ok || value == "" {
					t.Errorf("startup signal is missing provenance field %q: %v", field, line[field])
				}
			}

			// The metric carries the same ID, so a dashboard and the log agree.
			info := gatheredMetric(t, booted, artifactprofile.ArtifactInfoMetric)
			if got := labelValue(info, "artifact_id"); got != wantID {
				t.Errorf("metric artifact_id = %q, want %q", got, wantID)
			}
			if got := labelValue(info, "profile"); got != string(artifactprofile.ProfileNativeCapable) {
				t.Errorf("metric profile = %q, want native_capable", got)
			}

			// THE NO-ENROLLMENT PROOF. The shipped cohort policy permits zero
			// (surface, cohort) pairs, so nothing can claim; and nothing HAS
			// claimed, which is what the counters below show.
			policy := gatheredMetric(t, booted, "baml_rest_debaml_cohort_policy_info")
			if got := policy.GetGauge().GetValue(); got != 0 {
				t.Errorf("shipped cohort policy enrolls %v (surface, cohort) pairs, want 0; S2 must not enroll anything", got)
			}
			assertNoNativeClaims(t, booted)
		})
	}
}

// TestNativeCapableArtifactUnderFlagOffDoesZeroNativeWork is the flag-OFF
// kill-switch proof for every shippable native-capable artifact — the exact
// assertion whose absence let cmd/worker-shadow ship a binary that EXITED instead
// of serving BAML when the flag was turned off.
func TestNativeCapableArtifactUnderFlagOffDoesZeroNativeWork(t *testing.T) {
	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			bin := a.binary(t)
			wantID := a.expectedArtifactID(t)

			booted, err := bootWorker(t, bin, map[string]string{"BAML_REST_USE_DEBAML": "false"})
			if err != nil {
				t.Fatalf("the flag-off native-capable artifact failed to boot: %v\n%s", err, stderrSettled(booted))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if healthy, err := booted.worker.Health(ctx); err != nil || !healthy {
				t.Fatalf("the flag-off native-capable artifact is not serving (healthy=%v, err=%v)", healthy, err)
			}

			line := startupSignal(t, booted)
			for field, want := range map[string]any{
				// Still the native-capable ARTIFACT: the profile describes what was
				// built, not what the flag permits. Deriving baml_only here would
				// contradict the build stamp and take the process down — which is
				// precisely the failure this assertion now catches.
				"artifact_profile":           string(artifactprofile.ProfileNativeCapable),
				"artifact_id":                wantID,
				"debaml_flag_enabled":        false,
				"native_build_capable":       true,
				"native_runtime_initialized": false,
				"rollout_mode":               "off",
				"native_serving":             "off",
			} {
				if got := line[field]; got != want {
					t.Errorf("flag-off startup signal %s = %v, want %v", field, got, want)
				}
			}

			// ZERO native factories ran. Every de-BAML admission collector is
			// registered by a factory constructed only inside the flag-on branch, so
			// their total absence is the runtime observation of "no native init, no
			// factory, no socket".
			allowed := map[string]bool{
				artifactprofile.ArtifactInfoMetric: true,
				artifactprofile.ExpectationMetric:  true,
			}
			names := deBAMLMetricNames(t, booted)
			for _, name := range names {
				if !allowed[name] {
					t.Errorf("flag-off native-capable artifact exposes de-BAML collector %q; a native factory ran behind the kill switch", name)
				}
			}
			// Non-vacuity: an empty de-BAML metric set would satisfy the loop above
			// while actually meaning "the metrics RPC told us nothing". The two S2
			// identity series are registered unconditionally, so their presence
			// proves this assertion looked at a real, populated metric set.
			for want := range allowed {
				found := false
				for _, name := range names {
					if name == want {
						found = true
					}
				}
				if !found {
					t.Errorf("flag-off native-capable artifact does not expose %q; the zero-native assertion above read an empty metric set", want)
				}
			}
		})
	}
}

// TestNativeCapableArtifactInARollbackSlotAlertsAndKeepsServing is the
// expectation alert on the artifact S2 promotes: a slot pinned to the rollback
// profile that receives the standard artifact must page and keep serving.
func TestNativeCapableArtifactInARollbackSlotAlertsAndKeepsServing(t *testing.T) {
	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			booted, err := bootWorker(t, a.binary(t), map[string]string{
				artifactprofile.ExpectedProfileEnv: string(artifactprofile.ProfileBAMLOnly),
			})
			if err != nil {
				t.Fatalf("the native-capable artifact refused to boot in a rollback-expecting slot: %v\n%s", err, stderrSettled(booted))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if healthy, err := booted.worker.Health(ctx); err != nil || !healthy {
				t.Fatalf("the native-capable artifact is not serving under an expectation mismatch (healthy=%v, err=%v)", healthy, err)
			}

			violation := gatheredMetric(t, booted, artifactprofile.ExpectationMetric)
			if got := violation.GetGauge().GetValue(); got != 1 {
				t.Errorf("expectation violation gauge = %v, want 1", got)
			}
			if got := labelValue(violation, "alert_reason"); got != artifactprofile.AlertStandardArtifactInRollbackSlot {
				t.Errorf("alert_reason = %q, want %q", got, artifactprofile.AlertStandardArtifactInRollbackSlot)
			}
			requireStderrContains(t, booted, "does not match the expected deployment profile")
		})
	}
}

// assertNoNativeClaims requires that every de-BAML COUNTER the freshly booted
// artifact exposes is at zero. With the cohort policy empty nothing can be
// admitted, so a freshly booted standard artifact must have claimed nothing,
// declined nothing post-socket and opened no native socket — the observable form
// of "native-capable artifact plus empty policy is BAML transport".
func assertNoNativeClaims(t *testing.T, b *bootedWorker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload, err := b.worker.GetMetrics(ctx)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	checked := 0
	for _, raw := range payload {
		var mf dto.MetricFamily
		if err := proto.Unmarshal(raw, &mf); err != nil {
			t.Fatalf("unmarshal metric family: %v", err)
		}
		if !strings.HasPrefix(mf.GetName(), "baml_rest_debaml_") || mf.GetType() != dto.MetricType_COUNTER {
			continue
		}
		for _, m := range mf.Metric {
			checked++
			if v := m.GetCounter().GetValue(); v != 0 {
				t.Errorf("de-BAML counter %s is %v on a freshly booted artifact; nothing may be claimed while the cohort policy is empty", mf.GetName(), v)
			}
		}
	}
	// A zero-counter assertion over an EMPTY set proves nothing: if every de-BAML
	// counter stopped being exported, the loop above would inspect nothing and
	// this function would report success. That is a false green in a
	// "nothing was claimed" proof, so an empty set FAILS.
	if checked == 0 {
		t.Fatalf("no de-BAML counter samples were exported by the booted artifact; the \"nothing was claimed\" assertion inspected an empty set and would pass vacuously")
	}
	t.Logf("inspected %d de-BAML counter samples, all zero", checked)
}
