package staticoracle

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

// parse returns a BAMLOnlyParse closure yielding out (and recording the raw it received).
func parse(out string, captured *string) BAMLOnlyParse {
	return func(_ context.Context, raw string) ([]byte, error) {
		if captured != nil {
			*captured = raw
		}
		return []byte(out), nil
	}
}

// TestResolve_StructuredNativeWin: a clean structured claim whose same-bytes BAML parse
// matches serves native.
func TestResolve_StructuredNativeWin(t *testing.T) {
	var captured string
	res := &execute.AttemptResult{Outcome: execute.OutcomeStructured, Structured: []byte(`{"k":1}`), AssistantText: `{"k":1}`}
	entered := false
	r := Resolve(context.Background(), nil, res, nil, parse(`{"k":1}`, &captured), func() { entered = true })
	if !entered {
		t.Error("onStructuredOracle hook was not called before the parser")
	}
	if !r.Served || r.Winner != bamlutils.NativeStaticServeEngineNative {
		t.Fatalf("served=%v winner=%q, want native win", r.Served, r.Winner)
	}
	if captured != `{"k":1}` {
		t.Errorf("parser got %q, want the assistant text %q", captured, `{"k":1}`)
	}
	if !r.SameResponseOracleRan || !r.StructuredBranchServed || !r.StructuredMatch || !r.OrderMatch {
		t.Errorf("facets = %+v, want structured/order match", r)
	}
	if string(r.FinalJSON) != `{"k":1}` {
		t.Errorf("FinalJSON = %s, want native structured", r.FinalJSON)
	}
}

// TestResolve_DriftServesBAMLParse: a structured/order drift serves the same-bytes BAML parse.
func TestResolve_DriftServesBAMLParse(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.OutcomeStructured, Structured: []byte(`{"k":1}`), AssistantText: `{"k":1}`}
	r := Resolve(context.Background(), nil, res, nil, parse(`{"k":2}`, nil), nil)
	if !r.Served || r.Winner != bamlutils.NativeStaticServeEngineBAMLParse {
		t.Fatalf("served=%v winner=%q, want BAML-parse win", r.Served, r.Winner)
	}
	if !r.Fallback || string(r.FinalJSON) != `{"k":2}` {
		t.Errorf("fallback=%v FinalJSON=%s, want the BAML parse of the drifted bytes", r.Fallback, r.FinalJSON)
	}
}

// TestResolve_ParseDeclineServesBAMLParse: a native SAP decline serves the same-bytes BAML
// parse, WITHOUT entering the structured same-response phase.
func TestResolve_ParseDeclineServesBAMLParse(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.OutcomeParseDeclined, AssistantText: `hello`}
	entered := false
	r := Resolve(context.Background(), nil, res, nil, parse(`"hello"`, nil), func() { entered = true })
	if entered {
		t.Error("onStructuredOracle fired on a parse-decline; the phase belongs to the structured branch only")
	}
	if !r.Served || r.Winner != bamlutils.NativeStaticServeEngineBAMLParse || !r.ParseDeclineServed || r.SameResponseOracleRan {
		t.Fatalf("result = %+v, want a parse-decline BAML-parse win with no same-response phase", r)
	}
}

// TestResolve_HookFiresBeforeParserPanic pins P2-6: onStructuredOracle is invoked BEFORE
// bamlOnlyParse, so a parser PANIC (which unwinds without Resolve returning) still lets the
// caller record the same-response phase — the loss master avoided.
func TestResolve_HookFiresBeforeParserPanic(t *testing.T) {
	entered := false
	res := &execute.AttemptResult{Outcome: execute.OutcomeStructured, Structured: []byte(`{"k":1}`), AssistantText: `{"k":1}`}
	panicParse := func(context.Context, string) ([]byte, error) { panic("boom") }
	func() {
		defer func() { _ = recover() }()
		Resolve(context.Background(), nil, res, nil, panicParse, func() { entered = true })
		t.Error("Resolve returned despite a parser panic")
	}()
	if !entered {
		t.Error("onStructuredOracle did not fire before the parser panicked — the same-response phase would be lost")
	}
}

// TestResolve_ProviderErrorIsHTTPError: a provider non-2xx maps to a typed HTTPError.
func TestResolve_ProviderErrorIsHTTPError(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.OutcomeProviderError, ProviderStatus: 429, ProviderBody: []byte(`{"error":"x"}`)}
	r := Resolve(context.Background(), nil, res, nil, parse(``, nil), nil)
	if r.Served || r.Outcome != OutcomeProviderError {
		t.Fatalf("served=%v outcome=%v, want a provider-error failure", r.Served, r.Outcome)
	}
	var httpErr *llmhttp.HTTPError
	if !errors.As(r.Err, &httpErr) || httpErr.StatusCode != 429 {
		t.Errorf("err = %v (%T), want *llmhttp.HTTPError{429}", r.Err, r.Err)
	}
}

// TestResolve_TransportErrorReturnsUnwrapped: a transport failure returns the error
// unchanged (so errors.Is holds for the outer policy).
func TestResolve_TransportErrorReturnsUnwrapped(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	r := Resolve(context.Background(), nil, nil, boom, parse(``, nil), nil)
	if r.Served || r.Outcome != OutcomeTransportError || !errors.Is(r.Err, boom) {
		t.Fatalf("result = %+v (err %v), want a transport failure returning the error unchanged", r, r.Err)
	}
}

// TestResolve_NilBAMLParseIsOutputParseError: reaching the structured branch without a
// same-bytes parse closure is a terminal parse error.
func TestResolve_NilBAMLParseIsOutputParseError(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.OutcomeStructured, Structured: []byte(`{"k":1}`), Raw: `{"k":1}`}
	r := Resolve(context.Background(), nil, res, nil, nil, nil)
	if r.Served || r.Outcome != OutcomeParseError {
		t.Fatalf("served=%v outcome=%v, want a parse-error failure", r.Served, r.Outcome)
	}
	var parseErr *buildrequest.OutputParseError
	if !errors.As(r.Err, &parseErr) || !errors.Is(r.Err, ErrNoBAMLOnlyParse) {
		t.Errorf("err = %v, want an OutputParseError wrapping ErrNoBAMLOnlyParse", r.Err)
	}
}

// TestResolve_UnknownOutcomePreservesCanaryErrorBytes pins P3-7: the defensive
// unknown-outcome path keeps the legacy "nativeserve/canary: …" bytes so canary.ServeStatic
// stays byte-for-byte unchanged. The comparison is EXACT — prefix AND the Stringer-rendered
// outcome suffix (execute.Outcome.String() renders 99 as "Outcome(99)") — because a
// strings.Contains check omitting the suffix would let the "nativeserve/staticoracle:" prefix
// the extraction briefly produced (the P3-7 defect), or any other envelope-byte drift, pass.
func TestResolve_UnknownOutcomePreservesCanaryErrorBytes(t *testing.T) {
	res := &execute.AttemptResult{Outcome: execute.Outcome(99)}
	r := Resolve(context.Background(), nil, res, nil, parse(``, nil), nil)
	if r.Served {
		t.Fatal("an unknown outcome must be a failure")
	}
	if r.Err == nil {
		t.Fatal("an unknown outcome produced no error")
	}
	const want = "nativeserve/canary: unexpected static attempt outcome Outcome(99)"
	if got := r.Err.Error(); got != want {
		t.Errorf("err = %q, want EXACTLY %q (byte-for-byte canary compatibility)", got, want)
	}
}
