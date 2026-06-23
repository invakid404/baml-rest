package llmhttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestEndpointOverride_E2E_NonStreaming pins the end-to-end contract
// for the non-streaming Bedrock dispatch with an `endpoint_url`
// override: a request whose BAML-emitted URL points at the public
// Bedrock host gets rewritten to a localhost mock, signed for the
// configured region, and dispatched via the standard llmhttp.Execute
// path. The mock server asserts the rewritten host shows up on the
// wire AND the SigV4 Authorization header references the configured
// region — together those are what makes operator-facing overrides
// (LocalStack, VPC endpoints, FIPS, China, GovCloud) actually work.
//
// This stitches together the three pieces the codegen generator wires
// up at build time: AttachBedrockAuthForClient (dispatch), URL rewrite
// (inside AttachBedrockAuthWithOptions), and signRequest (signs the
// rewritten URL). A regression in any of them surfaces here as either
// a wrong host on the wire or a wrong credential-scope substring.
func TestEndpointOverride_E2E_NonStreaming(t *testing.T) {
	var (
		gotHost      string
		gotPath      string
		gotAuth      string
		gotAmzDate   string
		gotAmzSha256 string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAmzDate = r.Header.Get("X-Amz-Date")
		gotAmzSha256 = r.Header.Get("X-Amz-Content-Sha256")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"output":{"message":{"content":[{"text":"ok"}]}}}`)
	}))
	defer mock.Close()

	mockURL, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock URL: %v", err)
	}

	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
	}

	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		EndpointURL: mock.URL,
		Region:      "us-east-1",
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	// Pin time + credentials onto the attached metadata so the
	// signature is deterministic and doesn't pull from the host's
	// AWS configuration.
	if req.AWSAuth == nil {
		t.Fatal("expected AWSAuth attached after dispatch")
	}
	req.AWSAuth.NowFunc = func() time.Time { return pinned }
	req.AWSAuth.Credentials = staticCreds{cred: aws.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRETEXAMPLE",
	}}

	client := NewClient(mock.Client())
	resp, err := client.Execute(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer resp.Release()
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	if gotHost != mockURL.Host {
		t.Errorf("Host on wire = %q, want override host %q (URL rewrite did not flow through)", gotHost, mockURL.Host)
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse" {
		t.Errorf("Path on wire = %q, want path preserved across override", gotPath)
	}
	if !strings.Contains(gotAuth, "/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization credential scope did not reference us-east-1/bedrock; got: %s", gotAuth)
	}
	if gotAmzDate != "20260511T123456Z" {
		t.Errorf("X-Amz-Date = %q, want 20260511T123456Z", gotAmzDate)
	}
	if len(gotAmzSha256) != 64 {
		t.Errorf("X-Amz-Content-Sha256 length = %d, want 64 hex chars", len(gotAmzSha256))
	}
}

// TestEndpointOverride_E2E_Streaming pins the same E2E shape for the
// streaming path: codegen mutates /converse → /converse-stream BEFORE
// auth attach, the override rewrites scheme+host while preserving the
// streaming path, and ExecuteAWSStream dispatches to the mock with the
// signed Authorization. The mock returns a minimal AWS event-stream
// frame so ExecuteAWSStream's content-type + decoder gate both run.
func TestEndpointOverride_E2E_Streaming(t *testing.T) {
	// Build a minimal valid event-stream frame so the decoder accepts
	// the response. A messageStop event is enough — we are not
	// asserting on the decoded content, only on the wire shape that
	// the override + sign produce.
	var buf bytes.Buffer
	buf.Write(encodeAWSStreamFrame(t, map[string]string{
		":message-type": "event",
		":event-type":   "messageStop",
		":content-type": "application/json",
	}, []byte(`{"stopReason":"end_turn"}`)))
	wireBody := buf.Bytes()

	var (
		gotHost   string
		gotPath   string
		gotAuth   string
		gotAccept string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", AWSStreamContentType)
		w.WriteHeader(200)
		w.Write(wireBody)
	}))
	defer mock.Close()

	mockURL, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock URL: %v", err)
	}

	// Mirror what emitBedrockStreamPostProcessFor emits at build
	// time: BAML produces /converse, codegen mutates to
	// /converse-stream, sets Accept, then dispatches the auth attach.
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
	}
	req.URL = strings.Replace(req.URL, "/converse", "/converse-stream", 1)
	req.Headers["Accept"] = AWSStreamContentType

	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		EndpointURL: mock.URL,
		Region:      "us-east-1",
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	req.AWSAuth.NowFunc = func() time.Time { return pinned }
	req.AWSAuth.Credentials = staticCreds{cred: aws.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRETEXAMPLE",
	}}

	client := NewClient(mock.Client())
	resp, err := client.ExecuteAWSStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ExecuteAWSStream: %v", err)
	}
	defer resp.Close()
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	// Drain at least one event so the decoder runs end-to-end against
	// the mock response — a regression in the wire format would surface
	// as a Next() error rather than a silent success.
	if _, err := resp.Events.Next(); err != nil && err != io.EOF {
		t.Fatalf("Events.Next: %v", err)
	}

	if gotHost != mockURL.Host {
		t.Errorf("Host on wire = %q, want override host %q", gotHost, mockURL.Host)
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse-stream" {
		t.Errorf("Path on wire = %q, want streaming path preserved across override", gotPath)
	}
	if !strings.Contains(gotAuth, "/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization credential scope did not reference us-east-1/bedrock; got: %s", gotAuth)
	}
	if gotAccept != AWSStreamContentType {
		t.Errorf("Accept header = %q, want %q (must survive auth attach)", gotAccept, AWSStreamContentType)
	}
}

// TestEndpointOverride_E2E_RegionFallback pins the env-fallback path
// end-to-end: when `.baml options.region` is unset (parser produces
// empty Region), the dispatcher passes "" through to
// AttachBedrockAuthForClient, which routes to AttachBedrockAuthWithOptions
// (because endpoint_url is set), and the helper resolves region from
// AWS_REGION. The signed credential scope must reflect the env value.
func TestEndpointOverride_E2E_RegionFallback(t *testing.T) {
	t.Setenv("AWS_REGION", "ap-southeast-2")

	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"output":{"message":{"content":[{"text":"ok"}]}}}`)
	}))
	defer mock.Close()

	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
	}
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		EndpointURL: mock.URL,
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	pinned := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	req.AWSAuth.NowFunc = func() time.Time { return pinned }
	req.AWSAuth.Credentials = staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}}

	client := NewClient(mock.Client())
	if _, err := client.Execute(context.Background(), req, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(gotAuth, "/20260511/ap-southeast-2/bedrock/aws4_request") {
		t.Errorf("Authorization scope did not pick up AWS_REGION fallback (ap-southeast-2); got: %s", gotAuth)
	}
}

