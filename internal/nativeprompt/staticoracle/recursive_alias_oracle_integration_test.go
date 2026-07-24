//go:build integration

// De-BAML Phase 3a (recursive ALIASES) — STATIC recursive-alias differential MANIFEST.
//
// The byte-exact oracle for the served five-arm JSON alias
// (type JSON = int | string | bool | JSON[] | map<string, JSON>). For every row it
// runs the native debaml.ParseStaticBundle over an assistant text and asserts RAW-BYTE
// equality against stock BAML v0.223's Parse.StaticRecursiveAliasJSON + json.Marshal
// (the EXACT production static callback — HTML-escaped, sorted-public map keys), never
// the unsafe dynamic Cgo route. It also decodes the native canonical JSON through the
// SAME narrow DecodeStaticAliasFinal carrier the generated serve seam uses and asserts
// the re-marshaled concrete union equals the native bytes. The wider JsonValue is
// exercised as a zero-native-socket DECLINE + the null-distinction witness
// (JsonValue null -> null, vs served JSON null -> []).

package staticoracle

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
)

// aliasJSONMarshal is the EXACT production static-callback canonicalizer for the alias:
// json.Marshal (HTML-escaping ON, sorted-public map keys), matching the generated
// bamlOnlyParse closure (Parse.<Method> then json.Marshal). This differs from the
// recursive-CLASS oracle's SetEscapeHTML(false) marshalCanon because the native alias
// FinalJSON is itself json.Marshal of the equivalent Go value (HTML-escaped).
func aliasJSONMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal BAML alias concrete: %v", err)
	}
	return out
}

