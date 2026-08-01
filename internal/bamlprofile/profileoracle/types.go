package profileoracle

import (
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
	"github.com/invakid404/minijinja-go/v2/value"
)

// This file is the enum/class type layer of the differential: the shared,
// deterministic set of enum/class declarations, the types.baml source that
// declares them for the stock BAML leg, the matching bamlprofile.Config.Enums
// for the profile leg, and the converter that turns a row's plain-Go argument
// into a profile host VALUE the same way BAML lowers a BamlValue::Enum/Class/List
// on its leg.
//
// The declarations are AUTHOR-CONTROLLED corpus constants (fixed identifiers and
// alias literals), exactly like Param.Name/BamlType in functionSource — not a
// caller-fuzzed surface. A malformed one fails LOUD at CreateRuntime (a parse
// error), never a silent valid-but-wrong render, so they are emitted verbatim
// without a collision-proof delimiter. The enum aliases are deliberately chosen
// to discriminate: Color.RED aliases to "rouge" (alias != canonical); Shade.RED
// has the SAME canonical+alias as Color.RED (cross-enum equality proves the
// comparator omits enum_name); Size.RED has NO alias (same-canonical/different-
// alias inequality, and the None<Some alias ordering); and EmptyAlias.X /
// NoAliasX.X are an explicit `@alias("")` and its unaliased control, which
// separate Some("") from None in display and in the Option tie-break.

// enumVarDecl is one enum variant: canonical name and OPTIONAL alias.
//
// alias is a *string, not a "" sentinel, because BAML distinguishes NO @alias
// from `@alias("")`: the former leaves the resolved alias None (display falls
// back to the canonical name, and the comparator's Option tie-break orders it
// BELOW any Some), the latter is Some("") (display is empty, and the Option
// tie-break puts it ABOVE None). A "" sentinel cannot express the second, and
// the corpus needs both — see the enum_empty_alias row. nil = absent, non-nil ""
// = `@alias("")`. This matches bamlprofile.EnumValue.Alias exactly.
type enumVarDecl struct {
	canonical string
	alias     *string
}

// enumDecl is one enum declaration in declared order.
type enumDecl struct {
	name string
	vars []enumVarDecl
}

// classFieldDecl is one class field: canonical name, OPTIONAL alias (nil =
// unaliased, non-nil "" = `@alias("")`, as in enumVarDecl), and BAML type text
// (e.g. "string", "int", "B", "string[]", "Color", "string?", "string?[]").
type classFieldDecl struct {
	canonical string
	alias     *string
	typ       string
}

// al is the corpus's alias literal: a pointer to a PRESENT alias. Its absence is
// spelled as a plain nil in the declaration tables, so the two cases read
// differently at every use site.
func al(s string) *string { return &s }

// classDecl is one class declaration in declared (IndexMap) order.
type classDecl struct {
	name   string
	fields []classFieldDecl
}

// enumDecls is the shared enum set. It is fixed and deterministic.
func enumDecls() []enumDecl {
	return []enumDecl{
		{"Color", []enumVarDecl{{"RED", al("rouge")}, {"GREEN", nil}, {"BLUE", nil}}},
		// Same canonical AND alias as Color.RED -> Color.RED == Shade.RED is true,
		// proving the comparator ignores enum_name.
		{"Shade", []enumVarDecl{{"RED", al("rouge")}}},
		// Size.RED: canonical "RED", NO alias -> same-canonical/different-alias
		// (Some("rouge") vs None) is NOT equal, and orders None < Some.
		{"Size", []enumVarDecl{{"RED", nil}, {"SMALL", al("petit")}}},
		// EmptyAlias.X carries an EXPLICIT empty alias, Some(""): it displays as
		// nothing, its .value is still "X", and it compares as Some — strictly
		// ABOVE NoAliasX.X's None. NoAliasX.X is the None control with the same
		// canonical name, so the pair isolates Some("") vs None without also
		// changing the canonical string. Neither is expressible with a "" sentinel.
		{"EmptyAlias", []enumVarDecl{{"X", al("")}}},
		{"NoAliasX", []enumVarDecl{{"X", nil}}},
	}
}

