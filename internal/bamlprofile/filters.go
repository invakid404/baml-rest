package bamlprofile

import (
	"fmt"
	"regexp"
	"strings"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/filters"
	"github.com/invakid404/minijinja-go/v2/value"
)

// regexMatchFilter is BAML get_env's `regex_match` filter, added by
// jinja_helpers.rs:32,38-43:
//
//	fn regex_match(value: String, regex: String) -> bool {
//	    match Regex::new(&regex) {
//	        Err(_) => false,
//	        Ok(re) => re.is_match(&value),
//	    }
//	}
//
// As a minijinja filter, `value` is the piped subject and `regex` is the single
// positional argument. A regex that fails to compile yields false (not an
// error), matching BAML. It is registered as a filter (`x|regex_match("...")`).
//
// # Contract (see the #651 declared-divergence follow-up)
//
// BAML uses Rust's `regex` crate (string API, Unicode-aware, `u`-flag ON by
// default); this uses Go's `regexp`. They are NOT the same regex language, so
// regex_match uses a STRICT, DECLINE-BY-DEFAULT whitelist that makes two
// guarantees hold and CONVERGE:
//
//	G1  never Go-invalid: rustRegexToGo only ever returns a *compiled* regexp —
//	    the walker declines any grammar it does not fully recognize, and the
//	    emitted pattern is then compiled and FAILS CLOSED to a decline if Go
//	    rejects it. A Go-invalid pattern can never reach a match.
//	G2  never more permissive than BAML: the walker sets accept only when the
//	    ENTIRE pattern parses into an explicitly-known Go==Rust-safe grammar (see
//	    scanPattern). Any unrecognized token, malformed/ambiguous/partial parse,
//	    or edge -> decline -> false, which can only under-match BAML, never exceed
//	    it. A new/unknown construct is declined, so this converges.
//
// The compile backstop (G1) does NOT establish G2: a Go-VALID pattern that
// out-does Rust must be excluded by the grammar itself, which is why the walker
// declines named groups, `\<`/`\>`, raw non-repeat braces, class ranges that
// touch a shorthand/non-ASCII endpoint, etc.
//
// Reproduced BAML byte-exact (the accept grammar, exhaustively differential-
// verified — see TestRegexNeverOutdo and #651): ASCII literals and escaped
// regex-metacharacter literals; the control escapes and `\x{<0x80}`; Unicode
// `\d` -> `\p{Nd}` (guarded by TestRegexDigitUnicodeGuard; `\D` is DECLINED
// because Go's and BAML's Unicode tables differ); `^` `$` `.` `|`, balanced
// `(...)`/`(?:...)` groups, quantifiers `*` `+` `?` and fully-validated
// `{m}`/`{m,}`/`{m,n}` (0<=m<=n<=1000, NO leading-zero bounds); ASCII-only classes
// with ASCII-endpoint ranges and `\d` members; and `i`/`m`/`s`/`U` flag groups in
// Unicode-ON mode with NO DUPLICATE flags. Everything else is DECLINED. Full
// faithful parity (incl. a frozen BAML-compatible Unicode table) is the #651
// follow-up, to close BEFORE the profile is wired into serving.
func regexMatchFilter(_ filters.State, val value.Value, args []value.Value, kwargs *value.OrderedMap) (value.Value, error) {
	a := filters.NewArgs(args, kwargs)
	pattern, err := a.Str()
	if err != nil {
		return value.Undefined(), err
	}
	if err := a.Done(); err != nil {
		return value.Undefined(), err
	}

	// The `value: String` parameter: minijinja converts the piped subject to a
	// String and errors if it is not one, before the body runs.
	s, ok := val.AsString()
	if !ok {
		return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
			fmt.Sprintf("cannot convert %s to string", val.Kind()))
	}

	re, ok := rustRegexToGo(pattern)
	if !ok {
		// Not provably Go==Rust-safe (or would not compile): conservatively false,
		// so we can never out-do BAML (G2), and never match a Go-invalid pattern (G1).
		return value.FromBool(false), nil
	}
	return value.FromBool(re.MatchString(s)), nil
}

