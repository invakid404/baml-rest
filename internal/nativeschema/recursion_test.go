package nativeschema

import (
	"reflect"
	"testing"
)

// mkGraph builds an adjacency-set graph from an edge list, ensuring every node
// (source or target) is present as a key so tarjanSCC sees the full vertex set.
func mkGraph(edges map[int][]int) map[int]map[int]bool {
	g := make(map[int]map[int]bool)
	ensure := func(n int) {
		if g[n] == nil {
			g[n] = make(map[int]bool)
		}
	}
	for src, dsts := range edges {
		ensure(src)
		for _, d := range dsts {
			ensure(d)
			g[src][d] = true
		}
	}
	return g
}

// TestTarjanSCCMatchesBAML reproduces BAML's own tarjan.rs unit vectors so the
// cycle member ordering (which drives recursive_classes / structural_recursive_aliases
// hoist order) is provably identical to the v0.223 oracle. The expected outputs
// are copied verbatim from engine/baml-lib/parser-database/src/tarjan.rs tests.
func TestTarjanSCCMatchesBAML(t *testing.T) {
	t.Run("find_cycles", func(t *testing.T) {
		// graph + expected from tarjan.rs::find_cycles. Note [7] is a component
		// because node 7 has a self-edge; nodes with no cycle are omitted.
		g := mkGraph(map[int][]int{
			0: {1},
			1: {2},
			2: {0},
			3: {1, 2, 4},
			4: {5, 3},
			5: {2, 6},
			6: {5},
			7: {4, 6, 7},
		})
		got := tarjanSCC(g)
		want := [][]int{{0, 1, 2}, {3, 4}, {5, 6}, {7}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tarjanSCC = %v, want %v", got, want)
		}
	})

	t.Run("no_cycles", func(t *testing.T) {
		// tarjan.rs::no_cycles_found — a DAG yields no components.
		g := mkGraph(map[int][]int{
			0: {1},
			1: {2, 3},
			2: {4},
			3: {5},
			4: nil,
			5: nil,
		})
		if got := tarjanSCC(g); len(got) != 0 {
			t.Errorf("tarjanSCC = %v, want no components", got)
		}
	})

	t.Run("single self loop", func(t *testing.T) {
		// A lone node pointing at itself is a cycle (component of size 1).
		g := mkGraph(map[int][]int{0: {0}, 1: {0}})
		got := tarjanSCC(g)
		want := [][]int{{0}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tarjanSCC = %v, want %v", got, want)
		}
	})

	t.Run("three cycle rotates to min id", func(t *testing.T) {
		// A directed 3-cycle whose edges are non-monotonic: 0->2->1->0. The
		// component is rotated so the minimum id (0) is first, then follows the
		// DFS path (0,2,1), NOT sorted order — this is exactly BAML's behavior
		// and the reason recursive_classes is not merely declaration-sorted.
		g := mkGraph(map[int][]int{0: {2}, 2: {1}, 1: {0}})
		got := tarjanSCC(g)
		want := [][]int{{0, 2, 1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tarjanSCC = %v, want %v", got, want)
		}
	})
}
