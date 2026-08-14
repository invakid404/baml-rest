// GENERATED at test time from the Slice 7.2a-3 serving-oracle corpus — do not edit.
//
// Every declaration below is rendered from the schema.Bundle of one corpus
// fixture (serving_oracle_corpus_test.go) by soRenderProject, so the .baml the
// stock BAML v0.223.0 CFFI compiles and the schema.Bundle the native collector
// coerces against are the same description of the same shape.
//
// Regenerate with:
//   BAML_SERVING_ORACLE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml -run TestServingOracleProjectDrift

client<llm> ServingOracleClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://serving-oracle.invalid/v1"
  }
}

// ISOLATED PROJECT ge: it declares the two name-pinned classes ONCE,
// with this project's own predicate. See servingOracleFixture.Project.

class StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this >= 0 }})
}

class StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this >= 0 }})
}

function SO_static_answer_confidence_ge_check_pass(topic: string) -> StaticCheckedAnswer {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_static_answer_confidence_ge_check_fail(topic: string) -> StaticCheckedAnswer {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_static_answer_confidence_ge_assert_pass(topic: string) -> StaticAssertAnswer {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}

function SO_static_answer_confidence_ge_assert_fail(topic: string) -> StaticAssertAnswer {
  client ServingOracleClient
  prompt #"{{ topic }} {{ ctx.output_format }}"#
}
