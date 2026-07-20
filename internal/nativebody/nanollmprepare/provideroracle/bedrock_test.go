//go:build integration && nanollm_integration

package provideroracle

// TPBP-B AWS Bedrock provider-correctness oracle + INDEPENDENT SigV4 verification
// (scope §5/§6.2/§7/§9-B, tpbp-scope.md). TEST-ONLY, no-send, tar-free.
//
// # The bar (owner-ruled): provider-CORRECTNESS + cryptographically-VALID SigV4
//
// BAML v0.223.0's CFFI returns an UNSIGNED Converse HTTPRequest and signs it only
// at SEND-time; native nanollm v0.4.3 `Prepare` returns an ALREADY-SigV4-signed
// plan at PREPARE-time with its own `Meta.SignedAt`. The two full signed requests
// are therefore NOT the same artifact (different phase / clock). This suite does
// NOT chase full-request byte-equality. It proves, per corpus row:
//
//   - PRE-SIGN CORE parity: exact method (POST), exact normalized Converse URL
//     (`/model/<percent-encoded-model-id>/converse` on the fixed
//     bedrock-runtime.<region>.amazonaws.com host, `%3A` for a `:` in the model
//     id), and the base semantic header SET (accept / content-type);
//   - BODY correctness: native's body is a valid Converse request (no `model`
//     field; text→{text} blocks; system lifted; the 4 inference fields mapped to
//     inferenceConfig; additional fields under additionalModelRequestFields) and
//     carries every mapped semantic value — compared cross-leg for MEANING (BAML
//     serializes temperature/top_p as widened f32 and orders additional fields by
//     declaration while native emits f64 and Go-map order, so the exact BYTES
//     diverge on those rows: a documented, checked-in divergence, never forced
//     equal);
//   - SigV4 VALIDITY (the security bar): native's `Authorization` is verified
//     CRYPTOGRAPHICALLY against the fixed FAKE secret key by a from-scratch
//     crypto/hmac reconstruction of the canonical request from native's OWN
//     signed-header list (independent of both nanollm and any AWS SDK). The payload
//     hash (SHA-256 of the exact body bytes), the signed-header set, the credential
//     scope (`<date>/<region>/bedrock/aws4_request`), the X-Amz-Date, the
//     session-token handling, and the plan expiry window are all pinned;
//   - LEGACY-SIGNER cross-check (§7 step 5-6): BAML's captured UNSIGNED request is
//     driven through baml-rest's ACTUAL legacy signing path — the real
//     `llmhttp.Client.Execute` → `signRequest` (bamlutils/llmhttp/awssigv4.go), NOT
//     a re-implementation — with the signing clock pinned to native's
//     `Meta.SignedAt` via `AWSAuthConfig.NowFunc` and a capturing transport (no
//     network, no real AWS). Body-INDEPENDENT signed fields (X-Amz-Date, credential
//     scope) match native exactly, proving the phase/clock control; the legacy
//     signer's `X-Amz-Content-Sha256` header is preserved and asserted to equal the
//     EXACT SHA-256(body) (a mutation control proves it is signed); and the
//     legacy-vs-native SIGNED-HEADER-SET divergence is recorded honestly (see below)
//     rather than papered over into a forced Authorization equality.
//
// # Recorded divergence: nanollm signs a minimal set, the legacy path a superset
//
// nanollm's Bedrock signer signs `{accept, content-type, host, x-amz-date}`
// (+`x-amz-security-token` with temporary creds). The production baml-rest legacy
// signer signs a strict SUPERSET, additionally covering `content-length` and
// `x-amz-content-sha256`. Both are VALID SigV4 requests: the payload hash is carried
// in the canonical request either way, and `x-amz-content-sha256` as a signed header
// is required only for Amazon S3, not bedrock-runtime — so native is not
// under-signed. This is a benign divergence in WHICH headers each signer chooses to
// sign, recorded as a checked-in fact on BOTH sides (native's minimal set and the
// legacy superset are each pinned), and it means the two Authorizations can never be
// byte-equal — so the suite ties the legs through the payload hash instead of
// forcing signature equality.
//
// The mutation controls (bottom of this file) prove every positive SigV4 assertion
// is load-bearing: a body / host / path / date / session-token / content-hash
// mutation MUST invalidate the corresponding signature.
//
// # Deterministic SigV4 (§7)
//
// There is no nonce; the only nondeterministic signing input is the TIMESTAMP. The
// suite uses fixed FAKE credentials and native's returned `Meta.SignedAt` as the
// reference clock the legacy signer's `NowFunc` returns — no nanollm clock hook is
// needed. It NEVER compares an independently-generated Authorization without
// controlling time, NEVER drops X-Amz-Content-Sha256/session-token from correctness
// to force equality, and NEVER touches a real AWS credential chain or the network.
//
// # Redaction
//
// Every diagnostic is secret-free: the fake secret key, the derived signing key,
// the signature hex, session tokens, bodies, and URLs never print raw. A signature
// mismatch reports a fixed structural token (and, at most, a length); credential
// scope / signed-header NAMES / X-Amz-Date are non-secret and may print.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/testutil"
)

// --- The literal fake fence (secret-free; never dialed, never a real AWS key). ---
//
// The credentials are the AWS-published SigV4 documentation example values with an
// oracle-specific alias, so they are unmistakably fake and non-secret; this suite
// never sends, so the fake creds/host are never contacted. bedrockFakeSecret is
// still redacted from every diagnostic (a secret-shaped value must never print,
// example or not).
const (
	bedrockFenceRegion = "us-east-1"
	bedrockService     = "bedrock"

	// bedrockColonModel needs path percent-encoding (its `:` → `%3A`);
	// bedrockPlainModel is the unencoded control.
	bedrockColonModel = "anthropic.claude-3-haiku-20240307-v1:0"
	bedrockPlainModel = "amazon.titan-text-lite-v1"

	bedrockFakeAKID      = "AKIDORACLEFAKEEXAMPLE"
	bedrockFakeSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	bedrockFakeSessionTk = "FQoGZXIvYXdzEXAMPLEORACLESESSIONTOKEN=="

	// SigV4 fixed tokens (all non-secret).
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Terminator = "aws4_request"
	sigV4TimeFormat = "20060102T150405Z" // amz-date (UTC, second precision)
	sigV4DateFormat = "20060102"         // credential-scope date

	bedrockHostPrefix = "bedrock-runtime."
	bedrockHostSuffix = ".amazonaws.com"
)

// --- Converse contract DTO (required-meaning comparison, §6.2). ---

// converseContract is the subset of the Bedrock Converse request whose MEANING
// both builders must agree on. It has NO `model` member (Converse carries the
// model in the URL, not the body) — DisallowUnknownFields therefore fail-closes on
// a stray top-level `model` (a native mapping bug) exactly as it does on any
// renamed field. additionalModelRequestFields is an opaque JSON object compared by
// value (order-insensitive), so a validly reordered nested map is not a meaning
// divergence.
type converseContract struct {
	Messages                     []converseMessage  `json:"messages"`
	System                       []converseTextPart `json:"system"`
	InferenceConfig              *converseInference `json:"inferenceConfig"`
	AdditionalModelRequestFields map[string]any     `json:"additionalModelRequestFields"`
}

type converseMessage struct {
	Role    string             `json:"role"`
	Content []converseTextPart `json:"content"`
}

// converseTextPart is a Converse content/system block. Bedrock text blocks are
// `{"text": ...}` (NO `type` discriminator, unlike Anthropic) on BOTH builders.
type converseTextPart struct {
	Text string `json:"text"`
}

