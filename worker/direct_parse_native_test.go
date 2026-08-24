package worker

// De-BAML native-first DYNAMIC direct parse + the same-input BAML transition
// oracle, from the worker side.
//
// The bridge's whole claim is "native never out-claims BAML", and these tests hold
// it from both directions: native's bytes are served when — and only when — they
// equal BAML's, and every way native can disagree ends with BAML's answer on the
// wire and a named structural reason in the counter.
//
// The BITING test is TestNativeDirectParseNeverOutClaimsOnDrift and its order-only
// sibling: they mutate the native parser into producing a DIFFERENT answer than
// BAML for the same input, which is exactly the failure the oracle exists to catch.
// If the bridge ever served native's answer unchecked, those two would fail.

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/invakid404/baml-rest/bamlutils"
)

// rawDynamicOutput stands in for the generated Baml_Rest_DynamicOutput envelope:
// one field, marshalled by the same sonic call worker.Parse uses on BAML's result.
// Holding the payload as a RawMessage lets a test state BAML's exact bytes.
type rawDynamicOutput struct {
	DynamicProperties stdjson.RawMessage `json:"DynamicProperties"`
}

// oracleLeg records what BAML's parse implementation saw, so a test can prove the
// oracle leg ran, ran on the same raw input, and ran with the de-BAML flag off (a
// genuine BAML parse rather than a second trip through the generated native seam).
type oracleLeg struct {
	calls    int
	raw      string
	deBAMLOn bool
}

// dynamicParseRuntime builds a runtime whose Baml_Rest_Dynamic parse method stands
// in for BAML: it returns payload (or bamlErr) and records the call on leg.
func dynamicParseRuntime(leg *oracleLeg, payload string, bamlErr error) *fakeRuntime {
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &rawDynamicOutput{} },
		Impl: func(a bamlutils.Adapter, raw string) (any, error) {
			leg.calls++
			leg.raw = raw
			leg.deBAMLOn = a.DeBAMLConfig().Enabled
			if bamlErr != nil {
				return nil, bamlErr
			}
			return rawDynamicOutput{DynamicProperties: stdjson.RawMessage(payload)}, nil
		},
	}
	return &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{
		bamlutils.DynamicMethodName: method,
	}}
}

// nativeLeg records the native parser's invocations.
type nativeLeg struct {
	calls int
	raw   string
}

// nativeParser returns a DeBAMLParseFunc that answers with payload (or err) and
// records the call on leg.
func nativeParser(leg *nativeLeg, payload string, err error) bamlutils.DeBAMLParseFunc {
	return func(_ context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
		leg.calls++
		leg.raw = req.Raw
		if err != nil {
			return bamlutils.DeBAMLParseResult{}, err
		}
		return bamlutils.DeBAMLParseResult{JSON: stdjson.RawMessage(payload)}, nil
	}
}

// dynamicParseInput marshals a worker parse input carrying a dynamic output schema,
// which is what makes a request eligible for the native-first bridge.
func dynamicParseInput(t *testing.T, raw string, stream bool) []byte {
	t.Helper()
	in, err := sonic.Marshal(workerParseInput{
		Raw:    raw,
		Stream: stream,
		Options: &bamlutils.BamlOptions{
			OutputSchema: &bamlutils.DynamicOutputSchema{},
		},
	})
	if err != nil {
		t.Fatalf("marshal parse input: %v", err)
	}
	return in
}

// deBAMLOnConfig is the flag-on umbrella config every bridge test uses.
func deBAMLOnConfig() bamlutils.DeBAMLConfig {
	return bamlutils.DeBAMLConfig{Enabled: true}
}

