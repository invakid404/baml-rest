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
	NDJSONEventData  NDJSONEventType = "data"
	NDJSONEventReset NDJSONEventType = "reset"
	NDJSONEventError NDJSONEventType = "error"
)

// NDJSONEvent represents a single NDJSON streaming event.
type NDJSONEvent struct {
	Type  NDJSONEventType `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
	Raw   string          `json:"raw,omitempty"`
	Error string          `json:"error,omitempty"`
}

// NegotiateStreamFormat determines the stream format based on Accept header.
// Returns NDJSON only if explicitly requested, otherwise defaults to SSE.
func NegotiateStreamFormat(r *http.Request) StreamFormat {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return StreamFormatSSE
	}

	// Parse Accept header - it may contain multiple types with quality values
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.Split(part, ";")[0])
		if mediaType == ContentTypeNDJSON {
			return StreamFormatNDJSON
		}
	}

	return StreamFormatSSE
}

// StreamPublisher is the unified interface for publishing stream events.
// Both SSE and NDJSON implementations use this interface.
type StreamPublisher interface {
	// PublishData sends a data event with parsed data and optional raw response.
	PublishData(data []byte, raw string) error
	// PublishReset sends a reset event indicating client should discard state.
	PublishReset() error
	// PublishError sends an error event.
	PublishError(errMsg string) error
	// Close performs any cleanup needed (e.g., signaling SSE to stop).
	Close()
}

// SSEPublisher implements StreamPublisher for Server-Sent Events.
type SSEPublisher struct {
	server    *sse.Server
	topic     string
	errorType sse.EventType
	resetType sse.EventType
	needsRaw  bool
	cancel    context.CancelFunc
	done      <-chan struct{}
}

// NewSSEPublisher creates an SSE publisher and starts the SSE server.
// It blocks until the SSE connection is ready or context is cancelled.
// Returns nil if the connection could not be established.
func NewSSEPublisher(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	sseServer *sse.Server,
	errorType sse.EventType,
	resetType sse.EventType,
	needsRaw bool,
	pathPrefix string,
	methodName string,
) (*SSEPublisher, context.Context) {
	// Create unique topic for this request
	topic := pathPrefix + "/" + methodName + "/" + ptrString(r)
	ready := make(chan struct{})

	ctx, cancel := context.WithCancel(
		context.WithValue(
			context.WithValue(ctx, sseContextKeyTopic, topic),
			sseContextKeyReady, ready,
		),
	)

	req := r.WithContext(ctx)

	// Start SSE server in background
	sseDone := make(chan struct{})
	go recovery.Go(func() error {
		defer close(sseDone)
		sseServer.ServeHTTP(w, req)
		return nil
	})

	// Wait for SSE connection to be established
	select {
	case <-ready:
		// SSE is ready
	case <-ctx.Done():
		// Context cancelled
		cancel()
		<-sseDone
		return nil, ctx
	}

	return &SSEPublisher{
		server:    sseServer,
		topic:     topic,
		errorType: errorType,
		resetType: resetType,
		needsRaw:  needsRaw,
		cancel:    cancel,
		done:      sseDone,
	}, ctx
}

func (p *SSEPublisher) PublishData(data []byte, raw string) error {
	message := &sse.Message{}

	if p.needsRaw {
		wrapped, err := json.Marshal(CallWithRawResponse{
			Data: data,
			Raw:  raw,
		})
		if err != nil {
			return err
		}
		message.AppendData(unsafeutil.BytesToString(wrapped))
	} else {
		message.AppendData(unsafeutil.BytesToString(data))
	}

	return p.server.Publish(message, p.topic)
}

func (p *SSEPublisher) PublishReset() error {
	message := &sse.Message{Type: p.resetType}
	message.AppendData("{}")
	return p.server.Publish(message, p.topic)
}

func (p *SSEPublisher) PublishError(errMsg string) error {
	message := &sse.Message{Type: p.errorType}
	message.AppendData(errMsg)
	return p.server.Publish(message, p.topic)
}

func (p *SSEPublisher) Close() {
	p.cancel()
	<-p.done
}

// NDJSONPublisher implements StreamPublisher for NDJSON format.
type NDJSONPublisher struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	buf       *bufio.Writer
	committed bool
}

// NewNDJSONPublisher creates an NDJSON publisher.
// Headers are committed immediately to start the HTTP streaming response.
func NewNDJSONPublisher(w http.ResponseWriter) *NDJSONPublisher {
	// Set headers for NDJSON streaming
	w.Header().Set("Content-Type", ContentTypeNDJSON)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff") // Prevents buffering

	p := &NDJSONPublisher{
		w:   w,
		buf: bufio.NewWriter(w),
	}

	// Get flusher, unwrapping middleware wrappers if necessary
	p.flusher = getFlusher(w)

	// Immediately flush headers to start the streaming response.
	// Don't call WriteHeader - let it be implicit on first write/flush.
	// This matches how go-sse handles streaming.
	if p.flusher != nil {
		p.flusher.Flush()
	}
	p.committed = true

	return p
}

// getFlusher extracts http.Flusher from a ResponseWriter, unwrapping middleware wrappers if needed.
func getFlusher(w http.ResponseWriter) http.Flusher {
	for {
		if f, ok := w.(http.Flusher); ok {
			return f
		}
		// Try to unwrap middleware wrappers
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
			w = u.Unwrap()
			continue
		}
		return nil
	}
}

func (p *NDJSONPublisher) writeEvent(event *NDJSONEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// Write directly to ResponseWriter (bypassing bufio.Writer)
	// This ensures each event is sent immediately
	if _, err := p.w.Write(data); err != nil {
		return err
	}
	if _, err := p.w.Write([]byte{'\n'}); err != nil {
		return err
	}

	// Flush immediately after EVERY write - critical for streaming
	if p.flusher != nil {
		p.flusher.Flush()
	}

	return nil
}

func (p *NDJSONPublisher) PublishData(data []byte, raw string) error {
	event := &NDJSONEvent{
		Type: NDJSONEventData,
		Data: data,
	}
	if raw != "" {
		event.Raw = raw
	}
	return p.writeEvent(event)
}

func (p *NDJSONPublisher) PublishReset() error {
	return p.writeEvent(&NDJSONEvent{Type: NDJSONEventReset})
}

func (p *NDJSONPublisher) PublishError(errMsg string) error {
	return p.writeEvent(&NDJSONEvent{Type: NDJSONEventError, Error: errMsg})
}

func (p *NDJSONPublisher) Close() {
	// NDJSON doesn't need cleanup
}

// StreamHandlerConfig contains configuration for the unified stream handler.
type StreamHandlerConfig struct {
	SSEServer    *sse.Server
	SSEErrorType sse.EventType
	SSEResetType sse.EventType
	PathPrefix   string
}

// HandleStream is the unified stream handler that routes to SSE or NDJSON.
// It creates the appropriate publisher and uses a single consumer loop.
func HandleStream(
	w http.ResponseWriter,
	r *http.Request,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
	config *StreamHandlerConfig,
) {
	format := NegotiateStreamFormat(r)
	ctx := r.Context()

	var publisher StreamPublisher
	var streamCtx context.Context

	if format == StreamFormatNDJSON {
		publisher = NewNDJSONPublisher(w)
		streamCtx = ctx
	} else {
		var ssePublisher *SSEPublisher
		ssePublisher, streamCtx = NewSSEPublisher(
			ctx, w, r,
			config.SSEServer,
			config.SSEErrorType,
			config.SSEResetType,
			streamMode.NeedsRaw(),
			config.PathPrefix,
			methodName,
		)
		if ssePublisher == nil {
			// SSE connection failed to establish
			return
		}
		publisher = ssePublisher
	}
	defer publisher.Close()

	// Create cancellable context for the stream
	streamCtx, cancel := context.WithCancel(streamCtx)
	defer cancel()

	// Start the stream
	results, err := workerPool.CallStream(streamCtx, methodName, rawBody, streamMode)
	if err != nil {
		httplogger.SetError(r.Context(), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Single consumer loop - the unified pattern
	consumeStream(r.Context(), results, publisher, streamMode)
}

// consumeStream is the single consumer that iterates over pool results
// and publishes through the StreamPublisher interface.
func consumeStream(
	requestCtx context.Context,
	results <-chan *workerplugin.StreamResult,
	publisher StreamPublisher,
	streamMode bamlutils.StreamMode,
) {
	// Accumulate raw deltas for output (internal gRPC sends deltas to save bandwidth)
	var accumulatedRaw strings.Builder

	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			if result.Error != nil {
				httplogger.SetError(requestCtx, workerplugin.NewErrorWithStack(result.Error, result.Stacktrace))
				_ = publisher.PublishError(result.Error.Error())
			}

		case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
			// Handle reset signal (retry occurred)
			if result.Reset {
				accumulatedRaw.Reset()
				if err := publisher.PublishReset(); err != nil {
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

			if err := publisher.PublishData(result.Data, rawForOutput); err != nil {
				workerplugin.ReleaseStreamResult(result)
				drainResults(results)
				return
			}
		}

		workerplugin.ReleaseStreamResult(result)
	}
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
