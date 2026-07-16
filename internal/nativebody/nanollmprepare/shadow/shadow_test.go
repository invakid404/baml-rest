//go:build nanollm_integration

package shadow

// De-BAML cutover Slice 4 one-send SHADOW comparator suite. Gated by
// `nanollm_integration` (the opt-in tag) because the Comparator runs the S3
// admission predicate, which links nanollm through nanollm.New/Prepare/Close; it
// imports NO BAML runtime, so it needs no CFFI.
//
// It proves, entirely WITHOUT a socket:
//   - the pure plan comparison (comparePlans) classifies method/target/host/
//     headers/body match vs mismatch, excludes BAML's baml-original-url, fails
//     closed on duplicate headers, and NEVER leaks a secret into a diagnostic;
//   - the Comparator admits a well-formed dynamic call, obtains BAML's built plan
//     WITHOUT sending, records one plan_compare series per field (NO values), and
//     ALWAYS returns a decline so BAML serves;
//   - a deliberate native/BAML plan drift is caught as a `mismatch`, never a
//     silent pass;
//   - an admission decline records the S3 decline (no plan comparison) and
//     returns the stable admission stage/reason;
//   - a production-shaped registry maps into the native config path;
//   - ZERO RoundTrips occur on any path (a counting exact transport stays at
//     zero across the whole suite).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/admission"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/planassert"
)

func sp(s string) *string { return &s }

// leakSecret is a fake bearer token distinct from the fence key; a diagnostic
// must NEVER contain it (redaction proof).
const leakSecret = "sk-LEAKED-SHADOW-SECRET-nevereverlogged"

// --- zero-socket transport (mirrors the admission suite) ---

type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

func assertNoSocket(t *testing.T, ct *countingTransport) {
	t.Helper()
	if n := ct.n.Load(); n != 0 {
		t.Fatalf("shadow comparison opened %d socket(s); the no-send comparator must open zero", n)
	}
}

// --- metric readers ---

func counterVal(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			if labelsMatch(mm.GetLabel(), labels) {
				return mm.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func planCompareVal(t *testing.T, reg *prometheus.Registry, result, field string) float64 {
	return counterVal(t, reg, "baml_rest_debaml_plan_compare_total", map[string]string{"result": result, "field": field})
}

// --- fixtures ---

func shadowRegistry() *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "TestClient",
			Provider: "openai",
			Options: map[string]any{
				"model":    planassert.FenceModel,
				"base_url": planassert.FenceBaseURL,
				"api_key":  planassert.FenceAPIKey,
			},
		}},
	}
}

func shadowMessages() []bamlutils.DynamicMessage {
	return []bamlutils.DynamicMessage{
		{Role: "system", TextContent: sp("You are concise.")},
		{Role: "user", TextContent: sp("What is 2+2?")},
	}
}

func shadowSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

func shadowRequest(build func(ctx context.Context) (*llmhttp.Request, error)) bamlutils.NativeShadowRequest {
	return bamlutils.NativeShadowRequest{
		Registry:         shadowRegistry(),
		Messages:         shadowMessages(),
		OutputSchema:     shadowSchema(),
		Provider:         "openai",
		Mode:             bamlutils.NativeShadowModeCall,
		SingleLeaf:       true,
		BuildBAMLRequest: build,
	}
}

// referenceNativePlan runs the SAME admission the Comparator runs, so the test
// can construct a byte-identical (or deliberately drifted) BAML plan from the
// admitted native plan. It uses its own counting transport and preflight — no
// socket — and shares the package-internal toAdmissionInput mapping.
func referenceNativePlan(t *testing.T, req bamlutils.NativeShadowRequest) *llmhttp.ExactAttemptRequest {
	t.Helper()
	ct := &countingTransport{}
	a := admission.NewAdmitter(nil, llmhttp.NewExactExecutor(ct))
	ad, err := a.Admit(context.Background(), toAdmissionInput(req))
	if err != nil {
		t.Fatalf("reference admission declined a well-formed request: %v", err)
	}
	assertNoSocket(t, ct)
	return ad.ExactRequest
}

