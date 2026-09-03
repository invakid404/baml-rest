//go:build nanollm_integration

package spine

// ExecBridge-U1c CallWithOracle output-contract integration matrix. It drives the REAL
// exact transport + native static SAP + shared same-bytes oracle over a loopback, with
// the admission step injected to the FROZEN claim (admission.AdmitStaticSpineClaim) so the
// live plan compare — proven separately by the cross-boundary staticserve proof and the
// admission unit tests — does not gate reaching the response phase. It asserts the
// CallWithOracle-specific contract: on a structured/order MATCH native's canonical JSON
// wins; on drift the SAME-bytes BAML parse wins; a post-claim provider fault is terminal;
// and exactly ONE socket opens with the atomic counters consistent.

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

// oracleExec builds an executor at baseURL over the jsonalias fixture with admitClaimOracle
// injected to the frozen claim (so CallWithOracle reaches the response phase without the
// live plan compare).
func oracleExec(t *testing.T, baseURL string) *UnaryExecutor {
	t.Helper()
	e, err := NewUnaryExecutor(jsonAliasProjectAt(t, baseURL), []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	e.admitClaimOracle = admission.AdmitStaticSpineClaim
	return e
}

// oracleInv builds a well-formed oracle invocation for the jsonalias method with the two
// mandatory neutral closures. bamlJSON is what the same-bytes BAML parse returns.
func oracleInv(t *testing.T, bamlJSON string) bamlutils.NativeStaticInvocation {
	t.Helper()
	values, err := nativespinejsonfixture.Binding().ProjectInput(&nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("ProjectInput: %v", err)
	}
	return bamlutils.NativeStaticInvocation{
		Method:           nativespinejsonfixture.MethodName,
		Values:           values,
		Provider:         "openai",
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) { return nil, nil },
		BAMLOnlyParse:    func(context.Context, string) ([]byte, error) { return []byte(bamlJSON), nil },
	}
}

func TestCallWithOracle_NativeWinsOnMatch(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONContent(w, `{"weather":"sunny"}`) })
	e := oracleExec(t, lb.baseURL())

	// BAML parse of the SAME bytes yields the SAME structured value/order -> native wins.
	res := e.CallWithOracle(context.Background(), oracleInv(t, `{"weather":"sunny"}`))
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q, want native", res.WinnerEngine)
	}
	if len(res.FinalJSON) == 0 {
		t.Error("succeeded result carries no owned canonical FinalJSON")
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1", lb.hits)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Successes != 1 || snap.Failures != 0 || snap.Declines != 0 {
		t.Errorf("metrics = %+v, want one claim/socket/success", snap)
	}
}

func TestCallWithOracle_BAMLParseWinsOnDrift(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) { okJSONContent(w, `{"weather":"sunny"}`) })
	e := oracleExec(t, lb.baseURL())

	// BAML parse of the SAME bytes yields a DIFFERENT value -> the same-bytes BAML parse
	// wins and its JSON is served, still on exactly ONE provider request.
	res := e.CallWithOracle(context.Background(), oracleInv(t, `{"weather":"rainy"}`))
	if res.Disposition != bamlutils.NativeSpineSucceeded {
		t.Fatalf("disposition = %v (err %v), want succeeded", res.Disposition, res.Err)
	}
	if res.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want native_baml_parse (drift serves the BAML parse)", res.WinnerEngine)
	}
	if string(res.FinalJSON) != `{"weather":"rainy"}` {
		t.Errorf("FinalJSON = %s, want the BAML parse of the drifted bytes", res.FinalJSON)
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1 (no re-send to compare)", lb.hits)
	}
	if snap := e.Metrics().Snapshot(); snap.Sockets != 1 || snap.Successes != 1 {
		t.Errorf("metrics = %+v, want one socket/success", snap)
	}
}

func TestCallWithOracle_ProviderFaultIsTerminal(t *testing.T) {
	lb := newOracleLoopback(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	})
	e := oracleExec(t, lb.baseURL())

	res := e.CallWithOracle(context.Background(), oracleInv(t, `{"weather":"sunny"}`))
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("disposition = %v, want failed_after_claim (post-claim provider fault is terminal)", res.Disposition)
	}
	if res.Err == nil {
		t.Error("failed result carries no typed error")
	}
	if lb.hits != 1 {
		t.Errorf("provider hits = %d, want exactly 1 (no BAML resend after claim)", lb.hits)
	}
	if snap := e.Metrics().Snapshot(); snap.Claims != 1 || snap.Sockets != 1 || snap.Failures != 1 {
		t.Errorf("metrics = %+v, want one claim/socket/failure", snap)
	}
}
