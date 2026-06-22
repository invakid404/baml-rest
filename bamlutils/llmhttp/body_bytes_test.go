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
		if resp.BodyBytes == nil {
			t.Fatal("net/http unary success must populate BodyBytes")
		}
		if string(resp.BodyBytes) != responseJSON {
			t.Errorf("BodyBytes = %q, want %q", resp.BodyBytes, responseJSON)
		}
		if resp.Body != responseJSON {
			t.Errorf("Body = %q, want %q", resp.Body, responseJSON)
		}
		// Body must be a zero-copy view over BodyBytes.
		if unsafe.StringData(resp.Body) != unsafe.SliceData(resp.BodyBytes) {
			t.Error("Body must alias BodyBytes backing array (zero-copy)")
		}
	})

	t.Run("fasthttp", func(t *testing.T) {
		c := withFastClient(t, server.Client())
		resp, err := c.Execute(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// fasthttp response storage is pooled/released, so its body is an
		// owned copy and BodyBytes stays nil — the orchestrator falls back to
		// the string extractor for this lane.
		if resp.BodyBytes != nil {
			t.Errorf("fasthttp lane must leave BodyBytes nil, got %d bytes", len(resp.BodyBytes))
		}
		if resp.Body != responseJSON {
			t.Errorf("Body = %q, want %q", resp.Body, responseJSON)
		}
	})
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
