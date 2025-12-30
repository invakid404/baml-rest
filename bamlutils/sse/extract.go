package sse

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// OpenAI Chat Completions SSE format
type openAIChatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// OpenAI Responses API SSE format
type openAIResponsesChunk struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// Anthropic SSE format
type anthropicChunk struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

// Google AI SSE format
type googleChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// AWS Bedrock SSE format (debug string wrapped in JSON)
type bedrockChunk struct {
	Debug string `json:"debug"`
}

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
	// Get raw text and parse ourselves for reliability
	rawText, err := chunk.Text()
	if err != nil {
		return "", fmt.Errorf("failed to get chunk text: %w", err)
	}

	switch provider {
	// OpenAI-compatible providers (Chat Completions API format)
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		var c openAIChatChunk
		if err := json.Unmarshal([]byte(rawText), &c); err != nil {
			return "", nil
		}
		if len(c.Choices) > 0 {
			return c.Choices[0].Delta.Content, nil
		}
		return "", nil

	// OpenAI Responses API (different format)
	case "openai-responses":
		var c openAIResponsesChunk
		if err := json.Unmarshal([]byte(rawText), &c); err != nil {
			return "", nil
		}
		if c.Type == "response.output_text.delta" {
			return c.Delta, nil
		}
		return "", nil

	// Anthropic
	case "anthropic":
		var c anthropicChunk
		if err := json.Unmarshal([]byte(rawText), &c); err != nil {
			return "", nil
		}
		if c.Type == "content_block_delta" {
			switch c.Delta.Type {
			case "text_delta":
				return c.Delta.Text, nil
			case "thinking_delta":
				return c.Delta.Thinking, nil
			}
		}
		return "", nil

	// Google (both use same format)
	case "google-ai", "vertex-ai":
		var c googleChunk
		if err := json.Unmarshal([]byte(rawText), &c); err != nil {
			return "", nil
		}
		if len(c.Candidates) > 0 && len(c.Candidates[0].Content.Parts) > 0 {
			return c.Candidates[0].Content.Parts[0].Text, nil
		}
		return "", nil

	// AWS Bedrock (Debug string format)
	case "aws-bedrock":
		var c bedrockChunk
		if err := json.Unmarshal([]byte(rawText), &c); err != nil {
			return "", nil
		}
		return extractBedrockFromDebug(c.Debug)

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
