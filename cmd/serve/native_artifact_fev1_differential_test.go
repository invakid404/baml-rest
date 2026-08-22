//go:build subprocess && nativeartifactproof

package main

// De-BAML serving cutover S3b — the BOOTED-ARTIFACT STOCK-vs-NATIVE DIFFERENTIAL
// and the DEPLOYED-ROUTE BAML-PRESERVATION MATRIX.
//
// native_artifact_route_proof_test.go proves the enrolled tuple SERVES on the
// deployed route and that the artifact publishes the enrollment it claims to. This
// file is the other two halves the cutover gates promotion on, and both of them
// run through the SAME booted artifact and the SAME public routes:
//
//  1. ONE end-to-end differential. The exact fe-v1 `/call` request traverses the
//     DEPLOYED public route TWICE against ONE deterministic upstream — once
//     served by stock BAML v0.223 (the umbrella flag off) and once served
//     natively (the flag on, the enrolled slot sealed) — and everything the
//     caller and the provider can see is compared: HTTP status, the response
//     body byte-for-byte (which IS the structured value AND its ordering, since
//     the dynamic `/call` body is the flattened output), the routing/observability
//     header set, and the upstream request on the wire (method, target, host,
//     header multimap, byte-exact body). The native leg additionally has to show
//     ONE native RoundTrip, ZERO BAML resend, winner=native, zero parse-only, and
//     BOTH retained BAML oracles run and matched — including the same-response
//     oracle's raw and reasoning facets, which the `/call` envelope does not
//     expose but the worker's own per-facet counters do.
//
//  2. The DEPLOYED-ROUTE MATRIX. With the enrollment present and the flag ON,
//     every OTHER shape the artifact can be sent still behaves exactly as stock
//     BAML does, with ZERO native sockets: a different unary MODE
//     (`/call-with-raw`), the DIRECT-PARSE surface (`/parse`), a FALLBACK chain, a
//     ROUND-ROBIN strategy, a LEGACY-routed client, an output schema the native
//     renderer does not claim, and a caller-DEFINED configuration. Each arm is a
//     stock/native pair on one upstream, so "BAML preserved" is a comparison
//     rather than an assertion about a token.
//
//  3. The two surfaces with no chi `/call` route of their own — dynamic STREAM and
//     STATIC — proved the same way, and with the same NON-VACUITY guards the unary
//     arms carry. Neither has an `X-BAML-Path` equivalent for "the native seam
//     ran", so both read the artifact's own per-surface decline pair
//     (`admission_phase{surface,preclaim_decline}` + `winner{surface,baml_transport}`)
//     and both require the flag-on artifact to have the native lane INSTALLED. The
//     static arm boots a STATIC-CAPABLE artifact built from a real BAML project, so
//     it drives an actual `/call/<Method>` route rather than an unknown method name.
//
// Every arm boots its own worker subprocess, so these are minutes-scale proofs by
// construction; they are gated with the rest of the artifact lane.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/internal/artifactprofile"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// volatileResponseHeaders are the response headers a differential must NOT pin:
// they encode this attempt's own timing/identity rather than its answer.
var volatileResponseHeaders = map[string]bool{
	"date":                        true,
	"content-length":              true,
	"x-request-id":                true,
	"x-baml-upstream-duration-ms": true,
}

// volatileWireHeaders are the upstream request headers a plan differential must
// NOT pin. Everything else — content-type, authorization, and any
// provider-specific header — IS compared, because a difference there is a real
// request-plan divergence.
//
// Two groups, for two different reasons:
//
//   - transport artifacts of the Go client that sent the plan (host is asserted
//     separately as the effective destination; the rest are connection-level);
//   - `baml-original-url`, BAML's INTERNAL transport header. It is excluded here
//     for exactly the reason nativeserve/parity's ComparePlans excludes it from
//     the BAML side: it is not part of the request BAML intends to send, it is
//     bookkeeping BAML's own transport adds. Using the same rule keeps this
//     differential's header policy identical to the retained plan oracle's, so
//     the two can never disagree about what "the same request" means.
var volatileWireHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"accept-encoding":   true,
	"user-agent":        true,
	"connection":        true,
	"baml-original-url": true,
}

