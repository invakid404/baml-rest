//go:build integration

// De-BAML Phase 3c — the `JsonValue` STREAMING evidence.
//
// `JsonValue` is served on the FINAL lane and DECLINED on the streaming lane. This file
// carries the three things that decision rests on:
//
//  1. [TestJsonValueStreamGateDeclines] — the gate really is closed, at every layer.
//  2. [TestJsonValueStreamResidualLedger] — the EXACT, enumerated set of inputs for which
//     the alias stream parser does not reproduce BAML. This is the blocker: the set must
//     be EMPTY before the gate may be opened. The values live IN the corpus and are
//     asserted, never carved out of it.
//  3. [TestJsonValueStreamPrefixDifferential] — a STRICT per-prefix differential over the
//     complement (everything not on the ledger), proving the streaming carrier/coercer
//     itself is byte-exact for the surface it can own.
//
// # Why the gate is closed
//
// The static-stream gate admits by DESCRIPTOR SHAPE, pre-socket, and a claimed native
// stream has NO route back to BAML: the generated seam maps a partial-parser error to no
// event, and the orchestrator treats a final-parser error as TERMINAL, explicitly
// forbidding a BAML fallback. So on a claimed stream, any decline that depends on the
// VALUE is a lost partial or a terminal error where BAML would have produced a result.
// The UNARY lane has no such hazard — native owns the single provider request and BAML
// parse-only produces the final over the SAME response — which is why the final lane
// serves this family. internal/debaml/static_stream_serve.go carries the full reasoning.
//
// Both differentials therefore drive the GATE-FREE parser entries
// (debaml.ParseAliasStreamPartial / debaml.ParseAliasStreamFinal), because the production
// entries now decline this family by design. That separation is deliberate: it lets the
// parser be measured and proven independently of the admission decision.
//
// # The cadence finding (unchanged, and still the parser's basis)
//
// For `JsonValue`, BAML's ParseStream(prefix) is byte-identical to its Parse(prefix) at
// every prefix — nothing is dropped by semantic streaming, because the float arm absorbs
// every number whose as_i64 is None (so the required-done int arm only ever wins on a
// clean complete i64 token), the float arm is not required-done in v0.223, and bool/null
// are intrinsically complete. [TestJsonValueStreamPartialEqualsFinal] pins that as a named
// BAML-vs-BAML fact.

package staticoracle

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"slices"
	"sort"
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

// bamlJsonValueStreamPartial returns BAML's public partial for a prefix via ParseStream:
// ("", false) if it errored (no emit), else (marshaled bytes, true). A successfully
// decoded typed NIL is an EMIT whose bytes are `null`.
func bamlJsonValueStreamPartial(text string) (string, bool) {
	v, err := bamlclient.ParseStream.StaticRecursiveAliasJsonValue(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR:" + merr.Error(), true
	}
	return string(b), true
}

// bamlJsonValueFinal returns BAML's public FINAL for a completed text via Parse.
func bamlJsonValueFinal(text string) (string, bool) {
	v, err := bamlclient.Parse.StaticRecursiveAliasJsonValue(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR:" + merr.Error(), true
	}
	return string(b), true
}

