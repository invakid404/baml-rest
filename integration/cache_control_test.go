//go:build integration

package integration

// Regression coverage for issue #304: the dynamic public API sends
// `cache_control.type` but the generated dynamic BAML input type uses
// `cache_control.cache_type`. Before the bridge fix, bamlutils
// DynamicInput.ToWorkerInput reused the public MessageMetadata struct
// directly, so Go JSON unmarshalling into the generated input dropped
// the `type` value and left a non-nil empty cache-control object. With
// `allowed_role_metadata: ["cache_control"]`, BAML then forwarded the
// malformed object to Anthropic, which rejected the request with
// "cache_control.type: Field required".
//
// The mock Anthropic endpoint mirrors that validation rule so this test
// observes the same failure class locally; the test asserts the fixed
// path produces a well-formed `{"type":"ephemeral"}` payload in the
// captured upstream request body.

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestDynclientCacheControlMessageMetadataRoundTrip verifies that the
// dynclient public API ({"cache_control":{"type":"ephemeral"}}) survives
// the bamlutils bridge layer end-to-end: the upstream Anthropic body
// captured by the mock LLM must contain a well-formed cache_control
// object on the tagged user message, and the call must succeed with the
// expected answer.
//
// This regression test has teeth because the mock Anthropic endpoint
// rejects malformed content-block cache_control with HTTP 400 — the
// exact failure class issue #304 reported against real Anthropic.
// Reverting the bridge fix in bamlutils/dynamic.go must make this test
// fail with a "cache_control.type: Field required" style error.
func TestDynclientCacheControlMessageMetadataRoundTrip(t *testing.T) {
	dynclientCallGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	waitForHealthy(t, 30*time.Second)

	scenarioID := "test-dynclient-cache-control-metadata"
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "anthropic",
		Content:        `{"answer": "ok"}`,
		InitialDelayMs: 25,
	}
	regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
	defer regCancel()
	if err := MockClient.RegisterScenario(regCtx, scenario); err != nil {
		t.Fatalf("RegisterScenario: %v", err)
	}

	httpRegistry := testutil.CreateAnthropicTestClient(TestEnv.MockLLMInternal, scenarioID)
	// allowed_role_metadata enables BAML's cache_control forwarding;
	// without it, BAML drops metadata silently and the bug shape never
	// reaches the wire.
	if len(httpRegistry.Clients) == 0 {
		t.Fatalf("CreateAnthropicTestClient returned no clients")
	}
	if httpRegistry.Clients[0].Options == nil {
		httpRegistry.Clients[0].Options = map[string]any{}
	}
	httpRegistry.Clients[0].Options["allowed_role_metadata"] = []any{"cache_control"}

	registry := dynRegistry(httpRegistry)
	if registry == nil {
		t.Fatalf("dynRegistry returned nil")
	}

	systemText := "You are a helpful assistant."
	userText := "Say ok."
	assistantText := "I will answer carefully."
	followupText := "Now answer."

	libReq := dynclient.Request{
		Messages: []dynclient.Message{
			{Role: "system", TextContent: &systemText},
			{
				Role: "user",
				PartsContent: []dynclient.ContentPart{
					{Type: "text", Text: &userText},
				},
				Metadata: &dynclient.MessageMetadata{
					CacheControl: &dynclient.CacheControl{Type: "ephemeral"},
				},
			},
			{Role: "assistant", TextContent: &assistantText},
			{Role: "user", TextContent: &followupText},
		},
		ClientRegistry: registry,
		OutputSchema: &dynclient.OutputSchema{
			Properties: map[string]*dynclient.Property{
				"answer": {Type: "string"},
			},
		},
	}

	// Pin the BuildRequest path — the issue reporter ran into the bug
	// with WithUseBuildRequest(true) explicitly. Don't rely on the
	// global ActuallyBuildRequest() flag for this regression.
	client := newDynclient(t, dynclient.WithUseBuildRequest(true))

	resp, err := client.DynamicCall(ctx, libReq)
	if err != nil {
		// Log the captured upstream body to help diagnose what the bridge
		// actually emitted on the wire when the call failed.
		if captured, getErr := MockClient.GetLastRequest(ctx, scenarioID); getErr == nil {
			t.Logf("captured anthropic body on failure: %s", string(captured))
		}
		t.Fatalf("DynamicCall failed (expected success after bridge fix): %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(resp.Data, &decoded); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, string(resp.Data))
	}
	if decoded["answer"] != "ok" {
		t.Errorf("answer = %v, want %q", decoded["answer"], "ok")
	}

	captured, err := MockClient.GetLastRequest(ctx, scenarioID)
	if err != nil {
		t.Fatalf("GetLastRequest: %v", err)
	}
	t.Logf("captured anthropic body: %s", string(captured))

	var upstream struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &upstream); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}

	// Walk every content block in every message and assert the only
	// cache_control objects we see are well-formed ephemeral payloads.
	// Tolerant about which message Anthropic ends up tagging — BAML may
	// reorder system text or coalesce adjacent user blocks, but every
	// surviving cache_control object must be valid.
	wellFormedCount := 0
	for mi, m := range upstream.Messages {
		for bi, blk := range m.Content {
			var block map[string]json.RawMessage
			if err := json.Unmarshal(blk, &block); err != nil {
				continue
			}
			ccRaw, ok := block["cache_control"]
			if !ok {
				continue
			}
			var cc map[string]any
			if err := json.Unmarshal(ccRaw, &cc); err != nil {
				t.Errorf("messages[%d].content[%d].cache_control is not an object: %s", mi, bi, ccRaw)
				continue
			}
			typ, hasType := cc["type"]
			if !hasType {
				t.Errorf("messages[%d].content[%d].cache_control missing type key: %v", mi, bi, cc)
				continue
			}
			s, ok := typ.(string)
			if !ok || s == "" {
				t.Errorf("messages[%d].content[%d].cache_control.type is missing/empty: %v", mi, bi, typ)
				continue
			}
			if s != "ephemeral" {
				t.Errorf("messages[%d].content[%d].cache_control.type = %q, want %q", mi, bi, s, "ephemeral")
				continue
			}
			wellFormedCount++
		}
	}
	if wellFormedCount == 0 {
		t.Errorf("expected at least one well-formed cache_control:{type:ephemeral} on the upstream body; got none.\ncaptured=%s", string(captured))
	}
}
