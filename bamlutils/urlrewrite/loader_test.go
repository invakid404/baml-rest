package urlrewrite

import (
	"testing"
)

// TestLoadDefaultRulesEnvOverridesBuiltin pins the env-overrides-
// builtin precedence on the uncached loader, mirroring the cached
// GlobalRules contract every existing call site already depends on.
// Without the same precedence, cmd/serve and cmd/worker would
// silently observe different rewrite rules from previous boots
// after the migration to LoadDefaultRules.
func TestLoadDefaultRulesEnvOverridesBuiltin(t *testing.T) {
	// Not parallel: mutates builtinRules / env.
	prev := builtinRules
	defer func() { builtinRules = prev }()
	builtinRules = "https://baked.example/=http://baked-target.local/"

	t.Run("env present overrides builtin", func(t *testing.T) {
		t.Setenv("BAML_REST_BASE_URL_REWRITES", "https://env.example/=http://env-target.local/")
		rules := LoadDefaultRules()
		if len(rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(rules))
		}
		if rules[0].From != "https://env.example/" || rules[0].To != "http://env-target.local/" {
			t.Errorf("expected env rule, got %+v", rules[0])
		}
	})

	t.Run("env empty falls back to builtin", func(t *testing.T) {
		t.Setenv("BAML_REST_BASE_URL_REWRITES", "")
		rules := LoadDefaultRules()
		if len(rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(rules))
		}
		if rules[0].From != "https://baked.example/" || rules[0].To != "http://baked-target.local/" {
			t.Errorf("expected builtin rule, got %+v", rules[0])
		}
	})
}

// TestLoadDefaultRulesIsUncached pins that consecutive calls don't
// cache. Operators relying on cached GlobalRules behaviour stay on
// that helper; the explicit loader must re-read every time so
// programmatic callers can swap rules between constructions without
// poking package state.
func TestLoadDefaultRulesIsUncached(t *testing.T) {
	// Clear BAML_REST_BASE_URL_REWRITES so a developer-local env
	// value doesn't shadow the builtinRules under test. t.Setenv
	// restores the prior value on cleanup.
	t.Setenv("BAML_REST_BASE_URL_REWRITES", "")

	prev := builtinRules
	defer func() { builtinRules = prev }()

	builtinRules = "https://first.example/=http://one.local/"
	first := LoadDefaultRules()
	if len(first) != 1 || first[0].To != "http://one.local/" {
		t.Fatalf("first call: unexpected rules %+v", first)
	}

	builtinRules = "https://second.example/=http://two.local/"
	second := LoadDefaultRules()
	if len(second) != 1 || second[0].To != "http://two.local/" {
		t.Fatalf("second call: expected fresh read, got %+v", second)
	}
}
