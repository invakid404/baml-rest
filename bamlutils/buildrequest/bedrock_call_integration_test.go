package buildrequest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// bedrockStaticCreds is a CredentialsProvider with pinned values so the
// signature embedded in the test is deterministic. The mock server
// doesn't verify the signature against real IAM — it only checks that
// the expected SigV4 headers are present and well-formed — so any
// non-empty credential works.
type bedrockStaticCreds struct{}

func (bedrockStaticCreds) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return aws.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRETEXAMPLE",
	}, nil
}

// TestRunCallOrchestration_AWSBedrock exercises the full stack for the
// aws-bedrock call branch: build request with AWSAuth attached → SigV4
// signing inside llmhttp → mock Converse server hit → response
// extraction → final result with reasoning routed correctly under the
// IncludeReasoning flag.
func TestRunCallOrchestration_AWSBedrock(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	var (
		requestSeen        atomic.Int32
		sawAuthorization   atomic.Bool
		sawAmzDate         atomic.Bool
		sawAmzContentSha   atomic.Bool
		sawCorrectAuthAlgo atomic.Bool
		sawPath            string
		bodyReceived       string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen.Add(1)
		sawPath = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			sawAuthorization.Store(true)
			if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
				sawCorrectAuthAlgo.Store(true)
			}
		}
		if r.Header.Get("X-Amz-Date") != "" {
			sawAmzDate.Store(true)
		}
		if r.Header.Get("X-Amz-Content-Sha256") != "" {
			sawAmzContentSha.Store(true)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		bodyReceived = string(buf[:n])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"output":{"message":{"role":"assistant","content":[
				{"reasoningContent":{"reasoningText":{"text":"thinking about it"}}},
				{"text":"42"}
			]}},
			"stopReason":"end_turn",
			"usage":{"inputTokens":10,"outputTokens":5}
		}`)
	}))
	defer server.Close()

	// The buildRequestFn shape that the generated codegen emits for the
	// aws-bedrock call branch: produce an llmhttp.Request with the
	// Converse URL + AWSAuth metadata. Real codegen reaches AWSAuth via
	// llmhttp.MaybeAttachBedrockAuth (which loads the default
	// credential chain); we inject static credentials here so the test
	// does not depend on the host's AWS configuration.
	buildRequestFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		return &llmhttp.Request{
			URL:    server.URL + "/model/anthropic.claude-3-sonnet-20240229-v1:0/converse",
			Method: "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body:    `{"messages":[{"role":"user","content":[{"text":"What is 6 * 7?"}]}],"inferenceConfig":{"maxTokens":128}}`,
			AWSAuth: AWSAuthConfigForTest(pinnedTime),
		}, nil
	}

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 16)

	config := &CallConfig{
		Provider:         "aws-bedrock",
		NeedsRaw:         true,
		IncludeReasoning: true,
	}

	if err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildRequestFn,
		identityParseFinal,
		ExtractResponseContent,
		ExtractResponseContentBytes,
		newTestResult,
	); err != nil {
		t.Fatalf("RunCallOrchestration: %v", err)
	}
	close(out)

	// Mock server invariants
	if requestSeen.Load() != 1 {
		t.Fatalf("expected exactly one request to mock server, got %d", requestSeen.Load())
	}
	if !sawAuthorization.Load() {
		t.Error("Authorization header was missing on the upstream request")
	}
	if !sawCorrectAuthAlgo.Load() {
		t.Error("Authorization header did not start with AWS4-HMAC-SHA256 / pinned credential prefix")
	}
	if !sawAmzDate.Load() {
		t.Error("X-Amz-Date header was missing on the upstream request")
	}
	if !sawAmzContentSha.Load() {
		t.Error("X-Amz-Content-Sha256 header was missing on the upstream request")
	}
	if !strings.HasPrefix(sawPath, "/model/") || !strings.HasSuffix(sawPath, "/converse") {
		t.Errorf("upstream path = %q, want /model/<id>/converse", sawPath)
	}
	if bodyReceived == "" {
		t.Error("upstream body was empty")
	}

	// Orchestrator output: planned-metadata-absent + heartbeat + final.
	var (
		sawHeartbeat bool
		final        bamlutils.StreamResult
	)
	for r := range out {
		switch r.Kind() {
		case bamlutils.StreamResultKindHeartbeat:
			sawHeartbeat = true
		case bamlutils.StreamResultKindFinal:
			final = r
		case bamlutils.StreamResultKindError:
			t.Fatalf("unexpected error result: %v", r.Error())
		}
	}
	if !sawHeartbeat {
		t.Error("expected at least one heartbeat result")
	}
	if final == nil {
		t.Fatal("expected a final result")
	}
	if got, want := final.Final(), "42"; got != want {
		t.Errorf("Final = %q, want %q", got, want)
	}
	if got, want := final.Raw(), "42"; got != want {
		t.Errorf("Raw = %q, want %q (raw is text-only by construction)", got, want)
	}
	if got, want := final.Reasoning(), "thinking about it"; got != want {
		t.Errorf("Reasoning = %q, want %q (IncludeReasoning=true)", got, want)
	}
}

// TestRunCallOrchestration_AWSBedrock_NoReasoning verifies the
// reasoning channel stays empty when IncludeReasoning is false, even
// when the upstream Converse response contains a reasoningContent
// block. Parseable and raw must be identical to the IncludeReasoning=true
// case — the flag never affects parseable or raw.
func TestRunCallOrchestration_AWSBedrock_NoReasoning(t *testing.T) {
	pinnedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"output":{"message":{"role":"assistant","content":[
				{"reasoningContent":{"reasoningText":{"text":"should not surface"}}},
				{"text":"answer-only"}
			]}}
		}`)
	}))
	defer server.Close()

	buildRequestFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		return &llmhttp.Request{
			URL:     server.URL + "/model/foo/converse",
			Method:  "POST",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{}`,
			AWSAuth: AWSAuthConfigForTest(pinnedTime),
		}, nil
	}

	out := make(chan bamlutils.StreamResult, 16)
	if err := RunCallOrchestration(
		context.Background(), out,
		&CallConfig{Provider: "aws-bedrock", NeedsRaw: true, IncludeReasoning: false},
		llmhttp.NewClient(server.Client()),
		buildRequestFn,
		identityParseFinal,
		ExtractResponseContent,
		ExtractResponseContentBytes,
		newTestResult,
	); err != nil {
		t.Fatalf("RunCallOrchestration: %v", err)
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
	if got, want := final.Final(), "answer-only"; got != want {
		t.Errorf("Final = %q, want %q", got, want)
	}
	if got, want := final.Raw(), "answer-only"; got != want {
		t.Errorf("Raw = %q, want %q", got, want)
	}
	if final.Reasoning() != "" {
		t.Errorf("Reasoning = %q, want empty under IncludeReasoning=false", final.Reasoning())
	}
}

// AWSAuthConfigForTest constructs a pinned llmhttp.AWSAuthConfig used by
// the bedrock integration tests so signing is deterministic without
// touching the host's AWS configuration.
func AWSAuthConfigForTest(at time.Time) *llmhttp.AWSAuthConfig {
	return &llmhttp.AWSAuthConfig{
		Region:      "us-east-1",
		Service:     "bedrock",
		Credentials: bedrockStaticCreds{},
		NowFunc:     func() time.Time { return at },
	}
}
