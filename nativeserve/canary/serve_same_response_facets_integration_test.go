//go:build nanollm_integration

package canary

// De-BAML serving cutover S3b — the SAME-RESPONSE NATIVE-WINNER PREDICATE, one
// facet at a time, over the PUBLIC Serve entrypoint and a REAL socket.
//
// # What is under proof
//
// S3 admission gate 9 says a native winner requires that BAML, reading the SAME
// response bytes, agrees on ALL of: the assistant text, the /call-with-raw raw
// channel, the reasoning channel, the structured value, and its ordering. The
// serve path records all of those facets; this file proves it also GATES on all
// of them — that a facet which disagrees can never be merely a metric while the
// caller still receives native's answer.
//
// Each arm drifts EXACTLY ONE facet and requires the same three things the
// cutover's rollout query is read from:
//
//   - exactly ONE upstream request (a drift is never repaired by asking again);
//   - ZERO native winners;
//   - the separately labelled baml_parse_same_response winner, with the served
//     envelope — structured value AND raw AND reasoning — coming from BAML's leg.
//
// # Why the drift is injected on the BAML leg rather than driven from upstream
//
// On the enrolled OpenAI surface the two legs are two readings of one response:
// native reads nanollm's translated body, BAML reads the raw provider bytes, and
// BOTH run buildrequest.ExtractResponseContentBytes with provider "openai".
// nanollm returns openai 2xx responses BYTE-VERBATIM — pinned by
// execute.TestOpenAITranslateResponseIsByteVerbatim — so the two readings are the
// same function over the same bytes, and NO upstream response can make the
// assistant, raw or reasoning facet disagree on its own. There is no end-to-end
// drift to drive; the facets are defence in depth against a future translator,
// extractor or provider mapping that stops making them identical.
//
// So the injection point is Server.bamlExtract, the BAML leg of the oracle, whose
// only writer is this file. What that mutates is one side of a COMPARISON, and
// the comparator cannot tell which side a difference came from — the same
// argument the served-path plan/parse controls rest on. Everything else here is
// real: the production Serve entrypoint, a real nanollm engine and Prepared plan,
// a real exact RoundTrip to a loopback provider, the production extractor on the
// unmutated facets, and the production metrics.
//
// The pre-claim plan oracle is deliberately made to AGREE by construction (BAML's
// plan is built from the claim's own exact request), because it is not what these
// arms are testing; it keeps its own biting per-field mutations in the served-path
// matrix (fev1_admission_matrix_integration_test.go).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// The one deterministic upstream response every arm reads, and the three channels
// the production extractor pulls out of it. facetRaw equals facetAssistant by
// construction: for openai the raw channel IS the text-only assistant channel.
const (
	facetAssistant = `{"answer":"ok"}`
	facetRaw       = facetAssistant
	facetReasoning = "because"
)

const facetServeAlias = "__same_response_facet_alias__"

// facetUpstreamBody is the loopback provider's single response: an OpenAI-shaped
// 2xx carrying the flattened schema JSON as assistant content plus a reasoning
// channel, so all three extracted facets are non-empty and individually drifted.
const facetUpstreamBody = `{"choices":[{"message":{"role":"assistant",` +
	`"content":"{\"answer\":\"ok\"}","reasoning_content":"because"}}]}`

// facetDrift describes one arm: which facet the BAML leg reads differently, and
// what BAML's leg then reports for each channel.
type facetDrift struct {
	// name is the arm label and field is the response_compare facet that MUST be
	// recorded as a mismatch.
	name  string
	field admission.ResponseCompareField
	// mutate rewrites the BAML leg's three extracted channels. Nil is the
	// positive control (no drift at all).
	mutate func(parseable, raw, reasoning string) (string, string, string)
	// bamlStructured is what BAML's same-bytes parse returns. Empty means the
	// value native produced, i.e. a structured MATCH.
	bamlStructured string
}

