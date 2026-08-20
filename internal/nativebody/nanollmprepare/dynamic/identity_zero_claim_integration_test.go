//go:build integration && nanollm_integration

package dynamic

// De-BAML serving cutover S3a — the GENERATED-SEAM zero-claim and PROVENANCE proof.
//
// S3a wires a real effective-configuration identity resolver into the native
// admission seam, sourced from the deployment's TRUSTED-CONFIGURATION SEAL. Two
// questions follow, and neither can be answered by a unit test:
//
//  1. With the resolver LIVE and actually resolving an identity for the request in
//     front of it, does the worker still claim nothing?
//  2. Does a request that DESCRIBES the approved configuration itself — same client
//     name, same provider, same model, same base_url, same credential — get an
//     identity? It must not. The `client_registry` is the caller's document, and a
//     configuration the caller wrote is not the deployment's approved one however
//     closely it matches.
//
// So this drives the REAL generated dynamic `/call` seam (dynclient + patched BAML +
// the serve implementation the PRODUCTION factory builds — nativeserve.New, no test
// seam, no injected gate, no proof identity) through the REAL worker config-load
// sealing pass, and reads the worker's own de-BAML collectors.
//
// The BOOTED-ARTIFACT HTTP proof lives in cmd/serve/native_artifact_route_proof_test.go
// (build tags `subprocess && nativeartifactproof`): it boots the native-capable
// serve-profile worker as a subprocess, drives it through the real pool, and POSTs the
// public `/call` body over a real HTTP listener. This file is the seam-level
// companion — same production factory, no artifact build required, and able to mutate
// the request in ways a booted binary cannot.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve"
)

// zeroClaimFingerprint is the opaque slot the deployment assigns to the loopback
// oracle configuration. It is a declared-but-unassigned production slot, declared
// through the deployment's own configuration exactly as an operator would — never in
// shipped source.
const zeroClaimFingerprint = "cfg001"

// zeroClaimDeclaration is the deployment's approved-configuration declaration for the
// loopback oracle: the SAME effective configuration liveOracleRegistry describes,
// owned by the deployment instead of by the request.
func zeroClaimDeclaration(t *testing.T, base string) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"TestClient",
		"fingerprint":"` + zeroClaimFingerprint + `",
		"provider":"openai",
		"options":{"model":"` + fenceModel + `","base_url":"` + base + `/v1","api_key":"` + fenceAPIKey + `"}
	}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	return set
}

// namedOnlyOracleRegistry is what a request that merely NAMES the approved class
// looks like: a primary and a client name, and nothing else. The worker's sealing
// pass installs the deployment's provider and options onto it.
func namedOnlyOracleRegistry() *dynclient.ClientRegistry {
	return &dynclient.ClientRegistry{
		Primary: sp("TestClient"),
		Clients: []*dynclient.ClientProperty{{Name: "TestClient"}},
	}
}

// deployedIdentityCall is everything this proof reads about one request through the
// generated seam: what the caller saw, what the provider saw, and what the worker's
// own de-BAML collectors recorded.
type deployedIdentityCall struct {
	observed        observedCall
	claims          float64
	nativeWinners   float64
	nativeSockets   float64
	declineNone     float64
	declineResolved float64
	bamlTransport   float64
}

// identityCallOpts is one arm of the proof.
type identityCallOpts struct {
	// declare installs the deployment's approved-configuration declaration.
	declare bool
	// callerSupplies sends the full configuration in the request instead of naming
	// the class — the shape that must never obtain an identity.
	callerSupplies bool
	// flagOn is the one global umbrella switch.
	flagOn bool
	status int
	body   []byte
}

