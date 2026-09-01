//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestNativeWorkerFlagOffStandardArtifactBuildsAndServes is the AUTHORITATIVE
// flag-off gate for ExecBridge-U1b. Before U1b, no integration
// leg built the STANDARD native_capable artifact: the container renderer had no
// nativeWorker field and always emitted NATIVE_WORKER=false, so even the
// --baml-source matrix built the BAML-only ROLLBACK root worker, never the isolated
// nanollmprepare:./cmd/worker/ path. This leg builds that standard isolated worker
// (NativeWorker=true, NativeOnlyWorker=false) through the full overlay/build/boot
// container path and serves a real request — authoritatively proving U1b left the
// standard artifact green (every U1b build.sh change is gated on
// NATIVE_ONLY_WORKER=true, so this path is byte-unchanged, and this leg is what
// PROVES it rather than asserting it).
//
// It requires a BAML source build (BAML_SOURCE): the isolated worker's generated
// baml_client compiles against the PATCHED BAML runtime (OrderedFields et al.),
// which the overlay wires only from the custom/source BAML — stock
// github.com/boundaryml/baml lacks those symbols. With BAML_SOURCE unset (the
// ordinary matrix), the leg skips rather than build the wrong (stock-BAML) worker.
func TestNativeWorkerFlagOffStandardArtifactBuildsAndServes(t *testing.T) {
	if BAMLSourcePath == "" {
		t.Skip("standard isolated worker needs the patched BAML runtime; set BAML_SOURCE (the --baml-source lane) to run this authoritative flag-off gate")
	}

	opts := matrixSetupOptions()
	// The one thing under test: build the STANDARD native_capable artifact
	// (isolated nanollmprepare:./cmd/worker/), NOT the native-only artifact and NOT
	// the BAML-only rollback root worker.
	opts.NativeWorker = true
	opts.NativeOnlyWorker = false

	setupCtx, setupCancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer setupCancel()

	// Setup builds the image (overlay + baml_client + isolated worker CGO build) and
	// boots the container. Its success IS the flag-off standard-artifact build proof.
	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("flag-off standard native_capable artifact failed to build/boot: %v", err)
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("env Terminate: %v", err)
		}
	}()

	// Serve a real request so the proof is that the standard artifact WORKS, not
	// merely that it linked. BAML_REST_USE_DEBAML defaults on, but with the shipped
	// empty cohort policy the standard worker still serves 100% BAML — exactly the
	// unchanged behaviour U1b must preserve.
	mockClient := mockllm.NewClient(env.MockLLMURL)
	bamlClient := testutil.NewBAMLRestClient(env.BAMLRestURL)

	regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer regCancel()
	scenario := &mockllm.Scenario{
		ID:       "u1b-flagoff-standard",
		Provider: "openai",
		Content:  "Hello, standard artifact!",
	}
	if err := mockClient.RegisterScenario(regCtx, scenario); err != nil {
		t.Fatalf("RegisterScenario: %v", err)
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer callCancel()
	resp, err := bamlClient.Call(callCtx, testutil.CallRequest{
		Method: "GetGreeting",
		Input:  map[string]any{"name": "Standard"},
		Options: &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(env.MockLLMInternal, scenario.ID),
		},
	})
	if err != nil {
		t.Fatalf("Call against the standard native_capable artifact failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from the standard artifact, got %d: %s", resp.StatusCode, resp.Error)
	}
	var result string
	if err := sonic.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result != "Hello, standard artifact!" {
		t.Fatalf("standard artifact returned %q, want %q", result, "Hello, standard artifact!")
	}
}
