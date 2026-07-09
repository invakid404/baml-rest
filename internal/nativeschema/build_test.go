package nativeschema

import (
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// typeDefs mirrors the supported shapes from
// integration/testdata/baml_src/types.baml (SimpleOutput / Person /
// PersonWithAddress / CategorizedItem / ComprehensiveOutput and friends) plus
// a few extra containers/literals exercised by the builder.
const typeDefs = `
class SimpleOutput {
    message string
}

class Address {
    street string
    city string
    zip string
}

class Person {
    name string
    age int
    email string?
    tags string[]
}

class PersonWithAddress {
    name string
    address Address
}

enum Category {
    TECH
    BUSINESS
    OTHER
}

class CategorizedItem {
    name string
    category Category
}

enum Priority {
    LOW
    MEDIUM
    HIGH
    CRITICAL
}

class Tag {
    name string
    value string?
}

class Metadata {
    created_by string
    priority Priority
    tags Tag[]
}

class ComprehensiveOutput {
    id int
    title string
    description string?
    score float
    is_active bool
    metadata Metadata
    related_ids int[]
    labels string[]?
}

class SuccessResult {
    data string
}

class ErrorResult {
    error string
    code int
}

class Shapes {
    kind "circle" | "square"
    count int
    ratios map<string, float>
    matrix int[][]
    maybe_person Person?
    categories map<string, Category>
}
`

// fn renders a minimal function declaration returning ret.
func fn(name, ret string) string {
	return fmt.Sprintf("function %s(x: string) -> %s {\n    client C\n    prompt #\"p\"#\n}\n", name, ret)
}

// buildFromSource parses src as a single .baml file and runs the native
// static-schema builder over it (carrying the source so `///` doc-comment
// detection has the bytes it needs).
func buildFromSource(t *testing.T, src string) (map[string]sd.Bundle, map[string]string) {
	t.Helper()
	file, err := bamlparser.ParseString("build_test.baml", src)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return BuildStaticSchemas([]SourceFile{{File: file, Source: []byte(src)}})
}

// TestBuildStaticSchemasSupported proves the supported corpus builds a
// descriptor and re-lowers + validates cleanly (schema.FromStaticDescriptor +
// ValidateOutput), with no decline recorded.
func TestBuildStaticSchemasSupported(t *testing.T) {
	src := typeDefs +
		fn("GetGreeting", "string") +
		fn("GetSimple", "SimpleOutput") +
		fn("GetPerson", "Person") +
		fn("GetPersonWithAddress", "PersonWithAddress") +
		fn("GetPeople", "Person[]") +
		fn("GetCategory", "CategorizedItem") +
		fn("GetComprehensive", "ComprehensiveOutput") +
		fn("GetResult", "SuccessResult | ErrorResult") +
		fn("GetOptionalPerson", "Person?") +
		fn("GetMap", "map<string, int>") +
		fn("GetEnumValueMap", "map<string, Category>") +
		fn("GetLiteralKeyMap", `map<"a" | "b", int>`) +
		fn("GetMatrix", "int[][]") +
		fn("GetStringLiteralUnion", `"active" | "inactive"`) +
		fn("GetNullableUnion", "int | string | null") +
		fn("GetShapes", "Shapes")

	bundles, declines := buildFromSource(t, src)

	supported := []string{
		"GetGreeting", "GetSimple", "GetPerson", "GetPersonWithAddress",
		"GetPeople", "GetCategory", "GetComprehensive", "GetResult",
		"GetOptionalPerson", "GetMap", "GetEnumValueMap", "GetLiteralKeyMap",
		"GetMatrix", "GetStringLiteralUnion", "GetNullableUnion", "GetShapes",
	}

	for _, name := range supported {
		t.Run(name, func(t *testing.T) {
			if reason, declined := declines[name]; declined {
				t.Fatalf("function %q was declined, want supported: %s", name, reason)
			}
			bundle, ok := bundles[name]
			if !ok {
				t.Fatalf("function %q has no descriptor", name)
			}
			if bundle.Version != sd.Version {
				t.Errorf("bundle version = %d, want %d", bundle.Version, sd.Version)
			}
			if bundle.Method != name {
				t.Errorf("bundle method = %q, want %q", bundle.Method, name)
			}
			// The builder already lowered + validated; re-run to prove the
			// stored descriptor is self-contained and output-legal on its own.
			internal, err := schema.FromStaticDescriptor(bundle)
			if err != nil {
				t.Fatalf("FromStaticDescriptor(%q): %v", name, err)
			}
			if err := internal.ValidateOutput(); err != nil {
				t.Fatalf("ValidateOutput(%q): %v", name, err)
			}
		})
	}
}

// TestBuildStaticSchemasTargets asserts a few structural properties of the
// built descriptors so a regression in lowering shape is caught.
func TestBuildStaticSchemasTargets(t *testing.T) {
	src := typeDefs +
		fn("GetPerson", "Person") +
		fn("GetPeople", "Person[]") +
		fn("GetCategory", "CategorizedItem") +
		fn("GetOptionalPerson", "Person?") +
		fn("GetNullableUnion", "int | string | null") +
		fn("GetMatrix", "int[][]")

	bundles, declines := buildFromSource(t, src)
	if len(declines) != 0 {
		t.Fatalf("unexpected declines: %v", declines)
	}

	t.Run("class target reaches its definitions", func(t *testing.T) {
		b := bundles["GetPerson"]
		if b.Target.Kind != sd.TypeClass || b.Target.Name != "Person" {
			t.Fatalf("target = %+v, want class Person", b.Target)
		}
		if !hasClass(b, "Person") {
			t.Fatalf("Person class not collected: %+v", b.Classes)
		}
	})

	t.Run("list target wraps element class", func(t *testing.T) {
		b := bundles["GetPeople"]
		if b.Target.Kind != sd.TypeList {
			t.Fatalf("target kind = %q, want list", b.Target.Kind)
		}
		if b.Target.Elem == nil || b.Target.Elem.Kind != sd.TypeClass || b.Target.Elem.Name != "Person" {
			t.Fatalf("list elem = %+v, want class Person", b.Target.Elem)
		}
	})

	t.Run("enum reference collects enum def", func(t *testing.T) {
		b := bundles["GetCategory"]
		if !hasEnum(b, "Category") {
			t.Fatalf("Category enum not collected: %+v", b.Enums)
		}
	})

	t.Run("optional class lowers to nullable union", func(t *testing.T) {
		b := bundles["GetOptionalPerson"]
		if b.Target.Kind != sd.TypeUnion || b.Target.Union == nil {
			t.Fatalf("target = %+v, want union", b.Target)
		}
		if !b.Target.Union.Nullable {
			t.Fatalf("optional target union is not nullable: %+v", b.Target.Union)
		}
		if len(b.Target.Union.Variants) != 1 || b.Target.Union.Variants[0].Name != "Person" {
			t.Fatalf("union variants = %+v, want [Person]", b.Target.Union.Variants)
		}
	})

	t.Run("explicit null is hoisted into Nullable, never a variant", func(t *testing.T) {
		b := bundles["GetNullableUnion"]
		if b.Target.Kind != sd.TypeUnion || b.Target.Union == nil {
			t.Fatalf("target = %+v, want union", b.Target)
		}
		if !b.Target.Union.Nullable {
			t.Fatalf("union is not nullable despite explicit null: %+v", b.Target.Union)
		}
		for i, v := range b.Target.Union.Variants {
			if v.Kind == sd.TypePrimitive && v.Primitive == sd.PrimitiveNull {
				t.Fatalf("variant %d is a null primitive; null must live only in Nullable", i)
			}
		}
		if len(b.Target.Union.Variants) != 2 {
			t.Fatalf("want 2 non-null variants (int|string), got %d", len(b.Target.Union.Variants))
		}
	})

	t.Run("multi-dim array nests lists", func(t *testing.T) {
		b := bundles["GetMatrix"]
		if b.Target.Kind != sd.TypeList || b.Target.Elem == nil {
			t.Fatalf("target = %+v, want list", b.Target)
		}
		if b.Target.Elem.Kind != sd.TypeList || b.Target.Elem.Elem == nil {
			t.Fatalf("inner = %+v, want nested list", b.Target.Elem)
		}
		if b.Target.Elem.Elem.Kind != sd.TypePrimitive || b.Target.Elem.Elem.Primitive != sd.PrimitiveInt {
			t.Fatalf("innermost = %+v, want int primitive", b.Target.Elem.Elem)
		}
	})
}

// TestBuildStaticSchemasReachabilityOrder proves the builder emits enum/class
// slices in BAML output-format reachability order (reverse-of-reference DFS from
// the target), NOT declaration order. The fixture declares enums First,Second
// and classes Leaf,Envelope, but Envelope's fields reference First then Second
// then Leaf, so LIFO pops the last field first: enums render [Second, First]
// and classes render [Envelope (target, first popped), Leaf]. This mirrors the
// dynamic path's two_hoisted_enums_nested_class parity fixture.
func TestBuildStaticSchemasReachabilityOrder(t *testing.T) {
	src := `
enum First {
    F1
    F2
}
enum Second {
    S1
    S2
}
class Leaf {
    x string
}
class Envelope {
    a First
    b Second
    leaf Leaf
}
` + fn("GetEnvelope", "Envelope")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetEnvelope"]; declined {
		t.Fatalf("GetEnvelope declined: %s", reason)
	}
	b := bundles["GetEnvelope"]

	gotEnums := make([]string, 0, len(b.Enums))
	for _, e := range b.Enums {
		gotEnums = append(gotEnums, e.Name.Name)
	}
	if want := []string{"Second", "First"}; !equalStrings(gotEnums, want) {
		t.Errorf("enum order = %v, want %v (reachability, not declaration)", gotEnums, want)
	}

	gotClasses := make([]string, 0, len(b.Classes))
	for _, c := range b.Classes {
		gotClasses = append(gotClasses, c.Name.Name)
	}
	if want := []string{"Envelope", "Leaf"}; !equalStrings(gotClasses, want) {
		t.Errorf("class order = %v, want %v (target first, then reverse-of-reference)", gotClasses, want)
	}

	// The reordered descriptor must still lower + validate.
	internal, err := schema.FromStaticDescriptor(b)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	if err := internal.ValidateOutput(); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
}

