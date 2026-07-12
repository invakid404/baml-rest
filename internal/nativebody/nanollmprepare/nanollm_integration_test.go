//go:build nanollm_integration

package nanollmprepare

// De-BAML Phase 4b: gated, no-send nanollm Prepare validation for the OpenAI
// provider. It proves that the EXACT, already-BAML-byte-proven 4a canonical
// OpenAI chat-completions body (internal/nativebody.BuildOpenAIChat,
// authoritative by the 4a differential oracles) is ACCEPTED and PLANNED by
// nanollm's openai provider Prepare — with NO network and NO send.
//
// This is an ADDITIONAL compatibility gate, NOT a replacement for the 4a BAML
// byte-oracle: nanollm Prepare parses generic JSON and does not strictly
// validate the OpenAI chat schema, so the 4a raw-body oracle stays the
// authoritative parity proof.
//
// It lives in a SEPARATE, test-only module (see go.mod) excluded from go.work,
// so the public github.com/viktordanov/nanollm-ffi/go dependency stays entirely
// out of the root/default module graph — a cold `go build ./...` /
// `go test ./...` (no tags) never needs nanollm, cgo, or a C toolchain. On top
// of that isolation, this file is GATED by `//go:build nanollm_integration`:
//
//   - The public nanollm-ffi module embeds its prebuilt Rust FFI archive
//     per-platform and links it automatically
//     (go/internal/ffi/link_embedded.go), so NO nanollm_prebuilt tag,
//     CGO_CFLAGS, or CGO_LDFLAGS is needed. It is still a cgo package, so
//     CGO_ENABLED=1 and a working C toolchain/linker are required.
//   - nanollm_integration opts this file in.
//
// Run it (local macOS ARM, which links the committed aarch64-apple-darwin
// archive) from inside this module directory:
//
//	CGO_ENABLED=1 GOWORK=off go test -tags "nanollm_integration" ./...
//
// It calls ONLY Client.Prepare — never HTTPRequest, never Do/DoStream, never a
// send. baml-rest keeps retry/fallback/round-robin ownership; nanollm Fallbacks
// are not used.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

const (
	// prepareBaseURL is a deliberately non-routable fake OpenAI base. Prepare is
	// a pure translation (no send), so nothing ever dials it; a `.invalid` host
	// guarantees a would-be send fails loudly rather than escaping the sandbox.
	prepareBaseURL = "http://nanollm-prepare-4b.invalid/v1"
	// prepareAPIKey is a fake key; it must surface verbatim in the Authorization
	// header of the plan (never on any wire).
	prepareAPIKey = "sk-nanollm-prepare-4b-fake"
	// prepareAlias is the SEPARATE baml-rest/nanollm alias — the nanollm
	// Request.Model / ModelConfig.Name. It is deliberately DIFFERENT from every
	// target model below so the model-separation proof is meaningful: the alias
	// must never appear in the prepared JSON body; only the target model does.
	prepareAlias = "nanollm-openai-alias"
)

func sp(s string) *string { return &s }

// chat builds a KindChat RenderedPrompt from ordered messages, using the
// EXPORTED nativeprompt surface (this is an external test in a separate module,
// so the in-package 4a test helpers are not visible).
func chat(msgs ...nativeprompt.RenderedMessage) *nativeprompt.RenderedPrompt {
	return &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindChat, Messages: msgs}
}

func msg(role string, texts ...string) nativeprompt.RenderedMessage {
	parts := make([]nativeprompt.Part, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, nativeprompt.Part{Text: sp(t)})
	}
	return nativeprompt.RenderedMessage{Role: role, Parts: parts}
}

// prepareCase is one 4a-admitted fixture: a rendered prompt plus its literal
// BAML target model, and the EXACT canonical OpenAI body BAML v0.223 emits for
// it. The want bytes are the same byte-exact strings the 4a builder pins in
// TestBuildOpenAIChatExactBytes / the dynamic oracle — reproduced here so 4b
// asserts against the concrete BAML-proven bytes, not merely against whatever
// BuildOpenAIChat happens to emit.
type prepareCase struct {
	name        string
	rendered    *nativeprompt.RenderedPrompt
	targetModel string
	wantBody    string
}

