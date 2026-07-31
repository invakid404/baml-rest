package bamlprofile

import (
	"errors"
	"strings"
	"testing"

	minijinja "github.com/invakid404/minijinja-go/v2"
	"github.com/invakid404/minijinja-go/v2/value"
)

// These are pure-Go GUARDRAIL tests for the enum/class/list host value model.
// They pin the obvious observable behavior for fast feedback; the byte-exact
// AUTHORITY is the stock-BAML-v0.223 differential in ./profileoracle
// (integration tag). The goldens here are hand-derived from BAML's Rust source
// and its own test expectations, then confirmed by the differential.

func strptr(s string) *string { return &s }

// colorEnum is the discriminating enum used across these tests: RED has an alias
// that DIFFERS from its canonical name, so alias-vs-canonical confusions surface.
func colorEnum() EnumDef {
	return EnumDef{Name: "Color", Values: []EnumValue{
		{Canonical: "RED", Alias: strptr("rouge")},
		{Canonical: "GREEN"},
		{Canonical: "BLUE"},
	}}
}

func renderHost(t *testing.T, cfg Config, src string, ctx map[string]any) (string, error) {
	t.Helper()
	env, err := New(cfg)
	if err != nil {
		return "", err
	}
	tmpl, err := env.TemplateFromNamedString("host_test", src)
	if err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = map[string]any{}
	}
	return tmpl.Render(ctx)
}

func mustRender(t *testing.T, cfg Config, src string, ctx map[string]any) string {
	t.Helper()
	out, err := renderHost(t, cfg, src, ctx)
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return out
}

