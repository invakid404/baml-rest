package debaml

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3c (the `JsonValue` recursive-alias family) — BAML v0.223's NUMBER
// classification, the fact the `float` arm's scoring crux rests on.
//
// `JSON` has no float arm, so native never had to decide whether a jsonish number is
// stored as an INTEGER or a FLOAT: every number reached the single `int` arm. With
// BOTH an `int` (arm 0) and a `float` (arm 1) arm, BAML's union tries strict casts in
// declaration order and returns the FIRST score-zero match — strict `int` accepts a
// number only through `as_i64`, strict `float` through `as_f64` — so the ARM is
// decided entirely by whether the parsed value's `as_i64` is Some. That in turn is
// decided by the PARSER, and BAML v0.223 has TWO number-producing paths with
// DIFFERENT rules (LIVE-CAPTURED against stock v0.223, see the table below):
//
//	PATH A — the whole candidate is valid strict JSON (serde_json parses it).
//	  The number is serde_json's own representation:
//	    * an integer token in i64 range        -> as_i64 Some  -> INT
//	    * `-0`                                 -> f64 -0.0     -> FLOAT (negative zero
//	      is preserved, so as_i64 is None — this is the ONE integer-looking strict-JSON
//	      token that is NOT an int)
//	    * an integer token OUT of i64 range    -> as_i64 None  -> FLOAT
//	    * any `.` / `e` / `E` form             -> as_i64 None  -> FLOAT
//	  A token serde_json REJECTS (an f64 OVERFLOW such as `1e400`) fails the WHOLE
//	  strict parse, so the entire candidate re-parses through PATH B.
//
//	PATH B — the fixing parser's unquoted-literal conversion.
//	  Tried in order: Rust `i64` -> Rust `u64` -> Rust `f64` (which must be FINITE,
//	  because serde_json::Number::from_f64 returns None for NaN/±Inf); if all three
//	  miss, the token stays an unquoted STRING.
//	    * `-0`, `-00`, `007`, `+1`  -> i64      -> INT   (no negative-zero preservation
//	      here: `-0`.parse::<i64>() is Ok(0), so a fixing-parsed `-0` is the INTEGER 0)
//	    * `1.`, `.5`, `5.`, `-.5`   -> f64      -> FLOAT
//	    * `1e`, `1e400`, `0x10`, `1_000`, `NaN`, `Infinity`, `nul`, `abc` -> STRING
//
// The observable split (LIVE, stock v0.223 `Parse.StaticRecursiveAliasJsonValue`):
//
//	`[-0]`   -> [-0]    (PATH A: valid JSON        -> float -0.0)
//	`[-0,]`  -> [0]     (PATH B: trailing comma    -> i64 0)
//	`[-0`    -> [0]     (PATH B: unclosed          -> i64 0)
//
// Native's own parser has the same two paths ([strictDecode] and the fixing parser),
// so the model is reproduced by (a) tagging a PATH-A number whose `as_i64` is None
// with [value.numSerdeFloat], (b) FAILING a PATH-A decode that contains a token
// serde_json would reject so the candidate falls through to the fixing parser, and
// (c) giving the fixing parser a PATH-B scalar classifier. All three are gated behind
// [numModeSerde], which ONLY the `JsonValue` lane selects — [numModeLegacy] keeps the
// `JSON` family, the Phase-2 class families, and the dynamic 289/157/132 universe
// byte-for-byte unchanged.

// numMode selects how a parse classifies bare numeric/unquoted scalar tokens.
type numMode uint8

const (
	// numModeLegacy is the Phase-3a/3b/Phase-2/dynamic classification: a bare token is
	// true/false/null or a STRICT-JSON number, and anything else DECLINES (the
	// conservative under-claim that keeps the proven lanes' bytes fixed). Every
	// pre-Phase-3c caller passes this, so their behaviour is untouched.
	numModeLegacy numMode = iota
	// numModeSerde is the Phase-3c `JsonValue` classification: BAML v0.223's exact
	// two-path number model documented in this file's header. Selected ONLY by the
	// admitted `JsonValue` alias lane.
	numModeSerde
)

