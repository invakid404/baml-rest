package debaml

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/invakid404/baml-rest/internal/schema"
)

// coerce converts an ordered value (decoded strict, or via the
// conservative fixing pass) into the flattened dynamic output JSON for
// type t, returning a json.RawMessage. checkSupported has already rejected
// every out-of-scope kind, so coerce only handles the M2a cut-line; an
// unexpected kind is a claimed coercion error.
//
// Field and enum-value names follow BAML's rendered/canonical split: input
// keys are matched by rendered name (the alias the model is shown), and
// output keys use the canonical name — the form the downstream
// FlattenDynamicOutput / InjectAbsentOptionals / ReorderDynamicOutputBySchema
// pipeline keys on. Class fields are emitted in schema declaration order so
// that order pass remains the authority.
func coerce(b *schema.Bundle, t schema.Type, input value) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		return coercePrimitive(t.Primitive, input)
	case schema.TypeLiteral:
		return coerceLiteral(t.Literal, input)
	case schema.TypeEnum:
		return coerceEnum(b, t.Name, input)
	case schema.TypeClass:
		return coerceClass(b, t.Name, t.Mode, input)
	case schema.TypeList:
		return coerceList(b, t.Elem, input)
	case schema.TypeUnion:
		return coerceUnion(b, t.Union, input)
	default:
		return nil, fmt.Errorf("debaml: cannot coerce type kind %q", t.Kind)
	}
}

