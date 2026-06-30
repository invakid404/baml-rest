package debaml

import "testing"

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
		// folds to "GREEN".
		{"punctuation", "(GREEN).", enumLike, true, "GREEN", matchOne},
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
		name, outcome := matchString(c.input, c.candidates, c.allowSubstring)
		if outcome != c.wantOutcome || (outcome == matchOne && name != c.wantName) {
			t.Errorf("%s: matchString(%q) = (%q, %v), want (%q, %v)",
				c.name, c.input, name, outcome, c.wantName, c.wantOutcome)
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
		if got := matchesStringToString(c.input, c.target); got != c.want {
			t.Errorf("matchesStringToString(%q, %q) = %v, want %v", c.input, c.target, got, c.want)
		}
	}
}
