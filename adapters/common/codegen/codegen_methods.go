package codegen

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/stoewer/go-strcase"
)

// structMediaParam tracks a sync-method input parameter whose type
// is a struct (or struct wrapper) containing nested media fields.
// Each entry pairs the BAML param name with the generated mirror
// struct + conversion-function names so the per-method preamble can
// emit the right call site.
type structMediaParam struct {
	paramName   string
	mirrorName  string
	convertFunc string
	paramType   reflect.Type // original param type (may be ptr/slice wrapped)
}

// methodEmitter is the per-method sub-context for the sync-method
// emit loop. It carries the names, reflected types, and derived
// identifiers that the legacy-stream / BuildRequest / router emit
// methods all read. Built once per method via newMethodEmitter and
// then handed off to the per-concern emit methods (defined in
// sibling files); the back-reference to *generator gives access to
// the file-level state (out, opts, mirrors, supportsWithClient,
// emittedUnwrapHelpers).
type methodEmitter struct {
	g *generator

	methodName        string
	args              []string
	syncFuncType      reflect.Type
	methodMediaParams map[string]bamlutils.MediaKind

	structMediaParams   []structMediaParam
	structMediaParamSet map[string]bool

	inputStructName  string
	outputStructName string

	finalType      parsedReflectType
	finalTypePtr   *jen.Statement
	isDynamicFinal bool

	streamType      parsedReflectType
	streamTypePtr   *jen.Statement
	isDynamicStream bool

	finalResultType jen.Code

	poolVarName             string
	getterFuncName          string
	errorConstructorName    string
	metadataConstructorName string
	unwrapStreamFuncName    string
	unwrapFinalFuncName     string

	noRawMethodName            string
	fullMethodName             string
	buildRequestMethodName     string
	buildCallRequestMethodName string
}

// newMethodEmitter validates the method's reflect signature and (on
// success) returns a methodEmitter with the per-method derived names
// and types populated. The boolean return is false when this method
// should be skipped entirely (no ParseStream counterpart, missing
// SyncFuncs entry, non-function value, or non-context first param);
// callers continue past it without emitting anything.
func (g *generator) newMethodEmitter(methodName string, args []string) (*methodEmitter, bool) {
	if _, hasParseStream := introspected.ParseStreamMethods[methodName]; !hasParseStream {
		return nil, false
	}

	syncFuncValue, ok := introspected.SyncFuncs[methodName]
	if !ok {
		return nil, false
	}

	syncFuncType := reflect.TypeOf(syncFuncValue)
	if syncFuncType.Kind() != reflect.Func {
		return nil, false
	}
	if syncFuncType.NumIn() < 1 {
		return nil, false
	}
	if syncFuncType.In(0).String() != "context.Context" {
		return nil, false
	}

	me := &methodEmitter{
		g:                 g,
		methodName:        methodName,
		args:              args,
		syncFuncType:      syncFuncType,
		methodMediaParams: introspected.MediaParams[methodName],
	}
	me.inputStructName = strcase.UpperCamelCase(fmt.Sprintf("%sInput", methodName))
	me.outputStructName = strcase.UpperCamelCase(fmt.Sprintf("%sOutput", methodName))
	me.poolVarName = strcase.LowerCamelCase(fmt.Sprintf("%sPool", me.outputStructName))
	me.getterFuncName = strcase.LowerCamelCase(fmt.Sprintf("get%s", me.outputStructName))
	me.errorConstructorName = strcase.LowerCamelCase(fmt.Sprintf("new%sError", me.outputStructName))
	me.metadataConstructorName = strcase.LowerCamelCase(fmt.Sprintf("new%sMetadata", me.outputStructName))
	me.unwrapStreamFuncName = strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sStream", me.outputStructName))
	me.unwrapFinalFuncName = strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sFinal", me.outputStructName))
	me.noRawMethodName = strcase.LowerCamelCase(methodName + "_noRaw")
	me.fullMethodName = strcase.LowerCamelCase(methodName + "_full")
	me.buildRequestMethodName = strcase.LowerCamelCase(methodName + "_buildRequest")
	me.buildCallRequestMethodName = strcase.LowerCamelCase(methodName + "_buildCallRequest")
	return me, true
}

