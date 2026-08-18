//go:build integration && nanollm_integration

package dynamic

// De-BAML serving cutover S1 — the EXTERNAL-EQUIVALENCE proof.
//
// The claim S1 makes is not "the native callback returns Declined". It is that a
// native-capable worker with NO cohort enrolled is EXTERNALLY EQUIVALENT to BAML
// transport: the caller gets the same result, the same error, and the provider sees
// the same requests. A cold review pointed out — correctly — that the first draft
// only checked the callback's disposition and never ran BAML for the same request,
// so it could not have noticed a difference if there had been one.
//
// This drives the REAL generated dynamic call seam (dynclient + patched BAML + the
// nanollm-backed serve implementation built by the PRODUCTION factory) twice over
// the same fixture and the same loopback provider, and compares what an external
// caller can actually see:
//
//   - the served structured output, byte-for-byte;
//   - the returned error (or its absence), including the provider-error envelope;
//   - the number of provider requests;
//   - the routing metadata a client can read.
//
// And it MUTATES: the same comparison run against a serve implementation that IS
// enrolled must FAIL, because enrollment changes observable behaviour. That is what
// makes the equivalence assertion load-bearing rather than a restatement of
// "Policy.Len() == 0".

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/canary"
)

// observedCall is everything an external caller (and the provider) can see about one
// request. Equivalence is defined as equality of this whole struct, not of a
// disposition token.
type observedCall struct {
	data             string
	errText          string
	providerRequests int64
	winnerEngine     string
	plannedEngine    string
	serveCalls       int64
}

// runEquivalenceCall issues one DynamicCall through the generated seam with the
// given serve implementation installed (nil = no native callback at all, which is
// exactly what a flag-off / BAML-only worker presents to the orchestrator).
func runEquivalenceCall(t *testing.T, serve bamlutils.NativeServeFunc, deBAMLEnabled bool, status int, body []byte) observedCall {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(status, body)

	var serveCalls atomic.Int64
	opts := []dynclient.Option{
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(loopbackOracleHTTPClient()),
		dynclient.WithDeBAML(deBAMLEnabled),
		dynclient.WithDeBAMLRenderer(debaml.Render),
	}
	if serve != nil {
		opts = append(opts, dynclient.WithNativeServeComparator(
			func(ctx context.Context, req bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
				serveCalls.Add(1)
				return serve(ctx, req)
			}))
	}
	client, err := dynclient.New(opts...)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      liveOracleRegistry(server.base()),
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})

	out := observedCall{providerRequests: int64(server.count()), serveCalls: serveCalls.Load()}
	if callErr != nil {
		out.errText = callErr.Error()
	}
	if res != nil {
		out.data = string(res.Data)
		out.winnerEngine, out.plannedEngine = lastOutcomeEngine(res)
	}
	return out
}

// productionServeFunc is the serve implementation a native-capable worker actually
// installs: nativeserve.New, which presents the production (zero) configuration
// identity against the shipped EMPTY policy. No test seam, no injected gate.
func productionServeFunc(t *testing.T) bamlutils.NativeServeFunc {
	t.Helper()
	fn, err := nativeserve.New(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("nativeserve.New: %v", err)
	}
	return fn
}

// enrolledServeFunc is the MUTANT: the same serve core with a cohort enrolled
// through the production admission path. Nothing else differs.
func enrolledServeFunc(t *testing.T) bamlutils.NativeServeFunc {
	t.Helper()
	m, err := admission.NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}
	return canary.NewServerWithCohortIdentity(m, llmhttp.NewExactExecutor(nil), admission.ProofCohortInputForTest()).Serve
}

// equivalenceTB lets the mutation bite observe whether the comparison body failed.
type equivalenceTB interface {
	Helper()
	Errorf(string, ...any)
}

type recordingEquivalenceTB struct {
	t      *testing.T
	failed bool
}

func (r *recordingEquivalenceTB) Helper()               {}
func (r *recordingEquivalenceTB) Errorf(string, ...any) { r.failed = true }

