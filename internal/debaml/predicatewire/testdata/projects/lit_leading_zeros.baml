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

// Slice 7.2c-1 CANONICAL-LITERAL DISCRIMINATOR: `this > 007` — leading zeros, which FormatInt never emits.
// Isolated so a compile rejection is attributable to this spelling alone.

class StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this > 007 }})
}

// WIRE: the non-canonical literal, driven once.
function PW_Checked(topic: string) -> StaticCheckedAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
