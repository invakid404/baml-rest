//go:build integration && nanollm_integration

package dynamic

// De-BAML serving cutover S3b — the SERVED-PATH ADMISSION MATRIX and the two
// retained-oracle controls.
//
// fev1_served_integration_test.go proves the one enrolled tuple SERVES. This file
// proves the other half, which is the half that keeps the enrollment honest:
// everything that is not that tuple still DECLINES before a native socket, and the
// two retained BAML oracles still refuse a native outcome when they disagree.
//
// Every arm runs through the SHIPPED policy and the REAL sealing pass — the same
// harness as the positive proof, the same production factory — so a decline here
// is a decline of the DEPLOYED predicate rather than of a hand-built CohortInput.
//
// # What each arm has to show
//
// A decline is only interesting if it declines a request that would OTHERWISE have
// claimed. So every arm asserts all four of:
//
//   - the native callback WAS invoked — otherwise the arm compares BAML against
//     BAML and proves nothing;
//   - ZERO claims, ZERO native winners, ZERO native sockets, ZERO plan oracles;
//   - the caller-visible result is EQUIVALENT to the stock BAML leg;
//   - the provider saw exactly ONE request — BAML's.
//
// # Coverage that lives on the DEPLOYED ROUTE instead
//
// Three families of shape cannot honestly be driven through THIS seam, because
// BAML resolves them onto a route that never offers the dynamic serve callback a
// child attempt. They are not deferred to a gate or a factory: they are proved on
// the BOOTED ARTIFACT's own public routes, as stock/native PAIRS against one
// upstream, in cmd/serve/native_artifact_fev1_differential_test.go —
// TestBootedArtifactPreservesBAMLForEveryUnenrolledShapeOnTheDeployedRoute and
// its stream/static siblings:
//
//   - FALLBACK chains, ROUND ROBIN and LEGACY dispatch — driven as real
//     client_registry shapes through the public `/call` route, each asserting the
//     answer is byte-identical with the flag on and off and that the artifact
//     opened ZERO native sockets. The identity resolver and strategy gate bites
//     over the SHIPPED gate remain in nativeserve/admission/fev1_enrollment_test.go.
//   - the DIRECT-PARSE surface — the artifact's public `/parse` route, which
//     makes no provider request at all.
//   - the DYNAMIC STREAM surface (a different callback entirely,
//     NativeStreamServeFunc) — driven at the worker boundary the fiber `/stream`
//     handler uses. Its seam-level zero-claim proof stays in
//     native_stream_serve_integration_test.go.
//
//   - the STATIC call surface — a real `/call/<Method>` route on a STATIC-CAPABLE
//     booted artifact. Root adapter.go is the overwritten-during-build stub, so
//     that artifact is built from an actual BAML project
//     (internal/nativeprompt/testdata/staticserve_fixture) by
//     scripts/build-s3b-static-fixture-artifact.sh, with the same shipped tags and
//     the same attestation stamp; the arm requires BAML's own send on the wire,
//     byte-identical answers with the flag on and off, zero native sockets, and the
//     static seam's own preclaim_decline. nativeserve/canary/cohort_serve_test.go's
//     four-factory sweep remains as additional coverage of the claiming half.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// feV1CallerDefinedRegistry is the request that DESCRIBES the approved
// configuration instead of naming it: same client name, same provider, same model,
// same base_url, same credential. It must never obtain an identity.
func feV1CallerDefinedRegistry(base string) *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp(feV1ClientName),
		Clients: []*dynclient.ClientProperty{{
			Name:     feV1ClientName,
			Provider: "openai",
			Options: map[string]any{
				"model":    fenceModel,
				"base_url": base + "/v1",
				"api_key":  fenceAPIKey,
			},
		}},
	}
}

// feV1RetryPolicyRegistry NAMES the approved class and attaches a retry policy.
// The sealing pass refuses to seal a client the request partly defined, so it
// carries no identity — and the strategy gate would decline it regardless.
func feV1RetryPolicyRegistry(string) *dynclient.ClientRegistry {
	policy := "SomeRetryPolicy"
	return &dynclient.ClientRegistry{
		Primary: sp(feV1ClientName),
		Clients: []*dynclient.ClientProperty{{Name: feV1ClientName, RetryPolicy: &policy}},
	}
}