// TestBootedArtifactFeV1MatchesStockBAMLOnTheDeployedRoute is the S3b
// stock-v0.223 differential, run END TO END through the booted artifact's public
// `/call` route rather than assembled from two half-proofs.
func TestBootedArtifactFeV1MatchesStockBAMLOnTheDeployedRoute(t *testing.T) {
	provider := newRouteProofProvider(t)

	// STOCK LEG: the same artifact, the same public route, the same request body,
	// the same upstream — with the umbrella flag OFF, which is BAML v0.223 serving
	// end to end (no native factory is installed at all).
	stock := runRouteProof(t, routeProofOpts{
		declare: true, fingerprint: feV1RouteFingerprint, flagOn: false, provider: provider,
	})
	// NATIVE LEG: the one thing that changes is the flag.
	native := runRouteProof(t, routeProofOpts{
		declare: true, fingerprint: feV1RouteFingerprint, flagOn: true, provider: provider,
	})

	// --- the client-visible envelope -----------------------------------------
	if stock.status != http.StatusOK || native.status != http.StatusOK {
		t.Fatalf("public /call status: stock=%d native=%d (stock body %s / native body %s)",
			stock.status, native.status, stock.body, native.body)
	}
	// BYTE-FOR-BYTE. The dynamic `/call` body IS the flattened structured output,
	// emitted in the request's schema order, so this single comparison covers the
	// structured VALUE and its ORDERING at once.
	if stock.body != native.body {
		t.Errorf("the deployed route's answer differs between engines:\n  stock:  %s\n  native: %s", stock.body, native.body)
	}
	assertHeadersEquivalent(t, "response", stock.header, native.header, volatileResponseHeaders)

	// --- the wire ------------------------------------------------------------
	if len(stock.wire) != 1 || len(native.wire) != 1 {
		t.Fatalf("upstream requests: stock=%d native=%d, want exactly 1 each (one send per leg, zero resend)",
			len(stock.wire), len(native.wire))
	}
	assertWireEquivalent(t, stock.wire[0], native.wire[0])

	// --- the counts the cutover gates on -------------------------------------
	if native.providerRequests != 1 {
		t.Errorf("the native leg's provider saw %d request(s), want exactly 1 (one native RoundTrip, ZERO BAML resend)", native.providerRequests)
	}
	if stock.providerRequests != 1 {
		t.Errorf("the stock leg's provider saw %d request(s), want exactly 1", stock.providerRequests)
	}
	if native.feV1Claims != 1 || native.nativeSockets != 1 {
		t.Errorf("native leg: claimed=%v native_sockets=%v, want 1 and 1", native.feV1Claims, native.nativeSockets)
	}
	if native.feV1NativeWinners != 1 {
		t.Errorf("native leg: winner{fe_v1,native} = %v, want 1", native.feV1NativeWinners)
	}
	if native.feV1ParseOnly != 0 || native.feV1Failures != 0 {
		t.Errorf("native leg: parse_only=%v failures=%v, want 0 and 0", native.feV1ParseOnly, native.feV1Failures)
	}
	// The STOCK leg is the control that makes those readings causal: identical
	// request, identical upstream, zero native activity of any kind.
	assertZeroNativeOnTheArtifact(t, "stock leg (flag off)", stock)

	// --- both retained BAML oracles, per facet -------------------------------
	if native.planCompareMatch == 0 || native.planCompareBad != 0 {
		t.Errorf("native leg plan oracle: match=%v mismatch=%v, want ≥1 and 0", native.planCompareMatch, native.planCompareBad)
	}
	if native.feV1SameResponse != 1 {
		t.Errorf("native leg: same_response_oracle phase = %v, want 1", native.feV1SameResponse)
	}
	// EVERY facet of the native-winner predicate was compared on this served
	// request and agreed — including the raw and reasoning channels, which the
	// `/call` envelope never exposes to the caller, so this is the only place the
	// deployed route can show that BAML read them the same way.
	for _, facet := range []string{"assistant", "raw", "reasoning", "structured", "order"} {
		if native.respCompareMatch[facet] != 1 {
			t.Errorf("native leg: response_compare{match,%s} = %v, want 1 — the facet was not compared on the deployed route",
				facet, native.respCompareMatch[facet])
		}
		if native.respCompareMismatch[facet] != 0 {
			t.Errorf("native leg: response_compare{mismatch,%s} = %v, want 0", facet, native.respCompareMismatch[facet])
		}
	}
}

