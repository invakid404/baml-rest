package nativeschema

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
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
// static-schema builder over it.
func buildFromSource(t *testing.T, src string) (map[string]sd.Bundle, map[string]string) {
	t.Helper()
	file, err := bamlparser.ParseString("build_test.baml", src)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return BuildStaticSchemas([]SourceFile{{File: file}})
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

// TestBuildStaticSchemasConstraints proves @assert/@check lower into opaque
// schemadescriptor.Constraints on the right descriptor nodes — field
// constraints reassociate onto the field's TYPE (Type.Meta.Constraints), class/
// enum block constraints onto ClassDef/EnumDef.Constraints — with the level,
// nil-vs-present label, and verbatim {{ }} inner expression preserved, repeated
// constraints kept in order, and the whole thing round-tripping through
// FromStaticDescriptor (which must preserve every constraint) + ValidateOutput.
func TestBuildStaticSchemasConstraints(t *testing.T) {
	src := `
enum Ranked {
    LOW
    HIGH
    @@assert(has_values, {{ this|length > 0 }})
}
class Constrained {
    score int @check(positive, {{ this > 0 }}) @assert(bounded, {{ this < 100 }})
    name string @assert({{ this|length > 0 }})
    rank Ranked
    @@assert(class_ok, {{ this.score < 100 }})
}
` + fn("GetConstrained", "Constrained")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetConstrained"]; declined {
		t.Fatalf("GetConstrained declined, want supported: %s", reason)
	}
	b := bundles["GetConstrained"]

	cls := findClass(b, "Constrained")
	if cls == nil {
		t.Fatalf("Constrained class not found: %+v", b.Classes)
	}
	// Class-level @@assert(class_ok, {{ ... }}).
	if len(cls.Constraints) != 1 {
		t.Fatalf("class constraints = %+v, want 1", cls.Constraints)
	}
	assertConstraint(t, "class Constrained", cls.Constraints[0], sd.ConstraintAssert, ptr("class_ok"), " this.score < 100 ")

	// Field `score`: two constraints in source order, both on the field TYPE.
	score := findField(cls, "score")
	if score == nil {
		t.Fatalf("score field not found: %+v", cls.Fields)
	}
	if len(score.Type.Meta.Constraints) != 2 {
		t.Fatalf("score type constraints = %+v, want 2", score.Type.Meta.Constraints)
	}
	assertConstraint(t, "score[0]", score.Type.Meta.Constraints[0], sd.ConstraintCheck, ptr("positive"), " this > 0 ")
	assertConstraint(t, "score[1]", score.Type.Meta.Constraints[1], sd.ConstraintAssert, ptr("bounded"), " this < 100 ")

	// Field `name`: a lone-expression @assert (no label).
	nameField := findField(cls, "name")
	if nameField == nil {
		t.Fatalf("name field not found: %+v", cls.Fields)
	}
	if len(nameField.Type.Meta.Constraints) != 1 {
		t.Fatalf("name type constraints = %+v, want 1", nameField.Type.Meta.Constraints)
	}
	assertConstraint(t, "name[0]", nameField.Type.Meta.Constraints[0], sd.ConstraintAssert, nil, " this|length > 0 ")

	// Field `rank` (enum ref) carries no constraint metadata.
	rank := findField(cls, "rank")
	if rank == nil || len(rank.Type.Meta.Constraints) != 0 {
		t.Fatalf("rank field should carry no constraints: %+v", rank)
	}

	// Enum-level @@assert(has_values, {{ ... }}).
	enm := findEnum(b, "Ranked")
	if enm == nil || len(enm.Constraints) != 1 {
		t.Fatalf("Ranked enum constraints = %+v, want 1", enm)
	}
	assertConstraint(t, "enum Ranked", enm.Constraints[0], sd.ConstraintAssert, ptr("has_values"), " this|length > 0 ")

	// Round-trip: FromStaticDescriptor must preserve every constraint, and the
	// bundle must still validate as an output.
	internal, err := schema.FromStaticDescriptor(b)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	if err := internal.ValidateOutput(); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	iCls, ok := internal.FindClass("Constrained", schema.NonStreaming)
	if !ok {
		t.Fatalf("internal Constrained class not found")
	}
	if len(iCls.Constraints) != 1 || iCls.Constraints[0].Expression != " this.score < 100 " {
		t.Errorf("internal class constraint not preserved: %+v", iCls.Constraints)
	}
	var iScore *schema.ClassField
	for i := range iCls.Fields {
		if iCls.Fields[i].Name.Name == "score" {
			iScore = &iCls.Fields[i]
		}
	}
	if iScore == nil || len(iScore.Type.Meta.Constraints) != 2 {
		t.Fatalf("internal score field constraints not preserved: %+v", iScore)
	}
	if iScore.Type.Meta.Constraints[0].Expression != " this > 0 " || iScore.Type.Meta.Constraints[1].Expression != " this < 100 " {
		t.Errorf("internal score constraints not preserved in order: %+v", iScore.Type.Meta.Constraints)
	}
	iEnum, ok := internal.FindEnum("Ranked")
	if !ok || len(iEnum.Constraints) != 1 || iEnum.Constraints[0].Expression != " this|length > 0 " {
		t.Errorf("internal enum constraint not preserved: %+v", iEnum)
	}
}