// facetResult is one arm's observation.
type facetResult struct {
	out              bamlutils.NativeServeResult
	reg              *prometheus.Registry
	providerRequests int64
	bamlParses       int64
}

// runFacetServe drives ONE request through the PUBLIC Serve entrypoint against a
// loopback provider, with a real strict-OpenAI claim and the BAML leg drifted per
// the arm.
func runFacetServe(t *testing.T, d facetDrift) facetResult {
	t.Helper()

	var hits atomic.Int64
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(facetUpstreamBody))
	}))
	defer cs.Close()

	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	s := NewServer(m, llmhttp.NewExactExecutor(&http.Transport{DisableKeepAlives: true}))

	claim, err := admission.AdmitStrictOpenAIClaimForTest(trustedRegistry(cs.URL), facetServeAlias)
	if err != nil {
		t.Fatalf("AdmitStrictOpenAIClaimForTest: %v", err)
	}
	s.admitClaim = func(context.Context, admission.Input) (*admission.Claim, error) { return claim, nil }

	if d.mutate != nil {
		inner := s.bamlExtract
		s.bamlExtract = func(provider string, body []byte, includeReasoning bool) (string, string, string, error) {
			parseable, raw, reasoning, xerr := inner(provider, body, includeReasoning)
			if xerr != nil {
				return parseable, raw, reasoning, xerr
			}
			p, r, rs := d.mutate(parseable, raw, reasoning)
			return p, r, rs, nil
		}
	}

	bamlStructured := d.bamlStructured
	if bamlStructured == "" {
		bamlStructured = facetAssistant
	}
	var parses atomic.Int64

	out := s.Serve(context.Background(), bamlutils.NativeServeRequest{
		Provider:     "openai",
		Mode:         bamlutils.NativeServeModeCall,
		SingleLeaf:   true,
		OutputSchema: trustedSchema(),
		// Reasoning ON so all three extracted channels are live and individually
		// comparable rather than two empty strings agreeing trivially.
		IncludeReasoning: true,
		// BAML's no-send plan, built from the claim's OWN exact request: the
		// pre-claim plan oracle agrees by construction (see the file header).
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) {
			return bamlPlanFromExact(claim.ExactRequest), nil
		},
		// BAML's parse of the same bytes. It ignores its input deliberately: the
		// assistant-facet arm drifts the text BAML parses, and a structured value
		// that moved WITH it would stop the arm being a single-facet mutation.
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			parses.Add(1)
			return []byte(bamlStructured), nil
		},
	})

	return facetResult{out: out, reg: reg, providerRequests: hits.Load(), bamlParses: parses.Load()}
}

// bamlPlanFromExact re-expresses a claimed exact request as BAML's llmhttp plan
// shape, field for field.
func bamlPlanFromExact(exact *llmhttp.ExactAttemptRequest) *llmhttp.Request {
	headers := make(map[string]string, len(exact.Headers))
	for _, h := range exact.Headers {
		headers[h.Name] = h.Value
	}
	return &llmhttp.Request{
		Method:  exact.Method,
		URL:     exact.URL,
		Headers: headers,
		Body:    string(exact.Body),
	}
}

