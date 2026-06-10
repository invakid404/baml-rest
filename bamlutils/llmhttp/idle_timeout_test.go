package llmhttp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/sseclient"
)

// controlledReader serves a fixed prefix of bytes, then blocks every
// subsequent Read until Close() is called — modelling a provider that
// delivers a first burst and then goes silent mid-stream. Close() unblocks
// the parked Read with io.EOF so the test exercises the race the wrapper is
// designed to defeat: a stalled socket that resolves to a CLEAN EOF, which
// the wrapper must override with ErrIdleTimeout.
type controlledReader struct {
	data      []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newControlledReader(initial []byte) *controlledReader {
	return &controlledReader{data: initial, closed: make(chan struct{})}
}

func (r *controlledReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	<-r.closed
	return 0, io.EOF
}

func (r *controlledReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

// pacedReader delivers each chunk after a fixed delay, then returns io.EOF —
// modelling a slow-but-progressing stream. Read is called serially by the
// SSE scanner, so no synchronisation is needed.
type pacedReader struct {
	chunks [][]byte
	i      int
	delay  time.Duration
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

func (r *pacedReader) Close() error { return nil }

// streamThroughIdle mirrors what ExecuteStream does: wrap the body with the
// idle timeout, parse SSE, and run the terminal error through
// classifyStreamErrc. It returns the events received and the classified
// terminal error.
func streamThroughIdle(ctx context.Context, body io.ReadCloser, idle time.Duration) ([]sseclient.Event, error) {
	wrapped := newIdleTimeoutReader(ctx, body, nil, idle)
	defer wrapped.Close()
	events, errc := sseclient.Stream(ctx, wrapped)
	classified := classifyStreamErrc(errc)

	var got []sseclient.Event
	for ev := range events {
		got = append(got, ev)
	}
	return got, <-classified
}

// TestIdleTimeoutFiresOnSilentStream is the load-bearing safety test: a
// stream that delivers two events and then goes silent must surface a
// retryable error (ErrTransportFlake AND ErrIdleTimeout), never a clean EOF
// that would look like a successful (truncated) completion.
func TestIdleTimeoutFiresOnSilentStream(t *testing.T) {
	t.Parallel()

	// Test ctx deadline is far above the idle window so the IDLE timeout,
	// not the test ctx, is what fires (max(0) << 50ms << 10s).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Two complete SSE events, then the reader blocks forever (until the
	// watchdog closes it). All bytes are delivered in the first Read, so
	// both events are dispatched before any Read blocks.
	body := newControlledReader([]byte("data: {\"v\":1}\n\ndata: {\"v\":2}\n\n"))

	got, err := streamThroughIdle(ctx, body, 50*time.Millisecond)

	if err == nil {
		t.Fatal("expected a terminal error from the idle timeout, got nil (silent truncation!)")
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("terminal error is not ErrIdleTimeout: %v", err)
	}
	if !errors.Is(err, ErrTransportFlake) {
		t.Errorf("terminal error is not ErrTransportFlake (not retryable): %v", err)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("terminal error must not be io.EOF: %v", err)
	}
	// Deterministic here: both events are fully delivered before the
	// blocking Read, so they are dispatched before the watchdog fires.
	if len(got) != 2 {
		t.Errorf("expected 2 events before the stall, got %d", len(got))
	}
}

// TestIdleTimeoutDoesNotFireOnProgressingStream verifies that a slow-but-
// progressing stream (a byte every 20ms, idle window 200ms — a 10x margin)
// is never killed: all events arrive and the terminal error is a clean nil.
func TestIdleTimeoutDoesNotFireOnProgressingStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 5
	chunks := make([][]byte, n)
	for i := range chunks {
		chunks[i] = []byte("data: {\"v\":1}\n\n")
	}
	body := &pacedReader{chunks: chunks, delay: 20 * time.Millisecond}

	got, err := streamThroughIdle(ctx, body, 200*time.Millisecond)

	if err != nil {
		t.Fatalf("idle timeout fired on a progressing stream: %v", err)
	}
	if len(got) != n {
		t.Errorf("expected %d events, got %d", n, len(got))
	}
}

// truncReader returns a complete SSE event together with a non-EOF error in a
// single Read — modelling fastStreamReader's io.ErrUnexpectedEOF on a
// truncated chunked stream. The watchdog never fires (long idle window), so
// this exercises the genuine-upstream-error path on the n>0 branch.
type truncReader struct {
	sent bool
	err  error
}

func (r *truncReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, []byte("data: {\"v\":1}\n\n"))
		return n, r.err
	}
	return 0, r.err
}