// TestBuildStaticSchemasStreaming proves @stream.done/@stream.not_null/
// @stream.with_state lower into the descriptor streaming fields exactly as BAML
// models them: a field stream attribute reassociates onto the field's TYPE
// (Type.Meta.Stream), a class-level @@stream.* onto ClassDef.Stream, while
// ClassField.StreamingNeeded stays unset (BAML's override-derived bool, D8). It
// round-trips through FromStaticDescriptor.
func TestBuildStaticSchemasStreaming(t *testing.T) {
	src := `
class Streamed {
    ready bool @stream.done
    value string @stream.not_null @stream.with_state
    plain int
    @@stream.done
    @@stream.with_state
}
` + fn("GetStreamed", "Streamed")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetStreamed"]; declined {
		t.Fatalf("GetStreamed declined, want supported: %s", reason)
	}
	b := bundles["GetStreamed"]
	cls := findClass(b, "Streamed")
	if cls == nil {
		t.Fatalf("Streamed class not found")
	}

	if want := (sd.StreamingBehavior{Done: true, State: true}); cls.Stream != want {
		t.Errorf("class Stream = %+v, want %+v", cls.Stream, want)
	}

	ready := findField(cls, "ready")
	if want := (sd.StreamingBehavior{Done: true}); ready == nil || ready.Type.Meta.Stream != want {
		t.Errorf("ready field type Stream = %+v, want %+v", ready, want)
	}
	value := findField(cls, "value")
	if value == nil {
		t.Fatalf("value field not found: %+v", cls.Fields)
	}
	if want := (sd.StreamingBehavior{Needed: true, State: true}); value.Type.Meta.Stream != want {
		t.Errorf("value field type Stream = %+v, want %+v", value.Type.Meta.Stream, want)
	}
	// D8: @stream.not_null goes on the TYPE, NOT the override-derived
	// ClassField.StreamingNeeded bool, which a static attribute never sets.
	if value.StreamingNeeded {
		t.Errorf("value.StreamingNeeded = true, want false (override-derived per D8)")
	}
	plain := findField(cls, "plain")
	if plain == nil || !plain.Type.Meta.Stream.IsZero() {
		t.Errorf("plain field should carry no streaming: %+v", plain)
	}

	// Round-trip: FromStaticDescriptor preserves the streaming triple.
	internal, err := schema.FromStaticDescriptor(b)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	if err := internal.ValidateOutput(); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	iCls, ok := internal.FindClass("Streamed", schema.NonStreaming)
	if !ok {
		t.Fatalf("internal Streamed class not found")
	}
	if want := (schema.StreamingBehavior{Done: true, State: true}); iCls.Stream != want {
		t.Errorf("internal class Stream = %+v, want %+v", iCls.Stream, want)
	}
	// REQUIRE the round-tripped `value` field to exist rather than only asserting
	// when found — a dropped/renamed field must fail loudly, not silently pass.
	var iValue *schema.ClassField
	for i := range iCls.Fields {
		if iCls.Fields[i].Name.Name == "value" {
			iValue = &iCls.Fields[i]
		}
	}
	if iValue == nil {
		t.Fatalf("internal Streamed.value field not found: %+v", iCls.Fields)
	}
	if want := (schema.StreamingBehavior{Needed: true, State: true}); iValue.Type.Meta.Stream != want {
		t.Errorf("internal value type Stream = %+v, want %+v", iValue.Type.Meta.Stream, want)
	}
}

