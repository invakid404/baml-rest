package codegen

import (
	"github.com/dave/jennifer/jen"
)

// emitMaybeAttachBedrockAuth emits the call-branch postProcess block
// that hands the freshly-built *llmhttp.Request to
// llmhttp.MaybeAttachBedrockAuth, returning early on error. The helper
// is a URL-pattern attach hook: it no-ops on non-bedrock URLs, so the
// unconditional emit costs every other provider nothing. Extracted as
// a named function so codegen tests can pin the exact emitted shape
// AND assert it does not appear in the streaming-path emission (which
// passes nil postProcess to emitBAMLHTTPRequestConversion for
// non-bedrock providers).
//
// Kept as a callable target for the codegen-emission tests that pin
// the simple URL-pattern shape; production codegen routes through
// emitBedrockAuthDispatchFor so the explicit endpoint_url, region, and
// credential-selector override paths are honoured when the operator
// has configured them.
func emitMaybeAttachBedrockAuth(g *jen.Group, pkgs PackageConfig) {
	g.If(
		jen.Id("authErr").Op(":=").Qual(pkgs.LLMHTTPPkg, "MaybeAttachBedrockAuth").Call(jen.Id("ctx"), jen.Id("req")),
		jen.Id("authErr").Op("!=").Nil(),
	).Block(
		jen.Return(jen.Nil(), jen.Id("authErr")),
	)
}

