package artifactprofile

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// The S2 startup profile/mode signal, as metrics. Two series per process, both
// with predeclared bounded labels, registered on the SAME registry that already
// carries the S1 surface/cohort/winner collectors — that shared registry is the
// join: one scrape answers "which artifact is this, and what did its admission
// gate do?" without any offline correlation.
//
// Naming follows the S1 collectors (baml_rest_debaml_*), and the `_info` suffix
// follows the established `baml_rest_debaml_config_inventory_info` shape: a
// constant 1-valued gauge whose LABELS are the payload.
const (
	// ArtifactInfoMetric is the constant 1-valued identity series.
	ArtifactInfoMetric = "baml_rest_debaml_artifact_profile_info"

	// ExpectationMetric is the alertable expectation series: 1 exactly when the
	// running artifact contradicts the configured expectation.
	ExpectationMetric = "baml_rest_debaml_artifact_profile_expectation_violation"
)

// Collectors returns the S2 startup profile/mode collectors for an attestation.
// They are plain constant gauges: the attestation is resolved once at startup
// and never changes for the life of the process, so there is nothing to update
// and no per-request path that can touch them.
func Collectors(a Attestation) []prometheus.Collector {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: ArtifactInfoMetric,
		Help: "de-BAML serving-cutover S2 artifact identity: one series per process, value 1. " +
			"profile is the DERIVED profile of the running binary (native_capable = the standard " +
			"nanollmprepare-based worker; baml_only = the explicit rollback artifact), already proven " +
			"equal to the build stamp when one is present. artifact_id is a fixed-width opaque digest " +
			"of the build SELECTION AXES, or \"unstamped\". Bounded labels only: NO URLs, client/model " +
			"names, aliases, methods, prompts, headers or secrets, and nothing per-request.",
	}, []string{"profile", "artifact_id", "stamped"})
	info.WithLabelValues(string(a.Profile), a.ArtifactID, a.StampedLabel()).Set(1)

	violation := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: ExpectationMetric,
		Help: "de-BAML serving-cutover S2 artifact-profile expectation: 1 when the running artifact " +
			"contradicts " + ExpectedProfileEnv + ", 0 otherwise (including when no expectation is " +
			"configured, reported as expected=\"none\"). A 1 with expected=\"native_capable\" is the " +
			"scope's required alert: a BAML-only rollback artifact is running where the standard " +
			"native-capable artifact is expected. Booting is never blocked by this — rollback must " +
			"always work — so this series is the only thing that reports it.",
	}, []string{"expected", "actual", "alert_reason"})
	reason := a.AlertReason
	if reason == "" {
		reason = "none"
	}
	value := 0.0
	if a.ExpectationViolated() {
		value = 1
	}
	violation.WithLabelValues(a.ExpectationLabel(), string(a.Profile), reason).Set(value)

	return []prometheus.Collector{info, violation}
}

// Register registers the S2 collectors on reg. A registration failure is
// returned, never swallowed: a startup signal that silently failed to register
// is exactly the false-green this slice must not ship.
func Register(reg prometheus.Registerer, a Attestation) error {
	for _, c := range Collectors(a) {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("artifactprofile: registering artifact profile collector: %w", err)
		}
	}
	return nil
}