type converseInference struct {
	MaxTokens     *int     `json:"maxTokens"`
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"topP"`
	StopSequences []string `json:"stopSequences"`
}

// Fixed, secret-free strict-decode outcomes (never echo body bytes/field values).
var (
	errConverseDecode   = errors.New("provideroracle: body is not a recognized single Bedrock Converse request")
	errConverseTrailing = errors.New("provideroracle: converse body carries trailing data after the JSON value")
)

// strictDecodeConverse is the SINGLE strict fail-closed decode path shared by every
// bedrock site that parses a captured/prepared body: it rejects unknown
// top-level/nested struct fields (DisallowUnknownFields — including a forbidden
// top-level `model`) and requires EXACTLY ONE top-level JSON value (trailing JSON
// after the request is rejected; trailing whitespace tolerated). It returns only
// fixed secret-free sentinels, never the raw decoder error.
func strictDecodeConverse(body []byte) (converseContract, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var c converseContract
	if err := dec.Decode(&c); err != nil {
		return converseContract{}, errConverseDecode
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return converseContract{}, errConverseTrailing
	}
	return c, nil
}

// decodeConverse strictly decodes a captured/prepared body into the Converse DTO.
// It never prints the raw body on error — only a fixed stage token + length.
func decodeConverse(t *testing.T, leg string, body []byte) converseContract {
	t.Helper()
	c, err := strictDecodeConverse(body)
	if err != nil {
		stage := "decode"
		if errors.Is(err, errConverseTrailing) {
			stage = "trailing_json"
		}
		t.Fatalf("%s body is not a recognized single Bedrock Converse request (stage=%s, len=%d)", leg, stage, len(body))
	}
	return c
}

// --- Corpus (§9-B). ---

// bedrockRow is one corpus case. The SAME structured inputs drive BOTH legs — BAML
// through a generated in-memory `.baml` source, native through the canonical
// OpenAI builder + the mapped client Params — so the two requests cannot silently
// drift apart.
type bedrockRow struct {
	name     string
	model    string          // target model id (bedrockColonModel needs %3A)
	messages []corpusMessage // ordered role turns (one leading system max)
	opts     bodyOptions     // inference_configuration → inferenceConfig
	session  bool            // temporary credentials WITH an STS session token

	// additional is the additional_model_request_fields object (nil ⇒ none). It is
	// rendered into BAML `.baml` syntax and passed to native via client Params, so
	// both legs carry the SAME semantic object. additionalBAMLKeyOrder, when set,
	// pins the top-level member order BAML emits — deliberately NON-sorted so it
	// diverges from native's Go-map (sorted) order and manufactures a valid,
	// meaning-preserving member-ORDER body divergence (§7 step 7).
	additional             map[string]any
	additionalBAMLKeyOrder []string

	// wantModelEncoded pins whether the URL must percent-encode the model id
	// (`%3A`). wantBodyByteEqual is the CHECKED-IN pre-sign body divergence ledger:
	// true ⇒ the exact BAML and native body bytes match (so the legacy signer's
	// X-Amz-Content-Sha256 equals the hash native's own signature is bound to);
	// false ⇒ a documented valid divergence (f32 float formatting or additional-field
	// ordering) where the two payload hashes differ and each signature is verified
	// against its OWN payload. divergenceNote documents the reason.
	wantModelEncoded  bool
	wantBodyByteEqual bool
	divergenceNote    string
}

func bedrockCorpus() []bedrockRow {
	sys := corpusMessage{role: "system", text: "You are a concise assistant."}
	return []bedrockRow{
		{
			// %3A model id; system + user; ALL FOUR inference fields; static creds.
			// temperature/top_p make the body diverge (BAML widens f32 → f64).
			name:     "static_colon_model_all_inference",
			model:    bedrockColonModel,
			messages: []corpusMessage{sys, {role: "user", text: "Tell me about cats."}},
			opts: bodyOptions{
				maxTokens:     64,
				temperature:   f64(0.2),
				topP:          f64(0.9),
				stopSequences: []string{"STOP", "END"},
			},
			wantModelEncoded:  true,
			wantBodyByteEqual: false,
			divergenceNote:    "baml serializes temperature/top_p as widened f32; native emits f64",
		},
		{
			// temporary credentials WITH session token; %3A model; maxTokens-only
			// integer body ⇒ byte-equal ⇒ EXACT signed-field parity INCLUDING the
			// signed x-amz-security-token.
			name:              "session_token_colon_model_byte_equal",
			model:             bedrockColonModel,
			messages:          []corpusMessage{sys, {role: "user", text: "Name one fact about the moon."}},
			opts:              bodyOptions{maxTokens: 32},
			session:           true,
			wantModelEncoded:  true,
			wantBodyByteEqual: true,
		},
		{
			// nested / multi-key additional_model_request_fields; system + ordered
			// user/assistant turns; static creds. Additional fields diverge in
			// serialized member order (native = Go-map order, BAML = declaration
			// order) — a documented, order-only body divergence.
			name:  "additional_fields_nested_ordered_turns",
			model: bedrockColonModel,
			messages: []corpusMessage{
				sys,
				{role: "user", text: "Name one fact about the moon."},
				{role: "assistant", text: "It has no atmosphere to speak of."},
			},
			opts: bodyOptions{maxTokens: 48},
			additional: map[string]any{
				"anthropic_version": "bedrock-2023-05-31",
				"budget":            1030,
				"thinking": map[string]any{
					"type":  "enabled",
					"depth": 2,
				},
			},
			// BAML emits the top-level additional members in reverse-alphabetical
			// order; native emits them Go-map (sorted). Same meaning, divergent bytes.
			additionalBAMLKeyOrder: []string{"thinking", "budget", "anthropic_version"},
			wantModelEncoded:       true,
			wantBodyByteEqual:      false,
			divergenceNote:         "additional_model_request_fields member order (native go-map sorted vs baml declaration order)",
		},
		{
			// plain model id (NO %3A control); user-only; maxTokens-only integer body
			// ⇒ byte-equal ⇒ EXACT signed-field parity with the legacy signer.
			name:              "plain_model_user_only_byte_equal",
			model:             bedrockPlainModel,
			messages:          []corpusMessage{{role: "user", text: "Ping."}},
			opts:              bodyOptions{maxTokens: 16},
			wantModelEncoded:  false,
			wantBodyByteEqual: true,
		},
	}
}

// wantMaxTokens / wantSystemTexts / wantProviderMessages mirror the row's expected
// mapped meaning.
func (r bedrockRow) wantMaxTokens() *int {
	if r.opts.maxTokens < 0 {
		return nil
	}
	v := r.opts.maxTokens
	return &v
}

func (r bedrockRow) wantSystemTexts() []string {
	var out []string
	for _, m := range r.messages {
		if m.role == "system" {
			out = append(out, m.text)
		}
	}
	return out
}

func (r bedrockRow) wantProviderMessages() []corpusMessage {
	var out []corpusMessage
	for _, m := range r.messages {
		if m.role == "system" || m.role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// --- Bedrock URL / path encoding (must match BOTH builders). ---

// bedrockConverseURL is the exact Converse endpoint for a model id: the fixed
// bedrock-runtime.<region>.amazonaws.com host and the `/model/<enc>/converse` path,
// where <enc> percent-encodes every non-unreserved byte (BAML's
// PATH_SEGMENT_ENCODE_SET == RFC3986-unreserved; nanollm matches), so `:` → `%3A`.
func bedrockConverseURL(model string) string {
	return "https://" + bedrockHostPrefix + bedrockFenceRegion + bedrockHostSuffix +
		"/model/" + pathEncodeModel(model) + "/converse"
}

func pathEncodeModel(model string) string {
	var b strings.Builder
	for i := 0; i < len(model); i++ {
		c := model[i]
		if isUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// isUnreserved reports the RFC3986 unreserved set (A-Z a-z 0-9 - _ . ~) — the set
// left un-escaped by BAML's PATH_SEGMENT_ENCODE_SET and by AWS canonical-URI
// escaping alike.
func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// --- native (nanollm) leg: build the S2-equivalent signed Bedrock plan. ---

// newBedrockClient builds a native nanollm engine for one Bedrock alias at the fake
// fence values, mirroring S2 admission's newTrustedConfig (mapping.go): model
// `bedrock/<target>`, zero retry budget, no process env, region + resolved
// credential literals + additional_model_request_fields as Params. The literals
// carry no `${...}`, so they pass nanollm's interpolation unchanged (S2 routes them
// through Env placeholders purely to keep secrets out of the config JSON; the
// resulting engine input is identical).
func newBedrockClient(t *testing.T, r bedrockRow) *nanollm.Client {
	t.Helper()
	params := map[string]any{
		"region":            bedrockFenceRegion,
		"access_key_id":     bedrockFakeAKID,
		"secret_access_key": bedrockFakeSecret,
	}
	if r.session {
		params["session_token"] = bedrockFakeSessionTk
	}
	if len(r.additional) > 0 {
		params["additional_model_request_fields"] = r.additional
	}
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       fenceAlias,
			Model:      "bedrock/" + r.model,
			MaxRetries: 0,
			Params:     params,
		}},
		UseProcessEnv: false,
	})
	if err != nil {
		// The engine error can echo the config (fake creds/region) — report the stage only.
		t.Fatal("nanollm.New(bedrock) failed at stage=engine_new")
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// bedrockCapture is one row's paired capture.
type bedrockCapture struct {
	baml     bamlCapture
	prep     *nanollm.PreparedRequest
	nanoSnap testutil.Snapshot
}

func runBedrockRow(t *testing.T, r bedrockRow) bedrockCapture {
	t.Helper()

	// BAML unsigned Converse capture (in-memory stock v0.223.0, no socket, no send).
	src, fn := bamlBedrockSource(r)
	baml := captureBAMLAnthropic(t, src, fn, nil) // generic BuildRequest capture helper

	// Native signed plan (nanollm Prepare, no send).
	c := newBedrockClient(t, r)
	req := buildCanonicalRequest(t, r.messages, r.opts)
	prep, err := c.Prepare(req)
	if err != nil {
		// A Prepare error can echo the request body — report the stage only.
		t.Fatal("nanollm Prepare rejected a canonical Bedrock body (stage=prepare)")
	}
	snap, err := testutil.NewSnapshot(prep.Method, prep.URL, prep.Headers, prep.Body)
	if err != nil {
		t.Fatal("nanollm bedrock plan has a duplicate/multi-value header (stage=snapshot)")
	}
	return bedrockCapture{baml: baml, prep: prep, nanoSnap: snap}
}

// --- BAML in-memory source generation (keeps the two legs coupled). ---

// bamlBedrockSource renders the tiny in-memory `.baml` project for a row: one
// aws-bedrock client at the fence values with the row's mapped options, and one
// zero-arg function whose prompt is the row's ordered role turns.
func bamlBedrockSource(r bedrockRow) (src, fn string) {
	fn = "OracleFn"
	var opt strings.Builder
	fmt.Fprintf(&opt, "    model_id %q\n", r.model)
	fmt.Fprintf(&opt, "    region %q\n", bedrockFenceRegion)
	fmt.Fprintf(&opt, "    access_key_id %q\n", bedrockFakeAKID)
	fmt.Fprintf(&opt, "    secret_access_key %q\n", bedrockFakeSecret)
	if r.session {
		fmt.Fprintf(&opt, "    session_token %q\n", bedrockFakeSessionTk)
	}

	var inf strings.Builder
	if r.opts.maxTokens >= 0 {
		fmt.Fprintf(&inf, "      max_tokens %d\n", r.opts.maxTokens)
	}
	if r.opts.temperature != nil {
		fmt.Fprintf(&inf, "      temperature %s\n", formatBAMLFloat(*r.opts.temperature))
	}
	if r.opts.topP != nil {
		fmt.Fprintf(&inf, "      top_p %s\n", formatBAMLFloat(*r.opts.topP))
	}
	if len(r.opts.stopSequences) > 0 {
		quoted := make([]string, len(r.opts.stopSequences))
		for i, s := range r.opts.stopSequences {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&inf, "      stop_sequences [%s]\n", strings.Join(quoted, ", "))
	}
	if inf.Len() > 0 {
		opt.WriteString("    inference_configuration {\n")
		opt.WriteString(inf.String())
		opt.WriteString("    }\n")
	}

	if len(r.additional) > 0 {
		opt.WriteString("    additional_model_request_fields ")
		if len(r.additionalBAMLKeyOrder) > 0 {
			opt.WriteString(renderBAMLObjectOrdered(r.additional, r.additionalBAMLKeyOrder, "    "))
		} else {
			opt.WriteString(renderBAMLValue(r.additional, "    "))
		}
		opt.WriteString("\n")
	}

	var prompt strings.Builder
	for _, m := range r.messages {
		fmt.Fprintf(&prompt, "    {{ _.role(%q) }}\n    %s\n", m.role, bamlPromptLine(m.text))
	}

	src = fmt.Sprintf(`
client<llm> OracleBedrock {
  provider aws-bedrock
  options {
%s  }
}

function %s() -> string {
  client OracleBedrock
  prompt #"
%s  "#
}
`, opt.String(), fn, prompt.String())
	return src, fn
}

// formatBAMLFloat renders a float for a `.baml` numeric literal without Go's
// shortest-float scientific notation (BAML parses temperature/top_p as f32).
func formatBAMLFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// renderBAMLObjectOrdered renders a top-level object in an EXPLICIT key order
// (nested values still render via renderBAMLValue, i.e. sorted). order must list
// exactly the object's keys; a missing/extra key panics loudly (a test-authoring bug).
func renderBAMLObjectOrdered(m map[string]any, order []string, indent string) string {
	if len(order) != len(m) {
		panic("renderBAMLObjectOrdered: key order does not cover the object")
	}
	var b strings.Builder
	b.WriteString("{\n")
	for _, k := range order {
		v, ok := m[k]
		if !ok {
			panic("renderBAMLObjectOrdered: ordered key not present in the object: " + k)
		}
		fmt.Fprintf(&b, "%s  %s %s\n", indent, k, renderBAMLValue(v, indent+"  "))
	}
	fmt.Fprintf(&b, "%s}", indent)
	return b.String()
}

// renderBAMLValue renders a JSON-compatible Go value as BAML config-value syntax
// (maps as `{ key value ... }`, arrays as `[a, b]`, strings quoted, numbers/bools
// bare). Map keys are emitted in SORTED order so the rendered `.baml` is
// deterministic; member order is a documented body divergence, not a meaning one,
// so the sort does not affect the semantic comparison.
func renderBAMLValue(v any, indent string) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "%s  %s %s\n", indent, k, renderBAMLValue(t[k], indent+"  "))
		}
		fmt.Fprintf(&b, "%s}", indent)
		return b.String()
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = renderBAMLValue(e, indent)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case string:
		return fmt.Sprintf("%q", t)
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case float64:
		return formatBAMLFloat(t)
	case nil:
		return "null"
	default:
		panic(fmt.Sprintf("renderBAMLValue: unsupported additional-field type %T", v))
	}
}

// --- The core differential. ---

func TestBedrockProviderCorrectnessAndSigV4(t *testing.T) {
	for _, r := range bedrockCorpus() {
		r := r
		t.Run(r.name, func(t *testing.T) {
			cap := runBedrockRow(t, r)
			wantURL := bedrockConverseURL(r.model)

			// (1) Method + exact endpoint on BOTH legs (not merely baml==nano).
			for _, leg := range []struct {
				name string
				snap testutil.Snapshot
			}{{"baml", cap.baml.snap}, {"nanollm", cap.nanoSnap}} {
				if leg.snap.Method != "POST" {
					t.Errorf("%s method = %q, want POST", leg.name, leg.snap.Method)
				}
				if leg.snap.URL != wantURL {
					// Never print the raw URL; report length + the fixed expected endpoint.
					t.Errorf("%s converse endpoint mismatch: got_len=%d want=%q", leg.name, len(leg.snap.URL), wantURL)
				}
			}

			// (1b) Model-id path encoding: `%3A` present iff the id needs it, and a
			// raw `:` never survives into the model path segment.
			assertModelEncoding(t, "baml", cap.baml.snap.URL, r)
			assertModelEncoding(t, "nanollm", cap.nanoSnap.URL, r)

			// (2) Base semantic header SET (accept / content-type) via the shared
			// redacted comparator, body EXCLUDED (its bytes are compared/classified in
			// step 4). BAML's request is UNSIGNED and native's is SIGNED, so the
			// SigV4-owned headers live only on native — they are asserted directly in
			// step 5, not cross-diffed here.
			if diffs := testutil.Diff(bedrockBaseOnly(cap.baml.snap), bedrockBaseOnly(cap.nanoSnap)); len(diffs) != 0 {
				t.Errorf("base semantic header differential mismatch:\n%s", strings.Join(diffs, "\n"))
			}

			// (3) Required provider MEANING on each leg, then cross-leg equivalence.
			bamlC := decodeConverse(t, "baml", cap.baml.body)
			nanoC := decodeConverse(t, "nanollm", cap.prep.Body)
			assertRequiredConverse(t, "baml", bamlC, r)
			assertRequiredConverse(t, "nanollm", nanoC, r)
			assertConverseEquivalent(t, bamlC, nanoC)

			// (3b) Top-level member order (native's own, pinned independently) + no
			// forbidden `model` member on either leg.
			assertConverseTopLevel(t, "baml", cap.baml.body)
			assertConverseTopLevel(t, "nanollm", cap.prep.Body)

			// (4) Pre-sign body divergence ledger: byte-equality must match the
			// checked-in expectation.
			bodyByteEqual := bytes.Equal(cap.baml.body, cap.prep.Body)
			if bodyByteEqual != r.wantBodyByteEqual {
				t.Errorf("pre-sign body byte-equality = %v, want %v (baml_len=%d nano_len=%d)",
					bodyByteEqual, r.wantBodyByteEqual, len(cap.baml.body), len(cap.prep.Body))
			}
			if !bodyByteEqual && r.divergenceNote == "" {
				t.Error("body diverged but the row records no divergenceNote (an unlisted divergence must be documented)")
			}

			// (5) The SigV4 security bar.
			assertBedrockSigV4(t, r, cap)
		})
	}
}

// assertModelEncoding pins the model-id percent-encoding in a leg's URL.
func assertModelEncoding(t *testing.T, leg, rawURL string, r bedrockRow) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("%s url did not parse (stage=url_parse)", leg)
	}
	seg := "/model/" + pathEncodeModel(r.model) + "/converse"
	if u.EscapedPath() != seg {
		t.Errorf("%s escaped path mismatch: got_len=%d want=%q", leg, len(u.EscapedPath()), seg)
	}
	if r.wantModelEncoded {
		if !strings.Contains(u.EscapedPath(), "%3A") {
			t.Errorf("%s: model id %q must percent-encode ':' as %%3A", leg, r.model)
		}
		if strings.Contains(u.EscapedPath(), ":") {
			t.Errorf("%s: a raw ':' survived into the model path segment", leg)
		}
	} else if strings.Contains(u.EscapedPath(), "%") {
		t.Errorf("%s: plain model id must not be percent-encoded", leg)
	}
}

// bedrockBaseOnly returns a snapshot carrying only method/URL and the BASE semantic
// headers (accept / content-type) so testutil.Diff compares only the pre-sign base
// transport across the unsigned (BAML) and signed (native) legs. The SigV4-owned
// headers are excluded HERE only because BAML's request is unsigned and native's is
// signed (different lifecycle phases) — they are NOT dropped from correctness:
// native's Authorization / X-Amz-Date / signed-header set and the legacy signer's
// X-Amz-Content-Sha256 are each asserted in assertBedrockSigV4.
func bedrockBaseOnly(s testutil.Snapshot) testutil.Snapshot {
	base := map[string]string{}
	for k, v := range s.Headers {
		if isSigV4OwnedHeader(k) {
			continue
		}
		base[k] = v
	}
	return testutil.Snapshot{Method: s.Method, URL: s.URL, Headers: base, Body: nil}
}

// isSigV4OwnedHeader reports the request headers the SigV4 signing lifecycle owns
// (present only on the signed native leg). Names are compared case-insensitively.
func isSigV4OwnedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-amz-date", "x-amz-content-sha256", "x-amz-security-token":
		return true
	}
	return false
}

// assertRequiredConverse pins the provider-required meaning of one decoded leg
// (absolute anchors). Text values are compared but reported by LENGTH only.
func assertRequiredConverse(t *testing.T, leg string, c converseContract, r bedrockRow) {
	t.Helper()

	// System turns lifted to top-level `system` as {text} blocks (never left in messages).
	wantSys := r.wantSystemTexts()
	if len(c.System) != len(wantSys) {
		t.Errorf("%s: system block count = %d, want %d", leg, len(c.System), len(wantSys))
	} else {
		for i, blk := range c.System {
			if blk.Text != wantSys[i] {
				t.Errorf("%s: system block[%d] text mismatch (got_len=%d want_len=%d)", leg, i, len(blk.Text), len(wantSys[i]))
			}
		}
	}

	// Provider messages: exact ordered non-system turns, each one {text} block.
	wantMsgs := r.wantProviderMessages()
	if len(c.Messages) != len(wantMsgs) {
		t.Fatalf("%s: messages count = %d, want %d", leg, len(c.Messages), len(wantMsgs))
	}
	for i, m := range c.Messages {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("%s: message[%d] retained forbidden role %q", leg, i, m.Role)
		}
		if m.Role != wantMsgs[i].role {
			t.Errorf("%s: message[%d] role = %q, want %q", leg, i, m.Role, wantMsgs[i].role)
		}
		if len(m.Content) != 1 {
			t.Fatalf("%s: message[%d] content blocks = %d, want 1", leg, i, len(m.Content))
		}
		if m.Content[0].Text != wantMsgs[i].text {
			t.Errorf("%s: message[%d] text mismatch (got_len=%d want_len=%d)", leg, i, len(m.Content[0].Text), len(wantMsgs[i].text))
		}
	}

	// inferenceConfig mapping (temperature/top_p compared at f32 precision — BAML
	// widens the f32 it parsed, native emits f64; both round to the same float32).
	if r.opts.maxTokens < 0 && len(r.opts.stopSequences) == 0 && r.opts.temperature == nil && r.opts.topP == nil {
		if c.InferenceConfig != nil {
			t.Errorf("%s: inferenceConfig present but the row sets no inference fields", leg)
		}
	} else {
		if c.InferenceConfig == nil {
			t.Fatalf("%s: inferenceConfig absent but the row sets inference fields", leg)
		}
		if !equalPtrInt(c.InferenceConfig.MaxTokens, r.wantMaxTokens()) {
			t.Errorf("%s: inferenceConfig.maxTokens mismatch (got_set=%v want_set=%v)", leg, c.InferenceConfig.MaxTokens != nil, r.wantMaxTokens() != nil)
		}
		if !equalPtrFloat32(c.InferenceConfig.Temperature, r.opts.temperature) {
			t.Errorf("%s: inferenceConfig.temperature mismatch (got_set=%v want_set=%v)", leg, c.InferenceConfig.Temperature != nil, r.opts.temperature != nil)
		}
		if !equalPtrFloat32(c.InferenceConfig.TopP, r.opts.topP) {
			t.Errorf("%s: inferenceConfig.topP mismatch (got_set=%v want_set=%v)", leg, c.InferenceConfig.TopP != nil, r.opts.topP != nil)
		}
		if !equalStrings(c.InferenceConfig.StopSequences, r.opts.stopSequences) {
			t.Errorf("%s: inferenceConfig.stopSequences count = %d, want %d", leg, len(c.InferenceConfig.StopSequences), len(r.opts.stopSequences))
		}
	}

	// additionalModelRequestFields carried through by value (order-insensitive).
	if !normalizedJSONEqual(c.AdditionalModelRequestFields, r.additional) {
		t.Errorf("%s: additionalModelRequestFields diverge from the row input (got_keys=%d want_keys=%d)", leg, len(c.AdditionalModelRequestFields), len(r.additional))
	}
}

// assertConverseEquivalent proves the two legs carry the SAME Converse meaning
// (diagnostic cross-check; the byte-equality ledger still pins the divergence).
func assertConverseEquivalent(t *testing.T, a, b converseContract) {
	t.Helper()
	if len(a.System) != len(b.System) {
		t.Errorf("cross-leg system count mismatch: baml=%d nanollm=%d", len(a.System), len(b.System))
	} else {
		for i := range a.System {
			if a.System[i].Text != b.System[i].Text {
				t.Errorf("cross-leg system[%d] text mismatch (baml_len=%d nanollm_len=%d)", i, len(a.System[i].Text), len(b.System[i].Text))
			}
		}
	}
	if len(a.Messages) != len(b.Messages) {
		t.Fatalf("cross-leg message count mismatch: baml=%d nanollm=%d", len(a.Messages), len(b.Messages))
	}
	for i := range a.Messages {
		if a.Messages[i].Role != b.Messages[i].Role {
			t.Errorf("cross-leg message[%d] role mismatch: baml=%q nanollm=%q", i, a.Messages[i].Role, b.Messages[i].Role)
		}
		if len(a.Messages[i].Content) != len(b.Messages[i].Content) {
			t.Fatalf("cross-leg message[%d] content count mismatch", i)
		}
		for j := range a.Messages[i].Content {
			if a.Messages[i].Content[j].Text != b.Messages[i].Content[j].Text {
				t.Errorf("cross-leg message[%d] content[%d] text mismatch", i, j)
			}
		}
	}
	if (a.InferenceConfig == nil) != (b.InferenceConfig == nil) {
		t.Fatalf("cross-leg inferenceConfig presence mismatch: baml=%v nanollm=%v", a.InferenceConfig != nil, b.InferenceConfig != nil)
	}
	if a.InferenceConfig != nil {
		if !equalPtrInt(a.InferenceConfig.MaxTokens, b.InferenceConfig.MaxTokens) {
			t.Error("cross-leg inferenceConfig.maxTokens mismatch")
		}
		if !equalPtrFloat32(a.InferenceConfig.Temperature, b.InferenceConfig.Temperature) {
			t.Error("cross-leg inferenceConfig.temperature mismatch (beyond f32 precision)")
		}
		if !equalPtrFloat32(a.InferenceConfig.TopP, b.InferenceConfig.TopP) {
			t.Error("cross-leg inferenceConfig.topP mismatch (beyond f32 precision)")
		}
		if !equalStrings(a.InferenceConfig.StopSequences, b.InferenceConfig.StopSequences) {
			t.Error("cross-leg inferenceConfig.stopSequences mismatch")
		}
	}
	if !normalizedJSONEqual(a.AdditionalModelRequestFields, b.AdditionalModelRequestFields) {
		t.Error("cross-leg additionalModelRequestFields mismatch (beyond member order)")
	}
}

// assertConverseTopLevel pins a leg's top-level member order against the fixed
// Converse root order (messages, then optional system / inferenceConfig /
// additionalModelRequestFields) and proves NO forbidden top-level `model` member.
func assertConverseTopLevel(t *testing.T, leg string, body []byte) {
	t.Helper()
	keys := topLevelKeys(t, leg, body)
	if len(keys) == 0 || keys[0] != "messages" {
		t.Errorf("%s: Converse root must begin with messages, got %v", leg, keys)
	}
	// The permitted members in their fixed relative order.
	order := map[string]int{"messages": 0, "system": 1, "inferenceConfig": 2, "additionalModelRequestFields": 3}
	last := -1
	for _, k := range keys {
		rank, ok := order[k]
		if !ok {
			t.Errorf("%s: Converse root carries an unexpected member %q", leg, k)
			continue
		}
		if rank <= last {
			t.Errorf("%s: Converse root member %q out of the fixed order (keys=%v)", leg, k, keys)
		}
		last = rank
	}
	if _, hasModel := order["model"]; hasModel {
		t.Fatal("internal: model must not be a permitted Converse root member")
	}
}

// --- SigV4 independent verification (§7). ---

// sigV4Auth is a parsed Authorization header.
type sigV4Auth struct {
	accessKeyID     string
	credentialScope string   // <date>/<region>/<service>/aws4_request
	scopeDate       string   // <date>
	signedHeaders   []string // lowercase, sorted
	signature       string   // 64-char lowercase hex
}

// parseSigV4Authorization parses `AWS4-HMAC-SHA256 Credential=<akid>/<scope>,
// SignedHeaders=<a;b;c>, Signature=<hex>`. It never returns the raw signature to a
// diagnostic (callers redact); the scope/header-names are non-secret.
func parseSigV4Authorization(v string) (sigV4Auth, bool) {
	if !strings.HasPrefix(v, sigV4Algorithm+" ") {
		return sigV4Auth{}, false
	}
	var out sigV4Auth
	for _, part := range strings.Split(strings.TrimPrefix(v, sigV4Algorithm+" "), ", ") {
		switch {
		case strings.HasPrefix(part, "Credential="):
			cred := strings.TrimPrefix(part, "Credential=")
			slash := strings.IndexByte(cred, '/')
			if slash < 0 {
				return sigV4Auth{}, false
			}
			out.accessKeyID = cred[:slash]
			out.credentialScope = cred[slash+1:]
			if d := strings.IndexByte(out.credentialScope, '/'); d >= 0 {
				out.scopeDate = out.credentialScope[:d]
			}
		case strings.HasPrefix(part, "SignedHeaders="):
			out.signedHeaders = strings.Split(strings.TrimPrefix(part, "SignedHeaders="), ";")
		case strings.HasPrefix(part, "Signature="):
			out.signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if out.accessKeyID == "" || out.credentialScope == "" || len(out.signedHeaders) == 0 || out.signature == "" {
		return sigV4Auth{}, false
	}
	return out, true
}

// hmacSHA256 is the SigV4 keyed hash primitive.
func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// deriveSigningKey builds the SigV4 signing key from a secret + scope components.
func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, sigV4Terminator)
}

// sigV4EscapePath double-encodes a request path for the SigV4 canonical URI exactly
// as smithy's httpbinding.EscapePath(path, false) does for a non-S3 service: every
// non-unreserved byte (the input is already a percent-encoded EscapedPath, so its
// `%` bytes are re-escaped to `%25`) is %XX-escaped; `/` is preserved.
func sigV4EscapePath(escapedPath string) string {
	var b strings.Builder
	for i := 0; i < len(escapedPath); i++ {
		c := escapedPath[i]
		if isUnreserved(c) || c == '/' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// stripExcessSpaces trims and collapses runs of interior spaces in a canonical
// header value, matching the AWS signer's StripExcessSpaces + TrimSpace.
func stripExcessSpaces(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// deriveSigV4SignatureForPlan reconstructs the SigV4 canonical request from a
// signed request's OWN signed-header list + method/URL/body and recomputes the
// signature with the given secret. This is a from-scratch verification — no AWS SDK
// and no nanollm — and is the primitive both the native verification and every
// mutation control drive. host is taken from the URL; the payload hash is the
// SHA-256 of the exact body bytes; every other signed header value comes from the
// plan header map. It returns the recomputed lowercase-hex signature.
func deriveSigV4SignatureForPlan(t *testing.T, method, rawURL string, planHeaders map[string]string, a sigV4Auth, body, secret []byte, region string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal("sigv4 verify: url did not parse (stage=url_parse)")
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	var canonicalHeaders strings.Builder
	for _, name := range a.signedHeaders {
		var val string
		switch name {
		case "host":
			val = u.Host
		case "content-length":
			val = strconv.Itoa(len(body))
		default:
			v, ok := planHeaders[name]
			if !ok {
				t.Fatalf("sigv4 verify: signed header %q absent from the plan (stage=missing_signed_header)", name)
			}
			val = v
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(stripExcessSpaces(val))
		canonicalHeaders.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		method,
		sigV4EscapePath(u.EscapedPath()),
		"", // canonical query string (Converse carries none)
		canonicalHeaders.String(),
		strings.Join(a.signedHeaders, ";"),
		payloadHash,
	}, "\n")
	crHash := sha256.Sum256([]byte(canonicalRequest))

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		planHeaders["x-amz-date"],
		a.credentialScope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	key := deriveSigningKey(string(secret), a.scopeDate, region, bedrockService)
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

// legacyCapturingTransport records the outbound *http.Request the genuine llmhttp
// signing path produces and returns a canned 200 so Execute completes — no network
// I/O, no real AWS. It is the seam that lets a test observe baml-rest's ACTUAL
// signed request without a socket.
type legacyCapturingTransport struct{ req *http.Request }

func (c *legacyCapturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.req = r
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    r,
	}, nil
}

// runRealLegacySigner drives an UNSIGNED Bedrock request through baml-rest's
// GENUINE legacy SigV4 signing path — the real llmhttp.Client.Execute →
// signRequest → aws-sdk-go-v2 signer at bamlutils/llmhttp/awssigv4.go, the exact
// code the production send path runs — NOT a re-implementation. The signing clock
// is pinned deterministically via AWSAuthConfig.NowFunc (the production test seam);
// a capturing transport records the signed request and returns a canned response,
// so nothing is sent and no real AWS credential chain is touched (the creds are a
// fixed static fake provider). It returns the signed request's headers exactly as
// the legacy signer emitted them — including the X-Amz-Content-Sha256 it sets and
// signs pre-sign (awssigv4.go:135), which is asserted, never stripped.
func runRealLegacySigner(t *testing.T, method, rawURL string, body []byte, baseHeaders map[string]string, akid, secret, sessionToken, region string, at time.Time) http.Header {
	t.Helper()
	rt := &legacyCapturingTransport{}
	// Mode=NetHTTP forces the net/http backend so our RoundTripper is the transport;
	// RewriteRules=nil means no URL rewrite, so the signature is over rawURL as given.
	client := llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
		NetHTTPClient: &http.Client{Transport: rt},
		Mode:          llmhttp.ClientModeNetHTTP,
	})
	headers := make(map[string]string, len(baseHeaders))
	for k, v := range baseHeaders {
		headers[k] = v
	}
	req := &llmhttp.Request{
		URL:     rawURL,
		Method:  method,
		Headers: headers,
		Body:    string(body),
		AWSAuth: &llmhttp.AWSAuthConfig{
			Region:      region,
			Service:     bedrockService,
			Credentials: credentials.NewStaticCredentialsProvider(akid, secret, sessionToken),
			// Pin the signing clock to native's Meta.SignedAt — the deterministic
			// phase/clock control §7 requires (no nonce; timestamp is the only variable).
			NowFunc: func() time.Time { return at },
		},
	}
	if _, err := client.Execute(context.Background(), req, nil); err != nil {
		// The error can echo the request/config — report the stage only.
		t.Fatal("legacy signer: llmhttp.Execute failed (stage=execute)")
	}
	if rt.req == nil {
		t.Fatal("legacy signer: capturing transport recorded no request (stage=capture)")
	}
	return rt.req.Header
}

// bedrockPlanHeaders folds a signed plan's ordered header pairs into a
// lowercase-keyed map.
func bedrockPlanHeaders(prep *nanollm.PreparedRequest) map[string]string {
	m := make(map[string]string, len(prep.Headers))
	for _, h := range prep.Headers {
		m[strings.ToLower(h[0])] = h[1]
	}
	return m
}

// assertBedrockSigV4 is the SigV4 security bar for one row.
func assertBedrockSigV4(t *testing.T, r bedrockRow, cap bedrockCapture) {
	t.Helper()
	prep := cap.prep
	planHeaders := bedrockPlanHeaders(prep)

	// Signing metadata must be present (a signed plan).
	if prep.Meta.SignedAt == nil {
		t.Fatal("native bedrock plan carries no Meta.SignedAt (unsigned plan)")
	}
	if prep.Meta.ExpiresAt == nil {
		t.Fatal("native bedrock plan carries no Meta.ExpiresAt (SigV4 window)")
	}
	signedAt := prep.Meta.SignedAt.UTC()
	// Expiry: a bounded, positive SigV4 window after signing.
	window := prep.Meta.ExpiresAt.Sub(*prep.Meta.SignedAt)
	if window <= 0 {
		t.Errorf("plan expiry is not after signing (window=%s)", window)
	}
	if window > 15*time.Minute {
		t.Errorf("plan expiry window = %s, want a bounded SigV4 window (<= 15m)", window)
	}

	authRaw, ok := planHeaders["authorization"]
	if !ok {
		t.Fatal("native bedrock plan carries no Authorization header")
	}
	auth, ok := parseSigV4Authorization(authRaw)
	if !ok {
		t.Fatal("native Authorization is not a well-formed AWS4-HMAC-SHA256 header (stage=parse_auth)")
	}

	// Credential scope: fixed AKID + <date>/<region>/bedrock/aws4_request.
	if auth.accessKeyID != bedrockFakeAKID {
		t.Errorf("credential access key id mismatch (matched=%v)", auth.accessKeyID == bedrockFakeAKID)
	}
	wantScope := auth.scopeDate + "/" + bedrockFenceRegion + "/" + bedrockService + "/" + sigV4Terminator
	if auth.credentialScope != wantScope {
		t.Errorf("credential scope = %q, want %q", auth.credentialScope, wantScope)
	}

	// X-Amz-Date: present, second-precise, and consistent with SignedAt + the scope date.
	amzDate, ok := planHeaders["x-amz-date"]
	if !ok {
		t.Fatal("native plan carries no X-Amz-Date")
	}
	if amzDate != signedAt.Format(sigV4TimeFormat) {
		t.Errorf("X-Amz-Date = %q, want SignedAt-formatted %q", amzDate, signedAt.Format(sigV4TimeFormat))
	}
	if auth.scopeDate != signedAt.Format(sigV4DateFormat) || auth.scopeDate != amzDate[:8] {
		t.Errorf("credential scope date = %q, want %q (== X-Amz-Date date)", auth.scopeDate, signedAt.Format(sigV4DateFormat))
	}

	// NATIVE signed-header SET: sorted, and EXACTLY nanollm's minimal set (host +
	// base semantic headers + x-amz-date, plus x-amz-security-token iff a session
	// token is used). This is a CHECKED-IN divergence fact: nanollm deliberately does
	// NOT sign content-length or x-amz-content-sha256 (see the legacy comparison
	// below, which signs both). Both are valid SigV4 — the payload hash is carried in
	// the canonical request regardless; x-amz-content-sha256 as a signed header is
	// required only for Amazon S3, not bedrock-runtime — so native is not
	// under-signed, and the set is pinned so a future nanollm signing change is
	// noticed.
	if !sort.StringsAreSorted(auth.signedHeaders) {
		t.Errorf("signed headers are not sorted: %v", auth.signedHeaders)
	}
	nativeWantSigned := bedrockExpectedSignedHeaders(r.session, false)
	if !equalStringSet(auth.signedHeaders, nativeWantSigned) {
		t.Errorf("native signed-header set = %v, want %v", auth.signedHeaders, nativeWantSigned)
	}
	// nanollm's minimal set also means the plan emits NO x-amz-content-sha256 /
	// content-length header at all — record that explicitly (not silently dropped).
	if _, has := planHeaders["x-amz-content-sha256"]; has {
		t.Error("native plan emits x-amz-content-sha256 (nanollm's known minimal signed set omits it — signing behavior changed)")
	}

	// Session-token handling: the X-Amz-Security-Token header is present AND signed
	// iff the row uses temporary credentials — never dropped to force a simpler set.
	_, hasToken := planHeaders["x-amz-security-token"]
	if hasToken != r.session {
		t.Errorf("x-amz-security-token present=%v, want %v", hasToken, r.session)
	}
	if inStringSet(auth.signedHeaders, "x-amz-security-token") != r.session {
		t.Errorf("x-amz-security-token signed=%v, want %v", inStringSet(auth.signedHeaders, "x-amz-security-token"), r.session)
	}
	// Presence + signed is not enough: a WRONG-but-present token would still be
	// signed and self-verify, yet AWS would reject that temporary credential. Assert
	// the native token is EXACTLY the CONFIGURED test session token (compared by
	// equality only — the token value is never printed, keeping diagnostics redacted).
	if r.session && planHeaders["x-amz-security-token"] != bedrockFakeSessionTk {
		t.Error("native x-amz-security-token is not the configured temporary-credential token (a wrong-but-present token must not pass)")
	}

	// (A) INDEPENDENT from-scratch crypto/hmac verification of native's signature
	// against the fixed FAKE secret key, reconstructing the canonical request from
	// native's OWN signed-header list. The payload hash is tied to the EXACT body
	// bytes (SHA-256(prep.Body)); a body/host/path/date/token mutation breaks it
	// (proven by the controls). This is fully independent of both nanollm (which
	// produced the signature) and any AWS SDK.
	nativeBodyHash := sha256.Sum256(prep.Body)
	got := deriveSigV4SignatureForPlan(t, prep.Method, prep.URL, planHeaders, auth, prep.Body, []byte(bedrockFakeSecret), bedrockFenceRegion)
	if !hmac.Equal([]byte(got), []byte(auth.signature)) {
		// Never print either signature (secret-derived) — a fixed token only.
		t.Error("native SigV4 signature FAILED independent crypto verification against the fake key")
	}

	// (B) REAL legacy-signer cross-check over BAML's UNSIGNED request (§7 step 5-6):
	// drive BAML's captured method/URL/body + base headers through baml-rest's ACTUAL
	// llmhttp signing path with the clock pinned to native's Meta.SignedAt via
	// AWSAuthConfig.NowFunc (no re-implementation). This is the production signer, so
	// it signs the fuller set it always signs.
	sessionTok := ""
	if r.session {
		sessionTok = bedrockFakeSessionTk
	}
	legacyHdr := runRealLegacySigner(t, cap.baml.snap.Method, cap.baml.snap.URL, cap.baml.body, bedrockBaseSemanticHeaders(cap.baml.headers), bedrockFakeAKID, bedrockFakeSecret, sessionTok, bedrockFenceRegion, signedAt)
	legacyMap := httpHeaderMap(legacyHdr)
	legacyAuth, ok := parseSigV4Authorization(legacyMap["authorization"])
	if !ok {
		t.Fatal("legacy signer produced a malformed Authorization")
	}

	// Body-INDEPENDENT signed fields MUST match native exactly (same pinned clock,
	// creds, host, region) — this is what the phase/clock control proves.
	if legacyMap["x-amz-date"] != amzDate {
		t.Errorf("legacy X-Amz-Date = %q, want native's %q", legacyMap["x-amz-date"], amzDate)
	}
	if legacyAuth.credentialScope != auth.credentialScope {
		t.Errorf("legacy credential scope = %q, want native's %q", legacyAuth.credentialScope, auth.credentialScope)
	}

	// SESSION-TOKEN VALUE (temporary credentials): the signed x-amz-security-token
	// must be the CONFIGURED test token on BOTH legs and equal to each other — so a
	// mismatched/wrong (but present and signed) token cannot pass. Compared by
	// equality only; the token value is never printed.
	if r.session {
		if legacyMap["x-amz-security-token"] != bedrockFakeSessionTk {
			t.Error("legacy x-amz-security-token is not the configured temporary-credential token")
		}
		if legacyMap["x-amz-security-token"] != planHeaders["x-amz-security-token"] {
			t.Error("native and legacy x-amz-security-token differ (both must carry the same configured token)")
		}
	}

	// PAYLOAD-HASH HEADER (P1b): the real legacy signer sets and signs
	// X-Amz-Content-Sha256 = SHA-256(body). Assert its EXACT value (the header is
	// preserved and verified, never stripped). A mutation control
	// (TestBedrockControlLegacyContentSha256MutationInvalidates) proves it is signed.
	legacyContentSha := legacyMap["x-amz-content-sha256"]
	bamlBodyHash := sha256.Sum256(cap.baml.body)
	if legacyContentSha != hex.EncodeToString(bamlBodyHash[:]) {
		t.Error("legacy X-Amz-Content-Sha256 is not the exact SHA-256 of the signed body")
	}

	// SIGNED-HEADER SET DIVERGENCE (recorded honestly): the production legacy signer
	// signs a strict SUPERSET of native's set — it additionally signs content-length
	// and x-amz-content-sha256. Both requests are valid SigV4; nanollm omits the two
	// (benign, see the native-set comment above). Pin the legacy superset so the
	// divergence is checked in on BOTH sides, and prove it is a strict superset.
	legacyWantSigned := bedrockExpectedSignedHeaders(r.session, true)
	if !equalStringSet(legacyAuth.signedHeaders, legacyWantSigned) {
		t.Errorf("legacy signed-header set = %v, want the production superset %v", legacyAuth.signedHeaders, legacyWantSigned)
	}
	for _, h := range auth.signedHeaders {
		if !inStringSet(legacyAuth.signedHeaders, h) {
			t.Errorf("legacy signed set is not a superset of native's: missing %q", h)
		}
	}
	if inStringSet(auth.signedHeaders, "x-amz-content-sha256") {
		t.Error("native unexpectedly signs x-amz-content-sha256 (the recorded divergence assumes it does not)")
	}

	// The real legacy signer's output is itself VALID SigV4 over its OWN (superset)
	// canonical request — independently verified with the same from-scratch hmac path.
	// This also cross-validates the from-scratch verifier against a DIFFERENT real
	// SigV4 implementation (aws-sdk-go-v2, via llmhttp) over the same URL/host/date.
	gotLegacy := deriveSigV4SignatureForPlan(t, cap.baml.snap.Method, cap.baml.snap.URL, legacyMap, legacyAuth, cap.baml.body, []byte(bedrockFakeSecret), bedrockFenceRegion)
	if !hmac.Equal([]byte(gotLegacy), []byte(legacyAuth.signature)) {
		t.Error("the real legacy signer's signature FAILED independent crypto verification")
	}

	// Because the legacy signer signs a SUPERSET, its Authorization can never equal
	// native's — do NOT force equality (that would require dropping headers). Instead,
	// tie the two legs through the payload hash: for a byte-equal body the legacy
	// signer's X-Amz-Content-Sha256 must equal the hash native's own signature is
	// bound to; for a validly divergent body they must differ (§7 step 7).
	nativeHashHex := hex.EncodeToString(nativeBodyHash[:])
	if r.wantBodyByteEqual {
		if legacyContentSha != nativeHashHex {
			t.Error("byte-equal body but legacy payload hash != native's signed payload hash")
		}
	} else {
		if legacyContentSha == nativeHashHex {
			t.Error("body diverged but legacy and native payload hashes matched (divergent payloads must hash differently)")
		}
	}
}

// bedrockExpectedSignedHeaders returns the sorted signed-header set a signer emits
// for a Bedrock Converse request: nanollm's minimal set, or (superset=true) the
// production baml-rest legacy signer's fuller set that additionally signs
// content-length and x-amz-content-sha256. x-amz-security-token is included iff the
// request uses temporary credentials.
func bedrockExpectedSignedHeaders(session, superset bool) []string {
	set := []string{"accept", "content-type", "host", "x-amz-date"}
	if superset {
		set = append(set, "content-length", "x-amz-content-sha256")
	}
	if session {
		set = append(set, "x-amz-security-token")
	}
	sort.Strings(set)
	return set
}

// bedrockBaseSemanticHeaders returns the base semantic headers (accept /
// content-type) from BAML's captured (unsigned) header map, canonicalized to
// lowercase — the headers handed to the legacy signer (which owns the SigV4 headers).
func bedrockBaseSemanticHeaders(headers map[string]string) map[string]string {
	base := map[string]string{}
	for k, v := range headers {
		lk := strings.ToLower(k)
		if lk == "accept" || lk == "content-type" {
			base[lk] = v
		}
	}
	return base
}

// httpHeaderMap folds a signed *http.Request's header set into the lowercase
// plan-header map shape deriveSigV4SignatureForPlan consumes (Authorization / X-Amz-*
// + the base headers that were signed).
func httpHeaderMap(h http.Header) map[string]string {
	m := map[string]string{}
	for name, vals := range h {
		if len(vals) == 0 {
			continue
		}
		m[strings.ToLower(name)] = vals[0]
	}
	return m
}

// --- small comparison helpers (redacted diagnostics). ---

func equalPtrFloat32(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// BAML widens the f32 it parsed; native emits f64. Compare at float32 precision.
	return float32(*a) == float32(*b)
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func inStringSet(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// normalizedJSONEqual compares two JSON-compatible values for deep equality after a
// marshal/unmarshal round-trip, so member order and numeric spelling (int vs
// float64) never make two equal objects compare unequal. nil and an empty object
// are treated as equal (an absent additionalModelRequestFields vs `{}`).
func normalizedJSONEqual(a, b any) bool {
	na := normalizeJSON(a)
	nb := normalizeJSON(b)
	return reflect.DeepEqual(na, nb)
}

func normalizeJSON(v any) any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return map[string]any{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

// --- Mutation controls (§9-B): each proves a positive SigV4 assertion is
// load-bearing by INVALIDATING the signature and requiring detection. ---

// bedrockControlRow is a byte-equal, session-less row reused by the controls (a
// clean, deterministic signed plan).
func bedrockControlRow() bedrockRow {
	return bedrockRow{
		name:              "control",
		model:             bedrockColonModel,
		messages:          []corpusMessage{{role: "system", text: "Sys."}, {role: "user", text: "Hi."}},
		opts:              bodyOptions{maxTokens: 32},
		wantModelEncoded:  true,
		wantBodyByteEqual: true,
	}
}

// verifyControlSignature recomputes native's signature for a (possibly mutated)
// method/URL/headers/body and reports whether it still matches the ORIGINAL
// Authorization signature. A control asserts a mutation makes this FALSE.
func verifyControlSignature(t *testing.T, method, rawURL string, planHeaders map[string]string, auth sigV4Auth, body []byte) bool {
	t.Helper()
	got := deriveSigV4SignatureForPlan(t, method, rawURL, planHeaders, auth, body, []byte(bedrockFakeSecret), bedrockFenceRegion)
	return hmac.Equal([]byte(got), []byte(auth.signature))
}

// TestBedrockControlSignatureValidBaseline: the UNMUTATED plan verifies (so the
// mutation controls below prove detection, not a broken verifier).
func TestBedrockControlSignatureValidBaseline(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, ok := parseSigV4Authorization(ph["authorization"])
	if !ok {
		t.Fatal("baseline: Authorization did not parse")
	}
	if !verifyControlSignature(t, cap.prep.Method, cap.prep.URL, ph, auth, cap.prep.Body) {
		t.Fatal("baseline: the unmutated native signature failed verification (the verifier is broken)")
	}
}

// TestBedrockControlBodyMutationInvalidates: a single-byte body change breaks the
// payload-hash → signature binding.
func TestBedrockControlBodyMutationInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, _ := parseSigV4Authorization(ph["authorization"])

	mutated := bytes.Replace(cap.prep.Body, []byte(`"maxTokens":32`), []byte(`"maxTokens":33`), 1)
	if bytes.Equal(mutated, cap.prep.Body) {
		t.Fatal("the body mutation did not change the bytes")
	}
	if verifyControlSignature(t, cap.prep.Method, cap.prep.URL, ph, auth, mutated) {
		t.Fatal("a mutated body was NOT detected by SigV4 verification (payload hash not bound to the body)")
	}
}

// TestBedrockControlHostMutationInvalidates: signing covers host — a redirected
// host breaks the signature (no signed plan may be silently re-hosted).
func TestBedrockControlHostMutationInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, _ := parseSigV4Authorization(ph["authorization"])

	badURL := strings.Replace(cap.prep.URL, bedrockFenceRegion, "us-west-2", 1)
	if badURL == cap.prep.URL {
		t.Fatal("the host mutation did not change the URL")
	}
	if verifyControlSignature(t, cap.prep.Method, badURL, ph, auth, cap.prep.Body) {
		t.Fatal("a mutated host was NOT detected by SigV4 verification (host not signed)")
	}
}

// TestBedrockControlPathMutationInvalidates: the canonical URI is signed — a
// tampered model path breaks the signature.
func TestBedrockControlPathMutationInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, _ := parseSigV4Authorization(ph["authorization"])

	badURL := strings.Replace(cap.prep.URL, "/converse", "/invoke", 1)
	if badURL == cap.prep.URL {
		t.Fatal("the path mutation did not change the URL")
	}
	if verifyControlSignature(t, cap.prep.Method, badURL, ph, auth, cap.prep.Body) {
		t.Fatal("a mutated path was NOT detected by SigV4 verification (canonical URI not signed)")
	}
}

// TestBedrockControlDateMutationInvalidates: X-Amz-Date is signed — a changed date
// breaks the signature (and the credential scope date is bound to it).
func TestBedrockControlDateMutationInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, _ := parseSigV4Authorization(ph["authorization"])

	mutated := map[string]string{}
	for k, v := range ph {
		mutated[k] = v
	}
	// Advance the amz-date by one second; the signature (over the original date) must fail.
	orig, err := time.Parse(sigV4TimeFormat, ph["x-amz-date"])
	if err != nil {
		t.Fatal("control: X-Amz-Date did not parse")
	}
	mutated["x-amz-date"] = orig.Add(time.Second).UTC().Format(sigV4TimeFormat)
	if mutated["x-amz-date"] == ph["x-amz-date"] {
		t.Fatal("the date mutation did not change X-Amz-Date")
	}
	if verifyControlSignature(t, cap.prep.Method, cap.prep.URL, mutated, auth, cap.prep.Body) {
		t.Fatal("a mutated X-Amz-Date was NOT detected by SigV4 verification (date not signed)")
	}
}

// TestBedrockControlSessionTokenMutationInvalidates: a temporary-credential plan
// signs X-Amz-Security-Token — tampering with it breaks the signature (a session
// token can never be silently dropped/altered to obtain a match).
func TestBedrockControlSessionTokenMutationInvalidates(t *testing.T) {
	r := bedrockControlRow()
	r.session = true
	cap := runBedrockRow(t, r)
	ph := bedrockPlanHeaders(cap.prep)
	auth, ok := parseSigV4Authorization(ph["authorization"])
	if !ok {
		t.Fatal("session control: Authorization did not parse")
	}
	if !inStringSet(auth.signedHeaders, "x-amz-security-token") {
		t.Fatal("session control precondition: x-amz-security-token is not signed")
	}
	// Baseline verifies.
	if !verifyControlSignature(t, cap.prep.Method, cap.prep.URL, ph, auth, cap.prep.Body) {
		t.Fatal("session control: the unmutated session-token signature failed verification")
	}
	// Tamper the signed session token; the signature (over the original token) must fail.
	mutated := map[string]string{}
	for k, v := range ph {
		mutated[k] = v
	}
	mutated["x-amz-security-token"] = ph["x-amz-security-token"] + "TAMPERED"
	if verifyControlSignature(t, cap.prep.Method, cap.prep.URL, mutated, auth, cap.prep.Body) {
		t.Fatal("a tampered X-Amz-Security-Token was NOT detected by SigV4 verification (session token not signed)")
	}
}

// TestBedrockControlWrongKeyInvalidates: the signature is bound to the specific
// secret — verifying against a DIFFERENT fake key must fail (the verification is
// genuinely keyed, not a structural check).
func TestBedrockControlWrongKeyInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	ph := bedrockPlanHeaders(cap.prep)
	auth, _ := parseSigV4Authorization(ph["authorization"])

	wrongSecret := []byte("wJalrXUtnFEMI/K7MDENG+bPxRfiCYWRONGKEYXX")
	if bytes.Equal(wrongSecret, []byte(bedrockFakeSecret)) {
		t.Fatal("control: the wrong key equals the fake key")
	}
	got := deriveSigV4SignatureForPlan(t, cap.prep.Method, cap.prep.URL, ph, auth, cap.prep.Body, wrongSecret, bedrockFenceRegion)
	if hmac.Equal([]byte(got), []byte(auth.signature)) {
		t.Fatal("the signature verified under a WRONG key (verification is not key-bound)")
	}
}

