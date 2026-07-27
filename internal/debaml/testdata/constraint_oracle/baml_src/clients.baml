// The oracle never issues an LLM call — it drives Parse.<Method> over a fixed
// assistant text — but every BAML function must name a client, so this is a
// deliberately unroutable placeholder.
client<llm> ConstraintOracleClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://constraint-oracle.invalid/v1"
  }
}
