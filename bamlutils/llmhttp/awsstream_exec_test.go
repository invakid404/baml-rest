package llmhttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
)

// encodeAWSStreamFrame produces wire bytes for a single AWS
// event-stream frame using the SDK encoder. Reused across tests so
// fixtures track any future SDK encoding changes automatically.
func encodeAWSStreamFrame(t *testing.T, headers map[string]string, payload []byte) []byte {
	t.Helper()
	var hs eventstream.Headers
	for name, value := range headers {
		hs.Set(name, eventstream.StringValue(value))
	}
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{
		Headers: hs,
		Payload: payload,
	}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// TestExecuteAWSStreamSuccess pins the happy path: a server emits a
// short sequence of AWS event-stream frames, ExecuteAWSStream wires up
// the *awsstream.Decoder, and the caller iterates Next() to EOF.
func TestExecuteAWSStreamSuccess(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(encodeAWSStreamFrame(t, map[string]string{
		":message-type": "event",
		":event-type":   "messageStart",
		":content-type": "application/json",
	}, []byte(`{"role":"assistant"}`)))
	buf.Write(encodeAWSStreamFrame(t, map[string]string{
		":message-type": "event",
		":event-type":   "contentBlockDelta",
		":content-type": "application/json",
	}, []byte(`{"delta":{"text":"hello"}}`)))
	buf.Write(encodeAWSStreamFrame(t, map[string]string{
		":message-type": "event",
		":event-type":   "messageStop",
		":content-type": "application/json",
	}, []byte(`{"stopReason":"end_turn"}`)))
	wireBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", AWSStreamContentType)
		w.WriteHeader(200)
		w.Write(wireBody)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL:    server.URL,
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       AWSStreamContentType,
		},
		Body: `{}`,
	})
	if err != nil {
		t.Fatalf("ExecuteAWSStream: %v", err)
	}
	defer resp.Close()

	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Events == nil {
		t.Fatal("Events decoder is nil")
	}

	var got []string
	for {
		evt, err := resp.Events.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, evt.Type)
	}
	want := []string{"messageStart", "contentBlockDelta", "messageStop"}
	if len(got) != len(want) {
		t.Fatalf("decoded %d events (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExecuteAWSStreamRejectsWrongContentType pins the
// content-type guard: a Bedrock-shaped request whose mock proxy
// returns JSON instead of event-stream must surface as a diagnostic
// error (not silently feed JSON into the decoder, which would produce
// confusing framing errors downstream).
func TestExecuteAWSStreamRejectsWrongContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"output":"oops"}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL: server.URL, Method: "POST", Body: `{}`,
	})
	if err == nil {
		t.Fatal("expected error on wrong content type")
	}
	if !strings.Contains(err.Error(), "unexpected Content-Type") {
		t.Errorf("error = %q, want it to mention unexpected Content-Type", err.Error())
	}
}

// TestExecuteAWSStreamRejectsMissingContentType pins that a 200 with
// no Content-Type at all fails closed. A misconfigured upstream / a
// proxy that strips headers is exactly the case where silently
// decoding the body would produce mysterious downstream failures.
func TestExecuteAWSStreamRejectsMissingContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Explicitly avoid auto-setting Content-Type from body sniffing.
		w.Header()["Content-Type"] = nil
		w.WriteHeader(200)
		io.WriteString(w, "anything")
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL: server.URL, Method: "POST", Body: `{}`,
	})
	if err == nil {
		t.Fatal("expected error on missing content type")
	}
	if !strings.Contains(err.Error(), "missing Content-Type") {
		t.Errorf("error = %q, want it to mention missing Content-Type", err.Error())
	}
}

