// Default test client (will be overridden via __baml_options__ in tests)
client<llm> TestClient {
    provider openai
    options {
        model "gpt-4o"
        base_url "http://localhost:11434"  // Placeholder, overridden in tests
        api_key "test-key"
    }
}

// ============================================================
// Fallback chain test clients
//
// These use the Docker-internal mockllm hostname. The scenario ID
// is set via the "model" field, which the mock server matches on.
// ============================================================

retry_policy FallbackRetry {
    max_retries 3
    strategy {
        type constant_delay
        delay_ms 100
    }
}

client<llm> FallbackPrimary {
    provider openai
    options {
        model "fallback-primary"
        base_url "http://mockllm:8080/v1"
        api_key "test-key"
    }
}

client<llm> FallbackSecondary {
    provider openai
    options {
        model "fallback-secondary"
        base_url "http://mockllm:8080/v1"
        api_key "test-key"
    }
}

client<llm> FallbackTertiary {
    provider openai
    options {
        model "fallback-tertiary"
        base_url "http://mockllm:8080/v1"
        api_key "test-key"
    }
}

client<llm> TestFallbackChain {
    provider baml-fallback
    retry_policy FallbackRetry
    options {
        strategy [FallbackPrimary, FallbackSecondary, FallbackTertiary]
    }
}

// Two-client fallback for simpler tests
client<llm> TestFallbackPair {
    provider baml-fallback
    retry_policy FallbackRetry
    options {
        strategy [FallbackPrimary, FallbackSecondary]
    }
}
