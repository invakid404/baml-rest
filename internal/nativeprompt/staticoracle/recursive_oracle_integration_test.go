//go:build integration

// De-BAML Phase 2 (recursive classes) — STATIC-RECURSION differential MANIFEST.
//
// This is the byte-exact oracle for the narrow recursive-class family (self Node,
// mutual A<->B). For every row it lowers the emitted static Return descriptor, runs
// the native debaml.ParseStaticBundle over an assistant text, and asserts RAW-BYTE
// equality (after canonical no-HTML-escape marshaling) against stock BAML v0.223's
// Parse.<Method> on the SAME text — never the unsafe recursive dynamic Cgo route. It
// also decodes the native canonical JSON through the SAME generic DecodeStaticFinal
// carrier the generated serve seam uses and asserts the concrete pointer tree equals
// BAML's concrete generated return value.
//
// The positive matrix is 3 roots × depths {0,1,2,32} × {explicit-null, omitted}
// terminals = 24 rows, plus a Unicode/escaping row so the claim is a real byte
// comparison, and a pair-guard row that asserts BAML's circular-reference behavior
// versus native coerce on a self-referential one-field shape. The manifest count is
// pinned with an anti-omission over the enumerated matrix.

package staticoracle

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
	introspected "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/introspected"
)

// nodeChain builds a self-recursive Node chain of the given depth (edge count) with
// the given per-node value function and terminal encoding.
func nodeChain(depth int, explicitNull bool, val func(i int) string) string {
	var build func(i int) string
	build = func(i int) string {
		v := `"value":` + quoteJSON(val(i))
		if i == depth {
			if explicitNull {
				return "{" + v + `,"next":null}`
			}
			return "{" + v + "}"
		}
		return "{" + v + `,"next":` + build(i+1) + "}"
	}
	return build(0)
}

// abChain builds a mutual A<->B chain rooted at root ("A" or "B").
func abChain(root string, depth int, explicitNull bool, val func(typ string, i int) string) string {
	var build func(i int, typ string) string
	build = func(i int, typ string) string {
		edge, child := "b", "B"
		if typ == "B" {
			edge, child = "a", "A"
		}
		v := `"value":` + quoteJSON(val(typ, i))
		if i == depth {
			if explicitNull {
				return "{" + v + `,"` + edge + `":null}`
			}
			return "{" + v + "}"
		}
		return "{" + v + `,"` + edge + `":` + build(i+1, child) + "}"
	}
	return build(0, root)
}

