//go:build nanollm_integration

package execute

import (
	"testing"

	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// TestOpenAITranslateResponseIsByteVerbatim pins the fact the serving cutover's
// same-response oracle coverage argument rests on.
//
// On the enrolled fe-v1 surface the oracle compares two readings of one response:
//
//   - NATIVE reads translated.Body — what nanollm's TranslateResponse returned;
//   - BAML reads res.ProviderBody — the raw upstream bytes.
//
// Both readings run buildrequest.ExtractResponseContentBytes with provider
// "openai". So as long as TranslateResponse hands an openai response BACK
// VERBATIM, no upstream response can make the assistant, raw or reasoning facet
// disagree on its own — those three facets of the native-winner predicate are
// unreachable through a real socket, and their biting controls must therefore
// drive the BAML leg directly (see nativeserve/canary's same-response facet
// proof, which says so and points here).
//
// That is a property of a PINNED FFI, not a law, which is exactly why it is
// asserted rather than written down. The day a nanollm bump starts normalizing
// openai responses — dropping an unknown field, reordering keys, rewriting
// reasoning_content — this test fails, and the failure is the notice that those
// facets have become live drift channels on real traffic and that the coverage
// note above is stale.
func TestOpenAITranslateResponseIsByteVerbatim(t *testing.T) {
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name: "verbatim-probe",
			// A base URL that can never be dialled: this test TRANSLATES an
			// already-fetched body and must open no socket at all.
			Model:      "openai/gpt-4o-mini",
			APIKey:     "sk-verbatim-probe",
			BaseURL:    "http://127.0.0.1:1/v1",
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	defer c.Close()

	// Deliberately awkward: an unknown top-level field, a reasoning channel, and
	// a content string that is itself JSON — every shape a normalizing translator
	// would be tempted to rewrite.
	raw := []byte(`{"id":"verbatim","object":"chat.completion","zzz_unknown":"keep-me",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer\":\"ok\"}",` +
		`"reasoning_content":"because"},"finish_reason":"stop"}]}`)

	res, err := c.TranslateResponse("verbatim-probe", 200, raw)
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if !res.BodyIsJSON {
		t.Fatalf("translated body is not JSON; the 2xx native pipeline requires it")
	}
	if string(res.Body) != string(raw) {
		t.Fatalf("nanollm no longer returns openai 2xx responses verbatim.\n"+
			"The same-response oracle's assistant/raw/reasoning facets are now REAL drift channels on\n"+
			"served traffic, and the coverage note in nativeserve/canary's same-response facet proof is stale:\n"+
			"add an end-to-end drift arm there instead of driving the BAML leg directly.\n"+
			"  raw:        %s\n  translated: %s", raw, res.Body)
	}
}
