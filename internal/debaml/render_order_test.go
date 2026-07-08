package debaml

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// fieldTokenIndex returns the lowest index at which name appears as a
// rendered field-key token (`name:`) within window, or -1. It mirrors the
// integration test's helper so the unit assertion tracks the same tokens
// the end-to-end order tests pin.
func fieldTokenIndex(window, name string) int {
	if idx := strings.Index(window, name+":"); idx >= 0 {
		return idx
	}
	return -1
}

func assertRenderedFieldOrder(t *testing.T, label, prompt, anchor string, names []string) {
	t.Helper()
	window := prompt
	if anchor != "" {
		start := strings.Index(prompt, anchor)
		if start < 0 {
			t.Fatalf("%s: anchor %q not found in:\n%s", label, anchor, prompt)
		}
		rest := prompt[start+len(anchor):]
		end := strings.Index(rest, "}")
		if end < 0 {
			t.Fatalf("%s: closing brace not found after anchor %q", label, anchor)
		}
		window = rest[:end]
	}
	prev := -1
	for _, name := range names {
		idx := fieldTokenIndex(window, name)
		if idx < 0 {
			t.Fatalf("%s: field %q not found in window:\n%s", label, name, window)
		}
		if idx <= prev {
			t.Errorf("%s: field %q (at %d) must appear after the previous field (at %d)\nwindow:\n%s", label, name, idx, prev, window)
		}
		prev = idx
	}
}

// TestRender_HonorsPropertyFieldOrder locks the guarantee the preserve-order
// fix depends on: debaml.Render renders each class's fields in the order they
// appear in the DynamicOutputSchema. The wire-order schema renders in wire
// order; the alphabetized schema renders alphabetically. Combined with
// bamlutils' preserve=false normalization (which alphabetizes before this
// seam), that reproduces BAML's applyDynamicTypes sorted-key fallback.
func TestRender_HonorsPropertyFieldOrder(t *testing.T) {
	build := func(rootProps, profileProps bamlutils.OrderedMap[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
		return &bamlutils.DynamicOutputSchema{
			Properties: rootProps,
			Classes: bamlutils.MustOrderedMap(
				bamlutils.OrderedKV("Profile", &bamlutils.DynamicClass{Properties: profileProps}),
			),
		}
	}

	wire := build(
		bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("delta", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("alpha", &bamlutils.DynamicProperty{Type: "int"}),
			bamlutils.OrderedKV("charlie", &bamlutils.DynamicProperty{Type: "bool"}),
			bamlutils.OrderedKV("golf", &bamlutils.DynamicProperty{Ref: "Profile"}),
		),
		bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("zulu", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("alpha", &bamlutils.DynamicProperty{Type: "int"}),
			bamlutils.OrderedKV("status", &bamlutils.DynamicProperty{Type: "string"}),
		),
	)

	sorted := build(
		bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("alpha", &bamlutils.DynamicProperty{Type: "int"}),
			bamlutils.OrderedKV("charlie", &bamlutils.DynamicProperty{Type: "bool"}),
			bamlutils.OrderedKV("delta", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("golf", &bamlutils.DynamicProperty{Ref: "Profile"}),
		),
		bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("alpha", &bamlutils.DynamicProperty{Type: "int"}),
			bamlutils.OrderedKV("status", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("zulu", &bamlutils.DynamicProperty{Type: "string"}),
		),
	)

	wirePrompt, err := Render(wire)
	if err != nil {
		t.Fatalf("render wire schema: %v", err)
	}
	assertRenderedFieldOrder(t, "wire top-level", wirePrompt, "", []string{"delta", "alpha", "charlie", "golf"})
	assertRenderedFieldOrder(t, "wire Profile", wirePrompt, "golf: {", []string{"zulu", "alpha", "status"})

	sortedPrompt, err := Render(sorted)
	if err != nil {
		t.Fatalf("render sorted schema: %v", err)
	}
	assertRenderedFieldOrder(t, "sorted top-level", sortedPrompt, "", []string{"alpha", "charlie", "delta", "golf"})
	assertRenderedFieldOrder(t, "sorted Profile", sortedPrompt, "golf: {", []string{"alpha", "status", "zulu"})
}
