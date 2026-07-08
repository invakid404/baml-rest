package main

// This file is the de-BAML P3 native static-schema builder (issue #586,
// slice 2). It consumes ONLY the native BAML AST produced by
// bamlutils/bamlparser (slice 1) plus baml-rest's own descriptor model — no
// BAML runtime, no generated client. For every function that declares an
// output type it builds a per-function bamlutils/schemadescriptor.Bundle,
// lowers it with internal/schema.FromStaticDescriptor, and validates it with
// Bundle.ValidateOutput. A function is SUPPORTED only when its WHOLE output
// graph lowers and validates; otherwise it is DECLINED fail-closed with a
// stable reason and no descriptor is emitted for it.
//
// EMBED-REGEN / BUILD SITE (read before adding a file to cmd/introspect): this
// is now a MULTI-FILE package. Two out-of-package sites must stay in sync when
// you add a .go file here:
//   1. the root //go:embed manifest (embed.go) must list the new file so it
//      ships in the customer/container build's embed.FS — run `go run
//      cmd/embed/main.go` and commit the regenerated embed.go; and
//   2. the container build must compile the WHOLE package, not a single file.
//      cmd/build/build.sh runs `go run ./cmd/introspect` (package form) for
//      exactly this reason — `go run cmd/introspect/main.go` compiles only
//      main.go and reports a sibling symbol (e.g. buildStaticSchemas) as
//      "undefined". See #586 and the comment at cmd/build/build.sh.
//
// SCOPE (slice 2 — structural build only):
//   - Lower primitives, named class/enum refs, non-recursive aliases (inlined),
//     lists, maps, optionals, unions, and string/int/bool literals.
//   - Validate STRUCTURALLY via FromStaticDescriptor + ValidateOutput. This is
//     NOT output-format render parity — reachability ORDERING and the BAML
//     oracle are slice 3, so the Enums/Classes slices are emitted in discovery
//     order (dependencies first), which FromStaticDescriptor preserves
//     verbatim and Validate/ValidateOutput accept order-independently.
//   - Nothing is wired into the request/response runtime; the built bundles
//     and decline reasons are stored on *bamlConfig but not consumed yet.
//
// Lax-vs-BAML / decline decisions logged here are also recorded on #586:
//
//   D1. ANY schema-affecting attribute on a reachable output node
//       (@alias/@description/@assert/@check/@stream.*/@@dynamic/@skip, or any
//       other attribute) DECLINES the function. Slice 2 has no attribute
//       lowering (that is slices 3/4), so rather than silently dropping an
//       attribute we fail closed. This is a parity-decline, not input laxness.
//   D2. media and tuple output are lowered FAITHFULLY into the descriptor and
//       left for ValidateOutput to reject (it errors on tuple/arrow/top/media),
//       so the single authority on output-legality stays internal/schema, not
//       a hand-rolled check here.
//   D3. duplicate class/enum/alias names are POISONED in the semantic index; a
//       function whose output graph reaches a poisoned name declines, while
//       functions that never reach it still build. Per-method fail-closed,
//       mirroring the rest of the decline surface.
//   D4. duplicate function names are last-wins (a later declaration's build
//       outcome overrides an earlier one), matching the config walker's
//       last-wins posture for duplicate keys.

import (
	"fmt"
	"reflect"
	"strconv"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// buildStaticSchemas builds a native static output-schema descriptor for every
// function that has a parsed return type, from the already-parsed .baml files.
// It returns two always-non-nil maps keyed by function name: bundles for the
// SUPPORTED functions (whose whole output graph lowered + validated) and
// declines for the unsupported ones (a stable reason string). A declined
// function never appears in bundles, and a supported one never in declines.
//
// It never errors: an output graph that cannot be represented faithfully is a
// per-function decline, not a pipeline failure, so this pass is safe to run on
// the production introspect path without changing any config extraction.
func buildStaticSchemas(files []*bamlparser.File) (map[string]sd.Bundle, map[string]string) {
	idx := buildSchemaTypeIndex(files)

	bundles := make(map[string]sd.Bundle)
	declines := make(map[string]string)

	for _, f := range files {
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			fn := it.Function
			if fn == nil || fn.Name == "" {
				continue
			}
			bundle, err := buildFunctionDescriptor(idx, fn)
			if err != nil {
				// D4: last-wins on duplicate function names — a later
				// declaration's decline supersedes an earlier success.
				declines[fn.Name] = err.Error()
				delete(bundles, fn.Name)
				continue
			}
			bundles[fn.Name] = bundle
			delete(declines, fn.Name)
		}
	}

	return bundles, declines
}

