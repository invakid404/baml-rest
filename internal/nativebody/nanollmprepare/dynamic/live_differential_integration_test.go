//go:build integration && nanollm_integration

package dynamic

// De-BAML Phase 6 Slice 6c: the BAML-as-served LIVE-LOOPBACK differential — the
// core Phase 6 green bar.
//
// This EXTENDS the Phase-5 dynamic prepared-request oracle (this same package's
// prepared_request_integration_test.go) from a no-send capture into an actual
// two-leg loopback SEND. The P5 leg intercepts BAML's request at a
// short-circuiting RoundTripper and compares it to nanollm's PREPARED plan with
// no socket. Slice 6c instead stands up a real baml-rest-owned loopback capture
// server and runs BOTH legs against it, one physical request each:
//
//	Oracle leg (BAML-as-served — what production serves today):
//	  dynclient.DynamicCall + WithDeBAML(false)
//	    + fake ClientRegistry(base_url=127.0.0.1:<port>/v1, fake key, no retry)
//	  -> BAML v0.223 BuildRequest -> baml-rest llmhttp net/http SEND
//	  -> loopback capture server -> BAML extraction + (native-disabled) final parse
//
//	Native leg (the 6a+6b path):
//	  same fixture -> nativeprompt.Render -> nativebody.BuildOpenAIChat
//	  -> nanollm Prepare -> 6a exact one-attempt transport (llmhttp ExactExecutor)
//	  -> SAME loopback capture server
//	  -> nanollm TranslateResponse(prep.Meta.ModelAlias) -> OpenAI extraction
//	  -> internal/debaml.Parse (native SAP), reusing the 6b execute.RunAttempt adapter
//
// Both legs make EXACTLY ONE real net/http request over loopback (proven by the
// capture server's per-leg request counter). The comparison policy (scope
// "Comparison policy"):
//
//   - exactly one request per leg, no fallback/retry;
//   - exact method / request target (path+query) / effective host / raw body bytes;
//   - semantic header equality as a case-insensitive multimap (every value +
//     per-name value order), with the fake authorization asserted exactly and
//     redacted in diagnostics; global header-name ORDER/CASING is DECLINED;
//   - only named, justified exclusions: BAML's internal `baml-original-url` and
//     transport-generated framing fields neither planner controls;
//   - identical deterministic upstream status/body delivered to BOTH legs;
//   - the native translated OpenAI body + assistant text is the served content;
//   - final structured-output equality (incl. schema field order);
//   - negative controls (flag-off + mutated method/URL/header/body/response) fail
//     for the intended reason; non-2xx / invalid-2xx controls prove native SAP is
//     NOT invoked and no second request is made.
//
// Credential/egress fence: literal fake key/model/alias, a literal 127.0.0.1 base
// from the running server, nanollm UseProcessEnv:false / Env:nil / MaxRetries:0,
// both HTTP clients with Proxy:nil + a dial guard that rejects any non-loopback
// destination, and no secret values in failure logs. No network escapes loopback.
//
// Runs in the go.work-excluded nanollmprepare module, gated
// `integration && nanollm_integration` (BAML CFFI + nanollm). It reuses the 6b
// adapter (execute.RunAttempt) — the ONLY cross-package symbol — and changes NO
// serving behavior: nothing here is reachable from production routing.

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
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/testutil"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// liveCallTimeout is a generous per-leg context deadline; the live differential
// is not a timing test.
const liveCallTimeout = 60 * time.Second

func bptr(b bool) *bool { return &b }

// --- fixtures: reuse the P5-admitted dynamic fixtures + a deterministic OpenAI
// success whose assistant content BOTH final parsers cleanly claim ---

// liveFixture pairs a P5-admitted dynamic fixture with the exact assistant
// content the shared deterministic OpenAI 2xx carries. The content must be JSON
// that cleanly parses against the fixture's schema on BOTH legs (BAML's final
// parse and internal/debaml.Parse) so each leg reaches final structured output.
type liveFixture struct {
	dynFixture
	content string
}

