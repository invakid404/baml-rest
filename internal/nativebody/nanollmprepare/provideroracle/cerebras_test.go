//go:build integration && nanollm_integration

package provideroracle

// TPBP-C Cerebras PROVIDER-CORRECTNESS oracle (scope §2.4/§3.3/§4-Cerebras/§9-C).
//
// # The bar (owner-ruled): provider-correctness, NOT "BAML Cerebras parity"
//
// The CRUX of this slice: BAML v0.223.0 has NO `cerebras` provider — for the
// literal `provider "cerebras"` spelling it builds NO request at all
// (clientspec.rs does not list cerebras; the in-memory runtime is REJECTED). So a
// literal BAML-vs-native byte oracle for Cerebras is undefined. This suite proves
// that rejection directly (TestCerebrasLiteralProviderRejected) and then proves
// native emits a PROVIDER-CORRECT OpenAI-compatible Cerebras request — correct
// method / endpoint (`POST <api-root>/chat/completions`) / Bearer auth / JSON
// content-type / ordered content-part-array body carrying every mapped value.
//
// BAML documents Cerebras through `provider "openai-generic"` (base
// `https://api.cerebras.ai/v1`). That request is used ONLY as an EXPLICITLY
// LABELED SURROGATE (TestCerebrasOpenAIGenericSurrogate): it agrees on method /
// URL / Bearer-auth / JSON, but the surrogate emits text as a JSON STRING while
// native emits an ordered content-part ARRAY — a documented, provider-valid
// OpenAI-compatible shape divergence, NOT a missing literal byte match. Nothing in
// this file is ever called "literal Cerebras byte parity"; the surrogate bodies
// are asserted to be deliberately NON-byte-equal.
//
// # Consumes the shared TPBP-A harness (does NOT rebuild it)
//
// It reuses common_test.go / baml_stock_test.go verbatim: the in-memory stock-BAML
// capture (captureBAMLAnthropic — provider-NEUTRAL; the name reflects its TPBP-A
// origin, not an Anthropic constraint), the canonical nanollm ChatRequest builder
// (buildCanonicalRequest / prepareNative), the redacted nativeserve/testutil
// comparator, and the exact top-level-order primitives (topLevelKeys / sameOrder).
// The `baml-original-url` transport-header exemption stays ONE-SIDED (native must
// never emit it). Diagnostics are secret-free: bodies/URLs/keys never print raw —
// mismatches report a field path plus a length or a fixed structural token.
//
// # Isolation
//
// Every file is `_test.go` behind `integration && nanollm_integration`, in a file
// SEPARATE from anthropic_test.go / bedrock_test.go so TPBP-B/C merge sequentially.
// No production / admission / go.mod / tar change.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// --- The Cerebras fake fence (secret-free; never dialed, never a real key). ---
//
// A `.invalid` API root is non-routable and this suite never sends. Distinct from
// the Anthropic fence so a leaked Anthropic value in a Cerebras body would fail.
const (
	fenceCerebrasModel  = "cerebras-fixture-model"
	fenceCerebrasAPIKey = "fake-cerebras-oracle-key"

	// fenceCerebrasAPIRoot is the OpenAI-compatible API ROOT (already carries /v1).
	// BOTH legs append /chat/completions to it — for Cerebras the base is used
	// VERBATIM (no service-root->API-root /v1 adaptation, unlike Anthropic).
	fenceCerebrasAPIRoot = "https://cerebras-oracle.invalid/v1"

	// cerebrasDefaultChatURL is nanollm's built-in Cerebras endpoint when no
	// base_url is configured — the provider default service root + the OpenAI path.
	cerebrasDefaultChatURL = "https://api.cerebras.ai/v1/chat/completions"

	cerebrasChatPath = "/chat/completions"
)

// cerebrasChatURL returns the exact Cerebras Chat Completions endpoint for an API
// root (or the provider default when apiRoot is empty).
func cerebrasChatURL(apiRoot string) string {
	if apiRoot == "" {
		return cerebrasDefaultChatURL
	}
	return apiRoot + cerebrasChatPath
}

// --- nanollm (native) Cerebras leg. ---

