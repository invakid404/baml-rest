// Package buildrequest — response_extract.go provides provider-specific
// extraction of LLM output text from non-streaming JSON responses.
//
// This is the non-streaming counterpart to sse.ExtractDeltaFromText. While
// the streaming version extracts text deltas from individual SSE events, this
// function extracts the complete text from a single JSON response body.
package buildrequest

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cloudwego/gjson"
)

// ExtractResponseContent extracts the LLM output text from a non-streaming
// JSON response body. Each provider returns a different JSON structure for
// non-streaming completions.
//
// Returns three strings:
//   - parseable: text content fed to Parse.Method(). For all providers
//     this is text-only — reasoning/thinking content is never
//     accumulated into parseable so the BAML parser cannot be influenced
//     by reasoning text.
//   - raw: the text used for /call-with-raw's Raw() field. Also text-only
//     by construction; matches parseable for the providers handled here.
//   - reasoning: provider-specific reasoning text (Anthropic thinking
//     blocks, Gemini thought-tagged parts, OpenAI Chat Completions
//     reasoning_content, OpenAI Responses summary[].text entries from
//     output[].type == "reasoning" items). Populated only when
//     includeReasoning is true; empty otherwise.
//
// Returns an error for unsupported providers, empty bodies, invalid JSON,
// and when a non-empty response body does not contain the expected structure.
func ExtractResponseContent(provider string, responseBody string, includeReasoning bool) (parseable, raw, reasoning string, err error) {
	// An LLM provider should never return a valid 200 with an empty body.
	if strings.TrimSpace(responseBody) == "" {
		return "", "", "", fmt.Errorf("provider %s: empty response body", provider)
	}

	// Validate JSON before provider-specific extraction. gjson is lenient
	// and will return fields from truncated payloads or bodies with trailing
	// garbage, which could produce false-success on corrupted 200 responses.
	if !gjson.Valid(responseBody) {
		return "", "", "", fmt.Errorf("provider %s: response body is not valid JSON", provider)
	}

	// gjson.Get aliases responseBody for unescaped result substrings. The
	// copyingJoiner concatenates multi-segment array content through a
	// strings.Builder (owned output), but the scalar/single-field fast-paths —
	// OpenAI message.content and reasoning_content — return gjson's substring
	// view directly, so a returned string MAY alias responseBody. That is safe
	// for the owned-string inputs this entry point serves: the returned alias
	// keeps responseBody's backing array live and nothing here mutates it.
	// (The borrowed twin ExtractResponseContentBorrowed runs the same extractors
	// over pooled storage and re-states this aliasing as a Release-lifetime
	// constraint; the byte twin ExtractResponseContentBytes is the only
	// uniformly-owned one, because gjson.GetBytes copies every match out.)
	return dispatchResponseContent(provider, func(path string) gjson.Result {
		return gjson.Get(responseBody, path)
	}, includeReasoning, &copyingJoiner{})
}

// ExtractResponseContentBytes is the byte-oriented twin of
// ExtractResponseContent for the net/http unary path, where the response
// body is available as caller-owned bytes (llmhttp.Response.BodyBytes()).
// It validates and extracts via gjson.ValidBytes / gjson.GetBytes instead
// of converting the whole body to a string first, eliminating the
// whole-body []byte->string copy on the hot unary path.
//
// Safety: gjson.GetBytes views body as a string only for the duration of
// each call and copies the matched Raw/Str out before returning (see
// getBytes in cloudwego/gjson), so every gjson.Result handed back owns its
// data. The provider extractors further copy retained text into string
// builders. body must stay valid through the call; it need not survive
// afterward. Returns results identical to ExtractResponseContent for the
// same JSON — see TestExtractResponseContentBytesMatchesString.
func ExtractResponseContentBytes(provider string, body []byte, includeReasoning bool) (parseable, raw, reasoning string, err error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", "", "", fmt.Errorf("provider %s: empty response body", provider)
	}

	if !gjson.ValidBytes(body) {
		return "", "", "", fmt.Errorf("provider %s: response body is not valid JSON", provider)
	}

	return dispatchResponseContent(provider, func(path string) gjson.Result {
		return gjson.GetBytes(body, path)
	}, includeReasoning, &copyingJoiner{})
}

