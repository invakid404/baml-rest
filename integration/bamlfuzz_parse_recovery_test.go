//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/invakid404/baml-rest/internal/debaml"
)

// parseRecoveryCorpusDir is the in-tree JSONish recovery corpus. The
// bamlfuzz package lives under adapters/common; package integration reaches
// it via a repo-root-relative path, matching the dynamic/static corpora.
const parseRecoveryCorpusDir = "../adapters/common/codegen/testdata/bamlfuzz/parse_recovery"

// parseRecoveryArtifactDir is where parse-diff failure envelopes land on
// failure or with BAMLFUZZ_KEEP_ARTIFACTS=1. Gitignored like the other
// bamlfuzz oracle artifact dirs.
const parseRecoveryArtifactDir = "../adapters/common/codegen/testdata/bamlfuzz/parse_recovery/_artifacts"

// bamlDynamicParser adapts dynclient's final dynamic parse to the
// bamlfuzz.Parser interface so the differential harness can drive BAML as
// the oracle. It covers final parse only: parse-stream over accumulated
// prefixes is not yet exposed by dynclient/worker, so a Stream request is
// declined with ErrParserUnavailable (the streaming differential is
// deferred — see the scope's blocked-on-plumbing note).
type bamlDynamicParser struct {
	dyn *dynclient.Client
}

// Name identifies the BAML oracle leg in diff output and envelopes.
func (p bamlDynamicParser) Name() string { return "baml_dynamic" }

// Parse drives dynclient.DynamicParse for a final parse. The returned
// JSON is the flattened, absent-optional-injected, order-normalized
// payload the dynamic endpoints expose — exactly the shape a native
// parser must reproduce. Stream requests are declined (deferred).
func (p bamlDynamicParser) Parse(ctx context.Context, req bamlfuzz.ParseRequest) (bamlfuzz.ParseResult, error) {
	if req.Stream {
		return bamlfuzz.ParseResult{}, bamlfuzz.ErrParserUnavailable
	}
	lowered, err := bamlfuzz.LowerToDynamicSchema(req.Schema)
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	preserve := req.PreserveSchemaOrder
	resp, err := p.dyn.DynamicParse(ctx, dynclient.ParseRequest{
		Raw:                 req.Raw,
		OutputSchema:        &lowered,
		PreserveSchemaOrder: &preserve,
	})
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	return bamlfuzz.ParseResult{JSON: append(json.RawMessage(nil), resp.Data...)}, nil
}

// nativeDeBAMLParser adapts the bounded native de-BAML parser
// (internal/debaml.Parse) to the bamlfuzz.Parser interface so the
// differential harness can diff the native candidate against the BAML
// oracle. It lowers the FuzzSchema with the SAME LowerToDynamicSchema
// helper the BAML leg uses, calls the native parser, maps
// ErrDeBAMLParseUnsupported to ErrParserUnavailable (so the comparator
// records a fallback skip rather than spurious drift), and runs the
// identical absent-optional-injection + order/sort normalization
// dynclient's parse-only path applies after a (already-flattened) parse —
// so a CLAIMED native result is directly comparable to BAML's. A claimed
// native parse ERROR is surfaced unchanged so the comparator checks
// error parity.
type nativeDeBAMLParser struct{}

// Name identifies the native candidate leg in diff output and envelopes.
func (nativeDeBAMLParser) Name() string { return "native_debaml" }

