package bamlutils

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// De-BAML Slice 7.2b-1 — the carrier's UNIT half.
//
// Everything here is byte-exact against a literal: the assertions compare the raw
// marshalled []byte, never "no error" and never a re-decoded document, because the
// whole point of the carrier is which BYTES come out. The stock provenance of those
// literals is established separately and independently, against the real BAML
// v0.223.0 CFFI, by internal/debaml/checkedwire — this file pins that the carrier
// keeps producing them, under BOTH encoders.
//
// [TestCheckedWireAssertionsAreProvenToBite] is the anti-false-green control: it
// re-implements each mutation the pinned bytes are supposed to catch (checks before
// value, Go map iteration order, a permuted check-object field order, a
// duplicate-label fold) and requires every one of them to produce DIFFERENT bytes.

// The four stock-shaped wire literals this slice is about. They are the byte strings
// the scope doc records for stock v0.223.0 and that checkedwire re-derives from the
// CFFI.
const (
	wantCheckedIntSucceeded = `{"value":5,"checks":{"gt":{"name":"gt","expression":"this > 0","status":"succeeded"}}}`
	wantCheckedIntFailed    = `{"value":5,"checks":{"gt":{"name":"gt","expression":"this > 100","status":"failed"}}}`
	wantNestedChecked       = `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
)

// staticCheckedAnswer mirrors the generated static return type for the nested
// placement the scope names: a flat class whose `confidence` field carries the
// carrier. The generator's own shape (`V Checked[int64] json:"v"`) is pinned against
// the checked-in stock-generated client by checkedwire; here it only has to place a
// carrier inside a struct so the nested bytes are exercised.
type staticCheckedAnswer struct {
	Answer     string         `json:"answer"`
	Confidence Checked[int64] `json:"confidence"`
}

// mustChecked builds a carrier or fails the test. Constructor errors are never
// discarded — a swallowed error here would make every byte assertion below vacuous.
func mustChecked[T any](t *testing.T, value T, ordered ...Check) Checked[T] {
	t.Helper()
	c, err := NewChecked(value, ordered)
	if err != nil {
		t.Fatalf("NewChecked(%v, %v): %v", value, ordered, err)
	}
	return c
}

// The three rewrites encoding/json applies to the output of ANY json.Marshaler.
//
// The escape TEXT is spelled out here rather than derived from the encoder, so a
// change in either direction is visible; [TestCheckedEncoderFramingDivergence] pins
// the whole composed transform against a literal on both sides.
const (
	escapedLT  = "\\u003c"
	escapedGT  = "\\u003e"
	escapedAmp = "\\u0026"
)

// stdFraming is encoding/json's HTML escaping, written out explicitly.
//
// encoding/json compacts the output of ANY json.Marshaler with escaping ON, so `<`,
// `>` and `&` come back as six-byte escapes. That reaches the canonical fixtures
// directly: stock's own `this > 0` carries a `>`. Stating the transform here rather
// than deriving the expectation from encoding/json keeps the comparison byte-exact
// against a rule this file owns.
func stdFraming(s string) string {
	return strings.NewReplacer("<", escapedLT, ">", escapedGT, "&", escapedAmp).Replace(s)
}

// requireBothEncoders asserts that sonic (the production wire route) produces exactly
// want, and that encoding/json produces exactly want under its own HTML framing.
//
// Both are required because the carrier's determinism — field order, key order, check
// field order — must not depend on which encoder a caller reached for. sonic is the
// ACCEPTANCE authority because it is what worker/parse.go and the final stream path
// use, and it is the encoder whose bytes stock's plain struct also produces.
func requireBothEncoders(t *testing.T, v any, want string) {
	t.Helper()
	gotSonic, err := sonic.Marshal(v)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	if string(gotSonic) != want {
		t.Errorf("sonic.Marshal bytes:\n got %s\nwant %s", gotSonic, want)
	}
	wantStd := stdFraming(want)
	gotStd, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("encoding/json.Marshal: %v", err)
	}
	if string(gotStd) != wantStd {
		t.Errorf("encoding/json.Marshal bytes:\n got %s\nwant %s", gotStd, wantStd)
	}
}

// TestCheckedMarshalStockWireShape pins the carrier's bytes for the three shapes the
// scope names: a succeeded check, a FALSE check (whose value is still emitted), and
// the nested class placement.
func TestCheckedMarshalStockWireShape(t *testing.T) {
	pass := mustChecked(t, int64(5), Check{Name: "gt", Expression: "this > 0", Status: CheckSucceeded})
	requireBothEncoders(t, pass, wantCheckedIntSucceeded)

	// A false check keeps its value — that is the whole difference between @check and
	// @assert, and the byte that carries it is `"value":5` surviving beside "failed".
	fail := mustChecked(t, int64(5), Check{Name: "gt", Expression: "this > 100", Status: CheckFailed})
	requireBothEncoders(t, fail, wantCheckedIntFailed)
	if !strings.Contains(wantCheckedIntFailed, `"value":5`) {
		t.Fatal("the failed-check literal does not carry a value; the fixture would not witness the @check/@assert split")
	}

	nested := staticCheckedAnswer{
		Answer:     "sunny",
		Confidence: mustChecked(t, int64(9), Check{Name: "positive", Expression: "this > 0", Status: CheckSucceeded}),
	}
	requireBothEncoders(t, nested, wantNestedChecked)

	// A POINTER to the carrier must marshal identically: the method is on the value
	// receiver precisely so both forms reach it, and a *Checked field in a generated
	// struct is a shape the generator can emit.
	requireBothEncoders(t, &pass, wantCheckedIntSucceeded)
}

// TestCheckedMarshalUsesDeclarationOrder proves the recorded order — not the sorted
// order, and not Go's — decides the key sequence, under BOTH encoders.
//
// The labels are chosen so declaration order and lexicographic order DISAGREE: a
// carrier that fell back to sorting would emit alpha,mid,zeta and fail here, which is
// what makes this test distinguish the two code paths rather than merely observe one.
//
// The encoding/json half is load-bearing rather than decorative. encoding/json sorts
// MAP keys lexicographically on its own, so a single-check carrier — and the hand-built
// lexicographic-fallback carrier in [TestCheckedMarshalNilAndEmptyChecks] and
// [TestCheckedDecodedCarrierHasNoTrustedOrder] — would emit identical bytes whether or
// not the custom marshal ran at all. Only a MULTI-check carrier whose declaration order
// is not the sorted one can tell "encoding/json honoured MarshalJSON" from
// "encoding/json fell back to encoding the struct itself", which is exactly what this
// fixture is.
func TestCheckedMarshalUsesDeclarationOrder(t *testing.T) {
	c := mustChecked(t, int64(1),
		Check{Name: "zeta", Expression: "this > 0", Status: CheckSucceeded},
		Check{Name: "alpha", Expression: "this < 9", Status: CheckSucceeded},
		Check{Name: "mid", Expression: "this != 4", Status: CheckFailed},
	)
	const want = `{"value":1,"checks":{` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"},` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"},` +
		`"mid":{"name":"mid","expression":"this != 4","status":"failed"}}}`
	requireBothEncoders(t, c, want)

	sorted := `{"value":1,"checks":{` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"},` +
		`"mid":{"name":"mid","expression":"this != 4","status":"failed"},` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"}}}`
	if want == sorted {
		t.Fatal("the fixture's declaration order equals its lexicographic order, so this test cannot " +
			"tell the recorded order from the fallback")
	}
	// The DISCRIMINATING negative, stated per encoder: neither may emit the sorted
	// order, which is both the documented fallback AND what encoding/json produces on
	// its own if the custom marshal is bypassed.
	gotSonic, err := sonic.Marshal(c)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	if string(gotSonic) == sorted {
		t.Fatalf("sonic emitted LEXICOGRAPHIC order for a constructed carrier: %s", gotSonic)
	}
	gotStd, err := stdjson.Marshal(c)
	if err != nil {
		t.Fatalf("encoding/json.Marshal: %v", err)
	}
	if string(gotStd) == stdFraming(sorted) {
		t.Fatalf("encoding/json emitted LEXICOGRAPHIC order for a constructed carrier, so it did not "+
			"honour MarshalJSON: %s", gotStd)
	}
}

// TestCheckedMarshalIsStableAcrossRuns is the direct refutation of Go map iteration.
//
// Go randomises map iteration per range statement, so an implementation that ranged
// over Checks would produce a different key sequence on essentially every call for
// this many labels. Marshalling the same carrier repeatedly and requiring byte
// equality every time is therefore a live measurement rather than a claim, and it
// covers BOTH order sources: the constructed carrier and the hand-built one that
// falls back to sorting.
func TestCheckedMarshalIsStableAcrossRuns(t *testing.T) {
	const labels = 8
	ordered := make([]Check, 0, labels)
	byName := make(map[string]Check, labels)
	for i := labels - 1; i >= 0; i-- { // declared in DESCENDING label order
		c := Check{Name: fmt.Sprintf("c%d", i), Expression: fmt.Sprintf("this > %d", i), Status: CheckSucceeded}
		ordered = append(ordered, c)
		byName[c.Name] = c
	}
	constructed := mustChecked(t, int64(7), ordered...)
	// The hand-built twin: same map, NO recorded order, so it takes the documented
	// lexicographic fallback.
	handBuilt := Checked[int64]{Value: 7, Checks: byName}

	encoders := []struct {
		name string
		fn   func(any) ([]byte, error)
	}{
		{"sonic", sonic.Marshal},
		{"encoding/json", func(v any) ([]byte, error) { return stdjson.Marshal(v) }},
	}
	requireStable := func(enc string, marshal func(any) ([]byte, error), name string, v any) string {
		t.Helper()
		first, err := marshal(v)
		if err != nil {
			t.Fatalf("%s: %s.Marshal: %v", name, enc, err)
		}
		for i := 0; i < 100; i++ {
			again, err := marshal(v)
			if err != nil {
				t.Fatalf("%s: %s.Marshal (run %d): %v", name, enc, i, err)
			}
			if string(again) != string(first) {
				t.Fatalf("%s: %s bytes changed between runs (run %d); the key order is not deterministic:\n %s\n %s",
					name, enc, i, first, again)
			}
		}
		return string(first)
	}

	declared := descendingLabels(labels)
	lexicographic := descendingLabels(labels)
	sort.Strings(lexicographic)
	if checkedEqualStrings(declared, lexicographic) {
		t.Fatal("the fixture's declaration order equals its lexicographic order, so neither carrier's " +
			"key order could distinguish the recorded order from the fallback")
	}

	// BOTH encoders, over BOTH order sources. encoding/json is not a duplicate run: it
	// sorts map keys itself, so the constructed carrier's DESCENDING key order is the
	// only thing that shows encoding/json went through MarshalJSON rather than encoding
	// the struct.
	for _, enc := range encoders {
		constructedBytes := requireStable(enc.name, enc.fn, "constructed", constructed)
		fallbackBytes := requireStable(enc.name, enc.fn, "hand-built", handBuilt)

		// And the two orders are the ones documented, not merely stable ones.
		if got := checkKeyOrder(t, constructedBytes); !checkedEqualStrings(got, declared) {
			t.Fatalf("%s: constructed carrier key order = %v, want the declaration order %v", enc.name, got, declared)
		}
		if got := checkKeyOrder(t, fallbackBytes); !checkedEqualStrings(got, lexicographic) {
			t.Fatalf("%s: hand-built carrier key order = %v, want the lexicographic fallback %v", enc.name, got, lexicographic)
		}
	}
}

func descendingLabels(n int) []string {
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, fmt.Sprintf("c%d", i))
	}
	return out
}

func checkedEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkKeyOrder recovers the `checks` key sequence from raw bytes by SCANNING them.
//
// It deliberately does not decode into a map: a map would destroy the very ordering
// under test. The scan is anchored on the `"checks":{` prefix the carrier is required
// to emit, so a carrier that moved `checks` would fail here rather than be re-ordered
// into agreement.
func checkKeyOrder(t *testing.T, raw string) []string {
	t.Helper()
	const anchor = `,"checks":{`
	i := strings.Index(raw, anchor)
	if i < 0 {
		t.Fatalf("no %q in %s", anchor, raw)
	}
	var out []string
	rest := raw[i+len(anchor):]
	for {
		q := strings.Index(rest, `"`)
		if q < 0 {
			return out
		}
		rest = rest[q+1:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			t.Fatalf("unterminated key in %s", raw)
		}
		key := rest[:end]
		out = append(out, key)
		// Skip past this entry's object, which the carrier always writes as exactly
		// {"name":…,"expression":…,"status":…}.
		closeAt := strings.Index(rest, "}")
		if closeAt < 0 {
			return out
		}
		rest = rest[closeAt+1:]
		if !strings.HasPrefix(rest, ",") {
			return out
		}
	}
}

// TestCheckedValueIsWrittenBeforeChecks pins the OUTER field order on its own, so a
// swap would fail with a message that names the invariant even if every other
// assertion in the file were somehow satisfied.
func TestCheckedValueIsWrittenBeforeChecks(t *testing.T) {
	c := mustChecked(t, int64(5), Check{Name: "gt", Expression: "this > 0", Status: CheckSucceeded})
	got, err := sonic.Marshal(c)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	valueAt := strings.Index(string(got), `"value"`)
	checksAt := strings.Index(string(got), `"checks"`)
	if valueAt < 0 || checksAt < 0 {
		t.Fatalf("carrier emitted neither value nor checks: %s", got)
	}
	if valueAt > checksAt {
		t.Fatalf("checks precede value; stock writes value first: %s", got)
	}
	// The check object's own field order is equally material.
	nameAt := strings.Index(string(got), `"name"`)
	exprAt := strings.Index(string(got), `"expression"`)
	statusAt := strings.Index(string(got), `"status"`)
	if !(nameAt < exprAt && exprAt < statusAt) {
		t.Fatalf("check field order is not name,expression,status: %s", got)
	}
}

