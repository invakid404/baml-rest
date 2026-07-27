package debaml

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// De-BAML Phase 3c — the BYTE-UNCHANGED regression for everything the slice touched but
// must not move.
//
// Phase 3c parameterized machinery the shipped lanes share: the alias coercer and stream
// coercer now take a [recAliasProfile], the fixer and the extraction cascade now take a
// [numMode], and [value] grew a numSerdeFloat provenance tag. Each of those is a place a
// refactor could silently change the PROVEN `JSON` (Phase 3a final + Phase 3b stream),
// Phase-2 recursive-class, 8C static, or dynamic outputs.
//
// This file is the CGO-free half of the proof: it pins the `JSON` family's exact final and
// per-prefix stream bytes — chosen to hit precisely the code paths the parameterization
// changed — plus the mode-selection invariant that keeps every non-JsonValue lane on the
// legacy classification. The remaining halves are the frozen integration manifests, which
// this slice does not edit:
//
//	internal/nativeprompt/staticoracle/recursive_alias_oracle_integration_test.go   (3a final)
//	internal/nativeprompt/staticoracle/alias_stream_prefix_differential_test.go     (3b prefix)
//	internal/nativebody/nanollmprepare/staticserve/generated_stream_serve_e2e_test.go (3b SSE
//	                                                                                   + the
//	  stream decline partition, which still contains StaticRecursiveAliasJsonValue)
//	internal/nativebody/nanollmprepare/staticserve/cutover_manifest_test.go         (8C 5/5/5/5/5/2/3/1)
//	internal/nativebody/nanollmprepare/staticserve/recursive_cutover_manifest_test.go (Phase 2)
//	integration/stream_typespace_differential_test.go                               (dynamic 289/157/132)

// TestPhase3cRegression_JSONFinalBytesUnchanged pins the Phase-3a `JSON` FINAL bytes for
// the shapes most exposed to the Phase-3c refactor: numbers (which now flow through a
// mode-aware classifier and an int leaf that consults numSerdeFloat), the null -> [] trap
// (which the nullable fast path must never reach for this family), and the map/list
// ordering the profile threading passes through.
func TestPhase3cRegression_JSONFinalBytesUnchanged(t *testing.T) {
	b := jsonAliasBundle(t)
	// Every value here is the SHIPPED Phase-3a output (the same bytes
	// TestAliasCoerce_ByteExact pins), restated against the Phase-3c-refactored code.
	cases := []struct{ in, want string }{
		// The null trap — the single most important unchanged fact: `JSON` is NOT
		// nullable, so null must still fall through every arm to the empty list.
		{`null`, `[]`}, {`[null]`, `[[]]`}, {`{"n":null}`, `{"n":[]}`},
		{`[1,null,2]`, `[1,[],2]`}, {`{"a":1,"b":null}`, `{"a":1,"b":[]}`},
		{`[null,null]`, `[[],[]]`}, {`[[null]]`, `[[[]]]`},
		// No float arm: a float-valued number still rounds through FloatToInt.
		{`1.5`, `2`}, {`3.0`, `3`}, {`-2.5`, `-3`}, {`0.1`, `0`}, {`2.5`, `3`},
		{`1e3`, `1000`}, {`-0`, `0`}, {`-0.0`, `0`}, {`0.0`, `0`},
		// i64 saturation (unchanged by the numSerdeFloat tag, which is never set here).
		{`9223372036854775807`, `9223372036854775807`},
		{`9223372036854775808`, `-9223372036854775808`},
		{`1e20`, `9223372036854775807`},
		// Terminals, numeric strings, composites, ordering, escaping.
		{`1`, `1`}, {`-7`, `-7`}, {`0`, `0`}, {`true`, `true`}, {`false`, `false`},
		{`"1"`, `"1"`}, {`"1.5"`, `"1.5"`}, {`"hello"`, `"hello"`}, {`""`, `""`},
		{`[]`, `[]`}, {`{}`, `{}`}, {`[1,"1",true]`, `[1,"1",true]`},
		{`{"z":1,"a":2}`, `{"a":2,"z":1}`}, {`{"z":1,"a":2,"z":3}`, `{"a":2,"z":3}`},
		{`{"a":1,"a":"two"}`, `{"a":"two"}`}, {`{"k":1,"k":2,"k":3}`, `{"k":3}`},
		{`[{"a":[1,2]},{"b":["x"]}]`, `[{"a":[1,2]},{"b":["x"]}]`},
		{`{"outer":{"z":1,"a":2,"z":9}}`, `{"outer":{"a":2,"z":9}}`},
		{`"<tag> & </tag>"`, `"\u003ctag\u003e \u0026 \u003c/tag\u003e"`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			res, err := ParseStaticBundle(context.Background(), b, tc.in)
			if err != nil {
				t.Fatalf("JSON ParseStaticBundle(%q): %v (Phase 3c must not make a shipped row decline)", tc.in, err)
			}
			if string(res.JSON) != tc.want {
				t.Fatalf("JSON FINAL BYTES MOVED for %q:\n got:  %s\n want: %s", tc.in, res.JSON, tc.want)
			}
		})
	}
}

