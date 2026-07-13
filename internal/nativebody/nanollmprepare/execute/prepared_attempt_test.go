//go:build nanollm_integration

package execute

// De-BAML Slice 6b shared harness: the fixtures + a baml-rest-owned loopback
// CAPTURE server used by the PreparedRequest-adapter proofs
// (prepared_capture_test.go) and the go-mocklm realism proof
// (prepared_mocklm_test.go).
//
// The capture server is the wire-fidelity oracle: unlike go-mocklm (whose admin
// log records headers as a map[string]string and so cannot prove repeated names
// or per-name order), it records the exact method / request target / effective
// Host / a deep copy of every header value slice / the byte-exact body, plus a
// request counter — everything needed to prove the EXACT PreparedRequest bytes
// are what hit the wire. It also returns fully deterministic success / non-2xx /
// invalid-2xx fixtures so the response side (TranslateResponse -> extraction ->
// native SAP) is exercised end to end.
//
// The fence is the same fail-closed posture as the rest of the module: a literal
// fake API key, UseProcessEnv:false / Env:nil / MaxRetries:0, and a loopback-only
// dial guard on the exact transport — no process-env secret resolution and no
// public egress.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/testutil"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

const (
	// The Slice 6b fence. The alias is deliberately DISTINCT from the target so
	// the "attempted alias, not target" claim is meaningful: nanollm rewrites the
	// body's top-level model to the target, while TranslateResponse keys on the
	// alias — the captured body must carry the target and never the alias.
	p6bAlias  = "p6b-openai-alias"
	p6bTarget = "p6b-openai-target"
	p6bAPIKey = "sk-p6b-fake"

	// p6bSourceModel is the placeholder model in the native body BEFORE nanollm
	// rewrites it to p6bTarget — its absence from the sent body is the rewrite
	// proof.
	p6bSourceModel = "p6b-source-model"

	// p6bTimeout is a generous per-call context deadline (the pipeline proofs are
	// not timing tests).
	p6bTimeout = 30 * time.Second
)

