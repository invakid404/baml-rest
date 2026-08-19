//go:build artifactrehearsal

package workerboot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// De-BAML serving cutover S2 — the LIVE ROLLBACK REHEARSAL.
//
// S2 promotes the native-capable worker to the standard artifact. That promotion
// is only safe if the artifact it displaces still works, so this file REHEARSES
// the rollback for real, before anything asserts the promotion: it BUILDS the
// BAML-only rollback worker, BOOTS it through the actual go-plugin handshake the
// pool uses, SERVES real RPCs on it, and reads the S2 startup profile signal back
// out of the booted process — both from its startup log and from the metrics
// channel the host merges into /metrics.
//
// It also runs the two MUTATIONS the slice must bite, against real binaries
// rather than against a stub: a mislabelled profile stamp must stop the worker
// from ever serving, and a rollback artifact running where the standard profile
// is expected must ALERT while continuing to serve.
//
// Behind the `artifactrehearsal` tag because it compiles and boots binaries: the
// default unit lane runs every test with -race -count=100, and rebuilding and
// rebooting a worker a hundred times would dominate that lane. CI runs it as its
// own step (.github/workflows/unit-tests.yml).
//
// NOT COVERED HERE, deliberately: booting the NATIVE-CAPABLE artifact. That
// binary needs CGO and a linked nanollm archive, so it cannot be produced in this
// pure-Go lane. It is proved by native_artifact_proof_test.go, which the gated
// nanollm lane runs against artifacts it builds — and which FAILS rather than
// skips if they are not supplied. An earlier version of this file carried those
// tests behind an optional env var, so CI ran them as green skips; a cold review
// caught that, and the split is what makes the native proof non-optional.

const artifactProfilePkg = "github.com/invakid404/baml-rest/internal/artifactprofile"

// repoRoot is the checkout root, two levels above internal/workerboot.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// buildRollbackWorker builds the BAML-only rollback worker (root ./cmd/worker)
// with the given artifact stamp and returns the binary path.
//
// The stamp is the FULL triple a real build emits — profile, ID and the inputs
// the ID is derived from — because the startup attestation verifies the ID
// against those inputs. A test that stamped only a profile and an ID would be
// testing a stamp shape no builder produces.
func buildRollbackWorker(t *testing.T, in *artifactprofile.Inputs, idOverride string) string {
	t.Helper()
	return buildStampedBinary(t, "./cmd/worker", in, idOverride)
}

// buildStampedBinary builds one package from the repo root with the FULL artifact
// stamp triple a real build emits — profile, ID and the inputs the ID is derived
// from — because the startup attestation verifies the ID against those inputs.
// A nil `in` builds unstamped.
func buildStampedBinary(t *testing.T, pkg string, in *artifactprofile.Inputs, idOverride string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "stamped-binary")
	args := []string{"build", "-o", out}
	if in != nil {
		id := artifactprofile.ComputeArtifactID(*in)
		if idOverride != "" {
			id = idOverride
		}
		args = append(args, "-ldflags", fmt.Sprintf(
			"-X %s.stampedProfile=%s -X %s.stampedArtifactID=%s -X %s.stampedArtifactInputs=%s",
			artifactProfilePkg, in.Profile, artifactProfilePkg, id, artifactProfilePkg, in.Marshal()))
	}
	args = append(args, pkg)

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, combined)
	}
	return out
}

// rollbackInputs is the build selection a real BAML-only rollback build emits.
func rollbackInputs() artifactprofile.Inputs {
	return artifactprofile.Inputs{
		Profile:               artifactprofile.ProfileBAMLOnly,
		WorkerPackage:         "root:./cmd/worker/",
		BuildTags:             "subprocess",
		Subprocess:            true,
		BAMLVersion:           "0.223.0",
		AdapterVersion:        "v0.219.0",
		SourceRevision:        "27af8af5ae04",
		SourceBundleDigest:    "1122334455667788",
		NativeWorkerTarDigest: "99aabbccddeeff00",
	}
}

