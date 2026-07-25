//go:build integration

// De-BAML Phase 3c (the `JsonValue` recursive-alias family) — STRICT static FINAL
// differential MANIFEST, the sibling of the frozen Phase-3a `JSON` manifest
// (recursive_alias_oracle_integration_test.go).
//
// For every row it runs native debaml.ParseStaticBundle over an assistant text and
// asserts RAW-BYTE equality against stock BAML v0.223's
// Parse.StaticRecursiveAliasJsonValue + json.Marshal (the EXACT production static
// callback), then decodes the native canonical JSON through the SAME narrow
// DecodeStaticAliasFinal[types.JsonValue] carrier the generated serve seam uses and
// asserts the re-marshaled concrete union equals the native bytes.
//
// The rows target the TWO deltas versus `JSON`:
//
//   - the FLOAT arm: `1` -> int but `1.0`/`1.5` -> float (BAML returns the FIRST
//     score-zero strict cast, and strict int accepts a number only through as_i64), the
//     `-0` negative-zero split, exponent/boundary formatting, and the fact that the
//     public float bytes come from Go json.Marshal of the coerced float64 (so `1.0`
//     prints as `1` and the provider's lexeme is intentionally lost);
//   - the NULL arm: a first-class typed null at the root, inside a list, and inside a
//     map — NEVER the `JSON` family's null -> [] list fallback, and never erased.
package staticoracle

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
)

// jsonValueRows is the required matrix. Every row is a candidate the native FINAL
// extractor can own (see [jsonValueFinalBoundaryRows] for the shared extraction
// boundary that is deliberately excluded).
func jsonValueRows() []string {
	rows := []string{
		// --- int-vs-float scoring crux -------------------------------------------
		`1`, `-7`, `0`, `42`, `9223372036854775807`, `-9223372036854775808`,
		`1.0`, `1.5`, `-2.5`, `3.0`, `0.1`, `2.5`,
		// `0.0` is the float arm printing as `0`. (`-0`/`-0.0` are NOT here: a coerced
		// negative zero is a value-scoped DECLINE — see
		// TestJsonValueStaticDifferential_NegativeZeroDeclines.)
		`0.0`,
		// integers just outside i64 range fall to the float arm.
		`9223372036854775808`, `-9223372036854775809`, `18446744073709551615`,
		`123456789012345678901234567890`,
		// --- float public formatting (Go json.Marshal of the coerced float64) -----
		`1e3`, `1E3`, `1.2e5`, `1.2e-5`, `1e-7`, `1e20`, `1e21`, `1.5e3`,
		`5e-324`, `1.7976931348623157e308`, `-1.7976931348623157e308`, `2.2250738585072014e-308`,
		`0.30000000000000004`, `0.1234567890123456789`, `1e-306`, `-1e-306`,
		// --- numeric strings stay STRINGS (strict string arm, score 0) ------------
		`"1"`, `"1.5"`, `"-0"`, `"1e400"`, `"true"`, `"null"`,
		// --- bool / string terminals ---------------------------------------------
		`true`, `false`, `"hello"`, `""`, `"<tag> & </tag>"`, `"café ☕ 漢"`,
		// --- the NULL arm: root, list, map, nested --------------------------------
		`null`, `[null]`, `[1,null,2]`, `{"n":null}`, `{"a":1,"b":null}`,
		`[null,null]`, `[[null]]`, `{"k":{"n":null}}`, `[1,null,{"k":null}]`,
		`{"z":null,"a":1}`,
		// --- empty composites -----------------------------------------------------
		`[]`, `{}`,
		// --- mixed arms + arm re-selection inside containers ----------------------
		`[1,1.5,"x",true,null]`, `[1.0,2]`, `[1.5,2.5]`, `[null,1.5,"x",true]`,
		`{"f":1.5,"n":null,"i":7}`, `[{"a":1},2,"x"]`, `[[1],{"k":2},3]`,
		`["a",1,"b",2]`, `[1,"x",2,true,3]`,
		// --- map ordering / duplicate keys (sorted-public, IndexMap overwrite) ----
		`{"z":1,"a":2}`, `{"z":1,"a":2,"z":3}`, `{"a":1,"a":"two"}`, `{"k":1,"k":2,"k":3}`,
		`{"z":1,"a":1.5,"z":null}`, `{"outer":{"z":1,"a":2,"z":9}}`,
		// --- alternating list/map nesting, both directions ------------------------
		`[{"a":[1,2]},{"b":["x"]}]`, `{"list":[{"k":1},{"k":2}]}`,
		`[[1],[2,3],{"m":[true]}]`, `[{"a":[1,{"z":3,"y":4}]}]`,
		// --- Unicode / HTML escaping ----------------------------------------------
		`{"é":"ü","key":"<v> & \"q\""}`, `["<a>","&b","c>d"]`, `{"emoji":"😀","note":"a&b<c>d"}`,
		`["漢字","x"]`, `{"kéy":"☕"}`,
		// --- comments / fences / prose (jsonish recovery) -------------------------
		`[1.5,2]//trailing note`, `{"a":null}/*block*/`, "[1,/*x*/1.5,null]",
		"{\n// line\n\"a\":1.5}", "```json\n[null,1.5]\n```", "here: {\"a\":1.5,\"n\":null}",
	}
	// A deep alternating list/map case well beyond fixture depth (no cap), bottoming
	// out on the new float arm.
	var deep strings.Builder
	const depth = 40
	for i := 0; i < depth; i++ {
		if i%2 == 0 {
			deep.WriteString(`{"k":`)
		} else {
			deep.WriteString(`[`)
		}
	}
	deep.WriteString(`1.5`)
	for i := depth - 1; i >= 0; i-- {
		if i%2 == 0 {
			deep.WriteString(`}`)
		} else {
			deep.WriteString(`]`)
		}
	}
	rows = append(rows, deep.String())
	return rows
}

