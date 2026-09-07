//go:build integration && nanollm_integration

package listserve

// Scope §3.A proof 5 — the NEGATIVE controls for the required scalar-LIST input
// widening, at the two layers where a near miss can arise once the cohort is wider:
//
//	population — a corpus one axis outside the cohort produces an EMPTY standard
//	             population, so the composite declines with a registry miss;
//	request    — an admitted list-input method with ONE request-scoped fact flipped
//	             declines PRE-CLAIM.
//
// Every row asserts the same three things: a pre-claim decline, ZERO native sockets
// and ZERO provider requests, and therefore ordinary BAML ownership of the request
// (the already-running generated orchestrator serves it — this harness asserts the
// decline that hands it back, which is the part the composite owns).
//
// The registry-level input fence (nullable list, nullable element, nested list
// including the alias-hidden spelling, list-of-class, list-of-enum) is proven
// exhaustively against the classifier itself in
// nativeserve/spine/executor_test.go's register:input-cohort rows; the population
// rows here are the end-to-end half showing those declines really do empty the
// SERVING population rather than merely returning an error somewhere.

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinelistfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// outOfCohortCorpus rewrites the fixture's function signature/return so the method
// is one axis outside the cohort while everything else — client, prompt shape,
// method name — is unchanged.
func outOfCohortCorpus(baseURL, signature, ret string) map[string]string {
	sources := nativespine.ListArgsFixtureSourcesAt(baseURL)
	sources["functions.baml"] = "function " + nativespine.ListArgsFixtureMethod + "(" + signature + ") -> " + ret + ` {
  client ListOracle
  prompt #"
    Summarize {{ topic }} as arbitrary JSON.
    Tags: {{ tags }}
    {{ ctx.output_format }}
  "#
}
`
	if ret != "JSON" {
		sources["types.baml"] = "type JSON = int | string | bool | JSON[] | map<string, JSON>\n" +
			"type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>\n"
	}
	return sources
}

// TestOutOfCohortCorporaProduceAnEmptyServingPopulation is the POPULATION half.
//
// The rows split by WHICH layer declines, and the split is load-bearing rather than
// cosmetic, because it decides what a deployment's generated registry would even
// contain:
//
//	sourceDeclined=false — the SOURCE classifier admits the method (so codegen emits
//	                       a binding for it) and the SERVING registry declines it.
//	                       The binding is registered and the population comes out
//	                       EMPTY.
//	sourceDeclined=true  — the source classifier declines the whole function, so it
//	                       is not in Project.Methods and codegen emits NO binding.
//	                       Registering one anyway would be a corrupt registry, which
//	                       the constructor correctly FAILS on; the honest deployment
//	                       shape is a project with no candidate at all.
func TestOutOfCohortCorporaProduceAnEmptyServingPopulation(t *testing.T) {
	cases := []struct {
		name           string
		signature      string
		ret            string
		sourceDeclined bool
	}{
		// Declined by the SERVING registry (the widening's own fence).
		{name: "nullable_list", signature: "topic: string, tags: string[]?", ret: "JSON"},
		{name: "nullable_list_element", signature: "topic: string, tags: (string?)[]", ret: "JSON"},
		// float[] is NOT in the cohort: the measured Debug-vs-Display list-render
		// residual (float_residual_test.go) keeps it out.
		{name: "float_list", signature: "topic: string, tags: float[]", ret: "JSON"},
		// The RETURN family is unchanged by this slice: a list-input method whose
		// return leaves the exact five-arm alias must still decline.
		{name: "changed_output_family", signature: "topic: string, tags: string[]", ret: "JsonValue"},
		{name: "scalar_output_family", signature: "topic: string, tags: string[]", ret: "string"},

		// Declined upstream by the SOURCE classifier.
		{name: "nested_list", signature: "topic: string, tags: string[][]", ret: "JSON", sourceDeclined: true},
		{name: "map_input", signature: "topic: string, tags: map<string, string>", ret: "JSON", sourceDeclined: true},
		{name: "union_input", signature: "topic: string, tags: string | int", ret: "JSON", sourceDeclined: true},
		{name: "media_input", signature: "topic: string, tags: image[]", ret: "JSON", sourceDeclined: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newJSONServer(t, `[1]`)
			proj, err := nativespine.BuildFromSource(outOfCohortCorpus(server.baseURL(), tc.signature, tc.ret))
			if err != nil {
				t.Fatalf("BuildFromSource: %v", err)
			}

			admitted := false
			for _, m := range proj.Methods {
				if m.Name == nativespine.ListArgsFixtureMethod {
					admitted = true
				}
			}
			if admitted == tc.sourceDeclined {
				t.Fatalf("source-classifier admission = %v, want %v — the row is filed under the wrong declining layer",
					admitted, !tc.sourceDeclined)
			}

			var candidates []spine.UnaryRegistration
			if !tc.sourceDeclined {
				candidates = []spine.UnaryRegistration{{
					Binding:     nativespinelistfixture.Binding(),
					BuildMethod: nativespinelistfixture.BuildMethod,
				}}
			}
			exec, err := spine.NewPopulationExecutor(proj, candidates, nil)
			if err != nil {
				t.Fatalf("the standard constructor must PERMIT an empty population, got: %v", err)
			}
			if got := exec.Methods(); len(got) != 0 {
				t.Fatalf("an out-of-cohort corpus was ADMITTED into the standard population: %v", got)
			}

			serve, err := standardspineoracle.NewStaticServeFromExecutor(prometheus.NewRegistry(), exec)
			if err != nil {
				t.Fatalf("NewStaticServeFromExecutor: %v", err)
			}
			leg := newBAMLLeg(t, server.baseURL())
			res := serve(context.Background(), unaryInv(t, leg, argRows()[0]))

			if res.Disposition != bamlutils.NativeStaticServeDeclined {
				t.Fatalf("disposition = %v, want a pre-socket DECLINE so BAML owns the request", res.Disposition)
			}
			if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Fatalf("a registry miss claimed or opened a socket: %+v", snap)
			}
			if got := server.hits.Load(); got != 0 {
				t.Fatalf("the provider saw %d request(s) on a declined row, want 0", got)
			}
		})
	}
}

