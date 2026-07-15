package llmhttp

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// heartbeatProbeBody is an io.ReadCloser that records, on its FIRST Read, whether
// the heartbeat had already fired. The exact executor reads the body via
// io.ReadAll AFTER it invokes onSuccess, so a correct implementation always finds
// the heartbeat already fired at the first body read.
type heartbeatProbeBody struct {
	data             []byte
	off              int
	firstRead        bool
	firedAtFirstRead *bool
	heartbeatFired   *bool
}

func (b *heartbeatProbeBody) Read(p []byte) (int, error) {
	if !b.firstRead {
		b.firstRead = true
		*b.firedAtFirstRead = *b.heartbeatFired
	}
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

func (b *heartbeatProbeBody) Close() error { return nil }

// heartbeatProbeRT is a RoundTripper that returns a response whose body probes
// the heartbeat ordering. It also captures the outbound request so the test can
// assert the heartbeat callback never mutates the wire request.
type heartbeatProbeRT struct {
	status           int
	respBody         []byte
	firedAtFirstRead *bool
	heartbeatFired   *bool

	gotMethod string
	gotURL    string
	gotHeader http.Header
	gotBody   []byte
}

func (rt *heartbeatProbeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.gotMethod = req.Method
	rt.gotURL = req.URL.String()
	rt.gotHeader = req.Header.Clone()
	if req.Body != nil {
		rt.gotBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: rt.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: &heartbeatProbeBody{
			data:             rt.respBody,
			firedAtFirstRead: rt.firedAtFirstRead,
			heartbeatFired:   rt.heartbeatFired,
		},
		Request: req,
	}, nil
}

func heartbeatProbeReq() *ExactAttemptRequest {
	return &ExactAttemptRequest{
		Method:      "POST",
		URL:         "http://api.example.test/v1/chat/completions",
		Headers:     []HeaderField{{"Content-Type", "application/json"}, {"Authorization", "Bearer sk-fake"}},
		Body:        []byte(`{"model":"gpt-4o"}`),
		BodyPresent: true,
	}
}

// TestExactHeartbeatFiresOnceBefore2xxBody pins the pre-canary compatibility
// requirement: ExecuteWithHeartbeat fires onSuccess EXACTLY ONCE, on a 2xx, the
// instant the status line is observed and BEFORE the body is buffered — so a
// slow/large body can never stall pool hung-detection — and it never mutates the
// wire request (the callback takes no arguments; the body/headers reach the
// RoundTripper verbatim).
func TestExactHeartbeatFiresOnceBefore2xxBody(t *testing.T) {
	firedAtFirstRead := false
	heartbeatFired := false
	rt := &heartbeatProbeRT{
		status:           200,
		respBody:         []byte(`{"ok":true}`),
		firedAtFirstRead: &firedAtFirstRead,
		heartbeatFired:   &heartbeatFired,
	}
	var count int
	onSuccess := func() {
		count++
		heartbeatFired = true
	}

	req := heartbeatProbeReq()
	resp, err := NewExactExecutor(rt).ExecuteWithHeartbeat(context.Background(), req, onSuccess)
	if err != nil {
		t.Fatalf("ExecuteWithHeartbeat: %v", err)
	}
	if count != 1 {
		t.Errorf("heartbeat fired %d times, want exactly 1", count)
	}
	if !firedAtFirstRead {
		t.Error("heartbeat had NOT fired at the first body read; it must fire on 2xx headers BEFORE the body is buffered")
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` {
		t.Errorf("response = %d %q, want 200 {\"ok\":true}", resp.StatusCode, resp.Body)
	}

	// The heartbeat never mutates the request: the RoundTripper saw the exact
	// method / URL / headers / body the plan carried.
	if rt.gotMethod != "POST" || rt.gotURL != "http://api.example.test/v1/chat/completions" {
		t.Errorf("wire method/url = %q %q, want POST + verbatim URL", rt.gotMethod, rt.gotURL)
	}
	if got := rt.gotHeader.Get("Authorization"); got != "Bearer sk-fake" {
		t.Errorf("wire Authorization = %q, want verbatim Bearer sk-fake", got)
	}
	if string(rt.gotBody) != `{"model":"gpt-4o"}` {
		t.Errorf("wire body = %q, want verbatim plan body", rt.gotBody)
	}
}

// TestExactHeartbeatNotFiredOnNon2xx proves onSuccess never fires on a non-2xx:
// a non-2xx is ordinary response DATA, not the 2xx-headers liveness signal the
// pool hung-detector consumes.
func TestExactHeartbeatNotFiredOnNon2xx(t *testing.T) {
	firedAtFirstRead := false
	heartbeatFired := false
	rt := &heartbeatProbeRT{
		status:           500,
		respBody:         []byte(`{"error":"boom"}`),
		firedAtFirstRead: &firedAtFirstRead,
		heartbeatFired:   &heartbeatFired,
	}
	var count int
	onSuccess := func() { count++; heartbeatFired = true }

	resp, err := NewExactExecutor(rt).ExecuteWithHeartbeat(context.Background(), heartbeatProbeReq(), onSuccess)
	if err != nil {
		t.Fatalf("ExecuteWithHeartbeat: %v", err)
	}
	if count != 0 {
		t.Errorf("heartbeat fired %d times on a non-2xx, want 0", count)
	}
	if resp.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500 (non-2xx returned as data)", resp.StatusCode)
	}
}

// TestExactExecuteNilHeartbeatUnchanged proves Execute (and ExecuteWithHeartbeat
// with a nil callback) is byte-identical to the pre-heartbeat behaviour: the
// response comes back intact and no callback is required.
func TestExactExecuteNilHeartbeatUnchanged(t *testing.T) {
	rt := &heartbeatProbeRT{
		status:           200,
		respBody:         []byte(`{"ok":true}`),
		firedAtFirstRead: new(bool),
		heartbeatFired:   new(bool),
	}
	resp, err := NewExactExecutor(rt).Execute(context.Background(), heartbeatProbeReq())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` {
		t.Errorf("response = %d %q, want 200 {\"ok\":true}", resp.StatusCode, resp.Body)
	}
}
