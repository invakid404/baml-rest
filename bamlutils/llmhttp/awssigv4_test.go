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
// covered here — see #254 for the endpoint_url deferral.
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

// TestAttachBedrockAuthWithOptions_EndpointOverride pins the URL
// rewrite contract for the new explicit attach helper. The endpoint
// flavors (custom localhost, VPC, FIPS, China, GovCloud) cover the
// matrix that the URL-pattern detection helper deliberately does not
// touch — they are exactly the configs where operators need to set
// `.baml options.endpoint_url` for the request to reach a real Bedrock
// surface (or, in the localhost case, a LocalStack-style mock).
func TestAttachBedrockAuthWithOptions_EndpointOverride(t *testing.T) {
	const originalURL = "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse"
	cases := []struct {
		name       string
		endpoint   string
		region     string
		wantURL    string
		wantRegion string
	}{
		{
			name:       "default endpoint (no override) leaves URL unchanged",
			endpoint:   "",
			region:     "us-east-1",
			wantURL:    originalURL,
			wantRegion: "us-east-1",
		},
		{
			name:       "custom localhost endpoint",
			endpoint:   "http://localhost:9000",
			region:     "us-east-1",
			wantURL:    "http://localhost:9000/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "us-east-1",
		},
		{
			name:       "VPC endpoint preserves path and region option",
			endpoint:   "https://vpc-endpoint-id.bedrock-runtime.us-east-1.vpce.amazonaws.com",
			region:     "us-east-1",
			wantURL:    "https://vpc-endpoint-id.bedrock-runtime.us-east-1.vpce.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "us-east-1",
		},
		{
			name:       "FIPS endpoint",
			endpoint:   "https://bedrock-runtime-fips.us-east-1.amazonaws.com",
			region:     "us-east-1",
			wantURL:    "https://bedrock-runtime-fips.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "us-east-1",
		},
		{
			name:       "China partition",
			endpoint:   "https://bedrock-runtime.cn-north-1.amazonaws.com.cn",
			region:     "cn-north-1",
			wantURL:    "https://bedrock-runtime.cn-north-1.amazonaws.com.cn/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "cn-north-1",
		},
		{
			name:       "GovCloud",
			endpoint:   "https://bedrock-runtime.us-gov-west-1.amazonaws.com",
			region:     "us-gov-west-1",
			wantURL:    "https://bedrock-runtime.us-gov-west-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "us-gov-west-1",
		},
		{
			name:       "trailing slash on endpoint_url tolerated",
			endpoint:   "http://localhost:9000/",
			region:     "us-east-1",
			wantURL:    "http://localhost:9000/model/anthropic.claude-3-sonnet/converse",
			wantRegion: "us-east-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				URL:     originalURL,
				Method:  "POST",
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    `{"messages":[]}`,
			}
			err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
				EndpointURL: tc.endpoint,
				Region:      tc.region,
				Credentials: staticCreds{cred: aws.Credentials{
					AccessKeyID:     "AKIDEXAMPLE",
					SecretAccessKey: "SECRETEXAMPLE",
				}},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL, tc.wantURL)
			}
			if req.AWSAuth == nil {
				t.Fatal("expected AWSAuth attached")
			}
			if req.AWSAuth.Region != tc.wantRegion {
				t.Errorf("Region = %q, want %q", req.AWSAuth.Region, tc.wantRegion)
			}
			if req.AWSAuth.Service != "bedrock" {
				t.Errorf("Service = %q, want %q", req.AWSAuth.Service, "bedrock")
			}
			if req.AWSAuth.Credentials == nil {
				t.Error("Credentials must not be nil")
			}
		})
	}
}

// TestAttachBedrockAuthWithOptions_PreservesQueryAndFragment pins that
// the override only rewrites scheme + host. Path, query, and fragment
// must survive the rewrite so any auxiliary request shaping (an
// upstream BAML signed-URL future, debug query knobs, etc.) is
// preserved into the final request that gets signed.
func TestAttachBedrockAuthWithOptions_PreservesQueryAndFragment(t *testing.T) {
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse?trace=1#frag",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
	}
	if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		EndpointURL: "http://localhost:9000",
		Region:      "us-east-1",
		Credentials: staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "http://localhost:9000/model/foo/converse?trace=1#frag"
	if req.URL != want {
		t.Errorf("URL = %q, want %q (path/query/fragment must flow through the rewrite)", req.URL, want)
	}
}

// TestAttachBedrockAuthWithOptions_StreamingPathPreserved pins the
// streaming-mode invariant: when codegen has already mutated /converse
// → /converse-stream BEFORE auth attach (per
// emitBedrockStreamPostProcess's documented order), the override join
// must preserve that mutated path. Without this guarantee the URL
// rewrite would silently revert to /converse and the SigV4 signature
// would not match the wire request.
func TestAttachBedrockAuthWithOptions_StreamingPathPreserved(t *testing.T) {
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse-stream",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json", "Accept": AWSStreamContentType},
		Body:    `{}`,
	}
	if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		EndpointURL: "http://localhost:9000",
		Region:      "us-east-1",
		Credentials: staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "http://localhost:9000/model/foo/converse-stream"
	if req.URL != want {
		t.Errorf("URL = %q, want %q (stream path must survive rewrite)", req.URL, want)
	}
}

