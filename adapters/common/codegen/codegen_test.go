package codegen

import (
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
)

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