// liveFixtures pairs every P5 fixture with schema-appropriate structured
// content. Selecting by NAME (not index) keeps a reorder/extend of dynFixtures()
// from silently rebinding content to the wrong schema.
func liveFixtures(t *testing.T) []liveFixture {
	t.Helper()
	contentByName := map[string]string{
		"single_user_message":              `{"answer":"ok"}`,
		"system_user_ordered":              `{"answer":"ok"}`,
		"system_user_assistant_ordered":    `{"answer":"ok"}`,
		"special_characters_escaping":      `{"answer":"ok"}`,
		"output_format_rendered_into_text": `{"answer":"ok","count":3}`,
	}
	out := make([]liveFixture, 0, len(contentByName))
	for _, f := range dynFixtures() {
		c, ok := contentByName[f.name]
		if !ok {
			t.Fatalf("no served content mapped for fixture %q", f.name)
		}
		out = append(out, liveFixture{dynFixture: f, content: c})
	}
	return out
}

// openAISuccess builds a deterministic OpenAI Chat Completions 2xx whose
// assistant content is exactly `content`. The SAME bytes are served to both legs.
func openAISuccess(content string) []byte {
	m := map[string]any{
		"id":      "chatcmpl-p6c-live",
		"object":  "chat.completion",
		"created": 0,
		"model":   fenceModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	b, err := stdjson.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// openAIError builds a deterministic OpenAI-style non-2xx error body.
func openAIError(message, typ string) []byte {
	m := map[string]any{"error": map[string]any{"message": message, "type": typ, "code": typ}}
	b, err := stdjson.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// --- baml-rest-owned loopback capture server ---
//
// This is the wire-fidelity + final-output oracle: it records the exact method /
// request target / effective Host / a DEEP COPY of every header value slice / the
// byte-exact body, plus a request counter, for EVERY request. Unlike go-mocklm
// (whose admin log records headers as map[string]string, collapsing repeats and
// order) it can prove repeated header names and per-name value order. Both legs
// hit the SAME server sequentially, so the per-leg count is len(recs) after each
// leg, and recs[0]/recs[1] are the oracle/native observations.

// recordedLive is one captured request. Header is a deep clone (http.Header is
// map[string][]string; Clone copies the map AND every value slice) so a later
// mutation of the request cannot alter the snapshot.
type recordedLive struct {
	Method     string
	RequestURI string
	Host       string
	Header     http.Header
	Body       []byte
}

// liveCaptureServer is a loopback httptest.Server (httptest binds 127.0.0.1) that
// records every request and replies with a single programmable deterministic
// fixture served IDENTICALLY to both legs.
type liveCaptureServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	recs       []recordedLive
	respStatus int
	respBody   []byte
}

func newLiveCaptureServer(t *testing.T) *liveCaptureServer {
	t.Helper()
	cs := &liveCaptureServer{t: t, respStatus: http.StatusOK}
	cs.srv = httptest.NewServer(http.HandlerFunc(cs.handle))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *liveCaptureServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// A client may abandon a read (never expected on this lane); record what
		// arrived rather than crashing the handler goroutine.
		body = append(body, []byte("<read-error>")...)
	}
	rec := recordedLive{
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

func (cs *liveCaptureServer) base() string { return cs.srv.URL }

func (cs *liveCaptureServer) setResponse(status int, body []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.respStatus = status
	cs.respBody = body
}

// count is the per-leg request counter: total requests observed so far. After
// the oracle leg it must be 1; after the native leg it must be 2.
func (cs *liveCaptureServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.recs)
}

// rec returns the i-th captured request, failing if it is absent.
func (cs *liveCaptureServer) rec(i int) recordedLive {
	cs.t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if i >= len(cs.recs) {
		cs.t.Fatalf("no captured request at index %d (have %d)", i, len(cs.recs))
	}
	return cs.recs[i]
}

// --- loopback dial guard (egress fence) ---

// isLoopbackLiveHost reports whether host is a loopback literal (or "localhost").
func isLoopbackLiveHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackDial refuses any non-loopback destination: a mistyped base fails here
// instead of leaving the machine. Shared by both legs' transports.
func loopbackDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("loopback guard: unparsable dial address %q: %w", addr, err)
	}
	if !isLoopbackLiveHost(host) {
		return nil, fmt.Errorf("loopback guard: refusing non-loopback dial to %q", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(host, port))
}

// loopbackOracleHTTPClient is the net/http client baml-rest's llmhttp uses on the
// ORACLE leg: Proxy nil (no proxy-auth injection) + the loopback dial guard. It
// is otherwise a stock client so the oracle mirrors baml-rest's production send
// behavior (this is BAML-as-served) rather than the exact lane's hardened one.
func loopbackOracleHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:       nil,
		DialContext: loopbackDial,
	}}
}

