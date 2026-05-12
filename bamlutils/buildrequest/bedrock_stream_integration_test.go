package buildrequest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/awsstream"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// bedrockStreamFrame is a fixture helper that builds a single AWS
// event-stream frame for a Bedrock ConverseStream response.
func bedrockStreamFrame(t *testing.T, eventType string, payload []byte) []byte {
	t.Helper()
	var hs eventstream.Headers
	hs.Set(":message-type", eventstream.StringValue("event"))
	hs.Set(":event-type", eventstream.StringValue(eventType))
	hs.Set(":content-type", eventstream.StringValue("application/json"))
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{Headers: hs, Payload: payload}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// bedrockStreamExceptionFrame builds a single :message-type=exception
// frame, used to exercise the modeled-error arm.
func bedrockStreamExceptionFrame(t *testing.T, exceptionType string, payload []byte) []byte {
	t.Helper()
	var hs eventstream.Headers
	hs.Set(":message-type", eventstream.StringValue("exception"))
	hs.Set(":exception-type", eventstream.StringValue(exceptionType))
	hs.Set(":content-type", eventstream.StringValue("application/json"))
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{Headers: hs, Payload: payload}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// bedrockStreamTransportErrorFrame builds a single :message-type=error
// frame. The awsstream decoder surfaces these as *TransportError,
// which the worker classifier maps to provider_error.
func bedrockStreamTransportErrorFrame(t *testing.T, code, message string) []byte {
	t.Helper()
	var hs eventstream.Headers
	hs.Set(":message-type", eventstream.StringValue("error"))
	hs.Set(":error-code", eventstream.StringValue(code))
	hs.Set(":error-message", eventstream.StringValue(message))
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{Headers: hs, Payload: nil}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// newMockBedrockStreamServer returns an httptest.Server that responds
// to POST /model/<id>/converse-stream with the supplied frame bytes.
// SigV4 headers are asserted to be present (codegen emits them via
// MaybeAttachBedrockAuth) but the signature is not verified against
// real IAM — same contract as the call-branch integration test.
//
// All observability flows through the supplied atomic pointers — the
// handler runs on a separate httptest goroutine where t.Fatalf is
// unsafe, so the caller asserts after the request returns.
func newMockBedrockStreamServer(
	t *testing.T,
	frames []byte,
	sawHeaders *atomic.Int32,
	sawPath *atomic.Pointer[string],
	sawMethod *atomic.Pointer[string],
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sawHeaders != nil {
			n := int32(0)
			if r.Header.Get("Authorization") != "" {
				n++
			}
			if r.Header.Get("X-Amz-Date") != "" {
				n++
			}
			if r.Header.Get("X-Amz-Content-Sha256") != "" {
				n++
			}
			if r.Header.Get("Accept") == llmhttp.AWSStreamContentType {
				n++
			}
			sawHeaders.Store(n)
		}
		if sawPath != nil {
			p := r.URL.Path
			sawPath.Store(&p)
		}
		if sawMethod != nil {
			m := r.Method
			sawMethod.Store(&m)
		}
		w.Header().Set("Content-Type", llmhttp.AWSStreamContentType)
		w.WriteHeader(200)
		w.Write(frames)
	}))
}

// pinnedAWSStreamAuth builds an AWSAuthConfig with fixed credentials +
// timestamp so signing is deterministic. The mock server doesn't
// verify the signature; the test only pins that headers were emitted.
func pinnedAWSStreamAuth(at time.Time) *llmhttp.AWSAuthConfig {
	return AWSAuthConfigForTest(at)
}

