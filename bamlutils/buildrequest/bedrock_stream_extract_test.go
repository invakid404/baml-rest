package buildrequest

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
)

// encodeBedrockEvent produces wire bytes for a single eventstream frame
// using the SDK encoder. The PR 2 decoder is the production consumer
// so round-tripping through it is the most reliable form of fixture.
func encodeBedrockEvent(t *testing.T, headers map[string]string, payload []byte) []byte {
	t.Helper()
	var hs eventstream.Headers
	for name, value := range headers {
		hs.Set(name, eventstream.StringValue(value))
	}
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{
		Headers: hs,
		Payload: payload,
	}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func decodeBedrockEvent(t *testing.T, frame []byte) *awsstream.Event {
	t.Helper()
	dec := awsstream.NewDecoder(bytes.NewReader(frame))
	evt, err := dec.Next()
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return evt
}

func bedrockEventHeaders(eventType string) map[string]string {
	return map[string]string{
		":message-type": "event",
		":event-type":   eventType,
		":content-type": "application/json",
	}
}

// TestExtractBedrockStreamDelta_TextDelta pins the parseable+raw
// routing for a contentBlockDelta carrying `delta.text`. Reasoning
// stays empty regardless of the IncludeReasoning flag.
func TestExtractBedrockStreamDelta_TextDelta(t *testing.T) {
	frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"),
		[]byte(`{"delta":{"text":"hello"},"contentBlockIndex":0}`))
	evt := decodeBedrockEvent(t, frame)

	for _, includeReasoning := range []bool{false, true} {
		parts, err := extractBedrockStreamDelta(evt, includeReasoning)
		if err != nil {
			t.Fatalf("includeReasoning=%v: unexpected error: %v", includeReasoning, err)
		}
		if parts.Parseable != "hello" {
			t.Errorf("includeReasoning=%v: Parseable = %q, want %q", includeReasoning, parts.Parseable, "hello")
		}
		if parts.Raw != "hello" {
			t.Errorf("includeReasoning=%v: Raw = %q, want %q", includeReasoning, parts.Raw, "hello")
		}
		if parts.Reasoning != "" {
			t.Errorf("includeReasoning=%v: Reasoning = %q, want empty (text delta must never enter reasoning)", includeReasoning, parts.Reasoning)
		}
	}
}

// TestExtractBedrockStreamDelta_ReasoningDelta_OptIn pins reasoning
// gating: parseable and raw stay empty regardless of the flag, and
// reasoning is populated only when IncludeReasoning is true.
func TestExtractBedrockStreamDelta_ReasoningDelta_OptIn(t *testing.T) {
	frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"),
		[]byte(`{"delta":{"reasoningContent":{"text":"thinking..."}},"contentBlockIndex":0}`))
	evt := decodeBedrockEvent(t, frame)

	// IncludeReasoning=false: reasoning empty.
	parts, err := extractBedrockStreamDelta(evt, false)
	if err != nil {
		t.Fatalf("IncludeReasoning=false: %v", err)
	}
	if parts.Reasoning != "" {
		t.Errorf("IncludeReasoning=false: Reasoning = %q, want empty (opt-in gate must fire)", parts.Reasoning)
	}
	if parts.Parseable != "" || parts.Raw != "" {
		t.Errorf("IncludeReasoning=false: Parseable=%q Raw=%q, both want empty (reasoning never enters either)", parts.Parseable, parts.Raw)
	}

	// IncludeReasoning=true: reasoning surfaces, parseable+raw stay empty.
	parts, err = extractBedrockStreamDelta(evt, true)
	if err != nil {
		t.Fatalf("IncludeReasoning=true: %v", err)
	}
	if parts.Reasoning != "thinking..." {
		t.Errorf("IncludeReasoning=true: Reasoning = %q, want %q", parts.Reasoning, "thinking...")
	}
	if parts.Parseable != "" {
		t.Errorf("IncludeReasoning=true: Parseable = %q, want empty (reasoning must not influence parser)", parts.Parseable)
	}
	if parts.Raw != "" {
		t.Errorf("IncludeReasoning=true: Raw = %q, want empty (raw is text-only by construction)", parts.Raw)
	}
}

