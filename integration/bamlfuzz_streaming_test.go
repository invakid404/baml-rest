//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// streamingOracleArtifactDir is where streaming-oracle envelope artifacts
// land on failure or with BAMLFUZZ_KEEP_ARTIFACTS=1. Sibling to the
// dynamic / invalid dirs so the nightly's `_artifacts/` glob collects it
// with no workflow change.
const streamingOracleArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/streaming/_artifacts"

// streamingCasesPerMode controls how many random streaming cases run per
// preserve mode. Sized for PR CI wall time (each case drives four calls:
// dynclient stream, REST SSE stream, REST NDJSON stream, unary
// cross-check); the nightly fuzz workflow cranks it via
// BAMLFUZZ_STREAMING_CASES so a scheduled run explores a broader slice
// of the schema space.
func streamingCasesPerMode() int {
	return envIntDefault("BAMLFUZZ_STREAMING_CASES", 16)
}

// TestBamlfuzzStreamingOracle drives the streaming dynamic oracle: for
// each fuzz case it lowers the schema through the dynamic emitter, then
// drives the same case through the streaming surfaces and asserts on the
// terminal frame:
//
//  1. three-way final equivalence — dynclient stream final ≡ REST SSE
//     stream final ≡ walker Expected;
//  2. final-frame key order on each leg when PreserveSchemaOrder is true;
//  3. streaming final ≡ unary parse (streaming-vs-unary divergence);
//  4. SSE final ≡ NDJSON final (transport parity);
//  5. each leg terminates in exactly one final, no error event, and no
//     unexpected reset.
//
// Only the terminal frame is asserted on. Partial frames are full
// snapshots with placeholder nulls and are not reordered, so a key-order
// assertion on a partial would spuriously fail; the final frame is the
// only one with a contract-shaped key order and is chunk-timing
// invariant.
func TestBamlfuzzStreamingOracle(t *testing.T) {
	dynclientCallGate(t)

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}

	t.Run("rapid", func(t *testing.T) {
		modes := []bool{true, false}
		for _, preserve := range modes {
			preserve := preserve
			label := "preserve_off"
			if preserve {
				label = "preserve_on"
			}
			t.Run(label, func(t *testing.T) {
				cases := streamingCasesPerMode()
				for i := 0; i < cases; i++ {
					i := i
					seed := streamingSeedFor(preserve, i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						c := buildStreamingRapidCase(t, seed, preserve, i)
						runStreamingOracleCase(t, dyn, c, i, caseSourceRapid)
					})
				}
			})
		}
	})
}