// TestEnumPresentation pins display (alias-or-canonical), .value (canonical
// only), and that .name/.alias are absent.
func TestEnumPresentation(t *testing.T) {
	cfg := Config{Enums: []EnumDef{colorEnum()}}
	cases := []struct{ name, src, want string }{
		{"display_alias", `{{ Color.RED }}`, "rouge"},
		{"display_noalias", `{{ Color.GREEN }}`, "GREEN"},
		{"value_alias", `{{ Color.RED.value }}`, "RED"},
		{"value_noalias", `{{ Color.GREEN.value }}`, "GREEN"},
		{"name_absent", `{{ Color.RED.name is undefined }}`, "true"},
		{"alias_absent", `{{ Color.RED.alias is undefined }}`, "true"},
		{"value_present", `{{ Color.RED.value is undefined }}`, "false"},
		{"unknown_member", `{{ Color.PURPLE is undefined }}`, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRender(t, cfg, tc.src, nil); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestEnumValueCmp597 pins the six #597 cases in both operand orders plus
// membership, and the decline edges — the profile-level closure of the fence.
func TestEnumValueCmp597(t *testing.T) {
	// A second enum whose RED has the SAME canonical+alias as Color.RED, and a
	// third whose RED has NO alias, to probe enum_name omission and the
	// Some/None alias tie-break.
	shade := EnumDef{Name: "Shade", Values: []EnumValue{{Canonical: "RED", Alias: strptr("rouge")}}}
	size := EnumDef{Name: "Size", Values: []EnumValue{{Canonical: "RED"}, {Canonical: "SMALL", Alias: strptr("petit")}}}
	cfg := Config{Enums: []EnumDef{colorEnum(), shade, size}}
	cases := []struct{ name, src, want string }{
		// the six
		{"canon_eq_fwd", `{{ Color.RED == 'RED' }}`, "true"},
		{"canon_eq_rev", `{{ 'RED' == Color.RED }}`, "true"},
		{"same_member", `{{ Color.RED == Color.RED }}`, "true"},
		{"in_list", `{{ 'RED' in [Color.RED] }}`, "true"},
		{"alias_str_false", `{{ Color.RED == 'rouge' }}`, "false"},
		{"diff_member", `{{ Color.RED == Color.BLUE }}`, "false"},
		// extras
		{"member_in_strlist", `{{ Color.RED in ['RED'] }}`, "true"},
		{"alias_str_rev", `{{ 'rouge' == Color.RED }}`, "false"},
		{"ne_member", `{{ Color.RED != Color.BLUE }}`, "true"},
		{"ne_same", `{{ Color.RED != Color.RED }}`, "false"},
		// cross-enum: enum_name omitted (same canonical+alias -> equal); alias IS
		// part of identity (same canonical, Some vs None -> not equal).
		{"cross_enum_eq", `{{ Color.RED == Shade.RED }}`, "true"},
		{"cross_enum_in_list", `{{ Shade.RED in [Color.RED] }}`, "true"},
		{"same_canon_diff_alias", `{{ Color.RED == Size.RED }}`, "false"},
		// ordering
		{"order_str_lt", `{{ Color.GREEN < 'RED' }}`, "true"},
		{"order_str_gt", `{{ Color.RED < 'GREEN' }}`, "false"},
		{"order_member_none_lt_some", `{{ Size.RED < Color.RED }}`, "true"},
		{"order_member_some_gt_none", `{{ Color.RED < Size.RED }}`, "false"},
		// decline non-string primitives (never invent equality)
		{"decline_int_eq", `{{ Color.RED == 5 }}`, "false"},
		{"decline_int_ne", `{{ Color.RED != 5 }}`, "true"},
		{"decline_bool", `{{ Color.RED == true }}`, "false"},
		{"decline_none_eq", `{{ Color.RED == none }}`, "false"},
		{"decline_none_ne", `{{ Color.RED != none }}`, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRender(t, cfg, tc.src, nil); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestClassAccessAndRender pins canonical access / alias-undefined / map
// behavior and the exact {map:#?} direct-render bytes (single field, nested
// class+list, nested none, and scalar escaping). The nested golden is BAML's own
// render_nested_class expectation (lib.rs:1888).
func TestClassAccessAndRender(t *testing.T) {
	single, err := ClassValue([]ClassField{{Canonical: "prop1", Alias: strptr("key1"), Value: value.FromString("value")}})
	if err != nil {
		t.Fatal(err)
	}

	bList, err := ListValue([]value.Value{value.FromString("item1"), value.FromString("item2")})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := ClassValue([]ClassField{
		{Canonical: "b_prop1", Alias: strptr("alias_b_prop1"), Value: value.FromString("value_b")},
		{Canonical: "b_prop2", Value: bList},
	})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := ClassValue([]ClassField{
		{Canonical: "a_prop1", Alias: strptr("alias_a_prop1"), Value: value.FromString("value_a")},
		{Canonical: "a_prop2", Value: inner},
	})
	if err != nil {
		t.Fatal(err)
	}

	noneField, err := ClassValue([]ClassField{
		{Canonical: "maybe", Alias: strptr("perhaps"), Value: value.None()},
		{Canonical: "always", Value: value.FromString("x")},
	})
	if err != nil {
		t.Fatal(err)
	}

	esc, err := ClassValue([]ClassField{{Canonical: "raw", Alias: strptr("escaped"), Value: value.FromString("a\"b\tc\nd")}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, src, want string }{
		{"canonical_access", `{{ c.prop1 }}`, "value"},
		{"alias_undefined", `{{ c.key1 is undefined }}`, "true"},
		{"canonical_defined", `{{ c.prop1 is undefined }}`, "false"},
		{"missing_undefined", `{{ c.nope is undefined }}`, "true"},
		{"truthy", `{% if c %}yes{% else %}no{% endif %}`, "yes"},
		{"render_single", `{{ c }}`, "{\n    \"key1\": \"value\",\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRender(t, Config{}, tc.src, map[string]any{"c": single}); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}

	t.Run("iter_and_length", func(t *testing.T) {
		if got := mustRender(t, Config{}, `{% for k in c %}{{ k }},{% endfor %}/{{ c|length }}`, map[string]any{"c": nested}); got != "a_prop1,a_prop2,/2" {
			t.Errorf("iter/length = %q", got)
		}
	})
	t.Run("render_nested", func(t *testing.T) {
		want := "{\n    \"alias_a_prop1\": \"value_a\",\n    \"a_prop2\": {\n        \"alias_b_prop1\": \"value_b\",\n        \"b_prop2\": [\n            \"item1\",\n            \"item2\",\n        ],\n    },\n}"
		if got := mustRender(t, Config{}, `{{ c }}`, map[string]any{"c": nested}); got != want {
			t.Errorf("render_nested =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("render_nested_none", func(t *testing.T) {
		want := "{\n    \"perhaps\": null,\n    \"always\": \"x\",\n}"
		if got := mustRender(t, Config{}, `{{ c }}`, map[string]any{"c": noneField}); got != want {
			t.Errorf("render_nested_none = %q, want %q", got, want)
		}
	})
	t.Run("render_escaping", func(t *testing.T) {
		want := "{\n    \"escaped\": \"a\\\"b\\tc\\nd\",\n}"
		if got := mustRender(t, Config{}, `{{ c }}`, map[string]any{"c": esc}); got != want {
			t.Errorf("render_escaping = %q, want %q", got, want)
		}
	})
	// The leaf preserves the EXACT insertion order it is given — it does not sort
	// fields. This is the multi-field ordering the CFFI differential cannot pin
	// (a class ARGUMENT's field order is the Go client's random map order there),
	// so it is proven here: reversed-alphabetical fields render reversed, and the
	// render_nested golden above is BAML's own multi-field expectation
	// (jinja-runtime/src/lib.rs:1888) reproduced byte-for-byte.
	t.Run("insertion_order_preserved", func(t *testing.T) {
		c, err := ClassValue([]ClassField{
			{Canonical: "zebra", Value: value.FromString("z")},
			{Canonical: "apple", Value: value.FromString("a")},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantRender := "{\n    \"zebra\": \"z\",\n    \"apple\": \"a\",\n}"
		if got := mustRender(t, Config{}, `{{ c }}`, map[string]any{"c": c}); got != wantRender {
			t.Errorf("render = %q, want %q (must NOT be alphabetized)", got, wantRender)
		}
		if got := mustRender(t, Config{}, `{% for k in c %}{{ k }},{% endfor %}`, map[string]any{"c": c}); got != "zebra,apple," {
			t.Errorf("iteration = %q, want insertion order zebra,apple,", got)
		}
	})

	t.Run("empty_class_falsey", func(t *testing.T) {
		empty, err := ClassValue(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := mustRender(t, Config{}, `{% if c %}yes{% else %}no{% endif %}/{{ c|length }}/{{ c }}`, map[string]any{"c": empty}); got != "no/0/{}" {
			t.Errorf("empty class = %q, want %q", got, "no/0/{}")
		}
	})
}

// TestListRenderAndBehavior pins host-list rendering (pretty debug-list with
// BARE enum aliases and nested none -> null) plus indexing/length/membership.
func TestListRenderAndBehavior(t *testing.T) {
	cfg := Config{Enums: []EnumDef{colorEnum()}}
	red, err := EnumMember("Color", "RED", strptr("rouge"))
	if err != nil {
		t.Fatal(err)
	}
	green, err := EnumMember("Color", "GREEN", nil)
	if err != nil {
		t.Fatal(err)
	}
	enumList, err := ListValue([]value.Value{red, green})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("render_enum_list", func(t *testing.T) {
		// A directly-rendered host list is COMPACT (non-alternate debug-list);
		// enum members render as their BARE aliases.
		want := "[rouge, GREEN]"
		if got := mustRender(t, cfg, `{{ xs }}`, map[string]any{"xs": enumList}); got != want {
			t.Errorf("render_enum_list = %q, want %q", got, want)
		}
	})
	t.Run("index_and_cmp", func(t *testing.T) {
		if got := mustRender(t, cfg, `{{ xs[0] == 'RED' }}/{{ xs[0] }}/{{ xs[1] }}/{{ xs|length }}`, map[string]any{"xs": enumList}); got != "true/rouge/GREEN/2" {
			t.Errorf("index_and_cmp = %q", got)
		}
	})
	t.Run("membership", func(t *testing.T) {
		if got := mustRender(t, cfg, `{{ 'RED' in xs }}/{{ 'BLUE' in xs }}`, map[string]any{"xs": enumList}); got != "true/false" {
			t.Errorf("membership = %q", got)
		}
	})
	t.Run("render_none_item", func(t *testing.T) {
		l, err := ListValue([]value.Value{value.FromString("a"), value.None()})
		if err != nil {
			t.Fatal(err)
		}
		want := `["a", null]`
		if got := mustRender(t, Config{}, `{{ xs }}`, map[string]any{"xs": l}); got != want {
			t.Errorf("render_none_item = %q, want %q", got, want)
		}
	})
	t.Run("render_empty", func(t *testing.T) {
		empty, err := ListValue(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := mustRender(t, Config{}, `{{ xs }}`, map[string]any{"xs": empty}); got != "[]" {
			t.Errorf("render_empty = %q, want []", got)
		}
	})
}

// TestHostConstructionFailsLoud pins that malformed enum/class metadata is
// rejected at construction (a DECLINE/fail-loud), never silently accepted into a
// value that would render or compare wrong.
func TestHostConstructionFailsLoud(t *testing.T) {
	t.Run("new_empty_enum_name", func(t *testing.T) {
		if _, err := New(Config{Enums: []EnumDef{{Name: "", Values: []EnumValue{{Canonical: "X"}}}}}); err == nil {
			t.Error("New accepted an enum with an empty name")
		}
	})
	t.Run("new_empty_canonical", func(t *testing.T) {
		if _, err := New(Config{Enums: []EnumDef{{Name: "E", Values: []EnumValue{{Canonical: ""}}}}}); err == nil {
			t.Error("New accepted a variant with an empty canonical name")
		}
	})
	t.Run("new_dup_variant", func(t *testing.T) {
		if _, err := New(Config{Enums: []EnumDef{{Name: "E", Values: []EnumValue{{Canonical: "X"}, {Canonical: "X"}}}}}); err == nil {
			t.Error("New accepted a duplicate variant")
		}
	})
	t.Run("enum_member_empty_canonical", func(t *testing.T) {
		if _, err := EnumMember("E", "", nil); err == nil {
			t.Error("EnumMember accepted an empty canonical name")
		}
	})
	t.Run("enum_member_empty_enum_name", func(t *testing.T) {
		if _, err := EnumMember("", "X", nil); err == nil {
			t.Error("EnumMember accepted an empty enum name")
		}
	})
	t.Run("class_empty_field", func(t *testing.T) {
		if _, err := ClassValue([]ClassField{{Canonical: "", Value: value.FromString("x")}}); err == nil {
			t.Error("ClassValue accepted an empty field name")
		}
	})
	t.Run("class_dup_field", func(t *testing.T) {
		if _, err := ClassValue([]ClassField{
			{Canonical: "a", Value: value.FromString("x")},
			{Canonical: "a", Value: value.FromString("y")},
		}); err == nil {
			t.Error("ClassValue accepted a duplicate field name")
		}
	})
	t.Run("class_rejects_native_container", func(t *testing.T) {
		// A native fork slice as a field value would render compactly via Repr, not
		// as BAML's pretty host list — reject it rather than silently diverge.
		native := value.FromSlice([]value.Value{value.FromString("x")})
		if _, err := ClassValue([]ClassField{{Canonical: "a", Value: native}}); err == nil {
			t.Error("ClassValue accepted a native fork slice as a field value")
		}
	})
	t.Run("list_rejects_native_container", func(t *testing.T) {
		native := value.FromSlice([]value.Value{value.FromString("x")})
		if _, err := ListValue([]value.Value{native}); err == nil {
			t.Error("ListValue accepted a native fork slice as an item")
		}
	})
	// Environment.AddGlobal is a plain map assignment, so without a preflight the
	// SECOND definition would silently replace the first and `Duplicate.A` would
	// become undefined while the Config still looked accepted. This is the exact
	// construction from the PR-2 review.
	// "ctx" and "_" are installed by New itself, BEFORE the enum namespaces, so an
	// EnumDef with either name would win the AddGlobal race and silently blank out
	// ctx.output_format. Stock BAML v0.223 rejects `enum ctx {}` / `enum _ {}` at
	// CreateRuntime, so such a definition cannot come from real resolved metadata
	// — declining it is the conservative match.
	t.Run("new_enum_named_like_a_reserved_global", func(t *testing.T) {
		for _, name := range []string{"ctx", "_"} {
			cfg := Config{OutputFormat: "SCHEMA", Enums: []EnumDef{{Name: name, Values: []EnumValue{{Canonical: "A"}}}}}
			if _, err := New(cfg); err == nil {
				t.Errorf("New accepted an enum named %q, which would clobber the %q global", name, name)
			}
		}
	})
	// The alias pointer must be OWNED, like ListValue's items: writing through the
	// caller's *string afterwards must not change the member's display or its
	// Option<alias> comparison identity.
	t.Run("enum_member_owns_its_alias", func(t *testing.T) {
		alias := "rouge"
		m, err := EnumMember("Color", "RED", &alias)
		if err != nil {
			t.Fatal(err)
		}
		alias = "vert"
		if got := mustRender(t, Config{}, `{{ m }}`, map[string]any{"m": m}); got != "rouge" {
			t.Errorf("member display after caller mutation = %q, want rouge", got)
		}
	})
	t.Run("new_dup_enum_definition", func(t *testing.T) {
		_, err := New(Config{Enums: []EnumDef{
			{Name: "Duplicate", Values: []EnumValue{{Canonical: "A"}}},
			{Name: "Duplicate", Values: []EnumValue{{Canonical: "B"}}},
		}})
		if err == nil {
			t.Fatal("New accepted two EnumDefs with the same Name")
		}
		if !strings.Contains(err.Error(), "duplicate enum definition") {
			t.Errorf("duplicate-enum error = %q, want a 'duplicate enum definition' message", err.Error())
		}
	})
	// ListValue validates every item, then stores an OWNED copy. Without the copy
	// a caller could swap in a native container after construction and turn the
	// debug walker's "unreachable" panic into a reachable one — the review's
	// disproof of the construction-time invariant.
	t.Run("list_ingress_copy_defeats_post_construction_mutation", func(t *testing.T) {
		items := []value.Value{value.FromString("safe")}
		xs, err := ListValue(items)
		if err != nil {
			t.Fatal(err)
		}
		items[0] = value.FromSlice([]value.Value{value.FromString("mutated")})
		if got := mustRender(t, Config{}, `{{ xs }}`, map[string]any{"xs": xs}); got != `["safe"]` {
			t.Errorf("render after caller mutation = %q, want [\"safe\"] (ListValue must own its items)", got)
		}
	})
	// The same ingress contract for a list nested in a class: the class stores the
	// already-owned host list value, so the caller's slice is equally inert.
	t.Run("class_field_list_unaffected_by_caller_mutation", func(t *testing.T) {
		items := []value.Value{value.FromString("safe")}
		xs, err := ListValue(items)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ClassValue([]ClassField{{Canonical: "items", Alias: strptr("data"), Value: xs}})
		if err != nil {
			t.Fatal(err)
		}
		items[0] = value.FromSlice([]value.Value{value.FromString("mutated")})
		want := "{\n    \"data\": [\n        \"safe\",\n    ],\n}"
		if got := mustRender(t, Config{}, `{{ c }}`, map[string]any{"c": c}); got != want {
			t.Errorf("nested render after caller mutation = %q, want %q", got, want)
		}
	})
}

// TestOptionalAliasNoneVsEmptySome pins the distinction a regression collapsing
// EnumValue.Alias from *string to a plain string would ERASE: an absent @alias
// (nil, Option::None) and an explicit @alias("") (non-nil "", Some("")) are
// DIFFERENT values, and None sorts strictly BELOW Some(""). BAML's comparator is
// Rust's `Option<String>::cmp` (rs:249-252), which this reproduces.
//
// Every OTHER alias assertion in this file stays green under that collapse,
// because they only ever use a non-empty alias or no alias; only the
// None-vs-Some("") pair separates the two representations. This is the CGO-free
// twin of the enum_empty_alias corpus row.
func TestOptionalAliasNoneVsEmptySome(t *testing.T) {
	// The comparator primitive, directly: None < Some(""), and neither is equal
	// across the nil boundary.
	empty := ""
	for _, tc := range []struct {
		name string
		a, b *string
		want int
	}{
		{`None < Some("")`, nil, &empty, -1},
		{`Some("") > None`, &empty, nil, 1},
		{"None == None", nil, nil, 0},
		{`Some("") == Some("")`, &empty, &empty, 0},
	} {
		if got := compareOptionalAlias(tc.a, tc.b); got != tc.want {
			t.Errorf("compareOptionalAlias(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	// And end-to-end through two members with the SAME canonical name: the
	// absent-alias member sorts below the explicit-empty-alias one, and they are
	// NOT equal. If the alias were a plain string, both would be "" and this would
	// render `false/true/false`.
	noAlias, err := EnumMember("E", "X", nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyAlias, err := EnumMember("E", "X", strptr(""))
	if err != nil {
		t.Fatal(err)
	}
	got := mustRender(t, Config{}, `{{ a < b }}/{{ a == b }}/{{ a != b }}`,
		map[string]any{"a": noAlias, "b": emptyAlias})
	if got != "true/false/true" {
		t.Errorf(`None-vs-Some("") member comparison = %q, want "true/false/true"`, got)
	}
}

// TestEnumHostObjectsAreNonEnumerableMaps is the LEAF GUARDRAIL for the
// non-enumerable-map wiring. Both enum host objects are ObjectReprMap and
// implement NEITHER value.MapObject NOR value.MapGetter, which is how BAML's
// `Enumerator::NonEnumerable` with an unknown length is spelled at the fork's
// generic value-model seam.
//
// The fork encodes the distinction in the boolean of Value.MapKeys: a
// known-empty `{}` answers ([], true); an object with no enumerable pairs
// answers (nil, false). Equality, ordering, membership, iteration and `is
// iterable` all branch on that boolean (v2.16.0-baml.4, PATCHES #102/#103/#105),
// so if a Keys()/Map() method were ever added to either type — even one
// returning nothing — the member would become a KNOWN-EMPTY map and every one of
// those behaviors would silently diverge from stock BAML.
//
// The behavioral consequences are proven byte-for-byte against stock BAML CFFI
// in ./profileoracle (enum_cmp_opaque_* and enum_non_enumerable_state); this
// test pins the underlying capability directly so the cause is visible at the
// leaf even without CGO.
func TestEnumHostObjectsAreNonEnumerableMaps(t *testing.T) {
	member, err := EnumMember("Color", "RED", strptr("rouge"))
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := newEnumType(colorEnum())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		val  value.Value
	}{
		{"member", member},
		{"namespace", value.FromObject(namespace)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.val.Kind(); got != value.KindMap {
				t.Errorf("Kind() = %v, want KindMap (BAML's ObjectRepr::Map)", got)
			}
			keys, ok := tc.val.MapKeys()
			if ok {
				t.Errorf("MapKeys() reported ok = true (keys %v); a %s is NON-ENUMERABLE, "+
					"not a known-empty map — it must not implement MapObject/MapGetter", keys, tc.name)
			}
			if keys != nil {
				t.Errorf("MapKeys() returned keys %v alongside ok = false", keys)
			}
			obj, _ := tc.val.AsObject()
			if _, isMapObject := obj.(value.MapObject); isMapObject {
				t.Errorf("%s implements value.MapObject; that makes it an enumerable map", tc.name)
			}
			if _, isMapGetter := obj.(value.MapGetter); isMapGetter {
				t.Errorf("%s implements value.MapGetter; that makes it an enumerable map", tc.name)
			}
		})
	}

	// A known-empty class is the CONTROL: same KindMap, but its pairs ARE
	// enumerable, so it answers ([], true). This is exactly the distinction the
	// fork's equality/ordering seam reads.
	t.Run("empty_class_control_is_enumerable", func(t *testing.T) {
		empty, err := ClassValue(nil)
		if err != nil {
			t.Fatal(err)
		}
		keys, ok := empty.MapKeys()
		if !ok {
			t.Fatal("an empty host class reported ok = false; it is a KNOWN-empty map")
		}
		if len(keys) != 0 {
			t.Errorf("empty class MapKeys() = %v, want no keys", keys)
		}
	})
}

// TestEnumOpaqueMapSemantics pins the observable behavior the non-enumerable
// wiring buys, at the leaf, for fast CGO-free feedback. Every expectation here
// is stock BAML v0.223 CFFI authority, proven byte-for-byte by the
// enum_cmp_opaque_* corpus rows in ./profileoracle.
//
// Note the deliberate ASYMMETRY: `Color.RED == {}` is false but `{} ==
// Color.RED` is true, because BAML's map fallback short-circuits on the LEFT
// operand's absent pair iterator and otherwise counts a non-enumerable right
// side as length zero. Membership inherits it through the orientation of
// `item.Equal(needle)`. That is stock behavior, not a defect to smooth over.
func TestEnumOpaqueMapSemantics(t *testing.T) {
	shade := EnumDef{Name: "Shade", Values: []EnumValue{{Canonical: "RED", Alias: strptr("rouge")}}}
	cfg := Config{Enums: []EnumDef{colorEnum(), shade}}
	cases := []struct{ name, src, want string }{
		// Namespace/member equality: the comparator declines, and the map fallback
		// must NOT then call two non-enumerable maps structurally equal.
		{"member_eq_namespace", `{{ Color.RED == Color }}`, "false"},
		{"namespace_eq_member", `{{ Color == Color.RED }}`, "false"},
		{"member_ne_namespace", `{{ Color.RED != Color }}`, "true"},
		{"namespace_ne_member", `{{ Color != Color.RED }}`, "true"},
		// Membership through both orientations.
		{"member_in_namespace_list", `{{ Color.RED in [Color] }}`, "false"},
		{"namespace_in_member_list", `{{ Color in [Color.RED] }}`, "false"},
		{"namespace_in_namespace_list", `{{ Color in [Shade] }}`, "false"},
		{"member_in_mixed_list", `{{ Color.RED in [Color, Shade] }}`, "false"},
		// The empty-map row, both orientations. Asymmetric ON PURPOSE.
		{"member_eq_empty_map", `{{ Color.RED == {} }}`, "false"},
		{"empty_map_eq_member", `{{ {} == Color.RED }}`, "true"},
		{"member_ne_empty_map", `{{ Color.RED != {} }}`, "true"},
		{"empty_map_ne_member", `{{ {} != Color.RED }}`, "false"},
		{"member_in_empty_map_list", `{{ Color.RED in [{}] }}`, "true"},
		{"empty_map_in_member_list", `{{ {} in [Color.RED] }}`, "false"},
		// Map representation is NOT enumerability, and neither is truthiness.
		{"is_mapping", `{{ Color.RED is mapping }}/{{ Color is mapping }}`, "true/true"},
		{"is_iterable", `{{ Color.RED is iterable }}/{{ Color is iterable }}`, "false/false"},
		{"truthy", `{% if Color.RED %}T{% else %}F{% endif %}/{% if Color %}T{% else %}F{% endif %}`, "T/T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRender(t, cfg, tc.src, nil); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
		})
	}

	// Ordering two non-enumerable maps is stock MiniJinja's `unreachable!()`. The
	// profile must FAULT, never render a conservative `false` — a native success
	// where BAML fails internally is the parity-decline rule's out-do.
	t.Run("order_faults", func(t *testing.T) {
		for _, src := range []string{
			`{{ Color.RED < Color }}`,
			`{{ Color < Color.RED }}`,
			`{{ Color.RED > Color }}`,
			`{{ Color.RED <= Color }}`,
		} {
			out, err := renderHostRecovering(t, cfg, src, nil)
			if err == nil {
				t.Errorf("%s rendered %q; ordering two non-enumerable maps must FAULT the way stock BAML's unreachable!() does", src, out)
			}
		}
	})

	// Iterating either object errors ("map is not iterable"); it is NOT silently
	// an empty loop.
	t.Run("for_faults", func(t *testing.T) {
		for _, src := range []string{
			`{% for x in Color.RED %}{{ x }}{% endfor %}`,
			`{% for x in Color %}{{ x }}{% endfor %}`,
		} {
			out, err := renderHostRecovering(t, cfg, src, nil)
			if err == nil {
				t.Errorf("%s rendered %q; a non-enumerable map is not an empty iterable", src, out)
			}
		}
	})
}

// renderHostRecovering renders like renderHost but also converts the fork's
// recoverable ordering panic (value.UnorderableMaps, the fork's spelling of
// MiniJinja's `unreachable!()`) into an error, so a fault row can be asserted
// without crashing the test binary.
func renderHostRecovering(t *testing.T, cfg Config, src string, ctx map[string]any) (out string, err error) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			if u, ok := rec.(value.UnorderableMaps); ok {
				err = u
				return
			}
			panic(rec)
		}
	}()
	return renderHost(t, cfg, src, ctx)
}

// TestHostDebugWalkerModes pins the ONE debug walker's two modes and the exact
// mode each nesting position selects — the semantics the previously duplicated
// compactDebug/prettyDebug pair carried. A directly-rendered list is compact
// (ambient non-alternate debug_list, rs:373-388); a class is ALWAYS alternate
// (Display forces `{map:#?}`, rs:279); a list nested in a class inherits
// alternate; a list nested in a compact list stays compact; a nested none is
// null and a nested enum member is its BARE alias in both modes.
func TestHostDebugWalkerModes(t *testing.T) {
	red, err := EnumMember("Color", "RED", strptr("rouge"))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := ListValue([]value.Value{value.FromString("a"), value.None(), red})
	if err != nil {
		t.Fatal(err)
	}
	nestedInList, err := ListValue([]value.Value{inner})
	if err != nil {
		t.Fatal(err)
	}
	cls, err := ClassValue([]ClassField{{Canonical: "items", Alias: strptr("data"), Value: inner}})
	if err != nil {
		t.Fatal(err)
	}
	clsInList, err := ListValue([]value.Value{cls})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, want string
		val        value.Value
	}{
		{"direct_list_is_compact", `["a", null, rouge]`, inner},
		{"list_in_list_stays_compact", `[["a", null, rouge]]`, nestedInList},
		{"list_in_class_goes_alternate",
			"{\n    \"data\": [\n        \"a\",\n        null,\n        rouge,\n    ],\n}", cls},
		// A class inside a COMPACT list is still multi-line, at indent 0: its
		// Display forces the alternate map regardless of the ambient flag.
		{"class_in_compact_list_is_still_alternate",
			"[{\n    \"data\": [\n        \"a\",\n        null,\n        rouge,\n    ],\n}]", clsInList},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRender(t, Config{}, `{{ v }}`, map[string]any{"v": tc.val}); got != tc.want {
				t.Errorf("render = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHostObjectStringComposes pins that host display reaches ORDINARY Jinja
// containers, coercions and concatenation — not just the top-level formatter.
// The fork's Value.String and Value.Repr dispatch value.ObjectWithString
// (v2.16.0-baml.4, PATCHES #104), so every consumer built on those two
// primitives inherits it and NO per-filter shim exists in this package.
//
// Each case also asserts the output carries no Go-format leakage (`&{` or a
// `0x` pointer), which is what these paths emitted before the fork fix — and
// which was nondeterministic output, not merely ugly. The byte authority is the
// object_string_* corpus rows in ./profileoracle.
func TestHostObjectStringComposes(t *testing.T) {
	cfg := Config{Enums: []EnumDef{colorEnum()}}
	red, err := EnumMember("Color", "RED", strptr("rouge"))
	if err != nil {
		t.Fatal(err)
	}
	xs, err := ListValue([]value.Value{red})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ClassValue([]ClassField{{Canonical: "prop1", Alias: strptr("key1"), Value: value.FromString("value")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := map[string]any{"xs": xs, "c": c}

	cases := []struct{ name, src, want string }{
		{"native_enum_list", `{{ [Color.RED] }}`, "[rouge]"},
		{"join", `{{ [Color.RED]|join(",") }}`, "rouge"},
		{"upper", `{{ Color.RED|upper }}`, "ROUGE"},
		{"concat", `{{ Color.RED ~ "!" }}`, "rouge!"},
		{"filter_string", `{{ Color.RED|string }}`, "rouge"},
		{"native_host_list", `{{ [xs] }}`, "[[rouge]]"},
		{"native_class", `{{ [c] }}`, "[{\n    \"key1\": \"value\",\n}]"},
		// The NAMESPACE object: BAML gives MinijinjaBamlEnumType no Display, so the
		// engine's default Map render produces `{}` from its (absent) pairs. Every
		// position, because this is the object whose Go `%v` carried heap addresses.
		{"namespace_bare", `[{{ Color }}]`, "[{}]"},
		{"namespace_in_list", `{{ [Color] }}`, "[{}]"},
		{"namespace_string", `[{{ Color|string }}]`, "[{}]"},
		{"namespace_concat", `{{ Color ~ "!" }}`, "{}!"},
		{"namespace_upper", `{{ Color|upper }}`, "{}"},
		{"namespace_in_map", `{{ {"k": Color} }}`, `{"k": {}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRender(t, cfg, tc.src, ctx)
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
			}
			if strings.Contains(got, "&{") || strings.Contains(got, "0x") {
				t.Errorf("%s leaked Go formatting: %q", tc.src, got)
			}
		})
	}
}

// TestFormatTypeDeclined pins that BAML's render-layer format(type=...) host
// serialization is NOT reproduced by the profile: the get_env-level `format` is
// the fork's printf filter, so `class|format(type="json")` ERRORS ("value is not
// a string") rather than silently emitting a JSON/YAML/TOON serialization. This
// is a real, non-silent boundary — the deferral is tracked on #602. (Media host
// values are likewise declined: the profile exposes no media constructor, so a
// media value cannot enter the render context in the first place.)
func TestFormatTypeDeclined(t *testing.T) {
	c, err := ClassValue([]ClassField{{Canonical: "prop1", Alias: strptr("key1"), Value: value.FromString("value")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderHost(t, Config{}, `{{ c|format(type="json") }}`, map[string]any{"c": c})
	if err == nil {
		t.Fatal("format(type=\"json\") on a class did not error — the profile must NOT silently claim BAML's format serialization")
	}
	// Only the error KIND is asserted: ErrInvalidOperation is the documented
	// contract, while the message text ("... not a string") is an incidental
	// detail of which printf-filter arm declines and is not a contract to pin.
	var me *minijinja.Error
	if !errors.As(err, &me) || me.Kind != minijinja.ErrInvalidOperation {
		t.Errorf("format decline error = %v (kind %v), want ErrInvalidOperation", err, kindOf(err))
	}
}
