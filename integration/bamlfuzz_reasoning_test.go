//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// reasoningOracleArtifactDir is where ReasoningFailureEnvelope artifacts
// land on failure or with BAMLFUZZ_KEEP_ARTIFACTS=1. Stable relative to
// the integration test working directory so CI can collect from a
// predictable path.
const reasoningOracleArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/reasoning/_artifacts"

// reasoningLeakMarker is a sentinel embedded in every fuzzed thinking
// string. The reasoning channel is free text BAML never parses, but if a
// regression muxed thinking into the parseable or raw channel this token
// would surface in `data`/`raw`, where it must never appear. The
// authoritative separation checks are `raw == MockLLMContent` and
// `data == Expected`; the marker is a defense-in-depth substring probe on
// every streaming frame.
const reasoningLeakMarker = "DO_NOT_LEAK_INTO_PARSEABLE"

// reasoningCasesPerMode controls how many random reasoning cases run per
// preserve mode. The default of 2 is smaller than the dynamic/raw
// oracles' 4 because each reasoning case drives six calls (dynclient +
// REST unary, each flag-on and flag-off, plus two streaming legs); the
// nightly fuzz workflow cranks it up via BAMLFUZZ_REASONING_CASES.
func reasoningCasesPerMode() int {
	return envIntDefault("BAMLFUZZ_REASONING_CASES", 2)
}

// TestBamlfuzzReasoningOracle drives the reasoning-channel oracle: for
// each fuzz case it lowers the schema through the dynamic emitter, feeds
// the Anthropic mock the walker's content plus a fuzzed thinking block,
// and exercises the with-raw legs in both flag states —
//
//  1. dynclient.Client.DynamicCallRaw (in-proc), and
//  2. the REST /call-with-raw/_dynamic endpoint —
//
// once with __baml_options__.include_reasoning on and once with it off,
// plus the streaming with-raw legs (dynclient DynamicStreamRaw and REST
// /stream-with-raw/_dynamic). It asserts R1–R4 (see runReasoningOracleCase).
//
// DESIGN — input-echo, not recomputation: reasoning is not modeled by the
// walker (there is no reasoning field on OracleCase/WalkResult). BAML
// passes provider reasoning through verbatim without parsing it, so the
// oracle's reasoning "expected" is the fed thinking string itself. This
// oracle therefore proves cross-path PRESERVATION (every leg returns the
// same thinking) and content/reasoning SEPARATION (thinking never leaks
// into parsed `data` or `raw`), NOT correct reasoning parsing.
//
// PROVIDER — the bamlfuzz dynamic/raw oracles run on the OpenAI mock,
// which does not emit reasoning; the Anthropic mock emits a thinking
// block today with zero mock change (Scenario.Thinking), so the reasoning
// oracle runs on it. The Anthropic mock's usage/message_delta shape is
// already version-robust across the 0.214→0.222 matrix (it always emits
// usage), exactly as the existing reasoning_test.go relies on, so no
// extra version gate beyond the dynamic-endpoint gates in dynclientCallGate
// is needed.
func TestBamlfuzzReasoningOracle(t *testing.T) {
	dynclientCallGate(t)

	corpus, err := loadDynamicCorpus(dynamicOracleCorpusDir)
	if err != nil {
		t.Fatalf("load corpus from %s: %v", dynamicOracleCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("dynamic corpus at %s is empty — the reasoning oracle reuses it", dynamicOracleCorpusDir)
	}

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}

	t.Run("corpus", func(t *testing.T) {
		for i, c := range corpus {
			caseIdx := i
			caseCopy := c
			t.Run(caseCopy.Name, func(t *testing.T) {
				runReasoningOracleCase(t, dyn, caseCopy, caseIdx, caseSourceCorpus)
			})
		}
	})

	t.Run("rapid", func(t *testing.T) {
		modes := []bool{true, false}
		for _, preserve := range modes {
			preserve := preserve
			label := "preserve_off"
			if preserve {
				label = "preserve_on"
			}
			t.Run(label, func(t *testing.T) {
				cases := reasoningCasesPerMode()
				for i := 0; i < cases; i++ {
					i := i
					seed := reasoningSeedFor(preserve, i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						caseCopy := buildReasoningRapidCase(t, seed, preserve, i)
						runReasoningOracleCase(t, dyn, caseCopy, i, caseSourceRapid)
					})
				}
			})
		}
	})
}

