package schema

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// FromStaticDescriptor lowers a public [schemadescriptor.Bundle] — the stable
// contract a BAML-side generator emits — into an internal [Bundle].
//
// The two type sets are deliberately independent (the descriptor mirrors this
// model without importing it), so lowering re-validates everything on the way
// in and fails closed on anything it does not recognise:
//
//   - the descriptor Version must equal [schemadescriptor.Version]; an
//     unknown version is rejected before any structural work;
//   - every descriptor enum string (type kind, primitive, media subtype,
//     literal kind, streaming mode, constraint level) is switched over
//     explicitly and an unknown value is an error — no descriptor enum is
//     ever cast blindly into an internal constant;
//   - every slice and pointer is DEEP-COPIED into a fresh [Bundle]; the
//     result shares no memory with the descriptor, so a caller mutating the
//     descriptor afterwards cannot perturb the lowered bundle;
//   - the descriptor's enum/class/recursive-alias order is preserved verbatim
//     ([orderByReachability] is NOT run): the descriptor is already emitted in
//     BAML output-format order, so recomputing reachability here would only
//     risk diverging from it;
//   - nil-vs-present aliases, descriptions, and constraint labels are
//     preserved exactly — an empty string is never collapsed to nil, and nil
//     is never materialised into an empty string;
//   - constraints and streaming behavior are carried opaquely: expressions
//     are never parsed or evaluated (only the constraint level enum is
//     checked);
//   - malformed payloads are rejected: a list/map/arrow/union/literal missing
//     its required child, a null primitive appearing as a union variant, a
//     media primitive without a valid subtype, an enum/class/recursive-alias
//     reference with an empty name, and an unknown constraint level.
//
// Finally [Bundle.Validate] runs, enforcing the cross-reference, uniqueness,
// and map-key invariants (dangling refs, duplicate rendered field/value
// names, null-only-via-Nullable, jsonish-compatible map keys). Validate is
// structural: it does NOT reject the output-illegal kinds (tuple/arrow/top/
// media), so a descriptor carrying them lowers successfully here — a caller
// destined for rendering or parse-resolution must additionally run
// [Bundle.ValidateOutput]. The [BuildOptions.Resolver] seam is intentionally
// unused: a static descriptor is self-contained, so every reference must
// resolve within the bundle.
//
// The descriptor's envelope metadata ([schemadescriptor.Bundle.Method] and
// [schemadescriptor.Bundle.Stream]) has no counterpart in the internal
// [Bundle] and is not carried here; it is host registry state, consumed by
// the caller of [StaticBundlesFromDescriptors], not by type lowering.
func FromStaticDescriptor(d schemadescriptor.Bundle) (*Bundle, error) {
	if d.Version != schemadescriptor.Version {
		return nil, fmt.Errorf("schema: unsupported static descriptor version %d (want %d)", d.Version, schemadescriptor.Version)
	}

	// One guard for the whole bundle. It tracks the descriptor pointers on the
	// ACTIVE recursion path (with backtracking), so an inline pointer cycle in
	// any type tree is caught before it can recurse to a stack overflow, while
	// legitimate cross-tree pointer aliasing (a DAG) is not falsely rejected.
	g := newCycleGuard()

	target, err := lowerDescriptorType(d.Target, "target", g)
	if err != nil {
		return nil, err
	}

	enums, err := lowerDescriptorEnums(d.Enums)
	if err != nil {
		return nil, err
	}

	classes, err := lowerDescriptorClasses(d.Classes, g)
	if err != nil {
		return nil, err
	}

	aliases, err := lowerDescriptorAliases(d.StructuralRecursiveAliases, g)
	if err != nil {
		return nil, err
	}

	bundle := &Bundle{
		Target:                     target,
		Enums:                      enums,
		Classes:                    classes,
		RecursiveClasses:           cloneStrings(d.RecursiveClasses),
		StructuralRecursiveAliases: aliases,
	}

	// Validate rebuilds the indexes and enforces the remaining invariants
	// (self-containment, uniqueness, map keys, null-via-Nullable). Only the
	// structural profile is run: tuple/arrow/top/media lower successfully and
	// are the caller's concern via ValidateOutput.
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// StaticBundlesFromDescriptors lowers a keyed set of descriptor constructors,
// partitioning the results: successfully lowered bundles keyed the same as
// the input, and per-key errors for the ones that failed (unknown version,
// malformed payload, unresolved reference, ...). Both maps are always
// non-nil. A nil constructor is itself a per-key error rather than a panic.
//
// Callers register descriptors lazily via a func() so a malformed one is a
// localised error at lowering time, not an init-time panic; the (method,
// stream) envelope a host keys on lives on each [schemadescriptor.Bundle],
// while the map key here is whatever identity the caller chose.
func StaticBundlesFromDescriptors(in map[string]func() schemadescriptor.Bundle) (map[string]*Bundle, map[string]error) {
	bundles := make(map[string]*Bundle, len(in))
	errs := make(map[string]error)
	for key, ctor := range in {
		if ctor == nil {
			errs[key] = fmt.Errorf("schema: %q: nil descriptor constructor", key)
			continue
		}
		bundle, err := FromStaticDescriptor(ctor())
		if err != nil {
			errs[key] = fmt.Errorf("schema: %q: %w", key, err)
			continue
		}
		bundles[key] = bundle
	}
	return bundles, errs
}

// lowerDescriptorType recursively lowers one descriptor [schemadescriptor.Type]
// node into an internal [Type], validating every enum string and required
// child pointer for its kind. It:
//
//   - rejects any payload child field that does not belong to the node's Kind
//     (a stray field is a generator-drift signal, not silently dropped); and
//   - descends inline type pointers (Elem/Key/Value/Union/Arrow) through the
//     cycle guard g, so an inline pointer cycle fails closed instead of
//     recursing to a stack overflow.
func lowerDescriptorType(t schemadescriptor.Type, path string, g *cycleGuard) (Type, error) {
	kind, err := lowerTypeKind(t.Kind, path)
	if err != nil {
		return Type{}, err
	}

	// Reject child fields incompatible with the resolved kind before doing any
	// kind-specific work, so an accepted node carries exactly the payload its
	// kind defines.
	if err := rejectStrayPayload(t, kind, path); err != nil {
		return Type{}, err
	}

	meta, err := lowerTypeMeta(t.Meta, path)
	if err != nil {
		return Type{}, err
	}
	out := Type{Kind: kind, Meta: meta}

	switch kind {
	case TypeTop:
		// No payload beyond meta.

	case TypePrimitive:
		prim, err := lowerPrimitiveKind(t.Primitive, path)
		if err != nil {
			return Type{}, err
		}
		out.Primitive = prim
		if prim == PrimitiveMedia {
			media, err := lowerMediaKind(t.Media, path)
			if err != nil {
				return Type{}, err
			}
			out.Media = media
		}

	case TypeEnum:
		if t.Name == "" {
			return Type{}, fmt.Errorf("schema: %s: enum reference has empty name", path)
		}
		out.Name = t.Name
		out.Dynamic = t.Dynamic

	case TypeClass:
		if t.Name == "" {
			return Type{}, fmt.Errorf("schema: %s: class reference has empty name", path)
		}
		mode, err := lowerStreamingMode(t.Mode, path)
		if err != nil {
			return Type{}, fmt.Errorf("schema: %s: class reference %q: %w", path, t.Name, err)
		}
		out.Name = t.Name
		out.Mode = mode
		out.Dynamic = t.Dynamic

	case TypeRecursiveAlias:
		if t.Name == "" {
			return Type{}, fmt.Errorf("schema: %s: recursive alias reference has empty name", path)
		}
		mode, err := lowerStreamingMode(t.Mode, path)
		if err != nil {
			return Type{}, fmt.Errorf("schema: %s: recursive alias reference %q: %w", path, t.Name, err)
		}
		out.Name = t.Name
		out.Mode = mode

	case TypeLiteral:
		if t.Literal == nil {
			return Type{}, fmt.Errorf("schema: %s: literal type missing value", path)
		}
		lit, err := lowerLiteral(*t.Literal, path)
		if err != nil {
			return Type{}, err
		}
		out.Literal = &lit

	case TypeList:
		if t.Elem == nil {
			return Type{}, fmt.Errorf("schema: %s: list type missing element", path)
		}
		elem, err := g.descendType(t.Elem, path+"[]")
		if err != nil {
			return Type{}, err
		}
		out.Elem = &elem

	case TypeMap:
		if t.Key == nil || t.Value == nil {
			return Type{}, fmt.Errorf("schema: %s: map type requires key and value", path)
		}
		key, err := g.descendType(t.Key, path+"[key]")
		if err != nil {
			return Type{}, err
		}
		val, err := g.descendType(t.Value, path+"[value]")
		if err != nil {
			return Type{}, err
		}
		out.Key = &key
		out.Value = &val

	case TypeUnion:
		if t.Union == nil {
			return Type{}, fmt.Errorf("schema: %s: union type missing payload", path)
		}
		union, err := g.descendUnion(t.Union, path)
		if err != nil {
			return Type{}, err
		}
		out.Union = &union

	case TypeTuple:
		items, err := lowerTypeSlice(t.Items, path, ".", "tuple items", g)
		if err != nil {
			return Type{}, err
		}
		out.Items = items

	case TypeArrow:
		if t.Arrow == nil {
			return Type{}, fmt.Errorf("schema: %s: arrow type missing payload", path)
		}
		arrow, err := g.descendArrow(t.Arrow, path)
		if err != nil {
			return Type{}, err
		}
		out.Arrow = &arrow
	}

	return out, nil
}

// cycleGuard tracks, on the ACTIVE recursion path, every descriptor container a
// malformed descriptor could reuse to recurse forever, so an inline cycle is
// caught before it overflows the stack. Two kinds of identity are tracked:
//
//   - the child POINTERS *Type / *UnionType / *ArrowType (Elem/Key/Value on a
//     type, plus the Union and Arrow payload pointers). All three are needed
//     because a cycle can close through any of them — e.g. a union whose
//     variant's *UnionType is the same pointer, with no *Type in between; and
//   - the RANGE of a []Type VALUE slice (Type.Items and ArrowType.Params).
//     These are not reached through a tracked pointer, so a descriptor whose
//     Items/Params aliases an ancestor's slice range would otherwise recurse
//     unbounded WITHOUT ever repeating a tracked pointer. The range is keyed by
//     ([sliceKey]) the address of its first element AND its length — the head
//     alone conflates distinct sub-ranges of one backing array (e.g. shared and
//     shared[:1]), which would falsely reject a finite prefix-sharing DAG. Two
//     slices with the same (head, len) alias the exact same elements, so a
//     re-encounter is a genuine cycle; a different-length sub-range is not.
//
// UnionType.Variants is a []Type value slice too, but it is reached only
// through its *UnionType (already tracked), and any tuple/arrow that reuses its
// backing array trips the slice guard the moment lowerTypeSlice iterates it —
// so Variants needs no separate registration.
//
// Descent adds an identity and defer-removes it on the way back up, so only
// ancestors-on-the-path (true cycles) trip the guard; sibling reuse of the same
// pointer or slice range (a finite DAG) does not.
type cycleGuard struct {
	types  map[*schemadescriptor.Type]struct{}
	unions map[*schemadescriptor.UnionType]struct{}
	arrows map[*schemadescriptor.ArrowType]struct{}
	slices map[sliceKey]struct{}
}

// sliceKey identifies a []Type range by its backing-array head and length. Both
// components are required: the head alone cannot distinguish a slice from a
// shorter prefix of the same backing array, so keying on the head alone would
// treat an acyclic prefix-sharing DAG as a cycle.
type sliceKey struct {
	head *schemadescriptor.Type
	n    int
}

func newCycleGuard() *cycleGuard {
	return &cycleGuard{
		types:  map[*schemadescriptor.Type]struct{}{},
		unions: map[*schemadescriptor.UnionType]struct{}{},
		arrows: map[*schemadescriptor.ArrowType]struct{}{},
		slices: map[sliceKey]struct{}{},
	}
}

// cycleErr formats the fail-closed inline-cycle error. Recursion must be
// expressed through named refs (recursive_alias / recursive_classes /
// structural_recursive_aliases), never inline pointer or slice cycles.
func cycleErr(path, via string) error {
	return fmt.Errorf("schema: %s: inline %s pointer cycle; express recursion via a named ref (recursive_alias / recursive_classes)", path, via)
}

// sliceCycleErr is cycleErr's []Type value-slice counterpart.
func sliceCycleErr(path, via string) error {
	return fmt.Errorf("schema: %s: inline %s slice cycle; express recursion via a named ref (recursive_alias / recursive_classes)", path, via)
}

func (g *cycleGuard) descendType(p *schemadescriptor.Type, path string) (Type, error) {
	if _, ok := g.types[p]; ok {
		return Type{}, cycleErr(path, "type")
	}
	g.types[p] = struct{}{}
	defer delete(g.types, p)
	return lowerDescriptorType(*p, path, g)
}

func (g *cycleGuard) descendUnion(p *schemadescriptor.UnionType, path string) (UnionType, error) {
	if _, ok := g.unions[p]; ok {
		return UnionType{}, cycleErr(path, "union")
	}
	g.unions[p] = struct{}{}
	defer delete(g.unions, p)
	return lowerUnion(*p, path, g)
}

func (g *cycleGuard) descendArrow(p *schemadescriptor.ArrowType, path string) (ArrowType, error) {
	if _, ok := g.arrows[p]; ok {
		return ArrowType{}, cycleErr(path, "arrow")
	}
	g.arrows[p] = struct{}{}
	defer delete(g.arrows, p)
	return lowerArrow(*p, path, g)
}

// payloadFieldOrder lists the kind-selected child fields of a descriptor Type
// in a stable order, so [rejectStrayPayload]'s error message is deterministic.
var payloadFieldOrder = []string{
	"primitive", "media", "name", "mode", "dynamic",
	"literal", "elem", "key", "value", "items", "union", "arrow",
}

// rejectStrayPayload fails closed when a descriptor Type carries a child field
// that does not belong to its Kind (e.g. a Media subtype on a non-media
// primitive, a Mode on an enum ref, or an Elem on a union). Silently dropping
// such fields would hide generator drift; the design requires malformed
// payloads to be rejected, so an incompatible field is an error naming the
// offending field and kind.
func rejectStrayPayload(t schemadescriptor.Type, kind TypeKind, path string) error {
	present := map[string]bool{}
	if t.Primitive != "" {
		present["primitive"] = true
	}
	if t.Media != "" {
		present["media"] = true
	}
	if t.Name != "" {
		present["name"] = true
	}
	if t.Mode != "" {
		present["mode"] = true
	}
	if t.Dynamic {
		present["dynamic"] = true
	}
	if t.Literal != nil {
		present["literal"] = true
	}
	if t.Elem != nil {
		present["elem"] = true
	}
	if t.Key != nil {
		present["key"] = true
	}
	if t.Value != nil {
		present["value"] = true
	}
	// Presence is nil-based for every pointer/slice child so a NON-nil but empty
	// payload (e.g. Items: []Type{}) on an incompatible kind is still rejected,
	// not silently dropped. Items is the only slice child; the string/bool
	// fields above have no nil state, so their zero value is the analogous
	// "absent". A legitimately empty tuple still lowers: "items" is allowed for
	// TypeTuple, so it is never flagged there.
	if t.Items != nil {
		present["items"] = true
	}
	if t.Union != nil {
		present["union"] = true
	}
	if t.Arrow != nil {
		present["arrow"] = true
	}

	allow := map[string]bool{}
	// A media primitive is the only kind whose allowed set is conditional: the
	// Media subtype is permitted only when the primitive is itself media.
	for _, f := range allowedPayloadFields(kind, t.Primitive == schemadescriptor.PrimitiveMedia) {
		allow[f] = true
	}

	for _, f := range payloadFieldOrder {
		if present[f] && !allow[f] {
			return fmt.Errorf("schema: %s: field %q is not valid for kind %q", path, f, kind)
		}
	}
	return nil
}

// allowedPayloadFields returns the child fields a given kind may carry. isMedia
// widens the primitive case to also permit the media subtype.
func allowedPayloadFields(kind TypeKind, isMedia bool) []string {
	switch kind {
	case TypeTop:
		return nil
	case TypePrimitive:
		if isMedia {
			return []string{"primitive", "media"}
		}
		return []string{"primitive"}
	case TypeEnum:
		return []string{"name", "dynamic"}
	case TypeClass:
		return []string{"name", "mode", "dynamic"}
	case TypeLiteral:
		return []string{"literal"}
	case TypeList:
		return []string{"elem"}
	case TypeMap:
		return []string{"key", "value"}
	case TypeRecursiveAlias:
		return []string{"name", "mode"}
	case TypeTuple:
		return []string{"items"}
	case TypeArrow:
		return []string{"arrow"}
	case TypeUnion:
		return []string{"union"}
	default:
		return nil
	}
}

// lowerUnion lowers a union payload, rejecting a null primitive that appears
// directly as a variant: BAML represents null solely via Nullable, so a null
// variant is a malformed descriptor (the same invariant [Bundle.Validate]
// enforces, checked here first for a precise, lowering-time error).
func lowerUnion(u schemadescriptor.UnionType, path string, g *cycleGuard) (UnionType, error) {
	var variants []Type
	if len(u.Variants) > 0 {
		variants = make([]Type, 0, len(u.Variants))
	}
	for i := range u.Variants {
		v := u.Variants[i]
		if v.Kind == schemadescriptor.TypePrimitive && v.Primitive == schemadescriptor.PrimitiveNull {
			return UnionType{}, fmt.Errorf("schema: %s: union variant %d is a null primitive; represent null via Nullable", path, i)
		}
		lowered, err := lowerDescriptorType(v, fmt.Sprintf("%s|%d", path, i), g)
		if err != nil {
			return UnionType{}, err
		}
		variants = append(variants, lowered)
	}
	return UnionType{Variants: variants, Nullable: u.Nullable}, nil
}

// lowerArrow lowers an arrow payload. The return type is a value (never a
// pointer), so a zero descriptor Return surfaces as an unknown-kind error
// from lowerTypeKind rather than a nil dereference.
func lowerArrow(a schemadescriptor.ArrowType, path string, g *cycleGuard) (ArrowType, error) {
	params, err := lowerTypeSlice(a.Params, path, ".param", "arrow params", g)
	if err != nil {
		return ArrowType{}, err
	}
	ret, err := lowerDescriptorType(a.Return, path+".return", g)
	if err != nil {
		return ArrowType{}, err
	}
	return ArrowType{Params: params, Return: ret}, nil
}

// lowerTypeSlice deep-copies a []Type value slice (tuple items / arrow params),
// preserving order. A nil or empty input yields a nil slice. It guards against
// a value-slice cycle by registering the backing array (keyed by the address of
// its first element) on the active recursion path: a descriptor whose
// Items/Params aliases an ancestor's backing array reaches this slice again and
// fails closed instead of recursing to a stack overflow. via names the carrier
// for the error message.
func lowerTypeSlice(in []schemadescriptor.Type, path, sep, via string, g *cycleGuard) ([]Type, error) {
	if len(in) == 0 {
		return nil, nil
	}

	// Key on the full range identity (backing head + length), not the head
	// alone: distinct sub-ranges of one backing array (e.g. shared vs
	// shared[:1]) are different keys and lower fine, while a re-entry into the
	// SAME (head, len) range — a genuine cycle — is still rejected.
	key := sliceKey{head: &in[0], n: len(in)}
	if _, ok := g.slices[key]; ok {
		return nil, sliceCycleErr(path, via)
	}
	g.slices[key] = struct{}{}
	defer delete(g.slices, key)

	out := make([]Type, 0, len(in))
	for i := range in {
		lowered, err := lowerDescriptorType(in[i], fmt.Sprintf("%s%s%d", path, sep, i), g)
		if err != nil {
			return nil, err
		}
		out = append(out, lowered)
	}
	return out, nil
}

// lowerDescriptorEnums deep-copies enum definitions in descriptor order.
func lowerDescriptorEnums(in []schemadescriptor.EnumDef) ([]EnumDef, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]EnumDef, 0, len(in))
	for i := range in {
		e := in[i]
		path := fmt.Sprintf("enum %q", e.Name.Name)
		values, err := lowerEnumValues(e.Values)
		if err != nil {
			return nil, fmt.Errorf("schema: %s: %w", path, err)
		}
		constraints, err := lowerConstraints(e.Constraints, path)
		if err != nil {
			return nil, err
		}
		out = append(out, EnumDef{
			Name:        lowerName(e.Name),
			Values:      values,
			Constraints: constraints,
		})
	}
	return out, nil
}

