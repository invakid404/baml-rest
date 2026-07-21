//go:build integration && nanollm_integration

package static

// De-BAML Slice 8B — end-to-end STATIC observe test through the INSTALLED observer.
//
// This drives the REAL, worker-installed observer factory (nativeserve.NewStaticObserve
// — the exact bamlutils.NativeStaticObserveFunc a native worker installs on every
// adapter) end to end, rather than calling admission.AdmitStatic directly. It builds
// the neutral NativeStaticInvocation the generated debaml_static.go helper
// (maybeObserveDeBAMLStaticFinal) builds — same field mapping, whose exact assignment
// set is separately pinned by the codegen helper-file test
// (adapters/common/codegen/codegen_debaml_static_test.go) and whose gate ordering +
// fact forwarding are pinned by the codegen /call seam shape test — then observes it
// through NewStaticObserve.
//
// It asserts, for the stock static corpus + one negative row per EXCLUDED route
// family (client override, call-with-raw, retry, fallback, round-robin, proxy/rewrite,
// stream, non-OpenAI):
//
//   - the observer's Disposition is ALWAYS NativeStaticDeclined (observe-only — BAML
//     serves every request, no matter the observation);
//   - a default single-leaf OpenAI call is observed as would_admit;
//   - every excluded family is a clean CLIENT-stage decline (never would_admit),
//     proving the narrow surface is enforced TRUTHFULLY through the installed observer;
//   - ZERO provider RoundTrips: the corpus client's base_url is the unroutable fence
//     host (…​.invalid), so a would_admit — which requires a SUCCESSFUL nanollm Prepare
//     AND a successful BAML Request.<Method> build — is only possible because BOTH are
//     no-send (a real socket would DNS-fail against .invalid and could never
//     would_admit). NewStaticObserve reaches only nanollm.Prepare; no RoundTrip / Do /
//     DoStream / ExactExecutor is on its path.
//
// A generated-adapter compile-through-the-router variant is NOT included because the
// only checked-in static project (static_oracle) cannot host a compilable generated
// adapter, for TWO inherent reasons (both in "DO NOT EDIT" generated fixtures, with no
// baml-cli or ctx-first static fixture available to regenerate a consistent one):
//   1. its baml_client's build_request/parse/parse_stream/build_request_stream
//      singletons are STRING-first, while codegen emits the ctx/adapter-first calling
//      convention (a thin ctx-first wrapper resolves this); and
//   2. its introspected package is internally inconsistent on the dynamic-types path —
//      `type TypeBuilder = <local>.TypeBuilder` but `NewTypeBuilder = bamlclient.NewTypeBuilder`
//      (which returns the STOCK type_builder) — so the generated createTypeBuilder /
//      applyDynamicTypes DEAD CODE (never reached for static, no dynamic types) cannot
//      type-check. A ctx-first wrapper compiled the ENTIRE generated adapter except
//      those three dead-code lines.
// The generated seam's shape (flag-off gate before the descriptor lookup, exact fact
// forwarding including retryPolicy != nil and raw) is therefore pinned by the codegen
// shape tests (adapters/common/codegen); this test pins the installed-observer RUNTIME
// behaviour those forwarded facts drive.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/nativeserve"
)

// staticInvocationLikeGenerated builds the NativeStaticInvocation the generated
// maybeObserveDeBAMLStaticFinal builds for a single default-client /call: final mode,
// descriptor provider, single leaf, no override/strategy/raw. mut applies a negative
// mutation (a specific excluded route family) before observation.
func staticInvocationLikeGenerated(
	fn promptdescriptor.Function,
	c staticCase,
	mut func(*bamlutils.NativeStaticInvocation),
) bamlutils.NativeStaticInvocation {
	inv := bamlutils.NativeStaticInvocation{
		Method:           fn.Method,
		Descriptor:       fn,
		Args:             c.args,
		ArgOrder:         argOrderOf(fn),
		Mode:             bamlutils.NativeStaticModeFinal,
		Provider:         fn.Provider,
		SingleLeaf:       true,
		BuildBAMLRequest: bamlPlanClosure(c.build),
	}
	if mut != nil {
		mut(&inv)
	}
	return inv
}

