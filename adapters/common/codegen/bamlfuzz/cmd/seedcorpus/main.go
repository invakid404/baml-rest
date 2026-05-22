// seedcorpus generates the dynamic oracle seed corpus
// (testdata/bamlfuzz/dynamic/*.json). The corpus is hand-curated to cover
// every type-kind branch the dynamic emitter has to handle; each JSON
// file is an OracleCase that the integration test loads and replays.
//
// Re-run with:
//
//	cd adapters/common
//	GOWORK=off go run ./codegen/bamlfuzz/cmd/seedcorpus -out=./codegen/testdata/bamlfuzz/dynamic
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
)

func main() {
	outDir := flag.String("out", "", "output directory")
	flag.Parse()
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "missing -out")
		os.Exit(2)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	cases := []seedSpec{
		scalarString(),
		multiScalar(),
		listOfStrings(),
		mapOfInts(),
		optionalThreeShapes(),
		enumRef(),
		nestedClass(),
		classWithEnum(),
		optionalListOfClass(),
		mapToClass(),
		mutualRecursion(),
		literalsAll(),
	}

	for i, spec := range cases {
		ocase := materialize(i, spec)
		path := filepath.Join(*outDir, fmt.Sprintf("%02d_%s.json", i, ocase.Name))
		data, err := json.MarshalIndent(ocase, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal %s: %v\n", ocase.Name, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
	}
}

type seedSpec struct {
	Name     string
	Schema   bamlfuzz.FuzzSchema
	Value    bamlfuzz.FuzzValue
	Preserve bool
}

func materialize(idx int, spec seedSpec) bamlfuzz.OracleCase {
	schema := bamlfuzz.AnalyzeGraph(spec.Schema)
	walk, err := bamlfuzz.Walk(schema, spec.Value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", spec.Name, err)
		os.Exit(1)
	}
	return bamlfuzz.OracleCase{
		Name:                spec.Name,
		Seed:                int64(idx + 1),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleDynamicThreeWay,
		PreserveSchemaOrder: spec.Preserve,
		Schema:              schema,
		Value:               spec.Value,
		MockLLMContent:      walk.MockLLMContent,
		Expected:            walk.Expected,
		Metadata:            walk.Metadata,
	}
}

func cls(name string, props ...bamlfuzz.FuzzProperty) bamlfuzz.FuzzClass {
	return bamlfuzz.FuzzClass{Name: name, Properties: props}
}

func prop(name string, t bamlfuzz.FuzzType) bamlfuzz.FuzzProperty {
	return bamlfuzz.FuzzProperty{Name: name, Type: t}
}

func tStr() bamlfuzz.FuzzType   { return bamlfuzz.FuzzType{Kind: bamlfuzz.KindString} }
func tInt() bamlfuzz.FuzzType   { return bamlfuzz.FuzzType{Kind: bamlfuzz.KindInt} }
func tFloat() bamlfuzz.FuzzType { return bamlfuzz.FuzzType{Kind: bamlfuzz.KindFloat} }
func tBool() bamlfuzz.FuzzType  { return bamlfuzz.FuzzType{Kind: bamlfuzz.KindBool} }
func tNull() bamlfuzz.FuzzType  { return bamlfuzz.FuzzType{Kind: bamlfuzz.KindNull} }

func tOpt(inner bamlfuzz.FuzzType) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindOptional, Inner: &inner}
}
func tList(inner bamlfuzz.FuzzType) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindList, Inner: &inner}
}
func tMap(inner bamlfuzz.FuzzType) bamlfuzz.FuzzType {
	key := tStr()
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindMap, Key: &key, Inner: &inner}
}
func tClassRef(name string) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindClassRef, Ref: name}
}
func tEnumRef(name string) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindEnumRef, Ref: name}
}
func tLitStr(s string) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindLiteral, Literal: &bamlfuzz.FuzzLiteral{Kind: bamlfuzz.LiteralString, String: s}}
}
func tLitInt(n int64) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindLiteral, Literal: &bamlfuzz.FuzzLiteral{Kind: bamlfuzz.LiteralInt, Int: n}}
}
func tLitBool(b bool) bamlfuzz.FuzzType {
	return bamlfuzz.FuzzType{Kind: bamlfuzz.KindLiteral, Literal: &bamlfuzz.FuzzLiteral{Kind: bamlfuzz.LiteralBool, Bool: b}}
}

