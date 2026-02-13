package codegen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
)

// Self-referential types for testing cycle detection in structContainsMedia.
// These simulate the patterns BAML supports via recursive type aliases.

// selfRefDirect is a struct with a pointer to itself (via slice).
// Simulates: class Node { children Node[]? }
type selfRefDirect struct {
	Value    string
	Children *[]selfRefDirect
}

// mutualA and mutualB form a mutual recursion cycle.
// Simulates: class A { b B? } / class B { a A? }
type mutualA struct {
	Value string
	B     *mutualB
}
type mutualB struct {
	Value string
	A     *mutualA
}

// deepCycle has a longer cycle: deepA -> deepB -> deepC -> deepA
type deepCycleA struct {
	Next *deepCycleB
}
type deepCycleB struct {
	Next *deepCycleC
}
type deepCycleC struct {
	Next *deepCycleA
}

// selfRefViaSlice uses a slice (not pointer) as the recursion vehicle.
// Simulates: type JsonValue = int | string | JsonValue[]
type selfRefViaSlice struct {
	Text     string
	Children []selfRefViaSlice
}

// selfRefViaMap uses a map value type (struct containing itself).
// Note: structContainsMedia only walks struct fields, not map values,
// so this tests that it doesn't crash on map types.
type selfRefViaMap struct {
	Data   string
	Nested map[string]selfRefViaMap
}

// flatStruct has no recursion and no media.
type flatStruct struct {
	Name string
	Age  int
}

func TestStructContainsMedia_SelfReferentialTypes(t *testing.T) {
	tests := []struct {
		name     string
		typ      reflect.Type
		expected bool
	}{
		{
			name:     "self-referential via pointer to slice",
			typ:      reflect.TypeOf(selfRefDirect{}),
			expected: false,
		},
		{
			name:     "pointer to self-referential",
			typ:      reflect.TypeOf((*selfRefDirect)(nil)),
			expected: false,
		},
		{
			name:     "slice of self-referential",
			typ:      reflect.TypeOf([]selfRefDirect{}),
			expected: false,
		},
		{
			name:     "mutual recursion A",
			typ:      reflect.TypeOf(mutualA{}),
			expected: false,
		},
		{
			name:     "mutual recursion B",
			typ:      reflect.TypeOf(mutualB{}),
			expected: false,
		},
		{
			name:     "deep cycle A->B->C->A",
			typ:      reflect.TypeOf(deepCycleA{}),
			expected: false,
		},
		{
			name:     "self-referential via slice field",
			typ:      reflect.TypeOf(selfRefViaSlice{}),
			expected: false,
		},
		{
			name:     "self-referential via map",
			typ:      reflect.TypeOf(selfRefViaMap{}),
			expected: false,
		},
		{
			name:     "flat struct (no recursion, no media)",
			typ:      reflect.TypeOf(flatStruct{}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The primary assertion: this must terminate (not stack overflow).
			// We call it in a goroutine-safe way; if cycle detection is broken,
			// this will stack overflow and crash the test process.
			result := structContainsMedia(tt.typ)
			if result != tt.expected {
				t.Errorf("structContainsMedia(%v) = %v, want %v", tt.typ, result, tt.expected)
			}
		})
	}
}

func TestStructContainsMediaVisited_CycleBreaks(t *testing.T) {
	// Verify that the visited set actually prevents re-entry.
	// Pre-populate visited with the type and confirm it returns false immediately.
	typ := reflect.TypeOf(selfRefDirect{})
	visited := map[reflect.Type]bool{typ: true}

	if structContainsMediaVisited(typ, visited) {
		t.Error("structContainsMediaVisited should return false for already-visited type")
	}
}

func TestStructContainsMediaVisited_SharedVisitedAcrossFields(t *testing.T) {
	// When scanning mutualA, visiting mutualB should add it to visited,
	// so if mutualB references mutualA again, it doesn't re-enter.
	visited := make(map[reflect.Type]bool)
	structContainsMediaVisited(reflect.TypeOf(mutualA{}), visited)

	// Both types should now be in visited
	if !visited[reflect.TypeOf(mutualA{})] {
		t.Error("mutualA should be in visited set")
	}
	if !visited[reflect.TypeOf(mutualB{})] {
		t.Error("mutualB should be in visited set after scanning mutualA")
	}
}

func TestEnumValueAttrsCode(t *testing.T) {
	// Get the generated code
	code := enumValueAttrsCode()

	// Should have 3 statements (Description, Alias, Skip)
	if len(code) != 3 {
		t.Errorf("enumValueAttrsCode() returned %d statements, want 3", len(code))
	}

	// Render and verify the output
	// We'll create a dummy function to contain the code so we can render it
	f := jen.NewFile("test")
	f.Func().Id("test").Params().Block(code...)

	output := f.GoString()

	// Check for Description handling
	if !strings.Contains(output, `v.Description != ""`) {
		t.Error("enumValueAttrsCode() missing Description check")
	}
	if !strings.Contains(output, `vb.SetDescription(v.Description)`) {
		t.Error("enumValueAttrsCode() missing SetDescription call")
	}

	// Check for Alias handling
	if !strings.Contains(output, `v.Alias != ""`) {
		t.Error("enumValueAttrsCode() missing Alias check")
	}
	if !strings.Contains(output, `vb.SetAlias(v.Alias)`) {
		t.Error("enumValueAttrsCode() missing SetAlias call")
	}

	// Check for Skip handling
	if !strings.Contains(output, `v.Skip`) {
		t.Error("enumValueAttrsCode() missing Skip check")
	}
	if !strings.Contains(output, `vb.SetSkip(true)`) {
		t.Error("enumValueAttrsCode() missing SetSkip call")
	}
}

func TestEnumValueAttrsCode_Order(t *testing.T) {
	// The order should be: Description, Alias, Skip
	code := enumValueAttrsCode()

	f := jen.NewFile("test")
	f.Func().Id("test").Params().Block(code...)
	output := f.GoString()

	descIdx := strings.Index(output, "SetDescription")
	aliasIdx := strings.Index(output, "SetAlias")
	skipIdx := strings.Index(output, "SetSkip")

	if descIdx == -1 || aliasIdx == -1 || skipIdx == -1 {
		t.Fatal("Missing expected method calls in output")
	}

	if !(descIdx < aliasIdx && aliasIdx < skipIdx) {
		t.Error("enumValueAttrsCode() methods not in expected order (Description, Alias, Skip)")
	}
}