// TestBuildStaticSchemasSkip proves @skip DROPS a class field / enum value
// exactly as BAML does (find_existing_class_field / find_enum_value return
// Ok(None) before the definition is built — #586 D11): the skipped member never
// appears in the descriptor or the rendered output_format, and a class reachable
// ONLY through a skipped field vanishes entirely (its type is never reached).
// This mirrors BAML's own render_output_format unit tests
// (skipped_class_fields_are_not_rendered / skipped_variants_are_not_rendered),
// and the ParitySkip integration fixture proves the exact bytes against the real
// v0.223 oracle. @skip fields are optional here because BAML requires it
// (validation "Class field with @skip attribute must be optional"); our parser is
// laxer, but the fixtures stay BAML-valid so the oracle compiles.
func TestBuildStaticSchemasSkip(t *testing.T) {
	src := `
enum SkipEnum {
    KEEP_A
    DROP_B @skip
    KEEP_C
}

// OnlyViaSkip is referenced ONLY by SkipHolder.secret, which is @skip, so it
// must disappear from the output_format entirely (its type is never reached).
class OnlyViaSkip {
    ghost string
}

class SkipHolder {
    kept string
    dropped string? @skip
    category SkipEnum
    secret OnlyViaSkip? @skip
}
` + fn("GetSkip", "SkipHolder")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetSkip"]; declined {
		t.Fatalf("GetSkip declined, want supported (@skip DROPS, it does not decline): %s", reason)
	}
	b, ok := bundles["GetSkip"]
	if !ok {
		t.Fatalf("GetSkip built no descriptor")
	}

	// Skipped field dropped; kept fields remain in source order.
	holder := findClass(b, "SkipHolder")
	if holder == nil {
		t.Fatalf("SkipHolder class not found")
	}
	var names []string
	for i := range holder.Fields {
		names = append(names, holder.Fields[i].Name.Name)
	}
	if want := []string{"kept", "category"}; !reflect.DeepEqual(names, want) {
		t.Errorf("SkipHolder fields = %v, want %v (dropped + secret @skip removed)", names, want)
	}

	// A class reached ONLY through the skipped `secret` field must not be
	// collected at all — @skip stops reachability, matching BAML (it never pushes
	// the skipped field's type onto the stack).
	if findClass(b, "OnlyViaSkip") != nil {
		t.Errorf("OnlyViaSkip should be dropped (reachable only via a @skip field)")
	}

	// Skipped enum value dropped; kept values remain in source order.
	e := findEnum(b, "SkipEnum")
	if e == nil {
		t.Fatalf("SkipEnum not found")
	}
	var vals []string
	for i := range e.Values {
		vals = append(vals, e.Values[i].Name.Name)
	}
	if want := []string{"KEEP_A", "KEEP_C"}; !reflect.DeepEqual(vals, want) {
		t.Errorf("SkipEnum values = %v, want %v (DROP_B @skip removed)", vals, want)
	}

	// The rendered output_format carries no skipped member and no only-via-skip
	// type (the container oracle proves the exact bytes; this pins the shape).
	got := renderNative(t, b)
	for _, banned := range []string{"dropped", "secret", "OnlyViaSkip", "ghost", "DROP_B"} {
		if strings.Contains(got, banned) {
			t.Errorf("rendered output_format contains skipped token %q:\n%s", banned, got)
		}
	}
	for _, wantTok := range []string{"kept", "category", "KEEP_A", "KEEP_C"} {
		if !strings.Contains(got, wantTok) {
			t.Errorf("rendered output_format missing kept token %q:\n%s", wantTok, got)
		}
	}
}