// TestBuildStaticSchemasUnreachablePruned proves definitions the target cannot
// reach are absent from the descriptor (BAML's relevant_data_models returns
// only reachable models). Only referenced defs are collected in the first
// place, and reachability ordering re-affirms the pruning.
func TestBuildStaticSchemasUnreachablePruned(t *testing.T) {
	src := typeDefs + fn("GetSimple", "SimpleOutput")
	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetSimple"]; declined {
		t.Fatalf("GetSimple declined: %s", reason)
	}
	b := bundles["GetSimple"]
	if len(b.Enums) != 0 {
		t.Errorf("SimpleOutput reaches no enum, got %+v", b.Enums)
	}
	if len(b.Classes) != 1 || b.Classes[0].Name.Name != "SimpleOutput" {
		t.Errorf("SimpleOutput should reach only itself, got %+v", b.Classes)
	}
}

// TestBuildStaticSchemasAliasDescription proves @alias/@description lower onto
// the descriptor for class fields, enum values, and class/enum-level block
// attributes, and that the resulting descriptor lowers + validates.
func TestBuildStaticSchemasAliasDescription(t *testing.T) {
	src := `
enum Category {
    BUG @alias("bug") @description("a defect")
    FEATURE
    @@alias("Kind")
}
class Item {
    name string @alias("full_name") @description("the item name")
    category Category
    @@alias("Thing")
    @@description("an item")
}
` + fn("GetItem", "Item")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetItem"]; declined {
		t.Fatalf("GetItem declined, want supported: %s", reason)
	}
	b := bundles["GetItem"]

	cls := findClass(b, "Item")
	if cls == nil {
		t.Fatalf("Item class not found: %+v", b.Classes)
	}
	assertAlias(t, "class Item", cls.Name, "Thing")
	assertDesc(t, "class Item", cls.Description, "an item")

	if len(cls.Fields) == 0 || cls.Fields[0].Name.Name != "name" {
		t.Fatalf("Item.name field missing: %+v", cls.Fields)
	}
	assertAlias(t, "field name", cls.Fields[0].Name, "full_name")
	assertDesc(t, "field name", cls.Fields[0].Description, "the item name")
	// The `category` field carries no metadata; its alias/description stay nil.
	if len(cls.Fields) < 2 {
		t.Fatalf("Item.category field missing: %+v", cls.Fields)
	}
	if cls.Fields[1].Name.Alias != nil {
		t.Errorf("field category alias = %v, want nil", *cls.Fields[1].Name.Alias)
	}
	if cls.Fields[1].Description != nil {
		t.Errorf("field category description = %v, want nil", *cls.Fields[1].Description)
	}

	enm := findEnum(b, "Category")
	if enm == nil {
		t.Fatalf("Category enum not found: %+v", b.Enums)
	}
	assertAlias(t, "enum Category", enm.Name, "Kind")
	if len(enm.Values) != 2 {
		t.Fatalf("Category values = %+v, want 2", enm.Values)
	}
	assertAlias(t, "value BUG", enm.Values[0].Name, "bug")
	assertDesc(t, "value BUG", enm.Values[0].Description, "a defect")
	if enm.Values[1].Name.Alias != nil || enm.Values[1].Description != nil {
		t.Errorf("value FEATURE should carry no alias/description: %+v", enm.Values[1])
	}

	internal, err := schema.FromStaticDescriptor(b)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	if err := internal.ValidateOutput(); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
}

