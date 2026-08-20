package main

import (
	"strconv"
	"strings"
	"testing"
)

// De-BAML serving cutover S2 — the ARTIFACT SELECTOR decoder.
//
// These two flags choose which artifact ships. They used to be read with
// viper.GetBool, whose cast turns any unrecognised non-empty string into false
// without complaint, so `BAML_REST_NATIVE_WORKER=yes` silently selected the
// BAML-only ROLLBACK artifact from a build that plainly asked for the standard
// one — and slipped past the explicit-selection conflict check, because the value
// it produced did not contradict anything.
//
// cmd/build/build.sh has always decoded its own NATIVE_WORKER/SHADOW_WORKER
// strictly. TestArtifactSelectorsAreStrictlyDecoded (artifact_profile_test.go)
// drives that half against build.sh itself; this file drives the front end, so
// the two agree on exactly which spellings exist.

// TestBoolSelectorRejectsMalformedEnvironmentValues is the negative half: a
// malformed value must FAIL THE BUILD, never resolve to a silent false.
func TestBoolSelectorRejectsMalformedEnvironmentValues(t *testing.T) {
	for _, raw := range []string{
		"yes", "no", "1", "0", "TRUE", "False", "on", "off", "y", "n", "maybe", "true false",
		// WHITESPACE. build.sh matches `""|true|false` raw, so every one of these
		// is rejected there. The front end used to TrimSpace first, which made
		// " false " select the rollback artifact here while failing the build
		// script, and made a whitespace-only value silently mean "unset" — i.e.
		// the STANDARD artifact — while build.sh rejected it.
		" false ", " true ", "false ", " false", "\tfalse", "false\n", " ", "  ", "\t", "\n",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("BAML_REST_NATIVE_WORKER", raw)
			value, explicit, err := resolveBoolSelector(boolSelectorInputs{
				Name:    "native-worker",
				EnvKey:  "BAML_REST_NATIVE_WORKER",
				Default: true,
			})
			if err == nil {
				t.Fatalf("resolveBoolSelector accepted BAML_REST_NATIVE_WORKER=%q and resolved to value=%v explicit=%v; "+
					"a typo must fail the build, not silently select the other artifact", raw, value, explicit)
			}
			// The message has to name the variable and the flag, because the whole
			// point is that an operator can see which selector they mistyped.
			for _, want := range []string{"BAML_REST_NATIVE_WORKER", "native-worker"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestBoolSelectorRejectsMalformedRollbackSelector pins the SAME rule on the
// other half of the axis. A silent cast there ships the STANDARD artifact from a
// build that asked to roll back — the more dangerous direction of the two.
func TestBoolSelectorRejectsMalformedRollbackSelector(t *testing.T) {
	t.Setenv("BAML_REST_BAML_ONLY_ROLLBACK_WORKER", "yes")
	if _, _, err := resolveBoolSelector(boolSelectorInputs{
		Name:    "baml-only-rollback-worker",
		EnvKey:  "BAML_REST_BAML_ONLY_ROLLBACK_WORKER",
		Default: false,
	}); err == nil {
		t.Fatal("resolveBoolSelector accepted a malformed rollback selector; a build that asked to roll back would have shipped the standard artifact")
	}
}

// TestBoolSelectorPrecedenceAndValidValues pins that the strictness did not cost
// the precedence viper provided: an explicitly-set FLAG wins over the
// environment, the environment wins over the config file, and an absent/empty
// setting falls through to the default without being reported as explicit.
func TestBoolSelectorPrecedenceAndValidValues(t *testing.T) {
	const envKey = "BAML_REST_NATIVE_WORKER"

	for _, tc := range []struct {
		name          string
		env           string
		envSet        bool
		flagChanged   bool
		flagValue     bool
		configValue   any
		configPresent bool
		wantValue     bool
		wantExplicit  bool
	}{
		{name: "nothing set falls through to the default", wantValue: true},
		{name: "env true", env: "true", envSet: true, wantValue: true, wantExplicit: true},
		{name: "env false", env: "false", envSet: true, wantValue: false, wantExplicit: true},
		{
			name: "EMPTY env means UNSET, matching build.sh's ${VAR:-default}",
			env:  "", envSet: true, wantValue: true,
		},
		{
			name: "an explicit flag beats the environment",
			env:  "false", envSet: true,
			flagChanged: true, flagValue: true,
			wantValue: true, wantExplicit: true,
		},
		{
			name: "the environment beats the config file",
			env:  "false", envSet: true,
			configValue: true, configPresent: true,
			wantValue: false, wantExplicit: true,
		},
		{
			name:        "a config bool is used when nothing else set it",
			configValue: false, configPresent: true,
			wantValue: false, wantExplicit: true,
		},
		{
			name:        "a config string is decoded with the same strictness",
			configValue: "false", configPresent: true,
			wantValue: false, wantExplicit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv(envKey, tc.env)
			} else {
				// t.Setenv cannot unset; make sure a developer's shell cannot leak
				// the very variable under test into the no-env cases.
				t.Setenv(envKey, "")
			}
			value, explicit, err := resolveBoolSelector(boolSelectorInputs{
				Name:          "native-worker",
				EnvKey:        envKey,
				FlagChanged:   tc.flagChanged,
				FlagValue:     tc.flagValue,
				ConfigValue:   tc.configValue,
				ConfigPresent: tc.configPresent,
				Default:       true,
			})
			if err != nil {
				t.Fatalf("resolveBoolSelector: %v", err)
			}
			if value != tc.wantValue {
				t.Errorf("value = %v, want %v", value, tc.wantValue)
			}
			if explicit != tc.wantExplicit {
				t.Errorf("explicit = %v, want %v", explicit, tc.wantExplicit)
			}
		})
	}
}

// TestBoolSelectorRejectsMalformedConfigValues pins the config-file half: a
// non-boolean, non-decodable entry fails rather than being inferred.
func TestBoolSelectorRejectsMalformedConfigValues(t *testing.T) {
	t.Setenv("BAML_REST_NATIVE_WORKER", "")
	for _, cv := range []any{"yes", "1", 1, 0, []string{"true"}} {
		if _, _, err := resolveBoolSelector(boolSelectorInputs{
			Name:          "native-worker",
			EnvKey:        "BAML_REST_NATIVE_WORKER",
			ConfigValue:   cv,
			ConfigPresent: true,
			Default:       true,
		}); err == nil {
			t.Errorf("resolveBoolSelector accepted config value %#v", cv)
		}
	}
}

// TestFrontEndAndBuildScriptAcceptTheSameSpellings is the agreement check, and it
// drives the REAL front-end resolver — not parseStrictBool in isolation.
//
// That distinction is the reason this test exists in this form. An earlier
// version called parseStrictBool directly, which skipped the resolver's own
// normalisation, so it reported agreement while resolveBoolSelector was quietly
// trimming whitespace and build.sh was not. A parity test that does not run the
// production path is not a parity test.
func TestFrontEndAndBuildScriptAcceptTheSameSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool // whether BOTH sides must accept it
	}{
		{name: "true", raw: "true", want: true},
		{name: "false", raw: "false", want: true},
		{name: "empty means unset on both sides", raw: "", want: true},
		{name: "yes", raw: "yes", want: false},
		{name: "one", raw: "1", want: false},
		{name: "TRUE", raw: "TRUE", want: false},
		{name: "off", raw: "off", want: false},
		// The whitespace spellings the front end used to accept and build.sh
		// always rejected.
		{name: "padded false", raw: " false ", want: false},
		{name: "padded true", raw: " true ", want: false},
		{name: "leading space", raw: " false", want: false},
		{name: "trailing space", raw: "false ", want: false},
		{name: "whitespace only", raw: " ", want: false},
		{name: "tab only", raw: "\t", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// FRONT END: the actual resolver, reading the actual environment.
			t.Setenv("BAML_REST_NATIVE_WORKER", tc.raw)
			_, _, frontEndErr := resolveBoolSelector(boolSelectorInputs{
				Name:    "native-worker",
				EnvKey:  "BAML_REST_NATIVE_WORKER",
				Default: true,
			})
			if (frontEndErr == nil) != tc.want {
				t.Fatalf("front end accepted NATIVE_WORKER=%q = %v, want %v (err: %v)",
					tc.raw, frontEndErr == nil, tc.want, frontEndErr)
			}

			// BUILD SCRIPT: the same raw value, through build.sh's own decoder.
			out, scriptErr := execBuildScript(t, map[string]string{"NATIVE_WORKER": tc.raw})
			if (scriptErr == nil) != tc.want {
				t.Fatalf("build.sh accepted NATIVE_WORKER=%q = %v, want %v:\n%s",
					tc.raw, scriptErr == nil, tc.want, out)
			}

			// And the SHADOW selector build.sh also decodes, so the agreement is
			// not proven on one variable and assumed for the other.
			if tc.raw != "" {
				shadowOut, shadowErr := execBuildScript(t, map[string]string{"SHADOW_WORKER": tc.raw})
				if (shadowErr == nil) != tc.want {
					t.Fatalf("build.sh accepted SHADOW_WORKER=%q = %v, want %v:\n%s",
						tc.raw, shadowErr == nil, tc.want, shadowOut)
				}
			}
		})
	}
}

