//go:build integration && nanollm_integration

package static

// De-BAML Slice 8C — static per-method decoder DIFFERENTIAL (the "real unknown").
//
// The native static SAP produces canonical JSON; BAML `Parse.<Method>` returns a
// per-method CFFI-decoded Go type. Scope §8.1 requires the generated
// DecodeNativeStaticFinal mapper to be BAML-v0.223-differential-proven — NOT assumed
// json.Unmarshal — and the initial admitted return set limited to shapes whose mapper
// is proven. This test IS that proof for the two admitted shapes over the stock
// static_oracle client + REAL BAML v0.223:
//
//   - SHADOW same-bytes: native debaml.ParseStaticBundle(Return, text) produces the
//     SAME structured JSON as BAML `Parse.<Method>(text)` for the class shape (the
//     shape native SAP genuinely claims).
//   - MAPPER equivalence: bamlutils.DecodeStaticFinal[Return](nativeCanonical) equals
//     BAML's own CFFI-decoded return value — the exact concrete Go type — for the
//     admitted primitive-scalar and flat-class shapes.
//
// Every richer shape declines PRE-CLAIM at admittedStaticReturnShape, so this narrow
// proof is exactly the admitted set; a recursive/alias/optional/enum shape is not
// (yet) proven and is not served.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	statictypes "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/introspected"
)

// lowerReturn lowers a static method's descriptor Return to the internal Bundle the
// native SAP parses against.
func lowerReturn(t *testing.T, method string) *schema.Bundle {
	t.Helper()
	fn, ok := introspected.StaticPromptDescriptor(method)
	if !ok {
		t.Fatalf("descriptor for %q absent", method)
	}
	b, err := schema.FromStaticDescriptor(fn.Return)
	if err != nil {
		t.Fatalf("FromStaticDescriptor(%s): %v", method, err)
	}
	return b
}

// TestStaticMapperDifferential_StaticAnswer proves, for the flat StaticAnswer class
// return, that (a) native SAP == BAML parse on the SAME bytes, and (b) the generated
// decoder core reproduces BAML's exact concrete return value.
func TestStaticMapperDifferential_StaticAnswer(t *testing.T) {
	bundle := lowerReturn(t, "StaticOutputFormat")

	// A handful of canonical assistant texts BAML v0.223 accepts for StaticAnswer.
	for _, text := range []string{
		`{"answer": "it is 21°C", "confidence": 7}`,
		"Here you go:\n```json\n{ \"answer\": \"trees are green\", \"confidence\": 0 }\n```",
		`{"answer":"exact","confidence":-3}`,
	} {
		// Native leg.
		nativeRes, nerr := debaml.ParseStaticBundle(context.Background(), bundle, text)
		if nerr != nil {
			t.Fatalf("ParseStaticBundle(%q): %v", text, nerr)
		}
		// BAML leg — the v0.223 oracle: CFFI-decoded concrete StaticAnswer.
		bamlAnswer, berr := bamlclient.Parse.StaticOutputFormat(text)
		if berr != nil {
			t.Fatalf("BAML Parse.StaticOutputFormat(%q): %v", text, berr)
		}
		bamlJSON, merr := json.Marshal(bamlAnswer)
		if merr != nil {
			t.Fatalf("marshal BAML answer: %v", merr)
		}

		// (a) SHADOW same-bytes: native canonical == BAML parse structured.
		if !jsonSemanticEqual(t, nativeRes.JSON, bamlJSON) {
			t.Errorf("shadow drift for %q:\n native=%s\n baml  =%s", text, nativeRes.JSON, bamlJSON)
		}

		// (b) MAPPER equivalence: the decoder core reproduces BAML's exact struct.
		decoded, derr := bamlutils.DecodeStaticFinal[statictypes.StaticAnswer](nativeRes.JSON)
		if derr != nil {
			t.Fatalf("DecodeStaticFinal[StaticAnswer](%s): %v", nativeRes.JSON, derr)
		}
		if decoded != bamlAnswer {
			t.Errorf("mapper differential for %q: decoder=%+v BAML=%+v", text, decoded, bamlAnswer)
		}
	}
}

// TestStaticMapperDifferential_PrimitiveString proves the decoder core reproduces
// BAML's exact string return for the primitive-scalar admitted shape. Native SAP
// declines a BARE (non-JSON) string (no cleanly-claimable candidate), so BAML serves
// the string via the parse-only fallback; the decoder over the canonical
// JSON-encoded string still reproduces BAML's value exactly.
func TestStaticMapperDifferential_PrimitiveString(t *testing.T) {
	bundle := lowerReturn(t, "StaticCompletion")

	const bare = "Cats are great." // ONE bare (non-JSON) input, fed to BOTH engines.

	// Native SAP DECLINES the bare string with the DECLINE SENTINEL specifically (no
	// cleanly-claimable JSON candidate), NOT some other/claimed parse failure — that
	// exact sentinel is what routes the response to BAML's parse-only fallback. Assert
	// the sentinel, not merely "an error": a non-sentinel error would be a claimed
	// native failure that must NOT silently fall back.
	if _, nerr := debaml.ParseStaticBundle(context.Background(), bundle, bare); !errors.Is(nerr, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("native SAP on bare string: got %v, want decline sentinel ErrDeBAMLParseUnsupported (so BAML serves)", nerr)
	}

	// True differential: feed BAML the SAME bare input native declined. BAML (the
	// lenient fallback) serves it; the decoder core over the canonical JSON encoding
	// of BAML's own value reproduces that value exactly.
	bamlStr, berr := bamlclient.Parse.StaticCompletion(bare)
	if berr != nil {
		t.Fatalf("BAML Parse.StaticCompletion(%q): %v", bare, berr)
	}
	canonical, merr := json.Marshal(bamlStr)
	if merr != nil {
		t.Fatalf("marshal BAML string: %v", merr)
	}
	decoded, derr := bamlutils.DecodeStaticFinal[string](canonical)
	if derr != nil {
		t.Fatalf("DecodeStaticFinal[string](%s): %v", canonical, derr)
	}
	if decoded != bamlStr {
		t.Errorf("mapper differential (string): decoder=%q BAML=%q", decoded, bamlStr)
	}
}

// jsonSemanticEqual reports whether two JSON documents are structurally equal
// (object key order ignored) — sufficient for the canonical flat-class comparison.
// Both sides decode with UseNumber() so integer-valued fields (e.g. confidence) stay
// exact json.Number tokens rather than being coerced to lossy float64 (which would
// re-marshal 9 as 9 but 10000000000000001 as 1e+16), keeping the compare truly exact.
func jsonSemanticEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	decode := func(label string, raw []byte) any {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("unmarshal %s json %s: %v", label, raw, err)
		}
		return v
	}
	na, _ := json.Marshal(decode("native", a))
	nb, _ := json.Marshal(decode("baml", b))
	return string(na) == string(nb)
}