// TestBuildStaticSchemasUnionNormalization proves that union members which
// lower into their own union (a non-recursive alias or a parenthesized group)
// or into a bare null (an alias to null) are flattened/hoisted so the
// descriptor is BAML-equivalent: nested unions are flattened up, and ALL
// nullability sources collapse into a single Nullable flag with no
// null-primitive variant. Each case must also round-trip FromStaticDescriptor +
// ValidateOutput.
func TestBuildStaticSchemasUnionNormalization(t *testing.T) {
	// intStringNullable asserts b.Target is TypeUnion{Variants:[int,string],
	// Nullable:true} with no null-primitive variant.
	intStringNullable := func(t *testing.T, b sd.Bundle) {
		t.Helper()
		if b.Target.Kind != sd.TypeUnion || b.Target.Union == nil {
			t.Fatalf("target = %+v, want union", b.Target)
		}
		u := b.Target.Union
		if !u.Nullable {
			t.Fatalf("union is not nullable: %+v", u)
		}
		if len(u.Variants) != 2 {
			t.Fatalf("want 2 non-null variants, got %d: %+v", len(u.Variants), u.Variants)
		}
		for i, v := range u.Variants {
			if v.Kind == sd.TypePrimitive && v.Primitive == sd.PrimitiveNull {
				t.Fatalf("variant %d is a null primitive; null must live only in Nullable", i)
			}
			if v.Kind == sd.TypeUnion {
				t.Fatalf("variant %d is a nested union; unions must be flattened: %+v", i, v)
			}
		}
		if u.Variants[0].Primitive != sd.PrimitiveInt || u.Variants[1].Primitive != sd.PrimitiveString {
			t.Fatalf("variants = %+v, want [int, string]", u.Variants)
		}
	}

	cases := []struct {
		name   string
		fnName string
		src    string
		check  func(*testing.T, sd.Bundle)
	}{
		{
			name:   "optional alias-to-union flattens (a)",
			fnName: "GetChoiceOpt",
			src:    "type Choice = int | string\n" + fn("GetChoiceOpt", "Choice?"),
			check:  intStringNullable,
		},
		{
			name:   "optional parenthesized union flattens (b)",
			fnName: "GetGroupOpt",
			src:    fn("GetGroupOpt", "(int | string)?"),
			check:  intStringNullable,
		},
		{
			name:   "explicit alias-to-null folds into Nullable (c)",
			fnName: "GetStrOrNull",
			src:    "type NullAlias = null\n" + fn("GetStrOrNull", "string | NullAlias"),
			check: func(t *testing.T, b sd.Bundle) {
				t.Helper()
				if b.Target.Kind != sd.TypeUnion || b.Target.Union == nil {
					t.Fatalf("target = %+v, want union", b.Target)
				}
				u := b.Target.Union
				if !u.Nullable {
					t.Fatalf("union is not nullable: %+v", u)
				}
				if len(u.Variants) != 1 {
					t.Fatalf("want 1 non-null variant, got %d: %+v", len(u.Variants), u.Variants)
				}
				if u.Variants[0].Kind != sd.TypePrimitive || u.Variants[0].Primitive != sd.PrimitiveString {
					t.Fatalf("variant = %+v, want string primitive", u.Variants[0])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundles, declines := buildFromSource(t, tc.src)
			if reason, declined := declines[tc.fnName]; declined {
				t.Fatalf("function %q was declined, want supported: %s", tc.fnName, reason)
			}
			b, ok := bundles[tc.fnName]
			if !ok {
				t.Fatalf("function %q has no descriptor", tc.fnName)
			}
			tc.check(t, b)
			// Normalized shape must lower + validate cleanly.
			internal, err := schema.FromStaticDescriptor(b)
			if err != nil {
				t.Fatalf("FromStaticDescriptor(%q): %v", tc.fnName, err)
			}
			if err := internal.ValidateOutput(); err != nil {
				t.Fatalf("ValidateOutput(%q): %v", tc.fnName, err)
			}
		})
	}
}

// TestBuildStaticSchemasDeclines proves every unsupported shape is declined
// with a non-empty, informative reason and emits NO descriptor.
func TestBuildStaticSchemasDeclines(t *testing.T) {
	cases := []struct {
		name          string
		fnName        string
		src           string
		wantReasonSub string
	}{
		{
			name:   "recursive class",
			fnName: "GetNode",
			src: `class Node {
    value string
    next Node?
}
` + fn("GetNode", "Node"),
			wantReasonSub: "recursive class",
		},
		{
			name:          "recursive alias (structural)",
			fnName:        "GetLoop",
			src:           "type Loop = Loop[]\n" + fn("GetLoop", "Loop"),
			wantReasonSub: "recursive type alias",
		},
		{
			name:          "direct alias cycle",
			fnName:        "GetSelf",
			src:           "type Self = Self\n" + fn("GetSelf", "Self"),
			wantReasonSub: "recursive type alias",
		},
		{
			name:   "field constraint attribute",
			fnName: "GetChecked",
			src: `class Checked {
    name string @check(non_empty, {{ this|length > 0 }})
}
` + fn("GetChecked", "Checked"),
			wantReasonSub: "attribute",
		},
		{
			name:   "field stream attribute",
			fnName: "GetStreamed",
			src: `class Streamed {
    value string @stream.done
}
` + fn("GetStreamed", "Streamed"),
			wantReasonSub: "attribute",
		},
		{
			name:   "class block attribute @@dynamic",
			fnName: "GetDynamic",
			src: `class DynamicOutput {
    base_field string
    @@dynamic
}
` + fn("GetDynamic", "DynamicOutput"),
			wantReasonSub: "block attribute",
		},
		{
			name:   "enum block attribute @@dynamic",
			fnName: "GetDynEnum",
			src: `enum DynamicCategory {
    DEFAULT
    @@dynamic
}
class DynEnumOut {
    category DynamicCategory
}
` + fn("GetDynEnum", "DynEnumOut"),
			wantReasonSub: "block attribute",
		},
		{
			name:   "enum value @skip",
			fnName: "GetSkip",
			src: `enum SkipEnum {
    KEEP
    HIDE @skip
}
class SkipOut {
    v SkipEnum
}
` + fn("GetSkip", "SkipOut"),
			wantReasonSub: "attribute",
		},
		{
			name:   "alias with non-string argument",
			fnName: "GetBadAlias",
			src: `class BadAlias {
    name string @alias(not_a_string)
}
` + fn("GetBadAlias", "BadAlias"),
			wantReasonSub: "must be a plain string",
		},
		{
			name:   "alias with no argument",
			fnName: "GetEmptyAlias",
			src: `class EmptyAlias {
    name string @alias
}
` + fn("GetEmptyAlias", "EmptyAlias"),
			wantReasonSub: "single string argument",
		},
		{
			name:   "alias rendered-name collision",
			fnName: "GetCollide",
			src: `class Collide {
    a string @alias("b")
    b string
}
` + fn("GetCollide", "Collide"),
			wantReasonSub: "duplicate",
		},
		{
			name:   "class-level /// doc comment",
			fnName: "GetDocClass",
			src: `/// A documented class.
class DocClass {
    value string
}
` + fn("GetDocClass", "DocClass"),
			wantReasonSub: "doc comment",
		},
		{
			name:   "field-level /// doc comment",
			fnName: "GetDocField",
			src: `class DocField {
    /// the value field
    value string
}
` + fn("GetDocField", "DocField"),
			wantReasonSub: "doc comment",
		},
		{
			name:   "enum-level /// doc comment",
			fnName: "GetDocEnum",
			src: `/// A documented enum.
enum DocEnum {
    A
    B
}
class DocEnumOut {
    e DocEnum
}
` + fn("GetDocEnum", "DocEnumOut"),
			wantReasonSub: "doc comment",
		},
		{
			name:   "enum-value /// doc comment",
			fnName: "GetDocValue",
			src: `enum DocValueEnum {
    /// the first value
    A
    B
}
class DocValueOut {
    e DocValueEnum
}
` + fn("GetDocValue", "DocValueOut"),
			wantReasonSub: "doc comment",
		},
		{
			name:          "tuple output",
			fnName:        "GetTuple",
			src:           fn("GetTuple", "(int, string)"),
			wantReasonSub: "tuple",
		},
		{
			name:          "media output (bare)",
			fnName:        "GetImage",
			src:           fn("GetImage", "image"),
			wantReasonSub: "media",
		},
		{
			name:   "media output (class field)",
			fnName: "GetImageClass",
			src: `class ImageWithCaption {
    img image
    caption string
}
` + fn("GetImageClass", "ImageWithCaption"),
			wantReasonSub: "media",
		},
		{
			name:   "float literal type",
			fnName: "GetFloatLit",
			src: `class HasFloatLit {
    ratio 1.5
}
` + fn("GetFloatLit", "HasFloatLit"),
			wantReasonSub: "float literal",
		},
		{
			name:          "invalid map key",
			fnName:        "GetBadMap",
			src:           fn("GetBadMap", "map<int, string>"),
			wantReasonSub: "map key",
		},
		{
			name:          "namespaced identifier",
			fnName:        "GetNs",
			src:           fn("GetNs", "foo::Bar"),
			wantReasonSub: "namespaced",
		},
		{
			name:          "path identifier",
			fnName:        "GetPath",
			src:           fn("GetPath", "foo.Bar"),
			wantReasonSub: "path",
		},
		{
			name:          "unresolved reference",
			fnName:        "GetUnknown",
			src:           fn("GetUnknown", "NoSuchType"),
			wantReasonSub: "unresolved type reference",
		},
		{
			name:   "duplicate class name",
			fnName: "GetDup",
			src: `class Dup {
    a string
}
class Dup {
    b string
}
` + fn("GetDup", "Dup"),
			wantReasonSub: "declared more than once",
		},
		{
			name:   "duplicate enum name",
			fnName: "GetDupEnum",
			src: `enum DupE {
    A
}
enum DupE {
    B
}
` + fn("GetDupEnum", "DupE"),
			wantReasonSub: "declared more than once",
		},
		{
			name:          "duplicate alias name",
			fnName:        "GetDupAlias",
			src:           "type DupA = int\ntype DupA = string\n" + fn("GetDupAlias", "DupA"),
			wantReasonSub: "declared more than once",
		},
		{
			name:   "cross-kind duplicate name (class vs enum)",
			fnName: "GetCross",
			src: `class Cross {
    a string
}
enum Cross {
    A
}
` + fn("GetCross", "Cross"),
			wantReasonSub: "declared more than once",
		},
		{
			name:   "duplicate class field",
			fnName: "GetDupField",
			src: `class DupField {
    a string
    a int
}
` + fn("GetDupField", "DupField"),
			wantReasonSub: "duplicate field name",
		},
		{
			name:   "duplicate enum value",
			fnName: "GetDupValue",
			src: `enum DupValue {
    A
    A
}
class DupValueOut {
    v DupValue
}
` + fn("GetDupValue", "DupValueOut"),
			wantReasonSub: "duplicate value name",
		},
		{
			name:          "generic beyond map<K,V>",
			fnName:        "GetGeneric",
			src:           fn("GetGeneric", "Foo<int>"),
			wantReasonSub: "generic",
		},
		{
			name:   "type alias RHS attribute",
			fnName: "GetAliasAttr",
			// A trailing attribute on the alias RHS folds onto the RHS type in
			// slice 1 (reassociation is deferred), so it is declined via
			// lowerType's D1 guard; resolveAlias also guards alias.Attributes for
			// the reassociated form. Either way: fail-closed with "attribute".
			src:           "type AliasAttr = int @check(x, {{ this > 0 }})\n" + fn("GetAliasAttr", "AliasAttr"),
			wantReasonSub: "attribute",
		},
		{
			name:   "class method / expr_fn",
			fnName: "GetMethodClass",
			src: `class WithMethod {
    x string
    function double() -> int {
        prompt #"p"#
    }
}
` + fn("GetMethodClass", "WithMethod"),
			wantReasonSub: "unsupported body content",
		},
		{
			name:   "nested unsupported block (type_builder-like)",
			fnName: "GetNestedBlock",
			src: `class WithNested {
    value string
    extra {
        inner string
    }
}
` + fn("GetNestedBlock", "WithNested"),
			wantReasonSub: "unsupported body content",
		},
		{
			name:   "class field @skip",
			fnName: "GetFieldSkip",
			src: `class FieldSkip {
    a string @skip
}
` + fn("GetFieldSkip", "FieldSkip"),
			wantReasonSub: "attribute",
		},
		// NOTE: descriptor kinds TypeTop and TypeArrow have NO corresponding
		// bamlparser TypeExpr node (the AST union is Unsupported/Primitive/Media/
		// NameRef/List/Map/Union/Literal/Tuple/Group only), so AST->descriptor
		// lowering can never emit an arrow/top and there is no source shape for a
		// builder to decline. They remain covered by internal/schema.ValidateOutput
		// tests, not here.
		{
			name:          "function without return type",
			fnName:        "NoReturn",
			src:           "function NoReturn(x: string) {\n    client C\n    prompt #\"p\"#\n}\n",
			wantReasonSub: "no parsed return type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundles, declines := buildFromSource(t, tc.src)

			if _, ok := bundles[tc.fnName]; ok {
				t.Fatalf("function %q built a descriptor, want decline", tc.fnName)
			}
			reason, declined := declines[tc.fnName]
			if !declined {
				t.Fatalf("function %q was not declined", tc.fnName)
			}
			if reason == "" {
				t.Fatalf("function %q declined with an empty reason", tc.fnName)
			}
			if tc.wantReasonSub != "" && !strings.Contains(reason, tc.wantReasonSub) {
				t.Fatalf("function %q decline reason %q does not contain %q", tc.fnName, reason, tc.wantReasonSub)
			}
		})
	}
}

