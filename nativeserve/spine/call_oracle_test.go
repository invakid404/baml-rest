package spine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// TestUnaryExecutorSatisfiesOracleInterface pins that the production executor is the
// optional oracle-capable contract the ExecBridge-U1c standard composite drives, so a
// build-time break is caught here rather than at composite wiring.
func TestUnaryExecutorSatisfiesOracleInterface(t *testing.T) {
	e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
	if err != nil {
		t.Fatalf("newExec: %v", err)
	}
	var _ bamlutils.NativeSpineUnaryOracleExecutor = e
}

// okBuildBAMLRequest / okBAMLOnlyParse are non-nil placeholder oracle callbacks for the
// PRE-SOCKET decline table: every case below declines before admission ever calls them,
// so their bodies are never reached — they exist only so the nil-callback guards do not
// fire for cases meant to decline for a different reason.
func okBuildBAMLRequest(context.Context) (*llmhttp.Request, error) { return nil, nil }
func okBAMLOnlyParse(context.Context, string) ([]byte, error)      { return nil, nil }

// TestCallWithOracle_PreSocketDeclines drives every pre-socket decline arm of the
// oracle Call that does NOT need a real socket/FFI: a registry miss, the request-scoped
// exact-cohort declines (client registry / dynamic schema), the two mandatory
// oracle-callback guards, and a cancelled context. Each MUST be
// NativeSpineDeclinedPreSocket, open ZERO sockets, and carry the bounded stage/reason —
// the fallback-legal outcome the outer composite returns to BAML.
func TestCallWithOracle_PreSocketDeclines(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name      string
		ctx       context.Context
		inv       bamlutils.NativeStaticInvocation
		wantStage string
	}{
		{
			name: "unregistered method",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:           "NoSuchMethod",
				BuildBAMLRequest: okBuildBAMLRequest,
				BAMLOnlyParse:    okBAMLOnlyParse,
			},
			wantStage: "registry",
		},
		{
			name: "client registry override",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:                    jsonAliasMethod,
				HasClientRegistryOverride: true,
				BuildBAMLRequest:          okBuildBAMLRequest,
				BAMLOnlyParse:             okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "dynamic output schema",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:                 jsonAliasMethod,
				HasDynamicOutputSchema: true,
				BuildBAMLRequest:       okBuildBAMLRequest,
				BAMLOnlyParse:          okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "missing BAML plan closure",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:        jsonAliasMethod,
				BAMLOnlyParse: okBAMLOnlyParse,
			},
			wantStage: "admission",
		},
		{
			name: "missing BAML parse closure",
			ctx:  context.Background(),
			inv: bamlutils.NativeStaticInvocation{
				Method:           jsonAliasMethod,
				BuildBAMLRequest: okBuildBAMLRequest,
			},
			wantStage: "admission",
		},
		{
			name: "cancelled context",
			ctx:  cancelled,
			inv: bamlutils.NativeStaticInvocation{
				Method:           jsonAliasMethod,
				BuildBAMLRequest: okBuildBAMLRequest,
				BAMLOnlyParse:    okBAMLOnlyParse,
			},
			wantStage: "preflight",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
			if err != nil {
				t.Fatalf("newExec: %v", err)
			}
			var oracle bamlutils.NativeSpineUnaryOracleExecutor = e
			res := oracle.CallWithOracle(tc.ctx, tc.inv)
			if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
				t.Fatalf("disposition = %v (err %v), want declined_pre_socket", res.Disposition, res.Err)
			}
			if res.Err == nil {
				t.Fatal("declined result carries no typed error")
			}
			if res.Stage != tc.wantStage {
				t.Errorf("stage = %q, want %q (reason %q)", res.Stage, tc.wantStage, res.Reason)
			}
			// A declined oracle result MUST certify zero RoundTrips: no claim, no socket.
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Errorf("declined path opened a socket: sockets=%d claims=%d", snap.Sockets, snap.Claims)
			}
		})
	}
}