// TestBuildStaticSchemasStreamingNotEmitted is the slice-6 static-streaming
// decline-boundary (#586 D12). Static streaming partialization is a parity-
// DECLINE (kept as the BAML fallback): BAML NEVER renders a streaming
// output_format into the prompt — the Jinja RenderContext carries a single
// output_format populated with the NON-streaming definitions
// (engine/.../prompt_renderer/mod.rs render_prompt, line 149), while the
// partialized/streaming OutputFormatContent (output.to_streaming_type)
// feeds ONLY the partial-response JSONish parser (mod.rs parse, allow_partials).
// So `{{ ctx.output_format }}` resolves to the non-streaming block for streaming
// AND non-streaming calls, and there is no streaming output_format artifact for
// the byte-parity oracle to compare against. The builder therefore emits ONLY the
// non-streaming descriptor: no bundle sets Bundle.Stream and every class is under
// StreamingMode::NonStreaming — even for a function whose output carries @stream.*
// (those lower as metadata on the FINAL non-streaming descriptor, slice 4, and do
// NOT spawn a streaming-mode class). This pins that no partialized descriptor
// leaks into the static build.
func TestBuildStaticSchemasStreamingNotEmitted(t *testing.T) {
	src := `
class Streamed {
    ready bool @stream.done
    value string @stream.not_null @stream.with_state
    plain int
    @@stream.done
}
enum StreamEnum {
    A
    B
}
class Nested {
    e StreamEnum
}
class Holder {
    s Streamed
    n Nested
    items Streamed[]
}
` + fn("GetStreamed", "Streamed") + fn("GetHolder", "Holder")

	bundles, declines := buildFromSource(t, src)
	for _, name := range []string{"GetStreamed", "GetHolder"} {
		if reason, declined := declines[name]; declined {
			t.Fatalf("%s declined, want supported: %s", name, reason)
		}
		b, ok := bundles[name]
		if !ok {
			t.Fatalf("%s built no descriptor", name)
		}
		// D12: no streaming (partialized) descriptor is emitted.
		if b.Stream {
			t.Errorf("%s: Bundle.Stream = true, want false (static streaming is a parity-decline, D12)", name)
		}
		if b.Target.Mode == sd.Streaming {
			t.Errorf("%s: target mode = Streaming, want NonStreaming", name)
		}
		for _, c := range b.Classes {
			if c.Mode != sd.NonStreaming {
				t.Errorf("%s: class %q mode = %q, want NonStreaming (no streaming-mode class emitted, D12)", name, c.Name.Name, c.Mode)
			}
		}
	}

	// Sanity: declining STREAMING partialization does not drop the @stream.*
	// FLAGS — they are still carried on the final non-streaming descriptor
	// (slice-4 behavior), just never used to render a distinct streaming block.
	if cls := findClass(bundles["GetStreamed"], "Streamed"); cls == nil || cls.Stream.IsZero() {
		t.Errorf("GetStreamed: @@stream.* metadata should ride on the non-streaming descriptor")
	}
}

// TestBuildStaticSchemasDocCommentsNoOp proves a `///` doc comment is captured
// as a NO-OP: BAML's output_format renderer never reads doc comments (only
// @description renders — #586 D6, oracle-confirmed), so a doc-commented class/
// field/enum/enum-value builds SUPPORTED, produces NO description, and yields a
// descriptor byte-identical to the same shapes without doc comments. It also
// pins the precedence: with BOTH a `///` and an @description on one node, the
// @description wins (the doc comment is invisible).
func TestBuildStaticSchemasDocCommentsNoOp(t *testing.T) {
	docSrc := `
/// A documented class.
/// Second line.
class Doc {
    /// the value field
    value string
    plain int
}
/// A documented enum.
enum DocEnum {
    /// the first value
    A
    B
}
class DocOut {
    d Doc
    e DocEnum
}
` + fn("GetDoc", "DocOut")

	plainSrc := `
class Doc {
    value string
    plain int
}
enum DocEnum {
    A
    B
}
class DocOut {
    d Doc
    e DocEnum
}
` + fn("GetDoc", "DocOut")

	docBundles, docDeclines := buildFromSource(t, docSrc)
	if reason, declined := docDeclines["GetDoc"]; declined {
		t.Fatalf("doc-commented GetDoc declined, want supported (doc comments are no-ops): %s", reason)
	}
	plainBundles, plainDeclines := buildFromSource(t, plainSrc)
	if reason, declined := plainDeclines["GetDoc"]; declined {
		t.Fatalf("plain GetDoc declined: %s", reason)
	}

	// No description came from any /// doc comment.
	docCls := findClass(docBundles["GetDoc"], "Doc")
	if docCls == nil {
		t.Fatalf("Doc class not found")
	}
	if docCls.Description != nil {
		t.Errorf("class Doc got description %q from a /// doc comment; want nil", *docCls.Description)
	}
	for i := range docCls.Fields {
		if docCls.Fields[i].Description != nil {
			t.Errorf("field %q got description %q from a /// doc comment; want nil", docCls.Fields[i].Name.Name, *docCls.Fields[i].Description)
		}
	}
	docEnum := findEnum(docBundles["GetDoc"], "DocEnum")
	if docEnum == nil {
		t.Fatalf("DocEnum not found")
	}
	for i := range docEnum.Values {
		if docEnum.Values[i].Description != nil {
			t.Errorf("enum value %q got a description from a /// doc comment; want nil", docEnum.Values[i].Name.Name)
		}
	}

	// The doc-commented descriptor must be byte-identical to the plain one, so
	// the rendered output_format cannot differ (proven end-to-end by the oracle).
	if !reflect.DeepEqual(docBundles["GetDoc"], plainBundles["GetDoc"]) {
		t.Errorf("doc-commented descriptor differs from the plain one:\n doc:   %+v\n plain: %+v", docBundles["GetDoc"], plainBundles["GetDoc"])
	}
}

