package nativeserve

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestServeAndStaticServeShareOneRegistry proves the SERVE profile's two factories —
// the dynamic unary serve (New) and the static serve (NewStaticServe) — can both be
// installed on the SAME worker registry without a duplicate-collector-registration
// error (de-BAML Slice 8C P1.1). Before the reuse fix the second factory failed with
// "duplicate metrics collector registration attempted" and the flag-on serve worker
// exited before its go-plugin handshake.
func TestServeAndStaticServeShareOneRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg); err != nil {
		t.Fatalf("New(reg): %v", err)
	}
	// The SAME registry the serve profile passes to every native factory.
	if _, err := NewStaticServe(reg); err != nil {
		t.Fatalf("NewStaticServe(reg) on the shared serve registry: %v", err)
	}
	// The dynamic stream serve also coexists in the serve profile.
	if _, err := NewStream(reg); err != nil {
		t.Fatalf("NewStream(reg) on the shared serve registry: %v", err)
	}
	// Reuse must be order-independent for a defensive second pass.
	if _, err := NewStaticServe(reg); err != nil {
		t.Fatalf("second NewStaticServe(reg): %v", err)
	}
}
