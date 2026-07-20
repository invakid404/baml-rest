//go:build integration && nanollm_integration

// Package provideroracle is the de-BAML TPBP-A gated, TEST-ONLY, no-send
// Anthropic PROVIDER-CORRECTNESS oracle plus the shared trusted-provider oracle
// harness (scope §6.1/§6.2, tpbp-scope.md).
//
// # The bar (owner-ruled): provider-correctness, NOT BAML byte-equality
//
// BAML v0.223.0 and native nanollm v0.4.3 both emit a VALID Anthropic Messages
// request for the same admitted call, but they are DELIBERATELY not byte-equal:
// the top-level JSON member ORDER differs, and BAML additionally carries its own
// internal `baml-original-url` transport header. This suite therefore does NOT
// chase byte-equality. It proves that native emits a PROVIDER-CORRECT Anthropic
// request — correct method / endpoint / auth / version / content-type and every
// mapped semantic value present — and records every BAML divergence as a
// CHECKED-IN, secret-free expected-divergence allowlist entry with FAILING
// controls (controls_test.go). There is no "semantic-equal = pass" escape hatch:
// each side's exact top-level key order is pinned independently, so an UNLISTED
// reorder or an extra/missing key FAILS.
//
// # Shape
//
// For each corpus row the harness:
//
//   - builds a tiny in-memory BAML project with the stock public Go API
//     (baml.CreateRuntime + runtime.BuildRequest) with fixed FAKE creds / model /
//     base / prompt, and captures the REAL stock v0.223.0 Anthropic request
//     (method, URL, header map, exact Body.Text()) with NO socket and NO
//     checked-in generated source (baml_stock_test.go);
//   - builds the SAME S2 canonical OpenAI-shaped request (public
//     nanollm.ChatRequest builder, ordered content-part blocks matching S2's
//     canonicalTextBlock) and calls nanollm Prepare with NO send;
//   - compares method / URL / the required semantic header SET exactly through
//     the shared, redacted nativeserve/testutil comparator (which already exempts
//     BAML's `baml-original-url` on the oracle side only);
//   - decodes both bodies into an Anthropic contract DTO and compares the REQUIRED
//     MEANING (model, max_tokens, system text, ordered message roles/text with NO
//     system/developer role, temperature/top_p/top_k/stop_sequences);
//   - asserts the expected top-level key-order divergence + the one-sided
//     `baml-original-url` header via the checked-in allowlist (anthropic_test.go).
//
// # Redaction
//
// Every diagnostic is secret-free: header values redact through
// testutil.RedactValue (only content-type / accept / anthropic-version print);
// bodies, URLs, and prompt/credential-derived text NEVER print raw — mismatches
// report a field path plus a length or a fixed structural token, never the value
// and never a content-derived hash.
//
// # Isolation
//
// Every file is `_test.go` (excluded from the native-worker tar) behind
// `integration && nanollm_integration`. It runs in the go.work-excluded
// nanollmprepare module, links stock BAML v0.223.0 + the public nanollm-ffi
// prebuilt FFI archive, and touches NO production package. It shares the pure,
// runtime-neutral nativeserve/testutil comparator with the OpenAI Phase-5.1 legs
// but keeps every Anthropic-specific normalization here in `_test.go`.
package provideroracle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// --- The literal fake fence (secret-free; never dialed, never a real key). ---
//
// The `.invalid` base is non-routable and this suite never sends, so the fake
// key and unreachable host are never contacted. The alias is deliberately
// distinct from the target so a plan that leaks the alias into the body fails.
const (
	fenceModel        = "claude-fixture-model"
	fenceAPIKey       = "fake-anthropic-oracle-key"
	fenceAlias        = "tpbpa-anthropic-alias"
	fenceServiceRoot  = "https://anthropic-oracle.invalid" // BAML service root (no /v1)
	fenceCustomHdrKey = "x-custom-oracle"
	fenceCustomHdrVal = "safe-custom-header-value"

	// anthropicVersion is the pinned default Anthropic API version both builders
	// emit unless overridden (a fixed, non-secret token — safe to print).
	anthropicVersion = "2023-06-01"

	// defaultServiceRoot / defaultMessagesURL are the provider defaults BAML and
	// nanollm both resolve when no base_url is configured.
	defaultServiceRoot  = "https://api.anthropic.com"
	defaultMessagesPath = "/v1/messages"
)

// messagesURL returns the exact Anthropic Messages endpoint for a BAML service
// root (or the provider default when serviceRoot is empty). BAML appends
// /v1/messages to its service root; S2 converts the service root to nanollm's API
// root by appending /v1, and nanollm appends /messages — the same effective URL.
func messagesURL(serviceRoot string) string {
	if serviceRoot == "" {
		serviceRoot = defaultServiceRoot
	}
	return serviceRoot + defaultMessagesPath
}

