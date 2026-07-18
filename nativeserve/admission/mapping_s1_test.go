package admission

import (
	"context"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func s1sp(s string) *string { return &s }

// s1Registry is a minimal effective one-client registry carrying the fence trio,
// used by the socket-free pure-mapping tests (no FFI, no engine construction).
func s1Registry(provider string) *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: s1sp("C"),
		Clients: []*bamlutils.ClientProperty{{
			Name:     "C",
			Provider: provider,
			Options: map[string]any{
				"model":    "some-model",
				"base_url": "https://example.invalid",
				"api_key":  "SECRET_KEY_do_not_leak",
			},
		}},
	}
}

// TestMapClientConfig_OpenAIStrict proves the strict OpenAI mapping still resolves
// to a mappedClient carrying the fence trio and PolicyStrictOpenAI — the pure
// mapper returns the intent WITHOUT constructing an engine (that is
// newMappedClient's sole job). UNCHANGED strict anchor.
func TestMapClientConfig_OpenAIStrict(t *testing.T) {
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: "openai"})
	if err != nil {
		t.Fatalf("openai mapping planner error: %v", err)
	}
	if dec != nil {
		t.Fatalf("openai declined: %v", dec)
	}
	if m == nil {
		t.Fatal("openai mappedClient is nil")
	}
	if m.verification != PolicyStrictOpenAI {
		t.Errorf("policy = %v, want strict_openai", m.verification)
	}
	if m.nanollmProvider != "openai" || m.target != "some-model" {
		t.Errorf("mapping = (%q,%q), want (openai, some-model)", m.nanollmProvider, m.target)
	}
}

// TestMapClientConfig_S2TrustedBearerMaps proves the S2 boundary shift: every
// common-bearer provider (anthropic/cerebras/cohere/any-other) now MAPS (the S1
// mapping_unavailable decline is gone) to a PolicyTrustedProvider mappedClient
// carrying the fence trio — WITHOUT constructing an engine. Cohere maps at the
// pure level too; it auto-declines later PRE-socket at nanollm.New/Prepare
// (embeddings-only in v0.4.3), not here. No decline detail leaks the api key.
func TestMapClientConfig_S2TrustedBearerMaps(t *testing.T) {
	for _, p := range []string{"anthropic", "cerebras", "cohere", "some-future-provider"} {
		t.Run(p, func(t *testing.T) {
			m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: s1Registry(p), alias: "__alias__", resolvedProvider: p})
			if err != nil {
				t.Fatalf("planner error: %v", err)
			}
			if dec != nil {
				t.Fatalf("declined: %v (want a trusted mapping)", dec)
			}
			if m == nil {
				t.Fatal("mappedClient is nil (want a trusted mapping)")
			}
			if m.verification != PolicyTrustedProvider {
				t.Errorf("policy = %v, want trusted_provider", m.verification)
			}
			if m.nanollmProvider != p || m.target != "some-model" {
				t.Errorf("mapping = (%q,%q), want (%s, some-model)", m.nanollmProvider, m.target, p)
			}
			if m.apiKey != "SECRET_KEY_do_not_leak" {
				t.Errorf("api key not carried onto the mappedClient")
			}
		})
	}
}

// TestMapClientConfig_ProviderMismatch proves §4.2 provenance: a selected client
// whose explicit provider disagrees with the resolved leaf provider declines
// provider_mismatch (never a guess). The equality is CANONICAL, so distinct
// canonical providers (openai vs cerebras) disagree.
func TestMapClientConfig_ProviderMismatch(t *testing.T) {
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: "cerebras"})
	if err != nil {
		t.Fatalf("planner error: %v", err)
	}
	if m != nil {
		t.Fatal("mappedClient must be nil on a provider mismatch")
	}
	if dec == nil || dec.Stage != StageClientSelection || dec.Reason != ReasonProviderMismatch {
		t.Fatalf("decline = %v, want client_selection/provider_mismatch", dec)
	}
}

// TestMapClientConfig_EmptyResolvedLeafDeclines proves §4.2 authority: an ABSENT
// resolved leaf provider declines client_selection/provider_mismatch BEFORE any
// engine is constructed (m nil) — native never lets the selected client's own
// provider stand in as the authoritative provider.
func TestMapClientConfig_EmptyResolvedLeafDeclines(t *testing.T) {
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: ""})
	if err != nil {
		t.Fatalf("planner error: %v", err)
	}
	if m != nil {
		t.Fatal("mappedClient must be nil — no engine may be constructed without an authoritative resolved leaf")
	}
	if dec == nil || dec.Stage != StageClientSelection || dec.Reason != ReasonProviderMismatch {
		t.Fatalf("decline = %v, want client_selection/provider_mismatch (absent resolved leaf)", dec)
	}
}

// TestMapClientConfig_BedrockSpellingEquivalence proves the provenance equality is
// CANONICAL: BAML's aws-bedrock and nanollm's bedrock compare EQUAL (no
// provider_mismatch), so an agreeing bedrock leaf REACHES the Bedrock special
// mapper. The s1Registry carries the bearer trio (api_key/base_url), which are not
// aws-bedrock options, so the Bedrock mapper declines them unproven_client_option
// — NOT a spurious provider_mismatch, and NOT the retired mapping_unavailable. No
// decline detail leaks the api key. Both spelling orders are covered.
func TestMapClientConfig_BedrockSpellingEquivalence(t *testing.T) {
	for _, tc := range []struct{ resolved, client string }{
		{"aws-bedrock", "bedrock"},
		{"bedrock", "aws-bedrock"},
		{"aws-bedrock", "aws-bedrock"},
	} {
		t.Run(tc.resolved+"_vs_"+tc.client, func(t *testing.T) {
			m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: s1Registry(tc.client), alias: "__alias__", resolvedProvider: tc.resolved})
			if err != nil {
				t.Fatalf("planner error: %v", err)
			}
			if m != nil {
				t.Fatal("mappedClient must be nil (the bearer trio is not a valid Bedrock shape)")
			}
			// Canonical agreement -> reaches the Bedrock mapper (NOT provider_mismatch)
			// -> declines the non-bedrock api_key option as unproven.
			if dec == nil || dec.Stage != StageClientOption || dec.Reason != ReasonUnprovenClientOption {
				t.Fatalf("decline = %v, want client_option/unproven_client_option (canonical bedrock agreement reached the Bedrock mapper)", dec)
			}
			if dec.Reason == ReasonProviderMismatch {
				t.Fatal("canonical bedrock agreement must not decline provider_mismatch")
			}
			if strings.Contains(dec.Detail, "SECRET_KEY") {
				// Never print dec.Detail — it contains the leaked secret; report the fact.
				t.Fatal("decline detail contained the forbidden SECRET_KEY token (detail redacted)")
			}
		})
	}
}
