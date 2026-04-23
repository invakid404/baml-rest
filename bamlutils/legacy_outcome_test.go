package bamlutils

import (
	"testing"
)

func TestBuildLegacyOutcome_NilPlanned(t *testing.T) {
	t.Parallel()

	if got := BuildLegacyOutcome(nil, 100, "", "", nil); got != nil {
		t.Fatalf("nil planned should yield nil outcome; got %+v", got)
	}
}

func TestBuildLegacyOutcome_SingleProviderRouteUsesPlanned(t *testing.T) {
	t.Parallel()

	planned := &Metadata{
		Phase:    MetadataPhasePlanned,
		Path:     "legacy",
		Client:   "MyClient",
		Provider: "openai",
	}
	got := BuildLegacyOutcome(planned, 250, "", "", nil)

	if got == nil {
		t.Fatal("expected non-nil outcome")
	}
	if got.Phase != MetadataPhaseOutcome {
		t.Errorf("Phase: got %q, want outcome", got.Phase)
	}
	if got.WinnerPath != "legacy" {
		t.Errorf("WinnerPath: got %q, want legacy", got.WinnerPath)
	}
	if got.WinnerClient != "MyClient" {
		t.Errorf("WinnerClient: got %q, want MyClient (fallback to planned)", got.WinnerClient)
	}
	if got.WinnerProvider != "openai" {
		t.Errorf("WinnerProvider: got %q, want openai (fallback to planned)", got.WinnerProvider)
	}
	if got.UpstreamDurMs == nil || *got.UpstreamDurMs != 250 {
		t.Errorf("UpstreamDurMs: got %v, want &250", got.UpstreamDurMs)
	}
	if got.BamlCallCount != nil {
		t.Errorf("BamlCallCount: should be nil when not provided; got %v", *got.BamlCallCount)
	}
	if got.RetryCount != nil {
		t.Errorf("RetryCount: must be nil on legacy; got %v", *got.RetryCount)
	}
}

func TestBuildLegacyOutcome_ExplicitWinnerOverridesPlanned(t *testing.T) {
	t.Parallel()

	planned := &Metadata{
		Phase:    MetadataPhasePlanned,
		Path:     "legacy",
		Client:   "MyClient",
		Provider: "openai",
	}
	five := 5
	got := BuildLegacyOutcome(planned, 100, "FromSelectedCall", "anthropic", &five)

	if got.WinnerClient != "FromSelectedCall" {
		t.Errorf("WinnerClient: got %q, want FromSelectedCall", got.WinnerClient)
	}
	if got.WinnerProvider != "anthropic" {
		t.Errorf("WinnerProvider: got %q, want anthropic", got.WinnerProvider)
	}
	if got.BamlCallCount == nil || *got.BamlCallCount != 5 {
		t.Errorf("BamlCallCount: got %v, want &5", got.BamlCallCount)
	}
}

func TestBuildLegacyOutcome_StrategyWithoutWinnerLeavesAbsent(t *testing.T) {
	t.Parallel()

	planned := &Metadata{
		Phase:    MetadataPhasePlanned,
		Path:     "legacy",
		Client:   "Strategy",
		Strategy: "baml-fallback",
		Chain:    []string{"A", "B"},
	}
	got := BuildLegacyOutcome(planned, 100, "", "", nil)

	if got.WinnerClient != "" {
		t.Errorf("WinnerClient must be absent on strategy route without SelectedCall data; got %q", got.WinnerClient)
	}
	if got.WinnerProvider != "" {
		t.Errorf("WinnerProvider must be absent on strategy route without SelectedCall data; got %q", got.WinnerProvider)
	}
	if got.WinnerPath != "legacy" {
		t.Errorf("WinnerPath: got %q, want legacy", got.WinnerPath)
	}
}