// TestRollbackArtifactBootsAndServes is the rehearsal itself, and it runs BEFORE
// anything in this slice asserts the promotion: the BAML-only rollback artifact
// builds, completes the real go-plugin handshake, answers a health RPC, and
// reports the S2 artifact identity on both the startup log and the metrics
// channel. If this ever fails, promoting the native-capable artifact has no
// rollback to fall back to.
func TestRollbackArtifactBootsAndServes(t *testing.T) {
	in := rollbackInputs()
	bin := buildRollbackWorker(t, &in, "")
	booted, err := bootWorker(t, bin, nil)
	if err != nil {
		t.Fatalf("the BAML-only rollback artifact failed to boot: %v\n%s", err, stderrSettled(booted))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	healthy, err := booted.worker.Health(ctx)
	if err != nil {
		t.Fatalf("Health RPC on the rollback artifact: %v", err)
	}
	if !healthy {
		t.Fatal("the rollback artifact booted but reports itself unhealthy")
	}

	line := startupSignal(t, booted)
	for field, want := range map[string]any{
		"artifact_profile":          string(artifactprofile.ProfileBAMLOnly),
		"artifact_id":               artifactprofile.ComputeArtifactID(in),
		"artifact_stamped":          true,
		"expected_artifact_profile": artifactprofile.ExpectationNone,
		"native_build_capable":      false,
		"rollout_mode":              "off",
		"native_serving":            "off",
	} {
		if got := line[field]; got != want {
			t.Errorf("startup signal %s = %v, want %v", field, got, want)
		}
	}

	info := gatheredMetric(t, booted, artifactprofile.ArtifactInfoMetric)
	if got := labelValue(info, "profile"); got != string(artifactprofile.ProfileBAMLOnly) {
		t.Errorf("metric profile label = %q, want %q", got, artifactprofile.ProfileBAMLOnly)
	}
	if got := labelValue(info, "artifact_id"); got != artifactprofile.ComputeArtifactID(in) {
		t.Errorf("metric artifact_id label = %q, want %q", got, artifactprofile.ComputeArtifactID(in))
	}
	if got := labelValue(info, "stamped"); got != "true" {
		t.Errorf("metric stamped label = %q, want \"true\"", got)
	}
	if got := info.GetGauge().GetValue(); got != 1 {
		t.Errorf("artifact info gauge = %v, want 1", got)
	}
}

// TestRollbackArtifactUnderFlagOffStaysTotalBAML pins the kill-switch contract on
// the rollback lane: with BAML_REST_USE_DEBAML=false the artifact still boots and
// serves, and reports no native capability, no native lane and no native serving.
func TestRollbackArtifactUnderFlagOffStaysTotalBAML(t *testing.T) {
	in := rollbackInputs()
	bin := buildRollbackWorker(t, &in, "")
	booted, err := bootWorker(t, bin, map[string]string{"BAML_REST_USE_DEBAML": "false"})
	if err != nil {
		t.Fatalf("flag-off rollback artifact failed to boot: %v\n%s", err, stderrSettled(booted))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if healthy, err := booted.worker.Health(ctx); err != nil || !healthy {
		t.Fatalf("flag-off rollback artifact is not serving (healthy=%v, err=%v)", healthy, err)
	}

	line := startupSignal(t, booted)
	for field, want := range map[string]any{
		"debaml_flag_enabled":        false,
		"native_build_capable":       false,
		"native_runtime_initialized": false,
		"rollout_mode":               "off",
		"native_serving":             "off",
		"artifact_profile":           string(artifactprofile.ProfileBAMLOnly),
	} {
		if got := line[field]; got != want {
			t.Errorf("flag-off startup signal %s = %v, want %v", field, got, want)
		}
	}
}

// TestRollbackIntoAStandardSlotAlertsAndKeepsServing is the ALERT the scope
// requires, exercised live: a BAML-only artifact running where the standard
// native-capable profile is expected must page AND keep serving. Turning this
// into a refusal to boot would make a rollback impossible in exactly the slot
// where one is most likely to be needed.
func TestRollbackIntoAStandardSlotAlertsAndKeepsServing(t *testing.T) {
	in := rollbackInputs()
	bin := buildRollbackWorker(t, &in, "")
	booted, err := bootWorker(t, bin, map[string]string{
		artifactprofile.ExpectedProfileEnv: string(artifactprofile.ProfileNativeCapable),
	})
	if err != nil {
		t.Fatalf("rollback artifact refused to boot in a standard-expecting slot: %v\n%s", err, stderrSettled(booted))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if healthy, err := booted.worker.Health(ctx); err != nil || !healthy {
		t.Fatalf("rollback artifact is not serving under an expectation mismatch (healthy=%v, err=%v)", healthy, err)
	}

	violation := gatheredMetric(t, booted, artifactprofile.ExpectationMetric)
	if got := violation.GetGauge().GetValue(); got != 1 {
		t.Errorf("expectation violation gauge = %v, want 1", got)
	}
	if got := labelValue(violation, "expected"); got != string(artifactprofile.ProfileNativeCapable) {
		t.Errorf("violation expected label = %q, want %q", got, artifactprofile.ProfileNativeCapable)
	}
	if got := labelValue(violation, "alert_reason"); got != artifactprofile.AlertRollbackArtifactInStandardSlot {
		t.Errorf("violation alert_reason label = %q, want %q", got, artifactprofile.AlertRollbackArtifactInStandardSlot)
	}

	requireStderrContains(t, booted, "does not match the expected deployment profile")

	// Both the identity record and the alert record are on the log now, and they
	// share a message prefix. startupSignal must still return the IDENTITY one —
	// selecting the alert would silently drop every flag/lane field an assertion
	// might ask for.
	line := startupSignal(t, booted)
	if _, ok := line["rollout_mode"]; !ok {
		t.Errorf("startupSignal returned a record without rollout_mode; it selected the alert record, not the identity record: %v", line)
	}
	if got := line["artifact_profile"]; got != string(artifactprofile.ProfileBAMLOnly) {
		t.Errorf("identity record artifact_profile = %v, want %q", got, artifactprofile.ProfileBAMLOnly)
	}
}

// TestMislabelledArtifactRefusesToServe is the live MUTATION BITE. It stamps the
// BAML-only worker as native_capable — the exact mislabelling that would put a
// rollback artifact on a dashboard as the standard one — and requires that the
// resulting binary never completes the handshake. The whole S2 signal is only
// worth anything if a build cannot lie about it.
func TestMislabelledArtifactRefusesToServe(t *testing.T) {
	mislabelled := rollbackInputs()
	mislabelled.Profile = artifactprofile.ProfileNativeCapable
	bin := buildRollbackWorker(t, &mislabelled, "")
	booted, err := bootWorker(t, bin, nil)
	if err == nil {
		t.Fatal("a BAML-only worker stamped native_capable completed the handshake and would have served")
	}
	requireStderrContains(t, booted, "artifact profile attestation failed")
}

// TestWellFormedButWrongArtifactIDRefusesToServe is the live half of the
// artifact-ID verification. The ID is stamped as a perfectly well-formed 16-hex
// token that simply is not the one this build's inputs produce — the shape an
// edited, replayed or hand-written stamp takes. Before the ID was verified
// against its own stamped inputs, this booted and served happily while reporting
// someone else's release identity.
func TestWellFormedButWrongArtifactIDRefusesToServe(t *testing.T) {
	in := rollbackInputs()
	wrong := "0123456789abcdef"
	if wrong == artifactprofile.ComputeArtifactID(in) {
		t.Fatal("test setup picked the correct ID; it would prove nothing")
	}
	if err := artifactprofile.ValidateArtifactID(wrong); err != nil {
		t.Fatalf("test setup picked a malformed ID (%v); the point is a WELL-FORMED wrong ID", err)
	}

	bin := buildRollbackWorker(t, &in, wrong)
	booted, err := bootWorker(t, bin, nil)
	if err == nil {
		t.Fatal("a worker stamped with an artifact ID its own inputs do not produce completed the handshake and would have served")
	}
	requireStderrContains(t, booted, "artifact profile attestation failed")
}

// TestUnstampedArtifactStillBoots pins the do-not-hard-break rule against a real
// binary: a worker built without the builder's -ldflags stamp — a hand-built
// binary, or any deploy path this repository does not reveal — makes no claim,
// boots normally, and attests its derived profile with an "unstamped" ID.
func TestUnstampedArtifactStillBoots(t *testing.T) {
	bin := buildRollbackWorker(t, nil, "")
	booted, err := bootWorker(t, bin, nil)
	if err != nil {
		t.Fatalf("an unstamped worker failed to boot: %v\n%s", err, stderrSettled(booted))
	}

	line := startupSignal(t, booted)
	if got := line["artifact_id"]; got != artifactprofile.UnstampedArtifactID {
		t.Errorf("unstamped artifact_id = %v, want %q", got, artifactprofile.UnstampedArtifactID)
	}
	if got := line["artifact_stamped"]; got != false {
		t.Errorf("unstamped artifact_stamped = %v, want false", got)
	}
	if got := line["artifact_profile"]; got != string(artifactprofile.ProfileBAMLOnly) {
		t.Errorf("unstamped artifact_profile = %v, want %q", got, artifactprofile.ProfileBAMLOnly)
	}
}

// attestOrderFixture is the smallest native-capable worker (see
// testdata/attestorder): it advertises the static build capability and supplies a
// NativeInit that records only that it ran.
const attestOrderFixture = "./internal/workerboot/testdata/attestorder"

// attestOrderInputs is the build selection stamped onto that fixture.
func attestOrderInputs() artifactprofile.Inputs {
	in := rollbackInputs()
	in.Profile = artifactprofile.ProfileNativeCapable
	in.WorkerPackage = "testdata:./attestorder"
	return in
}

// runAttestOrderFixture builds the fixture with the given stamp, runs it, and
// reports whether its NativeInit ran.
func runAttestOrderFixture(t *testing.T, in artifactprofile.Inputs) (nativeInitRan bool, stderr string) {
	t.Helper()

	bin := buildStampedBinary(t, attestOrderFixture, &in, "")
	sentinel := filepath.Join(t.TempDir(), "native-init-ran")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "WORKERBOOT_NATIVE_INIT_SENTINEL="+sentinel)
	out, _ := cmd.CombinedOutput()

	if _, err := os.Stat(sentinel); err == nil {
		return true, string(out)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
	return false, string(out)
}

// TestAttestationRefusesBeforeNativeRuntimeInit pins the ORDER of the fail-closed
// path: a binary whose build stamp contradicts what it is must refuse BEFORE the
// native runtime is initialized, not after.
//
// Both inputs of the attestation are static — the Options the entry point passed
// and the linker stamp — so nothing is gained by waiting, and waiting costs
// something real: a mislabelled artifact would first run nanollm.New (and
// whatever it allocates and links) and only then decline to serve.
//
// The exit code cannot show this: a correctly-stamped fixture also exits non-zero,
// because it has no go-plugin handshake to complete. The sentinel file its
// NativeInit writes is the discriminator, and the second half below is what keeps
// the first from passing vacuously.
func TestAttestationRefusesBeforeNativeRuntimeInit(t *testing.T) {
	t.Run("mislabelled artifact never initializes the native runtime", func(t *testing.T) {
		in := attestOrderInputs()
		// The fixture advertises NativeBuildCapable, so it IS native_capable.
		// Stamp it baml_only: a contradiction the attestation must catch.
		in.Profile = artifactprofile.ProfileBAMLOnly

		ran, out := runAttestOrderFixture(t, in)
		if ran {
			t.Errorf("NativeInit ran on a mislabelled artifact; attestation must refuse BEFORE any native runtime work\n%s", out)
		}
		if !strings.Contains(out, "artifact profile attestation failed") {
			t.Errorf("mislabelled fixture did not report an attestation failure:\n%s", out)
		}
	})

	t.Run("a well-formed but wrong artifact ID also refuses first", func(t *testing.T) {
		in := attestOrderInputs()
		wrong := "0123456789abcdef"
		if wrong == artifactprofile.ComputeArtifactID(in) {
			t.Fatal("test setup picked the correct ID; it would prove nothing")
		}
		bin := buildStampedBinary(t, attestOrderFixture, &in, wrong)
		sentinel := filepath.Join(t.TempDir(), "native-init-ran")
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "WORKERBOOT_NATIVE_INIT_SENTINEL="+sentinel)
		out, _ := cmd.CombinedOutput()

		if _, err := os.Stat(sentinel); err == nil {
			t.Errorf("NativeInit ran despite a wrong artifact ID\n%s", out)
		}
		if !strings.Contains(string(out), "artifact profile attestation failed") {
			t.Errorf("wrong-ID fixture did not report an attestation failure:\n%s", out)
		}
	})

	// NON-VACUITY. Without this, "the sentinel is absent" would also be satisfied
	// by a fixture whose NativeInit never runs under ANY stamp — a broken test
	// that always passes. A correctly-stamped fixture must reach NativeInit.
	t.Run("a correctly stamped artifact does reach the native runtime init", func(t *testing.T) {
		ran, out := runAttestOrderFixture(t, attestOrderInputs())
		if !ran {
			t.Fatalf("NativeInit did not run on a correctly stamped artifact; the refusal assertions above would be vacuous\n%s", out)
		}
	})
}
