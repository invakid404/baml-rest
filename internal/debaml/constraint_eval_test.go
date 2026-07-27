package debaml

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Unit-lane cover for the constraint evaluator. The AUTHORITATIVE proof is the
// stock BAML v0.223.0 differential in internal/debaml/constraintoracle (330
// cases, //go:build integration). These tests are the parts of that claim that
// can be pinned without CGO and a generated client, so they run in the ordinary
// `go test ./...` lane and catch a regression before anyone reaches for the
// oracle:
//
//   - BAML's OWN jinja_helpers.rs unit tests (:102-218);
//   - the evaluate_predicate contract (exact "true"/"false", else an error);
//   - the value model's mappings and enum-to-string erasure;
//   - the five builtins withdrawn because BAML's minijinja build lacks them;
//   - the fail-closed profile, including the 64-bit numeric boundary.

// TestJinjaHelpersPinnedRenders reproduces the render_expression, regex_match
// and sum_filter assertions from BAML's own test module
// (engine/baml-lib/baml-core/src/ir/jinja_helpers.rs:102-218). These are the
// only expressions BAML itself pins, so a divergence here is unambiguous.
//
// The regex cases are reachable ONLY from this direction: BAML's @check
// attribute lexer doubles backslashes, so a .baml constraint cannot express
// `\d` at all (pinned by the oracle's f_regex_class / f_regex_word cases).
func TestJinjaHelpersPinnedRenders(t *testing.T) {
	list := ListValue([]ConstraintValue{IntValue(1), IntValue(2), IntValue(3)})
	phone := StringValue("(123)456-7890")

	cases := []struct {
		name string
		this ConstraintValue
		expr string
		want string
	}{
		{"literal", list, "1", "1"},
		{"arithmetic", list, "1 + 1", "2"},
		{"length_gt", list, "this|length > 2", "true"},
		{"regex_substring", phone, `this|regex_match("123")`, "true"},
		{"sum_ints", list, "[1,2]|sum", "3"},
		{"sum_mixed", list, "[1,2.5]|sum", "3.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderConstraintExpression(tc.this, tc.expr)
			if err != nil {
				t.Fatalf("render %q: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Fatalf("render %q = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}

	// ACCEPTED COST. jinja_helpers.rs's own phone-number case carries a `-`
	// inside a character class, and the arithmetic gate does not skip string
	// literals — skipping them would need string lexing that could itself fail
	// open, which is the mistake round 6 exists to undo. So a regex containing an
	// arithmetic byte is refused rather than parsed. Pinned so the cost is
	// visible rather than discovered.
	phoneRe := `this|regex_match("\\(?\\d{3}\\)?[-.\\s]?\\d{3}[-.\\s]?\\d{4}")`
	if _, err := RenderConstraintExpression(phone, phoneRe); !errors.Is(err, ErrConstraintUnsupported) {
		t.Errorf("a regex containing an arithmetic byte is expected to be refused; got err=%v", err)
	}
}

// TestEvaluateConstraintBooleanContract pins evaluate_predicate
// (jinja_helpers.rs:83-94): the render must be EXACTLY "true" or "false".
// Anything else is an evaluation error, which BAML turns into a coercion
// failure of the constrained node — NOT a failed check — so a caller must never
// map it to `status: failed`.
func TestEvaluateConstraintBooleanContract(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		ok, err := EvaluateConstraint(IntValue(5), "this > 0")
		if err != nil || !ok {
			t.Fatalf("got (%v, %v), want (true, nil)", ok, err)
		}
	})
	t.Run("false", func(t *testing.T) {
		ok, err := EvaluateConstraint(IntValue(-5), "this > 0")
		if err != nil || ok {
			t.Fatalf("got (%v, %v), want (false, nil)", ok, err)
		}
	})

	// Each of these renders successfully but not to a boolean.
	nonBoolean := []struct {
		expr string
		this ConstraintValue
	}{
		{"this", IntValue(7)},           // renders "7"
		{`this ~ ""`, IntValue(1)},      // renders "1"
		{"[this]", IntValue(1)},         // renders "[1]"
		{`"yes"`, IntValue(1)},          // renders "yes"
		{"this|length", ListValue(nil)}, // renders "0"
	}
	for _, tc := range nonBoolean {
		if _, err := EvaluateConstraint(tc.this, tc.expr); err == nil {
			t.Errorf("%q rendered a non-boolean but did not error", tc.expr)
		}
	}

	// The check is on the rendered TEXT, not on a type: a string whose content
	// happens to be "true" IS accepted, exactly as in BAML.
	ok, err := EvaluateConstraint(StringValue("true"), "this")
	if err != nil || !ok {
		t.Errorf(`a string "true" should evaluate as the boolean true; got (%v, %v)`, ok, err)
	}

	// A null `this` renders "null" through BAML's formatter (jinja_helpers.rs:20-24
	// substitutes the string "null" for a top-level none, where stock minijinja
	// would render "none") — which is also not a boolean.
	rendered, err := RenderConstraintExpression(NullValue(), "this")
	if err != nil {
		t.Fatalf("render bare null: %v", err)
	}
	if rendered != "null" {
		t.Fatalf("bare null rendered %q, want %q (BAML's none formatter)", rendered, "null")
	}
	if _, err := EvaluateConstraint(NullValue(), "this"); err == nil {
		t.Error("a null `this` is not a boolean but did not error")
	}
}

// TestConstraintValueMappingIsOrderedInTheModelButOpaqueInTheEngine pins the
// split the profile draws around mappings.
//
// The MODEL is lossless: BAML's BamlMap is an IndexMap, and insertion order
// survives into ConstraintValue and out through MarshalJSON. The ENGINE is not:
// minijinja-Go can represent a mapping either order-faithfully (an object,
// invisible to `in`) or membership-faithfully (a Go map, enumerated sorted),
// never both, so the evaluator refuses any expression whose answer depends on
// which one it picked. What survives is the order-independent surface.
//
// "z","a","m" is deliberately neither sorted nor reverse-sorted, so a sorted
// implementation cannot pass by accident.
func TestConstraintValueMappingIsOrderedInTheModelButOpaqueInTheEngine(t *testing.T) {
	entries := []ConstraintEntry{
		{Key: "z", Value: IntValue(1)},
		{Key: "a", Value: IntValue(2)},
		{Key: "m", Value: IntValue(3)},
	}
	for _, this := range []ConstraintValue{MapValue(entries), ClassValue("Probe", entries)} {
		t.Run(this.Kind().String(), func(t *testing.T) {
			// The model keeps the order.
			got, err := json.Marshal(this)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if want := `{"z":1,"a":2,"m":3}`; string(got) != want {
				t.Fatalf("model marshalled %s, want %s (insertion order)", got, want)
			}

			// The order-independent surface still decides.
			for expr, want := range map[string]string{
				`this|length`: "3",
				`this.z`:      "1",
				`this["m"]`:   "3",
			} {
				got, err := RenderConstraintExpression(this, expr)
				if err != nil {
					t.Errorf("render %q: %v", expr, err)
					continue
				}
				if got != want {
					t.Errorf("render %q = %q, want %q", expr, got, want)
				}
			}

			// Anything that would expose the order — or membership — is refused.
			for _, expr := range []string{
				`this|list|join(",")`, `this|first`, `this|last`, `this|items`,
				`this|tojson`, `this|string`, `this.keys()`, `this.values()`,
				`this.get("z")`, `"z" in this`,
			} {
				if _, err := RenderConstraintExpression(this, expr); !errors.Is(err, ErrConstraintUnsupported) {
					t.Errorf("render %q should be outside the profile; got err=%v", expr, err)
				}
			}
		})
	}
}

// TestConstraintValueTypeNameIsInvisible pins that the class/enum type name is
// carried by the model (BamlValue::Class(name, _) / Enum(name, _)) but is
// INVISIBLE to the predicate: the expression sees a mapping or a string, and
// nothing addressable by the type name.
func TestConstraintValueTypeNameIsInvisible(t *testing.T) {
	cls := ClassValue("Probe", []ConstraintEntry{{Key: "b", Value: IntValue(1)}})
	enum := EnumValue("Hue", "RED")
	if got := cls.TypeName(); got != "Probe" {
		t.Errorf("class TypeName = %q, want %q", got, "Probe")
	}
	if got := enum.TypeName(); got != "Hue" {
		t.Errorf("enum TypeName = %q, want %q", got, "Hue")
	}
	if got := IntValue(1).TypeName(); got != "" {
		t.Errorf("scalar TypeName = %q, want empty", got)
	}
	// The name is not addressable, and not in the marshalled document either.
	if _, err := EvaluateConstraint(cls, `this.Probe is defined`); err != nil {
		t.Errorf("unexpected error probing for the class name: %v", err)
	} else if ok, _ := EvaluateConstraint(cls, `this.Probe is defined`); ok {
		t.Error("the class name is addressable as an attribute; it must be dropped")
	}
	doc, err := json.Marshal(cls)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(doc), "Probe") {
		t.Errorf("marshalled class %s leaks the class name", doc)
	}
	if got, err := RenderConstraintExpression(enum, "this"); err != nil || got != "RED" {
		t.Errorf(`render bare enum = (%q, %v), want ("RED", nil)`, got, err)
	}
}