// TestBedrockControlNoModelFieldInBody: a Converse body must NOT carry a top-level
// `model` member (the model rides in the URL). An injected `model` is rejected by
// the strict decode.
func TestBedrockControlNoModelFieldInBody(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	if _, err := strictDecodeConverse(cap.prep.Body); err != nil {
		t.Fatal("precondition: the unmutated native Converse body failed strict decode")
	}
	injected := bytes.Replace(cap.prep.Body, []byte(`{"messages"`), []byte(`{"model":"x","messages"`), 1)
	if bytes.Equal(injected, cap.prep.Body) {
		t.Fatal("the model-injection mutation did not change the body")
	}
	if _, err := strictDecodeConverse(injected); err == nil {
		t.Fatal("a Converse body carrying a forbidden top-level model was NOT rejected")
	}
}

// TestBedrockControlRenamedInferenceFieldRejected: a renamed inferenceConfig field
// (maxTokens → maxTokenz) is rejected by the strict decode.
func TestBedrockControlRenamedInferenceFieldRejected(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	mutated := bytes.Replace(cap.prep.Body, []byte(`"maxTokens"`), []byte(`"maxTokenz"`), 1)
	if bytes.Equal(mutated, cap.prep.Body) {
		t.Fatal("the field-rename mutation did not change the body")
	}
	if _, err := strictDecodeConverse(mutated); err == nil {
		t.Fatal("a renamed inferenceConfig field was NOT rejected by the strict decode")
	}
}