// bamlFromNative mirrors a native plan into BAML's llmhttp.Request shape (adding
// BAML's baml-original-url transport header, which the comparison must exclude),
// then applies an optional mutation to induce drift.
func bamlFromNative(ex *llmhttp.ExactAttemptRequest, mutate func(*llmhttp.Request)) *llmhttp.Request {
	headers := map[string]string{}
	for _, h := range ex.Headers {
		headers[h.Name] = h.Value
	}
	headers["baml-original-url"] = planassert.FenceBaseURL
	req := &llmhttp.Request{Method: ex.Method, URL: ex.URL, Headers: headers, Body: string(ex.Body)}
	if mutate != nil {
		mutate(req)
	}
	return req
}

// --- comparePlans pure unit tests (no admission needed) ---

func nativeExact(body string, extra ...llmhttp.HeaderField) *llmhttp.ExactAttemptRequest {
	headers := []llmhttp.HeaderField{
		{Name: "Content-Type", Value: "application/json"},
		{Name: "Authorization", Value: "Bearer " + planassert.FenceAPIKey},
	}
	headers = append(headers, extra...)
	return &llmhttp.ExactAttemptRequest{Method: "POST", URL: planassert.WantURL, Headers: headers, Body: []byte(body), BodyPresent: true}
}

func bamlExact(body string, mutate func(*llmhttp.Request)) *llmhttp.Request {
	req := &llmhttp.Request{
		Method: "POST",
		URL:    planassert.WantURL,
		Headers: map[string]string{
			"content-type":      "application/json",
			"authorization":     "Bearer " + planassert.FenceAPIKey,
			"baml-original-url": planassert.FenceBaseURL,
		},
		Body: body,
	}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func TestComparePlans_MatchExcludesBAMLTransportHeader(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	cmp := comparePlans(bamlExact(body, nil), nativeExact(body))
	if !cmp.allMatch() {
		t.Fatalf("expected full match, got %+v", cmp)
	}
	if len(cmp.Diffs) != 0 {
		t.Fatalf("expected no diffs (baml-original-url excluded), got %v", cmp.Diffs)
	}
}

func TestComparePlans_FieldDrift(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	cases := []struct {
		name    string
		baml    *llmhttp.Request
		native  *llmhttp.ExactAttemptRequest
		method  bool
		target  bool
		host    bool
		headers bool
		bodyOK  bool
	}{
		{"body", bamlExact(`{"model":"OTHER"}`, nil), nativeExact(body), true, true, true, true, false},
		{"method", bamlExact(body, func(r *llmhttp.Request) { r.Method = "GET" }), nativeExact(body), false, true, true, true, true},
		{"host", bamlExact(body, func(r *llmhttp.Request) { r.URL = "https://evil.invalid/v1/chat/completions" }), nativeExact(body), true, true, false, true, true},
		{"target", bamlExact(body, func(r *llmhttp.Request) { r.URL = planassert.FenceBaseURL + "/responses" }), nativeExact(body), true, false, true, true, true},
		{"headers_extra_native", bamlExact(body, nil), nativeExact(body, llmhttp.HeaderField{Name: "X-Extra", Value: "1"}), true, true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmp := comparePlans(tc.baml, tc.native)
			if cmp.Method != tc.method || cmp.Target != tc.target || cmp.Host != tc.host || cmp.Headers != tc.headers || cmp.Body != tc.bodyOK {
				t.Fatalf("drift classification mismatch: got %+v", cmp)
			}
		})
	}
}

