// The ISOLATED < operator project of de-BAML Slice 7.2c-3.
//
// It declares the two PRODUCTION-PINNED class names exactly ONCE each, with this
// project's own predicate. Nothing else: no siblings, no extra classes, no second
// definition of either name — TestOperatorFixtureProjectsAreIsolatedAndPinned parses
// this file and fails if any of that changes.

client<llm> StaticOracleClient {
  provider openai
  options {
    model "fake-static-oracle-model"
    api_key "fake-static-oracle-key"
    base_url "http://127.0.0.1:17656/v1"
  }
}

class StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this < 0 }})
}

class StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this < 0 }})
}

function StaticCheckedConfidence(topic: string) -> StaticCheckedAnswer {
  client StaticOracleClient
  prompt #"
    Topic: {{ topic }}
    {{ ctx.output_format }}
  "#
}

function StaticAssertConfidence(topic: string) -> StaticAssertAnswer {
  client StaticOracleClient
  prompt #"
    Topic: {{ topic }}
    {{ ctx.output_format }}
  "#
}
