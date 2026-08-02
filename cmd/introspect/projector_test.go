package main

// De-BAML Slice 7.1b — proof for the generated ARGUMENT PROJECTOR emitter
// (projector.go). Two legs:
//
//   - AUDIT: the build-time Go-AST check over a synthetic generated `types`
//     package. A well-formed enum/class/list graph is projectable; every
//     near-neighbour drift (missing struct, missing/duplicate/renamed json tag,
//     an unexported or embedded carrier, a wrong or non-slice Go type, an extra
//     tagged field, a non-string enum, a nullable/null node) omits the projector
//     and records a reason. Omission is the load-bearing outcome: it makes the
//     method a pre-render static decline instead of a reflection fallback.
//   - BEHAVIOUR: the emitted projector is COMPILED against a synthesized types
//     package and run, proving the emitted selectors read the right fields, the
//     canonical names/orders are the SOURCE ones (not the Go ones, and never a
//     display alias), list order is input order, and a wrong-typed or wrong-arity
//     argument vector returns ok=false rather than a partial binding.
//
// The behaviour leg needs the Go toolchain and is skipped under -short.

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// projectorTypesSrc is a synthetic stand-in for BAML v0.223's generated `types`
// package: enums as `type X string` with canonical-named constants, classes as
// structs whose json tags are the CANONICAL BAML field names (never the display
// aliases), int -> int64 and float -> float64.
const projectorTypesSrc = `package types

type Color string

const (
	ColorRED   Color = "RED"
	ColorGREEN Color = "GREEN"
	ColorBLUE  Color = "BLUE"
)

type Swatch struct {
	Color Color  ` + "`json:\"color\"`" + `
	Label string ` + "`json:\"label\"`" + `
}

type Palette struct {
	Primary Color   ` + "`json:\"primary\"`" + `
	Shades  []Color ` + "`json:\"shades\"`" + `
	Swatch  Swatch  ` + "`json:\"swatch\"`" + `
	Name    string  ` + "`json:\"name\"`" + `
	Count   int64   ` + "`json:\"count\"`" + `
	Ratio   float64 ` + "`json:\"ratio\"`" + `
	Flag    bool    ` + "`json:\"flag\"`" + `
}
`

