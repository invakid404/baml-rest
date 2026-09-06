//go:build nanollm_integration

package admission

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// static_spine_stream_oracle_test.go drives AdmitStaticSpineStreamOracleClaim with the SAME
// fully valid exact-JSON input the frozen entry's prepare test uses, so the ONLY difference
// between the two entries — the LIVE BAML StreamRequest plan compare — is what these rows
// exercise. The completeness is what makes them discriminating: the same input ADMITS on a
// byte-matching plan, so deleting the compare turns every negative row into a claim.
//
// Gated by nanollm_integration because it reaches nanollm Prepare; it opens NO socket
// (Prepare is no-send and the compare never sends).

// nativePlanFor captures the native prepared plan for in through the FROZEN entry (which
// runs no compare), so a test can hand the identical bytes back as BAML's no-send plan.
func nativePlanFor(t *testing.T, in StaticStreamInput) *llmhttp.Request {
	t.Helper()
	frozen, err := AdmitStaticSpineStreamClaim(context.Background(), in)
	if err != nil {
		t.Fatalf("capture the native plan via the frozen entry: %v", err)
	}
	defer frozen.Close()
	hdr := map[string]string{}
	for _, p := range frozen.Prepared.Headers {
		hdr[p[0]] = p[1]
	}
	return &llmhttp.Request{
		Method:  frozen.Prepared.Method,
		URL:     frozen.Prepared.URL,
		Headers: hdr,
		Body:    string(frozen.Prepared.Body),
	}
}

// oraclePreparedInput is spinePreparedInput plus the untouched-target predicate every row
// here needs (the rewrite/proxy gate is mandatory and fail-closed on both spine entries).
func oraclePreparedInput() StaticStreamInput {
	in := spinePreparedInput()
	in.WouldRewriteOrProxy = func(string) bool { return false }
	return in
}

// TestAdmitStaticSpineStreamOracleClaimAdmitsOnAByteMatchingPlan is the POSITIVE CONTROL.
// Without it, every negative row below would stay green with the compare deleted, because
// "it declined" would still hold for a different reason.
func TestAdmitStaticSpineStreamOracleClaimAdmitsOnAByteMatchingPlan(t *testing.T) {
	in := oraclePreparedInput()
	plan := nativePlanFor(t, in)
	builds := 0
	in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
		builds++
		return plan, nil
	}
	claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
	if err != nil {
		t.Fatalf("a byte-matching live plan must ADMIT: %v", err)
	}
	defer claim.Close()
	if builds != 1 {
		t.Errorf("the live plan builder ran %d time(s), want exactly 1 — the compare is the oracle entry's whole difference", builds)
	}
	if claim.Surface != SurfaceStaticStream {
		t.Errorf("claim surface = %v, want static_stream", claim.Surface)
	}
	// The lane is enrollment-EXEMPT: it resolves the structural CohortNone bucket, never a
	// rollout row.
	if claim.Cohort != CohortNone {
		t.Errorf("claim cohort = %v, want none — the spine stream lane is a code-owned totality", claim.Cohort)
	}
}

// TestAdmitStaticSpineStreamOracleClaimDeclinesOnPlanDrift: any byte difference in BAML's
// plan declines PRE-CLAIM, so the request goes to BAML rather than claiming an irreversible
// stream on an unverified plan.
func TestAdmitStaticSpineStreamOracleClaimDeclinesOnPlanDrift(t *testing.T) {
	in := oraclePreparedInput()
	plan := nativePlanFor(t, in)
	drifted := *plan
	drifted.Body = plan.Body + " "
	in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) { return &drifted, nil }

	claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
	if claim != nil {
		claim.Close()
		t.Fatal("a drifting BAML plan produced a claim; the live compare must decline pre-socket")
	}
	d, ok := err.(*StaticDecline)
	if !ok {
		t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
	}
	if d.Stage != string(StagePlanCompare) {
		t.Errorf("declined at stage %q reason %q, want the plan_compare stage", d.Stage, d.Reason)
	}
}