// liveExactExecutor is the NATIVE leg's 6a exact executor over a loopback-guarded
// transport mirroring the exact lane's transparency guarantees: Proxy nil,
// compression disabled (no injected Accept-Encoding / transparent decode), and
// keep-alives disabled (no stale-connection replay, so one RoundTrip is one
// server-visible request).
func liveExactExecutor() *llmhttp.ExactExecutor {
	return llmhttp.NewExactExecutor(&http.Transport{
		Proxy:              nil,
		DialContext:        loopbackDial,
		DisableCompression: true,
		DisableKeepAlives:  true,
	})
}

// --- native-parser spy ---

// liveParseSpy wraps a bamlutils.DeBAMLParseFunc and counts invocations. On the
// native leg it is the independent proof that provider outcomes (non-2xx /
// invalid-2xx) never reach SAP (calls == 0). On the ORACLE leg it is installed
// via WithDeBAMLParser to prove BAML-as-served (de-BAML false) NEVER reaches the
// native parser at all — on every fixture and every control.
type liveParseSpy struct {
	fn    bamlutils.DeBAMLParseFunc
	calls int
}

func (s *liveParseSpy) Parse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	s.calls++
	return s.fn(ctx, req)
}

// --- oracle + native leg wiring ---

// liveOracleRegistry builds the fake client_registry with the literal fence
// model + api key and the loopback base — the EXACT same values fed to the paired
// nanollm config, so the two legs' request bodies are byte-identical (as the P5
// no-send differential already proves).
func liveOracleRegistry(base string) *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*dynclient.ClientProperty{{
			Name:     "TestClient",
			Provider: "openai",
			Options: map[string]any{
				"model":    fenceModel,
				"base_url": base + "/v1",
				"api_key":  fenceAPIKey,
			},
		}},
	}
}

// newLiveOracleClient builds a dynclient bound to the loopback-guarded net/http
// client with de-BAML DISABLED, so BAML itself renders + builds + parses (this is
// BAML-as-served). The parser spy is installed but must never fire while de-BAML
// is off.
func newLiveOracleClient(t *testing.T, httpClient *http.Client, spy *liveParseSpy) *dynclient.Client {
	t.Helper()
	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(httpClient),
		dynclient.WithDeBAML(false),
		dynclient.WithDeBAMLParser(spy.Parse),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	return client
}

