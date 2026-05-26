//go:build integration

// Nightly fuzz workflow
//
// The bamlfuzz oracles (this file + bamlfuzz_dynamic_test.go) run on
// every PR with their default case counts (4 dynamic cases per
// preserve mode, 1 static rapid batch). They additionally run nightly
// under .github/workflows/bamlfuzz-nightly.yml with a random seed
// (github.run_id) and the cranked-up knobs BAMLFUZZ_DYNAMIC_CASES=50
// and BAMLFUZZ_STATIC_BATCHES=10 so each scheduled run explores a
// broader slice of the schema space than PR CI can afford.
//
// Failures are tracked on the sticky "bamlfuzz nightly status" issue
// in the GitHub repo. Each failed run appends a comment with the
// seed, the matrix cell (unary-server=true|false), the workflow URL,
// and the envelope artifact name. To triage:
//
//  1. Open the failed run, download the
//     `bamlfuzz-envelopes-<unary>-<run_id>` artifact.
//  2. Locate the failing case's envelope JSON under
//     adapters/common/codegen/testdata/bamlfuzz/{static,dynamic}/_artifacts/.
//  3. Run the repro command from the sticky-issue comment locally with
//     BAML/Docker available (the comment echoes the exact BAMLFUZZ_SEED
//     + BAMLFUZZ_DYNAMIC_CASES + BAMLFUZZ_STATIC_BATCHES the run used).

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// staticOracleCorpusDir is the in-tree directory holding hand-curated
// static oracle replay artifacts. The bamlfuzz package lives in
// adapters/common; tests in package integration reach it via a relative
// path from the repo root.
const staticOracleCorpusDir = "../adapters/common/codegen/testdata/bamlfuzz/static"

// staticOracleArtifactDir is where envelope artifacts land on failure
// or with BAMLFUZZ_KEEP_ARTIFACTS=1.
const staticOracleArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/static/_artifacts"

// integrationBamlSrcDir is the integration testdata baml_src/ that
// every static batch is layered on top of. The generated case files
// merge with the existing clients.baml / types.baml / functions.baml
// / generators.baml so TestClient (the client referenced by every
// generated function) resolves at build time.
const integrationBamlSrcDir = "testdata/baml_src"

// staticCasesPerBatch is the locked batch size from scope D4: exactly
// five generated functions per temporary baml_src/. Build-failure
// isolation reruns the five cases singly when the batched build
// fails; PR-gating coverage rides on this size as the upper bound on
// blast radius per Docker build.
const staticCasesPerBatch = 5

// staticEnvSetupTimeout caps how long any one batch is allowed to
// spend bringing up its dedicated integration environment. Scope D9
// sets 10 minutes inside the 12-minute job timeout; tests get the
// full slice so cold-cache builds can complete on slower runners.
const staticEnvSetupTimeout = 10 * time.Minute

// staticCallTimeout bounds one /call/<FunctionName> request inside a
// batch. Generous so a slow worker doesn't trip a per-case timeout
// before the integration server reports a real failure.
const staticCallTimeout = 60 * time.Second