func TestComparePlans_RedactsSecretsInDiffs(t *testing.T) {
	const body = `{"model":"m"}`
	// BAML carries a DIFFERENT (leaked) authorization value: headers mismatch, and
	// the diagnostic must redact both the fence key and the leaked value.
	baml := bamlExact(body, func(r *llmhttp.Request) { r.Headers["authorization"] = "Bearer " + leakSecret })
	cmp := comparePlans(baml, nativeExact(body))
	if cmp.Headers {
		t.Fatal("expected a header mismatch on divergent authorization")
	}
	joined := strings.Join(cmp.Diffs, "\n")
	if joined == "" {
		t.Fatal("expected a redacted diff describing the header mismatch")
	}
	if strings.Contains(joined, leakSecret) || strings.Contains(joined, planassert.FenceAPIKey) {
		t.Fatalf("diagnostic leaked a credential: %q", joined)
	}
	if !strings.Contains(joined, "authorization") || !strings.Contains(joined, "<redacted>") {
		t.Fatalf("expected a redacted authorization diff, got %q", joined)
	}
}

func TestComparePlans_BodyDiffNeverPrintsBytes(t *testing.T) {
	// Bodies carry prompt text in production; a diff must show only a bounded byte
	// LENGTH — never the bytes AND never a content-derived hash/digest (even a
	// truncated digest can confirm/correlate body content).
	baml := bamlExact(`{"secret_prompt":"do not log me"}`, nil)
	cmp := comparePlans(baml, nativeExact(`{"model":"m"}`))
	if cmp.Body {
		t.Fatal("expected a body mismatch")
	}
	joined := strings.Join(cmp.Diffs, "\n")
	if strings.Contains(joined, "do not log me") {
		t.Fatalf("body diff leaked prompt bytes: %q", joined)
	}
	// Assert the COMPLETE body-diff entry is EXACTLY the two bounded byte-length
	// fields and nothing else — no bytes, no hash/digest, and no extra
	// content-derived field appended. An exact-match (rather than substring)
	// assertion fails closed if any future field is added to the body diff.
	var bodyDiffs []string
	for _, d := range cmp.Diffs {
		if strings.HasPrefix(d, "body:") {
			bodyDiffs = append(bodyDiffs, d)
		}
	}
	if len(bodyDiffs) != 1 {
		t.Fatalf("expected exactly one body diff entry, got %d: %v", len(bodyDiffs), cmp.Diffs)
	}
	const wantBodyDiff = `body: baml=33B native=13B`
	if bodyDiffs[0] != wantBodyDiff {
		t.Fatalf("body diff = %q, want EXACTLY %q (bounded lengths only — any extra field/hash/body-derived value must fail)", bodyDiffs[0], wantBodyDiff)
	}
}

func TestComparePlans_DuplicateHeaderFailsClosed(t *testing.T) {
	const body = `{"model":"m"}`
	// A duplicate Content-Type on the native side must fail closed on the HEADERS
	// facet only (not silently merge) — but the method, target, host, and body are
	// identical here and do NOT depend on header normalization, so they must keep
	// their real (matching) result rather than being fabricated as mismatches.
	native := nativeExact(body, llmhttp.HeaderField{Name: "Content-Type", Value: "application/json"})
	cmp := comparePlans(bamlExact(body, nil), native)
	if cmp.allMatch() {
		t.Fatal("expected a fail-closed header mismatch on a duplicate header")
	}
	if !cmp.Method || !cmp.Target || !cmp.Host || !cmp.Body {
		t.Fatalf("header fail-closed must not fabricate method/target/host/body mismatches, got %+v", cmp)
	}
	if cmp.Headers {
		t.Fatalf("headers facet must fail closed on a duplicate header, got %+v", cmp)
	}
	if !cmp.MetaMismatch {
		t.Fatalf("a structural header-normalization failure must flag a meta mismatch, got %+v", cmp)
	}
	// The structural reason is surfaced (redacted, no values) for the native side.
	joined := strings.Join(cmp.Diffs, "\n")
	if !strings.Contains(joined, "native plan headers not normalizable") {
		t.Fatalf("expected the structural fail-closed reason in the diffs, got %q", joined)
	}
	if strings.Contains(joined, planassert.FenceAPIKey) {
		t.Fatalf("structural diff leaked a credential: %q", joined)
	}
}

