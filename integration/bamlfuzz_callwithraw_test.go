//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
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

// callWithRawOracleArtifactDir is where RawFailureEnvelope artifacts land
// on failure or with BAMLFUZZ_KEEP_ARTIFACTS=1. Stable relative to the
// integration test working directory so CI can collect from a predictable
// path.
const callWithRawOracleArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/callwithraw/_artifacts"

// rawCasesPerMode controls how many random call-with-raw cases run per
// preserve mode. The default of 4 is sized for PR CI wall time; the
// nightly fuzz workflow cranks it up via BAMLFUZZ_CALLWITHRAW_CASES so
// each scheduled run explores a broader slice of the schema space.
func rawCasesPerMode() int {
	return envIntDefault("BAMLFUZZ_CALLWITHRAW_CASES", 4)
}

// TestBamlfuzzCallWithRawOracle drives the /call-with-raw oracle: for each
// fuzz case it lowers the schema through the dynamic emitter and exercises
// the with-raw legs —
//
//  1. dynclient.Client.DynamicCallRaw (in-proc), and
//  2. the REST /call-with-raw/_dynamic endpoint —
//
// then asserts the dynamic oracle's parsed-`data` equivalence still holds
// (A1/A2) AND pins the raw echo channel (A3): the extracted output text
// each leg returns must equal the mock's emitted content byte-for-byte and
// equal each other. Raw is pre-parse wire text, not JSON, so it is
// compared with Go string equality, never SemanticDiff. A4 anchors the
// with-raw parsed data against the plain /call dynclient leg, proving
// with-raw ⊇ plain-call.
//
// It reuses the dynamic oracle's case stream verbatim
// (CoupledCaseGen(DynamicSafeSchemaGen()), LowerToDynamicSchema, the
// dynamic call-body builder, and unsupportedActionFor) and the dynamic
// corpus, so no schema/value-generator change is needed for raw coverage —
// MockLLMContent already *is* the expected raw text.
func TestBamlfuzzCallWithRawOracle(t *testing.T) {
	dynclientCallGate(t)

	corpus, err := loadDynamicCorpus(dynamicOracleCorpusDir)
	if err != nil {
		t.Fatalf("load corpus from %s: %v", dynamicOracleCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("dynamic corpus at %s is empty — the raw oracle reuses it", dynamicOracleCorpusDir)
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
				runCallWithRawOracleCase(t, dyn, caseCopy, caseIdx, caseSourceCorpus)
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
				cases := rawCasesPerMode()
				for i := 0; i < cases; i++ {
					i := i
					seed := rawSeedFor(preserve, i)
					t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
						caseCopy := buildRawRapidCase(t, seed, preserve, i)
						runCallWithRawOracleCase(t, dyn, caseCopy, i, caseSourceRapid)
					})
				}
			})
		}
	})
}

// FuzzBamlfuzzCallWithRaw is the testing.F companion to
// TestBamlfuzzCallWithRawOracle, exposing the raw oracle to Go's native
// fuzz engine via the bamlfuzz.MakeFuzz bridge. Like FuzzBamlfuzzDynamic
// it draws PreserveSchemaOrder from the bit stream and ships no f.Add seed
// corpus (the engine populates its own under -fuzzcachedir). The version
// gates match the dynamic oracle: dynamic endpoints require BAML
// >= 0.215.0, and without an external baml source the pre-0.219 streaming
// API does not propagate dynamic classes to the parser.
func FuzzBamlfuzzCallWithRaw(f *testing.F) {
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
			Mode:                bamlfuzz.OracleCallWithRaw,
			PreserveSchemaOrder: preserve,
			Schema:              cc.Schema,
			Value:               cc.Value,
			MockLLMContent:      cc.Walk.MockLLMContent,
			Expected:            cc.Walk.Expected,
			Metadata:            cc.Walk.Metadata,
		}
		runCallWithRawOracleCase(t, dyn, c, 0, caseSourceFuzz)
	})
}