// FuzzBamlfuzzReasoning is the testing.F companion to
// TestBamlfuzzReasoningOracle, exposing the reasoning oracle to Go's
// native fuzz engine via the bamlfuzz.MakeFuzz bridge. Like
// FuzzBamlfuzzCallWithRaw it draws PreserveSchemaOrder from the bit stream
// and ships no f.Add seed corpus. The version gates match the dynamic
// oracle: dynamic endpoints require BAML >= 0.215.0, and without an
// external baml source the pre-0.219 streaming API does not propagate
// dynamic classes to the parser.
func FuzzBamlfuzzReasoning(f *testing.F) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		f.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		f.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		f.Fatalf("NewDynclient: %v", err)
	}

	bamlfuzz.MakeFuzz(f, func(t *testing.T, rt *rapid.T) {
		preserve := rapid.Bool().Draw(rt, "preserve_schema_order")
		cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Draw(rt, "coupled_case")
		c := bamlfuzz.OracleCase{
			Name:                "fuzz",
			Seed:                0,
			CaseIndex:           0,
			Mode:                bamlfuzz.OracleReasoning,
			PreserveSchemaOrder: preserve,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
		runReasoningOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// reasoningSeedFor produces a deterministic rapid seed for a (preserve, i)
// pair. The "reasoning" domain prefix keeps the reasoning oracle's seed
// stream disjoint from the dynamic and raw oracles' so the three explore
// different schema shapes at the same case index. BAMLFUZZ_SEED, when set,
// XORs into every per-case seed so a single env var perturbs the matrix.
func reasoningSeedFor(preserve bool, i int) uint64 {
	h := fnv.New64a()
	h.Write([]byte("reasoning:"))
	if preserve {
		h.Write([]byte("preserve_on"))
	} else {
		h.Write([]byte("preserve_off"))
	}
	fmt.Fprintf(h, ":%d", i)
	base := h.Sum64()
	if v := os.Getenv("BAMLFUZZ_SEED"); v != "" {
		base ^= fnv64aString(v)
	}
	return base
}

// buildReasoningRapidCase synthesizes one OracleCase by drawing a
// dynamic-safe schema + value deterministically from seed, using the same
// CoupledCaseGen the dynamic oracle uses so the reasoning oracle exercises
// the identical case stream.
func buildReasoningRapidCase(t *testing.T, seed uint64, preserve bool, idx int) bamlfuzz.OracleCase {
	t.Helper()
	cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Example(int(seed))
	return bamlfuzz.OracleCase{
		Name:                fmt.Sprintf("reasoningrapid_%t_%d", preserve, idx),
		Seed:                int64(seed),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleReasoning,
		PreserveSchemaOrder: preserve,
		Schema:              cc.Schema,
		Value:               cc.Value,
		MockLLMContent:      cc.Walk.MockLLMContent,
		Expected:            cc.Walk.Expected,
		Metadata:            cc.Walk.Metadata,
	}
}

// reasoningThinkingFor derives the deterministic, case-unique thinking
// string fed to the Anthropic mock for one case. It is JSON-ish (so a leak
// into the parseable channel would corrupt parsed `data` against the
// walker's Expected) and carries reasoningLeakMarker (so a leak into any
// streaming frame's `data`/`raw` is detectable by substring). It is the
// reasoning channel's "expected" value — every leg's reasoning must echo
// it under include_reasoning=true.
func reasoningThinkingFor(c bamlfuzz.OracleCase) string {
	h := fnv.New64a()
	h.Write([]byte("reasoning-thinking:"))
	h.Write([]byte(c.Name))
	h.Write(c.MockLLMContent)
	marker := strconv.FormatUint(h.Sum64(), 36)
	return fmt.Sprintf(`Let me reason about case %s. The answer should be: {"%s":"%s"}`,
		c.Name, reasoningLeakMarker, marker)
}

// reasoningLegOutcome is one with-raw leg's captured result for a single
// flag state. ok is true only when the leg completed without a transport
// or HTTP error AND surfaced a usable response — a leg that errors carries
// ok=false so an empty payload is not mistaken for clean output.
type reasoningLegOutcome struct {
	ok        bool
	reasoning string
	data      json.RawMessage
	raw       string
	// errStr is the recorded non-context error / HTTP>=400 message / nil-
	// or-empty-result diagnostic for a leg that did NOT succeed. Every
	// reasoning case is a successful-path case (valid content, dynamic-safe
	// schema), so a leg with ok=false must contribute errStr as an oracle
	// failure — it can never be silently skipped past the ok-gated checks.
	errStr string
}

// runReasoningOracleCase performs the reasoning-channel comparison for one
// OracleCase, capturing all relevant context into a ReasoningFailureEnvelope
// when any leg disagrees. `source` selects how ErrDynamicSchemaUnsupported
// is treated: corpus/fuzz cases skip, rapid cases fail.
func runReasoningOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, caseIdx int, source caseSource) {
	t.Helper()

	thinking := reasoningThinkingFor(c)
	envelope := &bamlfuzz.ReasoningFailureEnvelope{
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          bamlfuzz.OracleReasoning,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		Schema:              c.Schema,
		Value:               c.Value,
		MockLLMContent:      c.MockLLMContent,
		Expected:            c.Expected,
		ThinkingInput:       thinking,
		Metadata:            c.Metadata,
		Reproduction:        reproductionForReasoning(c, caseIdx, source),
	}

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		switch unsupportedActionFor(source) {
		case unsupportedSkip:
			t.Skipf("dynamic emitter skipped schema: %v", err)
			return
		case unsupportedFail:
			envelope.DynamicSkipReason = err.Error()
			failAndDumpReasoning(t, envelope, "rapid generator produced unsupported schema: %v", err)
			return
		}
	}
	if err != nil {
		envelope.DynamicSkipReason = err.Error()
		failAndDumpReasoning(t, envelope, "LowerToDynamicSchema failed: %v", err)
		return
	}
	envelope.DynamicSchema = &lowered

	// The parsed-data legs diff against the walker's non-empty Expected; an
	// empty MockLLMContent would make those checks vacuous, so treat it as
	// a harness error (the walker never renders empty content for a valid
	// case). The reasoning channel additionally needs a non-empty thinking
	// string so its echo assertions cannot pass by an empty==empty match.
	if len(c.MockLLMContent) == 0 {
		failAndDumpReasoning(t, envelope, "case has empty MockLLMContent; reasoning oracle cannot assert on a vacuous case")
		return
	}
	if thinking == "" {
		failAndDumpReasoning(t, envelope, "empty thinking string; reasoning echo assertions would be vacuous")
		return
	}
	expectedRaw := string(c.MockLLMContent)

	scenarioID := fmt.Sprintf("bamlfuzz-reason-%s", scenarioSafe(c.Name))
	envelope.MockLLMScenarioID = scenarioID
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer registerCancel()
	// Anthropic provider so Scenario.Thinking surfaces a thinking block.
	// ChunkSize drives the streaming legs; ChunkJitterMs stays 0 (the mock
	// default) so chunk boundaries are deterministic — jitter affects only
	// timing, never content, but the streaming reasoning assertions compare
	// cumulative snapshots and must not depend on wall-clock.
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "anthropic",
		Content:        expectedRaw,
		Thinking:       thinking,
		ChunkSize:      8,
		ChunkDelayMs:   0,
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(registerCtx, scenario); err != nil {
		failAndDumpReasoning(t, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateAnthropicTestClient(TestEnv.MockLLMInternal, scenarioID)

	preserve := c.PreserveSchemaOrder
	preservePtr := &preserve
	hello := "Return the dynamic fuzz value."
	// Base request shared by every leg. IncludeReasoning is toggled per
	// run; everything else is identical so the only variable across the
	// flag-on / flag-off runs is the reasoning opt-in itself.
	baseReq := dynclient.Request{
		Messages: []dynclient.Message{
			{Role: "system", PartsContent: []dynclient.ContentPart{
				{Type: "text", Text: &hello},
				{Type: "output_format"},
			}},
			{Role: "user", TextContent: &hello},
		},
		ClientRegistry:      testutil.DynRegistry(clientReg),
		OutputSchema:        &lowered,
		PreserveSchemaOrder: preservePtr,
	}

	// ---- unary dynclient leg, flag on/off ----
	// A context/transport error inside a leg is a harness failure (t.Fatalf
	// there); every other non-success (panic, non-context error, HTTP>=400,
	// nil/empty result) returns ok=false with errStr set, and is turned into
	// an explicit oracle failure below — a broken leg can never be skipped
	// into a pass by the later ok-gated checks.
	dynOn := runReasoningDynclientUnary(t, dyn, baseReq, true, c, envelope)
	dynOff := runReasoningDynclientUnary(t, dyn, baseReq, false, c, envelope)
	envelope.DynclientReasoningOn, envelope.DynclientDataOn, envelope.DynclientRawOn = dynOn.reasoning, dynOn.data, dynOn.raw
	envelope.DynclientReasoningOff, envelope.DynclientDataOff, envelope.DynclientRawOff = dynOff.reasoning, dynOff.data, dynOff.raw

	// ---- unary REST /call-with-raw/_dynamic leg, flag on/off ----
	restOn := runReasoningRESTUnary(t, baseReq, &lowered, true, c, envelope)
	restOff := runReasoningRESTUnary(t, baseReq, &lowered, false, c, envelope)
	envelope.RESTReasoningOn, envelope.RESTDataOn, envelope.RESTRawOn = restOn.reasoning, restOn.data, restOn.raw
	envelope.RESTReasoningOff, envelope.RESTDataOff, envelope.RESTRawOff = restOff.reasoning, restOff.data, restOff.raw

	var failures []string

	// Leg-error propagation (the vacuous-pass guard): any leg that did not
	// succeed on this successful-path case contributes its error string, so
	// no ok==false leg is silently skipped by a later ok check.
	failures = append(failures, legErrorFailure("dynclient opt-in unary", dynOn)...)
	failures = append(failures, legErrorFailure("dynclient default unary", dynOff)...)
	failures = append(failures, legErrorFailure("REST opt-in unary", restOn)...)
	failures = append(failures, legErrorFailure("REST default unary", restOff)...)

	// R1/R2/R4 + raw-separation + data-presence per leg, via the pure
	// reasoning-channel checker (unit-tested in isolation).
	failures = append(failures, recordReasoningFailures(envelope, "dynclient", thinking, expectedRaw, dynOn, dynOff)...)
	failures = append(failures, recordReasoningFailures(envelope, "rest", thinking, expectedRaw, restOn, restOff)...)

	// R1 cross-path: both legs' opt-in reasoning must agree (== thinking is
	// already asserted per leg; this pins them equal to each other so a
	// shared drift away from thinking that somehow matched would still be
	// caught against the input).
	if dynOn.ok && restOn.ok && dynOn.reasoning != restOn.reasoning {
		envelope.ReasoningMismatch = append(envelope.ReasoningMismatch,
			fmt.Sprintf("dynclient vs REST opt-in reasoning differ: %q vs %q", dynOn.reasoning, restOn.reasoning))
		failures = append(failures, "dynclient reasoning ≠ REST reasoning (opt-in)")
	}

	// Parsed `data` correctness: each ok leg/flag must still equal the
	// walker's Expected (proves include_reasoning never perturbs parsing,
	// and that the JSON-ish thinking probe did not leak into the parseable
	// channel). Reuse the dynamic oracle's SemanticDiff tolerances unchanged.
	failures = append(failures, reasoningDataDiff(envelope, "expected_vs_dynclient_on", c.Expected, dynOn)...)
	failures = append(failures, reasoningDataDiff(envelope, "expected_vs_dynclient_off", c.Expected, dynOff)...)
	failures = append(failures, reasoningDataDiff(envelope, "expected_vs_rest_on", c.Expected, restOn)...)
	failures = append(failures, reasoningDataDiff(envelope, "expected_vs_rest_off", c.Expected, restOff)...)

	// Cross-leg parity on the opt-in run (tolerates a leaked null key on
	// either BAML-generated side, like the dynamic oracle's dynclient_vs_rest).
	if dynOn.ok && restOn.ok && len(dynOn.data) > 0 && len(restOn.data) > 0 {
		if diff, derr := bamlfuzz.SemanticDiffParity("dynclient_vs_rest_reasoning_on", dynOn.data, restOn.data); derr != nil {
			failures = append(failures, fmt.Sprintf("dynclient_vs_rest_reasoning_on diff: %v", derr))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "dynclient ≠ REST data (opt-in)")
		}
	}

	// ---- R3: streaming legs ----
	failures = append(failures, runReasoningStreamingLegs(t, dyn, baseReq, &lowered, c, thinking, envelope)...)

	if len(failures) == 0 {
		if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
			if path, werr := bamlfuzz.WriteReasoningReplayArtifact(reasoningOracleArtifactDir, envelope); werr == nil {
				t.Logf("kept replay artifact (success): %s", path)
			}
		}
		return
	}
	failAndDumpReasoning(t, envelope, "%s", strings.Join(failures, "; "))
}

// legFailureFrom turns a non-ok (ok==false) leg into an explicit oracle
// failure. Every reasoning case is a successful-path case, so a leg that did
// not succeed (errored, returned HTTP>=400, or produced no result) must
// contribute its recorded error — it can never be silently skipped past the
// ok-gated channel checks. An ok leg contributes nothing. Shared by the
// unary and streaming legs (both carry an ok flag + errStr).
func legFailureFrom(label string, ok bool, errStr string) []string {
	if ok {
		return nil
	}
	msg := errStr
	if msg == "" {
		msg = "leg did not complete and produced no result"
	}
	return []string{fmt.Sprintf("%s leg failed on a successful-path case: %s", label, msg)}
}

// legErrorFailure is the unary-leg adapter for legFailureFrom.
func legErrorFailure(label string, leg reasoningLegOutcome) []string {
	return legFailureFrom(label, leg.ok, leg.errStr)
}

// runReasoningDynclientUnary drives one dynclient.DynamicCallRaw with the
// given include_reasoning flag. A context/transport error is a harness
// failure (t.Fatalf) because a call that never produced a verdict must not
// satisfy an equality check. Every other non-success (panic, non-context
// error, nil response) returns ok=false with errStr set and the envelope's
// error/panic fields populated; the caller turns ok=false into an explicit
// oracle failure via legErrorFailure, so no failure is reported inline and
// no broken leg is skipped into a pass.
func runReasoningDynclientUnary(t *testing.T, dyn *dynclient.Client, base dynclient.Request, includeReasoning bool, c bamlfuzz.OracleCase, envelope *bamlfuzz.ReasoningFailureEnvelope) reasoningLegOutcome {
	t.Helper()
	req := base
	req.IncludeReasoning = includeReasoning

	recordErr := func(msg string) reasoningLegOutcome {
		if includeReasoning {
			envelope.DynclientErrorOn = msg
		} else {
			envelope.DynclientErrorOff = msg
		}
		return reasoningLegOutcome{errStr: msg}
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		resp *dynclient.CallRawResult
		cerr error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		resp, cerr = dyn.DynamicCallRaw(callCtx, req)
	})
	if panicked {
		envelope.DynclientPanic = fmt.Sprintf("%v", panicVal)
		envelope.DynclientPanicStack = string(panicStack)
		return recordErr(fmt.Sprintf("dyn.DynamicCallRaw panicked: %v", panicVal))
	}
	switch {
	case cerr != nil:
		if isContextErr(cerr) {
			t.Fatalf("harness failure: dynclient DynamicCallRaw (include_reasoning=%v, case=%s): %v", includeReasoning, c.Name, cerr)
		}
		return recordErr(cerr.Error())
	case resp == nil:
		return recordErr("nil response from dyn.DynamicCallRaw")
	default:
		return reasoningLegOutcome{ok: true, reasoning: resp.Reasoning, data: resp.Data, raw: resp.Raw}
	}
}

