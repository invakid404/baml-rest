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
// DUPLICATE LABELS on the pinned family. The raw CFFI list keeps both; baml_go's map
// fold keeps one. checkedwire pins the same fold on a bare target; this row pins it
// inside the admitted family's own shape.

class StaticCheckedAnswer {
  answer string
  confidence int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }})
}

// RESIDUAL: driven for characterization only.
function PW_Checked(topic: string) -> StaticCheckedAnswer {
  client PredicateWireClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