// methodOut returns the summary the post-loop Methods map uses to
// project this method's input / output / stream-output struct names.
func (me *methodEmitter) methodOut() methodOut {
	return methodOut{
		name:                   me.methodName,
		inputStructName:        me.inputStructName,
		outputStructQual:       me.finalResultType,
		streamOutputStructQual: me.streamType.statement,
	}
}

// emitInputAndOutputStructs emits the per-method input struct,
// output struct, the StreamResult interface methods, dynamic-property
// unwrap helpers, the output-struct sync.Pool wrapper, and the error
// / metadata constructors. The post-call methodEmitter has every
// per-method derived field populated and is ready for the legacy
// stream / BuildRequest / router emit methods to run against.
func (me *methodEmitter) emitInputAndOutputStructs() {
	g := me.g
	out := g.out

	// Generate the input struct
	var structFields []jen.Code

	// Parameters start at index 1 (after context) and end before the variadic opts
	for paramIdx := 1; paramIdx < me.syncFuncType.NumIn()-1; paramIdx++ {
		argIdx := paramIdx - 1
		if argIdx >= len(me.args) {
			break
		}
		paramName := me.args[argIdx]
		paramType := me.syncFuncType.In(paramIdx)

		var fieldType *jen.Statement
		if _, isMedia := me.methodMediaParams[paramName]; isMedia {
			// Direct media-typed param: use MediaInput with the same pointer/slice wrapping
			fieldType = jen.Id(strcase.UpperCamelCase(paramName)).Add(mediaFieldType(paramType))
		} else if structContainsMedia(paramType) {
			// Struct param containing nested media: use mirror struct
			mirrorName := g.mirrors.ensureMirrorStruct(out, paramType)
			fieldType = jen.Id(strcase.UpperCamelCase(paramName)).Add(mirrorFieldType(paramType, mirrorName))

			// Unwrap to get the inner type for the convert function name
			inner := paramType
			for inner.Kind() == reflect.Ptr || inner.Kind() == reflect.Slice {
				inner = inner.Elem()
			}
			me.structMediaParams = append(me.structMediaParams, structMediaParam{
				paramName:   paramName,
				mirrorName:  mirrorName,
				convertFunc: "convert" + mirrorName,
				paramType:   paramType,
			})
		} else {
			fieldType = jen.Id(strcase.UpperCamelCase(paramName)).Add(parseReflectType(paramType).statement)
		}

		structFields = append(structFields,
			fieldType.
				Tag(map[string]string{
					"json": paramName,
				}))
	}

	out.Type().Id(me.inputStructName).Struct(structFields...)

	// Get the return type (first return value, second is error)
	if me.syncFuncType.NumOut() >= 1 {
		me.finalResultType = parseReflectType(me.syncFuncType.Out(0)).statement
	} else {
		me.finalResultType = jen.Any()
	}

	// Get the ParseStream function for stream type reflection
	parseStreamFuncValue, hasParseStreamFunc := introspected.ParseStreamFuncs[me.methodName]

	// Final type (from sync function return)
	me.finalType = parseReflectType(me.syncFuncType.Out(0))
	me.finalTypePtr = jen.Op("*").Add(me.finalType.statement.Clone())
	me.isDynamicFinal = hasDynamicPropertiesForType(me.syncFuncType.Out(0))

	// Stream type (from ParseStream return) - may be different from final type
	if hasParseStreamFunc {
		parseStreamFuncType := reflect.TypeOf(parseStreamFuncValue)
		if parseStreamFuncType.Kind() == reflect.Func && parseStreamFuncType.NumOut() >= 1 {
			me.streamType = parseReflectType(parseStreamFuncType.Out(0))
			me.streamTypePtr = jen.Op("*").Add(me.streamType.statement.Clone())
			me.isDynamicStream = hasDynamicPropertiesForType(parseStreamFuncType.Out(0))
		}
	}
	// Fallback to final type if we couldn't get stream type
	if me.streamTypePtr == nil {
		me.streamType = me.finalType
		me.streamTypePtr = me.finalTypePtr
		me.isDynamicStream = me.isDynamicFinal
	}

	// Output struct holds: kind, raw LLM response, typed parsed values, error,
	// and optional routing metadata.
	// streamParsed and finalParsed are typed pointer fields that eliminate the
	// interface boxing and runtime type assertions of a single `parsed any` field.
	// metadata is populated only when kind==StreamResultKindMetadata.
	out.Type().Id(me.outputStructName).Struct(
		jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
		jen.Id("raw").String(),
		jen.Id("reasoning").String(),
		jen.Id("streamParsed").Add(me.streamTypePtr.Clone()),
		jen.Id("finalParsed").Add(me.finalTypePtr.Clone()),
		jen.Id("err").Error(),
		jen.Id("reset").Bool(), // true when client should discard accumulated state (retry occurred)
		jen.Id("metadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
	)

	// Implement `StreamResult` interface for the output struct
	selfName := jen.Id("v")
	selfParam := selfName.Clone().Op("*").Id(me.outputStructName)

	// Kind() method
	out.Func().
		Params(selfParam.Clone()).
		Id("Kind").Params().
		Qual(common.InterfacesPkg, "StreamResultKind").
		Block(
			jen.Return(selfName.Clone().Dot("kind")),
		)

	// Generate unwrap helpers for dynamic types (called at setter time, not getter time)
	if me.isDynamicStream {
		g.emitDynamicUnwrapFunc(me.unwrapStreamFuncName, me.streamTypePtr)
	}
	if me.isDynamicFinal {
		g.emitDynamicUnwrapFunc(me.unwrapFinalFuncName, me.finalTypePtr)
		g.emittedUnwrapHelpers[me.unwrapFinalFuncName] = true
	}

	// Stream() method - returns typed streamParsed field directly (no type assertion)
	out.Func().
		Params(selfParam.Clone()).
		Id("Stream").Params().
		Any().
		Block(
			jen.Return(selfName.Clone().Dot("streamParsed")),
		)

	// Final() method - returns typed finalParsed field directly (no type assertion)
	out.Func().
		Params(selfParam.Clone()).
		Id("Final").Params().
		Any().
		Block(
			jen.Return(selfName.Clone().Dot("finalParsed")),
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

	// Reasoning() method - returns the structured reasoning channel
	out.Func().
		Params(selfParam.Clone()).
		Id("Reasoning").Params().
		String().
		Block(
			jen.Return(selfName.Clone().Dot("reasoning")),
		)

	// Reset() method - returns true if client should discard accumulated state
	out.Func().
		Params(selfParam.Clone()).
		Id("Reset").Params().
		Bool().
		Block(
			jen.Return(selfName.Clone().Dot("reset")),
		)

	// Metadata() method - returns the routing/retry metadata payload.
	// Non-nil only when kind==StreamResultKindMetadata.
	out.Func().
		Params(selfParam.Clone()).
		Id("Metadata").Params().
		Op("*").Qual(common.InterfacesPkg, "Metadata").
		Block(
			jen.Return(selfName.Clone().Dot("metadata")),
		)

	// Generate pool for output struct reuse
	out.Var().Id(me.poolVarName).Op("=").Qual(common.InterfacesPkg, "NewPool").Call(
		jen.Func().Params().Op("*").Id(me.outputStructName).Block(
			jen.Return(jen.Op("&").Id(me.outputStructName).Values()),
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
			jen.Op("*").Add(selfName.Clone()).Op("=").Id(me.outputStructName).Values(),
			jen.Id(me.poolVarName).Dot("Put").Call(selfName.Clone()),
		)

	// Generate getter function for output struct
	out.Func().
		Id(me.getterFuncName).
		Params().
		Op("*").Id(me.outputStructName).
		Block(
			jen.Return(jen.Id(me.poolVarName).Dot("Get").Call()),
		)

	// Generate error constructor function
	out.Func().
		Id(me.errorConstructorName).
		Params(jen.Id("err").Error()).
		Op("*").Id(me.outputStructName).
		Block(
			jen.Id("r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindError"),
			jen.Id("r").Dot("err").Op("=").Id("err"),
			jen.Return(jen.Id("r")),
		)

	// Generate metadata constructor function. Produces a StreamResult whose
	// Kind()==StreamResultKindMetadata and whose Metadata() returns the
	// supplied payload. Uses the same pool as the regular result path.
	out.Func().
		Id(me.metadataConstructorName).
		Params(jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata")).
		Op("*").Id(me.outputStructName).
		Block(
			jen.Id("r").Op(":=").Id(me.getterFuncName).Call(),
			jen.Id("r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindMetadata"),
			jen.Id("r").Dot("metadata").Op("=").Id("md"),
			jen.Return(jen.Id("r")),
		)

	// Build the struct-media-param lookup set used by argCallParam to
	// map an arg name to the correct emit-time variable name.
	me.structMediaParamSet = make(map[string]bool, len(me.structMediaParams))
	for _, smp := range me.structMediaParams {
		me.structMediaParamSet[smp.paramName] = true
	}
}

// argCallParam returns the Jen expression for the named arg in a
// call-site parameter list (Stream.<Method>, ParseStream.<Method>,
// Request.<Method>, etc.). Direct-media params resolve to a
// `__media_<name>` local; nested-media-struct params resolve to a
// `__struct_<name>` local; everything else is a field access on the
// `input` struct under its UpperCamelCase name.
func (me *methodEmitter) argCallParam(arg string) jen.Code {
	if _, isMedia := me.methodMediaParams[arg]; isMedia {
		return jen.Id("__media_" + arg)
	}
	if me.structMediaParamSet[arg] {
		return jen.Id("__struct_" + arg)
	}
	return jen.Id("input").Dot(strcase.UpperCamelCase(arg))
}

// makePreambleWithArgs builds the shared body prefix for every
// generated dispatch site: resolve the options helper, type-assert
// rawInput into the per-method input struct, and convert any media-
// or media-struct-bearing params into local variables. extraCallArgs
// is appended to the helper-call argument list after the always-
// present `adapter`. optionsHelperName names the registry-options
// helper to invoke. The two convenience wrappers makePreamble /
// makeLegacyPreamble pin the helper name + extras at each dispatch
// site.
func (me *methodEmitter) makePreambleWithArgs(optionsHelperName string, extraCallArgs ...jen.Code) []jen.Code {
	callArgs := append([]jen.Code{jen.Id("adapter")}, extraCallArgs...)
	preamble := []jen.Code{
		jen.List(jen.Id("options"), jen.Id("err")).Op(":=").Id(optionsHelperName).Call(callArgs...),
		jen.If(jen.Id("err").Op("!=").Nil()).Block(
			jen.Return(jen.Id("err")),
		),
	}
	if len(me.args) > 0 {
		preamble = append(preamble,
			jen.List(jen.Id("input"), jen.Id("ok")).Op(":=").Id("rawInput").Assert(jen.Op("*").Id(me.inputStructName)),
			jen.If(jen.Op("!").Id("ok")).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(
					jen.Lit("invalid input type: expected *%s, got %T"),
					jen.Lit(me.inputStructName),
					jen.Id("rawInput"),
				)),
			),
		)

		// Generate media conversion code for each direct media-typed param
		for paramIdx := 1; paramIdx < me.syncFuncType.NumIn()-1; paramIdx++ {
			argIdx := paramIdx - 1
			if argIdx >= len(me.args) {
				break
			}
			paramName := me.args[argIdx]
			mediaKind, isMedia := me.methodMediaParams[paramName]
			if !isMedia {
				continue
			}

			fieldName := strcase.UpperCamelCase(paramName)
			convertedVar := "__media_" + paramName
			paramType := me.syncFuncType.In(paramIdx)

			preamble = append(preamble, mediaConversionCode(convertedVar, fieldName, paramType, mediaKind)...)
		}

		// Generate struct conversion code for params with nested media.
		// Must handle direct, pointer, and slice wrapping of the struct param.
		for _, smp := range me.structMediaParams {
			fieldName := strcase.UpperCamelCase(smp.paramName)
			convertedVar := "__struct_" + smp.paramName
			errVar := "__err_" + convertedVar

			isPtr := smp.paramType.Kind() == reflect.Ptr
			isSlice := smp.paramType.Kind() == reflect.Slice

			if isSlice {
				// []ClassWithMedia -> iterate and convert each element
				elemType := smp.paramType.Elem()
				elemIsPtr := elemType.Kind() == reflect.Ptr

				// Determine how to pass the element to the conversion function
				var vArg jen.Code
				if elemIsPtr {
					vArg = jen.Id("__v") // already *Mirror, pass directly
				} else {
					vArg = jen.Op("&").Id("__v") // Mirror, take address
				}

				convBlock := []jen.Code{
					jen.List(jen.Id("__converted"), jen.Id(errVar)).Op(":=").Id(smp.convertFunc).Call(
						jen.Id("adapter"),
						vArg,
					),
					jen.If(jen.Id(errVar).Op("!=").Nil()).Block(
						jen.Return(jen.Qual("fmt", "Errorf").Call(
							jen.Lit(smp.paramName+"[%d]: %w"),
							jen.Id("__i"),
							jen.Id(errVar),
						)),
					),
					func() jen.Code {
						if elemIsPtr {
							return jen.Id(convertedVar).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted")
						}
						return jen.Id(convertedVar).Index(jen.Id("__i")).Op("=").Id("__converted")
					}(),
				}

				// For pointer elements, wrap in a nil guard to preserve null entries
				var loopBody []jen.Code
				if elemIsPtr {
					loopBody = []jen.Code{
						jen.If(jen.Id("__v").Op("!=").Nil()).Block(convBlock...),
					}
				} else {
					loopBody = convBlock
				}

				preamble = append(preamble,
					jen.Id(convertedVar).Op(":=").Make(
						jen.Add(parseReflectType(smp.paramType).statement),
						jen.Len(jen.Id("input").Dot(fieldName)),
					),
					jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Id("input").Dot(fieldName)).Block(loopBody...),
				)
			} else if isPtr {
				innerAfterPtr := smp.paramType.Elem()
				if innerAfterPtr.Kind() == reflect.Slice {
					// *[]ClassWithMedia -> nil check, iterate slice, convert each element
					elemType := innerAfterPtr.Elem()
					elemIsPtr := elemType.Kind() == reflect.Ptr

					var vArg jen.Code
					if elemIsPtr {
						vArg = jen.Id("__v")
					} else {
						vArg = jen.Op("&").Id("__v")
					}

					innerConvBlock := []jen.Code{
						jen.List(jen.Id("__converted"), jen.Id(errVar)).Op(":=").Id(smp.convertFunc).Call(
							jen.Id("adapter"),
							vArg,
						),
						jen.If(jen.Id(errVar).Op("!=").Nil()).Block(
							jen.Return(jen.Qual("fmt", "Errorf").Call(
								jen.Lit(smp.paramName+"[%d]: %w"),
								jen.Id("__i"),
								jen.Id(errVar),
							)),
						),
						func() jen.Code {
							if elemIsPtr {
								return jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Op("&").Id("__converted")
							}
							return jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Id("__converted")
						}(),
					}

					var innerLoopBody []jen.Code
					if elemIsPtr {
						innerLoopBody = []jen.Code{
							jen.If(jen.Id("__v").Op("!=").Nil()).Block(innerConvBlock...),
						}
					} else {
						innerLoopBody = innerConvBlock
					}

					preamble = append(preamble,
						jen.Var().Id(convertedVar).Add(parseReflectType(smp.paramType).statement),
						jen.If(jen.Id("input").Dot(fieldName).Op("!=").Nil()).Block(
							jen.Id("__ptrSlice").Op(":=").Make(
								jen.Add(parseReflectType(innerAfterPtr).statement),
								jen.Len(jen.Op("*").Id("input").Dot(fieldName)),
							),
							jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Op("*").Id("input").Dot(fieldName)).Block(innerLoopBody...),
							jen.Id(convertedVar).Op("=").Op("&").Id("__ptrSlice"),
						),
					)
				} else {
					// *ClassWithMedia -> nil check, then convert the dereferenced value
					preamble = append(preamble,
						jen.Var().Id(convertedVar).Add(parseReflectType(smp.paramType).statement),
						jen.If(jen.Id("input").Dot(fieldName).Op("!=").Nil()).Block(
							jen.List(jen.Id("__converted"), jen.Id(errVar)).Op(":=").Id(smp.convertFunc).Call(
								jen.Id("adapter"),
								jen.Id("input").Dot(fieldName), // already *Mirror, pass directly
							),
							jen.If(jen.Id(errVar).Op("!=").Nil()).Block(
								jen.Return(jen.Qual("fmt", "Errorf").Call(
									jen.Lit(smp.paramName+": %w"),
									jen.Id(errVar),
								)),
							),
							jen.Id(convertedVar).Op("=").Op("&").Id("__converted"),
						),
					)
				}
			} else {
				// Direct: ClassWithMedia -> take address and convert
				preamble = append(preamble,
					jen.List(jen.Id(convertedVar), jen.Id(errVar)).Op(":=").Id(smp.convertFunc).Call(
						jen.Id("adapter"),
						jen.Op("&").Id("input").Dot(fieldName),
					),
					jen.If(jen.Id(errVar).Op("!=").Nil()).Block(
						jen.Return(jen.Qual("fmt", "Errorf").Call(
							jen.Lit(smp.paramName+": %w"),
							jen.Id(errVar),
						)),
					),
				)
			}
		}
	}
	return preamble
}