// TestBamlfuzzStaticOracle drives the static prompt oracle: lower
// each FuzzSchema to .baml source, batch five cases into a temporary
// baml_src/, spin up a dedicated integration environment, register a
// mockllm scenario per case, then exercise /call/<FunctionName>
// against the per-request client_registry override.
//
// The oracle compares the parsed REST response to bamlfuzz.Walk's
// schema-driven expected JSON. Preserve-on cases additionally run a
// schema-aware key-order check; the diagnostic lines travel under
// OrderWarning per scope D8 and an order mismatch fails the subtest.
//
// PR coverage: one corpus batch (the five hand-curated seeds) +
// BAMLFUZZ_STATIC_BATCHES rapid batches (default 1, scope D9). Nightly
// can crank the rapid batch count via the env knob.
func TestBamlfuzzStaticOracle(t *testing.T) {
	dynclientCallGate(t)

	corpus, err := loadStaticCorpus(staticOracleCorpusDir)
	if err != nil {
		t.Fatalf("load corpus from %s: %v", staticOracleCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("static corpus at %s is empty — PR-C must ship hand-curated cases", staticOracleCorpusDir)
	}

	// Chunk the corpus into batches of staticCasesPerBatch so a single
	// build always covers at most five functions. The union expansion
	// of the corpus pushes the count past one batch; each batch keeps
	// the build-failure-isolation contract from D4.
	corpusBatches := chunkStaticCorpus(corpus, staticCasesPerBatch)
	for i, batch := range corpusBatches {
		i := i
		batch := batch
		label := "corpus"
		if len(corpusBatches) > 1 {
			label = fmt.Sprintf("corpus_batch_%d", i)
		}
		t.Run(label, func(t *testing.T) {
			runStaticBatch(t, batch, label, staticCaseSourceCorpus)
		})
	}

	rapidBatches := envIntDefault("BAMLFUZZ_STATIC_BATCHES", 1)
	for b := 0; b < rapidBatches; b++ {
		b := b
		t.Run(fmt.Sprintf("rapid_batch_%d", b), func(t *testing.T) {
			cases := buildStaticRapidBatch(t, b, staticCasesPerBatch)
			runStaticBatch(t, cases, fmt.Sprintf("rapid_batch_%d", b), staticCaseSourceRapid)
		})
	}
}

// FuzzBamlfuzzStatic is the testing.F companion to
// TestBamlfuzzStaticOracle. It exposes the static prompt oracle to
// Go's native fuzz engine via the bamlfuzz.MakeFuzz bridge: testing.F
// supplies a []byte input that's decoded (8-byte LE) or hashed via
// SeedFromBytes into the rapid stream that drives one CoupledCaseGen
// draw.
//
// Each fuzz invocation runs exactly one case through runStaticBatch
// with a singleton batch — the static path's Docker build dominates
// per-invocation wall time, so batching multiple cases per
// invocation would defeat the fuzz engine's input-shrinking.
//
// No f.Add seed corpus: Go's test runner replays every f.Add input
// as a subtest under plain `go test` (no `-fuzz=` flag), and the body
// draws schema/value shapes the deterministic rapid oracle does not
// cover, so a hardcoded seed corpus turns PR-time CI red on shapes
// only meaningful under the nightly fuzz pipeline. The fuzz engine
// populates its own corpus under -fuzzcachedir as it explores, and
// the nightly workflow restores that corpus between runs.
//
// PreserveSchemaOrder is drawn from the bit stream so the fuzzer
// explores both halves of the order-preservation oracle.
func FuzzBamlfuzzStatic(f *testing.F) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		f.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		f.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	bamlfuzz.MakeFuzz(f, func(t *testing.T, rt *rapid.T) {
		preserve := rapid.Bool().Draw(rt, "preserve_schema_order")
		cc := bamlfuzz.CoupledCaseGen(bamlfuzz.StaticSchemaGen()).Draw(rt, "coupled_case")
		c := bamlfuzz.OracleCase{
			Name:                "fuzz",
			Seed:                0,
			CaseIndex:           0,
			Mode:                bamlfuzz.OracleStaticPrompt,
			PreserveSchemaOrder: preserve,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
		runStaticBatch(t, []bamlfuzz.OracleCase{c}, "fuzz", staticCaseSourceRapid)
	})
}

// chunkStaticCorpus splits cases into consecutive batches of at most
// batchSize entries. The last batch may be smaller; an empty input
// returns nil. Used so a corpus that exceeds the locked five-function
// batch ceiling still fits the build-isolation harness.
func chunkStaticCorpus(cases []bamlfuzz.OracleCase, batchSize int) [][]bamlfuzz.OracleCase {
	if len(cases) == 0 {
		return nil
	}
	var out [][]bamlfuzz.OracleCase
	for i := 0; i < len(cases); i += batchSize {
		end := i + batchSize
		if end > len(cases) {
			end = len(cases)
		}
		out = append(out, cases[i:end])
	}
	return out
}

// staticCaseSource mirrors caseSource from the dynamic oracle: corpus
// cases get one set of failure semantics, rapid-generated cases get
// another. v1 treats both identically inside the static oracle (every
// shape must build and call cleanly), but the source label travels
// through into the reproduction command so the developer copy-pastes
// the correct subtest path.
type staticCaseSource int

const (
	staticCaseSourceCorpus staticCaseSource = iota
	staticCaseSourceRapid
)

// runStaticBatch lowers each OracleCase in `cases` to .baml source,
// materialises a temporary baml_src/ that layers the generated case
// files on top of the integration fixtures, then spins up a dedicated
// integration environment. On Setup failure the harness reruns the
// cases singly to isolate the offending source per scope D4. On
// successful Setup it registers a mockllm scenario per case and runs
// the /call/<FunctionName> request, comparing the parsed response to
// the schema-driven expected JSON.
func runStaticBatch(t *testing.T, cases []bamlfuzz.OracleCase, batchLabel string, source staticCaseSource) {
	t.Helper()
	if len(cases) == 0 {
		t.Fatalf("empty batch")
	}
	if len(cases) > staticCasesPerBatch {
		t.Fatalf("batch size %d exceeds locked maximum %d", len(cases), staticCasesPerBatch)
	}

	lowered := make([]loweredStaticCase, len(cases))
	for i := range cases {
		caseID := caseIDFor(batchLabel, i)
		src, err := bamlfuzz.LowerToBamlSource(cases[i].Schema, caseID)
		if err != nil {
			envelope := newStaticEnvelope(cases[i], i, batchLabel, source, false)
			envelope.BuildError = fmt.Sprintf("LowerToBamlSource: %v", err)
			failStaticAndDump(t, envelope, "LowerToBamlSource for case %s: %v", cases[i].Name, err)
			return
		}
		lowered[i] = loweredStaticCase{
			Case:   cases[i],
			Source: src,
			CaseID: caseID,
		}
	}

	// Attempt the batched build first; isolate on failure.
	batchDir := t.TempDir()
	if err := writeBatchBamlSrc(batchDir, lowered); err != nil {
		t.Fatalf("write batch baml_src: %v", err)
	}

	env, err := setupStaticEnv(batchDir)
	if err != nil {
		t.Logf("batch build failed for %s: %v; isolating cases singly", batchLabel, err)
		if !isolateStaticBatch(t, lowered, batchLabel, source) {
			// All isolated rebuilds passed, but the batch build failed.
			// Without this guard the parent subtest would pass and the
			// batch-only regression would land in CI silently. Use
			// t.Errorf so any envelope artifacts already on disk stay
			// readable and the test still tears down cleanly.
			t.Errorf("batch build failed for %s (%v) but every isolated rebuild passed — batch-only failure must not pass silently",
				batchLabel, err)
		}
		return
	}
	defer terminateStaticEnv(t, env)

	mockClient := mockllm.NewClient(env.MockLLMURL)
	bamlClient := testutil.NewBAMLRestClient(env.BAMLRestURL)

	for i, lc := range lowered {
		i := i
		lc := lc
		t.Run(lc.Case.Name, func(t *testing.T) {
			runOneStaticCase(t, env, mockClient, bamlClient, lc, batchDir, i, batchLabel, source)
		})
	}
}

// loweredStaticCase pairs an OracleCase with the static lowering
// output. Kept as a tiny struct so the batch driver and the isolation
// path share the same case shape.
type loweredStaticCase struct {
	Case   bamlfuzz.OracleCase
	Source bamlfuzz.StaticBamlSource
	CaseID string
}

// runOneStaticCase exercises one already-built case: register a
// mockllm scenario, post /call/<FunctionName>, compare the parsed
// response against Expected, and write a StaticFailureEnvelope on any
// disagreement.
func runOneStaticCase(t *testing.T, env *testutil.TestEnvironment, mockClient *mockllm.Client, bamlClient *testutil.BAMLRestClient, lc loweredStaticCase, batchDir string, caseIdx int, batchLabel string, source staticCaseSource) {
	envelope := newStaticEnvelope(lc.Case, caseIdx, batchLabel, source, false)
	envelope.BamlSource = lc.Source.Source
	envelope.FunctionName = lc.Source.FunctionName
	envelope.ClassNames = lc.Source.ClassNames
	envelope.EnumNames = lc.Source.EnumNames
	envelope.BuildSourcePath = caseSourcePath(batchDir, lc.CaseID)

	scenarioID := fmt.Sprintf("bamlfuzz-static-%s", scenarioSafe(lc.CaseID))
	envelope.MockLLMScenarioID = scenarioID
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer registerCancel()
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        string(lc.Case.MockLLMContent),
		ChunkSize:      0,
		InitialDelayMs: 0,
	}
	if err := mockClient.RegisterScenario(registerCtx, scenario); err != nil {
		failStaticAndDump(t, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateTestClient(env.MockLLMInternal, scenarioID)

	callCtx, callCancel := context.WithTimeout(context.Background(), staticCallTimeout)
	defer callCancel()
	resp, err := bamlClient.Call(callCtx, testutil.CallRequest{
		Method:  lc.Source.FunctionName,
		Input:   map[string]any{"input": "Static fuzz call."},
		Options: &testutil.BAMLOptions{ClientRegistry: clientReg},
	})
	switch {
	case err != nil:
		envelope.RESTError = err.Error()
		failStaticAndDump(t, envelope, "/call/%s: %v", lc.Source.FunctionName, err)
		return
	case resp == nil:
		envelope.RESTError = "nil response from bamlClient.Call"
		failStaticAndDump(t, envelope, "/call/%s returned nil response without an error", lc.Source.FunctionName)
		return
	}
	envelope.RESTStatus = resp.StatusCode
	if resp.StatusCode >= 400 {
		envelope.RESTError = resp.Error
		failStaticAndDump(t, envelope, "/call/%s status %d: %s", lc.Source.FunctionName, resp.StatusCode, resp.Error)
		return
	}
	envelope.RESTBody = resp.Body

	if diff, err := bamlfuzz.SemanticDiff("expected_vs_rest", lc.Case.Expected, resp.Body); err != nil {
		envelope.RESTError = err.Error()
		failStaticAndDump(t, envelope, "expected_vs_rest diff: %v", err)
		return
	} else if len(diff) > 0 {
		envelope.SemanticDiff = diff
		failStaticAndDump(t, envelope, "expected ≠ /call/%s", lc.Source.FunctionName)
		return
	}
	if lc.Case.PreserveSchemaOrder {
		diffs, err := bamlfuzz.SchemaOrderDiffWithChoices("expected_vs_rest", lc.Case.Schema, lc.Case.Expected, resp.Body, lc.Case.Metadata.UnionChoices)
		switch {
		case errors.Is(err, bamlfuzz.ErrSchemaOrderUnsupported):
			envelope.OrderWarning = append(envelope.OrderWarning, err.Error())
			failStaticAndDump(t, envelope, "schema order check unsupported for %s: %v", lc.Case.Name, err)
			return
		case err != nil:
			envelope.OrderWarning = append(envelope.OrderWarning, err.Error())
			failStaticAndDump(t, envelope, "schema order check failed for %s: %v", lc.Case.Name, err)
			return
		case len(diffs) > 0:
			envelope.OrderWarning = append(envelope.OrderWarning, bamlfuzz.FormatSchemaOrderDiffs(diffs)...)
			failStaticAndDump(t, envelope, "schema key order mismatch for /call/%s", lc.Source.FunctionName)
			return
		}
	}
	if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
		if path, err := bamlfuzz.WriteStaticReplayArtifact(staticOracleArtifactDir, envelope); err == nil {
			t.Logf("kept replay artifact (success): %s", path)
		}
	}
}

// isolateStaticBatch reruns each lowered case in its own baml_src/ so
// the failing one can be named in the failure envelope. Scope D4
// requires this whenever a five-function batch build fails — the
// batched error message rarely identifies which case carries the bad
// source.
//
// Returns whether any isolated rebuild failed. The caller treats a
// false return as a batch-only regression and fails the parent
// subtest explicitly: a Go `t.Run` only propagates failures from its
// own body, so without that explicit signal a transient batched
// build failure would pass silently when the isolated reruns happen
// to succeed.
func isolateStaticBatch(t *testing.T, lowered []loweredStaticCase, batchLabel string, source staticCaseSource) (anyFailed bool) {
	for i, lc := range lowered {
		i := i
		lc := lc
		if !t.Run(lc.Case.Name+"_isolated", func(t *testing.T) {
			runIsolatedStaticCase(t, lc, i, batchLabel, source)
		}) {
			anyFailed = true
		}
	}
	return anyFailed
}

// runIsolatedStaticCase is the per-case body of isolateStaticBatch.
// Extracted so the t.Run aggregation pattern stays readable and so
// the body's envelope wiring can evolve without rewriting the loop.
// The isolated flag on newStaticEnvelope is what routes the
// reproduction command at the `<case>_isolated` subtest leaf.
func runIsolatedStaticCase(t *testing.T, lc loweredStaticCase, caseIdx int, batchLabel string, source staticCaseSource) {
	envelope := newStaticEnvelope(lc.Case, caseIdx, batchLabel, source, true)
	envelope.BamlSource = lc.Source.Source
	envelope.FunctionName = lc.Source.FunctionName
	envelope.ClassNames = lc.Source.ClassNames
	envelope.EnumNames = lc.Source.EnumNames

	singleDir := t.TempDir()
	envelope.BuildSourcePath = caseSourcePath(singleDir, lc.CaseID)
	if err := writeBatchBamlSrc(singleDir, []loweredStaticCase{lc}); err != nil {
		envelope.BuildError = fmt.Sprintf("write isolated baml_src: %v", err)
		failStaticAndDump(t, envelope, "isolated write: %v", err)
		return
	}
	env, err := setupStaticEnv(singleDir)
	if err != nil {
		envelope.BuildError = err.Error()
		failStaticAndDump(t, envelope, "isolated build failed: %v", err)
		return
	}
	terminateStaticEnv(t, env)
}

// caseSourcePath returns the absolute path to the generated .baml
// file inside `bamlSrcDir` for the given caseID. Pinned to the
// `bamlfuzz_<CaseID>.baml` naming used by writeBatchBamlSrc so the
// envelope's BuildSourcePath always identifies a real file on disk.
func caseSourcePath(bamlSrcDir, caseID string) string {
	return filepath.Join(bamlSrcDir, fmt.Sprintf("bamlfuzz_%s.baml", caseID))
}

// writeBatchBamlSrc materialises a baml_src/ at `dir` that is the
// union of the integration testdata fixtures and one .baml file per
// lowered case. The generated case files are named
// `bamlfuzz_<CaseID>.baml` so the failure envelope's BuildSourcePath
// + on-disk filename uniquely identify each case during diagnosis.
func writeBatchBamlSrc(dir string, lowered []loweredStaticCase) error {
	srcRoot, err := absIntegrationBamlSrc()
	if err != nil {
		return err
	}
	if err := copyDir(srcRoot, dir); err != nil {
		return fmt.Errorf("copy integration baml_src: %w", err)
	}
	for _, lc := range lowered {
		fileName := fmt.Sprintf("bamlfuzz_%s.baml", lc.CaseID)
		out := filepath.Join(dir, fileName)
		if err := os.WriteFile(out, []byte(lc.Source.Source), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
	}
	return nil
}

// absIntegrationBamlSrc returns the absolute path to the integration
// fixtures' baml_src directory. Resolved relative to the test working
// directory (the package directory), which is where `go test` starts
// before TestMain runs cwd-dependent helpers.
func absIntegrationBamlSrc() (string, error) {
	abs, err := filepath.Abs(integrationBamlSrcDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("integration baml_src not found at %s: %w", abs, err)
	}
	return abs, nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// setupStaticEnv spins up a dedicated integration environment with
// the supplied baml_src/. Matrix axes from TestMain (BAML version,
// build-request gate, in-process vs subprocess) inherit through
// matrixSetupOptions so the static oracle runs under the same
// runtime shape as the rest of the integration suite.
func setupStaticEnv(bamlSrcDir string) (*testutil.TestEnvironment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), staticEnvSetupTimeout)
	defer cancel()
	opts := matrixSetupOptions()
	opts.BAMLSrcPath = bamlSrcDir
	return testutil.Setup(ctx, opts)
}

func terminateStaticEnv(t *testing.T, env *testutil.TestEnvironment) {
	t.Helper()
	if env == nil {
		return
	}
	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer termCancel()
	if err := env.Terminate(termCtx); err != nil {
		t.Logf("static env Terminate: %v", err)
	}
}

// newStaticEnvelope returns a partially-populated envelope with the
// common header fields stamped. Callers fill in lowering / build /
// REST fields as the case progresses. `isolated` selects the
// reproduction-command shape: the isolated rebuild path runs as a
// `<case>_isolated` subtest so the developer's copy-paste needs the
// correct leaf segment.
func newStaticEnvelope(c bamlfuzz.OracleCase, caseIdx int, batchLabel string, source staticCaseSource, isolated bool) *bamlfuzz.StaticFailureEnvelope {
	flags := bamlfuzz.AnalyzeGraph(c.Schema)
	return &bamlfuzz.StaticFailureEnvelope{
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          bamlfuzz.OracleStaticPrompt,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		Schema:              c.Schema,
		HasSelfRef:          flags.HasSelfRef,
		MockLLMContent:      c.MockLLMContent,
		Expected:            c.Expected,
		Metadata:            c.Metadata,
		Reproduction:        staticReproductionFor(c, caseIdx, batchLabel, source, isolated),
	}
}

// failStaticAndDump writes the envelope to the artifact directory and
// fails the test with a message that points at the replay path.
func failStaticAndDump(t *testing.T, envelope *bamlfuzz.StaticFailureEnvelope, format string, args ...any) {
	t.Helper()
	path, err := bamlfuzz.WriteStaticReplayArtifact(staticOracleArtifactDir, envelope)
	if err != nil {
		t.Errorf("write static replay artifact: %v", err)
		t.Errorf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// caseIDFor returns the per-case BAML identifier suffix for a slot in
// a batch. The label encodes batch provenance (corpus vs
// rapid_batch_N) so the mangled symbol names hint at the source when
// staring at a build error.
func caseIDFor(batchLabel string, idx int) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, batchLabel)
	if clean == "" {
		clean = "batch"
	}
	if !(clean[0] >= 'A' && clean[0] <= 'Z') && !(clean[0] >= 'a' && clean[0] <= 'z') {
		clean = "B" + clean
	}
	return fmt.Sprintf("%s_C%02d", clean, idx)
}

// staticReproductionFor returns the canonical `go test` command to
// rerun one failing static case. The reproduction path mirrors the
// subtest tree this file builds (corpus / rapid_batch_N) so the
// developer can copy-paste from the failure log.
//
//	TestBamlfuzzStaticOracle / corpus           / <case_name>
//	TestBamlfuzzStaticOracle / corpus           / <case_name>_isolated
//	TestBamlfuzzStaticOracle / rapid_batch_<n>  / <case_name>
//	TestBamlfuzzStaticOracle / rapid_batch_<n>  / <case_name>_isolated
//
// `isolated` flips the leaf segment to the `_isolated` subtest name
// that isolateStaticBatch creates, so an isolated build failure's
// envelope command actually selects the failing subtest.
//
// BAMLFUZZ_SEED is echoed when set so a rapid-seeded draw is
// reproducible.
func staticReproductionFor(c bamlfuzz.OracleCase, caseIdx int, batchLabel string, source staticCaseSource, isolated bool) string {
	leaf := c.Name
	if isolated {
		leaf = c.Name + "_isolated"
	}
	segments := []string{"^TestBamlfuzzStaticOracle$"}
	switch source {
	case staticCaseSourceCorpus, staticCaseSourceRapid:
		// Both sources use the parent's actual batch label. The
		// corpus path used to be hardcoded to "^corpus$", but
		// chunkStaticCorpus emits "corpus_batch_<i>" subtest names
		// when there is more than one batch, so the repro command
		// would not select the failing case for any non-first
		// corpus batch. Routing the label through here keeps the
		// repro string aligned with the subtest tree the harness
		// actually builds.
		segments = append(segments,
			"^"+regexp.QuoteMeta(batchLabel)+"$",
			"^"+regexp.QuoteMeta(leaf)+"$",
		)
	}
	cmd := fmt.Sprintf("go test -tags=integration -run='%s' ./integration -count=1",
		strings.Join(segments, "/"))
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if batches := os.Getenv("BAMLFUZZ_STATIC_BATCHES"); batches != "" {
		cmd = "BAMLFUZZ_STATIC_BATCHES=" + batches + " " + cmd
	}
	return cmd
}

// buildStaticRapidBatch draws `n` static-mode cases deterministically
// from a seed derived from batchIdx. Uses bamlfuzz.CoupledCaseGen so
// the union-aware Move B shrink-biased collapse pass and the value
// generator's choice metadata land on the case together. Each case
// is materialised into an OracleCase up front so a failure envelope
// can describe the exact shape that triggered the bug.
func buildStaticRapidBatch(t *testing.T, batchIdx int, n int) []bamlfuzz.OracleCase {
	t.Helper()
	cases := make([]bamlfuzz.OracleCase, n)
	for i := 0; i < n; i++ {
		seed := staticSeedFor(batchIdx, i)
		cc := bamlfuzz.CoupledCaseGen(bamlfuzz.StaticSchemaGen()).Example(int(seed))
		cases[i] = bamlfuzz.OracleCase{
			Name:                fmt.Sprintf("rapid_b%d_c%d", batchIdx, i),
			Seed:                int64(seed),
			CaseIndex:           i,
			Mode:                bamlfuzz.OracleStaticPrompt,
			PreserveSchemaOrder: i%2 == 0,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
	}
	return cases
}

func staticSeedFor(batchIdx, caseIdx int) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "static_b%d_c%d", batchIdx, caseIdx)
	base := h.Sum64()
	if v := os.Getenv("BAMLFUZZ_SEED"); v != "" {
		base ^= fnv64aStaticString(v)
	}
	return base
}