// dispatchResponseContent routes to the per-provider extractor. The get
// closure performs each top-level path lookup against the underlying JSON
// (string via gjson.Get, or bytes via gjson.GetBytes); nested navigation
// happens on the returned gjson.Result values, which are source-agnostic.
// This keeps the string, byte, and borrowed entry points sharing one
// implementation.
//
// pj is the accumulator the array/multi-block providers use to assemble
// parseable text. The string/byte entry points pass a copyingJoiner (always
// concatenate through strings.Builder → owned output); the borrowed entry
// point passes an aliasJoiner (single content block → return that block's
// aliasing view verbatim, zero copy). pj never changes the extracted VALUE,
// only whether a single-segment result aliases the source — see
// TestExtractResponseContentBorrowedMatchesString.
func dispatchResponseContent(provider string, get func(path string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	switch provider {
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		return extractOpenAIContent(provider, get, includeReasoning, pj)

	case "anthropic":
		return extractAnthropicContent(provider, get, includeReasoning, pj)

	case "openai-responses":
		return extractOpenAIResponsesContent(provider, get, includeReasoning, pj)

	case "google-ai", "vertex-ai":
		return extractGeminiContent(provider, get, includeReasoning, pj)

	case "aws-bedrock":
		// aws-bedrock non-streaming extractor for the Converse API.
		// v0.219-only (v0.204 / v0.215 fall through to the legacy path
		// because their codegen does not emit the BuildRequest call
		// branch). Current scope: default credential chain only (no
		// static `.baml` creds), no endpoint_url override, reasoning
		// block signature/redactedContent skipped (see #254).
		return extractAWSBedrockContent(provider, get, includeReasoning, pj)

	default:
		return "", "", "", fmt.Errorf("unsupported provider for non-streaming extraction: %s", provider)
	}
}

// extractOpenAIContent extracts text from an OpenAI Chat Completions
// non-streaming response.
//
// Returns:
//   - parseable: text content only — message.content (scalar string, array
//     text parts, or explicit null). Reasoning content never enters this
//     value.
//   - raw: same as parseable (text-only by construction).
//   - reasoning: message.reasoning_content when includeReasoning is true
//     (the de-facto reasoning surface for DeepSeek-R1 and several
//     OAI-compat gateways). Non-string reasoning_content is silently
//     skipped.
//
// Handles:
//   - Scalar string content (common case)
//   - Array content parts (multimodal / tool-use)
//   - Explicit null content (function-call-only responses)
//   - Refusal responses (message.refusal field or {"type":"refusal"} parts)
//
// Returns an error for refusals, missing/malformed content, and non-object
// array elements. reasoning_content is telemetry — its presence does not
// change content's strict error semantics.
func extractOpenAIContent(provider string, get func(string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	extractReasoning := func() string {
		if !includeReasoning {
			return ""
		}
		r := get("choices.0.message.reasoning_content")
		if r.Type != gjson.String {
			return ""
		}
		return r.String()
	}

	// Check for refusal first. OpenAI returns refusals either as a
	// top-level message.refusal field or as a content array part with
	// type == "refusal". The presence of the refusal field means the model
	// refused, regardless of its value (empty, non-string, etc.).
	refusal := get("choices.0.message.refusal")
	if refusal.Exists() && refusal.Type != gjson.Null {
		refusalText := "unknown reason"
		if refusal.Type == gjson.String && refusal.String() != "" {
			refusalText = refusal.String()
		}
		return "", "", "", fmt.Errorf("%s: model refused request: %s", provider, refusalText)
	}

	content := get("choices.0.message.content")

	// Scalar string — the common case
	if content.Type == gjson.String {
		text := content.String()
		return text, text, extractReasoning(), nil
	}

	// Array of content parts
	if content.IsArray() {
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
				pj.WriteString(textField.String())
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
			return "", "", "", iterErr
		}
		text := pj.String()
		return text, text, extractReasoning(), nil
	}

	// Explicitly null content (e.g. function-call-only responses) — valid.
	if content.Exists() && content.Type == gjson.Null {
		return "", "", extractReasoning(), nil
	}

	if content.Exists() {
		return "", "", "", fmt.Errorf("%s: unexpected content type in response (got %s)", provider, content.Type)
	}

	return "", "", "", fmt.Errorf("%s: could not extract text content from response (choices[0].message.content not found)", provider)
}

// extractAnthropicContent extracts text from an Anthropic Messages API
// non-streaming response.
//
// Returns:
//   - parseable: only "text" blocks (the final answer), suitable for
//     Parse.Method. Thinking blocks are never accumulated here.
//   - raw: same as parseable (text-only by construction).
//   - reasoning: "thinking" blocks' thinking text, only when
//     includeReasoning is true. Empty otherwise.
func extractAnthropicContent(provider string, get func(string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	contentArray := get("content")

	if contentArray.IsArray() {
		var reasoningSB strings.Builder
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
				pj.WriteString(textField.String())
			case "thinking":
				thinkField := value.Get("thinking")
				if thinkField.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for thinking block (got %s)", provider, thinkField.Type)
					return false
				}
				// Thinking populates reasoning only when the caller has
				// opted in; parseable and raw never receive thinking
				// content.
				if includeReasoning {
					reasoningSB.WriteString(thinkField.String())
				}
			}
			return true
		})
		if iterErr != nil {
			return "", "", "", iterErr
		}
		text := pj.String()
		return text, text, reasoningSB.String(), nil
	}

	return "", "", "", fmt.Errorf("%s: could not extract text content from response (content array not found)", provider)
}

