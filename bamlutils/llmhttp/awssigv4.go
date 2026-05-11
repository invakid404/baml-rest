// awssigv4.go implements an AWS SigV4 signing hook for outgoing
// llmhttp requests. The hook is gated by per-request AWSAuth metadata
// so non-bedrock requests pay nothing — and it runs after llmhttp's
// URL rewrite step so the signature is over the URL the request
// actually goes out with (mock/proxy hosts and production hosts
// alike).
//
// PR1-bedrock breadcrumb: introduced as part of issue #243 PR 1 to
// support aws-bedrock on the BuildRequest call path. PR 3 will reuse
// this hook for the streaming path; PR 4 will scrub PR1-bedrock
// breadcrumb comments.
package llmhttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
)

// AWSAuthConfig carries the per-request information the SigV4 hook
// needs to sign an outgoing llmhttp.Request. Attached by the
// aws-bedrock codegen branch; nil for every other provider.
type AWSAuthConfig struct {
	// Region is the AWS region the signing algorithm is scoped to,
	// e.g. "us-east-1". Parsed from the BAML-emitted Bedrock URL host
	// (bedrock-runtime.<region>.amazonaws.com).
	Region string

	// Service is the AWS service name, e.g. "bedrock". Fixed by the
	// caller; the SDK signer just plugs it into the credential scope.
	Service string

	// Credentials retrieves AWS credentials per-attempt. Each retry
	// asks the provider for a fresh credential set; the SDK's
	// cred-cache handles refresh internally.
	Credentials aws.CredentialsProvider

	// NowFunc lets tests pin the signing timestamp so the
	// Authorization header (and X-Amz-Date) are deterministic. nil
	// uses time.Now().
	NowFunc func() time.Time
}

// now returns the current time according to the auth config, falling
// back to time.Now when NowFunc is nil.
func (a *AWSAuthConfig) now() time.Time {
	if a == nil || a.NowFunc == nil {
		return time.Now()
	}
	return a.NowFunc()
}

// signRequest mutates req.Headers in place to add SigV4 headers when
// req carries AWSAuth metadata. Returns nil and leaves headers
// untouched when AWSAuth is nil — every non-bedrock provider hits this
// branch.
//
// rewrittenURL is the URL the request will actually go out with after
// llmhttp.resolveRequestURL. Signing the rewritten URL keeps the
// signature aligned with the wire request even when the URL rewrite
// rules point at a mock/proxy host.
func signRequest(ctx context.Context, req *Request, rewrittenURL string) error {
	if req == nil || req.AWSAuth == nil {
		return nil
	}
	auth := req.AWSAuth
	if auth.Credentials == nil {
		return errors.New("llmhttp: AWS SigV4 requested but Credentials provider is nil")
	}
	if auth.Region == "" {
		return errors.New("llmhttp: AWS SigV4 requested but Region is empty")
	}
	if auth.Service == "" {
		return errors.New("llmhttp: AWS SigV4 requested but Service is empty")
	}

	creds, err := auth.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("llmhttp: retrieve AWS credentials: %w", err)
	}

	// Build a throwaway *http.Request so the SDK signer can read the
	// URL, host, and headers it needs. The dispatch path doesn't
	// consume this request — we only copy the signed headers back
	// onto req.Headers below.
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, rewrittenURL, body)
	if err != nil {
		return fmt.Errorf("llmhttp: build sign-only request: %w", err)
	}
	// Mirror caller-supplied headers, routing Host through Request.Host
	// (mirrors buildHTTPRequest) so the SDK sees the same effective
	// host both backends will send on the wire.
	for k, v := range req.Headers {
		if strings.EqualFold(k, "Host") {
			httpReq.Host = v
			continue
		}
		httpReq.Header.Set(k, v)
	}
	if req.Body != "" {
		httpReq.ContentLength = int64(len(req.Body))
	}

	// X-Amz-Content-Sha256 is required by the Bedrock service and must
	// be set BEFORE SignHTTP so the signer includes it in the signed
	// headers — SignHTTP itself does not write this header.
	sum := sha256.Sum256([]byte(req.Body))
	payloadHash := hex.EncodeToString(sum[:])
	httpReq.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, httpReq, payloadHash, auth.Service, auth.Region, auth.now()); err != nil {
		return fmt.Errorf("llmhttp: SigV4 sign: %w", err)
	}

	if req.Headers == nil {
		req.Headers = make(map[string]string, 4)
	}
	// SignHTTP writes Authorization + X-Amz-Date (and X-Amz-Security-Token
	// when the credential carries a session token). Copy them — plus
	// X-Amz-Content-Sha256 which we set manually — back onto req.Headers
	// so both the net/http and fasthttp dispatch backends emit them on
	// the wire.
	for _, name := range []string{
		"Authorization",
		"X-Amz-Date",
		"X-Amz-Content-Sha256",
		"X-Amz-Security-Token",
	} {
		if v := httpReq.Header.Get(name); v != "" {
			req.Headers[name] = v
		}
	}
	return nil
}

