// Package buildrequest — bedrock_stream_extract.go decodes a Bedrock
// ConverseStream response (AWS event-stream wire protocol) into the
// per-event DeltaParts split shared by every streaming provider.
//
// The non-streaming counterpart is extractAWSBedrockContent in
// response_extract.go; both surface text under Parseable+Raw and route
// reasoning to Reasoning only when IncludeReasoning is set. The two
// extractors share strictness invariants (parseable/raw text-only,
// reasoning opt-in, tool-use / signature variants silently skipped per
// the PR 3 scope cut).
package buildrequest

import (
	"errors"
	"fmt"

	"github.com/tidwall/gjson"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
	"github.com/invakid404/baml-rest/bamlutils/sse"
)

// BedrockStreamException is the typed error returned when a Bedrock
// ConverseStream emits an event whose :message-type is "exception".
// Modeled exceptions are protocol data, not transport failures (those
// surface as *awsstream.TransportError), so the streaming extractor
// returns this type to keep the two failure classes distinguishable
// upstream. The orchestrator wraps the error so the existing typed
// classifier (cmd/worker/error_classify.go's *llmhttp.HTTPError /
// transport-flake arms) can carry provider_error context.
//
// PR3-bedrock-stream breadcrumb (issue #243): PR 4 may attach a richer
// payload (parsed AWS exception JSON, retry hints) — the wire bytes
// are preserved verbatim in Payload to keep that option open without
// breaking the current Error() surface.
type BedrockStreamException struct {
	// ExceptionType is the value of the :exception-type header (the
	// modeled shape name — e.g. "modelStreamErrorException",
	// "ThrottlingException"). The awsstream decoder normalises this to
	// Event.Type for exception messages, so the extractor copies it
	// here verbatim.
	ExceptionType string
	// Payload is the raw payload bytes from the exception frame —
	// typically a JSON object like `{"message":"..."}`. Preserved
	// verbatim so PR 4 can parse fields out without revisiting the
	// extractor.
	Payload []byte
}

func (e *BedrockStreamException) Error() string {
	// gjson.GetBytes is safe on a nil/empty payload — returns an empty
	// Result and triggers the "no message" branch below.
	if msg := gjson.GetBytes(e.Payload, "message"); msg.Type == gjson.String && msg.String() != "" {
		return fmt.Sprintf("bedrock: stream exception %s: %s", e.ExceptionType, msg.String())
	}
	if e.ExceptionType != "" {
		return fmt.Sprintf("bedrock: stream exception %s", e.ExceptionType)
	}
	return "bedrock: stream exception"
}

