package debaml

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// extractOutcome classifies the result of locating a JSON candidate in a
// raw model response.
type extractOutcome int

const (
	// extractParsed: a candidate was found and strict-parsed; the decoded
	// value is returned.
	extractParsed extractOutcome = iota
	// extractNeedsFixing: a JSON-looking candidate was found but is not
	// strict JSON (trailing commas, unquoted keys, single quotes, …). The
	// fixing parser is BAML's job in M1, so the caller falls back.
	extractNeedsFixing
	// extractNotFound: no complete JSON value is present (e.g. truncated
	// mid-value). The caller claims a parse error — BAML errors here too.
	extractNotFound
)

// extractCandidate finds the M1 JSON candidate in raw and strict-parses
// it, in BAML-recovery priority order: the whole input, then a markdown
// fenced block, then the first balanced object/array embedded in prose.
//
// The first stage that yields a JSON-looking candidate decides the
// outcome: if that candidate strict-parses it is returned (extractParsed);
// if it is JSON-looking but not strict JSON the result is
// extractNeedsFixing (fall back to BAML's fixing parser) — extraction does
// NOT keep trying later, weaker stages, because a present-but-unfixed
// candidate is exactly the fixing-parser case M1 defers. Only when no
// stage finds any candidate is the result extractNotFound.
func extractCandidate(raw string) (any, extractOutcome) {
	// 1. Strict whole-input parse.
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if v, err := strictUnmarshal(trimmed); err == nil {
			return v, extractParsed
		}
	}

	// 2. Markdown fenced block: strict JSON inside the fence.
	if fence, ok := extractFenceContent(raw); ok {
		if v, err := strictUnmarshal(strings.TrimSpace(fence)); err == nil {
			return v, extractParsed
		}
		return nil, extractNeedsFixing
	}

	// 3. First balanced JSON object/array embedded in prose.
	if span, ok := extractBalancedSpan(raw); ok {
		if v, err := strictUnmarshal(span); err == nil {
			return v, extractParsed
		}
		return nil, extractNeedsFixing
	}

	return nil, extractNotFound
}

// strictUnmarshal decodes s as a single strict JSON value using
// encoding/json, which rejects exactly the fixing-parser syntax M1 defers
// (trailing commas, unquoted keys, single quotes). UseNumber preserves the
// distinction between integers and floats so coercion can enforce
// conservative JSON-type matching. Trailing non-whitespace after the value
// is rejected so a "value + junk" string does not pass as strict JSON.
func strictUnmarshal(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// A second Decode must hit EOF: any remaining token is trailing data.
	var rest any
	if err := dec.Decode(&rest); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing data after JSON value")
		}
		return nil, fmt.Errorf("unexpected trailing data after JSON value: %w", err)
	}
	return v, nil
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
// strict-parses, and a malformed nesting is caught by the strict parse.
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