// TestRecordComparison_HeaderFailClosedOnlyMarksHeadersAndMeta proves the
// per-field telemetry on the fail-closed header-normalization branch: when only
// the header set is un-normalizable, the recorded plan_compare series mark
// method/target/host/body as MATCH (they are unaffected) and only headers + meta
// as MISMATCH — no fabricated method/target/host/body mismatch alerts.
func TestRecordComparison_HeaderFailClosedOnlyMarksHeadersAndMeta(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	c := NewComparator(m, llmhttp.NewExactExecutor(&countingTransport{}))

	const body = `{"model":"m"}`
	native := nativeExact(body, llmhttp.HeaderField{Name: "Content-Type", Value: "application/json"})
	c.recordComparison(comparePlans(bamlExact(body, nil), native))

	for _, f := range []string{"method", "target", "host", "body"} {
		if got := planCompareVal(t, reg, "match", f); got != 1 {
			t.Errorf("plan_compare{match,%s} = %v, want 1 (unaffected by header fail-closed)", f, got)
		}
		if got := planCompareVal(t, reg, "mismatch", f); got != 0 {
			t.Errorf("plan_compare{mismatch,%s} = %v, want 0 (must not fabricate a mismatch)", f, got)
		}
	}
	if got := planCompareVal(t, reg, "mismatch", "headers"); got != 1 {
		t.Errorf("plan_compare{mismatch,headers} = %v, want 1 (fail closed)", got)
	}
	if got := planCompareVal(t, reg, "match", "headers"); got != 0 {
		t.Errorf("plan_compare{match,headers} = %v, want 0", got)
	}
	if got := planCompareVal(t, reg, "mismatch", "meta"); got != 1 {
		t.Errorf("plan_compare{mismatch,meta} = %v, want 1 (structural fail-closed)", got)
	}
}

// --- Comparator end-to-end (admission + compare + record + decline) ---

func TestComparator_AdmitMatchRecordsAndDeclines(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(nil)
	ref := referenceNativePlan(t, req)
	req.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
		return bamlFromNative(ref, nil), nil
	}

	res := c.Compare(context.Background(), req)

	// Always declines to BAML with the terminal shadow disposition.
	if res.Stage != stageShadow || res.Reason != reasonServedBAML {
		t.Fatalf("expected a served_baml decline, got stage=%q reason=%q", res.Stage, res.Reason)
	}
	// Every field matched: exactly one match series per field, zero mismatches.
	for _, f := range []string{"method", "target", "host", "headers", "body"} {
		if got := planCompareVal(t, reg, "match", f); got != 1 {
			t.Errorf("plan_compare{match,%s} = %v, want 1", f, got)
		}
		if got := planCompareVal(t, reg, "mismatch", f); got != 0 {
			t.Errorf("plan_compare{mismatch,%s} = %v, want 0", f, got)
		}
	}
	// The admission counted a full admit.
	if got := counterVal(t, reg, "baml_rest_debaml_attempts_total", map[string]string{"mode": "call", "engine": "native", "provider": "openai", "outcome": "admitted"}); got != 1 {
		t.Errorf("attempts{admitted} = %v, want 1", got)
	}
	// ZERO sockets on the whole comparison.
	assertNoSocket(t, ct)
}

