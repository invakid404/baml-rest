// The harness never issues an LLM call — it drives Parse.<Method> over fixed
// assistant text — but every BAML function must name a client, so this is a
// deliberately unroutable placeholder.
client<llm> GuardLedgerClient {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://guard-ledger.invalid/v1"
  }
}
