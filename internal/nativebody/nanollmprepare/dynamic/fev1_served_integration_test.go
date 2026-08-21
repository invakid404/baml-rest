//go:build integration && nanollm_integration

package dynamic

// De-BAML serving cutover S3b — the SERVED-PATH fe-v1 proof: the first native
// traffic this repository permits, proved against stock BAML v0.223.
//
// # What is under proof, and why it is not the S1/S3a suites again
//
// S1 built the default-deny gate and proved a native-capable worker enrolled
// nothing. S3a wired the trusted effective-configuration identity resolver and
// proved a SEALED configuration still claimed nothing. S3b adds the one thing
// neither could: an ENROLLMENT. So the question changes from "does it decline?"
// to "when it serves, is what the caller gets still exactly what BAML v0.223
// would have produced, at the cost of exactly ONE provider request?".
//
// Everything here therefore runs through the SHIPPED policy — no injected gate,
// no proof fingerprint, no canary.NewServerWithCohortIdentity. The identity comes
// from the deployment's own approved-configuration declaration
// (bamlutils/trustedclients), applied by the REAL worker config-load sealing pass,
// and the serve implementation is the one the production factory builds
// (nativeserve.New). If the shipped enrollment were removed, every positive
// assertion in this file would fail.
//
// # The differential
//
// Both legs are the SAME public dynamic `/call` request — same messages, same
// output schema, same deployment declaration, same named-only client_registry —
// against ONE loopback capture server that serves ONE deterministic response:
//
//	stock leg:  BAML_REST_USE_DEBAML off, no native callback at all
//	            -> BAML v0.223 BuildRequest -> baml-rest llmhttp SEND
//	native leg: flag on, nativeserve.New installed as the serve implementation
//	            -> identity resolves fe-v1 -> the shipped policy enrolls it
//	            -> strict render/schema/Prepare + BAML no-send plan equality
//	            -> exactly ONE native RoundTrip
//	            -> BAML parses THOSE SAME BYTES and the results are compared
//
// The capture server records both requests, so the comparison is over the actual
// wire (method, target, host, byte-exact body, semantic header multimap) and over
// the client-visible envelope (structured data incl. field order, error text),
// not over a disposition token.
//
// # The invariants asserted on every positive arm
//
//   - the provider saw exactly ONE request for the native leg (one native
//     RoundTrip, ZERO BAML resend);
//   - winner_engine=native, and ZERO parse-only winners;
//   - claimed == native_sockets == 1;
//   - BOTH retained BAML oracles RAN and MATCHED — plan_compare on the pre-claim
//     no-send plan, response_compare on the same response bytes;
//   - the cohort label is the enrolled bucket, and the surface is dynamic_call.
//
// Gated `integration && nanollm_integration` (BAML CFFI + nanollm). The
// booted-artifact HTTP twin of this file — same enrollment, real subprocess
// worker, real listener, the worker's OWN collectors — lives in
// cmd/serve/native_artifact_route_proof_test.go.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// feV1ClientName is the client name the deployment's declaration approves. A
// request may NAME it; it may never define it.
const feV1ClientName = "FeV1Approved"

// feV1Fingerprint is the opaque slot the SHIPPED policy enrolls. It is read from
// the admission package rather than re-spelled, so a change to the enrolled slot
// moves this proof with it instead of leaving it asserting a stale literal.
var feV1Fingerprint = string(admission.FeV1ConfigFingerprint)

// feV1Declaration is the deployment's approved-configuration declaration for the
// loopback oracle configuration: the strict transport trio, owned by the
// deployment rather than by the request.
//
// fingerprint, name and provider are parameters so the admission matrix can
// declare the SAME configuration under a DIFFERENT (unenrolled) slot, a different
// name, or a provider CLASS the enrolled record does not name, and prove that none
// of those inherits the approved cohort.
func feV1Declaration(t *testing.T, base, fingerprint, name, provider string) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"` + name + `",
		"fingerprint":"` + fingerprint + `",
		"provider":"` + provider + `",
		"options":{"model":"` + fenceModel + `","base_url":"` + base + `/v1","api_key":"` + fenceAPIKey + `"}
	}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	return set
}

