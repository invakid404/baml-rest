//go:build nanollm_integration

package spine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// frozenV0223Request is the committed BAML v0.223 request golden (generated + kept
// current by internal/nativeprompt/staticoracle.TestStaticSpineRequestGoldenIsCurrentV0223,
// which reads it straight from the BAML CFFI). spine cannot link BAML, so the oracle
// is frozen there and asserted here.
type frozenV0223Request struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Body    string      `json:"body"`
	Headers [][2]string `json:"headers"`
}

const frozenGoldenInput = "arbitrary json"

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <repo>/nativeserve/spine/transport_integration_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFrozenGolden(t *testing.T) frozenV0223Request {
	t.Helper()
	path := filepath.Join(repoRootFromTest(t), "nativeserve", "spine", "testdata", "staticrecursivealiasjson_v0223_request.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frozen v0.223 golden (regenerate: go test -tags integration ./internal/nativeprompt/staticoracle/ -run TestStaticSpineRequestGoldenIsCurrentV0223 -update-spine-request-golden): %v", err)
	}
	var g frozenV0223Request
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	return g
}

// staticOracleProject builds the whole-project descriptor from the checked-in
// static_oracle baml_src — the SAME method (StaticRecursiveAliasJSON) + client
// (StaticOracleClient: fake literal creds, .invalid base URL) the frozen BAML v0.223
// golden was generated from — so the spine's nanollm plan is comparable to it.
func staticOracleProject(t *testing.T) projectdescriptor.Project {
	t.Helper()
	dir := filepath.Join(repoRootFromTest(t), "internal", "nativeprompt", "testdata", "static_oracle", "baml_src")
	sources := map[string]string{}
	for _, name := range []string{"clients.baml", "types.baml", "functions.baml", "generators.baml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read static_oracle %s: %v", name, err)
		}
		sources[name] = string(b)
	}
	proj, err := nativespine.BuildFromSource(sources)
	if err != nil {
		t.Fatalf("BuildFromSource(static_oracle): %v", err)
	}
	return proj
}