// FuzzBamlfuzzStreaming is the testing.F companion to
// TestBamlfuzzStreamingOracle. It exposes the streaming oracle to Go's
// native fuzz engine via the bamlfuzz.MakeFuzz bridge: testing.F's
// []byte corpus is decoded (8-byte LE) or hashed via SeedFromBytes into
// a uint64 seed that drives one CoupledCaseGen draw against the
// dynamic-safe schema generator. PreserveSchemaOrder is drawn from the
// bit stream so the fuzzer explores both halves of the oracle.
//
// One f.Add seed is registered so plain `go test` (no `-fuzz=`) runs the
// target with a deterministic input. The seed is derived FNV-64a from a
// stable label and is NOT folded with BAMLFUZZ_SEED — the nightly's fuzz
// mode sets BAMLFUZZ_SEED=github.run_id in the job env, but the fuzz
// repro command does not echo it back (only the corpus bytes under
// -fuzzcachedir identify the failing input), so folding it in would make
// f.Add bytes differ between the nightly and a developer machine and
// break replay.
func FuzzBamlfuzzStreaming(f *testing.F) {
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

	f.Add(streamingFuzzSeedBytes("streaming"))

	bamlfuzz.MakeFuzz(f, func(t *testing.T, rt *rapid.T) {
		preserve := rapid.Bool().Draw(rt, "preserve_schema_order")
		cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Draw(rt, "coupled_case")
		c := bamlfuzz.OracleCase{
			Name:                "fuzz",
			Seed:                0,
			CaseIndex:           0,
			Mode:                bamlfuzz.OracleDynamicStreaming,
			PreserveSchemaOrder: preserve,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
		runStreamingOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// streamingSeedFor produces a deterministic rapid seed for a (preserve,
// i) pair. Tests are deterministic across runs unless BAMLFUZZ_SEED is
// set; BAMLFUZZ_SEED, when present, XORs into the per-case seed so a
// single env var perturbs the entire matrix at once. Same convention as
// dynamicSeedFor.
func streamingSeedFor(preserve bool, i int) uint64 {
	label := "streaming_preserve_off"
	if preserve {
		label = "streaming_preserve_on"
	}
	base := fnv64aString(fmt.Sprintf("%s:%d", label, i))
	if v := os.Getenv("BAMLFUZZ_SEED"); v != "" {
		base ^= fnv64aString(v)
	}
	return base
}

// streamingFuzzSeedBytes derives the deterministic 8-byte fuzz-corpus
// seed for the f.Add input from a stable label. The output is the exact
// wire shape bamlfuzz.MakeFuzz recognizes as a verbatim uint64. The
// label-only derivation (no BAMLFUZZ_SEED fold) keeps the f.Add bytes
// byte-identical across every environment so a failed nightly's
// `go test -fuzz` command replays the same case it failed on.
func streamingFuzzSeedBytes(label string) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, fnv64aString(label))
	return b
}

// buildStreamingRapidCase synthesizes one streaming OracleCase
// deterministically from seed. Uses CoupledCaseGen so the value
// generator's union-choice metadata + the shrink-biased collapse pass
// land on the case together — the same draw the dynamic oracle uses,
// driven through the streaming path instead.
func buildStreamingRapidCase(t *testing.T, seed uint64, preserve bool, idx int) bamlfuzz.OracleCase {
	t.Helper()
	cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Example(int(seed))
	return bamlfuzz.OracleCase{
		Name:                fmt.Sprintf("streaming_rapid_%t_%d", preserve, idx),
		Seed:                int64(seed),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleDynamicStreaming,
		PreserveSchemaOrder: preserve,
		Schema:              cc.Schema,
		Value:               cc.Value,
		MockLLMContent:      cc.Walk.MockLLMContent,
		Expected:            cc.Walk.Expected,
		Metadata:            cc.Walk.Metadata,
	}
}

// streamChunkSizeFor derives the mockllm per-chunk byte width from the
// case seed, bounded to [8, 32]. >0 so mockllm genuinely pages the
// response across frames and exercises the streaming/partial code path;
// ChunkSize 0 would emit a single blob and skip it. v1 asserts only on
// the final frame, which the pre-flight proved is identical regardless
// of chunk granularity, so the exact size does not affect the verdict —
// it just varies which byte boundaries the partial sequence splits on
// across cases.
func streamChunkSizeFor(seed int64) int {
	return 8 + int(uint64(seed)%25)
}

// runStreamingOracleCase drives one OracleCase through the streaming
// surfaces and applies the five v1 assertions, capturing all relevant
// context into a StreamingFailureEnvelope when any leg disagrees.
func runStreamingOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, caseIdx int, source caseSource) {
	t.Helper()

	chunkSize := streamChunkSizeFor(c.Seed)
	envelope := &bamlfuzz.StreamingFailureEnvelope{
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          bamlfuzz.OracleDynamicStreaming,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		Schema:              c.Schema,
		MockLLMContent:      c.MockLLMContent,
		Expected:            c.Expected,
		Metadata:            c.Metadata,
		ChunkSize:           chunkSize,
		Reproduction:        reproductionForStreaming(c, caseIdx, source),
	}

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		switch unsupportedActionFor(source) {
		case unsupportedSkip:
			t.Skipf("dynamic emitter skipped schema: %v", err)
			return
		case unsupportedFail:
			envelope.DynamicSkipReason = err.Error()
			failAndDumpStreaming(t, envelope, "rapid generator produced unsupported schema: %v", err)
			return
		}
	}
	if err != nil {
		envelope.DynamicSkipReason = err.Error()
		failAndDumpStreaming(t, envelope, "LowerToDynamicSchema failed: %v", err)
		return
	}
	envelope.DynamicSchema = &lowered

	scenarioID := fmt.Sprintf("bamlfuzz-stream-%s", scenarioSafe(c.Name))
	envelope.MockLLMScenarioID = scenarioID
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer registerCancel()
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        string(c.MockLLMContent),
		ChunkSize:      chunkSize,
		ChunkDelayMs:   0,
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(registerCtx, scenario); err != nil {
		failAndDumpStreaming(t, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID)

	preserve := c.PreserveSchemaOrder
	hello := "Return the dynamic fuzz value."
	libReq := dynclient.Request{
		Messages: []dynclient.Message{
			{Role: "system", PartsContent: []dynclient.ContentPart{
				{Type: "text", Text: &hello},
				{Type: "output_format"},
			}},
			{Role: "user", TextContent: &hello},
		},
		ClientRegistry:      testutil.DynRegistry(clientReg),
		OutputSchema:        &lowered,
		PreserveSchemaOrder: &preserve,
	}

	// REST legs are driven with a body built via bamlutils so the
	// OrderedMap insertion order survives the wire; the map-backed
	// testutil.DynamicOutputSchema would scramble property order and
	// defeat the final-frame key-order assertion.
	restBody, err := buildDynamicCallBody(libReq, &lowered)
	if err != nil {
		failAndDumpStreaming(t, envelope, "build REST body: %v", err)
		return
	}

	// Independent contexts per surface, derived from a shared parent.
	// A shared deadline would let one surface draining the clock mask
	// another's failure; per-surface child contexts give each of the
	// four calls its own budget, while the common parent still lets a
	// test-level cancellation abort all of them together.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	var failures []string

	// --- dynclient streaming leg ---
	dynCtx, dynCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer dynCancel()
	stream, openErr := dyn.DynamicStream(dynCtx, libReq)
	if openErr != nil {
		if isContextErr(openErr) {
			t.Fatalf("harness failure: dynclient stream open (case=%s): %v", c.Name, openErr)
		}
		envelope.DynclientError = openErr.Error()
		failures = append(failures, fmt.Sprintf("dynclient stream open: %v", openErr))
	} else {
		dynPartials, dynFinal, dynKinds, drainErr := drainDynclientStream(stream)
		stream.Close()
		envelope.DynclientPartials = dynPartials
		envelope.DynclientFinal = dynFinal
		envelope.DynclientKinds = dynKinds
		if drainErr != nil {
			if isContextErr(drainErr) {
				t.Fatalf("harness failure: dynclient stream drain (case=%s): %v", c.Name, drainErr)
			}
			envelope.DynclientError = drainErr.Error()
			failures = append(failures, fmt.Sprintf("dynclient stream errored: %v", drainErr))
		}
	}

	// --- REST SSE streaming leg ---
	sseCtx, sseCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer sseCancel()
	sseEvents, sseErrs := BAMLClient.DynamicStreamBody(sseCtx, restBody)
	ssePartials, sseFinal, sseKinds, sseErr := drainRESTStream(sseEvents, sseErrs)
	envelope.RESTPartialsSSE = ssePartials
	envelope.RESTFinalSSE = sseFinal
	envelope.RESTKindsSSE = sseKinds
	if sseErr != nil {
		if isContextErr(sseErr) {
			t.Fatalf("harness failure: REST SSE stream (case=%s): %v", c.Name, sseErr)
		}
		envelope.RESTErrorSSE = sseErr.Error()
		failures = append(failures, fmt.Sprintf("REST SSE stream errored: %v", sseErr))
	}

	// --- REST NDJSON streaming leg ---
	ndjsonCtx, ndjsonCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer ndjsonCancel()
	ndjsonEvents, ndjsonErrs := BAMLClient.DynamicStreamNDJSONBody(ndjsonCtx, restBody)
	ndjsonPartials, ndjsonFinal, ndjsonKinds, ndjsonErr := drainRESTStream(ndjsonEvents, ndjsonErrs)
	envelope.RESTPartialsNDJSON = ndjsonPartials
	envelope.RESTFinalNDJSON = ndjsonFinal
	envelope.RESTKindsNDJSON = ndjsonKinds
	if ndjsonErr != nil {
		if isContextErr(ndjsonErr) {
			t.Fatalf("harness failure: REST NDJSON stream (case=%s): %v", c.Name, ndjsonErr)
		}
		envelope.RESTErrorNDJSON = ndjsonErr.Error()
		failures = append(failures, fmt.Sprintf("REST NDJSON stream errored: %v", ndjsonErr))
	}

	// --- unary cross-check leg ---
	unaryCtx, unaryCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer unaryCancel()
	unaryResp, unaryErr := dyn.DynamicCall(unaryCtx, libReq)
	switch {
	case unaryErr != nil:
		if isContextErr(unaryErr) {
			t.Fatalf("harness failure: unary cross-check (case=%s): %v", c.Name, unaryErr)
		}
		envelope.UnaryError = unaryErr.Error()
		failures = append(failures, fmt.Sprintf("unary cross-check errored: %v", unaryErr))
	case unaryResp == nil:
		envelope.UnaryError = "nil response from dyn.DynamicCall"
		failAndDumpStreaming(t, envelope, "unary cross-check returned nil response without an error")
		return
	default:
		envelope.UnaryParse = unaryResp.Data
	}

	// Assertion 5 — each streaming leg terminates in exactly one final,
	// no error event, no unexpected reset. Run first so a degenerate
	// stream (no final) is reported even when the equivalence diffs
	// below short-circuit on an empty payload.
	dynReset := checkSingleCleanFinal("dynclient", envelope.DynclientKinds, &failures)
	sseReset := checkSingleCleanFinal("rest_sse", envelope.RESTKindsSSE, &failures)
	ndjsonReset := checkSingleCleanFinal("rest_ndjson", envelope.RESTKindsNDJSON, &failures)
	envelope.SawReset = dynReset || sseReset || ndjsonReset

	// Assertion 1 — three-way final equivalence: dynclient final ≡ REST
	// SSE final ≡ Expected.
	diffAndRecord(envelope, &failures, "expected_vs_dynclient_final", c.Expected, envelope.DynclientFinal)
	diffAndRecord(envelope, &failures, "expected_vs_rest_sse_final", c.Expected, envelope.RESTFinalSSE)
	diffAndRecord(envelope, &failures, "dynclient_vs_rest_sse_final", envelope.DynclientFinal, envelope.RESTFinalSSE)

	// Assertion 3 — streaming final ≡ unary parse. Directly probes a
	// streaming-vs-unary parser divergence.
	diffAndRecord(envelope, &failures, "dynclient_final_vs_unary", envelope.DynclientFinal, envelope.UnaryParse)

	// Assertion 4 — transport parity: SSE final ≡ NDJSON final.
	diffAndRecord(envelope, &failures, "rest_sse_vs_ndjson_final", envelope.RESTFinalSSE, envelope.RESTFinalNDJSON)

	// Assertion 2 — final-frame key order on each leg (preserve only).
	checkStreamFinalOrder(t, c, envelope, &failures, "dynclient_final_order", envelope.DynclientFinal)
	checkStreamFinalOrder(t, c, envelope, &failures, "rest_sse_final_order", envelope.RESTFinalSSE)
	checkStreamFinalOrder(t, c, envelope, &failures, "rest_ndjson_final_order", envelope.RESTFinalNDJSON)

	if len(failures) == 0 {
		if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
			if path, err := bamlfuzz.WriteStreamingReplayArtifact(streamingOracleArtifactDir, envelope); err == nil {
				t.Logf("kept replay artifact (success): %s", path)
			}
		}
		return
	}
	failAndDumpStreaming(t, envelope, "%s", strings.Join(failures, "; "))
}