// runReasoningRESTUnary drives one REST /call-with-raw/_dynamic call with
// the given include_reasoning flag. The body is built via buildDynamicCallBody
// so include_reasoning AND property order travel identically to the
// dynclient leg (buildDynamicCallBody copies req.IncludeReasoning). Same
// harness-failure / recorded-error contract as the dynclient leg: context
// errors t.Fatalf, every other non-success surfaces ok=false + errStr for
// legErrorFailure to escalate.
func runReasoningRESTUnary(t *testing.T, base dynclient.Request, lowered *bamlutils.DynamicOutputSchema, includeReasoning bool, c bamlfuzz.OracleCase, envelope *bamlfuzz.ReasoningFailureEnvelope) reasoningLegOutcome {
	t.Helper()
	req := base
	req.IncludeReasoning = includeReasoning

	recordErr := func(msg string) reasoningLegOutcome {
		if includeReasoning {
			envelope.RESTErrorOn = msg
		} else {
			envelope.RESTErrorOff = msg
		}
		return reasoningLegOutcome{errStr: msg}
	}

	body, berr := buildDynamicCallBody(req, lowered)
	if berr != nil {
		return recordErr(fmt.Sprintf("build REST body: %v", berr))
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		resp *testutil.DynamicCallWithRawResponse
		rerr error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		resp, rerr = BAMLClient.DynamicCallWithRawJSON(callCtx, body)
	})
	if panicked {
		envelope.RESTPanic = fmt.Sprintf("%v", panicVal)
		envelope.RESTPanicStack = string(panicStack)
		return recordErr(fmt.Sprintf("BAMLClient.DynamicCallWithRawJSON panicked: %v", panicVal))
	}
	switch {
	case rerr != nil:
		if isContextErr(rerr) {
			t.Fatalf("harness failure: REST DynamicCallWithRawJSON (include_reasoning=%v, case=%s): %v", includeReasoning, c.Name, rerr)
		}
		return recordErr(rerr.Error())
	case resp == nil:
		return recordErr("nil response from BAMLClient.DynamicCallWithRawJSON")
	}
	if includeReasoning {
		envelope.RESTStatusOn = resp.StatusCode
	} else {
		envelope.RESTStatusOff = resp.StatusCode
	}
	if resp.StatusCode >= 400 {
		return recordErr(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Error))
	}
	return reasoningLegOutcome{ok: true, reasoning: resp.Reasoning, data: resp.Data, raw: resp.Raw}
}

