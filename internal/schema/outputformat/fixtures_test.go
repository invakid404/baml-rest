package outputformat

import "github.com/invakid404/baml-rest/internal/schema"

// This file builds the golden-parity fixture corpus. Every `want` string is
// BAML ground truth: it is copied byte-for-byte from the corresponding
// passing render test in BAML engine
// engine/baml-lib/jinja-runtime/src/output_format/types.rs (commit
// e803afa246837a6bd38a15beb4aa403f7e3970e5), or — for the few constructs BAML
// has no upstream render test for (bare bool/null primitives, literal
// targets) — derived directly from the cited Display impls. The renderer must
// reproduce each want exactly.

// goldenCase is one fixture: a freshly-built bundle, render options, and the
// exact BAML output expected. bundle is a constructor so each test (and the
// JSON round-trip corpus) gets an independent value.
type goldenCase struct {
	name   string
	bundle func() *schema.Bundle
	opts   Options
	want   string
}

// --- small constructors mirroring TypeIR builders ---------------------------

func sp(s string) *string { return &s }

func pString() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
}
func pInt() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
}
func pFloat() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat}
}
func pBool() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool}
}
func pNull() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveNull}
}

func litStr(s string) schema.Type {
	return schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralString, String: s}}
}
func litInt(n int64) schema.Type {
	return schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralInt, Int: n}}
}
func litBool(b bool) schema.Type {
	return schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralBool, Bool: b}}
}

func classRef(name string) schema.Type {
	return schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming}
}
func enumRef(name string) schema.Type {
	return schema.Type{Kind: schema.TypeEnum, Name: name}
}
func aliasRef(name string) schema.Type {
	return schema.Type{Kind: schema.TypeRecursiveAlias, Name: name, Mode: schema.NonStreaming}
}
func list(elem schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeList, Elem: &elem}
}
func mapT(k, v schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeMap, Key: &k, Value: &v}
}

// optional(t) is BAML TypeIR::optional(t): a single-variant nullable union.
func optional(t schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{t}, Nullable: true}}
}

// oneOf is a non-nullable union of >= 2 variants.
func oneOf(variants ...schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: variants}}
}

// oneOfNull is a union of non-null variants plus null.
func oneOfNull(variants ...schema.Type) schema.Type {
	return schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: variants, Nullable: true}}
}

func field(name string, t schema.Type) schema.ClassField {
	return schema.ClassField{Name: schema.Name{Name: name}, Type: t}
}
func fieldDesc(name string, t schema.Type, desc string) schema.ClassField {
	return schema.ClassField{Name: schema.Name{Name: name}, Type: t, Description: sp(desc)}
}

func class(name string, fields ...schema.ClassField) schema.ClassDef {
	return schema.ClassDef{Name: schema.Name{Name: name}, Mode: schema.NonStreaming, Fields: fields}
}
func classDescribed(name, desc string, fields ...schema.ClassField) schema.ClassDef {
	return schema.ClassDef{Name: schema.Name{Name: name}, Description: sp(desc), Mode: schema.NonStreaming, Fields: fields}
}

func enumVal(name string) schema.EnumValue {
	return schema.EnumValue{Name: schema.Name{Name: name}}
}
func enumValDesc(name, desc string) schema.EnumValue {
	return schema.EnumValue{Name: schema.Name{Name: name}, Description: sp(desc)}
}
func enumValAliasDesc(name, alias, desc string) schema.EnumValue {
	return schema.EnumValue{Name: schema.Name{Name: name, Alias: sp(alias)}, Description: sp(desc)}
}

func enumDef(name string, values ...schema.EnumValue) schema.EnumDef {
	return schema.EnumDef{Name: schema.Name{Name: name}, Values: values}
}
func enumDefAlias(name, alias string, values ...schema.EnumValue) schema.EnumDef {
	return schema.EnumDef{Name: schema.Name{Name: name, Alias: sp(alias)}, Values: values}
}

