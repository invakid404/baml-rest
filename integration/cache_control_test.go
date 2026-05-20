//go:build integration

package integration

// Regression coverage for issue #304: the dynamic public API sends
// `cache_control.type` but the generated dynamic BAML input type uses
// `cache_control.cache_type`. Before the bridge fix, bamlutils
// DynamicInput.ToWorkerInput reused the public MessageMetadata struct
// directly, so Go JSON unmarshalling into the generated input dropped
// the `type` value and left a non-nil empty cache-control object. With
// `allowed_role_metadata: ["cache_control"]`, BAML then forwarded the
// malformed object to the upstream provider, which rejected the request
// with "cache_control.type: Field required".
//
// The mock LLM mirrors that validation rule body-shape-gated across
// both /v1/messages (Anthropic) and /v1/chat/completions (OpenAI),
// because callers can route Anthropic-shaped metadata through
// openai-generic providers (LiteLLM and similar). The table-driven
// test covers both legs: a regression in the bridge or the template
// alias-bypass must fail at least one of them.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestDynclientCacheControlMessageMetadataRoundTrip verifies that the
// dynclient public API ({"cache_control":{"type":"ephemeral"}}) survives
// the bamlutils bridge layer end-to-end on both Anthropic and OpenAI
// dynamic dispatch paths. The captured upstream body must contain
// exactly one well-formed cache_control object on the tagged user
// message; the call must succeed with the expected answer.
//
// This regression test has teeth because the mock validator rejects
// malformed `cache_control` objects with HTTP 400 — the exact failure
// class issue #304 reported against real Anthropic. Reverting commit
// 5c223a637 (the class-free literal in dynamic.baml) makes BAML emit
// `cache_control:{"cache_type":"ephemeral"}`, which the validator
// rejects and the post-call assertion would catch.
func TestDynclientCacheControlMessageMetadataRoundTrip(t *testing.T) {
	dynclientCallGate(t)

	type caseSpec struct {
		// name labels the subtest and is folded into the scenario id so
		// concurrent registrations don't collide on the mock store.
		name string
		// scenarioProvider drives the mock LLM response shape. For the
		// Anthropic leg, the mock dispatches via /v1/messages and the
		// AnthropicProvider response formatter; for OpenAI it routes
		// through /v1/chat/completions and the openai formatter.
		scenarioProvider string
		// buildRegistry produces the httpClient registry the dynclient
		// driver consumes. Both helpers point at TestEnv.MockLLMInternal
		// (the container-internal URL the BAML adapter will dial); the
		// dynclient base-URL rewrite in newDynclient maps that to the
		// host-reachable URL.
		buildRegistry func(scenarioID string) *testutil.ClientRegistry
	}

	cases := []caseSpec{
		{
			name:             "anthropic",
			scenarioProvider: "anthropic",
			buildRegistry: func(scenarioID string) *testutil.ClientRegistry {
				return testutil.CreateAnthropicTestClient(TestEnv.MockLLMInternal, scenarioID)
			},
		},
		{
			name:             "openai",
			scenarioProvider: "openai",
			buildRegistry: func(scenarioID string) *testutil.ClientRegistry {
				return testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			waitForHealthy(t, 30*time.Second)

			scenarioID := "test-dynclient-cache-control-metadata-" + tc.name
			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       tc.scenarioProvider,
				Content:        `{"answer": "ok"}`,
				InitialDelayMs: 25,
			}
			regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
			defer regCancel()
			if err := MockClient.RegisterScenario(regCtx, scenario); err != nil {
				t.Fatalf("RegisterScenario: %v", err)
			}

			httpRegistry := tc.buildRegistry(scenarioID)
			// allowed_role_metadata enables BAML's cache_control
			// forwarding; without it BAML drops metadata silently and the
			// bug shape never reaches the wire. Both Anthropic and
			// openai-generic providers honor this option.
			if len(httpRegistry.Clients) == 0 {
				t.Fatalf("registry builder returned no clients")
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
					Properties: dynclientFromMap(map[string]*dynclient.Property{
						"answer": {Type: "string"},
					}),
				},
			}

			// Pin the BuildRequest path — the issue reporter ran into
			// the bug with WithUseBuildRequest(true) explicitly. Don't
			// rely on the global ActuallyBuildRequest() flag for this
			// regression.
			client := newDynclient(t, dynclient.WithUseBuildRequest(true))

			resp, err := client.DynamicCall(ctx, libReq)
			if err != nil {
				// Log the captured upstream body to help diagnose what
				// the bridge actually emitted on the wire when the call
				// failed.
				if captured, getErr := MockClient.GetLastRequest(ctx, scenarioID); getErr == nil {
					t.Logf("[%s] captured body on failure: %s", tc.name, string(captured))
				}
				t.Fatalf("[%s] DynamicCall failed (expected success after bridge+template fix): %v", tc.name, err)
			}

			var decoded map[string]any
			if err := json.Unmarshal(resp.Data, &decoded); err != nil {
				t.Fatalf("[%s] decode response: %v (raw=%s)", tc.name, err, string(resp.Data))
			}
			if decoded["answer"] != "ok" {
				t.Errorf("[%s] answer = %v, want %q", tc.name, decoded["answer"], "ok")
			}

			captured, err := MockClient.GetLastRequest(ctx, scenarioID)
			if err != nil {
				t.Fatalf("[%s] GetLastRequest: %v", tc.name, err)
			}
			t.Logf("[%s] captured body: %s", tc.name, string(captured))

			assertExactlyOneEphemeralCacheControl(t, captured)
		})
	}
}

