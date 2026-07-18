package debaml

import (
	"encoding/json"
	"errors"
	"strings"
)

// errFixUnsupported signals that the fixing parser met a construct outside
// the conservative M2a repair subset. The caller maps it to
// bamlutils.ErrDeBAMLParseUnsupported so the response falls back to BAML —
// preserving parity for repairs the native parser deliberately does not
// claim. The fixing parser NEVER returns any other error: it either
// produces a value it is confident BAML would produce, or it declines.
var errFixUnsupported = errors.New("debaml: fix: unsupported construct")

// fixParse runs the conservative fixing pass over a JSONish candidate span
// and returns an ordered value. It mirrors the subset of BAML's jsonish
// fixing parser that is safe to claim without diverging from BAML's chosen
// value:
//
//   - trailing / leading / repeated commas in objects and arrays,
//   - unquoted object keys,
//   - single-quoted object keys and string values,
//   - unquoted primitive values that BAML resolves to true/false/null or a
//     JSON number.
//
// Everything else — backtick / triple-quoted strings, escapes inside
// double-quoted strings, missing commas between elements, unquoted bareword
// string values, unterminated structures, and multiple top-level values —
// returns errFixUnsupported. BAML supports many of those, but they interact
// with completion flags, inferred arrays, and union scoring, so declining keeps
// the native-vs-BAML differential green via fallback. (JSONish COMMENTS are the
// exception since Phase 7C: a string-aware stripJSONComments pre-pass removes
// `/* */` and `//` comments outside strings before extraction, matching BAML, so
// the fixer never sees them and a comment-bearing input is CLAIMED, not declined.)
//
// The top level must be an object or array; a bare scalar span is declined
// (recovery schemas always target object/list/class shapes, and a strict
// scalar is already handled by the strict decode that runs first).
func fixParse(s string) (value, error) {
	p := &fixer{s: s}
	p.skipWS()
	if p.eof() {
		return value{}, errFixUnsupported
	}
	if c := p.s[p.pos]; c != '{' && c != '[' {
		return value{}, errFixUnsupported
	}
	v, err := p.parseValue("")
	if err != nil {
		return value{}, err
	}
	p.skipWS()
	if !p.eof() {
		// Trailing content after a single top-level value is the
		// multiple-values / inferred-array case BAML handles with scoring.
		return value{}, errFixUnsupported
	}
	return v, nil
}

// fixer is a single-pass cursor over the candidate span. It works on bytes:
// every byte it special-cases is ASCII, and UTF-8 continuation/lead bytes
// are all >= 0x80, so multi-byte runes pass through string content intact.
type fixer struct {
	s   string
	pos int
}

// Terminator sets for an unquoted scalar value, by enclosing collection.
const (
	objectValueTerm = ",}"
	arrayValueTerm  = ",]"
)

func (p *fixer) eof() bool { return p.pos >= len(p.s) }

