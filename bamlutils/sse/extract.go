package sse

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// DeltaParts contains the per-event split for a single SSE event.
//
// Parseable is text-only — reasoning/thinking content never enters it, so
// the BAML parser cannot be influenced by reasoning text.
//
// Raw is the wire-output text, also text-only by construction. It mirrors
// Parseable for the providers handled here; the field is kept separate so
// future provider extractors can surface text that should reach /with-raw
// but not Parse/ParseStream (e.g. citation footnotes) without conflating
// with reasoning.
//
// Reasoning carries provider-specific reasoning/thinking text — Anthropic
// thinking_delta, Gemini thought-tagged parts, OpenAI Chat Completions
// reasoning_content, OpenAI Responses reasoning_summary_text deltas — and
// is populated only when the caller opts in via IncludeReasoning on the
// IncrementalExtractor or the includeReasoning parameter on
// ExtractDeltaPartsFromText. Reasoning is never written into Raw under any
// flag value.
type DeltaParts struct {
	Parseable string
	Raw       string
	Reasoning string
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
// by extracting deltas from SSE chunks of the last call only.
// Earlier calls are ignored since they represent failed retry attempts.
//
// includeReasoning is the per-request opt-in flag for surfacing provider-
// specific reasoning text on the structured reasoning channel. It does
// NOT affect the returned string here — this helper concatenates the raw
// (text-only) field of each delta. Callers that want reasoning text
// should walk DeltaParts.Reasoning explicitly via ExtractDeltaPartsFromText.
func GetCurrentContent(data *StreamingData, includeReasoning bool) (string, error) {
	if len(data.Calls) == 0 {
		return "", nil
	}

	// Only use the last call - earlier calls are failed retries
	call := data.Calls[len(data.Calls)-1]

	var sb strings.Builder
	for _, chunk := range call.Chunks {
		delta, err := ExtractDeltaContent(call.Provider, chunk, includeReasoning)
		if err == nil && delta != "" {
			sb.WriteString(delta)
		}
	}

	return sb.String(), nil
}

// ExtractDeltaContent extracts the text delta from an SSE chunk based on provider.
// See ExtractDeltaPartsFromText for the includeReasoning semantics.
func ExtractDeltaContent(provider string, chunk SSEChunk, includeReasoning bool) (string, error) {
	rawText, err := chunk.Text()
	if err != nil {
		return "", fmt.Errorf("failed to get chunk text: %w", err)
	}
	return ExtractDeltaFromText(provider, rawText, includeReasoning)
}

// ExtractDeltaPartsFromText contains the provider-specific delta extraction
// logic, operating on the raw text string. It returns the per-event split
// across Parseable (text-only, fed to Parse/ParseStream), Raw (text-only,
// fed to /with-raw's raw channel), and Reasoning (provider-specific
// reasoning/thinking text, populated only under opt-in).
//
// includeReasoning is the per-request opt-in flag. When false (default),
// Reasoning is empty and reasoning events are dropped. When true,
// provider-specific reasoning text (Anthropic thinking_delta, Gemini
// thought-tagged parts, OpenAI Chat Completions reasoning_content, OpenAI
// Responses reasoning_summary_text deltas) populates Reasoning. Raw and
// Parseable are unaffected by this flag — both stay text-only by
// construction so neither the BAML parser nor the wire's raw channel can
// be influenced by reasoning text.
func ExtractDeltaPartsFromText(provider string, rawText string, includeReasoning bool) (DeltaParts, error) {
	switch provider {
	// OpenAI-compatible providers (Chat Completions API format)
	// Path: choices[0].delta.content (parseable+raw); choices[0].delta.reasoning_content
	// (reasoning only, opt-in). reasoning_content is the de-facto reasoning surface
	// for DeepSeek-R1 and several OAI-compat gateways; non-string values are
	// silently skipped, matching the forgiving streaming semantics.
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		delta := gjson.Get(rawText, "choices.0.delta.content").String()
		parts := DeltaParts{Parseable: delta, Raw: delta}
		if includeReasoning {
			reasoning := gjson.Get(rawText, "choices.0.delta.reasoning_content")
			if reasoning.Type == gjson.String {
				parts.Reasoning = reasoning.String()
			}
		}
		return parts, nil

	// OpenAI Responses API (different format)
	// Path: delta (when type == "response.output_text.delta") for parseable+raw.
	// Path: delta (when type == "response.reasoning_summary_text.delta") for
	// reasoning only, opt-in. Surfaces the human-readable reasoning summary
	// OpenAI exposes for o1/o3-style models; the underlying chain-of-thought
	// is server-encrypted and intentionally not surfaced.
	case "openai-responses":
		switch gjson.Get(rawText, "type").String() {
		case "response.output_text.delta":
			delta := gjson.Get(rawText, "delta")
			if delta.Type != gjson.String {
				return DeltaParts{}, nil
			}
			text := delta.String()
			return DeltaParts{Parseable: text, Raw: text}, nil

		case "response.reasoning_summary_text.delta":
			if !includeReasoning {
				return DeltaParts{}, nil
			}
			delta := gjson.Get(rawText, "delta")
			if delta.Type != gjson.String {
				return DeltaParts{}, nil
			}
			return DeltaParts{Reasoning: delta.String()}, nil
		}
		return DeltaParts{}, nil

	// Anthropic
	// Path: delta.text (text_delta) or delta.thinking (thinking_delta)
	// when type == "content_block_delta". Thinking deltas populate
	// Reasoning only when includeReasoning is true; Parseable and Raw
	// always exclude thinking so neither the parser nor /with-raw's raw
	// channel sees reasoning text.
	case "anthropic":
		if gjson.Get(rawText, "type").String() == "content_block_delta" {
			switch gjson.Get(rawText, "delta.type").String() {
			case "text_delta":
				delta := gjson.Get(rawText, "delta.text").String()
				return DeltaParts{Parseable: delta, Raw: delta}, nil
			case "thinking_delta":
				if !includeReasoning {
					return DeltaParts{}, nil
				}
				return DeltaParts{Reasoning: gjson.Get(rawText, "delta.thinking").String()}, nil
			}
		}
		return DeltaParts{}, nil

	// Google (both use same format)
	// Path: candidates[0].content.parts[]
	// Iterate all parts. Non-thought string text writes to Parseable and Raw;
	// thought-tagged parts never enter Parseable or Raw (matching upstream
	// BAML's text_content_part filter at
	// engine/baml-runtime/src/internal/llm_client/primitive/google/response_handler.rs)
	// and populate Reasoning only when includeReasoning is on. Missing/non-
	// array parts yields an empty delta with no error, and non-string text
	// on any part is silently skipped — preserving the forgiving streaming
	// semantics.
	case "google-ai", "vertex-ai":
		parts := gjson.Get(rawText, "candidates.0.content.parts")
		var parseableSB strings.Builder
		var rawSB strings.Builder
		var reasoningSB strings.Builder
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				text := part.Get("text")
				if text.Type != gjson.String {
					return true
				}
				if part.Get("thought").Bool() {
					if includeReasoning {
						reasoningSB.WriteString(text.String())
					}
					return true
				}
				parseableSB.WriteString(text.String())
				rawSB.WriteString(text.String())
				return true
			})
		}
		return DeltaParts{
			Parseable: parseableSB.String(),
			Raw:       rawSB.String(),
			Reasoning: reasoningSB.String(),
		}, nil

	// AWS Bedrock (Debug string format)
	// Path: debug (then parsed via regex)
	case "aws-bedrock":
		delta, err := extractBedrockFromDebug(gjson.Get(rawText, "debug").String())
		if err != nil {
			return DeltaParts{}, err
		}
		return DeltaParts{Parseable: delta, Raw: delta}, nil

	default:
		return DeltaParts{}, fmt.Errorf("unsupported provider: %s", provider)
	}
}

