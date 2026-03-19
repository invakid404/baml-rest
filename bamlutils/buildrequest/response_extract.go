// Package buildrequest — response_extract.go provides provider-specific
// extraction of LLM output text from non-streaming JSON responses.
//
// This is the non-streaming counterpart to sse.ExtractDeltaFromText. While
// the streaming version extracts text deltas from individual SSE events, this
// function extracts the complete text from a single JSON response body.
package buildrequest

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractResponseContent extracts the LLM output text from a non-streaming
// JSON response body. Each provider returns a different JSON structure for
// non-streaming completions.
//
// Returns two strings:
//   - parseable: the text that should be passed to Parse.Method(). For most
//     providers this is the full text content. For Anthropic with extended
//     thinking, this excludes thinking blocks (only text blocks).
//   - raw: the full text including thinking/reasoning content, used for
//     /call-with-raw's Raw() field. For providers without thinking support,
//     raw == parseable.
//
// Returns an error for unsupported providers, empty bodies, invalid JSON,
// and when a non-empty response body does not contain the expected structure.
func ExtractResponseContent(provider string, responseBody string) (parseable, raw string, err error) {
	// An LLM provider should never return a valid 200 with an empty body.
	if strings.TrimSpace(responseBody) == "" {
		return "", "", fmt.Errorf("provider %s: empty response body", provider)
	}

	// Validate JSON before provider-specific extraction. gjson is lenient
	// and will return fields from truncated payloads or bodies with trailing
	// garbage, which could produce false-success on corrupted 200 responses.
	if !gjson.Valid(responseBody) {
		return "", "", fmt.Errorf("provider %s: response body is not valid JSON", provider)
	}

	switch provider {
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		text, extractErr := extractOpenAIContent(provider, responseBody)
		return text, text, extractErr

	case "anthropic":
		return extractAnthropicContent(provider, responseBody)

	case "openai-responses":
		text, extractErr := extractOpenAIResponsesContent(provider, responseBody)
		return text, text, extractErr

	case "google-ai", "vertex-ai":
		text, extractErr := extractGeminiContent(provider, responseBody)
		return text, text, extractErr

	default:
		return "", "", fmt.Errorf("unsupported provider for non-streaming extraction: %s", provider)
	}
}

// extractOpenAIContent extracts text from an OpenAI Chat Completions
// non-streaming response.
//
// Handles:
//   - Scalar string content (common case)
//   - Array content parts (multimodal / tool-use)
//   - Explicit null content (function-call-only responses)
//   - Refusal responses (message.refusal field or {"type":"refusal"} parts)
//
// Returns an error for refusals, missing/malformed content, and non-object
// array elements.
func extractOpenAIContent(provider string, responseBody string) (string, error) {
	// Check for refusal first. OpenAI returns refusals either as a
	// top-level message.refusal field or as a content array part with
	// type == "refusal". The presence of the refusal field means the model
	// refused, regardless of its value (empty, non-string, etc.).
	refusal := gjson.Get(responseBody, "choices.0.message.refusal")
	if refusal.Exists() && refusal.Type != gjson.Null {
		refusalText := "unknown reason"
		if refusal.Type == gjson.String && refusal.String() != "" {
			refusalText = refusal.String()
		}
		return "", fmt.Errorf("%s: model refused request: %s", provider, refusalText)
	}

	content := gjson.Get(responseBody, "choices.0.message.content")

	// Scalar string — the common case
	if content.Type == gjson.String {
		return content.String(), nil
	}

	// Array of content parts
	if content.IsArray() {
		var sb strings.Builder
		var iterErr error
		content.ForEach(func(_, part gjson.Result) bool {
			// Reject non-object array elements
			if !part.IsObject() {
				iterErr = fmt.Errorf("%s: non-object element in content array (got %s)", provider, part.Type)
				return false
			}
			// Require a type discriminator on every content part
			partTypeField := part.Get("type")
			if partTypeField.Type != gjson.String || partTypeField.String() == "" {
				iterErr = fmt.Errorf("%s: content array element missing required 'type' field", provider)
				return false
			}
			switch partTypeField.String() {
			case "text":
				textField := part.Get("text")
				if textField.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for content part text field (got %s)", provider, textField.Type)
					return false
				}
				sb.WriteString(textField.String())
			case "refusal":
				// A refusal part is never a valid completion. Error
				// unconditionally — even if the refusal text is empty or
				// missing, the presence of type=="refusal" means the model
				// declined to respond.
				refusalField := part.Get("refusal")
				refusalText := "unknown reason"
				if refusalField.Type == gjson.String && refusalField.String() != "" {
					refusalText = refusalField.String()
				}
				iterErr = fmt.Errorf("%s: model refused request: %s", provider, refusalText)
				return false
			}
			// Other part types (image_url, etc.) are silently skipped
			return true
		})
		if iterErr != nil {
			return "", iterErr
		}
		return sb.String(), nil
	}

	// Explicitly null content (e.g. function-call-only responses) — valid.
	if content.Exists() && content.Type == gjson.Null {
		return "", nil
	}

	if content.Exists() {
		return "", fmt.Errorf("%s: unexpected content type in response (got %s)", provider, content.Type)
	}

	return "", fmt.Errorf("%s: could not extract text content from response (choices[0].message.content not found)", provider)
}

