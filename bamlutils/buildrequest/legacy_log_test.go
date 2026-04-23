package buildrequest

import (
	"context"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// recordingLogger captures Warn invocations for assertion.
type recordingLogger struct {
	mu    sync.Mutex
	warns int
	last  []any
}

func (r *recordingLogger) Debug(string, ...any) {}
func (r *recordingLogger) Info(string, ...any)  {}
func (r *recordingLogger) Warn(_ string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warns++
	r.last = args
}
func (r *recordingLogger) Error(string, ...any) {}

// loggingAdapter is a mockAdapter that returns a configured logger.
type loggingAdapter struct {
	mockAdapter
	logger bamlutils.Logger
}

func (l *loggingAdapter) Logger() bamlutils.Logger { return l.logger }

func newLoggingAdapter(logger bamlutils.Logger) *loggingAdapter {
	return &loggingAdapter{
		mockAdapter: mockAdapter{Context: context.Background()},
		logger:      logger,
	}
}

// resetLegacyAlertSeen wipes the package-private dedup state so a test
// starts from a known baseline. Tests that touch this are not safe to
// run in parallel with each other.
func resetLegacyAlertSeen(t *testing.T) {
	t.Helper()
	legacyAlertSeen = sync.Map{}
}

func TestLogLegacyClassification_DedupSameKey(t *testing.T) {
	resetLegacyAlertSeen(t)

	logger := &recordingLogger{}
	adapter := newLoggingAdapter(logger)
	plan := &bamlutils.Metadata{
		Path:       "legacy",
		PathReason: PathReasonUnsupportedProvider,
		Client:     "MyClient",
		Provider:   "aws-bedrock",
	}

	// First call must log.
	LogLegacyClassification(adapter, "MyMethod", plan)
	if logger.warns != 1 {
		t.Fatalf("first call: expected 1 warn, got %d", logger.warns)
	}
	// Second call with the same key must NOT log.
	LogLegacyClassification(adapter, "MyMethod", plan)
	if logger.warns != 1 {
		t.Fatalf("dedup failed: expected still 1 warn after second call, got %d", logger.warns)
	}
	// Third call from a different invocation but same key still dedups.
	LogLegacyClassification(adapter, "MyMethod", plan)
	if logger.warns != 1 {
		t.Fatalf("dedup failed: expected still 1 warn after third call, got %d", logger.warns)
	}
}

func TestLogLegacyClassification_DifferentKeysLogIndependently(t *testing.T) {
	resetLegacyAlertSeen(t)

	logger := &recordingLogger{}
	adapter := newLoggingAdapter(logger)

	// Same reason+provider, different method — distinct key.
	plan1 := &bamlutils.Metadata{
		Path: "legacy", PathReason: PathReasonUnsupportedProvider, Provider: "aws-bedrock",
	}
	LogLegacyClassification(adapter, "MethodA", plan1)
	LogLegacyClassification(adapter, "MethodB", plan1)
	if logger.warns != 2 {
		t.Fatalf("different methods: expected 2 warns, got %d", logger.warns)
	}

	// Same method+provider, different reason — distinct key.
	plan2 := &bamlutils.Metadata{
		Path: "legacy", PathReason: PathReasonEmptyProvider, Provider: "aws-bedrock",
	}
	LogLegacyClassification(adapter, "MethodA", plan2)
	if logger.warns != 3 {
		t.Fatalf("different reason: expected 3 warns, got %d", logger.warns)
	}

	// Same method+reason, different provider — distinct key.
	plan3 := &bamlutils.Metadata{
		Path: "legacy", PathReason: PathReasonUnsupportedProvider, Provider: "different-provider",
	}
	LogLegacyClassification(adapter, "MethodA", plan3)
	if logger.warns != 4 {
		t.Fatalf("different provider: expected 4 warns, got %d", logger.warns)
	}
}

func TestLogLegacyClassification_SkipsDeliberateReasons(t *testing.T) {
	resetLegacyAlertSeen(t)

	logger := &recordingLogger{}
	adapter := newLoggingAdapter(logger)

	for _, reason := range []string{
		PathReasonRoundRobin,
		PathReasonFallbackAllLegacy,
		PathReasonBuildRequestDisabled,
	} {
		plan := &bamlutils.Metadata{Path: "legacy", PathReason: reason, Provider: "x"}
		LogLegacyClassification(adapter, "M", plan)
	}
	if logger.warns != 0 {
		t.Fatalf("deliberate reasons should not warn; got %d", logger.warns)
	}
}

func TestLogLegacyClassification_SkipsBuildRequestPath(t *testing.T) {
	resetLegacyAlertSeen(t)

	logger := &recordingLogger{}
	adapter := newLoggingAdapter(logger)

	// Path="buildrequest" must never produce a legacy-route warn even if
	// some upstream code accidentally set a non-empty PathReason.
	plan := &bamlutils.Metadata{
		Path:       "buildrequest",
		PathReason: PathReasonUnsupportedProvider,
		Provider:   "aws-bedrock",
	}
	LogLegacyClassification(adapter, "M", plan)
	if logger.warns != 0 {
		t.Fatalf("buildrequest-path plan should not warn; got %d", logger.warns)
	}
}

func TestLogLegacyClassification_NilPlanIsNoop(t *testing.T) {
	resetLegacyAlertSeen(t)

	logger := &recordingLogger{}
	adapter := newLoggingAdapter(logger)

	LogLegacyClassification(adapter, "M", nil)
	if logger.warns != 0 {
		t.Fatalf("nil plan should not warn; got %d", logger.warns)
	}
}