// TestJsonValueStaticDifferential is the byte-exact JsonValue manifest: every row's
// native FinalJSON must equal stock BAML v0.223's Parse + json.Marshal, and decode back
// through the narrow alias carrier to the same bytes.
func TestJsonValueStaticDifferential(t *testing.T) {
	ctx := context.Background()
	bundle := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	if !debaml.IsProvenJsonValueRecursiveAliasStaticFamily(bundle) {
		t.Fatal("StaticRecursiveAliasJsonValue bundle must be the proven JsonValue alias family")
	}
	if debaml.IsProvenRecursiveAliasStaticFamily(bundle) {
		t.Fatal("the JsonValue bundle must NOT satisfy the frozen JSON-only predicate")
	}
	if err := debaml.SupportsNativeFinalBundle(bundle); err != nil {
		t.Fatalf("JsonValue must be final-supported: %v", err)
	}
	rows := jsonValueRows()
	rawMatch, typedMatch := 0, 0
	for _, text := range rows {
		t.Run(text, func(t *testing.T) {
			nativeRes, err := debaml.ParseStaticBundle(ctx, bundle, text)
			if err != nil {
				t.Fatalf("native ParseStaticBundle: %v\ntext: %s", err, text)
			}
			bamlVal, berr := aliasParseJsonValue(text)
			if berr != nil {
				t.Fatalf("BAML Parse.StaticRecursiveAliasJsonValue: %v\ntext: %s", berr, text)
			}
			bamlJSON := aliasJSONMarshal(t, bamlVal)
			if !bytes.Equal(nativeRes.JSON, bamlJSON) {
				t.Fatalf("RAW-BYTE mismatch:\n native: %s\n baml:   %s\n text:   %s", nativeRes.JSON, bamlJSON, text)
			}
			rawMatch++
			// Concrete decode round-trip through the NARROW alias carrier. types.JsonValue
			// is a POINTER union, so a `null` row decodes to a TYPED NIL — a present value
			// whose re-marshal is `null`, never a decode error.
			concrete, derr := bamlutils.DecodeStaticAliasFinal[types.JsonValue](nativeRes.JSON)
			if derr != nil {
				t.Fatalf("DecodeStaticAliasFinal: %v\njson: %s", derr, nativeRes.JSON)
			}
			reMarshaled := aliasJSONMarshal(t, concrete)
			if !bytes.Equal(reMarshaled, nativeRes.JSON) {
				t.Fatalf("concrete carrier does not round-trip:\n decoded: %s\n native:  %s", reMarshaled, nativeRes.JSON)
			}
			typedMatch++
		})
	}
	if rawMatch != len(rows) || typedMatch != len(rows) {
		t.Fatalf("JsonValue manifest count drift: raw_byte_match=%d typed_match=%d, want %d each", rawMatch, typedMatch, len(rows))
	}
	t.Logf("JsonValue differential: %d rows raw-byte + concrete round-trip exact", len(rows))
}

