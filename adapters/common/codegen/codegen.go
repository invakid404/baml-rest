package codegen

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/stoewer/go-strcase"
)

const (
	// BamlPkg is the path to the BAML Go runtime package
	BamlPkg = "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

type methodOut struct {
	name             string
	inputStructName  string
	outputStructQual jen.Code
}

// Generate generates the adapter.go file for the given adapter package.
// selfPkg should be the full package path, e.g. "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
func Generate(selfPkg string) {
	selfAdapterPkg := selfPkg + "/adapter"
	selfUtilsPkg := selfPkg + "/utils"

	out := common.MakeFile()

	var methods []methodOut

	// Iterate over sync functions (new approach using sync + onTick + ParseStream)
	for methodName, args := range introspected.SyncMethods {
		// Check if ParseStream has this method (required for streaming parsing)
		if _, hasParseStream := introspected.ParseStreamMethods[methodName]; !hasParseStream {
			continue
		}

		// Get the sync function value for reflection
		syncFuncValue, ok := introspected.SyncFuncs[methodName]
		if !ok {
			continue
		}

		syncFuncType := reflect.TypeOf(syncFuncValue)

		// Verify it's a function with context.Context as first param
		if syncFuncType.Kind() != reflect.Func {
			continue
		}
		if syncFuncType.NumIn() < 1 {
			continue
		}
		firstParam := syncFuncType.In(0)
		if firstParam.String() != "context.Context" {
			continue
		}

		// Generate the input struct
		var structFields []jen.Code

		// Parameters start at index 1 (after context) and end before the variadic opts
		for paramIdx := 1; paramIdx < syncFuncType.NumIn()-1; paramIdx++ {
			argIdx := paramIdx - 1
			if argIdx >= len(args) {
				break
			}
			paramName := args[argIdx]
			paramType := syncFuncType.In(paramIdx)

			parsedType := jen.Id(strcase.UpperCamelCase(paramName)).Add(parseReflectType(paramType).statement)

			structFields = append(structFields,
				parsedType.
					Tag(map[string]string{
						"json": paramName,
					}))
		}

		inputStructName := strcase.UpperCamelCase(fmt.Sprintf("%sInput", methodName))
		out.Type().Id(inputStructName).Struct(structFields...)

		// Generate the output struct with raw LLM response support
		outputStructName := strcase.UpperCamelCase(fmt.Sprintf("%sOutput", methodName))

		// Get the return type (first return value, second is error)
		var finalResultType jen.Code
		if syncFuncType.NumOut() >= 1 {
			finalResultType = parseReflectType(syncFuncType.Out(0)).statement
		} else {
			finalResultType = jen.Any()
		}

		// Output struct holds: kind, raw LLM response, parsed value, error
		out.Type().Id(outputStructName).Struct(
			jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
			jen.Id("raw").String(),
			jen.Id("parsed").Any(),
			jen.Id("err").Error(),
		)

		// Implement `StreamResult` interface for the output struct
		selfName := jen.Id("v")
		selfParam := selfName.Clone().Op("*").Id(outputStructName)

		// Kind() method
		out.Func().
			Params(selfParam.Clone()).
			Id("Kind").Params().
			Qual(common.InterfacesPkg, "StreamResultKind").
			Block(
				jen.Return(selfName.Clone().Dot("kind")),
			)

		// Get the ParseStream function for stream type reflection
		parseStreamFuncValue, hasParseStreamFunc := introspected.ParseStreamFuncs[methodName]

		// Final type (from sync function return)
		finalType := parseReflectType(syncFuncType.Out(0))
		finalTypePtr := jen.Op("*").Add(finalType.statement.Clone())
		isDynamicFinal := hasDynamicPropertiesForType(syncFuncType.Out(0))

		// Stream type (from ParseStream return) - may be different from final type
		var streamType parsedReflectType
		var streamTypePtr *jen.Statement
		var isDynamicStream bool
		if hasParseStreamFunc {
			parseStreamFuncType := reflect.TypeOf(parseStreamFuncValue)
			if parseStreamFuncType.Kind() == reflect.Func && parseStreamFuncType.NumOut() >= 1 {
				streamType = parseReflectType(parseStreamFuncType.Out(0))
				streamTypePtr = jen.Op("*").Add(streamType.statement.Clone())
				isDynamicStream = hasDynamicPropertiesForType(parseStreamFuncType.Out(0))
			}
		}
		// Fallback to final type if we couldn't get stream type
		if streamTypePtr == nil {
			streamType = finalType
			streamTypePtr = finalTypePtr
			isDynamicStream = isDynamicFinal
		}

		// Helper to create value getter with specific type
		makeValueGetter := func(typePtr *jen.Statement, isDynamic bool) []jen.Code {
			baseCode := []jen.Code{
				jen.List(jen.Id("result"), jen.Id("ok")).Op(":=").Add(selfName.Clone().Dot("parsed")).Assert(typePtr.Clone()),
				jen.If(jen.Op("!").Id("ok").Op("||").Id("result").Op("==").Nil()).Block(
					jen.Return(selfName.Clone().Dot("parsed")),
				),
			}

			// Only add DynamicProperties unwrapping for types that have it
			if isDynamic {
				baseCode = append(baseCode,
					jen.If(jen.Id("result").Dot("DynamicProperties").Op("!=").Nil()).Block(
						jen.For(jen.List(jen.Id("key"), jen.Id("value")).Op(":=").Range().Id("result").Dot("DynamicProperties")).
							Block(
								jen.If(
									jen.List(jen.Id("reflectValue"), jen.Id("ok")).Op(":=").Id("value").Assert(jen.Qual("reflect", "Value")),
									jen.Id("ok"),
								).Block(
									jen.Id("result").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("reflectValue").Dot("Interface").Call()),
								),
							),
					),
				)
			}

			baseCode = append(baseCode, jen.Return(jen.Id("result")))
			return baseCode
		}

		// Stream() method - uses stream type (*stream_types.X)
		out.Func().
			Params(selfParam.Clone()).
			Id("Stream").Params().
			Any().
			Block(
				makeValueGetter(streamTypePtr, isDynamicStream)...,
			)

		// Final() method - uses final type (*types.X)
		out.Func().
			Params(selfParam.Clone()).
			Id("Final").Params().
			Any().
			Block(
				makeValueGetter(finalTypePtr, isDynamicFinal)...,
			)

		// Error() method
		out.Func().
			Params(selfParam.Clone()).
			Id("Error").Params().
			Error().
			Block(
				jen.Return(selfName.Clone().Dot("err")),
			)

		// Raw() method - returns the raw LLM response
		out.Func().
			Params(selfParam.Clone()).
			Id("Raw").Params().
			String().
			Block(
				jen.Return(selfName.Clone().Dot("raw")),
			)

		// Generate error constructor function
		errorConstructorName := strcase.LowerCamelCase(fmt.Sprintf("new%sError", outputStructName))
		out.Func().
			Id(errorConstructorName).
			Params(jen.Id("err").Error()).
			Op("*").Id(outputStructName).
			Block(
				jen.Return(jen.Op("&").Id(outputStructName).Values(jen.Dict{
					jen.Id("kind"): jen.Qual(common.InterfacesPkg, "StreamResultKindError"),
					jen.Id("err"):  jen.Id("err"),
				})),
			)

		// Generate the method implementation using sync + onTick + ParseStream
		var methodBody []jen.Code

		// Get options from adapter
		methodBody = append(methodBody,
			jen.List(jen.Id("options"), jen.Id("err")).Op(":=").Id("makeOptionsFromAdapter").Call(jen.Id("adapter")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
		)

		// Only define `input` if the prompt has arguments
		if len(args) > 0 {
			methodBody = append(methodBody,
				jen.List(jen.Id("input"), jen.Id("ok")).Op(":=").Id("rawInput").Assert(jen.Op("*").Id(inputStructName)),
				jen.If(jen.Op("!").Id("ok")).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
						jen.Lit("invalid input type: expected *%s, got %T"),
						jen.Lit(inputStructName),
						jen.Id("rawInput"),
					)),
				),
			)
		}

		// Initialize queue, context, WaitGroup, and mutex for async partial processing
		streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

		methodBody = append(methodBody,
			// Output channel for results
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
			// Unbounded queue for passing raw text from OnTick to partials goroutine (never drops)
			jen.Id("rawQueue").Op(":=").Qual(common.GoConcurrentQueuePkg, "NewFIFO").Call(),
			// Context to signal partials goroutine to stop
			// NOT derived from adapter - we only cancel after ticksDone to ensure no late enqueues
			jen.List(jen.Id("queueCtx"), jen.Id("queueCancel")).Op(":=").Qual("context", "WithCancel").Call(jen.Qual("context", "Background").Call()),

			// Two-phase shutdown state
			jen.Var().Id("stopping").Qual("sync/atomic", "Bool"),   // shutdown requested
			jen.Var().Id("inTick").Qual("sync/atomic", "Int64"),    // number of onTick currently executing
			jen.Var().Id("pending").Qual("sync/atomic", "Int64"),   // number of queued items not yet completed
			jen.Id("ticksDone").Op(":=").Make(jen.Chan().Struct()),  // closed when stopping && inTick==0
			jen.Id("allDone").Op(":=").Make(jen.Chan().Struct()),    // closed when stopping && pending==0
			jen.Var().Id("ticksOnce").Qual("sync", "Once"),
			jen.Var().Id("allOnce").Qual("sync", "Once"),
			jen.Var().Id("shutdownOnce").Qual("sync", "Once"),

			// Fatal error storage
			jen.Var().Id("fatalMu").Qual("sync", "Mutex"),
			jen.Var().Id("fatalErr").Error(),

			// Mutex and lastRawResponse for final raw capture
			jen.Var().Id("lastRawResponse").String(),
			jen.Var().Id("mu").Qual("sync", "Mutex"),

			// Two-phase shutdown function - guarded by shutdownOnce
			jen.Id("doShutdown").Op(":=").Func().Params().Block(
				// Phase 1: Stop accepting new ticks
				jen.Id("stopping").Dot("Store").Call(jen.True()),
				jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
					jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
						jen.Close(jen.Id("ticksDone")),
					)),
				).Else().Block(
					jen.Op("<-").Id("ticksDone"),
				),

				// Phase 2: Cancel queue and wait for pending items
				jen.Id("queueCancel").Call(),
				jen.If(jen.Id("pending").Dot("Load").Call().Op("==").Lit(0)).Block(
					jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(
						jen.Close(jen.Id("allDone")),
					)),
				).Else().Block(
					jen.Op("<-").Id("allDone"),
				),
			),

			// Error handler for goroutine panics - records error and initiates shutdown
			// Triggers full teardown asynchronously so errors abort promptly even if stream hangs
			jen.Id("errHandler").Op(":=").Func().Params(jen.Id("err").Error()).Block(
				jen.Id("fatalMu").Dot("Lock").Call(),
				jen.If(jen.Id("fatalErr").Op("==").Nil()).Block(
					jen.Id("fatalErr").Op("=").Id("err"),
				),
				jen.Id("fatalMu").Dot("Unlock").Call(),

				// Initiate shutdown and kick off full teardown asynchronously
				jen.If(jen.Id("stopping").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
						jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
							jen.Close(jen.Id("ticksDone")),
						)),
					),
					// Trigger full shutdown asynchronously (non-blocking)
					jen.Go().Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
				),
			),
		)

		// Watcher goroutine: trigger shutdown when adapter (HTTP request) is cancelled
		// This ensures prompt cleanup on client disconnect while respecting shutdown ordering
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(
				jen.Op("<-").Id("adapter").Dot("Done").Call(),
				jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
			).Call(),
		)

		// Build the onTick callback - extracts raw text and enqueues for async processing
		// IMPORTANT: OnTick is called synchronously by BAML, so we must return ASAP
		// Uses panic recovery and inTick barrier for safe shutdown
		onTickBody := []jen.Code{
			// Wrap in gorecovery.Call for panic safety - call errHandler if panic occurs
			jen.If(
				jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
					jen.Func().Params().Error().Block(
					// CRITICAL: increment inTick FIRST, before any checks
					// This prevents the race where shutdown sees inTick==0 while we're between
					// checking stopping and incrementing inTick
					jen.Id("inTick").Dot("Add").Call(jen.Lit(1)),
					// Defer exit: decrement counter and close ticksDone if we're the last and stopping
					jen.Defer().Func().Params().Block(
						jen.If(
							jen.Id("inTick").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call(),
						).Block(
							jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
								jen.Close(jen.Id("ticksDone")),
							)),
						),
					).Call(),

					// Now check if we should bail out (after inTick is incremented)
					jen.If(jen.Id("stopping").Dot("Load").Call()).Block(
						jen.Return(jen.Nil()),
					),
					jen.Select().Block(
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Return(jen.Nil()),
						),
						jen.Default().Block(),
					),

					// Build StreamingData from FunctionLog
					jen.Id("calls").Op(",").Id("callsErr").Op(":=").Id("funcLog").Dot("Calls").Call(),
					jen.If(jen.Id("callsErr").Op("!=").Nil()).Block(
						jen.Return(jen.Nil()),
					),

					// Build StreamingData struct
					jen.Id("streamingData").Op(":=").Op("&").Qual(common.SSEPkg, "StreamingData").Values(),
					jen.For(jen.List(jen.Id("_"), jen.Id("call")).Op(":=").Range().Id("calls")).Block(
						// Type-assert to LLMStreamCall (extends LLMCall with SSEChunks)
						jen.Id("streamCall").Op(",").Id("ok").Op(":=").Id("call").Assert(jen.Qual(BamlPkg, "LLMStreamCall")),
						jen.If(jen.Op("!").Id("ok")).Block(
							jen.Continue(),
						),
						jen.Id("provider").Op(",").Id("provErr").Op(":=").Id("streamCall").Dot("Provider").Call(),
						jen.If(jen.Id("provErr").Op("!=").Nil()).Block(
							jen.Continue(),
						),
						jen.Id("chunks").Op(",").Id("chunksErr").Op(":=").Id("streamCall").Dot("SSEChunks").Call(),
						jen.If(jen.Id("chunksErr").Op("!=").Nil()).Block(
							jen.Continue(),
						),
						// Convert chunks to SSEChunk interface slice
						jen.Id("sseChunks").Op(":=").Make(jen.Index().Qual(common.SSEPkg, "SSEChunk"), jen.Len(jen.Id("chunks"))),
						jen.For(jen.List(jen.Id("i"), jen.Id("chunk")).Op(":=").Range().Id("chunks")).Block(
							jen.Id("sseChunks").Index(jen.Id("i")).Op("=").Id("chunk"),
						),
						jen.Id("streamingData").Dot("Calls").Op("=").Append(
							jen.Id("streamingData").Dot("Calls"),
							jen.Qual(common.SSEPkg, "StreamingCall").Values(jen.Dict{
								jen.Id("Provider"): jen.Id("provider"),
								jen.Id("Chunks"):   jen.Id("sseChunks"),
							}),
						),
					),

					// Get raw LLM response by extracting SSE deltas
					jen.List(jen.Id("raw"), jen.Id("rawErr")).Op(":=").Qual(common.SSEPkg, "GetCurrentContent").Call(jen.Id("streamingData")),
					jen.If(jen.Id("rawErr").Op("!=").Nil()).Block(
						jen.Return(jen.Nil()),
					),

					// Skip if no content yet
					jen.If(jen.Id("raw").Op("==").Lit("")).Block(
						jen.Return(jen.Nil()),
					),

					// Update lastRawResponse for final capture
					jen.Id("mu").Dot("Lock").Call(),
					jen.Id("lastRawResponse").Op("=").Id("raw"),
					jen.Id("mu").Dot("Unlock").Call(),

					// Final check before enqueue: if adapter cancelled, don't enqueue
					// (second line of defense against enqueue after parser exit)
					jen.Select().Block(
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Return(jen.Nil()),
						),
						jen.Default().Block(),
					),

					// Reserve pending slot, then enqueue
					// If enqueue fails (shouldn't happen with FIFO), rollback pending
					jen.Id("pending").Dot("Add").Call(jen.Lit(1)),
					jen.If(jen.Id("err").Op(":=").Id("rawQueue").Dot("Enqueue").Call(jen.Id("raw")), jen.Id("err").Op("!=").Nil()).Block(
						// Rollback and check if we need to close allDone
						jen.If(
							jen.Id("pending").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call(),
						).Block(
							jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(
								jen.Close(jen.Id("allDone")),
							)),
						),
					),

					jen.Return(jen.Nil()),
				),
			),
				jen.Id("err").Op("!=").Nil(),
			).Block(
				jen.Id("errHandler").Call(jen.Id("err")),
			),

			jen.Return(jen.Nil()),
		}

		// Build the call parameters for the Stream method
		var streamCallParams []jen.Code
		streamCallParams = append(streamCallParams, jen.Id("adapter")) // context

		for _, arg := range args {
			argName := strcase.UpperCamelCase(arg)
			streamCallParams = append(streamCallParams, jen.Id("input").Dot(argName))
		}

		// Add all options: prepend WithOnTick to the options slice
		// This generates: append([]CallOptionFunc{WithOnTick(onTick)}, options...)...
		streamCallParams = append(streamCallParams,
			jen.Append(
				jen.Index().Qual(common.GeneratedClientPkg, "CallOptionFunc").Values(
					jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
				),
				jen.Id("options").Op("..."),
			).Op("..."),
		)

		// Helper to decrement pending and maybe close allDone
		decrementPending := []jen.Code{
			jen.If(
				jen.Id("pending").Dot("Add").Call(jen.Lit(-1)).Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call(),
			).Block(
				jen.Id("allOnce").Dot("Do").Call(jen.Func().Params().Block(
					jen.Close(jen.Id("allDone")),
				)),
			),
		}

		// Build the partials processing goroutine body - reads from queue, calls ParseStream, emits partials
		// Each item is processed in a separate function call so defers run per-item
		// Per-item panics are caught and reported but don't exit the loop
		// The entire loop iteration is wrapped in gorecovery.Call to catch panics from queue operations
		partialsGoroutineBody := []jen.Code{
			// Define process function - handles one queue item with defer and panic recovery
			jen.Id("process").Op(":=").Func().Params(jen.Id("raw").String()).Block(
				// Defer pending decrement - runs when THIS function returns (per-item)
				jen.Defer().Func().Params().Block(decrementPending...).Call(),

				// Wrap parse+send in gorecovery.Call - if panic, call errHandler but DON'T exit
				jen.If(
					jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
						jen.Func().Params().Error().Block(
							// Call ParseStream outside of OnTick callback context (avoids deadlock)
							jen.List(jen.Id("parsed"), jen.Id("parseErr")).Op(":=").
								Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
									jen.Id("raw"),
									jen.Id("options").Op("..."),
								),
							jen.If(jen.Id("parseErr").Op("==").Nil()).Block(
								// Pre-check: if adapter is cancelled, don't attempt send
								// (avoids race where select randomly picks out <- after close)
								jen.Select().Block(
									jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
										jen.Return(jen.Nil()),
									),
									jen.Default().Block(),
								),

								// ParseStream returns a concrete type - always take address for consistency
								jen.Id("parsedPtr").Op(":=").Op("&").Id("parsed"),

								// Best-effort send (non-blocking)
								jen.Select().Block(
									jen.Case(jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
										jen.Id("kind"):   jen.Qual(common.InterfacesPkg, "StreamResultKindStream"),
										jen.Id("raw"):    jen.Id("raw"),
										jen.Id("parsed"): jen.Id("parsedPtr"),
									})).Block(),
									jen.Default().Block(),
								),
							),
							jen.Return(jen.Nil()),
						),
					),
					jen.Id("err").Op("!=").Nil(),
				).Block(
					// Record fatal error but DON'T return - let defer run and continue loop
					jen.Id("errHandler").Call(jen.Id("err")),
				),
			),

			// Drain helper - processes remaining queue items, checking for adapter cancellation
			jen.Id("drain").Op(":=").Func().Params().Block(
				jen.For().Block(
					// Check if adapter is cancelled - if so, just decrement pending without processing
					jen.Select().Block(
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							// Request cancelled - decrement all remaining pending items and exit
							jen.For().Block(
								jen.List(jen.Id("_"), jen.Id("drainErr")).Op(":=").Id("rawQueue").Dot("Dequeue").Call(),
								jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(
									jen.Return(), // Queue empty
								),
								jen.Block(decrementPending...),
							),
						),
						jen.Default().Block(),
					),

					jen.List(jen.Id("remaining"), jen.Id("drainErr")).Op(":=").Id("rawQueue").Dot("Dequeue").Call(),
					jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(
						jen.Return(), // Queue empty
					),
					jen.List(jen.Id("raw"), jen.Id("ok")).Op(":=").Id("remaining").Assert(jen.String()),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Block(decrementPending...),
						jen.Continue(),
					),
					jen.Id("process").Call(jen.Id("raw")),
				),
			),

			// Main loop: wait for items from queue until context is cancelled
			// Entire loop iteration wrapped in gorecovery.Call1 to catch panics from queue operations
			jen.For().Block(
				jen.List(jen.Id("shouldExit"), jen.Id("err")).Op(":=").Qual("github.com/gregwebs/go-recovery", "Call1").Types(jen.Bool()).Call(
					jen.Func().Params().Params(jen.Bool(), jen.Error()).Block(
						jen.List(jen.Id("item"), jen.Id("err")).Op(":=").Id("rawQueue").Dot("DequeueOrWaitForNextElementContext").Call(jen.Id("queueCtx")),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							// Context cancelled - drain remaining items and exit
							jen.Id("drain").Call(),
							jen.Return(jen.True(), jen.Nil()),
						),
						jen.List(jen.Id("raw"), jen.Id("ok")).Op(":=").Id("item").Assert(jen.String()),
						jen.If(jen.Op("!").Id("ok")).Block(
							jen.Block(decrementPending...),
							jen.Return(jen.False(), jen.Nil()), // Continue loop
						),
						jen.Id("process").Call(jen.Id("raw")),
						jen.Return(jen.False(), jen.Nil()),
					),
				),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Id("errHandler").Call(jen.Id("err")),
					jen.Continue(), // Keep looping after panic
				),
				jen.If(jen.Id("shouldExit")).Block(
					jen.Return(jen.Nil()),
				),
			),
			jen.Return(jen.Nil()),
		}

		// Build the stream drain goroutine - drains stream channel, waits for partials via two-phase shutdown, emits final/error
		streamDrainBody := []jen.Code{
			// Call the Stream method with WithOnTick
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(streamCallParams...),

			// If initial stream creation failed, shutdown and emit error
			jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
				jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("streamErr"))).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
				),
				jen.Return(jen.Nil()),
			),

			// Drain the stream channel - ignore partials (we get ours from ParseStream)
			jen.Var().Id("result").Any(),
			jen.Var().Id("streamErr2").Error(),
			jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
				jen.If(jen.Id("streamVal").Dot("IsError")).Block(
					jen.Id("streamErr2").Op("=").Id("streamVal").Dot("Error"),
					jen.Continue(),
				),
				jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
					jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
				),
				// Ignore non-final partials - our partials come from the ParseStream goroutine
			),

			// Perform two-phase shutdown - waits for all ticks and pending items
			jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),

			// Check if there was a fatal error from one of the goroutines
			jen.Id("fatalMu").Dot("Lock").Call(),
			jen.Id("fatalErrCopy").Op(":=").Id("fatalErr"),
			jen.Id("fatalMu").Dot("Unlock").Call(),
			jen.If(jen.Id("fatalErrCopy").Op("!=").Nil()).Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("fatalErrCopy"))).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
				),
				jen.Return(jen.Nil()),
			),

			// Now emit final/error (after all partials have been sent)
			jen.If(jen.Id("streamErr2").Op("!=").Nil()).Block(
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("streamErr2"))).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
				),
				jen.Return(jen.Nil()),
			),

			// Get the final raw response
			jen.Id("mu").Dot("Lock").Call(),
			jen.Id("finalRaw").Op(":=").Id("lastRawResponse"),
			jen.Id("mu").Dot("Unlock").Call(),

			// Send final result - handle both pointer and value types from streamVal.Final()
			jen.Var().Id("finalParsed").Any(),
			jen.If(jen.Id("result").Op("!=").Nil()).Block(
				// First try pointer type (BAML often returns pointers from Final())
				jen.If(
					jen.List(jen.Id("ptr"), jen.Id("ok")).Op(":=").Id("result").Assert(finalTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					jen.Id("finalParsed").Op("=").Id("ptr"),
				).Else().If(
					jen.List(jen.Id("val"), jen.Id("ok")).Op(":=").Id("result").Assert(finalType.statement.Clone()),
					jen.Id("ok"),
				).Block(
					jen.Id("finalParsed").Op("=").Op("&").Id("val"),
				).Else().Block(
					jen.Id("finalParsed").Op("=").Id("result"),
				),
			),
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
					jen.Id("kind"):   jen.Qual(common.InterfacesPkg, "StreamResultKindFinal"),
					jen.Id("raw"):    jen.Id("finalRaw"),
					jen.Id("parsed"): jen.Id("finalParsed"),
				})).Block(),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
			),

			jen.Return(jen.Nil()),
		}

		// Define the onTick callback variable
		methodBody = append(methodBody,
			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("ctx").Qual("context", "Context"),
				jen.Id("reason").Qual(BamlPkg, "TickReason"),
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(onTickBody...),
		)

		// Start the partials processing goroutine wrapped in GoHandler for panic safety
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					jen.Id("errHandler"),
					jen.Func().Params().Error().Block(partialsGoroutineBody...),
				),
			).Call(),
		)

		// Shutdown call for error handler - uses shutdownOnce to ensure only one shutdown runs
		doShutdownCode := []jen.Code{
			jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
		}

		// Start the stream drain goroutine (drains stream, cancels queue context, waits for partials, emits final)
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					// Error handler function - performs full shutdown then emits error
					jen.Func().Params(jen.Id("err").Error()).Block(
						append(doShutdownCode,
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("err"))).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
							),
						)...,
					),
					// Main goroutine function
					jen.Func().Params().Error().Block(streamDrainBody...),
				),
			).Call(),
		)

		// Return the channel
		methodBody = append(methodBody,
			jen.Return(jen.Id("out"), jen.Nil()),
		)

		out.Func().
			Id(methodName).
			Params(
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
				jen.Id("rawInput").Any(),
			).
			Call(
				jen.List(
					jen.Op("<-").Chan().Add(streamResultInterface.Clone()),
					jen.Error(),
				)).
			Block(methodBody...)

		methods = append(methods, methodOut{
			name:             methodName,
			inputStructName:  inputStructName,
			outputStructQual: finalResultType,
		})
	}

	// Generate the map of methods
	streamingFunctionInterface := jen.Qual(common.InterfacesPkg, "StreamingMethod")

	mapElements := make(jen.Dict)
	for _, method := range methods {
		mapElements[jen.Lit(method.name)] = jen.Values(jen.Dict{
			jen.Id("MakeInput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(jen.Id(method.inputStructName))),
				),
			jen.Id("MakeOutput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(method.outputStructQual)),
				),
			jen.Id("Impl"): jen.Id(method.name),
		})
	}

	out.Var().Id("Methods").Op("=").
		Map(jen.String()).Add(streamingFunctionInterface).
		Values(mapElements)

	// Generate `MakeAdapter`
	out.Func().Id("MakeAdapter").
		Params(jen.Id("ctx").Qual("context", "Context")).
		Qual(common.InterfacesPkg, "Adapter").
		Block(
			jen.Return(
				jen.Op("&").Qual(selfAdapterPkg, "BamlAdapter").
					Values(jen.Dict{
						jen.Id("Context"): jen.Id("ctx"),
						jen.Id("TypeBuilderFactory"): jen.Func().Params().
							Call(
								jen.Qual(common.InterfacesPkg, "BamlTypeBuilder"),
								jen.Error(),
							).
							Block(
								jen.Return(jen.Qual(common.GeneratedClientPkg, "NewTypeBuilder").Call()),
							),
					}),
			),
		)

	// Generate `makeOptionsFromAdapter`
	out.Func().Id("makeOptionsFromAdapter").
		Params(jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter")).
		Params(
			jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.Error(),
		).
		Block(
			jen.List(jen.Id("adapter"), jen.Id("ok")).Op(":=").Id("adapterIn").Assert(jen.Op("*").Qual(selfAdapterPkg, "BamlAdapter")),
			jen.If(jen.Op("!").Id("ok")).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
					jen.Lit("invalid adapter type: expected *BamlAdapter, got %T"),
					jen.Id("adapterIn"),
				)),
			),
			jen.Var().Id("result").Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
			jen.If(jen.Id("adapter").Dot("ClientRegistry").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").
						Call(jen.Id("adapter").Dot("ClientRegistry"))),
			),
			jen.If(jen.Id("adapter").Dot("TypeBuilder").Op("!=").Nil()).Block(
				jen.List(jen.Id("typeBuilder"), jen.Id("ok")).Op(":=").Id("adapter").Dot("TypeBuilder").Assert(jen.Op("*").Qual(common.GeneratedClientPkg, "TypeBuilder")),
				jen.If(jen.Op("!").Id("ok")).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(
						jen.Lit("invalid TypeBuilder type: expected *TypeBuilder, got %T"),
						jen.Id("adapter").Dot("TypeBuilder"),
					)),
				),
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").
						Call(jen.Id("typeBuilder"))),
			),
			jen.Return(jen.Id("result"), jen.Nil()),
		)

	if err := common.Commit(out); err != nil {
		panic(err)
	}
}

