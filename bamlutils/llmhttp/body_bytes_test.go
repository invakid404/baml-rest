package llmhttp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"unsafe"
)

// TestExecuteNetHTTPPopulatesBodyBytes asserts the net/http unary success
// path exposes caller-owned BodyBytes whose backing array Body aliases (the
// zero-copy contract), while the fasthttp lane keeps BodyBytes nil and an
// owned Body string. The orchestrator branches on BodyBytes to pick the byte
// extractor, so this routing invariant must hold per backend.
func TestExecuteNetHTTPPopulatesBodyBytes(t *testing.T) {
	const responseJSON = `{"choices":[{"message":{"content":"Hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()
	req := &Request{URL: server.URL, Method: "POST", Body: `{}`}

	t.Run("nethttp", func(t *testing.T) {
		c := NewClientWithOptions(ClientOptions{Mode: ClientModeNetHTTP})
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer resp.Release()
		if resp.BodyBytes() == nil {
			t.Fatal("net/http unary success must populate BodyBytes")
		}
		if string(resp.BodyBytes()) != responseJSON {
			t.Errorf("BodyBytes = %q, want %q", resp.BodyBytes(), responseJSON)
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("Body = %q, want %q", resp.BodyString(), responseJSON)
		}
		// BodyString must be a zero-copy view over BodyBytes.
		if unsafe.StringData(resp.BodyString()) != unsafe.SliceData(resp.BodyBytes()) {
			t.Error("BodyString must alias BodyBytes backing array (zero-copy)")
		}
	})

	t.Run("fasthttp", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer resp.Release()
		// The fasthttp lane leaves BodyBytes nil (exposeBytes=false) so the
		// orchestrator falls back to the string extractor for this lane,
		// regardless of borrow vs owned. Execute returns an owned copy here.
		if resp.BodyBytes() != nil {
			t.Errorf("fasthttp lane must leave BodyBytes nil, got %d bytes", len(resp.BodyBytes()))
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("Body = %q, want %q", resp.BodyString(), responseJSON)
		}
	})
}

// TestExecuteBorrowedContract exercises the consumer-managed borrow returned
// by ExecuteBorrowed across both fasthttp lanes and net/http: the body is
// correct before Release, BodyBytes stays nil on the fasthttp lanes (extractor
// routing invariant) and non-nil on net/http, Release is idempotent, and the
// borrowed fasthttp accessors fail closed (empty) after Release while the owned
// net/http response keeps its accessors valid (Release is a no-op for it).
func TestExecuteBorrowedContract(t *testing.T) {
	const responseJSON = `{"choices":[{"message":{"content":"Hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()
	req := &Request{URL: server.URL, Method: "POST", Body: `{}`}

	// fasthttp buffered lane: background ctx + nil onSuccess.
	t.Run("fasthttp-buffered", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		resp, err := c.ExecuteBorrowed(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.BodyBytes() != nil {
			t.Errorf("fasthttp borrow must keep BodyBytes nil, got %d bytes", len(resp.BodyBytes()))
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("BodyString = %q, want %q", resp.BodyString(), responseJSON)
		}
		resp.Release()
		resp.Release() // idempotent
		if resp.BodyString() != "" {
			t.Errorf("BodyString after Release = %q, want empty", resp.BodyString())
		}
		if resp.BodyBytes() != nil {
			t.Error("BodyBytes after Release must be nil")
		}
	})

	// fasthttp streamed-header lane: non-nil onSuccess selects it.
	t.Run("fasthttp-streamed", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		var fired bool
		resp, err := c.ExecuteBorrowed(context.Background(), req, func() { fired = true })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fired {
			t.Error("onSuccess should have fired on the streamed lane")
		}
		if resp.BodyBytes() != nil {
			t.Errorf("fasthttp borrow must keep BodyBytes nil, got %d bytes", len(resp.BodyBytes()))
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("BodyString = %q, want %q", resp.BodyString(), responseJSON)
		}
		if err := resp.Close(); err != nil {
			t.Errorf("Close returned %v, want nil", err)
		}
		if resp.BodyString() != "" {
			t.Errorf("BodyString after Close = %q, want empty", resp.BodyString())
		}
	})

	// net/http lane: the body is already owned even via ExecuteBorrowed
	// (borrowed()==false), so Release is a true no-op and the accessors stay
	// valid afterward; BodyBytes is exposed.
	t.Run("nethttp", func(t *testing.T) {
		c := NewClientWithOptions(ClientOptions{Mode: ClientModeNetHTTP})
		resp, err := c.ExecuteBorrowed(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.borrowed() {
			t.Fatal("net/http lane must be owned, not borrowed")
		}
		if resp.BodyBytes() == nil {
			t.Fatal("net/http unary success must populate BodyBytes")
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("BodyString = %q, want %q", resp.BodyString(), responseJSON)
		}
		resp.Release()
		// Owned: Release is a no-op for the accessors.
		if string(resp.BodyBytes()) != responseJSON {
			t.Errorf("owned BodyBytes after Release = %q, want %q", resp.BodyBytes(), responseJSON)
		}
	})
}

// TestReleaseOwnedVsBorrowed pins the owned/no-op vs borrowed Release contract:
// Release must NOT invalidate an owned response's body (so `defer resp.Release()`
// is safe for Execute / net/http results), but MUST return a borrowed
// response's pooled storage and clear its now-dangling view.
func TestReleaseOwnedVsBorrowed(t *testing.T) {
	const responseJSON = `{"choices":[{"message":{"content":"Hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()
	req := &Request{URL: server.URL, Method: "POST", Body: `{}`}

	// Owned net/http Execute response: accessors stay valid after Release.
	t.Run("owned-nethttp-survives-release", func(t *testing.T) {
		c := NewClientWithOptions(ClientOptions{Mode: ClientModeNetHTTP})
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.borrowed() {
			t.Fatal("Execute must return an owned response")
		}
		resp.Release()
		resp.Release() // idempotent
		if resp.BodyString() != responseJSON {
			t.Errorf("owned BodyString after Release = %q, want %q", resp.BodyString(), responseJSON)
		}
		if string(resp.BodyBytes()) != responseJSON {
			t.Errorf("owned BodyBytes after Release = %q, want %q", resp.BodyBytes(), responseJSON)
		}
	})

	// Owned fasthttp Execute response (own() copied out of the pool): body still
	// valid after Release, BodyBytes still nil (routing invariant).
	t.Run("owned-fasthttp-survives-release", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.borrowed() {
			t.Fatal("Execute must return an owned response")
		}
		resp.Release()
		if resp.BodyString() != responseJSON {
			t.Errorf("owned BodyString after Release = %q, want %q", resp.BodyString(), responseJSON)
		}
		if resp.BodyBytes() != nil {
			t.Error("fasthttp owned lane must keep BodyBytes nil")
		}
	})

	// Borrowed fasthttp ExecuteBorrowed response: Release returns the pooled
	// storage and clears the view; double-release stays safe.
	t.Run("borrowed-fasthttp-releases-storage", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		resp, err := c.ExecuteBorrowed(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.borrowed() {
			t.Fatal("fasthttp ExecuteBorrowed must return a borrowed response")
		}
		if resp.BodyString() != responseJSON {
			t.Errorf("borrowed BodyString = %q, want %q", resp.BodyString(), responseJSON)
		}
		resp.Release()
		if resp.borrowed() {
			t.Error("Release must clear borrowed pooled storage (fastResp/drainBuf)")
		}
		if resp.BodyString() != "" {
			t.Errorf("borrowed BodyString after Release = %q, want empty", resp.BodyString())
		}
		resp.Release() // idempotent / double-release-safe
	})
}

// TestExecuteOwnedSurvivesPoolReuse pins the safe-default contract: the owned
// Response returned by Execute on the fasthttp buffered lane keeps a valid body
// even after subsequent requests churn the fasthttp response pool — i.e. own()
// really copied out of the pooled buffer rather than aliasing it.
func TestExecuteOwnedSurvivesPoolReuse(t *testing.T) {
	const responseJSON = `{"choices":[{"message":{"content":"Hello world"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, responseJSON)
	}))
	defer server.Close()
	req := &Request{URL: server.URL, Method: "POST", Body: `{}`}

	c := withFastClient(t, server.Client())
	resp, err := c.Execute(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Release()
	owned := resp.BodyString()

	// Churn the pool: many more requests whose responses recycle the same
	// pooled fasthttp.Response buffers the first owned copy was taken from.
	for i := 0; i < 50; i++ {
		r2, err := c.ExecuteBorrowed(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("churn request %d failed: %v", i, err)
		}
		r2.Release()
	}

	if owned != responseJSON {
		t.Errorf("owned body corrupted by pool reuse: %q, want %q", owned, responseJSON)
	}
	if resp.BodyString() != responseJSON {
		t.Errorf("owned BodyString corrupted by pool reuse: %q, want %q", resp.BodyString(), responseJSON)
	}
}

// TestBytesToBodyStringZeroCopy proves the net/http unary success path no
// longer pays a whole-body []byte->string copy: bytesToBodyString must
// allocate nothing regardless of body size, and the returned string must
// share BodyBytes' backing array. This is the allocation contract that lets
// llmhttp.Response expose both Body (string) and BodyBytes ([]byte) for free.
func TestBytesToBodyStringZeroCopy(t *testing.T) {
	for _, size := range []int{0, 1, 1 << 10, 256 << 10} {
		body := bytes.Repeat([]byte("x"), size)
		allocs := testing.AllocsPerRun(1000, func() {
			s := bytesToBodyString(body)
			// Touch the result so the compiler can't elide the call.
			if len(s) != size {
				t.Fatalf("len mismatch: got %d, want %d", len(s), size)
			}
		})
		if allocs != 0 {
			t.Errorf("size=%d: bytesToBodyString allocated %v times, want 0 (whole-body copy regressed)", size, allocs)
		}
	}
}

// TestBytesToBodyStringAliases confirms Body and BodyBytes share the same
// backing array on the net/http path — the zero-copy view documented on
// Response.Body. (Empty bodies legitimately yield a nil/empty string with no
// shared pointer, so they are excluded.)
func TestBytesToBodyStringAliases(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s := bytesToBodyString(body)
	if s != string(body) {
		t.Fatalf("content mismatch: %q vs %q", s, string(body))
	}
	if unsafe.StringData(s) != unsafe.SliceData(body) {
		t.Errorf("expected Body to alias BodyBytes backing array (zero-copy), but pointers differ")
	}
}
