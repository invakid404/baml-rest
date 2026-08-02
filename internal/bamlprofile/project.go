package bamlprofile

import (
	"fmt"

	"github.com/invakid404/minijinja-go/v2/value"
)

// This file is the CONSTRAINT-side lowering of a BAML value — the half of BAML's
// host model that is NOT the prompt host model in enum.go/class.go/list.go.
//
// BAML has two different lowerings of the same BamlValue and uses each in exactly
// one place:
//
//   - the PROMPT renderer lowers Enum/Class/List into custom MiniJinja host
//     objects carrying aliases and Rust-debug rendering
//     (jinja-runtime/src/baml_value_to_jinja_value.rs:19-80). That is PR-2's
//     enumMember / classObject / listObject.
//   - the CONSTRAINT evaluator does NOT. evaluate_predicate binds
//     `Value::from_serialize(this)` (baml-core/src/ir/jinja_helpers.rs:83-89),
//     which goes through BamlValue's serde impl
//     (baml-types/src/baml_value.rs:41-57):
//
//     BamlValue::Enum(_, v)  => serialize_str(v)   // CANONICAL variant, no alias
//     BamlValue::Class(_, m) => m.serialize(..)    // canonical-key ordered map
//     BamlValue::List(l)     => l.serialize(..)    // ordinary sequence
//     BamlValue::Null        => serialize_none()
//     scalars                => themselves
//
// So in a predicate an enum IS its canonical string, a class IS a plain mapping
// keyed by canonical field names, and neither carries a prompt alias or the
// hand-written debug rendering. `this|string`, `this|pprint`, `this == "RED"` and
// `this.aliasKey` all answer differently under the two lowerings, which is why
// the projection exists rather than binding the PR-2 object directly.
//
// PR-3 accepts the PR-2 host values at its public boundary (they are the resolved
// values a later slice already knows how to build) and projects them here, once,
// before binding `this`.

// projectConstraintThis lowers a bamlprofile host value into the shape BAML's
// serde projection produces, for binding as a constraint's `this`.
//
// It is deliberately DECLINE-BY-DEFAULT. Only the shapes PR-2 proved and BAML's
// serde impl covers are accepted:
//
//   - none stays none (serialize_none);
//   - a string/number/bool scalar is retained verbatim;
//   - an enumMember becomes its CANONICAL variant string — never the alias;
//   - a classObject becomes a fork ORDERED map keyed by canonical field names,
//     with recursively projected values. The ordered map (not a Go map) is what
//     preserves BAML's insertion-ordered BamlMap, which an iteration-sensitive
//     predicate (`this|list`, `this|dictsort`, `for k in this`) observes;
//   - a listObject becomes an ordinary fork slice of recursively projected items.
//
// Everything else FAILS CLOSED with an error: an undefined value, a media value,
// a native fork container passed as `This`, and any unrecognized object. Binding
// a prompt host object directly would be the exact out-do the parity-decline rule
// forbids — the predicate would see an alias, an alternate key, or the pretty
// debug rendering that BAML's constraint evaluator never shows it.
func projectConstraintThis(v value.Value) (value.Value, error) {
	if v.IsUndefined() {
		// Undefined is not a BamlValue at all; BAML's `this` is always a real
		// value. There is no serde projection to reproduce, so decline.
		return value.Undefined(), fmt.Errorf("value is undefined")
	}
	if v.IsNone() {
		return v, nil
	}
	if obj, ok := v.AsObject(); ok {
		switch o := obj.(type) {
		case *enumMember:
			// BamlValue::Enum(_, v) => serializer.serialize_str(v): the CANONICAL
			// variant name. The prompt display alias (o.display()) is deliberately
			// NOT used — `this == "rouge"` must be false where `this == "RED"` is
			// true, even for an @alias("rouge") variant.
			return value.FromString(o.canonical), nil
		case *classObject:
			// BamlValue::Class(_, m) => m.serialize(..) over a BamlMap keyed by
			// CANONICAL field names. The alias projection classObject carries for
			// rendering has no counterpart here: `this.key1` is undefined where
			// `this.prop1` is the value.
			m := value.NewOrderedMap(len(o.fields))
			for _, f := range o.fields {
				pv, err := projectConstraintThis(f.value)
				if err != nil {
					return value.Undefined(), fmt.Errorf("field %q: %w", f.canonical, err)
				}
				m.Set(f.canonical, pv)
			}
			return value.FromOrderedMap(m), nil
		case *listObject:
			// BamlValue::List(l) => l.serialize(..): an ordinary sequence. The host
			// list's compact/alternate debug rendering is prompt-only.
			items := make([]value.Value, len(o.items))
			for i, it := range o.items {
				pv, err := projectConstraintThis(it)
				if err != nil {
					return value.Undefined(), fmt.Errorf("item %d: %w", i, err)
				}
				items[i] = pv
			}
			return value.FromSlice(items), nil
		default:
			// A media value, an external fork object, or any future host type whose
			// constraint ingress has not been differentially proven. Decline rather
			// than bind the object and hope its render happens to match.
			return value.Undefined(), fmt.Errorf("unsupported host object type %T", obj)
		}
	}
	switch v.Kind() {
	case value.KindString, value.KindNumber, value.KindBool:
		// BamlValue::String/Int/Float/Bool serialize as themselves. This is the same
		// accepted scalar set assertHostRenderable admits into a host class/list, so
		// a value that PR-2 can hold is a value PR-3 can project.
		return v, nil
	default:
		// A native fork sequence/mapping/bytes/callable reaching `This` means the
		// caller bypassed the host constructors; BAML's `this` is always a lowered
		// BamlValue, so there is nothing faithful to project.
		return value.Undefined(), fmt.Errorf("value must be a scalar, none, or a bamlprofile host value, got kind %v", v.Kind())
	}
}
