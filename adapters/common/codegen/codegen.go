package codegen

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/stoewer/go-strcase"
)

const (
	// BamlPkg is the path to the BAML Go runtime package
	BamlPkg = "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

type methodOut struct {
	name                   string
	inputStructName        string
	outputStructQual       jen.Code
	streamOutputStructQual jen.Code
}

// mediaTypeNames maps BAML media type names to their MediaKind.
// Used for reflection-based detection of media types in struct fields.
var mediaTypeNames = map[string]bamlutils.MediaKind{
	"Image": bamlutils.MediaKindImage,
	"Audio": bamlutils.MediaKindAudio,
	"PDF":   bamlutils.MediaKindPDF,
	"Video": bamlutils.MediaKindVideo,
}

// isMediaReflectType checks whether a type (after unwrapping ptr/slice) is a known
// BAML media type. Detection is by type name (Image, Audio, PDF, Video) from any
// BAML-related package (runtime or generated client).
func isMediaReflectType(typ reflect.Type) (bamlutils.MediaKind, bool) {
	// Unwrap pointer and slice layers
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	// Check if the type name matches a known media type and comes from a BAML package
	if kind, ok := mediaTypeNames[typ.Name()]; ok {
		pkgPath := typ.PkgPath()
		if strings.Contains(pkgPath, "boundaryml/baml") || strings.Contains(pkgPath, "baml_client") {
			return kind, true
		}
	}
	return 0, false
}

// structContainsMedia recursively checks whether a struct type (or any nested struct)
// contains BAML media-typed fields (Image, Audio, PDF, Video).
func structContainsMedia(typ reflect.Type) bool {
	// Unwrap pointer and slice layers
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		ft := field.Type
		// Unwrap ptr/slice for the check
		inner := ft
		for inner.Kind() == reflect.Ptr || inner.Kind() == reflect.Slice {
			inner = inner.Elem()
		}
		if _, ok := isMediaReflectType(ft); ok {
			return true
		}
		if inner.Kind() == reflect.Struct && structContainsMedia(ft) {
			return true
		}
	}
	return false
}

// mirrorStructs tracks which BAML struct types have already had a mirror input struct
// generated, to avoid duplicates when the same type appears in multiple functions.
// Maps the original BAML type name to the generated mirror struct name.
type mirrorStructTracker struct {
	generated map[reflect.Type]string // baml type -> mirror struct name
}

func newMirrorStructTracker() *mirrorStructTracker {
	return &mirrorStructTracker{generated: make(map[reflect.Type]string)}
}

// mirrorInputName returns the name for a mirror input struct.
func mirrorInputName(typ reflect.Type) string {
	return typ.Name() + "MediaInput"
}

// ensureMirrorStruct generates a mirror struct and conversion function for a BAML struct
// that contains media fields. Returns the mirror struct name. Skips generation if already done.
func (m *mirrorStructTracker) ensureMirrorStruct(out *jen.File, typ reflect.Type) string {
	// Unwrap pointer/slice to get to the struct
	inner := typ
	for inner.Kind() == reflect.Ptr || inner.Kind() == reflect.Slice {
		inner = inner.Elem()
	}

	if name, ok := m.generated[inner]; ok {
		return name
	}

	mirrorName := mirrorInputName(inner)
	m.generated[inner] = mirrorName

	// First, recursively ensure mirror structs for any nested structs with media
	for i := 0; i < inner.NumField(); i++ {
		field := inner.Field(i)
		if !field.IsExported() {
			continue
		}
		fieldInner := field.Type
		for fieldInner.Kind() == reflect.Ptr || fieldInner.Kind() == reflect.Slice {
			fieldInner = fieldInner.Elem()
		}
		if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type) {
			m.ensureMirrorStruct(out, field.Type)
		}
	}

	// Generate the mirror struct
	var fields []jen.Code
	for i := 0; i < inner.NumField(); i++ {
		field := inner.Field(i)
		if !field.IsExported() {
			continue
		}

		// Preserve the original json tag from the BAML struct
		jsonTag := field.Tag.Get("json")
		if jsonTag == "" {
			jsonTag = field.Name
		}

		var fieldCode *jen.Statement
		if _, isMedia := isMediaReflectType(field.Type); isMedia {
			// Replace media type with MediaInput, preserving ptr/slice wrapping
			fieldCode = jen.Id(field.Name).Add(mediaFieldType(field.Type))
		} else {
			fieldInner := field.Type
			for fieldInner.Kind() == reflect.Ptr || fieldInner.Kind() == reflect.Slice {
				fieldInner = fieldInner.Elem()
			}
			if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type) {
				// Nested struct with media: use its mirror type, preserving wrapping
				nestedMirrorName := mirrorInputName(fieldInner)
				fieldCode = jen.Id(field.Name).Add(mirrorFieldType(field.Type, nestedMirrorName))
			} else {
				fieldCode = jen.Id(field.Name).Add(parseReflectType(field.Type).statement)
			}
		}

		fields = append(fields, fieldCode.Tag(map[string]string{"json": jsonTag}))
	}
	out.Type().Id(mirrorName).Struct(fields...)

	// Generate the conversion function: convert<MirrorName>(adapter, input) -> (bamlType, error)
	m.generateConversionFunc(out, inner, mirrorName)

	return mirrorName
}

// generateConversionFunc generates a function that converts a mirror input struct
// to the real BAML struct, handling media field conversion.
func (m *mirrorStructTracker) generateConversionFunc(out *jen.File, bamlType reflect.Type, mirrorName string) {
	funcName := "convert" + mirrorName
	bamlTypeExpr := parseReflectType(bamlType).statement

	var bodyCode []jen.Code
	bodyCode = append(bodyCode, jen.Var().Id("result").Add(bamlTypeExpr.Clone()))

	for i := 0; i < bamlType.NumField(); i++ {
		field := bamlType.Field(i)
		if !field.IsExported() {
			continue
		}

		fieldName := field.Name
		srcExpr := jen.Id("input").Dot(fieldName)

		if mediaKind, isMedia := isMediaReflectType(field.Type); isMedia {
			bodyCode = append(bodyCode, mediaFieldConversion(fieldName, srcExpr, field.Type, mediaKind)...)
		} else {
			fieldInner := field.Type
			for fieldInner.Kind() == reflect.Ptr || fieldInner.Kind() == reflect.Slice {
				fieldInner = fieldInner.Elem()
			}
			if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type) {
				bodyCode = append(bodyCode, nestedStructConversion(fieldName, srcExpr, field.Type, m)...)
			} else {
				// Direct copy
				bodyCode = append(bodyCode, jen.Id("result").Dot(fieldName).Op("=").Add(srcExpr.Clone()))
			}
		}
	}

	bodyCode = append(bodyCode, jen.Return(jen.Id("result"), jen.Nil()))

	out.Func().Id(funcName).
		Params(
			jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
			jen.Id("input").Op("*").Id(mirrorName),
		).
		Params(bamlTypeExpr.Clone(), jen.Error()).
		Block(bodyCode...)
}

