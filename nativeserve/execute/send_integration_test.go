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
// OpenAI body into a provider-native Messages request.
//
// Slice 2a (happy path) rows: OpenAI Do parity, Anthropic Do parity, Anthropic
// DoStream. Slice 2b (fault matrix) rows, layered on the SAME harness, prove the
// executor's failure/fallback contract:
//
//   - returned-503 fallback: a returned non-2xx advances a configured chain;
//   - delay-induced client-timeout fallback: the ONLY public observable proof
//     the internal transport classifier marked a timeout retryable (asserted as
//     behavior — backup wins — NEVER by importing the off-limits internal
//     classifier or reproducing its message-sensitive reason);
//   - uniform Do error envelope: a non-2xx is normalized into the ONE uniform
//     JSON error envelope (classified OpenAI-safe error.type, native type under
//     error.code, bounded raw + status/provider under details);
//   - DoStream stream-start non-2xx: the deliberate v0.4.3 "D11" divergence —
//     no envelope, a wrapped *ProviderStatusError carrying the bounded raw
//     provider-native error body — pinned here as the contract.
//
// Provider/response JSON structs are LOCAL and narrow: no production abstraction
// is introduced around nanollm. Every credential is a literal fake, every
// BaseURL is a literal 127.0.0.1 URL, UseProcessEnv is false, and every call
// runs on the loopback-guarded transport. Tests are SERIAL (no t.Parallel).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	// Slice 2b fault-matrix aliases + provider targets. Each 2b test runs
	// against its OWN isolated mock process, so these never collide at runtime
	// with the 2a happy-path keys or with each other.
	//
	// The fallback rows (4, 5) put a healthy Anthropic backup in the OpenAI
	// primary's Fallbacks, proving cross-provider chain advance.
	fbPrimaryAlias  = "s2b-fb-primary"
	fbBackupAlias   = "s2b-fb-backup"
	fbPrimaryTarget = "s2b-fb-primary-target" // openai (the faulting primary)
	fbBackupTarget  = "s2b-fb-backup-target"  // anthropic (the healthy backup)

	// The uniform-envelope / DoStream-divergence rows (6, 7) drive an
	// Anthropic-only, no-fallback alias so the single non-2xx surfaces at the
	// API boundary instead of advancing a chain.
	faultAlias  = "s2b-fault"
	faultTarget = "s2b-fault-target" // anthropic

	// timeoutClientDeadline is the SHORT deadline set on the HTTP CLIENT (not on
	// the shared context.Context — that would cancel the healthy backup too).
	// It is far below the primary scenario's 2s delay and far above the
	// immediate backup's real latency, so the classification outcome is
	// deterministic without asserting millisecond-exact elapsed time.
	timeoutClientDeadline = 400 * time.Millisecond
	primaryDelayMs        = 2000

	// maxRawBytes mirrors nanollm's errorBodyReadCap (ERROR_BODY_READ_CAP, 64
	// KiB): the ceiling on both the DoStream *ProviderStatusError raw body and
	// the raw upstream nested under the uniform envelope's details. Used only as
	// a bounded-ness sanity ceiling — the mock's error bodies are tiny.
	maxRawBytes = 64 * 1024
)

func sp(s string) *string { return &s }