// lowerEnumValues deep-copies enum values, preserving each value's
// nil-vs-present alias and description.
func lowerEnumValues(in []schemadescriptor.EnumValue) ([]EnumValue, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]EnumValue, 0, len(in))
	for i := range in {
		v := in[i]
		out = append(out, EnumValue{
			Name:        lowerName(v.Name),
			Description: clonePtr(v.Description),
		})
	}
	return out, nil
}

// lowerDescriptorClasses deep-copies class definitions in descriptor order.
func lowerDescriptorClasses(in []schemadescriptor.ClassDef, g *cycleGuard) ([]ClassDef, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ClassDef, 0, len(in))
	for i := range in {
		c := in[i]
		path := fmt.Sprintf("class %q", c.Name.Name)
		mode, err := lowerStreamingMode(c.Mode, path)
		if err != nil {
			return nil, fmt.Errorf("schema: %s: %w", path, err)
		}
		fields, err := lowerClassFields(c.Fields, path, g)
		if err != nil {
			return nil, err
		}
		constraints, err := lowerConstraints(c.Constraints, path)
		if err != nil {
			return nil, err
		}
		out = append(out, ClassDef{
			Name:        lowerName(c.Name),
			Description: clonePtr(c.Description),
			Mode:        mode,
			Fields:      fields,
			Constraints: constraints,
			Stream:      lowerStreamingBehavior(c.Stream),
		})
	}
	return out, nil
}