// nativeAliasStreamPartial drives the GATE-FREE partial route
// (debaml.ParseAliasStreamPartial) → the narrow stream carrier decoder
// (stream_types.JsonValue, a *Union6 pointer union) → re-marshal — i.e. exactly what the
// production seam WOULD do if the stream gate were open. The recover is a GENERIC backstop
// that fails the test on any unexpected panic.
//
// It is the SINGLE strict error classifier for the gate-free partial route, shared by every
// caller (the JsonValue ledger, the JsonValue prefix differential, and the comparative
// shipped-`JSON` measurement). ONLY bamlutils.ErrDeBAMLParseUnsupported counts as a
// decline/no-emit; any other error is a CLAIMED parse failure and fails the test
// immediately. Having exactly one implementation is deliberate: a second, looser copy is
// how a genuine native regression gets silently absorbed into a residual baseline.
func nativeAliasStreamPartial(t *testing.T, bundle *schema.Bundle, prefix string) (out string, emit bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("native ParseAliasStreamPartial PANICKED on prefix %q: %v", prefix, r)
		}
	}()
	res, emitted, err := debaml.ParseAliasStreamPartial(bundle, prefix)
	if err != nil {
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("native ParseAliasStreamPartial(%q): non-decline error %v (a claimed parse failure must not be swallowed as no-emit)", prefix, err)
		}
		return "", false
	}
	if !emitted {
		return "", false
	}
	// Decode through the SAME narrow stream carrier decoder the generated seam would use,
	// then re-marshal. A `null` partial decodes to a TYPED NIL *Union6 — a present value,
	// so it is re-marshaled (to `null`) exactly like any other, never turned into a
	// no-emit.
	dec, derr := bamlutils.DecodeStaticAliasStream[streamtypes.JsonValue](res)
	if derr != nil {
		t.Fatalf("DecodeStaticAliasStream(%q): %v\njson: %s", prefix, derr, res)
	}
	mb, merr := stdjson.Marshal(dec)
	if merr != nil {
		t.Fatalf("re-marshal stream carrier (%q): %v", prefix, merr)
	}
	return string(mb), true
}

// nativeAliasStreamFinal drives the GATE-FREE stream-FINAL route
// (debaml.ParseAliasStreamFinal, which is the body ParseStaticStreamFinal delegates to
// after its gate) → the narrow final decoder (types.JsonValue) → re-marshal.
//
// Same contract as [nativeAliasStreamPartial]: it is the SINGLE strict error classifier for
// the gate-free final route, and only the unsupported sentinel counts as a decline.
func nativeAliasStreamFinal(t *testing.T, bundle *schema.Bundle, text string) (string, bool) {
	t.Helper()
	res, err := debaml.ParseAliasStreamFinal(context.Background(), bundle, text)
	if err != nil {
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Fatalf("native ParseAliasStreamFinal(%q): non-decline error %v (a claimed parse failure must not be swallowed as no-emit)", text, err)
		}
		return "", false
	}
	dec, derr := bamlutils.DecodeStaticAliasFinal[types.JsonValue](res.JSON)
	if derr != nil {
		t.Fatalf("DecodeStaticAliasFinal(%q): %v\njson: %s", text, derr, res.JSON)
	}
	mb, merr := stdjson.Marshal(dec)
	if merr != nil {
		t.Fatalf("re-marshal final carrier (%q): %v", text, merr)
	}
	return string(mb), true
}

// ---- 1. the gate is closed ---------------------------------------------------------

// TestJsonValueStreamGateDeclines proves the streaming lane declines `JsonValue` at EVERY
// layer, while the FINAL lane still serves it. The asymmetry is the whole point of the
// Phase-3c admission decision, so it is asserted rather than left implicit — re-enabling
// streaming cannot happen by accident.
func TestJsonValueStreamGateDeclines(t *testing.T) {
	jv := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	js := lowerReturn(t, "StaticRecursiveAliasJSON")

	// FINAL: served.
	if !debaml.IsProvenJsonValueRecursiveAliasStaticFamily(jv) {
		t.Fatal("JsonValue must be the proven FINAL JsonValue family")
	}
	if !debaml.IsProvenServedRecursiveAliasStaticFamily(jv) {
		t.Fatal("JsonValue must be a served FINAL family")
	}
	if err := debaml.SupportsNativeFinalBundle(jv); err != nil {
		t.Fatalf("JsonValue must be FINAL-supported: %v", err)
	}

	// STREAM: declined, at the predicate, the support gate, and both parse entrypoints.
	if debaml.IsProvenRecursiveAliasStaticStreamFamily(jv) {
		t.Fatal("JsonValue must NOT be the proven static-STREAM family (a value-scoped decline behind a shape-scoped gate has no route back to BAML)")
	}
	if err := debaml.SupportsNativeStaticStreamBundle(jv); err == nil {
		t.Fatal("JsonValue must DECLINE SupportsNativeStaticStreamBundle")
	}
	if _, err := debaml.ParseStaticStreamPartial(context.Background(), jv, `[1,2`); err == nil {
		t.Fatal("the PRODUCTION partial entry must decline JsonValue")
	} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("production partial decline must be the sentinel, got %v", err)
	}
	if _, err := debaml.ParseStaticStreamFinal(context.Background(), jv, `[1,2]`); err == nil {
		t.Fatal("the PRODUCTION stream-final entry must decline JsonValue")
	} else if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("production stream-final decline must be the sentinel, got %v", err)
	}

	// The `JSON` family's stream admission is UNTOUCHED by that narrowing.
	if !debaml.IsProvenRecursiveAliasStaticStreamFamily(js) {
		t.Fatal("the JSON family must still be the proven static-STREAM family")
	}
	if err := debaml.SupportsNativeStaticStreamBundle(js); err != nil {
		t.Fatalf("the JSON family must still be stream-supported: %v", err)
	}
}

