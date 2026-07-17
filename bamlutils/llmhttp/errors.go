package llmhttp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
)

// ErrTransportFlake is the umbrella sentinel that test code uses to
// gate "transient transport failure is acceptable here":
//
//	if errors.Is(err, llmhttp.ErrTransportFlake) { ... }
//
// Every *TransportError returned by classifyTransportErr unwraps to
// ErrTransportFlake (and to its Underlying err), so errors.Is matches
// both the umbrella and any typed underlying error (e.g.
// syscall.ECONNRESET) where applicable.
var ErrTransportFlake = errors.New("llmhttp: transport flake")

// TransportFlakeCategory tags a *TransportError with the underlying
// transport class. Categories are diagnostic — control flow keys off
// the umbrella ErrTransportFlake sentinel via errors.Is. Use
// errors.As(err, &te) and switch on te.Category for category-specific
// behavior (e.g. retry policy that distinguishes connection-refused
// from stale-keepalive teardown).
type TransportFlakeCategory int

const (
	TransportFlakeUnknown TransportFlakeCategory = iota
	TransportFlakeConnectionRefused
	TransportFlakeConnectionReset
	TransportFlakeBrokenPipe
	TransportFlakeClosedConnection
	// TransportFlakeStaleConnTeardown folds the three distinct
	// mechanisms — net/http stale-keepalive bare EOF, HTTP/2 GOAWAY,
	// and fasthttp ErrConnectionClosed — that all surface as "the
	// upstream tore down a reusable connection". Same retry-policy
	// class; the distinct mechanisms remain visible via the Underlying
	// chain.
	TransportFlakeStaleConnTeardown
	// TransportFlakeIdleTimeout is the inter-token idle read timeout
	// (ErrIdleTimeout) fired by idleTimeoutReader when a streaming
	// provider goes silent mid-stream. It is unambiguous — the watchdog
	// only fires after the configured idle window of zero bytes — so it
	// is classified retryable at every wrap site (ungated).
	TransportFlakeIdleTimeout
)

func (c TransportFlakeCategory) String() string {
	switch c {
	case TransportFlakeConnectionRefused:
		return "connection-refused"
	case TransportFlakeConnectionReset:
		return "connection-reset"
	case TransportFlakeBrokenPipe:
		return "broken-pipe"
	case TransportFlakeClosedConnection:
		return "closed-connection"
	case TransportFlakeStaleConnTeardown:
		return "stale-conn-teardown"
	case TransportFlakeIdleTimeout:
		return "idle-timeout"
	default:
		return "unknown"
	}
}

// TransportError wraps a transport-class err with a category tag and
// the original wrap-site message. Its Error() method formats as
// "<prefix>: <underlying err>" — a single line, identical to the
// existing fmt.Errorf("llmhttp: ...: %w", err) shape, so rendered
// error strings (t.Logf, panic dumps, log lines) are unchanged for
// any consumer that prints err.Error().
//
// Unwrap returns both the underlying err and ErrTransportFlake so
// errors.Is matches both the typed underlying error (e.g.
// syscall.ECONNRESET) and the umbrella sentinel.
//
// errors.Join is deliberately NOT used: its joinError.Error() joins
// members with "\n", which would leak "llmhttp: transport flake" as
// a second line into every rendered transport-error message.
type TransportError struct {
	Category   TransportFlakeCategory
	Prefix     string
	Underlying error
}

func (e *TransportError) Error() string {
	return e.Prefix + ": " + e.Underlying.Error()
}

func (e *TransportError) Unwrap() []error {
	return []error{e.Underlying, ErrTransportFlake}
}

