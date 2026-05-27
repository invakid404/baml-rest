//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
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

// invalidDynamicArtifactDir is where envelope artifacts land on Axis B
// failure (or with BAMLFUZZ_KEEP_ARTIFACTS=1). Mirrors the existing
// dynamic / static directory split so CI artifact collection picks up
// every oracle from one glob.
const invalidDynamicArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/invalid_dynamic/_artifacts"

// invalidJSONCoercionArtifactDir is where envelope artifacts land on
// Axis C failure.
const invalidJSONCoercionArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/invalid_json_coercion/_artifacts"

// invalidCasesPerRun controls how many random invalid-input cases run
// per oracle invocation. Sized for PR CI wall time; nightly fuzz can
// crank it via BAMLFUZZ_INVALID_CASES so a scheduled run explores a
// broader slice of the mutation × schema space.
func invalidCasesPerRun() int {
	return envIntDefault("BAMLFUZZ_INVALID_CASES", 16)
}

// TestBamlfuzzInvalidDynamic drives Axis B of the invalid-input oracles:
// a valid base FuzzSchema is lowered, then perturbed with one targeted
// mutation (drop a required composite field, set both type and ref on
// a property, etc.) so the resulting DynamicOutputSchema should be
// rejected. The oracle asserts both dynclient.Client.DynamicCall and
// the REST /call/_dynamic endpoint return an error — error text is
// not compared (the pre-flight check found dynclient and REST emit
// divergent surface-layer prefixes even when the BAML diagnostic core
// is identical).
func TestBamlfuzzInvalidDynamic(t *testing.T) {
	dynclientCallGate(t)

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}

	t.Run("rapid", func(t *testing.T) {
		cases := invalidCasesPerRun()
		for i := 0; i < cases; i++ {
			i := i
			seed := invalidSeedFor("invalid_dynamic", i)
			t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
				c := buildRapidInvalidDynamicCase(t, seed, i)
				runInvalidOracleCase(t, dyn, c, i, caseSourceRapid)
			})
		}
	})
}

// FuzzBamlfuzzInvalidDynamic is the testing.F companion to
// TestBamlfuzzInvalidDynamic. It exposes the Axis B oracle to Go's
// native fuzz engine via the bamlfuzz.MakeFuzz bridge: testing.F's
// []byte corpus is decoded (8-byte LE) or hashed via SeedFromBytes
// into a uint64 seed that drives one InvalidDynamicSchemaGen draw.
//
// One f.Add seed is registered so plain `go test` (no `-fuzz=`) runs
// the target with a deterministic input. The seed is derived FNV-64a
// from a stable label so the same case replays across runs.
func FuzzBamlfuzzInvalidDynamic(f *testing.F) {
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

	f.Add(invalidFuzzSeedBytes("invalid_dynamic", 0))

	bamlfuzz.MakeFuzz(f, func(t *testing.T, rt *rapid.T) {
		c := bamlfuzz.InvalidDynamicSchemaGen().Draw(rt, "invalid_dynamic_case")
		c.Name = "fuzz_" + c.Mutation
		runInvalidOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// TestBamlfuzzInvalidJSONCoercion drives Axis C: a valid schema with a
// perturbed mock LLM JSON response. The oracle is conditional — when
// both surfaces parse successfully the two outputs must SemanticDiff
// equal (so a lenient-coercion divergence between dynclient and REST
// surfaces); when at least one surface errors both must error (no
// error-text comparison; see pre-flight finding).
func TestBamlfuzzInvalidJSONCoercion(t *testing.T) {
	dynclientCallGate(t)

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}

	t.Run("rapid", func(t *testing.T) {
		cases := invalidCasesPerRun()
		for i := 0; i < cases; i++ {
			i := i
			seed := invalidSeedFor("invalid_json_coercion", i)
			t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
				c := buildRapidInvalidJSONCase(t, seed, i)
				runInvalidOracleCase(t, dyn, c, i, caseSourceRapid)
			})
		}
	})
}