// TestSameResponseDriftOnAnyFacetRefusesTheNativeWinner is the P0 bite: EVERY
// compared facet is part of the native-winner predicate, and a drift on any ONE
// of them serves BAML's reading of the same bytes instead.
//
// Removing any single facet from the terminal predicate makes exactly one of
// these arms fail, which is what makes them individually discriminating rather
// than a single "something drifted" assertion.
func TestSameResponseDriftOnAnyFacetRefusesTheNativeWinner(t *testing.T) {
	for _, d := range []facetDrift{
		{
			name:  "assistant text",
			field: admission.ResponseCompareFieldAssistant,
			mutate: func(_, raw, reasoning string) (string, string, string) {
				return `{"answer":"DIFFERENT"}`, raw, reasoning
			},
		},
		{
			name:  "raw channel",
			field: admission.ResponseCompareFieldRaw,
			mutate: func(parseable, _, reasoning string) (string, string, string) {
				return parseable, parseable + " trailing", reasoning
			},
		},
		{
			name:  "reasoning channel",
			field: admission.ResponseCompareFieldReasoning,
			mutate: func(parseable, raw, _ string) (string, string, string) {
				return parseable, raw, "a different justification"
			},
		},
		{
			// Structured value drift, driven the way the served-path control
			// drives it (BAML's parse returns a different value). Kept here so
			// the five-facet predicate is proved as ONE table rather than split
			// across two files; the ORDER facet's end-to-end arm lives in the
			// served-path matrix, where a map-typed field makes it drivable.
			name:           "structured value",
			field:          admission.ResponseCompareFieldStructured,
			bamlStructured: `{"answer":"DIFFERENT"}`,
		},
	} {
		t.Run(d.name, func(t *testing.T) {
			got := runFacetServe(t, d)

			if got.out.Disposition != bamlutils.NativeServeSucceeded {
				t.Fatalf("disposition = %v, want succeeded (a post-claim drift is served, never declined): stage=%q reason=%q err=%v",
					got.out.Disposition, got.out.Stage, got.out.Reason, got.out.Err)
			}
			// ONE upstream request. A same-response disagreement must never be
			// repaired by asking the provider again.
			if got.providerRequests != 1 {
				t.Errorf("the provider saw %d request(s), want exactly 1 — a %s drift must never cause a resend", got.providerRequests, d.name)
			}
			if got.bamlParses != 1 {
				t.Errorf("BAML's same-bytes parse ran %d time(s), want exactly 1", got.bamlParses)
			}
			// NOT a native winner.
			if got.out.WinnerEngine != bamlutils.NativeServeEngineBAMLParse {
				t.Errorf("winner_engine = %q, want %q — a %s drift may never read as a native win",
					got.out.WinnerEngine, bamlutils.NativeServeEngineBAMLParse, d.name)
			}
			assertFacetWinner(t, got.reg, admission.WinnerBAMLParseSameResponse)
			if v := counterValue(t, got.reg, "baml_rest_debaml_fallback_total", map[string]string{"kind": "parse_only"}); v != 1 {
				t.Errorf("fallback{kind=parse_only} = %v, want 1", v)
			}
			// The drift is recorded on the FACET that drifted, and on no other.
			assertFacetCompare(t, got.reg, d.field)

			// The served envelope is BAML's leg END TO END — its parse AND its
			// raw/reasoning channels. Returning native's channels next to BAML's
			// structured value would ship a third answer neither engine produced.
			wantStructured := d.bamlStructured
			if wantStructured == "" {
				wantStructured = facetAssistant
			}
			wantRaw, wantReasoning := facetRaw, facetReasoning
			if d.mutate != nil {
				_, wantRaw, wantReasoning = d.mutate(facetAssistant, facetRaw, facetReasoning)
			}
			if string(got.out.FinalJSON) != wantStructured {
				t.Errorf("served structured = %s, want BAML's parse %s", got.out.FinalJSON, wantStructured)
			}
			if got.out.Raw != wantRaw {
				t.Errorf("served raw = %q, want BAML's raw channel %q — the drift terminal must not expose native's channels", got.out.Raw, wantRaw)
			}
			if got.out.Reasoning != wantReasoning {
				t.Errorf("served reasoning = %q, want BAML's reasoning channel %q", got.out.Reasoning, wantReasoning)
			}
		})
	}
}

