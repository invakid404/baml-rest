package codegen

// nativespine_recursion.go is the M3c recursion layer (codegen-spine slice M3c).
// It extends the merged M3a output-carrier core (nativespine.go) and the M3b
// union layer (nativespine_union.go) with:
//
//   - structural recursive alias declarations (a Go `type Output<Name> = <target>`
//     ALIAS, never a new defined wrapper), reproducing BAML v0.223's generated
//     type_aliases.go — INCLUDING the counterintuitive pure-container fallback
//     where a single-alias structural cycle with no concrete leaf drops its
//     recursive occurrence to `any` (`type ListNode = ListNode[]` ->
//     `type OutputListNode = []any`), mirroring the generator's `invalid_cycles`
//     rule in engine/generators/languages/go/src/{lib.rs,ir_to_go/type_aliases.rs};
//   - the SHARED emitter-feasibility gate [CheckNativeCarrierShape], which the
//     classifier (classifyOutputSchema) and the emitter (EmitNativeStaticUnary)
//     both call so admission can never outrun what emission can render;
//   - the direct-by-value class-SCC decline: v0.223 REJECTS a class dependency
//     cycle whose edges are all non-nullable class values ("These classes form a
//     dependency cycle"), so there is no faithful generated carrier and native
//     declines it, catching what M2's permissive descriptor builder can synthesize;
//   - the recursion-safe marshal guard emitted into a recursive carrier's custom
//     MarshalJSON: M3a's custom codec bypasses encoding/json's ordinary pointer-
//     cycle tracking, so a user-built pointer cycle would recurse until the stack
//     overflows. The guard runs a bounded, PER-CALL reflection pass before the
//     real marshal (finite values pass it untouched, so their bytes are
//     unchanged) and turns a cycle into a marshal error — matching the generated
//     non-recursive class carrier, whose default json.Marshal reports a cycle.
//
// The v0.223 recursion representation adds NO blanket pointer: each edge keeps
// its ordinary lowering (T? -> *T, list -> []T, map -> map[string]T, a
// non-nullable class ref -> the class value, a union arm -> its existing private
// *T arm pointer, a recursive alias -> a Go ALIAS to its lowered target).
// Termination comes from a declared nullable edge, an empty/nil list or map, or a
// terminating union arm — never from pointerizing every edge.

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// CheckNativeCarrierShape is the SHARED emitter-feasibility gate. It reports an
// error when the emitter could not render a faithful, compiling carrier for ret,
// so the classifier (which maps a failure to unsupported_output_shape) and the
// direct emitter (which calls it as a backstop) decline EXACTLY the same shapes —
// one source of truth, no admission/emission drift. Name-collision failures are
// deliberately NOT covered here; they are mapped to name_collision by
// CheckNativeNameCollision (a separate, precise code).
//
// It checks, in order:
//
//  1. every reachable multi-arm union lowers to a nameable carrier (buildCarrierPlan);
//  2. every class/enum/recursive-alias reference resolves in the bundle
//     (validateOutputRefs);
//  3. every reachable type lowers to a Go expression — the return target, every
//     class field, every structural-recursive-alias declaration, and every union
//     arm — so no admitted shape fails in schemaGoType at emit time;
//  4. the Go declaration graph has finite type size: there is no direct by-value
//     class SCC (checkNoDirectClassValueCycle). Optional, list, map, and M3b
//     union-arm pointers break size cycles; a direct class value does not.
func CheckNativeCarrierShape(ret schemadescriptor.Bundle) error {
	plan, err := buildCarrierPlan(ret)
	if err != nil {
		return err
	}
	if err := validateOutputRefs(ret); err != nil {
		return err
	}
	if _, err := schemaGoType(ret.Target, plan); err != nil {
		return fmt.Errorf("output target %w", err)
	}
	for i := range ret.Classes {
		c := &ret.Classes[i]
		for j := range c.Fields {
			if _, err := schemaGoType(c.Fields[j].Type, plan); err != nil {
				return fmt.Errorf("class %q field %q %w", c.Name.Name, c.Fields[j].Name.Name, err)
			}
		}
	}
	for i := range ret.StructuralRecursiveAliases {
		if _, err := lowerRecursiveAliasDecl(ret.StructuralRecursiveAliases[i], plan); err != nil {
			return err
		}
	}
	for _, u := range plan.unions {
		if _, err := resolveUnionArms(u, plan); err != nil {
			return err
		}
	}
	if err := checkNoDirectClassValueCycle(ret); err != nil {
		return err
	}
	return nil
}

