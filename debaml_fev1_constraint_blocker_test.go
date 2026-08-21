package baml_rest

// De-BAML serving cutover S3b — the MULTI-CHECK WIRE-ORDER BLOCKER guard.
//
// # The blocker, and why this file is its evidence
//
// The cutover scope makes fe-v1 enrollment CONDITIONAL: before the real inventory
// entry may be enrolled, its dynamic output schema must have no unsupported
// checks/asserts. If it has — or later gains — multiple checks/asserts, enrollment
// is BLOCKED until native reproduces stock v0.223's declaration-order wire/error
// tree, and the same-response BAML oracle does NOT waive that (relying on it would
// yield parse-only outcomes, which fe-v1 prohibits).
//
// The check passes for S3b, and it passes STRUCTURALLY rather than by inspection of
// one chosen configuration: the dynamic `/call` surface has no constraint channel
// at all.
//
//   - The wire types a dynamic output schema is made of — DynamicOutputSchema,
//     DynamicProperty, DynamicTypeSpec, DynamicClass, DynamicEnum, DynamicEnumValue
//     — declare no check/assert/constraint field. There is no JSON key a caller
//     could send one under, so no fe-v1 request can carry one.
//   - The generated dynamic BAML function's output class (`Baml_Rest_DynamicOutput`)
//     is an empty `@@dynamic` class with no block-level `@@check`/`@@assert`. The
//     whole output type is supplied per request through the channel above, so there
//     is no static constraint to inherit either.
//
// Both halves are asserted over the actual source, and both are paired with a
// mutation bite, because the value of this file is entirely in what it does when
// the premise stops holding. The day someone adds a `checks` field to the dynamic
// wire schema, or a `@@check` to the generated output class, the fe-v1 blocker
// RE-OPENS — and this test is what says so, at the exact commit that does it,
// instead of leaving a stale "verified constraint-free" note in a PR description.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// dynamicSchemaWireTypes is the closed set of Go types a dynamic `/call` output
// schema decodes into. If a new one is introduced it must be added here — a type
// this list does not know is a constraint channel this guard cannot see, which is
// exactly the drift the list exists to make visible.
func dynamicSchemaWireTypes() []string {
	return []string{
		"DynamicOutputSchema",
		"DynamicProperty",
		"DynamicTypeSpec",
		"DynamicClass",
		"DynamicEnum",
		"DynamicEnumValue",
	}
}

// dynamicSchemaSourceFiles are the files those types are declared in.
func dynamicSchemaSourceFiles() []string {
	return []string{"bamlutils/interfaces.go", "bamlutils/dynamic.go"}
}

// constraintChannelToken matches the ways a constraint channel would be spelled on
// a wire type: a Go field name or a JSON key containing check/assert/constraint.
// It is deliberately broad — a guard against a future addition has to over-match
// rather than enumerate the spelling someone will pick.
var constraintChannelToken = regexp.MustCompile(`(?i)(check|assert|constraint)`)

// constraintFieldsIn reports every (type, field) on the named types whose Go name
// or struct tag looks like a constraint channel.
func constraintFieldsIn(t *testing.T, src string, wanted []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	want := map[string]bool{}
	for _, n := range wanted {
		want[n] = true
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !want[ts.Name.Name] {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, f := range st.Fields.List {
			tag := ""
			if f.Tag != nil {
				tag = f.Tag.Value
			}
			for _, name := range f.Names {
				if constraintChannelToken.MatchString(name.Name) || constraintChannelToken.MatchString(tag) {
					found = append(found, ts.Name.Name+"."+name.Name)
				}
			}
		}
		return true
	})
	return found
}

// TestDynamicOutputSchemaHasNoConstraintChannel is the first half of the blocker
// check: no fe-v1 request can carry a check or an assert, because the wire types
// have nowhere to put one.
func TestDynamicOutputSchemaHasNoConstraintChannel(t *testing.T) {
	seen := 0
	for _, src := range dynamicSchemaSourceFiles() {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			for _, want := range dynamicSchemaWireTypes() {
				if ts.Name.Name == want {
					seen++
				}
			}
			return true
		})
		if found := constraintFieldsIn(t, src, dynamicSchemaWireTypes()); len(found) != 0 {
			t.Errorf("%s: the dynamic output-schema wire types now declare a constraint channel (%s).\n"+
				"The de-BAML serving cutover's MULTI-CHECK WIRE-ORDER BLOCKER re-opens: the fe-v1 enrollment "+
				"assumes a constraint-free dynamic output schema, and a request that can carry checks/asserts "+
				"invalidates that assumption. Native must reproduce stock v0.223's declaration-order wire/error "+
				"tree, with a served-path differential and a swapped-order biting mutation, before this may pass.",
				src, strings.Join(found, ", "))
		}
	}
	if seen != len(dynamicSchemaWireTypes()) {
		t.Fatalf("found %d of the %d dynamic output-schema wire types in %v; the scan is not looking at what it claims to",
			seen, len(dynamicSchemaWireTypes()), dynamicSchemaSourceFiles())
	}
}

