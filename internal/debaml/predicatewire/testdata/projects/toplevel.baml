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

// Slice 7.2c-1 TOP-LEVEL OPERATOR CAPTURE: the six direct comparisons on a BARE `int`
// target, at both levels. The nested twins live in the op_* projects; these are the
// outcomes that cannot be inferred from them — an unenclosed carrier, and an assert
// failure with no required-field wrapper.

// WIRE: top-level @check "this > 0", driven true and false.
function PW_Top_Check_gt(topic: string) -> int @check(positive, {{ this > 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this > 0", driven true and false.
function PW_Top_Assert_gt(topic: string) -> int @assert(positive, {{ this > 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: top-level @check "this >= 0", driven true and false.
function PW_Top_Check_ge(topic: string) -> int @check(positive, {{ this >= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this >= 0", driven true and false.
function PW_Top_Assert_ge(topic: string) -> int @assert(positive, {{ this >= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: top-level @check "this < 0", driven true and false.
function PW_Top_Check_lt(topic: string) -> int @check(positive, {{ this < 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this < 0", driven true and false.
function PW_Top_Assert_lt(topic: string) -> int @assert(positive, {{ this < 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: top-level @check "this <= 0", driven true and false.
function PW_Top_Check_le(topic: string) -> int @check(positive, {{ this <= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this <= 0", driven true and false.
function PW_Top_Assert_le(topic: string) -> int @assert(positive, {{ this <= 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: top-level @check "this == 0", driven true and false.
function PW_Top_Check_eq(topic: string) -> int @check(positive, {{ this == 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this == 0", driven true and false.
function PW_Top_Assert_eq(topic: string) -> int @assert(positive, {{ this == 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE: top-level @check "this != 0", driven true and false.
function PW_Top_Check_ne(topic: string) -> int @check(positive, {{ this != 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: top-level @assert "this != 0", driven true and false.
function PW_Top_Assert_ne(topic: string) -> int @assert(positive, {{ this != 0 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