// makeBedrockStreamRequestFn constructs the codegen-shape closure that
// the orchestrator's bedrock branch consumes via
// StreamConfig.BuildBedrockStreamRequest. Production codegen also
// rewrites the URL to /converse-stream and attaches AWS event-stream
// Accept + AWSAuth; the test hard-codes them here.
func makeBedrockStreamRequestFn(serverURL string, at time.Time) BuildRequestFunc {
	return func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		return &llmhttp.Request{
			URL:    serverURL + "/model/anthropic.claude-3-sonnet-20240229-v1:0/converse-stream",
			Method: "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
				"Accept":       llmhttp.AWSStreamContentType,
			},
			Body:    `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			AWSAuth: pinnedAWSStreamAuth(at),
		}, nil
	}
}

// TestRunStreamOrchestration_AWSBedrock_HappyPath drives the full
// stack for a Bedrock streaming success: BuildRequest closure
// produces an llmhttp.Request with AWSAuth + event-stream Accept,
// llmhttp.ExecuteAWSStream signs and dispatches, the orchestrator
// iterates *awsstream.Decoder events through
// extractBedrockStreamDelta, accumulates parseable+raw, and emits
// heartbeat + final on the result channel.
func TestRunStreamOrchestration_AWSBedrock_HappyPath(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(`{"delta":{"text":"Hello"}}`)))
	frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(`{"delta":{"text":" world"}}`)))
	frames.Write(bedrockStreamFrame(t, "contentBlockStop", []byte(`{"contentBlockIndex":0}`)))
	frames.Write(bedrockStreamFrame(t, "messageStop", []byte(`{"stopReason":"end_turn"}`)))

	var sawHeaders atomic.Int32
	var sawPath atomic.Pointer[string]
	var sawMethod atomic.Pointer[string]
	server := newMockBedrockStreamServer(t, frames.Bytes(), &sawHeaders, &sawPath, &sawMethod)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 32)

	config := &StreamConfig{
		Provider:                  "aws-bedrock",
		NeedsPartials:             true,
		NeedsRaw:                  true,
		IncludeReasoning:          false,
		BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		// buildRequest must be non-nil per the signature; the
		// dispatcher routes bedrock to BuildBedrockStreamRequest
		// regardless of this closure's contents.
		func(ctx context.Context, _ string) (*llmhttp.Request, error) {
			return nil, errors.New("buildRequest must not be called for aws-bedrock")
		},
		func(_ context.Context, accumulated string) (any, error) {
			return accumulated, nil
		},
		func(_ context.Context, accumulated string) (any, error) {
			return accumulated, nil
		},
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration: %v", err)
	}
	close(out)

	if got := sawHeaders.Load(); got < 4 {
		t.Errorf("expected all 4 signed/accept headers present (got %d)", got)
	}
	if p := sawPath.Load(); p == nil || !strings.HasSuffix(*p, "/converse-stream") {
		t.Errorf("upstream path should end in /converse-stream; got %v", p)
	}
	// Bedrock ConverseStream is POST-only — pin the verb so a
	// codegen regression that flipped the method (or a request-
	// builder reshuffle) surfaces here rather than as a confusing
	// upstream 4xx.
	if m := sawMethod.Load(); m == nil || *m != http.MethodPost {
		t.Errorf("expected upstream HTTP method POST; got %v", m)
	}

	var (
		sawHeartbeat bool
		sawFinal     bool
		finalText    string
		finalRaw     string
	)
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindHeartbeat:
			sawHeartbeat = true
		case bamlutils.StreamResultKindStream:
			// Partials are best-effort; their text is unbounded by
			// throttle so we don't assert their count.
		case bamlutils.StreamResultKindFinal:
			sawFinal = true
			finalText = fmt.Sprintf("%v", r.Final())
			finalRaw = r.Raw()
		case bamlutils.StreamResultKindError:
			t.Fatalf("unexpected error result: %v", r.Error())
		}
	}
	if !sawHeartbeat {
		t.Error("expected heartbeat result")
	}
	if !sawFinal {
		t.Error("expected final result")
	}
	if finalText != "Hello world" {
		t.Errorf("Final = %q, want %q", finalText, "Hello world")
	}
	if finalRaw != "Hello world" {
		t.Errorf("Raw = %q, want %q (raw is text-only by construction)", finalRaw, "Hello world")
	}
}

// TestRunStreamOrchestration_AWSBedrock_ReasoningOptIn pins the
// IncludeReasoning gate end-to-end: a stream with text + reasoning
// frames feeds reasoning into the dedicated channel only when the
// caller opts in. Parseable/Raw stay text-only regardless.
func TestRunStreamOrchestration_AWSBedrock_ReasoningOptIn(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(`{"delta":{"reasoningContent":{"text":"thinking..."}}}`)))
	frames.Write(bedrockStreamFrame(t, "contentBlockDelta", []byte(`{"delta":{"text":"answer"}}`)))
	frames.Write(bedrockStreamFrame(t, "messageStop", []byte(`{"stopReason":"end_turn"}`)))

	for _, includeReasoning := range []bool{false, true} {
		t.Run(fmt.Sprintf("includeReasoning=%v", includeReasoning), func(t *testing.T) {
			server := newMockBedrockStreamServer(t, frames.Bytes(), nil, nil, nil)
			defer server.Close()

			client := llmhttp.NewClient(server.Client())
			out := make(chan bamlutils.StreamResult, 32)

			config := &StreamConfig{
				Provider:                  "aws-bedrock",
				NeedsRaw:                  true,
				IncludeReasoning:          includeReasoning,
				BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
			}

			err := RunStreamOrchestration(
				context.Background(), out, config, client,
				func(ctx context.Context, _ string) (*llmhttp.Request, error) {
					return nil, errors.New("buildRequest must not be called for aws-bedrock")
				},
				func(_ context.Context, s string) (any, error) { return s, nil },
				func(_ context.Context, s string) (any, error) { return s, nil },
				newTestResult,
			)
			if err != nil {
				t.Fatalf("RunStreamOrchestration: %v", err)
			}
			close(out)

			var final bamlutils.StreamResult
			for r := range out {
				if r.Kind() == bamlutils.StreamResultKindFinal {
					final = r
				}
			}
			if final == nil {
				t.Fatal("expected final result")
			}
			if got := fmt.Sprintf("%v", final.Final()); got != "answer" {
				t.Errorf("Final = %q, want %q (parseable must be text-only regardless of IncludeReasoning)", got, "answer")
			}
			if got := final.Raw(); got != "answer" {
				t.Errorf("Raw = %q, want %q (raw is text-only by construction)", got, "answer")
			}
			wantReasoning := ""
			if includeReasoning {
				wantReasoning = "thinking..."
			}
			if got := final.Reasoning(); got != wantReasoning {
				t.Errorf("Reasoning = %q, want %q", got, wantReasoning)
			}
		})
	}
}

// TestRunStreamOrchestration_AWSBedrock_ModeledException pins the
// failure path for a modeled exception frame: the orchestrator
// surfaces an error result (no Final) so the worker classifier maps
// it through to provider_error.
func TestRunStreamOrchestration_AWSBedrock_ModeledException(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	frames.Write(bedrockStreamExceptionFrame(t, "modelStreamErrorException", []byte(`{"message":"the model errored"}`)))

	server := newMockBedrockStreamServer(t, frames.Bytes(), nil, nil, nil)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 32)

	config := &StreamConfig{
		Provider:                  "aws-bedrock",
		NeedsRaw:                  true,
		BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, _ string) (*llmhttp.Request, error) {
			return nil, errors.New("unused")
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration: %v", err)
	}
	close(out)

	var (
		sawError    bool
		errResult   error
		sawFinal    bool
		exceptionOK bool
	)
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindError:
			sawError = true
			errResult = r.Error()
			var be *BedrockStreamException
			if errors.As(errResult, &be) {
				exceptionOK = true
			}
		case bamlutils.StreamResultKindFinal:
			sawFinal = true
		}
	}
	if !sawError {
		t.Fatalf("expected error result")
	}
	if sawFinal {
		t.Errorf("must not emit Final after a modeled exception")
	}
	if !exceptionOK {
		t.Errorf("error chain must contain *BedrockStreamException; got %v", errResult)
	}
}

// TestRunStreamOrchestration_AWSBedrock_TransportError pins the
// failure path for a :message-type=error transport frame: the
// awsstream decoder surfaces *TransportError, the orchestrator wraps
// it, and the worker classifier sees the typed error via errors.As.
func TestRunStreamOrchestration_AWSBedrock_TransportError(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var frames bytes.Buffer
	frames.Write(bedrockStreamFrame(t, "messageStart", []byte(`{"role":"assistant"}`)))
	frames.Write(bedrockStreamTransportErrorFrame(t, "InternalServerError", "service unavailable"))

	server := newMockBedrockStreamServer(t, frames.Bytes(), nil, nil, nil)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 32)

	config := &StreamConfig{
		Provider:                  "aws-bedrock",
		NeedsRaw:                  true,
		BuildBedrockStreamRequest: makeBedrockStreamRequestFn(server.URL, pinnedTime),
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		func(ctx context.Context, _ string) (*llmhttp.Request, error) {
			return nil, errors.New("unused")
		},
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("RunStreamOrchestration: %v", err)
	}
	close(out)

	var (
		sawError  bool
		sawFinal  bool
		errResult error
		typedOK   bool
	)
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindError:
			sawError = true
			errResult = r.Error()
			var te *awsstream.TransportError
			if errors.As(errResult, &te) {
				typedOK = te.Code == "InternalServerError" && te.Message == "service unavailable"
			}
		case bamlutils.StreamResultKindFinal:
			sawFinal = true
		}
	}
	if !sawError {
		t.Fatalf("expected error result")
	}
	if sawFinal {
		// Mirrors the modeled-exception test's contract: a terminal
		// transport error must not be followed by a Final frame.
		// Without this guard a regression that flushed the partial
		// stream's accumulated state as a successful result could
		// pass the error-side assertions silently.
		t.Errorf("transport-error stream emitted a Final result after the error frame; must terminate")
	}
	if !typedOK {
		t.Errorf("error chain must contain *awsstream.TransportError with the right code/message; got %v", errResult)
	}
}

// TestRunStreamOrchestration_AWSBedrock_RequiresBuilder pins the
// up-front validation: a single-provider aws-bedrock stream
// configuration without BuildBedrockStreamRequest must fail before
// any HTTP work.
func TestRunStreamOrchestration_AWSBedrock_RequiresBuilder(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 4)
	config := &StreamConfig{
		Provider: "aws-bedrock",
		// BuildBedrockStreamRequest deliberately nil.
	}
	err := RunStreamOrchestration(
		context.Background(), out, config, llmhttp.DefaultClient,
		func(ctx context.Context, _ string) (*llmhttp.Request, error) { return nil, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	if err == nil {
		t.Fatal("expected validation error when BuildBedrockStreamRequest is nil")
	}
	if !strings.Contains(err.Error(), "BuildBedrockStreamRequest") {
		t.Errorf("error = %q, want it to mention BuildBedrockStreamRequest", err.Error())
	}
}