// TestStaticCreds_E2E_NonStreaming pins the full static-credentials
// dispatch end-to-end on the non-streaming path. Mirror of
// TestEndpointOverride_E2E_NonStreaming with one twist: credentials
// flow from BedrockClientAuthOptions.Credentials through the resolver,
// not via test-pinned overrides. The mock asserts the static AKID
// appears in the wire-side Authorization credential scope AND the
// configured session token appears in X-Amz-Security-Token — together
// proving the resolver's static provider made it all the way to
// signRequest and that the SDK signer's session-token copy-back hits
// the wire.
//
// This is the canonical end-to-end pin for #254 item 2 (call path).
// Mirror coverage for the streaming path lives in
// TestStaticCreds_E2E_Streaming below.
func TestStaticCreds_E2E_NonStreaming(t *testing.T) {
	var (
		gotAuth        string
		gotSecurityTok string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSecurityTok = r.Header.Get("X-Amz-Security-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"output":{"message":{"content":[{"text":"ok"}]}}}`)
	}))
	defer mock.Close()

	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
	}
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		ClientName:  "StaticE2EClient",
		EndpointURL: mock.URL,
		Region:      "us-east-1",
		Credentials: BedrockCredentialSelector{
			AccessKeyID:            "STATIC_TEST_ACCESS_KEY",
			AccessKeyIDPresent:     true,
			SecretAccessKey:        "STATIC_TEST_SECRET_KEY",
			SecretAccessKeyPresent: true,
			// Session token is configured here too so the
			// non-streaming and streaming E2E tests pin the same
			// header set — STS short-lived credentials are a
			// realistic call-path shape, not a streaming-only
			// shape.
			SessionToken:        "STATIC_TEST_SESSION_TOKEN",
			SessionTokenPresent: true,
		},
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	req.AWSAuth.NowFunc = func() time.Time { return pinned }

	client := NewClient(mock.Client())
	if _, err := client.Execute(context.Background(), req, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(gotAuth, "Credential=STATIC_TEST_ACCESS_KEY/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization must carry static access key in credential scope; got: %s", gotAuth)
	}
	if gotSecurityTok != "STATIC_TEST_SESSION_TOKEN" {
		t.Errorf("X-Amz-Security-Token on the wire = %q, want %q (session token must flow through resolver -> SigV4 -> Execute)", gotSecurityTok, "STATIC_TEST_SESSION_TOKEN")
	}
}

