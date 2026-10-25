//go:build nanollm_integration

package spine

// The /call-with-raw LIVE-oracle envelope matrix. Each test drives the REAL
// AdmitStaticSpineOracleClaim + one real provider RoundTrip through CallWithOracle and pins
// the raw-aware reference oracle's contract: the reference channels come from BAML's
// extraction of the ORIGINAL provider body, native wins only on a FULL facet match, and
// any drift, decline, or native post-response failure serves the WHOLE reference envelope
// from the same response — never a resend.
//
// Every positive assertion is split into the two facts that are easy to conflate: WHO won
// (winner + one claim + one provider request) and WHAT was served (the exact bytes). A
// lane that always fell back to BAML would serve correct bytes too, so the winner facts are
// asserted separately and a native-win test FAILS on a fallback.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// okJSONWithReasoning writes an OpenAI-shaped 2xx whose assistant message carries content
// and a reasoning_content channel, both verbatim.
func okJSONWithReasoning(w http.ResponseWriter, content, reasoning string) {
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": content, "reasoning_content": reasoning,
		}}},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(env)
}

// rawInv is liveInv on /call-with-raw with the reasoning channel opted in.
func rawInv(t *testing.T, buildBAML func(context.Context) (*llmhttp.Request, error), bamlJSON string, captured *string) bamlutils.NativeStaticInvocation {
	t.Helper()
	inv := liveInv(t, buildBAML, bamlJSON, captured)
	inv.Raw = true
	inv.IncludeReasoning = true
	return inv
}

// assertOneClaimedSend pins the ownership facts every claimed raw-lane row shares: one
// claim, one native socket, one provider request.
func assertOneClaimedSend(t *testing.T, e *UnaryExecutor, lb *oracleLoopback) {
	t.Helper()
	if got := lb.hits.Load(); got != 1 {
		t.Errorf("provider hits = %d, want exactly 1 (native's one send; no BAML resend)", got)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Declines != 0 {
		t.Errorf("executor counters = %+v, want claims=1 sockets=1 declines=0", snap)
	}
}

// TestCallWithOracle_LiveOracle_CallWithRawNativeWinsTheWholeEnvelope is the positive
// control for the matrix below: a /call-with-raw request whose content, raw and reasoning
// channels all agree with the reference serves NATIVE, and the served raw/reasoning pair is
// exactly the provider's text.
func TestCallWithOracle_LiveOracle_CallWithRawNativeWinsTheWholeEnvelope(t *testing.T) {
	const content = "{\"weather\":\"sunny\"}"
	const reasoning = "thinking it through\nline two \"quoted\""
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONWithReasoning(w, content, reasoning) })
	e, buildBAML := liveOracle(t, lb.baseURL())

	var captured string
	res := e.CallWithOracle(context.Background(), rawInv(t, buildBAML, content, &captured))
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v, stage %q reason %q), want succeeded", res.Disposition, res.Err, res.Stage, res.Reason)
	}
	// WHO won — asserted separately from the bytes, so an always-fallback lane fails here.
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("winner = %q, want native: every facet agreed, so native must own the raw envelope", res.WinnerEngine)
	}
	assertOneClaimedSend(t, e, lb)
	// WHAT was served.
	if string(res.FinalJSON) != content || res.Raw != content || res.Reasoning != reasoning {
		t.Errorf("served envelope = (%s, %q, %q), want (%s, %q, %q)", res.FinalJSON, res.Raw, res.Reasoning, content, content, reasoning)
	}
	if captured != content {
		t.Errorf("BAML parsed %q, want the reference parseable text %q", captured, content)
	}
	o := res.Observations
	if !o.ChannelsCompared || !o.AssistantMatch || !o.RawMatch || !o.ReasoningMatch ||
		!o.StructuredBranchServed || !o.StructuredMatch || !o.OrderMatch || !o.CanonicalBytesMatch || o.Fallback {
		t.Errorf("observations = %+v, want every facet compared and matched with no fallback", o)
	}
}

