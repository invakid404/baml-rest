package parity

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"io"
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
// SetEscapeHTML(false) at EVERY depth: the native SAP emits strings unescaped
// (coerce.go's marshalJSON), but the production BAML-only callback returns
// encoding/json.Marshal (which escapes `<` `>` `&` to </>/&, codegen_buildrequest.go), so a
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

	case schema.TypeRecursiveAlias:
		// De-BAML Phase 3a: a structural-recursive alias (the served JSON) resolves to a
		// DYNAMIC JSON value — any nesting of scalar / list / map, whose shape is
		// input-driven, not schema-driven. The map materialization contract is
		// insertion-INTERNAL but SORTED-public, so the /call comparator must canonicalize
		// BOTH the native FinalJSON and the BAML-callback bytes to the sorted-public form
		// (recursively sorted map keys) with consistent HTML escaping before the order
		// compare. The prior default case compacted maps in place (insertion order), which
		// is insufficient for an alias that resolves to a map. canonicalizeAliasJSON
		// re-emits with json.Marshal (sorted map keys + HTML escaping) while preserving
		// exact integer number tokens (UseNumber, no float64 round-trip).
		return canonicalizeAliasJSON(data)

	default:
		// Primitives, enums, literals, maps (insertion-order preserved), and TypeTop:
		// compacted, with EVERY string inside re-escaped to native's SetEscapeHTML(false)
		// so an escaping-only difference never reads as a content/order divergence. The
		// de-BAML Slice 7.2b-3 `Checked[T]` carrier lands here: its schema type is the
		// constrained `int` PRIMITIVE while its value is an object whose `expression`
		// string carries the predicate's `>`.
		return canonicalScalar(data)
	}
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// canonicalizeAliasJSON re-emits a dynamic recursive-alias JSON value in the canonical
// SORTED-public form: json.Marshal of the decoded value sorts every (nested) map key
// lexically and HTML-escapes < > & — the exact public byte shape the generated static
// callback produces (json.Marshal on the generated types.JSON union). Decoding with
// UseNumber keeps integer number tokens EXACT (no float64 round-trip that would corrupt
// a large integer), and a value that does not decode as JSON falls back to plain
// compaction. Applied to BOTH the native FinalJSON and the BAML-callback bytes, so an
// (internal) insertion-order difference never reads as an order/content divergence.
func canonicalizeAliasJSON(data []byte) ([]byte, error) {
	dec := stdjson.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return compactJSON(data)
	}
	out, err := stdjson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("parity: canonicalize alias json: %w", err)
	}
	return out, nil
}

// canonicalScalar compacts data, and re-encodes every JSON STRING inside it with
// SetEscapeHTML(false) — matching the native SAP's string emission (coerce.go's
// marshalJSON) so an HTML-metacharacter value (`<` `>` `&`) that the production
// BAML-only callback escaped (encoding/json.Marshal) canonicalizes to the SAME bytes.
//
// It descends into OBJECTS and ARRAYS, and de-BAML Slice 7.2b-3 is why. A constrained
// `int` field's schema type is a PRIMITIVE, but its value is the `Checked[T]` carrier —
// an object whose `expression` string is the predicate source, and every admitted
// predicate contains `>`. Native's sonic bytes carry it raw; the BAML-only callback's
// encoding/json.Marshal escapes it to `>`. Canonicalizing only top-level string
// scalars left that difference inside the carrier, so the order compare failed and the
// route served BAML's parse of the same bytes — native computing the right answer and
// shipping someone else's. Descending fixes it for the carrier and for any other
// composite whose strings differ only in escaping.
//
// Object key ORDER is preserved exactly as it appears (this function normalizes
// escaping, never order — reordering is [reorderStaticByBundle]'s job and is
// schema-driven), and number tokens are copied VERBATIM, so a large integer cannot be
// corrupted by a float64 round trip.
func canonicalScalar(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := canonicalEscapeInto(&buf, stdjson.NewDecoder(bytes.NewReader(data))); err != nil {
		// Not decodable as a single JSON value: fall back to plain compaction, which is
		// what this function did before it descended.
		return compactJSON(data)
	}
	return buf.Bytes(), nil
}

// canonicalEscapeInto copies one JSON value from dec into buf, compacted, with every
// string re-encoded under SetEscapeHTML(false) and every number token verbatim.
func canonicalEscapeInto(buf *bytes.Buffer, dec *stdjson.Decoder) error {
	dec.UseNumber()
	if err := canonicalEscapeValue(buf, dec); err != nil {
		return err
	}
	// Exactly ONE value: trailing content means this was not a single JSON document and
	// the caller must fall back rather than emit a truncated prefix.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("parity: trailing JSON value")
		}
		return err
	}
	return nil
}

func canonicalEscapeValue(buf *bytes.Buffer, dec *stdjson.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return canonicalEscapeToken(buf, dec, tok)
}

func canonicalEscapeToken(buf *bytes.Buffer, dec *stdjson.Decoder, tok stdjson.Token) error {
	switch t := tok.(type) {
	case stdjson.Delim:
		switch t {
		case '{':
			buf.WriteByte('{')
			first := true
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyTok.(string)
				if !ok {
					return fmt.Errorf("parity: non-string object key")
				}
				if !first {
					buf.WriteByte(',')
				}
				first = false
				if err := writeCanonicalString(buf, key); err != nil {
					return err
				}
				buf.WriteByte(':')
				if err := canonicalEscapeValue(buf, dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // the closing '}'
				return err
			}
			buf.WriteByte('}')
			return nil
		case '[':
			buf.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					buf.WriteByte(',')
				}
				first = false
				if err := canonicalEscapeValue(buf, dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // the closing ']'
				return err
			}
			buf.WriteByte(']')
			return nil
		default:
			return fmt.Errorf("parity: unexpected JSON delimiter %q", t)
		}
	case string:
		return writeCanonicalString(buf, t)
	case stdjson.Number:
		// VERBATIM: the decoded token text, never a re-formatted float.
		buf.WriteString(t.String())
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case nil:
		buf.WriteString("null")
		return nil
	default:
		return fmt.Errorf("parity: unexpected JSON token %T", tok)
	}
}

func writeCanonicalString(buf *bytes.Buffer, s string) error {
	var enc bytes.Buffer
	e := stdjson.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(enc.Bytes(), "\n"))
	return nil
}

func compactJSON(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := stdjson.Compact(&buf, data); err != nil {
		return nil, fmt.Errorf("parity: compact static json: %w", err)
	}
	return buf.Bytes(), nil
}
