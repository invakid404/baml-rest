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

// ============================================================
// Round-robin test clients
//
// Reuse the Fallback* child clients above (each points at a
// distinct mock scenario ID via its model field) so tests can
// count per-child hits to verify the rotation works across
// requests. The strategy clients below are the round-robin
// wrappers tests invoke.
// ============================================================

client<llm> TestRoundRobinPair {
    provider round-robin
    options {
        strategy [FallbackPrimary, FallbackSecondary]
    }
}

client<llm> TestRoundRobinChain {
    provider round-robin
    options {
        strategy [FallbackPrimary, FallbackSecondary, FallbackTertiary]
    }
}

// ============================================================
// Round-robin wrapper with its own retry_policy
//
// Pins the BAML semantic that LLMStrategyProvider::WithRetryPolicy
// honors a retry_policy declared on the strategy wrapper itself
// (applied AROUND the strategy iteration). The leaf clients here
// have NO retry_policy of their own, so any retry behavior observed
// when the wrapper is unwrapped to a leaf must be inherited from
// the wrapper. Used by integration tests to verify codegen routes
// wrapper retry through the post-RR-unwrap path on v0.219+.
// ============================================================

retry_policy RoundRobinWrapperRetry {
    max_retries 2
    strategy {
        type constant_delay
        delay_ms 50
    }
}

client<llm> TestRoundRobinWithRetry {
    provider round-robin
    retry_policy RoundRobinWrapperRetry
    options {
        strategy [FallbackPrimary, FallbackSecondary]
    }
}

// ============================================================
// Fallback containing an immediate round-robin child
//
// Static composition that exercises centralised RR-child
// unwrapping: the fallback chain's first entry is an RR wrapper
// whose leaves are ordinary openai leaves. Centralised dispatch
// rotates across [FallbackPrimary, FallbackSecondary] via the
// same SharedState advancer top-level RR uses, while the sibling
// fallback child FallbackTertiary remains a static leaf for
// failure-handoff coverage.
// ============================================================

client<llm> TestFallbackRoundRobinPair {
    provider baml-fallback
    options {
        strategy [TestRoundRobinPair, FallbackTertiary]
    }
}