// directParseCount reads one disposition counter off the handler's own registry —
// the same registry the worker exports through GetMetrics, so what a test asserts
// is what an operator would scrape.
func directParseCount(t *testing.T, h *Handler, engine, reason string) float64 {
	t.Helper()
	families, err := h.metricsReg.Gather()
	if err != nil {
		t.Fatalf("gather worker metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "debaml_direct_parse_total" {
			continue
		}
		for _, m := range mf.Metric {
			if labelValue(m, "engine") == engine && labelValue(m, "reason") == reason &&
				labelValue(m, "surface") == directParseSurface {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// directParseTotal sums every disposition the handler recorded. Zero is the
// flag-off / ineligible property: the bridge did not run at all.
func directParseTotal(t *testing.T, h *Handler) float64 {
	t.Helper()
	families, err := h.metricsReg.Gather()
	if err != nil {
		t.Fatalf("gather worker metrics: %v", err)
	}
	var total float64
	for _, mf := range families {
		if mf.GetName() != "debaml_direct_parse_total" {
			continue
		}
		for _, m := range mf.Metric {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

const (
	// agreedPayload is what both legs produce when they agree.
	agreedPayload = `{"name":"Ada","age":41}`
	// agreedEnvelope is the worker-boundary payload that agreement produces.
	agreedEnvelope = `{"DynamicProperties":{"name":"Ada","age":41}}`
)

// TestNativeDirectParseServesNativeOnAgreement is the positive case: native and
// BAML produce identical bytes for the same raw input, so native's payload is
// served — and it is byte-identical to what BAML alone would have returned.
func TestNativeDirectParseServesNativeOnAgreement(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want %s", got, agreedEnvelope)
	}
	if native.calls != 1 {
		t.Errorf("native parser called %d times, want exactly 1", native.calls)
	}
	if native.raw != "{...}" {
		t.Errorf("native parsed %q, want the exact raw input", native.raw)
	}
	// The oracle is not optional: BAML must have parsed the SAME input even on the
	// path where native wins. Without this, "agreement" would be unverified.
	if baml.calls != 1 {
		t.Errorf("BAML oracle leg called %d times, want exactly 1", baml.calls)
	}
	if baml.raw != "{...}" {
		t.Errorf("BAML oracle parsed %q, want the same raw input native saw", baml.raw)
	}
	if baml.deBAMLOn {
		t.Error("the BAML oracle leg ran with de-BAML still enabled; it would re-enter the native seam instead of being an independent parse")
	}
	if got := directParseCount(t, h, directParseEngineNative, directParseReasonAgreement); got != 1 {
		t.Errorf("native/agreement counter = %v, want 1", got)
	}
}

// TestNativeDirectParseNeverOutClaimsOnDrift is the BITING test. The native parser
// is mutated to answer differently from BAML for the same input — the exact shape
// of an out-claim. The bridge must serve BAML's bytes and record drift; a bridge
// that trusted native would fail here.
func TestNativeDirectParseNeverOutClaimsOnDrift(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime: dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:  deBAMLOnConfig(),
		// One field differs. BAML says age 41; native says 42.
		DeBAMLParse: nativeParser(native, `{"name":"Ada","age":42}`, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("native out-claimed BAML: served %s, want BAML's %s", got, agreedEnvelope)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonResultDrift); got != 1 {
		t.Errorf("baml/result_drift counter = %v, want 1", got)
	}
	if got := directParseCount(t, h, directParseEngineNative, directParseReasonAgreement); got != 0 {
		t.Errorf("a drifting parse was recorded as agreement (%v)", got)
	}
}

// TestNativeDirectParseNeverOutClaimsOnKeyOrder is the second biting case, and the
// reason the oracle compares BYTES rather than semantics: these two payloads carry
// the same fields with the same values in a different ORDER. Key order is
// observable on the wire (nothing downstream re-canonicalizes it), so a native
// answer that reorders BAML's output is a real difference and must not be served.
func TestNativeDirectParseNeverOutClaimsOnKeyOrder(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, `{"age":41,"name":"Ada"}`, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("native out-claimed BAML on key order: served %s, want BAML's %s", got, agreedEnvelope)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonResultDrift); got != 1 {
		t.Errorf("baml/result_drift counter = %v, want 1", got)
	}
}

// TestNativeDirectParseNormalizesEncodingBeforeComparing is the counterpart to the
// key-order test: whitespace and escaping are NOT observable differences, so a
// native answer that differs from BAML only in encoding still agrees. Without the
// re-encode the bridge would decline every one of these and claim nothing.
func TestNativeDirectParseNormalizesEncodingBeforeComparing(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime: dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:  deBAMLOnConfig(),
		// Same fields, same order, same numbers — pretty-printed.
		DeBAMLParse: nativeParser(native, "{\n  \"name\": \"Ada\",\n  \"age\": 41\n}", nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want %s", got, agreedEnvelope)
	}
	if got := directParseCount(t, h, directParseEngineNative, directParseReasonAgreement); got != 1 {
		t.Errorf("native/agreement counter = %v, want 1", got)
	}
}

// TestNativeDirectParseKeepsNumberSpelling pins the other half of normalization: a
// number's SPELLING is observable (41 and 41.0 reach the wire differently), so the
// re-encode must preserve it and the comparison must reject the difference.
func TestNativeDirectParseKeepsNumberSpelling(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, `{"name":"Ada","age":41.0}`, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("41.0 was served for BAML's 41: %s", got)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonResultDrift); got != 1 {
		t.Errorf("baml/result_drift counter = %v, want 1", got)
	}
}

// TestNativeDirectParseDeclinesUnsupported is the ordinary decline: the raw text or
// the schema is outside the native cut-line, so BAML parses and the reason names
// the cut-line rather than a disagreement.
func TestNativeDirectParseDeclinesUnsupported(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, "", bamlutils.ErrDeBAMLParseUnsupported),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want BAML's %s", got, agreedEnvelope)
	}
	if baml.calls != 1 {
		t.Errorf("BAML parsed %d times, want exactly 1", baml.calls)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeUnsupported); got != 1 {
		t.Errorf("baml/native_unsupported counter = %v, want 1", got)
	}
}

// TestNativeDirectParseClaimedErrorDoesNotFailARequestBAMLParses is the third
// out-claim shape: native CLAIMED a parse failure on input BAML parses fine. The
// caller must still get BAML's successful result — a native bug cannot turn a
// working parse into a failure.
func TestNativeDirectParseClaimedErrorDoesNotFailARequestBAMLParses(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, "", errors.New("native: cannot coerce field age")),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("a claimed native failure failed a parse BAML succeeded at: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want BAML's %s", got, agreedEnvelope)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeErrorBAMLOK); got != 1 {
		t.Errorf("baml/native_error_baml_ok counter = %v, want 1", got)
	}
}

