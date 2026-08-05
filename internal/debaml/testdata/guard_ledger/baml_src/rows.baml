// GENERATED from internal/debaml/guardledger/corpus_test.go — do not edit.
//
// Regenerate with:
//   GUARD_LEDGER_FIXTURE_WRITE=1 go test -tags integration \
//     ./internal/debaml/guardledger -run TestGuardLedgerFixtureDrift
// then re-run the stock BAML v0.223.0 generator over this project (see the
// package comment).
//
// Every declaration here exists only to give the stock client a constrained
// node to evaluate. The `v` field carries the @check/@assert attributes;
// the attribute LABEL is the witness row id (suffixed `_A` at @assert
// level), so a batched result is recoverable from the checks map by name.

class GLProbe {
  b int
  a string
}

class GLInner {
  name int
  tags string[]
}

class GLNest {
  a GLInner
  name string
  rows GLInner[]
}

class GLB_int1 {
  v int
    @check(N7, {{ "9007199254740993"|int == "9007199254740992"|int }})
    @check(FMAP_LIST, {{ {"b": 1, "a": 2}|list|length == 2 }})
    @check(FMAP_DICT, {{ dict(b=1, a=2)|list|length == 2 }})
    @check(FMAP_NONSTRING_KEY, {{ {1: "a"}|list|length == 1 }})
    @check(RANGE_LIST, {{ range(3)|list|length == 3 }})
    @check(RANGE_LAST, {{ range(3)|last == 2 }})
    @check(RANGE_STEP, {{ range(3, 0, -1)|list|length == 3 }})
    @check(NAMESPACE_ATTR, {{ namespace(x=1).x == 1 }})
    @check(DEBUG_CALL, {{ debug(this)|length > 0 }})
    @check(O1, {{ 1 in "1" }})
    @check(O1b, {{ 1 not in "1" }})
    @check(O3, {{ "a" ~ 1 == "a1" }})
    @check(O4, {{ ("" or "fallback") == "fallback" }})
    @check(O5, {{ not false }})
    @check(O6, {{ (1 if true else 2) == 1 }})
    @check(O7, {{ true == 1 }})
    @check(PY_FORMAT, {{ "{:,}".format(1234567) == "1,234,567" }})
    @check(FM_LIST_LIT, {{ {"b": 1, "a": 2}|list|first == "b" }})
    @check(FM_LIST_DICT, {{ dict(b=1, a=2)|list|first == "b" }})
    @check(FM_JOIN_LIT, {{ {"b": 1, "a": 2}|join(",") == "b,a" }})
    @check(FM_JOIN_DICT, {{ dict(b=1, a=2)|join(",") == "b,a" }})
    @check(FM_FIRST_LIT, {{ {"b": 1, "a": 2}|first == "b" }})
    @check(FM_FIRST_DICT, {{ dict(b=1, a=2)|first == "b" }})
    @check(FM_MAP_LIT, {{ {"b": 1, "a": 2}|map("upper")|first == "B" }})
    @check(FM_MAP_DICT, {{ dict(b=1, a=2)|map("upper")|first == "B" }})
    @check(FM_SELECT_LIT, {{ {"b": 1, "a": 2}|select("string")|list|first == "b" }})
    @check(FM_SELECT_DICT, {{ dict(b=1, a=2)|select("string")|list|first == "b" }})
    @check(FM_REJECT_LIT, {{ {"b": 1, "a": 2}|reject("number")|list|first == "b" }})
    @check(FM_REJECT_DICT, {{ dict(b=1, a=2)|reject("number")|list|first == "b" }})
    @check(FM_SELECTATTR_LIT, {{ {"b": 1, "a": 2}|selectattr("q")|list|length == 0 }})
    @check(FM_SELECTATTR_DICT, {{ dict(b=1, a=2)|selectattr("q")|list|length == 0 }})
    @check(FM_REJECTATTR_LIT, {{ {"b": 1, "a": 2}|rejectattr("q")|list|first == "b" }})
    @check(FM_REJECTATTR_DICT, {{ dict(b=1, a=2)|rejectattr("q")|list|first == "b" }})
    @check(FM_GROUPBY_LIT, {{ {"b": 1, "a": 2}|groupby("q")|list|length == 0 }})
    @check(FM_GROUPBY_DICT, {{ dict(b=1, a=2)|groupby("q")|list|length == 0 }})
    @check(FM_CHAIN_LIT, {{ {"b": 1, "a": 2}|chain([])|list|first == "b" }})
    @check(FM_CHAIN_DICT, {{ dict(b=1, a=2)|chain([])|list|first == "b" }})
    @check(FM_ZIP_LIT, {{ {"b": 1, "a": 2}|zip([1, 2])|list|length == 2 }})
    @check(FM_ZIP_DICT, {{ dict(b=1, a=2)|zip([1, 2])|list|length == 2 }})
    @check(FM_UNIQUE_LIT, {{ {"b": 1, "a": 2}|unique|list|first == "b" }})
    @check(FM_UNIQUE_DICT, {{ dict(b=1, a=2)|unique|list|first == "b" }})
    @check(FM_BATCH_LIT, {{ {"b": 1, "a": 2}|batch(2)|list|length == 1 }})
    @check(FM_BATCH_DICT, {{ dict(b=1, a=2)|batch(2)|list|length == 1 }})
    @check(FM_SLICE_LIT, {{ {"b": 1, "a": 2}|slice(2)|list|length == 2 }})
    @check(FM_SLICE_DICT, {{ dict(b=1, a=2)|slice(2)|list|length == 2 }})
    @check(FM_REVERSE_LIT, {{ {"b": 1, "a": 2}|reverse|list|first == "a" }})
    @check(FM_REVERSE_DICT, {{ dict(b=1, a=2)|reverse|list|first == "a" }})
    @check(FM_PPRINT_LIT, {{ {"b": 1, "a": 2}|pprint|length > 0 }})
    @check(FM_PPRINT_DICT, {{ dict(b=1, a=2)|pprint|length > 0 }})
    @check(FM_STRING_LIT, {{ ({"b": 1, "a": 2}|string)[2] == "b" }})
    @check(FM_STRING_DICT, {{ (dict(b=1, a=2)|string)[2] == "b" }})
    @check(FM_INDENT_LIT, {{ {"b": 1, "a": 2}|indent(2)|length > 0 }})
    @check(FM_INDENT_DICT, {{ dict(b=1, a=2)|indent(2)|length > 0 }})
}