// schemaTypeIndex classifies every top-level type name (class/enum/alias)
// across all parsed files BEFORE any body is lowered, mirroring
// internal/schema.FromDynamicOutputSchema's classify-first approach so that a
// reference resolves regardless of declaration order or cross-file layout.
//
// A name declared more than once (in one category or across categories) is
// recorded in ambiguous; the builder declines any function that reaches a
// poisoned name (D3) rather than silently last-wins-ing type definitions.
type schemaTypeIndex struct {
	classes   map[string]*bamlparser.TypeBlock
	enums     map[string]*bamlparser.TypeBlock
	aliases   map[string]*bamlparser.TypeAlias
	ambiguous map[string]struct{}
}

func (i *schemaTypeIndex) isAmbiguous(name string) bool {
	_, ok := i.ambiguous[name]
	return ok
}

// buildSchemaTypeIndex classifies all class/enum/alias names first. `test`
// blocks are not schema definitions and are skipped, matching the parser's
// metadata-only posture for them.
func buildSchemaTypeIndex(files []*bamlparser.File) *schemaTypeIndex {
	idx := &schemaTypeIndex{
		classes:   make(map[string]*bamlparser.TypeBlock),
		enums:     make(map[string]*bamlparser.TypeBlock),
		aliases:   make(map[string]*bamlparser.TypeAlias),
		ambiguous: make(map[string]struct{}),
	}

	counts := make(map[string]int)
	for _, f := range files {
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			switch {
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "class":
				counts[it.TypeBlock.Name]++
				idx.classes[it.TypeBlock.Name] = it.TypeBlock
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "enum":
				counts[it.TypeBlock.Name]++
				idx.enums[it.TypeBlock.Name] = it.TypeBlock
			case it.TypeAlias != nil && it.TypeAlias.Name != "":
				counts[it.TypeAlias.Name]++
				idx.aliases[it.TypeAlias.Name] = it.TypeAlias
			}
		}
	}
	for name, c := range counts {
		if c > 1 {
			idx.ambiguous[name] = struct{}{}
		}
	}
	return idx
}

// buildFunctionDescriptor builds and validates the descriptor for one
// function's output type. It returns a decline error the moment any reachable
// construct cannot be represented faithfully; the resulting descriptor is only
// returned once FromStaticDescriptor and ValidateOutput both accept it.
func buildFunctionDescriptor(idx *schemaTypeIndex, fn *bamlparser.FunctionBlock) (sd.Bundle, error) {
	if fn.Return == nil {
		return sd.Bundle{}, fmt.Errorf("function %q has no parsed return type", fn.Name)
	}

	b := newDescriptorBuilder(idx)
	target, err := b.lowerType(fn.Return)
	if err != nil {
		return sd.Bundle{}, err
	}

	bundle := sd.Bundle{
		Version: sd.Version,
		Method:  fn.Name,
		Target:  target,
		Enums:   b.orderedEnums(),
		Classes: b.orderedClasses(),
	}

	// Lower into the internal model (fails closed on unresolved refs, malformed
	// payloads, duplicate rendered names, invalid map keys, null-as-variant),
	// then enforce the output profile (rejects tuple/arrow/top/media).
	internal, err := schema.FromStaticDescriptor(bundle)
	if err != nil {
		return sd.Bundle{}, err
	}
	if err := internal.ValidateOutput(); err != nil {
		return sd.Bundle{}, err
	}
	return bundle, nil
}

// descriptorBuilder lowers one function's output graph. It is single-use: a
// fresh builder is created per function so the recursion-guard and collected
// definition sets are scoped to that function's reachable graph.
type descriptorBuilder struct {
	index *schemaTypeIndex

	// classes/enums are the collected definitions (the reachable set), keyed by
	// canonical name and deduplicated. classOrder/enumOrder record discovery
	// order (dependencies first, post-order); slice 3 will replace this with
	// BAML reachability order.
	classes    map[string]sd.ClassDef
	classOrder []string
	enums      map[string]sd.EnumDef
	enumOrder  []string

	// classVisiting / aliasVisiting are the active DFS paths, used to detect
	// recursion (declined until slice 5). A name on the path that is re-entered
	// is a cycle; a name already fully collected is a DAG re-reference and
	// resolves normally.
	classVisiting map[string]bool
	aliasVisiting map[string]bool
}

