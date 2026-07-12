//go:build integration && nanollm_prebuilt && nanollm_integration

package dynamic

// De-BAML Phase 5.1 DYNAMIC OpenAI prepared-request differential.
//
// For every Phase-4a-admitted dynamic fixture this proves that nanollm's
// PreparedRequest (from Client.Prepare on the native 4a canonical body) matches
// the ACTUAL provider request BAML v0.223 builds through baml-rest's PATCHED
// runtime (dynclient with de-BAML disabled) — with NO network and NO send, and
// WITHOUT ever calling HTTPRequest / Do / DoStream / TranslateResponse or any
// baml-rest transport.
//
// Capture: a short-circuiting net/http RoundTripper snapshots, under its mutex and
// BEFORE returning a canned response, the FULL outbound request — method,
// URL.String(), a deep copy of every header value, and the exact io.ReadAll body.
// The whole snapshot is reset per row; an incomplete capture is a hard failure,
// never a stale-data pass.
//
// Differential (scope "Exact field policy"), all without decoding the body:
//   - the native canonical body equals BAML's captured body byte-for-byte, then
//     nanollm's plan body equals BOTH (triple byte equality);
//   - method == POST on both and == prep.Method;
//   - URL == prep.URL and contains the fake base + /v1 + /chat/completions;
//   - semantic headers (a unique ASCII-lowercase map; duplicates/multi-values
//     rejected) match by set + value exactly, BAML's internal `baml-original-url`
//     transport header explicitly declined (../testutil);
//   - exactly one authorization each side, exact `Bearer <fake key>`, redacted in
//     diagnostics;
//   - nanollm's OWN emitted header order (Content-Type then Authorization) is
//     asserted independently — NEVER compared to BAML;
//   - prep.ResponseFormat == FormatJSON; plan meta resolves
//     alias/target/openai/ChatCompletion/non-stream with no transform and a zero
//     retry budget; the plan is unsigned (SignedAt/ExpiresAt nil, !Expired()).
//
// Runs in the SEPARATE, go.work-excluded nanollmprepare module; links the patched
// BAML runtime (dynclient/baml-patched) and nanollm's Rust FFI, and must never
// share a binary with the stock-BAML static leg (../static).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/planassert"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/testutil"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm/go"
)

// The literal auth/environment fence (scope "Concrete auth/environment fence"),
// aliased from the shared single source of truth in ../planassert. The SAME
// model/base/key drive the BAML dynamic ClientRegistry and the paired nanollm
// ModelConfig; the alias is deliberately distinct from the target so the
// model-separation proof is meaningful (the alias never appears in the JSON body).
// The `.invalid` base is non-routable: the capture transport returns locally, so
// nothing is ever dialed.
const (
	fenceModel   = planassert.FenceModel
	fenceBaseURL = planassert.FenceBaseURL
	fenceAPIKey  = planassert.FenceAPIKey
	fenceAlias   = planassert.FenceAlias

	wantURL = planassert.WantURL
)

func sp(s string) *string { return &s }

func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

func richSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("count", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

// dynFixture is one Phase-4a-admitted dynamic case: ordered input messages plus
// the output schema. It reuses the exact admitted text-only surface of the 4a
// dynamic body oracle (internal/nativebody/oracle_integration_test.go): single/
// ordered system/user/assistant messages, output_format rendered into text, and
// the serde_json escaping case.
type dynFixture struct {
	name     string
	messages []nativeprompt.Message
	schema   *bamlutils.DynamicOutputSchema
}

func dynFixtures() []dynFixture {
	return []dynFixture{
		{
			name:     "single_user_message",
			messages: []nativeprompt.Message{{Role: "user", Content: sp("What is 2+2?")}},
			schema:   simpleSchema(),
		},
		{
			name: "system_user_ordered",
			messages: []nativeprompt.Message{
				{Role: "system", Content: sp("You are a helpful assistant.")},
				{Role: "user", Content: sp("What is 2+2?")},
			},
			schema: simpleSchema(),
		},
		{
			name: "system_user_assistant_ordered",
			messages: []nativeprompt.Message{
				{Role: "system", Content: sp("You are concise.")},
				{Role: "user", Content: sp("Tell me about weather.")},
				{Role: "assistant", Content: sp("Here are 3 facts.")},
			},
			schema: simpleSchema(),
		},
		{
			name: "output_format_rendered_into_text",
			messages: []nativeprompt.Message{
				{Role: "system", Parts: []nativeprompt.ContentPart{{OutputFormat: true}}},
				{Role: "user", Content: sp("Produce the structured output.")},
			},
			schema: richSchema(),
		},
		{
			name: "special_characters_escaping",
			messages: []nativeprompt.Message{
				{Role: "user", Content: sp("html <b>&</b>, quote \"q\", slash a/b, unicode café ☕")},
			},
			schema: simpleSchema(),
		},
	}
}

// dynFixtureByName returns the admitted fixture with the given name, failing
// loudly if it is absent. Negative controls select rows by NAME (not index) so a
// reorder/extend of dynFixtures() can never silently rebind a control to a
// different fixture.
func dynFixtureByName(t *testing.T, name string) dynFixture {
	t.Helper()
	for _, f := range dynFixtures() {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no dynamic fixture named %q", name)
	return dynFixture{}
}

// capturedRequest is the FULL snapshot of one intercepted outbound request. The
// present flag distinguishes "no capture yet" from a captured-but-empty field so
// an incomplete capture fails loudly. err records a body read/close failure: a
// mid-stream read error yields a PARTIAL non-empty body that would otherwise
// satisfy the length-based completeness guard, so the per-row guard hard-fails on
// a non-nil err rather than publish a corrupt capture.
type capturedRequest struct {
	method  string
	url     string
	header  http.Header
	body    []byte
	present bool
	err     error
}

// captureRoundTripper records the whole outbound request (method, URL, a deep
// copy of every header value, and the exact body bytes) under its mutex before
// returning a canned OpenAI response — no network, no send. The entire snapshot
// is reset per row.
type captureRoundTripper struct {
	mu   sync.Mutex
	snap capturedRequest
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	var readErr, closeErr error
	if req.Body != nil {
		body, readErr = io.ReadAll(req.Body)
		closeErr = req.Body.Close()
	}
	// Preserve the FIRST error (the read error, if any) — it is the more
	// informative one for a mid-stream failure.
	captureErr := readErr
	if captureErr == nil {
		captureErr = closeErr
	}

	// Deep copy the header: copy the map AND every value slice so a later mutation
	// of req.Header (or slice aliasing) can never alter the snapshot.
	hdr := make(http.Header, len(req.Header))
	for k, vs := range req.Header {
		cp := make([]string, len(vs))
		copy(cp, vs)
		hdr[k] = cp
	}

	c.mu.Lock()
	c.snap = capturedRequest{
		method:  req.Method,
		url:     req.URL.String(),
		header:  hdr,
		body:    body,
		present: true,
		err:     captureErr,
	}
	c.mu.Unlock()

	// Fail closed on a body read/close error: never publish a partial capture as a
	// canned success. Surfacing the error to BAML's Do makes the row fail loudly,
	// AND the per-row completeness guard independently t.Fatals on the recorded
	// snap.err — so a partial non-empty body can never masquerade as complete.
	if captureErr != nil {
		return nil, captureErr
	}

	const canned = `{"id":"chatcmpl-p5-dynamic","object":"chat.completion","created":0,` +
		`"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(canned)),
		Request:    req,
	}, nil
}

func (c *captureRoundTripper) take() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

// reset clears the ENTIRE snapshot (not just the body) so a row whose BAML build
// fails before send yields an absent capture — caught by the hard-fail guard —
// instead of surfacing the previous row's request as a false pass.
func (c *captureRoundTripper) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = capturedRequest{}
}

// headerNames returns the sorted header NAMES only — never any value — so a
// capture-integrity diagnostic can describe what was captured without printing
// the fake bearer token (or any other header value) on a failure path.
func headerNames(h http.Header) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// newOracleClient builds a dynclient bound to the explicit capture *http.Client
// with de-BAML disabled, so BAML itself renders the prompt and builds the
// provider request, intercepted locally before any network I/O.
func newOracleClient(t *testing.T, capture *captureRoundTripper) *dynclient.Client {
	t.Helper()
	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(&http.Client{Transport: capture}),
		dynclient.WithDeBAML(false),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	return client
}

// oracleRegistry builds a client_registry with the literal fence model, base URL,
// and API key — the exact same values fed to the paired nanollm config.
func oracleRegistry() *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*dynclient.ClientProperty{{
			Name:     "TestClient",
			Provider: "openai",
			Options: map[string]any{
				"model":    fenceModel,
				"base_url": fenceBaseURL,
				"api_key":  fenceAPIKey,
			},
		}},
	}
}

// toDynMessages / toDynPart convert the native fixture messages into dynclient
// request messages so ONE fixture drives both legs. The admitted surface is
// text-only (plus output_format), so no media conversion is needed.
func toDynMessages(msgs []nativeprompt.Message) []dynclient.Message {
	out := make([]dynclient.Message, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		dm := dynclient.Message{Role: m.Role}
		if len(m.Parts) > 0 {
			for j := range m.Parts {
				dm.PartsContent = append(dm.PartsContent, toDynPart(&m.Parts[j]))
			}
		} else if m.Content != nil {
			dm.TextContent = m.Content
		}
		out = append(out, dm)
	}
	return out
}

func toDynPart(p *nativeprompt.ContentPart) dynclient.ContentPart {
	switch {
	case p.Text != nil:
		return dynclient.ContentPart{Type: "text", Text: p.Text}
	case p.OutputFormat:
		return dynclient.ContentPart{Type: "output_format"}
	}
	return dynclient.ContentPart{}
}

// dynRow is the assembled evidence for one fixture: the native canonical body,
// BAML's full captured request, nanollm's plan, and both neutral snapshots.
type dynRow struct {
	native   *nativebody.CanonicalBody
	captured capturedRequest
	prep     *nanollm.PreparedRequest
	bamlSnap testutil.Snapshot
	prepSnap testutil.Snapshot
}

// captureAndPrepare drives all three moving parts for one fixture with the given
// nanollm client (which fixes the configured target) and returns the assembled
// row. It fails closed on an incomplete capture, a declined native build, a
// duplicate/multi-value header on either side, or a Prepare error — it does NOT
// assert cross-leg parity (callers do).
func captureAndPrepare(t *testing.T, client *dynclient.Client, capture *captureRoundTripper, nano *nanollm.Client, tc dynFixture) dynRow {
	t.Helper()

	// --- BAML dynamic leg: reset, then capture the full request (no send). ---
	capture.reset()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:       toDynMessages(tc.messages),
		ClientRegistry: oracleRegistry(),
		OutputSchema:   tc.schema,
	}); err != nil {
		// A response-parse error after capture is diagnostic (the canned body does
		// not match the schema); the request is captured before the response is read.
		t.Logf("DynamicCall returned (response parse may fail on canned body; request still captured): %v", err)
	}
	captured := capture.take()
	// A body read/close error yields a PARTIAL body: fail closed FIRST so a
	// partial non-empty body can never satisfy the length guard below.
	if captured.err != nil {
		t.Fatalf("capture read/close failed for %q: %v", tc.name, captured.err)
	}
	// Incomplete capture is a HARD failure — never a stale-data pass. The
	// diagnostic prints ONLY safe capture metadata (presence, method, URL, body
	// length, header NAMES + count) — never a header VALUE, so the fake bearer
	// token can never leak on this failure path.
	if !captured.present || captured.method == "" || captured.url == "" ||
		len(captured.body) == 0 || len(captured.header) == 0 {
		t.Fatalf("incomplete BAML capture for %q: present=%v method=%q url=%q body_len=%d header_names=%v",
			tc.name, captured.present, captured.method, captured.url, len(captured.body), headerNames(captured.header))
	}

	// --- Native leg: render + normalize client + build the canonical body. ---
	rendered, err := nativeprompt.Render(tc.messages, tc.schema)
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	intent, err := nativebody.NormalizeDynamicClient(oracleRegistry(), fenceAlias, false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	native, err := nativebody.BuildOpenAIChat(rendered, intent)
	if err != nil {
		t.Fatalf("BuildOpenAIChat declined a 4a-admitted case: %v", err)
	}

	// --- nanollm leg: Prepare on the native raw body (no send). ---
	prep, err := nano.Prepare(nanollm.Request{
		Model:  fenceAlias,
		Body:   native.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if err != nil {
		t.Fatalf("Prepare rejected a 4a-admitted body: %v", err)
	}

	bamlSnap, err := testutil.NewSnapshot(captured.method, captured.url,
		testutil.PairsFromMultiMap(captured.header), captured.body)
	if err != nil {
		t.Fatalf("BAML capture has duplicate/multi-value headers: %v", err)
	}
	prepSnap, err := testutil.NewSnapshot(prep.Method, prep.URL, prep.Headers, prep.Body)
	if err != nil {
		t.Fatalf("nanollm plan has duplicate/multi-value headers: %v", err)
	}

	return dynRow{native: native, captured: captured, prep: prep, bamlSnap: bamlSnap, prepSnap: prepSnap}
}

// TestDynamicPreparedRequestParity is the core dynamic differential.
func TestDynamicPreparedRequestParity(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	for _, tc := range dynFixtures() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			row := captureAndPrepare(t, client, capture, nano, tc)

			// (1) Raw body: native == BAML captured, then nanollm plan == both.
			// bytes.Equal on the raw []byte with NO textual conversion before the
			// assertion (byte-exact for the admitted surface; no JSON decode).
			if !bytes.Equal(row.native.Bytes(), row.captured.body) {
				t.Errorf("native body != BAML captured body\n--- native ---\n%s\n--- BAML ---\n%s",
					row.native.Bytes(), row.captured.body)
			}
			if !bytes.Equal(row.prep.Body, row.native.Bytes()) {
				t.Errorf("plan body != native body\n--- plan ---\n%s\n--- native ---\n%s",
					row.prep.Body, row.native.Bytes())
			}
			if !bytes.Equal(row.prep.Body, row.captured.body) {
				t.Errorf("plan body != BAML captured body")
			}

			// (2) Method / URL exact.
			if row.prep.Method != http.MethodPost || row.captured.method != http.MethodPost {
				t.Errorf("method: baml=%q nanollm=%q, want POST", row.captured.method, row.prep.Method)
			}
			if row.prep.URL != wantURL || row.captured.url != wantURL {
				t.Errorf("url: baml=%q nanollm=%q, want %q", row.captured.url, row.prep.URL, wantURL)
			}
			planassert.AssertFenceURL(t, row.prep.URL)

			// (3) Semantic headers + method + url + body via the shared comparator.
			if diffs := testutil.Diff(row.bamlSnap, row.prepSnap); len(diffs) != 0 {
				t.Errorf("prepared-request differential mismatch:\n%s", strings.Join(diffs, "\n"))
			}

			// (4) Auth: exactly one authorization each side, exact fake value (redacted).
			planassert.AssertAuth(t, row.bamlSnap.Headers, row.prepSnap.Headers)

			// (5) nanollm's OWN emitted header order — asserted independently, NEVER
			// compared to BAML (the BAML order/casing claim is explicitly declined).
			planassert.AssertNanollmHeaderOrder(t, row.prep.Headers)

			// (6) Response format + plan meta + unsigned-plan invariants.
			planassert.AssertResponseFormat(t, row.prep)
			planassert.AssertMeta(t, row.prep, fenceModel)
			planassert.AssertUnsigned(t, row.prep)
		})
	}
}

// --- controls: each must FAIL as a negative to keep the positives meaningful. ---

// TestDynamicCaptureResetControl proves reset() clears the WHOLE snapshot so a
// later pre-send failure cannot surface stale data as a green row.
func TestDynamicCaptureResetControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "single_user_message"))
	if !row.captured.present {
		t.Fatal("precondition: a row must have been captured")
	}
	capture.reset()
	// Diagnose ONLY safe metadata (never a header VALUE) so a reset regression
	// that leaves the prior row's Authorization behind cannot leak it here.
	if got := capture.take(); got.present || got.method != "" || got.url != "" || got.body != nil || got.header != nil || got.err != nil {
		t.Fatalf("reset() left a partial snapshot: present=%v method=%q url=%q body_len=%d header_names=%v err=%v",
			got.present, got.method, got.url, len(got.body), headerNames(got.header), got.err)
	}
}

// erroringBody is a request body whose Read returns some bytes together with an
// error, modelling a body read that fails mid-stream: io.ReadAll yields a
// PARTIAL, non-empty []byte plus the error — exactly the case that must NOT be
// published as a complete capture.
type erroringBody struct{}

func (erroringBody) Read(p []byte) (int, error) {
	return copy(p, []byte("{partial")), io.ErrUnexpectedEOF
}
func (erroringBody) Close() error { return nil }

// TestDynamicCaptureReadErrorControl proves the fail-closed capture path: a body
// read that errors mid-stream (a partial non-empty body) must NOT publish a
// "complete" capture. RoundTrip surfaces the error (no canned response) and
// records it on the snapshot, which is exactly what the per-row completeness
// guard t.Fatals on — so a partial read hard-fails instead of degrading into a
// misleading downstream body parity mismatch.
func TestDynamicCaptureReadErrorControl(t *testing.T) {
	rt := &captureRoundTripper{}
	req, err := http.NewRequest(http.MethodPost, wantURL, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	req.Body = erroringBody{}

	resp, rtErr := rt.RoundTrip(req)
	if rtErr == nil {
		t.Fatal("RoundTrip must surface a body read error, got nil (a partial read would be published silently)")
	}
	if resp != nil {
		t.Errorf("RoundTrip must not return a canned response after a read error, got a %d response", resp.StatusCode)
	}
	// The recorded capture error is precisely the condition the completeness guard
	// in captureAndPrepare t.Fatals on ("capture read/close failed").
	got := rt.take()
	if got.err == nil {
		t.Fatal("a body read error must be recorded on the snapshot so the completeness guard hard-fails")
	}
	if len(got.body) == 0 {
		t.Fatal("precondition: the induced read must have produced a PARTIAL non-empty body (else the length guard, not err, would catch it)")
	}
}

// TestDynamicBodyByteMutationControl proves the body comparison is byte-sensitive:
// a single-byte mutation of the native body breaks equality with BAML's body.
func TestDynamicBodyByteMutationControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "system_user_ordered"))
	mutated := row.native.Bytes()
	mutated[len(mutated)/2] ^= 0x20
	if bytes.Equal(mutated, row.captured.body) {
		t.Fatal("a one-byte body mutation was NOT detected — the differential is not byte-sensitive")
	}
}

// TestDynamicURLMutationControl proves a URL divergence is caught by the comparator.
func TestDynamicURLMutationControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "single_user_message"))
	bad := row.prepSnap
	bad.URL = row.prepSnap.URL + "/extra"
	if diffs := testutil.Diff(row.bamlSnap, bad); len(diffs) == 0 {
		t.Fatal("a mutated URL was NOT detected by the differential")
	}
}

// TestDynamicAuthorizationMutationControl proves an authorization divergence is
// caught AND that the secret value never leaks into diagnostics.
func TestDynamicAuthorizationMutationControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "single_user_message"))
	bad := testutil.Snapshot{Method: row.prepSnap.Method, URL: row.prepSnap.URL, Body: row.prepSnap.Body,
		Headers: map[string]string{}}
	for k, v := range row.prepSnap.Headers {
		bad.Headers[k] = v
	}
	bad.Headers["authorization"] = "Bearer tampered-key"
	diffs := testutil.Diff(row.bamlSnap, bad)
	if len(diffs) == 0 {
		t.Fatal("a mutated authorization value was NOT detected")
	}
	joined := strings.Join(diffs, "\n")
	if strings.Contains(joined, "tampered-key") || strings.Contains(joined, fenceAPIKey) {
		t.Fatalf("authorization value leaked into diagnostics: %q", joined)
	}
}

// TestDynamicDuplicateHeaderControl proves the comparator REJECTS a duplicate
// nanollm header rather than silently merging it.
func TestDynamicDuplicateHeaderControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "single_user_message"))
	dup := append(append([][2]string(nil), row.prep.Headers...), [2]string{"Authorization", "Bearer second"})
	if _, err := testutil.NewSnapshot(row.prep.Method, row.prep.URL, dup, row.prep.Body); err == nil {
		t.Fatal("a duplicate nanollm header was NOT rejected by the comparator")
	}
}

// TestDynamicTargetRewriteControl proves a mismatched configured target is a
// negative control: nanollm rewrites the top-level JSON model to the configured
// target, so the plan body diverges from BAML's captured body.
func TestDynamicTargetRewriteControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	// A nanollm client whose configured target is NOT the body's model.
	rw := planassert.NewPrepareClient(t, "rewrite-target-model")

	row := captureAndPrepare(t, client, capture, rw, dynFixtureByName(t, "single_user_message"))
	if bytes.Equal(row.prep.Body, row.captured.body) {
		t.Fatal("a rewritten target did NOT change the plan body — the body differential is not sensitive")
	}
	if !strings.Contains(string(row.prep.Body), `"model":"rewrite-target-model"`) {
		t.Errorf("expected nanollm to rewrite the JSON model to the configured target, got %s", row.prep.Body)
	}
}

// TestDynamicUnsignedExpiryControl proves the unsigned-plan assertion is
// meaningful: the real openai plan carries no expiry, while a synthetic plan with
// a past ExpiresAt reports Expired()==true.
func TestDynamicUnsignedExpiryControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)
	nano := planassert.NewPrepareClient(t, fenceModel)

	row := captureAndPrepare(t, client, capture, nano, dynFixtureByName(t, "single_user_message"))
	planassert.AssertUnsigned(t, row.prep)

	past := time.Now().Add(-time.Hour)
	expired := &nanollm.PreparedRequest{Meta: nanollm.PreparedMeta{ExpiresAt: &past}}
	if !expired.Expired() {
		t.Fatal("a plan with a past ExpiresAt must report Expired()==true — the unsigned assertion is not meaningful")
	}
}