// TestAdmitStaticSpineStreamOracleClaimDeclinesWithoutAPlanBuilder: a nil builder means
// there is no oracle to compare against, which on a stream must be a decline rather than a
// claim taken without the rail.
func TestAdmitStaticSpineStreamOracleClaimDeclinesWithoutAPlanBuilder(t *testing.T) {
	in := oraclePreparedInput()
	in.BuildBAMLRequest = nil
	claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
	if claim != nil {
		claim.Close()
		t.Fatal("a nil plan builder produced a claim")
	}
	if _, ok := err.(*StaticDecline); !ok {
		t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
	}
}

// TestAdmitStaticSpineStreamOracleClaimKeepsTheFrozenLanesGates proves the oracle entry did
// not become a relaxed copy: it applies the SAME exact-cohort totality cut and the SAME
// MANDATORY fail-closed rewrite/proxy gate the frozen entry does, and it does NOT apply the
// legacy lane's default-deny cohort gate.
func TestAdmitStaticSpineStreamOracleClaimKeepsTheFrozenLanesGates(t *testing.T) {
	t.Run("nil rewrite/proxy predicate fails closed", func(t *testing.T) {
		in := spinePreparedInput() // deliberately WITHOUT the predicate
		plan := nativePlanFor(t, oraclePreparedInput())
		builds := 0
		in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
			builds++
			return plan, nil
		}
		claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
		if claim != nil {
			claim.Close()
			t.Fatal("an unverifiable send target produced a claim; the gate is fail-closed")
		}
		d, ok := err.(*StaticDecline)
		if !ok {
			t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
		}
		if d.Stage != string(StageStrategy) {
			t.Errorf("declined at stage %q reason %q, want the strategy stage", d.Stage, d.Reason)
		}
		if builds != 0 {
			t.Errorf("the live plan builder ran %d time(s); the rewrite/proxy gate sits ABOVE the compare", builds)
		}
	})

	t.Run("a non-exact return bundle declines at the totality cut", func(t *testing.T) {
		in := oraclePreparedInput()
		// Replace the exact five-arm alias Return with the NULLABLE six-arm `JsonValue`
		// family: it is served on the FINAL lane but is deliberately OUTSIDE the stream
		// totality predicate, so it must decline before any nanollm work.
		in.Descriptor.Return = aliasDescriptorBundle(in.Method, "JsonValue", true, descJsonValueArms())
		builds := 0
		in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
			builds++
			return nil, nil
		}
		claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
		if claim != nil {
			claim.Close()
			t.Fatal("a non-exact return bundle produced a claim")
		}
		if _, ok := err.(*StaticDecline); !ok {
			t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
		}
		if builds != 0 {
			t.Errorf("the live plan builder ran %d time(s); the totality cut sits ABOVE the compare and before any nanollm work", builds)
		}
	})

	t.Run("no default-deny cohort gate", func(t *testing.T) {
		in := oraclePreparedInput()
		in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) {
			return nativePlanFor(t, oraclePreparedInput()), nil
		}
		// The legacy lane would decline this SAME input at (cohort, cohort_not_enrolled),
		// because nothing is enrolled for static_stream.
		if _, lerr := AdmitStaticStreamClaim(context.Background(), in); lerr == nil {
			t.Fatal("the legacy lane admitted an unenrolled static stream")
		} else if d, ok := lerr.(*StaticDecline); !ok || d.Stage != string(StageCohort) {
			t.Fatalf("the legacy lane declined at %v, want the cohort gate — the contrast is the point", lerr)
		}
		claim, err := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
		if err != nil {
			t.Fatalf("the oracle lane must NOT consult the dynamic-rollout cohort manifest: %v", err)
		}
		claim.Close()
	})
}