// TestBootedArtifactPreservesBAMLForEveryUnenrolledShapeOnTheDeployedRoute is the
// deployed-route half of the admission matrix: with the fe-v1 enrollment present
// and the flag ON, every shape that is not the enrolled tuple is still served by
// BAML, byte-identically to the stock leg, with ZERO native sockets.
//
// Each arm is a PAIR against one upstream — flag off, then flag on — so the claim
// is "the two engines produced the same thing", not "the token said BAML".
func TestBootedArtifactPreservesBAMLForEveryUnenrolledShapeOnTheDeployedRoute(t *testing.T) {
	for _, arm := range []struct {
		name string
		// wantProviderRequests is what BAML itself puts on the wire for this
		// shape; -1 means "whatever stock did", which is the right answer for
		// shapes BAML routes differently.
		wantProviderRequests int
		// wantPath is the BAML dispatch path the arm must actually have taken,
		// read off the route's own X-BAML-Path header. It is what stops an arm
		// from passing because it drove some OTHER shape than the one it names.
		wantPath string
		// wantSeamDecline says whether the native serve callback is REACHED and
		// declines (1) or is never offered the request at all (0). Both are
		// zero-native; they are different, stronger-or-weaker facts, and pinning
		// which one applies keeps an arm from silently changing meaning.
		wantSeamDecline float64
		// skipHeaders are response headers this arm alone must not pin.
		skipHeaders map[string]bool
		opts        routeProofOpts
	}{
		{
			// A different unary MODE. fe-v1 enrolls ModeCall only;
			// ModeCallWithRaw is refused at admission layer 1.
			name:                 "call-with-raw mode",
			wantProviderRequests: 1,
			wantPath:             "buildrequest",
			wantSeamDecline:      1,
			opts:                 routeProofOpts{route: routeCallWithRaw},
		},
		{
			// The DIRECT-PARSE surface: no provider request exists at all, and
			// the fe-v1 record declares dynamic_call only.
			name:                 "direct parse surface",
			wantProviderRequests: 0,
			wantSeamDecline:      0,
			opts:                 routeProofOpts{route: routeParse},
		},
		{
			// A FALLBACK chain over the approved class: an orchestration shape
			// whose effective selected leaf is not a single proven answer.
			name:                 "fallback chain",
			wantProviderRequests: -1,
			wantPath:             "buildrequest",
			wantSeamDecline:      1,
			opts:                 routeProofOpts{registryFor: routeProofStrategyRegistry("fallback")},
		},
		{
			// ROUND ROBIN over the approved class.
			name:                 "round robin",
			wantProviderRequests: -1,
			wantPath:             "buildrequest",
			wantSeamDecline:      1,
			// Round robin ROTATES: two consecutive requests through one host
			// coordinator legitimately select different INDEXES. The child they
			// select is compared (both entries name the approved class), and the
			// answer and the wire are compared; the rotation counter is the one
			// thing that is supposed to move.
			skipHeaders: map[string]bool{"x-baml-roundrobin-index": true},
			opts:        routeProofOpts{registryFor: routeProofStrategyRegistry("round-robin")},
		},
		{
			// A LEGACY-routed request: an explicitly EMPTY provider override on
			// the named class drops BAML off the BuildRequest orchestrator onto
			// its legacy dispatch path — where there is no native seam at all.
			// The arm verifies BAML really took that path (X-BAML-Path) rather
			// than assuming it.
			name:                 "legacy dispatch path",
			wantProviderRequests: -1,
			wantPath:             "legacy",
			// The legacy route offers the native callback nothing at all — there
			// is no BuildRequest child attempt to admit — which is a STRONGER
			// decline than an admission decline, not a weaker one.
			wantSeamDecline: 0,
			opts:            routeProofOpts{registryFor: routeProofLegacyRegistry},
		},
		{
			// An output schema the native renderer does not claim. It must
			// decline PRE-socket and leave BAML's own runtime schema handling
			// untouched.
			name:                 "unsupported output schema",
			wantProviderRequests: -1,
			wantPath:             "buildrequest",
			wantSeamDecline:      1,
			opts:                 routeProofOpts{schema: routeProofUnsupportedSchema()},
		},
		{
			// The caller DEFINES the configuration instead of naming the class
			// the deployment sealed — the same values, no seal.
			name:                 "caller-defined configuration",
			wantProviderRequests: 1,
			wantPath:             "buildrequest",
			wantSeamDecline:      1,
			opts:                 routeProofOpts{callerDefines: true},
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			provider := newRouteProofProvider(t)

			base := arm.opts
			base.declare, base.fingerprint, base.provider = true, feV1RouteFingerprint, provider
			base.flagOn = false
			stock := runRouteProof(t, base)

			base.flagOn = true
			native := runRouteProof(t, base)

			// BAML PRESERVED: the caller cannot tell the two apart.
			if stock.status != native.status {
				t.Errorf("status: stock=%d native=%d\n  stock body:  %s\n  native body: %s",
					stock.status, native.status, stock.body, native.body)
			}
			if stock.body != native.body {
				t.Errorf("answer differs with the flag on:\n  stock:  %s\n  native: %s", stock.body, native.body)
			}
			skip := volatileResponseHeaders
			if arm.skipHeaders != nil {
				skip = map[string]bool{}
				for k := range volatileResponseHeaders {
					skip[k] = true
				}
				for k := range arm.skipHeaders {
					skip[k] = true
				}
			}
			assertHeadersEquivalent(t, "response", stock.header, native.header, skip)

			// The arm drove the shape it NAMES. Without this an arm could pass by
			// exercising some other route entirely.
			if arm.wantPath != "" && native.header.Get(HeaderBAMLPath) != arm.wantPath {
				t.Errorf("%s: X-BAML-Path = %q, want %q — the arm did not drive the shape it names",
					arm.name, native.header.Get(HeaderBAMLPath), arm.wantPath)
			}
			// NON-VACUITY: the native lane IS installed on this artifact. Without
			// it, "zero native" would be true of a worker that has no de-BAML
			// collector at all — the flag-off state, not a decline.
			assertNativeLaneInstalled(t, arm.name, native)
			if native.preclaimDeclines != arm.wantSeamDecline {
				t.Errorf("%s: the dynamic-call surface recorded %v pre-claim decline(s), want %v",
					arm.name, native.preclaimDeclines, arm.wantSeamDecline)
			}

			// ZERO NATIVE SOCKET. This is the half that makes the enrollment a
			// one-tuple enrollment rather than a general permission.
			assertZeroNativeOnTheArtifact(t, arm.name+" (flag on)", native)
			if native.feV1Claims != 0 || native.feV1NativeWinners != 0 || native.feV1ParseOnly != 0 {
				t.Errorf("%s was attributed to the enrolled cohort (claims=%v winners=%v parse_only=%v)",
					arm.name, native.feV1Claims, native.feV1NativeWinners, native.feV1ParseOnly)
			}
			if native.planCompareMatch != 0 || native.planCompareBad != 0 {
				t.Errorf("%s recorded a plan comparison (match=%v mismatch=%v); a shape that never claims never builds a native plan",
					arm.name, native.planCompareMatch, native.planCompareBad)
			}
			if native.respCompareBad != 0 || len(native.respCompareMatch) != 0 {
				t.Errorf("%s recorded a same-response comparison; nothing was claimed", arm.name)
			}

			// The wire is BAML's, unchanged.
			wantSends := arm.wantProviderRequests
			if wantSends < 0 {
				wantSends = int(stock.providerRequests)
			}
			if int(stock.providerRequests) != wantSends {
				t.Errorf("%s: the stock leg sent %d upstream request(s), want %d", arm.name, stock.providerRequests, wantSends)
			}
			if native.providerRequests != stock.providerRequests {
				t.Errorf("%s: the provider saw %d request(s) with the flag on vs %d with it off; a decline must change nothing on the wire",
					arm.name, native.providerRequests, stock.providerRequests)
			}
			// A differing number of CAPTURED upstream requests is a broken capture
			// leg, not a reason to skip the strict wire comparison — skipping it
			// would turn the one thing that can catch a real plan divergence into a
			// silent no-op. Fail on the count, then still compare every pair that
			// does exist so the diagnostic names the field that drifted.
			if len(stock.wire) != len(native.wire) {
				t.Errorf("%s: captured %d upstream request(s) on the stock leg vs %d on the native leg; the strict wire comparison cannot be complete",
					arm.name, len(stock.wire), len(native.wire))
			}
			for i := 0; i < len(stock.wire) && i < len(native.wire); i++ {
				assertWireEquivalent(t, stock.wire[i], native.wire[i])
			}
		})
	}
}

