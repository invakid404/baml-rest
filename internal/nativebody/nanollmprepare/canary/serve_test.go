package canary

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/execute"
)

// newTestServer builds a Server over a fresh private registry so a test can read
// the bounded de-BAML collectors. The exact executor is unused by the pure
// disposition mapper (mapAttempt / serveParseOnly / serveStructured never dial),
// so a default one is fine.
func newTestServer(t *testing.T) (*Server, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	return NewServer(m, llmhttp.NewExactExecutor(nil)), reg
}

// sumAttempts sums baml_rest_debaml_attempts_total for one outcome label.
func sumAttempts(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	return sumCounter(t, reg, "baml_rest_debaml_attempts_total", "outcome", outcome)
}

// sumFallback sums baml_rest_debaml_fallback_total for one kind label.
func sumFallback(t *testing.T, reg *prometheus.Registry, kind string) float64 {
	return sumCounter(t, reg, "baml_rest_debaml_fallback_total", "kind", kind)
}

func sumCounter(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					sum += mm.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

func openaiBody(content string) []byte {
	return []byte(`{"choices":[{"message":{"role":"assistant","content":` + jsonQuote(content) + `}}]}`)
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestMapAttempt_ProviderNon2xx proves a provider non-2xx maps to *llmhttp.HTTPError
// with the REAL status and the body capped to the 4 KiB PUBLIC cap (not the 64 KiB
// internal cap), recorded as provider_error. SAP is never involved.
func TestMapAttempt_ProviderNon2xx(t *testing.T) {
	s, reg := newTestServer(t)
	bigBody := bytes.Repeat([]byte("x"), 5000) // > 4 KiB public cap
	res := &execute.AttemptResult{
		Outcome:        execute.OutcomeProviderError,
		ProviderStatus: 429,
		ProviderBody:   bigBody,
	}
	out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, res, nil)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	var httpErr *llmhttp.HTTPError
	if !errors.As(out.Err, &httpErr) {
		t.Fatalf("err = %v, want *llmhttp.HTTPError", out.Err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("status = %d, want 429", httpErr.StatusCode)
	}
	if len(httpErr.Body) != llmhttp.MaxErrorBodyBytes {
		t.Errorf("body len = %d, want the 4 KiB public cap %d", len(httpErr.Body), llmhttp.MaxErrorBodyBytes)
	}
	if sumAttempts(t, reg, "provider_error") != 1 {
		t.Errorf("provider_error attempts = %v, want 1", sumAttempts(t, reg, "provider_error"))
	}
}

// TestMapAttempt_MalformedBody proves a malformed provider 2xx maps to today's
// extraction error class (NOT an *HTTPError, NOT ErrOutputParse — so it becomes
// worker_error, never nanollm's synthesized 502) with the raw upstream body
// retained as details.raw. Recorded as translate_error.
func TestMapAttempt_MalformedBody(t *testing.T) {
	s, reg := newTestServer(t)
	res := &execute.AttemptResult{
		Outcome:        execute.OutcomeInvalidBody,
		ProviderStatus: 200,
		ProviderBody:   []byte("not json at all"),
	}
	out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, res, nil)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	var httpErr *llmhttp.HTTPError
	if errors.As(out.Err, &httpErr) {
		t.Errorf("malformed 2xx must NOT surface as an HTTPError/502, got %v", out.Err)
	}
	if errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("malformed 2xx must NOT be a parse_error, got %v", out.Err)
	}
	if out.RawDiagnostic != "not json at all" {
		t.Errorf("details.raw = %q, want the raw upstream body", out.RawDiagnostic)
	}
	if sumAttempts(t, reg, "translate_error") != 1 {
		t.Errorf("translate_error attempts = %v, want 1", sumAttempts(t, reg, "translate_error"))
	}
}