// feV1AmbiguousRegistry names no primary and offers two clients: there is no
// single effective selected leaf, so the identity is ambiguous — and ambiguity is
// declined, never guessed.
func feV1AmbiguousRegistry(string) *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Clients: []*dynclient.ClientProperty{
			{Name: feV1ClientName},
			{Name: "SecondApproved"},
		},
	}
}

// feV1UnsupportedSchema is an output schema OUTSIDE the native schema/SAP bounds:
// a property typed by a class the schema never declares. DynamicInput.Validate
// does not resolve references, so the request reaches the seam intact and the
// NATIVE schema build is what refuses it — which is the point of the arm.
func feV1UnsupportedSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("detail", &bamlutils.DynamicProperty{Ref: "NeverDeclaredByThisRequest"}),
		),
	}
}

// feV1MatrixArm is one non-fe-v1 shape.
type feV1MatrixArm struct {
	name string
	// why states the property the arm exists to prove, so a failure reads as the
	// rollout problem it would actually be rather than as a broken fixture.
	why string
	// mutate turns the baseline (declared, flag-on, native) options into this arm.
	mutate func(*feV1Opts)
	// wantStage/wantReason pin the bounded decline the shape must produce. Left
	// empty only where BAML's own resolution order legitimately decides which of
	// two gates reports first; the zero-native assertions still apply.
	wantStage  admission.Stage
	wantReason admission.Reason
	// neverReachesTheSeam marks the shapes BAML resolves onto a route that offers
	// the native callback NOTHING — a client retry policy and an ambiguous registry
	// both change how BAML routes the request before a child attempt exists. That
	// is a STRONGER decline than an admission decline, not a weaker one (the native
	// predicate is never even consulted), so the arm asserts zero callbacks rather
	// than one, and every zero-native assertion still applies.
	neverReachesTheSeam bool
}

func feV1MatrixArms() []feV1MatrixArm {
	return []feV1MatrixArm{
		{
			name: "alternate configuration under an unenrolled slot",
			why: "an OpenAI class the deployment sealed under a DIFFERENT opaque slot must not inherit the " +
				"enrolled cohort — enrollment is per SLOT, never per provider",
			mutate:     func(o *feV1Opts) { o.fingerprint = "cfg001" },
			wantStage:  admission.StageCohort,
			wantReason: admission.ReasonCohortNotEnrolled,
		},
		{
			name: "non-OpenAI provider class on the enrolled slot",
			why: "the record binds the slot to the openai CLASS; a class mismatch folds onto the bounded " +
				"unrecognized bucket instead of the approved cohort",
			mutate:     func(o *feV1Opts) { o.declaredProvider = "anthropic" },
			wantStage:  admission.StageCohort,
			wantReason: admission.ReasonCohortNotEnrolled,
		},
		{
			name: "caller-defined configuration",
			why: "client_registry is the CALLER's document: matching the approved configuration byte for byte " +
				"is matching the mask, not being the configuration",
			mutate:     func(o *feV1Opts) { o.registryFor = feV1CallerDefinedRegistry },
			wantStage:  admission.StageCohort,
			wantReason: admission.ReasonCohortNotEnrolled,
		},
		{
			name: "nothing declared",
			why:  "a deployment that approved no configuration has no fe-v1 traffic, whatever this build enrolls",
			mutate: func(o *feV1Opts) {
				o.declare = false
				o.registryFor = feV1CallerDefinedRegistry
			},
			wantStage:  admission.StageCohort,
			wantReason: admission.ReasonCohortNotEnrolled,
		},
		{
			name:                "client retry policy",
			why:                 "a per-client retry policy is a strategy the single-attempt exact lane would bypass",
			mutate:              func(o *feV1Opts) { o.registryFor = feV1RetryPolicyRegistry },
			neverReachesTheSeam: true,
		},
		{
			name:   "request retry override",
			why:    "a retry override means the effective selected leaf is not one proven answer",
			mutate: func(o *feV1Opts) { o.retryOverride = true },
		},
		{
			name:                "ambiguous client selection",
			why:                 "two clients and no primary is not a single effective selected leaf; ambiguity declines rather than guesses",
			mutate:              func(o *feV1Opts) { o.registryFor = feV1AmbiguousRegistry },
			neverReachesTheSeam: true,
		},
		{
			name:       "base-URL rewrite on the effective send path",
			why:        "a rewritten or proxied effective target is an unproven wire shape; it declines before the engine is built",
			mutate:     func(o *feV1Opts) { o.rewriteBaseURL = true },
			wantStage:  admission.StageStrategy,
			wantReason: admission.ReasonURLRewriteOrProxy,
		},
		{
			name:       "ModeCallWithRaw",
			why:        "fe-v1 enrolls ModeCall exactly; call-with-raw is a different MODE on the same surface and is not proven",
			mutate:     func(o *feV1Opts) { o.withRaw = true },
			wantStage:  admission.StageMode,
			wantReason: admission.ReasonWithRawUnproven,
		},
		{
			name: "unsupported output schema",
			why: "an output schema the native schema/SAP build cannot bound declines at the PROMPT layer, " +
				"before the canonical body, Prepare, the plan oracle or any socket",
			mutate:     func(o *feV1Opts) { o.schema = feV1UnsupportedSchema() },
			wantStage:  admission.StagePrompt,
			wantReason: admission.ReasonOutputSchemaUnbounded,
		},
		{
			name:       "media part in the prompt",
			why:        "the native predicate does not claim a media prompt; an unproven message shape declines pre-Prepare",
			mutate:     func(o *feV1Opts) { o.mediaPart = true },
			wantStage:  admission.StageMessage,
			wantReason: admission.ReasonMediaPart,
		},
	}
}