// runDeployedIdentityCall issues ONE DynamicCall through the generated seam against
// the serve implementation a native-capable worker actually installs, with the
// deployment's declaration threaded through the REAL worker config-load sealing pass
// (dynclient.WithTrustedClients installs it on the same worker.Config the subprocess
// worker builds from BAML_REST_DEBAML_TRUSTED_CLIENTS).
func runDeployedIdentityCall(t *testing.T, opts identityCallOpts) deployedIdentityCall {
	t.Helper()
	fx := dynFixtureByName(t, "single_user_message")
	server := newLiveCaptureServer(t)
	server.setResponse(opts.status, opts.body)

	// The PRODUCTION factory. Not canary.NewServerWithCohortIdentity, not an injected
	// gate, not a proof fingerprint — the constructor workerboot can actually reach.
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
		dynclient.WithNativeServeComparator(func(ctx context.Context, req bamlutils.NativeServeRequest) bamlutils.NativeServeResult {
			serveCalls.Add(1)
			return serveFn(ctx, req)
		}),
	}
	if opts.declare {
		dynOpts = append(dynOpts, dynclient.WithTrustedClients(zeroClaimDeclaration(t, server.base())))
	}
	client, err := dynclient.New(dynOpts...)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	registry := namedOnlyOracleRegistry()
	if opts.callerSupplies {
		registry = liveOracleRegistry(server.base())
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	res, callErr := client.DynamicCall(ctx, dynclient.Request{
		Messages:            toDynMessages(fx.messages),
		ClientRegistry:      registry,
		OutputSchema:        fx.schema,
		PreserveSchemaOrder: bptr(true),
	})

	out := deployedIdentityCall{observed: observedCall{
		providerRequests: int64(server.count()),
		serveCalls:       serveCalls.Load(),
	}}
	if callErr != nil {
		out.observed.errText = callErr.Error()
	}
	if res != nil {
		out.observed.data = string(res.Data)
		out.observed.winnerEngine, out.observed.plannedEngine = lastOutcomeEngine(res)
	}
	out.claims = deployedCounter(t, reg, "baml_rest_debaml_admission_phase_total",
		map[string]string{"surface": "dynamic_call", "phase": "claimed"})
	out.nativeWinners = deployedCounter(t, reg, "baml_rest_debaml_winner_total",
		map[string]string{"surface": "dynamic_call", "winner": "native"})
	out.nativeSockets = deployedCounter(t, reg, "baml_rest_debaml_native_sockets_total", nil)
	out.declineNone = deployedCounter(t, reg, "baml_rest_debaml_admission_phase_total",
		map[string]string{"surface": "dynamic_call", "cohort": "none", "phase": "preclaim_decline"})
	out.declineResolved = deployedCounter(t, reg, "baml_rest_debaml_admission_phase_total",
		map[string]string{"surface": "dynamic_call", "cohort": "unrecognized", "phase": "preclaim_decline"})
	out.bamlTransport = deployedCounter(t, reg, "baml_rest_debaml_winner_total",
		map[string]string{"surface": "dynamic_call", "winner": "baml_transport"})
	return out
}