// TestMapAttempt_TranslateError proves a post-response translate/extract failure
// (aerr != nil, SAPInvoked == false) maps to the extraction error class with the
// raw upstream body retained. Recorded as translate_error.
func TestMapAttempt_TranslateError(t *testing.T) {
	s, reg := newTestServer(t)
	res := &execute.AttemptResult{ProviderStatus: 200, ProviderBody: []byte("upstream body")}
	out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, res, errors.New("translate boom"))

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	if errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("translate error must NOT be a parse_error, got %v", out.Err)
	}
	if out.RawDiagnostic != "upstream body" {
		t.Errorf("details.raw = %q, want the raw upstream body", out.RawDiagnostic)
	}
	if sumAttempts(t, reg, "translate_error") != 1 {
		t.Errorf("translate_error attempts = %v, want 1", sumAttempts(t, reg, "translate_error"))
	}
}

// TestMapAttempt_ClaimedSAPError proves a CLAIMED native SAP parse failure
// (aerr != nil, SAPInvoked == true) maps to ErrOutputParse with the extracted
// assistant text as details.raw. Recorded as parse_error.
func TestMapAttempt_ClaimedSAPError(t *testing.T) {
	s, reg := newTestServer(t)
	res := &execute.AttemptResult{
		ProviderStatus: 200,
		ProviderBody:   []byte("openai body"),
		AssistantText:  "assistant text",
		Raw:            "assistant text",
		SAPInvoked:     true,
	}
	out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, res, errors.New("claimed parse failure"))

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	if !errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("claimed SAP error must be a parse_error, got %v", out.Err)
	}
	if out.RawDiagnostic != "assistant text" {
		t.Errorf("details.raw = %q, want the extracted assistant text", out.RawDiagnostic)
	}
	if sumAttempts(t, reg, "parse_error") != 1 {
		t.Errorf("parse_error attempts = %v, want 1", sumAttempts(t, reg, "parse_error"))
	}
}

// TestMapAttempt_TransportError proves a transport failure (no response context)
// is handed to the outer policy as the typed error UNCHANGED (errors.Is preserved)
// with no raw. Recorded as transport_error.
func TestMapAttempt_TransportError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"cancel", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"flake", &llmhttp.TransportError{Category: llmhttp.TransportFlakeConnectionRefused, Prefix: "llmhttp: exact roundtrip failed", Underlying: errors.New("connection refused")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reg := newTestServer(t)
			out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, nil, tc.err)
			if out.Disposition != bamlutils.NativeServeFailed {
				t.Fatalf("disposition = %v, want failed", out.Disposition)
			}
			if !errors.Is(out.Err, tc.err) {
				t.Errorf("transport error must be preserved for the outer policy, got %v", out.Err)
			}
			if out.RawDiagnostic != "" {
				t.Errorf("transport error carries no body, got raw %q", out.RawDiagnostic)
			}
			if sumAttempts(t, reg, "transport_error") != 1 {
				t.Errorf("transport_error attempts = %v, want 1", sumAttempts(t, reg, "transport_error"))
			}
		})
	}
}

// TestServe_EntryCancellationDeclines proves the pre-admission cancellation gate:
// a request already cancelled at entry declines to BAML with ZERO native work —
// no admission FFI (New/render/Prepare), no claim, no socket — so the orchestrator
// runs the ordinary BAML attempt once (which then surfaces the context error). No
// native socket is recorded.
func TestServe_EntryCancellationDeclines(t *testing.T) {
	s, reg := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := s.Serve(ctx, bamlutils.NativeServeRequest{Provider: "openai", Mode: bamlutils.NativeServeModeCall})
	if out.Disposition != bamlutils.NativeServeDeclined {
		t.Fatalf("disposition = %v, want declined (pre-socket, zero native work)", out.Disposition)
	}
	if out.Stage != stageServe || out.Reason != reasonServedBAMLCtx {
		t.Errorf("decline = (%s,%s), want (%s,%s)", out.Stage, out.Reason, stageServe, reasonServedBAMLCtx)
	}
	if got := sumCounter(t, reg, "baml_rest_debaml_native_sockets_total", "flag", "on"); got != 0 {
		t.Errorf("native_sockets{flag=on} = %v, want 0 (no claim on a pre-socket decline)", got)
	}
}

