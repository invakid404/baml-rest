package nativeschema

// De-BAML Slice 7.1b — proof for the V3 SOURCE-RESOLVED input value resolver
// (inputvalues.go), driven through the real BuildPromptDescriptors pipeline over
// inline .baml mini-projects. Two halves:
//
//   - what V3 DESCRIBES: project enums in deterministic source order with exact
//     resolved aliases, the transitive input-class closure in declaration order
//     with source field order/aliases, alias EXPANSION, list elements, and the
//     nullable flag;
//   - what V3 REFUSES: every shape that would need a guess. Each refusal is
//     asserted as a real descriptor ABSENCE plus a stable reason, because that is
//     what makes the function fall back to BAML instead of rendering a value the
//     source never stated.

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// ivClientProvider is the client->provider map every mini-project below resolves
// against (each declares `client<llm> C { provider openai ... }`).
var ivClientProvider = map[string]string{"C": "openai"}

// buildIV runs the production build pipeline over one inline .baml source and
// returns the descriptors and prompt declines.
func buildIV(t *testing.T, src string) (map[string]promptdescriptor.Function, map[string]string) {
	t.Helper()
	f, err := bamlparser.ParseBytes("iv.baml", []byte(src))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	files := []SourceFile{{File: f, Path: "iv.baml"}}
	schemas, schemaDeclines := BuildStaticSchemas(files)
	return BuildPromptDescriptors(files, schemas, schemaDeclines, ivClientProvider, BuildClientConfigs(files))
}

// mustIV builds and requires an eligible descriptor for F.
func mustIV(t *testing.T, src string) promptdescriptor.Function {
	t.Helper()
	descriptors, declines := buildIV(t, src)
	fn, ok := descriptors["F"]
	if !ok {
		t.Fatalf("expected an eligible descriptor for F, got decline %q", declines["F"])
	}
	if fn.Version != promptdescriptor.Version {
		t.Fatalf("descriptor version = %d, want %d", fn.Version, promptdescriptor.Version)
	}
	return fn
}

const ivClient = "client<llm> C { provider openai options { model \"m\" } }\n"

// TestProjectEnumsAreWholeAndSourceOrdered proves ProjectEnums carries EVERY
// declared enum — including one no argument reaches — in parsed source order,
// with each member's canonical name in declaration order and its exact resolved
// alias (nil when unaliased, present-and-empty for @alias("")).
func TestProjectEnumsAreWholeAndSourceOrdered(t *testing.T) {
	fn := mustIV(t, `enum Color {
  RED @alias("rouge")
  GREEN
  BLUE @alias("")
}
enum Unreached {
  ONLY
}
`+ivClient+`function F(topic: string) -> string { client C prompt #"About {{ topic }}"# }`)

	enums := fn.InputValues.ProjectEnums
	if len(enums) != 2 {
		t.Fatalf("want 2 project enums (including the unreached one), got %d", len(enums))
	}
	if enums[0].Name != "Color" || enums[1].Name != "Unreached" {
		t.Fatalf("project enums are not in source declaration order: %q, %q", enums[0].Name, enums[1].Name)
	}

	members := enums[0].Members
	if len(members) != 3 {
		t.Fatalf("want 3 Color members, got %d", len(members))
	}
	for i, want := range []string{"RED", "GREEN", "BLUE"} {
		if members[i].Canonical != want {
			t.Errorf("member %d = %q, want %q (source declaration order)", i, members[i].Canonical, want)
		}
	}
	if members[0].Alias == nil || *members[0].Alias != "rouge" {
		t.Errorf("RED alias = %v, want \"rouge\"", members[0].Alias)
	}
	if members[1].Alias != nil {
		t.Errorf("GREEN alias = %q, want nil (unaliased displays the canonical name)", *members[1].Alias)
	}
	if members[2].Alias == nil || *members[2].Alias != "" {
		t.Errorf(`BLUE alias = %v, want a PRESENT empty string (@alias("") differs from unaliased)`, members[2].Alias)
	}
}