// extractGeminiContent extracts text from a Google AI / Vertex AI (Gemini)
// non-streaming response.
//
// Returns:
//   - parseable: only non-thought string text. Thought-tagged parts never
//     enter parseable — aligned with upstream BAML's text_content_part
//     filter at
//     engine/baml-runtime/src/internal/llm_client/primitive/google/response_handler.rs.
//   - raw: same as parseable (text-only by construction).
//   - reasoning: thought-tagged parts' text, only when includeReasoning
//     is true. Empty otherwise.
//
// Non-thought parts retain the existing strict validation: missing parts,
// non-object array elements, and non-string text on a non-thought part
// remain errors. Thought parts are filtered, not validated — non-string
// text on a thought part is silently skipped, since the part contributes
// to neither parseable nor (validated) reasoning output.
func extractGeminiContent(provider string, get func(string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	parts := get("candidates.0.content.parts")

	if parts.IsArray() {
		var reasoningSB strings.Builder
		var iterErr error
		parts.ForEach(func(_, part gjson.Result) bool {
			// Reject non-object array elements
			if !part.IsObject() {
				iterErr = fmt.Errorf("%s: non-object element in parts array (got %s)", provider, part.Type)
				return false
			}
			// Thought parts are filtered, not validated. They never enter
			// parseable/raw; they populate reasoning only as opt-in string
			// text.
			if part.Get("thought").Bool() {
				if includeReasoning {
					text := part.Get("text")
					if text.Type == gjson.String {
						reasoningSB.WriteString(text.String())
					}
				}
				return true
			}
			text := part.Get("text")
			if text.Exists() {
				if text.Type != gjson.String {
					iterErr = fmt.Errorf("%s: unexpected type for part text field (got %s)", provider, text.Type)
					return false
				}
				pj.WriteString(text.String())
			}
			return true
		})
		if iterErr != nil {
			return "", "", "", iterErr
		}
		text := pj.String()
		return text, text, reasoningSB.String(), nil
	}

	return "", "", "", fmt.Errorf("%s: could not extract text content from response (candidates[0].content.parts not found)", provider)
}

// extractOpenAIResponsesContent extracts text from an OpenAI Responses API
// non-streaming response. The format differs from Chat Completions:
//
//	{
//	  "output": [
//	    {"type": "reasoning", "content": [], "summary": [
//	      {"type": "summary_text", "text": "..."}
//	    ], "encrypted_content": "..."},
//	    {"type": "message", "status": "completed", "content": [
//	      {"type": "output_text", "text": "The response text."}
//	    ], "role": "assistant"}
//	  ]
//	}
//
// Returns:
//   - parseable: assistant text from all output items with type == "message",
//     concatenated in array order. Reasoning surfaces never enter this
//     value.
//   - raw: same as parseable (text-only by construction).
//   - reasoning: when includeReasoning is true, reasoning items'
//     summary[].text entries are written into reasoning in array order.
//
// Responses API output ordering/count is model-dependent, so we walk the
// array in order rather than assume the first message item contains the
// whole answer.
//
// OpenAI's underlying reasoning chain-of-thought is server-encrypted for
// o1/o3-style models. Only the human-readable summary[].text entries are
// surfaced; reasoning.content[].text and encrypted_content are intentionally
// not surfaced.
//
// Reasoning summary shape is treated as optional telemetry: non-array
// summary, non-object entries, missing text, and non-string text are all
// silently skipped — they never produce errors. The strict no-message-item
// contract is preserved: a reasoning-only response (no message item) still
// errors regardless of the flag.
func extractOpenAIResponsesContent(provider string, get func(string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	output := get("output")
	if !output.IsArray() {
		return "", "", "", fmt.Errorf("%s: could not extract text content from response (output array not found)", provider)
	}

	// Validate ALL output elements are objects, then aggregate all message
	// items. We must not stop early because trailing output items still need
	// validation and may contain additional assistant text.
	var foundMessage bool
	var reasoningSB strings.Builder
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
		switch itemTypeField.String() {
		case "message":
			foundMessage = true
			contentArray := item.Get("content")
			if !contentArray.IsArray() {
				outputErr = fmt.Errorf("%s: message item has no content array", provider)
				return false
			}

			contentArray.ForEach(func(_, entry gjson.Result) bool {
				if !entry.IsObject() {
					outputErr = fmt.Errorf("%s: non-object element in message content array (got %s)", provider, entry.Type)
					return false
				}
				entryTypeField := entry.Get("type")
				if entryTypeField.Type != gjson.String || entryTypeField.String() == "" {
					outputErr = fmt.Errorf("%s: message content element missing required 'type' field", provider)
					return false
				}
				switch entryTypeField.String() {
				case "output_text":
					textField := entry.Get("text")
					if textField.Type != gjson.String {
						outputErr = fmt.Errorf("%s: unexpected type for output_text text field (got %s)", provider, textField.Type)
						return false
					}
					pj.WriteString(textField.String())
				case "refusal":
					refusalField := entry.Get("refusal")
					refusalText := "unknown reason"
					if refusalField.Type == gjson.String && refusalField.String() != "" {
						refusalText = refusalField.String()
					}
					outputErr = fmt.Errorf("%s: model refused request: %s", provider, refusalText)
					return false
				}
				return true
			})
			if outputErr != nil {
				return false
			}

		case "reasoning":
			if includeReasoning {
				appendOpenAIResponsesReasoningSummary(item, &reasoningSB)
			}
		}
		return true
	})

	if outputErr != nil {
		return "", "", "", outputErr
	}
	if !foundMessage {
		return "", "", "", fmt.Errorf("%s: no message item found in output array", provider)
	}

	text := pj.String()
	return text, text, reasoningSB.String(), nil
}