// TestBuildStaticSchemasDocVsDescriptionPrecedence proves that when a node
// carries BOTH a `///` doc comment and an @description, the @description is the
// sole source of the rendered description (the doc comment is invisible — D6).
func TestBuildStaticSchemasDocVsDescriptionPrecedence(t *testing.T) {
	src := `
class DocVsDesc {
    /// doc comment on the field
    field string @description("the real description")
    plain int
}
` + fn("GetDocVsDesc", "DocVsDesc")

	bundles, declines := buildFromSource(t, src)
	if reason, declined := declines["GetDocVsDesc"]; declined {
		t.Fatalf("GetDocVsDesc declined, want supported: %s", reason)
	}
	cls := findClass(bundles["GetDocVsDesc"], "DocVsDesc")
	if cls == nil {
		t.Fatalf("DocVsDesc class not found")
	}
	f := findField(cls, "field")
	if f == nil {
		t.Fatalf("field not found: %+v", cls.Fields)
	}
	assertDesc(t, "field with both /// and @description", f.Description, "the real description")
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
// renderNative lowers a built descriptor and renders its ctx.output_format with
// RenderOptions::default, the same path the integration parity oracle exercises.
func renderNative(t *testing.T, b sd.Bundle) string {
	t.Helper()
	internal, err := schema.FromStaticDescriptor(b)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	got, err := outputformat.Render(internal, outputformat.Options{})
	if err != nil {
		t.Fatalf("outputformat.Render: %v", err)
	}
	return got
}

// TestBuildStaticSchemasRecursion proves the slice-5 recursion detection +
// lowering: a recursive class (TreeNode via a list<TreeNode> field), a
// structural recursive alias (JsonValue recursing through list + map), and
// mutual class recursion (A -> B -> A). Each builds SUPPORTED, populates
// RecursiveClasses / StructuralRecursiveAliases in BAML order, and renders the
// output_format the way BAML v0.223 does (the integration harness proves the
// bytes against the real oracle; here the expected strings pin the shape).
func TestBuildStaticSchemasRecursion(t *testing.T) {
	t.Run("recursive class via list field", func(t *testing.T) {
		src := `class TreeNode {
    value string
    children TreeNodeList?
}
type TreeNodeList = TreeNode[]
` + fn("ParseTree", "TreeNode")

		bundles, declines := buildFromSource(t, src)
		if reason, ok := declines["ParseTree"]; ok {
			t.Fatalf("ParseTree declined, want supported: %s", reason)
		}
		b := bundles["ParseTree"]
		// TreeNode is the only recursive class; TreeNodeList is a non-recursive
		// alias that inlines to TreeNode[], so no structural alias is emitted.
		if got := b.RecursiveClasses; !reflect.DeepEqual(got, []string{"TreeNode"}) {
			t.Errorf("RecursiveClasses = %v, want [TreeNode]", got)
		}
		if len(b.StructuralRecursiveAliases) != 0 {
			t.Errorf("StructuralRecursiveAliases = %v, want none", b.StructuralRecursiveAliases)
		}
		// The recursive field references TreeNode as a class (inlined alias),
		// never re-descending into a fresh copy.
		tn := findClass(b, "TreeNode")
		if tn == nil {
			t.Fatalf("TreeNode class not collected")
		}
		children := findField(tn, "children")
		if children == nil || children.Type.Kind != sd.TypeUnion || children.Type.Union == nil {
			t.Fatalf("children field = %+v, want nullable union", children)
		}
		want := "TreeNode {\n  value: string,\n  children: TreeNode[] or null,\n}\n\nAnswer in JSON using this schema: TreeNode"
		if got := renderNative(t, b); got != want {
			t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("structural recursive alias through list and map", func(t *testing.T) {
		src := `type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>
class JsonContainer {
    data JsonValue
}
` + fn("GetJson", "JsonContainer")

		bundles, declines := buildFromSource(t, src)
		if reason, ok := declines["GetJson"]; ok {
			t.Fatalf("GetJson declined, want supported: %s", reason)
		}
		b := bundles["GetJson"]
		if len(b.RecursiveClasses) != 0 {
			t.Errorf("RecursiveClasses = %v, want none", b.RecursiveClasses)
		}
		if len(b.StructuralRecursiveAliases) != 1 || b.StructuralRecursiveAliases[0].Name != "JsonValue" {
			t.Fatalf("StructuralRecursiveAliases = %+v, want [JsonValue]", b.StructuralRecursiveAliases)
		}
		// JsonContainer.data references the alias by a TypeRecursiveAlias node.
		jc := findClass(b, "JsonContainer")
		if jc == nil {
			t.Fatalf("JsonContainer not collected")
		}
		data := findField(jc, "data")
		if data == nil || data.Type.Kind != sd.TypeRecursiveAlias || data.Type.Name != "JsonValue" {
			t.Fatalf("data field type = %+v, want recursive_alias JsonValue", data)
		}
		want := "JsonValue = int or float or bool or string or JsonValue[] or map<string, JsonValue> or null\n\nAnswer in JSON using this schema:\n{\n  data: JsonValue,\n}"
		if got := renderNative(t, b); got != want {
			t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("mutual class recursion A -> B -> A", func(t *testing.T) {
		src := `class A {
    b B
}
class B {
    a A
}
` + fn("GetA", "A")

		bundles, declines := buildFromSource(t, src)
		if reason, ok := declines["GetA"]; ok {
			t.Fatalf("GetA declined, want supported: %s", reason)
		}
		b := bundles["GetA"]
		// A and B share one SCC; BAML's recursive_classes carries them in cycle
		// (Tarjan min-id-first) order = declaration order [A, B].
		if got := b.RecursiveClasses; !reflect.DeepEqual(got, []string{"A", "B"}) {
			t.Errorf("RecursiveClasses = %v, want [A, B]", got)
		}
		want := "A {\n  b: B,\n}\n\nB {\n  a: A,\n}\n\nAnswer in JSON using this schema: A"
		if got := renderNative(t, b); got != want {
			t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("two independent structural recursive aliases (disjoint SCCs)", func(t *testing.T) {
		// ListNode and StrMap are DISJOINT single-alias structural cycles (each
		// recurses through its own container), both reached from one holder. This
		// exercises MULTI-entry StructuralRecursiveAliases ordering: the holder's
		// fields are [a: ListNode, b: StrMap] in declaration order, so BAML's LIFO
		// reachability pops the last field first and emits [StrMap, ListNode] —
		// reverse of reference. A single-alias test could not catch a mis-ordering
		// here (the exact gap CodeRabbit flagged on PR #594).
		src := `type ListNode = ListNode[]
type StrMap = map<string, StrMap>
class TwoRecursions {
    a ListNode
    b StrMap
}
` + fn("GetTwoAliases", "TwoRecursions")

		bundles, declines := buildFromSource(t, src)
		if reason, ok := declines["GetTwoAliases"]; ok {
			t.Fatalf("GetTwoAliases declined, want supported: %s", reason)
		}
		b := bundles["GetTwoAliases"]
		if len(b.RecursiveClasses) != 0 {
			t.Errorf("RecursiveClasses = %v, want none", b.RecursiveClasses)
		}
		// BOTH disjoint SCCs detected, in reverse-of-reference (LIFO) order.
		var aliasNames []string
		for _, a := range b.StructuralRecursiveAliases {
			aliasNames = append(aliasNames, a.Name)
		}
		if !reflect.DeepEqual(aliasNames, []string{"StrMap", "ListNode"}) {
			t.Errorf("StructuralRecursiveAliases order = %v, want [StrMap, ListNode]", aliasNames)
		}
		want := "StrMap = map<string, StrMap>\nListNode = ListNode[]\n\nAnswer in JSON using this schema:\n{\n  a: ListNode,\n  b: StrMap,\n}"
		if got := renderNative(t, b); got != want {
			t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("two disjoint recursive classes", func(t *testing.T) {
		// SelfA and SelfB are two INDEPENDENT self-recursive classes (disjoint
		// single-class SCCs) reached from one holder. This exercises MULTI-entry
		// RecursiveClasses ordering: fields [a: SelfA, b: SelfB] pop last-first,
		// so recursive_classes hoists as [SelfB, SelfA] — reverse of reference.
		src := `class SelfA {
    self_a SelfA?
    x int
}
class SelfB {
    self_b SelfB?
    y int
}
class TwoSelfHolder {
    a SelfA
    b SelfB
}
` + fn("GetTwoSelf", "TwoSelfHolder")

		bundles, declines := buildFromSource(t, src)
		if reason, ok := declines["GetTwoSelf"]; ok {
			t.Fatalf("GetTwoSelf declined, want supported: %s", reason)
		}
		b := bundles["GetTwoSelf"]
		if got := b.RecursiveClasses; !reflect.DeepEqual(got, []string{"SelfB", "SelfA"}) {
			t.Errorf("RecursiveClasses = %v, want [SelfB, SelfA] (reverse-of-reference)", got)
		}
		if len(b.StructuralRecursiveAliases) != 0 {
			t.Errorf("StructuralRecursiveAliases = %v, want none", b.StructuralRecursiveAliases)
		}
		want := "SelfB {\n  self_b: SelfB or null,\n  y: int,\n}\n\nSelfA {\n  self_a: SelfA or null,\n  x: int,\n}\n\nAnswer in JSON using this schema:\n{\n  a: SelfA,\n  b: SelfB,\n}"
		if got := renderNative(t, b); got != want {
			t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestBuildStaticSchemasDeclines(t *testing.T) {
	cases := []struct {
		name          string
		fnName        string
		src           string
		wantReasonSub string
	}{
		{
			// A cycle NOT mediated by a list/map is invalid (BAML rejects it at
			// validation). The builder declines it fail-closed rather than
			// emitting an approximation. Recursive CLASSES and STRUCTURAL
			// recursive aliases (through list/map) are now SUPPORTED — see
			// TestBuildStaticSchemasRecursion.
			name:          "direct alias cycle",
			fnName:        "GetSelf",
			src:           "type Self = Self\n" + fn("GetSelf", "Self"),
			wantReasonSub: "direct/degenerate alias cycle",
		},
		{
			// A cycle through a union (not a list/map) is also degenerate/invalid.
			name:          "union alias cycle",
			fnName:        "GetUnionCycle",
			src:           "type UC = int | UC\n" + fn("GetUnionCycle", "UC"),
			wantReasonSub: "direct/degenerate alias cycle",
		},
		{
			// A direct optional self-cycle (`type A = A?`) is not mediated by a
			// list/map either — invalid.
			name:          "optional self alias cycle",
			fnName:        "GetOptCycle",
			src:           "type OC = OC?\n" + fn("GetOptCycle", "OC"),
			wantReasonSub: "direct/degenerate alias cycle",
		},
		{
			// A structural cycle spanning TWO aliases (P = Q[], Q = P[]) is valid
			// structural recursion in BAML, but slice 5 lowers only single-alias
			// structural cycles; a multi-alias cycle declines fail-closed (its
			// hoist order is not oracle-proven here).
			name:          "multi-alias structural cycle",
			fnName:        "GetMultiAlias",
			src:           "type PA = QA[]\ntype QA = PA[]\n" + fn("GetMultiAlias", "PA"),
			wantReasonSub: "not supported yet",
		},
		{
			// A recursive class whose output graph reaches MEDIA still declines:
			// media output is rejected by ValidateOutput even though the class is
			// a legal recursive class (MediaTreeNode-style).
			name:   "recursion reaching media output",
			fnName: "GetMediaTree",
			src: `class MediaTreeNode {
    label string
    thumbnail image?
    children MediaTreeList?
}
type MediaTreeList = MediaTreeNode[]
` + fn("GetMediaTree", "MediaTreeNode"),
			wantReasonSub: "media is not usable as an output type",
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
			name:   "enum value constraint (no descriptor home)",
			fnName: "GetValConstraint",
			src: `enum ValConstraint {
    A @check(pos, {{ this }})
    B
}
class ValConstraintOut {
    v ValConstraint
}
` + fn("GetValConstraint", "ValConstraintOut"),
			wantReasonSub: "attribute",
		},
		{
			name:   "enum value stream attribute (no descriptor home)",
			fnName: "GetValStream",
			src: `enum ValStream {
    A @stream.done
    B
}
class ValStreamOut {
    v ValStream
}
` + fn("GetValStream", "ValStreamOut"),
			wantReasonSub: "attribute",
		},
		{
			name:   "enum-level @@stream block attribute (no descriptor home)",
			fnName: "GetEnumStream",
			src: `enum EnumStream {
    A
    B
    @@stream.done
}
class EnumStreamOut {
    v EnumStream
}
` + fn("GetEnumStream", "EnumStreamOut"),
			wantReasonSub: "stream",
		},
		{
			name:   "nested-type constraint (reassociation not performed)",
			fnName: "GetNestedConstraint",
			src: `class NestedConstraint {
    m map<string, int @check(pos, {{ this > 0 }})>
}
` + fn("GetNestedConstraint", "NestedConstraint"),
			wantReasonSub: "attribute",
		},
		{
			name:   "constraint missing {{ }} expression",
			fnName: "GetBadConstraint",
			src: `class BadConstraint {
    x int @assert(foo)
}
` + fn("GetBadConstraint", "BadConstraint"),
			wantReasonSub: "Jinja",
		},
		{
			name:   "constraint label is not a bare identifier",
			fnName: "GetBadLabel",
			src: `class BadLabel {
    x int @check("lbl", {{ this > 0 }})
}
` + fn("GetBadLabel", "BadLabel"),
			wantReasonSub: "identifier",
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
// functions still build even when the file also declares media and @@dynamic
// outputs. A recursive class (ParseTree/TreeNode) is now SUPPORTED, so it is
// asserted to BUILD alongside the plain supported functions.
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
		fn("ParseTree", "TreeNode") + // supported: recursive class
		fn("DescribeCaption", "ImageWithCaption") + // declined: media
		fn("GetDynamic", "DynamicOutput") // declined: @@dynamic

	bundles, declines := buildFromSource(t, src)

	for _, ok := range []string{"GetSimple", "GetPerson", "ParseTree"} {
		if _, built := bundles[ok]; !built {
			t.Errorf("supported function %q was not built (reason: %q)", ok, declines[ok])
		}
	}
	for _, bad := range []string{"DescribeCaption", "GetDynamic"} {
		if _, built := bundles[bad]; built {
			t.Errorf("unsupported function %q was built, want decline", bad)
		}
		if _, declined := declines[bad]; !declined {
			t.Errorf("unsupported function %q was not declined", bad)
		}
	}
}

// TestBuildStaticSchemasRegularCommentNotDeclined proves an ordinary `//`
// comment (two slashes) does not perturb the build: the shared lexer elides it,
// so a class/enum/field/value preceded by a regular comment builds normally.
// A `///` doc comment is likewise a no-op — NOT a decline — captured by BAML but
// never rendered in ctx.output_format (see TestBuildStaticSchemasDocCommentsNoOp
// and #586 D6); neither comment form affects the built descriptor.
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

func findField(c *sd.ClassDef, name string) *sd.ClassField {
	for i := range c.Fields {
		if c.Fields[i].Name.Name == name {
			return &c.Fields[i]
		}
	}
	return nil
}

// ptr returns a pointer to s, for building expected nil-vs-present values.
func ptr(s string) *string { return &s }

// assertConstraint checks a constraint's level, expression, and nil-vs-present
// label against expectations.
func assertConstraint(t *testing.T, what string, c sd.Constraint, level sd.ConstraintLevel, label *string, expr string) {
	t.Helper()
	if c.Level != level {
		t.Errorf("%s: level = %q, want %q", what, c.Level, level)
	}
	if c.Expression != expr {
		t.Errorf("%s: expression = %q, want %q", what, c.Expression, expr)
	}
	switch {
	case label == nil && c.Label != nil:
		t.Errorf("%s: label = %q, want nil", what, *c.Label)
	case label != nil && c.Label == nil:
		t.Errorf("%s: label = nil, want %q", what, *label)
	case label != nil && c.Label != nil && *label != *c.Label:
		t.Errorf("%s: label = %q, want %q", what, *c.Label, *label)
	}
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
