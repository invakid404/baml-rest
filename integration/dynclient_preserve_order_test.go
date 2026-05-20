//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/dynclient"
)

// buildPreserveOrderDynclientReq constructs a dynclient.Request whose
// system message includes an `{type: "output_format"}` content part —
// this is what triggers the BAML renderer to splice the rendered output
// schema string into the upstream prompt. Without that part, the
// upstream message body would carry only the user prompt and we'd have
// nothing to assert ordering against.
//
// OutputSchema is intentionally non-alphabetical (delta < alpha <
// charlie < bravo in the caller's chosen order). Whether the rendered
// prompt preserves that order or sorts it alphabetically is the
// behaviour under test.
func buildPreserveOrderDynclientReq(registry *dynclient.ClientRegistry) dynclient.Request {
	systemText := "Return only JSON matching the requested schema."
	userText := "Build a sample record."
	return dynclient.Request{
		Messages: []dynclient.Message{
			{
				Role: "system",
				PartsContent: []dynclient.ContentPart{
					{Type: "text", Text: &systemText},
					{Type: "output_format"},
				},
			},
			{Role: "user", TextContent: &userText},
		},
		ClientRegistry: registry,
		OutputSchema: &dynclient.OutputSchema{
			Properties: dynclient.MustOrderedMap(
				dynclient.OrderedKV("delta", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("alpha", &dynclient.Property{Type: "int"}),
				dynclient.OrderedKV("charlie", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("bravo", &dynclient.Property{Type: "string"}),
			),
		},
	}
}

// TestDynclientPreservesOrderByDefault pins the #318 dynclient default:
// a Go caller constructs Request with non-alphabetical OrderedMap
// entries and leaves Request.PreserveSchemaOrder nil. The rendered
// upstream output_format must follow the OrderedMap insertion order,
// not the alphabetical fallback. This is the end-to-end counterpart
// of the unit-level truth-table test in dynclient/client_test.go.
//
// The assertion path mirrors #313's TestDynamicOutputFormatPreserveSchemaOrder:
// capture the upstream LLM request body via MockClient.GetLastRequest,
// extract the first message text (which carries the rendered
// output_format because the system message includes an output_format
// content part), and assert the property names appear in OrderedMap
// insertion order.
func TestDynclientPreservesOrderByDefault(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-dynclient-preserve-order-default"
	opts := setupNonStreamingScenario(t, scenarioID, `{"delta": "d", "alpha": 1, "charlie": "c", "bravo": "b"}`)

	libReq := buildPreserveOrderDynclientReq(dynRegistry(opts.ClientRegistry))
	client := newDynclient(t)
	if _, err := client.DynamicCall(ctx, libReq); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}

	capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest: %v", err)
	}
	prompt, err := extractFirstMessageText(capturedBody)
	if err != nil {
		t.Fatalf("extractFirstMessageText: %v\ncaptured: %s", err, string(capturedBody))
	}
	assertOrderIn(t, "dynclient default preserve (top-level)", prompt, "", []string{"delta", "alpha", "charlie", "bravo"})
}

// TestDynclientPreservesOrderOptOut pins that
// WithPreserveSchemaOrderDefault(false) flips the dynclient default
// back to alphabetical sort. Same construction as the by-default test,
// but the client opts out — the rendered output_format must surface
// keys in sorted (alpha, bravo, charlie, delta) order.
func TestDynclientPreservesOrderOptOut(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-dynclient-preserve-order-optout"
	opts := setupNonStreamingScenario(t, scenarioID, `{"delta": "d", "alpha": 1, "charlie": "c", "bravo": "b"}`)

	libReq := buildPreserveOrderDynclientReq(dynRegistry(opts.ClientRegistry))
	client := newDynclient(t, dynclient.WithPreserveSchemaOrderDefault(false))
	if _, err := client.DynamicCall(ctx, libReq); err != nil {
		t.Fatalf("DynamicCall: %v", err)
	}

	capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest: %v", err)
	}
	prompt, err := extractFirstMessageText(capturedBody)
	if err != nil {
		t.Fatalf("extractFirstMessageText: %v\ncaptured: %s", err, string(capturedBody))
	}
	assertOrderIn(t, "dynclient default sorted (top-level)", prompt, "", []string{"alpha", "bravo", "charlie", "delta"})
}
