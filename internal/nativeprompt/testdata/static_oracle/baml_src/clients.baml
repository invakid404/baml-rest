// One stock OpenAI client with a fake model, fake API key, and a `.invalid`
// base URL. The oracle only calls Request.<Function> (BAML's BuildRequest),
// which renders the prompt and builds the provider HTTP request but NEVER
// sends it, so the unreachable `.invalid` host and fake credentials are never
// contacted. The client name maps to provider "openai" in the native leg's
// fixture client->provider constant (mirrors this file, no shorthand).
client<llm> StaticOracleClient {
  provider openai
  options {
    model "fake-static-oracle-model"
    api_key "fake-static-oracle-key"
    base_url "https://static-oracle.invalid/v1"
  }
}
