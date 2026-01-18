// Default test client (will be overridden via __baml_options__ in tests)
client<llm> TestClient {
    provider openai
    options {
        model "gpt-4o"
        base_url "http://localhost:11434"  // Placeholder, overridden in tests
        api_key "test-key"
    }
}
