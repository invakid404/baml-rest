package nativeserve

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNew_ReturnsServeFunc proves the public constructor returns a usable, non-nil
// bamlutils.NativeServeFunc on a fresh registry with no error — the exact value a
// dynclient consumer passes to dynclient.WithNativeServeComparator. This is the
// standalone (in-module) smoke test; the end-to-end behaviour through dynclient is
// proven by the gated parity test in nanollmprepare/dynamic.
func TestNew_ReturnsServeFunc(t *testing.T) {
	fn, err := New(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("New on a fresh registry: unexpected error %v", err)
	}
	if fn == nil {
		t.Fatal("New returned a nil serve func; consumers would install a no-op native callback")
	}
}

// TestNew_SurfacesRegistrationError proves the constructor propagates a collector
// registration failure instead of swallowing it: registering the bounded de-BAML
// collectors twice on the SAME registry must return a non-nil error (a duplicate
// registration), so a misconfigured consumer fails loudly rather than silently
// running without metrics.
func TestNew_SurfacesRegistrationError(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg); err != nil {
		t.Fatalf("first New: unexpected error %v", err)
	}
	_, err := New(reg) // same registry -> duplicate collector registration
	if err == nil {
		t.Fatal("New on an already-populated registry must return an error, got nil")
	}
	var are prometheus.AlreadyRegisteredError
	if !errors.As(err, &are) {
		t.Errorf("err = %v, want a prometheus.AlreadyRegisteredError", err)
	}
}
