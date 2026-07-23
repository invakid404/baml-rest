package parity

import (
	"bytes"
	stdjson "encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

func nodeBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	d := schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticRecursiveNode",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "Node", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "Node"}, Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "value"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "next"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypeUnion, Union: &schemadescriptor.UnionType{
					Nullable: true, Variants: []schemadescriptor.Type{{Kind: schemadescriptor.TypeClass, Name: "Node", Mode: schemadescriptor.NonStreaming}},
				}}},
			},
		}},
		RecursiveClasses: []string{"Node"},
	}
	b, err := schema.FromStaticDescriptor(d)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	return b
}

// TestCompareStaticStructured_EscapingCanonicalized proves the comparator does NOT
// read an escaping-only difference as an order mismatch: native's SetEscapeHTML(false)
// bytes (literal `<` `>` `&`) and the production BAML-only callback's
// encoding/json.Marshal bytes (`<` etc.) order-MATCH after canonicalization, both
// at the top level and through the recursive nullable-class edge — so the admitted
// family serves native for HTML-metacharacter values instead of silently falling back
// to the BAML parse winner.
func TestCompareStaticStructured_EscapingCanonicalized(t *testing.T) {
	b := nodeBundle(t)

	// A Node-shaped carrier so both legs are computed FROM THE SAME VALUE, differing
	// ONLY in escaping (not hand-typed literals that could accidentally be identical).
	type nodeT struct {
		Value string `json:"value"`
		Next  *nodeT `json:"next"`
	}
	// baml() = the EXACT bytes the production BAML-only callback emits — encoding/json.
	// Marshal, which HTML-escapes `<` `>` `&` to `<` `>` `&`.
	baml := func(v *nodeT) []byte {
		out, err := stdjson.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return out
	}
	// native() = the native SAP's emission — SetEscapeHTML(false), so `<` `>` `&` stay literal.
	native := func(v *nodeT) []byte {
		var buf bytes.Buffer
		enc := stdjson.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return bytes.TrimRight(buf.Bytes(), "\n")
	}

	// Shallow: a value carrying HTML metacharacters.
	shallow := &nodeT{Value: `<tag> & "q"`}
	nS, bS := native(shallow), baml(shallow)
	if bytes.Equal(nS, bS) {
		t.Fatalf("native and baml raw bytes must DIFFER by escaping, both = %s", nS)
	}
	if _, om := CompareStaticStructured(nS, bS, b); !om {
		t.Fatalf("shallow escaping-only difference must order-MATCH (native wins):\n native=%s\n baml  =%s", nS, bS)
	}

	// Deep: the metacharacter value is under the recursive `next` edge (union recursion).
	deep := &nodeT{Value: "root", Next: &nodeT{Value: `<b> & </b>`}}
	nD, bD := native(deep), baml(deep)
	if bytes.Equal(nD, bD) {
		t.Fatalf("deep native and baml raw bytes must DIFFER by escaping, both = %s", nD)
	}
	if _, om := CompareStaticStructured(nD, bD, b); !om {
		t.Fatalf("deep escaping-only difference must order-MATCH via union recursion:\n native=%s\n baml  =%s", nD, bD)
	}

	// Sanity: a REAL content difference (different value) must still mismatch.
	if _, om := CompareStaticStructured([]byte(`{"value":"x","next":null}`), []byte(`{"value":"y","next":null}`), b); om {
		t.Fatalf("a real content difference must NOT order-match")
	}
}