// TestNoSameResponseDriftServesNative is the positive control that makes every
// arm above causal: the IDENTICAL harness with no drift at all serves natively,
// with native's own channels, over the same single upstream request. Without it,
// "not a native winner" could be true because nothing in this harness can ever
// win natively.
func TestNoSameResponseDriftServesNative(t *testing.T) {
	got := runFacetServe(t, facetDrift{name: "no drift"})

	if got.out.Disposition != bamlutils.NativeServeSucceeded {
		t.Fatalf("disposition = %v, want succeeded: stage=%q reason=%q err=%v",
			got.out.Disposition, got.out.Stage, got.out.Reason, got.out.Err)
	}
	if got.providerRequests != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1", got.providerRequests)
	}
	if got.out.WinnerEngine != bamlutils.NativeServeEngineNative {
		t.Fatalf("winner_engine = %q, want %q — the control cannot win natively, so the drift arms prove nothing",
			got.out.WinnerEngine, bamlutils.NativeServeEngineNative)
	}
	assertFacetWinner(t, got.reg, admission.WinnerNative)
	if v := counterValue(t, got.reg, "baml_rest_debaml_fallback_total", map[string]string{"kind": "parse_only"}); v > 0 {
		t.Errorf("fallback{kind=parse_only} = %v, want none for a native win", v)
	}
	if v := counterValue(t, got.reg, "baml_rest_debaml_response_compare_total", map[string]string{"result": string(admission.ResponseCompareMismatch)}); v > 0 {
		t.Errorf("response_compare{mismatch} = %v, want 0 on the control", v)
	}
	// Every facet the predicate gates on was actually COMPARED and matched — so a
	// facet silently dropped from the recorder (and thus from the predicate) fails
	// here rather than passing as "no mismatch".
	for _, f := range []admission.ResponseCompareField{
		admission.ResponseCompareFieldAssistant,
		admission.ResponseCompareFieldRaw,
		admission.ResponseCompareFieldReasoning,
		admission.ResponseCompareFieldStructured,
		admission.ResponseCompareFieldOrder,
	} {
		labels := map[string]string{"result": string(admission.ResponseCompareMatch), "field": string(f)}
		if v := counterValue(t, got.reg, "baml_rest_debaml_response_compare_total", labels); v != 1 {
			t.Errorf("response_compare{match,%s} = %v, want 1 — the facet was not compared on the served path", f, v)
		}
	}
	// Native's OWN channels on a native win.
	if string(got.out.FinalJSON) != facetAssistant {
		t.Errorf("served structured = %s, want %s", got.out.FinalJSON, facetAssistant)
	}
	if got.out.Raw != facetRaw || got.out.Reasoning != facetReasoning {
		t.Errorf("served raw/reasoning = %q/%q, want %q/%q", got.out.Raw, got.out.Reasoning, facetRaw, facetReasoning)
	}
}

// TestExpiredPlanDeclinesPreSocket is the oracle control the plan-mutation matrix
// cannot drive through the seam: the admitted plan's signature window passes
// BEFORE the claim, so the request declines PRE-SOCKET to BAML rather than
// claiming and failing.
//
// The enrolled OpenAI surface prepares plans that never expire, so this is the
// only place the guard is reachable at all — and it is reachable here because the
// prepared plan's expiry is data on the claim, not a property of the provider.
func TestExpiredPlanDeclinesPreSocket(t *testing.T) {
	var hits atomic.Int64
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(facetUpstreamBody))
	}))
	defer cs.Close()

	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	s := NewServer(m, llmhttp.NewExactExecutor(&http.Transport{DisableKeepAlives: true}))

	claim, err := admission.AdmitStrictOpenAIClaimForTest(trustedRegistry(cs.URL), facetServeAlias)
	if err != nil {
		t.Fatalf("AdmitStrictOpenAIClaimForTest: %v", err)
	}
	// The ONE mutation: the signature window has already passed.
	expired := time.Now().Add(-time.Hour)
	claim.Prepared.Meta.ExpiresAt = &expired
	if !claim.PlanExpired() {
		t.Fatal("the mutated plan does not report expired; the control is inert")
	}
	s.admitClaim = func(context.Context, admission.Input) (*admission.Claim, error) { return claim, nil }

	out := s.Serve(context.Background(), bamlutils.NativeServeRequest{
		Provider:     "openai",
		Mode:         bamlutils.NativeServeModeCall,
		SingleLeaf:   true,
		OutputSchema: trustedSchema(),
		BuildBAMLRequest: func(context.Context) (*llmhttp.Request, error) {
			return bamlPlanFromExact(claim.ExactRequest), nil
		},
		BAMLOnlyParse: func(context.Context, string) ([]byte, error) {
			panic("an expired plan declines pre-claim: no BAML parse may run")
		},
	})

	if out.Disposition != bamlutils.NativeServeDeclined {
		t.Fatalf("disposition = %v, want Declined — an expired plan must never claim", out.Disposition)
	}
	if out.Stage != stageServe || out.Reason != reasonPlanExpired {
		t.Errorf("decline = %q/%q, want %q/%q", out.Stage, out.Reason, stageServe, reasonPlanExpired)
	}
	// NO SEND, and no claim: BAML owns the request.
	if hits.Load() != 0 {
		t.Errorf("the provider saw %d request(s); an expiry decline is PRE-socket", hits.Load())
	}
	if v := counterValue(t, reg, "baml_rest_debaml_native_sockets_total", nil); v > 0 {
		t.Errorf("native_sockets = %v, want 0", v)
	}
	claimed := map[string]string{"phase": string(admission.PhaseClaimed)}
	if v := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed); v > 0 {
		t.Errorf("claimed phase = %v, want 0", v)
	}
	assertFacetWinner(t, reg, admission.WinnerBAMLTransport)
}

