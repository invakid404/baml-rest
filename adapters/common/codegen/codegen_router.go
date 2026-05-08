package codegen

import (
	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
)

// emitRouter emits the public per-method router function. The
// router selects between BuildRequest, the streaming-bridged call
// path, and the legacy CallStream+OnTick fallthrough based on the
// adapter's StreamMode and the runtime UseBuildRequest gate; it
// then forwards to the corresponding generated impl
// (<method>_buildRequest / <method>_buildCallRequest / <method>_noRaw
// / <method>_full).
func (me *methodEmitter) emitRouter() {
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

	hasBuildRequest := introspected.StreamRequest != nil
	hasCallBuildRequest := introspected.Request != nil

	// Generate the public router function that dispatches based on StreamMode()
	// Creates the output channel and passes it to the inner implementation.
	//
	// When the BuildRequest path is available and the feature flag is set,
	// it takes priority for both call and stream modes. Falls back to the
	// legacy paths for unsupported providers or when the feature flag is off.
	routerBody := []jen.Code{
		jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
		jen.Var().Id("err").Error(),
		jen.Id("mode").Op(":=").Id("adapter").Dot("StreamMode").Call(),
	}
	// Always seed __effective from ResolvePrimaryClient and leave
	// __rrInfo nil. This is the legacy-equivalent shape: primary
	// override applied, no RR unwrap, no coordinator advance, no
	// RR metadata. supportsWithClient adapters with the flag on
	// upgrade these via ResolveEffectiveClient below; everyone
	// else (older adapters, or modern adapters with the flag off)
	// keeps this baseline so BAML's own strategy rotation owns RR.
	routerBody = append(routerBody,
		jen.Id("__effective").Op(":=").Qual(common.BuildRequestPkg, "ResolvePrimaryClient").Call(
			jen.Id("adapter"),
			jen.Qual(common.IntrospectedPkg, "FunctionClient").Index(jen.Lit(me.methodName)),
		),
		jen.Var().Id("__rrInfo").Op("*").Qual(common.InterfacesPkg, "RoundRobinInfo"),
	)
	if g.supportsWithClient {
		// Cache UseBuildRequest() once per request so every gate
		// downstream sees the same value. Without this,
		// ResolveEffectiveClient ran unconditionally for
		// supportsWithClient adapters and the BR landing blocks
		// re-read the env var, which (a) made the flag-off path
		// keep advancing the RR coordinator and pinning BAML to
		// the leaf via WithClient, defeating its kill-switch
		// contract, and (b) created split-decision risk if the
		// cached value ever drifted within a request.
		//
		// Scoped under `if supportsWithClient` because every
		// __useBuildRequest reference downstream — the upgrade
		// gate just below, the BR landing-block gates, and the
		// legacy predicate gate — is itself only emitted when
		// supportsWithClient is true (those gates check
		// hasCallBuildRequest / hasBuildRequest, which the
		// fail-fast invariant in generate ties to
		// supportsWithClient via panic). Hoisting the declaration
		// to router entry would emit "declared and not used" on
		// v0.204 / v0.215 adapters where every consumer is
		// filtered out.
		routerBody = append(routerBody,
			jen.Id("__useBuildRequest").Op(":=").Qual(common.BuildRequestPkg, "UseBuildRequest").Call(),
		)

		// Full RR resolution upgrade: apply the runtime primary
		// override, unwrap baml-roundrobin wrappers, and advance
		// the coordinator (or the worker-installed RemoteAdvancer)
		// for the leaf selection. Gated on __useBuildRequest so
		// that flipping BAML_REST_USE_BUILD_REQUEST off truly
		// reverts to the legacy CallStream+OnTick semantics —
		// including BAML's per-worker runtime RR rotation. Without
		// the runtime gate, the coordinator advances and __rrInfo
		// gets populated even when the operator has explicitly
		// disabled the new code path.
		routerBody = append(routerBody,
			jen.If(jen.Id("__useBuildRequest")).Block(
				jen.List(jen.Id("__rrEffective"), jen.Id("__rrInfoUpgrade"), jen.Id("__rrErr")).Op(":=").
					Qual(common.BuildRequestPkg, "ResolveEffectiveClient").Call(
					jen.Id("adapter"),
					jen.Qual(common.IntrospectedPkg, "FunctionClient").Index(jen.Lit(me.methodName)),
					jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					jen.Qual(common.IntrospectedPkg, "RoundRobinCoordinator"),
				),
				jen.If(jen.Id("__rrErr").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("__rrErr")),
				),
				jen.Id("__effective").Op("=").Id("__rrEffective"),
				jen.Id("__rrInfo").Op("=").Id("__rrInfoUpgrade"),
			),
		)
	}
	routerBody = append(routerBody,
		jen.Id("__reg").Op(":=").Id("adapter").Dot("OriginalClientRegistry").Call(),
	)

	// Helper to generate the common retry policy resolution + dispatch call
	// for both single-provider and fallback-chain paths.
	//
	// Keyed on __effective (the post-RR leaf) rather than
	// FunctionClient[methodName] (the function's declared default
	// client, which may be an RR wrapper). For non-RR functions the
	// two are identical; for RR, we must use the child that will
	// actually handle the request, otherwise the retry policy would
	// come from the wrapper — retry config isn't inherited from
	// strategy wrappers in BAML semantics.
	resolveRetryPolicy := func() jen.Code {
		return jen.Id("retryPolicy").Op(":=").Qual(common.BuildRequestPkg, "ResolveRetryPolicy").Call(
			jen.Id("adapter"),
			jen.Id("__effective"),
			jen.Qual(common.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
			jen.Qual(common.IntrospectedPkg, "RetryPolicies"),
		)
	}

	// fallbackChainConsumeCond builds the condition guarding the call-side
	// fallback block. When a StreamRequest bridge exists downstream, the
	// call block only consumes chains with no call-legacy children; mixed
	// chains must fall through so the bridge can re-resolve them with
	// IsProviderSupported and drive call-legacy-but-stream-supported
	// children through the streaming path rather than legacyCallChildFn.
	// Without a bridge, the call block is the only BuildRequest landing
	// spot and accepts any non-empty chain, matching pre-bridge behaviour.
	fallbackChainConsumeCond := func(hasBridge bool) jen.Code {
		nonEmpty := jen.Len(jen.Id("__chain")).Op(">").Lit(0)
		if !hasBridge {
			return nonEmpty
		}
		return nonEmpty.Op("&&").Len(jen.Id("__legacyChildren")).Op("==").Lit(0)
	}

	// Non-streaming BuildRequest path for /call and /call-with-raw.
	// Uses Request (not StreamRequest) to build non-streaming HTTP requests.
	// Checked before the streaming path since it's more efficient for call modes.
	if hasCallBuildRequest {
		routerBody = append(routerBody,
			jen.Comment("Try non-streaming BuildRequest path for /call and /call-with-raw"),
			jen.If(
				jen.Id("__useBuildRequest").
					Op("&&").Qual(common.IntrospectedPkg, "Request").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
					Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw")),
			).Block(
				// Single-provider path — keyed off the effective (post-RR)
				// client. ResolveClientProvider consults the runtime
				// registry for a per-client provider override before
				// falling back to the introspected default.
				jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsCallProviderSupported").Call(jen.Id("provider"))).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("provider"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildCallRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("provider"), jen.Id("retryPolicy"),
						jen.Nil(), jen.Nil(),
						jen.Nil(),
						jen.Id("__planned"),
						jen.Id("__effective"),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
				// Fallback chain path: if single-provider check failed,
				// try resolving a fallback chain off the effective client.
				// When a bridge block exists (hasBuildRequest), a mixed
				// chain — one with any call-legacy children — must fall
				// through so the bridge re-resolves it with
				// IsProviderSupported; a child that is call-legacy may
				// still be stream-supported, and the bridge's
				// StreamRequest path drives such children better than
				// legacyCallChildFn. This block therefore only takes
				// chains that are fully call-supported. When no bridge
				// exists (hasBuildRequest=false) mixed chains stay here,
				// matching the pre-bridge behaviour.
				jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					jen.Qual(common.BuildRequestPkg, "IsCallProviderSupported"),
				),
				jen.If(fallbackChainConsumeCond(hasBuildRequest)).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("__chain"),
						jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIRequest"),
						jen.Id("__fbReason"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildCallRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Lit(""), jen.Id("retryPolicy"),
						jen.Id("__chain"), jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
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

	// Streaming BuildRequest path for /stream and /stream-with-raw.
	// Explicitly gated to streaming modes; a separate bridge block below
	// handles /call and /call-with-raw via stream accumulation when the
	// non-streaming Request API declined.
	if hasBuildRequest {
		routerBody = append(routerBody,
			jen.Comment("Try streaming BuildRequest path for /stream and /stream-with-raw"),
			jen.If(
				jen.Id("__useBuildRequest").
					Op("&&").Qual(common.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeStream").
					Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeStreamWithRaw")),
			).Block(
				// Single-provider path keyed off the effective client.
				jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("provider"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("provider"), jen.Id("retryPolicy"),
						jen.Nil(), jen.Nil(),
						jen.Nil(),
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
				// generated legacyStreamChildFn.
				jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					jen.Qual(common.BuildRequestPkg, "IsProviderSupported"),
				),
				jen.If(jen.Len(jen.Id("__chain")).Op(">").Lit(0)).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("__chain"),
						jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
						jen.Id("__fbReason"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Lit(""), jen.Id("retryPolicy"),
						jen.Id("__chain"), jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
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

	// Bridge: /call and /call-with-raw that the non-streaming block
	// declined fall through to the streaming BuildRequest path, which
	// accumulates SSE deltas into a unary response. Triggered when the
	// non-streaming Request API is unavailable (introspected.Request==nil
	// or the call-side support gate rejected the provider/chain) but the
	// StreamRequest API can drive it. StreamMode is StreamModeCall or
	// StreamModeCallWithRaw, so NeedsPartials is false inside the
	// orchestrator and no partials ever reach the output channel — the
	// pool sees the same shape as the non-streaming call path.
	if hasBuildRequest {
		routerBody = append(routerBody,
			jen.Comment("Bridge: /call and /call-with-raw via StreamRequest when Request is unavailable"),
			jen.If(
				jen.Id("__useBuildRequest").
					Op("&&").Qual(common.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
					Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
					Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw")),
			).Block(
				// Single-provider path
				jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
				),
				jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("provider"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Id("provider"), jen.Id("retryPolicy"),
						jen.Nil(), jen.Nil(),
						jen.Nil(),
						jen.Id("__planned"),
						jen.Id("__effective"),
					),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					),
					jen.Return(jen.Id("out"), jen.Nil()),
				),
				// Fallback chain path (bridge). Uses IsProviderSupported
				// (stream side) because the whole point of the bridge is
				// to accept chains that IsCallProviderSupported rejected.
				jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
					jen.Id("__reg"),
					jen.Id("__effective"),
					jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					jen.Qual(common.BuildRequestPkg, "IsProviderSupported"),
				),
				jen.If(jen.Len(jen.Id("__chain")).Op(">").Lit(0)).Block(
					resolveRetryPolicy(),
					jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
						jen.Id("__effective"),
						jen.Id("__chain"),
						jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
						jen.Id("retryPolicy"),
						jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
						jen.Id("__fbReason"),
					),
					jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
					jen.Id("err").Op("=").Id(me.buildRequestMethodName).Call(
						jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
						jen.Lit(""), jen.Id("retryPolicy"),
						jen.Id("__chain"), jen.Id("__cprov"),
						jen.Id("__legacyChildren"),
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

	// Compute the planned metadata for the legacy path. BuildLegacyMetadataPlan
	// classifies the request (PathReason) and includes chain details when the
	// client is a fallback strategy. The helper also picks a retry policy so
	// the plan carries RetryMax/RetryPolicy on legacy requests that still
	// honour them (the policy is used by the legacy BAML runtime, not by
	// the generator).
	routerBody = append(routerBody,
		jen.Comment("Legacy path: CallStream + OnTick (for unsupported providers or when BuildRequest is disabled)"),
		// Retry policy and client identity for the legacy dispatch
		// derive from __effective, not FunctionClient[methodName].
		// __effective already accounts for both the RR unwrap and
		// any client_registry primary override, so this keys both
		// the retry policy and the WithClient/plan on the client
		// that will actually handle the request.
		//
		// Without this, a request with `primary` set to a client
		// whose retry_policy is only statically declared (not
		// redeclared in the runtime registry) would fall through to
		// ResolveRetryPolicy's step-4 default — which looked up
		// FunctionRetryPolicy[methodName] and therefore returned the
		// FUNCTION's declared client's policy, not the primary-
		// overridden client's. The BuildRequest path had the same
		// bug; the legacy branch was missed.
		jen.Id("__legacyRetryPolicy").Op(":=").Qual(common.BuildRequestPkg, "ResolveRetryPolicy").Call(
			jen.Id("adapter"),
			jen.Id("__effective"),
			jen.Qual(common.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
			jen.Qual(common.IntrospectedPkg, "RetryPolicies"),
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
		// the stream support table or the call support table. For
		// call modes that reach final legacy *without* a stream
		// bridge having re-resolved them (hasBuildRequest=false)
		// the metadata reason should reflect the call-side
		// classification — otherwise
		// BAML_REST_DISABLE_CALL_BUILD_REQUEST or the debug
		// BAML_REST_CALL_UNSUPPORTED_PROVIDERS flag can produce a
		// too-optimistic PathReason. When a stream bridge exists
		// every call-mode fallthrough has already been gated on
		// IsProviderSupported, so the stream predicate is the
		// correct one. Stream modes always use the stream
		// predicate.
		//
		// Every BuildRequest landing block is also gated at
		// runtime on UseBuildRequest(). When BuildRequest is
		// compiled in but the runtime gate returns false, /call
		// and /call-with-raw skip both the non-streaming Request
		// path AND the stream bridge, falling directly to final
		// legacy. In that path no stream-bridge re-resolution
		// happened, so the metadata predicate must be the call-
		// side IsCallProviderSupported.
		//
		// Emit a runtime gate inside the hasBuildRequest=true
		// arm: default to IsProviderSupported (correct when the
		// stream bridge actually ran), and override to
		// IsCallProviderSupported only when mode is a call mode
		// AND UseBuildRequest() returned false. The
		// no-hasBuildRequest arm keeps its original
		// compile-time-only override since there is no stream
		// bridge to re-resolve in any case.
		jen.Id("__legacyPredicate").Op(":=").Qual(common.BuildRequestPkg, "IsProviderSupported"),
		func() jen.Code {
			callModeCond := jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
				Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw")
			if hasBuildRequest {
				return jen.If(
					jen.Parens(callModeCond).
						Op("&&").Op("!").Id("__useBuildRequest"),
				).Block(
					jen.Id("__legacyPredicate").Op("=").Qual(common.BuildRequestPkg, "IsCallProviderSupported"),
				)
			}
			return jen.If(callModeCond).Block(
				jen.Id("__legacyPredicate").Op("=").Qual(common.BuildRequestPkg, "IsCallProviderSupported"),
			)
		}(),
		jen.Id("__plannedLegacy").Op(":=").Qual(common.BuildRequestPkg, "BuildLegacyMetadataPlanForClient").Call(
			jen.Id("__reg"),
			jen.Id("__effective"),
			jen.Qual(common.IntrospectedPkg, "ClientProvider").Index(jen.Id("__effective")),
			jen.Qual(common.IntrospectedPkg, "FallbackChains"),
			jen.Qual(common.IntrospectedPkg, "ClientProvider"),
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
		jen.Qual(common.BuildRequestPkg, "LogLegacyClassification").Call(
			jen.Id("adapter"),
			jen.Lit(me.methodName),
			jen.Id("__plannedLegacy"),
		),
		jen.Switch(jen.Id("mode")).Block(
			// StreamModeCall: final only, no raw, skip partials
			jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCall")).Block(
				jen.Id("err").Op("=").Id(me.noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeStream: partials + final, no raw
			jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStream")).Block(
				jen.Id("err").Op("=").Id(me.noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeCallWithRaw: final + raw, skip intermediate parsing
			jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCallWithRaw")).Block(
				jen.Id("err").Op("=").Id(me.fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
			),
			// StreamModeStreamWithRaw: partials + final + raw
			jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStreamWithRaw")).Block(
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
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
		).
		Call(
			jen.List(
				jen.Op("<-").Chan().Add(streamResultInterface.Clone()),
				jen.Error(),
			)).
		Block(routerBody...)
}