func newDescriptorBuilder(idx *schemaTypeIndex) *descriptorBuilder {
	return &descriptorBuilder{
		index:         idx,
		classes:       make(map[string]sd.ClassDef),
		enums:         make(map[string]sd.EnumDef),
		classVisiting: make(map[string]bool),
		aliasVisiting: make(map[string]bool),
	}
}

func (b *descriptorBuilder) orderedClasses() []sd.ClassDef {
	if len(b.classOrder) == 0 {
		return nil
	}
	out := make([]sd.ClassDef, 0, len(b.classOrder))
	for _, name := range b.classOrder {
		out = append(out, b.classes[name])
	}
	return out
}

func (b *descriptorBuilder) orderedEnums() []sd.EnumDef {
	if len(b.enumOrder) == 0 {
		return nil
	}
	out := make([]sd.EnumDef, 0, len(b.enumOrder))
	for _, name := range b.enumOrder {
		out = append(out, b.enums[name])
	}
	return out
}

// lowerType lowers one parsed TypeExpr into a descriptor Type, collecting any
// reachable class/enum definitions along the way. It returns a decline error
// for any construct slice 2 cannot represent faithfully.
func (b *descriptorBuilder) lowerType(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t == nil {
		return sd.Type{}, fmt.Errorf("missing type expression")
	}

	// D1: any attribute on a reachable output node declines the function.
	// Slice 2 has no attribute lowering, so we fail closed rather than drop it.
	if len(t.Attributes) > 0 {
		return sd.Type{}, fmt.Errorf("type carries attribute @%s (schema attributes are not supported yet — slices 3/4)", t.Attributes[0].Name)
	}

	switch t.Kind {
	case bamlparser.KindUnsupported:
		reason := t.Reason
		if reason == "" {
			reason = "unsupported type"
		}
		return sd.Type{}, fmt.Errorf("%s", reason)

	case bamlparser.KindPrimitive:
		return lowerPrimitive(t.Primitive)

	case bamlparser.KindMedia:
		// D2: lowered faithfully; ValidateOutput rejects media output.
		return lowerMedia(t.Media)

	case bamlparser.KindNameRef:
		return b.lowerNameRef(t)

	case bamlparser.KindList:
		return b.lowerList(t)

	case bamlparser.KindMap:
		return b.lowerMap(t)

	case bamlparser.KindUnion:
		return b.lowerUnion(t)

	case bamlparser.KindLiteral:
		return lowerLiteral(t)

	case bamlparser.KindTuple:
		// D2: lowered faithfully; ValidateOutput rejects tuple output.
		return b.lowerTuple(t)

	case bamlparser.KindGroup:
		// A parenthesized single type is not a runtime node; unwrap it. Any
		// group-level attribute was already rejected by the D1 check above.
		return b.lowerType(t.Inner)

	default:
		return sd.Type{}, fmt.Errorf("unhandled type kind %d", t.Kind)
	}
}

// lowerPrimitive maps a bare primitive identifier to its descriptor primitive.
func lowerPrimitive(prim string) (sd.Type, error) {
	switch prim {
	case "string":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}, nil
	case "int":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveInt}, nil
	case "float":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveFloat}, nil
	case "bool":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveBool}, nil
	case "null":
		return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveNull}, nil
	default:
		return sd.Type{}, fmt.Errorf("unknown primitive %q", prim)
	}
}

// lowerMedia maps a media identifier to a descriptor media primitive. The
// resulting type lowers fine but is rejected by ValidateOutput (media is not a
// usable output type), which is the intended decline path (D2).
func lowerMedia(media string) (sd.Type, error) {
	var kind sd.MediaKind
	switch media {
	case "image":
		kind = sd.MediaImage
	case "audio":
		kind = sd.MediaAudio
	case "pdf":
		kind = sd.MediaPDF
	case "video":
		kind = sd.MediaVideo
	default:
		return sd.Type{}, fmt.Errorf("unknown media type %q", media)
	}
	return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveMedia, Media: kind}, nil
}

