package debaml

import (
	"strings"
)

// stripJSONComments removes JSONish comments the way BAML's jsonish tokenizer does
// before extraction — a string-aware pre-pass that drops `/* ... */` block comments
// (INCLUDING an unterminated `/* ... EOF`) and `// ...` line comments (to the next
// newline, or EOF), OUTSIDE quoted strings, replacing each with a single space
// (preserving token separation). Comment markers INSIDE a string value are literal and
// kept verbatim (LIVE-CAPTURED: BAML keeps {"name":"a/*b*/c"} AND {'name':'a/*b*/c'}
// intact). A bare `/` (not `/*` or `//`) is not a comment and is written unchanged.
//
// BOTH quote delimiters are tracked, since BAML's JSONish accepts single-quoted keys and
// values: a comment marker inside a single-quoted value is content, not a comment. But a
// single quote opens a string only in a STRUCTURAL value/key/element START position (at
// the very start, or right after `{ [ , :`, whitespace-tolerant) — matching BAML, which
// tokenizes `'` as a string delimiter structurally, NOT as a bare apostrophe in prose or
// mid-bareword. So `x's /* c */ {..}` still strips the comment (the apostrophe in `x's`
// is literal), while `{'name':'a/*b*/c'}` preserves it. A double quote always opens
// (BAML's primary delimiter, unambiguous). Escapes are honoured for both quote types so
// `\'` / `\"` does not prematurely close.
//
// BAML strips comments in every non-string position — leading, between fields, before
// a close, trailing, and unterminated (streaming) — so this pre-pass runs on the raw
// text before both the final and streaming extractors, letting the existing
// extract/fix/coerce machinery reproduce BAML's comment recovery byte-exact WITHOUT a
// BAML fallback. It is a no-op for text with no out-of-string comment markers, so it
// never perturbs the non-comment corpus.
func stripJSONComments(s string) string {
	// Fast path: nothing to strip if there is no `/*` or `//` anywhere.
	if !strings.Contains(s, "/*") && !strings.Contains(s, "//") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	var strQuote byte // active string delimiter while inStr: '"' or '\''
	escaped := false
	var lastSig byte // last significant (non-whitespace) byte emitted OUTSIDE a string/comment
	i := 0
	for i < len(s) {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == strQuote:
				inStr = false
				lastSig = c
			}
			i++
			continue
		}
		if c == '"' {
			inStr, strQuote, escaped = true, '"', false
			b.WriteByte(c)
			i++
			continue
		}
		if c == '\'' && (lastSig == 0 || lastSig == '{' || lastSig == '[' || lastSig == ',' || lastSig == ':') {
			// Structural single-quote: opens a string only in a fresh value/key/element
			// position (see doc comment). Elsewhere it falls through as a literal byte.
			inStr, strQuote, escaped = true, '\'', false
			b.WriteByte(c)
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i < len(s) && !(s[i] == '*' && i+1 < len(s) && s[i+1] == '/') {
				i++
			}
			if i < len(s) { // consume the closing */
				i += 2
			}
			b.WriteByte(' ')
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			// Leave the newline (if any) for the next iteration to preserve line
			// structure; the comment body itself collapses to one space.
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			lastSig = c // track the last significant byte for structural single-quote gating
		}
		i++
	}
	return b.String()
}

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
// Single quotes are deliberately NOT treated as string delimiters — span
// detection is single-quote-BLIND, matching BAML's multi-json/prose grep,
// which has no string-state tracking and closes purely on { } [ ] counting
// (it runs BEFORE the fixing parser). So `{name:'Ada } Lovelace', age:36}`
// slices to `{name:'Ada }`, which the fixing pass then rejects as an
// unterminated single-quoted string and declines — the parity-safe outcome,
// since BAML greps that same prefix as one of several scored candidates and
// native must not claim the wider object on its own. A bare apostrophe in
// prose ("Here's the answer: {...}") is harmless under quote-blindness (an
// apostrophe is not a bracket).
//
// Double-quoted content stays opaque so the anchor search skips a literal
// `"{}"` in prose, and a quoted brace inside a value does not change depth.
// Only the outer bracket type is depth-counted; inner brackets of the other
// type are balanced by construction in any candidate that later decodes, and
// a malformed nesting is caught by the decode step. Span selection stays
// free of repair logic so the same candidate decoder (strict then fix)
// serves the whole, markdown, and prose paths.
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
			// Anchor on the first opening bracket outside double-quoted content.
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