// serdeParsesNumber reports whether serde_json would ACCEPT the strict-JSON number
// token tok. serde_json rejects a token whose f64 magnitude overflows (`1e400`), and
// that rejection fails the WHOLE strict parse — BAML then re-parses the candidate with
// its fixing parser. Native reproduces that by failing its own strict decode here.
//
// [parseF64Rust] already returns ok=false on a Go range error, so an overflowing token
// is caught; the finiteness re-check guards the "inf"/"nan" SPELLINGS parseF64Rust
// accepts (they cannot appear in strict JSON, but the check keeps the predicate
// honest at its own boundary).
func serdeParsesNumber(tok string) bool {
	f, ok := parseF64Rust(tok)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// serdeStoresAsFloat reports whether serde_json stores the strict-JSON number token
// tok as an f64 rather than an integer — i.e. whether `as_i64` is None, which is
// exactly what makes BAML's strict `int` arm miss and the `float` arm win.
//
// Within the strict-JSON number grammar the only integer-looking token that is stored
// as a float is `-0` (negative zero is preserved); every other token either i64-parses
// (an int) or does not (a `.`/`e` form, or an out-of-i64-range integer — both floats).
func serdeStoresAsFloat(tok string) bool {
	if tok == "-0" {
		return true // negative zero: serde_json keeps the sign as f64 -0.0
	}
	_, ok := parseI64Rust(tok)
	return !ok
}

// bamlNumberToken reports how BAML's FIXING parser (PATH B) converts the unquoted
// token s: (numeric=true) when one of Rust i64 / u64 / FINITE f64 parses it — the
// token then becomes a jsonish Number carrying its raw text — and (numeric=false) when
// none do, in which case BAML keeps the token as an unquoted STRING.
//
// The raw token text is retained (rather than a re-formatted number) because every
// downstream reader re-derives the value from it: the `int` arm through
// [parseI64Rust] and the `float` arm through [parseF64Rust], which reproduce
// `as_i64` / `as_f64` on the value BAML stored.
func bamlNumberToken(s string) bool {
	if _, ok := parseI64Rust(s); ok {
		return true
	}
	if _, ok := parseU64Rust(s); ok {
		return true
	}
	f, ok := parseF64Rust(s)
	// serde_json::Number::from_f64 returns None for NaN/±Inf, so a non-finite parse
	// (the "inf" / "nan" / "Infinity" spellings, and any overflowing literal) is NOT a
	// number — BAML keeps it as a string ("1e400" -> `"1e400"`, LIVE-CAPTURED).
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// classifyScalarSerde is the PATH-B (fixing parser) bare-token conversion for the
// `JsonValue` lane: the three exact keywords, then BAML's i64/u64/finite-f64 number
// test, then an unquoted STRING. Unlike [classifyScalar] it NEVER declines — BAML's
// fixing parser always produces a value for a bare token, and the `JsonValue` family
// has a `string` arm to receive whatever is not a number.
func classifyScalarSerde(raw string) (value, error) {
	s := strings.TrimSpace(raw)
	switch s {
	case "true":
		return value{kind: valBool, boolV: true}, nil
	case "false":
		return value{kind: valBool, boolV: false}, nil
	case "null":
		return value{kind: valNull}, nil
	}
	if bamlNumberToken(s) {
		return value{kind: valNumber, numV: json.Number(s)}, nil
	}
	return value{kind: valString, strV: s}, nil
}

// classifyScalarMode dispatches the bare-token conversion by parse mode: the
// unchanged conservative [classifyScalar] for every legacy lane, BAML's exact PATH-B
// conversion for the `JsonValue` lane.
func classifyScalarMode(raw string, nm numMode) (value, error) {
	if nm == numModeSerde {
		return classifyScalarSerde(raw)
	}
	return classifyScalar(raw)
}

// bundleNumMode selects the parse mode for a bundle: [numModeSerde] for the admitted
// `JsonValue` alias family (which needs BAML's exact int-vs-float number model),
// [numModeLegacy] for EVERY other bundle — the `JSON` alias family, the Phase-2 class
// families, the 8C static shapes, and the whole dynamic universe — so their extracted
// candidates stay byte-for-byte what they are today.
func bundleNumMode(b *schema.Bundle) numMode {
	if IsProvenJsonValueRecursiveAliasStaticFamily(b) {
		return numModeSerde
	}
	return numModeLegacy
}

// profileNumMode is [bundleNumMode] for a caller that already classified the family.
func profileNumMode(prof recAliasProfile) numMode {
	if prof.isJsonValue() {
		return numModeSerde
	}
	return numModeLegacy
}
