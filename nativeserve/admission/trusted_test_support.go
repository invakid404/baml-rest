//go:build nanollm_integration

package admission

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// AdmitTrustedClaimForTest builds a SYNTHETIC trusted-provider Claim over an
// openai-wire target, for gated tests of the serve trusted-verification path. S1
// ships NO production non-openai mapping (every non-openai provider
// mapping-declines before nanollm.New), so the PolicyTrustedProvider branch is
// otherwise unreachable through the serve seam; this helper is the ONLY way to
// exercise it, and it is compiled ONLY under the nanollm_integration tag (never in
// a production/release build).
//
// It maps the effective registry client as strict OpenAI (a real nanollm engine +
// a Prepared plan, NO send/socket), then marks the returned Claim TRUSTED so the
// serve path's policy branch is exercised without a real non-openai mapper or a
// non-openai socket. The caller OWNS the returned Claim and MUST Close it.
func AdmitTrustedClaimForTest(reg *bamlutils.ClientRegistry, alias string) (*Claim, error) {
	client, facts, _, dec, err := mapDynamicClient(reg, alias, nativebody.ProviderOpenAI, nil)
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
			Content: []canonicalTextBlock{{Type: "text", Text: "trusted claim test"}},
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
			// The one synthetic bit: a trusted policy over an openai-wire plan, so
			// the serve path's trusted branch runs without a real non-openai mapping.
			Verification: PolicyTrustedProvider,
		},
		client: client,
	}, nil
}