// TestBootedArtifactPreservesBAMLOnTheDynamicStreamSurface is the STREAM arm of
// the matrix. The dynamic stream route is served by the fiber app rather than the
// chi unary router, so the request goes in at the WORKER boundary the fiber
// handler itself calls — pool.CallStream with StreamModeStream — against the same
// booted artifact carrying the same enrollment. fe-v1 enrolls no stream surface,
// and the stream seam installs a different callback entirely, so the artifact must
// show zero native activity while BAML streams the answer.
func TestBootedArtifactPreservesBAMLOnTheDynamicStreamSurface(t *testing.T) {
	provider := newRouteProofProvider(t)
	stock := runStreamProof(t, provider, false)
	native := runStreamProof(t, provider, true)

	if stock.final == "" {
		t.Fatalf("the stock stream produced no final result; the arm cannot compare anything")
	}
	if stock.final != native.final {
		t.Errorf("the streamed answer differs with the flag on:\n  stock:  %s\n  native: %s", stock.final, native.final)
	}
	if native.providerRequests != stock.providerRequests {
		t.Errorf("the provider saw %d request(s) with the flag on vs %d with it off", native.providerRequests, stock.providerRequests)
	}
	assertZeroNativeOnTheArtifact(t, "dynamic stream (flag on)", native.metrics)
	if native.metrics.feV1Claims != 0 || native.metrics.feV1NativeWinners != 0 {
		t.Errorf("the stream surface was attributed to the enrolled cohort (claims=%v winners=%v)",
			native.metrics.feV1Claims, native.metrics.feV1NativeWinners)
	}

	// NON-VACUITY, in two parts. Without them a flag-on artifact that installed no
	// native lane at all would pass this arm as ordinary all-BAML — which is the
	// kill-switch state, not a decline.
	//
	// (1) the native lane IS installed on the flag-on artifact;
	assertNativeLaneInstalled(t, "dynamic stream", native.metrics)
	// (2) THIS request reached the stream seam and declined there. The stream
	// surface has no X-BAML-Path equivalent for "the native callback ran", so the
	// artifact's own per-surface decline pair is the signal: the dynamic-stream
	// seam records exactly one preclaim_decline + baml_transport winner per
	// declined stream, and records nothing at all if it was never invoked.
	if got := native.metrics.phaseBySurface["dynamic_stream/preclaim_decline"]; got != 1 {
		t.Errorf("admission_phase{surface=dynamic_stream,phase=preclaim_decline} = %v on the flag-on artifact, want 1 — the stream request never reached the native seam, so its zero-native reading proves nothing",
			got)
	}
	if got := native.metrics.winnerBySurface["dynamic_stream/baml_transport"]; got != 1 {
		t.Errorf("winner{surface=dynamic_stream,winner=baml_transport} = %v, want 1 — every declined request records exactly one winner", got)
	}
	for _, w := range []string{"native", "baml_parse_same_response", "failure"} {
		if got := native.metrics.winnerBySurface["dynamic_stream/"+w]; got != 0 {
			t.Errorf("winner{surface=dynamic_stream,winner=%s} = %v, want 0", w, got)
		}
	}
	// The flag-OFF leg is the control that makes the reading above causal: with no
	// native lane there is no seam to reach, so the same request records nothing.
	if got := stock.metrics.phaseBySurface["dynamic_stream/preclaim_decline"]; got != 0 {
		t.Errorf("the flag-OFF artifact recorded %v dynamic_stream decline(s); with the kill switch on nothing native may run at all", got)
	}
	// And the stream request did not move the ENROLLED surface's series.
	if got := native.metrics.phaseBySurface["dynamic_call/preclaim_decline"]; got != stock.metrics.phaseBySurface["dynamic_call/preclaim_decline"] {
		t.Errorf("the stream surface moved the dynamic-call decline series (flag on=%v, off=%v)",
			got, stock.metrics.phaseBySurface["dynamic_call/preclaim_decline"])
	}
}

// staticFixture* is the STATIC booted-artifact fixture: the same shipped
// serve-profile entrypoint, stamped with the same artifact identity, carrying the
// staticserve fixture project's REAL static method table instead of dynclient's one
// dynamic method. scripts/build-s3b-static-fixture-artifact.sh builds it.
const (
	staticFixtureWorkerBinEnv        = "BAML_REST_S3B_STATIC_FIXTURE_WORKER_BIN"
	staticFixtureWorkerArtifactIDEnv = "BAML_REST_S3B_STATIC_FIXTURE_WORKER_ARTIFACT_ID"

	// staticFixtureMethod is the simplest function in that project:
	// `StaticCompletion(topic: string) -> string`, one client, no constraints.
	staticFixtureMethod = "StaticCompletion"
	// staticFixtureClient is the BAML client that function uses.
	staticFixtureClient = "StaticOracleClient"
	// staticFixtureLoopbackAddr is the FIXED loopback the fixture project's client
	// bakes as its base_url (baml_src/clients.baml + the generated introspected
	// package). A booted subprocess reaches the capture server only if the parent
	// binds exactly this address, so the arm binds it rather than an ephemeral one.
	staticFixtureLoopbackAddr = "127.0.0.1:17654"
)