// newFallbackClient builds a v0.4.3 client whose OpenAI primary names the
// Anthropic backup in its Fallbacks, so FallbackChain(primary) == [primary,
// backup]. MaxRetries:0 on both aliases keeps the observation a CROSS-model
// advance (never a hidden same-model retry); UseProcessEnv:false forbids
// process-env secret resolution. Used by the returned-503 and timeout rows.
func newFallbackClient(t *testing.T, mockBase string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{
			{
				Name:       fbPrimaryAlias,
				Model:      "openai/" + fbPrimaryTarget,
				APIKey:     fakeAPIKey,
				BaseURL:    mockBase + "/v1",
				MaxRetries: 0,
				Fallbacks:  []string{fbBackupAlias},
			},
			{
				Name:       fbBackupAlias,
				Model:      "anthropic/" + fbBackupTarget,
				APIKey:     fakeAPIKey,
				BaseURL:    mockBase + "/v1",
				MaxRetries: 0,
			},
		},
		// v0.4.3 defaults an empty TransformMode to the native Anthropic route;
		// pin the jq oracle so this gated Anthropic Do/DoStream stays byte-identical
		// to v0.3.2 (native only falls back to jq on error — no shadow byte-compare).
		TransformMode: "jq",
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New (fallback): %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// newFaultClient builds a v0.4.3 client with a single Anthropic alias and NO
// fallbacks, so a returned non-2xx surfaces at the API boundary (uniform
// envelope for Do, *ProviderStatusError for DoStream) rather than advancing a
// chain. Used by the uniform-envelope and DoStream-divergence rows.
func newFaultClient(t *testing.T, mockBase string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{
			{
				Name:       faultAlias,
				Model:      "anthropic/" + faultTarget,
				APIKey:     fakeAPIKey,
				BaseURL:    mockBase + "/v1",
				MaxRetries: 0,
			},
		},
		// v0.4.3 defaults an empty TransformMode to the native Anthropic route;
		// pin the jq oracle so this gated Anthropic Do/DoStream stays byte-identical
		// to v0.3.2 (native only falls back to jq on error — no shadow byte-compare).
		TransformMode: "jq",
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New (fault): %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// timeoutClient returns a loopback-guarded http.Client with a SHORT client-level
// timeout. The deadline lives on the HTTP client, not on the request context, so
// each attempt (primary AND backup) gets its own fresh budget — the primary's 2s
// delay trips the timeout while the immediate backup completes well within it.
func timeoutClient(d time.Duration) *http.Client {
	return &http.Client{Transport: loopbackTransport(), Timeout: d}
}

// newSendClient builds a v0.4.3 client with the two literal, fake aliases bound
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
		// v0.4.3 defaults an empty TransformMode to the native Anthropic route;
		// pin the jq oracle so this gated Anthropic Do/DoStream stays byte-identical
		// to v0.3.2 (native only falls back to jq on error — no shadow byte-compare).
		TransformMode: "jq",
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
		t.Fatalf("status = %d, want 200 (body: %s)", resp.Status, bodyDigest(resp.Body))
	}
	if !resp.BodyIsJSON {
		t.Errorf("BodyIsJSON = false, want true")
	}
	var parsed openAIChatResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("response body is not valid JSON: %v\n%s", err, bodyDigest(resp.Body))
	}
	if len(parsed.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly 1", len(parsed.Choices))
	}
	ch := parsed.Choices[0]
	if ch.Message.Role != "assistant" {
		t.Errorf("choice role = %q, want assistant", ch.Message.Role)
	}
	if ch.Message.Content != wantText {
		t.Errorf("assistant content = %q, want %q", strDigest(ch.Message.Content), wantText)
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
		// Body is sensitive — report the parse error + a digest, never the bytes.
		t.Fatalf("captured OpenAI body is not JSON: %v (%s)", err, bodyDigest(cap.Body))
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
		// Never print the authorization value; report presence + match.
		t.Errorf("Authorization header missing or mismatched (present=%v, matched=%v)", ok, v == "Bearer "+fakeAPIKey)
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
		t.Fatalf("captured Anthropic body is not JSON: %v (%s)", err, bodyDigest(cap.Body))
	}
	if areq.Model != anthropicTarget {
		t.Errorf("captured model = %q, want %q", areq.Model, anthropicTarget)
	}
	if areq.MaxTokens < 1 {
		t.Errorf("captured max_tokens = %d, want >= 1 (Anthropic requires it)", areq.MaxTokens)
	}
	if len(areq.System) == 0 || string(areq.System) == "null" {
		t.Errorf("captured top-level system missing; nanollm must extract it, got %q", strDigest(string(areq.System)))
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
		// Never print the api-key value; report presence + match.
		t.Errorf("x-api-key header missing or mismatched (present=%v, matched=%v)", ok, v == fakeAPIKey)
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
		// Reassembled stream text is response content — report digests, not the text.
		t.Errorf("reassembled stream != want (got %s, want %s)", strDigest(got), strDigest(wantText))
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
		t.Fatalf("captured Anthropic stream body is not JSON: %v (%s)", err, bodyDigest(cap.Body))
	}
	if !areq.Stream {
		t.Errorf("captured body stream = false, want true")
	}
	if got := m.scenarioAttemptCount(id); got != 1 {
		t.Errorf("scenario attempt count = %d, want 1", got)
	}
}