// feV1NamedOnlyRegistry is what a request that merely NAMES the approved class
// looks like: a primary and a client name, nothing else. The worker's sealing
// pass installs the deployment's provider and options onto it.
func feV1NamedOnlyRegistry(name string) *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp(name),
		Clients: []*dynclient.ClientProperty{{Name: name}},
	}
}

// feV1Opts is one arm of the proof. The zero value is deliberately the
// FLAG-OFF, UNDECLARED, no-native-callback arm, so every arm has to opt IN to the
// things that could make it claim.
type feV1Opts struct {
	// server is the shared loopback capture server both legs hit, so their wire
	// requests can be compared directly.
	server *liveCaptureServer
	// fixture selects the dynamic messages + output schema.
	fixture dynFixture
	// schema overrides the fixture's output schema (the unsupported-schema arm).
	schema *bamlutils.DynamicOutputSchema

	// declare installs the deployment's approved-configuration declaration.
	declare bool
	// fingerprint / declaredName / declaredProvider override what the declaration
	// says. Empty means the enrolled slot, the approved name and `openai`.
	fingerprint      string
	declaredName     string
	declaredProvider string

	// registryFor overrides the request's client_registry. It is a FUNCTION of the
	// loopback base because a caller-defined registry has to carry this run's own
	// base URL, and a registry built before the server existed would carry a stale
	// one that declines for the wrong reason.
	registryFor func(base string) *dynclient.ClientRegistry

	// retryOverride attaches a REQUEST retry override, which makes the effective
	// selected leaf not a single proven answer.
	retryOverride bool
	// rewriteBaseURL installs a base-URL rewrite so the effective send path would
	// rewrite the outbound target.
	rewriteBaseURL bool
	// mediaPart replaces the prompt with one carrying a MEDIA part, a render shape
	// the native renderer does not claim.
	mediaPart bool

	// flagOn is the ONE global umbrella switch.
	flagOn bool
	// native installs the PRODUCTION serve implementation (nativeserve.New).
	native bool

	// mutateBAMLPlan rewrites BAML's NO-SEND request plan at the serve seam,
	// immediately before the strict pre-claim plan comparison. It is the oracle
	// control for "a plan difference must produce NO native send".
	mutateBAMLPlan func(*llmhttp.Request)
	// mutateBAMLParse rewrites what BAML's same-bytes parse returns, immediately
	// before the post-response comparison. It is the oracle control for "a
	// structured/order difference must produce the separately labelled BAML-parse
	// outcome, never a native winner".
	mutateBAMLParse func([]byte) []byte
	// bamlPlanFailure breaks the pre-claim plan oracle's INPUT rather than its
	// content: the closure that builds BAML's no-send plan errors, returns nil, or
	// is absent entirely. Each is a way of NOT having BAML's plan to compare
	// against, and the strict anchor must decline pre-socket for all three rather
	// than claim on an unverified plan.
	bamlPlanFailure feV1BAMLPlanFailure

	// withRaw drives DynamicCallRaw (ModeCallWithRaw), which fe-v1 does not enroll.
	withRaw bool

	status int
	body   []byte
}

// feV1BAMLPlanFailure selects how the BAML no-send plan fails to arrive.
type feV1BAMLPlanFailure string

const (
	feV1BAMLPlanOK     feV1BAMLPlanFailure = ""
	feV1BAMLPlanErrors feV1BAMLPlanFailure = "error"
	feV1BAMLPlanNil    feV1BAMLPlanFailure = "nil"
	feV1BAMLPlanAbsent feV1BAMLPlanFailure = "absent"
)

// feV1Result is everything one arm observes: what the caller saw, what the
// provider saw, and what the serve implementation's own collectors recorded.
type feV1Result struct {
	observed observedCall
	// providerRequestsThisLeg is the number of requests THIS leg put on the wire,
	// which is what "exactly one native RoundTrip, zero BAML resend" is read from.
	providerRequestsThisLeg int
	reg                     *prometheus.Registry
}