// TestSpineStreamEntriesShareOneAdmissionBody pins that the two spine stream entries run the
// SAME pre-claim portion: for every input that declines on the frozen entry, the oracle
// entry declines with the SAME bounded stage/reason. A future edit that forked the shared
// helper — adding a gate to one lane, or dropping one from the other — shows up here rather
// than as a production asymmetry nobody is watching.
func TestSpineStreamEntriesShareOneAdmissionBody(t *testing.T) {
	plan := nativePlanFor(t, oraclePreparedInput())
	withPlan := func(in StaticStreamInput) StaticStreamInput {
		in.BuildBAMLRequest = func(context.Context) (*llmhttp.Request, error) { return plan, nil }
		return in
	}
	cases := map[string]func(StaticStreamInput) StaticStreamInput{
		"not a stream mode":    func(in StaticStreamInput) StaticStreamInput { in.Mode = ""; return in },
		"not single leaf":      func(in StaticStreamInput) StaticStreamInput { in.SingleLeaf = false; return in },
		"fallback chain":       func(in StaticStreamInput) StaticStreamInput { in.HasFallbackChain = true; return in },
		"round robin":          func(in StaticStreamInput) StaticStreamInput { in.HasRoundRobin = true; return in },
		"retry override":       func(in StaticStreamInput) StaticStreamInput { in.HasRequestRetryOverride = true; return in },
		"provider not openai":  func(in StaticStreamInput) StaticStreamInput { in.Provider = "anthropic"; return in },
		"flag disabled":        func(in StaticStreamInput) StaticStreamInput { in.FlagEnabled = false; return in },
		"non-static route":     func(in StaticStreamInput) StaticStreamInput { in.RouteKind = RouteKindDynamic; return in },
		"unverifiable rewrite": func(in StaticStreamInput) StaticStreamInput { in.WouldRewriteOrProxy = nil; return in },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := mutate(withPlan(oraclePreparedInput()))
			frozenClaim, frozenErr := AdmitStaticSpineStreamClaim(context.Background(), in)
			if frozenClaim != nil {
				frozenClaim.Close()
				t.Fatalf("the frozen entry admitted %q; the case is not a decline", name)
			}
			oracleClaim, oracleErr := AdmitStaticSpineStreamOracleClaim(context.Background(), in)
			if oracleClaim != nil {
				oracleClaim.Close()
				t.Fatalf("the oracle entry admitted %q while the frozen entry declined it", name)
			}
			fd, fok := frozenErr.(*StaticDecline)
			od, ook := oracleErr.(*StaticDecline)
			if !fok || !ook {
				t.Fatalf("declines are not typed: frozen=%v oracle=%v", frozenErr, oracleErr)
			}
			if fd.Stage != od.Stage || fd.Reason != od.Reason {
				t.Errorf("the two spine stream entries diverged on %q: frozen=%s/%s oracle=%s/%s",
					name, fd.Stage, fd.Reason, od.Stage, od.Reason)
			}
		})
	}
}

// TestStaticStreamLaneSkipsCohortGate pins the lane policy itself, so a future lane added
// without a decision about the cohort gate is a compile-visible choice rather than a
// default. It is the unexported half of the exported-entry proof in cohort_test.go.
func TestStaticStreamLaneSkipsCohortGate(t *testing.T) {
	for lane, want := range map[staticStreamLane]bool{
		laneLegacyStaticStream:      false,
		laneSpineStaticStream:       true,
		laneSpineStaticStreamOracle: true,
	} {
		if got := lane.skipsCohortGate(); got != want {
			t.Errorf("lane %d skipsCohortGate() = %v, want %v", lane, got, want)
		}
	}
	// A FOURTH lane cannot be detected from here (Go has no enum reflection), and pretending
	// otherwise with a numeric equality would be a guard that never fires. The real one is
	// TestEveryAdmissionEntryPointIsCohortGated: a new lane needs its own exported Admit*
	// entry, and that scan refuses an entry no cohort-gate proof drives.
}