// TestConstraintChannelScannerBites is the mutation bite for the scanner above: it
// runs the SAME extraction over a synthetic source that DOES declare a constraint
// channel, in each of the two shapes a real addition could take, and requires each
// to be reported.
func TestConstraintChannelScannerBites(t *testing.T) {
	for _, mutant := range []struct {
		name string
		src  string
	}{
		{"a Go field name", "package p\n\ntype DynamicProperty struct {\n\tType string\n\tChecks []string\n}\n"},
		{"a JSON tag only", "package p\n\ntype DynamicProperty struct {\n\tType string\n\tX []string `json:\"asserts\"`\n}\n"},
	} {
		t.Run(mutant.name, func(t *testing.T) {
			path := t.TempDir() + "/mutant.go"
			if err := os.WriteFile(path, []byte(mutant.src), 0o600); err != nil {
				t.Fatalf("write mutant: %v", err)
			}
			if found := constraintFieldsIn(t, path, dynamicSchemaWireTypes()); len(found) == 0 {
				t.Fatal("the scanner did not see a constraint channel it was shown; the guard above is vacuous")
			}
		})
	}
}

// generatedDynamicBAML is the generated dynamic function + output class the public
// `/call` route serves. It is regenerated by cmd/regenerate-dynclient, so a change
// to it is a committed, reviewable diff.
const generatedDynamicBAML = "dynclient/internal/generated/baml_src/dynamic.baml"

// bamlConstraintAttr matches BAML's constraint attributes in either the field
// (`@check` / `@assert`) or the block (`@@check` / `@@assert`) position.
var bamlConstraintAttr = regexp.MustCompile(`@@?(?:check|assert)\b`)

// TestGeneratedDynamicOutputClassCarriesNoConstraint is the second half of the
// blocker check: the static side of the dynamic route declares no constraint
// either, so there is none for a per-request schema to inherit.
func TestGeneratedDynamicOutputClassCarriesNoConstraint(t *testing.T) {
	raw, err := os.ReadFile(generatedDynamicBAML)
	if err != nil {
		t.Fatalf("read %s: %v", generatedDynamicBAML, err)
	}
	src := string(raw)

	// The premise: this really is the file that declares the dynamic route's output
	// class. If the generator renamed either, the assertion below would pass while
	// checking nothing.
	for _, anchor := range []string{"class Baml_Rest_DynamicOutput", "@@dynamic", "function Baml_Rest_Dynamic("} {
		if !strings.Contains(src, anchor) {
			t.Fatalf("%s does not contain %q; this guard is not reading the generated dynamic route", generatedDynamicBAML, anchor)
		}
	}

	if hits := bamlConstraintAttr.FindAllString(src, -1); len(hits) != 0 {
		t.Errorf("%s now carries constraint attributes (%s).\n"+
			"The de-BAML serving cutover's MULTI-CHECK WIRE-ORDER BLOCKER re-opens: the fe-v1 enrollment "+
			"assumes the dynamic route's output schema is constraint-free. Enrollment stays blocked until "+
			"native reproduces stock v0.223's declaration-order wire/error tree, with a served-path "+
			"differential and a swapped-order biting mutation.",
			generatedDynamicBAML, strings.Join(hits, ", "))
	}

	// The bite: the same matcher over the same file with one constraint added must
	// FIRE, so a green run means "no constraint present" rather than "the matcher
	// never matches anything".
	mutated := strings.Replace(src, "class Baml_Rest_DynamicOutput {\n  @@dynamic\n}",
		"class Baml_Rest_DynamicOutput {\n  @@dynamic\n  @@assert(nonempty, {{ this != nil }})\n}", 1)
	if mutated == src {
		t.Fatal("the mutation anchor no longer matches the generated output class; the bite below would be vacuous")
	}
	if len(bamlConstraintAttr.FindAllString(mutated, -1)) == 0 {
		t.Fatal("the constraint matcher did not see an @@assert it was shown; the assertion above is vacuous")
	}
}