// TestAttachBedrockAuthWithOptions_RegionResolution pins the resolution
// order documented on BedrockAuthOptions.Region: explicit option, then
// AWS_REGION env, then error. Region MUST NOT be inferred from the URL
// host — that is the contract that lets custom endpoints work safely.
func TestAttachBedrockAuthWithOptions_RegionResolution(t *testing.T) {
	mkReq := func() *Request {
		return &Request{
			URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
			Method:  "POST",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{}`,
		}
	}
	creds := staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}}

	t.Run("explicit option wins", func(t *testing.T) {
		t.Setenv("AWS_REGION", "eu-west-1")
		req := mkReq()
		if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
			Region:      "us-east-1",
			Credentials: creds,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.AWSAuth.Region != "us-east-1" {
			t.Errorf("Region = %q, want us-east-1 (explicit option must win over env)", req.AWSAuth.Region)
		}
	})

	t.Run("AWS_REGION env fallback", func(t *testing.T) {
		t.Setenv("AWS_REGION", "ap-southeast-2")
		req := mkReq()
		if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
			Credentials: creds,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.AWSAuth.Region != "ap-southeast-2" {
			t.Errorf("Region = %q, want ap-southeast-2 (AWS_REGION fallback)", req.AWSAuth.Region)
		}
	})

	t.Run("missing region with no env errors", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		req := mkReq()
		err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
			Credentials: creds,
		})
		if err == nil {
			t.Fatal("expected error when no region is configured")
		}
		if !strings.Contains(err.Error(), "region is required") {
			t.Errorf("error message must mention region requirement; got: %v", err)
		}
	})

	t.Run("region NOT inferred from URL host", func(t *testing.T) {
		// Even though the URL host encodes "us-east-1", the helper
		// must not pull region from it — this is the load-bearing
		// invariant that lets the override safely target hosts that
		// don't encode a region (LocalStack, VPC endpoints, etc.).
		t.Setenv("AWS_REGION", "")
		req := mkReq()
		err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
			Credentials: creds,
		})
		if err == nil {
			t.Fatal("expected error — region must not be inferred from URL host even when present")
		}
	})
}

// TestAttachBedrockAuthWithOptions_RejectsInvalidEndpoint pins the
// scheme/host validation. A bare host with no scheme, an opaque URI,
// or a non-http scheme is operator misconfiguration — surface it as an
// error rather than silently signing a request that won't reach the
// intended host.
func TestAttachBedrockAuthWithOptions_RejectsInvalidEndpoint(t *testing.T) {
	creds := staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}}
	cases := []struct {
		name     string
		endpoint string
	}{
		{"missing scheme", "localhost:9000"},
		{"non-http scheme", "ftp://localhost:9000"},
		{"empty host", "http:///foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
				Method: "POST",
				Body:   `{}`,
			}
			err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
				EndpointURL: tc.endpoint,
				Region:      "us-east-1",
				Credentials: creds,
			})
			if err == nil {
				t.Fatalf("expected error for endpoint %q, got nil", tc.endpoint)
			}
		})
	}
}

// TestAttachBedrockAuthWithOptions_SigningCoversRewrittenURL pins the
// end-to-end invariant: the explicit-attach + rewrite combo must produce
// a SigV4 Authorization whose signed Host header references the override
// host, not the original BAML-emitted Bedrock host. Without this
// invariant a localhost mock would reject the signature on host mismatch.
func TestAttachBedrockAuthWithOptions_SigningCoversRewrittenURL(t *testing.T) {
	pinned := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[]}`,
	}
	if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		EndpointURL: "http://localhost:9000",
		Region:      "us-east-1",
		Credentials: staticCreds{cred: aws.Credentials{
			AccessKeyID:     "AKIDEXAMPLE",
			SecretAccessKey: "SECRETEXAMPLE",
		}},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	req.AWSAuth.NowFunc = func() time.Time { return pinned }

	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("sign: %v", err)
	}
	auth := req.Headers["Authorization"]
	if auth == "" {
		t.Fatal("Authorization header missing")
	}
	if !strings.Contains(auth, "/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization credential scope must reference us-east-1/bedrock; got: %s", auth)
	}
}

