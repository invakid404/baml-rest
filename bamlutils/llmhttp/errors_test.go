package llmhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2"
)

func TestClassifyTransportErr(t *testing.T) {
	t.Parallel()

	// Stdlib net/http's GOAWAY error format, mirrored verbatim from
	// h2_bundle.go:9445. Used as a synthetic input to exercise the
	// substring-detection branch without needing a real HTTP/2 server.
	stdlibGoAway := fmt.Errorf(`Get "https://example.com": http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`)

	// Asymmetric gating: typed syscall errors (ECONNREFUSED / ECONNRESET /
	// EPIPE) and net.ErrClosed are unambiguous transport drops at any
	// layer and classify regardless of staleConnTeardownAcceptable. The
	// stale-conn-teardown family (fasthttp.ErrConnectionClosed, HTTP/2
	// GOAWAY, x/net/http2.GoAwayError, bare io.EOF) overlaps with mid-
	// content failure at body-read sites and is gated together — accepted
	// only when the wrap site sets staleConnTeardownAcceptable=true
	// (initial-Do, before any body has been read).
	cases := []struct {
		name                        string
		err                         error
		staleConnTeardownAcceptable bool
		wantCategory                TransportFlakeCategory
		wantFlake                   bool
	}{
		{
			name:                        "nil",
			err:                         nil,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "connection refused via *net.OpError->*os.SyscallError, gate=true",
			err:                         &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeConnectionRefused,
			wantFlake:                   true,
		},
		{
			name:                        "connection refused via *net.OpError->*os.SyscallError, gate=false (still classifies)",
			err:                         &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeConnectionRefused,
			wantFlake:                   true,
		},
		{
			name:                        "connection reset via *net.OpError->*os.SyscallError, gate=true",
			err:                         &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeConnectionReset,
			wantFlake:                   true,
		},
		{
			name:                        "connection reset via *net.OpError->*os.SyscallError, gate=false (still classifies)",
			err:                         &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeConnectionReset,
			wantFlake:                   true,
		},
		{
			name:                        "broken pipe via *net.OpError->*os.SyscallError, gate=true",
			err:                         &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}},
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeBrokenPipe,
			wantFlake:                   true,
		},
		{
			name:                        "broken pipe via *net.OpError->*os.SyscallError, gate=false (still classifies)",
			err:                         &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}},
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeBrokenPipe,
			wantFlake:                   true,
		},
		{
			name:                        "use of closed network connection via net.ErrClosed, gate=true",
			err:                         fmt.Errorf("read tcp 127.0.0.1:1234->127.0.0.1:5678: %w", net.ErrClosed),
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeClosedConnection,
			wantFlake:                   true,
		},
		{
			name:                        "use of closed network connection via net.ErrClosed, gate=false (still classifies)",
			err:                         fmt.Errorf("read tcp 127.0.0.1:1234->127.0.0.1:5678: %w", net.ErrClosed),
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeClosedConnection,
			wantFlake:                   true,
		},
		{
			name:                        "fasthttp.ErrConnectionClosed direct, gate=true",
			err:                         fasthttp.ErrConnectionClosed,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "fasthttp.ErrConnectionClosed direct, gate=false (rejected)",
			err:                         fasthttp.ErrConnectionClosed,
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "fasthttp.ErrConnectionClosed wrapped, gate=true",
			err:                         fmt.Errorf("hostclient: %w", fasthttp.ErrConnectionClosed),
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "fasthttp.ErrConnectionClosed wrapped, gate=false (rejected)",
			err:                         fmt.Errorf("hostclient: %w", fasthttp.ErrConnectionClosed),
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "stdlib HTTP/2 GOAWAY substring, gate=true",
			err:                         stdlibGoAway,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "stdlib HTTP/2 GOAWAY substring, gate=false (rejected)",
			err:                         stdlibGoAway,
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "x/net/http2.GoAwayError value (errors.As), gate=true",
			err:                         http2.GoAwayError{LastStreamID: 1, ErrCode: 0, DebugData: ""},
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "x/net/http2.GoAwayError value (errors.As), gate=false (rejected)",
			err:                         http2.GoAwayError{LastStreamID: 1, ErrCode: 0, DebugData: ""},
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "x/net/http2.GoAwayError wrapped, gate=true",
			err:                         fmt.Errorf("transport: %w", http2.GoAwayError{LastStreamID: 1, ErrCode: 0}),
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "x/net/http2.GoAwayError wrapped, gate=false (rejected)",
			err:                         fmt.Errorf("transport: %w", http2.GoAwayError{LastStreamID: 1, ErrCode: 0}),
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "io.EOF, gate=true",
			err:                         io.EOF,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "io.EOF wrapped, gate=true",
			err:                         fmt.Errorf(`Post "http://127.0.0.1:1234": %w`, io.EOF),
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeStaleConnTeardown,
			wantFlake:                   true,
		},
		{
			name:                        "io.EOF, gate=false (rejected)",
			err:                         io.EOF,
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "io.ErrUnexpectedEOF, gate=true (not a flake)",
			err:                         io.ErrUnexpectedEOF,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "stringly 'unexpected EOF', gate=false (rejected)",
			err:                         fmt.Errorf("unexpected EOF"),
			staleConnTeardownAcceptable: false,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "context.DeadlineExceeded (not a flake)",
			err:                         context.DeadlineExceeded,
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "tls.RecordHeaderError (not a flake)",
			err:                         tls.RecordHeaderError{Msg: "bogus header"},
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
		{
			name:                        "plain unrelated error (not a flake)",
			err:                         errors.New("nope"),
			staleConnTeardownAcceptable: true,
			wantCategory:                TransportFlakeUnknown,
			wantFlake:                   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := classifyTransportErr(tc.err, "llmhttp: request failed", tc.staleConnTeardownAcceptable)
			if !tc.wantFlake {
				if te != nil {
					t.Fatalf("expected nil *TransportError for non-flake err %v, got %+v", tc.err, te)
				}
				if tc.err != nil && errors.Is(tc.err, ErrTransportFlake) {
					t.Fatalf("non-flake err %v should not match ErrTransportFlake via errors.Is", tc.err)
				}
				return
			}
			if te == nil {
				t.Fatalf("expected non-nil *TransportError, got nil for err %v", tc.err)
			}
			if te.Category != tc.wantCategory {
				t.Errorf("category: got %v, want %v", te.Category, tc.wantCategory)
			}
			if !errors.Is(te, ErrTransportFlake) {
				t.Errorf("errors.Is(te, ErrTransportFlake) = false, want true")
			}
			if !errors.Is(te, tc.err) {
				t.Errorf("errors.Is(te, underlying) = false, want true (underlying chain broken)")
			}
		})
	}
}