// bundle assembles a bundle from a target and definition slices.
func mkBundle(target schema.Type, enums []schema.EnumDef, classes []schema.ClassDef, recursive []string, aliases []schema.RecursiveAliasDef) func() *schema.Bundle {
	return func() *schema.Bundle {
		return &schema.Bundle{
			Target:                     target,
			Enums:                      enums,
			Classes:                    classes,
			RecursiveClasses:           recursive,
			StructuralRecursiveAliases: aliases,
		}
	}
}

// goldenCases returns the full fixture corpus.
func goldenCases() []goldenCase {
	return []goldenCase{
		// --- top-level primitives -------------------------------------------
		{
			name:   "primitive_string_empty",
			bundle: mkBundle(pString(), nil, nil, nil, nil),
			want:   "",
		},
		{
			name:   "primitive_int",
			bundle: mkBundle(pInt(), nil, nil, nil, nil),
			want:   "Answer as an int",
		},
		{
			name:   "primitive_float",
			bundle: mkBundle(pFloat(), nil, nil, nil, nil),
			want:   "Answer as a float",
		},
		{
			name:   "primitive_bool",
			bundle: mkBundle(pBool(), nil, nil, nil, nil),
			want:   "Answer as a bool",
		},
		{
			name:   "primitive_null",
			bundle: mkBundle(pNull(), nil, nil, nil, nil),
			want:   "Answer as a null",
		},
		{
			name:   "list_of_string",
			bundle: mkBundle(list(pString()), nil, nil, nil, nil),
			want:   "Answer with a JSON Array using this schema:\nstring[]",
		},

		// --- literals (LiteralValue::Display) -------------------------------
		{
			name:   "literal_string",
			bundle: mkBundle(litStr("foo"), nil, nil, nil, nil),
			want:   "Answer using this specific value:\n\"foo\"",
		},
		{
			name:   "literal_int",
			bundle: mkBundle(litInt(42), nil, nil, nil, nil),
			want:   "Answer using this specific value:\n42",
		},
		{
			name:   "literal_bool",
			bundle: mkBundle(litBool(true), nil, nil, nil, nil),
			want:   "Answer using this specific value:\ntrue",
		},

		// --- enums ----------------------------------------------------------
		{
			name: "enum_inline_target",
			bundle: mkBundle(enumRef("Color"),
				[]schema.EnumDef{enumDef("Color", enumVal("Red"), enumVal("Green"), enumVal("Blue"))},
				nil, nil, nil),
			want: "Answer with any of the categories:\nColor\n----\n- Red\n- Green\n- Blue",
		},
		{
			name: "enum_hoist_more_than_max",
			bundle: mkBundle(classRef("Output"),
				[]schema.EnumDef{enumDef("Enm", enumVal("A"), enumVal("B"), enumVal("C"), enumVal("D"), enumVal("E"), enumVal("F"), enumVal("G"))},
				[]schema.ClassDef{class("Output", field("output", enumRef("Enm")))},
				nil, nil),
			want: "Enm\n----\n- A\n- B\n- C\n- D\n- E\n- F\n- G\n\nAnswer in JSON using this schema:\n{\n  output: Enm,\n}",
		},
		{
			name: "enum_hoist_variant_has_description",
			bundle: mkBundle(classRef("Output"),
				[]schema.EnumDef{enumDef("Enm", enumValDesc("A", "A description"), enumVal("B"), enumVal("C"), enumVal("D"), enumVal("E"), enumVal("F"))},
				[]schema.ClassDef{class("Output", field("output", enumRef("Enm")))},
				nil, nil),
			want: "Enm\n----\n- A: A description\n- B\n- C\n- D\n- E\n- F\n\nAnswer in JSON using this schema:\n{\n  output: Enm,\n}",
		},
		{
			name: "enum_always_hoist_option",
			bundle: mkBundle(classRef("Output"),
				[]schema.EnumDef{enumDef("Enm", enumVal("A"), enumVal("B"), enumVal("C"), enumVal("D"), enumVal("E"), enumVal("F"))},
				[]schema.ClassDef{class("Output", field("output", enumRef("Enm")))},
				nil, nil),
			opts: Options{AlwaysHoistEnums: AlwaysBool(true)},
			want: "Enm\n----\n- A\n- B\n- C\n- D\n- E\n- F\n\nAnswer in JSON using this schema:\n{\n  output: Enm,\n}",
		},
		{
			name: "enum_descriptions_null_prefix",
			bundle: mkBundle(enumRef("EnumOutput"),
				[]schema.EnumDef{enumDef("EnumOutput",
					enumValDesc("ONE", "The first enum."),
					enumValAliasDesc("TWO", "two", "The second enum."),
					enumValAliasDesc("THREE", "hi", "three"))},
				nil, nil, nil),
			opts: Options{Prefix: NeverString()},
			want: "EnumOutput\n----\n- ONE: The first enum.\n- two: The second enum.\n- hi: three",
		},
		{
			name: "enum_descriptions_default_prefix",
			bundle: mkBundle(enumRef("EnumOutput"),
				[]schema.EnumDef{enumDefAlias("EnumOutput", "VALUE_ENUM",
					enumValDesc("ONE", "The first enum."),
					enumValAliasDesc("TWO", "two", "The second enum."),
					enumValAliasDesc("THREE", "hi", "three"))},
				nil, nil, nil),
			want: "Answer with any of the categories:\nVALUE_ENUM\n----\n- ONE: The first enum.\n- two: The second enum.\n- hi: three",
		},

		// --- classes --------------------------------------------------------
		{
			name: "class_with_field_descriptions",
			bundle: mkBundle(classRef("Person"), nil,
				[]schema.ClassDef{class("Person",
					fieldDesc("name", pString(), "The person's name"),
					fieldDesc("age", pInt(), "The person's age"))},
				nil, nil),
			want: "Answer in JSON using this schema:\n{\n  // The person's name\n  name: string,\n  // The person's age\n  age: int,\n}",
		},
		{
			name: "class_with_block_description",
			bundle: mkBundle(classRef("User"), nil,
				[]schema.ClassDef{classDescribed("User", "Represents a system user", field("name", pString()))},
				nil, nil),
			want: "Answer in JSON using this schema:\n{\n  // Represents a system user\n\n  name: string,\n}",
		},
		{
			name: "class_with_multiline_block_description",
			bundle: mkBundle(classRef("Resume"), nil,
				[]schema.ClassDef{classDescribed("Resume", "A professional resume\ncontaining work history\nand qualifications", field("name", pString()))},
				nil, nil),
			want: "Answer in JSON using this schema:\n{\n  // A professional resume\n  // containing work history\n  // and qualifications\n\n  name: string,\n}",
		},
		{
			name: "class_with_multiline_field_descriptions",
			bundle: mkBundle(classRef("Education"), nil,
				[]schema.ClassDef{class("Education",
					fieldDesc("school", optional(pString()), "111\n  "),
					fieldDesc("degree", pString(), "2222222"),
					field("year", pInt()))},
				nil, nil),
			want: "Answer in JSON using this schema:\n{\n  // 111\n  //   \n  school: string or null,\n  // 2222222\n  degree: string,\n  year: int,\n}",
		},
		{
			name: "class_with_quoted_fields",
			bundle: mkBundle(classRef("Person"), nil,
				[]schema.ClassDef{class("Person",
					fieldDesc("name", pString(), "The person's name"),
					fieldDesc("age", pInt(), "The person's age"))},
				nil, nil),
			opts: Options{QuoteClassFields: true},
			want: "Answer in JSON using this schema:\n{\n  // The person's name\n  \"name\": string,\n  // The person's age\n  \"age\": int,\n}",
		},

		// --- unions ---------------------------------------------------------
		{
			name: "top_level_union",
			bundle: mkBundle(oneOf(classRef("Bug"), classRef("Enhancement"), classRef("Documentation")), nil,
				[]schema.ClassDef{
					class("Bug", field("description", pString()), field("severity", pString())),
					class("Enhancement", field("title", pString()), field("description", pString())),
					class("Documentation", field("module", pString()), field("format", pString())),
				}, nil, nil),
			want: "Answer in JSON using any of these schemas:\n{\n  description: string,\n  severity: string,\n} or {\n  title: string,\n  description: string,\n} or {\n  module: string,\n  format: string,\n}",
		},
		{
			name: "nested_union",
			bundle: mkBundle(classRef("Issue"), nil,
				[]schema.ClassDef{
					class("Issue",
						field("category", oneOf(classRef("Bug"), classRef("Enhancement"), classRef("Documentation"))),
						field("date", pString())),
					class("Bug", field("description", pString()), field("severity", pString())),
					class("Enhancement", field("title", pString()), field("description", pString())),
					class("Documentation", field("module", pString()), field("format", pString())),
				}, nil, nil),
			want: "Answer in JSON using this schema:\n{\n  category: {\n    description: string,\n    severity: string,\n  } or {\n    title: string,\n    description: string,\n  } or {\n    module: string,\n    format: string,\n  },\n  date: string,\n}",
		},
		{
			name:   "null_only_union",
			bundle: mkBundle(oneOfNull(), nil, nil, nil, nil),
			want:   "Answer ONLY with null:\nnull",
		},
		{
			name:   "optional_of_one",
			bundle: mkBundle(optional(pInt()), nil, nil, nil, nil),
			want:   "Answer in JSON using this schema:\nint or null",
		},
		{
			// or_splitter joins inline union/enum alternatives. An explicitly
			// empty splitter (BAML Some("")) joins with nothing — distinct from
			// the unset zero value, which is the default " or ".
			name:   "or_splitter_empty",
			bundle: mkBundle(oneOf(pInt(), pString(), pBool()), nil, nil, nil, nil),
			opts:   Options{OrSplitter: SetOrSplitter("")},
			want:   "Answer in JSON using any of these schemas:\nintstringbool",
		},
		{
			name:   "or_splitter_custom",
			bundle: mkBundle(oneOf(pInt(), pString(), pBool()), nil, nil, nil, nil),
			opts:   Options{OrSplitter: SetOrSplitter(" | ")},
			want:   "Answer in JSON using any of these schemas:\nint | string | bool",
		},

		// --- maps -----------------------------------------------------------
		{
			name:   "map_string_string",
			bundle: mkBundle(mapT(pString(), pString()), nil, nil, nil, nil),
			want:   "Answer in JSON using this schema:\nmap<string, string>",
		},
		{
			name:   "map_object_style",
			bundle: mkBundle(mapT(pString(), pString()), nil, nil, nil, nil),
			opts:   Options{MapStyle: MapStyleObject},
			want:   "Answer in JSON using this schema:\n{string: string}",
		},
		{
			name: "map_enum_key_class_value",
			bundle: mkBundle(mapT(enumRef("Key"), classRef("Val")),
				[]schema.EnumDef{enumDef("Key", enumVal("A"), enumVal("B"))},
				[]schema.ClassDef{class("Val", field("x", pInt()))},
				nil, nil),
			want: "Answer in JSON using this schema:\nmap<'A' or 'B', {\n  x: int,\n}>",
		},

		// --- lists ----------------------------------------------------------
		{
			name: "list_of_inline_class",
			bundle: mkBundle(list(classRef("Foo")), nil,
				[]schema.ClassDef{class("Foo", field("a", pString()))},
				nil, nil),
			want: "Answer with a JSON Array using this schema:\n[\n  {\n    a: string,\n  }\n]",
		},

		// --- recursive classes ----------------------------------------------
		{
			name: "hoisted_class_with_description",
			bundle: mkBundle(classRef("Node"), nil,
				[]schema.ClassDef{classDescribed("Node", "A node in a linked list",
					field("value", pInt()), field("next", optional(classRef("Node"))))},
				[]string{"Node"}, nil),
			want: "Node {\n  // A node in a linked list\n\n  value: int,\n  next: Node or null,\n}\n\nAnswer in JSON using this schema: Node",
		},
		{
			name: "top_level_simple_recursive_class",
			bundle: mkBundle(classRef("Node"), nil,
				[]schema.ClassDef{class("Node", field("data", pInt()), field("next", optional(classRef("Node"))))},
				[]string{"Node"}, nil),
			want: "Node {\n  data: int,\n  next: Node or null,\n}\n\nAnswer in JSON using this schema: Node",
		},
		{
			name: "nested_simple_recursive_class",
			bundle: mkBundle(classRef("LinkedList"), nil,
				[]schema.ClassDef{
					class("Node", field("data", pInt()), field("next", optional(classRef("Node")))),
					class("LinkedList", field("head", optional(classRef("Node"))), field("len", pInt())),
				},
				[]string{"Node"}, nil),
			want: "Node {\n  data: int,\n  next: Node or null,\n}\n\nAnswer in JSON using this schema:\n{\n  head: Node or null,\n  len: int,\n}",
		},
		{
			name: "top_level_recursive_cycle",
			bundle: mkBundle(classRef("A"), nil,
				[]schema.ClassDef{
					class("A", field("pointer", classRef("B"))),
					class("B", field("pointer", classRef("C"))),
					class("C", field("pointer", optional(classRef("A")))),
				},
				[]string{"A", "B", "C"}, nil),
			want: "A {\n  pointer: B,\n}\n\nB {\n  pointer: C,\n}\n\nC {\n  pointer: A or null,\n}\n\nAnswer in JSON using this schema: A",
		},
		{
			name: "mutually_recursive_list",
			bundle: mkBundle(classRef("Tree"), nil,
				[]schema.ClassDef{
					class("Tree", field("data", pInt()), field("children", classRef("Forest"))),
					class("Forest", field("trees", list(classRef("Tree")))),
				},
				[]string{"Tree", "Forest"}, nil),
			want: "Tree {\n  data: int,\n  children: Forest,\n}\n\nForest {\n  trees: Tree[],\n}\n\nAnswer in JSON using this schema: Tree",
		},
		{
			name: "self_referential_union",
			bundle: mkBundle(classRef("SelfReferential"), nil,
				[]schema.ClassDef{class("SelfReferential",
					field("recursion", oneOfNull(pInt(), pString(), classRef("SelfReferential"))))},
				[]string{"SelfReferential"}, nil),
			want: "SelfReferential {\n  recursion: int or string or SelfReferential or null,\n}\n\nAnswer in JSON using this schema: SelfReferential",
		},
		{
			name: "top_level_list_with_recursive_items",
			bundle: mkBundle(list(classRef("Node")), nil,
				[]schema.ClassDef{class("Node", field("data", pInt()), field("next", optional(classRef("Node"))))},
				[]string{"Node"}, nil),
			want: "Node {\n  data: int,\n  next: Node or null,\n}\n\nAnswer with a JSON Array using this schema:\nNode[]",
		},
		{
			name: "top_level_class_with_self_referential_map",
			bundle: mkBundle(classRef("RecursiveMap"), nil,
				[]schema.ClassDef{class("RecursiveMap", field("data", mapT(pString(), classRef("RecursiveMap"))))},
				[]string{"RecursiveMap"}, nil),
			want: "RecursiveMap {\n  data: map<string, RecursiveMap>,\n}\n\nAnswer in JSON using this schema: RecursiveMap",
		},
		{
			name: "top_level_map_pointing_to_recursive_class",
			bundle: mkBundle(mapT(pString(), classRef("Node")), nil,
				[]schema.ClassDef{class("Node", field("data", pInt()), field("next", optional(classRef("Node"))))},
				[]string{"Node"}, nil),
			want: "Node {\n  data: int,\n  next: Node or null,\n}\n\nAnswer in JSON using this schema:\nmap<string, Node>",
		},
		{
			name: "top_level_map_pointing_to_recursive_union",
			bundle: mkBundle(mapT(pString(), oneOf(classRef("Node"), pInt(), classRef("NonRecursive"))), nil,
				[]schema.ClassDef{
					class("Node", field("data", pInt()), field("next", optional(classRef("Node")))),
					class("NonRecursive", field("field", pString()), field("data", pInt())),
				},
				[]string{"Node"}, nil),
			want: "Node {\n  data: int,\n  next: Node or null,\n}\n\nAnswer in JSON using this schema:\nmap<string, Node or int or {\n  field: string,\n  data: int,\n}>",
		},

		// --- recursive aliases ----------------------------------------------
		{
			name: "simple_recursive_alias",
			bundle: mkBundle(aliasRef("RecursiveMapAlias"), nil, nil, nil,
				[]schema.RecursiveAliasDef{{Name: "RecursiveMapAlias", Target: mapT(pString(), aliasRef("RecursiveMapAlias"))}}),
			want: "RecursiveMapAlias = map<string, RecursiveMapAlias>\n\nAnswer in JSON using this schema: RecursiveMapAlias",
		},
		{
			name: "recursive_alias_cycle",
			bundle: mkBundle(aliasRef("A"), nil, nil, nil,
				[]schema.RecursiveAliasDef{
					{Name: "A", Target: aliasRef("B")},
					{Name: "B", Target: aliasRef("C")},
					{Name: "C", Target: list(aliasRef("A"))},
				}),
			want: "A = B\nB = C\nC = A[]\n\nAnswer in JSON using this schema: A",
		},
		{
			name: "recursive_alias_cycle_with_hoist_prefix",
			bundle: mkBundle(aliasRef("A"), nil, nil, nil,
				[]schema.RecursiveAliasDef{
					{Name: "A", Target: aliasRef("B")},
					{Name: "B", Target: aliasRef("C")},
					{Name: "C", Target: list(aliasRef("A"))},
				}),
			opts: Options{HoistedClassPrefix: AlwaysString("type")},
			want: "type A = B\ntype B = C\ntype C = A[]\n\nAnswer in JSON using this type: A",
		},

		// --- hoist options --------------------------------------------------
		{
			name: "hoisted_classes_with_prefix",
			bundle: mkBundle(classRef("NonRecursive"), nil,
				[]schema.ClassDef{
					class("A", field("pointer", classRef("B"))),
					class("B", field("pointer", classRef("C"))),
					class("C", field("pointer", optional(classRef("A")))),
					class("NonRecursive", field("pointer", classRef("A")), field("data", pInt()), field("field", pBool())),
				},
				[]string{"A", "B", "C"}, nil),
			opts: Options{HoistedClassPrefix: AlwaysString("interface")},
			want: "interface A {\n  pointer: B,\n}\n\ninterface B {\n  pointer: C,\n}\n\ninterface C {\n  pointer: A or null,\n}\n\nAnswer in JSON using this interface:\n{\n  pointer: A,\n  data: int,\n  field: bool,\n}",
		},
		{
			name: "hoist_classes_subset",
			bundle: mkBundle(classRef("Ret"), nil,
				[]schema.ClassDef{
					class("A", field("prop", pInt())),
					class("B", field("prop", pString())),
					class("C", field("prop", pFloat())),
					class("Ret", field("a", classRef("A")), field("b", classRef("B")), field("c", classRef("C"))),
				}, nil, nil),
			opts: Options{HoistClasses: HoistClasses{Mode: HoistSubset, Subset: []string{"A", "B"}}},
			want: "A {\n  prop: int,\n}\n\nB {\n  prop: string,\n}\n\nAnswer in JSON using this schema:\n{\n  a: A,\n  b: B,\n  c: {\n    prop: float,\n  },\n}",
		},
		{
			name: "hoist_all_classes",
			bundle: mkBundle(classRef("Ret"), nil,
				[]schema.ClassDef{
					class("A", field("prop", pInt())),
					class("B", field("prop", pString())),
					class("C", field("prop", pFloat())),
					class("Ret", field("a", classRef("A")), field("b", classRef("B")), field("c", classRef("C"))),
				}, nil, nil),
			opts: Options{HoistClasses: HoistClasses{Mode: HoistAll}},
			want: "A {\n  prop: int,\n}\n\nB {\n  prop: string,\n}\n\nC {\n  prop: float,\n}\n\nRet {\n  a: A,\n  b: B,\n  c: C,\n}\n\nAnswer in JSON using this schema: Ret",
		},
	}
}