// lowerClassFields deep-copies class fields, lowering each field type and
// preserving StreamingNeeded and the nil-vs-present description/alias.
func lowerClassFields(in []schemadescriptor.ClassField, path string, g *cycleGuard) ([]ClassField, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ClassField, 0, len(in))
	for i := range in {
		f := in[i]
		ty, err := lowerDescriptorType(f.Type, fmt.Sprintf("%s.%s", path, f.Name.Name), g)
		if err != nil {
			return nil, err
		}
		out = append(out, ClassField{
			Name:            lowerName(f.Name),
			Type:            ty,
			Description:     clonePtr(f.Description),
			StreamingNeeded: f.StreamingNeeded,
		})
	}
	return out, nil
}

// lowerDescriptorAliases deep-copies structural recursive alias definitions
// in descriptor order.
func lowerDescriptorAliases(in []schemadescriptor.RecursiveAliasDef, g *cycleGuard) ([]RecursiveAliasDef, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]RecursiveAliasDef, 0, len(in))
	for i := range in {
		a := in[i]
		target, err := lowerDescriptorType(a.Target, fmt.Sprintf("alias %q", a.Name), g)
		if err != nil {
			return nil, err
		}
		out = append(out, RecursiveAliasDef{Name: a.Name, Target: target})
	}
	return out, nil
}