// bundleHasRecursion reports whether ret carries any recursion representation
// (recursive classes or structural recursive aliases). It gates the recursion-
// safe marshal guard: a non-recursive bundle keeps M3a/M3b emission byte-for-byte
// identical (no guard, no reflect import), so their goldens/differentials are
// untouched.
func bundleHasRecursion(ret schemadescriptor.Bundle) bool {
	return len(ret.RecursiveClasses) > 0 || len(ret.StructuralRecursiveAliases) > 0
}

// lowerRecursiveAliasDecl renders the Go type expression for one structural
// recursive alias declaration `type Output<Name> = <expr>`, reproducing BAML
// v0.223's generated type_aliases.go. Its recursive occurrence is dropped to
// `any` iff the alias is a pure-container cycle with no concrete scalar/class/
// enum/literal leaf (the generator's `invalid_cycles` rule): `type ListNode =
// ListNode[]` -> `[]any`, `type StrMap = map<string, StrMap>` -> `map[string]any`.
func lowerRecursiveAliasDecl(a schemadescriptor.RecursiveAliasDef, plan *carrierPlan) (string, error) {
	dropSelf := !aliasHasConcreteLeaf(a.Target)
	return lowerAliasTarget(a.Target, a.Name, dropSelf, plan)
}

// aliasHasConcreteLeaf reports whether t contains a concrete scalar/class/enum/
// literal leaf anywhere in its type tree, reproducing BAML's
// TypeIR::find_if(is_concrete_leaf, ignore_map_keys=true) used by the go
// generator's invalid_cycles filter (engine/generators/languages/go/src/lib.rs):
//
//   - a non-null primitive, an enum, a class, or a literal IS a concrete leaf;
//   - a null primitive is NOT (null cannot terminate a cycle);
//   - a recursive-alias occurrence is opaque — not a leaf and NOT descended into;
//   - list elements, map VALUES (map keys are ignored), and union non-null
//     variants are descended.
//
// A single-alias structural cycle whose target has NO concrete leaf is the case
// v0.223 makes Go-compilable by dropping the recursive occurrence to `any`.
func aliasHasConcreteLeaf(t schemadescriptor.Type) bool {
	switch t.Kind {
	case schemadescriptor.TypePrimitive:
		// Null does not count; string/int/float/bool (and media, which declines
		// upstream) do — matching BAML's Primitive(Null) => false, Primitive(..) => true.
		return t.Primitive != schemadescriptor.PrimitiveNull
	case schemadescriptor.TypeEnum, schemadescriptor.TypeClass, schemadescriptor.TypeLiteral:
		return true
	case schemadescriptor.TypeList:
		return t.Elem != nil && aliasHasConcreteLeaf(*t.Elem)
	case schemadescriptor.TypeMap:
		// ignore_map_keys=true: only the value counts toward a concrete leaf.
		return t.Value != nil && aliasHasConcreteLeaf(*t.Value)
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return false
		}
		for _, v := range t.Union.Variants {
			if aliasHasConcreteLeaf(v) {
				return true
			}
		}
		return false
	default:
		// recursive_alias (opaque), top, tuple, arrow: not a concrete leaf.
		return false
	}
}