// makePreamble pins the BuildRequest-safe registry view for the
// BuildRequest landing sites and the call-mode legacy path.
func (me *methodEmitter) makePreamble() []jen.Code {
	return me.makePreambleWithArgs("makeOptionsFromAdapter")
}

// makeLegacyPreamble pins the legacy-stream registry view for the
// top-level legacy fallthrough impls (_noRaw / _full). The
// clientOverride is threaded into makeLegacyStreamOptionsFromAdapter
// so the registry's primary pin reaches BAML's streaming path
// (WithClient is silently dropped on Stream.<Method>).
func (me *methodEmitter) makeLegacyPreamble() []jen.Code {
	return me.makePreambleWithArgs("makeLegacyStreamOptionsFromAdapter", jen.Id("clientOverride"))
}

// emitDynamicUnwrapFunc generates an in-place dynamic properties unwrap function
// for a pointer type. The generated function mutates val.DynamicProperties once
// so getters need no work. Used by both the streaming-method loop and the
// parse-only method loop to avoid duplicating the Jen AST.
func (g *generator) emitDynamicUnwrapFunc(funcName string, typePtr *jen.Statement) {
	g.out.Func().Id(funcName).
		Params(jen.Id("val").Add(typePtr.Clone())).
		Block(
			jen.If(jen.Id("val").Op("==").Nil()).Block(jen.Return()),
			jen.If(jen.Id("val").Dot("DynamicProperties").Op("==").Nil()).Block(jen.Return()),
			jen.For(jen.List(jen.Id("key"), jen.Id("value")).Op(":=").Range().Id("val").Dot("DynamicProperties")).
				Block(
					jen.If(
						jen.List(jen.Id("reflectValue"), jen.Id("ok")).Op(":=").Id("value").Assert(jen.Qual("reflect", "Value")),
						jen.Id("ok"),
					).Block(
						jen.Id("val").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(g.selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("reflectValue").Dot("Interface").Call()),
					).Else().Block(
						jen.Id("val").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(g.selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("value")),
					),
				),
		)
}