// canonTextBlock mirrors S2 admission's canonicalTextBlock: an ordered
// {type,text} content part. Using a struct (not a map) reproduces S2's exact
// content-part member order, so the ONLY body divergence from BAML is the
// documented TOP-LEVEL member order — nested block order matches byte-for-byte.
type canonTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textBlocks(text string) []canonTextBlock {
	return []canonTextBlock{{Type: "text", Text: text}}
}

// corpusMessage is one rendered chat turn used to build BOTH legs' request: BAML
// renders it from the prompt template, native builds the same canonical OpenAI
// content-part message. Exactly one text block per turn (the corpus shape).
type corpusMessage struct {
	role string
	text string
}

// bodyOptions are the mapped sampling/limit options a row exercises. Pointers
// distinguish "unset" from a valid zero. maxTokens uses -1 as the "unset"
// sentinel (nil default is emitted by both builders as 4096).
type bodyOptions struct {
	maxTokens     int // -1 => unset (both default to 4096)
	temperature   *float64
	topP          *float64
	topK          *int
	stopSequences []string
}

// --- nanollm (native) leg: build the S2-equivalent canonical request. ---

// newAnthropicClient builds a native nanollm engine for one Anthropic alias at
// the fake fence values, with a zero retry budget, no fallbacks, no process env,
// and optional custom headers (injected verbatim via Params["headers"], exactly
// as S2's newMappedClient does). serviceRoot is the BAML service root; the
// nanollm API base appends /v1 to it (S2's anthropicNanollmBase), or is left
// unset for the provider default.
func newAnthropicClient(t *testing.T, serviceRoot string, customHeaders map[string]string) *nanollm.Client {
	t.Helper()
	mc := nanollm.ModelConfig{
		Name:       fenceAlias,
		Model:      "anthropic/" + fenceModel,
		APIKey:     fenceAPIKey,
		MaxRetries: 0,
	}
	if serviceRoot != "" {
		mc.BaseURL = serviceRoot + "/v1" // S2 anthropicNanollmBase: service root + /v1
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
		t.Fatal("nanollm.New(anthropic) failed at stage=engine_new")
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// buildCanonicalRequest builds the S2 canonical OpenAI-shaped request bytes for
// the given messages + options via the public nanollm.ChatRequest builder — the
// same construction S2 admission performs (typed common fields, top_k through
// Extra). It sets Request.Model to the alias so Prepare rewrites the body model
// to the configured target, exactly like S2.
func buildCanonicalRequest(t *testing.T, msgs []corpusMessage, opts bodyOptions) nanollm.Request {
	t.Helper()
	cr := nanollm.ChatRequest{Model: fenceAlias}
	for _, m := range msgs {
		cr.Messages = append(cr.Messages, nanollm.ChatMessage{Role: m.role, Content: textBlocks(m.text)})
	}
	if opts.maxTokens >= 0 {
		mt := opts.maxTokens
		cr.MaxTokens = &mt
	}
	cr.Temperature = opts.temperature
	cr.TopP = opts.topP
	if len(opts.stopSequences) > 0 {
		cr.Stop = opts.stopSequences
	}
	if opts.topK != nil {
		// top_k is an Anthropic provider extension: S2 carries it through
		// ChatRequest.Extra (unmodeled top-level field), never a typed field.
		cr.Extra = map[string]any{"top_k": *opts.topK}
	}
	req, err := cr.Build(json.Marshal)
	if err != nil {
		// A marshal error can echo the request body — report the stage only.
		t.Fatal("canonical ChatRequest.Build failed at stage=marshal")
	}
	req.Model = fenceAlias
	return req
}

// prepareNative runs nanollm Prepare (NO send) and returns the plan plus a
// runtime-neutral snapshot (rejecting a duplicate/multi-value header).
func prepareNative(t *testing.T, c *nanollm.Client, req nanollm.Request) (*nanollm.PreparedRequest, testutil.Snapshot) {
	t.Helper()
	prep, err := c.Prepare(req)
	if err != nil {
		// A Prepare error can echo the request body — report the stage only.
		t.Fatal("nanollm Prepare rejected a canonical Anthropic body (stage=prepare)")
	}
	snap, err := testutil.NewSnapshot(prep.Method, prep.URL, prep.Headers, prep.Body)
	if err != nil {
		t.Fatal("nanollm plan has a duplicate/multi-value header (stage=snapshot)")
	}
	return prep, snap
}

// --- Anthropic contract DTO (required-meaning comparison). ---

// anthropicContract is the subset of the Anthropic Messages request whose MEANING
// both builders must agree on. Decoding discards member order (the intended
// divergence) and lets a semantic comparison run over provider-native fields.
type anthropicContract struct {
	Model         string             `json:"model"`
	MaxTokens     *int               `json:"max_tokens"`
	System        []anthropicBlock   `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// Fixed, secret-free strict-decode outcomes (never echo body bytes/field values).
var (
	errStrictDecode = errors.New("provideroracle: body is not a recognized single Anthropic Messages request")
	errTrailingJSON = errors.New("provideroracle: body carries trailing data after the JSON value")
)

// strictDecodeAnthropic is the SINGLE strict fail-closed decode path shared by
// every provideroracle site that parses a captured/prepared request body. It:
//
//   - rejects unknown top-level/nested fields (DisallowUnknownFields);
//   - requires EXACTLY ONE top-level JSON value — a second value appended after
//     the request (trailing JSON) is rejected, while trailing WHITESPACE is
//     tolerated (a bare json.Decoder.Decode would silently accept both).
//
// It returns only fixed, secret-free sentinels — never the raw decoder error,
// which could echo body bytes.
func strictDecodeAnthropic(body []byte) (anthropicContract, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var c anthropicContract
	if err := dec.Decode(&c); err != nil {
		return anthropicContract{}, errStrictDecode
	}
	// Exactly one value: a second decode must reach clean EOF (only whitespace may
	// remain). A successful second value, or malformed trailing bytes, fails closed.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return anthropicContract{}, errTrailingJSON
	}
	return c, nil
}

// decodeAnthropic strictly decodes a captured/prepared body into the contract DTO
// via the shared exact-one-value path. It never prints the raw body on error —
// only a fixed stage token + length.
func decodeAnthropic(t *testing.T, leg string, body []byte) anthropicContract {
	t.Helper()
	c, err := strictDecodeAnthropic(body)
	if err != nil {
		stage := "decode"
		if errors.Is(err, errTrailingJSON) {
			stage = "trailing_json"
		}
		t.Fatalf("%s body is not a recognized single Anthropic Messages request (stage=%s, len=%d)", leg, stage, len(body))
	}
	return c
}

// blocksEqual compares two content/system block sequences for EXACT equality —
// count, per-block type, and per-block text, in order. This is the primitive the
// system-block assertions and their mutation control share, so a block with the
// same text but a wrong/absent `type`, or a reordered/extra block, is caught
// (never reduced to concatenated text).
func blocksEqual(a, b []anthropicBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

// textBlockSeq builds the expected provider-native block sequence for the given
// texts: one {type:"text", text} block each, in order.
func textBlockSeq(texts ...string) []anthropicBlock {
	out := make([]anthropicBlock, 0, len(texts))
	for _, tx := range texts {
		out = append(out, anthropicBlock{Type: "text", Text: tx})
	}
	return out
}

// --- Top-level member-order extraction (the divergence primitive). ---

// topLevelKeys returns the top-level object member names of a JSON body in their
// EXACT serialized order, without decoding into an (unordered) map. It is the
// primitive the expected-divergence allowlist pins on each side. It relies on
// json.Decoder's object-state tracking: after the opening '{', dec.Token()
// returns each member KEY, and skipJSONValue consumes that member's value
// (scalar or nested container) so the next Token() is the following key.
func topLevelKeys(t *testing.T, leg string, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		// The tokenizer error can echo body bytes — report stage + length only.
		t.Fatalf("%s body: stage=open_token (len=%d)", leg, len(body))
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("%s body is not a JSON object (len=%d)", leg, len(body))
	}
	var keys []string
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			t.Fatalf("%s body: stage=member_key (len=%d)", leg, len(body))
		}
		key, ok := ktok.(string)
		if !ok {
			t.Fatalf("%s body: stage=non_string_key (len=%d)", leg, len(body))
		}
		keys = append(keys, key)
		skipJSONValue(t, leg, dec)
	}
	return keys
}

// skipJSONValue consumes exactly one JSON value from dec: a scalar is one token;
// an object/array is walked to its matching close. Errors report only a fixed
// stage token — a raw tokenizer error could echo body bytes.
func skipJSONValue(t *testing.T, leg string, dec *json.Decoder) {
	t.Helper()
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("%s body: stage=value_token", leg)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return // scalar value already consumed
	}
	if d != '{' && d != '[' {
		t.Fatalf("%s body: stage=unexpected_delim", leg)
	}
	depth := 1
	for depth > 0 {
		tk, err := dec.Token()
		if err != nil {
			t.Fatalf("%s body: stage=nested_value", leg)
		}
		if dd, ok := tk.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
}

func sameKeySet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, k := range a {
		seen[k]++
	}
	for _, k := range b {
		seen[k]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func sameOrder(a, b []string) bool { return reflect.DeepEqual(a, b) }