// TestNewCheckedRejectsDuplicateLabels is the last line of the rule that a
// duplicate-label node must never be claimed natively.
//
// Stock's map fold is last-write-wins (measured against the CFFI by checkedwire), so
// a duplicate cannot be reproduced byte-for-byte; the admission gate declines such a
// node long before this, and the constructor refuses to build one anyway.
func TestNewCheckedRejectsDuplicateLabels(t *testing.T) {
	_, err := NewChecked(int64(5),
		[]Check{
			{Name: "dup", Expression: "this > 0", Status: CheckSucceeded},
			{Name: "dup", Expression: "this > 1", Status: CheckSucceeded},
		})
	if err == nil {
		t.Fatal("NewChecked accepted a duplicate label; stock folds it to one entry (last write wins) and " +
			"the bytes could not be proven equal")
	}
	if !errors.Is(err, ErrCheckedMalformed) {
		t.Fatalf("duplicate label error is not ErrCheckedMalformed: %v", err)
	}

	// CONTROL: the same two checks under DISTINCT labels are accepted, so the refusal
	// is about duplication rather than about two checks.
	if _, err := NewChecked(int64(5), []Check{
		{Name: "a", Expression: "this > 0", Status: CheckSucceeded},
		{Name: "b", Expression: "this > 1", Status: CheckSucceeded},
	}); err != nil {
		t.Fatalf("the distinct-label control was refused: %v", err)
	}

	// An empty label cannot come from BAML's grammar and is refused too.
	if _, err := NewChecked(int64(5), []Check{{Name: "", Expression: "this > 0", Status: CheckSucceeded}}); !errors.Is(err, ErrCheckedMalformed) {
		t.Fatalf("NewChecked accepted an empty label (err=%v)", err)
	}
}