// TestExecuteAWSStreamHTTPError pins that 4xx/5xx responses surface
// as *HTTPError with the body included, matching ExecuteStream and
// Execute. The worker classifier turns these into provider_error.
func TestExecuteAWSStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"message":"rate limited"}`)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL: server.URL, Method: "POST", Body: `{}`,
	})
	if err == nil {
		t.Fatal("expected HTTPError")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error type = %T, want *HTTPError", err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", httpErr.StatusCode)
	}
	if !strings.Contains(httpErr.Body, "rate limited") {
		t.Errorf("Body = %q, want it to contain rate limit text", httpErr.Body)
	}
}

// TestExecuteAWSStreamSigningHeadersForwarded pins that AWSAuth-driven
// SigV4 signing runs on the rewritten URL and the resulting headers
// reach the wire. Mirrors the call-path Execute test for bedrock,
// scoped to the streaming transport.
func TestExecuteAWSStreamSigningHeadersForwarded(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var (
		sawAuth     bool
		sawAmzDate  bool
		sawAmzSha   bool
		sawAlgo     bool
		seenURLPath string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURLPath = r.URL.Path
		auth := r.Header.Get("Authorization")
		sawAuth = auth != ""
		sawAlgo = strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/")
		sawAmzDate = r.Header.Get("X-Amz-Date") != ""
		sawAmzSha = r.Header.Get("X-Amz-Content-Sha256") != ""

		w.Header().Set("Content-Type", AWSStreamContentType)
		w.WriteHeader(200)
		// Empty event-stream body: dec.Next() will return io.EOF
		// immediately, which is fine for this test — we only care
		// about the request headers.
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL:     server.URL + "/model/test/converse-stream",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
		AWSAuth: &AWSAuthConfig{
			Region:      "us-east-1",
			Service:     "bedrock",
			Credentials: awsStaticCredsForTest{},
			NowFunc:     func() time.Time { return pinnedTime },
		},
	})
	if err != nil {
		t.Fatalf("ExecuteAWSStream: %v", err)
	}
	defer resp.Close()

	if !sawAuth {
		t.Error("Authorization header missing")
	}
	if !sawAlgo {
		t.Error("Authorization header missing AWS4-HMAC-SHA256/pinned-credential prefix")
	}
	if !sawAmzDate {
		t.Error("X-Amz-Date header missing")
	}
	if !sawAmzSha {
		t.Error("X-Amz-Content-Sha256 header missing")
	}
	if !strings.HasSuffix(seenURLPath, "/converse-stream") {
		t.Errorf("URL path = %q, want suffix /converse-stream", seenURLPath)
	}
}

// awsStaticCredsForTest is a deterministic credential source used by
// the streaming Sigv4 forwarding test. The mock server doesn't verify
// the signature against IAM; the test only pins that signed headers
// are present.
type awsStaticCredsForTest struct{}

func (awsStaticCredsForTest) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return aws.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRETEXAMPLE",
	}, nil
}

// TestExecuteAWSStreamNilGuards pins the defensive nil-arg behaviour.
// Symmetric to ExecuteStream / Execute so caller mistakes surface as
// a Go error rather than a nil-pointer panic deep in the SDK.
func TestExecuteAWSStreamNilGuards(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.ExecuteAWSStream(context.Background(), &Request{}); err == nil {
		t.Error("nil client: expected error")
	}
	if _, err := DefaultClient.ExecuteAWSStream(context.Background(), nil); err == nil {
		t.Error("nil request: expected error")
	}
}

// TestExecuteAWSStreamCloseIsIdempotent pins the Close contract used
// by callers via defer.
func TestExecuteAWSStreamCloseIsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", AWSStreamContentType)
		w.WriteHeader(200)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	resp, err := client.ExecuteAWSStream(context.Background(), &Request{
		URL: server.URL, Method: "POST", Body: `{}`,
	})
	if err != nil {
		t.Fatalf("ExecuteAWSStream: %v", err)
	}
	// First Close releases the body; a second Close must not panic.
	resp.Close()
	resp.Close()
}

// Ensure the awsstream package import is non-decorative.
var _ = awsstream.NewDecoder