// TestMapAttempt_ContextCancelledIsTransport proves cancellation racing the
// post-2xx translate/SAP pipeline (aerr is a context error WITH a non-nil response
// context) is still returned as the typed transport-class error — errors.Is holds
// — and is NEVER misclassified as a translate/parse failure. It is a FailNativeCall
// to the outer policy (no hidden resend), recorded as transport_error.
func TestMapAttempt_ContextCancelledIsTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		res     *execute.AttemptResult
		wantErr error
	}{
		{"cancel post-response", context.Canceled, &execute.AttemptResult{ProviderStatus: 200, ProviderBody: []byte("body"), AssistantText: "x", SAPInvoked: true}, context.Canceled},
		{"deadline post-response", context.DeadlineExceeded, &execute.AttemptResult{ProviderStatus: 200, ProviderBody: []byte("body")}, context.DeadlineExceeded},
		{"cancel pre-response", context.Canceled, nil, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reg := newTestServer(t)
			out := s.mapAttempt(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, tc.res, tc.err)
			if out.Disposition != bamlutils.NativeServeFailed {
				t.Fatalf("disposition = %v, want failed", out.Disposition)
			}
			if !errors.Is(out.Err, tc.wantErr) {
				t.Errorf("err = %v, want errors.Is(%v) preserved for the outer policy", out.Err, tc.wantErr)
			}
			if out.RawDiagnostic != "" {
				t.Errorf("cancellation carries no details.raw, got %q", out.RawDiagnostic)
			}
			if got := sumAttempts(t, reg, "transport_error"); got != 1 {
				t.Errorf("transport_error attempts = %v, want 1 (not translate/parse_error)", got)
			}
			// Not misclassified as a parse error even though SAPInvoked may be true.
			if got := sumAttempts(t, reg, "parse_error"); got != 0 {
				t.Errorf("parse_error attempts = %v, want 0 (cancellation is transport-class)", got)
			}
		})
	}
}

// TestServeParseOnly_ServesBAMLParse proves a native SAP decline runs BAML
// parse-only on the SAME extracted assistant text and serves that final with
// winner_engine=native_baml_parse, recording parse_decline + fallback{parse_only}.
func TestServeParseOnly_ServesBAMLParse(t *testing.T) {
	s, reg := newTestServer(t)
	bamlFlat := []byte(`{"name":"Ada"}`)
	req := bamlutils.NativeServeRequest{
		Provider: "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			return bamlFlat, nil
		},
	}
	res := &execute.AttemptResult{
		Outcome:       execute.OutcomeParseDeclined,
		AssistantText: "assistant text",
		Raw:           "assistant text",
	}
	out := s.serveParseOnly(context.Background(), req, res)

	if out.Disposition != bamlutils.NativeServeSucceeded {
		t.Fatalf("disposition = %v, want succeeded", out.Disposition)
	}
	if out.WinnerEngine != bamlutils.NativeServeEngineBAMLParse {
		t.Errorf("winner_engine = %q, want native_baml_parse", out.WinnerEngine)
	}
	if !bytes.Equal(out.FinalJSON, bamlFlat) {
		t.Errorf("final = %s, want the BAML parse-only output", out.FinalJSON)
	}
	if sumAttempts(t, reg, "parse_decline") != 1 {
		t.Errorf("parse_decline attempts = %v, want 1", sumAttempts(t, reg, "parse_decline"))
	}
	if sumFallback(t, reg, "parse_only") != 1 {
		t.Errorf("fallback parse_only = %v, want 1", sumFallback(t, reg, "parse_only"))
	}
}

// TestServeParseOnly_BAMLParseErrors proves that when native SAP declined AND the
// BAML parse-only fallback itself errors, the attempt FAILS with ErrOutputParse +
// raw (parse_error) — never a hidden resend, never a served unverified result.
func TestServeParseOnly_BAMLParseErrors(t *testing.T) {
	s, reg := newTestServer(t)
	req := bamlutils.NativeServeRequest{
		Provider: "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			return nil, errors.New("baml parse boom")
		},
	}
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, AssistantText: "x", Raw: "raw text"}
	out := s.serveParseOnly(context.Background(), req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	if !errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("err = %v, want ErrOutputParse", out.Err)
	}
	if out.RawDiagnostic != "raw text" {
		t.Errorf("details.raw = %q, want the extracted raw", out.RawDiagnostic)
	}
	if sumAttempts(t, reg, "parse_error") != 1 {
		t.Errorf("parse_error attempts = %v, want 1", sumAttempts(t, reg, "parse_error"))
	}
}

