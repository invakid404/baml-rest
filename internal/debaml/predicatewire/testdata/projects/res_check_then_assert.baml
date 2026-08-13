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
// MIXED, @check FIRST. Driven with the assert HOLDING (check also holds) and with the
// assert FALSE while the check would otherwise have been emitted — the state the
// one-constraint mapper and one-assert renderer have no model for.

class StaticCheckedAnswer {
  answer string
  confidence int @check(c, {{ this < 100 }}) @assert(a, {{ this > 0 }})
}

// RESIDUAL: driven for characterization only.
function PW_Checked(topic: string) -> StaticCheckedAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
