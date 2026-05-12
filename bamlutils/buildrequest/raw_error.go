package buildrequest

import "errors"

// rawCarryingError attaches accumulated raw model output to a wrapped
// error so the text survives the retry/fallback boundary in the
// BuildRequest orchestrators. retry.Execute and the fallback walk only
// carry the final error across attempts; the streaming accumulator and
// non-streaming response body live inside the attempt closure and would
// otherwise be lost by the time the orchestrator reaches its outer error
// emission. Wrap at the failing-attempt return sites; extract once at
// the outer newResult call before the error frame ships to the worker
// bridge.
//
// The wrapper is intentionally transparent to errors.Is by way of
// Unwrap — classification (e.g. ErrOutputParse) keeps working through
// any number of rawCarryingError wraps. Raw() is the typed accessor;
// rawFromError is the errors.As-aware helper that walks the chain.
type rawCarryingError struct {
	err error
	raw string
}

func (e *rawCarryingError) Error() string { return e.err.Error() }

func (e *rawCarryingError) Unwrap() error { return e.err }

func (e *rawCarryingError) Raw() string { return e.raw }

// newRawError wraps err with raw. Returns err unchanged when err is nil
// or raw is empty — the empty-raw short-circuit keeps allocations off
// the common pre-stream-failure path where there is nothing to attach
// and rawFromError's empty-string return would be identical anyway.
func newRawError(err error, raw string) error {
	if err == nil || raw == "" {
		return err
	}
	return &rawCarryingError{err: err, raw: raw}
}

// rawFromError extracts the accumulated raw from err if it (or any
// wrapped cause) is a *rawCarryingError. Returns "" for nil errors and
// for chains with no rawCarryingError in them — the orchestrator's
// outer emission passes this verbatim to newResult, and the worker
// bridge's mergeRawDetail treats an empty raw as a no-op (omitempty).
func rawFromError(err error) string {
	if err == nil {
		return ""
	}
	var rce *rawCarryingError
	if errors.As(err, &rce) {
		return rce.raw
	}
	return ""
}