// emitMethods walks introspected.SyncMethods and emits, for every
// method that has a ParseStream counterpart and a usable reflect
// signature, the input/output structs, the legacy _noRaw / _full
// streaming impls, the BuildRequest / BuildCallRequest impls
// (gated on the corresponding introspected singletons), and the
// public router. Returns the methodOut summaries used to build the
// Methods map.
func (g *generator) emitMethods() []methodOut {
	var methods []methodOut

	// Sort method names so emitted declaration order is deterministic
	// across runs. Go's `range` over a map randomises iteration order,
	// which propagated through the emitted Methods map (and the
	// generated input/output struct + impl declarations whose names
	// derive from methodName), making two consecutive generator runs
	// produce ASTs whose top-level declarations sit in arbitrary
	// positions. The framework adapter emitter's CI determinism check
	// caught a similar drift; this is the same fix for the streaming
	// router half.
	methodNames := make([]string, 0, len(introspected.SyncMethods))
	for k := range introspected.SyncMethods {
		methodNames = append(methodNames, k)
	}
	slices.Sort(methodNames)

	for _, methodName := range methodNames {
		args := introspected.SyncMethods[methodName]
		me, ok := g.newMethodEmitter(methodName, args)
		if !ok {
			continue
		}
		me.emitInputAndOutputStructs()
		me.emitLegacyStream()
		me.emitBuildRequest()
		me.emitBuildCallRequest()
		me.emitRouter()
		methods = append(methods, me.methodOut())
	}

	return methods
}

