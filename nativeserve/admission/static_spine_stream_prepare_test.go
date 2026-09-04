//go:build nanollm_integration

package admission

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// static_spine_stream_prepare_test.go drives AdmitStaticSpineStreamClaim with a FULLY
// VALID exact-JSON input — one that renders, normalizes, and reaches nanollm Prepare —
// so the MANDATORY rewrite/proxy gate is the only thing left between it and a claim.
//
// That completeness is the whole point. A partial fixture that declines earlier (during
// render or prepare) would leave these rows green even if the gate were deleted, because
// "it declined" would still hold. Here the same input ADMITS when the predicate says the
// target is untouched, so a deleted gate turns both negative rows into claims and the
// test goes red. Gated by nanollm_integration because Prepare needs the native engine; it
// opens NO socket (Prepare is no-send).

// spinePreparedInput is a complete, admissible static-stream input for the exact
// five-arm `JSON` alias: no arguments, a literal prompt with no template features, a
// literal-model OpenAI leaf client with literal base_url/api_key, and the stream mode.
// Only WouldRewriteOrProxy is left to the caller.
func spinePreparedInput() StaticStreamInput {
	const method = "StreamPrepared"
	lit := func(s string) promptdescriptor.OptionValue {
		return promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: s}
	}
	return StaticStreamInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           RouteKindStatic,
		Method:              method,
		Mode:                bamlutils.NativeStreamModeStream,
		SingleLeaf:          true,
		Provider:            "openai",
		Descriptor: promptdescriptor.Function{
			Version:  promptdescriptor.Version,
			Method:   method,
			Prompt:   "Return a JSON document.",
			Provider: "openai",
			Client:   "JSONOracle",
			Return:   aliasDescriptorBundle(method, "JSON", false, descJSONArms()),
			ClientConfig: promptdescriptor.ClientConfig{
				Present:  true,
				Name:     "JSONOracle",
				Provider: "openai",
				Model: promptdescriptor.ClientModel{
					Value:      "gpt-4o-mini",
					Provenance: promptdescriptor.ModelProvenanceLiteral,
				},
				TransportOptions: []promptdescriptor.ClientOption{
					{Key: "base_url", Value: lit("http://127.0.0.1:9/v1")},
					{Key: "api_key", Value: lit("sk-m3e-a-not-a-real-secret")},
				},
			},
		},
	}
}

// TestAdmitStaticSpineStreamClaimReachesThePreparedPlan is the POSITIVE CONTROL that
// makes the two negative rows below discriminating: with a predicate reporting the
// effective target is untouched, this exact input ADMITS, and the predicate was invoked
// exactly once with the PREPARED plan's URL (not the descriptor's raw base_url, and not
// some other string). Without this row a deleted rewrite/proxy gate would be invisible.
func TestAdmitStaticSpineStreamClaimReachesThePreparedPlan(t *testing.T) {
	var seen []string
	in := spinePreparedInput()
	in.WouldRewriteOrProxy = func(u string) bool {
		seen = append(seen, u)
		return false
	}

	claim, err := AdmitStaticSpineStreamClaim(context.Background(), in)
	if err != nil {
		t.Fatalf("a fully valid exact-JSON stream input declined (%v); the rewrite/proxy rows below would prove nothing", err)
	}
	defer claim.Close()

	if len(seen) != 1 {
		t.Fatalf("rewrite/proxy predicate invoked %d time(s), want exactly 1: %v", len(seen), seen)
	}
	if claim.Prepared == nil {
		t.Fatal("the claim carries no prepared plan")
	}
	if seen[0] != claim.Prepared.URL {
		t.Fatalf("predicate saw %q, want the PREPARED effective URL %q", seen[0], claim.Prepared.URL)
	}
	if claim.Client() == nil {
		t.Fatal("the claim did not keep the request-scoped engine alive")
	}
	if claim.Surface != SurfaceStaticStream {
		t.Fatalf("claim surface = %q, want %q", claim.Surface, SurfaceStaticStream)
	}
	// The spine lane is cohort-gate exempt, so the claim carries the neutral bucket.
	if claim.Cohort != CohortNone {
		t.Fatalf("claim cohort = %v, want CohortNone (the spine lane must not enroll)", claim.Cohort)
	}
}

// TestAdmitStaticSpineStreamClaimRewriteProxyGateIsMandatory pins BOTH halves of the
// fail-closed rule on the SAME input that admits above: a NIL predicate (the effective
// target's status could not be verified) and a POSITIVE predicate (it would be diverted)
// each decline PRE-CLAIM at the exact strategy stage and reason. Deleting the gate makes
// both rows return a claim.
func TestAdmitStaticSpineStreamClaimRewriteProxyGateIsMandatory(t *testing.T) {
	cases := []struct {
		name       string
		predicate  func(string) bool
		wantReason Reason
		why        string
	}{
		{
			name:       "nil_predicate_fails_closed",
			predicate:  nil,
			wantReason: reasonSpineRewriteProxyUnverified,
			why:        "this lane is cohort-gate exempt, so an unverifiable target must decline exactly as a diverted one would",
		},
		{
			name:       "diverted_target_declines",
			predicate:  func(string) bool { return true },
			wantReason: ReasonURLRewriteOrProxy,
			why:        "a rewritten/proxied effective target makes the exact-transport evidence meaningless",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := spinePreparedInput()
			in.WouldRewriteOrProxy = tc.predicate

			claim, err := AdmitStaticSpineStreamClaim(context.Background(), in)
			if claim != nil {
				claim.Close()
				t.Fatalf("the rewrite/proxy gate produced a CLAIM (%s); it must decline pre-socket", tc.why)
			}
			d, ok := err.(*StaticDecline)
			if !ok {
				t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
			}
			if Stage(d.Stage) != StageStrategy || Reason(d.Reason) != tc.wantReason {
				t.Fatalf("(stage, reason) = (%q, %q), want (%q, %q) — %s", d.Stage, d.Reason, StageStrategy, tc.wantReason, tc.why)
			}
		})
	}
}

// TestAdmitStaticSpineStreamClaimGateOrderIsTotalityThenRewrite proves the two gates are
// independent and ordered: an out-of-cohort return declines at the TOTALITY gate even
// with a diverting predicate installed, so neither row above can be satisfied by the
// other gate firing.
func TestAdmitStaticSpineStreamClaimGateOrderIsTotalityThenRewrite(t *testing.T) {
	in := spinePreparedInput()
	in.Descriptor.Return = schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  in.Method,
		Target:  descPrim(schemadescriptor.PrimitiveString),
	}
	in.WouldRewriteOrProxy = func(string) bool { return true }

	claim, err := AdmitStaticSpineStreamClaim(context.Background(), in)
	if claim != nil {
		claim.Close()
		t.Fatal("an out-of-cohort return produced a claim")
	}
	d, ok := err.(*StaticDecline)
	if !ok {
		t.Fatalf("err = %v (%T), want *StaticDecline", err, err)
	}
	if Reason(d.Reason) != reasonSpineNotExactAlias {
		t.Fatalf("(stage, reason) = (%q, %q), want the totality gate to fire FIRST (%q)", d.Stage, d.Reason, reasonSpineNotExactAlias)
	}
}
