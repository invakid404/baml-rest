package buildrequest

import "errors"

// ErrOutputParse is the umbrella sentinel for "BAML could not coerce or
// validate the raw LLM output against the method's return-type schema
// after a successful HTTP response". Callers gate on this sentinel via
// errors.Is to distinguish post-LLM parse/coercion failures from upstream
// provider failures and from request-building bugs.
//
// Wrapped at the two BuildRequest final-parse sites (non-streaming in
// call_orchestrator.go and streaming in orchestrator.go). The worker-side
// classifier maps it to apierror.CodeParseError on the way out so
// consumers receive the same code on /call, /call-with-raw, and /stream
// that /parse already emits.
var ErrOutputParse = errors.New("buildrequest: output parse failure")

// OutputParseError wraps a BAML parse/coercion error from the
// BuildRequest final-parse step. The wrapped Err carries the verbatim
// BAML message so downstream consumers still receive the original text
// in apierror.error; the wrapper only exists to make the failure class
// reachable via errors.Is(err, ErrOutputParse) and
// errors.As(err, &outputParseErr) without changing the rendered string.
//
// Wrap once at the final-parse call site (parseFinal), not at the
// extractor: extractor failures are a mixed bag (invalid provider 200
// JSON, missing fields, refusals, extractor bugs) that don't all
// translate to parse_error.
type OutputParseError struct {
	Err error
}

func (e *OutputParseError) Error() string { return e.Err.Error() }

func (e *OutputParseError) Unwrap() error { return e.Err }

// Is reports whether target is the ErrOutputParse sentinel. Allows
// callers to use errors.Is(err, ErrOutputParse) on a wrapped value
// without also threading the sentinel through Unwrap.
func (e *OutputParseError) Is(target error) bool {
	return target == ErrOutputParse
}

// wrapOutputParse wraps a BAML final-parse error in an OutputParseError.
// Returns nil for a nil input so call sites can write
// `return wrapOutputParse(parseErr)` without a manual nil guard.
func wrapOutputParse(err error) error {
	if err == nil {
		return nil
	}
	return &OutputParseError{Err: err}
}
