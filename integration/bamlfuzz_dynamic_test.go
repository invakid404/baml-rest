//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// dynamicOracleCorpusDir is the in-tree directory holding hand-curated
// dynamic oracle replay artifacts. The bamlfuzz package lives in
// adapters/common; tests in package integration reach it via a relative
// path from the repo root.
const dynamicOracleCorpusDir = "../adapters/common/codegen/testdata/bamlfuzz/dynamic"

// dynamicOracleArtifactDir is where envelope artifacts land on failure
// or with BAMLFUZZ_KEEP_ARTIFACTS=1. Stable relative to the integration
// test working directory so CI can collect from a predictable path.
const dynamicOracleArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/dynamic/_artifacts"

// rapidCasesPerMode controls how many random dynamic cases run per
// preserve mode in PR-B. v1's full target is 20 per mode (40 total);
// PR-B keeps the count small to bound CI wall time until the static
// path lands in PR-C and the full smoke matrix runs.
const rapidCasesPerMode = 4

// TestBamlfuzzDynamicOracle drives the three-way dynamic oracle: for each
// fuzz case we
//
//  1. compute Expected by walking schema+value through bamlfuzz.Walk,
//  2. ask dynclient.Client.DynamicCall for its parsed output,
//  3. ask the REST /call/_dynamic endpoint for its parsed output,
//
// and assert all three agree semantically. mockllm replays the case's
// generated content as the LLM response so the full call path
// (request validation, schema rendering, worker dispatch, response
// flattening) is exercised on both runtime legs.
//
// Order mismatches are recorded under OrderWarning per scope D8 and
// logged but do not fail the test in v1.
func TestBamlfuzzDynamicOracle(t *testing.T) {
	dynclientCallGate(t)

	corpus, err := loadDynamicCorpus(dynamicOracleCorpusDir)
	if err != nil {
		t.Fatalf("load corpus from %s: %v", dynamicOracleCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("dynamic corpus at %s is empty — PR-B must ship hand-curated cases", dynamicOracleCorpusDir)
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
				runDynamicOracleCase(t, dyn, caseCopy, caseIdx)
			})
		}
	})

	t.Run("rapid", func(t *testing.T) {
		// Two preserve modes × rapidCasesPerMode each. We materialize
		// each case explicitly so each subtest carries the exact case
		// it ran with into the failure envelope, instead of relying on
		// rapid's internal shrinking corpus.
		modes := []bool{true, false}
		for _, preserve := range modes {
			preserve := preserve
			label := "preserve_off"
			if preserve {
				label = "preserve_on"
			}
			t.Run(label, func(t *testing.T) {
				for i := 0; i < rapidCasesPerMode; i++ {
					i := i
					seed := dynamicSeedFor(preserve, i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						caseCopy := buildRapidCase(t, seed, preserve, i)
						runDynamicOracleCase(t, dyn, caseCopy, i)
					})
				}
			})
		}
	})
}