// ---- 2. the residual ledger --------------------------------------------------------

// jsonValueStreamResidual is the EXACT set of corpus inputs for which the `JsonValue`
// alias stream parser does not reproduce stock BAML v0.223. It is the complete blocker
// list for opening the streaming gate: while it is non-empty, a claimed stream could lose
// a BAML partial or finish as a terminal native error, and the gate stays closed.
//
// Every entry is a SHARED limitation of native's deliberately-conservative jsonish
// extractor (#583) — a bare root scalar that is not strict JSON, an unquoted token
// containing whitespace, prose with no container, multiple top-level values, a
// triple-quoted / backtick string, a deferred escape, or the greedy object-value cascade —
// EXCEPT the negative-zero rows, which are this family's own documented value decline
// (internal/debaml/alias_coerce.go errNegativeZeroFloat).
//
// The shipped `JSON` streaming family has a residual of the SAME kind and comparable size
// (TestJSONStreamResidualIsComparable below measures it), so closing this set means
// closing shared extractor debt, not `JsonValue`-specific debt.
var jsonValueStreamResidual = []string{
	// this family's own value decline: a strict-parsed negative zero
	`-0`, `-0.0`, `[-0]`, `{"z":-0}`, `[1,-0]`,
	// bare ROOT scalars that are not strict JSON (native's final extractor has no
	// `{`/`[` to anchor its fixing pass on, so the FINAL declines; the PARTIAL is exact)
	`nul`, `NaN`, `Infinity`, `-`, `+1`, `.5`, `5.`, `007`, `1e400`, `abc`, `1.`, `1.2e`,
	// unquoted tokens containing WHITESPACE (native's scalar ends at the first space)
	`1 2`, `true false`, `null x`, `value: 1`, `hello world`,
	// prose with NO container
	`the answer is 42`, `I think 1.5`, `Sure! null`,
	// multiple top-level values (BAML's inferred-array / multi-json)
	`[1] [2]`, `{"a":1} {"b":2}`, `{"a":1}{"b":2}`,
	// triple-quoted / backtick strings, and a fence whose body is a bare scalar
	`"""hi"""`, "`hi`", "```\nhi\n```", "```json\nnull", "```json\ntrue", "```json\n1.5",
	// escapes native defers
	`"a\/b"`, `"a\zb"`,
	// the greedy InObjectValue cascade + missing-comma junk
	`{"a":1,}`, `{"z":1,"a":2`, `{"n":1,"m":x y`, `[1 2]`, `{"a":1 "b":2}`,
}

// jsonValueStreamCorpus is the FULL corpus: everything the parser is expected to own, PLUS
// every ledger entry. Nothing is carved out — the ledger entries are exercised here and
// classified, they are simply expected to land on the ledger rather than to match.
func jsonValueStreamCorpus() []string {
	rows := append([]string{}, jsonValueStreamSpecimens()...)
	rows = append(rows, jsonValueStreamResidual...)
	return rows
}

// streamDivergence classifies one input on the gate-free stream parser.
type streamDivergence struct {
	in   string
	kind string // LOST-PARTIAL | WRONG-PARTIAL | TERMINAL-FINAL | WRONG-FINAL
	baml string
	got  string
}

