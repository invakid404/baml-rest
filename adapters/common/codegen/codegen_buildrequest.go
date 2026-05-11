package codegen

import (
	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
)

// emitBAMLHTTPRequestConversion generates the jen code that converts a
// baml.HTTPRequest (stored in local variable "httpReq") into a
// *llmhttp.Request stored in local variable "req", emits any postProcess
// statements (e.g. PR1-bedrock SigV4 metadata attach), and returns
// (req, nil). Shared by both the streaming _buildRequest and
// non-streaming _buildCallRequest codegen paths so they stay in sync.
//
// postProcess receives the closure scope so callers can add side
// effects (header rewrites, auth attachment) after `req` is built but
// before the closure returns. nil is fine.
func emitBAMLHTTPRequestConversion(g *jen.Group, postProcess func(*jen.Group)) {
	g.List(jen.Id("url"), jen.Id("urlErr")).Op(":=").Id("httpReq").Dot("Url").Call()
	g.If(jen.Id("urlErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get URL: %w"), jen.Id("urlErr"))),
	)
	g.List(jen.Id("method"), jen.Id("methodErr")).Op(":=").Id("httpReq").Dot("Method").Call()
	g.If(jen.Id("methodErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get method: %w"), jen.Id("methodErr"))),
	)
	g.List(jen.Id("headers"), jen.Id("headersErr")).Op(":=").Id("httpReq").Dot("Headers").Call()
	g.If(jen.Id("headersErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get headers: %w"), jen.Id("headersErr"))),
	)
	g.List(jen.Id("body"), jen.Id("bodyErr")).Op(":=").Id("httpReq").Dot("Body").Call()
	g.If(jen.Id("bodyErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get body: %w"), jen.Id("bodyErr"))),
	)
	g.List(jen.Id("bodyText"), jen.Id("bodyTextErr")).Op(":=").Id("body").Dot("Text").Call()
	g.If(jen.Id("bodyTextErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get body text: %w"), jen.Id("bodyTextErr"))),
	)
	g.Id("req").Op(":=").Op("&").Qual(common.LLMHTTPPkg, "Request").Values(jen.Dict{
		jen.Id("URL"):     jen.Id("url"),
		jen.Id("Method"):  jen.Id("method"),
		jen.Id("Headers"): jen.Id("headers"),
		jen.Id("Body"):    jen.Id("bodyText"),
	})
	if postProcess != nil {
		postProcess(g)
	}
	g.Return(jen.Id("req"), jen.Nil())
}

// buildLegacyChildCallParams emits the call-parameter list for the
// generated Stream.<Method>(ctx, args..., opts...) invocation inside
// legacyStreamChildFn / legacyCallChildFn. The first argument MUST
// be the closure's per-attempt `ctx` parameter (not `adapter`, which
// embeds the request-wide context.Context). RunStreamOrchestration
// and RunCallOrchestration scope ctx per attempt — failed-fallback
// retries, deadline propagation, and orchestrator-level cancellation
// all flow through the closure's ctx parameter, and BAML's generated
// Stream.<Method> takes context.Context as its first arg. Threading
// `adapter` here would silently route the BAML call through the
// outer context and ignore per-attempt cancellation. Extracted so
// the contract is unit-testable; callers must pass `args` in their
// declaration order plus an `argCallParam` resolver that converts a
// BAML arg name into the local variable expression (input-field
// access, materialised media, etc.).
func buildLegacyChildCallParams(args []string, argCallParam func(string) jen.Code) []jen.Code {
	out := make([]jen.Code, 0, len(args)+2)
	out = append(out, jen.Id("ctx"))
	for _, arg := range args {
		out = append(out, argCallParam(arg))
	}
	out = append(out, jen.Id("opts").Op("..."))
	return out
}