// dynamicSeedFor produces a deterministic rapid seed for a (preserve, i)
// pair. Tests are deterministic across runs unless BAMLFUZZ_SEED is set;
// BAMLFUZZ_SEED, when present, XORs into the per-case seed so a single
// env var perturbs the entire matrix at once.
func dynamicSeedFor(preserve bool, i int) uint64 {
	h := fnv.New64a()
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

func fnv64aString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// buildRapidCase synthesizes one OracleCase by drawing a dynamic-safe
// schema + value deterministically from seed. Generator.Example is the
// rapid API for one-shot seeded draws; rapid.Check uses unstable
// internal selection that we don't need for the dynamic three-way oracle.
func buildRapidCase(t *testing.T, seed uint64, preserve bool, idx int) bamlfuzz.OracleCase {
	t.Helper()
	schemaSeed := int(seed)
	schema := bamlfuzz.DynamicSafeSchemaGen().Example(schemaSeed)
	value := bamlfuzz.ValueGen(schema).Example(schemaSeed + 1)
	walk, err := bamlfuzz.Walk(schema, value)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return bamlfuzz.OracleCase{
		Name:                fmt.Sprintf("rapid_%t_%d", preserve, idx),
		Seed:                int64(seed),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleDynamicThreeWay,
		PreserveSchemaOrder: preserve,
		Schema:              schema,
		Value:               value,
		MockLLMContent:      walk.MockLLMContent,
		Expected:            walk.Expected,
		Metadata:            walk.Metadata,
	}
}

// runDynamicOracleCase performs the three-way comparison for one
// OracleCase, capturing all relevant context into a DynamicFailureEnvelope
// when any leg disagrees semantically.
func runDynamicOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, caseIdx int) {
	t.Helper()

	envelope := &bamlfuzz.DynamicFailureEnvelope{
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          bamlfuzz.OracleDynamicThreeWay,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		Schema:              c.Schema,
		MockLLMContent:      c.MockLLMContent,
		Expected:            c.Expected,
		Metadata:            c.Metadata,
		Reproduction:        reproductionFor(c, caseIdx),
	}

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		// The dynamic emitter is correctly rejecting an upstream-unsafe
		// schema (self-ref or mutual cycle). Per scope D8 the failure
		// shape that fails the test is "dynamic emitter unexpectedly
		// ACCEPTS" — rejection is the contract. Skip with a clear
		// reason; the corpus file remains in tree so coverage returns
		// once the upstream BAML limitation is fixed.
		t.Skipf("dynamic emitter skipped schema: %v", err)
		return
	}
	if err != nil {
		envelope.DynamicSkipReason = err.Error()
		failAndDump(t, envelope, "LowerToDynamicSchema failed: %v", err)
		return
	}
	envelope.DynamicSchema = &lowered

	scenarioID := fmt.Sprintf("bamlfuzz-dyn-%s", scenarioSafe(c.Name))
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
		failAndDump(t, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID)

	// dynclient leg
	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer callCancel()

	preserve := c.PreserveSchemaOrder
	preservePtr := &preserve
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
		PreserveSchemaOrder: preservePtr,
	}
	libResp, libErr := dyn.DynamicCall(callCtx, libReq)
	if libErr != nil {
		envelope.DynclientError = libErr.Error()
	} else if libResp != nil {
		envelope.DynclientOutput = libResp.Data
	}

	// REST leg — build body via bamlutils so OrderedMap insertion order
	// travels intact across the wire; testutil.DynamicOutputSchema is
	// map-backed and would scramble property order.
	restBody, err := buildDynamicCallBody(libReq, &lowered)
	if err != nil {
		failAndDump(t, envelope, "build REST body: %v", err)
		return
	}
	restResp, err := BAMLClient.DynamicCallJSON(callCtx, restBody)
	if err != nil {
		envelope.RESTError = err.Error()
	} else if restResp != nil {
		envelope.RESTStatus = restResp.StatusCode
		envelope.RESTBody = restResp.Body
		if restResp.StatusCode >= 400 {
			envelope.RESTError = restResp.Error
		}
	}

	// Three-way semantic comparison.
	var failures []string
	if libErr != nil {
		failures = append(failures, fmt.Sprintf("dynclient errored: %v", libErr))
	}
	if envelope.RESTError != "" {
		failures = append(failures, fmt.Sprintf("REST errored (status %d): %s", envelope.RESTStatus, envelope.RESTError))
	}

	if libErr == nil && envelope.DynclientOutput != nil {
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_dynclient", c.Expected, envelope.DynclientOutput); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_dynclient diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "expected ≠ dynclient")
		}
		envelope.OrderWarning = append(envelope.OrderWarning, bamlfuzz.DetectOrderWarning("expected_vs_dynclient", c.Expected, envelope.DynclientOutput)...)
	}
	if envelope.RESTError == "" && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_rest", c.Expected, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_rest diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "expected ≠ REST")
		}
		envelope.OrderWarning = append(envelope.OrderWarning, bamlfuzz.DetectOrderWarning("expected_vs_rest", c.Expected, envelope.RESTBody)...)
	}
	if libErr == nil && envelope.RESTError == "" && envelope.DynclientOutput != nil && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiff("dynclient_vs_rest", envelope.DynclientOutput, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("dynclient_vs_rest diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "dynclient ≠ REST")
		}
		envelope.OrderWarning = append(envelope.OrderWarning, bamlfuzz.DetectOrderWarning("dynclient_vs_rest", envelope.DynclientOutput, envelope.RESTBody)...)
	}

	if len(envelope.OrderWarning) > 0 {
		t.Logf("order warnings (non-fatal in v1) for %s: %s", c.Name, strings.Join(envelope.OrderWarning, "; "))
	}
	if len(failures) == 0 {
		if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
			if path, err := bamlfuzz.WriteReplayArtifact(dynamicOracleArtifactDir, envelope); err == nil {
				t.Logf("kept replay artifact (success): %s", path)
			}
		}
		return
	}
	failAndDump(t, envelope, "%s", strings.Join(failures, "; "))
}

// failAndDump writes the envelope to the artifact dir and fails the test
// with a message that points at the replay path.
func failAndDump(t *testing.T, envelope *bamlfuzz.DynamicFailureEnvelope, format string, args ...any) {
	t.Helper()
	path, err := bamlfuzz.WriteReplayArtifact(dynamicOracleArtifactDir, envelope)
	if err != nil {
		t.Errorf("write replay artifact: %v", err)
		t.Errorf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// reproductionFor returns the canonical command to re-run a failing case
// in isolation. Embedded in the envelope so a developer can copy-paste
// it from the failure log.
func reproductionFor(c bamlfuzz.OracleCase, caseIdx int) string {
	return fmt.Sprintf("go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/%s$' ./integration -count=1",
		c.Name)
}

// scenarioSafe sanitizes a case name into a mockllm scenario ID. Mockllm
// scenarios are keyed by ID and threaded through OpenAI `model` so the
// characters must stay model-name-clean.
func scenarioSafe(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// buildDynamicCallBody builds the JSON body for a /call/_dynamic POST
// using bamlutils types directly so the OrderedMap entries in `lowered`
// reach the server in declaration order.
func buildDynamicCallBody(req dynclient.Request, lowered *bamlutils.DynamicOutputSchema) ([]byte, error) {
	input := &bamlutils.DynamicInput{
		Messages:            req.Messages,
		ClientRegistry:      req.ClientRegistry,
		OutputSchema:        lowered,
		PreserveSchemaOrder: req.PreserveSchemaOrder,
	}
	return json.Marshal(input)
}

// loadDynamicCorpus reads every `*.json` file under dir and decodes it
// as an OracleCase. Filenames are used as the sort key so subtests run
// in a stable order.
func loadDynamicCorpus(dir string) ([]bamlfuzz.OracleCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]bamlfuzz.OracleCase, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var c bamlfuzz.OracleCase
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if c.Name == "" {
			c.Name = strings.TrimSuffix(name, ".json")
		}
		out = append(out, c)
	}
	return out, nil
}