// Parse drives internal/debaml.Parse for a final parse and normalizes its
// flattened output exactly like dynclient's parse-only path.
func (nativeDeBAMLParser) Parse(ctx context.Context, req bamlfuzz.ParseRequest) (bamlfuzz.ParseResult, error) {
	lowered, err := bamlfuzz.LowerToDynamicSchema(req.Schema)
	if err != nil {
		// A schema bamlfuzz cannot lower onto the dynamic path is out of the
		// native parser's scope; decline so the comparator falls back rather
		// than flag spurious drift (the BAML leg lowers via the same helper,
		// so it declines/errors symmetrically).
		return bamlfuzz.ParseResult{}, bamlfuzz.ErrParserUnavailable
	}
	res, err := debaml.Parse(ctx, bamlutils.DeBAMLParseRequest{
		Raw:          req.Raw,
		OutputSchema: &lowered,
		Stream:       req.Stream,
	})
	if err != nil {
		if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			// Native declined -> comparator skips the native leg (fallback).
			return bamlfuzz.ParseResult{}, bamlfuzz.ErrParserUnavailable
		}
		// Claimed native parse failure -> surfaced so the comparator checks
		// success/error parity against BAML.
		return bamlfuzz.ParseResult{}, err
	}

	// Post-process the already-flattened native JSON exactly like
	// dynclient.DynamicParse does after FlattenDynamicOutput (which is a
	// no-op here — the native parser never emits a DynamicProperties
	// envelope): inject absent optionals, then order by schema (preserve)
	// or sort alphabetically.
	out := append(json.RawMessage(nil), res.JSON...)
	out, err = bamlutils.InjectAbsentOptionals(out, &lowered)
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	if req.PreserveSchemaOrder {
		out, err = bamlutils.ReorderDynamicOutputBySchema(out, &lowered)
	} else {
		out, err = bamlutils.SortDynamicOutput(out)
	}
	if err != nil {
		return bamlfuzz.ParseResult{}, err
	}
	return bamlfuzz.ParseResult{JSON: out}, nil
}

// parseRecoveryNativeClaim pins the native-parser cut-line per corpus case
// (by Name): true means the native parser is expected to CLAIM the final
// parse — produce JSON, or a claimed parse error that matches BAML's error
// — and false means it FALLS BACK to BAML (ErrDeBAMLParseUnsupported ->
// SkippedNative). Strict / markdown-fenced / prose-extracted JSON is
// claimed; the conservative M2a fixing subset (trailing commas, unquoted
// keys, single quotes, mixed jsonish, and the nested/literal/prose variants)
// is now also claimed. Cases the native parser DECLINES — repairs outside
// the fixing subset (comments), and "couldn't find/complete a candidate"
// (an unterminated/truncated structure, which BAML recovers but M2a defers)
// — stay fallback. Cases absent from the map (the streaming-only ones)
// carry no final leg and are not asserted.
var parseRecoveryNativeClaim = map[string]bool{
	"markdown_fence_object":    true,
	"prose_before_after_json":  true,
	"quoted_brace_prose":       true,
	"fenced_backticks_in_json": true,
	"strict_list_optional":     true,
	"strict_literal_enum":      true,
	// Truncated mid-value: an opening brace that never closes. Native finds
	// no cleanly-claimable candidate and DECLINES (BAML closes it at EOF and
	// recovers a partial value, which M2a defers) — so it falls back, not a
	// claimed error.
	"truncated_final_error": false,
	// M2a fixing-parser subset: now native-claimed and diff-green vs BAML.
	"trailing_commas": true,
	"unquoted_keys":   true,
	"single_quotes":   true,
	"mixed_jsonish":   true,
	// M2a differential-guarded additions.
	"unquoted_keys_literals":        true,
	"single_quotes_nested":          true,
	"prose_jsonish_unquoted_single": true,
	// Nested trailing commas with QUOTED values — parity-safe, claimed.
	"nested_trailing_commas_quoted": true,
	// Nested trailing commas around UNQUOTED NUMBER values: BAML greedily
	// consumes the comma and errors on coercion; native declines (fallback)
	// rather than claim a cleanly-parsed object it can't match.
	"trailing_commas_nested_object_array": false,
	// Leading / repeated / stray commas — also part of the claimed subset
	// (BAML's object/array states ignore stray commas while waiting for
	// content), so native claims them too.
	"leading_comma_object":   true,
	"repeated_commas_object": true,
	"array_stray_commas":     true,
	// Deferred repair (comments) — pinned fallback until claimed.
	"comments_fallback": false,
}