// recordReasoningFailures runs the pure reasoning-channel checker for one
// leg, appends any path-level reasoning mismatches to the envelope for
// forensics, and returns the failure summaries.
func recordReasoningFailures(envelope *bamlfuzz.ReasoningFailureEnvelope, leg, thinking, expectedRaw string, on, off reasoningLegOutcome) []string {
	failures := reasoningChannelFailures(leg, thinking, expectedRaw, on, off)
	envelope.ReasoningMismatch = append(envelope.ReasoningMismatch, failures...)
	return failures
}

// reasoningChannelFailures is the pure core of the reasoning oracle's
// per-leg assertions, extracted so its behaviour can be unit-tested
// without driving Docker (TestReasoningChannelFailures). It encodes:
//
//   - data presence (vacuous-pass guard): an ok leg MUST carry non-empty
//     parsed `data` — a successful call returning nil/empty data is a
//     failure, never a no-op.
//   - R1 (preservation): under include_reasoning=true the leg's reasoning
//     must equal the fed thinking string (and thinking is non-empty by
//     construction, so this also rejects an empty reasoning by skip).
//   - R4 (default excludes): with the flag off the reasoning must be empty.
//   - R2 (separation): reasoning differs across the flag (thinking vs ""),
//     while parsed `data` is byte-identical and `raw` is identical with the
//     flag on vs off — thinking never leaks into the parseable/raw channel.
//   - raw echo: `raw` equals the mock's emitted content (text-only) under
//     both flag states (raw is pre-parse wire text, compared with string ==).
//
// Each clause is gated on the relevant leg(s) being ok, so an errored leg
// (which already records its own failure) never trips these by skip.
func reasoningChannelFailures(leg, thinking, expectedRaw string, on, off reasoningLegOutcome) []string {
	var failures []string
	add := func(format string, args ...any) {
		failures = append(failures, leg+": "+fmt.Sprintf(format, args...))
	}

	// Data presence — vacuous-pass guard.
	if on.ok && len(on.data) == 0 {
		add("opt-in returned empty parsed data on a successful call")
	}
	if off.ok && len(off.data) == 0 {
		add("default returned empty parsed data on a successful call")
	}

	// R1 — opt-in reasoning echoes the fed thinking (non-empty by construction).
	if on.ok {
		if on.reasoning == "" {
			add("opt-in reasoning is empty (expected the fed thinking)")
		} else if on.reasoning != thinking {
			add("opt-in reasoning ≠ thinking: got %q want %q", on.reasoning, thinking)
		}
	}

	// R4 — default flag leaves reasoning empty.
	if off.ok && off.reasoning != "" {
		add("default reasoning non-empty: got %q want empty", off.reasoning)
	}

	// R2 — separation: reasoning differs across the flag; data + raw don't.
	if on.ok && off.ok {
		if on.reasoning == off.reasoning {
			add("reasoning identical across flag states (expected thinking vs empty): %q", on.reasoning)
		}
		if string(on.data) != string(off.data) {
			add("parsed data differs across flag states (separation violated):\n  opt-in: %s\n  default: %s", on.data, off.data)
		}
		if on.raw != off.raw {
			add("raw differs across flag states: opt-in=%q default=%q", on.raw, off.raw)
		}
	}

	// Raw echo — raw is text-only and equals the mock's emitted content
	// under any flag value (thinking never muxed into raw).
	if on.ok && on.raw != expectedRaw {
		add("opt-in raw ≠ MockLLMContent: got %q want %q", on.raw, expectedRaw)
	}
	if off.ok && off.raw != expectedRaw {
		add("default raw ≠ MockLLMContent: got %q want %q", off.raw, expectedRaw)
	}

	return failures
}

