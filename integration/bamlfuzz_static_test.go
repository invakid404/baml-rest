//go:build integration

package integration

import (
	"context"
	"encoding/json"
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

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
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
// schema-driven expected JSON. Order mismatches travel under
// OrderWarning per scope D8 and log but do not fail in v1.
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
	if len(corpus) != staticCasesPerBatch {
		t.Fatalf("corpus must have exactly %d cases for one batch, got %d", staticCasesPerBatch, len(corpus))
	}

	t.Run("corpus", func(t *testing.T) {
		runStaticBatch(t, corpus, "corpus", staticCaseSourceCorpus)
	})

	rapidBatches := envIntDefault("BAMLFUZZ_STATIC_BATCHES", 1)
	for b := 0; b < rapidBatches; b++ {
		b := b
		t.Run(fmt.Sprintf("rapid_batch_%d", b), func(t *testing.T) {
			cases := buildStaticRapidBatch(t, b, staticCasesPerBatch)
			runStaticBatch(t, cases, fmt.Sprintf("rapid_batch_%d", b), staticCaseSourceRapid)
		})
	}
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
			envelope := newStaticEnvelope(cases[i], i, batchLabel, source)
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
		isolateStaticBatch(t, lowered, batchLabel, source)
		return
	}
	defer terminateStaticEnv(t, env)

	mockClient := mockllm.NewClient(env.MockLLMURL)
	bamlClient := testutil.NewBAMLRestClient(env.BAMLRestURL)

	for i, lc := range lowered {
		i := i
		lc := lc
		t.Run(lc.Case.Name, func(t *testing.T) {
			runOneStaticCase(t, env, mockClient, bamlClient, lc, i, batchLabel, source)
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
func runOneStaticCase(t *testing.T, env *testutil.TestEnvironment, mockClient *mockllm.Client, bamlClient *testutil.BAMLRestClient, lc loweredStaticCase, caseIdx int, batchLabel string, source staticCaseSource) {
	envelope := newStaticEnvelope(lc.Case, caseIdx, batchLabel, source)
	envelope.BamlSource = lc.Source.Source
	envelope.FunctionName = lc.Source.FunctionName
	envelope.ClassNames = lc.Source.ClassNames
	envelope.EnumNames = lc.Source.EnumNames

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
	envelope.OrderWarning = bamlfuzz.DetectOrderWarning("expected_vs_rest", lc.Case.Expected, resp.Body)
	if len(envelope.OrderWarning) > 0 {
		t.Logf("order warnings (non-fatal in v1) for %s: %s", lc.Case.Name, strings.Join(envelope.OrderWarning, "; "))
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
func isolateStaticBatch(t *testing.T, lowered []loweredStaticCase, batchLabel string, source staticCaseSource) {
	for i, lc := range lowered {
		i := i
		lc := lc
		t.Run(lc.Case.Name+"_isolated", func(t *testing.T) {
			envelope := newStaticEnvelope(lc.Case, i, batchLabel, source)
			envelope.BamlSource = lc.Source.Source
			envelope.FunctionName = lc.Source.FunctionName
			envelope.ClassNames = lc.Source.ClassNames
			envelope.EnumNames = lc.Source.EnumNames

			singleDir := t.TempDir()
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
		})
	}
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
// REST fields as the case progresses.
func newStaticEnvelope(c bamlfuzz.OracleCase, caseIdx int, batchLabel string, source staticCaseSource) *bamlfuzz.StaticFailureEnvelope {
	flags := bamlfuzz.AnalyzeGraph(c.Schema)
	return &bamlfuzz.StaticFailureEnvelope{
		GeneratorVersion: bamlfuzz.GeneratorVersion,
		RapidSeed:        c.Seed,
		CaseIndex:        caseIdx,
		CaseName:         c.Name,
		OracleMode:       bamlfuzz.OracleStaticPrompt,
		Schema:           c.Schema,
		HasSelfRef:       flags.HasSelfRef,
		MockLLMContent:   c.MockLLMContent,
		Expected:         c.Expected,
		Metadata:         c.Metadata,
		Reproduction:     staticReproductionFor(c, caseIdx, batchLabel, source),
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
//	TestBamlfuzzStaticOracle / rapid_batch_<n>  / <case_name>
//
// BAMLFUZZ_SEED is echoed when set so a rapid-seeded draw is
// reproducible.
func staticReproductionFor(c bamlfuzz.OracleCase, caseIdx int, batchLabel string, source staticCaseSource) string {
	segments := []string{"^TestBamlfuzzStaticOracle$"}
	switch source {
	case staticCaseSourceCorpus:
		segments = append(segments, "^corpus$", "^"+regexp.QuoteMeta(c.Name)+"$")
	case staticCaseSourceRapid:
		segments = append(segments,
			"^"+regexp.QuoteMeta(batchLabel)+"$",
			"^"+regexp.QuoteMeta(c.Name)+"$",
		)
	}
	cmd := fmt.Sprintf("go test -tags=integration -run='%s' ./integration -count=1",
		strings.Join(segments, "/"))
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	return cmd
}

// buildStaticRapidBatch draws `n` static-mode cases deterministically
// from a seed derived from batchIdx. Uses bamlfuzz.StaticSchemaGen so
// self-ref and mutual-cycle schemas appear at scope D9's density. Each
// case is materialised into an OracleCase up front so a failure
// envelope can describe the exact shape that triggered the bug.
func buildStaticRapidBatch(t *testing.T, batchIdx int, n int) []bamlfuzz.OracleCase {
	t.Helper()
	cases := make([]bamlfuzz.OracleCase, n)
	for i := 0; i < n; i++ {
		seed := staticSeedFor(batchIdx, i)
		schema := bamlfuzz.StaticSchemaGen().Example(int(seed))
		value := bamlfuzz.ValueGen(schema).Example(int(seed) + 1)
		walk, err := bamlfuzz.Walk(schema, value)
		if err != nil {
			t.Fatalf("walk batch=%d idx=%d: %v", batchIdx, i, err)
		}
		cases[i] = bamlfuzz.OracleCase{
			Name:           fmt.Sprintf("rapid_b%d_c%d", batchIdx, i),
			Seed:           int64(seed),
			CaseIndex:      i,
			Mode:           bamlfuzz.OracleStaticPrompt,
			Schema:         schema,
			Value:          value,
			MockLLMContent: walk.MockLLMContent,
			Expected:       walk.Expected,
			Metadata:       walk.Metadata,
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
// TestReproductionFor coverage for the static subtest tree.
func TestStaticReproductionFor(t *testing.T) {
	t.Setenv("BAMLFUZZ_SEED", "")
	corpus := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "self_referential_tree"},
		4,
		"corpus",
		staticCaseSourceCorpus,
	)
	wantCorpus := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus$/^self_referential_tree$' ./integration -count=1"
	if corpus != wantCorpus {
		t.Errorf("corpus repro:\n got:  %s\n want: %s", corpus, wantCorpus)
	}

	rapid := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "rapid_b0_c2"},
		2,
		"rapid_batch_0",
		staticCaseSourceRapid,
	)
	wantRapid := "go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^rapid_batch_0$/^rapid_b0_c2$' ./integration -count=1"
	if rapid != wantRapid {
		t.Errorf("rapid repro:\n got:  %s\n want: %s", rapid, wantRapid)
	}

	t.Setenv("BAMLFUZZ_SEED", "9999")
	withSeed := staticReproductionFor(
		bamlfuzz.OracleCase{Name: "scalar_object"},
		0,
		"corpus",
		staticCaseSourceCorpus,
	)
	wantWithSeed := "BAMLFUZZ_SEED=9999 go test -tags=integration -run='^TestBamlfuzzStaticOracle$/^corpus$/^scalar_object$' ./integration -count=1"
	if withSeed != wantWithSeed {
		t.Errorf("corpus+seed repro:\n got:  %s\n want: %s", withSeed, wantWithSeed)
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
