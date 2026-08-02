package nativeprompt

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// This file is the #597 enum fence, rewritten for de-BAML Slice 7.1b.
//
// Its history matters. Before Slice 7.1a it stood up a PRIVATE enum object on
// the pre-fork external engine and pinned that engine's divergence from BAML:
// `Color.RED == 'RED'` rendered false. 7.1a cut over to bamlprofile and the
// ENGINE started answering exactly as stock BAML v0.223 — but nothing was
// ADMITTED, so the file could only prove leaf behaviour.
//
// 7.1b closes it end to end. Every row below now goes through the REAL path:
//
//	V3 descriptor -> V3 binder -> bamlprofile render context -> RenderStatic
//
// so a passing row is evidence about the SERVED surface, not about a leaf that
// happens to match. The file has two halves, and both are load-bearing:
//
//   - MATCHES BAML v0.223: the admitted grammar renders the stock answers, and
//     a bound enum / class / list renders BAML's host-value spelling. These are
//     what 7.1b claims.
//   - DELIBERATE DECLINES: the near neighbours stay declined, including the
//     historical `Color.RED == 'rouge'` row, whose stock answer (`false`) this
//     slice deliberately does NOT claim. A display alias is not an identity;
//     admitting that row would make it a second equality language. The stock
//     integration oracle owns its exact BAML output.
//
// bamlprofile keeps its own leaf comparator tests; they do not stand in for
// native static admission, which is what this file proves.

// enumUniverse is the V3 universe every row here resolves against: one project
// enum whose members carry deliberately non-canonical display aliases.
func enumUniverse() promptdescriptor.InputValueUniverse {
	return promptdescriptor.InputValueUniverse{
		ProjectEnums: []promptdescriptor.ResolvedEnum{testColorEnum()},
	}
}

// enumPredicateFn builds a one-message chat whose only content is expr, so the
// rendered message text IS the expression's rendered value.
func enumPredicateFn(expr string, args ...promptdescriptor.Argument) promptdescriptor.Function {
	fn := staticFn(`{{ _.role("user") }}`+"\n"+expr, args...)
	fn.InputValues = enumUniverse()
	return fn
}