// rawSeedFor produces a deterministic rapid seed for a (preserve, i) pair.
// The "callwithraw" domain prefix keeps the raw oracle's seed stream
// disjoint from the dynamic oracle's (dynamicSeedFor), so the two oracles
// explore different schema shapes at the same case index. BAMLFUZZ_SEED,
// when set, XORs into every per-case seed so a single env var perturbs the
// whole matrix.
func rawSeedFor(preserve bool, i int) uint64 {
	h := fnv.New64a()
	h.Write([]byte("callwithraw:"))
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

// buildRawRapidCase synthesizes one OracleCase by drawing a dynamic-safe
// schema + value deterministically from seed, using the same
// CoupledCaseGen the dynamic oracle uses so the raw oracle exercises the
// identical case stream.
func buildRawRapidCase(t *testing.T, seed uint64, preserve bool, idx int) bamlfuzz.OracleCase {
	t.Helper()
	cc := bamlfuzz.CoupledCaseGen(bamlfuzz.DynamicSafeSchemaGen()).Example(int(seed))
	return bamlfuzz.OracleCase{
		Name:                fmt.Sprintf("rawrapid_%t_%d", preserve, idx),
		Seed:                int64(seed),
		CaseIndex:           idx,
		Mode:                bamlfuzz.OracleCallWithRaw,
		PreserveSchemaOrder: preserve,
		Schema:              cc.Schema,
		Value:               cc.Value,
		MockLLMContent:      cc.Walk.MockLLMContent,
		Expected:            cc.Walk.Expected,
		Metadata:            cc.Walk.Metadata,
	}
}

// runCallWithRawOracleCase performs the with-raw comparison for one
// OracleCase, capturing all relevant context into a RawFailureEnvelope
// when any leg disagrees. `source` selects how
// ErrDynamicSchemaUnsupported is treated: corpus/fuzz cases skip, rapid
// cases fail (the dynamic-safe generator must never produce an unsupported
// schema).
func runCallWithRawOracleCase(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, caseIdx int, source caseSource) {
	t.Helper()

	envelope := &bamlfuzz.RawFailureEnvelope{
		GeneratorVersion:    bamlfuzz.GeneratorVersion,
		RapidSeed:           c.Seed,
		CaseIndex:           caseIdx,
		CaseName:            c.Name,
		OracleMode:          bamlfuzz.OracleCallWithRaw,
		PreserveSchemaOrder: c.PreserveSchemaOrder,
		Schema:              c.Schema,
		MockLLMContent:      c.MockLLMContent,
		Expected:            c.Expected,
		Metadata:            c.Metadata,
		Reproduction:        reproductionForRaw(c, caseIdx, source),
	}

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if errors.Is(err, bamlfuzz.ErrDynamicSchemaUnsupported) {
		switch unsupportedActionFor(source) {
		case unsupportedSkip:
			t.Skipf("dynamic emitter skipped schema: %v", err)
			return
		case unsupportedFail:
			envelope.DynamicSkipReason = err.Error()
			failAndDumpRaw(t, envelope, "rapid generator produced unsupported schema: %v", err)
			return
		}
	}
	if err != nil {
		envelope.DynamicSkipReason = err.Error()
		failAndDumpRaw(t, envelope, "LowerToDynamicSchema failed: %v", err)
		return
	}
	envelope.DynamicSchema = &lowered

	// The raw channel's "expected" is the mock's emitted content. An empty
	// MockLLMContent would make the A3 string-equality checks vacuous
	// (raw == "" == ""), so treat it as a harness error: the walker never
	// renders empty content for a valid case.
	if len(c.MockLLMContent) == 0 {
		failAndDumpRaw(t, envelope, "case has empty MockLLMContent; raw oracle cannot assert a vacuous echo")
		return
	}
	expectedRaw := string(c.MockLLMContent)

	scenarioID := fmt.Sprintf("bamlfuzz-raw-%s", scenarioSafe(c.Name))
	envelope.MockLLMScenarioID = scenarioID
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer registerCancel()
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        expectedRaw,
		ChunkSize:      0,
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(registerCtx, scenario); err != nil {
		failAndDumpRaw(t, envelope, "register scenario: %v", err)
		return
	}

	clientReg := testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID)

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

	// --- dynclient with-raw leg ---
	var (
		libResp *dynclient.CallRawResult
		libErr  error
	)
	panicked, panicVal, panicStack := callWithRecover(func() {
		libResp, libErr = dyn.DynamicCallRaw(callCtx, libReq)
	})
	if panicked {
		envelope.DynclientPanic = fmt.Sprintf("%v", panicVal)
		envelope.DynclientPanicStack = string(panicStack)
		failAndDumpRaw(t, envelope, "dyn.DynamicCallRaw panicked: %v\n%s", panicVal, panicStack)
		return
	}
	switch {
	case libErr != nil:
		// A context/transport error means the leg never produced a
		// verdict; letting it satisfy an equality check would mask a real
		// divergence, so it is a harness failure, not an oracle rejection.
		if isContextErr(libErr) {
			t.Fatalf("harness failure: dynclient DynamicCallRaw (case=%s): %v", c.Name, libErr)
		}
		envelope.DynclientError = libErr.Error()
	case libResp == nil:
		envelope.DynclientError = "nil response from dyn.DynamicCallRaw"
		failAndDumpRaw(t, envelope, "dynclient returned nil response without an error")
		return
	default:
		envelope.DynclientOutput = libResp.Data
		envelope.DynclientRaw = libResp.Raw
	}

	// --- REST /call-with-raw/_dynamic leg ---
	// Build the body via bamlutils so OrderedMap insertion order travels
	// intact across the wire (testutil.DynamicOutputSchema is map-backed
	// and would scramble property order).
	restBody, err := buildDynamicCallBody(libReq, &lowered)
	if err != nil {
		failAndDumpRaw(t, envelope, "build REST body: %v", err)
		return
	}
	var restResp *testutil.DynamicCallWithRawResponse
	panicked, panicVal, panicStack = callWithRecover(func() {
		restResp, err = BAMLClient.DynamicCallWithRawJSON(callCtx, restBody)
	})
	if panicked {
		envelope.RESTPanic = fmt.Sprintf("%v", panicVal)
		envelope.RESTPanicStack = string(panicStack)
		failAndDumpRaw(t, envelope, "BAMLClient.DynamicCallWithRawJSON panicked: %v\n%s", panicVal, panicStack)
		return
	}
	switch {
	case err != nil:
		if isContextErr(err) {
			t.Fatalf("harness failure: REST DynamicCallWithRawJSON (case=%s): %v", c.Name, err)
		}
		envelope.RESTError = err.Error()
	case restResp == nil:
		envelope.RESTError = "nil response from BAMLClient.DynamicCallWithRawJSON"
		failAndDumpRaw(t, envelope, "REST client returned nil response without an error")
		return
	default:
		envelope.RESTStatus = restResp.StatusCode
		envelope.RESTBody = restResp.Data
		envelope.RESTRaw = restResp.Raw
		if restResp.StatusCode >= 400 {
			envelope.RESTError = restResp.Error
		}
	}

	// --- A4 anchor: plain /call dynclient leg (same case) ---
	// Cheap reuse of the existing DynamicCall path to prove with-raw's
	// parsed `data` matches the plain call. An error here is recorded but
	// only fails the A4 assertion below, not the whole case.
	var (
		plainResp *dynclient.CallResult
		plainErr  error
	)
	panicked, panicVal, panicStack = callWithRecover(func() {
		plainResp, plainErr = dyn.DynamicCall(callCtx, libReq)
	})
	if panicked {
		envelope.PlainCallError = fmt.Sprintf("panic: %v", panicVal)
	} else if plainErr != nil {
		if isContextErr(plainErr) {
			t.Fatalf("harness failure: dynclient DynamicCall (A4 anchor, case=%s): %v", c.Name, plainErr)
		}
		envelope.PlainCallError = plainErr.Error()
	} else if plainResp != nil {
		envelope.PlainCallOutput = plainResp.Data
	}

	// ---- Assertions ----
	var failures []string
	if libErr != nil {
		failures = append(failures, fmt.Sprintf("dynclient errored: %v", libErr))
	}
	if envelope.RESTError != "" {
		failures = append(failures, fmt.Sprintf("REST errored (status %d): %s", envelope.RESTStatus, envelope.RESTError))
	}

	// A1 — parsed data still equals Expected on both legs. SemanticDiff
	// hard-errors on an empty payload (decodeAny), so an empty `data` side
	// cannot pass by skip.
	if libErr == nil && envelope.DynclientOutput != nil {
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_dynclient_raw", c.Expected, envelope.DynclientOutput); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_dynclient_raw diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "expected ≠ dynclient (data)")
		}
	}
	if envelope.RESTError == "" && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_rest_raw", c.Expected, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_rest_raw diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "expected ≠ REST (data)")
		}
	}
	// A2 — data parity between the two with-raw legs.
	if libErr == nil && envelope.RESTError == "" && envelope.DynclientOutput != nil && envelope.RESTBody != nil {
		if diff, err := bamlfuzz.SemanticDiffParity("dynclient_raw_vs_rest_raw", envelope.DynclientOutput, envelope.RESTBody); err != nil {
			failures = append(failures, fmt.Sprintf("dynclient_raw_vs_rest_raw diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "dynclient ≠ REST (data)")
		}
	}

	// A3 — the raw echo channel. raw is plain pre-parse text, NOT JSON, so
	// it is compared with Go string equality against the known mock input
	// (MockLLMContent) on both legs, and the two raw strings must equal
	// each other. Feeding raw into SemanticDiff/decodeAny would error (raw
	// text isn't JSON) or, worse, falsely pass.
	if libErr == nil {
		if envelope.DynclientRaw != expectedRaw {
			envelope.RawMismatch = append(envelope.RawMismatch,
				fmt.Sprintf("dynclient raw ≠ MockLLMContent: got %q want %q", envelope.DynclientRaw, expectedRaw))
			failures = append(failures, "dynclient raw ≠ MockLLMContent")
		}
	}
	if envelope.RESTError == "" {
		if envelope.RESTRaw != expectedRaw {
			envelope.RawMismatch = append(envelope.RawMismatch,
				fmt.Sprintf("REST raw ≠ MockLLMContent: got %q want %q", envelope.RESTRaw, expectedRaw))
			failures = append(failures, "REST raw ≠ MockLLMContent")
		}
	}
	if libErr == nil && envelope.RESTError == "" {
		if envelope.DynclientRaw != envelope.RESTRaw {
			envelope.RawMismatch = append(envelope.RawMismatch,
				fmt.Sprintf("dynclient raw ≠ REST raw: %q vs %q", envelope.DynclientRaw, envelope.RESTRaw))
			failures = append(failures, "dynclient raw ≠ REST raw")
		}
	}

	// A4 — with-raw parsed data ⊇ plain /call. Parity comparison so a
	// leaked null key on either BAML-generated side is tolerated like the
	// dynamic oracle's dynclient_vs_rest check.
	if envelope.PlainCallError == "" && envelope.PlainCallOutput != nil &&
		libErr == nil && envelope.DynclientOutput != nil {
		if diff, err := bamlfuzz.SemanticDiffParity("dynclient_raw_vs_plain_call", envelope.DynclientOutput, envelope.PlainCallOutput); err != nil {
			failures = append(failures, fmt.Sprintf("dynclient_raw_vs_plain_call diff: %v", err))
		} else if len(diff) > 0 {
			envelope.SemanticDiff = append(envelope.SemanticDiff, diff...)
			failures = append(failures, "with-raw data ≠ plain /call data")
		}
	}

	if len(failures) == 0 {
		if os.Getenv("BAMLFUZZ_KEEP_ARTIFACTS") == "1" {
			if path, err := bamlfuzz.WriteRawReplayArtifact(callWithRawOracleArtifactDir, envelope); err == nil {
				t.Logf("kept replay artifact (success): %s", path)
			}
		}
		return
	}
	failAndDumpRaw(t, envelope, "%s", strings.Join(failures, "; "))
}

// failAndDumpRaw writes the RawFailureEnvelope to the artifact dir and
// fails the test with a message that points at the replay path.
func failAndDumpRaw(t *testing.T, envelope *bamlfuzz.RawFailureEnvelope, format string, args ...any) {
	t.Helper()
	path, err := bamlfuzz.WriteRawReplayArtifact(callWithRawOracleArtifactDir, envelope)
	if err != nil {
		t.Errorf("write replay artifact: %v", err)
		t.Errorf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	t.Errorf("%s\nreplay: %s\nrepro: %s", msg, path, envelope.Reproduction)
}

// reproductionForRaw returns the canonical command to re-run a failing
// call-with-raw case in isolation, embedded in the envelope so a developer
// can copy-paste it. The subtest trees mirror reproductionFor:
//
//	TestBamlfuzzCallWithRawOracle / corpus / <case_name>
//	TestBamlfuzzCallWithRawOracle / rapid  / preserve_{on,off} / case_<index>
//
// Fuzz cases bypass the subtest tree: the engine reaches them by replaying
// a corpus entry under -fuzzcachedir, so the recipe runs the engine.
func reproductionForRaw(c bamlfuzz.OracleCase, caseIdx int, source caseSource) string {
	if source == caseSourceFuzz {
		return "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzCallWithRaw$' -fuzztime=10m " +
			"-fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	}
	segments := []string{"^TestBamlfuzzCallWithRawOracle$"}
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
	// Prepend SEED first, then CASES, so CASES lands leftmost — matching
	// reproductionFor's rendered prefix order.
	if seed := os.Getenv("BAMLFUZZ_SEED"); seed != "" {
		cmd = "BAMLFUZZ_SEED=" + seed + " " + cmd
	}
	if cases := os.Getenv("BAMLFUZZ_CALLWITHRAW_CASES"); cases != "" {
		cmd = "BAMLFUZZ_CALLWITHRAW_CASES=" + cases + " " + cmd
	}
	return cmd
}

// TestReproductionForRaw pins the shape of the reproduction command
// embedded in raw-oracle failure envelopes across the corpus, rapid, and
// fuzz source trees plus the BAMLFUZZ_SEED / BAMLFUZZ_CALLWITHRAW_CASES env
// prefixes.
func TestReproductionForRaw(t *testing.T) {
	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_CALLWITHRAW_CASES", "")

	corpus := reproductionForRaw(bamlfuzz.OracleCase{Name: "scalar_string"}, 0, caseSourceCorpus)
	wantCorpus := "go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^corpus$/^scalar_string$' ./integration -count=1"
	if corpus != wantCorpus {
		t.Errorf("corpus repro:\n got:  %s\n want: %s", corpus, wantCorpus)
	}

	rapidOn := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 2, caseSourceRapid)
	wantRapidOn := "go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^rapid$/^preserve_on$/^case_2$' ./integration -count=1"
	if rapidOn != wantRapidOn {
		t.Errorf("rapid preserve-on repro:\n got:  %s\n want: %s", rapidOn, wantRapidOn)
	}

	rapidOff := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 3, caseSourceRapid)
	wantRapidOff := "go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^rapid$/^preserve_off$/^case_3$' ./integration -count=1"
	if rapidOff != wantRapidOff {
		t.Errorf("rapid preserve-off repro:\n got:  %s\n want: %s", rapidOff, wantRapidOff)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	withSeed := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 0, caseSourceRapid)
	wantWithSeed := "BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^rapid$/^preserve_on$/^case_0$' ./integration -count=1"
	if withSeed != wantWithSeed {
		t.Errorf("rapid+seed repro:\n got:  %s\n want: %s", withSeed, wantWithSeed)
	}

	t.Setenv("BAMLFUZZ_SEED", "")
	t.Setenv("BAMLFUZZ_CALLWITHRAW_CASES", "50")
	withCases := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 25, caseSourceRapid)
	wantWithCases := "BAMLFUZZ_CALLWITHRAW_CASES=50 go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withCases != wantWithCases {
		t.Errorf("rapid+cases repro:\n got:  %s\n want: %s", withCases, wantWithCases)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	t.Setenv("BAMLFUZZ_CALLWITHRAW_CASES", "50")
	withSeedAndCases := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: false}, 25, caseSourceRapid)
	wantWithSeedAndCases := "BAMLFUZZ_CALLWITHRAW_CASES=50 BAMLFUZZ_SEED=12345 go test -tags=integration -run='^TestBamlfuzzCallWithRawOracle$/^rapid$/^preserve_off$/^case_25$' ./integration -count=1"
	if withSeedAndCases != wantWithSeedAndCases {
		t.Errorf("rapid+seed+cases repro:\n got:  %s\n want: %s", withSeedAndCases, wantWithSeedAndCases)
	}

	t.Setenv("BAMLFUZZ_SEED", "12345")
	t.Setenv("BAMLFUZZ_CALLWITHRAW_CASES", "50")
	fuzz := reproductionForRaw(bamlfuzz.OracleCase{PreserveSchemaOrder: true}, 0, caseSourceFuzz)
	wantFuzz := "go test -tags=integration,subprocess -run='^$' -fuzz='^FuzzBamlfuzzCallWithRaw$' -fuzztime=10m -fuzzcachedir=adapters/common/codegen/testdata/bamlfuzz/.fuzzcache ./integration"
	if fuzz != wantFuzz {
		t.Errorf("fuzz repro:\n got:  %s\n want: %s", fuzz, wantFuzz)
	}
}