// FuzzBamlfuzzInvalidJSONCoercion is the testing.F companion to
// TestBamlfuzzInvalidJSONCoercion. Same bridge / seed pattern as
// FuzzBamlfuzzInvalidDynamic.
func FuzzBamlfuzzInvalidJSONCoercion(f *testing.F) {
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

	f.Add(invalidFuzzSeedBytes("invalid_json_coercion", 0))

	bamlfuzz.MakeFuzz(f, func(t *testing.T, rt *rapid.T) {
		c := bamlfuzz.InvalidJSONCoercionGen().Draw(rt, "invalid_json_case")
		c.Name = "fuzz_" + c.Mutation
		runInvalidOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// invalidSeedFor derives a deterministic uint64 rapid seed for an
// invalid-case (mode, idx) pair. Tests are deterministic across runs
// unless BAMLFUZZ_SEED is set; BAMLFUZZ_SEED XORs into the per-case
// seed so a single env var perturbs the entire matrix at once. Same
// convention as dynamicSeedFor for the valid-case oracle.
func invalidSeedFor(label string, idx int) uint64 {
	h := fnv.New64a()
	h.Write([]byte(label))
	fmt.Fprintf(h, ":%d", idx)
	base := h.Sum64()
	if v := os.Getenv("BAMLFUZZ_SEED"); v != "" {
		base ^= fnv64aInvalidString(v)
	}
	return base
}

func fnv64aInvalidString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// invalidFuzzSeedBytes encodes invalidSeedFor's uint64 output as 8
// little-endian bytes — the exact wire shape bamlfuzz.MakeFuzz
// recognizes as a verbatim seed (fuzzbridge.go).
func invalidFuzzSeedBytes(label string, idx int) []byte {
	seed := invalidSeedFor(label, idx)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, seed)
	return b
}

// buildRapidInvalidDynamicCase synthesizes one Axis B case
// deterministically from seed.
func buildRapidInvalidDynamicCase(t *testing.T, seed uint64, idx int) bamlfuzz.InvalidOracleCase {
	t.Helper()
	c := bamlfuzz.InvalidDynamicSchemaGen().Example(int(seed))
	c.Name = fmt.Sprintf("rapid_%d_%s", idx, c.Mutation)
	c.Seed = int64(seed)
	c.CaseIndex = idx
	return c
}

// buildRapidInvalidJSONCase synthesizes one Axis C case
// deterministically from seed.
func buildRapidInvalidJSONCase(t *testing.T, seed uint64, idx int) bamlfuzz.InvalidOracleCase {
	t.Helper()
	c := bamlfuzz.InvalidJSONCoercionGen().Example(int(seed))
	c.Name = fmt.Sprintf("rapid_%d_%s", idx, c.Mutation)
	c.Seed = int64(seed)
	c.CaseIndex = idx
	return c
}

// invalidArtifactDirFor selects the artifact directory matching c.Mode.
// Keeping the two axes in sibling directories means CI artifact
// collection (the nightly's `_artifacts/` glob) picks up failures from
// both with no workflow change.
func invalidArtifactDirFor(mode bamlfuzz.InvalidOracleMode) string {
	switch mode {
	case bamlfuzz.InvalidDynamicSchema:
		return invalidDynamicArtifactDir
	case bamlfuzz.InvalidJSONCoercion:
		return invalidJSONCoercionArtifactDir
	}
	return invalidDynamicArtifactDir
}

// runInvalidOracleCase executes one InvalidOracleCase end-to-end and
// applies c.ExpectedOutcome. Dumps an envelope artifact on any oracle
// disagreement; success-path artifacts dump only when
// BAMLFUZZ_KEEP_ARTIFACTS=1, matching the valid-case oracle's
// behaviour.
func runInvalidOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.InvalidOracleCase, caseIdx int, source caseSource) {
	t.Helper()

	artifactDir := invalidArtifactDirFor(c.Mode)
	envelope := &bamlfuzz.InvalidFailureEnvelope{
		GeneratorVersion: bamlfuzz.GeneratorVersion,
		RapidSeed:        c.Seed,
		CaseIndex:        caseIdx,
		CaseName:         c.Name,
		OracleMode:       c.Mode,
		Mutation:         c.Mutation,
		ExpectedOutcome:  c.ExpectedOutcome,
		Schema:           c.Schema,
		MockLLMContent:   c.MockLLMContent,
		Metadata:         c.Metadata,
		Reproduction:     reproductionForInvalid(c, caseIdx, source),
	}

	lowered, err := bamlfuzz.LowerInvalidToDynamicSchema(c)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		switch unsupportedActionFor(source) {
		case unsupportedSkip:
			t.Skipf("dynamic emitter skipped schema: %v", err)
			return
		case unsupportedFail:
			envelope.LoweringError = err.Error()
			failAndDumpInvalid(t, artifactDir, envelope, "rapid generator produced unsupported base schema: %v", err)
			return
		}
	}
	if err != nil {
		envelope.LoweringError = err.Error()
		failAndDumpInvalid(t, artifactDir, envelope, "LowerInvalidToDynamicSchema failed: %v", err)
		return
	}
	envelope.DynamicSchema = &lowered

	scenarioID := fmt.Sprintf("bamlfuzz-invalid-%s", scenarioSafe(c.Name))
	envelope.MockLLMScenarioID = scenarioID
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer registerCancel()
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        string(c.MockLLMContent),
		ChunkSize:      0,
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(registerCtx, scenario); err != nil {
		failAndDumpInvalid(t, artifactDir, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID)

	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer callCancel()

	hello := "Return the dynamic fuzz value."
	libReq := dynclient.Request{
		Messages: []dynclient.Message{
			{Role: "system", PartsContent: []dynclient.ContentPart{
				{Type: "text", Text: &hello},
				{Type: "output_format"},
			}},
			{Role: "user", TextContent: &hello},
		},
		ClientRegistry: testutil.DynRegistry(clientReg),
		OutputSchema:   &lowered,
	}

	libResp, libErr := dyn.DynamicCall(callCtx, libReq)
	var dynSuccess bool
	switch {
	case libErr != nil:
		envelope.DynclientError = libErr.Error()
	case libResp == nil:
		envelope.DynclientError = "nil response from dyn.DynamicCall"
		failAndDumpInvalid(t, artifactDir, envelope, "dynclient returned nil response without an error")
		return
	default:
		envelope.DynclientOutput = libResp.Data
		dynSuccess = true
	}

	restBody, err := buildDynamicCallBody(libReq, &lowered)
	if err != nil {
		failAndDumpInvalid(t, artifactDir, envelope, "build REST body: %v", err)
		return
	}
	restResp, err := BAMLClient.DynamicCallJSON(callCtx, restBody)
	var restSuccess bool
	switch {
	case err != nil:
		envelope.RESTError = err.Error()
	case restResp == nil:
		envelope.RESTError = "nil response from BAMLClient.DynamicCallJSON"
		failAndDumpInvalid(t, artifactDir, envelope, "REST client returned nil response without an error")
		return
	default:
		envelope.RESTStatus = restResp.StatusCode
		if restResp.StatusCode >= 400 {
			envelope.RESTError = restResp.Error
		} else {
			envelope.RESTBody = restResp.Body
			restSuccess = true
		}
	}

	envelope.ActualOutcome = formatActualOutcome(dynSuccess, restSuccess)

	switch c.ExpectedOutcome {
	case bamlfuzz.OutcomeBothReject:
		var failures []string
		if dynSuccess {
			failures = append(failures, "dynclient unexpectedly succeeded")
		}
		if restSuccess {
			failures = append(failures, "REST unexpectedly succeeded")
		}
		if len(failures) > 0 {
			failAndDumpInvalid(t, artifactDir, envelope, "axis B both-reject violated (mutation=%s): %s", c.Mutation, strings.Join(failures, "; "))
			return
		}
	case bamlfuzz.OutcomeConditional:
		switch {
		case dynSuccess && restSuccess:
			diffs, derr := bamlfuzz.SemanticDiff("dynclient_vs_rest", envelope.DynclientOutput, envelope.RESTBody)
			if derr != nil {
				failAndDumpInvalid(t, artifactDir, envelope, "axis C diff decode (mutation=%s): %v", c.Mutation, derr)
				return
			}
			if len(diffs) > 0 {
				envelope.SemanticDiff = diffs
				failAndDumpInvalid(t, artifactDir, envelope, "axis C both-success but dynclient ≠ REST body (mutation=%s)", c.Mutation)
				return
			}
		case !dynSuccess && !restSuccess:
			// Both errored: pass — error-text divergence is intentional
			// per the pre-flight finding.
		default:
			failAndDumpInvalid(t, artifactDir, envelope, "axis C succeed-vs-reject parity violated (mutation=%s): %s", c.Mutation, envelope.ActualOutcome)
			return
		}
	default:
		failAndDumpInvalid(t, artifactDir, envelope, "unknown expected outcome %q", c.ExpectedOutcome)
		return
	}

	if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
		if path, err := bamlfuzz.WriteInvalidReplayArtifact(artifactDir, envelope); err == nil {
			t.Logf("kept replay artifact (success): %s", path)
		}
	}
}

