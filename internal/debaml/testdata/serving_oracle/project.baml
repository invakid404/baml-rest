// GENERATED at test time from the Slice 7.2a-3 serving-oracle corpus — do not edit.
//
// Every declaration below is rendered from the schema.Bundle of one corpus
// fixture (serving_oracle_corpus_test.go) by soRenderProject, so the .baml the
// stock BAML v0.223.0 CFFI compiles and the schema.Bundle the native collector
// coerces against are the same description of the same shape.
//
// Regenerate with:
//   BAML_SERVING_ORACLE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml -run TestServingOracleProjectDrift

client<llm> ServingOracleClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://serving-oracle.invalid/v1"
  }
}

class SoIntPass {
  v int @check(gt, {{ this > 0 }})
}

class SoIntFail {
  v int @check(gt, {{ this > 100 }})
}

class SoIntAssertPass {
  v int @assert(gt, {{ this > 0 }})
}

class SoIntAssertFail {
  v int @assert(gt, {{ this > 100 }})
}

class SoFloat {
  v float @check(gt, {{ this > 1.0 }})
}

class SoBool {
  v bool @check(istrue, {{ this }})
}

class SoStrField {
  v string @check(eq, {{ this == "expected" }})
}

class SoOptNull {
  v int? @check(isnull, {{ this == none }})
}

class SoMulti {
  v int @check(a, {{ this > 0 }}) @check(b, {{ this > 3 }}) @check(c, {{ this > 100 }})
}

enum SoSuit {
  Hearts @alias("hearts_alias")
  Spades
}

class SoEnumCanon {
  v SoSuit @check(spades, {{ this == "Spades" }})
}

class SoEnumShadow {
  v SoSuit @check(hearts, {{ this == "Hearts" }})
}

class SoEnumAlias {
  v SoSuit @check(canonical, {{ this == "Hearts" }}) @check(alias, {{ this == "hearts_alias" }})
}

class SoClsLevel {
  s string
  n int
  @@check(hass, {{ this.s|length > 0 }})
}

class SoClsAssert {
  s string
  @@assert(long, {{ this.s|length > 10 }})
}

class SoOrder {
  b int @check(bpos, {{ this > 0 }})
  a string @check(astr, {{ this == "x" }})
}

class SoAliasCls {
  amount int @alias("qty")
  @@check(amount, {{ this.amount == 3 }})
}

class SoOuter {
  i SoInner @check(innerb, {{ this.b > 0 }})
}

class SoInner {
  b int @check(bpos, {{ this > 1 }})
  a string
}

class SoListType {
  v int[] @check(len, {{ this|length == 3 }})
}

class SoListElem {
  v (int @check(pos, {{ this > 0 }}))[]
}

class SoListDrop {
  v (int @check(pos, {{ this > 0 }}))[]
}

class SoNestDrop {
  v SoNestDropInner[]
}

class SoNestDropInner {
  w (int @check(pos, {{ this > 0 }}))[]
}

class SoListWrap {
  v (int @check(pos, {{ this > 0 }}))[]
}

class SoMapVal {
  v map<string, (int @check(pos, {{ this > 0 }}))>
}

class SoMapKey {
  v map<(string @check(k, {{ this|length > 0 }})), int>
}

class SoUnionInt {
  v (int @check(pos, {{ this > 0 }})) | string
}

class SoUnionStr {
  v (int @check(pos, {{ this > 0 }})) | string
}

class SoDup {
  v int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 100 }})
}

class SoDupSame {
  v int @check(a, {{ this > 0 }}) @check(b, {{ this > 3 }}) @check(a, {{ this > 0 }})
}

class SoErrFilter {
  v int @check(uf, {{ this|nosuchfilter }})
}

class SoErrNonBool {
  v int @check(nb, {{ this + 1 }})
}

class SoErrMethod {
  v string @check(um, {{ this.nosuchmethod() }})
}

class SoErrOpt {
  v int? @check(uf, {{ this|nosuchfilter }})
}

class SoBigInt {
  v int @check(gt, {{ this > 9007199254740992 }})
}

class SoBigFloat {
  v float @check(gt, {{ this > 1.0 }})
}

class SoStrNum {
  v string @check(eq, {{ this == "9007199254740993" }})
}

class SoArith {
  v int @check(ar, {{ (this + 1) * 2 == 12 }})
}

class SoLen {
  v string @check(len, {{ this|length == 5 }})
}