// diffAndRecord runs a SemanticDiff for one leg pairing and records any
// disagreement on the envelope + failures list. A missing operand
// (empty payload) is skipped here — the absent final is reported by the
// single-clean-final check, so re-flagging it as a decode error would
// just duplicate the signal.
func diffAndRecord(envelope *bamlfuzz.StreamingFailureEnvelope, failures *[]string, side string, a, b json.RawMessage) {
	if len(a) == 0 || len(b) == 0 {
		return
	}
	diff, err := bamlfuzz.SemanticDiff(side, a, b)
	if err != nil {
		*failures = append(*failures, fmt.Sprintf("%s diff: %v", side, err))
		return
	}
	if len(diff) > 0 {
		envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
		*failures = append(*failures, side+" mismatch")
	}
}

// checkStreamFinalOrder runs the strict schema-aware key-order assertion
// on one leg's final frame against the walker's Expected when the case
// opts into PreserveSchemaOrder. Mirrors the unary oracle's
// checkSchemaOrder: ErrSchemaOrderUnsupported is a hard failure (a
// missing union choice is an integrity bug), and an order mismatch
// records the diagnostic on OrderWarning before appending a failure.
// Final frames are reordered (partials are not), so the order contract
// applies only here.
func checkStreamFinalOrder(t *testing.T, c bamlfuzz.OracleCase, envelope *bamlfuzz.StreamingFailureEnvelope, failures *[]string, label string, finalData json.RawMessage) {
	t.Helper()
	if !c.PreserveSchemaOrder || len(finalData) == 0 {
		return
	}
	diffs, err := bamlfuzz.SchemaOrderDiffWithChoices(label, c.Schema, c.Expected, finalData, c.Metadata.UnionChoices)
	switch {
	case errors.Is(err, bamlfuzz.ErrSchemaOrderUnsupported):
		envelope.OrderWarning = append(envelope.OrderWarning, fmt.Sprintf("%s: %v", label, err))
		*failures = append(*failures, fmt.Sprintf("%s schema order unsupported: %v", label, err))
	case err != nil:
		envelope.OrderWarning = append(envelope.OrderWarning, fmt.Sprintf("%s: %v", label, err))
		*failures = append(*failures, fmt.Sprintf("%s schema order: %v", label, err))
	case len(diffs) > 0:
		envelope.OrderWarning = append(envelope.OrderWarning, bamlfuzz.FormatSchemaOrderDiffs(diffs)...)
		*failures = append(*failures, fmt.Sprintf("%s: schema key order mismatch", label))
	}
}