// colorArg declares `color: Color`.
func colorArg(name string) promptdescriptor.Argument {
	return v3Arg(name, promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"})
}

// renderOnlyMessage renders fn with values and returns its single message's
// single text part.
func renderOnlyMessage(t *testing.T, fn promptdescriptor.Function, values []promptdescriptor.ArgumentValue) string {
	t.Helper()
	rp := mustRenderStaticValues(t, fn, values)
	if rp.Kind != KindChat || len(rp.Messages) != 1 || len(rp.Messages[0].Parts) != 1 ||
		rp.Messages[0].Parts[0].Text == nil {
		t.Fatalf("expected a single-text-part chat message, got %+v", rp)
	}
	return *rp.Messages[0].Parts[0].Text
}

// ---------------------------------------------------------------------------
// Matches BAML v0.223
// ---------------------------------------------------------------------------

// TestAdmittedEnumPredicatesMatchBAML drives every ADMITTED equality and
// membership form of the Slice 7.1b grammar through descriptor -> binder ->
// renderer and pins stock BAML v0.223's answer.
//
// The five historical #597 rows that this slice claims are here, plus the
// reverse operand orders and the enum-ARGUMENT forms. Each is admitted only
// after the V3 type gate resolved every operand, so a row passing here means
// the SERVED path produces it.
func TestAdmittedEnumPredicatesMatchBAML(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		args   []promptdescriptor.Argument
		values []promptdescriptor.ArgumentValue
		want   string
	}{
		// Canonical-name equality, member token on either side (#597 row 1).
		{"member_eq_canonical", `{{ Color.RED == 'RED' }}`, nil, nil, "true"},
		{"canonical_eq_member", `{{ 'RED' == Color.RED }}`, nil, nil, "true"},
		{"member_eq_canonical_false", `{{ Color.RED == 'BLUE' }}`, nil, nil, "false"},

		// Same-member and different-member equality (#597 rows 3 and 5).
		{"member_eq_same_member", `{{ Color.RED == Color.RED }}`, nil, nil, "true"},
		{"member_eq_other_member", `{{ Color.RED == Color.BLUE }}`, nil, nil, "false"},

		// One-element membership, both stock-proven directions (#597 row 4).
		{"canonical_in_member_list", `{{ 'RED' in [Color.RED] }}`, nil, nil, "true"},
		{"member_in_canonical_list", `{{ Color.RED in ['RED'] }}`, nil, nil, "true"},
		{"canonical_in_member_list_false", `{{ 'BLUE' in [Color.RED] }}`, nil, nil, "false"},

		// Enum ARGUMENT vs canonical string, both operand orders.
		{"arg_eq_canonical", `{{ color == 'RED' }}`,
			[]promptdescriptor.Argument{colorArg("color")},
			vals(argV("color", enumV("Color", "RED"))), "true"},
		{"canonical_eq_arg", `{{ 'RED' == color }}`,
			[]promptdescriptor.Argument{colorArg("color")},
			vals(argV("color", enumV("Color", "RED"))), "true"},
		{"arg_eq_canonical_false", `{{ color == 'RED' }}`,
			[]promptdescriptor.Argument{colorArg("color")},
			vals(argV("color", enumV("Color", "GREEN"))), "false"},

		// Enum ARGUMENT vs member token, both operand orders.
		{"arg_eq_member", `{{ color == Color.RED }}`,
			[]promptdescriptor.Argument{colorArg("color")},
			vals(argV("color", enumV("Color", "RED"))), "true"},
		{"member_eq_arg", `{{ Color.RED == color }}`,
			[]promptdescriptor.Argument{colorArg("color")},
			vals(argV("color", enumV("Color", "BLUE"))), "false"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fn := enumPredicateFn(c.expr, c.args...)
			if got := renderOnlyMessage(t, fn, c.values); got != c.want {
				t.Errorf("%s => %q, want %q (stock BAML v0.223)", c.expr, got, c.want)
			}
		})
	}
}

// TestAdmittedHostValueRenders proves the BINDING half: a bound enum, class and
// list render BAML's host-value spelling — the display ALIAS for an enum, the
// alternate debug-map keyed by ALIASES for a class, and the compact debug-list
// for a list, with source field order and input item order preserved.
//
// This is what separates 7.1b from "the engine happens to match": these bytes
// exist only because the descriptor carried resolved aliases/order and the
// binder built real bamlprofile host values from them.
func TestAdmittedHostValueRenders(t *testing.T) {
	t.Run("enum_argument_renders_its_display_alias", func(t *testing.T) {
		fn := enumPredicateFn(`{{ color }}`, colorArg("color"))
		if got := renderOnlyMessage(t, fn, vals(argV("color", enumV("Color", "RED")))); got != "rouge" {
			t.Errorf("bound Color.RED rendered %q, want its display alias %q", got, "rouge")
		}
		// An UNALIASED member displays its canonical name.
		if got := renderOnlyMessage(t, fn, vals(argV("color", enumV("Color", "BLUE")))); got != "BLUE" {
			t.Errorf("bound Color.BLUE rendered %q, want the canonical name", got)
		}
	})

	t.Run("list_argument_renders_the_compact_debug_list", func(t *testing.T) {
		fn := enumPredicateFn(`{{ colors }}`, v3Arg("colors", promptdescriptor.ResolvedValueType{
			Kind: promptdescriptor.ValueList,
			Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
		}))
		got := renderOnlyMessage(t, fn, vals(argV("colors",
			listV(enumV("Color", "BLUE"), enumV("Color", "RED")))))
		// INPUT order, not canonical order; each item displays its alias.
		if got != "[BLUE, rouge]" {
			t.Errorf("bound Color[] rendered %q, want %q", got, "[BLUE, rouge]")
		}
	})

	t.Run("class_argument_renders_alias_keys_in_source_order", func(t *testing.T) {
		fn := swatchFn(`{{ _.role("user") }}` + "\n" + `{{ s }}`)
		got := renderOnlyMessage(t, fn, vals(argV("s", classV("Swatch",
			fieldV("color", enumV("Color", "GREEN")),
			fieldV("label", strV("hi"))))))
		// Rust's alternate debug-map: the DISPLAY keys (alias where present) in
		// SOURCE field order, the enum as its alias, the string Debug-quoted.
		want := "{\n    \"color\": vert,\n    \"etiquette\": \"hi\",\n}"
		if got != want {
			t.Errorf("bound Swatch rendered\n%q\nwant\n%q", got, want)
		}
	})
}

