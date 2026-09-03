//go:build nanollm_integration

package spine

// ExecBridge-U1c CallWithOracle LIVE-oracle output-contract matrix (review P1-2). Unlike a
// frozen-claim stand-in, this drives the REAL AdmitStaticSpineOracleClaim: it supplies a
// BuildBAMLRequest that byte-matches the executor's OWN native prepared plan (so the live
// plan compare MATCHES and the attempt claims), CAPTURES the BAMLOnlyParse argument
// byte-for-byte (proving BAML parses the exact assistant bytes the ONE provider request
// returned, never a re-extraction), and counts every provider send. It asserts the
// load-bearing sequence: live plan match -> claim -> one send -> same-bytes BAML parse ->
// native/drift winner, and a post-claim provider fault -> terminal, no resend.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// oracleLoopback stands a loopback provider up and returns its /v1 base URL + a hit
// counter.
type oracleLoopback struct {
	srv  *httptest.Server
	hits int
}

func newOracleLoopback(t *testing.T, handler http.HandlerFunc) *oracleLoopback {
	t.Helper()
	lb := &oracleLoopback{}
	lb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lb.hits++
		handler(w, r)
	}))
	t.Cleanup(lb.srv.Close)
	return lb
}

func (lb *oracleLoopback) baseURL() string { return lb.srv.URL + "/v1" }

// okJSONContent writes an OpenAI-shaped 2xx whose assistant content is content (verbatim).
func okJSONContent(w http.ResponseWriter, content string) {
	env, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(env)
}

// liveOracle builds a real executor at baseURL (its admitClaimOracle is the production
// AdmitStaticSpineOracleClaim), plus a BuildBAMLRequest that byte-matches the executor's OWN
// native prepared plan. The plan is captured through the FROZEN admission entry (which opens
// no socket and does not run the compare), then handed back as BAML's no-send plan — so the
// LIVE compare inside CallWithOracle byte-matches and the attempt claims.
func liveOracle(t *testing.T, baseURL string) (*UnaryExecutor, func(context.Context) (*llmhttp.Request, error)) {
	t.Helper()
	e, err := NewUnaryExecutor(jsonAliasProjectAt(t, baseURL), []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	values, err := nativespinejsonfixture.Binding().ProjectInput(&nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("ProjectInput: %v", err)
	}
	// Build the exact admission input CallWithOracle will build, then capture the native plan
	// via the frozen claim (no compare, no socket).
	rm := e.registry[nativespinejsonfixture.MethodName]
	if rm == nil {
		t.Fatal("the jsonalias method is not registered")
	}
	staticIn := e.oracleStaticInput(rm, bamlutils.NativeStaticInvocation{
		Method: nativespinejsonfixture.MethodName, Values: values,
		Mode: bamlutils.NativeStaticModeFinal, Provider: "openai", SingleLeaf: true,
	})
	frozen, err := admission.AdmitStaticSpineClaim(context.Background(), staticIn)
	if err != nil {
		t.Fatalf("capture the native plan via the frozen claim: %v", err)
	}
	prep := frozen.Prepared
	hdr := map[string]string{}
	for _, p := range prep.Headers {
		hdr[p[0]] = p[1]
	}
	bamlReq := &llmhttp.Request{Method: prep.Method, URL: prep.URL, Headers: hdr, Body: string(prep.Body)}
	frozen.Close()
	return e, func(context.Context) (*llmhttp.Request, error) { return bamlReq, nil }
}

// liveInv builds a well-formed live-oracle invocation: the projected values, the matching
// BuildBAMLRequest, and a BAMLOnlyParse that RECORDS the exact bytes it is handed (into
// *captured) before returning bamlJSON.
func liveInv(t *testing.T, buildBAML func(context.Context) (*llmhttp.Request, error), bamlJSON string, captured *string) bamlutils.NativeStaticInvocation {
	t.Helper()
	values, err := nativespinejsonfixture.Binding().ProjectInput(&nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("ProjectInput: %v", err)
	}
	return bamlutils.NativeStaticInvocation{
		Method:           nativespinejsonfixture.MethodName,
		Values:           values,
		Mode:             bamlutils.NativeStaticModeFinal,
		Provider:         "openai",
		SingleLeaf:       true,
		BuildBAMLRequest: buildBAML,
		BAMLOnlyParse: func(_ context.Context, raw string) ([]byte, error) {
			*captured = raw
			return []byte(bamlJSON), nil
		},
	}
}

func TestCallWithOracle_LiveOracle_NativeWinsAndParsesExactBytes(t *testing.T) {
	const content = `{"weather":"sunny"}`
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONContent(w, content) })
	e, buildBAML := liveOracle(t, lb.baseURL())

	var captured string
	// BAML parse of the SAME bytes yields the SAME structured value/order -> native wins.
	res := e.CallWithOracle(context.Background(), liveInv(t, buildBAML, content, &captured))
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v, stage %q reason %q), want succeeded — the live plan must match and claim", res.Disposition, res.Err, res.Stage, res.Reason)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q, want native", res.WinnerEngine)
	}
	if captured != content {
		t.Errorf("BAMLOnlyParse was handed %q, want the exact assistant bytes %q (not a re-extraction)", captured, content)
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1", lb.hits)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 || snap.Failures != 0 || snap.Declines != 0 {
		t.Errorf("metrics = %+v, want one claim/socket/success (the live plan matched)", snap)
	}
	// The observations the composite replays confirm a matched plan + one responded socket.
	if o := res.Observations; !o.PlanCompareRan || !o.PlanMatched || !o.SocketOpened || !o.SocketResponded || !o.SameResponseOracleRan {
		t.Errorf("observations = %+v, want plan-matched + one responded socket + same-response oracle", o)
	}
}

