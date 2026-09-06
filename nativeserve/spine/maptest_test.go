package spine

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/streamoracle"
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
	e, err := NewUnaryExecutor(proj, []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	rm := e.registry["StaticRecursiveAliasJSON"]
	if rm == nil {
		t.Fatalf("method not registered")
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

// TestRecordPrefixCompareAccountsForEveryResolvedClassification is the anti-silent-drop
// guard on the bounded per-prefix ledger. recordPrefixCompare is a switch with no default,
// so a classification the resolver can return but the switch does not name would be counted
// NOWHERE — a tally that silently under-reports drift, which is the one direction that
// matters here.
//
// It enumerates the closed classification set and requires each to land in exactly one
// counter, so adding a token without a counter fails here rather than in a dashboard.
func TestRecordPrefixCompareAccountsForEveryResolvedClassification(t *testing.T) {
	resolved := []bamlutils.NativeStreamOracleCompare{
		bamlutils.NativeStreamCompareMatch,
		bamlutils.NativeStreamCompareNativeNoValue,
		bamlutils.NativeStreamCompareBAMLNoValue,
		bamlutils.NativeStreamCompareBytesMismatch,
		bamlutils.NativeStreamCompareNativeError,
	}
	for _, c := range resolved {
		var obs bamlutils.NativeSpineStreamOracleObservations
		recordPrefixCompare(&obs, streamoracle.PrefixOutcome{Compare: c})
		total := obs.PrefixMatch + obs.PrefixNativeNoValue + obs.PrefixBAMLNoValue +
			obs.PrefixBytesMismatch + obs.PrefixNativeError
		if total != 1 {
			t.Errorf("classification %q landed in %d counter(s), want exactly 1 — an unnamed classification is counted nowhere", c, total)
		}
	}
	// The zero value is the "no comparison performed" token and must move nothing.
	var obs bamlutils.NativeSpineStreamOracleObservations
	recordPrefixCompare(&obs, streamoracle.PrefixOutcome{Compare: bamlutils.NativeStreamCompareNone})
	if obs.PrefixMatch+obs.PrefixNativeNoValue+obs.PrefixBAMLNoValue+obs.PrefixBytesMismatch+obs.PrefixNativeError != 0 {
		t.Error("the zero classification moved a counter; it means no comparison was performed")
	}
	// The two drift SHAPES are recorded independently of the classification, because
	// "BAML answered instead" and "nothing was released" are different operational facts.
	obs = bamlutils.NativeSpineStreamOracleObservations{}
	recordPrefixCompare(&obs, streamoracle.PrefixOutcome{Compare: bamlutils.NativeStreamCompareBytesMismatch, Substituted: true})
	recordPrefixCompare(&obs, streamoracle.PrefixOutcome{Compare: bamlutils.NativeStreamCompareBAMLNoValue, Suppressed: true})
	if !obs.Substituted || !obs.Suppressed {
		t.Errorf("observations = %+v, want both drift shapes latched", obs)
	}
}
