//go:build integration

package mockllm

import (
	"encoding/json"
	"fmt"
)

// Provider interface for formatting LLM responses.
type Provider interface {
	// FormatChunk formats a content chunk as an SSE data line
	FormatChunk(content string, index int) string

	// FormatFinalChunk formats the final chunk with finish_reason: "stop"
	FormatFinalChunk(index int) string

	// FormatDone returns the final SSE event indicating stream completion
	FormatDone() string

	// FormatNonStreaming formats a complete non-streaming response
	FormatNonStreaming(content string) ([]byte, error)

	// ContentType returns the Content-Type header value
	ContentType(streaming bool) string
}

// OpenAIProvider implements the OpenAI Chat Completions API format.
type OpenAIProvider struct{}

func (p *OpenAIProvider) FormatChunk(content string, index int) string {
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-test-%d", index),
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": content,
				},
				"finish_reason": nil,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func (p *OpenAIProvider) FormatFinalChunk(index int) string {
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-test-%d", index),
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func (p *OpenAIProvider) FormatDone() string {
	return "data: [DONE]\n\n"
}

func (p *OpenAIProvider) FormatNonStreaming(content string) ([]byte, error) {
	response := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": len(content) / 4,
			"total_tokens":      10 + len(content)/4,
		},
	}
	return json.Marshal(response)
}

func (p *OpenAIProvider) ContentType(streaming bool) string {
	if streaming {
		return "text/event-stream"
	}
	return "application/json"
}

// GetProvider returns the provider implementation for the given name.
func GetProvider(name string) (Provider, error) {
	switch name {
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		return &OpenAIProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", name)
	}
}
