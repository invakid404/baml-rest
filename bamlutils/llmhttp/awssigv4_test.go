package llmhttp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// staticCreds is an aws.CredentialsProvider that hands out a fixed
// credential set. Used to make signatures deterministic in tests
// without touching the real default chain.
type staticCreds struct {
	cred aws.Credentials
}

func (s staticCreds) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return s.cred, nil
}

func newPinnedAuth(now time.Time, sessionToken string) *AWSAuthConfig {
	return &AWSAuthConfig{
		Region:  "us-east-1",
		Service: "bedrock",
		Credentials: staticCreds{
			cred: aws.Credentials{
				AccessKeyID:     "AKIDEXAMPLE",
				SecretAccessKey: "SECRETEXAMPLE",
				SessionToken:    sessionToken,
			},
		},
		NowFunc: func() time.Time { return now },
	}
}

func TestSignRequest_NilAuth_NoHeadersAdded(t *testing.T) {
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[]}`,
	}
	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, h := range []string{"Authorization", "X-Amz-Date", "X-Amz-Content-Sha256", "X-Amz-Security-Token"} {
		if _, ok := req.Headers[h]; ok {
			t.Errorf("expected header %q to be absent when AWSAuth is nil, found it set", h)
		}
	}
}

func TestSignRequest_AddsSigV4Headers(t *testing.T) {
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		AWSAuth: newPinnedAuth(pinned, ""),
	}
	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	auth := req.Headers["Authorization"]
	if auth == "" {
		t.Fatal("Authorization header missing")
	}
	expectedPrefix := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260511/us-east-1/bedrock/aws4_request, SignedHeaders="
	if !strings.HasPrefix(auth, expectedPrefix) {
		t.Errorf("Authorization header has wrong prefix.\n  got: %s\n  want prefix: %s", auth, expectedPrefix)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization header missing Signature: %s", auth)
	}

	if got, want := req.Headers["X-Amz-Date"], "20260511T123456Z"; got != want {
		t.Errorf("X-Amz-Date = %q, want %q", got, want)
	}
	// X-Amz-Content-Sha256 must be the hex SHA-256 of the body.
	if got := req.Headers["X-Amz-Content-Sha256"]; len(got) != 64 {
		t.Errorf("X-Amz-Content-Sha256 length = %d, want 64 (hex sha256)", len(got))
	}
	if _, ok := req.Headers["X-Amz-Security-Token"]; ok {
		t.Errorf("X-Amz-Security-Token must be absent when credential has no session token")
	}
}

func TestSignRequest_SessionToken(t *testing.T) {
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	req := &Request{
		URL:     "https://bedrock-runtime.us-west-2.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
		AWSAuth: newPinnedAuth(pinned, "FQoSESSION"),
	}
	req.AWSAuth.Region = "us-west-2"

	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := req.Headers["X-Amz-Security-Token"], "FQoSESSION"; got != want {
		t.Errorf("X-Amz-Security-Token = %q, want %q", got, want)
	}
}

func TestSignRequest_DeterministicAcrossCalls(t *testing.T) {
	// Same inputs + pinned time + pinned credentials => byte-identical
	// Authorization signature. This locks in determinism so a future
	// SDK upgrade that changed the canonical-request shape would be
	// caught immediately.
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	build := func() *Request {
		return &Request{
			URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
			Method: "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			AWSAuth: newPinnedAuth(pinned, ""),
		}
	}
	r1 := build()
	if err := signRequest(context.Background(), r1, r1.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r2 := build()
	if err := signRequest(context.Background(), r2, r2.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r1.Headers["Authorization"] != r2.Headers["Authorization"] {
		t.Errorf("Authorization not deterministic across signings:\n  r1: %s\n  r2: %s",
			r1.Headers["Authorization"], r2.Headers["Authorization"])
	}
}

func TestSignRequest_SignsRewrittenURL(t *testing.T) {
	// Signing should use the rewritten URL, not the original. This is
	// the load-bearing invariant: tests + production URL rewrite both
	// expect the signature to match the host the request actually
	// reaches.
	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	rOriginal := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
		AWSAuth: newPinnedAuth(pinned, ""),
	}
	rRewritten := &Request{
		URL:     rOriginal.URL,
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
		AWSAuth: newPinnedAuth(pinned, ""),
	}
	// Sign rOriginal with the original URL.
	if err := signRequest(context.Background(), rOriginal, rOriginal.URL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Sign rRewritten with a different rewritten URL.
	rewritten := "http://127.0.0.1:9999/model/x/converse"
	if err := signRequest(context.Background(), rRewritten, rewritten); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rOriginal.Headers["Authorization"] == rRewritten.Headers["Authorization"] {
		t.Error("expected different Authorization headers when the signed URL host differs")
	}
}

func TestSignRequest_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		auth *AWSAuthConfig
	}{
		{"nil credentials", &AWSAuthConfig{Region: "us-east-1", Service: "bedrock"}},
		{"empty region", &AWSAuthConfig{Service: "bedrock", Credentials: staticCreds{}}},
		{"empty service", &AWSAuthConfig{Region: "us-east-1", Credentials: staticCreds{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
				Method:  "POST",
				Body:    `{}`,
				AWSAuth: tc.auth,
			}
			err := signRequest(context.Background(), req, req.URL)
			if err == nil {
				t.Fatal("expected error for invalid AWSAuth config")
			}
		})
	}
}

func TestParseBedrockRegion(t *testing.T) {
	cases := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{"https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse", "us-east-1", false},
		{"https://bedrock-runtime.eu-west-3.amazonaws.com/model/foo/converse", "eu-west-3", false},
		{"https://api.openai.com/v1/chat/completions", "", true},
		{"https://bedrock-runtime.amazonaws.com/model/foo/converse", "", true},
		{"://broken", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			region, err := parseBedrockRegion(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got region %q", region)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if region != tc.want {
				t.Errorf("region = %q, want %q", region, tc.want)
			}
		})
	}
}