// assertExactlyOneEphemeralCacheControl walks the captured upstream body
// as raw JSON and asserts the bridge+template fix delivered exactly one
// well-formed `{"type":"ephemeral"}` cache_control object. The helper is
// endpoint-agnostic so the same shape rules apply to Anthropic
// (`messages[].content[].cache_control`) and OpenAI/LiteLLM (top-level
// or block-level cache_control on chat-completions messages).
//
// Failure modes caught:
//   - missing cache_control entirely (totalCount == 0);
//   - extra cache_control objects injected on untagged messages
//     (totalCount > 1);
//   - malformed cache_control on the tagged message — empty/non-string
//     `type`, `cache_type` leak from the upstream BAML jinja-runtime bug
//     this PR works around, or a non-object cache_control value
//     (per-object t.Errorf alongside the wellFormedCount delta).
func assertExactlyOneEphemeralCacheControl(t *testing.T, captured []byte) {
	t.Helper()

	var root any
	if err := json.Unmarshal(captured, &root); err != nil {
		t.Fatalf("unmarshal captured body: %v\ncaptured=%s", err, string(captured))
	}

	totalCount := 0
	wellFormedCount := 0
	walkCacheControl(t, root, "$", &totalCount, &wellFormedCount, captured)

	if totalCount != 1 {
		t.Errorf("expected exactly 1 cache_control object on the upstream body; got %d.\ncaptured=%s", totalCount, string(captured))
	}
	if wellFormedCount != 1 {
		t.Errorf("expected exactly 1 well-formed cache_control:{type:ephemeral} on the upstream body; got %d well-formed of %d total.\ncaptured=%s", wellFormedCount, totalCount, string(captured))
	}
}

// walkCacheControl recursively descends into maps and arrays, validating
// every entry whose key is `cache_control`. Mutates totalCount and
// wellFormedCount pointers so callers can fold the result into a single
// pair of summary assertions. captured is passed through only for
// diagnostic logging.
func walkCacheControl(t *testing.T, node any, path string, totalCount, wellFormedCount *int, captured []byte) {
	t.Helper()

	switch v := node.(type) {
	case map[string]any:
		if cc, ok := v["cache_control"]; ok {
			ccPath := path + ".cache_control"
			*totalCount++
			if validateOneEphemeralCacheControl(t, cc, ccPath, captured) {
				*wellFormedCount++
			}
		}
		for k, child := range v {
			if k == "cache_control" {
				// The cache_control value is a leaf object; recursing
				// into it would re-flag its own `type` key as if it
				// were a nested cache_control entry.
				continue
			}
			walkCacheControl(t, child, path+"."+k, totalCount, wellFormedCount, captured)
		}
	case []any:
		for i, child := range v {
			walkCacheControl(t, child, fmt.Sprintf("%s[%d]", path, i), totalCount, wellFormedCount, captured)
		}
	}
}

// validateOneEphemeralCacheControl checks that a single cache_control
// value at the given path is the well-formed `{"type":"ephemeral"}`
// shape. Returns true on success; emits a path-tagged t.Errorf on any
// failure mode (non-object, missing/empty/non-string type, cache_type
// leak, type != "ephemeral") and returns false.
func validateOneEphemeralCacheControl(t *testing.T, cc any, path string, captured []byte) bool {
	t.Helper()

	obj, ok := cc.(map[string]any)
	if !ok {
		t.Errorf("%s is not an object: %T %v\ncaptured=%s", path, cc, cc, string(captured))
		return false
	}
	if _, hasCacheType := obj["cache_type"]; hasCacheType {
		t.Errorf("%s carries cache_type — upstream BAML alias-bypass regression: %v\ncaptured=%s", path, obj, string(captured))
		return false
	}
	typ, hasType := obj["type"]
	if !hasType {
		t.Errorf("%s missing type key: %v\ncaptured=%s", path, obj, string(captured))
		return false
	}
	s, ok := typ.(string)
	if !ok {
		t.Errorf("%s.type is not a string: %T %v\ncaptured=%s", path, typ, typ, string(captured))
		return false
	}
	if s == "" {
		t.Errorf("%s.type is empty\ncaptured=%s", path, string(captured))
		return false
	}
	if s != "ephemeral" {
		t.Errorf("%s.type = %q, want %q\ncaptured=%s", path, s, "ephemeral", string(captured))
		return false
	}
	return true
}
