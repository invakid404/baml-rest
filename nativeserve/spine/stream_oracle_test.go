package spine_test

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// stream_oracle_test.go is the ExecBridge-U1s PRE-SOCKET proof for StreamWithOracle. Every
// row here is decided BEFORE any transport could exist — no nanollm engine, no socket, no
// build tag — which is itself part of what it proves: a near-miss on the default-serve
// stream lane must decline with zero provider sockets and zero public events, because a
// claimed stream has no route back.
//
// The post-claim drift/fault matrix (which needs a real claim) lives in the gated
// stream_oracle_integration_test.go; the pure decision matrix it drives lives in
// nativeserve/streamoracle.

// newStreamOracleExec builds the population-filtered stream executor the standard composite
// constructs, over the admitted five-arm JSON alias project.
func newStreamOracleExec(t *testing.T) *spine.StreamExecutor {
	t.Helper()
	e, err := spine.NewPopulationStreamExecutor(jsonAliasProject(t), []spine.StreamRegistration{streamReg()}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor: %v", err)
	}
	return e
}

// The non-nil placeholder oracle callbacks for the decline table: every case below declines
// before admission ever calls them, so their bodies are unreachable. They exist only so the
// nil-callback guards do not fire for a case meant to decline for a different reason.
func okBuildBAMLStreamRequest(context.Context) (*llmhttp.Request, error) { return nil, nil }
func okBAMLStreamParse(context.Context, string) (bamlutils.BAMLStreamPrefixResult, error) {
	return bamlutils.BAMLStreamPrefixResult{}, nil
}
func okBAMLFinalParse(context.Context, string) (any, error) { return nil, nil }
func okDecode([]byte) (any, error)                          { return nil, nil }

// okEmit is a non-nil emit sink that FAILS the test if it is ever called: a pre-socket
// decline must deliver no public event at all.
func okEmit(t *testing.T) bamlutils.NativeSpineStreamEmit {
	t.Helper()
	return func(bamlutils.NativeSpineStreamEvent) error {
		t.Error("a pre-socket decline delivered a public stream event")
		return nil
	}
}

// oracleStreamInv is a valid exact-cohort invocation; each case flips ONE fact.
func oracleStreamInv(t *testing.T) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	return bamlutils.NativeStaticStreamOracleInvocation{
		Method:                    jsonAliasMethod,
		Mode:                      bamlutils.NativeStreamModeStream,
		Provider:                  "openai",
		SingleLeaf:                true,
		Values:                    jsonAliasValues(t),
		BuildBAMLStreamRequest:    okBuildBAMLStreamRequest,
		BAMLStreamParse:           okBAMLStreamParse,
		BAMLFinalParse:            okBAMLFinalParse,
		DecodeNativeStreamPartial: okDecode,
		DecodeNativeStreamFinal:   okDecode,
	}
}

// TestStreamExecutorSatisfiesStreamOracleInterface pins that the production stream executor
// is the optional oracle-capable contract the standard composite drives, so a build-time
// break is caught here rather than at composite wiring.
func TestStreamExecutorSatisfiesStreamOracleInterface(t *testing.T) {
	var _ bamlutils.NativeSpineStreamOracleExecutor = newStreamOracleExec(t)
}

