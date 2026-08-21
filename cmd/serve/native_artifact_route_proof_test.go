//go:build subprocess && nativeartifactproof

package main

// De-BAML serving cutover S3a — the BOOTED-ARTIFACT PUBLIC `/call` ZERO-CLAIM PROOF.
//
// This is the join every other S3a proof stops short of. It boots the native-capable
// serve-profile worker as a real subprocess, drives it through the REAL pool, and
// POSTs the public `/call/Baml_Rest_Dynamic` body over a REAL HTTP listener into the
// production chi handler — then reads the BOOTED WORKER'S OWN de-BAML collectors, over
// the plugin boundary, to assert the cutover's central claim:
//
//	flag ON + the shipped EMPTY cohort policy + a live trusted declaration
//	  => the configuration identity RESOLVES inside the deployed worker (cohort=unrecognized)
//	  => and it still makes ZERO native claims, ZERO native winners, ZERO native sockets
//	  => BAML serves the request, with exactly ONE provider send.
//
// with the two controls that make those readings causal: nothing declared reports
// `cohort=none`, and flag-off installs no de-BAML collector at all.
//
// # The binary under proof, and the one thing the fixture tag changes
//
// baml-rest's root `adapter.go` is the "overwritten during build" stub: `Methods` is
// empty until the CONTAINER build generates a client from the deployment's own BAML
// project. An artifact built from a checkout therefore knows no methods and cannot be
// sent a request at all — which is why the S2 artifact proof boots the binaries and
// asserts only their startup state.
//
// So the binary here is the SHIPPED serve-profile worker
// (internal/nativebody/nanollmprepare/cmd/worker), built by
// scripts/build-s3a-fixture-artifact.sh with the shipped tag set, the shipped
// -ldflags attestation stamp and the shipped GOWORK=off + CGO isolated-module build,
// PLUS `debamlworkerfixture` — which links dynclient's COMMITTED generated dynamic
// client so `Baml_Rest_Dynamic` exists. One tag changes exactly one thing: which BAML
// methods are present. The serve-profile options, the native serve factories, the
// flag-first branch, and above all the admission predicate are the shipped ones.
//
// The proof asserts that directly rather than trusting the sentence above: it checks
// the booted binary reports the STANDARD artifact profile and the artifact ID computed
// for the SHIPPED tag set, so a fixture that had drifted into a differently-built
// artifact fails here instead of quietly proving something about a lookalike.
//
// # Why it cannot skip
//
// An earlier native-artifact proof skipped when its binary env was unset, the CI step
// that was supposed to supply it did not, and a real flag-off kill-switch failure
// shipped underneath a green skip. Missing env is a FAILURE here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/internal/artifactprofile"
	"github.com/invakid404/baml-rest/pool"
)

// Where the gated nanollm lane hands the fixture artifact over.
const (
	fixtureWorkerBinEnv        = "BAML_REST_S3A_FIXTURE_WORKER_BIN"
	fixtureWorkerArtifactIDEnv = "BAML_REST_S3A_FIXTURE_WORKER_ARTIFACT_ID"
)

// routeProofFingerprint is the opaque slot the deployment assigns to its approved
// configuration class here. It is a declared-but-unassigned production slot; the
// assignment lives in the deployment's own configuration, never in shipped source.
const routeProofFingerprint = "cfg001"

const routeProofClient = "RouteProofClient"

// routeProofProvider is a loopback OpenAI-shaped provider. Its counter is what "BAML
// sent exactly one request" is read from.
type routeProofProvider struct {
	srv   *httptest.Server
	calls atomic.Int64
}