// newLiveNativeClient builds a nanollm v0.3.2 client with ONE openai alias bound
// to the loopback base, a distinct alias vs. target, a zero retry budget, and no
// process-env secret resolution.
func newLiveNativeClient(t *testing.T, base string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       fenceAlias,
			Model:      "openai/" + fenceModel,
			APIKey:     fenceAPIKey,
			BaseURL:    base + "/v1",
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// runOracleLeg performs the BAML-as-served send and returns the flattened
// structured output. PreserveSchemaOrder pins the public-contract field order so
// the final-output comparison can assert order, not just set membership.
func runOracleLeg(t *testing.T, client *dynclient.Client, base string, tc dynFixture) (stdjson.RawMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(tc.messages),
		ClientRegistry:      liveOracleRegistry(base),
		OutputSchema:        tc.schema,
		PreserveSchemaOrder: bptr(true),
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// runNativeLeg performs the 6a+6b native send, gated by the umbrella
// BAML_REST_USE_DEBAML switch (default-on; disabled only on an explicit falsy
// value) — never an ambient CI decision, and NO new flag. When the switch is off
// it returns ran=false having made ZERO native sends. The parseSpy is the
// independent no-SAP proof.
func runNativeLeg(t *testing.T, nano *nanollm.Client, base string, tc dynFixture) (res *execute.AttemptResult, spy *liveParseSpy, ran bool, err error) {
	t.Helper()
	if bamlutils.IsFalsyEnvValue(os.Getenv(bamlutils.EnvUseDeBAML)) {
		return nil, nil, false, nil
	}
	rendered, rerr := nativeprompt.Render(tc.messages, tc.schema)
	if rerr != nil {
		t.Fatalf("nativeprompt.Render: %v", rerr)
	}
	intent, ierr := nativebody.NormalizeDynamicClient(liveOracleRegistry(base), fenceAlias, false)
	if ierr != nil {
		t.Fatalf("NormalizeDynamicClient: %v", ierr)
	}
	nb, berr := nativebody.BuildOpenAIChat(rendered, intent)
	if berr != nil {
		t.Fatalf("BuildOpenAIChat declined a P5-admitted case: %v", berr)
	}
	prep, perr := nano.Prepare(nanollm.Request{
		Model:  fenceAlias,
		Body:   nb.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if perr != nil {
		t.Fatalf("Prepare rejected a P5-admitted body: %v", perr)
	}
	spy = &liveParseSpy{fn: debaml.Parse}
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, err = execute.RunAttempt(ctx, execute.AttemptConfig{
		Client:       nano,
		Prepared:     prep,
		Executor:     liveExactExecutor(),
		Parse:        spy.Parse,
		OutputSchema: tc.schema,
	})
	return res, spy, true, err
}

// --- wire parity comparator ---

// liveTransportOnlyHeaders are the request-header names the Go transport itself
// frames and that no planner controls — the ONLY permitted wire-only headers,
// excluded on BOTH sides. Empirically (a real loopback send of both legs): the
// BAML llmhttp client leaves compression on so its transport injects
// Accept-Encoding: gzip, while the native exact lane disables compression
// (injecting none) and disables keep-alives (framing Connection: close); Go moves
// the body length into Content-Length and stamps the default User-Agent on both.
// None is a semantic header either planner emits.
var liveTransportOnlyHeaders = map[string]bool{
	"accept-encoding": true,
	"content-length":  true,
	"user-agent":      true,
	"connection":      true,
}

// liveBAMLDeclinedHeaders are BAML-internal transport/routing headers Phase 5/6
// EXPLICITLY DECLINE to compare (scope). BAML v0.223 emits `baml-original-url`
// (the base URL, for its own downstream rewrite); nanollm never emits it. It is
// excluded from the ORACLE side ONLY — if nanollm ever emitted it, it would
// surface as a native-only header and correctly FAIL parity. Any OTHER unexpected
// BAML header stays in the semantic set and fails the test (fail closed). A NEW
// exclusion here requires an explicit parity-deferral note + a control.
var liveBAMLDeclinedHeaders = map[string]bool{
	"baml-original-url": true,
}

// liveMultiMap folds an http.Header into a lowercase-keyed multimap, dropping the
// named transport-only headers (and, when dropDeclined, the BAML-declined ones).
// Repeated names and per-name value order are preserved — the properties the
// capture server exists to prove.
func liveMultiMap(h http.Header, dropDeclined bool) map[string][]string {
	out := map[string][]string{}
	for k, vs := range h {
		n := strings.ToLower(k)
		if liveTransportOnlyHeaders[n] {
			continue
		}
		if dropDeclined && liveBAMLDeclinedHeaders[n] {
			continue
		}
		out[n] = append(out[n], vs...)
	}
	return out
}

// redactVals redacts every non-allowlisted header value for diagnostics.
func redactVals(name string, vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = testutil.RedactValue(name, v)
	}
	return out
}

// liveWireDiffs is the core wire differential between the two live legs, returned
// as a sorted list of REDACTED, human-readable differences (empty == parity):
// exact method, request target (path+query), effective host, byte-exact body,
// semantic header multimap equality (every value + per-name order), and the exact
// fake authorization on both sides. Global header-name order/casing is NOT
// compared. Every header value is redacted unless allowlisted, so a diagnostic
// never leaks the fake bearer token.
func liveWireDiffs(oracle, native recordedLive) []string {
	var d []string

	if oracle.Method != native.Method {
		d = append(d, fmt.Sprintf("method: oracle=%q native=%q", oracle.Method, native.Method))
	}
	if oracle.RequestURI != native.RequestURI {
		d = append(d, fmt.Sprintf("request target: oracle=%q native=%q", oracle.RequestURI, native.RequestURI))
	}
	if oracle.Host != native.Host {
		d = append(d, fmt.Sprintf("effective host: oracle=%q native=%q", oracle.Host, native.Host))
	}
	if !bytes.Equal(oracle.Body, native.Body) {
		d = append(d, fmt.Sprintf("raw body not byte-identical: oracle=%s native=%s",
			liveBodyDigest(oracle.Body), liveBodyDigest(native.Body)))
	}

	oSem := liveMultiMap(oracle.Header, true)  // oracle: drop baml-original-url
	nSem := liveMultiMap(native.Header, false) // native: nothing to decline
	for name, ov := range oSem {
		nv, ok := nSem[name]
		if !ok {
			d = append(d, fmt.Sprintf("header %q present on oracle only (values %v)", name, redactVals(name, ov)))
			continue
		}
		if !slices.Equal(ov, nv) {
			d = append(d, fmt.Sprintf("header %q values differ: oracle=%v native=%v",
				name, redactVals(name, ov), redactVals(name, nv)))
		}
	}
	for name, nv := range nSem {
		if _, ok := oSem[name]; !ok {
			d = append(d, fmt.Sprintf("header %q present on native only (values %v)", name, redactVals(name, nv)))
		}
	}

	// Exact fake authorization on both sides.
	wantAuth := "Bearer " + fenceAPIKey
	d = append(d, authExactDiffs("oracle", oSem, wantAuth)...)
	d = append(d, authExactDiffs("native", nSem, wantAuth)...)

	sort.Strings(d)
	return d
}

// assertLiveWireParity fails the test with every wire difference, and separately
// pins the request target to the OpenAI chat-completions endpoint.
func assertLiveWireParity(t *testing.T, oracle, native recordedLive) {
	t.Helper()
	if oracle.Method != http.MethodPost || native.Method != http.MethodPost {
		t.Errorf("method: oracle=%q native=%q, want POST", oracle.Method, native.Method)
	}
	if want := "/v1/chat/completions"; oracle.RequestURI != want {
		t.Errorf("request target = %q, want %q", oracle.RequestURI, want)
	}
	for _, s := range liveWireDiffs(oracle, native) {
		t.Error(s)
	}
}

func authExactDiffs(side string, sem map[string][]string, want string) []string {
	vs := sem["authorization"]
	if len(vs) != 1 {
		return []string{fmt.Sprintf("%s: want exactly one authorization header, got %d", side, len(vs))}
	}
	if vs[0] != want {
		return []string{fmt.Sprintf("%s authorization mismatch (redacted): got %s want %s",
			side, testutil.RedactValue("authorization", vs[0]), testutil.RedactValue("authorization", want))}
	}
	return nil
}

// --- final structured-output parity ---

// assertStructuredParity proves final structured-output equality between the two
// legs, both semantically (object-key order ignored) and — reordered to the
// schema's field order (the order the public dynamic response contract preserves
// under PreserveSchemaOrder) — byte-for-byte.
func assertStructuredParity(t *testing.T, schema *bamlutils.DynamicOutputSchema, oracle, native stdjson.RawMessage) {
	t.Helper()
	if !liveJSONSemEqual(t, oracle, native) {
		t.Errorf("final structured output differs semantically: oracle=%s native=%s",
			liveBodyDigest(oracle), liveBodyDigest(native))
	}
	oOrd, err := bamlutils.ReorderDynamicOutputBySchema(oracle, schema)
	if err != nil {
		t.Fatalf("reordering oracle output by schema: %v", err)
	}
	nOrd, err := bamlutils.ReorderDynamicOutputBySchema(native, schema)
	if err != nil {
		t.Fatalf("reordering native output by schema: %v", err)
	}
	if !bytes.Equal(oOrd, nOrd) {
		t.Errorf("final structured output field order differs under the schema contract: oracle=%s native=%s",
			liveBodyDigest(oOrd), liveBodyDigest(nOrd))
	}
}

// --- secret-safe diagnostics ---

// liveBodyDigest renders a payload as "<n>B sha256:<12hex>" — never raw bytes —
// so a CI failure log holds no request/response payload.
func liveBodyDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%dB sha256:%s", len(b), hex.EncodeToString(sum[:])[:12])
}

// liveJSONSemEqual reports whether a and b decode to structurally equal JSON. On
// a decode failure it reports only a digest of the offending payload.
func liveJSONSemEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := stdjson.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode oracle output (%s): %v", liveBodyDigest(a), err)
	}
	if err := stdjson.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode native output (%s): %v", liveBodyDigest(b), err)
	}
	return jsonDeepEqual(av, bv)
}

