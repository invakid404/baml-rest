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

// emitBAMLHTTPRequestConversion generates the jen code that converts a
// baml.HTTPRequest (stored in local variable "httpReq") into a
// *llmhttp.Request. This is shared by both the streaming _buildRequest
// and non-streaming _buildCallRequest codegen paths to avoid duplication
// and ensure they stay in sync.
func emitBAMLHTTPRequestConversion(g *jen.Group) {
	g.List(jen.Id("url"), jen.Id("urlErr")).Op(":=").Id("httpReq").Dot("Url").Call()
	g.If(jen.Id("urlErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get URL: %w"), jen.Id("urlErr"))),
	)
	g.List(jen.Id("method"), jen.Id("methodErr")).Op(":=").Id("httpReq").Dot("Method").Call()
	g.If(jen.Id("methodErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get method: %w"), jen.Id("methodErr"))),
	)
	g.List(jen.Id("headers"), jen.Id("headersErr")).Op(":=").Id("httpReq").Dot("Headers").Call()
	g.If(jen.Id("headersErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get headers: %w"), jen.Id("headersErr"))),
	)
	g.List(jen.Id("body"), jen.Id("bodyErr")).Op(":=").Id("httpReq").Dot("Body").Call()
	g.If(jen.Id("bodyErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get body: %w"), jen.Id("bodyErr"))),
	)
	g.List(jen.Id("bodyText"), jen.Id("bodyTextErr")).Op(":=").Id("body").Dot("Text").Call()
	g.If(jen.Id("bodyTextErr").Op("!=").Nil()).Block(
		jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to get body text: %w"), jen.Id("bodyTextErr"))),
	)
	g.Return(jen.Op("&").Qual(common.LLMHTTPPkg, "Request").Values(jen.Dict{
		jen.Id("URL"):     jen.Id("url"),
		jen.Id("Method"):  jen.Id("method"),
		jen.Id("Headers"): jen.Id("headers"),
		jen.Id("Body"):    jen.Id("bodyText"),
	}), jen.Nil())
}

// Generate generates the adapter.go file for the given adapter package.
// selfPkg should be the full package path, e.g. "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
// Options configures code generation per adapter. Each adapter's
// cmd/main.go populates this with its BAML-version-specific feature
// flags before invoking Generate.
type Options struct {
	// SelfPkg is the adapter module's import path, e.g.
	// "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0".
	SelfPkg string
	// SupportsWithClient reports whether the bundled BAML runtime
	// exposes the `WithClient(clientName)` CallOption. Added in BAML
	// 0.219.0; older runtimes (v0.204, v0.215) lack the symbol and
	// emitting a reference to it produces an "undefined: WithClient"
	// compile error. When false, the generated router skips the
	// per-call client override and the legacy path relies on BAML's
	// own strategy rotation for baml-roundrobin chains.
	SupportsWithClient bool
}

// Generate is the legacy entry point kept for backward compatibility
// with existing adapter cmd/main.go callers that pass only the self
// package. It assumes the full modern feature set, matching BAML
// 0.219+ behaviour. Callers on older BAML runtimes should use
// GenerateWithOptions with SupportsWithClient=false.
func Generate(selfPkg string) {
	GenerateWithOptions(Options{SelfPkg: selfPkg, SupportsWithClient: true})
}

// GenerateWithOptions runs code generation with per-adapter feature
// flags. See Options for the available knobs.
func GenerateWithOptions(opts Options) {
	generate(opts)
}