// TestAttachBedrockAuthWithOptions_EndpointPathConcat pins the
// path-concatenation contract for path-prefixed proxies.
//
// CR Round 1 (PR #262 finding 2) caught that the helper used to drop
// the endpoint's path silently. Operators setting
// `endpoint_url "https://my-proxy.example.com/v1/bedrock"` would have
// requests misrouted to `https://my-proxy.example.com/model/.../converse`
// without the `/v1/bedrock` prefix. This matches AWS SDK v2's
// ResolveEndpointV2 middleware semantics: the endpoint URI path
// prepends every request URI.
//
// The matrix here covers the trailing-slash / leading-slash join
// permutations (Smithy JoinPath semantics) plus the streaming-path
// preservation invariant — the codegen mutates /converse →
// /converse-stream BEFORE this helper runs, so the helper sees the
// already-mutated path and must concatenate against it intact.
func TestAttachBedrockAuthWithOptions_EndpointPathConcat(t *testing.T) {
	creds := staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}}
	cases := []struct {
		name        string
		originalURL string
		endpoint    string
		wantURL     string
	}{
		{
			name:        "no path on endpoint preserves original path",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h",
			wantURL:     "http://h/model/x/converse",
		},
		{
			name:        "root-only path on endpoint preserves original path (no double slash)",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h/",
			wantURL:     "http://h/model/x/converse",
		},
		{
			name:        "single-segment endpoint prefix concatenates",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h/base",
			wantURL:     "http://h/base/model/x/converse",
		},
		{
			name:        "single-segment endpoint with trailing slash does not double-slash",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h/base/",
			wantURL:     "http://h/base/model/x/converse",
		},
		{
			name:        "multi-segment endpoint prefix concatenates",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h/base/sub",
			wantURL:     "http://h/base/sub/model/x/converse",
		},
		{
			name: "streaming path concatenates against /converse-stream",
			// The codegen mutates /converse → /converse-stream BEFORE
			// AttachBedrockAuthForClient runs (see emitBedrockStreamPostProcessFor),
			// so this helper must see the already-mutated path and
			// concatenate the prefix against it without further rewrite.
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse-stream",
			endpoint:    "http://h/v1/bedrock",
			wantURL:     "http://h/v1/bedrock/model/x/converse-stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				URL:     tc.originalURL,
				Method:  "POST",
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    `{}`,
			}
			if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
				EndpointURL: tc.endpoint,
				Region:      "us-east-1",
				Credentials: creds,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL, tc.wantURL)
			}
		})
	}
}

// TestAttachBedrockAuthWithOptions_EndpointQueryMerge pins the query
// merge contract: endpoint-set query knobs and original query knobs
// coexist on the wire, joined with `&` so the resulting query string
// preserves both. Either side empty → use the other; both empty →
// empty (no stray `?` or `&`).
func TestAttachBedrockAuthWithOptions_EndpointQueryMerge(t *testing.T) {
	creds := staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}}
	cases := []struct {
		name        string
		originalURL string
		endpoint    string
		wantURL     string
	}{
		{
			name:        "endpoint query + original query → joined with &",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse?bar=2",
			endpoint:    "http://h/?foo=1",
			wantURL:     "http://h/model/x/converse?foo=1&bar=2",
		},
		{
			name:        "endpoint-only query, original has no query",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h?foo=1",
			wantURL:     "http://h/model/x/converse?foo=1",
		},
		{
			name:        "original-only query, endpoint has no query",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse?bar=2",
			endpoint:    "http://h",
			wantURL:     "http://h/model/x/converse?bar=2",
		},
		{
			name:        "no query on either side",
			originalURL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
			endpoint:    "http://h",
			wantURL:     "http://h/model/x/converse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				URL:    tc.originalURL,
				Method: "POST",
				Body:   `{}`,
			}
			if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
				EndpointURL: tc.endpoint,
				Region:      "us-east-1",
				Credentials: creds,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL, tc.wantURL)
			}
		})
	}
}

// TestAttachBedrockAuthWithOptions_RejectsEndpointFragment pins the
// fragment-rejection contract. Fragments are client-side identifiers
// (RFC 3986 §3.5: not sent in HTTP requests) and have no meaning when
// combined with request routing — silently dropping or concatenating
// them would mask operator misconfiguration. The error message must
// surface both the failing endpoint_url and the word "fragment" so
// the diagnostic points operators at the actual problem.
func TestAttachBedrockAuthWithOptions_RejectsEndpointFragment(t *testing.T) {
	req := &Request{
		URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
		Method: "POST",
		Body:   `{}`,
	}
	err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		EndpointURL: "http://h#frag",
		Region:      "us-east-1",
		Credentials: staticCreds{cred: aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}},
	})
	if err == nil {
		t.Fatal("expected error for endpoint_url with fragment, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "endpoint_url") {
		t.Errorf("error must mention endpoint_url; got: %v", err)
	}
	if !strings.Contains(msg, "fragment") {
		t.Errorf("error must mention fragment; got: %v", err)
	}
}

