package codegen

import (
	"github.com/dave/jennifer/jen"
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

	// Build call parameters for the noRaw Stream call (includes WithOnTick for heartbeat)
	var noRawStreamCallParams []jen.Code
	noRawStreamCallParams = append(noRawStreamCallParams, jen.Id("adapter")) // context
	for _, arg := range me.args {
		noRawStreamCallParams = append(noRawStreamCallParams, me.argCallParam(arg))
	}
	noRawStreamCallParams = append(noRawStreamCallParams,
		jen.Id("streamOpts").Op("..."),
	)

	// noRaw goroutine body - wrapped in gorecovery.GoHandler for panic resilience
	noRawGoroutineBody := []jen.Code{
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

	// _noRaw is the top-level legacy streaming impl invoked from
	// the BuildRequest fallthrough branch (see __legacyClientOverride
	// dispatch sites below). Use the legacy registry view so
	// runtime strategy-parent overrides are visible to BAML.
	noRawBody := me.makeLegacyPreamble()

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
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
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