// TestJsonValueStaticDifferential_ArmSelection pins the int-vs-float SELECTION itself
// (not merely the printed bytes) by interrogating the generated union's arm accessors on
// BOTH legs. `1` must be the INT arm and `1.0` the FLOAT arm even though both print `1`
// — a bytes-only assertion could not tell them apart.
func TestJsonValueStaticDifferential_ArmSelection(t *testing.T) {
	ctx := context.Background()
	bundle := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	cases := []struct {
		text    string
		wantInt bool // else float
		bytes   string
	}{
		{`1`, true, `1`},
		{`0`, true, `0`},
		{`-7`, true, `-7`},
		{`9223372036854775807`, true, `9223372036854775807`},
		{`1.0`, false, `1`},
		{`3.0`, false, `3`},
		{`1.5`, false, `1.5`},
		{`1e3`, false, `1000`},
		{`9223372036854775808`, false, `9223372036854776000`},
		{`0.0`, false, `0`},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			bamlVal, berr := bamlclient.Parse.StaticRecursiveAliasJsonValue(tc.text)
			if berr != nil {
				t.Fatalf("BAML Parse: %v", berr)
			}
			if bamlVal == nil {
				t.Fatalf("BAML returned a nil alias pointer for %q", tc.text)
			}
			if got := bamlVal.IsInt(); got != tc.wantInt {
				t.Fatalf("BAML arm for %q: IsInt=%v IsFloat=%v, want int=%v",
					tc.text, bamlVal.IsInt(), bamlVal.IsFloat(), tc.wantInt)
			}
			if bamlVal.IsInt() == bamlVal.IsFloat() {
				t.Fatalf("BAML arm for %q is neither/both int and float", tc.text)
			}
			res, err := debaml.ParseStaticBundle(ctx, bundle, tc.text)
			if err != nil {
				t.Fatalf("native ParseStaticBundle: %v", err)
			}
			if string(res.JSON) != tc.bytes {
				t.Fatalf("native bytes for %q = %s, want %s", tc.text, res.JSON, tc.bytes)
			}
			// The native bytes must decode into the SAME arm BAML selected. (`1.0` -> `1`
			// re-decodes as the int arm because the public bytes are integral: the arm
			// identity assertion above is on BAML's own carrier, which is what pins the
			// selection rule; native's obligation is the BYTES, which are what ship.)
			if b := aliasJSONMarshal(t, bamlVal); string(b) != tc.bytes {
				t.Fatalf("BAML public bytes for %q = %s, want %s", tc.text, b, tc.bytes)
			}
		})
	}
}

// jsonValueFinalBoundaryRows are the inputs the native FINAL extractor DECLINES: a bare
// ROOT scalar that is not valid strict JSON has no `{`/`[` for the fixing pass to anchor
// on, so extraction finds no cleanly-claimable candidate.
//
// This is a PRE-EXISTING extraction boundary shared with the shipped Phase-3a `JSON`
// family (LIVE-VERIFIED: the `JSON` bundle declines the FINAL for exactly the same
// inputs), not something the `JsonValue` family introduces — which is why the manifest
// above, like the frozen `JSON` manifest, contains no such row. The assertion here is
// the strong one: native must DECLINE, never claim wrong bytes.
//
// On the UNARY lane a decline is SAFE: BAML parse-only produces the final over the same
// response, so the caller gets BAML's bytes with no second request. These same inputs are
// on the STREAM residual ledger (TestJsonValueStreamResidualLedger), where they are part of
// the blocker that keeps the streaming gate closed — because there a decline would be a
// terminal error instead of a repair.
func jsonValueFinalBoundaryRows() []string {
	return []string{`nul`, `NaN`, `Infinity`, `-`, `+1`, `.5`, `5.`, `007`, `1e400`, `abc`}
}

