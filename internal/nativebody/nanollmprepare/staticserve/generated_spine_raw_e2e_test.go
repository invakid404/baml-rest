//go:build integration && nanollm_integration

package staticserve

// The /call-with-raw default-serve DIFFERENTIAL through the REAL generated adapter. For
// every corpus row the SAME provider response is served twice through the fixture's
// generated `StaticRecursiveAliasJSON` on StreamModeCallWithRaw:
//
//	flag OFF -> stock BAML v0.223 end to end (generated Request + Parse over CFFI)
//	flag ON  -> installNativeStaticCall -> the standardspineoracle composite -> the spine
//	            CallWithOracle lane (live plan compare, one native socket, the raw-aware
//	            reference oracle) -> the generated decoder
//
// and the COMPLETE public outcome must be identical: the final value's bytes, the raw and
// reasoning channels, or — for a failing row — the error text and its details.raw.
//
// Equality alone cannot tell native serving from a lane that always handed the request
// back to BAML, so the OWNERSHIP facts are asserted separately on the flag-on leg: the
// serve seam ran once, the spine claimed exactly one socket, the provider saw exactly one
// request, and the outcome winner is the row's expected engine (native on every row whose
// response native reads identically; the reference parse where native declines or fails).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/spine"

	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
	fwadapter "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated/adapter"
)

// rawOutcome is ONE leg's complete public outcome, reduced to what a /call-with-raw client
// receives (the worker marshals the final carrier; raw/reasoning ride beside it; an error
// carries its text and details.raw).
type rawOutcome struct {
	data, raw, reasoning string
	err, errRaw          string
	winner               string
}

// openAIEnvelope renders an OpenAI chat completion whose message is exactly msg.
func openAIEnvelope(msg map[string]any) []byte {
	env, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	return env
}

func assistantMsg(content any) map[string]any {
	return map[string]any{"role": "assistant", "content": content}
}

func withReasoning(msg map[string]any, reasoning any) map[string]any {
	msg["reasoning_content"] = reasoning
	return msg
}

// driveCallWithRaw drives the U1 method on the given adapter and drains every frame into
// the leg's public outcome.
func driveCallWithRaw(t *testing.T, a bamlutils.Adapter) rawOutcome {
	t.Helper()
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("StaticRecursiveAliasJSON install: %v", err)
	}
	var out rawOutcome
	for r := range ch {
		switch r.Kind() {
		case bamlutils.StreamResultKindFinal:
			out.data = jsonOf(t, r.Final())
			out.raw, out.reasoning = r.Raw(), r.Reasoning()
		case bamlutils.StreamResultKindError:
			if e := r.Error(); e != nil {
				out.err = e.Error()
			}
			out.errRaw = r.Raw()
		case bamlutils.StreamResultKindMetadata:
			if md := r.Metadata(); md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
				out.winner = md.WinnerEngine
			}
		}
		r.Release()
	}
	return out
}

// callWithRawRow is one corpus row: the provider's exact 2xx (or non-2xx) response, the
// request's reasoning preference, and the engine expected to own the flag-on envelope.
type callWithRawRow struct {
	name             string
	status           int
	body             []byte
	includeReasoning bool
	// wantWinner is the flag-on outcome winner: "native" when native's reading of the
	// response is identical facet-for-facet, "native_baml_parse" when the claimed lane
	// served the reference envelope (native declined or failed), "" for an error row.
	wantWinner string
	// wantError marks a row whose flag-off outcome is a failure.
	wantError bool
}