func (r *truncReader) Close() error { return nil }

// TestIdleTimeoutPropagatesUpstreamError verifies the data-wins fix does NOT
// swallow a genuine upstream error riding with n>0 bytes. The event must be
// delivered AND the error must surface on errc (non-nil, NOT a clean nil end,
// NOT relabelled ErrIdleTimeout) — otherwise a real truncation would be masked
// as a successful partial.
func TestIdleTimeoutPropagatesUpstreamError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Long idle window so the watchdog never fires — fired stays false, so
	// the error is genuinely upstream, not ours.
	body := &truncReader{err: io.ErrUnexpectedEOF}
	got, err := streamThroughIdle(ctx, body, time.Hour)

	if err == nil {
		t.Fatal("expected the upstream error to surface on errc, got nil (silent truncation reintroduced!)")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF to propagate, got %v", err)
	}
	if errors.Is(err, ErrIdleTimeout) {
		t.Errorf("a genuine upstream error must not be relabelled ErrIdleTimeout: %v", err)
	}
	// The event that rode in with the error must still be delivered.
	if len(got) != 1 {
		t.Errorf("expected the 1 event delivered with the error, got %d", len(got))
	}
}

// TestIdleTimeoutReadDirectError white-box-checks the n>0 branch's error
// provenance: only our own idle close (fired AND a closed-conn error) is
// suppressed; every other error propagates with its real identity — including
// a genuine io.ErrUnexpectedEOF that merely races the timer firing.
func TestIdleTimeoutReadDirectError(t *testing.T) {
	t.Parallel()

	t.Run("genuine_error_propagates_not_fired", func(t *testing.T) {
		r := newIdleTimeoutReader(context.Background(), &truncReader{err: io.ErrUnexpectedEOF}, nil, time.Hour).(*idleTimeoutReader)
		defer r.Close()
		n, err := r.Read(make([]byte, 64))
		if n <= 0 {
			t.Fatalf("expected the bytes delivered, got n=%d", n)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("expected io.ErrUnexpectedEOF propagated with bytes, got %v", err)
		}
	})

	t.Run("idle_close_suppressed_only_for_closed_conn", func(t *testing.T) {
		// net.ErrClosed with fired=true IS our idle close (only a local close
		// produces it) → suppressed so data wins; the n==0 read surfaces the
		// sentinel.
		r := newIdleTimeoutReader(context.Background(), &truncReader{err: net.ErrClosed}, nil, time.Hour).(*idleTimeoutReader)
		defer r.Close()
		r.fired.Store(true)
		n, err := r.Read(make([]byte, 64))
		if n <= 0 {
			t.Fatalf("expected the bytes delivered, got n=%d", n)
		}
		if err != nil {
			t.Errorf("our idle-close (net.ErrClosed, fired) must be suppressed on n>0, got %v", err)
		}
	})

	t.Run("genuine_error_with_fired_still_propagates", func(t *testing.T) {
		// The provenance bug: a genuine io.ErrUnexpectedEOF (truncated chunked
		// stream) that arrives in the SAME window the timer fired. fired alone
		// must NOT suppress it — io.ErrUnexpectedEOF is not a closed-conn error
		// — so it propagates with its real identity, never relabeled as the
		// retryable idle timeout (which would mask a content-integrity failure).
		r := newIdleTimeoutReader(context.Background(), &truncReader{err: io.ErrUnexpectedEOF}, nil, time.Hour).(*idleTimeoutReader)
		defer r.Close()
		r.fired.Store(true)
		n, err := r.Read(make([]byte, 64))
		if n <= 0 {
			t.Fatalf("expected the bytes delivered, got n=%d", n)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("genuine io.ErrUnexpectedEOF racing the timer must propagate, got %v", err)
		}
		if errors.Is(err, ErrIdleTimeout) {
			t.Errorf("genuine error must not be relabeled ErrIdleTimeout: %v", err)
		}
	})
}

