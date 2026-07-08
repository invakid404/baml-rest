//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// De-BAML production wiring, GitHub #536 — REST/container analogue of
// Oracle B.
//
// Oracle B proves native metadata appears for the in-process dynclient.
// This test proves the SAME on the REST production path: a dedicated
// baml-rest container started with BAML_REST_USE_DEBAML=true must emit the
// native ctx.output_format block (with the metadata BAML's dynamic
// TypeBuilder drops) into the provider request — and a dedicated
// de-BAML-OFF container (BAML_REST_USE_DEBAML pinned to false) must not.
// The BuildRequest route is unconditional as of #537, so both containers
// share the same route and the differential isolates exactly
// BAML_REST_USE_DEBAML. This guards the gap where the production generated
// adapter never actually emitted the de-BAML injection (codegen only wired
// it for dynclient), so BAML_REST_USE_DEBAML was inert for REST traffic.
//
// De-BAML is now DEFAULT-ON, so the shared TestEnv can no longer serve as
// the flag-OFF control (it runs native by default). Both legs therefore use
// dedicated containers with the umbrella flag pinned explicitly — ON vs OFF,
// never ON vs the-default — so the differential stays honest regardless of
// the suite-wide default.

// restMetadataSchema mirrors oracleBSchema in the HTTP/testutil shape:
// field description + alias, class description (+ a class alias that must
// stay invisible), field-of-class description + alias, and an enum-level
// alias hoisted via a value description.
func restMetadataSchema() *testutil.DynamicOutputSchema {
	return &testutil.DynamicOutputSchema{
		Properties: map[string]*testutil.DynamicProperty{
			"title":  {Type: "string", Description: "The headline.", Alias: "headline"},
			"addr":   {Ref: "Address"},
			"status": {Ref: "Status"},
		},
		Classes: map[string]*testutil.DynamicClass{
			"Address": {
				Description: "A postal address.",
				Alias:       "PostalAddress", // not prompt-visible; asserted absent
				Properties: map[string]*testutil.DynamicProperty{
					"street": {Type: "string", Description: "Street line.", Alias: "line1"},
					"city":   {Type: "string"},
				},
			},
		},
		Enums: map[string]*testutil.DynamicEnum{
			"Status": {
				Alias: "State",
				Values: []*testutil.DynamicEnumValue{
					{Name: "ACTIVE", Description: "is active"},
					{Name: "INACTIVE"},
				},
			},
		},
	}
}

func restOutputFormatFirstMessage() []testutil.DynamicMessage {
	return []testutil.DynamicMessage{
		{Role: "system", Content: []testutil.DynamicContentPart{{Type: "output_format"}}},
		{Role: "user", Content: "Produce the structured output."},
	}
}

const restDeBAMLMock = `{"title":"t","addr":{"street":"s","city":"c"},"status":"ACTIVE"}`

