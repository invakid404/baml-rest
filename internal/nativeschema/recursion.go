package nativeschema

import (
	"sort"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// This file is the de-BAML P3 slice-5 recursion-detection pass (#586). It
// classifies, over the WHOLE parsed project, which classes are recursive and
// which aliases are structural recursive aliases vs invalid (direct/degenerate)
// alias cycles, reproducing BAML v0.223's cycle detection exactly so the
// descriptor lowering (build.go) can emit recursive classes + structural
// recursive aliases in BAML output-format order and decline the shapes BAML
// rejects.
//
// It mirrors three BAML computations:
//
//   - finite_recursive_cycles (engine/baml-lib/parser-database/src/lib.rs
//     finalize_dependencies): Tarjan's SCC over the class dependency graph,
//     where a class depends on every class its fields reference through ANY
//     construct (list/map/union/tuple/optional), with non-recursive aliases
//     resolved (followed to their class refs) and recursive aliases skipped.
//     A class is recursive iff it is in an SCC of size > 1 or is a single class
//     that references itself.
//
//   - recursive_alias_cycles (parser-database/src/types/mod.rs
//     resolve_type_aliases): Tarjan's SCC over the FULL alias dependency graph,
//     where an alias depends on every alias its RHS references through any
//     construct. An alias in such a cycle is a structural recursive alias —
//     UNLESS the same cycle already exists without list/map edges, in which
//     case it is an invalid (infinite) cycle BAML rejects.
//
//   - the non-structural alias graph (baml-core/.../validations/cycle.rs
//     insert_required_alias_deps): the alias graph with list/map edges REMOVED
//     (only Symbol/Union/Tuple edges). A cycle here is a "these aliases form a
//     dependency cycle" validation error in BAML — a direct/degenerate alias
//     cycle (`type A = A`, `type A = A | int`, `type A = A?`). The builder
//     declines any function that reaches such an alias, fail-closed.
//
// Tarjan itself (tarjanSCC) reproduces parser-database/src/tarjan.rs member
// ordering (min-id-first rotation, ascending-id DFS) so recursive_classes /
// structural_recursive_aliases hoist in the exact order BAML's IndexSet/IndexMap
// carries — the render order is that slice order.

// recursionInfo is the project-wide recursion classification consumed by the
// descriptor builder. All maps are keyed by canonical type name.
type recursionInfo struct {
	// recursiveClass reports whether a class participates in a finite recursive
	// cycle (a class SCC of size > 1, or a single self-referential class).
	recursiveClass map[string]bool
	// classCycleOf maps each recursive class to the ordered members of its cycle
	// (BAML/Tarjan order). Every member of a cycle maps to the same slice, so
	// the builder can extend recursive_classes with the whole cycle when it
	// first pops any member (reproducing BAML's recursive_classes.extend(cycle)).
	classCycleOf map[string][]string

	// structuralAlias reports whether an alias is a structural recursive alias:
	// in a full-graph alias cycle that recurses THROUGH a list/map (so it is not
	// an invalid non-structural cycle).
	structuralAlias map[string]bool
	// structuralAliasCycleSize is the number of aliases in a structural alias's
	// cycle. The builder supports only single-member structural cycles (the
	// JSON-value shape) and declines larger ones (their hoist order is not
	// oracle-proven in this slice — see #586 slice-5 decline notes).
	structuralAliasCycleSize map[string]int

	// invalidAlias reports whether an alias is in a non-structural (direct/
	// degenerate) cycle — a cycle that survives removing list/map edges. BAML
	// rejects these at validation; the builder declines any function that
	// reaches one, fail-closed (never emits an approximation).
	invalidAlias map[string]bool
}

// recursion returns the project's recursion classification, computing it once
// and caching it on the index. Safe to call repeatedly (per function).
func (idx *schemaTypeIndex) recursion() *recursionInfo {
	if idx.rec == nil {
		idx.rec = analyzeRecursion(idx)
	}
	return idx.rec
}

// analyzeRecursion runs the full detection pass. Aliases are classified first
// (structural vs invalid), then the class graph is built with recursive aliases
// skipped — matching BAML's order (resolve_type_aliases runs before
// finalize_dependencies).
func analyzeRecursion(idx *schemaTypeIndex) *recursionInfo {
	info := &recursionInfo{
		recursiveClass:           make(map[string]bool),
		classCycleOf:             make(map[string][]string),
		structuralAlias:          make(map[string]bool),
		structuralAliasCycleSize: make(map[string]int),
		invalidAlias:             make(map[string]bool),
	}
	a := &recursionAnalyzer{idx: idx, info: info}
	a.classifyAliases()
	a.classifyClasses()
	return info
}

// recursionAnalyzer holds the per-analysis state (node-id spaces and the
// mutable recursionInfo being filled).
type recursionAnalyzer struct {
	idx  *schemaTypeIndex
	info *recursionInfo
}

// classifyAliases builds the full and non-structural alias graphs, runs Tarjan
// on each, and records structural vs invalid aliases. An alias in a
// non-structural cycle is invalid (takes precedence); an alias in a full-graph
// cycle that is not invalid is structural.
func (a *recursionAnalyzer) classifyAliases() {
	aliasID := make(map[string]int, len(a.idx.aliasDeclOrder))
	for i, name := range a.idx.aliasDeclOrder {
		aliasID[name] = i
	}

	full := newGraph(len(a.idx.aliasDeclOrder))
	nonStructural := newGraph(len(a.idx.aliasDeclOrder))
	for i, name := range a.idx.aliasDeclOrder {
		alias := a.idx.aliases[name]
		if alias == nil || alias.Expr == nil {
			continue
		}
		fullRefs := make(map[string]bool)
		a.collectAliasRefs(alias.Expr, true, fullRefs)
		for ref := range fullRefs {
			full[i][aliasID[ref]] = true
		}
		nsRefs := make(map[string]bool)
		a.collectAliasRefs(alias.Expr, false, nsRefs)
		for ref := range nsRefs {
			nonStructural[i][aliasID[ref]] = true
		}
	}

	for _, comp := range tarjanSCC(nonStructural) {
		for _, id := range comp {
			a.info.invalidAlias[a.idx.aliasDeclOrder[id]] = true
		}
	}
	for _, comp := range tarjanSCC(full) {
		size := len(comp)
		for _, id := range comp {
			name := a.idx.aliasDeclOrder[id]
			if a.info.invalidAlias[name] {
				continue
			}
			a.info.structuralAlias[name] = true
			a.info.structuralAliasCycleSize[name] = size
		}
	}
}

// classifyClasses builds the class dependency graph (non-recursive aliases
// resolved, recursive aliases skipped), runs Tarjan, and records recursive
// classes plus each cycle's member order.
func (a *recursionAnalyzer) classifyClasses() {
	classID := make(map[string]int, len(a.idx.classDeclOrder))
	for i, name := range a.idx.classDeclOrder {
		classID[name] = i
	}

	graph := newGraph(len(a.idx.classDeclOrder))
	for i, name := range a.idx.classDeclOrder {
		tb := a.idx.classes[name]
		if tb == nil {
			continue
		}
		deps := make(map[string]bool)
		for _, m := range tb.Fields {
			if m.Type != nil {
				a.collectClassDeps(m.Type, deps, make(map[string]bool))
			}
		}
		for dep := range deps {
			if id, ok := classID[dep]; ok {
				graph[i][id] = true
			}
		}
	}

	for _, comp := range tarjanSCC(graph) {
		members := make([]string, len(comp))
		for j, id := range comp {
			members[j] = a.idx.classDeclOrder[id]
		}
		for _, name := range members {
			a.info.recursiveClass[name] = true
			a.info.classCycleOf[name] = members
		}
	}
}

// collectAliasRefs walks a type expression and records every ALIAS name it
// references. When includeContainers is true it descends into list elements and
// map key/values (the full dependency graph); when false it stops at list/map
// (the non-structural graph, `insert_required_alias_deps`, which allows
// structural recursion only through list/map). Union/tuple/group are always
// transparent, matching BAML.
func (a *recursionAnalyzer) collectAliasRefs(t *bamlparser.TypeExpr, includeContainers bool, out map[string]bool) {
	if t == nil {
		return
	}
	switch t.Kind {
	case bamlparser.KindNameRef:
		if t.Namespaced || t.Path {
			return
		}
		if _, ok := a.idx.aliases[t.Name]; ok {
			out[t.Name] = true
		}
	case bamlparser.KindUnion:
		for _, v := range t.Variants {
			a.collectAliasRefs(v, includeContainers, out)
		}
	case bamlparser.KindTuple:
		for _, it := range t.Items {
			a.collectAliasRefs(it, includeContainers, out)
		}
	case bamlparser.KindGroup:
		a.collectAliasRefs(t.Inner, includeContainers, out)
	case bamlparser.KindList:
		if includeContainers {
			a.collectAliasRefs(t.Elem, includeContainers, out)
		}
	case bamlparser.KindMap:
		if includeContainers {
			a.collectAliasRefs(t.Key, includeContainers, out)
			a.collectAliasRefs(t.Value, includeContainers, out)
		}
	}
}

// collectClassDeps walks a type expression and records every CLASS name it
// reaches through any construct. A non-recursive alias is resolved (its RHS is
// followed) so a field `x: SomeAlias` that resolves to a class adds that class;
// a recursive alias (structural or invalid) is skipped, exactly like BAML's
// finalize_dependencies, because recursive aliases are not part of the finite
// class cycle. visitedAlias guards against pathological alias graphs (a
// non-recursive alias is acyclic by construction, so this only backstops).
func (a *recursionAnalyzer) collectClassDeps(t *bamlparser.TypeExpr, out, visitedAlias map[string]bool) {
	if t == nil {
		return
	}
	switch t.Kind {
	case bamlparser.KindNameRef:
		if t.Namespaced || t.Path {
			return
		}
		name := t.Name
		if _, ok := a.idx.classes[name]; ok {
			out[name] = true
			return
		}
		if _, ok := a.idx.enums[name]; ok {
			return
		}
		alias, ok := a.idx.aliases[name]
		if !ok {
			return
		}
		if a.info.structuralAlias[name] || a.info.invalidAlias[name] {
			return
		}
		if visitedAlias[name] {
			return
		}
		visitedAlias[name] = true
		a.collectClassDeps(alias.Expr, out, visitedAlias)
		delete(visitedAlias, name)
	case bamlparser.KindList:
		a.collectClassDeps(t.Elem, out, visitedAlias)
	case bamlparser.KindMap:
		a.collectClassDeps(t.Key, out, visitedAlias)
		a.collectClassDeps(t.Value, out, visitedAlias)
	case bamlparser.KindUnion:
		for _, v := range t.Variants {
			a.collectClassDeps(v, out, visitedAlias)
		}
	case bamlparser.KindTuple:
		for _, it := range t.Items {
			a.collectClassDeps(it, out, visitedAlias)
		}
	case bamlparser.KindGroup:
		a.collectClassDeps(t.Inner, out, visitedAlias)
	}
}

// newGraph returns an adjacency-set graph with n nodes (ids 0..n-1), every node
// present as a key (Tarjan iterates all keys and reads self-edges, so absent
// keys must not exist).
func newGraph(n int) map[int]map[int]bool {
	g := make(map[int]map[int]bool, n)
	for i := 0; i < n; i++ {
		g[i] = make(map[int]bool)
	}
	return g
}

// tarjanSCC runs Tarjan's strongly-connected-components algorithm over an
// int-node graph and returns the CYCLIC components in BAML's exact order,
// reproducing engine/baml-lib/parser-database/src/tarjan.rs:
//
//   - nodes and each node's successors are visited in ascending-id order, so
//     the DFS path is deterministic;
//   - a component is popped in reverse-DFS order, reversed to parent->child,
//     then rotated left so its minimum id is first (BAML's deterministic cycle
//     start);
//   - a component is returned only when it is a genuine cycle: size > 1, or a
//     single node with a self-edge;
//   - the returned components are sorted by their first (minimum) id.
//
// The member order of each returned component is exactly BAML's
// finite_recursive_cycles / recursive_alias_cycles member order, which drives
// recursive_classes / structural_recursive_aliases hoist order.
func tarjanSCC(graph map[int]map[int]bool) [][]int {
	t := &tarjanState{
		graph:   graph,
		idxOf:   make(map[int]int, len(graph)),
		lowlink: make(map[int]int, len(graph)),
		onStack: make(map[int]bool, len(graph)),
		visited: make(map[int]bool, len(graph)),
	}
	nodes := make([]int, 0, len(graph))
	for n := range graph {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	for _, n := range nodes {
		if !t.visited[n] {
			t.strongConnect(n)
		}
	}
	sort.Slice(t.components, func(i, j int) bool {
		return t.components[i][0] < t.components[j][0]
	})
	return t.components
}

type tarjanState struct {
	graph      map[int]map[int]bool
	index      int
	stack      []int
	idxOf      map[int]int
	lowlink    map[int]int
	onStack    map[int]bool
	visited    map[int]bool
	components [][]int
}

func (t *tarjanState) strongConnect(v int) {
	t.idxOf[v] = t.index
	t.lowlink[v] = t.index
	t.index++
	t.visited[v] = true
	t.stack = append(t.stack, v)
	t.onStack[v] = true

	succ := make([]int, 0, len(t.graph[v]))
	for w := range t.graph[v] {
		succ = append(succ, w)
	}
	sort.Ints(succ)
	for _, w := range succ {
		if !t.visited[w] {
			t.strongConnect(w)
			if t.lowlink[w] < t.lowlink[v] {
				t.lowlink[v] = t.lowlink[w]
			}
		} else if t.onStack[w] {
			if t.idxOf[w] < t.lowlink[v] {
				t.lowlink[v] = t.idxOf[w]
			}
		}
	}

	if t.lowlink[v] != t.idxOf[v] {
		return
	}
	var comp []int
	for {
		w := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		t.onStack[w] = false
		comp = append(comp, w)
		if w == v {
			break
		}
	}
	// Reverse to parent->child (BAML's component.reverse()).
	for i, j := 0, len(comp)-1; i < j; i, j = i+1, j-1 {
		comp[i], comp[j] = comp[j], comp[i]
	}
	// A cycle is size > 1 or a single self-referential node.
	if len(comp) == 1 && !t.graph[v][v] {
		return
	}
	// Rotate left so the minimum id is first (BAML's deterministic start node).
	minIdx := 0
	for i := 1; i < len(comp); i++ {
		if comp[i] < comp[minIdx] {
			minIdx = i
		}
	}
	rotated := make([]int, 0, len(comp))
	rotated = append(rotated, comp[minIdx:]...)
	rotated = append(rotated, comp[:minIdx]...)
	t.components = append(t.components, rotated)
}