// reasoningDataDiff diffs one ok leg's parsed `data` against the walker's
// Expected, recording any semantic divergence on the envelope. An errored
// or empty-data leg is skipped here — its emptiness is already a failure
// recorded by reasoningChannelFailures, so it never reaches this diff to
// pass by skip.
func reasoningDataDiff(envelope *bamlfuzz.ReasoningFailureEnvelope, side string, expected json.RawMessage, leg reasoningLegOutcome) []string {
	if !leg.ok || len(leg.data) == 0 {
		return nil
	}
	diff, err := bamlfuzz.SemanticDiff(side, expected, leg.data)
	if err != nil {
		return []string{fmt.Sprintf("%s diff: %v", side, err)}
	}
	if len(diff) > 0 {
		envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
		return []string{side + ": expected ≠ actual (data)"}
	}
	return nil
}

// reasoningStreamOutcome is the accumulated result of one streaming
// with-raw leg: the cumulative reasoning at the last frame (NOT a per-frame
// delta — chunk boundaries interleave thinking/text blocks, so only the
// final cumulative snapshot is comparable), the final-frame data, and any
// per-frame violation (a leak of the thinking marker into data/raw, or
// reasoning arriving on a frame other than data/final). ok=false with
// errStr set marks a leg that did not complete (escalated by the caller).
type reasoningStreamOutcome struct {
	ok                  bool
	errStr              string
	cumulativeReasoning string
	finalData           json.RawMessage
	frameFailures       []string
}

// runReasoningStreamingLegs drives R3: both streaming transports (dynclient
// DynamicStreamRaw and REST /stream-with-raw/_dynamic) in BOTH flag states.
// Every successful stream's final data is validated against Expected and
// its frames are checked for leaks/provenance, regardless of flag. On the
// opt-in runs the cumulative reasoning must equal the fed thinking; on the
// default runs it must be empty (R4 holds on every path). A leg that did
// not complete contributes its error via legErrorFailure — no stream is
// skipped into a pass, and the four legs run independently (no early
// return) so one broken leg never hides another.
func runReasoningStreamingLegs(t *testing.T, dyn *dynclient.Client, base dynclient.Request, lowered *bamlutils.DynamicOutputSchema, c bamlfuzz.OracleCase, thinking string, envelope *bamlfuzz.ReasoningFailureEnvelope) []string {
	t.Helper()
	var failures []string

	// --- dynclient streaming, opt-in / default ---
	dynOnS := drainReasoningDynclientStream(t, dyn, base, true, c, envelope)
	envelope.StreamDynclientReasoning, envelope.StreamDynclientFinal = dynOnS.cumulativeReasoning, dynOnS.finalData
	failures = append(failures, legFailureFrom("dynclient opt-in stream", dynOnS.ok, dynOnS.errStr)...)
	if dynOnS.ok {
		failures = append(failures, assertStreamOutcome("dynclient stream opt-in", dynOnS, thinking, c.Expected, envelope)...)
	}

	dynOffS := drainReasoningDynclientStream(t, dyn, base, false, c, envelope)
	envelope.StreamDynclientReasoningOff, envelope.StreamDynclientFinalOff = dynOffS.cumulativeReasoning, dynOffS.finalData
	failures = append(failures, legFailureFrom("dynclient default stream", dynOffS.ok, dynOffS.errStr)...)
	if dynOffS.ok {
		failures = append(failures, assertStreamOutcome("dynclient stream default", dynOffS, "", c.Expected, envelope)...)
	}

	// --- REST streaming, opt-in / default ---
	restOnS := drainReasoningRESTStream(t, lowered, base, true, c, envelope)
	envelope.StreamRESTReasoning, envelope.StreamRESTFinal = restOnS.cumulativeReasoning, restOnS.finalData
	failures = append(failures, legFailureFrom("REST opt-in stream", restOnS.ok, restOnS.errStr)...)
	if restOnS.ok {
		failures = append(failures, assertStreamOutcome("REST stream opt-in", restOnS, thinking, c.Expected, envelope)...)
	}

	restOffS := drainReasoningRESTStream(t, lowered, base, false, c, envelope)
	envelope.StreamRESTReasoningOff, envelope.StreamRESTFinalOff = restOffS.cumulativeReasoning, restOffS.finalData
	failures = append(failures, legFailureFrom("REST default stream", restOffS.ok, restOffS.errStr)...)
	if restOffS.ok {
		failures = append(failures, assertStreamOutcome("REST stream default", restOffS, "", c.Expected, envelope)...)
	}

	// Cross-path: both opt-in streaming legs' cumulative reasoning must agree
	// (only meaningful when both completed).
	if dynOnS.ok && restOnS.ok && dynOnS.cumulativeReasoning != restOnS.cumulativeReasoning {
		failures = append(failures, fmt.Sprintf("dynclient stream reasoning ≠ REST stream reasoning: %q vs %q",
			dynOnS.cumulativeReasoning, restOnS.cumulativeReasoning))
	}

	return failures
}

// assertStreamOutcome encodes the per-leg R3 assertions for one completed
// streaming run. wantReasoning is the fed thinking on opt-in runs (the
// cumulative reasoning must equal it, non-empty) or "" on default runs (the
// cumulative reasoning must be empty — R4). In BOTH flag states the final
// data must be present and equal the walker's Expected, and no frame may
// leak the thinking marker or carry reasoning on a non-data/final frame.
func assertStreamOutcome(leg string, out reasoningStreamOutcome, wantReasoning string, expected json.RawMessage, envelope *bamlfuzz.ReasoningFailureEnvelope) []string {
	var failures []string
	if wantReasoning == "" {
		if out.cumulativeReasoning != "" {
			failures = append(failures, fmt.Sprintf("%s: cumulative reasoning non-empty under default flag: got %q want empty", leg, out.cumulativeReasoning))
		}
	} else if out.cumulativeReasoning == "" {
		failures = append(failures, leg+": cumulative reasoning is empty (expected the fed thinking)")
	} else if out.cumulativeReasoning != wantReasoning {
		failures = append(failures, fmt.Sprintf("%s: cumulative reasoning ≠ thinking: got %q want %q", leg, out.cumulativeReasoning, wantReasoning))
	}

	if len(out.finalData) == 0 {
		failures = append(failures, leg+": empty final-frame data on a successful stream")
	} else if diff, err := bamlfuzz.SemanticDiff(leg+"_final", expected, out.finalData); err != nil {
		failures = append(failures, fmt.Sprintf("%s final diff: %v", leg, err))
	} else if len(diff) > 0 {
		envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
		failures = append(failures, leg+": final data ≠ Expected")
	}
	failures = append(failures, out.frameFailures...)
	return failures
}