// emitMethodsMap emits the package-level Methods variable mapping
// every emitted streaming method's name to its StreamingMethod
// implementation triple (MakeInput / MakeOutput / MakeStreamOutput
// pool factories plus the router function value).
func (g *generator) emitMethodsMap(methods []methodOut) {
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
			jen.Id("MakeStreamOutput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(method.streamOutputStructQual)),
				),
			jen.Id("Impl"): jen.Id(method.name),
		})
	}

	g.out.Var().Id("Methods").Op("=").
		Map(jen.String()).Add(streamingFunctionInterface).
		Values(mapElements)
}

// parseMethodOut summarises a parse-only method's emitted output
// type so the post-loop ParseMethods map can reference it without
// re-deriving from reflect.Type.
type parseMethodOut struct {
	name             string
	outputStructQual jen.Code
}

// emitParseMethods walks introspected.ParseMethods, emits the
// parse_<Method> wrapper for each method whose SyncFuncs entry has
// a usable reflect signature, and returns the per-method
// parseMethodOut summaries used to build the ParseMethods map.
func (g *generator) emitParseMethods() []parseMethodOut {
	var parseMethods []parseMethodOut

	// Same map-iteration determinism fix as emitMethods: sort the
	// parse-method names so emitted parse_<Method> functions and the
	// resulting ParseMethods map appear in a stable order across
	// runs.
	parseMethodNames := make([]string, 0, len(introspected.ParseMethods))
	for k := range introspected.ParseMethods {
		parseMethodNames = append(parseMethodNames, k)
	}
	slices.Sort(parseMethodNames)

	for _, methodName := range parseMethodNames {
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

		// Check if return type has DynamicProperties that need unwrapping
		isDynamic := hasDynamicPropertiesForType(syncFuncType.Out(0))

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
		}

		// Unwrap DynamicProperties at parse time.
		// For streaming methods the helper was already emitted by the streaming loop.
		// For parse-only methods we must emit it here.
		if isDynamic {
			parseUnwrapName := strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sFinal", strcase.UpperCamelCase(fmt.Sprintf("%sOutput", methodName))))
			if !g.emittedUnwrapHelpers[parseUnwrapName] {
				finalTypeForParse := parseReflectType(syncFuncType.Out(0))
				finalTypePtrForParse := jen.Op("*").Add(finalTypeForParse.statement.Clone())
				g.emitDynamicUnwrapFunc(parseUnwrapName, finalTypePtrForParse)
				g.emittedUnwrapHelpers[parseUnwrapName] = true
			}
			parseBody = append(parseBody,
				jen.Id(parseUnwrapName).Call(jen.Op("&").Id("result")),
			)
		}

		parseBody = append(parseBody, jen.Return(jen.Id("result"), jen.Nil()))

		g.out.Func().
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

	return parseMethods
}

