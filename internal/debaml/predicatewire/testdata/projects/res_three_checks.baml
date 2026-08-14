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

// Slice 7.2c-1 RESIDUAL (captured, NEVER admitted):
// THREE UNIQUE @check attributes — the same instability with more keys, so the
// observation is not a two-element coincidence.

class StaticCheckedAnswer {
  answer string
  confidence int @check(alpha, {{ this > 0 }}) @check(beta, {{ this < 100 }}) @check(gamma, {{ this != 7 }})
}

// RESIDUAL: driven for characterization only.
function PW_Checked(topic: string) -> StaticCheckedAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
