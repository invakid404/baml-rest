//go:build nanollm_integration

package nativeworker

// Library-level companion to the worker boot smoke (de-BAML cutover Slice 2):
// proves the nanollm-backed capability reports a stable engine identity and
// that the native runtime initializes in-process (ProbeRuntime), without
// building or booting a separate binary. The heavier cmd/worker boot smoke
// covers the full process boot + go-plugin handshake.
//
// Gated by nanollm_integration: it links nanollm (CGO), so the default no-tag
// build never needs it.

import (
	"testing"

	"github.com/invakid404/baml-rest/worker"
)

func TestNewCapabilityReportsNanollmEngine(t *testing.T) {
	var cap worker.NativeCapability = NewCapability()
	if cap == nil {
		t.Fatal("NewCapability returned nil")
	}
	if got := cap.NativeEngine(); got != EngineName {
		t.Fatalf("NativeEngine() = %q, want %q", got, EngineName)
	}
	if got := cap.NativeEngine(); got != "nanollm" {
		t.Fatalf("NativeEngine() = %q, want stable token %q", got, "nanollm")
	}
	// Version is resolved from the linked FFI; assert only that it is present
	// and secret-free-shaped (non-empty), never its exact value.
	if cap.NativeEngineVersion() == "" {
		t.Fatal("NativeEngineVersion() is empty; expected the linked crate version")
	}
}

func TestProbeRuntimeInitializesNanollm(t *testing.T) {
	// Proves nanollm's runtime comes up (engine construct + free) with a fully
	// offline config and no network I/O — the same init the worker performs at
	// startup beside BAML.
	if err := ProbeRuntime(); err != nil {
		t.Fatalf("ProbeRuntime: %v", err)
	}
	// Idempotent: a second probe must also succeed (each builds + frees its own
	// throwaway engine).
	if err := ProbeRuntime(); err != nil {
		t.Fatalf("second ProbeRuntime: %v", err)
	}
}