// classDecls is the shared class set. Every class is referenced by a function
// parameter (directly or nested) so the stock project has no dead type.
//
// The DIRECT-RENDER and ITERATION rows use SINGLE-field classes (or ordered list
// fields), on purpose. A class argument is decoded directly from the protobuf
// map the Go client builds, whose entry order is the Go map's RANDOM iteration
// order (serde.EncodeMapEntries does not sort), and BAML preserves that arg
// order — so the RELATIVE order of two sibling fields is non-deterministic
// through this harness and cannot be byte-pinned. Single-field classes make
// every rendering mechanic (aliased key, indentation, trailing comma, nested
// class/list, none->null, escaping) deterministic. Multi-field INSERTION-order
// fidelity is proven at the leaf (bamlprofile.host_test.go) against BAML's own
// documented multi-field golden (jinja-runtime/src/lib.rs:1888). Multi-field
// classes are used ONLY where order does not matter: attribute access, length,
// and truthiness.
func classDecls() []classDecl {
	return []classDecl{
		{"C", []classFieldDecl{{"prop1", al("key1"), "string"}}},
		// Multi-field, used only for order-independent probes (access, length,
		// int-compare, enum-in-class access).
		{"WithColor", []classFieldDecl{
			{"color", al("colour"), "Color"},
			{"n", nil, "int"},
		}},
		// Lw/Cw: single-field wrappers that make a nested list and a nested class
		// deterministic, proving list-in-class, class-in-class, and the indentation
		// depths without a sibling-order dependency.
		{"Lw", []classFieldDecl{{"items", al("data"), "string[]"}}},
		{"Cw", []classFieldDecl{{"inner", al("nested"), "Lw"}}},
		// Nw: a single optional field, for the nested none -> null render row.
		{"Nw", []classFieldDecl{{"maybe", al("perhaps"), "string?"}}},
		// Ew: a single field carrying special characters, for scalar escaping.
		{"Ew", []classFieldDecl{{"raw", al("escaped"), "string"}}},
		// Ecw: a single enum field, rendered as the BARE alias inside the map.
		{"Ecw", []classFieldDecl{{"color", al("colour"), "Color"}}},
		// Empty: a FIELDLESS class. It is a KNOWN-empty map — falsey, length 0,
		// rendering as `{}` — which is exactly what a non-enumerable enum object is
		// NOT, so it is the live-CFFI control for the opaque-map rows.
		{"Empty", nil},
	}
}

