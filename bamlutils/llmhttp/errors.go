package llmhttp

import (
	"errors"
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
// bareEOFAcceptable gates the stale-keepalive bare-EOF heuristic:
// true at initial-Do wrap sites (where bare EOF unambiguously means
// the upstream tore down a reusable connection before any body was
// read); false at body-read wrap sites (where bare EOF is application
// content failure, not transport).
func classifyTransportErr(err error, prefix string, bareEOFAcceptable bool) *TransportError {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return &TransportError{Category: TransportFlakeConnectionRefused, Prefix: prefix, Underlying: err}
	case errors.Is(err, syscall.ECONNRESET):
		return &TransportError{Category: TransportFlakeConnectionReset, Prefix: prefix, Underlying: err}
	case errors.Is(err, syscall.EPIPE):
		return &TransportError{Category: TransportFlakeBrokenPipe, Prefix: prefix, Underlying: err}
	case errors.Is(err, net.ErrClosed):
		return &TransportError{Category: TransportFlakeClosedConnection, Prefix: prefix, Underlying: err}
	case errors.Is(err, fasthttp.ErrConnectionClosed):
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
	// Active only at initial-Do wrap sites; at body-read sites bare
	// EOF is application content failure (deliberately rejected).
	if bareEOFAcceptable && errors.Is(err, io.EOF) {
		return &TransportError{Category: TransportFlakeStaleConnTeardown, Prefix: prefix, Underlying: err}
	}

	return nil
}
