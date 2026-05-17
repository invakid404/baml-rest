//go:build integration && inprocess

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestInProcessDebugSurface asserts the inprocess-build contract for the
// two /_debug endpoints whose semantics change in single-process mode.
//
// /_debug/in-flight: the pool is force-collapsed to one worker, so a
// healthy server reports exactly one entry. This guards against the
// `force-1` invariant silently regressing.
//
// /_debug/native-stacks: there is no separate worker process to attach
// to, so pool/native_stacks_inprocess.go returns a per-worker stub
// error. The integration test asserts the stub fires and the error
// text is the one operators see, so the contract stays observable.
func TestInProcessDebugSurface(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inFlight, err := BAMLClient.GetInFlightStatus(ctx)
	if err != nil {
		t.Fatalf("GetInFlightStatus failed: %v", err)
	}
	if got := len(inFlight.Workers); got != 1 {
		t.Errorf("expected exactly 1 worker in inprocess mode, got %d (workers=%+v)", got, inFlight.Workers)
	}
	if len(inFlight.Workers) > 0 && !inFlight.Workers[0].Healthy {
		t.Errorf("expected the single inprocess worker to be healthy, got %+v", inFlight.Workers[0])
	}

	native, err := BAMLClient.GetNativeStacks(ctx)
	if err != nil {
		t.Fatalf("GetNativeStacks failed: %v", err)
	}
	if got := len(native.Workers); got != 1 {
		t.Fatalf("expected exactly 1 worker result from native-stacks in inprocess mode, got %d (result=%+v)", got, native)
	}
	const wantSubstr = "native worker stacks are unavailable in inprocess builds"
	if !strings.Contains(native.Workers[0].Error, wantSubstr) {
		t.Errorf("native-stacks worker error = %q, want substring %q", native.Workers[0].Error, wantSubstr)
	}
}
