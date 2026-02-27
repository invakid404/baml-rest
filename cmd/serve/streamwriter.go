package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
	fiberadaptor "github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/bamlutils"
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

const (
	defaultSSEKeepaliveInterval = 1 * time.Second
	minSSEKeepaliveInterval     = 1 * time.Second
)

// NDJSONEventType represents the type of NDJSON streaming event.
type NDJSONEventType string

const (
	NDJSONEventData  NDJSONEventType = "data"
	NDJSONEventFinal NDJSONEventType = "final"
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
	// PublishData sends a partial data event with parsed data and optional raw response.
	// Partial events may have null values for fields not yet parsed.
	PublishData(data []byte, raw string) error
	// PublishFinal sends the final data event with the complete, validated result.
	PublishFinal(data []byte, raw string) error
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
	finalType sse.EventType
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
	finalType sse.EventType,
	needsRaw bool,
	keepaliveInterval time.Duration,
	pathPrefix string,
	methodName string,
) (*SSEPublisher, context.Context) {
	keepaliveInterval = normalizeSSEKeepaliveInterval(keepaliveInterval)

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

	// Ensure stream context is cancelled when the SSE connection closes.
	// Fiber's net/http adaptor does not always propagate client disconnects via
	// r.Context(), so tie cancellation directly to the SSE server lifecycle.
	go recovery.Go(func() error {
		<-sseDone
		cancel()
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

	// Publish lightweight keepalive comments so dead client connections are
	// detected even before the first real data event is available.
	go recovery.Go(func() error {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				keepalive := &sse.Message{}
				keepalive.AppendComment("keepalive")
				if err := sseServer.Publish(keepalive, topic); err != nil {
					cancel()
					return nil
				}
			}
		}
	})

	return &SSEPublisher{
		server:    sseServer,
		topic:     topic,
		errorType: errorType,
		resetType: resetType,
		finalType: finalType,
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

func (p *SSEPublisher) PublishFinal(data []byte, raw string) error {
	// SSE uses the same format for partial and final data events
	// The distinction is made by using a "final" event type
	message := &sse.Message{Type: p.finalType}

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
		w: w,
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

func getCloseNotify(w http.ResponseWriter) <-chan bool {
	for {
		if cn, ok := w.(http.CloseNotifier); ok {
			return cn.CloseNotify()
		}
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

	// Write JSON event followed by newline
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

func (p *NDJSONPublisher) PublishFinal(data []byte, raw string) error {
	event := &NDJSONEvent{
		Type: NDJSONEventFinal,
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
	SSEServer            *sse.Server
	SSEErrorType         sse.EventType
	SSEResetType         sse.EventType
	SSEFinalType         sse.EventType
	SSEKeepaliveInterval time.Duration
	PathPrefix           string
}

// HandleStream is the unified stream handler that routes to SSE or NDJSON.
// It creates the appropriate publisher and uses a single consumer loop.
// If flattenDynamic is true, DynamicProperties fields will be flattened to the root level.
func HandleStream(
	w http.ResponseWriter,
	r *http.Request,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
	config *StreamHandlerConfig,
	flattenDynamic bool,
) {
	format := NegotiateStreamFormat(r)
	ctx := r.Context()
	if fiberCtx, ok := fiberadaptor.LocalContextFromHTTPRequest(r); ok && fiberCtx != nil {
		mergedCtx, mergedCancel := context.WithCancel(ctx)
		defer mergedCancel()
		go recovery.Go(func() error {
			select {
			case <-fiberCtx.Done():
				mergedCancel()
			case <-mergedCtx.Done():
			}
			return nil
		})
		ctx = mergedCtx
	}

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
			config.SSEFinalType,
			streamMode.NeedsRaw(),
			config.SSEKeepaliveInterval,
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
	if closeNotify := getCloseNotify(w); closeNotify != nil {
		go recovery.Go(func() error {
			select {
			case <-closeNotify:
				cancel()
			case <-streamCtx.Done():
			}
			return nil
		})
	}

	// Start the stream
	results, err := workerPool.CallStream(streamCtx, methodName, rawBody, streamMode)
	if err != nil {
		// Headers are already committed (HTTP 200), so we can't use http.Error.
		// Send the error through the stream instead.
		_ = publisher.PublishError(err.Error())
		return
	}

	// Single consumer loop - the unified pattern
	consumeStream(r.Context(), results, publisher, streamMode, flattenDynamic)
}

// consumeStream is the single consumer that iterates over pool results
// and publishes through the StreamPublisher interface.
// If flattenDynamic is true, DynamicProperties fields will be flattened to the root level.
func consumeStream(
	requestCtx context.Context,
	results <-chan *workerplugin.StreamResult,
	publisher StreamPublisher,
	streamMode bamlutils.StreamMode,
	flattenDynamic bool,
) {
	// Accumulate raw deltas for output (internal gRPC sends deltas to save bandwidth)
	var accumulatedRaw strings.Builder

	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			if result.Error != nil {
				_ = publisher.PublishError(result.Error.Error())
			}

		case workerplugin.StreamResultKindStream:
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
				// Accumulate delta into full raw response
				accumulatedRaw.WriteString(result.Raw)
				rawForOutput = accumulatedRaw.String()
			}

			// Flatten DynamicProperties for dynamic endpoints
			data := result.Data
			if flattenDynamic {
				if flattened, err := bamlutils.FlattenDynamicOutput(data); err == nil {
					data = flattened
				}
			}

			// Publish partial data event (may have null placeholders for unparsed fields)
			if err := publisher.PublishData(data, rawForOutput); err != nil {
				workerplugin.ReleaseStreamResult(result)
				drainResults(results)
				return
			}

		case workerplugin.StreamResultKindFinal:
			// Handle reset signal (retry occurred) - unlikely but possible
			if result.Reset {
				accumulatedRaw.Reset()
				if err := publisher.PublishReset(); err != nil {
					workerplugin.ReleaseStreamResult(result)
					drainResults(results)
					return
				}
			}

			// Determine raw output for final event
			var rawForOutput string
			if streamMode.NeedsRaw() {
				// Final contains full raw
				rawForOutput = result.Raw
			}

			// Flatten DynamicProperties for dynamic endpoints
			data := result.Data
			if flattenDynamic {
				if flattened, err := bamlutils.FlattenDynamicOutput(data); err == nil {
					data = flattened
				}
			}

			// Publish final data event (complete, validated result)
			if err := publisher.PublishFinal(data, rawForOutput); err != nil {
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

func normalizeSSEKeepaliveInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultSSEKeepaliveInterval
	}
	if interval < minSSEKeepaliveInterval {
		return minSSEKeepaliveInterval
	}
	return interval
}
