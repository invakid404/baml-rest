package worker

import (
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
)

// LoadClientDefaults parses the deployment-wide ClientRegistry option
// defaults from BAML_REST_CLIENT_DEFAULTS. Thin wrapper around
// clientdefaults.Load so cmd/worker and the inprocess server wiring in
// cmd/serve hit the same entry point. The caller decides what to do on
// error — the subprocess binary treats it as fatal, but that policy
// lives in cmd/worker rather than here.
func LoadClientDefaults() (*clientdefaults.Config, error) {
	return clientdefaults.Load()
}