// newPreparedClient builds a v0.3.2 client with ONE openai alias bound to the
// given loopback base (a literal 127.0.0.1 URL from a running test server).
// MaxRetries:0 keeps the attempt count unambiguous; UseProcessEnv:false / Env:nil
// forbid process-env secret resolution.
func newPreparedClient(t *testing.T, base string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       p6bAlias,
			Model:      "openai/" + p6bTarget,
			APIKey:     p6bAPIKey,
			BaseURL:    base + "/v1",
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New (prepared): %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// p6bNativeBody builds ONE OpenAI chat body via baml-rest's own native path
// (nativeprompt + nativebody.BuildOpenAIChat), carrying the source placeholder
// model so nanollm's target rewrite is observable in the sent body.
func p6bNativeBody(t *testing.T) []byte {
	t.Helper()
	rendered := &nativeprompt.RenderedPrompt{
		Kind: nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{
			{Role: "system", Parts: []nativeprompt.Part{{Text: sp("You are a precise extractor.")}}},
			{Role: "user", Parts: []nativeprompt.Part{{Text: sp("Return the person as JSON.")}}},
		},
	}
	body, err := nativebody.BuildOpenAIChat(rendered, nativebody.ClientIntent{
		Provider:    nativebody.ProviderOpenAI,
		TargetModel: p6bSourceModel,
		ModelAlias:  p6bAlias,
	})
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	return body.Bytes()
}

// preparePlan runs nanollm Prepare for the fence alias on the native body and
// asserts the plan-shape invariants every 6b proof relies on: the attempted
// alias, POST, the exact endpoint, the JSON response format, an unexpired plan,
// and the target-model rewrite (alias absent from the body).
func preparePlan(t *testing.T, nano *nanollm.Client, base string, body []byte) *nanollm.PreparedRequest {
	t.Helper()
	prep, err := nano.Prepare(nanollm.Request{
		Model:  p6bAlias,
		Body:   body,
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Meta.ModelAlias != p6bAlias {
		t.Fatalf("plan alias = %q, want %q", prep.Meta.ModelAlias, p6bAlias)
	}
	if prep.Method != "POST" {
		t.Fatalf("plan method = %q, want POST", prep.Method)
	}
	if want := base + "/v1/chat/completions"; prep.URL != want {
		t.Fatalf("plan url = %q, want %q", prep.URL, want)
	}
	if prep.ResponseFormat != nanollm.FormatJSON {
		t.Fatalf("plan response format = %q, want json", prep.ResponseFormat)
	}
	if prep.Expired() {
		t.Fatalf("unsigned openai plan must not be Expired()")
	}
	// Rewrite proof: the sent body carries the configured TARGET, not the source
	// placeholder and not the alias.
	sbody := string(prep.Body)
	if !strings.Contains(sbody, `"model":"`+p6bTarget+`"`) {
		t.Fatalf("plan body missing target model rewrite (%s)", bodyDigest(prep.Body))
	}
	if strings.Contains(sbody, p6bSourceModel) {
		t.Fatalf("plan body still carries the source placeholder model %q", p6bSourceModel)
	}
	if strings.Contains(sbody, p6bAlias) {
		t.Fatalf("plan body leaks the alias %q (must carry the target only)", p6bAlias)
	}
	return prep
}

// --- exact transport: loopback-only, single physical attempt ---

// exactLoopbackExecutor builds an ExactExecutor over a loopback-guarded
// transport that mirrors the exact lane's transparency guarantees: Proxy nil (no
// proxy-auth injection), compression disabled (no injected Accept-Encoding / no
// transparent decode), and keep-alives disabled (no stale-connection replay, so
// one RoundTrip is one server-visible request). A mistyped base fails the dial
// guard rather than escaping to a public host.
func exactLoopbackExecutor() *llmhttp.ExactExecutor {
	return llmhttp.NewExactExecutor(&http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("loopback guard: unparsable dial address %q: %w", addr, err)
			}
			if !isLoopbackHost(host) {
				return nil, fmt.Errorf("loopback guard: refusing non-loopback dial to %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
	})
}

// --- native parser spy ---

// parseSpy wraps a bamlutils.DeBAMLParseFunc and counts invocations. It is the
// INDEPENDENT proof that provider outcomes (non-2xx / invalid-2xx) never reach
// SAP: those cases must leave calls == 0, corroborating AttemptResult.SAPInvoked.
type parseSpy struct {
	fn    bamlutils.DeBAMLParseFunc
	calls int
}

func (s *parseSpy) Parse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	s.calls++
	return s.fn(ctx, req)
}

// --- native output schemas ---

// personSchema6b is a two-field class {name string, age int} the native parser
// cleanly CLAIMS for a well-formed JSON object, DECLINES for non-JSON prose.
func personSchema6b() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("name", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("age", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

// enumTieSchema6b is Root{animal: Animal enum{CAT,DOG}}; a value that
// substring-ties both enum values is a CLAIMED parse error (BAML errors it too),
// distinct from a decline.
func enumTieSchema6b() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("animal", &bamlutils.DynamicProperty{Ref: "Animal"}),
		),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Animal", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "CAT"}, {Name: "DOG"}},
			}),
		),
	}
}

// --- deterministic response fixtures ---