func newRouteProofProvider(t *testing.T) *routeProofProvider {
	t.Helper()
	p := &routeProofProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"route-proof","object":"chat.completion",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// routeProofDeclaration is the deployment's approved-configuration declaration,
// pointed at the loopback provider. A request may NAME this class; it may not define
// it.
func routeProofDeclaration(base string) string {
	return fmt.Sprintf(
		`{"trusted_clients":[{"name":%q,"fingerprint":%q,"provider":"openai",`+
			`"options":{"model":"gpt-4o-mini","base_url":%q,"api_key":"sk-route-proof"}}]}`,
		routeProofClient, routeProofFingerprint, base+"/v1")
}

// routeProofBody is the PUBLIC `/call/Baml_Rest_Dynamic` request body. namedOnly picks
// the shape: a request that merely NAMES the approved class (which the deployment
// seals), or one that DEFINES the configuration itself with the very same values
// (which it must not).
func routeProofBody(t *testing.T, base string, namedOnly bool) []byte {
	t.Helper()
	primary := routeProofClient
	text := "hello"
	client := &bamlutils.ClientProperty{Name: routeProofClient}
	if !namedOnly {
		client = &bamlutils.ClientProperty{
			Name:     routeProofClient,
			Provider: "openai",
			Options: map[string]any{
				"model":    "gpt-4o-mini",
				"base_url": base + "/v1",
				"api_key":  "sk-route-proof",
			},
		}
	}
	body, err := json.Marshal(bamlutils.DynamicInput{
		Messages:       []bamlutils.DynamicMessage{{Role: "user", TextContent: &text}},
		ClientRegistry: &bamlutils.ClientRegistry{Primary: &primary, Clients: []*bamlutils.ClientProperty{client}},
		OutputSchema: &bamlutils.DynamicOutputSchema{
			Properties: bamlutils.MustOrderedMap(
				bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			),
		},
	})
	if err != nil {
		t.Fatalf("marshal the public /call body: %v", err)
	}
	return body
}

// routeProofResult is everything this proof reads about one request through the
// deployed route.
type routeProofResult struct {
	status           int
	body             string
	providerRequests int64

	// The BOOTED WORKER's own collectors, gathered over the plugin boundary — not a
	// registry this process constructed.
	claims          float64
	nativeWinners   float64
	nativeSockets   float64
	declineNone     float64
	declineResolved float64
	bamlTransport   float64
	// deBAMLFamilies are the names of every de-BAML metric family the worker
	// exposes at all, which is what the flag-off arm reads.
	deBAMLFamilies []string
	// artifactProfile / artifactID are the booted binary's OWN identity, read off
	// the S2 artifact-info gauge it publishes unconditionally.
	artifactProfile string
	artifactID      string
}

// routeProofOpts is one arm of the proof.
type routeProofOpts struct {
	// declare installs the deployment's approved-configuration declaration in the
	// booted worker's environment.
	declare bool
	// callerDefines sends the whole configuration in the request body instead of
	// naming the class.
	callerDefines bool
	// flagOn is the one global umbrella switch, as the booted worker sees it.
	flagOn bool
}

// fixtureBinary returns the fixture artifact, failing when the lane did not supply it.
func fixtureBinary(t *testing.T) string {
	t.Helper()
	bin, ok := os.LookupEnv(fixtureWorkerBinEnv)
	if !ok || strings.TrimSpace(bin) == "" {
		t.Fatalf("%s is not set: this lane must BOOT the native-capable artifact and send it a request; a missing artifact is a lane misconfiguration, not a reason to report success", fixtureWorkerBinEnv)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s=%q is not usable: %v", fixtureWorkerBinEnv, bin, err)
	}
	return bin
}

// runRouteProof boots the artifact and drives ONE public `/call` request through it.
func runRouteProof(t *testing.T, opts routeProofOpts) routeProofResult {
	t.Helper()
	bin := fixtureBinary(t)
	provider := newRouteProofProvider(t)

	// The worker subprocess inherits this process's environment (pool builds its
	// exec.Command from os.Environ()), so t.Setenv is how the DEPLOYMENT's
	// configuration reaches the booted artifact — exactly the channel a real
	// deployment uses.
	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", opts.flagOn))
	if opts.declare {
		t.Setenv(trustedclients.EnvVar, routeProofDeclaration(provider.srv.URL))
	} else {
		t.Setenv(trustedclients.EnvVar, "")
	}

	workerPool, err := pool.New(&pool.Config{
		WorkerPath:         bin,
		PoolSize:           1,
		LogOutput:          io.Discard,
		WorkerStartTimeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New over the native-capable artifact: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = workerPool.Shutdown(ctx)
	})

	// THE PUBLIC ROUTE, over a REAL HTTP listener. makeChiDynamicCallHandler is the
	// handler newUnaryRouter installs for `/call/Baml_Rest_Dynamic`, so the request
	// below goes through the same body read, the same decode, the same
	// DynamicInput.Validate, the same ToWorkerInput and the same pool dispatch a
	// production request does.
	srv := httptest.NewServer(makeChiDynamicCallHandler(workerPool, bamlutils.StreamModeCall, false))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/call/"+bamlutils.DynamicEndpointName, "application/json",
		strings.NewReader(string(routeProofBody(t, provider.srv.URL, !opts.callerDefines))))
	if err != nil {
		t.Fatalf("POST the public /call route: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the /call response: %v", err)
	}

	out := routeProofResult{
		status:           resp.StatusCode,
		body:             string(raw),
		providerRequests: provider.calls.Load(),
	}
	readArtifactDeBAMLMetrics(t, workerPool, &out)
	return out
}

