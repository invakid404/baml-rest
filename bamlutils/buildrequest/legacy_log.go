package buildrequest

import (
	"sync"

	"github.com/invakid404/baml-rest/bamlutils"
)

// legacyAlertKey identifies a (method, reason, provider) classification for
// dedup purposes. Each unique key produces at most one warn-level log per
// process lifetime.
type legacyAlertKey struct {
	method   string
	reason   string
	provider string
}

// legacyAlertSeen tracks classification keys we've already warned about.
// Per-replica dedup is sufficient — replicas come up independently and a
// rolling deploy will see one warn per replica at most, which is the
// observable signal operators want.
var legacyAlertSeen sync.Map // map[legacyAlertKey]struct{}

// LogLegacyClassification emits a once-per-process warning for legacy-path
// classifications that almost certainly indicate a deployment problem.
// Deliberate-configuration reasons (roundrobin, all-legacy, feature flag
// off) are skipped — operators expect those.
//
// Called by the request entry point (router) after computing the planned
// metadata so the warning has the same view of the request as the headers
// and metadata events do.
func LogLegacyClassification(adapter bamlutils.Adapter, method string, plan *bamlutils.Metadata) {
	if plan == nil || plan.Path != "legacy" {
		return
	}
	switch plan.PathReason {
	case PathReasonEmptyProvider,
		PathReasonUnsupportedProvider,
		PathReasonFallbackEmptyChain,
		PathReasonFallbackEmptyChildProvider:
		// fall through — these are the alertable classifications
	default:
		return
	}

	logger := adapter.Logger()
	if logger == nil {
		return
	}

	key := legacyAlertKey{method: method, reason: plan.PathReason, provider: plan.Provider}
	if _, loaded := legacyAlertSeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}

	logger.Warn("baml-rest: routing request through legacy path",
		"method", method,
		"reason", plan.PathReason,
		"client", plan.Client,
		"provider", plan.Provider,
	)
}