// TestConstraintValueEnumIsAString pins the value model's enum erasure
// (baml_value.rs:51 — `BamlValue::Enum(_, v) => serializer.serialize_str(v)`).
// It is also why the BoundaryML value_cmp fork, the one documented divergence
// of the prompt renderer (internal/nativeprompt/valuecmp_test.go), does not
// reach constraints at all: a predicate never sees an enum OBJECT, only its
// variant name.
func TestConstraintValueEnumIsAString(t *testing.T) {
	red := EnumValue("Hue", "RED")
	for expr, want := range map[string]string{
		`this == "RED"`:            "true",
		`this == "rouge"`:          "false", // the @alias is NOT the value
		`this is string`:           "true",
		`this|length`:              "3",
		`this in ["RED", "GREEN"]`: "true",
	} {
		got, err := RenderConstraintExpression(red, expr)
		if err != nil {
			t.Fatalf("render %q: %v", expr, err)
		}
		if got != want {
			t.Errorf("render %q = %q, want %q", expr, got, want)
		}
	}
}

// TestConstraintSumFilter pins BAML's `sum` (jinja_helpers.rs:45-65), which
// REPLACES minijinja's built-in one. The int-vs-float rule is asymmetric
// because minijinja's own conversions are: an integral float converts to i64,
// a bool converts to i64 but NOT to f64.
func TestConstraintSumFilter(t *testing.T) {
	for expr, want := range map[string]string{
		"[1,2]|sum":    "3",
		"[1,2.5]|sum":  "3.5",
		"[1,2.0]|sum":  "3", // 2.0 IS an i64 to minijinja, so this stays an int
		"[true,1]|sum": "2", // bool -> i64, and the i64 arm wins
		`["a"]|sum`:    "0", // neither arm applies
		"[]|sum":       "0",
	} {
		got, err := RenderConstraintExpression(NullValue(), expr)
		if err != nil {
			t.Fatalf("render %q: %v", expr, err)
		}
		if got != want {
			t.Errorf("render %q = %q, want %q", expr, got, want)
		}
	}

	// `Vec<Value>` accepts only a sequence or iterable, so a string, a mapping
	// or a scalar is an error rather than 0 — and BAML's sum takes no arguments,
	// so minijinja's `attribute=` kwarg is rejected too.
	for _, expr := range []string{`"ab"|sum`, `{"a":1}|sum`, `1|sum`, `[{"a":1}]|sum(attribute="a")`} {
		if _, err := RenderConstraintExpression(NullValue(), expr); err == nil {
			t.Errorf("%q should have errored", expr)
		}
	}
}