func quoteJSON(s string) string {
	var buf bytes.Buffer
	enc := stdjson.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

func marshalCanon(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := stdjson.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal BAML concrete: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func lowerReturn(t *testing.T, method string) *schema.Bundle {
	t.Helper()
	fn, ok := introspected.StaticPromptDescriptor(method)
	if !ok {
		t.Fatalf("no static descriptor for %q", method)
	}
	b, err := schema.FromStaticDescriptor(fn.Return)
	if err != nil {
		t.Fatalf("FromStaticDescriptor(%s): %v", method, err)
	}
	return b
}

// bamlParseCanon runs stock BAML v0.223 Parse.<method> and returns (canonical JSON,
// the concrete return value) for the differential.
func bamlParseCanon(t *testing.T, root, text string) ([]byte, any) {
	t.Helper()
	switch root {
	case "Node":
		v, err := bamlclient.Parse.StaticRecursiveNode(text)
		if err != nil {
			t.Fatalf("BAML Parse.StaticRecursiveNode: %v", err)
		}
		return marshalCanon(t, v), v
	case "A":
		v, err := bamlclient.Parse.StaticRecursiveA(text)
		if err != nil {
			t.Fatalf("BAML Parse.StaticRecursiveA: %v", err)
		}
		return marshalCanon(t, v), v
	case "B":
		v, err := bamlclient.Parse.StaticRecursiveB(text)
		if err != nil {
			t.Fatalf("BAML Parse.StaticRecursiveB: %v", err)
		}
		return marshalCanon(t, v), v
	default:
		t.Fatalf("unknown root %q", root)
		return nil, nil
	}
}

// decodeNative decodes the native canonical JSON through the SAME generic carrier the
// generated serve seam uses, returning the concrete pointer tree for the DeepEqual.
func decodeNative(t *testing.T, root string, canonical []byte) any {
	t.Helper()
	switch root {
	case "Node":
		v, err := bamlutils.DecodeStaticFinal[types.Node](canonical)
		if err != nil {
			t.Fatalf("DecodeStaticFinal[Node]: %v\njson: %s", err, canonical)
		}
		return v
	case "A":
		v, err := bamlutils.DecodeStaticFinal[types.A](canonical)
		if err != nil {
			t.Fatalf("DecodeStaticFinal[A]: %v\njson: %s", err, canonical)
		}
		return v
	case "B":
		v, err := bamlutils.DecodeStaticFinal[types.B](canonical)
		if err != nil {
			t.Fatalf("DecodeStaticFinal[B]: %v\njson: %s", err, canonical)
		}
		return v
	default:
		t.Fatalf("unknown root %q", root)
		return nil
	}
}

func methodFor(root string) string {
	switch root {
	case "Node":
		return "StaticRecursiveNode"
	case "A":
		return "StaticRecursiveA"
	case "B":
		return "StaticRecursiveB"
	}
	return ""
}

// TestRecursiveStaticDifferential is the 24-positive-row byte-exact manifest.
func TestRecursiveStaticDifferential(t *testing.T) {
	ctx := context.Background()
	depths := []int{0, 1, 2, 32}
	roots := []string{"Node", "A", "B"}

	positive, typedMatch := 0, 0
	for _, root := range roots {
		bundle := lowerReturn(t, methodFor(root))
		for _, depth := range depths {
			for _, explicitNull := range []bool{true, false} {
				name := root + "/depth" + strconv.Itoa(depth)
				if explicitNull {
					name += "/explicit_null"
				} else {
					name += "/omitted"
				}
				t.Run(name, func(t *testing.T) {
					var text string
					if root == "Node" {
						text = nodeChain(depth, explicitNull, func(i int) string { return "n" + strconv.Itoa(i) })
					} else {
						text = abChain(root, depth, explicitNull, func(typ string, i int) string { return strings.ToLower(typ) + strconv.Itoa(i) })
					}
					// Native leg.
					nativeRes, err := debaml.ParseStaticBundle(ctx, bundle, text)
					if err != nil {
						t.Fatalf("native ParseStaticBundle: %v\ntext: %s", err, text)
					}
					// BAML leg (stock v0.223 Parse, NOT the dynamic Cgo route).
					bamlJSON, bamlVal := bamlParseCanon(t, root, text)
					if !bytes.Equal(nativeRes.JSON, bamlJSON) {
						t.Fatalf("RAW-BYTE mismatch native vs BAML:\n native: %s\n baml:   %s\n text:   %s", nativeRes.JSON, bamlJSON, text)
					}
					positive++
					// Concrete pointer tree: native decode == BAML concrete.
					nativeConcrete := decodeNative(t, root, nativeRes.JSON)
					if !reflect.DeepEqual(nativeConcrete, bamlVal) {
						t.Fatalf("concrete pointer tree mismatch native vs BAML:\n native: %#v\n baml:   %#v", nativeConcrete, bamlVal)
					}
					typedMatch++
				})
			}
		}
	}

	// Anti-omission over the enumerated matrix: 3 roots × 4 depths × 2 terminals.
	const wantPositive = 24
	if positive != wantPositive || typedMatch != wantPositive {
		t.Fatalf("static-recursion manifest count drift: positive_parser_byte_match=%d typed_match=%d, want %d each",
			positive, typedMatch, wantPositive)
	}
	t.Logf("static-recursion manifest: positive_parser_and_typed_match=%d (3 roots × depths 0/1/2/32 × explicit-null/omitted)", positive)
}

// TestRecursiveStaticDifferential_Unicode proves the byte claim is not merely
// structural: a value carrying Unicode + JSON HTML-escaping metacharacters is
// reproduced correctly. Crucially it exercises the PRODUCTION escaping seam: the
// generated BAML-only callback returns encoding/json.Marshal (which escapes `<` `>`
// `&`), while native emits them literally (SetEscapeHTML(false)). The RAW production
// bytes therefore DIFFER from native — proving the difference is real — but once
// canonicalized with the SAME helper the comparator uses (SetEscapeHTML(false)) they
// byte-match native. The corresponding /call ROUTE row
// (recursive_cutover_manifest_test.go) proves the production comparator serves native.
func TestRecursiveStaticDifferential_Unicode(t *testing.T) {
	ctx := context.Background()
	bundle := lowerReturn(t, "StaticRecursiveNode")
	exotic := "café ☕ \"q\" \\b <tag> & é — 日本語"
	text := nodeChain(1, false, func(i int) string {
		if i == 0 {
			return exotic
		}
		return "leaf"
	})
	nativeRes, err := debaml.ParseStaticBundle(ctx, bundle, text)
	if err != nil {
		t.Fatalf("native ParseStaticBundle: %v", err)
	}
	_, bamlVal := bamlParseCanon(t, "Node", text)

	// The EXACT production BAML-only callback: encoding/json.Marshal (HTML-escaping).
	rawProduction, err := stdjson.Marshal(bamlVal)
	if err != nil {
		t.Fatalf("production json.Marshal: %v", err)
	}
	// Canonicalized with native's helper (SetEscapeHTML(false)) — what the comparator does.
	canonProduction := marshalCanon(t, bamlVal)

	// The metacharacter value makes the RAW production bytes DIFFER from both native and
	// the canonicalized form — so the escaping mismatch the comparator must absorb is real.
	if bytes.Equal(rawProduction, canonProduction) {
		t.Fatalf("expected raw production (HTML-escaped) to differ from canonical bytes for a `<tag> &` value; escaping row is not exercising the difference")
	}
	if bytes.Equal(rawProduction, nativeRes.JSON) {
		t.Fatalf("expected raw production (HTML-escaped) to differ from native (unescaped) for a `<tag> &` value")
	}
	// After canonicalization, production byte-matches native.
	if !bytes.Equal(canonProduction, nativeRes.JSON) {
		t.Fatalf("Unicode canonicalized-production vs native RAW-BYTE mismatch:\n native: %s\n canon:  %s", nativeRes.JSON, canonProduction)
	}
	got := decodeNative(t, "Node", nativeRes.JSON)
	if !reflect.DeepEqual(got, bamlVal) {
		t.Fatalf("Unicode concrete mismatch:\n native: %#v\n baml: %#v", got, bamlVal)
	}
}

// TestRecursiveStaticDifferential_PairGuard is the pair-guard regression row of the
// manifest (the two "pair_guard_*" counts). It proves BOTH legs on the one-field Loop
// self-referential shape:
//
//   - pair_guard_preclaim_decline: the served fingerprint DECLINES Loop, so the native
//     static parser returns the ErrDeBAMLParseUnsupported fallback sentinel (BAML
//     serves, ZERO native claim) — never reaching coerce on the route.
//   - pair_guard_parser_error_match: stock BAML v0.223 Parse.StaticRecursiveLoop on the
//     SAME self-referential input ERRORS (its circular-reference parsing error), the
//     shape native's own DIRECT Class::coerce pair-guard also errors on (proven
//     white-box in internal/debaml TestPairGuard_CoerceCircularReference). Status
//     parity, never a silent native over-claim of a clean parse.
func TestRecursiveStaticDifferential_PairGuard(t *testing.T) {
	loop := lowerReturn(t, "StaticRecursiveLoop")
	// A bare scalar string → BAML's InferedObject feeds it into the lone `next Loop?`
	// field recursively, re-entering Loop with the SAME value → circular reference
	// (probed against stock v0.223). Native's Class::coerce guard errors identically.
	text := `"x"`
	preclaimDecline, bamlError := 0, 0

	if _, err := debaml.ParseStaticBundle(context.Background(), loop, text); errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		preclaimDecline++
	} else {
		t.Fatalf("Loop must DECLINE pre-claim (unsupported → BAML fallback), got: %v", err)
	}
	if _, err := bamlclient.Parse.StaticRecursiveLoop(text); err != nil {
		bamlError++
	} else {
		t.Fatalf("stock BAML Parse.StaticRecursiveLoop must ERROR on the self-referential input")
	}

	if preclaimDecline != 1 || bamlError != 1 {
		t.Fatalf("pair-guard pin drift: preclaim_decline=%d parser_error_match=%d, want 1 each", preclaimDecline, bamlError)
	}
	t.Logf("pair-guard manifest: pair_guard_preclaim_decline=%d pair_guard_parser_error_match=%d", preclaimDecline, bamlError)
}