// emitBedrockAuthDispatchFor returns a postProcess emitter that, for
// the given BAML method name, resolves the selected client at runtime
// and dispatches Bedrock auth attachment.
//
// Generated body (per closure):
//
//	selectedClient := clientOverride
//	if selectedClient == "" {
//	    selectedClient = introspected.FunctionClient["<methodName>"]
//	}
//	var bedrockEndpointURL string
//	var bedrockEndpointURLPresent bool
//	var bedrockRegion string
//	var bedrockRegionPresent bool
//	var bedrockCreds llmhttp.BedrockCredentialSelector
//	if bedrockOpts, ok := introspected.BedrockClientOptionsByName[selectedClient]; ok {
//	    bedrockEndpointURL, _ = bedrockOpts.EndpointURL.Resolve()
//	    bedrockEndpointURLPresent = bedrockOpts.EndpointURL.IsSet()
//	    bedrockRegion, _ = bedrockOpts.Region.Resolve()
//	    bedrockRegionPresent = bedrockOpts.Region.IsSet()
//	    bedrockCreds.AccessKeyID, _ = bedrockOpts.Credentials.AccessKeyID.Resolve()
//	    bedrockCreds.AccessKeyIDPresent = bedrockOpts.Credentials.AccessKeyID.IsSet()
//	    // ...same shape for SecretAccessKey, SessionToken, Profile
//	}
//	if authErr := llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{
//	    ClientName:         selectedClient,
//	    EndpointURL:        bedrockEndpointURL,
//	    EndpointURLPresent: bedrockEndpointURLPresent,
//	    Region:             bedrockRegion,
//	    RegionPresent:      bedrockRegionPresent,
//	    Credentials:        bedrockCreds,
//	}); authErr != nil {
//	    return nil, authErr
//	}
//
// Nothing-declared (all Present flags false, all values empty,
// credentials empty) falls through to MaybeAttachBedrockAuth's
// URL-pattern detection inside AttachBedrockAuthForClient, preserving
// the default-endpoint contract for clients without any `.baml`
// override.
//
// The closure variable `clientOverride` is in scope at the postProcess
// emission site (both buildRequestFn and buildBedrockStreamRequestFn
// declare it as a parameter), so the resolve-name step does not need
// any new closure plumbing.
//
// Presence semantics: BedrockOptionValue.IsSet() reports whether the
// field was declared in .baml; BedrockOptionValue.Resolve() returns
// the resolved value (which can be "" if an env.X reference does not
// resolve). Driving the *Present flags from IsSet() — NOT from
// Resolve()'s second return — is load-bearing for the
// declared-but-env-unset error path: a declared env.X reference whose
// env var is unset at runtime must enter the resolver's static branch
// (Present=true, Value="") so the existing "resolved to empty"
// error fires, rather than collapsing into the same shape as a
// never-declared field which would silently fall through to the
// default AWS chain. The same split applies to EndpointURL and
// Region: a declared env.X for endpoint_url that resolves unset must
// error (otherwise an operator routing to a proxy/LocalStack/VPC
// endpoint via env.MY_PROXY would silently fall back to real AWS
// Bedrock), not collapse into the no-override path.
func emitBedrockAuthDispatchFor(methodName string, pkgs PackageConfig) func(*jen.Group) {
	return func(g *jen.Group) {
		g.Id("selectedClient").Op(":=").Id("clientOverride")
		g.If(jen.Id("selectedClient").Op("==").Lit("")).Block(
			jen.Id("selectedClient").Op("=").Qual(pkgs.IntrospectedPkg, "FunctionClient").Index(jen.Lit(methodName)),
		)
		g.Var().Defs(
			jen.Id("bedrockEndpointURL").String(),
			jen.Id("bedrockEndpointURLPresent").Bool(),
			jen.Id("bedrockRegion").String(),
			jen.Id("bedrockRegionPresent").Bool(),
			jen.Id("bedrockCreds").Qual(pkgs.LLMHTTPPkg, "BedrockCredentialSelector"),
		)
		g.If(
			jen.List(jen.Id("bedrockOpts"), jen.Id("ok")).Op(":=").Qual(pkgs.IntrospectedPkg, "BedrockClientOptionsByName").Index(jen.Id("selectedClient")),
			jen.Id("ok"),
		).Block(
			jen.List(jen.Id("bedrockEndpointURL"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("EndpointURL").Dot("Resolve").Call(),
			jen.Id("bedrockEndpointURLPresent").Op("=").Id("bedrockOpts").Dot("EndpointURL").Dot("IsSet").Call(),
			jen.List(jen.Id("bedrockRegion"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("Region").Dot("Resolve").Call(),
			jen.Id("bedrockRegionPresent").Op("=").Id("bedrockOpts").Dot("Region").Dot("IsSet").Call(),
			jen.List(jen.Id("bedrockCreds").Dot("AccessKeyID"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("Credentials").Dot("AccessKeyID").Dot("Resolve").Call(),
			jen.Id("bedrockCreds").Dot("AccessKeyIDPresent").Op("=").Id("bedrockOpts").Dot("Credentials").Dot("AccessKeyID").Dot("IsSet").Call(),
			jen.List(jen.Id("bedrockCreds").Dot("SecretAccessKey"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("Credentials").Dot("SecretAccessKey").Dot("Resolve").Call(),
			jen.Id("bedrockCreds").Dot("SecretAccessKeyPresent").Op("=").Id("bedrockOpts").Dot("Credentials").Dot("SecretAccessKey").Dot("IsSet").Call(),
			jen.List(jen.Id("bedrockCreds").Dot("SessionToken"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("Credentials").Dot("SessionToken").Dot("Resolve").Call(),
			jen.Id("bedrockCreds").Dot("SessionTokenPresent").Op("=").Id("bedrockOpts").Dot("Credentials").Dot("SessionToken").Dot("IsSet").Call(),
			jen.List(jen.Id("bedrockCreds").Dot("Profile"), jen.Id("_")).Op("=").Id("bedrockOpts").Dot("Credentials").Dot("Profile").Dot("Resolve").Call(),
			jen.Id("bedrockCreds").Dot("ProfilePresent").Op("=").Id("bedrockOpts").Dot("Credentials").Dot("Profile").Dot("IsSet").Call(),
		)
		g.If(
			jen.Id("authErr").Op(":=").Qual(pkgs.LLMHTTPPkg, "AttachBedrockAuthForClient").Call(
				jen.Id("ctx"),
				jen.Id("req"),
				jen.Qual(pkgs.LLMHTTPPkg, "BedrockClientAuthOptions").Values(jen.Dict{
					jen.Id("ClientName"):         jen.Id("selectedClient"),
					jen.Id("EndpointURL"):        jen.Id("bedrockEndpointURL"),
					jen.Id("EndpointURLPresent"): jen.Id("bedrockEndpointURLPresent"),
					jen.Id("Region"):             jen.Id("bedrockRegion"),
					jen.Id("RegionPresent"):      jen.Id("bedrockRegionPresent"),
					jen.Id("Credentials"):        jen.Id("bedrockCreds"),
				}),
			),
			jen.Id("authErr").Op("!=").Nil(),
		).Block(
			jen.Return(jen.Nil(), jen.Id("authErr")),
		)
	}
}

// emitBedrockStreamPostProcessFor returns a postProcess emitter for
// the streaming-side closure: URL mutation /converse → /converse-stream,
// AWS event-stream Accept header injection, and Bedrock auth attach.
// BAML's non-streaming modular AWS builder produces a /converse URL;
// baml-rest streams against /converse-stream because BAML's
// StreamRequest path is upstream-blocked for aws-bedrock.
//
// Order matters:
//  1. URL rewrite BEFORE auth attach so the endpoint override (when
//     configured) and the SigV4 signature both run over the
//     /converse-stream path.
//  2. Accept header AFTER the conversion populates req.Headers from
//     the BAML output (overwriting the Content-Type-only default
//     headers BAML emits).
//  3. AttachBedrockAuthForClient last so the SigV4 hook reads the
//     final URL — including any operator-configured `endpoint_url`
//     override resolved through the introspected
//     BedrockClientOptionsByName map.
//
// Emitted only on the bedrock streaming closure; the SSE-path
// streaming closure still passes nil postProcess.
func emitBedrockStreamPostProcessFor(methodName string, pkgs PackageConfig) func(*jen.Group) {
	dispatch := emitBedrockAuthDispatchFor(methodName, pkgs)
	return func(g *jen.Group) {
		// URL mutation: single replacement of /converse → /converse-stream.
		// The Bedrock URL pattern is
		// https://bedrock-runtime.<region>.amazonaws.com/model/<modelId>/converse,
		// so a single-replacement pass is safe; double-replacement would
		// be a problem only if a model id contained `/converse`, which
		// AWS model id rules forbid.
		g.Id("req").Dot("URL").Op("=").Qual("strings", "Replace").Call(
			jen.Id("req").Dot("URL"),
			jen.Lit("/converse"),
			jen.Lit("/converse-stream"),
			jen.Lit(1),
		)
		// Inject the AWS event-stream Accept header. BAML's modular AWS
		// builder doesn't set Accept (it sets Content-Type only), and
		// without this header the Bedrock service responds with JSON
		// instead of the event-stream wire format.
		g.If(jen.Id("req").Dot("Headers").Op("==").Nil()).Block(
			jen.Id("req").Dot("Headers").Op("=").Make(jen.Map(jen.String()).String()),
		)
		g.Id("req").Dot("Headers").Index(jen.Lit("Accept")).Op("=").Qual(pkgs.LLMHTTPPkg, "AWSStreamContentType")

		// Bedrock auth dispatch: explicit endpoint_url, region, and/or
		// credential-selector override when configured for the
		// selected client; URL-pattern fallback for the
		// default-endpoint case.
		dispatch(g)
	}
}

// emitBAMLHTTPRequestConversion generates the jen code that converts a
// baml.HTTPRequest (stored in local variable "httpReq") into a
// *llmhttp.Request stored in local variable "req", emits any postProcess
// statements (e.g. SigV4 metadata attach for aws-bedrock), and returns
// (req, nil). Shared by both the streaming _buildRequest and
// non-streaming _buildCallRequest codegen paths so they stay in sync.
//
// postProcess receives the closure scope so callers can add side
// effects (header rewrites, auth attachment) after `req` is built but
// before the closure returns. nil is fine.
func emitBAMLHTTPRequestConversion(g *jen.Group, pkgs PackageConfig, postProcess func(*jen.Group)) {
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
	g.Id("req").Op(":=").Op("&").Qual(pkgs.LLMHTTPPkg, "Request").Values(jen.Dict{
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
// directly. Only emitted when g.intro.StreamRequest is non-nil
// (BAML >= 0.219.0); otherwise the symbol baml_client.StreamRequest
// doesn't exist and the reference would fail to compile.
//
// When g.intro.Request is also non-nil (v0.219), emit a sibling
// buildBedrockStreamRequestFn closure that calls BAML's non-streaming
// Request.<Method> (because BAML's StreamRequest errors for
// aws-bedrock), mutates the URL from /converse to /converse-stream,
// sets the AWS event-stream Accept header, and attaches AWSAuth. Wire
// it into StreamConfig as BuildBedrockStreamRequest. The orchestrator
// dispatches on provider.
func (me *methodEmitter) emitBuildRequest() {
	g := me.g
	if g.intro.StreamRequest == nil {
		return
	}
	out := g.out
	streamResultInterface := jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")

	// Build the StreamRequest call params using callOpts (which may
	// include a WithClient override for fallback chains).
	var buildRequestCallParams []jen.Code
	buildRequestCallParams = append(buildRequestCallParams, jen.Id("ctx"))
	for _, arg := range me.args {
		buildRequestCallParams = append(buildRequestCallParams, me.argCallParam(arg))
	}
	buildRequestCallParams = append(buildRequestCallParams, jen.Id("callOpts").Op("..."))

	// Identical call-param shape for BAML's non-streaming
	// Request.<Method>. Same args + opts as StreamRequest because both
	// consume the per-method signature. Built unconditionally; only
	// inlined into the closure when g.intro.Request is non-nil
	// (the v0.219 gate).
	var bedrockStreamCallParams []jen.Code
	bedrockStreamCallParams = append(bedrockStreamCallParams, jen.Id("ctx"))
	for _, arg := range me.args {
		bedrockStreamCallParams = append(bedrockStreamCallParams, me.argCallParam(arg))
	}
	bedrockStreamCallParams = append(bedrockStreamCallParams, jen.Id("callOpts").Op("..."))

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
			jen.Op("*").Qual(g.pkgs.LLMHTTPPkg, "Request"),
			jen.Error(),
		).BlockFunc(func(jg *jen.Group) {
			// Build callOpts: clone options and append WithClient if override is set
			jg.Id("callOpts").Op(":=").Id("options")
			jg.Add(g.withClientCloneAndAppend("callOpts", "options"))
			// Call StreamRequest.Method(ctx, args..., callOpts...)
			jg.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
				Qual(g.pkgs.GeneratedClientPkg, "StreamRequest").Dot(me.methodName).Call(buildRequestCallParams...)
			jg.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			)
			emitBAMLHTTPRequestConversion(jg, g.pkgs, nil)
		}),
	)

	// Bedrock streaming uses BAML's non-streaming modular
	// Request.<Method> as the body assembler (StreamRequest errors for
	// aws-bedrock per upstream), then mutates URL /converse →
	// /converse-stream, sets the AWS event-stream Accept header, and
	// attaches AWSAuth. The orchestrator dispatches to this closure
	// when the per-attempt provider is aws-bedrock.
	//
	// Gated on g.intro.Request being non-nil because the symbol
	// baml_client.Request only exists on BAML v0.219+. For v0.204 /
	// v0.215 the streaming bedrock path falls through to legacy via
	// the codegen-time gating elsewhere (callSupportedProviders /
	// supportedProviders are still consulted at runtime, but
	// resolution treats unsupported codegen as legacy).
	if g.intro.Request != nil {
		buildRequestBody = append(buildRequestBody,
			jen.Id("buildBedrockStreamRequestFn").Op(":=").Func().Params(
				jen.Id("ctx").Qual("context", "Context"),
				jen.Id("clientOverride").String(),
			).Params(
				jen.Op("*").Qual(g.pkgs.LLMHTTPPkg, "Request"),
				jen.Error(),
			).BlockFunc(func(jg *jen.Group) {
				jg.Id("callOpts").Op(":=").Id("options")
				jg.Add(g.withClientCloneAndAppend("callOpts", "options"))
				// Call Request.Method(ctx, args..., callOpts...) —
				// the non-streaming body assembler.
				jg.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
					Qual(g.pkgs.GeneratedClientPkg, "Request").Dot(me.methodName).Call(bedrockStreamCallParams...)
				jg.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				)
				emitBAMLHTTPRequestConversion(jg, g.pkgs, emitBedrockStreamPostProcessFor(me.methodName, g.pkgs))
			}),
		)
	}

	buildRequestBody = append(buildRequestBody,

		// parseStreamFn: calls ParseStream.Method(ctx, accumulated, opts...)
		// The context_fix hack adds ctx as first param to ParseStream methods.
		jen.Id("parseStreamFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("accumulated").String(),
		).Params(jen.Any(), jen.Error()).Block(
			jen.Return(
				jen.Qual(g.pkgs.GeneratedClientPkg, "ParseStream").Dot(me.methodName).Call(
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
				jen.Qual(g.pkgs.GeneratedClientPkg, "Parse").Dot(me.methodName).Call(
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
			jen.Id("kind").Qual(g.pkgs.InterfacesPkg, "StreamResultKind"),
			jen.Id("stream").Any(),
			jen.Id("final").Any(),
			jen.Id("raw").String(),
			jen.Id("reasoning").String(),
			jen.Id("err").Error(),
			jen.Id("reset").Bool(),
		).Params(jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")).Block(
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
				jen.Func().Params(jen.Id("onTick").Add(onTickType(g.pkgs.BamlPkg))).Params(jen.Any(), jen.Error()).Block(
					jen.Id("opts").Op(":=").Append(
						jen.Id("callOpts"),
						jen.Qual(g.pkgs.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
					),
					jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
						Qual(g.pkgs.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(legacyStreamCallParams...),
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
		jen.Id("streamConfig").Op(":=").Op("&").Qual(g.pkgs.BuildRequestPkg, "StreamConfig").Values(jen.Dict{
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
			// Wired only when g.intro.Request is non-nil (v0.219).
			// For v0.204/v0.215 the field stays nil and the
			// orchestrator's up-front validation rejects aws-bedrock
			// provider configurations on those adapters — matching the
			// codegen-time gating, since baml_client.Request doesn't
			// exist there.
			jen.Id("BuildBedrockStreamRequest"): func() jen.Code {
				if g.intro.Request != nil {
					return jen.Id("buildBedrockStreamRequestFn")
				}
				return jen.Nil()
			}(),
			jen.Id("MetadataPlan"): jen.Id("plannedMetadata"),
			jen.Id("NewMetadataResult"): jen.Func().Params(
				jen.Id("md").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
		}),

		// Run the orchestration in a goroutine with panic recovery, matching
		// the _noRaw/_full pattern. On panic, an error result is emitted so
		// the worker can surface it instead of crashing.
		// Resolve HTTP client: use adapter override if provided, else default
		jen.Id("__httpClient").Op(":=").Qual(g.pkgs.LLMHTTPPkg, "DefaultClient"),
		jen.If(
			jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
			jen.Id("__c").Op("!=").Nil(),
		).Block(
			jen.Id("__httpClient").Op("=").Id("__c"),
		),
	)

	// Seed_OmitAsyncDefer relocates the release defer to the outer
	// buildRequest function body — it fires when the outer function
	// returns, racing the orchestration goroutine's reads of
	// __struct_messages.
	if me.hasReleaseConverted && me.g.opts.Seed_OmitAsyncDefer {
		buildRequestBody = append(buildRequestBody, jen.Defer().Id("__releaseConverted").Call())
	}

	buildRequestBody = append(buildRequestBody,
		jen.Go().Func().Params().BlockFunc(func(grp *jen.Group) {
			grp.Defer().Close(jen.Id("out"))
			if me.hasReleaseConverted && !me.g.opts.Seed_OmitAsyncDefer {
				// Retry closures captured by RunStreamOrchestration
				// outlive the outer function, so the converted slice
				// release belongs inside the orchestration goroutine.
				grp.Defer().Id("__releaseConverted").Call()
			}
			grp.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
				jen.Func().Params(jen.Id("err").Error()).Block(
					jen.Id("__errR").Op(":=").Id("newResultFn").Call(
						jen.Qual(g.pkgs.InterfacesPkg, "StreamResultKindError"),
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
					jen.Return(jen.Qual(g.pkgs.BuildRequestPkg, "RunStreamOrchestration").Call(
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
			)
		}).Call(),
		jen.Return(jen.Nil()),
	)

	out.Func().
		Id(me.buildRequestMethodName).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("provider").String(),
			jen.Id("retryPolicy").Op("*").Qual(g.pkgs.RetryPkg, "Policy"),
			jen.Id("fallbackChain").Index().String(),
			jen.Id("clientProviders").Map(jen.String()).String(),
			jen.Id("legacyChildren").Map(jen.String()).Bool(),
			jen.Id("fallbackTargets").Map(jen.String()).String(),
			jen.Id("fallbackRoundRobin").Map(jen.String()).Op("*").Qual(g.pkgs.InterfacesPkg, "RoundRobinInfo"),
			jen.Id("plannedMetadata").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(buildRequestBody...)
}

// emitBuildCallRequest emits the non-streaming BuildRequest impl
// for this method (<method>_buildCallRequest) using BAML's Request
// API to build a non-streaming HTTP request, execute it, extract
// the LLM text from the JSON response, and parse. Only emitted when
// g.intro.Request is non-nil (BAML >= 0.219.0).
func (me *methodEmitter) emitBuildCallRequest() {
	g := me.g
	if g.intro.Request == nil {
		return
	}
	out := g.out
	streamResultInterface := jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")

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
			jen.Op("*").Qual(g.pkgs.LLMHTTPPkg, "Request"),
			jen.Error(),
		).BlockFunc(func(jg *jen.Group) {
			// Build callOpts: clone options and append WithClient if override is set
			jg.Id("callOpts").Op(":=").Id("options")
			jg.Add(g.withClientCloneAndAppend("callOpts", "options"))
			// Call Request.Method(ctx, args..., callOpts...) — non-streaming
			jg.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
				Qual(g.pkgs.GeneratedClientPkg, "Request").Dot(me.methodName).Call(callRequestCallParams...)
			jg.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			)
			// The call branch attaches AWS SigV4 metadata via the
			// client-aware dispatch. AttachBedrockAuthForClient
			// honours per-client `endpoint_url` + `region` overrides
			// (resolved through introspected.BedrockClientOptionsByName)
			// and falls through to MaybeAttachBedrockAuth's URL-
			// pattern detection for the default-endpoint case — so
			// the unconditional emit still costs every non-bedrock
			// provider nothing. The streaming branch uses its own
			// sibling closure (buildBedrockStreamRequestFn) with
			// emitBedrockStreamPostProcessFor — see emitBuildRequest above.
			emitBAMLHTTPRequestConversion(jg, g.pkgs, emitBedrockAuthDispatchFor(me.methodName, g.pkgs))
		}),

		// parseFinalFn: calls Parse.Method(ctx, text, opts...)
		jen.Id("parseFinalFn").Op(":=").Func().Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("text").String(),
		).Params(jen.Any(), jen.Error()).Block(
			jen.Return(
				jen.Qual(g.pkgs.GeneratedClientPkg, "Parse").Dot(me.methodName).Call(
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
			jen.Id("kind").Qual(g.pkgs.InterfacesPkg, "StreamResultKind"),
			jen.Id("stream").Any(), // unused in non-streaming path; kept for signature parity
			jen.Id("final").Any(),
			jen.Id("raw").String(),
			jen.Id("reasoning").String(),
			jen.Id("err").Error(),
			jen.Id("reset").Bool(),
		).Params(jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")).Block(
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
				jen.Func().Params(jen.Id("onTick").Add(onTickType(g.pkgs.BamlPkg))).Params(jen.Any(), jen.Error()).Block(
					jen.Id("opts").Op(":=").Append(
						jen.Id("callOpts"),
						jen.Qual(g.pkgs.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
					),
					jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
						Qual(g.pkgs.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(legacyCallStreamCallParams...),
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
		jen.Id("callConfig").Op(":=").Op("&").Qual(g.pkgs.BuildRequestPkg, "CallConfig").Values(jen.Dict{
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
			// BuildRequestConfig threads this handler's per-instance
			// settings through so the orchestrator's pre-attempt
			// validation honours DisableCallBuildRequest without
			// going through the env-cached global.
			jen.Id("BuildRequestConfig"): jen.Id("adapter").Dot("BuildRequestConfig").Call(),
			jen.Id("NewMetadataResult"): jen.Func().Params(
				jen.Id("md").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
		}),

		// Resolve HTTP client
		jen.Id("__httpClient").Op(":=").Qual(g.pkgs.LLMHTTPPkg, "DefaultClient"),
		jen.If(
			jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
			jen.Id("__c").Op("!=").Nil(),
		).Block(
			jen.Id("__httpClient").Op("=").Id("__c"),
		),
	)

	// Seed_OmitAsyncDefer relocates the release defer to the outer
	// buildCallRequest function body so it fires when the outer
	// function returns, racing the orchestration goroutine's reads.
	if me.hasReleaseConverted && me.g.opts.Seed_OmitAsyncDefer {
		buildCallRequestBody = append(buildCallRequestBody, jen.Defer().Id("__releaseConverted").Call())
	}

	buildCallRequestBody = append(buildCallRequestBody,
		// Run in goroutine with panic recovery
		jen.Go().Func().Params().BlockFunc(func(grp *jen.Group) {
			grp.Defer().Close(jen.Id("out"))
			if me.hasReleaseConverted && !me.g.opts.Seed_OmitAsyncDefer {
				// Retry closures captured by RunCallOrchestration
				// outlive the outer function, so the converted slice
				// release belongs inside the orchestration goroutine.
				grp.Defer().Id("__releaseConverted").Call()
			}
			grp.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
				jen.Func().Params(jen.Id("err").Error()).Block(
					jen.Id("__errR").Op(":=").Id("newResultFn").Call(
						jen.Qual(g.pkgs.InterfacesPkg, "StreamResultKindError"),
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
					jen.Return(jen.Qual(g.pkgs.BuildRequestPkg, "RunCallOrchestration").Call(
						jen.Id("adapter"),
						jen.Id("out"),
						jen.Id("callConfig"),
						jen.Id("__httpClient"),
						jen.Id("buildRequestFn"),
						jen.Id("parseFinalFn"),
						jen.Qual(g.pkgs.BuildRequestPkg, "ExtractResponseContent"),
						jen.Qual(g.pkgs.BuildRequestPkg, "ExtractResponseContentBytes"),
						jen.Id("newResultFn"),
					)),
				),
			)
		}).Call(),
		jen.Return(jen.Nil()),
	)

	out.Func().
		Id(me.buildCallRequestMethodName).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("provider").String(),
			jen.Id("retryPolicy").Op("*").Qual(g.pkgs.RetryPkg, "Policy"),
			jen.Id("fallbackChain").Index().String(),
			jen.Id("clientProviders").Map(jen.String()).String(),
			jen.Id("legacyChildren").Map(jen.String()).Bool(),
			jen.Id("fallbackTargets").Map(jen.String()).String(),
			jen.Id("fallbackRoundRobin").Map(jen.String()).Op("*").Qual(g.pkgs.InterfacesPkg, "RoundRobinInfo"),
			jen.Id("plannedMetadata").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(buildCallRequestBody...)
}