// TestConstraintRegexMatchFilter pins BAML's `regex_match`
// (jinja_helpers.rs:38-43): an INVALID pattern is `false`, not an error, and
// both parameters are minijinja `String` args, which Display any value rather
// than requiring a string.
func TestConstraintRegexMatchFilter(t *testing.T) {
	for expr, want := range map[string]string{
		`"abc"|regex_match("^a")`: "true",
		`"abc"|regex_match("^b")`: "false",
		`"abc"|regex_match("[")`:  "false", // invalid pattern -> false, NOT an error
		`1|regex_match("1")`:      "true",  // non-string subject is Displayed
	} {
		got, err := RenderConstraintExpression(NullValue(), expr)
		if err != nil {
			t.Fatalf("render %q: %v", expr, err)
		}
		if got != want {
			t.Errorf("render %q = %q, want %q", expr, got, want)
		}
	}
	if _, err := RenderConstraintExpression(NullValue(), `"a"|regex_match`); err == nil {
		t.Error("regex_match without a pattern should have errored")
	}
}

// TestWithdrawnBuiltinsError pins the five names minijinja-Go registers that
// BAML's minijinja build does not have (engine/Cargo.toml:99-115 selects
// default-features=false + builtins/json, which excludes urlencode; cycler,
// joiner, lipsum and the `containing` test do not exist in minijinja 2.16.0 at
// all). Leaving them live would be the dangerous asymmetry — native answering
// where BAML rejects the value.
func TestWithdrawnBuiltinsError(t *testing.T) {
	for _, expr := range []string{
		"range(3)|length == 3", // withdrawn in round 6: no handle to guard it
		`"a b"|urlencode == "a%20b"`,
		`"abc" is containing("b")`,
		`cycler("a","b").next() == "a"`,
		`joiner(",")() == ""`,
		`lipsum(1)|length > 0`,
	} {
		if _, err := EvaluateConstraint(NullValue(), expr); err == nil {
			t.Errorf("%q evaluated natively; BAML's minijinja build does not have it", expr)
		}
	}

	// Control: the builtins BAML DOES have must still work, so the withdrawal is
	// specific rather than a blanket break.
	for _, expr := range []string{
		`"abc"|length == 3`, `dict(a=1)|length == 1`,
		`namespace(a=1).a == 1`, `[1,2]|tojson == "[1,2]"`, `"abc" is startingwith("a")`,
	} {
		ok, err := EvaluateConstraint(NullValue(), expr)
		if err != nil || !ok {
			t.Errorf("%q = (%v, %v), want (true, nil)", expr, ok, err)
		}
	}
}

