//go:build integration

package constraintoracle

import "github.com/invakid404/baml-rest/internal/debaml"

// The corpus. It was generated once from the discovery run that measured both
// legs, and is hand-maintained from here on: adding a case means adding a row
// and regenerating the .baml fixture and the stock client. See the package
// comment in oracle_integration_test.go for what this proves and how.

// legOutcome is what one leg produced for one case.
type legOutcome string

const (
	// outTrue / outFalse: the predicate rendered exactly "true" / "false".
	outTrue  legOutcome = "true"
	outFalse legOutcome = "false"
	// outError: the expression failed to compile or evaluate, or rendered
	// something that is not a boolean. BAML turns this into a coercion failure
	// of the constrained node; native returns an error from EvaluateConstraint.
	outError legOutcome = "error"
	// outUnsupported: NATIVE ONLY. The evaluator refused the expression with
	// ErrConstraintUnsupported, i.e. it declined to decide rather than risk a
	// boolean BAML would not have produced. This is the ONLY outcome native is
	// allowed to have when it does not match stock exactly.
	outUnsupported legOutcome = "unsupported"
	// outNoChecks: stock emitted the value with NO checks map entry at all.
	// Observed only for an optional field whose predicate errored — the optional
	// coercion swallows the failure and yields null instead of rejecting.
	outNoChecks legOutcome = "no-checks"
)

// constraintGroup binds one `this` value: the BAML field type that carries the
// checks, the assistant text that produces the value, and the native
// ConstraintValue that must model the same thing.
type constraintGroup struct {
	Name     string
	BAMLType string
	// Input is the full assistant text the oracle feeds Parse.<Method>: the
	// one-field object {"v": <value>} the fixture classes are shaped for.
	Input string
	This  debaml.ConstraintValue
}

// constraintCase is one (expression, `this`) observation.
type constraintCase struct {
	// Label is the case id AND the @check label, so a batched check's result is
	// recoverable from the checks map by name.
	Label string
	Group string
	// Expr is the text written into the .baml @check(...) attribute.
	Expr string
	// Retained is the JinjaExpression BAML actually evaluates, as reported back
	// in Check.Expression. Empty means identical to Expr. It differs only where
	// BAML's attribute lexer DOUBLES backslashes, which is pinned by the drift
	// check and is why a BAML constraint cannot express a regex escape.
	Retained string
	// Stock and Native are the pinned outcomes of the two legs. The INVARIANT
	// (TestConstraintProfileIsFailClosed) is that Native is either exactly
	// Stock's outcome or outUnsupported — never a different boolean. A Native
	// of outUnsupported where stock ANSWERED is the measured cost of the
	// profile and carries a Note naming the guard that refused it.
	Stock  legOutcome
	Native legOutcome
	Note   string
}

// bamlPrelude carries the shared declarations the group field types reference.
// bamlPrelude carries the shared declarations the group field types reference.
const bamlPrelude = `
class Probe {
  b int
  a string
  c int[]
}

enum Hue {
  RED @alias("rouge")
  GREEN
}
`

var constraintGroups = []constraintGroup{
	{Name: "const", BAMLType: "int", Input: "{\"v\":1}", This: debaml.IntValue(1)},
	{Name: "int7", BAMLType: "int", Input: "{\"v\":7}", This: debaml.IntValue(7)},
	{Name: "intneg", BAMLType: "int", Input: "{\"v\":-3}", This: debaml.IntValue(-3)},
	{Name: "int0", BAMLType: "int", Input: "{\"v\":0}", This: debaml.IntValue(0)},
	{Name: "f25", BAMLType: "float", Input: "{\"v\":2.5}", This: debaml.FloatValue(2.5)},
	{Name: "f20", BAMLType: "float", Input: "{\"v\":2.0}", This: debaml.FloatValue(2.0)},
	{Name: "str", BAMLType: "string", Input: "{\"v\":\"Hello World\"}", This: debaml.StringValue("Hello World")},
	{Name: "stre", BAMLType: "string", Input: "{\"v\":\"\"}", This: debaml.StringValue("")},
	{Name: "stru", BAMLType: "string", Input: "{\"v\":\"h\\u00e9llo \\u2713 \\u65e5\\u672c\"}", This: debaml.StringValue("h\u00e9llo \u2713 \u65e5\u672c")},
	{Name: "boolt", BAMLType: "bool", Input: "{\"v\":true}", This: debaml.BoolValue(true)},
	{Name: "boolf", BAMLType: "bool", Input: "{\"v\":false}", This: debaml.BoolValue(false)},
	{Name: "list", BAMLType: "int[]", Input: "{\"v\":[1,2,3]}", This: debaml.ListValue([]debaml.ConstraintValue{debaml.IntValue(1), debaml.IntValue(2), debaml.IntValue(3)})},
	{Name: "liste", BAMLType: "int[]", Input: "{\"v\":[]}", This: debaml.ListValue([]debaml.ConstraintValue{})},
	{Name: "listf", BAMLType: "float[]", Input: "{\"v\":[1,2.5]}", This: debaml.ListValue([]debaml.ConstraintValue{debaml.FloatValue(1.0), debaml.FloatValue(2.5)})},
	{Name: "lists", BAMLType: "string[]", Input: "{\"v\":[\"a\",\"b\"]}", This: debaml.ListValue([]debaml.ConstraintValue{debaml.StringValue("a"), debaml.StringValue("b")})},
	{Name: "map", BAMLType: "map<string, int>", Input: "{\"v\":{\"z\":1,\"a\":2,\"m\":3}}", This: debaml.MapValue([]debaml.ConstraintEntry{{Key: "z", Value: debaml.IntValue(1)}, {Key: "a", Value: debaml.IntValue(2)}, {Key: "m", Value: debaml.IntValue(3)}})},
	{Name: "cls", BAMLType: "Probe", Input: "{\"v\":{\"b\":1,\"a\":\"x\",\"c\":[1,2]}}", This: debaml.ClassValue("Probe", []debaml.ConstraintEntry{{Key: "b", Value: debaml.IntValue(1)}, {Key: "a", Value: debaml.StringValue("x")}, {Key: "c", Value: debaml.ListValue([]debaml.ConstraintValue{debaml.IntValue(1), debaml.IntValue(2)})}})},
	{Name: "enum", BAMLType: "Hue", Input: "{\"v\":\"rouge\"}", This: debaml.EnumValue("Hue", "RED")},
	{Name: "null", BAMLType: "int?", Input: "{\"v\":null}", This: debaml.NullValue()},
}