// ExtractDeltaFromText contains the provider-specific delta extraction logic,
// operating on the raw text string. It takes a provider name and the raw JSON
// text of a single SSE event, and returns the raw textual delta content.
//
// includeReasoning is the per-request opt-in flag for the structured
// reasoning channel. See ExtractDeltaPartsFromText for full semantics.
// This helper returns DeltaParts.Raw, which is text-only regardless of the
// flag — callers that need reasoning text should use
// ExtractDeltaPartsFromText directly.
func ExtractDeltaFromText(provider string, rawText string, includeReasoning bool) (string, error) {
	delta, err := ExtractDeltaPartsFromText(provider, rawText, includeReasoning)
	if err != nil {
		return "", err
	}
	return delta.Raw, nil
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

// IsDeltaProviderSupported returns true if the given provider string is
// handled by ExtractDeltaFromText. Callers that resolve providers from
// FunctionLog calls can use this to detect meta-providers like
// "baml-fallback" and walk a precedence ladder to resolve the actual child
// provider. The generated processTick closure uses:
//  1. Backward scan of funcLog.Calls() for a supported runtime-reported
//     provider (handles the common fallback case).
//  2. Runtime OriginalClientRegistry().Clients lookup by ClientName
//     (handles per-request client_registry provider overrides).
//  3. Static introspected.ClientProvider[ClientName] — only if it returns
//     a supported provider.
//  4. Otherwise the caller should leave the provider unchanged so
//     ExtractDeltaFromText fails its own unsupported-provider check
//     rather than using a stale value.
func IsDeltaProviderSupported(provider string) bool {
	switch provider {
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter",
		"openai-responses",
		"anthropic",
		"google-ai", "vertex-ai",
		"aws-bedrock":
		return true
	default:
		return false
	}
}

// IncrementalExtractor extracts SSE content incrementally, tracking which chunks
// have already been processed to avoid re-parsing on each tick.
//
// Three buffers are maintained side-by-side:
//
//   - parseable accumulates text-only deltas (from DeltaParts.Parseable).
//     This is what BAML's parser sees via ParseStream — reasoning/thinking
//     content never enters this buffer regardless of the opt-in flag,
//     so a malformed JSON-ish thinking fragment cannot influence parsing.
//   - raw accumulates wire-output deltas (from DeltaParts.Raw). Also
//     text-only by construction; mirrors parseable for the providers
//     handled here.
//   - reasoning accumulates provider-specific reasoning text (from
//     DeltaParts.Reasoning). Populated only when IncludeReasoning is
//     enabled; otherwise stays empty.
type IncrementalExtractor struct {
	// callCount tracks the number of calls seen (to detect retries)
	callCount int
	// cursor tracks chunks processed in the current (last) call
	cursor int
	// parseable accumulates text-only deltas; fed to ParseStream so the
	// BAML parser cannot be influenced by reasoning content.
	parseable strings.Builder
	// raw accumulates the wire-output deltas (text-only); fed to
	// /with-raw's raw channel.
	raw strings.Builder
	// reasoning accumulates provider-specific reasoning text; fed to
	// /with-raw's reasoning channel when IncludeReasoning is enabled.
	reasoning strings.Builder
	// includeReasoning mirrors the per-request opt-in for surfacing
	// provider-specific reasoning text. False (default) leaves the
	// reasoning buffer empty; true accumulates reasoning into its own
	// buffer alongside parseable/raw.
	includeReasoning bool
}

// NewIncrementalExtractor creates a new incremental extractor. Pass
// includeReasoning=true to opt the extractor into accumulating provider-
// specific reasoning text (Anthropic thinking_delta, Gemini thought-tagged
// parts, OpenAI Chat Completions reasoning_content, OpenAI Responses
// reasoning_summary_text deltas) into the reasoning buffer; pass false
// (default) to leave the reasoning buffer empty. The parseable and raw
// buffers are text-only regardless of this flag.
func NewIncrementalExtractor(includeReasoning bool) *IncrementalExtractor {
	return &IncrementalExtractor{includeReasoning: includeReasoning}
}

// ExtractResult contains the result of an incremental extraction. Three
// parallel pairs of (Delta, Full) values are returned, one for each
// internal buffer:
//
//   - ParseableDelta / ParseableFull: text-only content. Feed ParseStream
//     so reasoning/thinking content cannot influence the BAML parser.
//   - RawDelta / RawFull: wire-output text-only content; mirrors
//     parseable for the providers handled here.
//   - ReasoningDelta / ReasoningFull: provider-specific reasoning text.
//     Populated only when IncludeReasoning is enabled.
//
// Reset signals that the consumer should discard accumulated state — this
// is set when a retry rebuild has occurred (chunks decreased or callCount
// changed).
type ExtractResult struct {
	// ParseableDelta is the new parseable (text-only) content this tick.
	ParseableDelta string
	// ParseableFull is the cumulative parseable (text-only) content.
	ParseableFull string
	// RawDelta is the new raw (text-only) content this tick.
	RawDelta string
	// RawFull is the cumulative raw (text-only) content.
	RawFull string
	// ReasoningDelta is the new reasoning content this tick. Empty when
	// IncludeReasoning is disabled.
	ReasoningDelta string
	// ReasoningFull is the cumulative reasoning content. Empty when
	// IncludeReasoning is disabled.
	ReasoningFull string
	// Reset is true if the client should discard accumulated state (retry
	// occurred). This is NOT set on first extraction — only when a retry
	// causes a rebuild.
	Reset bool
}

// Extract processes new SSE chunks incrementally, returning the parseable,
// raw, and reasoning deltas plus their cumulative buffers.
//
// Parameters:
//   - callCount: total number of calls in the FunctionLog (used to detect retries)
//   - provider: the LLM provider for the current (last) call
//   - chunks: SSE chunks from the current (last) call only
//
// A full rebuild occurs if:
//   - First extraction (initial state)
//   - Call count changed (retry added a new call)
//   - Chunk count decreased (shouldn't happen normally)
//
// Extract delegates to ExtractFrom[SSEChunk] so the logic is defined once.
func (e *IncrementalExtractor) Extract(callCount int, provider string, chunks []SSEChunk) ExtractResult {
	return ExtractFrom(e, callCount, provider, chunks)
}

// Clear resets the extractor state.
func (e *IncrementalExtractor) Clear() {
	e.callCount = 0
	e.cursor = 0
	e.parseable.Reset()
	e.raw.Reset()
	e.reasoning.Reset()
}

// ExtractFrom is like Extract but accepts a concrete slice type via generics,
// avoiding the overhead of boxing each element into an []SSEChunk interface slice.
// The caller can pass the concrete chunk slice directly (e.g., from the BAML FFI layer)
// without first copying it into []SSEChunk.
//
// Internally it calls chunk.Text() on the concrete type and passes the raw
// string to ExtractDeltaPartsFromText so the parseable/raw/reasoning split
// is preserved per-event, then accumulates each part into its own buffer.
func ExtractFrom[T SSEChunk](e *IncrementalExtractor, callCount int, provider string, chunks []T) ExtractResult {
	if callCount == 0 {
		return ExtractResult{}
	}

	isFirstExtraction := e.callCount == 0
	isRetry := !isFirstExtraction && callCount != e.callCount
	chunksDecreased := !isFirstExtraction && len(chunks) < e.cursor

	needsRebuild := isFirstExtraction || isRetry || chunksDecreased

	if needsRebuild {
		e.callCount = callCount
		e.cursor = 0
		e.parseable.Reset()
		e.raw.Reset()
		e.reasoning.Reset()

		for _, chunk := range chunks {
			rawText, err := chunk.Text()
			if err != nil {
				continue
			}
			parts, err := ExtractDeltaPartsFromText(provider, rawText, e.includeReasoning)
			if err != nil {
				continue
			}
			if parts.Parseable != "" {
				e.parseable.WriteString(parts.Parseable)
			}
			if parts.Raw != "" {
				e.raw.WriteString(parts.Raw)
			}
			if parts.Reasoning != "" {
				e.reasoning.WriteString(parts.Reasoning)
			}
		}
		e.cursor = len(chunks)

		return ExtractResult{
			ParseableDelta: e.parseable.String(),
			ParseableFull:  e.parseable.String(),
			RawDelta:       e.raw.String(),
			RawFull:        e.raw.String(),
			ReasoningDelta: e.reasoning.String(),
			ReasoningFull:  e.reasoning.String(),
			Reset:          isRetry || chunksDecreased,
		}
	}

	// Incremental: only process new chunks
	var parseableDeltaBuf strings.Builder
	var rawDeltaBuf strings.Builder
	var reasoningDeltaBuf strings.Builder
	for i := e.cursor; i < len(chunks); i++ {
		rawText, err := chunks[i].Text()
		if err != nil {
			continue
		}
		parts, err := ExtractDeltaPartsFromText(provider, rawText, e.includeReasoning)
		if err != nil {
			continue
		}
		if parts.Parseable != "" {
			parseableDeltaBuf.WriteString(parts.Parseable)
			e.parseable.WriteString(parts.Parseable)
		}
		if parts.Raw != "" {
			rawDeltaBuf.WriteString(parts.Raw)
			e.raw.WriteString(parts.Raw)
		}
		if parts.Reasoning != "" {
			reasoningDeltaBuf.WriteString(parts.Reasoning)
			e.reasoning.WriteString(parts.Reasoning)
		}
	}
	e.cursor = len(chunks)

	return ExtractResult{
		ParseableDelta: parseableDeltaBuf.String(),
		ParseableFull:  e.parseable.String(),
		RawDelta:       rawDeltaBuf.String(),
		RawFull:        e.raw.String(),
		ReasoningDelta: reasoningDeltaBuf.String(),
		ReasoningFull:  e.reasoning.String(),
		Reset:          false,
	}
}

// RawFull returns the complete accumulated raw (text-only) content
// without processing new data.
func (e *IncrementalExtractor) RawFull() string {
	return e.raw.String()
}

// ParseableFull returns the complete accumulated parseable (text-only)
// content without processing new data. Use this to feed ParseStream — the
// returned content never includes reasoning/thinking text.
func (e *IncrementalExtractor) ParseableFull() string {
	return e.parseable.String()
}

// ReasoningFull returns the complete accumulated reasoning content
// without processing new data. Empty when IncludeReasoning is disabled.
func (e *IncrementalExtractor) ReasoningFull() string {
	return e.reasoning.String()
}
