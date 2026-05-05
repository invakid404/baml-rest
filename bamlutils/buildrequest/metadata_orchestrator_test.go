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

func collectMetadata(t *testing.T, ch <-chan bamlutils.StreamResult) (planned, outcome *bamlutils.Metadata, allKinds []bamlutils.StreamResultKind, metadataPhases []bamlutils.MetadataPhase) {
	t.Helper()
	for r := range ch {
		allKinds = append(allKinds, r.Kind())
		if r.Kind() == bamlutils.StreamResultKindMetadata {
			md := r.Metadata()
			if md == nil {
				t.Fatalf("metadata kind without payload")
			}
			// metadataPhases preserves the in-channel ORDER of the
			// metadata events so callers can assert "planned came
			// first". A phase-bucketed return alone (planned,
			// outcome) hides event order — a regression that emitted
			// outcome before planned could still satisfy
			// `kinds[0] == Metadata` and `planned != nil`.
			metadataPhases = append(metadataPhases, md.Phase)
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
	return planned, outcome, allKinds, metadataPhases
}

// TestRunStreamOrchestration_EmitsPlannedFirstThenHeartbeat verifies
// the planned-first ordering: planned metadata is emitted upfront
// from the orchestrator (before any HTTP work), and the heartbeat
// fires later when the upstream provider returns 2xx. Emitting
// heartbeat first and gating planned on the heartbeat CAS would lose
// the planned event for requests that completed without firing the
// onSuccess heartbeat (legacy path with WithClient targeting a
// strategy parent that resolves through static IR). Pool's
// first-byte tracking treats any first event as liveness, so
// planned-first does not break hung detection.
func TestRunStreamOrchestration_EmitsPlannedFirstThenHeartbeat(t *testing.T) {
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

	planned, outcome, kinds, metadataPhases := collectMetadata(t, out)
	if planned == nil {
		t.Fatalf("expected planned metadata to be emitted")
	}
	if outcome == nil {
		t.Fatalf("expected outcome metadata to be emitted")
	}

	// Success path emits exactly four frames in order: planned metadata
	// (upfront), heartbeat (sendHeartbeat on 2xx), outcome metadata
	// (just before final), final.
	if len(kinds) != 4 {
		t.Fatalf("expected exactly 4 frames (planned, heartbeat, outcome, final); got %d: %v", len(kinds), kinds)
	}
	if kinds[0] != bamlutils.StreamResultKindMetadata {
		t.Errorf("frame 0 should be planned metadata; got kind %d", kinds[0])
	}
	if kinds[1] != bamlutils.StreamResultKindHeartbeat {
		t.Errorf("frame 1 should be heartbeat; got kind %d", kinds[1])
	}
	if kinds[2] != bamlutils.StreamResultKindMetadata {
		t.Errorf("frame 2 should be outcome metadata; got kind %d", kinds[2])
	}
	if kinds[3] != bamlutils.StreamResultKindFinal {
		t.Errorf("frame 3 should be final; got kind %d", kinds[3])
	}
	// Pin that the FIRST metadata payload was MetadataPhasePlanned,
	// not just "some metadata kind first". A regression that swapped
	// planned/outcome emission order would still satisfy
	// `kinds[0] == Metadata` but flunk this check.
	if len(metadataPhases) == 0 || metadataPhases[0] != bamlutils.MetadataPhasePlanned {
		t.Errorf("first metadata phase should be planned; got %v", metadataPhases)
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

// TestRunCallOrchestration_EmitsPlannedAndOutcome mirrors the streaming
// orchestrator's metadata test for the non-streaming call path.
func TestRunCallOrchestration_EmitsPlannedAndOutcome(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"hi"}}]}`)
	defer server.Close()

	client := llmhttp.NewClient(server.Client())
	out := make(chan bamlutils.StreamResult, 100)

	plan := &bamlutils.Metadata{
		Path:     "buildrequest",
		Client:   "MyClient",
		Provider: "openai",
	}

	config := &CallConfig{
		Provider:          "openai",
		NeedsRaw:          false,
		MetadataPlan:      plan,
		NewMetadataResult: newTestMetadataResult,
	}

	err := RunCallOrchestration(
		context.Background(), out, config, client,
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
		newTestResult,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(out)

	planned, outcome, kinds, metadataPhases := collectMetadata(t, out)
	if planned == nil {
		t.Fatalf("expected planned metadata to be emitted")
	}
	if outcome == nil {
		t.Fatalf("expected outcome metadata to be emitted")
	}

	// Success path emits exactly four frames in order: planned, heartbeat,
	// outcome, final. See the streaming counterpart for rationale.
	if len(kinds) != 4 {
		t.Fatalf("expected exactly 4 frames (planned, heartbeat, outcome, final); got %d: %v", len(kinds), kinds)
	}
	if kinds[0] != bamlutils.StreamResultKindMetadata {
		t.Errorf("frame 0 should be planned metadata; got kind %d", kinds[0])
	}
	if kinds[1] != bamlutils.StreamResultKindHeartbeat {
		t.Errorf("frame 1 should be heartbeat; got kind %d", kinds[1])
	}
	if kinds[2] != bamlutils.StreamResultKindMetadata {
		t.Errorf("frame 2 should be outcome metadata; got kind %d", kinds[2])
	}
	if kinds[3] != bamlutils.StreamResultKindFinal {
		t.Errorf("frame 3 should be final; got kind %d", kinds[3])
	}
	// See streaming counterpart for rationale.
	if len(metadataPhases) == 0 || metadataPhases[0] != bamlutils.MetadataPhasePlanned {
		t.Errorf("first metadata phase should be planned; got %v", metadataPhases)
	}

	if outcome.WinnerProvider != "openai" {
		t.Errorf("outcome winner provider: got %q, want openai", outcome.WinnerProvider)
	}
	if outcome.WinnerPath != "buildrequest" {
		t.Errorf("outcome winner path: got %q, want buildrequest", outcome.WinnerPath)
	}
	if outcome.RetryCount == nil || *outcome.RetryCount != 0 {
		t.Errorf("outcome retry count: got %v, want 0", outcome.RetryCount)
	}
	if outcome.UpstreamDurMs == nil {
		t.Errorf("outcome upstream duration should be set")
	}
}

// TestRunCallOrchestration_NoMetadataPlanIsNoop verifies the non-streaming
// path also opts out cleanly when no plan is provided.
func TestRunCallOrchestration_NoMetadataPlanIsNoop(t *testing.T) {
	server := makeJSONServer(200, `{"choices":[{"message":{"content":"hi"}}]}`)
	defer server.Close()

	out := make(chan bamlutils.StreamResult, 100)
	config := &CallConfig{Provider: "openai"}
	err := RunCallOrchestration(
		context.Background(), out, config,
		llmhttp.NewClient(server.Client()),
		makeBuildCallRequest(server.URL),
		identityParseFinal,
		ExtractResponseContent,
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