// readArtifactDeBAMLMetrics gathers the BOOTED WORKER's own Prometheus families over
// the plugin boundary and reads the de-BAML series out of them.
func readArtifactDeBAMLMetrics(t *testing.T, p *pool.Pool, out *routeProofResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gathered := p.GatherWorkerMetrics(ctx)
	if len(gathered) == 0 {
		t.Fatal("no worker reported metrics; the artifact's own counters are what this proof reads")
	}
	for _, wm := range gathered {
		if wm.Err != nil {
			t.Fatalf("worker %d metrics: %v", wm.WorkerID, wm.Err)
		}
		for _, rawFamily := range wm.MetricFamilies {
			var mf dto.MetricFamily
			if err := proto.Unmarshal(rawFamily, &mf); err != nil {
				t.Fatalf("decode worker MetricFamily: %v", err)
			}
			name := mf.GetName()
			if !strings.HasPrefix(name, "baml_rest_debaml_") {
				continue
			}
			out.deBAMLFamilies = append(out.deBAMLFamilies, name)
			for _, m := range mf.GetMetric() {
				labels := map[string]string{}
				for _, lp := range m.GetLabel() {
					labels[lp.GetName()] = lp.GetValue()
				}
				v := m.GetCounter().GetValue()
				if name == artifactprofile.ArtifactInfoMetric {
					out.artifactProfile = labels["profile"]
					out.artifactID = labels["artifact_id"]
					continue
				}
				switch {
				case name == "baml_rest_debaml_native_sockets_total":
					out.nativeSockets += v
				case name == "baml_rest_debaml_admission_phase_total" && labels["surface"] == "dynamic_call":
					switch {
					case labels["phase"] == "claimed":
						out.claims += v
					case labels["phase"] == "preclaim_decline" && labels["cohort"] == "none":
						out.declineNone += v
					case labels["phase"] == "preclaim_decline" && labels["cohort"] == "unrecognized":
						out.declineResolved += v
					}
				case name == "baml_rest_debaml_winner_total" && labels["surface"] == "dynamic_call":
					switch labels["winner"] {
					case "native":
						out.nativeWinners += v
					case "baml_transport":
						out.bamlTransport += v
					}
				}
			}
		}
	}
}

// assertServedByBAML pins the caller-visible half: the public route answered 200 with
// the model's output, and the provider saw exactly ONE request — BAML's.
func assertServedByBAML(t *testing.T, label string, got routeProofResult) {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("%s: the public /call route returned %d: %s", label, got.status, got.body)
	}
	if !strings.Contains(got.body, `"ok"`) {
		t.Errorf("%s: the served body does not carry the model output: %s", label, got.body)
	}
	if got.providerRequests != 1 {
		t.Errorf("%s: the provider saw %d request(s), want exactly 1 (BAML's)", label, got.providerRequests)
	}
}