func fnv64aStaticString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// envIntDefault reads `key` from the environment, parses it as an int,
// and returns `def` on missing/invalid. Used for the
// BAMLFUZZ_STATIC_BATCHES knob.
func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// loadStaticCorpus reads every `*.json` file under dir and decodes it
// as an OracleCase. Filenames sort lexicographically so subtests run
// in a stable order.
func loadStaticCorpus(dir string) ([]bamlfuzz.OracleCase, error) {
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

// TestStaticReproductionFor pins the shape of the reproduction command
// embedded in static failure envelopes. Mirrors the dynamic oracle's
// TestReproductionFor coverage for the static subtest tree, plus the
// `_isolated` leaf shape isolateStaticBatch emits.
func TestStaticReproductionFor(t *testing.T) {
	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_STATIC_BATCHES", "")
	corpus := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "self_referential_tree"},
		4,
		"corpus",
		staticCaseSourceCorpus,
		false,
	)
	wantCorpus := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus$/^self_referential_tree$' ./integration -count=1"
	if corpus != wantCorpus {
		t.Errorf("corpus repro:\n got:  %s\n want: %s", corpus, wantCorpus)
	}

	corpusIsolated := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "self_referential_tree"},
		4,
		"corpus",
		staticCaseSourceCorpus,
		true,
	)
	wantCorpusIsolated := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus$/^self_referential_tree_isolated$' ./integration -count=1"
	if corpusIsolated != wantCorpusIsolated {
		t.Errorf("corpus isolated repro:\n got:  %s\n want: %s", corpusIsolated, wantCorpusIsolated)
	}

	rapid := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "rapid_b0_c2"},
		2,
		"rapid_batch_0",
		staticCaseSourceRapid,
		false,
	)
	wantRapid := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^rapid_batch_0$/^rapid_b0_c2$' ./integration -count=1"
	if rapid != wantRapid {
		t.Errorf("rapid repro:\n got:  %s\n want: %s", rapid, wantRapid)
	}

	rapidIsolated := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "rapid_b0_c2"},
		2,
		"rapid_batch_0",
		staticCaseSourceRapid,
		true,
	)
	wantRapidIsolated := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^rapid_batch_0$/^rapid_b0_c2_isolated$' ./integration -count=1"
	if rapidIsolated != wantRapidIsolated {
		t.Errorf("rapid isolated repro:\n got:  %s\n want: %s", rapidIsolated, wantRapidIsolated)
	}

	t.Setenv("BAMLFUZZ_SEED", "9999")
	withSeed := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "scalar_object"},
		0,
		"corpus",
		staticCaseSourceCorpus,
		false,
	)
	wantWithSeed := "BAMLFUZZ_SEED=9999 go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus$/^scalar_object$' ./integration -count=1"
	if withSeed != wantWithSeed {
		t.Errorf("corpus+seed repro:\n got:  %s\n want: %s", withSeed, wantWithSeed)
	}

	// Corpus chunking with more than one batch emits
	// "corpus_batch_<i>" subtest names. The repro command must use
	// that exact label, including the isolated leaf shape, so the
	// developer can copy-paste straight from the envelope.
	t.Setenv("BAMLFUZZ_SEED", "")
	corpusBatch := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "raw_union_root"},
		1,
		"corpus_batch_3",
		staticCaseSourceCorpus,
		false,
	)
	wantCorpusBatch := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus_batch_3$/^raw_union_root$' ./integration -count=1"
	if corpusBatch != wantCorpusBatch {
		t.Errorf("corpus_batch repro:\n got:  %s\n want: %s", corpusBatch, wantCorpusBatch)
	}
	corpusBatchIsolated := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "raw_union_root"},
		1,
		"corpus_batch_3",
		staticCaseSourceCorpus,
		true,
	)
	wantCorpusBatchIsolated := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus_batch_3$/^raw_union_root_isolated$' ./integration -count=1"
	if corpusBatchIsolated != wantCorpusBatchIsolated {
		t.Errorf("corpus_batch isolated repro:\n got:  %s\n want: %s", corpusBatchIsolated, wantCorpusBatchIsolated)
	}

	// Nightly raises BAMLFUZZ_STATIC_BATCHES so rapid_batch_<i>
	// beyond batch 0 actually exists in the repro. Verify the
	// prefix propagates.
	t.Setenv("BAMLFUZZ_STATIC_BATCHES", "10")
	withBatches := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "rapid_b7_c2"},
		2,
		"rapid_batch_7",
		staticCaseSourceRapid,
		false,
	)
	wantWithBatches := "BAMLFUZZ_STATIC_BATCHES=10 go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^rapid_batch_7$/^rapid_b7_c2$' ./integration -count=1"
	if withBatches != wantWithBatches {
		t.Errorf("rapid+batches repro:\n got:  %s\n want: %s", withBatches, wantWithBatches)
	}

	// Both env knobs set together. BAMLFUZZ_SEED is prepended first
	// in staticReproductionFor, then BAMLFUZZ_STATIC_BATCHES wraps
	// it, so the batches prefix sits leftmost in the rendered
	// command.
	t.Setenv("BAMLFUZZ_SEED", "9999")
	t.Setenv("BAMLFUZZ_STATIC_BATCHES", "10")
	withSeedAndBatches := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "rapid_b7_c2"},
		2,
		"rapid_batch_7",
		staticCaseSourceRapid,
		false,
	)
	wantWithSeedAndBatches := "BAMLFUZZ_STATIC_BATCHES=10 BAMLFUZZ_SEED=9999 go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^rapid_batch_7$/^rapid_b7_c2$' ./integration -count=1"
	if withSeedAndBatches != wantWithSeedAndBatches {
		t.Errorf("rapid+seed+batches repro:\n got:  %s\n want: %s", withSeedAndBatches, wantWithSeedAndBatches)
	}
}

