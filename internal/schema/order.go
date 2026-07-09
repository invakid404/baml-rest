package schema

import (
	"strconv"
	"strings"
)

// This file reproduces BAML's reachability traversal
// (engine/baml-runtime/src/internal/prompt_renderer/render_output_format.rs
// ::relevant_data_models) so that the enum and class slices a dynamic
// schema lowers into carry BAML's exact definition order, not declared
// order. BAML discovers the definitions reachable from the target with a
// LIFO stack and appends each definition the first time it is popped, so
// the rendered hoist order is the reverse-of-reference (DFS) order, never
// the declaration order an IndexMap would give.
//
// Three details are load-bearing and reproduced exactly:
//
//   - The dedup key is a BAML TypeIR::to_string() reproduction
//     ([displayKey]), NOT pointer identity, Go %v, or a bare name. Named
//     refs key by bare name (a streaming class/alias prefixes
//     "Streaming."); containers key from their children. Two refs to the
//     same key collapse to one definition exactly as BAML's checked_types
//     HashSet collapses them.
//   - Only the checked set gates pushes; there is no separate queued/in-
//     stack set. So the same type can be pushed twice (from two fields)
//     and the duplicate is discarded when popped after the first pop
//     inserted its key — matching BAML, whose push checks also consult
//     only checked_types.
//   - A class is appended the instant it is popped, BEFORE its queued
//     field types are processed (append-on-pop, not post-order). Field
//     types are pushed in field order, so LIFO processes the last field
//     first.

// ReachableOrder walks the bundle's target type graph the way BAML's
// relevant_data_models does and returns the enum names, class keys, and
// structural-recursive-alias names reachable from the target, in BAML
// output-format hoist order (reverse-of-reference DFS). Unreachable
// definitions are omitted, matching BAML and [orderByReachability].
//
// This is the exported seam a native static-schema builder uses to emit its
// descriptor's enum/class/alias slices in BAML order. The dynamic path gets
// that order for free inside [FromDynamicOutputSchema], which calls
// [orderByReachability] directly; a static builder that lowers a
// declaration-ordered descriptor lowers it here, then reorders the descriptor
// by this result before storing it (FromStaticDescriptor preserves order
// verbatim and never reruns reachability). It relies on the same single source
// of truth for the traversal, so the two paths cannot drift.
//
// The alias-name slice is the discovery order of [TypeRecursiveAlias] nodes:
// the order the first reference to each structural recursive alias is popped.
// A native builder emitting structural recursive aliases uses it to order
// Bundle.StructuralRecursiveAliases exactly as BAML's structural_recursive_aliases
// IndexMap is ordered (see [orderByReachability]).
//
// The bundle's exported slices are the inputs; the unexported indexes are not
// consulted, so ReachableOrder is safe to call on a freshly lowered bundle
// whether or not [Bundle.RebuildIndexes] has run.
func (b *Bundle) ReachableOrder() ([]string, []ClassKey, []string) {
	enumDefs := make(map[string]EnumDef, len(b.Enums))
	for i := range b.Enums {
		enumDefs[b.Enums[i].Name.Name] = b.Enums[i]
	}
	classDefs := make(map[ClassKey]ClassDef, len(b.Classes))
	for i := range b.Classes {
		classDefs[ClassKey{Name: b.Classes[i].Name.Name, Mode: b.Classes[i].Mode}] = b.Classes[i]
	}
	aliasTargets := make(map[string]Type, len(b.StructuralRecursiveAliases))
	for i := range b.StructuralRecursiveAliases {
		aliasTargets[b.StructuralRecursiveAliases[i].Name] = b.StructuralRecursiveAliases[i].Target
	}

	enums, classes, aliasNames := orderByReachability(b.Target, enumDefs, classDefs, aliasTargets)

	enumNames := make([]string, 0, len(enums))
	for i := range enums {
		enumNames = append(enumNames, enums[i].Name.Name)
	}
	classKeys := make([]ClassKey, 0, len(classes))
	for i := range classes {
		classKeys = append(classKeys, ClassKey{Name: classes[i].Name.Name, Mode: classes[i].Mode})
	}
	return enumNames, classKeys, aliasNames
}