// TestCallWithOracle_LiveOracle_ReferenceReadsTheOriginalProviderBody proves the reference
// leg extracts from the ORIGINAL provider bytes, not nanollm's re-serialized translation: the
// provider writes a body with non-canonical spacing and an extra top-level key, and the
// reference extractor must be handed EXACTLY those bytes.
func TestCallWithOracle_LiveOracle_ReferenceReadsTheOriginalProviderBody(t *testing.T) {
	const body = `{ "zzz_extra" : 1 ,  "choices" : [ { "message" : { "role" : "assistant" , "content" : "{\"weather\":\"sunny\"}" } } ] }`
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	e, buildBAML := liveOracle(t, lb.baseURL())
	var seen []byte
	e.extractReference = func(provider string, b []byte, includeReasoning bool) (string, string, string, error) {
		seen = append([]byte(nil), b...)
		return buildrequest.ExtractResponseContentBytes(provider, b, includeReasoning)
	}

	var captured string
	res := e.CallWithOracle(context.Background(), rawInv(t, buildBAML, `{"weather":"sunny"}`, &captured))
	if res.Disposition != bamlutils.NativeSpineSucceeded || res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("disposition/winner = %v/%q (err %v), want a native win", res.Disposition, res.WinnerEngine, res.Err)
	}
	if string(seen) != body {
		t.Errorf("the reference extractor saw %q, want the ORIGINAL provider bytes %q (never the translated body)", seen, body)
	}
	assertOneClaimedSend(t, e, lb)
}

// TestCallWithOracle_LiveOracle_CallWithRawFacetDriftServesTheReferenceEnvelope injects ONE
// facet's drift at a time — assistant text only, raw only, reasoning only, the canonical
// bytes only (a value that is SEMANTICALLY equal), and the value itself — and requires each
// to (a) refuse native, (b) serve the WHOLE reference envelope, (c) attribute the drift to
// exactly that facet, and (d) stay on ONE provider request. A gate that ignored any one
// facet would serve native for that row; an envelope that mixed native channels into the
// substitute would fail the served-bytes check.
func TestCallWithOracle_LiveOracle_CallWithRawFacetDriftServesTheReferenceEnvelope(t *testing.T) {
	const content = `{"weather":"sunny"}`
	const reasoning = "thinking it through"
	perturb := func(dp, dr, ds string) func(string, []byte, bool) (string, string, string, error) {
		return func(provider string, b []byte, includeReasoning bool) (string, string, string, error) {
			p, r, s, err := buildrequest.ExtractResponseContentBytes(provider, b, includeReasoning)
			return p + dp, r + dr, s + ds, err
		}
	}
	type facets struct{ assistant, raw, reasoning, structured, canonical bool }
	cases := []struct {
		name     string
		extract  func(string, []byte, bool) (string, string, string, error)
		bamlJSON string
		wantRaw  string
		wantReas string
		want     facets
	}{
		{
			name: "assistant only", extract: perturb(" ", "", ""), bamlJSON: content,
			wantRaw: content, wantReas: reasoning,
			want: facets{assistant: false, raw: true, reasoning: true, structured: true, canonical: true},
		},
		{
			name: "raw only", extract: perturb("", "\n", ""), bamlJSON: content,
			wantRaw: content + "\n", wantReas: reasoning,
			want: facets{assistant: true, raw: false, reasoning: true, structured: true, canonical: true},
		},
		{
			name: "reasoning only", extract: perturb("", "", "!"), bamlJSON: content,
			wantRaw: content, wantReas: reasoning + "!",
			want: facets{assistant: true, raw: true, reasoning: false, structured: true, canonical: true},
		},
		{
			// Semantically and order-equal, but NOT the same bytes: the literal canonical
			// gate alone refuses native.
			name: "canonical bytes only", extract: nil, bamlJSON: `{"weather": "sunny"}`,
			wantRaw: content, wantReas: reasoning,
			want: facets{assistant: true, raw: true, reasoning: true, structured: true, canonical: false},
		},
		{
			name: "value", extract: nil, bamlJSON: `{"weather":"rainy"}`,
			wantRaw: content, wantReas: reasoning,
			want: facets{assistant: true, raw: true, reasoning: true, structured: false, canonical: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONWithReasoning(w, content, reasoning) })
			e, buildBAML := liveOracle(t, lb.baseURL())
			if tc.extract != nil {
				e.extractReference = tc.extract
			}
			var captured string
			res := e.CallWithOracle(context.Background(), rawInv(t, buildBAML, tc.bamlJSON, &captured))
			if res.Disposition != bamlutils.NativeSpineSucceeded {
				t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
			}
			if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
				t.Fatalf("winner = %q, want %q — a %s drift must refuse native", res.WinnerEngine, bamlutils.NativeStaticServeEngineBAMLParse, tc.name)
			}
			assertOneClaimedSend(t, e, lb)
			if string(res.FinalJSON) != tc.bamlJSON || res.Raw != tc.wantRaw || res.Reasoning != tc.wantReas {
				t.Errorf("served envelope = (%s, %q, %q), want the WHOLE reference (%s, %q, %q)",
					res.FinalJSON, res.Raw, res.Reasoning, tc.bamlJSON, tc.wantRaw, tc.wantReas)
			}
			o := res.Observations
			got := facets{o.AssistantMatch, o.RawMatch, o.ReasoningMatch, o.StructuredMatch, o.CanonicalBytesMatch}
			if got != tc.want {
				t.Errorf("facets = %+v, want %+v — the drift must be attributed to exactly its facet", got, tc.want)
			}
			if !o.ChannelsCompared || !o.StructuredBranchServed || !o.Fallback || o.NativeFailureRecovered {
				t.Errorf("observations = %+v, want a compared structured branch served as a fallback", o)
			}
		})
	}
}

