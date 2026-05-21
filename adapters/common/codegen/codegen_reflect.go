package codegen

import (
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
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
// BAML media type. Detection is by type name (Image, Audio, PDF, Video) and by an
// exact-or-prefix match of the type's reflected PkgPath against the configured
// BAML runtime and generated-client module paths. Threading the PackageConfig
// here matters because a non-default runtime (e.g. a forked patched-BAML module)
// reflects PkgPath values that the legacy substring-match heuristic would miss
// silently — every media field would round-trip as a plain struct.
func isMediaReflectType(typ reflect.Type, pkgs PackageConfig) (bamlutils.MediaKind, bool) {
	// Unwrap pointer and slice layers
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if kind, ok := mediaTypeNames[typ.Name()]; ok {
		if pkgPathMatchesBAML(typ.PkgPath(), pkgs) {
			return kind, true
		}
	}
	return 0, false
}

// pkgPathMatchesBAML reports whether pkgPath is the configured BAML runtime
// module, the configured generated-client root, or a subpackage of either.
// Exact-and-prefix matching avoids the cross-module false positives a bare
// substring search would let through (any path containing the bytes
// "boundaryml/baml" or "baml_client" anywhere — including under a non-BAML
// vendored dependency).
func pkgPathMatchesBAML(pkgPath string, pkgs PackageConfig) bool {
	if pkgPath == "" {
		return false
	}
	if pkgs.BamlPkg != "" {
		if pkgPath == pkgs.BamlPkg || strings.HasPrefix(pkgPath, pkgs.BamlPkg+"/") {
			return true
		}
	}
	if pkgs.GeneratedClientPkg != "" {
		if pkgPath == pkgs.GeneratedClientPkg || strings.HasPrefix(pkgPath, pkgs.GeneratedClientPkg+"/") {
			return true
		}
	}
	return false
}

// structContainsMedia recursively checks whether a struct type (or any nested struct)
// contains BAML media-typed fields (Image, Audio, PDF, Video).
// Uses a visited set to break cycles from self-referential or mutually recursive types.
func structContainsMedia(typ reflect.Type, pkgs PackageConfig) bool {
	return structContainsMediaVisited(typ, make(map[reflect.Type]bool), pkgs)
}

func structContainsMediaVisited(typ reflect.Type, visited map[reflect.Type]bool, pkgs PackageConfig) bool {
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
		if _, ok := isMediaReflectType(ft, pkgs); ok {
			return true
		}
		if inner.Kind() == reflect.Struct && structContainsMediaVisited(ft, visited, pkgs) {
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
	// convertNeedsOwnedNested records which mirror converters take
	// the third `ownedNested *[]func()` parameter — either because the
	// converter directly pools a `*[]Struct` field, or because it
	// transitively calls another converter that takes the parameter.
	//
	// The `*[]func()` (closure-context) shape lets a single propagated
	// slice carry release closures for every pooled nested type at any
	// depth — distinct nested element types append distinct closures,
	// the outer dispatch drains them uniformly. The earlier `*[]*[]T`
	// shape forced a single inner element type per converter and could
	// not propagate through nested converter calls without per-type
	// parameter explosion.
	convertNeedsOwnedNested map[reflect.Type]bool
}

func newMirrorStructTracker() *mirrorStructTracker {
	return &mirrorStructTracker{
		generated:               make(map[reflect.Type]string),
		convertNeedsOwnedNested: make(map[reflect.Type]bool),
	}
}

// convertNeedsOwnedNestedFor reports whether convert<Mirror> for
// `outer` takes the `ownedNested *[]func()` parameter. Call sites
// (dispatch top-level + nested helpers) consult this to thread the
// matching slice through.
func (m *mirrorStructTracker) convertNeedsOwnedNestedFor(outer reflect.Type) bool {
	if m == nil {
		return false
	}
	return m.convertNeedsOwnedNested[outer]
}

// precomputeOwnedNestedNeeds walks the struct-media type graph
// reachable from `roots` and populates `convertNeedsOwnedNested`
// for every reachable type via fixpoint iteration. Must run before
// any `generateConversionFunc` call so nested converter call sites
// query the final transitive value instead of a partial answer
// frozen mid-emission.
//
// Why a separate pass: `ensureMirrorStruct` marks each type as
// "generated" before recursing into its fields. For cycles like
// `A.B *B; B.A *A`, B's body emission completes BEFORE A finishes
// its own analysis — so B's nestedStructConversion call site for
// the A field used to snapshot the pre-fixpoint
// (`convertNeedsOwnedNested[A] == false`) answer, while A's
// converter signature later gained the third parameter once its
// own pre-walk set the flag. The rendered Go is then internally
// inconsistent: B's call site is 2-arg, A's signature is 3-arg →
// compile error.
//
// The precompute resolves this by computing the transitive closure
// up front. The fixpoint converges because the lattice is finite
// (one bit per type, monotonically rising true), so each round
// either flips at least one bit or terminates.
func (m *mirrorStructTracker) precomputeOwnedNestedNeeds(roots []reflect.Type, pkgs PackageConfig) {
	if m == nil {
		return
	}

	// Walk every struct-media type reachable from the roots so the
	// fixpoint loop iterates the full graph and not just the
	// subset emitted so far. Cycle detection is via `visited`; the
	// recursion descends into ptr/slice element types because
	// those are the wrapping shapes struct-media fields take.
	visited := map[reflect.Type]bool{}
	var types []reflect.Type
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		if !structContainsMedia(t, pkgs) {
			return
		}
		if visited[t] {
			return
		}
		visited[t] = true
		types = append(types, t)
		for field := range t.Fields() {
			if !field.IsExported() {
				continue
			}
			if _, isMedia := isMediaReflectType(field.Type, pkgs); isMedia {
				continue
			}
			walk(field.Type)
		}
	}
	for _, root := range roots {
		walk(root)
	}

	// Seed: any reachable type that DIRECTLY pools (has a
	// `*[]Struct` value-element field whose inner struct contains
	// media) needs `ownedNested` outright. This mirrors
	// nestedStructConversion's `*[]Struct` value-element branch
	// gating.
	for _, t := range types {
		if m.convertNeedsOwnedNested[t] {
			continue
		}
		for field := range t.Fields() {
			if !field.IsExported() {
				continue
			}
			ft := field.Type
			if _, isMedia := isMediaReflectType(ft, pkgs); isMedia {
				continue
			}
			if ft.Kind() != reflect.Ptr || ft.Elem().Kind() != reflect.Slice || ft.Elem().Elem().Kind() == reflect.Ptr {
				continue
			}
			inner := ft.Elem().Elem()
			if inner.Kind() != reflect.Struct || !structContainsMedia(ft, pkgs) {
				continue
			}
			m.convertNeedsOwnedNested[t] = true
			break
		}
	}

	// Fixpoint: propagate from callees to callers. If T calls
	// convert<N> and N needs ownedNested, T must accept and
	// thread the same parameter through.
	for {
		changed := false
		for _, t := range types {
			if m.convertNeedsOwnedNested[t] {
				continue
			}
			for field := range t.Fields() {
				if !field.IsExported() {
					continue
				}
				ft := field.Type
				if _, isMedia := isMediaReflectType(ft, pkgs); isMedia {
					continue
				}
				inner := ft
				for inner.Kind() == reflect.Ptr || inner.Kind() == reflect.Slice {
					inner = inner.Elem()
				}
				if inner.Kind() != reflect.Struct || !structContainsMedia(ft, pkgs) {
					continue
				}
				if m.convertNeedsOwnedNested[inner] {
					m.convertNeedsOwnedNested[t] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			return
		}
	}
}

// mirrorInputName returns the name for a mirror input struct.
func mirrorInputName(typ reflect.Type) string {
	return typ.Name() + "MediaInput"
}

// ensureMirrorStruct generates a mirror struct and conversion function for a BAML struct
// that contains media fields. Returns the mirror struct name. Skips generation if already done.
//
// `pools` (optional) wires slice-pool helpers into nested `*[]Struct`
// conversions and the convert function signature. Pass nil for adapters
// that should not opt into pooling.
func (m *mirrorStructTracker) ensureMirrorStruct(out *jen.File, typ reflect.Type, pkgs PackageConfig, pools *slicePoolTracker) string {
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
		if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type, pkgs) {
			m.ensureMirrorStruct(out, field.Type, pkgs, pools)
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
		if _, isMedia := isMediaReflectType(field.Type, pkgs); isMedia {
			// Replace media type with MediaInput, preserving ptr/slice wrapping
			fieldCode = jen.Id(field.Name).Add(mediaFieldType(field.Type, pkgs.InterfacesPkg))
		} else {
			fieldInner := field.Type
			for fieldInner.Kind() == reflect.Ptr || fieldInner.Kind() == reflect.Slice {
				fieldInner = fieldInner.Elem()
			}
			if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type, pkgs) {
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
	m.generateConversionFunc(out, inner, mirrorName, pkgs, pools)

	return mirrorName
}

// generateConversionFunc generates a function that converts a mirror input struct
// to the real BAML struct, handling media field conversion.
//
// pools is the per-file slice pool tracker. When non-nil, `*[]Struct`
// value-element fields route through the pool helpers and the emitted
// function gains an extra `ownedNested *[]func()` parameter — a slice
// of release closures the dispatch caller drains after BAML consumes
// the converted value. The closure-context shape (versus the earlier
// `*[]*[]T` typed-pointer shape) lets a single propagated parameter
// carry release closures for arbitrarily many distinct pooled nested
// types at any depth: each pooled-nested branch captures its own
// `put<X>Slice` call in a closure and appends it; the outer release
// loop invokes them uniformly.
//
// A converter takes the `ownedNested` parameter when EITHER:
//
//  1. It directly pools at least one `*[]Struct` field with value
//     elements (registering a release closure during conversion).
//  2. It transitively calls another converter that takes the parameter
//     (in which case it must thread its own `ownedNested` through so
//     the inner converter's appended closures bubble up to the outer
//     dispatch's drain).
//
// Because ensureMirrorStruct generates nested converters before the
// outer (depth-first), `convertNeedsOwnedNestedFor` returns the correct
// answer for any nested call site by the time the outer converter is
// emitted.
func (m *mirrorStructTracker) generateConversionFunc(out *jen.File, bamlType reflect.Type, mirrorName string, pkgs PackageConfig, pools *slicePoolTracker) {
	funcName := "convert" + mirrorName
	bamlTypeExpr := parseReflectType(bamlType).statement

	// `needsOwnedNested` is the precomputed transitive answer from
	// `precomputeOwnedNestedNeeds`. Earlier revisions of this code
	// re-derived the flag inline mid-emission, which races with
	// recursive `ensureMirrorStruct` callers: for cycles like
	// `A.B *B; B.A *A` where A directly pools, B's emission
	// would complete before A set its own flag, so B's call site
	// for A was 2-arg while A's signature ended up 3-arg → compile
	// error. The precompute pass guarantees every reachable type's
	// flag is final before any body emission runs.
	//
	// When `pools` is nil the legacy non-pooling path applies and
	// neither signatures nor call sites thread ownedNested.
	needsOwnedNested := pools != nil && m.convertNeedsOwnedNested[bamlType]

	var bodyCode []jen.Code
	bodyCode = append(bodyCode, jen.Var().Id("result").Add(bamlTypeExpr.Clone()))

	for field := range bamlType.Fields() {
		if !field.IsExported() {
			continue
		}

		fieldName := field.Name
		srcExpr := jen.Id("input").Dot(fieldName)

		if mediaKind, isMedia := isMediaReflectType(field.Type, pkgs); isMedia {
			bodyCode = append(bodyCode, mediaFieldConversion(fieldName, srcExpr, field.Type, mediaKind, pkgs)...)
		} else {
			fieldInner := field.Type
			for fieldInner.Kind() == reflect.Ptr || fieldInner.Kind() == reflect.Slice {
				fieldInner = fieldInner.Elem()
			}
			if fieldInner.Kind() == reflect.Struct && structContainsMedia(field.Type, pkgs) {
				bodyCode = append(bodyCode, nestedStructConversion(fieldName, srcExpr, field.Type, m, pools, out)...)
			} else {
				// Direct copy
				bodyCode = append(bodyCode, jen.Id("result").Dot(fieldName).Op("=").Add(srcExpr.Clone()))
			}
		}
	}

	bodyCode = append(bodyCode, jen.Return(jen.Id("result"), jen.Nil()))

	params := []jen.Code{
		jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter"),
		jen.Id("input").Op("*").Id(mirrorName),
	}
	if needsOwnedNested {
		params = append(params, jen.Id("ownedNested").Op("*").Index().Func().Params())
	}

	out.Func().Id(funcName).
		Params(params...).
		Params(bamlTypeExpr.Clone(), jen.Error()).
		Block(bodyCode...)
}

// mediaFieldConversion generates code to convert a MediaInput field to a BAML media type.
// Handles direct, pointer, and slice wrapping.
func mediaFieldConversion(fieldName string, srcExpr *jen.Statement, fieldType reflect.Type, mediaKind bamlutils.MediaKind, pkgs PackageConfig) []jen.Code {
	kindExpr := jen.Qual(pkgs.InterfacesPkg, mediaKind.ConstName())

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
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
//
// pools (optional) wires the `*[]Struct` value-element branch through
// a shared slice pool — the parts slice is checked out via the
// generated getter, a release closure that `put`s the slice back is
// appended to the enclosing convert function's `ownedNested
// *[]func()` parameter, and the dispatch site invokes the closures
// after BAML consumes the converted value. Passing nil keeps the
// legacy `make([]T, n)` shape.
//
// Every converter call inside the body threads `ownedNested` through
// when the called converter takes it. This lets a single propagated
// `*[]func()` slice carry release closures across arbitrarily nested
// converters and arbitrarily many distinct pooled element types.
func nestedStructConversion(fieldName string, srcExpr *jen.Statement, fieldType reflect.Type, tracker *mirrorStructTracker, pools *slicePoolTracker, out *jen.File) []jen.Code {
	isPtr := fieldType.Kind() == reflect.Ptr
	isSlice := fieldType.Kind() == reflect.Slice

	innerType := fieldType
	for innerType.Kind() == reflect.Ptr || innerType.Kind() == reflect.Slice {
		innerType = innerType.Elem()
	}
	convertFunc := "convert" + mirrorInputName(innerType)
	innerNeedsOwnedNested := pools != nil && tracker.convertNeedsOwnedNestedFor(innerType)

	// buildConvertCall threads `ownedNested` into the convert<Inner>
	// call when the inner converter takes it. Sits at the head of the
	// var-args list so every branch builds the call the same way.
	buildConvertCall := func(vArg jen.Code) *jen.Statement {
		callArgs := []jen.Code{jen.Id("adapter"), vArg}
		if innerNeedsOwnedNested {
			callArgs = append(callArgs, jen.Id("ownedNested"))
		}
		return jen.List(jen.Id("__converted"), jen.Id("__err")).Op(":=").Id(convertFunc).Call(callArgs...)
	}

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
			buildConvertCall(vArg),
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
				buildConvertCall(vArg),
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

			// Only pool when the inner slice element is a value type.
			// Pointer-element slices (`*[]*T`) fall through to the
			// legacy `make` path because pooling them would have to
			// nil-clear per element and the dispatch site never
			// promised a pool for that shape. The closure-context
			// `ownedNested` parameter accepts arbitrary release
			// closures, so distinct pooled inner types coexist; only
			// the element-shape mismatch still gates the pool branch.
			if pools != nil && out != nil && !elemIsPtr {
				poolNames := pools.ensure(out, elemType, 256)
				return []jen.Code{
					jen.If(srcExpr.Clone().Op("!=").Nil()).Block(
						jen.Id("__partsPtr").Op(":=").Id(poolNames.getFunc).Call(
							jen.Len(jen.Op("*").Add(srcExpr.Clone())),
						),
						// Append a release closure capturing this
						// branch's specific pool helper + pointer.
						// Distinct nested types append distinct
						// closures; the outer dispatch drains them
						// uniformly without knowing the inner types.
						jen.Op("*").Id("ownedNested").Op("=").Append(
							jen.Op("*").Id("ownedNested"),
							jen.Func().Params().Block(
								jen.Id(poolNames.putFunc).Call(jen.Id("__partsPtr")),
							),
						),
						jen.Op("*").Id("__partsPtr").Op("=").Parens(jen.Op("*").Id("__partsPtr")).Index(
							jen.Empty(),
							jen.Len(jen.Op("*").Add(srcExpr.Clone())),
						),
						jen.Id("__ptrSlice").Op(":=").Op("*").Id("__partsPtr"),
						jen.For(jen.List(jen.Id("__i"), jen.Id("__v")).Op(":=").Range().Op("*").Add(srcExpr.Clone())).Block(innerLoopBody...),
						jen.Id("result").Dot(fieldName).Op("=").Op("&").Id("__ptrSlice"),
					),
				}
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
				buildConvertCall(srcExpr.Clone()),
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
			buildConvertCall(jen.Op("&").Add(srcExpr.Clone())),
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
func mediaConversionCode(convertedVar, fieldName string, paramType reflect.Type, mediaKind bamlutils.MediaKind, pkgs PackageConfig) []jen.Code {
	// Determine the wrapping: direct, pointer, or slice
	isPtr := paramType.Kind() == reflect.Ptr
	isSlice := paramType.Kind() == reflect.Slice

	// Resolve the innermost BAML type for the type assertion
	innerType := paramType
	for innerType.Kind() == reflect.Ptr || innerType.Kind() == reflect.Slice {
		innerType = innerType.Elem()
	}
	bamlType := parseReflectType(innerType).statement

	kindExpr := jen.Qual(pkgs.InterfacesPkg, mediaKind.ConstName())

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
			jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
				jen.List(jen.Id("__raw"), jen.Id("__err")).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
		jen.List(jen.Id("__raw_"+convertedVar), jen.Id("__err_"+convertedVar)).Op(":=").Qual(pkgs.InterfacesPkg, "ConvertMedia").Call(
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
func mediaFieldType(typ reflect.Type, interfacesPkg string) *jen.Statement {
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

	statement := jen.Qual(interfacesPkg, "MediaInput")

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