// assertFacetWinner requires EXACTLY ONE winner for the request, and that it is
// the expected one — so an arm can never pass by recording two winners or none.
func assertFacetWinner(t *testing.T, reg *prometheus.Registry, want admission.Winner) {
	t.Helper()
	for _, w := range []admission.Winner{
		admission.WinnerNative,
		admission.WinnerBAMLParseSameResponse,
		admission.WinnerBAMLTransport,
		admission.WinnerFailure,
	} {
		got := counterValue(t, reg, "baml_rest_debaml_winner_total", map[string]string{"winner": string(w)})
		wantN := 0.0
		if w == want {
			wantN = 1
		}
		if got != wantN && !(wantN == 0 && got < 0) {
			t.Errorf("winner{%s} = %v, want %v", w, got, wantN)
		}
	}
}

// assertFacetCompare requires the mismatch to be recorded on the drifted facet
// and on NO other facet — the arm's single-facet claim, checked rather than
// assumed.
func assertFacetCompare(t *testing.T, reg *prometheus.Registry, drifted admission.ResponseCompareField) {
	t.Helper()
	all := []admission.ResponseCompareField{
		admission.ResponseCompareFieldAssistant,
		admission.ResponseCompareFieldRaw,
		admission.ResponseCompareFieldReasoning,
		admission.ResponseCompareFieldStructured,
		admission.ResponseCompareFieldOrder,
	}
	var extra []string
	for _, f := range all {
		labels := map[string]string{"result": string(admission.ResponseCompareMismatch), "field": string(f)}
		got := counterValue(t, reg, "baml_rest_debaml_response_compare_total", labels)
		switch {
		case f == drifted && got != 1:
			t.Errorf("response_compare{mismatch,%s} = %v, want 1 — the comparison did not examine the drifted facet", f, got)
		case f != drifted && got > 0:
			extra = append(extra, fmt.Sprintf("%s=%v", f, got))
		}
	}
	// The structured value and its ordering move together by construction (the
	// order facet re-compares the same two documents after a schema reorder), so
	// a structured drift legitimately marks both. Nothing else may ride along.
	if drifted == admission.ResponseCompareFieldStructured {
		var unexpected []string
		for _, e := range extra {
			if !strings.HasPrefix(e, string(admission.ResponseCompareFieldOrder)+"=") {
				unexpected = append(unexpected, e)
			}
		}
		extra = unexpected
	}
	if len(extra) > 0 {
		t.Errorf("a %s drift also marked %s; the arm is not a single-facet mutation", drifted, strings.Join(extra, ", "))
	}
}
