//go:build nanollm_integration

package execute

// De-BAML Slice 6b realism proof: the SAME PreparedRequest-adapter pipeline, but
// executed against the real go-mocklm v0.4.0 subprocess (reusing the Slice-2
// harness in mocklm_integration_test.go) instead of the baml-rest capture server.
//
// Where the capture-server proofs pin byte-exact wire fidelity, this one pins
// PROVIDER-NATIVE VALIDITY: the exact PreparedRequest nanollm produced is a real
// OpenAI Chat Completions request that go-mocklm (with MOCKLM_VALIDATE_RESPONSES)
// accepts on its /v1/chat/completions route and answers deterministically, and
// the whole adapter pipeline (exact transport -> TranslateResponse -> OpenAI
// extraction -> native SAP) reaches structured output — in exactly one request,
// with no Do/DoStream, fallback, or same-model retry.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/internal/debaml"
)

// TestPreparedAttemptAgainstMockLM drives the 6b adapter against the go-mocklm
// subprocess and asserts a provider-native accepted request reaches native
// structured output.
func TestPreparedAttemptAgainstMockLM(t *testing.T) {
	m := startMock(t)
	nano := newPreparedClient(t, m.baseURL)
	prep := preparePlan(t, nano, m.baseURL, p6bNativeBody(t))

	const id = "p6b-openai-structured"
	// The assistant text is the structured JSON the native parser cleanly claims
	// against personSchema6b.
	const content = `{"name":"Ada","age":36}`
	// The fixture pins the output-token count deterministically; the translated
	// usage's completion tokens must map to exactly this value (below), so a wrong
	// go-mocklm-path usage translation cannot pass on a non-nil check alone.
	const wantOutputTokens = 7
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "openai",
		Model:    p6bTarget,
		Output:   &exactOutput{Text: content, OutputTokens: wantOutputTokens},
	})

	spy := &parseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), p6bTimeout)
	defer cancel()

	res, err := RunAttempt(ctx, AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     exactLoopbackExecutor(),
		Parse:        spy.Parse,
		OutputSchema: personSchema6b(),
	})
	if err != nil {
		t.Fatalf("RunAttempt(go-mocklm): %v", err)
	}

	// Structured output through the full pipeline; attempted alias retained.
	if res.Outcome != OutcomeStructured {
		t.Fatalf("outcome = %s, want structured", res.Outcome)
	}
	if res.AttemptedAlias != p6bAlias {
		t.Errorf("attempted alias = %q, want %q", res.AttemptedAlias, p6bAlias)
	}
	if !res.SAPInvoked || spy.calls != 1 {
		t.Errorf("SAP invocation: SAPInvoked=%v calls=%d, want invoked once", res.SAPInvoked, spy.calls)
	}
	if !jsonSemEqual(t, res.Structured, []byte(content)) {
		t.Errorf("structured output %s != want (name/age)", bodyDigest(res.Structured))
	}
	// Usage is retained AND its deterministic completion-token value is the
	// translated mapping of the fixture's pinned output tokens — binding the
	// go-mocklm-path usage translation, not merely asserting presence.
	if res.Usage == nil {
		t.Fatalf("Usage is nil, want retained from the translated response")
	}
	if res.Usage.CompletionTokens != wantOutputTokens {
		t.Errorf("usage completion tokens = %d, want %d (fixture-pinned)", res.Usage.CompletionTokens, wantOutputTokens)
	}

	// Exactly one provider-native request; the exact plan took the OpenAI route
	// with the rewritten target model.
	if got := m.scenarioRequestCount(id); got != 1 {
		t.Errorf("scenario request count = %d, want 1", got)
	}
	cap := m.lastRequest(id)
	if cap.Path != "/v1/chat/completions" {
		t.Errorf("captured path = %q, want /v1/chat/completions", cap.Path)
	}
	var reqBody openAIRequestBody
	if err := json.Unmarshal(cap.Body, &reqBody); err != nil {
		t.Fatalf("captured body is not JSON (%s): %v", bodyDigest(cap.Body), err)
	}
	if reqBody.Model != p6bTarget {
		t.Errorf("captured model = %q, want %q (nanollm target rewrite)", reqBody.Model, p6bTarget)
	}

	// The fake Bearer key reached the provider-native route verbatim.
	rec := m.recordedFor("openai", "/v1/chat/completions")
	if v, ok := headerValue(rec.Headers, "Authorization"); !ok || v != "Bearer "+p6bAPIKey {
		t.Errorf("Authorization present=%v value mismatch (redacted)", ok)
	}
}