// TestFeV1AdmissionMatrixDeclinesEverythingElse drives every non-fe-v1 shape
// through the deployed seam and requires a pre-claim decline with zero native
// socket activity and preserved BAML behaviour.
func TestFeV1AdmissionMatrixDeclinesEverythingElse(t *testing.T) {
	for _, arm := range feV1MatrixArms() {
		t.Run(arm.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			body := openAISuccess(`{"answer":"ok"}`)

			// The baseline is the arm that DOES claim; each arm changes exactly one
			// thing about it. That is what makes the decline attributable to the
			// change rather than to the fixture.
			native := feV1Opts{server: server, declare: true, flagOn: true, native: true,
				status: http.StatusOK, body: body}
			arm.mutate(&native)

			// The stock leg is the SAME request shape with the flag off and no
			// native callback, so the equivalence comparison is over the same
			// request rather than over the baseline one.
			stock := native
			stock.flagOn = false
			stock.native = false

			stockGot := runFeV1Call(t, stock)
			got := runFeV1Call(t, native)

			switch {
			case arm.neverReachesTheSeam:
				if got.observed.serveCalls != 0 {
					t.Fatalf("%s: the native callback ran %d time(s); this shape is routed by BAML before any child attempt exists (%s)",
						arm.name, got.observed.serveCalls, arm.why)
				}
			default:
				if got.observed.serveCalls != 1 {
					t.Fatalf("%s: the native callback ran %d time(s), want 1 — a decline that never reached admission proves nothing (%s)",
						arm.name, got.observed.serveCalls, arm.why)
				}
			}
			assertFeV1NoNativeActivity(t, arm.name+" — "+arm.why, stockGot, got)
			assertExternallyEquivalent(t, arm.name, stockGot.observed, got.observed)

			if arm.wantStage != "" {
				labels := map[string]string{"stage": string(arm.wantStage), "reason": string(arm.wantReason)}
				if v := got.counter(t, "baml_rest_debaml_declines_total", labels); v != 1 {
					t.Errorf("%s: declines{stage=%s,reason=%s} = %v, want 1 — the decline must be attributable to the gate that owns it",
						arm.name, arm.wantStage, arm.wantReason, v)
				}
			}
		})
	}
}

