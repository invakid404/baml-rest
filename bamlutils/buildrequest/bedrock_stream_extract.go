// Package buildrequest — bedrock_stream_extract.go decodes a Bedrock
// ConverseStream response (AWS event-stream wire protocol) into the
// per-event DeltaParts split shared by every streaming provider.
//
// The non-streaming counterpart is extractAWSBedrockContent in
// response_extract.go; both surface text under Parseable+Raw and route
// reasoning to Reasoning only when IncludeReasoning is set. The two
// extractors share strictness invariants (parseable/raw text-only,
// reasoning opt-in, tool-use / signature variants silently skipped —
// see #254).
package buildrequest

import (
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
// upstream. The orchestrator wraps the error and the worker's
// classifier (cmd/worker/error_classify.go's *BedrockStreamException
// arm) routes it to provider_error with details.{exception_type,
// exception_message}. The raw payload bytes are preserved in Payload
// so downstream consumers can parse additional fields without
// revisiting the extractor.
type BedrockStreamException struct {
	// ExceptionType is the value of the :exception-type header (the
	// modeled shape name — e.g. "modelStreamErrorException",
	// "ThrottlingException"). The awsstream decoder normalises this to
	// Event.Type for exception messages, so the extractor copies it
	// here verbatim.
	ExceptionType string
	// Payload is the raw payload bytes from the exception frame —
	// typically a JSON object like `{"message":"..."}`. Preserved
	// verbatim; the Message() method parses the standard `message`
	// field, and downstream consumers can extract additional fields
	// directly from these bytes if needed.
	Payload []byte
}

func (e *BedrockStreamException) Error() string {
	if msg := e.Message(); msg != "" {
		return fmt.Sprintf("bedrock: stream exception %s: %s", e.ExceptionType, msg)
	}
	if e.ExceptionType != "" {
		return fmt.Sprintf("bedrock: stream exception %s", e.ExceptionType)
	}
	return "bedrock: stream exception"
}

// Message returns the human-readable message string parsed from the
// exception payload, or "" when the payload is absent or doesn't carry
// a string `message` field. Bedrock's modeled exceptions encode the
// operator-facing description under that key per the AWS
// runtime_ConverseStream documentation; exposing it as a method lets
// the worker error classifier surface it on the provider_error envelope
// without re-parsing the payload bytes.
func (e *BedrockStreamException) Message() string {
	if e == nil || len(e.Payload) == 0 {
		return ""
	}
	r := gjson.GetBytes(e.Payload, "message")
	if r.Type != gjson.String {
		return ""
	}
	return r.String()
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
// metadata, and tool-use / reasoning signature variants — see #254).
// Returns a *BedrockStreamException for modeled exception events so
// the orchestrator can surface them as provider_error.
//
// The unknown-variant arms below stay defensive — silent skip rather
// than error, so a future Bedrock event shape we haven't modeled yet
// doesn't fail open requests.
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
		// changes by returning the same typed *awsstream.TransportError
		// surface the production path produces — without this, an
		// errors.New here would bypass the worker classifier's
		// TransportError arm and downgrade the failure to
		// worker_error. The Event shape doesn't carry :error-code /
		// :error-message headers, so fall back to a synthetic code +
		// the event Type (if any) for the message; this is purely
		// defensive code and operator diagnostics here lean on the
		// "decoder gave us an error frame we didn't expect" framing.
		fallbackMessage := "bedrock: unexpected error message-type at extractor"
		if evt.Type != "" {
			fallbackMessage = fallbackMessage + ": " + evt.Type
		}
		return sse.DeltaParts{}, &awsstream.TransportError{
			Code:    "UnexpectedErrorMessageType",
			Message: fallbackMessage,
		}
	case awsstream.MessageTypeEvent, "":
		// fall through
	}

	switch evt.Type {
	case "contentBlockDelta":
		return extractBedrockContentBlockDelta(evt.Payload, includeReasoning)
	case "messageStart", "contentBlockStart", "contentBlockStop", "messageStop", "metadata":
		// No-op frames that produce no incremental DeltaParts content.
		// Surfacing stopReason / usage from messageStop and metadata is
		// deferred — see #254.
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
// toolResult, or citationsContent — only the first two are surfaced
// today (see #254 for the deferred tool-use deltas).
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
	// `signature` and `redactedContent` are silently skipped — see
	// #254. Parseable/Raw invariance: reasoning never enters either
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
		// `signature` / `redactedContent` — silently skipped, see #254.
		return sse.DeltaParts{}, nil
	}

	// tool-use streaming deltas (delta.toolUse.input, delta.toolResult),
	// citationsContent, and any other future delta variant: silently
	// skipped (see #254).
	return sse.DeltaParts{}, nil
}
