package debaml

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// numV builds a JSON-number value from a literal token.
func numV(tok string) value { return value{kind: valNumber, numV: json.Number(tok)} }

// strV builds a JSON-string value.
func strVv(s string) value { return value{kind: valString, strV: s} }

// TestFloatFromCommaSeparated ports BAML's own test_float_from_comma_separated
// table (coerce_primitive.rs) VERBATIM, pinning the extracted-number regex,
// comma stripping, \p{Sc} currency removal, and the "exactly one match"
// requirement byte-for-byte against Rust. The key semantic anchors: "50%" is
// 50.0 (percent is NOT part of the match and NOT divided), a currency symbol
// before OR after a number is stripped, US thousands-commas collapse, European
// decimal-commas and multi-number strings yield no match.
func TestFloatFromCommaSeparated(t *testing.T) {
	type tc struct {
		in   string
		want float64
		ok   bool
	}
	cases := []tc{
		// European formats (deliberately unsupported where noted in BAML).
		{"3,14", 314.0, true},
		{"1.234,56", 0, false},
		{"1.234.567,89", 0, false},
		{"€1.234,56", 0, false},
		{"-€1.234,56", 0, false},
		{"€1.234", 1.234, true}, // TODO in BAML - technically incorrect
		{"1.234€", 1.234, true}, // TODO in BAML - technically incorrect
		{"€1,234.56", 1234.56, true},
		// US formats.
		{"3,000", 3000.0, true},
		{"3,100.00", 3100.00, true},
		{"1,234.56", 1234.56, true},
		{"1,234,567.89", 1234567.89, true},
		{"$1,234.56", 1234.56, true},
		{"-$1,234.56", -1234.56, true},
		{"$1,234", 1234.0, true},
		{"1,234$", 1234.0, true},
		{"+$1,234.56", 1234.56, true},
		{"$9,999,999,999", 9999999999.0, true},
		{"$1.23.456", 0, false},
		{"$1.234.567.890", 0, false},
		{"$314", 314.0, true},
		// Indian format.
		{"$1,23,456", 123456.0, true},
		// Percentages.
		{"50%", 50.0, true},
		{"3.15%", 3.15, true},
		{".009%", 0.009, true},
		{"1.234,56%", 0, false},
		{"$1,234.56%", 1234.56, true},
		// Strings containing numbers.
		{"The answer is 10,000", 10000.0, true},
		{"The total is €1.234,56 today", 0, false},
		{"You owe $3,000 for the service", 3000.0, true},
		{"Save up to 20% on your purchase", 20.0, true},
		{"Revenue grew by 1,234.56 this quarter", 1234.56, true},
		{"Profit is -€1.234,56 in the last month", 0, false},
		// Multiple numbers -> no single match.
		{"The answer is 10,000 and $3,000", 0, false},
		{"We earned €1.234,56 and $2,345.67 this year", 0, false},
		{"Increase of 5% and a profit of $1,000", 0, false},
		{"Loss of -€500 and a gain of 1,200.50", 0, false},
		{"Targets: 2,000 units and €3.000,75 revenue", 0, false},
		// Trailing periods and commas.
		{"12,111,123.", 12111123.0, true},
		{"12,111,123,", 12111123.0, true},
	}
	for _, c := range cases {
		got, ok := floatFromCommaSeparated(c.in)
		if ok != c.ok {
			t.Errorf("floatFromCommaSeparated(%q) ok=%v, want %v (got %v)", c.in, ok, c.ok, got)
			continue
		}
		if ok && got != c.want {
			t.Errorf("floatFromCommaSeparated(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseF64Rust_RejectsGoOnlySpellings pins the CR-B1 fix: parseF64Rust
// rejects the float spellings Go's strconv.ParseFloat accepts but Rust's
// str::parse::<f64>() does NOT (underscores, hex floats), and reports a
// ParseFloat range error (overflow) as a decline — while still accepting every
// valid Rust decimal/exponent/leading-dot/signed form and the "inf"/"nan"
// special values (as non-finite, ok=true).
func TestParseF64Rust_RejectsGoOnlySpellings(t *testing.T) {
	reject := []string{"1_000", "0x1p4", "+0x1p4", "0X1p4", "-0x1.8p+2", "1_0.5", "1e400"}
	for _, s := range reject {
		if v, ok := parseF64Rust(s); ok {
			t.Errorf("parseF64Rust(%q) accepted (=%v), want reject (Go-only/overflow)", s, v)
		}
	}
	// Valid Rust forms accepted (finite).
	accept := map[string]float64{
		"1": 1, "+1": 1, "-1": -1, "1.5": 1.5, ".5": 0.5, "1.": 1, "+.5": 0.5,
		"1e5": 1e5, "1E5": 1e5, "-2.5e-1": -0.25,
	}
	for s, want := range accept {
		v, ok := parseF64Rust(s)
		if !ok || v != want {
			t.Errorf("parseF64Rust(%q) = (%v,%v), want (%v,true)", s, v, ok, want)
		}
	}
	// Rust also accepts "inf"/"nan" spellings; parseF64Rust returns them
	// non-finite with ok=true (the caller decides — int saturates, float declines).
	for _, s := range []string{"inf", "+inf", "-inf", "Infinity", "nan", "NaN"} {
		if _, ok := parseF64Rust(s); !ok {
			t.Errorf("parseF64Rust(%q) rejected, want accept (non-finite, ok=true)", s)
		}
	}
}

// TestFloatRaw_NonFinite pins that floatRaw refuses to emit a non-finite float
// (which has no valid JSON number form), reporting ok=false so the caller
// declines instead of emitting an invalid token.
func TestFloatRaw_NonFinite(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := floatRaw(f); ok {
			t.Errorf("floatRaw(%v) ok=true, want false (invalid JSON)", f)
		}
	}
	out, ok := floatRaw(50)
	if !ok || string(out) != "50" {
		t.Errorf("floatRaw(50) = (%q,%v), want (\"50\",true)", string(out), ok)
	}
}

// TestI64FromF64Round pins Rust's f64::round (ties away from zero) followed by
// the saturating float→int cast (NaN→0, out-of-range→i64 bound).
func TestI64FromF64Round(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{1.6, 2}, {1.4, 1}, {-1.6, -2}, {-1.4, -1},
		{0.5, 1}, {-0.5, -1}, {2.5, 3}, {-2.5, -3}, // ties away from zero
		{0.0, 0}, {-0.0, 0},
		{1.5e19, math.MaxInt64},  // > 2^63 saturates high
		{-1.5e19, math.MinInt64}, // < -2^63 saturates low
	}
	for _, c := range cases {
		if got := i64FromF64Round(c.in); got != c.want {
			t.Errorf("i64FromF64Round(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := i64FromF64Round(math.NaN()); got != 0 {
		t.Errorf("i64FromF64Round(NaN) = %d, want 0", got)
	}
	if got := i64FromF64Round(math.Inf(1)); got != math.MaxInt64 {
		t.Errorf("i64FromF64Round(+Inf) = %d, want MaxInt64", got)
	}
	if got := i64FromF64Round(math.Inf(-1)); got != math.MinInt64 {
		t.Errorf("i64FromF64Round(-Inf) = %d, want MinInt64", got)
	}
}

// TestParseU64Rust pins the u64 parse (leading '+' allowed) and the u64→i64
// two's-complement wrap that BAML's `n as i64` performs for values above
// i64::MAX (e.g. u64::MAX → -1).
func TestParseU64Rust(t *testing.T) {
	if v, ok := parseU64Rust("18446744073709551615"); !ok || int64(v) != -1 {
		t.Errorf("parseU64Rust(u64::MAX) = (%d,%v); int64 wrap = %d, want -1", v, ok, int64(v))
	}
	if v, ok := parseU64Rust("+9223372036854775808"); !ok || int64(v) != math.MinInt64 {
		t.Errorf("parseU64Rust(+2^63) = (%d,%v); int64 wrap = %d, want MinInt64", v, ok, int64(v))
	}
	if _, ok := parseU64Rust("-5"); ok {
		t.Errorf("parseU64Rust(-5) accepted, want reject")
	}
	if _, ok := parseU64Rust("abc"); ok {
		t.Errorf("parseU64Rust(abc) accepted, want reject")
	}
}

// TestTrimNumericString pins the shared preprocessing: trim() THEN
// trim_end_matches(',') with no second whitespace trim.
func TestTrimNumericString(t *testing.T) {
	cases := []struct{ in, want string }{
		{" 123, ", "123"},
		{"123, ,", "123, "}, // comma-trim stops at the space
		{"  42  ", "42"},
		{"7,,,", "7"},
	}
	for _, c := range cases {
		if got := trimNumericString(c.in); got != c.want {
			t.Errorf("trimNumericString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCoerceIntValue pins coerce_int across JSON-number and string inputs and
// the FloatToInt flag (score-bearing) on the float/fraction/extracted paths.
func TestCoerceIntValue(t *testing.T) {
	cases := []struct {
		name    string
		in      value
		want    int64
		flagged bool
		ok      bool
	}{
		{"json-int", numV("5"), 5, false, true},
		{"json-neg", numV("-7"), -7, false, true},
		{"json-u64-wrap", numV("18446744073709551615"), -1, false, true},
		{"json-float-round", numV("1.6"), 2, true, true},
		{"json-float-exact", numV("5.0"), 5, true, true}, // 5.0 is f64 -> FloatToInt
		{"str-int", strVv("123"), 123, false, true},
		{"str-int-trim-comma", strVv(" 123, "), 123, false, true},
		{"str-u64-wrap", strVv("18446744073709551615"), -1, false, true},
		{"str-float-round", strVv("1.6"), 2, true, true},
		{"str-fraction", strVv("3/2"), 2, true, true},       // round(1.5)=2
		{"str-fraction-neg", strVv("-7/2"), -4, true, true}, // round(-3.5)=-4
		{"str-extracted-currency", strVv("$1,234"), 1234, true, true},
		{"str-extracted-sentence", strVv("The answer is 10,000"), 10000, true, true},
		{"str-comma-grouped", strVv("1,234"), 1234, true, true}, // i64 parse fails on comma -> extracted
		{"str-garbage", strVv("hello"), 0, false, false},
		{"str-multi-number", strVv("1 and 2"), 0, false, false},
		{"bool-input", value{kind: valBool, boolV: true}, 0, false, false},
		// CR-B1: Go-only float spellings Rust rejects must DECLINE (no over-claim).
		{"str-hex-float-reject", strVv("0x1p4"), 0, false, false},
		{"str-underscore-reject", strVv("1_000"), 0, false, false},
		{"str-hex-fraction-reject", strVv("0x1p4/2"), 0, false, false},
		// CR-B1: non-finite ("inf"/"nan"/fraction) DECLINES (parity-safe under-claim).
		{"str-inf-declines", strVv("inf"), 0, false, false},
		{"str-nan-declines", strVv("nan"), 0, false, false},
		{"str-inf-fraction-declines", strVv("inf/2"), 0, false, false},
		// A FINITE out-of-range value still saturates and CLAIMS (matches BAML).
		{"str-finite-overflow-saturates", strVv("1e19"), math.MaxInt64, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, flagged, err := coerceIntValue(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("coerceIntValue(%+v) unexpected error: %v", c.in, err)
				}
				if got != c.want {
					t.Errorf("value = %d, want %d", got, c.want)
				}
				if flagged != c.flagged {
					t.Errorf("flagged = %v, want %v", flagged, c.flagged)
				}
			} else {
				if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Fatalf("want ErrDeBAMLParseUnsupported, got err=%v", err)
				}
			}
		})
	}
}

// TestCoerceFloatValue pins coerce_float across JSON-number and string inputs
// and the StringToFloat flag on the extracted-number path only.
func TestCoerceFloatValue(t *testing.T) {
	cases := []struct {
		name    string
		in      value
		want    float64
		flagged bool
		ok      bool
	}{
		{"json-int", numV("5"), 5, false, true},
		{"json-float", numV("3.14"), 3.14, false, true},
		{"str-float", strVv("3.14"), 3.14, false, true},
		{"str-int", strVv("42"), 42, false, true},
		{"str-fraction", strVv("3/2"), 1.5, false, true}, // fraction: no flag
		{"str-percent", strVv("50%"), 50.0, true, true},  // extracted: StringToFloat
		{"str-currency", strVv("$1,234.56"), 1234.56, true, true},
		{"str-sentence", strVv("Save up to 20% on your purchase"), 20.0, true, true},
		{"str-garbage", strVv("hello"), 0, false, false},
		{"str-multi", strVv("1 and 2"), 0, false, false},
		// CR-B1: Go-only float spellings Rust rejects must DECLINE (no over-claim).
		{"str-hex-reject", strVv("0x1p4"), 0, false, false},
		{"str-underscore-reject", strVv("1_000"), 0, false, false},
		{"str-hex-fraction-reject", strVv("0x1p4/2"), 0, false, false},
		// CR-B1: non-finite has no valid JSON number form -> DECLINE (never emit).
		{"str-nan-declines", strVv("NaN"), 0, false, false},
		{"str-inf-declines", strVv("inf"), 0, false, false},
		{"str-nan-fraction-declines", strVv("NaN/2"), 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, flagged, err := coerceFloatValue(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("coerceFloatValue(%+v) unexpected error: %v", c.in, err)
				}
				var got float64
				if e := json.Unmarshal(out, &got); e != nil {
					t.Fatalf("emitted %q is not a JSON number: %v", string(out), e)
				}
				if got != c.want {
					t.Errorf("value = %v, want %v", got, c.want)
				}
				if flagged != c.flagged {
					t.Errorf("flagged = %v, want %v", flagged, c.flagged)
				}
			} else {
				if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Fatalf("want ErrDeBAMLParseUnsupported, got err=%v", err)
				}
			}
		})
	}
}

// TestCoerceBoolValue pins coerce_bool: JSON bool (clean), casefold
// "true"/"false" (StringToBool), match_string substring (StringToBool, NOT
// SubstringMatch), and no-match / ambiguous-tie declines.
func TestCoerceBoolValue(t *testing.T) {
	cases := []struct {
		name    string
		in      value
		want    bool
		flagged bool
		ok      bool
	}{
		{"json-true", value{kind: valBool, boolV: true}, true, false, true},
		{"json-false", value{kind: valBool, boolV: false}, false, false, true},
		{"casefold-true", strVv("TRUE"), true, true, true},
		{"casefold-mixed", strVv("TrUe"), true, true, true},
		{"casefold-false", strVv("False"), false, true, true},
		{"substring-true", strVv("it is true."), true, true, true},
		{"substring-false", strVv("definitely false"), false, true, true},
		{"no-match", strVv("maybe"), false, false, false},
		{"ambiguous-tie", strVv("true or false"), false, false, false},
		{"number-input", numV("1"), false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, flagged, uncertain, err := coerceBoolValue(c.in)
			if uncertain {
				t.Fatalf("unexpected uncertain for ASCII input")
			}
			if c.ok {
				if err != nil {
					t.Fatalf("coerceBoolValue(%+v) unexpected error: %v", c.in, err)
				}
				if got != c.want {
					t.Errorf("value = %v, want %v", got, c.want)
				}
				if flagged != c.flagged {
					t.Errorf("flagged = %v, want %v", flagged, c.flagged)
				}
			} else {
				if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Fatalf("want ErrDeBAMLParseUnsupported, got err=%v", err)
				}
			}
		})
	}
}

// TestCoercePrimitiveNull pins coerce_null: JSON null is clean; any non-null
// value defaults to null and flags DefaultButHadValue (score 110).
func TestCoercePrimitiveNull(t *testing.T) {
	cf := &coerceFlags{}
	out, err := coercePrimitiveNull(value{kind: valNull}, cf)
	if err != nil || string(out) != "null" {
		t.Fatalf("null input: out=%q err=%v, want null/nil", string(out), err)
	}
	if cf.isFlagged() {
		t.Errorf("null input should be clean, cf flagged")
	}
	cf2 := &coerceFlags{}
	out, err = coercePrimitiveNull(numV("5"), cf2)
	if err != nil || string(out) != "null" {
		t.Fatalf("non-null input: out=%q err=%v, want null/nil", string(out), err)
	}
	if !cf2.isFlagged() {
		t.Errorf("non-null input should flag DefaultButHadValue")
	}
}
