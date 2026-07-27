// GENERATED from internal/debaml/constraintoracle/corpus_test.go — do not edit.
//
// Regenerate with:
//   BAML_CONSTRAINT_FIXTURE_WRITE=1 go test -tags integration \
//     ./internal/debaml/constraintoracle -run TestConstraintFixtureDrift
// then re-run the stock generator over this project (see the package comment).
//
// Every function here exists only to give the stock BAML v0.223.0 client a
// constrained node to evaluate. The `v` field carries the @check(s); the
// check LABEL is the corpus case label, so a batched result is recoverable
// from the checks map by name.

class Probe {
  b int
  a string
  c int[]
}

enum Hue {
  RED @alias("rouge")
  GREEN
}


class Batch_const {
  v int
    @check(op_add, {{ 1 + 1 == 2 }})
    @check(op_sub, {{ 1 - 2 == -1 }})
    @check(op_mul, {{ 2 * 3 == 6 }})
    @check(op_div, {{ 7 / 2 == 3.5 }})
    @check(op_div_int, {{ 4 / 2 == 2 }})
    @check(op_floordiv, {{ 7 // 2 == 3 }})
    @check(op_rem, {{ 7 % 3 == 1 }})
    @check(op_pow, {{ 2 ** 10 == 1024 }})
    @check(op_neg, {{ -3 < 0 }})
    @check(op_not, {{ not false }})
    @check(op_and, {{ (true and false) == false }})
    @check(op_or, {{ true or false }})
    @check(op_lt, {{ 1 < 2 }})
    @check(op_le, {{ 2 <= 2 }})
    @check(op_gt, {{ 3 > 2 }})
    @check(op_ge, {{ 3 >= 3 }})
    @check(op_eq, {{ 1 == 1 }})
    @check(op_ne, {{ 1 != 2 }})
    @check(op_concat, {{ "a" ~ "b" == "ab" }})
    @check(op_concat_num, {{ 1 ~ 2 == "12" }})
    @check(op_in_list, {{ 1 in [1,2] }})
    @check(op_notin_list, {{ 4 not in [1,2] }})
    @check(op_in_str, {{ "ell" in "hello" }})
    @check(op_in_maplit, {{ "a" in {"a": 1} }})
    @check(op_eq_list, {{ [1,2] == [1,2] }})
    @check(op_eq_map, {{ {"a":1} == {"a":1} }})
    @check(op_ternary, {{ (1 if true else 2) == 1 }})
    @check(op_float_int_eq, {{ 1.0 == 1 }})
    @check(op_str_num_eq, {{ "1" == 1 }})
    @check(op_bool_num_eq, {{ true == 1 }})
    @check(op_none_eq, {{ none == none }})
    @check(op_index, {{ [1,2][0] == 1 }})
    @check(op_index_neg, {{ [1,2,3][-1] == 3 }})
    @check(op_slice, {{ [1,2,3][1:] == [2,3] }})
    @check(op_str_index, {{ "abc"[0] == "a" }})
    @check(op_mixed_arith, {{ 1 + 1.5 == 2.5 }})
    @check(op_i64max, {{ 9223372036854775807 - 1 == 9223372036854775806 }})
    @check(op_float_assoc, {{ 0.1 + 0.2 == 0.3 }})
    @check(op_div_zero, {{ (1 / 0) > 0 }})
    @check(op_str_gt_num, {{ "x" > 0 }})
    @check(op_num_lt_list, {{ 1 < [1] }})
    @check(op_undefined_cmp, {{ nosuchvar > 0 }})
    @check(op_undefined_attr, {{ this.nope > 0 }})
    @check(f_length_str, {{ "abc"|length == 3 }})
    @check(f_length_list, {{ [1,2,3]|length == 3 }})
    @check(f_length_map, {{ {"a":1,"b":2}|length == 2 }})
    @check(f_count_alias, {{ "abc"|count == 3 }})
    @check(f_lower, {{ "ABC"|lower == "abc" }})
    @check(f_upper, {{ "abc"|upper == "ABC" }})
    @check(f_capitalize, {{ "abc"|capitalize == "Abc" }})
    @check(f_title, {{ "a b"|title == "A B" }})
    @check(f_trim, {{ "  a "|trim == "a" }})
    @check(f_replace, {{ "aXa"|replace("X","-") == "a-a" }})
    @check(f_format, {{ "%s!"|format("hi") == "hi!" }})
    @check(f_default_present, {{ 1|default(2) == 1 }})
    @check(f_default_missing, {{ nosuchvar|default(2) == 2 }})
    @check(f_d_alias, {{ "abc"|d("x") == "abc" }})
    @check(f_escape, {{ "<b>"|escape == "<b>" }})
    @check(f_e_alias, {{ "<"|e == "<" }})
    @check(f_safe, {{ "a"|safe == "a" }})
    @check(f_string, {{ 1|string == "1" }})
    @check(f_string_float, {{ 2.0|string == "2.0" }})
    @check(f_bool, {{ 1|bool == true }})
    @check(f_lines, {{ "a\nb"|lines|length == 2 }})
    @check(f_first, {{ [1,2]|first == 1 }})
    @check(f_last, {{ [1,2]|last == 2 }})
    @check(f_reverse, {{ [1,2]|reverse|first == 2 }})
    @check(f_sort, {{ [2,1]|sort|first == 1 }})
    @check(f_join, {{ [1,2]|join("-") == "1-2" }})
    @check(f_list_str, {{ "ab"|list|length == 2 }})
    @check(f_unique, {{ [1,1,2]|unique|length == 2 }})
    @check(f_min, {{ [1,2]|min == 1 }})
    @check(f_max, {{ [1,2]|max == 2 }})
    @check(f_sum_ints, {{ [1,2]|sum == 3 }})
    @check(f_sum_mixed, {{ [1,2.5]|sum == 3.5 }})
    @check(f_sum_intfloat, {{ [1,2.0]|sum == 3 }})
    @check(f_sum_bool, {{ [true,1]|sum == 2 }})
    @check(f_sum_str, {{ ["a"]|sum == 0 }})
    @check(f_sum_empty, {{ []|sum == 0 }})
    @check(f_batch, {{ [1,2,3,4]|batch(2)|length == 2 }})
    @check(f_slice, {{ [1,2,3]|slice(2)|length == 2 }})
    @check(f_map_filter, {{ [1,2]|map("string")|first == "1" }})
    @check(f_select, {{ [1,2,3]|select("odd")|length == 2 }})
    @check(f_reject, {{ [1,2,3]|reject("odd")|length == 1 }})
    @check(f_selectattr, {{ [{"a":1}]|selectattr("a")|length == 1 }})
    @check(f_rejectattr, {{ [{"a":1}]|rejectattr("a")|length == 0 }})
    @check(f_groupby, {{ [{"k":"a"},{"k":"a"},{"k":"b"}]|groupby("k")|length == 2 }})
    @check(f_chain, {{ [1,2]|chain([3])|length == 3 }})
    @check(f_zip, {{ [1,2]|zip([3,4])|length == 2 }})
    @check(f_abs, {{ -1|abs == 1 }})
    @check(f_int, {{ "2"|int == 2 }})
    @check(f_float, {{ "2.5"|float == 2.5 }})
    @check(f_round, {{ 2.4|round == 2 }})
    @check(f_round_half, {{ 2.5|round == 3 }})
    @check(f_items, {{ {"a":1}|items|length == 1 }})
    @check(f_items_order, {{ ({"z":1,"a":2}|items|first|first) == "z" }})
    @check(f_dictsort, {{ ({"b":2,"a":1}|dictsort|first|first) == "a" }})
    @check(f_attr, {{ {"a":1}|attr("a") == 1 }})
    @check(f_indent, {{ "a"|indent(2) == "a" }})
    @check(f_pprint, {{ [1]|pprint == "[1]" }})
    @check(f_tojson_list, {{ [1,2]|tojson == "[1,2]" }})
    @check(f_tojson_map, {{ ({"z":1,"a":2}|tojson)[2] == "z" }})
    @check(f_regex_simple, {{ "abc"|regex_match("^a") }})
    @check(f_regex_class, {{ "(123)456-7890"|regex_match("\\(?\\d{3}\\)?[-.\\s]?\\d{3}[-.\\s]?\\d{4}") }})
    @check(f_regex_bad, {{ "abc"|regex_match("[") }})
    @check(f_regex_nonstr, {{ 1|regex_match("1") }})
    @check(f_regex_word, {{ "a b"|regex_match("\\ba\\b") }})
    @check(f_regex_unicode, {{ "\u65e5"|regex_match("\\p{Han}") }})
    @check(t_defined, {{ 1 is defined }})
    @check(t_undefined, {{ nosuchvar is undefined }})
    @check(t_none, {{ none is none }})
    @check(t_true, {{ true is true }})
    @check(t_false, {{ false is false }})
    @check(t_odd, {{ 1 is odd }})
    @check(t_even, {{ 2 is even }})
    @check(t_divisibleby, {{ 4 is divisibleby(2) }})
    @check(t_eq, {{ 1 is eq(1) }})
    @check(t_equalto, {{ 1 is equalto(1) }})
    @check(t_ne, {{ 1 is ne(2) }})
    @check(t_lt, {{ 1 is lt(2) }})
    @check(t_lessthan, {{ 1 is lessthan(2) }})
    @check(t_le, {{ 1 is le(1) }})
    @check(t_gt, {{ 2 is gt(1) }})
    @check(t_greaterthan, {{ 2 is greaterthan(1) }})
    @check(t_ge, {{ 2 is ge(2) }})
    @check(t_in, {{ 1 is in([1,2]) }})
    @check(t_string, {{ "a" is string }})
    @check(t_number, {{ 1 is number }})
    @check(t_integer, {{ 1 is integer }})
    @check(t_int_alias, {{ 1 is int }})
    @check(t_float, {{ 1.5 is float }})
    @check(t_boolean, {{ true is boolean }})
    @check(t_sequence, {{ [1] is sequence }})
    @check(t_mapping, {{ {"a":1} is mapping }})
    @check(t_iterable, {{ [1] is iterable }})
    @check(t_startingwith, {{ "abc" is startingwith("a") }})
    @check(t_endingwith, {{ "abc" is endingwith("c") }})
    @check(t_safe, {{ "a" is safe }})
    @check(t_escaped, {{ 1 is escaped }})
    @check(t_sameas, {{ 1 is sameas(1) }})
    @check(t_lower, {{ "a" is lower }})
    @check(t_upper, {{ "A" is upper }})
    @check(t_filter, {{ "upper" is filter }})
    @check(t_test, {{ "odd" is test }})
    @check(fn_range, {{ range(3)|length == 3 }})
    @check(fn_range_args, {{ range(1,4)|first == 1 }})
    @check(fn_dict, {{ dict(a=1)|length == 1 }})
    @check(fn_namespace, {{ namespace(a=1).a == 1 }})
    @check(py_str_upper, {{ "abc".upper() == "ABC" }})
    @check(py_str_lower, {{ "ABC".lower() == "abc" }})
    @check(py_str_startswith, {{ "abc".startswith("a") }})
    @check(py_str_endswith, {{ "abc".endswith("c") }})
    @check(py_str_split, {{ "a-b".split("-")|length == 2 }})
    @check(py_str_format, {{ "{}".format(1) == "1" }})
    @check(py_str_format_commas, {{ "{:,}".format(1234567) == "1,234,567" }})
    @check(py_str_format_float, {{ "{:.2f}".format(3.14159) == "3.14" }})
    @check(py_str_find, {{ "abc".find("b") == 1 }})
    @check(py_str_rfind, {{ "abcb".rfind("b") == 3 }})
    @check(py_str_count, {{ "aba".count("a") == 2 }})
    @check(py_str_strip, {{ "  a ".strip() == "a" }})
    @check(py_str_lstrip, {{ " a".lstrip() == "a" }})
    @check(py_str_rstrip, {{ "a ".rstrip() == "a" }})
    @check(py_str_isalpha, {{ "abc".isalpha() }})
    @check(py_str_isdigit, {{ "123".isdigit() }})
    @check(py_str_isascii, {{ "abc".isascii() }})
    @check(py_str_islower, {{ "abc".islower() }})
    @check(py_str_isupper, {{ "ABC".isupper() }})
    @check(py_str_isspace, {{ "  ".isspace() }})
    @check(py_str_isalnum, {{ "a1".isalnum() }})
    @check(py_str_isnumeric, {{ "1".isnumeric() }})
    @check(py_str_title, {{ "a b".title() == "A B" }})
    @check(py_str_capitalize, {{ "abc".capitalize() == "Abc" }})
    @check(py_str_replace, {{ "aXa".replace("X","-") == "a-a" }})
    @check(py_str_splitlines, {{ "a\nb".splitlines()|length == 2 }})
    @check(py_str_join, {{ ",".join(["a","b"]) == "a,b" }})
    @check(py_seq_count, {{ [1,1].count(1) == 2 }})
    @check(fn_debug, {{ debug()|length > 0 }})
    @check(op_str_lt_str, {{ "a" < "b" }})
    @check(op_empty_map, {{ {}|length == 0 }})
    @check(op_empty_list, {{ []|length == 0 }})
    @check(f_sort_kwarg, {{ [1,2]|sort(reverse=true)|first == 2 }})
    @check(f_default_boolean, {{ ""|default("b", true) == "b" }})
    @check(f_string_list, {{ ([1,2]|string)[0] == "[" }})
    @check(f_string_map, {{ ({"z":1}|string)[0] == "{" }})
    @check(f_maplit_order, {{ ({"z":1,"a":2}|list|join(",")) == "z,a" }})
    @check(f_maplit_first, {{ {"z":1,"a":2}|first == "z" }})
    @check(f_maplit_string, {{ ({"z":1,"a":2}|string)[2] == "z" }})
    @check(f_maplit_reverse, {{ ({"z":1,"a":2}|reverse|first) == "a" }})
    @check(f_split_first, {{ "a,b"|split(",")|first == "a" }})
    @check(f_split_join, {{ ("a,b"|split(",")|join("-")) == "a-b" }})
    @check(num_exact_max, {{ 9007199254740991 == 9007199254740991 }})
    @check(num_conflate, {{ 9007199254740993 == 9007199254740992 }})
    @check(num_at_boundary, {{ 9007199254740992 == 9007199254740992 }})
    @check(num_add_escape, {{ 9007199254740990 + 1 == 9007199254740991 }})
    @check(num_i64_min, {{ 0 - 9223372036854775807 == -9223372036854775807 }})
    @check(num_mul_escape, {{ 3037000500 * 3037000500 > 9007199254740992 }})
    @check(num_small_mul, {{ 1000 * 1000 == 1000000 }})
    @check(num_pow_small, {{ 2 ** 3 == 8 }})
    @check(num_float_big, {{ 1.0e300 > 1.0e299 }})
}

function Batch_constFn(topic: string) -> Batch_const {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_int7 {
  v int
    @check(this_int_gt, {{ this > 0 }})
    @check(this_int_eq, {{ this == 7 }})
    @check(this_int_abs, {{ this|abs == 7 }})
    @check(this_int_odd, {{ this is odd }})
    @check(this_int_arith, {{ this + 1 == 8 }})
    @check(this_int_string, {{ this|string == "7" }})
    @check(this_int_in, {{ this in [7] }})
    @check(this_int_big_cmp, {{ this * 1000000000 < 9007199254740992 }})
}

function Batch_int7Fn(topic: string) -> Batch_int7 {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_intneg {
  v int
    @check(this_intneg_abs, {{ this|abs == 3 }})
    @check(this_intneg_lt, {{ this < 0 }})
}

function Batch_intnegFn(topic: string) -> Batch_intneg {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_int0 {
  v int
    @check(this_int0_eq, {{ this == 0 }})
    @check(this_int0_bool, {{ this|bool == false }})
}

function Batch_int0Fn(topic: string) -> Batch_int0 {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_f25 {
  v float
    @check(this_f25_eq, {{ this == 2.5 }})
    @check(this_f25_gt, {{ this > 2 }})
    @check(this_f25_round, {{ this|round == 3 }})
    @check(this_f25_int, {{ this|int == 2 }})
    @check(this_f25_string, {{ this|string == "2.5" }})
    @check(this_f25_isfloat, {{ this is float }})
}

function Batch_f25Fn(topic: string) -> Batch_f25 {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_f20 {
  v float
    @check(this_f20_eq_int, {{ this == 2 }})
    @check(this_f20_string, {{ this|string == "2.0" }})
    @check(this_f20_isint, {{ this is integer }})
    @check(this_f20_sum, {{ [this]|sum == 2 }})
}

function Batch_f20Fn(topic: string) -> Batch_f20 {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_str {
  v string
    @check(this_str_length, {{ this|length == 11 }})
    @check(this_str_lower, {{ this|lower == "hello world" }})
    @check(this_str_contains, {{ "World" in this }})
    @check(this_str_regex, {{ this|regex_match("^Hello") }})
    @check(this_str_index, {{ this[0] == "H" }})
    @check(this_str_isstring, {{ this is string }})
}

function Batch_strFn(topic: string) -> Batch_str {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_stre {
  v string
    @check(this_stre_length, {{ this|length == 0 }})
    @check(this_stre_eq, {{ this == "" }})
    @check(this_stre_nonempty, {{ this|length > 0 }})
}

function Batch_streFn(topic: string) -> Batch_stre {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_stru {
  v string
    @check(this_stru_length, {{ this|length == 10 }})
    @check(this_stru_upper, {{ this|upper == "HÉLLO ✓ 日本" }})
    @check(this_stru_regex, {{ this|regex_match("日") }})
    @check(this_stru_in, {{ "✓" in this }})
    @check(this_stru_index, {{ this[1] == "é" }})
    @check(this_stru_isascii, {{ this|length > 0 }})
}

function Batch_struFn(topic: string) -> Batch_stru {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_boolt {
  v bool
    @check(this_boolt_bare, {{ this }})
    @check(this_boolt_eq, {{ this == true }})
    @check(this_boolt_eq_num, {{ this == 1 }})
}

function Batch_booltFn(topic: string) -> Batch_boolt {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_boolf {
  v bool
    @check(this_boolf_not, {{ not this }})
    @check(this_boolf_bare, {{ this }})
}

function Batch_boolfFn(topic: string) -> Batch_boolf {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_list {
  v int[]
    @check(this_list_length, {{ this|length == 3 }})
    @check(this_list_sum, {{ this|sum == 6 }})
    @check(this_list_in, {{ 1 in this }})
    @check(this_list_index, {{ this[0] == 1 }})
    @check(this_list_first, {{ this|first == 1 }})
    @check(this_list_join, {{ this|join(",") == "1,2,3" }})
    @check(this_list_max, {{ this|max == 3 }})
    @check(this_list_sortlast, {{ this|sort|last == 3 }})
    @check(this_list_isseq, {{ this is sequence }})
    @check(this_list_eq, {{ this == [1,2,3] }})
    @check(this_list_string, {{ (this|string)[0] == "[" }})
}

function Batch_listFn(topic: string) -> Batch_list {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_liste {
  v int[]
    @check(this_liste_length, {{ this|length == 0 }})
    @check(this_liste_sum, {{ this|sum == 0 }})
    @check(this_liste_first, {{ this|first is undefined }})
}

function Batch_listeFn(topic: string) -> Batch_liste {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_listf {
  v float[]
    @check(this_listf_sum, {{ this|sum == 3.5 }})
}

function Batch_listfFn(topic: string) -> Batch_listf {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_lists {
  v string[]
    @check(this_lists_join, {{ this|join("") == "ab" }})
    @check(this_lists_in, {{ "a" in this }})
}

function Batch_listsFn(topic: string) -> Batch_lists {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_map {
  v map<string, int>
    @check(this_map_length, {{ this|length == 3 }})
    @check(this_map_item, {{ this["z"] == 1 }})
    @check(this_map_attr, {{ this.z == 1 }})
    @check(this_map_order, {{ this|list|join(",") == "z,a,m" }})
    @check(this_map_in, {{ "z" in this }})
    @check(this_map_notin, {{ "q" not in this }})
    @check(this_map_keys, {{ this.keys()|join(",") == "z,a,m" }})
    @check(this_map_values, {{ this.values()|sum == 6 }})
    @check(this_map_items, {{ this.items()|length == 3 }})
    @check(this_map_get, {{ this.get("z") == 1 }})
    @check(this_map_get_missing, {{ this.get("q") == none }})
    @check(this_map_get_default, {{ this.get("q", 9) == 9 }})
    @check(this_map_items_filter, {{ this|items|length == 3 }})
    @check(this_map_dictsort, {{ (this|dictsort|first|first) == "a" }})
    @check(this_map_tojson, {{ (this|tojson)[2] == "z" }})
    @check(this_map_ismapping, {{ this is mapping }})
    @check(this_map_first, {{ this|first == "z" }})
    @check(this_map_eq, {{ this == {"z":1,"a":2,"m":3} }})
    @check(this_map_missing_attr, {{ this.q is undefined }})
    @check(this_map_string, {{ (this|string)[0] == "{" }})
    @check(this_map_string_len, {{ (this|string)|length == 24 }})
    @check(this_map_values_join, {{ this.values()|join(",") == "1,2,3" }})
    @check(this_map_concat, {{ (this ~ "")|length == 24 }})
}

function Batch_mapFn(topic: string) -> Batch_map {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_cls {
  v Probe
    @check(this_cls_attr, {{ this.b == 1 }})
    @check(this_cls_attr_str, {{ this.a == "x" }})
    @check(this_cls_nested, {{ this.c|sum == 3 }})
    @check(this_cls_order, {{ this|list|join(",") == "b,a,c" }})
    @check(this_cls_in, {{ "a" in this }})
    @check(this_cls_length, {{ this|length == 3 }})
    @check(this_cls_ismapping, {{ this is mapping }})
    @check(this_cls_item, {{ this["b"] == 1 }})
    @check(this_cls_keys, {{ this.keys()|join(",") == "b,a,c" }})
    @check(this_cls_string_len, {{ (this|string)|length == 33 }})
    @check(this_cls_nested_index, {{ this.c[0] == 1 }})
    @check(this_cls_concat, {{ (this ~ "")|length == 33 }})
}

function Batch_clsFn(topic: string) -> Batch_cls {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_enum {
  v Hue
    @check(this_enum_eq, {{ this == "RED" }})
    @check(this_enum_alias, {{ this == "rouge" }})
    @check(this_enum_length, {{ this|length == 3 }})
    @check(this_enum_isstring, {{ this is string }})
    @check(this_enum_in, {{ this in ["RED","GREEN"] }})
}

function Batch_enumFn(topic: string) -> Batch_enum {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Batch_null {
  v int?
    @check(this_null_eq, {{ this == none }})
    @check(this_null_isnone, {{ this is none }})
    @check(this_null_defined, {{ this is defined }})
    @check(this_null_string, {{ this|string == "none" }})
    @check(this_null_default, {{ this|default(1) == 1 }})
}

function Batch_nullFn(topic: string) -> Batch_null {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// Driven ONLY by TestStockDivisibleByZeroIsProcessFatal, from a subprocess:
// stock BAML v0.223 aborts the process on this expression.
class Iso_divzero {
  v int @check(divzero, {{ this is divisibleby(0) }})
}

function Iso_divzeroFn(topic: string) -> Iso_divzero {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_op_floordiv_zero {
  v int @check(op_floordiv_zero, {{ (1 // 0) > 0 }})
}

function Iso_op_floordiv_zeroFn(topic: string) -> Iso_op_floordiv_zero {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_op_rem_zero {
  v int @check(op_rem_zero, {{ (1 % 0) > 0 }})
}

function Iso_op_rem_zeroFn(topic: string) -> Iso_op_rem_zero {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_op_str_add_num {
  v int @check(op_str_add_num, {{ ("a" + 1) == "a1" }})
}

function Iso_op_str_add_numFn(topic: string) -> Iso_op_str_add_num {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_split {
  v int @check(f_split, {{ "a,b"|split(",")|length == 2 }})
}

function Iso_f_splitFn(topic: string) -> Iso_f_split {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_sum_str_arg {
  v int @check(f_sum_str_arg, {{ "ab"|sum == 0 }})
}

function Iso_f_sum_str_argFn(topic: string) -> Iso_f_sum_str_arg {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_sum_map {
  v int @check(f_sum_map, {{ {"a":1}|sum == 0 }})
}

function Iso_f_sum_mapFn(topic: string) -> Iso_f_sum_map {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_sum_attr_kwarg {
  v int @check(f_sum_attr_kwarg, {{ [{"a":1}]|sum(attribute="a") == 1 }})
}

function Iso_f_sum_attr_kwargFn(topic: string) -> Iso_f_sum_attr_kwarg {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_urlencode {
  v int @check(f_urlencode, {{ "a b"|urlencode == "a%20b" }})
}

function Iso_f_urlencodeFn(topic: string) -> Iso_f_urlencode {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_regex_noarg {
  v int @check(f_regex_noarg, {{ "a"|regex_match }})
}

function Iso_f_regex_noargFn(topic: string) -> Iso_f_regex_noarg {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_unknown {
  v int @check(f_unknown, {{ 1|nosuchfilter == 1 }})
}

function Iso_f_unknownFn(topic: string) -> Iso_f_unknown {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_t_containing {
  v int @check(t_containing, {{ "abc" is containing("b") }})
}

function Iso_t_containingFn(topic: string) -> Iso_t_containing {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_t_unknown {
  v int @check(t_unknown, {{ 1 is nosuchtest }})
}

function Iso_t_unknownFn(topic: string) -> Iso_t_unknown {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_fn_cycler {
  v int @check(fn_cycler, {{ cycler("a","b").next() == "a" }})
}

function Iso_fn_cyclerFn(topic: string) -> Iso_fn_cycler {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_fn_joiner {
  v int @check(fn_joiner, {{ joiner(",")() == "" }})
}

function Iso_fn_joinerFn(topic: string) -> Iso_fn_joiner {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_fn_unknown {
  v int @check(fn_unknown, {{ nosuchfn() == 1 }})
}

function Iso_fn_unknownFn(topic: string) -> Iso_fn_unknown {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_py_num_method {
  v int @check(py_num_method, {{ (1).nosuchmethod() == 1 }})
}

function Iso_py_num_methodFn(topic: string) -> Iso_py_num_method {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_int_bare {
  v int @check(this_int_bare, {{ this }})
}

function Iso_this_int_bareFn(topic: string) -> Iso_this_int_bare {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_int0_bare {
  v int @check(this_int0_bare, {{ this }})
}

function Iso_this_int0_bareFn(topic: string) -> Iso_this_int0_bare {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_str_split {
  v string @check(this_str_split, {{ this|split(" ")|length == 2 }})
}

function Iso_this_str_splitFn(topic: string) -> Iso_this_str_split {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_stre_bare {
  v string @check(this_stre_bare, {{ this }})
}

function Iso_this_stre_bareFn(topic: string) -> Iso_this_stre_bare {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_map_sum {
  v map<string, int> @check(this_map_sum, {{ this|sum == 0 }})
}

function Iso_this_map_sumFn(topic: string) -> Iso_this_map_sum {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_null_bare {
  v int? @check(this_null_bare, {{ this }})
}

function Iso_this_null_bareFn(topic: string) -> Iso_this_null_bare {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_fn_lipsum {
  v int @check(fn_lipsum, {{ lipsum(1)|length > 0 }})
}

function Iso_fn_lipsumFn(topic: string) -> Iso_fn_lipsum {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_length_none {
  v int @check(f_length_none, {{ none|length == 0 }})
}

function Iso_f_length_noneFn(topic: string) -> Iso_f_length_none {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_this_map_last {
  v map<string, int> @check(this_map_last, {{ this|last == "m" }})
}

function Iso_this_map_lastFn(topic: string) -> Iso_this_map_last {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_length_undefined {
  v int @check(f_length_undefined, {{ nosuchvar|length == 0 }})
}

function Iso_f_length_undefinedFn(topic: string) -> Iso_f_length_undefined {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class Iso_f_length_int {
  v int @check(f_length_int, {{ 1|length == 0 }})
}

function Iso_f_length_intFn(topic: string) -> Iso_f_length_int {
  client ConstraintOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
