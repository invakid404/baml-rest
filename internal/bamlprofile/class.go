package bamlprofile

import (
	"fmt"
	"strings"

	"github.com/invakid404/minijinja-go/v2/value"
)

// This file is BAML v0.223's class host value model: the map object a
// BamlValue::Class lowers to, plus the ONE hand-written Rust-debug walker
// (debugValue/debugClass/debugList) that renders both classes and listObjects,
// in either the alternate `{:#?}` or the non-alternate spelling.
//
// Authority (byte-for-byte): BAML v0.223
//   - class lowering + object impl + Display/serialize:
//     engine/baml-lib/jinja-runtime/src/baml_value_to_jinja_value.rs:60-80,255-334
//   - concrete expected direct-render bytes:
//     engine/baml-lib/jinja-runtime/src/lib.rs:1631,1888 (single field and
//     nested class/list goldens, reproduced by the pretty renderer here).
//
// The load-bearing split (rs:60-80,320-334): a class stores its fields under
// their CANONICAL names and, separately, a canonical->alias map. Attribute
// access, iteration, length and membership use the CANONICAL names; only DIRECT
// rendering and the serialization projection use the aliases. So `c.original`
// works even when the field is @alias("wire"), while `{{ c }}` prints "wire".

// classField is one class field in stored (declared) order.
type classField struct {
	canonical string      // the attribute/iteration/index name
	aliasKey  string      // resolved alias, or the canonical name when unaliased; used only in rendering
	value     value.Value // the field value (a scalar, none, or a host value)
}

// ClassField is one resolved class field: its canonical name, optional resolved
// alias (nil = unaliased, so the display key is the canonical name), and value.
// Values must be a scalar, none, or a bamlprofile host value (enum/class/list);
// a native fork container is rejected because it would not render like BAML's
// lowered host value.
type ClassField struct {
	Canonical string
	Alias     *string
	Value     value.Value
}

// classObject is BAML's MinijinjaBamlClass (rs:255-334): a Map-repr object keyed
// by canonical field names, carrying the alias projection used only for display
// and serialization.
type classObject struct {
	fields []classField           // ordered; drives iteration, length, and rendering order
	byName map[string]value.Value // canonical -> value, for attribute/index access
}

// ClassValue constructs a host class value (BAML's BamlValue::Class lowering).
// It fails loud on malformed metadata — an empty or duplicate canonical field
// name, or a field value that is neither a scalar, none, nor a bamlprofile host
// value — rather than building an object that renders or iterates wrong.
func ClassValue(fields []ClassField) (value.Value, error) {
	obj := &classObject{
		fields: make([]classField, 0, len(fields)),
		byName: make(map[string]value.Value, len(fields)),
	}
	for _, f := range fields {
		if f.Canonical == "" {
			return value.Undefined(), fmt.Errorf("bamlprofile: class has a field with an empty canonical name")
		}
		if _, dup := obj.byName[f.Canonical]; dup {
			return value.Undefined(), fmt.Errorf("bamlprofile: class has duplicate field %q", f.Canonical)
		}
		if err := assertHostRenderable(f.Value); err != nil {
			return value.Undefined(), fmt.Errorf("bamlprofile: class field %q: %w", f.Canonical, err)
		}
		aliasKey := f.Canonical
		if f.Alias != nil {
			aliasKey = *f.Alias
		}
		obj.fields = append(obj.fields, classField{canonical: f.Canonical, aliasKey: aliasKey, value: f.Value})
		obj.byName[f.Canonical] = f.Value
	}
	return value.FromObject(obj), nil
}

// GetAttr looks a field up by its CANONICAL name (rs:322-328). An alias is not
// an alternate attribute, and a missing field is undefined — so `c.wire_alias`
// and `c.absent` are both undefined while `c.canonical` returns the value
// (including a stored none, which is present, not undefined).
func (c *classObject) GetAttr(name string) value.Value {
	if v, ok := c.byName[name]; ok {
		return v
	}
	return value.Undefined()
}

// Keys returns the canonical field names in stored order, driving `for k in c`,
// `|length`, and truthiness (empty class falsey, nonempty truthy) — BAML's
// Enumerator::Values over the class keys (rs:329-334).
func (c *classObject) Keys() []string {
	keys := make([]string, len(c.fields))
	for i, f := range c.fields {
		keys[i] = f.canonical
	}
	return keys
}

// ObjectRepr is Map (rs:257-259).
func (c *classObject) ObjectRepr() value.ObjectRepr { return value.ObjectReprMap }

// ObjectString renders the class as Rust's pretty debug-map with aliased keys
// and nested none as null (BAML's Display, rs:260-283). A class is ALWAYS
// alternate, whatever the surrounding context — Display forces `{map:#?}`
// (rs:279) — so it enters the walker in debugAlternate at indent 0.
func (c *classObject) ObjectString() string { return debugClass(c, 0) }

// prettyIndentWidth is the four-space nesting Rust's alternate debug ({:#?})
// uses; BAML renders classes/lists with `{map:#?}` / debug_list (rs:279,373).
const prettyIndentWidth = 4

// debugMode selects which Rust debug formatting a host value renders with:
// non-alternate `{:?}` or alternate `{:#?}`. It is the Go stand-in for the Rust
// formatter's ambient alternate flag, which BAML's Display impls read
// (rs:373-388 for a list) or force (rs:279 for a class).
type debugMode int