// TestServeParseOnly_NoBAMLBuilder proves a missing BAML-only parse closure (a
// wiring bug) FAILS post-claim rather than serving an unverified native result.
func TestServeParseOnly_NoBAMLBuilder(t *testing.T) {
	s, _ := newTestServer(t)
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, AssistantText: "x", Raw: "raw"}
	out := s.serveParseOnly(context.Background(), bamlutils.NativeServeRequest{Provider: "openai"}, res)
	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed (never serve unverified)", out.Disposition)
	}
	if !errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("err = %v, want ErrOutputParse", out.Err)
	}
}

// TestServeStructured_BAMLParseRejects proves the BAML-as-current compatibility
// rule: when native SAP claims cleanly but BAML's parse of the SAME bytes errors,
// the attempt returns the parse error (never serves a native final BAML would
// have rejected).
func TestServeStructured_BAMLParseRejects(t *testing.T) {
	s, reg := newTestServer(t)
	req := bamlutils.NativeServeRequest{
		Provider: "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			return nil, errors.New("baml would reject this")
		},
	}
	res := &execute.AttemptResult{
		Outcome:       execute.OutcomeStructured,
		ProviderBody:  openaiBody(`{"name":"Ada"}`),
		AssistantText: `{"name":"Ada"}`,
		Raw:           `{"name":"Ada"}`,
		Structured:    []byte(`{"name":"Ada"}`),
	}
	out := s.serveStructured(context.Background(), req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed (BAML rejects -> reject)", out.Disposition)
	}
	if !errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("err = %v, want ErrOutputParse", out.Err)
	}
	if sumAttempts(t, reg, "parse_error") != 1 {
		t.Errorf("parse_error attempts = %v, want 1", sumAttempts(t, reg, "parse_error"))
	}
}

// TestServeStructured_BAMLParseContextCancelled proves the structured-path
// consistency fix: a caller cancellation racing the S5 BAML parse is handled as a
// transport-class typed error (matching mapAttempt) — recorded transport_error,
// the ORIGINAL context error returned UNCHANGED (NOT wrapped as ErrOutputParse),
// with NO details.raw — never a parse_error, never a hidden resend.
func TestServeStructured_BAMLParseContextCancelled(t *testing.T) {
	s, reg := newTestServer(t)
	req := bamlutils.NativeServeRequest{
		Provider:      "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) { return nil, context.Canceled },
	}
	res := &execute.AttemptResult{
		Outcome:       execute.OutcomeStructured,
		ProviderBody:  openaiBody(`{"name":"Ada"}`),
		AssistantText: `{"name":"Ada"}`,
		Raw:           `{"name":"Ada"}`,
		Structured:    []byte(`{"name":"Ada"}`),
	}
	out := s.serveStructured(context.Background(), req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Errorf("err = %v, want the context.Canceled preserved for the outer policy", out.Err)
	}
	if errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("a cancellation must NOT be wrapped as ErrOutputParse, got %v", out.Err)
	}
	if out.RawDiagnostic != "" {
		t.Errorf("cancellation carries no details.raw, got %q", out.RawDiagnostic)
	}
	if got := sumAttempts(t, reg, "transport_error"); got != 1 {
		t.Errorf("transport_error attempts = %v, want 1", got)
	}
	if got := sumAttempts(t, reg, "parse_error"); got != 0 {
		t.Errorf("parse_error attempts = %v, want 0 (a cancellation is transport-class)", got)
	}
}