// lowerTypeMeta deep-copies the per-type metadata, validating constraint
// levels and copying streaming behavior verbatim.
func lowerTypeMeta(m schemadescriptor.TypeMeta, path string) (TypeMeta, error) {
	constraints, err := lowerConstraints(m.Constraints, path)
	if err != nil {
		return TypeMeta{}, err
	}
	return TypeMeta{
		Constraints: constraints,
		Stream:      lowerStreamingBehavior(m.Stream),
	}, nil
}

// lowerConstraints deep-copies constraints, validating only the level enum;
// the expression is carried opaquely and never parsed. The label's
// nil-vs-present distinction is preserved.
func lowerConstraints(in []schemadescriptor.Constraint, path string) ([]Constraint, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]Constraint, 0, len(in))
	for i := range in {
		c := in[i]
		level, err := lowerConstraintLevel(c.Level, path)
		if err != nil {
			return nil, err
		}
		out = append(out, Constraint{
			Level:      level,
			Expression: c.Expression,
			Label:      clonePtr(c.Label),
		})
	}
	return out, nil
}

// lowerName deep-copies a name, preserving the nil-vs-present alias exactly.
func lowerName(n schemadescriptor.Name) Name {
	return Name{Name: n.Name, Alias: clonePtr(n.Alias)}
}

