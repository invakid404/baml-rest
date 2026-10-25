//go:build subprocess && nativeartifactproof

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/pool"
)

// The BOOTED STANDARD-ARTIFACT /call-with-raw DIFFERENTIAL, over BOTH public HTTP servers.
//
// It boots the SHIPPED serve-profile worker (the S3b static fixture artifact) twice — the
// umbrella flag OFF (stock BAML v0.223 end to end) and ON (the live-oracle spine lane) —
// and drives the exact-U1 method's PUBLIC `/call-with-raw` route through the production
// Fiber handler (Sonic JSON encoder) AND the production chi unary handler (Sonic stream
// encoder), against one deterministic loopback provider whose response is swapped per
// corpus row. Within each server it requires the COMPLETE HTTP response — status,
// Content-Type, and every body byte including the encoder's trailing framing, string
// escaping, and the omitted-when-empty reasoning field — to be identical across the flags,
// for success AND error rows. `/call` rides along as the regression control.
//
// Byte equality alone would also pass for a native lane that always handed the request back
// to BAML. So the ownership facts are read separately, per row, off the booted worker's own
// counters: on the flag-on leg every row opens exactly one native socket and the provider
// sees exactly one request, and the winner is native on every row whose response native
// reads identically (the reference parse where native declines/fails; a claimed failure on
// an error row). On the flag-off leg nothing native happens at all.

// rawProofProvider is a fixed-address loopback OpenAI provider whose unary response is
// swapped per row. The handler reads the current response under the mutex. A row with a
// raw writer takes over the connection and writes exactly those bytes before closing it —
// how a truncated body or a connection closed before the first response byte is served.
type rawProofProvider struct {
	srv   *httptest.Server
	calls atomic.Int64

	mu     sync.Mutex
	status int
	body   []byte
	raw    func(w *bufio.Writer)
}

func newRawProofProvider(t *testing.T, addr string) *rawProofProvider {
	t.Helper()
	p := &rawProofProvider{status: http.StatusOK}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		status, body, raw := p.status, p.body, p.raw
		p.mu.Unlock()
		if raw != nil {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "the proof provider cannot take over the connection", http.StatusInternalServerError)
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			raw(buf.Writer)
			_ = buf.Writer.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind the fixture's baked base_url %s: %v — a busy port is a lane fault, not a reason to report success", addr, err)
	}
	p.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	p.srv.Start()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *rawProofProvider) respond(row rawProofRow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.body, p.raw = row.status, []byte(row.body), row.raw
}

// rawProofRow is one provider response plus the request's reasoning preference and the
// winner the flag-on leg must record for it.
type rawProofRow struct {
	name             string
	status           int
	body             string
	includeReasoning bool
	// wantWinner: "native", "baml_parse_same_response" (native declined/failed, the
	// claimed lane served the reference), or "failure" (BAML itself fails on the response).
	wantWinner string
	// raw, when set, replaces status/body: the provider writes exactly these bytes on the
	// connection and closes it.
	raw func(w *bufio.Writer)
}

func rawProofMessage(msg map[string]any) string {
	b, _ := json.Marshal(map[string]any{"id": "raw-proof", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}}})
	return string(b)
}

