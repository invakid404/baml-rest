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

// TestClassifyWorkerError_CancelBranchWrapsCtxErr pins the
// race-window contract for Pool.Parse / Pool.CallStream's caller-
// cancel branch: when ctx is cancelled but getWorkerForRetry
// returns a non-cancel error (e.g. "no healthy workers available"
// from a transient race), wrapping ctx.Err() — not the pool err —
// is what lets classifyWorkerError detect the cancellation and
// surface request_canceled. Wrapping the pool err alone would
// leave context.Canceled out of the chain entirely and the
// retry-exhaustion sentinel branch would otherwise capture the
// shape as worker_unavailable.
func TestClassifyWorkerError_CancelBranchWrapsCtxErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			"context.Canceled wrapped, pool err as %v",
			fmt.Errorf("retry failed, no workers available: %w (pool error: %v)", context.Canceled, errors.New("no healthy workers available")),
		},
		{
			"context.DeadlineExceeded wrapped, pool err as %v",
			fmt.Errorf("retry failed, no workers available: %w (pool error: %v)", context.DeadlineExceeded, errors.New("pool is closed")),
		},
		{
			"with previous lastErr in message",
			fmt.Errorf("retry failed, no workers available: %w (pool error: %v, previous: %v)", context.Canceled, errors.New("no healthy workers available"), errors.New("worker died")),
		},
		// CallStream pre-loop cancel shape: caller cancelled before
		// the goroutine even started, getWorkerForRetry surfaced a
		// non-cancel error in the race window. The pre-loop wrap
		// (pool.go:1421) emits this exact format.
		{
			"CallStream pre-loop cancel wrap",
			fmt.Errorf("could not acquire worker: %w (pool error: %v)", context.Canceled, errors.New("pool is draining")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyWorkerError(tt.err)
			if got != apierror.CodeRequestCanceled {
				t.Errorf("classifyWorkerError = %q, want %q", got, apierror.CodeRequestCanceled)
			}
		})
	}
}