// frameLeak returns a non-empty description when a streaming frame's
// parseable data or raw text carries the reasoning leak marker — thinking
// must never reach either channel.
func frameLeak(leg, kind string, data json.RawMessage, raw string) string {
	if len(data) > 0 && strings.Contains(string(data), reasoningLeakMarker) {
		return fmt.Sprintf("%s %s data leaked thinking marker: %s", leg, kind, string(data))
	}
	if strings.Contains(raw, reasoningLeakMarker) {
		return fmt.Sprintf("%s %s raw leaked thinking marker: %q", leg, kind, raw)
	}
	return ""
}

// drainReasoningDynclientStream opens a dynclient DynamicStreamRaw with the
// given flag and drains it to EOF, accumulating the cumulative reasoning
// (last non-empty snapshot from a data/final frame — final frames carry the
// full text), the final data, and any per-frame violation. Reasoning is
// accumulated ONLY from partial/final frames; reasoning on a reset/metadata
// (or any other) frame is a contract violation (reasoning rides inside
// data/final frames — there is no dedicated reasoning frame) and is
// recorded as a frame failure. A context error is a harness failure
// (t.Fatalf); a panic or other drain error returns ok=false + errStr for
// the caller to escalate.
func drainReasoningDynclientStream(t *testing.T, dyn *dynclient.Client, base dynclient.Request, includeReasoning bool, c bamlfuzz.OracleCase, envelope *bamlfuzz.ReasoningFailureEnvelope) reasoningStreamOutcome {
	t.Helper()
	req := base
	req.IncludeReasoning = includeReasoning

	legLabel := "dynclient stream opt-in"
	if !includeReasoning {
		legLabel = "dynclient stream default"
	}
	recordErr := func(msg string) reasoningStreamOutcome {
		envelope.StreamError = msg
		return reasoningStreamOutcome{errStr: msg}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		stream  *dynclient.Stream
		openErr error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		stream, openErr = dyn.DynamicStreamRaw(ctx, req)
	})
	if panicked {
		envelope.DynclientPanicStack = string(panicStack)
		return recordErr(fmt.Sprintf("dyn.DynamicStreamRaw panicked: %v", panicVal))
	}
	if openErr != nil {
		if isContextErr(openErr) {
			t.Fatalf("harness failure: dynclient DynamicStreamRaw open (include_reasoning=%v, case=%s): %v", includeReasoning, c.Name, openErr)
		}
		return recordErr(openErr.Error())
	}
	defer stream.Close()

	out := reasoningStreamOutcome{ok: true}
	for {
		ev, e := stream.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			if isContextErr(e) {
				t.Fatalf("harness failure: dynclient DynamicStreamRaw next (include_reasoning=%v, case=%s): %v", includeReasoning, c.Name, e)
			}
			return recordErr(e.Error())
		}
		switch ev.Kind {
		case dynclient.EventPartial:
			if ev.Reasoning != "" {
				out.cumulativeReasoning = ev.Reasoning
			}
			if leak := frameLeak(legLabel, "partial", ev.Data, ev.Raw); leak != "" {
				out.frameFailures = append(out.frameFailures, leak)
			}
		case dynclient.EventFinal:
			if ev.Reasoning != "" {
				out.cumulativeReasoning = ev.Reasoning
			}
			out.finalData = append(json.RawMessage(nil), ev.Data...)
			if leak := frameLeak(legLabel, "final", ev.Data, ev.Raw); leak != "" {
				out.frameFailures = append(out.frameFailures, leak)
			}
		default:
			// reset / metadata / any future kind: reasoning must NOT ride
			// here — the contract is that reasoning travels inside
			// data/final frames only.
			if ev.Reasoning != "" {
				out.frameFailures = append(out.frameFailures,
					fmt.Sprintf("%s: reasoning on a non-data frame (kind=%s): %q", legLabel, ev.Kind, ev.Reasoning))
			}
		}
	}
	return out
}

// drainReasoningRESTStream drives the REST /stream-with-raw/_dynamic SSE
// leg with the given flag via an order-preserving body, accumulating the
// cumulative reasoning, final data, and per-frame violations with the same
// provenance enforcement as the dynclient drain: reasoning is accepted
// ONLY from data/final frames; reasoning on a metadata/reset/error frame is
// a frame failure. Context errors t.Fatalf; other errors surface ok=false +
// errStr for the caller to escalate.
func drainReasoningRESTStream(t *testing.T, lowered *bamlutils.DynamicOutputSchema, base dynclient.Request, includeReasoning bool, c bamlfuzz.OracleCase, envelope *bamlfuzz.ReasoningFailureEnvelope) reasoningStreamOutcome {
	t.Helper()
	req := base
	req.IncludeReasoning = includeReasoning

	legLabel := "REST stream opt-in"
	if !includeReasoning {
		legLabel = "REST stream default"
	}
	recordErr := func(msg string) reasoningStreamOutcome {
		envelope.StreamError = msg
		return reasoningStreamOutcome{errStr: msg}
	}

	body, berr := buildDynamicCallBody(req, lowered)
	if berr != nil {
		return recordErr(fmt.Sprintf("build REST stream body: %v", berr))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		events <-chan testutil.StreamEvent
		errs   <-chan error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		events, errs = BAMLClient.DynamicStreamWithRawBody(ctx, body)
	})
	if panicked {
		envelope.RESTPanicStack = string(panicStack)
		return recordErr(fmt.Sprintf("DynamicStreamWithRawBody panicked: %v", panicVal))
	}

	out := reasoningStreamOutcome{ok: true}
	for ev := range events {
		switch {
		case ev.IsFinal():
			if ev.Reasoning != "" {
				out.cumulativeReasoning = ev.Reasoning
			}
			out.finalData = append(json.RawMessage(nil), ev.Data...)
			if leak := frameLeak(legLabel, "final", ev.Data, ev.Raw); leak != "" {
				out.frameFailures = append(out.frameFailures, leak)
			}
		case ev.IsPartialData():
			if ev.Reasoning != "" {
				out.cumulativeReasoning = ev.Reasoning
			}
			if leak := frameLeak(legLabel, "partial", ev.Data, ev.Raw); leak != "" {
				out.frameFailures = append(out.frameFailures, leak)
			}
		default:
			// metadata / reset / error (or any future) frame: reasoning must
			// NOT ride here — there is no dedicated reasoning frame type.
			if ev.Reasoning != "" {
				out.frameFailures = append(out.frameFailures,
					fmt.Sprintf("%s: reasoning on a non-data frame: %q", legLabel, ev.Reasoning))
			}
		}
	}
	if e, okErr := <-errs; okErr && e != nil {
		if isContextErr(e) {
			t.Fatalf("harness failure: REST DynamicStreamWithRawBody (include_reasoning=%v, case=%s): %v", includeReasoning, c.Name, e)
		}
		return recordErr(e.Error())
	}
	return out
}