// TestBuildStaticSchemasPerFunctionIsolation proves a single unsupported
// function does not poison sibling functions in the same file: supported
// functions still build even when the file also declares recursion, media,
// and @@dynamic outputs.
func TestBuildStaticSchemasPerFunctionIsolation(t *testing.T) {
	src := typeDefs + `
class TreeNode {
    value string
    children TreeNodeList?
}
type TreeNodeList = TreeNode[]

class ImageWithCaption {
    img image
    caption string
}

class DynamicOutput {
    base_field string
    @@dynamic
}
` +
		fn("GetSimple", "SimpleOutput") + // supported
		fn("GetPerson", "Person") + // supported
		fn("ParseTree", "TreeNode") + // declined: recursion
		fn("DescribeCaption", "ImageWithCaption") + // declined: media
		fn("GetDynamic", "DynamicOutput") // declined: @@dynamic

	bundles, declines := buildFromSource(t, src)

	for _, ok := range []string{"GetSimple", "GetPerson"} {
		if _, built := bundles[ok]; !built {
			t.Errorf("supported function %q was not built (reason: %q)", ok, declines[ok])
		}
	}
	for _, bad := range []string{"ParseTree", "DescribeCaption", "GetDynamic"} {
		if _, built := bundles[bad]; built {
			t.Errorf("unsupported function %q was built, want decline", bad)
		}
		if _, declined := declines[bad]; !declined {
			t.Errorf("unsupported function %q was not declined", bad)
		}
	}
}