// TestProjectEnumGlobalsAreInstalledWholesale pins the render-context half of
// the stock model: EVERY project enum becomes a namespace global, so a
// no-argument function can compare `Color.RED` even though nothing binds a
// Color. A subset would be a render context BAML never has.
func TestProjectEnumGlobalsAreInstalledWholesale(t *testing.T) {
	fn := enumPredicateFn(`{{ Color.RED == 'RED' }}`)
	if len(fn.Args) != 0 {
		t.Fatal("this proof requires a NO-ARGUMENT function")
	}
	if got := renderOnlyMessage(t, fn, noVals()); got != "true" {
		t.Errorf("a literal-only enum comparison rendered %q, want %q", got, "true")
	}

	// And the negative: a descriptor with an EMPTY universe installs no global,
	// so the same template declines rather than rendering `undefined`.
	bare := staticFn(`{{ _.role("user") }}` + "\n" + `{{ Color.RED == 'RED' }}`)
	assertStaticDecline(t, bare, noVals(), FeatureEnumComparison)
}

// ---------------------------------------------------------------------------
// Deliberate declines
// ---------------------------------------------------------------------------

// TestDeliberateEnumDeclines is the parity fence. Every row is a near neighbour
// of an admitted form, and every one of them stays on BAML.
//
// The alias row is the headline: stock BAML answers `false` for
// `Color.RED == 'rouge'`, and 7.1b still declines it. That is not an engine
// divergence — it is the refusal to make a DISPLAY string a second identity.
func TestDeliberateEnumDeclines(t *testing.T) {
	cases := []struct {
		name string
		expr string
		args []promptdescriptor.Argument
		want string
	}{
		// The parity control: a display alias is not an identity.
		{"member_eq_display_alias", `{{ Color.RED == 'rouge' }}`, nil, FeatureEnumComparison},
		{"display_alias_eq_member", `{{ 'rouge' == Color.RED }}`, nil, FeatureEnumComparison},
		{"arg_eq_display_alias", `{{ color == 'rouge' }}`, []promptdescriptor.Argument{colorArg("color")}, FeatureEnumComparison},

		// Operators outside the claimed grammar.
		{"inequality", `{{ Color.RED != 'RED' }}`, nil, FeatureEnumComparison},
		{"inequality_members", `{{ Color.RED != Color.BLUE }}`, nil, FeatureEnumComparison},
		{"ordering_lt", `{{ Color.BLUE < Color.RED }}`, nil, FeatureEnumComparison},
		{"ordering_ge", `{{ Color.BLUE >= Color.RED }}`, nil, FeatureEnumComparison},

		// Unknown / cross-namespace operands.
		{"unknown_member", `{{ Color.NOPE == 'NOPE' }}`, nil, FeatureEnumComparison},
		{"unknown_canonical_string", `{{ Color.RED == 'CRIMSON' }}`, nil, FeatureEnumComparison},
		{"unknown_namespace", `{{ Shade.RED == 'RED' }}`, nil, FeatureEnumComparison},
		{"numeric_string", `{{ Color.RED == '1' }}`, nil, FeatureEnumComparison},
		{"bool_literal", `{{ Color.RED == true }}`, nil, FeatureEnumComparison},
		{"none_literal", `{{ Color.RED == none }}`, nil, FeatureEnumComparison},

		// Two bare enum VARIABLES: the stock fixture proves the member forms, not
		// variable-vs-variable.
		{"arg_eq_arg", `{{ a == b }}`,
			[]promptdescriptor.Argument{colorArg("a"), colorArg("b")}, FeatureEnumComparison},

		// Membership neighbours: multi-element, dynamic, or non-literal lists.
		{"multi_element_list", `{{ 'RED' in [Color.RED, Color.BLUE] }}`, nil, FeatureEnumComparison},
		{"empty_list", `{{ 'RED' in [] }}`, nil, FeatureEnumComparison},
		{"arg_in_list_variable", `{{ color in colors }}`,
			[]promptdescriptor.Argument{colorArg("color"), v3Arg("colors", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueList,
				Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			})}, FeatureEnumComparison},
		{"alias_in_member_list", `{{ 'rouge' in [Color.RED] }}`, nil, FeatureEnumComparison},
		{"member_in_alias_list", `{{ Color.RED in ['rouge'] }}`, nil, FeatureEnumComparison},
		{"arg_in_member_list", `{{ color in [Color.RED] }}`,
			[]promptdescriptor.Argument{colorArg("color")}, FeatureEnumComparison},
		{"not_in", `{{ 'RED' not in [Color.RED] }}`, nil, FeatureEnumComparison},

		// Attribute / index / call neighbours of the direct-render row.
		{"member_bare_render", `{{ Color.RED }}`, nil, FeatureEnumClassValue},
		{"arg_value_attribute", `{{ color.value }}`, []promptdescriptor.Argument{colorArg("color")}, FeatureEnumClassValue},
		{"member_value_attribute", `{{ Color.RED.value }}`, nil, FeatureEnumClassValue},
		{"class_field_attribute", `{{ s.color }}`, nil, FeatureEnumClassValue},

		// Filters and methods stay unconditionally fenced.
		{"filter_on_member", `{{ Color.RED|string }}`, nil, FeatureUnknownFilter},
		{"filter_join_on_list", `{{ colors|join(',') }}`,
			[]promptdescriptor.Argument{v3Arg("colors", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueList,
				Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			})}, FeatureUnknownFilter},

		// Glue: the accepted spellings are exact, so a whitespace-broken namespace
		// access is not one of them.
		{"unglued_namespace", `{{ Color . RED == 'RED' }}`, nil, FeatureEnumComparison},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fn := enumPredicateFn(c.expr, c.args...)
			values := make([]promptdescriptor.ArgumentValue, 0, len(c.args))
			for _, a := range c.args {
				values = append(values, argV(a.Name, declineArgValue(a)))
			}
			if len(values) == 0 {
				values = nil
			}
			assertStaticDecline(t, fn, values, c.want)
		})
	}
}

