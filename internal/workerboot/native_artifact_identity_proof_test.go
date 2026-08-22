//go:build nativeartifactproof

package workerboot

// De-BAML serving cutover S3a — the STANDARD-ARTIFACT arms.
//
// The S2 proof in this package boots the shipped native-capable artifacts and
// asserts the flag-on / flag-off properties of the binary itself. S3a adds one
// thing the binary can now get wrong, and it is the thing an enrollment would
// depend on: the DEPLOYMENT'S APPROVED-CONFIGURATION DECLARATION has to reach the
// deployed worker's config load. If packaging, the entrypoint, or the boot order
// dropped it, nothing would ever be sealed, no request would ever obtain an
// identity, and every in-process suite in this repo would still be green.
//
// So these arms boot the SAME artifacts with the declaration in their environment
// and read it back off the artifact's own bounded startup signal — a COUNT, never a
// name or a value — and re-assert that the artifact still claims nothing. Plus the
// two failure modes worth pinning on a real binary: nothing declared reports zero,
// and a declaration nobody can read REFUSES TO BOOT rather than coming up as if
// nothing were approved.
//
// WHY THIS LANE SENDS NO REQUEST, AND WHERE THE REQUEST PROOF IS. These two binaries
// are built from a checkout, and baml-rest's root `adapter.go` is the "overwritten
// during build" stub — `Methods` is empty until the CONTAINER build generates a client
// from the deployment's own BAML project — so they know no methods and cannot be sent
// a `/call`. The request half is proven on a binary that CAN receive one:
// cmd/serve/native_artifact_route_proof_test.go boots this same serve-profile
// entrypoint, built with one extra tag that links dynclient's committed generated
// dynamic client, and POSTs the public `/call` body through the real pool and the
// production chi handler. What THIS lane adds that that one cannot is coverage of BOTH
// shipped entrypoints — cmd/worker and cmd/worker-shadow — at their real, unmodified
// tag sets.

import (
	"strings"
	"testing"
)

// trustedClientsEnv is the deployment's approved-configuration declaration variable,
// spelled out here rather than imported so this proof pins the NAME a deployment
// actually sets.
const trustedClientsEnv = "BAML_REST_DEBAML_TRUSTED_CLIENTS"

// s3aDeclaration is a well-formed declaration of one approved configuration class.
// The values are inert — nothing in this lane sends a request — but they are a real
// configuration shape, so the artifact's decoder runs the same path a deployment's
// does.
const s3aDeclaration = `{"trusted_clients":[{"name":"ApprovedClient","fingerprint":"cfg001","provider":"openai",` +
	`"options":{"model":"gpt-4o-mini","base_url":"https://approved.invalid/v1","api_key":"sk-artifact-proof"}}]}`

// TestNativeCapableArtifactCarriesTheDeploymentDeclaration is the S3a carriage proof:
// the declaration a deployment sets reaches the SHIPPED binary's config load, and the
// artifact still claims nothing while carrying it.
func TestNativeCapableArtifactCarriesTheDeploymentDeclaration(t *testing.T) {
	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			booted, err := bootWorker(t, a.binary(t), map[string]string{trustedClientsEnv: s3aDeclaration})
			if err != nil {
				t.Fatalf("the native-capable standard artifact failed to boot with a declaration: %v\n%s", err, stderrSettled(booted))
			}
			line := startupSignal(t, booted)
			// A COUNT, not a name: the signal must confirm the declaration loaded
			// without publishing anything about the configuration it describes.
			if got := line["trusted_config_classes"]; got != float64(1) {
				t.Errorf("trusted_config_classes = %v, want 1 — the deployment's declaration did not reach the artifact's config load", got)
			}
			// The whole boot log, not just the signal line: nothing about the
			// declared configuration may be observable anywhere on it.
			bootLog := stderrSettled(booted)
			for _, forbidden := range []string{"ApprovedClient", "cfg001", "gpt-4o-mini", "approved.invalid", "sk-artifact-proof"} {
				if strings.Contains(bootLog, forbidden) {
					t.Errorf("the boot log carries %q; the declaration must be reported as a bounded count only", forbidden)
				}
			}
			// SEALING IS NOT ENROLLING, and after serving cutover S3b that is a real
			// distinction rather than a vacuous one: the shipped policy now enrolls one
			// tuple, and the class declared above is sealed under `cfg001` — a declared
			// but UNENROLLED slot — so it still resolves a bounded, non-enrolled cohort.
			// The count is asserted against the shipped manifest so an artifact that
			// gained an unreviewed second enrollment fails here.
			policy := gatheredMetric(t, booted, "baml_rest_debaml_cohort_policy_info")
			if got := policy.GetGauge().GetValue(); got != wantShippedEnrollments {
				t.Errorf("the shipped cohort policy enrolls %v pair(s) with a declaration present, want %v", got, wantShippedEnrollments)
			}
			assertNoNativeClaims(t, booted)
		})
	}
}