func TestComparator_PlanDriftRecordsMismatch(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(nil)
	ref := referenceNativePlan(t, req)
	// Deliberate body drift: BAML's plan carries an extra field the native plan
	// does not. This must be caught as a body mismatch, not a silent pass.
	req.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
		return bamlFromNative(ref, func(r *llmhttp.Request) { r.Body = r.Body + " " }), nil
	}

	res := c.Compare(context.Background(), req)
	if res.Reason != reasonServedBAML {
		t.Fatalf("a mismatch must still decline to BAML, got reason=%q", res.Reason)
	}
	if got := planCompareVal(t, reg, "mismatch", "body"); got != 1 {
		t.Errorf("plan_compare{mismatch,body} = %v, want 1", got)
	}
	if got := planCompareVal(t, reg, "match", "body"); got != 0 {
		t.Errorf("plan_compare{match,body} = %v, want 0", got)
	}
	// The other fields still matched.
	for _, f := range []string{"method", "target", "host", "headers"} {
		if got := planCompareVal(t, reg, "match", f); got != 1 {
			t.Errorf("plan_compare{match,%s} = %v, want 1", f, got)
		}
	}
	assertNoSocket(t, ct)
}

func TestComparator_PanickingBAMLBuilderDeclines(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(func(context.Context) (*llmhttp.Request, error) {
		panic("synthetic no-send builder panic")
	})
	res := c.Compare(context.Background(), req)

	if res.Stage != stageShadow || res.Reason != reasonServedBAML {
		t.Fatalf("panicking shadow work must decline to BAML, got stage=%q reason=%q", res.Stage, res.Reason)
	}
	for _, f := range []string{"method", "target", "host", "headers", "body"} {
		if got := planCompareVal(t, reg, "match", f); got != 0 {
			t.Errorf("plan_compare{match,%s} = %v, want 0 after panic", f, got)
		}
		if got := planCompareVal(t, reg, "mismatch", f); got != 0 {
			t.Errorf("plan_compare{mismatch,%s} = %v, want 0 after panic", f, got)
		}
	}
	assertNoSocket(t, ct)
}

func TestComparator_AdmissionDeclineSkipsComparison(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	// A non-openai provider declines at the provider stage before any plan is
	// built; BuildBAMLRequest must never run and no plan_compare is recorded.
	req := shadowRequest(func(context.Context) (*llmhttp.Request, error) {
		t.Fatal("BuildBAMLRequest must not run on an admission decline")
		return nil, nil
	})
	req.Provider = "anthropic"
	req.Registry.Clients[0].Provider = "anthropic"

	res := c.Compare(context.Background(), req)
	if res.Stage != string(admission.StageProvider) || res.Reason != string(admission.ReasonProviderNotOpenAI) {
		t.Fatalf("expected a provider decline, got stage=%q reason=%q", res.Stage, res.Reason)
	}
	// No plan_compare series exist at all.
	if fams, _ := reg.Gather(); hasFamily(fams, "baml_rest_debaml_plan_compare_total") {
		t.Error("admission decline must record no plan_compare series")
	}
	// The decline was counted.
	if got := counterVal(t, reg, "baml_rest_debaml_declines_total", map[string]string{"stage": "provider", "reason": "provider_not_openai"}); got != 1 {
		t.Errorf("declines{provider,provider_not_openai} = %v, want 1", got)
	}
	assertNoSocket(t, ct)
}

// TestComparator_ProductionRegistryMapping proves a production-shaped effective
// registry (a named primary among a single resolved openai client with the
// transport trio) maps into the native config path and admits.
func TestComparator_ProductionRegistryMapping(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(nil)
	// A realistic dynamic registry: primary names the sole openai client, values
	// are real-looking (still fake) literals — no ambient env, no extra options.
	req.Registry = &bamlutils.ClientRegistry{
		Primary: sp("Prod"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "Prod",
			Provider: "openai",
			Options: map[string]any{
				"model":    planassert.FenceModel,
				"base_url": planassert.FenceBaseURL,
				"api_key":  planassert.FenceAPIKey,
			},
		}},
	}
	ref := referenceNativePlan(t, req)
	req.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
		return bamlFromNative(ref, nil), nil
	}

	res := c.Compare(context.Background(), req)
	if res.Reason != reasonServedBAML {
		t.Fatalf("production-shaped registry did not admit: stage=%q reason=%q", res.Stage, res.Reason)
	}
	if got := planCompareVal(t, reg, "match", "body"); got != 1 {
		t.Errorf("plan_compare{match,body} = %v, want 1", got)
	}
	assertNoSocket(t, ct)
}

