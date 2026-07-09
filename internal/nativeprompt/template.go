package nativeprompt

import "strings"

// BAML v0.223 jinja-runtime magic delimiters. The role helper wraps its marker
// with roleDelim on both sides; media objects wrap their marker with mediaDelim
// on both sides. After rendering, the output is split on these to reconstruct
// the structured RenderedPrompt (see lower.go). These are internal to the
// renderer — they never reach a provider — so reproducing BAML's exact literals
// is a faithfulness choice, not a correctness requirement (the split is
// symmetric with the emit either way).
//
// Source: BAML v0.223 engine/baml-lib/jinja-runtime/src/lib.rs
//
//	const MAGIC_CHAT_ROLE_DELIMITER: &str = "BAML_CHAT_ROLE_MAGIC_STRING_DELIMITER";
//	const MAGIC_MEDIA_DELIMITER: &str = "BAML_MEDIA_MAGIC_STRING_DELIMITER";
const (
	roleDelim  = "BAML_CHAT_ROLE_MAGIC_STRING_DELIMITER"
	mediaDelim = "BAML_MEDIA_MAGIC_STRING_DELIMITER"

	roleMarkerPrefix = ":baml-start-baml:"
	roleMarkerSuffix = ":baml-end-baml:"

	mediaMarkerPrefix = ":baml-start-media:"
	mediaMarkerSuffix = ":baml-end-media:"

	// allowDupeRoleKey is BAML's reserved role kwarg; it never becomes message
	// metadata and defaults to false.
	allowDupeRoleKey = "__baml_allow_dupe_role__"
	roleKey          = "role"
)

// rawDynamicPrompt is the prompt body of cmd/build/dynamic.baml's
// Baml_Rest_Dynamic function, verbatim between the #" ... "# delimiters
// (delimiters stripped, content otherwise untouched — exactly what
// bamlparser.ParseBytes retains as the prompt field's Raw value).
//
// A drift guard (template_test.go) parses the real cmd/build/dynamic.baml and
// asserts this constant still matches, so an upstream template change cannot
// silently invalidate the parity proof.
const rawDynamicPrompt = "\n" +
	"    {%- for m in messages %}\n" +
	"      {%- if m.metadata and m.metadata.cache_control %}{{ _.role(m.role, cache_control={\"type\": m.metadata.cache_control.cache_type}) }}{% else %}{{ _.role(m.role) }}{% endif -%}\n" +
	"      {%- if m.parts -%}\n" +
	"        {%- for p in m.parts -%}\n" +
	"          {%- if p.text %}{{ p.text }}{% elif p.img %}{{ p.img }}{% elif p.aud %}{{ p.aud }}{% elif p.doc %}{{ p.doc }}{% elif p.vid %}{{ p.vid }}{% elif p.output_format %}{{ ctx.output_format }}{% endif -%}\n" +
	"        {%- endfor -%}\n" +
	"      {%- elif m.content %}{{ m.content | replace(\"{output_format}\", ctx.output_format) }}{% endif -%}\n" +
	"    {%- endfor %}\n" +
	"  "

// dynamicTemplate is the dedented+trimmed template that BAML actually feeds to
// minijinja. Computed once from rawDynamicPrompt via BAML's preprocessing.
var dynamicTemplate = dedentTrim(rawDynamicPrompt)

// dedentTrim reproduces BAML's prompt preprocessing
// (engine/baml-lib/jinja-runtime/src/lib.rs):
//
//	let whitespace_length = template.split('\n')
//	    .filter(|line| !line.trim().is_empty())
//	    .map(|line| line.chars().take_while(|c| c.is_whitespace()).count())
//	    .min().unwrap_or(0);
//	let template = template.split('\n')
//	    .map(|line| line.chars().skip(whitespace_length).collect())
//	    .collect().join("\n");
//	let template = template.trim();
//
// i.e. dedent every line by the minimum leading-whitespace count of the
// non-blank lines, then trim the whole template. Rune-based, matching Rust's
// char iteration.
func dedentTrim(t string) string {
	lines := strings.Split(t, "\n")

	minWS := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue // blank lines do not constrain the dedent width
		}
		ws := leadingWhitespaceRunes(ln)
		if minWS == -1 || ws < minWS {
			minWS = ws
		}
	}
	if minWS < 0 {
		minWS = 0
	}

	for i, ln := range lines {
		r := []rune(ln)
		if len(r) > minWS {
			lines[i] = string(r[minWS:])
		} else {
			lines[i] = ""
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// leadingWhitespaceRunes counts leading Unicode-whitespace runes, matching
// Rust's char::is_whitespace().take_while count.
func leadingWhitespaceRunes(s string) int {
	n := 0
	for _, c := range s {
		if !isRustWhitespace(c) {
			break
		}
		n++
	}
	return n
}

// isRustWhitespace mirrors Rust's char::is_whitespace (Unicode White_Space).
// For the ASCII whitespace that appears in template indentation the two agree;
// the extra ranges are covered for completeness so a non-ASCII space in a line
// prefix would be dedented the same way BAML does it.
func isRustWhitespace(c rune) bool {
	switch c {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x85, 0xA0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007,
		0x2008, 0x2009, 0x200A, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
		return true
	default:
		return false
	}
}
