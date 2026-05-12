package awsstream_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
)

// encode produces the wire bytes for a single eventstream message with the
// supplied headers and payload. Tests use this to round-trip fixtures
// through the SDK's encoder — the most reliable form of fixture, since it
// tracks any subtle encoding changes between SDK versions.
func encode(t *testing.T, headers map[string]string, payload []byte) []byte {
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

func eventHeaders(eventType string) map[string]string {
	return map[string]string{
		":message-type": "event",
		":event-type":   eventType,
		":content-type": "application/json",
	}
}

func TestDecoder_TextEvent(t *testing.T) {
	payload := []byte(`{"delta":{"text":"hello"},"contentBlockIndex":0}`)
	frame := encode(t, eventHeaders("contentBlockDelta"), payload)

	dec := awsstream.NewDecoder(bytes.NewReader(frame))

	evt, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.Type != "contentBlockDelta" {
		t.Errorf("Type = %q, want contentBlockDelta", evt.Type)
	}
	if evt.MessageType != "event" {
		t.Errorf("MessageType = %q, want event", evt.MessageType)
	}
	if evt.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", evt.ContentType)
	}
	if !bytes.Equal(evt.Payload, payload) {
		t.Errorf("Payload = %q, want %q", evt.Payload, payload)
	}

	if _, err := dec.Next(); err != io.EOF {
		t.Errorf("second Next err = %v, want io.EOF", err)
	}
}

func TestDecoder_ReasoningEvent(t *testing.T) {
	// Bedrock surfaces reasoning content under
	// contentBlockDelta.delta.reasoningContent.text. The decoder is
	// payload-agnostic — this test just pins that the raw bytes survive
	// intact so PR 3's extractor can parse them.
	payload := []byte(`{"delta":{"reasoningContent":{"text":"thinking..."}},"contentBlockIndex":0}`)
	frame := encode(t, eventHeaders("contentBlockDelta"), payload)

	dec := awsstream.NewDecoder(bytes.NewReader(frame))
	evt, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.Type != "contentBlockDelta" {
		t.Errorf("Type = %q, want contentBlockDelta", evt.Type)
	}
	if !bytes.Equal(evt.Payload, payload) {
		t.Errorf("Payload bytes did not survive intact:\n got: %s\nwant: %s", evt.Payload, payload)
	}
}

func TestDecoder_MultiEventStream(t *testing.T) {
	type frameSpec struct {
		eventType string
		payload   []byte
	}
	specs := []frameSpec{
		{"messageStart", []byte(`{"role":"assistant"}`)},
		{"contentBlockDelta", []byte(`{"delta":{"text":"hello"},"contentBlockIndex":0}`)},
		{"contentBlockDelta", []byte(`{"delta":{"text":" world"},"contentBlockIndex":0}`)},
		{"contentBlockStop", []byte(`{"contentBlockIndex":0}`)},
		{"messageStop", []byte(`{"stopReason":"end_turn"}`)},
		{"metadata", []byte(`{"usage":{"inputTokens":10,"outputTokens":5}}`)},
	}

	var buf bytes.Buffer
	for _, spec := range specs {
		buf.Write(encode(t, eventHeaders(spec.eventType), spec.payload))
	}

	dec := awsstream.NewDecoder(&buf)
	for i, spec := range specs {
		evt, err := dec.Next()
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		if evt.Type != spec.eventType {
			t.Errorf("Next[%d].Type = %q, want %q", i, evt.Type, spec.eventType)
		}
		if evt.MessageType != "event" {
			t.Errorf("Next[%d].MessageType = %q, want event", i, evt.MessageType)
		}
		if !bytes.Equal(evt.Payload, spec.payload) {
			t.Errorf("Next[%d].Payload = %q, want %q", i, evt.Payload, spec.payload)
		}
	}

	if _, err := dec.Next(); err != io.EOF {
		t.Errorf("trailing Next err = %v, want io.EOF", err)
	}
}

func TestDecoder_ModeledException(t *testing.T) {
	// Modeled exceptions are protocol data, not transport failures — they
	// must surface as ordinary Events so PR 3's extractor can route them
	// through the same provider-error pipeline as JSON-body errors.
	payload := []byte(`{"message":"the model refused"}`)
	frame := encode(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "modelStreamErrorException",
		":content-type":   "application/json",
	}, payload)

	dec := awsstream.NewDecoder(bytes.NewReader(frame))
	evt, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.MessageType != "exception" {
		t.Errorf("MessageType = %q, want exception", evt.MessageType)
	}
	if evt.Type != "modelStreamErrorException" {
		t.Errorf("Type = %q, want modelStreamErrorException", evt.Type)
	}
	if !bytes.Equal(evt.Payload, payload) {
		t.Errorf("Payload = %q, want %q", evt.Payload, payload)
	}
}

func TestDecoder_ExceptionPrefersExceptionType(t *testing.T) {
	// AWS event-stream modeled exceptions use :exception-type as the
	// authoritative shape discriminator. Some services also set
	// :event-type to a generic fallback value. When both are present on
	// an exception frame, Event.Type must equal :exception-type — the
	// modeled shape — not :event-type.
	payload := []byte(`{"message":"rate exceeded"}`)
	frame := encode(t, map[string]string{
		":message-type":   "exception",
		":event-type":     "genericFallback",
		":exception-type": "ThrottlingException",
		":content-type":   "application/json",
	}, payload)

	dec := awsstream.NewDecoder(bytes.NewReader(frame))
	evt, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.MessageType != "exception" {
		t.Errorf("MessageType = %q, want exception", evt.MessageType)
	}
	if evt.Type != "ThrottlingException" {
		t.Errorf("Type = %q, want ThrottlingException (must prefer :exception-type over :event-type)", evt.Type)
	}
}