// rustRegexToGo returns a compiled Go regexp equivalent to the Rust pattern, and
// ok=true, ONLY when the whole pattern is on the strict Go==Rust-safe accept
// grammar (scanPattern) AND the emitted pattern compiles. Otherwise ok=false —
// which the caller turns into false. ok=true therefore GUARANTEES a valid,
// non-out-doing match (G1 by compile backstop, G2 by grammar). See
// regexMatchFilter for the contract.
func rustRegexToGo(pattern string) (*regexp.Regexp, bool) {
	goPattern, ok := scanPattern(pattern)
	if !ok {
		return nil, false
	}
	re, err := regexp.Compile(goPattern)
	if err != nil {
		return nil, false // G1 fail-closed backstop
	}
	return re, true
}

// scanPattern is the strict, decline-by-default walker. It returns the translated
// Go pattern and ok=true ONLY if every token is explicitly recognized as
// Go==Rust-safe and groups are balanced; ANY unrecognized/malformed token, or an
// unbalanced group, returns ok=false.
func scanPattern(pattern string) (string, bool) {
	rs := []rune(pattern)
	var b strings.Builder
	depth := 0
	for i := 0; i < len(rs); {
		switch c := rs[i]; c {
		case '\\':
			emit, adv, ok := scanEscape(rs, i)
			if !ok {
				return "", false
			}
			b.WriteString(emit)
			i += adv
		case '[':
			emit, adv, ok := scanClass(rs, i)
			if !ok {
				return "", false
			}
			b.WriteString(emit)
			i += adv
		case '(':
			emit, adv, opens, ok := scanGroupOpen(rs, i)
			if !ok {
				return "", false
			}
			if opens {
				depth++
			}
			b.WriteString(emit)
			i += adv
		case ')':
			depth--
			if depth < 0 {
				return "", false // unbalanced close
			}
			b.WriteRune(')')
			i++
		case '{':
			emit, adv, ok := scanRepeat(rs, i)
			if !ok {
				return "", false
			}
			b.WriteString(emit)
			i += adv
		case '^', '$', '.', '|', '*', '+', '?':
			b.WriteRune(c)
			i++
		default:
			// A bare literal: an ASCII char that is not a metacharacter, or a
			// non-ASCII rune (in Unicode-ON both engines match the rune itself).
			// `]` and `}` reach here as literals (both engines accept them bare).
			b.WriteRune(c)
			i++
		}
	}
	if depth != 0 {
		return "", false // unbalanced open
	}
	return b.String(), true
}