// declineArgValue builds a WELL-FORMED projected value for a declared argument,
// so a decline row can never pass because the value was malformed — the
// template shape is what must decline.
func declineArgValue(a promptdescriptor.Argument) promptdescriptor.StaticValue {
	switch a.ValueType.Kind {
	case promptdescriptor.ValueEnum:
		return enumV(a.ValueType.EnumName, "RED")
	case promptdescriptor.ValueList:
		return listV(enumV("Color", "RED"))
	default:
		return strV("x")
	}
}

// TestClassAndListNeighbourDeclines pins the class/list rows of the admission
// table: the BARE render is admitted, every traversal of it is not.
func TestClassAndListNeighbourDeclines(t *testing.T) {
	listArg := v3Arg("colors", promptdescriptor.ResolvedValueType{
		Kind: promptdescriptor.ValueList,
		Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
	})

	t.Run("bare_class_render_is_admitted", func(t *testing.T) {
		fn := swatchFn(`{{ _.role("user") }}` + "\n" + `{{ s }}`)
		if err := SupportsStatic(fn, vals(argV("s", classV("Swatch",
			fieldV("color", enumV("Color", "RED")), fieldV("label", strV("l")))))); err != nil {
			t.Fatalf("a bare bound class render must be admitted: %v", err)
		}
	})
	t.Run("class_attribute_declines", func(t *testing.T) {
		fn := swatchFn(`{{ _.role("user") }}` + "\n" + `{{ s.color }}`)
		assertStaticDecline(t, fn, vals(argV("s", classV("Swatch",
			fieldV("color", enumV("Color", "RED")), fieldV("label", strV("l"))))), FeatureEnumClassValue)
	})
	t.Run("class_alias_attribute_declines", func(t *testing.T) {
		fn := swatchFn(`{{ _.role("user") }}` + "\n" + `{{ s.etiquette }}`)
		assertStaticDecline(t, fn, vals(argV("s", classV("Swatch",
			fieldV("color", enumV("Color", "RED")), fieldV("label", strV("l"))))), FeatureEnumClassValue)
	})
	t.Run("bare_list_render_is_admitted", func(t *testing.T) {
		fn := enumPredicateFn(`{{ colors }}`, listArg)
		if err := SupportsStatic(fn, vals(argV("colors", listV(enumV("Color", "RED"))))); err != nil {
			t.Fatalf("a bare bound list render must be admitted: %v", err)
		}
	})
	t.Run("list_index_declines", func(t *testing.T) {
		fn := enumPredicateFn(`{{ colors[0] }}`, listArg)
		assertStaticDecline(t, fn, vals(argV("colors", listV(enumV("Color", "RED")))), FeatureUnrecognizedPrompt)
	})
}