// newCerebrasClient builds a native nanollm engine for one Cerebras alias at the
// fake fence values: model `cerebras/<target>`, a zero retry budget, no fallbacks,
// no process env, optional custom headers (injected via Params["headers"] exactly
// as S2's newMappedClient does). apiRoot is used VERBATIM as the nanollm base (no
// /v1 adaptation), or left unset for the provider default.
func newCerebrasClient(t *testing.T, apiRoot string, customHeaders map[string]string) *nanollm.Client {
	t.Helper()
	mc := nanollm.ModelConfig{
		Name:       fenceAlias,
		Model:      "cerebras/" + fenceCerebrasModel,
		APIKey:     fenceCerebrasAPIKey,
		MaxRetries: 0,
	}
	if apiRoot != "" {
		mc.BaseURL = apiRoot
	}
	if len(customHeaders) > 0 {
		mc.Params = map[string]any{"headers": customHeaders}
	}
	c, err := nanollm.New(nanollm.Config{
		Models:        []nanollm.ModelConfig{mc},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		// The engine error can echo the config (fake key/base) — report the stage only.
		t.Fatal("nanollm.New(cerebras) failed at stage=engine_new")
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// --- Cerebras / OpenAI-compatible contract DTO (required-meaning comparison). ---

// cerebrasContract is the subset of an OpenAI-compatible Chat Completions request
// whose MEANING both legs must agree on. Content is kept as RawMessage so the
// string-vs-content-part-array divergence can be classified WITHOUT collapsing it.
type cerebrasContract struct {
	Model       string            `json:"model"`
	Messages    []cerebrasMessage `json:"messages"`
	Temperature *float64          `json:"temperature"`
	TopP        *float64          `json:"top_p"`
	MaxTokens   *int              `json:"max_tokens"`
	Stop        []string          `json:"stop"`
}

type cerebrasMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Fixed, secret-free strict-decode outcomes (never echo body bytes/field values).
var (
	errCerebrasDecode   = errors.New("provideroracle: body is not a recognized single OpenAI-compatible chat request")
	errCerebrasTrailing = errors.New("provideroracle: cerebras body carries trailing data after the JSON value")
)

// strictDecodeCerebras is the fail-closed decode shared by every cerebras site: it
// rejects unknown top-level/message fields (DisallowUnknownFields) and requires
// EXACTLY ONE top-level JSON value (trailing JSON rejected; trailing whitespace
// tolerated). It returns only fixed sentinels — never the raw decoder error.
func strictDecodeCerebras(body []byte) (cerebrasContract, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var c cerebrasContract
	if err := dec.Decode(&c); err != nil {
		return cerebrasContract{}, errCerebrasDecode
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return cerebrasContract{}, errCerebrasTrailing
	}
	return c, nil
}

// decodeCerebras strictly decodes a captured/prepared body, printing only a fixed
// stage token + length on error (never the raw body).
func decodeCerebras(t *testing.T, leg string, body []byte) cerebrasContract {
	t.Helper()
	c, err := strictDecodeCerebras(body)
	if err != nil {
		stage := "decode"
		if errors.Is(err, errCerebrasTrailing) {
			stage = "trailing_json"
		}
		t.Fatalf("%s body is not a recognized single OpenAI-compatible chat request (stage=%s, len=%d)", leg, stage, len(body))
	}
	return c
}

// --- content-representation classification (the surrogate-divergence primitive). ---

// cerebrasContentKind classifies a message `content` value by its JSON shape:
// "string" (the BAML openai-generic surrogate's concatenated text), "array" (the
// native ordered content-part blocks), or "other". It reads only the first
// non-space byte — never the value.
func cerebrasContentKind(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "other"
	}
	switch trimmed[0] {
	case '"':
		return "string"
	case '[':
		return "array"
	default:
		return "other"
	}
}

func cerebrasContentString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// cerebrasContentParts decodes a content-part array into the shared canonTextBlock
// shape ({type,text}); false if it is not an array.
func cerebrasContentParts(raw json.RawMessage) ([]canonTextBlock, bool) {
	if cerebrasContentKind(raw) != "array" {
		return nil, false
	}
	var parts []canonTextBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, false
	}
	return parts, true
}

// --- BAML in-memory source generation. ---

// cerebrasClientSource renders a one-client, one-function project for the given
// PROVIDER token (a bareword like `cerebras` or a quoted `"openai-generic"`) and a
// verbatim options block — used to prove the literal cerebras rejection against an
// otherwise identical openai-generic control.
func cerebrasClientSource(provider, options string) string {
	return fmt.Sprintf(`
client<llm> OracleCerebras {
  provider %s
  options {
%s
  }
}

function OracleFn() -> string {
  client OracleCerebras
  prompt #"
    {{ _.role("user") }}
    Tell me about cats.
  "#
}
`, provider, options)
}

// bamlOpenAIGenericSource renders the tiny in-memory `openai-generic` SURROGATE
// project for the surrogate row: one openai-generic client at the fence values and
// one zero-arg function whose prompt is the row's ordered role turns.
func bamlOpenAIGenericSource(apiRoot string, msgs []corpusMessage) (src, fn string) {
	fn = "OracleFn"
	var prompt strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&prompt, "    {{ _.role(%q) }}\n    %s\n", m.role, bamlPromptLine(m.text))
	}
	src = fmt.Sprintf(`
client<llm> OracleCerebras {
  provider "openai-generic"
  options {
    model %q
    api_key %q
    base_url %q
  }
}

function %s() -> string {
  client OracleCerebras
  prompt #"
%s  "#
}
`, fenceCerebrasModel, fenceCerebrasAPIKey, apiRoot, fn, prompt.String())
	return src, fn
}