func TestJsonValueStaticDifferential_FinalExtractionBoundary(t *testing.T) {
	ctx := context.Background()
	jv := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	js := lowerReturn(t, "StaticRecursiveAliasJSON")
	// `1e400` is excluded from the SHARED half of the assertion: the shipped JSON family
	// CLAIMS it today (native `[]` vs BAML `"1e400"`) — a pre-existing Phase-3a
	// divergence outside this slice's scope, which the frozen JSON corpus does not cover
	// and which this PR must not change. JsonValue still declines it, which is what is
	// asserted for every row.
	sharedExempt := map[string]bool{`1e400`: true}
	for _, text := range jsonValueFinalBoundaryRows() {
		t.Run(text, func(t *testing.T) {
			_, err := debaml.ParseStaticBundle(ctx, jv, text)
			if err == nil {
				t.Fatalf("JsonValue FINAL must DECLINE the bare non-strict-JSON root scalar %q (it claimed instead)", text)
			}
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("JsonValue FINAL on %q: want the decline sentinel, got %v", text, err)
			}
			if sharedExempt[text] {
				return
			}
			// The SHARED boundary proof: the shipped JSON family declines identically, so
			// this is inherited extractor behaviour, not a Phase-3c regression.
			//
			// This leg needs the SAME sentinel check as the JsonValue leg above, and for
			// the same reason: ParseStaticBundle deliberately propagates non-sentinel
			// CLAIMED parse failures, so a bare `jerr != nil` would let a JSON hard
			// failure on one of these shared rows masquerade as a safe decline — and
			// "safely fallbackable" is precisely the claim this test exists to establish.
			// Only the sentinel is repairable by BAML parse-only on the unary lane.
			_, jerr := debaml.ParseStaticBundle(ctx, js, text)
			if jerr == nil {
				t.Fatalf("expected the shipped JSON family to decline %q too (the boundary is shared); it claimed", text)
			}
			if !errors.Is(jerr, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("shipped JSON family on %q: the shared boundary must be the decline SENTINEL (a repairable fallback), got a claimed parse failure: %v", text, jerr)
			}
		})
	}
}

