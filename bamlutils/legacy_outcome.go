package bamlutils

// BuildLegacyOutcome composes a Metadata payload describing the outcome of
// a request that ran on the legacy CallStream+OnTick path. Called by the
// per-method orchestrators (runFullOrchestration / runNoRawOrchestration)
// once the winning attempt has been identified.
//
// The helper is intentionally pure: callers extract winner/call-count
// values from BAML's FunctionLog and pass them in, so this function does
// not depend on the BAML SDK and is trivial to unit-test.
//
// Inputs:
//   - planned: the planned-phase metadata used to seed the outcome. Returns
//     nil when planned is nil (callers no-op when no plan is configured).
//   - upstreamDurMs: wall-clock milliseconds since orchestration started.
//     The caller computes this (typically time.Since(startTime).Milliseconds())
//     so the helper stays pure.
//   - winnerClient, winnerProvider: extracted from
//     funcLog.SelectedCall().{ClientName,Provider}(). Pass empty strings
//     when SelectedCall returned an error, was nil, or no FunctionLog was
//     ever observed.
//   - bamlCallCount: max(len(funcLog.Calls())-1, 0). Pass nil when
//     Calls() failed or no FunctionLog was ever observed.
//
// Behavior:
//   - Phase is set to MetadataPhaseOutcome.
//   - WinnerPath is always "legacy".
//   - WinnerClient / WinnerProvider follow a fallback ladder:
//     1. If winnerClient != "", use the provided values.
//     2. Else if planned.Strategy == "" (single-provider route), fall back
//        to planned.Client / planned.Provider — they are tautologically the
//        actual winner on a single-provider route.
//     3. Else (strategy route, no SelectedCall data), leave both absent.
//        Reporting the strategy name as the winner would conflate the
//        planned strategy with the actual chosen child.
//   - BamlCallCount is echoed as-is (nil-passthrough).
//   - UpstreamDurMs is set from the caller-provided duration.
//   - Planned-only fields (RetryMax, RetryPolicy, Chain, LegacyChildren,
//     Strategy, Provider) are cleared from the outcome to keep the event
//     compact. Consumers see the planned-phase event separately for those
//     fields.
//   - RetryCount is left nil. The legacy path has no outer retry
//     orchestrator; BAML's internal retries are surfaced via BamlCallCount.
func BuildLegacyOutcome(
	planned *Metadata,
	upstreamDurMs int64,
	winnerClient, winnerProvider string,
	bamlCallCount *int,
) *Metadata {
	if planned == nil {
		return nil
	}

	out := *planned
	out.Phase = MetadataPhaseOutcome
	out.Attempt = 0
	out.RetryMax = nil
	out.RetryPolicy = ""
	out.Chain = nil
	out.LegacyChildren = nil
	out.Strategy = ""
	out.Provider = ""
	out.RetryCount = nil

	out.WinnerPath = "legacy"
	switch {
	case winnerClient != "":
		out.WinnerClient = winnerClient
		out.WinnerProvider = winnerProvider
	case planned.Strategy == "":
		out.WinnerClient = planned.Client
		out.WinnerProvider = planned.Provider
	default:
		out.WinnerClient = ""
		out.WinnerProvider = ""
	}

	out.BamlCallCount = bamlCallCount
	dur := upstreamDurMs
	out.UpstreamDurMs = &dur

	return &out
}