// mediaFieldConversion generates code to convert a MediaInput field to a BAML media type.
// Handles direct, pointer, and slice wrapping.
func mediaFieldConversion(fieldName string, srcExpr *jen.Statement, fieldType reflect.Type, mediaKind bamlutils.MediaKind) []jen.Code {
	kindExpr := jen.Qual(common.InterfacesPkg, mediaKind.ConstName())

	isPtr := fieldType.Kind() == reflect.Ptr
	isSlice := fieldType.Kind() == reflect.Slice

	innerType := fieldType
	for innerType.Kind() == reflect.Ptr || innerType.Kind() == reflect.Slice {
		innerType = innerType.Elem()
	}
	bamlType := parseReflectType(innerType).statement

	if isSlice {
		elemType := fieldType.Elem()
		elemIsPtr := elemType.Kind() == reflect.Ptr

		var assertExpr *jen.Statement
		if elemIsPtr {
			assertExpr = parseReflectType(elemType).statement
		} else {
			assertExpr = bamlType.Clone()
		}

		// When elem is a pointer, the range variable is already *MediaInput;
		// pass it directly instead of taking &__mi (which would be **MediaInput).
		var miArg jen.Code
		if elemIsPtr {
			miArg = jen.Id("__mi")
		} else {
			miArg = jen.Op("&").Id("__mi")
		}

		var assignStmts []jen.Code
		if elemIsPtr {
			assignStmts = []jen.Code{
				jen.Id("__converted").Op(":=").Id("__raw").Assert(bamlType.Clone()),
				jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted"),
			}
		} else {
			assignStmts = []jen.Code{
				jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Id("__raw").Assert(assertExpr),
			}
		}

		conversionBlock := append([]jen.Code{
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
				jen.Id("adapter"),
				kindExpr.Clone(),
				miArg,
			),
			jen.If(jen.Id("__err").Op("!=").Nil()).Block(
				jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
					jen.Lit(fieldName+"[%d]: %w"),
					jen.Id("__i"),
					jen.Id("__err"),
				)),
			),
		}, assignStmts...)

		var loopBody []jen.Code
		if elemIsPtr {
			loopBody = []jen.Code{
				jen.If(jen.Id("__mi").Op("!=").Nil()).Block(conversionBlock...),
			}
		} else {
			loopBody = conversionBlock
		}

		return []jen.Code{
			jen.Id("result").Dot(fieldName).Op("=").Make(
				jen.Add(parseReflectType(fieldType).statement),
				jen.Len(srcExpr.Clone()),
			),
			jen.For(jen.List(jen.Id("__i"), jen.Id("__mi")).Op(":=").Range().Add(srcExpr.Clone())).Block(loopBody...),
		}
	}

	if isPtr {
		innerAfterPtr := fieldType.Elem()
		if innerAfterPtr.Kind() == reflect.Slice {
			// *[]Image (optional list, e.g., image[]? inside a class) — unwrap pointer, then iterate
			elemType := innerAfterPtr.Elem()
			elemIsPtr := elemType.Kind() == reflect.Ptr

			var elemAssert *jen.Statement
			if elemIsPtr {
				elemAssert = parseReflectType(elemType).statement
			} else {
				elemAssert = bamlType.Clone()
			}

			var miArg jen.Code
			if elemIsPtr {
				miArg = jen.Id("__mi")
			} else {
				miArg = jen.Op("&").Id("__mi")
			}

			var innerAssignStmts []jen.Code
			if elemIsPtr {
				innerAssignStmts = []jen.Code{
					jen.Id("__converted").Op(":=").Id("__raw").Assert(bamlType.Clone()),
					jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Op("&").Id("__converted"),
				}
			} else {
				innerAssignStmts = []jen.Code{
					jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Id("__raw").Assert(elemAssert),
				}
			}

			innerConvBlock := append([]jen.Code{
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
					jen.Id("adapter"),
					kindExpr.Clone(),
					miArg,
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+"[%d]: %w"),
						jen.Id("__i"),
						jen.Id("__err"),
					)),
				),
			}, innerAssignStmts...)

			var innerLoopBody []jen.Code
			if elemIsPtr {
				innerLoopBody = []jen.Code{
					jen.If(jen.Id("__mi").Op("!=").Nil()).Block(innerConvBlock...),
				}
			} else {
				innerLoopBody = innerConvBlock
			}

			var stmts []jen.Code
			stmts = append(stmts,
				jen.If(srcExpr.Clone().Op("!=").Nil()).Block(
					jen.Id("__ptrSlice").Op(":=").Make(
						jen.Add(parseReflectType(innerAfterPtr).statement),
						jen.Len(jen.Op("*").Add(srcExpr.Clone())),
					),
					jen.For(jen.List(jen.Id("__i"), jen.Id("__mi")).Op(":=").Range().Op("*").Add(srcExpr.Clone())).Block(innerLoopBody...),
					jen.Id("result").Dot(fieldName).Op("=").Op("&").Id("__ptrSlice"),
				),
			)
			return stmts
		}

		// *MediaInput -> *baml.Image (optional single value)
		return []jen.Code{
			jen.If(srcExpr.Clone().Op("!=").Nil()).Block(
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
					jen.Id("adapter"),
					kindExpr.Clone(),
					srcExpr.Clone(),
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+": %w"),
						jen.Id("__err"),
					)),
				),
				jen.Id("__typed").Op(":=").Id("__raw").Assert(bamlType.Clone()),
				jen.Id("result").Dot(fieldName).Op("=").Op("&").Id("__typed"),
			),
		}
	}

	// Direct
	return []jen.Code{
		jen.Block(
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
				jen.Id("adapter"),
				kindExpr,
				jen.Op("&").Add(srcExpr.Clone()),
			),
			jen.If(jen.Id("__err").Op("!=").Nil()).Block(
				jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
					jen.Lit(fieldName+": %w"),
					jen.Id("__err"),
				)),
			),
			jen.Id("result").Dot(fieldName).Op("=").Id("__raw").Assert(bamlType),
		),
	}
}

