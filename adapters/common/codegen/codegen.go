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

		// Initialize state for streaming
		streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

		methodBody = append(methodBody,
			// Output channel for results
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),

			// Shutdown state
			jen.Var().Id("stopping").Qual("sync/atomic", "Bool"),  // shutdown requested
			jen.Var().Id("inTick").Qual("sync/atomic", "Int64"),   // number of onTick currently executing
			jen.Id("ticksDone").Op(":=").Make(jen.Chan().Struct()), // closed when stopping && inTick==0
			jen.Var().Id("ticksOnce").Qual("sync", "Once"),
			jen.Var().Id("shutdownOnce").Qual("sync", "Once"),

			// Fatal error storage
			jen.Var().Id("fatalMu").Qual("sync", "Mutex"),
			jen.Var().Id("fatalErr").Error(),

			// Mutex and lastRawResponse for final raw capture
			jen.Var().Id("lastRawResponse").String(),
			jen.Var().Id("mu").Qual("sync", "Mutex"),

			// Tick counter for logging
			jen.Var().Id("tickCount").Qual("sync/atomic", "Int64"),

			// Shutdown function - waits for all in-flight ticks to complete
			jen.Id("doShutdown").Op(":=").Func().Params().Block(
				jen.Qual("log", "Println").Call(jen.Lit("[SHUTDOWN] doShutdown called")),
				jen.Id("stopping").Dot("Store").Call(jen.True()),
				jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
					jen.Qual("log", "Println").Call(jen.Lit("[SHUTDOWN] No ticks in flight, closing ticksDone immediately")),
					jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
						jen.Close(jen.Id("ticksDone")),
					)),
				).Else().Block(
					jen.Qual("log", "Printf").Call(jen.Lit("[SHUTDOWN] Waiting for %d in-flight ticks to complete..."), jen.Id("inTick").Dot("Load").Call()),
					jen.Op("<-").Id("ticksDone"),
					jen.Qual("log", "Println").Call(jen.Lit("[SHUTDOWN] All ticks completed")),
				),
			),

			// Error handler for goroutine panics - records error and initiates shutdown
			jen.Id("errHandler").Op(":=").Func().Params(jen.Id("err").Error()).Block(
				jen.Qual("log", "Printf").Call(jen.Lit("[ERROR] errHandler called with: %v"), jen.Id("err")),
				jen.Id("fatalMu").Dot("Lock").Call(),
				jen.If(jen.Id("fatalErr").Op("==").Nil()).Block(
					jen.Id("fatalErr").Op("=").Id("err"),
				),
				jen.Id("fatalMu").Dot("Unlock").Call(),

				// Initiate shutdown
				jen.If(jen.Id("stopping").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
					jen.Qual("log", "Println").Call(jen.Lit("[ERROR] Initiating shutdown due to error")),
					jen.If(jen.Id("inTick").Dot("Load").Call().Op("==").Lit(0)).Block(
						jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
							jen.Close(jen.Id("ticksDone")),
						)),
					),
					jen.Go().Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
				),
			),
		)

		// Watcher goroutine: trigger shutdown when adapter (HTTP request) is cancelled
		methodBody = append(methodBody,
			jen.Qual("log", "Println").Call(jen.Lit("[INIT] Starting request processing")),
			jen.Go().Func().Params().Block(
				jen.Op("<-").Id("adapter").Dot("Done").Call(),
				jen.Qual("log", "Println").Call(jen.Lit("[WATCHER] Adapter context cancelled, triggering shutdown")),
				jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
			).Call(),
		)

		// Build the onTick callback - extracts raw text and calls ParseStream directly
		// Uses panic recovery and inTick barrier for safe shutdown
		onTickBody := []jen.Code{
			// Wrap in gorecovery.Call for panic safety
			jen.If(
				jen.Id("err").Op(":=").Qual("github.com/gregwebs/go-recovery", "Call").Call(
					jen.Func().Params().Error().Block(
						// CRITICAL: increment inTick FIRST, before any checks
						jen.Id("currentTick").Op(":=").Id("tickCount").Dot("Add").Call(jen.Lit(1)),
						jen.Id("inTick").Dot("Add").Call(jen.Lit(1)),
						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] onTick started, reason=%v, inTick=%d"), jen.Id("currentTick"), jen.Id("reason"), jen.Id("inTick").Dot("Load").Call()),

						// Defer exit: decrement counter and close ticksDone if we're the last and stopping
						jen.Defer().Func().Params().Block(
							jen.Id("remaining").Op(":=").Id("inTick").Dot("Add").Call(jen.Lit(-1)),
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] onTick finished, remaining inTick=%d, stopping=%v"), jen.Id("currentTick"), jen.Id("remaining"), jen.Id("stopping").Dot("Load").Call()),
							jen.If(
								jen.Id("remaining").Op("==").Lit(0).Op("&&").Id("stopping").Dot("Load").Call(),
							).Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Last tick completed during shutdown, closing ticksDone"), jen.Id("currentTick")),
								jen.Id("ticksOnce").Dot("Do").Call(jen.Func().Params().Block(
									jen.Close(jen.Id("ticksDone")),
								)),
							),
						).Call(),

						// Check if we should bail out
						jen.If(jen.Id("stopping").Dot("Load").Call()).Block(
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Bailing out - shutdown in progress"), jen.Id("currentTick")),
							jen.Return(jen.Nil()),
						),
						jen.Select().Block(
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Bailing out - adapter cancelled"), jen.Id("currentTick")),
								jen.Return(jen.Nil()),
							),
							jen.Default().Block(),
						),

						// Build StreamingData from FunctionLog
						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Extracting calls from funcLog..."), jen.Id("currentTick")),
						jen.Id("calls").Op(",").Id("callsErr").Op(":=").Id("funcLog").Dot("Calls").Call(),
						jen.If(jen.Id("callsErr").Op("!=").Nil()).Block(
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Error getting calls: %v"), jen.Id("currentTick"), jen.Id("callsErr")),
							jen.Return(jen.Nil()),
						),
						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Got %d calls"), jen.Id("currentTick"), jen.Len(jen.Id("calls"))),

						// Build StreamingData struct
						jen.Id("streamingData").Op(":=").Op("&").Qual(common.SSEPkg, "StreamingData").Values(),
						jen.For(jen.List(jen.Id("_"), jen.Id("call")).Op(":=").Range().Id("calls")).Block(
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
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Error getting content: %v"), jen.Id("currentTick"), jen.Id("rawErr")),
							jen.Return(jen.Nil()),
						),

						// Skip if no content yet
						jen.If(jen.Id("raw").Op("==").Lit("")).Block(
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] No content yet, skipping"), jen.Id("currentTick")),
							jen.Return(jen.Nil()),
						),

						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Got raw content (%d bytes): %.100s..."), jen.Id("currentTick"), jen.Len(jen.Id("raw")), jen.Id("raw")),

						// Update lastRawResponse for final capture
						jen.Id("mu").Dot("Lock").Call(),
						jen.Id("lastRawResponse").Op("=").Id("raw"),
						jen.Id("mu").Dot("Unlock").Call(),

						// Check again before parsing
						jen.Select().Block(
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Adapter cancelled before parse"), jen.Id("currentTick")),
								jen.Return(jen.Nil()),
							),
							jen.Default().Block(),
						),

						// Call ParseStream directly
						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Calling ParseStream..."), jen.Id("currentTick")),
						jen.List(jen.Id("parsed"), jen.Id("parseErr")).Op(":=").
							Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
								jen.Id("adapter"),
								jen.Id("raw"),
								jen.Id("options").Op("..."),
							),

						jen.If(jen.Id("parseErr").Op("!=").Nil()).Block(
							jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] ParseStream error: %v"), jen.Id("currentTick"), jen.Id("parseErr")),
							jen.Return(jen.Nil()),
						),

						jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] ParseStream succeeded, sending to output..."), jen.Id("currentTick")),

						// Pre-check: if adapter is cancelled, don't attempt send
						jen.Select().Block(
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Adapter cancelled before send"), jen.Id("currentTick")),
								jen.Return(jen.Nil()),
							),
							jen.Default().Block(),
						),

						// Take address of parsed result for consistency
						jen.Id("parsedPtr").Op(":=").Op("&").Id("parsed"),

						// Best-effort send (non-blocking)
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
								jen.Id("kind"):   jen.Qual(common.InterfacesPkg, "StreamResultKindStream"),
								jen.Id("raw"):    jen.Id("raw"),
								jen.Id("parsed"): jen.Id("parsedPtr"),
							})).Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Sent partial to output channel"), jen.Id("currentTick")),
							),
							jen.Default().Block(
								jen.Qual("log", "Printf").Call(jen.Lit("[TICK #%d] Output channel full, dropping partial"), jen.Id("currentTick")),
							),
						),

						jen.Return(jen.Nil()),
					),
				),
				jen.Id("err").Op("!=").Nil(),
			).Block(
				jen.Qual("log", "Printf").Call(jen.Lit("[TICK] Panic recovered: %v"), jen.Id("err")),
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
		streamCallParams = append(streamCallParams,
			jen.Append(
				jen.Index().Qual(common.GeneratedClientPkg, "CallOptionFunc").Values(
					jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
				),
				jen.Id("options").Op("..."),
			).Op("..."),
		)

		// Build the stream goroutine - calls Stream, drains it, waits for ticks, emits final
		streamDrainBody := []jen.Code{
			jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Starting Stream call...")),

			// Call the Stream method with WithOnTick
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(streamCallParams...),

			// If initial stream creation failed, shutdown and emit error
			jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
				jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Stream creation failed: %v"), jen.Id("streamErr")),
				jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("streamErr"))).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
				),
				jen.Return(jen.Nil()),
			),

			jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Stream created successfully, draining...")),

			// Drain the stream channel - ignore partials (we get ours from onTick)
			jen.Var().Id("result").Any(),
			jen.Var().Id("streamErr2").Error(),
			jen.Id("streamCount").Op(":=").Lit(0),
			jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
				jen.Id("streamCount").Op("++"),
				jen.If(jen.Id("streamVal").Dot("IsError")).Block(
					jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Got error at item #%d: %v"), jen.Id("streamCount"), jen.Id("streamVal").Dot("Error")),
					jen.Id("streamErr2").Op("=").Id("streamVal").Dot("Error"),
					jen.Continue(),
				),
				jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
					jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Got final result at item #%d"), jen.Id("streamCount")),
					jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
				).Else().Block(
					jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Got partial at item #%d (ignoring, we use onTick)"), jen.Id("streamCount")),
				),
			),

			jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Stream drained, received %d items total"), jen.Id("streamCount")),

			// Perform shutdown - waits for all in-flight ticks
			jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Initiating shutdown...")),
			jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
			jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Shutdown complete")),

			// Check if there was a fatal error
			jen.Id("fatalMu").Dot("Lock").Call(),
			jen.Id("fatalErrCopy").Op(":=").Id("fatalErr"),
			jen.Id("fatalMu").Dot("Unlock").Call(),
			jen.If(jen.Id("fatalErrCopy").Op("!=").Nil()).Block(
				jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Fatal error detected: %v"), jen.Id("fatalErrCopy")),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("fatalErrCopy"))).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
				),
				jen.Return(jen.Nil()),
			),

			// Emit stream error if any
			jen.If(jen.Id("streamErr2").Op("!=").Nil()).Block(
				jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Emitting stream error: %v"), jen.Id("streamErr2")),
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
			jen.Qual("log", "Printf").Call(jen.Lit("[STREAM] Final raw response: %d bytes"), jen.Len(jen.Id("finalRaw"))),

			// Send final result
			jen.Var().Id("finalParsed").Any(),
			jen.If(jen.Id("result").Op("!=").Nil()).Block(
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

			jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Sending final result to output channel")),
			jen.Select().Block(
				jen.Case(jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
					jen.Id("kind"):   jen.Qual(common.InterfacesPkg, "StreamResultKindFinal"),
					jen.Id("raw"):    jen.Id("finalRaw"),
					jen.Id("parsed"): jen.Id("finalParsed"),
				})).Block(
					jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Final result sent successfully")),
				),
				jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
					jen.Qual("log", "Println").Call(jen.Lit("[STREAM] Adapter cancelled before final send")),
				),
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

		// Start the stream goroutine (drains stream, waits for ticks, emits final)
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(
				jen.Defer().Func().Params().Block(
					jen.Qual("log", "Println").Call(jen.Lit("[GOROUTINE] Closing output channel")),
					jen.Close(jen.Id("out")),
				).Call(),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					// Error handler function - performs shutdown then emits error
					jen.Func().Params(jen.Id("err").Error()).Block(
						jen.Qual("log", "Printf").Call(jen.Lit("[GOROUTINE] Panic in stream goroutine: %v"), jen.Id("err")),
						jen.Id("shutdownOnce").Dot("Do").Call(jen.Id("doShutdown")),
						jen.Select().Block(
							jen.Case(jen.Id("out").Op("<-").Id(errorConstructorName).Call(jen.Id("err"))).Block(),
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
						),
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