// TestNestedListDescriptorsDeclineAtTheBinder is the binder-side half of the
// nested-list fence. The SOURCE resolver already declines every spelling that
// produces one — including the alias-hidden `type L = string[]; F(x: L[])` form,
// covered in internal/nativeschema/inputvalues_test.go — so these descriptors
// cannot come from .baml. They are hand-built on purpose: the binder must fail
// closed on a malformed or future descriptor without trusting the producer.
//
// The last row is the reason the rejection is a REJECTION and not a recursion:
// a value type whose Elem points back at itself is a list-of-list by
// construction, so refusing nested lists is also what bounds the edge walk. A
// binder that recursed instead would overflow the stack on it.
func TestNestedListDescriptorsDeclineAtTheBinder(t *testing.T) {
	listOf := func(elem *promptdescriptor.ResolvedValueType) *promptdescriptor.ResolvedValueType {
		return &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList, Elem: elem}
	}

	t.Run("nested_list_argument", func(t *testing.T) {
		fn := staticFn(`{{ _.role("user") }}`+"\n"+`{{ grid }}`,
			v3Arg("grid", *listOf(listOf(&promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}))))
		assertStaticDecline(t, fn, vals(argV("grid", listV(listV(strV("a"))))), FeatureStaticArgType)
	})

	t.Run("nested_list_class_field", func(t *testing.T) {
		fn := staticFn(`{{ _.role("user") }}`+"\n"+`text`,
			v3Arg("h", promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: "Holder"}))
		fn.InputValues = promptdescriptor.InputValueUniverse{
			Classes: []promptdescriptor.ResolvedClass{{
				Name: "Holder",
				Fields: []promptdescriptor.ResolvedClassField{
					{Canonical: "rows", Type: *listOf(listOf(&promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}))},
				},
			}},
		}
		// A malformed UNIVERSE is a descriptor contract violation, caught before a
		// template or a value is considered.
		assertStaticDecline(t, fn, vals(argV("h", classV("Holder", fieldV("rows", listV(listV(strV("a"))))))), FeatureStaticDescriptor)
	})

	t.Run("self_referential_element_does_not_recurse", func(t *testing.T) {
		// t.Elem == t: a cyclic value-type graph. It must DECLINE (as a nested
		// list), not walk forever. Reaching the assertion at all is the proof.
		cyclic := &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList}
		cyclic.Elem = cyclic
		fn := staticFn(`{{ _.role("user") }}`+"\n"+`{{ loop }}`, v3Arg("loop", *cyclic))
		assertStaticDecline(t, fn, vals(argV("loop", listV())), FeatureStaticArgType)
	})
}