// lowerList expands a parsed KindList (element + dimension count) into nested
// descriptor TypeList nodes, one per dimension. An optional array suffix was
// already lowered by the parser into a nullable union around the whole list,
// so it is not this function's concern.
func (b *descriptorBuilder) lowerList(t *bamlparser.TypeExpr) (sd.Type, error) {
	elem, err := b.lowerType(t.Elem)
	if err != nil {
		return sd.Type{}, err
	}
	dims := t.Dims
	if dims < 1 {
		dims = 1
	}
	result := elem
	for i := 0; i < dims; i++ {
		child := result
		result = sd.Type{Kind: sd.TypeList, Elem: &child}
	}
	return result, nil
}

func (b *descriptorBuilder) lowerMap(t *bamlparser.TypeExpr) (sd.Type, error) {
	key, err := b.lowerType(t.Key)
	if err != nil {
		return sd.Type{}, err
	}
	val, err := b.lowerType(t.Value)
	if err != nil {
		return sd.Type{}, err
	}
	// ValidateOutput enforces BAML-legal map key shapes (string primitive,
	// enum, string literal, or a non-nullable union of string literals).
	return sd.Type{Kind: sd.TypeMap, Key: &key, Value: &val}, nil
}

// lowerUnion lowers every union member and then NORMALIZES the lowered result
// so the descriptor is BAML-equivalent regardless of how nesting arose. A union
// member is not always a bare AST child: a non-recursive alias or a
// parenthesized group can lower into its own TypeUnion, and an alias can lower
// to a bare null. Appending those verbatim would emit a nested union or a
// null-primitive variant (which FromStaticDescriptor rejects), so normalization
// flattens nested unions and folds ALL nullability sources (a trailing `?`, an
// explicit `null` member, a group?/alias-to-null) into a single Nullable flag —
// exactly like the dynamic builder's makeUnion/simplifyUnion path
// (internal/schema/build.go). See #586 (D1/union-normalization).
func (b *descriptorBuilder) lowerUnion(t *bamlparser.TypeExpr) (sd.Type, error) {
	choices := make([]sd.Type, 0, len(t.Variants))
	for _, v := range t.Variants {
		// Lower every member — including a bare `null` (which normalization
		// hoists into Nullable) and an attributed null (which lowerType's D1
		// guard declines). No special-casing here keeps the null-folding in one
		// place (normalizeDescriptorUnion).
		lowered, err := b.lowerType(v)
		if err != nil {
			return sd.Type{}, err
		}
		choices = append(choices, lowered)
	}
	return normalizeDescriptorUnion(choices, t.Nullable), nil
}

// normalizeDescriptorUnion mirrors internal/schema.simplifyUnion for descriptor
// types: it flattens nested (constraint-free) unions, deduplicates variants
// preserving first-seen order, hoists every null into the Nullable marker
// (never a null-primitive variant), collapses an all-null union to the null
// primitive, and collapses a single non-null variant to that variant unless the
// union is also nullable (then it stays an optional-of-one).
//
// Slice 2 never attaches constraints/streaming metadata (any attribute-bearing
// node is declined by D1), so every lowered union here is metadata-free and
// flattens fully — the constraint-carrying-union special case BAML preserves
// cannot arise yet.
func normalizeDescriptorUnion(choices []sd.Type, nullable bool) sd.Type {
	flattened := make([]sd.Type, 0, len(choices))
	for _, c := range choices {
		flattened = append(flattened, flattenDescriptorUnion(c)...)
	}
	if nullable {
		flattened = append(flattened, descriptorNull())
	}

	hasNull := false
	variants := make([]sd.Type, 0, len(flattened))
	for _, t := range flattened {
		if isDescriptorNull(t) {
			hasNull = true
			continue
		}
		if containsDescriptorType(variants, t) {
			continue
		}
		variants = append(variants, t)
	}

	switch len(variants) {
	case 0:
		return descriptorNull()
	case 1:
		if hasNull {
			v := variants[0]
			return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{v}, Nullable: true}}
		}
		return variants[0]
	default:
		return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: variants, Nullable: hasNull}}
	}
}

