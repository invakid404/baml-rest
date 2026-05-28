package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

// applyInvalidSchemaMutation replaces the first root property of `out`
// with a tailored-but-invalid DynamicProperty matching `kind`. The
// other properties and any nested classes/enums stay untouched so the
// resulting schema is "almost valid" with one localized fault — the
// shape the rapid shrinker can isolate to a minimal repro.
func applyInvalidSchemaMutation(out *bamlutils.DynamicOutputSchema, kind string) error {
	keys := out.Properties.Keys()
	if len(keys) == 0 {
		return fmt.Errorf("output schema has no properties to mutate")
	}
	target := keys[0]
	prop, err := buildInvalidProperty(kind)
	if err != nil {
		return err
	}
	if err := out.Properties.Replace(target, prop); err != nil {
		return fmt.Errorf("replace property %q: %w", target, err)
	}
	return nil
}

// buildInvalidProperty returns a DynamicProperty whose shape violates
// DynamicTypes.Validate (interfaces.go:688), per the mutation kind.
// Each shape exercises one rejection path:
//
//   - both_type_and_ref: validateTypeRef rejects properties carrying both
//     "type" and "ref" (interfaces.go:754).
//   - misspelled_type: an unknown type string lands on the default arm
//     (interfaces.go:832).
//   - drop_items / drop_inner / drop_keys_values / drop_oneof: each
//     composite kind requires its companion field (interfaces.go:772+).
//   - over_depth: 70 nested optional wrappers exceeds maxTypeDepth (64).
func buildInvalidProperty(kind string) (*bamlutils.DynamicProperty, error) {
	switch kind {
	case MutationBothTypeAndRef:
		return &bamlutils.DynamicProperty{Type: "string", Ref: "Nonexistent"}, nil
	case MutationMisspelledType:
		return &bamlutils.DynamicProperty{Type: "strin"}, nil
	case MutationDropItems:
		return &bamlutils.DynamicProperty{Type: "list"}, nil
	case MutationDropInner:
		return &bamlutils.DynamicProperty{Type: "optional"}, nil
	case MutationDropKeysValues:
		return &bamlutils.DynamicProperty{Type: "map"}, nil
	case MutationDropOneOf:
		return &bamlutils.DynamicProperty{Type: "union"}, nil
	case MutationOverDepth:
		inner := &bamlutils.DynamicTypeSpec{Type: "string"}
		for i := 0; i < 70; i++ {
			inner = &bamlutils.DynamicTypeSpec{Type: "optional", Inner: inner}
		}
		return &bamlutils.DynamicProperty{Type: "optional", Inner: inner}, nil
	}
	return nil, fmt.Errorf("unknown invalid schema mutation %q", kind)
}