// emitParseMethodsMap emits the package-level ParseMethods variable
// mapping every emitted parse method's name to its ParseMethod
// implementation pair (MakeOutput pool factory + the parse_<Method>
// function value).
func (g *generator) emitParseMethodsMap(parseMethods []parseMethodOut) {
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

	g.out.Var().Id("ParseMethods").Op("=").
		Map(jen.String()).Add(parseMethodInterface).
		Values(parseMapElements)
}

// emitFactories emits the createTypeBuilder / MakeAdapter /
// createMedia trio. createTypeBuilder applies the per-request
// TypeBuilder config (DynamicTypes + BamlSnippets); MakeAdapter
// constructs a fresh BamlAdapter with the codegen-emitted factories
// wired in; createMedia dispatches to baml_client's NewImage /
// NewAudio / NewPDF / NewVideo constructors based on MediaKind.
func (g *generator) emitFactories() {
	out := g.out

	// Generate `createTypeBuilder` - creates TypeBuilder and applies config
	out.Func().Id("createTypeBuilder").
		Params(
			jen.Id("config").Op("*").Qual(common.InterfacesPkg, "TypeBuilder"),
		).
		Params(jen.Op("*").Qual(common.IntrospectedPkg, "TypeBuilder"), jen.Error()).
		Block(
			jen.List(jen.Id("tb"), jen.Id("err")).Op(":=").Qual(common.IntrospectedPkg, "NewTypeBuilder").Call(),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
			jen.If(jen.Id("config").Op("==").Nil()).Block(
				jen.Return(jen.Id("tb"), jen.Nil()),
			),
			// Apply dynamic_types first (imperative API)
			jen.If(jen.Id("config").Dot("DynamicTypes").Op("!=").Nil()).Block(
				jen.If(jen.Id("err").Op(":=").Id("applyDynamicTypes").Call(jen.Id("tb"), jen.Id("config").Dot("DynamicTypes")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to apply dynamic types: %w"), jen.Id("err"))),
				),
			),
			// Then add BAML snippets (can reference types created above)
			jen.For(jen.List(jen.Id("idx"), jen.Id("input")).Op(":=").Range().Id("config").Dot("BamlSnippets")).Block(
				jen.If(jen.Id("err").Op(":=").Id("tb").Dot("AddBaml").Call(jen.Id("input")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("baml_snippets[%d]: %w"), jen.Id("idx"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Id("tb"), jen.Nil()),
		)

	// Generate `MakeAdapter`
	//
	// IntrospectedClientProvider plumbs the build-time client→provider
	// map into the adapter so SetClientRegistry can materialise
	// providers for omitted-provider runtime registry entries
	// (strategy-only / presence-only RR overrides). Without this seam
	// the adapter would forward `provider: ""` into BAML's CFFI, which
	// rejects in ClientProvider::from_str (clientspec.rs:119-144) and
	// kills the request before WithClient(leaf) resolves anything.
	out.Func().Id("MakeAdapter").
		Params(jen.Id("ctx").Qual("context", "Context")).
		Qual(common.InterfacesPkg, "Adapter").
		Block(
			jen.Return(
				jen.Op("&").Qual(g.selfAdapterPkg, "BamlAdapter").
					Values(jen.Dict{
						jen.Id("Context"): jen.Id("ctx"),
						jen.Id("TypeBuilderFactory"): jen.Func().
							Params(
								jen.Id("config").Op("*").Qual(common.InterfacesPkg, "TypeBuilder"),
							).
							Params(jen.Op("*").Qual(common.IntrospectedPkg, "TypeBuilder"), jen.Error()).
							Block(
								jen.Return(jen.Id("createTypeBuilder").Call(jen.Id("config"))),
							),
						jen.Id("MediaFactory"):               jen.Id("createMedia"),
						jen.Id("IntrospectedClientProvider"): jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					}),
			),
		)

	// Generate `createMedia` - dispatches to baml_client's media constructors
	out.Func().Id("createMedia").
		Params(
			jen.Id("kind").Qual(common.InterfacesPkg, "MediaKind"),
			jen.Id("url").Op("*").String(),
			jen.Id("base64").Op("*").String(),
			jen.Id("mimeType").Op("*").String(),
		).
		Params(jen.Any(), jen.Error()).
		Block(
			jen.If(jen.Id("url").Op("!=").Nil()).Block(
				jen.Switch(jen.Id("kind")).Block(
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindImage")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewImageFromUrl").Call(jen.Op("*").Id("url"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindAudio")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewAudioFromUrl").Call(jen.Op("*").Id("url"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindPDF")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewPDFFromUrl").Call(jen.Op("*").Id("url"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindVideo")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewVideoFromUrl").Call(jen.Op("*").Id("url"), jen.Id("mimeType"))),
					),
				),
			),
			jen.If(jen.Id("base64").Op("!=").Nil()).Block(
				jen.Switch(jen.Id("kind")).Block(
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindImage")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewImageFromBase64").Call(jen.Op("*").Id("base64"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindAudio")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewAudioFromBase64").Call(jen.Op("*").Id("base64"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindPDF")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewPDFFromBase64").Call(jen.Op("*").Id("base64"), jen.Id("mimeType"))),
					),
					jen.Case(jen.Qual(common.InterfacesPkg, "MediaKindVideo")).Block(
						jen.Return(jen.Qual(common.GeneratedClientPkg, "NewVideoFromBase64").Call(jen.Op("*").Id("base64"), jen.Id("mimeType"))),
					),
				),
			),
			jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("unsupported media kind: %v"), jen.Id("kind"))),
		)
}

// emitInitBamlRuntime emits the InitBamlRuntime wrapper around
// baml_client.InitRuntime. The framework adapter calls this once at
// startup so the BAML CFFI runtime is initialised before any
// generated method dispatches.
func (g *generator) emitInitBamlRuntime() {
	g.out.Func().Id("InitBamlRuntime").
		Params().
		Block(
			jen.Qual(common.GeneratedClientPkg, "InitRuntime").Call(),
		)
}