// parseRecoveryStats tallies how many final-parse cases the native parser
// claimed vs fell back on, logged as a summary so a shift in the native
// cut-line is visible even when every case still passes. The harness runs
// the per-case subtests sequentially (no t.Parallel), so a plain
// pointer-shared counter needs no locking.
type parseRecoveryStats struct {
	claimed  int
	fallback int
}

// TestBamlfuzzParseRecovery characterizes BAML's JSONish final-parse
// recovery behavior against the checked-in corpus and, when a native
// parser is registered, diffs native against BAML. BAML is the oracle:
// each case's `want` is BAML's observed outcome, so the gating leg asserts
// the live BAML parse still matches the recorded outcome (a drift detector
// for BAML behavior changes), and the differential leg holds any future
// native parser to BAML's exact behavior.
//
// With the default NoopParser the differential leg is a vacuous skip, so
// today the test is a pure BAML characterization. Streaming-prefix cases
// validate corpus format + prefix-growth monotonicity but skip the
// per-prefix differential, which is blocked on direct parse-stream
// exposure (deferred).
//
// Run with BAMLFUZZ_PARSE_CAPTURE=1 to log BAML's observed final-parse
// outcomes instead of gating — used to (re)capture the corpus `want`
// values when adding cases or after an intentional BAML behavior change.
func TestBamlfuzzParseRecovery(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	dynclientCallGate(t)

	corpus, err := bamlfuzz.LoadParseRecoveryCorpus(parseRecoveryCorpusDir)
	if err != nil {
		t.Fatalf("load parse recovery corpus from %s: %v", parseRecoveryCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("parse recovery corpus at %s is empty", parseRecoveryCorpusDir)
	}

	dyn, err := testutil.NewDynclient(TestEnv)
	if err != nil {
		t.Fatalf("NewDynclient: %v", err)
	}
	baml := bamlDynamicParser{dyn: dyn}
	// Register the bounded M1 native parser as the differential candidate so
	// DiffParsers diffs it against BAML; restore the prior (no-op) parser
	// when the test ends.
	restore := bamlfuzz.RegisterNativeParser(nativeDeBAMLParser{})
	defer restore()
	native := bamlfuzz.RegisteredNativeParser()
	capture := os.Getenv("BAMLFUZZ_PARSE_CAPTURE") == "1"

	stats := &parseRecoveryStats{}
	for i, c := range corpus {
		i := i
		c := c
		t.Run(c.Name, func(t *testing.T) {
			runParseRecoveryCase(t, baml, native, c, i, capture, stats)
		})
	}
	t.Logf("native de-BAML final-parse dispositions: %d claimed, %d fell back to BAML", stats.claimed, stats.fallback)
}

// runParseRecoveryCase drives the final and/or streaming legs of one
// recovery case.
func runParseRecoveryCase(t *testing.T, baml, native bamlfuzz.Parser, c bamlfuzz.ParseRecoveryCase, idx int, capture bool, stats *parseRecoveryStats) {
	t.Helper()

	if c.HasFinal() {
		t.Run("final", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req := bamlfuzz.ParseRequest{
				Schema:              c.Schema,
				Raw:                 c.Raw,
				Stream:              false,
				PreserveSchemaOrder: c.PreserveSchemaOrder,
			}

			bamlRes, bamlErr := baml.Parse(ctx, req)
			if capture {
				logObservedOutcome(t, "final", c.Raw, bamlRes, bamlErr)
			} else {
				characterizeFinal(t, c, idx, bamlRes, bamlErr)
			}

			// Native-vs-BAML differential. choices is nil: recovery
			// schemas carry no unions, so the order check needs none.
			res := bamlfuzz.DiffParsers(ctx, baml, native, req, nil)
			if !res.SkippedNative && len(res.Failures) > 0 {
				dumpParseDiffAndFail(t, parseRecoveryArtifactDir, parseDiffEnvelope(c, idx, -1, "", c.Raw, false, res), strings.Join(res.Failures, "; "))
			}

			// Record and assert the native parser's disposition (claimed vs
			// fallback). SkippedNative means it returned ErrParserUnavailable
			// (mapped from ErrDeBAMLParseUnsupported) and the BAML leg stands
			// alone; otherwise it claimed the parse and the diff above held it
			// to BAML. The expected disposition pins the M1 cut-line per case.
			claimed := !res.SkippedNative
			if claimed {
				stats.claimed++
			} else {
				stats.fallback++
			}
			// Every final-parse fixture MUST pin an expected disposition, so a
			// new corpus case can't silently skip the cut-line assertion.
			want, ok := parseRecoveryNativeClaim[c.Name]
			if !ok {
				t.Fatalf("final-parse case %q has no pinned native disposition; add it to parseRecoveryNativeClaim (true=claimed, false=fallback)", c.Name)
			}
			if claimed {
				t.Logf("native CLAIMED final parse for %q", c.Name)
			} else {
				t.Logf("native FELL BACK to BAML for %q", c.Name)
			}
			if claimed != want {
				t.Errorf("native disposition for %q: got claimed=%v, want claimed=%v (M1 cut-line drift)", c.Name, claimed, want)
			}
		})
	}

	if c.HasPrefixes() {
		t.Run("streaming", func(t *testing.T) {
			// Exercise the corpus-format invariant: accumulated prefixes
			// must grow. This holds independent of any BAML plumbing.
			raws := c.PrefixRaws()
			for i := 1; i < len(raws); i++ {
				if !strings.HasPrefix(raws[i], raws[i-1]) {
					t.Errorf("prefix[%d] %q does not extend prefix[%d] %q",
						i, c.Prefixes[i].Name, i-1, c.Prefixes[i-1].Name)
				}
			}
			// The per-prefix native-vs-BAML differential needs direct
			// BAML parse-stream over arbitrary accumulated prefixes, which
			// dynclient/worker do not yet expose. The corpus + harness
			// format are in place (DiffParserPrefixes); wiring the live
			// differential is deferred to the parse-stream plumbing PR.
			t.Skip("streaming parse-stream differential blocked on direct parse-stream exposure (deferred)")
		})
	}
}

