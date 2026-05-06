package buildrequest

import (
	"os"
	"strings"
)

// parseCallUnsupportedProvidersEnv reads BAML_REST_CALL_UNSUPPORTED_PROVIDERS
// as a comma-separated list and returns the providers marked as
// call-unsupported. Whitespace around each entry is trimmed; empty entries
// are ignored. Extracted so the test can verify parsing without the cache.
func parseCallUnsupportedProvidersEnv() map[string]bool {
	raw := os.Getenv("BAML_REST_CALL_UNSUPPORTED_PROVIDERS")
	if raw == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	return set
}
