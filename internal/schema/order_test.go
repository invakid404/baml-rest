package schema

import "testing"

// enumNamesOf returns the canonical enum names of the bundle's Enums slice
// in slice order, the BAML-traversal hoist order the renderer consumes.
func enumNamesOf(b *Bundle) []string {
	out := make([]string, len(b.Enums))
	for i := range b.Enums {
		out[i] = b.Enums[i].Name.Name
	}
	return out
}

// classNamesOf returns the canonical class names of the bundle's Classes
// slice in slice order.
func classNamesOf(b *Bundle) []string {
	out := make([]string, len(b.Classes))
	for i := range b.Classes {
		out[i] = b.Classes[i].Name.Name
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOrderEnumsByReachabilityReverse pins the keystone behaviour: BAML's
// LIFO traversal queues the top-level fields in field order and pops the
// last first, so two enums referenced as fields First then Second land in
// Bundle.Enums as [Second, First] — reverse of reference, and independent
// of the declared (OrderedMap) order, which here is First, Second.
func TestOrderEnumsByReachabilityReverse(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"First": {"values": [{"name": "A"}]},
			"Second": {"values": [{"name": "B"}]}
		},
		"properties": {
			"first": {"ref": "First"},
			"second": {"ref": "Second"}
		}
	}`)
	got := enumNamesOf(b)
	if want := []string{"Second", "First"}; !eqStrings(got, want) {
		t.Errorf("enum order = %v, want %v (LIFO reverse-of-reference, not declared order)", got, want)
	}
}

// TestOrderNestedClassDrivesEnumOrder proves field traversal inside a
// reached class drives enum order: the target reaches Envelope, whose
// fields reference First then Second, so the enums hoist as [Second, First].
func TestOrderNestedClassDrivesEnumOrder(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"First": {"values": [{"name": "A"}]},
			"Second": {"values": [{"name": "B"}]}
		},
		"classes": {
			"Envelope": {"properties": {
				"first": {"ref": "First"},
				"second": {"ref": "Second"}
			}}
		},
		"properties": {"env": {"ref": "Envelope"}}
	}`)
	if got, want := enumNamesOf(b), []string{"Second", "First"}; !eqStrings(got, want) {
		t.Errorf("enum order = %v, want %v", got, want)
	}
	if got, want := classNamesOf(b), []string{dynamicOutputClassName, "Envelope"}; !eqStrings(got, want) {
		t.Errorf("class order = %v, want %v", got, want)
	}
}

// TestOrderClassesByReachabilityReverse pins class slice order: the
// synthetic target is appended first (popped first), then its field classes
// in reverse-of-reference order. Fields reference A then B, so classes are
// [synthetic, B, A].
func TestOrderClassesByReachabilityReverse(t *testing.T) {
	b := mustBuild(t, `{
		"classes": {
			"A": {"properties": {"x": {"type": "string"}}},
			"B": {"properties": {"y": {"type": "string"}}}
		},
		"properties": {
			"a": {"ref": "A"},
			"b": {"ref": "B"}
		}
	}`)
	if got, want := classNamesOf(b), []string{dynamicOutputClassName, "B", "A"}; !eqStrings(got, want) {
		t.Errorf("class order = %v, want %v", got, want)
	}
}

// TestOrderMapValueBeforeKey documents BAML's map push discipline: a map
// pushes key THEN value, so LIFO processes the VALUE first. With an enum
// key and a distinct enum value the resulting enum order is [Value, Key].
func TestOrderMapValueBeforeKey(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"KeyEnum": {"values": [{"name": "K"}]},
			"ValEnum": {"values": [{"name": "V"}]}
		},
		"properties": {
			"m": {"type": "map", "keys": {"ref": "KeyEnum"}, "values": {"ref": "ValEnum"}}
		}
	}`)
	if got, want := enumNamesOf(b), []string{"ValEnum", "KeyEnum"}; !eqStrings(got, want) {
		t.Errorf("enum order = %v, want %v (value processed before key)", got, want)
	}
}

// TestOrderUnionMemberOrder documents union push order: members are pushed
// in iter_include_null order (non-null first), so LIFO pops the LAST member
// first. A union of First then Second yields enums [Second, First].
func TestOrderUnionMemberOrder(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"First": {"values": [{"name": "A"}]},
			"Second": {"values": [{"name": "B"}]}
		},
		"properties": {
			"u": {"type": "union", "oneOf": [{"ref": "First"}, {"ref": "Second"}]}
		}
	}`)
	if got, want := enumNamesOf(b), []string{"Second", "First"}; !eqStrings(got, want) {
		t.Errorf("enum order = %v, want %v", got, want)
	}
}

// TestOrderDedupKeepsFirstPop pins the dedup contract: two fields
// referencing the same enum collapse to ONE definition, positioned by the
// FIRST pop. Fields reference Shared, Other, Shared; LIFO pops the second
// Shared first (appending Shared), then Other, and the first field's Shared
// is discarded on pop. Result: [Shared, Other].
func TestOrderDedupKeepsFirstPop(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"Shared": {"values": [{"name": "S"}]},
			"Other": {"values": [{"name": "O"}]}
		},
		"properties": {
			"a": {"ref": "Shared"},
			"b": {"ref": "Other"},
			"c": {"ref": "Shared"}
		}
	}`)
	if got, want := enumNamesOf(b), []string{"Shared", "Other"}; !eqStrings(got, want) {
		t.Errorf("enum order = %v, want %v (single Shared def at first pop)", got, want)
	}
}

// TestReachabilityPrunesUnreferenced documents the approved BAML-parity
// consequence: an unreferenced dynamic enum or class is NOT included in the
// bundle, because relevant_data_models only returns definitions reachable
// from the target. The declared-but-unused Ghost enum and Orphan class are
// absent from the bundle's slices and indexes.
func TestReachabilityPrunesUnreferenced(t *testing.T) {
	b := mustBuild(t, `{
		"enums": {
			"Used": {"values": [{"name": "U"}]},
			"Ghost": {"values": [{"name": "G"}]}
		},
		"classes": {
			"Orphan": {"properties": {"z": {"type": "string"}}}
		},
		"properties": {"used": {"ref": "Used"}}
	}`)

	if got, want := enumNamesOf(b), []string{"Used"}; !eqStrings(got, want) {
		t.Errorf("enum slice = %v, want only the reachable %v", got, want)
	}
	if _, ok := b.FindEnum("Ghost"); ok {
		t.Error("unreferenced enum Ghost must be pruned from the bundle")
	}
	if _, ok := b.FindClass("Orphan", NonStreaming); ok {
		t.Error("unreferenced class Orphan must be pruned from the bundle")
	}
	if got, want := classNamesOf(b), []string{dynamicOutputClassName}; !eqStrings(got, want) {
		t.Errorf("class slice = %v, want only the synthetic target %v", got, want)
	}
}
