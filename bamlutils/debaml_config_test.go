package bamlutils

import (
	"os"
	"testing"
)

// TestIsFalsyEnvValue pins the explicit-falsy accept list that backs the
// default-ON env vars: exactly 0/false/no/off (case-insensitive) are
// recognized as an explicit "disable"; everything else — including empty,
// whitespace-padded variants (no trimming), truthy tokens, and unknown
// values — is not falsy. It is the exact inverse of IsTruthyEnvValue's
// accept list.
func TestIsFalsyEnvValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"false", true},
		{"no", true},
		{"off", true},
		{"FALSE", true},
		{"No", true},
		{"Off", true},
		{"OFF", true},
		{"", false},
		{"1", false},
		{"true", false},
		{"yes", false},
		{"on", false},
		{" false ", false}, // no whitespace trimming — mirrors IsTruthyEnvValue
		{"falsey", false},
		{"2", false},
		{"disable", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := IsFalsyEnvValue(tc.in); got != tc.want {
				t.Errorf("IsFalsyEnvValue(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDeBAMLConfigFromEnv pins the DEFAULT-ON truth table for the umbrella
// switch: native de-BAML is enabled when BAML_REST_USE_DEBAML is unset or
// empty OR set to a truthy value, and disabled ONLY when set to a
// recognized falsy value (0/false/no/off, case-insensitive). Any
// unrecognized token leaves the feature on — default-on means "off only on
// an explicit falsy value". This is NOT parallel: it mutates process env.
func TestDeBAMLConfigFromEnv(t *testing.T) {
	// unset -> enabled (default-on)
	t.Run("unset", func(t *testing.T) {
		withEnvUseDeBAMLUnset(t)
		if got := DeBAMLConfigFromEnv(); !got.Enabled {
			t.Errorf("DeBAMLConfigFromEnv() with %s unset = %+v, want Enabled=true (default-on)", EnvUseDeBAML, got)
		}
	})

	cases := []struct {
		value string
		want  bool
	}{
		// empty and truthy values -> enabled
		{"", true},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{"On", true},
		// unrecognized tokens are NOT falsy -> stay enabled (default-on)
		{"maybe", true},
		{"2", true},
		{" false ", true}, // whitespace-padded is not a recognized falsy token
		// explicit falsy values -> disabled
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"FALSE", false},
		{"Off", false},
	}
	for _, tc := range cases {
		name := tc.value
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvUseDeBAML, tc.value)
			if got := DeBAMLConfigFromEnv(); got.Enabled != tc.want {
				t.Errorf("DeBAMLConfigFromEnv() with %s=%q = %+v, want Enabled=%v", EnvUseDeBAML, tc.value, got, tc.want)
			}
		})
	}
}

// withEnvUseDeBAMLUnset removes BAML_REST_USE_DEBAML for the duration of a
// test and restores its prior state afterwards. t.Setenv only sets a value;
// the default-on "unset" leg needs a genuine absence, so this saves/clears/
// restores by hand.
func withEnvUseDeBAMLUnset(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv(EnvUseDeBAML)
	if err := os.Unsetenv(EnvUseDeBAML); err != nil {
		t.Fatalf("os.Unsetenv(%s): %v", EnvUseDeBAML, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(EnvUseDeBAML, prev)
		} else {
			_ = os.Unsetenv(EnvUseDeBAML)
		}
	})
}
