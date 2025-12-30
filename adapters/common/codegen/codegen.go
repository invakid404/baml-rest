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

		// Stream() method - returns the parsed partial value
		// Check if the result type has DynamicProperties for unwrapping
		isDynamic := hasDynamicPropertiesForType(syncFuncType.Out(0))

		// Get the actual result type for type assertion in makeValueGetter
		// We always store pointers, so use pointer type for assertion
		resultType := parseReflectType(syncFuncType.Out(0))
		resultTypePtr := jen.Op("*").Add(resultType.statement.Clone())

		makeValueGetter := func() []jen.Code {
			// Type-assert to pointer type since we always store pointers
			baseCode := []jen.Code{
				jen.List(jen.Id("result"), jen.Id("ok")).Op(":=").Add(selfName.Clone().Dot("parsed")).Assert(resultTypePtr.Clone()),
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

		out.Func().
			Params(selfParam.Clone()).
			Id("Stream").Params().
			Any().
			Block(
				makeValueGetter()...,
			)

		// Final() method - returns the parsed final value
		out.Func().
			Params(selfParam.Clone()).
			Id("Final").Params().
			Any().
			Block(
				makeValueGetter()...,
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
			jen.List(jen.Id("queueCtx"), jen.Id("queueCancel")).Op(":=").Qual("context", "WithCancel").Call(jen.Qual("context", "Background").Call()),
			// WaitGroup to track pending partial processing
			jen.Var().Id("wg").Qual("sync", "WaitGroup"),
			// Mutex and lastRawResponse for final raw capture
			jen.Var().Id("lastRawResponse").String(),
			jen.Var().Id("mu").Qual("sync", "Mutex"),
		)

		// Build the onTick callback - extracts raw text and enqueues for async processing
		// IMPORTANT: OnTick is called synchronously by BAML, so we must return ASAP
		onTickBody := []jen.Code{
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

			// Enqueue raw for async processing - increment WaitGroup BEFORE enqueue
			jen.Id("wg").Dot("Add").Call(jen.Lit(1)),
			jen.Id("_").Op("=").Id("rawQueue").Dot("Enqueue").Call(jen.Id("raw")),

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

		// Helper to process a single raw item from the queue
		// Always store as pointer for consistency
		parsedValueExpr := jen.Op("&").Id("parsed")

		processQueueItem := func(rawVarName string) []jen.Code {
			return []jen.Code{
				// Call ParseStream outside of OnTick callback context (avoids deadlock)
				jen.List(jen.Id("parsed"), jen.Id("parseErr")).Op(":=").
					Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
						jen.Id(rawVarName),
						jen.Id("options").Op("..."),
					),
				jen.If(jen.Id("parseErr").Op("==").Nil()).Block(
					jen.Select().Block(
						jen.Case(jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
							jen.Id("kind"):   jen.Qual(common.InterfacesPkg, "StreamResultKindStream"),
							jen.Id("raw"):    jen.Id(rawVarName),
							jen.Id("parsed"): parsedValueExpr.Clone(),
						})).Block(),
						jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(),
						jen.Default().Block(),
					),
				),
				// Signal this partial is done processing
				jen.Id("wg").Dot("Done").Call(),
			}
		}

		// Build the partials processing goroutine - reads from queue, calls ParseStream, emits partials
		partialsGoroutineBody := []jen.Code{
			// Main loop: wait for items from queue until context is cancelled
			jen.For().Block(
				jen.List(jen.Id("item"), jen.Id("err")).Op(":=").Id("rawQueue").Dot("DequeueOrWaitForNextElementContext").Call(jen.Id("queueCtx")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					// Context cancelled - drain remaining items
					jen.For().Block(
						jen.List(jen.Id("remaining"), jen.Id("drainErr")).Op(":=").Id("rawQueue").Dot("Dequeue").Call(),
						jen.If(jen.Id("drainErr").Op("!=").Nil()).Block(
							jen.Return(), // Queue empty, exit
						),
						jen.Id("raw").Op(":=").Id("remaining").Assert(jen.String()),
						jen.Block(processQueueItem("raw")...),
					),
				),
				jen.Id("raw").Op(":=").Id("item").Assert(jen.String()),
				jen.Block(processQueueItem("raw")...),
			),
		}

		// Build the stream drain goroutine - drains stream channel, waits for partials, emits final/error
		streamDrainBody := []jen.Code{
			// Call the Stream method with WithOnTick
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(streamCallParams...),

			jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
				jen.Id("queueCancel").Call(),
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

			// Signal partials goroutine to stop and drain remaining items
			jen.Id("queueCancel").Call(),

			// Wait for all pending partials to finish processing
			jen.Id("wg").Dot("Wait").Call(),

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
					jen.List(jen.Id("ptr"), jen.Id("ok")).Op(":=").Id("result").Assert(resultTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					jen.Id("finalParsed").Op("=").Id("ptr"),
				).Else().If(
					jen.List(jen.Id("val"), jen.Id("ok")).Op(":=").Id("result").Assert(resultType.statement.Clone()),
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

		// Start the partials processing goroutine (reads from rawChan, calls ParseStream, emits partials)
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(partialsGoroutineBody...).Call(),
		)

		// Start the stream drain goroutine (drains stream, cancels queue context, waits for partials, emits final)
		methodBody = append(methodBody,
			jen.Go().Func().Params().Block(
				jen.Defer().Close(jen.Id("out")),
				jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
					// Error handler function
					jen.Func().Params(jen.Id("err").Error()).Block(
						jen.Id("queueCancel").Call(), // Signal partials goroutine to stop on panic
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