// assertFeV1NoNativeActivity is the shared "nothing native happened" assertion.
//
// The provider-request count is compared against the STOCK leg rather than pinned
// to 1: what a decline has to preserve is BAML's own behaviour, and a shape BAML
// routes differently (a retry policy, an ambiguous registry) legitimately makes a
// different number of requests — the property is that native changed none of it.
func assertFeV1NoNativeActivity(t *testing.T, label string, stock, got feV1Result) {
	t.Helper()
	if v := got.counter(t, "baml_rest_debaml_admission_phase_total", map[string]string{"phase": string(admission.PhaseClaimed)}); v != 0 {
		t.Errorf("%s: %v native claim(s); only the enrolled fe-v1 tuple may claim", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_winner_total", map[string]string{"winner": string(admission.WinnerNative)}); v != 0 {
		t.Errorf("%s: %v native winner(s)", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_native_sockets_total", nil); v != 0 {
		t.Errorf("%s: %v native socket(s) opened on a pre-claim decline", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_plan_compare_total", nil); v != 0 {
		t.Errorf("%s: %v plan comparison(s); a decline before the plan oracle must not reach it", label, v)
	}
	if v := got.counter(t, "baml_rest_debaml_response_compare_total", nil); v != 0 {
		t.Errorf("%s: %v response comparison(s); nothing was claimed, so there is no response to compare", label, v)
	}
	if got.observed.winnerEngine != "" {
		t.Errorf("%s: winner_engine = %q, want empty — BAML owns a declined request", label, got.observed.winnerEngine)
	}
	if got.providerRequestsThisLeg != stock.providerRequestsThisLeg {
		t.Errorf("%s: the provider saw %d request(s) under the native-capable worker vs %d under stock BAML; a decline must change nothing on the wire",
			label, got.providerRequestsThisLeg, stock.providerRequestsThisLeg)
	}
}

// --- the PRE-CLAIM plan oracle control ---------------------------------------

// TestFeV1PlanMutationsNeverSend is the pre-claim oracle control: a difference
// between BAML's no-send request plan and the native prepared plan must decline
// BEFORE a socket, on every field the strict comparison covers.
//
// The mutation is applied to BAML's plan at the serve seam, immediately before the
// comparison. That is the one place a per-field difference can be injected without
// changing what either planner would otherwise produce, and the comparison cannot
// tell which side a difference came from — which is exactly why mutating either
// side is a valid control over it.
func TestFeV1PlanMutationsNeverSend(t *testing.T) {
	for _, m := range []struct {
		name  string
		field admission.PlanCompareField
		apply func(*llmhttp.Request)
	}{
		{"method", admission.PlanCompareFieldMethod, func(r *llmhttp.Request) { r.Method = http.MethodPut }},
		{"target URL", admission.PlanCompareFieldTarget, func(r *llmhttp.Request) { r.URL += "?tampered=1" }},
		{"allowed header", admission.PlanCompareFieldHeaders, func(r *llmhttp.Request) {
			if r.Headers == nil {
				r.Headers = map[string]string{}
			}
			r.Headers["X-Tampered"] = "1"
		}},
		{"body", admission.PlanCompareFieldBody, func(r *llmhttp.Request) { r.Body += " " }},
	} {
		t.Run(m.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			body := openAISuccess(`{"answer":"ok"}`)
			stock := runFeV1Call(t, feV1Opts{server: server, declare: true, status: http.StatusOK, body: body})

			apply := m.apply
			got := runFeV1Call(t, feV1Opts{
				server: server, declare: true, flagOn: true, native: true,
				mutateBAMLPlan: apply,
				status:         http.StatusOK, body: body,
			})

			if got.observed.serveCalls != 1 {
				t.Fatalf("the native callback ran %d time(s), want 1", got.observed.serveCalls)
			}
			// NO SEND. This is the whole control: the plan differed, so the native
			// path must never have opened a socket, and BAML must have served.
			if v := got.counter(t, "baml_rest_debaml_native_sockets_total", nil); v != 0 {
				t.Errorf("%s mutation opened %v native socket(s); a plan difference declines PRE-socket", m.name, v)
			}
			if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseClaimed)})); v != 0 {
				t.Errorf("%s mutation produced %v claim(s); the comparison precedes the claim", m.name, v)
			}
			if got.providerRequestsThisLeg != 1 {
				t.Errorf("%s mutation: the provider saw %d request(s), want exactly 1 (BAML's)", m.name, got.providerRequestsThisLeg)
			}
			// The mismatch is recorded, on the FIELD that was mutated — so the
			// control proves the comparison examined that field rather than
			// declining for some incidental reason.
			labels := map[string]string{"result": string(admission.PlanCompareMismatch), "field": string(m.field)}
			if v := got.counter(t, "baml_rest_debaml_plan_compare_total", labels); v < 1 {
				t.Errorf("%s mutation: plan_compare{mismatch,%s} = %v, want at least 1", m.name, m.field, v)
			}
			if got.observed.winnerEngine != "" {
				t.Errorf("%s mutation: winner_engine = %q, want empty — BAML owns a pre-claim decline", m.name, got.observed.winnerEngine)
			}
			assertExternallyEquivalent(t, m.name+" plan mutation", stock.observed, got.observed)
		})
	}
}

