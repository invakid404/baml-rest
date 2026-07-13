//go:build nanollm_integration

package execute

// De-BAML Slice 2a functional matrix: the real-send HAPPY PATH. One
// baml-rest-native OpenAI chat body (built once by nativebody.BuildOpenAIChat)
// is executed through nanollm's real Do/DoStream over loopback against the
// go-mocklm subprocess (see mocklm_integration_test.go for the harness):
//
//	nativebody body -> nanollm Go API -> CGO/Rust translation core
//	  -> provider-native HTTP over loopback -> go-mocklm openai|anthropic route
//	  -> provider-native response/SSE -> nanollm translation -> OpenAI caller result
//
// The SAME raw body bytes drive both provider aliases; nanollm rewrites the
// top-level model to each configured target and (for Anthropic) reshapes the
// OpenAI body into a provider-native Messages request. This file implements only
// the three happy-path rows (OpenAI Do parity, Anthropic Do parity, Anthropic
// DoStream). Fallback / timeout / uniform-error-envelope rows are Slice 2b.
//
// Provider/response JSON structs are LOCAL and narrow: no production abstraction
// is introduced around nanollm. Every credential is a literal fake, every
// BaseURL is a literal 127.0.0.1 URL, UseProcessEnv is false, and every call
// runs on the loopback-guarded transport. Tests are SERIAL (no t.Parallel).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

const (
	// Two aliases resolving to two provider targets; the alias (Name) is what a
	// Request references, the target lives in Model as "provider/target".
	openaiAlias     = "slice2-openai"
	anthropicAlias  = "slice2-anthropic"
	openaiTarget    = "slice2-openai-target"
	anthropicTarget = "slice2-anthropic-target"

	// fakeAPIKey is a literal fake — it must surface verbatim in the recorded
	// auth headers and NEVER be read from the process environment.
	fakeAPIKey = "sk-slice2-fake"

	// sourceModel is the placeholder model in the shared source body. nanollm
	// rewrites it to each alias's configured target, so the captured bodies must
	// carry the target model and never this placeholder — proof the rewrite ran.
	sourceModel = "slice2-source-model"

	// callTimeout is a generous per-call context deadline (no artificial client
	// timeout — the happy path is not a timing test).
	callTimeout = 30 * time.Second
)

func sp(s string) *string { return &s }

// newSendClient builds a v0.3.2 client with the two literal, fake aliases bound
// to the mock's loopback base. MaxRetries:0 keeps attempt counts unambiguous;
// UseProcessEnv:false forbids process-env secret resolution.
func newSendClient(t *testing.T, mockBase string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{
			{
				Name:       openaiAlias,
				Model:      "openai/" + openaiTarget,
				APIKey:     fakeAPIKey,
				BaseURL:    mockBase + "/v1",
				MaxRetries: 0,
			},
			{
				Name:       anthropicAlias,
				Model:      "anthropic/" + anthropicTarget,
				APIKey:     fakeAPIKey,
				BaseURL:    mockBase + "/v1",
				MaxRetries: 0,
			},
		},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// sharedNativeBody builds ONE OpenAI chat body via baml-rest's own native path
// (nativeprompt + nativebody.BuildOpenAIChat), with an ordered system+user
// prompt so the Anthropic translation has a system message to lift to top-level.
// The same bytes are reused for every alias.
func sharedNativeBody(t *testing.T) []byte {
	t.Helper()
	rendered := &nativeprompt.RenderedPrompt{
		Kind: nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{
			{Role: "system", Parts: []nativeprompt.Part{{Text: sp("You are a concise assistant.")}}},
			{Role: "user", Parts: []nativeprompt.Part{{Text: sp("Say the magic words.")}}},
		},
	}
	body, err := nativebody.BuildOpenAIChat(rendered, nativebody.ClientIntent{
		Provider:    nativebody.ProviderOpenAI,
		TargetModel: sourceModel,
		// ModelAlias is metadata only (never serialized into the body); the same
		// bytes serve both aliases, so its value here is immaterial.
		ModelAlias: openaiAlias,
	})
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	return body.Bytes()
}

// --- local, narrow provider/response shapes ---

// openAIChatResponse is the narrow OpenAI caller-result view nanollm returns
// (for BOTH providers — Anthropic is translated back to this shape).
type openAIChatResponse struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// openAIRequestBody is the narrow view of the captured OpenAI-native request.
type openAIRequestBody struct {
	Model    string `json:"model"`
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
}

// anthropicRequestBody is the narrow view of the captured Anthropic-native
// request (post-translation): target model, max_tokens, an extracted top-level
// system, and messages that must no longer carry system/developer roles.
type anthropicRequestBody struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Stream    bool            `json:"stream"`
	Messages  []struct {
		Role string `json:"role"`
	} `json:"messages"`
}

