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
// by extracting deltas from all SSE chunks
func GetCurrentContent(data *StreamingData) (string, error) {
	var sb strings.Builder

	for _, call := range data.Calls {
		for _, chunk := range call.Chunks {
			delta, err := ExtractDeltaContent(call.Provider, chunk)
			if err == nil && delta != "" {
				sb.WriteString(delta)
			}
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
