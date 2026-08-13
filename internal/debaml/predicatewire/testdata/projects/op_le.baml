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

// Slice 7.2c-1 OPERATOR CAPTURE: the two name-pinned static families carrying
// the direct comparison `this <= 0`. Isolated in its own project because the
// class names are PINNED — six predicate variants of one name cannot coexist.

class StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this <= 0 }})
}

class StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this <= 0 }})
}

// WIRE: the @check family, driven true and false.
function PW_Checked(topic: string) -> StaticCheckedAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

// WIRE + ERROR BYTES: the @assert family, driven true and false.
function PW_Assert(topic: string) -> StaticAssertAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