// ============================================================================
// Slice 2b — fault / fallback / error-envelope matrix (rows 4–7)
// ============================================================================

// TestFallbackReturned503 — row 4: a RETURNED provider non-2xx advances a
// configured chain. The OpenAI primary is armed to return 503 on its first (and
// only) attempt; the healthy Anthropic backup then answers 200 and its exact
// content is what the caller sees. This proves v0.4.3's rule that a returned
// non-2xx advances a configured chain — it deliberately does NOT claim the 503
// status was itself "retryable" (that is the separate classification concern
// exercised by row 5).
func TestFallbackReturned503(t *testing.T) {
	m := startMock(t)
	client := newFallbackClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const primaryID = "s2b-fb503-primary"
	const backupID = "s2b-fb503-backup"
	const wantText = "fb503-backup-ok"

	// Primary: deterministic 503 on the first attempt (fail_first_n:1 +
	// error_status:503). No Output — it never produces a success body.
	m.registerScenario(scenarioSpec{
		ID:       primaryID,
		Provider: "openai",
		Model:    fbPrimaryTarget,
		Config:   json.RawMessage(`{"fail_first_n":1,"error_status":503}`),
	})
	// Backup: healthy + exact content; strict Anthropic so a 200 proves a
	// provider-native body crossed the FFI.
	m.registerScenario(scenarioSpec{
		ID:       backupID,
		Provider: "anthropic",
		Model:    fbBackupTarget,
		Output:   &exactOutput{Text: wantText, OutputTokens: 7},
		Config:   json.RawMessage(`{"strict":true}`),
	})

	// The configured chain is exactly [primary, backup].
	chain := client.FallbackChain(fbPrimaryAlias)
	if len(chain) != 2 || chain[0] != fbPrimaryAlias || chain[1] != fbBackupAlias {
		t.Fatalf("FallbackChain(%q) = %v, want [%s %s]", fbPrimaryAlias, chain, fbPrimaryAlias, fbBackupAlias)
	}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := client.Do(ctx, nanollm.Request{
		Model: fbPrimaryAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()))
	if err != nil {
		t.Fatalf("Do(fallback-503): %v", err)
	}

	// The backup's exact 200/content is the caller-visible result.
	assertOpenAIResult(t, resp, wantText, "stop")

	// Each alias was attempted exactly once: primary failed 503, the chain
	// advanced, the backup answered — no same-model retry (MaxRetries:0).
	if got := m.scenarioAttemptCount(primaryID); got != 1 {
		t.Errorf("primary attempt count = %d, want 1", got)
	}
	if got := m.scenarioAttemptCount(backupID); got != 1 {
		t.Errorf("backup attempt count = %d, want 1", got)
	}

	// The winning response came off the Anthropic backup route.
	if cap := m.lastRequest(backupID); cap.Path != "/v1/messages" {
		t.Errorf("backup captured path = %q, want /v1/messages", cap.Path)
	}
}