// projectorUniverse is the V3 source universe matching projectorTypesSrc. The
// display aliases are DELIBERATELY different from the canonical names so a
// projector that ever wrote an alias where a canonical name belongs is caught.
func projectorUniverse() promptdescriptor.InputValueUniverse {
	return promptdescriptor.InputValueUniverse{
		ProjectEnums: []promptdescriptor.ResolvedEnum{{
			Name: "Color",
			Members: []promptdescriptor.ResolvedEnumMember{
				{Canonical: "RED", Alias: sp("rouge")},
				{Canonical: "GREEN", Alias: sp("vert")},
				{Canonical: "BLUE"},
			},
		}},
		Classes: []promptdescriptor.ResolvedClass{
			{Name: "Swatch", Fields: []promptdescriptor.ResolvedClassField{
				{Canonical: "color", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"}},
				{Canonical: "label", Alias: sp("etiquette"), Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
			}},
			{Name: "Palette", Fields: []promptdescriptor.ResolvedClassField{
				{Canonical: "primary", Alias: sp("principale"), Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"}},
				{Canonical: "shades", Type: promptdescriptor.ResolvedValueType{
					Kind: promptdescriptor.ValueList,
					Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
				}},
				{Canonical: "swatch", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"}},
				{Canonical: "name", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
				{Canonical: "count", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueInt}},
				{Canonical: "ratio", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueFloat}},
				{Canonical: "flag", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueBool}},
			}},
		},
	}
}

// indexFromSource parses a synthetic types package source into the audit index.
func indexFromSource(t *testing.T, pkgPath, src string) *typesIndex {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic types package: %v", err)
	}
	idx := newTypesIndex(pkgPath)
	idx.addFile(f)
	return idx
}

func vt(kind promptdescriptor.ValueKind) *promptdescriptor.ResolvedValueType {
	return &promptdescriptor.ResolvedValueType{Kind: kind}
}

// TestProjectorAuditAcceptsExactGeneratedCarriers proves the happy path: every
// V3 shape this slice claims has an exact generated carrier, and the audit
// records the deduplicated helper set it needs.
func TestProjectorAuditAcceptsExactGeneratedCarriers(t *testing.T) {
	idx := indexFromSource(t, "example.com/types", projectorTypesSrc)
	u := projectorUniverse()

	args := []*promptdescriptor.ResolvedValueType{
		vt(promptdescriptor.ValueString),
		vt(promptdescriptor.ValueInt),
		vt(promptdescriptor.ValueFloat),
		vt(promptdescriptor.ValueBool),
		{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
		{Kind: promptdescriptor.ValueClass, ClassName: "Palette"},
		{Kind: promptdescriptor.ValueList, Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Palette"}},
	}
	a := newProjectorAudit(idx, u)
	for i, arg := range args {
		if err := a.auditValueType(arg); err != nil {
			t.Fatalf("argument %d (%s) must be projectable: %v", i, arg.Kind, err)
		}
	}

	// The helper set is deduplicated by mangled name and reaches every nested
	// type (Palette's Swatch and []Color arrive only through the class walk).
	want := []string{
		"String", "Int", "Float", "Bool",
		"EnumColor", "ClassPalette", "ClassSwatch",
		"ListOfEnumColor", "ListOfClassPalette",
	}
	for _, k := range want {
		if _, ok := a.need[k]; !ok {
			t.Errorf("helper %q missing from the audited need set (have %v)", k, sortedKeysOf(a.need))
		}
	}
	if len(a.need) != len(want) {
		t.Errorf("need set = %v, want exactly %v", sortedKeysOf(a.need), want)
	}
}

func sortedKeysOf(m map[string]promptdescriptor.ResolvedValueType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small set; insertion sort keeps the helper dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// TestProjectorAuditNearNeighbourDeclines is the fail-closed half. Every row is
// a drift a reflection-based projector would silently absorb; each must instead
// omit the projector with a reason naming the problem.
func TestProjectorAuditNearNeighbourDeclines(t *testing.T) {
	cases := []struct {
		name    string
		typesGo string
		arg     *promptdescriptor.ResolvedValueType
		want    string
	}{
		{
			name:    "class_struct_absent",
			typesGo: "package types\n\ntype Color string\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "has no struct",
		},
		{
			name:    "enum_not_string_underlying",
			typesGo: "package types\n\ntype Color int\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			want:    "`type Color string`",
		},
		{
			name:    "enum_declared_twice",
			typesGo: "package types\n\ntype Color string\ntype Color string\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			want:    "unambiguous",
		},
		{
			name:    "field_json_tag_missing",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor Color `json:\"color\"`\n\tLabel string\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "json-tagged fields but the source class has",
		},
		{
			name:    "field_json_tag_renamed_to_alias",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor Color `json:\"color\"`\n\tLabel string `json:\"etiquette\"`\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    `has no field tagged json:"label"`,
		},
		{
			name:    "field_json_tag_duplicated",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor Color `json:\"color\"`\n\tLabel string `json:\"color\"`\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "more than one field tagged",
		},
		{
			name:    "extra_tagged_field",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor Color `json:\"color\"`\n\tLabel string `json:\"label\"`\n\tExtra string `json:\"extra\"`\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "json-tagged fields but the source class has",
		},
		{
			name:    "field_go_type_mismatch",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor string `json:\"color\"`\n\tLabel string `json:\"label\"`\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "does not carry the source field's type",
		},
		{
			name:    "field_unexported_carrier",
			typesGo: "package types\n\ntype Color string\ntype Swatch struct {\n\tColor Color `json:\"color\"`\n\tlabel string `json:\"label\"`\n}\n",
			arg:     &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Swatch"},
			want:    "embedded or unexported",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			idx := indexFromSource(t, "example.com/types", c.typesGo)
			a := newProjectorAudit(idx, projectorUniverse())
			err := a.auditValueType(c.arg)
			if err == nil {
				t.Fatalf("expected an audit decline, got none")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("audit reason %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

// TestProjectorAuditRefusesUnprovenShapes pins the shapes this slice REFUSES to
// project even when a Go carrier exists: a nullable edge (whose nil case has no
// stock differential), the bare null type, and an enum/class not present in the
// descriptor's own V3 universe (which would make the projector and the binder
// disagree about what the value means).
func TestProjectorAuditRefusesUnprovenShapes(t *testing.T) {
	idx := indexFromSource(t, "example.com/types", projectorTypesSrc)
	cases := []struct {
		name string
		arg  *promptdescriptor.ResolvedValueType
		want string
	}{
		{"nil_value_type", nil, "no resolved V3 value type"},
		{"nullable_scalar", &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString, Nullable: true}, "nullable"},
		{"nullable_enum", &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color", Nullable: true}, "nullable"},
		{"null_kind", vt(promptdescriptor.ValueNull), "null value type"},
		{"enum_outside_universe", &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Missing"}, "not in the descriptor's V3 universe"},
		{"class_outside_universe", &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Missing"}, "not in the descriptor's V3 universe"},
		{"list_without_element", &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList}, "no element type"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			a := newProjectorAudit(idx, projectorUniverse())
			err := a.auditValueType(c.arg)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("reason %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

// projectorFixtureDescriptors is the emitted-projector corpus: one method per
// admitted shape plus one deliberately unprojectable method.
func projectorFixtureDescriptors() map[string]promptdescriptor.Function {
	u := projectorUniverse()
	fn := func(method string, args ...promptdescriptor.Argument) promptdescriptor.Function {
		return promptdescriptor.Function{
			Version: promptdescriptor.Version, Method: method,
			Client: "C", Provider: "openai",
			Args: args, InputValues: u,
		}
	}
	return map[string]promptdescriptor.Function{
		"NoArgs":     fn("NoArgs"),
		"Scalars":    fn("Scalars", promptdescriptor.Argument{Name: "s", ValueType: vt(promptdescriptor.ValueString)}, promptdescriptor.Argument{Name: "i", ValueType: vt(promptdescriptor.ValueInt)}, promptdescriptor.Argument{Name: "f", ValueType: vt(promptdescriptor.ValueFloat)}, promptdescriptor.Argument{Name: "b", ValueType: vt(promptdescriptor.ValueBool)}),
		"EnumArg":    fn("EnumArg", promptdescriptor.Argument{Name: "color", ValueType: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"}}),
		"ClassArg":   fn("ClassArg", promptdescriptor.Argument{Name: "palette", ValueType: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Palette"}}),
		"ListArg":    fn("ListArg", promptdescriptor.Argument{Name: "colors", ValueType: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList, Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"}}}),
		"NestedList": fn("NestedList", promptdescriptor.Argument{Name: "palettes", ValueType: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList, Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Palette"}}}),
		// Unprojectable: a nullable enum has a Go carrier but no proven nil render.
		"Unprojectable": fn("Unprojectable", promptdescriptor.Argument{Name: "maybe", ValueType: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color", Nullable: true}}),
	}
}

func emitProjectors(t *testing.T, idx *typesIndex, descriptors map[string]promptdescriptor.Function) string {
	t.Helper()
	out := jen.NewFile("introspected")
	emitStaticPromptArgumentProjectors(out, &config{InterfacesPkg: gateAInterfacesPkg}, descriptors, idx)
	var b strings.Builder
	if err := out.Render(&b); err != nil {
		t.Fatalf("render projectors: %v", err)
	}
	return b.String()
}

// TestProjectorEmissionDeterministicAndPartitioned proves the emitted registry
// is byte-stable across differently-iterated maps, that its method keys and the
// decline ledger are sorted, and that the two are disjoint.
func TestProjectorEmissionDeterministicAndPartitioned(t *testing.T) {
	idx := indexFromSource(t, "example.com/types", projectorTypesSrc)
	descriptors := projectorFixtureDescriptors()

	first := emitProjectors(t, idx, descriptors)
	second := emitProjectors(t, idx, copyFnMap(descriptors))
	if first != second {
		t.Fatalf("projector emission is not deterministic")
	}

	assertSortedKeys(t, first, `"([A-Za-z0-9_]+)": func\(args \[\]any\)`, "StaticPromptArgumentProjectors")

	if !strings.Contains(first, `"Unprojectable":`) {
		t.Error("the unprojectable method must appear in StaticPromptProjectorDeclines")
	}
	if strings.Contains(first, `"Unprojectable": func(args []any)`) {
		t.Error("the unprojectable method must NOT get a projector")
	}

	// Each helper is emitted exactly once even though several methods reach it.
	for _, helper := range []string{"staticPromptValueEnumColor", "staticPromptValueClassPalette", "staticPromptValueClassSwatch"} {
		if n := strings.Count(first, "func "+helper+"("); n != 1 {
			t.Errorf("helper %s declared %d times, want exactly 1", helper, n)
		}
	}

	// No reflection / JSON / Encode escape hatch may appear in generated code.
	for _, banned := range []string{"reflect.", "json.Marshal", ".Encode()", "fmt.Sprint"} {
		if strings.Contains(first, banned) {
			t.Errorf("generated projector source must not contain %q", banned)
		}
	}
}

// TestProjectorEmittedBehaviour compiles the emitted projectors against the
// synthetic types package and runs them. This is the leg that proves the
// SELECTORS are right — an audit alone cannot show that `v.Primary` is what
// `json:"primary"` named.
func TestProjectorEmittedBehaviour(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess compile/behaviour harness in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	repoRoot := gateARepoRoot(t)
	dir := t.TempDir()

	const modPath = "projectorharness"
	typesPkg := modPath + "/types"

	if err := os.MkdirAll(filepath.Join(dir, "types"), 0o755); err != nil {
		t.Fatalf("mkdir types: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "types", "types.go"), []byte(projectorTypesSrc), 0o644); err != nil {
		t.Fatalf("write types package: %v", err)
	}

	idx := indexFromSource(t, typesPkg, projectorTypesSrc)
	out := jen.NewFile("introspected")
	emitStaticPromptArgumentProjectors(out, &config{InterfacesPkg: gateAInterfacesPkg}, projectorFixtureDescriptors(), idx)
	if err := out.Save(filepath.Join(dir, "introspected.go")); err != nil {
		t.Fatalf("save emitted projectors: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness_test.go"), []byte(projectorHarness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	bamlutilsAbs := filepath.Join(repoRoot, "bamlutils")
	goMod := fmt.Sprintf("module %s\n\ngo %s\n\nrequire %s v0.0.0\n\nreplace %s => %s\n",
		modPath, gateAGoVersion(t, repoRoot), gateAInterfacesPkg, gateAInterfacesPkg, filepath.ToSlash(bamlutilsAbs))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("write go.sum: %v", err)
		}
	}

	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOWORK=off",
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
	)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emitted-projector harness failed: %v\n%s", err, outBytes)
	}
}

// projectorHarness is the in-module test the behaviour leg compiles and runs
// against the emitted projectors.
const projectorHarness = `package introspected

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"

	"projectorharness/types"
)

func TestNoArgsProjectsAnEmptyVector(t *testing.T) {
	vals, ok := StaticPromptArgumentValues("NoArgs", nil)
	if !ok {
		t.Fatal("a no-argument method must project an EMPTY vector, not decline")
	}
	if len(vals) != 0 {
		t.Fatalf("want 0 values, got %d", len(vals))
	}
	if _, ok := StaticPromptArgumentValues("NoArgs", []any{"x"}); ok {
		t.Error("an over-long argument vector must decline")
	}
}

func TestScalarsProjectExactGoTypes(t *testing.T) {
	vals, ok := StaticPromptArgumentValues("Scalars", []any{"s", int64(7), 2.5, true})
	if !ok {
		t.Fatal("exact Go scalars must project")
	}
	want := []promptdescriptor.ArgumentValue{
		{Name: "s", Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticString, String: "s"}},
		{Name: "i", Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticInt, Int: 7}},
		{Name: "f", Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticFloat, Float: 2.5}},
		{Name: "b", Value: promptdescriptor.StaticValue{Kind: promptdescriptor.StaticBool, Bool: true}},
	}
	if len(vals) != len(want) {
		t.Fatalf("want %d values, got %d", len(want), len(vals))
	}
	for i := range want {
		g, w := vals[i], want[i]
		if g.Name != w.Name || g.Value.Kind != w.Value.Kind || g.Value.String != w.Value.String ||
			g.Value.Int != w.Value.Int || g.Value.Float != w.Value.Float || g.Value.Bool != w.Value.Bool ||
			len(g.Value.Fields) != 0 || len(g.Value.Items) != 0 {
			t.Errorf("value %d = %+v, want %+v", i, g, w)
		}
	}

	// No coercion: an int where an int64 is declared, and a wrong order, decline.
	if _, ok := StaticPromptArgumentValues("Scalars", []any{"s", 7, 2.5, true}); ok {
		t.Error("an int (not int64) must decline")
	}
	if _, ok := StaticPromptArgumentValues("Scalars", []any{int64(7), "s", 2.5, true}); ok {
		t.Error("a permuted argument vector must decline")
	}
	if _, ok := StaticPromptArgumentValues("Scalars", []any{"s", int64(7), 2.5}); ok {
		t.Error("a short argument vector must decline")
	}
}

func TestEnumProjectsCanonicalNotAlias(t *testing.T) {
	vals, ok := StaticPromptArgumentValues("EnumArg", []any{types.ColorRED})
	if !ok {
		t.Fatal("a generated enum value must project")
	}
	got := vals[0].Value
	if got.Kind != promptdescriptor.StaticEnum || got.TypeName != "Color" || got.Canonical != "RED" {
		t.Fatalf("enum projected as %+v, want Color/RED", got)
	}
	// The display alias ("rouge") must never appear in the projected value.
	if got.String != "" || got.Canonical == "rouge" {
		t.Errorf("projector leaked a display alias: %+v", got)
	}
	// An out-of-range enum string still projects (the BINDER validates it against
	// V3); the projector must not silently repair or drop it.
	vals, ok = StaticPromptArgumentValues("EnumArg", []any{types.Color("NOPE")})
	if !ok || vals[0].Value.Canonical != "NOPE" {
		t.Errorf("an unknown member must project verbatim for the binder to reject, got ok=%v %+v", ok, vals)
	}
}

func TestClassProjectsSourceFieldOrderAndCanonicalNames(t *testing.T) {
	p := types.Palette{
		Primary: types.ColorGREEN,
		Shades:  []types.Color{types.ColorBLUE, types.ColorRED},
		Swatch:  types.Swatch{Color: types.ColorRED, Label: "hi"},
		Name:    "spring",
		Count:   3,
		Ratio:   0.5,
		Flag:    true,
	}
	vals, ok := StaticPromptArgumentValues("ClassArg", []any{p})
	if !ok {
		t.Fatal("a generated class value must project")
	}
	v := vals[0].Value
	if v.Kind != promptdescriptor.StaticClass || v.TypeName != "Palette" {
		t.Fatalf("class projected as %+v", v)
	}
	wantOrder := []string{"primary", "shades", "swatch", "name", "count", "ratio", "flag"}
	if len(v.Fields) != len(wantOrder) {
		t.Fatalf("want %d fields, got %d", len(wantOrder), len(v.Fields))
	}
	for i, canonical := range wantOrder {
		if v.Fields[i].Canonical != canonical {
			t.Fatalf("field %d canonical = %q, want %q (SOURCE order, canonical names)", i, v.Fields[i].Canonical, canonical)
		}
	}
	if v.Fields[0].Value.Canonical != "GREEN" {
		t.Errorf("primary = %+v, want GREEN", v.Fields[0].Value)
	}
	// List order is INPUT order, not sorted and not canonical order.
	shades := v.Fields[1].Value
	if shades.Kind != promptdescriptor.StaticList || len(shades.Items) != 2 ||
		shades.Items[0].Canonical != "BLUE" || shades.Items[1].Canonical != "RED" {
		t.Errorf("shades = %+v, want [BLUE RED] in input order", shades)
	}
	nested := v.Fields[2].Value
	if nested.Kind != promptdescriptor.StaticClass || nested.TypeName != "Swatch" ||
		len(nested.Fields) != 2 || nested.Fields[1].Canonical != "label" ||
		nested.Fields[1].Value.String != "hi" {
		t.Errorf("swatch = %+v", nested)
	}
	if v.Fields[3].Value.String != "spring" || v.Fields[4].Value.Int != 3 ||
		v.Fields[5].Value.Float != 0.5 || !v.Fields[6].Value.Bool {
		t.Errorf("scalar fields projected wrong: %+v", v.Fields[3:])
	}
}

func TestListProjectsInputOrderAndEmpties(t *testing.T) {
	vals, ok := StaticPromptArgumentValues("ListArg", []any{[]types.Color{types.ColorRED, types.ColorBLUE, types.ColorRED}})
	if !ok {
		t.Fatal("a generated list must project")
	}
	v := vals[0].Value
	if v.Kind != promptdescriptor.StaticList || len(v.Items) != 3 {
		t.Fatalf("list projected as %+v", v)
	}
	for i, want := range []string{"RED", "BLUE", "RED"} {
		if v.Items[i].Canonical != want {
			t.Errorf("item %d = %q, want %q", i, v.Items[i].Canonical, want)
		}
	}
	// A nil slice is an EMPTY list, not a decline and not a null.
	vals, ok = StaticPromptArgumentValues("ListArg", []any{[]types.Color(nil)})
	if !ok || vals[0].Value.Kind != promptdescriptor.StaticList || len(vals[0].Value.Items) != 0 {
		t.Errorf("a nil slice must project an empty list, got ok=%v %+v", ok, vals)
	}
	// A list of the WRONG element type declines outright.
	if _, ok := StaticPromptArgumentValues("ListArg", []any{[]string{"RED"}}); ok {
		t.Error("a []string where []types.Color is declared must decline")
	}
}

func TestNestedListOfClasses(t *testing.T) {
	p := types.Palette{Primary: types.ColorRED, Shades: []types.Color{}, Swatch: types.Swatch{Color: types.ColorBLUE, Label: "l"}, Name: "n"}
	vals, ok := StaticPromptArgumentValues("NestedList", []any{[]types.Palette{p, p}})
	if !ok {
		t.Fatal("a list of classes must project")
	}
	v := vals[0].Value
	if v.Kind != promptdescriptor.StaticList || len(v.Items) != 2 {
		t.Fatalf("nested list projected as %+v", v)
	}
	if v.Items[0].Kind != promptdescriptor.StaticClass || v.Items[0].TypeName != "Palette" {
		t.Errorf("item 0 = %+v", v.Items[0])
	}
}

func TestUnprojectableMethodHasNoProjector(t *testing.T) {
	if _, ok := StaticPromptArgumentValues("Unprojectable", []any{types.ColorRED}); ok {
		t.Fatal("a method whose audit failed must have NO projector")
	}
	if _, ok := StaticPromptArgumentProjectors["Unprojectable"]; ok {
		t.Fatal("Unprojectable must not be in the registry")
	}
	if StaticPromptProjectorDeclines["Unprojectable"] == "" {
		t.Fatal("Unprojectable must carry a stable decline reason")
	}
	if _, ok := StaticPromptArgumentValues("NeverDeclared", nil); ok {
		t.Fatal("an unknown method must decline")
	}
}
`
