package debaml

import (
	"strings"
)

// extractOutcome classifies the result of locating a JSON candidate in a
// raw model response.
type extractOutcome int

const (
	// extractParsed: a candidate was found and decoded (strict, or via the
	// conservative fixing pass); the value is returned.
	extractParsed extractOutcome = iota
	// extractNeedsFixing: a JSON-looking candidate was found but neither
	// strict decoding nor the conservative fixing subset could claim it
	// (e.g. comments, escapes, missing commas, unterminated structures).
	// The caller falls back to BAML's full fixing parser.
	extractNeedsFixing
	// extractNotFound: no JSON candidate is present at all (e.g. truncated
	// mid-value, or pure prose). The caller claims a parse error — BAML
	// errors here too.
	extractNotFound
)

// extractCandidate finds the JSON candidate in raw and decodes it, in
// BAML-recovery priority order: the whole input, then a markdown fenced
// block, then the first balanced object/array embedded in prose. Each
// stage runs a strict decode first and then, only if that fails, the
// conservative fixing pass on the SAME candidate span — mirroring BAML,
// whose markdown and multi-json/prose stages recurse into the selected
// span with fixes enabled.
//
// The first stage that yields a JSON-looking candidate decides the
// outcome: if that candidate decodes (strict or fixed) it is returned
// (extractParsed); if it is JSON-looking but neither strict nor fixable
// within the M2a subset the result is extractNeedsFixing (fall back to
// BAML). Extraction does NOT keep trying later, weaker stages once a
// candidate is found. Only when no stage finds any candidate is the result
// extractNotFound.
func extractCandidate(raw string) (value, extractOutcome) {
	// 1. Strict whole-input parse.
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if v, err := strictDecode(trimmed); err == nil {
			return v, extractParsed
		}
	}

	// 2. Markdown fenced block: strict, then conservative fix, on the fence
	//    content (BAML recurses into the fence with fixes enabled).
	if fence, ok := extractFenceContent(raw); ok {
		return decodeSpan(strings.TrimSpace(fence))
	}

	// 3. First balanced JSON object/array embedded in prose: strict, then
	//    conservative fix, on the span (BAML's multi-json grep recurses
	//    into the span with fixes enabled).
	if span, ok := extractBalancedSpan(raw); ok {
		return decodeSpan(span)
	}

	return value{}, extractNotFound
}

// decodeSpan decodes a single selected candidate span: strict first, then
// the conservative fixing pass. A span the fixing subset declines yields
// extractNeedsFixing (fall back to BAML), never a claimed error — a span
// that merely needs an out-of-subset repair is exactly the fixing-parser
// case M2a defers.
func decodeSpan(span string) (value, extractOutcome) {
	if v, err := strictDecode(span); err == nil {
		return v, extractParsed
	}
	if v, err := fixParse(span); err == nil {
		return v, extractParsed
	}
	return value{}, extractNeedsFixing
}

// extractFenceContent returns the body of the first ``` markdown fence in
// raw, where BOTH the opening and closing fences are anchored to the start
// of a line (after optional leading whitespace) — matching BAML's markdown
// parser, which line-anchors fences. The opening line's info string (e.g.
// ```json) is dropped. The body is every line between the opening and
// closing fence lines, joined by "\n".
//
// Line-anchoring the close is what lets a fenced JSON body itself contain
// ``` inside a string (e.g. {"msg":"x ``` y"}) without being truncated at
// the inline backticks: an inline fence is not at a line start, so it is
// not treated as the close. The second return is false when no opening
// fence line, or no later closing fence line, is present.
func extractFenceContent(raw string) (string, bool) {
	const fence = "```"
	lines := strings.Split(raw, "\n")
	open := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), fence) {
			open = i
			break
		}
	}
	if open < 0 {
		return "", false
	}
	for j := open + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimLeft(lines[j], " \t"), fence) {
			return strings.Join(lines[open+1:j], "\n"), true
		}
	}
	return "", false
}

// extractBalancedSpan returns the first balanced JSON object or array span
// in raw — from the first '{' or '[' that appears OUTSIDE double-quoted
// content to its matching close — skipping braces/brackets that appear
// inside double-quoted strings (honouring backslash escapes). The second
// return is false when no opening bracket is found or the structure never
// closes (a truncated response), which the caller treats as "no candidate".
//
// The opening-bracket search itself honours quote/escape state, so prose
// like `the literal "{}" then {"name":"Ada"}` anchors on the real object,
// not the quoted braces. A single pass tracks string state for both the
// anchor search (before start is set) and the depth count (after).
//
// Only the outer bracket type is depth-counted; inner brackets of the
// other type are balanced by construction in any candidate that later
// decodes, and a malformed nesting is caught by the decode step. Span
// selection stays free of repair logic so the same candidate decoder
// (strict then fix) serves the whole, markdown, and prose paths.
func extractBalancedSpan(raw string) (string, bool) {
	start := -1
	depth := 0
	var open, closing byte
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if start < 0 {
			// Anchor on the first opening bracket outside quoted content.
			switch c {
			case '{':
				start, open, closing, depth = i, '{', '}', 1
			case '[':
				start, open, closing, depth = i, '[', ']', 1
			}
			continue
		}
		switch c {
		case open:
			depth++
		case closing:
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return "", false
}
