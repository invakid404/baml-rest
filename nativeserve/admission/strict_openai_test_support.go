//go:build nanollm_integration

package admission

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// AdmitStrictOpenAIClaimForTest builds a STRICT-OpenAI Claim over an openai-wire
// target — a real nanollm engine and a real Prepared plan, with NO send and NO
// socket — for gated same-package proofs of the serve path's strict terminal.
//
// It is the strict sibling of [AdmitTrustedClaimForTest], and it exists for the
// same structural reason that one does: the strict terminal (the S5 same-response
// oracle and its native-winner predicate) lives BEHIND the pre-claim plan compare,
// which needs BAML's own request plan for the very same selected leaf. The
// nativeserve module carries no BAML CFFI, so a same-package test cannot obtain
// one — but it CAN read the returned claim's [Claim.ExactRequest] and hand the
// serve path a plan built from it, which makes the plan oracle agree BY
// CONSTRUCTION and leaves the response oracle as the only thing under test. The
// plan oracle keeps its own biting controls elsewhere (the served-path plan
// mutation matrix); nothing here weakens it.
//
// Compiled ONLY under the nanollm_integration tag, never in a production/release
// build. The caller OWNS the returned Claim and MUST Close it.
func AdmitStrictOpenAIClaimForTest(reg *bamlutils.ClientRegistry, alias string) (*Claim, error) {
	client, facts, _, dec, err := mapDynamicClient(context.Background(), reg, alias, nativebody.ProviderOpenAI, nil)
	if err != nil {
		return nil, err
	}
	if dec != nil {
		return nil, dec
	}

	nreq, berr := (nanollm.ChatRequest{
		Model: facts.target,
		Messages: []nanollm.ChatMessage{{
			Role:    "system",
			Content: []canonicalTextBlock{{Type: "text", Text: "strict openai claim test"}},
		}},
	}).Build(canonicalSonicMarshaler)
	if berr != nil {
		client.Close()
		return nil, berr
	}
	nreq.Model = alias
	prep, perr := client.Prepare(nreq)
	if perr != nil {
		client.Close()
		return nil, perr
	}

	return &Claim{
		Admitted: Admitted{
			Prepared:     prep,
			ExactRequest: exactRequestFromPlan(prep),
			Alias:        alias,
			Target:       facts.target,
			Provider:     facts.provider,
			// The regime under proof: STRICT OpenAI runs BOTH retained BAML
			// oracles, which is exactly the path the fe-v1 enrollment serves on.
			Verification: PolicyStrictOpenAI,
		},
		client: client,
	}, nil
}
