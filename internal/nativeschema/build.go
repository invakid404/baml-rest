// Package nativeschema is the de-BAML P3 native static-schema builder (issue
// #586). It consumes ONLY the native BAML AST produced by
// bamlutils/bamlparser (slice 1) plus baml-rest's own descriptor model — no
// BAML runtime, no generated client. For every function that declares an
// output type it builds a per-function bamlutils/schemadescriptor.Bundle,
// lowers it with internal/schema.FromStaticDescriptor, validates it with
// Bundle.ValidateOutput, and emits its enum/class slices in BAML
// output-format reachability order. A function is SUPPORTED only when its
// WHOLE output graph lowers, validates, and orders; otherwise it is DECLINED
// fail-closed with a stable reason and no descriptor is emitted for it.
//
// This package was extracted from cmd/introspect (slice 2 lived there) so the
// same builder can be driven both by the production introspect pipeline and by
// the byte-parity integration harness (slice 3), which needs to build a
// descriptor from parsed .baml bytes and render it. cmd/introspect calls
// BuildStaticSchemas and stores the results on *bamlConfig; nothing is wired
// into the request/response runtime.
//
// EMBED-REGEN / BUILD SITE (read before adding a file here): cmd/introspect
// imports this package, and the container build compiles cmd/introspect in
// PACKAGE form (`go run ./cmd/introspect`, see cmd/build/build.sh), so every
// import of cmd/introspect must ship in the customer/container embed.FS. When
// you add a .go file to this package, run `go run cmd/embed/main.go` and commit
// the regenerated root embed.go so the new file is embedded; otherwise the
// container introspection step fails to compile a sibling symbol as
// "undefined". See #586 and the slice-2 build fix.
//
// SCOPE (slice 3 — structural build + reachability ordering + alias/description):
//   - Lower primitives, named class/enum refs, non-recursive aliases (inlined),
//     lists, maps, optionals, unions, and string/int/bool literals.
//   - Emit Enums/Classes in BAML output-format reachability order (see
//     [descriptorBuilder.reorderByReachability]); FromStaticDescriptor preserves
//     order verbatim, so the STORED descriptor must already be in BAML order for
//     the renderer to match byte-for-byte.
//   - Lower @alias -> Name.Alias and @description -> Description on class fields,
//     enum values, and class/enum-level block attributes. Constraints
//     (@assert/@check), streaming (@stream.*), @@dynamic, and @skip stay
//     DECLINED (slices 4-6).
//   - Validate STRUCTURALLY via FromStaticDescriptor + ValidateOutput.
//
// Lax-vs-BAML / decline decisions logged here are also recorded on #586:
//
//	D1. Only @alias and @description are lowered from a reachable output node
//	    (slice 3). ANY OTHER attribute (@assert/@check/@stream.*/@@dynamic/@skip,
//	    or an unknown attribute) still DECLINES the function fail-closed rather
//	    than being silently dropped. This is a parity-decline, not input laxness.
//	D2. media and tuple output are lowered FAITHFULLY into the descriptor and
//	    left for ValidateOutput to reject (it errors on tuple/arrow/top/media),
//	    so the single authority on output-legality stays internal/schema.
//	D3. duplicate class/enum/alias names are POISONED in the semantic index; a
//	    function whose output graph reaches a poisoned name declines, while
//	    functions that never reach it still build. Per-method fail-closed.
//	D4. duplicate function names are last-wins (a later declaration's build
//	    outcome overrides an earlier one), matching the config walker.
//	D5. @alias/@description arguments must be a SINGLE plain string (a quoted
//	    string or a raw string). A missing, multi-valued, or non-string argument
//	    (e.g. a Jinja description) declines the function — slice 3 renders the
//	    description verbatim, so an argument it cannot reproduce byte-for-byte
//	    fails closed rather than guessing. Slice 4 revisits richer descriptions.
//	D6. /// doc comments are NOT captured as descriptions yet: the shared lexer
//	    elides all //-prefixed comments, so the builder never sees them. Class/
//	    enum/field/value descriptions come from @description only in slice 3.
//	    Capturing /// needs a lexer change gated on byte-identical config
//	    goldens; deferred to a fast-follow. Logged on #586.
package nativeschema

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// SourceFile pairs a parsed .baml [bamlparser.File] with its raw source bytes.
// The builder needs the source to detect `///` doc comments by declaration
// span (see #586 D6): BAML lowers `///` to a rendered Description, but the
// shared lexer elides all `//`-prefixed comments so the AST never retains them.
// Carrying the source lets the builder DECLINE fail-closed on a reachable
// doc-commented node instead of silently emitting a descriptor with a missing
// description.
type SourceFile struct {
	File   *bamlparser.File
	Source []byte
}