// TestCallWithOracle_LiveOracle_NativeTranslateFailureRecoversFromTheReference is the
// native-failure-versus-BAML-success case with a REAL native failure: a lone UTF-16
// surrogate in the provider body makes nanollm's translation fail, while BAML's extractor
// decodes it (to U+FFFD) and BAML parses the text. The reference leg is established
// independently of native, so the reference envelope is served from the SAME response on
// both /call and /call-with-raw — no resend, and no terminal error flag-off would not have
// raised.
func TestCallWithOracle_LiveOracle_NativeTranslateFailureRecoversFromTheReference(t *testing.T) {
	const body = `{"choices":[{"message":{"role":"assistant","content":"{\"a\":\"\ud800\"}"}}]}`
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "call", true: "call-with-raw"}[raw], func(t *testing.T) {
			lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			e, buildBAML := liveOracle(t, lb.baseURL())
			var captured string
			inv := liveInv(t, buildBAML, `{"a":"�"}`, &captured)
			inv.Raw = raw
			res := e.CallWithOracle(context.Background(), inv)
			if res.Disposition != bamlutils.NativeSpineSucceeded {
				t.Fatalf("disposition = %v (err %v), want succeeded — BAML succeeds on these bytes, so the claimed request must be served", res.Disposition, res.Err)
			}
			if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
				t.Errorf("winner = %q, want the reference (native failed)", res.WinnerEngine)
			}
			assertOneClaimedSend(t, e, lb)
			const want = "{\"a\":\"�\"}"
			if captured != want || res.Raw != want {
				t.Errorf("reference parseable/raw = %q/%q, want BAML's extraction %q", captured, res.Raw, want)
			}
			o := res.Observations
			if !o.NativeFailureRecovered || o.ChannelsCompared || o.StructuredBranchServed || !o.Fallback {
				t.Errorf("observations = %+v, want a recovered native failure (no native channels, fallback served)", o)
			}
			if !o.ErrorCompareRecorded || o.ErrorCompareMatch {
				t.Errorf("error facet recorded=%v match=%v, want a recorded MISMATCH — the native leg errored", o.ErrorCompareRecorded, o.ErrorCompareMatch)
			}
		})
	}
}