func callWithRawCorpus() []callWithRawRow {
	ok := func(name string, content any, wantWinner string) callWithRawRow {
		return callWithRawRow{name: name, status: http.StatusOK, body: openAIEnvelope(assistantMsg(content)), wantWinner: wantWinner}
	}
	rows := []callWithRawRow{}
	// The served exact-JSON manifest, so raw (pre-coercion text) and data (BAML coercion +
	// canonical serialization) visibly differ on the coercing rows (float->int, null->[],
	// key sort, duplicate-key overwrite).
	for _, r := range aliasManifestRows() {
		rows = append(rows, ok("manifest/"+r.name, r.content, "native"))
	}
	rows = append(rows,
		ok("whitespace_and_newlines", "\n  {\"k\" :\t[1, 2]}\n\n", "native"),
		ok("escapes_unicode_html", `{"k":"café ☕ \"q\" \\ back \t tab <b>&amp;</b> `+" "+` end","n":-9223372036854775808}`, "native"),
		ok("int64_limits", `[9223372036854775807,-9223372036854775808,0,-1]`, "native"),
		ok("markdown_fenced", "Here you go:\n```json\n{\"a\":[1,\"x\",true]}\n```\n", "native"),
		// Multipart content: raw is the concatenated TEXT parts; non-text parts are skipped.
		ok("multipart_text", []any{
			map[string]any{"type": "text", "text": `{"a":`},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/x.png"}},
			map[string]any{"type": "text", "text": `[1,2]}`},
		}, "native"),
		// A response native's static parser declines: the claimed lane serves the reference.
		ok("plain_text_decline", "the weather is sunny", "native_baml_parse"),
		ok("empty_content", "", "native_baml_parse"),
		ok("null_content", nil, "native_baml_parse"),
		// A lone UTF-16 surrogate: nanollm's translation fails, BAML's extractor decodes
		// it to U+FFFD — the reference envelope recovers the SAME response.
		callWithRawRow{
			name: "lone_surrogate_native_translate_failure", status: http.StatusOK,
			body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"{\"a\":\"\ud800\"}"}}]}`),
			wantWinner: "native_baml_parse",
		},
		// Reasoning channel variants (opted in): present with control characters, absent,
		// non-string (ignored by BAML's extractor), and present but NOT opted in.
		callWithRawRow{
			name: "reasoning_present", status: http.StatusOK, includeReasoning: true,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), "step 1\nstep \"2\"\tdone ☕")),
			wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_opted_in_absent", status: http.StatusOK, includeReasoning: true,
			body: openAIEnvelope(assistantMsg(`{"a":1}`)), wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_non_string", status: http.StatusOK, includeReasoning: true,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), 42)),
			wantWinner: "native",
		},
		callWithRawRow{
			name: "reasoning_present_not_opted_in", status: http.StatusOK, includeReasoning: false,
			body:       openAIEnvelope(withReasoning(assistantMsg(`{"a":1}`), "hidden")),
			wantWinner: "native",
		},
		// Error rows: the flag-on failure must be BAML's own failure, byte for byte.
		callWithRawRow{
			name: "refusal", status: http.StatusOK,
			body:      openAIEnvelope(map[string]any{"role": "assistant", "content": nil, "refusal": "I can't"}),
			wantError: true,
		},
		callWithRawRow{name: "malformed_json_body", status: http.StatusOK, body: []byte(`{"choices":[`), wantError: true},
		callWithRawRow{name: "missing_content", status: http.StatusOK, body: []byte(`{"choices":[{"message":{"role":"assistant"}}]}`), wantError: true},
		callWithRawRow{name: "provider_429", status: http.StatusTooManyRequests, body: []byte(`{"error":{"message":"rate limited"}}`), wantError: true},
	)
	return rows
}

// runCallWithRawLeg serves the row once on a fresh fixture server and returns the leg's
// outcome plus the ownership facts the flag-on assertions need.
func runCallWithRawLeg(t *testing.T, row callWithRawRow, flagOn bool) (out rawOutcome, serveCalls int, providerHits int64, snapClaims, snapSockets int64) {
	t.Helper()
	serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
	server := newFixtureServer(t, row.status, row.body)
	defer server.close()
	a := buildFixtureAdapterWithServe(t, serveFn, flagOn)
	ba := a.(*fwadapter.BamlAdapter)
	ba.SetStreamMode(bamlutils.StreamModeCallWithRaw)
	ba.SetIncludeReasoning(row.includeReasoning)
	out = driveCallWithRaw(t, a)
	snap := exec.Metrics().Snapshot()
	return out, *calls, server.count.Load(), int64(snap.Claims), int64(snap.Sockets)
}

// TestSpineComposite_CallWithRawMatchesStockBAML is the headline differential.
func TestSpineComposite_CallWithRawMatchesStockBAML(t *testing.T) {
	for _, row := range callWithRawCorpus() {
		t.Run(row.name, func(t *testing.T) {
			stock, stockCalls, stockHits, _, _ := runCallWithRawLeg(t, row, false)
			native, calls, hits, claims, sockets := runCallWithRawLeg(t, row, true)

			// The stock leg really is stock BAML: no serve seam, one BAML send.
			if stockCalls != 0 || stockHits != 1 {
				t.Fatalf("stock leg: serve seam ran %d times, provider saw %d requests; want 0 and 1", stockCalls, stockHits)
			}
			if (stock.err != "") != row.wantError {
				t.Fatalf("stock leg outcome = %+v, want error=%v — the corpus row no longer exercises what it names", stock, row.wantError)
			}

			// OWNERSHIP (flag on), asserted separately from the bytes: the native lane CLAIMED
			// the one socket — a pre-socket decline back to BAML would show claims=0.
			if calls != 1 {
				t.Fatalf("flag-on serve seam ran %d times, want exactly 1", calls)
			}
			if claims != 1 || sockets != 1 {
				t.Fatalf("flag-on spine claims=%d sockets=%d, want 1 and 1 — the raw request must be claimed natively, not declined to BAML", claims, sockets)
			}
			if hits != 1 {
				t.Fatalf("flag-on provider saw %d requests, want exactly 1 (no BAML resend)", hits)
			}
			if !row.wantError && native.winner != row.wantWinner {
				t.Errorf("flag-on winner = %q, want %q", native.winner, row.wantWinner)
			}

			// BYTES: the complete public outcome is stock BAML's.
			if native.data != stock.data || native.raw != stock.raw || native.reasoning != stock.reasoning {
				t.Errorf("served envelope differs from stock BAML:\n  stock:  data=%s raw=%q reasoning=%q\n  native: data=%s raw=%q reasoning=%q",
					stock.data, stock.raw, stock.reasoning, native.data, native.raw, native.reasoning)
			}
			if native.err != stock.err || native.errRaw != stock.errRaw {
				t.Errorf("failure differs from stock BAML:\n  stock:  err=%q raw=%q\n  native: err=%q raw=%q", stock.err, stock.errRaw, native.err, native.errRaw)
			}
		})
	}
}

// TestSpineComposite_CallWithRawCorpusIsNotVacuous guards the differential's own corpus: it
// must contain rows native WINS (so the lane is proven to serve, not merely to hand back),
// rows where raw and data visibly differ (so raw cannot be reconstructed from data), and a
// row whose reasoning channel is non-empty.
func TestSpineComposite_CallWithRawCorpusIsNotVacuous(t *testing.T) {
	var nativeRows, coercing, reasoningRows int
	for _, row := range callWithRawCorpus() {
		if row.wantWinner == "native" {
			nativeRows++
		}
		if row.includeReasoning && row.name == "reasoning_present" {
			reasoningRows++
		}
	}
	for _, r := range aliasManifestRows() {
		if r.content != r.want {
			coercing++
		}
	}
	if nativeRows < 10 || coercing < 3 || reasoningRows != 1 {
		t.Fatalf("corpus native-win rows=%d coercing rows=%d reasoning rows=%d; the differential would prove too little", nativeRows, coercing, reasoningRows)
	}
}

// rawFixtureServer is a fixture-port loopback that answers every request with the exact
// bytes write produces and then closes the connection — how a truncated body, a
// connection closed before the first response byte, or an oversized body is produced.
type rawFixtureServer struct {
	ln    net.Listener
	count atomic.Int64
}

func newRawFixtureServer(t *testing.T, write func(w *bufio.Writer)) *rawFixtureServer {
	t.Helper()
	rs := &rawFixtureServer{ln: listenFixturePort(t)}
	go func() {
		for {
			c, err := rs.ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				rs.count.Add(1)
				w := bufio.NewWriter(c)
				write(w)
				_ = w.Flush()
			}(c)
		}
	}()
	return rs
}

func (rs *rawFixtureServer) close() { _ = rs.ln.Close() }

// transportRows are provider behaviors the transport — not the response oracle — decides:
// sends that fail before a complete response exists, a response only a lenient backend
// accepts, and a truncation past any fixed read cap. Flag-off ends wherever the ordinary
// llmhttp Client ends; the claimed native lane must end in the same place. (Deadlines are
// pinned at the spine boundary, nativeserve/spine's transport rows: once the request
// context is done the orchestrator publishes no frame on either leg, so a deadline row
// here would compare two empty outcomes.)
type transportRow struct {
	name  string
	write func(w *bufio.Writer)
}

func transportRows() []transportRow {
	return []transportRow{
		{name: "body_over_the_size_cap", write: func(w *bufio.Writer) {
			fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				llmhttp.MaxResponseBodyBytes+1, strings.Repeat(" ", llmhttp.MaxResponseBodyBytes+1))
		}},
		{name: "2xx_body_truncated", write: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"choices\":[{\"message\":")
		}},
		{name: "non_2xx_body_truncated", write: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"error\":{\"message\":")
		}},
		{name: "non_2xx_body_truncated_past_64KiB", write: func(w *bufio.Writer) {
			fmt.Fprintf(w, "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 200000\r\nConnection: close\r\n\r\n%s", strings.Repeat("e", 100000))
		}},
		{name: "closed_before_the_first_response_byte", write: func(*bufio.Writer) {}},
		{name: "chunked_2xx_usable_chunk_no_terminating_chunk", write: func(w *bufio.Writer) {
			body := `{"choices":[{"message":{"role":"assistant","content":"{\"weather\":\"sunny\"}"}}]}`
			fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n", len(body), body)
		}},
	}
}

// TestSpineComposite_TransportMatchesStockBAML runs every transport row through the
// generated adapter flag-off (stock BAML over the ordinary Client) and flag-on (the
// claimed native lane), under both client modes the worker can be configured with, on
// /call-with-raw AND /call, and requires the identical public outcome — the served
// final/raw/reasoning, or the error text and details.raw — with the flag-on leg's one
// claimed socket and single provider request asserted separately. A row the ordinary
// Client delivers must be SERVED by the native lane (never failed where flag-off
// succeeds), and its winner must be native.
func TestSpineComposite_TransportMatchesStockBAML(t *testing.T) {
	for _, mode := range []string{"auto", "nethttp"} {
		for _, row := range transportRows() {
			for _, streamMode := range []bamlutils.StreamMode{bamlutils.StreamModeCallWithRaw, bamlutils.StreamModeCall} {
				t.Run(fmt.Sprintf("%s/%s/%v", mode, row.name, streamMode.NeedsRaw()), func(t *testing.T) {
					t.Setenv(llmhttp.EnvVarClientMode, mode)
					leg := func(flagOn bool) (rawOutcome, int, int64, int64) {
						serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
						server := newRawFixtureServer(t, row.write)
						defer server.close()
						a := buildFixtureAdapterWithServe(t, serveFn, flagOn)
						a.(*fwadapter.BamlAdapter).SetStreamMode(streamMode)
						out := driveCallWithRaw(t, a)
						return out, *calls, server.count.Load(), int64(exec.Metrics().Snapshot().Claims)
					}
					stock, stockCalls, stockHits, _ := leg(false)
					native, calls, hits, claims := leg(true)

					if stockCalls != 0 || stockHits != 1 {
						t.Fatalf("stock leg: serve seam ran %d times, provider saw %d requests; want 0 and 1", stockCalls, stockHits)
					}
					if stock.err == "" && stock.data == "" {
						t.Fatalf("the stock leg produced no outcome at all; the row compares nothing")
					}
					if calls != 1 || claims != 1 || hits != 1 {
						t.Fatalf("flag-on: serve seam %d, claims %d, provider requests %d; want 1, 1, 1 — the native lane must own the one send", calls, claims, hits)
					}
					if stock.err == "" && stock.data != "" && native.winner != bamlutils.NativeStaticServeEngineNative {
						t.Errorf("flag-off served %s; the native lane must serve it too (winner %q, err %q)", stock.data, native.winner, native.err)
					}
					stock.winner, native.winner = "", ""
					if native != stock {
						t.Errorf("outcome differs from stock BAML:\n  stock:  %+v\n  native: %+v", stock, native)
					}
					if strings.Contains(native.err, "exact") {
						t.Errorf("the exact lane's wording reached the public error: %q", native.err)
					}
				})
			}
		}
	}
}

// fixtureSentRequest is one request the fixture provider received.
type fixtureSentRequest struct{ method, path, body string }

// redirectFixtureServer binds the fixture port, answers the admitted chat-completions path
// with a redirect of the given status to /redirected, and /redirected (any method) with a
// complete OpenAI response; it records the request sequence.
type redirectFixtureServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	seq []fixtureSentRequest
}

func newRedirectFixtureServer(t *testing.T, status int) *redirectFixtureServer {
	t.Helper()
	rs := &redirectFixtureServer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rs.mu.Lock()
		rs.seq = append(rs.seq, fixtureSentRequest{method: r.Method, path: r.URL.Path, body: string(body)})
		rs.mu.Unlock()
		if r.URL.Path == "/redirected" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(openAIJSONMap())
			return
		}
		w.Header().Set("Location", "/redirected")
		w.Header().Set("Connection", "close")
		w.WriteHeader(status)
	}))
	_ = srv.Listener.Close()
	srv.Listener = listenFixturePort(t)
	srv.Start()
	rs.srv = srv
	return rs
}

func (rs *redirectFixtureServer) close() { rs.srv.Close() }

func (rs *redirectFixtureServer) sequence() []fixtureSentRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]fixtureSentRequest(nil), rs.seq...)
}

// TestSpineComposite_RedirectsMatchStockBAML is the generated-adapter redirect
// differential: every redirect status, under both client modes, on /call-with-raw and
// /call. Stock BAML (flag off) and the claimed native lane (flag on) must put the SAME
// provider-request sequence on the wire — the redirect hop, its method and body included —
// and produce the identical public outcome; the native leg's socket accounting equals
// that sequence's length, and its claim count is one (so no BAML send followed it: the
// native leg's sequence equals the stock leg's, which is BAML's only).
func TestSpineComposite_RedirectsMatchStockBAML(t *testing.T) {
	for _, mode := range []string{"auto", "nethttp"} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			for _, streamMode := range []bamlutils.StreamMode{bamlutils.StreamModeCallWithRaw, bamlutils.StreamModeCall} {
				t.Run(fmt.Sprintf("%s/%d/%v", mode, status, streamMode.NeedsRaw()), func(t *testing.T) {
					t.Setenv(llmhttp.EnvVarClientMode, mode)
					leg := func(flagOn bool) (rawOutcome, []fixtureSentRequest, int, spine.MetricsSnapshot) {
						serveFn, exec, calls := fixtureBamlSrcSpineServe(t)
						server := newRedirectFixtureServer(t, status)
						defer server.close()
						a := buildFixtureAdapterWithServe(t, serveFn, flagOn)
						a.(*fwadapter.BamlAdapter).SetStreamMode(streamMode)
						out := driveCallWithRaw(t, a)
						return out, server.sequence(), *calls, exec.Metrics().Snapshot()
					}
					stock, stockSeq, stockCalls, _ := leg(false)
					native, nativeSeq, calls, snap := leg(true)

					if stockCalls != 0 || calls != 1 || snap.Claims != 1 {
						t.Fatalf("serve seam stock=%d native=%d, native claims=%d; want 0, 1, 1", stockCalls, calls, snap.Claims)
					}
					if !reflect.DeepEqual(nativeSeq, stockSeq) || len(stockSeq) == 0 {
						t.Fatalf("provider-request sequence differs from stock BAML:\n  stock:  %+v\n  native: %+v", stockSeq, nativeSeq)
					}
					if snap.Sockets != int64(len(nativeSeq)) {
						t.Errorf("native sockets = %d, want %d — one per provider request actually sent", snap.Sockets, len(nativeSeq))
					}
					if mode == "nethttp" && len(nativeSeq) != 2 {
						t.Errorf("net/http must follow the %d: sequence %+v", status, nativeSeq)
					}
					if mode == "auto" && len(nativeSeq) != 1 {
						t.Errorf("fasthttp follows no redirect: sequence %+v", nativeSeq)
					}
					if stock.err == "" && native.winner != bamlutils.NativeStaticServeEngineNative {
						t.Errorf("flag-off served %s; the native lane must serve it too (winner %q, err %q)", stock.data, native.winner, native.err)
					}
					stock.winner, native.winner = "", ""
					if native != stock {
						t.Errorf("outcome differs from stock BAML:\n  stock:  %+v\n  native: %+v", stock, native)
					}
				})
			}
		}
	}
}

// TestSpineComposite_LaneSendsThroughTheOrchestratorsClient pins the premise the lane's
// transport parity rests on: the client the generated seam hands the live-oracle lane IS
// the client the orchestrator sends the BAML path through — the same instance, so the same
// redirect, retry, timeout and backend policy — for an adapter that carries its own client
// and for one that does not (both fall back to llmhttp.DefaultClient). The behavioral half
// configures the adapter's client to REFUSE redirects: flag-off then surfaces the 307
// itself, and the native lane must too — it would follow it if it sent through any other
// client.
func TestSpineComposite_LaneSendsThroughTheOrchestratorsClient(t *testing.T) {
	t.Run("same instance", func(t *testing.T) {
		for _, withClient := range []bool{true, false} {
			inner, _, _ := fixtureBamlSrcSpineServe(t)
			var seen *llmhttp.Client
			serveFn := func(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticServeResult {
				seen = inv.HTTPClient
				return inner(ctx, inv)
			}
			server := newFixtureServer(t, http.StatusOK, openAIJSONMap())
			a := buildFixtureAdapterWithServe(t, serveFn, true)
			ba := a.(*fwadapter.BamlAdapter)
			want := llmhttp.DefaultClient
			if withClient {
				want = llmhttp.NewClient(&http.Client{Transport: &http.Transport{Proxy: nil}})
				ba.SetHTTPClient(want)
			} else {
				ba.SetHTTPClient(nil)
			}
			_ = driveCallWithRaw(t, a)
			server.close()
			if seen == nil || seen != want {
				t.Errorf("adapter client set=%v: the lane received %p, want the orchestrator's client %p", withClient, seen, want)
			}
		}
	})
	t.Run("the adapter client's redirect policy governs the native send", func(t *testing.T) {
		t.Setenv(llmhttp.EnvVarClientMode, "nethttp")
		noRedirect := func() *llmhttp.Client {
			return llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
				Mode: llmhttp.ClientModeNetHTTP,
				NetHTTPClient: &http.Client{
					Transport:     &http.Transport{Proxy: nil},
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				},
			})
		}
		leg := func(flagOn bool) (rawOutcome, []fixtureSentRequest) {
			serveFn, _, _ := fixtureBamlSrcSpineServe(t)
			server := newRedirectFixtureServer(t, http.StatusTemporaryRedirect)
			defer server.close()
			a := buildFixtureAdapterWithServe(t, serveFn, flagOn)
			a.(*fwadapter.BamlAdapter).SetHTTPClient(noRedirect())
			a.(*fwadapter.BamlAdapter).SetStreamMode(bamlutils.StreamModeCallWithRaw)
			return driveCallWithRaw(t, a), server.sequence()
		}
		stock, stockSeq := leg(false)
		native, nativeSeq := leg(true)
		if len(stockSeq) != 1 || stock.err == "" {
			t.Fatalf("stock leg did not stop at the 307 (%+v, %+v); the client policy is not exercised", stock, stockSeq)
		}
		if !reflect.DeepEqual(nativeSeq, stockSeq) || native.err != stock.err {
			t.Errorf("native ignored the adapter client's redirect policy:\n  stock:  %+v %+v\n  native: %+v %+v", stock, stockSeq, native, nativeSeq)
		}
	})
}