// TestCallWithOracle_ForwardsTruthfulNearMissFactsAndDeclines proves the P1 fix
// DISCRIMINATINGLY: oracleStaticInput FORWARDS the request's truthful selected-route facts,
// so a near-miss OUTSIDE the exact U1 population declines at its OWN mode/strategy gate,
// BEFORE the plan builder runs. This bites the pre-fix c1abcd6c0 code, which SYNTHESIZED
// fixed final/single-leaf/no-override/openai facts: under that code every mutation below was
// overwritten, so the request passed the mode/strategy gates, reached the live plan builder,
// and declined LATER (baml_plan_build_failed / nanollm_new_failed / a plan mismatch) — a
// different stage+reason, with the plan builder having RUN. Each case therefore asserts (a)
// the EXACT bounded stage+reason its gate emits, and (b) that the plan-builder spy was NEVER
// invoked. The positive control — a matching-plan exact-U1 request SERVES native — is the
// gated TestCallWithOracle_LiveOracle_NativeWinsAndParsesExactBytes (a live socket); every
// gate here is pre-nanollm-FFI, so this proof needs neither a socket nor a build tag.
func TestCallWithOracle_ForwardsTruthfulNearMissFactsAndDeclines(t *testing.T) {
	// A valid exact-U1 base (single default-client final call with the descriptor's bound
	// argument vector); each case flips ONE fact.
	values := jsonAliasValues(t)
	base := func() bamlutils.NativeStaticInvocation {
		return bamlutils.NativeStaticInvocation{
			Method:        jsonAliasMethod,
			Mode:          bamlutils.NativeStaticModeFinal,
			Provider:      "openai",
			SingleLeaf:    true,
			Values:        values,
			BAMLOnlyParse: okBAMLOnlyParse,
		}
	}
	// The bounded admission stage/reason tokens each near-miss gate emits (the exact wire
	// bytes; call.go forwards admission's StaticDecline.Stage/Reason unchanged). Hard-coded
	// rather than imported because they are the unexported admission constants — pinning the
	// literals is the point.
	cases := []struct {
		name       string
		mutfn      func(*bamlutils.NativeStaticInvocation)
		wantStage  string
		wantReason string
	}{
		{"call-with-raw", func(in *bamlutils.NativeStaticInvocation) { in.Raw = true }, "mode", "call_with_raw_unproven"},
		{"non-final mode", func(in *bamlutils.NativeStaticInvocation) { in.Mode = bamlutils.NativeStaticModeStream }, "mode", "mode_not_final"},
		{"not single leaf", func(in *bamlutils.NativeStaticInvocation) { in.SingleLeaf = false }, "strategy", "not_single_leaf"},
		{"fallback chain", func(in *bamlutils.NativeStaticInvocation) { in.HasFallbackChain = true }, "strategy", "fallback_chain"},
		{"round robin", func(in *bamlutils.NativeStaticInvocation) { in.HasRoundRobin = true }, "strategy", "round_robin_strategy"},
		{"retry override", func(in *bamlutils.NativeStaticInvocation) { in.HasRequestRetryOverride = true }, "strategy", "request_retry_override"},
		{"client override", func(in *bamlutils.NativeStaticInvocation) { in.ClientOverride = "SomeOtherClient" }, "strategy", "client_override_unproven"},
		{"provider not openai", func(in *bamlutils.NativeStaticInvocation) { in.Provider = "anthropic" }, "strategy", "provider_not_openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
			if err != nil {
				t.Fatalf("newExec: %v", err)
			}
			// A SPY plan builder: if the request reaches the live plan compare (as the
			// synthesizing pre-fix code did for these near-misses), this fires. It never
			// should for a near-miss, which declines at the mode/strategy gate above it.
			planBuilds := 0
			inv := base()
			inv.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
				planBuilds++
				return nil, nil
			}
			tc.mutfn(&inv)
			res := e.CallWithOracle(context.Background(), inv)
			if res.Disposition != bamlutils.NativeSpineDeclinedPreSocket {
				t.Fatalf("disposition = %v (stage %q reason %q), want declined_pre_socket — a near-miss must not claim on a plan match", res.Disposition, res.Stage, res.Reason)
			}
			if res.Stage != tc.wantStage || res.Reason != tc.wantReason {
				t.Errorf("declined at stage/reason %q/%q, want %q/%q — the near-miss must decline at ITS gate, not later at the plan builder", res.Stage, res.Reason, tc.wantStage, tc.wantReason)
			}
			if planBuilds != 0 {
				t.Errorf("plan builder ran %d times; a forwarded near-miss must decline BEFORE the plan builder (the pre-fix synthesized code reached it)", planBuilds)
			}
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Errorf("near-miss opened a socket: sockets=%d claims=%d", snap.Sockets, snap.Claims)
			}
		})
	}
}

// TestCallWithOracle_UnregisteredMethodTypedDecline pins that a registry miss surfaces
// the typed capability-decline (the same value Parse returns), never an ordinary error.
func TestCallWithOracle_UnregisteredMethodTypedDecline(t *testing.T) {
	e, err := newExec(t, jsonAliasProject(t), jsonAliasBinding())
	if err != nil {
		t.Fatalf("newExec: %v", err)
	}
	res := e.CallWithOracle(context.Background(), bamlutils.NativeStaticInvocation{
		Method:           "NoSuchMethod",
		BuildBAMLRequest: okBuildBAMLRequest,
		BAMLOnlyParse:    okBAMLOnlyParse,
	})
	var unsupported *bamlutils.NativeSpineUnsupportedMethodError
	if !errors.As(res.Err, &unsupported) {
		t.Fatalf("err = %v (%T), want *NativeSpineUnsupportedMethodError", res.Err, res.Err)
	}
}