// counter sums the serve implementation's own counter family for the given labels.
func (r feV1Result) counter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	if r.reg == nil {
		return 0
	}
	return deployedCounter(t, r.reg, name, labels)
}

// dynamicCall is the (surface, cohort) label pair every enrolled series carries.
func feV1Labels(extra map[string]string) map[string]string {
	out := map[string]string{
		"surface": admission.SurfaceDynamicCall.Label(),
		"cohort":  string(admission.FeV1Cohort),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// runFeV1Call issues ONE DynamicCall through the generated seam against the
// SHIPPED admission policy, with the deployment's declaration threaded through the
// REAL worker config-load sealing pass.
func runFeV1Call(t *testing.T, opts feV1Opts) feV1Result {
	t.Helper()
	if opts.server == nil {
		t.Fatal("runFeV1Call needs a capture server: the wire differential reads both legs off one")
	}
	fixture := opts.fixture
	if fixture.name == "" {
		fixture = dynFixtureByName(t, "single_user_message")
	}
	schema := opts.schema
	if schema == nil {
		schema = fixture.schema
	}
	opts.server.setResponse(opts.status, opts.body)
	before := opts.server.count()

	// The PRODUCTION factory on its own registry: nativeserve.New is the
	// constructor workerboot reaches, presenting no injected gate and no proof
	// identity. Built even for the arms that do not install it, so a mistake that
	// silently dropped the callback shows up as serveCalls == 0 rather than as a
	// nil dereference.
	reg := prometheus.NewRegistry()
	serveFn, err := nativeserve.New(reg)
	if err != nil {
		t.Fatalf("nativeserve.New: %v", err)
	}

	var serveCalls atomic.Int64
	dynOpts := []dynclient.Option{
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(opts.flagOn),
		dynclient.WithDeBAMLRenderer(debaml.Render),
	}
	if opts.retryOverride {
		dynOpts = append(dynOpts, dynclient.WithRequestRetryOverride(&dynclient.RetryConfig{MaxRetries: 2, Strategy: "constant_delay", DelayMs: 1}))
	}
	if opts.rewriteBaseURL {
		// A rewrite that does not change the effective destination (the loopback
		// base maps to itself with a trailing marker path removed) — the POINT is
		// that a rewrite rule EXISTS on the effective send path, not that it sends
		// the request somewhere else. Rewriting to a different host would prove a
		// dial guard rather than the admission predicate.
		dynOpts = append(dynOpts, dynclient.WithBaseURLRewrites([]dynclient.BaseURLRewriteRule{
			{From: opts.server.base(), To: opts.server.base()},
		}))
	}
	if opts.native {
		dynOpts = append(dynOpts, dynclient.WithNativeServeComparator(
			func(ctx context.Context, req bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
				serveCalls.Add(1)
				applyFeV1OracleMutations(&req, opts)
				return serveFn(ctx, req)
			}))
	}
	if opts.declare {
		fp := opts.fingerprint
		if fp == "" {
			fp = feV1Fingerprint
		}
		name := opts.declaredName
		if name == "" {
			name = feV1ClientName
		}
		provider := opts.declaredProvider
		if provider == "" {
			provider = "openai"
		}
		dynOpts = append(dynOpts, dynclient.WithTrustedClients(feV1Declaration(t, opts.server.base(), fp, name, provider)))
	}
	client, err := dynclient.New(dynOpts...)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	registry := feV1NamedOnlyRegistry(feV1ClientName)
	if opts.registryFor != nil {
		registry = opts.registryFor(opts.server.base())
	}
	messages := toDynMessages(fixture.messages)
	if opts.mediaPart {
		messages = feV1MediaMessages()
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	req := dynclient.Request{
		Messages:            messages,
		ClientRegistry:      registry,
		OutputSchema:        schema,
		PreserveSchemaOrder: bptr(true),
	}

	out := feV1Result{reg: reg}
	if opts.withRaw {
		res, callErr := client.DynamicCallRaw(ctx, req)
		if callErr != nil {
			out.observed.errText = callErr.Error()
		}
		if res != nil {
			out.observed.data = string(res.Data)
			out.observed.winnerEngine, out.observed.plannedEngine = lastOutcomeEngineRaw(res)
		}
	} else {
		res, callErr := client.DynamicCall(ctx, req)
		if callErr != nil {
			out.observed.errText = callErr.Error()
		}
		if res != nil {
			out.observed.data = string(res.Data)
			out.observed.winnerEngine, out.observed.plannedEngine = lastOutcomeEngine(res)
		}
	}

	out.providerRequestsThisLeg = opts.server.count() - before
	out.observed.providerRequests = int64(out.providerRequestsThisLeg)
	out.observed.serveCalls = serveCalls.Load()
	return out
}

// lastOutcomeEngineRaw is lastOutcomeEngine for the /call-with-raw result shape.
func lastOutcomeEngineRaw(res *dynclient.CallRawResult) (winner, planned string) {
	for i := range res.Metadata {
		md := res.Metadata[i]
		if md.Phase == bamlutils.MetadataPhaseOutcome {
			winner, planned = md.WinnerEngine, md.PlannedEngine
		}
	}
	return winner, planned
}

// errFeV1BAMLPlanBuild is the synthetic BAML plan-build failure the pre-claim
// oracle control injects.
var errFeV1BAMLPlanBuild = errors.New("fe-v1 control: BAML could not build its no-send plan")

// applyFeV1OracleMutations installs the ORACLE controls at the serve seam.
//
// Both mutate BAML's side of a comparison rather than native's, and that is the
// only side this seam can drive deterministically: the native plan is built by
// nanollm from the same sealed configuration, and the native response is whatever
// the one provider request returned. What the controls are actually asserting is a
// property of the COMPARISON — that a difference between the two sides, from
// whichever side it originates, refuses the native outcome. A mutation on the BAML
// side is indistinguishable to the comparator from the same mutation on the native
// side, which is precisely why it is a valid control here.
func applyFeV1OracleMutations(req *bamlutils.NativeServeRequest, opts feV1Opts) {
	if opts.mutateBAMLPlan != nil && req.BuildBAMLRequest != nil {
		inner := req.BuildBAMLRequest
		req.BuildBAMLRequest = func(ctx context.Context) (*llmhttp.Request, error) {
			plan, err := inner(ctx)
			if err != nil || plan == nil {
				return plan, err
			}
			opts.mutateBAMLPlan(plan)
			return plan, nil
		}
	}
	switch opts.bamlPlanFailure {
	case feV1BAMLPlanErrors:
		req.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
			return nil, errFeV1BAMLPlanBuild
		}
	case feV1BAMLPlanNil:
		// A nil plan with a nil error: the closure "succeeded" and produced
		// nothing to compare, which must be treated exactly like an error.
		req.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) { return nil, nil }
	case feV1BAMLPlanAbsent:
		req.BuildBAMLRequest = nil
	}
	if opts.mutateBAMLParse != nil && req.BAMLOnlyParse != nil {
		inner := req.BAMLOnlyParse
		req.BAMLOnlyParse = func(ctx context.Context, raw string) ([]byte, error) {
			got, err := inner(ctx, raw)
			if err != nil {
				return got, err
			}
			return opts.mutateBAMLParse(got), nil
		}
	}
}

// --- shared assertions -------------------------------------------------------

// assertFeV1NativeWin is the S3b serving guarantee, stated once: one native
// RoundTrip, zero BAML resend, a native winner, zero parse-only winners, and both
// retained BAML oracles run and matched.
func assertFeV1NativeWin(t *testing.T, label string, got feV1Result) {
	t.Helper()
	if got.providerRequestsThisLeg != 1 {
		t.Errorf("%s: the provider saw %d request(s) for this leg, want exactly 1 (one native RoundTrip, ZERO BAML resend)",
			label, got.providerRequestsThisLeg)
	}
	if got.observed.serveCalls != 1 {
		t.Errorf("%s: the generated seam invoked the native callback %d time(s), want 1", label, got.observed.serveCalls)
	}
	if got.observed.winnerEngine != bamlutils.NativeServeEngineNative {
		t.Errorf("%s: winner_engine = %q, want %q", label, got.observed.winnerEngine, bamlutils.NativeServeEngineNative)
	}
	if got.observed.plannedEngine != "native" {
		t.Errorf("%s: planned_engine = %q, want native", label, got.observed.plannedEngine)
	}

	if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseClaimed)})); v != 1 {
		t.Errorf("%s: admission_phase{cohort=fe_v1,phase=claimed} = %v, want 1", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerNative)})); v != 1 {
		t.Errorf("%s: winner{cohort=fe_v1,winner=native} = %v, want 1", label, v)
	}
	// ZERO PARSE-ONLY WINNERS — the acceptance criterion fe-v1 is gated on. A
	// parse-only win means native transported but BAML's parse of the same bytes
	// produced the served value, which is a promotion blocker rather than a success.
	if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerBAMLParseSameResponse)})); v != 0 {
		t.Errorf("%s: winner{winner=baml_parse_same_response} = %v, want 0 — a successful fe-v1 request has zero parse-only winners", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_fallback_total", map[string]string{"kind": "parse_only"}); v != 0 {
		t.Errorf("%s: fallback{kind=parse_only} = %v, want 0", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerFailure)})); v != 0 {
		t.Errorf("%s: winner{winner=failure} = %v, want 0", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_native_sockets_total", map[string]string{"flag": "on"}); v != 1 {
		t.Errorf("%s: native_sockets{flag=on} = %v, want exactly 1 (claimed == sockets)", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_native_sockets_total", map[string]string{"flag": "off"}); v != 0 {
		t.Errorf("%s: native_sockets{flag=off} = %v, want 0 (paging invariant)", label, v)
	}

	// BOTH retained oracles RAN and MATCHED. "No mismatch" alone would also be true
	// of an oracle that never ran, which is exactly the regression that would make
	// the enrollment unsafe, so each is asserted positively.
	if v := got.counter(t, "baml_rest_debaml_plan_compare_total", map[string]string{"result": "match"}); v == 0 {
		t.Errorf("%s: the pre-claim BAML no-send plan comparison never recorded a match — the strict oracle did not run", label)
	}
	if v := got.counter(t, "baml_rest_debaml_plan_compare_total", map[string]string{"result": "mismatch"}); v != 0 {
		t.Errorf("%s: plan_compare{mismatch} = %v, want 0", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseSameResponseOracle)})); v != 1 {
		t.Errorf("%s: admission_phase{phase=same_response_oracle} = %v, want 1 — BAML must parse the same bytes", label, v)
	}
	for _, field := range []admission.ResponseCompareField{
		admission.ResponseCompareFieldStructured,
		admission.ResponseCompareFieldOrder,
		admission.ResponseCompareFieldAssistant,
	} {
		if v := got.counter(t, "baml_rest_debaml_response_compare_total", map[string]string{"result": "match", "field": string(field)}); v != 1 {
			t.Errorf("%s: response_compare{match,%s} = %v, want 1", label, field, v)
		}
		if v := got.counter(t, "baml_rest_debaml_response_compare_total", map[string]string{"result": "mismatch", "field": string(field)}); v != 0 {
			t.Errorf("%s: response_compare{mismatch,%s} = %v, want 0", label, field, v)
		}
	}
	// The pre-claim decline buckets must be untouched: a claim and a decline for
	// the same request would mean the terminal accounting is double-counting.
	for _, cohort := range []string{string(admission.CohortNone), string(admission.CohortUnrecognized), string(admission.FeV1Cohort)} {
		labels := map[string]string{"surface": admission.SurfaceDynamicCall.Label(), "cohort": cohort, "phase": string(admission.PhasePreclaimDecline)}
		if v := got.counter(t, "baml_rest_debaml_admission_phase_total", labels); v != 0 {
			t.Errorf("%s: preclaim_decline{cohort=%s} = %v, want 0 on a claimed request", label, cohort, v)
		}
	}
}

