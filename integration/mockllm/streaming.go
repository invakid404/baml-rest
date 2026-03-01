//go:build integration

package mockllm

import (
	"bufio"
	"context"
	"math/rand"
	"time"
)

// StreamWriter handles writing SSE chunks with timing.
type StreamWriter struct {
	w        *bufio.Writer
	provider Provider
}

// NewStreamWriter creates a new stream writer.
func NewStreamWriter(w *bufio.Writer, provider Provider) *StreamWriter {
	return &StreamWriter{
		w:        w,
		provider: provider,
	}
}

// WriteChunk writes a single chunk and flushes.
func (sw *StreamWriter) WriteChunk(content string, index int) error {
	chunk := sw.provider.FormatChunk(content, index)
	if _, err := sw.w.WriteString(chunk); err != nil {
		return err
	}
	return sw.w.Flush()
}

// WriteFinalChunk writes the final chunk with finish_reason: "stop".
func (sw *StreamWriter) WriteFinalChunk(index int) error {
	chunk := sw.provider.FormatFinalChunk(index)
	if _, err := sw.w.WriteString(chunk); err != nil {
		return err
	}
	return sw.w.Flush()
}

// WriteDone writes the completion marker.
func (sw *StreamWriter) WriteDone() error {
	done := sw.provider.FormatDone()
	if _, err := sw.w.WriteString(done); err != nil {
		return err
	}
	return sw.w.Flush()
}

// StreamResponse streams the scenario content with configured timing.
// The effectiveDelay parameter overrides scenario.InitialDelayMs when specified (>= 0).
func StreamResponse(ctx context.Context, w *bufio.Writer, scenario *Scenario, provider Provider, effectiveDelay int) error {
	sw := NewStreamWriter(w, provider)

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