// TestClassifyWorkerError_NoFalsePositiveOnContextCanceledText pins
// the typed-cancellation contract: a plain worker error string that
// happens to contain "context canceled" or "context deadline
// exceeded" verbatim (e.g. an upstream LLM provider error message)
// must NOT be classified as request_canceled. Previously
// classifyWorkerError used pool.IsCallerCancellationError, whose
// trailing string-match fallback would fire on these substrings —
// turning a real worker_error into a misleading 408. Switched to
// pool.IsTypedCancellationError which uses only typed signals
// (errors.Is, *status.Status, "rpc error: code = Canceled" prefix).
func TestClassifyWorkerError_NoFalsePositiveOnContextCanceledText(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want apierror.Code
	}{
		{
			"upstream LLM timeout containing canceled text",
			errors.New("openai: provider error: context deadline exceeded"),
			apierror.CodeWorkerError,
		},
		{
			"BAML parse failure containing canceled text",
			errors.New("BAML parse error: validation failed; context canceled at line 12"),
			apierror.CodeWorkerError,
		},
		{
			"plain canceled-shaped error stays worker_error",
			errors.New("context canceled by upstream"),
			apierror.CodeWorkerError,
		},
		// Sanity: typed forms still classify correctly.
		{
			"typed context.Canceled still classifies as canceled",
			context.Canceled,
			apierror.CodeRequestCanceled,
		},
		{
			"serialized gRPC Canceled still classifies as canceled",
			errors.New("rpc error: code = Canceled desc = client gone"),
			apierror.CodeRequestCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyWorkerError(tt.err)
			if got != tt.want {
				t.Errorf("classifyWorkerError(%q) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestClassifyWorkerError_PoolUnavailable pins the admission-side
// sentinel: pre-retry pool-availability failures (pool closed,
// draining, no healthy workers) classify as worker_unavailable rather
// than falling through to worker_error on plain string matching.
// Without ErrPoolUnavailable wrapping, "pool is closed" or "no healthy
// workers available" would surface as worker_error and clients
// wouldn't know to retry against transient infra unavailability.
func TestClassifyWorkerError_PoolUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"pool closed", fmt.Errorf("%w: pool is closed", pool.ErrPoolUnavailable)},
		{"pool draining", fmt.Errorf("%w: pool is draining, not accepting new requests", pool.ErrPoolUnavailable)},
		{"no healthy workers", fmt.Errorf("%w: no healthy workers available", pool.ErrPoolUnavailable)},
		{"bare sentinel", pool.ErrPoolUnavailable},
		{"sentinel through outer wrap", fmt.Errorf("call failed: %w", fmt.Errorf("%w: pool is closed", pool.ErrPoolUnavailable))},
		// CallStream pre-loop wrap shape: getWorkerForRetry returned
		// a non-cancel error and the caller didn't cancel. The
		// pre-loop wrap (pool.go:1421) emits this exact format.
		{"CallStream pre-loop wrap", fmt.Errorf("%w: %w", pool.ErrPoolUnavailable, errors.New("pool is draining"))},
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
			"context.DeadlineExceeded → request_canceled",
			context.DeadlineExceeded,
			apierror.CodeRequestCanceled,
		},
		{
			"gRPC Canceled status → request_canceled",
			status.Error(codes.Canceled, "client gone"),
			apierror.CodeRequestCanceled,
		},
		{
			"gRPC DeadlineExceeded status → request_canceled",
			status.Error(codes.DeadlineExceeded, "request timed out"),
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

// TestClassifyStreamResultError_StructuredDetailsOverride is the
// streaming-side counterpart to TestClassifyWorkerError_StructuredDetailsBeatStacktrace:
// when a worker emits both ErrorDetails (a valid JSON object) and a
// Stacktrace, the structured payload wins and the stacktrace stays
// out of the response (it's still logged via httplogger.SetError on
// the calling path). Pins the precedence so a future refactor of
// classifyStreamResultError can't silently regress to clobbering
// worker-supplied structured details with the stacktrace fallback.
func TestClassifyStreamResultError_StructuredDetailsOverride(t *testing.T) {
	wantDetails := []byte(`{"foo":"bar","field":"sentiment"}`)
	result := &workerplugin.StreamResult{
		Kind:         workerplugin.StreamResultKindError,
		Error:        errors.New("BAML parse error"),
		ErrorDetails: wantDetails,
		Stacktrace:   "goroutine 9 [running]:\nshould.not.appear(...)",
	}

	_, details := classifyStreamResultError(result)
	if string(details) != string(wantDetails) {
		t.Errorf("details = %s, want %s", details, wantDetails)
	}
	if strings.Contains(string(details), "stacktrace") {
		t.Errorf("details should not include stacktrace when structured details supplied; got %s", details)
	}

	var parsed map[string]string
	if err := json.Unmarshal(details, &parsed); err != nil {
		t.Fatalf("details did not unmarshal as object: %v (raw: %s)", err, details)
	}
	if parsed["foo"] != "bar" || parsed["field"] != "sentiment" {
		t.Errorf("unmarshaled details = %v, want foo=bar field=sentiment", parsed)
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

// TestNormalizeWorkerMetadata pins the contract: worker-supplied codes
// are validated against the public apierror.Code enum, and details are
// validated as JSON objects. Anything off-contract is dropped — the
// host-side classifier then takes over for code, and a stacktrace
// fallback (handled by the caller) replaces malformed details.
func TestNormalizeWorkerMetadata(t *testing.T) {
	tests := []struct {
		name        string
		rawCode     string
		rawDetails  []byte
		wantCode    apierror.Code
		wantDetails string // empty means details should be nil
	}{
		// Code validation
		{"known code preserved", string(apierror.CodeParseError), nil, apierror.CodeParseError, ""},
		{"known worker_error preserved", string(apierror.CodeWorkerError), nil, apierror.CodeWorkerError, ""},
		{"unknown code dropped", "made_up_code", nil, "", ""},
		{"empty code stays empty", "", nil, "", ""},
		{"case-mismatch unknown dropped", "Worker_Error", nil, "", ""},

		// Details validation
		{"valid object preserved", "", []byte(`{"field":"x"}`), "", `{"field":"x"}`},
		{"empty object preserved", "", []byte(`{}`), "", `{}`},
		{"object with leading whitespace preserved", "", []byte(" \n{\"x\":1}"), "", " \n{\"x\":1}"},
		{"json null dropped", "", []byte(`null`), "", ""},
		{"json scalar dropped", "", []byte(`42`), "", ""},
		{"json string dropped", "", []byte(`"oops"`), "", ""},
		{"json bool dropped", "", []byte(`true`), "", ""},
		{"json array dropped", "", []byte(`[1,2,3]`), "", ""},
		{"invalid json dropped", "", []byte(`{not json}`), "", ""},
		{"empty bytes stays nil", "", []byte{}, "", ""},
		{"nil bytes stays nil", "", nil, "", ""},

		// Combined
		{"valid code + valid object both preserved", string(apierror.CodeParseError), []byte(`{"a":1}`), apierror.CodeParseError, `{"a":1}`},
		{"valid code + invalid details: code kept, details dropped", string(apierror.CodeParseError), []byte(`null`), apierror.CodeParseError, ""},
		{"invalid code + valid details: code dropped, details kept", "bogus", []byte(`{"a":1}`), "", `{"a":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCode, gotDetails := normalizeWorkerMetadata(tt.rawCode, tt.rawDetails)
			if gotCode != tt.wantCode {
				t.Errorf("code = %q, want %q", gotCode, tt.wantCode)
			}
			if tt.wantDetails == "" {
				if gotDetails != nil {
					t.Errorf("details = %s, want nil", gotDetails)
				}
			} else {
				if string(gotDetails) != tt.wantDetails {
					t.Errorf("details = %s, want %s", gotDetails, tt.wantDetails)
				}
			}
		})
	}
}

// TestClassifyWorkerError_UnknownWorkerCodeFallsThrough verifies that
// a worker emitting an off-contract code falls through to host-side
// classification rather than introducing the unknown code into the
// response envelope. Locks the public OpenAPI enum as authoritative.
func TestClassifyWorkerError_UnknownWorkerCodeFallsThrough(t *testing.T) {
	err := workerplugin.NewErrorWithMetadata(
		status.Error(codes.Unavailable, "worker died"),
		"",
		"made_up_code",
		nil,
	)
	got, _ := classifyWorkerError(err)
	if got != apierror.CodeWorkerUnavailable {
		t.Errorf("classifyWorkerError = %q, want %q (host-side reclassification of inner Unavailable)", got, apierror.CodeWorkerUnavailable)
	}
}

// TestClassifyWorkerError_ScalarDetailsFallsBackToStacktrace verifies
// that worker-supplied non-object details get dropped, and when a
// stacktrace is available it's used to synthesize the response
// details instead. Without normalization the schema-violating scalar
// would have been forwarded.
func TestClassifyWorkerError_ScalarDetailsFallsBackToStacktrace(t *testing.T) {
	const trace = "goroutine 1 [running]:\npanic..."
	err := workerplugin.NewErrorWithMetadata(
		errors.New("worker panic"),
		trace,
		"",
		[]byte(`null`), // schema-violating
	)
	_, details := classifyWorkerError(err)
	if details == nil {
		t.Fatal("expected stacktrace fallback, got nil details")
	}
	var parsed struct {
		Stacktrace string `json:"stacktrace"`
	}
	if err := json.Unmarshal(details, &parsed); err != nil {
		t.Fatalf("details did not unmarshal as {stacktrace}: %v (raw: %s)", err, details)
	}
	if parsed.Stacktrace != trace {
		t.Errorf("details.stacktrace = %q, want %q", parsed.Stacktrace, trace)
	}
}

// TestClassifyStreamResultError_NormalizesWorkerFields mirrors the
// unary normalization tests for the streaming path: worker-supplied
// fields on a StreamResult go through the same gate.
func TestClassifyStreamResultError_NormalizesWorkerFields(t *testing.T) {
	t.Run("unknown code falls through", func(t *testing.T) {
		result := &workerplugin.StreamResult{
			Kind:      workerplugin.StreamResultKindError,
			Error:     status.Error(codes.Unavailable, "worker died"),
			ErrorCode: "fictional_code",
		}
		got, _ := classifyStreamResultError(result)
		if got != apierror.CodeWorkerUnavailable {
			t.Errorf("code = %q, want %q", got, apierror.CodeWorkerUnavailable)
		}
	})

	t.Run("scalar details dropped, stacktrace replaces", func(t *testing.T) {
		const trace = "goroutine 7..."
		result := &workerplugin.StreamResult{
			Kind:         workerplugin.StreamResultKindError,
			Error:        errors.New("worker panic"),
			ErrorDetails: []byte(`42`),
			Stacktrace:   trace,
		}
		_, details := classifyStreamResultError(result)
		if details == nil {
			t.Fatalf("expected stacktrace fallback, got nil details")
		}
		var parsed struct {
			Stacktrace string `json:"stacktrace"`
		}
		if err := json.Unmarshal(details, &parsed); err != nil {
			t.Fatalf("details did not unmarshal as {stacktrace}: %v (raw: %s)", err, details)
		}
		if parsed.Stacktrace != trace {
			t.Errorf("details.stacktrace = %q, want %q", parsed.Stacktrace, trace)
		}
	})
}
