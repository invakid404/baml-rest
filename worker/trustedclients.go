package worker

import (
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
)

// LoadTrustedClients parses the deployment's approved-configuration declaration
// from BAML_REST_DEBAML_TRUSTED_CLIENTS. Thin wrapper around
// trustedclients.Load so cmd/worker, workerboot and the in-process server wiring
// in cmd/serve hit the same entry point — the twin of [LoadClientDefaults].
//
// The caller decides what to do on error, but there is only one right answer: a
// malformed declaration must FAIL BOOT. Degrading to an empty set would leave a
// deployment believing a configuration class was approved when nothing was.
func LoadTrustedClients() (*trustedclients.Set, error) {
	return trustedclients.Load()
}