// openAISuccessBody returns a deterministic OpenAI Chat Completions success body
// whose assistant content is exactly `content` (the structured JSON string the
// native parser is fed) and whose usage block is fixed so the retained
// Response.Usage is assertable.
func openAISuccessBody(content string) []byte {
	m := map[string]any{
		"id":     "chatcmpl-p6b",
		"object": "chat.completion",
		"model":  p6bTarget,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}
	b, err := stdjson.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// openAIErrorBody returns a deterministic OpenAI-style non-2xx error body.
func openAIErrorBody(message, typ string) []byte {
	m := map[string]any{"error": map[string]any{"message": message, "type": typ, "code": typ}}
	b, err := stdjson.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// --- baml-rest-owned loopback capture server ---

// recordedExact is one captured request: the exact method, request target
// (path+query), effective Host, a deep copy of the header value slices, and the
// byte-exact body.
type recordedExact struct {
	Method     string
	RequestURI string
	Host       string
	Header     http.Header
	Body       []byte
}

// captureServer is a loopback httptest.Server that records every request and
// replies with a programmable deterministic fixture.
type captureServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	recs       []recordedExact
	respStatus int
	respBody   []byte
}

// newCaptureServer starts a loopback capture server (httptest binds 127.0.0.1)
// defaulting to a 200 + empty body until the test programs a response.
func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{t: t, respStatus: http.StatusOK}
	cs.srv = httptest.NewServer(http.HandlerFunc(cs.handle))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *captureServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// The client may deliberately abandon a read (cap / cancellation cases);
		// record what arrived rather than failing the handler goroutine.
		body = append(body, []byte("<read-error>")...)
	}
	rec := recordedExact{
		Method:     r.Method,
		RequestURI: r.RequestURI,
		Host:       r.Host,
		Header:     r.Header.Clone(),
		Body:       body,
	}
	cs.mu.Lock()
	cs.recs = append(cs.recs, rec)
	status, respBody := cs.respStatus, cs.respBody
	cs.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (cs *captureServer) base() string { return cs.srv.URL }

// setResponse programs the deterministic reply the handler writes next.
func (cs *captureServer) setResponse(status int, body []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.respStatus = status
	cs.respBody = body
}

func (cs *captureServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.recs)
}

// only returns the single captured request, failing if there is not exactly one.
func (cs *captureServer) only() recordedExact {
	cs.t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.recs) != 1 {
		cs.t.Fatalf("want exactly 1 captured request, got %d", len(cs.recs))
	}
	return cs.recs[0]
}

// --- exact-plan-sent assertion ---

// assertExactPlanSent proves the captured request is byte-identical to the
// PreparedRequest fields supplied to the adapter: exact method, exact request
// target + effective host, byte-exact body, and semantic header equality (every
// plan header present with its exact value and per-name order; the ONLY wire-only
// headers permitted are transport-generated framing fields neither planner
// controls). Global header-name order/casing is explicitly NOT compared.
func assertExactPlanSent(t *testing.T, prep *nanollm.PreparedRequest, rec recordedExact) {
	t.Helper()

	if rec.Method != prep.Method {
		t.Errorf("method: wire=%q plan=%q", rec.Method, prep.Method)
	}

	u, err := url.Parse(prep.URL)
	if err != nil {
		t.Fatalf("parsing plan URL %q: %v", prep.URL, err)
	}
	wantTarget := u.EscapedPath()
	if u.RawQuery != "" {
		wantTarget += "?" + u.RawQuery
	}
	if rec.RequestURI != wantTarget {
		t.Errorf("request target: wire=%q plan=%q", rec.RequestURI, wantTarget)
	}
	if rec.Host != u.Host {
		t.Errorf("effective host: wire=%q plan=%q", rec.Host, u.Host)
	}

	if !bytes.Equal(rec.Body, prep.Body) {
		t.Errorf("body not byte-identical: wire=%d bytes, plan=%d bytes", len(rec.Body), len(prep.Body))
	}

	assertHeaderMultisetSent(t, prep.Headers, rec.Header)
}

// transportOnlyHeaders are the request-header names net/http itself frames and
// that no planner controls; they are the ONLY permitted wire-only headers. On
// the server side Go moves Content-Length into r.ContentLength (so it rarely
// appears in r.Header); User-Agent is auto-added unless the plan set it; with
// compression disabled the transport injects no Accept-Encoding; and with
// keep-alives disabled (the exact lane's single-physical-attempt guarantee) it
// frames the connection with Connection: close. The allowlist stays defensive.
var transportOnlyHeaders = map[string]bool{
	"user-agent":      true,
	"content-length":  true,
	"accept-encoding": true,
	"connection":      true,
}