// TestNativeDirectParseBAMLErrorWins is the most dangerous out-claim shape
// inverted: native produced a RESULT for input BAML rejects. The caller must get
// BAML's error, not native's data.
func TestNativeDirectParseBAMLErrorWins(t *testing.T) {
	t.Parallel()

	bamlErr := errors.New("baml: unterminated object")
	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, "", bamlErr),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...", false))
	if err == nil {
		t.Fatalf("native's result was served where BAML errored: %s", string(res.Data))
	}
	if !errors.Is(err, bamlErr) {
		t.Errorf("Parse error = %v, want BAML's own error", err)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeOKBAMLError); got != 1 {
		t.Errorf("baml/native_ok_baml_error counter = %v, want 1", got)
	}
}

// TestNativeDirectParseUnusableResultWithBAMLErrorIsNotAnOutClaim pins the one
// combination the two settle paths could disagree about: native reported success,
// its result could not be encoded, and BAML then errored.
//
// Native produced nothing servable, so there was no out-claim to prevent. Counting
// it as native_ok_baml_error would put a parser bug in the bucket that exists to
// count the most dangerous out-claim shape — and warn that native "claimed a
// result" it could never have served.
func TestNativeDirectParseUnusableResultWithBAMLErrorIsNotAnOutClaim(t *testing.T) {
	t.Parallel()

	bamlErr := errors.New("baml: unterminated object")
	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, "", bamlErr),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, `["not","an","object"]`, nil),
	})

	_, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...", false))
	if !errors.Is(err, bamlErr) {
		t.Fatalf("Parse error = %v, want BAML's own error", err)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeResultUnusable); got != 1 {
		t.Errorf("baml/native_result_unusable counter = %v, want 1", got)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeOKBAMLError); got != 0 {
		t.Errorf("an unusable native result was counted as a prevented out-claim (%v)", got)
	}
}

// TestNativeDirectParseBothErrorServesBAMLsError pins that error PARITY is not
// enough to win: the bytes a client sees for a failed parse are BAML's message, and
// native's message is not proven to match, so BAML's error is served.
func TestNativeDirectParseBothErrorServesBAMLsError(t *testing.T) {
	t.Parallel()

	bamlErr := errors.New("baml: unterminated object")
	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, "", bamlErr),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, "", errors.New("native: unterminated object")),
	})

	_, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...", false))
	if !errors.Is(err, bamlErr) {
		t.Fatalf("Parse error = %v, want BAML's own error", err)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonBothError); got != 1 {
		t.Errorf("baml/both_error counter = %v, want 1", got)
	}
}