func TestDecoder_TransportError(t *testing.T) {
	// :message-type=error frames carry no payload meaningful to the
	// caller; they have :error-code and :error-message headers and signal
	// a transport-level failure from the AWS stack. Decoder surfaces them
	// as a *TransportError from Next.
	frame := encode(t, map[string]string{
		":message-type":  "error",
		":error-code":    "InternalServerError",
		":error-message": "service unavailable",
	}, nil)

	dec := awsstream.NewDecoder(bytes.NewReader(frame))
	_, err := dec.Next()
	if err == nil {
		t.Fatal("Next: nil error, want *TransportError")
	}
	var tErr *awsstream.TransportError
	if !errors.As(err, &tErr) {
		t.Fatalf("Next err type = %T, want *TransportError", err)
	}
	if tErr.Code != "InternalServerError" {
		t.Errorf("TransportError.Code = %q, want InternalServerError", tErr.Code)
	}
	if tErr.Message != "service unavailable" {
		t.Errorf("TransportError.Message = %q, want service unavailable", tErr.Message)
	}
	if !strings.Contains(tErr.Error(), "InternalServerError") {
		t.Errorf("Error() = %q, expected to contain code", tErr.Error())
	}
	if !strings.Contains(tErr.Error(), "service unavailable") {
		t.Errorf("Error() = %q, expected to contain message", tErr.Error())
	}
}

func TestDecoder_EmptyStream(t *testing.T) {
	dec := awsstream.NewDecoder(bytes.NewReader(nil))
	evt, err := dec.Next()
	if err != io.EOF {
		t.Fatalf("Next err = %v, want io.EOF", err)
	}
	if evt != nil {
		t.Errorf("Next event = %+v, want nil", evt)
	}
}

func TestDecoder_CRCCorruption(t *testing.T) {
	frame := encode(t, eventHeaders("messageStart"), []byte(`{"role":"assistant"}`))
	// The trailing 4 bytes are the message CRC. Flip a bit in the last
	// byte to force a CRC mismatch.
	corrupt := make([]byte, len(frame))
	copy(corrupt, frame)
	corrupt[len(corrupt)-1] ^= 0xFF

	dec := awsstream.NewDecoder(bytes.NewReader(corrupt))
	_, err := dec.Next()
	if err == nil {
		t.Fatal("Next: nil error on corrupt CRC, want non-nil")
	}
	if err == io.EOF {
		t.Errorf("Next err = io.EOF, want a framing error")
	}
}

func TestDecoder_TruncatedFrame(t *testing.T) {
	frame := encode(t, eventHeaders("messageStart"), []byte(`{"role":"assistant"}`))
	// Truncate one byte off the tail — this lands mid-CRC, where the
	// SDK's io.ReadFull on the 4-byte trailing CRC reads some bytes
	// before failing and returns io.ErrUnexpectedEOF. (A truncation that
	// lands exactly on a 4-byte boundary surfaces as io.EOF, which is
	// indistinguishable from a clean stream end — the SDK provides no
	// way to disambiguate. Real-world HTTP truncation returns
	// ErrUnexpectedEOF from the body reader, so this matches production.)
	truncated := frame[:len(frame)-1]

	dec := awsstream.NewDecoder(bytes.NewReader(truncated))
	_, err := dec.Next()
	if err == nil {
		t.Fatal("Next: nil error on truncated frame, want non-nil")
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Next err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDecoder_PayloadBufferIndependence(t *testing.T) {
	// The SDK reuses its internal payload buffer across Decode calls. The
	// decoder must copy payload bytes into each Event so that a prior
	// Event's Payload survives subsequent Next() calls. This is the
	// regression guard.
	first := []byte(`{"delta":{"text":"hello"},"contentBlockIndex":0}`)
	second := []byte(`{"delta":{"text":"WORLDWIDE"},"contentBlockIndex":0}`)

	var buf bytes.Buffer
	buf.Write(encode(t, eventHeaders("contentBlockDelta"), first))
	buf.Write(encode(t, eventHeaders("contentBlockDelta"), second))

	dec := awsstream.NewDecoder(&buf)

	evt1, err := dec.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// Snapshot the bytes the caller would observe right now.
	firstSnapshot := make([]byte, len(evt1.Payload))
	copy(firstSnapshot, evt1.Payload)

	evt2, err := dec.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}

	if !bytes.Equal(evt1.Payload, firstSnapshot) {
		t.Errorf("first Event.Payload was overwritten by second Next:\n got: %s\nwant: %s", evt1.Payload, firstSnapshot)
	}
	if !bytes.Equal(evt1.Payload, first) {
		t.Errorf("first Event.Payload = %s, want %s", evt1.Payload, first)
	}
	if !bytes.Equal(evt2.Payload, second) {
		t.Errorf("second Event.Payload = %s, want %s", evt2.Payload, second)
	}
}
