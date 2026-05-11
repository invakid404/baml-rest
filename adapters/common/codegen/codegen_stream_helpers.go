package codegen

import (
	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
)

// onTickType returns the jen type for the BAML OnTick callback signature.
func onTickType() *jen.Statement {
	return jen.Func().Params(
		jen.Qual("context", "Context"),
		jen.Qual(BamlPkg, "TickReason"),
		jen.Qual(BamlPkg, "FunctionLog"),
	).Qual(BamlPkg, "FunctionSignal")
}

// generateStreamHelpers emits shared orchestration functions that encapsulate
// the goroutine management, heartbeat tracking, panic recovery, and shutdown
// state machine common to all streaming methods. Per-method code supplies
// only the method-specific closures.
func generateStreamHelpers(out *jen.File) {
	streamResultIface := jen.Qual(common.InterfacesPkg, "StreamResult")

	// emitPlannedPieces returns the two Jen nodes shared by
	// runNoRawOrchestration and runFullOrchestration for emitting
	// planned-metadata upfront from the stream goroutine: the
	// `plannedSent atomic.Bool` declaration and the `emitPlanned`
	// closure body. Centralised here so the two orchestrators don't
	// duplicate the closure verbatim.
	//
	// The helper deliberately does NOT cover `lastFuncLog`: that
	// declaration sits at different positions in each orchestrator
	// (early in noRaw, after extractor setup in full) and carries
	// site-specific rationale comments about FunctionLog live-handle
	// semantics. Each call site keeps its `lastFuncLog` decl inline.
	//
	// Returned values are fresh Jen statements per invocation, so
	// each call site gets its own AST nodes (no aliasing).
	emitPlannedPieces := func() (plannedSentDecl, emitPlannedDecl jen.Code) {
		plannedSentDecl = jen.Var().Id("plannedSent").Qual("sync/atomic", "Bool")
		emitPlannedDecl = jen.Id("emitPlanned").Op(":=").Func().Params().Block(
			jen.If(jen.Id("plannedMetadata").Op("==").Nil().Op("||").Id("newMetadataResult").Op("==").Nil()).Block(jen.Return()),
			jen.If(jen.Op("!").Id("plannedSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(jen.Return()),
			jen.Id("__plan").Op(":=").Op("*").Id("plannedMetadata"),
			jen.Id("__plan").Dot("Phase").Op("=").Qual(common.InterfacesPkg, "MetadataPhasePlanned"),
			jen.Id("__m").Op(":=").Id("newMetadataResult").Call(jen.Op("&").Id("__plan")),
			jen.If(jen.Id("__m").Op("==").Nil()).Block(jen.Return()),
			// Non-blocking emit: release on full / shutdown so a busy
			// or torn-down channel cannot wedge this path. Matches the
			// heartbeat's drop-on-full policy.
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Id("__m")).Block(),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__m"))),
				jen.Default().Block(jen.Id("release").Call(jen.Id("__m"))),
			),
		)
		return
	}

	// ──────────────────────────────────────────────────────────────────
	// runNoRawOrchestration
	// ──────────────────────────────────────────────────────────────────
	out.Comment("runNoRawOrchestration manages the noRaw streaming lifecycle:")
	out.Comment("heartbeat tracking, onTick callback, goroutine launch, and panic recovery.")
	out.Comment("The per-method body closure handles stream creation and iteration.")
	plannedSentDeclA, emitPlannedDeclA := emitPlannedPieces()
	out.Func().Id("runNoRawOrchestration").
		Params(
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("out").Chan().Add(streamResultIface.Clone()),
			jen.Id("newHeartbeat").Func().Params().Add(streamResultIface.Clone()),
			jen.Id("newError").Func().Params(jen.Error()).Add(streamResultIface.Clone()),
			jen.Id("release").Func().Params(streamResultIface.Clone()),
			// plannedMetadata: pre-built planned-phase Metadata for this request,
			// or nil to disable metadata emission entirely (tests, mixed-mode
			// legacy children whose parent orchestrator already emitted
			// metadata for the chain). The orchestrator emits the planned
			// event upfront from the stream goroutine — before BAML's stream
			// starts — so routing observability does not depend on BAML
			// firing onTick. The outcome event is emitted later via the
			// body's beforeFinal callback.
			jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			// newMetadataResult: pool-wraps a Metadata payload as a StreamResult.
			// Required when plannedMetadata is non-nil.
			jen.Id("newMetadataResult").Func().Params(jen.Op("*").Qual(common.InterfacesPkg, "Metadata")).Add(streamResultIface.Clone()),
			// body receives a beforeFinal callback that the body MUST invoke
			// before sending the final StreamResult, and an onTick callback
			// that body MUST install on the BAML stream. beforeFinal builds
			// and emits the outcome metadata event so it lands between the
			// last partial and the final.
			jen.Id("body").Func().Params(jen.Func().Params(), jen.Add(onTickType())).Error(),
		).
		Error().
		Block(
			// startTime anchors the UpstreamDurMs measurement on the outcome
			// metadata event.
			jen.Id("startTime").Op(":=").Qual("time", "Now").Call(),
			jen.Var().Id("heartbeatSent").Qual("sync/atomic", "Bool"),
			// plannedSent gates the planned-metadata emission so the
			// same payload doesn't go out twice. Emit it upfront
			// (before body() runs the BAML stream) rather than inside
			// onTick because planned metadata describes the routing
			// decision already made — it must not depend on BAML's
			// state-change callbacks firing. For requests where BAML
			// completes synchronously (e.g., legacy path with
			// WithClient naming a strategy parent that resolves
			// through static IR), no onTick fires; gating planned
			// emission on heartbeatSent would lose the event in that
			// case.
			plannedSentDeclA,
			// lastFuncLog stores the most recent FunctionLog reference seen
			// by onTick. Read by beforeFinal to derive winner identity and
			// BAML's internal call count for the outcome event. nil if no
			// onTick fired before the stream completed (rare; degrade
			// gracefully via BuildLegacyOutcome's planned-fallback ladder).
			jen.Var().Id("lastFuncLog").Qual("sync/atomic", "Value"),

			// emitPlanned sends the planned-metadata event to out
			// exactly once per orchestrator invocation. Called
			// upfront from the stream goroutine below, before body()
			// runs BAML's stream, so emission is guaranteed
			// regardless of whether BAML fires onTick. plannedSent
			// CAS guarantees idempotency. Body shared with
			// runFullOrchestration via emitPlannedPieces.
			emitPlannedDeclA,

			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("_").Qual("context", "Context"),
				jen.Id("_").Qual(BamlPkg, "TickReason"),
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(
				// Store the latest FunctionLog on every tick (not just the
				// first) so the outcome event reflects the post-stream
				// state, not the pre-stream state.
				jen.Id("lastFuncLog").Dot("Store").Call(jen.Id("funcLog")),
				jen.If(jen.Id("heartbeatSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.Select().Block(
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
						jen.Default().Block(),
					),
					jen.Id("__r").Op(":=").Id("newHeartbeat").Call(),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
						jen.Default().Block(jen.Id("release").Call(jen.Id("__r"))),
					),
				),
				jen.Return(jen.Nil()),
			),

			// beforeFinal builds the outcome metadata event from lastFuncLog
			// (if any) and emits it. Body MUST call this exactly once,
			// immediately before sending the final StreamResult, so the
			// metadata event lands between the last partial and the final.
			// Safe to call when plannedMetadata is nil — short-circuits.
			jen.Id("beforeFinal").Op(":=").Func().Params().Block(
				jen.If(jen.Id("plannedMetadata").Op("==").Nil().Op("||").Id("newMetadataResult").Op("==").Nil()).Block(
					jen.Return(),
				),
				jen.Var().Id("__winnerClient").String(),
				jen.Var().Id("__winnerProvider").String(),
				jen.Var().Id("__bamlCallCount").Op("*").Int(),
				jen.If(
					jen.List(jen.Id("fl"), jen.Id("flOk")).Op(":=").Id("lastFuncLog").Dot("Load").Call().Assert(jen.Qual(BamlPkg, "FunctionLog")),
					jen.Id("flOk"),
				).Block(
					jen.If(
						jen.List(jen.Id("__sel"), jen.Id("__selErr")).Op(":=").Id("fl").Dot("SelectedCall").Call(),
						jen.Id("__selErr").Op("==").Nil().Op("&&").Id("__sel").Op("!=").Nil(),
					).Block(
						jen.If(
							jen.List(jen.Id("__cn"), jen.Id("__cnErr")).Op(":=").Id("__sel").Dot("ClientName").Call(),
							jen.Id("__cnErr").Op("==").Nil(),
						).Block(jen.Id("__winnerClient").Op("=").Id("__cn")),
						jen.If(
							jen.List(jen.Id("__pv"), jen.Id("__pvErr")).Op(":=").Id("__sel").Dot("Provider").Call(),
							jen.Id("__pvErr").Op("==").Nil(),
						).Block(jen.Id("__winnerProvider").Op("=").Id("__pv")),
					),
					jen.If(
						jen.List(jen.Id("__calls"), jen.Id("__callsErr")).Op(":=").Id("fl").Dot("Calls").Call(),
						jen.Id("__callsErr").Op("==").Nil(),
					).Block(
						jen.Id("__n").Op(":=").Len(jen.Id("__calls")).Op("-").Lit(1),
						jen.If(jen.Id("__n").Op("<").Lit(0)).Block(jen.Id("__n").Op("=").Lit(0)),
						jen.Id("__bamlCallCount").Op("=").Op("&").Id("__n"),
					),
				),
				jen.Id("__outcome").Op(":=").Qual(common.InterfacesPkg, "BuildLegacyOutcome").Call(
					jen.Id("plannedMetadata"),
					jen.Qual("time", "Since").Call(jen.Id("startTime")).Dot("Milliseconds").Call(),
					jen.Id("__winnerClient"),
					jen.Id("__winnerProvider"),
					jen.Id("__bamlCallCount"),
				),
				jen.If(jen.Id("__outcome").Op("==").Nil()).Block(jen.Return()),
				jen.Id("__om").Op(":=").Id("newMetadataResult").Call(jen.Id("__outcome")),
				jen.If(jen.Id("__om").Op("==").Nil()).Block(jen.Return()),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__om")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__om"))),
				),
			),

			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				// Emit planned metadata upfront. Decoupled from onTick
				// so the routing decision is observable even when BAML
				// completes without firing onTick (legacy path with
				// WithClient targeting a strategy parent that resolves
				// through static IR).
				jen.Id("emitPlanned").Call(),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					jen.Func().Params(jen.Id("err").Error()).Block(
						jen.Id("__errR").Op(":=").Id("newError").Call(jen.Id("err")),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
								jen.Id("release").Call(jen.Id("__errR")),
							),
						),
					),
					jen.Func().Params().Error().Block(
						jen.Return(jen.Id("body").Call(jen.Id("beforeFinal"), jen.Id("onTick"))),
					),
				),
			).Call(),

			jen.Return(jen.Nil()),
		)

	// ──────────────────────────────────────────────────────────────────
	// runFullOrchestration
	// ──────────────────────────────────────────────────────────────────
	out.Comment("runFullOrchestration manages the full streaming lifecycle:")
	out.Comment("queue management, two-phase shutdown, heartbeat, onTick, partials goroutine,")
	out.Comment("stream drain goroutine, and panic recovery.")
	out.Comment("Per-method code supplies processTick, driveStream, and emitFinal closures.")
	plannedSentDeclB, emitPlannedDeclB := emitPlannedPieces()
	out.Func().Id("runFullOrchestration").
		Params(
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("out").Chan().Add(streamResultIface.Clone()),
			jen.Id("options").Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Id("newHeartbeat").Func().Params().Add(streamResultIface.Clone()),
			jen.Id("newError").Func().Params(jen.Error()).Add(streamResultIface.Clone()),
			jen.Id("release").Func().Params(streamResultIface.Clone()),
			// plannedMetadata: pre-built planned-phase Metadata for this request,
			// or nil to disable metadata emission entirely. The orchestrator
			// emits the planned event upfront from the stream-drain goroutine
			// — before driveStream starts BAML's stream — so routing
			// observability does not depend on BAML firing onTick. The
			// outcome event is emitted just before the final result. Both
			// events are constructed from this seed via newMetadataResult.
			jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
			// newMetadataResult: pool-wraps a Metadata payload as a StreamResult.
			// Required when plannedMetadata is non-nil.
			jen.Id("newMetadataResult").Func().Params(jen.Op("*").Qual(common.InterfacesPkg, "Metadata")).Add(streamResultIface.Clone()),
			// processTick: handle one FunctionLog tick (extract chunks, parse, emit partial).
			// Receives the shared extractor + mutex.
			jen.Id("processTick").Func().Params(
				jen.Qual(BamlPkg, "FunctionLog"),
				jen.Op("*").Qual(common.SSEPkg, "IncrementalExtractor"),
				jen.Op("*").Qual("sync", "Mutex"),
			).Error(),
			// driveStream: create the BAML stream and iterate it, returning (finalResult, lastError).
			jen.Id("driveStream").Func().Params(
				jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			).Params(jen.Any(), jen.Error()),
			// emitFinal: wrap and emit the final result.
			// Signature is (result, raw, reasoning) — raw is text-only, reasoning
			// is the structured /with-raw reasoning channel.
			jen.Id("emitFinal").Func().Params(jen.Any(), jen.String(), jen.String()).Add(streamResultIface.Clone()),
		).
		Error().
		Block(
			// startTime anchors the UpstreamDurMs measurement on the outcome
			// metadata event. Captured at orchestrator entry to match the
			// BuildRequest path's wall-clock semantics.
			jen.Id("startTime").Op(":=").Qual("time", "Now").Call(),

			// Queue + context
			jen.Id("funcLogQueue").Op(":=").Qual(common.GoConcurrentQueuePkg, "NewFIFO").Call(),
			jen.List(jen.Id("queueCtx"), jen.Id("queueCancel")).Op(":=").Qual("context", "WithCancel").Call(jen.Qual("context", "Background").Call()),

			// Shutdown state
			jen.Var().Id("stopping").Qual("sync/atomic", "Bool"),
			jen.Var().Id("inTick").Qual("sync/atomic", "Int64"),
			jen.Var().Id("pending").Qual("sync/atomic", "Int64"),
			jen.Id("ticksDone").Op(":=").Make(jen.Chan().Struct()),
			jen.Id("allDone").Op(":=").Make(jen.Chan().Struct()),
			jen.Var().Id("ticksOnce").Qual("sync", "Once"),
			jen.Var().Id("allOnce").Qual("sync", "Once"),
			jen.Var().Id("shutdownOnce").Qual("sync", "Once"),
			// watcherDone is closed when the stream drain goroutine finishes, allowing
			// the watcher goroutine to exit without waiting for adapter cancellation.
			jen.Id("watcherDone").Op(":=").Make(jen.Chan().Struct()),
			jen.Var().Id("heartbeatSent").Qual("sync/atomic", "Bool"),
			// plannedSent gates the planned-metadata emission (decoupled
			// from heartbeatSent so we can emit upfront without firing
			// the heartbeat early). Same rationale as runNoRawOrchestration:
			// planned metadata describes the routing decision already
			// made and must not depend on BAML's onTick firing.
			plannedSentDeclB,
			jen.Var().Id("fatalMu").Qual("sync", "Mutex"),
			jen.Var().Id("fatalErr").Error(),

			// emitPlanned is called once per orchestrator invocation,
			// upfront in the stream goroutine. plannedSent CAS
			// guarantees idempotency. Body shared with
			// runNoRawOrchestration via emitPlannedPieces.
			emitPlannedDeclB,

			// Extractor. The boolean argument captures the per-request
			// IncludeReasoning opt-in: when true, provider-specific
			// reasoning text is accumulated into the extractor's
			// reasoning buffer; when false (default), the reasoning
			// buffer stays empty. Parseable and raw buffers are
			// text-only by construction regardless of this flag.
			jen.Id("extractor").Op(":=").Qual(common.SSEPkg, "NewIncrementalExtractor").Call(
				jen.Id("adapter").Dot("IncludeReasoning").Call(),
			),
			jen.Var().Id("extractorMu").Qual("sync", "Mutex"),
			// lastFuncLog stores the most recent FunctionLog reference seen by
			// onTick. FunctionLog is a live handle into the BAML runtime, so
			// re-reading Calls()/SSEChunks() after the stream ends reflects the
			// final state — useful when the last onTick fires before the last
			// SSE chunk arrives.
			jen.Var().Id("lastFuncLog").Qual("sync/atomic", "Value"),

			// doShutdown
			jen.Id("doShutdown").Op(":=").Func().Params().Block(
				jen.Id("stopping").Dot("Store").Call(jen.True()),
				jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
					jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("ticksDone")))),
				).Else().Block(jen.Op("<-").Id("ticksDone")),
				jen.Id("queueCancel").Call(),
				jen.If(jen.Id("pending").Dot("Load").Call().Op("==").Lit(0)).Block(
					jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("allDone")))),
				).Else().Block(jen.Op("<-").Id("allDone")),
			),

			// errHandler
			jen.Id("errHandler").Op(":=").Func().Params(jen.Id("err").Error()).Block(
				jen.Id("fatalMu").Dot("Lock").Call(),
				jen.If(jen.Id("fatalErr").Op("==").Nil()).Block(jen.Id("fatalErr").Op("=").Id("err")),
				jen.Id("fatalMu").Dot("Unlock").Call(),
				jen.If(jen.Id("stopping").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
						jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("ticksDone")))),
					),
					jen.Go().Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
				),
			),

			// Watcher goroutine — exits on adapter cancellation OR normal stream completion.
			jen.Go().Func().Params().Block(
				jen.Select().Block(
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
					),
					jen.Case(jen.Op("<-").Id("watcherDone")).Block(),
				),
			).Call(),

			// decrementPending helper
			jen.Id("decrementPending").Op(":=").Func().Params().Block(
				jen.If(jen.Id("pending").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call()).Block(
					jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("allDone")))),
				),
			),

			// onTick callback
			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("_").Qual("context", "Context"),
				jen.Id("_").Qual(BamlPkg, "TickReason"),
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(
				jen.If(
					jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
						jen.Func().Params().Error().Block(
							jen.Id("inTick").Dot("Add").Call(jen.Lit(1)),
							jen.Defer().Func().Params().Block(
								jen.If(jen.Id("inTick").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call()).Block(
									jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("ticksDone")))),
								),
							).Call(),
							jen.If(jen.Id("stopping").Dot("Load").Call()).Block(jen.Return(jen.Nil())),
							jen.Select().Block(
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
								jen.Default().Block(),
							),
							// Heartbeat: signals the pool's hung detector that
							// upstream returned 2xx. Planned metadata emission
							// is now decoupled and fires upfront from the
							// stream goroutine — see emitPlanned above.
							jen.If(jen.Id("heartbeatSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
								jen.Id("__r").Op(":=").Id("newHeartbeat").Call(),
								jen.Select().Block(
									jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
									jen.Default().Block(jen.Id("release").Call(jen.Id("__r"))),
								),
							),
							// Store latest FunctionLog for the final reconciliation pass
							// in the stream drain goroutine.
							jen.Id("lastFuncLog").Dot("Store").Call(jen.Id("funcLog")),
							// Enqueue
							jen.Id("pending").Dot("Add").Call(jen.Lit(1)),
							jen.If(jen.Id("err").Op(":=").Id("funcLogQueue").Dot("Enqueue").Call(jen.Id("funcLog")), jen.Id("err").Op("!=").Nil()).Block(
								jen.If(jen.Id("pending").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call()).Block(
									jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(jen.Close(jen.Id("allDone")))),
								),
							),
							jen.Return(jen.Nil()),
						),
					),
					jen.Id("err").Op("!=").Nil(),
				).Block(jen.Id("errHandler").Call(jen.Id("err"))),
				jen.Return(jen.Nil()),
			),

			// Partials goroutine: process function + drain + main loop
			jen.Id("processItem").Op(":=").Func().Params(jen.Id("funcLog").Qual(BamlPkg, "FunctionLog")).Block(
				jen.Defer().Id("decrementPending").Call(),
				jen.If(
					jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
						jen.Func().Params().Error().Block(
							jen.Return(jen.Id("processTick").Call(jen.Id("funcLog"), jen.Id("extractor"), jen.Op("&").Id("extractorMu"))),
						),
					),
					jen.Id("err").Op("!=").Nil(),
				).Block(jen.Id("errHandler").Call(jen.Id("err"))),
			),

			jen.Id("drain").Op(":=").Func().Params().Block(
				jen.For().Block(
					jen.Select().Block(
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.For().Block(
								jen.List(jen.Id("_"), jen.Id("drainErr")).Op(":=").Id("funcLogQueue").Dot("Dequeue").Call(),
								jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(jen.Return()),
								jen.Id("decrementPending").Call(),
							),
						),
						jen.Default().Block(),
					),
					jen.List(jen.Id("remaining"), jen.Id("drainErr")).Op(":=").Id("funcLogQueue").Dot("Dequeue").Call(),
					jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(jen.Return()),
					jen.List(jen.Id("funcLog"), jen.Id("ok")).Op(":=").Id("remaining").Assert(jen.Qual(BamlPkg, "FunctionLog")),
					jen.If(jen.Op("!").Id("ok")).Block(jen.Id("decrementPending").Call(), jen.Continue()),
					jen.Id("processItem").Call(jen.Id("funcLog")),
				),
			),

			// Partials goroutine
			jen.Go().Func().Params().Block(
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					jen.Id("errHandler"),
					jen.Func().Params().Error().Block(
						jen.For().Block(
							jen.List(jen.Id("shouldExit"), jen.Id("err")).Op(":=").Qual("github.com/gregwebs/go-recovery", "Call1").Types(jen.Bool()).Call(
								jen.Func().Params().Params(jen.Bool(), jen.Error()).Block(
									jen.List(jen.Id("item"), jen.Id("err")).Op(":=").Id("funcLogQueue").Dot("DequeueOrWaitForNextElementContext").Call(jen.Id("queueCtx")),
									jen.If(jen.Id("err").Op("!=").Nil()).Block(jen.Id("drain").Call(), jen.Return(jen.True(), jen.Nil())),
									jen.List(jen.Id("funcLog"), jen.Id("ok")).Op(":=").Id("item").Assert(jen.Qual(BamlPkg, "FunctionLog")),
									jen.If(jen.Op("!").Id("ok")).Block(jen.Id("decrementPending").Call(), jen.Return(jen.False(), jen.Nil())),
									jen.Id("processItem").Call(jen.Id("funcLog")),
									jen.Return(jen.False(), jen.Nil()),
								),
							),
							jen.If(jen.Id("err").Op("!=").Nil()).Block(jen.Id("errHandler").Call(jen.Id("err")), jen.Continue()),
							jen.If(jen.Id("shouldExit")).Block(jen.Return(jen.Nil())),
						),
						jen.Return(jen.Nil()),
					),
				),
			).Call(),

			// Stream drain goroutine
			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				jen.Defer().Close(jen.Id("watcherDone")),
				// Emit planned metadata upfront. Decoupled from onTick so
				// the routing decision is observable even when BAML
				// completes synchronously without firing onTick.
				jen.Id("emitPlanned").Call(),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					jen.Func().Params(jen.Id("err").Error()).Block(
						jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
						jen.Id("__errR").Op(":=").Id("newError").Call(jen.Id("err")),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__errR"))),
						),
					),
					jen.Func().Params().Error().Block(
						jen.Id("opts").Op(":=").Append(jen.Id("options"), jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick"))),
						jen.List(jen.Id("finalResult"), jen.Id("lastErr")).Op(":=").Id("driveStream").Call(jen.Id("opts")),
						jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),

						// Check fatal error
						jen.Id("fatalMu").Dot("Lock").Call(),
						jen.Id("fatalErrCopy").Op(":=").Id("fatalErr"),
						jen.Id("fatalMu").Dot("Unlock").Call(),
						jen.If(jen.Id("fatalErrCopy").Op("!=").Nil()).Block(
							jen.Id("__errR").Op(":=").Id("newError").Call(jen.Id("fatalErrCopy")),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__errR"))),
							),
							jen.Return(jen.Nil()),
						),

						// Check stream error
						jen.If(jen.Id("lastErr").Op("!=").Nil()).Block(
							jen.Id("__errR").Op(":=").Id("newError").Call(jen.Id("lastErr")),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__errR"))),
							),
							jen.Return(jen.Nil()),
						),

						// Final reconciliation: FunctionLog is a live handle, but
						// lastFuncLog's SSEChunks view can be stale relative to the
						// post-stream state. Re-run processTick once to pick up any
						// additional chunks that became visible after driveStream
						// returned. This is best-effort — a length-based cross-check
						// against RawLLMResponse below catches remaining gaps.
						jen.Id("fl").Op(",").Id("flOk").Op(":=").Id("lastFuncLog").Dot("Load").Call().Assert(jen.Qual(BamlPkg, "FunctionLog")),
						jen.If(jen.Id("flOk")).Block(
							jen.Id("_").Op("=").Id("processTick").Call(jen.Id("fl"), jen.Id("extractor"), jen.Op("&").Id("extractorMu")),
						),

						// Emit final. Reconciling extractor.RawFull() with
						// RawLLMResponse() handles the stale-SSE-view race:
						// in rare CI-only races where lastFuncLog's SSEChunks
						// view is stale (onTick didn't cover the final SSE
						// event), the extractor can end up truncated while
						// RawLLMResponse is complete. We splice in only the
						// missing text suffix from RawLLMResponse so any
						// late text the extractor missed is recovered.
						//
						// Both the extractor's raw buffer and RawLLMResponse
						// are text-only under the new model — reasoning is
						// surfaced via the separate ReasoningFull buffer and
						// no reconciliation is needed for it (BAML's runtime
						// doesn't track reasoning, so the extractor is the
						// sole source of truth).
						//
						// Reconciliation logic for raw:
						//   1. If RawLLMResponse extends extractor.ParseableFull
						//      as a prefix and is longer, splice in the missing
						//      text suffix.
						//   2. Else if RawLLMResponse is longer than
						//      extractor.RawFull (the prefix invariant is
						//      violated — defensive), defer to RawLLMResponse.
						//   3. Else keep extractor.RawFull.
						jen.Id("extractorMu").Dot("Lock").Call(),
						jen.Id("finalRaw").Op(":=").Id("extractor").Dot("RawFull").Call(),
						jen.Id("parseableFull").Op(":=").Id("extractor").Dot("ParseableFull").Call(),
						jen.Id("finalReasoning").Op(":=").Id("extractor").Dot("ReasoningFull").Call(),
						jen.Id("extractorMu").Dot("Unlock").Call(),
						jen.If(jen.Id("flOk")).Block(
							jen.If(
								jen.List(jen.Id("authRaw"), jen.Id("rawErr")).Op(":=").Id("fl").Dot("RawLLMResponse").Call(),
								jen.Id("rawErr").Op("==").Nil(),
							).Block(
								jen.If(
									jen.Len(jen.Id("authRaw")).Op(">").Len(jen.Id("parseableFull")).
										Op("&&").
										Qual("strings", "HasPrefix").Call(jen.Id("authRaw"), jen.Id("parseableFull")),
								).Block(
									jen.Id("finalRaw").Op("=").Id("finalRaw").Op("+").Id("authRaw").Index(jen.Len(jen.Id("parseableFull")), jen.Empty()),
								).Else().If(
									jen.Len(jen.Id("authRaw")).Op(">").Len(jen.Id("finalRaw")),
								).Block(
									jen.Id("finalRaw").Op("=").Id("authRaw"),
								),
							),
						),

						// Outcome metadata: build from the winning attempt's
						// FunctionLog and emit just before the final, so clients
						// observe the routing outcome attached to (and ordered
						// before) the terminal payload. Mirrors the BuildRequest
						// path's contract; no-op when plannedMetadata is nil.
						jen.If(jen.Id("plannedMetadata").Op("!=").Nil().Op("&&").Id("newMetadataResult").Op("!=").Nil()).Block(
							jen.Var().Id("__winnerClient").String(),
							jen.Var().Id("__winnerProvider").String(),
							jen.Var().Id("__bamlCallCount").Op("*").Int(),
							jen.If(jen.Id("flOk")).Block(
								jen.If(
									jen.List(jen.Id("__sel"), jen.Id("__selErr")).Op(":=").Id("fl").Dot("SelectedCall").Call(),
									jen.Id("__selErr").Op("==").Nil().Op("&&").Id("__sel").Op("!=").Nil(),
								).Block(
									jen.If(
										jen.List(jen.Id("__cn"), jen.Id("__cnErr")).Op(":=").Id("__sel").Dot("ClientName").Call(),
										jen.Id("__cnErr").Op("==").Nil(),
									).Block(jen.Id("__winnerClient").Op("=").Id("__cn")),
									jen.If(
										jen.List(jen.Id("__pv"), jen.Id("__pvErr")).Op(":=").Id("__sel").Dot("Provider").Call(),
										jen.Id("__pvErr").Op("==").Nil(),
									).Block(jen.Id("__winnerProvider").Op("=").Id("__pv")),
								),
								jen.If(
									jen.List(jen.Id("__calls"), jen.Id("__callsErr")).Op(":=").Id("fl").Dot("Calls").Call(),
									jen.Id("__callsErr").Op("==").Nil(),
								).Block(
									jen.Id("__n").Op(":=").Len(jen.Id("__calls")).Op("-").Lit(1),
									jen.If(jen.Id("__n").Op("<").Lit(0)).Block(jen.Id("__n").Op("=").Lit(0)),
									jen.Id("__bamlCallCount").Op("=").Op("&").Id("__n"),
								),
							),
							jen.Id("__outcome").Op(":=").Qual(common.InterfacesPkg, "BuildLegacyOutcome").Call(
								jen.Id("plannedMetadata"),
								jen.Qual("time", "Since").Call(jen.Id("startTime")).Dot("Milliseconds").Call(),
								jen.Id("__winnerClient"),
								jen.Id("__winnerProvider"),
								jen.Id("__bamlCallCount"),
							),
							jen.If(jen.Id("__outcome").Op("!=").Nil()).Block(
								jen.Id("__om").Op(":=").Id("newMetadataResult").Call(jen.Id("__outcome")),
								jen.If(jen.Id("__om").Op("!=").Nil()).Block(
									jen.Select().Block(
										jen.Case(jen.Id("out").Op("<-").Id("__om")).Block(),
										jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
											jen.Id("release").Call(jen.Id("__om")),
											jen.Return(jen.Nil()),
										),
									),
								),
							),
						),

						jen.Id("__r").Op(":=").Id("emitFinal").Call(jen.Id("finalResult"), jen.Id("finalRaw"), jen.Id("finalReasoning")),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__r"))),
						),
						jen.Return(jen.Nil()),
					),
				),
			).Call(),

			jen.Return(jen.Nil()),
		)

	// ──────────────────────────────────────────────────────────────────
	// runLegacyChildStream — shared helper for mixed-mode fallback
	// ──────────────────────────────────────────────────────────────────
	out.Comment("runLegacyChildStream drives a single child of a mixed-mode fallback")
	out.Comment("chain through BAML's Stream API. The per-method closure supplies")
	out.Comment("driveStream, which appends the WithOnTick option and invokes")
	out.Comment("Stream.<Method>. This helper wires the onTick callback that fires")
	out.Comment("sendHeartbeat on the first FunctionLog tick (matching the")
	out.Comment("BuildRequest path's post-HTTP liveness semantics) and captures the")
	out.Comment("last FunctionLog so raw can be read via RawLLMResponse.")
	out.Func().Id("runLegacyChildStream").
		Params(
			jen.Id("ctx").Qual("context", "Context"),
			jen.Id("needsRaw").Bool(),
			jen.Id("sendHeartbeat").Func().Params(),
			jen.Id("driveStream").Func().Params(
				jen.Add(onTickType()),
			).Params(jen.Any(), jen.Error()),
		).
		Params(jen.Any(), jen.String(), jen.Error()).
		Block(
			// Track first-tick state so sendHeartbeat fires exactly once when
			// BAML reports the first FunctionLog tick (i.e. first observed
			// upstream activity). The orchestrator owns heartbeat
			// idempotency; this guard just saves the extra closure call.
			jen.Var().Id("heartbeatFired").Qual("sync/atomic", "Bool"),
			// lastFuncLog stores the most recent FunctionLog handle so
			// RawLLMResponse can be read after driveStream returns.
			jen.Var().Id("lastFuncLog").Qual("sync/atomic", "Value"),

			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("_").Qual("context", "Context"),
				jen.Id("_").Qual(BamlPkg, "TickReason"),
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(
				jen.If(jen.Id("heartbeatFired").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.Id("sendHeartbeat").Call(),
				),
				jen.Id("lastFuncLog").Dot("Store").Call(jen.Id("funcLog")),
				jen.Return(jen.Nil()),
			),

			jen.List(jen.Id("finalResult"), jen.Id("err")).Op(":=").Id("driveStream").Call(jen.Id("onTick")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Lit(""), jen.Id("err")),
			),

			// Raw is only computed when requested; callers that don't need
			// raw skip the BAML call entirely (it's cheap but pointless).
			jen.Var().Id("raw").String(),
			jen.If(jen.Id("needsRaw")).Block(
				jen.If(
					jen.List(jen.Id("fl"), jen.Id("ok")).Op(":=").Id("lastFuncLog").Dot("Load").Call().Assert(jen.Qual(BamlPkg, "FunctionLog")),
					jen.Id("ok"),
				).Block(
					jen.If(
						jen.List(jen.Id("r"), jen.Id("rawErr")).Op(":=").Id("fl").Dot("RawLLMResponse").Call(),
						jen.Id("rawErr").Op("==").Nil(),
					).Block(
						jen.Id("raw").Op("=").Id("r"),
					),
				),
			),

			jen.Return(jen.Id("finalResult"), jen.Id("raw"), jen.Nil()),
		)
}