// orderByReachability walks the type graph rooted at target the way BAML's
// relevant_data_models does and returns the enum and class definitions, plus
// the structural-recursive-alias names, in the order BAML would hoist them.
// enumDefs and classDefs are the lowered definition bodies keyed by canonical
// name and (name, mode); aliasTargets maps each structural recursive alias
// name to its target type (nil/empty when the graph has no recursive aliases,
// as on the dynamic path). A reference with no local body (a resolver-
// classified external ref) contributes no definition but is still marked
// checked, exactly as the construction contract requires — Validate catches the
// dangling reference later.
//
// A [TypeRecursiveAlias] node is appended to the returned alias-name slice on
// first pop and its target is pushed onto the stack, reproducing BAML's
// RecursiveTypeAlias arm (which inserts the alias into structural_recursive_aliases
// and pushes its target). This both orders the alias definitions and lets the
// traversal discover any enum/class reachable only through an alias target.
//
// Unreachable declared definitions are intentionally absent from the
// result: relevant_data_models only returns what the target reaches.
func orderByReachability(target Type, enumDefs map[string]EnumDef, classDefs map[ClassKey]ClassDef, aliasTargets map[string]Type) ([]EnumDef, []ClassDef, []string) {
	var enumsOut []EnumDef
	var classesOut []ClassDef
	var aliasesOut []string

	// checked mirrors BAML's checked_types: a type's display key is
	// inserted the first time the type is popped (for the inserting kinds),
	// and every push is gated by membership in this set alone.
	checked := make(map[string]struct{})
	stack := []Type{target}

	for len(stack) > 0 {
		// LIFO: pop from the end (`while let Some(output) = stack.pop()`).
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		switch t.Kind {
		case TypeEnum:
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			// Append the enum body on first pop; enums push no children.
			if def, ok := enumDefs[t.Name]; ok {
				enumsOut = append(enumsOut, def)
			}

		case TypeClass:
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			def, ok := classDefs[ClassKey{Name: t.Name, Mode: t.Mode}]
			if !ok {
				// Resolver-classified external ref: no local body to append
				// or descend into. Still checked above so a second ref to it
				// is a no-op, matching BAML's single-visit semantics.
				continue
			}
			// Append-on-pop, BEFORE queuing field types (not post-order).
			classesOut = append(classesOut, def)
			// Push every field type in field order; LIFO then processes the
			// last field first, reproducing BAML's stack discipline.
			for i := range def.Fields {
				pushIfUnchecked(&stack, checked, def.Fields[i].Type)
			}

		case TypeList:
			// BAML does NOT insert the list wrapper into checked_types; it
			// only pushes the element if the element key is unchecked.
			if t.Elem != nil {
				pushIfUnchecked(&stack, checked, *t.Elem)
			}

		case TypeMap:
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			// Push key THEN value, so LIFO processes the value before the
			// key. The value push re-reads checked, which does not yet hold
			// anything queued by the key push (dedup is checked-only).
			if t.Key != nil {
				pushIfUnchecked(&stack, checked, *t.Key)
			}
			if t.Value != nil {
				pushIfUnchecked(&stack, checked, *t.Value)
			}

		case TypeUnion:
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			if t.Union != nil {
				// Push iter_include_null order (non-null variants first, null
				// last when optional); LIFO pops the last member first. The
				// appended null pops first for a nullable union and is a no-op.
				for _, m := range iterIncludeNull(t.Union) {
					pushIfUnchecked(&stack, checked, m)
				}
			}

		case TypeTuple:
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			// Tuple/arrow/top never occur in the dynamic output subset
			// (ValidateOutput rejects them), but reproduce the push shape for
			// completeness: items in source order, last popped first.
			for i := range t.Items {
				pushIfUnchecked(&stack, checked, t.Items[i])
			}

		case TypeRecursiveAlias:
			// BAML's RecursiveTypeAlias arm inserts the alias into
			// structural_recursive_aliases (an IndexMap, first-insert wins) and
			// pushes its target. Reproduce that: record the name on first pop
			// (the alias-definition hoist order) and push its target so any
			// enum/class reachable only through the alias body is discovered.
			// displayKey is the bare alias name, so a second reference dedups.
			key := displayKey(t)
			if _, seen := checked[key]; seen {
				continue
			}
			checked[key] = struct{}{}
			aliasesOut = append(aliasesOut, t.Name)
			if tgt, ok := aliasTargets[t.Name]; ok {
				pushIfUnchecked(&stack, checked, tgt)
			}

		default:
			// Primitives, literals, arrow, and top are no-ops in this
			// traversal: they insert nothing and push nothing. (The dynamic
			// path produces none of arrow/top; recursive aliases are handled by
			// the TypeRecursiveAlias case above.)
		}
	}

	return enumsOut, classesOut, aliasesOut
}

// pushIfUnchecked pushes t onto the stack only when its display key is not
// already in checked, reproducing BAML's `if !checked_types.contains(...)`
// push guard. Gating on checked alone (never an in-stack set) is what lets
// a type be queued twice from two fields and discarded on the second pop.
func pushIfUnchecked(stack *[]Type, checked map[string]struct{}, t Type) {
	if _, seen := checked[displayKey(t)]; seen {
		return
	}
	*stack = append(*stack, t)
}