// TestBootedArtifactPreservesBAMLOnTheStaticCallSurface is the STATIC arm of the
// deployed-route matrix, and it drives a REAL static route.
//
// # What it replaces, and why
//
// The first version of this arm sent an unknown method name to the DYNAMIC fixture
// (whose method table is `Baml_Rest_Dynamic` and nothing else) and compared the two
// legs' error strings. A cold review correctly called that false green: the worker
// rejects an unknown method by NAME, before any route, adapter, generated seam,
// native factory or admission gate is reached, so the arm passed with no static
// route, no static serve seam, no admission attempt and no BAML static behaviour
// exercised. It could not have failed.
//
// # What it does now
//
// It boots a STATIC-CAPABLE artifact — the same shipped serve-profile entrypoint,
// the same shipped tag set, the same -ldflags attestation stamp (both fixtures
// stamp the SAME artifact id, asserted below), with one extra tag selecting the
// staticserve fixture project's method table — and POSTs the PUBLIC static route
// `/call/StaticCompletion` over a real HTTP listener into the production chi
// handler, twice against ONE capture upstream: flag OFF (stock BAML v0.223 end to
// end) then flag ON.
//
// The deployment even seals that project's OWN client under the ENROLLED slot, so
// the arm is not passing because the configuration is unrecognisable. It declines
// anyway, and that is the property: the enrollment is per (surface, cohort) and
// names `dynamic_call` ONLY, so the static lane presents no identity at all and the
// default-deny gate refuses it before any native work.
func TestBootedArtifactPreservesBAMLOnTheStaticCallSurface(t *testing.T) {
	wantID := strings.TrimSpace(os.Getenv(staticFixtureWorkerArtifactIDEnv))
	if wantID == "" {
		t.Fatalf("%s is not set: this lane must BOOT a STATIC-CAPABLE artifact and send it a real static request; a missing artifact is a lane misconfiguration, not a reason to report success", staticFixtureWorkerArtifactIDEnv)
	}

	provider := newRouteProofProviderAt(t, staticFixtureLoopbackAddr)
	stock := runStaticRouteProof(t, provider, staticRouteOpts{fingerprint: feV1RouteFingerprint})
	native := runStaticRouteProof(t, provider, staticRouteOpts{flagOn: true, fingerprint: feV1RouteFingerprint})

	// The binary under proof is the SHIPPED artifact, not a lookalike: it publishes
	// the standard native-capable profile and the artifact id its own build stamped
	// — the same id the DYNAMIC fixture stamps, because both attest the shipped tag
	// set. Checked before anything is claimed on the artifact's behalf.
	if native.artifactProfile != string(artifactprofile.ProfileNativeCapable) {
		t.Fatalf("the booted static fixture publishes profile=%q, want %q", native.artifactProfile, artifactprofile.ProfileNativeCapable)
	}
	if native.artifactID != wantID {
		t.Fatalf("the booted static fixture publishes artifact_id=%q, want the shipped serve-profile artifact's %q", native.artifactID, wantID)
	}

	// The static route really ran: 200, a body, and BAML's own send on the wire.
	if stock.status != http.StatusOK {
		t.Fatalf("the public /call/%s route returned %d on the STOCK leg: %s", staticFixtureMethod, stock.status, stock.body)
	}
	if strings.TrimSpace(stock.body) == "" {
		t.Fatalf("the stock static leg produced an empty body; the arm cannot compare anything")
	}
	if stock.providerRequests != 1 {
		t.Fatalf("the stock static leg put %d request(s) on the wire, want exactly 1 — a static call that never reached the provider exercises no BAML behaviour", stock.providerRequests)
	}

	// BAML PRESERVED, byte for byte, with the flag on.
	if stock.status != native.status {
		t.Errorf("static /call status: stock=%d native=%d\n  stock body:  %s\n  native body: %s",
			stock.status, native.status, stock.body, native.body)
	}
	if stock.body != native.body {
		t.Errorf("the static route's answer differs with the flag on:\n  stock:  %s\n  native: %s", stock.body, native.body)
	}
	assertHeadersEquivalent(t, "response", stock.header, native.header, volatileResponseHeaders)
	if native.providerRequests != stock.providerRequests {
		t.Errorf("the provider saw %d request(s) with the flag on vs %d with it off; a decline must change nothing on the wire",
			native.providerRequests, stock.providerRequests)
	}
	// A differing number of CAPTURED upstream requests is a broken capture leg, not
	// a reason to skip the strict wire comparison — skipping it would turn the one
	// thing that can catch a real plan divergence into a silent no-op. Fail on the
	// count, then still compare every pair that does exist so the diagnostic names
	// the field that drifted. Stated exactly as the unary matrix arm states it, so
	// the two guards cannot drift apart.
	if len(stock.wire) != len(native.wire) {
		t.Errorf("static call: captured %d upstream request(s) on the stock leg vs %d on the native leg; the strict wire comparison cannot be complete",
			len(stock.wire), len(native.wire))
	}
	for i := 0; i < len(stock.wire) && i < len(native.wire); i++ {
		assertWireEquivalent(t, stock.wire[i], native.wire[i])
	}

	// ZERO NATIVE SOCKET, and nothing attributed to the enrolled cohort.
	assertZeroNativeOnTheArtifact(t, "static call (flag on)", native)
	if native.feV1Claims != 0 || native.feV1NativeWinners != 0 || native.feV1ParseOnly != 0 {
		t.Errorf("the static surface was attributed to the enrolled cohort (claims=%v winners=%v parse_only=%v)",
			native.feV1Claims, native.feV1NativeWinners, native.feV1ParseOnly)
	}
	if native.planCompareMatch != 0 || native.planCompareBad != 0 || native.respCompareBad != 0 {
		t.Errorf("the static surface ran a BAML oracle (plan match=%v mismatch=%v, response mismatch=%v); nothing was claimed",
			native.planCompareMatch, native.planCompareBad, native.respCompareBad)
	}

	// NON-VACUITY, the same two parts the unary arms require.
	assertNativeLaneInstalled(t, "static call", native)
	if got := native.phaseBySurface["static_call/preclaim_decline"]; got != 1 {
		t.Errorf("admission_phase{surface=static_call,phase=preclaim_decline} = %v on the flag-on artifact, want 1 — the static request never reached the native static seam, so its zero-native reading proves nothing",
			got)
	}
	// ATTRIBUTION — the cohort label is READ, not collapsed away. `unrecognized` is
	// the gate saying "an identity WAS resolved and this surface is not one the
	// record declares it for"; `none` is "nothing identifiable ever arrived". A
	// decline that recorded `none` here would be the generic no-identity refusal and
	// would prove nothing about the deployment's approved configuration.
	assertStaticDeclineCohort(t, "flag on, fe-v1 slot sealed", native, "unrecognized")
	// The flag-OFF control: with the kill switch on there is no seam to reach, so
	// the same request records nothing at all.
	if got := stock.phaseBySurface["static_call/preclaim_decline"]; got != 0 {
		t.Errorf("the flag-OFF artifact recorded %v static_call decline(s); with the kill switch on nothing native may run", got)
	}
	// And a static request did not move the ENROLLED surface's series.
	if got := native.phaseBySurface["dynamic_call/preclaim_decline"]; got != 0 {
		t.Errorf("the static surface moved the dynamic-call decline series (%v); a static call is not a dynamic one", got)
	}

	// THE MUTATION BITE for the attribution above, and it is a SINGLE-VARIABLE one.
	// The deployment still declares the very same class under the very same slot;
	// only the REQUEST changes, dropping the `client_registry` through which it
	// selected that class. The worker's seal runs over a carried registry and
	// nothing else, so the sealed configuration no longer reaches the gate — and the
	// identical answer on the identical wire must now be attributed to `none`.
	//
	// Without this, `unrecognized` could be true for a reason that has nothing to do
	// with the configuration in front of the gate.
	t.Run("the same request without the sealed configuration has no identity at all", func(t *testing.T) {
		unsealed := runStaticRouteProof(t, provider, staticRouteOpts{
			flagOn: true, fingerprint: feV1RouteFingerprint, omitRegistry: true,
		})

		assertNativeLaneInstalled(t, "static call, no sealed configuration", unsealed)
		if got := unsealed.phaseBySurface["static_call/preclaim_decline"]; got != 1 {
			t.Fatalf("admission_phase{surface=static_call,phase=preclaim_decline} = %v, want 1 — the mutation must still reach the seam, or it proves nothing", got)
		}
		assertStaticDeclineCohort(t, "flag on, no sealed configuration", unsealed, "none")
		// The mutation moves the ATTRIBUTION and nothing else: same status, same
		// served answer, same single upstream send, still zero native sockets.
		if unsealed.status != stock.status || unsealed.body != stock.body {
			t.Errorf("the unsealed leg answered differently (%d): %s", unsealed.status, unsealed.body)
		}
		if unsealed.providerRequests != stock.providerRequests {
			t.Errorf("the unsealed leg put %d request(s) on the wire vs %d; the mutation must change only the identity",
				unsealed.providerRequests, stock.providerRequests)
		}
		if unsealed.nativeSockets != 0 {
			t.Errorf("the unsealed leg opened %v native socket(s)", unsealed.nativeSockets)
		}
	})
}

