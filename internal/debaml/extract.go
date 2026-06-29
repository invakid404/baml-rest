package debaml

import (
	"strings"
)

// extractCandidate finds the JSON candidate in raw and decodes it, in
// BAML-recovery priority order: the whole input, then a markdown fenced
// block, then the first balanced object/array embedded in prose. Each
// stage runs a strict decode first and then, only if that fails, the
// conservative fixing pass on the SAME candidate span — mirroring BAML,
// whose markdown and multi-json/prose stages recurse into the selected
// span with fixes enabled.
//
// The second return is false (DECLINE) whenever no candidate can be
// cleanly claimed — caller maps it to ErrDeBAMLParseUnsupported (fall back
// to BAML). That covers: no JSON-looking content at all; a candidate that
// needs a repair outside the conservative M2a fixing subset; an opening
// bracket that never closes (unterminated — BAML closes it at EOF and
// recovers, which M2a defers); and multiple top-level values (BAML wraps
// them and scores, which M2a defers). Extraction NEVER claims a parse
// error: a claim only happens when a candidate is found AND coercion
// against the schema then fails in a way BAML would also fail (handled by
// the caller after this returns true).
func extractCandidate(raw string) (value, bool) {
	// 1. Strict whole-input parse.
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if v, err := strictDecode(trimmed); err == nil {
			return v, true
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
	if span, end, ok := extractBalancedSpan(raw); ok {
		// If another top-level structure follows the first span, this is
		// BAML's multiple-values / inferred-array case (it collects ALL
		// balanced objects and scores them, so a later one can win). M2a
		// defers that, so decline rather than claim the first span — which
		// would otherwise propagate a spurious mismatch.
		if containsUnquotedBracket(raw[end:]) {
			return value{}, false
		}
		return decodeSpan(span)
	}

	return value{}, false
}

// decodeSpan decodes a single selected candidate span: strict first, then
// the conservative fixing pass. A span the fixing subset declines returns
// false (fall back to BAML), never a claimed error — a span that merely
// needs an out-of-subset repair is exactly the fixing-parser case M2a
// defers.
func decodeSpan(span string) (value, bool) {
	if v, err := strictDecode(span); err == nil {
		return v, true
	}
	if v, err := fixParse(span); err == nil {
		return v, true
	}
	return value{}, false
}

// containsUnquotedBracket reports whether s contains a '{' or '[' outside
// double-quoted content (honouring backslash escapes). It is the
// second-top-level-candidate detector for extractCandidate: a bracket in
// the input remaining after the first balanced span means BAML would grep
// more than one structure, which M2a declines. Quote-awareness keeps a
// brace that only appears inside a string value from forcing a false
// decline.
func containsUnquotedBracket(s string) bool {
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
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
		switch c {
		case '"':
			inString = true
		case '{', '[':
			return true
		}
	}
	return false
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
// inside double-quoted strings (honouring backslash escapes). It also
// returns the index just past the closing bracket so the caller can scan
// the remainder for further top-level candidates. The ok return is false
// when no opening bracket is found or the structure never closes (a
// truncated response), which the caller treats as "no candidate".
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
func extractBalancedSpan(raw string) (span string, end int, ok bool) {
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
				return raw[start : i+1], i + 1, true
			}
		}
	}
	return "", 0, false
}