// flattenDescriptorUnion replaces a constraint-free union with its
// (recursively flattened) variants plus a trailing null sentinel when the union
// is nullable; every other type flattens to itself. Mirrors
// internal/schema.flattenForUnion.
func flattenDescriptorUnion(t sd.Type) []sd.Type {
	if t.Kind == sd.TypeUnion && t.Union != nil && len(t.Meta.Constraints) == 0 {
		out := make([]sd.Type, 0, len(t.Union.Variants)+1)
		for _, v := range t.Union.Variants {
			out = append(out, flattenDescriptorUnion(v)...)
		}
		if t.Union.Nullable {
			out = append(out, descriptorNull())
		}
		return out
	}
	return []sd.Type{t}
}

// descriptorNull is the canonical null primitive used as the nullability
// sentinel during union flattening.
func descriptorNull() sd.Type {
	return sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveNull}
}

// isDescriptorNull reports whether t is the null primitive (BAML
// TypeGeneric::is_null: only Primitive(Null), not an optional union).
func isDescriptorNull(t sd.Type) bool {
	return t.Kind == sd.TypePrimitive && t.Primitive == sd.PrimitiveNull
}

// containsDescriptorType reports whether ts already holds a type structurally
// equal to t. Mirrors internal/schema.containsType: BAML's simplify()
// deduplicates with full structural equality, which for the metadata-free
// slice-2 path reduces to a deep comparison of the descriptor type trees.
func containsDescriptorType(ts []sd.Type, t sd.Type) bool {
	for i := range ts {
		if reflect.DeepEqual(ts[i], t) {
			return true
		}
	}
	return false
}

func (b *descriptorBuilder) lowerTuple(t *bamlparser.TypeExpr) (sd.Type, error) {
	items := make([]sd.Type, 0, len(t.Items))
	for _, it := range t.Items {
		lowered, err := b.lowerType(it)
		if err != nil {
			return sd.Type{}, err
		}
		items = append(items, lowered)
	}
	return sd.Type{Kind: sd.TypeTuple, Items: items}, nil
}

// lowerLiteral lowers a string/int/bool literal type. Float literals are
// declined: BAML v0.223 rejects them and the descriptor model has no float
// literal kind, so they cannot be represented faithfully.
func lowerLiteral(t *bamlparser.TypeExpr) (sd.Type, error) {
	switch t.LiteralKind {
	case "string":
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: t.LiteralValue}}, nil
	case "int":
		n, err := strconv.ParseInt(t.LiteralValue, 10, 64)
		if err != nil {
			return sd.Type{}, fmt.Errorf("invalid int literal %q: %w", t.LiteralValue, err)
		}
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralInt, Int: n}}, nil
	case "bool":
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralBool, Bool: t.LiteralValue == "true"}}, nil
	case "float":
		return sd.Type{}, fmt.Errorf("float literal types are not supported (BAML v0.223 rejects them)")
	default:
		return sd.Type{}, fmt.Errorf("unknown literal kind %q", t.LiteralKind)
	}
}

// lowerNameRef resolves a named reference against the semantic index and lowers
// the referenced definition (collecting classes/enums, inlining non-recursive
// aliases). Path/namespaced identifiers, ambiguous names, and unresolved names
// all decline.
func (b *descriptorBuilder) lowerNameRef(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t.Namespaced || t.Path {
		return sd.Type{}, fmt.Errorf("path/namespaced identifier %q is not supported in a type position", t.Name)
	}
	name := t.Name
	if b.index.isAmbiguous(name) {
		return sd.Type{}, fmt.Errorf("type name %q is declared more than once (duplicate class/enum/alias)", name)
	}

	if tb, ok := b.index.classes[name]; ok {
		if err := b.ensureClass(name, tb); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeClass, Name: name, Mode: sd.NonStreaming}, nil
	}
	if tb, ok := b.index.enums[name]; ok {
		if err := b.ensureEnum(name, tb); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeEnum, Name: name}, nil
	}
	if alias, ok := b.index.aliases[name]; ok {
		return b.resolveAlias(name, alias)
	}
	return sd.Type{}, fmt.Errorf("unresolved type reference %q", name)
}

