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
// baml-rest container started with BOTH BAML_REST_USE_BUILD_REQUEST=true
// AND BAML_REST_USE_DEBAML=true must emit the native ctx.output_format
// block (with the metadata BAML's dynamic TypeBuilder drops) into the
// provider request — and the shared, flag-OFF TestEnv must not. This
// guards the gap where the production generated adapter never actually
// emitted the de-BAML injection (codegen only wired it for dynclient),
// so BAML_REST_USE_DEBAML was inert for REST traffic.

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
// container with both flags on, sends a metadata-bearing dynamic request,
// and asserts the native metadata is in the captured provider body — the
// REST-production proof that BAML_REST_USE_DEBAML is live. The shared
// flag-OFF TestEnv provides the negative leg.
func TestDeBAMLRest_MetadataAppearsOnProductionPath(t *testing.T) {
	// The native seam exists only on the BuildRequest route (BAML >= 0.219).
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("Skipping: de-BAML REST seam requires the BuildRequest route (BAML >= 0.219.0)")
	}

	// --- flag-ON: dedicated container with both flags ---
	opts := matrixSetupOptions()
	opts.UseBuildRequest = true // → container BAML_REST_USE_BUILD_REQUEST=true
	opts.RuntimeEnv = map[string]string{
		"BAML_REST_USE_DEBAML": "true",
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer setupCancel()

	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("Failed to setup de-BAML dedicated env: %v", err)
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated env Terminate: %v", err)
		}
	}()

	onMock := mockllm.NewClient(env.MockLLMURL)
	onClient := testutil.NewBAMLRestClient(env.BAMLRestURL)
	onScenario := "test-debaml-rest-on"
	registerDeBAMLRestScenario(t, onMock, onScenario)

	onBlock := restCaptureBlock(t, onClient, onMock, env.MockLLMInternal, onScenario)

	// --- flag-OFF: shared TestEnv (no BAML_REST_USE_DEBAML) ---
	offScenario := "test-debaml-rest-off"
	registerDeBAMLRestScenario(t, MockClient, offScenario)
	offBlock := restCaptureBlock(t, BAMLClient, MockClient, TestEnv.MockLLMInternal, offScenario)

	// ON must carry the native metadata BAML-dynamic drops; OFF must not.
	for _, tok := range []struct{ what, token string }{
		{"field description", "The headline."},
		{"field alias", "headline:"},
		{"class description", "A postal address."},
		{"field-of-class alias", "line1:"},
		{"enum alias (hoisted header)", "State\n----"},
	} {
		if !strings.Contains(onBlock, tok.token) {
			t.Errorf("flag-ON REST block missing %s %q — de-BAML not active on the production path\n--- ON ---\n%s", tok.what, tok.token, onBlock)
		}
		if strings.Contains(offBlock, tok.token) {
			t.Errorf("flag-OFF REST block unexpectedly contains %s %q\n--- OFF ---\n%s", tok.what, tok.token, offBlock)
		}
	}

	if onBlock == offBlock {
		t.Fatalf("flag-ON and flag-OFF REST blocks were identical for a metadata-bearing schema:\n%q", onBlock)
	}

	// Canonical class reference rendered in BOTH (so the absence checks
	// above can't pass by the Address reference being dropped), and the
	// class alias never appears (canonical-name behaviour).
	for _, tok := range []string{"addr: {", "city:"} {
		if !strings.Contains(onBlock, tok) {
			t.Errorf("flag-ON REST block missing canonical token %q\n--- ON ---\n%s", tok, onBlock)
		}
		if !strings.Contains(offBlock, tok) {
			t.Errorf("flag-OFF REST block missing canonical token %q\n--- OFF ---\n%s", tok, offBlock)
		}
	}
	for _, block := range []struct{ name, text string }{{"ON", onBlock}, {"OFF", offBlock}} {
		if strings.Contains(block.text, "PostalAddress") {
			t.Errorf("%s REST block unexpectedly contains class alias %q\n%s", block.name, "PostalAddress", block.text)
		}
	}
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