// applyJSONMutation perturbs the mock LLM JSON response with one
// targeted edit. Returns the mutated bytes. The mock content the
// walker emits at the dynamic-safe root is always a JSON object whose
// keys are the class's properties; every mutation operates on the
// top-level wire shape.
//
// Decode/re-encode goes through bamlutils.OrderedMap so wire-order is
// preserved across the mutation. The Axis C order oracle compares
// dynclient vs REST key order at every class node via
// SchemaOrderDiffWithChoices, so scrambling the mock's input order
// here would corrupt the assertion's expected behaviour: a mock with
// keys [a, b, c] must come out of the mutation in the same [a, b, c]
// order modulo the specific key the mutation touched (drop_key
// removes one in place; retype/swap_enum/wrap_in_array replace one
// in place; add_unknown_key appends at the end).
//
// `schema` is consulted by schema-aware mutations: JSONMutationSwapEnum
// scans the root class for a property of enum type so the replacement
// actually exercises an enum-coercion code path; a generic first-field
// replacement would only swap a string for a string when the first
// field is non-enum, defeating the mutation's intent. When no enum
// field exists in the schema the mutation returns an error and the
// caller's fallback (typically JSONMutationAddUnknownKey) takes over.
func applyJSONMutation(schema FuzzSchema, content json.RawMessage, kind string) (json.RawMessage, error) {
	var obj bamlutils.OrderedMap[json.RawMessage]
	if err := obj.UnmarshalJSON(content); err != nil {
		return nil, fmt.Errorf("unmarshal mock content: %w", err)
	}
	keys := obj.Keys()

	switch kind {
	case JSONMutationDropKey:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys to drop")
		}
		obj.Delete(keys[0])
	case JSONMutationAddUnknownKey:
		// Set returns an error on duplicate key. The reserved
		// "_invalid_unknown_key" name is namespaced so it cannot
		// collide with the generator's Fuzz_field_N convention; a
		// duplicate here would mean a caller wired the same case
		// twice, which is an integrity bug worth surfacing.
		if err := obj.Set("_invalid_unknown_key", json.RawMessage(`"unknown"`)); err != nil {
			return nil, fmt.Errorf("add_unknown_key: %w", err)
		}
	case JSONMutationRetypeScalar:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys to retype")
		}
		cur, _ := obj.Get(keys[0])
		if err := obj.Replace(keys[0], retypeScalarValue(cur)); err != nil {
			return nil, fmt.Errorf("retype_scalar: %w", err)
		}
	case JSONMutationSwapEnum:
		target, ok := findRootEnumFieldName(schema)
		if !ok {
			return nil, fmt.Errorf("swap_enum: schema has no root-class enum field")
		}
		if !obj.Has(target) {
			// The walker is supposed to render every root-class field
			// into the mock; a missing key here would be a generator
			// integrity bug. Surface as a hard error so the caller's
			// fallback handles it deterministically.
			return nil, fmt.Errorf("swap_enum: enum field %q absent from mock content", target)
		}
		if err := obj.Replace(target, json.RawMessage(`"_invalid_enum_variant"`)); err != nil {
			return nil, fmt.Errorf("swap_enum: %w", err)
		}
	case JSONMutationWrapInArray:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys for wrap_in_array")
		}
		cur, _ := obj.Get(keys[0])
		wrapped := json.RawMessage("[" + string(cur) + "]")
		if err := obj.Replace(keys[0], wrapped); err != nil {
			return nil, fmt.Errorf("wrap_in_array: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown json mutation %q", kind)
	}

	out, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal mutated mock: %w", err)
	}
	return out, nil
}

// findRootEnumFieldName scans the root class for a property typed as
// an enum reference. Walks through KindOptional and KindUnion
// wrappers so an optional<enum> or a union arm naming an enum still
// counts. Returns the property name plus a found flag.
func findRootEnumFieldName(schema FuzzSchema) (string, bool) {
	rootCls, ok := schema.FindClass(schema.RootClass)
	if !ok {
		return "", false
	}
	for _, prop := range rootCls.Properties {
		if typeReachesEnum(prop.Type) {
			return prop.Name, true
		}
	}
	return "", false
}

func typeReachesEnum(t FuzzType) bool {
	switch t.Kind {
	case KindEnumRef:
		return true
	case KindOptional:
		if t.Inner != nil {
			return typeReachesEnum(*t.Inner)
		}
	case KindUnion:
		for _, v := range t.Variants {
			if typeReachesEnum(v) {
				return true
			}
		}
	}
	return false
}

// retypeScalarValue produces a value of a different JSON type than
// `raw`. Dispatched on the first non-whitespace byte: bool → string,
// string → number, number → string, otherwise number. Composite values
// (arrays, objects, null) collapse to a number — the goal is "wrong
// type at this slot", not a faithful type-swap matrix.
func retypeScalarValue(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "true" || s == "false":
		return json.RawMessage(`"yes"`)
	case len(s) > 0 && s[0] == '"':
		return json.RawMessage("42")
	case len(s) > 0 && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9')):
		return json.RawMessage(`"twelve"`)
	default:
		return json.RawMessage("42")
	}
}