func TestTransportError_ErrorRendering(t *testing.T) {
	t.Parallel()

	// The wrap-site Error() rendering must be byte-identical to the
	// fmt.Errorf("%s: %w", prefix, underlying) shape it replaces, with
	// no newline (errors.Join's "\n" leakage) and no sentinel-text
	// leakage from ErrTransportFlake.
	cases := []struct {
		name       string
		prefix     string
		underlying error
		want       string
	}{
		{
			name:       "request failed + ECONNRESET via OpError",
			prefix:     "llmhttp: request failed",
			underlying: &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
			want:       "llmhttp: request failed: read: read: connection reset by peer",
		},
		{
			name:       "request failed + fasthttp ErrConnectionClosed",
			prefix:     "llmhttp: request failed",
			underlying: fasthttp.ErrConnectionClosed,
			want:       "llmhttp: request failed: " + fasthttp.ErrConnectionClosed.Error(),
		},
		{
			name:       "failed to read response body + ECONNRESET",
			prefix:     "llmhttp: failed to read response body",
			underlying: &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
			want:       "llmhttp: failed to read response body: read: read: connection reset by peer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := &TransportError{
				Category:   TransportFlakeConnectionReset,
				Prefix:     tc.prefix,
				Underlying: tc.underlying,
			}
			got := te.Error()
			if got != tc.want {
				t.Errorf("Error() rendering mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
			// Must match the fmt.Errorf shape it replaces.
			equiv := fmt.Errorf("%s: %w", tc.prefix, tc.underlying).Error()
			if got != equiv {
				t.Errorf("Error() rendering diverges from fmt.Errorf shape:\n got: %q\nfmt: %q", got, equiv)
			}
			// Must NOT contain ErrTransportFlake's text — the sentinel
			// is only visible to errors.Is, not to err.Error().
			if got == "" || got[len(got)-1] == '\n' {
				t.Errorf("Error() rendering ends with a newline (errors.Join-style leakage): %q", got)
			}
			if containsTransportFlakeText(got) {
				t.Errorf("Error() rendering leaked ErrTransportFlake text: %q", got)
			}
		})
	}
}

func containsTransportFlakeText(s string) bool {
	const needle = "llmhttp: transport flake"
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