// TestDeBAMLRest_MetadataAppearsOnProductionPath spins a dedicated
// container with de-BAML on, sends a metadata-bearing dynamic request, and
// asserts the native metadata is in the captured provider body — the
// REST-production proof that BAML_REST_USE_DEBAML is live. A second
// dedicated container with the flag pinned OFF provides the negative leg;
// the shared TestEnv is now default-ON and cannot serve as that control.
func TestDeBAMLRest_MetadataAppearsOnProductionPath(t *testing.T) {
	// The native seam exists only on the BuildRequest route (BAML >= 0.219),
	// which is unconditional as of #537.
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("Skipping: de-BAML REST seam requires the BuildRequest route (BAML >= 0.219.0)")
	}

	// --- de-BAML ON: dedicated container with the flag pinned true ---
	onBlock := captureDeBAMLRestBlock(t, "true", "test-debaml-rest-on")

	// flag-ON must carry the native metadata BAML-dynamic drops, plus the
	// canonical class reference (so the metadata check can't be satisfied
	// vacuously by the Address ref being dropped) and never the class
	// alias. These hold regardless of CI leg: the dedicated ON container
	// always runs BuildRequest + de-BAML, and BAML-dynamic cannot produce
	// this metadata on any route — so their presence alone proves de-BAML
	// is live on the REST production path.
	nativeMetadata := []struct{ what, token string }{
		{"field description", "The headline."},
		{"field alias", "headline:"},
		{"class description", "A postal address."},
		{"field-of-class alias", "line1:"},
		{"enum alias (hoisted header)", "State\n----"},
	}
	for _, tok := range nativeMetadata {
		if !strings.Contains(onBlock, tok.token) {
			t.Errorf("flag-ON REST block missing %s %q — de-BAML not active on the production path\n--- ON ---\n%s", tok.what, tok.token, onBlock)
		}
	}
	for _, tok := range []string{"addr: {", "city:"} {
		if !strings.Contains(onBlock, tok) {
			t.Errorf("flag-ON REST block missing canonical token %q\n--- ON ---\n%s", tok, onBlock)
		}
	}
	if strings.Contains(onBlock, "PostalAddress") {
		t.Errorf("flag-ON REST block unexpectedly contains class alias %q (canonical names only)\n--- ON ---\n%s", "PostalAddress", onBlock)
	}

	// de-BAML-OFF differential — a dedicated container with the umbrella flag
	// pinned OFF. The BuildRequest route is unconditional (#537), so this OFF
	// container runs the SAME route as the dedicated ON container on
	// BAML >= 0.219 (already guaranteed by the skip above); the comparison
	// therefore isolates exactly BAML_REST_USE_DEBAML and never the route. The
	// flag is pinned explicitly rather than inherited from the shared TestEnv,
	// which is now default-ON and would make this an on-vs-on comparison.
	offBlock := captureDeBAMLRestBlock(t, "false", "test-debaml-rest-off")

	for _, tok := range nativeMetadata {
		if strings.Contains(offBlock, tok.token) {
			t.Errorf("flag-OFF REST block unexpectedly contains %s %q\n--- OFF ---\n%s", tok.what, tok.token, offBlock)
		}
	}
	if onBlock == offBlock {
		t.Fatalf("flag-ON and flag-OFF REST blocks were identical for a metadata-bearing schema:\n%q", onBlock)
	}
	for _, tok := range []string{"addr: {", "city:"} {
		if !strings.Contains(offBlock, tok) {
			t.Errorf("flag-OFF REST block missing canonical token %q\n--- OFF ---\n%s", tok, offBlock)
		}
	}
	if strings.Contains(offBlock, "PostalAddress") {
		t.Errorf("flag-OFF REST block unexpectedly contains class alias %q\n--- OFF ---\n%s", "PostalAddress", offBlock)
	}
}

// captureDeBAMLRestBlock spins a dedicated baml-rest container with the
// de-BAML umbrella flag pinned explicitly to flagValue ("true" or "false"),
// registers the REST scenario on that container's own mock, drives a
// metadata-bearing dynamic request, and returns the first message's rendered
// output_format block from the captured provider body.
//
// The explicit RuntimeEnv pin is load-bearing on BOTH legs now that de-BAML
// is default-ON: the shared TestEnv is no longer a flag-OFF control, so each
// leg gets its own container with the flag set to a known value. The
// container is torn down via t.Cleanup.
func captureDeBAMLRestBlock(t *testing.T, flagValue, scenarioID string) string {
	t.Helper()

	opts := matrixSetupOptions()
	opts.RuntimeEnv = map[string]string{
		bamlutils.EnvUseDeBAML: flagValue,
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer setupCancel()

	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("Failed to setup dedicated de-BAML=%s env: %v", flagValue, err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated env (de-BAML=%s) Terminate: %v", flagValue, err)
		}
	})

	mock := mockllm.NewClient(env.MockLLMURL)
	client := testutil.NewBAMLRestClient(env.BAMLRestURL)
	registerDeBAMLRestScenario(t, mock, scenarioID)

	return restCaptureBlock(t, client, mock, env.MockLLMInternal, scenarioID)
}

func registerDeBAMLRestScenario(t *testing.T, client *mockllm.Client, scenarioID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.RegisterScenario(ctx, &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        restDeBAMLMock,
		ChunkSize:      0,
		InitialDelayMs: 10,
	}); err != nil {
		t.Fatalf("RegisterScenario %q: %v", scenarioID, err)
	}
}

// restCaptureBlock sends the output_format-first dynamic request through
// the HTTP server and returns the first message's rendered text from the
// captured provider body.
func restCaptureBlock(t *testing.T, client *testutil.BAMLRestClient, mock *mockllm.Client, mockInternalURL, scenarioID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.DynamicCall(ctx, testutil.DynamicRequest{
		Messages:       restOutputFormatFirstMessage(),
		ClientRegistry: testutil.CreateTestClient(mockInternalURL, scenarioID),
		OutputSchema:   restMetadataSchema(),
	}); err != nil {
		// Response parsing may fail; the request is captured beforehand.
		t.Logf("DynamicCall(%s) (request still captured): %v", scenarioID, err)
	}

	body, err := mock.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest(%s): %v", scenarioID, err)
	}
	block, err := extractFirstMessageText(body)
	if err != nil {
		t.Fatalf("extractFirstMessageText(%s): %v\n%s", scenarioID, err, body)
	}
	return block
}