// --- 1. the served-path stock differential -----------------------------------

// TestFeV1ServedPathMatchesStockBAML is the S3b headline: the exact enrolled
// fe-v1 dynamic `/call`, run through stock BAML v0.223 and through the
// native-capable serve path, produces the same client-visible result off the same
// wire request — with the native leg making ONE upstream request and winning
// natively.
func TestFeV1ServedPathMatchesStockBAML(t *testing.T) {
	for _, fx := range liveFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			body := openAISuccess(fx.content)

			// Leg 1: stock BAML v0.223. Flag off, no native callback installed at
			// all — the shape a BAML-only worker presents to the orchestrator.
			stock := runFeV1Call(t, feV1Opts{
				server: server, fixture: fx.dynFixture, declare: true,
				status: http.StatusOK, body: body,
			})
			if stock.providerRequestsThisLeg != 1 {
				t.Fatalf("stock BAML leg made %d provider request(s), want 1", stock.providerRequestsThisLeg)
			}
			if stock.observed.errText != "" {
				t.Fatalf("stock BAML leg errored on an admitted fixture: %s", stock.observed.errText)
			}
			if stock.observed.winnerEngine != "" || stock.observed.plannedEngine != "" {
				t.Fatalf("the BAML-only leg advertised engines (winner=%q planned=%q), want none",
					stock.observed.winnerEngine, stock.observed.plannedEngine)
			}

			// Leg 2: the native-capable path with the SHIPPED enrollment.
			native := runFeV1Call(t, feV1Opts{
				server: server, fixture: fx.dynFixture, declare: true,
				flagOn: true, native: true,
				status: http.StatusOK, body: body,
			})
			assertFeV1NativeWin(t, "fe-v1 served", native)

			// The CLIENT-VISIBLE envelope: same structured data (including field
			// order under preserve_schema_order), same absence of an error.
			if native.observed.errText != "" {
				t.Fatalf("fe-v1 served leg errored: %s", native.observed.errText)
			}
			assertStructuredParity(t, fx.schema, []byte(stock.observed.data), []byte(native.observed.data))

			// The WIRE: one request each, compared byte-for-byte and header-for-header.
			assertLiveWireParity(t, server.rec(0), server.rec(1))
			if diffs := liveWireDiffs(server.rec(0), server.rec(1)); len(diffs) != 0 {
				t.Fatalf("fe-v1 served wire differs from stock BAML v0.223:\n  %s", strings.Join(diffs, "\n  "))
			}
			if got := server.count(); got != 2 {
				t.Fatalf("the provider saw %d requests across BOTH legs, want exactly 2 (one each)", got)
			}
		})
	}
}

