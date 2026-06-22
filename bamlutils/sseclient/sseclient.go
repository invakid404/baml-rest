// Package sseclient provides a minimal Server-Sent Events (SSE) parser for
// consuming streaming responses from LLM provider HTTP APIs.
//
// It implements the subset of the SSE protocol that LLM providers actually use:
// event type, data fields, id fields, and blank-line delimiters. It does not
// handle reconnection or last-event-id semantics — those are irrelevant for
// one-shot LLM streaming responses.
package sseclient

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
)

const (
	sseScannerInitialBufferSize = 64 * 1024
	sseScannerMaxTokenSize      = 1024 * 1024
)

// sseScannerBufferPool retains the 64 KiB initial buffer that backs each
// bufio.Scanner created by scan. Tokens above 64 KiB cause the scanner to
// allocate a larger internal buffer that is not written back into the
// caller's slice pointer, so the pool stays bounded at the initial size.
var sseScannerBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, sseScannerInitialBufferSize)
		return &buf
	},
}

func getSSEScannerBuffer() *[]byte {
	bufp := sseScannerBufferPool.Get().(*[]byte)
	*bufp = (*bufp)[:0]
	if cap(*bufp) < sseScannerInitialBufferSize {
		buf := make([]byte, 0, sseScannerInitialBufferSize)
		bufp = &buf
	}
	return bufp
}

func putSSEScannerBuffer(bufp *[]byte) {
	if bufp == nil {
		return
	}
	if cap(*bufp) != sseScannerInitialBufferSize {
		return
	}
	*bufp = (*bufp)[:0]
	sseScannerBufferPool.Put(bufp)
}

// Event represents a single SSE event parsed from the stream.
type Event struct {
	// Type is the SSE event type (from "event:" lines). Empty string means
	// the default "message" type. LLM providers may use custom types like
	// "content_block_delta" (Anthropic) or leave it empty (OpenAI).
	Type string

	// Data is the concatenated content of all "data:" lines for this event.
	// Multiple data lines are joined with "\n". For LLM providers this is
	// typically a single JSON object per event.
	Data string

	// ID is the event ID from "id:" lines, if present. Most LLM providers
	// do not use this field.
	ID string
}

// Stream reads SSE events from r until EOF, an error, or ctx cancellation.
// It returns a receive-only channel of events and a receive-only channel for
// the terminal error (nil on clean EOF). The error channel receives exactly
// one value when the stream ends. Both channels are closed when done.
//
// The caller should read from events until it is closed, then read the error:
//
//	events, errc := sseclient.Stream(ctx, resp.Body)
//	for ev := range events {
//	    // process ev
//	}
//	if err := <-errc; err != nil {
//	    // handle stream error
//	}
func Stream(ctx context.Context, r io.Reader) (<-chan Event, <-chan error) {
	events := make(chan Event, 16)
	errc := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errc)
		errc <- scan(ctx, r, events)
	}()

	return events, errc
}

// scan is the internal SSE parser loop. It reads lines from r and assembles
// them into Event values sent on the out channel.
func scan(ctx context.Context, r io.Reader, out chan<- Event) error {
	bufp := getSSEScannerBuffer()
	defer putSSEScannerBuffer(bufp)

	scanner := bufio.NewScanner(r)
	// LLM SSE events are typically small JSON objects, but set a generous
	// max line size to handle edge cases (e.g., base64-encoded images).
	// The initial buffer is pooled.
	//
	// Parsing uses scanner.Bytes() (a view over the pooled scanner buffer
	// that is reused/overwritten on the next Scan) to avoid a per-line string
	// allocation. This is safe ONLY because no scanner.Bytes() view ever
	// escapes this loop: data is copied into the owned dataBuf builder via
	// Write, and the event/id fields are materialized with string(...) which
	// copies. Event.Data is produced by dataBuf.String(), a view over the
	// builder's own storage (not reused after Reset). NEVER assign a
	// scanner.Bytes() slice into an Event or send it on the channel — the
	// channel is buffered (cap 16) so the parser runs ahead of consumers, and
	// the underlying bytes would be overwritten before consumers read them.
	scanner.Buffer((*bufp)[:0], sseScannerMaxTokenSize)

	var (
		eventType string
		dataBuf   strings.Builder
		hasData   bool
		eventID   string
	)

	for scanner.Scan() {
		// Check for context cancellation between lines.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// line aliases the scanner's pooled buffer; valid only until the next
		// Scan(). It must never be stored in an Event or sent on the channel.
		line := scanner.Bytes()

		// Strip trailing \r in case the HTTP server uses \r\n line endings
		// that weren't fully consumed by the scanner's line splitter.
		// Go's bufio.ScanLines handles \r\n but not bare \r. This matches
		// master's strings.TrimRight(line, "\r"): ALL trailing \r are removed,
		// not just one. bytes.TrimRight reslices in place, so it does not
		// allocate and the result still aliases the scanner buffer.
		line = bytes.TrimRight(line, "\r")

		// Blank line = event boundary (dispatch if we have data)
		if len(line) == 0 {
			if hasData {
				ev := Event{
					Type: eventType,
					// dataBuf.String() copies out of the scanner buffer
					// (data was Write-copied into the builder), so Data is
					// owned by the event and safe to send on the channel.
					Data: dataBuf.String(),
					ID:   eventID,
				}

				select {
				case out <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			// Reset for next event
			eventType = ""
			dataBuf.Reset()
			hasData = false
			eventID = ""
			continue
		}

		// SSE comment lines start with ':' — skip them.
		// Providers use these for keepalives (e.g., ": keepalive\n").
		if line[0] == ':' {
			continue
		}

		// Parse "field: value" or "field:value" format on the raw bytes.
		field, value := parseSSELine(line)

		// switch string(field) is special-cased by the compiler to compare
		// against the case constants without allocating a string.
		switch string(field) {
		case "data":
			if hasData {
				// Multiple data lines are joined with newlines per SSE spec.
				dataBuf.WriteByte('\n')
			}
			// Write copies value out of the scanner buffer into builder
			// storage, so the eventual dataBuf.String() is owned.
			dataBuf.Write(value)
			hasData = true
		case "event":
			// string(value) copies; eventType escapes via the Event.
			eventType = string(value)
		case "id":
			// Per SSE spec, ignore IDs containing null.
			if bytes.IndexByte(value, '\x00') < 0 {
				eventID = string(value)
			}
		case "retry":
			// Retry field is not relevant for one-shot LLM streams; ignore.
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// If there's a trailing event without a final blank line, dispatch it.
	// Some providers don't send a trailing blank line before closing.
	if hasData {
		ev := Event{
			Type: eventType,
			Data: dataBuf.String(),
			ID:   eventID,
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// parseSSELine splits an SSE line into field name and value, operating on the
// raw line bytes to avoid allocating an intermediate string.
// Format: "field: value" or "field:value" (space after colon is optional but
// if present the first space is stripped per the SSE spec).
// If there is no colon, the entire line is the field name with empty value.
//
// The returned slices alias the input (and therefore the scanner buffer);
// callers must copy before retaining them past the next Scan().
func parseSSELine(line []byte) (field, value []byte) {
	idx := bytes.IndexByte(line, ':')
	if idx < 0 {
		return line, nil
	}

	field = line[:idx]
	value = line[idx+1:]

	// Per SSE spec: if first character after colon is a space, remove it.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}

	return field, value
}
