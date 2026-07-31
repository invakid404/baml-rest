package bamlprofile

import (
	"errors"
	"math"
	"strings"
	"testing"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/value"
)

// These are pure-Go smoke tests: they check that New wires up the get_env
// engine config (formatter, whitespace, the get_env filter overrides, pycompat,
// and the ctx/_ globals) and produces the obvious output. They are fast feedback
// only. The AUTHORITY for byte-exactness is the stock-BAML-v0.223 differential in
// ./profileoracle (build tag `integration`), not these hand-written expectations.

func render(t *testing.T, cfg Config, src string) string {
	t.Helper()
	env, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	tmpl, err := env.TemplateFromNamedString("smoke.txt", src)
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	out, err := tmpl.Render(map[string]any{})
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return out
}

func TestSmokeGetEnv(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"arith", `{{ 1 + 1 }}`, "2"},
		{"none_is_null", `{{ none }}`, "null"},
		{"sum_ints", `{{ [1, 2, 3]|sum }}`, "6"},
		{"sum_mixed_float", `{{ [1, 2.5]|sum }}`, "3.5"},
		{"sum_empty", `{{ []|sum }}`, "0"},
		{"regex_match_true", `{{ "abc123"|regex_match("[0-9]+") }}`, "true"},
		{"regex_match_false", `{{ "abc"|regex_match("[0-9]+") }}`, "false"},
		{"regex_match_bad_pattern", `{{ "x"|regex_match("(") }}`, "false"},
		{"pycompat_upper", `{{ "abc".upper() }}`, "ABC"},
		{"pycompat_items_len", `{{ {"a": 1, "b": 2}.items()|list|length }}`, "2"},
		{"macro", `{% macro greet(n) %}Hi {{ n }}{% endmacro %}{{ greet("x") }}`, "Hi x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(t, Config{}, tc.src); got != tc.want {
				t.Errorf("render(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

func TestSmokeCtxOutputFormat(t *testing.T) {
	if got := render(t, Config{OutputFormat: "SCHEMA-BLOCK"}, `{{ ctx.output_format }}`); got != "SCHEMA-BLOCK" {
		t.Errorf("ctx.output_format = %q, want %q", got, "SCHEMA-BLOCK")
	}
	if got := render(t, Config{}, `{{ ctx.output_format }}`); got != "" {
		t.Errorf("empty ctx.output_format = %q, want empty", got)
	}
}

func TestSmokeRoleHelperEmitsMarker(t *testing.T) {
	// _.role emits BAML's role marker; the split into messages is a later-slice
	// (lower.go) concern. Here we only check the marker is emitted and carries the
	// role, so the profile stays wire-compatible with nativeprompt/lower.go.
	out := render(t, Config{}, `{{ _.role("user") }}hello`)
	if wantMarker := roleDelim + roleMarkerPrefix; !strings.HasPrefix(out, wantMarker) {
		t.Fatalf("role marker not emitted: %q", out)
	}
	if want := `"role":"user"`; !strings.Contains(out, want) {
		t.Errorf("marker missing %s: %q", want, out)
	}
}

// TestSmokeRoleErrorKinds pins that `_.role` reports BAML's typed minijinja error
// KINDS (not a generic error): supplying both a positional and a role= kwarg is
// ErrTooManyArguments, and supplying neither is ErrMissingArgument.
func TestSmokeRoleErrorKinds(t *testing.T) {
	cases := []struct {
		name string
		src  string
		kind minijinja.ErrorKind
	}{
		{"both", `{{ _.role("user", role="admin") }}`, minijinja.ErrTooManyArguments},
		{"neither", `{{ _.role() }}`, minijinja.ErrMissingArgument},
		{"too_many_positional", `{{ _.role("user", "admin") }}`, minijinja.ErrTooManyArguments},
		{"positional_not_string", `{{ _.role(123) }}`, minijinja.ErrInvalidOperation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := New(Config{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tmpl, err := env.TemplateFromNamedString("smoke.txt", tc.src)
			if err != nil {
				t.Fatalf("compile %q: %v", tc.src, err)
			}
			_, err = tmpl.Render(map[string]any{})
			if err == nil {
				t.Fatalf("render %q: expected an error, got nil", tc.src)
			}
			var me *minijinja.Error
			if !errors.As(err, &me) || me.Kind != tc.kind {
				t.Errorf("render %q: got error %v (kind %v), want kind %v", tc.src, err, kindOf(err), tc.kind)
			}
		})
	}
}

// TestRoleMarshalErrorFailsLoud pins that a metadata kwarg that is not
// JSON-serializable (a NaN float) makes `_.role` return an ERROR, not a
// role-less/degraded marker the downstream lowerer cannot interpret.
func TestRoleMarshalErrorFailsLoud(t *testing.T) {
	bad := value.NewOrderedMap(2)
	bad.Set("role", value.FromString("user"))
	bad.Set("meta", value.FromFloat(math.NaN()))
	if _, err := (roleHelper{}).CallMethod(nil, "role", nil, bad); err == nil {
		t.Fatal("_.role with a NaN metadata kwarg returned nil error (silent degradation)")
	} else {
		var me *minijinja.Error
		if !errors.As(err, &me) || me.Kind != minijinja.ErrInvalidOperation {
			t.Errorf("got error %v (kind %v), want kind %v", err, kindOf(err), minijinja.ErrInvalidOperation)
		}
	}

	// A well-formed call still succeeds and emits a role marker.
	good := value.NewOrderedMap(1)
	good.Set("role", value.FromString("user"))
	got, err := (roleHelper{}).CallMethod(nil, "role", nil, good)
	if err != nil {
		t.Fatalf("well-formed _.role errored: %v", err)
	}
	if s, _ := got.AsString(); !strings.Contains(s, `"role":"user"`) {
		t.Errorf("well-formed marker missing role: %q", s)
	}
}

func kindOf(err error) any {
	var me *minijinja.Error
	if errors.As(err, &me) {
		return me.Kind
	}
	return "(not *minijinja.Error)"
}
