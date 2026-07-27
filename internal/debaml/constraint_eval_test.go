package debaml

import (
	"encoding/json"
	"strings"
	"testing"
)

// Unit-lane cover for the constraint evaluator. The AUTHORITATIVE proof is the
// stock BAML v0.223.0 differential in internal/debaml/constraintoracle (310
// cases, //go:build integration). These tests are the parts of that claim that
// can be pinned without CGO and a generated client, so they run in the ordinary
// `go test ./...` lane and catch a regression before anyone reaches for the
// oracle:
//
//   - BAML's OWN jinja_helpers.rs unit tests (:102-218);
//   - the evaluate_predicate contract (exact "true"/"false", else an error);
//   - the value model's insertion-ordered mappings and enum-to-string erasure;
//   - the five builtins withdrawn because BAML's minijinja build lacks them.

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
		{"regex_phone", phone, `this|regex_match("\\(?\\d{3}\\)?[-.\\s]?\\d{3}[-.\\s]?\\d{4}")`, "true"},
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

// TestConstraintValueMappingOrder pins the reason maps and classes are projected
// as an ordered object rather than minijinja-Go's native mapping: BAML's
// BamlMap is an IndexMap and its insertion order is observable inside the
// predicate, while a Go map enumerates SORTED.
//
// "z","a","m" is deliberately neither sorted nor reverse-sorted, so a sorted
// implementation cannot pass by accident.
func TestConstraintValueMappingOrder(t *testing.T) {
	entries := []ConstraintEntry{
		{Key: "z", Value: IntValue(1)},
		{Key: "a", Value: IntValue(2)},
		{Key: "m", Value: IntValue(3)},
	}
	for _, this := range []ConstraintValue{MapValue(entries), ClassValue("Probe", entries)} {
		t.Run(this.Kind().String(), func(t *testing.T) {
			for expr, want := range map[string]string{
				`this|list|join(",")`:     "z,a,m",
				`this.keys()|join(",")`:   "z,a,m",
				`this.values()|join(",")`: "1,2,3",
				`this|length`:             "3",
				`this.z`:                  "1",
				`this["m"]`:               "3",
				`this.get("q", 9)`:        "9",
				`this|string`:             `{"z": 1, "a": 2, "m": 3}`,
			} {
				got, err := RenderConstraintExpression(this, expr)
				if err != nil {
					t.Fatalf("render %q: %v", expr, err)
				}
				if got != want {
					t.Errorf("render %q = %q, want %q", expr, got, want)
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
	// The name reaches neither the value nor the expression namespace.
	for _, expr := range []string{`this|list|join(",")`, `this|string`} {
		got, err := RenderConstraintExpression(cls, expr)
		if err != nil {
			t.Fatalf("render %q: %v", expr, err)
		}
		if strings.Contains(got, "Probe") {
			t.Errorf("render %q = %q, which leaks the class name", expr, got)
		}
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
		`"abc"|length == 3`, `range(3)|length == 3`, `dict(a=1)|length == 1`,
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