// failAndDumpReasoning writes the ReasoningFailureEnvelope to the artifact
// dir and fails the test with a message that points at the replay path. A
// failed artifact write must not also cost the developer the one-line repro
// command, so the repro is emitted even when the write fails.
func failAndDumpReasoning(t *testing.T, envelope *bamlfuzz.ReasoningFailureEnvelope, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	path, err := bamlfuzz.WriteReasoningReplayArtifact(reasoningOracleArtifactDir, envelope)
	if err != nil {
		t.Errorf("write replay artifact: %v", err)
		t.Errorf("%s\nrepro: %s", msg, envelope.Reproduction)
		return
	}
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// reproductionForReasoning returns the canonical command to re-run a
// failing reasoning case in isolation, embedded in the envelope so a
// developer can copy-paste it. The subtest trees mirror reproductionForRaw:
//
//	TestBamlfuzzReasoningOracle / corpus / <case_name>
//	TestBamlfuzzReasoningOracle / rapid  / preserve_{on,off} / case_<index>
//
// Fuzz cases bypass the subtest tree: the engine reaches them by replaying
// a corpus entry under -fuzzcachedir, so the recipe runs the engine.
func reproductionForReasoning(c bamlfuzz.OracleCase, caseIdx int, source caseSource) string {
	if source == caseSourceFuzz {
		return "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzReasoning$' -fuzztime=10m " +
			"-fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	}
	segments := []string{"^TestBamlfuzzReasoningOracle$"}
	switch source {
	case caseSourceCorpus:
		segments = append(segments, "^corpus$", "^"+regexp.QuoteMeta(c.Name)+"$")
	case caseSourceRapid:
		preserve := "preserve_off"
		if c.PreserveSchemaOrder {
			preserve = "preserve_on"
		}
		segments = append(segments,
			"^rapid$",
			"^"+regexp.QuoteMeta(preserve)+"$",
			fmt.Sprintf("^case_%d$", caseIdx),
		)
	}
	cmd := fmt.Sprintf("go test -tags=integration -run='%s' ./integration -count=1",
		strings.Join(segments, "/"))
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if cases := os.Getenv("BAMLFUZZ_REASONING_CASES"); cases != "" {
		cmd = "BAMLFUZZ_REASONING_CASES=" + cases + " " + cmd
	}
	return cmd
}

// TestReproductionForReasoning pins the shape of the reproduction command
// embedded in reasoning-oracle failure envelopes across the corpus, rapid,
// and fuzz source trees plus the BAMLFUZZ_SEED / BAMLFUZZ_REASONING_CASES
// env prefixes.
func TestReproductionForReasoning(t *testing.T) {
	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_REASONING_CASES", "")

	corpus := reproductionForReasoning(bamlfuzz.OracleCase{Name: "scalar_string"}, 0, caseSourceCorpus)
	wantCorpus := "go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^corpus$/^scalar_string$' ./integration -count=1"
	if corpus != wantCorpus {
		t.Errorf("corpus repro:\n got:  %s\n want: %s", corpus, wantCorpus)
	}

	rapidOn := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 2, caseSourceRapid)
	wantRapidOn := "go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^rapid$/^preserve_on$/^case_2$' ./integration -count=1"
	if rapidOn != wantRapidOn {
		t.Errorf("rapid preserve-on repro:\n got:  %s\n want: %s", rapidOn, wantRapidOn)
	}

	rapidOff := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 3, caseSourceRapid)
	wantRapidOff := "go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^rapid$/^preserve_off$/^case_3$' ./integration -count=1"
	if rapidOff != wantRapidOff {
		t.Errorf("rapid preserve-off repro:\n got:  %s\n want: %s", rapidOff, wantRapidOff)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	withSeed := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 0, caseSourceRapid)
	wantWithSeed := "BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^rapid$/^preserve_on$/^case_0$' ./integration -count=1"
	if withSeed != wantWithSeed {
		t.Errorf("rapid+seed repro:\n got:  %s\n want: %s", withSeed, wantWithSeed)
	}

	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_REASONING_CASES", "50")
	withCases := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 25, caseSourceRapid)
	wantWithCases := "BAMLFUZZ_REASONING_CASES=50 go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withCases != wantWithCases {
		t.Errorf("rapid+cases repro:\n got:  %s\n want: %s", withCases, wantWithCases)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	t.Setenv("BAMLFUZZ_REASONING_CASES", "50")
	withSeedAndCases := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 25, caseSourceRapid)
	wantWithSeedAndCases := "BAMLFUZZ_REASONING_CASES=50 BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzReasoningOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withSeedAndCases != wantWithSeedAndCases {
		t.Errorf("rapid+seed+cases repro:\n got:  %s\n want: %s", withSeedAndCases, wantWithSeedAndCases)
	}

	fuzz := reproductionForReasoning(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 0, caseSourceFuzz)
	wantFuzz := "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzReasoning$' -fuzztime=10m -fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	if fuzz != wantFuzz {
		t.Errorf("fuzz repro:\n got:  %s\n want: %s", fuzz, wantFuzz)
	}
}