// TestOneSendExactTransportBytes proves the exact-transport contract BYTE-FOR-BYTE
// (Codex review finding 4): an admitted call opens EXACTLY one provider socket and the
// wire request equals the prepared nanollm plan exactly — same method, full URL
// (host + path + query), byte-identical body, and the exact semantic header set
// (name+values, transport-only headers exempt). The prepared plan is the reused
// nanollm exact plan, proven byte-equivalent to BAML v0.223 by the nanollmprepare
// provideroracle/staticoracle differentials this lane reuses unchanged; the executor
// forwarding it verbatim is what justifies omitting the runtime BAML plan-compare.
//
// White-box: it captures the executor's OWN prepared plan through the admission seam
// (no duplicated plan construction), so a wire ≠ plan drift fails the test.
func TestOneSendExactTransportBytes(t *testing.T) {
	type wireReq struct {
		method     string
		requestURI string
		host       string
		header     http.Header
		body       []byte
	}
	var wire wireReq
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := io.ReadAll(r.Body)
		wire = wireReq{method: r.Method, requestURI: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"k\":1}"}}]}`))
	}))
	defer srv.Close()

	proj := jsonAliasProjectAt(t, srv.URL+"/v1")
	e, err := NewUnaryExecutor(proj, []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}

	// Capture the executor's own prepared plan (no separate/duplicated construction).
	var pMethod, pURL string
	var pHeaders [][2]string
	var pBody []byte
	inner := e.admitClaim
	e.admitClaim = func(ctx context.Context, in admission.StaticInput) (*admission.StaticClaim, error) {
		c, cerr := inner(ctx, in)
		if c != nil && c.Prepared != nil {
			pMethod, pURL, pHeaders, pBody = c.Prepared.Method, c.Prepared.URL, c.Prepared.Headers, c.Prepared.Body
		}
		return c, cerr
	}

	res := e.Call(context.Background(), "StaticRecursiveAliasJSON", &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if hits != 1 {
		t.Fatalf("provider request count = %d, want exactly 1", hits)
	}
	if pMethod == "" || pURL == "" {
		t.Fatal("prepared plan was not captured")
	}

	// method
	if wire.method != pMethod {
		t.Errorf("method: wire=%q plan=%q", wire.method, pMethod)
	}
	// full URL: host + path + query
	u, perr := url.Parse(pURL)
	if perr != nil {
		t.Fatalf("parse plan URL %q: %v", pURL, perr)
	}
	wantTarget := u.EscapedPath()
	if u.RawQuery != "" {
		wantTarget += "?" + u.RawQuery
	}
	if wire.requestURI != wantTarget {
		t.Errorf("request target: wire=%q plan=%q", wire.requestURI, wantTarget)
	}
	if wire.host != u.Host {
		t.Errorf("effective host: wire=%q plan=%q", wire.host, u.Host)
	}
	// byte-identical body
	if !bytes.Equal(wire.body, pBody) {
		t.Errorf("body not byte-identical:\n wire=%s\n plan=%s", wire.body, pBody)
	}
	// exact semantic header set (name+values), transport-only headers exempt
	assertHeaderMultiset(t, pHeaders, wire.header)
}

// transportOnlyHeaders are wire-only headers net/http adds that the plan never carries.
var transportOnlyHeaders = map[string]bool{
	"user-agent": true, "content-length": true, "accept-encoding": true, "connection": true, "host": true,
}

// assertHeaderMultiset checks every plan header (grouped by lowercase name, values in
// order) is present on the wire with the same values, and the wire carries no extra
// non-transport header.
func assertHeaderMultiset(t *testing.T, plan [][2]string, wire http.Header) {
	t.Helper()
	want := map[string][]string{}
	for _, kv := range plan {
		n := strings.ToLower(kv[0])
		want[n] = append(want[n], kv[1])
	}
	got := map[string][]string{}
	for n, vs := range wire {
		got[strings.ToLower(n)] = append([]string(nil), vs...)
	}
	for n, vs := range want {
		if !slices.Equal(got[n], vs) {
			t.Errorf("header %q: wire=%v plan=%v", n, got[n], vs)
		}
	}
	for n, vs := range got {
		if _, ok := want[n]; ok || transportOnlyHeaders[n] {
			continue
		}
		t.Errorf("wire carries unexpected header %q=%v not in the plan", n, vs)
	}
}

// jsonAliasProjectAt builds the JSON-alias project with base_url pointed at baseURL
// (white-box helper; mirrors the black-box injectBaseURL).
func jsonAliasProjectAt(t *testing.T, baseURL string) projectdescriptor.Project {
	t.Helper()
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	clients := make([]projectdescriptor.Client, len(proj.Clients))
	copy(clients, proj.Clients)
	for ci := range clients {
		opts := make([]promptdescriptor.ClientOption, len(clients[ci].Config.TransportOptions))
		copy(opts, clients[ci].Config.TransportOptions)
		for oi := range opts {
			if opts[oi].Key == "base_url" {
				opts[oi].Value = promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: baseURL}
			}
		}
		clients[ci].Config.TransportOptions = opts
	}
	proj.Clients = clients
	return proj
}

// injectBaseInto returns proj with every client's base_url transport option set to
// baseURL (white-box helper).
func injectBaseInto(proj projectdescriptor.Project, baseURL string) projectdescriptor.Project {
	clients := make([]projectdescriptor.Client, len(proj.Clients))
	copy(clients, proj.Clients)
	for ci := range clients {
		opts := make([]promptdescriptor.ClientOption, len(clients[ci].Config.TransportOptions))
		copy(opts, clients[ci].Config.TransportOptions)
		for oi := range opts {
			if opts[oi].Key == "base_url" {
				opts[oi].Value = promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: baseURL}
			}
		}
		clients[ci].Config.TransportOptions = opts
	}
	proj.Clients = clients
	return proj
}

// capturePlanNoSend builds the spine's OWN prepared nanollm plan for method+input via
// the admission seam and DECLINES before any socket (the golden's .invalid host is
// never contacted). It returns the plan's method/url/body/headers.
func capturePlanNoSend(t *testing.T, e *UnaryExecutor, method string, input any) (pm, pu, pb string, ph [][2]string) {
	t.Helper()
	inner := e.admitClaim
	e.admitClaim = func(ctx context.Context, in admission.StaticInput) (*admission.StaticClaim, error) {
		c, cerr := inner(ctx, in)
		if c != nil && c.Prepared != nil {
			pm, pu, pb = c.Prepared.Method, c.Prepared.URL, string(c.Prepared.Body)
			ph = append([][2]string(nil), c.Prepared.Headers...)
			c.Close()
			return nil, &admission.StaticDecline{Stage: "test", Reason: "plan_captured_no_send"}
		}
		return c, cerr
	}
	_ = e.Call(context.Background(), method, input)
	return pm, pu, pb, ph
}

// assertSemanticHeadersMatchGolden proves the plan's semantic header MULTISET equals the
// frozen v0.223 golden's EXACTLY — every name+value pair, INCLUDING duplicates, compared
// in BOTH directions, so an ADDITIONAL native header, a DUPLICATE plan header, or a
// MISSING header all fail (review-3 finding 2: the earlier subset check collapsed
// duplicates into a one-value map and iterated only the golden headers). Header name
// casing and order are HTTP-insignificant and not compared (the pairs are lower-cased and
// sorted); BAML's own transport-only baml-original-url is dropped from both sides —
// nanollm never emits it, and it is the one documented exemption. Proving the semantic
// header set EQUAL (not merely a superset of the golden) is what justifies omitting the
// runtime BAML plan-compare on the spine path.
func assertSemanticHeadersMatchGolden(t *testing.T, plan, golden [][2]string) {
	t.Helper()
	exempt := map[string]bool{"baml-original-url": true}
	canon := func(hs [][2]string) []string {
		out := make([]string, 0, len(hs))
		for _, kv := range hs {
			n := strings.ToLower(kv[0])
			if exempt[n] {
				continue
			}
			// NUL separates name from value so no name/value pair can alias another.
			out = append(out, n+"\x00"+kv[1])
		}
		slices.Sort(out)
		return out
	}
	got, want := canon(plan), canon(golden)
	if !slices.Equal(got, want) {
		t.Errorf("semantic header multiset differs from BAML v0.223 golden (extra / duplicate / missing header):\n--- plan ---\n%s\n--- v0.223 ---\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestSpineRequestMatchesFrozenV0223Oracle proves the spine's prepared request plan
// for StaticRecursiveAliasJSON equals the REAL frozen BAML v0.223 request byte-for-byte
// — method, full URL, byte-identical body (the rendered prompt + ctx.output_format),
// and the semantic header set — for the SAME method + input + client the golden was
// generated from (Codex review #2 finding 3). This is the non-self-referential v0.223
// evidence that justifies omitting the runtime BAML plan-compare: the plan is compared
// to BAML's actual output, not to the executor's own plan.
func TestSpineRequestMatchesFrozenV0223Oracle(t *testing.T) {
	golden := readFrozenGolden(t)
	e, err := NewUnaryExecutor(staticOracleProject(t), []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor(static_oracle): %v", err)
	}
	pm, pu, pb, ph := capturePlanNoSend(t, e, "StaticRecursiveAliasJSON", &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: frozenGoldenInput})
	if pm == "" {
		t.Fatal("spine plan was not captured")
	}
	if pm != golden.Method {
		t.Errorf("method: plan=%q golden(v0.223)=%q", pm, golden.Method)
	}
	if pu != golden.URL {
		t.Errorf("url: plan=%q golden(v0.223)=%q", pu, golden.URL)
	}
	if pb != golden.Body {
		t.Errorf("body not byte-identical to BAML v0.223:\n--- plan ---\n%s\n--- v0.223 ---\n%s", pb, golden.Body)
	}
	assertSemanticHeadersMatchGolden(t, ph, golden.Headers)
}

// TestSpineOneSendBodyIsV0223 proves the spine opens EXACTLY one socket and the body
// it puts on the wire is byte-identical to the frozen BAML v0.223 body (the base URL
// is pointed at a loopback for the send; the body does not depend on it).
func TestSpineOneSendBodyIsV0223(t *testing.T) {
	golden := readFrozenGolden(t)
	var wireBody []byte
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		wireBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"[1,2,3]"}}]}`))
	}))
	defer srv.Close()

	proj := injectBaseInto(staticOracleProject(t), srv.URL+"/v1")
	e, err := NewUnaryExecutor(proj, []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	res := e.Call(context.Background(), "StaticRecursiveAliasJSON", &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: frozenGoldenInput})
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if hits != 1 {
		t.Fatalf("provider request count = %d, want exactly 1", hits)
	}
	if !bytes.Equal(wireBody, []byte(golden.Body)) {
		t.Errorf("wire body not byte-identical to BAML v0.223:\n--- wire ---\n%s\n--- v0.223 ---\n%s", wireBody, golden.Body)
	}
}