const (
	// debugCompact is Rust's `{:?}`: a list is one line, `[a, b]`, comma-space
	// separated with no trailing comma. This is the ambient mode for a
	// directly-rendered host list.
	debugCompact debugMode = iota
	// debugAlternate is Rust's `{:#?}`: multi-line, four-space nesting, one entry
	// per line, each with a trailing comma. This is the ambient mode inside a
	// class, which forces it.
	debugAlternate
)

// debugValue renders one host value the way it appears inside a host class or
// list, in the given mode at the given base indent. It is the SINGLE walker for
// both modes: the compact and alternate spellings differ only in the list
// layout, so keeping one object switch keeps the two from drifting.
//
//   - none -> "null" (BAML swaps a nested none for BamlNull, whose render is
//     "null"; rs:270-277,383-386);
//   - a nested class -> ALWAYS the alternate debug-map, in either mode: BAML's
//     class Display forces `{map:#?}` (rs:279), so a class nested in a compact
//     list is still multi-line;
//   - a nested list -> the CURRENT mode: the alternate flag propagates through a
//     Rust debug_list, and its absence propagates too, so a list inside a
//     compact list stays compact and one inside a class goes alternate;
//   - an enum member -> its BARE alias-or-canonical display (its render is
//     Display, not a quoted string; rs:181-186);
//   - a scalar -> the fork's Repr, which is Rust's Debug for that scalar
//     (QuoteDebug for strings, %d for ints, Rust f64 debug for floats,
//     true/false for bools).
//
// The default arms panic: ClassValue/ListValue validate every stored value with
// assertHostRenderable and store it in an owned container, so no value that
// reaches here can be anything else. Reaching them is an internal invariant
// violation, and failing loud beats emitting a degraded representation.
func debugValue(v value.Value, mode debugMode, indent int) string {
	if v.IsNone() {
		return "null"
	}
	if obj, ok := v.AsObject(); ok {
		switch o := obj.(type) {
		case *classObject:
			return debugClass(o, indent)
		case *listObject:
			return debugList(o, mode, indent)
		case *enumMember:
			return o.display()
		default:
			panic(fmt.Sprintf("bamlprofile: internal error: cannot render host object of type %T inside a host class/list", obj))
		}
	}
	switch v.Kind() {
	case value.KindString, value.KindNumber, value.KindBool:
		return v.Repr()
	default:
		panic(fmt.Sprintf("bamlprofile: internal error: cannot render value of kind %v inside a host class/list", v.Kind()))
	}
}

// debugClass renders a class as Rust's alternate debug-map at the given base
// indent (the column its closing brace aligns to). An empty map is `{}`;
// otherwise each entry is `<indent+4>"aliasKey": <value>,` with a trailing comma
// and the closing brace at the base indent. Its values are walked in
// debugAlternate because the class forces `{map:#?}`. Verified against BAML's
// goldens (lib.rs:1631 single field, lib.rs:1888 nested class+list).
func debugClass(c *classObject, indent int) string {
	if len(c.fields) == 0 {
		return "{}"
	}
	var b strings.Builder
	inner := indent + prettyIndentWidth
	pad := strings.Repeat(" ", inner)
	b.WriteString("{\n")
	for _, f := range c.fields {
		b.WriteString(pad)
		b.WriteString(debugString(f.aliasKey))
		b.WriteString(": ")
		b.WriteString(debugValue(f.value, debugAlternate, inner))
		b.WriteString(",\n")
	}
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString("}")
	return b.String()
}

// debugList renders a list as Rust's debug_list in the given mode (rs:373-388,
// which uses the AMBIENT formatter rather than forcing alternate the way a class
// does). An empty list is `[]` in both modes. A compact list has no indentation
// of its own, so its items are walked at indent 0.
func debugList(l *listObject, mode debugMode, indent int) string {
	if len(l.items) == 0 {
		return "[]"
	}
	if mode == debugCompact {
		parts := make([]string, len(l.items))
		for i, item := range l.items {
			parts[i] = debugValue(item, debugCompact, 0)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	var b strings.Builder
	inner := indent + prettyIndentWidth
	pad := strings.Repeat(" ", inner)
	b.WriteString("[\n")
	for _, item := range l.items {
		b.WriteString(pad)
		b.WriteString(debugValue(item, debugAlternate, inner))
		b.WriteString(",\n")
	}
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString("]")
	return b.String()
}

// assertHostRenderable is the construction-time guard behind debugValue's
// panics: a class/list value must be a scalar, none, or a bamlprofile host
// value. A native fork container (or any other object) is rejected — it would
// render compactly via the fork's Repr instead of BAML's pretty host rendering,
// a silent divergence.
func assertHostRenderable(v value.Value) error {
	if v.IsNone() {
		return nil
	}
	if v.IsUndefined() {
		return fmt.Errorf("value is undefined")
	}
	if obj, ok := v.AsObject(); ok {
		switch obj.(type) {
		case *classObject, *listObject, *enumMember:
			return nil
		default:
			return fmt.Errorf("unsupported host object type %T", obj)
		}
	}
	switch v.Kind() {
	case value.KindString, value.KindNumber, value.KindBool:
		return nil
	default:
		return fmt.Errorf("value must be a scalar, none, or a bamlprofile host value, got kind %v", v.Kind())
	}
}

// debugString renders s the way Rust's `Debug for str` does (the fork's Repr for
// a string, i.e. QuoteDebug), so class map keys are quoted and escaped exactly
// as `{map:#?}` prints them.
func debugString(s string) string {
	return value.FromString(s).Repr()
}