// skipWS advances over ASCII whitespace.
func (p *fixer) skipWS() {
	for !p.eof() {
		switch p.s[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// parseValue parses one value. term is the terminator set used only when
// the value is an unquoted scalar (a bare token whose end is delimited by
// its enclosing collection); quoted strings, objects, and arrays self-
// delimit and ignore it.
func (p *fixer) parseValue(term string) (value, error) {
	p.skipWS()
	if p.eof() {
		return value{}, errFixUnsupported // unterminated value
	}
	switch c := p.s[p.pos]; c {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		s, err := p.parseDoubleQuoted()
		if err != nil {
			return value{}, err
		}
		return value{kind: valString, strV: s}, nil
	case '\'':
		s, err := p.parseSingleQuoted()
		if err != nil {
			return value{}, err
		}
		return value{kind: valString, strV: s}, nil
	case '`', '/', '#':
		// Backtick strings and comments are deferred to BAML.
		return value{}, errFixUnsupported
	default:
		return p.parseUnquotedScalar(term)
	}
}

// parseObject parses an object body, tolerating leading/trailing/repeated
// commas (BAML's object state ignores stray commas). The opening '{' is at
// the cursor.
func (p *fixer) parseObject() (value, error) {
	p.pos++ // consume '{'
	obj := value{kind: valObject}
	for {
		p.skipWS()
		if p.eof() {
			return value{}, errFixUnsupported // unterminated object
		}
		switch p.s[p.pos] {
		case '}':
			p.pos++
			return obj, nil
		case ',':
			p.pos++ // tolerate leading / trailing / repeated commas
			continue
		}
		key, err := p.parseKey()
		if err != nil {
			return value{}, err
		}
		p.skipWS()
		if p.eof() || p.s[p.pos] != ':' {
			return value{}, errFixUnsupported // missing key/value separator
		}
		p.pos++ // consume ':'
		v, err := p.parseValue(objectValueTerm)
		if err != nil {
			return value{}, err
		}
		obj.objV = append(obj.objV, field{key: key, val: v})

		// A value must be followed by ',' or '}' (after whitespace). Any
		// other token is a missing-comma / junk case BAML resolves with
		// its own heuristics — decline so it falls back.
		p.skipWS()
		if p.eof() {
			return value{}, errFixUnsupported
		}
		switch p.s[p.pos] {
		case '}':
			p.pos++
			return obj, nil
		case ',':
			p.pos++
		default:
			return value{}, errFixUnsupported
		}
	}
}

// parseArray parses an array body, tolerating leading/trailing/repeated
// commas. The opening '[' is at the cursor.
func (p *fixer) parseArray() (value, error) {
	p.pos++ // consume '['
	arr := value{kind: valArray, arrV: []value{}}
	for {
		p.skipWS()
		if p.eof() {
			return value{}, errFixUnsupported // unterminated array
		}
		switch p.s[p.pos] {
		case ']':
			p.pos++
			return arr, nil
		case ',':
			p.pos++
			continue
		}
		v, err := p.parseValue(arrayValueTerm)
		if err != nil {
			return value{}, err
		}
		arr.arrV = append(arr.arrV, v)

		p.skipWS()
		if p.eof() {
			return value{}, errFixUnsupported
		}
		switch p.s[p.pos] {
		case ']':
			p.pos++
			return arr, nil
		case ',':
			p.pos++
		default:
			return value{}, errFixUnsupported
		}
	}
}

// parseKey parses an object key: a double-quoted, single-quoted, or
// unquoted identifier. Backtick keys are declined.
func (p *fixer) parseKey() (string, error) {
	p.skipWS()
	if p.eof() {
		return "", errFixUnsupported
	}
	switch p.s[p.pos] {
	case '"':
		return p.parseDoubleQuoted()
	case '\'':
		return p.parseSingleQuoted()
	case '`':
		return "", errFixUnsupported
	default:
		return p.parseUnquotedKey()
	}
}

// keyBailBytes are characters that, inside an unquoted key, signal a
// construct outside the conservative subset (nested structure, a quoted
// run, a comma-bearing key, a backtick string). Reaching one before ':'
// declines the parse.
const keyBailBytes = "{}[],\"'`"

// parseUnquotedKey reads a bare key up to its ':' separator. The run stops
// at whitespace or ':'; the caller consumes the ':'. A structural byte
// inside the run, or no key text, declines the parse. Internal whitespace
// is therefore not part of the key — a multi-word bare key is declined
// rather than guessed.
func (p *fixer) parseUnquotedKey() (string, error) {
	start := p.pos
	for !p.eof() {
		c := p.s[p.pos]
		if c == ':' {
			break
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		if strings.IndexByte(keyBailBytes, c) >= 0 {
			return "", errFixUnsupported
		}
		p.pos++
	}
	key := strings.TrimSpace(p.s[start:p.pos])
	if key == "" {
		return "", errFixUnsupported
	}
	return key, nil
}

// scalarBailBytes are characters that, mid unquoted value, indicate a
// construct outside the subset: a nested/adjacent value or quoted run
// (missing comma), a stray colon, or a backtick string. The value-context
// terminators (',', '}' / ']') are checked before this set, so they break
// the run rather than bail.
const scalarBailBytes = "{}[]:\"'`"

// parseUnquotedScalar reads a bare scalar value and classifies it. The run
// is the maximal sequence of non-whitespace, non-terminator bytes. After
// it, whitespace is skipped and the next byte MUST be a terminator (so a
// trailing-whitespace value like `36 }` closes cleanly while a missing
// comma like `36 x` declines). Only true / false / null and strict JSON
// numbers are claimed; a bareword string is declined to BAML, whose
// unquoted-string boundary rules are broader than this subset.
//
// Parity guard for BAML's greedy unquoted-OBJECT-value tokenization: BAML's
// fixing parser closes an unquoted object value at a ',' only when the very
// next byte is a space or newline (json_parse_state.rs InObjectValue); for
// ANY other following byte — another field, a digit, a tab, or a '}' (a
// trailing comma) — it CONSUMES the comma and keeps reading, yielding a
// longer string value than native's clean split (e.g. `{"x":1,"y":2,}` ->
// BAML reads x as the string `1,"y":2,`, which then fails int coercion).
// Native cannot reproduce that greedy scan, so when an unquoted object value
// is followed by ',' + (not space/newline) it DECLINES. Arrays have no such
// behavior — InArray closes at ',' / ']' directly — so the guard is scoped
// to object-value context.
//
// The common flat `{"name":"Ada","age":36,}` (trailing comma before the close,
// no space) is CLAIMED, not declined: BAML greedily reads age as "36," then its
// int/float/bool coercer TRIMS the trailing comma back to 36, which equals
// native's clean read of `36` — the comma is followed by '}', so there is NO
// following field for the greedy scan to swallow, and the two paths converge on
// the same value. A comma followed by tight FIELD content (a genuine cascade,
// e.g. `{"x":1,"y":2}`) still declines — and admission (checkStreamRootSupported)
// excludes any schema with a NON-LAST unquoted-scalar field, so on the native
// stream lane only the LAST-scalar trailing-comma-before-close reaches here.
func (p *fixer) parseUnquotedScalar(term string) (value, error) {
	start := p.pos
	for !p.eof() {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		if strings.IndexByte(term, c) >= 0 {
			break
		}
		if strings.IndexByte(scalarBailBytes, c) >= 0 {
			return value{}, errFixUnsupported
		}
		p.pos++
	}
	raw := p.s[start:p.pos]
	if strings.TrimSpace(raw) == "" {
		return value{}, errFixUnsupported
	}
	p.skipWS()
	if p.eof() || strings.IndexByte(term, p.s[p.pos]) < 0 {
		// Unterminated, or followed by a non-terminator (missing comma).
		return value{}, errFixUnsupported
	}
	if term == objectValueTerm && p.s[p.pos] == ',' {
		next := p.pos + 1
		if next >= len(p.s) || (p.s[next] != ' ' && p.s[next] != '\n' && p.s[next] != '}') {
			// BAML would consume this comma and read greedily — decline. EXCEPTION: a
			// comma immediately followed by '}' is a TRAILING comma with NO following
			// field to greedy-swallow, so BAML's greedy read (`36,` then the int/float/
			// bool coercer trims the trailing comma back to 36) produces the SAME value
			// as native's clean read of `36` — CLAIM it. (A comma followed by tight
			// FIELD content is the genuine InObjectValue cascade, which stays declined;
			// admission also excludes any NON-last unquoted scalar, so only this LAST-
			// scalar trailing-comma reaches here.)
			return value{}, errFixUnsupported
		}
	}
	return classifyScalar(raw)
}

// classifyScalar resolves a trimmed bare token to a value, matching BAML's
// unquoted-string conversion for the claimed subset (true/false/null and
// numbers). Anything else — including numeric-looking forms that are not
// strict JSON numbers (".5", "1.", "0x10") — is declined so BAML's broader
// number/string handling decides it.
func classifyScalar(raw string) (value, error) {
	s := strings.TrimSpace(raw)
	switch s {
	case "true":
		return value{kind: valBool, boolV: true}, nil
	case "false":
		return value{kind: valBool, boolV: false}, nil
	case "null":
		return value{kind: valNull}, nil
	}
	// true/false/null are the only bare JSON literals; a bareword string
	// needs quotes, so a span that is valid standalone JSON here is a
	// number. json.Valid rejects the non-JSON numeric forms BAML would
	// still parse, so those decline rather than mis-claim.
	if json.Valid([]byte(s)) {
		return value{kind: valNumber, numV: json.Number(s)}, nil
	}
	return value{}, errFixUnsupported
}

// decodeDoubleQuoteEscape decodes the escape sequence at p.pos (which must be a
// backslash) inside a double-quoted string, appending the decoded content to sb
// and advancing p.pos past what it consumed. It mirrors BAML's jsonish escape set
// exactly (live-probed): only \" \\ \n \t \r \b \f decode to their character.
// Every other escape — \/, an unknown letter like \z, a \uXXXX sequence, or a
// backslash that runs to EOF — is kept LITERAL: the backslash is emitted as-is and
// the following byte (if any) is left for the normal scan. That reproduces what
// BAML emits (e.g. `\/`->`\/`, `\z`->`\z`, a dangling `\`->`\`), rather than the
// standard-JSON decoding, which BAML does not apply here.
func (p *fixer) decodeDoubleQuoteEscape(sb *strings.Builder) {
	if p.pos+1 >= len(p.s) {
		sb.WriteByte('\\') // dangling backslash at EOF -> literal
		p.pos++
		return
	}
	switch p.s[p.pos+1] {
	case '"':
		sb.WriteByte('"')
		p.pos += 2
	case '\\':
		sb.WriteByte('\\')
		p.pos += 2
	case 'n':
		sb.WriteByte('\n')
		p.pos += 2
	case 't':
		sb.WriteByte('\t')
		p.pos += 2
	case 'r':
		sb.WriteByte('\r')
		p.pos += 2
	case 'b':
		sb.WriteByte('\b')
		p.pos += 2
	case 'f':
		sb.WriteByte('\f')
		p.pos += 2
	default:
		// \/, \uXXXX and unknown escapes are literal in BAML: keep the
		// backslash and let the next byte be scanned normally.
		sb.WriteByte('\\')
		p.pos++
	}
}

// parseDoubleQuoted reads a double-quoted string, closing at the first
// unescaped quote. The opening '"' is at the cursor. Escape sequences are
// decoded per BAML's set (see decodeDoubleQuoteEscape); a triple-quoted opener
// and an unterminated string are still deferred, as is BAML's embedded-quote
// close heuristic (an unescaped mid-string quote closes here).
func (p *fixer) parseDoubleQuoted() (string, error) {
	if strings.HasPrefix(p.s[p.pos:], `"""`) {
		return "", errFixUnsupported // triple-quoted string deferred
	}
	p.pos++ // consume opening '"'
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		if c == '\\' {
			p.decodeDoubleQuoteEscape(&sb)
			continue
		}
		if c == '"' {
			p.pos++
			return sb.String(), nil
		}
		sb.WriteByte(c)
		p.pos++
	}
	return "", errFixUnsupported // unterminated string
}

// parseSingleQuoted reads a single-quoted string, closing at the first
// quote. The opening '\” is at the cursor. BAML does not process escapes
// inside single-quoted strings, but a backslash makes its embedded-quote
// close heuristic ambiguous, so it is declined. An unterminated string is
// declined.
func (p *fixer) parseSingleQuoted() (string, error) {
	p.pos++ // consume opening '\''
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		switch c {
		case '\\':
			return "", errFixUnsupported
		case '\'':
			p.pos++
			return sb.String(), nil
		}
		sb.WriteByte(c)
		p.pos++
	}
	return "", errFixUnsupported // unterminated string
}

// --- M4b: streaming (raw_is_done=false) fixing parser ---------------------
//
// streamFix runs the STREAMING variant of the conservative fixing pass over a
// candidate span captured mid-generation (raw_is_done=false). On top of the M2a
// fixing subset (trailing/leading/repeated commas, unquoted keys, single quotes,
// unquoted true/false/null/number scalars) it RECOVERS the incomplete structures
// BAML's jsonish closes at EOF and tags each value's CompletionState via the
// value.incomplete bit:
//
//   - an object/array with no closing delimiter is closed at EOF (Incomplete);
//   - a double/single-quoted string with no closing quote keeps its partial
//     content and is closed at EOF (Incomplete — allow_as_string under is_done=false);
//   - an unquoted scalar that runs to EOF is Incomplete;
//   - an object key with no ':' — or a ':' with no value — at EOF is DROPPED: the
//     field has no value yet, so the class null-fills it downstream (this is how
//     BAML's `{`, `{"name"`, and `{"name":` prefixes recover an all-null object);
//   - a value terminated by its proper delimiter (',' or a closing bracket) is
//     Complete, so the LAST child of an incomplete container is Incomplete while
//     earlier properly-terminated siblings stay Complete (semantic_streaming.rs).
//
// It DECLINES (ok=false) exactly the constructs the final fixer declines for
// reasons OTHER than truncation — comments, backtick/triple-quoted strings,
// escapes inside strings, and a missing comma between two PRESENT values (a
// non-EOF junk byte) — plus any trailing content after a self-closed top-level
// value (the multiple-values / inferred-array case). The top level must be an
// object or array (recovery schemas target object/list/class shapes); a bare
// scalar span declines. Completion is advisory only: stream coercion consults it
// to DECLINE an incomplete done-required value (which BAML would delete) rather
// than model the deletion (M4c). The final decoders never call this, so
// value.incomplete stays false on the final-parse path.
func streamFix(s string) (value, bool) {
	p := &fixer{s: s}
	p.skipWS()
	if p.eof() {
		return value{}, false
	}
	if c := p.s[p.pos]; c != '{' && c != '[' {
		// A bare scalar span is not a recovery target (and would be handled by the
		// strict decode that runs first for a complete one).
		return value{}, false
	}
	v, err := p.parseValueStream("")
	if err != nil {
		return value{}, false
	}
	p.skipWS()
	if !p.eof() {
		// Trailing content after a self-closed top-level value is the
		// multiple-values / inferred-array case BAML scores; decline. (An
		// unterminated top-level value consumed to EOF, so this only fires when the
		// value self-closed and more text follows.)
		return value{}, false
	}
	return v, true
}

// parseValueStream parses one streaming value. The caller guarantees a non-EOF,
// non-terminator byte at the cursor (object/array callers handle the EOF and
// terminator cases before dispatching here). term is the terminator set for a
// bare unquoted scalar; quoted strings, objects, and arrays self-delimit.
func (p *fixer) parseValueStream(term string) (value, error) {
	switch c := p.s[p.pos]; c {
	case '{':
		return p.parseObjectStream()
	case '[':
		return p.parseArrayStream()
	case '"':
		return p.parseDoubleQuotedStream()
	case '\'':
		return p.parseSingleQuotedStream()
	case '`', '/', '#':
		// Backtick strings and comments are deferred to BAML.
		return value{}, errFixUnsupported
	default:
		return p.parseUnquotedScalarStream(term)
	}
}

// parseObjectStream parses an object body with streaming recovery. The opening
// '{' is at the cursor. On EOF it closes the object Incomplete; a key with no
// ':' or a ':' with no value at EOF drops that (still-streaming) field so the
// class null-fills it. A missing comma / separator with more input present is
// out of the subset and declines.
func (p *fixer) parseObjectStream() (value, error) {
	p.pos++ // consume '{'
	obj := value{kind: valObject}
	for {
		p.skipWS()
		if p.eof() {
			obj.incomplete = true
			return obj, nil
		}
		switch p.s[p.pos] {
		case '}':
			p.pos++
			return obj, nil // complete (closed)
		case ',':
			p.pos++ // tolerate leading / trailing / repeated commas
			continue
		}
		key, ok := p.parseKeyStream()
		if !ok {
			// Unterminated / unparseable key → field has no value yet: drop it and
			// close the object Incomplete.
			obj.incomplete = true
			return obj, nil
		}
		p.skipWS()
		if p.eof() {
			// Key present but no ':' yet → drop the still-streaming field.
			obj.incomplete = true
			return obj, nil
		}
		if p.s[p.pos] != ':' {
			return value{}, errFixUnsupported // missing key/value separator
		}
		p.pos++ // consume ':'
		p.skipWS()
		if p.eof() {
			// ':' with no value yet → drop the still-streaming field.
			obj.incomplete = true
			return obj, nil
		}
		v, err := p.parseValueStream(objectValueTerm)
		if err != nil {
			return value{}, err
		}
		obj.objV = append(obj.objV, field{key: key, val: v})

		p.skipWS()
		if p.eof() {
			obj.incomplete = true
			return obj, nil
		}
		switch p.s[p.pos] {
		case '}':
			p.pos++
			return obj, nil
		case ',':
			p.pos++
		default:
			return value{}, errFixUnsupported // missing comma / junk
		}
	}
}

// parseArrayStream parses an array body with streaming recovery. The opening '['
// is at the cursor. On EOF it closes the array Incomplete; trailing/leading/
// repeated commas are tolerated. A missing comma with more input present declines.
func (p *fixer) parseArrayStream() (value, error) {
	p.pos++ // consume '['
	arr := value{kind: valArray, arrV: []value{}}
	for {
		p.skipWS()
		if p.eof() {
			arr.incomplete = true
			return arr, nil
		}
		switch p.s[p.pos] {
		case ']':
			p.pos++
			return arr, nil // complete
		case ',':
			p.pos++
			continue
		}
		v, err := p.parseValueStream(arrayValueTerm)
		if err != nil {
			return value{}, err
		}
		arr.arrV = append(arr.arrV, v)

		p.skipWS()
		if p.eof() {
			arr.incomplete = true
			return arr, nil
		}
		switch p.s[p.pos] {
		case ']':
			p.pos++
			return arr, nil
		case ',':
			p.pos++
		default:
			return value{}, errFixUnsupported
		}
	}
}

// parseKeyStream reads an object key for the streaming parser. It returns
// (key, true) for a fully-formed key, and ("", false) when the key is still
// streaming (an unterminated quoted key closed by EOF) or unparseable (backtick /
// empty) — the caller then drops the field and closes the object Incomplete.
func (p *fixer) parseKeyStream() (string, bool) {
	p.skipWS()
	if p.eof() {
		return "", false
	}
	switch p.s[p.pos] {
	case '"':
		v, err := p.parseDoubleQuotedStream()
		if err != nil || v.incomplete {
			return "", false
		}
		return v.strV, true
	case '\'':
		v, err := p.parseSingleQuotedStream()
		if err != nil || v.incomplete {
			return "", false
		}
		return v.strV, true
	case '`':
		return "", false
	default:
		key, err := p.parseUnquotedKey()
		if err != nil {
			return "", false
		}
		return key, true
	}
}

// parseDoubleQuotedStream reads a double-quoted string with streaming recovery.
// Escape sequences are decoded per BAML's set (see decodeDoubleQuoteEscape) so a
// partial string emits the same decoded content BAML streams (e.g. a dangling
// `"x\`->`x\`); a triple-quoted opener still declines; an unterminated string keeps
// its partial content and is marked Incomplete.
//
// An EMBEDDED quote (a `\"` escape decoding to a literal ") is a #583 deferred
// greedy-recovery trigger: BAML applies its embedded-quote CLOSE heuristic (greedily
// re-absorbing past the apparent close at incomplete-object prefixes), which native's
// clean standard-JSON close cannot reproduce byte-exact. Rather than emit a
// byte-DIFFERENT clean close, DECLINE the stream partial so native purely UNDER-emits /
// skips those frames — never a divergent emit (owner A′ / ledger #583). The FINAL of a
// complete, valid object decodes via strictDecode (not this fixer), so it is unaffected.
func (p *fixer) parseDoubleQuotedStream() (value, error) {
	if strings.HasPrefix(p.s[p.pos:], `"""`) {
		return value{}, errFixUnsupported // triple-quoted string deferred
	}
	p.pos++ // consume opening '"'
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		if c == '\\' {
			if p.pos+1 < len(p.s) && p.s[p.pos+1] == '"' {
				return value{}, errFixUnsupported // #583 embedded-quote close heuristic deferred
			}
			p.decodeDoubleQuoteEscape(&sb)
			continue
		}
		if c == '"' {
			p.pos++
			return value{kind: valString, strV: sb.String()}, nil // complete
		}
		sb.WriteByte(c)
		p.pos++
	}
	// Unterminated → keep partial content, mark Incomplete.
	return value{kind: valString, strV: sb.String(), incomplete: true}, nil
}

// parseSingleQuotedStream reads a single-quoted string with streaming recovery.
// A backslash declines (its embedded-quote close heuristic is ambiguous); an
// unterminated string keeps its partial content and is marked Incomplete.
func (p *fixer) parseSingleQuotedStream() (value, error) {
	p.pos++ // consume opening '\''
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		switch c {
		case '\\':
			return value{}, errFixUnsupported
		case '\'':
			p.pos++
			return value{kind: valString, strV: sb.String()}, nil // complete
		}
		sb.WriteByte(c)
		p.pos++
	}
	return value{kind: valString, strV: sb.String(), incomplete: true}, nil
}