// classifyTransportErr inspects err and returns a *TransportError with
// the right category attached, or nil if err is not a transport flake.
// Called at each transport wrap site.
//
// staleConnTeardownAcceptable gates the entire TransportFlakeStaleConnTeardown
// family — fasthttp.ErrConnectionClosed, the HTTP/2 GOAWAY substring, the
// x/net/http2.GoAwayError forward-compat secondary, and the bare-EOF
// heuristic — together. Pass true at initial-Do wrap sites (no body has
// been read yet, so a torn-down reusable connection is unambiguously a
// transport flake). Pass false at body-read wrap sites: every form of
// stale-conn-teardown there overlaps with mid-content failure, so the
// strict-content-integrity contract rejects them all rather than letting
// a torn-down stream silently look like a transport flake.
//
// Typed syscall errors (ECONNREFUSED / ECONNRESET / EPIPE) and net.ErrClosed
// remain ungated: those are unambiguous transport drops at any layer.
func classifyTransportErr(err error, prefix string, staleConnTeardownAcceptable bool) *TransportError {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, ErrIdleTimeout):
		// Inter-token idle read timeout. Ungated: the watchdog only fires
		// after the configured window of zero bytes, so it is an
		// unambiguous mid-stream stall at any wrap site. Must classify
		// retryable so the orchestrator retries/falls back rather than
		// running parseFinal on the truncated accumulator.
		return &TransportError{Category: TransportFlakeIdleTimeout, Prefix: prefix, Underlying: err}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &TransportError{Category: TransportFlakeConnectionRefused, Prefix: prefix, Underlying: err}
	case errors.Is(err, syscall.ECONNRESET):
		return &TransportError{Category: TransportFlakeConnectionReset, Prefix: prefix, Underlying: err}
	case errors.Is(err, syscall.EPIPE):
		return &TransportError{Category: TransportFlakeBrokenPipe, Prefix: prefix, Underlying: err}
	case errors.Is(err, net.ErrClosed):
		return &TransportError{Category: TransportFlakeClosedConnection, Prefix: prefix, Underlying: err}
	}

	if !staleConnTeardownAcceptable {
		return nil
	}

	if errors.Is(err, fasthttp.ErrConnectionClosed) {
		return &TransportError{Category: TransportFlakeStaleConnTeardown, Prefix: prefix, Underlying: err}
	}

	// HTTP/2 GOAWAY: stdlib net/http embeds its own *unexported*
	// http2GoAwayError (net/http/h2_bundle.go). errors.As against
	// golang.org/x/net/http2.GoAwayError will NOT catch it. Substring
	// match on err.Error() is the load-bearing detection.
	if strings.Contains(strings.ToLower(err.Error()), "http2: server sent goaway") {
		return &TransportError{Category: TransportFlakeStaleConnTeardown, Prefix: prefix, Underlying: err}
	}
	// Forward-compat secondary for code paths that explicitly use
	// x/net/http2.Transport. GoAwayError is returned by value, so
	// errors.As needs a value-typed variable addressed by &.
	var goAway http2.GoAwayError
	if errors.As(err, &goAway) {
		return &TransportError{Category: TransportFlakeStaleConnTeardown, Prefix: prefix, Underlying: err}
	}

	// net/http stale-keepalive teardown: errServerClosedIdle is
	// unexported, so the bare-EOF heuristic is the only signal.
	if errors.Is(err, io.EOF) {
		return &TransportError{Category: TransportFlakeStaleConnTeardown, Prefix: prefix, Underlying: err}
	}

	return nil
}

// ErrExactStream is the umbrella sentinel for the exact native STREAM
// transport's own terminal failures (exact_stream.go): a refused second
// RoundTrip, a plan drift, an invalid 2xx content type, a first-body or idle
// deadline, or a pre-send body-read failure. Test/caller code gates on it via
// errors.Is(err, ErrExactStream).
//
// It is DELIBERATELY DISTINCT from ErrTransportFlake. Every one of these
// failures occurs on the exact stream lane, whose one physical request is
// claimed immediately before the underlying RoundTrip (scope I4): once claimed,
// the failure is TERMINAL for that stream and must never be reclassified as a
// retryable transport flake. Keeping a separate umbrella prevents an exact
// stream failure from accidentally unwrapping to ErrTransportFlake and being
// retried/replayed. The legacy classifyTransportErr path is unchanged and never
// produces these; the (still unrouted in Phase 7A) native lane matches on them
// directly.
var ErrExactStream = errors.New("llmhttp: exact stream")

// ErrExactStreamSecondRoundTrip is returned when the one-shot exact stream
// RoundTripper is entered a second time. It is a ZERO-SOCKET failure — the
// second entry never touches the network — so a nanollm retry/fallback
// regression surfaces here as a refused second call rather than a duplicate
// physical request (scope §5.7 / I3). Unwraps to ErrExactStream.
var ErrExactStreamSecondRoundTrip = fmt.Errorf("llmhttp: exact stream second RoundTrip refused (zero socket): %w", ErrExactStream)