// classifyStreamInput reports every way the gate-free parser diverges from BAML for one
// COMPLETE input, in exactly the terms that matter on a claimed stream: a partial BAML
// emits and native does not (a lost event), a partial whose bytes differ, a final BAML
// produces and native declines (a TERMINAL native error), and a final whose bytes differ.
func classifyStreamInput(t *testing.T, bundle *schema.Bundle, in string) []streamDivergence {
	t.Helper()
	var out []streamDivergence
	bp, be := bamlJsonValueStreamPartial(in)
	np, ne := nativeAliasStreamPartial(t, bundle, in)
	switch {
	case be && !ne:
		out = append(out, streamDivergence{in, "LOST-PARTIAL", bp, "NO-EMIT"})
	case be && ne && bp != np:
		out = append(out, streamDivergence{in, "WRONG-PARTIAL", bp, np})
	}
	bf, bfe := bamlJsonValueFinal(in)
	nf, nfe := nativeAliasStreamFinal(t, bundle, in)
	switch {
	case bfe && !nfe:
		out = append(out, streamDivergence{in, "TERMINAL-FINAL", bf, "DECLINE"})
	case bfe && nfe && bf != nf:
		out = append(out, streamDivergence{in, "WRONG-FINAL", bf, nf})
	}
	return out
}