// TestIdleTimeoutZeroIsInfinite verifies the BAML idle_timeout_ms=0 parity:
// a non-positive timeout returns the body unwrapped (no bound, zero cost).
func TestIdleTimeoutZeroIsInfinite(t *testing.T) {
	t.Parallel()

	body := newControlledReader(nil)
	defer body.Close()

	for _, d := range []time.Duration{0, -1} {
		got := newIdleTimeoutReader(context.Background(), body, nil, d)
		if got != io.ReadCloser(body) {
			t.Errorf("timeout %v: expected the body returned unwrapped, got a wrapper %T", d, got)
		}
	}
}

// eofReader always returns (0, io.EOF) — models a connection the watchdog has
// already closed, where the next read yields no usable data. Used to drive the
// fired-override branch (which only triggers on an n==0 read).
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
func (eofReader) Close() error             { return nil }

// TestIdleTimeoutPrefersCtxErr verifies that on a no-data (n==0) read after the
// watchdog fired, the wrapper surfaces ctx.Err() when the context was
// cancelled (so a client cancel is not mislabelled a provider stall) and
// ErrIdleTimeout otherwise — overriding the underlying io.EOF either way.
func TestIdleTimeoutPrefersCtxErr(t *testing.T) {
	t.Parallel()

	t.Run("ctx_cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		r := &idleTimeoutReader{
			ctx:       ctx,
			r:         eofReader{},
			closer:    io.NopCloser(nil),
			interrupt: func() {},
			timeout:   time.Hour,
		}
		r.fired.Store(true)
		cancel()

		n, err := r.Read(make([]byte, 8))
		if n != 0 {
			t.Errorf("expected n=0 on fired override, got %d", n)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		if errors.Is(err, ErrIdleTimeout) {
			t.Errorf("ctx cancel must not be mislabelled ErrIdleTimeout: %v", err)
		}
	})

	t.Run("ctx_live", func(t *testing.T) {
		r := &idleTimeoutReader{
			ctx:       context.Background(),
			r:         eofReader{},
			closer:    io.NopCloser(nil),
			interrupt: func() {},
			timeout:   time.Hour,
		}
		r.fired.Store(true)

		n, err := r.Read(make([]byte, 8))
		if n != 0 {
			t.Errorf("expected n=0 on fired override, got %d", n)
		}
		if !errors.Is(err, ErrIdleTimeout) {
			t.Errorf("expected ErrIdleTimeout, got %v", err)
		}
	})
}

// alwaysByteReader returns exactly one byte per Read after sleeping `delay`,
// and IGNORES Close() — it keeps producing bytes even after the watchdog has
// fired and "closed" it. This is the adversary for the data-wins invariant: it
// lets the idle timer fire repeatedly mid-stream while bytes keep arriving, so
// the test can prove the wrapper never surfaces ErrIdleTimeout as long as the
// underlying reader returns n>0.
type alwaysByteReader struct {
	delay  time.Duration
	closes atomic.Int64
}

func (r *alwaysByteReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, nil
}

func (r *alwaysByteReader) Close() error {
	r.closes.Add(1)
	return nil
}

