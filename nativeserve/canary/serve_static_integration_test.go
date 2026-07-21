//go:build nanollm_integration

package canary

// Gated (real FFI + one loopback socket) proof of the de-BAML Slice 8C static SERVE
// ownership contract through the PUBLIC ServeStatic entrypoint:
//
//   - EXACT ONE-SEND: a CLAIMED static /call sends exactly one provider RoundTrip.
//   - S5 same-response BAML compare: on a structured MATCH native wins (winner=native);
//     on drift the BAML parse of the SAME bytes wins (winner=native_baml_parse) — still
//     exactly one native send, never a resend.
//   - TRI-STATE PRE-CLAIM decline: a pre-claim decline sends ZERO native sockets and
//     returns NativeStaticServeDeclined so BAML serves.
//
// The claim is SYNTHETIC (admission.AdmitStaticClaimForTest, injected through the
// test-only Server.staticAdmitClaim seam) so the post-claim pipeline is exercised
// without a live BAML plan oracle; BAMLOnlyParse is a stub (the same-response
// comparator/fallback), never a second provider request.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

const staticFenceAPIKey = "sk-static-fence-not-a-real-secret"

// staticAnswerBundle lowers the flat StaticAnswer{answer:string, confidence:int}
// return — the 8C admitted class shape.
func staticAnswerBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	b, err := schema.FromStaticDescriptor(schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticOutputFormat",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "StaticAnswer", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "StaticAnswer"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "answer"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "confidence"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	return b
}

// staticServeServer builds a Server whose exact executor sends the claimed plan to a
// loopback capture server returning an OpenAI-shaped 2xx whose assistant content is
// the flattened StaticAnswer JSON, and a synthetic static claim over that loopback.
func staticServeServer(t *testing.T, hits *atomic.Int64) (*Server, *schema.Bundle) {
	t.Helper()
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"ok\",\"confidence\":7}"}}]}`))
	}))
	t.Cleanup(cs.Close)

	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	s := NewServer(m, llmhttp.NewExactExecutor(&http.Transport{DisableKeepAlives: true}))

	bundle := staticAnswerBundle(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	claim, err := admission.AdmitStaticClaimForTest(cs.URL+"/v1", staticFenceAPIKey, "__static_serve_alias__", "gpt-4o-mini", bundle, body)
	if err != nil {
		t.Fatalf("AdmitStaticClaimForTest: %v", err)
	}
	s.staticAdmitClaim = func(context.Context, admission.StaticInput) (*admission.StaticClaim, error) { return claim, nil }
	return s, bundle
}

// TestServeStatic_OneSend_NativeWins proves a claimed static /call sends EXACTLY one
// provider RoundTrip and, on a same-response BAML MATCH, serves the native result.
func TestServeStatic_OneSend_NativeWins(t *testing.T) {
	var hits atomic.Int64
	s, _ := staticServeServer(t, &hits)

	inv := bamlutils.NativeStaticInvocation{
		Method:   "StaticOutputFormat",
		Provider: "openai",
		Mode:     bamlutils.NativeStaticModeFinal,
		// BAML parse of the SAME bytes yields the identical canonical StaticAnswer JSON
		// -> structured+order match -> native wins. It NEVER opens a socket.
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			return []byte(`{"answer":"ok","confidence":7}`), nil
		},
	}
	out := s.ServeStatic(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStaticServeSucceeded {
		t.Fatalf("disposition = %v (err=%v), want succeeded", out.Disposition, out.Err)
	}
	if out.WinnerEngine != bamlutils.NativeStaticServeEngineNative {
		t.Errorf("winner = %q, want native", out.WinnerEngine)
	}
	if string(out.FinalJSON) != `{"answer":"ok","confidence":7}` {
		t.Errorf("final = %q, want the native SAP structured output", string(out.FinalJSON))
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("capture server saw %d requests, want EXACTLY 1 native socket", got)
	}
}

// TestServeStatic_OneSend_BAMLParseWinsOnDrift proves that when native structured
// output DRIFTS from BAML's parse of the SAME bytes, the BAML parse is served for
// safety (winner=native_baml_parse) — still EXACTLY one native send, never a resend.
func TestServeStatic_OneSend_BAMLParseWinsOnDrift(t *testing.T) {
	var hits atomic.Int64
	s, _ := staticServeServer(t, &hits)

	inv := bamlutils.NativeStaticInvocation{
		Method:   "StaticOutputFormat",
		Provider: "openai",
		Mode:     bamlutils.NativeStaticModeFinal,
		// BAML parse yields a DIFFERENT value than native SAP -> structured mismatch ->
		// serve BAML's parse of the same bytes (still one native send).
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			return []byte(`{"answer":"different","confidence":7}`), nil
		},
	}
	out := s.ServeStatic(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStaticServeSucceeded {
		t.Fatalf("disposition = %v (err=%v), want succeeded", out.Disposition, out.Err)
	}
	if out.WinnerEngine != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Errorf("winner = %q, want native_baml_parse (drift serves BAML parse)", out.WinnerEngine)
	}
	if string(out.FinalJSON) != `{"answer":"different","confidence":7}` {
		t.Errorf("final = %q, want BAML's parse of the same bytes", string(out.FinalJSON))
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("capture server saw %d requests, want EXACTLY 1 native socket even on drift", got)
	}
}

// TestServeStatic_PreClaimDeclineZeroSend proves a pre-claim decline sends ZERO
// native sockets and returns NativeStaticServeDeclined so BAML serves — the tri-state
// pre-claim boundary.
func TestServeStatic_PreClaimDeclineZeroSend(t *testing.T) {
	var hits atomic.Int64
	s, _ := staticServeServer(t, &hits)
	// Override the claim to a pre-claim decline: NO socket may open.
	s.staticAdmitClaim = func(context.Context, admission.StaticInput) (*admission.StaticClaim, error) {
		return nil, &admission.StaticDecline{Stage: "strategy", Reason: "client_override_unproven"}
	}

	inv := bamlutils.NativeStaticInvocation{
		Method:   "StaticOutputFormat",
		Provider: "openai",
		Mode:     bamlutils.NativeStaticModeFinal,
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			panic("BAMLOnlyParse must NOT run on a pre-claim decline")
		},
	}
	out := s.ServeStatic(context.Background(), inv)

	if out.Disposition != bamlutils.NativeStaticServeDeclined {
		t.Fatalf("disposition = %v, want declined", out.Disposition)
	}
	if out.Reason != "client_override_unproven" {
		t.Errorf("reason = %q, want the forwarded decline reason", out.Reason)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("capture server saw %d requests, want ZERO native sockets on a pre-claim decline", got)
	}
}
