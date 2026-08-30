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

// CheckNativeCarrierShape is the SHARED, AUTHORITATIVE emitter-feasibility gate:
// it owns the WHOLE output decline boundary so the classifier (which maps a
// failure to unsupported_output_shape) and the direct emitter (which calls it as
// its ONLY output backstop) decline EXACTLY the same shapes — one source of
// truth, no admission/emission drift, and no metadata silently dropped at emit
// time. Name-collision failures are the one exception, mapped to name_collision
// by CheckNativeNameCollision (a separate, precise code).
//
// It checks, in order:
//
//  1. no out-of-profile METADATA is reachable — @check/@assert constraints,
//     @@dynamic, or any streaming behavior (Meta.Stream / class Stream /
//     ClassField.StreamingNeeded / a Streaming Mode). M3c is output-only,
//     non-streaming final-call; the emitter carries none of these, so admitting
//     them would silently drop them (checkNoUnsupportedMetadata);
//  2. every class/enum/recursive-alias reference resolves KIND- and MODE-exactly
//     (validateOutputRefs) — a TypeClass must name a class, not an enum, and a
//     streaming reference does not resolve against a non-streaming declaration;
//  3. every structural recursive alias entry is an M2-supported SINGLE-alias
//     structural cycle (checkStructuralAliasCycles) — a multi-alias SCC or a
//     non-cyclic table entry would emit an uncompilable `type A = ... B ...` /
//     `type B = ... A ...` Go alias cycle;
//  4. every reachable multi-arm union lowers to a nameable carrier
//     (buildCarrierPlan);
//  5. every reachable type lowers to a Go expression — target, class fields,
//     alias declarations, union arms — so nothing fails in schemaGoType at emit;
//  6. the Go declaration graph has finite type size: no direct by-value class SCC
//     (checkNoDirectClassValueCycle). Optional/list/map/union-arm pointers break
//     size cycles; a direct class value does not.
func CheckNativeCarrierShape(ret schemadescriptor.Bundle) error {
	if err := checkNoUnsupportedMetadata(ret); err != nil {
		return err
	}
	if err := validateOutputRefs(ret); err != nil {
		return err
	}
	if err := checkStructuralAliasCycles(ret); err != nil {
		return err
	}
	plan, err := buildCarrierPlan(ret)
	if err != nil {
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

// checkNoUnsupportedMetadata rejects any reachable @check/@assert constraint,
// @@dynamic type, or streaming behavior — the decline boundary the emitter would
// otherwise cross by emitting a carrier that silently drops the metadata. The
// classifier ALSO catches constraints/@@dynamic earlier with their precise codes
// (checks/asserts/schema_dynamic_class); this shared gate is what makes the
// EMITTER backstop reject the same shapes (as unsupported_output_shape), and it
// is the only place either path rejects streaming metadata (M3e territory).
func checkNoUnsupportedMetadata(ret schemadescriptor.Bundle) error {
	var walk func(t schemadescriptor.Type) error
	walk = func(t schemadescriptor.Type) error {
		if t.Dynamic {
			return fmt.Errorf("references a @@dynamic type")
		}
		for _, c := range t.Meta.Constraints {
			return fmt.Errorf("carries a @%s constraint", c.Level)
		}
		if !t.Meta.Stream.IsZero() {
			return fmt.Errorf("carries @stream.* streaming behavior (streaming is not in the M3c output profile)")
		}
		switch t.Kind {
		case schemadescriptor.TypeClass, schemadescriptor.TypeRecursiveAlias:
			if t.Mode == schemadescriptor.Streaming {
				return fmt.Errorf("references a streaming-mode type %q (streaming is not in the M3c output profile)", t.Name)
			}
		case schemadescriptor.TypeList:
			if t.Elem != nil {
				return walk(*t.Elem)
			}
		case schemadescriptor.TypeMap:
			if t.Key != nil {
				if err := walk(*t.Key); err != nil {
					return err
				}
			}
			if t.Value != nil {
				return walk(*t.Value)
			}
		case schemadescriptor.TypeUnion:
			if t.Union != nil {
				for i := range t.Union.Variants {
					if err := walk(t.Union.Variants[i]); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(ret.Target); err != nil {
		return fmt.Errorf("return target %w", err)
	}
	for i := range ret.Classes {
		c := &ret.Classes[i]
		if c.Mode == schemadescriptor.Streaming {
			return fmt.Errorf("class %q is a streaming-mode declaration (streaming is not in the M3c output profile)", c.Name.Name)
		}
		for _, cc := range c.Constraints {
			return fmt.Errorf("class %q carries a @%s constraint", c.Name.Name, cc.Level)
		}
		if !c.Stream.IsZero() {
			return fmt.Errorf("class %q carries @@stream.* streaming behavior (streaming is not in the M3c output profile)", c.Name.Name)
		}
		for j := range c.Fields {
			if c.Fields[j].StreamingNeeded {
				return fmt.Errorf("class %q field %q is @stream-needed (streaming is not in the M3c output profile)", c.Name.Name, c.Fields[j].Name.Name)
			}
			if err := walk(c.Fields[j].Type); err != nil {
				return fmt.Errorf("class %q field %q %w", c.Name.Name, c.Fields[j].Name.Name, err)
			}
		}
	}
	for i := range ret.Enums {
		for _, cc := range ret.Enums[i].Constraints {
			return fmt.Errorf("enum %q carries a @%s constraint", ret.Enums[i].Name.Name, cc.Level)
		}
	}
	for i := range ret.StructuralRecursiveAliases {
		if err := walk(ret.StructuralRecursiveAliases[i].Target); err != nil {
			return fmt.Errorf("recursive alias %q target %w", ret.StructuralRecursiveAliases[i].Name, err)
		}
	}
	return nil
}

// checkStructuralAliasCycles verifies every StructuralRecursiveAliases entry is an
// M2-supported SINGLE-alias structural cycle: its target references ITSELF (a
// self-cycle) and it does NOT participate in a cycle with any OTHER alias. A
// multi-alias SCC (`A = B[]`, `B = A[]`) or a non-cyclic table entry passes
// reference resolution and lowers (lowerAliasTarget keeps other-alias references
// named), but emits `type OutputA = []OutputB` / `type OutputB = []OutputA` — an
// invalid recursive Go alias cycle the compiler rejects — so both must decline.
// Multiple DISJOINT single-alias self-cycles in one bundle stay admitted.
func checkStructuralAliasCycles(ret schemadescriptor.Bundle) error {
	aliasNames := make(map[string]bool, len(ret.StructuralRecursiveAliases))
	for i := range ret.StructuralRecursiveAliases {
		aliasNames[ret.StructuralRecursiveAliases[i].Name] = true
	}
	names := make([]string, 0, len(ret.StructuralRecursiveAliases))
	interEdges := make(map[string][]string, len(ret.StructuralRecursiveAliases))
	for i := range ret.StructuralRecursiveAliases {
		a := &ret.StructuralRecursiveAliases[i]
		names = append(names, a.Name)
		refs := map[string]bool{}
		collectAliasNameRefs(a.Target, aliasNames, refs)
		if !refs[a.Name] {
			return fmt.Errorf("structural recursive alias %q is not a single-alias structural cycle (its target does not reference itself)", a.Name)
		}
		var inter []string
		for r := range refs {
			if r != a.Name {
				inter = append(inter, r)
			}
		}
		interEdges[a.Name] = inter
	}
	// A cycle in the inter-alias graph (self-references removed) is a multi-alias
	// structural SCC — unsupported (M2 declines it before a bundle exists).
	if findGraphCycle(names, interEdges) {
		return fmt.Errorf("bundle contains a multi-alias structural recursive cycle, which is unsupported (only single-alias structural cycles emit a compiling Go alias)")
	}
	return nil
}

// collectAliasNameRefs records every alias name in aliasNames that t references
// through ANY construct (list/map/union/optional arms are all transparent here —
// this is a NAME collector, not a lowering).
func collectAliasNameRefs(t schemadescriptor.Type, aliasNames, out map[string]bool) {
	switch t.Kind {
	case schemadescriptor.TypeRecursiveAlias:
		if aliasNames[t.Name] {
			out[t.Name] = true
		}
	case schemadescriptor.TypeList:
		if t.Elem != nil {
			collectAliasNameRefs(*t.Elem, aliasNames, out)
		}
	case schemadescriptor.TypeMap:
		if t.Value != nil {
			collectAliasNameRefs(*t.Value, aliasNames, out)
		}
	case schemadescriptor.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				collectAliasNameRefs(t.Union.Variants[i], aliasNames, out)
			}
		}
	}
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

// findGraphCycle reports whether the directed graph (nodes + adjacency) contains
// any cycle, including a self-loop (a node with an edge to itself). Tri-state DFS
// over logical string keys.
func findGraphCycle(nodes []string, edges map[string][]string) bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	var dfs func(n string) bool
	dfs = func(n string) bool {
		color[n] = gray
		for _, m := range edges[n] {
			switch color[m] {
			case gray:
				return true
			case white:
				if dfs(m) {
					return true
				}
			}
		}
		color[n] = black
		return false
	}
	for _, n := range nodes {
		if color[n] == white && dfs(n) {
			return true
		}
	}
	return false
}

// carrierGraphIsRecursive reports whether the EMITTED Go carrier graph is
// recursive — i.e. some class/union/alias carrier is reachable from itself. It is
// derived STRUCTURALLY from the validated lowered graph (never from the descriptor's
// RecursiveClasses/StructuralRecursiveAliases metadata, which the emitter must not
// trust): the marshal cycle guard is emitted exactly when this is true, so a
// truly-recursive carrier always gets the guard (no stack overflow) and a
// non-recursive bundle never does (M3a/M3b bytes/source unchanged).
//
// Nodes are "class:<name>", "union:<carrier>", "alias:<name>"; an edge is a Go
// reference. A pure-container alias whose recursive occurrence lowered to `any`
// (`type OutputListNode = []any`) contributes NO self-edge and is correctly NOT
// recursive; a recursive occurrence surviving through a value union carrier
// (`type OutputRecursive1 = OutputUnion1`, arm `*[]OutputRecursive1`) forms a real
// alias<->union cycle. MUST be called with the plan built for ret.
func carrierGraphIsRecursive(ret schemadescriptor.Bundle, plan *carrierPlan) bool {
	edges := map[string][]string{}
	var nodes []string
	add := func(node string, refs map[string]bool) {
		nodes = append(nodes, node)
		out := make([]string, 0, len(refs))
		for r := range refs {
			out = append(out, r)
		}
		edges[node] = out
	}
	for i := range ret.Classes {
		c := &ret.Classes[i]
		refs := map[string]bool{}
		for j := range c.Fields {
			collectCarrierGoRefs(c.Fields[j].Type, plan, refs)
		}
		add("class:"+c.Name.Name, refs)
	}
	for _, u := range plan.unions {
		refs := map[string]bool{}
		for i := range u.variants {
			collectCarrierGoRefs(u.variants[i], plan, refs)
		}
		add("union:"+u.name, refs)
	}
	for i := range ret.StructuralRecursiveAliases {
		a := &ret.StructuralRecursiveAliases[i]
		refs := map[string]bool{}
		collectAliasCarrierGoRefs(a.Target, a.Name, !aliasHasConcreteLeaf(a.Target), plan, refs)
		add("alias:"+a.Name, refs)
	}
	return findGraphCycle(nodes, edges)
}

// collectCarrierGoRefs records the carrier nodes t references at the Go level, as
// a class field / union arm lowers it (no self-drop). A class/alias reference and
// a multi-arm union carrier are node leaves; list/map/optional recurse.
func collectCarrierGoRefs(t schemadescriptor.Type, plan *carrierPlan, out map[string]bool) {
	switch t.Kind {
	case schemadescriptor.TypeClass:
		out["class:"+t.Name] = true
	case schemadescriptor.TypeRecursiveAlias:
		out["alias:"+t.Name] = true
	case schemadescriptor.TypeList:
		if t.Elem != nil {
			collectCarrierGoRefs(*t.Elem, plan, out)
		}
	case schemadescriptor.TypeMap:
		if t.Value != nil {
			collectCarrierGoRefs(*t.Value, plan, out)
		}
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return
		}
		if t.Union.Nullable && len(t.Union.Variants) == 1 {
			collectCarrierGoRefs(t.Union.Variants[0], plan, out)
			return
		}
		if name, ok := plan.unionName(t.Union.Variants); ok {
			out["union:"+name] = true
		}
	}
}

// collectAliasCarrierGoRefs mirrors lowerAliasTarget: a self-occurrence dropped to
// `any` contributes no reference; a multi-arm union resolves to its carrier node
// (its arms are edges FROM that union node, collected separately). Any other
// reference matches collectCarrierGoRefs.
func collectAliasCarrierGoRefs(t schemadescriptor.Type, self string, dropSelf bool, plan *carrierPlan, out map[string]bool) {
	switch t.Kind {
	case schemadescriptor.TypeRecursiveAlias:
		if t.Name == self && dropSelf {
			return
		}
		out["alias:"+t.Name] = true
	case schemadescriptor.TypeList:
		if t.Elem != nil {
			collectAliasCarrierGoRefs(*t.Elem, self, dropSelf, plan, out)
		}
	case schemadescriptor.TypeMap:
		if t.Value != nil {
			collectAliasCarrierGoRefs(*t.Value, self, dropSelf, plan, out)
		}
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return
		}
		if t.Union.Nullable && len(t.Union.Variants) == 1 {
			collectAliasCarrierGoRefs(t.Union.Variants[0], self, dropSelf, plan, out)
			return
		}
		if name, ok := plan.unionName(t.Union.Variants); ok {
			out["union:"+name] = true
		}
	default:
		collectCarrierGoRefs(t, plan, out)
	}
}

// nativeSpineCycleGuardSource is the recursion-safe marshal guard emitted into a
// recursive carrier file (gated on carrierGraphIsRecursive + a class/union carrier
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
