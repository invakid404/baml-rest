//go:build nanollm_integration

package admission

import (
	"github.com/invakid404/baml-rest/internal/schema"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// AdmitStaticClaimForTest builds a SYNTHETIC static serve StaticClaim over an
// openai-wire target, for gated tests of the static SERVE post-claim pipeline
// (de-BAML Slice 8C): the exact one-send, native static SAP over the Bundle, the S5
// same-response BAML compare, and the tri-state mapping. It performs the SAME
// nanollm New/Prepare the production predicate does, but SKIPS the descriptor
// envelope / RenderStatic / BAML plan compare so a test can drive ServeStatic's
// post-claim behaviour without a live BAML plan oracle. It keeps the engine ALIVE;
// the caller (ServeStatic) closes it. It is compiled ONLY under the
// nanollm_integration tag (never in a production/release build).
func AdmitStaticClaimForTest(baseURL, apiKey, alias, targetModel string, bundle *schema.Bundle, body []byte) (*StaticClaim, error) {
	client, nerr := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       alias,
			Model:      "openai/" + targetModel,
			APIKey:     apiKey,
			BaseURL:    baseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if nerr != nil {
		return nil, nerr
	}
	prep, perr := client.Prepare(nanollm.Request{
		Model:  alias,
		Body:   body,
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if perr != nil {
		client.Close()
		return nil, perr
	}
	return &StaticClaim{
		client:       client,
		Prepared:     prep,
		Bundle:       bundle,
		ExactRequest: exactRequestFromPlan(prep),
		Alias:        alias,
	}, nil
}