// TestFeV1ServedPathPreservesTheProviderErrorEnvelope is the differential's error
// arm: an upstream non-2xx must reach the caller as the SAME error envelope on
// both legs, and the native leg must still make exactly one upstream request with
// no BAML resend behind it.
func TestFeV1ServedPathPreservesTheProviderErrorEnvelope(t *testing.T) {
	server := newLiveCaptureServer(t)
	body := []byte(`{"error":{"message":"slow down","type":"rate_limit"}}`)

	stock := runFeV1Call(t, feV1Opts{server: server, declare: true, status: http.StatusTooManyRequests, body: body})
	native := runFeV1Call(t, feV1Opts{server: server, declare: true, flagOn: true, native: true, status: http.StatusTooManyRequests, body: body})

	if stock.observed.errText == "" {
		t.Fatal("the stock BAML leg did not error on a 429; the comparison would be vacuous")
	}
	if stock.observed.errText != native.observed.errText {
		t.Errorf("provider-error envelope differs:\n  stock  = %q\n  native = %q", stock.observed.errText, native.observed.errText)
	}
	// ONE upstream request on the claimed leg. A post-claim failure must NEVER
	// silently re-send: that is the ownership boundary, and the counter is the
	// evidence.
	if native.providerRequestsThisLeg != 1 {
		t.Errorf("the claimed leg made %d upstream request(s) on a 429, want exactly 1 (no post-claim resend)", native.providerRequestsThisLeg)
	}
	if v := native.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseClaimed)})); v != 1 {
		t.Errorf("claimed = %v, want 1 — the request was claimed and then failed, not declined", v)
	}
	if v := native.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerNative)})); v != 0 {
		t.Errorf("winner{native} = %v on a provider error, want 0", v)
	}
	// A post-claim failure is a FAILURE, never a pre-claim decline: a decline here
	// would mean BAML was invited to re-send a request native already sent.
	if v := native.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerFailure)})); v != 1 {
		t.Errorf("winner{failure} = %v, want 1 — a post-claim provider error must terminate as a failure", v)
	}
	if v := native.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhasePreclaimDecline)})); v != 0 {
		t.Errorf("preclaim_decline = %v after a CLAIM, want 0 — a post-claim decline would trigger a hidden BAML resend", v)
	}
}