// TestCallWithOracle_LiveOracle_ReferenceFailuresAreTerminal pins the BAML-error-terminal
// half: when BAML's own serving path would FAIL on the response, the claimed request fails
// with exactly BAML's error class, wrap prefix, and details.raw — never a native success,
// never a resend.
func TestCallWithOracle_LiveOracle_ReferenceFailuresAreTerminal(t *testing.T) {
	errBAML := errors.New("baml: could not coerce")
	cases := []struct {
		name      string
		body      string
		parseErr  error
		wantErr   string
		wantRaw   string
		wantParse bool
	}{
		{
			name:     "extraction (refusal)",
			body:     `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"no"}}]}`,
			wantErr:  "buildrequest: failed to extract response content: openai: model refused request: no",
			wantRaw:  `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"no"}}]}`,
			parseErr: nil,
		},
		{
			name:      "parse",
			body:      `{"choices":[{"message":{"role":"assistant","content":"{\"weather\":\"sunny\"}"}}]}`,
			parseErr:  errBAML,
			wantErr:   "buildrequest: failed to parse final result: baml: could not coerce",
			wantRaw:   `{"weather":"sunny"}`,
			wantParse: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			})
			e, buildBAML := liveOracle(t, lb.baseURL())
			parsed := 0
			inv := rawInv(t, buildBAML, "", new(string))
			inv.BAMLOnlyParse = func(context.Context, string) ([]byte, error) {
				parsed++
				if tc.parseErr != nil {
					return nil, tc.parseErr
				}
				return []byte(`"never served"`), nil
			}
			res := e.CallWithOracle(context.Background(), inv)
			if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
				t.Fatalf("disposition = %v, want failed_after_claim", res.Disposition)
			}
			if res.Err == nil || res.Err.Error() != tc.wantErr {
				t.Errorf("err = %v, want EXACTLY %q (BAML's serving-path envelope)", res.Err, tc.wantErr)
			}
			if tc.wantParse && !errors.Is(res.Err, buildrequest.ErrOutputParse) {
				t.Errorf("err = %v, want an ErrOutputParse-classed failure", res.Err)
			}
			if res.RawDiagnostic != tc.wantRaw {
				t.Errorf("details.raw = %q, want %q", res.RawDiagnostic, tc.wantRaw)
			}
			if (parsed == 1) != tc.wantParse {
				t.Errorf("BAML parse ran %d times, want parse=%v", parsed, tc.wantParse)
			}
			assertOneClaimedSend(t, e, lb)
			if o := res.Observations; !o.SameResponseOracleRan || !o.ErrorCompareRecorded || o.ErrorCompareMatch || o.Fallback {
				t.Errorf("observations = %+v, want the oracle entered with an error-facet mismatch and nothing served", o)
			}
		})
	}
}

// TestCallWithOracle_LiveOracle_CancelDuringReferenceParseIsTerminal: a request canceled
// while BAML parses the reference is a terminal transport-class failure carrying the
// context error UNCHANGED — even though the parser ignored cancellation and returned a
// value — with one send and no resend.
func TestCallWithOracle_LiveOracle_CancelDuringReferenceParseIsTerminal(t *testing.T) {
	const content = `{"weather":"sunny"}`
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONWithReasoning(w, content, "r") })
	e, buildBAML := liveOracle(t, lb.baseURL())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inv := rawInv(t, buildBAML, "", new(string))
	inv.BAMLOnlyParse = func(context.Context, string) ([]byte, error) {
		cancel()
		return []byte(content), nil
	}
	res := e.CallWithOracle(ctx, inv)
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim", res.Disposition)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled unchanged", res.Err)
	}
	if res.Raw != "" || res.FinalJSON != nil {
		t.Errorf("a canceled request carried a served payload (%s, %q)", res.FinalJSON, res.Raw)
	}
	assertOneClaimedSend(t, e, lb)
}

// TestCallWithOracle_LiveOracle_CallWithRawProviderFaultIsTerminal: a non-2xx on the raw lane
// is the provider error BAML's transport raises (real status + capped body), the reference
// parse never runs, and nothing is resent.
func TestCallWithOracle_LiveOracle_CallWithRawProviderFaultIsTerminal(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	})
	e, buildBAML := liveOracle(t, lb.baseURL())
	var captured string
	res := e.CallWithOracle(context.Background(), rawInv(t, buildBAML, `"x"`, &captured))
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim", res.Disposition)
	}
	var httpErr *llmhttp.HTTPError
	if !errors.As(res.Err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable || !strings.Contains(httpErr.Body, "overloaded") {
		t.Errorf("err = %v (%T), want *llmhttp.HTTPError{503, body}", res.Err, res.Err)
	}
	if captured != "" {
		t.Errorf("the reference parse ran on a non-2xx (captured %q)", captured)
	}
	assertOneClaimedSend(t, e, lb)
}

