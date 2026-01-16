package sse

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// SSEChunk represents a single SSE chunk that can provide text or JSON
type SSEChunk interface {
	Text() (string, error)
	JSON() (any, error)
}

// StreamingCall represents a single streaming LLM call with its chunks
type StreamingCall struct {
	Provider string
	Chunks   []SSEChunk
}

// StreamingData holds all streaming call data from a FunctionLog
type StreamingData struct {
	Calls []StreamingCall
}

// GetCurrentContent returns the accumulated content so far from streaming
// by extracting deltas from SSE chunks of the last call only.
// Earlier calls are ignored since they represent failed retry attempts.
func GetCurrentContent(data *StreamingData) (string, error) {
	if len(data.Calls) == 0 {
		return "", nil
	}

	// Only use the last call - earlier calls are failed retries
	call := data.Calls[len(data.Calls)-1]

	var sb strings.Builder
	for _, chunk := range call.Chunks {
		delta, err := ExtractDeltaContent(call.Provider, chunk)
		if err == nil && delta != "" {
			sb.WriteString(delta)
		}
	}

	return sb.String(), nil
}

// ExtractDeltaContent extracts the text delta from an SSE chunk based on provider
func ExtractDeltaContent(provider string, chunk SSEChunk) (string, error) {
	rawText, err := chunk.Text()
	if err != nil {
		return "", fmt.Errorf("failed to get chunk text: %w", err)
	}

	switch provider {
	// OpenAI-compatible providers (Chat Completions API format)
	// Path: choices[0].delta.content
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		return gjson.Get(rawText, "choices.0.delta.content").String(), nil

	// OpenAI Responses API (different format)
	// Path: delta (when type == "response.output_text.delta")
	case "openai-responses":
		if gjson.Get(rawText, "type").String() == "response.output_text.delta" {
			return gjson.Get(rawText, "delta").String(), nil
		}
		return "", nil

	// Anthropic
	// Path: delta.text or delta.thinking (when type == "content_block_delta")
	case "anthropic":
		if gjson.Get(rawText, "type").String() == "content_block_delta" {
			switch gjson.Get(rawText, "delta.type").String() {
			case "text_delta":
				return gjson.Get(rawText, "delta.text").String(), nil
			case "thinking_delta":
				return gjson.Get(rawText, "delta.thinking").String(), nil
			}
		}
		return "", nil

	// Google (both use same format)
	// Path: candidates[0].content.parts[0].text
	case "google-ai", "vertex-ai":
		return gjson.Get(rawText, "candidates.0.content.parts.0.text").String(), nil

	// AWS Bedrock (Debug string format)
	// Path: debug (then parsed via regex)
	case "aws-bedrock":
		return extractBedrockFromDebug(gjson.Get(rawText, "debug").String())

	default:
		return "", fmt.Errorf("unsupported provider: %s", provider)
	}
}

var bedrockTextRegex = regexp.MustCompile(`Text\("((?:[^"\\]|\\.)*)"\)`)

func extractBedrockFromDebug(debugStr string) (string, error) {
	// Only process ContentBlockDelta events
	if !strings.HasPrefix(debugStr, "ContentBlockDelta") {
		return "", nil
	}

	// Extract text from: delta: Some(Text("..."))
	// This regex handles escaped quotes within the text
	matches := bedrockTextRegex.FindStringSubmatch(debugStr)
	if len(matches) < 2 {
		return "", nil
	}

	// Unescape the string (handle \" -> " etc.)
	text := strings.ReplaceAll(matches[1], `\"`, `"`)
	text = strings.ReplaceAll(text, `\\`, `\`)

	return text, nil
}

// IncrementalExtractor extracts SSE content incrementally, tracking which chunks
// have already been processed to avoid re-parsing on each tick.
type IncrementalExtractor struct {
	// callCount tracks the number of calls seen (to detect retries)
	callCount int
	// cursor tracks chunks processed in the current (last) call
	cursor int
	// accumulated content from processed chunks
	accumulated strings.Builder
}

// NewIncrementalExtractor creates a new incremental extractor.
func NewIncrementalExtractor() *IncrementalExtractor {
	return &IncrementalExtractor{}
}

// ExtractResult contains the result of an incremental extraction.
type ExtractResult struct {
	// Delta is the new content extracted from this tick (empty if no new content)
	Delta string
	// Full is the complete accumulated content
	Full string
	// Reset is true if the client should discard accumulated state (retry occurred).
	// This is NOT set on first extraction - only when a retry causes a rebuild.
	Reset bool
}

// Extract processes new SSE chunks incrementally, returning only the delta.
// Parameters:
//   - callCount: total number of calls in the FunctionLog (used to detect retries)
//   - provider: the LLM provider for the current (last) call
//   - chunks: SSE chunks from the current (last) call only
//
// A full rebuild occurs if:
//   - First extraction (initial state)
//   - Call count changed (retry added a new call)
//   - Chunk count decreased (shouldn't happen normally)
func (e *IncrementalExtractor) Extract(callCount int, provider string, chunks []SSEChunk) ExtractResult {
	if callCount == 0 {
		return ExtractResult{}
	}

	// Determine rebuild reasons
	isFirstExtraction := e.callCount == 0
	isRetry := !isFirstExtraction && callCount != e.callCount
	chunksDecreased := !isFirstExtraction && len(chunks) < e.cursor

	needsRebuild := isFirstExtraction || isRetry || chunksDecreased

	if needsRebuild {
		// Full rebuild: reset state and process all chunks from last call
		e.callCount = callCount
		e.cursor = 0
		e.accumulated.Reset()

		for _, chunk := range chunks {
			delta, err := ExtractDeltaContent(provider, chunk)
			if err == nil && delta != "" {
				e.accumulated.WriteString(delta)
			}
		}
		e.cursor = len(chunks)

		return ExtractResult{
			Delta: e.accumulated.String(),
			Full:  e.accumulated.String(),
			// Signal reset when retry occurred or chunks decreased (client state is invalid)
			Reset: isRetry || chunksDecreased,
		}
	}

	// Incremental: only process new chunks
	var deltaBuf strings.Builder
	for i := e.cursor; i < len(chunks); i++ {
		delta, err := ExtractDeltaContent(provider, chunks[i])
		if err == nil && delta != "" {
			deltaBuf.WriteString(delta)
			e.accumulated.WriteString(delta)
		}
	}
	e.cursor = len(chunks)

	return ExtractResult{
		Delta: deltaBuf.String(),
		Full:  e.accumulated.String(),
		Reset: false,
	}
}

// Clear resets the extractor state.
func (e *IncrementalExtractor) Clear() {
	e.callCount = 0
	e.cursor = 0
	e.accumulated.Reset()
}

// Full returns the complete accumulated content without processing new data.
func (e *IncrementalExtractor) Full() string {
	return e.accumulated.String()
}