func TestStaticObserveEndToEnd(t *testing.T) {
	descriptors := buildDescriptors(t)
	observe, err := nativeserve.NewStaticObserve(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewStaticObserve: %v", err)
	}
	ctx := context.Background()

	base := staticCases()[0] // StaticCompletion/ascii
	fn, ok := descriptors[base.fn]
	if !ok {
		t.Fatalf("no descriptor for %q", base.fn)
	}

	// --- Positive: a default single-leaf OpenAI call would-admit (and, by the
	// unroutable-base argument above, did so with ZERO provider RoundTrips). --------
	got := observe(ctx, staticInvocationLikeGenerated(fn, base, nil))
	if got.Disposition != bamlutils.NativeStaticDeclined {
		t.Errorf("observe-only: Disposition = %v, want NativeStaticDeclined", got.Disposition)
	}
	if got.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		t.Fatalf("default OpenAI call: Observation = %q (family=%q reason=%q), want would_admit",
			got.Observation, got.Family, got.Reason)
	}

	// --- Negative: every EXCLUDED route family is a clean CLIENT decline (never
	// would_admit), observed through the installed observer. --------------------------
	negatives := []struct {
		name string
		mut  func(*bamlutils.NativeStaticInvocation)
	}{
		{"client override", func(inv *bamlutils.NativeStaticInvocation) { inv.ClientOverride = "OtherClient" }},
		{"call-with-raw", func(inv *bamlutils.NativeStaticInvocation) { inv.Raw = true }},
		// Retry: the generated seam forwards HasRequestRetryOverride = (retryPolicy != nil),
		// where retryPolicy is the EFFECTIVE policy ResolveStrategyAwareRetryPolicy resolves
		// from the per-request override, the runtime client-registry policy, OR the static
		// introspected client policy. Both the static-introspected and registry cases reach
		// the observer as HasRequestRetryOverride=true and MUST decline (they are
		// indistinguishable at the neutral boundary — the router collapses them into one
		// resolved policy); assert both.
		{"static introspected retry policy", func(inv *bamlutils.NativeStaticInvocation) { inv.HasRequestRetryOverride = true }},
		{"registry retry policy", func(inv *bamlutils.NativeStaticInvocation) { inv.HasRequestRetryOverride = true }},
		{"fallback chain", func(inv *bamlutils.NativeStaticInvocation) { inv.HasFallbackChain = true }},
		{"round robin", func(inv *bamlutils.NativeStaticInvocation) { inv.HasRoundRobin = true }},
		{"proxy/rewrite", func(inv *bamlutils.NativeStaticInvocation) {
			inv.WouldRewriteOrProxy = func(string) bool { return true }
		}},
		{"stream mode", func(inv *bamlutils.NativeStaticInvocation) { inv.Mode = bamlutils.NativeStaticModeStream }},
		{"not single leaf", func(inv *bamlutils.NativeStaticInvocation) { inv.SingleLeaf = false }},
		{"non-openai provider", func(inv *bamlutils.NativeStaticInvocation) { inv.Provider = "anthropic" }},
	}
	for _, n := range negatives {
		t.Run(n.name, func(t *testing.T) {
			res := observe(ctx, staticInvocationLikeGenerated(fn, base, n.mut))
			if res.Disposition != bamlutils.NativeStaticDeclined {
				t.Errorf("Disposition = %v, want NativeStaticDeclined", res.Disposition)
			}
			if res.Observation != bamlutils.NativeStaticObserveDecline {
				t.Fatalf("Observation = %q (family=%q reason=%q), want a decline",
					res.Observation, res.Family, res.Reason)
			}
			if res.Family != bamlutils.NativeStaticFamilyClient {
				t.Errorf("Family = %q, want client (stage=%q reason=%q)", res.Family, res.Stage, res.Reason)
			}
		})
	}

	// --- Parse-only observation through the installed observer would-admits the
	// Return-Bundle final surface (and always declines). ------------------------------
	parseInv := bamlutils.NativeStaticInvocation{
		Method:     fn.Method,
		Descriptor: fn,
		Mode:       bamlutils.NativeStaticModeParseOnly,
		Provider:   fn.Provider,
		SingleLeaf: true,
	}
	pres := observe(ctx, parseInv)
	if pres.Disposition != bamlutils.NativeStaticDeclined {
		t.Errorf("parse-only: Disposition = %v, want NativeStaticDeclined", pres.Disposition)
	}
	if pres.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		t.Errorf("parse-only default: Observation = %q (family=%q reason=%q), want would_admit",
			pres.Observation, pres.Family, pres.Reason)
	}
}