// ErrExactStreamFirstBodyTimeout is surfaced by the exact stream body reader
// when no raw upstream body byte arrives within the first-body window after
// successful response headers (scope §5.10 deadline 2). It is distinct from the
// inter-byte idle timeout below and from the legacy ErrIdleTimeout so the two
// exact-stream deadlines are separately observable. Unwraps to ErrExactStream.
var ErrExactStreamFirstBodyTimeout = fmt.Errorf("llmhttp: exact stream first-body timeout: %w", ErrExactStream)

// ErrExactStreamIdleTimeout is surfaced by the exact stream body reader when no
// raw upstream body byte arrives within the inter-byte idle window after the
// first body byte (scope §5.10 deadline 3). It is the exact stream lane's own
// idle sentinel, distinct from the legacy ErrIdleTimeout, because an exact
// stream idle stall is TERMINAL (never a retryable flake). Unwraps to
// ErrExactStream.
var ErrExactStreamIdleTimeout = fmt.Errorf("llmhttp: exact stream idle timeout: %w", ErrExactStream)

// ExactStreamPlanMismatchError reports that the request the exact stream
// transport was asked to send drifted from the admitted plan. Field names the
// drifting facet only — "method", "url", "host", "body", or "header:<name>"
// (a header NAME, never its value) — so a diagnostic can never leak a URL
// query, header value, or body byte. It is a ZERO-SOCKET, terminal invariant
// failure detected before the underlying RoundTrip. Unwraps to ErrExactStream.
type ExactStreamPlanMismatchError struct {
	Field string
}

func (e *ExactStreamPlanMismatchError) Error() string {
	return "llmhttp: exact stream plan mismatch (" + e.Field + ")"
}

func (e *ExactStreamPlanMismatchError) Unwrap() error { return ErrExactStream }

// ExactStreamContentTypeError reports a 2xx response whose Content-Type is not
// text/event-stream. The bounded media type is surfaced to aid diagnosis (a
// Content-Type carries no secret); the response body is never buffered or
// echoed. Unwraps to ErrExactStream.
type ExactStreamContentTypeError struct {
	MediaType string
}

func (e *ExactStreamContentTypeError) Error() string {
	return "llmhttp: exact stream 2xx content type is not text/event-stream: " + e.MediaType
}

func (e *ExactStreamContentTypeError) Unwrap() error { return ErrExactStream }

// ExactStreamBodyReadError reports that reading the OUTGOING request body — in
// order to compare it against the admitted plan and restore it for the single
// send — failed. It is a ZERO-SOCKET failure detected before the underlying
// RoundTrip. The underlying cause is retained for diagnosis; no body bytes are
// ever surfaced. Unwraps to both the cause and ErrExactStream.
type ExactStreamBodyReadError struct {
	Cause error
}

func (e *ExactStreamBodyReadError) Error() string {
	return "llmhttp: exact stream outgoing body read failed: " + e.Cause.Error()
}

func (e *ExactStreamBodyReadError) Unwrap() []error { return []error{e.Cause, ErrExactStream} }

// classifyStreamErrc consumes the single terminal value sseclient.Stream
// (and the fast-path stream reader) emits and re-emits it on a buffered(1)
// channel after running non-nil values through classifyTransportErr with
// body-read semantics — same as Execute and drainFastBodyLimited, where
// any bytes already delivered upstream rule out the gated stale-conn-
// teardown family. A typed mid-stream transport drop (ECONNRESET / EPIPE /
// ECONNREFUSED / net.ErrClosed) therefore carries the ErrTransportFlake
// umbrella sentinel out of the streaming path; io.ErrUnexpectedEOF
// (chunked truncation) falls through to the generic wrap so its
// content-integrity meaning is preserved.
func classifyStreamErrc(src <-chan error) <-chan error {
	out := make(chan error, 1)
	go func() {
		defer close(out)
		out <- classifyStreamErr(<-src)
	}()
	return out
}

// classifyStreamErr is the synchronous core of classifyStreamErrc, shared with
// the callback streaming path (StreamCallback.ForEach). It runs a non-nil
// terminal stream error through classifyTransportErr with body-read semantics
// (staleConnTeardownAcceptable=false, since any delivered bytes rule out the
// gated stale-conn-teardown family) and wraps anything else generically so
// io.ErrUnexpectedEOF keeps its content-integrity meaning. nil passes through.
func classifyStreamErr(err error) error {
	if err == nil {
		return nil
	}
	if te := classifyTransportErr(err, "llmhttp: failed to read response body", false); te != nil {
		return te
	}
	return fmt.Errorf("llmhttp: failed to read response body: %w", err)
}
