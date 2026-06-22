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
	"errors"
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

// EventBytes is the byte-oriented view of a parsed SSE event handed to an
// EventFunc by ScanEvents. Every slice aliases parser-owned scratch buffers
// that are RESET and reused on the next event — the views are valid ONLY for
// the duration of the EventFunc invocation that received them.
//
// The callback MUST fully consume these bytes (e.g. parse Data via
// gjson.GetBytes) before returning, and MUST NOT let any slice — nor any
// gjson.Result / string that aliases one — escape the call. gjson.GetBytes
// copies the matched Raw/Str out before returning, so DeltaParts strings
// built from it are owned and safe to retain; the raw Data/Type/ID slices
// themselves are not.
//
// This is the whole point of the synchronous path: because the parser never
// runs ahead of the consumer (no channel, no goroutine), Data need not be
// materialized into a durable string per frame the way Stream's Event.Data
// must be.
type EventBytes struct {
	// Type is the SSE event type (from "event:" lines), empty for the
	// default "message" type. Aliases a parser-owned buffer.
	Type []byte

	// Data is the concatenated content of all "data:" lines for this event,
	// joined with "\n". Aliases a parser-owned buffer.
	Data []byte

	// ID is the event ID from "id:" lines, if present. Aliases a
	// parser-owned buffer.
	ID []byte
}

// EventFunc is invoked synchronously by ScanEvents for each parsed SSE event,
// in stream order. Returning a non-nil error stops scanning: ScanEvents
// returns that error verbatim, EXCEPT ErrStopScan, which stops scanning
// cleanly (ScanEvents returns nil) so a caller can halt for its own reason
// while tracking its own error separately.
type EventFunc func(ev EventBytes) error

// ErrStopScan, when returned by an EventFunc, stops ScanEvents cleanly:
// ScanEvents returns nil rather than surfacing this sentinel as a stream
// error. Use it when the caller wants to abort parsing for a caller-side
// reason (e.g. a downstream context cancellation or a tracked extraction
// error) without it being misclassified as a transport failure.
var ErrStopScan = errors.New("sseclient: stop scan")

// ScanEvents reads SSE events from r until EOF, an error, or ctx
// cancellation, invoking fn synchronously for each event. Unlike Stream, it
// uses NO channel and NO goroutine — fn runs inline on the parsing goroutine,
// so the event bytes handed to fn never cross a buffering boundary and need
// not be copied into a durable string. This lets a caller parse Data in place
// (gjson.GetBytes) without the per-frame Event.Data allocation Stream
// requires.
//
// fn receives EventBytes whose slices alias parser-owned scratch buffers that
// are reused on the next event; fn must fully consume them before returning
// (see EventBytes). The terminal error is returned directly (nil on clean
// EOF), with the same semantics as the value Stream delivers on its error
// channel before classification.
//
// The public Stream channel API is unchanged and remains the path for all
// concurrent/buffered consumers; ScanEvents is the additional synchronous
// path for callers that can consume each event in place.
func ScanEvents(ctx context.Context, r io.Reader, fn EventFunc) error {
	bufp := getSSEScannerBuffer()
	defer putSSEScannerBuffer(bufp)

	scanner := bufio.NewScanner(r)
	scanner.Buffer((*bufp)[:0], sseScannerMaxTokenSize)

	// Reused per-event scratch buffers. Field values are append-copied out of
	// the scanner buffer into these (so they survive across the several Scan
	// iterations that make up one event), and reset to [:0] at each event
	// boundary so steady-state parsing allocates nothing per token. The
	// buffers — and therefore the EventBytes views built from them — are
	// reused on the next event, which is safe precisely because fn consumes
	// them synchronously before the next Scan.
	var (
		typeBuf []byte
		dataBuf []byte
		idBuf   []byte
		hasData bool
	)

	emit := func() error {
		// Re-check ctx at dispatch time, mirroring the channel-based Stream's
		// `select { case out <- ev: case <-ctx.Done(): return ctx.Err() }`.
		// The top-of-loop check alone leaves two gaps: a concurrent cancel in
		// the window between that check and this dispatch, and the trailing
		// (post-loop) event below — which the loop-body check never guards
		// because the loop exits via a false Scan(). Checking here stops a
		// cancelled stream promptly without ever entering fn for a further
		// event. ctx.Err() is allocation-free. Returned ctx.Err() is not
		// ErrStopScan, so callers surface it as the terminal error.
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(EventBytes{Type: typeBuf, Data: dataBuf, ID: idBuf})
	}

	for scanner.Scan() {
		// Check for context cancellation between lines.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// line aliases the scanner's pooled buffer; valid only until the next
		// Scan(). Field values are copied out of it via append below.
		line := bytes.TrimRight(scanner.Bytes(), "\r")

		// Blank line = event boundary (dispatch if we have data).
		if len(line) == 0 {
			if hasData {
				if err := emit(); err != nil {
					if errors.Is(err, ErrStopScan) {
						return nil
					}
					return err
				}
			}
			// Reset for next event. Truncate (not reallocate) so the backing
			// arrays are reused.
			typeBuf = typeBuf[:0]
			dataBuf = dataBuf[:0]
			idBuf = idBuf[:0]
			hasData = false
			continue
		}

		// SSE comment lines start with ':' — skip them (keepalives).
		if line[0] == ':' {
			continue
		}

		field, value := parseSSELine(line)

		switch string(field) {
		case "data":
			if hasData {
				// Multiple data lines are joined with newlines per SSE spec.
				dataBuf = append(dataBuf, '\n')
			}
			// append copies value out of the scanner buffer into dataBuf.
			dataBuf = append(dataBuf, value...)
			hasData = true
		case "event":
			// Last event: line wins per spec; overwrite by truncating first.
			typeBuf = append(typeBuf[:0], value...)
		case "id":
			// Per SSE spec, ignore IDs containing null.
			if bytes.IndexByte(value, '\x00') < 0 {
				idBuf = append(idBuf[:0], value...)
			}
		case "retry":
			// Not relevant for one-shot LLM streams; ignore.
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Trailing event without a final blank line.
	if hasData {
		if err := emit(); err != nil {
			if errors.Is(err, ErrStopScan) {
				return nil
			}
			return err
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