// typesBAMLSource is the deterministic types.baml declaring every shared enum and
// class for the stock BAML leg. Included in every generated project so the enum
// namespace globals (which BAML injects for ALL declared enums) exist on both
// legs identically.
func typesBAMLSource() string {
	var b strings.Builder
	for _, e := range enumDecls() {
		b.WriteString("enum ")
		b.WriteString(e.name)
		b.WriteString(" {\n")
		for _, v := range e.vars {
			b.WriteString("  ")
			b.WriteString(v.canonical)
			// A PRESENT alias is emitted even when it is the empty string: `@alias("")`
			// is a distinct declaration from no @alias at all, and the corpus asserts
			// the difference.
			if v.alias != nil {
				b.WriteString(` @alias("`)
				b.WriteString(*v.alias)
				b.WriteString(`")`)
			}
			b.WriteString("\n")
		}
		b.WriteString("}\n\n")
	}
	for _, c := range classDecls() {
		b.WriteString("class ")
		b.WriteString(c.name)
		b.WriteString(" {\n")
		for _, f := range c.fields {
			b.WriteString("  ")
			b.WriteString(f.canonical)
			b.WriteString(" ")
			b.WriteString(f.typ)
			if f.alias != nil {
				b.WriteString(` @alias("`)
				b.WriteString(*f.alias)
				b.WriteString(`")`)
			}
			b.WriteString("\n")
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

// profileEnums is the bamlprofile.Config.Enums matching typesBAMLSource, so the
// profile leg installs the same namespace globals BAML injects.
func profileEnums() []bamlprofile.EnumDef {
	decls := enumDecls()
	out := make([]bamlprofile.EnumDef, 0, len(decls))
	for _, e := range decls {
		vals := make([]bamlprofile.EnumValue, len(e.vars))
		for i, v := range e.vars {
			vals[i] = bamlprofile.EnumValue{Canonical: v.canonical, Alias: v.alias}
		}
		out = append(out, bamlprofile.EnumDef{Name: e.name, Values: vals})
	}
	return out
}

func enumByName(name string) (enumDecl, bool) {
	for _, e := range enumDecls() {
		if e.name == name {
			return e, true
		}
	}
	return enumDecl{}, false
}

func classByName(name string) (classDecl, bool) {
	for _, c := range classDecls() {
		if c.name == name {
			return c, true
		}
	}
	return classDecl{}, false
}

// unparen strips one layer of BAML's grouping parentheses. BAML v0.223 rejects a
// bare `string?[]` and requires `(string?)[]` for a list of optionals, so the
// element type this walker recurses into can arrive parenthesized.
func unparen(typ string) string {
	t := strings.TrimSpace(typ)
	if len(t) >= 2 && t[0] == '(' && t[len(t)-1] == ')' {
		return strings.TrimSpace(t[1 : len(t)-1])
	}
	return t
}

// needsHostValue reports whether a parameter type must be converted into a
// profile host value on the profile leg. Enums, classes, and ANY list (BAML
// lowers a list arg to MinijinjaBamlList) are host-shaped; plain scalars are not.
func needsHostValue(typ string) bool {
	base := strings.TrimSuffix(unparen(typ), "?")
	if strings.HasSuffix(base, "[]") {
		return true
	}
	base = unparen(base)
	if _, ok := enumByName(base); ok {
		return true
	}
	_, ok := classByName(base)
	return ok
}

// hostValue lowers a plain-Go argument to the profile host value BAML would
// build for the same typed argument: an enum canonical string -> EnumMember, a
// map -> ClassValue over the class's resolved fields, a slice -> ListValue, a
// scalar -> the matching fork scalar. It fails loud on a shape that does not
// match the declared type rather than fabricating a value.
func hostValue(typ string, arg any) (value.Value, error) {
	// Grouping parentheses first: `(string?)[]` decomposes to the element type
	// `(string?)`, which only becomes `string?` once unwrapped.
	typ = unparen(typ)
	// Optional: a nil arg is none; otherwise unwrap and recurse.
	if strings.HasSuffix(typ, "?") {
		if arg == nil {
			return value.None(), nil
		}
		return hostValue(strings.TrimSuffix(typ, "?"), arg)
	}
	// List -> host list (MinijinjaBamlList).
	if strings.HasSuffix(typ, "[]") {
		elem := strings.TrimSuffix(typ, "[]")
		items, ok := arg.([]any)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: type %q expects a []any arg, got %T", typ, arg)
		}
		vals := make([]value.Value, len(items))
		for i, it := range items {
			hv, err := hostValue(elem, it)
			if err != nil {
				return value.Undefined(), err
			}
			vals[i] = hv
		}
		return bamlprofile.ListValue(vals)
	}
	// Enum -> enum member with the resolved alias for that canonical variant.
	if e, ok := enumByName(typ); ok {
		canonical, ok := arg.(string)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: enum %q expects a string canonical, got %T", typ, arg)
		}
		for _, v := range e.vars {
			if v.canonical == canonical {
				return bamlprofile.EnumMember(e.name, canonical, v.alias)
			}
		}
		return value.Undefined(), fmt.Errorf("profileoracle: enum %q has no variant %q", typ, canonical)
	}
	// Class -> class value over the class's declared fields. A directly-rendered
	// class here is always single-field (classDecls), so field order is trivially
	// deterministic; multi-field classes are used only for order-independent
	// probes (access/length), where the build order does not affect the result.
	// (A class ARGUMENT's field order is otherwise the Go client's random map
	// order, which BAML preserves — see classDecls — so it cannot be byte-pinned;
	// multi-field insertion order is proven at the leaf against BAML's own
	// documented golden.)
	if c, ok := classByName(typ); ok {
		m, ok := arg.(map[string]any)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: class %q expects a map[string]any arg, got %T", typ, arg)
		}
		// Every key in the arg map must name a DECLARED canonical field. A key
		// that does not — a misspelled OPTIONAL-field name, say — would otherwise
		// be silently dropped: the real field goes absent -> none on both legs, so
		// the differential stays green while proving less than the row reads. Fail
		// loud instead.
		declared := make(map[string]struct{}, len(c.fields))
		for _, fd := range c.fields {
			declared[fd.canonical] = struct{}{}
		}
		for k := range m {
			if _, ok := declared[k]; !ok {
				return value.Undefined(), fmt.Errorf("profileoracle: class %q has no field %q (arg map has an unknown key — a misspelled field name?)", typ, k)
			}
		}
		fields := make([]bamlprofile.ClassField, 0, len(c.fields))
		for _, fd := range c.fields {
			fv, present := m[fd.canonical]
			if !present {
				if !strings.HasSuffix(fd.typ, "?") {
					return value.Undefined(), fmt.Errorf("profileoracle: class %q missing required field %q", typ, fd.canonical)
				}
				fv = nil // an absent optional field is none
			}
			hv, err := hostValue(fd.typ, fv)
			if err != nil {
				return value.Undefined(), err
			}
			fields = append(fields, bamlprofile.ClassField{Canonical: fd.canonical, Alias: fd.alias, Value: hv})
		}
		return bamlprofile.ClassValue(fields)
	}
	// Scalar.
	switch typ {
	case "string":
		s, ok := arg.(string)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: string field got %T", arg)
		}
		return value.FromString(s), nil
	case "int":
		i, ok := arg.(int64)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: int field got %T (use int64)", arg)
		}
		return value.FromInt(i), nil
	case "float":
		f, ok := arg.(float64)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: float field got %T (use float64)", arg)
		}
		return value.FromFloat(f), nil
	case "bool":
		b, ok := arg.(bool)
		if !ok {
			return value.Undefined(), fmt.Errorf("profileoracle: bool field got %T", arg)
		}
		return value.FromBool(b), nil
	default:
		return value.Undefined(), fmt.Errorf("profileoracle: unsupported host type %q", typ)
	}
}
