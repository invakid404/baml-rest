package bamlunicode

import "strings"

// IsAlphanumeric reports whether r is alphanumeric under Rust 1.93.0 std
// char::is_alphanumeric (Unicode 17.0.0): Alphabetic (the derived core property,
// which already folds in Other_Alphabetic) OR General_Category = N (Nd, Nl, No).
//
// This is deliberately NOT Go's unicode.IsLetter || unicode.IsNumber: Rust's
// predicate keys off the Alphabetic property (e.g. it includes U+00AA FEMININE
// ORDINAL INDICATOR and letter-like symbols Go's IsLetter would miss), which is
// exactly the classification BAML's punctuation-stripping uses.
func IsAlphanumeric(r rune) bool {
	return inRanges(alphabetic17, r) || inRanges(numberGC17, r)
}

// IsWhitespace reports whether r has the Unicode 17.0.0 White_Space property,
// matching Rust char::is_whitespace (which str::trim consults).
func IsWhitespace(r rune) bool {
	return inRanges(whiteSpace17, r)
}

// TrimSpace trims leading and trailing White_Space scalars from s, byte-for-byte
// identical to Rust str::trim.
func TrimSpace(s string) string {
	return strings.TrimFunc(s, IsWhitespace)
}

// IsCombiningMark reports whether r is a combining mark under
// unicode_normalization::char::is_combining_mark (Unicode 16.0.0): General
// Category Mn, Mc, or Me. BAML strips these after NFKD; it uses the
// normalization crate's 16.0.0 data, NOT Go's unicode.Mn/Mc/Me categories.
func IsCombiningMark(r rune) bool {
	return inRanges(combiningMark16, r)
}

// isCased reports the internal Rust std Cased property (Unicode 17.0.0), consumed
// only by the Final_Sigma context in LowerString.
func isCased(r rune) bool {
	return inRanges(cased17, r)
}

// isCaseIgnorable reports the internal Rust std Case_Ignorable property (Unicode
// 17.0.0), consumed only by the Final_Sigma context in LowerString.
func isCaseIgnorable(r rune) bool {
	return inRanges(caseIgnorable17, r)
}
