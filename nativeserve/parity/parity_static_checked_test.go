package parity

import (
	"bytes"
	stdjson "encoding/json"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2b-3 — the comparator over the `Checked[T]` CARRIER.
//
// The carrier lands in the comparator's `default` arm: the constrained field's SCHEMA
// type is a primitive `int`, while its VALUE is an object. Every admitted predicate
// contains `>`, so native's sonic bytes and the BAML-only callback's
// encoding/json.Marshal bytes differ inside the carrier's `expression` — and a
// comparator that canonicalized only top-level string scalars read that as an ORDER
// MISMATCH, which makes the serve path ship BAML's parse of the same response instead of
// native's own bytes. Measured live before it was fixed; pinned here so it stays fixed
// without the CFFI.

// checkedBundle is the admitted fingerprint's Return Bundle.
func checkedBundle(t *testing.T) *schema.Bundle {
	t.Helper()
	label := "positive"
	d := schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticCheckedConfidence",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "StaticCheckedAnswer", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "StaticCheckedAnswer"}, Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "answer"},
					Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
				{Name: schemadescriptor.Name{Name: "confidence"}, Type: schemadescriptor.Type{
					Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt,
					Meta: schemadescriptor.TypeMeta{Constraints: []schemadescriptor.Constraint{{
						Level: schemadescriptor.ConstraintCheck, Expression: "this > 0", Label: &label,
					}}},
				}},
			},
		}},
	}
	b, err := schema.FromStaticDescriptor(d)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	return b
}

// TestCompareStaticStructured_CheckedCarrierEscapingCanonicalized proves an
// escaping-only difference INSIDE the carrier order-matches, so the admitted
// fingerprint serves NATIVE's bytes rather than falling back to BAML's parse.
func TestCompareStaticStructured_CheckedCarrierEscapingCanonicalized(t *testing.T) {
	b := checkedBundle(t)

	// Both legs are computed FROM THE SAME VALUE, so the only difference is the
	// serializer — not two hand-typed literals that could accidentally agree.
	type answer struct {
		Answer     string                   `json:"answer"`
		Confidence bamlutils.Checked[int64] `json:"confidence"`
	}
	carrier, err := bamlutils.NewChecked(int64(9), []bamlutils.Check{{
		Name: "positive", Expression: "this > 0", Status: bamlutils.CheckSucceeded,
	}})
	if err != nil {
		t.Fatalf("NewChecked: %v", err)
	}
	v := answer{Answer: "sunny", Confidence: carrier}

	// native() = the worker's serializer, which does not HTML-escape.
	nativeBytes, err := sonic.Marshal(v)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	// baml() = the EXACT bytes the production BAML-only callback emits.
	bamlBytes, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// The hazard is REAL: the two raw byte strings differ, and they differ INSIDE the
	// carrier rather than at the top level.
	if bytes.Equal(nativeBytes, bamlBytes) {
		t.Fatalf("native and BAML raw bytes are identical (%s); this guard describes a hazard that no "+
			"longer exists", nativeBytes)
	}
	if !bytes.Contains(bamlBytes, []byte(`\u003e`)) {
		t.Fatalf("the BAML leg does not carry the \\u003e escape of `>` (%s); the difference under test "+
			"is absent", bamlBytes)
	}
	if !bytes.Contains(nativeBytes, []byte(`this > 0`)) {
		t.Fatalf("the native leg does not carry a literal `>` (%s)", nativeBytes)
	}

	sm, om := CompareStaticStructured(nativeBytes, bamlBytes, b)
	if !sm {
		t.Fatal("an escaping-only difference must be structurally EQUAL")
	}
	if !om {
		t.Fatalf("an escaping-only difference INSIDE the Checked carrier must order-MATCH, or the serve "+
			"path ships BAML's parse instead of native's bytes:\n native=%s\n baml  =%s",
			nativeBytes, bamlBytes)
	}
}

// TestCompareStaticStructured_CheckedCarrierRealDifferencesStillMismatch is the
// discriminating half: canonicalizing escaping must not make the comparator blind to a
// real divergence inside the carrier.
func TestCompareStaticStructured_CheckedCarrierRealDifferencesStillMismatch(t *testing.T) {
	b := checkedBundle(t)
	const base = `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
	for _, tc := range []struct{ name, other string }{
		{"a different check STATUS",
			`{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`},
		{"a different carrier VALUE",
			`{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`},
		{"a different EXPRESSION",
			`{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 1","status":"succeeded"}}}}`},
		{"a different check LABEL",
			`{"answer":"sunny","confidence":{"value":9,"checks":{"other":{"name":"other","expression":"this > 0","status":"succeeded"}}}}`},
		{"a REORDERED check object",
			`{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"expression":"this > 0","name":"positive","status":"succeeded"}}}}`},
		{"a reordered CARRIER",
			`{"answer":"sunny","confidence":{"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}},"value":9}}`},
		{"an EXTRA check",
			`{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"},"big":{"name":"big","expression":"this > 5","status":"failed"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, om := CompareStaticStructured([]byte(base), []byte(tc.other), b); om {
				t.Fatalf("a real divergence inside the carrier order-MATCHED:\n a=%s\n b=%s", base, tc.other)
			}
		})
	}
	// CONTROL: the value against itself still matches, so the rejections above are about
	// the mutations rather than about a comparator that agrees with nothing.
	if _, om := CompareStaticStructured([]byte(base), []byte(base), b); !om {
		t.Fatal("the identical-bytes control did not order-match; every rejection above is vacuous")
	}
}

// TestCanonicalScalarPreservesNumbersAndOrder pins the two properties the descent must
// NOT change: an exact number token, and object key order.
//
// Canonicalizing escaping by decoding into `any` and re-marshalling would have done both
// wrong — a large integer becomes a float64 and every map key gets sorted — so the
// descent walks tokens instead, and this is what says so.
func TestCanonicalScalarPreservesNumbersAndOrder(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"an i64 past float64's exact range", `{"n":9223372036854775807}`, `{"n":9223372036854775807}`},
		{"a large float literal", `{"n":1e21}`, `{"n":1e21}`},
		{"a negative zero", `{"n":-0}`, `{"n":-0}`},
		{"UNSORTED object keys stay in place", `{"z":1,"a":2}`, `{"z":1,"a":2}`},
		{"nested unsorted keys stay in place", `{"o":{"z":1,"a":2}}`, `{"o":{"z":1,"a":2}}`},
		{"escaping is canonicalized at depth", `{"o":{"s":"a > b"}}`, `{"o":{"s":"a > b"}}`},
		{"escaping is canonicalized in arrays", `["<","&"]`, `["<","&"]`},
		{"whitespace is compacted", "{\n  \"a\" : 1\n}", `{"a":1}`},
		{"a bare string", `"a > b"`, `"a > b"`},
		{"literals", `[true,false,null]`, `[true,false,null]`},
		{"a real control character stays escaped", `{"s":"a\nb"}`, `{"s":"a\nb"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalScalar([]byte(tc.in))
			if err != nil {
				t.Fatalf("canonicalScalar(%s): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonicalScalar(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
	// Undecodable input falls back to compaction rather than erroring the whole compare,
	// which is what the pre-descent implementation did for anything non-string.
	if _, err := canonicalScalar([]byte(`{"a":`)); err == nil {
		t.Fatal("a truncated document must fail compaction rather than be silently accepted")
	}
	if _, err := canonicalScalar([]byte(`{} {}`)); err == nil {
		t.Fatal("two top-level values must not be accepted as one")
	}
}
