package parity

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"sort"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 8C — static Bundle-aware structured/order comparator.
//
// CompareStaticStructured is the static twin of [CompareStructured]: it compares
// the native static SAP flattened JSON against the same-response BAML parse
// flattened JSON, but reorders both to the Return BUNDLE's class-field order
// (rather than a DynamicOutputSchema's) so a class field reorder is caught while a
// content-equal difference is not misread as a drift. The admitted static return
// set carries no optionals, so no absent-optional injection is needed (unlike the
// dynamic comparator); a widened set that adds optionals would extend this the same
// way InjectAbsentOptionals does for dynamic.
//
// Both inputs are SENSITIVE parsed provider outputs (the model's full structured
// response); they are NEVER surfaced/logged/returned. Only the BOUNDED booleans —
// structuredMatch / orderMatch — leave this function.
func CompareStaticStructured(nativeFlat, bamlFlat []byte, bundle *schema.Bundle) (structuredMatch, orderMatch bool) {
	structuredMatch = jsonSemEqual(nativeFlat, bamlFlat)
	if bundle == nil {
		// No Bundle to normalize order against: fall back to byte equality, which is
		// conservative (an incidental key-order difference reads as an order mismatch).
		return structuredMatch, bytes.Equal(nativeFlat, bamlFlat)
	}
	nOrd, e1 := reorderStaticByBundle(nativeFlat, bundle.Target, bundle)
	bOrd, e2 := reorderStaticByBundle(bamlFlat, bundle.Target, bundle)
	if e1 != nil || e2 != nil {
		return structuredMatch, false
	}
	orderMatch = bytes.Equal(nOrd, bOrd)
	return structuredMatch, orderMatch
}

// reorderStaticByBundle re-emits data with object keys canonicalized to the Bundle's
// class-field order (recursing through classes, lists, and the direct nullable-class
// edge), so an order-only diff is normalized away and a residual byte diff is a real
// content/order divergence. It ALSO canonicalizes string-scalar ESCAPING to
// SetEscapeHTML(false): the native SAP emits strings unescaped (coerce.go's
// marshalJSON), but the production BAML-only callback returns encoding/json.Marshal
// (which escapes `<` `>` `&` to </>/&, codegen_buildrequest.go), so a
// value like `<tag> &` would otherwise byte-differ and force a false order mismatch →
// a silent BAML-parse winner even though the two parses are identical. Canonicalizing
// both sides to native's escaping makes the admitted family serve native for
// HTML-metacharacter values too. Maps preserve insertion order (never reordered) and
// other kinds pass through compacted. A null / non-object where a class is expected
// passes through unchanged (nullable targets).
func reorderStaticByBundle(data []byte, t schema.Type, bundle *schema.Bundle) ([]byte, error) {
	switch t.Kind {
	case schema.TypeClass:
		if isJSONNull(data) {
			return compactJSON(data)
		}
		var obj map[string]stdjson.RawMessage
		if err := stdjson.Unmarshal(data, &obj); err != nil {
			// Not an object (a coercion produced a scalar/array where a class was
			// declared): pass through compacted rather than fail the whole compare.
			return compactJSON(data)
		}
		cd, ok := bundle.FindClass(t.Name, t.Mode)
		if !ok {
			return compactJSON(data)
		}
		var buf bytes.Buffer
		buf.WriteByte('{')
		first := true
		writeField := func(key string, raw stdjson.RawMessage, ft schema.Type) error {
			rv, err := reorderStaticByBundle(raw, ft, bundle)
			if err != nil {
				return err
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			kb, err := stdjson.Marshal(key)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			buf.Write(rv)
			return nil
		}
		for i := range cd.Fields {
			key := cd.Fields[i].Name.RenderedName()
			raw, present := obj[key]
			if !present {
				continue
			}
			if err := writeField(key, raw, cd.Fields[i].Type); err != nil {
				return nil, err
			}
			delete(obj, key)
		}
		// Defensive: any keys not declared by the class (should not happen for the
		// admitted set) are appended in sorted order so the output stays deterministic.
		if len(obj) > 0 {
			extra := make([]string, 0, len(obj))
			for k := range obj {
				extra = append(extra, k)
			}
			sort.Strings(extra)
			for _, k := range extra {
				cv, err := compactJSON(obj[k])
				if err != nil {
					return nil, err
				}
				if err := writeField(k, cv, schema.Type{Kind: schema.TypeTop}); err != nil {
					return nil, err
				}
			}
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case schema.TypeList:
		if isJSONNull(data) {
			return compactJSON(data)
		}
		var arr []stdjson.RawMessage
		if err := stdjson.Unmarshal(data, &arr); err != nil {
			return compactJSON(data)
		}
		elem := schema.Type{Kind: schema.TypeTop}
		if t.Elem != nil {
			elem = *t.Elem
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range arr {
			rv, err := reorderStaticByBundle(e, elem, bundle)
			if err != nil {
				return nil, err
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(rv)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil

	case schema.TypeUnion:
		// The admitted direct nullable-class edge (`Node?` / `B?` / `A?`): a null stays
		// null; a present non-null value recurses into the lone class variant so a deep
		// child class is reordered AND its string values escape-canonicalized. Any other
		// union passes through canonicalized (native/BAML emit them in the same shape).
		if t.Union != nil && t.Union.Nullable && len(t.Union.Variants) == 1 && !isJSONNull(data) {
			return reorderStaticByBundle(data, t.Union.Variants[0], bundle)
		}
		return canonicalScalar(data)

	default:
		// Primitives, enums, literals, maps (insertion-order preserved), and TypeTop:
		// compacted, with string scalars re-escaped to native's SetEscapeHTML(false) so
		// an escaping-only difference never reads as a content/order divergence.
		return canonicalScalar(data)
	}
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// canonicalScalar compacts data, and for a JSON STRING re-encodes it with
// SetEscapeHTML(false) — matching the native SAP's string emission (coerce.go's
// marshalJSON) so an HTML-metacharacter value (`<` `>` `&`) that the production
// BAML-only callback escaped (encoding/json.Marshal) canonicalizes to the SAME bytes.
// Non-string scalars fall back to plain compaction (their native/BAML forms already
// agree).
func canonicalScalar(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := stdjson.Unmarshal(trimmed, &s); err == nil {
			var buf bytes.Buffer
			enc := stdjson.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(s); err == nil {
				return bytes.TrimRight(buf.Bytes(), "\n"), nil
			}
		}
	}
	return compactJSON(data)
}

func compactJSON(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := stdjson.Compact(&buf, data); err != nil {
		return nil, fmt.Errorf("parity: compact static json: %w", err)
	}
	return buf.Bytes(), nil
}