// TestNativeDirectParseRejectsUnusableNativeResult covers a native parser that
// reports success but hands back something that is not a dynamic output object.
// It is a parser bug, not a semantic disagreement, and it must not reach the wire.
func TestNativeDirectParseRejectsUnusableNativeResult(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, `["not","an","object"]`, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want BAML's %s", got, agreedEnvelope)
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonNativeResultUnusable); got != 1 {
		t.Errorf("baml/native_result_unusable counter = %v, want 1", got)
	}
}

// TestNativeDirectParseRecordsAnUnserializableBAMLResult covers the one path where
// there is no BAML payload to compare against: BAML recovered a value with no JSON
// form (a non-finite float), so the marshal fails and the request errors exactly as
// it always did. The disposition is still recorded, so every request the bridge
// handled accounts for itself.
func TestNativeDirectParseRecordsAnUnserializableBAMLResult(t *testing.T) {
	t.Parallel()

	native := &nativeLeg{}
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &nonFiniteDynamicOutput{} },
		Impl: func(bamlutils.Adapter, string) (any, error) {
			return nonFiniteDynamicOutput{DynamicProperties: map[string]any{"ratio": math.Inf(1)}}, nil
		},
	}
	h := newTestHandler(t, Config{
		Runtime: &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{
			bamlutils.DynamicMethodName: method,
		}},
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	if _, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false)); err == nil {
		t.Fatal("an unserializable BAML result did not fail the parse")
	}
	if got := directParseCount(t, h, directParseEngineBAML, directParseReasonBAMLResultUnusable); got != 1 {
		t.Errorf("baml/baml_result_unusable counter = %v, want 1", got)
	}
	if got := directParseTotal(t, h); got != 1 {
		t.Errorf("the request recorded %v dispositions, want exactly 1", got)
	}
}

// nonFiniteDynamicOutput is a dynamic-output envelope whose payload cannot be
// marshalled: JSON has no representation for ±Inf or NaN.
type nonFiniteDynamicOutput struct {
	DynamicProperties map[string]any `json:"DynamicProperties"`
}

// TestNativeDirectParseIsOffWithTheFlagOff is the flag-off control: zero native
// parse work, zero counters, and the adapter keeps the de-BAML config the handler
// gave it.
func TestNativeDirectParseIsOffWithTheFlagOff(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	native := &nativeLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      bamlutils.DeBAMLConfig{},
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	res, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(res.Data); got != agreedEnvelope {
		t.Fatalf("served payload = %s, want BAML's %s", got, agreedEnvelope)
	}
	if native.calls != 0 {
		t.Errorf("the native parser ran %d times with the umbrella flag off", native.calls)
	}
	if got := directParseTotal(t, h); got != 0 {
		t.Errorf("the flag-off build recorded %v direct-parse dispositions, want 0", got)
	}
}

// TestNativeDirectParseIsOffWithoutAParser is the BAML-only worker control: the
// flag is on but no native parser is injected, so nothing native happens.
func TestNativeDirectParseIsOffWithoutAParser(t *testing.T) {
	t.Parallel()

	baml := &oracleLeg{}
	h := newTestHandler(t, Config{
		Runtime: dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:  deBAMLOnConfig(),
	})

	if _, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", false)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !baml.deBAMLOn {
		t.Error("the parse ran with de-BAML disabled on the adapter; with no bridge active the adapter must keep the handler's config")
	}
	if got := directParseTotal(t, h); got != 0 {
		t.Errorf("a parser-less worker recorded %v direct-parse dispositions, want 0", got)
	}
}