// TestFeV1MissingBAMLPlanNeverSends is the other half of the pre-claim oracle
// control. TestFeV1PlanMutationsNeverSend proves a DIFFERENCE between the two
// plans declines; this proves the absence of BAML's plan does too.
//
// The strict OpenAI anchor's precondition is a plan it can COMPARE. If BAML's
// no-send plan errors, comes back nil, or the generated seam never supplied the
// closure, there is nothing to verify the native plan against — and the only safe
// answer is to decline PRE-SOCKET and let BAML serve, never to claim on an
// unverified plan. Each arm records the mismatch on the `meta` field, so an
// operator can tell "we could not compare" from "the plans differed".
func TestFeV1MissingBAMLPlanNeverSends(t *testing.T) {
	for _, m := range []struct {
		name    string
		failure feV1BAMLPlanFailure
		// wantMetaMismatch is false for the arm that declines EARLIER than the
		// plan oracle: a request with no BAML-plan closure can never be a valid
		// strict claim, so the identity resolver refuses it an identity at the
		// cohort gate and no plan comparison is ever recorded. That is a stronger
		// decline, not a weaker one, and the arm asserts the stronger shape.
		wantMetaMismatch bool
	}{
		{"BAML plan build error", feV1BAMLPlanErrors, true},
		{"BAML plan returned nil", feV1BAMLPlanNil, true},
		{"no BAML plan closure at all", feV1BAMLPlanAbsent, false},
	} {
		t.Run(m.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			body := openAISuccess(`{"answer":"ok"}`)
			stock := runFeV1Call(t, feV1Opts{server: server, declare: true, status: http.StatusOK, body: body})

			got := runFeV1Call(t, feV1Opts{
				server: server, declare: true, flagOn: true, native: true,
				bamlPlanFailure: m.failure,
				status:          http.StatusOK, body: body,
			})

			if got.observed.serveCalls != 1 {
				t.Fatalf("%s: the native callback ran %d time(s), want 1", m.name, got.observed.serveCalls)
			}
			// NO SEND: the request must reach BAML with no native socket behind it.
			if v := got.counter(t, "baml_rest_debaml_native_sockets_total", nil); v != 0 {
				t.Errorf("%s opened %v native socket(s); a missing BAML plan declines PRE-socket", m.name, v)
			}
			if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseClaimed)})); v != 0 {
				t.Errorf("%s produced %v claim(s); the comparison precedes the claim", m.name, v)
			}
			if got.providerRequestsThisLeg != 1 {
				t.Errorf("%s: the provider saw %d request(s), want exactly 1 (BAML's)", m.name, got.providerRequestsThisLeg)
			}
			// Recorded as a META mismatch — "could not compare", not "differed" —
			// for the arms that reach the oracle at all.
			labels := map[string]string{"result": string(admission.PlanCompareMismatch), "field": string(admission.PlanCompareFieldMeta)}
			switch {
			case m.wantMetaMismatch:
				if v := got.counter(t, "baml_rest_debaml_plan_compare_total", labels); v != 1 {
					t.Errorf("%s: plan_compare{mismatch,meta} = %v, want 1", m.name, v)
				}
				if v := got.counter(t, "baml_rest_debaml_plan_compare_total", map[string]string{"result": string(admission.PlanCompareMatch)}); v != 0 {
					t.Errorf("%s: plan_compare{match} = %v, want 0 — nothing was comparable", m.name, v)
				}
			default:
				if v := got.counter(t, "baml_rest_debaml_plan_compare_total", nil); v > 0 {
					t.Errorf("%s: %v plan comparison(s); a request with no BAML plan oracle is refused an identity before the compare", m.name, v)
				}
				if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhasePreclaimDecline)})); v != 0 {
					t.Errorf("%s: it declined as the ENROLLED cohort (%v); with no plan oracle it must not resolve an identity at all", m.name, v)
				}
			}
			if got.observed.winnerEngine != "" {
				t.Errorf("%s: winner_engine = %q, want empty — BAML owns a pre-claim decline", m.name, got.observed.winnerEngine)
			}
			assertExternallyEquivalent(t, m.name, stock.observed, got.observed)
		})
	}
}

// --- the POST-RESPONSE same-response oracle control --------------------------

