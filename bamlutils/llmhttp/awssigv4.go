// awssigv4.go implements an AWS SigV4 signing hook for outgoing
// llmhttp requests. The hook is gated by per-request AWSAuth metadata
// so non-bedrock requests pay nothing — and it runs after llmhttp's
// URL rewrite step so the signature is over the URL the request
// actually goes out with (mock/proxy hosts and production hosts
// alike).
//
// Used by both the call path (Execute) and the streaming paths
// (ExecuteStream, ExecuteAWSStream).
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
	"os"
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
	// Purge any prior SigV4 headers (case-insensitive) before mirroring
	// caller headers into httpReq. Without this, reuse of a *Request
	// leaks stale Authorization / X-Amz-* — most concerning
	// X-Amz-Security-Token, which the next sign won't overwrite when
	// the new credential lacks a session token. Headers is a plain
	// map[string]string so case variants ("x-amz-date" vs "X-Amz-Date")
	// can otherwise coexist; an EqualFold walk drops all of them.
	for key := range req.Headers {
		for _, target := range sigV4OwnedHeaders {
			if strings.EqualFold(key, target) {
				delete(req.Headers, key)
				break
			}
		}
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
		req.Headers = make(map[string]string, len(sigV4OwnedHeaders))
	}
	// SignHTTP writes Authorization + X-Amz-Date (and X-Amz-Security-Token
	// when the credential carries a session token). Copy them — plus
	// X-Amz-Content-Sha256 which we set manually — back onto req.Headers
	// so both the net/http and fasthttp dispatch backends emit them on
	// the wire. Shares sigV4OwnedHeaders with the pre-mirror purge above
	// so the canonical-case copy-back can never miss a header that was
	// purged but produced by the signer.
	for _, name := range sigV4OwnedHeaders {
		if v := httpReq.Header.Get(name); v != "" {
			req.Headers[name] = v
		}
	}
	return nil
}

// sigV4OwnedHeaders enumerates the request headers the SigV4 signing
// path writes (or that signRequest itself sets pre-sign in the case of
// X-Amz-Content-Sha256). Used both to purge stale prior-sign headers
// from req.Headers before mirroring and to copy fresh signed headers
// back onto req.Headers after SignHTTP. Keeping the set in one place
// means adding a new SigV4 header (e.g. an additional X-Amz-* header
// in a future signer version) automatically updates both halves of
// the lifecycle and the regression tests.
var sigV4OwnedHeaders = []string{
	"Authorization",
	"X-Amz-Date",
	"X-Amz-Content-Sha256",
	"X-Amz-Security-Token",
}

// parseBedrockRegion extracts the AWS region from a BAML-emitted
// Bedrock URL whose host is bedrock-runtime.<region>.amazonaws.com.
// Custom endpoints (e.g. a future endpoint_url override path) would
// break the host-encoded-region assumption.
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

// defaultAWSConfigLoader loads an aws.Config via the standard SDK
// default chain. Indirected through a package-level var so tests can
// substitute a deterministic loader (e.g. first-call-fail / second-
// call-succeed scenarios) without driving the real AWS SDK chain.
// Production callers always go through config.LoadDefaultConfig.
var defaultAWSConfigLoader = func(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx)
}

var (
	defaultAWSCredsMu sync.Mutex
	// defaultAWSCreds is nil until the first successful load. Failures
	// must NOT populate this — see DefaultAWSCredentialProvider's
	// retry-on-failure contract.
	defaultAWSCreds aws.CredentialsProvider
)