// TestDynamicAdmissionCannotReachEnumGlobals pins the other half of the fence:
// the dynamic lane admits exactly one template (the generated Baml_Rest_Dynamic
// prompt), which references no enum, and production builds its render context
// with an EMPTY enum set — so no enum namespace global exists on the dynamic
// path at all, whatever a template tried to say.
func TestDynamicAdmissionCannotReachEnumGlobals(t *testing.T) {
	if err := Supports(`{{ _.role("user") }}{{ Color.RED == 'RED' }}`, nil); err == nil {
		t.Fatal("an enum-comparison template must not be admitted by Supports")
	}
	assertUnsupported(t, Supports(`{{ Color.RED }}`, nil))

	// The dynamic constructor installs no enum namespace, so `Color` is undefined
	// rather than a member namespace.
	if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ Color is undefined }}`); got != "true" {
		t.Errorf("dynamic render context defines Color = %q, want it undefined", got)
	}
}

// TestEnumOrderingThroughProfileSeam keeps the LEAF-level behaviour the profile
// owns — display is the alias, ordering is by canonical name — asserted against
// the production seam. It is explicitly NOT an admission claim: every expression
// here is declined by the static gate (TestDeliberateEnumDeclines), and the rows
// exist so a bamlprofile regression is caught where the fence would otherwise
// hide it.
func TestEnumOrderingThroughProfileSeam(t *testing.T) {
	cfg := bamlprofile.Config{Enums: []bamlprofile.EnumDef{{
		Name: "Color",
		Values: []bamlprofile.EnumValue{
			{Canonical: "RED", Alias: aliasOf("rouge")},
			{Canonical: "GREEN", Alias: aliasOf("vert")},
			{Canonical: "BLUE", Alias: aliasOf("bleu")},
		},
	}}}
	cases := []struct {
		expr string
		want string
	}{
		{`{{ Color.RED }}`, "rouge"},
		{`{{ Color.BLUE < Color.RED }}`, "true"},
		{`{{ Color.RED < Color.BLUE }}`, "false"},
		{`{% for c in [Color.RED, Color.GREEN, Color.BLUE]|sort %}{{ c }},{% endfor %}`, "bleu,vert,rouge,"},
		{`{{ [Color.RED, Color.GREEN, Color.BLUE]|min }}`, "bleu"},
		{`{{ [Color.RED, Color.GREEN, Color.BLUE]|max }}`, "rouge"},
	}
	for _, tc := range cases {
		if got := mustRenderThroughSeam(t, cfg, tc.expr); got != tc.want {
			t.Errorf("%s => %q, want %q", tc.expr, got, tc.want)
		}
	}
}

func aliasOf(s string) *string { return &s }

// leakCanary is the string payload every no-leak row below carries. It is
// deliberately distinctive so a substring search cannot match incidentally.
const leakCanary = "super-secret-payload"

// TestDeclineDetailsCarryNoValues is a SECURITY-shaped guard: a binder decline
// names the argument/field PATH and the KINDS involved, never the value itself,
// so a decline reason can be logged as a bounded token without leaking request
// data.
//
// The rows deliberately span EARLY and LATE declines. An early kind mismatch may
// be rejected before the binder formats anything, so on its own it would prove
// only that one path is safe. The later rows carry a payload through a value
// whose KIND MATCHES its declared type — so binding proceeds, walks into the
// class/list, and formats a message about something else — which is where an
// interpolated value would actually show up.
func TestDeclineDetailsCarryNoValues(t *testing.T) {
	// The unknown MEMBER name is metadata the projector read from a generated
	// enum constant, so naming it is intentional and safe. Pin that the message
	// really does say so, rather than asserting it in a comment only.
	fn := colorFn("{{ c }}", colorArg("c"))
	err := SupportsStatic(fn, vals(argV("c", enumV("Color", "NOPE"))))
	if err == nil {
		t.Fatal("expected a decline for an unknown enum member")
	}
	for _, want := range []string{"NOPE", "Color", "not a canonical member"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-member decline %q should name %q (member/enum identity is intentional metadata)", err, want)
		}
	}

	// paletteClass is a two-field class whose SECOND field is a string, so a row
	// can put the canary in a well-typed payload position and still decline for a
	// different reason further along the walk.
	paletteFn := func(prompt string) promptdescriptor.Function {
		f := staticFn(prompt, v3Arg("s", promptdescriptor.ResolvedValueType{
			Kind: promptdescriptor.ValueClass, ClassName: "Swatch",
		}))
		f.InputValues = promptdescriptor.InputValueUniverse{
			ProjectEnums: []promptdescriptor.ResolvedEnum{testColorEnum()},
			Classes:      []promptdescriptor.ResolvedClass{testSwatchClass()},
		}
		return f
	}

	rows := []struct {
		name   string
		fn     promptdescriptor.Function
		values []promptdescriptor.ArgumentValue
	}{
		{
			// EARLY: a top-level kind mismatch. The binder may reject this before
			// formatting anything, which is exactly why it cannot stand alone.
			name:   "early_top_level_kind_mismatch",
			fn:     staticFn("{{ v }}", primArg("v", "int")),
			values: vals(argV("v", strV(leakCanary))),
		},
		{
			// LATE: the argument's kind MATCHES (a class), so binding walks in. The
			// canary sits in a correctly-typed string field while a DIFFERENT field
			// is misnamed, so the decline is formatted after the payload was seen.
			name: "late_class_field_name_mismatch",
			fn:   paletteFn("{{ _.role(\"user\") }}\ntext"),
			values: vals(argV("s", classV("Swatch",
				fieldV("colour", enumV("Color", "RED")), // misnamed: 'color' is declared
				fieldV("label", strV(leakCanary))))),
		},
		{
			// LATE: the argument's kind MATCHES (a class) and every field name is
			// right; the canary rides in the string field while the OTHER field's
			// kind is wrong, so the decline happens one level down.
			name: "late_nested_field_kind_mismatch",
			fn:   paletteFn("{{ _.role(\"user\") }}\ntext"),
			values: vals(argV("s", classV("Swatch",
				fieldV("color", strV("not-an-enum")),
				fieldV("label", strV(leakCanary))))),
		},
		{
			// LATE: the argument's kind MATCHES (a list) and the first element binds
			// cleanly; the canary is the element that fails validation.
			name: "late_list_element_failure",
			fn: enumPredicateFn("{{ colors }}", v3Arg("colors", promptdescriptor.ResolvedValueType{
				Kind: promptdescriptor.ValueList,
				Elem: &promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: "Color"},
			})),
			values: vals(argV("colors", listV(enumV("Color", "RED"), strV(leakCanary)))),
		},
	}

	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			serr := SupportsStatic(r.fn, r.values)
			if serr == nil {
				t.Fatal("expected a decline; a row that ADMITS proves nothing about leaking")
			}
			if strings.Contains(serr.Error(), leakCanary) {
				t.Errorf("decline detail leaked an argument value: %v", serr)
			}
		})
	}
}
