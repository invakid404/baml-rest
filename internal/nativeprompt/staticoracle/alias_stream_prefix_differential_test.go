//go:build integration

// De-BAML Phase 3b — STRICT per-prefix parser differential for the served JSON alias stream,
// driven through the PRODUCTION static-stream route. For every VALID-UTF-8 byte prefix of
// every specimen it compares stock BAML v0.223 ParseStream.StaticRecursiveAliasJSON →
// json.Marshal against the PRODUCTION native path debaml.ParseStaticStreamPartial → the
// narrow generated stream decoder (bamlutils.DecodeStaticAliasStream[stream_types.JSON]) →
// json.Marshal: exact agreement on emit-vs-no-emit and (if emitted) the exact public bytes.
// Mid-multibyte-UTF-8 prefixes are SKIPPED here (they never reach the parser — see the loop
// comment; the production parser only ever accumulates valid-UTF-8 text) and are proven
// separately by the SSE-replay test's mid-☕-split case. Then, ONCE per specimen, at the
// COMPLETE input it ALSO compares BAML Parse.StaticRecursiveAliasJSON → json.Marshal against
// debaml.ParseStaticStreamFinal → the final decoder (DecodeStaticAliasFinal[types.JSON]).
// NO #583-deferred branch — every compared prefix is strict.

package staticoracle

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client"
	streamtypes "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/stream_types"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
)

// bamlStreamPartial returns BAML's public partial for a prefix via ParseStream: ("", false)
// if it errored (no emit), else (marshaled bytes, true).
func bamlStreamPartial(text string) (string, bool) {
	v, err := bamlclient.ParseStream.StaticRecursiveAliasJSON(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR:" + merr.Error(), true
	}
	return string(b), true
}

// bamlFinal returns BAML's public FINAL for a completed text via Parse (not ParseStream).
func bamlFinal(text string) (string, bool) {
	v, err := bamlclient.Parse.StaticRecursiveAliasJSON(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR:" + merr.Error(), true
	}
	return string(b), true
}

// nativePartialProd drives the PRODUCTION partial route: ParseStaticStreamPartial (the gated
// production entry) → the narrow stream carrier decoder (stream_types.JSON, a *Union5 pointer
// union) → re-marshal. The recover is a GENERIC backstop that fails the test on any UNEXPECTED
// panic — every prefix reaching this helper is VALID UTF-8 (the loop skips invalid ones via
// utf8.ValidString), so it is NOT a mid-UTF-8 / invalid-input no-panic proof (the SSE-replay's
// mid-☕-split case owns that transport-level proof). A decline (ErrDeBAMLParseUnsupported) is
// a benign no-emit.
func nativePartialProd(t *testing.T, bundle *schema.Bundle, prefix string) (out string, emit bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("native ParseStaticStreamPartial PANICKED on prefix %q: %v", prefix, r)
		}
	}()
	res, err := debaml.ParseStaticStreamPartial(context.Background(), bundle, prefix)
	if err != nil {
		// A no-emit is ONLY the unsupported sentinel (per ParseStaticStreamPartial's
		// contract); a claimed parse FAILURE (a non-sentinel error) must surface, not be
		// swallowed as no-emit where it could spuriously match BAML's own no-emit.
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("native ParseStaticStreamPartial(%q): non-decline error %v (a claimed parse failure must not be swallowed as no-emit)", prefix, err)
		}
		return "", false // benign no-emit (the unsupported sentinel)
	}
	// Decode through the SAME narrow stream carrier decoder the generated seam uses, then
	// re-marshal (proves the production decoder round-trips the canonical bytes).
	dec, derr := bamlutils.DecodeStaticAliasStream[streamtypes.JSON](res.JSON)
	if derr != nil {
		t.Fatalf("DecodeStaticAliasStream(%q): %v\njson: %s", prefix, derr, res.JSON)
	}
	mb, merr := stdjson.Marshal(dec)
	if merr != nil {
		t.Fatalf("re-marshal stream carrier (%q): %v", prefix, merr)
	}
	return string(mb), true
}