// TestBothSelectorsRejectWhitespaceIdentically pins the same raw strictness on
// the ROLLBACK selector. build.sh has no variable of its own for it, so the
// parity that matters is the RULE: whatever spelling the front end rejects for
// one artifact selector it must reject for the other, or a whitespace-padded
// value could still silently choose an artifact through the half nobody tested.
func TestBothSelectorsRejectWhitespaceIdentically(t *testing.T) {
	selectors := []struct {
		name   string
		envKey string
		def    bool
	}{
		{"native-worker", "BAML_REST_NATIVE_WORKER", true},
		{"baml-only-rollback-worker", "BAML_REST_BAML_ONLY_ROLLBACK_WORKER", false},
	}
	for _, sel := range selectors {
		for _, raw := range []string{" false ", " true ", " ", "\t", "false ", " true"} {
			t.Run(sel.name+"/"+strconv.Quote(raw), func(t *testing.T) {
				t.Setenv(sel.envKey, raw)
				value, explicit, err := resolveBoolSelector(boolSelectorInputs{
					Name:    sel.name,
					EnvKey:  sel.envKey,
					Default: sel.def,
				})
				if err == nil {
					t.Fatalf("%s=%q resolved to value=%v explicit=%v instead of failing; "+
						"build.sh rejects this spelling, so accepting it here can select an artifact the build script would refuse to produce",
						sel.envKey, raw, value, explicit)
				}
			})
		}
	}

	// A config-file string gets the same raw treatment, so the strictness cannot
	// be bypassed by moving the value from the environment into baml-rest.toml.
	t.Setenv("BAML_REST_NATIVE_WORKER", "")
	if _, _, err := resolveBoolSelector(boolSelectorInputs{
		Name:          "native-worker",
		EnvKey:        "BAML_REST_NATIVE_WORKER",
		ConfigValue:   " false ",
		ConfigPresent: true,
		Default:       true,
	}); err == nil {
		t.Error("a whitespace-padded config string was accepted; the config path must be as raw-strict as the environment path")
	}
}