func coercePrimitive(p schema.PrimitiveKind, input value) (json.RawMessage, error) {
	switch p {
	case schema.PrimitiveString:
		if input.kind != valString {
			return nil, typeMismatch("string", input)
		}
		return marshalJSON(input.strV)
	case schema.PrimitiveInt:
		if input.kind != valNumber {
			return nil, typeMismatch("int", input)
		}
		if _, err := input.numV.Int64(); err != nil {
			return nil, fmt.Errorf("debaml: %s is not an integer", input.numV.String())
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.PrimitiveFloat:
		if input.kind != valNumber {
			return nil, typeMismatch("float", input)
		}
		if _, err := input.numV.Float64(); err != nil {
			return nil, fmt.Errorf("debaml: %s is not a number", input.numV.String())
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.PrimitiveBool:
		if input.kind != valBool {
			return nil, typeMismatch("bool", input)
		}
		return marshalJSON(input.boolV)
	case schema.PrimitiveNull:
		if input.kind != valNull {
			return nil, typeMismatch("null", input)
		}
		return json.RawMessage("null"), nil
	default:
		return nil, fmt.Errorf("debaml: unsupported primitive %q", p)
	}
}

func coerceLiteral(lit *schema.LiteralValue, input value) (json.RawMessage, error) {
	if lit == nil {
		return nil, fmt.Errorf("debaml: literal type missing value")
	}
	switch lit.Kind {
	case schema.LiteralString:
		if input.kind != valString || input.strV != lit.String {
			return nil, fmt.Errorf("debaml: expected literal string %q", lit.String)
		}
		return marshalJSON(input.strV)
	case schema.LiteralInt:
		if input.kind != valNumber {
			return nil, typeMismatch("literal int", input)
		}
		n, err := input.numV.Int64()
		if err != nil || n != lit.Int {
			return nil, fmt.Errorf("debaml: expected literal int %d", lit.Int)
		}
		return json.RawMessage(input.numV.String()), nil
	case schema.LiteralBool:
		if input.kind != valBool || input.boolV != lit.Bool {
			return nil, fmt.Errorf("debaml: expected literal bool %v", lit.Bool)
		}
		return marshalJSON(input.boolV)
	default:
		return nil, fmt.Errorf("debaml: unknown literal kind %q", lit.Kind)
	}
}

func coerceEnum(b *schema.Bundle, name string, input value) (json.RawMessage, error) {
	if input.kind != valString {
		return nil, typeMismatch("enum", input)
	}
	e, ok := b.FindEnum(name)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown enum %q", name)
	}
	v, ok := e.ValueByRenderedName(input.strV)
	if !ok {
		return nil, fmt.Errorf("debaml: %q is not a value of enum %q", input.strV, name)
	}
	// Emit the canonical enum value name, matching BAML's enum coercion.
	return marshalJSON(v.Name.Name)
}

func coerceClass(b *schema.Bundle, name string, mode schema.StreamingMode, input value) (json.RawMessage, error) {
	if input.kind != valObject {
		return nil, typeMismatch("object", input)
	}
	cls, ok := b.FindClass(name, mode)
	if !ok {
		return nil, fmt.Errorf("debaml: unknown class %q", name)
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i := range cls.Fields {
		f := &cls.Fields[i]
		val, present := lookupField(input.objV, f.Name)
		if !present {
			if isOptional(f.Type) {
				// Absent optional: omit it. The downstream
				// InjectAbsentOptionals pass inserts the null, identically
				// for the native and BAML paths.
				continue
			}
			return nil, fmt.Errorf("debaml: class %q missing required field %q", name, f.Name.RenderedName())
		}
		child, err := coerce(b, f.Type, val)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := marshalJSON(f.Name.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(child)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func coerceList(b *schema.Bundle, elem *schema.Type, input value) (json.RawMessage, error) {
	if elem == nil {
		return nil, fmt.Errorf("debaml: list type missing element")
	}
	if input.kind != valArray {
		return nil, typeMismatch("array", input)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := range input.arrV {
		child, err := coerce(b, *elem, input.arrV[i])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(child)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// coerceUnion handles the only union shape M1/M2a supports: an optional —
// a nullable union with exactly one non-null variant. A JSON null becomes
// null; anything else coerces against the lone variant. checkSupported has
// already rejected every other union, so this is reached only for
// optionals.
func coerceUnion(b *schema.Bundle, u *schema.UnionType, input value) (json.RawMessage, error) {
	if u == nil {
		return nil, fmt.Errorf("debaml: union type missing payload")
	}
	if input.kind == valNull {
		if !u.Nullable {
			return nil, typeMismatch("non-nullable union", input)
		}
		return json.RawMessage("null"), nil
	}
	if len(u.Variants) != 1 {
		return nil, fmt.Errorf("debaml: general union scoring is unsupported")
	}
	return coerce(b, u.Variants[0], input)
}

// lookupField returns the input value for a class field, matched by the
// rendered name ONLY (the alias when present, else the canonical name) —
// the same key BAML's jsonish class coercer matches against
// (name.rendered_name()). An aliased field is therefore NOT matched by its
// canonical name: BAML treats the canonical key as an extra field and the
// rendered key as missing, and the native path must agree to stay
// drift-free. For a field with no alias, RenderedName()==Name, so this is
// unchanged. Duplicate input keys resolve last-wins, matching the
// encoding/json map decode the M1 path used. The comma-ok form
// distinguishes an absent key from a present null value.
func lookupField(obj []field, name schema.Name) (value, bool) {
	rendered := name.RenderedName()
	var found value
	ok := false
	for i := range obj {
		if obj[i].key == rendered {
			found = obj[i].val
			ok = true
		}
	}
	return found, ok
}

// isOptional reports whether t is an optional (a nullable union), the only
// type for which an absent field is tolerated rather than a parse error.
func isOptional(t schema.Type) bool {
	return t.Kind == schema.TypeUnion && t.Union != nil && t.Union.Nullable
}

// marshalJSON encodes v to compact JSON without HTML escaping, so emitted
// strings read naturally. (Comparison against BAML is semantic — both
// sides are decoded before diffing — so escaping never affects parity.)
func marshalJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// typeMismatch reports a conservative JSON-type mismatch for a coercion.
func typeMismatch(want string, input value) error {
	return fmt.Errorf("debaml: expected %s, got %s", want, input.kind.String())
}