// scanEscape handles a top-level `\<next>`: `\d` (translated to \p{Nd}), the
// control escapes, `\x{<0x80}`, and escaped regex-metacharacter literals. It
// DECLINES every other escape — `\D` (Unicode-table skew), `\s`/`\w`/`\b`, `\p`,
// octal, `\Q`, `\<`/`\>`, letter escapes, and malformed/high hex — since Rust
// and Go disagree on them.
func scanEscape(rs []rune, i int) (emit string, adv int, ok bool) {
	if i+1 >= len(rs) {
		return "", 0, false // trailing backslash
	}
	next := rs[i+1]
	switch {
	case next == 'd':
		// \d -> \p{Nd}. Safe today because Go's Unicode table (Nd) is a subset of
		// BAML's newer one, so Go matches => BAML matches (only under-matches on a
		// digit added in a Unicode version Go lacks). TestRegexDigitUnicodeGuard
		// pins Go-Nd ⊆ BAML-\d; if a future toolchain breaks that, decline \d too.
		return `\p{Nd}`, 2, true
	case next == 'n' || next == 't' || next == 'r' || next == 'f' || next == 'v':
		return `\` + string(next), 2, true
	case next == 'x':
		v, end, okHex := parseHexEscape(rs, i+2)
		if !okHex || v >= 0x80 {
			return "", 0, false
		}
		return string(rs[i:end]), end - i, true
	case isEscapableMeta(next):
		return `\` + string(next), 2, true
	default:
		return "", 0, false
	}
}

// scanClass parses `[...]`. It accepts ONLY: ASCII single-char members, ASCII
// endpoint..endpoint ranges (start<=end), a `\d` shorthand member, control /
// `\x{<0x80}` / escaped-metacharacter members, and a leading `^` negation. It
// DECLINES an empty class, a nested/POSIX `[`, a set operation, a non-ASCII
// member, a `\D`/`\p`/`\s`/`\w`/etc member, a reversed range, and any range whose
// endpoint is a shorthand or non-ASCII (`[\d-a]`, `[a-\d]`, `[a-\x00]`, ...).
func scanClass(rs []rune, start int) (emit string, adv int, ok bool) {
	var b strings.Builder
	b.WriteRune('[')
	j := start + 1
	if j < len(rs) && rs[j] == '^' {
		b.WriteRune('^')
		j++
	}
	firstContent := j
	count := 0
	pendingCP := -1 // an ASCII single-char member buffered for a possible range
	pendingEmit := ""
	prevShort := false
	flush := func() {
		if pendingCP >= 0 {
			b.WriteString(pendingEmit)
			pendingCP = -1
		}
	}
	for j < len(rs) {
		c := rs[j]
		// Set operations && / -- / ~~ are Rust-only; decline.
		if (c == '&' || c == '~') && j+1 < len(rs) && rs[j+1] == c {
			return "", 0, false
		}
		switch {
		case c == ']':
			flush()
			if count == 0 {
				return "", 0, false // empty class [] or [^]
			}
			b.WriteRune(']')
			return b.String(), j + 1 - start, true
		case c == '[':
			return "", 0, false // POSIX [[:...:]] or nested class
		case c == '-':
			// A range separator between two ASCII single-char members, or a literal
			// hyphen at the class boundary; anything else declines.
			if prevShort {
				return "", 0, false // [\d-...]
			}
			if pendingCP >= 0 {
				if j+1 < len(rs) && rs[j+1] == ']' {
					flush()
					b.WriteString(`\-`)
					count++
					j++
					continue
				}
				cp2, emit2, nj, ok2 := classAtom(rs, j+1)
				if !ok2 || cp2 < 0 { // next is not a single ASCII char (e.g. a shorthand)
					return "", 0, false
				}
				if pendingCP > cp2 {
					return "", 0, false // reversed range [z-a]
				}
				b.WriteString(pendingEmit + "-" + emit2)
				pendingCP = -1
				count++
				j = nj
				continue
			}
			// pendingCP < 0: literal hyphen only at the boundary.
			if j == firstContent || (j+1 < len(rs) && rs[j+1] == ']') {
				b.WriteString(`\-`)
				count++
				j++
				continue
			}
			return "", 0, false
		default:
			cp, atomEmit, nj, ok2 := classAtom(rs, j)
			if !ok2 {
				return "", 0, false
			}
			flush()
			if cp < 0 { // shorthand (\d/\D): a whole-class member, never a range end
				b.WriteString(atomEmit)
				prevShort = true
			} else {
				pendingCP = cp
				pendingEmit = atomEmit
				prevShort = false
			}
			count++
			j = nj
		}
	}
	return "", 0, false // unterminated class
}

// classAtom reads one class member at rs[j]. It returns cp>=0 with the emitted
// text for a single ASCII code point (usable as a range endpoint), cp<0 for a
// `\d`/`\D` shorthand, or ok=false to decline. It handles escapes but NOT `]`,
// `-`, `[` (structural, handled by scanClass).
func classAtom(rs []rune, j int) (cp int, emit string, next int, ok bool) {
	c := rs[j]
	if c == '\\' {
		if j+1 >= len(rs) {
			return 0, "", 0, false
		}
		e := rs[j+1]
		switch {
		case e == 'd':
			return -1, `\p{Nd}`, j + 2, true // \D declined (Unicode-table skew; see scanEscape)
		case e == 'n':
			return '\n', `\n`, j + 2, true
		case e == 't':
			return '\t', `\t`, j + 2, true
		case e == 'r':
			return '\r', `\r`, j + 2, true
		case e == 'f':
			return '\f', `\f`, j + 2, true
		case e == 'v':
			return '\v', `\v`, j + 2, true
		case e == 'x':
			v, end, okHex := parseHexEscape(rs, j+2)
			if !okHex || v >= 0x80 {
				return 0, "", 0, false
			}
			return v, string(rs[j:end]), end, true
		case isEscapableMeta(e):
			return int(e), `\` + string(e), j + 2, true
		default:
			return 0, "", 0, false
		}
	}
	if c >= 0x80 {
		return 0, "", 0, false // non-ASCII member/range endpoint
	}
	return int(c), string(c), j + 1, true
}

// scanGroupOpen parses a `(`: a capturing group `(`, `(?:...)`, or a flag group
// `(?flags)`/`(?flags:...)` whose flags are exactly a Go set (>=1 of i/m/s/U,
// optional `-`, nothing else). It DECLINES named groups, backreferences,
// lookaround, comments, and any u/x/R flag. opens reports whether a lasting
// group was opened (for balance tracking).
func scanGroupOpen(rs []rune, i int) (emit string, adv int, opens, ok bool) {
	if i+1 >= len(rs) || rs[i+1] != '?' {
		return "(", 1, true, true // capturing group
	}
	if i+2 >= len(rs) {
		return "", 0, false, false // "(?" at end
	}
	c2 := rs[i+2]
	if c2 == ':' {
		return "(?:", 3, true, true
	}
	// Named groups, backrefs, lookaround, comments — declined.
	if c2 == 'P' || c2 == '<' || c2 == '=' || c2 == '!' || c2 == '#' {
		return "", 0, false, false
	}
	// Flag group: scan the flag letters after `(?`. Go accepts duplicate flags and
	// a repeat across the set/clear sides; Rust rejects both, so we require a set
	// (each of i/m/s/U at most once, across BOTH sides of a single `-`).
	j := i + 2
	hasLetter := false
	seen := ""
	dashes := 0
	for j < len(rs) && isGoFlag(rs[j]) {
		c := rs[j]
		if c == '-' {
			dashes++
			if dashes > 1 {
				return "", 0, false, false // more than one `-`
			}
		} else {
			if strings.ContainsRune(seen, c) {
				return "", 0, false, false // duplicate flag ((?ii), (?i-i))
			}
			seen += string(c)
			hasLetter = true
		}
		j++
	}
	if !hasLetter || j >= len(rs) || (rs[j] != ')' && rs[j] != ':') {
		return "", 0, false, false // contains u/x/R, empty, or malformed
	}
	opens = rs[j] == ':' // (?flags:...) opens a group; (?flags) is inline
	return string(rs[i : j+1]), j + 1 - i, opens, true
}

// scanRepeat parses `{...}` at rs[start] as a FULLY-validated repetition
// `{m}`/`{m,}`/`{m,n}` with 0<=m<=n<=1000; anything else — a bare `{`, `{}`,
// `{,1}`, `{1,x}`, `{1` (unclosed), `{1,0}`, `{1000,999}`, `{1001}` — is DECLINED
// (Rust rejects these spellings even where Go treats `{` as a literal).
func scanRepeat(rs []rune, start int) (emit string, adv int, ok bool) {
	j := start + 1
	lowStart := j
	n1, d1 := scanNumber(rs, j)
	if d1 == 0 {
		return "", 0, false // no lower bound: {, {}, {,1
	}
	if d1 > 1 && rs[lowStart] == '0' {
		return "", 0, false // leading-zero bound: Go reads {01} as a literal, Rust as a repeat
	}
	j += d1
	n2 := n1
	if j < len(rs) && rs[j] == ',' {
		j++
		upStart := j
		n2v, d2 := scanNumber(rs, j)
		j += d2
		if d2 > 0 {
			if d2 > 1 && rs[upStart] == '0' {
				return "", 0, false // leading-zero upper bound
			}
			n2 = n2v
		} else {
			n2 = -1 // {m,} unbounded upper
		}
	}
	if j >= len(rs) || rs[j] != '}' {
		return "", 0, false // unclosed or trailing junk: {1, {1,x
	}
	if n1 > 1000 {
		return "", 0, false
	}
	if n2 >= 0 && (n2 > 1000 || n1 > n2) {
		return "", 0, false // {1,0}, {1000,999}, {1,1001}
	}
	return string(rs[start : j+1]), j + 1 - start, true
}

// scanNumber reads a run of ASCII digits, capped so a large count cannot overflow
// (it stays above 1000, which is all the caller checks).
func scanNumber(rs []rune, start int) (val, n int) {
	for start+n < len(rs) && rs[start+n] >= '0' && rs[start+n] <= '9' {
		if val <= 1_000_000 {
			val = val*10 + int(rs[start+n]-'0')
		}
		n++
	}
	return val, n
}

// parseHexEscape reads a `\x` body starting at rs[start]: `{H..}` (>=1 hex digit,
// <= U+10FFFF) or EXACTLY two bare hex digits. It returns the value and the index
// just past the escape; a one-digit `\xA` or non-hex `\x1G` is not parseable.
func parseHexEscape(rs []rune, start int) (val, end int, ok bool) {
	if start >= len(rs) {
		return 0, 0, false
	}
	if rs[start] == '{' {
		j, v, any := start+1, 0, false
		for j < len(rs) && rs[j] != '}' {
			d, isHex := hexDigit(rs[j])
			if !isHex {
				return 0, 0, false
			}
			v, any = v*16+d, true
			if v > 0x10FFFF {
				return 0, 0, false
			}
			j++
		}
		if !any || j >= len(rs) {
			return 0, 0, false
		}
		return v, j + 1, true
	}
	// bare form: exactly two hex digits (Go's \xHH).
	if start+1 >= len(rs) {
		return 0, 0, false
	}
	d0, ok0 := hexDigit(rs[start])
	d1, ok1 := hexDigit(rs[start+1])
	if !ok0 || !ok1 {
		return 0, 0, false
	}
	return d0*16 + d1, start + 2, true
}

func hexDigit(r rune) (int, bool) {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0'), true
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10, true
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10, true
	}
	return 0, false
}

// isGoFlag reports whether r is a Go inline flag or the `-` clearer. u/x/R are
// excluded, so any group using them is declined.
func isGoFlag(r rune) bool {
	switch r {
	case 'i', 'm', 's', 'U', '-':
		return true
	}
	return false
}

// escapableMeta is the regex-metacharacter set that `\` + char denotes as the
// literal char identically in Go and Rust. It deliberately excludes `<` and `>`
// (Go treats `\<`/`\>` as literals; Rust rejects them) and all non-metacharacter
// punctuation (Rust rejects many `\<punct>` escapes Go accepts).
const escapableMeta = `\.+*?()[]{}|^$/-`

func isEscapableMeta(r rune) bool {
	return strings.ContainsRune(escapableMeta, r)
}

// sumFilter is BAML get_env's `sum` filter, which REPLACES the engine builtin
// sum (jinja_helpers.rs:33,45-65). The fork's default sum is the engine builtin
// (no start arg, refuses non-numbers); this is BAML's asymmetric int/float rule:
//
//	fn sum_filter(value: Vec<Value>) -> Value {
//	    let int_sum   = value.iter().map(|v| i64::try_from(v).ok()).collect::<Option<Vec<_>>>().map(sum);
//	    let float_sum = value.iter().map(|v| f64::try_from(v).ok()).collect::<Option<Vec<_>>>().map(sum);
//	    // all convertible to int -> int; else all convertible to float -> float; else 0.
//	    int_sum.map_or(float_sum.map_or(Value::from(0), Value::from), Value::from)
//	}
//
// minijinja materializes the piped subject into `Vec<Value>` (try_iter) before
// the body runs, so a non-iterable subject is a conversion error and the filter
// takes no further arguments.
//
// The i64/f64 conversions are the fork's Value.AsInt / Value.AsFloat, which are
// the engine's `i64::try_from` / `f64::try_from` (Value.AsFloat documents this
// and, matching the engine, rejects a bool where AsInt accepts it). collect into
// Option<Vec<_>> is all-or-nothing: one non-convertible element drops the whole
// path, so the loops short-circuit on the first failure. The int accumulation
// wraps on overflow, matching a Rust release-build i64 sum.
func sumFilter(_ filters.State, val value.Value, args []value.Value, kwargs *value.OrderedMap) (value.Value, error) {
	if len(args) > 0 || kwargs.Len() > 0 {
		return value.Undefined(), minijinja.NewError(minijinja.ErrTooManyArguments, "too many arguments")
	}

	items := val.Iter()
	if items == nil {
		return value.Undefined(), minijinja.NewError(minijinja.ErrInvalidOperation,
			fmt.Sprintf("%s is not iterable", val.Kind()))
	}

	allInt := true
	var intSum int64
	for _, it := range items {
		n, ok := it.AsInt()
		if !ok {
			allInt = false
			break
		}
		intSum += n
	}
	if allInt {
		return value.FromInt(intSum), nil
	}

	allFloat := true
	var floatSum float64
	for _, it := range items {
		f, ok := it.AsFloat()
		if !ok {
			allFloat = false
			break
		}
		floatSum += f
	}
	if allFloat {
		return value.FromFloat(floatSum), nil
	}

	return value.FromInt(0), nil
}