// lowerStreamingBehavior copies the {needed, done, state} triple verbatim.
func lowerStreamingBehavior(s schemadescriptor.StreamingBehavior) StreamingBehavior {
	return StreamingBehavior{Needed: s.Needed, Done: s.Done, State: s.State}
}

// lowerLiteral validates the literal kind and copies only the scalar field
// that kind selects. A populated scalar that does not match the kind (e.g. a
// string literal carrying a non-zero Int, or an int literal carrying a
// non-empty String) is rejected rather than dropped: a mismatched scalar
// signals generator drift, and the design requires malformed payloads to fail
// closed. The kind-selected scalar's own zero value (empty string, 0, false)
// stays a legitimate literal.
func lowerLiteral(l schemadescriptor.LiteralValue, path string) (LiteralValue, error) {
	switch l.Kind {
	case schemadescriptor.LiteralString:
		if l.Int != 0 || l.Bool {
			return LiteralValue{}, fmt.Errorf("schema: %s: string literal carries a non-string scalar (int/bool)", path)
		}
		return LiteralValue{Kind: LiteralString, String: l.String}, nil
	case schemadescriptor.LiteralInt:
		if l.String != "" || l.Bool {
			return LiteralValue{}, fmt.Errorf("schema: %s: int literal carries a non-int scalar (string/bool)", path)
		}
		return LiteralValue{Kind: LiteralInt, Int: l.Int}, nil
	case schemadescriptor.LiteralBool:
		if l.String != "" || l.Int != 0 {
			return LiteralValue{}, fmt.Errorf("schema: %s: bool literal carries a non-bool scalar (string/int)", path)
		}
		return LiteralValue{Kind: LiteralBool, Bool: l.Bool}, nil
	default:
		return LiteralValue{}, fmt.Errorf("schema: %s: unknown literal kind %q", path, l.Kind)
	}
}