// TestServeParseOnly_BAMLParseContextDeadline proves the same consistency fix on
// the parse-only path: a deadline racing the BAML parse-only fallback maps to
// transport_error + the typed context error (no ErrOutputParse wrap, no
// details.raw), not a parse_error.
func TestServeParseOnly_BAMLParseContextDeadline(t *testing.T) {
	s, reg := newTestServer(t)
	req := bamlutils.NativeServeRequest{
		Provider:      "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded },
	}
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, AssistantText: "x", Raw: "raw text"}
	out := s.serveParseOnly(context.Background(), req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed", out.Disposition)
	}
	if !errors.Is(out.Err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded preserved", out.Err)
	}
	if errors.Is(out.Err, buildrequest.ErrOutputParse) {
		t.Errorf("a deadline must NOT be wrapped as ErrOutputParse, got %v", out.Err)
	}
	if out.RawDiagnostic != "" {
		t.Errorf("deadline carries no details.raw, got %q", out.RawDiagnostic)
	}
	if got := sumAttempts(t, reg, "transport_error"); got != 1 {
		t.Errorf("transport_error attempts = %v, want 1", got)
	}
	if got := sumAttempts(t, reg, "parse_error"); got != 0 {
		t.Errorf("parse_error attempts = %v, want 0", got)
	}
}

// TestServeStructured_CanceledCtxNotServedDespiteValidParse proves the after-gate:
// a BAMLOnlyParse callback that IGNORES cancellation and returns VALID JSON
// (err==nil) on a canceled context must NOT be served. The post-parse ctx.Err()
// gate maps it to transport_error + the typed context error (no raw, no
// serve/native final), never a served result.
func TestServeStructured_CanceledCtxNotServedDespiteValidParse(t *testing.T) {
	s, reg := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := bamlutils.NativeServeRequest{
		Provider: "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			cancel()                             // the request got canceled; a naive callback ignores it...
			return []byte(`{"name":"Ada"}`), nil // ...and returns a VALID result
		},
	}
	res := &execute.AttemptResult{
		Outcome:       execute.OutcomeStructured,
		ProviderBody:  openaiBody(`{"name":"Ada"}`),
		AssistantText: `{"name":"Ada"}`,
		Raw:           `{"name":"Ada"}`,
		Structured:    []byte(`{"name":"Ada"}`),
	}
	out := s.serveStructured(ctx, req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed (a canceled request must not be served)", out.Disposition)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled preserved", out.Err)
	}
	if out.FinalJSON != nil {
		t.Errorf("FinalJSON = %s, want nil (no native/parse final served on a canceled ctx)", out.FinalJSON)
	}
	if out.RawDiagnostic != "" {
		t.Errorf("cancellation carries no details.raw, got %q", out.RawDiagnostic)
	}
	if got := sumAttempts(t, reg, "transport_error"); got != 1 {
		t.Errorf("transport_error attempts = %v, want 1", got)
	}
	if got := sumAttempts(t, reg, "success"); got != 0 {
		t.Errorf("success attempts = %v, want 0 (never serve a canceled request)", got)
	}
}

// TestServeParseOnly_CanceledCtxNotServedDespiteValidParse proves the same after-
// gate on the parse-only path: a valid BAMLOnlyParse result on a canceled context
// is not served — transport_error + typed ctx error, no final, no raw.
func TestServeParseOnly_CanceledCtxNotServedDespiteValidParse(t *testing.T) {
	s, reg := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := bamlutils.NativeServeRequest{
		Provider: "openai",
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			cancel()
			return []byte(`{"name":"Ada"}`), nil
		},
	}
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, AssistantText: "x", Raw: "raw text"}
	out := s.serveParseOnly(ctx, req, res)

	if out.Disposition != bamlutils.NativeServeFailed {
		t.Fatalf("disposition = %v, want failed (a canceled request must not be served)", out.Disposition)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled preserved", out.Err)
	}
	if out.FinalJSON != nil {
		t.Errorf("FinalJSON = %s, want nil (no parse-only final served on a canceled ctx)", out.FinalJSON)
	}
	if out.RawDiagnostic != "" {
		t.Errorf("cancellation carries no details.raw, got %q", out.RawDiagnostic)
	}
	if got := sumAttempts(t, reg, "transport_error"); got != 1 {
		t.Errorf("transport_error attempts = %v, want 1", got)
	}
	if got := sumAttempts(t, reg, "parse_decline"); got != 0 {
		t.Errorf("parse_decline attempts = %v, want 0 (never serve a canceled request)", got)
	}
}
