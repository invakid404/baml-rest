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

// orderByReachability walks the type graph rooted at target the way BAML's
// relevant_data_models does and returns the enum and class definitions in
// the order BAML would hoist them. enumDefs and classDefs are the lowered
// definition bodies keyed by canonical name and (name, mode); a reference
// with no local body (a resolver-classified external ref) contributes no
// definition but is still marked checked, exactly as the construction
// contract requires — Validate catches the dangling reference later.
//
// Unreachable declared definitions are intentionally absent from the
// result: relevant_data_models only returns what the target reaches.
func orderByReachability(target Type, enumDefs map[string]EnumDef, classDefs map[ClassKey]ClassDef) ([]EnumDef, []ClassDef) {
	var enumsOut []EnumDef
	var classesOut []ClassDef

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

		default:
			// Primitives, literals, recursive aliases, arrow, and top are
			// no-ops in this traversal: they insert nothing and push nothing.
			// (The dynamic path produces none of alias/arrow/top.)
		}
	}

	return enumsOut, classesOut
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
		return "top"
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