// prepareCases enumerates the 4a-admitted, BAML-byte-proven surface: a
// completion realized as a single system message, single/ordered text-only chat
// messages across the proven system/user/assistant roles, and the
// escaping-sensitive case (HTML chars, quote, slash, non-ASCII) that pins
// serde_json escaping parity. Each is admitted by SupportsOpenAIChat and its
// wantBody is byte-identical to BAML's captured request body.
func prepareCases() []prepareCase {
	return []prepareCase{
		{
			name:        "completion_realized_as_system_message",
			rendered:    &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: "Write a short, plain note about cats and dogs."},
			targetModel: "fake-static-oracle-model",
			wantBody:    `{"model":"fake-static-oracle-model","messages":[{"role":"system","content":[{"type":"text","text":"Write a short, plain note about cats and dogs."}]}]}`,
		},
		{
			name:        "single_user_message",
			rendered:    chat(msg("user", "What is 2+2?")),
			targetModel: "gpt-4o-mini",
			wantBody:    `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"What is 2+2?"}]}]}`,
		},
		{
			name: "ordered_system_user_assistant",
			rendered: chat(
				msg("system", "You are a concise assistant."),
				msg("user", "Tell me about weather."),
				msg("assistant", "Here are 3 facts."),
			),
			targetModel: "gpt-4o-mini",
			wantBody:    `{"model":"gpt-4o-mini","messages":[{"role":"system","content":[{"type":"text","text":"You are a concise assistant."}]},{"role":"user","content":[{"type":"text","text":"Tell me about weather."}]},{"role":"assistant","content":[{"type":"text","text":"Here are 3 facts."}]}]}`,
		},
		{
			name:        "special_characters_escaping",
			rendered:    chat(msg("user", `html <b>&</b>, quote "q", slash a/b, unicode café ☕`)),
			targetModel: "gpt-4o-mini",
			wantBody:    `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"html <b>&</b>, quote \"q\", slash a/b, unicode café ☕"}]}]}`,
		},
	}
}

