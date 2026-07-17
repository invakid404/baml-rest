package codegen

import (
	"github.com/dave/jennifer/jen"
	"github.com/stoewer/go-strcase"
)

// emitLegacyStream emits the legacy CallStream+OnTick streaming
// impls for this method: <method>_noRaw (BAML's native streaming
// without raw collection) and <method>_full (the SSE-extractor +
// ParseStream goroutine variant). Both are invoked from the public
// router's legacy-fallthrough switch via __legacyClientOverride.
func (me *methodEmitter) emitLegacyStream() {
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")

	// Build call parameters for Stream method
	// ====== SIMPLIFIED IMPLEMENTATION: methodName_noRaw ======
	// This path uses BAML's native streaming without raw collection overhead.
	// Partials come directly from BAML's stream, no OnTick/SSE parsing needed.

	// The stream-driving body is factored into noRawStreamBody so the de-BAML
	// direct-legacy probe (emitDirectLegacyCall) reuses the EXACT same body on a
	// native DECLINE — guaranteeing byte-identical error/final/heartbeat/partial
	// frame behaviour with the ordinary legacy path.
	noRawGoroutineBody := me.noRawStreamBody()

	// _noRaw is the top-level legacy streaming impl invoked from
	// the BuildRequest fallthrough branch (see __legacyClientOverride
	// dispatch sites below). Use the legacy registry view so
	// runtime strategy-parent overrides are visible to BAML.
	noRawBody := me.makeLegacyPreamble()

	// runNoRawOrchestration spawns its own goroutine and returns
	// immediately. The body closure below is what actually consumes
	// __struct_messages, and it runs inside that orchestration
	// goroutine — so the pooled-slice release MUST live inside the
	// body closure, not at the outer function level (where the defer
	// would fire before the orchestrator even read the slice).
	//
	// Seed_OmitAsyncDefer relocates the defer to the outer
	// noRawBody. When _noRaw returns, the defer fires while the
	// orchestrator's goroutine is still reading __struct_messages —
	// the timing-race shape backing the async-defer regression in
	// the pool-lifecycle harness.
	if me.hasReleaseConverted {
		if me.g.opts.Seed_OmitAsyncDefer {
			noRawBody = append(noRawBody, jen.Defer().Id("__releaseConverted").Call())
		} else {
			noRawGoroutineBody = append([]jen.Code{jen.Defer().Id("__releaseConverted").Call()}, noRawGoroutineBody...)
		}
	}

	// Delegate to shared orchestration helper - supplies per-method closures
	noRawBody = append(noRawBody,
		jen.Return(jen.Id("runNoRawOrchestration").Call(
			jen.Id("adapter"),
			jen.Id("out"),
			// newHeartbeat
			jen.Func().Params().Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindHeartbeat"),
				jen.Return(jen.Id("__r")),
			),
			// newError
			jen.Func().Params(jen.Id("err").Error()).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.errorConstructorName).Call(jen.Id("err"))),
			),
			// release
			jen.Func().Params(jen.Id("__r").Qual(g.pkgs.InterfacesPkg, "StreamResult")).Block(
				jen.Id("__r").Dot("Release").Call(),
			),
			// plannedMetadata: passed straight through; orchestrator
			// builds both planned and outcome events from this seed.
			jen.Id("plannedMetadata"),
			// newMetadataResult: pool-wraps a Metadata payload.
			jen.Func().Params(jen.Id("md").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata")).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
			// body - receives beforeFinal + onTick, handles stream creation and iteration
			jen.Func().Params(jen.Id("beforeFinal").Func().Params(), jen.Id("onTick").Add(onTickType(g.pkgs.BamlPkg))).Error().Block(noRawGoroutineBody...),
		)),
	)

	// Generate the noRaw implementation function
	// Accepts output channel from caller and skipPartials flag, returns only error
	out.Func().
		Id(me.noRawMethodName).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("skipPartials").Bool(),
			jen.Id("plannedMetadata").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(noRawBody...)

	// ====== FULL IMPLEMENTATION: methodName_full ======
	// This path uses the ParseStream goroutine for partial results

	// Build the per-tick processing closure for the full path.
	// This captures method-specific state (options, adapter, output pool, ParseStream call).
	processTickBody := []jen.Code{
		jen.Id("calls").Op(",").Id("callsErr").Op(":=").Id("funcLog").Dot("Calls").Call(),
		jen.If(jen.Id("callsErr").Op("!=").Nil()).Block(jen.Return(jen.Nil())),
		jen.Id("callCount").Op(":=").Len(jen.Id("calls")),
		jen.If(jen.Id("callCount").Op("==").Lit(0)).Block(jen.Return(jen.Nil())),
		jen.Id("lastCall").Op(":=").Id("calls").Index(jen.Id("callCount").Op("-").Lit(1)),
		jen.Id("streamCall").Op(",").Id("ok").Op(":=").Id("lastCall").Assert(jen.Qual(g.pkgs.BamlPkg, "LLMStreamCall")),
		jen.If(jen.Op("!").Id("ok")).Block(jen.Return(jen.Nil())),
		jen.Id("provider").Op(",").Id("provErr").Op(":=").Id("streamCall").Dot("Provider").Call(),
		jen.If(jen.Id("provErr").Op("!=").Nil()).Block(jen.Return(jen.Nil())),
		// Meta-providers like "baml-fallback" are not handled by
		// ExtractDeltaFromText. Resolve the actual child provider using
		// the same precedence as BuildRequest's resolveChildProvider:
		//   1. Scan child calls for a supported runtime-reported provider
		//   2. Runtime client_registry override for the selected child
		//   3. Static g.intro.ClientProvider map (only if supported)
		// If none match, leave `provider` unchanged (ExtractDeltaFromText
		// will fail the unsupported-provider check and skip extraction
		// rather than using a stale or unsupported value).
		jen.If(jen.Op("!").Qual(g.pkgs.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("provider"))).Block(
			jen.Id("resolved").Op(":=").Lit(false),
			// 1. Prefer the runtime-reported provider from a child call.
			jen.For(jen.Id("i").Op(":=").Id("callCount").Op("-").Lit(1), jen.Id("i").Op(">=").Lit(0).Op("&&").Op("!").Id("resolved"), jen.Id("i").Op("--")).Block(
				jen.If(
					jen.List(jen.Id("sc"), jen.Id("scOk")).Op(":=").Id("calls").Index(jen.Id("i")).Assert(jen.Qual(g.pkgs.BamlPkg, "LLMStreamCall")),
					jen.Id("scOk"),
				).Block(
					jen.If(
						jen.List(jen.Id("cp"), jen.Id("cpErr")).Op(":=").Id("sc").Dot("Provider").Call(),
						jen.Id("cpErr").Op("==").Nil().Op("&&").Qual(g.pkgs.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("cp")),
					).Block(
						jen.Id("provider").Op("=").Id("cp"),
						jen.Id("resolved").Op("=").Lit(true),
					),
				),
			),
			jen.If(jen.Op("!").Id("resolved")).Block(
				jen.If(
					jen.List(jen.Id("clientName"), jen.Id("cnErr")).Op(":=").Id("streamCall").Dot("ClientName").Call(),
					jen.Id("cnErr").Op("==").Nil().Op("&&").Id("clientName").Op("!=").Lit(""),
				).Block(
					// 2. Runtime client_registry override for this client name.
					jen.If(
						jen.Id("reg").Op(":=").Id("adapter").Dot("OriginalClientRegistry").Call(),
						jen.Id("reg").Op("!=").Nil(),
					).Block(
						jen.For(jen.Id("_").Op(",").Id("rc").Op(":=").Range().Id("reg").Dot("Clients")).Block(
							jen.If(
								jen.Id("rc").Op("!=").Nil().Op("&&").Id("rc").Dot("Name").Op("==").Id("clientName").Op("&&").Id("rc").Dot("Provider").Op("!=").Lit("").Op("&&").Qual(g.pkgs.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("rc").Dot("Provider")),
							).Block(
								jen.Id("provider").Op("=").Id("rc").Dot("Provider"),
								jen.Id("resolved").Op("=").Lit(true),
								jen.Break(),
							),
						),
					),
					// 3. Static g.intro.ClientProvider (only if supported).
					jen.If(jen.Op("!").Id("resolved")).Block(
						jen.If(
							jen.List(jen.Id("sp"), jen.Id("spOk")).Op(":=").Qual(g.pkgs.IntrospectedPkg, "ClientProvider").Index(jen.Id("clientName")),
							jen.Id("spOk").Op("&&").Qual(g.pkgs.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("sp")),
						).Block(
							jen.Id("provider").Op("=").Id("sp"),
						),
					),
				),
			),
		),
		jen.Id("chunks").Op(",").Id("chunksErr").Op(":=").Id("streamCall").Dot("SSEChunks").Call(),
		jen.If(jen.Id("chunksErr").Op("!=").Nil()).Block(jen.Return(jen.Nil())),
		// Extract incrementally (no interface boxing).
		// Defer unlock so a panic in ExtractFrom does not leave the mutex held.
		jen.Id("extractorMu").Dot("Lock").Call(),
		jen.Defer().Id("extractorMu").Dot("Unlock").Call(),
		jen.Id("extractResult").Op(":=").Qual(g.pkgs.SSEPkg, "ExtractFrom").Call(
			jen.Id("extractor"), jen.Id("callCount"), jen.Id("provider"), jen.Id("chunks"),
		),
		jen.If(jen.Id("skipIntermediateParsing")).Block(jen.Return(jen.Nil())),
		// Skip if absolutely nothing new arrived this tick (no delta on
		// any buffer) and no reset occurred. Reasoning is checked too —
		// under IncludeReasoning=true a reasoning-only event has empty
		// Parseable and Raw, so the gate must consider ReasoningDelta to
		// avoid dropping that frame before it reaches the wire.
		jen.If(
			jen.Id("extractResult").Dot("ParseableDelta").Op("==").Lit("").
				Op("&&").Id("extractResult").Dot("RawDelta").Op("==").Lit("").
				Op("&&").Id("extractResult").Dot("ReasoningDelta").Op("==").Lit("").
				Op("&&").Op("!").Id("extractResult").Dot("Reset"),
		).Block(jen.Return(jen.Nil())),
		// parseable is the cumulative text-only buffer fed to ParseStream.
		// Reasoning content (e.g. Anthropic thinking_delta under
		// IncludeReasoning=true) is excluded by construction in
		// IncrementalExtractor.
		jen.Id("parseable").Op(":=").Id("extractResult").Dot("ParseableFull"),
		jen.Id("parseableDelta").Op(":=").Id("extractResult").Dot("ParseableDelta"),
		// rawDelta is the per-tick wire-output content (text-only by
		// construction). reasoningDelta is the per-tick reasoning text —
		// populated only under IncludeReasoning=true.
		jen.Id("rawDelta").Op(":=").Id("extractResult").Dot("RawDelta"),
		jen.Id("reasoningDelta").Op(":=").Id("extractResult").Dot("ReasoningDelta"),
		// Raw-only / reasoning-only / reset-only path. Triggered when:
		//   - No parseable content has accumulated yet (e.g., the only
		//     events seen so far are thinking_delta under opt-in).
		//   - No new parseable content this tick.
		//   - A reset boundary occurred but parseable is empty.
		//
		// We cannot meaningfully call ParseStream, but we still emit a
		// partial so the wire's raw + reasoning buffers accumulate and any
		// reset boundary reaches the client.
		jen.If(
			jen.Id("parseable").Op("==").Lit("").
				Op("||").Id("parseableDelta").Op("==").Lit(""),
		).Block(
			jen.Select().Block(
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
				jen.Default().Block(),
			),
			jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindStream"),
			jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
			jen.Id("__r").Dot("reasoning").Op("=").Id("reasoningDelta"),
			jen.Id("__r").Dot("reset").Op("=").Id("extractResult").Dot("Reset"),
			// Reset-bearing partials must not be dropped: block until sent or cancelled.
			jen.If(jen.Id("extractResult").Dot("Reset")).Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__r").Dot("Release").Call(),
					),
				),
			).Else().Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Default().Block(jen.Id("__r").Dot("Release").Call()),
				),
			),
			jen.Return(jen.Nil()),
		),
		// Call ParseStream on the cumulative parseable buffer.
		jen.List(jen.Id("parsed"), jen.Id("parseErr")).Op(":=").
			Qual(g.pkgs.GeneratedClientPkg, "ParseStream").Dot(me.methodName).Call(
			jen.Id("adapter"), jen.Id("parseable"), jen.Id("options").Op("..."),
		),
		jen.If(jen.Id("parseErr").Op("==").Nil()).Block(
			jen.Select().Block(
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
				jen.Default().Block(),
			),
			jen.Id("parsedPtr").Op(":=").Op("&").Id("parsed"),
			func() jen.Code {
				if me.isDynamicStream {
					return jen.Id(me.unwrapStreamFuncName).Call(jen.Id("parsedPtr"))
				}
				return jen.Null()
			}(),
			jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindStream"),
			jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
			jen.Id("__r").Dot("reasoning").Op("=").Id("reasoningDelta"),
			jen.Id("__r").Dot("streamParsed").Op("=").Id("parsedPtr"),
			jen.Id("__r").Dot("reset").Op("=").Id("extractResult").Dot("Reset"),
			// Reset-bearing partials must not be dropped: block until sent or cancelled.
			jen.If(jen.Id("extractResult").Dot("Reset")).Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__r").Dot("Release").Call(),
					),
				),
			).Else().Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Default().Block(jen.Id("__r").Dot("Release").Call()),
				),
			),
		).Else().If(jen.Id("extractResult").Dot("Reset")).Block(
			// ParseStream failed on a rebuild tick. The reset boundary must still
			// reach the client so it discards stale state, even though we have no
			// parsed content to send. Emit a raw-only/reset-only partial.
			jen.Select().Block(
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
				jen.Default().Block(),
			),
			jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindStream"),
			jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
			jen.Id("__r").Dot("reasoning").Op("=").Id("reasoningDelta"),
			jen.Id("__r").Dot("reset").Op("=").Lit(true),
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
					jen.Id("__r").Dot("Release").Call(),
				),
			),
		),
		jen.Return(jen.Nil()),
	}

	// Build call parameters for the driveStream closure.
	// The closure receives a pre-built opts slice (already contains WithOnTick)
	// from the orchestration helper. We append WithClient(clientOverride) when
	// the router resolved a specific leaf client (e.g. baml-roundrobin).
	var driveStreamCallParams []jen.Code
	driveStreamCallParams = append(driveStreamCallParams, jen.Id("adapter"))
	for _, arg := range me.args {
		driveStreamCallParams = append(driveStreamCallParams, me.argCallParam(arg))
	}
	driveStreamCallParams = append(driveStreamCallParams, jen.Id("driveOpts").Op("..."))

	// Build the driveStream closure: creates the BAML stream, iterates, returns (finalResult, lastError).
	driveStreamBody := []jen.Code{
		jen.Id("driveOpts").Op(":=").Id("opts"),
		g.withClientCloneAndAppend("driveOpts", "opts"),
		jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
			Qual(g.pkgs.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(driveStreamCallParams...),
		jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
			jen.Return(jen.Nil(), jen.Id("streamErr")),
		),
		jen.Var().Id("result").Any(),
		jen.Var().Id("lastErr").Error(),
		jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
			jen.If(jen.Id("streamVal").Dot("IsError")).Block(jen.Id("lastErr").Op("=").Id("streamVal").Dot("Error"), jen.Continue()),
			jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call()),
		),
		jen.Return(jen.Id("result"), jen.Id("lastErr")),
	}

	// Build the emitFinal closure: wraps final result with type-specific handling.
	// Sets the typed finalParsed field directly — no interface boxing.
	emitFinalBody := []jen.Code{
		jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
		jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindFinal"),
		jen.Id("__r").Dot("raw").Op("=").Id("raw"),
		jen.Id("__r").Dot("reasoning").Op("=").Id("reasoning"),
		jen.If(jen.Id("result").Op("!=").Nil()).Block(
			jen.If(
				jen.List(jen.Id("ptr"), jen.Id("ok")).Op(":=").Id("result").Assert(me.finalTypePtr.Clone()),
				jen.Id("ok"),
			).Block(
				func() jen.Code {
					if me.isDynamicFinal {
						return jen.Id(me.unwrapFinalFuncName).Call(jen.Id("ptr"))
					}
					return jen.Null()
				}(),
				jen.Id("__r").Dot("finalParsed").Op("=").Id("ptr"),
			).Else().If(
				jen.List(jen.Id("val"), jen.Id("ok")).Op(":=").Id("result").Assert(me.finalType.statement.Clone()),
				jen.Id("ok"),
			).Block(
				func() jen.Code {
					if me.isDynamicFinal {
						return jen.Id(me.unwrapFinalFuncName).Call(jen.Op("&").Id("val"))
					}
					return jen.Null()
				}(),
				jen.Id("__r").Dot("finalParsed").Op("=").Op("&").Id("val"),
			),
		),
		jen.Return(jen.Id("__r")),
	}

	// _full is the top-level legacy streaming impl with full raw
	// collection, also invoked from the BuildRequest fallthrough.
	// Same legacy-view rationale as _noRaw.
	var fullBody []jen.Code
	fullBody = append(fullBody, me.makeLegacyPreamble()...)
	if me.hasReleaseConverted {
		// runFullOrchestration spawns its own goroutines and returns
		// immediately. driveStream is the only callback that touches
		// __struct_messages, and it runs inside the orchestrator
		// goroutine — so the pooled-slice release MUST live inside
		// driveStream's body, not at the outer function level (where
		// the defer would fire before the orchestrator even started
		// the stream). Mirror of the _noRaw fix.
		//
		// Seed_OmitAsyncDefer relocates the defer to the outer
		// fullBody so it fires when _full returns, racing the
		// driveStream callback still reading the converted slice.
		if me.g.opts.Seed_OmitAsyncDefer {
			fullBody = append(fullBody, jen.Defer().Id("__releaseConverted").Call())
		} else {
			driveStreamBody = append([]jen.Code{jen.Defer().Id("__releaseConverted").Call()}, driveStreamBody...)
		}
	}

	// Delegate to shared full orchestration helper with per-method closures
	fullBody = append(fullBody,
		jen.Return(jen.Id("runFullOrchestration").Call(
			jen.Id("adapter"),
			jen.Id("out"),
			jen.Id("options"),
			// newHeartbeat
			jen.Func().Params().Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindHeartbeat"),
				jen.Return(jen.Id("__r")),
			),
			// newError: error envelope carries the accumulated raw text
			// from runFullOrchestration (per #256) so the worker bridge
			// can forward it as details.raw.
			jen.Func().Params(jen.Id("err").Error(), jen.Id("raw").String()).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Id("__r").Op(":=").Id(me.errorConstructorName).Call(jen.Id("err")),
				jen.Id("__r").Dot("raw").Op("=").Id("raw"),
				jen.Return(jen.Id("__r")),
			),
			// release
			jen.Func().Params(jen.Id("__r").Qual(g.pkgs.InterfacesPkg, "StreamResult")).Block(
				jen.Id("__r").Dot("Release").Call(),
			),
			// plannedMetadata: passed straight through; orchestrator
			// builds both planned and outcome events from this seed.
			jen.Id("plannedMetadata"),
			// newMetadataResult: pool-wraps a Metadata payload.
			jen.Func().Params(jen.Id("md").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata")).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
			),
			// processTick
			jen.Func().Params(
				jen.Id("funcLog").Qual(g.pkgs.BamlPkg, "FunctionLog"),
				jen.Id("extractor").Op("*").Qual(g.pkgs.SSEPkg, "IncrementalExtractor"),
				jen.Id("extractorMu").Op("*").Qual("sync", "Mutex"),
			).Error().Block(processTickBody...),
			// driveStream
			jen.Func().Params(
				jen.Id("opts").Op("[]").Qual(g.pkgs.GeneratedClientPkg, "CallOptionFunc"),
			).Params(jen.Any(), jen.Error()).Block(driveStreamBody...),
			// emitFinal
			jen.Func().Params(jen.Id("result").Any(), jen.Id("raw").String(), jen.Id("reasoning").String()).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(emitFinalBody...),
		)),
	)

	// Generate the full implementation function
	// Accepts output channel from caller and skipIntermediateParsing flag, returns only error
	out.Func().
		Id(me.fullMethodName).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("skipIntermediateParsing").Bool(),
			jen.Id("plannedMetadata").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(fullBody...)
}

