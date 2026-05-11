package llmhttp

import (
	"context"
	"errors"
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

// TestSignRequest_PurgesStaleSigV4HeadersAcrossReuse pins the
// CR follow-up (PR #244): when the same *Request is reused for two
// sequential signings — first with a session-token credential, then
// with a no-token credential — none of the prior SigV4 headers may
// survive into the second sign. The most concerning leak is
// X-Amz-Security-Token: SignHTTP only writes it when the new
// credential carries a session token, so without an explicit purge
// the stale token rides along on the new request.
//
// Also covers the case-variant leak: a header set with non-canonical
// casing (e.g. "x-amz-date") must be purged so it can't coexist
// alongside the canonical "X-Amz-Date" the signer writes.
//
// llmhttp.Request.Headers is a plain map[string]string (not http.Header),
// so case canonicalization is the caller's job — which is exactly the
// motivation for the EqualFold walk in signRequest.
func TestSignRequest_PurgesStaleSigV4HeadersAcrossReuse(t *testing.T) {
	pinned1 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	pinned2 := time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC)

	req := &Request{
		URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			// Deliberate case variant on a SigV4-owned header — the
			// purge must catch it regardless of casing so it can't
			// coexist with the canonical key the signer writes below.
			"x-amz-date": "stale-case-variant",
		},
		Body:    `{"messages":[]}`,
		AWSAuth: newPinnedAuth(pinned1, "FIRST-SESSION-TOKEN"),
	}
	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("first sign: %v", err)
	}
	// Sanity: first sign produced a session-token header (the credential
	// has one) — this is the value that MUST be purged on the next sign.
	if req.Headers["X-Amz-Security-Token"] != "FIRST-SESSION-TOKEN" {
		t.Fatalf("first sign did not produce expected session token; got Headers=%+v", req.Headers)
	}
	// Sanity: the stale case-variant entry is gone after the first
	// sign too (the purge runs every sign, not just on reuse).
	if _, ok := req.Headers["x-amz-date"]; ok {
		t.Errorf("first sign did not purge case-variant x-amz-date; Headers=%+v", req.Headers)
	}

	firstAuth := req.Headers["Authorization"]
	firstDate := req.Headers["X-Amz-Date"]
	if firstAuth == "" || firstDate == "" {
		t.Fatalf("first sign missing Authorization / X-Amz-Date; Headers=%+v", req.Headers)
	}

	// Now re-sign the same *Request with a credential that has no
	// session token. The purge must remove the stale token; the new
	// Authorization + X-Amz-Date must reflect the second sign.
	req.AWSAuth = newPinnedAuth(pinned2, "")

	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("second sign: %v", err)
	}

	// Walk Headers case-insensitively to catch any X-Amz-Security-Token
	// variant that might have leaked. EqualFold against every SigV4 key
	// here matches the production-code purge invariant.
	for k, v := range req.Headers {
		if strings.EqualFold(k, "X-Amz-Security-Token") {
			t.Errorf("stale session-token header survived: Headers[%q] = %q (entire Headers: %+v)", k, v, req.Headers)
		}
	}
	// The second sign's headers must replace the first sign's values.
	if req.Headers["Authorization"] == firstAuth {
		t.Errorf("Authorization did not change between signs (stale value persisted): %q", firstAuth)
	}
	if got, want := req.Headers["X-Amz-Date"], "20260511T130000Z"; got != want {
		t.Errorf("X-Amz-Date = %q, want %q (second sign's pinned time)", got, want)
	}
	if got := req.Headers["X-Amz-Date"]; got == firstDate {
		t.Errorf("X-Amz-Date did not change between signs (stale value persisted): %q", firstDate)
	}
	// Caller-owned non-SigV4 header must survive the purge — only
	// sigV4OwnedHeaders are dropped, everything else passes through.
	if req.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type should survive purge; Headers=%+v", req.Headers)
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

