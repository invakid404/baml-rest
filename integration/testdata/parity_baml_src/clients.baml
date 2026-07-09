// Dedicated baml_src for the de-BAML P3 slice-3 byte-parity oracle harness
// (integration/static_output_format_renderer_test.go). Kept SEPARATE from the
// shared testdata/baml_src so adding output_format-only parity functions does
// not perturb the shared corpus's introspect/openapi/golden expectations.
//
// TestClient is a placeholder; the parity test overrides it via a per-call
// ClientRegistry (testutil.CreateTestClient) that redirects it at the mock LLM.
client<llm> TestClient {
    provider openai
    options {
        model "gpt-4o"
        base_url "http://localhost:11434"  // Placeholder, overridden in tests
        api_key "test-key"
    }
}