// assertStaticDeclineCohort pins the static surface's decline to EXACTLY ONE cohort
// bucket, on both the phase and the winner series, and requires every other bucket
// and every non-BAML winner to be zero.
//
// # Why `unrecognized` and not `fe_v1`, and why that is the strongest TRUE statement
//
// A sealed fe-v1 configuration arriving on `static_call` CANNOT be labelled `fe_v1`,
// and that is the enrollment invariant working rather than a gap in the proof.
// CohortGate.Resolve returns an inventory record's cohort ONLY on the surfaces that
// record DECLARES (nativeserve/admission/cohort.go), and the shipped fe-v1 record
// declares `dynamic_call` and nothing else — deliberately, because S3b enrolls
// exactly one (surface, cohort) tuple and the scope requires every other surface to
// stay absent. So the gate folds it onto the reserved, NON-ENROLLABLE `unrecognized`
// bucket. Making the label read `fe_v1` here would mean adding `static_call` to the
// record's declared surfaces — widening what the deployment states the class is
// approved for — which the scope forbids and the enrollment bites would (rightly)
// fail.
//
// So the strongest true statement this surface can make is the three-way one the
// gate itself draws: `none` (nothing identifiable arrived) vs `unrecognized` (an
// identity was resolved and refused for this surface) vs an enrolled bucket
// (impossible here). This arm asserts the middle one, and its mutation bite proves
// the first one is what it would otherwise have been. That the resolved identity is
// specifically the ENROLLED class is proved by
// TestBootedArtifactSealsTheSameClassThatClaimsOnTheEnrolledSurface, which seals the
// SAME client under the SAME slot with the SAME options and shows it claiming as
// fe_v1 on `dynamic_call`.
func assertStaticDeclineCohort(t *testing.T, label string, got routeProofResult, wantCohort string) {
	t.Helper()
	for _, cohort := range []string{"none", "unrecognized", feV1RouteCohort} {
		want := 0.0
		if cohort == wantCohort {
			want = 1
		}
		if v := got.phaseBySurfaceCohort["static_call/"+cohort+"/preclaim_decline"]; v != want {
			t.Errorf("%s: admission_phase{surface=static_call,cohort=%s,phase=preclaim_decline} = %v, want %v",
				label, cohort, v, want)
		}
		if v := got.winnerBySurfaceCohort["static_call/"+cohort+"/baml_transport"]; v != want {
			t.Errorf("%s: winner{surface=static_call,cohort=%s,winner=baml_transport} = %v, want %v",
				label, cohort, v, want)
		}
	}
	// No claimed terminal of any kind, under any bucket.
	for _, cohort := range []string{"none", "unrecognized", feV1RouteCohort} {
		for _, w := range []string{"native", "baml_parse_same_response", "failure"} {
			if v := got.winnerBySurfaceCohort["static_call/"+cohort+"/"+w]; v != 0 {
				t.Errorf("%s: winner{surface=static_call,cohort=%s,winner=%s} = %v, want 0", label, cohort, w, v)
			}
		}
		if v := got.phaseBySurfaceCohort["static_call/"+cohort+"/claimed"]; v != 0 {
			t.Errorf("%s: admission_phase{surface=static_call,cohort=%s,phase=claimed} = %v, want 0 — static_call is not enrolled", label, cohort, v)
		}
	}
}

// staticFixtureDeclaration is the DEPLOYMENT's approved-configuration declaration
// for the static fixture project's own client, under the given opaque slot. The
// options are the fixture's baked ones byte for byte, because a seal only applies
// to a client the deployment configured exactly as it declared it.
func staticFixtureDeclaration(fingerprint string) string {
	return fmt.Sprintf(
		`{"trusted_clients":[{"name":%q,"fingerprint":%q,"provider":"openai",`+
			`"options":{"model":"fake-static-oracle-model","base_url":"http://%s/v1","api_key":"fake-static-oracle-key"}}]}`,
		staticFixtureClient, fingerprint, staticFixtureLoopbackAddr)
}

// staticFixtureBody is the PUBLIC static `/call/<Method>` request body.
//
// withRegistry NAMES the fixture project's client in
// `__baml_options__.client_registry`, and naming it is the whole point:
// workerBamlOptions.apply runs the worker's trusted-configuration seal ONLY over a
// registry a request actually carries, so a body without one can never present a
// deployment-owned identity however the deployment declared it. A request that
// merely NAMES a client is exactly the shape the seal is for — it may name the
// class, it may never define it — and the seal then installs the deployment's own
// provider and options onto it.
//
// Both shapes send the SAME bytes to the SAME upstream: the sealed one because the
// seal installs the deployment's options, the bare one because BAML falls back to
// the identical client baked in the fixture project's own baml_src. That is what
// makes dropping the registry a clean single-variable mutation of the IDENTITY
// dimension and nothing else.
func staticFixtureBody(withRegistry bool) string {
	if !withRegistry {
		return `{"topic":"the deployed static route"}`
	}
	return fmt.Sprintf(
		`{"topic":"the deployed static route","__baml_options__":{"client_registry":{`+
			`"primary":%q,"clients":[{"name":%q}]}}}`,
		staticFixtureClient, staticFixtureClient)
}

// staticRouteOpts is one static-surface leg.
type staticRouteOpts struct {
	// flagOn is the one global umbrella switch, as the booted worker sees it.
	flagOn bool
	// fingerprint is the opaque slot the deployment seals the fixture client under.
	fingerprint string
	// omitRegistry drops `__baml_options__.client_registry` from the request. It is
	// the identity mutation: the deployment still declares the same class, but the
	// request no longer selects it through the one channel the seal runs over, so
	// nothing deployment-owned reaches the gate.
	omitRegistry bool
}

