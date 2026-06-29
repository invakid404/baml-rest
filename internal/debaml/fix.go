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
// Everything else — comments, backtick / triple-quoted strings, escapes
// inside double-quoted strings, missing commas between elements, unquoted
// bareword string values, unterminated structures, and multiple top-level
// values — returns errFixUnsupported. BAML supports many of those, but
// they interact with completion flags, inferred arrays, and union scoring,
// so declining keeps the native-vs-BAML differential green via fallback.
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

// parseDoubleQuoted reads a double-quoted string, closing at the first
// unescaped quote. The opening '"' is at the cursor. A backslash escape
// declines the parse (BAML's escape fixing and its embedded-quote close
// heuristics are deferred), as does a triple-quoted opener or an
// unterminated string.
func (p *fixer) parseDoubleQuoted() (string, error) {
	if strings.HasPrefix(p.s[p.pos:], `"""`) {
		return "", errFixUnsupported // triple-quoted string deferred
	}
	p.pos++ // consume opening '"'
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.pos]
		switch c {
		case '\\':
			return "", errFixUnsupported // escapes deferred to BAML
		case '"':
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
