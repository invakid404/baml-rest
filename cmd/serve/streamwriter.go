package main

import (
	"bufio"
	"context"
	"mime"
	"strconv"
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
	NDJSONEventPing  NDJSONEventType = "heartbeat"
)

// NDJSONEvent represents a single NDJSON streaming event.
type NDJSONEvent struct {
	Type  NDJSONEventType `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
	Raw   string          `json:"raw,omitempty"`
	Error string          `json:"error,omitempty"`
}

// serverRepresentations lists the stream formats the server can produce,
// ordered by server preference (used as a final tie-breaker).
var serverRepresentations = [...]struct {
	format   StreamFormat
	mainType string
	subType  string
}{
	{StreamFormatSSE, "text", "event-stream"},
	{StreamFormatNDJSON, "application", "x-ndjson"},
}

// acceptEntry is a parsed media range from an Accept header.
type acceptEntry struct {
	mainType string
	subType  string
	q        float64
	index    int // position in the Accept header (for tie-breaking)
}

// parseAcceptEntries parses all valid media ranges from an Accept header value.
func parseAcceptEntries(accept string) []acceptEntry {
	parts := strings.Split(accept, ",")
	entries := make([]acceptEntry, 0, len(parts))

	for i, part := range parts {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}

		q, ok := parseAcceptQuality(params["q"])
		if !ok {
			continue
		}

		slash := strings.IndexByte(mediaType, '/')
		if slash < 0 {
			continue
		}

		entries = append(entries, acceptEntry{
			mainType: mediaType[:slash],
			subType:  mediaType[slash+1:],
			q:        q,
			index:    i,
		})
	}

	return entries
}

// matchSpecificity returns the specificity of an Accept entry for a server
// representation: 3 = exact type/subtype, 2 = type/*, 1 = */*, 0 = no match.
func matchSpecificity(entryMain, entrySub, reprMain, reprSub string) int {
	if strings.EqualFold(entryMain, reprMain) && strings.EqualFold(entrySub, reprSub) {
		return 3 // exact match
	}
	if strings.EqualFold(entryMain, reprMain) && entrySub == "*" {
		return 2 // type/*
	}
	if entryMain == "*" && entrySub == "*" {
		return 1 // */*
	}
	return 0
}

// reprScore holds the effective quality score of a server representation after
// matching it against the Accept list using most-specific-match-wins semantics.
type reprScore struct {
	matched     bool
	q           float64
	specificity int // specificity of the winning Accept entry (3=exact, 2=type/*, 1=*/*)
	acceptIndex int // index of the winning Accept entry
}

// scoreRepresentation finds the most-specific Accept entry that matches the
// given representation and returns its quality value. When two entries share
// the same specificity level, the one that appeared first in the Accept header
// wins (lowest index).
func scoreRepresentation(reprMain, reprSub string, entries []acceptEntry) reprScore {
	bestSpecificity := 0
	result := reprScore{}

	for _, entry := range entries {
		spec := matchSpecificity(entry.mainType, entry.subType, reprMain, reprSub)
		if spec == 0 || spec < bestSpecificity {
			continue
		}

		if spec > bestSpecificity {
			bestSpecificity = spec
			result = reprScore{matched: true, q: entry.q, specificity: spec, acceptIndex: entry.index}
		}
	}

	return result
}

// NegotiateStreamFormatFromAccept determines the stream format from an Accept
// header value using RFC 7231 most-specific-match-wins semantics.
//
// It returns the chosen StreamFormat and true when a format is acceptable, or
// (StreamFormatSSE, false) when no representation is acceptable (the caller
// should return HTTP 406).
func NegotiateStreamFormatFromAccept(accept string) (StreamFormat, bool) {
	if accept == "" {
		return StreamFormatSSE, true
	}

	entries := parseAcceptEntries(accept)
	if len(entries) == 0 {
		// Every entry was unparseable; treat as absent Accept header.
		return StreamFormatSSE, true
	}

	type candidate struct {
		format      StreamFormat
		q           float64
		specificity int
		acceptIndex int
	}

	var best *candidate

	for _, repr := range serverRepresentations {
		score := scoreRepresentation(repr.mainType, repr.subType, entries)
		if !score.matched || score.q == 0 {
			continue
		}

		better := best == nil ||
			score.q > best.q ||
			(score.q == best.q && score.specificity > best.specificity) ||
			(score.q == best.q && score.specificity == best.specificity && score.acceptIndex < best.acceptIndex)

		if better {
			best = &candidate{format: repr.format, q: score.q, specificity: score.specificity, acceptIndex: score.acceptIndex}
		}
	}

	if best != nil {
		return best.format, true
	}

	return StreamFormatSSE, false
}

// parseAcceptQuality parses an Accept quality value and validates the range.
func parseAcceptQuality(raw string) (float64, bool) {
	if raw == "" {
		return 1, true
	}

	q, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || q < 0 || q > 1 {
		return 0, false
	}

	return q, true
}

// NDJSONStreamWriterPublisher implements StreamPublisher for native Fiber streaming.
type NDJSONStreamWriterPublisher struct {
	w      *bufio.Writer
	cancel context.CancelFunc

	mu sync.Mutex
}

func encodeNDJSONEvent(event *NDJSONEvent) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (p *NDJSONStreamWriterPublisher) writeFrame(frame []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := p.w.Write(frame); err != nil {
		p.cancel()
		return err
	}

	if err := p.w.Flush(); err != nil {
		p.cancel()
		return err
	}

	return nil
}

func (p *NDJSONStreamWriterPublisher) writeEvent(event *NDJSONEvent) error {
	frame, err := encodeNDJSONEvent(event)
	if err != nil {
		p.cancel()
		return err
	}

	return p.writeFrame(frame)
}

func (p *NDJSONStreamWriterPublisher) writeKeepalive() error {
	return p.writeEvent(&NDJSONEvent{Type: NDJSONEventPing})
}

func (p *NDJSONStreamWriterPublisher) startKeepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	go func() {
		if ctx.Err() != nil {
			return
		}

		if err := p.writeKeepalive(); err != nil {
			return
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}

				if err := p.writeKeepalive(); err != nil {
					return
				}
			}
		}
	}()
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
		reqCtx.Response.ImmediateHeaderFlush = true
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithCancel(streamParentCtx)
		defer cancel()

		publisher := &NDJSONStreamWriterPublisher{
			w:      w,
			cancel: cancel,
		}
		publisher.startKeepalive(streamCtx, normalizeStreamKeepaliveInterval(streamKeepaliveInterval))

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

	// fasthttp RequestCtx.Done() is server-shutdown scoped, not client-disconnect scoped.
	// We force early stream writes and periodic keepalives so disconnects surface
	// as write errors and upstream generation is canceled promptly.
	defaultStreamKeepaliveInterval = 1 * time.Second
	minStreamKeepaliveInterval     = 100 * time.Millisecond
)

func normalizeStreamKeepaliveInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultStreamKeepaliveInterval
	}
	if interval < minStreamKeepaliveInterval {
		return minStreamKeepaliveInterval
	}
	return interval
}

// SSEStreamWriterPublisher implements StreamPublisher for native Fiber SSE streaming.
type SSEStreamWriterPublisher struct {
	w        *bufio.Writer
	cancel   context.CancelFunc
	needsRaw bool

	mu sync.Mutex
}

func formatSSEEvent(eventType string, payload string) string {
	var frame strings.Builder

	if eventType != "" {
		frame.WriteString("event: ")
		frame.WriteString(eventType)
		frame.WriteByte('\n')
	}

	if payload != "" {
		for _, line := range strings.Split(payload, "\n") {
			frame.WriteString("data: ")
			frame.WriteString(line)
			frame.WriteByte('\n')
		}
	}

	frame.WriteByte('\n')
	return frame.String()
}

func formatSSEComment(comment string) string {
	return ": " + comment + "\n\n"
}

func (p *SSEStreamWriterPublisher) writeFrame(frame string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := p.w.WriteString(frame); err != nil {
		p.cancel()
		return err
	}

	if err := p.w.Flush(); err != nil {
		p.cancel()
		return err
	}

	return nil
}

func (p *SSEStreamWriterPublisher) writeEvent(eventType string, payload string) error {
	return p.writeFrame(formatSSEEvent(eventType, payload))
}

func (p *SSEStreamWriterPublisher) writeComment(comment string) error {
	return p.writeFrame(formatSSEComment(comment))
}

func (p *SSEStreamWriterPublisher) startKeepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	go func() {
		if ctx.Err() != nil {
			return
		}

		if err := p.writeComment("keepalive"); err != nil {
			return
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}

				if err := p.writeComment("keepalive"); err != nil {
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
		p.cancel()
		return err
	}

	return p.writeEvent("", payload)
}

func (p *SSEStreamWriterPublisher) PublishFinal(data []byte, raw string) error {
	payload, err := p.formatPayload(data, raw)
	if err != nil {
		p.cancel()
		return err
	}

	return p.writeEvent(sseEventFinal, payload)
}

func (p *SSEStreamWriterPublisher) PublishReset() error {
	return p.writeEvent(sseEventReset, "{}")
}

func (p *SSEStreamWriterPublisher) PublishError(errMsg string) error {
	payload, err := json.Marshal(map[string]string{"error": errMsg})
	if err != nil {
		return p.writeEvent(sseEventError, errMsg)
	}

	return p.writeEvent(sseEventError, unsafeutil.BytesToString(payload))
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
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithCancel(streamParentCtx)
		defer cancel()

		publisher := &SSEStreamWriterPublisher{
			w:        w,
			cancel:   cancel,
			needsRaw: streamMode.NeedsRaw(),
		}
		publisher.startKeepalive(streamCtx, normalizeStreamKeepaliveInterval(streamKeepaliveInterval))

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
