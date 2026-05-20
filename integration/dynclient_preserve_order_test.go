//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/dynclient"
)

// TestDynclientPreservesOrderByDefault pins the #318 dynclient default:
// a Go caller constructs Request with non-alphabetical OrderedMap
// entries and leaves Request.PreserveSchemaOrder nil. The rendered
// upstream output_format must follow the OrderedMap insertion order,
// not the alphabetical fallback. This is the end-to-end counterpart
// of the unit-level truth-table test in dynclient/client_test.go.
func TestDynclientPreservesOrderByDefault(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-dynclient-preserve-order-default"
	opts := setupNonStreamingScenario(t, scenarioID, `{"delta": "d", "alpha": 1, "charlie": "c", "bravo": "b"}`)

	hello := "Build a sample record."
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		// PreserveSchemaOrder intentionally left nil — dynclient must
		// resolve it to its default (true) and pass that through.
		OutputSchema: &dynclient.OutputSchema{
			Properties: dynclient.MustOrderedMap(
				dynclient.OrderedKV("delta", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("alpha", &dynclient.Property{Type: "int"}),
				dynclient.OrderedKV("charlie", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("bravo", &dynclient.Property{Type: "string"}),
			),
		},
	}
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
// back to alphabetical sort. The Go caller constructs the same
// non-alphabetical OrderedMap with no per-request preserve_schema_order,
// and the rendered output_format must surface keys in sorted order.
func TestDynclientPreservesOrderOptOut(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scenarioID := "test-dynclient-preserve-order-optout"
	opts := setupNonStreamingScenario(t, scenarioID, `{"delta": "d", "alpha": 1, "charlie": "c", "bravo": "b"}`)

	hello := "Build a sample record."
	libReq := dynclient.Request{
		Messages:       []dynclient.Message{{Role: "user", TextContent: &hello}},
		ClientRegistry: dynRegistry(opts.ClientRegistry),
		OutputSchema: &dynclient.OutputSchema{
			Properties: dynclient.MustOrderedMap(
				dynclient.OrderedKV("delta", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("alpha", &dynclient.Property{Type: "int"}),
				dynclient.OrderedKV("charlie", &dynclient.Property{Type: "string"}),
				dynclient.OrderedKV("bravo", &dynclient.Property{Type: "string"}),
			),
		},
	}
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