// characterizeFinal gates the live BAML final parse against the recorded
// `want`: status (success/error) parity, plus strict JSON + key-order
// equality on a successful parse. `want` is BAML's own captured output, so
// strict equality is correct — a divergence means BAML's parse behavior
// drifted from the checked-in characterization.
func characterizeFinal(t *testing.T, c bamlfuzz.ParseRecoveryCase, idx int, res bamlfuzz.ParseResult, parseErr error) {
	t.Helper()
	observed := bamlfuzz.ParseStatusSuccess
	if parseErr != nil {
		observed = bamlfuzz.ParseStatusError
	}
	if observed != c.Want.Status {
		outcome := bamlfuzz.ParseOutcome{Parser: "baml_dynamic"}
		if parseErr != nil {
			outcome.Error = parseErr.Error()
		} else {
			outcome.JSON = res.JSON
		}
		env := &bamlfuzz.ParseDiffFailureEnvelope{
			CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
			PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
			PrefixIndex: -1, Raw: c.Raw,
			ExpectedStatus: c.Want.Status, Expected: c.Want.JSON,
			BAML:     outcome,
			Failures: []string{fmt.Sprintf("status parity: want %q, BAML produced %q", c.Want.Status, observed)},
		}
		dumpParseDiffAndFail(t, parseRecoveryArtifactDir, env, fmt.Sprintf("BAML status %q ≠ want %q", observed, c.Want.Status))
		return
	}
	if !c.Want.IsSuccess() {
		return // both errored — parity holds, no payload to compare.
	}
	diff, err := bamlfuzz.SemanticDiffStrict("want_vs_baml", c.Want.JSON, res.JSON)
	if err != nil {
		t.Errorf("characterize %q: semantic diff: %v", c.Name, err)
		return
	}
	var failures []string
	if len(diff) > 0 {
		failures = append(failures, "want ≠ BAML (semantic)")
	}
	var orderDiff []bamlfuzz.SchemaOrderDiffEntry
	if c.PreserveSchemaOrder {
		od, oerr := bamlfuzz.SchemaOrderDiff("want_vs_baml", c.Schema, c.Want.JSON, res.JSON)
		if oerr != nil {
			failures = append(failures, fmt.Sprintf("schema order: %v", oerr))
		} else if len(od) > 0 {
			orderDiff = od
			failures = append(failures, "want ≠ BAML (order)")
		}
	}
	if len(failures) == 0 {
		return
	}
	env := &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		PrefixIndex: -1, Raw: c.Raw,
		ExpectedStatus: c.Want.Status, Expected: c.Want.JSON,
		BAML:         bamlfuzz.ParseOutcome{Parser: "baml_dynamic", JSON: res.JSON},
		SemanticDiff: diff,
		OrderDiff:    orderDiff,
		Failures:     failures,
	}
	dumpParseDiffAndFail(t, parseRecoveryArtifactDir, env, strings.Join(failures, "; "))
}