// TestFallbackTimeoutClassification — row 5: a delay-induced CLIENT timeout on
// the primary advances to the healthy backup. This is the ONLY public observable
// proof that nanollm's internal transport classifier marked the timeout
// retryable: that classifier is private and its Reason never reaches the caller,
// so the test asserts the OUTCOME (backup wins) — never the reason, and NEVER by
// importing internal/ffi.ClassifyError or reproducing the message-sensitive
// classifier.
//
// The short deadline lives on the HTTP CLIENT (timeoutClient), NOT on the shared
// context — a context deadline would also cancel the immediate backup. MaxRetries
// is 0, so the advance is cross-model, not a same-model retry. Assertions are on
// counters + content, not millisecond-exact elapsed time.
func TestFallbackTimeoutClassification(t *testing.T) {
	m := startMock(t)
	client := newFallbackClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const primaryID = "s2b-timeout-primary"
	const backupID = "s2b-timeout-backup"
	const wantText = "timeout-backup-ok"

	// Primary: attempt 1 holds ~2s (a cancel-aware delay), attempt 2 clean. With
	// MaxRetries:0 the client times out on attempt 1 and the chain advances, so
	// the empty attempt-2 slot is never reached. attempt_faults (not a global
	// delay) keeps the fault scoped to the first attempt.
	m.registerScenario(scenarioSpec{
		ID:       primaryID,
		Provider: "openai",
		Model:    fbPrimaryTarget,
		Config:   json.RawMessage(fmt.Sprintf(`{"attempt_faults":[[{"mode":"delay","delay_ms":%d}],[]]}`, primaryDelayMs)),
	})
	// Backup: healthy + immediate.
	m.registerScenario(scenarioSpec{
		ID:       backupID,
		Provider: "anthropic",
		Model:    fbBackupTarget,
		Output:   &exactOutput{Text: wantText, OutputTokens: 7},
		Config:   json.RawMessage(`{"strict":true}`),
	})

	// Generous parent context; the SHORT deadline is on the client only.
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := client.Do(ctx, nanollm.Request{
		Model: fbPrimaryAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(timeoutClient(timeoutClientDeadline)))
	if err != nil {
		t.Fatalf("Do(fallback-timeout): %v", err)
	}

	// The backup's exact 200/content wins.
	assertOpenAIResult(t, resp, wantText, "stop")

	// Each alias was attempted exactly once (primary timed out, backup answered).
	if got := m.scenarioAttemptCount(primaryID); got != 1 {
		t.Errorf("primary attempt count = %d, want 1 (single timed-out attempt)", got)
	}
	if got := m.scenarioAttemptCount(backupID); got != 1 {
		t.Errorf("backup attempt count = %d, want 1", got)
	}

	// The parent context is still live: only the per-attempt CLIENT deadline
	// fired, so the backup was free to succeed on a fresh budget.
	if ctx.Err() != nil {
		t.Errorf("parent context error = %v, want nil (only the client deadline should fire)", ctx.Err())
	}
}

// uniformErrorEnvelope is the LOCAL, narrow view of nanollm's uniform non-2xx
// error envelope — the shape Do returns for every normalized non-2xx. The stable
// details block is nested UNDER the top-level error object (error.details), which
// carries the caller-facing status/provider plus the bounded raw upstream body.
// Assertions against it are STRUCTURAL and additive-field tolerant: they pin the
// stable semantic fields and require the raw upstream to be nested + bounded, but
// never assert an exact key set or JSON byte order.
type uniformErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			StatusCode int    `json:"status_code"`
			Provider   string `json:"provider"`
			Raw        string `json:"raw"`
			Truncated  bool   `json:"truncated"`
		} `json:"details"`
	} `json:"error"`
}