// TestRequestScopedNearMissesDeclinePreClaim is the REQUEST half: the population is
// the admitted list-input method and the plan matches, so native WOULD win (that
// control is TestUnaryComposite_NativeWinsOnEveryScalarListRow). Each row flips
// exactly ONE request-scoped fact and must decline before any socket.
func TestRequestScopedNearMissesDeclinePreClaim(t *testing.T) {
	rewrites := func(string) bool { return true }
	cases := []struct {
		name   string
		mutate func(*bamlutils.NativeStaticInvocation)
	}{
		{"client_override_not_the_default", func(i *bamlutils.NativeStaticInvocation) { i.ClientOverride = "SomeOtherClient" }},
		{"fallback_chain", func(i *bamlutils.NativeStaticInvocation) { i.HasFallbackChain = true }},
		{"round_robin", func(i *bamlutils.NativeStaticInvocation) { i.HasRoundRobin = true }},
		{"request_retry_override", func(i *bamlutils.NativeStaticInvocation) { i.HasRequestRetryOverride = true }},
		{"not_single_leaf", func(i *bamlutils.NativeStaticInvocation) { i.SingleLeaf = false }},
		{"client_registry_override", func(i *bamlutils.NativeStaticInvocation) { i.HasClientRegistryOverride = true }},
		{"dynamic_output_schema", func(i *bamlutils.NativeStaticInvocation) { i.HasDynamicOutputSchema = true }},
		{"provider_not_openai", func(i *bamlutils.NativeStaticInvocation) { i.Provider = "anthropic" }},
		{"call_with_raw_mode", func(i *bamlutils.NativeStaticInvocation) { i.Raw = true }},
		{"rewrite_or_proxy_target", func(i *bamlutils.NativeStaticInvocation) { i.WouldRewriteOrProxy = rewrites }},
		{"no_baml_plan_builder", func(i *bamlutils.NativeStaticInvocation) { i.BuildBAMLRequest = nil }},
		{"baml_plan_build_fails", func(i *bamlutils.NativeStaticInvocation) {
			i.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) { return nil, errPlanUnavailable }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newJSONServer(t, `[1,"x",true]`)
			leg := newBAMLLeg(t, server.baseURL())
			serve, exec := unaryComposite(t, server.baseURL())

			inv := unaryInv(t, leg, argRows()[0])
			tc.mutate(&inv)
			res := serve(context.Background(), inv)

			if res.Disposition != bamlutils.NativeStaticServeDeclined {
				t.Fatalf("disposition = %v (stage=%q reason=%q), want a PRE-SOCKET decline", res.Disposition, res.Stage, res.Reason)
			}
			if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 || snap.Declines != 1 {
				t.Fatalf("counters = %+v, want sockets=0 claims=0 declines=1", snap)
			}
			if got := server.hits.Load(); got != 0 {
				t.Fatalf("the provider saw %d request(s) on a pre-socket decline, want 0", got)
			}
		})
	}
}