// vClass constructs a class-instance value. The walker iterates the
// schema's property declaration order; the value's Fields slice must
// supply one entry per property by name.
func vClass(name string, fields ...bamlfuzz.FuzzFieldValue) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindClassRef, ClassName: name, Fields: fields}
}
func vField(name string, v bamlfuzz.FuzzValue) bamlfuzz.FuzzFieldValue {
	return bamlfuzz.FuzzFieldValue{Name: name, Value: v}
}

func vStr(s string) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindString, String: s}
}
func vInt(n int64) bamlfuzz.FuzzValue { return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindInt, Int: n} }
func vFloat(f float64) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindFloat, Float: f}
}
func vBool(b bool) bamlfuzz.FuzzValue { return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindBool, Bool: b} }
func vNull() bamlfuzz.FuzzValue       { return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindNull} }

func vOptPresent(inner bamlfuzz.FuzzValue) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindOptional, OptionalShape: bamlfuzz.OptionalPresent, Inner: &inner}
}
func vOptNull() bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindOptional, OptionalShape: bamlfuzz.OptionalNull}
}
func vOptAbsent() bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindOptional, OptionalShape: bamlfuzz.OptionalAbsent}
}
func vList(items ...bamlfuzz.FuzzValue) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindList, Items: items}
}
func vMap(entries ...bamlfuzz.FuzzMapEntry) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindMap, MapEntries: entries}
}
func mapEntry(k string, v bamlfuzz.FuzzValue) bamlfuzz.FuzzMapEntry {
	return bamlfuzz.FuzzMapEntry{Key: k, Value: v}
}
func vEnum(name string) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindEnumRef, Enum: name}
}
func vLitStr(s string) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindLiteral, String: s}
}
func vLitInt(n int64) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindLiteral, Int: n}
}
func vLitBool(b bool) bamlfuzz.FuzzValue {
	return bamlfuzz.FuzzValue{Kind: bamlfuzz.KindLiteral, Bool: b}
}

func scalarString() seedSpec {
	return seedSpec{
		Name: "scalar_string",
		Schema: bamlfuzz.FuzzSchema{
			Classes:   []bamlfuzz.FuzzClass{cls("Root", prop("name", tStr()))},
			RootClass: "Root",
		},
		Value:    vClass("Root", vField("name", vStr("hello"))),
		Preserve: false,
	}
}

func multiScalar() seedSpec {
	return seedSpec{
		Name: "multi_scalar",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{cls("Root",
				prop("name", tStr()),
				prop("age", tInt()),
				prop("score", tFloat()),
				prop("active", tBool()),
				prop("nothing", tNull()),
			)},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("name", vStr("Ada")),
			vField("age", vInt(36)),
			vField("score", vFloat(99.5)),
			vField("active", vBool(true)),
			vField("nothing", vNull()),
		),
		Preserve: true,
	}
}

func listOfStrings() seedSpec {
	return seedSpec{
		Name: "list_of_strings",
		Schema: bamlfuzz.FuzzSchema{
			Classes:   []bamlfuzz.FuzzClass{cls("Root", prop("tags", tList(tStr())))},
			RootClass: "Root",
		},
		Value:    vClass("Root", vField("tags", vList(vStr("alpha"), vStr("beta"), vStr("gamma")))),
		Preserve: false,
	}
}

func mapOfInts() seedSpec {
	return seedSpec{
		Name: "map_of_ints",
		Schema: bamlfuzz.FuzzSchema{
			Classes:   []bamlfuzz.FuzzClass{cls("Root", prop("counts", tMap(tInt())))},
			RootClass: "Root",
		},
		Value: vClass("Root", vField("counts", vMap(
			mapEntry("a", vInt(1)),
			mapEntry("b", vInt(2)),
			mapEntry("c", vInt(3)),
		))),
		Preserve: true,
	}
}

