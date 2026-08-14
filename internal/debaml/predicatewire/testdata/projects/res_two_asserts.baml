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
// TWO @assert attributes, both FAILING, on the ASSERT family. checkedwire measured
// stock's MAX_CAUSES=5 and cause ORDER on a bare target; this row asks the same of the
// name-pinned family, whose renderer models exactly ONE failing assert on one required
// field. The cause order recorded here is what a multi-assert admission would have to
// reproduce.

class StaticAssertAnswer {
  answer string
  confidence int @assert(first, {{ this > 100 }}) @assert(second, {{ this > 200 }})
}

// RESIDUAL: driven for characterization only.
function PW_Assert(topic: string) -> StaticAssertAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