// TestComparePlans_SchemeDriftIsHostMismatch proves a pure scheme drift
// (http:// vs https://, same host/path/body/headers) is caught as a HOST
// mismatch, not silently recorded as a full match. The transport scheme is part
// of the effective destination, so a rewrite that only flipped the scheme must
// still surface.
func TestComparePlans_SchemeDriftIsHostMismatch(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	native := nativeExact(body) // https://…
	baml := bamlExact(body, func(r *llmhttp.Request) {
		r.URL = strings.Replace(planassert.WantURL, "https://", "http://", 1)
	})
	cmp := comparePlans(baml, native)
	if cmp.Host {
		t.Fatal("expected a host mismatch on a scheme drift (http vs https)")
	}
	if !cmp.Method || !cmp.Target || !cmp.Headers || !cmp.Body {
		t.Fatalf("only host should differ on a pure scheme drift, got %+v", cmp)
	}
	joined := strings.Join(cmp.Diffs, "\n")
	if !strings.Contains(joined, "host differs") || strings.Contains(joined, "http://") || strings.Contains(joined, "https://") {
		t.Fatalf("expected a secret-free host diff for the scheme mismatch, got %q", joined)
	}
}

func TestComparePlans_HostCaseIsNotDrift(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	baml := bamlExact(body, func(r *llmhttp.Request) {
		r.URL = strings.Replace(planassert.WantURL, "static-oracle.invalid", "STATIC-ORACLE.INVALID", 1)
	})

	cmp := comparePlans(baml, nativeExact(body))
	if !cmp.allMatch() {
		t.Fatalf("equivalent host casing must match, got %+v", cmp)
	}
}

// TestComparePlans_URLDiffsNeverLeakRawValues proves that query/userinfo
// credentials and malformed URLs participate in exact equality, but never escape
// through the log-safe Diffs diagnostics.
func TestComparePlans_URLDiffsNeverLeakRawValues(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	cases := []struct {
		name       string
		bamlURL    string
		wantHost   bool
		wantTarget bool
		secret     string
	}{
		{
			name:       "query credential",
			bamlURL:    planassert.WantURL + "?api_key=url-query-secret-nevereverlogged",
			wantHost:   true,
			wantTarget: false,
			secret:     "url-query-secret-nevereverlogged",
		},
		{
			name:       "userinfo credential",
			bamlURL:    strings.Replace(planassert.WantURL, "https://", "https://userinfo-secret-nevereverlogged@", 1),
			wantHost:   false,
			wantTarget: false,
			secret:     "userinfo-secret-nevereverlogged",
		},
		{
			name:       "malformed URL",
			bamlURL:    "://malformed.invalid/?token=malformed-url-secret-nevereverlogged",
			wantHost:   false,
			wantTarget: false,
			secret:     "malformed-url-secret-nevereverlogged",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baml := bamlExact(body, func(r *llmhttp.Request) { r.URL = tc.bamlURL })
			cmp := comparePlans(baml, nativeExact(body))
			if cmp.Host != tc.wantHost || cmp.Target != tc.wantTarget {
				t.Fatalf("URL drift classification mismatch: got host=%v target=%v, want host=%v target=%v", cmp.Host, cmp.Target, tc.wantHost, tc.wantTarget)
			}
			joined := strings.Join(cmp.Diffs, "\n")
			if strings.Contains(joined, tc.secret) || strings.Contains(joined, tc.bamlURL) {
				t.Fatalf("URL diagnostic leaked a raw value: %q", joined)
			}
			if !strings.Contains(joined, "target differs") {
				t.Fatalf("expected a field-only target mismatch, got %q", joined)
			}
			if !tc.wantHost && !strings.Contains(joined, "host differs") {
				t.Fatalf("expected a field-only host mismatch, got %q", joined)
			}
		})
	}
}