// failAndDumpInvalid writes the invalid-case envelope to dir and fails
// the test with a message pointing at the replay path. Parallel to
// failAndDump for the valid-case oracle.
func failAndDumpInvalid(t *testing.T, dir string, envelope *bamlfuzz.InvalidFailureEnvelope, format string, args ...any) {
	t.Helper()
	path, err := bamlfuzz.WriteInvalidReplayArtifact(dir, envelope)
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		t.Errorf("write replay artifact: %v", err)
		t.Errorf("%s", msg)
		return
	}
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// formatActualOutcome describes which surfaces succeeded vs errored,
// in a form the envelope's ActualOutcome field can carry verbatim.
func formatActualOutcome(dynSuccess, restSuccess bool) string {
	dyn := "error"
	if dynSuccess {
		dyn = "success"
	}
	rest := "error"
	if restSuccess {
		rest = "success"
	}
	return fmt.Sprintf("dynclient=%s rest=%s", dyn, rest)
}

// reproductionForInvalid returns the canonical command to re-run a
// failing invalid-case in isolation, embedded in the envelope so a
// developer can copy-paste it from the failure log. Mirrors
// reproductionFor's source dispatch: fuzz cases re-run the engine
// against the persisted -fuzzcachedir; rapid cases re-run a single
// subtest under -run.
func reproductionForInvalid(c bamlfuzz.InvalidOracleCase, caseIdx int, source caseSource) string {
	var testName, fuzzName string
	switch c.Mode {
	case bamlfuzz.InvalidDynamicSchema:
		testName = "TestBamlfuzzInvalidDynamic"
		fuzzName = "FuzzBamlfuzzInvalidDynamic"
	case bamlfuzz.InvalidJSONCoercion:
		testName = "TestBamlfuzzInvalidJSONCoercion"
		fuzzName = "FuzzBamlfuzzInvalidJSONCoercion"
	}
	if source == caseSourceFuzz {
		return "go test -tags=integration,subprocess -run='^$' -fuzz='^" + fuzzName +
			"$' -fuzztime=10m -fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	}
	cmd := fmt.Sprintf("go test -tags=integration -run='^%s$/^rapid$/^case_%d$' ./integration -count=1",
		testName, caseIdx)
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if cases := os.Getenv("BAMLFUZZ_INVALID_CASES"); cases != "" {
		cmd = "BAMLFUZZ_INVALID_CASES=" + cases + " " + cmd
	}
	return cmd
}