// extractAnthropicContent extracts text from an Anthropic Messages API
// non-streaming response.
//
// Returns two strings:
//   - parseable: only "text" blocks (the final answer), suitable for Parse.Method
//   - raw: "text" + "thinking" blocks, suitable for /call-with-raw's Raw()
//
// The streaming path accumulates both text_delta and thinking_delta into
// the raw stream, so the raw value here matches. But Parse.Method only
// expects the final answer text, not the reasoning trace.
func extractAnthropicContent(provider string, responseBody string) (parseable, raw string, err error) {
	contentArray := gjson.Get(responseBody, "content")

	if contentArray.IsArray() {
		var parseableSB strings.Builder
		var rawSB strings.Builder
		var iterErr error
		contentArray.ForEach(func(_, value gjson.Result) bool {
			// Reject non-object array elements
			if !value.IsObject() {
				iterErr = fmt.Errorf("%s: non-object element in content array (got %s)", provider, value.Type)
				return false
			}
			// Require a type discriminator on every content block
			blockTypeField := value.Get("type")
			if blockTypeField.Type != gjson.String || blockTypeField.String() == "" {
				iterErr = fmt.Errorf("%s: content array element missing required 'type' field", provider)
				return false
			}
			switch blockTypeField.String() {
			case "text":
				textField := value.Get("text")
				if textField.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for text block (got %s)", provider, textField.Type)
					return false
				}
				parseableSB.WriteString(textField.String())
				rawSB.WriteString(textField.String())
			case "thinking":
				thinkField := value.Get("thinking")
				if thinkField.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for thinking block (got %s)", provider, thinkField.Type)
					return false
				}
				// Thinking goes to raw only, not parseable
				rawSB.WriteString(thinkField.String())
			}
			return true
		})
		if iterErr != nil {
			return "", "", iterErr
		}
		return parseableSB.String(), rawSB.String(), nil
	}

	return "", "", fmt.Errorf("%s: could not extract text content from response (content array not found)", provider)
}

// extractGeminiContent extracts text from a Google AI / Vertex AI (Gemini)
// non-streaming response.
func extractGeminiContent(provider string, responseBody string) (string, error) {
	parts := gjson.Get(responseBody, "candidates.0.content.parts")

	if parts.IsArray() {
		var sb strings.Builder
		var iterErr error
		parts.ForEach(func(_, part gjson.Result) bool {
			// Reject non-object array elements
			if !part.IsObject() {
				iterErr = fmt.Errorf("%s: non-object element in parts array (got %s)", provider, part.Type)
				return false
			}
			text := part.Get("text")
			if text.Exists() {
				if text.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for part text field (got %s)", provider, text.Type)
					return false
				}
				sb.WriteString(text.String())
			}
			return true
		})
		if iterErr != nil {
			return "", iterErr
		}
		return sb.String(), nil
	}

	return "", fmt.Errorf("%s: could not extract text content from response (candidates[0].content.parts not found)", provider)
}

// extractOpenAIResponsesContent extracts text from an OpenAI Responses API
// non-streaming response. The format differs from Chat Completions:
//
//	{
//	  "output": [
//	    {"type": "reasoning", "content": [], "summary": []},
//	    {"type": "message", "status": "completed", "content": [
//	      {"type": "output_text", "text": "The response text."}
//	    ], "role": "assistant"}
//	  ]
//	}
//
// We find the first output item with type == "message" and concatenate all
// content entries with type == "output_text". Reasoning items are skipped
// (they are not part of the model's answer).
func extractOpenAIResponsesContent(provider string, responseBody string) (string, error) {
	output := gjson.Get(responseBody, "output")
	if !output.IsArray() {
		return "", fmt.Errorf("%s: could not extract text content from response (output array not found)", provider)
	}

	// Validate ALL output elements are objects, then find the first message
	// item. We must not stop early on the message item because trailing
	// non-object elements would go unvalidated.
	var messageItem gjson.Result
	var found bool
	var outputErr error
	output.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			outputErr = fmt.Errorf("%s: non-object element in output array (got %s)", provider, item.Type)
			return false
		}
		// Require a type discriminator on every output item
		itemTypeField := item.Get("type")
		if itemTypeField.Type != gjson.String || itemTypeField.String() == "" {
			outputErr = fmt.Errorf("%s: output array element missing required 'type' field", provider)
			return false
		}
		if !found && itemTypeField.String() == "message" {
			messageItem = item
			found = true
		}
		return true
	})

	if outputErr != nil {
		return "", outputErr
	}
	if !found {
		return "", fmt.Errorf("%s: no message item found in output array", provider)
	}

	contentArray := messageItem.Get("content")
	if !contentArray.IsArray() {
		return "", fmt.Errorf("%s: message item has no content array", provider)
	}

	var sb strings.Builder
	var iterErr error
	contentArray.ForEach(func(_, entry gjson.Result) bool {
		// Reject non-object array elements
		if !entry.IsObject() {
			iterErr = fmt.Errorf("%s: non-object element in message content array (got %s)", provider, entry.Type)
			return false
		}
		// Require a type discriminator on every content entry
		entryTypeField := entry.Get("type")
		if entryTypeField.Type != gjson.String || entryTypeField.String() == "" {
			iterErr = fmt.Errorf("%s: message content element missing required 'type' field", provider)
			return false
		}
		switch entryTypeField.String() {
		case "output_text":
			textField := entry.Get("text")
			if textField.Type != gjson.String {
				iterErr = fmt.Errorf("%s: unexpected type for output_text text field (got %s)", provider, textField.Type)
				return false
			}
			sb.WriteString(textField.String())
		case "refusal":
			// A refusal part means the model refused — never a valid
			// completion. Same pattern as Chat Completions refusal handling.
			refusalField := entry.Get("refusal")
			refusalText := "unknown reason"
			if refusalField.Type == gjson.String && refusalField.String() != "" {
				refusalText = refusalField.String()
			}
			iterErr = fmt.Errorf("%s: model refused request: %s", provider, refusalText)
			return false
		}
		return true
	})
	if iterErr != nil {
		return "", iterErr
	}

	return sb.String(), nil
}