// TestClassClosureIsTransitiveAndDeclarationOrdered proves the class closure
// carries exactly the classes an argument reaches, in project declaration order
// (not discovery order), each with its SOURCE field order, canonical names, and
// resolved aliases; and that an unreached class stays out.
func TestClassClosureIsTransitiveAndDeclarationOrdered(t *testing.T) {
	fn := mustIV(t, `enum Color { RED GREEN }
class Swatch {
  color Color
  label string @alias("etiquette")
}
class Palette {
  primary Color @alias("principale")
  shades Color[]
  swatch Swatch
  name string
}
class Unreached { x string }
`+ivClient+`function F(palette: Palette) -> string { client C prompt #"{{ palette }}"# }`)

	classes := fn.InputValues.Classes
	if len(classes) != 2 {
		t.Fatalf("want exactly the 2 reachable classes, got %d (%v)", len(classes), classNames(classes))
	}
	// Swatch is DECLARED first but DISCOVERED second (through Palette.swatch):
	// declaration order is what makes the emitted universe byte-stable.
	if classes[0].Name != "Swatch" || classes[1].Name != "Palette" {
		t.Fatalf("class closure order = %v, want [Swatch Palette] (source declaration order)", classNames(classes))
	}

	palette := classes[1]
	wantFields := []string{"primary", "shades", "swatch", "name"}
	if len(palette.Fields) != len(wantFields) {
		t.Fatalf("Palette has %d fields, want %d", len(palette.Fields), len(wantFields))
	}
	for i, want := range wantFields {
		if palette.Fields[i].Canonical != want {
			t.Errorf("Palette field %d = %q, want %q (source order)", i, palette.Fields[i].Canonical, want)
		}
	}
	if palette.Fields[0].Alias == nil || *palette.Fields[0].Alias != "principale" {
		t.Errorf("primary alias = %v, want \"principale\"", palette.Fields[0].Alias)
	}
	if palette.Fields[1].Type.Kind != promptdescriptor.ValueList ||
		palette.Fields[1].Type.Elem == nil ||
		palette.Fields[1].Type.Elem.Kind != promptdescriptor.ValueEnum ||
		palette.Fields[1].Type.Elem.EnumName != "Color" {
		t.Errorf("shades type = %+v, want list of enum Color", palette.Fields[1].Type)
	}
	if palette.Fields[2].Type.Kind != promptdescriptor.ValueClass || palette.Fields[2].Type.ClassName != "Swatch" {
		t.Errorf("swatch type = %+v, want class Swatch", palette.Fields[2].Type)
	}

	// The argument itself names the class by source name.
	if len(fn.Args) != 1 || fn.Args[0].ValueType == nil ||
		fn.Args[0].ValueType.Kind != promptdescriptor.ValueClass ||
		fn.Args[0].ValueType.ClassName != "Palette" {
		t.Errorf("argument value type = %+v, want class Palette", fn.Args[0].ValueType)
	}
}

func classNames(cs []promptdescriptor.ResolvedClass) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// TestArgumentValueTypesCoverTheClaimedGraph pins each admitted argument shape,
// including alias EXPANSION (an alias has no distinct Jinja host identity, so it
// resolves to its underlying value type rather than becoming a node) and the
// explicit nullable flag.
func TestArgumentValueTypesCoverTheClaimedGraph(t *testing.T) {
	fn := mustIV(t, `enum Color { RED }
class Item { name string }
type Alias = Color
type ListAlias = Color[]
`+ivClient+`function F(s: string, i: int, f: float, b: bool, c: Color, it: Item, cs: Color[], a: Alias, la: ListAlias, opt: string?, c2: Color?, it2: Item?, cs2: Color[]?) -> string {
  client C
  prompt #"{{ s }}"#
}`)

	if len(fn.Args) != 13 {
		t.Fatalf("want 13 arguments, got %d", len(fn.Args))
	}
	want := []struct {
		name string
		kind promptdescriptor.ValueKind
		enum string
		cls  string
		elem promptdescriptor.ValueKind
		null bool
	}{
		{name: "s", kind: promptdescriptor.ValueString},
		{name: "i", kind: promptdescriptor.ValueInt},
		{name: "f", kind: promptdescriptor.ValueFloat},
		{name: "b", kind: promptdescriptor.ValueBool},
		{name: "c", kind: promptdescriptor.ValueEnum, enum: "Color"},
		{name: "it", kind: promptdescriptor.ValueClass, cls: "Item"},
		{name: "cs", kind: promptdescriptor.ValueList, elem: promptdescriptor.ValueEnum},
		// `type Alias = Color` expands to the enum edge itself.
		{name: "a", kind: promptdescriptor.ValueEnum, enum: "Color"},
		{name: "la", kind: promptdescriptor.ValueList, elem: promptdescriptor.ValueEnum},
		{name: "opt", kind: promptdescriptor.ValueString, null: true},
		// NULLABLE non-scalar edges. resolveType sets Nullable only in the
		// KindUnion branch (the shape `T?` lowers to), so an enum/class/list is
		// where the flag is most likely to be dropped — and dropping it would make
		// a nullable edge look non-nullable to the binder, which is the one thing
		// that decides whether the value is bindable at all.
		{name: "c2", kind: promptdescriptor.ValueEnum, enum: "Color", null: true},
		{name: "it2", kind: promptdescriptor.ValueClass, cls: "Item", null: true},
		{name: "cs2", kind: promptdescriptor.ValueList, elem: promptdescriptor.ValueEnum, null: true},
	}
	for i, w := range want {
		got := fn.Args[i]
		if got.Name != w.name {
			t.Fatalf("argument %d name = %q, want %q", i, got.Name, w.name)
		}
		if got.ValueType == nil {
			t.Fatalf("argument %q has no ValueType; V3 requires one on every argument", w.name)
		}
		vt := got.ValueType
		if vt.Kind != w.kind {
			t.Errorf("argument %q kind = %q, want %q", w.name, vt.Kind, w.kind)
		}
		if vt.EnumName != w.enum {
			t.Errorf("argument %q enum = %q, want %q", w.name, vt.EnumName, w.enum)
		}
		if vt.ClassName != w.cls {
			t.Errorf("argument %q class = %q, want %q", w.name, vt.ClassName, w.cls)
		}
		if vt.Nullable != w.null {
			t.Errorf("argument %q nullable = %v, want %v", w.name, vt.Nullable, w.null)
		}
		if w.elem != "" {
			if vt.Elem == nil || vt.Elem.Kind != w.elem {
				t.Errorf("argument %q element = %+v, want kind %q", w.name, vt.Elem, w.elem)
			}
		}
		// The LEGACY source spelling is retained alongside, never consulted for
		// value semantics.
		if got.Type == nil {
			t.Errorf("argument %q lost its retained source TypeExpr", w.name)
		}
	}
}

