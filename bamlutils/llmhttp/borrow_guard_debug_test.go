//go:build llmhttp_debug

package llmhttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// These tests run only under -tags llmhttp_debug and prove the debug-only
// borrow guard actually fires on the two bug classes it targets. In a normal
// (untagged) build the guard hooks are no-ops (borrow_guard.go) and these tests
// are excluded.

func newBorrowDebugServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, body)
	}))
}

// TestBorrowGuardPoisonsReleasedStorage proves the use-after-release guard:
// a body view retained across Release must read poison bytes, not the original
// (now-recycled) content.
func TestBorrowGuardPoisonsReleasedStorage(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hello"}}]}`
	server := newBorrowDebugServer(t, body)
	defer server.Close()

	c := withFastClient(t, server.Client())
	resp, err := c.ExecuteBorrowed(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.borrowed() {
		t.Fatal("expected a borrowed response on the fasthttp lane")
	}

	// Deliberately retain a body view past Release — the use-after-release bug.
	view := resp.BodyString()
	if view != body {
		t.Fatalf("pre-release view = %q, want %q", view, body)
	}

	resp.Release()

	if view == body {
		t.Error("debug guard did NOT poison released storage — the retained view still reads the original body")
	}
	for i := 0; i < len(view); i++ {
		if view[i] != poisonByte {
			t.Fatalf("released storage byte %d = %#x, want poison %#x (poisoning incomplete)", i, view[i], poisonByte)
		}
	}
}

// TestBorrowGuardDetectsMissedRelease proves the missed-release finalizer:
// dropping a borrowed response without Release must increment missedReleaseCount
// when it is garbage-collected.
func TestBorrowGuardDetectsMissedRelease(t *testing.T) {
	const body = `{"choices":[{"message":{"content":"hello"}}]}`
	server := newBorrowDebugServer(t, body)
	defer server.Close()

	c := withFastClient(t, server.Client())

	before := missedReleaseCount.Load()

	// Acquire a borrowed response and drop it WITHOUT Release inside a nested
	// scope so no stack slot keeps it reachable.
	func() {
		resp, err := c.ExecuteBorrowed(context.Background(), &Request{URL: server.URL, Method: "POST", Body: `{}`}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.borrowed() {
			t.Fatal("expected a borrowed response on the fasthttp lane")
		}
		_ = resp.BodyString()
		// resp goes out of scope here, unreleased.
	}()

	// Drive GC until the finalizer reports the missed release (bounded wait).
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		runtime.Gosched()
		if missedReleaseCount.Load() > before {
			return // detected — guard works
		}
		if time.Now().After(deadline) {
			t.Fatal("debug guard did NOT detect the missed release (finalizer never reported)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