// lowerAliasTarget lowers a structural recursive alias's target to a Go type
// expression. It mirrors schemaGoType with one addition: a TypeRecursiveAlias
// occurrence of `self` drops to `any` when dropSelf (the invalid_cycles case),
// and otherwise renders as the named alias reference; a reference to ANOTHER
// alias always renders as that alias's name (the self-drop is scoped to `self`,
// matching the generator's LookupWithDrop, whose drop_type is the alias being
// rendered). A multi-arm union resolves to its planned carrier NAME and its arms
// are NOT descended here — the union carrier's arms are lowered by schemaGoType
// with no drop, exactly like the generator rendering a union with pkg.lookup().
func lowerAliasTarget(t schemadescriptor.Type, self string, dropSelf bool, plan *carrierPlan) (string, error) {
	switch t.Kind {
	case schemadescriptor.TypeRecursiveAlias:
		if t.Name == self && dropSelf {
			return "any", nil
		}
		return outputTypeName(t.Name), nil
	case schemadescriptor.TypeList:
		if t.Elem == nil {
			return "", fmt.Errorf("recursive alias %q: list has no element type", self)
		}
		elem, err := lowerAliasTarget(*t.Elem, self, dropSelf, plan)
		if err != nil {
			return "", err
		}
		return "[]" + elem, nil
	case schemadescriptor.TypeMap:
		if t.Key == nil || t.Value == nil {
			return "", fmt.Errorf("recursive alias %q: map has no key/value type", self)
		}
		if t.Key.Kind != schemadescriptor.TypePrimitive || t.Key.Primitive != schemadescriptor.PrimitiveString {
			return "", fmt.Errorf("recursive alias %q: only string-keyed maps are in the carrier profile", self)
		}
		val, err := lowerAliasTarget(*t.Value, self, dropSelf, plan)
		if err != nil {
			return "", err
		}
		return "map[string]" + val, nil
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return "", fmt.Errorf("recursive alias %q: union has no payload", self)
		}
		// Optional-of-one (`T?`) stays `*T`; a self-occurrence under it still drops
		// (the generator's make_optional wraps the dropped inner).
		if t.Union.Nullable && len(t.Union.Variants) == 1 {
			inner, err := lowerAliasTarget(t.Union.Variants[0], self, dropSelf, plan)
			if err != nil {
				return "", err
			}
			return "*" + inner, nil
		}
		if len(t.Union.Variants) >= 2 {
			name, ok := plan.unionName(t.Union.Variants)
			if !ok {
				return "", fmt.Errorf("recursive alias %q: multi-arm union was not planned (admission outran emission)", self)
			}
			if t.Union.Nullable {
				return "*" + name, nil
			}
			return name, nil
		}
		return "", fmt.Errorf("recursive alias %q: union with %d variant(s) is outside the carrier profile", self, len(t.Union.Variants))
	default:
		// primitive/enum/class/literal: no self-occurrence, identical to schemaGoType.
		return schemaGoType(t, plan)
	}
}