// --- 2. the flag-off proof ---------------------------------------------------

// TestFeV1FlagOffIsZeroNative is the kill-switch proof WITH the enrollment
// present and the configuration sealed: BAML_REST_USE_DEBAML off means the native
// callback is never invoked, no native work runs, no socket opens, and the caller
// sees ordinary BAML.
//
// It is the reversal the whole cutover rests on. The enrollment is admission
// EVIDENCE, not a second switch: turning the flag off must be sufficient on its
// own, with no policy change at all.
func TestFeV1FlagOffIsZeroNative(t *testing.T) {
	for _, c := range equivalenceCases() {
		t.Run(c.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			stock := runFeV1Call(t, feV1Opts{server: server, declare: true, status: c.status, body: c.body})
			// Flag OFF but the serve implementation still constructed and the
			// configuration still declared — everything except the umbrella switch.
			off := runFeV1Call(t, feV1Opts{server: server, declare: true, native: true, status: c.status, body: c.body})

			if off.observed.serveCalls != 0 {
				t.Errorf("the flag-off seam invoked the native callback %d time(s); the umbrella switch must install none", off.observed.serveCalls)
			}
			if off.observed.plannedEngine != "" || off.observed.winnerEngine != "" {
				t.Errorf("the flag-off outcome advertises engines (planned=%q winner=%q); the native lane must not be advertised",
					off.observed.plannedEngine, off.observed.winnerEngine)
			}
			if off.providerRequestsThisLeg != 1 {
				t.Errorf("the flag-off leg made %d provider request(s), want exactly 1 (BAML's)", off.providerRequestsThisLeg)
			}
			// Not one de-BAML series moved: no admission ran, so no cohort was even
			// resolved. This is stronger than "zero claims" — it is "zero work".
			for _, name := range []string{
				"baml_rest_debaml_admission_phase_total",
				"baml_rest_debaml_winner_total",
				"baml_rest_debaml_native_sockets_total",
				"baml_rest_debaml_plan_compare_total",
				"baml_rest_debaml_response_compare_total",
				"baml_rest_debaml_declines_total",
				"baml_rest_debaml_attempts_total",
			} {
				if v := off.counter(t, name, nil); v != 0 {
					t.Errorf("flag off: %s summed to %v, want 0 — no native admission may run at all", name, v)
				}
			}
			assertExternallyEquivalent(t, "flag off with the fe-v1 enrollment present", stock.observed, off.observed)
		})
	}
}