// lowerTypeKind maps a descriptor type-kind string to the internal constant,
// rejecting any value not in BAML's TypeIR variant set.
func lowerTypeKind(k schemadescriptor.TypeKind, path string) (TypeKind, error) {
	switch k {
	case schemadescriptor.TypeTop:
		return TypeTop, nil
	case schemadescriptor.TypePrimitive:
		return TypePrimitive, nil
	case schemadescriptor.TypeEnum:
		return TypeEnum, nil
	case schemadescriptor.TypeLiteral:
		return TypeLiteral, nil
	case schemadescriptor.TypeClass:
		return TypeClass, nil
	case schemadescriptor.TypeList:
		return TypeList, nil
	case schemadescriptor.TypeMap:
		return TypeMap, nil
	case schemadescriptor.TypeRecursiveAlias:
		return TypeRecursiveAlias, nil
	case schemadescriptor.TypeTuple:
		return TypeTuple, nil
	case schemadescriptor.TypeArrow:
		return TypeArrow, nil
	case schemadescriptor.TypeUnion:
		return TypeUnion, nil
	default:
		return "", fmt.Errorf("schema: %s: unknown type kind %q", path, k)
	}
}

// lowerPrimitiveKind maps a descriptor primitive string to the internal
// constant, rejecting any value not in BAML's primitive set.
func lowerPrimitiveKind(p schemadescriptor.PrimitiveKind, path string) (PrimitiveKind, error) {
	switch p {
	case schemadescriptor.PrimitiveString:
		return PrimitiveString, nil
	case schemadescriptor.PrimitiveInt:
		return PrimitiveInt, nil
	case schemadescriptor.PrimitiveFloat:
		return PrimitiveFloat, nil
	case schemadescriptor.PrimitiveBool:
		return PrimitiveBool, nil
	case schemadescriptor.PrimitiveNull:
		return PrimitiveNull, nil
	case schemadescriptor.PrimitiveMedia:
		return PrimitiveMedia, nil
	default:
		return "", fmt.Errorf("schema: %s: unknown primitive %q", path, p)
	}
}