// nestedStructConversion generates code to convert a nested mirror struct field
// to the real BAML struct via its conversion function. Handles ptr/slice wrapping.
func nestedStructConversion(fieldName string, srcExpr *jen.Statement, fieldType reflect.Type, tracker *mirrorStructTracker) []jen.Code {
	isPtr := fieldType.Kind() == reflect.Ptr
	isSlice := fieldType.Kind() == reflect.Slice

	innerType := fieldType
	for innerType.Kind() == reflect.Ptr || innerType.Kind() == reflect.Slice {
		innerType = innerType.Elem()
	}
	convertFunc := "convert" + mirrorInputName(innerType)

	if isSlice {
		elemType := fieldType.Elem()
		elemIsPtr := elemType.Kind() == reflect.Ptr

		// When elem is a pointer, the range variable is already *MirrorType;
		// pass it directly instead of taking &__v (which would be **MirrorType).
		var vArg jen.Code
		if elemIsPtr {
			vArg = jen.Id("__v")
		} else {
			vArg = jen.Op("&").Id("__v")
		}

		return []jen.Code{
			jen.Id("result").Dot(fieldName).Op("=").Make(
				jen.Add(parseReflectType(fieldType).statement),
				jen.Len(srcExpr.Clone()),
			),
			jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Add(srcExpr.Clone())).Block(
				jen.List(jen.Id("__converted"), jen.Id("__err")).Op(":=").Id(convertFunc).Call(
					jen.Id("adapter"),
					vArg,
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+"[%d]: %w"),
						jen.Id("__i"),
						jen.Id("__err"),
					)),
				),
				func() jen.Code {
					if elemType.Kind() == reflect.Ptr {
						return jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted")
					}
					return jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Id("__converted")
				}(),
			),
		}
	}

	if isPtr {
		return []jen.Code{
			jen.If(srcExpr.Clone().Op("!=").Nil()).Block(
				jen.List(jen.Id("__converted"), jen.Id("__err")).Op(":=").Id(convertFunc).Call(
					jen.Id("adapter"),
					srcExpr.Clone(),
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+": %w"),
						jen.Id("__err"),
					)),
				),
				jen.Id("result").Dot(fieldName).Op("=").Op("&").Id("__converted"),
			),
		}
	}

	// Direct
	return []jen.Code{
		jen.Block(
			jen.List(jen.Id("__converted"), jen.Id("__err")).Op(":=").Id(convertFunc).Call(
				jen.Id("adapter"),
				jen.Op("&").Add(srcExpr.Clone()),
			),
			jen.If(jen.Id("__err").Op("!=").Nil()).Block(
				jen.Return(jen.Id("result"), jen.Qual("fmt", "Errorf").Call(
					jen.Lit(fieldName+": %w"),
					jen.Id("__err"),
				)),
			),
			jen.Id("result").Dot(fieldName).Op("=").Id("__converted"),
		),
	}
}

// mirrorFieldType returns a jen statement referencing a local mirror struct name,
// preserving any pointer/slice wrapping from the original reflected type.
func mirrorFieldType(typ reflect.Type, mirrorName string) *jen.Statement {
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

	statement := jen.Id(mirrorName)
	for _, op := range slices.Backward(ops) {
		statement = jen.Op(op).Add(statement)
	}
	return statement
}