// TestSubtestAggregationContract pins the Go testing-API contract
// isolateStaticBatch relies on for F1's batch-mask guard: a t.Run
// invocation returns the pass/fail state of its body, so the parent
// can aggregate failures across iterations and detect the case where
// every isolated rebuild passed but the batched build still failed.
//
// Only the green path (no subtest failures => aggregator stays false)
// is asserted here — deliberately failing a subtest from within this
// test would propagate up to TestSubtestAggregationContract itself,
// which there's no built-in way to suppress. The red path is locked
// by the Go testing package's contract that t.Run returns false on
// any t.Errorf inside the body.
func TestSubtestAggregationContract(t *testing.T) {
	var anyFailed bool
	if !t.Run("ok_a", func(t *testing.T) {}) {
		anyFailed = true
	}
	if !t.Run("ok_b", func(t *testing.T) {}) {
		anyFailed = true
	}
	if anyFailed {
		t.Errorf("expected anyFailed=false when no subtest body failed, got true")
	}
}

// TestCaseSourcePath pins the on-disk filename writeBatchBamlSrc uses
// for each generated case. The envelope's BuildSourcePath assignment
// targets exactly this path, so a drift between the two breaks the
// developer's path-from-envelope-to-source lookup.
func TestCaseSourcePath(t *testing.T) {
	got := caseSourcePath("/tmp/bamlsrc", "corpus_C03")
	want := filepath.Join("/tmp/bamlsrc", "bamlfuzz_corpus_C03.baml")
	if got != want {
		t.Errorf("caseSourcePath: got %q want %q", got, want)
	}
}

