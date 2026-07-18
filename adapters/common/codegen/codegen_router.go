package codegen

import (
	"github.com/dave/jennifer/jen"
)

// emitRouter emits the public per-method router function. The
// router selects between BuildRequest, the streaming-bridged call
// path, and the legacy CallStream+OnTick fallthrough based on the
// adapter's StreamMode and the available BuildRequest surfaces
// (the BuildRequest route is unconditional as of #537); it then
// forwards to the corresponding generated impl
// (<method>_buildRequest / <method>_buildCallRequest / <method>_noRaw
// / <method>_full).
func (me *methodEmitter) emitRouter() {
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")

	hasBuildRequest := g.intro.StreamRequest != nil
	hasCallBuildRequest := g.intro.Request != nil

	// Generate the public router function that dispatches based on StreamMode()
	// Creates the output channel and passes it to the inner implementation.
	//
	// When a BuildRequest surface is available it takes priority for both
	// call and stream modes (the route is unconditional as of #537). Falls
	// back to the legacy paths for unsupported/empty providers or BAML
	// versions that expose neither Request nor StreamRequest.
	routerBody := []jen.Code{
		jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
		jen.Var().Id("err").Error(),
		jen.Id("mode").Op(":=").Id("adapter").Dot("StreamMode").Call(),
	}
	// Seed __retryClient and __effective from ResolvePrimaryClient
	// and leave __rrInfo nil. This is the legacy-equivalent shape:
	// primary override applied, no RR unwrap, no coordinator advance,
	// no RR metadata. supportsWithClient adapters upgrade __effective
	// via ResolveEffectiveClient below; older adapters keep this
	// baseline so BAML's own strategy rotation owns RR.
	//
	// __retryClient preserves the pre-unwrap client name (the strategy
	// wrapper itself, or the function's primary-override target).
	// __effective may later become an RR-selected leaf, but
	// retry-policy resolution must be wrapper-first to honor BAML's
	// LLMStrategyProvider::WithRetryPolicy semantics — when an RR or
	// fallback wrapper carries its own retry_policy, BAML applies it
	// AROUND the strategy iteration rather than per-leaf.
	routerBody = append(routerBody,
		jen.Id("__retryClient").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolvePrimaryClient").Call(
			jen.Id("adapter"),
			jen.Qual(g.pkgs.IntrospectedPkg, "FunctionClient").Index(jen.Lit(me.methodName)),
		),
		jen.Id("__effective").Op(":=").Id("__retryClient"),
		jen.Var().Id("__rrInfo").Op("*").Qual(g.pkgs.InterfacesPkg, "RoundRobinInfo"),
	)
	if g.supportsWithClient {
		// Full RR resolution upgrade: apply the runtime primary
		// override, unwrap baml-roundrobin wrappers, and advance the
		// coordinator (or the worker-installed RemoteAdvancer) for the
		// leaf selection. Unconditional on supportsWithClient adapters
		// — the BuildRequest route is always attempted (#537), so RR
		// centralization always runs for modern adapters.
		routerBody = append(routerBody,
			jen.List(jen.Id("__rrEffective"), jen.Id("__rrInfoUpgrade"), jen.Id("__rrErr")).Op(":=").
				Qual(g.pkgs.BuildRequestPkg, "ResolveEffectiveClient").Call(
				jen.Id("adapter"),
				jen.Qual(g.pkgs.IntrospectedPkg, "FunctionClient").Index(jen.Lit(me.methodName)),
				jen.Qual(g.pkgs.IntrospectedPkg, "FallbackChains"),
				jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
				jen.Qual(g.pkgs.IntrospectedPkg, "RoundRobinCoordinator"),
			),
			jen.If(jen.Id("__rrErr").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("__rrErr")),
			),
			jen.Id("__effective").Op("=").Id("__rrEffective"),
			jen.Id("__rrInfo").Op("=").Id("__rrInfoUpgrade"),
		)
	}
	routerBody = append(routerBody,
		jen.Id("__reg").Op(":=").Id("adapter").Dot("OriginalClientRegistry").Call(),
	)

	// Helper to generate the common retry policy resolution + dispatch call
	// for both single-provider and fallback-chain paths.
	//
	// Wrapper-first, leaf-fallback. BAML's LLMStrategyProvider
	// implements WithRetryPolicy
	// (engine/baml-runtime/src/internal/llm_client/strategy/mod.rs):
	// when the strategy wrapper has its own retry_policy, BAML wraps
	// the whole strategy iteration in ExecutionScope::Retry; only
	// when the wrapper has none does the selected leaf's retry_policy
	// apply. ResolveStrategyAwareRetryPolicy honors that order: it
	// consults __retryClient (pre-unwrap wrapper / primary override)
	// first and falls back to __effective (post-RR leaf) only when
	// the wrapper has no policy and the unwrap actually changed the
	// client name. The per-request __baml_options__.retry override
	// remains highest priority via the underlying ResolveRetryPolicy.
	resolveRetryPolicy := func() jen.Code {
		return jen.Id("retryPolicy").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveStrategyAwareRetryPolicy").Call(
			jen.Id("adapter"),
			jen.Id("__retryClient"),
			jen.Id("__effective"),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__retryClient")),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
			jen.Qual(g.pkgs.IntrospectedPkg, "RetryPolicies"),
		)
	}

	// fallbackChainConsumeCond builds the condition guarding a fallback-
	// chain landing block: a non-nil resolution with a non-empty chain.
	// Reads off __resolution (the typed FallbackChainResolution from
	// ResolveFallbackChainPlanForClient); the nil-check guards against
	// the "not a fallback strategy" return shape.
	fallbackChainConsumeCond := func() jen.Code {
		return jen.Id("__resolution").Op("!=").Nil().
			Op("&&").Len(jen.Id("__resolution").Dot("Chain")).Op(">").Lit(0)
	}

	// callFallbackDispatch emits the non-streaming Request dispatch for an
	// already-resolved fallback chain: build the plan with
	// BuildRequestAPIRequest, copy the top-level RR metadata, and forward to
	// the call-request impl with the resolver's per-child Chain / Providers /
	// LegacyChildren / Targets / NestedRoundRobin threaded through. Assumes
	// retryPolicy and __resolution are already in scope. Shared by the
	// no-bridge call block (the sole-landing-spot case) and the bridge's
	// call-supported preference branch (#543); both consume a SINGLE
	// __resolution, so centralized RR is selected exactly once and never
	// double-advances the advancer.
	callFallbackDispatch := func() []jen.Code {
		return []jen.Code{
			jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildFallbackChainPlanFromResolution").Call(
				jen.Id("__effective"),
				jen.Id("__resolution"),
				jen.Id("retryPolicy"),
				jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIRequest"),
			),
			jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
			jen.Id("err").Op("=").Id(me.buildCallRequestMethodName).Call(
				jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
				jen.Lit(""), jen.Id("retryPolicy"),
				jen.Id("__resolution").Dot("Chain"),
				jen.Id("__resolution").Dot("Providers"),
				jen.Id("__resolution").Dot("LegacyChildren"),
				jen.Id("__resolution").Dot("Targets"),
				jen.Id("__resolution").Dot("NestedRoundRobin"),
				jen.Id("__planned"),
				jen.Lit(""),
			),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
			jen.Return(jen.Id("out"), jen.Nil()),
		}
	}

	// Non-streaming BuildRequest path for /call and /call-with-raw.
	// Uses Request (not StreamRequest) to build non-streaming HTTP requests.
	// Checked before the streaming path since it's more efficient for call modes.
	//
	// When the StreamRequest bridge exists (hasBuildRequest=true) the
	// call block emits only the single-provider arm; the bridge below owns
	// the SOLE call-mode fallback-chain resolution and, from that one
	// resolution, dispatches the chain via the non-streaming Request API
	// when every provider is call-supported, otherwise via the StreamRequest
	// bridge (#543). Two reasons the single resolver lives in the bridge:
	//
	//   1. Single resolver per request. If both the call block and the
	//      bridge called the typed resolver, a mixed chain (centralised RR
	//      child plus any call-legacy sibling) would advance the SharedState
	//      advancer in the call block and again in the bridge after falling
	//      through — burning two idempotency-cache slots and skewing
	//      rotation for that request shape. Choosing Request vs StreamRequest
	//      from the already-resolved __resolution (its LegacyChildren +
	//      Providers) reuses that single resolution — including its
	//      centralised RR Targets / NestedRoundRobin — so RR is never
	//      selected a second time.
	//   2. Stream-side support is a superset of call-side support
	//      (IsProviderSupported ⊇ IsCallProviderSupported), so the bridge's
	//      stream-side resolver accepts every chain the call block could have
	//      driven directly PLUS call-unsupported chains the Request API must
	//      decline. The call-supported subset dispatches unary via Request;
	//      the remainder bridges through StreamRequest with SSE accumulation.
	//
	// When no bridge exists (hasBuildRequest=false) the call block is
	// the sole BuildRequest landing spot for call modes and emits the
	// fallback-chain arm itself.
	if hasCallBuildRequest {
		callBlockBody := []jen.Code{
			// Single-provider path — keyed off the effective (post-RR)
			// client. ResolveClientProvider consults the runtime
			// registry for a per-client provider override before
			// falling back to the introspected default.
			jen.Id("provider").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveClientProvider").Call(
				jen.Id("__reg"),
				jen.Id("__effective"),
				jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
			),
			jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(g.pkgs.BuildRequestPkg, "IsCallProviderSupported").Call(jen.Id("provider"))).Block(
				resolveRetryPolicy(),
				jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
					jen.Id("__effective"),
					jen.Id("provider"),
					jen.Id("retryPolicy"),
					jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIRequest"),
				),
				jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
				jen.Id("err").Op("=").Id(me.buildCallRequestMethodName).Call(
					jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
					jen.Id("provider"), jen.Id("retryPolicy"),
					jen.Nil(), jen.Nil(),
					jen.Nil(),
					jen.Nil(), jen.Nil(),
					jen.Id("__planned"),
					jen.Id("__effective"),
				),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				),
				jen.Return(jen.Id("out"), jen.Nil()),
			),
		}
		if !hasBuildRequest {
			// No bridge below — the call block must resolve and consume
			// fallback chains itself. The typed resolver threads its
			// per-child Targets + NestedRoundRobin onto the generated
			// config so that centrally-unwrapped RR fallback children
			// dispatch to the selected leaf via the BuildRequest path
			// while rotation stays cross-worker. PreferAdvancer mirrors
			// ResolveEffectiveClient's per-request RemoteAdvancer
			// preference so the SharedState idempotency key threads
			// identically for top-level and nested RR. Hard errors from
			// the resolver (cycle / empty children / advancer transport /
			// duplicate RR child) propagate request-fatal — matching
			// top-level RR semantics — rather than silently degrading.
			callBlockBody = append(callBlockBody,
				jen.List(jen.Id("__resolution"), jen.Id("__fbErr")).Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveFallbackChainPlanForClient").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(g.pkgs.IntrospectedPkg, "FallbackChains"),
					jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
					jen.Qual(g.pkgs.BuildRequestPkg, "IsCallProviderSupported"),
					jen.Qual(g.pkgs.BuildRequestPkg, "PreferAdvancer").Call(
						jen.Id("adapter"),
						jen.Qual(g.pkgs.IntrospectedPkg, "RoundRobinCoordinator"),
					),
				),
				jen.If(jen.Id("__fbErr").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("__fbErr")),
				),
				jen.If(fallbackChainConsumeCond()).Block(
					append([]jen.Code{resolveRetryPolicy()}, callFallbackDispatch()...)...,
				),
			)
		}
		routerBody = append(routerBody,
			jen.Comment("Try non-streaming BuildRequest path for /call and /call-with-raw"),
			jen.If(
				jen.Qual(g.pkgs.IntrospectedPkg, "Request").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCall").
					Op("||").Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCallWithRaw")),
			).Block(callBlockBody...),
		)
	}

	// Streaming BuildRequest path for /stream and /stream-with-raw.
	// Explicitly gated to streaming modes; a separate bridge block below
	// handles /call and /call-with-raw via stream accumulation when the
	// non-streaming Request API declined.
	if hasBuildRequest {
		routerBody = append(routerBody,
			jen.Comment("Try streaming BuildRequest path for /stream and /stream-with-raw"),
			jen.If(
				jen.Qual(g.pkgs.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeStream").
					Op("||").Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeStreamWithRaw")),
			).Block(
				// Single-provider path keyed off the effective client.
				jen.Id("provider").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(g.pkgs.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("provider"),
						jen.Id("retryPolicy"),
						jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("provider"), jen.Id("retryPolicy"),
						jen.Nil(), jen.Nil(),
						jen.Nil(),
						jen.Nil(), jen.Nil(),
						jen.Id("__planned"),
						jen.Id("__effective"),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
				// Fallback chain path. Mixed chains (with any legacy
				// children) route through the BuildRequest path — the
				// orchestrator dispatches legacy children to the
				// generated legacyStreamChildFn. Centralized RR fallback
				// children (an immediate RR child whose selected leaf is
				// BR-supported) reach BuildRequest with the leaf in
				// __resolution.Targets so dispatch rotates cross-worker
				// via the same advancer top-level RR uses; ineligible RR
				// children stay on the legacy callback path. Hard
				// resolver errors propagate request-fatal.
				jen.List(jen.Id("__resolution"), jen.Id("__fbErr")).Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveFallbackChainPlanForClient").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(g.pkgs.IntrospectedPkg, "FallbackChains"),
					jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
					jen.Qual(g.pkgs.BuildRequestPkg, "IsProviderSupported"),
					jen.Qual(g.pkgs.BuildRequestPkg, "PreferAdvancer").Call(
						jen.Id("adapter"),
						jen.Qual(g.pkgs.IntrospectedPkg, "RoundRobinCoordinator"),
					),
				),
				jen.If(jen.Id("__fbErr").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("__fbErr")),
				),
				jen.If(fallbackChainConsumeCond()).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildFallbackChainPlanFromResolution").Call(
						jen.Id("__effective"),
						jen.Id("__resolution"),
						jen.Id("retryPolicy"),
						jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Lit(""), jen.Id("retryPolicy"),
						jen.Id("__resolution").Dot("Chain"),
						jen.Id("__resolution").Dot("Providers"),
						jen.Id("__resolution").Dot("LegacyChildren"),
						jen.Id("__resolution").Dot("Targets"),
						jen.Id("__resolution").Dot("NestedRoundRobin"),
						jen.Id("__planned"),
						jen.Lit(""),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
			),
		)
	}

	// Bridge: /call and /call-with-raw that the single-provider non-
	// streaming arm declined fall through here. This block also owns the
	// SOLE call-mode fallback-chain resolution (see the call-block comment
	// above): it runs one typed resolver, then for fallback chains prefers
	// the non-streaming Request dispatch when a Request surface exists and
	// the resolved chain is fully call-supported (#543); otherwise it
	// accumulates SSE deltas into a unary response via StreamRequest. The
	// StreamRequest bridge is reached when the non-streaming Request API is
	// unavailable (g.intro.Request==nil) or the call-side support gate
	// rejected the provider/chain. StreamMode is StreamModeCall or
	// StreamModeCallWithRaw, so NeedsPartials is false inside the
	// orchestrator and no partials ever reach the output channel — the pool
	// sees the same shape as the non-streaming call path.
	if hasBuildRequest {
		// Build the bridge's fallback-chain consume body. resolveRetryPolicy
		// runs once; then, when a Request surface exists, a fully
		// call-supported resolved chain dispatches unary via Request (#543)
		// before the StreamRequest bridge dispatch. Both arms consume the
		// SAME __resolution above — the choice is read off the resolved
		// chain, so there is no second resolve and no second RR advance.
		bridgeFallbackConsume := []jen.Code{resolveRetryPolicy()}
		if hasCallBuildRequest {
			// __callChainSupported is true only when the resolved chain has
			// no legacy children AND every resolved provider is call-
			// supported. Computed from __resolution (not a second resolve),
			// and emitted only when a Request surface exists so stream-only
			// routers never reference IsCallProviderSupported here.
			bridgeFallbackConsume = append(bridgeFallbackConsume,
				jen.Id("__callChainSupported").Op(":=").Len(jen.Id("__resolution").Dot("LegacyChildren")).Op("==").Lit(0),
				jen.If(jen.Id("__callChainSupported")).Block(
					jen.For(
						jen.List(jen.Id("_"), jen.Id("__provider")).Op(":=").Range().Id("__resolution").Dot("Providers"),
					).Block(
						jen.If(jen.Op("!").Qual(g.pkgs.BuildRequestPkg, "IsCallProviderSupported").Call(jen.Id("__provider"))).Block(
							jen.Id("__callChainSupported").Op("=").False(),
							jen.Break(),
						),
					),
				),
				jen.If(jen.Id("__callChainSupported")).Block(
					callFallbackDispatch()...,
				),
			)
		}
		bridgeFallbackConsume = append(bridgeFallbackConsume,
			jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildFallbackChainPlanFromResolution").Call(
				jen.Id("__effective"),
				jen.Id("__resolution"),
				jen.Id("retryPolicy"),
				jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
			),
			jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
			jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
				jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
				jen.Lit(""), jen.Id("retryPolicy"),
				jen.Id("__resolution").Dot("Chain"),
				jen.Id("__resolution").Dot("Providers"),
				jen.Id("__resolution").Dot("LegacyChildren"),
				jen.Id("__resolution").Dot("Targets"),
				jen.Id("__resolution").Dot("NestedRoundRobin"),
				jen.Id("__planned"),
				jen.Lit(""),
			),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
			jen.Return(jen.Id("out"), jen.Nil()),
		)

		routerBody = append(routerBody,
			jen.Comment("Bridge: /call and /call-with-raw via StreamRequest when Request is unavailable"),
			jen.If(
				jen.Qual(g.pkgs.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCall").
					Op("||").Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCallWithRaw")),
			).Block(
				// Single-provider path
				jen.Id("provider").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(g.pkgs.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("provider"),
						jen.Id("retryPolicy"),
						jen.Qual(g.pkgs.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("provider"), jen.Id("retryPolicy"),
						jen.Nil(), jen.Nil(),
						jen.Nil(),
						jen.Nil(), jen.Nil(),
						jen.Id("__planned"),
						jen.Id("__effective"),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
				// Fallback chain path — the sole call-mode resolver.
				// Resolves with IsProviderSupported (stream side, the
				// superset predicate) so this one resolution covers every
				// chain: those fully call-supported AND those that need the
				// bridge. The dispatch API is then chosen off that single
				// resolution (no second resolve, no second RR advance):
				// when a Request surface exists and the resolved chain has
				// no legacy children and every provider is call-supported,
				// dispatch unary via BuildRequestAPIRequest (#543);
				// otherwise bridge via BuildRequestAPIStreamRequest. Either
				// way the typed resolver's per-child Targets +
				// NestedRoundRobin thread through so centralized RR fallback
				// children rotate cross-worker.
				jen.List(jen.Id("__resolution"), jen.Id("__fbErr")).Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveFallbackChainPlanForClient").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(g.pkgs.IntrospectedPkg, "FallbackChains"),
					jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
					jen.Qual(g.pkgs.BuildRequestPkg, "IsProviderSupported"),
					jen.Qual(g.pkgs.BuildRequestPkg, "PreferAdvancer").Call(
						jen.Id("adapter"),
						jen.Qual(g.pkgs.IntrospectedPkg, "RoundRobinCoordinator"),
					),
				),
				jen.If(jen.Id("__fbErr").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("__fbErr")),
				),
				jen.If(fallbackChainConsumeCond()).Block(
					bridgeFallbackConsume...,
				),
			),
		)
	}

	// Compute the planned metadata for the legacy path. BuildLegacyMetadataPlan
	// classifies the request (PathReason) and includes chain details when the
	// client is a fallback strategy. The helper also picks a retry policy so
	// the plan carries RetryMax/RetryPolicy on legacy requests that still
	// honour them (the policy is used by the legacy BAML runtime, not by
	// the generator).
	routerBody = append(routerBody,
		jen.Comment("Legacy path: CallStream + OnTick (for unsupported/empty providers or BAML versions without a BuildRequest surface)"),
		// Retry policy resolution mirrors the BuildRequest paths:
		// wrapper-first (__retryClient — the pre-unwrap strategy /
		// primary-override target) with leaf-fallback to __effective
		// (the post-RR leaf, which also folds in any client_registry
		// primary override). The dispatched client identity for
		// WithClient / SetPrimaryClient is __effective so the legacy
		// dispatcher actually contacts the resolved leaf, but retry
		// honors BAML's LLMStrategyProvider::WithRetryPolicy.
		//
		// Without this seam, a request that resolved __effective to
		// a leaf via RR unwrap or primary override would lose the
		// strategy wrapper's retry_policy: ResolveRetryPolicy on the
		// leaf alone cannot see the wrapper's introspected entry,
		// since ClientRetryPolicy is keyed by client name and the
		// wrapper's name was thrown away at __effective assignment.
		jen.Id("__legacyRetryPolicy").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveStrategyAwareRetryPolicy").Call(
			jen.Id("adapter"),
			jen.Id("__retryClient"),
			jen.Id("__effective"),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__retryClient")),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
			jen.Qual(g.pkgs.IntrospectedPkg, "RetryPolicies"),
		),
		// Build the legacy metadata plan keyed on __effective in
		// every case — RR, primary override, or neither. Previously
		// the non-RR branch called BuildLegacyMetadataPlan with
		// FunctionClient[methodName], which re-resolved primary
		// internally but wouldn't propagate the resolved identity
		// back out as __legacyClientOverride, so the subsequent
		// WithClient append (on BAML 0.219+) targeted BAML with an
		// empty override even when primary was set. Passing
		// __effective unconditionally collapses the branching and
		// fixes both paths. __rrInfo is copied in afterwards; it's
		// nil on the non-RR and legacy-only-adapter paths.
		// Mode-aware predicate selection: the legacy metadata
		// plan classifies the request's provider against either
		// the stream support table or the call support table. The
		// BuildRequest route is unconditional (#537), so a call
		// mode reaches final legacy only when no BuildRequest
		// landing block accepted it.
		//
		// When a stream bridge exists (hasBuildRequest=true) every
		// call-mode fallthrough was already gated on
		// IsProviderSupported — a provider that is stream-supported
		// but call-only-unsupported (e.g. under the debug
		// BAML_REST_CALL_UNSUPPORTED_PROVIDERS hook) bridges and
		// never reaches here — so the stream predicate is the
		// correct one and no call-side override is emitted.
		//
		// When there is no stream bridge (hasBuildRequest=false) a
		// call mode the non-streaming Request path declined falls
		// straight to legacy, so the metadata reason must reflect
		// the call-side classification; otherwise it would produce a
		// too-optimistic PathReason. Stream modes always use the
		// stream predicate.
		jen.Id("__legacyPredicate").Op(":=").Qual(g.pkgs.BuildRequestPkg, "IsProviderSupported"),
		func() jen.Code {
			if hasBuildRequest {
				// Stream bridge present: the stream predicate is
				// always correct for call-mode fallthroughs.
				return jen.Null()
			}
			callModeCond := jen.Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCall").
				Op("||").Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCallWithRaw")
			// For call modes the legacy classification swaps in the
			// call-side support predicate so the metadata reason
			// reflects call-side support — a provider that is
			// stream-supported but call-unsupported reaches legacy
			// here only when there is no bridge.
			return jen.If(callModeCond).Block(
				jen.Id("__legacyPredicate").Op("=").Qual(g.pkgs.BuildRequestPkg, "IsCallProviderSupported"),
			)
		}(),
		// BuildLegacyMetadataPlanForClient classifies the legacy
		// route's PathReason. The BuildRequest route is unconditional
		// (#537), and the call-side support knob is already folded into
		// __legacyPredicate above.
		jen.Id("__plannedLegacy").Op(":=").Qual(g.pkgs.BuildRequestPkg, "BuildLegacyMetadataPlanForClient").Call(
			jen.Id("__reg"),
			jen.Id("__effective"),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider").Index(jen.Id("__effective")),
			jen.Qual(g.pkgs.IntrospectedPkg, "FallbackChains"),
			jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
			jen.Id("__legacyPredicate"),
			jen.Id("__legacyRetryPolicy"),
		),
		jen.Id("__plannedLegacy").Dot("RoundRobin").Op("=").Id("__rrInfo"),
		// __legacyClientOverride is always __effective. On 0.219+
		// the legacy dispatcher uses it both for WithClient
		// (non-streaming) and for SetPrimaryClient on the legacy
		// streaming registry. On pre-0.219 adapters WithClient
		// emission is gated on supportsWithClient and elided; the
		// legacy streaming path STILL reads __legacyClientOverride
		// via makeLegacyStreamOptionsFromAdapter's
		// SetPrimaryClient(clientOverride) call (BAML's
		// Stream.<Method> drops callOpts.client and reads only
		// the registry's primary, so the override survives that
		// seam regardless of runtime version).
		jen.Id("__legacyClientOverride").Op(":=").Id("__effective"),
		jen.Qual(g.pkgs.BuildRequestPkg, "LogLegacyClassification").Call(
			jen.Id("adapter"),
			jen.Lit(me.methodName),
			jen.Id("__plannedLegacy"),
		),
		// Direct-legacy native-first probe (de-BAML unary multi-provider S1).
		// Emitted ONLY for the dynamic de-BAML method. It fires ONLY when: the
		// umbrella flag is on, a SERVE (not shadow) callback is installed, the mode
		// is plain unary call, and the resolved leaf is a DIRECT single leaf whose
		// existing BAML route is legacy (a call-UNSUPPORTED provider, no
		// strategy/chain/RR). It routes the SAME selected leaf through the native
		// serve probe FIRST; on a native DECLINE it runs the EXACT ordinary legacy
		// serving lifecycle (bamlRest…DirectLegacyCall reuses runNoRawOrchestration
		// + the same stream-driving body, so result/error/heartbeat/raw/metadata are
		// byte-identical to the plain legacy path — only planned_engine=native
		// differs). Anything else (flag off, shadow-only, with-raw/stream, a
		// strategy, a call-supported/empty provider) falls through UNCHANGED to the
		// plain legacy dispatch below.
		func() jen.Code {
			if !me.isDeBAMLMethod() {
				return jen.Null()
			}
			return jen.If(
				jen.Id("adapter").Dot("DeBAMLConfig").Call().Dot("Enabled").
					Op("&&").Id("mode").Op("==").Qual(g.pkgs.InterfacesPkg, "StreamModeCall").
					Op("&&").Id("hasNativeServe").Call(jen.Id("adapter")).
					Op("&&").Id("__plannedLegacy").Dot("Strategy").Op("==").Lit("").
					Op("&&").Id("__plannedLegacy").Dot("Chain").Op("==").Nil().
					Op("&&").Id("__plannedLegacy").Dot("RoundRobin").Op("==").Nil(),
			).Block(
				jen.Id("__directLegacyProvider").Op(":=").Qual(g.pkgs.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"), jen.Id("__effective"), jen.Qual(g.pkgs.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(
					jen.Id("__directLegacyProvider").Op("!=").Lit("").
						Op("&&").Op("!").Qual(g.pkgs.BuildRequestPkg, "IsCallProviderSupported").Call(jen.Id("__directLegacyProvider")),
				).Block(
					jen.Id("err").Op("=").Id(me.directLegacyCallMethodName()).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("__directLegacyProvider"),
						// TRUTHFUL retry-override fact: a resolved strategy-aware retry
						// policy declines native admission pre-socket, preserving BAML's
						// retry semantics on the ordinary legacy path.
						jen.Id("__legacyRetryPolicy").Op("!=").Nil(),
						jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride"),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
			)
		}(),
		jen.Switch(jen.Id("mode")).Block(
			// StreamModeCall: final only, no raw, skip partials
			jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "StreamModeCall")).Block(
				jen.Id("err").Op("=").Id(me.noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeStream: partials + final, no raw
			jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "StreamModeStream")).Block(
				jen.Id("err").Op("=").Id(me.noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeCallWithRaw: final + raw, skip intermediate parsing
			jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "StreamModeCallWithRaw")).Block(
				jen.Id("err").Op("=").Id(me.fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeStreamWithRaw: partials + final + raw
			jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "StreamModeStreamWithRaw")).Block(
				jen.Id("err").Op("=").Id(me.fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// Default case to prevent silent hangs if unknown mode
			jen.Default().Block(
				jen.Id("err").Op("=").Qual("fmt", "Errorf").Call(
					jen.Lit("unknown StreamMode: %d"),
					jen.Id("mode"),
				),
			),
		),
		jen.If(jen.Id("err").Op("!=").Nil()).Block(
			jen.Return(jen.Nil(), jen.Id("err")),
		),
		jen.Return(jen.Id("out"), jen.Nil()),
	)

	out.Func().
		Id(me.methodName).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
		).
		Call(
			jen.List(
				jen.Op("<-").Chan().Add(streamResultInterface.Clone()),
				jen.Error(),
			)).
		Block(routerBody...)
}