// TestPlanMismatchOnListValuesDeclinesPreClaim is the single-fact plan-compare
// control, and the one that is specific to THIS slice: the native plan is built
// from one row's list values while BAML's no-send plan is built from another's, so
// the two bodies differ only in the rendered lists. It must decline PRE-SOCKET.
//
// Without it, "the plan compare matched" in the positive tests could be true of a
// comparator that never looked at the list arguments at all.
func TestPlanMismatchOnListValuesDeclinesPreClaim(t *testing.T) {
	server := newJSONServer(t, `[1,"x",true]`)
	leg := newBAMLLeg(t, server.baseURL())
	serve, exec := unaryComposite(t, server.baseURL())

	native := argRows()[0]
	other := argRow{name: "other", topic: native.topic,
		tags: append(append([]string{}, native.tags...), "one-more-tag"),
		// Every non-list fact is held EQUAL, so the only difference between the two
		// plans is a list element.
		ratio: native.ratio, counts: native.counts, flags: native.flags}

	inv := unaryInv(t, leg, native)
	inv.BuildBAMLRequest = leg.buildRequestFn(t, other, false)

	res := serve(context.Background(), inv)
	if res.Disposition != bamlutils.NativeStaticServeDeclined {
		t.Fatalf("disposition = %v (stage=%q reason=%q); a plan that differs only in a LIST element must decline pre-socket",
			res.Disposition, res.Stage, res.Reason)
	}
	if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
		t.Fatalf("a plan mismatch claimed or opened a socket: %+v", snap)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("the provider saw %d request(s), want 0", got)
	}
	// NON-VACUITY: the two plans really do differ, and only in the rendered list.
	a := leg.buildRequest(t, native, false)
	b := leg.buildRequest(t, other, false)
	if a.Body == b.Body {
		t.Fatal("the two rows produced identical BAML plans; this control proves nothing")
	}
	if !strings.Contains(b.Body, "one-more-tag") {
		t.Fatal("the extra list element does not appear in BAML's rendered body; the rows are not differing where the test claims")
	}
}

// TestStreamNearMissesDeclinePreClaim is the streaming half of the request-scoped
// controls, run on BOTH public stream routes. A claimed stream has NO route back to
// BAML, so a near miss that declined late would be unrecoverable — which is why both
// stream routes carry the same fence as /call, and why both are exercised here.
func TestStreamNearMissesDeclinePreClaim(t *testing.T) {
	rewrites := func(string) bool { return true }
	cases := []struct {
		name   string
		mutate func(*bamlutils.NativeStaticStreamOracleInvocation)
	}{
		{"client_override_not_the_default", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.ClientOverride = "SomeOtherClient" }},
		{"fallback_chain", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.HasFallbackChain = true }},
		{"round_robin", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.HasRoundRobin = true }},
		{"request_retry_override", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.HasRequestRetryOverride = true }},
		{"not_single_leaf", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.SingleLeaf = false }},
		{"client_registry_override", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.HasClientRegistryOverride = true }},
		{"dynamic_output_schema", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.HasDynamicOutputSchema = true }},
		{"provider_not_openai", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.Provider = "anthropic" }},
		{"rewrite_or_proxy_target", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.WouldRewriteOrProxy = rewrites }},
		{"no_baml_stream_plan_builder", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.BuildBAMLStreamRequest = nil }},
		{"no_per_prefix_oracle", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.BAMLStreamParse = nil }},
		{"no_final_oracle", func(i *bamlutils.NativeStaticStreamOracleInvocation) { i.BAMLFinalParse = nil }},
	}
	// BOTH public stream routes. /stream-with-raw takes a different cadence branch
	// (NeedsRaw flows raw on ticks that release no structured partial), so a
	// route-specific regression in the pre-claim fence could otherwise let it open a
	// socket for a near miss while plain /stream stayed green.
	modes := []struct {
		name string
		mode bamlutils.NativeStreamMode
	}{
		{"stream", bamlutils.NativeStreamModeStream},
		{"stream_with_raw", bamlutils.NativeStreamModeStreamWithRaw},
	}
	for _, m := range modes {
		for _, tc := range cases {
			t.Run(m.name+"/"+tc.name, func(t *testing.T) {
				server := newSSEServer(t, listStreamCorpus())
				leg := newBAMLLeg(t, server.baseURL())
				serve, exec := streamComposite(t, server.baseURL())

				inv := streamInv(t, leg, argRows()[0], m.mode)
				tc.mutate(&inv)
				collector := &eventCollector{}
				res := serve(context.Background(), inv, collector.emit)

				if res.Disposition != bamlutils.NativeStaticStreamOracleDeclined {
					t.Fatalf("disposition = %v (stage=%q reason=%q), want a PRE-SOCKET decline", res.Disposition, res.Stage, res.Reason)
				}
				if snap := exec.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
					t.Fatalf("counters = %+v, want sockets=0 claims=0", snap)
				}
				if got := server.hits.Load(); got != 0 {
					t.Fatalf("the provider saw %d request(s) on a pre-socket decline, want 0", got)
				}
				if got := collector.snapshot(); len(got) != 0 {
					t.Fatalf("a declined stream emitted %d public event(s); a decline certifies ZERO events", len(got))
				}
			})
		}
	}
}

var errPlanUnavailable = errStr("the BAML plan could not be built")

type errStr string

func (e errStr) Error() string { return string(e) }
