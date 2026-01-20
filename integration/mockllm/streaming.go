//go:build integration

package mockllm

import (
	"context"
	"math/rand"
	"net/http"
	"time"
)

// StreamWriter handles writing SSE chunks with timing.
type StreamWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	provider Provider
}

// NewStreamWriter creates a new stream writer.
func NewStreamWriter(w http.ResponseWriter, provider Provider) (*StreamWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return &StreamWriter{
		w:        w,
		flusher:  flusher,
		provider: provider,
	}, true
}

// WriteChunk writes a single chunk and flushes.
func (sw *StreamWriter) WriteChunk(content string, index int) error {
	chunk := sw.provider.FormatChunk(content, index)
	_, err := sw.w.Write([]byte(chunk))
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// WriteFinalChunk writes the final chunk with finish_reason: "stop".
func (sw *StreamWriter) WriteFinalChunk(index int) error {
	chunk := sw.provider.FormatFinalChunk(index)
	_, err := sw.w.Write([]byte(chunk))
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// WriteDone writes the completion marker.
func (sw *StreamWriter) WriteDone() error {
	done := sw.provider.FormatDone()
	_, err := sw.w.Write([]byte(done))
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// StreamResponse streams the scenario content with configured timing.
// The effectiveDelay parameter overrides scenario.InitialDelayMs when specified (>= 0).
func StreamResponse(ctx context.Context, w http.ResponseWriter, scenario *Scenario, provider Provider, effectiveDelay int) error {
	sw, ok := NewStreamWriter(w, provider)
	if !ok {
		return nil // Can't stream, let caller handle
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", provider.ContentType(true))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Initial delay - use the effective delay passed from the caller
	if effectiveDelay > 0 {
		select {
		case <-time.After(time.Duration(effectiveDelay) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	content := scenario.Content
	chunkSize := scenario.ChunkSize
	if chunkSize <= 0 {
		chunkSize = len(content)
	}

	chunkIndex := 0
	for i := 0; i < len(content); i += chunkSize {
		end := i + chunkSize
		if end > len(content) {
			end = len(content)
		}
		chunk := content[i:end]

		// Check for failure injection
		if scenario.FailAfter > 0 && chunkIndex >= scenario.FailAfter {
			return handleFailure(ctx, scenario)
		}

		if err := sw.WriteChunk(chunk, chunkIndex); err != nil {
			return err
		}
		chunkIndex++

		// Inter-chunk delay (skip for last chunk)
		if end < len(content) {
			delay := scenario.ChunkDelayMs
			if scenario.ChunkJitterMs > 0 {
				delay += rand.Intn(scenario.ChunkJitterMs)
			}
			if delay > 0 {
				select {
				case <-time.After(time.Duration(delay) * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}

	// Send final chunk with finish_reason: "stop"
	if err := sw.WriteFinalChunk(chunkIndex); err != nil {
		return err
	}

	return sw.WriteDone()
}

func handleFailure(ctx context.Context, scenario *Scenario) error {
	switch scenario.FailureMode {
	case "timeout":
		// Just hang until context cancels
		<-ctx.Done()
		return ctx.Err()
	case "disconnect":
		// Return error to close connection abruptly
		return errDisconnect
	case "500":
		// Already streaming, can't change status code
		// Return error to close connection
		return errServerError
	default:
		return nil
	}
}

var (
	errDisconnect  = &disconnectError{}
	errServerError = &serverError{}
)

type disconnectError struct{}

func (e *disconnectError) Error() string { return "simulated disconnect" }

type serverError struct{}

func (e *serverError) Error() string { return "simulated server error" }
