package spine

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/execute"
)

// TestMapAttemptUnknownOutcomeFailsAfterClaim proves the defensive default of
// mapAttempt: an UNKNOWN execute outcome (a value outside the four the pipeline
// produces) maps to a terminal FailedAfterClaim, never a decline and never a
// fallback. This is the "unknown result disposition" fault-matrix row; it is a
// white-box test because the outcome cannot be produced by a real provider.
func TestMapAttemptUnknownOutcomeFailsAfterClaim(t *testing.T) {
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	var m = proj.Methods[0]
	fn, err := nativespine.ReconstructFunction(proj, m)
	if err != nil {
		t.Fatalf("ReconstructFunction: %v", err)
	}
	e, err := NewUnaryExecutor([]SpineMethod{{Function: fn, Binding: nativespinejsonfixture.Binding()}}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	rm := e.registry[m.Name]
	if rm == nil {
		t.Fatalf("method %q not registered", m.Name)
	}

	// An outcome value outside {Structured, ParseDeclined, ProviderError, InvalidBody}.
	res := e.mapAttempt(rm, &execute.AttemptResult{Outcome: execute.Outcome(99)}, nil)
	if res.Disposition != bamlutils.NativeSpineFailedAfterClaim {
		t.Fatalf("unknown-outcome disposition = %v, want failed_after_claim", res.Disposition)
	}
	if res.Reason != reasonUnknownOutcome {
		t.Fatalf("reason = %q, want %q", res.Reason, reasonUnknownOutcome)
	}
	if snap := e.Metrics().Snapshot(); snap.Failures != 1 {
		t.Fatalf("metrics = %+v, want one failure", snap)
	}
}