// assertZeroNativeOnTheArtifact is the S3a serving guarantee, read off the booted
// artifact's own counters.
func assertZeroNativeOnTheArtifact(t *testing.T, label string, got routeProofResult) {
	t.Helper()
	if got.claims != 0 {
		t.Errorf("%s: the artifact made %v native claim(s); the empty policy must permit zero", label, got.claims)
	}
	if got.nativeWinners != 0 {
		t.Errorf("%s: %v request(s) had winner_engine=native", label, got.nativeWinners)
	}
	if got.nativeSockets != 0 {
		t.Errorf("%s: %v native socket(s) were opened", label, got.nativeSockets)
	}
}

// TestFixtureArtifactIsTheShippedNativeCapableArtifact is the guard that makes every
// assertion below an assertion ABOUT THE STANDARD ARTIFACT. The fixture differs from
// the shipped serve-profile worker by one build tag that adds a method table; if it
// ever drifted into a differently-built binary — different profile, different tag set,
// different attestation — the stamped identity would stop matching and this fails
// before anything else is claimed on its behalf.
func TestFixtureArtifactIsTheShippedNativeCapableArtifact(t *testing.T) {
	wantID := strings.TrimSpace(os.Getenv(fixtureWorkerArtifactIDEnv))
	if wantID == "" {
		t.Fatalf("%s is not set: without the artifact ID this proof cannot show which artifact it booted", fixtureWorkerArtifactIDEnv)
	}
	if err := artifactprofile.ValidateArtifactID(wantID); err != nil {
		t.Fatalf("%s=%q is not a release artifact ID: %v", fixtureWorkerArtifactIDEnv, wantID, err)
	}

	got := runRouteProof(t, routeProofOpts{declare: true, flagOn: true})
	assertServedByBAML(t, "artifact identity", got)

	// The identity the BOOTED BINARY publishes about itself, read off the artifact-info
	// gauge it registers unconditionally — not inferred from the build script, and not
	// from a log line this process happens to have captured.
	if got.artifactProfile != string(artifactprofile.ProfileNativeCapable) {
		t.Errorf("the booted fixture publishes profile=%q, want %q — this proof must be about the STANDARD artifact",
			got.artifactProfile, artifactprofile.ProfileNativeCapable)
	}
	if got.artifactID != wantID {
		t.Errorf("the booted fixture publishes artifact_id=%q, want the SHIPPED serve-profile artifact's %q; the fixture has drifted into a differently-built binary",
			got.artifactID, wantID)
	}
}

// TestBootedArtifactServesTheDeployedCallRouteWithZeroNativeClaims is the headline
// joined proof.
func TestBootedArtifactServesTheDeployedCallRouteWithZeroNativeClaims(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, flagOn: true})

	assertServedByBAML(t, "flag on, deployment-sealed identity", got)
	assertZeroNativeOnTheArtifact(t, "flag on, deployment-sealed identity", got)

	// THE BITING HALF. The identity RESOLVED inside the booted artifact: the sealed
	// fingerprint is not inventoried, so the gate folds it onto the bounded
	// `unrecognized` bucket. `none` here would mean the deployed worker presented no
	// identity at all — what a deleted or unwired resolver produces, and what a
	// packaging path that never carried the declaration produces.
	if got.declineResolved != 1 {
		t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 1 — the booted artifact did not identify the sealed configuration on the deployed route", got.declineResolved)
	}
	if got.declineNone != 0 {
		t.Errorf("preclaim_decline{cohort=none} = %v, want 0 — the deployed route presented no identity for a configuration the deployment sealed", got.declineNone)
	}
	if got.bamlTransport != 1 {
		t.Errorf("winner{baml_transport} = %v, want 1 — BAML must own the request", got.bamlTransport)
	}
}

