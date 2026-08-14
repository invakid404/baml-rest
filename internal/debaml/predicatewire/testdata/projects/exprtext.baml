// GENERATED at test time from the Slice 7.2c-1 predicate-wire project table
// (projects_test.go) — do not edit.
//
// Regenerate with:
//   PREDICATE_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml/predicatewire -run TestPredicateWireProjectDrift

client<llm> PredicateWireClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://predicate-wire.invalid/v1"
  }
}

// Slice 7.2c-1 EXPRESSION TEXT: what stock retains in Check.Expression for each of the
// six canonical predicates under zero, one and two ASCII spaces of source padding.
// Bare `int` targets, so all of them fit one project without touching a pinned class name.

// EXPRESSION TEXT: 0 space(s) each side of "this > 0".
function PW_Pad_pad0_gt(topic: string) -> int @check(pad0_gt, {{this > 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this > 0".
function PW_Pad_pad1_gt(topic: string) -> int @check(pad1_gt, {{ this > 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 0 space(s) each side of "this >= 0".
function PW_Pad_pad0_ge(topic: string) -> int @check(pad0_ge, {{this >= 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this >= 0".
function PW_Pad_pad1_ge(topic: string) -> int @check(pad1_ge, {{ this >= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 2 space(s) each side of "this >= 0".
function PW_Pad_pad2_ge(topic: string) -> int @check(pad2_ge, {{  this >= 0  }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 0 space(s) each side of "this < 0".
function PW_Pad_pad0_lt(topic: string) -> int @check(pad0_lt, {{this < 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this < 0".
function PW_Pad_pad1_lt(topic: string) -> int @check(pad1_lt, {{ this < 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 0 space(s) each side of "this <= 0".
function PW_Pad_pad0_le(topic: string) -> int @check(pad0_le, {{this <= 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this <= 0".
function PW_Pad_pad1_le(topic: string) -> int @check(pad1_le, {{ this <= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 0 space(s) each side of "this == 0".
function PW_Pad_pad0_eq(topic: string) -> int @check(pad0_eq, {{this == 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this == 0".
function PW_Pad_pad1_eq(topic: string) -> int @check(pad1_eq, {{ this == 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 0 space(s) each side of "this != 0".
function PW_Pad_pad0_ne(topic: string) -> int @check(pad0_ne, {{this != 0}}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// EXPRESSION TEXT: 1 space(s) each side of "this != 0".
function PW_Pad_pad1_ne(topic: string) -> int @check(pad1_ne, {{ this != 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