// TestNativeDirectParseSkipsParseStream keeps parse-STREAM on BAML: the bridge
// models final-parse semantics only, so a Stream request must not reach native and
// must leave the generated seam's own config untouched.
func TestNativeDirectParseSkipsParseStream(t *testing.T) {
	t.Parallel()

	native := &nativeLeg{}
	var streamRan bool
	var streamDeBAMLOn bool
	method := bamlutils.ParseMethod{
		MakeOutput: func() any { return &rawDynamicOutput{} },
		Impl:       func(bamlutils.Adapter, string) (any, error) { return rawDynamicOutput{}, nil },
		StreamImpl: func(a bamlutils.Adapter, _ string) (any, error) {
			streamRan = true
			streamDeBAMLOn = a.DeBAMLConfig().Enabled
			return rawDynamicOutput{DynamicProperties: stdjson.RawMessage(`{"partial":true}`)}, nil
		},
	}
	h := newTestHandler(t, Config{
		Runtime: &fakeRuntime{parseMethods: map[string]bamlutils.ParseMethod{
			bamlutils.DynamicMethodName: method,
		}},
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	if _, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, dynamicParseInput(t, "{...}", true)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !streamRan {
		t.Fatal("the parse-stream implementation did not run")
	}
	if native.calls != 0 {
		t.Errorf("the final-mode native bridge ran on a parse-stream request (%d calls)", native.calls)
	}
	if !streamDeBAMLOn {
		t.Error("the parse-stream leg lost its de-BAML config; the bridge must not touch a request it does not handle")
	}
}

// TestNativeDirectParseSkipsStaticMethods keeps static `/parse/{method}` on BAML.
// The native parser coerces against a dynamic output schema a static method does
// not have, so the bridge must not fire — and must leave the static path's own
// de-BAML config alone.
func TestNativeDirectParseSkipsStaticMethods(t *testing.T) {
	t.Parallel()

	native := &nativeLeg{}
	baml := &oracleLeg{}
	rt := dynamicParseRuntime(baml, agreedPayload, nil)
	rt.parseMethods["ParseTree"] = rt.parseMethods[bamlutils.DynamicMethodName]
	h := newTestHandler(t, Config{
		Runtime:     rt,
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	if _, err := h.Parse(context.Background(), "ParseTree", dynamicParseInput(t, "{...}", false)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if native.calls != 0 {
		t.Errorf("the dynamic bridge ran for a static parse method (%d calls)", native.calls)
	}
	if !baml.deBAMLOn {
		t.Error("a static parse lost its de-BAML config; the bridge must not touch a request it does not handle")
	}
	if got := directParseTotal(t, h); got != 0 {
		t.Errorf("a static parse recorded %v direct-parse dispositions, want 0", got)
	}
}

// TestNativeDirectParseSkipsSchemalessRequests covers a dynamic parse that carries
// no output schema: there is nothing for the native parser to coerce against, so
// BAML owns it.
func TestNativeDirectParseSkipsSchemalessRequests(t *testing.T) {
	t.Parallel()

	native := &nativeLeg{}
	baml := &oracleLeg{}
	h := newTestHandler(t, Config{
		Runtime:     dynamicParseRuntime(baml, agreedPayload, nil),
		DeBAML:      deBAMLOnConfig(),
		DeBAMLParse: nativeParser(native, agreedPayload, nil),
	})

	if _, err := h.Parse(context.Background(), bamlutils.DynamicMethodName, []byte(`{"raw":"{...}"}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if native.calls != 0 {
		t.Errorf("the bridge ran without a carried output schema (%d calls)", native.calls)
	}
	if got := directParseTotal(t, h); got != 0 {
		t.Errorf("a schema-less parse recorded %v direct-parse dispositions, want 0", got)
	}
}

// TestDirectParseMetricsReuseASharedRegistry pins the construction contract: an
// in-process host may build several handlers on one registry, and that must keep
// counting rather than fail.
func TestDirectParseMetricsReuseASharedRegistry(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	first := newTestHandler(t, Config{Runtime: &fakeRuntime{}, Metrics: reg})
	second := newTestHandler(t, Config{Runtime: &fakeRuntime{}, Metrics: reg})

	first.directParseMetrics.record(directParseEngineNative, directParseReasonAgreement)
	second.directParseMetrics.record(directParseEngineNative, directParseReasonAgreement)

	if got := directParseCount(t, first, directParseEngineNative, directParseReasonAgreement); got != 2 {
		t.Fatalf("shared-registry counter = %v, want 2 (both handlers counting into one series)", got)
	}
}
