package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClassifyWorkerError_RetriesExhaustedBeatsCancellation pins the
// precedence rule that the pool's retry-exhaustion sentinel takes
// priority over caller-cancellation detection. The pool's last
// retryable error is often a hung-detection-driven gRPC Canceled
// (worker side aborted the request after first-byte timeout) — without
// this ordering, IsCallerCancellationError would walk the wrap chain
// via status.FromError, find the inner Canceled, and misreport a
// retry-exhaustion as a client-driven abort. Regression guard for the
// P1 ordering bug Codex flagged after the initial sentinel landed.
func TestClassifyWorkerError_RetriesExhaustedBeatsCancellation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			"exhausted wrapping gRPC Canceled",
			fmt.Errorf("%w: %w", pool.ErrPoolRetriesExhausted, status.Error(codes.Canceled, "hung-detected")),
		},
		{
			"exhausted wrapping gRPC DeadlineExceeded",
			fmt.Errorf("%w: %w", pool.ErrPoolRetriesExhausted, status.Error(codes.DeadlineExceeded, "first-byte timeout")),
		},
		{
			"exhausted wrapping gRPC Unavailable",
			fmt.Errorf("%w: %w", pool.ErrPoolRetriesExhausted, status.Error(codes.Unavailable, "worker died")),
		},
		{
			"exhausted with no inner error",
			fmt.Errorf("%w (no terminal stream frame)", pool.ErrPoolRetriesExhausted),
		},
		// No-workers retry tail: getWorkerForRetry can't supply a
		// replacement worker after a retryable failure, and the pool
		// wraps with the sentinel so the HTTP layer classifies as
		// worker_unavailable rather than falling through to
		// worker_error on the plain "no healthy workers available"
		// string. Caller-cancellation paths bypass the wrap (verified
		// separately via the precedence test below).
		{
			"exhausted wrapping no healthy workers",
			fmt.Errorf("%w: %w", pool.ErrPoolRetriesExhausted, errors.New("no healthy workers available")),
		},
		{
			"exhausted wrapping pool closed",
			fmt.Errorf("%w: %w", pool.ErrPoolRetriesExhausted, errors.New("pool is closed")),
		},
		{
			"exhausted wrapping no-workers with previous",
			fmt.Errorf("%w: retry failed, no workers available: %w (previous: worker died)", pool.ErrPoolRetriesExhausted, errors.New("no healthy workers available")),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := classifyWorkerError(tt.err)
			if code != apierror.CodeWorkerUnavailable {
				t.Errorf("classifyWorkerError(%v) code = %q, want %q", tt.err, code, apierror.CodeWorkerUnavailable)
			}
		})
	}
}

// TestClassifyWorkerError_PrecedenceOrder spot-checks the full
// classification ladder so future reorderings can't silently break a
// rung. Each case exercises exactly one rung.
func TestClassifyWorkerError_PrecedenceOrder(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want apierror.Code
	}{
		{
			"worker-supplied code wins over everything",
			workerplugin.NewErrorWithMetadata(
				status.Error(codes.Canceled, "client gone"),
				"",
				string(apierror.CodeParseError),
				nil,
			),
			apierror.CodeParseError,
		},
		{
			"context.Canceled → request_canceled",
			context.Canceled,
			apierror.CodeRequestCanceled,
		},
		{
			"gRPC Canceled status → request_canceled",
			status.Error(codes.Canceled, "client gone"),
			apierror.CodeRequestCanceled,
		},
		{
			"gRPC Unavailable → worker_unavailable",
			status.Error(codes.Unavailable, "worker dead"),
			apierror.CodeWorkerUnavailable,
		},
		{
			"plain BAML-shaped error → worker_error",
			errors.New("BAML validation failed: field 'x' required"),
			apierror.CodeWorkerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyWorkerError(tt.err)
			if got != tt.want {
				t.Errorf("classifyWorkerError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestClassifyWorkerError_StacktraceDetails pins the contract that a
// worker-supplied stacktrace surfaces in the response Details field
// as {"stacktrace": "..."} when no structured Details were provided.
// Previously the stacktrace was logged server-side only and never
// reached the client envelope despite the PR comment promising it
// would — Codex caught the gap and this test locks the fix.
func TestClassifyWorkerError_StacktraceDetails(t *testing.T) {
	const trace = "goroutine 42 [running]:\nmain.boom(...)\n\tfile.go:10"
	err := workerplugin.NewErrorWithMetadata(
		errors.New("worker panic"),
		trace,
		"",
		nil,
	)

	_, details := classifyWorkerError(err)
	if details == nil {
		t.Fatalf("expected non-nil details carrying stacktrace, got nil")
	}

	var parsed struct {
		Stacktrace string `json:"stacktrace"`
	}
	if err := json.Unmarshal(details, &parsed); err != nil {
		t.Fatalf("details did not unmarshal as {stacktrace}: %v (raw: %s)", err, string(details))
	}
	if parsed.Stacktrace != trace {
		t.Errorf("details.stacktrace = %q, want %q", parsed.Stacktrace, trace)
	}
}

// TestClassifyWorkerError_StructuredDetailsBeatStacktrace verifies
// that worker-supplied structured details take priority over the
// stacktrace fallback — when both are present the structured payload
// is what reaches the client (the stacktrace is still logged via
// httplogger.SetError on the calling path).
func TestClassifyWorkerError_StructuredDetailsBeatStacktrace(t *testing.T) {
	wantDetails := []byte(`{"field":"sentiment","expected":"positive|negative"}`)
	err := workerplugin.NewErrorWithMetadata(
		errors.New("BAML parse error"),
		"goroutine 42 [running]:\n...",
		"",
		wantDetails,
	)

	_, details := classifyWorkerError(err)
	if string(details) != string(wantDetails) {
		t.Errorf("details = %s, want %s", string(details), string(wantDetails))
	}
	if strings.Contains(string(details), "stacktrace") {
		t.Errorf("details should not include stacktrace when structured details supplied; got %s", string(details))
	}
}

// TestClassifyStreamResultError_StacktraceDetails covers the streaming
// counterpart: StreamResult.Stacktrace surfaces as
// {"stacktrace": "..."} when ErrorDetails is empty. Mirrors the unary
// path so streaming consumers get the same agent-facing context.
func TestClassifyStreamResultError_StacktraceDetails(t *testing.T) {
	const trace = "goroutine 7 [running]:\nworker.run(...)"
	result := &workerplugin.StreamResult{
		Kind:       workerplugin.StreamResultKindError,
		Error:      errors.New("worker panic"),
		Stacktrace: trace,
	}

	_, details := classifyStreamResultError(result)
	if details == nil {
		t.Fatalf("expected non-nil details, got nil")
	}
	var parsed struct {
		Stacktrace string `json:"stacktrace"`
	}
	if err := json.Unmarshal(details, &parsed); err != nil {
		t.Fatalf("details did not unmarshal: %v (raw: %s)", err, string(details))
	}
	if parsed.Stacktrace != trace {
		t.Errorf("details.stacktrace = %q, want %q", parsed.Stacktrace, trace)
	}
}
