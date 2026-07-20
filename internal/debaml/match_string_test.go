package debaml

import (
	"strings"
	"testing"
)

// TestRemoveAccents mirrors match_string.rs's remove_accents unit tests
// verbatim, locking the native fold to BAML's NFKD + ligature behavior.
func TestRemoveAccents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"étude", "etude"},
		{"français", "francais"},
		{"Español", "Espanol"},
		{"português", "portugues"},
		{"médium", "medium"},
		{"Grün", "Grun"},
		{"Über", "Uber"},
		{"Straße", "Strasse"},
		{"Stadt", "Stadt"},
		{"æ", "ae"},
		{"Æ", "AE"},
		{"ø", "o"},
		{"Ø", "O"},
		{"œ", "oe"},
		{"Œ", "OE"},
		{"København", "Kobenhavn"},
		{"cœur", "coeur"},
		{"œuvre", "oeuvre"},
		{"Straße ældre øl œuvre", "Strasse aeldre ol oeuvre"},
		// Leaves non-alphanumeric ASCII and other scripts alone; folds ligatures.
		{"ß, æ, ø, œ, こんにちは, 🦄", "ss, ae, o, oe, こんにちは, 🦄"},
	}
	for _, c := range cases {
		if got := removeAccents(c.in); got != c.want {
			t.Errorf("removeAccents(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMatchStringUnicodeClaims pins the #555 Slice 2 win: match_string now folds
// through bamlunicode (byte-for-byte Rust str::to_lowercase / NFKD / properties),
// so exotic non-ASCII case folds the old x/text-based fold could not prove are now
// CLAIMED matches — a match verdict is native's to return, never "uncertain".
// The U+A7DC → U+019B sentinel (which Go's own tables don't even classify as
// uppercase) is the headline case. Negative near-matches confirm no over-claim.
func TestMatchStringUnicodeClaims(t *testing.T) {
	cases := []struct {
		name  string
		input string
		cand  string // single candidate (name == valid value)
		want  matchOutcome
	}{
		// Uppercase accented Latin folds to the lowercase candidate (case + accent).
		{"grun-umlaut", "GRÜN", "grün", matchOne},
		{"e-acute", "É", "é", matchOne}, // É -> é
		// U+A7DC LATIN CAPITAL LETTER LAMBDA WITH STROKE lowercases to U+019B under
		// Rust/Unicode-17; the old Go-15 x/text tables left it unchanged (the #555
		// sentinel). Now a CLAIMED case-fold match.
		{"a7dc-sentinel", "Ƛ", "ƛ", matchOne},
		// One-to-many lowercase İ (U+0130 → "i"+U+0307): folds to "id" via the
		// accent-strip inside the case-fold pass.
		{"dotted-capital-i", "İD", "id", matchOne},
		// Stable non-ASCII control U+1E9E (ẞ) lowercases to ß, which the ligature
		// fold rewrites to "ss": "GRoẞ" folds to "gross".
		{"capital-sharp-s", "GROẞ", "gross", matchOne},
		// Negative near-matches: no over-claim.
		{"neg-different-word", "GRÜN", "blau", matchNone},
		{"neg-lambda-vs-x", "Ƛ", "x", matchNone},
	}

	for _, c := range cases {
		cands := []matchCandidate{{name: c.cand, validValues: []string{c.cand}}}
		_, outcome, _ := matchString(c.input, cands, true)
		if outcome != c.want {
			t.Errorf("%s: matchString(%q vs %q) outcome = %v, want %v", c.name, c.input, c.cand, outcome, c.want)
		}
	}
}

// TestMatchStringOutcomes exercises each match_string strategy and the
// substring tie path, with both substring-enabled (enum/literal/map-key) and
// substring-disabled (class field key) candidate sets.
func TestMatchStringOutcomes(t *testing.T) {
	enumLike := []matchCandidate{
		{name: "RED", validValues: []string{"RED"}},
		{name: "GREEN", validValues: []string{"GREEN"}},
		{name: "BLUE", validValues: []string{"BLUE"}},
	}
	cases := []struct {
		name           string
		input          string
		candidates     []matchCandidate
		allowSubstring bool
		wantName       string
		wantOutcome    matchOutcome
	}{
		{"exact", "GREEN", enumLike, true, "GREEN", matchOne},
		{"trim", "  GREEN  ", enumLike, true, "GREEN", matchOne},
		{"case-insensitive", "green", enumLike, true, "GREEN", matchOne},
		// '.' and '(' ')' are stripped (only '-'/'_' are kept), so "(GREEN)."
		// folds to "GREEN" via the strip-exact path (substring DISABLED here, so
		// it's a clean score-0 match, not a SubstringMatch).
		{"punctuation", "(GREEN).", enumLike, false, "GREEN", matchOne},
		// With substring ENABLED the same input matches "GREEN" as a substring
		// (SubstringMatch, cost 2) before the strip pass runs.
		{"substring-punct", "(GREEN).", enumLike, true, "GREEN", matchOne},
		{"no-match", "MAUVE", enumLike, true, "", matchNone},
		{
			"accent-fold",
			"Grün",
			[]matchCandidate{{name: "GRUN", validValues: []string{"Grun"}}},
			true,
			"GRUN",
			matchOne,
		},
		{
			"substring-single",
			"the color is green today",
			enumLike,
			true,
			"GREEN",
			matchOne,
		},
		{
			// "catdog" contains both CAT and DOG non-overlapping -> tie.
			"substring-tie",
			"cat dog",
			[]matchCandidate{
				{name: "CAT", validValues: []string{"CAT"}},
				{name: "DOG", validValues: []string{"DOG"}},
			},
			true,
			"",
			matchAmbiguous,
		},
		{
			// Same candidates, but substring disabled (class field key path):
			// no exact/fold match -> none, never a tie.
			"substring-disabled-no-match",
			"cat dog",
			[]matchCandidate{
				{name: "CAT", validValues: []string{"CAT"}},
				{name: "DOG", validValues: []string{"DOG"}},
			},
			false,
			"",
			matchNone,
		},
		{
			// Description candidate value matches (enum_match_candidates form).
			"description-match",
			"a primary color",
			[]matchCandidate{{name: "RED", validValues: []string{"RED", "a primary color", "RED: a primary color"}}},
			true,
			"RED",
			matchOne,
		},
	}
	for _, c := range cases {
		name, outcome, viaSub := matchString(c.input, c.candidates, c.allowSubstring)
		if outcome != c.wantOutcome || (outcome == matchOne && name != c.wantName) {
			t.Errorf("%s: matchString(%q) = (%q, %v), want (%q, %v)",
				c.name, c.input, name, outcome, c.wantName, c.wantOutcome)
		}
		// Only the substring-* cases should report a substring match (the
		// SubstringMatch flag, cost 2); exact/fold matches are score 0.
		wantSub := strings.HasPrefix(c.name, "substring") && c.wantOutcome != matchNone
		if viaSub != wantSub {
			t.Errorf("%s: matchString(%q) viaSubstring = %v, want %v", c.name, c.input, viaSub, wantSub)
		}
	}
}

// TestMatchesStringToString covers the no-substring field-key matcher: it
// folds case/punctuation/accents but never substring-matches.
func TestMatchesStringToString(t *testing.T) {
	cases := []struct {
		input, target string
		want          bool
	}{
		{"name", "name", true},
		{"Name", "name", true},
		{"  NAME  ", "name", true},
		{"full name", "fullname", true},  // space stripped to "fullname"
		{"Grün", "grun", true},           // accent fold + case
		{"act", "active", false},         // substring NOT allowed for field keys
		{"full_name", "fullname", false}, // '_' is KEPT, so no match
		{"names", "name", false},
		{"color", "shade", false},
		// #555 Slice 2: uppercase non-ASCII field keys now fold to their lowercase
		// rendered name and CLAIM the match (were formerly case-fold-uncertain).
		{"GRÜN", "grün", true},
		{"Ƛ", "ƛ", true}, // U+A7DC -> U+019B sentinel
		{"ÉTAT", "état", true},
	}
	for _, c := range cases {
		got := matchesStringToString(c.input, c.target)
		if got != c.want {
			t.Errorf("matchesStringToString(%q, %q) = %v, want %v", c.input, c.target, got, c.want)
		}
	}
}
