package buildrequest

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// newTestMetadataResult satisfies NewMetadataResultFunc for tests; the
// production version comes from generated per-method code that uses a
// pooled struct, but tests just need a plain pointer-backed value.
func newTestMetadataResult(md *bamlutils.Metadata) bamlutils.StreamResult {
	return &testResult{kind: bamlutils.StreamResultKindMetadata, metadata: md}
}

func collectMetadata(t *testing.T, ch <-chan bamlutils.StreamResult) (planned, outcome *bamlutils.Metadata, allKinds []bamlutils.StreamResultKind) {
	t.Helper()
	for r := range ch {
		allKinds = append(allKinds, r.Kind())
		if r.Kind() == bamlutils.StreamResultKindMetadata {
			md := r.Metadata()
			if md == nil {
				t.Fatalf("metadata kind without payload")
			}
			switch md.Phase {
			case bamlutils.MetadataPhasePlanned:
				if planned != nil {
					t.Fatalf("planned emitted twice")
				}
				cp := *md
				planned = &cp
			case bamlutils.MetadataPhaseOutcome:
				if outcome != nil {
					t.Fatalf("outcome emitted twice")
				}
				cp := *md
				outcome = &cp
			}
		}
	}
	return planned, outcome, allKinds
}

// TestRunStreamOrchestration_EmitsPlannedAfterHeartbeat verifies the
// ordering invariant from §4a: heartbeat is the first event, planned
// metadata is the second. This guarantees the pool's first-byte tracking
// and reset injection logic continues to work.
func TestRunStreamOrchestration_EmitsPlannedAfterHeartbeat(t *testing.T) {
	server := makeOpenAIServer([]string{"hi"})
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	plan := &bamlutils.Metadata{
		Path:     "buildrequest",
		Client:   "MyClient",
		Provider: "openai",
	}

	config := &StreamConfig{
		Provider:          "openai",
		NeedsPartials:     false,
		NeedsRaw:          false,
		MetadataPlan:      plan,
		NewMetadataResult: newTestMetadataResult,
	}

	err := RunStreamOrchestration(
		context.Background(),
		out,
		config,
		client,
		func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		func(ctx context.Context, accumulated string) (any, error) { return accumulated, nil },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	planned, outcome, kinds := collectMetadata(t, out)
	if planned == nil {
		t.Fatalf("expected planned metadata to be emitted")
	}
	if outcome == nil {
		t.Fatalf("expected outcome metadata to be emitted")
	}

	// Heartbeat must be first; planned metadata second.
	if len(kinds) < 2 {
		t.Fatalf("expected at least heartbeat + planned events; got %d", len(kinds))
	}
	if kinds[0] != bamlutils.StreamResultKindHeartbeat {
		t.Errorf("first event should be heartbeat; got kind %d", kinds[0])
	}
	if kinds[1] != bamlutils.StreamResultKindMetadata {
		t.Errorf("second event should be metadata (planned); got kind %d", kinds[1])
	}

	// Phase invariants.
	if planned.Phase != bamlutils.MetadataPhasePlanned {
		t.Errorf("planned phase: got %q, want planned", planned.Phase)
	}
	if outcome.Phase != bamlutils.MetadataPhaseOutcome {
		t.Errorf("outcome phase: got %q, want outcome", outcome.Phase)
	}
	if planned.Path != "buildrequest" || planned.Client != "MyClient" {
		t.Errorf("planned client/path: got %+v", planned)
	}
	if outcome.WinnerProvider != "openai" {
		t.Errorf("outcome winner provider: got %q, want openai", outcome.WinnerProvider)
	}
	if outcome.RetryCount == nil || *outcome.RetryCount != 0 {
		t.Errorf("outcome retry count: got %v, want 0", outcome.RetryCount)
	}
}

// TestRunStreamOrchestration_NoMetadataPlanIsNoop verifies that callers
// who don't care about routing metadata (legacy tests, deliberate opt-out)
// see no metadata events at all.
func TestRunStreamOrchestration_NoMetadataPlanIsNoop(t *testing.T) {
	server := makeOpenAIServer([]string{"hi"})
	defer server.Close()

	out := make(chan bamlutils.StreamResult, 100)
	config := &StreamConfig{Provider: "openai"}
	err := RunStreamOrchestration(
		context.Background(), out, config,
		llmhttp.NewClient(server.Client()),
		func(ctx context.Context, _ string) (*llmhttp.Request, error) {
			return &llmhttp.Request{URL: server.URL, Method: "POST", Body: `{}`}, nil
		},
		func(ctx context.Context, s string) (any, error) { return s, nil },
		func(ctx context.Context, s string) (any, error) { return s, nil },
		newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	for r := range out {
		if r.Kind() == bamlutils.StreamResultKindMetadata {
			t.Errorf("metadata event emitted despite nil MetadataPlan")
		}
	}
}
