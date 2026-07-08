package bamlparser

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// Helpers: locate parsed type-system nodes and render TypeExpr to a stable
// compact string used as the golden form.
// -----------------------------------------------------------------------

func findTypeBlock(f *File, name string) *TypeBlock {
	for _, it := range f.Items {
		if it.TypeBlock != nil && it.TypeBlock.Name == name {
			return it.TypeBlock
		}
	}
	return nil
}

func findTypeAlias(f *File, name string) *TypeAlias {
	for _, it := range f.Items {
		if it.TypeAlias != nil && it.TypeAlias.Name == name {
			return it.TypeAlias
		}
	}
	return nil
}

func memberByName(members []*TypeMember, name string) *TypeMember {
	for _, m := range members {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// renderType serialises a TypeExpr into a compact, deterministic string used
// as the golden representation in the tests below. Attributes are appended
// in source order.
func renderType(t *TypeExpr) string {
	if t == nil {
		return "<nil>"
	}
	var s string
	switch t.Kind {
	case KindPrimitive:
		s = t.Primitive
	case KindMedia:
		s = t.Media
	case KindNameRef:
		s = t.Name
	case KindList:
		s = renderType(t.Elem) + strings.Repeat("[]", t.Dims)
	case KindMap:
		s = fmt.Sprintf("map<%s, %s>", renderType(t.Key), renderType(t.Value))
	case KindUnion:
		if t.Nullable && len(t.Variants) == 1 {
			s = renderType(t.Variants[0]) + "?"
		} else {
			parts := make([]string, 0, len(t.Variants))
			for _, v := range t.Variants {
				parts = append(parts, renderType(v))
			}
			s = strings.Join(parts, " | ")
			if t.Nullable {
				s += " |null"
			}
		}
	case KindLiteral:
		if t.LiteralKind == "string" {
			s = fmt.Sprintf("%q", t.LiteralValue)
		} else {
			s = t.LiteralValue
		}
	case KindTuple:
		parts := make([]string, 0, len(t.Items))
		for _, it := range t.Items {
			parts = append(parts, renderType(it))
		}
		s = "(" + strings.Join(parts, ", ") + ")"
	case KindGroup:
		s = "(" + renderType(t.Inner) + ")"
	case KindUnsupported:
		s = "unsupported(" + t.Reason + ")"
	default:
		s = "?"
	}
	return s + renderAttrs(t.Attributes)
}

func renderAttrs(attrs []*Attribute) string {
	var b strings.Builder
	for _, a := range attrs {
		b.WriteString(" ")
		if a.Block {
			b.WriteString("@@")
		} else {
			b.WriteString("@")
		}
		b.WriteString(a.Name)
		if a.HasParens {
			b.WriteString("(" + a.RawArgs + ")")
		}
	}
	return b.String()
}

// aliasType parses `type T = <expr>` and returns the parsed RHS TypeExpr.
func aliasType(t *testing.T, expr string) *TypeExpr {
	t.Helper()
	f := mustParse(t, "type T = "+expr+"\n")
	ta := findTypeAlias(f, "T")
	if ta == nil {
		t.Fatalf("type alias T not parsed from %q; items: %+v", expr, f.Items)
	}
	if ta.Expr == nil {
		t.Fatalf("alias RHS %q produced nil Expr", expr)
	}
	return ta.Expr
}

// -----------------------------------------------------------------------
// Type-expression grammar / lowering.
// -----------------------------------------------------------------------

func TestTypeExpr_Table(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		// Primitives + media.
		{"string", "string"},
		{"int", "int"},
		{"float", "float"},
		{"bool", "bool"},
		{"image", "image"},
		{"audio", "audio"},
		{"pdf", "pdf"},
		{"video", "video"},
		// Named refs.
		{"Person", "Person"},
		// Optionals lower to a nullable union with a single variant.
		{"string?", "string?"},
		{"Person?", "Person?"},
		// Arrays (single + multi-dimensional) and optional arrays.
		{"string[]", "string[]"},
		{"Person[]", "Person[]"},
		{"int[][]", "int[][]"},
		{"string[]?", "string[]?"},
		// Maps.
		{"map<string, int>", "map<string, int>"},
		{"map<string, Person>?", "map<string, Person>?"},
		// Unions.
		{"int | string", "int | string"},
		{"int | float | bool", "int | float | bool"},
		// Explicit null variant is retained as a primitive variant in slice 1.
		{"int | null", "int | null"},
		// Parenthesized group + union, then array.
		{"(image | null)[]", "(image | null)[]"},
		{"(int | string)?", "(int | string)?"},
		// Literals.
		{`"active"`, `"active"`},
		{"42", "42"},
		{"true", "true"},
		{"false", "false"},
		// Recursive-alias JSON shape from types.baml.
		{
			"int | float | bool | string | null | JsonValue[] | map<string, JsonValue>",
			"int | float | bool | string | null | JsonValue[] | map<string, JsonValue>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got := renderType(aliasType(t, tc.expr))
			if got != tc.want {
				t.Errorf("type %q rendered %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

func TestTypeExpr_OptionalLowersToNullableUnion(t *testing.T) {
	te := aliasType(t, "string?")
	if te.Kind != KindUnion || !te.Nullable || len(te.Variants) != 1 {
		t.Fatalf("string? should lower to nullable single-variant union, got %+v", te)
	}
	if te.Variants[0].Kind != KindPrimitive || te.Variants[0].Primitive != "string" {
		t.Errorf("variant should be primitive string, got %+v", te.Variants[0])
	}
}

func TestTypeExpr_ArrayDimsAndOptionalWrapsList(t *testing.T) {
	te := aliasType(t, "string[]?")
	if te.Kind != KindUnion || !te.Nullable || len(te.Variants) != 1 {
		t.Fatalf("string[]? should be nullable union around the list, got %+v", te)
	}
	list := te.Variants[0]
	if list.Kind != KindList || list.Dims != 1 || list.Elem.Primitive != "string" {
		t.Errorf("inner should be List<string> dims=1, got %+v", list)
	}
}

func TestTypeExpr_MapKeyValue(t *testing.T) {
	te := aliasType(t, "map<string, Person>")
	if te.Kind != KindMap {
		t.Fatalf("want map, got %+v", te)
	}
	if te.Key.Kind != KindPrimitive || te.Key.Primitive != "string" {
		t.Errorf("map key should be string, got %+v", te.Key)
	}
	if te.Value.Kind != KindNameRef || te.Value.Name != "Person" {
		t.Errorf("map value should be NameRef Person, got %+v", te.Value)
	}
}

func TestTypeExpr_Tuple(t *testing.T) {
	te := aliasType(t, "(int, string, bool)")
	if te.Kind != KindTuple || len(te.Items) != 3 {
		t.Fatalf("want 3-item tuple, got %+v", te)
	}
}

func TestTypeExpr_QualifiedNamesDeclineHints(t *testing.T) {
	// Namespaced and path identifiers are retained (BAML rejects path
	// identifiers in base types; we are laxer and mark them for the builder
	// to decline). See #586.
	ns := aliasType(t, "foo::Bar")
	if ns.Kind != KindNameRef || !ns.Namespaced || ns.Path {
		t.Errorf("foo::Bar should be namespaced NameRef, got %+v", ns)
	}
	pth := aliasType(t, "foo.Bar")
	if pth.Kind != KindNameRef || !pth.Path || pth.Namespaced {
		t.Errorf("foo.Bar should be path NameRef, got %+v", pth)
	}
}

func TestTypeExpr_NonMapGenericDeclines(t *testing.T) {
	te := aliasType(t, "list<int>")
	if te.Kind != KindUnsupported {
		t.Fatalf("non-map generic should be Unsupported, got %+v", te)
	}
	if !strings.Contains(te.Reason, "generic") {
		t.Errorf("reason should mention generic, got %q", te.Reason)
	}
}

func TestTypeExpr_FloatLiteralParsedForLaterDecline(t *testing.T) {
	// BAML rejects float literal types; we accept at parse and tag the kind
	// so the builder declines. See #586.
	te := aliasType(t, "3.14")
	if te.Kind != KindLiteral || te.LiteralKind != "float" || te.LiteralValue != "3.14" {
		t.Fatalf("float literal should parse as Literal{float,3.14}, got %+v", te)
	}
}

// -----------------------------------------------------------------------
// Classes and enums.
// -----------------------------------------------------------------------

func TestClass_MembersFromTypesBaml(t *testing.T) {
	src := `
class Person {
    name string
    age int
    email string?
    tags string[]
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "Person")
	if c == nil || c.Keyword != "class" {
		t.Fatalf("class Person not parsed; items: %+v", f.Items)
	}
	want := map[string]string{
		"name":  "string",
		"age":   "int",
		"email": "string?",
		"tags":  "string[]",
	}
	if len(c.Fields) != len(want) {
		t.Fatalf("want %d fields, got %d: %+v", len(want), len(c.Fields), c.Fields)
	}
	for _, m := range c.Fields {
		if m.Type == nil {
			t.Errorf("class field %q should have a type", m.Name)
			continue
		}
		if got := renderType(m.Type); got != want[m.Name] {
			t.Errorf("field %q = %q, want %q", m.Name, got, want[m.Name])
		}
	}
}

func TestEnum_ValuesHaveNoType(t *testing.T) {
	src := `
enum Category {
    TECH
    BUSINESS
    OTHER
}
`
	f := mustParse(t, src)
	e := findTypeBlock(f, "Category")
	if e == nil || e.Keyword != "enum" {
		t.Fatalf("enum Category not parsed; items: %+v", f.Items)
	}
	wantOrder := []string{"TECH", "BUSINESS", "OTHER"}
	if len(e.Fields) != len(wantOrder) {
		t.Fatalf("want %d values, got %d: %+v", len(wantOrder), len(e.Fields), e.Fields)
	}
	for i, m := range e.Fields {
		if m.Name != wantOrder[i] {
			t.Errorf("value[%d] = %q, want %q", i, m.Name, wantOrder[i])
		}
		if m.Type != nil {
			t.Errorf("enum value %q should have nil Type, got %+v", m.Name, m.Type)
		}
	}
}

func TestClass_NestedAndEnumRefAndComprehensive(t *testing.T) {
	// A representative slice of integration/testdata/baml_src/types.baml.
	src := `
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
`
	f := mustParse(t, src)
	comp := findTypeBlock(f, "ComprehensiveOutput")
	if comp == nil {
		t.Fatalf("ComprehensiveOutput not parsed")
	}
	want := map[string]string{
		"id":          "int",
		"title":       "string",
		"description": "string?",
		"score":       "float",
		"is_active":   "bool",
		"metadata":    "Metadata",
		"related_ids": "int[]",
		"labels":      "string[]?",
	}
	for name, w := range want {
		m := memberByName(comp.Fields, name)
		if m == nil || m.Type == nil {
			t.Errorf("missing field %q", name)
			continue
		}
		if got := renderType(m.Type); got != w {
			t.Errorf("ComprehensiveOutput.%s = %q, want %q", name, got, w)
		}
	}
}

func TestClass_MediaFields(t *testing.T) {
	src := `
class DocumentBundle {
    cover image?
    document pdf
    title string
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "DocumentBundle")
	if c == nil {
		t.Fatalf("DocumentBundle not parsed")
	}
	cover := memberByName(c.Fields, "cover")
	if cover == nil || renderType(cover.Type) != "image?" {
		t.Errorf("cover = %v, want image?", cover)
	}
	doc := memberByName(c.Fields, "document")
	if doc == nil || doc.Type.Kind != KindMedia || doc.Type.Media != "pdf" {
		t.Errorf("document should be media pdf, got %+v", doc)
	}
}

// -----------------------------------------------------------------------
// Type aliases.
// -----------------------------------------------------------------------

func TestTypeAlias_RecursiveShapes(t *testing.T) {
	src := `
type TreeNodeList = TreeNode[]
type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>
`
	f := mustParse(t, src)
	tl := findTypeAlias(f, "TreeNodeList")
	if tl == nil || tl.Expr == nil {
		t.Fatalf("TreeNodeList alias not parsed")
	}
	if got := renderType(tl.Expr); got != "TreeNode[]" {
		t.Errorf("TreeNodeList = %q, want TreeNode[]", got)
	}
	jv := findTypeAlias(f, "JsonValue")
	if jv == nil || jv.Expr == nil {
		t.Fatalf("JsonValue alias not parsed")
	}
	if jv.Expr.Kind != KindUnion || len(jv.Expr.Variants) != 7 {
		t.Errorf("JsonValue should be a 7-variant union, got %+v", jv.Expr)
	}
}

// countItems tallies the top-level item kinds relevant to the fail-closed
// alias-RHS assertions below.
func countItems(f *File) (aliases, typeBlocks, clients int) {
	for _, it := range f.Items {
		switch {
		case it.TypeAlias != nil:
			aliases++
		case it.TypeBlock != nil:
			typeBlocks++
		case it.Client != nil:
			clients++
		}
	}
	return
}

// TestTypeAlias_PartialOrGarbageRHSFailClosed pins that a malformed or
// partially-parsed type-alias RHS does not fabricate phantom AST: leftover /
// trailing tokens after the RHS are NOT misread into extra TypeAlias
// declarations or into TypeBlock fields, and a valid declaration after the
// alias still parses. This mirrors the fail-closed member behaviour added in
// workstream 5 (typeless class fields -> Unsupported).
//
// This is a deliberately laxer-than-BAML tolerance: BAML rejects a
// malformed alias RHS at validation before our parser ever runs, so in
// production we only see valid input. When we do encounter garbage, we
// capture the valid RHS prefix (or nothing) and drop the rest to Other
// items rather than fabricating schema-affecting nodes. See #586.
func TestTypeAlias_PartialOrGarbageRHSFailClosed(t *testing.T) {
	t.Run("valid_prefix_then_trailing_garbage", func(t *testing.T) {
		// `int` is a valid RHS prefix; `! ! !` is trailing garbage. The alias
		// captures `int`; the garbage must NOT become extra aliases or class
		// members, and the following client must still parse.
		src := `
type Partial = int ! ! !

client<llm> After { provider openai }
`
		f := mustParse(t, src)
		aliases, typeBlocks, clients := countItems(f)
		if aliases != 1 {
			t.Errorf("want exactly 1 TypeAlias, got %d", aliases)
		}
		if typeBlocks != 0 {
			t.Errorf("garbage must not fabricate any TypeBlock, got %d", typeBlocks)
		}
		if clients != 1 {
			t.Errorf("following client should still parse, got %d clients", clients)
		}
		ta := findTypeAlias(f, "Partial")
		if ta == nil || ta.Expr == nil || renderType(ta.Expr) != "int" {
			t.Errorf("alias should capture the valid `int` prefix, got %+v", ta)
		}
		if findClient(f, "After") == nil {
			t.Errorf("client After lost after garbage alias RHS")
		}
	})

	t.Run("unparseable_rhs_marks_nil_expr", func(t *testing.T) {
		// The RHS cannot begin a type at all. Fail-closed: the alias is kept
		// with a nil Expr (no fabricated RHS), no phantom TypeBlock is
		// produced, and the following client still parses.
		src := `
type Broken = ! ? &

client<llm> After2 { provider openai }
`
		f := mustParse(t, src)
		aliases, typeBlocks, clients := countItems(f)
		if aliases != 1 {
			t.Errorf("want exactly 1 TypeAlias, got %d", aliases)
		}
		if typeBlocks != 0 {
			t.Errorf("garbage must not fabricate any TypeBlock, got %d", typeBlocks)
		}
		if clients != 1 {
			t.Errorf("following client should still parse, got %d clients", clients)
		}
		ta := findTypeAlias(f, "Broken")
		if ta == nil {
			t.Fatalf("alias Broken missing")
		}
		if ta.Expr != nil {
			t.Errorf("unparseable RHS must leave Expr nil (fail-closed), got %+v", ta.Expr)
		}
		if findClient(f, "After2") == nil {
			t.Errorf("client After2 lost after unparseable alias RHS")
		}
	})

	t.Run("trailing_bare_identifier_not_absorbed", func(t *testing.T) {
		// A valid alias prefix followed by a stray identifier: the identifier
		// must not be absorbed into the alias type nor fabricated as a class
		// field. It drops to an Other item.
		src := `
type Stray = string Leftover

client<llm> After3 { provider openai }
`
		f := mustParse(t, src)
		aliases, typeBlocks, _ := countItems(f)
		if aliases != 1 {
			t.Errorf("want exactly 1 TypeAlias, got %d", aliases)
		}
		if typeBlocks != 0 {
			t.Errorf("stray identifier must not fabricate a TypeBlock, got %d", typeBlocks)
		}
		ta := findTypeAlias(f, "Stray")
		if ta == nil || ta.Expr == nil || renderType(ta.Expr) != "string" {
			t.Errorf("alias should capture only `string`, got %+v", ta)
		}
		if findClient(f, "After3") == nil {
			t.Errorf("client After3 lost after stray-identifier alias RHS")
		}
	})
}

// -----------------------------------------------------------------------
// Function signatures.
// -----------------------------------------------------------------------

func TestFunction_ParamsAndReturn(t *testing.T) {
	src := `
function GetPerson(description: string) -> Person {
    client TestClient
    prompt #"{{ ctx.output_format }}"#
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "GetPerson")
	if fn == nil {
		t.Fatalf("GetPerson not parsed")
	}
	if len(fn.Params) != 1 || fn.Params[0].Name != "description" {
		t.Fatalf("params = %+v, want [description]", fn.Params)
	}
	if renderType(fn.Params[0].Type) != "string" {
		t.Errorf("param type = %q, want string", renderType(fn.Params[0].Type))
	}
	if fn.Return == nil || renderType(fn.Return) != "Person" {
		t.Errorf("return = %v, want Person", fn.Return)
	}
	// Config extraction must still see the client field.
	if c := fieldByKey(fn.Fields, "client"); c == nil || *c.Value.Ident != "TestClient" {
		t.Errorf("client field lost: %+v", c)
	}
}

func TestFunction_ReturnTypeShapes(t *testing.T) {
	cases := []struct {
		sig  string
		want string
	}{
		{"function F(x: string) -> string", "string"},
		{"function F(x: string) -> Person[]", "Person[]"},
		{"function F(img: image) -> string", "string"},
		{"function F(images: (image | null)[]) -> string", "string"},
		{"function F(images: image[]?) -> string", "string"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			f := mustParse(t, tc.sig+" {\n    client C\n    prompt #\"x\"#\n}\n")
			fn := findFunction(f, "F")
			if fn == nil {
				t.Fatalf("F not parsed from %q", tc.sig)
			}
			if fn.Return == nil || renderType(fn.Return) != tc.want {
				t.Errorf("return = %v, want %q", renderType(fn.Return), tc.want)
			}
		})
	}
}

func TestFunction_MediaAndListParams(t *testing.T) {
	f := mustParse(t, `function DescribeOptionalImages(images: (image | null)[]) -> string {
    client C
    prompt #"x"#
}
`)
	fn := findFunction(f, "DescribeOptionalImages")
	if fn == nil || len(fn.Params) != 1 {
		t.Fatalf("params not parsed: %+v", fn)
	}
	if got := renderType(fn.Params[0].Type); got != "(image | null)[]" {
		t.Errorf("param type = %q, want (image | null)[]", got)
	}
}

// -----------------------------------------------------------------------
// Attributes.
// -----------------------------------------------------------------------

func TestAttr_BlockDynamicOnClass(t *testing.T) {
	src := `
class DynamicOutput {
    base_field string
    @@dynamic
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "DynamicOutput")
	if c == nil {
		t.Fatalf("DynamicOutput not parsed")
	}
	if len(c.Attributes) != 1 {
		t.Fatalf("want 1 block attr, got %+v", c.Attributes)
	}
	a := c.Attributes[0]
	if !a.Block || a.Name != "dynamic" {
		t.Errorf("block attr = %+v, want @@dynamic", a)
	}
	// base_field must still be a member.
	if m := memberByName(c.Fields, "base_field"); m == nil || renderType(m.Type) != "string" {
		t.Errorf("base_field member lost: %+v", m)
	}
}

func TestAttr_EnumValueAttribute(t *testing.T) {
	src := `
enum Category {
    TECH @alias("technology")
    OTHER
}
`
	f := mustParse(t, src)
	e := findTypeBlock(f, "Category")
	tech := memberByName(e.Fields, "TECH")
	if tech == nil {
		t.Fatalf("TECH value missing")
	}
	if tech.Type != nil {
		t.Errorf("enum value should have no type, got %+v", tech.Type)
	}
	if len(tech.Attributes) != 1 || tech.Attributes[0].Name != "alias" {
		t.Fatalf("TECH should carry @alias, got %+v", tech.Attributes)
	}
	if tech.Attributes[0].RawArgs != `"technology"` {
		t.Errorf("alias raw args = %q, want %q", tech.Attributes[0].RawArgs, `"technology"`)
	}
	if len(tech.Attributes[0].Args) != 1 {
		t.Fatalf("alias should have 1 structured arg")
	}
	if lit, ok := tech.Attributes[0].Args[0].LiteralValue(); !ok || lit != "technology" {
		t.Errorf("alias arg literal = %q,%v, want technology", lit, ok)
	}
}

func TestAttr_FieldDescriptionAttachesToType(t *testing.T) {
	src := `
class C {
    name string @description("the name")
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	m := memberByName(c.Fields, "name")
	if m == nil || m.Type == nil {
		t.Fatalf("name field missing")
	}
	// The trailing field attribute folds onto the type via field_type_with_attr.
	if len(m.Type.Attributes) != 1 || m.Type.Attributes[0].Name != "description" {
		t.Fatalf("description should attach to the field type, got type attrs %+v / member attrs %+v",
			m.Type.Attributes, m.Attributes)
	}
	if m.Type.Attributes[0].RawArgs != `"the name"` {
		t.Errorf("raw args = %q", m.Type.Attributes[0].RawArgs)
	}
}

func TestAttr_DottedStreamNames(t *testing.T) {
	src := `
class C {
    value string @stream.done @stream.not_null
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	m := memberByName(c.Fields, "value")
	if m == nil || m.Type == nil {
		t.Fatalf("value field missing")
	}
	if len(m.Type.Attributes) != 2 {
		t.Fatalf("want 2 stream attrs, got %+v", m.Type.Attributes)
	}
	names := []string{m.Type.Attributes[0].Name, m.Type.Attributes[1].Name}
	if names[0] != "stream.done" || names[1] != "stream.not_null" {
		t.Errorf("dotted attr names = %v, want [stream.done stream.not_null]", names)
	}
}

func TestAttr_ConstraintRawArgsPreserved(t *testing.T) {
	// Constraint attributes carry Jinja expression text that must survive
	// verbatim in RawArgs even though the structured parse cannot represent
	// it.
	src := `
class C {
    count int @check(positive, {{ this > 0 }})
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	m := memberByName(c.Fields, "count")
	if m == nil || m.Type == nil || len(m.Type.Attributes) != 1 {
		t.Fatalf("count @check not parsed: %+v", m)
	}
	a := m.Type.Attributes[0]
	if a.Name != "check" {
		t.Errorf("attr name = %q, want check", a.Name)
	}
	if a.RawArgs != "positive, {{ this > 0 }}" {
		t.Errorf("raw args = %q, want %q", a.RawArgs, "positive, {{ this > 0 }}")
	}
}

func TestAttr_AliasOnTypeAlias(t *testing.T) {
	// BAML permits @check/@assert on type aliases. Ensure trailing alias
	// attributes are captured.
	f := mustParse(t, "type Positive = int @assert(gt0, {{ this > 0 }})\n")
	ta := findTypeAlias(f, "Positive")
	if ta == nil || ta.Expr == nil {
		t.Fatalf("Positive alias not parsed")
	}
	// The attribute folds onto the RHS type via the type grammar.
	if len(ta.Expr.Attributes) != 1 || ta.Expr.Attributes[0].Name != "assert" {
		t.Fatalf("assert should attach to alias RHS type, got %+v (alias attrs %+v)",
			ta.Expr.Attributes, ta.Attributes)
	}
}

// classFieldAttr parses `class C { f <type> }` and returns the sole attribute
// attached to field f's type (attributes fold onto the type via
// field_type_with_attr).
func classFieldAttr(t *testing.T, typeExpr string) *Attribute {
	t.Helper()
	f := mustParse(t, "class C {\n    f "+typeExpr+"\n}\n")
	c := findTypeBlock(f, "C")
	if c == nil {
		t.Fatalf("class C not parsed from field type %q", typeExpr)
	}
	m := memberByName(c.Fields, "f")
	if m == nil || m.Type == nil {
		t.Fatalf("field f missing/typeless for %q", typeExpr)
	}
	if len(m.Type.Attributes) != 1 {
		t.Fatalf("want exactly 1 attr on field type for %q, got %+v", typeExpr, m.Type.Attributes)
	}
	return m.Type.Attributes[0]
}

// -----------------------------------------------------------------------
// Attribute structured-arg fidelity: complex (Jinja / list / group) args
// must NOT leak scalar tokens into Args, but the exact RawArgs is preserved.
// -----------------------------------------------------------------------

func attrArgIdents(a *Attribute) []string {
	var out []string
	for _, v := range a.Args {
		if id, ok := v.IdentValue(); ok {
			out = append(out, id)
		} else if lit, ok := v.LiteralValue(); ok {
			out = append(out, lit)
		} else {
			out = append(out, "<non-scalar>")
		}
	}
	return out
}

func TestAttr_ComplexArgsDoNotLeakScalars(t *testing.T) {
	cases := []struct {
		name     string
		typeExpr string
		wantRaw  string
		wantArgs []string // structured Args (idents/literals), in order
	}{
		{
			// The Jinja arg must not leak `this` (the pre-fix bug).
			name:     "jinja_second_arg",
			typeExpr: `int @check(positive, {{ this > 0 }})`,
			wantRaw:  "positive, {{ this > 0 }}",
			wantArgs: []string{"positive"},
		},
		{
			// A leading Jinja arg produces NO structured args at all.
			name:     "jinja_first_arg",
			typeExpr: `int @meta({{ this|length > 0 }})`,
			wantRaw:  "{{ this|length > 0 }}",
			wantArgs: nil,
		},
		{
			// A list arg must not leak its elements, nor mis-segment on the
			// commas inside the brackets.
			name:     "list_arg",
			typeExpr: `int @meta([alpha, beta])`,
			wantRaw:  "[alpha, beta]",
			wantArgs: nil,
		},
		{
			// A group arg must not leak its inner scalars.
			name:     "group_arg",
			typeExpr: `int @meta((alpha, beta))`,
			wantRaw:  "(alpha, beta)",
			wantArgs: nil,
		},
		{
			// Simple scalar args are still captured.
			name:     "plain_scalars",
			typeExpr: `int @meta(alpha, "beta")`,
			wantRaw:  `alpha, "beta"`,
			wantArgs: []string{"alpha", "beta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := classFieldAttr(t, tc.typeExpr)
			if a.RawArgs != tc.wantRaw {
				t.Errorf("RawArgs = %q, want %q", a.RawArgs, tc.wantRaw)
			}
			got := attrArgIdents(a)
			if len(got) != len(tc.wantArgs) {
				t.Fatalf("Args = %v, want %v", got, tc.wantArgs)
			}
			for i := range got {
				if got[i] != tc.wantArgs[i] {
					t.Errorf("Args[%d] = %q, want %q (full %v)", i, got[i], tc.wantArgs[i], tc.wantArgs)
				}
			}
			// `this` must never appear as a structured arg.
			for _, id := range got {
				if id == "this" {
					t.Errorf("leaked Jinja token `this` into structured Args: %v", got)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------
// Parenthesized markers on tuples / groups.
// -----------------------------------------------------------------------

func TestTypeExpr_TupleIsParenthesized(t *testing.T) {
	te := aliasType(t, "(int, string, bool)")
	if te.Kind != KindTuple {
		t.Fatalf("want tuple, got %+v", te)
	}
	if !te.Parenthesized {
		t.Errorf("a tuple is syntactically parenthesized; Parenthesized should be true")
	}
}

func TestTypeExpr_ParenthesizedChildrenMarked(t *testing.T) {
	// `(int | string @attr)`: @attr lives on the `string` union VARIANT, and
	// the whole thing is inside parentheses, so it must be marked
	// Parenthesized (recursively into the variant), not just the group's
	// inner node.
	grp := aliasType(t, "(int | string @description(\"d\"))")
	if grp.Kind != KindGroup || !grp.Parenthesized {
		t.Fatalf("outer should be a parenthesized group, got %+v", grp)
	}
	union := grp.Inner
	if union == nil || union.Kind != KindUnion || len(union.Variants) != 2 {
		t.Fatalf("inner should be a 2-variant union, got %+v", union)
	}
	strVariant := union.Variants[1]
	if len(strVariant.Attributes) != 1 {
		t.Fatalf("string variant should carry @description, got %+v", strVariant.Attributes)
	}
	if !strVariant.Attributes[0].Parenthesized {
		t.Errorf("attribute on a variant inside parentheses must be marked Parenthesized")
	}
}

func TestTypeExpr_TupleItemAttrsMarkedParenthesized(t *testing.T) {
	tup := aliasType(t, `(int @description("a"), string @description("b"))`)
	if tup.Kind != KindTuple || len(tup.Items) != 2 {
		t.Fatalf("want 2-item tuple, got %+v", tup)
	}
	for i, it := range tup.Items {
		if len(it.Attributes) != 1 || !it.Attributes[0].Parenthesized {
			t.Errorf("tuple item[%d] attribute should be Parenthesized, got %+v", i, it.Attributes)
		}
	}
}

// -----------------------------------------------------------------------
// Fail-closed: a typeless CLASS field is Unsupported, not a silent nil-type.
// -----------------------------------------------------------------------

func TestClass_TypelessFieldMarkedUnsupported(t *testing.T) {
	// A class field with no type on its line is malformed (BAML rejects it);
	// it must be marked Unsupported rather than silently created as an
	// enum-like nil-type member.
	src := `
class C {
    name
    age int
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	if c == nil {
		t.Fatalf("class C not parsed")
	}
	if !c.HasUnsupportedContent {
		t.Errorf("class with a typeless field should set HasUnsupportedContent")
	}
	name := memberByName(c.Fields, "name")
	if name == nil {
		t.Fatalf("member `name` missing")
	}
	if name.Type == nil {
		t.Errorf("typeless CLASS field must NOT be a silent nil-type member")
	} else if name.Type.Kind != KindUnsupported {
		t.Errorf("typeless class field should be Unsupported, got %+v", name.Type)
	}
	// The well-formed field is unaffected.
	if age := memberByName(c.Fields, "age"); age == nil || renderType(age.Type) != "int" {
		t.Errorf("age should be int, got %+v", age)
	}
}

func TestEnum_ValuesStayTypelessNotUnsupported(t *testing.T) {
	// The fail-closed rule for typeless CLASS fields must NOT touch enum
	// values, which legitimately have Type == nil.
	src := `
enum E {
    A
    B
}
`
	f := mustParse(t, src)
	e := findTypeBlock(f, "E")
	if e == nil {
		t.Fatalf("enum E not parsed")
	}
	if e.HasUnsupportedContent {
		t.Errorf("enum with plain values should not be flagged unsupported")
	}
	for _, m := range e.Fields {
		if m.Type != nil {
			t.Errorf("enum value %q must keep Type == nil, got %+v", m.Name, m.Type)
		}
	}
}

// -----------------------------------------------------------------------
// Class methods -> unsupported flag.
// -----------------------------------------------------------------------

func TestClass_MethodMarksUnsupported(t *testing.T) {
	// A class containing a method/expr_fn (with its own brace body) is
	// detected as an expr_fn and DECLINED before it can be misread into
	// Fields: the body is balanced-skipped, the method name is recorded on
	// Methods, HasUnsupportedContent is set, and the trailing client still
	// parses. Crucially, the method header (`function double() -> int`) must
	// NOT pollute Fields — only the real `value int` member remains.
	src := `
class C {
    value int
    function double() -> int {
        value * 2
    }
}

client<llm> After { provider openai }
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	if c == nil {
		t.Fatalf("class C not parsed")
	}
	if !c.HasUnsupportedContent {
		t.Errorf("class with a method body should set HasUnsupportedContent")
	}
	// Fields must contain ONLY the real member, not the method header tokens.
	if len(c.Fields) != 1 {
		t.Fatalf("Fields polluted by method header: want 1 member, got %d: %+v",
			len(c.Fields), c.Fields)
	}
	only := c.Fields[0]
	if only.Name != "value" || only.Type == nil || renderType(only.Type) != "int" {
		t.Errorf("sole member should be `value int`, got %+v", only)
	}
	if memberByName(c.Fields, "function") != nil || memberByName(c.Fields, "double") != nil {
		t.Errorf("method header leaked into Fields: %+v", c.Fields)
	}
	// The method name is recorded on Methods for later decline diagnostics.
	if len(c.Methods) != 1 || c.Methods[0] != "double" {
		t.Errorf("Methods = %v, want [double]", c.Methods)
	}
	if findClient(f, "After") == nil {
		t.Errorf("client After lost after class with method; brace-skip misaligned")
	}
}

func TestClass_MethodBetweenFieldsKeepsBothMembers(t *testing.T) {
	// A method declared between two real fields must not consume or corrupt
	// the fields on either side.
	src := `
class C {
    a int
    function helper(x: int) -> int {
        x + 1
    }
    b string
}
`
	f := mustParse(t, src)
	c := findTypeBlock(f, "C")
	if c == nil {
		t.Fatalf("class C not parsed")
	}
	if len(c.Fields) != 2 {
		t.Fatalf("want 2 real members, got %d: %+v", len(c.Fields), c.Fields)
	}
	a := memberByName(c.Fields, "a")
	b := memberByName(c.Fields, "b")
	if a == nil || renderType(a.Type) != "int" {
		t.Errorf("member a = %+v, want int", a)
	}
	if b == nil || renderType(b.Type) != "string" {
		t.Errorf("member b = %+v, want string", b)
	}
	if len(c.Methods) != 1 || c.Methods[0] != "helper" || !c.HasUnsupportedContent {
		t.Errorf("method helper not recorded/declined: Methods=%v flag=%v", c.Methods, c.HasUnsupportedContent)
	}
}

// -----------------------------------------------------------------------
// Lexer: multi-hash raw strings.
// -----------------------------------------------------------------------

func TestLexer_MultiHashRawString(t *testing.T) {
	// A double-hash raw string can embed a `"#` sequence verbatim.
	src := `
function F(x: string) -> string {
    client C
    prompt ##"contains "# inside"##
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("F not parsed")
	}
	pr := fieldByKey(fn.Fields, "prompt")
	if pr == nil || pr.Value == nil || pr.Value.Raw == nil {
		t.Fatalf("prompt raw missing: %+v", pr)
	}
	if *pr.Value.Raw != `contains "# inside` {
		t.Errorf("multi-hash raw content = %q, want %q", *pr.Value.Raw, `contains "# inside`)
	}
}

func TestLexer_SingleHashRawStringStillWorks(t *testing.T) {
	if got := stripRawString(`#"hello"#`); got != "hello" {
		t.Errorf("single-hash strip = %q, want hello", got)
	}
	if got := stripRawString(`###"a"#b"###`); got != `a"#b` {
		t.Errorf("triple-hash strip = %q, want %q", got, `a"#b`)
	}
}

// -----------------------------------------------------------------------
// End-to-end: the real integration corpus. Skipped when the files are not
// present (e.g. a stripped-down checkout); the inline tests above are the
// authoritative coverage.
// -----------------------------------------------------------------------

func TestParse_RealTypesBamlCorpus(t *testing.T) {
	data, err := os.ReadFile("../../integration/testdata/baml_src/types.baml")
	if err != nil {
		t.Skip("types.baml not available")
	}
	f, err := ParseBytes("types.baml", data)
	if err != nil {
		t.Fatalf("parse types.baml: %v", err)
	}
	// Spot-check representative declarations parse with the expected shape.
	if c := findTypeBlock(f, "Person"); c == nil || len(c.Fields) != 4 {
		t.Errorf("Person should have 4 fields, got %+v", c)
	}
	if e := findTypeBlock(f, "Category"); e == nil || len(e.Fields) != 3 {
		t.Errorf("Category should have 3 enum values, got %+v", e)
	}
	// @@dynamic block attribute captured.
	if c := findTypeBlock(f, "DynamicOutput"); c == nil || len(c.Attributes) != 1 ||
		c.Attributes[0].Name != "dynamic" {
		t.Errorf("DynamicOutput should carry @@dynamic, got %+v", c)
	}
	// Optional-media list field.
	if c := findTypeBlock(f, "OptionalImageGallery"); c != nil {
		m := memberByName(c.Fields, "images")
		if m == nil || renderType(m.Type) != "(image | null)[]" {
			t.Errorf("OptionalImageGallery.images = %v, want (image | null)[]", m)
		}
	} else {
		t.Errorf("OptionalImageGallery not parsed")
	}
	// Recursive alias union.
	if a := findTypeAlias(f, "JsonValue"); a == nil || a.Expr == nil ||
		a.Expr.Kind != KindUnion || len(a.Expr.Variants) != 7 {
		t.Errorf("JsonValue alias should be a 7-variant union, got %+v", a)
	}
}

func TestParse_RealFunctionsBamlCorpus(t *testing.T) {
	data, err := os.ReadFile("../../integration/testdata/baml_src/functions.baml")
	if err != nil {
		t.Skip("functions.baml not available")
	}
	f, err := ParseBytes("functions.baml", data)
	if err != nil {
		t.Fatalf("parse functions.baml: %v", err)
	}
	checks := []struct {
		fn         string
		params     int
		wantReturn string
	}{
		{"GetGreeting", 1, "string"},
		{"GetPeople", 1, "Person[]"},
		{"DescribeOptionalImages", 1, "string"},
		{"ParseJson", 1, "JsonContainer"},
	}
	for _, c := range checks {
		fn := findFunction(f, c.fn)
		if fn == nil {
			t.Errorf("function %s not parsed", c.fn)
			continue
		}
		if len(fn.Params) != c.params {
			t.Errorf("%s params = %d, want %d", c.fn, len(fn.Params), c.params)
		}
		if fn.Return == nil || renderType(fn.Return) != c.wantReturn {
			t.Errorf("%s return = %v, want %q", c.fn, renderType(fn.Return), c.wantReturn)
		}
	}
	// The media-list param shape from functions.baml.
	if fn := findFunction(f, "DescribeOptionalImages"); fn != nil && len(fn.Params) == 1 {
		if got := renderType(fn.Params[0].Type); got != "(image | null)[]" {
			t.Errorf("DescribeOptionalImages param = %q, want (image | null)[]", got)
		}
	}
}