// lowerMediaKind maps a descriptor media subtype to the internal constant. An
// empty or unrecognised subtype is rejected: a media primitive must name a
// valid subtype to be well-formed even though the output profile forbids
// media entirely.
func lowerMediaKind(m schemadescriptor.MediaKind, path string) (MediaKind, error) {
	switch m {
	case schemadescriptor.MediaImage:
		return MediaImage, nil
	case schemadescriptor.MediaAudio:
		return MediaAudio, nil
	case schemadescriptor.MediaPDF:
		return MediaPDF, nil
	case schemadescriptor.MediaVideo:
		return MediaVideo, nil
	default:
		return "", fmt.Errorf("schema: %s: media primitive has invalid subtype %q", path, m)
	}
}

// lowerStreamingMode maps a descriptor streaming-mode string to the internal
// constant, rejecting an empty or unrecognised mode. Every class definition
// and class/recursive-alias reference carries an explicit mode in BAML.
func lowerStreamingMode(mode schemadescriptor.StreamingMode, path string) (StreamingMode, error) {
	switch mode {
	case schemadescriptor.NonStreaming:
		return NonStreaming, nil
	case schemadescriptor.Streaming:
		return Streaming, nil
	default:
		return "", fmt.Errorf("schema: %s: invalid streaming mode %q", path, mode)
	}
}

// lowerConstraintLevel maps a descriptor constraint level to the internal
// constant, rejecting an unknown level. This is the ONLY gate on constraints:
// their expressions are opaque, so a bad level is the sole way lowering can
// reject a constraint.
func lowerConstraintLevel(level schemadescriptor.ConstraintLevel, path string) (ConstraintLevel, error) {
	switch level {
	case schemadescriptor.ConstraintCheck:
		return ConstraintCheck, nil
	case schemadescriptor.ConstraintAssert:
		return ConstraintAssert, nil
	default:
		return "", fmt.Errorf("schema: %s: unknown constraint level %q", path, level)
	}
}

// clonePtr returns a fresh copy of a *string, preserving the nil-vs-present
// distinction: nil stays nil, and a pointer to "" stays a pointer to "".
// Descriptions, aliases, and constraint labels rely on this so the lowered
// bundle never collapses an intentional empty string to absent.
func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := *p
	return &s
}

// cloneStrings deep-copies a string slice, preserving order. A nil or empty
// input yields a nil slice.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
