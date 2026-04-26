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
	// Initial delay - use the effective delay passed from the caller
	if effectiveDelay > 0 {
		select {
		case <-time.After(time.Duration(effectiveDelay) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Anthropic with scenario.Thinking takes a dedicated path that emits a
	// thinking content block (index 0) before the text content block (index 1).
	// The existing FormatChunk-based path doesn't handle multi-block streams,
	// so we keep it for the default text-only case and branch here when needed.
	if anthropic, ok := provider.(*AnthropicProvider); ok && scenario.Thinking != "" {
		return streamAnthropicWithThinking(ctx, w, scenario, anthropic)
	}

	sw := NewStreamWriter(w, provider)
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

// streamAnthropicWithThinking emits an Anthropic streaming response with a
// thinking content block followed by a text content block. Both blocks honor
// scenario.ChunkSize / ChunkDelayMs / ChunkJitterMs for delta pacing, and
// failure injection is honored across the full event sequence (counted from
// the first delta of either block, matching the existing semantics).
func streamAnthropicWithThinking(ctx context.Context, w *bufio.Writer, scenario *Scenario, provider *AnthropicProvider) error {
	deltaIndex := 0

	// message_start
	if _, err := w.WriteString(provider.AnthropicMessageStart()); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Thinking content block (index 0)
	if err := streamAnthropicBlock(ctx, w, provider, "thinking", "thinking_delta", scenario.Thinking, 0, scenario, &deltaIndex); err != nil {
		return err
	}

	// Text content block (index 1)
	if err := streamAnthropicBlock(ctx, w, provider, "text", "text_delta", scenario.Content, 1, scenario, &deltaIndex); err != nil {
		return err
	}

	// message_delta + message_stop
	if _, err := w.WriteString(provider.AnthropicMessageStop()); err != nil {
		return err
	}
	return w.Flush()
}

// streamAnthropicBlock streams a single Anthropic content_block (start, one
// or more deltas honoring scenario chunking, stop). deltaIndex is the
// running cross-block delta count used by the FailAfter check so it matches
// the chunk counting semantics of the default StreamResponse path.
func streamAnthropicBlock(
	ctx context.Context,
	w *bufio.Writer,
	provider *AnthropicProvider,
	blockType, deltaType, content string,
	blockIndex int,
	scenario *Scenario,
	deltaIndex *int,
) error {
	if _, err := w.WriteString(provider.AnthropicBlockStart(blockType, blockIndex)); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	chunkSize := scenario.ChunkSize
	if chunkSize <= 0 {
		chunkSize = len(content)
	}
	if chunkSize <= 0 {
		// Empty content — emit one empty delta so the block has a delta event,
		// matching the shape of FormatChunk for an empty content scenario.
		if _, err := w.WriteString(provider.AnthropicBlockDelta(deltaType, "", blockIndex)); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		*deltaIndex++
	} else {
		for i := 0; i < len(content); i += chunkSize {
			end := i + chunkSize
			if end > len(content) {
				end = len(content)
			}
			chunk := content[i:end]

			if scenario.FailAfter > 0 && *deltaIndex >= scenario.FailAfter {
				return handleFailure(ctx, scenario)
			}

			if _, err := w.WriteString(provider.AnthropicBlockDelta(deltaType, chunk, blockIndex)); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			*deltaIndex++

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
	}

	if _, err := w.WriteString(provider.AnthropicBlockStop(blockIndex)); err != nil {
		return err
	}
	return w.Flush()
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