// TestCheckedMarshalFailsForMalformedCarrier proves the carrier FAILS serialization
// rather than emitting something contradictory, for each state the doc names.
//
// Every arm asserts the error identity AND that no bytes were produced: a marshaller
// that returned partial bytes alongside an error would be just as dangerous as one
// that silently normalised.
func TestCheckedMarshalFailsForMalformedCarrier(t *testing.T) {
	cases := []struct {
		name string
		c    Checked[int64]
	}{{
		name: "map key disagrees with Check.Name",
		c: Checked[int64]{Value: 5, Checks: map[string]Check{
			"gt": {Name: "lt", Expression: "this > 0", Status: CheckSucceeded},
		}},
	}, {
		name: "recorded order is missing an entry added later",
		c: func() Checked[int64] {
			c := mustChecked(t, int64(5), Check{Name: "gt", Expression: "this > 0", Status: CheckSucceeded})
			c.Checks["extra"] = Check{Name: "extra", Expression: "this > 1", Status: CheckSucceeded}
			return c
		}(),
	}, {
		name: "recorded order names an entry deleted later",
		c: func() Checked[int64] {
			c := mustChecked(t, int64(5),
				Check{Name: "gt", Expression: "this > 0", Status: CheckSucceeded},
				Check{Name: "lt", Expression: "this < 9", Status: CheckSucceeded})
			delete(c.Checks, "lt")
			c.declaredCheckOrder = []string{"gt", "lt"}
			c.Checks["zz"] = Check{Name: "zz", Expression: "this > 2", Status: CheckSucceeded}
			return c
		}(),
	}, {
		name: "recorded order repeats a label",
		c: Checked[int64]{
			Value: 5,
			Checks: map[string]Check{
				"gt": {Name: "gt", Expression: "this > 0", Status: CheckSucceeded},
			},
			declaredCheckOrder: []string{"gt", "gt"},
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.MarshalJSON()
			if err == nil {
				t.Fatalf("MarshalJSON accepted a malformed carrier and emitted %s", got)
			}
			if !errors.Is(err, ErrCheckedMalformed) {
				t.Fatalf("error is not ErrCheckedMalformed: %v", err)
			}
			if got != nil {
				t.Fatalf("MarshalJSON returned %d byte(s) alongside its error", len(got))
			}
			// The failure must propagate through the encoders too, not be swallowed
			// into a partial document.
			if out, err := sonic.Marshal(tc.c); err == nil {
				t.Fatalf("sonic.Marshal accepted the malformed carrier: %s", out)
			}
			if out, err := stdjson.Marshal(tc.c); err == nil {
				t.Fatalf("encoding/json.Marshal accepted the malformed carrier: %s", out)
			}
		})
	}

	// CONTROL: a hand-built carrier whose key and name AGREE is accepted, in the
	// documented lexicographic order — so the refusals above are about the
	// contradiction, not about hand-built carriers.
	ok := Checked[int64]{Value: 5, Checks: map[string]Check{
		"zz": {Name: "zz", Expression: "this > 1", Status: CheckFailed},
		"aa": {Name: "aa", Expression: "this > 0", Status: CheckSucceeded},
	}}
	const want = `{"value":5,"checks":{` +
		`"aa":{"name":"aa","expression":"this > 0","status":"succeeded"},` +
		`"zz":{"name":"zz","expression":"this > 1","status":"failed"}}}`
	requireBothEncoders(t, ok, want)
}

// TestCheckedMarshalNilAndEmptyChecks pins the two degenerate maps apart. Go's own
// map encoding renders a nil map as `null` and an empty one as `{}`, and the carrier
// mirrors that rather than collapsing them, so a carrier with no map does not present
// as one with an empty map.
func TestCheckedMarshalNilAndEmptyChecks(t *testing.T) {
	requireBothEncoders(t, Checked[int64]{Value: 5}, `{"value":5,"checks":null}`)

	empty := mustChecked(t, int64(5))
	requireBothEncoders(t, empty, `{"value":5,"checks":{}}`)

	// The stock struct renders the same two ways, which is what "mirrors" means here.
	type stockShaped struct {
		Value  int64            `json:"value"`
		Checks map[string]Check `json:"checks"`
	}
	requireBothEncoders(t, stockShaped{Value: 5}, `{"value":5,"checks":null}`)
	requireBothEncoders(t, stockShaped{Value: 5, Checks: map[string]Check{}}, `{"value":5,"checks":{}}`)
}

