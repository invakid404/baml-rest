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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
// configuration class in the S3a arms. It is a declared-but-UNENROLLED production
// slot, which is exactly what those arms need: the identity resolves, and the
// shipped policy still refuses it.
const routeProofFingerprint = "cfg001"

// feV1RouteFingerprint / feV1RouteCohort are the ENROLLED tuple the serving cutover
// ships (S3b). They are spelled as literals here because cmd/serve lives in the root
// module and nativeserve is a separate out-of-workspace one — so instead of
// importing the constants, TestBootedArtifactPublishesTheShippedFeV1Enrollment reads
// them back off the BOOTED ARTIFACT's own inventory gauge and fails if the binary
// enrolls anything other than exactly this. That is the stronger check: it verifies
// the deployed binary rather than agreeing with a constant in the same repository.
const (
	feV1RouteFingerprint = "cfg100"
	feV1RouteCohort      = "fe_v1"
)

const routeProofClient = "RouteProofClient"

// routeProofStreamChunks is the deterministic OpenAI SSE stream the provider
// serves to a streaming request: one content delta carrying the same flattened
// answer the unary body carries, a stop chunk, and the terminator.
var routeProofStreamChunks = []string{
	`{"id":"route-proof","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":null}]}`,
	`{"id":"route-proof","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`[DONE]`,
}

// capturedUpstream is one request the loopback provider actually received, kept
// so a stock leg and a native leg can be compared ON THE WIRE rather than only by
// their answers. Headers are the full multimap in received order.
type capturedUpstream struct {
	method  string
	target  string
	host    string
	headers http.Header
	body    []byte
}

// routeProofProvider is a loopback OpenAI-shaped provider. Its counter is what "BAML
// sent exactly one request" is read from, and its capture log is what the stock/native
// wire differential is read from.
type routeProofProvider struct {
	srv   *httptest.Server
	calls atomic.Int64

	mu   sync.Mutex
	seen []capturedUpstream
}

func newRouteProofProvider(t *testing.T) *routeProofProvider {
	t.Helper()
	return newRouteProofProviderAt(t, "")
}

// newRouteProofProviderAt is [newRouteProofProvider] bound to a SPECIFIC loopback
// address. An empty addr takes an ephemeral port, which is what every arm whose
// base_url the request carries wants.
//
// A fixed address is needed by exactly one arm: the STATIC surface. Its fixture
// project bakes its client's base_url as a literal in generated source, so the
// capture server has to bind THAT address for the booted subprocess to reach it.
// A bind failure is a lane fault, not a reason to report success, so it is fatal.
func newRouteProofProviderAt(t *testing.T, addr string) *routeProofProvider {
	t.Helper()
	p := &routeProofProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.seen = append(p.seen, capturedUpstream{
			method:  r.Method,
			target:  r.URL.RequestURI(),
			host:    r.Host,
			headers: r.Header.Clone(),
			body:    body,
		})
		p.mu.Unlock()
		// The STREAM surface asks the same provider for SSE. Answer in the wire
		// shape the request actually asked for, with the SAME final content, so a
		// stream arm compares an answer rather than a transport mismatch.
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, chunk := range routeProofStreamChunks {
				_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"route-proof","object":"chat.completion",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}]}`))
	})
	if addr == "" {
		p.srv = httptest.NewServer(handler)
		t.Cleanup(p.srv.Close)
		return p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind the fixture's baked base_url %s: %v — this proof drives a booted artifact whose client address is a generated literal, so a busy port is a lane fault, not a reason to report success", addr, err)
	}
	p.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	p.srv.Start()
	t.Cleanup(p.srv.Close)
	return p
}

// captured returns the requests this provider received, oldest first.
func (p *routeProofProvider) captured() []capturedUpstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]capturedUpstream(nil), p.seen...)
}

// routeProofDeclaration is the deployment's approved-configuration declaration,
// pointed at the loopback provider. A request may NAME this class; it may not define
// it.
func routeProofDeclaration(base, fingerprint string) string {
	return fmt.Sprintf(
		`{"trusted_clients":[{"name":%q,"fingerprint":%q,"provider":"openai",`+
			`"options":{"model":"gpt-4o-mini","base_url":%q,"api_key":"sk-route-proof"}}]}`,
		routeProofClient, fingerprint, base+"/v1")
}

