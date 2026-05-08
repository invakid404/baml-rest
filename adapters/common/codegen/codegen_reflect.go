package codegen

import (
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
)

// methodOut summarises a streaming method's emitted types so the
// post-loop Methods map can reference them without re-deriving from
// reflect.Type.
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
// Uses a visited set to break cycles from self-referential or mutually recursive types.
func structContainsMedia(typ reflect.Type) bool {
	return structContainsMediaVisited(typ, make(map[reflect.Type]bool))
}

func structContainsMediaVisited(typ reflect.Type, visited map[reflect.Type]bool) bool {
	// Unwrap pointer and slice layers
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return false
	}
	if visited[typ] {
		return false
	}
	visited[typ] = true
	for field := range typ.Fields() {
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
		if inner.Kind() == reflect.Struct && structContainsMediaVisited(ft, visited) {
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
	for field := range inner.Fields() {
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
	for field := range inner.Fields() {
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

	for field := range bamlType.Fields() {
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

		conversionBlock := []jen.Code{
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
				if elemIsPtr {
					return jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Op("&").Id("__converted")
				}
				return jen.Id("result").Dot(fieldName).Index(jen.Id("__i")).Op("=").Id("__converted")
			}(),
		}

		// For pointer elements, wrap in a nil guard to preserve null entries
		var loopBody []jen.Code
		if elemIsPtr {
			loopBody = []jen.Code{
				jen.If(jen.Id("__v").Op("!=").Nil()).Block(conversionBlock...),
			}
		} else {
			loopBody = conversionBlock
		}

		return []jen.Code{
			jen.Id("result").Dot(fieldName).Op("=").Make(
				jen.Add(parseReflectType(fieldType).statement),
				jen.Len(srcExpr.Clone()),
			),
			jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Add(srcExpr.Clone())).Block(loopBody...),
		}
	}

	if isPtr {
		innerAfterPtr := fieldType.Elem()
		if innerAfterPtr.Kind() == reflect.Slice {
			// *[]Struct (optional list of structs with media, e.g. ContentPart[]?)
			elemType := innerAfterPtr.Elem()
			elemIsPtr := elemType.Kind() == reflect.Ptr

			var vArg jen.Code
			if elemIsPtr {
				vArg = jen.Id("__v")
			} else {
				vArg = jen.Op("&").Id("__v")
			}

			innerConvBlock := []jen.Code{
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
					if elemIsPtr {
						return jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Op("&").Id("__converted")
					}
					return jen.Id("__ptrSlice").Index(jen.Id("__i")).Op("=").Id("__converted")
				}(),
			}

			// For pointer elements, wrap in a nil guard to preserve null entries
			var innerLoopBody []jen.Code
			if elemIsPtr {
				innerLoopBody = []jen.Code{
					jen.If(jen.Id("__v").Op("!=").Nil()).Block(innerConvBlock...),
				}
			} else {
				innerLoopBody = innerConvBlock
			}

			return []jen.Code{
				jen.If(srcExpr.Clone().Op("!=").Nil()).Block(
					jen.Id("__ptrSlice").Op(":=").Make(
						jen.Add(parseReflectType(innerAfterPtr).statement),
						jen.Len(jen.Op("*").Add(srcExpr.Clone())),
					),
					jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Op("*").Add(srcExpr.Clone())).Block(innerLoopBody...),
					jen.Id("result").Dot(fieldName).Op("=").Op("&").Id("__ptrSlice"),
				),
			}
		}

		// *Struct (optional single nested struct with media)
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

// parsedReflectType is a Jen type expression plus the generic type
// arguments that were extracted from the reflected type's name.
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