// captureBAMLGeneric is a readability alias for the shared, provider-NEUTRAL
// in-memory BAML capture (baml_stock_test.go's captureBAMLAnthropic — named for
// its TPBP-A origin, not an Anthropic constraint). The surrogate uses it unchanged:
// CONSUME the shared harness, don't rebuild it.
func captureBAMLGeneric(t *testing.T, src, fn string) bamlCapture {
	t.Helper()
	return captureBAMLAnthropic(t, src, fn, nil)
}

// cerebrasSurrogatePair builds the paired legs the surrogate row and its header/
// path controls all share at the SAME fence API root: the BAML `openai-generic`
// SURROGATE capture and the native Cerebras Prepare, over one fixed user turn with
// no options. It returns the row messages too so the surrogate divergence assertion
// pins the EXACT turns the request was built from — a single source of truth keeps
// all five call sites in sync if the message / api root / options ever change.
func cerebrasSurrogatePair(t *testing.T) (msgs []corpusMessage, bamlCap bamlCapture, prep *nanollm.PreparedRequest, nanoSnap testutil.Snapshot) {
	t.Helper()
	msgs = []corpusMessage{{role: "user", text: "Tell me about cats."}}
	src, fn := bamlOpenAIGenericSource(fenceCerebrasAPIRoot, msgs)
	bamlCap = captureBAMLGeneric(t, src, fn)
	client := newCerebrasClient(t, fenceCerebrasAPIRoot, nil)
	prep, nanoSnap = prepareNative(t, client, buildCanonicalRequest(t, msgs, bodyOptions{maxTokens: -1}))
	return msgs, bamlCap, prep, nanoSnap
}

// tryBuildBAMLRequest attempts the FULL in-memory BAML build for a single-client
// project and reports whether a provider request was actually produced. It NEVER
// sends and NEVER surfaces the raw CFFI/runtime error (which could echo the
// generated source or the fake credential) — the boolean IS the observation, so a
// rejection at ANY stage (runtime creation, arg encoding, request build, empty
// body) reads uniformly as "no request".
func tryBuildBAMLRequest(t *testing.T, src string) bool {
	t.Helper()
	rt, err := baml.CreateRuntime(".", map[string]string{"main.baml": src}, map[string]string{})
	if err != nil {
		return false
	}
	args := baml.BamlFunctionArguments{Kwargs: map[string]any{"stream": false}, Env: map[string]string{}}
	encoded, err := args.Encode()
	if err != nil {
		return false
	}
	req, err := rt.BuildRequest(context.Background(), "OracleFn", encoded)
	if err != nil {
		return false
	}
	bodyObj, err := req.Body()
	if err != nil {
		return false
	}
	bodyText, err := bodyObj.Text()
	if err != nil {
		return false
	}
	return bodyText != ""
}

// ============================================================================
// (1) CRUX — literal BAML `provider "cerebras"` is REJECTED / builds no request.
// ============================================================================

// TestCerebrasLiteralProviderRejected proves BAML v0.223.0 builds NO request for
// the literal `cerebras` provider (bareword and quoted spellings), and proves the
// SAME options under `provider "openai-generic"` DO build a request — isolating the
// rejection to the provider spelling, so a green result cannot be attributed to an
// unrelated defect in the mini-project. This is the anti-oracle the whole slice
// rests on: there is no literal BAML Cerebras request to compare against.
func TestCerebrasLiteralProviderRejected(t *testing.T) {
	options := fmt.Sprintf("    model %q\n    api_key %q\n    base_url %q",
		fenceCerebrasModel, fenceCerebrasAPIKey, fenceCerebrasAPIRoot)

	for _, spelling := range []string{"cerebras", `"cerebras"`} {
		if tryBuildBAMLRequest(t, cerebrasClientSource(spelling, options)) {
			t.Errorf("BAML v0.223.0 unexpectedly built a request for provider %s; it has NO cerebras provider (no literal oracle exists)", spelling)
		}
	}

	// Control: the identical options under openai-generic MUST build a request —
	// otherwise the rejection above could be blamed on the mini-project, not the
	// cerebras provider spelling.
	if !tryBuildBAMLRequest(t, cerebrasClientSource(`"openai-generic"`, options)) {
		t.Error("control: BAML failed to build a request for the openai-generic surrogate with identical options; the cerebras rejection cannot be attributed to the provider spelling")
	}
}

// ============================================================================
// (2) Native provider-correctness corpus (default + custom root, options, header).
// ============================================================================

// cerebrasNativeRow is one native-only provider-correctness case. There is no
// cross-leg BAML comparison here (no literal Cerebras oracle exists); each row
// pins native's exact endpoint / auth / body / mapped values against the Cerebras
// contract, and its exact top-level member order (nanoOrder).
type cerebrasNativeRow struct {
	name          string
	apiRoot       string // "" => provider default (api.cerebras.ai/v1)
	messages      []corpusMessage
	opts          bodyOptions
	customHeaders map[string]string
	nanoOrder     []string
}

