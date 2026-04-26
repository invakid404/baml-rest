package sse

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// DeltaParts contains the parseable/raw split for a single SSE event.
// Parseable is always text-only — reasoning/thinking content never enters
// it, so the BAML parser cannot be influenced by reasoning text. Raw
// matches Parseable for providers without a reasoning surface; for
// Anthropic, raw additionally carries thinking_delta content when the
// caller opts in via IncludeThinkingInRaw on the IncrementalExtractor or
// the includeThinking parameter on ExtractDeltaPartsFromText.
type DeltaParts struct {
	Parseable string
	Raw       string
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
// includeThinking is the per-request opt-in flag for surfacing provider-
// specific reasoning content. False (default) yields BAML-aligned text-only
// content; true additionally accumulates Anthropic thinking_delta into the
// returned string.
func GetCurrentContent(data *StreamingData, includeThinking bool) (string, error) {
	if len(data.Calls) == 0 {
		return "", nil
	}

	// Only use the last call - earlier calls are failed retries
	call := data.Calls[len(data.Calls)-1]

	var sb strings.Builder
	for _, chunk := range call.Chunks {
		delta, err := ExtractDeltaContent(call.Provider, chunk, includeThinking)
		if err == nil && delta != "" {
			sb.WriteString(delta)
		}
	}

	return sb.String(), nil
}

// ExtractDeltaContent extracts the text delta from an SSE chunk based on provider.
// See ExtractDeltaPartsFromText for the includeThinking semantics.
func ExtractDeltaContent(provider string, chunk SSEChunk, includeThinking bool) (string, error) {
	rawText, err := chunk.Text()
	if err != nil {
		return "", fmt.Errorf("failed to get chunk text: %w", err)
	}
	return ExtractDeltaFromText(provider, rawText, includeThinking)
}

// ExtractDeltaPartsFromText contains the provider-specific delta extraction
// logic, operating on the raw text string. It returns both the parseable delta
// (used for Parse/ParseStream) and the raw delta (used for /with-raw output).
//
// includeThinking is the per-request opt-in flag for surfacing provider-
// specific reasoning content in Raw. When false (default), Parseable == Raw
// and Anthropic thinking_delta events are dropped — matching BAML's
// RawLLMResponse() text-only contract. When true, Anthropic thinking_delta
// events contribute to Raw only; Parseable is unaffected by construction so
// the BAML parser cannot be influenced by reasoning text.
func ExtractDeltaPartsFromText(provider string, rawText string, includeThinking bool) (DeltaParts, error) {
	switch provider {
	// OpenAI-compatible providers (Chat Completions API format)
	// Path: choices[0].delta.content
	case "openai", "openai-generic", "azure-openai", "ollama", "openrouter":
		delta := gjson.Get(rawText, "choices.0.delta.content").String()
		return DeltaParts{Parseable: delta, Raw: delta}, nil

	// OpenAI Responses API (different format)
	// Path: delta (when type == "response.output_text.delta")
	case "openai-responses":
		if gjson.Get(rawText, "type").String() == "response.output_text.delta" {
			delta := gjson.Get(rawText, "delta").String()
			return DeltaParts{Parseable: delta, Raw: delta}, nil
		}
		return DeltaParts{}, nil

	// Anthropic
	// Path: delta.text (text_delta) or delta.thinking (thinking_delta)
	// when type == "content_block_delta". Thinking deltas contribute to
	// Raw only when includeThinking is true; Parseable always excludes
	// thinking so the BAML parser sees text-only input.
	case "anthropic":
		if gjson.Get(rawText, "type").String() == "content_block_delta" {
			switch gjson.Get(rawText, "delta.type").String() {
			case "text_delta":
				delta := gjson.Get(rawText, "delta.text").String()
				return DeltaParts{Parseable: delta, Raw: delta}, nil
			case "thinking_delta":
				if !includeThinking {
					return DeltaParts{}, nil
				}
				return DeltaParts{Raw: gjson.Get(rawText, "delta.thinking").String()}, nil
			}
		}
		return DeltaParts{}, nil

	// Google (both use same format)
	// Path: candidates[0].content.parts[0].text
	case "google-ai", "vertex-ai":
		delta := gjson.Get(rawText, "candidates.0.content.parts.0.text").String()
		return DeltaParts{Parseable: delta, Raw: delta}, nil

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
// includeThinking is the per-request opt-in flag for surfacing provider-
// specific reasoning content. See ExtractDeltaPartsFromText for semantics.
//
// This function is used by both:
//   - The existing OnTick/IncrementalExtractor path (via ExtractFrom)
//   - Callers that want the raw textual contribution of an SSE event without
//     the parseable/raw split from ExtractDeltaPartsFromText
func ExtractDeltaFromText(provider string, rawText string, includeThinking bool) (string, error) {
	delta, err := ExtractDeltaPartsFromText(provider, rawText, includeThinking)
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
// Two buffers are maintained side-by-side so the parseable invariant holds
// uniformly across all consumers:
//
//   - parseable accumulates text-only deltas (from DeltaParts.Parseable).
//     This is what BAML's parser sees via ParseStream — reasoning/thinking
//     content never enters this buffer regardless of includeThinkingInRaw,
//     so a malformed JSON-ish thinking fragment cannot influence parsing.
//   - raw accumulates wire-output deltas (from DeltaParts.Raw). When
//     includeThinkingInRaw is true, this additionally carries Anthropic
//     thinking_delta content; otherwise it equals the parseable buffer
//     byte-for-byte.
//
// Under the default flag-off configuration the two buffers always hold
// identical content. Under opt-in they diverge whenever a non-text content
// surface (e.g., thinking_delta) is observed: parseable stays text-only,
// raw absorbs the additional content.
type IncrementalExtractor struct {
	// callCount tracks the number of calls seen (to detect retries)
	callCount int
	// cursor tracks chunks processed in the current (last) call
	cursor int
	// parseable accumulates text-only deltas; fed to ParseStream so the
	// BAML parser cannot be influenced by reasoning content even under
	// IncludeThinkingInRaw=true.
	parseable strings.Builder
	// raw accumulates the wire-output deltas; mirrors parseable when
	// includeThinkingInRaw is false, and additionally carries provider-
	// specific reasoning content when true.
	raw strings.Builder
	// includeThinkingInRaw mirrors the per-request opt-in for surfacing
	// provider-specific reasoning content. False (default) yields BAML-
	// aligned text-only output across both buffers; true additionally
	// accumulates Anthropic thinking_delta into raw only.
	includeThinkingInRaw bool
}

// NewIncrementalExtractor creates a new incremental extractor. Pass
// includeThinking=true to opt the extractor into accumulating Anthropic
// thinking_delta content alongside text deltas in the raw buffer; pass
// false (default) for BAML-aligned text-only output across both the
// parseable and raw buffers. The parseable buffer is text-only regardless
// of this flag — the gate only affects the raw buffer.
func NewIncrementalExtractor(includeThinking bool) *IncrementalExtractor {
	return &IncrementalExtractor{includeThinkingInRaw: includeThinking}
}

// ExtractResult contains the result of an incremental extraction. Two
// parallel pairs of (Delta, Full) values are returned, one for each
// internal buffer:
//
//   - ParseableDelta / ParseableFull: text-only content. Use these to feed
//     ParseStream so reasoning/thinking content cannot influence the
//     BAML parser.
//   - RawDelta / RawFull: wire-output content. Mirrors the parseable
//     values under default (flag-off) configuration; additionally carries
//     reasoning content when IncludeThinkingInRaw is enabled.
//
// Reset signals that the consumer should discard accumulated state — this
// is set when a retry rebuild has occurred (chunks decreased or callCount
// changed).
type ExtractResult struct {
	// ParseableDelta is the new parseable (text-only) content this tick.
	ParseableDelta string
	// ParseableFull is the cumulative parseable (text-only) content.
	ParseableFull string
	// RawDelta is the new raw content this tick (text + thinking when
	// IncludeThinkingInRaw=true; same as ParseableDelta otherwise).
	RawDelta string
	// RawFull is the cumulative raw content (text + thinking when
	// IncludeThinkingInRaw=true; same as ParseableFull otherwise).
	RawFull string
	// Reset is true if the client should discard accumulated state (retry
	// occurred). This is NOT set on first extraction — only when a retry
	// causes a rebuild.
	Reset bool
}

// Extract processes new SSE chunks incrementally, returning the parseable
// and raw deltas plus their cumulative buffers.
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
}

// ExtractFrom is like Extract but accepts a concrete slice type via generics,
// avoiding the overhead of boxing each element into an []SSEChunk interface slice.
// The caller can pass the concrete chunk slice directly (e.g., from the BAML FFI layer)
// without first copying it into []SSEChunk.
//
// Internally it calls chunk.Text() on the concrete type and passes the raw
// string to ExtractDeltaPartsFromText so the parseable/raw split is preserved
// per-event, then accumulates each part into its own buffer on the extractor.
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

		for _, chunk := range chunks {
			rawText, err := chunk.Text()
			if err != nil {
				continue
			}
			parts, err := ExtractDeltaPartsFromText(provider, rawText, e.includeThinkingInRaw)
			if err != nil {
				continue
			}
			if parts.Parseable != "" {
				e.parseable.WriteString(parts.Parseable)
			}
			if parts.Raw != "" {
				e.raw.WriteString(parts.Raw)
			}
		}
		e.cursor = len(chunks)

		return ExtractResult{
			ParseableDelta: e.parseable.String(),
			ParseableFull:  e.parseable.String(),
			RawDelta:       e.raw.String(),
			RawFull:        e.raw.String(),
			Reset:          isRetry || chunksDecreased,
		}
	}

	// Incremental: only process new chunks
	var parseableDeltaBuf strings.Builder
	var rawDeltaBuf strings.Builder
	for i := e.cursor; i < len(chunks); i++ {
		rawText, err := chunks[i].Text()
		if err != nil {
			continue
		}
		parts, err := ExtractDeltaPartsFromText(provider, rawText, e.includeThinkingInRaw)
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
	}
	e.cursor = len(chunks)

	return ExtractResult{
		ParseableDelta: parseableDeltaBuf.String(),
		ParseableFull:  e.parseable.String(),
		RawDelta:       rawDeltaBuf.String(),
		RawFull:        e.raw.String(),
		Reset:          false,
	}
}

// RawFull returns the complete accumulated raw content without processing
// new data. Under IncludeThinkingInRaw=true this includes thinking_delta
// content for Anthropic; otherwise it matches ParseableFull byte-for-byte.
func (e *IncrementalExtractor) RawFull() string {
	return e.raw.String()
}

// ParseableFull returns the complete accumulated parseable (text-only)
// content without processing new data. Use this to feed ParseStream — the
// returned content never includes reasoning/thinking text regardless of
// the IncludeThinkingInRaw flag.
func (e *IncrementalExtractor) ParseableFull() string {
	return e.parseable.String()
}