func TestBuildLegacyOutcome_StrategyWithWinnerUsesSelectedCall(t *testing.T) {
	t.Parallel()

	planned := &Metadata{
		Phase:    MetadataPhasePlanned,
		Path:     "legacy",
		Client:   "Strategy",
		Strategy: "baml-fallback",
		Chain:    []string{"Primary", "Backup"},
	}
	got := BuildLegacyOutcome(planned, 100, "Backup", "anthropic", nil)

	if got.WinnerClient != "Backup" {
		t.Errorf("WinnerClient: got %q, want Backup", got.WinnerClient)
	}
	if got.WinnerProvider != "anthropic" {
		t.Errorf("WinnerProvider: got %q, want anthropic", got.WinnerProvider)
	}
}

func TestBuildLegacyOutcome_ClearsPlannedOnlyFields(t *testing.T) {
	t.Parallel()

	five := 5
	planned := &Metadata{
		Phase:          MetadataPhasePlanned,
		Path:           "legacy",
		Client:         "Strategy",
		Strategy:       "baml-fallback",
		Provider:       "openai",
		Chain:          []string{"A", "B"},
		LegacyChildren: []string{"B"},
		RetryMax:       &five,
		RetryPolicy:    "exp:200ms:1.5:10s",
	}
	got := BuildLegacyOutcome(planned, 100, "A", "openai", nil)

	if got.RetryMax != nil {
		t.Errorf("RetryMax must be cleared from outcome; got %v", *got.RetryMax)
	}
	if got.RetryPolicy != "" {
		t.Errorf("RetryPolicy must be cleared from outcome; got %q", got.RetryPolicy)
	}
	if got.Chain != nil {
		t.Errorf("Chain must be cleared from outcome; got %v", got.Chain)
	}
	if got.LegacyChildren != nil {
		t.Errorf("LegacyChildren must be cleared from outcome; got %v", got.LegacyChildren)
	}
	if got.Strategy != "" {
		t.Errorf("Strategy must be cleared from outcome; got %q", got.Strategy)
	}
	if got.Provider != "" {
		t.Errorf("Provider must be cleared from outcome; got %q", got.Provider)
	}
}

func TestBuildLegacyOutcome_PreservesPath(t *testing.T) {
	t.Parallel()

	planned := &Metadata{
		Phase:      MetadataPhasePlanned,
		Path:       "legacy",
		PathReason: "unsupported-provider",
		Client:     "MyClient",
		Provider:   "aws-bedrock",
	}
	got := BuildLegacyOutcome(planned, 100, "", "", nil)

	if got.Path != "legacy" {
		t.Errorf("Path must be preserved; got %q", got.Path)
	}
	if got.PathReason != "unsupported-provider" {
		t.Errorf("PathReason must be preserved; got %q", got.PathReason)
	}
}

func TestBuildLegacyOutcome_ZeroDurationStillSetsField(t *testing.T) {
	t.Parallel()

	planned := &Metadata{Phase: MetadataPhasePlanned, Path: "legacy", Client: "X", Provider: "openai"}
	got := BuildLegacyOutcome(planned, 0, "", "", nil)

	if got.UpstreamDurMs == nil {
		t.Fatal("UpstreamDurMs must be set even on zero duration")
	}
	if *got.UpstreamDurMs != 0 {
		t.Errorf("UpstreamDurMs: got %d, want 0", *got.UpstreamDurMs)
	}
}

func TestBuildLegacyOutcome_BamlCallCountZeroPropagates(t *testing.T) {
	t.Parallel()

	planned := &Metadata{Phase: MetadataPhasePlanned, Path: "legacy", Client: "X", Provider: "openai"}
	zero := 0
	got := BuildLegacyOutcome(planned, 100, "", "", &zero)

	if got.BamlCallCount == nil {
		t.Fatal("BamlCallCount=&0 must propagate (distinct from nil)")
	}
	if *got.BamlCallCount != 0 {
		t.Errorf("BamlCallCount: got %d, want 0", *got.BamlCallCount)
	}
}
