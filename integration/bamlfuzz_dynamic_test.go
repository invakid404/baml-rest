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
	"regexp"
	"sort"
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
// preserve mode. The default of 4 is sized for PR CI wall time; the
// nightly fuzz workflow cranks it up via BAMLFUZZ_DYNAMIC_CASES so
// each scheduled run explores a broader slice of the schema space.
func rapidCasesPerMode() int {
	return envIntDefault("BAMLFUZZ_DYNAMIC_CASES", 4)
}

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
// Preserve-on cases additionally run a schema-aware key-order check
// against the expected JSON on both runtime legs; diagnostics travel
// under OrderWarning per scope D8 and an order mismatch fails the
// subtest. Preserve-off cases skip the order assertion and gate on
// semantic equality only.
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
				runDynamicOracleCase(t, dyn, caseCopy, caseIdx, caseSourceCorpus)
			})
		}
	})

	t.Run("rapid", func(t *testing.T) {
		// Two preserve modes × rapidCasesPerMode each. We materialize
		// each case explicitly so each subtest carries the exact case
		// it ran with into the failure envelope, without leaning on
		// rapid's internal shrinking corpus.
		modes := []bool{true, false}
		for _, preserve := range modes {
			preserve := preserve
			label := "preserve_off"
			if preserve {
				label = "preserve_on"
			}
			t.Run(label, func(t *testing.T) {
				cases := rapidCasesPerMode()
				for i := 0; i < cases; i++ {
					i := i
					seed := dynamicSeedFor(preserve, i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						caseCopy := buildRapidCase(t, seed, preserve, i)
						runDynamicOracleCase(t, dyn, caseCopy, i, caseSourceRapid)
					})
				}
			})
		}
	})
}