// TestCaseIDFor pins the BAML-identifier-safe transform applied to
// batch labels. The lowering rejects unsafe caseIDs at the
// LowerToBamlSource boundary; the test harness is the producer that
// must guarantee a valid suffix even when batch labels carry
// separators or numeric prefixes.
func TestCaseIDFor(t *testing.T) {
	cases := []struct {
		label string
		idx   int
		want  string
	}{
		{"corpus", 0, "corpus_C00"},
		{"rapid_batch_0", 3, "rapid_batch_0_C03"},
		{"rapid-batch-0", 1, "rapid_batch_0_C01"},
		{"0bad", 2, "B0bad_C02"},
		{"", 4, "batch_C04"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := caseIDFor(c.label, c.idx)
			if got != c.want {
				t.Errorf("caseIDFor(%q, %d) = %q, want %q", c.label, c.idx, got, c.want)
			}
			if _, err := bamlfuzz.LowerToBamlSource(bamlfuzz.FuzzSchema{
				Classes: []bamlfuzz.FuzzClass{{Name: "Root", Properties: []bamlfuzz.FuzzProperty{
					{Name: "x", Type: bamlfuzz.FuzzType{Kind: bamlfuzz.KindString}},
				}}},
				RootClass: "Root",
			}, got); err != nil {
				t.Errorf("caseID %q rejected by LowerToBamlSource: %v", got, err)
			}
		})
	}
}