func optionalThreeShapes() seedSpec {
	return seedSpec{
		Name: "optional_three_shapes",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{cls("Root",
				prop("present_field", tOpt(tStr())),
				prop("null_field", tOpt(tStr())),
				prop("absent_field", tOpt(tStr())),
			)},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("present_field", vOptPresent(vStr("here"))),
			vField("null_field", vOptNull()),
			vField("absent_field", vOptAbsent()),
		),
		Preserve: true,
	}
}

func enumRef() seedSpec {
	return seedSpec{
		Name: "enum_ref",
		Schema: bamlfuzz.FuzzSchema{
			Classes:   []bamlfuzz.FuzzClass{cls("Root", prop("status", tEnumRef("Status")))},
			Enums:     []bamlfuzz.FuzzEnum{{Name: "Status", Values: []string{"ACTIVE", "INACTIVE", "PENDING"}}},
			RootClass: "Root",
		},
		Value:    vClass("Root", vField("status", vEnum("ACTIVE"))),
		Preserve: true,
	}
}

func nestedClass() seedSpec {
	return seedSpec{
		Name: "nested_class",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{
				cls("Root",
					prop("name", tStr()),
					prop("inner", tClassRef("Inner")),
				),
				cls("Inner",
					prop("label", tStr()),
					prop("count", tInt()),
				),
			},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("name", vStr("outer")),
			vField("inner", vClass("Inner",
				vField("label", vStr("inside")),
				vField("count", vInt(7)),
			)),
		),
		Preserve: true,
	}
}

func classWithEnum() seedSpec {
	return seedSpec{
		Name: "class_with_enum",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{
				cls("Root",
					prop("title", tStr()),
					prop("priority", tEnumRef("Priority")),
				),
			},
			Enums:     []bamlfuzz.FuzzEnum{{Name: "Priority", Values: []string{"HIGH", "MEDIUM", "LOW"}}},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("title", vStr("ship it")),
			vField("priority", vEnum("HIGH")),
		),
		Preserve: true,
	}
}

func optionalListOfClass() seedSpec {
	return seedSpec{
		Name: "optional_list_of_class",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{
				cls("Root",
					prop("items", tOpt(tList(tClassRef("Item")))),
				),
				cls("Item",
					prop("sku", tStr()),
				),
			},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("items", vOptPresent(vList(
				vClass("Item", vField("sku", vStr("ABC"))),
				vClass("Item", vField("sku", vStr("DEF"))),
			))),
		),
		Preserve: false,
	}
}

func mapToClass() seedSpec {
	return seedSpec{
		Name: "map_to_class",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{
				cls("Root",
					prop("by_id", tMap(tClassRef("Item"))),
				),
				cls("Item",
					prop("label", tStr()),
				),
			},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("by_id", vMap(
				mapEntry("a", vClass("Item", vField("label", vStr("apple")))),
				mapEntry("b", vClass("Item", vField("label", vStr("banana")))),
			)),
		),
		Preserve: true,
	}
}

// mutualRecursion exercises a two-class A↔B cycle realized through
// optional back-edges. The value walks one step: A.b carries a present
// nested B, and B.a is OptionalAbsent so the cycle terminates after a
// single hop while still exercising the back-edge schema shape.
func mutualRecursion() seedSpec {
	return seedSpec{
		Name: "mutual_recursion",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{
				cls("A",
					prop("name", tStr()),
					prop("b", tOpt(tClassRef("B"))),
				),
				cls("B",
					prop("kind", tStr()),
					prop("a", tOpt(tClassRef("A"))),
				),
			},
			RootClass: "A",
		},
		Value: vClass("A",
			vField("name", vStr("root-a")),
			vField("b", vOptPresent(vClass("B",
				vField("kind", vStr("nested-b")),
				vField("a", vOptAbsent()),
			))),
		),
		Preserve: false,
	}
}

func literalsAll() seedSpec {
	return seedSpec{
		Name: "literals_all",
		Schema: bamlfuzz.FuzzSchema{
			Classes: []bamlfuzz.FuzzClass{cls("Root",
				prop("kind", tLitStr("widget")),
				prop("count", tLitInt(42)),
				prop("active", tLitBool(true)),
			)},
			RootClass: "Root",
		},
		Value: vClass("Root",
			vField("kind", vLitStr("widget")),
			vField("count", vLitInt(42)),
			vField("active", vLitBool(true)),
		),
		Preserve: true,
	}
}