// routeProofSchema is the deterministic output schema every arm uses unless it
// deliberately overrides it (the unsupported-schema arm).
func routeProofSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

// routeProofBody is the PUBLIC `/call/Baml_Rest_Dynamic` request body. namedOnly picks
// the shape: a request that merely NAMES the approved class (which the deployment
// seals), or one that DEFINES the configuration itself with the very same values
// (which it must not). registry/schema override the whole client_registry or output
// schema, which is how the deployed-route matrix drives the fallback / round-robin /
// legacy / unsupported-schema shapes through the very same public route.
func routeProofBody(t *testing.T, base string, namedOnly bool, registry *bamlutils.ClientRegistry, schema *bamlutils.DynamicOutputSchema) []byte {
	t.Helper()
	text := "hello"
	if registry == nil {
		primary := routeProofClient
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
		registry = &bamlutils.ClientRegistry{Primary: &primary, Clients: []*bamlutils.ClientProperty{client}}
	}
	if schema == nil {
		schema = routeProofSchema()
	}
	body, err := json.Marshal(bamlutils.DynamicInput{
		Messages:       []bamlutils.DynamicMessage{{Role: "user", TextContent: &text}},
		ClientRegistry: registry,
		OutputSchema:   schema,
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
	// header is the public route's own response header set — the rest of the
	// client-visible envelope beside status + body.
	header http.Header
	// wire is what the provider ACTUALLY received on this leg, which is what the
	// stock/native request-plan differential is read from.
	wire []capturedUpstream

	// The BOOTED WORKER's own collectors, gathered over the plugin boundary — not a
	// registry this process constructed.
	claims          float64
	nativeWinners   float64
	nativeSockets   float64
	declineNone     float64
	declineResolved float64
	bamlTransport   float64
	// The S3b serving readings: what the enrolled cohort actually did.
	feV1Claims        float64
	feV1NativeWinners float64
	feV1ParseOnly     float64
	feV1Failures      float64
	feV1SameResponse  float64
	planCompareMatch  float64
	planCompareBad    float64
	respCompareBad    float64
	// respCompareMatch / respCompareMismatch are the same-response oracle read
	// PER FACET, so "assistant/raw/reasoning/structured/order all agreed" is an
	// assertion about this served request rather than an aggregate.
	respCompareMatch    map[string]float64
	respCompareMismatch map[string]float64
	// preclaimDeclines is every pre-claim decline on the dynamic call surface,
	// whatever cohort bucket it resolved to.
	preclaimDeclines float64
	// phaseBySurface / winnerBySurface are the artifact's OWN admission-phase and
	// winner readings for EVERY surface, keyed "<surface>/<phase>" and
	// "<surface>/<winner>". The dynamic-call arms read the dedicated fields above;
	// the stream and static arms need their own surface's decline to be observable,
	// because that is the only per-shape signal proving the native lane's callback
	// was invoked for THAT request and declined pre-socket (there is no
	// X-BAML-Path equivalent for "the native seam ran").
	phaseBySurface  map[string]float64
	winnerBySurface map[string]float64
	// phaseBySurfaceCohort / winnerBySurfaceCohort keep the COHORT label instead of
	// collapsing it, keyed "<surface>/<cohort>/<phase>" and
	// "<surface>/<cohort>/<winner>". Which of the three refusal shapes a decline
	// recorded — no identity at all (`none`), an identity the inventory does not
	// recognise for this surface (`unrecognized`), or an enrolled bucket — is the
	// only thing that makes a decline ATTRIBUTABLE to the configuration in front of
	// it, so an arm that wants to say more than "it declined" has to read it.
	phaseBySurfaceCohort  map[string]float64
	winnerBySurfaceCohort map[string]float64
	// The operator-visible enrollment the BOOTED binary publishes about itself.
	policyVersion     string
	policyEnrollments float64
	inventoryRows     []map[string]string
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
	// fingerprint is the opaque slot that declaration assigns. Empty means the
	// UNENROLLED S3a slot; feV1RouteFingerprint is the enrolled one.
	fingerprint string
	// callerDefines sends the whole configuration in the request body instead of
	// naming the class.
	callerDefines bool
	// flagOn is the one global umbrella switch, as the booted worker sees it.
	flagOn bool

	// route selects which PUBLIC unary route the request goes to. Empty means
	// `/call` (the enrolled surface); the matrix also drives `/call-with-raw`
	// (a MODE fe-v1 does not enroll) and `/parse` (the direct-parse surface).
	route routeProofRoute
	// registry overrides the request's whole client_registry, which is how the
	// fallback-chain / round-robin / legacy shapes are driven through the very
	// same public route. It is a FUNCTION of the loopback base because those
	// shapes have to carry this run's own base URL.
	registryFor func(base string) *bamlutils.ClientRegistry
	// schema overrides the request's output schema (the unsupported-schema arm).
	schema *bamlutils.DynamicOutputSchema
	// provider lets several arms share ONE booted artifact + ONE upstream, so a
	// stock leg and a native leg are comparable on the same wire. Nil boots a
	// fresh one.
	provider *routeProofProvider
	// declarationFor overrides the deployment's approved-configuration declaration
	// wholesale. It exists for the cross-surface control, which has to seal the
	// SAME client class the static artifact seals — same name, same options, same
	// slot — so the two surfaces are comparing one configuration rather than two.
	declarationFor func(base string) string
}

// routeProofRoute names a PUBLIC unary route on the booted artifact.
type routeProofRoute string

const (
	routeCall        routeProofRoute = ""
	routeCallWithRaw routeProofRoute = "call-with-raw"
	routeParse       routeProofRoute = "parse"
)

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
	provider := opts.provider
	if provider == nil {
		provider = newRouteProofProvider(t)
	}
	before := provider.calls.Load()
	seenBefore := len(provider.captured())

	// The worker subprocess inherits this process's environment (pool builds its
	// exec.Command from os.Environ()), so t.Setenv is how the DEPLOYMENT's
	// configuration reaches the booted artifact — exactly the channel a real
	// deployment uses.
	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", opts.flagOn))
	if opts.declare {
		fingerprint := opts.fingerprint
		if fingerprint == "" {
			fingerprint = routeProofFingerprint
		}
		declaration := routeProofDeclaration(provider.srv.URL, fingerprint)
		if opts.declarationFor != nil {
			declaration = opts.declarationFor(provider.srv.URL)
		}
		t.Setenv(trustedclients.EnvVar, declaration)
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

	// THE PUBLIC ROUTE, over a REAL HTTP listener. These are the handlers
	// newUnaryRouter installs for the dynamic endpoint, so the request below goes
	// through the same body read, the same decode, the same Validate, the same
	// ToWorkerInput and the same pool dispatch a production request does.
	var handler http.HandlerFunc
	var path string
	var reqBody []byte
	var registry *bamlutils.ClientRegistry
	if opts.registryFor != nil {
		registry = opts.registryFor(provider.srv.URL)
	}
	switch opts.route {
	case routeParse:
		handler = makeChiDynamicParseHandler(workerPool, false)
		path = "/parse/" + bamlutils.DynamicEndpointName
		reqBody = routeProofParseBody(t, opts.schema)
	case routeCallWithRaw:
		handler = makeChiDynamicCallHandler(workerPool, bamlutils.StreamModeCallWithRaw, false)
		path = "/call-with-raw/" + bamlutils.DynamicEndpointName
		reqBody = routeProofBody(t, provider.srv.URL, !opts.callerDefines, registry, opts.schema)
	default:
		handler = makeChiDynamicCallHandler(workerPool, bamlutils.StreamModeCall, false)
		path = "/call/" + bamlutils.DynamicEndpointName
		reqBody = routeProofBody(t, provider.srv.URL, !opts.callerDefines, registry, opts.schema)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		t.Fatalf("POST the public %s route: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the %s response: %v", path, err)
	}

	out := newRouteProofResult()
	out.status = resp.StatusCode
	out.body = string(raw)
	out.header = resp.Header.Clone()
	out.providerRequests = provider.calls.Load() - before
	out.wire = provider.captured()[seenBefore:]
	readArtifactDeBAMLMetrics(t, workerPool, &out)
	return out
}

// routeProofParseBody is the PUBLIC `/parse/Baml_Rest_Dynamic` body: the
// direct-parse surface, which carries no client registry and opens no socket at
// all.
func routeProofParseBody(t *testing.T, schema *bamlutils.DynamicOutputSchema) []byte {
	t.Helper()
	if schema == nil {
		schema = routeProofSchema()
	}
	body, err := json.Marshal(bamlutils.DynamicParseInput{
		Raw:          `{"answer":"ok"}`,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("marshal the public /parse body: %v", err)
	}
	return body
}

// newRouteProofResult is the zero observation with every map ready, so a reading
// that is never taken is an explicit zero rather than a nil-map panic.
func newRouteProofResult() routeProofResult {
	return routeProofResult{
		respCompareMatch:      map[string]float64{},
		respCompareMismatch:   map[string]float64{},
		phaseBySurface:        map[string]float64{},
		winnerBySurface:       map[string]float64{},
		phaseBySurfaceCohort:  map[string]float64{},
		winnerBySurfaceCohort: map[string]float64{},
	}
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
				// The operator-visible enrollment the artifact publishes: the policy
				// version and its enrollment count, plus one row per declared record ×
				// surface. Gauges, not counters, so they are read from GetGauge.
				if name == "baml_rest_debaml_cohort_policy_info" {
					out.policyVersion = labels["version"]
					out.policyEnrollments = m.GetGauge().GetValue()
					continue
				}
				if name == "baml_rest_debaml_config_inventory_info" {
					out.inventoryRows = append(out.inventoryRows, labels)
					continue
				}
				switch name {
				case "baml_rest_debaml_admission_phase_total":
					out.phaseBySurface[labels["surface"]+"/"+labels["phase"]] += v
					out.phaseBySurfaceCohort[labels["surface"]+"/"+labels["cohort"]+"/"+labels["phase"]] += v
				case "baml_rest_debaml_winner_total":
					out.winnerBySurface[labels["surface"]+"/"+labels["winner"]] += v
					out.winnerBySurfaceCohort[labels["surface"]+"/"+labels["cohort"]+"/"+labels["winner"]] += v
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
				case name == "baml_rest_debaml_plan_compare_total":
					if labels["result"] == "match" {
						out.planCompareMatch += v
					} else {
						out.planCompareBad += v
					}
				case name == "baml_rest_debaml_response_compare_total":
					// FAIL CLOSED on the result label. Only the literal "match"
					// counts as agreement: treating "anything that is not mismatch"
					// as a match would let an unexpected or future result label —
					// including the oracle's own `error` shapes — satisfy the
					// same-response assertions, which is the one thing this reading
					// exists to prevent.
					if labels["result"] == "match" {
						out.respCompareMatch[labels["field"]] += v
					} else {
						out.respCompareBad += v
						out.respCompareMismatch[labels["field"]] += v
					}
				}
				if name == "baml_rest_debaml_admission_phase_total" &&
					labels["surface"] == "dynamic_call" && labels["phase"] == "preclaim_decline" {
					out.preclaimDeclines += v
				}
				// The enrolled cohort's own serving readings, kept separate from the
				// surface-wide ones above so "fe_v1 claimed" and "something claimed"
				// can never be confused.
				if labels["surface"] == "dynamic_call" && labels["cohort"] == feV1RouteCohort {
					switch {
					case name == "baml_rest_debaml_admission_phase_total" && labels["phase"] == "claimed":
						out.feV1Claims += v
					case name == "baml_rest_debaml_admission_phase_total" && labels["phase"] == "same_response_oracle":
						out.feV1SameResponse += v
					case name == "baml_rest_debaml_winner_total" && labels["winner"] == "native":
						out.feV1NativeWinners += v
					case name == "baml_rest_debaml_winner_total" && labels["winner"] == "baml_parse_same_response":
						out.feV1ParseOnly += v
					case name == "baml_rest_debaml_winner_total" && labels["winner"] == "failure":
						out.feV1Failures += v
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

// --- serving cutover S3b: the ENROLLED cohort on the deployed route ----------

// TestBootedArtifactPublishesTheShippedFeV1Enrollment reads the enrollment off the
// BOOTED BINARY's own gauges rather than off a constant in this repository. It is
// what lets every assertion below name `cfg100` / `fe_v1` as literals: if the
// shipped policy ever enrolls something else, this fails first and says so.
func TestBootedArtifactPublishesTheShippedFeV1Enrollment(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, fingerprint: feV1RouteFingerprint, flagOn: true})

	if got.policyEnrollments != 1 {
		t.Fatalf("the booted artifact enrolls %v (surface, cohort) pair(s), want exactly 1", got.policyEnrollments)
	}
	if got.policyVersion == "" || strings.Contains(got.policyVersion, "default-deny-empty") {
		t.Errorf("the booted artifact publishes policy version %q; a worker that enrolls a cohort must not name itself default-deny-empty", got.policyVersion)
	}
	if len(got.inventoryRows) != 1 {
		t.Fatalf("the booted artifact publishes %d inventory row(s), want exactly 1 (the fe-v1 record on its one surface)", len(got.inventoryRows))
	}
	row := got.inventoryRows[0]
	for k, want := range map[string]string{
		"fingerprint": feV1RouteFingerprint,
		"cohort":      feV1RouteCohort,
		"surface":     "dynamic_call",
		"provider":    "openai",
	} {
		if row[k] != want {
			t.Errorf("the booted artifact's inventory row has %s=%q, want %q", k, row[k], want)
		}
	}
	if row["approval"] == "" {
		t.Error("the booted artifact's inventory row carries no approval reference; the enrollment must be joinable to its offline approval")
	}
	// And nothing sensitive rode along with it.
	for k, v := range row {
		if strings.Contains(v, "sk-") || strings.Contains(v, "http") || strings.Contains(v, "gpt-") || strings.Contains(v, routeProofClient) {
			t.Errorf("the published inventory row leaks configuration material in %s=%q", k, v)
		}
	}
}

// TestBootedArtifactServesTheFeV1CohortNativelyOnTheDeployedRoute is the S3b
// headline, and the first assertion in this repository that a REAL BOOTED WORKER
// serves a public request through the native transport.
//
// It is the same artifact, the same public `/call` route over a real HTTP listener,
// the same real pool, and the same request body as the S3a zero-claim proof. The
// ONLY difference is which opaque slot the deployment's declaration assigns — the
// ENROLLED one instead of an unenrolled one — which is exactly what the cutover
// claims the enrollment is: data, not a code path.
func TestBootedArtifactServesTheFeV1CohortNativelyOnTheDeployedRoute(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, fingerprint: feV1RouteFingerprint, flagOn: true})

	// The caller-visible half is unchanged from BAML's: 200, with the model output.
	if got.status != http.StatusOK {
		t.Fatalf("the public /call route returned %d: %s", got.status, got.body)
	}
	if !strings.Contains(got.body, `"ok"`) {
		t.Errorf("the served body does not carry the model output: %s", got.body)
	}

	// ONE upstream request — NATIVE's — and ZERO BAML resend behind it.
	if got.providerRequests != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 (one native RoundTrip, zero BAML resend)", got.providerRequests)
	}
	if got.feV1Claims != 1 {
		t.Errorf("admission_phase{cohort=fe_v1,phase=claimed} = %v, want 1", got.feV1Claims)
	}
	if got.nativeSockets != 1 {
		t.Errorf("native_sockets_total = %v, want exactly 1 (claimed == sockets)", got.nativeSockets)
	}
	if got.feV1NativeWinners != 1 {
		t.Errorf("winner{cohort=fe_v1,winner=native} = %v, want 1 — the deployed route did not serve natively", got.feV1NativeWinners)
	}
	// ZERO parse-only winners, the criterion fe-v1 promotion is gated on.
	if got.feV1ParseOnly != 0 {
		t.Errorf("winner{cohort=fe_v1,winner=baml_parse_same_response} = %v, want 0", got.feV1ParseOnly)
	}
	if got.feV1Failures != 0 {
		t.Errorf("winner{cohort=fe_v1,winner=failure} = %v, want 0", got.feV1Failures)
	}
	// BAML did not transport it, and it was not declined either — the request was
	// owned end to end by the native lane.
	if got.bamlTransport != 0 {
		t.Errorf("winner{baml_transport} = %v, want 0 — native owned the request", got.bamlTransport)
	}
	if got.declineNone != 0 || got.declineResolved != 0 {
		t.Errorf("the served request also recorded a pre-claim decline (none=%v, unrecognized=%v); a request is claimed or declined, never both",
			got.declineNone, got.declineResolved)
	}

	// BOTH retained BAML oracles ran on the deployed path, and both agreed.
	if got.planCompareMatch == 0 {
		t.Error("the deployed route recorded no plan-comparison match; the pre-claim BAML no-send plan oracle did not run")
	}
	if got.planCompareBad != 0 {
		t.Errorf("plan_compare{mismatch} = %v, want 0", got.planCompareBad)
	}
	if got.feV1SameResponse != 1 {
		t.Errorf("admission_phase{phase=same_response_oracle} = %v, want 1 — BAML must parse the same bytes the one native request returned", got.feV1SameResponse)
	}
	if got.respCompareBad != 0 {
		t.Errorf("response_compare{mismatch} = %v, want 0", got.respCompareBad)
	}
}

// TestBootedArtifactStillDeclinesAnUnenrolledSlotWithTheEnrollmentPresent is the
// control that makes the arm above causal: the SAME booted artifact, carrying the
// SAME enrollment, declines a configuration the deployment sealed under a different
// slot. It rules out "the artifact now claims whatever it is given".
func TestBootedArtifactStillDeclinesAnUnenrolledSlotWithTheEnrollmentPresent(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, fingerprint: routeProofFingerprint, flagOn: true})

	assertServedByBAML(t, "flag on, unenrolled slot", got)
	assertZeroNativeOnTheArtifact(t, "flag on, unenrolled slot", got)
	if got.declineResolved != 1 {
		t.Errorf("preclaim_decline{cohort=unrecognized} = %v, want 1 — the artifact must identify the sealed configuration and refuse it", got.declineResolved)
	}
	if got.feV1Claims != 0 || got.feV1NativeWinners != 0 {
		t.Errorf("an unenrolled slot was attributed to the fe-v1 cohort (claims=%v winners=%v)", got.feV1Claims, got.feV1NativeWinners)
	}
}

