package bamlutils

import "slices"

// sortOrderedMapByKey returns a copy of m whose entries are re-ordered so
// the keys are in ascending lexicographical order. Values are carried by
// shallow copy (pointers are shared), matching OrderedMap.Clone. Keys are
// unique in m, so the rebuild never hits a duplicate-key error.
func sortOrderedMapByKey[V any](m OrderedMap[V]) OrderedMap[V] {
	keys := m.Keys() // Keys returns a copy, safe to sort in place.
	if len(keys) == 0 {
		return OrderedMap[V]{}
	}
	slices.Sort(keys)
	out := OrderedMap[V]{
		keys: keys,
		vals: make(map[string]V, len(keys)),
	}
	for _, k := range keys {
		out.vals[k] = m.vals[k]
	}
	return out
}

// normalizeSchemaFieldOrderForRender returns a clone of s whose root
// Properties and every class's Properties are re-ordered alphabetically,
// leaving the caller-owned schema untouched.
//
// It implements the preserve_schema_order=false half of the native
// ctx.output_format render policy. When a caller does not opt into
// preserve order, BAML's codegen-emitted applyDynamicTypes populates the
// TypeBuilder in sorted key order (schemaKeys sorts when preserve is
// false), so BAML renders each class's fields alphabetically. The native
// renderer lowers the carried DynamicOutputSchema verbatim, so to match
// BAML byte-for-byte it must be handed a schema whose field order is
// already alphabetized. Sorting only the per-class property order is
// sufficient: the rendered class-hoist order is derived from the field
// reference graph (see internal/schema.orderByReachability), so the
// Classes / Enums map order does not affect the rendered prompt and is
// preserved as-is.
//
// The clone never mutates s: root Properties and each class's Properties
// become fresh OrderedMaps, and every class is copied into a new
// *DynamicClass. Enums are carried by reference because the renderer only
// reads them and never reorders enum values (BAML adds enum values in
// declaration order regardless of preserve).
func normalizeSchemaFieldOrderForRender(s *DynamicOutputSchema) *DynamicOutputSchema {
	if s == nil {
		return nil
	}
	out := &DynamicOutputSchema{
		Properties: sortOrderedMapByKey(s.Properties),
		Enums:      s.Enums,
	}
	if s.Classes.Len() > 0 {
		classes := OrderedMap[*DynamicClass]{
			keys: s.Classes.Keys(),
			vals: make(map[string]*DynamicClass, s.Classes.Len()),
		}
		s.Classes.Range(func(name string, class *DynamicClass) bool {
			if class == nil {
				classes.vals[name] = nil
				return true
			}
			classes.vals[name] = &DynamicClass{
				Description: class.Description,
				Alias:       class.Alias,
				Properties:  sortOrderedMapByKey(class.Properties),
			}
			return true
		})
		out.Classes = classes
	}
	return out
}
