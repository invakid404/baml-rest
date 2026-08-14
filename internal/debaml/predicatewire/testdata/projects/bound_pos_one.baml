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

// Slice 7.2c-1 DIRECT-i64 BOUNDARY: all six operators against 1 (the smallest positive threshold).
// One function, six checks, so one parse yields the whole truth table for this literal.

// BOUNDARY: six direct comparisons against 1.
function PW_Bound(topic: string) -> int @check(gt, {{ this > 1 }}) @check(ge, {{ this >= 1 }}) @check(lt, {{ this < 1 }}) @check(le, {{ this <= 1 }}) @check(eq, {{ this == 1 }}) @check(ne, {{ this != 1 }}) {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