// assertExternallyEquivalent compares everything an external caller can observe.
func assertExternallyEquivalent(tb equivalenceTB, label string, baml, native observedCall) {
	tb.Helper()
	if baml.data != native.data {
		tb.Errorf("%s: served output differs — BAML %q vs native-capable %q", label, baml.data, native.data)
	}
	if baml.errText != native.errText {
		tb.Errorf("%s: returned error differs — BAML %q vs native-capable %q", label, baml.errText, native.errText)
	}
	if baml.providerRequests != native.providerRequests {
		tb.Errorf("%s: provider saw %d requests under BAML vs %d under the native-capable worker",
			label, baml.providerRequests, native.providerRequests)
	}
	if baml.winnerEngine != native.winnerEngine {
		tb.Errorf("%s: winner_engine differs — BAML %q vs native-capable %q", label, baml.winnerEngine, native.winnerEngine)
	}
	// planned_engine is DELIBERATELY not in the equality set, and this is the one
	// place the two profiles legitimately differ.
	//
	// The generated seam sets planned_engine="native" whenever a serve callback is
	// INSTALLED — it means "the native lane was consulted", not "native served" — so
	// it has been present on every declining request of a flag-on serve-profile
	// worker since long before this slice (a stream, a non-openai client and a
	// fallback chain all decline and still carry it). Folding it into the equality
	// set would assert that a native-capable artifact must be indistinguishable from
	// a BAML-only one even in its own diagnostics, which is neither true today nor
	// what S1 claims.
	//
	// What S1 claims is that the CALLER's result, the CALLER's error, the provider
	// interaction and the OWNER of the outcome are identical. Those are compared
	// above; planned_engine is asserted explicitly by the caller instead, so the
	// exception is visible rather than hidden in an omission.
}

// equivalenceCases are the observable shapes worth comparing: a clean success and a
// provider error, so the error envelope is compared and not just the happy path.
func equivalenceCases() []struct {
	name   string
	status int
	body   []byte
} {
	return []struct {
		name   string
		status int
		body   []byte
	}{
		{"clean 2xx", http.StatusOK, openAISuccess(`{"answer":"ok"}`)},
		{"provider 429", http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down","type":"rate_limit"}}`)},
	}
}

// TestNoEnrollmentIsExternallyEquivalentToBAML is the S1 headline proof.
func TestNoEnrollmentIsExternallyEquivalentToBAML(t *testing.T) {
	for _, c := range equivalenceCases() {
		t.Run(c.name, func(t *testing.T) {
			// BAML transport: the flag-off worker, which installs no native callback.
			baml := runEquivalenceCall(t, nil, false, c.status, c.body)
			// The native-capable worker with the SHIPPED policy: flag on, serve callback
			// installed through the production factory, nothing enrolled.
			native := runEquivalenceCall(t, productionServeFunc(t), true, c.status, c.body)

			assertExternallyEquivalent(t, c.name, baml, native)

			// The callback really was installed and really did run — otherwise this
			// would be comparing BAML against BAML and proving nothing.
			if native.serveCalls == 0 {
				t.Fatal("the native serve callback was never invoked; the comparison is vacuous")
			}
			// The one legitimate difference, asserted rather than omitted: on a
			// SUCCESSFUL call the native-capable worker advertises that the native lane
			// was CONSULTED. An errored call produces no outcome frame at all, so
			// neither profile carries the diagnostic and the two are identical there
			// too — which is why this is keyed on the error and not asserted blindly.
			switch {
			case native.errText == "":
				if native.plannedEngine != "native" {
					t.Errorf("planned_engine = %q on a successful call, want native (the callback was installed and consulted)", native.plannedEngine)
				}
			default:
				if native.plannedEngine != baml.plannedEngine {
					t.Errorf("planned_engine differs on an errored call — BAML %q vs native-capable %q", baml.plannedEngine, native.plannedEngine)
				}
			}
			if native.winnerEngine != "" {
				t.Errorf("winner_engine = %q with nothing enrolled, want empty — BAML owned the request", native.winnerEngine)
			}
			if baml.plannedEngine != "" {
				t.Errorf("the BAML-only profile advertised planned_engine=%q, want empty", baml.plannedEngine)
			}
		})
	}
}

// TestEnrollmentBreaksExternalEquivalence is the MUTATION. Enroll a cohort through
// the production admission path and the SAME comparison must fail — because native
// then owns the request and an external caller can tell.
//
// This is what the equivalence assertion is for. A test that only checked
// `Policy.Len() == 0` would pass unchanged after an enrollment that silently
// rerouted traffic; this one cannot.
func TestEnrollmentBreaksExternalEquivalence(t *testing.T) {
	c := equivalenceCases()[0]
	baml := runEquivalenceCall(t, nil, false, c.status, c.body)
	enrolled := runEquivalenceCall(t, enrolledServeFunc(t), true, c.status, c.body)

	rec := &recordingEquivalenceTB{t: t}
	assertExternallyEquivalent(rec, "mutant", baml, enrolled)
	if !rec.failed {
		t.Fatal("enrolling a cohort did NOT change anything an external caller can observe: " +
			"the equivalence assertion is vacuous, or enrollment does not actually route traffic")
	}
	// Name the difference explicitly, so the bite is about behaviour rather than
	// about some incidental field: with a cohort enrolled, native plans the request.
	if enrolled.plannedEngine != "native" {
		t.Fatalf("enrolled planned_engine = %q, want native — enrollment must route through the native lane", enrolled.plannedEngine)
	}
	if enrolled.winnerEngine == "" {
		t.Error("enrolled winner_engine is empty; native did not own the outcome")
	}
}