// TestCheckedEncoderFramingDivergence records the ONE way the two encoders differ over
// this carrier, so it is a measured and delimited fact rather than a surprise.
//
// encoding/json HTML-escapes the output of any json.Marshaler; sonic's default config
// does not. A `<` in a check EXPRESSION — `@check(lt, {{ this < 100 }})` is ordinary
// BAML — is therefore six bytes under encoding/json and one under sonic. sonic is the
// wire authority (it is what the worker calls), so the sonic form is the one stock's
// plain struct also produces; checkedwire proves that equality against the CFFI.
func TestCheckedEncoderFramingDivergence(t *testing.T) {
	c := mustChecked(t, int64(5), Check{Name: "lt", Expression: "this < 100", Status: CheckSucceeded})

	const wantSonic = `{"value":5,"checks":{"lt":{"name":"lt","expression":"this < 100","status":"succeeded"}}}`
	// Written out independently of [stdFraming], so the two are cross-checked below
	// rather than one restating the other.
	wantStd := `{"value":5,"checks":{"lt":{"name":"lt","expression":"this ` + escapedLT + ` 100","status":"succeeded"}}}`
	if wantSonic == wantStd {
		t.Fatal("the two literals are identical; this test would witness nothing")
	}
	// Pin [stdFraming] itself against these two independent literals, so every other
	// encoding/json expectation in this file rests on a measured transform rather than
	// on a restatement of what encoding/json happens to do.
	if got := stdFraming(wantSonic); got != wantStd {
		t.Fatalf("stdFraming does not describe encoding/json's escaping:\n got %s\nwant %s", got, wantStd)
	}
	gotSonic, err := sonic.Marshal(c)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	if string(gotSonic) != wantSonic {
		t.Errorf("sonic bytes:\n got %s\nwant %s", gotSonic, wantSonic)
	}
	gotStd, err := stdjson.Marshal(c)
	if err != nil {
		t.Fatalf("encoding/json.Marshal: %v", err)
	}
	if string(gotStd) != wantStd {
		t.Errorf("encoding/json bytes:\n got %s\nwant %s", gotStd, wantStd)
	}
	// The divergence is encoding/json's framing of ANY marshaler, not something the
	// carrier does: the plain stock-shaped struct diverges identically.
	type stockShaped struct {
		Value  int64            `json:"value"`
		Checks map[string]Check `json:"checks"`
	}
	plain := stockShaped{Value: 5, Checks: map[string]Check{"lt": {Name: "lt", Expression: "this < 100", Status: CheckSucceeded}}}
	plainSonic, err := sonic.Marshal(plain)
	if err != nil {
		t.Fatalf("sonic.Marshal(plain): %v", err)
	}
	if string(plainSonic) != wantSonic {
		t.Errorf("the stock-shaped struct's sonic bytes differ from the carrier's:\n got %s\nwant %s", plainSonic, wantSonic)
	}
	plainStd, err := stdjson.Marshal(plain)
	if err != nil {
		t.Fatalf("encoding/json.Marshal(plain): %v", err)
	}
	if string(plainStd) != wantStd {
		t.Errorf("the stock-shaped struct's encoding/json bytes differ from the carrier's:\n got %s\nwant %s", plainStd, wantStd)
	}
}

// checkedDecoders is the pair every decode claim is made over. sonic is the wire
// authority, but both must reach [Checked.UnmarshalJSON] — a reset that held for only
// one of them would be a trap for whichever caller used the other.
func checkedDecoders() []struct {
	name string
	fn   func([]byte, any) error
} {
	return []struct {
		name string
		fn   func([]byte, any) error
	}{
		{"encoding/json", func(b []byte, v any) error { return stdjson.Unmarshal(b, v) }},
		{"sonic", func(b []byte, v any) error { return sonic.Unmarshal(b, v) }},
	}
}