func cerebrasNativeCorpus() []cerebrasNativeRow {
	sys := corpusMessage{role: "system", text: "You are a concise assistant."}
	return []cerebrasNativeRow{
		{
			// default API root; system + user, both as content-part arrays (OpenAI-
			// compatible: the system role STAYS in messages, unlike Anthropic).
			name:      "default_root_system_user",
			apiRoot:   "",
			messages:  []corpusMessage{sys, {role: "user", text: "Tell me about cats."}},
			opts:      bodyOptions{maxTokens: -1},
			nanoOrder: []string{"model", "messages"},
		},
		{
			// custom API root; user only. Cerebras synthesizes NO default max_tokens.
			name:      "custom_root_user",
			apiRoot:   fenceCerebrasAPIRoot,
			messages:  []corpusMessage{{role: "user", text: "Name one fact about the moon."}},
			opts:      bodyOptions{maxTokens: -1},
			nanoOrder: []string{"model", "messages"},
		},
		{
			// mapped sampling/stop fields + Unicode & escaping.
			name:    "sampling_options_unicode_escaping",
			apiRoot: fenceCerebrasAPIRoot,
			messages: []corpusMessage{
				sys,
				{role: "user", text: `Escape café ☕ "q" and a backslash \b then 🙂.`},
			},
			opts: bodyOptions{
				maxTokens:     100,
				temperature:   f64(0.7),
				topP:          f64(0.9),
				stopSequences: []string{"STOP", "END"},
			},
			nanoOrder: []string{"model", "messages", "temperature", "max_tokens", "top_p", "stop"},
		},
		{
			// one safe custom header; user only.
			name:          "custom_header_user_only",
			apiRoot:       fenceCerebrasAPIRoot,
			customHeaders: map[string]string{fenceCustomHdrKey: fenceCustomHdrVal},
			messages:      []corpusMessage{{role: "user", text: "Ping."}},
			opts:          bodyOptions{maxTokens: -1},
			nanoOrder:     []string{"model", "messages"},
		},
	}
}

// TestCerebrasNativeProviderCorrectness pins native's Cerebras/OpenAI-compatible
// request contract over the corpus: POST, the exact endpoint, Bearer auth + JSON
// (and NO Anthropic x-api-key), ordered content-part-array messages carrying every
// text, the fence model, the mapped sampling/stop fields, native's own header
// order, and the exact top-level member order.
func TestCerebrasNativeProviderCorrectness(t *testing.T) {
	for _, r := range cerebrasNativeCorpus() {
		r := r
		t.Run(r.name, func(t *testing.T) {
			client := newCerebrasClient(t, r.apiRoot, r.customHeaders)
			prep, snap := prepareNative(t, client, buildCanonicalRequest(t, r.messages, r.opts))
			wantURL := cerebrasChatURL(r.apiRoot)

			// (a) method + endpoint.
			if snap.Method != "POST" {
				t.Errorf("method = %q, want POST", snap.Method)
			}
			if snap.URL != wantURL {
				// Never print the URL (query creds) — report length + fixed expected only.
				t.Errorf("endpoint mismatch: got_len=%d want=%q", len(snap.URL), wantURL)
			}

			// (b) Bearer auth + JSON content-type; NO Anthropic x-api-key.
			assertCerebrasAuthJSON(t, "nanollm", snap.Headers)

			// (c) required provider meaning: model, content-part messages, options.
			c := decodeCerebras(t, "nanollm", prep.Body)
			if c.Model != fenceCerebrasModel {
				t.Errorf("model = %q, want %q", c.Model, fenceCerebrasModel)
			}
			assertCerebrasContentParts(t, "nanollm", c, r.messages)
			assertCerebrasMappedOptions(t, "nanollm", c, r.opts)

			// (d) native's OWN header order (BAML has no Cerebras leg to compare to).
			assertNativeCerebrasHeaderOrder(t, prep, r)

			// (e) exact top-level member order (no unlisted reorder/new key).
			nanoKeys := topLevelKeys(t, "nanollm", prep.Body)
			if !sameOrder(nanoKeys, r.nanoOrder) {
				t.Errorf("native top-level order = %v, want allowlisted %v", nanoKeys, r.nanoOrder)
			}

			// (f) native must NEVER emit BAML's transport-only header.
			if _, ok := snap.Headers["baml-original-url"]; ok {
				t.Error("native must NOT emit baml-original-url")
			}
		})
	}
}

// ============================================================================
// (3) The EXPLICITLY LABELED openai-generic SURROGATE row.
// ============================================================================

