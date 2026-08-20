package main

import (
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// De-BAML serving-cutover S2 — the HOST half of the artifact attestation.
//
// The deployable unit is the serve binary plus the worker bytes embedded in it,
// so the host attests the artifact profile too, from its own compile-time fact
// (hostEmbeddedWorkerNativeCapable) rather than from anything the worker reports
// over the plugin channel. Host and worker are stamped by the same build with
// the same values, so the two attestations are independent checks of one claim:
// a build that embedded the wrong worker, or set the wrong tag, contradicts its
// own stamp on one side or the other and fails there.
//
// The host signal is registered on prometheus.DefaultRegisterer, which the
// combined /metrics gatherer already merges with each worker's private registry
// (tagging them process="main" / process="worker_N"). So one scrape carries the
// host artifact identity, every worker's artifact identity, and the S1
// surface/cohort admission telemetry.

// attestArtifactProfile resolves this serve binary's artifact identity, emits
// the S2 startup profile/mode signal, raises the expectation alert, and
// publishes the bounded collectors on reg.
//
// It returns an error ONLY for an unproven identity (a stamp that contradicts
// the binary, a malformed stamp, an invalid expectation setting, or a failed
// registration). An expectation MISMATCH is not an error: it alerts and boots,
// because the BAML-only artifact is the rollback lane and rolling back into a
// slot that still expects the standard profile must always work.
func attestArtifactProfile(logger zerolog.Logger, reg prometheus.Registerer, lookupEnv func(string) (string, bool)) (artifactprofile.Attestation, error) {
	att, err := artifactprofile.Attest(
		artifactprofile.DeriveProfile(hostEmbeddedWorkerNativeCapable), lookupEnv)
	if err != nil {
		return artifactprofile.Attestation{}, err
	}

	logger.Info().
		Str("artifact_profile", string(att.Profile)).
		Str("artifact_id", att.ArtifactID).
		Bool("artifact_stamped", att.Stamped).
		Str("artifact_source_revision", att.SourceRevision()).
		Str("artifact_source_bundle_digest", att.SourceBundleDigest()).
		Str("artifact_native_worker_tar_digest", att.NativeWorkerTarDigest()).
		Str("expected_artifact_profile", att.ExpectationLabel()).
		Bool("native_stream_serve_capable", nativeStreamServeCapable).
		Msg("de-BAML serve artifact profile")

	if att.ExpectationViolated() {
		logger.Error().
			Str("artifact_profile", string(att.Profile)).
			Str("expected_artifact_profile", att.ExpectationLabel()).
			Str("artifact_id", att.ArtifactID).
			Str("alert_reason", att.AlertReason).
			Msg("de-BAML serve artifact profile does not match the expected deployment profile")
	}

	if err := artifactprofile.Register(reg, att); err != nil {
		return artifactprofile.Attestation{}, err
	}
	return att, nil
}

// attestArtifactProfileAtStartup is the production entry point: the default
// registry and the real environment. A failure is fatal — an artifact whose
// identity cannot be proven must not serve — and Fatal here exits before the
// pool starts, so no traffic is ever accepted under an unproven identity.
func attestArtifactProfileAtStartup(logger zerolog.Logger) {
	if _, err := attestArtifactProfile(logger, prometheus.DefaultRegisterer, os.LookupEnv); err != nil {
		logger.Fatal().Err(err).Msg("de-BAML serve artifact profile attestation failed; refusing to serve under an unproven artifact identity")
	}
}
