package outputformat

import "github.com/invakid404/baml-rest/internal/schema"

// viewKind classifies a union the way BAML's UnionTypeViewGeneric does, which
// drives both the top-level prefix string and (via is_optional) whether null
// is appended during rendering.
type viewKind uint8

const (
	// viewNull is a union with no non-null variants (renders as just null).
	viewNull viewKind = iota
	// viewOptional is a union with exactly one non-null variant.
	viewOptional
	// viewOneOf is a union with multiple non-null variants and no null.
	viewOneOf
	// viewOneOfOptional is multiple non-null variants plus null.
	viewOneOfOptional
)

// unionView classifies u exactly as BAML's UnionTypeGeneric::view, working
// from the non-null variant count and the nullable flag. The schema model
// keeps non-null variants in [schema.UnionType.Variants] and tracks null
// separately in Nullable, so the classification reduces to the variant count:
// BAML returns Optional for a single non-null variant regardless of whether
// null is present, and distinguishes OneOf from OneOfOptional by null
// presence only when there are multiple non-null variants.
func unionView(u *schema.UnionType) viewKind {
	switch len(u.Variants) {
	case 0:
		return viewNull
	case 1:
		return viewOptional
	default:
		if u.Nullable {
			return viewOneOfOptional
		}
		return viewOneOf
	}
}

// unionIsOptional mirrors BAML's UnionTypeGeneric::is_optional: true for the
// Null, Optional, and OneOfOptional views (everything except OneOf).
func unionIsOptional(u *schema.UnionType) bool {
	switch len(u.Variants) {
	case 0, 1:
		return true
	default:
		return u.Nullable
	}
}

// unionIterIncludeNull reproduces BAML's iter_include_null: the non-null
// variants in order, with the canonical null primitive appended when the
// union is optional. This is the order union rendering joins by the
// or-splitter.
func unionIterIncludeNull(u *schema.UnionType) []schema.Type {
	out := make([]schema.Type, 0, len(u.Variants)+1)
	out = append(out, u.Variants...)
	if unionIsOptional(u) {
		out = append(out, nullType())
	}
	return out
}

// nullType is the canonical null primitive BAML appends for optional unions.
func nullType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveNull}
}

// isPrimitive mirrors BAML's TypeGeneric::is_primitive: a primitive (null
// included) is primitive, a list is primitive when its element is, everything
// else is not. Used by the list-wrapping union check.
func isPrimitive(t *schema.Type) bool {
	switch t.Kind {
	case schema.TypePrimitive:
		return true
	case schema.TypeList:
		return t.Elem != nil && isPrimitive(t.Elem)
	default:
		return false
	}
}

// orderedSet is an insertion-ordered string set matching Rust's IndexSet
// semantics: the first insertion fixes a member's position and later
// insertions of the same value are no-ops. Render order must come from sets
// like this and from the bundle's ordered slices, never from Go map
// iteration.
type orderedSet struct {
	items []string
	seen  map[string]struct{}
}

func newOrderedSet() *orderedSet {
	return &orderedSet{seen: make(map[string]struct{})}
}

func (s *orderedSet) add(x string) {
	if _, ok := s.seen[x]; ok {
		return
	}
	s.seen[x] = struct{}{}
	s.items = append(s.items, x)
}

func (s *orderedSet) contains(x string) bool {
	_, ok := s.seen[x]
	return ok
}