// runDynamicParseDiffLeg runs the issue-523 direct final-parse leg on an
// OracleCase: it parses the exact mock content through BAML directly,
// confirms the result matches the walker's Expected (using the same
// lenient #3690-tolerant comparators the call legs use), and diffs it
// against any registered native parser. With the default NoopParser the
// native differential is a vacuous skip, so today this leg only adds the
// BAML-direct-parse-vs-Expected anchor on top of the existing call/REST
// legs. The moment a native parser registers, the same corpus + rapid
// cases become live native-vs-BAML differentials.
func runDynamicParseDiffLeg(t *testing.T, dyn *dynclient.Client, c bamlfuzz.OracleCase, idx int) {
	t.Helper()
	t.Run("parse_diff", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		baml := bamlDynamicParser{dyn: dyn}
		native := bamlfuzz.RegisteredNativeParser()
		req := bamlfuzz.ParseRequest{
			Schema:              c.Schema,
			Raw:                 string(c.MockLLMContent),
			Stream:              false,
			PreserveSchemaOrder: c.PreserveSchemaOrder,
		}

		bamlRes, bamlErr := baml.Parse(ctx, req)
		if bamlErr != nil {
			env := dynParseDiffEnvelope(c, idx,
				bamlfuzz.ParseOutcome{Parser: baml.Name(), Error: bamlErr.Error()},
				nil, nil, []string{fmt.Sprintf("BAML direct parse errored: %v", bamlErr)})
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, fmt.Sprintf("BAML direct parse errored: %v", bamlErr))
			return
		}

		var (
			failures []string
			semDiff  []bamlfuzz.SemanticDiffEntry
			ordDiff  []bamlfuzz.SchemaOrderDiffEntry
		)
		if diff, err := bamlfuzz.SemanticDiff("expected_vs_baml_parse", c.Expected, bamlRes.JSON); err != nil {
			failures = append(failures, fmt.Sprintf("expected_vs_baml_parse diff: %v", err))
		} else if len(diff) > 0 {
			semDiff = diff
			failures = append(failures, "expected ≠ baml_parse")
		}
		if c.PreserveSchemaOrder {
			od, oerr := bamlfuzz.SchemaOrderDiffWithChoices("expected_vs_baml_parse", c.Schema, c.Expected, bamlRes.JSON, c.Metadata.UnionChoices)
			switch {
			case errors.Is(oerr, bamlfuzz.ErrSchemaOrderUnsupported):
				failures = append(failures, fmt.Sprintf("schema order unsupported: %v", oerr))
			case oerr != nil:
				failures = append(failures, fmt.Sprintf("schema order: %v", oerr))
			case len(od) > 0:
				ordDiff = od
				failures = append(failures, "expected ≠ baml_parse (order)")
			}
		}
		if len(failures) > 0 {
			env := dynParseDiffEnvelope(c, idx,
				bamlfuzz.ParseOutcome{Parser: baml.Name(), JSON: bamlRes.JSON}, semDiff, ordDiff, failures)
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, strings.Join(failures, "; "))
			return
		}

		// Native-vs-BAML differential (NoopParser → SkippedNative today).
		res := bamlfuzz.DiffParsers(ctx, baml, native, req, c.Metadata.UnionChoices)
		if !res.SkippedNative && len(res.Failures) > 0 {
			env := &bamlfuzz.ParseDiffFailureEnvelope{
				CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
				PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
				PrefixIndex: -1, Raw: string(c.MockLLMContent),
				ExpectedStatus: bamlfuzz.ParseStatusSuccess, Expected: c.Expected,
				SkippedNative: res.SkippedNative, BAML: res.BAML, Native: res.NativeOutcome(),
				SemanticDiff: res.SemanticDiff, OrderDiff: res.OrderDiff, Failures: res.Failures,
			}
			dumpParseDiffAndFail(t, dynamicOracleArtifactDir, env, strings.Join(res.Failures, "; "))
		}
	})
}

