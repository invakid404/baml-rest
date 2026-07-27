package debaml

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// valueKind tags the JSON shape of an ordered value.
type valueKind uint8

const (
	valNull valueKind = iota
	valBool
	valNumber
	valString
	valArray
	valObject
)

// String names the kind for diagnostics (coercion type-mismatch messages).
func (k valueKind) String() string {
	switch k {
	case valNull:
		return "null"
	case valBool:
		return "bool"
	case valNumber:
		return "number"
	case valString:
		return "string"
	case valArray:
		return "array"
	case valObject:
		return "object"
	default:
		return "unknown"
	}
}

// value is the native parser's ordered JSONish value. Unlike a
// map[string]any decode it preserves object key order, which the M2+ map
// path needs (maps emit input key order while classes emit schema order),
// and it carries numbers as json.Number so coercion can keep the
// conservative int/float distinction. Exactly one payload field is
// meaningful per kind:
//
//   - valNull:   none
//   - valBool:   boolV
//   - valNumber: numV
//   - valString: strV
//   - valArray:  arrV
//   - valObject: objV (ordered fields)
type value struct {
	kind  valueKind
	boolV bool
	numV  json.Number
	strV  string
	arrV  []value
	objV  []field

	// incomplete records BAML's jsonish CompletionState::Incomplete for a value
	// recovered under raw_is_done=false (M4b stream parse). It is set ONLY by the
	// streaming fixing parser (see streamFix in fix.go): a container/string/scalar
	// closed by EOF rather than its proper delimiter, and the last (still-building)
	// value inside an unterminated container, are Incomplete. The final (strict /
	// M2a fixing) decoders never set it — every value they produce is complete —
	// so final-parse behavior is unchanged. Stream coercion uses it to DECLINE an
	// incomplete value whose type requires done (semantic streaming would delete
	// it), which M4b does not model. See coerceStream in coerce.go.
	incomplete bool

	// numSerdeFloat records that a valNumber came from a STRICT (serde-equivalent)
	// decode that stores it as an f64, so BAML's `as_i64` on it is None. It is set
	// ONLY by [strictDecodeMode] under [numModeSerde] — the Phase-3c `JsonValue` lane
	// — and read ONLY by that family's int/float arm split (alias_number.go documents
	// the two-path number model and the `-0` split it exists for). Every legacy lane
	// leaves it false, so the `JSON` family, the Phase-2 class families, and the
	// dynamic universe are unaffected.
	//
	// It is deliberately NOT part of [valueEqual]: the pair-guard's (name, value)
	// membership keeps its exact Phase-2/3a/3b semantics, and the tag can only ever
	// differ between a strict-parsed and a fixing-parsed `-0` — two spellings that
	// never meet on one active set (a candidate is parsed by exactly one path).
	numSerdeFloat bool
}

// field is one ordered object entry. Duplicate keys are allowed (the
// strict decoder and fixing parser both preserve them in input order);
// coerceClass's input-key-first field assignment resolves duplicate matches
// FIRST-wins, matching BAML's update_map "keep first" (coerce_class.rs:548).
type field struct {
	key string
	val value
}

// strictDecode parses s as a single strict JSON value into an ordered
// value. It uses encoding/json's token stream, so it rejects exactly the
// fixing-parser syntax the fix pass recovers (trailing commas, unquoted
// keys, single quotes), preserves object key order, and keeps numbers as
// json.Number. Trailing non-whitespace after the value is rejected so a
// "value + junk" string does not pass as strict JSON — matching M1's
// encoding/json strict decode while gaining key-order fidelity.
func strictDecode(s string) (value, error) {
	return strictDecodeMode(s, numModeLegacy)
}

// strictDecodeMode is [strictDecode] parameterized by the number-classification mode.
// Under [numModeSerde] (the Phase-3c `JsonValue` lane) it additionally reproduces
// serde_json's two number facts: a token serde_json REJECTS (an f64 overflow such as
// `1e400`) fails the WHOLE decode — so the candidate falls through to the fixing
// parser exactly as it does in BAML — and a token serde_json stores as an f64 (`-0`,
// an out-of-i64-range integer, any `.`/`e` form) is tagged [value.numSerdeFloat] so
// the family's strict `int` arm misses it. See alias_number.go for the model.
func strictDecodeMode(s string, nm numMode) (value, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return value{}, err
	}
	v, err := decodeToken(dec, tok, nm)
	if err != nil {
		return value{}, err
	}
	// A second token must hit EOF: any remaining token is trailing data.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return value{}, fmt.Errorf("unexpected trailing data after JSON value")
		}
		return value{}, fmt.Errorf("unexpected trailing data after JSON value: %w", err)
	}
	return v, nil
}

// decodeToken builds the ordered value rooted at an already-read token.
// The decoder is positioned just after tok, so container tokens recurse
// to consume their contents (and matching close delimiter).
func decodeToken(dec *json.Decoder, tok json.Token, nm numMode) (value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObject(dec, nm)
		case '[':
			return decodeArray(dec, nm)
		default:
			return value{}, fmt.Errorf("unexpected JSON delimiter %q", t)
		}
	case string:
		return value{kind: valString, strV: t}, nil
	case json.Number:
		if nm == numModeSerde {
			if !serdeParsesNumber(string(t)) {
				// serde_json REJECTS this token (f64 overflow), which fails the whole
				// strict parse in BAML — the caller then re-parses the candidate with
				// the fixing parser, exactly as BAML does.
				return value{}, fmt.Errorf("serde-rejected number token %q", string(t))
			}
			return value{kind: valNumber, numV: t, numSerdeFloat: serdeStoresAsFloat(string(t))}, nil
		}
		return value{kind: valNumber, numV: t}, nil
	case bool:
		return value{kind: valBool, boolV: t}, nil
	case nil:
		return value{kind: valNull}, nil
	default:
		return value{}, fmt.Errorf("unexpected JSON token %T", tok)
	}
}

// decodeObject consumes an object body (the opening '{' is already read),
// preserving field order, up to and including the matching '}'.
func decodeObject(dec *json.Decoder, nm numMode) (value, error) {
	obj := value{kind: valObject}
	for {
		tok, err := dec.Token()
		if err != nil {
			return value{}, err
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return obj, nil
		}
		key, ok := tok.(string)
		if !ok {
			return value{}, fmt.Errorf("expected object key string, got %T", tok)
		}
		vtok, err := dec.Token()
		if err != nil {
			return value{}, err
		}
		v, err := decodeToken(dec, vtok, nm)
		if err != nil {
			return value{}, err
		}
		obj.objV = append(obj.objV, field{key: key, val: v})
	}
}

// decodeArray consumes an array body (the opening '[' is already read) up
// to and including the matching ']'. The element slice is non-nil so an
// empty array stays distinct from a null.
func decodeArray(dec *json.Decoder, nm numMode) (value, error) {
	arr := value{kind: valArray, arrV: []value{}}
	for {
		tok, err := dec.Token()
		if err != nil {
			return value{}, err
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return arr, nil
		}
		v, err := decodeToken(dec, tok, nm)
		if err != nil {
			return value{}, err
		}
		arr.arrV = append(arr.arrV, v)
	}
}