// TestReasoningChannelFailures pins the pure reasoning-channel checker:
// the R1/R2/R4 + raw-echo + data-presence invariants that govern one leg.
// Driving it directly (rather than through the Docker oracle) lets the
// vacuous-pass guards and the separation invariant be asserted in
// isolation — a Go subtest failure cannot be "expected", so the logic
// lives in a pure function.
func TestReasoningChannelFailures(t *testing.T) {
	const thinking = "thinking-text {\"DO_NOT_LEAK_INTO_PARSEABLE\":\"x\"}"
	const content = `{"k":"v"}`
	data := json.RawMessage(content)

	// The all-clean baseline: opt-in echoes thinking, default empty, data
	// byte-identical, raw == content on both.
	cleanOn := reasoningLegOutcome{ok: true, reasoning: thinking, data: data, raw: content}
	cleanOff := reasoningLegOutcome{ok: true, reasoning: "", data: data, raw: content}

	cases := []struct {
		name      string
		on        reasoningLegOutcome
		off       reasoningLegOutcome
		wantMatch []string // substrings that must each appear in some failure
		wantClean bool     // expect zero failures
	}{
		{name: "all_clean", on: cleanOn, off: cleanOff, wantClean: true},
		{
			name:      "optin_reasoning_empty_fails_R1",
			on:        reasoningLegOutcome{ok: true, reasoning: "", data: data, raw: content},
			off:       cleanOff,
			wantMatch: []string{"opt-in reasoning is empty", "reasoning identical across flag states"},
		},
		{
			name:      "optin_reasoning_mismatch_fails_R1",
			on:        reasoningLegOutcome{ok: true, reasoning: "other", data: data, raw: content},
			off:       cleanOff,
			wantMatch: []string{"opt-in reasoning ≠ thinking"},
		},
		{
			name:      "default_reasoning_nonempty_fails_R4",
			on:        cleanOn,
			off:       reasoningLegOutcome{ok: true, reasoning: thinking, data: data, raw: content},
			wantMatch: []string{"default reasoning non-empty", "reasoning identical across flag states"},
		},
		{
			name:      "data_differs_across_flag_fails_R2",
			on:        cleanOn,
			off:       reasoningLegOutcome{ok: true, reasoning: "", data: json.RawMessage(`{"k":"LEAKED"}`), raw: content},
			wantMatch: []string{"parsed data differs across flag states"},
		},
		{
			name:      "raw_not_echo_fails",
			on:        reasoningLegOutcome{ok: true, reasoning: thinking, data: data, raw: "different"},
			off:       cleanOff,
			wantMatch: []string{"opt-in raw ≠ MockLLMContent", "raw differs across flag states"},
		},
		{
			name:      "optin_empty_data_fails_presence",
			on:        reasoningLegOutcome{ok: true, reasoning: thinking, data: nil, raw: content},
			off:       cleanOff,
			wantMatch: []string{"opt-in returned empty parsed data"},
		},
		{
			name:      "errored_legs_not_flagged",
			on:        reasoningLegOutcome{ok: false},
			off:       reasoningLegOutcome{ok: false},
			wantClean: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := reasoningChannelFailures("dynclient", thinking, content, tc.on, tc.off)
			if tc.wantClean {
				if len(got) != 0 {
					t.Fatalf("expected no failures, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected failures, got none")
			}
			for _, want := range tc.wantMatch {
				found := false
				for _, g := range got {
					if strings.Contains(g, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a failure containing %q; got %v", want, got)
				}
			}
		})
	}
}

// TestLegFailureFrom pins the leg-error vacuous-pass guard: an ok==false
// leg MUST contribute its error string as a failure (never be skipped),
// while an ok==true leg contributes nothing. This is the unit-level proof
// that a broken unary or streaming leg cannot pass as long as other legs
// pass.
func TestLegFailureFrom(t *testing.T) {
	if got := legFailureFrom("dynclient opt-in", true, ""); got != nil {
		t.Errorf("ok leg should contribute no failures, got %v", got)
	}
	got := legFailureFrom("dynclient opt-in", false, "boom")
	if len(got) != 1 || !strings.Contains(got[0], "boom") || !strings.Contains(got[0], "dynclient opt-in") {
		t.Errorf("errored leg must surface its label+error, got %v", got)
	}
	// A non-ok leg with no recorded error still fails (no silent skip).
	got = legFailureFrom("REST default", false, "")
	if len(got) != 1 || !strings.Contains(got[0], "did not complete") {
		t.Errorf("non-ok leg with empty errStr must still fail, got %v", got)
	}
}

// TestAssertStreamOutcome pins the per-stream R3/R4 assertions: opt-in
// streams must echo the thinking and carry a present, correct final data;
// default streams must have EMPTY reasoning AND still a present, correct
// final data; frame failures (leak/provenance) always propagate. This is
// the unit-level proof that the flag-off streaming path validates final
// data (not just reasoning) and that R4 holds on the streaming path.
func TestAssertStreamOutcome(t *testing.T) {
	const thinking = "think {\"x\":1}"
	expected := json.RawMessage(`{"k":"v"}`)
	good := json.RawMessage(`{"k":"v"}`)
	bad := json.RawMessage(`{"k":"LEAKED"}`)

	cases := []struct {
		name          string
		out           reasoningStreamOutcome
		wantReasoning string
		wantClean     bool
		wantMatch     []string
	}{
		{
			name:          "optin_clean",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: thinking, finalData: good},
			wantReasoning: thinking,
			wantClean:     true,
		},
		{
			name:          "default_clean_empty_reasoning",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: "", finalData: good},
			wantReasoning: "",
			wantClean:     true,
		},
		{
			name:          "default_with_reasoning_fails_R4",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: thinking, finalData: good},
			wantReasoning: "",
			wantMatch:     []string{"cumulative reasoning non-empty under default flag"},
		},
		{
			name:          "optin_empty_reasoning_fails",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: "", finalData: good},
			wantReasoning: thinking,
			wantMatch:     []string{"cumulative reasoning is empty"},
		},
		{
			name:          "optin_missing_final_data_fails",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: thinking, finalData: nil},
			wantReasoning: thinking,
			wantMatch:     []string{"empty final-frame data"},
		},
		{
			name:          "default_missing_final_data_fails",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: "", finalData: nil},
			wantReasoning: "",
			wantMatch:     []string{"empty final-frame data"},
		},
		{
			name:          "final_data_diverges_fails",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: thinking, finalData: bad},
			wantReasoning: thinking,
			wantMatch:     []string{"final data ≠ Expected"},
		},
		{
			name:          "frame_failures_propagate",
			out:           reasoningStreamOutcome{ok: true, cumulativeReasoning: thinking, finalData: good, frameFailures: []string{"frame leaked marker"}},
			wantReasoning: thinking,
			wantMatch:     []string{"frame leaked marker"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := &bamlfuzz.ReasoningFailureEnvelope{}
			got := assertStreamOutcome("leg", tc.out, tc.wantReasoning, expected, env)
			if tc.wantClean {
				if len(got) != 0 {
					t.Fatalf("expected no failures, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected failures, got none")
			}
			for _, want := range tc.wantMatch {
				found := false
				for _, g := range got {
					if strings.Contains(g, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a failure containing %q; got %v", want, got)
				}
			}
		})
	}
}
