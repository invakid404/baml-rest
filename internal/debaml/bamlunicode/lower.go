package bamlunicode

import (
	"strings"
	"unicode/utf8"
)

// Greek sigma scalars involved in the language-independent Final_Sigma rule.
const (
	capitalSigma = 'Σ' // Σ GREEK CAPITAL LETTER SIGMA
	smallSigma   = 'σ' // σ GREEK SMALL LETTER SIGMA
	finalSigma   = 'ς' // ς GREEK SMALL LETTER FINAL SIGMA
)

// LowerString returns the full Unicode lowercase of s, byte-for-byte identical
// to Rust 1.93.0 std str::to_lowercase (Unicode 17.0.0).
//
// This mirrors the Rust implementation exactly:
//
//   - Every scalar maps through the unconditional to_lower table (SpecialCasing
//     unconditional full mappings where present, else UnicodeData simple
//     lowercase, else identity), which includes one-to-many expansions such as
//     U+0130 -> U+0069 U+0307.
//   - U+03A3 (Σ) is the single contextual exception: it lowercases to ς at the
//     end of a word (Final_Sigma) and to σ otherwise. Rust hard-codes this one
//     language-independent condition rather than a generic condition engine, and
//     so do we. The context is examined against the ORIGINAL string, exactly as
//     Rust's map_uppercase_sigma does.
//
// Locale-specific Turkic/Lithuanian tailorings are intentionally NOT applied:
// Rust's str::to_lowercase is language-independent, and so is BAML's matcher.
func LowerString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i, c := range s {
		switch {
		case c == capitalSigma:
			if isFinalSigmaContext(s, i) {
				b.WriteRune(finalSigma)
			} else {
				b.WriteRune(smallSigma)
			}
		default:
			if seq, ok := lookupLowerFull(c); ok {
				for _, r := range seq {
					b.WriteRune(r)
				}
			} else {
				b.WriteRune(lookupLowerSingle(c))
			}
		}
	}
	return b.String()
}

// isFinalSigmaContext reports whether the Σ at byte offset i in s is word-final
// under the Unicode Final_Sigma condition, matching Rust exactly:
//
//	is_word_final = case_ignorable_then_cased(before.chars().rev())
//	                && !case_ignorable_then_cased(after.chars())
//
// where case_ignorable_then_cased skips Case_Ignorable scalars and then reports
// whether the next scalar is Cased (false if the side is empty). Σ is 2 bytes in
// UTF-8, so the following text begins at i+2.
func isFinalSigmaContext(s string, i int) bool {
	before := caseIgnorableThenCasedReverse(s[:i])
	after := caseIgnorableThenCasedForward(s[i+utf8.RuneLen(capitalSigma):])
	return before && !after
}

// caseIgnorableThenCasedForward skips leading Case_Ignorable scalars of t and
// reports whether the first non-Case_Ignorable scalar is Cased (false if none).
func caseIgnorableThenCasedForward(t string) bool {
	for _, r := range t {
		if isCaseIgnorable(r) {
			continue
		}
		return isCased(r)
	}
	return false
}

// caseIgnorableThenCasedReverse is caseIgnorableThenCasedForward over t reversed,
// i.e. it skips trailing Case_Ignorable scalars and reports whether the nearest
// preceding non-Case_Ignorable scalar is Cased.
func caseIgnorableThenCasedReverse(t string) bool {
	for len(t) > 0 {
		r, size := utf8.DecodeLastRuneInString(t)
		t = t[:len(t)-size]
		if isCaseIgnorable(r) {
			continue
		}
		return isCased(r)
	}
	return false
}
