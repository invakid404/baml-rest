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

// TestUndLowerCaser pins that native's case fold follows Rust's full-Unicode
// str::to_lowercase, not Go's simple strings.ToLower: İ (U+0130) lowercases to
// "i" + U+0307 (combining dot above), which Go's strings.ToLower flattens to a
// bare "i". (Both fold to "i" after the inner accent removal, but the
// intermediate must match Rust.)
func TestUndLowerCaser(t *testing.T) {
	// İ (U+0130) -> "i" + U+0307 (combining dot above) under full Unicode
	// lowercasing (Rust / cases.Lower), vs a bare "i" under strings.ToLower.
	if got := undLowerCaser().String("İ"); got != "i̇" {
		t.Errorf("undLowerCaser().String(U+0130) = %q, want %q", got, "i̇")
	}
	if got := strings.ToLower("İ"); got != "i" {
		t.Errorf("strings.ToLower(U+0130) = %q, want %q (sanity: it differs from Rust)", got, "i")
	}
}

// TestCaseFoldUncertain pins the conservative non-ASCII case-fold uncertainty
// test: genuinely lowercase non-ASCII letters are CERTAIN (so accented inputs
// still match), while anything Go can't prove is lowercase-stable is uncertain.
func TestCaseFoldUncertain(t *testing.T) {
	// ASCII, plus non-ASCII letters Go reports IsLower (é, ß, ü, ƛ U+019B).
	certain := []string{"", "name", "RESUME", "résumé", "straße", "grün", "ƛ"}
	for _, s := range certain {
		if caseFoldUncertain(s) {
			t.Errorf("caseFoldUncertain(%q) = true, want false", s)
		}
	}
	// U+A7DC 'Ƛ' (x/text leaves it unchanged but Rust lowercases it to U+019B),
	// accented uppercase 'É' (U+00C9), and 'İ' (U+0130): all non-ASCII and not
	// IsLower -> uncertain.
	uncertain := []string{"Ƛ", "ÉTAT", "İD"}
	for _, s := range uncertain {
		if !caseFoldUncertain(s) {
			t.Errorf("caseFoldUncertain(%q) = false, want true", s)
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
		name, outcome, viaSub, uncertain := matchString(c.input, c.candidates, c.allowSubstring)
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
		// All these cases are ASCII (or fold before the case-fold attempt), so
		// none should be flagged non-ASCII-case-fold-uncertain.
		if uncertain {
			t.Errorf("%s: matchString(%q) unexpectedly uncertain", c.name, c.input)
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
	}
	for _, c := range cases {
		got, uncertain := matchesStringToString(c.input, c.target)
		if got != c.want {
			t.Errorf("matchesStringToString(%q, %q) = %v, want %v", c.input, c.target, got, c.want)
		}
		if uncertain {
			t.Errorf("matchesStringToString(%q, %q) unexpectedly uncertain (all ASCII)", c.input, c.target)
		}
	}
}