// assertOpenAIResult checks the caller-visible structural contract shared by
// both Do parity rows: 200, JSON body, exactly one valid choice with the exact
// assistant text and finish reason, and usage present.
func assertOpenAIResult(t *testing.T, resp *nanollm.Response, wantText, wantFinish string) {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.Status, resp.Body)
	}
	if !resp.BodyIsJSON {
		t.Errorf("BodyIsJSON = false, want true")
	}
	var parsed openAIChatResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("response body is not valid JSON: %v\n%s", err, resp.Body)
	}
	if len(parsed.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly 1", len(parsed.Choices))
	}
	ch := parsed.Choices[0]
	if ch.Message.Role != "assistant" {
		t.Errorf("choice role = %q, want assistant", ch.Message.Role)
	}
	if ch.Message.Content != wantText {
		t.Errorf("assistant content = %q, want %q", ch.Message.Content, wantText)
	}
	if ch.FinishReason != wantFinish {
		t.Errorf("finish_reason = %q, want %q", ch.FinishReason, wantFinish)
	}
	// Usage present (nanollm surfaces it on the translated Response).
	if resp.Usage == nil {
		t.Errorf("Response.Usage is nil, want present")
	}
}

func assertMessageRoles(t *testing.T, roles []string, forbidden ...string) {
	t.Helper()
	for i, role := range roles {
		for _, bad := range forbidden {
			if role == bad {
				t.Errorf("message %d retained role %q; it must not survive translation", i, role)
			}
		}
	}
}

// TestSendParityOpenAI — row (a): Do through the OpenAI alias with the shared
// native body. The mock returns an exact OpenAI-native completion; nanollm hands
// it back as an OpenAI caller result. Proves the OpenAI native route + capture +
// fake Bearer auth.
func TestSendParityOpenAI(t *testing.T) {
	m := startMock(t)
	client := newSendClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const id = "s2-openai-happy"
	const wantText = "openai-native-ok"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "openai",
		Model:    openaiTarget,
		Output:   &exactOutput{Text: wantText, OutputTokens: 7},
	})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := client.Do(ctx, nanollm.Request{
		Model: openaiAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()))
	if err != nil {
		t.Fatalf("Do(openai): %v", err)
	}

	assertOpenAIResult(t, resp, wantText, "stop")

	if got := m.scenarioRequestCount(id); got != 1 {
		t.Errorf("scenario request count = %d, want 1", got)
	}

	// Captured request: OpenAI-native route + body (rewritten target model,
	// ordered system+user messages).
	cap := m.lastRequest(id)
	if cap.Path != "/v1/chat/completions" {
		t.Errorf("captured path = %q, want /v1/chat/completions", cap.Path)
	}
	var reqBody openAIRequestBody
	if err := json.Unmarshal(cap.Body, &reqBody); err != nil {
		t.Fatalf("captured OpenAI body is not JSON: %v\n%s", err, cap.Body)
	}
	if reqBody.Model != openaiTarget {
		t.Errorf("captured model = %q, want %q (nanollm rewrite)", reqBody.Model, openaiTarget)
	}
	roles := make([]string, len(reqBody.Messages))
	for i, msg := range reqBody.Messages {
		roles[i] = msg.Role
	}
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Errorf("captured message roles = %v, want [system user] (ordered)", roles)
	}

	// Recorded auth is the fake Bearer key.
	rec := m.recordedFor("openai", "/v1/chat/completions")
	if v, ok := headerValue(rec.Headers, "Authorization"); !ok || v != "Bearer "+fakeAPIKey {
		t.Errorf("Authorization = %q (present=%v), want %q", v, ok, "Bearer "+fakeAPIKey)
	}
}