// parseBedrockRegion extracts the AWS region from a BAML-emitted
// Bedrock URL whose host is bedrock-runtime.<region>.amazonaws.com.
// PR1-bedrock breadcrumb: PR 4 may revisit this if an endpoint_url
// override path lands, since custom endpoints break the
// host-encoded-region assumption.
func parseBedrockRegion(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse bedrock URL: %w", err)
	}
	host := parsed.Host
	const prefix = "bedrock-runtime."
	const suffix = ".amazonaws.com"
	if !strings.HasPrefix(host, prefix) || !strings.HasSuffix(host, suffix) {
		return "", fmt.Errorf("not a bedrock-runtime host: %s", host)
	}
	// Reject hosts where prefix and suffix overlap (e.g.
	// "bedrock-runtime.amazonaws.com" — len 28 < 30 combined), which
	// would slice into negative territory.
	if len(host) < len(prefix)+len(suffix) {
		return "", fmt.Errorf("could not parse region from host: %s", host)
	}
	region := host[len(prefix) : len(host)-len(suffix)]
	// Region must be a single DNS label — reject empty or dotted forms
	// like "bedrock-runtime..amazonaws.com" or
	// "bedrock-runtime.amazonaws.com" (which TrimPrefix would otherwise
	// leave as "amazonaws.com").
	if region == "" || strings.ContainsRune(region, '.') {
		return "", fmt.Errorf("could not parse region from host: %s", host)
	}
	return region, nil
}

var (
	defaultAWSCredsOnce sync.Once
	defaultAWSCreds     aws.CredentialsProvider
	defaultAWSCredsErr  error
)

// DefaultAWSCredentialProvider returns a process-wide cached
// CredentialsProvider built from the standard AWS default chain
// (environment variables, shared config, profile, IMDS). Called from
// the aws-bedrock codegen branch when attaching AWSAuth to a request.
//
// The provider is loaded once on first call; refresh is owned by the
// SDK's internal cred cache. PR1-bedrock breadcrumb: PR 4 will
// introduce a path to override this with static `.baml` credentials
// once the adapter exposes them through a non-public surface.
func DefaultAWSCredentialProvider(ctx context.Context) (aws.CredentialsProvider, error) {
	defaultAWSCredsOnce.Do(func() {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			defaultAWSCredsErr = err
			return
		}
		defaultAWSCreds = cfg.Credentials
	})
	if defaultAWSCredsErr != nil {
		return nil, defaultAWSCredsErr
	}
	return defaultAWSCreds, nil
}

// MaybeAttachBedrockAuth is the codegen entry point. It populates
// req.AWSAuth with default Bedrock signing metadata when req.URL looks
// like a BAML-emitted Bedrock URL (bedrock-runtime.<region>.amazonaws.com),
// and is a no-op for every other URL.
//
// Safe to call unconditionally from the generated call branch — the
// URL-pattern check keeps non-bedrock providers untouched, so adding
// the call to all generated _buildCallRequest implementations costs
// nothing for openai/anthropic/etc. PR1-bedrock breadcrumb.
func MaybeAttachBedrockAuth(ctx context.Context, req *Request) error {
	if req == nil {
		return nil
	}
	if _, err := parseBedrockRegion(req.URL); err != nil {
		// Not a Bedrock URL — leave req untouched. The codegen calls
		// this for every provider, so a non-bedrock URL is the
		// expected case for every provider that isn't aws-bedrock.
		return nil
	}
	return AttachBedrockAuth(ctx, req)
}

// AttachBedrockAuth populates req.AWSAuth with the standard Bedrock
// signing metadata (region parsed from req.URL, service = "bedrock",
// default credential provider). Returns an error if the URL host does
// not match the BAML-emitted bedrock-runtime.<region>.amazonaws.com
// pattern or if the default credential chain cannot be loaded.
//
// PR1-bedrock breadcrumb: codegen calls this from the aws-bedrock
// non-streaming branch only. PR 3 will reuse it for streaming once
// the AWS event-stream decoder lands.
func AttachBedrockAuth(ctx context.Context, req *Request) error {
	if req == nil {
		return errors.New("llmhttp: AttachBedrockAuth: nil request")
	}
	region, err := parseBedrockRegion(req.URL)
	if err != nil {
		return err
	}
	creds, err := DefaultAWSCredentialProvider(ctx)
	if err != nil {
		return fmt.Errorf("llmhttp: load AWS credentials: %w", err)
	}
	req.AWSAuth = &AWSAuthConfig{
		Region:      region,
		Service:     "bedrock",
		Credentials: creds,
	}
	return nil
}