// TestCerebrasOpenAIGenericSurrogate characterizes the BAML `openai-generic`
// SURROGATE against native Cerebras at the SAME API root. It is NEVER "literal
// Cerebras byte parity": the two agree on method / URL / Bearer-auth / JSON and the
// same semantic text, but the surrogate emits `content` as a JSON STRING while
// native emits an ordered content-part ARRAY — the documented, provider-valid
// OpenAI-compatible shape divergence. The bodies are asserted NON-byte-equal, and
// `baml-original-url` is proven one-sided.
func TestCerebrasOpenAIGenericSurrogate(t *testing.T) {
	// BAML surrogate leg (openai-generic) + native Cerebras leg, SAME API root.
	msgs, bamlCap, prep, nanoSnap := cerebrasSurrogatePair(t)

	wantURL := cerebrasChatURL(fenceCerebrasAPIRoot)

	// (1) method + endpoint agree on BOTH legs.
	for _, leg := range []struct {
		name string
		snap testutil.Snapshot
	}{{"baml", bamlCap.snap}, {"nanollm", nanoSnap}} {
		if leg.snap.Method != "POST" {
			t.Errorf("%s method = %q, want POST", leg.name, leg.snap.Method)
		}
		if leg.snap.URL != wantURL {
			t.Errorf("%s endpoint mismatch: got_len=%d want=%q", leg.name, len(leg.snap.URL), wantURL)
		}
	}

	// (2) method + URL + required semantic header SET (Bearer + JSON) agree via the
	// shared redacted comparator, body EXCLUDED. baml-original-url is exempted on the
	// oracle side by testutil; any OTHER header mismatch fails here.
	if diffs := testutil.Diff(headerOnly(bamlCap.snap), headerOnly(nanoSnap)); len(diffs) != 0 {
		t.Errorf("method/URL/semantic-header differential mismatch:\n%s", strings.Join(diffs, "\n"))
	}

	// (3) Bearer + JSON on both legs (absolute anchors, not merely baml==nano).
	assertCerebrasAuthJSON(t, "baml", bamlCap.snap.Headers)
	assertCerebrasAuthJSON(t, "nanollm", nanoSnap.Headers)

	// (4) same model + roles.
	bamlC := decodeCerebras(t, "baml", bamlCap.body)
	nanoC := decodeCerebras(t, "nanollm", prep.Body)
	if bamlC.Model != fenceCerebrasModel || nanoC.Model != fenceCerebrasModel {
		t.Errorf("model mismatch (baml=%q nanollm=%q want=%q)", bamlC.Model, nanoC.Model, fenceCerebrasModel)
	}

	// (5) THE surrogate divergence: BAML string content vs native content-part array,
	// same semantic text.
	assertSurrogateContentDivergence(t, bamlC, nanoC, msgs)

	// (6) one-sided baml-original-url.
	_, bamlTransport := testutil.SplitSemantic(bamlCap.snap.Headers)
	if _, ok := bamlTransport["baml-original-url"]; !ok {
		t.Error("expected the openai-generic surrogate to carry its internal baml-original-url transport header")
	}
	if len(bamlTransport) != 1 {
		t.Errorf("surrogate carries %d transport-only headers, want exactly baml-original-url", len(bamlTransport))
	}
	if _, ok := nanoSnap.Headers["baml-original-url"]; ok {
		t.Error("native must NOT emit baml-original-url")
	}

	// (7) NOT literal byte parity: the surrogate and native bodies deliberately DIVERGE.
	if bytes.Equal(bamlCap.body, prep.Body) {
		t.Error("the openai-generic surrogate body is byte-equal to native; this row is a SURROGATE characterization, NOT literal Cerebras byte parity — the string-vs-array divergence must hold")
	}
}

// ============================================================================
// Shared assertion helpers (Cerebras-specific; redacted diagnostics).
// ============================================================================

// assertCerebrasAuthJSON pins Bearer auth + JSON content-type on a leg and proves
// NO Anthropic x-api-key rode along. The bearer VALUE is checked but reported by
// presence/match only (authorization is not on testutil's safe-print allowlist).
func assertCerebrasAuthJSON(t *testing.T, leg string, headers map[string]string) {
	t.Helper()
	auth, ok := testutil.Authorization(headers)
	wantAuth := "Bearer " + fenceCerebrasAPIKey
	if !ok || auth != wantAuth {
		t.Errorf("%s: authorization missing or not the fence Bearer key (present=%v, matched=%v)", leg, ok, auth == wantAuth)
	}
	if v, ok := headers["content-type"]; !ok || !strings.HasPrefix(v, "application/json") {
		t.Errorf("%s: content-type = %q (present=%v), want application/json", leg, v, ok)
	}
	if _, ok := headers["x-api-key"]; ok {
		t.Errorf("%s: unexpected Anthropic-style x-api-key on a Cerebras request", leg)
	}
}