type parsedReflectType struct {
	statement *jen.Statement
	generics  []jen.Code
}

func parseReflectType(typ reflect.Type) parsedReflectType {
	var ops []string
	for {
		if typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
			ops = append(ops, "*")
		} else if typ.Kind() == reflect.Slice {
			typ = typ.Elem()
			ops = append(ops, "[]")
		} else {
			break
		}
	}

	pkgPath := typ.PkgPath()
	typeName := typ.Name()

	genericsStartIdx := strings.Index(typeName, "[")
	genericsEndIdx := strings.LastIndex(typeName, "]")

	var genericsTypes []jen.Code
	if genericsStartIdx != -1 && genericsEndIdx != -1 {
		genericsStr := typeName[genericsStartIdx+1 : genericsEndIdx]
		typeName = typeName[:genericsStartIdx]

		genericsEntries := strings.Split(genericsStr, ",")

		for _, entry := range genericsEntries {
			operator := ""
			pkgName := ""
			ident := entry

			lastDot := strings.LastIndex(entry, ".")
			if lastDot != -1 {
				pkgName = entry[:lastDot]
				ident = entry[lastDot+1:]
			}

			firstChar := strings.IndexFunc(pkgName, unicode.IsLetter)
			if firstChar > 0 {
				operator = pkgName[:firstChar]
				pkgName = pkgName[firstChar:]
			}

			var current *jen.Statement
			if pkgName == "" {
				current = jen.Id(ident)
			} else {
				current = jen.Qual(pkgName, ident)
			}

			if operator != "" {
				current = jen.Op(operator).Add(current)
			}

			genericsTypes = append(genericsTypes, current)
		}
	}

	var statement *jen.Statement
	if pkgPath == "" {
		statement = jen.Id(typeName)
	} else {
		statement = jen.Qual(pkgPath, typeName)
	}

	for _, op := range slices.Backward(ops) {
		statement = jen.Op(op).Add(statement)
	}

	return parsedReflectType{
		statement.Types(genericsTypes...),
		genericsTypes,
	}
}

func hasDynamicProperties(typ reflect.Type) bool {
	for typ.Kind() != reflect.Ptr {
		typ = reflect.PointerTo(typ)
	}

	finalMethod, hasFinalMethod := typ.MethodByName("Final")
	if !hasFinalMethod {
		return false
	}

	finalMethodType := finalMethod.Type
	if finalMethodType.NumOut() != 1 {
		return false
	}

	returnType := finalMethodType.Out(0).Elem()
	if returnType.Kind() == reflect.Ptr {
		returnType = returnType.Elem()
	}

	if returnType.Kind() != reflect.Struct {
		return false
	}

	_, hasDynamicFields := returnType.FieldByName("DynamicProperties")
	return hasDynamicFields
}

// hasDynamicPropertiesForType checks if a type directly has DynamicProperties field
func hasDynamicPropertiesForType(typ reflect.Type) bool {
	// Unwrap pointer types
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return false
	}

	_, hasDynamicFields := typ.FieldByName("DynamicProperties")
	return hasDynamicFields
}
