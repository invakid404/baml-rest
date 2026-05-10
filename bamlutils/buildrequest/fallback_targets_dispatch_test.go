package buildrequest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// TestRunStreamOrchestration_FallbackTargets_RoutesBuildRequestToLeaf
// pins the streaming orchestrator's wrapper-vs-target dispatch
// (issue #237 PR 2). When the resolver has centralized an immediate
// RR fallback child to a leaf, StreamConfig.FallbackTargets[child]
// names that leaf and the orchestrator must pass IT (not the wrapper
// child) as the clientOverride to buildRequest. On success,
// WinnerClient must report the leaf — the dispatch target is the
// realised serving identity.
func TestRunStreamOrchestration_FallbackTargets_RoutesBuildRequestToLeaf(t *testing.T) {
	server := makeOpenAIServer([]string{"hi"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	var (
		mu        sync.Mutex
		overrides []string
	)
	buildFn := func(_ context.Context, clientOverride string) (*llmhttp.Request, error) {
		mu.Lock()
		overrides = append(overrides, clientOverride)
		mu.Unlock()
		return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
	}

	plan := &bamlutils.Metadata{Path: "buildrequest", Client: "MyFallback"}

	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: true,
		FallbackChain: []string{"InnerRR", "Sibling"},
		ClientProviders: map[string]string{
			"InnerRR": "openai",
			"Sibling": "openai",
		},
		// PR 2 resolver populates this when InnerRR's RR resolution
		// selected the leaf "A". The orchestrator must dispatch BR
		// against "A", not "InnerRR".
		FallbackTargets:   map[string]string{"InnerRR": "A"},
		MetadataPlan:      plan,
		NewMetadataResult: newTestMetadataResult,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		buildFn,
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	gotOverrides := append([]string(nil), overrides...)
	mu.Unlock()
	if len(gotOverrides) != 1 {
		t.Fatalf("expected exactly one buildRequest call, got %d: %v", len(gotOverrides), gotOverrides)
	}
	if gotOverrides[0] != "A" {
		t.Errorf("clientOverride for first child: got %q, want %q (FallbackTargets[InnerRR]=A must drive dispatch, not the wrapper)",
			gotOverrides[0], "A")
	}

	// Outcome metadata must report the leaf as the winner. Sweep through
	// the channel for the outcome event.
	planned, outcome, _, _ := collectMetadata(t, out)
	if planned == nil {
		t.Fatal("expected planned metadata event")
	}
	if outcome == nil {
		t.Fatal("expected outcome metadata event on success")
	}
	if outcome.WinnerClient != "A" {
		t.Errorf("outcome WinnerClient: got %q, want %q (centralized leaf is the realised serving client)",
			outcome.WinnerClient, "A")
	}
	if outcome.WinnerPath != "buildrequest" {
		t.Errorf("outcome WinnerPath: got %q, want buildrequest", outcome.WinnerPath)
	}
}

// TestRunStreamOrchestration_FallbackTargets_EmptyPassesThrough pins
// the backward-compatible shape: a chain with no FallbackTargets
// entries (or an entry equal to child) keeps the pre-PR-2 dispatch
// identity — clientOverride equals the chain-position name. Crucial
// for codegen sites that haven't migrated to populating FallbackTargets
// yet (PR 3 wires those).
func TestRunStreamOrchestration_FallbackTargets_EmptyPassesThrough(t *testing.T) {
	server := makeOpenAIServer([]string{"ok"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	var seen atomic.Value // string
	buildFn := func(_ context.Context, clientOverride string) (*llmhttp.Request, error) {
		seen.Store(clientOverride)
		return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
	}

	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: true,
		FallbackChain: []string{"OnlyChild"},
		ClientProviders: map[string]string{
			"OnlyChild": "openai",
		},
		// No FallbackTargets — pre-PR-2 shape.
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, client,
		buildFn,
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := seen.Load().(string)
	if got != "OnlyChild" {
		t.Errorf("clientOverride: got %q, want %q (no FallbackTargets entry must dispatch verbatim)",
			got, "OnlyChild")
	}
}

// TestRunStreamOrchestration_FallbackTargets_LegacyChildStaysAtWrapper
// pins the wrapper-rooted legacy callback path: even when a sibling
// has a FallbackTargets entry, true legacy children continue to
// dispatch with the chain-position name (the wrapper) as the
// clientOverride. The legacy callback's scoped registry depends on
// the wrapper identity to preserve runtime strategy overrides for
// genuinely-deferred RR shapes (see #199 / #224 codegen scope split).
func TestRunStreamOrchestration_FallbackTargets_LegacyChildStaysAtWrapper(t *testing.T) {
	out := make(chan bamlutils.StreamResult, 100)

	var (
		legacyOverride string
		legacyProvider string
	)
	legacyCalled := atomic.Int32{}
	legacyFn := func(_ context.Context, clientOverride, provider string, _ bool, sendHeartbeat func()) (any, string, error) {
		legacyCalled.Add(1)
		legacyOverride = clientOverride
		legacyProvider = provider
		sendHeartbeat()
		return "legacy ok", "", nil
	}

	config := &StreamConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		NeedsPartials: false,
		FallbackChain: []string{"BedrockWrapper"},
		ClientProviders: map[string]string{
			"BedrockWrapper": "aws-bedrock",
		},
		LegacyChildren: map[string]bool{"BedrockWrapper": true},
		// A bogus FallbackTargets entry that, if (incorrectly) honored
		// for legacy dispatch, would surface as legacyOverride=="bypass".
		FallbackTargets:   map[string]string{"BedrockWrapper": "bypass"},
		LegacyStreamChild: legacyFn,
	}

	err := RunStreamOrchestration(
		context.Background(), out, config, nil,
		func(_ context.Context, _ string) (*llmhttp.Request, error) {
			t.Fatal("BR path must not be invoked for legacy-only chain")
			return nil, nil
		},
		nil,
		func(_ context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if legacyCalled.Load() != 1 {
		t.Fatalf("expected legacy callback invoked once, got %d", legacyCalled.Load())
	}
	if legacyOverride != "BedrockWrapper" {
		t.Errorf("legacy clientOverride: got %q, want %q (legacy dispatch must stay rooted at wrapper; FallbackTargets is BR-only)",
			legacyOverride, "BedrockWrapper")
	}
	if legacyProvider != "aws-bedrock" {
		t.Errorf("legacy provider: got %q, want aws-bedrock (ClientProviders[child] feeds legacy callback)", legacyProvider)
	}
}

// TestRunCallOrchestration_FallbackTargets_RoutesBuildRequestToLeaf
// mirrors the streaming-side test on the non-streaming call path so
// the dispatch contract is identical across both orchestrators.
func TestRunCallOrchestration_FallbackTargets_RoutesBuildRequestToLeaf(t *testing.T) {
	// Build a simple JSON response server. The call orchestrator's
	// extractResponse callback consumes the body; we just have to
	// return something it can decode.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 16)

	var (
		mu        sync.Mutex
		overrides []string
	)
	buildFn := func(_ context.Context, clientOverride string) (*llmhttp.Request, error) {
		mu.Lock()
		overrides = append(overrides, clientOverride)
		mu.Unlock()
		return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
	}

	plan := &bamlutils.Metadata{Path: "buildrequest", Client: "MyFallback"}

	config := &CallConfig{
		RetryPolicy:   &retry.Policy{MaxRetries: 0},
		FallbackChain: []string{"InnerRR", "Sibling"},
		ClientProviders: map[string]string{
			"InnerRR": "openai",
			"Sibling": "openai",
		},
		FallbackTargets:   map[string]string{"InnerRR": "A"},
		MetadataPlan:      plan,
		NewMetadataResult: newTestMetadataResult,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		buildFn,
		func(_ context.Context, s string) (any, error) { return s, nil },
		func(_ string, body string, _ bool) (string, string, error) { return body, body, nil },
		newTestResult,
	)
	close(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	gotOverrides := append([]string(nil), overrides...)
	mu.Unlock()
	if len(gotOverrides) != 1 {
		t.Fatalf("expected exactly one buildRequest call, got %d: %v", len(gotOverrides), gotOverrides)
	}
	if gotOverrides[0] != "A" {
		t.Errorf("clientOverride for first child: got %q, want %q (call-side dispatch must honor FallbackTargets)",
			gotOverrides[0], "A")
	}

	planned, outcome, _, _ := collectMetadata(t, out)
	if planned == nil {
		t.Fatal("expected planned metadata event")
	}
	if outcome == nil {
		t.Fatal("expected outcome metadata event on success")
	}
	if outcome.WinnerClient != "A" {
		t.Errorf("outcome WinnerClient: got %q, want %q", outcome.WinnerClient, "A")
	}
}
