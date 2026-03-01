package main

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v3"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/unsafeutil"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// StreamFormat represents the output format for streaming responses.
type StreamFormat int

const (
	StreamFormatSSE StreamFormat = iota
	StreamFormatNDJSON
)

// ContentTypeNDJSON is the MIME type for NDJSON streams.
const ContentTypeNDJSON = "application/x-ndjson"

const contentTypeSSE = "text/event-stream"

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

// NegotiateStreamFormatFromAccept determines the stream format from an Accept header value.
func NegotiateStreamFormatFromAccept(accept string) StreamFormat {
	if accept == "" {
		return StreamFormatSSE
	}

	// Parse Accept header - it may contain multiple types with quality values
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.EqualFold(mediaType, ContentTypeNDJSON) {
			return StreamFormatNDJSON
		}
	}

	return StreamFormatSSE
}

// NDJSONStreamWriterPublisher implements StreamPublisher for native Fiber streaming.
type NDJSONStreamWriterPublisher struct {
	w      *bufio.Writer
	cancel context.CancelFunc
}

func (p *NDJSONStreamWriterPublisher) writeEvent(event *NDJSONEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	if _, err := p.w.Write(data); err != nil {
		p.cancel()
		return err
	}
	if err := p.w.WriteByte('\n'); err != nil {
		p.cancel()
		return err
	}

	if err := p.w.Flush(); err != nil {
		p.cancel()
		return err
	}

	return nil
}

func (p *NDJSONStreamWriterPublisher) PublishData(data []byte, raw string) error {
	event := &NDJSONEvent{Type: NDJSONEventData, Data: data}
	if raw != "" {
		event.Raw = raw
	}
	return p.writeEvent(event)
}

func (p *NDJSONStreamWriterPublisher) PublishFinal(data []byte, raw string) error {
	event := &NDJSONEvent{Type: NDJSONEventFinal, Data: data}
	if raw != "" {
		event.Raw = raw
	}
	return p.writeEvent(event)
}

func (p *NDJSONStreamWriterPublisher) PublishReset() error {
	return p.writeEvent(&NDJSONEvent{Type: NDJSONEventReset})
}

func (p *NDJSONStreamWriterPublisher) PublishError(errMsg string) error {
	return p.writeEvent(&NDJSONEvent{Type: NDJSONEventError, Error: errMsg})
}

func (p *NDJSONStreamWriterPublisher) Close() {
	// No cleanup needed.
}

// HandleNDJSONStreamFiber handles NDJSON stream requests with native Fiber streaming.
func HandleNDJSONStreamFiber(
	c fiber.Ctx,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
	flattenDynamic bool,
) error {
	c.Set(fiber.HeaderContentType, ContentTypeNDJSON)
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	c.Set("X-Content-Type-Options", "nosniff")

	streamParentCtx := c.Context()
	if reqCtx := c.RequestCtx(); reqCtx != nil {
		streamParentCtx = reqCtx
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithCancel(streamParentCtx)
		defer cancel()

		publisher := &NDJSONStreamWriterPublisher{
			w:      w,
			cancel: cancel,
		}

		results, err := workerPool.CallStream(streamCtx, methodName, rawBody, streamMode)
		if err != nil {
			_ = publisher.PublishError(err.Error())
			return
		}

		consumeStream(results, publisher, streamMode, flattenDynamic)
	})
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

const (
	sseEventFinal = "final"
	sseEventReset = "reset"
	sseEventError = "error"

	preFirstByteKeepaliveInterval = 100 * time.Millisecond
)

// SSEStreamWriterPublisher implements StreamPublisher for native Fiber SSE streaming.
type SSEStreamWriterPublisher struct {
	w        *bufio.Writer
	cancel   context.CancelFunc
	needsRaw bool

	mu         sync.Mutex
	wroteEvent bool
}