// TestStaticCreds_E2E_Streaming pins the same static-credentials
// dispatch end-to-end on the streaming path. Mirrors
// TestEndpointOverride_E2E_Streaming but configures the credential
// selector instead of supplying staticCreds via test-pinned override.
// Asserts both the AKID-in-credential-scope and the session token on
// the wire so the streaming path has call-path parity for session
// token coverage.
func TestStaticCreds_E2E_Streaming(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(encodeAWSStreamFrame(t, map[string]string{
		":message-type": "event",
		":event-type":   "messageStop",
		":content-type": "application/json",
	}, []byte(`{"stopReason":"end_turn"}`)))
	wireBody := buf.Bytes()

	var (
		gotAuth        string
		gotSecurityTok string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSecurityTok = r.Header.Get("X-Amz-Security-Token")
		w.Header().Set("Content-Type", AWSStreamContentType)
		w.WriteHeader(200)
		w.Write(wireBody)
	}))
	defer mock.Close()

	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
	}
	req.URL = strings.Replace(req.URL, "/converse", "/converse-stream", 1)
	req.Headers["Accept"] = AWSStreamContentType

	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		ClientName:  "StaticE2EStreamingClient",
		EndpointURL: mock.URL,
		Region:      "us-east-1",
		Credentials: BedrockCredentialSelector{
			AccessKeyID:            "STATIC_TEST_ACCESS_KEY",
			AccessKeyIDPresent:     true,
			SecretAccessKey:        "STATIC_TEST_SECRET_KEY",
			SecretAccessKeyPresent: true,
			SessionToken:           "STATIC_TEST_SESSION_TOKEN",
			SessionTokenPresent:    true,
		},
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	req.AWSAuth.NowFunc = func() time.Time { return pinned }

	client := NewClient(mock.Client())
	resp, err := client.ExecuteAWSStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ExecuteAWSStream: %v", err)
	}
	defer resp.Close()
	if _, err := resp.Events.Next(); err != nil && err != io.EOF {
		t.Fatalf("Events.Next: %v", err)
	}

	if !strings.Contains(gotAuth, "Credential=STATIC_TEST_ACCESS_KEY/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization must carry static access key in credential scope on the streaming path; got: %s", gotAuth)
	}
	if gotSecurityTok != "STATIC_TEST_SESSION_TOKEN" {
		t.Errorf("X-Amz-Security-Token on the streaming-path wire = %q, want %q (session token must flow through resolver -> SigV4 -> ExecuteAWSStream)", gotSecurityTok, "STATIC_TEST_SESSION_TOKEN")
	}
}

// TestEndpointOverride_E2E_DefaultEndpointFallthrough pins the
// no-override case end-to-end: when neither endpoint_url nor region is
// configured, AttachBedrockAuthForClient routes to
// MaybeAttachBedrockAuth's URL-pattern detection. The request URL is
// not rewritten — but we can't dispatch to the real Bedrock host
// without credentials, so this test asserts the AWSAuth attach itself
// (region pulled from URL host) and leaves dispatch coverage to the
// override-rewrites-to-mock tests above.
func TestEndpointOverride_E2E_DefaultEndpointFallthrough(t *testing.T) {
	req := &Request{
		URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method: "POST",
		Body:   `{}`,
	}
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	if req.AWSAuth == nil {
		t.Fatal("expected AWSAuth attached via URL-pattern detection")
	}
	if req.AWSAuth.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1 (URL-pattern path)", req.AWSAuth.Region)
	}
	if req.URL != "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse" {
		t.Errorf("URL must NOT be rewritten on the no-override fallthrough path; got %q", req.URL)
	}
}