// DefaultAWSCredentialProvider returns a process-wide cached
// CredentialsProvider built from the standard AWS default chain
// (environment variables, shared config, profile, IMDS). Called from
// the aws-bedrock codegen branch when attaching AWSAuth to a request.
//
// Successful loads are cached for the worker lifetime; the SDK's
// internal cred cache owns refresh from there. **Failures are not
// cached** — a transient first-call failure (canceled request context,
// malformed shared config, IMDS timeout) would otherwise poison Bedrock
// auth for the rest of the worker's life. Subsequent calls retry via
// the loader.
func DefaultAWSCredentialProvider(ctx context.Context) (aws.CredentialsProvider, error) {
	defaultAWSCredsMu.Lock()
	cached := defaultAWSCreds
	defaultAWSCredsMu.Unlock()
	if cached != nil {
		return cached, nil
	}

	cfg, err := defaultAWSConfigLoader(ctx)
	if err != nil {
		// Intentionally do not cache. The next call retries.
		return nil, err
	}

	defaultAWSCredsMu.Lock()
	defer defaultAWSCredsMu.Unlock()
	// Double-checked: another goroutine may have raced past our first
	// unlock and populated the cache. Reuse their provider so callers
	// see a single canonical provider for the worker lifetime.
	if defaultAWSCreds == nil {
		defaultAWSCreds = cfg.Credentials
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
// nothing for openai/anthropic/etc.
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
// Codegen calls this from the aws-bedrock branch on both the call path
// (Request.<Method>) and the streaming path
// (Request.<Method> + /converse-stream URL rewrite).
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

// BedrockAuthOptions carries the operator-configured aws-bedrock client
// options that AttachBedrockAuthWithOptions needs to override the BAML-
// emitted Bedrock URL and pin the SigV4 region. Populated by codegen
// from the .baml `options { ... }` block via the introspected
// BedrockClientOptions map.
type BedrockAuthOptions struct {
	// EndpointURL overrides the BAML-emitted Bedrock URL host (and
	// scheme). Empty leaves req.URL untouched, so the existing
	// bedrock-runtime.<region>.amazonaws.com value is signed as-is.
	// The override preserves req.URL's path, query, and fragment so
	// /model/<id>/converse and the streaming /converse-stream path
	// mutation both flow through unchanged.
	EndpointURL string

	// Region is the AWS region SigV4 signs with. Empty falls back to
	// the AWS_REGION env var; if that is also empty, the helper
	// returns an error rather than guessing — custom endpoints (VPC,
	// LocalStack, FIPS, China, GovCloud) break the host-based region
	// extraction MaybeAttachBedrockAuth uses, so the region must come
	// from config or env, never from URL parsing.
	Region string

	// Credentials is a forward-compat slot for the static-credentials
	// follow-up (#254 item 2: access_key_id / secret_access_key /
	// session_token / profile). Leave unset; nil means "use the default
	// AWS credential chain" via DefaultAWSCredentialProvider. This PR
	// does not populate the slot — wiring it is the static-creds PR's
	// job. Documenting the field here keeps the API stable across the
	// two PRs so the codegen call site doesn't change again when
	// static creds land.
	Credentials aws.CredentialsProvider
}

// AttachBedrockAuthWithOptions applies an explicit Bedrock endpoint
// override (when opts.EndpointURL is set), validates region, and
// attaches the SigV4 metadata so the downstream Execute / ExecuteAWS-
// Stream path signs the final URL.
//
// Unlike MaybeAttachBedrockAuth, this helper does NOT infer "is this a
// Bedrock request" from the URL host. Callers must only invoke it for
// requests they have already identified as targeting aws-bedrock — the
// codegen dispatch in adapters/common/codegen does this by gating on
// the introspected BedrockClientOptions map (only aws-bedrock clients
// land in that map). That separation is what lets the override safely
// rewrite the URL to a non-AWS host (LocalStack, VPC endpoint, FIPS,
// China, GovCloud) without first re-parsing the host to confirm it is
// Bedrock.
func AttachBedrockAuthWithOptions(ctx context.Context, req *Request, opts BedrockAuthOptions) error {
	if req == nil {
		return errors.New("llmhttp: AttachBedrockAuthWithOptions: nil request")
	}

	if opts.EndpointURL != "" {
		rewritten, err := rewriteBedrockEndpoint(req.URL, opts.EndpointURL)
		if err != nil {
			return err
		}
		req.URL = rewritten
	}

	region := opts.Region
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		return errors.New("aws-bedrock: region is required (set via .baml options.region or AWS_REGION env)")
	}

	creds := opts.Credentials
	if creds == nil {
		// Default credential chain. Static credential plumbing is
		// deferred — see the BedrockAuthOptions.Credentials doc.
		c, err := DefaultAWSCredentialProvider(ctx)
		if err != nil {
			return fmt.Errorf("llmhttp: load AWS credentials: %w", err)
		}
		creds = c
	}

	req.AWSAuth = &AWSAuthConfig{
		Region:      region,
		Service:     "bedrock",
		Credentials: creds,
	}
	return nil
}

// rewriteBedrockEndpoint replaces the scheme + host of originalURL with
// the scheme + host of endpointURL while preserving the original
// request's path, query, and fragment. A trailing slash on endpointURL
// is tolerated so operators don't have to remember to strip it from
// `endpoint_url "http://localhost:9000/"` shaped configs.
func rewriteBedrockEndpoint(originalURL, endpointURL string) (string, error) {
	endpointParsed, err := url.Parse(endpointURL)
	if err != nil {
		return "", fmt.Errorf("aws-bedrock: parse endpoint_url %q: %w", endpointURL, err)
	}
	if endpointParsed.Scheme == "" {
		return "", fmt.Errorf("aws-bedrock: endpoint_url %q is missing scheme", endpointURL)
	}
	if endpointParsed.Scheme != "http" && endpointParsed.Scheme != "https" {
		return "", fmt.Errorf("aws-bedrock: endpoint_url %q has unsupported scheme %q (expected http or https)", endpointURL, endpointParsed.Scheme)
	}
	if endpointParsed.Host == "" {
		return "", fmt.Errorf("aws-bedrock: endpoint_url %q is missing host", endpointURL)
	}

	originalParsed, err := url.Parse(originalURL)
	if err != nil {
		return "", fmt.Errorf("aws-bedrock: parse request URL %q: %w", originalURL, err)
	}

	rewritten := url.URL{
		Scheme:   endpointParsed.Scheme,
		Host:     endpointParsed.Host,
		Path:     originalParsed.Path,
		RawQuery: originalParsed.RawQuery,
		Fragment: originalParsed.Fragment,
	}
	return rewritten.String(), nil
}

// AttachBedrockAuthForClient is the codegen dispatch entry point for
// the aws-bedrock provider. When endpointURL or region is non-empty
// (i.e., the operator configured an override in `.baml options { ... }`
// for the selected client), it routes to AttachBedrockAuthWithOptions
// so the explicit endpoint and region are honored. When both are empty,
// it falls through to MaybeAttachBedrockAuth's URL-pattern detection —
// preserving the default-endpoint contract for clients that did not
// declare an override.
//
// The split exists because custom endpoints (VPC, LocalStack, FIPS,
// China, GovCloud) break the host-based region extraction
// MaybeAttachBedrockAuth uses; once the operator supplies an override,
// we must take the explicit path. Codegen looks up
// `introspected.BedrockClientOptions[selectedClient]` and resolves the
// literal-vs-env values before calling this helper.
func AttachBedrockAuthForClient(ctx context.Context, req *Request, endpointURL, region string) error {
	if endpointURL == "" && region == "" {
		return MaybeAttachBedrockAuth(ctx, req)
	}
	return AttachBedrockAuthWithOptions(ctx, req, BedrockAuthOptions{
		EndpointURL: endpointURL,
		Region:      region,
	})
}
