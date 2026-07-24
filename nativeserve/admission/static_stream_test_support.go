//go:build nanollm_integration

package admission

import (
	"github.com/invakid404/baml-rest/internal/schema"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// AdmitStaticStreamClaimForTest builds a SYNTHETIC static-STREAM serve StaticStreamClaim
// over an openai-wire target, for gated tests of the static-stream SERVE post-claim
// pipeline (de-BAML Phase 3b): the exact one-send DoStream, no-fallback ownership, and the
// tri-state mapping. It performs the SAME nanollm New/Prepare(Stream:true) the production
// predicate does, but SKIPS the descriptor envelope / RenderStatic / return-shape gate /
// BAML StreamRequest plan compare so a test can drive ServeStaticStream's post-claim
// behaviour without a live BAML plan oracle. It keeps the engine ALIVE and retains the
// streaming nanollm.Request for DoStream; the caller (ServeStaticStream) closes it. It is
// the streaming twin of AdmitStaticClaimForTest and is compiled ONLY under the
// nanollm_integration tag.
func AdmitStaticStreamClaimForTest(baseURL, apiKey, alias, targetModel string, bundle *schema.Bundle, body []byte) (*StaticStreamClaim, error) {
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
	req := nanollm.Request{
		Model:  alias,
		Body:   body,
		Type:   nanollm.ChatCompletion,
		Stream: true,
	}
	prep, perr := client.Prepare(req)
	if perr != nil {
		client.Close()
		return nil, perr
	}
	return &StaticStreamClaim{
		client:       client,
		Prepared:     prep,
		Bundle:       bundle,
		ExactRequest: exactRequestFromPlan(prep),
		Alias:        alias,
		request:      req,
	}, nil
}
