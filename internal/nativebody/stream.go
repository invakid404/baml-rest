package nativebody

import (
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// De-BAML Phase 7B streaming canonical body. This file adds the STREAMING twin
// of body.go's non-streaming BuildOpenAIChat: the exact canonical OpenAI
// chat-completions STREAM request bytes BAML v0.223 emits for a given rendered
// prompt + client. The streaming body is byte-identical to the non-streaming
// body except for BAML's trailing `,"stream":true,"stream_options":{"include_usage":true}`
// suffix (see streamOptionsSuffix in body.go).
//
// It is deliberately a SEPARATE entry point rather than a mode flag on the
// proven non-streaming path: the non-streaming builder must keep DECLINING every
// streaming attempt (FeatureStreaming) unchanged — the two surfaces share only
// the mode-agnostic client/prompt gates (supportsClientCommon / supportsRendered)
// so neither can silently claim the other's bytes.
//
// Like the rest of nativebody it is pure Go with NO nanollm/BAML dependency; the
// nanollm-linked stream admission (nativeserve/admission) uses these bytes as the
// zero-nanollm parity anchor, exactly as the unary lane does for BuildOpenAIChat.

// SupportsOpenAIChatStream is the fail-closed admission predicate for the
// STREAMING canonical OpenAI chat body. It returns nil when
// [BuildOpenAIChatStream] is proven to reproduce BAML's exact StreamRequest bytes
// for (rendered, client), and a *Decline (unwrapping to [ErrUnsupported])
// otherwise. It shares every gate with [SupportsOpenAIChat] EXCEPT the mode
// check: it REQUIRES a streaming attempt (client.Stream==true), the mirror of the
// non-streaming predicate's requirement that client.Stream is false.
func SupportsOpenAIChatStream(rendered *nativeprompt.RenderedPrompt, client ClientIntent) error {
	if err := supportsClientStream(client); err != nil {
		return err
	}
	return supportsRendered(rendered)
}

// BuildOpenAIChatStream serializes rendered + client into the exact canonical
// OpenAI chat-completions STREAM body BAML v0.223 emits, or returns a
// fail-closed *Decline (unwrapping to [ErrUnsupported]). It calls
// [SupportsOpenAIChatStream] first, so it never serializes an unproven shape.
//
// The body is the same top-level "model" + "messages" array the non-streaming
// builder produces, with BAML's trailing streaming suffix appended after the
// messages array and before the closing brace. The returned CanonicalBody's
// bytes are the runtime parity anchor the stream admission compares nanollm's
// prepared streaming plan against (validatePreparedStreamBody), so a divergent
// engine body fails closed to BAML rather than being sent.
func BuildOpenAIChatStream(rendered *nativeprompt.RenderedPrompt, client ClientIntent) (*CanonicalBody, error) {
	if err := SupportsOpenAIChatStream(rendered, client); err != nil {
		return nil, err
	}
	return buildCanonicalBody(rendered, client, true), nil
}