// Generate generates the adapter.go file for the given adapter package.
// selfPkg should be the full package path, e.g. "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
func Generate(selfPkg string) {
	selfAdapterPkg := selfPkg + "/adapter"
	selfUtilsPkg := selfPkg + "/utils"

	out := common.MakeFile()

	var methods []methodOut

	// Track mirror structs to avoid duplicates
	mirrors := newMirrorStructTracker()

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

		// Look up media params for this function
		methodMediaParams := introspected.MediaParams[methodName]

		// Track which params need struct conversion (nested media in classes)
		type structMediaParam struct {
			paramName   string
			mirrorName  string
			convertFunc string
			paramType   reflect.Type // original param type (may be ptr/slice wrapped)
		}
		var structMediaParams []structMediaParam

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

			var fieldType *jen.Statement
			if _, isMedia := methodMediaParams[paramName]; isMedia {
				// Direct media-typed param: use MediaInput with the same pointer/slice wrapping
				fieldType = jen.Id(strcase.UpperCamelCase(paramName)).Add(mediaFieldType(paramType))
			} else if structContainsMedia(paramType) {
				// Struct param containing nested media: use mirror struct
				mirrorName := mirrors.ensureMirrorStruct(out, paramType)
				fieldType = jen.Id(strcase.UpperCamelCase(paramName)).Add(mirrorFieldType(paramType, mirrorName))

				// Unwrap to get the inner type for the convert function name
				inner := paramType
				for inner.Kind() == reflect.Ptr || inner.Kind() == reflect.Slice {
					inner = inner.Elem()
				}
				structMediaParams = append(structMediaParams, structMediaParam{
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

				// Generate media conversion code for each direct media-typed param
				for paramIdx := 1; paramIdx < syncFuncType.NumIn()-1; paramIdx++ {
					argIdx := paramIdx - 1
					if argIdx >= len(args) {
						break
					}
					paramName := args[argIdx]
					mediaKind, isMedia := methodMediaParams[paramName]
					if !isMedia {
						continue
					}

					fieldName := strcase.UpperCamelCase(paramName)
					convertedVar := "__media_" + paramName
					paramType := syncFuncType.In(paramIdx)

					preamble = append(preamble, mediaConversionCode(convertedVar, fieldName, paramType, mediaKind)...)
				}

				// Generate struct conversion code for params with nested media.
				// Must handle direct, pointer, and slice wrapping of the struct param.
				for _, smp := range structMediaParams {
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

						preamble = append(preamble,
							jen.Id(convertedVar).Op(":=").Make(
								jen.Add(parseReflectType(smp.paramType).statement),
								jen.Len(jen.Id("input").Dot(fieldName)),
							),
							jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Id("input").Dot(fieldName)).Block(
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
							),
						)
					} else if isPtr {
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

		// Build a set of struct media param names for quick lookup
		structMediaParamSet := make(map[string]bool, len(structMediaParams))
		for _, smp := range structMediaParams {
			structMediaParamSet[smp.paramName] = true
		}

		// Helper to build a call parameter for an arg, using the converted variable for media params
		argCallParam := func(arg string) jen.Code {
			if _, isMedia := methodMediaParams[arg]; isMedia {
				return jen.Id("__media_" + arg)
			}
			if structMediaParamSet[arg] {
				return jen.Id("__struct_" + arg)
			}
			return jen.Id("input").Dot(strcase.UpperCamelCase(arg))
		}

		// Build call parameters for Stream method
		var streamCallParams []jen.Code
		streamCallParams = append(streamCallParams, jen.Id("adapter")) // context
		for _, arg := range args {
			streamCallParams = append(streamCallParams, argCallParam(arg))
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
			streamCallParamsNoOnTick = append(streamCallParamsNoOnTick, argCallParam(arg))
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

				// Handle partial - forward BAML's native partial via Stream() (only if not skipping partials)
				// Stream() returns *TStream - already a pointer, use directly
				jen.If(jen.Op("!").Id("skipPartials")).Block(
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
		// Accepts output channel from caller and skipPartials flag, returns only error
		out.Func().
			Id(noRawMethodName).
			Params(
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
				jen.Id("rawInput").Any(),
				jen.Id("out").Chan().Add(streamResultInterface.Clone()),
				jen.Id("skipPartials").Bool(),
			).
			Error().
			Block(noRawBody...)

		// ====== FULL IMPLEMENTATION: methodName_full ======
		// This path uses the ParseStream goroutine for partial results
		fullMethodName := strcase.LowerCamelCase(methodName + "_full")

		var fullBody []jen.Code
		fullBody = append(fullBody, makePreamble()...)

		fullBody = append(fullBody,
			// Unbounded queue for passing FunctionLog from OnTick to partials goroutine (never drops)
			jen.Id("funcLogQueue").Op(":=").Qual(common.GoConcurrentQueuePkg, "NewFIFO").Call(),
			// Context to signal partials goroutine to stop
			// NOT derived from adapter - we only cancel after ticksDone to ensure no late enqueues
			jen.List(jen.Id("queueCtx"), jen.Id("queueCancel")).Op(":=").Qual("context", "WithCancel").Call(jen.Qual("context", "Background").Call()),

			// Two-phase shutdown state
			jen.Var().Id("stopping").Qual("sync/atomic", "Bool"),   // shutdown requested
			jen.Var().Id("inTick").Qual("sync/atomic", "Int64"),    // number of onTick currently executing
			jen.Var().Id("pending").Qual("sync/atomic", "Int64"),   // number of queued items not yet completed
			jen.Id("ticksDone").Op(":=").Make(jen.Chan().Struct()), // closed when stopping && inTick==0
			jen.Id("allDone").Op(":=").Make(jen.Chan().Struct()),   // closed when stopping && pending==0
			jen.Var().Id("ticksOnce").Qual("sync", "Once"),
			jen.Var().Id("allOnce").Qual("sync", "Once"),
			jen.Var().Id("shutdownOnce").Qual("sync", "Once"),

			// Heartbeat tracking - sends a single heartbeat when first data is received
			jen.Var().Id("heartbeatSent").Qual("sync/atomic", "Bool"),

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

						// Send heartbeat on first tick - signals "data received" for hung detection
						jen.If(jen.Id("heartbeatSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(
							jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
							jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindHeartbeat"),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__r")).Block(),
								jen.Default().Block(
									jen.Id("__r").Dot("Release").Call(),
								),
							),
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

							// Skip ParseStream and partial emissions when skipIntermediateParsing is true
							// (raw is still accumulated by extractor for final result)
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
		// Accepts output channel from caller and skipIntermediateParsing flag, returns only error
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

		// Generate the public router function that dispatches based on StreamMode()
		// Creates the output channel and passes it to the inner implementation
		routerBody := []jen.Code{
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
			jen.Var().Id("err").Error(),
			jen.Id("mode").Op(":=").Id("adapter").Dot("StreamMode").Call(),
			jen.Switch(jen.Id("mode")).Block(
				// StreamModeCall: final only, no raw, skip partials
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCall")).Block(
					jen.Id("err").Op("=").Id(noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True()),
				),
				// StreamModeStream: partials + final, no raw
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStream")).Block(
					jen.Id("err").Op("=").Id(noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False()),
				),
				// StreamModeCallWithRaw: final + raw, skip intermediate parsing
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCallWithRaw")).Block(
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True()),
				),
				// StreamModeStreamWithRaw: partials + final + raw
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStreamWithRaw")).Block(
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False()),
				),
				// Default case to prevent silent hangs if unknown mode
				jen.Default().Block(
					jen.Id("err").Op("=").Qual("fmt", "Errorf").Call(
						jen.Lit("unknown StreamMode: %d"),
						jen.Id("mode"),
					),
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
			name:                   methodName,
			inputStructName:        inputStructName,
			outputStructQual:       finalResultType,
			streamOutputStructQual: streamType.statement,
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
			jen.Id("MakeStreamOutput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(method.streamOutputStructQual)),
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

		// Add DynamicProperties unwrapping for types that have it
		if isDynamic {
			parseBody = append(parseBody,
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

		parseBody = append(parseBody, jen.Return(jen.Id("result"), jen.Nil()))

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

	// Generate `applyDynamicTypes` - translates DynamicTypes to TypeBuilder calls
	generateApplyDynamicTypes(out)

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
	out.Func().Id("MakeAdapter").
		Params(jen.Id("ctx").Qual("context", "Context")).
		Qual(common.InterfacesPkg, "Adapter").
		Block(
			jen.Return(
				jen.Op("&").Qual(selfAdapterPkg, "BamlAdapter").
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
						jen.Id("MediaFactory"): jen.Id("createMedia"),
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
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").
						Call(jen.Id("adapter").Dot("TypeBuilder"))),
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

// mediaConversionCode generates code that converts a MediaInput field to the
// opaque BAML media type via bamlutils.ConvertMedia + a type assertion.
// Handles direct, pointer (optional), and slice (list) media types.
func mediaConversionCode(convertedVar, fieldName string, paramType reflect.Type, mediaKind bamlutils.MediaKind) []jen.Code {
	// Determine the wrapping: direct, pointer, or slice
	isPtr := paramType.Kind() == reflect.Ptr
	isSlice := paramType.Kind() == reflect.Slice

	// Resolve the innermost BAML type for the type assertion
	innerType := paramType
	for innerType.Kind() == reflect.Ptr || innerType.Kind() == reflect.Slice {
		innerType = innerType.Elem()
	}
	bamlType := parseReflectType(innerType).statement

	kindExpr := jen.Qual(common.InterfacesPkg, mediaKind.ConstName())

	if isSlice {
		// []MediaInput -> []baml.Image
		// Generate a loop that converts each element
		elemType := paramType.Elem()
		elemIsPtr := elemType.Kind() == reflect.Ptr

		var assertExpr *jen.Statement
		if elemIsPtr {
			assertExpr = parseReflectType(elemType).statement
		} else {
			assertExpr = bamlType.Clone()
		}

		// When elem is a pointer, the range variable is already *MediaInput;
		// pass it directly instead of taking &__mi (which would be **MediaInput).
		var miArg jen.Code
		if elemIsPtr {
			miArg = jen.Id("__mi")
		} else {
			miArg = jen.Op("&").Id("__mi")
		}

		var assignStmts []jen.Code
		if elemIsPtr {
			// For pointer elements (e.g., []*Image): assert to base type, then take address.
			// Can't type-assert `any` containing an interface value to a pointer-to-interface.
			assignStmts = []jen.Code{
				jen.Id("__converted").Op(":=").Id("__raw").Assert(bamlType.Clone()),
				jen.Id(convertedVar).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted"),
			}
		} else {
			assignStmts = []jen.Code{
				jen.Id(convertedVar).Index(jen.Id("__i")).Op("=").Id("__raw").Assert(assertExpr),
			}
		}

		conversionBlock := append([]jen.Code{
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
				jen.Id("adapter"),
				kindExpr.Clone(),
				miArg,
			),
			jen.If(jen.Id("__err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(
					jen.Lit(fieldName+"[%d]: %w"),
					jen.Id("__i"),
					jen.Id("__err"),
				)),
			),
		}, assignStmts...)

		// When elements are pointers (nullable), wrap the conversion in a nil check
		// so that null array elements stay as nil in the output slice.
		var loopBody []jen.Code
		if elemIsPtr {
			loopBody = []jen.Code{
				jen.If(jen.Id("__mi").Op("!=").Nil()).Block(conversionBlock...),
			}
		} else {
			loopBody = conversionBlock
		}

		return []jen.Code{
			jen.Id(convertedVar).Op(":=").Make(
				jen.Index().Add(parseReflectType(paramType.Elem()).statement),
				jen.Len(jen.Id("input").Dot(fieldName)),
			),
			jen.For(jen.List(jen.Id("__i"), jen.Id("__mi")).Op(":=").Range().Id("input").Dot(fieldName)).Block(loopBody...),
		}
	}

	if isPtr {
		innerAfterPtr := paramType.Elem()
		if innerAfterPtr.Kind() == reflect.Slice {
			// *[]Image (optional list, e.g., image[]?) — unwrap pointer, then iterate slice
			elemType := innerAfterPtr.Elem()
			elemIsPtr := elemType.Kind() == reflect.Ptr

			var elemAssert *jen.Statement
			if elemIsPtr {
				elemAssert = parseReflectType(elemType).statement
			} else {
				elemAssert = bamlType.Clone()
			}

			var miArg jen.Code
			if elemIsPtr {
				miArg = jen.Id("__mi")
			} else {
				miArg = jen.Op("&").Id("__mi")
			}

			sliceVar := "__slice_" + convertedVar

			var ptrSliceAssign []jen.Code
			if elemIsPtr {
				ptrSliceAssign = []jen.Code{
					jen.Id("__converted").Op(":=").Id("__raw").Assert(bamlType.Clone()),
					jen.Id(sliceVar).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted"),
				}
			} else {
				ptrSliceAssign = []jen.Code{
					jen.Id(sliceVar).Index(jen.Id("__i")).Op("=").Id("__raw").Assert(elemAssert),
				}
			}

			ptrSliceConv := append([]jen.Code{
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
					jen.Id("adapter"),
					kindExpr.Clone(),
					miArg,
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+"[%d]: %w"),
						jen.Id("__i"),
						jen.Id("__err"),
					)),
				),
			}, ptrSliceAssign...)

			var ptrSliceLoopBody []jen.Code
			if elemIsPtr {
				ptrSliceLoopBody = []jen.Code{
					jen.If(jen.Id("__mi").Op("!=").Nil()).Block(ptrSliceConv...),
				}
			} else {
				ptrSliceLoopBody = ptrSliceConv
			}
			return []jen.Code{
				jen.Var().Id(convertedVar).Add(parseReflectType(paramType).statement),
				jen.If(jen.Id("input").Dot(fieldName).Op("!=").Nil()).Block(
					jen.Id(sliceVar).Op(":=").Make(
						jen.Add(parseReflectType(innerAfterPtr).statement),
						jen.Len(jen.Op("*").Id("input").Dot(fieldName)),
					),
					jen.For(jen.List(jen.Id("__i"), jen.Id("__mi")).Op(":=").Range().Op("*").Id("input").Dot(fieldName)).Block(ptrSliceLoopBody...),
					jen.Id(convertedVar).Op("=").Op("&").Id(sliceVar),
				),
			}
		}

		// *MediaInput -> *baml.Image (optional single value)
		return []jen.Code{
			jen.Var().Id(convertedVar).Add(parseReflectType(paramType).statement),
			jen.If(jen.Id("input").Dot(fieldName).Op("!=").Nil()).Block(
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
					jen.Id("adapter"),
					kindExpr.Clone(),
					jen.Id("input").Dot(fieldName),
				),
				jen.If(jen.Id("__err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(
						jen.Lit(fieldName+": %w"),
						jen.Id("__err"),
					)),
				),
				jen.Id("__typed").Op(":=").Id("__raw").Assert(bamlType.Clone()),
				jen.Id(convertedVar).Op("=").Op("&").Id("__typed"),
			),
		}
	}

	// Direct: MediaInput -> baml.Image
	return []jen.Code{
		jen.List(jen.Id("__raw_"+convertedVar), jen.Id("__err_"+convertedVar)).Op(":=").Qual(common.InterfacesPkg, "ConvertMedia").Call(
			jen.Id("adapter"),
			kindExpr,
			jen.Op("&").Id("input").Dot(fieldName),
		),
		jen.If(jen.Id("__err_" + convertedVar).Op("!=").Nil()).Block(
			jen.Return(jen.Qual("fmt", "Errorf").Call(
				jen.Lit(fieldName+": %w"),
				jen.Id("__err_"+convertedVar),
			)),
		),
		jen.Id(convertedVar).Op(":=").Id("__raw_" + convertedVar).Assert(bamlType),
	}
}