// --- M4b: streaming (raw_is_done=false) candidate extraction --------------
//
// streamExtractCandidate finds the JSON candidate in a raw prefix captured
// mid-generation and decodes it with streaming recovery, in the SAME
// BAML-recovery priority order as extractCandidate: the whole input (strict),
// then a markdown fenced block (open-to-close OR open-to-EOF while still
// streaming), then the first balanced-or-truncated object/array embedded in
// prose. A strict whole-input success is a fully-closed value whose children are
// all complete; every other path routes through decodeSpanStream, which recovers
// the incomplete structure via streamFix and tags per-value completion.
//
// The second return is false (DECLINE) when no candidate can be found — no
// JSON-looking content at all (bare prose / a just-opened fence with no body), or
// a construct outside the streaming fixing subset. Extraction NEVER claims a
// parse error: a claim only happens when a candidate is found AND stream
// coercion against the schema succeeds (handled by the caller).
func streamExtractCandidate(raw string) (value, bool) {
	// 1. Strict whole-input parse — a fully-closed, complete value.
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if v, err := strictDecode(trimmed); err == nil {
			return v, true
		}
	}

	// 2. Markdown fenced block: content between the opening fence and the closing
	//    fence, or — while still streaming — everything after the opening fence to
	//    EOF. Strict, then streaming fix, on the fence content.
	if content, ok := extractFenceContentStream(raw); ok {
		trimmed := strings.TrimSpace(content)
		// A completed object/array inside the fence may be followed only by a PARTIAL
		// closing fence (the ``` arriving one backtick at a time) plus whitespace. BAML
		// emits the completed object at those frames; native must too. Extract the
		// balanced span and, when the only remainder is whitespace/backticks, decode
		// the span — ignoring the partial closing fence. (The span scan is string-aware,
		// so backticks INSIDE a string value stay part of the span, not the remainder.)
		if span, end, closed, spanOk := extractBalancedSpanStream(trimmed); spanOk && closed {
			if strings.Trim(trimmed[end:], " \t\r\n`") == "" {
				return decodeSpanStream(span)
			}
		}
		return decodeSpanStream(trimmed)
	}

	// 3. First balanced-or-truncated object/array span embedded in prose.
	if span, end, closed, ok := extractBalancedSpanStream(raw); ok {
		if closed && containsUnquotedBracket(raw[end:]) {
			// A second top-level structure follows the first CLOSED span: BAML's
			// multiple-values / inferred-array case (scored), which M4b defers — so
			// decline rather than claim the first span.
			return value{}, false
		}
		// NO TrimSpace: the span starts at the opening bracket (never leading whitespace)
		// and an UNTERMINATED span runs to EOF, so trailing whitespace may be INSIDE an
		// open string (e.g. `{"a":"café ` streaming) — trimming it would drop string
		// content BAML keeps. Inter-token whitespace is handled by the fixer.
		return decodeSpanStream(span)
	}

	return value{}, false
}

// decodeSpanStream decodes a selected candidate span with streaming recovery:
// strict first (a fully-closed span whose values are all complete), then the
// streaming fixing pass (which recovers open objects/arrays/strings, drops
// still-streaming keys, and tags per-value completion). A span the streaming
// subset declines returns false (fall back to BAML).
func decodeSpanStream(span string) (value, bool) {
	if v, err := strictDecode(span); err == nil {
		return v, true
	}
	return streamFix(span)
}

// extractFenceContentStream is the streaming variant of extractFenceContent: it
// returns the body between the first opening ``` fence line and the next closing
// fence line, OR — when no closing fence has arrived yet (still streaming) —
// everything after the opening fence line to EOF. The second return is false when
// no opening fence line is present. Both fences are line-anchored, exactly as in
// the final extractor, so an inline ``` inside a string value does not truncate.
func extractFenceContentStream(raw string) (string, bool) {
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
	// No closing fence yet: take everything after the opening fence line to EOF.
	return strings.Join(lines[open+1:], "\n"), true
}

// extractBalancedSpanStream is the streaming variant of extractBalancedSpan: it
// anchors on the first '{' or '[' outside double-quoted content and returns the
// span to its matching close (closed=true) OR, when the structure never closes
// (a truncated response BAML recovers at is_done=false), the span from the anchor
// to EOF (closed=false). end is the index just past a closed span (len(raw) for a
// truncated one) so the caller can scan the remainder for a second top-level
// candidate. ok is false only when no opening bracket is found at all. Like the
// final extractor, span detection is single-quote-blind and double-quote-aware.
func extractBalancedSpanStream(raw string) (span string, end int, closed bool, ok bool) {
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
				return raw[start : i+1], i + 1, true, true
			}
		}
	}
	if start < 0 {
		return "", 0, false, false // no anchor
	}
	// Unterminated: take the anchor to EOF (BAML closes it at is_done=false).
	return raw[start:], len(raw), false, true
}
