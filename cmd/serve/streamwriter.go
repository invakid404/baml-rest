package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/goccy/go-json"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/httplogger"
	"github.com/invakid404/baml-rest/internal/unsafeutil"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/tmaxmax/go-sse"
)

// StreamFormat represents the output format for streaming responses.
type StreamFormat int

const (
	StreamFormatSSE StreamFormat = iota
	StreamFormatNDJSON
)

// ContentTypeNDJSON is the MIME type for NDJSON streams.
const ContentTypeNDJSON = "application/x-ndjson"

// NDJSONEventType represents the type of NDJSON streaming event.
type NDJSONEventType string

const (
	NDJSONEventPartial NDJSONEventType = "partial"
	NDJSONEventFinal   NDJSONEventType = "final"
	NDJSONEventReset   NDJSONEventType = "reset"
	NDJSONEventError   NDJSONEventType = "error"
)

// NDJSONEvent represents a single NDJSON streaming event.
// The structure varies based on Type:
//   - "partial"/"final": Data field contains the parsed data, Raw contains accumulated raw (if stream-with-raw)
//   - "reset": No other fields
//   - "error": Error field contains the error message
type NDJSONEvent struct {
	Type  NDJSONEventType `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
	Raw   string          `json:"raw,omitempty"`
	Error string          `json:"error,omitempty"`
}

// NDJSONWriter writes NDJSON events to an HTTP response.
type NDJSONWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	buf     *bufio.Writer
}

// NewNDJSONWriter creates a new NDJSON writer for the given response.
func NewNDJSONWriter(w http.ResponseWriter) *NDJSONWriter {
	// Set headers for NDJSON streaming
	w.Header().Set("Content-Type", ContentTypeNDJSON)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	nw := &NDJSONWriter{
		w:   w,
		buf: bufio.NewWriter(w),
	}

	// Get flusher if available
	if f, ok := w.(http.Flusher); ok {
		nw.flusher = f
	}

	return nw
}

// WriteEvent writes a single NDJSON event and flushes.
func (nw *NDJSONWriter) WriteEvent(event *NDJSONEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	if _, err := nw.buf.Write(data); err != nil {
		return err
	}
	if err := nw.buf.WriteByte('\n'); err != nil {
		return err
	}

	if err := nw.buf.Flush(); err != nil {
		return err
	}

	if nw.flusher != nil {
		nw.flusher.Flush()
	}

	return nil
}

// WritePartial writes a partial data event.
func (nw *NDJSONWriter) WritePartial(data []byte, raw string) error {
	event := &NDJSONEvent{
		Type: NDJSONEventPartial,
		Data: data,
	}
	if raw != "" {
		event.Raw = raw
	}
	return nw.WriteEvent(event)
}

// WriteFinal writes a final data event.
func (nw *NDJSONWriter) WriteFinal(data []byte, raw string) error {
	event := &NDJSONEvent{
		Type: NDJSONEventFinal,
		Data: data,
	}
	if raw != "" {
		event.Raw = raw
	}
	return nw.WriteEvent(event)
}

// WriteReset writes a reset event.
func (nw *NDJSONWriter) WriteReset() error {
	return nw.WriteEvent(&NDJSONEvent{Type: NDJSONEventReset})
}

// WriteError writes an error event.
func (nw *NDJSONWriter) WriteError(errMsg string) error {
	return nw.WriteEvent(&NDJSONEvent{Type: NDJSONEventError, Error: errMsg})
}

// NegotiateStreamFormat determines the stream format based on Accept header.
// Returns NDJSON only if explicitly requested, otherwise defaults to SSE.
func NegotiateStreamFormat(r *http.Request) StreamFormat {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return StreamFormatSSE
	}

	// Parse Accept header - it may contain multiple types with quality values
	// e.g., "application/x-ndjson, text/event-stream;q=0.9"
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.Split(part, ";")[0])
		if mediaType == ContentTypeNDJSON {
			return StreamFormatNDJSON
		}
	}

	return StreamFormatSSE
}

// StreamDataEvent represents data to be sent as a stream event.
// This is used to abstract the common data preparation logic.
type StreamDataEvent struct {
	Data    []byte
	Raw     string
	IsFinal bool
}

// MarshalForSSE marshals the event data for SSE output.
// For stream-with-raw, it wraps data in {"data": ..., "raw": "..."}.
func (e *StreamDataEvent) MarshalForSSE(needsRaw bool) ([]byte, error) {
	if !needsRaw {
		return e.Data, nil
	}
	return json.Marshal(CallWithRawResponse{
		Data: e.Data,
		Raw:  e.Raw,
	})
}

// MarshalForSSEString marshals the event data for SSE output and returns as string.
func (e *StreamDataEvent) MarshalForSSEString(needsRaw bool) (string, error) {
	data, err := e.MarshalForSSE(needsRaw)
	if err != nil {
		return "", err
	}
	return unsafeutil.BytesToString(data), nil
}

// handleNDJSONStream handles streaming responses in NDJSON format.
func handleNDJSONStream(
	w http.ResponseWriter,
	r *http.Request,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	results, err := workerPool.CallStream(ctx, methodName, rawBody, streamMode)
	if err != nil {
		httplogger.SetError(r.Context(), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nw := NewNDJSONWriter(w)

	// Accumulate raw deltas for output (internal gRPC sends deltas to save bandwidth)
	var accumulatedRaw strings.Builder

	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			if result.Error != nil {
				httplogger.SetError(r.Context(), workerplugin.NewErrorWithStack(result.Error, result.Stacktrace))
				_ = nw.WriteError(result.Error.Error())
			}

		case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
			// Handle reset signal (retry occurred) - send reset event and clear accumulated state
			if result.Reset {
				accumulatedRaw.Reset()
				if err := nw.WriteReset(); err != nil {
					workerplugin.ReleaseStreamResult(result)
					drainResults(results)
					return
				}
			}

			// Determine raw output for this event
			var rawForOutput string
			if streamMode.NeedsRaw() {
				if result.Kind == workerplugin.StreamResultKindStream {
					// Accumulate delta into full raw response
					accumulatedRaw.WriteString(result.Raw)
					rawForOutput = accumulatedRaw.String()
				} else {
					// Final contains full raw
					rawForOutput = result.Raw
				}
			}

			// Write the appropriate event type
			var writeErr error
			if result.Kind == workerplugin.StreamResultKindFinal {
				writeErr = nw.WriteFinal(result.Data, rawForOutput)
			} else {
				writeErr = nw.WritePartial(result.Data, rawForOutput)
			}

			if writeErr != nil {
				workerplugin.ReleaseStreamResult(result)
				drainResults(results)
				return
			}
		}

		workerplugin.ReleaseStreamResult(result)
	}
}

// handleSSEStream handles streaming responses in SSE format.
func handleSSEStream(
	w http.ResponseWriter,
	r *http.Request,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
	sseServer *sse.Server,
	sseErrorKind sse.Type,
	sseResetKind sse.Type,
	pathPrefix string,
) {
	// Use request pointer address to create unique topic per request
	topic := pathPrefix + "/" + methodName + "/" + ptrString(r)
	ready := make(chan struct{})

	ctx, cancel := context.WithCancel(
		context.WithValue(
			context.WithValue(r.Context(), sseContextKeyTopic, topic),
			sseContextKeyReady, ready,
		),
	)
	defer cancel()

	r = r.WithContext(ctx)

	results, err := workerPool.CallStream(ctx, methodName, rawBody, streamMode)
	if err != nil {
		httplogger.SetError(r.Context(), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sseDone := make(chan struct{})
	go recovery.Go(func() error {
		defer close(sseDone)
		sseServer.ServeHTTP(w, r)
		return nil
	})

	// Wait for SSE connection to be established before publishing
	select {
	case <-ready:
		// SSE is ready, proceed
	case <-ctx.Done():
		// Context cancelled (client disconnected)
		<-sseDone
		return
	}

	// Accumulate raw deltas for SSE output (internal gRPC sends deltas to save bandwidth)
	var accumulatedRaw strings.Builder

	for result := range results {
		message := &sse.Message{}

		switch result.Kind {
		case workerplugin.StreamResultKindError:
			message.Type = sseErrorKind
			if result.Error != nil {
				message.AppendData(result.Error.Error())
				// Log error with stacktrace if available
				httplogger.SetError(r.Context(), workerplugin.NewErrorWithStack(result.Error, result.Stacktrace))
			}
		case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
			// Handle reset signal (retry occurred) - send reset event and clear accumulated state
			if result.Reset {
				accumulatedRaw.Reset()
				resetMessage := &sse.Message{Type: sseResetKind}
				resetMessage.AppendData("{}")
				if err := sseServer.Publish(resetMessage, topic); err != nil {
					workerplugin.ReleaseStreamResult(result)
					drainResults(results)
					break
				}
			}

			data := result.Data
			if streamMode.NeedsRaw() {
				// Stream messages contain deltas, Final contains full raw
				rawForOutput := result.Raw
				if result.Kind == workerplugin.StreamResultKindStream {
					// Accumulate delta into full raw response for SSE output
					accumulatedRaw.WriteString(result.Raw)
					rawForOutput = accumulatedRaw.String()
				}
				var err error
				data, err = json.Marshal(CallWithRawResponse{
					Data: result.Data,
					Raw:  rawForOutput,
				})
				if err != nil {
					message.Type = sseErrorKind
					message.AppendData("Failed to marshal response: " + err.Error())
					_ = sseServer.Publish(message, topic)
					workerplugin.ReleaseStreamResult(result)
					continue
				}
			}
			// SAFETY: data is owned by this goroutine, used only for this
			// AppendData call, and never modified afterward. The string is consumed
			// immediately by the SSE library.
			message.AppendData(unsafeutil.BytesToString(data))
		}

		workerplugin.ReleaseStreamResult(result)
		if err := sseServer.Publish(message, topic); err != nil {
			// Drain and release remaining results to avoid leaking pooled structs
			drainResults(results)
			break
		}
	}

	// Cancel context to signal SSE server to stop, then wait for it
	cancel()
	<-sseDone
}

// drainResults drains and releases all remaining results from a channel.
func drainResults(results <-chan *workerplugin.StreamResult) {
	for remaining := range results {
		workerplugin.ReleaseStreamResult(remaining)
	}
}

// ptrString returns a string representation of a pointer for topic uniqueness.
func ptrString(p any) string {
	return fmt.Sprintf("%p", p)
}