// mediaFieldType returns a jen statement for a MediaInput field type,
// preserving any pointer/slice wrapping from the original reflected type.
// e.g., *baml.Image -> *bamlutils.MediaInput, []baml.Image -> []bamlutils.MediaInput
func mediaFieldType(typ reflect.Type) *jen.Statement {
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

	statement := jen.Qual(common.InterfacesPkg, "MediaInput")

	for _, op := range slices.Backward(ops) {
		statement = jen.Op(op).Add(statement)
	}

	return statement
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

// enumValueAttrsCode returns jen.Code statements for setting Description, Alias, and Skip
// on an enum value builder (vb) from a value struct (v).
func enumValueAttrsCode() []jen.Code {
	return []jen.Code{
		jen.If(jen.Id("v").Dot("Description").Op("!=").Lit("")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetDescription").Call(jen.Id("v").Dot("Description")),
		),
		jen.If(jen.Id("v").Dot("Alias").Op("!=").Lit("")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetAlias").Call(jen.Id("v").Dot("Alias")),
		),
		jen.If(jen.Id("v").Dot("Skip")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetSkip").Call(jen.True()),
		),
	}
}

// generateApplyDynamicTypes generates the applyDynamicTypes function that translates
// DynamicTypes JSON schema to imperative TypeBuilder calls.
// Uses the introspected package for type lookups instead of reflection.
func generateApplyDynamicTypes(out *jen.File) {
	// Use introspected.Type to be consistent with introspected.TypeBuilder
	// (both come from the generated client, not the runtime library directly)
	introspectedPkg := common.IntrospectedPkg
	typeAlias := jen.Qual(introspectedPkg, "Type")
	// Generate applyDynamicTypes function
	out.Func().Id("applyDynamicTypes").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("dt").Op("*").Qual(common.InterfacesPkg, "DynamicTypes"),
		).
		Error().
		Block(
			jen.If(jen.Id("dt").Op("==").Nil()).Block(
				jen.Return(jen.Nil()),
			),
			// Validate the schema before processing
			jen.If(jen.Id("err").Op(":=").Id("dt").Dot("Validate").Call(), jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("invalid dynamic_types schema: %w"), jen.Id("err"))),
			),
			// Create caches
			jen.Id("typeCache").Op(":=").Make(jen.Map(jen.String()).Add(typeAlias)),
			jen.Id("classBuilderCache").Op(":=").Make(jen.Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder")),
			jen.Line(),
			// Phase 1: Create all enum shells (for NEW enums only, with values since we have builder)
			jen.For(jen.List(jen.Id("name"), jen.Id("enum")).Op(":=").Range().Id("dt").Dot("Enums")).Block(
				jen.If(jen.Id("err").Op(":=").Id("createEnumShell").Call(jen.Id("tb"), jen.Id("name"), jen.Id("enum"), jen.Id("typeCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("enum %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 2: Add values to EXISTING enums
			jen.For(jen.List(jen.Id("name"), jen.Id("enum")).Op(":=").Range().Id("dt").Dot("Enums")).Block(
				jen.If(jen.Id("err").Op(":=").Id("addEnumValues").Call(jen.Id("tb"), jen.Id("name"), jen.Id("enum"), jen.Id("typeCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("enum %q values: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 3: Create all NEW class shells (no properties yet - just register the type)
			jen.For(jen.List(jen.Id("name"), jen.Id("_")).Op(":=").Range().Id("dt").Dot("Classes")).Block(
				jen.If(jen.Id("err").Op(":=").Id("createClassShell").Call(jen.Id("tb"), jen.Id("name"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 4a: Add properties to NEW classes only (from classBuilderCache)
			// These classes only have primitive types or reference other new classes
			jen.For(jen.List(jen.Id("name"), jen.Id("class")).Op(":=").Range().Id("dt").Dot("Classes")).Block(
				jen.If(jen.Id("err").Op(":=").Id("addNewClassProperties").Call(jen.Id("tb"), jen.Id("name"), jen.Id("class"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q properties: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 4b: Cache types for all NEW classes (now that properties are added)
			jen.For(jen.List(jen.Id("name"), jen.Id("cb")).Op(":=").Range().Id("classBuilderCache")).Block(
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("cb").Dot("Type").Call(),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q type: %w"), jen.Id("name"), jen.Id("err"))),
				),
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
			),
			jen.Line(),
			// Phase 4c: Add properties to EXISTING dynamic classes (refs can now be resolved)
			jen.For(jen.List(jen.Id("name"), jen.Id("class")).Op(":=").Range().Id("dt").Dot("Classes")).Block(
				jen.If(jen.Id("err").Op(":=").Id("addExistingClassProperties").Call(jen.Id("tb"), jen.Id("name"), jen.Id("class"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q properties: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate createEnumShell helper - creates enum with values (new) or skips existing
	// For NEW enums: creates enum AND adds values (since we have the builder)
	// For EXISTING enums: does nothing (values added in Phase 2 via introspected accessors)
	// IMPORTANT: We must check if the enum already exists BEFORE calling AddEnum, because
	// calling AddEnum on an existing enum may have side effects in BAML's internal state.
	out.Func().Id("createEnumShell").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("enum").Op("*").Qual(common.InterfacesPkg, "DynamicEnum"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
		).
		Error().
		Block(
			// Check if enum already exists (either dynamic or static from baml_src)
			// If so, skip - values will be added in Phase 2 for dynamic enums
			jen.If(jen.Qual(introspectedPkg, "EnumExists").Call(jen.Id("name"))).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Create new enum
			jen.List(jen.Id("eb"), jen.Id("err")).Op(":=").Id("tb").Dot("AddEnum").Call(jen.Id("name")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to create enum: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// NEW enum - add values now since we have the builder
			jen.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id("enum").Dot("Values")).Block(
				append([]jen.Code{
					jen.List(jen.Id("vb"), jen.Id("err")).Op(":=").Id("eb").Dot("AddValue").Call(jen.Id("v").Dot("Name")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Continue(), // Value might already exist
					),
				}, enumValueAttrsCode()...)...,
			),
			jen.Line(),
			// Cache the new enum's type
			jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("eb").Dot("Type").Call(),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get type: %w"), jen.Id("err"))),
			),
			jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
			jen.Return(jen.Nil()),
		)

	// Generate addEnumValues helper - adds values to EXISTING dynamic enums (Phase 2)
	// Only for enums that already existed in baml_src (marked @@dynamic)
	// New enums already had values added in createEnumShell
	out.Func().Id("addEnumValues").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("enum").Op("*").Qual(common.InterfacesPkg, "DynamicEnum"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
		).
		Error().
		Block(
			jen.Id("_").Op("=").Id("typeCache"), // unused
			// Check if this is an existing dynamic enum via introspected accessor
			jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Qual(introspectedPkg, "DynamicEnums").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Not a dynamic enum OR it's a new enum (values already added in Phase 1)
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Get the enum builder using the typed accessor
			jen.List(jen.Id("eb"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get enum builder: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Add values using the typed EnumBuilder interface
			jen.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id("enum").Dot("Values")).Block(
				append([]jen.Code{
					jen.List(jen.Id("vb"), jen.Id("err")).Op(":=").Id("eb").Dot("AddValue").Call(jen.Id("v").Dot("Name")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						// Value might already exist, skip silently
						jen.Continue(),
					),
				}, enumValueAttrsCode()...)...,
			),
			jen.Return(jen.Nil()),
		)

	// Generate createClassShell helper - creates class shell only (no properties, no type caching)
	// For NEW classes: creates class, caches builder for Phase 4a
	// For EXISTING classes: does nothing (properties added in Phase 4c via introspected accessors)
	// IMPORTANT: We must check if the class already exists BEFORE calling AddClass, because
	// calling AddClass on an existing class may have side effects in BAML's internal state.
	// IMPORTANT: We do NOT call Type() here - that happens in Phase 4b AFTER properties are added
	out.Func().Id("createClassShell").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			jen.Id("_").Op("=").Id("typeCache"), // unused in this function
			// Check if class already exists (either dynamic or static from baml_src)
			// If so, skip - properties will be added in Phase 4c for dynamic classes
			jen.If(jen.Qual(introspectedPkg, "ClassExists").Call(jen.Id("name"))).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Create new class shell (no properties yet, no Type() call)
			jen.List(jen.Id("cb"), jen.Id("err")).Op(":=").Id("tb").Dot("AddClass").Call(jen.Id("name")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to create class: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Cache the builder for Phase 4a property additions
			// Type will be cached in Phase 4b AFTER properties are added
			jen.Id("classBuilderCache").Index(jen.Id("name")).Op("=").Id("cb"),
			jen.Return(jen.Nil()),
		)

	// Generate addNewClassProperties helper - adds properties to NEW classes only (Phase 4a)
	// Only processes classes that are in classBuilderCache (created in Phase 3)
	// Properties should only reference primitive types or other new classes
	out.Func().Id("addNewClassProperties").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("class").Op("*").Qual(common.InterfacesPkg, "DynamicClass"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			// Only process NEW classes (in classBuilderCache)
			jen.List(jen.Id("cb"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Not a new class - skip (will be processed in Phase 4c)
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Add properties to the new class builder
			jen.For(jen.List(jen.Id("propName"), jen.Id("prop")).Op(":=").Range().Id("class").Dot("Properties")).Block(
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolvePropertyType").Call(jen.Id("tb"), jen.Id("prop"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					// Skip unresolved refs - they may reference existing types in baml_src
					// which will be resolved when the type is used
					jen.If(jen.Qual("strings", "Contains").Call(jen.Id("err").Dot("Error").Call(), jen.Lit("unresolved reference"))).Block(
						jen.Continue(),
					),
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q type: %w"), jen.Id("propName"), jen.Id("err"))),
				),
				jen.Line(),
				// Call AddProperty using the typed ClassBuilder interface
				jen.List(jen.Id("_"), jen.Id("err")).Op("=").Id("cb").Dot("AddProperty").Call(jen.Id("propName"), jen.Id("typ")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q: %w"), jen.Id("propName"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate addExistingClassProperties helper - adds properties to EXISTING dynamic classes (Phase 4c)
	// Only processes classes that are in introspected.DynamicClasses (from baml_src with @@dynamic)
	// At this point, all new class types are cached in typeCache (from Phase 4b)
	out.Func().Id("addExistingClassProperties").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("class").Op("*").Qual(common.InterfacesPkg, "DynamicClass"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			// Skip if this is a NEW class (already processed in Phase 4a)
			jen.If(jen.List(jen.Id("_"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Check if it's an EXISTING dynamic class
			jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Qual(introspectedPkg, "DynamicClasses").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Class is read-only (static) - cannot add properties
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Get the class builder using the typed accessor
			jen.List(jen.Id("cb"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get class builder: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Add properties to the existing class builder
			jen.For(jen.List(jen.Id("propName"), jen.Id("prop")).Op(":=").Range().Id("class").Dot("Properties")).Block(
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolvePropertyType").Call(jen.Id("tb"), jen.Id("prop"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					// Skip unresolved refs - they may reference types in baml_src
					jen.If(jen.Qual("strings", "Contains").Call(jen.Id("err").Dot("Error").Call(), jen.Lit("unresolved reference"))).Block(
						jen.Continue(),
					),
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q type: %w"), jen.Id("propName"), jen.Id("err"))),
				),
				jen.Line(),
				// Call AddProperty using the typed ClassBuilder interface
				jen.List(jen.Id("_"), jen.Id("err")).Op("=").Id("cb").Dot("AddProperty").Call(jen.Id("propName"), jen.Id("typ")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q: %w"), jen.Id("propName"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate resolvePropertyType helper
	out.Func().Id("resolvePropertyType").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("prop").Op("*").Qual(common.InterfacesPkg, "DynamicProperty"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			// Handle $ref
			jen.If(jen.Id("prop").Dot("Ref").Op("!=").Lit("")).Block(
				jen.Return(jen.Id("resolveRef").Call(jen.Id("tb"), jen.Id("prop").Dot("Ref"), jen.Id("typeCache"), jen.Id("classBuilderCache"))),
			),
			jen.Line(),
			// Convert to DynamicTypeSpec and resolve
			jen.Return(jen.Id("resolveTypeRef").Call(
				jen.Id("tb"),
				jen.Op("&").Qual(common.InterfacesPkg, "DynamicTypeSpec").Values(jen.Dict{
					jen.Id("Type"):   jen.Id("prop").Dot("Type"),
					jen.Id("Items"):  jen.Id("prop").Dot("Items"),
					jen.Id("Inner"):  jen.Id("prop").Dot("Inner"),
					jen.Id("OneOf"):  jen.Id("prop").Dot("OneOf"),
					jen.Id("Keys"):   jen.Id("prop").Dot("Keys"),
					jen.Id("Values"): jen.Id("prop").Dot("Values"),
					jen.Id("Value"):  jen.Id("prop").Dot("Value"),
				}),
				jen.Id("typeCache"),
				jen.Id("classBuilderCache"),
			)),
		)

	// Generate resolveTypeRef helper
	out.Func().Id("resolveTypeRef").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("ref").Op("*").Qual(common.InterfacesPkg, "DynamicTypeSpec"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			jen.If(jen.Id("ref").Op("==").Nil()).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("nil type reference"))),
			),
			jen.Line(),
			// Handle $ref
			jen.If(jen.Id("ref").Dot("Ref").Op("!=").Lit("")).Block(
				jen.Return(jen.Id("resolveRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Ref"), jen.Id("typeCache"), jen.Id("classBuilderCache"))),
			),
			jen.Line(),
			jen.Switch(jen.Id("ref").Dot("Type")).Block(
				jen.Case(jen.Lit("string")).Block(
					jen.Return(jen.Id("tb").Dot("String").Call()),
				),
				jen.Case(jen.Lit("int")).Block(
					jen.Return(jen.Id("tb").Dot("Int").Call()),
				),
				jen.Case(jen.Lit("float")).Block(
					jen.Return(jen.Id("tb").Dot("Float").Call()),
				),
				jen.Case(jen.Lit("bool")).Block(
					jen.Return(jen.Id("tb").Dot("Bool").Call()),
				),
				jen.Case(jen.Lit("null")).Block(
					jen.Return(jen.Id("tb").Dot("Null").Call()),
				),
				jen.Case(jen.Lit("literal_string")).Block(
					jen.List(jen.Id("str"), jen.Id("ok")).Op(":=").Id("ref").Dot("Value").Assert(jen.String()),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_string value must be a string, got %T"), jen.Id("ref").Dot("Value"))),
					),
					jen.Return(jen.Id("tb").Dot("LiteralString").Call(jen.Id("str"))),
				),
				jen.Case(jen.Lit("literal_int")).Block(
					jen.Switch(jen.Id("v").Op(":=").Id("ref").Dot("Value").Assert(jen.Type())).Block(
						jen.Case(jen.Float64()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Int64().Call(jen.Id("v")))),
						),
						jen.Case(jen.Int64()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Id("v"))),
						),
						jen.Case(jen.Int()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Int64().Call(jen.Id("v")))),
						),
						jen.Default().Block(
							jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_int value must be a number, got %T"), jen.Id("ref").Dot("Value"))),
						),
					),
				),
				jen.Case(jen.Lit("literal_bool")).Block(
					jen.List(jen.Id("b"), jen.Id("ok")).Op(":=").Id("ref").Dot("Value").Assert(jen.Bool()),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_bool value must be a boolean, got %T"), jen.Id("ref").Dot("Value"))),
					),
					jen.Return(jen.Id("tb").Dot("LiteralBool").Call(jen.Id("b"))),
				),
				jen.Case(jen.Lit("list")).Block(
					jen.If(jen.Id("ref").Dot("Items").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("list type requires 'items' field"))),
					),
					jen.List(jen.Id("inner"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Items"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("list items: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("List").Call(jen.Id("inner"))),
				),
				jen.Case(jen.Lit("optional")).Block(
					jen.If(jen.Id("ref").Dot("Inner").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("optional type requires 'inner' field"))),
					),
					jen.List(jen.Id("inner"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Inner"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("optional inner: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("Optional").Call(jen.Id("inner"))),
				),
				jen.Case(jen.Lit("map")).Block(
					jen.If(jen.Id("ref").Dot("Keys").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map type requires 'keys' field"))),
					),
					jen.If(jen.Id("ref").Dot("Values").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map type requires 'values' field"))),
					),
					jen.List(jen.Id("keys"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Keys"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map keys: %w"), jen.Id("err"))),
					),
					jen.List(jen.Id("values"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Values"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map values: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("Map").Call(jen.Id("keys"), jen.Id("values"))),
				),
				jen.Case(jen.Lit("union")).Block(
					jen.If(jen.Len(jen.Id("ref").Dot("OneOf")).Op("==").Lit(0)).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("union type requires 'oneOf' field with at least one type"))),
					),
					jen.Id("types").Op(":=").Make(jen.Index().Add(typeAlias), jen.Lit(0), jen.Len(jen.Id("ref").Dot("OneOf"))),
					jen.For(jen.List(jen.Id("i"), jen.Id("item")).Op(":=").Range().Id("ref").Dot("OneOf")).Block(
						jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("item"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("union oneOf[%d]: %w"), jen.Id("i"), jen.Id("err"))),
						),
						jen.Id("types").Op("=").Append(jen.Id("types"), jen.Id("typ")),
					),
					jen.Return(jen.Id("tb").Dot("Union").Call(jen.Id("types"))),
				),
				jen.Case(jen.Lit("")).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("type field is required"))),
				),
				jen.Default().Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("unknown type: %q"), jen.Id("ref").Dot("Type"))),
				),
			),
		)

	// Generate resolveRef helper
	// Updated to also accept classBuilderCache for resolving newly created classes
	out.Func().Id("resolveRef").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			// Check cache first (from dynamic_types)
			jen.If(jen.List(jen.Id("typ"), jen.Id("ok")).Op(":=").Id("typeCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Check if this is a newly created class - get type directly from builder
			jen.If(jen.List(jen.Id("cb"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("cb").Dot("Type").Call(),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("get type for class %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Try to get existing class via introspected accessor
			jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Qual(introspectedPkg, "GetClassType").Call(jen.Id("tb"), jen.Id("name")),
			jen.If(jen.Id("err").Op("==").Nil().Op("&&").Id("typ").Op("!=").Nil()).Block(
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Try to get existing enum via introspected accessor
			jen.List(jen.Id("typ"), jen.Id("err")).Op("=").Qual(introspectedPkg, "GetEnumType").Call(jen.Id("tb"), jen.Id("name")),
			jen.If(jen.Id("err").Op("==").Nil().Op("&&").Id("typ").Op("!=").Nil()).Block(
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("unresolved reference: %q"), jen.Id("name"))),
		)
}