// --- 3. telemetry bounds -----------------------------------------------------

// TestFeV1ServedTelemetryStaysBounded reads every de-BAML series a SERVED fe-v1
// request produced and requires each label value to be a bounded bucket, with no
// configuration name, model, URL, credential, prompt or response content anywhere
// in it.
//
// The forbidden corpus is the actual values this request carried, so the check is
// about THIS request's own secrets rather than about a generic pattern: the fence
// api key, the fence model, the loopback base URL, the approved client name, the
// prompt text and the served answer all have to be absent.
func TestFeV1ServedTelemetryStaysBounded(t *testing.T) {
	server := newLiveCaptureServer(t)
	got := runFeV1Call(t, feV1Opts{
		server: server, declare: true, flagOn: true, native: true,
		status: http.StatusOK, body: openAISuccess(`{"answer":"ok"}`),
	})
	assertFeV1NativeWin(t, "telemetry arm", got)

	families, err := got.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	forbidden := []string{
		fenceAPIKey, fenceModel, server.base(), feV1ClientName,
		"What is 2+2?", "answer", "chat/completions", "Bearer",
	}
	seen := 0
	for _, mf := range families {
		if !strings.HasPrefix(mf.GetName(), "baml_rest_debaml_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				seen++
				for _, f := range forbidden {
					if f == "" {
						continue
					}
					if strings.Contains(lp.GetValue(), f) || strings.Contains(lp.GetName(), f) {
						t.Errorf("%s{%s=%q} carries request-derived material %q", mf.GetName(), lp.GetName(), lp.GetValue(), f)
					}
				}
				// Bounded shape: a de-BAML label value is a short lowercase token
				// from a declared enum or a predeclared bucket. Nothing that can
				// spell a URL, a header, a key or a sentence fits.
				if len(lp.GetValue()) > 48 {
					t.Errorf("%s{%s} label value is %d bytes; de-BAML labels are bounded buckets", mf.GetName(), lp.GetName(), len(lp.GetValue()))
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no de-BAML label was gathered; the redaction check is vacuous")
	}

	// The cohort/policy identity an operator scrapes is exactly the enrolled one.
	if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerNative)})); v != 1 {
		t.Errorf("the native win was not attributed to the enrolled cohort bucket (got %v)", v)
	}
	assertFeV1PolicyInfoPublished(t, got.reg)
}