// assertCerebrasContentParts requires each message to be an ordered content-part
// ARRAY of exactly one {type:"text", text} block matching the row input, in order,
// with the role preserved (OpenAI-compatible: system/developer are NOT lifted).
func assertCerebrasContentParts(t *testing.T, leg string, c cerebrasContract, msgs []corpusMessage) {
	t.Helper()
	if len(c.Messages) != len(msgs) {
		t.Fatalf("%s: messages count = %d, want %d", leg, len(c.Messages), len(msgs))
	}
	for i, m := range c.Messages {
		if m.Role != msgs[i].role {
			t.Errorf("%s: message[%d] role = %q, want %q", leg, i, m.Role, msgs[i].role)
		}
		if kind := cerebrasContentKind(m.Content); kind != "array" {
			t.Errorf("%s: message[%d] content is not a content-part array (kind=%s)", leg, i, kind)
			continue
		}
		parts, ok := cerebrasContentParts(m.Content)
		if !ok || len(parts) != 1 {
			t.Errorf("%s: message[%d] content-part array is not exactly one block", leg, i)
			continue
		}
		if parts[0].Type != "text" {
			t.Errorf("%s: message[%d] block type = %q, want text", leg, i, parts[0].Type)
		}
		if parts[0].Text != msgs[i].text {
			t.Errorf("%s: message[%d] text mismatch (got_len=%d want_len=%d)", leg, i, len(parts[0].Text), len(msgs[i].text))
		}
	}
}

// assertCerebrasMappedOptions pins the mapped sampling/stop fields. Cerebras
// synthesizes NO default max_tokens (unlike Anthropic), so an unset row must carry
// NO max_tokens member.
func assertCerebrasMappedOptions(t *testing.T, leg string, c cerebrasContract, opts bodyOptions) {
	t.Helper()
	assertPtrFloat(t, leg, "temperature", c.Temperature, opts.temperature)
	assertPtrFloat(t, leg, "top_p", c.TopP, opts.topP)
	if opts.maxTokens >= 0 {
		if c.MaxTokens == nil || *c.MaxTokens != opts.maxTokens {
			t.Errorf("%s: max_tokens = %v, want %d", leg, c.MaxTokens, opts.maxTokens)
		}
	} else if c.MaxTokens != nil {
		t.Errorf("%s: Cerebras must NOT synthesize a default max_tokens, got %d", leg, *c.MaxTokens)
	}
	if !equalStrings(c.Stop, opts.stopSequences) {
		t.Errorf("%s: stop count = %d, want %d", leg, len(c.Stop), len(opts.stopSequences))
	}
}

// assertNativeCerebrasHeaderOrder pins native's OWN emitted header order —
// Content-Type, Authorization, then any custom headers — never compared to BAML.
func assertNativeCerebrasHeaderOrder(t *testing.T, prep *nanollm.PreparedRequest, r cerebrasNativeRow) {
	t.Helper()
	wantPrefix := []string{"Content-Type", "Authorization"}
	if len(prep.Headers) != len(wantPrefix)+len(r.customHeaders) {
		t.Fatalf("native emitted %d headers, want %d", len(prep.Headers), len(wantPrefix)+len(r.customHeaders))
	}
	for i, want := range wantPrefix {
		if prep.Headers[i][0] != want {
			t.Errorf("native header[%d] name = %q, want %q", i, prep.Headers[i][0], want)
		}
	}
	for name, val := range r.customHeaders {
		found := false
		for _, p := range prep.Headers[len(wantPrefix):] {
			if p[0] == name {
				found = true
				if p[1] != val {
					t.Errorf("native custom header %q value mismatch (len got=%d want=%d)", name, len(p[1]), len(val))
				}
			}
		}
		if !found {
			t.Errorf("native missing custom header %q", name)
		}
	}
}

// assertSurrogateContentDivergence pins the documented openai-generic divergence:
// the BAML surrogate emits STRING content, native emits a content-part ARRAY, the
// two representations DIFFER, yet carry the SAME semantic text. It is the checked-in
// divergence record for the surrogate — never a "semantic-equal = pass" escape hatch.
func assertSurrogateContentDivergence(t *testing.T, bamlC, nanoC cerebrasContract, msgs []corpusMessage) {
	t.Helper()
	if len(bamlC.Messages) != len(msgs) || len(nanoC.Messages) != len(msgs) {
		t.Fatalf("surrogate/native message counts (baml=%d nanollm=%d) != want %d", len(bamlC.Messages), len(nanoC.Messages), len(msgs))
	}
	for i := range msgs {
		bKind := cerebrasContentKind(bamlC.Messages[i].Content)
		nKind := cerebrasContentKind(nanoC.Messages[i].Content)
		if bKind != "string" {
			t.Errorf("openai-generic surrogate message[%d] content kind = %s, want string (concatenated text)", i, bKind)
		}
		if nKind != "array" {
			t.Errorf("native message[%d] content kind = %s, want content-part array", i, nKind)
		}
		if bKind == nKind {
			t.Errorf("expected the documented openai-generic string-vs-array content divergence at message[%d], both were %s", i, bKind)
		}
		bs, bok := cerebrasContentString(bamlC.Messages[i].Content)
		parts, pok := cerebrasContentParts(nanoC.Messages[i].Content)
		if !bok || !pok || len(parts) != 1 {
			t.Fatalf("surrogate/native content shapes not decodable as string/one-block-array at message[%d]", i)
		}
		if bs != parts[0].Text || bs != msgs[i].text {
			t.Errorf("message[%d] semantic text mismatch (baml_len=%d native_len=%d want_len=%d)", i, len(bs), len(parts[0].Text), len(msgs[i].text))
		}
	}
}