// TestPhase3cRegression_JSONStreamBytesUnchanged pins the Phase-3b `JSON` PARTIAL bytes,
// including the family's float-shaped-number DROP — the behaviour that has NO counterpart
// in `JsonValue` and is therefore the likeliest thing a shared-code refactor would break.
func TestPhase3cRegression_JSONStreamBytesUnchanged(t *testing.T) {
	b := jsonAliasBundle(t)
	cases := []struct{ in, want string }{
		// The required-done int DROP on a float-shaped root token → the [] fallback.
		{`1.`, `[]`}, {`1.5`, `[]`}, {`3.0`, `[]`}, {`-2.5`, `[]`}, {`-0`, `[]`},
		{`1e5`, `[]`}, {`007`, `[]`}, {`5.`, `[]`}, {`+1`, `[]`},
		// null → [] (the non-nullable list fallback), at the root and nested.
		{`null`, `[]`}, {`[null`, `[[]]`}, {`{"a":null`, `{"a":[]}`},
		{`[1,null,2]`, `[1,[],2]`},
		// Clean integers, bools and strings are KEPT mid-stream.
		{`1`, `1`}, {`-7`, `-7`}, {`[1`, `[1]`}, {`[1,2`, `[1,2]`},
		{`true`, `true`}, {`false`, `false`}, {`t`, `"t"`}, {`tru`, `"tru"`},
		{`n`, `"n"`}, {`nu`, `"nu"`}, {`nul`, `"nul"`}, {`-`, `"-"`},
		{`"hi`, `"hi"`}, {`"`, `""`},
		// A number-ish but non-canonical root token is ALSO the [] fallback for `JSON`
		// (isNumberishToken), where `JsonValue` keeps it as the string arm — the exact
		// per-family split this regression exists to hold apart.
		{`1e`, `[]`}, {`1.2e`, `[]`},
		// Containers.
		{`[`, `[]`}, {`{`, `{}`}, {`{"a":`, `{}`}, {`{"a":1`, `{"a":1}`},
		{`[1,"x"`, `[1,"x"]`},
		// The InObjectValue GREEDY cascade (a comma followed by tight content is absorbed
		// into one raw unquoted span) — shipped Phase-3b behaviour, unchanged.
		{`{"z":1,"a":2`, `{"z":"1,\"a\":2"}`},
		{`{"z":1, "a":2`, `{"a":2,"z":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			out, emit, err := ParseAliasStreamPartial(b, tc.in)
			if err != nil {
				t.Fatalf("JSON ParseAliasStreamPartial(%q): %v", tc.in, err)
			}
			if !emit {
				t.Fatalf("JSON ParseAliasStreamPartial(%q): NO-EMIT, want %s (a shipped partial must not disappear)", tc.in, tc.want)
			}
			if string(out) != tc.want {
				t.Fatalf("JSON PARTIAL BYTES MOVED for %q:\n got:  %s\n want: %s", tc.in, out, tc.want)
			}
		})
	}
}

// TestPhase3cRegression_LegacyNumModeIsUniversal is the structural half of the
// byte-unchanged proof: [numModeSerde] — the ONLY thing that can change how a bare
// numeric/unquoted token is classified — must be selected for the `JsonValue` bundle and
// for NOTHING else. If a future change makes it the default, or attaches it to the `JSON`
// bundle, every legacy lane's extracted candidate could shift; this catches that at the
// selector rather than through downstream byte diffs.
func TestPhase3cRegression_LegacyNumModeIsUniversal(t *testing.T) {
	if got := bundleNumMode(jsonAliasBundle(t)); got != numModeLegacy {
		t.Fatalf("the JSON alias bundle selects numMode %v, want numModeLegacy", got)
	}
	if got := bundleNumMode(nil); got != numModeLegacy {
		t.Fatalf("a nil bundle selects numMode %v, want numModeLegacy", got)
	}
	if got := bundleNumMode(jsonValueAliasBundle(t)); got != numModeSerde {
		t.Fatalf("the JsonValue alias bundle selects numMode %v, want numModeSerde", got)
	}
	// The default zero value of the mode MUST be the legacy classification, so any
	// unparameterized caller (and any future one) keeps today's behaviour.
	var zero numMode
	if zero != numModeLegacy {
		t.Fatal("the numMode zero value must be numModeLegacy")
	}
	// And the legacy classifier itself is unchanged: it still DECLINES the non-strict-JSON
	// numeric forms rather than adopting BAML's PATH-B string fallback.
	for _, tok := range []string{`.5`, `1.`, `0x10`, `1e`, `abc`, `1_000`} {
		if _, err := classifyScalarMode(tok, numModeLegacy); err == nil {
			t.Errorf("legacy classifyScalar(%q) now SUCCEEDS; the conservative decline moved", tok)
		}
	}
	// …while the serde classifier never declines (it has a string arm to land on).
	for _, tok := range []string{`.5`, `1.`, `0x10`, `1e`, `abc`, `1_000`, `true`, `null`} {
		if _, err := classifyScalarMode(tok, numModeSerde); err != nil {
			t.Errorf("serde classifyScalar(%q) declined: %v", tok, err)
		}
	}
}

// TestPhase3cRegression_PairGuardIdentityUnchanged pins that the numSerdeFloat provenance
// tag is NOT part of [valueEqual]. The pair guard's (name, value) membership is the
// Phase-2/3a/3b circular-reference contract; folding a new field into it would change
// which recursive descents error, for every family at once.
func TestPhase3cRegression_PairGuardIdentityUnchanged(t *testing.T) {
	plain := value{kind: valNumber, numV: "-0"}
	tagged := value{kind: valNumber, numV: "-0", numSerdeFloat: true}
	if !valueEqual(plain, tagged) {
		t.Fatal("valueEqual now distinguishes numSerdeFloat; the pair-guard identity contract moved")
	}
	// The facts valueEqual DOES carry must still be carried.
	if valueEqual(plain, value{kind: valNumber, numV: "-0", incomplete: true}) {
		t.Fatal("valueEqual must still distinguish completion state")
	}
	if valueEqual(plain, value{kind: valNumber, numV: "0"}) {
		t.Fatal("valueEqual must still distinguish the raw number token")
	}
}

// deepNestingOnce guards the DEEP (depth-1000) no-depth-cap witness so it runs EXACTLY
// ONCE per test binary regardless of -count.
//
// The property under test is STRUCTURAL and deterministic — the coercer either imposes a
// bound or it does not — and this test has no concurrency and no randomness, so repeating
// it detects nothing that the first run does not. `-race -count=100` exists to shake out
// data races and flakes; paying 100x for a deterministic structural assertion is pure
// waste, and here it was actively harmful (see the cost note below).
var deepNestingOnce sync.Once

// TestPhase3cRegression_JSONDeepNestingUnchanged re-runs the NO-DEPTH-CAP proof for the
// `JSON` family through the profile-threaded coercer: the Phase-3c parameterization must
// not have introduced a bound. The objective this slice is held to requires no depth cap,
// so this is the test that pins it.
//
// # Where each depth runs, and why
//
//   - depths 40 and 200 run on EVERY iteration of the always-on unit lane, so they get the
//     full `-race -count=100` exercise. 200 is already clear of every plausible
//     hard-coded bound (32/64/100/128).
//   - depth 1000 — the genuine deep witness — runs ONCE per test binary via
//     [deepNestingOnce]. It still runs in the SAME always-on lane, on every CI run; it is
//     simply not repeated 100 times.
//
// # Why not the two obvious alternatives
//
// NOT testing.Short(): this repo's CI never passes `-short` (verified across all
// workflows), so a Short-gated deep case would run at the full 100x cost in CI — i.e. it
// would not fix anything. `testing.Short()` here would only skip the witness for local
// developers, which is exactly backwards.
//
// NOT an integration-tagged lane: no workflow runs `go test -tags integration` over
// `./internal/...` — the only lane that executes this package is the untagged unit lane.
// Moving the witness behind a build tag would mean it runs in FEWER places, weakening the
// guarantee rather than preserving it.
//
// # The cost that forced this arrangement
//
// Coercion cost is superlinear in depth (the path-local pair-guard walks its frame chain at
// every level, and each frame comparison is a structural valueEqual over the subtree),
// measured on this tree at 200 -> 0.08s, 400 -> 0.61s, 1000 -> 10.33s. The unit-tests job
// runs the root module as `go test -race -count=100 -timeout 20m ./...` under a 35-minute
// job cap, so an unguarded depth-1000 row is ~17 minutes before race overhead — it blew
// both budgets and cancelled the job on two consecutive heads.
//
// IMPORTANT: it is the TEST that was made cheaper, never the parser. No depth cap was
// introduced in production; depth 1000 still parses and coerces successfully here, and the
// integration lane additionally exercises depth 40 (final differential) and 60 (per-prefix
// differential) against the live oracle. If you are tempted to drop the depth-1000 case to
// save the remaining ~10s, do not: it is the only witness for the deep end of the
// guarantee.
func TestPhase3cRegression_JSONDeepNestingUnchanged(t *testing.T) {
	b := jsonAliasBundle(t)
	check := func(depth int) {
		t.Helper()
		var in, want strings.Builder
		for i := 0; i < depth; i++ {
			in.WriteString(`{"k":[`)
			want.WriteString(`{"k":[`)
		}
		in.WriteString(`42`)
		want.WriteString(`42`)
		for i := 0; i < depth; i++ {
			in.WriteString(`]}`)
			want.WriteString(`]}`)
		}
		res, err := ParseStaticBundle(context.Background(), b, in.String())
		if err != nil {
			t.Fatalf("depth %d: %v (a depth cap appeared)", depth, err)
		}
		if string(res.JSON) != want.String() {
			t.Fatalf("depth %d bytes moved", depth)
		}
	}
	// Cheap depths: every iteration.
	for _, depth := range []int{40, 200} {
		check(depth)
	}
	// Deep witness: once per binary. A failure here still fails the test that observes it.
	deepNestingOnce.Do(func() { check(1000) })
}
