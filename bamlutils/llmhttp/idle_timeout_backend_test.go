package llmhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hangingSSEServer returns an httptest.Server that writes the given complete
// SSE events, flushes them, then goes silent — blocking until the client tears
// down the connection (its request context is cancelled when the idle watchdog
// severs the conn). This drives the REAL backend close paths end-to-end
// through ExecuteStream: net/http resp.Body.Close and fasthttp slot.shutdown.
func hangingSSEServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		for _, ev := range events {
			_, _ = io.WriteString(w, ev)
			fl.Flush()
		}
		// Go silent: block until the client disconnects. The idle watchdog
		// closing the body / severing the socket is what unblocks this.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertIdleTimeoutFires drives a hanging SSE stream through ExecuteStream on
// the given backend and asserts the idle timeout surfaces ErrIdleTimeout /
// ErrTransportFlake on Errc (and never a clean io.EOF). The ctx deadline is
// far above the idle window so the SERVER-side idle timeout — not the ctx — is
// what ends the stream.
func assertIdleTimeoutFires(t *testing.T, mode ClientMode) {
	t.Helper()
	srv := hangingSSEServer(t, "data: {\"v\":1}\n\n", "data: {\"v\":2}\n\n")

	idle := 150 * time.Millisecond
	client := NewClientWithOptions(ClientOptions{
		Mode:              mode,
		StreamIdleTimeout: &idle,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := client.ExecuteStream(ctx, &Request{URL: srv.URL, Method: "POST", Body: "{}"})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	defer resp.Close()

	var got int
	for range resp.Events {
		got++
	}
	streamErr := <-resp.Errc
	elapsed := time.Since(start)

	if streamErr == nil {
		t.Fatal("expected a terminal error from the idle timeout, got nil (silent truncation!)")
	}
	if !errors.Is(streamErr, ErrIdleTimeout) {
		t.Errorf("terminal error is not ErrIdleTimeout: %v", streamErr)
	}
	if !errors.Is(streamErr, ErrTransportFlake) {
		t.Errorf("terminal error is not ErrTransportFlake (not retryable): %v", streamErr)
	}
	if errors.Is(streamErr, io.EOF) {
		t.Errorf("terminal error must not be io.EOF: %v", streamErr)
	}
	// The idle window (150ms) must be what fired, well under the 10s ctx.
	if elapsed > 5*time.Second {
		t.Errorf("stream took %v to terminate; expected the ~150ms idle window to fire", elapsed)
	}
	t.Logf("mode=%v: received %d events, idle timeout fired after %v, terminal err=%v", mode, got, elapsed, streamErr)
}

// TestExecuteStreamIdleTimeoutNetHTTP exercises the real net/http backend
// close path (resp.Body.Close interrupts the parked SSE read).
func TestExecuteStreamIdleTimeoutNetHTTP(t *testing.T) {
	t.Parallel()
	assertIdleTimeoutFires(t, ClientModeNetHTTP)
}

// TestExecuteStreamIdleTimeoutFastHTTP exercises the real fasthttp backend
// close path (slot.shutdown severs the captured socket).
func TestExecuteStreamIdleTimeoutFastHTTP(t *testing.T) {
	t.Parallel()
	assertIdleTimeoutFires(t, ClientModeFastHTTP)
}

// TestExecuteStreamProgressingStreamNetHTTP is the negative control on the
// real net/http backend: a server that keeps flushing events within the idle
// window then ends cleanly must complete with no error and all events.
func TestExecuteStreamProgressingStreamNetHTTP(t *testing.T) {
	t.Parallel()

	const n = 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < n; i++ {
			_, _ = io.WriteString(w, "data: {\"v\":1}\n\n")
			fl.Flush()
			time.Sleep(20 * time.Millisecond) // 20ms gap << 300ms idle window
		}
		// Clean end (handler returns -> response body EOF).
	}))
	t.Cleanup(srv.Close)

	idle := 300 * time.Millisecond
	client := NewClientWithOptions(ClientOptions{
		Mode:              ClientModeNetHTTP,
		StreamIdleTimeout: &idle,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ExecuteStream(ctx, &Request{URL: srv.URL, Method: "POST", Body: "{}"})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	defer resp.Close()

	var got int
	for range resp.Events {
		got++
	}
	if streamErr := <-resp.Errc; streamErr != nil {
		t.Fatalf("idle timeout fired on a progressing stream: %v", streamErr)
	}
	if got != n {
		t.Errorf("expected %d events, got %d", n, got)
	}
}