// TestComparator_RetryOverrideDeclinesBeforeBuild proves a request carrying a
// retry override (a whole-plan shape the single-attempt exact lane would bypass)
// declines at the STRATEGY gate BEFORE any native plan is built and BEFORE BAML's
// plan is obtained: BuildBAMLRequest never runs, no plan_compare series is
// recorded, and the S3 strategy decline is counted.
func TestComparator_RetryOverrideDeclinesBeforeBuild(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(func(context.Context) (*llmhttp.Request, error) {
		t.Fatal("BuildBAMLRequest must not run for a request-retry-override decline")
		return nil, nil
	})
	req.HasRequestRetryOverride = true

	res := c.Compare(context.Background(), req)
	if res.Stage != string(admission.StageStrategy) || res.Reason != string(admission.ReasonRequestRetryOverride) {
		t.Fatalf("expected a strategy/request_retry_override decline, got stage=%q reason=%q", res.Stage, res.Reason)
	}
	if fams, _ := reg.Gather(); hasFamily(fams, "baml_rest_debaml_plan_compare_total") {
		t.Error("a retry-override decline must record no plan_compare series")
	}
	if got := counterVal(t, reg, "baml_rest_debaml_declines_total", map[string]string{"stage": "strategy", "reason": "request_retry_override"}); got != 1 {
		t.Errorf("declines{strategy,request_retry_override} = %v, want 1", got)
	}
	assertNoSocket(t, ct)
}

// TestComparator_URLRewriteOrProxyDeclinesBeforeBuild proves a request whose
// effective send path would rewrite/proxy the target declines BEFORE BAML's plan
// is built. BAML applies rewrites/proxying at execution time (AFTER the captured
// plan is built), so a comparison could otherwise record a false match while BAML
// sends elsewhere — the parity-decline forecloses that. It also proves the
// predicate is evaluated against the EFFECTIVE TARGET admission resolves
// (base_url + /chat/completions), so the proxy decision is the transport's own
// resolver against the REAL target.
func TestComparator_URLRewriteOrProxyDeclinesBeforeBuild(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, _ := admission.NewMetrics(reg)
	ct := &countingTransport{}
	c := NewComparator(m, llmhttp.NewExactExecutor(ct))

	req := shadowRequest(func(context.Context) (*llmhttp.Request, error) {
		t.Fatal("BuildBAMLRequest must not run for a url-rewrite/proxy decline")
		return nil, nil
	})
	var gotURL string
	req.WouldRewriteOrProxy = func(effectiveURL string) bool {
		gotURL = effectiveURL
		return true
	}

	res := c.Compare(context.Background(), req)
	if res.Stage != string(admission.StageStrategy) || res.Reason != string(admission.ReasonURLRewriteOrProxy) {
		t.Fatalf("expected a strategy/url_rewrite_or_proxy decline, got stage=%q reason=%q", res.Stage, res.Reason)
	}
	if want := planassert.FenceBaseURL + "/chat/completions"; gotURL != want {
		t.Errorf("WouldRewriteOrProxy evaluated against %q, want the effective target %q", gotURL, want)
	}
	if fams, _ := reg.Gather(); hasFamily(fams, "baml_rest_debaml_plan_compare_total") {
		t.Error("a url-rewrite/proxy decline must record no plan_compare series")
	}
	if got := counterVal(t, reg, "baml_rest_debaml_declines_total", map[string]string{"stage": "strategy", "reason": "url_rewrite_or_proxy"}); got != 1 {
		t.Errorf("declines{strategy,url_rewrite_or_proxy} = %v, want 1", got)
	}
	assertNoSocket(t, ct)
}

func hasFamily(fams []*dto.MetricFamily, name string) bool {
	for _, mf := range fams {
		if mf.GetName() == name && len(mf.GetMetric()) > 0 {
			return true
		}
	}
	return false
}