// checkSingleCleanFinal applies assertion 5 to one leg's event-kind
// sequence: exactly one final, no error event, no reset. Returns whether
// a reset was seen so the caller can aggregate SawReset across legs. A
// reset is a finding (not a silent pass) because the rapid generator's
// mock scenarios are deterministic single-attempt, so the orchestrator
// never retries.
func checkSingleCleanFinal(leg string, kinds []string, failures *[]string) (sawReset bool) {
	finals := 0
	for _, k := range kinds {
		switch k {
		case "final":
			finals++
		case "reset":
			sawReset = true
		case "error":
			*failures = append(*failures, fmt.Sprintf("%s: error event in stream", leg))
		}
	}
	if finals != 1 {
		*failures = append(*failures, fmt.Sprintf("%s: expected exactly one final frame, saw %d", leg, finals))
	}
	if sawReset {
		*failures = append(*failures, fmt.Sprintf("%s: unexpected reset frame (deterministic single-attempt mock)", leg))
	}
	return sawReset
}

// drainDynclientStream consumes a dynclient streaming iterator to
// completion, returning the ordered partial snapshots, the final frame,
// the ordered event-kind sequence, and the terminal error (nil on clean
// io.EOF). Mid-stream worker errors surface as the returned error rather
// than an "error" kind. Payloads are copied off the pooled byte slices.
func drainDynclientStream(s *dynclient.Stream) (partials []json.RawMessage, final json.RawMessage, kinds []string, err error) {
	for {
		ev, e := s.Next()
		if errors.Is(e, io.EOF) {
			return partials, final, kinds, nil
		}
		if e != nil {
			return partials, final, kinds, e
		}
		switch ev.Kind {
		case dynclient.EventPartial:
			kinds = append(kinds, "partial")
			partials = append(partials, append(json.RawMessage(nil), ev.Data...))
		case dynclient.EventFinal:
			kinds = append(kinds, "final")
			final = append(json.RawMessage(nil), ev.Data...)
		case dynclient.EventReset:
			kinds = append(kinds, "reset")
		case dynclient.EventMetadata:
			kinds = append(kinds, "metadata")
		}
	}
}

