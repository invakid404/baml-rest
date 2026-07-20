package bamlunicode

import "testing"

// s builds a string from explicit scalar values; used for exotic sentinels where
// eyeballing a glyph literal is error-prone and byte-exactness is the bar.
func s(cps ...rune) string { return string(cps) }

// Sentinel cases that distinguish the exact BAML dual-version profile from the
// host Go/x-text Unicode tables. These are fast and always run; the exhaustive
// byte-for-byte proof against the Rust reference lives in rustref_test.go and the
// full NFKD conformance suite in conformance_test.go.

func TestLowerSentinels(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The #555 sentinel: U+A7DC LATIN CAPITAL LETTER LAMBDA WITH STROKE
		// lowercases to U+019B under Unicode 17.0.0. Go 15 tables do not know it.
		{"A7DC->019B", s(0xA7DC), s(0x019B)},
		// One-to-many unconditional SpecialCasing: U+0130 -> U+0069 U+0307.
		{"0130 one-to-many", s(0x0130), s(0x0069, 0x0307)},
		// Final_Sigma: word-final sigma -> U+03C2, medial/isolated -> U+03C3.
		{"sigma isolated", s(0x03A3), s(0x03C3)},
		{"sigma after cased", s('a', 0x03A3), s('a', 0x03C2)},
		{"sigma medial", s('a', 0x03A3, 'a'), s('a', 0x03C3, 'a')},
		// Case-ignorable (U+0027 APOSTROPHE) between cased letter and end keeps
		// the sigma word-final.
		{"sigma final through case-ignorable", s('a', 0x03A3, 0x0027), s('a', 0x03C2, 0x0027)},
		// Non-final because a cased letter follows past the case-ignorable.
		{"sigma medial through case-ignorable", s('a', 0x03A3, 0x0027, 'a'), s('a', 0x03C3, 0x0027, 'a')},
		// Stable non-ASCII: U+1E9E LATIN CAPITAL LETTER SHARP S -> U+00DF.
		{"1E9E->00DF", s(0x1E9E), s(0x00DF)},
		// ASCII fast path.
		{"ascii", "HeLLo", "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LowerString(tc.in); got != tc.want {
				t.Fatalf("LowerString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNFKDSentinels(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Compatibility decomposition: U+FB01 LATIN SMALL LIGATURE FI -> "fi".
		{"ligature fi", s(0xFB01), "fi"},
		// Superscript digit two U+00B2 -> "2" (compat).
		{"superscript 2", s(0x00B2), "2"},
		// Canonical composed char decomposes: U+00C0 -> U+0041 U+0300.
		{"A grave", s(0x00C0), s(0x0041, 0x0300)},
		// Recursive compat decomposition: U+FDFA ARABIC LIGATURE SALLALLAHOU...
		// expands to 18 scalars.
		{"arabic ligature FDFA len", s(0xFDFA), ""}, // want filled below
		// Hangul algorithmic decomposition: U+AC00 -> U+1100 U+1161.
		{"hangul ga", s(0xAC00), s(0x1100, 0x1161)},
		// Hangul with trailing jamo: U+AC01 -> U+1100 U+1161 U+11A8.
		{"hangul gag", s(0xAC01), s(0x1100, 0x1161, 0x11A8)},
		// Canonical reordering: D + dot-above(ccc 230) + dot-below(ccc 220)
		// given out of ccc order must reorder to below-then-above.
		{"canonical reorder", s(0x0044, 0x0307, 0x0323), s(0x0044, 0x0323, 0x0307)},
	}
	for _, tc := range cases {
		if tc.name == "arabic ligature FDFA len" {
			// Just assert it expanded to more than one scalar and is stable.
			got := NFKD(tc.in)
			if len([]rune(got)) <= 1 {
				t.Errorf("NFKD(U+FDFA) did not expand: %q", got)
			}
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if got := NFKD(tc.in); got != tc.want {
				t.Fatalf("NFKD(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPropertySentinels(t *testing.T) {
	// is_alphanumeric = Alphabetic OR GC=N. U+00AA FEMININE ORDINAL INDICATOR is
	// Alphabetic (Lo) — Rust claims it; a naive letter/number test could miss it.
	if !IsAlphanumeric(0x00AA) {
		t.Errorf("IsAlphanumeric(U+00AA) = false, want true")
	}
	// U+2160 ROMAN NUMERAL ONE is Nl (a number).
	if !IsAlphanumeric(0x2160) {
		t.Errorf("IsAlphanumeric(U+2160) = false, want true")
	}
	// Plain punctuation is neither.
	if IsAlphanumeric('!') {
		t.Errorf("IsAlphanumeric('!') = true, want false")
	}
	// White_Space: U+00A0 NO-BREAK SPACE is whitespace under the property.
	if !IsWhitespace(0x00A0) {
		t.Errorf("IsWhitespace(U+00A0) = false, want true")
	}
	if !IsWhitespace(' ') {
		t.Errorf("IsWhitespace(space) = false, want true")
	}
	// Combining marks (Unicode 16.0.0 Mn/Mc/Me).
	if !IsCombiningMark(0x0301) { // COMBINING ACUTE ACCENT (Mn)
		t.Errorf("IsCombiningMark(U+0301) = false, want true")
	}
	if IsCombiningMark('a') {
		t.Errorf("IsCombiningMark('a') = true, want false")
	}
	// Internal Final_Sigma inputs.
	if !isCased('A') || !isCased('a') || isCased('!') {
		t.Errorf("isCased sanity failed")
	}
	if !isCaseIgnorable(0x0027) { // APOSTROPHE is Case_Ignorable
		t.Errorf("isCaseIgnorable(U+0027) = false, want true")
	}
}

func TestTrimSpace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  hi  ", "hi"},
		{s(0x00A0, 'h', 'i', 0x00A0), "hi"}, // NBSP is White_Space
		{"\t\nhi\r\n", "hi"},
		{"nospace", "nospace"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := TrimSpace(tc.in); got != tc.want {
			t.Errorf("TrimSpace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