// rawFailureLoopback is a loopback provider that answers every request with the exact
// bytes write produces and then closes the connection — the only way to produce a
// truncated body, a connection closed before the first response byte, or a body over
// the size cap.
type rawFailureLoopback struct {
	ln   net.Listener
	hits atomic.Int64
}

func newRawFailureLoopback(t *testing.T, write func(w *bufio.Writer)) *rawFailureLoopback {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	lb := &rawFailureLoopback{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
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
				lb.hits.Add(1)
				w := bufio.NewWriter(c)
				write(w)
				_ = w.Flush()
			}(c)
		}
	}()
	return lb
}

func (lb *rawFailureLoopback) baseURL() string { return "http://" + lb.ln.Addr().String() + "/v1" }

// transportRow is one provider behavior the transport — not the response oracle — decides:
// the bytes the provider writes on the connection, and an optional deadline on the call.
type transportRow struct {
	name    string
	write   func(w *bufio.Writer)
	timeout time.Duration
}

func transportRows() []transportRow {
	chunkedUsable := func(w *bufio.Writer) {
		body := `{"choices":[{"message":{"role":"assistant","content":"{\"weather\":\"sunny\"}"}}]}`
		fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n", len(body), body)
	}
	return []transportRow{
		{name: "body over the size cap", write: func(w *bufio.Writer) {
			fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				llmhttp.MaxResponseBodyBytes+1, strings.Repeat(" ", llmhttp.MaxResponseBodyBytes+1))
		}},
		{name: "2xx body truncated", write: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"choices\":")
		}},
		{name: "non-2xx body truncated", write: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"error\":")
		}},
		{name: "non-2xx body truncated past 64 KiB", write: func(w *bufio.Writer) {
			fmt.Fprintf(w, "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 200000\r\nConnection: close\r\n\r\n%s", strings.Repeat("e", 100000))
		}},
		{name: "closed before the first response byte", write: func(*bufio.Writer) {}},
		{name: "chunked 2xx, usable chunk, no terminating chunk", write: chunkedUsable},
		{name: "deadline before headers", timeout: 300 * time.Millisecond, write: func(w *bufio.Writer) { time.Sleep(time.Second) }},
		{name: "deadline during the body", timeout: 300 * time.Millisecond, write: func(w *bufio.Writer) {
			w.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"choices\":")
			_ = w.Flush()
			time.Sleep(time.Second)
		}},
	}
}

// ordinaryOutcome is what flag-off's ordinary Client — the real one the worker builds,
// called as the orchestrator calls it — returns for the plan: the error, or the status and
// body it delivered.
type ordinaryOutcome struct {
	err    error
	status int
	body   string
}

func callOrdinary(client *llmhttp.Client, plan *llmhttp.Request, timeout time.Duration) ordinaryOutcome {
	ctx, cancel := rowContext(timeout)
	defer cancel()
	resp, err := client.ExecuteBorrowed(ctx, plan, func() {})
	if err != nil {
		return ordinaryOutcome{err: err}
	}
	defer resp.Release()
	return ordinaryOutcome{status: resp.StatusCode, body: resp.BodyString()}
}

func rowContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithCancel(context.Background())
}

