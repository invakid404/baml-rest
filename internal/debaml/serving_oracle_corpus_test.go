//go:build integration

package debaml

// The serving-shaped CORPUS.
//
// Every row is a complete serving unit: a schema.Bundle, the .baml function
// rendered from it, and the exact raw assistant text. Both legs are driven over
// the SAME two inputs, so the differential compares two engines rather than two
// descriptions of a fixture.
//
// The bundles reuse the helpers boundary_decline_test.go already declares
// (stringType, intType, scalarField, constrained, ptr) so the shapes the oracle
// drives and the shapes the unit-level decline guards pin are built the same way.

import (
	"fmt"

	"github.com/invakid404/baml-rest/internal/schema"
)

// ---------------------------------------------------------------------------
// Bundle helpers
// ---------------------------------------------------------------------------

func soFloatType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat}
}

func soBoolType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool}
}

func soClassType(name string) schema.Type {
	return schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming}
}

func soEnumType(name string) schema.Type {
	return schema.Type{Kind: schema.TypeEnum, Name: name}
}

func soListOf(elem schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeList, Elem: ptr(elem)}
}

func soMapOf(key, value schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeMap, Key: ptr(key), Value: ptr(value)}
}

func soOptional(v schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{v}, Nullable: true}}
}

func soUnionOf(variants ...schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: variants}}
}

// soCheck / soAssert build the two constraint levels. A @check MUST carry a label
// (BAML's grammar requires it); an @assert may, and the corpus uses labelled
// asserts throughout because stock names the label in its rejection message and
// the differential matches on it.
func soCheck(label, expr string) schema.Constraint {
	l := label
	return schema.Constraint{Level: schema.ConstraintCheck, Expression: expr, Label: &l}
}

func soAssert(label, expr string) schema.Constraint {
	l := label
	return schema.Constraint{Level: schema.ConstraintAssert, Expression: expr, Label: &l}
}

// soWith attaches constraints to a type node.
func soWith(t schema.Type, cs ...schema.Constraint) schema.Type {
	t.Meta.Constraints = append(append([]schema.Constraint(nil), t.Meta.Constraints...), cs...)
	return t
}

// soField builds a class field, optionally aliased.
func soField(name string, t schema.Type) schema.ClassField {
	return schema.ClassField{Name: schema.Name{Name: name}, Type: t}
}

func soAliasedField(name, alias string, t schema.Type) schema.ClassField {
	a := alias
	return schema.ClassField{Name: schema.Name{Name: name, Alias: &a}, Type: t}
}

// soBundle assembles a bundle and builds its indexes. A hand-built Bundle whose
// indexes were never built silently fails every FindClass/FindEnum lookup inside
// coerce, so this is not optional bookkeeping.
func soBundle(target schema.Type, classes []schema.ClassDef, enums []schema.EnumDef) *schema.Bundle {
	b := &schema.Bundle{Target: target, Classes: classes, Enums: enums}
	if err := b.RebuildIndexes(); err != nil {
		panic(fmt.Sprintf("serving oracle: corpus bundle indexes: %v", err))
	}
	return b
}

// soClassOf builds a class definition.
func soClassOf(name string, fields []schema.ClassField, constraints ...schema.Constraint) schema.ClassDef {
	return schema.ClassDef{
		Name: schema.Name{Name: name}, Mode: schema.NonStreaming,
		Fields: fields, Constraints: constraints,
	}
}

// soOneFieldBundle is the shape most rows use: a single class with one field `v`,
// returned by the function.
func soOneFieldBundle(class string, fieldType schema.Type, classConstraints ...schema.Constraint) *schema.Bundle {
	return soBundle(soClassType(class),
		[]schema.ClassDef{soClassOf(class, []schema.ClassField{soField("v", fieldType)}, classConstraints...)},
		nil)
}

// soSuitEnum is the aliased enum every enum row uses. The alias is on the VARIANT,
// which is the ingress the model writes and the canonical name the predicate must
// observe.
func soSuitEnum(name string, constraints ...schema.Constraint) schema.EnumDef {
	alias := "hearts_alias"
	return schema.EnumDef{
		Name: schema.Name{Name: name},
		Values: []schema.EnumValue{
			{Name: schema.Name{Name: "Hearts", Alias: &alias}},
			{Name: schema.Name{Name: "Spades"}},
		},
		Constraints: constraints,
	}
}

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

