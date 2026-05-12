// awsstream_exec.go provides ExecuteAWSStream — the streaming HTTP
// transport for AWS event-stream responses (Content-Type:
// application/vnd.amazon.eventstream). It's a sibling of ExecuteStream
// scoped to Bedrock ConverseStream: same URL-rewrite + SigV4 lifecycle,
// different body content-type check and different return type
// (an awsstream.Decoder instead of an SSE channel).
//
// Lives in llmhttp/ because the transport responsibilities (URL
// rewrite, SigV4, HTTP error classification, body limit on error
// responses) are identical to the rest of the package. The
// provider-specific event decoding stays in bamlutils/awsstream so
// non-bedrock callers don't pick up the AWS SDK dependency surface at
// the llmhttp boundary for no reason.
package llmhttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/awsstream"
)

// AWSStreamContentType is the content-type the Bedrock ConverseStream
// API emits on a 2xx response. ExecuteAWSStream rejects anything that
// doesn't include this substring (matching ExecuteStream's
// text/event-stream check). Exposed so callers and tests can use the
// exact same constant when wiring mocks.
const AWSStreamContentType = "application/vnd.amazon.eventstream"

// AWSStreamResponse is the active AWS event-stream connection returned
// from ExecuteAWSStream. Parallels StreamResponse:
//
//   - StatusCode / Headers expose the HTTP envelope so callers
//     diagnose unexpected upstream behaviour the same way they do for
//     SSE.
//   - Events is an *awsstream.Decoder reading the response body. The
//     caller iterates Next() until io.EOF, surfacing *TransportError /
//     *BedrockStreamException etc. to its error pipeline.
//   - Close releases the underlying connection. The caller is
//     responsible for invoking it; idempotent.
//
// The body channel is the load-bearing contract; modeled
// initial-response metadata (request id, model name from response
// headers) is not surfaced here today.
type AWSStreamResponse struct {
	StatusCode int
	Headers    http.Header
	Events     *awsstream.Decoder

	body io.ReadCloser
}

// Close releases the HTTP connection. Safe to call multiple times.
// Idempotent — matches StreamResponse.Close so callers can defer it.
func (s *AWSStreamResponse) Close() {
	if s == nil || s.body == nil {
		return
	}
	s.body.Close()
	s.body = nil
}

// ExecuteAWSStream sends the given request and returns an active AWS
// event-stream response.
//
// Lifecycle mirrors ExecuteStream:
//   - URL rewrite first (urlrewrite.GlobalRules) so the same rewrite
//     rules apply for AWS-stream requests as for SSE.
//   - SigV4 signing after rewrite, before dispatch — the AWSAuth hook
//     in signRequest already runs on the rewritten URL.
//   - 4xx/5xx surface as *HTTPError BEFORE attempting to decode the
//     body, identical to ExecuteStream's error envelope.
//   - Content-Type must include "application/vnd.amazon.eventstream";
//     anything else is rejected with the response body (capped to
//     MaxErrorBodyBytes) included in the error for diagnostics.
//
// On success, the caller MUST Close() the returned response when done
// iterating to release the underlying connection.
//
// Backend: this path is net/http only, by design. Bedrock's edge
// negotiates HTTP/2 with ALPN, so production traffic always lands on
// the stdlib HTTP/2 client anyway; the fasthttp branch's
// SSE-tail-tracking machinery has no equivalent for AWS event-stream
// framing (which has its own CRC + length-prefix terminator), so a
// fasthttp twin isn't worth maintaining. Mock servers
// (httptest.NewServer) speak HTTP/1.1 over net/http, which works fine
// here.
func (c *Client) ExecuteAWSStream(ctx context.Context, req *Request) (*AWSStreamResponse, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("llmhttp: nil client")
	}
	if req == nil {
		return nil, fmt.Errorf("llmhttp: nil request")
	}

	rewritten := resolveRequestURL(req)

	// SigV4 signing runs after URL rewrite (so the signature matches
	// the host the request actually goes out with) and before backend
	// dispatch — identical contract to ExecuteStream, reusing the same
	// signRequest hook.
	if err := signRequest(ctx, req, rewritten); err != nil {
		return nil, err
	}

	httpReq, err := buildHTTPRequest(ctx, req, rewritten)
	if err != nil {
		return nil, fmt.Errorf("llmhttp: failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if te := classifyTransportErr(err, "llmhttp: request failed", true); te != nil {
			return nil, te
		}
		return nil, fmt.Errorf("llmhttp: request failed: %w", err)
	}

	// Surface non-2xx as *HTTPError with a bounded error body, before
	// any decode work. Matches ExecuteStream's policy verbatim so the
	// worker-side classifier (cmd/worker/error_classify.go) sees the
	// same shape regardless of which streaming transport ran.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	// Reject any response whose Content-Type isn't AWS event-stream.
	// Failing closed against proxy error pages / misconfigured mocks
	// mirrors the SSE path's "missing Content-Type" rejection so
	// callers get a uniform diagnostic.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, AWSStreamContentType) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		resp.Body.Close()
		if ct == "" {
			return nil, fmt.Errorf("llmhttp: missing Content-Type header (expected %s): %s", AWSStreamContentType, string(body))
		}
		return nil, fmt.Errorf("llmhttp: unexpected Content-Type %q (expected %s): %s", ct, AWSStreamContentType, string(body))
	}

	return &AWSStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Events:     awsstream.NewDecoder(resp.Body),
		body:       resp.Body,
	}, nil
}