// TestJsonValueStreamResidualLedger pins [jsonValueStreamResidual] as EXACT over the full
// corpus. It fails in BOTH directions on purpose:
//
//   - a NEW divergent input means the parser regressed (or the corpus grew into untested
//     ground) and the gate must stay closed for a newly-discovered reason;
//   - a ledger entry that no longer diverges means the residual SHRANK, and the fix must
//     be recorded — when the ledger reaches empty, the streaming gate can be opened
//     (internal/debaml/static_stream_serve.go names this test as that precondition).
//
// This is what "cover the values rather than carve them out" means here: `-0` and the bare
// root scalars are exercised by this test, their exact BAML-vs-native behaviour is
// asserted, and they are the stated reason streaming is not admitted.
func TestJsonValueStreamResidualLedger(t *testing.T) {
	bundle := lowerReturn(t, "StaticRecursiveAliasJsonValue")

	diverged := map[string][]streamDivergence{}
	for _, in := range jsonValueStreamCorpus() {
		if ds := classifyStreamInput(t, bundle, in); len(ds) > 0 {
			diverged[in] = ds
		}
	}

	want := map[string]bool{}
	for _, in := range jsonValueStreamResidual {
		want[in] = true
	}

	var unexpected, healed []string
	for in := range diverged {
		if !want[in] {
			unexpected = append(unexpected, in)
		}
	}
	for in := range want {
		if _, ok := diverged[in]; !ok {
			healed = append(healed, in)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(healed)

	for _, in := range unexpected {
		for _, d := range diverged[in] {
			t.Errorf("UNEXPECTED stream divergence %-14s %-28q baml=%-26s native=%s", d.kind, d.in, d.baml, d.got)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("%d input(s) diverge that are NOT on the residual ledger; the stream parser regressed or the corpus grew", len(unexpected))
	}
	for _, in := range healed {
		t.Errorf("ledger entry %q no longer diverges — the residual SHRANK; remove it from jsonValueStreamResidual, and when the ledger is EMPTY open the streaming gate", in)
	}

	// Report the ledger's composition so the blocker's shape is visible in CI output.
	byKind := map[string]int{}
	for _, ds := range diverged {
		for _, d := range ds {
			byKind[d.kind]++
		}
	}
	t.Logf("JsonValue STREAM RESIDUAL LEDGER: %d/%d corpus inputs diverge, %+v — streaming gate stays CLOSED",
		len(diverged), len(jsonValueStreamCorpus()), byKind)
}

// TestJSONStreamResidualIsComparable measures the SHIPPED `JSON` streaming family against
// the same corpus. It is not a pass/fail gate on `JSON` (that family is already served and
// this slice must not change it) — it exists so the Phase-3c admission decision is
// grounded in a measurement rather than an assertion: the residual that blocks `JsonValue`
// streaming is SHARED extractor debt of comparable size, not something the float/null arms
// introduced.
func TestJSONStreamResidualIsComparable(t *testing.T) {
	bundle := lowerReturn(t, "StaticRecursiveAliasJSON")
	jsonDiverged := 0
	byKind := map[string]int{}
	for _, in := range jsonValueStreamCorpus() {
		// Drive the SAME strict helpers the JsonValue ledger uses. They Fatalf on any
		// error that is not the unsupported sentinel, so a genuine native regression
		// surfaces as a test failure instead of being counted as expected residual debt
		// in the very baseline this test exists to police. The `JSON` family decodes
		// through the *Union6 carrier here purely as a byte channel — the comparison is
		// against `JSON`'s own BAML oracles below, and the JSON alias never emits a value
		// the six-arm carrier cannot round-trip.
		diverged := false
		bp, be := jsonStreamOracle(in)
		np, ne := nativeAliasStreamPartial(t, bundle, in)
		switch {
		case be && !ne:
			byKind["LOST-PARTIAL"]++
			diverged = true
		case be && ne && bp != np:
			byKind["WRONG-PARTIAL"]++
			diverged = true
		}
		bf, bfe := jsonFinalOracle(in)
		nf, nfe := nativeAliasStreamFinal(t, bundle, in)
		switch {
		case bfe && !nfe:
			byKind["TERMINAL-FINAL"]++
			diverged = true
		case bfe && nfe && bf != nf:
			byKind["WRONG-FINAL"]++
			diverged = true
		}
		if diverged {
			jsonDiverged++
		}
	}
	t.Logf("SHIPPED JSON stream residual over the same corpus: %d/%d inputs diverge, %+v",
		jsonDiverged, len(jsonValueStreamCorpus()), byKind)
	// A guard, not a gate: if the shipped family's residual ever collapses to zero, the
	// shared extractor debt has been paid off and BOTH gates should be revisited.
	if jsonDiverged == 0 {
		t.Error("the shipped JSON streaming residual is now EMPTY — the shared extractor debt is closed; re-evaluate the JsonValue streaming gate")
	}
}

// ---- 3. the strict per-prefix differential over the parser's own surface ------------

// jsonValueStreamSpecimens is the strict per-prefix corpus: every shape the `JsonValue`
// stream parser OWNS, with the new float and null arms at the root, inside lists, inside
// maps, and in arm-reselection mixtures. Its complement — the inputs the parser does NOT
// own — is [jsonValueStreamResidual], which is exercised and asserted by
// [TestJsonValueStreamResidualLedger] rather than omitted.
func jsonValueStreamSpecimens() []string {
	rows := []string{
		// roots: int / float / bool / string / null + numeric token boundaries
		`1`, `-7`, `42`, `0`, `100`, `9223372036854775807`, `-9223372036854775808`,
		`1.0`, `1.5`, `3.0`, `-2.5`, `0.1`, `2.5e-3`, `1.2e5`, `1.2e-5`,
		`1e20`, `1e21`, `1e-7`, `5e-324`, `1.7976931348623157e308`,
		`9223372036854775808`, `-9223372036854775809`, `123456789012345678901234567890`,
		`1e5`, `true`, `false`, `null`,
		// quoted / escaped strings + multibyte specimens
		`"hi"`, `"a\"b"`, `""`, `"1"`, `"1.5"`, `"true"`, `"null"`, `"a\nb"`, `"a\\b"`,
		`"café ☕ 漢"`, `"<tag> & </tag>"`, `"a\tb\rc"`, `["漢字","x"]`, `{"kéy":"☕"}`,
		// lists (empty / open / closed / nested) with the new arms
		`[1,2,3]`, `[]`, `[1,"x",true]`, `[null]`, `[1,null,2]`, `[null,null]`, `[[null]]`,
		`[1.5,2.5]`, `[1.0,2]`, `[1.,2.]`, `[1e,2]`, `[null,1.5,"x",true]`,
		`[[]]`, `[[1],[2,3]]`, `[[[42]]]`, `[[1.5],[2]]`,
		// maps (empty / dup-overwrite / null value / float value / order)
		`{"a":1}`, `{}`, `{"a":1,"b":"two"}`, `{"z":1,"a":2,"z":3}`, `{"n":null}`,
		`{"a":1,"a":"two"}`, `{"k":1,"k":2,"k":3}`, `{"z":1,"a":2}`, `{"a":"bad","z":3}`,
		`{"f":1.5,"n":null,"i":7}`, `{"z":1,"a":1.5,"z":null}`, `{"a":1,"b":null}`,
		`{"k":{"n":null}}`, `{"f":1.}`, `{"f":1e}`,
		// arm-reselection + list sibling-hint mixtures across ALL SEVEN arms
		`[1,"x",2,true,3]`, `["a",1,"b",2]`, `[{"a":1},2,"x"]`, `[[1],{"k":2},3]`,
		`[1,1.5,"x",true,null]`, `[1.5,"x",2,null,true]`, `[null,1,null,1.5,null]`,
		// alternating list/map nesting (arbitrary depth) both directions
		`[{"a":[1,2]},{"b":["x"]}]`, `{"list":[{"k":1},{"k":2}]}`, `[[1],[2,3],{"m":[true]}]`,
		`{"outer":{"z":1,"a":2,"z":9}}`, `[{"a":[1,{"z":3,"y":4}]}]`, `[1,null,{"k":null}]`,
		// COMMENT specimens (jsonish strips string-aware comments before parse)
		`[1.5,null]//trailing note`, `{"a":null}/*block*/`, "[1,/*x*/1.5,null]",
		"{\n// line\n\"a\":1.5}",
		// markdown / prose boundaries with CONTAINER content
		"```json\n[null,1.5]\n```", "here: {\"a\":1.5,\"n\":null}", "```\n{\"k\":null}\n```",
		// EOF-unclosed structures
		`{"a":[1.5,null`, `[{"k":`, `{"a":null`, `[null`, `[1.`, `{"a":1.`,
	}
	// A deep alternating list/map case beyond the Phase-3a depth-40 (no cap), bottoming
	// out on the new float arm.
	var deep strings.Builder
	const depth = 60
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

// TestJsonValueStreamPrefixDifferential is the STRICT per-prefix differential over the
// surface the parser owns: for every VALID-UTF-8 byte prefix of every specimen it compares
// stock BAML v0.223 ParseStream → json.Marshal against the gate-free native path
// (ParseAliasStreamPartial → DecodeStaticAliasStream[stream_types.JsonValue] →
// json.Marshal), on both emit-vs-no-emit and exact bytes; then ONCE per specimen at the
// COMPLETE input it compares BAML Parse against ParseAliasStreamFinal →
// DecodeStaticAliasFinal[types.JsonValue]. No #583-deferred branch inside the corpus:
// anything that would need one is on the residual ledger instead, where it is asserted.
//
// A decoded typed-nil carrier is an EMIT whose bytes are `null` — the helper deliberately
// re-marshals it rather than short-circuiting on nil, so a present null is compared as the
// value it is.
func TestJsonValueStreamPrefixDifferential(t *testing.T) {
	bundle := lowerReturn(t, "StaticRecursiveAliasJsonValue")
	specimens := jsonValueStreamSpecimens()
	// The specimen list and the ledger must be DISJOINT: a specimen that is also a ledger
	// entry would be silently excused here.
	for _, s := range specimens {
		if slices.Contains(jsonValueStreamResidual, s) {
			t.Fatalf("specimen %q is also on the residual ledger; the strict corpus and the blocker list must be disjoint", s)
		}
	}

	total, match, mismatch := 0, 0, 0
	finalTotal, finalMatch := 0, 0
	emitted, typedNullEmits := 0, 0

	for _, spec := range specimens {
		b := []byte(spec)
		loggedForSpec := 0
		for i := 1; i <= len(b); i++ {
			prefix := string(b[:i])
			// The production parser only ever sees VALID-UTF-8 accumulated text (each
			// ParseableDelta is a complete JSON string), so a mid-multibyte split never
			// reaches it. A Unicode specimen's VALID prefixes still exercise multibyte
			// content.
			if !utf8.ValidString(prefix) {
				continue
			}
			total++
			wantBytes, wantEmit := bamlJsonValueStreamPartial(prefix)
			gotBytes, gotEmit := nativeAliasStreamPartial(t, bundle, prefix)

			if wantEmit == gotEmit && (!wantEmit || wantBytes == gotBytes) {
				match++
				if gotEmit {
					emitted++
					if gotBytes == "null" {
						typedNullEmits++
					}
				}
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
		// valid-UTF-8 prefix loop, so a specimen ending mid-multibyte can never silently
		// drop its FINAL comparison.
		finalTotal++
		wf, wfe := bamlJsonValueFinal(spec)
		gf, gfe := nativeAliasStreamFinal(t, bundle, spec)
		if wfe == gfe && (!wfe || wf == gf) {
			finalMatch++
		} else {
			t.Errorf("FINAL MISMATCH spec=%q\n  BAML Parse:            %s\n  ParseAliasStreamFinal: %s",
				spec, emitDesc(wf, wfe), emitDesc(gf, gfe))
		}
	}
	t.Logf("JSONVALUE PREFIX DIFFERENTIAL: partial total=%d match=%d mismatch=%d (%.1f%%); emitted=%d typed-null-emits=%d; final total=%d match=%d",
		total, match, mismatch, 100*float64(match)/float64(total), emitted, typedNullEmits, finalTotal, finalMatch)
	if mismatch > 0 {
		t.Errorf("prefix PARTIAL differential has %d/%d MISMATCHES", mismatch, total)
	}
	if finalMatch != finalTotal {
		t.Errorf("FINAL differential has %d/%d mismatches", finalTotal-finalMatch, finalTotal)
	}
	if finalTotal != len(specimens) {
		t.Errorf("FINAL differential covered %d/%d specimens; a complete input was skipped", finalTotal, len(specimens))
	}
	// Coverage guard for the crux of the null arm: the corpus MUST have produced present
	// typed-null partials.
	if typedNullEmits == 0 {
		t.Error("no typed-null partial was emitted; the present-null cadence is no longer covered")
	}
}

// TestJsonValueStreamPartialEqualsFinal pins the CADENCE FINDING as a named fact on BAML's
// own two entry points: for `JsonValue`, ParseStream(prefix) is byte-identical to
// Parse(prefix) at every valid-UTF-8 prefix of the whole corpus — nothing is dropped by
// semantic streaming.
//
// It is deliberately a BAML-vs-BAML assertion: if a future v0.223-behaviour change
// reintroduces a drop, this fails with a precise diagnosis instead of surfacing as an
// unexplained row mismatch. It runs over the FULL corpus (specimens + ledger), since the
// finding is about BAML, not about what native can own.
func TestJsonValueStreamPartialEqualsFinal(t *testing.T) {
	total, same := 0, 0
	for _, spec := range jsonValueStreamCorpus() {
		b := []byte(spec)
		for i := 1; i <= len(b); i++ {
			prefix := string(b[:i])
			if !utf8.ValidString(prefix) {
				continue
			}
			total++
			fb, fe := bamlJsonValueFinal(prefix)
			sb, se := bamlJsonValueStreamPartial(prefix)
			if fe == se && (!fe || fb == sb) {
				same++
				continue
			}
			t.Errorf("STREAM != FINAL for prefix %q: Parse=%s ParseStream=%s (the no-drop cadence finding no longer holds)",
				prefix, emitDesc(fb, fe), emitDesc(sb, se))
		}
	}
	t.Logf("JsonValue stream==final: %d/%d prefixes", same, total)
}

// jsonStreamOracle / jsonFinalOracle are the `JSON` family's ParseStream / Parse oracles,
// used by the comparative residual measurement above.
func jsonStreamOracle(text string) (string, bool) {
	v, err := bamlclient.ParseStream.StaticRecursiveAliasJSON(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR", true
	}
	return string(b), true
}

func jsonFinalOracle(text string) (string, bool) {
	v, err := bamlclient.Parse.StaticRecursiveAliasJSON(text)
	if err != nil {
		return "", false
	}
	b, merr := stdjson.Marshal(v)
	if merr != nil {
		return "MARSHALERR", true
	}
	return string(b), true
}