// TestBootedArtifactRefusesIdentityForACallerDefinedConfiguration is the provenance
// proof on the deployed route: the SAME artifact, the SAME declaration, the SAME
// effective configuration — but the request DEFINES it instead of naming it, and gets
// no identity. With an enrollment present, an identity here would be an out-claim by
// a request the deployment never approved.
func TestBootedArtifactRefusesIdentityForACallerDefinedConfiguration(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, callerDefines: true, flagOn: true})

	assertServedByBAML(t, "flag on, caller-defined configuration", got)
	assertZeroNativeOnTheArtifact(t, "flag on, caller-defined configuration", got)
	if got.declineResolved != 0 {
		t.Errorf("a caller-defined configuration resolved an identity (unrecognized=%v); client_registry is the CALLER's document and can never be an identity", got.declineResolved)
	}
	if got.declineNone != 1 {
		t.Errorf("preclaim_decline{cohort=none} = %v, want 1 — a caller-defined configuration must present NO identity", got.declineNone)
	}
}

// TestBootedArtifactWithNoDeclarationPresentsNoIdentity is the control that makes the
// `unrecognized` reading causal rather than a label default: same artifact, same
// route, same request, nothing declared.
func TestBootedArtifactWithNoDeclarationPresentsNoIdentity(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{callerDefines: true, flagOn: true})

	assertServedByBAML(t, "flag on, nothing declared", got)
	assertZeroNativeOnTheArtifact(t, "flag on, nothing declared", got)
	if got.declineNone != 1 {
		t.Errorf("preclaim_decline{cohort=none} = %v, want 1 — an undeclared deployment must present no identity", got.declineNone)
	}
	if got.declineResolved != 0 {
		t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 0 — nothing was declared, so nothing may resolve", got.declineResolved)
	}
}

// TestBootedArtifactWithTheFlagOffServesTheRouteWithNoNativeWork is the flag-off arm
// on the deployed route, with the resolver present and the configuration DECLARED: the
// one global kill switch must still mean the artifact installs no native factory at
// all — observable as the total absence of every de-BAML collector, because each of
// them is registered by a factory that only exists in the flag-on branch — while the
// public route keeps serving ordinary BAML.
func TestBootedArtifactWithTheFlagOffServesTheRouteWithNoNativeWork(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, flagOn: false})

	assertServedByBAML(t, "flag off, deployment-sealed identity", got)
	assertZeroNativeOnTheArtifact(t, "flag off, deployment-sealed identity", got)
	// ZERO native factories ran. Every de-BAML admission collector is registered by a
	// factory constructed only inside the flag-on branch, so their total absence is
	// the runtime observation of "no native init, no factory, no socket". The two S2
	// artifact-identity gauges are registered unconditionally and are the allowed
	// exceptions — the same rule the workerboot artifact lane applies, stated the same
	// way so the two cannot drift apart.
	allowed := map[string]bool{
		artifactprofile.ArtifactInfoMetric: true,
		artifactprofile.ExpectationMetric:  true,
	}
	for _, name := range got.deBAMLFamilies {
		if !allowed[name] {
			t.Errorf("the flag-off artifact exposes de-BAML collector %q; a native factory ran behind the kill switch", name)
		}
	}
	// Non-vacuity: an empty de-BAML metric set would satisfy the loop above while
	// actually meaning "the metrics RPC told us nothing".
	for want := range allowed {
		found := false
		for _, name := range got.deBAMLFamilies {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the flag-off artifact does not expose %q; the zero-native assertion above read an empty metric set", want)
		}
	}
	if got.declineNone != 0 || got.declineResolved != 0 {
		t.Errorf("the flag-off artifact recorded de-BAML declines (none=%v, resolved=%v); it must run no admission at all",
			got.declineNone, got.declineResolved)
	}
}