// assertNativeMatchesOrdinary drives the native leg with an identically configured client
// (each leg owns its client, as flag-off and flag-on each own their worker's) and checks it
// against the ordinary outcome: a delivered response must be SERVED (the oracle reads the
// very bytes the ordinary client delivered), and a failure must be the identical error —
// message, *HTTPError, flake classification, unexpected-EOF and context causes — and the
// serve outcome class must follow it. Ownership (one claim, one send) is asserted
// separately.
func assertNativeMatchesOrdinary(t *testing.T, e *UnaryExecutor, hits func() int64, inv bamlutils.NativeStaticInvocation, want ordinaryOutcome, timeout time.Duration) {
	t.Helper()
	before := hits()
	ctx, cancel := rowContext(timeout)
	defer cancel()
	res := e.CallWithOracle(ctx, inv)
	if got := hits() - before; got != 1 {
		t.Errorf("native leg provider hits = %d, want exactly 1 (no resend)", got)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 {
		t.Errorf("executor counters = %+v, want one claim and one socket", snap)
	}
	if want.err == nil {
		if res.Disposition != bamlutils.NativeSpineSucceeded {
			t.Fatalf("the ordinary client delivered %d %q but the native leg failed: %v — native must never be stricter than flag-off", want.status, want.body, res.Err)
		}
		return
	}
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v (err %v), want failed_after_claim like flag-off (%v)", res.Disposition, res.Err, want.err)
	}
	if res.Err == nil || res.Err.Error() != want.err.Error() {
		t.Fatalf("error:\n  flag-off: %q\n  native:   %v", want.err.Error(), res.Err)
	}
	var wantHTTP, gotHTTP *llmhttp.HTTPError
	if errors.As(want.err, &wantHTTP) != errors.As(res.Err, &gotHTTP) ||
		errors.Is(want.err, llmhttp.ErrTransportFlake) != errors.Is(res.Err, llmhttp.ErrTransportFlake) ||
		errors.Is(want.err, io.ErrUnexpectedEOF) != errors.Is(res.Err, io.ErrUnexpectedEOF) ||
		errors.Is(want.err, context.DeadlineExceeded) != errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("typed causes differ: flag-off %T / native %T", want.err, res.Err)
	}
	wantOutcome := bamlutils.NativeStaticOutcomeTransportError
	if wantHTTP != nil {
		wantOutcome = bamlutils.NativeStaticOutcomeProviderError
	}
	if got := res.Observations.ServeOutcome; got != wantOutcome {
		t.Errorf("serve outcome = %v, want %v", got, wantOutcome)
	}
}

// TestCallWithOracle_LiveOracle_TransportIsTheOrdinaryClients pins that the lane's one send
// IS the ordinary Client's: for every row — including a response only a lenient backend
// accepts, a truncation past any fixed read cap, and deadlines — under BOTH client modes the
// worker can run (auto, which routes a plain-http origin to fasthttp, and nethttp), on
// /call and /call-with-raw, the native leg ends exactly where the real ordinary Client ends:
// served when it delivers a response, the identical error when it fails.
func TestCallWithOracle_LiveOracle_TransportIsTheOrdinaryClients(t *testing.T) {
	for _, mode := range []string{"auto", "nethttp"} {
		for _, row := range transportRows() {
			for _, raw := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%s", mode, row.name, map[bool]string{false: "call", true: "call-with-raw"}[raw]), func(t *testing.T) {
					t.Setenv(llmhttp.EnvVarClientMode, mode)
					lb := newRawFailureLoopback(t, row.write)
					e, buildBAML := liveOracle(t, lb.baseURL())
					plan, err := buildBAML(context.Background())
					if err != nil {
						t.Fatalf("plan: %v", err)
					}
					newClient := func() *llmhttp.Client {
						return llmhttp.NewDefaultClientWithOptions(llmhttp.ClientOptions{Mode: llmhttp.ClientModeFromEnv()})
					}
					want := callOrdinary(newClient(), plan, row.timeout)

					var captured string
					inv := liveInv(t, buildBAML, `{"weather":"sunny"}`, &captured)
					inv.Raw = raw
					inv.HTTPClient = newClient()
					assertNativeMatchesOrdinary(t, e, lb.hits.Load, inv, want, row.timeout)
				})
			}
		}
	}
}

