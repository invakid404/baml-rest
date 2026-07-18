package admission

// Shared secret-safe test diagnostic helper (DEFAULT build, so both the default
// and the nanollm_integration-gated admission tests can use it). Admitted/Claim
// declare the prepared/exact request headers, exact body, and URL sensitive
// (admit.go), and the canonical body is request content — so a failure diagnostic
// must never emit those bytes. bodyDigest reduces any such payload to a
// length + short SHA-256, which still uniquely pins a mismatch.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// bodyDigest renders a request/canonical/plan body as "<n>B sha256:<12hex>" —
// NEVER the raw bytes.
func bodyDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%dB sha256:%s", len(b), hex.EncodeToString(sum[:])[:12])
}