// TestCheckedDecodedCarrierHasNoTrustedOrder proves the fallback is the state a
// DECODED carrier lands in, under both decoders.
//
// A carrier that came back from JSON has no declaration order — the wire never
// carried one — so it must serialize lexicographically rather than pick up whatever
// order the decoder's map happened to have.
func TestCheckedDecodedCarrierHasNoTrustedOrder(t *testing.T) {
	const wire = `{"value":5,"checks":{` +
		`"zz":{"name":"zz","expression":"this > 1","status":"failed"},` +
		`"aa":{"name":"aa","expression":"this > 0","status":"succeeded"}}}`
	const wantReMarshalled = `{"value":5,"checks":{` +
		`"aa":{"name":"aa","expression":"this > 0","status":"succeeded"},` +
		`"zz":{"name":"zz","expression":"this > 1","status":"failed"}}}`

	for _, dec := range checkedDecoders() {
		t.Run(dec.name, func(t *testing.T) {
			var c Checked[int64]
			if err := dec.fn([]byte(wire), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c.Value != 5 || len(c.Checks) != 2 {
				t.Fatalf("decoded carrier is %+v, want value 5 and two checks", c)
			}
			requireBothEncoders(t, c, wantReMarshalled)
		})
	}
}

// TestCheckedDecodedCarrierReplacesReusedState proves a decode inherits NEITHER half
// of a target [NewChecked] previously built: no stale map entry, and no trusted
// declaration order.
//
// The target deliberately starts as zeta,alpha,stale in a NON-lexicographic stored
// order. The payload replaces zeta and alpha, omits stale, and presents its own keys in
// the opposite of the required fallback order. So the exact alpha,zeta bytes asserted at
// the end are a live discriminator for BOTH reset requirements at once: decoding into
// the exported map would keep `stale`, and keeping the private order would either emit
// zeta,alpha or make the carrier malformed and fail to serialize at all.
//
// Both decoders are driven because both reach the same custom unmarshaller — that is
// the property that makes one implementation enough.
func TestCheckedDecodedCarrierReplacesReusedState(t *testing.T) {
	const wire = `{"value":5,"checks":{` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"},` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"}}}`
	const wantReMarshalled = `{"value":5,"checks":{` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"},` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"}}}`
	// What a carrier that RETAINED the reused map would serialize to. Stated so the
	// expectation above is known to differ from the bug it is meant to catch.
	const retainedState = `{"value":5,"checks":{` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"},` +
		`"stale":{"name":"stale","expression":"this != 0","status":"failed"},` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"}}}`
	if wantReMarshalled == retainedState || wire == wantReMarshalled {
		t.Fatal("the reused-target fixture cannot distinguish the required replacement from retained state")
	}

	for _, dec := range checkedDecoders() {
		t.Run(dec.name, func(t *testing.T) {
			c := mustChecked(t, int64(99),
				Check{Name: "zeta", Expression: "this > 900", Status: CheckFailed},
				Check{Name: "alpha", Expression: "this < -9", Status: CheckFailed},
				Check{Name: "stale", Expression: "this != 0", Status: CheckFailed},
			)
			if got := c.declaredCheckOrder; !checkedEqualStrings(got, []string{"zeta", "alpha", "stale"}) {
				t.Fatalf("the fixture lost its stored declaration order before decoding (%v), so this test "+
					"would not witness the reset", got)
			}

			if err := dec.fn([]byte(wire), &c); err != nil {
				t.Fatalf("unmarshal into a reused carrier: %v", err)
			}
			if c.Value != 5 {
				t.Fatalf("decoded value = %d, want 5", c.Value)
			}
			if len(c.Checks) != 2 {
				t.Fatalf("decoded Checks has %d entry/entries, want exactly the two the wire carried: %v",
					len(c.Checks), c.Checks)
			}
			if _, stale := c.Checks["stale"]; stale {
				t.Fatalf("the reused carrier retained the stale check: %v", c.Checks)
			}
			for _, want := range []Check{
				{Name: "zeta", Expression: "this > 0", Status: CheckSucceeded},
				{Name: "alpha", Expression: "this < 9", Status: CheckSucceeded},
			} {
				if got := c.Checks[want.Name]; got != want {
					t.Fatalf("decoded %s = %+v, want the wire check %+v", want.Name, got, want)
				}
			}
			if c.declaredCheckOrder != nil {
				t.Fatalf("the decoded carrier retained a trusted declaration order: %v", c.declaredCheckOrder)
			}

			// Through BOTH encoders. The literal is LEXICOGRAPHIC even though the wire
			// listed zeta first, so this also proves the decoded carrier took the
			// documented no-trusted-order fallback rather than the stale order.
			requireBothEncoders(t, c, wantReMarshalled)
		})
	}
}

// TestCheckedDecodeReplacesRatherThanMerges pins the other half of "replacement":
// a field the document does NOT carry is reset, not inherited.
//
// Go's default struct decoding leaves an absent field untouched, which is exactly how a
// stale value would survive. The carrier's contract is that after a decode it holds what
// the document said and nothing else, so an absent `value` yields the zero value and an
// absent `checks` yields the nil map (which marshals as `null`, matching Go's own map
// encoding).
func TestCheckedDecodeReplacesRatherThanMerges(t *testing.T) {
	for _, dec := range checkedDecoders() {
		t.Run(dec.name, func(t *testing.T) {
			c := mustChecked(t, int64(99), Check{Name: "stale", Expression: "this != 0", Status: CheckFailed})
			const noValue = `{"checks":{"beta":{"name":"beta","expression":"this > 0","status":"succeeded"}}}`
			if err := dec.fn([]byte(noValue), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c.Value != 0 {
				t.Fatalf("an absent `value` left the STALE value %d in place; the decode merged instead of "+
					"replacing", c.Value)
			}
			requireBothEncoders(t, c, `{"value":0,"checks":{"beta":{"name":"beta","expression":"this > 0","status":"succeeded"}}}`)

			c2 := mustChecked(t, int64(7), Check{Name: "stale", Expression: "this != 0", Status: CheckFailed})
			if err := dec.fn([]byte(`{"value":3}`), &c2); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c2.Checks != nil {
				t.Fatalf("an absent `checks` left the STALE map in place: %v", c2.Checks)
			}
			requireBothEncoders(t, c2, `{"value":3,"checks":null}`)
		})
	}
}

// TestCheckedUnmarshalIsAtomicOnError makes the fresh-temporaries rule observable.
//
// The input is syntactically COMPLETE, so both outer decoders dispatch to
// [Checked.UnmarshalJSON] rather than failing before it. Its inner decode consumes a
// valid `value` and a valid `checks` object and only then meets a second, incompatible
// `value` — encoding/json's LATE type error, reported after the rest of the object has
// already been assigned. An implementation that decoded straight into the receiver would
// have mutated it by then; this one must leave the constructed carrier untouched, stored
// order included.
func TestCheckedUnmarshalIsAtomicOnError(t *testing.T) {
	const invalidAfterFields = `{"value":5,"checks":{"beta":{"name":"beta","expression":"this > 0","status":"succeeded"}},"value":"not-an-int"}`
	// The ORIGINAL bytes, in the constructor's non-lexicographic order — so this also
	// witnesses that the private order survived the failed decode.
	const wantOriginal = `{"value":99,"checks":{` +
		`"zeta":{"name":"zeta","expression":"this > 900","status":"failed"},` +
		`"alpha":{"name":"alpha","expression":"this < -9","status":"failed"}}}`

	for _, dec := range checkedDecoders() {
		t.Run(dec.name, func(t *testing.T) {
			c := mustChecked(t, int64(99),
				Check{Name: "zeta", Expression: "this > 900", Status: CheckFailed},
				Check{Name: "alpha", Expression: "this < -9", Status: CheckFailed},
			)
			if err := dec.fn([]byte(invalidAfterFields), &c); err == nil {
				t.Fatal("an incompatible `value` decoded successfully")
			}
			requireBothEncoders(t, c, wantOriginal)
		})
	}
}

// TestCheckedUnmarshalIsStrict pins the inner decode's strictness, and — the reason it
// matters — proves it is what keeps [DecodeStaticFinal] strict over a carrier.
//
// A json.Unmarshaler takes over its whole subtree, so DecodeStaticFinal's
// DisallowUnknownFields stops applying inside the carrier the moment this method exists.
// If the method were lenient, a field stock cannot produce would be silently DROPPED by
// the one decoder the generated static path uses. The final arm is that end-to-end
// claim, not a restatement of the unit one.
func TestCheckedUnmarshalIsStrict(t *testing.T) {
	const unknownField = `{"value":5,"checks":{},"surprise":1}`
	const unknownCheckField = `{"value":5,"checks":{"a":{"name":"a","expression":"x","status":"succeeded","extra":1}}}`
	const trailing = `{"value":5,"checks":{}} {"value":6,"checks":{}}`

	for _, dec := range checkedDecoders() {
		t.Run(dec.name, func(t *testing.T) {
			for _, tc := range []struct{ name, wire string }{
				{"unknown carrier field", unknownField},
				{"unknown check field", unknownCheckField},
			} {
				var c Checked[int64]
				if err := dec.fn([]byte(tc.wire), &c); err == nil {
					t.Errorf("%s was accepted; the carrier's decode is not strict", tc.name)
				}
			}
			// CONTROL: the same documents without the unknown field are accepted, so the
			// rejections are about strictness rather than about the shape.
			var ok Checked[int64]
			if err := dec.fn([]byte(`{"value":5,"checks":{"a":{"name":"a","expression":"x","status":"succeeded"}}}`), &ok); err != nil {
				t.Fatalf("the well-formed control was rejected: %v", err)
			}
		})
	}

	// Trailing content, driven through the method directly: the outer decoders hand it
	// exactly one value, so this branch is only reachable from a direct call.
	var direct Checked[int64]
	if err := direct.UnmarshalJSON([]byte(trailing)); err == nil {
		t.Error("a trailing second JSON value was accepted")
	}
	// The nil-receiver guard, likewise unreachable through the encoders (both allocate).
	var nilCarrier *Checked[int64]
	if err := nilCarrier.UnmarshalJSON([]byte(`{"value":1,"checks":null}`)); err == nil {
		t.Error("UnmarshalJSON on a nil receiver was accepted")
	}

	// THE END-TO-END CLAIM: the strict static decoder still rejects an unknown field
	// inside a carrier. Without the strict inner decode this passes and drops it.
	if got, err := DecodeStaticFinal[Checked[int64]]([]byte(unknownField)); err == nil {
		t.Errorf("DecodeStaticFinal accepted an unknown field inside the carrier and produced %+v; "+
			"defining UnmarshalJSON has silently disabled its DisallowUnknownFields", got)
	}
	// CONTROL: the well-formed document decodes through that same path, and re-marshals
	// in the documented fallback order.
	good, err := DecodeStaticFinal[Checked[int64]]([]byte(`{"value":5,"checks":{` +
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"},` +
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"}}}`))
	if err != nil {
		t.Fatalf("DecodeStaticFinal rejected a well-formed carrier: %v", err)
	}
	requireBothEncoders(t, good, `{"value":5,"checks":{`+
		`"alpha":{"name":"alpha","expression":"this < 9","status":"succeeded"},`+
		`"zeta":{"name":"zeta","expression":"this > 0","status":"succeeded"}}}`)
}

// ---------------------------------------------------------------------------
// The anti-false-green control.
// ---------------------------------------------------------------------------

// TestCheckedWireAssertionsAreProvenToBite re-implements every mutation the pinned
// bytes are supposed to reject and requires each one to produce DIFFERENT bytes.
//
// Without this, a literal that happened to match a wrong implementation would be
// indistinguishable from one that pins the right one. Each mutant below is a
// plausible alternative implementation of the same carrier; if any of them produced
// the pinned bytes, the corresponding assertion in this file would be decoration.
func TestCheckedWireAssertionsAreProvenToBite(t *testing.T) {
	value := int64(1)
	ordered := []Check{
		{Name: "zeta", Expression: "this > 0", Status: CheckSucceeded},
		{Name: "alpha", Expression: "this < 9", Status: CheckSucceeded},
		{Name: "mid", Expression: "this != 4", Status: CheckFailed},
	}
	c := mustChecked(t, value, ordered...)
	actualBytes, err := sonic.Marshal(c)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}

	entry := func(ch Check) string {
		return fmt.Sprintf(`{"name":%q,"expression":%q,"status":%q}`, ch.Name, ch.Expression, ch.Status)
	}
	entriesInOrder := func(names []string) string {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%q:%s", n, entry(c.Checks[n]))
		}
		return strings.Join(parts, ",")
	}
	names := []string{"zeta", "alpha", "mid"}
	sortedNames := append([]string(nil), names...)
	sort.Strings(sortedNames)

	mutants := []struct {
		name string
		out  string
	}{{
		// checks BEFORE value.
		name: "checks emitted before value",
		out:  fmt.Sprintf(`{"checks":{%s},"value":%d}`, entriesInOrder(names), value),
	}, {
		// Go map iteration order is not reproducible, but ANY order other than the
		// declared one is what it would produce most of the time; the sorted order is
		// a concrete, deterministic representative of "not the declaration order".
		name: "keys in sorted (i.e. not declaration) order",
		out:  fmt.Sprintf(`{"value":%d,"checks":{%s}}`, value, entriesInOrder(sortedNames)),
	}, {
		name: "check fields permuted to status,name,expression",
		out: fmt.Sprintf(`{"value":%d,"checks":{"zeta":{"status":%q,"name":%q,"expression":%q},"alpha":%s,"mid":%s}}`,
			value, CheckSucceeded, "zeta", "this > 0", entry(c.Checks["alpha"]), entry(c.Checks["mid"])),
	}, {
		// What dropping the duplicate-label rejection looks like: two declarations
		// folded into one entry, stock's last-write-wins.
		name: "duplicate labels folded last-write-wins",
		out: fmt.Sprintf(`{"value":%d,"checks":{"zeta":%s,"alpha":%s,"mid":%s}}`, value,
			entry(Check{Name: "zeta", Expression: "this > 3", Status: CheckFailed}),
			entry(c.Checks["alpha"]), entry(c.Checks["mid"])),
	}}
	for _, m := range mutants {
		if m.out == string(actualBytes) {
			t.Errorf("the %q mutant produces the SAME bytes as the carrier, so no assertion in this file "+
				"distinguishes them:\n%s", m.name, actualBytes)
		}
	}
	// And the honest control: the carrier's own rendering, rebuilt from the same
	// helper, DOES match — so the helper is capable of producing the real bytes and
	// the four inequalities above are about the mutations rather than about the
	// helper's formatting.
	if want := fmt.Sprintf(`{"value":%d,"checks":{%s}}`, value, entriesInOrder(names)); want != string(actualBytes) {
		t.Fatalf("the mutation harness cannot reproduce the carrier's own bytes, so its inequalities prove "+
			"nothing:\n got %s\nwant %s", actualBytes, want)
	}

	// The duplicate mutant is not merely a different string: the constructor refuses
	// to build it at all, which is what makes it unreachable rather than merely
	// unequal.
	if _, err := NewChecked(value, []Check{
		{Name: "zeta", Expression: "this > 0", Status: CheckSucceeded},
		{Name: "zeta", Expression: "this > 3", Status: CheckFailed},
	}); !errors.Is(err, ErrCheckedMalformed) {
		t.Fatalf("the duplicate-label mutant is CONSTRUCTIBLE (err=%v)", err)
	}
}