func generate(opts Options) {
	selfPkg := opts.SelfPkg
	supportsWithClient := opts.SupportsWithClient

	// withClientOverrideBlock wraps a `clientOverride != ""` guard around
	// WithClient(clientOverride) appends. When the target BAML runtime
	// lacks the WithClient CallOption (< 0.219.0), the whole block is
	// elided — emitting jen.Empty() instead of a reference that would
	// produce "undefined: WithClient" at adapter compile time. In that
	// mode the legacy dispatcher's per-attempt client override becomes
	// a no-op; baml-roundrobin strategy clients fall back to BAML's
	// internal rotation, which is the pre-47e3203 behaviour.
	withClientOverrideBlock := func(body ...jen.Code) jen.Code {
		if !supportsWithClient {
			return jen.Empty()
		}
		return jen.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(body...)
	}
	selfAdapterPkg := selfPkg + "/adapter"
	selfUtilsPkg := selfPkg + "/utils"

	out := common.MakeFile()

	var methods []methodOut

	// emitDynamicUnwrapFunc generates an in-place dynamic properties unwrap function
	// for a pointer type. The generated function mutates val.DynamicProperties once
	// so getters need no work. Used by both the streaming-method loop and the
	// parse-only method loop to avoid duplicating the Jen AST.
	emitDynamicUnwrapFunc := func(funcName string, typePtr *jen.Statement) {
		out.Func().Id(funcName).
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
							jen.Id("val").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("reflectValue").Dot("Interface").Call()),
						).Else().Block(
							jen.Id("val").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Qual(selfUtilsPkg, "UnwrapDynamicValue").Call(jen.Id("value")),
						),
					),
			)
	}

	// Track mirror structs to avoid duplicates
	mirrors := newMirrorStructTracker()

	// Track which methods had their dynamic unwrap helpers emitted by the streaming loop.
	// Parse-only methods (in ParseMethods but not ParseStreamMethods) won't get helpers
	// from the streaming loop, so the parse-method loop must emit them itself.
	emittedUnwrapHelpers := make(map[string]bool)

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

		// Output struct holds: kind, raw LLM response, typed parsed values, error,
		// and optional routing metadata.
		// streamParsed and finalParsed are typed pointer fields that eliminate the
		// interface boxing and runtime type assertions of a single `parsed any` field.
		// metadata is populated only when kind==StreamResultKindMetadata.
		out.Type().Id(outputStructName).Struct(
			jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
			jen.Id("raw").String(),
			jen.Id("streamParsed").Add(streamTypePtr.Clone()),
			jen.Id("finalParsed").Add(finalTypePtr.Clone()),
			jen.Id("err").Error(),
			jen.Id("reset").Bool(), // true when client should discard accumulated state (retry occurred)
			jen.Id("metadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
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

		// Generate unwrap helpers for dynamic types (called at setter time, not getter time)
		unwrapStreamFuncName := strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sStream", outputStructName))
		unwrapFinalFuncName := strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sFinal", outputStructName))
		if isDynamicStream {
			emitDynamicUnwrapFunc(unwrapStreamFuncName, streamTypePtr)
		}
		if isDynamicFinal {
			emitDynamicUnwrapFunc(unwrapFinalFuncName, finalTypePtr)
			emittedUnwrapHelpers[unwrapFinalFuncName] = true
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

		// Generate metadata constructor function. Produces a StreamResult whose
		// Kind()==StreamResultKindMetadata and whose Metadata() returns the
		// supplied payload. Uses the same pool as the regular result path.
		metadataConstructorName := strcase.LowerCamelCase(fmt.Sprintf("new%sMetadata", outputStructName))
		out.Func().
			Id(metadataConstructorName).
			Params(jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata")).
			Op("*").Id(outputStructName).
			Block(
				jen.Id("r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindMetadata"),
				jen.Id("r").Dot("metadata").Op("=").Id("md"),
				jen.Return(jen.Id("r")),
			)

		streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

		// Helper: common preamble for both implementations
		// Inner methods return only error, so early returns just return the error
		//
		// optionsHelperName selects which generated registry-options
		// helper feeds the dispatch site. The default
		// makeOptionsFromAdapter pulls the BuildRequest-safe registry
		// view (drops every baml-rest-resolved strategy parent), used
		// by BuildRequest dispatch and by mixed-mode bridge legacy-
		// child callbacks (both target leaves, not parents).
		// makeLegacyOptionsFromAdapter pulls the legacy view (preserves
		// explicit parent overrides) and is used by the top-level
		// final-legacy fallthrough so BAML can honour runtime
		// strategy-parent overrides or emit canonical errors. See PR
		// #192 cold-review-4 + Option C.
		makePreambleWith := func(optionsHelperName string) []jen.Code {
			preamble := []jen.Code{
				jen.List(jen.Id("options"), jen.Id("err")).Op(":=").Id(optionsHelperName).Call(jen.Id("adapter")),
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

		// Convenience wrappers that pin the registry view at each
		// dispatch site. BuildRequest landing sites (the buildRequest
		// / buildCallRequest paths) get makePreamble; the top-level
		// legacy fallthrough impls (_noRaw / _full) get
		// makeLegacyPreamble. See PR #192 cold-review-4 + Option C.
		makePreamble := func() []jen.Code {
			return makePreambleWith("makeOptionsFromAdapter")
		}
		makeLegacyPreamble := func() []jen.Code {
			return makePreambleWith("makeLegacyOptionsFromAdapter")
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
		// ====== SIMPLIFIED IMPLEMENTATION: methodName_noRaw ======
		// This path uses BAML's native streaming without raw collection overhead.
		// Partials come directly from BAML's stream, no OnTick/SSE parsing needed.
		noRawMethodName := strcase.LowerCamelCase(methodName + "_noRaw")

		// Build call parameters for the noRaw Stream call (includes WithOnTick for heartbeat)
		var noRawStreamCallParams []jen.Code
		noRawStreamCallParams = append(noRawStreamCallParams, jen.Id("adapter")) // context
		for _, arg := range args {
			noRawStreamCallParams = append(noRawStreamCallParams, argCallParam(arg))
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
				jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
			),
			withClientOverrideBlock(
				jen.Id("streamOpts").Op("=").Append(
					jen.Qual("slices", "Clone").Call(jen.Id("streamOpts")),
					jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
				),
			),
			// Call Stream WITH OnTick for heartbeat tracking, but still use native streaming for data
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(noRawStreamCallParams...),

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
				// Final() already returns *TFinal from the BAML generated client — assign directly.
				jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
					jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindFinal"),
					jen.Id("__r").Dot("finalParsed").Op("=").Id("streamVal").Dot("Final").Call(),
					func() jen.Code {
						if isDynamicFinal {
							return jen.Id(unwrapFinalFuncName).Call(jen.Id("__r").Dot("finalParsed"))
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
							if isDynamicStream {
								return jen.Id(unwrapStreamFuncName).Call(jen.Id("__partial"))
							}
							return jen.Null()
						}(),
						jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
						jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
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
		// runtime strategy-parent overrides are visible to BAML — see
		// PR #192 cold-review-4 + Option C.
		noRawBody := makeLegacyPreamble()

		// Delegate to shared orchestration helper - supplies per-method closures
		noRawBody = append(noRawBody,
			jen.Return(jen.Id("runNoRawOrchestration").Call(
				jen.Id("adapter"),
				jen.Id("out"),
				// newHeartbeat
				jen.Func().Params().Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindHeartbeat"),
					jen.Return(jen.Id("__r")),
				),
				// newError
				jen.Func().Params(jen.Id("err").Error()).Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Return(jen.Id(errorConstructorName).Call(jen.Id("err"))),
				),
				// release
				jen.Func().Params(jen.Id("__r").Qual(common.InterfacesPkg, "StreamResult")).Block(
					jen.Id("__r").Dot("Release").Call(),
				),
				// plannedMetadata: passed straight through; orchestrator
				// builds both planned and outcome events from this seed.
				jen.Id("plannedMetadata"),
				// newMetadataResult: pool-wraps a Metadata payload.
				jen.Func().Params(jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata")).Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Return(jen.Id(metadataConstructorName).Call(jen.Id("md"))),
				),
				// body - receives beforeFinal + onTick, handles stream creation and iteration
				jen.Func().Params(jen.Id("beforeFinal").Func().Params(), jen.Id("onTick").Add(onTickType())).Error().Block(noRawGoroutineBody...),
			)),
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
				jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
				jen.Id("clientOverride").String(),
			).
			Error().
			Block(noRawBody...)

		// ====== FULL IMPLEMENTATION: methodName_full ======
		// This path uses the ParseStream goroutine for partial results
		fullMethodName := strcase.LowerCamelCase(methodName + "_full")

		// Build the per-tick processing closure for the full path.
		// This captures method-specific state (options, adapter, output pool, ParseStream call).
		processTickBody := []jen.Code{
			jen.Id("calls").Op(",").Id("callsErr").Op(":=").Id("funcLog").Dot("Calls").Call(),
			jen.If(jen.Id("callsErr").Op("!=").Nil()).Block(jen.Return(jen.Nil())),
			jen.Id("callCount").Op(":=").Len(jen.Id("calls")),
			jen.If(jen.Id("callCount").Op("==").Lit(0)).Block(jen.Return(jen.Nil())),
			jen.Id("lastCall").Op(":=").Id("calls").Index(jen.Id("callCount").Op("-").Lit(1)),
			jen.Id("streamCall").Op(",").Id("ok").Op(":=").Id("lastCall").Assert(jen.Qual(BamlPkg, "LLMStreamCall")),
			jen.If(jen.Op("!").Id("ok")).Block(jen.Return(jen.Nil())),
			jen.Id("provider").Op(",").Id("provErr").Op(":=").Id("streamCall").Dot("Provider").Call(),
			jen.If(jen.Id("provErr").Op("!=").Nil()).Block(jen.Return(jen.Nil())),
			// Meta-providers like "baml-fallback" are not handled by
			// ExtractDeltaFromText. Resolve the actual child provider using
			// the same precedence as BuildRequest's resolveChildProvider:
			//   1. Scan child calls for a supported runtime-reported provider
			//   2. Runtime client_registry override for the selected child
			//   3. Static introspected.ClientProvider map (only if supported)
			// If none match, leave `provider` unchanged (ExtractDeltaFromText
			// will fail the unsupported-provider check and skip extraction
			// rather than using a stale or unsupported value).
			jen.If(jen.Op("!").Qual(common.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("provider"))).Block(
				jen.Id("resolved").Op(":=").Lit(false),
				// 1. Prefer the runtime-reported provider from a child call.
				jen.For(jen.Id("i").Op(":=").Id("callCount").Op("-").Lit(1), jen.Id("i").Op(">=").Lit(0).Op("&&").Op("!").Id("resolved"), jen.Id("i").Op("--")).Block(
					jen.If(
						jen.List(jen.Id("sc"), jen.Id("scOk")).Op(":=").Id("calls").Index(jen.Id("i")).Assert(jen.Qual(BamlPkg, "LLMStreamCall")),
						jen.Id("scOk"),
					).Block(
						jen.If(
							jen.List(jen.Id("cp"), jen.Id("cpErr")).Op(":=").Id("sc").Dot("Provider").Call(),
							jen.Id("cpErr").Op("==").Nil().Op("&&").Qual(common.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("cp")),
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
									jen.Id("rc").Op("!=").Nil().Op("&&").Id("rc").Dot("Name").Op("==").Id("clientName").Op("&&").Id("rc").Dot("Provider").Op("!=").Lit("").Op("&&").Qual(common.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("rc").Dot("Provider")),
								).Block(
									jen.Id("provider").Op("=").Id("rc").Dot("Provider"),
									jen.Id("resolved").Op("=").Lit(true),
									jen.Break(),
								),
							),
						),
						// 3. Static introspected.ClientProvider (only if supported).
						jen.If(jen.Op("!").Id("resolved")).Block(
							jen.If(
								jen.List(jen.Id("sp"), jen.Id("spOk")).Op(":=").Qual(common.IntrospectedPkg, "ClientProvider").Index(jen.Id("clientName")),
								jen.Id("spOk").Op("&&").Qual(common.SSEPkg, "IsDeltaProviderSupported").Call(jen.Id("sp")),
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
			jen.Id("extractResult").Op(":=").Qual(common.SSEPkg, "ExtractFrom").Call(
				jen.Id("extractor"), jen.Id("callCount"), jen.Id("provider"), jen.Id("chunks"),
			),
			jen.If(jen.Id("skipIntermediateParsing")).Block(jen.Return(jen.Nil())),
			// Skip if absolutely nothing new arrived this tick (no delta on
			// either buffer) and no reset occurred.
			jen.If(
				jen.Id("extractResult").Dot("ParseableDelta").Op("==").Lit("").
					Op("&&").Id("extractResult").Dot("RawDelta").Op("==").Lit("").
					Op("&&").Op("!").Id("extractResult").Dot("Reset"),
			).Block(jen.Return(jen.Nil())),
			// parseable is the cumulative text-only buffer fed to ParseStream.
			// Reasoning content (e.g. Anthropic thinking_delta under
			// IncludeThinkingInRaw=true) is excluded by construction in
			// IncrementalExtractor, so the BAML parser sees only the textual
			// response stream regardless of the opt-in flag.
			jen.Id("parseable").Op(":=").Id("extractResult").Dot("ParseableFull"),
			jen.Id("parseableDelta").Op(":=").Id("extractResult").Dot("ParseableDelta"),
			// rawDelta is the per-tick wire-output content. Mirrors
			// parseableDelta when the opt-in is off; additionally carries
			// thinking_delta content under opt-in.
			jen.Id("rawDelta").Op(":=").Id("extractResult").Dot("RawDelta"),
			// Raw-only / reset-only path. Triggered when:
			//   - No parseable content has accumulated yet (e.g., the only
			//     events seen so far are thinking_delta under opt-in).
			//   - No new parseable content this tick (e.g., a thinking-only
			//     event under opt-in advanced raw but not parseable).
			//   - A reset boundary occurred but parseable is empty.
			//
			// In all three cases we cannot meaningfully call ParseStream, but
			// we still emit a partial so the wire's raw buffer accumulates and
			// any reset boundary reaches the client.
			jen.If(
				jen.Id("parseable").Op("==").Lit("").
					Op("||").Id("parseableDelta").Op("==").Lit(""),
			).Block(
				jen.Select().Block(
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
					jen.Default().Block(),
				),
				jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
				jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
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
				Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
				jen.Id("adapter"), jen.Id("parseable"), jen.Id("options").Op("..."),
			),
			jen.If(jen.Id("parseErr").Op("==").Nil()).Block(
				jen.Select().Block(
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Return(jen.Nil())),
					jen.Default().Block(),
				),
				jen.Id("parsedPtr").Op(":=").Op("&").Id("parsed"),
				func() jen.Code {
					if isDynamicStream {
						return jen.Id(unwrapStreamFuncName).Call(jen.Id("parsedPtr"))
					}
					return jen.Null()
				}(),
				jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
				jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
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
				jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
				jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindStream"),
				jen.Id("__r").Dot("raw").Op("=").Id("rawDelta"),
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
		for _, arg := range args {
			driveStreamCallParams = append(driveStreamCallParams, argCallParam(arg))
		}
		driveStreamCallParams = append(driveStreamCallParams, jen.Id("driveOpts").Op("..."))

		// Build the driveStream closure: creates the BAML stream, iterates, returns (finalResult, lastError).
		driveStreamBody := []jen.Code{
			jen.Id("driveOpts").Op(":=").Id("opts"),
			withClientOverrideBlock(
				jen.Id("driveOpts").Op("=").Append(
					jen.Qual("slices", "Clone").Call(jen.Id("opts")),
					jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
				),
			),
			jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
				Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(driveStreamCallParams...),
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
			jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
			jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindFinal"),
			jen.Id("__r").Dot("raw").Op("=").Id("raw"),
			jen.If(jen.Id("result").Op("!=").Nil()).Block(
				jen.If(
					jen.List(jen.Id("ptr"), jen.Id("ok")).Op(":=").Id("result").Assert(finalTypePtr.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if isDynamicFinal {
							return jen.Id(unwrapFinalFuncName).Call(jen.Id("ptr"))
						}
						return jen.Null()
					}(),
					jen.Id("__r").Dot("finalParsed").Op("=").Id("ptr"),
				).Else().If(
					jen.List(jen.Id("val"), jen.Id("ok")).Op(":=").Id("result").Assert(finalType.statement.Clone()),
					jen.Id("ok"),
				).Block(
					func() jen.Code {
						if isDynamicFinal {
							return jen.Id(unwrapFinalFuncName).Call(jen.Op("&").Id("val"))
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
		// Same legacy-view rationale as _noRaw — see PR #192
		// cold-review-4 + Option C.
		var fullBody []jen.Code
		fullBody = append(fullBody, makeLegacyPreamble()...)

		// Delegate to shared full orchestration helper with per-method closures
		fullBody = append(fullBody,
			jen.Return(jen.Id("runFullOrchestration").Call(
				jen.Id("adapter"),
				jen.Id("out"),
				jen.Id("options"),
				// newHeartbeat
				jen.Func().Params().Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Id("__r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("__r").Dot("kind").Op("=").Qual(common.InterfacesPkg, "StreamResultKindHeartbeat"),
					jen.Return(jen.Id("__r")),
				),
				// newError
				jen.Func().Params(jen.Id("err").Error()).Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Return(jen.Id(errorConstructorName).Call(jen.Id("err"))),
				),
				// release
				jen.Func().Params(jen.Id("__r").Qual(common.InterfacesPkg, "StreamResult")).Block(
					jen.Id("__r").Dot("Release").Call(),
				),
				// plannedMetadata: passed straight through; orchestrator
				// builds both planned and outcome events from this seed.
				jen.Id("plannedMetadata"),
				// newMetadataResult: pool-wraps a Metadata payload.
				jen.Func().Params(jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata")).Qual(common.InterfacesPkg, "StreamResult").Block(
					jen.Return(jen.Id(metadataConstructorName).Call(jen.Id("md"))),
				),
				// processTick
				jen.Func().Params(
					jen.Id("funcLog").Qual(BamlPkg, "FunctionLog"),
					jen.Id("extractor").Op("*").Qual(common.SSEPkg, "IncrementalExtractor"),
					jen.Id("extractorMu").Op("*").Qual("sync", "Mutex"),
				).Error().Block(processTickBody...),
				// driveStream
				jen.Func().Params(
					jen.Id("opts").Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
				).Params(jen.Any(), jen.Error()).Block(driveStreamBody...),
				// emitFinal
				jen.Func().Params(jen.Id("result").Any(), jen.Id("raw").String()).Qual(common.InterfacesPkg, "StreamResult").Block(emitFinalBody...),
			)),
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
				jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
				jen.Id("clientOverride").String(),
			).
			Error().
			Block(fullBody...)

		// ====== BUILD REQUEST PATH: methodName_buildRequest ======
		// This path uses BAML's BuildRequest/StreamRequest API to get the raw HTTP
		// request, executes it ourselves, and parses SSE events directly.
		// Only emitted when BAML >= 0.219.0 exposes the StreamRequest singleton —
		// on older versions the symbol baml_client.StreamRequest doesn't exist,
		// so we must not generate code that references it.
		hasBuildRequest := introspected.StreamRequest != nil
		buildRequestMethodName := strcase.LowerCamelCase(methodName + "_buildRequest")

		if hasBuildRequest {

			// Build the StreamRequest call params using callOpts (which may
			// include a WithClient override for fallback chains).
			var buildRequestCallParams []jen.Code
			buildRequestCallParams = append(buildRequestCallParams, jen.Id("ctx"))
			for _, arg := range args {
				buildRequestCallParams = append(buildRequestCallParams, argCallParam(arg))
			}
			buildRequestCallParams = append(buildRequestCallParams, jen.Id("callOpts").Op("..."))

			// Call params for the legacy-child Stream.<Method> invocation.
			// First arg is adapter (its embedded context.Context satisfies
			// the BAML generated Stream.Method signature); the rest is
			// method args + opts. Mirrors driveStreamBody at codegen.go:1456.
			var legacyStreamCallParams []jen.Code
			legacyStreamCallParams = append(legacyStreamCallParams, jen.Id("adapter"))
			for _, arg := range args {
				legacyStreamCallParams = append(legacyStreamCallParams, argCallParam(arg))
			}
			legacyStreamCallParams = append(legacyStreamCallParams, jen.Id("opts").Op("..."))

			// The _buildRequest body
			buildRequestBody := makePreamble()

			// Build the closures for RunStreamOrchestration
			buildRequestBody = append(buildRequestBody,
				// buildRequestFn: calls StreamRequest.Method(ctx, args, opts...) -> baml.HTTPRequest -> llmhttp.Request
				// Accepts clientOverride to support fallback chain iteration via WithClient.
				jen.Id("buildRequestFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("clientOverride").String(),
				).Params(
					jen.Op("*").Qual(common.LLMHTTPPkg, "Request"),
					jen.Error(),
				).BlockFunc(func(g *jen.Group) {
					// Build callOpts: clone options and append WithClient if override is set
					g.Id("callOpts").Op(":=").Id("options")
					if supportsWithClient {
						g.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(
							jen.Id("callOpts").Op("=").Append(
								jen.Qual("slices", "Clone").Call(jen.Id("options")),
								jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
							),
						)
					}
					// Call StreamRequest.Method(ctx, args..., callOpts...)
					g.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
						Qual(common.GeneratedClientPkg, "StreamRequest").Dot(methodName).Call(buildRequestCallParams...)
					g.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					)
					emitBAMLHTTPRequestConversion(g)
				}),

				// parseStreamFn: calls ParseStream.Method(ctx, accumulated, opts...)
				// The context_fix hack adds ctx as first param to ParseStream methods.
				jen.Id("parseStreamFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("accumulated").String(),
				).Params(jen.Any(), jen.Error()).Block(
					jen.Return(
						jen.Qual(common.GeneratedClientPkg, "ParseStream").Dot(methodName).Call(
							jen.Id("ctx"),
							jen.Id("accumulated"),
							jen.Id("options").Op("..."),
						),
					),
				),

				// parseFinalFn: calls Parse.Method(ctx, accumulated, opts...)
				// The context_fix hack adds ctx as first param to Parse methods.
				jen.Id("parseFinalFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("accumulated").String(),
				).Params(jen.Any(), jen.Error()).Block(
					jen.Return(
						jen.Qual(common.GeneratedClientPkg, "Parse").Dot(methodName).Call(
							jen.Id("ctx"),
							jen.Id("accumulated"),
							jen.Id("options").Op("..."),
						),
					),
				),

				// newResultFn: creates a pooled StreamResult.
				// ParseStream/Parse may return either a pointer (*T) or a value (T),
				// so we try pointer assertion first, then value assertion with &v.
				// This matches the legacy paths which handle both shapes.
				// Dynamic unwrap helpers are called when applicable, matching the
				// legacy _full path's behavior for DynamicProperties outputs.
				jen.Id("newResultFn").Op(":=").Func().Params(
					jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
					jen.Id("stream").Any(),
					jen.Id("final").Any(),
					jen.Id("raw").String(),
					jen.Id("err").Error(),
					jen.Id("reset").Bool(),
				).Params(jen.Qual(common.InterfacesPkg, "StreamResult")).Block(
					jen.Id("r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("r").Dot("kind").Op("=").Id("kind"),
					jen.Id("r").Dot("raw").Op("=").Id("raw"),
					jen.Id("r").Dot("err").Op("=").Id("err"),
					jen.Id("r").Dot("reset").Op("=").Id("reset"),
					// Set stream field: try *T first (pointer return), then T (value return → take address)
					jen.If(jen.Id("stream").Op("!=").Nil()).Block(
						jen.If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("stream").Assert(streamTypePtr.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicStream {
									return jen.Id(unwrapStreamFuncName).Call(jen.Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("streamParsed").Op("=").Id("v"),
						).Else().If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("stream").Assert(streamType.statement.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicStream {
									return jen.Id(unwrapStreamFuncName).Call(jen.Op("&").Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("streamParsed").Op("=").Op("&").Id("v"),
						),
					),
					// Set final field: try *T first, then T
					jen.If(jen.Id("final").Op("!=").Nil()).Block(
						jen.If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(finalTypePtr.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicFinal {
									return jen.Id(unwrapFinalFuncName).Call(jen.Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("finalParsed").Op("=").Id("v"),
						).Else().If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(finalType.statement.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicFinal {
									return jen.Id(unwrapFinalFuncName).Call(jen.Op("&").Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("finalParsed").Op("=").Op("&").Id("v"),
						),
					),
					jen.Return(jen.Id("r")),
				),

				// legacyStreamChildFn runs one mixed-mode legacy child via
				// BAML's Stream API. Delegates the lifecycle (heartbeat
				// wiring + FunctionLog capture for raw) to the shared
				// runLegacyChildStream helper; the inner closure supplies
				// only the per-method Stream.<Method> invocation.
				jen.Id("legacyStreamChildFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("clientOverride").String(),
					jen.Id("_").String(),
					jen.Id("needsRaw").Bool(),
					jen.Id("sendHeartbeat").Func().Params(),
				).Params(jen.Any(), jen.String(), jen.Error()).Block(
					jen.Id("callOpts").Op(":=").Id("options"),
					withClientOverrideBlock(
						jen.Id("callOpts").Op("=").Append(
							jen.Qual("slices", "Clone").Call(jen.Id("options")),
							jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
						),
					),
					jen.Return(jen.Id("runLegacyChildStream").Call(
						jen.Id("ctx"),
						jen.Id("needsRaw"),
						jen.Id("sendHeartbeat"),
						jen.Func().Params(jen.Id("onTick").Add(onTickType())).Params(jen.Any(), jen.Error()).Block(
							jen.Id("opts").Op(":=").Append(
								jen.Id("callOpts"),
								jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
							),
							jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
								Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(legacyStreamCallParams...),
							jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
								jen.Return(jen.Nil(), jen.Id("streamErr")),
							),
							jen.Var().Id("result").Any(),
							jen.Var().Id("lastErr").Error(),
							jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
								jen.If(jen.Id("streamVal").Dot("IsError")).Block(
									jen.Id("lastErr").Op("=").Id("streamVal").Dot("Error"),
									jen.Continue(),
								),
								jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
									jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
								),
							),
							jen.Return(jen.Id("result"), jen.Id("lastErr")),
						),
					)),
				),

				// StreamConfig. LegacyChildren is populated for mixed chains;
				// LegacyStreamChild is always wired so the orchestrator's
				// up-front validation passes even when legacyChildren is nil.
				// MetadataPlan / NewMetadataResult carry the per-request
				// routing metadata through to the orchestrator's planned +
				// outcome emissions.
				jen.Id("streamConfig").Op(":=").Op("&").Qual(common.BuildRequestPkg, "StreamConfig").Values(jen.Dict{
					jen.Id("Provider"):             jen.Id("provider"),
					jen.Id("RetryPolicy"):          jen.Id("retryPolicy"),
					jen.Id("NeedsPartials"):        jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsPartials").Call(),
					jen.Id("NeedsRaw"):             jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsRaw").Call(),
					jen.Id("IncludeThinkingInRaw"): jen.Id("adapter").Dot("IncludeThinkingInRaw").Call(),
					jen.Id("FallbackChain"):        jen.Id("fallbackChain"),
					jen.Id("ClientOverride"):       jen.Id("clientOverride"),
					jen.Id("ClientProviders"):      jen.Id("clientProviders"),
					jen.Id("LegacyChildren"):       jen.Id("legacyChildren"),
					jen.Id("LegacyStreamChild"):    jen.Id("legacyStreamChildFn"),
					jen.Id("MetadataPlan"):         jen.Id("plannedMetadata"),
					jen.Id("NewMetadataResult"): jen.Func().Params(
						jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata"),
					).Qual(common.InterfacesPkg, "StreamResult").Block(
						jen.Return(jen.Id(metadataConstructorName).Call(jen.Id("md"))),
					),
				}),

				// Run the orchestration in a goroutine with panic recovery, matching
				// the _noRaw/_full pattern. On panic, an error result is emitted so
				// the worker can surface it instead of crashing.
				// Resolve HTTP client: use adapter override if provided, else default
				jen.Id("__httpClient").Op(":=").Qual(common.LLMHTTPPkg, "DefaultClient"),
				jen.If(
					jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
					jen.Id("__c").Op("!=").Nil(),
				).Block(
					jen.Id("__httpClient").Op("=").Id("__c"),
				),

				jen.Go().Func().Params().Block(
					jen.Defer().Close(jen.Id("out")),
					jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
						jen.Func().Params(jen.Id("err").Error()).Block(
							jen.Id("__errR").Op(":=").Id("newResultFn").Call(
								jen.Qual(common.InterfacesPkg, "StreamResultKindError"),
								jen.Nil(), jen.Nil(), jen.Lit(""), jen.Id("err"), jen.False(),
							),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
									jen.Id("__errR").Dot("Release").Call(),
								),
							),
						),
						jen.Func().Params().Error().Block(
							jen.Return(jen.Qual(common.BuildRequestPkg, "RunStreamOrchestration").Call(
								jen.Id("adapter"),
								jen.Id("out"),
								jen.Id("streamConfig"),
								jen.Id("__httpClient"),
								jen.Id("buildRequestFn"),
								jen.Id("parseStreamFn"),
								jen.Id("parseFinalFn"),
								jen.Id("newResultFn"),
							)),
						),
					),
				).Call(),
				jen.Return(jen.Nil()),
			)

			out.Func().
				Id(buildRequestMethodName).
				Params(
					jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
					jen.Id("rawInput").Any(),
					jen.Id("out").Chan().Add(streamResultInterface.Clone()),
					jen.Id("provider").String(),
					jen.Id("retryPolicy").Op("*").Qual(common.RetryPkg, "Policy"),
					jen.Id("fallbackChain").Index().String(),
					jen.Id("clientProviders").Map(jen.String()).String(),
					jen.Id("legacyChildren").Map(jen.String()).Bool(),
					jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
					jen.Id("clientOverride").String(),
				).
				Error().
				Block(buildRequestBody...)

		} // end if hasBuildRequest

		// ====== BUILD CALL REQUEST PATH: methodName_buildCallRequest ======
		// Non-streaming counterpart to _buildRequest. Uses BAML's Request API
		// (not StreamRequest) to build an HTTP request with "stream": false,
		// executes it, extracts the LLM text from the JSON response, and parses.
		// Only emitted when BAML >= 0.219.0 exposes the Request singleton.
		hasCallBuildRequest := introspected.Request != nil
		buildCallRequestMethodName := strcase.LowerCamelCase(methodName + "_buildCallRequest")

		if hasCallBuildRequest {

			// Build the Request call params using callOpts (which may
			// include a WithClient override for fallback chains).
			var callRequestCallParams []jen.Code
			callRequestCallParams = append(callRequestCallParams, jen.Id("ctx"))
			for _, arg := range args {
				callRequestCallParams = append(callRequestCallParams, argCallParam(arg))
			}
			callRequestCallParams = append(callRequestCallParams, jen.Id("callOpts").Op("..."))

			// Stream.<Method> params for the legacy call-mode child. BAML
			// exposes streaming as the primitive even for non-streaming use,
			// so legacy call children reuse runLegacyChildStream just like
			// streaming children.
			var legacyCallStreamCallParams []jen.Code
			legacyCallStreamCallParams = append(legacyCallStreamCallParams, jen.Id("adapter"))
			for _, arg := range args {
				legacyCallStreamCallParams = append(legacyCallStreamCallParams, argCallParam(arg))
			}
			legacyCallStreamCallParams = append(legacyCallStreamCallParams, jen.Id("opts").Op("..."))

			buildCallRequestBody := makePreamble()

			buildCallRequestBody = append(buildCallRequestBody,
				// buildRequestFn: calls Request.Method(ctx, args, opts...) → baml.HTTPRequest → llmhttp.Request
				// Accepts clientOverride to support fallback chain iteration via WithClient.
				jen.Id("buildRequestFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("clientOverride").String(),
				).Params(
					jen.Op("*").Qual(common.LLMHTTPPkg, "Request"),
					jen.Error(),
				).BlockFunc(func(g *jen.Group) {
					// Build callOpts: clone options and append WithClient if override is set
					g.Id("callOpts").Op(":=").Id("options")
					if supportsWithClient {
						g.If(jen.Id("clientOverride").Op("!=").Lit("")).Block(
							jen.Id("callOpts").Op("=").Append(
								jen.Qual("slices", "Clone").Call(jen.Id("options")),
								jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
							),
						)
					}
					// Call Request.Method(ctx, args..., callOpts...) — non-streaming
					g.List(jen.Id("httpReq"), jen.Id("err")).Op(":=").
						Qual(common.GeneratedClientPkg, "Request").Dot(methodName).Call(callRequestCallParams...)
					g.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Id("err")),
					)
					emitBAMLHTTPRequestConversion(g)
				}),

				// parseFinalFn: calls Parse.Method(ctx, text, opts...)
				jen.Id("parseFinalFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("text").String(),
				).Params(jen.Any(), jen.Error()).Block(
					jen.Return(
						jen.Qual(common.GeneratedClientPkg, "Parse").Dot(methodName).Call(
							jen.Id("ctx"),
							jen.Id("text"),
							jen.Id("options").Op("..."),
						),
					),
				),

				// newResultFn: creates a pooled StreamResult. The signature matches
				// the streaming _buildRequest's newResultFn (including the unused
				// "stream" parameter) so both paths satisfy the same NewResultFunc
				// type. The non-streaming call path never passes stream data.
				jen.Id("newResultFn").Op(":=").Func().Params(
					jen.Id("kind").Qual(common.InterfacesPkg, "StreamResultKind"),
					jen.Id("stream").Any(), // unused in non-streaming path; kept for signature parity
					jen.Id("final").Any(),
					jen.Id("raw").String(),
					jen.Id("err").Error(),
					jen.Id("reset").Bool(),
				).Params(jen.Qual(common.InterfacesPkg, "StreamResult")).Block(
					jen.Id("r").Op(":=").Id(getterFuncName).Call(),
					jen.Id("r").Dot("kind").Op("=").Id("kind"),
					jen.Id("r").Dot("raw").Op("=").Id("raw"),
					jen.Id("r").Dot("err").Op("=").Id("err"),
					jen.Id("r").Dot("reset").Op("=").Id("reset"),
					// Set final field: try *T first (pointer return), then T (value return → take address)
					jen.If(jen.Id("final").Op("!=").Nil()).Block(
						jen.If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(finalTypePtr.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicFinal {
									return jen.Id(unwrapFinalFuncName).Call(jen.Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("finalParsed").Op("=").Id("v"),
						).Else().If(
							jen.List(jen.Id("v"), jen.Id("ok")).Op(":=").Id("final").Assert(finalType.statement.Clone()),
							jen.Id("ok"),
						).Block(
							func() jen.Code {
								if isDynamicFinal {
									return jen.Id(unwrapFinalFuncName).Call(jen.Op("&").Id("v"))
								}
								return jen.Null()
							}(),
							jen.Id("r").Dot("finalParsed").Op("=").Op("&").Id("v"),
						),
					),
					jen.Return(jen.Id("r")),
				),

				// legacyCallChildFn runs one mixed-mode legacy child for the
				// non-streaming path. Structurally identical to
				// legacyStreamChildFn in _buildRequest — see that closure
				// for the rationale.
				jen.Id("legacyCallChildFn").Op(":=").Func().Params(
					jen.Id("ctx").Qual("context", "Context"),
					jen.Id("clientOverride").String(),
					jen.Id("_").String(),
					jen.Id("needsRaw").Bool(),
					jen.Id("sendHeartbeat").Func().Params(),
				).Params(jen.Any(), jen.String(), jen.Error()).Block(
					jen.Id("callOpts").Op(":=").Id("options"),
					withClientOverrideBlock(
						jen.Id("callOpts").Op("=").Append(
							jen.Qual("slices", "Clone").Call(jen.Id("options")),
							jen.Qual(common.GeneratedClientPkg, "WithClient").Call(jen.Id("clientOverride")),
						),
					),
					jen.Return(jen.Id("runLegacyChildStream").Call(
						jen.Id("ctx"),
						jen.Id("needsRaw"),
						jen.Id("sendHeartbeat"),
						jen.Func().Params(jen.Id("onTick").Add(onTickType())).Params(jen.Any(), jen.Error()).Block(
							jen.Id("opts").Op(":=").Append(
								jen.Id("callOpts"),
								jen.Qual(common.GeneratedClientPkg, "WithOnTick").Call(jen.Id("onTick")),
							),
							jen.List(jen.Id("stream"), jen.Id("streamErr")).Op(":=").
								Qual(common.GeneratedClientPkg, "Stream").Dot(methodName).Call(legacyCallStreamCallParams...),
							jen.If(jen.Id("streamErr").Op("!=").Nil()).Block(
								jen.Return(jen.Nil(), jen.Id("streamErr")),
							),
							jen.Var().Id("result").Any(),
							jen.Var().Id("lastErr").Error(),
							jen.For(jen.Id("streamVal").Op(":=").Range().Id("stream")).Block(
								jen.If(jen.Id("streamVal").Dot("IsError")).Block(
									jen.Id("lastErr").Op("=").Id("streamVal").Dot("Error"),
									jen.Continue(),
								),
								jen.If(jen.Id("streamVal").Dot("IsFinal")).Block(
									jen.Id("result").Op("=").Id("streamVal").Dot("Final").Call(),
								),
							),
							jen.Return(jen.Id("result"), jen.Id("lastErr")),
						),
					)),
				),

				// CallConfig. LegacyChildren is populated for mixed chains;
				// LegacyCallChild is always wired so validation passes even
				// when legacyChildren is nil.
				jen.Id("callConfig").Op(":=").Op("&").Qual(common.BuildRequestPkg, "CallConfig").Values(jen.Dict{
					jen.Id("Provider"):             jen.Id("provider"),
					jen.Id("RetryPolicy"):          jen.Id("retryPolicy"),
					jen.Id("NeedsRaw"):             jen.Id("adapter").Dot("StreamMode").Call().Dot("NeedsRaw").Call(),
					jen.Id("IncludeThinkingInRaw"): jen.Id("adapter").Dot("IncludeThinkingInRaw").Call(),
					jen.Id("FallbackChain"):        jen.Id("fallbackChain"),
					jen.Id("ClientOverride"):       jen.Id("clientOverride"),
					jen.Id("ClientProviders"):      jen.Id("clientProviders"),
					jen.Id("LegacyChildren"):       jen.Id("legacyChildren"),
					jen.Id("LegacyCallChild"):      jen.Id("legacyCallChildFn"),
					jen.Id("MetadataPlan"):         jen.Id("plannedMetadata"),
					jen.Id("NewMetadataResult"): jen.Func().Params(
						jen.Id("md").Op("*").Qual(common.InterfacesPkg, "Metadata"),
					).Qual(common.InterfacesPkg, "StreamResult").Block(
						jen.Return(jen.Id(metadataConstructorName).Call(jen.Id("md"))),
					),
				}),

				// Resolve HTTP client
				jen.Id("__httpClient").Op(":=").Qual(common.LLMHTTPPkg, "DefaultClient"),
				jen.If(
					jen.Id("__c").Op(":=").Id("adapter").Dot("HTTPClient").Call(),
					jen.Id("__c").Op("!=").Nil(),
				).Block(
					jen.Id("__httpClient").Op("=").Id("__c"),
				),

				// Run in goroutine with panic recovery
				jen.Go().Func().Params().Block(
					jen.Defer().Close(jen.Id("out")),
					jen.Qual("github.com/gregwebs/go-recovery", "GoHandler").Call(
						jen.Func().Params(jen.Id("err").Error()).Block(
							jen.Id("__errR").Op(":=").Id("newResultFn").Call(
								jen.Qual(common.InterfacesPkg, "StreamResultKindError"),
								jen.Nil(), jen.Nil(), jen.Lit(""), jen.Id("err"), jen.False(),
							),
							jen.Select().Block(
								jen.Case(jen.Id("out").Op("<-").Id("__errR")).Block(),
								jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(
									jen.Id("__errR").Dot("Release").Call(),
								),
							),
						),
						jen.Func().Params().Error().Block(
							jen.Return(jen.Qual(common.BuildRequestPkg, "RunCallOrchestration").Call(
								jen.Id("adapter"),
								jen.Id("out"),
								jen.Id("callConfig"),
								jen.Id("__httpClient"),
								jen.Id("buildRequestFn"),
								jen.Id("parseFinalFn"),
								jen.Qual(common.BuildRequestPkg, "ExtractResponseContent"),
								jen.Id("newResultFn"),
							)),
						),
					),
				).Call(),
				jen.Return(jen.Nil()),
			)

			out.Func().
				Id(buildCallRequestMethodName).
				Params(
					jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
					jen.Id("rawInput").Any(),
					jen.Id("out").Chan().Add(streamResultInterface.Clone()),
					jen.Id("provider").String(),
					jen.Id("retryPolicy").Op("*").Qual(common.RetryPkg, "Policy"),
					jen.Id("fallbackChain").Index().String(),
					jen.Id("clientProviders").Map(jen.String()).String(),
					jen.Id("legacyChildren").Map(jen.String()).Bool(),
					jen.Id("plannedMetadata").Op("*").Qual(common.InterfacesPkg, "Metadata"),
					jen.Id("clientOverride").String(),
				).
				Error().
				Block(buildCallRequestBody...)

		} // end if hasCallBuildRequest

		// Generate the public router function that dispatches based on StreamMode()
		// Creates the output channel and passes it to the inner implementation.
		//
		// When the BuildRequest path is available and the feature flag is set,
		// it takes priority for both call and stream modes. Falls back to the
		// legacy paths for unsupported providers or when the feature flag is off.
		routerBody := []jen.Code{
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone()), jen.Lit(100)),
			jen.Var().Id("err").Error(),
			jen.Id("mode").Op(":=").Id("adapter").Dot("StreamMode").Call(),
		}
		if supportsWithClient {
			// Full RR resolution: apply the runtime primary override,
			// unwrap baml-roundrobin wrappers, and advance the coordinator
			// (or the worker-installed RemoteAdvancer) for the leaf
			// selection. __effective is the resolved leaf; __rrInfo
			// describes the outermost RR decision (nil when no RR unwrap
			// happened). Requires WithClient so the legacy dispatcher can
			// pass the chosen leaf to BAML's runtime — on older adapters
			// that lack WithClient we skip this path entirely and let
			// BAML's own strategy rotation handle RR.
			routerBody = append(routerBody,
				jen.List(jen.Id("__effective"), jen.Id("__rrInfo"), jen.Id("__rrErr")).Op(":=").
					Qual(common.BuildRequestPkg, "ResolveEffectiveClient").Call(
					jen.Id("adapter"),
					jen.Qual(common.IntrospectedPkg, "FunctionClient").Index(jen.Lit(methodName)),
					jen.Qual(common.IntrospectedPkg, "FallbackChains"),
					jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					jen.Qual(common.IntrospectedPkg, "RoundRobinCoordinator"),
				),
				jen.If(jen.Id("__rrErr").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("__rrErr")),
				),
			)
		} else {
			// Legacy-only adapters: skip RR unwrap + coordinator advance.
			// The counter slot would otherwise be wasted on a decision
			// the legacy dispatcher can't honor (no WithClient means we
			// can't pass the chosen leaf to BAML), and the rrInfo would
			// be misleading — it would report OUR selection while BAML's
			// runtime picks a different child via its own rotation. Use
			// the primary-override-only helper and leave __rrInfo nil so
			// metadata stays silent about RR, letting BAML's internal
			// routing speak for itself via outcome metadata.
			routerBody = append(routerBody,
				jen.Id("__effective").Op(":=").Qual(common.BuildRequestPkg, "ResolvePrimaryClient").Call(
					jen.Id("adapter"),
					jen.Qual(common.IntrospectedPkg, "FunctionClient").Index(jen.Lit(methodName)),
				),
				jen.Var().Id("__rrInfo").Op("*").Qual(common.InterfacesPkg, "RoundRobinInfo"),
			)
		}
		routerBody = append(routerBody,
			jen.Id("__reg").Op(":=").Id("adapter").Dot("OriginalClientRegistry").Call(),
		)

		// Helper to generate the common retry policy resolution + dispatch call
		// for both single-provider and fallback-chain paths.
		//
		// Keyed on __effective (the post-RR leaf) rather than
		// FunctionClient[methodName] (the function's declared default
		// client, which may be an RR wrapper). For non-RR functions the
		// two are identical; for RR, we must use the child that will
		// actually handle the request, otherwise the retry policy would
		// come from the wrapper — retry config isn't inherited from
		// strategy wrappers in BAML semantics.
		resolveRetryPolicy := func() jen.Code {
			return jen.Id("retryPolicy").Op(":=").Qual(common.BuildRequestPkg, "ResolveRetryPolicy").Call(
				jen.Id("adapter"),
				jen.Id("__effective"),
				jen.Qual(common.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
				jen.Qual(common.IntrospectedPkg, "RetryPolicies"),
			)
		}

		// fallbackChainConsumeCond builds the condition guarding the call-side
		// fallback block. When a StreamRequest bridge exists downstream, the
		// call block only consumes chains with no call-legacy children; mixed
		// chains must fall through so the bridge can re-resolve them with
		// IsProviderSupported and drive call-legacy-but-stream-supported
		// children through the streaming path rather than legacyCallChildFn.
		// Without a bridge, the call block is the only BuildRequest landing
		// spot and accepts any non-empty chain, matching pre-bridge behaviour.
		fallbackChainConsumeCond := func(hasBridge bool) jen.Code {
			nonEmpty := jen.Len(jen.Id("__chain")).Op(">").Lit(0)
			if !hasBridge {
				return nonEmpty
			}
			return nonEmpty.Op("&&").Len(jen.Id("__legacyChildren")).Op("==").Lit(0)
		}

		// Non-streaming BuildRequest path for /call and /call-with-raw.
		// Uses Request (not StreamRequest) to build non-streaming HTTP requests.
		// Checked before the streaming path since it's more efficient for call modes.
		if hasCallBuildRequest {
			routerBody = append(routerBody,
				jen.Comment("Try non-streaming BuildRequest path for /call and /call-with-raw"),
				jen.If(
					jen.Qual(common.BuildRequestPkg, "UseBuildRequest").Call().
						Op("&&").Qual(common.IntrospectedPkg, "Request").Op("!=").Nil().
						Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
						Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw")),
				).Block(
					// Single-provider path — keyed off the effective (post-RR)
					// client. ResolveClientProvider consults the runtime
					// registry for a per-client provider override before
					// falling back to the introspected default.
					jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					),
					jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsCallProviderSupported").Call(jen.Id("provider"))).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("provider"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIRequest"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildCallRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Id("provider"), jen.Id("retryPolicy"),
							jen.Nil(), jen.Nil(),
							jen.Nil(),
							jen.Id("__planned"),
							jen.Id("__effective"),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
					// Fallback chain path: if single-provider check failed,
					// try resolving a fallback chain off the effective client.
					// When a bridge block exists (hasBuildRequest), a mixed
					// chain — one with any call-legacy children — must fall
					// through so the bridge re-resolves it with
					// IsProviderSupported; a child that is call-legacy may
					// still be stream-supported, and the bridge's
					// StreamRequest path drives such children better than
					// legacyCallChildFn. This block therefore only takes
					// chains that are fully call-supported. When no bridge
					// exists (hasBuildRequest=false) mixed chains stay here,
					// matching the pre-bridge behaviour.
					jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "FallbackChains"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
						jen.Qual(common.BuildRequestPkg, "IsCallProviderSupported"),
					),
					jen.If(fallbackChainConsumeCond(hasBuildRequest)).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("__chain"),
							jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIRequest"),
							jen.Id("__fbReason"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildCallRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Lit(""), jen.Id("retryPolicy"),
							jen.Id("__chain"), jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("__planned"),
							jen.Lit(""),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
				),
			)
		}

		// Streaming BuildRequest path for /stream and /stream-with-raw.
		// Explicitly gated to streaming modes; a separate bridge block below
		// handles /call and /call-with-raw via stream accumulation when the
		// non-streaming Request API declined.
		if hasBuildRequest {
			routerBody = append(routerBody,
				jen.Comment("Try streaming BuildRequest path for /stream and /stream-with-raw"),
				jen.If(
					jen.Qual(common.BuildRequestPkg, "UseBuildRequest").Call().
						Op("&&").Qual(common.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
						Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeStream").
						Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeStreamWithRaw")),
				).Block(
					// Single-provider path keyed off the effective client.
					jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					),
					jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("provider"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Id("provider"), jen.Id("retryPolicy"),
							jen.Nil(), jen.Nil(),
							jen.Nil(),
							jen.Id("__planned"),
							jen.Id("__effective"),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
					// Fallback chain path. Mixed chains (with any legacy
					// children) route through the BuildRequest path — the
					// orchestrator dispatches legacy children to the
					// generated legacyStreamChildFn.
					jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "FallbackChains"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
						jen.Qual(common.BuildRequestPkg, "IsProviderSupported"),
					),
					jen.If(jen.Len(jen.Id("__chain")).Op(">").Lit(0)).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("__chain"),
							jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
							jen.Id("__fbReason"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Lit(""), jen.Id("retryPolicy"),
							jen.Id("__chain"), jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("__planned"),
							jen.Lit(""),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
				),
			)
		}

		// Bridge: /call and /call-with-raw that the non-streaming block
		// declined fall through to the streaming BuildRequest path, which
		// accumulates SSE deltas into a unary response. Triggered when the
		// non-streaming Request API is unavailable (introspected.Request==nil
		// or the call-side support gate rejected the provider/chain) but the
		// StreamRequest API can drive it. StreamMode is StreamModeCall or
		// StreamModeCallWithRaw, so NeedsPartials is false inside the
		// orchestrator and no partials ever reach the output channel — the
		// pool sees the same shape as the non-streaming call path.
		if hasBuildRequest {
			routerBody = append(routerBody,
				jen.Comment("Bridge: /call and /call-with-raw via StreamRequest when Request is unavailable"),
				jen.If(
					jen.Qual(common.BuildRequestPkg, "UseBuildRequest").Call().
						Op("&&").Qual(common.IntrospectedPkg, "StreamRequest").Op("!=").Nil().
						Op("&&").Parens(jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
						Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw")),
				).Block(
					// Single-provider path
					jen.Id("provider").Op(":=").Qual(common.BuildRequestPkg, "ResolveClientProvider").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
					),
					jen.If(jen.Id("provider").Op("!=").Lit("").Op("&&").Qual(common.BuildRequestPkg, "IsProviderSupported").Call(jen.Id("provider"))).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildSingleProviderPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("provider"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Id("provider"), jen.Id("retryPolicy"),
							jen.Nil(), jen.Nil(),
							jen.Nil(),
							jen.Id("__planned"),
							jen.Id("__effective"),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
					// Fallback chain path (bridge). Uses IsProviderSupported
					// (stream side) because the whole point of the bridge is
					// to accept chains that IsCallProviderSupported rejected.
					jen.List(jen.Id("__chain"), jen.Id("__cprov"), jen.Id("__legacyChildren"), jen.Id("__fbReason")).Op(":=").Qual(common.BuildRequestPkg, "ResolveFallbackChainForClientWithReason").Call(
						jen.Id("__reg"),
						jen.Id("__effective"),
						jen.Qual(common.IntrospectedPkg, "FallbackChains"),
						jen.Qual(common.IntrospectedPkg, "ClientProvider"),
						jen.Qual(common.BuildRequestPkg, "IsProviderSupported"),
					),
					jen.If(jen.Len(jen.Id("__chain")).Op(">").Lit(0)).Block(
						resolveRetryPolicy(),
						jen.Id("__planned").Op(":=").Qual(common.BuildRequestPkg, "BuildFallbackChainPlanForClient").Call(
							jen.Id("__effective"),
							jen.Id("__chain"),
							jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("retryPolicy"),
							jen.Qual(common.BuildRequestPkg, "BuildRequestAPIStreamRequest"),
							jen.Id("__fbReason"),
						),
						jen.Id("__planned").Dot("RoundRobin").Op("=").Id("__rrInfo"),
						jen.Id("err").Op("=").Id(buildRequestMethodName).Call(
							jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"),
							jen.Lit(""), jen.Id("retryPolicy"),
							jen.Id("__chain"), jen.Id("__cprov"),
							jen.Id("__legacyChildren"),
							jen.Id("__planned"),
							jen.Lit(""),
						),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Id("err")),
						),
						jen.Return(jen.Id("out"), jen.Nil()),
					),
				),
			)
		}

		// Compute the planned metadata for the legacy path. BuildLegacyMetadataPlan
		// classifies the request (PathReason) and includes chain details when the
		// client is a fallback strategy. The helper also picks a retry policy so
		// the plan carries RetryMax/RetryPolicy on legacy requests that still
		// honour them (the policy is used by the legacy BAML runtime, not by
		// the generator).
		routerBody = append(routerBody,
			jen.Comment("Legacy path: CallStream + OnTick (for unsupported providers or when BuildRequest is disabled)"),
			// Retry policy and client identity for the legacy dispatch
			// derive from __effective, not FunctionClient[methodName].
			// __effective already accounts for both the RR unwrap and
			// any client_registry primary override, so this keys both
			// the retry policy and the WithClient/plan on the client
			// that will actually handle the request.
			//
			// Without this, a request with `primary` set to a client
			// whose retry_policy is only statically declared (not
			// redeclared in the runtime registry) would fall through to
			// ResolveRetryPolicy's step-4 default — which looked up
			// FunctionRetryPolicy[methodName] and therefore returned the
			// FUNCTION's declared client's policy, not the primary-
			// overridden client's. Same bug, in principle, as CR-11 on
			// the BuildRequest path; the legacy branch was missed.
			jen.Id("__legacyRetryPolicy").Op(":=").Qual(common.BuildRequestPkg, "ResolveRetryPolicy").Call(
				jen.Id("adapter"),
				jen.Id("__effective"),
				jen.Qual(common.IntrospectedPkg, "ClientRetryPolicy").Index(jen.Id("__effective")),
				jen.Qual(common.IntrospectedPkg, "RetryPolicies"),
			),
			// Build the legacy metadata plan keyed on __effective in
			// every case — RR, primary override, or neither. Previously
			// the non-RR branch called BuildLegacyMetadataPlan with
			// FunctionClient[methodName], which re-resolved primary
			// internally but wouldn't propagate the resolved identity
			// back out as __legacyClientOverride, so the subsequent
			// WithClient append (on BAML 0.219+) targeted BAML with an
			// empty override even when primary was set. Passing
			// __effective unconditionally collapses the branching and
			// fixes both paths. __rrInfo is copied in afterwards; it's
			// nil on the non-RR and legacy-only-adapter paths.
			// Mode-aware predicate selection (CodeRabbit verdict-21
			// finding 3): the legacy metadata plan classifies the
			// request's provider against either the stream support
			// table or the call support table. For call modes that
			// reach final legacy *without* a stream bridge having
			// re-resolved them (hasBuildRequest=false) the metadata
			// reason should reflect the call-side classification —
			// otherwise BAML_REST_DISABLE_CALL_BUILD_REQUEST or the
			// debug BAML_REST_CALL_UNSUPPORTED_PROVIDERS flag can
			// produce a too-optimistic PathReason. When a stream
			// bridge exists every call-mode fallthrough has already
			// been gated on IsProviderSupported, so the stream
			// predicate is the correct one. Stream modes always use
			// the stream predicate.
			func() jen.Code {
				if hasBuildRequest {
					return jen.Id("__legacyPredicate").Op(":=").Qual(common.BuildRequestPkg, "IsProviderSupported")
				}
				return jen.Id("__legacyPredicate").Op(":=").Qual(common.BuildRequestPkg, "IsProviderSupported")
			}(),
			func() jen.Code {
				if hasBuildRequest {
					// Stream bridge present — the predicate above is
					// already the right one. Emit a no-op so the
					// generated code stays linear.
					return jen.Null()
				}
				return jen.If(
					jen.Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCall").
						Op("||").Id("mode").Op("==").Qual(common.InterfacesPkg, "StreamModeCallWithRaw"),
				).Block(
					jen.Id("__legacyPredicate").Op("=").Qual(common.BuildRequestPkg, "IsCallProviderSupported"),
				)
			}(),
			jen.Id("__plannedLegacy").Op(":=").Qual(common.BuildRequestPkg, "BuildLegacyMetadataPlanForClient").Call(
				jen.Id("__reg"),
				jen.Id("__effective"),
				jen.Qual(common.IntrospectedPkg, "FunctionProvider").Index(jen.Lit(methodName)),
				jen.Qual(common.IntrospectedPkg, "FallbackChains"),
				jen.Qual(common.IntrospectedPkg, "ClientProvider"),
				jen.Id("__legacyPredicate"),
				jen.Id("__legacyRetryPolicy"),
			),
			jen.Id("__plannedLegacy").Dot("RoundRobin").Op("=").Id("__rrInfo"),
			// __legacyClientOverride is always __effective: on 0.219+
			// the legacy dispatcher uses it to append WithClient so
			// BAML's runtime targets the same leaf the plan reports;
			// on pre-0.219 adapters the helper is accepted as a
			// function parameter but never read (WithClient emission
			// is gated on supportsWithClient), so passing a non-empty
			// value is harmless.
			jen.Id("__legacyClientOverride").Op(":=").Id("__effective"),
			jen.Qual(common.BuildRequestPkg, "LogLegacyClassification").Call(
				jen.Id("adapter"),
				jen.Lit(methodName),
				jen.Id("__plannedLegacy"),
			),
			jen.Switch(jen.Id("mode")).Block(
				// StreamModeCall: final only, no raw, skip partials
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCall")).Block(
					jen.Id("err").Op("=").Id(noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
				),
				// StreamModeStream: partials + final, no raw
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStream")).Block(
					jen.Id("err").Op("=").Id(noRawMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
				),
				// StreamModeCallWithRaw: final + raw, skip intermediate parsing
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeCallWithRaw")).Block(
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.True(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
				),
				// StreamModeStreamWithRaw: partials + final + raw
				jen.Case(jen.Qual(common.InterfacesPkg, "StreamModeStreamWithRaw")).Block(
					jen.Id("err").Op("=").Id(fullMethodName).Call(jen.Id("adapter"), jen.Id("rawInput"), jen.Id("out"), jen.False(), jen.Id("__plannedLegacy"), jen.Id("__legacyClientOverride")),
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

		// Unwrap DynamicProperties at parse time.
		// For streaming methods the helper was already emitted by the streaming loop.
		// For parse-only methods we must emit it here.
		if isDynamic {
			parseUnwrapName := strcase.LowerCamelCase(fmt.Sprintf("unwrapDynamic%sFinal", strcase.UpperCamelCase(fmt.Sprintf("%sOutput", methodName))))
			if !emittedUnwrapHelpers[parseUnwrapName] {
				finalTypeForParse := parseReflectType(syncFuncType.Out(0))
				finalTypePtrForParse := jen.Op("*").Add(finalTypeForParse.statement.Clone())
				emitDynamicUnwrapFunc(parseUnwrapName, finalTypePtrForParse)
				emittedUnwrapHelpers[parseUnwrapName] = true
			}
			parseBody = append(parseBody,
				jen.Id(parseUnwrapName).Call(jen.Op("&").Id("result")),
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
	//
	// IntrospectedClientProvider plumbs the build-time client→provider
	// map into the adapter so SetClientRegistry can materialise
	// providers for omitted-provider runtime registry entries
	// (strategy-only / presence-only RR overrides). Without this seam
	// the adapter would forward `provider: ""` into BAML's CFFI, which
	// rejects in ClientProvider::from_str (clientspec.rs:119-144) and
	// kills the request before WithClient(leaf) resolves anything.
	// See PR #192 cold-review-3 finding 1.
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

	// Generate `makeOptionsFromAdapter` — pulls the BuildRequest-safe
	// registry view (drops every baml-rest-resolved strategy parent).
	// Used by BuildRequest dispatch and mixed-mode bridge legacy-child
	// callbacks (both target leaves, not parents). See PR #192
	// cold-review-4 + Option C.
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
			// Pre-size with capacity 3: room for ClientRegistry + TypeBuilder + WithOnTick
			jen.Id("result").Op(":=").Make(jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"), jen.Lit(0), jen.Lit(3)),
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

	// Generate `makeLegacyOptionsFromAdapter` — pulls the legacy
	// registry view (preserves explicit strategy-parent overrides).
	// Used by the top-level legacy fallthrough (_noRaw / _full impls
	// invoked via __legacyClientOverride) so BAML can honour runtime
	// strategy-parent overrides or emit canonical errors. See PR #192
	// cold-review-4 + Option C.
	out.Func().Id("makeLegacyOptionsFromAdapter").
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
			jen.Id("result").Op(":=").Make(jen.Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"), jen.Lit(0), jen.Lit(3)),
			jen.If(jen.Id("adapter").Dot("LegacyClientRegistry").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").
						Call(jen.Id("adapter").Dot("LegacyClientRegistry"))),
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

	// Generate shared streaming orchestration helpers (emitted once, called per-method)
	generateStreamHelpers(out)

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

	// ──────────────────────────────────────────────────────────────────
	// runNoRawOrchestration
	// ──────────────────────────────────────────────────────────────────
	out.Comment("runNoRawOrchestration manages the noRaw streaming lifecycle:")
	out.Comment("heartbeat tracking, onTick callback, goroutine launch, and panic recovery.")
	out.Comment("The per-method body closure handles stream creation and iteration.")
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
			// plannedSent gates the planned-metadata emission so the same
			// payload doesn't go out twice. We emit it upfront (before
			// body() runs the BAML stream) rather than inside onTick
			// because planned metadata describes the routing decision
			// already made — it must not depend on BAML's state-change
			// callbacks firing. For requests where BAML completes
			// synchronously (e.g., legacy path with WithClient naming
			// a strategy parent that resolves through static IR), no
			// onTick fires and the prior gating-on-heartbeatSent
			// design lost the planned event. See PR #192 verdict-15
			// follow-up: TestRoundRobinOverrides_InvalidStartRoutesToLegacy
			// returned 200 with empty X-BAML-Path / X-BAML-Path-Reason
			// for exactly this reason.
			jen.Var().Id("plannedSent").Qual("sync/atomic", "Bool"),
			// lastFuncLog stores the most recent FunctionLog reference seen
			// by onTick. Read by beforeFinal to derive winner identity and
			// BAML's internal call count for the outcome event. nil if no
			// onTick fired before the stream completed (rare; degrade
			// gracefully via BuildLegacyOutcome's planned-fallback ladder).
			jen.Var().Id("lastFuncLog").Qual("sync/atomic", "Value"),

			// emitPlanned sends the planned-metadata event to out exactly
			// once per orchestrator invocation. Called upfront from the
			// stream goroutine below, before body() runs BAML's stream,
			// so emission is guaranteed regardless of whether BAML fires
			// onTick. plannedSent CAS guarantees idempotency.
			jen.Id("emitPlanned").Op(":=").Func().Params().Block(
				jen.If(jen.Id("plannedMetadata").Op("==").Nil().Op("||").Id("newMetadataResult").Op("==").Nil()).Block(jen.Return()),
				jen.If(jen.Op("!").Id("plannedSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(jen.Return()),
				jen.Id("__plan").Op(":=").Op("*").Id("plannedMetadata"),
				jen.Id("__plan").Dot("Phase").Op("=").Qual(common.InterfacesPkg, "MetadataPhasePlanned"),
				jen.Id("__m").Op(":=").Id("newMetadataResult").Call(jen.Op("&").Id("__plan")),
				jen.If(jen.Id("__m").Op("==").Nil()).Block(jen.Return()),
				// Non-blocking emit: release on full / shutdown so a
				// busy or torn-down channel cannot wedge this path.
				// Matches the heartbeat's drop-on-full policy.
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__m")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__m"))),
					jen.Default().Block(jen.Id("release").Call(jen.Id("__m"))),
				),
			),

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
			jen.Id("emitFinal").Func().Params(jen.Any(), jen.String()).Add(streamResultIface.Clone()),
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
			// made and must not depend on BAML's onTick firing. See
			// PR #192 verdict-15 follow-up.
			jen.Var().Id("plannedSent").Qual("sync/atomic", "Bool"),
			jen.Var().Id("fatalMu").Qual("sync", "Mutex"),
			jen.Var().Id("fatalErr").Error(),

			// emitPlanned is called once per orchestrator invocation,
			// upfront in the stream goroutine. plannedSent CAS guarantees
			// idempotency.
			jen.Id("emitPlanned").Op(":=").Func().Params().Block(
				jen.If(jen.Id("plannedMetadata").Op("==").Nil().Op("||").Id("newMetadataResult").Op("==").Nil()).Block(jen.Return()),
				jen.If(jen.Op("!").Id("plannedSent").Dot("CompareAndSwap").Call(jen.False(), jen.True())).Block(jen.Return()),
				jen.Id("__plan").Op(":=").Op("*").Id("plannedMetadata"),
				jen.Id("__plan").Dot("Phase").Op("=").Qual(common.InterfacesPkg, "MetadataPhasePlanned"),
				jen.Id("__m").Op(":=").Id("newMetadataResult").Call(jen.Op("&").Id("__plan")),
				jen.If(jen.Id("__m").Op("==").Nil()).Block(jen.Return()),
				jen.Select().Block(
					jen.Case(jen.Id("out").Op("<-").Id("__m")).Block(),
					jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).Block(jen.Id("release").Call(jen.Id("__m"))),
					jen.Default().Block(jen.Id("release").Call(jen.Id("__m"))),
				),
			),

			// Extractor. The boolean argument captures the per-request
			// IncludeThinkingInRaw opt-in: when true, Anthropic
			// thinking_delta events are accumulated into the extractor's
			// raw buffer alongside text deltas; when false (default), the
			// extractor matches BAML's RawLLMResponse() text-only
			// semantics. Parseable text seen by Parse/ParseStream is never
			// affected by this flag — the extractor only feeds the raw
			// channel.
			jen.Id("extractor").Op(":=").Qual(common.SSEPkg, "NewIncrementalExtractor").Call(
				jen.Id("adapter").Dot("IncludeThinkingInRaw").Call(),
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
						// RawLLMResponse() handles two distinct concerns:
						//
						//   - Stale-SSE-view race (always-on): in rare CI-only
						//     races where lastFuncLog's SSEChunks view is stale
						//     (onTick didn't cover the final SSE event), the
						//     extractor can end up truncated or empty while
						//     RawLLMResponse is complete. We splice in only the
						//     missing text suffix from RawLLMResponse so any
						//     late text the extractor missed is recovered.
						//
						//   - IncludeThinkingInRaw opt-in (per-request): when
						//     enabled, the extractor accumulates Anthropic
						//     thinking_delta into raw while BAML's runtime never
						//     surfaces thinking. The splice approach preserves
						//     thinking content (which lives only in
						//     extractor.RawFull) alongside any recovered text.
						//
						// Reconciliation logic:
						//   1. If RawLLMResponse extends extractor.ParseableFull
						//      as a prefix and is longer, splice in the missing
						//      text suffix. This is the common case under both
						//      stale-race and happy-path-with-opt-in.
						//   2. Else if RawLLMResponse is longer than
						//      extractor.RawFull (the prefix invariant is
						//      violated — should not happen under Anthropic's
						//      thinking-then-text ordering, but defend anyway),
						//      defer to RawLLMResponse for correctness. This
						//      loses thinking under opt-in but ensures the
						//      authoritative text wins.
						//   3. Else keep extractor.RawFull (happy path:
						//      extractor's view is at least as complete).
						jen.Id("extractorMu").Dot("Lock").Call(),
						jen.Id("finalRaw").Op(":=").Id("extractor").Dot("RawFull").Call(),
						jen.Id("parseableFull").Op(":=").Id("extractor").Dot("ParseableFull").Call(),
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

						jen.Id("__r").Op(":=").Id("emitFinal").Call(jen.Id("finalResult"), jen.Id("finalRaw")),
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
