package nativeprompt

import (
	"testing"

	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// This file is the #597 enum-comparison fence, rewritten for Slice 7.1a.
//
// Before the cutover it stood up a PRIVATE enum object on the pre-fork external
// engine and pinned that engine's divergence from BAML: `Color.RED == 'RED'`
// rendered false because the old engine's equality path never consulted an
// object comparator. Nothing in that fixture was production code.
//
// After the cutover there is no second engine to diverge. The production render
// seam is bamlprofile (PR #652 closed the comparator at the leaf: ValueCmp is
// reached from BOTH operand positions), so the ENGINE now answers exactly as
// stock BAML v0.223 does. The two halves below pin that, and pin that Slice 7.1a
// still does not ADMIT enum comparison:
//
//  1. through the production render-context adapter, the five historical #597
//     rows — plus their reverse operand orders — now match stock BAML;
//  2. through the production admission gate, an enum comparison or an enum
//     attribute access in a static prompt STILL declines.
//
// (2) is the deliberate scope line: closing #597 end to end needs the resolved
// static host-type seam (descriptor -> binder -> RenderStatic) that Slice 7.1b
// builds. Until then baml-rest declines to BAML rather than admitting a shape it
// has no stock-fixture proof for, even though the engine underneath could now
// render it.

// colorConfig is the resolved Color enum of the historical #597 fixture: three
// variants, each with a display alias, in canonical declaration order. It is a
// bamlprofile.Config — the real typed input of the production adapter — not a
// private test object.
func colorConfig(outputFormat string) bamlprofile.Config {
	alias := func(s string) *string { return &s }
	return bamlprofile.Config{
		OutputFormat: outputFormat,
		Enums: []bamlprofile.EnumDef{{
			Name: "Color",
			Values: []bamlprofile.EnumValue{
				{Canonical: "RED", Alias: alias("rouge")},
				{Canonical: "GREEN", Alias: alias("vert")},
				{Canonical: "BLUE", Alias: alias("bleu")},
			},
		}},
	}
}

// TestEnumComparisonMatchesBAMLThroughProfileSeam is the former
// TestValueCmpEqualityDivergesFromBAML, inverted. Every row is the stock BAML
// v0.223 result; the old-engine column no longer exists because the old engine
// is no longer wired.
//
// This proves the ENGINE half of #597 through nativeprompt's production adapter.
// It does NOT claim #597 is closed end to end: no admitted 7.1a template can
// reach these expressions (see TestStaticAdmissionStillFencesEnumComparison).
func TestEnumComparisonMatchesBAMLThroughProfileSeam(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		// The five historical rows, with BAML's answers.
		{`{{ Color.RED == 'RED' }}`, "true"},
		{`{{ Color.RED == 'rouge' }}`, "false"}, // an alias is display only, never identity
		{`{{ Color.RED == Color.RED }}`, "true"},
		{`{{ 'RED' in [Color.RED] }}`, "true"},
		{`{{ Color.RED == Color.BLUE }}`, "false"},

		// Reverse operand order: the fork reaches ValueCmp from both sides.
		{`{{ 'RED' == Color.RED }}`, "true"},
		{`{{ 'rouge' == Color.RED }}`, "false"},
		{`{{ Color.RED != 'RED' }}`, "false"},
		{`{{ 'RED' != Color.RED }}`, "false"},

		// Canonical name, not alias, is the comparison identity in both
		// directions; `.value` exposes it.
		{`{{ Color.RED.value == 'RED' }}`, "true"},
	}
	for _, tc := range cases {
		if got := mustRenderThroughSeam(t, colorConfig(""), tc.expr); got != tc.want {
			t.Errorf("%s => %q, want %q (stock BAML v0.223)", tc.expr, got, tc.want)
		}
	}
}

// TestEnumOrderingThroughProfileSeam keeps the half the old engine already got
// right — display is the alias, ordering is by canonical name — now asserted
// against the production seam rather than a private object.
func TestEnumOrderingThroughProfileSeam(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{`{{ Color.RED }}`, "rouge"}, // display is the alias
		{`{{ Color.BLUE < Color.RED }}`, "true"},
		{`{{ Color.RED < Color.BLUE }}`, "false"},
		// sort orders by canonical name (BLUE, GREEN, RED), which displays as
		// bleu, vert, rouge — an order the aliases alone would not produce.
		{`{% for c in [Color.RED, Color.GREEN, Color.BLUE]|sort %}{{ c }},{% endfor %}`, "bleu,vert,rouge,"},
		{`{{ [Color.RED, Color.GREEN, Color.BLUE]|min }}`, "bleu"},
		{`{{ [Color.RED, Color.GREEN, Color.BLUE]|max }}`, "rouge"},
	}
	for _, tc := range cases {
		if got := mustRenderThroughSeam(t, colorConfig(""), tc.expr); got != tc.want {
			t.Errorf("%s => %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// TestStaticAdmissionStillFencesEnumComparison is the Slice 7.1a scope line: the
// engine can now answer #597, but SupportsStatic must NOT admit it. Every row
// below is a shape whose decline key is unchanged by the cutover.
//
// Without this test the cutover could silently widen the served surface — the
// exact failure mode the "wire the engine, do not broaden admission" split
// exists to prevent.
func TestStaticAdmissionStillFencesEnumComparison(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"equality", `{{ _.role("user") }}{{ Color.RED == 'RED' }}`, FeatureEnumComparison},
		{"reverse_equality", `{{ _.role("user") }}{{ 'RED' == Color.RED }}`, FeatureEnumComparison},
		{"inequality", `{{ _.role("user") }}{{ Color.RED != Color.BLUE }}`, FeatureEnumComparison},
		{"containment", `{{ _.role("user") }}{{ 'RED' in [Color.RED] }}`, FeatureEnumComparison},
		{"ordering", `{{ _.role("user") }}{{ Color.BLUE < Color.RED }}`, FeatureEnumComparison},
		{"member_render", `{{ _.role("user") }}{{ Color.RED }}`, FeatureEnumClassValue},
		{"member_value", `{{ _.role("user") }}{{ Color.RED.value }}`, FeatureEnumClassValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertStaticDecline(t, staticFn(tc.prompt), map[string]any{}, tc.want)
		})
	}
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

	// The production constructor installs no enum namespace, so `Color` is
	// undefined rather than a member namespace.
	if got := mustRenderThroughSeam(t, bamlprofile.Config{}, `{{ Color is undefined }}`); got != "true" {
		t.Errorf("production render context defines Color = %q, want it undefined", got)
	}
}
