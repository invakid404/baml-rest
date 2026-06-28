package outputformat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// TestHoistAllClassOrderFromDynamic is the #534 class-order coverage.
//
// Non-recursive class definition order under hoist_classes=true is NOT
// observable through the production bare {{ ctx.output_format }} content
// part (it never hoists non-recursive classes), and a real BAML
// hoist_classes=true oracle for the dynamic path cannot be wired here: the
// only in-process dynamic runtime is the embedded Baml_Rest_Dynamic
// function, whose template is the bare ctx.output_format and whose target
// is named Baml_Rest_DynamicOutput. A static baml_src oracle would emit a
// differently-named target class (e.g. DynamicOutput) as a hoisted
// definition, breaking byte-parity, and forking the embedded dynamic
// runtime to add the kwarg is explicitly out of scope for #534. The real
// BAML oracle is therefore recorded as a follow-up (see internal/schema/
// order.go and the #534 PR).
//
// This fallback — authorized by the #534 scope — pins the class-definition
// ORDER end-to-end on the dynamic path by lowering through the real
// schema.FromDynamicOutputSchema traversal and rendering with HoistClasses:
// HoistAll. The renderer consumes bundle slice order (it never reorders),
// and that slice order is itself pinned against BAML in
// internal/schema/order_test.go, so this asserts the two layers compose:
// the BAML-reachability bundle order survives into the hoisted class block.
func TestHoistAllClassOrderFromDynamic(t *testing.T) {
	hoistAll := Options{HoistClasses: HoistClasses{Mode: HoistAll}}

	cases := []struct {
		name string
		raw  string
		// want is the expected order of hoisted class-definition headers
		// (the synthetic target included; it is hoisted by HoistAll too).
		want []string
	}{
		{
			// Top-level fields reference A then B. LIFO pops B first, so the
			// class definitions order as synthetic, B, A — reverse of
			// reference and independent of declared (A, B) order.
			name: "hoist_all_classes_top_level_reverse",
			raw: `{
				"classes": {
					"A": {"properties": {"x": {"type": "string"}}},
					"B": {"properties": {"y": {"type": "string"}}}
				},
				"properties": {"a": {"ref": "A"}, "b": {"ref": "B"}}
			}`,
			want: []string{"Baml_Rest_DynamicOutput", "B", "A"},
		},
		{
			// Target reaches Root; Root's fields reference A then B. Root is
			// popped (and appended) before its children, then B before A.
			name: "hoist_all_classes_nested_reverse",
			raw: `{
				"classes": {
					"Root": {"properties": {"a": {"ref": "A"}, "b": {"ref": "B"}}},
					"A": {"properties": {"x": {"type": "string"}}},
					"B": {"properties": {"y": {"type": "string"}}}
				},
				"properties": {"root": {"ref": "Root"}}
			}`,
			want: []string{"Baml_Rest_DynamicOutput", "Root", "B", "A"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := mustBuildDynamic(t, tc.raw)
			got, err := Render(bundle, hoistAll)
			if err != nil {
				t.Fatalf("Render(HoistAll): %v", err)
			}
			if order := classHeaderOrder(got, tc.want); !equalStrings(order, tc.want) {
				t.Errorf("class definition order = %v, want %v\n--- rendered ---\n%s", order, tc.want, got)
			}
		})
	}
}

// TestHoistAllEnumsBeforeClassesFromDynamic pins BAML's block ordering: all
// hoisted enum definitions render before any hoisted class definition, and
// within each group the BAML-reachability order holds. Enums hoist via a
// value description; classes hoist via HoistAll.
func TestHoistAllEnumsBeforeClassesFromDynamic(t *testing.T) {
	bundle := mustBuildDynamic(t, `{
		"enums": {
			"E1": {"values": [{"name": "X", "description": "x"}, {"name": "Y"}]},
			"E2": {"values": [{"name": "P", "description": "p"}, {"name": "Q"}]}
		},
		"classes": {
			"A": {"properties": {"e": {"ref": "E1"}}},
			"B": {"properties": {"e": {"ref": "E2"}}}
		},
		"properties": {"a": {"ref": "A"}, "b": {"ref": "B"}}
	}`)

	got, err := Render(bundle, Options{HoistClasses: HoistClasses{Mode: HoistAll}})
	if err != nil {
		t.Fatalf("Render(HoistAll): %v", err)
	}

	// Every enum definition header must precede every class definition
	// header. Enum headers are "<Name>\n----"; class headers are "<Name> {".
	lastEnum := strings.LastIndex(got, "\n----")
	firstClass := strings.Index(got, "Baml_Rest_DynamicOutput {")
	if lastEnum == -1 || firstClass == -1 {
		t.Fatalf("expected both enum and class definitions in output:\n%s", got)
	}
	if lastEnum > firstClass {
		t.Errorf("an enum definition rendered after a class definition (enum block must come first):\n%s", got)
	}

	// Class order: synthetic, then B, then A (LIFO reverse-of-reference).
	if order := classHeaderOrder(got, []string{"Baml_Rest_DynamicOutput", "B", "A"}); !equalStrings(order, []string{"Baml_Rest_DynamicOutput", "B", "A"}) {
		t.Errorf("class order = %v, want [Baml_Rest_DynamicOutput B A]\n%s", order, got)
	}
}

// mustBuildDynamic lowers wire JSON through the real construction path and
// fails the test on any error, so the fixture exercises the same traversal
// production uses.
func mustBuildDynamic(t *testing.T, raw string) *schema.Bundle {
	t.Helper()
	var s bamlutils.DynamicOutputSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode schema fixture: %v", err)
	}
	b, err := schema.FromDynamicOutputSchema(&s, schema.BuildOptions{})
	if err != nil {
		t.Fatalf("FromDynamicOutputSchema: %v", err)
	}
	return b
}

// classHeaderOrder returns the class names from want that appear as
// definition headers ("<name> {") in rendered, in the order they appear.
// Searching only for names in want avoids matching a bare hoisted
// reference inside a field (those have no following brace).
func classHeaderOrder(rendered string, want []string) []string {
	type hit struct {
		name string
		idx  int
	}
	var hits []hit
	for _, name := range want {
		if i := strings.Index(rendered, name+" {"); i >= 0 {
			hits = append(hits, hit{name, i})
		}
	}
	// Insertion sort by index (small N, stable, no imports).
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].idx < hits[j-1].idx; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]string, len(hits))
	for i := range hits {
		out[i] = hits[i].name
	}
	return out
}

func equalStrings(a, b []string) bool {
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