// assertFeV1PolicyInfoPublished pins the operator-visible half: the shipped policy
// version and its enrollment count, plus the one inventory row.
func assertFeV1PolicyInfoPublished(t *testing.T, reg *prometheus.Registry) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var policyRows, inventoryRows int
	for _, mf := range families {
		switch mf.GetName() {
		case "baml_rest_debaml_cohort_policy_info":
			for _, m := range mf.GetMetric() {
				policyRows++
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "version" && lp.GetValue() != admission.ProductionCohortPolicyVersion {
						t.Errorf("cohort_policy_info version = %q, want %q", lp.GetValue(), admission.ProductionCohortPolicyVersion)
					}
				}
				if m.GetGauge().GetValue() != 1 {
					t.Errorf("cohort_policy_info = %v enrollments, want exactly 1", m.GetGauge().GetValue())
				}
			}
		case "baml_rest_debaml_config_inventory_info":
			for _, m := range mf.GetMetric() {
				inventoryRows++
				labels := map[string]string{}
				for _, lp := range m.GetLabel() {
					labels[lp.GetName()] = lp.GetValue()
				}
				want := map[string]string{
					"fingerprint": feV1Fingerprint,
					"cohort":      string(admission.FeV1Cohort),
					"surface":     admission.SurfaceDynamicCall.Label(),
					"provider":    string(admission.ConfigProviderOpenAI),
				}
				for k, v := range want {
					if labels[k] != v {
						t.Errorf("config_inventory_info %s = %q, want %q", k, labels[k], v)
					}
				}
				if labels["approval"] == "" {
					t.Error("config_inventory_info carries no approval reference; the enrollment must be joinable to its offline approval")
				}
			}
		}
	}
	if policyRows != 1 {
		t.Errorf("cohort_policy_info published %d series, want 1", policyRows)
	}
	if inventoryRows != 1 {
		t.Errorf("config_inventory_info published %d rows, want exactly 1 (the fe-v1 record on its one surface)", inventoryRows)
	}
}

// feV1MediaMessages is a prompt carrying a MEDIA part. The native renderer does
// not claim media, so it is the served-path arm for "unsupported render shape
// declines pre-Prepare" — and it is expressed as a generated dynamic message
// because that is the only place the dynamic `/call` surface can carry one.
func feV1MediaMessages() []dynclient.Message {
	text := "Describe this."
	return []dynclient.Message{{
		Role: "user",
		PartsContent: []dynclient.ContentPart{
			{Type: "text", Text: &text},
			{Type: "image", Image: &dynclient.MediaInput{URL: sp("https://example.invalid/cat.png")}},
		},
	}}
}