// newPrepareClient builds a nanollm engine with ONE test-only openai alias whose
// ModelConfig.Model is "openai/<target>" — the load-bearing separation: the
// provider/target lives in the config, the alias (Name) is what a Request
// references. Fake base URL + key; no send is ever performed.
func newPrepareClient(t *testing.T, target string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:    prepareAlias,
			Model:   "openai/" + target,
			APIKey:  prepareAPIKey,
			BaseURL: prepareBaseURL,
		}},
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestNanollmPrepareAcceptsCanonical4aBody is the core 4b proof. For every
// 4a-admitted case it builds the native canonical body, hands it to nanollm's
// openai Prepare as {alias, native raw body, ChatCompletion, non-stream}, and
// asserts the returned plan is exactly the intended OpenAI request: Prepare
// succeeds, the plan metadata resolves the alias/target/provider/request-type +
// non-stream, method/URL/ordered headers/response-format match, and the plan
// body equals the 4a body byte-for-byte (the model rewrite is a no-op because
// the alias target equals the BAML target). No server or round-tripper starts
// and HTTPRequest/Do/DoStream are never called.
func TestNanollmPrepareAcceptsCanonical4aBody(t *testing.T) {
	// No-send guard: poison the default HTTP transport so any accidental Go-level
	// send during Prepare fails the test loudly. Prepare is a pure FFI translation
	// and never touches net/http; this makes "no round-tripper starts" concrete.
	defer noSendGuard(t)()

	for _, tc := range prepareCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Build the native canonical body (must be admitted). ClientIntent carries
			// the SEPARATE alias as metadata only; the JSON model is the target.
			body, err := nativebody.BuildOpenAIChat(tc.rendered, nativebody.ClientIntent{
				Provider:    nativebody.ProviderOpenAI,
				TargetModel: tc.targetModel,
				ModelAlias:  prepareAlias,
			})
			if err != nil {
				t.Fatalf("BuildOpenAIChat declined a 4a-admitted case: %v", err)
			}
			// The native body must be the exact BAML-proven bytes before we even ask
			// nanollm to plan it (4a authority; 4b builds on it).
			if got := string(body.Bytes()); got != tc.wantBody {
				t.Fatalf("4a body drifted from the BAML-proven bytes\n got: %s\nwant: %s", got, tc.wantBody)
			}

			client := newPrepareClient(t, tc.targetModel)

			prep, err := client.Prepare(nanollm.Request{
				Model:  prepareAlias,
				Body:   body.Bytes(),
				Type:   nanollm.ChatCompletion,
				Stream: false,
			})
			if err != nil {
				t.Fatalf("Prepare rejected a 4a-admitted body: %v", err)
			}

			// --- method / URL / ordered headers / response format ---
			if prep.Method != http.MethodPost {
				t.Errorf("method = %q, want POST", prep.Method)
			}
			wantURL := prepareBaseURL + "/chat/completions"
			if prep.URL != wantURL {
				t.Errorf("url = %q, want %q", prep.URL, wantURL)
			}
			wantHeaders := [][2]string{
				{"Content-Type", "application/json"},
				{"Authorization", "Bearer " + prepareAPIKey},
			}
			if !equalHeaders(prep.Headers, wantHeaders) {
				t.Errorf("ordered headers = %v, want %v", prep.Headers, wantHeaders)
			}
			if prep.ResponseFormat != nanollm.FormatJSON {
				t.Errorf("response format = %q, want json", prep.ResponseFormat)
			}

			// --- plan metadata resolves alias / target / provider / type / non-stream ---
			if prep.Meta.ModelAlias != prepareAlias {
				t.Errorf("meta.ModelAlias = %q, want %q", prep.Meta.ModelAlias, prepareAlias)
			}
			if prep.Meta.TargetModel != tc.targetModel {
				t.Errorf("meta.TargetModel = %q, want %q", prep.Meta.TargetModel, tc.targetModel)
			}
			if prep.Meta.Provider != nativebody.ProviderOpenAI {
				t.Errorf("meta.Provider = %q, want %q", prep.Meta.Provider, nativebody.ProviderOpenAI)
			}
			if prep.Meta.RequestType != nanollm.ChatCompletion {
				t.Errorf("meta.RequestType = %q, want %q", prep.Meta.RequestType, nanollm.ChatCompletion)
			}
			if prep.Meta.Stream {
				t.Errorf("meta.Stream = true, want false (non-streaming attempt)")
			}
			// An OpenAI plan is unsigned: it must not carry a SigV4 expiry.
			if prep.Meta.ExpiresAt != nil {
				t.Errorf("unsigned OpenAI plan must not carry an expiry, got %v", prep.Meta.ExpiresAt)
			}

			// --- body: byte-identical to the 4a body (== the BAML-proven bytes) ---
			if got := string(prep.Body); got != tc.wantBody {
				t.Errorf("prepared body diverged from the 4a body\n got: %s\nwant: %s", got, tc.wantBody)
			}
			// Model separation: the alias must NEVER leak into the JSON body; only the
			// target model does. (prepareAlias is chosen distinct from every target.)
			if strings.Contains(string(prep.Body), prepareAlias) {
				t.Errorf("nanollm alias leaked into the prepared JSON body: %s", prep.Body)
			}
		})
	}
}

