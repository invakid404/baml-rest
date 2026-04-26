//go:build integration

package mockllm

import (
	"fmt"

	"github.com/goccy/go-json"
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

// AnthropicProvider implements the Anthropic Messages API streaming format.
type AnthropicProvider struct{}

func (p *AnthropicProvider) FormatChunk(content string, index int) string {
	// Anthropic uses event types for each SSE event
	if index == 0 {
		// First chunk: send message_start + content_block_start + first delta.
		// The prologue is emitted unconditionally even when content is empty —
		// the delta simply carries an empty "text" field in that case.
		start := `event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"msg-test","type":"message","role":"assistant","content":[],"model":"test-model"}}` + "\n\n"
		blockStart := `event: content_block_start` + "\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
		delta := map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{
				"type": "text_delta",
				"text": content,
			},
		}
		data, _ := json.Marshal(delta)
		return start + blockStart + fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", data)
	}
	delta := map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type": "text_delta",
			"text": content,
		},
	}
	data, _ := json.Marshal(delta)
	return fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", data)
}

func (p *AnthropicProvider) FormatFinalChunk(index int) string {
	return "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
}

func (p *AnthropicProvider) FormatDone() string {
	return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func (p *AnthropicProvider) FormatNonStreaming(content string) ([]byte, error) {
	return p.FormatNonStreamingWithThinking(content, "")
}

// FormatNonStreamingWithThinking emits the Anthropic non-streaming response
// shape with an optional leading "thinking" content block before the "text"
// block. When thinking is empty, the response is identical to
// FormatNonStreaming(content).
func (p *AnthropicProvider) FormatNonStreamingWithThinking(content, thinking string) ([]byte, error) {
	contentBlocks := make([]map[string]any, 0, 2)
	if thinking != "" {
		contentBlocks = append(contentBlocks, map[string]any{
			"type":     "thinking",
			"thinking": thinking,
		})
	}
	contentBlocks = append(contentBlocks, map[string]any{
		"type": "text",
		"text": content,
	})

	response := map[string]any{
		"id":          "msg-test",
		"type":        "message",
		"role":        "assistant",
		"model":       "test-model",
		"content":     contentBlocks,
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": (len(content) + len(thinking)) / 4,
		},
	}
	return json.Marshal(response)
}

func (p *AnthropicProvider) ContentType(streaming bool) string {
	if streaming {
		return "text/event-stream"
	}
	return "application/json"
}

// AnthropicMessageStart returns the message_start SSE event that opens an
// Anthropic streaming response. Used by streamAnthropicWithThinking which
// decouples the message prologue from FormatChunk so a thinking content
// block can be emitted before the text content block.
func (p *AnthropicProvider) AnthropicMessageStart() string {
	return `event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg-test","type":"message","role":"assistant","content":[],"model":"test-model"}}` + "\n\n"
}

// AnthropicBlockStart emits a content_block_start event for the given block
// type and content_block index. blockType is "text" or "thinking".
func (p *AnthropicProvider) AnthropicBlockStart(blockType string, blockIndex int) string {
	var emptyField string
	switch blockType {
	case "thinking":
		emptyField = `"thinking":""`
	default:
		emptyField = `"text":""`
	}
	return fmt.Sprintf(
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":%q,%s}}\n\n",
		blockIndex, blockType, emptyField,
	)
}

// AnthropicBlockDelta emits a content_block_delta event for the given delta
// type and content_block index. deltaType is "text_delta" or "thinking_delta",
// content is the delta payload (placed in the .text or .thinking field
// respectively).
func (p *AnthropicProvider) AnthropicBlockDelta(deltaType, content string, blockIndex int) string {
	var deltaPayload map[string]any
	switch deltaType {
	case "thinking_delta":
		deltaPayload = map[string]any{"type": "thinking_delta", "thinking": content}
	default:
		deltaPayload = map[string]any{"type": "text_delta", "text": content}
	}
	event := map[string]any{
		"type":  "content_block_delta",
		"index": blockIndex,
		"delta": deltaPayload,
	}
	data, _ := json.Marshal(event)
	return fmt.Sprintf("event: content_block_delta\ndata: %s\n\n", data)
}

// AnthropicBlockStop emits a content_block_stop event for the given index.
func (p *AnthropicProvider) AnthropicBlockStop(blockIndex int) string {
	return fmt.Sprintf(
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n",
		blockIndex,
	)
}

// AnthropicMessageStop returns the message_delta + message_stop terminator
// that closes an Anthropic streaming response. Equivalent to the existing
// FormatDone() output but exposed as a named primitive so the
// thinking-aware stream path can reuse it.
func (p *AnthropicProvider) AnthropicMessageStop() string {
	return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// GoogleAIProvider implements the Google AI (Gemini) streaming format.
type GoogleAIProvider struct{}

func (p *GoogleAIProvider) FormatChunk(content string, index int) string {
	chunk := map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": content},
					},
					"role": "model",
				},
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func (p *GoogleAIProvider) FormatFinalChunk(index int) string {
	chunk := map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": ""},
					},
					"role": "model",
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     10,
			"candidatesTokenCount": 5,
			"totalTokenCount":      15,
		},
	}
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func (p *GoogleAIProvider) FormatDone() string {
	// Google AI streams end after the last data chunk; no explicit done sentinel.
	// Returning "" causes WriteDone to write an empty string, which is a no-op
	// for the SSE protocol (blank line is just an event delimiter).
	return ""
}

func (p *GoogleAIProvider) FormatNonStreaming(content string) ([]byte, error) {
	response := map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": content},
					},
					"role": "model",
				},
				"finishReason": "STOP",
			},
		},
	}
	return json.Marshal(response)
}

func (p *GoogleAIProvider) ContentType(streaming bool) string {
	if streaming {
		return "text/event-stream"
	}
	return "application/json"
}

// OpenAIResponsesProvider implements the OpenAI Responses API streaming format.
type OpenAIResponsesProvider struct{}

func (p *OpenAIResponsesProvider) FormatChunk(content string, index int) string {
	chunk := map[string]any{
		"type":  "response.output_text.delta",
		"delta": content,
	}
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func (p *OpenAIResponsesProvider) FormatFinalChunk(index int) string {
	return "data: {\"type\":\"response.output_text.done\",\"text\":\"\"}\n\n"
}

func (p *OpenAIResponsesProvider) FormatDone() string {
	return "data: {\"type\":\"response.completed\"}\n\n"
}

func (p *OpenAIResponsesProvider) FormatNonStreaming(content string) ([]byte, error) {
	response := map[string]any{
		"id":     "resp-test",
		"object": "response",
		"output": []map[string]any{
			{
				"type": "message",
				"content": []map[string]any{
					{"type": "output_text", "text": content},
				},
			},
		},
		"status": "completed",
	}
	return json.Marshal(response)
}

func (p *OpenAIResponsesProvider) ContentType(streaming bool) string {
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
	case "openai-responses":
		return &OpenAIResponsesProvider{}, nil
	case "anthropic":
		return &AnthropicProvider{}, nil
	case "google-ai", "vertex-ai":
		return &GoogleAIProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", name)
	}
}