// checkNoDirectClassValueCycle rejects a return graph containing a direct
// by-value class SCC. An edge A -> C exists iff class A has a field whose type is
// a NON-nullable direct class reference to C (field.Type.Kind == TypeClass) — the
// only field shape Go embeds by value. Optional (`*C`), list (`[]C`), map
// (`map[string]C`), and M3b union-arm (`*C` inside a value union carrier) edges
// are pointer-mediated and give the Go struct a finite size, so they are NOT
// value edges. A cycle in this graph is the class dependency cycle stock v0.223
// REJECTS before codegen ("These classes form a dependency cycle"), so there is
// no faithful generated carrier to reproduce and native must decline it. M2's
// permissive descriptor builder can synthesize such a bundle (its recursive-class
// analysis marks the SCC recursive but does not reproduce this rejection), so the
// SHARED gate catches it before emission.
func checkNoDirectClassValueCycle(ret schemadescriptor.Bundle) error {
	classSet := make(map[string]bool, len(ret.Classes))
	for i := range ret.Classes {
		classSet[ret.Classes[i].Name.Name] = true
	}
	edges := make(map[string][]string, len(ret.Classes))
	for i := range ret.Classes {
		c := &ret.Classes[i]
		for j := range c.Fields {
			ft := c.Fields[j].Type
			if ft.Kind == schemadescriptor.TypeClass && classSet[ft.Name] {
				edges[c.Name.Name] = append(edges[c.Name.Name], ft.Name)
			}
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(ret.Classes))
	var path []string
	var dfs func(n string) error
	dfs = func(n string) error {
		color[n] = gray
		path = append(path, n)
		for _, m := range edges[n] {
			switch color[m] {
			case gray:
				return fmt.Errorf("return schema has a direct by-value class dependency cycle (%s), which v0.223 rejects as a class dependency cycle before codegen", cyclePath(path, m))
			case white:
				if err := dfs(m); err != nil {
					return err
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return nil
	}
	for i := range ret.Classes {
		if color[ret.Classes[i].Name.Name] == white {
			if err := dfs(ret.Classes[i].Name.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// cyclePath renders the cycle for a decline message: the active DFS path from the
// re-entered node `back` onward, closed back to `back` (e.g. "A -> B -> A").
func cyclePath(path []string, back string) string {
	start := 0
	for i, n := range path {
		if n == back {
			start = i
			break
		}
	}
	return fmt.Sprintf("%s -> %s", joinArrows(path[start:]), back)
}

func joinArrows(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += " -> "
		}
		out += n
	}
	return out
}

// nativeSpineCycleGuardSource is the recursion-safe marshal guard emitted into a
// recursive carrier file (gated on bundleHasRecursion + a class/union carrier
// existing). It imports reflect. See the package doc: it detects a user-built
// pointer/slice/map cycle before the custom codec recurses into it, so marshal
// returns an error instead of overflowing the stack — matching the generated
// non-recursive class carrier, whose default json.Marshal reports a cycle. The
// active set is created per top-level call (never a package-level variable).
const nativeSpineCycleGuardSource = `
// nativeSpineCheckAcyclic reports an error if v's in-memory Go value graph
// contains a pointer/slice/map cycle. A recursive carrier's custom MarshalJSON
// recurses without the ordinary encoder's cycle tracking, so a user-built cycle
// would recurse until the stack overflows; this bounded PER-CALL reflection pass
// runs before the real marshal (finite values pass it untouched, so their bytes
// are unchanged) and turns a cycle into a marshal error.
func nativeSpineCheckAcyclic(v any) error {
	return nativeSpineWalkAcyclic(reflect.ValueOf(v), map[[2]uintptr]bool{})
}

// nativeSpineWalkAcyclic walks rv, tracking the indirection nodes on the CURRENT
// path in active (add on enter, remove on leave — stack semantics, so a shared
// node reachable twice via siblings is fine and only a genuine ancestor cycle
// errors). A node's key mirrors encoding/json's ptrSeen: a pointer or map by its
// address alone, and a slice by (data pointer, len) so a sub-slice that shares its
// parent's start address is NOT a false cycle. It reads unexported carrier fields
// structurally (never via Interface(), which reflect forbids on them).
func nativeSpineWalkAcyclic(rv reflect.Value, active map[[2]uintptr]bool) error {
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		k := [2]uintptr{rv.Pointer(), 0}
		if active[k] {
			return fmt.Errorf("nativespine: encountered a cycle")
		}
		active[k] = true
		err := nativeSpineWalkAcyclic(rv.Elem(), active)
		delete(active, k)
		return err
	case reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return nativeSpineWalkAcyclic(rv.Elem(), active)
	case reflect.Slice:
		if rv.IsNil() || rv.Len() == 0 {
			return nil
		}
		k := [2]uintptr{rv.Pointer(), uintptr(rv.Len())}
		if active[k] {
			return fmt.Errorf("nativespine: encountered a cycle")
		}
		active[k] = true
		for i := 0; i < rv.Len(); i++ {
			if err := nativeSpineWalkAcyclic(rv.Index(i), active); err != nil {
				delete(active, k)
				return err
			}
		}
		delete(active, k)
		return nil
	case reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if err := nativeSpineWalkAcyclic(rv.Index(i), active); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if rv.IsNil() || rv.Len() == 0 {
			return nil
		}
		k := [2]uintptr{rv.Pointer(), 0}
		if active[k] {
			return fmt.Errorf("nativespine: encountered a cycle")
		}
		active[k] = true
		iter := rv.MapRange()
		for iter.Next() {
			if err := nativeSpineWalkAcyclic(iter.Value(), active); err != nil {
				delete(active, k)
				return err
			}
		}
		delete(active, k)
		return nil
	case reflect.Struct:
		for i := 0; i < rv.NumField(); i++ {
			if err := nativeSpineWalkAcyclic(rv.Field(i), active); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}
`