// TestFeV1SameResponseDriftIsLabelledBAMLParseNotNative is the post-response
// oracle control: when the two parses of the SAME response bytes disagree — in
// value or in ORDER — the served result is the separately labelled BAML-parse
// outcome, never a native winner, and there is still exactly ONE provider request.
//
// It is what makes "zero parse-only winners" a meaningful acceptance criterion
// rather than a statement about an unreachable label: this demonstrates the
// counter CAN move, so a green acceptance run means the oracle AGREED.
func TestFeV1SameResponseDriftIsLabelledBAMLParseNotNative(t *testing.T) {
	for _, m := range []struct {
		name    string
		field   admission.ResponseCompareField
		fixture string
		content string
		mutate  func(*testing.T, []byte) []byte
	}{
		{
			name:    "structured value drift",
			field:   admission.ResponseCompareFieldStructured,
			fixture: "single_user_message",
			content: `{"answer":"ok"}`,
			mutate:  func(_ *testing.T, _ []byte) []byte { return []byte(`{"answer":"DIFFERENT"}`) },
		},
		{
			// ORDER-ONLY drift. The oracle normalizes both parses into SCHEMA order
			// before comparing bytes, so reordering a schema-declared field proves
			// nothing — it is normalized away, correctly, and stays a native win.
			// What is NOT normalizable is the key order inside a MAP-typed value:
			// a map's keys are data, not schema, so the two parses' wire order is
			// compared as-is. Reversing them leaves the structured value identical
			// and makes the ORDER facet, and only the ORDER facet, disagree.
			name:    "structured ORDER drift",
			field:   admission.ResponseCompareFieldOrder,
			fixture: "",
			content: `{"answer":"ok","tags":{"alpha":"1","beta":"2"}}`,
			mutate:  feV1ReverseMapOrder,
		},
	} {
		t.Run(m.name, func(t *testing.T) {
			server := newLiveCaptureServer(t)
			mutate := m.mutate
			fixture := feV1MapOrderFixture()
			if m.fixture != "" {
				fixture = dynFixtureByName(t, m.fixture)
			}
			got := runFeV1Call(t, feV1Opts{
				server: server, fixture: fixture,
				declare: true, flagOn: true, native: true,
				mutateBAMLParse: func(in []byte) []byte { return mutate(t, in) },
				status:          http.StatusOK, body: openAISuccess(m.content),
			})

			// ONE provider request. A disagreement must NEVER be repaired by asking
			// the provider again — BAML may only parse the bytes native already got.
			if got.providerRequestsThisLeg != 1 {
				t.Fatalf("%s: the provider saw %d request(s), want exactly 1 — a same-response drift must never cause a resend",
					m.name, got.providerRequestsThisLeg)
			}
			// The request WAS claimed and a socket WAS opened: this is a post-claim
			// outcome, not a pre-claim decline.
			if v := got.counter(t, "baml_rest_debaml_admission_phase_total", feV1Labels(map[string]string{"phase": string(admission.PhaseClaimed)})); v != 1 {
				t.Errorf("%s: claimed = %v, want 1", m.name, v)
			}
			if v := got.counter(t, "baml_rest_debaml_native_sockets_total", map[string]string{"flag": "on"}); v != 1 {
				t.Errorf("%s: native_sockets = %v, want exactly 1", m.name, v)
			}
			// NOT a native winner.
			if got.observed.winnerEngine != bamlutils.NativeServeEngineBAMLParse {
				t.Errorf("%s: winner_engine = %q, want %q — a same-response drift may never read as a native win",
					m.name, got.observed.winnerEngine, bamlutils.NativeServeEngineBAMLParse)
			}
			if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerNative)})); v != 0 {
				t.Errorf("%s: winner{native} = %v, want 0", m.name, v)
			}
			if v := got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerBAMLParseSameResponse)})); v != 1 {
				t.Errorf("%s: winner{baml_parse_same_response} = %v, want 1 — the outcome must be separately observable", m.name, v)
			}
			if v := got.counter(t, "baml_rest_debaml_fallback_total", map[string]string{"kind": "parse_only"}); v != 1 {
				t.Errorf("%s: fallback{kind=parse_only} = %v, want 1", m.name, v)
			}
			// And the drift is recorded on the FACET that drifted.
			labels := map[string]string{"result": string(admission.ResponseCompareMismatch), "field": string(m.field)}
			if v := got.counter(t, "baml_rest_debaml_response_compare_total", labels); v != 1 {
				t.Errorf("%s: response_compare{mismatch,%s} = %v, want 1", m.name, m.field, v)
			}
			// The served value is BAML's parse of those same bytes — the safe answer.
			var served map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got.observed.data), &served); err != nil {
				t.Fatalf("%s: served data is not a JSON object: %v", m.name, err)
			}
			if len(served) == 0 {
				t.Errorf("%s: served nothing on a drift; BAML's parse of the same bytes is the safe result", m.name)
			}
		})
	}
}