// TestSendParityAnthropic — row (b): Do through the Anthropic alias with the
// SAME body bytes. nanollm reshapes the OpenAI body into a provider-native
// Anthropic Messages request that go-mocklm's STRICT mode accepts; the mock
// returns an exact (validated) Anthropic-native message, which nanollm
// translates back to the OpenAI caller shape.
func TestSendParityAnthropic(t *testing.T) {
	m := startMock(t)
	client := newSendClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const id = "s2-anthropic-happy"
	const wantText = "anthropic-native-ok"
	// strict:true makes go-mocklm reject a non-native Anthropic request (400),
	// so a 200 + exact content proves nanollm produced a provider-native body.
	// MOCKLM_VALIDATE_RESPONSES=1 additionally validates the raw Anthropic
	// response against the pinned spec before it is written (a failure would be a
	// 500), so the caller-visible 200 also proves the native response validated.
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "anthropic",
		Model:    anthropicTarget,
		Output:   &exactOutput{Text: wantText, OutputTokens: 7},
		Config:   json.RawMessage(`{"strict":true}`),
	})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := client.Do(ctx, nanollm.Request{
		Model: anthropicAlias,
		Body:  body, // same raw bytes as the OpenAI row
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()))
	if err != nil {
		t.Fatalf("Do(anthropic): %v", err)
	}

	assertOpenAIResult(t, resp, wantText, "stop")

	if got := m.scenarioRequestCount(id); got != 1 {
		t.Errorf("scenario request count = %d, want 1", got)
	}

	// Captured request: Anthropic-native route + STRICT-accepted body.
	cap := m.lastRequest(id)
	if cap.Path != "/v1/messages" {
		t.Errorf("captured path = %q, want /v1/messages", cap.Path)
	}
	var areq anthropicRequestBody
	if err := json.Unmarshal(cap.Body, &areq); err != nil {
		t.Fatalf("captured Anthropic body is not JSON: %v\n%s", err, cap.Body)
	}
	if areq.Model != anthropicTarget {
		t.Errorf("captured model = %q, want %q", areq.Model, anthropicTarget)
	}
	if areq.MaxTokens < 1 {
		t.Errorf("captured max_tokens = %d, want >= 1 (Anthropic requires it)", areq.MaxTokens)
	}
	if len(areq.System) == 0 || string(areq.System) == "null" {
		t.Errorf("captured top-level system missing; nanollm must extract it, got %q", string(areq.System))
	}
	roles := make([]string, len(areq.Messages))
	for i, msg := range areq.Messages {
		roles[i] = msg.Role
	}
	assertMessageRoles(t, roles, "system", "developer")

	// Recorded auth: fake x-api-key + a present anthropic-version (go-mocklm
	// requires both, so reaching a 200 already implies they were sent; assert
	// them explicitly against the fake key).
	rec := m.recordedFor("anthropic", "/v1/messages")
	if v, ok := headerValue(rec.Headers, "x-api-key"); !ok || v != fakeAPIKey {
		t.Errorf("x-api-key = %q (present=%v), want %q", v, ok, fakeAPIKey)
	}
	if v, ok := headerValue(rec.Headers, "anthropic-version"); !ok || v == "" {
		t.Errorf("anthropic-version = %q (present=%v), want non-empty", v, ok)
	}
}

// TestStreamAnthropic — row (c): DoStream through the Anthropic alias with exact
// Unicode text and rune chunking. Proves the SSE crosses the FFI decoder
// incrementally (>= 2 non-empty deltas), reassembles to the exact text, yields
// one translated finish reason, ends on a clean io.EOF, and reports usage.
func TestStreamAnthropic(t *testing.T) {
	m := startMock(t)
	client := newSendClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const id = "s2-anthropic-stream"
	const wantText = "Hel🙂lo stream"
	const wantOutputTokens = 7
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "anthropic",
		Model:    anthropicTarget,
		Output: &exactOutput{
			Text:         wantText,
			Chunking:     &chunking{Mode: "runes", Size: 2},
			OutputTokens: wantOutputTokens,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	stream, err := client.DoStream(ctx, nanollm.Request{
		Model: anthropicAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()))
	if err != nil {
		t.Fatalf("DoStream(anthropic): %v", err)
	}
	defer stream.Close()

	var (
		reassembled    strings.Builder
		nonEmptyDeltas int
		finishes       []string
	)
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next: unexpected decoder/transport error: %v", err)
		}
		if d := chunk.Delta(); d != "" {
			reassembled.WriteString(d)
			nonEmptyDeltas++
		}
		if fr := chunk.FinishReason(); fr != "" {
			finishes = append(finishes, fr)
		}
	}

	// Incremental: rune-chunked "Hel🙂lo stream" (size 2) is many small deltas,
	// not one buffered blob.
	if nonEmptyDeltas < 2 {
		t.Errorf("non-empty deltas = %d, want >= 2 (incremental SSE)", nonEmptyDeltas)
	}
	if got := reassembled.String(); got != wantText {
		t.Errorf("reassembled stream = %q, want %q", got, wantText)
	}
	if len(finishes) != 1 || finishes[0] != "stop" {
		t.Errorf("finish reasons = %v, want exactly [stop]", finishes)
	}

	// Clean EOF: a second Next stays EOF, with no decoder/transport error.
	if _, err := stream.Next(); err != io.EOF {
		t.Errorf("second Next = %v, want io.EOF (idempotent clean end)", err)
	}

	// Usage present after finish; completion tokens equal the scenario value.
	usage, ok := stream.Usage()
	if !ok || usage == nil {
		t.Fatalf("stream usage missing after finish")
	}
	if usage.CompletionTokens != wantOutputTokens {
		t.Errorf("completion tokens = %d, want %d", usage.CompletionTokens, wantOutputTokens)
	}

	// Capture proves the native streaming route + stream:true; exactly one
	// attempt.
	cap := m.lastRequest(id)
	if cap.Path != "/v1/messages" {
		t.Errorf("captured path = %q, want /v1/messages", cap.Path)
	}
	var areq anthropicRequestBody
	if err := json.Unmarshal(cap.Body, &areq); err != nil {
		t.Fatalf("captured Anthropic stream body is not JSON: %v\n%s", err, cap.Body)
	}
	if !areq.Stream {
		t.Errorf("captured body stream = false, want true")
	}
	if got := m.scenarioAttemptCount(id); got != 1 {
		t.Errorf("scenario attempt count = %d, want 1", got)
	}
}