// aliasRows is the required matrix: terminals (root + inside list/map), empty
// composites, ambiguous scalars, the null trap, alternating list/map nesting both
// directions + a deep case (no cap), map order/dupes (non-lexical, overwrite,
// dup-then-bad, bad-then-good, nested dup maps), and Unicode/HTML escaping.
func aliasRows() []string {
	rows := []string{
		// Terminals as root.
		`1`, `-7`, `0`, `"hello"`, `""`, `true`, `false`,
		// Ambiguous scalars.
		`"1"`, `"true"`, `"1.5"`,
		// Numbers with no float arm -> int (FloatToInt / round).
		`1.5`, `3.0`, `-2.5`,
		// Empty composites.
		`[]`, `{}`,
		// Terminals inside a list.
		`[1,"1",true]`, `["a",-3,false,"b"]`,
		// Terminals inside a map (non-lexical keys, sorted public).
		`{"z":1,"a":"s","m":true}`, `{"one":1,"two":"2","three":false}`,
		// Map order / duplicates.
		`{"z":1,"a":2,"z":3}`,           // overwrite -> z=3
		`{"a":1,"a":"two"}`,             // last-value-wins
		`{"z":1,"a":"bad","z":3}`,       // dup-then-good sibling kept
		`{"k":1,"k":2,"k":3}`,           // triple dup
		`{"outer":{"z":1,"a":2,"z":9}}`, // nested dup map
		// Alternating list/map, both directions.
		`[{"a":[1,2]},{"b":["x"]}]`,
		`{"list":[{"k":1},{"k":2}]}`,
		`[[1],[2,3],{"m":[true]}]`,
		// Null trap: root, list element, map value.
		`null`, `[null]`, `[1,null,2]`, `{"n":null}`, `{"a":1,"b":null}`,
		// Unicode / HTML escaping (non-ASCII keys+values, HTML metacharacters).
		`{"é":"ü","key":"<v> & \"q\""}`, `"<tag> & </tag>"`, `["<a>","&b","c>d"]`,
		`{"emoji":"😀","note":"a&b<c>d"}`,
	}
	// A deep alternating list/map case well beyond fixture depth (no cap).
	var deep strings.Builder
	const depth = 40
	for i := 0; i < depth; i++ {
		if i%2 == 0 {
			deep.WriteString(`{"k":`)
		} else {
			deep.WriteString(`[`)
		}
	}
	deep.WriteString(`42`)
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

// TestAliasStaticDifferential is the byte-exact JSON-alias manifest: every row's native
// FinalJSON must equal stock BAML v0.223's Parse + json.Marshal, and decode back through
// the narrow alias carrier to the same bytes.
func TestAliasStaticDifferential(t *testing.T) {
	ctx := context.Background()
	bundle := lowerReturn(t, "StaticRecursiveAliasJSON")
	if !debaml.IsProvenRecursiveAliasStaticFamily(bundle) {
		t.Fatal("StaticRecursiveAliasJSON bundle must be the proven alias family")
	}
	rows := aliasRows()
	rawMatch, typedMatch := 0, 0
	for _, text := range rows {
		t.Run(text, func(t *testing.T) {
			// Native leg.
			nativeRes, err := debaml.ParseStaticBundle(ctx, bundle, text)
			if err != nil {
				t.Fatalf("native ParseStaticBundle: %v\ntext: %s", err, text)
			}
			// BAML leg (stock v0.223 Parse + json.Marshal).
			bamlVal, berr := aliasParseJSON(text)
			if berr != nil {
				t.Fatalf("BAML Parse.StaticRecursiveAliasJSON: %v\ntext: %s", berr, text)
			}
			bamlJSON := aliasJSONMarshal(t, bamlVal)
			if !bytes.Equal(nativeRes.JSON, bamlJSON) {
				t.Fatalf("RAW-BYTE mismatch:\n native: %s\n baml:   %s\n text:   %s", nativeRes.JSON, bamlJSON, text)
			}
			rawMatch++
			// Concrete decode round-trip through the NARROW alias carrier.
			concrete, derr := bamlutils.DecodeStaticAliasFinal[types.Union5BoolOrIntOrListJSONOrMapStringKeyJSONValueOrString](nativeRes.JSON)
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
		t.Fatalf("alias manifest count drift: raw_byte_match=%d typed_match=%d, want %d each", rawMatch, typedMatch, len(rows))
	}
	t.Logf("alias JSON differential: %d rows raw-byte + concrete round-trip exact", len(rows))
}

// TestAliasStaticDifferential_NullDistinction pins the crux distinction: the served
// non-nullable JSON coerces null -> [] (the list fallback), while the wider declined
// JsonValue coerces null -> null (nil alias pointer). Both compared against stock BAML.
func TestAliasStaticDifferential_NullDistinction(t *testing.T) {
	ctx := context.Background()
	jsonBundle := lowerReturn(t, "StaticRecursiveAliasJSON")

	nativeRes, err := debaml.ParseStaticBundle(ctx, jsonBundle, `null`)
	if err != nil {
		t.Fatalf("native ParseStaticBundle(null): %v", err)
	}
	if string(nativeRes.JSON) != `[]` {
		t.Fatalf("JSON null native -> %s, want []", nativeRes.JSON)
	}
	jsonVal, jerr := aliasParseJSON(`null`)
	if jerr != nil {
		t.Fatalf("BAML Parse JSON(null): %v", jerr)
	}
	if b := aliasJSONMarshal(t, jsonVal); string(b) != `[]` {
		t.Fatalf("BAML JSON null -> %s, want []", b)
	}

	// JsonValue null -> null (BAML), and native DECLINES the whole family (nil pointer
	// serving is not part of Phase 3a), so the route falls back to BAML.
	jvVal, jverr := aliasParseJsonValue(`null`)
	if jverr != nil {
		t.Fatalf("BAML Parse JsonValue(null): %v", jverr)
	}
	if b := aliasJSONMarshal(t, jvVal); string(b) != `null` {
		t.Fatalf("BAML JsonValue null -> %s, want null (nil alias pointer)", b)
	}
}

// TestAliasStaticDifferential_JsonValueDeclines proves the wider JsonValue is a
// zero-native-socket DECLINE at BOTH the parser support gate and the exact fingerprint,
// so native never claims it and BAML serves.
func TestAliasStaticDifferential_JsonValueDeclines(t *testing.T) {
	jv := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	if debaml.IsProvenRecursiveAliasStaticFamily(jv) {
		t.Fatal("JsonValue must NOT be the proven alias family (float+null arms, non-JSON name)")
	}
	if err := debaml.SupportsNativeFinalBundle(jv); err == nil {
		t.Fatal("JsonValue must DECLINE SupportsNativeFinalBundle (structural recursive alias reject)")
	}
}

// aliasParseJSON / aliasParseJsonValue run stock BAML v0.223 Parse for the served and
// declined-oracle alias methods.
func aliasParseJSON(text string) (any, error) {
	return bamlclient.Parse.StaticRecursiveAliasJSON(text)
}

func aliasParseJsonValue(text string) (any, error) {
	return bamlclient.Parse.StaticRecursiveAliasJsonValue(text)
}