// TestIdleTimeoutNeverFalseKillsAtBoundary hammers the timer race: the reader
// trickles one byte per ~idle-window so the AfterFunc fires repeatedly while
// bytes are still arriving. The data-wins invariant requires that EVERY Read
// returning n>0 delivers its byte with a nil error and the wrapper NEVER
// returns ErrIdleTimeout while bytes keep coming — even though `fired` flips
// during the run. Run under -race -count to shake out the race window.
func TestIdleTimeoutNeverFalseKillsAtBoundary(t *testing.T) {
	t.Parallel()

	const (
		idle  = 200 * time.Microsecond
		delay = 200 * time.Microsecond // right at the boundary
		iters = 2000
	)
	reader := &alwaysByteReader{delay: delay}
	r := newIdleTimeoutReader(context.Background(), reader, nil, idle).(*idleTimeoutReader)
	defer r.Close()

	buf := make([]byte, 1)
	firedDuringRun := false
	for i := 0; i < iters; i++ {
		n, err := r.Read(buf)
		if errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("iter %d: false-kill — ErrIdleTimeout returned for a read that should deliver data", i)
		}
		if n != 1 || err != nil {
			t.Fatalf("iter %d: data lost — got (n=%d, err=%v), want (1, nil)", i, n, err)
		}
		if r.fired.Load() {
			firedDuringRun = true
		}
	}
	// Sanity: the boundary timing should have actually fired the watchdog at
	// least once, otherwise the test isn't exercising the race it claims to.
	if !firedDuringRun {
		t.Log("note: watchdog never fired during the run; boundary timing may be too loose to exercise the race")
	}
}

// TestClassifyIdleTimeoutRetryable verifies the classifier maps ErrIdleTimeout
// to a retryable transport flake at body-read sites (the gated stale-conn
// family is irrelevant; idle timeout is ungated).
func TestClassifyIdleTimeoutRetryable(t *testing.T) {
	t.Parallel()

	te := classifyTransportErr(ErrIdleTimeout, "llmhttp: failed to read response body", false)
	if te == nil {
		t.Fatal("classifyTransportErr returned nil for ErrIdleTimeout (not retryable!)")
	}
	if te.Category != TransportFlakeIdleTimeout {
		t.Errorf("category = %v, want %v", te.Category, TransportFlakeIdleTimeout)
	}
	if !errors.Is(te, ErrTransportFlake) {
		t.Errorf("classified error is not ErrTransportFlake: %v", te)
	}
	if !errors.Is(te, ErrIdleTimeout) {
		t.Errorf("classified error lost ErrIdleTimeout identity: %v", te)
	}
}

func TestParseStreamIdleTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultStreamIdleTimeout},
		{"   ", DefaultStreamIdleTimeout},
		{"0", 0}, // 0 = infinite (preserved)
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"garbage", DefaultStreamIdleTimeout},
		{"-5s", DefaultStreamIdleTimeout}, // negative collapses to default
	}
	for _, tc := range cases {
		if got := ParseStreamIdleTimeout(tc.raw); got != tc.want {
			t.Errorf("ParseStreamIdleTimeout(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestStreamIdleTimeoutClientDefault verifies the constructors install the
// protective default and honour an explicit value (including 0 = infinite).
func TestStreamIdleTimeoutClientDefault(t *testing.T) {
	t.Parallel()

	// Nil StreamIdleTimeout -> protective default, not disabled.
	c := NewClientWithOptions(ClientOptions{})
	if got := c.GetStreamIdleTimeout(); got != DefaultStreamIdleTimeout {
		t.Errorf("nil StreamIdleTimeout: got %v, want default %v", got, DefaultStreamIdleTimeout)
	}

	// Explicit value is used verbatim.
	custom := 42 * time.Second
	c = NewClientWithOptions(ClientOptions{StreamIdleTimeout: &custom})
	if got := c.GetStreamIdleTimeout(); got != custom {
		t.Errorf("explicit StreamIdleTimeout: got %v, want %v", got, custom)
	}

	// Explicit 0 = infinite (honoured, not overridden by the default).
	zero := time.Duration(0)
	c = NewClientWithOptions(ClientOptions{StreamIdleTimeout: &zero})
	if got := c.GetStreamIdleTimeout(); got != 0 {
		t.Errorf("explicit 0 StreamIdleTimeout: got %v, want 0 (infinite)", got)
	}

	// Runtime setter clamps negatives to 0 (infinite).
	c.SetStreamIdleTimeout(-1)
	if got := c.GetStreamIdleTimeout(); got != 0 {
		t.Errorf("SetStreamIdleTimeout(-1): got %v, want 0", got)
	}
}