func assertHeaderMultisetSent(t *testing.T, planHeaders [][2]string, wire http.Header) {
	t.Helper()

	// Group the plan's ordered pairs by lowercase name, preserving per-name
	// value order (repeats retained).
	planByName := map[string][]string{}
	var planOrder []string
	for _, p := range planHeaders {
		n := strings.ToLower(p[0])
		if _, seen := planByName[n]; !seen {
			planOrder = append(planOrder, n)
		}
		planByName[n] = append(planByName[n], p[1])
	}

	// http.Header keys are canonical + unique; fold to lowercase for the compare.
	wireByName := map[string][]string{}
	for k, vs := range wire {
		wireByName[strings.ToLower(k)] = append(wireByName[strings.ToLower(k)], vs...)
	}

	for _, n := range planOrder {
		got := wireByName[n]
		want := planByName[n]
		if !slices.Equal(got, want) {
			t.Errorf("header %q: wire values %v != plan values %v (redacted)", n, redactVals(n, got), redactVals(n, want))
		}
	}
	for n := range wireByName {
		if _, inPlan := planByName[n]; inPlan {
			continue
		}
		if !transportOnlyHeaders[n] {
			t.Errorf("unexpected wire-only header %q (not in plan, not transport-generated)", n)
		}
	}
}

func redactVals(name string, vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = testutil.RedactValue(name, v)
	}
	return out
}

// --- small JSON helper ---

// jsonSemEqual reports whether a and b decode to structurally equal JSON values
// (object key order ignored; numbers compared as float64). On a decode failure it
// reports only a size/hash digest of the offending payload, never the raw bytes.
func jsonSemEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := stdjson.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode a (%s): %v", bodyDigest(a), err)
	}
	if err := stdjson.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode b (%s): %v", bodyDigest(b), err)
	}
	return reflect.DeepEqual(av, bv)
}

// --- secret-safe diagnostics (no-full-body-log fence) ---
//
// Failure diagnostics in these gated tests must never emit a full request body,
// response body, translated envelope, structured output, assistant text, or a
// header value/credential — even though the fixtures use fake keys. The helpers
// below reduce every such payload to a size + short-hash summary and reduce the
// response/result structs to their safe scalar facts, so a CI failure log holds
// no plan or provider payload. The assertions stay exactly as strong; only the
// message is derived from redacted facts.

// bodyDigest renders a payload as "<n>B sha256:<12hex>" — never the raw bytes.
func bodyDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%dB sha256:%s", len(b), hex.EncodeToString(sum[:])[:12])
}

// translatedSummary renders a *nanollm.Response using only its safe scalar facts
// (status, JSON flag) plus a digest of the body — never the body bytes.
func translatedSummary(r *nanollm.Response) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{status:%d jsonBody:%v body:%s}", r.Status, r.BodyIsJSON, bodyDigest(r.Body))
}

// resultSummary renders an *AttemptResult for a failure diagnostic using only
// safe facts — outcome, attempted alias, statuses, the SAP flag, the decline
// reason, and size/hash digests of the payload fields — never the raw provider /
// translated / structured bytes or the assistant text.
func resultSummary(res *AttemptResult) string {
	if res == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"{outcome:%s alias:%q sap:%v providerStatus:%d providerBody:%s translated:%s structured:%s assistantText:%s decline:%q}",
		res.Outcome, res.AttemptedAlias, res.SAPInvoked, res.ProviderStatus,
		bodyDigest(res.ProviderBody), translatedSummary(res.Translated),
		bodyDigest(res.Structured), bodyDigest([]byte(res.AssistantText)), res.DeclineReason,
	)
}