// runStaticRouteProof boots the STATIC-capable artifact and drives ONE public
// `/call/<StaticMethod>` request through it, over a real HTTP listener into the
// production chi static handler — the same handler newUnaryRouter installs for a
// schema-defined method.
func runStaticRouteProof(t *testing.T, provider *routeProofProvider, opts staticRouteOpts) routeProofResult {
	t.Helper()
	bin := staticFixtureBinary(t)
	before := provider.calls.Load()
	seenBefore := len(provider.captured())

	// The DEPLOYMENT's own configuration, reaching the booted artifact through the
	// channel a real deployment uses.
	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", opts.flagOn))
	t.Setenv(trustedclients.EnvVar, staticFixtureDeclaration(opts.fingerprint))

	workerPool, err := pool.New(&pool.Config{
		WorkerPath:         bin,
		PoolSize:           1,
		LogOutput:          io.Discard,
		WorkerStartTimeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("pool.New over the static-capable artifact: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = workerPool.Shutdown(ctx)
	})

	srv := httptest.NewServer(makeChiCallHandler(workerPool, staticFixtureMethod, bamlutils.StreamModeCall))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/call/"+staticFixtureMethod, "application/json",
		strings.NewReader(staticFixtureBody(!opts.omitRegistry)))
	if err != nil {
		t.Fatalf("POST the public /call/%s route: %v", staticFixtureMethod, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the static /call response: %v", err)
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

// TestBootedArtifactSealsTheSameClassThatClaimsOnTheEnrolledSurface is the
// CROSS-SURFACE control, and it is what lets the static arm say "the ENROLLED
// configuration was refused" rather than only "some identity was refused".
//
// # Why it is needed
//
// A sealed fe-v1 configuration arriving on `static_call` is labelled
// `unrecognized`, because CohortGate.Resolve returns an inventory record's cohort
// only on the surfaces that record DECLARES, and the shipped fe-v1 record declares
// `dynamic_call` alone. That label is the truth, but it is shared with every other
// declared-yet-uninventoried slot — so on its own it cannot say WHICH configuration
// was refused.
//
// This closes that gap without touching the enrollment: it takes the SAME
// deployment declaration the static arm uses — byte for byte, same client name,
// same provider, same model/base_url/api_key, same `cfg100` slot — and drives it on
// the surface the policy DOES enroll. There it resolves `fe_v1` and CLAIMS
// natively. So the configuration the static surface refuses is demonstrably the
// enrolled class, and the only difference between the two outcomes is the surface.
func TestBootedArtifactSealsTheSameClassThatClaimsOnTheEnrolledSurface(t *testing.T) {
	// The same fixed loopback the static arm binds, so the two surfaces are driven
	// against one upstream with one identical declaration.
	provider := newRouteProofProviderAt(t, staticFixtureLoopbackAddr)

	got := runRouteProof(t, routeProofOpts{
		declare: true, fingerprint: feV1RouteFingerprint, flagOn: true, provider: provider,
		declarationFor: func(string) string { return staticFixtureDeclaration(feV1RouteFingerprint) },
		registryFor: func(string) *bamlutils.ClientRegistry {
			primary := staticFixtureClient
			return &bamlutils.ClientRegistry{
				Primary: &primary,
				Clients: []*bamlutils.ClientProperty{{Name: staticFixtureClient}},
			}
		},
	})

	if got.status != http.StatusOK {
		t.Fatalf("the public /call route returned %d: %s", got.status, got.body)
	}
	// THE POINT: this exact sealed class resolves the ENROLLED bucket on the
	// enrolled surface, and claims there.
	if v := got.phaseBySurfaceCohort["dynamic_call/"+feV1RouteCohort+"/claimed"]; v != 1 {
		t.Errorf("admission_phase{surface=dynamic_call,cohort=%s,phase=claimed} = %v, want 1 — the class the static arm sees refused is not the enrolled one",
			feV1RouteCohort, v)
	}
	if v := got.winnerBySurfaceCohort["dynamic_call/"+feV1RouteCohort+"/native"]; v != 1 {
		t.Errorf("winner{surface=dynamic_call,cohort=%s,winner=native} = %v, want 1", feV1RouteCohort, v)
	}
	if got.nativeSockets != 1 || got.providerRequests != 1 {
		t.Errorf("native_sockets=%v provider_requests=%v, want 1 and 1", got.nativeSockets, got.providerRequests)
	}
	// It was neither refused nor mis-bucketed on the surface it IS enrolled on.
	for _, cohort := range []string{"none", "unrecognized"} {
		if v := got.phaseBySurfaceCohort["dynamic_call/"+cohort+"/preclaim_decline"]; v != 0 {
			t.Errorf("admission_phase{surface=dynamic_call,cohort=%s,phase=preclaim_decline} = %v, want 0", cohort, v)
		}
	}
	// And the enrolled surface is the ONLY one it claims on: nothing static moved.
	for _, phase := range []string{"claimed", "preclaim_decline"} {
		if v := got.phaseBySurface["static_call/"+phase]; v != 0 {
			t.Errorf("a dynamic request moved admission_phase{surface=static_call,phase=%s} = %v", phase, v)
		}
	}
}

// staticFixtureBinary returns the static-capable artifact, failing when the lane did
// not supply it — for the same reason fixtureBinary does: a missing artifact once
// hid a real failure underneath a green skip.
func staticFixtureBinary(t *testing.T) string {
	t.Helper()
	bin, ok := os.LookupEnv(staticFixtureWorkerBinEnv)
	if !ok || strings.TrimSpace(bin) == "" {
		t.Fatalf("%s is not set: this lane must BOOT the static-capable artifact and send it a real static request; build it with scripts/build-s3b-static-fixture-artifact.sh", staticFixtureWorkerBinEnv)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s=%q is not usable: %v", staticFixtureWorkerBinEnv, bin, err)
	}
	return bin
}

// --- shapes ------------------------------------------------------------------

// routeProofStrategyRegistry builds a registry whose PRIMARY is a strategy client
// (fallback or round-robin) over the approved class, so the effective selected
// leaf is decided by an orchestration BAML resolves before any native seam.
func routeProofStrategyRegistry(provider string) func(base string) *bamlutils.ClientRegistry {
	return func(string) *bamlutils.ClientRegistry {
		primary := "RouteProofStrategy"
		return &bamlutils.ClientRegistry{
			Primary: &primary,
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     primary,
					Provider: provider,
					Options:  map[string]any{"strategy": []any{routeProofClient, routeProofClient}},
				},
				{Name: routeProofClient},
			},
		}
	}
}

// routeProofLegacyRegistry names the approved class with an EXPLICITLY EMPTY
// provider override, which BAML resolves off the BuildRequest orchestrator onto
// its legacy dispatch path.
func routeProofLegacyRegistry(string) *bamlutils.ClientRegistry {
	primary := routeProofClient
	return &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{{Name: routeProofClient, Provider: "", ProviderSet: true}},
	}
}