// extractBedrockStreamDelta routes a single decoded awsstream event into
// the per-event DeltaParts split. Mirrors the per-provider extractor
// contract used by sse.ExtractDeltaPartsFromText: text content fills
// Parseable+Raw, reasoning content fills Reasoning when
// includeReasoning is set. The caller (RunStreamOrchestration's bedrock
// branch) accumulates the returned parts identically to the SSE path.
//
// Returns a zero DeltaParts for events that produce no incremental
// content (messageStart, contentBlockStart/Stop, messageStop,
// metadata, and tool-use / reasoning signature variants reserved for
// PR 4). Returns a *BedrockStreamException for modeled exception
// events so the orchestrator can surface them as provider_error.
//
// PR3-bedrock-stream breadcrumb (issue #243): PR 4 will widen the
// reasoning union (signature / redactedContent), surface tool-use
// deltas, and pipe metadata.usage stats through the metadata channel.
// Until then, the unknown-variant arms below stay defensive — silent
// skip rather than error, so a future Bedrock event shape we haven't
// modeled yet doesn't fail open requests.
func extractBedrockStreamDelta(evt *awsstream.Event, includeReasoning bool) (sse.DeltaParts, error) {
	if evt == nil {
		return sse.DeltaParts{}, nil
	}

	switch evt.MessageType {
	case awsstream.MessageTypeException:
		return sse.DeltaParts{}, &BedrockStreamException{
			ExceptionType: evt.Type,
			Payload:       append([]byte(nil), evt.Payload...),
		}
	case awsstream.MessageTypeError:
		// awsstream.Decoder returns *TransportError directly from
		// Next() for :message-type=error frames, so the extractor
		// should never see one here. Defend against future decoder
		// changes by treating it as a transport error verbatim — the
		// orchestrator already classifies these via the worker arm.
		return sse.DeltaParts{}, errors.New("bedrock: unexpected error message-type at extractor")
	case awsstream.MessageTypeEvent, "":
		// fall through
	}

	switch evt.Type {
	case "contentBlockDelta":
		return extractBedrockContentBlockDelta(evt.Payload, includeReasoning)
	case "messageStart", "contentBlockStart", "contentBlockStop", "messageStop", "metadata":
		// PR3-bedrock-stream breadcrumb: PR 4 surfaces stopReason on
		// messageStop and usage metadata. For PR 3 these are no-op
		// frames that produce no incremental DeltaParts content.
		return sse.DeltaParts{}, nil
	default:
		// Forward-compat: an unknown event type (a future Bedrock
		// addition or a service variation) silently produces no
		// delta. Erring here would fail entire streams the moment AWS
		// shipped a new event we don't model.
		return sse.DeltaParts{}, nil
	}
}

// extractBedrockContentBlockDelta routes the contentBlockDelta payload
// to the right output channel. The delta union (per the AWS
// ContentBlockDelta spec) carries text, reasoningContent, toolUse,
// toolResult, or citationsContent — only the first two are surfaced in
// PR 3.
func extractBedrockContentBlockDelta(payload []byte, includeReasoning bool) (sse.DeltaParts, error) {
	if len(payload) == 0 {
		return sse.DeltaParts{}, nil
	}
	if !gjson.ValidBytes(payload) {
		return sse.DeltaParts{}, fmt.Errorf("bedrock: contentBlockDelta payload is not valid JSON")
	}
	delta := gjson.GetBytes(payload, "delta")
	if !delta.IsObject() {
		// No delta object — surface no incremental content. Bedrock
		// occasionally emits frames with only contentBlockIndex; that
		// is not an error.
		return sse.DeltaParts{}, nil
	}

	// `delta.text` (string) → Parseable + Raw. Mirrors the
	// non-streaming extractor's `block.text` arm in
	// extractAWSBedrockContent.
	if text := delta.Get("text"); text.Exists() {
		if text.Type != gjson.String {
			return sse.DeltaParts{}, fmt.Errorf("bedrock: unexpected type for delta.text (got %s)", text.Type)
		}
		s := text.String()
		return sse.DeltaParts{Parseable: s, Raw: s}, nil
	}

	// `delta.reasoningContent.text` → Reasoning when opted in.
	// `signature` and `redactedContent` are PR 4 territory: silently
	// skip so the existing payload shapes don't accidentally fail
	// open. Parseable/Raw invariance: reasoning never enters either
	// regardless of the flag.
	if rc := delta.Get("reasoningContent"); rc.IsObject() {
		if !includeReasoning {
			return sse.DeltaParts{}, nil
		}
		if t := rc.Get("text"); t.Exists() {
			if t.Type != gjson.String {
				// Non-string text on a reasoning delta — defensive
				// skip rather than fail. The reasoning channel is
				// opt-in telemetry; a malformed reasoning frame must
				// never poison the text stream.
				return sse.DeltaParts{}, nil
			}
			return sse.DeltaParts{Reasoning: t.String()}, nil
		}
		// `signature` / `redactedContent` — PR 4 territory.
		return sse.DeltaParts{}, nil
	}

	// tool-use streaming deltas (delta.toolUse.input, delta.toolResult),
	// citationsContent, and any other future delta variant: silent
	// skip per PR 3 scope cuts.
	return sse.DeltaParts{}, nil
}
