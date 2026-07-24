package debaml

import (
	"bytes"
	"encoding/json"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 2 (recursive classes) — static absent-optional normalizer.
//
// BAML's Go generator lowers an optional `Node?` field to a `*Node` POINTER
// (engine/generators/languages/go/src/type.rs), so a TERMINAL written as an OMITTED
// optional decodes to a nil pointer and BAML's Parse marshals it as an explicit JSON
// null — `{"value":"x","next":null}`. Native coerce instead OMITS an absent optional
// from its object output (coerce.go's default/missing pass records the absent
// OptionalDefaultFromNoValue but emits no field), so it would return
// `{"value":"x"}` for the same OMITTED terminal — a BYTE difference even though
// DecodeStaticFinal produces the same nil pointer. Relaxing the comparator would not
// meet a byte-exact claim; the native FinalJSON bytes must match.
//
// normalizeRecursiveFinal is the narrow, Bundle-owned fix applied ONLY to the
// admitted recursive-class family, immediately after coerce succeeds in
// [ParseStaticBundle]. For a present class object it re-emits the fields in SCHEMA
// field order, preserving an explicit null, INSERTING null for the one admitted
// direct nullable-class edge when it is ABSENT, and recursing into a non-null child.
// It is NOT a general union/alias injector: it only touches admitted classes and the
// admitted nullable-class edge, so both the explicit-`next:null` and the omitted-`next`
// raw forms converge on the same canonical bytes `{"value":"terminal","next":null}`.
func normalizeRecursiveFinal(data json.RawMessage, t schema.Type, b *schema.Bundle, prof recProfile) (json.RawMessage, error) {
	switch t.Kind {
	case schema.TypeClass:
		if isNormJSONNull(data) {
			// A nullable-class edge that coerced to null reaches here as the class
			// variant; keep the null (the union case already handled it, but be safe).
			return data, nil
		}
		cd, ok := b.FindClass(t.Name, t.Mode)
		if !ok {
			return data, nil
		}
		// Only normalize admitted SCC classes; a non-SCC class in the graph (there is
		// none in the family) passes through unchanged.
		if !prof.sccKeys[classKeyOf(t)] {
			return data, nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			// Not an object (a coercion produced a scalar/array where a class was
			// declared): pass through rather than fail the whole parse.
			return data, nil
		}
		var buf bytes.Buffer
		buf.WriteByte('{')
		first := true
		for i := range cd.Fields {
			ft := cd.Fields[i].Type
			key := cd.Fields[i].Name.RenderedName()
			raw, present := obj[key]
			var fieldOut json.RawMessage
			switch {
			case present:
				nv, err := normalizeRecursiveFinal(raw, ft, b, prof)
				if err != nil {
					return nil, err
				}
				fieldOut = nv
			case isAdmittedRecursiveEdge(ft, prof):
				// The absent admitted direct nullable-class edge: BAML marshals its nil
				// pointer as an explicit null. Insert it.
				fieldOut = json.RawMessage("null")
			default:
				// A non-edge field absent from a present class object cannot happen for
				// the admitted family (the scalar value field is required and coerce
				// would have errored). Skip defensively rather than invent a value.
				continue
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			kb, err := marshalJSON(key) // same compact, no-HTML-escape emit coerceClass uses
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			buf.Write(fieldOut)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case schema.TypeUnion:
		// The admitted direct nullable-class edge. A null terminal stays null; a
		// present non-null child recurses into the SCC class variant.
		if isNormJSONNull(data) {
			return data, nil
		}
		if isAdmittedRecursiveEdge(t, prof) {
			return normalizeRecursiveFinal(data, t.Union.Variants[0], b, prof)
		}
		return data, nil

	default:
		// Scalars (the value field) pass through unchanged.
		return data, nil
	}
}

func isNormJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
