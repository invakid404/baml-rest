package bamlfuzz

import (
	"encoding/json"
	"fmt"
	"sort"
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
// keys are the class's properties, so a flat map[string]json.RawMessage
// decode is sufficient — every mutation kind operates on a top-level
// key.
//
// Decode goes through encoding/json so the JSON wire-order may
// scramble across the mutation; the Axis C oracle compares semantic
// equality only, not source-order, so the reorder is invisible to the
// assertion.
func applyJSONMutation(content json.RawMessage, kind string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(content, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal mock content: %w", err)
	}
	if obj == nil {
		obj = make(map[string]json.RawMessage)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	switch kind {
	case JSONMutationDropKey:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys to drop")
		}
		delete(obj, keys[0])
	case JSONMutationAddUnknownKey:
		obj["_invalid_unknown_key"] = json.RawMessage(`"unknown"`)
	case JSONMutationRetypeScalar:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys to retype")
		}
		obj[keys[0]] = retypeScalarValue(obj[keys[0]])
	case JSONMutationSwapEnum:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys for swap_enum")
		}
		obj[keys[0]] = json.RawMessage(`"_invalid_enum_variant"`)
	case JSONMutationWrapInArray:
		if len(keys) == 0 {
			return nil, fmt.Errorf("no keys for wrap_in_array")
		}
		target := keys[0]
		obj[target] = json.RawMessage("[" + string(obj[target]) + "]")
	default:
		return nil, fmt.Errorf("unknown json mutation %q", kind)
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal mutated mock: %w", err)
	}
	return out, nil
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