func TestCallWithOracle_LiveOracle_BAMLParseWinsOnDrift(t *testing.T) {
	const content = `{"weather":"sunny"}`
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONContent(w, content) })
	e, buildBAML := liveOracle(t, lb.baseURL())

	var captured string
	// BAML parse of the SAME bytes yields a DIFFERENT value -> the same-bytes BAML parse wins,
	// still on exactly ONE provider request.
	res := e.CallWithOracle(context.Background(), liveInv(t, buildBAML, `{"weather":"rainy"}`, &captured))
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want native_baml_parse (drift serves the same-bytes BAML parse)", res.WinnerEngine)
	}
	if string(res.FinalJSON) != `{"weather":"rainy"}` {
		t.Errorf("FinalJSON = %s, want the BAML parse of the drifted bytes", res.FinalJSON)
	}
	if captured != content {
		t.Errorf("BAMLOnlyParse was handed %q, want the exact assistant bytes %q", captured, content)
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1 (no re-send to compare)", lb.hits)
	}
	if !res.Observations.Fallback {
		t.Errorf("drift did not record the fallback observation: %+v", res.Observations)
	}
}

func TestCallWithOracle_LiveOracle_ProviderFaultIsTerminal(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	e, buildBAML := liveOracle(t, lb.baseURL())

	var captured string
	res := e.CallWithOracle(context.Background(), liveInv(t, buildBAML, `{"weather":"sunny"}`, &captured))
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim (post-claim provider fault is terminal)", res.Disposition)
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1 (no BAML resend after claim)", lb.hits)
	}
	if captured != "" {
		t.Errorf("BAMLOnlyParse ran on a provider fault (captured %q); the same-bytes oracle must not run when there is no 2xx body", captured)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Failures != 1 {
		t.Errorf("metrics = %+v, want one claim/socket/failure", snap)
	}
}

// TestCallWithOracle_LiveOracle_PlanMismatchDeclines proves the live compare bites: a
// BuildBAMLRequest whose plan does NOT match the native plan declines PRE-SOCKET (zero
// sockets), so the outer composite falls back to BAML.
func TestCallWithOracle_LiveOracle_PlanMismatchDeclines(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONContent(w, `{"weather":"sunny"}`) })
	e, _ := liveOracle(t, lb.baseURL())

	var captured string
	// A deliberately-mismatching BAML plan (different URL) -> plan mismatch -> decline.
	mismatch := func(context.Context) (*llmhttp.Request, error) {
		return &llmhttp.Request{Method: "POST", URL: "http://127.0.0.1:1/v1/chat/completions", Body: "{}"}, nil
	}
	res := e.CallWithOracle(context.Background(), liveInv(t, mismatch, `{"weather":"sunny"}`, &captured))
	if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
		t.Fatalf("disposition = %v, want declined_pre_socket on a plan mismatch", res.Disposition)
	}
	if lb.hits != 0 {
		t.Errorf("provider hits = %d, want 0 (a plan mismatch declines pre-socket)", lb.hits)
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
		t.Errorf("metrics = %+v, want zero sockets/claims", snap)
	}
	if !res.Observations.PlanCompareRan || res.Observations.PlanMatched {
		t.Errorf("observations = %+v, want plan compare ran + not matched", res.Observations)
	}
}