func (p *SSEStreamWriterPublisher) writeEvent(eventType string, payload string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if eventType != "" {
		if _, err := p.w.WriteString("event: "); err != nil {
			p.cancel()
			return err
		}
		if _, err := p.w.WriteString(eventType); err != nil {
			p.cancel()
			return err
		}
		if err := p.w.WriteByte('\n'); err != nil {
			p.cancel()
			return err
		}
	}

	if payload != "" {
		for _, line := range strings.Split(payload, "\n") {
			if _, err := p.w.WriteString("data: "); err != nil {
				p.cancel()
				return err
			}
			if _, err := p.w.WriteString(line); err != nil {
				p.cancel()
				return err
			}
			if err := p.w.WriteByte('\n'); err != nil {
				p.cancel()
				return err
			}
		}
	}

	if err := p.w.WriteByte('\n'); err != nil {
		p.cancel()
		return err
	}

	if err := p.w.Flush(); err != nil {
		p.cancel()
		return err
	}

	p.wroteEvent = true

	return nil
}

func (p *SSEStreamWriterPublisher) writeComment(comment string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.wroteEvent {
		return false, nil
	}

	if _, err := p.w.WriteString(": "); err != nil {
		p.cancel()
		return false, err
	}
	if _, err := p.w.WriteString(comment); err != nil {
		p.cancel()
		return false, err
	}
	if _, err := p.w.WriteString("\n\n"); err != nil {
		p.cancel()
		return false, err
	}

	if err := p.w.Flush(); err != nil {
		p.cancel()
		return false, err
	}

	return true, nil
}

func (p *SSEStreamWriterPublisher) startKeepaliveUntilFirstEvent(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	go func() {
		wrote, err := p.writeComment("keepalive")
		if err != nil {
			return
		}
		if !wrote {
			return
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wrote, err := p.writeComment("keepalive")
				if err != nil {
					return
				}
				if !wrote {
					return
				}
			}
		}
	}()
}

func (p *SSEStreamWriterPublisher) formatPayload(data []byte, raw string) (string, error) {
	if !p.needsRaw {
		return unsafeutil.BytesToString(data), nil
	}

	wrapped, err := json.Marshal(CallWithRawResponse{Data: data, Raw: raw})
	if err != nil {
		return "", err
	}

	return unsafeutil.BytesToString(wrapped), nil
}

func (p *SSEStreamWriterPublisher) PublishData(data []byte, raw string) error {
	payload, err := p.formatPayload(data, raw)
	if err != nil {
		return err
	}

	return p.writeEvent("", payload)
}

func (p *SSEStreamWriterPublisher) PublishFinal(data []byte, raw string) error {
	payload, err := p.formatPayload(data, raw)
	if err != nil {
		return err
	}

	return p.writeEvent(sseEventFinal, payload)
}

func (p *SSEStreamWriterPublisher) PublishReset() error {
	return p.writeEvent(sseEventReset, "{}")
}

func (p *SSEStreamWriterPublisher) PublishError(errMsg string) error {
	return p.writeEvent(sseEventError, errMsg)
}

func (p *SSEStreamWriterPublisher) Close() {
	// No cleanup needed.
}

// HandleSSEStreamFiber handles SSE stream requests with native Fiber streaming.
func HandleSSEStreamFiber(
	c fiber.Ctx,
	methodName string,
	rawBody []byte,
	streamMode bamlutils.StreamMode,
	workerPool *pool.Pool,
	flattenDynamic bool,
) error {
	c.Set(fiber.HeaderContentType, contentTypeSSE)
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	c.Set("X-Content-Type-Options", "nosniff")

	streamParentCtx := c.Context()
	if reqCtx := c.RequestCtx(); reqCtx != nil {
		reqCtx.Response.ImmediateHeaderFlush = true
		streamParentCtx = reqCtx
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithCancel(streamParentCtx)
		defer cancel()

		publisher := &SSEStreamWriterPublisher{
			w:        w,
			cancel:   cancel,
			needsRaw: streamMode.NeedsRaw(),
		}
		publisher.startKeepaliveUntilFirstEvent(streamCtx, preFirstByteKeepaliveInterval)

		results, err := workerPool.CallStream(streamCtx, methodName, rawBody, streamMode)
		if err != nil {
			_ = publisher.PublishError(err.Error())
			return
		}

		consumeStream(results, publisher, streamMode, flattenDynamic)
	})
}

// consumeStream is the single consumer that iterates over pool results
// and publishes through the StreamPublisher interface.
// If flattenDynamic is true, DynamicProperties fields will be flattened to the root level.
func consumeStream(
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