// TestUniformErrorEnvelopeDo — row 6: Do NORMALIZES an Anthropic 529
// overloaded_error into the ONE uniform JSON error envelope. Do returns a
// Response (err == nil) whose Status is the real 529 and whose JSON body carries
// the classified OpenAI-safe error.type, the native type under error.code, the
// marker-bearing message, and details.{status_code,provider} with the raw
// upstream nested + bounded. This is the uniform-envelope boundary; its DoStream
// companion (row 7) deliberately diverges.
func TestUniformErrorEnvelopeDo(t *testing.T) {
	m := startMock(t)
	client := newFaultClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const id = "s2b-uniform-529"
	// Unique marker that must survive into the envelope's error.message and into
	// the raw upstream nested under details.
	const marker = "s2b-uniform-marker-2f7c9a"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "anthropic",
		Model:    faultTarget,
		Config:   json.RawMessage(fmt.Sprintf(`{"faults":[{"mode":"error","error_status":529,"error_type":"overloaded_error","error_message":%q}]}`, marker)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	// WithoutFallback is an explicit guard: this alias has no chain anyway, so
	// the single non-2xx must surface as the normalized Response.
	resp, err := client.Do(ctx, nanollm.Request{
		Model: faultAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()), nanollm.WithoutFallback())
	if err != nil {
		t.Fatalf("Do(uniform-529) returned error, want a normalized Response: %v", err)
	}
	if resp.Status != 529 {
		t.Fatalf("Response.Status = %d, want 529 (body: %s)", resp.Status, bodyDigest(resp.Body))
	}
	if !resp.BodyIsJSON {
		t.Errorf("BodyIsJSON = false, want true")
	}

	// Structural envelope fields.
	var env uniformErrorEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		t.Fatalf("envelope body is not JSON: %v\n%s", err, bodyDigest(resp.Body))
	}
	if env.Error.Type != "server_error" {
		t.Errorf("error.type = %q, want %q (classified OpenAI-safe type)", env.Error.Type, "server_error")
	}
	if env.Error.Code != "overloaded_error" {
		t.Errorf("error.code = %q, want %q (native type preserved)", env.Error.Code, "overloaded_error")
	}
	if !strings.Contains(env.Error.Message, marker) {
		t.Errorf("error.message = %q, want it to carry marker %q", env.Error.Message, marker)
	}
	if env.Error.Details.StatusCode != 529 {
		t.Errorf("error.details.status_code = %d, want 529", env.Error.Details.StatusCode)
	}
	if env.Error.Details.Provider != "anthropic" {
		t.Errorf("error.details.provider = %q, want anthropic", env.Error.Details.Provider)
	}

	// The raw upstream is preserved under bounded error.details.raw — the
	// verbatim provider-native Anthropic error, carrying the go-mocklm request_id
	// and the native overloaded_error type — and flagged as not truncated (the
	// tiny mock error fits well under the raw cap).
	rawUpstream := env.Error.Details.Raw
	if rawUpstream == "" {
		t.Fatalf("error.details.raw is empty, want the raw provider-native Anthropic error")
	}
	if len(rawUpstream) > maxRawBytes {
		t.Errorf("error.details.raw = %d bytes, want <= %d (bounded raw)", len(rawUpstream), maxRawBytes)
	}
	if env.Error.Details.Truncated {
		t.Errorf("error.details.truncated = true, want false (the raw upstream fits under the cap)")
	}
	if !strings.Contains(rawUpstream, marker) || !strings.Contains(rawUpstream, "req_mock_") {
		t.Errorf("error.details.raw = %q, want it to carry the marker and the go-mocklm request_id", strDigest(rawUpstream))
	}

	// The raw Anthropic wrapper (its top-level type:"error" and request_id)
	// survives ONLY nested under details.raw — never dumped at the envelope top
	// level, whose sole member is the classified error object.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &top); err != nil {
		t.Fatalf("envelope top-level is not a JSON object: %v", err)
	}
	if _, ok := top["request_id"]; ok {
		t.Errorf("top-level request_id survived; the raw Anthropic error must not be dumped at the envelope top level")
	}
	if raw, ok := top["type"]; ok {
		var ts string
		_ = json.Unmarshal(raw, &ts)
		if ts == "error" {
			t.Errorf(`top-level type == "error" survived; the raw Anthropic error must not be dumped at the envelope top level`)
		}
	}

	// The scenario fired exactly once.
	if got := m.scenarioAttemptCount(id); got != 1 {
		t.Errorf("scenario attempt count = %d, want 1", got)
	}
}