// TestBootedArtifactWithTheFeV1EnrollmentAndTheFlagOffIsZeroNative is the kill
// switch on the deployed route, with the ENROLLED configuration sealed: the one
// global flag must still be a complete reversal, with no native factory installed
// at all, and the public route still served by BAML.
func TestBootedArtifactWithTheFeV1EnrollmentAndTheFlagOffIsZeroNative(t *testing.T) {
	got := runRouteProof(t, routeProofOpts{declare: true, fingerprint: feV1RouteFingerprint, flagOn: false})

	assertServedByBAML(t, "flag off, ENROLLED configuration sealed", got)
	assertZeroNativeOnTheArtifact(t, "flag off, ENROLLED configuration sealed", got)
	allowed := map[string]bool{
		artifactprofile.ArtifactInfoMetric: true,
		artifactprofile.ExpectationMetric:  true,
	}
	for _, name := range got.deBAMLFamilies {
		if !allowed[name] {
			t.Errorf("the flag-off artifact exposes de-BAML collector %q with the fe-v1 configuration sealed; a native factory ran behind the kill switch", name)
		}
	}
	// Non-vacuity, the same guard the no-declaration flag-off arm carries: an EMPTY
	// de-BAML metric set satisfies the loop above while actually meaning "the
	// metrics RPC told us nothing". The two S2 artifact-identity gauges are
	// registered unconditionally, so their presence is what proves the reading
	// happened at all.
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
	if got.feV1Claims != 0 || got.feV1NativeWinners != 0 || got.nativeSockets != 0 {
		t.Errorf("the flag-off artifact produced native activity (claims=%v winners=%v sockets=%v)",
			got.feV1Claims, got.feV1NativeWinners, got.nativeSockets)
	}
}