// TestMaybeAttachBedrockAuth_Detection pins the URL-pattern detection
// path that the codegen call branch relies on. The codegen emits an
// unconditional MaybeAttachBedrockAuth(ctx, req) for every provider's
// generated _buildCallRequest, so this test must cover both the
// attach-on-bedrock-host case AND the no-op case for every URL shape
// that isn't a standard Bedrock runtime host.
//
// Scope note: only the default Bedrock runtime endpoint pattern
// (bedrock-runtime.<region>.amazonaws.com) attaches. FIPS / China /
// GovCloud / endpoint_url override shapes are intentionally not
// covered here — that's #243 PR 4 territory.
func TestMaybeAttachBedrockAuth_Detection(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantAttach bool
		wantRegion string
	}{
		{
			name:       "standard bedrock-runtime us-east-1",
			url:        "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
			wantAttach: true,
			wantRegion: "us-east-1",
		},
		{
			name:       "standard bedrock-runtime eu-west-3",
			url:        "https://bedrock-runtime.eu-west-3.amazonaws.com/model/foo/converse",
			wantAttach: true,
			wantRegion: "eu-west-3",
		},
		{
			// Non-bedrock provider URL — must not attach. Locks in the
			// "every other provider pays nothing" contract that gates
			// the unconditional codegen emit.
			name:       "openai chat completions",
			url:        "https://api.openai.com/v1/chat/completions",
			wantAttach: false,
		},
		{
			// Bedrock CONTROL-plane host (no `-runtime` suffix on the
			// service label). The signing path must not attach to
			// control-plane requests — they use a different IAM scope
			// and operation set. Locking this distinction in case the
			// pattern is ever loosened.
			name:       "bedrock control plane",
			url:        "https://bedrock.us-east-1.amazonaws.com/foundation-models",
			wantAttach: false,
		},
		{
			// Malformed URL — must not panic and must not attach.
			name:       "malformed URL",
			url:        "://broken",
			wantAttach: false,
		},
		{
			name:       "empty URL",
			url:        "",
			wantAttach: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{URL: tc.url, Method: "POST", Body: "{}"}
			if err := MaybeAttachBedrockAuth(context.Background(), req); err != nil {
				t.Fatalf("MaybeAttachBedrockAuth: unexpected error: %v", err)
			}
			if tc.wantAttach {
				if req.AWSAuth == nil {
					t.Fatal("expected AWSAuth attached, got nil")
				}
				if req.AWSAuth.Region != tc.wantRegion {
					t.Errorf("Region = %q, want %q", req.AWSAuth.Region, tc.wantRegion)
				}
				if req.AWSAuth.Service != "bedrock" {
					t.Errorf("Service = %q, want %q", req.AWSAuth.Service, "bedrock")
				}
				if req.AWSAuth.Credentials == nil {
					t.Error("expected Credentials provider to be non-nil")
				}
			} else {
				if req.AWSAuth != nil {
					t.Errorf("expected AWSAuth nil for %q, got %+v", tc.url, req.AWSAuth)
				}
			}
		})
	}
}

// TestMaybeAttachBedrockAuth_PreRewriteRegionFlowsIntoSignature pins
// the load-bearing invariant for the codegen call branch + llmhttp URL
// rewrite interaction:
//
//   - The codegen calls MaybeAttachBedrockAuth on the BAML-emitted URL
//     (the standard bedrock-runtime.<region>.amazonaws.com), which
//     locks the AWSAuth.Region into the request.
//   - llmhttp then rewrites the URL to whatever the operator's
//     URL-rewrite rules point at (typically a mock/proxy host).
//   - signRequest signs the REWRITTEN URL but uses the AWSAuth.Region
//     from pre-rewrite — so the signature's credential scope still
//     references the original AWS region.
//
// Without this invariant, mock-based tests and proxy-fronted
// deployments would either fail signature validation or sign for the
// wrong region. The integration test exercises the full orchestrator
// stack with manually-injected AWSAuth; this test exercises the
// codegen-equivalent attach path on its own.
func TestMaybeAttachBedrockAuth_PreRewriteRegionFlowsIntoSignature(t *testing.T) {
	req := &Request{
		URL:     "https://bedrock-runtime.eu-west-1.amazonaws.com/model/foo/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[]}`,
	}
	if err := MaybeAttachBedrockAuth(context.Background(), req); err != nil {
		t.Fatalf("MaybeAttachBedrockAuth: %v", err)
	}
	if req.AWSAuth == nil {
		t.Fatal("expected AWSAuth attached")
	}
	if req.AWSAuth.Region != "eu-west-1" {
		t.Fatalf("Region = %q, want eu-west-1 (extracted pre-rewrite from bedrock URL)", req.AWSAuth.Region)
	}

	// Replace the default credential provider + clock with deterministic
	// fixtures so the resulting signature is predictable and so we
	// don't depend on the host's AWS configuration. The Region from
	// MaybeAttachBedrockAuth must survive this override.
	pinned := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	req.AWSAuth.Credentials = staticCreds{
		cred: aws.Credentials{
			AccessKeyID:     "AKIDEXAMPLE",
			SecretAccessKey: "SECRETEXAMPLE",
		},
	}
	req.AWSAuth.NowFunc = func() time.Time { return pinned }

	// Sign with a rewritten mock URL — simulating llmhttp's
	// resolveRequestURL step. The credential scope in the Authorization
	// header must still reference the original bedrock URL's region.
	rewritten := "http://127.0.0.1:9999/model/foo/converse"
	if err := signRequest(context.Background(), req, rewritten); err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	auth := req.Headers["Authorization"]
	if auth == "" {
		t.Fatal("Authorization header missing after signRequest")
	}
	if !strings.Contains(auth, "/20260511/eu-west-1/bedrock/aws4_request") {
		t.Errorf("Authorization credential scope did not reference pre-rewrite region eu-west-1/bedrock.\n  got: %s", auth)
	}
}

