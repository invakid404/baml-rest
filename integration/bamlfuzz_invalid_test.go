//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
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
//
// The rapid loop iterates both preserve_schema_order modes so the Axis
// C oracle exercises the ordered-fields code path under
// preserve_schema_order=true. The order assertion (success quadrant
// only) compares dynclient vs REST key order at every class node via
// SchemaOrderDiffWithChoices — the same machinery the valid-case
// dynamic oracle uses.
func TestBamlfuzzInvalidJSONCoercion(t *testing.T) {
	dynclientCallGate(t)

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}

	t.Run("rapid", func(t *testing.T) {
		cases := invalidCasesPerRun()
		modes := []bool{true, false}
		for _, preserve := range modes {
			preserve := preserve
			label := "preserve_off"
			if preserve {
				label = "preserve_on"
			}
			t.Run(label, func(t *testing.T) {
				for i := 0; i < cases; i++ {
					i := i
					seed := invalidSeedFor(fmt.Sprintf("invalid_json_coercion:%s", label), i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						c := buildRapidInvalidJSONCase(t, seed, i, preserve)
						runInvalidOracleCase(t, dyn, c, i, caseSourceRapid)
					})
				}
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
		preserve := rapid.Bool().Draw(rt, "preserve_schema_order")
		c := bamlfuzz.InvalidJSONCoercionGen().Draw(rt, "invalid_json_case")
		c.PreserveSchemaOrder = preserve
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
		// Reuse fnv64aString from bamlfuzz_dynamic_test.go (same
		// package, same build tag). The static-test variant
		// fnv64aStaticString is a deliberate duplicate left in
		// place — same cleanup opportunity, but in an out-of-scope
		// file.
		base ^= fnv64aString(v)
	}
	return base
}

// invalidFuzzSeedBytes derives a deterministic 8-byte fuzz-corpus seed
// from (label, idx). The output is the exact wire shape
// bamlfuzz.MakeFuzz recognizes as a verbatim uint64 (fuzzbridge.go).
//
// BAMLFUZZ_SEED is intentionally NOT folded in here, even though the
// rapid path's invalidSeedFor does fold it in. Reason: the nightly's
// fuzz mode runs with BAMLFUZZ_SEED=github.run_id set in the job env,
// and the fuzz-mode repro command emitted on failure does not echo
// BAMLFUZZ_SEED back (only the corpus bytes under -fuzzcachedir
// identify the failing input). Folding the env var into the corpus
// seed bytes would make every nightly's seed bytes different from the
// developer-machine bytes under the same f.Add(label, idx) call, so a
// failed run's "go test -fuzz=..." command would replay a different
// case than the one that triggered the failure. Keeping the corpus
// bytes pure ensures f.Add(label, 0) is byte-identical across all
// environments.
func invalidFuzzSeedBytes(label string, idx int) []byte {
	h := fnv.New64a()
	h.Write([]byte(label))
	fmt.Fprintf(h, ":%d", idx)
	seed := h.Sum64()
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
// deterministically from seed. PreserveSchemaOrder is set explicitly
// by the caller's preserve-mode iterator and is encoded into the case
// name so per-mode artifact paths don't collide.
func buildRapidInvalidJSONCase(t *testing.T, seed uint64, idx int, preserve bool) bamlfuzz.InvalidOracleCase {
	t.Helper()
	c := bamlfuzz.InvalidJSONCoercionGen().Example(int(seed))
	mode := "preserve_off"
	if preserve {
		mode = "preserve_on"
	}
	c.Name = fmt.Sprintf("rapid_%s_%d_%s", mode, idx, c.Mutation)
	c.Seed = int64(seed)
	c.CaseIndex = idx
	c.PreserveSchemaOrder = preserve
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
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          c.Mode,
		Mutation:            c.Mutation,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		ExpectedOutcome:     c.ExpectedOutcome,
		Schema:              c.Schema,
		MockLLMContent:      c.MockLLMContent,
		Metadata:            c.Metadata,
		Reproduction:        reproductionForInvalid(c, caseIdx, source),
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

	// Independent contexts per surface. Sharing one context across
	// dynclient and REST lets a dynclient hang / cancellation drain
	// the deadline before REST runs, so REST returns context.Canceled
	// without ever hitting the server. The oracle would then see
	// "both error" and pass the case — masking a one-sided failure.
	// A common parentCtx still allows test-level cancellation to abort
	// both surfaces together; per-surface child contexts isolate the
	// 30s budget so each call gets its own clock.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	hello := "Return the dynamic fuzz value."
	preserve := c.PreserveSchemaOrder
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

	dynCtx, dynCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer dynCancel()
	var (
		libResp *dynclient.CallResult
		libErr  error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		libResp, libErr = dyn.DynamicCall(dynCtx, libReq)
	})
	if panicked {
		envelope.DynclientPanic = fmt.Sprintf("%v", panicVal)
		envelope.DynclientPanicStack = string(panicStack)
		failAndDumpInvalid(t, artifactDir, envelope, "dyn.DynamicCall panicked: %v\n%s", panicVal, panicStack)
		return
	}
	if libErr != nil && (errors.Is(libErr, context.Canceled) || errors.Is(libErr, context.DeadlineExceeded)) {
		t.Fatalf("harness failure: dynclient context (case=%s mutation=%s): %v", c.Name, c.Mutation, libErr)
	}
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
	restCtx, restCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer restCancel()
	var restResp *testutil.DynamicCallResponse
	panicked, panicVal, panicStack = callWithRecover(func() {
		restResp, err = BAMLClient.DynamicCallJSON(restCtx, restBody)
	})
	if panicked {
		envelope.RESTPanic = fmt.Sprintf("%v", panicVal)
		envelope.RESTPanicStack = string(panicStack)
		failAndDumpInvalid(t, artifactDir, envelope, "BAMLClient.DynamicCallJSON panicked: %v\n%s", panicVal, panicStack)
		return
	}
	if err != nil {
		t.Fatalf("harness failure: REST transport (case=%s mutation=%s): %v", c.Name, c.Mutation, err)
	}
	if restResp == nil {
		envelope.RESTError = "nil response from BAMLClient.DynamicCallJSON"
		failAndDumpInvalid(t, artifactDir, envelope, "REST client returned nil response without an error")
		return
	}
	var restSuccess bool
	envelope.RESTStatus = restResp.StatusCode
	if restResp.StatusCode >= 400 {
		envelope.RESTError = restResp.Error
	} else {
		envelope.RESTBody = restResp.Body
		restSuccess = true
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
			diffs, derr := bamlfuzz.SemanticDiffParity("dynclient_vs_rest", envelope.DynclientOutput, envelope.RESTBody)
			if derr != nil {
				failAndDumpInvalid(t, artifactDir, envelope, "axis C diff decode (mutation=%s): %v", c.Mutation, derr)
				return
			}
			if len(diffs) > 0 {
				envelope.SemanticDiff = diffs
				failAndDumpInvalid(t, artifactDir, envelope, "axis C both-success but dynclient ≠ REST body (mutation=%s)", c.Mutation)
				return
			}
			if c.PreserveSchemaOrder {
				if msg, fail := checkInvalidOrderC(c, envelope.DynclientOutput, envelope.RESTBody); msg != "" {
					envelope.OrderWarning = append(envelope.OrderWarning, msg)
					if fail {
						failAndDumpInvalid(t, artifactDir, envelope, "axis C dynclient ≠ REST key order (mutation=%s)", c.Mutation)
						return
					}
				}
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

// checkInvalidOrderC runs the schema-aware key-order assertion on the
// (dynclient.Data, REST.Body) pair for an Axis C case in the success
// quadrant. Returns (diagnostic, hardFail) — a non-empty diagnostic
// always indicates the check found something worth reporting; the
// second return reports whether the call site should fail the test.
//
// The order walker's UnionChoices contract requires a recorded choice
// for every union path it visits. The walker built CaseMetadata.UnionChoices
// from the pre-mutation FuzzValue, but the mock LLM JSON has been
// perturbed since: a mutation can flip the surfaces into a union arm
// the pre-mutation walk never recorded, leaving the order walker no
// way to dispatch at that node. To keep the order assertion runnable
// in that case, derive a fresh choices map from the actual parsed
// dynclient output (which SemanticDiff already proved equals the REST
// body at this point), then feed THAT into the walker. Derivation
// covers every union path in the schema reachable through the parsed
// JSON, so the walker should always have a choice and never bail.
//
// ErrSchemaOrderUnsupported after derivation is a hard fail: it means
// either (a) the parsed value at a union path has a shape no arm
// matches (a real schema-vs-output divergence the oracle should
// surface), or (b) the derivation itself is buggy. Both are worth
// failing on.
func checkInvalidOrderC(c bamlfuzz.InvalidOracleCase, dyn, rest json.RawMessage) (string, bool) {
	choices, err := deriveUnionChoicesFromParsed(c.Schema, dyn)
	if err != nil {
		return fmt.Sprintf("axis C order check: derive union choices: %v", err), true
	}
	diffs, err := bamlfuzz.SchemaOrderDiffParityWithChoices("dynclient_vs_rest_order", c.Schema, dyn, rest, choices)
	switch {
	case errors.Is(err, bamlfuzz.ErrSchemaOrderUnsupported):
		return fmt.Sprintf("axis C order check: %v (derivation did not cover every union path; treat as hard fail per F1 contract)", err), true
	case err != nil:
		return fmt.Sprintf("axis C order check: %v", err), true
	case len(diffs) > 0:
		return strings.Join(bamlfuzz.FormatSchemaOrderDiffs(diffs), "; "), true
	}
	return "", false
}

// deriveUnionChoicesFromParsed walks the schema in parallel with the
// parsed JSON, recording a UnionChoice at every union path. The path
// scheme matches the walker's CaseMetadata.UnionChoices convention
// exactly: paths start at "" (no leading "$"); class fields append
// ".<name>"; list elements append "[<idx>]"; map values append
// "[<quoted-key>]"; union variant arms append ":v". The order walker
// (order.go's walkType) strips the leading "$" before consulting
// choices, so a "" start here lands on the same key the walker
// queries.
//
// The variant picked at each union is the first arm whose Kind shape
// matches the parsed value. Ambiguous arms (e.g., KindString vs
// KindEnumRef vs KindLiteralString — all produce JSON strings) are
// disambiguated by content match where possible (KindLiteralString
// requires byte-equal value; KindEnumRef requires the string to be
// one of the enum's declared values), with KindString as the
// catch-all fallback. A union with no matching arm leaves the node
// unrecorded; the order walker then surfaces ErrSchemaOrderUnsupported,
// which checkInvalidOrderC treats as a hard failure.
func deriveUnionChoicesFromParsed(schema bamlfuzz.FuzzSchema, parsed json.RawMessage) (map[string]bamlfuzz.UnionChoice, error) {
	var v any
	if err := json.Unmarshal(parsed, &v); err != nil {
		return nil, err
	}
	choices := make(map[string]bamlfuzz.UnionChoice)
	deriveChoicesAt(schema, schema.EffectiveRoot(), v, "", choices)
	return choices, nil
}

func deriveChoicesAt(schema bamlfuzz.FuzzSchema, t bamlfuzz.FuzzType, v any, path string, choices map[string]bamlfuzz.UnionChoice) {
	switch t.Kind {
	case bamlfuzz.KindUnion:
		idx := pickUnionArm(schema, t.Variants, v)
		if idx < 0 || idx >= len(t.Variants) {
			return
		}
		selected := t.Variants[idx]
		choices[path] = bamlfuzz.UnionChoice{
			Index:        idx,
			Kind:         selected.Kind,
			Ref:          selected.Ref,
			VariantCount: len(t.Variants),
		}
		deriveChoicesAt(schema, selected, v, path+":v", choices)
	case bamlfuzz.KindOptional:
		if t.Inner == nil || v == nil {
			return
		}
		deriveChoicesAt(schema, *t.Inner, v, path, choices)
	case bamlfuzz.KindList:
		if t.Inner == nil {
			return
		}
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for i, item := range arr {
			deriveChoicesAt(schema, *t.Inner, item, fmt.Sprintf("%s[%d]", path, i), choices)
		}
	case bamlfuzz.KindMap:
		if t.Inner == nil {
			return
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, val := range obj {
			deriveChoicesAt(schema, *t.Inner, val, fmt.Sprintf("%s[%q]", path, k), choices)
		}
	case bamlfuzz.KindClassRef:
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		cls, found := schema.FindClass(t.Ref)
		if !found {
			return
		}
		for _, prop := range cls.Properties {
			fv, present := obj[prop.Name]
			if !present {
				continue
			}
			deriveChoicesAt(schema, prop.Type, fv, path+"."+prop.Name, choices)
		}
	}
}

// defaultMatchDepth caps the structural-match recursion in
// variantMatchesValue. The v1 schema generator bounds nesting at
// bamlfuzz.MaxTypeDepth=4, so 16 is comfortable headroom; the cap
// exists to keep a future grammar widening from blowing the stack.
const defaultMatchDepth = 16

// pickUnionArm picks the variant index whose structural matcher
// accepts v, choosing narrow shapes before broad ones. The previous
// implementation took the first shallow Kind match in declaration
// order, which let a `union<map<string, T>, class Foo>` with a value
// shaped like a `Foo` instance silently dispatch to the map arm
// (KindMap accepts any object) — and the order walker then skipped
// the nested class-level order check, defeating the very assertion
// PreserveSchemaOrder=true was meant to run.
//
// Narrowness order: literal → null/bool → enum ref → class ref →
// list → map → numeric → string → optional/union → catch-all. Within
// a tier, original declaration order is preserved (stable sort) so a
// schema's variant ordering still influences the pick when multiple
// arms are equally narrow.
//
// Returns -1 if no variant matches the value.
func pickUnionArm(schema bamlfuzz.FuzzSchema, variants []bamlfuzz.FuzzType, v any) int {
	indices := make([]int, len(variants))
	for i := range variants {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return variantNarrowness(variants[indices[a]]) < variantNarrowness(variants[indices[b]])
	})
	// Two passes: a strict pass first so an arm that matches every entry
	// exactly is preferred over one that only matches by tolerating
	// boundaryml/baml#3690 leaked nulls. Only if no arm matches strictly
	// do we fall back to the tolerant pass, which is the prior behavior.
	for _, tolerant := range []bool{false, true} {
		for _, idx := range indices {
			if variantMatchesValue(schema, variants[idx], v, defaultMatchDepth, tolerant) {
				return idx
			}
		}
	}
	return -1
}

// variantNarrowness ranks variant kinds from narrowest (lowest score)
// to broadest. Lower wins in pickUnionArm. KindOptional unwraps to
// its inner type so optional<class> sorts with class, not with the
// broad fallback.
func variantNarrowness(t bamlfuzz.FuzzType) int {
	switch t.Kind {
	case bamlfuzz.KindLiteral:
		return 0
	case bamlfuzz.KindNull:
		return 1
	case bamlfuzz.KindBool:
		return 2
	case bamlfuzz.KindEnumRef:
		return 3
	case bamlfuzz.KindClassRef:
		return 4
	case bamlfuzz.KindList:
		return 5
	case bamlfuzz.KindMap:
		return 6
	case bamlfuzz.KindInt, bamlfuzz.KindFloat:
		return 7
	case bamlfuzz.KindString:
		return 8
	case bamlfuzz.KindOptional:
		if t.Inner != nil {
			return variantNarrowness(*t.Inner)
		}
		return 9
	case bamlfuzz.KindUnion:
		return 9
	}
	return 10
}

// variantMatchesValue reports whether v structurally conforms to t.
// Class refs require the observed object's keys to be a subset of
// declared properties AND every required (non-optional) property to
// be present — the previous "any key-subset matches" rule let any
// JSON object that happened to use a class's property names slip
// through, even when missing required fields. List<T> and
// ClassRef<T> recurse into elements so nested narrowness is checked
// too, depth-limited to keep a degenerate cycle from blowing the
// stack.
//
// boundaryml/baml#3690 tolerance: when `tolerant` is set, a leaked
// null-valued key is ignored when matching maps whose value type cannot
// be null (the null is not a real entry) and classes (it is not a real
// extra key). For a map whose value type can be null the null is a
// legitimate entry and is matched normally regardless of `tolerant`. The
// order walker that consumes the derived choice is itself null-tolerant,
// so the arm picked here must agree with the null-stripped view the
// walker compares.
//
// pickUnionArm runs a strict pass (tolerant=false) before a tolerant one
// so an arm that matches every entry EXACTLY wins over one that only
// matches by skipping leaked nulls — e.g. map<string, optional<int>>
// beats map<string, int> for {"x": null, "y": 1}.
func variantMatchesValue(schema bamlfuzz.FuzzSchema, t bamlfuzz.FuzzType, v any, depth int, tolerant bool) bool {
	if depth <= 0 {
		// Out of depth budget: accept on top-level shape only. The
		// v1 grammar bounds nesting at bamlfuzz.MaxTypeDepth=4, so
		// random generation never lands here; the guard exists to
		// prevent a future widening from stack-blowing.
		return shallowKindMatch(t, v)
	}
	switch t.Kind {
	case bamlfuzz.KindNull:
		return v == nil
	case bamlfuzz.KindString:
		_, ok := v.(string)
		return ok
	case bamlfuzz.KindInt, bamlfuzz.KindFloat:
		_, ok := v.(float64)
		return ok
	case bamlfuzz.KindBool:
		_, ok := v.(bool)
		return ok
	case bamlfuzz.KindLiteral:
		if t.Literal == nil {
			return false
		}
		switch t.Literal.Kind {
		case bamlfuzz.LiteralString:
			s, ok := v.(string)
			return ok && s == t.Literal.String
		case bamlfuzz.LiteralInt:
			n, ok := v.(float64)
			return ok && n == float64(t.Literal.Int)
		case bamlfuzz.LiteralBool:
			b, ok := v.(bool)
			return ok && b == t.Literal.Bool
		}
		return false
	case bamlfuzz.KindEnumRef:
		s, ok := v.(string)
		if !ok {
			return false
		}
		enum, found := schema.FindEnum(t.Ref)
		if !found {
			return false
		}
		for _, val := range enum.Values {
			if val == s {
				return true
			}
		}
		return false
	case bamlfuzz.KindList:
		arr, ok := v.([]any)
		if !ok {
			return false
		}
		if t.Inner == nil {
			return true
		}
		for _, item := range arr {
			if !variantMatchesValue(schema, *t.Inner, item, depth-1, tolerant) {
				return false
			}
		}
		return true
	case bamlfuzz.KindMap:
		obj, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if t.Inner == nil {
			return true
		}
		innerNullable := bamlfuzz.CanBeNull(*t.Inner)
		matched := 0
		for _, val := range obj {
			// boundaryml/baml#3690: a leaked null-valued key from a
			// sibling class union arm is not a real map entry, so it
			// must not disqualify the map arm. The null-tolerant order
			// walker strips it from the comparison anyway; ignore it
			// here so arm derivation agrees with what the walker sees.
			// Only skip when the value type cannot itself be null — for
			// map<string, null> / map<string, optional<T>> a null is a
			// legitimate entry and must be checked normally. The skip is
			// also gated on tolerant: the strict pass rejects a null
			// against a non-nullable value so an exactly-matching arm is
			// preferred.
			if val == nil && !innerNullable && tolerant {
				continue
			}
			if !variantMatchesValue(schema, *t.Inner, val, depth-1, tolerant) {
				return false
			}
			matched++
		}
		// A non-empty object whose every entry was a skipped leaked null
		// is not a real instance of a non-nullable-value map: e.g.
		// {"k0": null} must not match map<string, int>. An empty object
		// ({}) is still a valid empty map.
		if len(obj) > 0 && matched == 0 && !innerNullable {
			return false
		}
		return true
	case bamlfuzz.KindClassRef:
		obj, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cls, found := schema.FindClass(t.Ref)
		if !found {
			return false
		}
		propByName := make(map[string]bamlfuzz.FuzzType, len(cls.Properties))
		for _, p := range cls.Properties {
			propByName[p.Name] = p.Type
		}
		for k, val := range obj {
			if _, ok := propByName[k]; !ok {
				// boundaryml/baml#3690: tolerate a leaked null-valued
				// key that is not a declared property; a non-null
				// unknown key still disqualifies the class arm. Gated on
				// tolerant so the strict pass rejects an unknown null key
				// and an exactly-matching arm is preferred.
				if val == nil && tolerant {
					continue
				}
				return false
			}
		}
		for _, p := range cls.Properties {
			if p.Type.Kind == bamlfuzz.KindOptional {
				continue
			}
			if _, present := obj[p.Name]; !present {
				return false
			}
		}
		for name, fieldType := range propByName {
			fv, present := obj[name]
			if !present {
				continue
			}
			if !variantMatchesValue(schema, fieldType, fv, depth-1, tolerant) {
				return false
			}
		}
		return true
	case bamlfuzz.KindOptional:
		if v == nil {
			return true
		}
		if t.Inner != nil {
			return variantMatchesValue(schema, *t.Inner, v, depth-1, tolerant)
		}
		return false
	case bamlfuzz.KindUnion:
		for _, variant := range t.Variants {
			if variantMatchesValue(schema, variant, v, depth-1, tolerant) {
				return true
			}
		}
		return false
	}
	return false
}

// shallowKindMatch is the depth-exhausted fallback: top-level
// shape compatibility only. Class refs degrade to "is an object";
// no key-set or required-field check.
func shallowKindMatch(t bamlfuzz.FuzzType, v any) bool {
	switch t.Kind {
	case bamlfuzz.KindNull:
		return v == nil
	case bamlfuzz.KindString, bamlfuzz.KindEnumRef:
		_, ok := v.(string)
		return ok
	case bamlfuzz.KindInt, bamlfuzz.KindFloat:
		_, ok := v.(float64)
		return ok
	case bamlfuzz.KindBool:
		_, ok := v.(bool)
		return ok
	case bamlfuzz.KindList:
		_, ok := v.([]any)
		return ok
	case bamlfuzz.KindMap, bamlfuzz.KindClassRef:
		_, ok := v.(map[string]any)
		return ok
	case bamlfuzz.KindLiteral:
		return true
	case bamlfuzz.KindOptional:
		return true
	case bamlfuzz.KindUnion:
		return true
	}
	return false
}

// reproductionForInvalid returns the canonical command to re-run a
// failing invalid-case in isolation, embedded in the envelope so a
// developer can copy-paste it from the failure log. Mirrors
// reproductionFor's source dispatch: fuzz cases re-run the engine
// against the persisted -fuzzcachedir; rapid cases re-run a single
// subtest under -run.
//
// The Axis C rapid subtree has a per-mode layer (preserve_off /
// preserve_on) under "rapid"; the repro path reflects this when the
// case carries the mode in c.PreserveSchemaOrder so the printed
// command targets the exact failing subtest.
//
// Both the fuzz and rapid repros hardcode `-tags=integration,subprocess`
// because the nightly invokes the test binary under those tags
// (subprocess routes through the cmd/worker go-plugin, inprocess uses
// the in-binary worker — they exercise different code paths in the
// worker pool). Replaying with `-tags=integration` alone would run the
// inprocess worker and potentially mask a failure that only reproduces
// on the subprocess path. NB: the existing TestBamlfuzz{Static,Dynamic}Oracle
// repro emitters in bamlfuzz_{static,dynamic}_test.go still emit
// `-tags=integration` for their rapid branch; that's the same bug in
// out-of-scope files, worth a future cleanup but left alone here.
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
	var runPath string
	switch c.Mode {
	case bamlfuzz.InvalidJSONCoercion:
		mode := "preserve_off"
		if c.PreserveSchemaOrder {
			mode = "preserve_on"
		}
		runPath = fmt.Sprintf("^%s$/^rapid$/^%s$/^case_%d$", testName, mode, caseIdx)
	default:
		runPath = fmt.Sprintf("^%s$/^rapid$/^case_%d$", testName, caseIdx)
	}
	cmd := fmt.Sprintf("go test -tags=integration,subprocess -run='%s' ./integration -count=1", runPath)
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if cases := os.Getenv("BAMLFUZZ_INVALID_CASES"); cases != "" {
		cmd = "BAMLFUZZ_INVALID_CASES=" + cases + " " + cmd
	}
	return cmd
}