// --- de-BAML direct-legacy native-first probe (mprov S1) ---------------------
// These live here (next to emitLegacyStream) rather than in a separate file so
// the shared noRawStreamBody and the probe emitter are always compiled together
// with the ordinary legacy path.

// noRawStreamBody returns the shared legacy noRaw stream-driving statements: it
// builds the BAML stream (WithOnTick), forwards every IsError frame and keeps
// draining, emits the final (after beforeFinal's outcome metadata), and forwards
// partials unless skipPartials. Factored out of emitLegacyStream so the de-BAML
// DIRECT-LEGACY probe (emitDirectLegacyCall) reuses the EXACT same body on a
// native DECLINE — guaranteeing byte-identical result/error/heartbeat/raw/
// metadata behaviour with the ordinary direct legacy path (only planned_engine
// differs). It references skipPartials / onTick / beforeFinal from the enclosing
// scope, which BOTH callers provide.
func (me *methodEmitter) noRawStreamBody() []jen.Code {
	g := me.g

	var noRawStreamCallParams []jen.Code
	noRawStreamCallParams = append(noRawStreamCallParams, jen.Id("adapter")) // context
	for _, arg := range me.args {
		noRawStreamCallParams = append(noRawStreamCallParams, me.argCallParam(arg))
	}
	noRawStreamCallParams = append(noRawStreamCallParams,
		jen.Id("streamOpts").Op("..."),
	)

	return []jen.Code{
		// Build streamOpts: base options + WithOnTick heartbeat; append
		// WithClient(clientOverride) when the router passed a resolved leaf
		// client (e.g. baml-roundrobin selected a specific child).
		//
		// The WithClient append clones streamOpts first, matching the
		// driveStream pattern. Without the clone, streamOpts shares its
		// backing array with `options` (the first append may or may not
		// have reallocated, depending on capacity), and appending
		// WithClient could mutate the caller's slice — stomping the
		// options list for any sibling request that held the same
		// backing array.
		jen.Id("streamOpts").Op(":=").Append(
			jen.Id("options"),
			jen.Qual(g.pkgs.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
		),
		g.withClientCloneAndAppend("streamOpts", "streamOpts"),
		// Call Stream WITH OnTick for heartbeat tracking, but still use native streaming for data
		jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
			Qual(g.pkgs.GeneratedClientPkg, "Stream").Dot(me.methodName).Call(noRawStreamCallParams...),

		// If stream creation failed, emit error
		jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
			jen.Id("__errR").Op(":=").Id(me.errorConstructorName).Call(jen.Id("streamErr")),
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
					jen.Id("__errR").Dot("Release").Call(),
				),
			),
			jen.Return(jen.Nil()),
		),

		// Process BAML's stream - forward partials and final
		jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
			// Check context cancellation at start of each iteration
			jen.Select().Block(
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
					jen.Return(jen.Nil()),
				),
				jen.Default().Block(),
			),

			// Handle errors
			jen.If(jen.Id("streamVal").Dot("IsError")).Block(
				jen.Id("__errR").Op(":=").Id(me.errorConstructorName).Call(jen.Id("streamVal").Dot("Error")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__errR").Dot("Release").Call(),
						jen.Return(jen.Nil()),
					),
				),
				jen.Continue(),
			),

			// Handle final result
			// Final() already returns *TFinal from the BAML generated client — assign directly.
			jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
				jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindFinal"),
				jen.Id("__r").Dot("finalParsed").Op("=").Id("streamVal").Dot("Final").Call(),
				func() jen.Code {
					if me.isDynamicFinal {
						return jen.Id(me.unwrapFinalFuncName).Call(jen.Id("__r").Dot("finalParsed"))
					}
					return jen.Null()
				}(),
				// Emit outcome metadata before sending Final, so it lands
				// between the last partial and the terminal payload —
				// matching the BuildRequest path's contract.
				jen.Id("beforeFinal").Call(),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__r").Dot("Release").Call(),
						jen.Return(jen.Nil()),
					),
				),
				jen.Continue(),
			),

			// Handle partial - forward BAML's native partial via Stream() (only if not skipping partials)
			// Stream() already returns *TStream from the BAML generated client — assign directly.
			jen.If(jen.Op("!").Id("skipPartials")).Block(
				jen.If(jen.Id("__partial").Op(":=").Id("streamVal").Dot("Stream").Call(), jen.Id("__partial").Op("!=").Nil()).Block(
					func() jen.Code {
						if me.isDynamicStream {
							return jen.Id(me.unwrapStreamFuncName).Call(jen.Id("__partial"))
						}
						return jen.Null()
					}(),
					jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindStream"),
					jen.Id("__r").Dot("streamParsed").Op("=").Id("__partial"),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Id("__r").Dot("Release").Call(),
							jen.Return(jen.Nil()),
						),
						jen.Default().Block(
							jen.Id("__r").Dot("Release").Call(),
						), // Non-blocking send for partials - release if not sent
					),
				),
			),
		),

		jen.Return(jen.Nil()),
	}
}