// BuildStaticSchemas builds a native static output-schema descriptor for every
// function that has a parsed return type, from the already-parsed .baml files.
// It returns two always-non-nil maps keyed by function name: bundles for the
// SUPPORTED functions (whose whole output graph lowered, validated, and
// ordered) and declines for the unsupported ones (a stable reason string). A
// declined function never appears in bundles, and a supported one never in
// declines.
//
// It never errors: an output graph that cannot be represented faithfully is a
// per-function decline, not a pipeline failure, so this pass is safe to run on
// the production introspect path without changing any config extraction.
func BuildStaticSchemas(files []SourceFile) (map[string]sd.Bundle, map[string]string) {
	idx := buildSchemaTypeIndex(files)

	bundles := make(map[string]sd.Bundle)
	declines := make(map[string]string)

	for _, sf := range files {
		f := sf.File
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
	classes   map[string]*typeDecl
	enums     map[string]*typeDecl
	aliases   map[string]*bamlparser.TypeAlias
	ambiguous map[string]struct{}
}

// typeDecl is a class/enum declaration plus the raw source of its file, so the
// builder can inspect the bytes around the declaration span (for `///` doc
// comment detection).
type typeDecl struct {
	tb  *bamlparser.TypeBlock
	src []byte
}

func (i *schemaTypeIndex) isAmbiguous(name string) bool {
	_, ok := i.ambiguous[name]
	return ok
}

// buildSchemaTypeIndex classifies all class/enum/alias names first. `test`
// blocks are not schema definitions and are skipped, matching the parser's
// metadata-only posture for them.
func buildSchemaTypeIndex(files []SourceFile) *schemaTypeIndex {
	idx := &schemaTypeIndex{
		classes:   make(map[string]*typeDecl),
		enums:     make(map[string]*typeDecl),
		aliases:   make(map[string]*bamlparser.TypeAlias),
		ambiguous: make(map[string]struct{}),
	}

	counts := make(map[string]int)
	for _, sf := range files {
		f := sf.File
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			switch {
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "class":
				counts[it.TypeBlock.Name]++
				idx.classes[it.TypeBlock.Name] = &typeDecl{tb: it.TypeBlock, src: sf.Source}
			case it.TypeBlock != nil && it.TypeBlock.Name != "" && it.TypeBlock.Keyword == "enum":
				counts[it.TypeBlock.Name]++
				idx.enums[it.TypeBlock.Name] = &typeDecl{tb: it.TypeBlock, src: sf.Source}
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
// returned once FromStaticDescriptor and ValidateOutput both accept it and its
// definitions have been reordered into BAML output-format order.
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
		// Discovery order for now; reorderByReachability below rewrites these
		// into BAML output-format order once the graph is lowered + validated.
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

	// SLICE 3 ORDERING: rewrite the enum/class slices into BAML's
	// render_output_format reachability order (reverse-of-reference DFS from the
	// target). FromStaticDescriptor preserves the descriptor's slice order
	// verbatim and never reruns reachability, so the STORED descriptor must
	// already be in BAML order for internal/schema/outputformat.Render to match
	// BAML byte-for-byte. The order is computed by the SAME orderByReachability
	// the dynamic path uses (via Bundle.ReachableOrder), so the two paths cannot
	// drift. Validation above is order-independent, so reordering a validated
	// bundle keeps it valid.
	b.reorderByReachability(&bundle, internal)
	return bundle, nil
}

// descriptorBuilder lowers one function's output graph. It is single-use: a
// fresh builder is created per function so the recursion-guard and collected
// definition sets are scoped to that function's reachable graph.
type descriptorBuilder struct {
	index *schemaTypeIndex

	// classes/enums are the collected definitions (the reachable set), keyed by
	// canonical name and deduplicated. classOrder/enumOrder record discovery
	// order (dependencies first, post-order); reorderByReachability rewrites the
	// emitted order into BAML output-format order.
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

// reorderByReachability rewrites bundle.Enums and bundle.Classes into the BAML
// output-format hoist order reported by the validated internal bundle. The
// builder collected exactly the reachable definition set (it only lowers what a
// reachable reference names), so ReachableOrder returns each collected name once
// and in BAML order; every returned name therefore resolves in the builder's
// by-name maps. Slice 3 never emits streaming classes, so a class key's mode is
// always NonStreaming and looking classes up by canonical name is sufficient.
func (b *descriptorBuilder) reorderByReachability(bundle *sd.Bundle, internal *schema.Bundle) {
	enumNames, classKeys := internal.ReachableOrder()

	if len(enumNames) == 0 {
		bundle.Enums = nil
	} else {
		enums := make([]sd.EnumDef, 0, len(enumNames))
		for _, name := range enumNames {
			if def, ok := b.enums[name]; ok {
				enums = append(enums, def)
			}
		}
		bundle.Enums = enums
	}

	if len(classKeys) == 0 {
		bundle.Classes = nil
	} else {
		classes := make([]sd.ClassDef, 0, len(classKeys))
		for _, key := range classKeys {
			if def, ok := b.classes[key.Name]; ok {
				classes = append(classes, def)
			}
		}
		bundle.Classes = classes
	}
}

// lowerType lowers one parsed TypeExpr into a descriptor Type, collecting any
// reachable class/enum definitions along the way. It returns a decline error
// for any construct slice 3 cannot represent faithfully.
func (b *descriptorBuilder) lowerType(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t == nil {
		return sd.Type{}, fmt.Errorf("missing type expression")
	}

	// D1: attributes on a type node decline the function. @alias/@description
	// that belong to an enclosing class field or enum value are consumed and
	// STRIPPED by the member handler before it calls lowerFieldType, so by the
	// time lowerType sees a node with attributes, they are either constraints/
	// streaming/unknown attributes (declined here) or metadata on a NESTED type
	// (which slice 3 does not support — declined here fail-closed rather than
	// silently dropped).
	if len(t.Attributes) > 0 {
		return sd.Type{}, declineAttribute(t.Attributes[0])
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

// lowerFieldType lowers a class-field type after its field-level metadata
// (@alias/@description) has been consumed by the caller ([extractMemberMeta],
// which declines any non-metadata attribute). The remaining attributes on the
// OUTERMOST node are therefore all metadata; they are dropped via a shallow
// copy (never mutating the shared AST, which other functions also reference)
// before lowering. Nested-node attributes are untouched and still decline in
// lowerType.
func (b *descriptorBuilder) lowerFieldType(t *bamlparser.TypeExpr) (sd.Type, error) {
	if t == nil {
		return sd.Type{}, fmt.Errorf("missing type expression")
	}
	if len(t.Attributes) > 0 {
		cp := *t
		cp.Attributes = nil
		return b.lowerType(&cp)
	}
	return b.lowerType(t)
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
// Slice 3 never attaches constraints/streaming metadata to a union node (any
// attribute-bearing node is declined by D1), so every lowered union here is
// metadata-free and flattens fully — the constraint-carrying-union special case
// BAML preserves cannot arise yet.
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
// slice-3 path reduces to a deep comparison of the descriptor type trees.
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

	if decl, ok := b.index.classes[name]; ok {
		if err := b.ensureClass(name, decl); err != nil {
			return sd.Type{}, err
		}
		return sd.Type{Kind: sd.TypeClass, Name: name, Mode: sd.NonStreaming}, nil
	}
	if decl, ok := b.index.enums[name]; ok {
		if err := b.ensureEnum(name, decl); err != nil {
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
// It declines unsupported body content (methods / nested blocks), named-argument
// (generic) class blocks, and recursion (a class re-entered while on the active
// path). Class-level @@alias/@@description block attributes lower to the class's
// alias/description; any other block attribute (@@dynamic, ...) declines. Field
// @alias/@description lower to the field's alias/description; any other field
// attribute declines.
func (b *descriptorBuilder) ensureClass(name string, decl *typeDecl) error {
	if _, done := b.classes[name]; done {
		return nil
	}
	if b.classVisiting[name] {
		return fmt.Errorf("recursive class %q is not supported yet (slice 5)", name)
	}
	tb := decl.tb
	if tb.HasUnsupportedContent {
		return fmt.Errorf("class %q has unsupported body content (methods or nested blocks)", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("class %q has a named-argument list (parameterized classes are not supported)", name)
	}
	// D6: a `///` doc comment on the class renders a Description in BAML, which
	// slice 3 does not capture — decline fail-closed rather than emit a
	// descriptor with a missing description.
	if hasDocCommentBefore(decl.src, tb.Span.Start) {
		return fmt.Errorf("class %q carries a /// doc comment (doc-comment descriptions are not supported yet — see #586 D6)", name)
	}

	classAlias, classDesc, err := extractBlockMeta(tb.Attributes)
	if err != nil {
		return fmt.Errorf("class %q: %w", name, err)
	}

	b.classVisiting[name] = true
	defer delete(b.classVisiting, name)

	fields := make([]sd.ClassField, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type == nil {
			return fmt.Errorf("class %q field %q has no type", name, m.Name)
		}
		if hasDocCommentBefore(decl.src, m.Span.Start) {
			return fmt.Errorf("class %q field %q carries a /// doc comment (doc-comment descriptions are not supported yet — see #586 D6)", name, m.Name)
		}
		alias, desc, err := extractMemberMeta(memberAttributes(m))
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		ft, err := b.lowerFieldType(m.Type)
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		fields = append(fields, sd.ClassField{
			Name:        sd.Name{Name: m.Name, Alias: alias},
			Type:        ft,
			Description: desc,
		})
	}

	b.classes[name] = sd.ClassDef{
		Name:        sd.Name{Name: name, Alias: classAlias},
		Description: classDesc,
		Mode:        sd.NonStreaming,
		Fields:      fields,
	}
	b.classOrder = append(b.classOrder, name)
	return nil
}

// ensureEnum lowers an enum definition into the collected set exactly once. It
// declines unsupported body content and named-argument enum blocks. Enum-level
// @@alias/@@description block attributes lower to the enum's alias/description;
// any other block attribute (@@dynamic, ...) declines. Enum-value @alias/
// @description lower to the value's alias/description; any other value attribute
// (@skip, ...) declines.
func (b *descriptorBuilder) ensureEnum(name string, decl *typeDecl) error {
	if _, done := b.enums[name]; done {
		return nil
	}
	tb := decl.tb
	if tb.HasUnsupportedContent {
		return fmt.Errorf("enum %q has unsupported body content", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("enum %q has a named-argument list (parameterized enums are not supported)", name)
	}
	// D6: a `///` doc comment on the enum renders a Description in BAML, which
	// slice 3 does not capture — decline fail-closed.
	if hasDocCommentBefore(decl.src, tb.Span.Start) {
		return fmt.Errorf("enum %q carries a /// doc comment (doc-comment descriptions are not supported yet — see #586 D6)", name)
	}

	enumAlias, enumDesc, err := extractBlockMeta(tb.Attributes)
	if err != nil {
		return fmt.Errorf("enum %q: %w", name, err)
	}
	// The descriptor model (and BAML's OutputFormatContent) has no enum-LEVEL
	// description — only enum VALUES carry one, and BAML renders no enum-level
	// description in ctx.output_format. An @@description on an enum is therefore
	// not representable; decline fail-closed rather than silently drop it.
	if enumDesc != nil {
		return fmt.Errorf("enum %q carries @@description, which has no rendered representation (only enum values carry descriptions)", name)
	}

	values := make([]sd.EnumValue, 0, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Type != nil {
			return fmt.Errorf("enum %q value %q unexpectedly carries a type", name, m.Name)
		}
		if hasDocCommentBefore(decl.src, m.Span.Start) {
			return fmt.Errorf("enum %q value %q carries a /// doc comment (doc-comment descriptions are not supported yet — see #586 D6)", name, m.Name)
		}
		alias, desc, err := extractMemberMeta(m.Attributes)
		if err != nil {
			return fmt.Errorf("enum %q value %q: %w", name, m.Name, err)
		}
		values = append(values, sd.EnumValue{
			Name:        sd.Name{Name: m.Name, Alias: alias},
			Description: desc,
		})
	}

	b.enums[name] = sd.EnumDef{
		Name:   sd.Name{Name: name, Alias: enumAlias},
		Values: values,
	}
	b.enumOrder = append(b.enumOrder, name)
	return nil
}

// resolveAlias inlines a non-recursive alias by lowering its right-hand side in
// place (BAML substitutes non-recursive aliases). It declines alias attributes
// (slice 3 supports none, including the @check/@assert BAML permits on
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

// memberAttributes returns the field-level attributes of a class member: the
// member's own attributes plus the trailing field attributes the slice-1 parser
// attaches to the OUTERMOST type node (it does not reassociate field-vs-type
// attributes, so @alias/@description on `name string @alias("x")` land on the
// string node). Both are field metadata; nested-node attributes are NOT
// included (they decline in lowerType).
func memberAttributes(m *bamlparser.TypeMember) []*bamlparser.Attribute {
	if m.Type == nil || len(m.Type.Attributes) == 0 {
		return m.Attributes
	}
	if len(m.Attributes) == 0 {
		return m.Type.Attributes
	}
	out := make([]*bamlparser.Attribute, 0, len(m.Attributes)+len(m.Type.Attributes))
	out = append(out, m.Attributes...)
	out = append(out, m.Type.Attributes...)
	return out
}

// extractMemberMeta partitions a field's / enum value's attributes into the
// slice-3-supported metadata (@alias, @description) and everything else. It
// returns the alias and description (nil when absent) and DECLINES fail-closed
// on any other attribute (@assert/@check/@stream.*/@skip/unknown) or a repeated
// @alias/@description. These are single-@ FIELD attributes; a stray @@ block
// attribute here is malformed and also declines.
func extractMemberMeta(attrs []*bamlparser.Attribute) (alias, description *string, err error) {
	for _, a := range attrs {
		if a.Block {
			return nil, nil, declineAttribute(a)
		}
		switch a.Name {
		case "alias":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if alias != nil {
				return nil, nil, fmt.Errorf("duplicate @alias attribute")
			}
			alias = &v
		case "description":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if description != nil {
				return nil, nil, fmt.Errorf("duplicate @description attribute")
			}
			description = &v
		default:
			return nil, nil, declineAttribute(a)
		}
	}
	return alias, description, nil
}

// extractBlockMeta is extractMemberMeta's counterpart for class/enum-level block
// attributes (@@alias, @@description). Any other block attribute (@@dynamic, a
// stray non-block attribute, an unknown block attribute) declines fail-closed.
func extractBlockMeta(attrs []*bamlparser.Attribute) (alias, description *string, err error) {
	for _, a := range attrs {
		if !a.Block {
			return nil, nil, declineAttribute(a)
		}
		switch a.Name {
		case "alias":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if alias != nil {
				return nil, nil, fmt.Errorf("duplicate @@alias attribute")
			}
			alias = &v
		case "description":
			v, e := attributeStringArg(a)
			if e != nil {
				return nil, nil, e
			}
			if description != nil {
				return nil, nil, fmt.Errorf("duplicate @@description attribute")
			}
			description = &v
		default:
			return nil, nil, declineAttribute(a)
		}
	}
	return alias, description, nil
}

// attributeStringArg extracts the single plain-string argument of an @alias/
// @description attribute (a quoted string or a raw string; both arrive with
// their delimiters stripped by the parser's normalization pass). D5: a missing,
// multi-valued, or non-string argument declines — slice 3 renders the alias/
// description verbatim, so an argument it cannot reproduce byte-for-byte fails
// closed rather than guessing.
func attributeStringArg(a *bamlparser.Attribute) (string, error) {
	if len(a.Args) != 1 {
		return "", fmt.Errorf("@%s expects a single string argument", attrDisplayName(a))
	}
	v := a.Args[0]
	switch {
	case v.Literal != nil:
		return *v.Literal, nil
	case v.Raw != nil:
		return *v.Raw, nil
	default:
		return "", fmt.Errorf("@%s argument must be a plain string", attrDisplayName(a))
	}
}

// hasDocCommentBefore reports whether the physical source line immediately
// above the declaration at byte offset `start` is a BAML `///` doc comment.
//
// BAML lowers `///` to a Description that renders in ctx.output_format, but the
// shared lexer elides all `//`-prefixed comments (#586 D6), so the AST never
// retains them. Rather than silently emit a descriptor with a MISSING
// description — the wrong failure mode for a parity slice — the builder uses
// this DETECTION (not capture) to DECLINE fail-closed any reachable class /
// field / enum / enum-value that carries a doc comment. Capturing `///` and
// lowering it to a Description is a documented fast-follow.
//
// It inspects only the single physical line directly above the declaration:
// BAML doc comments sit immediately above (no blank line in between), and a
// multi-line `///` block still has a `///` line directly above the
// declaration, so one line is sufficient for presence detection. A `///`
// prefix (three-or-more slashes) is a doc comment; a bare `//` is an ordinary
// comment and is ignored, matching BAML's doc_comment-before-comment grammar.
func hasDocCommentBefore(src []byte, start int) bool {
	if start <= 0 || start > len(src) {
		return false
	}
	// Rewind to the start of the declaration's own line.
	lineStart := start
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	if lineStart == 0 {
		return false // declaration is on the first line; nothing precedes it
	}
	// The preceding physical line is [prevStart, prevEnd), where prevEnd is the
	// '\n' that terminates it (at lineStart-1).
	prevEnd := lineStart - 1
	prevStart := prevEnd
	for prevStart > 0 && src[prevStart-1] != '\n' {
		prevStart--
	}
	return strings.HasPrefix(strings.TrimSpace(string(src[prevStart:prevEnd])), "///")
}

// declineAttribute produces the stable per-function decline error for an
// attribute slice 3 does not lower. The substrings "attribute" (field) and
// "block attribute" (block) are load-bearing for the decline-reason tests.
func declineAttribute(a *bamlparser.Attribute) error {
	if a.Block {
		return fmt.Errorf("block attribute @@%s is not supported yet (slice 3 lowers only @@alias/@@description; @@dynamic and other block attributes are declined)", a.Name)
	}
	return fmt.Errorf("attribute @%s is not supported yet (slice 3 lowers only @alias/@description; constraints, streaming, and @skip are declined)", a.Name)
}

// attrDisplayName renders an attribute's name with its @/@@ sigil for messages.
func attrDisplayName(a *bamlparser.Attribute) string {
	if a.Block {
		return "@" + a.Name
	}
	return a.Name
}