class SoPyFmt {
  v string @check(fmt, {{ "{:,}".format(1234567) == "1,234,567" }})
}

class SoRegex {
  v string @check(rx, {{ this|regex_match("abc") }})
}

class SoDivZero {
  v int @check(dz, {{ this is divisibleby(0) }})
}

class SoFenced {
  v int @check(gt, {{ this > 0 }})
}

class SoCommented {
  v int @check(gt, {{ this > 0 }})
}

class SoCtlCls {
  b int
  a string
}

class SoCtlList {
  v int[]
}

class SoCtlMap {
  v map<string, int>
}

function SO_scalar_int_check_pass(topic: string) -> SoIntPass {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_int_check_fail(topic: string) -> SoIntFail {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_int_assert_pass(topic: string) -> SoIntAssertPass {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_int_assert_fail(topic: string) -> SoIntAssertFail {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_float_check(topic: string) -> SoFloat {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_bool_check(topic: string) -> SoBool {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_string_check_fail(topic: string) -> SoStrField {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_optional_null(topic: string) -> SoOptNull {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_scalar_multi_check_order(topic: string) -> SoMulti {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_enum_canonical(topic: string) -> SoEnumCanon {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_enum_alias_shadows_canonical(topic: string) -> SoEnumShadow {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_enum_alias_ingress(topic: string) -> SoEnumAlias {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_class_level_check(topic: string) -> SoClsLevel {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_class_level_assert_fail(topic: string) -> SoClsAssert {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_class_schema_order(topic: string) -> SoOrder {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_class_field_alias(topic: string) -> SoAliasCls {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_class_nested(topic: string) -> SoOuter {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_list_type_check(topic: string) -> SoListType {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_list_elem_check(topic: string) -> SoListElem {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_list_dropped_elem(topic: string) -> SoListDrop {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_list_nested_dropped_elem(topic: string) -> SoNestDrop {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_list_single_to_array(topic: string) -> SoListWrap {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_map_value_check(topic: string) -> SoMapVal {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_map_key_check(topic: string) -> SoMapKey {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_union_constrained_arm_wins(topic: string) -> SoUnionInt {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_union_constrained_arm_loses(topic: string) -> SoUnionStr {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_union_target_level(topic: string) -> (int @check(pos, {{ this > 0 }})) | string {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_int_check_fail(topic: string) -> int @check(gt, {{ this > 100 }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_int_assert_fail(topic: string) -> int @assert(gt, {{ this > 100 }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_string_check_skipped(topic: string) -> string @check(eq, {{ this == "expected" }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_string_assert_skipped(topic: string) -> string @assert(eq, {{ this == "expected" }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_string_bare_word(topic: string) -> string @check(eq, {{ this == "expected" }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_target_list_elem_check(topic: string) -> (int @check(pos, {{ this > 0 }}))[] {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_duplicate_label(topic: string) -> SoDup {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_duplicate_identical_nonadjacent(topic: string) -> SoDupSame {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_err_unknown_filter(topic: string) -> SoErrFilter {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_err_non_boolean(topic: string) -> SoErrNonBool {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_err_unknown_method(topic: string) -> SoErrMethod {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_err_optional_swallows(topic: string) -> SoErrOpt {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_guard_int_above_2p53(topic: string) -> SoBigInt {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_guard_float_2p63(topic: string) -> SoBigFloat {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_guard_string_number(topic: string) -> SoStrNum {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_guard_arithmetic(topic: string) -> SoArith {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_guard_length_filter(topic: string) -> SoLen {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_decline_pycompat_format(topic: string) -> SoPyFmt {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_decline_regex_match(topic: string) -> SoRegex {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_decline_divisible_by_zero(topic: string) -> SoDivZero {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_serving_markdown_fence(topic: string) -> SoFenced {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_serving_jsonish_comments(topic: string) -> SoCommented {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_control_class_unconstrained(topic: string) -> SoCtlCls {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_control_list_unconstrained(topic: string) -> SoCtlList {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_control_map_unconstrained(topic: string) -> SoCtlMap {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_control_target_string(topic: string) -> string {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_bare_string_quoted_text(topic: string) -> string {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_probe_root_duplicate_label(topic: string) -> int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 100 }}) {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

class SoProbeNestedDup {
  v int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 100 }})
}

function SO_probe_nested_duplicate_label(topic: string) -> SoProbeNestedDup {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