// TestStreamStartProviderStatusError — row 7: the DoStream companion to row 6,
// documenting the DELIBERATE v0.4.3 "D11" divergence. On a stream-start non-2xx,
// DoStream does NOT normalize into the uniform envelope; it returns a wrapped
// *nanollm.ProviderStatusError carrying the bounded, provider-NATIVE Anthropic
// error body (<= 64 KiB). This is Viktor-confirmed intended behavior, pinned
// upstream by TestDoStreamErrorBody — it must NOT be mislabeled as the uniform
// envelope (row 6), nor hidden behind a baml-rest wrapper.
func TestStreamStartProviderStatusError(t *testing.T) {
	m := startMock(t)
	client := newFaultClient(t, m.baseURL)
	body := sharedNativeBody(t)

	const id = "s2b-d11-529"
	const marker = "s2b-d11-marker-8b13e0"
	m.registerScenario(scenarioSpec{
		ID:       id,
		Provider: "anthropic",
		Model:    faultTarget,
		Config:   json.RawMessage(fmt.Sprintf(`{"faults":[{"mode":"error","error_status":529,"error_type":"overloaded_error","error_message":%q}]}`, marker)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	stream, err := client.DoStream(ctx, nanollm.Request{
		Model: faultAlias,
		Body:  body,
		Type:  nanollm.ChatCompletion,
	}, nanollm.WithHTTPClient(loopbackClient()), nanollm.WithoutFallback())
	if stream != nil {
		stream.Close()
		t.Fatalf("DoStream returned a non-nil stream on a stream-start 529, want nil stream + error")
	}
	if err == nil {
		t.Fatalf("DoStream returned nil error on a stream-start 529, want *ProviderStatusError (D11)")
	}

	// Wrapped through the chain-exhaustion error: use errors.As, not a direct
	// type assertion.
	var statusErr *nanollm.ProviderStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error is not (wrapping) a *ProviderStatusError (D11): %v", err)
	}
	if statusErr.Status != 529 {
		t.Errorf("ProviderStatusError.Status = %d, want 529", statusErr.Status)
	}
	if len(statusErr.Body) == 0 {
		t.Fatalf("ProviderStatusError.Body is empty, want the provider-native Anthropic error JSON")
	}
	if len(statusErr.Body) > maxRawBytes {
		t.Errorf("ProviderStatusError.Body = %d bytes, want <= %d (bounded raw)", len(statusErr.Body), maxRawBytes)
	}

	// The body is the PROVIDER-NATIVE Anthropic error JSON (NOT the uniform
	// envelope): top-level type:"error", a nested error.type carrying the native
	// overloaded_error, and the marker message.
	var native struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(statusErr.Body, &native); err != nil {
		t.Fatalf("ProviderStatusError.Body is not JSON: %v\n%s", err, bodyDigest(statusErr.Body))
	}
	if native.Type != "error" {
		t.Errorf("native top-level type = %q, want \"error\" (raw Anthropic shape, NOT the uniform envelope)", native.Type)
	}
	if native.Error.Type != "overloaded_error" {
		t.Errorf("native error.type = %q, want overloaded_error", native.Error.Type)
	}
	if !strings.Contains(native.Error.Message, marker) {
		t.Errorf("native error.message = %q, want marker %q", native.Error.Message, marker)
	}
	// Confirm this is the RAW body and NOT the uniform envelope: the classified
	// "server_error" type and the details block are absent from a native body.
	if bytes.Contains(statusErr.Body, []byte("server_error")) {
		t.Errorf("D11 body contains \"server_error\"; it must carry the NATIVE error, not the uniform envelope\n%s", bodyDigest(statusErr.Body))
	}
	if bytes.Contains(statusErr.Body, []byte(`"details"`)) {
		t.Errorf("D11 body contains a details block; it must carry the NATIVE error, not the uniform envelope\n%s", bodyDigest(statusErr.Body))
	}

	// The scenario fired exactly once.
	if got := m.scenarioAttemptCount(id); got != 1 {
		t.Errorf("scenario attempt count = %d, want 1", got)
	}
}