// TestDefaultAWSCredentialProvider_RetriesAfterFailure pins the
// CR follow-up (PR #244): a transient first-call failure from the
// AWS config loader must NOT poison the cache. The next call has to
// retry the loader.
//
// The previous shape used sync.Once and cached both the provider and
// the loader's error — a single canceled request context (or any
// transient LoadDefaultConfig failure) would permanently disable
// Bedrock auth for the worker. This test substitutes the loader via
// defaultAWSConfigLoader so first-call-fail / second-call-succeed is
// deterministic without driving the real AWS SDK chain.
//
// Concurrency note: the test resets the cache state under the same
// mutex the production code uses, then restores it on cleanup so
// adjacent tests sharing the package-global cache aren't perturbed.
func TestDefaultAWSCredentialProvider_RetriesAfterFailure(t *testing.T) {
	// Save and restore the loader + cache so this test is hermetic.
	savedLoader := defaultAWSConfigLoader
	defaultAWSCredsMu.Lock()
	savedCreds := defaultAWSCreds
	defaultAWSCreds = nil
	defaultAWSCredsMu.Unlock()
	t.Cleanup(func() {
		defaultAWSConfigLoader = savedLoader
		defaultAWSCredsMu.Lock()
		defaultAWSCreds = savedCreds
		defaultAWSCredsMu.Unlock()
	})

	// First call: loader returns an error. The provider must surface
	// the error and must not cache anything.
	calls := 0
	wantErr := errors.New("simulated config load failure")
	defaultAWSConfigLoader = func(ctx context.Context) (aws.Config, error) {
		calls++
		return aws.Config{}, wantErr
	}
	if _, err := DefaultAWSCredentialProvider(context.Background()); err == nil {
		t.Fatal("first call: expected loader error, got nil")
	} else if !errors.Is(err, wantErr) {
		t.Fatalf("first call: error = %v, want wantErr (%v)", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("first call: expected loader invoked once, got %d", calls)
	}

	// Defensive: confirm the failure did not populate the cache.
	defaultAWSCredsMu.Lock()
	leakedCache := defaultAWSCreds
	defaultAWSCredsMu.Unlock()
	if leakedCache != nil {
		t.Errorf("first-call failure leaked into cache: %+v", leakedCache)
	}

	// Second call: loader succeeds. Caller must get a non-nil provider.
	defaultAWSConfigLoader = func(ctx context.Context) (aws.Config, error) {
		calls++
		return aws.Config{Credentials: staticCreds{}}, nil
	}
	got, err := DefaultAWSCredentialProvider(context.Background())
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("second call: expected non-nil provider")
	}
	if calls != 2 {
		t.Fatalf("second call: expected loader invoked twice total, got %d", calls)
	}

	// Third call: loader stays mounted but the success cache must
	// short-circuit, so calls stays at 2. Locks in the "successful
	// loads are cached" half of the contract.
	got2, err := DefaultAWSCredentialProvider(context.Background())
	if err != nil {
		t.Fatalf("third call: unexpected error: %v", err)
	}
	if got2 == nil {
		t.Fatal("third call: expected non-nil provider")
	}
	if calls != 2 {
		t.Errorf("third call: expected cache hit (calls=2), got loader invocations=%d", calls)
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