// TestConstraintValueMarshalJSON pins the value model's serialization against
// BAML's `impl Serialize for BamlValue` (baml_value.rs:42-56): enum erasure,
// class-name erasure, and INSERTION-ordered maps and classes (encoding/json
// would sort a Go map, so the entries are written by hand).
func TestConstraintValueMarshalJSON(t *testing.T) {
	entries := []ConstraintEntry{
		{Key: "z", Value: IntValue(1)},
		{Key: "a", Value: StringValue("x")},
		{Key: "m", Value: ListValue([]ConstraintValue{BoolValue(true), NullValue()})},
	}
	for _, tc := range []struct {
		name string
		val  ConstraintValue
		want string
	}{
		{"null", NullValue(), "null"},
		{"bool", BoolValue(true), "true"},
		{"int", IntValue(-3), "-3"},
		{"float", FloatValue(2.5), "2.5"},
		{"string", StringValue("hi"), `"hi"`},
		{"enum", EnumValue("Hue", "RED"), `"RED"`},
		{"list", ListValue([]ConstraintValue{IntValue(1), IntValue(2)}), "[1,2]"},
		{"empty_list", ListValue(nil), "[]"},
		{"map", MapValue(entries), `{"z":1,"a":"x","m":[true,null]}`},
		{"class", ClassValue("Probe", entries), `{"z":1,"a":"x","m":[true,null]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestConstraintErrorsAreFailClosed pins that the shapes native cannot
// reproduce come back as ERRORS rather than as a boolean. That is the whole
// safety argument for the future serving slice: an error declines to BAML, a
// wrong boolean would be served.
//
// The pycompat string and sequence methods are the bulk of it — BAML installs
// minijinja-contrib's unknown-method callback and minijinja-Go v2.16.0 has no
// hook for one. If a future minijinja-Go grows the hook, this test fails and
// the corpus notes in constraintoracle must be revisited.
func TestConstraintErrorsAreFailClosed(t *testing.T) {
	for _, expr := range []string{
		`"abc".upper() == "ABC"`,
		`"{:,}".format(1234567) == "1,234,567"`,
		`"abc".startswith("a")`,
		`[1,1].count(1) == 2`,
		`1|nosuchfilter == 1`,
		`1 is nosuchtest`,
		`nosuchfn() == 1`,
		`(1).nosuchmethod() == 1`,
	} {
		if _, err := EvaluateConstraint(NullValue(), expr); err == nil {
			t.Errorf("%q evaluated natively; expected a fail-closed error", expr)
		}
	}
}

// TestConstraintExpressionIsWrappedVerbatim pins the template
// render_expression builds (jinja_helpers.rs:76): the literal "{{ ", the bare
// source, then " }}". A stray trim or an extra space would change nothing for
// most expressions and everything for one that is whitespace-sensitive, so it
// is asserted directly rather than inferred.
func TestConstraintExpressionIsWrappedVerbatim(t *testing.T) {
	// A whitespace-control marker would only bite if the wrapper were rebuilt
	// differently; `{{- ... }}` is legal inside the outer braces only because
	// the source is interpolated verbatim.
	got, err := RenderConstraintExpression(StringValue("  x  "), "this")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "  x  " {
		t.Fatalf("render = %q, want %q — the wrapper must not trim the value", got, "  x  ")
	}

	// A syntactically broken expression must surface as a compile error naming
	// the evaluator, not panic or render partially.
	_, err = RenderConstraintExpression(NullValue(), "this |")
	if err == nil {
		t.Fatal("a malformed expression should not compile")
	}
	if !strings.Contains(err.Error(), "debaml:") {
		t.Fatalf("compile error %q should be wrapped by the evaluator", err)
	}
}

// ---------------------------------------------------------------------------
// The proven profile (constraint_profile.go).
// ---------------------------------------------------------------------------

// TestConstraintProfileNeverAnswersOutsideTheProfile is the unit-lane statement
// of the evaluator's contract: every shape the profile excludes must come back
// as ErrConstraintUnsupported, never as a usable boolean.
//
// Each entry is a MEASURED divergence from stock BAML v0.223.0 — the stock side
// of every one is pinned by the corresponding case in
// internal/debaml/constraintoracle. This test is the cheap, CGO-free half: it
// pins native's side so a guard cannot be removed without a red unit lane.
func TestConstraintProfileNeverAnswersOutsideTheProfile(t *testing.T) {
	mapping := MapValue([]ConstraintEntry{
		{Key: "z", Value: IntValue(1)},
		{Key: "a", Value: IntValue(2)},
		{Key: "m", Value: IntValue(3)},
	})
	class := ClassValue("Probe", []ConstraintEntry{
		{Key: "b", Value: IntValue(1)},
		{Key: "a", Value: StringValue("x")},
	})

	cases := []struct {
		name string
		this ConstraintValue
		expr string
		why  string
	}{
		// Representation-sensitive over a mapping — caught by the agreement
		// check, not by any per-filter guard.
		{"membership_map", mapping, `"z" in this`, "`in` is invisible to the ordered projection"},
		{"membership_class", class, `"a" in this`, "same, class-shaped"},
		{"iteration_order", mapping, `this|list|join(",") == "z,a,m"`, "order differs between projections"},
		{"first", mapping, `this|first == "z"`, "idem"},
		{"keys_method", mapping, `this.keys()|join(",") == "z,a,m"`, "pycompat map methods exist on only one projection"},
		{"render", mapping, `(this|string)[2] == "z"`, "rendering embeds the order"},
		{"concat", mapping, `(this ~ "")[2] == "z"`, "`~` renders, and is an operator with no filter hook"},

		// Shapes wrong in EVERY representation — caught by an explicit guard.
		{"length_none", NullValue(), "none|length == 0", "minijinja rejects a lengthless value"},
		{"length_undefined", NullValue(), "nosuchvar|length == 0", "idem"},
		{"split", StringValue("a,b"), `this|split(",")|length == 2`, "minijinja's split is a lazy iterator"},
		{"last_map", mapping, `this|last == "m"`, "minijinja rejects a mapping"},
		{"items_map", mapping, "this|items|length == 3", "items sorts here, preserves order there"},
		{"tojson_map", mapping, `(this|tojson)[2] == "z"`, "idem"},
		{"tojson_nested", NullValue(), `[{"a":1}]|tojson == "x"`, "a nested mapping is equally order-bearing"},
		{"maplit_order", NullValue(), `({"z":1,"a":2}|list|join(",")) == "z,a"`, "a mapping literal is minijinja-Go's own, enumerated sorted"},
		{"divisibleby_zero", IntValue(1), "this is divisibleby(0)", "stock BAML aborts the process"},

		// Contract narrowing: the pycompat string/sequence surface has no hook
		// in minijinja-Go, so it is outside the profile BY CONSTRUCTION.
		{"pycompat_upper", NullValue(), `"abc".upper() == "ABC"`, "no unknown-method callback"},
		{"pycompat_format", NullValue(), `"{:,}".format(1234567) == "1,234,567"`, "idem"},
		{"pycompat_seq_count", NullValue(), "[1,1].count(1) == 2", "idem"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateConstraint(tc.this, tc.expr)
			if err == nil {
				t.Fatalf("%q answered %v; it is outside the profile (%s) and must refuse", tc.expr, got, tc.why)
			}
			if !errors.Is(err, ErrConstraintUnsupported) {
				t.Fatalf("%q refused with %v, which does not wrap ErrConstraintUnsupported", tc.expr, err)
			}
		})
	}
}

// TestConstraintProfileStillAnswersInsideIt is the other half: narrowing must
// not have swallowed the surface the evaluator exists to decide. If any of
// these starts refusing, a guard has become over-broad.
func TestConstraintProfileStillAnswersInsideIt(t *testing.T) {
	mapping := MapValue([]ConstraintEntry{{Key: "z", Value: IntValue(1)}, {Key: "a", Value: IntValue(2)}})
	class := ClassValue("Probe", []ConstraintEntry{{Key: "b", Value: IntValue(1)}, {Key: "a", Value: StringValue("x")}})

	cases := []struct {
		this ConstraintValue
		expr string
		want bool
	}{
		// Mappings keep everything that is representation-independent.
		{mapping, "this|length == 2", true},
		{mapping, `this["z"] == 1`, true},
		{mapping, "this.a == 2", true},
		{mapping, "this is mapping", true},
		{mapping, `this == {"z":1,"a":2}`, true},
		{mapping, `(this|dictsort|first|first) == "a"`, true},
		{mapping, "this.q is undefined", true},
		{class, `this.a == "x"`, true},
		{class, "this|length == 2", true},
		// Everything not involving a mapping is untouched by the profile.
		{IntValue(7), "this > 0", true},
		{StringValue("Hello"), `this|length == 5`, true},
		{StringValue("Hello"), `"ell" in this`, true},
		{ListValue([]ConstraintValue{IntValue(1), IntValue(2)}), "this|sum == 3", true},
		{ListValue([]ConstraintValue{IntValue(1), IntValue(2)}), "1 in this", true},
		{ListValue([]ConstraintValue{IntValue(1), IntValue(2)}), `this|join(",") == "1,2"`, true},
		{EnumValue("Hue", "RED"), `this == "RED"`, true},
		{NullValue(), "this == none", true},
		{NullValue(), "4 is divisibleby(2)", true},
		{NullValue(), `"abc"|length == 3`, true},
		{NullValue(), `[1,2]|tojson == "[1,2]"`, true},
	}
	for _, tc := range cases {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%q over %s refused (%v); it is inside the profile", tc.expr, tc.this.Kind(), err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q over %s = %v, want %v", tc.expr, tc.this.Kind(), got, tc.want)
		}
	}
}

// TestConstraintMediaIsRefused pins the media contract.
//
// BAML's two conversions disagree on this one arm — `Value::from_serialize`
// (the path evaluate_predicate takes) emits the BamlMedia serde document, while
// `From<BamlValue> for minijinja::Value` (the prompt renderer's path) wraps it
// in a magic-marker object — and no media value can reach a constraint on the
// native path to decide between them: schema.Bundle.ValidateOutput rejects
// every media output before parsing ("media is not usable as an output type",
// internal/schema/validate.go:65). Rather than ship an unprovable conversion,
// the profile refuses media, at any depth.
func TestConstraintMediaIsRefused(t *testing.T) {
	mime := "image/png"
	media := MediaValue(ConstraintMedia{
		MediaType: "Image",
		MimeType:  &mime,
		Content: ConstraintMediaContent{
			Tag:    "Url",
			Fields: []ConstraintEntry{{Key: "url", Value: StringValue("https://example.invalid/x.png")}},
		},
	})
	for name, this := range map[string]ConstraintValue{
		"bare":     media,
		"in_list":  ListValue([]ConstraintValue{media}),
		"in_class": ClassValue("Doc", []ConstraintEntry{{Key: "img", Value: media}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EvaluateConstraint(this, "true"); !errors.Is(err, ErrConstraintUnsupported) {
				t.Fatalf("a media-bearing value evaluated (err=%v); media is outside the profile", err)
			}
		})
	}
}

// TestEveryEvaluatorErrorIsTheSentinel pins the contract's totality: there is
// no error path out of the evaluator that a caller could mistake for anything
// other than "decline to BAML".
func TestEveryEvaluatorErrorIsTheSentinel(t *testing.T) {
	for _, expr := range []string{
		"this |",           // compile error
		"1|nosuchfilter",   // unknown filter
		"1 is nosuchtest",  // unknown test
		"nosuchfn()",       // unknown function
		"this",             // non-boolean render
		`"abc".upper()`,    // pycompat
		`"a,b"|split(",")`, // profile guard
		"none|length",      // profile guard
	} {
		_, err := EvaluateConstraint(IntValue(1), expr)
		if err == nil {
			t.Errorf("%q did not error", expr)
			continue
		}
		if !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q returned %v, which does not wrap ErrConstraintUnsupported", expr, err)
		}
	}
}

// TestNumericBoundaryIsRefusedOnEveryArchitecture pins the 64-bit numeric
// boundary, the divergence that made the differential pass on darwin/arm64 and
// FAIL on linux/amd64 (PR #649 CI run 30262325503).
//
// minijinja-Go computes and compares integers as float64 (value/ops.go
// Add/Sub/Mul: `FromInt(int64(f1 - f2))`; Value.Equal compares via AsFloat),
// while minijinja in Rust keeps i64 exact. Two distinct wrong answers follow,
// and this test pins the refusal of both — plus, deliberately, the cases just
// INSIDE the boundary that must still decide, so the guard cannot be widened
// into a blanket ban on arithmetic.
func TestNumericBoundaryIsRefusedOnEveryArchitecture(t *testing.T) {
	refused := []struct {
		expr string
		why  string
	}{
		// Wrong on every architecture: 2^53 and 2^53+1 are the same float64.
		{"9007199254740993 == 9007199254740992", "float64 conflates neighbours past 2^53"},
		{"9007199254740992 == 9007199254740992", "exactly at the boundary"},
		{"9007199254740991 + 1 == 9007199254740992", "the sum reaches the boundary"},
		// Round-6: literal forms the previous hand scanner never modelled, each
		// of which produced a wrong boolean live against stock.
		{"0x20000000000001 == 0x20000000000000", "hexadecimal literals past 2^53"},
		{"9_007_199_254_740_993 == 9_007_199_254_740_992", "underscore-separated literals"},
		{"0b1 + 0b1 == 2", "binary literals are outside the recognised forms"},
		{"1e5 + 1 == 100001", "exponent notation is outside the recognised forms"},
		{"9007199254740991 \n + 1 \n + 1 == 9007199254740991 \n + 1", "newlines must not hide binary operators"},
		{"2 ** -1 == 0.5", "stock converts the exponent to u32 and errors"},
		{"2 ** 0.5 > 1", "a non-integer exponent is not the integer pow stock models"},
		// ARCHITECTURE-DEPENDENT: i64::MAX rounds up to 2^63 as a float64, so
		// int64(f) is out of range — arm64 saturates, amd64 does not.
		{"9223372036854775807 - 1 == 9223372036854775806", "the i64::MAX case that split arm64 from amd64"},
		{"0 - 9223372036854775807 == -9223372036854775807", "same, negative"},
		// The result escapes even though both operands are exact.
		{"3037000500 * 3037000500 > 9007199254740992", "the product escapes the exact range"},
		{"2 ** 62 > 0", "exponentiation can escape from small operands"},
	}
	for _, tc := range refused {
		if got, err := EvaluateConstraint(NullValue(), tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered %v (err=%v); it must be refused — %s", tc.expr, got, err, tc.why)
		}
	}

	// A large integer arriving through the VALUE, not a literal, is equally
	// unsafe and equally refused.
	if _, err := EvaluateConstraint(IntValue(9007199254740993), "this > 0"); !errors.Is(err, ErrConstraintUnsupported) {
		t.Errorf("a value-model integer past 2^53 must be refused; got err=%v", err)
	}
	if _, err := EvaluateConstraint(ListValue([]ConstraintValue{IntValue(1), IntValue(1 << 60)}), "this|length == 2"); !errors.Is(err, ErrConstraintUnsupported) {
		t.Errorf("a large integer nested in a list must be refused; got err=%v", err)
	}

	// Just inside the boundary, and arithmetic that provably cannot escape it,
	// must still DECIDE — otherwise the guard has swallowed the surface the
	// evaluator exists for.
	// The bound is OPERAND-AWARE: with a single growing operator whose operands
	// are integer literals, the operation is evaluated exactly rather than
	// estimated from the largest literal in sight. `2 ** 10` is 1024, not the
	// 10^10 a global-maximum estimate would guess, so it stays decidable — and
	// stays in live agreement with stock, which is what the corpus checks.
	decided := map[string]bool{
		"1000 * 1000 == 1000000":             true,
		"3000000 * 3000000 == 9000000000000": true,
		"2 ** 3 == 8":                        true,
		"2 ** 10 == 1024":                    true, // operand-aware: 2^10, not 10^10
		"1 + 1 == 2":                         true,
		"7 // 2 == 3":                        true,
		"0.1 + 0.2 == 0.3":                   false,
	}
	for expr, want := range decided {
		got, err := EvaluateConstraint(IntValue(7), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); it is inside the exact range", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}

// TestRuntimeIntegerProducersAreRefused pins the round-4 P1 hole: an integer
// manufactured DURING evaluation is invisible to the static magnitude bound.
//
// The reachable case is the `int` filter. minijinja-Go's FilterInt parses a
// string with strconv.ParseInt(s, 10, 64), so `"9007199254740993"|int` is an
// exact int64 — and then Value.Equal compares it through AsFloat, where it
// collapses onto its neighbour. minijinja's `int` parses i128 and compares
// exactly. Stock BAML v0.223 answers FALSE for the first case below; native
// answered true before [guardIntegerResult] existed.
//
// Nothing here is about the `int` filter specifically: the guard is on the
// VALUE a filter returns, so it closes the same hole for a string-valued
// `this`, for elements reached through `map("int")`, and for producers nobody
// has enumerated.
func TestRuntimeIntegerProducersAreRefused(t *testing.T) {
	refused := []struct {
		this ConstraintValue
		expr string
	}{
		{NullValue(), `"9007199254740993"|int == "9007199254740992"|int`},
		{NullValue(), `["9007199254740993"]|map("int")|first > 0`},
		{NullValue(), `"9007199254740993"|int|abs > 0`},
		// The same integer arriving through the VALUE rather than a literal.
		{StringValue("9007199254740993"), "this|int > 0"},
		{ListValue([]ConstraintValue{StringValue("9007199254740993")}), `this|map("int")|first > 0`},
	}
	for _, tc := range refused {
		if got, err := EvaluateConstraint(tc.this, tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered %v (err=%v); it manufactures an out-of-range integer and must be refused",
				tc.expr, got, err)
		}
	}

	// Controls. The guard is on the MAGNITUDE produced, not on the filter, so
	// ordinary conversions must still decide — otherwise `int` would have been
	// withdrawn rather than guarded.
	decided := []struct {
		this ConstraintValue
		expr string
		want bool
	}{
		{NullValue(), `"42"|int == 42`, true},
		{StringValue("42"), "this|int == 42", true},
		{NullValue(), `"2.9"|int == 2`, true},
		{NullValue(), "[1,2]|sum == 3", true},
		{NullValue(), "[1,2]|max == 2", true},
	}
	for _, tc := range decided {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%q was refused (%v); it is inside the profile", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, got, tc.want)
		}
	}

	// ACCEPTED COST of the round-6 whitelist. These are safe in themselves, but
	// they combine arithmetic with syntax the closed numeric grammar does not
	// accept, or they mention a 16-digit token the literal recogniser will not
	// vouch for. Refusing them is the price of never allowing an unmodelled
	// form, and it is pinned so the cost stays visible.
	for _, tc := range []struct {
		this ConstraintValue
		expr string
	}{
		{NullValue(), "-1|abs + 1 == 2"},
		{NullValue(), `"9007199254740993"|float == "9007199254740992"|float`},
		{ClassValue("P", []ConstraintEntry{{Key: "n", Value: IntValue(7)}}), "this.n + 1 == 8"},
		{ListValue([]ConstraintValue{IntValue(1), IntValue(2)}), "this[0] + 1 == 2"},
		{IntValue(7), "this + 1 == 8"},
	} {
		if _, err := EvaluateConstraint(tc.this, tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q is expected to be refused as an accepted cost of the whitelist; got err=%v",
				tc.expr, err)
		}
	}
}

// TestArithmeticMixedWithOtherSyntaxIsRefused pins the reason the numeric
// profile is a WHOLE-EXPRESSION whitelist rather than a per-operand rule.
//
// A runtime integer produced JUST INSIDE the exact range can cross 2^53 in a
// LATER operator, and minijinja-Go offers no post-operator hook. The reachable
// demonstration, live against stock:
//
//	this = "9007199254740991"   (2^53 - 1)
//	this|int + 1 + 1 == this|int + 1     native (before) true, stock FALSE
//
// `int` legitimately yields 2^53-1, so guarding the producer does not help; the
// source holds only literal 1s, so bounding their magnitudes does not either.
// The profile therefore admits arithmetic ONLY when the whole expression parses
// as the closed numeric sublanguage — literals, operators, parentheses,
// comparisons and nothing else. A filter, call or identifier anywhere in the
// expression ends that parse, so every case below is refused by construction.
func TestArithmeticMixedWithOtherSyntaxIsRefused(t *testing.T) {
	exact := StringValue("9007199254740991")
	numStrings := ListValue([]ConstraintValue{StringValue("9007199254740991"), StringValue("1")})
	bigInts := ListValue([]ConstraintValue{IntValue(4503599627370495), IntValue(4503599627370496)})
	cls := ClassValue("P", []ConstraintEntry{{Key: "n", Value: IntValue(7)}})

	for _, tc := range []struct {
		name string
		this ConstraintValue
		expr string
	}{
		{"reviewer_case", exact, "this|int + 1 + 1 == this|int + 1"},
		{"multiply", exact, "this|int * 2 > 0"},
		{"power", exact, "this|int ** 2 > 0"},
		{"subtract", exact, "this|int - 1 + 2 == this|int + 1"},
		{"sum", bigInts, "this|sum + 1 == this|sum"},
		{"max", bigInts, "this|max + 1 + 1 == this|max + 1"},
		{"min", bigInts, "this|min * 4 > 0"},
		{"abs_chain", exact, "this|int|abs + 1 + 1 == this|int|abs + 1"},
		{"map_sum", numStrings, `this|map("int")|sum + 1 + 1 == this|map("int")|sum + 1`},
		{"map_first", numStrings, `this|map("int")|first + 1 + 1 == this|map("int")|first + 1`},
		{"nested_chain", numStrings, `this|map("int")|max|abs + 1 + 1 == this|map("int")|max|abs + 1`},
		{"attr_filter", cls, `this|attr("n") + 1 == 8`},
		{"round_filter", FloatValue(2.5), "this|round + 1 == 4"},
		{"call_result", NullValue(), "range(3)|length + 1 == 4"},
		// Over-refusals the rule accepts: these happen to be small, but their
		// operands are filter results, which the rule cannot bound.
		{"length_small", NullValue(), "[1,2]|length + 1 == 3"},
		{"abs_small", NullValue(), "-1|abs + 1 == 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := EvaluateConstraint(tc.this, tc.expr); !errors.Is(err, ErrConstraintUnsupported) {
				t.Fatalf("%q answered %v (err=%v); a runtime integer reaches an arithmetic operator here",
					tc.expr, got, err)
			}
		})
	}
}

// TestPurelyNumericArithmeticStillDecides is the other half: the whitelist
// narrows arithmetic to the closed numeric sublanguage, and leaves everything
// outside arithmetic alone.
//
// NOTE ON SCOPE, corrected in round 8. An earlier version of this test claimed
// arithmetic over VALUE-MODEL integers still decides — `this + 1 == 8`. It does
// not, and has not since the whitelist landed: `this` is an identifier, so the
// numeric grammar rejects the expression. The accepted-cost block below asserts
// that refusal rather than the reverse. What survives is arithmetic over pure
// literals, and filters used WITHOUT arithmetic.
func TestPurelyNumericArithmeticStillDecides(t *testing.T) {
	for _, tc := range []struct {
		this ConstraintValue
		expr string
	}{
		{NullValue(), "2 ** 10 == 1024"},
		{NullValue(), "2 ** (10) == 1024"}, // parenthesised operands, round-6 P2
		{NullValue(), "((2)) ** ((10)) == 1024"},
		{NullValue(), "(1 + 2) * 3 == 9"},
		{NullValue(), "1000 * 1000 == 1000000"},
		{NullValue(), "3000000 * 3000000 == 9000000000000"},
		{NullValue(), "1 + 1 == 2"},
		{NullValue(), "1 \n + 1 == 2"}, // newlines are whitespace, round-6 P1.2
		// Filters are fine on their own — it is only their composition with
		// arithmetic that cannot be bounded.
		{NullValue(), "[1,2]|sum == 3"},
		{NullValue(), `"2"|int == 2`},
		{IntValue(7), "this|abs == 7"},
	} {
		got, err := EvaluateConstraint(tc.this, tc.expr)
		if err != nil {
			t.Errorf("%q over %s was refused (%v); the proven-parity whitelist has become over-broad — "+
				"this is a form it is supposed to ADMIT",
				tc.expr, tc.this.Kind(), err)
			continue
		}
		if !got {
			t.Errorf("%q over %s = false, want true", tc.expr, tc.this.Kind())
		}
	}
}

// TestPowerBoundTerminates is a LIVENESS regression test.
//
// The round-4 operand-aware bound computed the exact power with a linear loop,
// so `1 ** 9007199254740991` — a valid expression — spun roughly nine
// quadrillion times BEFORE the template was compiled, because multiplying by 1
// never saturates. satPow is now exponentiation by squaring with explicit 0/1
// bases, and an exponent beyond what stock's u32 conversion accepts is refused
// outright rather than evaluated (minijinja-Go would call math.Pow and answer
// where stock errors).
//
// The deadline is what makes this a regression test rather than a description:
// the previous implementation could not have finished inside it.
func TestPowerBoundTerminates(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, expr := range []string{
			"1 ** 9007199254740991 == 1",
			"0 ** 9007199254740991 == 0",
			"2 ** 9007199254740991 > 0",
			"9007199254740991 ** 9007199254740991 > 0",
			"1 ** 4294967296 == 1",
		} {
			if _, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
				t.Errorf("%q must be refused: stock rejects an exponent this large", expr)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the numeric bound did not terminate within 10s; satPow has regressed to a linear loop")
	}
}

// TestNumericProfileIsAWhitelist pins the ROUND-6 inversion: the numeric bound
// refuses everything it does not positively recognise.
//
// Rounds 2-5 bounded numerics with a hand scanner, and each scanner failed OPEN
// on a form it did not model — hexadecimal and underscored literals, newlines,
// parenthesised operands, negative exponents. Deriving the bound from
// minijinja-Go's real tokenizer is not possible (its lexer and parser are under
// `internal/`, which Go forbids this package from importing, and no AST is
// exported), so the default was inverted instead: a closed numeric sublanguage
// is recognised and evaluated exactly, and everything else is refused.
//
// This test is the statement of that property — unrecognised INPUT SHAPES, not
// just unrecognised magnitudes, must refuse.
func TestNumericProfileIsAWhitelist(t *testing.T) {
	// Shapes the grammar does not accept. None involves a large value; they are
	// refused for being unrecognised, which is the point.
	for _, expr := range []string{
		"0b1 + 0b1 == 2",                   // binary literal
		"0o7 + 1 == 8",                     // octal literal
		"0x1 + 1 == 2",                     // hex literal
		"1_0 + 1 == 11",                    // digit separator
		"1e2 + 1 == 101",                   // exponent notation
		`"a" ~ "b" == "ab" and 1 + 1 == 2`, // arithmetic mixed with non-numeric syntax
		"[1,2]|length + 1 == 3",            // arithmetic over a filter result
		"1 {# comment #} + 1 == 2",         // a comment inside arithmetic
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered %v (err=%v); the grammar does not recognise it and it must refuse",
				expr, got, err)
		}
	}

	// `range` is withdrawn: minijinja-Go exports no handle on its globals, so it
	// cannot be wrapped by the integer-result guard.
	for _, expr := range []string{"range(3)|length == 3", "range(3)|last == 2"} {
		if _, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q must refuse: `range` is outside the profile", expr)
		}
	}

	// Shapes the grammar DOES accept must still be evaluated on their real
	// operands, including through parentheses and across newlines.
	for expr, want := range map[string]bool{
		"2 ** (10) == 1024":       true,
		"((2)) ** ((10)) == 1024": true,
		"(1 + 2) * 3 == 9":        true,
		"1 \n + 1 == 2":           true,
		"1 \r\n + 1 == 2":         true,
		"7 % 3 == 1":              true,
		"7 // 2 == 3":             true,
		"-1 + 2 == 1":             true,
		"0.1 + 0.2 == 0.3":        false,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); the grammar accepts it", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}

// TestProvenParityWhitelistForSignedOps pins the round-7 class: two operators
// whose minijinja-Go and stock v0.223 semantics differ for reasons that have
// nothing to do with 2^53, and which the closed sublanguage previously admitted.
//
//	minijinja-Go Value.Rem      = Go's truncated `%`   (-1 % 2 == -1)
//	stock v2.16                 = checked_rem_euclid   (-1 % 2 ==  1)
//	minijinja-Go Value.FloorDiv = math.Floor(a/b)      ( 1 // -2 == -1)
//	stock v2.16                 = checked_div_euclid   ( 1 // -2 ==  0)
//	minijinja-Go Value.Pow      = math.Pow             ( 2 ** -1 == 0.5)
//	stock v2.16                 = u32 integer pow      ( 2 ** -1 ERRORS)
//
// So the sublanguage moved to a proven-parity posture: `//` and `%` require
// non-negative literal operands, and `**` requires a non-negative integer
// LITERAL exponent. Tracking a sign flag through unary syntax was not enough —
// `2 ** (0 - 1)` computes -1 with no unary minus anywhere — and proving the sign
// of a computed operand is deliberately not attempted.
func TestProvenParityWhitelistForSignedOps(t *testing.T) {
	for _, expr := range []string{
		// Exponents that are computed, parenthesised-negative, or non-literal.
		"2 ** (0 - 1) == 0.5", "2 ** (1 - 3) == 0.25", "2 ** (-1) == 0.5",
		"2 ** -1 == 0.5", "2 ** (1 + 1) == 4", "2 ** (4 // 2) == 4",
		"2 ** (5 % 3) == 4", "2 ** 3 ** 2 == 512", "2 ** 0.5 > 1",
		// Signed `//` and `%`, whether written or computed.
		"-1 % 2 == 1", "7 % -3 == 1", "(0 - 1) % 2 == 1", "-7 % 3 == 2",
		"1 // -2 == 0", "-7 // 2 == -4", "1 // (0 - 2) == 0", "(1 - 2) // 3 == -1",
		// Chained: the left operand is a computed value, not a literal.
		"7 // 2 // 1 == 3", "7 % 3 % 2 == 1",
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered %v (err=%v); the engines can differ on this form and it must refuse",
				expr, got, err)
		}
	}

	// The proven forms — non-negative literal operands, parenthesised or not —
	// must still decide, so the whitelist narrows the sign space and nothing else.
	for expr, want := range map[string]bool{
		"7 % 3 == 1":              true,
		"(7) % (3) == 1":          true,
		"7 // 2 == 3":             true,
		"(7) // (2) == 3":         true,
		"2 ** 10 == 1024":         true,
		"2 ** (10) == 1024":       true,
		"((2)) ** ((10)) == 1024": true,
		"2 ** 0 == 1":             true,
		// True division is f64 on both sides, so sign is immaterial there.
		"7 / 2 == 3.5":   true,
		"-7 / 2 == -3.5": true,
		// `+`, `-`, `*` are exact below 2^53 on both sides at any sign.
		"-1 + 2 == 1":      true,
		"1 - 3 == -2":      true,
		"(1 + 2) * 3 == 9": true,
		"1 \r\n + 1 == 2":  true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); it is proven identical to stock", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}

// TestAdditiveBoundIsTheSumNotTheMax pins the round-8 P1.3 hole.
//
// The additive magnitude bound used to be max(|a|, |b|), which is only an upper
// bound when the operands share a sign. Subtracting a NEGATIVE grows by their
// sum — `a - (-k) == a + k` — so a long chain crossed 2^53 while the retained
// bound stayed at k. Live against stock, with k = 999999999999999 and ten
// repetitions, the two chains differ (9999999999999991 vs 9999999999999992) and
// stock returns false; minijinja-Go's float64 Sub collapsed both onto the same
// value and returned true.
//
// The bound is now |a| + |b| for BOTH `+` and `-`. The signs are tracked
// syntactically but deliberately not relied on here: a chain's signs cannot be
// established without evaluating it, so the bound stays conservative.
func TestAdditiveBoundIsTheSumNotTheMax(t *testing.T) {
	const k = "999999999999999"
	chain := func(start string, n int) string {
		return "(" + start + strings.Repeat(" - (-"+k+")", n) + ")"
	}

	for _, expr := range []string{
		// The reviewer's exact case.
		chain("1", 10) + " == " + chain("2", 10),
		// The same growth reaching a comparison, and feeding a later operator.
		chain("1", 10) + " > 0",
		"(1 - (-" + k + ")) * 100 > 0",
		"2 ** (1 - (-" + k + ")) > 0",
		// A positive chain has always been bounded correctly; it is here so the
		// two directions are pinned together.
		"1 + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " + " + k + " > 0",
		// Two operands that are individually in range and jointly are not.
		"4503599627370496 + 4503599627370496 == 9007199254740992",
		"9007199254740991 - (-1) == 9007199254740992",
	} {
		if got, err := EvaluateConstraint(NullValue(), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("answered %v (err=%v) for a chain that can pass 2^53: %.80s", got, err, expr)
		}
	}

	// The sum bound must not swallow ordinary arithmetic: |a| + |b| stays tiny
	// for small operands whatever their signs.
	for expr, want := range map[string]bool{
		"1 + 2 == 3":                   true,
		"5 - 3 == 2":                   true,
		"1 - 3 == -2":                  true,
		"-1 + 2 == 1":                  true,
		"1 - (-2) == 3":                true,
		"7 - 2 - 1 == 4":               true,
		"1000000 + 1000000 == 2000000": true,
		"(1 + 2) * 3 == 9":             true,
	} {
		got, err := EvaluateConstraint(NullValue(), expr)
		if err != nil {
			t.Errorf("%q was refused (%v); the sum bound has become over-broad", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
	}
}