// extractAWSBedrockContent extracts text from an AWS Bedrock Converse
// non-streaming response. The Converse API shape is:
//
//	{
//	  "output": {
//	    "message": {
//	      "role": "assistant",
//	      "content": [
//	        {"text": "..."},
//	        {"reasoningContent": {"reasoningText": {"text": "...", "signature": "..."}}}
//	      ]
//	    }
//	  },
//	  "stopReason": "...",
//	  "usage": {...}
//	}
//
// Returns:
//   - parseable: text from `block.text` only — reasoning never enters
//     this value, mirroring every other provider's contract.
//   - raw: same as parseable (text-only by construction).
//   - reasoning: `block.reasoningContent.reasoningText.text` joined in
//     array order, only when includeReasoning is true.
//
// Strict on shape: missing output.message, non-array content, and a
// response with no text blocks AND no reasoning content surface as
// errors so a corrupted 200 cannot masquerade as an empty success.
//
// `signature` and `redactedContent` reasoning surfaces are
// intentionally skipped here — neither has an operator-actionable
// interpretation today; see #254 for the deferred design call.
// Tool-use blocks (toolUse, toolResult) are silently ignored — they do
// not contribute to any output, which preserves the strict
// no-empty-success contract because the extractor still errors when
// the message contains no recognised content blocks at all.
func extractAWSBedrockContent(provider string, get func(string) gjson.Result, includeReasoning bool, pj parseableJoiner) (parseable, raw, reasoning string, err error) {
	message := get("output.message")
	if !message.IsObject() {
		return "", "", "", fmt.Errorf("%s: could not extract text content from response (output.message not found)", provider)
	}
	contentArray := message.Get("content")
	if !contentArray.IsArray() {
		return "", "", "", fmt.Errorf("%s: output.message.content is not an array", provider)
	}

	var reasoningSB strings.Builder
	var iterErr error
	var sawAnyBlock bool
	contentArray.ForEach(func(_, block gjson.Result) bool {
		if !block.IsObject() {
			iterErr = fmt.Errorf("%s: non-object element in content array (got %s)", provider, block.Type)
			return false
		}
		// Bedrock Converse blocks are tagged by which key is present
		// (text, reasoningContent, toolUse, etc.), not by a `type`
		// discriminator. Match on key existence.
		if textField := block.Get("text"); textField.Exists() {
			if textField.Type != gjson.String {
				iterErr = fmt.Errorf("%s: unexpected type for text block (got %s)", provider, textField.Type)
				return false
			}
			pj.WriteString(textField.String())
			sawAnyBlock = true
			return true
		}
		if reasoningContent := block.Get("reasoningContent"); reasoningContent.IsObject() {
			sawAnyBlock = true
			if !includeReasoning {
				return true
			}
			// Only the human-readable reasoningText.text is surfaced;
			// signature / redactedContent (encrypted-summary variants)
			// are skipped — see #254.
			if rt := reasoningContent.Get("reasoningText"); rt.IsObject() {
				if t := rt.Get("text"); t.Type == gjson.String {
					reasoningSB.WriteString(t.String())
				}
			}
			return true
		}
		// toolUse / toolResult and other block shapes are silently
		// skipped. The strict no-recognised-content guard below still
		// errors if EVERY block is one of these — so a tool-only
		// Converse response surfaces as an error rather than an empty
		// success.
		return true
	})
	if iterErr != nil {
		return "", "", "", iterErr
	}
	if !sawAnyBlock {
		return "", "", "", fmt.Errorf("%s: output.message.content array has no recognised content blocks", provider)
	}
	text := pj.String()
	return text, text, reasoningSB.String(), nil
}

// appendOpenAIResponsesReasoningSummary writes the human-readable summary
// entries of an OpenAI Responses reasoning item to sb. The summary shape
// is treated as optional telemetry — non-array summary, non-object entries,
// missing text, and non-string text are silently skipped. reasoning.content
// and encrypted_content are NOT inspected; the underlying chain-of-thought
// is server-encrypted and intentionally not surfaced.
func appendOpenAIResponsesReasoningSummary(item gjson.Result, sb *strings.Builder) {
	summary := item.Get("summary")
	if !summary.IsArray() {
		return
	}
	summary.ForEach(func(_, entry gjson.Result) bool {
		text := entry.Get("text")
		if text.Type == gjson.String {
			sb.WriteString(text.String())
		}
		return true
	})
}
