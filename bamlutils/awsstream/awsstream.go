// Package awsstream decodes AWS event-stream binary frames
// (application/vnd.amazon.eventstream), the wire format Bedrock's
// ConverseStream API returns instead of SSE.
//
// The decoder is a thin baml-rest-shaped wrapper around the AWS SDK Go v2
// eventstream decoder. It surfaces the three standard event-stream headers
// (:event-type, :message-type, :content-type) as fields on Event and copies
// the payload bytes out of the SDK's reusable internal buffer so callers can
// hold Events past the next Next() call.
//
// Modeled exceptions (:message-type=exception) flow through as ordinary
// Events with MessageType=="exception" — they are protocol data, not
// transport-level failures. Transport-level errors (:message-type=error)
// surface as a *TransportError from Next().
package awsstream

import (
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Standard event-stream message header names.
const (
	HeaderEventType     = ":event-type"
	HeaderMessageType   = ":message-type"
	HeaderContentType   = ":content-type"
	HeaderExceptionType = ":exception-type"
	HeaderErrorCode     = ":error-code"
	HeaderErrorMessage  = ":error-message"
)

// Standard :message-type values.
const (
	MessageTypeEvent     = "event"
	MessageTypeException = "exception"
	MessageTypeError     = "error"
)

// Event is a decoded AWS event-stream message.
//
// Type carries the message's logical name — for MessageType=="event" it is
// the :event-type header (e.g. "messageStart", "contentBlockDelta",
// "messageStop"); for MessageType=="exception" it is the :exception-type
// header (the modeled exception's shape name). Empty for transport errors,
// which surface as a *TransportError instead.
//
// ContentType is the :content-type header — "application/json" for Bedrock
// ConverseStream events.
//
// Payload is the raw, uninterpreted message body. The decoder does NOT
// unmarshal it; callers parse it per the (Type, ContentType) pair. Payload
// bytes are owned by the Event — the decoder copies them out of the SDK's
// reusable internal buffer, so an Event remains valid after subsequent
// Next() calls.
type Event struct {
	Type        string
	MessageType string
	ContentType string
	Payload     []byte
}

// Decoder reads AWS event-stream frames from an io.Reader.
//
// Construct one with NewDecoder, then call Next() in a loop until it returns
// io.EOF. Next() returns a *TransportError when the wire carries a
// :message-type=error frame, and returns the underlying SDK error verbatim
// on framing, CRC, or truncation failures.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	r   io.Reader
	dec *eventstream.Decoder

	// payloadBuf is reused across Decode calls. Per the SDK contract, the
	// caller is responsible for consuming the previous Message.Payload
	// before reusing the buffer; Next() satisfies that contract by copying
	// the payload into a fresh slice on every call.
	payloadBuf []byte
}

// NewDecoder returns a Decoder reading from r. The reader should produce
// raw eventstream framed bytes — typically the body of an HTTP response
// with Content-Type: application/vnd.amazon.eventstream.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{
		r:   r,
		dec: eventstream.NewDecoder(),
	}
}

// Next reads and returns the next event from the stream.
//
// Returns (nil, io.EOF) cleanly when the reader is at end-of-stream at a
// message boundary. Returns (nil, *TransportError) when the wire carries a
// :message-type=error frame. Returns (nil, err) verbatim from the SDK
// decoder on framing / CRC / header-parse / truncation errors (the SDK
// returns io.ErrUnexpectedEOF on a truncated frame).
//
// Modeled exceptions (:message-type=exception) are returned as ordinary
// Events with MessageType=="exception" and Type set to :exception-type;
// callers decide whether to treat them as fatal.
func (d *Decoder) Next() (*Event, error) {
	msg, err := d.dec.Decode(d.r, d.payloadBuf[:0])
	if err != nil {
		return nil, err
	}
	// Hold onto the underlying capacity for reuse on the next call. The
	// payload bytes themselves are copied into the Event below, so reusing
	// the backing array is safe regardless of when the caller looks at
	// Event.Payload.
	if cap(msg.Payload) > cap(d.payloadBuf) {
		d.payloadBuf = msg.Payload[:0]
	}

	messageType := stringHeader(msg.Headers, HeaderMessageType)

	if messageType == MessageTypeError {
		return nil, &TransportError{
			Code:    stringHeader(msg.Headers, HeaderErrorCode),
			Message: stringHeader(msg.Headers, HeaderErrorMessage),
		}
	}

	typeName := stringHeader(msg.Headers, HeaderEventType)
	if typeName == "" && messageType == MessageTypeException {
		typeName = stringHeader(msg.Headers, HeaderExceptionType)
	}

	evt := &Event{
		Type:        typeName,
		MessageType: messageType,
		ContentType: stringHeader(msg.Headers, HeaderContentType),
	}
	if len(msg.Payload) > 0 {
		payload := make([]byte, len(msg.Payload))
		copy(payload, msg.Payload)
		evt.Payload = payload
	}

	return evt, nil
}

// TransportError is returned by Decoder.Next when the wire carries a
// :message-type=error frame. It corresponds to a transport-level failure
// from the AWS event-stream protocol stack (distinct from modeled
// exceptions, which flow through as ordinary Events).
type TransportError struct {
	Code    string
	Message string
}

func (e *TransportError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("awsstream: transport error %s: %s", e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("awsstream: transport error %s", e.Code)
	case e.Message != "":
		return fmt.Sprintf("awsstream: transport error: %s", e.Message)
	default:
		return "awsstream: transport error"
	}
}

// stringHeader returns the value of the named header as a Go string, or ""
// if the header is absent. Non-string typed headers fall back to fmt
// formatting; the three standard headers we surface (:event-type,
// :message-type, :content-type) are always string-typed on Bedrock and the
// other AWS event-stream services.
func stringHeader(hs eventstream.Headers, name string) string {
	v := hs.Get(name)
	if v == nil {
		return ""
	}
	if sv, ok := v.(eventstream.StringValue); ok {
		return string(sv)
	}
	return fmt.Sprintf("%v", v.Get())
}