// TestBedrockControlLegacyContentSha256MutationInvalidates: the REAL legacy signer
// SIGNS X-Amz-Content-Sha256 (unlike native, which omits it from its minimal set).
// Tampering that signed header's value must break the legacy signature — proving the
// payload-hash header is genuinely signed and its exact SHA-256(body) value is
// load-bearing (P1b), not decorative or droppable.
func TestBedrockControlLegacyContentSha256MutationInvalidates(t *testing.T) {
	cap := runBedrockRow(t, bedrockControlRow())
	signedAt := cap.prep.Meta.SignedAt.UTC()
	legacy := runRealLegacySigner(t, cap.baml.snap.Method, cap.baml.snap.URL, cap.baml.body, bedrockBaseSemanticHeaders(cap.baml.headers), bedrockFakeAKID, bedrockFakeSecret, "", bedrockFenceRegion, signedAt)
	lm := httpHeaderMap(legacy)
	auth, ok := parseSigV4Authorization(lm["authorization"])
	if !ok {
		t.Fatal("legacy control: Authorization did not parse")
	}
	if !inStringSet(auth.signedHeaders, "x-amz-content-sha256") {
		t.Fatal("legacy control precondition: the real legacy signer does not sign x-amz-content-sha256")
	}
	// Baseline: the legacy signature verifies over BAML's own body.
	if !verifyControlSignature(t, cap.baml.snap.Method, cap.baml.snap.URL, lm, auth, cap.baml.body) {
		t.Fatal("legacy control: the unmutated legacy signature failed verification")
	}
	// Tamper the signed payload-hash header; the signature (over the real hash) must fail.
	mutated := map[string]string{}
	for k, v := range lm {
		mutated[k] = v
	}
	mutated["x-amz-content-sha256"] = strings.Repeat("0", 64)
	if mutated["x-amz-content-sha256"] == lm["x-amz-content-sha256"] {
		t.Fatal("the content-hash mutation did not change the header")
	}
	if verifyControlSignature(t, cap.baml.snap.Method, cap.baml.snap.URL, mutated, auth, cap.baml.body) {
		t.Fatal("a tampered X-Amz-Content-Sha256 was NOT detected (the payload-hash header is not signed by the legacy signer)")
	}
}