// nativeFinalProd drives the PRODUCTION FINAL route: ParseStaticStreamFinal → the narrow
// final decoder (types.JSON value union) → re-marshal.
func nativeFinalProd(t *testing.T, bundle *schema.Bundle, text string) (string, bool) {
	t.Helper()
	res, err := debaml.ParseStaticStreamFinal(context.Background(), bundle, text)
	if err != nil {
		// Same rigor as the partial route: only the unsupported sentinel is a benign
		// no-emit; a claimed FINAL parse failure must surface, not silently match no-emit.
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("native ParseStaticStreamFinal(%q): non-decline error %v (a claimed parse failure must not be swallowed as no-emit)", text, err)
		}
		return "", false
	}
	dec, derr := bamlutils.DecodeStaticAliasFinal[types.JSON](res.JSON)
	if derr != nil {
		t.Fatalf("DecodeStaticAliasFinal(%q): %v\njson: %s", text, derr, res.JSON)
	}
	mb, merr := stdjson.Marshal(dec)
	if merr != nil {
		t.Fatalf("re-marshal final carrier (%q): %v", text, merr)
	}
	return string(mb), true
}

func emitDesc(b string, emit bool) string {
	if !emit {
		return "NO-EMIT"
	}
	return b
}

func aliasStreamSpecimens() []string {
	rows := []string{
		// roots: int / string / bool / null + numeric/bool token boundaries
		`1`, `-7`, `42`, `0`, `1.5`, `3.0`, `-2.5`, `100`, `-0`,
		`true`, `false`, `null`,
		// quoted / escaped strings + multibyte specimens (their VALID-UTF-8 prefixes exercise
		// multibyte content; mid-multibyte cut prefixes are SKIPPED here by the utf8.ValidString
		// guard in the loop — the transport-level mid-multibyte split is proven by the SSE-replay)
		`"hi"`, `"a\"b"`, `""`, `"1"`, `"true"`, `"a\nb"`, `"a\\b"`, `"café ☕ 漢"`,
		`"<tag> & </tag>"`, `"a\tb\rc"`, `["漢字","x"]`, `{"kéy":"☕"}`,
		// lists (empty / open / closed / nested)
		`[1,2,3]`, `["a","b"]`, `[]`, `[1,"x",true]`, `[null]`, `[1,null,2]`,
		`[[]]`, `[[1],[2,3]]`, `[[[42]]]`,
		// maps (empty / greedy-comma / dup-overwrite / null value / order)
		`{"a":1}`, `{}`, `{"a":1,"b":"two"}`, `{"z":1,"a":2,"z":3}`, `{"n":null}`,
		`{"a":1,"a":"two"}`, `{"k":1,"k":2,"k":3}`, `{"z":1,"a":2}`, `{"a":"bad","z":3}`,
		// arm-reselection + list sibling-hint mixtures
		`[1,"x",2,true,3]`, `["a",1,"b",2]`, `[{"a":1},2,"x"]`, `[[1],{"k":2},3]`,
		// alternating list/map nesting (arbitrary depth) both directions
		`[{"a":[1,2]},{"b":["x"]}]`, `{"list":[{"k":1},{"k":2}]}`, `[[1],[2,3],{"m":[true]}]`,
		`{"outer":{"z":1,"a":2,"z":9}}`, `[{"a":[1,{"z":3,"y":4}]}]`,
		// null in list / map
		`{"a":1,"b":null}`, `[1,null,{"k":null}]`,
		// COMMENT specimens (jsonish strips string-aware comments before parse)
		`[1,2]//trailing note`, `{"a":1}/*block*/`, "[1,/*x*/2,3]", "{\n// line\n\"a\":1}",
		// markdown / prose boundaries
		"```json\n[1,2]\n```", "here: {\"a\":1}", "```\n{\"k\":\"v\"}\n```",
		// EOF-unclosed structures
		`{"a":[1,2`, `[{"k":`,
	}
	// A deep alternating list/map case beyond the Phase-3a depth-40 (no cap).
	var deep strings.Builder
	const depth = 60
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

func TestAliasStreamPrefixDifferential(t *testing.T) {
	bundle := lowerReturn(t, "StaticRecursiveAliasJSON")
	if !debaml.IsProvenRecursiveAliasStaticStreamFamily(bundle) {
		t.Fatal("bundle must be the proven static-STREAM alias family")
	}

	specimens := aliasStreamSpecimens()
	total, match, mismatch := 0, 0, 0
	finalTotal, finalMatch := 0, 0

	for _, spec := range specimens {
		bytes := []byte(spec)
		loggedForSpec := 0
		for i := 1; i <= len(bytes); i++ {
			prefix := string(bytes[:i])
			// The PRODUCTION parser only ever sees VALID-UTF-8 accumulated text: each
			// ParseableDelta is a complete JSON string (valid UTF-8), so a mid-multibyte
			// split never reaches the parser — it is a TRANSPORT/SSE-decoder concern proven
			// separately by the SSE-replay's mid-UTF-8-split case (stock BAML's ParseStream
			// itself PANICS on invalid-UTF-8 input, confirming it is not a valid parser
			// input). A Unicode specimen's VALID prefixes still exercise multibyte content.
			if !utf8.ValidString(prefix) {
				continue
			}
			total++
			wantBytes, wantEmit := bamlStreamPartial(prefix)
			gotBytes, gotEmit := nativePartialProd(t, bundle, prefix)

			if wantEmit == gotEmit && (!wantEmit || wantBytes == gotBytes) {
				match++
			} else {
				mismatch++
				if loggedForSpec < 6 {
					loggedForSpec++
					t.Logf("PARTIAL MISMATCH spec=%q prefix=%q\n  BAML:   %s\n  native: %s",
						spec, prefix, emitDesc(wantBytes, wantEmit), emitDesc(gotBytes, gotEmit))
				}
			}
		}

		// The COMPLETE-input FINAL comparison runs EXACTLY ONCE per specimen, OUTSIDE the
		// valid-UTF-8 prefix loop above. The accumulated FINAL text is always the WHOLE
		// specimen (a valid Go string), so pulling it out of the loop guarantees a specimen
		// that happened to end mid-multibyte can never silently drop its FINAL comparison —
		// which the finalMatch==finalTotal check alone could not detect (a comparison that
		// never ran leaves both counters equal). Compares BAML Parse vs the production final
		// route (ParseStaticStreamFinal → the narrow final decoder).
		finalTotal++
		wf, wfe := bamlFinal(spec)
		gf, gfe := nativeFinalProd(t, bundle, spec)
		if wfe == gfe && (!wfe || wf == gf) {
			finalMatch++
		} else {
			t.Errorf("FINAL MISMATCH spec=%q\n  BAML Parse:             %s\n  ParseStaticStreamFinal: %s",
				spec, emitDesc(wf, wfe), emitDesc(gf, gfe))
		}
	}
	t.Logf("PREFIX DIFFERENTIAL: partial total=%d match=%d mismatch=%d (%.1f%%); final total=%d match=%d",
		total, match, mismatch, 100*float64(match)/float64(total), finalTotal, finalMatch)
	if mismatch > 0 {
		t.Errorf("prefix PARTIAL differential has %d/%d MISMATCHES", mismatch, total)
	}
	if finalMatch != finalTotal {
		t.Errorf("FINAL differential has %d/%d mismatches", finalTotal-finalMatch, finalTotal)
	}
	// Coverage guard: every specimen's FINAL route must have been compared exactly once, so a
	// skipped complete input cannot silently omit a comparison the counters could not flag.
	if finalTotal != len(specimens) {
		t.Errorf("FINAL differential covered %d/%d specimens; a complete input was skipped", finalTotal, len(specimens))
	}
}
