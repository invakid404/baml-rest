package nativeprompt

import (
	"testing"

	mj "github.com/mitsuhiko/minijinja/minijinja-go/v2"
	"github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// enumMember models a BAML enum value the way baml_value_to_jinja_value.rs does:
// it DISPLAYS its alias (if any) but its identity is the value NAME. BAML's
// value-cmp fork routes comparison through the value name, not the alias.
//
// In minijinja-Go the two comparison surfaces behave differently:
//   - ObjectWithCmp (ObjectCmp) IS consulted by ordering operators (<, >, sort,
//     min, max), so compare-by-name is reproducible there;
//   - it is NOT consulted by equality (==, !=, in), which go through
//     value.Value.Equal — and that has no object branch, so an enum object never
//     equals a string (or another object). BAML's fork DOES make == compare by
//     name. This is the one documented divergence; the dynamic template performs
//     no enum comparison, so it does not affect the parity proof, and Supports
//     fail-closes on enum globals anyway.
type enumMember struct {
	name  string
	alias string
}

func (e enumMember) GetAttr(name string) value.Value {
	switch name {
	case "name":
		return value.FromString(e.name)
	case "alias":
		return value.FromString(e.alias)
	}
	return value.Undefined()
}

func (e enumMember) ObjectString() string {
	if e.alias != "" {
		return e.alias
	}
	return e.name
}

func (e enumMember) ObjectCmp(other value.Object) (int, bool) {
	o, ok := other.(enumMember)
	if !ok {
		return 0, false
	}
	switch {
	case e.name < o.name:
		return -1, true
	case e.name > o.name:
		return 1, true
	default:
		return 0, true
	}
}

// enumGlobal is a BAML enum installed as a global (e.g. Color), resolving
// Color.RED to its member.
type enumGlobal struct{ members map[string]enumMember }

func (g enumGlobal) GetAttr(name string) value.Value {
	if m, ok := g.members[name]; ok {
		return value.FromObject(m)
	}
	return value.Undefined()
}

func valueCmpEnvWithColor() *mj.Environment {
	env := mj.NewEnvironment()
	env.SetAutoEscapeFunc(func(string) mj.AutoEscape { return mj.AutoEscapeNone })
	env.SetFormatter(func(_ *mj.State, val value.Value, escape func(string) string) string {
		if obj, ok := val.AsObject(); ok {
			if s, ok := obj.(value.ObjectWithString); ok {
				return escape(s.ObjectString())
			}
		}
		return escape(val.String())
	})
	env.AddGlobal("Color", value.FromObject(enumGlobal{members: map[string]enumMember{
		"RED":   {name: "RED", alias: "rouge"},
		"GREEN": {name: "GREEN", alias: "vert"},
		"BLUE":  {name: "BLUE", alias: "bleu"},
	}}))
	return env
}

func renderExpr(t *testing.T, src string) string {
	t.Helper()
	env := valueCmpEnvWithColor()
	tmpl, err := env.TemplateFromString(src)
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	out, err := tmpl.Render(nil)
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return out
}

// TestValueCmpOrderingReproducesBAML proves the reproducible half: minijinja-Go
// routes ORDERING through ObjectWithCmp, so a BAML enum sorts and compares by
// value NAME (not alias), matching BAML's value-cmp semantics.
func TestValueCmpOrderingReproducesBAML(t *testing.T) {
	// Display is the alias.
	if got := renderExpr(t, "{{ Color.RED }}"); got != "rouge" {
		t.Errorf("display: got %q, want alias %q", got, "rouge")
	}

	// Ordering is by NAME: BLUE < GREEN < RED (alphabetical by name), even
	// though the aliases (bleu, vert, rouge) sort differently.
	if got := renderExpr(t, "{{ Color.BLUE < Color.RED }}"); got != "true" {
		t.Errorf("BLUE<RED by name: got %q, want true", got)
	}
	if got := renderExpr(t, "{{ Color.RED < Color.BLUE }}"); got != "false" {
		t.Errorf("RED<BLUE by name: got %q, want false", got)
	}

	// sort orders by name: BLUE, GREEN, RED -> aliases bleu, vert, rouge.
	got := renderExpr(t, "{% for c in [Color.RED, Color.GREEN, Color.BLUE]|sort %}{{ c }},{% endfor %}")
	if got != "bleu,vert,rouge," {
		t.Errorf("sort by name: got %q, want %q", got, "bleu,vert,rouge,")
	}
}

// TestValueCmpEqualityDivergesFromBAML pins the one documented divergence:
// minijinja-Go's == does NOT route through ObjectWithCmp, so an enum member
// compares equal to neither its value name, its alias, nor another member. BAML
// v0.223's value-cmp fork makes `enum == "NAME"` true. This test asserts the
// minijinja-Go behaviour so a future minijinja-Go change that closes the gap is
// noticed. The dynamic template performs no enum comparison, so this does not
// affect the byte-parity claim.
func TestValueCmpEqualityDivergesFromBAML(t *testing.T) {
	cases := map[string]string{
		// (expr) -> minijinja-Go result. BAML value-cmp would make the first "true".
		"{{ Color.RED == 'RED' }}":      "false", // BAML: true  <-- divergence
		"{{ Color.RED == 'rouge' }}":    "false", // BAML: false (alias never matches)
		"{{ Color.RED == Color.RED }}":  "false", // BAML: true  <-- divergence
		"{{ 'RED' in [Color.RED] }}":    "false", // BAML: true  <-- divergence
		"{{ Color.RED == Color.BLUE }}": "false", // BAML: false
	}
	for expr, want := range cases {
		if got := renderExpr(t, expr); got != want {
			t.Errorf("%s => %q, want %q (minijinja-Go equality behaviour)", expr, got, want)
		}
	}
}
