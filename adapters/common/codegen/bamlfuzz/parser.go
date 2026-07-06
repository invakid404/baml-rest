package bamlfuzz

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
)

// ErrParserUnavailable is the sentinel a Parser returns when it cannot
// service a request — either because it is the no-op stub (no native
// parser has been registered yet) or because the request shape is one
// the parser does not implement (e.g. the native de-BAML candidate
// declining every Stream request, which stays fallback until a later slice
// claims native stream parity).
//
// The differential comparator treats ErrParserUnavailable from the
// *candidate* (native) parser as a skip/pass, not a parser success. The
// same error from the *BAML* (oracle) parser is a harness failure: BAML
// is supposed to be able to parse everything the harness throws at it on
// the legs it claims to cover.
var ErrParserUnavailable = errors.New("bamlfuzz: parser unavailable")

// ParseRequest is the input to a Parser: a schema, the exact raw model
// text to parse against it, and the parse-mode flags. The harness drives
// BAML and any registered native parser through the identical request so
// a divergence is attributable to the parser, not the inputs.
type ParseRequest struct {
	// Schema is the IR the raw text is parsed against.
	Schema FuzzSchema
	// Raw is the exact model text fed to the parser — a complete
	// response for a final parse, or an accumulated prefix for a stream
	// parse. It is passed verbatim; the parser does no extraction or
	// trimming the harness has not already applied.
	Raw string
	// Stream selects parse-stream semantics. Stream=false returns the
	// final typed JSON; Stream=true returns the partial typed JSON BAML's
	// parse-stream would emit for this raw prefix.
	Stream bool
	// PreserveSchemaOrder mirrors the dynamic endpoints' preserve flag:
	// when true the returned JSON's object keys follow schema declaration
	// order, so the comparator can run a schema-aware key-order check.
	PreserveSchemaOrder bool
}

// ParseResult is a Parser's successful output: the typed JSON in the
// flattened shape the dynamic endpoints expose (no DynamicProperties
// wrapper). A non-nil error from Parse means JSON is not meaningful.
type ParseResult struct {
	JSON json.RawMessage
}

// Parser is the pluggable parse backend the differential harness drives.
// Two implementations matter: a BAML adapter (the oracle/reference) and,
// once it exists, a native parser (the candidate). The interface lives in
// bamlfuzz rather than dynclient so a native parser can satisfy it
// without depending on BAML client concepts.
//
// Contract:
//   - Parse is given the FuzzSchema, the exact raw model text, and the
//     parse-mode flags via ParseRequest.
//   - A successful result carries valid JSON in the flattened dynamic
//     shape; the harness compares success/error parity and normalized
//     JSON, never exact error strings.
//   - Returning ErrParserUnavailable signals "I do not service this
//     request"; the comparator skips the native leg on that sentinel and
//     fails the harness on it from the BAML leg.
type Parser interface {
	Name() string
	Parse(ctx context.Context, req ParseRequest) (ParseResult, error)
}

// NoopParser is the default registered native parser. It services no
// request, always returning ErrParserUnavailable, so the differential
// leg is a no-op pass until a real native parser is registered. The
// moment one is, the same corpus and oracle cases become live
// native-vs-BAML differentials with no harness rewrite.
type NoopParser struct{}

// Name identifies the stub in diff output and envelopes.
func (NoopParser) Name() string { return "native_stub" }

// Parse always declines, signalling the comparator to skip the native leg.
func (NoopParser) Parse(context.Context, ParseRequest) (ParseResult, error) {
	return ParseResult{}, ErrParserUnavailable
}

// nativeParserRegistry holds the process-wide native parser the harness
// diffs BAML against. It defaults to NoopParser so a caller that never
// registers a real parser still gets a well-defined skip/pass leg.
var nativeParserRegistry = struct {
	mu sync.RWMutex
	p  Parser
}{p: NoopParser{}}

// RegisterNativeParser installs p as the process-wide native parser the
// differential harness diffs against and returns a restore func that
// reinstalls whatever was registered before. The restore func is
// idempotent. Passing a nil parser — an untyped nil, or a typed-nil such
// as a nil `*SomeParser` (a non-nil interface wrapping a nil pointer) —
// resets the registry to NoopParser, so the harness never holds a
// candidate that panics the moment DiffParsers calls Parse / Name.
//
// The returned restore is intended for `defer restore()` in a test that
// swaps in a fake or a real native parser for the duration of the test.
func RegisterNativeParser(p Parser) (restore func()) {
	if isNilParser(p) {
		p = NoopParser{}
	}
	nativeParserRegistry.mu.Lock()
	prev := nativeParserRegistry.p
	nativeParserRegistry.p = p
	nativeParserRegistry.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			nativeParserRegistry.mu.Lock()
			nativeParserRegistry.p = prev
			nativeParserRegistry.mu.Unlock()
		})
	}
}

// isNilParser reports whether p is unusable as a candidate: an untyped
// nil interface, or a typed-nil whose underlying value is a nilable kind
// (pointer, map, chan, func, slice, interface) that is itself nil. The
// latter is a non-nil interface — `p == nil` is false — but calling any
// method that dereferences the receiver would panic, so it must be
// treated like a missing registration.
func isNilParser(p Parser) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// RegisteredNativeParser returns the currently registered native parser,
// never nil. With no registration it is the NoopParser, so callers can
// unconditionally pass the result to DiffParsers and rely on the
// ErrParserUnavailable skip path.
func RegisteredNativeParser() Parser {
	nativeParserRegistry.mu.RLock()
	defer nativeParserRegistry.mu.RUnlock()
	return nativeParserRegistry.p
}