// TestNanollmPrepareModelSeparation pins the load-bearing model separation: the
// alias selects the ModelConfig, and nanollm's openai prep rewrites the
// TOP-LEVEL JSON model to the configured TARGET — the alias string is never
// substituted into the JSON model field. It proves both directions:
//
//   - target == body model  -> rewrite is a no-op, body preserved byte-for-byte;
//   - target != body model  -> the JSON model is rewritten to the configured
//     target (never to the alias), the rest of the body preserved.
func TestNanollmPrepareModelSeparation(t *testing.T) {
	defer noSendGuard(t)()

	// (1) Alias target EQUALS the body's model -> no-op rewrite, exact bytes.
	same := newPrepareClient(t, "gpt-4o-mini")
	body, err := nativebody.BuildOpenAIChat(chat(msg("user", "hi")),
		nativebody.ClientIntent{Provider: nativebody.ProviderOpenAI, TargetModel: "gpt-4o-mini", ModelAlias: prepareAlias})
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	prep, err := same.Prepare(nanollm.Request{Model: prepareAlias, Body: body.Bytes(), Type: nanollm.ChatCompletion})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if string(prep.Body) != string(body.Bytes()) {
		t.Errorf("no-op rewrite changed bytes\n got: %s\nwant: %s", prep.Body, body.Bytes())
	}
	if !strings.Contains(string(prep.Body), `"model":"gpt-4o-mini"`) {
		t.Errorf("JSON model must be the target, got %s", prep.Body)
	}

	// (2) Alias target DIFFERS from the body's model -> nanollm rewrites the JSON
	// model to the CONFIGURED target. This is the parity mechanism the scope
	// relies on: configure the alias with openai/<BAML target> and nanollm leaves
	// the intended target in the prepared body. The alias name never appears.
	const configuredTarget = "rewrite-target-model"
	rw := newPrepareClient(t, configuredTarget)
	// A body whose model is something OTHER than the configured target.
	other, err := nativebody.BuildOpenAIChat(chat(msg("user", "hi")),
		nativebody.ClientIntent{Provider: nativebody.ProviderOpenAI, TargetModel: "placeholder-model", ModelAlias: prepareAlias})
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	rwPrep, err := rw.Prepare(nanollm.Request{Model: prepareAlias, Body: other.Bytes(), Type: nanollm.ChatCompletion})
	if err != nil {
		t.Fatalf("Prepare(rewrite): %v", err)
	}
	if !strings.Contains(string(rwPrep.Body), `"model":"`+configuredTarget+`"`) {
		t.Errorf("nanollm must rewrite the JSON model to the configured target %q, got %s", configuredTarget, rwPrep.Body)
	}
	if strings.Contains(string(rwPrep.Body), "placeholder-model") {
		t.Errorf("the original body model should have been rewritten away, got %s", rwPrep.Body)
	}
	if strings.Contains(string(rwPrep.Body), prepareAlias) {
		t.Errorf("the alias must never be substituted into the JSON model, got %s", rwPrep.Body)
	}
	if prep.Meta.TargetModel == prepareAlias || rwPrep.Meta.TargetModel == prepareAlias {
		t.Errorf("meta.TargetModel must be the resolved target, never the alias")
	}
}

// equalHeaders reports exact ordered equality of two header pair lists. 4b
// asserts the ENTIRE ordered list (not a subset): the ordered header pairs are
// part of what Phase 5 will compare against BAML, so any addition/reorder must
// be caught here.
func equalHeaders(got, want [][2]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
			return false
		}
	}
	return true
}

// failingRoundTripper fails the test if any Go-level HTTP send is attempted.
type failingRoundTripper struct{ t *testing.T }

func (f failingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Fatalf("no-send violated: an HTTP request was issued to %s during Prepare", req.URL)
	return nil, nil
}

// noSendGuard swaps http.DefaultTransport for a failing transport so any
// accidental Go-level send during Prepare aborts the test, and returns a
// restore func. Prepare is a pure FFI translation that never dials, so this
// guard must never fire — it concretely backs the "no server/round-tripper
// starts, HTTPRequest is never called" contract.
func noSendGuard(t *testing.T) func() {
	prev := http.DefaultTransport
	http.DefaultTransport = failingRoundTripper{t: t}
	return func() { http.DefaultTransport = prev }
}