function GLB_int1Fn(topic: string) -> GLB_int1 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_int2 {
  v int
    @check(N4, {{ this ** 63 > 0 }})
    @check(PG_DIVISIBLEBY, {{ this is divisibleby(2) }})
}

function GLB_int2Fn(topic: string) -> GLB_int2 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_intneg7 {
  v int
    @check(N2, {{ this % 2 == 1 }})
    @check(N3, {{ this // 2 == -4 }})
    @check(PG_ABS, {{ this|abs == 7 }})
}

function GLB_intneg7Fn(topic: string) -> GLB_intneg7 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_bigint {
  v int
    @check(N1, {{ this == 9007199254740992 }})
    @check(N1b, {{ this == 9007199254740993 }})
}

function GLB_bigintFn(topic: string) -> GLB_bigint {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_f15 {
  v float
    @check(N12, {{ this is divisibleby(0.5) }})
}

function GLB_f15Fn(topic: string) -> GLB_f15 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_fbig {
  v float
    @check(N8, {{ this is even }})
}

function GLB_fbigFn(topic: string) -> GLB_fbig {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_strnum {
  v string
    @check(N9, {{ this|int == 9007199254740993 }})
}

function GLB_strnumFn(topic: string) -> GLB_strnum {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_strab {
  v string
    @check(LEN_STR, {{ this|length == 3 }})
    @check(CNT_STR, {{ this|count == 3 }})
    @check(SPLIT_LIST, {{ this|split(" ")|list|length == 2 }})
    @check(SPLIT_ITERABLE, {{ this|split(" ") is iterable }})
    @check(SPLIT_INDEX, {{ (this|split(" "))[0] == "a" }})
    @check(PY_UPPER, {{ this.upper() == "A B" }})
    @check(WT_TITLE, {{ this|title == "A B" }})
}

function GLB_strabFn(topic: string) -> GLB_strab {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_strhello {
  v string
    @check(O9c, {{ this[9] == "x" }})
    @check(O9d, {{ this[1] == "e" }})
    @check(BS_REGEX, {{ this|regex_match("\d") == false }})
}

function GLB_strhelloFn(topic: string) -> GLB_strhello {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_list123 {
  v int[]
    @check(N5, {{ this[1] == 2 }})
    @check(N6, {{ this[1.5] == 2 }})
    @check(LEN_LIST, {{ this|length == 3 }})
    @check(CNT_LIST, {{ this|count == 3 }})
    @check(LAST_LIST, {{ this|last == 3 }})
    @check(FIRST_LIST, {{ this|first == 1 }})
    @check(O9, {{ this[9] == 1 }})
    @check(O9b, {{ this[-1] == 3 }})
    @check(O9e, {{ this[1:]|length == 2 }})
    @check(WT_SORT, {{ this|sort|first == 1 }})
    @check(PG_SUM, {{ this|sum == 6 }})
}

function GLB_list123Fn(topic: string) -> GLB_list123 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_probe {
  v GLProbe
    @check(LEN_CLS, {{ this|length == 2 }})
    @check(CNT_CLS, {{ this|count == 2 }})
    @check(ITEMS_CLS, {{ this|items|list|length == 2 }})
    @check(CLS_FIELD, {{ this.b == 1 }})
    @check(CLS_LIST_ORDER, {{ this|list|first == "b" }})
    @check(ITEMS_ORDER_CLS, {{ this|items|first|first == "b" }})
}

function GLB_probeFn(topic: string) -> GLB_probe {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_mapba {
  v map<string, int>
    @check(LEN_MAP, {{ this|length == 2 }})
    @check(CNT_MAP, {{ this|count == 2 }})
    @check(ITEMS_MAP, {{ this|items|list|length == 2 }})
    @check(TOJSON_MAP, {{ this|tojson|length == 13 }})
    @check(O2, {{ "b" in this }})
    @check(O2b, {{ "z" in this }})
    @check(MAP_SUBSCRIPT, {{ this["b"] == 1 }})
    @check(MAP_LIST, {{ this|list|length == 2 }})
    @check(MAP_REVERSE_LIST, {{ this|reverse|list|length == 2 }})
    @check(MAP_STRING, {{ this|string|first == "{" }})
    @check(MAP_STRING_LEN, {{ this|string|length == 16 }})
    @check(MAP_CONCAT, {{ (this ~ "")|length == 16 }})
    @check(MAP_EQUALITY, {{ this == this }})
    @check(MAP_LIST_ORDER, {{ this|list|first == "b" }})
    @check(MAP_REVERSE_ORDER, {{ this|reverse|list|first == "a" }})
    @check(MAP_REVERSE_ORDER_B, {{ this|reverse|list|first == "b" }})
    @check(ITEMS_ORDER_MAP, {{ this|items|first|first == "b" }})
    @check(TOJSON_ORDER_MAP, {{ (this|tojson)[2] == "b" }})
    @check(MAP_STRING_ORDER, {{ (this|string)[2] == "b" }})
    @check(PG_IS_MAPPING, {{ this is mapping }})
}

function GLB_mapbaFn(topic: string) -> GLB_mapba {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_nestmap {
  v map<string, map<string, int>>
    @check(TOJSON_NEST, {{ this|tojson|length == 23 }})
    @check(ITEMS_NEST, {{ this|items|list|length == 1 }})
    @check(MAP_NESTED, {{ this["outer"]["b"] == 1 }})
    @check(TOJSON_ORDER_NEST, {{ (this|tojson)[11] == "b" }})
}

function GLB_nestmapFn(topic: string) -> GLB_nestmap {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLB_nest {
  v GLNest
    @check(O8, {{ this.rows[0].name == 7 }})
    @check(O8b, {{ this.a.name == "x" }})
    @check(CLS_NESTED_LIST, {{ this.a.tags[0] == "x" }})
}

function GLB_nestFn(topic: string) -> GLB_nest {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_int1 {
  v int
    @assert(FMAP_LIST_A, {{ {"b": 1, "a": 2}|list|length == 2 }})
    @assert(FMAP_DICT_A, {{ dict(b=1, a=2)|list|length == 2 }})
    @assert(FMAP_NONSTRING_KEY_A, {{ {1: "a"}|list|length == 1 }})
    @assert(RANGE_LIST_A, {{ range(3)|list|length == 3 }})
    @assert(RANGE_LAST_A, {{ range(3)|last == 2 }})
    @assert(RANGE_STEP_A, {{ range(3, 0, -1)|list|length == 3 }})
    @assert(NAMESPACE_ATTR_A, {{ namespace(x=1).x == 1 }})
    @assert(DEBUG_CALL_A, {{ debug(this)|length > 0 }})
    @assert(O1_A, {{ 1 in "1" }})
    @assert(O3_A, {{ "a" ~ 1 == "a1" }})
    @assert(O4_A, {{ ("" or "fallback") == "fallback" }})
    @assert(O5_A, {{ not false }})
    @assert(O6_A, {{ (1 if true else 2) == 1 }})
    @assert(O7_A, {{ true == 1 }})
    @assert(PY_FORMAT_A, {{ "{:,}".format(1234567) == "1,234,567" }})
    @assert(FM_LIST_LIT_A, {{ {"b": 1, "a": 2}|list|first == "b" }})
    @assert(FM_LIST_DICT_A, {{ dict(b=1, a=2)|list|first == "b" }})
    @assert(FM_JOIN_LIT_A, {{ {"b": 1, "a": 2}|join(",") == "b,a" }})
    @assert(FM_JOIN_DICT_A, {{ dict(b=1, a=2)|join(",") == "b,a" }})
    @assert(FM_FIRST_LIT_A, {{ {"b": 1, "a": 2}|first == "b" }})
    @assert(FM_FIRST_DICT_A, {{ dict(b=1, a=2)|first == "b" }})
    @assert(FM_MAP_LIT_A, {{ {"b": 1, "a": 2}|map("upper")|first == "B" }})
    @assert(FM_MAP_DICT_A, {{ dict(b=1, a=2)|map("upper")|first == "B" }})
    @assert(FM_SELECT_LIT_A, {{ {"b": 1, "a": 2}|select("string")|list|first == "b" }})
    @assert(FM_SELECT_DICT_A, {{ dict(b=1, a=2)|select("string")|list|first == "b" }})
    @assert(FM_REJECT_LIT_A, {{ {"b": 1, "a": 2}|reject("number")|list|first == "b" }})
    @assert(FM_REJECT_DICT_A, {{ dict(b=1, a=2)|reject("number")|list|first == "b" }})
    @assert(FM_SELECTATTR_LIT_A, {{ {"b": 1, "a": 2}|selectattr("q")|list|length == 0 }})
    @assert(FM_SELECTATTR_DICT_A, {{ dict(b=1, a=2)|selectattr("q")|list|length == 0 }})
    @assert(FM_REJECTATTR_LIT_A, {{ {"b": 1, "a": 2}|rejectattr("q")|list|first == "b" }})
    @assert(FM_REJECTATTR_DICT_A, {{ dict(b=1, a=2)|rejectattr("q")|list|first == "b" }})
    @assert(FM_CHAIN_LIT_A, {{ {"b": 1, "a": 2}|chain([])|list|first == "b" }})
    @assert(FM_CHAIN_DICT_A, {{ dict(b=1, a=2)|chain([])|list|first == "b" }})
    @assert(FM_ZIP_LIT_A, {{ {"b": 1, "a": 2}|zip([1, 2])|list|length == 2 }})
    @assert(FM_ZIP_DICT_A, {{ dict(b=1, a=2)|zip([1, 2])|list|length == 2 }})
    @assert(FM_UNIQUE_LIT_A, {{ {"b": 1, "a": 2}|unique|list|first == "b" }})
    @assert(FM_UNIQUE_DICT_A, {{ dict(b=1, a=2)|unique|list|first == "b" }})
    @assert(FM_BATCH_LIT_A, {{ {"b": 1, "a": 2}|batch(2)|list|length == 1 }})
    @assert(FM_BATCH_DICT_A, {{ dict(b=1, a=2)|batch(2)|list|length == 1 }})
    @assert(FM_SLICE_LIT_A, {{ {"b": 1, "a": 2}|slice(2)|list|length == 2 }})
    @assert(FM_SLICE_DICT_A, {{ dict(b=1, a=2)|slice(2)|list|length == 2 }})
    @assert(FM_PPRINT_LIT_A, {{ {"b": 1, "a": 2}|pprint|length > 0 }})
    @assert(FM_PPRINT_DICT_A, {{ dict(b=1, a=2)|pprint|length > 0 }})
    @assert(FM_STRING_LIT_A, {{ ({"b": 1, "a": 2}|string)[2] == "b" }})
    @assert(FM_STRING_DICT_A, {{ (dict(b=1, a=2)|string)[2] == "b" }})
    @assert(FM_INDENT_LIT_A, {{ {"b": 1, "a": 2}|indent(2)|length > 0 }})
    @assert(FM_INDENT_DICT_A, {{ dict(b=1, a=2)|indent(2)|length > 0 }})
}

function GLA_int1Fn(topic: string) -> GLA_int1 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_int2 {
  v int
    @assert(N4_A, {{ this ** 63 > 0 }})
    @assert(PG_DIVISIBLEBY_A, {{ this is divisibleby(2) }})
}

function GLA_int2Fn(topic: string) -> GLA_int2 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_intneg7 {
  v int
    @assert(N2_A, {{ this % 2 == 1 }})
    @assert(N3_A, {{ this // 2 == -4 }})
    @assert(PG_ABS_A, {{ this|abs == 7 }})
}

function GLA_intneg7Fn(topic: string) -> GLA_intneg7 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_bigint {
  v int
    @assert(N1b_A, {{ this == 9007199254740993 }})
}

function GLA_bigintFn(topic: string) -> GLA_bigint {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_f15 {
  v float
    @assert(N12_A, {{ this is divisibleby(0.5) }})
}

function GLA_f15Fn(topic: string) -> GLA_f15 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_strnum {
  v string
    @assert(N9_A, {{ this|int == 9007199254740993 }})
}

function GLA_strnumFn(topic: string) -> GLA_strnum {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_strab {
  v string
    @assert(LEN_STR_A, {{ this|length == 3 }})
    @assert(CNT_STR_A, {{ this|count == 3 }})
    @assert(SPLIT_LIST_A, {{ this|split(" ")|list|length == 2 }})
    @assert(SPLIT_ITERABLE_A, {{ this|split(" ") is iterable }})
    @assert(SPLIT_INDEX_A, {{ (this|split(" "))[0] == "a" }})
    @assert(PY_UPPER_A, {{ this.upper() == "A B" }})
    @assert(WT_TITLE_A, {{ this|title == "A B" }})
}

function GLA_strabFn(topic: string) -> GLA_strab {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_strhello {
  v string
    @assert(O9d_A, {{ this[1] == "e" }})
    @assert(BS_REGEX_A, {{ this|regex_match("\d") == false }})
}

function GLA_strhelloFn(topic: string) -> GLA_strhello {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_list123 {
  v int[]
    @assert(N5_A, {{ this[1] == 2 }})
    @assert(LEN_LIST_A, {{ this|length == 3 }})
    @assert(CNT_LIST_A, {{ this|count == 3 }})
    @assert(LAST_LIST_A, {{ this|last == 3 }})
    @assert(FIRST_LIST_A, {{ this|first == 1 }})
    @assert(O9b_A, {{ this[-1] == 3 }})
    @assert(O9e_A, {{ this[1:]|length == 2 }})
    @assert(WT_SORT_A, {{ this|sort|first == 1 }})
    @assert(PG_SUM_A, {{ this|sum == 6 }})
}

function GLA_list123Fn(topic: string) -> GLA_list123 {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_probe {
  v GLProbe
    @assert(LEN_CLS_A, {{ this|length == 2 }})
    @assert(CNT_CLS_A, {{ this|count == 2 }})
    @assert(ITEMS_CLS_A, {{ this|items|list|length == 2 }})
    @assert(CLS_FIELD_A, {{ this.b == 1 }})
    @assert(CLS_LIST_ORDER_A, {{ this|list|first == "b" }})
    @assert(ITEMS_ORDER_CLS_A, {{ this|items|first|first == "b" }})
}

function GLA_probeFn(topic: string) -> GLA_probe {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_mapba {
  v map<string, int>
    @assert(LEN_MAP_A, {{ this|length == 2 }})
    @assert(CNT_MAP_A, {{ this|count == 2 }})
    @assert(ITEMS_MAP_A, {{ this|items|list|length == 2 }})
    @assert(TOJSON_MAP_A, {{ this|tojson|length == 13 }})
    @assert(O2_A, {{ "b" in this }})
    @assert(MAP_SUBSCRIPT_A, {{ this["b"] == 1 }})
    @assert(MAP_LIST_A, {{ this|list|length == 2 }})
    @assert(MAP_REVERSE_LIST_A, {{ this|reverse|list|length == 2 }})
    @assert(MAP_STRING_A, {{ this|string|first == "{" }})
    @assert(MAP_STRING_LEN_A, {{ this|string|length == 16 }})
    @assert(MAP_CONCAT_A, {{ (this ~ "")|length == 16 }})
    @assert(MAP_EQUALITY_A, {{ this == this }})
    @assert(MAP_LIST_ORDER_A, {{ this|list|first == "b" }})
    @assert(MAP_REVERSE_ORDER_B_A, {{ this|reverse|list|first == "b" }})
    @assert(ITEMS_ORDER_MAP_A, {{ this|items|first|first == "b" }})
    @assert(TOJSON_ORDER_MAP_A, {{ (this|tojson)[2] == "b" }})
    @assert(MAP_STRING_ORDER_A, {{ (this|string)[2] == "b" }})
    @assert(PG_IS_MAPPING_A, {{ this is mapping }})
}

function GLA_mapbaFn(topic: string) -> GLA_mapba {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_nestmap {
  v map<string, map<string, int>>
    @assert(TOJSON_NEST_A, {{ this|tojson|length == 23 }})
    @assert(ITEMS_NEST_A, {{ this|items|list|length == 1 }})
    @assert(MAP_NESTED_A, {{ this["outer"]["b"] == 1 }})
    @assert(TOJSON_ORDER_NEST_A, {{ (this|tojson)[11] == "b" }})
}

function GLA_nestmapFn(topic: string) -> GLA_nestmap {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLA_nest {
  v GLNest
    @assert(O8_A, {{ this.rows[0].name == 7 }})
    @assert(CLS_NESTED_LIST_A, {{ this.a.tags[0] == "x" }})
}

function GLA_nestFn(topic: string) -> GLA_nest {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N1_assert {
  v int @assert(N1_A, {{ this == 9007199254740992 }})
}

function GLI_N1_assertFn(topic: string) -> GLI_N1_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N6_assert {
  v int[] @assert(N6_A, {{ this[1.5] == 2 }})
}

function GLI_N6_assertFn(topic: string) -> GLI_N6_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N7_assert {
  v int @assert(N7_A, {{ "9007199254740993"|int == "9007199254740992"|int }})
}

function GLI_N7_assertFn(topic: string) -> GLI_N7_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N8_assert {
  v float @assert(N8_A, {{ this is even }})
}

function GLI_N8_assertFn(topic: string) -> GLI_N8_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N10_check {
  v int[] @check(N10, {{ this|slice(0)|length == 1 }})
}

function GLI_N10_checkFn(topic: string) -> GLI_N10_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N10_assert {
  v int[] @assert(N10_A, {{ this|slice(0)|length == 1 }})
}

function GLI_N10_assertFn(topic: string) -> GLI_N10_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N11_check {
  v int[] @check(N11, {{ this|slice(1.5)|length == 1 }})
}

function GLI_N11_checkFn(topic: string) -> GLI_N11_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_N11_assert {
  v int[] @assert(N11_A, {{ this|slice(1.5)|length == 1 }})
}

function GLI_N11_assertFn(topic: string) -> GLI_N11_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LEN_INT_check {
  v int @check(LEN_INT, {{ this|length == 0 }})
}

function GLI_LEN_INT_checkFn(topic: string) -> GLI_LEN_INT_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LEN_INT_assert {
  v int @assert(LEN_INT_A, {{ this|length == 0 }})
}

function GLI_LEN_INT_assertFn(topic: string) -> GLI_LEN_INT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LEN_BOOL_check {
  v bool @check(LEN_BOOL, {{ this|length == 0 }})
}

function GLI_LEN_BOOL_checkFn(topic: string) -> GLI_LEN_BOOL_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LEN_BOOL_assert {
  v bool @assert(LEN_BOOL_A, {{ this|length == 0 }})
}

function GLI_LEN_BOOL_assertFn(topic: string) -> GLI_LEN_BOOL_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LEN_NULL_check {
  v int? @check(LEN_NULL, {{ this|length == 0 }})
}

function GLI_LEN_NULL_checkFn(topic: string) -> GLI_LEN_NULL_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_CNT_INT_check {
  v int @check(CNT_INT, {{ this|count == 0 }})
}

function GLI_CNT_INT_checkFn(topic: string) -> GLI_CNT_INT_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_CNT_INT_assert {
  v int @assert(CNT_INT_A, {{ this|count == 0 }})
}

function GLI_CNT_INT_assertFn(topic: string) -> GLI_CNT_INT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_CNT_BOOL_check {
  v bool @check(CNT_BOOL, {{ this|count == 0 }})
}

function GLI_CNT_BOOL_checkFn(topic: string) -> GLI_CNT_BOOL_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_CNT_BOOL_assert {
  v bool @assert(CNT_BOOL_A, {{ this|count == 0 }})
}

function GLI_CNT_BOOL_assertFn(topic: string) -> GLI_CNT_BOOL_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_CNT_NULL_check {
  v int? @check(CNT_NULL, {{ this|count == 0 }})
}

function GLI_CNT_NULL_checkFn(topic: string) -> GLI_CNT_NULL_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_CLS_VALUE_check {
  v GLProbe @check(LAST_CLS_VALUE, {{ this|last == "x" }})
}

function GLI_LAST_CLS_VALUE_checkFn(topic: string) -> GLI_LAST_CLS_VALUE_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_CLS_VALUE_assert {
  v GLProbe @assert(LAST_CLS_VALUE_A, {{ this|last == "x" }})
}

function GLI_LAST_CLS_VALUE_assertFn(topic: string) -> GLI_LAST_CLS_VALUE_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_CLS_KEY_check {
  v GLProbe @check(LAST_CLS_KEY, {{ this|last == "a" }})
}

function GLI_LAST_CLS_KEY_checkFn(topic: string) -> GLI_LAST_CLS_KEY_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_CLS_KEY_assert {
  v GLProbe @assert(LAST_CLS_KEY_A, {{ this|last == "a" }})
}

function GLI_LAST_CLS_KEY_assertFn(topic: string) -> GLI_LAST_CLS_KEY_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_MAP_KEY_check {
  v map<string, int> @check(LAST_MAP_KEY, {{ this|last == "a" }})
}

function GLI_LAST_MAP_KEY_checkFn(topic: string) -> GLI_LAST_MAP_KEY_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_LAST_MAP_KEY_assert {
  v map<string, int> @assert(LAST_MAP_KEY_A, {{ this|last == "a" }})
}

function GLI_LAST_MAP_KEY_assertFn(topic: string) -> GLI_LAST_MAP_KEY_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_SPLIT_LENGTH_check {
  v string @check(SPLIT_LENGTH, {{ this|split(" ")|length == 2 }})
}

function GLI_SPLIT_LENGTH_checkFn(topic: string) -> GLI_SPLIT_LENGTH_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_SPLIT_LENGTH_assert {
  v string @assert(SPLIT_LENGTH_A, {{ this|split(" ")|length == 2 }})
}

function GLI_SPLIT_LENGTH_assertFn(topic: string) -> GLI_SPLIT_LENGTH_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_DICT_ARITY_check {
  v int @check(DICT_ARITY, {{ dict(1)|length == 0 }})
}

function GLI_DICT_ARITY_checkFn(topic: string) -> GLI_DICT_ARITY_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_DICT_ARITY_assert {
  v int @assert(DICT_ARITY_A, {{ dict(1)|length == 0 }})
}

function GLI_DICT_ARITY_assertFn(topic: string) -> GLI_DICT_ARITY_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_URLENCODE_check {
  v string @check(WB_URLENCODE, {{ this|urlencode == "ab" }})
}

function GLI_WB_URLENCODE_checkFn(topic: string) -> GLI_WB_URLENCODE_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_URLENCODE_assert {
  v string @assert(WB_URLENCODE_A, {{ this|urlencode == "ab" }})
}

function GLI_WB_URLENCODE_assertFn(topic: string) -> GLI_WB_URLENCODE_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_CONTAINING_check {
  v string @check(WB_CONTAINING, {{ this is containing("a") }})
}

function GLI_WB_CONTAINING_checkFn(topic: string) -> GLI_WB_CONTAINING_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_CONTAINING_assert {
  v string @assert(WB_CONTAINING_A, {{ this is containing("a") }})
}

function GLI_WB_CONTAINING_assertFn(topic: string) -> GLI_WB_CONTAINING_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_CYCLER_check {
  v int @check(WB_CYCLER, {{ cycler(1, 2) == 1 }})
}

function GLI_WB_CYCLER_checkFn(topic: string) -> GLI_WB_CYCLER_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_CYCLER_assert {
  v int @assert(WB_CYCLER_A, {{ cycler(1, 2) == 1 }})
}

function GLI_WB_CYCLER_assertFn(topic: string) -> GLI_WB_CYCLER_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_JOINER_check {
  v int @check(WB_JOINER, {{ joiner() == 1 }})
}

function GLI_WB_JOINER_checkFn(topic: string) -> GLI_WB_JOINER_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_JOINER_assert {
  v int @assert(WB_JOINER_A, {{ joiner() == 1 }})
}

function GLI_WB_JOINER_assertFn(topic: string) -> GLI_WB_JOINER_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_LIPSUM_check {
  v int @check(WB_LIPSUM, {{ lipsum() == 1 }})
}

function GLI_WB_LIPSUM_checkFn(topic: string) -> GLI_WB_LIPSUM_check {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_WB_LIPSUM_assert {
  v int @assert(WB_LIPSUM_A, {{ lipsum() == 1 }})
}

function GLI_WB_LIPSUM_assertFn(topic: string) -> GLI_WB_LIPSUM_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_O1b_assert {
  v int @assert(O1b_A, {{ 1 not in "1" }})
}

function GLI_O1b_assertFn(topic: string) -> GLI_O1b_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_O2b_assert {
  v map<string, int> @assert(O2b_A, {{ "z" in this }})
}

function GLI_O2b_assertFn(topic: string) -> GLI_O2b_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_O8b_assert {
  v GLNest @assert(O8b_A, {{ this.a.name == "x" }})
}

function GLI_O8b_assertFn(topic: string) -> GLI_O8b_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_O9_assert {
  v int[] @assert(O9_A, {{ this[9] == 1 }})
}

function GLI_O9_assertFn(topic: string) -> GLI_O9_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_O9c_assert {
  v string @assert(O9c_A, {{ this[9] == "x" }})
}

function GLI_O9c_assertFn(topic: string) -> GLI_O9c_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_MAP_REVERSE_ORDER_assert {
  v map<string, int> @assert(MAP_REVERSE_ORDER_A, {{ this|reverse|list|first == "a" }})
}

function GLI_MAP_REVERSE_ORDER_assertFn(topic: string) -> GLI_MAP_REVERSE_ORDER_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_FM_GROUPBY_LIT_assert {
  v int @assert(FM_GROUPBY_LIT_A, {{ {"b": 1, "a": 2}|groupby("q")|list|length == 0 }})
}

function GLI_FM_GROUPBY_LIT_assertFn(topic: string) -> GLI_FM_GROUPBY_LIT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_FM_GROUPBY_DICT_assert {
  v int @assert(FM_GROUPBY_DICT_A, {{ dict(b=1, a=2)|groupby("q")|list|length == 0 }})
}

function GLI_FM_GROUPBY_DICT_assertFn(topic: string) -> GLI_FM_GROUPBY_DICT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_FM_REVERSE_LIT_assert {
  v int @assert(FM_REVERSE_LIT_A, {{ {"b": 1, "a": 2}|reverse|list|first == "a" }})
}

function GLI_FM_REVERSE_LIT_assertFn(topic: string) -> GLI_FM_REVERSE_LIT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLI_FM_REVERSE_DICT_assert {
  v int @assert(FM_REVERSE_DICT_A, {{ dict(b=1, a=2)|reverse|list|first == "a" }})
}

function GLI_FM_REVERSE_DICT_assertFn(topic: string) -> GLI_FM_REVERSE_DICT_assert {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// Driven ONLY by TestGuardLedgerDuplicateLabelIsLastWriteWins: two @check
// attributes under one label, so the collapse is observed rather than assumed.
class GLD_dup {
  v int
    @check(dup, {{ this == 1 }})
    @check(dup, {{ this == 2 }})
}

function GLD_dupFn(topic: string) -> GLD_dup {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// Driven ONLY from a SUBPROCESS (fatal_test.go): stock BAML v0.223 aborts or
// hangs its CFFI process on these, so they can never ride in this binary.
class GLF_divzero {
  v int @check(divzero, {{ this is divisibleby(0) }})
}

function GLF_divzeroFn(topic: string) -> GLF_divzero {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class GLF_hugerange {
  v int @check(hugerange, {{ range(1000000000000)|length == 1000000000000 }})
}

function GLF_hugerangeFn(topic: string) -> GLF_hugerange {
  client GuardLedgerClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