// drainRESTStream consumes a REST streaming event channel (SSE or
// NDJSON) to completion, returning the ordered partial snapshots, the
// final frame, the ordered event-kind sequence, and the transport-level
// error (nil on clean completion). An "error" event in the stream is
// recorded as an "error" kind, not as the returned error — the returned
// error is reserved for transport / parse / HTTP-status failures
// surfaced on the error channel.
func drainRESTStream(events <-chan testutil.StreamEvent, errs <-chan error) (partials []json.RawMessage, final json.RawMessage, kinds []string, err error) {
	for ev := range events {
		switch {
		case ev.IsFinal():
			kinds = append(kinds, "final")
			final = append(json.RawMessage(nil), ev.Data...)
		case ev.IsReset():
			kinds = append(kinds, "reset")
		case ev.IsError():
			kinds = append(kinds, "error")
		case ev.IsMetadata():
			kinds = append(kinds, "metadata")
		case ev.IsPartialData():
			kinds = append(kinds, "partial")
			partials = append(partials, append(json.RawMessage(nil), ev.Data...))
		}
	}
	// The producer buffers the error and closes errs before closing
	// events, so by the time the range above exits the error (if any)
	// is already queued.
	if e, ok := <-errs; ok && e != nil {
		err = e
	}
	return partials, final, kinds, err
}

