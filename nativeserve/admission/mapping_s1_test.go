package admission

import (
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func s1sp(s string) *string { return &s }

// s1Registry is a minimal effective one-client registry carrying the fence trio,
// used by the socket-free S1 pure-mapping tests (no FFI, no engine construction).
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
// newMappedClient's sole job).
func TestMapClientConfig_OpenAIStrict(t *testing.T) {
	m, dec, err := mapClientConfig(mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: "openai"})
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

// TestMapClientConfig_NonOpenAIMappingUnavailable proves the S1 boundary: EVERY
// non-openai provider mapping-declines (mapping_unavailable) BEFORE any engine is
// constructed — the returned mappedClient is nil (no nanollm.New / Prepare could
// run), and the decline detail never leaks the api key. aws-bedrock and bedrock
// both fold to the bedrock label but still decline in S1.
func TestMapClientConfig_NonOpenAIMappingUnavailable(t *testing.T) {
	for _, p := range []string{"anthropic", "cerebras", "cohere", "bedrock", "aws-bedrock", "some-future-provider"} {
		t.Run(p, func(t *testing.T) {
			m, dec, err := mapClientConfig(mappingInput{registry: s1Registry(p), alias: "__alias__", resolvedProvider: p})
			if err != nil {
				t.Fatalf("planner error: %v", err)
			}
			if m != nil {
				t.Fatal("mappedClient must be nil — no engine may be constructed on a mapping decline")
			}
			if dec == nil || dec.Stage != StageMapping || dec.Reason != ReasonMappingUnavailable {
				t.Fatalf("decline = %v, want mapping/mapping_unavailable", dec)
			}
			if strings.Contains(dec.Detail, "SECRET_KEY") {
				t.Fatalf("decline detail leaked a secret: %q", dec.Detail)
			}
		})
	}
}

// TestMapClientConfig_ProviderMismatch proves §4.2 provenance: a selected client
// whose explicit provider disagrees with the resolved leaf provider declines
// provider_mismatch (never a guess) — checked before the S1 mapping boundary.
// The equality is CANONICAL, so distinct canonical providers (openai vs cerebras)
// disagree.
func TestMapClientConfig_ProviderMismatch(t *testing.T) {
	m, dec, err := mapClientConfig(mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: "cerebras"})
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
	// The client carries provider "openai", but the resolved leaf is absent.
	m, dec, err := mapClientConfig(mappingInput{registry: s1Registry("openai"), alias: "__alias__", resolvedProvider: ""})
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

// TestMapClientConfig_BedrockSpellingEquivalence proves the provenance equality
// is CANONICAL: BAML's aws-bedrock and nanollm's bedrock compare EQUAL (no
// provider_mismatch), so an agreeing bedrock leaf reaches the S1 mapping boundary
// and declines mapping_unavailable (bedrock is not the strict OpenAI mapping in
// S1) — not a spurious provider_mismatch. Both spelling orders are covered.
func TestMapClientConfig_BedrockSpellingEquivalence(t *testing.T) {
	for _, tc := range []struct{ resolved, client string }{
		{"aws-bedrock", "bedrock"},
		{"bedrock", "aws-bedrock"},
		{"aws-bedrock", "aws-bedrock"},
	} {
		t.Run(tc.resolved+"_vs_"+tc.client, func(t *testing.T) {
			m, dec, err := mapClientConfig(mappingInput{registry: s1Registry(tc.client), alias: "__alias__", resolvedProvider: tc.resolved})
			if err != nil {
				t.Fatalf("planner error: %v", err)
			}
			if m != nil {
				t.Fatal("mappedClient must be nil (bedrock has no complete mapping in S1)")
			}
			// Canonical agreement -> NOT provider_mismatch; the S1 boundary declines.
			if dec == nil || dec.Stage != StageMapping || dec.Reason != ReasonMappingUnavailable {
				t.Fatalf("decline = %v, want mapping/mapping_unavailable (canonical bedrock agreement)", dec)
			}
		})
	}
}