// TestNativeCapableArtifactWithNoDeclarationReportsNone is the control that makes the
// count above causal rather than a constant.
func TestNativeCapableArtifactWithNoDeclarationReportsNone(t *testing.T) {
	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			booted, err := bootWorker(t, a.binary(t), map[string]string{trustedClientsEnv: ""})
			if err != nil {
				t.Fatalf("the native-capable standard artifact failed to boot: %v\n%s", err, stderrSettled(booted))
			}
			if got := startupSignal(t, booted)["trusted_config_classes"]; got != float64(0) {
				t.Errorf("trusted_config_classes = %v with nothing declared, want 0", got)
			}
			assertNoNativeClaims(t, booted)
		})
	}
}

// TestNativeCapableArtifactRefusesAMalformedDeclaration pins the fail-loud rule on a
// real binary: a declaration nobody can read must stop the worker, not leave it
// serving as though nothing were approved. An operator who wrote a declaration is
// entitled to find out it did not parse.
func TestNativeCapableArtifactRefusesAMalformedDeclaration(t *testing.T) {
	// The malformed record carries HOSTILE values in every position a declaration can
	// spell: the name, the fingerprint, the option key and the option value. A boot
	// failure is exactly when someone reads logs, so none of them may appear in one.
	const (
		hostileName        = "https://secrets.example/v1?token=abc"
		hostileFingerprint = "sk-live-51H8xQhostile"
		hostileOptionKey   = "gpt-4o-acme-tuned-2026"
		hostileOptionValue = "AKIAIOSFODNN7EXAMPLE"
	)
	declaration := `{"trusted_clients":[{"name":"` + hostileName + `","fingerprint":"` + hostileFingerprint +
		`","provider":"openai","options":{"` + hostileOptionKey + `":"` + hostileOptionValue + `"}}]}`

	for _, a := range nativeArtifacts() {
		t.Run(a.name, func(t *testing.T) {
			booted, err := bootWorker(t, a.binary(t), map[string]string{trustedClientsEnv: declaration})
			if err == nil {
				t.Fatal("the artifact booted with a malformed approved-configuration declaration; a declaration nobody can read must fail boot")
			}
			got := stderrSettled(booted)
			if !strings.Contains(got, trustedClientsEnv) {
				t.Errorf("the boot failure does not name %s, so an operator cannot tell what to fix:\n%s", trustedClientsEnv, got)
			}
			// The bounded reason survives; nothing the declaration said does.
			if !strings.Contains(got, "fingerprint_not_opaque") {
				t.Errorf("the boot failure does not carry its bounded reason code:\n%s", got)
			}
			for _, secret := range []string{hostileName, hostileFingerprint, hostileOptionKey, hostileOptionValue, "trusted_clients"} {
				if strings.Contains(got, secret) {
					t.Errorf("the boot failure log carries declared configuration text %q; a rejected declaration must be described, not quoted:\n%s", secret, got)
				}
			}
		})
	}
}