// parseUnquotedScalarStream reads a bare scalar value with streaming recovery.
// It classifies the token like the final parser (true/false/null and strict JSON
// numbers only; a bareword string declines), but a token that runs to EOF is
// marked Incomplete rather than declined. The same greedy-object-value comma
// guard the final parser uses applies (native cannot reproduce BAML's greedy
// unquoted-object-value scan), and a value followed by a non-terminator non-EOF
// byte is a missing-comma case that declines.
func (p *fixer) parseUnquotedScalarStream(term string) (value, error) {
	start := p.pos
	for !p.eof() {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		if strings.IndexByte(term, c) >= 0 {
			break
		}
		if strings.IndexByte(scalarBailBytes, c) >= 0 {
			return value{}, errFixUnsupported
		}
		p.pos++
	}
	raw := p.s[start:p.pos]
	if strings.TrimSpace(raw) == "" {
		return value{}, errFixUnsupported
	}
	p.skipWS()
	if p.eof() {
		// Ran to EOF with no terminator → still streaming → Incomplete.
		v, err := classifyScalar(raw)
		if err != nil {
			return value{}, err
		}
		v.incomplete = true
		return v, nil
	}
	if strings.IndexByte(term, p.s[p.pos]) < 0 {
		return value{}, errFixUnsupported // missing comma / junk
	}
	if term == objectValueTerm && p.s[p.pos] == ',' {
		next := p.pos + 1
		if next < len(p.s) && p.s[next] != ' ' && p.s[next] != '\n' && p.s[next] != '}' {
			// Comma FOLLOWED BY tight (non-space/newline, non-close) content: BAML's
			// fixing parser CONSUMES the comma and keeps reading the unquoted object
			// value PAST it (json_parse_state.rs InObjectValue), a GREEDY CASCADE that
			// swallows every following field into one failed value — native's per-field
			// parse cannot reproduce it (#546), so it DECLINES. Admission additionally
			// declines any schema with a NON-LAST unquoted-scalar field, so no ADMITTED
			// stream can reach this prefix.
			return value{}, errFixUnsupported
		}
		// Comma at EOF of the streamed prefix (next >= len) OR immediately followed by
		// '}' (a TRAILING comma before the close, with no following field to greedy-
		// swallow): the value is cleanly comma-terminated → COMPLETE, matching BAML
		// parse-stream (LIVE-CAPTURED: {"a":1, -> a kept; {"name":"Ada","age":36,} ->
		// {name,age:36} — greedy-read "36," then int-trim == native's clean 36). This
		// keeps the partial cadence aligned with the FINAL parser's same '}' exception.
		// Fall through.
	}
	// Terminated by a proper delimiter → Complete.
	return classifyScalar(raw)
}