var constraintCases = []constraintCase{
	{Label: "op_add", Group: "const", Expr: "1 + 1 == 2", Stock: outTrue, Native: outTrue},
	{Label: "op_sub", Group: "const", Expr: "1 - 2 == -1", Stock: outTrue, Native: outTrue},
	{Label: "op_mul", Group: "const", Expr: "2 * 3 == 6", Stock: outTrue, Native: outTrue},
	{Label: "op_div", Group: "const", Expr: "7 / 2 == 3.5", Stock: outTrue, Native: outTrue},
	{Label: "op_div_int", Group: "const", Expr: "4 / 2 == 2", Stock: outTrue, Native: outTrue},
	{Label: "op_floordiv", Group: "const", Expr: "7 // 2 == 3", Stock: outTrue, Native: outTrue},
	{Label: "op_rem", Group: "const", Expr: "7 % 3 == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_pow", Group: "const", Expr: "2 ** 10 == 1024", Stock: outTrue, Native: outTrue},
	{Label: "op_neg", Group: "const", Expr: "-3 < 0", Stock: outTrue, Native: outTrue},
	{Label: "op_not", Group: "const", Expr: "not false", Stock: outTrue, Native: outTrue},
	{Label: "op_and", Group: "const", Expr: "(true and false) == false", Stock: outTrue, Native: outTrue},
	{Label: "op_or", Group: "const", Expr: "true or false", Stock: outTrue, Native: outTrue},
	{Label: "op_lt", Group: "const", Expr: "1 < 2", Stock: outTrue, Native: outTrue},
	{Label: "op_le", Group: "const", Expr: "2 <= 2", Stock: outTrue, Native: outTrue},
	{Label: "op_gt", Group: "const", Expr: "3 > 2", Stock: outTrue, Native: outTrue},
	{Label: "op_ge", Group: "const", Expr: "3 >= 3", Stock: outTrue, Native: outTrue},
	{Label: "op_eq", Group: "const", Expr: "1 == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_ne", Group: "const", Expr: "1 != 2", Stock: outTrue, Native: outTrue},
	{Label: "op_concat", Group: "const", Expr: "\"a\" ~ \"b\" == \"ab\"", Stock: outTrue, Native: outTrue},
	{Label: "op_concat_num", Group: "const", Expr: "1 ~ 2 == \"12\"", Stock: outTrue, Native: outTrue},
	{Label: "op_in_list", Group: "const", Expr: "1 in [1,2]", Stock: outTrue, Native: outTrue},
	{Label: "op_notin_list", Group: "const", Expr: "4 not in [1,2]", Stock: outTrue, Native: outTrue},
	{Label: "op_in_str", Group: "const", Expr: "\"ell\" in \"hello\"", Stock: outTrue, Native: outTrue},
	{Label: "op_in_maplit", Group: "const", Expr: "\"a\" in {\"a\": 1}", Stock: outTrue, Native: outTrue},
	{Label: "op_eq_list", Group: "const", Expr: "[1,2] == [1,2]", Stock: outTrue, Native: outTrue},
	{Label: "op_eq_map", Group: "const", Expr: "{\"a\":1} == {\"a\":1}", Stock: outTrue, Native: outTrue},
	{Label: "op_ternary", Group: "const", Expr: "(1 if true else 2) == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_float_int_eq", Group: "const", Expr: "1.0 == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_str_num_eq", Group: "const", Expr: "\"1\" == 1", Stock: outFalse, Native: outFalse},
	{Label: "op_bool_num_eq", Group: "const", Expr: "true == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_none_eq", Group: "const", Expr: "none == none", Stock: outTrue, Native: outTrue},
	{Label: "op_index", Group: "const", Expr: "[1,2][0] == 1", Stock: outTrue, Native: outTrue},
	{Label: "op_index_neg", Group: "const", Expr: "[1,2,3][-1] == 3", Stock: outTrue, Native: outTrue},
	{Label: "op_slice", Group: "const", Expr: "[1,2,3][1:] == [2,3]", Stock: outTrue, Native: outTrue},
	{Label: "op_str_index", Group: "const", Expr: "\"abc\"[0] == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "op_mixed_arith", Group: "const", Expr: "1 + 1.5 == 2.5", Stock: outTrue, Native: outTrue},
	{Label: "op_i64max", Group: "const", Expr: "9223372036854775807 - 1 == 9223372036854775806", Stock: outTrue, Native: outTrue},
	{Label: "op_float_assoc", Group: "const", Expr: "0.1 + 0.2 == 0.3", Stock: outFalse, Native: outFalse},
	{Label: "op_div_zero", Group: "const", Expr: "(1 / 0) > 0", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 invalid operation: division by zero (at <string> line 1)"},
	{Label: "op_floordiv_zero", Group: "const", Expr: "(1 // 0) > 0", Stock: outError, Native: outUnsupported},
	{Label: "op_rem_zero", Group: "const", Expr: "(1 % 0) > 0", Stock: outError, Native: outUnsupported},
	{Label: "op_str_gt_num", Group: "const", Expr: "\"x\" > 0", Stock: outTrue, Native: outTrue},
	{Label: "op_num_lt_list", Group: "const", Expr: "1 < [1]", Stock: outTrue, Native: outTrue},
	{Label: "op_undefined_cmp", Group: "const", Expr: "nosuchvar > 0", Stock: outFalse, Native: outFalse},
	{Label: "op_undefined_attr", Group: "const", Expr: "this.nope > 0", Stock: outFalse, Native: outFalse},
	{Label: "op_str_add_num", Group: "const", Expr: "(\"a\" + 1) == \"a1\"", Stock: outError, Native: outUnsupported},
	{Label: "f_length_str", Group: "const", Expr: "\"abc\"|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_length_list", Group: "const", Expr: "[1,2,3]|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_length_map", Group: "const", Expr: "{\"a\":1,\"b\":2}|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_count_alias", Group: "const", Expr: "\"abc\"|count == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_lower", Group: "const", Expr: "\"ABC\"|lower == \"abc\"", Stock: outTrue, Native: outTrue},
	{Label: "f_upper", Group: "const", Expr: "\"abc\"|upper == \"ABC\"", Stock: outTrue, Native: outTrue},
	{Label: "f_capitalize", Group: "const", Expr: "\"abc\"|capitalize == \"Abc\"", Stock: outTrue, Native: outTrue},
	{Label: "f_title", Group: "const", Expr: "\"a b\"|title == \"A B\"", Stock: outTrue, Native: outTrue},
	{Label: "f_trim", Group: "const", Expr: "\"  a \"|trim == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "f_replace", Group: "const", Expr: "\"aXa\"|replace(\"X\",\"-\") == \"a-a\"", Stock: outTrue, Native: outTrue},
	{Label: "f_format", Group: "const", Expr: "\"%s!\"|format(\"hi\") == \"hi!\"", Stock: outTrue, Native: outTrue},
	{Label: "f_default_present", Group: "const", Expr: "1|default(2) == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_default_missing", Group: "const", Expr: "nosuchvar|default(2) == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_d_alias", Group: "const", Expr: "\"abc\"|d(\"x\") == \"abc\"", Stock: outTrue, Native: outTrue},
	{Label: "f_escape", Group: "const", Expr: "\"<b>\"|escape == \"<b>\"", Stock: outFalse, Native: outFalse},
	{Label: "f_e_alias", Group: "const", Expr: "\"<\"|e == \"<\"", Stock: outFalse, Native: outFalse},
	{Label: "f_safe", Group: "const", Expr: "\"a\"|safe == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "f_string", Group: "const", Expr: "1|string == \"1\"", Stock: outTrue, Native: outTrue},
	{Label: "f_string_float", Group: "const", Expr: "2.0|string == \"2.0\"", Stock: outTrue, Native: outTrue},
	{Label: "f_bool", Group: "const", Expr: "1|bool == true", Stock: outTrue, Native: outTrue},
	{Label: "f_split", Group: "const", Expr: "\"a,b\"|split(\",\")|length == 2", Stock: outError, Native: outUnsupported},
	{Label: "f_lines", Group: "const", Expr: "\"a\\nb\"|lines|length == 2", Retained: "\"a\\\\nb\"|lines|length == 2", Stock: outFalse, Native: outFalse},
	{Label: "f_first", Group: "const", Expr: "[1,2]|first == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_last", Group: "const", Expr: "[1,2]|last == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_reverse", Group: "const", Expr: "[1,2]|reverse|first == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_sort", Group: "const", Expr: "[2,1]|sort|first == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_join", Group: "const", Expr: "[1,2]|join(\"-\") == \"1-2\"", Stock: outTrue, Native: outTrue},
	{Label: "f_list_str", Group: "const", Expr: "\"ab\"|list|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_unique", Group: "const", Expr: "[1,1,2]|unique|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_min", Group: "const", Expr: "[1,2]|min == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_max", Group: "const", Expr: "[1,2]|max == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_ints", Group: "const", Expr: "[1,2]|sum == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_mixed", Group: "const", Expr: "[1,2.5]|sum == 3.5", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_intfloat", Group: "const", Expr: "[1,2.0]|sum == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_bool", Group: "const", Expr: "[true,1]|sum == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_str", Group: "const", Expr: "[\"a\"]|sum == 0", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_empty", Group: "const", Expr: "[]|sum == 0", Stock: outTrue, Native: outTrue},
	{Label: "f_sum_str_arg", Group: "const", Expr: "\"ab\"|sum == 0", Stock: outError, Native: outUnsupported},
	{Label: "f_sum_map", Group: "const", Expr: "{\"a\":1}|sum == 0", Stock: outError, Native: outUnsupported},
	{Label: "f_sum_attr_kwarg", Group: "const", Expr: "[{\"a\":1}]|sum(attribute=\"a\") == 1", Stock: outError, Native: outUnsupported},
	{Label: "f_batch", Group: "const", Expr: "[1,2,3,4]|batch(2)|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_slice", Group: "const", Expr: "[1,2,3]|slice(2)|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_map_filter", Group: "const", Expr: "[1,2]|map(\"string\")|first == \"1\"", Stock: outTrue, Native: outTrue},
	{Label: "f_select", Group: "const", Expr: "[1,2,3]|select(\"odd\")|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_reject", Group: "const", Expr: "[1,2,3]|reject(\"odd\")|length == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_selectattr", Group: "const", Expr: "[{\"a\":1}]|selectattr(\"a\")|length == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_rejectattr", Group: "const", Expr: "[{\"a\":1}]|rejectattr(\"a\")|length == 0", Stock: outTrue, Native: outTrue},
	{Label: "f_groupby", Group: "const", Expr: "[{\"k\":\"a\"},{\"k\":\"a\"},{\"k\":\"b\"}]|groupby(\"k\")|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_chain", Group: "const", Expr: "[1,2]|chain([3])|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_zip", Group: "const", Expr: "[1,2]|zip([3,4])|length == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_abs", Group: "const", Expr: "-1|abs == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_int", Group: "const", Expr: "\"2\"|int == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_float", Group: "const", Expr: "\"2.5\"|float == 2.5", Stock: outTrue, Native: outTrue},
	{Label: "f_round", Group: "const", Expr: "2.4|round == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_round_half", Group: "const", Expr: "2.5|round == 3", Stock: outTrue, Native: outTrue},
	{Label: "f_items", Group: "const", Expr: "{\"a\":1}|items|length == 1", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `items` over a mapping; minijinja-Go sorts the keys, minijinja preserves insertion order"},
	{Label: "f_items_order", Group: "const", Expr: "({\"z\":1,\"a\":2}|items|first|first) == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `items` over a mapping; minijinja-Go sorts the keys, minijinja preserves insertion order"},
	{Label: "f_dictsort", Group: "const", Expr: "({\"b\":2,\"a\":1}|dictsort|first|first) == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "f_attr", Group: "const", Expr: "{\"a\":1}|attr(\"a\") == 1", Stock: outTrue, Native: outTrue},
	{Label: "f_indent", Group: "const", Expr: "\"a\"|indent(2) == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "f_pprint", Group: "const", Expr: "[1]|pprint == \"[1]\"", Stock: outFalse, Native: outFalse},
	{Label: "f_tojson_list", Group: "const", Expr: "[1,2]|tojson == \"[1,2]\"", Stock: outTrue, Native: outTrue},
	{Label: "f_tojson_map", Group: "const", Expr: "({\"z\":1,\"a\":2}|tojson)[2] == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `tojson` over a value containing a mapping; key order differs"},
	{Label: "f_urlencode", Group: "const", Expr: "\"a b\"|urlencode == \"a%20b\"", Stock: outError, Native: outUnsupported},
	{Label: "f_regex_simple", Group: "const", Expr: "\"abc\"|regex_match(\"^a\")", Stock: outTrue, Native: outTrue},
	{Label: "f_regex_class", Group: "const", Expr: "\"(123)456-7890\"|regex_match(\"\\\\(?\\\\d{3}\\\\)?[-.\\\\s]?\\\\d{3}[-.\\\\s]?\\\\d{4}\")", Retained: "\"(123)456-7890\"|regex_match(\"\\\\\\\\(?\\\\\\\\d{3}\\\\\\\\)?[-.\\\\\\\\s]?\\\\\\\\d{3}[-.\\\\\\\\s]?\\\\\\\\d{4}\")", Stock: outFalse, Native: outFalse},
	{Label: "f_regex_bad", Group: "const", Expr: "\"abc\"|regex_match(\"[\")", Stock: outFalse, Native: outFalse},
	{Label: "f_regex_nonstr", Group: "const", Expr: "1|regex_match(\"1\")", Stock: outTrue, Native: outTrue},
	{Label: "f_regex_word", Group: "const", Expr: "\"a b\"|regex_match(\"\\\\ba\\\\b\")", Retained: "\"a b\"|regex_match(\"\\\\\\\\ba\\\\\\\\b\")", Stock: outFalse, Native: outFalse},
	{Label: "f_regex_unicode", Group: "const", Expr: "\"\\u65e5\"|regex_match(\"\\\\p{Han}\")", Retained: "\"\\\\u65e5\"|regex_match(\"\\\\\\\\p{Han}\")", Stock: outFalse, Native: outFalse},
	{Label: "f_regex_noarg", Group: "const", Expr: "\"a\"|regex_match", Stock: outError, Native: outUnsupported},
	{Label: "f_unknown", Group: "const", Expr: "1|nosuchfilter == 1", Stock: outError, Native: outUnsupported},
	{Label: "t_defined", Group: "const", Expr: "1 is defined", Stock: outTrue, Native: outTrue},
	{Label: "t_undefined", Group: "const", Expr: "nosuchvar is undefined", Stock: outTrue, Native: outTrue},
	{Label: "t_none", Group: "const", Expr: "none is none", Stock: outTrue, Native: outTrue},
	{Label: "t_true", Group: "const", Expr: "true is true", Stock: outTrue, Native: outTrue},
	{Label: "t_false", Group: "const", Expr: "false is false", Stock: outTrue, Native: outTrue},
	{Label: "t_odd", Group: "const", Expr: "1 is odd", Stock: outTrue, Native: outTrue},
	{Label: "t_even", Group: "const", Expr: "2 is even", Stock: outTrue, Native: outTrue},
	{Label: "t_divisibleby", Group: "const", Expr: "4 is divisibleby(2)", Stock: outTrue, Native: outTrue},
	{Label: "t_eq", Group: "const", Expr: "1 is eq(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_equalto", Group: "const", Expr: "1 is equalto(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_ne", Group: "const", Expr: "1 is ne(2)", Stock: outTrue, Native: outTrue},
	{Label: "t_lt", Group: "const", Expr: "1 is lt(2)", Stock: outTrue, Native: outTrue},
	{Label: "t_lessthan", Group: "const", Expr: "1 is lessthan(2)", Stock: outTrue, Native: outTrue},
	{Label: "t_le", Group: "const", Expr: "1 is le(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_gt", Group: "const", Expr: "2 is gt(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_greaterthan", Group: "const", Expr: "2 is greaterthan(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_ge", Group: "const", Expr: "2 is ge(2)", Stock: outTrue, Native: outTrue},
	{Label: "t_in", Group: "const", Expr: "1 is in([1,2])", Stock: outTrue, Native: outTrue},
	{Label: "t_string", Group: "const", Expr: "\"a\" is string", Stock: outTrue, Native: outTrue},
	{Label: "t_number", Group: "const", Expr: "1 is number", Stock: outTrue, Native: outTrue},
	{Label: "t_integer", Group: "const", Expr: "1 is integer", Stock: outTrue, Native: outTrue},
	{Label: "t_int_alias", Group: "const", Expr: "1 is int", Stock: outTrue, Native: outTrue},
	{Label: "t_float", Group: "const", Expr: "1.5 is float", Stock: outTrue, Native: outTrue},
	{Label: "t_boolean", Group: "const", Expr: "true is boolean", Stock: outTrue, Native: outTrue},
	{Label: "t_sequence", Group: "const", Expr: "[1] is sequence", Stock: outTrue, Native: outTrue},
	{Label: "t_mapping", Group: "const", Expr: "{\"a\":1} is mapping", Stock: outTrue, Native: outTrue},
	{Label: "t_iterable", Group: "const", Expr: "[1] is iterable", Stock: outTrue, Native: outTrue},
	{Label: "t_startingwith", Group: "const", Expr: "\"abc\" is startingwith(\"a\")", Stock: outTrue, Native: outTrue},
	{Label: "t_endingwith", Group: "const", Expr: "\"abc\" is endingwith(\"c\")", Stock: outTrue, Native: outTrue},
	{Label: "t_containing", Group: "const", Expr: "\"abc\" is containing(\"b\")", Stock: outError, Native: outUnsupported},
	{Label: "t_safe", Group: "const", Expr: "\"a\" is safe", Stock: outFalse, Native: outFalse},
	{Label: "t_escaped", Group: "const", Expr: "1 is escaped", Stock: outFalse, Native: outFalse},
	{Label: "t_sameas", Group: "const", Expr: "1 is sameas(1)", Stock: outTrue, Native: outTrue},
	{Label: "t_lower", Group: "const", Expr: "\"a\" is lower", Stock: outTrue, Native: outTrue},
	{Label: "t_upper", Group: "const", Expr: "\"A\" is upper", Stock: outTrue, Native: outTrue},
	{Label: "t_filter", Group: "const", Expr: "\"upper\" is filter", Stock: outTrue, Native: outTrue},
	{Label: "t_test", Group: "const", Expr: "\"odd\" is test", Stock: outTrue, Native: outTrue},
	{Label: "t_unknown", Group: "const", Expr: "1 is nosuchtest", Stock: outError, Native: outUnsupported},
	{Label: "fn_range", Group: "const", Expr: "range(3)|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "fn_range_args", Group: "const", Expr: "range(1,4)|first == 1", Stock: outTrue, Native: outTrue},
	{Label: "fn_dict", Group: "const", Expr: "dict(a=1)|length == 1", Stock: outTrue, Native: outTrue},
	{Label: "fn_namespace", Group: "const", Expr: "namespace(a=1).a == 1", Stock: outTrue, Native: outTrue},
	{Label: "fn_cycler", Group: "const", Expr: "cycler(\"a\",\"b\").next() == \"a\"", Stock: outError, Native: outUnsupported},
	{Label: "fn_joiner", Group: "const", Expr: "joiner(\",\")() == \"\"", Stock: outError, Native: outUnsupported},
	{Label: "fn_unknown", Group: "const", Expr: "nosuchfn() == 1", Stock: outError, Native: outUnsupported},
	{Label: "py_str_upper", Group: "const", Expr: "\"abc\".upper() == \"ABC\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_lower", Group: "const", Expr: "\"ABC\".lower() == \"abc\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_startswith", Group: "const", Expr: "\"abc\".startswith(\"a\")", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_endswith", Group: "const", Expr: "\"abc\".endswith(\"c\")", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_split", Group: "const", Expr: "\"a-b\".split(\"-\")|length == 2", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_format", Group: "const", Expr: "\"{}\".format(1) == \"1\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_format_commas", Group: "const", Expr: "\"{:,}\".format(1234567) == \"1,234,567\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_format_float", Group: "const", Expr: "\"{:.2f}\".format(3.14159) == \"3.14\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_find", Group: "const", Expr: "\"abc\".find(\"b\") == 1", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_rfind", Group: "const", Expr: "\"abcb\".rfind(\"b\") == 3", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_count", Group: "const", Expr: "\"aba\".count(\"a\") == 2", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_strip", Group: "const", Expr: "\"  a \".strip() == \"a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_lstrip", Group: "const", Expr: "\" a\".lstrip() == \"a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_rstrip", Group: "const", Expr: "\"a \".rstrip() == \"a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isalpha", Group: "const", Expr: "\"abc\".isalpha()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isdigit", Group: "const", Expr: "\"123\".isdigit()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isascii", Group: "const", Expr: "\"abc\".isascii()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_islower", Group: "const", Expr: "\"abc\".islower()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isupper", Group: "const", Expr: "\"ABC\".isupper()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isspace", Group: "const", Expr: "\"  \".isspace()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isalnum", Group: "const", Expr: "\"a1\".isalnum()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_isnumeric", Group: "const", Expr: "\"1\".isnumeric()", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_title", Group: "const", Expr: "\"a b\".title() == \"A B\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_capitalize", Group: "const", Expr: "\"abc\".capitalize() == \"Abc\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_replace", Group: "const", Expr: "\"aXa\".replace(\"X\",\"-\") == \"a-a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_splitlines", Group: "const", Expr: "\"a\\nb\".splitlines()|length == 2", Retained: "\"a\\\\nb\".splitlines()|length == 2", Stock: outFalse, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_str_join", Group: "const", Expr: "\",\".join([\"a\",\"b\"]) == \"a,b\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_seq_count", Group: "const", Expr: "[1,1].count(1) == 2", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "py_num_method", Group: "const", Expr: "(1).nosuchmethod() == 1", Stock: outError, Native: outUnsupported},
	{Label: "this_int_gt", Group: "int7", Expr: "this > 0", Stock: outTrue, Native: outTrue},
	{Label: "this_int_eq", Group: "int7", Expr: "this == 7", Stock: outTrue, Native: outTrue},
	{Label: "this_int_abs", Group: "int7", Expr: "this|abs == 7", Stock: outTrue, Native: outTrue},
	{Label: "this_int_odd", Group: "int7", Expr: "this is odd", Stock: outTrue, Native: outTrue},
	{Label: "this_int_arith", Group: "int7", Expr: "this + 1 == 8", Stock: outTrue, Native: outTrue},
	{Label: "this_int_string", Group: "int7", Expr: "this|string == \"7\"", Stock: outTrue, Native: outTrue},
	{Label: "this_int_in", Group: "int7", Expr: "this in [7]", Stock: outTrue, Native: outTrue},
	{Label: "this_int_bare", Group: "int7", Expr: "this", Stock: outError, Native: outUnsupported},
	{Label: "this_intneg_abs", Group: "intneg", Expr: "this|abs == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_intneg_lt", Group: "intneg", Expr: "this < 0", Stock: outTrue, Native: outTrue},
	{Label: "this_int0_eq", Group: "int0", Expr: "this == 0", Stock: outTrue, Native: outTrue},
	{Label: "this_int0_bare", Group: "int0", Expr: "this", Stock: outError, Native: outUnsupported},
	{Label: "this_int0_bool", Group: "int0", Expr: "this|bool == false", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_eq", Group: "f25", Expr: "this == 2.5", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_gt", Group: "f25", Expr: "this > 2", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_round", Group: "f25", Expr: "this|round == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_int", Group: "f25", Expr: "this|int == 2", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_string", Group: "f25", Expr: "this|string == \"2.5\"", Stock: outTrue, Native: outTrue},
	{Label: "this_f25_isfloat", Group: "f25", Expr: "this is float", Stock: outTrue, Native: outTrue},
	{Label: "this_f20_eq_int", Group: "f20", Expr: "this == 2", Stock: outTrue, Native: outTrue},
	{Label: "this_f20_string", Group: "f20", Expr: "this|string == \"2.0\"", Stock: outTrue, Native: outTrue},
	{Label: "this_f20_isint", Group: "f20", Expr: "this is integer", Stock: outFalse, Native: outFalse},
	{Label: "this_f20_sum", Group: "f20", Expr: "[this]|sum == 2", Stock: outTrue, Native: outTrue},
	{Label: "this_str_length", Group: "str", Expr: "this|length == 11", Stock: outTrue, Native: outTrue},
	{Label: "this_str_lower", Group: "str", Expr: "this|lower == \"hello world\"", Stock: outTrue, Native: outTrue},
	{Label: "this_str_contains", Group: "str", Expr: "\"World\" in this", Stock: outTrue, Native: outTrue},
	{Label: "this_str_regex", Group: "str", Expr: "this|regex_match(\"^Hello\")", Stock: outTrue, Native: outTrue},
	{Label: "this_str_index", Group: "str", Expr: "this[0] == \"H\"", Stock: outTrue, Native: outTrue},
	{Label: "this_str_split", Group: "str", Expr: "this|split(\" \")|length == 2", Stock: outError, Native: outUnsupported},
	{Label: "this_str_isstring", Group: "str", Expr: "this is string", Stock: outTrue, Native: outTrue},
	{Label: "this_stre_length", Group: "stre", Expr: "this|length == 0", Stock: outTrue, Native: outTrue},
	{Label: "this_stre_eq", Group: "stre", Expr: "this == \"\"", Stock: outTrue, Native: outTrue},
	{Label: "this_stre_nonempty", Group: "stre", Expr: "this|length > 0", Stock: outFalse, Native: outFalse},
	{Label: "this_stre_bare", Group: "stre", Expr: "this", Stock: outError, Native: outUnsupported},
	{Label: "this_stru_length", Group: "stru", Expr: "this|length == 10", Stock: outTrue, Native: outTrue},
	{Label: "this_stru_upper", Group: "stru", Expr: "this|upper == \"H\u00c9LLO \u2713 \u65e5\u672c\"", Stock: outTrue, Native: outTrue},
	{Label: "this_stru_regex", Group: "stru", Expr: "this|regex_match(\"\u65e5\")", Stock: outTrue, Native: outTrue},
	{Label: "this_stru_in", Group: "stru", Expr: "\"\u2713\" in this", Stock: outTrue, Native: outTrue},
	{Label: "this_stru_index", Group: "stru", Expr: "this[1] == \"\u00e9\"", Stock: outTrue, Native: outTrue},
	{Label: "this_boolt_bare", Group: "boolt", Expr: "this", Stock: outTrue, Native: outTrue},
	{Label: "this_boolt_eq", Group: "boolt", Expr: "this == true", Stock: outTrue, Native: outTrue},
	{Label: "this_boolt_eq_num", Group: "boolt", Expr: "this == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_boolf_not", Group: "boolf", Expr: "not this", Stock: outTrue, Native: outTrue},
	{Label: "this_boolf_bare", Group: "boolf", Expr: "this", Stock: outFalse, Native: outFalse},
	{Label: "this_list_length", Group: "list", Expr: "this|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_list_sum", Group: "list", Expr: "this|sum == 6", Stock: outTrue, Native: outTrue},
	{Label: "this_list_in", Group: "list", Expr: "1 in this", Stock: outTrue, Native: outTrue},
	{Label: "this_list_index", Group: "list", Expr: "this[0] == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_list_first", Group: "list", Expr: "this|first == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_list_join", Group: "list", Expr: "this|join(\",\") == \"1,2,3\"", Stock: outTrue, Native: outTrue},
	{Label: "this_list_max", Group: "list", Expr: "this|max == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_list_sortlast", Group: "list", Expr: "this|sort|last == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_list_isseq", Group: "list", Expr: "this is sequence", Stock: outTrue, Native: outTrue},
	{Label: "this_list_eq", Group: "list", Expr: "this == [1,2,3]", Stock: outTrue, Native: outTrue},
	{Label: "this_liste_length", Group: "liste", Expr: "this|length == 0", Stock: outTrue, Native: outTrue},
	{Label: "this_liste_sum", Group: "liste", Expr: "this|sum == 0", Stock: outTrue, Native: outTrue},
	{Label: "this_liste_first", Group: "liste", Expr: "this|first is undefined", Stock: outTrue, Native: outTrue},
	{Label: "this_listf_sum", Group: "listf", Expr: "this|sum == 3.5", Stock: outTrue, Native: outTrue},
	{Label: "this_lists_join", Group: "lists", Expr: "this|join(\"\") == \"ab\"", Stock: outTrue, Native: outTrue},
	{Label: "this_lists_in", Group: "lists", Expr: "\"a\" in this", Stock: outTrue, Native: outTrue},
	{Label: "this_map_length", Group: "map", Expr: "this|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_map_item", Group: "map", Expr: "this[\"z\"] == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_map_attr", Group: "map", Expr: "this.z == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_map_order", Group: "map", Expr: "this|list|join(\",\") == \"z,a,m\"", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_map_in", Group: "map", Expr: "\"z\" in this", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_map_notin", Group: "map", Expr: "\"q\" not in this", Stock: outTrue, Native: outTrue},
	{Label: "this_map_keys", Group: "map", Expr: "this.keys()|join(\",\") == \"z,a,m\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_values", Group: "map", Expr: "this.values()|sum == 6", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_items", Group: "map", Expr: "this.items()|length == 3", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_get", Group: "map", Expr: "this.get(\"z\") == 1", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_get_missing", Group: "map", Expr: "this.get(\"q\") == none", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_get_default", Group: "map", Expr: "this.get(\"q\", 9) == 9", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_map_items_filter", Group: "map", Expr: "this|items|length == 3", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `items` over a mapping; minijinja-Go sorts the keys, minijinja preserves insertion order"},
	{Label: "this_map_dictsort", Group: "map", Expr: "(this|dictsort|first|first) == \"a\"", Stock: outTrue, Native: outTrue},
	{Label: "this_map_tojson", Group: "map", Expr: "(this|tojson)[2] == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `tojson` over a value containing a mapping; key order differs"},
	{Label: "this_map_ismapping", Group: "map", Expr: "this is mapping", Stock: outTrue, Native: outTrue},
	{Label: "this_map_first", Group: "map", Expr: "this|first == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_map_sum", Group: "map", Expr: "this|sum == 0", Stock: outError, Native: outUnsupported},
	{Label: "this_map_eq", Group: "map", Expr: "this == {\"z\":1,\"a\":2,\"m\":3}", Stock: outTrue, Native: outTrue},
	{Label: "this_map_missing_attr", Group: "map", Expr: "this.q is undefined", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_attr", Group: "cls", Expr: "this.b == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_attr_str", Group: "cls", Expr: "this.a == \"x\"", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_nested", Group: "cls", Expr: "this.c|sum == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_order", Group: "cls", Expr: "this|list|join(\",\") == \"b,a,c\"", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_cls_in", Group: "cls", Expr: "\"a\" in this", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_cls_length", Group: "cls", Expr: "this|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_ismapping", Group: "cls", Expr: "this is mapping", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_item", Group: "cls", Expr: "this[\"b\"] == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_keys", Group: "cls", Expr: "this.keys()|join(\",\") == \"b,a,c\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_enum_eq", Group: "enum", Expr: "this == \"RED\"", Stock: outTrue, Native: outTrue},
	{Label: "this_enum_alias", Group: "enum", Expr: "this == \"rouge\"", Stock: outFalse, Native: outFalse},
	{Label: "this_enum_length", Group: "enum", Expr: "this|length == 3", Stock: outTrue, Native: outTrue},
	{Label: "this_enum_isstring", Group: "enum", Expr: "this is string", Stock: outTrue, Native: outTrue},
	{Label: "this_enum_in", Group: "enum", Expr: "this in [\"RED\",\"GREEN\"]", Stock: outTrue, Native: outTrue},
	{Label: "this_null_eq", Group: "null", Expr: "this == none", Stock: outTrue, Native: outTrue},
	{Label: "this_null_isnone", Group: "null", Expr: "this is none", Stock: outTrue, Native: outTrue},
	{Label: "this_null_defined", Group: "null", Expr: "this is defined", Stock: outTrue, Native: outTrue},
	{Label: "this_null_string", Group: "null", Expr: "this|string == \"none\"", Stock: outTrue, Native: outTrue},
	{Label: "this_null_default", Group: "null", Expr: "this|default(1) == 1", Stock: outFalse, Native: outFalse},
	{Label: "this_null_bare", Group: "null", Expr: "this", Stock: outNoChecks, Native: outUnsupported},
	{Label: "fn_debug", Group: "const", Expr: "debug()|length > 0", Stock: outTrue, Native: outTrue},
	{Label: "fn_lipsum", Group: "const", Expr: "lipsum(1)|length > 0", Stock: outError, Native: outUnsupported},
	{Label: "op_str_lt_str", Group: "const", Expr: "\"a\" < \"b\"", Stock: outTrue, Native: outTrue},
	{Label: "op_empty_map", Group: "const", Expr: "{}|length == 0", Stock: outTrue, Native: outTrue},
	{Label: "op_empty_list", Group: "const", Expr: "[]|length == 0", Stock: outTrue, Native: outTrue},
	{Label: "f_sort_kwarg", Group: "const", Expr: "[1,2]|sort(reverse=true)|first == 2", Stock: outTrue, Native: outTrue},
	{Label: "f_default_boolean", Group: "const", Expr: "\"\"|default(\"b\", true) == \"b\"", Stock: outTrue, Native: outTrue},
	{Label: "f_length_none", Group: "const", Expr: "none|length == 0", Stock: outError, Native: outUnsupported},
	{Label: "f_string_list", Group: "const", Expr: "([1,2]|string)[0] == \"[\"", Stock: outTrue, Native: outTrue},
	{Label: "f_string_map", Group: "const", Expr: "({\"z\":1}|string)[0] == \"{\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `string` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order"},
	{Label: "this_map_string", Group: "map", Expr: "(this|string)[0] == \"{\"", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_map_string_len", Group: "map", Expr: "(this|string)|length == 24", Stock: outTrue, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_cls_string_len", Group: "cls", Expr: "(this|string)|length == 33", Stock: outFalse, Native: outUnsupported, Note: "profile: representation-sensitive over a mapping \u2014 the ordered and native projections disagree, so neither `in` membership nor iteration order can be claimed. Refused by the agreement check in renderConstraint."},
	{Label: "this_list_string", Group: "list", Expr: "(this|string)[0] == \"[\"", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_nested_index", Group: "cls", Expr: "this.c[0] == 1", Stock: outTrue, Native: outTrue},
	{Label: "this_map_last", Group: "map", Expr: "this|last == \"m\"", Stock: outError, Native: outUnsupported},
	{Label: "this_map_values_join", Group: "map", Expr: "this.values()|join(\",\") == \"1,2,3\"", Stock: outTrue, Native: outUnsupported, Note: "profile: pycompat string/sequence method. BAML installs minijinja-contrib's unknown-method callback; minijinja-Go v2.16.0 has no environment-level hook for one, and a Go string Value has no method table, so these are outside the contract by construction."},
	{Label: "this_stru_isascii", Group: "stru", Expr: "this|length > 0", Stock: outTrue, Native: outTrue},
	{Label: "f_maplit_order", Group: "const", Expr: "({\"z\":1,\"a\":2}|list|join(\",\")) == \"z,a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `list` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order"},
	{Label: "f_maplit_first", Group: "const", Expr: "{\"z\":1,\"a\":2}|first == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `first` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order"},
	{Label: "f_maplit_string", Group: "const", Expr: "({\"z\":1,\"a\":2}|string)[2] == \"z\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `string` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order"},
	{Label: "f_maplit_reverse", Group: "const", Expr: "({\"z\":1,\"a\":2}|reverse|first) == \"a\"", Stock: outFalse, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `reverse` over a mapping literal; minijinja-Go enumerates it sorted, minijinja in insertion order"},
	{Label: "f_split_first", Group: "const", Expr: "\"a,b\"|split(\",\")|first == \"a\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `split` returns a lazy iterator in minijinja and a list in minijinja-Go"},
	{Label: "f_split_join", Group: "const", Expr: "(\"a,b\"|split(\",\")|join(\"-\")) == \"a-b\"", Stock: outTrue, Native: outUnsupported, Note: "profile: minijinja-Go raises where BAML answers \u2014 debaml: constraint expression outside the proven native profile: `split` returns a lazy iterator in minijinja and a list in minijinja-Go"},
	{Label: "f_length_undefined", Group: "const", Expr: "nosuchvar|length == 0", Stock: outError, Native: outUnsupported},
	{Label: "f_length_int", Group: "const", Expr: "1|length == 0", Stock: outError, Native: outUnsupported},
	{Label: "this_map_concat", Group: "map", Expr: "(this ~ \"\")|length == 24", Stock: outTrue, Native: outTrue},
	{Label: "this_cls_concat", Group: "cls", Expr: "(this ~ \"\")|length == 33", Stock: outFalse, Native: outFalse},
}