// TestJsonValueStaticDifferential_NegativeZeroDeclines pins the ONE finite float the
// served seam cannot carry: a coerced NEGATIVE ZERO.
//
// Native's canonical FinalJSON for `-0` would be `-0` — byte-identical to BAML's own
// json.Marshal of its Float(-0.0) arm — but the generated Union6.UnmarshalJSON tries the
// INT arm first, and `-0` unmarshals into an int64 as 0. The serve seam decodes the
// canonical bytes back through that carrier, so native would deliver `0` where BAML
// delivers `-0`. Rather than reshape the emitted bytes (the scope forbids working around a
// formatting boundary by changing the lexeme), native DECLINES the value.
//
// On the UNARY lane that decline is a REPAIR, not a loss: native owns the single provider
// request and BAML parse-only produces the final over the SAME response (the
// `native_baml_parse` winner token), so the caller still receives BAML's `-0`. The
// STREAMING lane has no such repair, which is why this family is stream-declined outright
// rather than relying on a value-scoped decline behind a shape-scoped gate — see
// TestJsonValueStreamGateDeclines and internal/debaml/static_stream_serve.go.
//
// The test asserts all three legs: BAML's value, native's decline, and the
// non-injectivity of the generated carrier that forces it.
func TestJsonValueStaticDifferential_NegativeZeroDeclines(t *testing.T) {
	ctx := context.Background()
	jv := lowerReturn(t, "StaticRecursiveAliasJsonValue")

	// The carrier really is non-injective on the sign of zero.
	rt, derr := bamlutils.DecodeStaticAliasFinal[types.JsonValue]([]byte(`-0`))
	if derr != nil {
		t.Fatalf("DecodeStaticAliasFinal(-0): %v", derr)
	}
	if b := aliasJSONMarshal(t, rt); string(b) != `0` {
		t.Fatalf("generated carrier round-trip of `-0` = %s; this test's premise (Int-arm-first decode) no longer holds — re-evaluate the decline", b)
	}
	// Every negative-zero-bearing shape declines, at the root and nested.
	for _, text := range []string{`-0`, `-0.0`, `[-0]`, `{"z":-0}`, `[1,-0,2]`, `[[-0]]`, `{"a":{"b":-0.0}}`} {
		t.Run(text, func(t *testing.T) {
			if _, err := debaml.ParseStaticBundle(ctx, jv, text); err == nil {
				t.Fatalf("JsonValue FINAL must DECLINE a coerced negative zero %q", text)
			} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("JsonValue FINAL on %q: want the decline sentinel, got %v", text, err)
			}
			// BAML's own answer keeps the sign, which is exactly why native must not claim.
			v, berr := bamlclient.Parse.StaticRecursiveAliasJsonValue(text)
			if berr != nil {
				t.Fatalf("BAML Parse(%q): %v", text, berr)
			}
			if b := aliasJSONMarshal(t, v); !bytes.Contains(b, []byte(`-0`)) {
				t.Fatalf("BAML %q -> %s, expected it to preserve the negative zero", text, b)
			}
		})
	}
	// A FIXING-parsed `-0` is the INTEGER 0 (alias_number.go PATH B), so it is NOT a
	// negative zero and still serves natively — final and partial alike.
	if res, err := debaml.ParseStaticBundle(ctx, jv, `[-0,]`); err != nil {
		t.Fatalf("fixing-parsed -0 in `[-0,]` must still serve natively: %v", err)
	} else if string(res.JSON) != `[0]` {
		t.Fatalf("fixing-parsed -0 in `[-0,]` -> %s, want [0] (the i64 path, not a negative zero)", res.JSON)
	}
	if out, emit, err := debaml.ParseAliasStreamPartial(jv, `[-0`); err != nil || !emit || string(out) != `[0]` {
		t.Fatalf("streaming partial `[-0` -> (%s, emit=%v, err=%v), want ([0], true, nil): the unclosed prefix is FIXING-parsed, so its -0 is the integer 0", out, emit, err)
	}
	// …but the stream FINAL of that same unclosed prefix declines: EOF completion closes
	// it to `[-0]`, which then STRICT-parses as a negative zero (alias_number.go PATH A).
	// Driven through the GATE-FREE entry because the family is stream-DECLINED at
	// admission; on the claimed stream lane such a decline would be unrepairable, which is
	// exactly why the gate is closed (internal/debaml/static_stream_serve.go).
	// Require the DECLINE SENTINEL specifically, not merely a non-nil error: a claimed
	// parse failure here would be a different outcome entirely.
	if _, err := debaml.ParseAliasStreamFinal(ctx, jv, `[-0`); err == nil {
		t.Fatal("stream FINAL of `[-0` must DECLINE: EOF completion makes it the strict-parsed `[-0]`, a negative zero")
	} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("stream FINAL of `[-0`: want the decline sentinel, got a claimed parse failure: %v", err)
	}
}

// TestJsonValueFloatCarrierInjectivity is the PROPERTY guard behind the narrow
// negative-zero decline: over a wide sweep of finite float64 values it asserts that
// json.Marshal(f) — the exact public projection the float arm uses — round-trips through
// the generated Union6 carrier to the SAME bytes, with the sign of zero as the ONLY
// exception. If BAML's generator ever changes the arm order (or a new value stops
// round-tripping), this fails and the decline set must be re-derived rather than silently
// under- or over-approximating.
func TestJsonValueFloatCarrierInjectivity(t *testing.T) {
	vals := []float64{
		0, math.Copysign(0, -1), 1, -1, 1.5, -2.5, 0.1, 3, 1e3, 1e20, 1e21, 1e-7,
		5e-324, 1.7976931348623157e308, -1.7976931348623157e308, 2.2250738585072014e-308,
		9223372036854775808, -9223372036854775809, 1.2345678901234568e+29,
		0.30000000000000004, 1e-306, -1e-306, 123456789, -987654321, 1e15, 1e16,
	}
	for _, f := range vals {
		want, merr := stdjson.Marshal(f)
		if merr != nil {
			t.Fatalf("json.Marshal(%v): %v", f, merr)
		}
		got, derr := bamlutils.DecodeStaticAliasFinal[types.JsonValue](want)
		if derr != nil {
			t.Fatalf("DecodeStaticAliasFinal(%s): %v", want, derr)
		}
		back := aliasJSONMarshal(t, got)
		isNegZero := f == 0 && math.Signbit(f)
		if bytes.Equal(back, want) == isNegZero {
			t.Fatalf("float carrier injectivity changed for %v: marshal=%s round-trip=%s (negative-zero exception expected only for -0)", f, want, back)
		}
	}
}