// iterIncludeNull reproduces BAML's UnionType::iter_include_null for the
// traversal: the non-null variants in order, with the canonical null
// primitive appended when the union is optional (BAML's is_optional: a
// single non-null variant, an empty union, or an explicitly nullable
// multi-variant union). The renderer carries its own copy in the
// outputformat package; this one keeps the traversal self-contained.
func iterIncludeNull(u *UnionType) []Type {
	out := make([]Type, 0, len(u.Variants)+1)
	out = append(out, u.Variants...)
	if len(u.Variants) <= 1 || u.Nullable {
		out = append(out, nullType())
	}
	return out
}

// displayKey reproduces BAML's TypeIR::to_string()
// (engine/baml-lib/baml-types/src/ir_type/display.rs) for the subset of
// types a dynamic output schema can produce. It is the dedup key for the
// reachability traversal, so it must collapse exactly the types BAML's
// HashSet<String> collapses:
//
//   - Named enum/class/alias refs render as the bare canonical name,
//     ignoring the dynamic flag (BAML's display ignores it for identity);
//     a streaming class/alias ref is prefixed "Streaming.".
//   - Primitives, literals, lists, maps, unions, and tuples render from
//     their children, so structurally equal containers share a key.
//
// The dynamic path carries no constraints or streaming metadata, so the
// metadata suffix BAML appends after the body is empty here. Primitive and
// literal keys never affect ordering (those arms are traversal no-ops and
// are never inserted into checked), but they are reproduced faithfully so
// a container key built from them is stable and injective.
func displayKey(t Type) string {
	switch t.Kind {
	case TypePrimitive:
		return primitiveDisplay(t)
	case TypeEnum:
		return t.Name
	case TypeClass, TypeRecursiveAlias:
		if t.Mode == Streaming {
			return "Streaming." + t.Name
		}
		return t.Name
	case TypeLiteral:
		return literalDisplayKey(t.Literal)
	case TypeList:
		if t.Elem == nil {
			return "null[]"
		}
		return displayKey(*t.Elem) + "[]"
	case TypeMap:
		k, v := "null", "null"
		if t.Key != nil {
			k = displayKey(*t.Key)
		}
		if t.Value != nil {
			v = displayKey(*t.Value)
		}
		return "map<" + k + ", " + v + ">"
	case TypeUnion:
		if t.Union == nil {
			return "()"
		}
		members := iterIncludeNull(t.Union)
		parts := make([]string, 0, len(members))
		for i := range members {
			parts = append(parts, displayKey(members[i]))
		}
		return "(" + strings.Join(parts, " | ") + ")"
	case TypeTuple:
		parts := make([]string, 0, len(t.Items))
		for i := range t.Items {
			parts = append(parts, displayKey(t.Items[i]))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case TypeArrow:
		if t.Arrow == nil {
			return "() -> top"
		}
		parts := make([]string, 0, len(t.Arrow.Params))
		for i := range t.Arrow.Params {
			parts = append(parts, displayKey(t.Arrow.Params[i]))
		}
		return "(" + strings.Join(parts, ", ") + ") -> " + displayKey(t.Arrow.Return)
	case TypeTop:
		// BAML's fmt_type_body prints Top as "ANY" (never reached on the
		// dynamic path: ValidateOutput rejects top and the traversal never
		// produces it, but match BAML's display for the dedup key anyway).
		return "ANY"
	default:
		return string(t.Kind)
	}
}

// primitiveDisplay reproduces BAML's TypeValue Display for the primitive
// kinds (the bare lowercase keyword), used as the leaf of a container key.
func primitiveDisplay(t Type) string {
	switch t.Primitive {
	case PrimitiveString:
		return "string"
	case PrimitiveInt:
		return "int"
	case PrimitiveFloat:
		return "float"
	case PrimitiveBool:
		return "bool"
	case PrimitiveNull:
		return "null"
	case PrimitiveMedia:
		return string(t.Media)
	default:
		return string(t.Primitive)
	}
}

// literalDisplayKey reproduces BAML's LiteralValue Display: a string is
// double-quoted verbatim, an int and bool render bare. It only feeds the
// dedup key, never the rendered prompt.
func literalDisplayKey(l *LiteralValue) string {
	if l == nil {
		return "literal[]"
	}
	switch l.Kind {
	case LiteralString:
		return `"` + l.String + `"`
	case LiteralInt:
		return strconv.FormatInt(l.Int, 10)
	case LiteralBool:
		return strconv.FormatBool(l.Bool)
	default:
		return "literal[]"
	}
}