// FuzzBamlfuzzDynamic is the testing.F companion to
// TestBamlfuzzDynamicOracle. It exposes the dynamic oracle to Go's
// native fuzz engine via the bamlfuzz.MakeFuzz bridge: testing.F's
// []byte corpus is decoded (8-byte LE) or hashed into a uint64 seed
// (see SeedFromBytes) and fed to a rapid stream so each fuzz input
// drives one CoupledCaseGen draw against the dynamic-safe schema
// generator.
//
// No f.Add seed corpus: Go's test runner replays every f.Add input as
// a subtest under plain `go test` (no `-fuzz=` flag), and the body
// draws schema/value shapes the deterministic rapid oracle does not
// cover, so a hardcoded seed corpus turns PR-time CI red on shapes
// only meaningful under the nightly fuzz pipeline. The fuzz engine
// populates its own corpus under -fuzzcachedir as it explores, and
// the nightly workflow restores that corpus between runs.
//
// Unlike the rapid oracle, this target draws PreserveSchemaOrder from
// the bit stream so the fuzzer explores both halves of the oracle.
// The dynclient and BAMLClient package-level state are reused
// verbatim — re-initialising them per fuzz invocation would explode
// the per-input wall time well beyond what `-fuzztime` can budget.
func FuzzBamlfuzzDynamic(f *testing.F) {
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
			Mode:                bamlfuzz.OracleDynamicThreeWay,
			PreserveSchemaOrder: preserve,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
		runDynamicOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// caseSource identifies where an OracleCase came from: the on-disk
// corpus, the rapid generator, or the testing.F fuzz engine.
// Provenance matters in two places. First, when the dynamic emitter
// rejects a schema with ErrDynamicSchemaUnsupported: corpus cases
// (e.g. the mutual_recursion case gated by
// TODO(upstream-mutual-rec-dynamic-crash)) skip with a clear message,
// but a rapid case hitting the same error is a generator regression —
// DynamicSafeSchemaGen pins MutualCycleProbability=0 and
// AllowSelfRef=false, so the random walker should never produce an
// unsupported schema; the rapid branch fails to surface that drift,
// while corpus cases take the skip branch via unsupportedActionFor.
// Fuzz cases skip on unsupported because the fuzz engine is allowed
// to wander into the same shape the dynamic emitter rejects.
//
// Second, the envelope's Reproduction field depends on the source:
// corpus and rapid emit a `-run` command that selects the specific
// subtest; fuzz emits a `-fuzz` command that re-runs the engine
// against the persisted -fuzzcachedir corpus, where the input bytes
// that triggered the failure live after the workflow restored the
// prior nightly's artifact.
type caseSource int

const (
	caseSourceCorpus caseSource = iota
	caseSourceRapid
	caseSourceFuzz
)

// unsupportedAction is what runDynamicOracleCase should do when the
// dynamic emitter rejects a schema (ErrDynamicSchemaUnsupported). The
// dispatch lives in its own function so a unit test can pin the
// contract without driving the full oracle harness.
type unsupportedAction int

const (
	unsupportedSkip unsupportedAction = iota
	unsupportedFail
)

func unsupportedActionFor(source caseSource) unsupportedAction {
	switch source {
	case caseSourceCorpus, caseSourceFuzz:
		return unsupportedSkip
	default:
		return unsupportedFail
	}
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
// schema + value deterministically from seed. Uses bamlfuzz.CoupledCaseGen
// so the value generator's union-choice metadata + the Move B
// shrink-biased collapse pass land on the case together — the
// integration test then runs the three-way oracle against the (possibly
// collapsed) schema/value pair.
func buildRapidCase(t *testing.T, seed uint64, preserve bool, idx int) bamlfuzz.OracleCase {
	t.Helper()
	cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Example(int(seed))
	return bamlfuzz.OracleCase{
		Name:                fmt.Sprintf("rapid_%t_%d", preserve, idx),
		Seed:                int64(seed),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleDynamicThreeWay,
		PreserveSchemaOrder: preserve,
		Schema:              cc.Schema,
		Value:               cc.Value,
		MockLLMContent:      cc.Walk.MockLLMContent,
		Expected:            cc.Walk.Expected,
		Metadata:            cc.Walk.Metadata,
	}
}

// runDynamicOracleCase performs the three-way comparison for one
// OracleCase, capturing all relevant context into a DynamicFailureEnvelope
// when any leg disagrees semantically. `source` selects how
// ErrDynamicSchemaUnsupported is treated: corpus cases skip, rapid
// cases fail.
func runDynamicOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, caseIdx int, source caseSource) {
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
		Reproduction:        reproductionFor(c, caseIdx, source),
	}

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		switch unsupportedActionFor(source) {
		case unsupportedSkip:
			// The dynamic emitter is correctly rejecting an
			// upstream-unsafe corpus schema (self-ref or mutual
			// cycle). Per scope D8 the failure shape that fails the
			// test is "dynamic emitter unexpectedly ACCEPTS" —
			// rejection is the contract. Skip with a clear reason;
			// the corpus file remains in tree so coverage returns
			// once the upstream BAML limitation is fixed.
			t.Skipf("dynamic emitter skipped schema: %v", err)
			return
		case unsupportedFail:
			// DynamicSafeSchemaGen pins MutualCycleProbability=0 and
			// AllowSelfRef=false, so a rapid-generated case must
			// never land in an unsupported shape. Treat this as a
			// generator regression and fail with the envelope
			// describing the offending schema.
			envelope.DynamicSkipReason = err.Error()
			failAndDump(t, envelope, "rapid generator produced unsupported schema: %v", err)
			return
		}
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
	switch {
	case libErr != nil:
		envelope.DynclientError = libErr.Error()
	case libResp == nil:
		// Defensive contract check: dynclient.Client.DynamicCall today
		// always returns a non-nil response on the nil-error path. A
		// future client regression returning (nil, nil) would otherwise
		// flow through as a successful empty response and silently
		// equate two legs. Surface as a hard failure with the envelope
		// noting which side went nil.
		envelope.DynclientError = "nil response from dyn.DynamicCall"
		failAndDump(t, envelope, "dynclient returned nil response without an error")
		return
	default:
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
	switch {
	case err != nil:
		envelope.RESTError = err.Error()
	case restResp == nil:
		// Defensive contract check: see the dynclient branch above.
		// DynamicCallJSON today always allocates a response on the
		// nil-error path; this guard turns a future regression into a
		// hard failure with the envelope noting the source leg.
		envelope.RESTError = "nil response from BAMLClient.DynamicCallJSON"
		failAndDump(t, envelope, "REST client returned nil response without an error")
		return
	default:
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
		checkSchemaOrder(t, c, envelope, &failures, "expected_vs_dynclient", c.Expected, envelope.DynclientOutput)
	}
	if envelope.RESTError == "" && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_rest", c.Expected, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_rest diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "expected ≠ REST")
		}
		checkSchemaOrder(t, c, envelope, &failures, "expected_vs_rest", c.Expected, envelope.RESTBody)
	}
	if libErr == nil && envelope.RESTError == "" && envelope.DynclientOutput != nil && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiff("dynclient_vs_rest", envelope.DynclientOutput, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("dynclient_vs_rest diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "dynclient ≠ REST")
		}
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

// checkSchemaOrder runs the strict schema-aware key-order assertion on
// one oracle pair when the case opts into PreserveSchemaOrder. A
// non-empty diff records the diagnostic lines on envelope.OrderWarning
// (the field still travels in the replay artifact for forensics) and
// appends a failure reason so the caller's collected `failures` list
// flows through failAndDump. ErrSchemaOrderUnsupported is a hard
// failure: UnionChoices are propagated for every union path the walker
// visits, so a missing or stale choice is an integrity bug.
func checkSchemaOrder(t *testing.T, c bamlfuzz.OracleCase, envelope *bamlfuzz.DynamicFailureEnvelope, failures *[]string, label string, expected, actual json.RawMessage) {
	t.Helper()
	if !c.PreserveSchemaOrder {
		return
	}
	diffs, err := bamlfuzz.SchemaOrderDiffWithChoices(label, c.Schema, expected, actual, c.Metadata.UnionChoices)
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
//
// The Go `-run` flag matches each `/`-separated segment as its own
// regex against the corresponding test-name level. Both subtest trees
// are spelled out here:
//
//	TestBamlfuzzDynamicOracle / corpus / <case_name>
//	TestBamlfuzzDynamicOracle / rapid  / preserve_{on,off} / case_<index>
//
// When the runner is invoked with BAMLFUZZ_SEED set, the env var is
// echoed into the command so the rapid-seeded draw is reproducible.
func reproductionFor(c bamlfuzz.OracleCase, caseIdx int, source caseSource) string {
	if source == caseSourceFuzz {
		// The fuzz engine reaches this case by replaying a corpus
		// entry under -fuzzcachedir. The Schema/Value/MockLLMContent
		// fields of the envelope are the canonical replay material,
		// but pointing the developer at the fuzz engine itself is
		// the fastest path back to the same failure: restore the
		// prior nightly's corpus artifact under .fuzzcache, then run
		// the engine.
		return "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzDynamic$' -fuzztime=10m " +
			"-fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	}
	segments := []string{"^TestBamlfuzzDynamicOracle$"}
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
	if cases := os.Getenv("BAMLFUZZ_DYNAMIC_CASES"); cases != "" {
		cmd = "BAMLFUZZ_DYNAMIC_CASES=" + cases + " " + cmd
	}
	return cmd
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

// TestReproductionFor pins the shape of the reproduction command
// embedded in failure envelopes. The two subtest trees (corpus + rapid)
// have different depths and the BAMLFUZZ_SEED env var prefix is only
// applied when set; this test exercises both.
func TestReproductionFor(t *testing.T) {
	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_DYNAMIC_CASES", "")
	corpus := reproductionFor(
		bamlfuzz.OracleCase{Name: "scalar_string"},
		0,
		caseSourceCorpus,
	)
	wantCorpus := "go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^corpus$/^scalar_string$' ./integration -count=1"
	if corpus != wantCorpus {
		t.Errorf("corpus repro:\n got:  %s\n want: %s", corpus, wantCorpus)
	}

	rapidOn := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: true},
		2,
		caseSourceRapid,
	)
	wantRapidOn := "go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^rapid$/^preserve_on$/^case_2$' ./integration -count=1"
	if rapidOn != wantRapidOn {
		t.Errorf("rapid preserve-on repro:\n got:  %s\n want: %s", rapidOn, wantRapidOn)
	}

	rapidOff := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: false},
		3,
		caseSourceRapid,
	)
	wantRapidOff := "go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^rapid$/^preserve_off$/^case_3$' ./integration -count=1"
	if rapidOff != wantRapidOff {
		t.Errorf("rapid preserve-off repro:\n got:  %s\n want: %s", rapidOff, wantRapidOff)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	withSeed := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: true},
		0,
		caseSourceRapid,
	)
	wantWithSeed := "BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^rapid$/^preserve_on$/^case_0$' ./integration -count=1"
	if withSeed != wantWithSeed {
		t.Errorf("rapid+seed repro:\n got:  %s\n want: %s", withSeed, wantWithSeed)
	}

	// Nightly raises BAMLFUZZ_DYNAMIC_CASES to surface cases beyond
	// the default 4. The repro recipe must propagate that count so a
	// developer rerunning the printed command reaches the failing
	// case index.
	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_DYNAMIC_CASES", "50")
	withCases := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: false},
		25,
		caseSourceRapid,
	)
	wantWithCases := "BAMLFUZZ_DYNAMIC_CASES=50 go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withCases != wantWithCases {
		t.Errorf("rapid+cases repro:\n got:  %s\n want: %s", withCases, wantWithCases)
	}

	// Both env knobs set together. BAMLFUZZ_SEED is prepended first
	// in reproductionFor, then BAMLFUZZ_DYNAMIC_CASES wraps it, so
	// the cases prefix sits leftmost in the rendered command.
	t.Setenv("BAMLFUZZ_SEED", "12345")
	t.Setenv("BAMLFUZZ_DYNAMIC_CASES", "50")
	withSeedAndCases := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: false},
		25,
		caseSourceRapid,
	)
	wantWithSeedAndCases := "BAMLFUZZ_DYNAMIC_CASES=50 BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzDynamicOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withSeedAndCases != wantWithSeedAndCases {
		t.Errorf("rapid+seed+cases repro:\n got:  %s\n want: %s", withSeedAndCases, wantWithSeedAndCases)
	}

	// Fuzz cases bypass the subtest-tree repro: the engine reaches
	// them by replaying a corpus entry under -fuzzcachedir, so the
	// recipe runs the engine itself. The BAMLFUZZ_SEED /
	// BAMLFUZZ_DYNAMIC_CASES env vars do not perturb the fuzz path
	// and must not leak into the command.
	t.Setenv("BAMLFUZZ_SEED", "12345")
	t.Setenv("BAMLFUZZ_DYNAMIC_CASES", "50")
	fuzz := reproductionFor(
		bamlfuzz.OracleCase{PreserveSchemaOrder: true},
		0,
		caseSourceFuzz,
	)
	wantFuzz := "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzDynamic$' -fuzztime=10m -fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	if fuzz != wantFuzz {
		t.Errorf("fuzz repro:\n got:  %s\n want: %s", fuzz, wantFuzz)
	}
}

// TestUnsupportedActionFor pins the source-dependent dispatch for the
// rapid-vs-corpus skip-vs-fail contract. Decoupled from
// runDynamicOracleCase itself because a Go subtest failure cascades to
// the parent — there is no built-in "expect this subtest to fail"
// mechanism, so the dispatch lives in a small pure function whose
// behaviour can be asserted directly.
func TestUnsupportedActionFor(t *testing.T) {
	if got := unsupportedActionFor(caseSourceCorpus); got != unsupportedSkip {
		t.Errorf("caseSourceCorpus: got %v, want unsupportedSkip", got)
	}
	if got := unsupportedActionFor(caseSourceRapid); got != unsupportedFail {
		t.Errorf("caseSourceRapid: got %v, want unsupportedFail", got)
	}
	if got := unsupportedActionFor(caseSourceFuzz); got != unsupportedSkip {
		t.Errorf("caseSourceFuzz: got %v, want unsupportedSkip", got)
	}
}