// ============================================================================
// (4) Mutation controls — each proves a positive assertion is load-bearing.
// ============================================================================

// strictDecodeCerebrasErr routes through the shared strict decode and returns its
// fixed error (nil on success) WITHOUT failing the test.
func strictDecodeCerebrasErr(body []byte) error {
	_, err := strictDecodeCerebras(body)
	return err
}

// cerebrasControlRow is a small user-only row reused by the controls.
func cerebrasControlRow() cerebrasNativeRow {
	return cerebrasNativeRow{
		name:      "control",
		apiRoot:   fenceCerebrasAPIRoot,
		messages:  []corpusMessage{{role: "user", text: "Tell me about cats."}},
		opts:      bodyOptions{maxTokens: -1},
		nanoOrder: []string{"model", "messages"},
	}
}

func cerebrasControlPrep(t *testing.T) (*nanollm.PreparedRequest, testutil.Snapshot) {
	t.Helper()
	r := cerebrasControlRow()
	client := newCerebrasClient(t, r.apiRoot, r.customHeaders)
	return prepareNative(t, client, buildCanonicalRequest(t, r.messages, r.opts))
}

// TestCerebrasControlContentConvergence: if native ever CONVERGED to the surrogate's
// string content, the content-part-array requirement must catch it. Mutating the
// real native array into a bare string flips the classifier to "string", which the
// surrogate-divergence + native-correctness gates both reject.
func TestCerebrasControlContentConvergence(t *testing.T) {
	prep, _ := cerebrasControlPrep(t)

	real := decodeCerebras(t, "nanollm", prep.Body)
	if cerebrasContentKind(real.Messages[0].Content) != "array" {
		t.Fatal("precondition: native content is not a content-part array")
	}
	converged := bytes.Replace(prep.Body,
		[]byte(`"content":[{"type":"text","text":"Tell me about cats."}]`),
		[]byte(`"content":"Tell me about cats."`), 1)
	if bytes.Equal(converged, prep.Body) {
		t.Fatal("the content-convergence mutation did not change the body")
	}
	mutated := decodeCerebras(t, "nanollm", converged)
	if cerebrasContentKind(mutated.Messages[0].Content) != "string" {
		t.Fatal("a native body that converged to string content was NOT detected (the content-part-array requirement is not load-bearing)")
	}
	// The array decoder must now refuse it — proving the native-correctness gate fails.
	if _, ok := cerebrasContentParts(mutated.Messages[0].Content); ok {
		t.Fatal("string content wrongly decoded as a content-part array")
	}
}

// TestCerebrasControlWrongPath: a mutated endpoint path is caught by the shared
// comparator (against the surrogate oracle).
func TestCerebrasControlWrongPath(t *testing.T) {
	_, bamlCap, _, nanoSnap := cerebrasSurrogatePair(t)

	bad := headerOnly(nanoSnap)
	bad.URL = strings.Replace(bad.URL, cerebrasChatPath, "/v1/complete", 1)
	if bad.URL == nanoSnap.URL {
		t.Fatal("path mutation did not change the URL")
	}
	if diffs := testutil.Diff(headerOnly(bamlCap.snap), bad); len(diffs) == 0 {
		t.Fatal("a wrong endpoint path was NOT detected by the differential")
	}
}