// TestStreamWithOracle_PreSocketDeclines drives every pre-socket decline arm that does not
// need a real socket/FFI: a registry miss, the request-scoped exact-cohort declines, a
// non-stream mode, a cancelled context, a nil emit sink, and each of the FIVE mandatory
// oracle/decoder callback guards. Each MUST be a pre-socket decline with zero sockets, zero
// claims and zero public events — the only fallback-legal outcome on this lane.
func TestStreamWithOracle_PreSocketDeclines(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name       string
		ctx        context.Context
		nilEmit    bool
		mutate     func(*bamlutils.NativeStaticStreamOracleInvocation)
		wantStage  string
		wantReason string
	}{
		{
			name:       "unregistered method",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.Method = "NoSuchMethod" },
			wantStage:  "registry",
			wantReason: "method_not_registered",
		},
		{
			name:       "nil emit callback",
			nilEmit:    true,
			mutate:     func(*bamlutils.NativeStaticStreamOracleInvocation) {},
			wantStage:  "preflight",
			wantReason: "nil_emit_callback",
		},
		{
			name:       "cancelled context",
			ctx:        cancelled,
			mutate:     func(*bamlutils.NativeStaticStreamOracleInvocation) {},
			wantStage:  "preflight",
			wantReason: "context_cancelled",
		},
		{
			name:       "not a stream mode",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.Mode = "" },
			wantStage:  "preflight",
			wantReason: "mode_not_stream",
		},
		{
			name:       "client registry override",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.HasClientRegistryOverride = true },
			wantStage:  "admission",
			wantReason: "client_registry_present",
		},
		{
			name:       "dynamic output schema",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.HasDynamicOutputSchema = true },
			wantStage:  "admission",
			wantReason: "dynamic_output_schema_present",
		},
		{
			name:       "missing BAML stream plan closure",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.BuildBAMLStreamRequest = nil },
			wantStage:  "admission",
			wantReason: "no_baml_stream_plan_closure",
		},
		{
			name:       "missing BAML per-prefix parse closure",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.BAMLStreamParse = nil },
			wantStage:  "admission",
			wantReason: "no_baml_stream_parse_closure",
		},
		{
			name:       "missing BAML final parse closure",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.BAMLFinalParse = nil },
			wantStage:  "admission",
			wantReason: "no_baml_stream_parse_closure",
		},
		{
			name:       "missing native partial decoder",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.DecodeNativeStreamPartial = nil },
			wantStage:  "admission",
			wantReason: "no_standard_stream_decoder",
		},
		{
			name:       "missing native final decoder",
			mutate:     func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.DecodeNativeStreamFinal = nil },
			wantStage:  "admission",
			wantReason: "no_standard_stream_decoder",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newStreamOracleExec(t)
			inv := oracleStreamInv(t)
			tc.mutate(&inv)
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var emit bamlutils.NativeSpineStreamEmit
			if !tc.nilEmit {
				emit = okEmit(t)
			}
			res := e.StreamWithOracle(ctx, inv, emit)
			if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
				t.Fatalf("disposition = %v (err %v), want declined_pre_socket", res.Disposition, res.Err)
			}
			if res.Err == nil {
				t.Fatal("declined result carries no typed error")
			}
			if res.Stage != tc.wantStage || res.Reason != tc.wantReason {
				t.Errorf("declined at stage/reason %q/%q, want %q/%q", res.Stage, res.Reason, tc.wantStage, tc.wantReason)
			}
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Errorf("declined path claimed/opened a socket: sockets=%d claims=%d", snap.Sockets, snap.Claims)
			}
			// A decline carries no winner and no post-claim evidence.
			if res.WinnerEngine != "" || res.Observations.SocketOpened || res.Observations.PrefixComparisons != 0 {
				t.Errorf("declined result carries post-claim evidence: winner=%q obs=%+v", res.WinnerEngine, res.Observations)
			}
		})
	}
}

// TestStreamWithOracle_ForwardsTruthfulNearMissFactsAndDeclines is the DISCRIMINATING proof
// that oracleStaticStreamInput FORWARDS the request's truthful selected-route facts rather
// than synthesizing fixed ones: a near-miss OUTSIDE the exact population must decline at its
// OWN mode/strategy gate, BEFORE the live plan builder runs.
//
// It bites the plausible wrong implementation — copying the native-only lane's
// staticStreamInput, which hard-codes SingleLeaf:true, no retry override, an empty client
// override and the descriptor's provider. Under that code every mutation below would be
// overwritten, the request would sail past the strategy gates into the live plan builder,
// and it would decline LATER with a different stage/reason. Each case therefore asserts BOTH
// the exact bounded stage+reason its gate emits AND that the plan-builder spy never ran.
//
// A claimed stream cannot be un-claimed, so "declines at its own gate, before the builder"
// is not a nicety here: it is the difference between BAML serving the request and an
// unproven shape owning an irreversible socket.
func TestStreamWithOracle_ForwardsTruthfulNearMissFactsAndDeclines(t *testing.T) {
	// The bounded admission stage/reason tokens each near-miss gate emits, hard-coded
	// rather than imported because they are unexported admission constants — pinning the
	// literal wire bytes is the point.
	cases := []struct {
		name       string
		mutate     func(*bamlutils.NativeStaticStreamOracleInvocation)
		wantStage  string
		wantReason string
	}{
		{"not single leaf", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.SingleLeaf = false }, "strategy", "not_single_leaf"},
		{"fallback chain", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.HasFallbackChain = true }, "strategy", "fallback_chain"},
		{"round robin", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.HasRoundRobin = true }, "strategy", "round_robin_strategy"},
		{"retry override", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.HasRequestRetryOverride = true }, "strategy", "request_retry_override"},
		{"client override", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.ClientOverride = "SomeOtherClient" }, "strategy", "client_override_unproven"},
		{"provider not openai", func(in *bamlutils.NativeStaticStreamOracleInvocation) { in.Provider = "anthropic" }, "strategy", "provider_not_openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newStreamOracleExec(t)
			planBuilds := 0
			inv := oracleStreamInv(t)
			inv.BuildBAMLStreamRequest = func(context.Context) (*llmhttp.Request, error) {
				planBuilds++
				return nil, nil
			}
			tc.mutate(&inv)
			res := e.StreamWithOracle(context.Background(), inv, okEmit(t))
			if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
				t.Fatalf("disposition = %v (stage %q reason %q), want declined_pre_socket — a near-miss must never claim a stream", res.Disposition, res.Stage, res.Reason)
			}
			if res.Stage != tc.wantStage || res.Reason != tc.wantReason {
				t.Errorf("declined at stage/reason %q/%q, want %q/%q — the near-miss must decline at ITS gate, not later at the plan builder", res.Stage, res.Reason, tc.wantStage, tc.wantReason)
			}
			if planBuilds != 0 {
				t.Errorf("the live BAML plan builder ran %d time(s) for a near-miss; it must decline at the strategy gate above it", planBuilds)
			}
			if snap := e.Metrics().Snapshot(); snap.Sockets != 0 || snap.Claims != 0 {
				t.Errorf("a near-miss claimed/opened a socket: sockets=%d claims=%d", snap.Sockets, snap.Claims)
			}
		})
	}
}