func rawProofCorpus() []rawProofRow {
	content := func(c any) map[string]any { return map[string]any{"role": "assistant", "content": c} }
	ok := func(name string, c any) rawProofRow {
		return rawProofRow{name: name, status: http.StatusOK, body: rawProofMessage(content(c)), wantWinner: "native"}
	}
	reasoned := content(`{"a":1}`)
	reasoned["reasoning_content"] = "step 1\nstep \"2\"\tdone ☕ <b>"
	return []rawProofRow{
		ok("map", `{"weather":"sunny"}`),
		// Coercion: the served data is BAML's coerced value, raw stays the provider text.
		ok("float_to_int", `1.5`),
		ok("null_to_empty_list", `{"n":null,"z":1,"a":2}`),
		ok("escapes_unicode_html", "\n {\"k\":\"café \\\"q\\\" \\\\ <b>&amp;</b>   end\"}\t\n"),
		ok("multipart_text", []any{
			map[string]any{"type": "text", "text": `[1,`},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/x.png"}},
			map[string]any{"type": "text", "text": `"x",true]`},
		}),
		{name: "reasoning_opted_in", status: http.StatusOK, body: rawProofMessage(reasoned), includeReasoning: true, wantWinner: "native"},
		{name: "reasoning_not_opted_in", status: http.StatusOK, body: rawProofMessage(reasoned), includeReasoning: false, wantWinner: "native"},
		{name: "plain_text_decline", status: http.StatusOK, body: rawProofMessage(content("sunny today")), wantWinner: "baml_parse_same_response"},
		{name: "lone_surrogate", status: http.StatusOK, body: `{"choices":[{"message":{"role":"assistant","content":"{\"a\":\"\ud800\"}"}}]}`, wantWinner: "baml_parse_same_response"},
		{name: "refusal", status: http.StatusOK, body: rawProofMessage(map[string]any{"role": "assistant", "content": nil, "refusal": "no"}), wantWinner: "failure"},
		{name: "malformed_json", status: http.StatusOK, body: `{"choices":[`, wantWinner: "failure"},
		{name: "provider_429", status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`, wantWinner: "failure"},
	}
}

// rawProofTransportRows are sends that fail before a complete response exists — the
// ordinary transport's error envelope, not a response oracle, decides these bytes.
func rawProofTransportRows() []rawProofRow {
	return []rawProofRow{
		{name: "body_over_the_size_cap", status: http.StatusOK, body: strings.Repeat(" ", llmhttp.MaxResponseBodyBytes+1), wantWinner: "failure"},
		{name: "2xx_body_truncated", wantWinner: "failure", raw: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"choices\":[{\"message\":")
		}},
		{name: "non_2xx_body_truncated", wantWinner: "failure", raw: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"error\":{\"message\":")
		}},
		{name: "closed_before_the_first_response_byte", wantWinner: "failure", raw: func(*bufio.Writer) {}},
	}
}

// rawProofRows selects corpus rows by name, failing on an unknown name so a renamed row
// cannot silently drop out of a control.
func rawProofRows(t *testing.T, names ...string) []rawProofRow {
	t.Helper()
	byName := map[string]rawProofRow{}
	for _, r := range append(rawProofCorpus(), rawProofTransportRows()...) {
		byName[r.name] = r
	}
	out := make([]rawProofRow, 0, len(names))
	for _, n := range names {
		r, ok := byName[n]
		if !ok {
			t.Fatalf("no corpus row %q", n)
		}
		out = append(out, r)
	}
	return out
}

// rowAt returns the observation for the named row.
func rowAt(t *testing.T, rows []rawProofRow, obs []rawProofObservation, name string) rawProofObservation {
	t.Helper()
	for i, r := range rows {
		if r.name == name {
			return obs[i]
		}
	}
	t.Fatalf("no corpus row %q", name)
	return rawProofObservation{}
}

// rawProofResponse is one complete HTTP response as a client receives it.
type rawProofResponse struct {
	status      int
	contentType string
	body        string
}

// rawProofObservation is one row's response plus the booted worker's per-row deltas.
type rawProofObservation struct {
	resp             rawProofResponse
	providerRequests int64
	nativeSockets    float64
	winners          map[string]float64
	rawModeAttempts  float64
}

// rawProofMetrics reads the booted worker's static-surface winners, native sockets, and the
// call_with_raw serve-outcome total.
func rawProofMetrics(t *testing.T, p *pool.Pool) (winners map[string]float64, sockets, rawAttempts float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	winners = map[string]float64{}
	gathered := p.GatherWorkerMetrics(ctx)
	if len(gathered) == 0 {
		t.Fatal("no worker reported metrics; the artifact's own counters are what this proof reads")
	}
	for _, wm := range gathered {
		if wm.Err != nil {
			t.Fatalf("worker %d metrics: %v", wm.WorkerID, wm.Err)
		}
		for _, raw := range wm.MetricFamilies {
			var mf dto.MetricFamily
			if err := proto.Unmarshal(raw, &mf); err != nil {
				t.Fatalf("decode worker MetricFamily: %v", err)
			}
			for _, m := range mf.GetMetric() {
				labels := map[string]string{}
				for _, lp := range m.GetLabel() {
					labels[lp.GetName()] = lp.GetValue()
				}
				v := m.GetCounter().GetValue()
				switch mf.GetName() {
				case "baml_rest_debaml_winner_total":
					if labels["surface"] == "static_call" {
						winners[labels["winner"]] += v
					}
				case "baml_rest_debaml_native_sockets_total":
					sockets += v
				case "baml_rest_debaml_attempts_total":
					if labels["mode"] == "call_with_raw" {
						rawAttempts += v
					}
				}
			}
		}
	}
	return winners, sockets, rawAttempts
}

// rawProofLeg boots the static artifact under one flag state and drives every row through
// BOTH servers on the given route, returning the per-server, per-row observations.
func rawProofLeg(t *testing.T, provider *rawProofProvider, flagOn bool, mode bamlutils.StreamMode, rows []rawProofRow) map[string][]rawProofObservation {
	t.Helper()
	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", flagOn))
	t.Setenv(trustedclients.EnvVar, staticFixtureDeclaration(feV1RouteFingerprint))
	workerPool, err := pool.New(&pool.Config{
		WorkerPath:         staticFixtureBinary(t),
		PoolSize:           1,
		LogOutput:          io.Discard,
		WorkerStartTimeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New over the static-capable artifact: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = workerPool.Shutdown(ctx)
	}()

	route := "/call/" + staticFixtureJSONMethod
	if mode.NeedsRaw() {
		route = "/call-with-raw/" + staticFixtureJSONMethod
	}
	chiSrv := httptest.NewServer(makeChiCallHandler(workerPool, staticFixtureJSONMethod, mode))
	defer chiSrv.Close()
	app := fiber.New(fiberAppConfig())
	app.Post(route, makeFiberCallHandler(workerPool, staticFixtureJSONMethod, mode, zerolog.Nop()))

	servers := map[string]func(body string) rawProofResponse{
		"chi": func(body string) rawProofResponse {
			resp, err := http.Post(chiSrv.URL+route, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("chi POST %s: %v", route, err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("chi read: %v", err)
			}
			return rawProofResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(b)}
		},
		"fiber": func(body string) rawProofResponse {
			req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 60 * time.Second})
			if err != nil {
				t.Fatalf("fiber POST %s: %v", route, err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("fiber read: %v", err)
			}
			return rawProofResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(b)}
		},
	}

	out := map[string][]rawProofObservation{}
	for _, server := range []string{"chi", "fiber"} {
		for _, row := range rows {
			provider.respond(row)
			reqBody := `{"topic":"the deployed static route"}`
			if row.includeReasoning {
				reqBody = `{"topic":"the deployed static route","__baml_options__":{"include_reasoning":true}}`
			}
			w0, s0, a0 := rawProofMetrics(t, workerPool)
			before := provider.calls.Load()
			resp := servers[server](reqBody)
			w1, s1, a1 := rawProofMetrics(t, workerPool)
			deltas := map[string]float64{}
			for k, v := range w1 {
				if d := v - w0[k]; d != 0 {
					deltas[k] = d
				}
			}
			out[server] = append(out[server], rawProofObservation{
				resp:             resp,
				providerRequests: provider.calls.Load() - before,
				nativeSockets:    s1 - s0,
				winners:          deltas,
				rawModeAttempts:  a1 - a0,
			})
		}
	}
	return out
}

// requireStaticArtifact fails (never skips) when the lane did not supply the artifact.
func requireStaticArtifact(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(staticFixtureWorkerArtifactIDEnv)) == "" {
		t.Fatalf("%s is not set: this lane must BOOT a STATIC-CAPABLE artifact; a missing artifact is a lane misconfiguration, not a reason to report success", staticFixtureWorkerArtifactIDEnv)
	}
}

// TestBootedArtifactDefaultServesTheExactJSONStaticCallWithRaw is the headline proof.
func TestBootedArtifactDefaultServesTheExactJSONStaticCallWithRaw(t *testing.T) {
	requireStaticArtifact(t)
	provider := newRawProofProvider(t, staticFixtureLoopbackAddr)
	rows := append(rawProofCorpus(), rawProofTransportRows()...)
	stock := rawProofLeg(t, provider, false, bamlutils.StreamModeCallWithRaw, rows)
	native := rawProofLeg(t, provider, true, bamlutils.StreamModeCallWithRaw, rows)
	assertRawProofLegsMatch(t, rows, stock, native)

	// The encoders really are different: chi's stream encoder frames with a trailing
	// newline, Fiber's Marshal does not. A proof that compared the two servers to each other
	// (rather than each to itself across the flags) would be blind to that.
	chiMap, fiberMap := rowAt(t, rows, stock["chi"], "map"), rowAt(t, rows, stock["fiber"], "map")
	if chiMap.resp.body == fiberMap.resp.body || !strings.HasSuffix(chiMap.resp.body, "\n") {
		t.Errorf("chi %q vs fiber %q: the two-server proof is not exercising two encoders", chiMap.resp.body, fiberMap.resp.body)
	}
	// The envelope really is the raw envelope: data/raw present, reasoning only when opted in.
	for _, server := range []string{"chi", "fiber"} {
		var env map[string]json.RawMessage
		on := rowAt(t, rows, native[server], "reasoning_opted_in").resp.body
		if err := json.Unmarshal([]byte(on), &env); err != nil || env["reasoning"] == nil || env["raw"] == nil || env["data"] == nil {
			t.Errorf("%s: the reasoning row's envelope %q lacks data/raw/reasoning (%v)", server, on, err)
		}
		env = nil
		off := rowAt(t, rows, native[server], "reasoning_not_opted_in").resp.body
		if err := json.Unmarshal([]byte(off), &env); err != nil || env["reasoning"] != nil || env["raw"] == nil {
			t.Errorf("%s: the not-opted-in row's envelope %q must carry raw and omit reasoning (%v)", server, off, err)
		}
	}
}

// TestBootedArtifactCallWithRawTransportFailuresUnderNetHTTP re-runs the transport rows
// with the worker's llmhttp client forced to net/http (BAML_REST_HTTP_CLIENT=nethttp),
// the backend an https/HTTP-2 provider is served on. The two ordinary backends word
// these failures differently — fasthttp reads the body inside Do ("request failed"),
// net/http reports a body-read failure ("failed to read response body") and turns a
// truncated non-2xx into the provider error with the bytes it read — and the native lane
// must reproduce whichever one the worker is configured with.
func TestBootedArtifactCallWithRawTransportFailuresUnderNetHTTP(t *testing.T) {
	requireStaticArtifact(t)
	t.Setenv(llmhttp.EnvVarClientMode, "nethttp")
	provider := newRawProofProvider(t, staticFixtureLoopbackAddr)
	rows := append(rawProofRows(t, "map"), rawProofTransportRows()...)
	stock := rawProofLeg(t, provider, false, bamlutils.StreamModeCallWithRaw, rows)
	native := rawProofLeg(t, provider, true, bamlutils.StreamModeCallWithRaw, rows)
	assertRawProofLegsMatch(t, rows, stock, native)
	// The backend really is different: the truncated non-2xx is a provider error here.
	if body := rowAt(t, rows, stock["chi"], "non_2xx_body_truncated").resp.body; !strings.Contains(body, `"provider_error"`) {
		t.Errorf("under nethttp the truncated non-2xx should be the provider error with the bytes read; stock body %q", body)
	}
}

// assertRawProofLegsMatch is the per-row, per-server assertion set: the stock leg is
// stock BAML with zero native activity; the native leg owns exactly one native socket
// and one provider request with the row's winner and a call_with_raw serve outcome
// (asserted separately from the bytes); and within each server the complete HTTP
// response is the stock leg's.
func assertRawProofLegsMatch(t *testing.T, rows []rawProofRow, stock, native map[string][]rawProofObservation) {
	t.Helper()
	for _, server := range []string{"chi", "fiber"} {
		for i, row := range rows {
			s, n := stock[server][i], native[server][i]
			label := server + "/" + row.name
			// The stock leg is stock BAML: one BAML send, nothing native.
			if s.providerRequests != 1 || s.nativeSockets != 0 || len(s.winners) != 0 {
				t.Errorf("%s: stock leg requests=%d sockets=%v winners=%v, want 1 BAML send and zero native activity",
					label, s.providerRequests, s.nativeSockets, s.winners)
			}
			// OWNERSHIP on the flag-on leg, asserted separately from the bytes.
			if n.providerRequests != 1 || n.nativeSockets != 1 {
				t.Errorf("%s: native leg requests=%d sockets=%v, want exactly one native socket and one provider request",
					label, n.providerRequests, n.nativeSockets)
			}
			if len(n.winners) != 1 || n.winners[row.wantWinner] != 1 {
				t.Errorf("%s: native leg winners=%v, want exactly {%s:1}", label, n.winners, row.wantWinner)
			}
			if n.rawModeAttempts != 1 {
				t.Errorf("%s: attempts{mode=call_with_raw} moved by %v, want 1 — the serve outcome must be recorded under the request's real mode", label, n.rawModeAttempts)
			}
			// BYTES: the complete HTTP response is stock BAML's, within this server.
			if n.resp != s.resp {
				t.Errorf("%s: HTTP response differs from stock BAML:\n  stock:  %d %q %q\n  native: %d %q %q",
					label, s.resp.status, s.resp.contentType, s.resp.body, n.resp.status, n.resp.contentType, n.resp.body)
			}
			wantStatus := http.StatusOK
			if row.wantWinner == "failure" {
				wantStatus = http.StatusInternalServerError
			}
			if s.resp.status != wantStatus {
				t.Errorf("%s: stock status %d, want %d — the row no longer exercises what it names (%s)", label, s.resp.status, wantStatus, s.resp.body)
			}
			if row.raw != nil || row.wantWinner == "failure" {
				if strings.Contains(n.resp.body, "exact") {
					t.Errorf("%s: the exact lane's wording reached the public error: %q", label, n.resp.body)
				}
			}
		}
	}
}

// TestBootedArtifactCallRouteUnchangedAlongsideCallWithRaw is the /call regression control on
// the same booted artifact and both servers: the bare data body stays byte-identical across
// the flags, and the flag-on leg is still native-owned with the serve outcome recorded under
// mode=call (zero call_with_raw attempts).
func TestBootedArtifactCallRouteUnchangedAlongsideCallWithRaw(t *testing.T) {
	requireStaticArtifact(t)
	provider := newRawProofProvider(t, staticFixtureLoopbackAddr)
	rows := rawProofRows(t, "map", "float_to_int", "lone_surrogate", "malformed_json", "body_over_the_size_cap", "2xx_body_truncated")
	stock := rawProofLeg(t, provider, false, bamlutils.StreamModeCall, rows)
	native := rawProofLeg(t, provider, true, bamlutils.StreamModeCall, rows)
	for _, server := range []string{"chi", "fiber"} {
		for i, row := range rows {
			s, n := stock[server][i], native[server][i]
			label := server + "/" + row.name
			if n.resp != s.resp {
				t.Errorf("%s: /call response differs from stock BAML:\n  stock:  %+v\n  native: %+v", label, s.resp, n.resp)
			}
			if n.providerRequests != 1 || n.nativeSockets != 1 || n.winners[row.wantWinner] != 1 {
				t.Errorf("%s: /call native leg requests=%d sockets=%v winners=%v, want one native socket and winner %s",
					label, n.providerRequests, n.nativeSockets, n.winners, row.wantWinner)
			}
			if n.rawModeAttempts != 0 {
				t.Errorf("%s: a /call request moved attempts{mode=call_with_raw} by %v", label, n.rawModeAttempts)
			}
		}
	}
}