// routeProofUnsupportedSchema is an output schema OUTSIDE the native schema/SAP
// bounds: a property typed by a class the schema never declares. The public
// route's DynamicInput.Validate does not resolve references, so the request
// reaches the seam intact and the NATIVE schema build is what refuses it — which
// is exactly what the arm needs, since BAML serves it normally.
func routeProofUnsupportedSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("detail", &bamlutils.DynamicProperty{Ref: "NeverDeclaredByThisRequest"}),
		),
	}
}

// --- the stream arm's harness -------------------------------------------------

type streamProofResult struct {
	final            string
	providerRequests int64
	metrics          routeProofResult
}

// runStreamProof boots the artifact and drives ONE dynamic STREAM request into it
// at the worker boundary the fiber `/stream` handler uses.
func runStreamProof(t *testing.T, provider *routeProofProvider, flagOn bool) streamProofResult {
	t.Helper()
	workerPool := bootRouteProofPool(t, provider, flagOn)
	before := provider.calls.Load()

	var input bamlutils.DynamicInput
	if err := json.Unmarshal(routeProofBody(t, provider.srv.URL, true, nil, nil), &input); err != nil {
		t.Fatalf("decode the stream request body: %v", err)
	}
	workerInput, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	results, err := workerPool.CallStream(ctx, bamlutils.DynamicMethodName, workerInput, bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream on the dynamic stream surface: %v", err)
	}
	out := streamProofResult{}
	for res := range results {
		if res == nil {
			continue
		}
		if res.Kind == workerplugin.StreamResultKindError {
			t.Fatalf("the dynamic stream returned an error: %v", res.Error)
		}
		if res.Kind == workerplugin.StreamResultKindFinal && len(res.Data) > 0 {
			out.final = string(res.Data)
		}
	}
	out.providerRequests = provider.calls.Load() - before
	out.metrics = newRouteProofResult()
	readArtifactDeBAMLMetrics(t, workerPool, &out.metrics)
	return out
}

// bootRouteProofPool boots the fixture artifact with the fe-v1 declaration and the
// given flag state, exactly as runRouteProof does — the shared half of every arm
// that goes in at the worker boundary rather than through a chi route.
func bootRouteProofPool(t *testing.T, provider *routeProofProvider, flagOn bool) *pool.Pool {
	t.Helper()
	bin := fixtureBinary(t)
	t.Setenv("BAML_REST_USE_DEBAML", fmt.Sprintf("%t", flagOn))
	t.Setenv(trustedclients.EnvVar, routeProofDeclaration(provider.srv.URL, feV1RouteFingerprint))

	p, err := pool.New(&pool.Config{
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
		_ = p.Shutdown(ctx)
	})
	return p
}

// --- comparison helpers --------------------------------------------------------

// assertNativeLaneInstalled requires the booted worker to expose de-BAML
// collectors beyond the two unconditional artifact-identity gauges — i.e. the
// native factory really was installed and really did decline, rather than never
// having existed. It is the exact complement of the flag-off proof's assertion
// that NO such collector exists.
func assertNativeLaneInstalled(t *testing.T, label string, got routeProofResult) {
	t.Helper()
	for _, name := range got.deBAMLFamilies {
		if name != artifactprofile.ArtifactInfoMetric && name != artifactprofile.ExpectationMetric {
			return
		}
	}
	t.Errorf("%s: the flag-on artifact exposes no de-BAML collector at all; zero-native here is the KILL SWITCH, not a decline", label)
}

// assertHeadersEquivalent compares two header sets as multimaps, skipping the
// named volatile ones and saying WHICH header differed — by NAME and value COUNT
// only. It runs over upstream REQUEST headers as well as response ones, and the
// compared set deliberately includes Authorization, so no diagnostic here may
// print a header value.
func assertHeadersEquivalent(t *testing.T, what string, stock, native http.Header, skip map[string]bool) {
	t.Helper()
	norm := func(h http.Header) map[string][]string {
		out := map[string][]string{}
		for k, vs := range h {
			lk := strings.ToLower(k)
			if skip[lk] {
				continue
			}
			cp := append([]string(nil), vs...)
			sort.Strings(cp)
			out[lk] = cp
		}
		return out
	}
	a, b := norm(stock), norm(native)
	// Diagnostics carry header NAMES and VALUE COUNTS only, never a value. The
	// compared set deliberately includes Authorization — a header whose equality is
	// exactly what a plan differential must check — so printing values here would
	// put credential material in test logs and break this helper's own contract.
	// A name plus a count is enough to locate the drift; the value never is.
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			t.Errorf("%s header %q present on the stock leg (%d value(s)) and absent on the native leg", what, k, len(av))
			continue
		}
		if strings.Join(av, "\x00") != strings.Join(bv, "\x00") {
			t.Errorf("%s header %q differs (redacted): stock=%d value(s) native=%d value(s)", what, k, len(av), len(bv))
		}
	}
	for k, bv := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("%s header %q present on the native leg (%d value(s)) and absent on the stock leg", what, k, len(bv))
		}
	}
}

// assertWireEquivalent is the STRICT request-plan comparison, on the bytes the
// provider actually received: method, request target, effective host, the header
// multimap, and the raw body. Bodies are compared byte-for-byte and reported by
// LENGTH only — a diagnostic must never carry prompt or credential material.
func assertWireEquivalent(t *testing.T, stock, native capturedUpstream) {
	t.Helper()
	if stock.method != native.method {
		t.Errorf("upstream method: stock=%q native=%q", stock.method, native.method)
	}
	if stock.target != native.target {
		t.Errorf("upstream target: stock=%q native=%q", stock.target, native.target)
	}
	if stock.host != native.host {
		t.Errorf("upstream host differs between engines")
	}
	if !bytes.Equal(stock.body, native.body) {
		t.Errorf("upstream body differs between engines: stock=%dB native=%dB", len(stock.body), len(native.body))
	}
	assertHeadersEquivalent(t, "upstream request", stock.headers, native.headers, volatileWireHeaders)
}