// jsonDeepEqual compares two decoded JSON values structurally (object key order
// ignored; numbers already normalized to float64 by encoding/json).
func jsonDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !jsonDeepEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// TestLiveDifferentialParity is the core Slice 6c live differential: for every
// P5-admitted fixture, both legs send exactly one real loopback request, the wire
// observations match, and both reach the same final structured output.
func TestLiveDifferentialParity(t *testing.T) {
	// Native leg default-on EXPLICITLY (umbrella flag), so ambient CI env cannot
	// change the result. The oracle leg is de-BAML false regardless.
	t.Setenv(bamlutils.EnvUseDeBAML, "1")

	for _, lc := range liveFixtures(t) {
		lc := lc
		t.Run(lc.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			served := openAISuccess(lc.content)
			server.setResponse(http.StatusOK, served)

			oracleSpy := &liveParseSpy{fn: debaml.Parse}
			oracle := newLiveOracleClient(t, loopbackOracleHTTPClient(), oracleSpy)
			nano := newLiveNativeClient(t, server.base())

			// --- Leg A: oracle (BAML-as-served) — exactly one request. ---
			oracleData, oerr := runOracleLeg(t, oracle, server.base(), lc.dynFixture)
			if oerr != nil {
				t.Fatalf("oracle DynamicCall failed on an admitted fixture: %v", oerr)
			}
			if got := server.count(); got != 1 {
				t.Fatalf("oracle leg made %d requests, want exactly 1 (no retry/fallback)", got)
			}
			oracleRec := server.rec(0)

			// --- Leg B: native (6a+6b) — exactly one more request. ---
			res, spy, ran, nerr := runNativeLeg(t, nano, server.base(), lc.dynFixture)
			if !ran {
				t.Fatal("native leg did not run (umbrella flag unexpectedly off)")
			}
			if nerr != nil {
				t.Fatalf("native RunAttempt failed on an admitted fixture: %v", nerr)
			}
			if got := server.count(); got != 2 {
				t.Fatalf("after the native leg the server saw %d total requests, want exactly 2", got)
			}
			nativeRec := server.rec(1)

			// (1) wire parity (method / target / host / body / semantic headers).
			assertLiveWireParity(t, oracleRec, nativeRec)

			// (2) both legs received the identical deterministic upstream 2xx body:
			// the native leg's RAW captured provider body is byte-exact the served
			// bytes (the exact executor returns the upstream body verbatim).
			if res.ProviderStatus != http.StatusOK {
				t.Errorf("native provider status = %d, want 200", res.ProviderStatus)
			}
			if !bytes.Equal(res.ProviderBody, served) {
				t.Errorf("native raw provider body != served upstream body: got %s want %s",
					liveBodyDigest(res.ProviderBody), liveBodyDigest(served))
			}

			// (3) native response side: structured output, attempted alias, the
			// translated OpenAI body + assistant text, SAP invoked once.
			if res.Outcome != execute.OutcomeStructured {
				t.Fatalf("native outcome = %s, want structured", res.Outcome)
			}
			if res.AttemptedAlias != fenceAlias {
				t.Errorf("native attempted alias = %q, want %q", res.AttemptedAlias, fenceAlias)
			}
			if !res.SAPInvoked || spy.calls != 1 {
				t.Errorf("native SAP invocation: SAPInvoked=%v calls=%d, want invoked once", res.SAPInvoked, spy.calls)
			}
			if res.Translated == nil || !res.Translated.BodyIsJSON {
				t.Fatalf("native translated response missing/!JSON")
			}
			// The translated OpenAI body carries the SAME response fields as the
			// served OpenAI 2xx — compared semantically (per the adapter contract;
			// the openai path passes the body through, so this also holds byte-for-
			// byte here). This is what catches a translation regression that keeps
			// the assistant text but drops/changes other OpenAI response fields —
			// asserted BEFORE the assistant-text check below.
			if !liveJSONSemEqual(t, res.Translated.Body, served) {
				t.Errorf("native translated OpenAI body != served OpenAI body: got %s want %s",
					liveBodyDigest(res.Translated.Body), liveBodyDigest(served))
			}
			if res.AssistantText != lc.content {
				t.Errorf("native assistant text mismatch (%s), want the served structured JSON", liveBodyDigest([]byte(res.AssistantText)))
			}

			// (4) final structured-output equality (semantic + schema field order).
			assertStructuredParity(t, lc.schema, oracleData, res.Structured)

			// (5) BAML-as-served (de-BAML false) NEVER touched the native parser.
			if oracleSpy.calls != 0 {
				t.Errorf("oracle native-parser spy fired %d times; BAML-as-served must never invoke native SAP", oracleSpy.calls)
			}
		})
	}
}