// isContextErr reports whether err is a context cancellation or deadline
// error. Context (and transport) errors mean the call never produced a
// verdict, so the oracle treats them as harness failures (t.Fatal), not
// oracle rejections — letting an outright timeout pass an equivalence
// check would mask a real divergence.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// failAndDumpStreaming writes the streaming envelope to the artifact dir
// and fails the test with a message pointing at the replay path. Mirrors
// failAndDump for the unary dynamic oracle.
func failAndDumpStreaming(t *testing.T, envelope *bamlfuzz.StreamingFailureEnvelope, format string, args ...any) {
	t.Helper()
	path, err := bamlfuzz.WriteStreamingReplayArtifact(streamingOracleArtifactDir, envelope)
	if err != nil {
		t.Errorf("write replay artifact: %v", err)
		t.Errorf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// reproductionForStreaming returns the canonical command to re-run a
// failing streaming case in isolation, embedded in the envelope. Mirrors
// reproductionForInvalid's source dispatch: fuzz cases re-run the engine
// against the persisted -fuzzcachedir; rapid cases re-run a single
// subtest under -run. Both pin -tags=integration,subprocess so the
// replay routes through the cmd/worker go-plugin path the nightly uses.
func reproductionForStreaming(c bamlfuzz.OracleCase, caseIdx int, source caseSource) string {
	if source == caseSourceFuzz {
		return "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzStreaming$' -fuzztime=10m " +
			"-fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	}
	mode := "preserve_off"
	if c.PreserveSchemaOrder {
		mode = "preserve_on"
	}
	runPath := fmt.Sprintf("^TestBamlfuzzStreamingOracle$/^rapid$/^%s$/^case_%d$", mode, caseIdx)
	cmd := fmt.Sprintf("go test -tags=integration,subprocess -run='%s' ./integration -count=1", runPath)
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if cases := os.Getenv("BAMLFUZZ_STREAMING_CASES"); cases != "" {
		cmd = "BAMLFUZZ_STREAMING_CASES=" + cases + " " + cmd
	}
	return cmd
}
