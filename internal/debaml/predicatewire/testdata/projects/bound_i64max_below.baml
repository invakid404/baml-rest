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

// Slice 7.2c-1 DIRECT-i64 BOUNDARY: all six operators against 9223372036854775806 (math.MaxInt64-1).
// One function, six checks, so one parse yields the whole truth table for this literal.

// BOUNDARY: six direct comparisons against 9223372036854775806.
function PW_Bound(topic: string) -> int @check(gt, {{ this > 9223372036854775806 }}) @check(ge, {{ this >= 9223372036854775806 }}) @check(lt, {{ this < 9223372036854775806 }}) @check(le, {{ this <= 9223372036854775806 }}) @check(eq, {{ this == 9223372036854775806 }}) @check(ne, {{ this != 9223372036854775806 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
