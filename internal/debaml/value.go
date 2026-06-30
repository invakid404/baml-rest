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
}

// field is one ordered object entry. Duplicate keys are allowed (the
// strict decoder and fixing parser both preserve them in input order);
// coerceClass's input-key-first field assignment resolves duplicates
// last-wins, matching BAML's update_map overwrite (and the encoding/json
// map-decode semantics the M1 path relied on).
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
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return value{}, err
	}
	v, err := decodeToken(dec, tok)
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
func decodeToken(dec *json.Decoder, tok json.Token) (value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObject(dec)
		case '[':
			return decodeArray(dec)
		default:
			return value{}, fmt.Errorf("unexpected JSON delimiter %q", t)
		}
	case string:
		return value{kind: valString, strV: t}, nil
	case json.Number:
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
func decodeObject(dec *json.Decoder) (value, error) {
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
		v, err := decodeToken(dec, vtok)
		if err != nil {
			return value{}, err
		}
		obj.objV = append(obj.objV, field{key: key, val: v})
	}
}

// decodeArray consumes an array body (the opening '[' is already read) up
// to and including the matching ']'. The element slice is non-nil so an
// empty array stays distinct from a null.
func decodeArray(dec *json.Decoder) (value, error) {
	arr := value{kind: valArray, arrV: []value{}}
	for {
		tok, err := dec.Token()
		if err != nil {
			return value{}, err
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return arr, nil
		}
		v, err := decodeToken(dec, tok)
		if err != nil {
			return value{}, err
		}
		arr.arrV = append(arr.arrV, v)
	}
}