// directLegacyCallMethodName is the de-BAML direct-legacy native-first probe
// entrypoint (e.g. bamlRestDynamicDirectLegacyCall). The router dispatches to it
// for a direct single unary-call leaf whose existing BAML route is legacy, only
// when the umbrella flag is on and a SERVE callback is installed.
func (me *methodEmitter) directLegacyCallMethodName() string {
	return strcase.LowerCamelCase(me.methodName + "_directLegacyCall")
}

// emitDirectLegacyCall emits the de-BAML DIRECT-LEGACY native-first probe
// (mprov S1). Emitted ONLY for the de-BAML dynamic method. It runs the native
// serve callback FIRST; on a native DECLINE it runs the EXACT ordinary legacy
// serving lifecycle by reusing runNoRawOrchestration (unchanged) with the SAME
// noRawStreamBody, so result/error/heartbeat/raw/metadata are byte-identical to
// bamlRest…NoRaw — only planned_engine=native differs (added on the OUTCOME frame
// by the metadata interceptor). On native SUCCESS it serves the native final; on
// native FAILURE it emits the typed error. There is no BuildRequest path behind
// it (BuildBAMLRequest is nil in the serve request): a non-openai leaf
// mapping-declines, and an OpenAI leaf forced to legacy declines strict
// verification (no BAML plan) — both fall through to the ordinary legacy stream.
func (me *methodEmitter) emitDirectLegacyCall() {
	if !me.isDeBAMLMethod() {
		return
	}
	g := me.g
	out := g.out
	streamResultInterface := jen.Qual(g.pkgs.InterfacesPkg, "StreamResult")

	convertedVar := me.deBAMLConvertedVar()

	// Message conversion + legacy options (options, __struct_messages,
	// __releaseConverted). The direct-legacy probe is plain unary call, so
	// skipPartials is always true.
	body := me.makeLegacyPreamble()
	body = append(body, jen.Id("skipPartials").Op(":=").True())

	// Resolve the effective send client for the WouldRewriteOrProxy predicate the
	// admission strategy gate evaluates against the effective target.
	body = append(body,
		jen.Id("__httpClient").Op(":=").Qual(g.pkgs.LLMHTTPPkg, "DefaultClient"),
		jen.If(
			jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
			jen.Id("__c").Op("!=").Nil(),
		).Block(
			jen.Id("__httpClient").Op("=").Id("__c"),
		),
	)

	// Resolve the SERVE implementation (the router already gated hasNativeServe,
	// so it is non-nil in practice; a nil serve safely runs the ordinary legacy
	// stream with no native probe).
	body = append(body,
		jen.Var().Id("serve").Qual(g.pkgs.InterfacesPkg, "NativeServeFunc"),
		jen.If(
			jen.List(jen.Id("__g"), jen.Id("__gok")).Op(":=").Id("adapter").Assert(jen.Id("nativeServeGetter")),
			jen.Id("__gok"),
		).Block(
			jen.Id("serve").Op("=").Id("__g").Dot("NativeServeComparator").Call(),
		),
	)

	// The metadata interceptor threads planned_engine=native onto the OUTCOME
	// frame ONLY (§9: serve-considered-then-declined), leaving the planned frame
	// and every legacy outcome field (winner, baml_call_count) byte-identical to
	// the ordinary path — that is beforeFinal's BuildLegacyOutcome result.
	newMetadataResultNative := jen.Func().Params(jen.Id("md").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata")).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
		jen.If(jen.Id("md").Op("!=").Nil().Op("&&").Id("md").Dot("Phase").Op("==").Qual(g.pkgs.InterfacesPkg, "MetadataPhaseOutcome")).Block(
			jen.Id("md").Dot("PlannedEngine").Op("=").Lit("native"),
		),
		jen.Return(jen.Id(me.metadataConstructorName).Call(jen.Id("md"))),
	)

	// The stream-driving body, run on a native DECLINE. Identical to the ordinary
	// legacy path by construction (shared noRawStreamBody).
	declineBody := me.noRawStreamBody()

	// wrappedBody: native probe first, then dispatch. On DECLINE it falls through
	// to the ordinary legacy stream (declineBody). __releaseConverted is deferred
	// at the top so it fires on every branch.
	var wrappedBody []jen.Code
	if me.hasReleaseConverted {
		wrappedBody = append(wrappedBody, jen.Defer().Id("__releaseConverted").Call())
	}
	wrappedBody = append(wrappedBody,
		jen.If(jen.Id("serve").Op("!=").Nil()).Block(
			// A first-2xx heartbeat frame for a native SERVE (idempotent). On a
			// pre-socket DECLINE it is never fired, so the heartbeat on the fall-
			// through leg comes solely from the ordinary stream's onTick — matching
			// the ordinary legacy path exactly.
			jen.Var().Id("__probeHB").Qual("sync/atomic", "Bool"),
			jen.Id("__sendHeartbeat").Op(":=").Func().Params().Block(
				jen.If(jen.Id("__probeHB").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.Id("__hb").Op(":=").Id(me.getterFuncName).Call(),
					jen.Id("__hb").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindHeartbeat"),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__hb")).Block(),
						jen.Default().Block(jen.Id("__hb").Dot("Release").Call()),
					),
				),
			),
			jen.Id("__res").Op(":=").Id("serve").Call(jen.Id("adapter"), jen.Qual(g.pkgs.InterfacesPkg, "NativeServeRequest").Values(jen.Dict{
				jen.Id("Registry"):         jen.Id("adapter").Dot("OriginalClientRegistry").Call(),
				jen.Id("Messages"):         jen.Id("nativeShadowMessages").Call(jen.Id(convertedVar)),
				jen.Id("OutputSchema"):     jen.Id("adapter").Dot("DeBAMLOutputSchema").Call(),
				jen.Id("Provider"):         jen.Id("provider"),
				jen.Id("ClientOverride"):   jen.Id("clientOverride"),
				jen.Id("Mode"):             jen.Qual(g.pkgs.InterfacesPkg, "NativeServeModeCall"),
				jen.Id("IncludeReasoning"): jen.Id("adapter").Dot("IncludeReasoning").Call(),
				jen.Id("SingleLeaf"):       jen.True(),
				jen.Id("HasFallbackChain"): jen.False(),
				jen.Id("HasRoundRobin"):    jen.False(),
				// Forwarded TRUTHFULLY from the router's strategy-aware
				// __legacyRetryPolicy (via the hasRequestRetryOverride param): a
				// direct single leaf can still carry a resolved retry policy the
				// single-attempt exact lane would bypass, so a resolved policy must
				// decline native admission PRE-SOCKET and keep BAML's retry semantics.
				// (SingleLeaf/HasFallbackChain/HasRoundRobin are literal because the
				// router only reaches this probe when Strategy/Chain/RoundRobin are all
				// empty; a retry override is INDEPENDENT of those.)
				jen.Id("HasRequestRetryOverride"): jen.Id("hasRequestRetryOverride"),
				jen.Id("WouldRewriteOrProxy"):     jen.Id("__httpClient").Dot("WouldRewriteOrProxy"),
				// The probe route has no BAML plan to compare (its BAML route is
				// legacy): a strict OpenAI leaf declines strict verification, a
				// non-openai leaf mapping-declines.
				jen.Id("BuildBAMLRequest"): jen.Nil(),
				jen.Id("SendHeartbeat"):    jen.Id("__sendHeartbeat"),
			})),
			jen.Switch(jen.Id("__res").Dot("Disposition")).Block(
				jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "NativeServeSucceeded")).Block(
					// Native served the request: emit the native outcome
					// (winner_path=legacy, winner_engine=native, planned_engine=native)
					// then the native final. (Unreachable in S1 — every non-openai
					// leaf mapping-declines before a socket; live once S2 adds the
					// mappers.)
					jen.List(jen.Id("__wrapped"), jen.Id("__werr")).Op(":=").Id("wrapDeBAMLDynamicOutput").Call(jen.Id("__res").Dot("FinalJSON")),
					jen.If(jen.Id("__werr").Op("!=").Nil()).Block(
						jen.Id("__errR").Op(":=").Id(me.errorConstructorName).Call(jen.Op("&").Qual(g.pkgs.BuildRequestPkg, "OutputParseError").Values(jen.Dict{jen.Id("Err"): jen.Id("__werr")})),
						// Carry the native-owned raw channel (res.Raw) as details.raw on the
						// wrap-failure terminal error — parity with the ordinary native-call
						// route's FailNativeCallWithRaw(&OutputParseError, res.Raw) in codegen_debaml.go.
						jen.Id("__errR").Dot("raw").Op("=").Id("__res").Dot("Raw"),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("__errR").Dot("Release").Call()),
						),
						jen.Return(jen.Nil()),
					),
					jen.If(jen.Id("plannedMetadata").Op("!=").Nil()).Block(
						jen.Id("__outcome").Op(":=").Qual(g.pkgs.InterfacesPkg, "BuildLegacyOutcome").Call(
							jen.Id("plannedMetadata"), jen.Lit(0),
							jen.Id("plannedMetadata").Dot("Client"), jen.Id("plannedMetadata").Dot("Provider"), jen.Nil(),
						),
						jen.If(jen.Id("__outcome").Op("!=").Nil()).Block(
							jen.Id("__outcome").Dot("WinnerEngine").Op("=").Id("__res").Dot("WinnerEngine"),
							jen.Id("__outcome").Dot("PlannedEngine").Op("=").Lit("native"),
							jen.Id("__om").Op(":=").Id(me.metadataConstructorName).Call(jen.Id("__outcome")),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__om")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("__om").Dot("Release").Call()),
							),
						),
					),
					jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindFinal"),
					jen.Id("__r").Dot("finalParsed").Op("=").Op("&").Id("__wrapped"),
					func() jen.Code {
						if me.isDynamicFinal {
							return jen.Id(me.unwrapFinalFuncName).Call(jen.Id("__r").Dot("finalParsed"))
						}
						return jen.Null()
					}(),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("__r").Dot("Release").Call()),
					),
					jen.Return(jen.Nil()),
				),
				jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "NativeServeFailed")).Block(
					// Post-claim native failure: the typed error is terminal. NEVER
					// fall through to a legacy resend. (Unreachable in S1.)
					jen.Id("__errR").Op(":=").Id(me.errorConstructorName).Call(jen.Id("__res").Dot("Err")),
					// Carry the native-owned raw diagnostic (res.RawDiagnostic) as details.raw
					// on the terminal native-failure error — parity with the ordinary native-call
					// route's FailNativeCallWithRaw(res.Err, res.RawDiagnostic) in codegen_debaml.go.
					jen.Id("__errR").Dot("raw").Op("=").Id("__res").Dot("RawDiagnostic"),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("__errR").Dot("Release").Call()),
					),
					jen.Return(jen.Nil()),
				),
				jen.Case(jen.Qual(g.pkgs.InterfacesPkg, "NativeServeDeclined")).Block(
					// Pre-socket decline (the S1 live path): the callback guarantees
					// NO provider socket occurred, so fall through to the ordinary
					// legacy stream below in the SAME iteration.
					jen.Comment("no-op: fall through to the ordinary legacy stream below"),
				),
				jen.Default().Block(
					// An out-of-contract disposition — NativeServeResult crosses a
					// public, integer-backed boundary — can NOT assert "no socket
					// occurred", so FAIL CLOSED with a terminal typed error and NEVER
					// run the legacy stream: a fall-through here could issue a hidden
					// SECOND same-child BAML request after the native callback may have
					// already opened one. Mirrors the fail-closed default in
					// maybeInstallNativeCall's serve dispatch.
					jen.Id("__errR").Op(":=").Id(me.errorConstructorName).Call(
						jen.Qual("fmt", "Errorf").Call(jen.Lit("native serve returned unknown disposition %d"), jen.Id("__res").Dot("Disposition")),
					),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("__errR").Dot("Release").Call()),
					),
					jen.Return(jen.Nil()),
				),
			),
		),
	)
	wrappedBody = append(wrappedBody, declineBody...)

	body = append(body,
		jen.Return(jen.Id("runNoRawOrchestration").Call(
			jen.Id("adapter"),
			jen.Id("out"),
			// newHeartbeat
			jen.Func().Params().Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Id("__r").Op(":=").Id(me.getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(g.pkgs.InterfacesPkg, "StreamResultKindHeartbeat"),
				jen.Return(jen.Id("__r")),
			),
			// newError
			jen.Func().Params(jen.Id("err").Error()).Qual(g.pkgs.InterfacesPkg, "StreamResult").Block(
				jen.Return(jen.Id(me.errorConstructorName).Call(jen.Id("err"))),
			),
			// release
			jen.Func().Params(jen.Id("__r").Qual(g.pkgs.InterfacesPkg, "StreamResult")).Block(
				jen.Id("__r").Dot("Release").Call(),
			),
			jen.Id("plannedMetadata"),
			// newMetadataResult: adds planned_engine=native on the OUTCOME frame.
			newMetadataResultNative,
			// body: native probe, then the ordinary legacy stream on decline.
			jen.Func().Params(jen.Id("beforeFinal").Func().Params(), jen.Id("onTick").Add(onTickType(g.pkgs.BamlPkg))).Error().Block(wrappedBody...),
		)),
	)

	out.Func().
		Id(me.directLegacyCallMethodName()).
		Params(
			jen.Id("adapter").Qual(g.pkgs.InterfacesPkg, "Adapter"),
			jen.Id("rawInput").Any(),
			jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			jen.Id("provider").String(),
			// TRUTHFUL retry-override fact: __legacyRetryPolicy != nil at the router
			// call site. A resolved retry policy declines native admission pre-socket.
			jen.Id("hasRequestRetryOverride").Bool(),
			jen.Id("plannedMetadata").Op("*").Qual(g.pkgs.InterfacesPkg, "Metadata"),
			jen.Id("clientOverride").String(),
		).
		Error().
		Block(body...)
}