// TestInputValueDeclines is the fail-closed half: every shape V3 cannot state
// exactly must produce NO descriptor and a stable reason.
func TestInputValueDeclines(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
		// v3 marks a row the V3 resolver is the FIRST gate for. A few rows
		// (an unresolved or ambiguous type name) are already caught upstream by
		// the pre-existing reachable-eligibility scan (contract (e)), so they
		// decline with that wording instead — still no descriptor, which is the
		// property under test.
		v3 bool
	}{
		{name: "map_argument", src: `function F(m: map<string, string>) -> string { client C prompt #"{{ m }}"# }`, want: "map types are not supported", v3: true},
		{name: "union_argument", src: `function F(u: int | string) -> string { client C prompt #"{{ u }}"# }`, want: "union types are not supported", v3: true},
		{name: "literal_argument", src: `function F(l: "a" | "b") -> string { client C prompt #"{{ l }}"# }`, want: "union types are not supported", v3: true},
		{name: "media_argument", src: `function F(img: image) -> string { client C prompt #"{{ img }}"# }`, want: "media types are not supported", v3: true},
		{name: "multidim_list", src: `function F(g: string[][]) -> string { client C prompt #"{{ g }}"# }`, want: "dimensions", v3: true},
		// ALIAS-HIDDEN nesting: `L[]` is a single-dimension SPELLING, so the Dims
		// check cannot see it — only the RESOLVED element can. It is the same
		// unproven shape `T[][]` declines for and must decline the same way.
		{
			name: "alias_hidden_nested_list",
			src: `type L = string[]
function F(x: L[]) -> string { client C prompt #"{{ x }}"# }`,
			want: "list element resolves to another list",
			v3:   true,
		},
		{
			name: "alias_hidden_nested_list_of_enum",
			src: `enum Color { RED }
type L = Color[]
function F(x: L[]) -> string { client C prompt #"{{ x }}"# }`,
			want: "list element resolves to another list",
			v3:   true,
		},
		// The same nesting reached through a CLASS FIELD rather than an argument.
		{
			name: "alias_hidden_nested_list_in_class_field",
			src: `type L = string[]
class Holder { rows L[] }
function F(h: Holder) -> string { client C prompt #"{{ h }}"# }`,
			want: "list element resolves to another list",
			v3:   true,
		},
		{name: "bare_argument", src: `function F(x) -> string { client C prompt #"{{ x }}"# }`, want: "bare/untyped", v3: true},
		{name: "unresolved_name", src: `function F(x: Nope) -> string { client C prompt #"{{ x }}"# }`, want: `unresolved type reference "Nope"`},
		{
			name: "recursive_input_class",
			src:  "class N { value string next N? }\nfunction F(n: N) -> string { client C prompt #\"{{ n }}\"# }",
			want: "recursive input class graph",
			v3:   true,
		},
		{
			name: "ambiguous_type_name",
			src:  "class Dup { a string }\nenum Dup { A }\nfunction F(d: Dup) -> string { client C prompt #\"{{ d }}\"# }",
			want: "declared more than once",
		},
		{
			name: "class_field_constraint_attribute",
			src:  "class WithCheck { n int @check(pos, {{ this > 0 }}) }\nfunction F(w: WithCheck) -> string { client C prompt #\"{{ w }}\"# }",
			want: "unsupported attribute @check",
			v3:   true,
		},
		{
			name: "class_block_attribute",
			src:  "class Aliased { n int\n@@alias(\"other\")\n}\nfunction F(a: Aliased) -> string { client C prompt #\"{{ a }}\"# }",
			want: "class-level block attributes are not proven",
			v3:   true,
		},
		{
			name: "enum_dynamic_poisons_the_project",
			src:  "enum D { X\n@@dynamic\n}\nfunction F(topic: string) -> string { client C prompt #\"{{ topic }}\"# }",
			want: "is @@dynamic",
			v3:   true,
		},
		{
			name: "enum_nonliteral_alias_poisons_the_project",
			src:  "enum Color { RED @alias(x) }\nfunction F(topic: string) -> string { client C prompt #\"{{ topic }}\"# }",
			want: "@alias is not a single plain string literal",
			v3:   true,
		},
		{
			name: "enum_duplicate_member",
			src:  "enum Color {\n  RED\n  RED\n}\nfunction F(topic: string) -> string { client C prompt #\"{{ topic }}\"# }",
			want: "duplicate member",
			v3:   true,
		},
		{
			name: "enum_block_attribute",
			src:  "enum Color { RED\n@@alias(\"couleur\")\n}\nfunction F(topic: string) -> string { client C prompt #\"{{ topic }}\"# }",
			want: "enum-level block attributes are not proven",
			v3:   true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			descriptors, declines := buildIV(t, ivClient+c.src)
			if _, ok := descriptors["F"]; ok {
				t.Fatalf("expected NO descriptor for F (a V3 shape it cannot state exactly)")
			}
			reason, ok := declines["F"]
			if !ok {
				t.Fatalf("expected a decline reason for F, got none")
			}
			if !strings.Contains(reason, c.want) {
				t.Errorf("decline reason %q does not contain %q", reason, c.want)
			}
			if c.v3 && !strings.Contains(reason, "input value graph cannot be resolved faithfully") {
				t.Errorf("decline reason %q is missing the stable V3 prefix", reason)
			}
		})
	}
}