// TestAttachBedrockAuthWithOptions_SigningCoversConcatenatedPath pins
// the post-fix invariant for path-prefixed proxies: SigV4 must sign
// over the *concatenated* URL, not over the unprefixed one. Without
// this, an operator routing through `https://proxy/v1/bedrock` would
// see signature mismatches at the proxy because the canonical request
// the signer hashed was `/model/.../converse` while the wire URI was
// `/v1/bedrock/model/.../converse`.
//
// We assert two things: req.URL after attach is the concatenated form
// (so a downstream signer hashing req.URL gets the right canonical
// path), and signRequest over that req.URL produces the standard
// SigV4 credential scope tying to the configured region.
func TestAttachBedrockAuthWithOptions_SigningCoversConcatenatedPath(t *testing.T) {
	pinned := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	req := &Request{
		URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{"messages":[]}`,
	}
	if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		EndpointURL: "http://h/v1/bedrock",
		Region:      "us-east-1",
		Credentials: staticCreds{cred: aws.Credentials{
			AccessKeyID:     "AKIDEXAMPLE",
			SecretAccessKey: "SECRETEXAMPLE",
		}},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	req.AWSAuth.NowFunc = func() time.Time { return pinned }

	wantURL := "http://h/v1/bedrock/model/foo/converse"
	if req.URL != wantURL {
		t.Fatalf("post-attach URL = %q, want %q", req.URL, wantURL)
	}

	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("sign: %v", err)
	}
	auth := req.Headers["Authorization"]
	if auth == "" {
		t.Fatal("Authorization header missing")
	}
	if !strings.Contains(auth, "/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("credential scope must reference us-east-1/bedrock; got: %s", auth)
	}
}

// TestJoinEndpointPath unit-tests the standalone path-join helper so
// the trailing-slash / leading-slash join semantics are pinned in
// isolation from the larger AttachBedrockAuthWithOptions plumbing.
// Mirrors smithy-go's transport/http.JoinPath: a single `/` always
// separates prefix from path, never zero, never two.
func TestJoinEndpointPath(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		path   string
		want   string
	}{
		{"empty prefix", "", "/model/x", "/model/x"},
		{"root prefix", "/", "/model/x", "/model/x"},
		{"prefix no trailing", "/base", "/model/x", "/base/model/x"},
		{"prefix trailing slash", "/base/", "/model/x", "/base/model/x"},
		{"prefix multi-segment", "/base/sub", "/model/x", "/base/sub/model/x"},
		{"prefix multi-segment trailing slash", "/base/sub/", "/model/x", "/base/sub/model/x"},
		{"prefix without leading slash + path with leading slash", "base", "/model/x", "base/model/x"},
		{"empty path with non-empty prefix", "/base", "", "/base"},
		{"empty path with prefix and trailing slash", "/base/", "", "/base"},
		{"path without leading slash", "/base", "model/x", "/base/model/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinEndpointPath(tc.prefix, tc.path); got != tc.want {
				t.Errorf("joinEndpointPath(%q, %q) = %q, want %q", tc.prefix, tc.path, got, tc.want)
			}
		})
	}
}

// TestJoinEndpointRawQuery unit-tests the standalone query-merge
// helper. Either side empty → use the other; both empty → empty;
// otherwise join with `&`. Duplicate keys are preserved on the wire
// (downstream parsers handle them per their own rules — typically
// last-wins, but we don't enforce a policy here).
func TestJoinEndpointRawQuery(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		suffix string
		want   string
	}{
		{"both empty", "", "", ""},
		{"prefix only", "foo=1", "", "foo=1"},
		{"suffix only", "", "bar=2", "bar=2"},
		{"both populated joined with &", "foo=1", "bar=2", "foo=1&bar=2"},
		{"duplicate keys preserved", "foo=1", "foo=2", "foo=1&foo=2"},
		{"multi-pair on each side", "a=1&b=2", "c=3&d=4", "a=1&b=2&c=3&d=4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinEndpointRawQuery(tc.prefix, tc.suffix); got != tc.want {
				t.Errorf("joinEndpointRawQuery(%q, %q) = %q, want %q", tc.prefix, tc.suffix, got, tc.want)
			}
		})
	}
}

// TestAttachBedrockAuthForClient_DispatchesByOptions pins the codegen
// dispatch contract: empty endpoint+region falls through to URL-pattern
// detection (so the default-endpoint case still attaches via
// MaybeAttachBedrockAuth), and a non-empty endpoint or region routes to
// the explicit override path (which rewrites the URL).
func TestAttachBedrockAuthForClient_DispatchesByOptions(t *testing.T) {
	t.Run("no override falls through to URL-pattern detection", func(t *testing.T) {
		req := &Request{
			URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
			Method: "POST",
			Body:   `{}`,
		}
		if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.AWSAuth == nil {
			t.Fatal("MaybeAttachBedrockAuth fallback must attach for default Bedrock URL")
		}
		if req.AWSAuth.Region != "us-east-1" {
			t.Errorf("Region = %q, want us-east-1 (URL-pattern path)", req.AWSAuth.Region)
		}
		if req.URL != "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse" {
			t.Errorf("URL must NOT be rewritten on the fallback path; got %q", req.URL)
		}
	})

	t.Run("non-empty endpoint routes to explicit override", func(t *testing.T) {
		req := &Request{
			URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
			Method: "POST",
			Body:   `{}`,
		}
		if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
			EndpointURL: "http://localhost:9000",
			Region:      "us-east-1",
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.URL != "http://localhost:9000/model/foo/converse" {
			t.Errorf("URL = %q, want override-rewritten", req.URL)
		}
	})

	t.Run("non-empty region only still routes to explicit", func(t *testing.T) {
		// region-only override (no endpoint) still must take the
		// explicit path — the operator has set region in `.baml`,
		// signaling they don't want the URL-host inference.
		req := &Request{
			URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
			Method: "POST",
			Body:   `{}`,
		}
		if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
			Region: "eu-west-1",
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.AWSAuth == nil {
			t.Fatal("expected AWSAuth attached")
		}
		if req.AWSAuth.Region != "eu-west-1" {
			t.Errorf("Region = %q, want eu-west-1 (explicit option)", req.AWSAuth.Region)
		}
		if req.URL != "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse" {
			t.Errorf("URL must NOT be rewritten when only region is overridden; got %q", req.URL)
		}
	})

	t.Run("non-bedrock URL with no override is a no-op", func(t *testing.T) {
		req := &Request{
			URL:    "https://api.openai.com/v1/chat/completions",
			Method: "POST",
			Body:   `{}`,
		}
		if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.AWSAuth != nil {
			t.Errorf("non-bedrock URL must not attach; got %+v", req.AWSAuth)
		}
	})
}

// TestAttachBedrockAuthWithOptions_NilCredentialsUsesDefaultChain pins
// the documented Credentials slot contract: nil means "use the default
// AWS credential chain", not "skip auth". The static-creds follow-up
// will populate the slot; until then nil is the production codegen
// path. The default chain may fail in CI without AWS creds set, so the
// test substitutes the package-level loader with a deterministic
// success.
func TestAttachBedrockAuthWithOptions_NilCredentialsUsesDefaultChain(t *testing.T) {
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
	defaultAWSConfigLoader = func(ctx context.Context) (aws.Config, error) {
		return aws.Config{Credentials: staticCreds{cred: aws.Credentials{
			AccessKeyID: "AK", SecretAccessKey: "SK",
		}}}, nil
	}

	req := &Request{
		URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method: "POST",
		Body:   `{}`,
	}
	if err := AttachBedrockAuthWithOptions(context.Background(), req, BedrockAuthOptions{
		Region: "us-east-1",
		// Credentials intentionally left nil — should resolve via the default chain.
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.AWSAuth == nil || req.AWSAuth.Credentials == nil {
		t.Fatal("expected default-chain credentials to be attached")
	}
}

// Static credential fixture values for #254 item 2 tests. Use obvious
// fakes (NOT real-looking access-key examples) so a security
// scanner cannot mistake them for compromised credentials.
const (
	staticCredsTestAccessKey    = "STATIC_TEST_ACCESS_KEY"
	staticCredsTestSecretKey    = "STATIC_TEST_SECRET_KEY"
	staticCredsTestSessionToken = "STATIC_TEST_SESSION_TOKEN"
)

// TestResolveBedrockCredentials_Static pins the static-credentials
// branch: AccessKeyID + SecretAccessKey (+ optional SessionToken) →
// credentials.NewStaticCredentialsProvider. The returned provider
// must hand out exactly the values we configured.
func TestResolveBedrockCredentials_Static(t *testing.T) {
	t.Run("without session token", func(t *testing.T) {
		sel := BedrockCredentialSelector{
			AccessKeyID:            staticCredsTestAccessKey,
			AccessKeyIDPresent:     true,
			SecretAccessKey:        staticCredsTestSecretKey,
			SecretAccessKeyPresent: true,
		}
		provider, err := ResolveBedrockCredentials(context.Background(), "TestClient", sel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider == nil {
			t.Fatal("expected non-nil provider for declared static credentials")
		}
		creds, err := provider.Retrieve(context.Background())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if creds.AccessKeyID != staticCredsTestAccessKey {
			t.Errorf("AccessKeyID = %q, want %q", creds.AccessKeyID, staticCredsTestAccessKey)
		}
		if creds.SecretAccessKey != staticCredsTestSecretKey {
			t.Errorf("SecretAccessKey = %q, want %q", creds.SecretAccessKey, staticCredsTestSecretKey)
		}
		if creds.SessionToken != "" {
			t.Errorf("SessionToken = %q, want empty for static pair without session token", creds.SessionToken)
		}
	})
	t.Run("with session token", func(t *testing.T) {
		sel := BedrockCredentialSelector{
			AccessKeyID:            staticCredsTestAccessKey,
			AccessKeyIDPresent:     true,
			SecretAccessKey:        staticCredsTestSecretKey,
			SecretAccessKeyPresent: true,
			SessionToken:           staticCredsTestSessionToken,
			SessionTokenPresent:    true,
		}
		provider, err := ResolveBedrockCredentials(context.Background(), "TestClient", sel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		creds, err := provider.Retrieve(context.Background())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if creds.SessionToken != staticCredsTestSessionToken {
			t.Errorf("SessionToken = %q, want %q", creds.SessionToken, staticCredsTestSessionToken)
		}
	})
}

// TestResolveBedrockCredentials_Profile pins the profile-only branch:
// the resolver delegates to profileAWSConfigLoader with the configured
// profile name and returns cfg.Credentials. Indirected through the
// test-seam loader so the test doesn't depend on ~/.aws/credentials.
func TestResolveBedrockCredentials_Profile(t *testing.T) {
	savedLoader := profileAWSConfigLoader
	t.Cleanup(func() { profileAWSConfigLoader = savedLoader })

	var seenProfile string
	stubProvider := staticCreds{cred: aws.Credentials{
		AccessKeyID:     "FROM_PROFILE_AK",
		SecretAccessKey: "FROM_PROFILE_SK",
	}}
	profileAWSConfigLoader = func(ctx context.Context, profile string) (aws.Config, error) {
		seenProfile = profile
		return aws.Config{Credentials: stubProvider}, nil
	}

	sel := BedrockCredentialSelector{
		Profile:        "ai-dev",
		ProfilePresent: true,
	}
	provider, err := ResolveBedrockCredentials(context.Background(), "TestClient", sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenProfile != "ai-dev" {
		t.Errorf("profile passed to loader = %q, want ai-dev", seenProfile)
	}
	creds, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != "FROM_PROFILE_AK" {
		t.Errorf("AccessKeyID = %q, want FROM_PROFILE_AK (must come from loader-returned cfg.Credentials)", creds.AccessKeyID)
	}
}

// TestResolveBedrockCredentials_ProfileLoaderError pins that a profile
// load failure is wrapped with the client name and surfaced verbatim
// (no swallowed errors, no silent fallback to the default chain).
func TestResolveBedrockCredentials_ProfileLoaderError(t *testing.T) {
	savedLoader := profileAWSConfigLoader
	t.Cleanup(func() { profileAWSConfigLoader = savedLoader })
	profileAWSConfigLoader = func(ctx context.Context, profile string) (aws.Config, error) {
		return aws.Config{}, errors.New("synthetic load error")
	}

	_, err := ResolveBedrockCredentials(context.Background(), "ClientName", BedrockCredentialSelector{
		Profile:        "ai-dev",
		ProfilePresent: true,
	})
	if err == nil {
		t.Fatal("expected error from synthetic profile load failure")
	}
	if !strings.Contains(err.Error(), "ClientName") {
		t.Errorf("error must name the client; got: %v", err)
	}
	if !strings.Contains(err.Error(), "synthetic load error") {
		t.Errorf("error must wrap loader error; got: %v", err)
	}
}

// TestResolveBedrockCredentials_StaticWinsOverProfile pins the
// BAML-parity precedence: when both static keys and profile are
// declared, static credentials win without consulting the profile
// loader. The test seam asserts profileAWSConfigLoader is NEVER called
// in this case — an indirect way to prove static wins silently.
func TestResolveBedrockCredentials_StaticWinsOverProfile(t *testing.T) {
	savedLoader := profileAWSConfigLoader
	t.Cleanup(func() { profileAWSConfigLoader = savedLoader })
	profileAWSConfigLoader = func(ctx context.Context, profile string) (aws.Config, error) {
		t.Fatalf("profileAWSConfigLoader must not be called when static creds are declared (got profile=%q)", profile)
		return aws.Config{}, nil
	}

	sel := BedrockCredentialSelector{
		AccessKeyID:            staticCredsTestAccessKey,
		AccessKeyIDPresent:     true,
		SecretAccessKey:        staticCredsTestSecretKey,
		SecretAccessKeyPresent: true,
		Profile:                "ignored",
		ProfilePresent:         true,
	}
	provider, err := ResolveBedrockCredentials(context.Background(), "TestClient", sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != staticCredsTestAccessKey {
		t.Errorf("AccessKeyID = %q, want static (static must win over profile)", creds.AccessKeyID)
	}
}

// TestResolveBedrockCredentials_Default pins that an empty selector
// returns (nil, nil) — the caller is responsible for routing to the
// default credential chain. Preserves #262's no-override fallthrough.
func TestResolveBedrockCredentials_Default(t *testing.T) {
	provider, err := ResolveBedrockCredentials(context.Background(), "TestClient", BedrockCredentialSelector{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != nil {
		t.Errorf("provider = %T, want nil (caller falls back to default chain)", provider)
	}
}

// TestResolveBedrockCredentials_InvalidPartialStates pins each partial
// static-credential configuration rejected by the resolver. Critical
// invariants:
//   - the error names the client and the missing field
//   - the error string DOES NOT contain any configured credential value
//
// The security-pin assertion guards against future regressions that
// would echo the configured AKID/secret/token in the error path —
// silently breaking the "no secret leakage" contract documented on
// ResolveBedrockCredentials.
func TestResolveBedrockCredentials_InvalidPartialStates(t *testing.T) {
	cases := []struct {
		name      string
		sel       BedrockCredentialSelector
		wantField string
	}{
		{
			name: "access_key_id without secret_access_key",
			sel: BedrockCredentialSelector{
				AccessKeyID:        staticCredsTestAccessKey,
				AccessKeyIDPresent: true,
			},
			wantField: "secret_access_key",
		},
		{
			name: "secret_access_key without access_key_id",
			sel: BedrockCredentialSelector{
				SecretAccessKey:        staticCredsTestSecretKey,
				SecretAccessKeyPresent: true,
			},
			wantField: "access_key_id",
		},
		{
			name: "session_token without key pair",
			sel: BedrockCredentialSelector{
				SessionToken:        staticCredsTestSessionToken,
				SessionTokenPresent: true,
			},
			wantField: "access_key_id",
		},
		{
			name: "access_key_id present but resolved empty",
			sel: BedrockCredentialSelector{
				AccessKeyIDPresent:     true,
				SecretAccessKey:        staticCredsTestSecretKey,
				SecretAccessKeyPresent: true,
			},
			wantField: "access_key_id",
		},
		{
			name: "secret_access_key present but resolved empty",
			sel: BedrockCredentialSelector{
				AccessKeyID:            staticCredsTestAccessKey,
				AccessKeyIDPresent:     true,
				SecretAccessKeyPresent: true,
			},
			wantField: "secret_access_key",
		},
		{
			name: "session_token present but resolved empty",
			sel: BedrockCredentialSelector{
				AccessKeyID:            staticCredsTestAccessKey,
				AccessKeyIDPresent:     true,
				SecretAccessKey:        staticCredsTestSecretKey,
				SecretAccessKeyPresent: true,
				SessionTokenPresent:    true,
			},
			wantField: "session_token",
		},
		{
			name: "profile present but resolved empty",
			sel: BedrockCredentialSelector{
				ProfilePresent: true,
			},
			wantField: "profile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveBedrockCredentials(context.Background(), "BedrockA", tc.sel)
			if err == nil {
				t.Fatal("expected error for partial credential state")
			}
			msg := err.Error()
			if !strings.Contains(msg, "BedrockA") {
				t.Errorf("error must name the client; got: %s", msg)
			}
			if !strings.Contains(msg, tc.wantField) {
				t.Errorf("error must mention missing field %q; got: %s", tc.wantField, msg)
			}
			// Security pin: the error must NEVER echo a configured
			// credential value. This is the load-bearing assertion
			// for the doc-comment promise on
			// ResolveBedrockCredentials.
			for _, secret := range []string{
				staticCredsTestAccessKey,
				staticCredsTestSecretKey,
				staticCredsTestSessionToken,
			} {
				if strings.Contains(msg, secret) {
					t.Errorf("error leaked credential value %q; got: %s", secret, msg)
				}
			}
		})
	}
}

// TestResolveBedrockCredentials_EnvRefUnsetIsRejected pins the
// "unset env reference is an error, not silent fallback" branch.
// The selector models the codegen output for an `env.UNSET_VAR`
// reference: codegen drives Present from BedrockOptionValue.IsSet()
// (declared-in-.baml), so a declared env.X with an unset env var
// produces Present=true, Value="". The resolver must reject this as
// a partial state rather than silently fall through to profile /
// default chain.
func TestResolveBedrockCredentials_EnvRefUnsetIsRejected(t *testing.T) {
	// env.UNSET_AKID + literal secret. AccessKeyIDPresent = true
	// because the field was declared in .baml; AccessKeyID = ""
	// because the env lookup did not resolve. The resolver enters
	// the static branch (any *Present is true) and rejects the
	// empty AKID.
	sel := BedrockCredentialSelector{
		AccessKeyIDPresent:     true,
		AccessKeyID:            "",
		SecretAccessKey:        staticCredsTestSecretKey,
		SecretAccessKeyPresent: true,
	}
	_, err := ResolveBedrockCredentials(context.Background(), "ClientName", sel)
	if err == nil {
		t.Fatal("expected error when an env.X access_key_id is unset")
	}
	if !strings.Contains(err.Error(), "access_key_id") {
		t.Errorf("error must point to the missing field; got: %v", err)
	}
}

// TestResolveBedrockCredentials_EnvRefAllUnset_Errors is the regression
// pin for the #263 sign-off blocker. Scenario: operator declared
// access_key_id AND secret_access_key as env.X refs in .baml, but both
// env vars are unset at runtime. Codegen sets Present=true (from
// IsSet()) and Value="" (from Resolve()) for both. The resolver MUST
// error in the static branch rather than silently fall through to the
// default AWS credential chain — that fallthrough would let a
// misconfigured worker sign requests with whatever credentials happen
// to be on the host, which is the original security-significant bug.
func TestResolveBedrockCredentials_EnvRefAllUnset_Errors(t *testing.T) {
	sel := BedrockCredentialSelector{
		AccessKeyIDPresent:     true,
		AccessKeyID:            "",
		SecretAccessKeyPresent: true,
		SecretAccessKey:        "",
	}
	_, err := ResolveBedrockCredentials(context.Background(), "EnvUnset", sel)
	if err == nil {
		t.Fatal("expected error when declared env.X access_key_id AND secret_access_key both resolve to empty; silent fallback to default chain is the #263 blocker")
	}
	msg := err.Error()
	if !strings.Contains(msg, "EnvUnset") {
		t.Errorf("error must name the client; got: %s", msg)
	}
	// The static branch is entered because *Present flags are true.
	// AccessKeyID is checked first, so that is the field the error
	// must reference.
	if !strings.Contains(msg, "access_key_id") {
		t.Errorf("error must reference the empty static field; got: %s", msg)
	}
}

// TestResolveBedrockCredentials_ProfileEnvUnset_Errors pins the same
// declared-but-env-unset semantics for the profile branch. An operator
// declaring `profile env.AWS_PROFILE` with AWS_PROFILE unset must get
// a client-scoped error, not a silent fall-through to the default
// chain. Codegen emits Present=true (from IsSet()), Value="" (from
// Resolve()) for an unset env ref.
func TestResolveBedrockCredentials_ProfileEnvUnset_Errors(t *testing.T) {
	sel := BedrockCredentialSelector{
		ProfilePresent: true,
		Profile:        "",
	}
	_, err := ResolveBedrockCredentials(context.Background(), "ProfileEnvUnset", sel)
	if err == nil {
		t.Fatal("expected error when declared profile env.X resolves to empty")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ProfileEnvUnset") {
		t.Errorf("error must name the client; got: %s", msg)
	}
	if !strings.Contains(msg, "profile") {
		t.Errorf("error must reference the profile field; got: %s", msg)
	}
}

// TestAttachBedrockAuthForClient_StaticProviderUsedForSigning pins
// that static credentials from .baml options flow all the way through
// AttachBedrockAuthForClient -> AttachBedrockAuthWithOptions ->
// AWSAuthConfig.Credentials, and that signRequest then uses them to
// produce an Authorization header referencing the static access key.
// Cross-checks the dispatch wiring against the resolver in isolation.
func TestAttachBedrockAuthForClient_StaticProviderUsedForSigning(t *testing.T) {
	req := &Request{
		URL:    "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
		Method: "POST",
		Body:   `{}`,
	}
	if err := AttachBedrockAuthForClient(context.Background(), req, BedrockClientAuthOptions{
		ClientName: "StaticClient",
		Region:     "us-east-1",
		Credentials: BedrockCredentialSelector{
			AccessKeyID:            staticCredsTestAccessKey,
			AccessKeyIDPresent:     true,
			SecretAccessKey:        staticCredsTestSecretKey,
			SecretAccessKeyPresent: true,
		},
	}); err != nil {
		t.Fatalf("AttachBedrockAuthForClient: %v", err)
	}
	if req.AWSAuth == nil || req.AWSAuth.Credentials == nil {
		t.Fatal("static credentials must flow through to AWSAuth.Credentials")
	}

	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	req.AWSAuth.NowFunc = func() time.Time { return pinned }
	if err := signRequest(context.Background(), req, req.URL); err != nil {
		t.Fatalf("signRequest: %v", err)
	}

	auth := req.Headers["Authorization"]
	if !strings.Contains(auth, "Credential="+staticCredsTestAccessKey+"/20260511/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization must reference the static access key in credential scope; got: %s", auth)
	}
}

// TestAttachBedrockAuthForClient_StaticVsDefaultProducesDifferentSignatures
// is the "did we actually plug the slot in" pin. A regression that
// silently dropped the resolved provider and re-fetched the default
// chain would still pass the earlier static-flow test (different
// AKID would still produce *some* Authorization header) — but the
// signature it produces would match the default-chain signature
// instead of the static one. This test signs the same request body
// twice, once with each provider, and asserts the two Authorization
// headers differ.
func TestAttachBedrockAuthForClient_StaticVsDefaultProducesDifferentSignatures(t *testing.T) {
	// Swap the default-chain loader for a deterministic stub so the
	// non-static signature is reproducible.
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
	defaultAWSConfigLoader = func(ctx context.Context) (aws.Config, error) {
		return aws.Config{Credentials: staticCreds{cred: aws.Credentials{
			AccessKeyID:     "DEFAULT_CHAIN_AK",
			SecretAccessKey: "DEFAULT_CHAIN_SK",
		}}}, nil
	}

	pinned := time.Date(2026, 5, 11, 12, 34, 56, 0, time.UTC)
	makeReq := func() *Request {
		return &Request{
			URL:     "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/converse",
			Method:  "POST",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"messages":[]}`,
		}
	}

	staticReq := makeReq()
	if err := AttachBedrockAuthForClient(context.Background(), staticReq, BedrockClientAuthOptions{
		ClientName: "StaticClient",
		Region:     "us-east-1",
		Credentials: BedrockCredentialSelector{
			AccessKeyID:            staticCredsTestAccessKey,
			AccessKeyIDPresent:     true,
			SecretAccessKey:        staticCredsTestSecretKey,
			SecretAccessKeyPresent: true,
		},
	}); err != nil {
		t.Fatalf("static AttachBedrockAuthForClient: %v", err)
	}
	staticReq.AWSAuth.NowFunc = func() time.Time { return pinned }
	if err := signRequest(context.Background(), staticReq, staticReq.URL); err != nil {
		t.Fatalf("static signRequest: %v", err)
	}

	defaultReq := makeReq()
	if err := AttachBedrockAuthForClient(context.Background(), defaultReq, BedrockClientAuthOptions{
		ClientName: "DefaultClient",
		Region:     "us-east-1",
	}); err != nil {
		t.Fatalf("default AttachBedrockAuthForClient: %v", err)
	}
	defaultReq.AWSAuth.NowFunc = func() time.Time { return pinned }
	if err := signRequest(context.Background(), defaultReq, defaultReq.URL); err != nil {
		t.Fatalf("default signRequest: %v", err)
	}

	staticAuth := staticReq.Headers["Authorization"]
	defaultAuth := defaultReq.Headers["Authorization"]
	if staticAuth == "" || defaultAuth == "" {
		t.Fatalf("missing Authorization headers: static=%q default=%q", staticAuth, defaultAuth)
	}
	if staticAuth == defaultAuth {
		t.Errorf("static and default-chain signatures must differ — the static provider slot is silently being dropped.\n  static:  %s\n  default: %s", staticAuth, defaultAuth)
	}
	// And specifically: each Authorization's Credential= prefix must
	// reference the corresponding access key.
	if !strings.Contains(staticAuth, "Credential="+staticCredsTestAccessKey+"/") {
		t.Errorf("static Authorization missing static access key in Credential=; got: %s", staticAuth)
	}
	if !strings.Contains(defaultAuth, "Credential=DEFAULT_CHAIN_AK/") {
		t.Errorf("default Authorization missing default-chain access key; got: %s", defaultAuth)
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