// TestCallWithOracle_LiveOracle_TransportFollowsTheNegotiatedHTTPSBackend runs the ALPN
// decision for real: an HTTPS origin the ordinary Client's protocol cache probes. An
// HTTP/1.1-only origin negotiates http/1.1 and is served on fasthttp; an HTTP/2 origin is
// served on net/http — and the two report a truncated 429 as different error CLASSES
// (fasthttp's transport "request failed: unexpected EOF" vs net/http's provider *HTTPError
// with the bytes read). The native leg must land in the same class as flag-off on each.
func TestCallWithOracle_LiveOracle_TransportFollowsTheNegotiatedHTTPSBackend(t *testing.T) {
	truncated429 := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if hj, ok := w.(http.Hijacker); ok { // HTTP/1.1: write the exact bytes and close
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			buf.WriteString("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"error\":")
			_ = buf.Flush()
			return
		}
		// HTTP/2 has no hijack: declare more than is sent, then abort the stream.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	for _, tc := range []struct {
		name      string
		http2     bool
		wantClass string
	}{
		{"http/1.1-only origin (fasthttp)", false, "transport"},
		{"http/2 origin (net/http)", true, "provider"},
	} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, raw), func(t *testing.T) {
				t.Setenv(llmhttp.EnvVarClientMode, "auto")
				var hits atomic.Int64
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					truncated429(w, r)
				}))
				srv.EnableHTTP2 = tc.http2
				srv.StartTLS()
				t.Cleanup(srv.Close)
				roots := x509.NewCertPool()
				roots.AddCert(srv.Certificate())
				tlsCfg := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
				newClient := func() *llmhttp.Client {
					return llmhttp.NewClientWithOptions(llmhttp.ClientOptions{
						Mode:          llmhttp.ClientModeAuto,
						NetHTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}},
						FastHTTP:      llmhttp.FastHTTPClientOptions{TLSConfig: tlsCfg},
					})
				}

				e, buildBAML := liveOracle(t, srv.URL+"/v1")
				plan, err := buildBAML(context.Background())
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				want := callOrdinary(newClient(), plan, 0)
				var wantHTTP *llmhttp.HTTPError
				class := "transport"
				if errors.As(want.err, &wantHTTP) {
					class = "provider"
				}
				if want.err == nil || class != tc.wantClass {
					t.Fatalf("ordinary client outcome %+v / %v (class %s), want a %s error — the origin is not exercising the backend it names", want, want.err, class, tc.wantClass)
				}
				var captured string
				inv := liveInv(t, buildBAML, `"never"`, &captured)
				inv.Raw = raw
				inv.HTTPClient = newClient()
				assertNativeMatchesOrdinary(t, e, hits.Load, inv, want, 0)
			})
		}
	}
}

// TestCallWithOracle_LiveOracle_LenientFramingIsServed is the success half of the framing
// rows, asserted on the served bytes: a chunked 2xx whose one chunk is a complete response
// but which closes without the terminating chunk is DELIVERED by the fasthttp backend (auto
// mode, plain http), so the native leg must serve it — native winner, the response's exact
// assistant text as raw — rather than fail where flag-off succeeds.
func TestCallWithOracle_LiveOracle_LenientFramingIsServed(t *testing.T) {
	t.Setenv(llmhttp.EnvVarClientMode, "auto")
	lb := newRawFailureLoopback(t, transportRows()[5].write)
	e, buildBAML := liveOracle(t, lb.baseURL())
	plan, err := buildBAML(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	newClient := func() *llmhttp.Client {
		return llmhttp.NewDefaultClientWithOptions(llmhttp.ClientOptions{Mode: llmhttp.ClientModeAuto})
	}
	if want := callOrdinary(newClient(), plan, 0); want.err != nil {
		t.Fatalf("the fasthttp backend rejected the unterminated chunked body (%v); the row no longer exercises lenient framing", want.err)
	}
	var captured string
	inv := rawInv(t, buildBAML, `{"weather":"sunny"}`, &captured)
	inv.HTTPClient = newClient()
	before := lb.hits.Load()
	res := e.CallWithOracle(context.Background(), inv)
	if res.Disposition != bamlutils.NativeSpineSucceeded || res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("disposition/winner = %v/%q (err %v), want a native win on the delivered response", res.Disposition, res.WinnerEngine, res.Err)
	}
	if lb.hits.Load()-before != 1 {
		t.Errorf("provider hits = %d, want 1", lb.hits.Load()-before)
	}
	if res.Raw != `{"weather":"sunny"}` || captured != `{"weather":"sunny"}` {
		t.Errorf("raw/reference parseable = %q/%q, want the delivered assistant text", res.Raw, captured)
	}
}