// emitBuildRequest emits the streaming BuildRequest impl for this
// method (<method>_buildRequest) using BAML's StreamRequest API to
// produce a BAML HTTPRequest, execute it, and parse SSE events
// directly. Only emitted when introspected.StreamRequest is non-nil
// (BAML >= 0.219.0); otherwise the symbol baml_client.StreamRequest
// doesn't exist and the reference would fail to compile.
func (me *methodEmitter) emitBuildRequest() {
	if introspected.StreamRequest == nil {
		return
	}
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

	// Build the StreamRequest call params using callOpts (which may
	// include a WithClient override for fallback chains).
	var buildRequestCallParams []jen.Code
	buildRequestCallParams = append(buildRequestCallParams, jen.Id("ctx"))
	for _, arg := range me.args {
		buildRequestCallParams = append(buildRequestCallParams, me.argCallParam(arg))
	}
	buildRequestCallParams = append(buildRequestCallParams, jen.Id("callOpts").Op("..."))

	// Call params for the legacy-child Stream.<Method> invocation.
	// First arg is ctx — the closure's per-attempt context.
	// Threading it (rather than adapter, which embeds the
	// request-wide context.Context) lets a child-scoped
	// cancellation from RunStreamOrchestration / Run-
	// CallOrchestration abort the BAML call promptly. The
	// closure already receives ctx context.Context as its
	// first parameter; the call-params list must consume it.
	legacyStreamCallParams := buildLegacyChildCallParams(me.args, me.argCallParam)

	// The _buildRequest body
	buildRequestBody := me.makePreamble()

	// Build the closures for RunStreamOrchestration
	buildRequestBody = append(buildRequestBody,
		// buildRequestFn: calls StreamRequest.Method(ctx, args, opts...) -> baml.HTTPRequest -> llmhttp.Request
		// Accepts clientOverride to support fallback chain iteration via WithClient.
		jen.Id("buildRequestFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("clientOverride").String(),
		).Params(
			jen.Op("*").Qual(common.LLMHTTPPkg, "Request"),
			jen.Error(),
		).BlockFunc(func(jg *jen.Group) {
			// Build callOpts: clone options and append WithClient if override is set
			jg.Id("callOpts").Op(":=").Id("options")
			jg.Add(g.withClientCloneAndAppend("callOpts", "options"))
			// Call StreamRequest.Method(ctx, args..., callOpts...)
			jg.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
				Qual(common.GeneratedClientPkg, "StreamRequest").Dot(me.methodName).Call(buildRequestCallParams...)
			jg.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			)
			emitBAMLHTTPRequestConversion(jg, nil)
		}),

		// parseStreamFn: calls ParseStream.Method(ctx, accumulated, opts...)
		// The context_fix hack adds ctx as first param to ParseStream methods.
		jen.Id("parseStreamFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("accumulated").String(),
		).Params(jen.Any(), jen.Error()).Block(
			jen.Return(
				jen.Qual(common.GeneratedClientPkg, "ParseStream").Dot(me.methodName).Call(
					jen.Id("ctx"),
					jen.Id("accumulated"),
					jen.Id("options").Op("..."),
				),
			),
		),

		// parseFinalFn: calls Parse.Method(ctx, accumulated, opts...)
		// The context_fix hack adds ctx as first param to Parse methods.
		jen.Id("parseFinalFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("accumulated").String(),
		).Params(jen.Any(), jen.Error()).Block(
			jen.Return(
				jen.Qual(common.GeneratedClientPkg, "Parse").Dot(me.methodName).Call(
					jen.Id("ctx"),
					jen.Id("accumulated"),
					jen.Id("options").Op("..."),
				),
			),
		),

		// newResultFn: creates a pooled StreamResult.
		// ParseStream/Parse may return either a pointer (*T) or a value (T),
		// so we try pointer assertion first, then value assertion with &v.
		// This matches the legacy paths which handle both shapes.
		// Dynamic unwrap helpers are called when applicable, matching the
		// legacy _full path's behavior for DynamicProperties outputs.
		jen.Id("newResultFn").Op(":=").Func().Params(
			jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
			jen.Id("stream").Any(),
			jen.Id("final").Any(),
			jen.Id("raw").String(),
			jen.Id("reasoning").String(),
			jen.Id("err").Error(),
			jen.Id("reset").Bool(),
		).Params(jen.Qual(common.InterfacesPkg, "StreamResult")).Block(
			jen.Id("r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("r").Dot("kind").Op("=").Id("kind"),
			jen.Id("r").Dot("raw").Op("=").Id("raw"),
			jen.Id("r").Dot("reasoning").Op("=").Id("reasoning"),
			jen.Id("r").Dot("err").Op("=").Id("err"),
			jen.Id("r").Dot("reset").Op("=").Id("reset"),
			// Set stream field: try *T first (pointer return), then T (value return → take address)
			jen.If(jen.Id("stream").Op("!=").Nil()).Block(
				jen.If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("stream").Assert(me.streamTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicStream {
							return jen.Id(me.unwrapStreamFuncName).Call(jen.Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("streamParsed").Op("=").Id("v"),
				).Else().If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("stream").Assert(me.streamType.statement.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicStream {
							return jen.Id(me.unwrapStreamFuncName).Call(jen.Op("&").Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("streamParsed").Op("=").Op("&").Id("v"),
				),
			),
			// Set final field: try *T first, then T
			jen.If(jen.Id("final").Op("!=").Nil()).Block(
				jen.If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(me.finalTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicFinal {
							return jen.Id(me.unwrapFinalFuncName).Call(jen.Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("finalParsed").Op("=").Id("v"),
				).Else().If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(me.finalType.statement.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicFinal {
							return jen.Id(me.unwrapFinalFuncName).Call(jen.Op("&").Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("finalParsed").Op("=").Op("&").Id("v"),
				),
			),
			jen.Return(jen.Id("r")),
		),

		// legacyStreamChildFn runs one mixed-mode legacy child via
		// BAML's Stream API. Delegates the lifecycle (heartbeat
		// wiring + FunctionLog capture for raw) to the shared
		// runLegacyChildStream helper; the inner closure supplies
		// only the per-method Stream.<Method> invocation.
		//
		// Uses makeLegacyChildOptionsFromAdapter (not the outer
		// BuildRequest-safe options) so that runtime overrides on
		// a nested strategy-parent child reach BAML. The outer
		// options drops every resolved strategy parent and would
		// silently lose overrides that change strategy children,
		// flip provider, or supply a dynamic-only parent
		// definition. The scoped helper rebuilds a fresh registry
		// per callback containing only the strategy parents
		// reachable from clientOverride plus all non-strategy
		// leaves, so unrelated explicit parents in the request
		// can't poison this child via BAML's eager registry parse.
		// Signature: (any, raw string, reasoning string, error). The
		// reasoning return is always empty for mixed-mode legacy children —
		// this path reads BAML's FunctionLog.RawLLMResponse() (text-only)
		// and has no extractor to populate reasoning. Documented limitation;
		// see bamlutils/buildrequest/orchestrator.go's
		// StreamConfig.LegacyStreamChild doc-comment.
		jen.Id("legacyStreamChildFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("clientOverride").String(),
			jen.Id("_").String(),
			jen.Id("needsRaw").Bool(),
			jen.Id("sendHeartbeat").Func().Params(),
		).Params(jen.Any(), jen.String(), jen.String(), jen.Error()).Block(
			jen.List(jen.Id("callOpts"), jen.Id("childOptsErr")).Op(":=").
				Id("makeLegacyChildOptionsFromAdapter").Call(jen.Id("adapter"), jen.Id("clientOverride")),
			jen.If(jen.Id("childOptsErr").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Lit(""), jen.Lit(""), jen.Id("childOptsErr")),
			),
			g.withClientCloneAndAppend("callOpts", "callOpts"),
			jen.Return(jen.Id("runLegacyChildStream").Call(
				jen.Id("ctx"),
				jen.Id("needsRaw"),
				jen.Id("sendHeartbeat"),
				jen.Func().Params(jen.Id("onTick").Add(onTickType())).Params(jen.Any(), jen.Error()).Block(
					jen.Id("opts").Op(":=").Append(
						jen.Id("callOpts"),
						jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
					),
					jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
						Qual(common.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(legacyStreamCallParams...),
					jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("streamErr")),
					),
					jen.Var().Id("result").Any(),
					jen.Var().Id("lastErr").Error(),
					jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
						jen.If(jen.Id("streamVal").Dot("IsError")).Block(
							jen.Id("lastErr").Op("=").Id("streamVal").Dot("Error"),
							jen.Continue(),
						),
						jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
							jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
						),
					),
					jen.Return(jen.Id("result"), jen.Id("lastErr")),
				),
			)),
		),

		// StreamConfig. LegacyChildren is populated for mixed chains;
		// LegacyStreamChild is always wired so the orchestrator's
		// up-front validation passes even when legacyChildren is nil.
		// MetadataPlan / NewMetadataResult carry the per-request
		// routing metadata through to the orchestrator's planned +
		// outcome emissions. FallbackTargets / FallbackRoundRobin
		// carry the per-child dispatch target and RR decision for
		// centralized RR fallback children — the orchestrator's
		// dispatch loop consults FallbackTargets[child] when computing
		// the WithClient target so a centrally-unwrapped RR child
		// routes to the selected leaf rather than the RR wrapper name.
		jen.Id("streamConfig").Op(":=").Op("&").Qual(common.BuildRequestPkg, "StreamConfig").Values(jen.Dict{
			jen.Id("Provider"):           jen.Id("provider"),
			jen.Id("RetryPolicy"):        jen.Id("retryPolicy"),
			jen.Id("NeedsPartials"):      jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsPartials").Call(),
			jen.Id("NeedsRaw"):           jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsRaw").Call(),
			jen.Id("IncludeReasoning"):   jen.Id("adapter").Dot("IncludeReasoning").Call(),
			jen.Id("FallbackChain"):      jen.Id("fallbackChain"),
			jen.Id("ClientOverride"):     jen.Id("clientOverride"),
			jen.Id("ClientProviders"):    jen.Id("clientProviders"),
			jen.Id("LegacyChildren"):     jen.Id("legacyChildren"),
			jen.Id("FallbackTargets"):    jen.Id("fallbackTargets"),
			jen.Id("FallbackRoundRobin"): jen.Id("fallbackRoundRobin"),
			jen.Id("LegacyStreamChild"):  jen.Id("legacyStreamChildFn"),
			jen.Id("MetadataPlan"):       jen.Id("plannedMetadata"),
			jen.Id("NewMetadataResult"): jen.Func().Params(
				jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			).Qual(common.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
		}),

		// Run the orchestration in a goroutine with panic recovery, matching
		// the _noRaw/_full pattern. On panic, an error result is emitted so
		// the worker can surface it instead of crashing.
		// Resolve HTTP client: use adapter override if provided, else default
		jen.Id("__httpClient").Op(":=").Qual(common.LLMHTTPPkg, "DefaultClient"),
		jen.If(
			jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
			jen.Id("__c").Op("!=").Nil(),
		).Block(
			jen.Id("__httpClient").Op("=").Id("__c"),
		),

		jen.Go().Func().Params().Block(
			jen.Defer().Close(jen.Id("out")),
			jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
				jen.Func().Params(jen.Id("err").Error()).Block(
					jen.Id("__errR").Op(":=").Id("newResultFn").Call(
						jen.Qual(common.InterfacesPkg, "StreamResultKindError"),
						jen.Nil(), jen.Nil(), jen.Lit(""), jen.Lit(""), jen.Id("err"), jen.False(),
					),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Id("__errR").Dot("Release").Call(),
						),
					),
				),
				jen.Func().Params().Error().Block(
					jen.Return(jen.Qual(common.BuildRequestPkg, "RunStreamOrchestration").Call(
						jen.Id("adapter"),
						jen.Id("out"),
						jen.Id("streamConfig"),
						jen.Id("__httpClient"),
						jen.Id("buildRequestFn"),
						jen.Id("parseStreamFn"),
						jen.Id("parseFinalFn"),
						jen.Id("newResultFn"),
					)),
				),
			),
		).Call(),
		jen.Return(jen.Nil()),
	)

	out.Func().
		Id(me.buildRequestMethodName).
		Params(
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("provider").String(),
			jen.Id("retryPolicy").Op("*").Qual(common.RetryPkg, "Policy"),
			jen.Id("fallbackChain").Index().String(),
			jen.Id("clientProviders").Map(jen.String()).String(),
			jen.Id("legacyChildren").Map(jen.String()).Bool(),
			jen.Id("fallbackTargets").Map(jen.String()).String(),
			jen.Id("fallbackRoundRobin").Map(jen.String()).Op("*").Qual(common.InterfacesPkg, "RoundRobinInfo"),
			jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(buildRequestBody...)
}

// emitBuildCallRequest emits the non-streaming BuildRequest impl
// for this method (<method>_buildCallRequest) using BAML's Request
// API to build a non-streaming HTTP request, execute it, extract
// the LLM text from the JSON response, and parse. Only emitted when
// introspected.Request is non-nil (BAML >= 0.219.0).
func (me *methodEmitter) emitBuildCallRequest() {
	if introspected.Request == nil {
		return
	}
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

	// Build the Request call params using callOpts (which may
	// include a WithClient override for fallback chains).
	var callRequestCallParams []jen.Code
	callRequestCallParams = append(callRequestCallParams, jen.Id("ctx"))
	for _, arg := range me.args {
		callRequestCallParams = append(callRequestCallParams, me.argCallParam(arg))
	}
	callRequestCallParams = append(callRequestCallParams, jen.Id("callOpts").Op("..."))

	// Stream.<Method> params for the legacy call-mode child. BAML
	// exposes streaming as the primitive even for non-streaming use,
	// so legacy call children reuse runLegacyChildStream just like
	// streaming children. Ctx-first reasoning is identical to
	// legacyStreamCallParams above.
	legacyCallStreamCallParams := buildLegacyChildCallParams(me.args, me.argCallParam)

	buildCallRequestBody := me.makePreamble()

	buildCallRequestBody = append(buildCallRequestBody,
		// buildRequestFn: calls Request.Method(ctx, args, opts...) → baml.HTTPRequest → llmhttp.Request
		// Accepts clientOverride to support fallback chain iteration via WithClient.
		jen.Id("buildRequestFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("clientOverride").String(),
		).Params(
			jen.Op("*").Qual(common.LLMHTTPPkg, "Request"),
			jen.Error(),
		).BlockFunc(func(jg *jen.Group) {
			// Build callOpts: clone options and append WithClient if override is set
			jg.Id("callOpts").Op(":=").Id("options")
			jg.Add(g.withClientCloneAndAppend("callOpts", "options"))
			// Call Request.Method(ctx, args..., callOpts...) — non-streaming
			jg.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
				Qual(common.GeneratedClientPkg, "Request").Dot(me.methodName).Call(callRequestCallParams...)
			jg.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			)
			// PR1-bedrock breadcrumb (issue #243): the call branch
			// attaches AWS SigV4 metadata when the BAML-emitted URL
			// looks like a Bedrock Converse endpoint. Non-bedrock URLs
			// are a no-op for MaybeAttachBedrockAuth, so this stays
			// invisible to every other provider. Streaming codegen
			// stays untouched in PR 1 — streaming is gated by
			// supportedProviders["aws-bedrock"] which lands in PR 3.
			emitBAMLHTTPRequestConversion(jg, func(g *jen.Group) {
				g.If(
					jen.Id("authErr").Op(":=").Qual(common.LLMHTTPPkg, "MaybeAttachBedrockAuth").Call(jen.Id("ctx"), jen.Id("req")),
					jen.Id("authErr").Op("!=").Nil(),
				).Block(
					jen.Return(jen.Nil(), jen.Id("authErr")),
				)
			})
		}),

		// parseFinalFn: calls Parse.Method(ctx, text, opts...)
		jen.Id("parseFinalFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("text").String(),
		).Params(jen.Any(), jen.Error()).Block(
			jen.Return(
				jen.Qual(common.GeneratedClientPkg, "Parse").Dot(me.methodName).Call(
					jen.Id("ctx"),
					jen.Id("text"),
					jen.Id("options").Op("..."),
				),
			),
		),

		// newResultFn: creates a pooled StreamResult. The signature matches
		// the streaming _buildRequest's newResultFn (including the unused
		// "stream" parameter) so both paths satisfy the same NewResultFunc
		// type. The non-streaming call path never passes stream data.
		jen.Id("newResultFn").Op(":=").Func().Params(
			jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
			jen.Id("stream").Any(), // unused in non-streaming path; kept for signature parity
			jen.Id("final").Any(),
			jen.Id("raw").String(),
			jen.Id("reasoning").String(),
			jen.Id("err").Error(),
			jen.Id("reset").Bool(),
		).Params(jen.Qual(common.InterfacesPkg, "StreamResult")).Block(
			jen.Id("r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("r").Dot("kind").Op("=").Id("kind"),
			jen.Id("r").Dot("raw").Op("=").Id("raw"),
			jen.Id("r").Dot("reasoning").Op("=").Id("reasoning"),
			jen.Id("r").Dot("err").Op("=").Id("err"),
			jen.Id("r").Dot("reset").Op("=").Id("reset"),
			// Set final field: try *T first (pointer return), then T (value return → take address)
			jen.If(jen.Id("final").Op("!=").Nil()).Block(
				jen.If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(me.finalTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicFinal {
							return jen.Id(me.unwrapFinalFuncName).Call(jen.Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("finalParsed").Op("=").Id("v"),
				).Else().If(
					jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(me.finalType.statement.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if me.isDynamicFinal {
							return jen.Id(me.unwrapFinalFuncName).Call(jen.Op("&").Id("v"))
						}
						return jen.Null()
					}(),
					jen.Id("r").Dot("finalParsed").Op("=").Op("&").Id("v"),
				),
			),
			jen.Return(jen.Id("r")),
		),

		// legacyCallChildFn runs one mixed-mode legacy child for the
		// non-streaming path. Structurally identical to
		// legacyStreamChildFn in _buildRequest — see that closure
		// for the rationale, including why the per-callback
		// scoped registry from makeLegacyChildOptionsFromAdapter
		// is used instead of the outer BuildRequest-safe options.
		//
		// Signature: (any, raw string, reasoning string, error). Reasoning
		// is always empty for mixed-mode legacy children; see
		// legacyStreamChildFn for the rationale.
		jen.Id("legacyCallChildFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("clientOverride").String(),
			jen.Id("_").String(),
			jen.Id("needsRaw").Bool(),
			jen.Id("sendHeartbeat").Func().Params(),
		).Params(jen.Any(), jen.String(), jen.String(), jen.Error()).Block(
			jen.List(jen.Id("callOpts"), jen.Id("childOptsErr")).Op(":=").
				Id("makeLegacyChildOptionsFromAdapter").Call(jen.Id("adapter"), jen.Id("clientOverride")),
			jen.If(jen.Id("childOptsErr").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Lit(""), jen.Lit(""), jen.Id("childOptsErr")),
			),
			g.withClientCloneAndAppend("callOpts", "callOpts"),
			jen.Return(jen.Id("runLegacyChildStream").Call(
				jen.Id("ctx"),
				jen.Id("needsRaw"),
				jen.Id("sendHeartbeat"),
				jen.Func().Params(jen.Id("onTick").Add(onTickType())).Params(jen.Any(), jen.Error()).Block(
					jen.Id("opts").Op(":=").Append(
						jen.Id("callOpts"),
						jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
					),
					jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
						Qual(common.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(legacyCallStreamCallParams...),
					jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("streamErr")),
					),
					jen.Var().Id("result").Any(),
					jen.Var().Id("lastErr").Error(),
					jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
						jen.If(jen.Id("streamVal").Dot("IsError")).Block(
							jen.Id("lastErr").Op("=").Id("streamVal").Dot("Error"),
							jen.Continue(),
						),
						jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
							jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
						),
					),
					jen.Return(jen.Id("result"), jen.Id("lastErr")),
				),
			)),
		),

		// CallConfig. LegacyChildren is populated for mixed chains;
		// LegacyCallChild is always wired so validation passes even
		// when legacyChildren is nil. FallbackTargets / FallbackRound-
		// Robin mirror StreamConfig — the call-side dispatch loop
		// also honours FallbackTargets[child] when computing the
		// per-attempt WithClient target so centralized RR fallback
		// children dispatch to the leaf rather than the wrapper.
		jen.Id("callConfig").Op(":=").Op("&").Qual(common.BuildRequestPkg, "CallConfig").Values(jen.Dict{
			jen.Id("Provider"):           jen.Id("provider"),
			jen.Id("RetryPolicy"):        jen.Id("retryPolicy"),
			jen.Id("NeedsRaw"):           jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsRaw").Call(),
			jen.Id("IncludeReasoning"):   jen.Id("adapter").Dot("IncludeReasoning").Call(),
			jen.Id("FallbackChain"):      jen.Id("fallbackChain"),
			jen.Id("ClientOverride"):     jen.Id("clientOverride"),
			jen.Id("ClientProviders"):    jen.Id("clientProviders"),
			jen.Id("LegacyChildren"):     jen.Id("legacyChildren"),
			jen.Id("FallbackTargets"):    jen.Id("fallbackTargets"),
			jen.Id("FallbackRoundRobin"): jen.Id("fallbackRoundRobin"),
			jen.Id("LegacyCallChild"):    jen.Id("legacyCallChildFn"),
			jen.Id("MetadataPlan"):       jen.Id("plannedMetadata"),
			jen.Id("NewMetadataResult"): jen.Func().Params(
				jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			).Qual(common.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
		}),

		// Resolve HTTP client
		jen.Id("__httpClient").Op(":=").Qual(common.LLMHTTPPkg, "DefaultClient"),
		jen.If(
			jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
			jen.Id("__c").Op("!=").Nil(),
		).Block(
			jen.Id("__httpClient").Op("=").Id("__c"),
		),

		// Run in goroutine with panic recovery
		jen.Go().Func().Params().Block(
			jen.Defer().Close(jen.Id("out")),
			jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
				jen.Func().Params(jen.Id("err").Error()).Block(
					jen.Id("__errR").Op(":=").Id("newResultFn").Call(
						jen.Qual(common.InterfacesPkg, "StreamResultKindError"),
						jen.Nil(), jen.Nil(), jen.Lit(""), jen.Lit(""), jen.Id("err"), jen.False(),
					),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Id("__errR").Dot("Release").Call(),
						),
					),
				),
				jen.Func().Params().Error().Block(
					jen.Return(jen.Qual(common.BuildRequestPkg, "RunCallOrchestration").Call(
						jen.Id("adapter"),
						jen.Id("out"),
						jen.Id("callConfig"),
						jen.Id("__httpClient"),
						jen.Id("buildRequestFn"),
						jen.Id("parseFinalFn"),
						jen.Qual(common.BuildRequestPkg, "ExtractResponseContent"),
						jen.Id("newResultFn"),
					)),
				),
			),
		).Call(),
		jen.Return(jen.Nil()),
	)

	out.Func().
		Id(me.buildCallRequestMethodName).
		Params(
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("provider").String(),
			jen.Id("retryPolicy").Op("*").Qual(common.RetryPkg, "Policy"),
			jen.Id("fallbackChain").Index().String(),
			jen.Id("clientProviders").Map(jen.String()).String(),
			jen.Id("legacyChildren").Map(jen.String()).Bool(),
			jen.Id("fallbackTargets").Map(jen.String()).String(),
			jen.Id("fallbackRoundRobin").Map(jen.String()).Op("*").Qual(common.InterfacesPkg, "RoundRobinInfo"),
			jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(buildCallRequestBody...)
}