// TestFeV1AcceptanceSuiteHasZeroParseOnlyWinners is the aggregate the cutover
// gates promotion on, asserted over every fixture: a successful fe-v1 acceptance
// run has zero parse-only winners and at least one native winner.
func TestFeV1AcceptanceSuiteHasZeroParseOnlyWinners(t *testing.T) {
	var parseOnly, native float64
	for _, fx := range liveFixtures(t) {
		server := newLiveCaptureServer(t)
		got := runFeV1Call(t, feV1Opts{
			server: server, fixture: fx.dynFixture, declare: true, flagOn: true, native: true,
			status: http.StatusOK, body: openAISuccess(fx.content),
		})
		if got.observed.errText != "" {
			t.Fatalf("%s: the enrolled fixture errored: %s", fx.name, got.observed.errText)
		}
		parseOnly += got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerBAMLParseSameResponse)}))
		native += got.counter(t, "baml_rest_debaml_winner_total", feV1Labels(map[string]string{"winner": string(admission.WinnerNative)}))
	}
	if native == 0 {
		t.Fatal("no fixture produced a native winner; the zero-parse-only assertion below would be vacuous")
	}
	if parseOnly != 0 {
		t.Errorf("the fe-v1 acceptance suite produced %v parse-only winner(s), want 0", parseOnly)
	}
	if strings.TrimSpace(admission.ProductionCohortPolicyVersion) == "" {
		t.Error("the shipped policy version is empty; an operator cannot name what this worker enrolls")
	}
}

// feV1MapOrderFixture is the ORDER-facet fixture: a MAP-typed output field whose
// key order is data rather than schema, and therefore the one thing the
// same-response oracle compares without normalizing first.
func feV1MapOrderFixture() dynFixture {
	return dynFixture{
		name:     "fev1_map_order",
		messages: []nativeprompt.Message{{Role: "user", Content: sp("Return the tags.")}},
		schema: &bamlutils.DynamicOutputSchema{
			Properties: bamlutils.MustOrderedMap(
				bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
				bamlutils.OrderedKV("tags", &bamlutils.DynamicProperty{
					Type:   "map",
					Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
					Values: &bamlutils.DynamicTypeSpec{Type: "string"},
				}),
			),
		},
	}
}

// feV1ReverseMapOrder reverses the key order INSIDE the `tags` map, leaving every
// value — and the document's semantic content — untouched.
func feV1ReverseMapOrder(t *testing.T, in []byte) []byte {
	t.Helper()
	var root bamlutils.OrderedMap[json.RawMessage]
	if err := json.Unmarshal(in, &root); err != nil {
		t.Fatalf("order control: %s is not a JSON object: %v", liveBodyDigest(in), err)
	}
	raw, ok := root.Get("tags")
	if !ok {
		t.Fatalf("order control: BAML's parse carries no `tags` map: %s", liveBodyDigest(in))
	}
	var tags bamlutils.OrderedMap[json.RawMessage]
	if err := json.Unmarshal(raw, &tags); err != nil {
		t.Fatalf("order control: `tags` is not a JSON object: %v", err)
	}
	keys := tags.Keys()
	if len(keys) < 2 {
		t.Fatalf("order control needs at least two map keys, got %d", len(keys))
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := len(keys) - 1; i >= 0; i-- {
		if i != len(keys)-1 {
			b.WriteByte(',')
		}
		v, _ := tags.Get(keys[i])
		b.WriteString(fmt.Sprintf("%q:%s", keys[i], string(v)))
	}
	b.WriteByte('}')

	if err := root.Replace("tags", json.RawMessage(b.String())); err != nil {
		t.Fatalf("order control: %v", err)
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("order control: %v", err)
	}
	return out
}