// deployedCounter sums a counter family's series whose labels all match want (an
// empty want matches the whole family). It returns 0 for an absent family, which is
// the right reading here: every assertion below is "this must be zero" or "one".
func deployedCounter(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
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
			got := make(map[string]string, len(mm.GetLabel()))
			for _, lp := range mm.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				sum += mm.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// assertZeroNative is the S3a serving guarantee, stated once and reused by every arm.
func assertZeroNative(t *testing.T, label string, got deployedIdentityCall) {
	t.Helper()
	if got.claims != 0 {
		t.Errorf("%s: the worker made %v native claim(s); the empty policy must permit zero", label, got.claims)
	}
	if got.nativeWinners != 0 {
		t.Errorf("%s: %v request(s) had winner_engine=native", label, got.nativeWinners)
	}
	if got.nativeSockets != 0 {
		t.Errorf("%s: %v native socket(s) were opened", label, got.nativeSockets)
	}
	if got.observed.winnerEngine != "" {
		t.Errorf("%s: outcome winner_engine = %q, want BAML's empty marker", label, got.observed.winnerEngine)
	}
}

// TestGeneratedSeamMakesZeroNativeClaimsWithIdentityLive is the zero-claim proof with
// the resolver actually resolving.
func TestGeneratedSeamMakesZeroNativeClaimsWithIdentityLive(t *testing.T) {
	for _, c := range equivalenceCases() {
		t.Run(c.name, func(t *testing.T) {
			baml := runEquivalenceCall(t, nil, false, c.status, c.body)
			got := runDeployedIdentityCall(t, identityCallOpts{declare: true, flagOn: true, status: c.status, body: c.body})

			assertZeroNative(t, "flag on, deployment-sealed identity", got)

			// The BITING half: the identity RESOLVED. A sealed-but-uninventoried
			// fingerprint folds onto the bounded `unrecognized` bucket, so this
			// distinguishes "the resolver ran and identified the configuration" from
			// "the seam presented nothing", which is what `none` means and what a
			// deleted or unwired resolver would record.
			if got.declineResolved != 1 {
				t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 1 — the resolver did not identify the sealed configuration through the generated seam", got.declineResolved)
			}
			if got.declineNone != 0 {
				t.Errorf("preclaim_decline{cohort=none} = %v, want 0 — the seam presented no identity for a configuration the deployment sealed", got.declineNone)
			}
			if got.bamlTransport != 1 {
				t.Errorf("winner{baml_transport} = %v, want 1 — BAML must own every request", got.bamlTransport)
			}
			if got.observed.serveCalls != 1 {
				t.Errorf("the generated seam invoked the native callback %d time(s), want 1", got.observed.serveCalls)
			}
			assertExternallyEquivalent(t, "flag on, deployment-sealed identity", baml, got.observed)
		})
	}
}

// TestCallerSuppliedConfigurationGetsNoIdentityThroughTheSeam is the P1 proof on the
// served path: the SAME deployment, the SAME approved class, the SAME effective
// configuration — but the request describes it instead of naming it, and therefore
// carries no identity at all.
//
// This is the arm that must stay green before S3b may enroll anything: with an
// enrollment present, an identity here would be an out-claim by a request the
// deployment never approved.
func TestCallerSuppliedConfigurationGetsNoIdentityThroughTheSeam(t *testing.T) {
	body := openAISuccess(`{"answer":"ok"}`)
	sealed := runDeployedIdentityCall(t, identityCallOpts{declare: true, flagOn: true, status: http.StatusOK, body: body})
	if sealed.declineResolved != 1 {
		t.Fatalf("the deployment-sealed control resolved no identity (unrecognized=%v); the comparison below would be vacuous", sealed.declineResolved)
	}

	supplied := runDeployedIdentityCall(t, identityCallOpts{
		declare: true, callerSupplies: true, flagOn: true, status: http.StatusOK, body: body,
	})
	assertZeroNative(t, "flag on, caller-supplied configuration", supplied)
	if supplied.declineResolved != 0 {
		t.Errorf("a caller-supplied configuration resolved an identity (unrecognized=%v); the client_registry is the CALLER's document and can never be an identity", supplied.declineResolved)
	}
	if supplied.declineNone != 1 {
		t.Errorf("preclaim_decline{cohort=none} = %v, want 1 — a caller-supplied configuration must present NO identity", supplied.declineNone)
	}
}

// TestUndeclaredDeploymentPresentsNoIdentityThroughTheSeam is the control for the
// biting half above: the SAME route, the SAME request, with nothing declared, must
// record the decline under `none`. It proves the `unrecognized` reading is caused by
// the declaration rather than by the label defaulting that way.
func TestUndeclaredDeploymentPresentsNoIdentityThroughTheSeam(t *testing.T) {
	// A COMPLETE request (the caller carries the whole configuration, because with
	// nothing declared there is nothing to seal onto a bare name) so the request
	// actually reaches the native seam and records a decline to read.
	got := runDeployedIdentityCall(t, identityCallOpts{
		callerSupplies: true, flagOn: true, status: http.StatusOK, body: openAISuccess(`{"answer":"ok"}`),
	})
	assertZeroNative(t, "flag on, nothing declared", got)
	if got.declineNone != 1 {
		t.Errorf("preclaim_decline{cohort=none} = %v, want 1 — an undeclared deployment must present no identity", got.declineNone)
	}
	if got.declineResolved != 0 {
		t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 0 — nothing was declared, so nothing may resolve", got.declineResolved)
	}
}

// TestGeneratedSeamWithTheFlagOffRunsNoNativeWorkAtAll is the flag-off proof, with the
// resolver PRESENT and the configuration SEALED: the one global kill switch must still
// mean the native callback is never invoked, no native work runs, and the caller sees
// ordinary BAML.
func TestGeneratedSeamWithTheFlagOffRunsNoNativeWorkAtAll(t *testing.T) {
	for _, c := range equivalenceCases() {
		t.Run(c.name, func(t *testing.T) {
			baml := runEquivalenceCall(t, nil, false, c.status, c.body)
			got := runDeployedIdentityCall(t, identityCallOpts{declare: true, status: c.status, body: c.body})

			assertZeroNative(t, "flag off, deployment-sealed identity", got)
			if got.observed.serveCalls != 0 {
				t.Errorf("the flag-off seam invoked the native callback %d time(s); BAML_REST_USE_DEBAML=false must install no callback", got.observed.serveCalls)
			}
			if got.declineNone != 0 || got.declineResolved != 0 {
				t.Errorf("the flag-off worker recorded de-BAML declines (none=%v, resolved=%v); it must run no admission at all",
					got.declineNone, got.declineResolved)
			}
			if got.observed.plannedEngine != "" {
				t.Errorf("the flag-off outcome carries planned_engine=%q; the native lane must not be advertised", got.observed.plannedEngine)
			}
			assertExternallyEquivalent(t, "flag off, deployment-sealed identity", baml, got.observed)
		})
	}
}

// TestSealingIsInertForAnUndeclaredDeployment pins the behaviour half of the shipped
// default: a deployment that declared nothing must serve byte-identically to one that
// has no sealing pass at all. The comparison is against the ordinary BAML baseline,
// which is what "inert" has to mean.
func TestSealingIsInertForAnUndeclaredDeployment(t *testing.T) {
	for _, c := range equivalenceCases() {
		t.Run(c.name, func(t *testing.T) {
			baml := runEquivalenceCall(t, nil, false, c.status, c.body)
			got := runDeployedIdentityCall(t, identityCallOpts{callerSupplies: true, flagOn: true, status: c.status, body: c.body})
			assertZeroNative(t, "flag on, nothing declared", got)
			assertExternallyEquivalent(t, "flag on, nothing declared", baml, got.observed)
		})
	}
}