// TestCerebrasControlMissingAuth: dropping the Bearer authorization is caught.
func TestCerebrasControlMissingAuth(t *testing.T) {
	_, bamlCap, _, nanoSnap := cerebrasSurrogatePair(t)

	bad := testutil.Snapshot{Method: nanoSnap.Method, URL: nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range nanoSnap.Headers {
		if k == "authorization" {
			continue
		}
		bad.Headers[k] = v
	}
	if diffs := testutil.Diff(headerOnly(bamlCap.snap), bad); len(diffs) == 0 {
		t.Fatal("a missing Bearer authorization was NOT detected by the differential")
	}
}

// TestCerebrasControlDuplicateAuth: a duplicate authorization is REJECTED at
// snapshot construction (fail closed), never merged.
func TestCerebrasControlDuplicateAuth(t *testing.T) {
	prep, _ := cerebrasControlPrep(t)
	dup := append(append([][2]string(nil), prep.Headers...), [2]string{"Authorization", "Bearer second-fake-key"})
	if _, err := testutil.NewSnapshot(prep.Method, prep.URL, dup, prep.Body); err == nil {
		t.Fatal("a duplicate authorization was NOT rejected by the comparator")
	}
}

// TestCerebrasControlAuthValueRedacted: a tampered Bearer value is detected AND
// never leaks into the diagnostics (authorization is not on the safe-print list).
func TestCerebrasControlAuthValueRedacted(t *testing.T) {
	_, bamlCap, _, nanoSnap := cerebrasSurrogatePair(t)

	bad := testutil.Snapshot{Method: nanoSnap.Method, URL: nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range nanoSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["authorization"] = "Bearer tampered-fake-key"
	diffs := testutil.Diff(headerOnly(bamlCap.snap), bad)
	if len(diffs) == 0 {
		t.Fatal("a tampered Bearer authorization value was NOT detected")
	}
	joined := strings.Join(diffs, "\n")
	tamperedLeaked := strings.Contains(joined, "tampered-fake-key")
	fenceLeaked := strings.Contains(joined, fenceCerebrasAPIKey)
	if tamperedLeaked || fenceLeaked {
		// Report only WHICH secret leaked (booleans) — never re-print the diagnostic.
		t.Fatalf("Bearer value leaked into diagnostics (tampered_present=%v fence_present=%v)", tamperedLeaked, fenceLeaked)
	}
}

// TestCerebrasControlWrongTarget: a client whose configured target differs from the
// fence model produces a body model the required-meaning anchor rejects (nanollm
// rewrites the top-level model to the configured target).
func TestCerebrasControlWrongTarget(t *testing.T) {
	const wrongTarget = "wrong-cerebras-target"
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       fenceAlias,
			Model:      "cerebras/" + wrongTarget,
			APIKey:     fenceCerebrasAPIKey,
			BaseURL:    fenceCerebrasAPIRoot,
			MaxRetries: 0,
		}},
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatal("nanollm.New(cerebras) failed at stage=engine_new")
	}
	defer c.Close()

	prep, _ := prepareNative(t, c, buildCanonicalRequest(t, []corpusMessage{{role: "user", text: "Tell me about cats."}}, bodyOptions{maxTokens: -1}))
	got := decodeCerebras(t, "nanollm", prep.Body)
	if got.Model == fenceCerebrasModel {
		t.Fatalf("a wrong configured target did NOT change the body model (still %q) — the model anchor is not sensitive", fenceCerebrasModel)
	}
	if got.Model != wrongTarget {
		t.Fatalf("expected native to rewrite the body model to the configured target %q, got a different value", wrongTarget)
	}
}

// TestCerebrasControlRenamedBodyField: a renamed top-level field (model -> modell)
// is rejected by the strict DTO decode (an unknown field fails closed).
func TestCerebrasControlRenamedBodyField(t *testing.T) {
	prep, _ := cerebrasControlPrep(t)
	if err := strictDecodeCerebrasErr(prep.Body); err != nil {
		t.Fatal("the unmutated native body failed strict decode (stage=precondition_decode)")
	}
	mutated := bytes.Replace(prep.Body, []byte(`"model"`), []byte(`"modell"`), 1)
	if bytes.Equal(mutated, prep.Body) {
		t.Fatal("the field-rename mutation did not change the body")
	}
	if err := strictDecodeCerebrasErr(mutated); err == nil {
		t.Fatal("a renamed/unlisted top-level field was NOT rejected by the strict decode")
	}
}

// TestCerebrasControlTrailingJSONRejected: a valid request followed by a second
// top-level JSON value is rejected; trailing whitespace is tolerated.
func TestCerebrasControlTrailingJSONRejected(t *testing.T) {
	prep, _ := cerebrasControlPrep(t)
	if err := strictDecodeCerebrasErr(prep.Body); err != nil {
		t.Fatal("the unmutated native body must decode cleanly (stage=precondition_decode)")
	}
	trailingObj := append(append([]byte(nil), prep.Body...), []byte(`{"x":1}`)...)
	if err := strictDecodeCerebrasErr(trailingObj); err == nil {
		t.Fatal("a body with a trailing JSON object after the request was NOT rejected")
	}
	whitespace := append(append([]byte(nil), prep.Body...), []byte("  \n\t")...)
	if err := strictDecodeCerebrasErr(whitespace); err != nil {
		t.Fatal("trailing whitespace must be tolerated by the strict decode (stage=whitespace)")
	}
}

// TestCerebrasControlBamlOriginalUrlOneSided: if native ever emitted
// baml-original-url the divergence gate must catch it (the exemption is one-sided).
func TestCerebrasControlBamlOriginalUrlOneSided(t *testing.T) {
	_, bamlCap, _, nanoSnap := cerebrasSurrogatePair(t)

	if _, ok := nanoSnap.Headers["baml-original-url"]; ok {
		t.Fatal("precondition: native unexpectedly already carries baml-original-url")
	}
	bad := testutil.Snapshot{Method: nanoSnap.Method, URL: nanoSnap.URL, Headers: map[string]string{}}
	for k, v := range nanoSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["baml-original-url"] = fenceCerebrasAPIRoot
	if diffs := testutil.Diff(headerOnly(bamlCap.snap), bad); len(diffs) == 0 {
		t.Fatal("native emitting baml-original-url was NOT detected (the exemption must be one-sided)")
	}
}
