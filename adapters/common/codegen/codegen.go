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
			jen.Id("reset").Bool(), // true when client should discard accumulated state (retry occurred)
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
								// Try reflect.Value first (old BAML behavior)
								jen.If(
									jen.List(jen.Id("reflectValue"), jen.Id("ok")).Op(":=").Id("value").Assert(jen.Qual("reflect", "Value")),
									jen.Id("ok"),
								).Block(
									jen.Id("result").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("reflectValue").Dot("Interface").Call()),
								).Else().Block(
									// Otherwise unwrap directly (BAML 0.215.0+ behavior)
									jen.Id("result").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("value")),
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

		// Reset() method - returns true if client should discard accumulated state
		out.Func().
			Params(selfParam.Clone()).
			Id("Reset").Params().
			Bool().
			Block(
				jen.Return(selfName.Clone().Dot("reset")),
			)

		// Generate pool for output struct reuse
		poolVarName := strcase.LowerCamelCase(fmt.Sprintf("%sPool", outputStructName))
		out.Var().Id(poolVarName).Op("=").Qual(common.InterfacesPkg, "NewPool").Call(
			jen.Func().Params().Op("*").Id(outputStructName).Block(
				jen.Return(jen.Op("&").Id(outputStructName).Values()),
			),
		)

		// Release() method - returns struct to pool
		// Uses struct reset (*v = T{}) instead of field-by-field for future-proofing
		out.Func().
			Params(selfParam.Clone()).
			Id("Release").Params().
			Block(
				jen.If(selfName.Clone().Op("==").Nil()).Block(
					jen.Return(),
				),
				// Reset entire struct before returning to pool
				jen.Op("*").Add(selfName.Clone()).Op("=").Id(outputStructName).Values(),
				jen.Id(poolVarName).Dot("Put").Call(selfName.Clone()),
			)

		// Generate getter function for output struct
		getterFuncName := strcase.LowerCamelCase(fmt.Sprintf("get%s", outputStructName))
		out.Func().
			Id(getterFuncName).
			Params().
			Op("*").Id(outputStructName).
			Block(
				jen.Return(jen.Id(poolVarName).Dot("Get").Call()),
			)

		// Generate error constructor function
		errorConstructorName := strcase.LowerCamelCase(fmt.Sprintf("new%sError", outputStructName))
		out.Func().
			Id(errorConstructorName).
			Params(jen.Id("err").Error()).
			Op("*").Id(outputStructName).
			Block(
				jen.Id("r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindError"),
				jen.Id("r").Dot("err").Op("=").Id("err"),
				jen.Return(jen.Id("r")),
			)

		streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

		// Helper: common preamble for both implementations
		// Inner methods return only error, so early returns just return the error
		makePreamble := func() []jen.Code {
			preamble := []jen.Code{
				jen.List(jen.Id("options"), jen.Id("err")).Op(":=").Id("makeOptionsFromAdapter").Call(jen.Id("adapter")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Id("err")),
				),
			}
			if len(args) > 0 {
				preamble = append(preamble,
					jen.List(jen.Id("input"), jen.Id("ok")).Op(":=").Id("rawInput").Assert(jen.Op("*").Id(inputStructName)),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Return(jen.Qual("fmt", "Errorf").Call(
							jen.Lit("invalid input type: expected *%s, got %T"),
							jen.Lit(inputStructName),
							jen.Id("rawInput"),
						)),
					),
				)
			}
			return preamble
		}

		// Build call parameters for Stream method
		var streamCallParams []jen.Code
		streamCallParams = append(streamCallParams, jen.Id("adapter")) // context
		for _, arg := range args {
			argName := strcase.UpperCamelCase(arg)
			streamCallParams = append(streamCallParams, jen.Id("input").Dot(argName))
		}
		streamCallParams = append(streamCallParams,
			jen.Append(
				jen.Index().Qual(common.GeneratedClientPkg, "CallOptionFunc").Values(
					jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
				),
				jen.Id("options").Op("..."),
			).Op("..."),
		)

		// ====== SIMPLIFIED IMPLEMENTATION: methodName_noRaw ======
		// This path uses BAML's native streaming without raw collection overhead.
		// Partials come directly from BAML's stream, no OnTick/SSE parsing needed.
		noRawMethodName := strcase.LowerCamelCase(methodName + "_noRaw")

		// Build call parameters for Stream method WITH OnTick for heartbeat tracking
		// The OnTick callback is lightweight - it only sends a heartbeat on first invocation
		var streamCallParamsNoOnTick []jen.Code
		streamCallParamsNoOnTick = append(streamCallParamsNoOnTick, jen.Id("adapter")) // context
		for _, arg := range args {
			argName := strcase.UpperCamelCase(arg)
			streamCallParamsNoOnTick = append(streamCallParamsNoOnTick, jen.Id("input").Dot(argName))
		}
		streamCallParamsNoOnTick = append(streamCallParamsNoOnTick,
			jen.Append(
				jen.Index().Qual(common.GeneratedClientPkg, "CallOptionFunc").Values(
					jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
				),
				jen.Id("options").Op("..."),
			).Op("..."),
		)

		// noRaw goroutine body - wrapped in gorecovery.GoHandler for panic resilience
		noRawGoroutineBody := []jen.Code{
			// Call Stream WITH OnTick for heartbeat tracking, but still use native streaming for data
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(streamCallParamsNoOnTick...),

			// If stream creation failed, emit error
			jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
				jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("streamErr")),
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
					jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("streamVal").Dot("Error")),
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
				// Final() returns *TFinal - already a pointer, use directly
				jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
					jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindFinal"),
					jen.Id("__r").Dot("parsed").Op("=").Id("streamVal").Dot("Final").Call(),
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
							jen.Id("__r").Dot("Release").Call(),
							jen.Return(jen.Nil()),
						),
					),
					jen.Continue(),
				),

				// Handle partial - forward BAML's native partial via Stream()
				// Stream() returns *TStream - already a pointer, use directly
				jen.If(jen.Id("partial").Op(":=").Id("streamVal").Dot("Stream").Call(), jen.Id("partial").Op("!=").Nil()).Block(
					jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
					jen.Id("__r").Dot("parsed").Op("=").Id("partial"),
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

			jen.Return(jen.Nil()),
		}

		noRawBody := makePreamble()

		// Add heartbeat tracking state - sends a single heartbeat when first data is received
		noRawBody = append(noRawBody,
			jen.Var().Id("heartbeatSent").Qual("sync/atomic", "Bool"),
		)

		// Define lightweight onTick callback that only sends heartbeat on first invocation
		// Unlike the _full implementation, this doesn't process FunctionLog - it just signals "data received"
		noRawOnTickBody := []jen.Code{
			jen.If(jen.Id("heartbeatSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
				// Check adapter.Done() first to avoid race with channel close on cancellation
				jen.Select().Block(
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Return(jen.Nil()),
					),
					jen.Default().Block(),
				),
				// Send heartbeat - non-blocking to avoid blocking BAML
				jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindHeartbeat"),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
					jen.Default().Block(
						jen.Id("__r").Dot("Release").Call(),
					),
				),
			),
			jen.Return(jen.Nil()),
		}

		noRawBody = append(noRawBody,
			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("_").Qual("context", "Context"),
				jen.Id("_").Qual(BamlPkg, "TickReason"),
				jen.Id("_").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(noRawOnTickBody...),
		)

		noRawBody = append(noRawBody,
			// Stream goroutine - uses BAML's native streaming, forwards partials directly
			// Wrapped in gorecovery.GoHandler for panic resilience
			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					// Error handler - emit panic as error to output channel
					jen.Func().Params(jen.Id("err").Error()).Block(
						jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("err")),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
								jen.Id("__errR").Dot("Release").Call(),
							),
						),
					),
					// Main goroutine function
					jen.Func().Params().Error().Block(noRawGoroutineBody...),
				),
			).Call(),

			jen.Return(jen.Nil()),
		)

		// Generate the noRaw implementation function
		// Accepts output channel from caller, returns only error
		out.Func().
			Id(noRawMethodName).
			Params(
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
				jen.Id("rawInput").Any(),
				jen.Id("out").Chan().Add(streamResultInterface.Clone()),
			).
			Error().
			Block(noRawBody...)

		// ====== FULL IMPLEMENTATION: methodName_full ======
		// This path uses the ParseStream goroutine for partial results.
		// When skipIntermediateParsing is true, it skips ParseStream calls and intermediate emissions
		// (used for FINAL_ONLY mode where we only need the final result + raw).
		fullMethodName := strcase.LowerCamelCase(methodName + "_full")

		var fullBody []jen.Code
		fullBody = append(fullBody, makePreamble()...)

		fullBody = append(fullBody,
			// Unbounded queue for passing FunctionLog from OnTick to partials goroutine (never drops)
			jen.Id("funcLogQueue").Op(":=").Qual(common.GoConcurrentQueuePkg, "NewFIFO").Call(),
			// Heartbeat tracking - sends heartbeat when first data is received (for first-byte detection)
			jen.Var().Id("heartbeatSent").Qual("sync/atomic", "Bool"),
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

			// Incremental extractor for SSE chunks (tracks cursor to avoid re-parsing)
			jen.Id("extractor").Op(":=").Qual(common.SSEPkg, "NewIncrementalExtractor").Call(),
			jen.Var().Id("extractorMu").Qual("sync", "Mutex"),

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
		fullBody = append(fullBody,
			jen.Go().Func().Params().Block(
				jen.Op("<-").Id("adapter").Dot("Done").Call(),
				jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
			).Call(),
		)

		// Build the onTick callback - enqueues FunctionLog for async processing
		// IMPORTANT: OnTick is called synchronously by BAML, so we must return ASAP
		// All data extraction happens in the partials goroutine, not here
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

						// Reserve pending slot, then enqueue the FunctionLog directly
						// All data extraction happens in the partials goroutine
						// If enqueue fails (shouldn't happen with FIFO), rollback pending
						jen.Id("pending").Dot("Add").Call(jen.Lit(1)),
						jen.If(jen.Id("err").Op(":=").Id("funcLogQueue").Dot("Enqueue").Call(jen.Id("funcLog")), jen.Id("err").Op("!=").Nil()).Block(
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

		// Build the partials processing goroutine body - reads FunctionLog from queue, extracts data, calls ParseStream, emits partials
		// Each item is processed in a separate function call so defers run per-item
		// Per-item panics are caught and reported but don't exit the loop
		// The entire loop iteration is wrapped in gorecovery.Call to catch panics from queue operations
		partialsGoroutineBody := []jen.Code{
			// Define process function - handles one queue item with defer and panic recovery
			jen.Id("process").Op(":=").Func().Params(jen.Id("funcLog").Qual(BamlPkg, "FunctionLog")).Block(
				// Defer pending decrement - runs when THIS function returns (per-item)
				jen.Defer().Func().Params().Block(decrementPending...).Call(),

				// Wrap extraction+parse+send in gorecovery.Call - if panic, call errHandler but DON'T exit
				jen.If(
					jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
						jen.Func().Params().Error().Block(
							// Send heartbeat on first data (for first-byte detection)
							jen.If(jen.Id("heartbeatSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
								jen.Select().Block(
									jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
										jen.Return(jen.Nil()),
									),
									jen.Default().Block(),
								),
								jen.Id("__hb").Op(":=").Id(getterFuncName).Call(),
								jen.Id("__hb").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindHeartbeat"),
								jen.Select().Block(
									jen.Case(jen.Id("out").Op("<-").Id("__hb")).Block(),
									jen.Default().Block(
										jen.Id("__hb").Dot("Release").Call(),
									),
								),
							),

							// Extract data from FunctionLog (moved from OnTick for async processing)
							// Only fetch data for the LAST call - earlier calls are failed retries
							jen.Id("calls").Op(",").Id("callsErr").Op(":=").Id("funcLog").Dot("Calls").Call(),
							jen.If(jen.Id("callsErr").Op("!=").Nil()).Block(
								jen.Return(jen.Nil()),
							),
							jen.Id("callCount").Op(":=").Len(jen.Id("calls")),
							jen.If(jen.Id("callCount").Op("==").Lit(0)).Block(
								jen.Return(jen.Nil()),
							),

							// Get only the last call (current attempt) - avoid FFI calls for failed retries
							jen.Id("lastCall").Op(":=").Id("calls").Index(jen.Id("callCount").Op("-").Lit(1)),
							jen.Id("streamCall").Op(",").Id("ok").Op(":=").Id("lastCall").Assert(jen.Qual(BamlPkg, "LLMStreamCall")),
							jen.If(jen.Op("!").Id("ok")).Block(
								jen.Return(jen.Nil()),
							),
							jen.Id("provider").Op(",").Id("provErr").Op(":=").Id("streamCall").Dot("Provider").Call(),
							jen.If(jen.Id("provErr").Op("!=").Nil()).Block(
								jen.Return(jen.Nil()),
							),
							jen.Id("chunks").Op(",").Id("chunksErr").Op(":=").Id("streamCall").Dot("SSEChunks").Call(),
							jen.If(jen.Id("chunksErr").Op("!=").Nil()).Block(
								jen.Return(jen.Nil()),
							),

							// Convert chunks to SSEChunk interface slice
							jen.Id("sseChunks").Op(":=").Make(jen.Index().Qual(common.SSEPkg, "SSEChunk"), jen.Len(jen.Id("chunks"))),
							jen.For(jen.List(jen.Id("i"), jen.Id("chunk")).Op(":=").Range().Id("chunks")).Block(
								jen.Id("sseChunks").Index(jen.Id("i")).Op("=").Id("chunk"),
							),

							// Extract new content incrementally (only processes new chunks)
							jen.Id("extractorMu").Dot("Lock").Call(),
							jen.Id("extractResult").Op(":=").Id("extractor").Dot("Extract").Call(
								jen.Id("callCount"),
								jen.Id("provider"),
								jen.Id("sseChunks"),
							),
							jen.Id("extractorMu").Dot("Unlock").Call(),

							// If skipIntermediateParsing is true, we're done - just extracted raw for final
							jen.If(jen.Id("skipIntermediateParsing")).Block(
								jen.Return(jen.Nil()),
							),

							// Skip if no new content AND no reset signal
							// (must emit reset even with empty delta so client discards stale state)
							jen.If(jen.Id("extractResult").Dot("Delta").Op("==").Lit("").Op("&&").Op("!").Id("extractResult").Dot("Reset")).Block(
								jen.Return(jen.Nil()),
							),

							jen.Id("raw").Op(":=").Id("extractResult").Dot("Full"),
							jen.Id("rawDelta").Op(":=").Id("extractResult").Dot("Delta"),

							// Short-circuit: if rawDelta is empty or raw is empty, skip ParseStream
							// (no new content to parse, or nothing accumulated yet)
							jen.If(jen.Id("rawDelta").Op("==").Lit("").Op("||").Id("raw").Op("==").Lit("")).Block(
								// Pre-check adapter cancellation
								jen.Select().Block(
									jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
										jen.Return(jen.Nil()),
									),
									jen.Default().Block(),
								),
								// Emit reset-only result (no parsed content)
								jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
								jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
								jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
								jen.Id("__r").Dot("reset").Op("=").Id("extractResult").Dot("Reset"),
								jen.Select().Block(
									jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
									jen.Default().Block(
										jen.Id("__r").Dot("Release").Call(),
									),
								),
								jen.Return(jen.Nil()),
							),

							// Call ParseStream outside of OnTick callback context (avoids deadlock)
							// Pass adapter as context (hack adds ctx param to ParseStream methods)
							jen.List(jen.Id("parsed"), jen.Id("parseErr")).Op(":=").
								Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
									jen.Id("adapter"),
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

								// Best-effort send (non-blocking) - send only delta to save bandwidth
								jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
								jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
								jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
								jen.Id("__r").Dot("parsed").Op("=").Id("parsedPtr"),
								jen.Id("__r").Dot("reset").Op("=").Id("extractResult").Dot("Reset"),
								jen.Select().Block(
									jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
									jen.Default().Block(
										jen.Id("__r").Dot("Release").Call(),
									),
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
								jen.List(jen.Id("_"), jen.Id("drainErr")).Op(":=").Id("funcLogQueue").Dot("Dequeue").Call(),
								jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(
									jen.Return(), // Queue empty
								),
								jen.Block(decrementPending...),
							),
						),
						jen.Default().Block(),
					),

					jen.List(jen.Id("remaining"), jen.Id("drainErr")).Op(":=").Id("funcLogQueue").Dot("Dequeue").Call(),
					jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(
						jen.Return(), // Queue empty
					),
					jen.List(jen.Id("funcLog"), jen.Id("ok")).Op(":=").Id("remaining").Assert(jen.Qual(BamlPkg, "FunctionLog")),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Block(decrementPending...),
						jen.Continue(),
					),
					jen.Id("process").Call(jen.Id("funcLog")),
				),
			),

			// Main loop: wait for items from queue until context is cancelled
			// Entire loop iteration wrapped in gorecovery.Call1 to catch panics from queue operations
			jen.For().Block(
				jen.List(jen.Id("shouldExit"), jen.Id("err")).Op(":=").Qual("github.com/gregwebs/go-recovery", "Call1").Types(jen.Bool()).Call(
					jen.Func().Params().Params(jen.Bool(), jen.Error()).Block(
						jen.List(jen.Id("item"), jen.Id("err")).Op(":=").Id("funcLogQueue").Dot("DequeueOrWaitForNextElementContext").Call(jen.Id("queueCtx")),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							// Context cancelled - drain remaining items and exit
							jen.Id("drain").Call(),
							jen.Return(jen.True(), jen.Nil()),
						),
						jen.List(jen.Id("funcLog"), jen.Id("ok")).Op(":=").Id("item").Assert(jen.Qual(BamlPkg, "FunctionLog")),
						jen.If(jen.Op("!").Id("ok")).Block(
							jen.Block(decrementPending...),
							jen.Return(jen.False(), jen.Nil()), // Continue loop
						),
						jen.Id("process").Call(jen.Id("funcLog")),
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
				jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("streamErr")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__errR").Dot("Release").Call(),
					),
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
				jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("fatalErrCopy")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__errR").Dot("Release").Call(),
					),
				),
				jen.Return(jen.Nil()),
			),

			// Now emit final/error (after all partials have been sent)
			jen.If(jen.Id("streamErr2").Op("!=").Nil()).Block(
				jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("streamErr2")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
						jen.Id("__errR").Dot("Release").Call(),
					),
				),
				jen.Return(jen.Nil()),
			),

			// Get the final raw response from extractor
			jen.Id("extractorMu").Dot("Lock").Call(),
			jen.Id("finalRaw").Op(":=").Id("extractor").Dot("Full").Call(),
			jen.Id("extractorMu").Dot("Unlock").Call(),

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
			jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
			jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindFinal"),
			jen.Id("__r").Dot("raw").Op("=").Id("finalRaw"),
			jen.Id("__r").Dot("parsed").Op("=").Id("finalParsed"),
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
					jen.Id("__r").Dot("Release").Call(),
				),
			),

			jen.Return(jen.Nil()),
		}

		// Define the onTick callback variable
		fullBody = append(fullBody,
			jen.Id("onTick").Op(":=").Func().Params(
				jen.Id("ctx").Qual("context", "Context"),
				jen.Id("reason").Qual(BamlPkg, "TickReason"),
				jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
			).Qual(BamlPkg, "FunctionSignal").Block(onTickBody...),
		)

		// Start the partials processing goroutine wrapped in GoHandler for panic safety
		fullBody = append(fullBody,
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
		fullBody = append(fullBody,
			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					// Error handler function - performs full shutdown then emits error
					jen.Func().Params(jen.Id("err").Error()).Block(
						append(doShutdownCode,
							jen.Id("__errR").Op(":=").Id(errorConstructorName).Call(jen.Id("err")),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
									jen.Id("__errR").Dot("Release").Call(),
								),
							),
						)...,
					),
					// Main goroutine function
					jen.Func().Params().Error().Block(streamDrainBody...),
				),
			).Call(),
		)

		// Return the channel
		fullBody = append(fullBody,
			jen.Return(jen.Nil()),
		)

		// Generate the full implementation function
		// Accepts output channel from caller, returns only error
		// skipIntermediateParsing: when true, skips ParseStream and intermediate emissions (for FINAL_ONLY mode)
		out.Func().
			Id(fullMethodName).
			Params(
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
				jen.Id("rawInput").Any(),
				jen.Id("out").Chan().Add(streamResultInterface.Clone()),
				jen.Id("skipIntermediateParsing").Bool(),
			).
			Error().
			Block(fullBody...)

		// Generate the public router function that dispatches based on RawCollectionMode()
		// Creates the output channel and passes it to the inner implementation
		routerBody := []jen.Code{
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
			jen.Var().Id("err").Error(),
			jen.Switch(jen.Id("adapter").Dot("RawCollectionMode").Call()).Block(
				jen.Case(jen.Qual(common.InterfacesPkg, "RawCollectionNone")).Block(
					jen.Id("err").Op("=").Id(noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out")),
				),
				jen.Case(jen.Qual(common.InterfacesPkg, "RawCollectionFinalOnly")).Block(
					// FINAL_ONLY mode: use _full with skipIntermediateParsing=true
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True()),
				),
				jen.Case(jen.Qual(common.InterfacesPkg, "RawCollectionAll")).Block(
					// ALL mode: use _full with skipIntermediateParsing=false
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False()),
				),
			),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
			jen.Return(jen.Id("out"), jen.Nil()),
		}

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
			Block(routerBody...)

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

	// Generate Parse methods
	type parseMethodOut struct {
		name             string
		outputStructQual jen.Code
	}
	var parseMethods []parseMethodOut

	for methodName := range introspected.ParseMethods {
		// Get the sync function to determine return type
		syncFuncValue, ok := introspected.SyncFuncs[methodName]
		if !ok {
			continue
		}

		syncFuncType := reflect.TypeOf(syncFuncValue)
		if syncFuncType.Kind() != reflect.Func || syncFuncType.NumOut() < 1 {
			continue
		}

		// Get the return type (first return value)
		finalResultType := parseReflectType(syncFuncType.Out(0)).statement

		// Generate the parse function: parse{MethodName}
		parseFuncName := strcase.LowerCamelCase("parse_" + methodName)

		// Build call parameters for Parse method
		var parseCallParams []jen.Code
		parseCallParams = append(parseCallParams, jen.Id("adapter")) // context
		parseCallParams = append(parseCallParams, jen.Id("raw"))     // raw string
		parseCallParams = append(parseCallParams,
			jen.Id("options").Op("..."),
		)

		parseBody := []jen.Code{
			jen.List(jen.Id("options"), jen.Id("err")).Op(":=").Id("makeOptionsFromAdapter").Call(jen.Id("adapter")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
			jen.List(jen.Id("result"), jen.Id("parseErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Parse").Dot(methodName).Call(parseCallParams...),
			jen.If(jen.Id("parseErr").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("parseErr")),
			),
			jen.Return(jen.Id("result"), jen.Nil()),
		}

		out.Func().
			Id(parseFuncName).
			Params(
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
				jen.Id("raw").String(),
			).
			Params(jen.Any(), jen.Error()).
			Block(parseBody...)

		parseMethods = append(parseMethods, parseMethodOut{
			name:             methodName,
			outputStructQual: finalResultType,
		})
	}

	// Generate the ParseMethods map
	parseMethodInterface := jen.Qual(common.InterfacesPkg, "ParseMethod")

	parseMapElements := make(jen.Dict)
	for _, method := range parseMethods {
		parseFuncName := strcase.LowerCamelCase("parse_" + method.name)
		parseMapElements[jen.Lit(method.name)] = jen.Values(jen.Dict{
			jen.Id("MakeOutput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(method.outputStructQual)),
				),
			jen.Id("Impl"): jen.Id(parseFuncName),
		})
	}

	out.Var().Id("ParseMethods").Op("=").
		Map(jen.String()).Add(parseMethodInterface).
		Values(parseMapElements)

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

	// Generate `InitBamlRuntime` - wrapper for baml_client.InitRuntime()
	out.Func().Id("InitBamlRuntime").
		Params().
		Block(
			jen.Qual(common.GeneratedClientPkg, "InitRuntime").Call(),
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