// servingOracleFixtures is the corpus. Stock and Native are RECORDINGS; see
// [TestServingOracleDifferential] for the recording mode that produces them.
var servingOracleFixtures = []servingOracleFixture{
	// -- scalars ------------------------------------------------------------
	{
		Name: "scalar_int_check_pass", Family: "scalar",
		Doc:    "an int @check whose predicate holds: stock emits the value carrying a succeeded check",
		Bundle: soOneFieldBundle("SoIntPass", soWith(intType(), soCheck("gt", "this > 0"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoIntPass{v=int:5} checks=[$.v|\"gt\"|this > 0=succeeded]",
		Native: "value class:SoIntPass{v=int:5} events=[$.v|type_meta/check/\"gt\"/this > 0=true]",
	},
	{
		Name: "scalar_int_check_fail", Family: "scalar",
		Doc:    "a FALSE @check is DATA: stock still emits the value, with status failed",
		Bundle: soOneFieldBundle("SoIntFail", soWith(intType(), soCheck("gt", "this > 100"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoIntFail{v=int:5} checks=[$.v|\"gt\"|this > 100=failed]",
		Native: "value class:SoIntFail{v=int:5} events=[$.v|type_meta/check/\"gt\"/this > 100=false]",
	},
	{
		Name: "scalar_int_assert_pass", Family: "scalar",
		Doc:    "a holding @assert leaves no trace in the value: no check entry is emitted",
		Bundle: soOneFieldBundle("SoIntAssertPass", soWith(intType(), soAssert("gt", "this > 0"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoIntAssertPass{v=int:5} checks=[]",
		Native: "value class:SoIntAssertPass{v=int:5} events=[$.v|type_meta/assert/\"gt\"/this > 0=true]",
	},
	{
		Name: "scalar_int_assert_fail", Family: "scalar",
		Doc:    "a FALSE @assert REJECTS the node — a different outcome class from a false check",
		Bundle: soOneFieldBundle("SoIntAssertFail", soWith(intType(), soAssert("gt", "this > 100"))),
		Raw:    `{"v":5}`,
		Stock:  "assertion-failure Failed while parsing required fields: missing=0, unparsed=1 | Failed to parse field v: <root>: Assertions failed. / - <root>: Failed: gt this > 100 | Assertions failed. | Failed: gt this > 100",
		Native: "assertion-failure class:SoIntAssertFail{v=int:5} events=[$.v|type_meta/assert/\"gt\"/this > 100=false]",
	},
	{
		Name: "scalar_float_check", Family: "scalar",
		Doc:    "float leaf: the value domain is the SCHEMA's, so 1.5 is a float on both legs",
		Bundle: soOneFieldBundle("SoFloat", soWith(soFloatType(), soCheck("gt", "this > 1.0"))),
		Raw:    `{"v":1.5}`,
		Stock:  "value class:SoFloat{v=float:1.5} checks=[$.v|\"gt\"|this > 1.0=succeeded]",
		Native: "value class:SoFloat{v=float:1.5} events=[$.v|type_meta/check/\"gt\"/this > 1.0=true]",
	},
	{
		Name: "scalar_bool_check", Family: "scalar",
		Doc:    "bool leaf, and a predicate that IS the value rather than a comparison",
		Bundle: soOneFieldBundle("SoBool", soWith(soBoolType(), soCheck("istrue", "this"))),
		Raw:    `{"v":true}`,
		Stock:  "value class:SoBool{v=bool:true} checks=[$.v|\"istrue\"|this=succeeded]",
		Native: "value class:SoBool{v=bool:true} events=[$.v|type_meta/check/\"istrue\"/this=true]",
	},
	{
		Name: "scalar_string_check_fail", Family: "scalar",
		Doc:    "CONTROL for the bare-string skip: the same false predicate on a string FIELD does run",
		Bundle: soOneFieldBundle("SoStrField", soWith(stringType(), soCheck("eq", `this == "expected"`))),
		Raw:    `{"v":"actual"}`,
		Stock:  "value class:SoStrField{v=string:\"actual\"} checks=[$.v|\"eq\"|this == \"expected\"=failed]",
		Native: "value class:SoStrField{v=string:\"actual\"} events=[$.v|type_meta/check/\"eq\"/this == \"expected\"=false]",
	},
	{
		Name: "scalar_optional_null", Family: "scalar",
		Doc:    "an optional field given null: the null arm wins and the predicate runs against null",
		Bundle: soOneFieldBundle("SoOptNull", soWith(soOptional(intType()), soCheck("isnull", "this == none"))),
		Raw:    `{"v":null}`,
		Divergence: "native refuses `none`: the closed predicate grammar has no null literal, so the " +
			"comparison is declined rather than decided",
		Stock:  "value class:SoOptNull{v=null} checks=[$.v|\"isnull\"|this == none=succeeded]",
		Native: "evaluator-unsupported class:SoOptNull{v=null} events=[$.v|type_meta/check/\"isnull\"/this == none=unsupported]",
	},
	{
		Name: "scalar_multi_check_order", Family: "scalar",
		Doc: "three checks in DECLARATION order with mixed results — order is part of the envelope",
		Bundle: soOneFieldBundle("SoMulti", soWith(intType(),
			soCheck("a", "this > 0"), soCheck("b", "this > 3"), soCheck("c", "this > 100"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoMulti{v=int:5} checks=[$.v|\"a\"|this > 0=succeeded $.v|\"b\"|this > 3=succeeded $.v|\"c\"|this > 100=failed]",
		Native: "value class:SoMulti{v=int:5} events=[$.v|type_meta/check/\"a\"/this > 0=true $.v|type_meta/check/\"b\"/this > 3=true $.v|type_meta/check/\"c\"/this > 100=false]",
	},

	// -- enums --------------------------------------------------------------
	{
		Name: "enum_canonical", Family: "enum",
		Doc: "the model writes an UNALIASED variant; the predicate observes the canonical name",
		Bundle: soBundle(soClassType("SoEnumCanon"),
			[]schema.ClassDef{soClassOf("SoEnumCanon", []schema.ClassField{
				soField("v", soWith(soEnumType("SoSuit"), soCheck("spades", `this == "Spades"`))),
			})},
			[]schema.EnumDef{soSuitEnum("SoSuit")}),
		Raw:    `{"v":"Spades"}`,
		Stock:  "value class:SoEnumCanon{v=enum:SoSuit=Spades} checks=[$.v|\"spades\"|this == \"Spades\"=succeeded]",
		Native: "value class:SoEnumCanon{v=enum:SoSuit=Spades} events=[$.v|type_meta/check/\"spades\"/this == \"Spades\"=true]",
	},
	{
		Name: "enum_alias_shadows_canonical", Family: "enum",
		Doc: "an @alias on a variant REPLACES its ingress spelling rather than adding one: the model " +
			"writing the CANONICAL name no longer matches, and BOTH legs refuse the value",
		Bundle: soBundle(soClassType("SoEnumShadow"),
			[]schema.ClassDef{soClassOf("SoEnumShadow", []schema.ClassField{
				soField("v", soWith(soEnumType("SoSuit"), soCheck("hearts", `this == "Hearts"`))),
			})},
			[]schema.EnumDef{soSuitEnum("SoSuit")}),
		Raw:    `{"v":"Hearts"}`,
		Stock:  "coercion-error Failed while parsing required fields: missing=0, unparsed=1 | Failed to parse field v: v: Expected SoSuit @check(hearts, {{..}} ) enum value, got String(\"Hearts\", Complete). | Expected SoSuit @check(hearts, {{..}} ) enum value, got String(\"Hearts\", Complete).",
		Native: "coercion-error debaml: constraint-state collector: $: production coercion did not succeed (the collector models a SUCCESSFUL canonical coercion only): bamlutils: de-BAML parser unsupported: class \"SoEnumShadow\": required field \"v\" provably fails to coerce (BAML errors the class)",
	},
	{
		Name: "enum_alias_ingress", Family: "asymmetry",
		Doc: "ASYMMETRY 3: the model writes the ALIAS, the predicate sees the CANONICAL variant — " +
			"`this == \"Hearts\"` holds and `this == \"hearts_alias\"` does not",
		Bundle: soBundle(soClassType("SoEnumAlias"),
			[]schema.ClassDef{soClassOf("SoEnumAlias", []schema.ClassField{
				soField("v", soWith(soEnumType("SoSuit"),
					soCheck("canonical", `this == "Hearts"`), soCheck("alias", `this == "hearts_alias"`))),
			})},
			[]schema.EnumDef{soSuitEnum("SoSuit")}),
		Raw:    `{"v":"hearts_alias"}`,
		Stock:  "value class:SoEnumAlias{v=enum:SoSuit=Hearts} checks=[$.v|\"canonical\"|this == \"Hearts\"=succeeded $.v|\"alias\"|this == \"hearts_alias\"=failed]",
		Native: "value class:SoEnumAlias{v=enum:SoSuit=Hearts} events=[$.v|type_meta/check/\"canonical\"/this == \"Hearts\"=true $.v|type_meta/check/\"alias\"/this == \"hearts_alias\"=false]",
	},

	// -- classes ------------------------------------------------------------
	{
		Name: "class_level_check", Family: "class",
		Doc: "a class-level @@check: the predicate sees the whole class value",
		Bundle: soBundle(soClassType("SoClsLevel"),
			[]schema.ClassDef{soClassOf("SoClsLevel",
				[]schema.ClassField{soField("s", stringType()), soField("n", intType())},
				soCheck("hass", "this.s|length > 0"))},
			nil),
		Raw:    `{"s":"hi","n":2}`,
		Stock:  "value class:SoClsLevel{s=string:\"hi\",n=int:2} checks=[$|\"hass\"|this.s|length > 0=succeeded~uncertified-order]",
		Native: "value class:SoClsLevel{s=string:\"hi\",n=int:2} events=[$|declaration/check/\"hass\"/this.s|length > 0=true]",
	},
	{
		Name: "class_level_assert_fail", Family: "class",
		Doc: "a class-level @assert that fails rejects the whole class",
		Bundle: soBundle(soClassType("SoClsAssert"),
			[]schema.ClassDef{soClassOf("SoClsAssert",
				[]schema.ClassField{soField("s", stringType())},
				soAssert("long", "this.s|length > 10"))},
			nil),
		Raw:    `{"s":"hi"}`,
		Stock:  "assertion-failure Assertions failed. | Failed: long this.s|length > 10 | Failed: long this.s|length > 10",
		Native: "assertion-failure class:SoClsAssert{s=string:\"hi\"} events=[$|declaration/assert/\"long\"/this.s|length > 10=false]",
	},
	{
		Name: "class_schema_order", Family: "class",
		Doc: "fields declared b,a — NOT alphabetical — so an observation that reports a,b is reporting " +
			"a sorted enumeration rather than the schema order",
		Bundle: soBundle(soClassType("SoOrder"),
			[]schema.ClassDef{soClassOf("SoOrder",
				[]schema.ClassField{
					soField("b", soWith(intType(), soCheck("bpos", "this > 0"))),
					soField("a", soWith(stringType(), soCheck("astr", `this == "x"`))),
				})},
			nil),
		Raw:    `{"a":"x","b":2}`,
		Stock:  "value class:SoOrder{b=int:2,a=string:\"x\"} checks=[$.b|\"bpos\"|this > 0=succeeded $.a|\"astr\"|this == \"x\"=succeeded]",
		Native: "value class:SoOrder{b=int:2,a=string:\"x\"} events=[$.b|type_meta/check/\"bpos\"/this > 0=true $.a|type_meta/check/\"astr\"/this == \"x\"=true]",
	},
	{
		Name: "class_field_alias", Family: "alias",
		Doc: "ASYMMETRY 3, class form: the model writes the field ALIAS `qty`, the canonical entry is " +
			"`amount`, and the class-level predicate reads the canonical name",
		Bundle: soBundle(soClassType("SoAliasCls"),
			[]schema.ClassDef{soClassOf("SoAliasCls",
				[]schema.ClassField{soAliasedField("amount", "qty", intType())},
				soCheck("amount", "this.amount == 3"))},
			nil),
		Raw:    `{"qty":3}`,
		Stock:  "value class:SoAliasCls{amount=int:3} checks=[$|\"amount\"|this.amount == 3=succeeded~uncertified-order]",
		Native: "value class:SoAliasCls{amount=int:3} events=[$|declaration/check/\"amount\"/this.amount == 3=true]",
	},
	{
		Name: "class_nested", Family: "class",
		Doc: "a nested class member with a constraint on the OUTER field and one INSIDE the inner class",
		Bundle: soBundle(soClassType("SoOuter"),
			[]schema.ClassDef{
				soClassOf("SoOuter", []schema.ClassField{
					soField("i", soWith(soClassType("SoInner"), soCheck("innerb", "this.b > 0"))),
				}),
				soClassOf("SoInner", []schema.ClassField{
					soField("b", soWith(intType(), soCheck("bpos", "this > 1"))),
					soField("a", stringType()),
				}),
			}, nil),
		Raw:    `{"i":{"a":"x","b":2}}`,
		Stock:  "value class:SoOuter{i=class:SoInner{b=int:2,a=string:\"x\"}} checks=[$.i|\"innerb\"|this.b > 0=succeeded $.i.b|\"bpos\"|this > 1=succeeded]",
		Native: "value class:SoOuter{i=class:SoInner{b=int:2,a=string:\"x\"}} events=[$.i|type_meta/check/\"innerb\"/this.b > 0=true $.i.b|type_meta/check/\"bpos\"/this > 1=true]",
	},

	// -- lists --------------------------------------------------------------
	{
		Name: "list_type_check", Family: "list",
		Doc:    "a constraint on the LIST type sees the whole list",
		Bundle: soOneFieldBundle("SoListType", soWith(soListOf(intType()), soCheck("len", "this|length == 3"))),
		Raw:    `{"v":[1,2,3]}`,
		Stock:  "value class:SoListType{v=list[int:1,int:2,int:3]} checks=[$.v|\"len\"|this|length == 3=succeeded]",
		Native: "value class:SoListType{v=list[int:1,int:2,int:3]} events=[$.v|type_meta/check/\"len\"/this|length == 3=true]",
	},
	{
		Name: "list_elem_check", Family: "list",
		Doc:    "a constraint on the ELEMENT runs once per element, in order, with per-element results",
		Bundle: soOneFieldBundle("SoListElem", soListOf(soWith(intType(), soCheck("pos", "this > 0")))),
		Raw:    `{"v":[1,-2,3]}`,
		Stock:  "value class:SoListElem{v=list[int:1,int:-2,int:3]} checks=[$.v[0]|\"pos\"|this > 0=succeeded $.v[1]|\"pos\"|this > 0=failed $.v[2]|\"pos\"|this > 0=succeeded]",
		Native: "value class:SoListElem{v=list[int:1,int:-2,int:3]} events=[$.v[0]|type_meta/check/\"pos\"/this > 0=true $.v[1]|type_meta/check/\"pos\"/this > 0=false $.v[2]|type_meta/check/\"pos\"/this > 0=true]",
	},
	{
		Name: "list_dropped_elem", Family: "list",
		Doc: "an element BAML cannot parse is dropped from the emitted list; the surviving elements' " +
			"constraints still run, and the native input index no longer equals the emitted index",
		Bundle: soOneFieldBundle("SoListDrop", soListOf(soWith(intType(), soCheck("pos", "this > 0")))),
		Raw:    `{"v":[1,{"nope":true},3]}`,
		Divergence: "stock reports each surviving element's check TWICE — the list is coerced again after " +
			"the ArrayItemParseError skip — where native records one event per element",
		Stock:  "value class:SoListDrop{v=list[int:1,int:3]} checks=[$.v[0]|\"pos\"|this > 0=succeeded $.v[0]|\"pos\"|this > 0=succeeded $.v[1]|\"pos\"|this > 0=succeeded $.v[1]|\"pos\"|this > 0=succeeded]",
		Native: "value class:SoListDrop{v=list[int:1,int:3]} events=[$.v[0]|type_meta/check/\"pos\"/this > 0=true $.v[2]|type_meta/check/\"pos\"/this > 0=true] skipped=[$.v[1]|type_meta/check/\"pos\"/this > 0~would-be-not-evaluated $.v[1]|node:skipped_child_or_union_path:element dropped by BAML's ArrayItemParseError partial-array skip]",
	},
	{
		Name: "list_nested_dropped_elem", Family: "list",
		Doc: "a dropped element inside a NESTED list: the constraint sites after it run at input " +
			"coordinates the emitted list no longer uses, and the drop is recorded under an owning-list " +
			"path that is itself indexed ($.v[1].w). It is the row that drives the nested input-vs-emitted " +
			"alignment end to end, producer through consumer",
		Bundle: soBundle(soClassType("SoNestDrop"),
			[]schema.ClassDef{
				soClassOf("SoNestDrop", []schema.ClassField{
					soField("v", soListOf(soClassType("SoNestDropInner"))),
				}),
				soClassOf("SoNestDropInner", []schema.ClassField{
					soField("w", soListOf(soWith(intType(), soCheck("pos", "this > 0")))),
				}),
			}, nil),
		Raw: `{"v":[{"w":[9]},{"w":[1,{"bad":true},3]}]}`,
		Divergence: "stock reports each surviving inner element's check TWICE — the inner list is coerced " +
			"again after the ArrayItemParseError skip — where native records one event per element",
		Stock:  "value class:SoNestDrop{v=list[class:SoNestDropInner{w=list[int:9]},class:SoNestDropInner{w=list[int:1,int:3]}]} checks=[$.v[0].w[0]|\"pos\"|this > 0=succeeded $.v[1].w[0]|\"pos\"|this > 0=succeeded $.v[1].w[0]|\"pos\"|this > 0=succeeded $.v[1].w[1]|\"pos\"|this > 0=succeeded $.v[1].w[1]|\"pos\"|this > 0=succeeded]",
		Native: "value class:SoNestDrop{v=list[class:SoNestDropInner{w=list[int:9]},class:SoNestDropInner{w=list[int:1,int:3]}]} events=[$.v[0].w[0]|type_meta/check/\"pos\"/this > 0=true $.v[1].w[0]|type_meta/check/\"pos\"/this > 0=true $.v[1].w[2]|type_meta/check/\"pos\"/this > 0=true] skipped=[$.v[1].w[1]|type_meta/check/\"pos\"/this > 0~would-be-not-evaluated $.v[1].w[1]|node:skipped_child_or_union_path:element dropped by BAML's ArrayItemParseError partial-array skip]",
	},
	{
		Name: "list_single_to_array", Family: "list",
		Doc:    "a non-array value is wrapped into a one-element list, and the element constraint runs on it",
		Bundle: soOneFieldBundle("SoListWrap", soListOf(soWith(intType(), soCheck("pos", "this > 0")))),
		Raw:    `{"v":7}`,
		Divergence: "stock reports the wrapped element's check TWICE — the value is coerced once bare and " +
			"once inside the synthesized list — where native records one event",
		Stock:  "value class:SoListWrap{v=list[int:7]} checks=[$.v[0]|\"pos\"|this > 0=succeeded $.v[0]|\"pos\"|this > 0=succeeded]",
		Native: "value class:SoListWrap{v=list[int:7]} events=[$.v[0]|type_meta/check/\"pos\"/this > 0=true]",
	},

	// -- maps ---------------------------------------------------------------
	{
		Name: "map_value_check", Family: "map",
		Doc: "constrained map VALUES, with the model's key order preserved (b before a) — a sorted " +
			"readback would be a different envelope",
		Bundle: soOneFieldBundle("SoMapVal", soMapOf(stringType(), soWith(intType(), soCheck("pos", "this > 0")))),
		Raw:    `{"v":{"b":1,"a":-2}}`,
		Stock:  "value class:SoMapVal{v=map{b=int:1,a=int:-2}} checks=[$.v[\"b\"]|\"pos\"|this > 0=succeeded $.v[\"a\"]|\"pos\"|this > 0=failed]",
		Native: "value class:SoMapVal{v=map{b=int:1,a=int:-2}} events=[$.v[\"b\"]|type_meta/check/\"pos\"/this > 0=true $.v[\"a\"]|type_meta/check/\"pos\"/this > 0=false]",
	},
	{
		Name: "map_key_check", Family: "map",
		Doc: "a constraint on the map KEY type: stock evaluates NOTHING for it (no check reaches the " +
			"value) and native records the key node as a policy-declined shape — the negative-admission fixture",
		Bundle: soOneFieldBundle("SoMapKey", soMapOf(soWith(stringType(), soCheck("k", "this|length > 0")), intType())),
		Raw:    `{"v":{"b":1}}`,
		Stock:  "value class:SoMapKey{v=map{b=int:1}} checks=[]",
		Native: "value class:SoMapKey{v=map{b=int:1}} events=[] skipped=[$.v.<key>|type_meta/check/\"k\"/this|length > 0~would-be-not-evaluated $.v.<key>|node:policy_declined_constrained_shape:map-key constraints stay a negative-admission fixture in Slice 7.2a]",
	},

	// -- unions -------------------------------------------------------------
	{
		Name: "union_constrained_arm_wins", Family: "union",
		Doc:    "the constrained arm wins, so its predicate runs",
		Bundle: soOneFieldBundle("SoUnionInt", soUnionOf(soWith(intType(), soCheck("pos", "this > 0")), stringType())),
		Raw:    `{"v":7}`,
		Divergence: "native's production coerce DECLINES a constrained union arm with the " +
			"ErrDeBAMLParseUnsupported sentinel, so no state exists to compare — fail-closed by construction",
		Stock:  "value class:SoUnionInt{v=int:7} checks=[$.v|\"pos\"|this > 0=succeeded]",
		Native: "coercion-error debaml: constraint-state collector: $: production coercion did not succeed (the collector models a SUCCESSFUL canonical coercion only): bamlutils: de-BAML parser unsupported: class \"SoUnionInt\": a field could not be resolved to BAML's value (deferred lenient success/default/scoring)",
	},
	{
		Name: "union_constrained_arm_loses", Family: "union",
		Doc:    "the UNCONSTRAINED arm wins, so the losing arm's predicate never runs on either leg",
		Bundle: soOneFieldBundle("SoUnionStr", soUnionOf(soWith(intType(), soCheck("pos", "this > 0")), stringType())),
		Raw:    `{"v":"seven"}`,
		Divergence: "same decline as the winning-arm row: the constrained union is refused by production " +
			"coerce whichever arm wins",
		Stock:  "value class:SoUnionStr{v=string:\"seven\"} checks=[]",
		Native: "coercion-error debaml: constraint-state collector: $: production coercion did not succeed (the collector models a SUCCESSFUL canonical coercion only): bamlutils: de-BAML parser unsupported: class \"SoUnionStr\": a field could not be resolved to BAML's value (deferred lenient success/default/scoring)",
	},

	{
		Name: "union_target_level", Family: "union",
		Doc: "a constrained arm of a union RETURN TYPE — the one union position production coerce reaches, " +
			"so the winning-arm path is observable rather than declined at the class field",
		Bundle: soBundle(soUnionOf(soWith(intType(), soCheck("pos", "this > 0")), stringType()), nil, nil),
		Raw:    `7`,
		Divergence: "production coerce DECLINES a constrained union arm at the RETURN TYPE too, so no union " +
			"arm is coerced anywhere in the corpus — see TestServingOracleNoUnionArmIsCoerced",
		Stock:  "value int:7 checks=[$|\"pos\"|this > 0=succeeded~uncertified-order]",
		Native: "coercion-error debaml: constraint-state collector: $: production coercion did not succeed (the collector models a SUCCESSFUL canonical coercion only): bamlutils: de-BAML parser unsupported: union variant kind \"primitive\": not a scalar/literal/enum leaf, a required-flat-leaf class, a list, or a string-keyed map",
	},

	// -- target-level -------------------------------------------------------
	{
		Name: "target_int_check_fail", Family: "target",
		Doc:    "a @check on the RETURN TYPE itself: stock evaluates it and reports it in a root Checked",
		Bundle: soBundle(soWith(intType(), soCheck("gt", "this > 100")), nil, nil),
		Raw:    `5`,
		Stock:  "value int:5 checks=[$|\"gt\"|this > 100=failed~uncertified-order]",
		Native: "value int:5 events=[$|type_meta/check/\"gt\"/this > 100=false]",
	},
	{
		Name: "target_int_assert_fail", Family: "target",
		Doc:    "a failing @assert on the return type rejects the whole parse",
		Bundle: soBundle(soWith(intType(), soAssert("gt", "this > 100")), nil, nil),
		Raw:    `5`,
		Stock:  "assertion-failure Assertions failed. | Failed: gt this > 100",
		Native: "assertion-failure int:5 events=[$|type_meta/assert/\"gt\"/this > 100=false]",
	},
	{
		Name: "target_string_check_skipped", Family: "asymmetry",
		Doc: "ASYMMETRY 1: a BARE STRING return skips constraint evaluation entirely — stock emits the " +
			"value with an EMPTY check collection even though the predicate is false",
		Bundle: soBundle(soWith(stringType(), soCheck("eq", `this == "expected"`)), nil, nil),
		Raw:    `"actual"`,
		Divergence: "both legs SKIP the predicate (the asymmetry), and they canonicalize the quoted text " +
			"differently: a bare-string return makes stock take the assistant text VERBATIM, quotes and all, " +
			"while native extracts the JSON string it denotes",
		Stock:  "value string:\"\\\"actual\\\"\" checks=[]",
		Native: "value string:\"actual\" events=[] skipped=[$|type_meta/check/\"eq\"/this == \"expected\"~would-be-false]",
	},
	{
		Name: "target_string_assert_skipped", Family: "asymmetry",
		Doc: "ASYMMETRY 1 at @assert level: a false assertion on a bare-string return does NOT reject — " +
			"the value is served unchanged",
		Bundle: soBundle(soWith(stringType(), soAssert("eq", `this == "expected"`)), nil, nil),
		Raw:    `"actual"`,
		Divergence: "as the @check row: both legs skip the predicate, and the quoted assistant text is " +
			"canonicalized verbatim by stock and as a JSON string by native",
		Stock:  "value string:\"\\\"actual\\\"\" checks=[]",
		Native: "value string:\"actual\" events=[] skipped=[$|type_meta/assert/\"eq\"/this == \"expected\"~would-be-false]",
	},
	{
		Name: "target_string_bare_word", Family: "target",
		Doc: "the assistant text is a BARE WORD rather than JSON: stock recovers it as the string return, " +
			"native's extraction finds no cleanly-claimable candidate and declines before coercion",
		Bundle: soBundle(soWith(stringType(), soCheck("eq", `this == "expected"`)), nil, nil),
		Raw:    `actual`,
		Divergence: "native's extraction stage declines a bare word; stock's recovery is broader. The " +
			"decline is the serving path's own (no cleanly-claimable JSON candidate)",
		Stock:  "value string:\"actual\" checks=[]",
		Native: "no-candidate no cleanly-claimable JSON candidate",
	},
	{
		Name: "target_list_elem_check", Family: "target",
		Doc:    "a constrained ELEMENT of a target list — the target walk one structural step in",
		Bundle: soBundle(soListOf(soWith(intType(), soCheck("pos", "this > 0"))), nil, nil),
		Raw:    `[1,-2]`,
		Stock:  "value list[int:1,int:-2] checks=[$[0]|\"pos\"|this > 0=succeeded~uncertified-order $[1]|\"pos\"|this > 0=failed~uncertified-order]",
		Native: "value list[int:1,int:-2] events=[$[0]|type_meta/check/\"pos\"/this > 0=true $[1]|type_meta/check/\"pos\"/this > 0=false]",
	},

	// -- the duplicate-label asymmetry --------------------------------------
	{
		Name: "duplicate_label", Family: "asymmetry",
		Doc: "ASYMMETRY 2: two @check attributes under ONE label are two ordered results with different " +
			"outcomes; folding them by label would silently drop one",
		Bundle: soOneFieldBundle("SoDup", soWith(intType(),
			soCheck("dup", "this > 0"), soCheck("dup", "this > 100"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoDup{v=int:5} checks=[$.v|\"dup\"|this > 0=succeeded $.v|\"dup\"|this > 100=failed]",
		Native: "value class:SoDup{v=int:5} events=[$.v|type_meta/check/\"dup\"/this > 0=true $.v|type_meta/check/\"dup\"/this > 100=false]",
	},

	{
		Name: "duplicate_identical_nonadjacent", Family: "asymmetry",
		Doc: "the SAME check declared twice with a third between them: stock reports three entries, two of " +
			"which are byte-identical but NOT adjacent — the case that makes the repeat-collapse's " +
			"consecutive-only restriction load-bearing rather than decorative",
		Bundle: soOneFieldBundle("SoDupSame", soWith(intType(),
			soCheck("a", "this > 0"), soCheck("b", "this > 3"), soCheck("a", "this > 0"))),
		Raw:    `{"v":5}`,
		Stock:  "value class:SoDupSame{v=int:5} checks=[$.v|\"a\"|this > 0=succeeded $.v|\"b\"|this > 3=succeeded $.v|\"a\"|this > 0=succeeded]",
		Native: "value class:SoDupSame{v=int:5} events=[$.v|type_meta/check/\"a\"/this > 0=true $.v|type_meta/check/\"b\"/this > 3=true $.v|type_meta/check/\"a\"/this > 0=true]",
	},

	// -- evaluator errors ---------------------------------------------------
	{
		Name: "err_unknown_filter", Family: "error",
		Doc:        "an unknown filter is an EVALUATOR error: stock rejects the node rather than failing a check",
		Bundle:     soOneFieldBundle("SoErrFilter", soWith(intType(), soCheck("uf", "this|nosuchfilter"))),
		Raw:        `{"v":5}`,
		Divergence: "stock REJECTS the whole node when a predicate fails to evaluate, so it emits no value; native's coercion is constraint-blind and canonicalizes int:5 while declining the predicate itself",
		Stock:      "evaluator-error Failed while parsing required fields: missing=0, unparsed=1 | Failed to parse field v: v: Failed to evaluate constraints: unknown filter: filter nosuchfilter is unknown (in <string>:1) | Failed to evaluate constraints: unknown filter: filter nosuchfilter is unknown (in <string>:1)",
		Native:     "evaluator-unsupported class:SoErrFilter{v=int:5} events=[$.v|type_meta/check/\"uf\"/this|nosuchfilter=unsupported]",
	},
	{
		Name: "err_non_boolean", Family: "error",
		Doc:        "a predicate that renders a non-boolean is an evaluator error, NOT a failed check",
		Bundle:     soOneFieldBundle("SoErrNonBool", soWith(intType(), soCheck("nb", "this + 1"))),
		Raw:        `{"v":5}`,
		Divergence: "as the unknown-filter row: stock rejects the node and emits nothing, native canonicalizes the value and declines the predicate",
		Stock:      "evaluator-error Failed while parsing required fields: missing=0, unparsed=1 | Failed to parse field v: v: Failed to evaluate constraints: Predicate did not evaluate to a boolean | Failed to evaluate constraints: Predicate did not evaluate to a boolean",
		Native:     "evaluator-unsupported class:SoErrNonBool{v=int:5} events=[$.v|type_meta/check/\"nb\"/this + 1=unsupported]",
	},
	{
		Name: "err_unknown_method", Family: "error",
		Doc:        "an unknown method on the value is an evaluator error",
		Bundle:     soOneFieldBundle("SoErrMethod", soWith(stringType(), soCheck("um", "this.nosuchmethod()"))),
		Raw:        `{"v":"x"}`,
		Divergence: "as the unknown-filter row: stock rejects the node and emits nothing, native canonicalizes the value and declines the predicate",
		Stock:      "evaluator-error Failed while parsing required fields: missing=0, unparsed=1 | Failed to parse field v: v: Failed to evaluate constraints: unknown method: string has no method named nosuchmethod (in <string>:1) | Failed to evaluate constraints: unknown method: string has no method named nosuchmethod (in <string>:1)",
		Native:     "evaluator-unsupported class:SoErrMethod{v=string:\"x\"} events=[$.v|type_meta/check/\"um\"/this.nosuchmethod()=unsupported]",
	},
	{
		Name: "err_optional_swallows", Family: "error",
		Doc: "an OPTIONAL field whose predicate errors: the optional coercion swallows the failure and " +
			"yields null with NO check entry — neither a pass nor a fail",
		Bundle: soOneFieldBundle("SoErrOpt", soWith(soOptional(intType()), soCheck("uf", "this|nosuchfilter"))),
		Raw:    `{"v":5}`,
		Divergence: "stock's OPTIONAL coercion is constraint-aware — the evaluator failure nulls the field " +
			"and emits no check — while native's coercion is constraint-blind and keeps int:5. Native " +
			"decides nothing (the predicate is declined), and the bundle declines at the gate",
		Stock:  "value class:SoErrOpt{v=null} checks=[]",
		Native: "evaluator-unsupported class:SoErrOpt{v=int:5} events=[$.v|type_meta/check/\"uf\"/this|nosuchfilter=unsupported]",
	},

	// -- guard-ledger rows --------------------------------------------------
	{
		Name: "guard_int_above_2p53", Family: "guard",
		Doc: "the exact i64 a float64 core cannot tell from its neighbour: 2^53+1 must survive coercion " +
			"and be compared exactly",
		Bundle:     soOneFieldBundle("SoBigInt", soWith(intType(), soCheck("gt", "this > 9007199254740992"))),
		Raw:        `{"v":9007199254740993}`,
		Divergence: "native's numeric whitelist refuses a comparison against an integer literal above 2^53",
		Stock:      "value class:SoBigInt{v=int:9007199254740993} checks=[$.v|\"gt\"|this > 9007199254740992=succeeded]",
		Native:     "evaluator-unsupported class:SoBigInt{v=int:9007199254740993} events=[$.v|type_meta/check/\"gt\"/this > 9007199254740992=unsupported]",
	},
	{
		Name: "guard_float_2p63", Family: "guard",
		Doc:    "2^63 as a float — the AsInt hazard where Go's conversion is implementation-defined",
		Bundle: soOneFieldBundle("SoBigFloat", soWith(soFloatType(), soCheck("gt", "this > 1.0"))),
		Raw:    `{"v":9223372036854775808.0}`,
		Divergence: "the #662 collector REFUSES this row: its float leaf is re-serialized as the shortest " +
			"round-trip decimal (9223372036854776000) while production coerce keeps the source token " +
			"(9223372036854775808.0), and the exact big.Rat divergence check compares the two DECIMALS " +
			"rather than the float64 they both denote. A limitation of the witness, not of either engine",
		Stock:  "value class:SoBigFloat{v=float:9.223372036854776e+18} checks=[$.v|\"gt\"|this > 1.0=succeeded]",
		Native: "collector-diverged debaml: constraint-state traversal diverged from production coercion at $.v: $: number 9223372036854776000 vs 9223372036854775808.0 (state 9223372036854776000 vs production 9223372036854775808.0)",
	},
	{
		Name: "guard_string_number", Family: "guard",
		Doc:        "a NUMERIC STRING stays a string: the schema chooses the domain, not the token",
		Bundle:     soOneFieldBundle("SoStrNum", soWith(stringType(), soCheck("eq", `this == "9007199254740993"`))),
		Raw:        `{"v":"9007199254740993"}`,
		Divergence: "native's numeric whitelist refuses the predicate: the string literal carries a numeric run outside the proven range",
		Stock:      "value class:SoStrNum{v=string:\"9007199254740993\"} checks=[$.v|\"eq\"|this == \"9007199254740993\"=succeeded]",
		Native:     "evaluator-unsupported class:SoStrNum{v=string:\"9007199254740993\"} events=[$.v|type_meta/check/\"eq\"/this == \"9007199254740993\"=unsupported]",
	},
	{
		Name: "guard_arithmetic", Family: "guard",
		Doc:        "arithmetic inside a predicate, which the native operator gate treats as a proven shape",
		Bundle:     soOneFieldBundle("SoArith", soWith(intType(), soCheck("ar", "(this + 1) * 2 == 12"))),
		Raw:        `{"v":5}`,
		Divergence: "native's operator gate refuses arithmetic inside a predicate",
		Stock:      "value class:SoArith{v=int:5} checks=[$.v|\"ar\"|(this + 1) * 2 == 12=succeeded]",
		Native:     "evaluator-unsupported class:SoArith{v=int:5} events=[$.v|type_meta/check/\"ar\"/(this + 1) * 2 == 12=unsupported]",
	},
	{
		Name: "guard_length_filter", Family: "guard",
		Doc:    "the length filter over a unicode string",
		Bundle: soOneFieldBundle("SoLen", soWith(stringType(), soCheck("len", "this|length == 5"))),
		Raw:    `{"v":"héllo"}`,
		Stock:  "value class:SoLen{v=string:\"h\u00e9llo\"} checks=[$.v|\"len\"|this|length == 5=succeeded]",
		Native: "value class:SoLen{v=string:\"h\u00e9llo\"} events=[$.v|type_meta/check/\"len\"/this|length == 5=true]",
	},

	// -- native-decline controls -------------------------------------------
	{
		Name: "decline_pycompat_format", Family: "decline",
		Doc: "stock DECIDES a Python-style str.format predicate; native has no pycompat hook and must " +
			"refuse rather than answer",
		Bundle: soOneFieldBundle("SoPyFmt", soWith(stringType(),
			soCheck("fmt", `"{:,}".format(1234567) == "1,234,567"`))),
		Raw:        `{"v":"x"}`,
		Divergence: "pycompat str.format: minijinja-Go v2.16.0 has no unknown-method callback",
		Stock:      "value class:SoPyFmt{v=string:\"x\"} checks=[$.v|\"fmt\"|\"{:,}\".format(1234567) == \"1,234,567\"=succeeded]",
		Native:     "evaluator-unsupported class:SoPyFmt{v=string:\"x\"} events=[$.v|type_meta/check/\"fmt\"/\"{:,}\".format(1234567) == \"1,234,567\"=unsupported]",
	},
	{
		Name: "decline_regex_match", Family: "decline",
		Doc: "stock DECIDES a regex_match predicate; native withdrew regex_match (RE2 and Rust's regex " +
			"crate are different engines) and must refuse",
		Bundle: soOneFieldBundle("SoRegex", soWith(stringType(),
			soCheck("rx", `this|regex_match("abc")`))),
		Raw:        `{"v":"abcdef"}`,
		Divergence: "regex_match is withdrawn: Go RE2 vs Rust regex are not proven identical",
		Stock:      "value class:SoRegex{v=string:\"abcdef\"} checks=[$.v|\"rx\"|this|regex_match(\"abc\")=succeeded]",
		Native:     "evaluator-unsupported class:SoRegex{v=string:\"abcdef\"} events=[$.v|type_meta/check/\"rx\"/this|regex_match(\"abc\")=unsupported]",
	},
	{
		Name: "decline_divisible_by_zero", Family: "decline",
		Doc: "stock cannot be OBSERVED at all: BAML v0.223 evaluates the test in Rust and panics on the " +
			"CFFI callback thread, so the row is driven from an isolated subprocess and no boolean is fabricated",
		Bundle:     soOneFieldBundle("SoDivZero", soWith(intType(), soCheck("dz", "this is divisibleby(0)"))),
		Raw:        `{"v":4}`,
		Fatal:      true,
		Divergence: "divisibleby(0) aborts or hangs the stock process; native refuses structurally",
	},

	// -- serving-shaped extraction -----------------------------------------
	{
		Name: "serving_markdown_fence", Family: "class",
		Doc: "the assistant text is a fenced markdown block, so both legs exercise their EXTRACTION " +
			"stage before any coercion or constraint runs",
		Bundle: soOneFieldBundle("SoFenced", soWith(intType(), soCheck("gt", "this > 0"))),
		Raw:    "Here you go:\n```json\n{\"v\": 5}\n```\n",
		Stock:  "value class:SoFenced{v=int:5} checks=[$.v|\"gt\"|this > 0=succeeded]",
		Native: "value class:SoFenced{v=int:5} events=[$.v|type_meta/check/\"gt\"/this > 0=true]",
	},
	{
		Name: "serving_jsonish_comments", Family: "class",
		Doc:    "JSONish comments in the assistant text, stripped by both legs before extraction",
		Bundle: soOneFieldBundle("SoCommented", soWith(intType(), soCheck("gt", "this > 0"))),
		Raw:    "{\n  // the answer\n  \"v\": 5\n}",
		Stock:  "value class:SoCommented{v=int:5} checks=[$.v|\"gt\"|this > 0=succeeded]",
		Native: "value class:SoCommented{v=int:5} events=[$.v|type_meta/check/\"gt\"/this > 0=true]",
	},

	// -- unconstrained controls --------------------------------------------
	//
	// These carry NO constraint anywhere. The boundary lock requires them to be
	// ADMITTED, which is what makes every other row's decline constraint-specific
	// rather than a blanket refusal of the corpus shape.
	{
		Name: "control_class_unconstrained", Family: "control",
		Doc: "CONTROL: the class/field shape of the constrained rows, with the constraints removed",
		Bundle: soBundle(soClassType("SoCtlCls"),
			[]schema.ClassDef{soClassOf("SoCtlCls", []schema.ClassField{
				soField("b", intType()), soField("a", stringType()),
			})}, nil),
		Raw:           `{"a":"x","b":2}`,
		Unconstrained: true,
		Stock:         "value class:SoCtlCls{b=int:2,a=string:\"x\"} checks=[]",
		Native:        "value class:SoCtlCls{b=int:2,a=string:\"x\"} events=[]",
	},
	{
		Name: "control_list_unconstrained", Family: "control",
		Doc:           "CONTROL: the list shape without the element constraint",
		Bundle:        soOneFieldBundle("SoCtlList", soListOf(intType())),
		Raw:           `{"v":[1,2,3]}`,
		Unconstrained: true,
		Stock:         "value class:SoCtlList{v=list[int:1,int:2,int:3]} checks=[]",
		Native:        "value class:SoCtlList{v=list[int:1,int:2,int:3]} events=[]",
	},
	{
		Name: "control_map_unconstrained", Family: "control",
		Doc:           "CONTROL: the map shape without the value constraint",
		Bundle:        soOneFieldBundle("SoCtlMap", soMapOf(stringType(), intType())),
		Raw:           `{"v":{"b":1,"a":2}}`,
		Unconstrained: true,
		Stock:         "value class:SoCtlMap{v=map{b=int:1,a=int:2}} checks=[]",
		Native:        "value class:SoCtlMap{v=map{b=int:1,a=int:2}} events=[]",
	},
	{
		Name: "control_target_string", Family: "control",
		Doc:           "CONTROL: a bare-string return with no constraint — the shape the static corpus serves",
		Bundle:        soBundle(stringType(), nil, nil),
		Raw:           `actual`,
		Unconstrained: true,
		Divergence: "native's extraction declines a bare word, so this control proves ADMISSION rather " +
			"than serving; the serving direction is covered by the controls that do emit bytes",
		Stock:  "value string:\"actual\" checks=[]",
		Native: "no-candidate no cleanly-claimable JSON candidate",
	},
	{
		Name: "bare_string_quoted_text", Family: "control",
		Doc: "KNOWN GAP, and UNCONSTRAINED: a bare-string return given QUOTED assistant text. Stock takes " +
			"the text verbatim, quotes included; native's static serving path extracts the JSON string it " +
			"denotes and serves a different value. Nothing in this slice causes it and nothing here fixes " +
			"it — see TestServingOracleKnownGap_BareStringQuotedText",
		Bundle:        soBundle(stringType(), nil, nil),
		Raw:           `"actual"`,
		Unconstrained: true,
		Divergence: "stock serves the verbatim text (quotes included) where native serves the string it " +
			"denotes — a PRE-EXISTING divergence in the admitted bare-string-target lane, unrelated to " +
			"constraints",
		Stock:  "value string:\"\\\"actual\\\"\" checks=[]",
		Native: "value string:\"actual\" events=[]",
	},
}

// servingOracleProbes are project functions that are NOT corpus rows.
//
// A probe exists so a named test can drive a shape the differential must not
// contain. The root duplicate-label probe is the only one: its folded readback is
// deliberately lossy, so it cannot be compared like a fixture — but the loss has to
// be MEASURED against the real baml_go decode rather than described, which is what
// TestServingOracleRootCheckFoldIsLossy does with it.
var servingOracleProbes = []servingOracleProbe{
	{
		Name: "probe_root_duplicate_label",
		Doc: "two @check attributes under ONE label on the RETURN TYPE. Stock evaluates both; baml_go's " +
			"root readback folds them into one map entry. Driven only by TestServingOracleRootCheckFoldIsLossy",
		Bundle: soBundle(soWith(intType(),
			soCheck("dup", "this > 0"), soCheck("dup", "this > 100")), nil, nil),
		Raw: `5`,
	},
	{
		Name: "probe_nested_duplicate_label",
		Doc: "the SAME two checks one level down, where the raw CFFI tree keeps both — the control that " +
			"makes the root fold a measurable LOSS rather than an absence",
		Bundle: soOneFieldBundle("SoProbeNestedDup", soWith(intType(),
			soCheck("dup", "this > 0"), soCheck("dup", "this > 100"))),
		Raw: `{"v":5}`,
	},
}