// dynParseDiffEnvelope builds a ParseDiffFailureEnvelope for the dynamic
// oracle's direct-parse leg, where BAML is compared against the walker's
// Expected rather than a native parser.
func dynParseDiffEnvelope(c bamlfuzz.OracleCase, idx int, baml bamlfuzz.ParseOutcome, semDiff []bamlfuzz.SemanticDiffEntry, ordDiff []bamlfuzz.SchemaOrderDiffEntry, failures []string) *bamlfuzz.ParseDiffFailureEnvelope {
	return &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		PrefixIndex: -1, Raw: string(c.MockLLMContent),
		ExpectedStatus: bamlfuzz.ParseStatusSuccess, Expected: c.Expected,
		BAML:         baml,
		SemanticDiff: semDiff,
		OrderDiff:    ordDiff,
		Failures:     failures,
	}
}

// logObservedOutcome prints BAML's observed parse outcome in a stable,
// greppable form so a developer running BAMLFUZZ_PARSE_CAPTURE=1 can copy
// the values into the corpus `want`. Not a gate.
func logObservedOutcome(t *testing.T, leg, raw string, res bamlfuzz.ParseResult, parseErr error) {
	t.Helper()
	if parseErr != nil {
		t.Logf("CAPTURE %s raw=%q -> status=error err=%v", leg, raw, parseErr)
		return
	}
	t.Logf("CAPTURE %s raw=%q -> status=success json=%s", leg, raw, string(res.JSON))
}

// parseDiffEnvelope builds a ParseDiffFailureEnvelope from a
// ParseDiffResult for the native-vs-BAML differential leg.
func parseDiffEnvelope(c bamlfuzz.ParseRecoveryCase, idx, prefixIdx int, prefixName, raw string, stream bool, res bamlfuzz.ParseDiffResult) *bamlfuzz.ParseDiffFailureEnvelope {
	return &bamlfuzz.ParseDiffFailureEnvelope{
		CaseIndex: idx, CaseName: c.Name, OracleMode: bamlfuzz.OracleParseDiff,
		PreserveSchemaOrder: c.PreserveSchemaOrder, Schema: c.Schema,
		Stream: stream, PrefixIndex: prefixIdx, PrefixName: prefixName, Raw: raw,
		SkippedNative: res.SkippedNative,
		BAML:          res.BAML,
		Native:        res.NativeOutcome(),
		SemanticDiff:  res.SemanticDiff,
		OrderDiff:     res.OrderDiff,
		Failures:      res.Failures,
	}
}

// dumpParseDiffAndFail writes the envelope to the given artifact dir and
// fails the test with a message pointing at the replay path.
func dumpParseDiffAndFail(t *testing.T, dir string, env *bamlfuzz.ParseDiffFailureEnvelope, msg string) {
	t.Helper()
	path, err := bamlfuzz.WriteParseDiffReplayArtifact(dir, env)
	if err != nil {
		t.Errorf("write parse-diff artifact: %v", err)
		t.Errorf("%s", msg)
		return
	}
	t.Errorf("%s\nreplay: %s", msg, path)
}