// TestBuildStaticSchemasRegularCommentNotDeclined proves an ordinary `//`
// comment (two slashes) is NOT mistaken for a `///` doc comment: it does not
// decline, and the function builds normally. This pins that the doc-comment
// decline is specific to the three-slash BAML doc-comment form.
func TestBuildStaticSchemasRegularCommentNotDeclined(t *testing.T) {
	src := `// a regular comment, not a doc comment
class Commented {
    // another regular comment
    value string
}
enum CommentedEnum {
    // regular comment before a value
    A
    B
}
class CommentedOut {
    v string
    e CommentedEnum
}
` + fn("GetCommented", "CommentedOut")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetCommented"]; declined {
		t.Fatalf("regular // comments must NOT decline: %s", reason)
	}
	if _, ok := bundles["GetCommented"]; !ok {
		t.Fatalf("GetCommented should build with only regular // comments")
	}
}

// TestBuildStaticSchemasEmpty proves the builder tolerates no-input safely.
func TestBuildStaticSchemasEmpty(t *testing.T) {
	bundles, declines := BuildStaticSchemas([]SourceFile(nil))
	if bundles == nil || declines == nil {
		t.Fatalf("maps must be non-nil, got bundles=%v declines=%v", bundles, declines)
	}
	if len(bundles) != 0 || len(declines) != 0 {
		t.Fatalf("want empty maps, got bundles=%v declines=%v", bundles, declines)
	}
}

func hasClass(b sd.Bundle, name string) bool {
	return findClass(b, name) != nil
}

func hasEnum(b sd.Bundle, name string) bool {
	return findEnum(b, name) != nil
}

func findClass(b sd.Bundle, name string) *sd.ClassDef {
	for i := range b.Classes {
		if b.Classes[i].Name.Name == name {
			return &b.Classes[i]
		}
	}
	return nil
}

func findEnum(b sd.Bundle, name string) *sd.EnumDef {
	for i := range b.Enums {
		if b.Enums[i].Name.Name == name {
			return &b.Enums[i]
		}
	}
	return nil
}

func assertAlias(t *testing.T, what string, n sd.Name, want string) {
	t.Helper()
	if n.Alias == nil {
		t.Errorf("%s: alias is nil, want %q", what, want)
		return
	}
	if *n.Alias != want {
		t.Errorf("%s: alias = %q, want %q", what, *n.Alias, want)
	}
}

func assertDesc(t *testing.T, what string, desc *string, want string) {
	t.Helper()
	if desc == nil {
		t.Errorf("%s: description is nil, want %q", what, want)
		return
	}
	if *desc != want {
		t.Errorf("%s: description = %q, want %q", what, *desc, want)
	}
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