// TestUnresolvableProjectEnumPoisonsEveryFunction pins the GLOBAL shape of the
// enum decline. BAML installs the enum namespace globals as a COMPLETE set, so a
// partial universe would describe a render context BAML never has — every
// function in the project must decline, including ones that never mention an
// enum.
func TestUnresolvableProjectEnumPoisonsEveryFunction(t *testing.T) {
	descriptors, declines := buildIV(t, `enum Broken { X
@@dynamic
}
enum Fine { A }
`+ivClient+`function F(topic: string) -> string { client C prompt #"{{ topic }}"# }
function G(topic: string) -> string { client C prompt #"{{ topic }}"# }`)

	if len(descriptors) != 0 {
		t.Fatalf("expected zero descriptors while a project enum is unresolvable, got %d", len(descriptors))
	}
	for _, name := range []string{"F", "G"} {
		if !strings.Contains(declines[name], `project enum "Broken"`) {
			t.Errorf("%s decline %q must name the unresolvable project enum", name, declines[name])
		}
	}
}

// TestMacroArgumentsCarryNoValueType pins the deliberate asymmetry: a
// template_string poisons every function at the macro gate, so a macro argument
// is never bound and never needs a V3 value type. (The macro set is only
// reachable through a descriptor, so this drives the builder directly.)
func TestMacroArgumentsCarryNoValueType(t *testing.T) {
	descriptors, declines := buildIV(t, "template_string Greet(n: string) #\"hi {{ n }}\"#\n"+ivClient+
		`function F(topic: string) -> string { client C prompt #"About {{ topic }}"# }`)
	// The macro itself is well-formed, so the descriptor still builds; the static
	// gate is what declines a macro-carrying project.
	fn, ok := descriptors["F"]
	if !ok {
		t.Fatalf("expected a descriptor for F, got decline %q", declines["F"])
	}
	if len(fn.Macros) != 1 || len(fn.Macros[0].Args) != 1 {
		t.Fatalf("expected one macro with one argument, got %+v", fn.Macros)
	}
	if fn.Macros[0].Args[0].ValueType != nil {
		t.Error("a macro argument must carry NO V3 ValueType (macros are never bound)")
	}
	if fn.Args[0].ValueType == nil {
		t.Error("a FUNCTION argument must carry a V3 ValueType")
	}
}