// TestStreamWithOracle_RewriteProxyPredicateIsMandatory pins the fail-closed rewrite/proxy
// gate on the oracle lane: the invocation's predicate is FORWARDED, so a request whose
// effective client would rewrite or proxy the send target declines pre-claim.
//
// The nil-predicate case is covered by the executor's own default (llmhttp.DefaultClient),
// so what this proves is the FORWARDING: a caller-supplied positive verdict must win over
// that default rather than being ignored.
func TestStreamWithOracle_RewriteProxyPredicateIsMandatory(t *testing.T) {
	e := newStreamOracleExec(t)
	inv := oracleStreamInv(t)
	asked := 0
	inv.WouldRewriteOrProxy = func(string) bool {
		asked++
		return true
	}
	planBuilds := 0
	inv.BuildBAMLStreamRequest = func(context.Context) (*llmhttp.Request, error) {
		planBuilds++
		return nil, nil
	}
	res := e.StreamWithOracle(context.Background(), inv, okEmit(t))
	if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
		t.Fatalf("disposition = %v, want declined_pre_socket for a rewriting/proxying send target", res.Disposition)
	}
	if res.Stage != "strategy" || res.Reason != "url_rewrite_or_proxy" {
		t.Errorf("declined at stage/reason %q/%q, want strategy/url_rewrite_or_proxy", res.Stage, res.Reason)
	}
	if asked == 0 {
		t.Error("the invocation's rewrite/proxy predicate was never consulted; the executor default silently replaced it")
	}
	if planBuilds != 0 {
		t.Errorf("the live plan builder ran %d time(s) after the rewrite/proxy gate should have declined", planBuilds)
	}
}

// TestStreamWithOracle_NilPlanBuilderDeclinesWithoutClaiming pins that a missing live plan
// oracle is a DECLINE, not a claim served without its safety rail. The guard sits before
// admission, so it also proves no engine was constructed.
func TestStreamWithOracle_NilPlanBuilderDeclinesWithoutClaiming(t *testing.T) {
	e := newStreamOracleExec(t)
	inv := oracleStreamInv(t)
	inv.BuildBAMLStreamRequest = nil
	res := e.StreamWithOracle(context.Background(), inv, okEmit(t))
	if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
		t.Fatalf("disposition = %v, want declined_pre_socket", res.Disposition)
	}
	if res.Observations.PlanCompareRan || res.Observations.PlanMatched {
		t.Errorf("observations claim a plan compare ran without a builder: %+v", res.Observations)
	}
}

// TestStreamWithOracleIsNotTheNativeOnlyLane pins that the two lanes are genuinely separate
// policies over ONE registry: the frozen Stream declines a request the oracle lane would
// admit for a DIFFERENT reason (it reads its facts off the adapter, not the invocation), and
// neither leaks the other's evidence type. It is the compile-and-behaviour guard against a
// future "just call Stream from StreamWithOracle" simplification, which would silently drop
// the live plan compare and the per-prefix oracle.
func TestStreamWithOracleIsNotTheNativeOnlyLane(t *testing.T) {
	e := newStreamOracleExec(t)
	// The native-only lane reads the public mode off the ADAPTER; a plain context carries
	// none, so it declines at the mode gate.
	native := e.Stream(context.Background(), jsonAliasMethod, map[string]any{"topic": "x"}, okEmit(t))
	if native.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket || native.Reason != "mode_not_stream" {
		t.Fatalf("native-only Stream: disposition=%v reason=%q, want a pre-socket mode decline on a plain context", native.Disposition, native.Reason)
	}
	// The oracle lane reads the mode off the INVOCATION, so the same plain context reaches
	// admission — a different gate entirely.
	inv := oracleStreamInv(t)
	inv.SingleLeaf = false
	oracle := e.StreamWithOracle(context.Background(), inv, okEmit(t))
	if oracle.Reason == "mode_not_stream" {
		t.Error("the oracle lane re-derived its mode from the adapter; it must use the truthful invocation fact")
	}
	if oracle.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
		t.Fatalf("oracle lane disposition = %v, want declined_pre_socket", oracle.Disposition)
	}
}