// TestExtractBedrockStreamDelta_NoOpEvents pins that the housekeeping
// event types (messageStart/Stop, contentBlockStart/Stop, metadata)
// produce no incremental content and no error in PR 3. PR 4 may
// surface stopReason / usage on some of these — the test pins the
// current PR 3 contract.
func TestExtractBedrockStreamDelta_NoOpEvents(t *testing.T) {
	cases := []struct {
		eventType string
		payload   []byte
	}{
		{"messageStart", []byte(`{"role":"assistant"}`)},
		{"contentBlockStart", []byte(`{"contentBlockIndex":0}`)},
		{"contentBlockStop", []byte(`{"contentBlockIndex":0}`)},
		{"messageStop", []byte(`{"stopReason":"end_turn"}`)},
		{"metadata", []byte(`{"usage":{"inputTokens":10,"outputTokens":5}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			frame := encodeBedrockEvent(t, bedrockEventHeaders(tc.eventType), tc.payload)
			evt := decodeBedrockEvent(t, frame)
			parts, err := extractBedrockStreamDelta(evt, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parts.Parseable != "" || parts.Raw != "" || parts.Reasoning != "" {
				t.Errorf("non-empty delta on no-op event: %+v", parts)
			}
		})
	}
}

// TestExtractBedrockStreamDelta_ToolUseSilentSkip pins that tool-use
// streaming delta variants and citationsContent are silently dropped
// in PR 3 (they land in PR 4). Failing here would mean a real PR-3
// stream errors out on tool-use frames the moment a model emits them
// — surfacing them as deltas would be worse because the orchestrator
// would forward partial JSON the BAML parser can't make sense of.
func TestExtractBedrockStreamDelta_ToolUseSilentSkip(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"delta":{"toolUse":{"input":"{\"x\":1}"}},"contentBlockIndex":0}`),
		[]byte(`{"delta":{"toolResult":{"content":[]}},"contentBlockIndex":0}`),
		[]byte(`{"delta":{"citationsContent":{"text":"src"}},"contentBlockIndex":0}`),
	}
	for _, payload := range payloads {
		t.Run(string(payload), func(t *testing.T) {
			frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"), payload)
			evt := decodeBedrockEvent(t, frame)
			parts, err := extractBedrockStreamDelta(evt, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parts.Parseable != "" || parts.Raw != "" || parts.Reasoning != "" {
				t.Errorf("non-empty delta on silently-skipped variant: %+v", parts)
			}
		})
	}
}

// TestExtractBedrockStreamDelta_ReasoningSignatureSkip pins that
// `reasoningContent.signature` / `redactedContent` (encrypted-summary
// variants) are silently skipped under IncludeReasoning=true. PR 4
// will decide whether to pipe them through; the strict invariant here
// is that parseable/raw stay empty AND no spurious reasoning string is
// produced from these fields.
func TestExtractBedrockStreamDelta_ReasoningSignatureSkip(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"delta":{"reasoningContent":{"signature":"sigbytes"}},"contentBlockIndex":0}`),
		[]byte(`{"delta":{"reasoningContent":{"redactedContent":"abc"}},"contentBlockIndex":0}`),
	}
	for _, payload := range payloads {
		t.Run(string(payload), func(t *testing.T) {
			frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"), payload)
			evt := decodeBedrockEvent(t, frame)
			parts, err := extractBedrockStreamDelta(evt, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parts.Parseable != "" || parts.Raw != "" || parts.Reasoning != "" {
				t.Errorf("non-empty delta on signature/redactedContent variant: %+v", parts)
			}
		})
	}
}

// TestExtractBedrockStreamDelta_UnknownEventTypeSilentSkip pins
// forward-compat: an event type Bedrock adds in the future must not
// fail the stream. Defensive: the extractor returns no delta and no
// error.
func TestExtractBedrockStreamDelta_UnknownEventTypeSilentSkip(t *testing.T) {
	frame := encodeBedrockEvent(t, bedrockEventHeaders("someFutureEvent"),
		[]byte(`{"whatever":"value"}`))
	evt := decodeBedrockEvent(t, frame)
	parts, err := extractBedrockStreamDelta(evt, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parts.Parseable != "" || parts.Raw != "" || parts.Reasoning != "" {
		t.Errorf("non-empty delta on unknown event type: %+v", parts)
	}
}

// TestExtractBedrockStreamDelta_ModeledException pins that an
// exception event surfaces as *BedrockStreamException with the
// :exception-type as the modeled name and the payload bytes preserved
// verbatim for PR 4 to parse.
func TestExtractBedrockStreamDelta_ModeledException(t *testing.T) {
	payload := []byte(`{"message":"the model refused"}`)
	frame := encodeBedrockEvent(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "modelStreamErrorException",
		":content-type":   "application/json",
	}, payload)
	evt := decodeBedrockEvent(t, frame)

	_, err := extractBedrockStreamDelta(evt, true)
	if err == nil {
		t.Fatal("nil error on exception event, want *BedrockStreamException")
	}
	var be *BedrockStreamException
	if !errors.As(err, &be) {
		t.Fatalf("error type = %T, want *BedrockStreamException", err)
	}
	if be.ExceptionType != "modelStreamErrorException" {
		t.Errorf("ExceptionType = %q, want modelStreamErrorException", be.ExceptionType)
	}
	if !bytes.Equal(be.Payload, payload) {
		t.Errorf("Payload = %q, want %q (must preserve wire bytes verbatim for PR 4 to parse)", be.Payload, payload)
	}
	if got := be.Error(); got == "" {
		t.Errorf("Error() returned empty string")
	}
}

// TestExtractBedrockStreamDelta_DefensiveErrorMessageType pins that
// the defensive MessageType==error branch surfaces a typed
// *awsstream.TransportError, not a bare errors.New. The production
// awsstream.Decoder intercepts :message-type=error frames before they
// reach the extractor — it returns *TransportError directly from
// Next() — so this branch is unreachable in practice. The test
// synthesizes an Event with MessageType==MessageTypeError directly to
// guard against a future decoder change that lets such events through:
// without the typed surface, the worker classifier's TransportError
// arm would miss the failure and downgrade it to worker_error.
func TestExtractBedrockStreamDelta_DefensiveErrorMessageType(t *testing.T) {
	// Synthesize the Event directly — the decoder won't produce one
	// of this shape, so we bypass it to exercise the defensive branch.
	evt := &awsstream.Event{
		Type:        "InternalServerError",
		MessageType: awsstream.MessageTypeError,
	}
	parts, err := extractBedrockStreamDelta(evt, false)
	if err == nil {
		t.Fatal("nil error on MessageTypeError event, want *awsstream.TransportError")
	}
	var transportErr *awsstream.TransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("error type = %T, want *awsstream.TransportError (worker classifier arm keys off this type)", err)
	}
	if transportErr.Code == "" {
		t.Errorf("TransportError.Code = empty, want a synthetic non-empty code so classifier details omit-empty doesn't drop it")
	}
	if transportErr.Message == "" {
		t.Errorf("TransportError.Message = empty, want a fallback message describing the defensive branch")
	}
	// Defensive branch must not surface incremental content alongside
	// the error — the failure terminates the stream attempt.
	if parts.Parseable != "" || parts.Raw != "" || parts.Reasoning != "" {
		t.Errorf("non-empty delta on defensive error branch: %+v", parts)
	}

	// Empty-Type variant: defensive branch still produces a typed
	// error, just without the Type tail in the message.
	evtEmpty := &awsstream.Event{MessageType: awsstream.MessageTypeError}
	_, err = extractBedrockStreamDelta(evtEmpty, false)
	var emptyTypedErr *awsstream.TransportError
	if !errors.As(err, &emptyTypedErr) {
		t.Fatalf("empty-Type variant: error type = %T, want *awsstream.TransportError", err)
	}
	if emptyTypedErr.Code == "" || emptyTypedErr.Message == "" {
		t.Errorf("empty-Type variant: typed surface still requires non-empty Code and Message; got %+v", emptyTypedErr)
	}
}

// TestExtractBedrockStreamDelta_MalformedJSONErrors pins strict
// behaviour for genuinely broken payloads. A non-JSON delta is a
// transport-integrity problem (the SDK signed/decoded the frame OK
// but the body inside is corrupt), not a forward-compat case, and
// must surface as an error so the orchestrator's classifier handles
// it rather than silently swallowing the stream.
func TestExtractBedrockStreamDelta_MalformedJSONErrors(t *testing.T) {
	frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"),
		[]byte(`{not valid json`))
	evt := decodeBedrockEvent(t, frame)
	_, err := extractBedrockStreamDelta(evt, false)
	if err == nil {
		t.Fatal("nil error on malformed JSON payload, want non-nil")
	}
}

// TestExtractBedrockStreamDelta_NonStringTextErrors pins that a
// `delta.text` field with the wrong JSON type surfaces an error.
// This is a stricter contract than for reasoning (where non-string
// text is silently skipped) because text is the load-bearing parser
// input: silently dropping a malformed text delta would corrupt the
// final result without anyone noticing.
func TestExtractBedrockStreamDelta_NonStringTextErrors(t *testing.T) {
	frame := encodeBedrockEvent(t, bedrockEventHeaders("contentBlockDelta"),
		[]byte(`{"delta":{"text":42}}`))
	evt := decodeBedrockEvent(t, frame)
	_, err := extractBedrockStreamDelta(evt, false)
	if err == nil {
		t.Fatal("nil error on non-string delta.text, want non-nil")
	}
}

// TestExtractBedrockStreamDelta_MultiEventStream walks a small
// representative Bedrock stream and accumulates deltas exactly the way
// the orchestrator does. Pins that:
//   - housekeeping events contribute no content
//   - text deltas concatenate into Parseable+Raw
//   - reasoning frames feed Reasoning only under the opt-in
//   - the final aggregates match the spec's mental model
func TestExtractBedrockStreamDelta_MultiEventStream(t *testing.T) {
	type frameSpec struct {
		headers map[string]string
		payload []byte
	}
	specs := []frameSpec{
		{bedrockEventHeaders("messageStart"), []byte(`{"role":"assistant"}`)},
		{bedrockEventHeaders("contentBlockDelta"), []byte(`{"delta":{"text":"Hello"},"contentBlockIndex":0}`)},
		{bedrockEventHeaders("contentBlockDelta"), []byte(`{"delta":{"reasoningContent":{"text":"thinking"}},"contentBlockIndex":0}`)},
		{bedrockEventHeaders("contentBlockDelta"), []byte(`{"delta":{"text":" world"},"contentBlockIndex":0}`)},
		{bedrockEventHeaders("contentBlockStop"), []byte(`{"contentBlockIndex":0}`)},
		{bedrockEventHeaders("messageStop"), []byte(`{"stopReason":"end_turn"}`)},
	}

	var buf bytes.Buffer
	for _, spec := range specs {
		buf.Write(encodeBedrockEvent(t, spec.headers, spec.payload))
	}

	for _, includeReasoning := range []bool{false, true} {
		dec := awsstream.NewDecoder(bytes.NewReader(buf.Bytes()))
		var parseable, raw, reasoning string
		for {
			evt, err := dec.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			// Discriminate EOF from CRC / framing / truncation
			// failures: the catch-all break would silently swallow a
			// regression in the multi-event fixture, masking it as
			// an accumulation mismatch (or missing it entirely).
			if err != nil {
				t.Fatalf("includeReasoning=%v: decode event: %v", includeReasoning, err)
			}
			parts, err := extractBedrockStreamDelta(evt, includeReasoning)
			if err != nil {
				t.Fatalf("includeReasoning=%v: %v", includeReasoning, err)
			}
			parseable += parts.Parseable
			raw += parts.Raw
			reasoning += parts.Reasoning
		}
		if parseable != "Hello world" {
			t.Errorf("includeReasoning=%v: parseable = %q, want %q", includeReasoning, parseable, "Hello world")
		}
		if raw != "Hello world" {
			t.Errorf("includeReasoning=%v: raw = %q, want %q", includeReasoning, raw, "Hello world")
		}
		wantReasoning := ""
		if includeReasoning {
			wantReasoning = "thinking"
		}
		if reasoning != wantReasoning {
			t.Errorf("includeReasoning=%v: reasoning = %q, want %q", includeReasoning, reasoning, wantReasoning)
		}
	}
}
