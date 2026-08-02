package nativeprompt

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// renderThroughSeam renders src through the PRODUCTION render-context adapter
// (bamlprofile.New + the profile globals) with the given profile config. Tests
// use it instead of standing up a private environment, so what they pin is the
// environment production actually renders in.
func renderThroughSeam(t *testing.T, cfg bamlprofile.Config, src string) (string, error) {
	t.Helper()
	rc, err := renderContextFrom(cfg)
	if err != nil {
		t.Fatalf("renderContextFrom(%+v): %v", cfg, err)
	}
	return rc.renderToString(src, "test")
}

// mustRenderThroughSeam is renderThroughSeam for the cases where a render error
// is a test failure.
func mustRenderThroughSeam(t *testing.T, cfg bamlprofile.Config, src string) string {
	t.Helper()
	out, err := renderThroughSeam(t, cfg, src)
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return out
}

// roleMarkerViaSeam returns the role marker the profile's `_` global emits for a
// plain `_.role(role)`. It replaces the removed local emitRoleMarker in tests
// that need a marker literal: the bytes now come from the production helper
// rather than a second, drift-prone copy of the emit rule.
func roleMarkerViaSeam(t *testing.T, role string) string {
	t.Helper()
	return mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ _.role("`+role+`") }}`)
}

// TestRenderContextIsTheProfileEnvironment is the wiring proof for Slice 7.1a:
// the adapter's environment really is bamlprofile's get_env, not a local
// look-alike. Every row below is DISCRIMINATING — it fails (compile error,
// render error, or different bytes) on the pre-fork external engine plus
// nativeprompt's removed buildEnv, which had none of the get_env filter/method
// deltas and no per-enum globals.
func TestRenderContextIsTheProfileEnvironment(t *testing.T) {
	t.Run("get_env_filter_deltas", func(t *testing.T) {
		// regex_match exists only because bamlprofile adds BAML's get_env filter;
		// buildEnv never had it (an unknown filter is a compile/render error).
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ "abc123"|regex_match("[0-9]+") }}`); got != "true" {
			t.Errorf(`"abc123"|regex_match("[0-9]+") = %q, want "true"`, got)
		}
		// BAML's sum OVERRIDES the engine builtin: a non-numeric sequence sums to
		// 0 instead of erroring, which is only true of the profile's sum.
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ ["a","b"]|sum }}`); got != "0" {
			t.Errorf(`["a","b"]|sum = %q, want "0" (BAML's sum, not the builtin)`, got)
		}
	})

	t.Run("pycompat_unknown_method_callback", func(t *testing.T) {
		// set_unknown_method_callback(pycompat) is a get_env delta; buildEnv had none.
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ "AB".lower() }}`); got != "ab" {
			t.Errorf(`"AB".lower() = %q, want "ab" (pycompat callback)`, got)
		}
	})

	t.Run("get_env_engine_config", func(t *testing.T) {
		// Top-level none renders as "null" (BAML's custom formatter).
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ none }}`); got != "null" {
			t.Errorf("{{ none }} = %q, want %q", got, "null")
		}
		// trim_blocks + lstrip_blocks: the indented block tag and its trailing
		// newline both vanish.
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, "a\n  {% if true %}\nb\n  {% endif %}\nc"); got != "a\nb\nc" {
			t.Errorf("whitespace control = %q, want %q", got, "a\nb\nc")
		}
		// Autoescape off: prompts are plain text, never HTML-escaped.
		if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ "<b>" }}`); got != "<b>" {
			t.Errorf("autoescape = %q, want %q", got, "<b>")
		}
	})

	t.Run("prompt_globals", func(t *testing.T) {
		// ctx.output_format is the per-render config value, not a fixed global.
		if got := mustRenderThroughSeam(t, bamlprofile.Config{OutputFormat: "BLOCK"}, `{{ ctx.output_format }}`); got != "BLOCK" {
			t.Errorf("ctx.output_format = %q, want %q", got, "BLOCK")
		}
		// `_` emits the role marker lower.go splits on.
		marker := roleMarkerViaSeam(t, "user")
		if !strings.HasPrefix(marker, roleDelim+roleMarkerPrefix) || !strings.HasSuffix(marker, roleMarkerSuffix+roleDelim) {
			t.Errorf("role marker = %q, want the roleDelim/marker-affix wrapping", marker)
		}
		role, allowDupe, meta, err := parseRoleMarker(strings.Split(marker, roleDelim)[1])
		if err != nil || role != "user" || allowDupe || meta != nil {
			t.Errorf("parseRoleMarker(%q) = (%q, %v, %v, %v)", marker, role, allowDupe, meta, err)
		}
	})

	t.Run("enum_namespace_globals", func(t *testing.T) {
		// The adapter forwards a resolved enum set to bamlprofile.New, which
		// installs one namespace global per enum. As of Slice 7.1b the STATIC
		// production constructor forwards the descriptor's WHOLE project enum set
		// here (rendercontext.go); this drives the same seam directly.
		// Display is the alias; `.value` is the canonical name.
		cfg := bamlprofile.Config{Enums: []bamlprofile.EnumDef{{
			Name: "Color",
			Values: []bamlprofile.EnumValue{
				{Canonical: "RED", Alias: aliasOf("rouge")},
				{Canonical: "GREEN", Alias: aliasOf("vert")},
				{Canonical: "BLUE", Alias: aliasOf("bleu")},
			},
		}}}
		got := mustRenderThroughSeam(t, cfg, `{{ Color.RED }}|{{ Color.RED.value }}|{{ Color.GREEN }}`)
		if want := "rouge|RED|vert"; got != want {
			t.Errorf("enum globals = %q, want %q", got, want)
		}
	})

	t.Run("malformed_enum_config_fails_loud", func(t *testing.T) {
		// A resolved-metadata contract violation must surface, not render through a
		// half-populated environment.
		_, err := renderContextFrom(bamlprofile.Config{Enums: []bamlprofile.EnumDef{
			{Name: "Color", Values: []bamlprofile.EnumValue{{Canonical: "RED"}}},
			{Name: "Color", Values: []bamlprofile.EnumValue{{Canonical: "BLUE"}}},
		}})
		if err == nil {
			t.Fatal("duplicate enum definition should fail loud, got nil error")
		}
		if !strings.Contains(err.Error(), "nativeprompt: build profile environment") {
			t.Errorf("error should be wrapped by the adapter, got %v", err)
		}
	})
}

// TestRoleHelperAllowDupeMustBeBool pins the fail-closed
// __baml_allow_dupe_role__ contract at the PRODUCTION seam: a non-bool value is
// rejected rather than silently coerced to false, and a real bool is carried
// into the role marker. It replaces the removed env_test.go, which called
// nativeprompt's own roleHelper — a duplicate of the profile's `_` that no
// longer exists.
func TestRoleHelperAllowDupeMustBeBool(t *testing.T) {
	for _, bad := range []string{`"true"`, `1`, `none`, `[]`} {
		src := `{{ _.role("user", ` + allowDupeRoleKey + `=` + bad + `) }}`
		if out, err := renderThroughSeam(t, bamlprofile.Config{}, src); err == nil {
			t.Errorf("%s should be rejected, rendered %q", src, out)
		}
	}

	marker := mustRenderThroughSeam(t, bamlprofile.Config{},
		`{{ _.role("user", `+allowDupeRoleKey+`=true) }}`)
	role, allowDupe, meta, err := parseRoleMarker(strings.Split(marker, roleDelim)[1])
	if err != nil {
		t.Fatalf("parseRoleMarker: %v", err)
	}
	if role != "user" || !allowDupe {
		t.Errorf("role=%q allowDupe=%v, want user/true", role, allowDupe)
	}
	// The reserved kwarg never becomes message metadata.
	if meta != nil {
		t.Errorf("meta = %v, want nil (%s is reserved)", meta, allowDupeRoleKey)
	}
}
