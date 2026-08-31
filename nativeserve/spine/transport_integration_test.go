//go:build nanollm_integration

package spine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