// ensureClass lowers a class definition into the collected set exactly once.
// It declines block attributes (@@dynamic etc.), unsupported body content
// (methods / nested blocks), named-argument (generic) class blocks, attribute
// -bearing fields, and recursion (a class re-entered while on the active path).
func (b *descriptorBuilder) ensureClass(name string, tb *bamlparser.TypeBlock) error {
	if _, done := b.classes[name]; done {
		return nil
	}
	if b.classVisiting[name] {
		return fmt.Errorf("recursive class %q is not supported yet (slice 5)", name)
	}
	if len(tb.Attributes) > 0 {
		return fmt.Errorf("class %q carries block attribute @@%s (dynamic/attribute classes are not supported yet)", name, tb.Attributes[0].Name)
	}
	if tb.HasUnsupportedContent {
		return fmt.Errorf("class %q has unsupported body content (methods or nested blocks)", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("class %q has a named-argument list (parameterized classes are not supported)", name)
	}

	b.classVisiting[name] = true
	defer delete(b.classVisiting, name)

	fields := make([]sd.ClassField, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type == nil {
			return fmt.Errorf("class %q field %q has no type", name, m.Name)
		}
		if len(m.Attributes) > 0 {
			return fmt.Errorf("class %q field %q carries attribute @%s (schema attributes are not supported yet — slices 3/4)", name, m.Name, m.Attributes[0].Name)
		}
		ft, err := b.lowerType(m.Type)
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		fields = append(fields, sd.ClassField{
			Name: sd.Name{Name: m.Name},
			Type: ft,
		})
	}

	b.classes[name] = sd.ClassDef{
		Name:   sd.Name{Name: name},
		Mode:   sd.NonStreaming,
		Fields: fields,
	}
	b.classOrder = append(b.classOrder, name)
	return nil
}

// ensureEnum lowers an enum definition into the collected set exactly once. It
// declines block attributes (@@dynamic etc.), unsupported body content, named
// -argument enum blocks, and attribute-bearing values (@alias/@description/
// @skip).
func (b *descriptorBuilder) ensureEnum(name string, tb *bamlparser.TypeBlock) error {
	if _, done := b.enums[name]; done {
		return nil
	}
	if len(tb.Attributes) > 0 {
		return fmt.Errorf("enum %q carries block attribute @@%s (dynamic/attribute enums are not supported yet)", name, tb.Attributes[0].Name)
	}
	if tb.HasUnsupportedContent {
		return fmt.Errorf("enum %q has unsupported body content", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("enum %q has a named-argument list (parameterized enums are not supported)", name)
	}

	values := make([]sd.EnumValue, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type != nil {
			return fmt.Errorf("enum %q value %q unexpectedly carries a type", name, m.Name)
		}
		if len(m.Attributes) > 0 {
			return fmt.Errorf("enum %q value %q carries attribute @%s (schema attributes are not supported yet — slices 3/4)", name, m.Name, m.Attributes[0].Name)
		}
		values = append(values, sd.EnumValue{Name: sd.Name{Name: m.Name}})
	}

	b.enums[name] = sd.EnumDef{
		Name:   sd.Name{Name: name},
		Values: values,
	}
	b.enumOrder = append(b.enumOrder, name)
	return nil
}

// resolveAlias inlines a non-recursive alias by lowering its right-hand side in
// place (BAML substitutes non-recursive aliases). It declines alias attributes
// (slice 2 supports none, including the @check/@assert BAML permits on
// aliases), an unparsed RHS, and recursion (an alias re-entered while on the
// active path — a structural recursive alias is slice 5).
func (b *descriptorBuilder) resolveAlias(name string, alias *bamlparser.TypeAlias) (sd.Type, error) {
	if b.aliasVisiting[name] {
		return sd.Type{}, fmt.Errorf("recursive type alias %q is not supported yet (slice 5)", name)
	}
	if len(alias.Attributes) > 0 {
		return sd.Type{}, fmt.Errorf("type alias %q carries attribute @%s (alias attributes are not supported yet — slice 4)", name, alias.Attributes[0].Name)
	}
	if alias.Expr == nil {
		return sd.Type{}, fmt.Errorf("type alias %q has an unparsed right-hand side", name)
	}

	b.aliasVisiting[name] = true
	defer delete(b.aliasVisiting, name)
	return b.lowerType(alias.Expr)
}
