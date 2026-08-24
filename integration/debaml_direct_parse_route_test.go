//go:build integration

package integration

// De-BAML native-first DYNAMIC direct parse — the DEPLOYED-ROUTE differential.
//
// # Why this test exists and what it proves that the unit tests cannot
//
// worker's own tests prove the transition oracle's LOGIC with fake legs, and
// TestBamlfuzzParseRecovery proves the native PARSER reproduces BAML over the same
// 186-case corpus by calling internal/debaml.Parse in process. Neither of them
// touches `/parse/_dynamic`. That gap is exactly where a native-first parse would
// go wrong in production: the schema has to survive the wire and the worker input
// encoding, the native parser has to be injected in the shipped binary, the
// umbrella flag has to actually reach the worker, and the payload native produces
// has to survive the host's flatten / absent-optional / ordering passes.
//
// So this test drives the real HTTP route on a real container, twice:
//
//   - a container with BAML_REST_USE_DEBAML pinned ON — the native-first path;
//   - a container with it pinned OFF — stock BAML, the reference.
//
// For every final-parse corpus case it asserts the two responses are BYTE-IDENTICAL
// (status, body, error message and error code), which is the "native never
// out-claims BAML" invariant stated in the only terms a caller can observe. Then it
// reads the ON container's own disposition counter to establish WHICH engine served
// the request, and holds that against the corpus's pinned native cut-line. The OFF
// container is checked for zero native dispositions at the end: flag-off is 100%
// BAML, not "BAML most of the time".
//
// Both legs pin the flag explicitly rather than inheriting the shared TestEnv,
// which is default-ON and could not serve as the OFF control.

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/adapters/common/codegen/bamlfuzz"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// directParseDispositionMetric is the counter the worker records one disposition
// into per native-first direct parse, as it appears in the server's combined
// exposition (the host prefixes every worker family with `bamlrest_`).
const directParseDispositionMetric = "bamlrest_debaml_direct_parse_total"

// directParseDisposition is one (engine, reason) pair from that counter. The
// `surface` label is constant for this series and is asserted rather than keyed on.
type directParseDisposition struct {
	engine string
	reason string
}

// directParseRouteTimeout bounds a single route call. A parse is local CPU work
// with no provider contact, so this is generous by an order of magnitude and only
// exists so a wedged container fails a case instead of the whole suite.
const directParseRouteTimeout = 30 * time.Second

// dedicatedDeBAMLEnv spins a baml-rest container with the umbrella flag pinned to
// flagValue and tears it down with the test. Both legs of this differential need a
// KNOWN flag value, which the shared TestEnv cannot provide.
func dedicatedDeBAMLEnv(t *testing.T, flagValue string) *testutil.TestEnvironment {
	t.Helper()

	opts := matrixSetupOptions()
	opts.RuntimeEnv = map[string]string{bamlutils.EnvUseDeBAML: flagValue}

	setupCtx, cancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer cancel()

	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("setup dedicated %s=%s env: %v", bamlutils.EnvUseDeBAML, flagValue, err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated env (%s=%s) Terminate: %v", bamlutils.EnvUseDeBAML, flagValue, err)
		}
	})
	return env
}

// readDirectParseDispositions scrapes the server and returns the direct-parse
// counter summed per (engine, reason) across every worker in the pool. A request
// may land on any worker, so the per-worker series are aggregated exactly the way
// a dashboard would.
//
// The exposition is parsed here rather than through expfmt: that parser reads a
// mutable package-global name-validation scheme in prometheus/common and panics
// when it is unset, which is exactly the state a test binary that never configured
// it is in. This series' shape is fixed and simple (one counter, four label pairs,
// no escapes), so a direct reader is both sufficient and free of that coupling.
func readDirectParseDispositions(t *testing.T, client *testutil.BAMLRestClient) map[directParseDisposition]float64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), directParseRouteTimeout)
	defer cancel()
	body, err := client.Metrics(ctx)
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}

	out := map[directParseDisposition]float64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, directParseDispositionMetric+"{") {
			continue
		}
		labels, value, ok := parseCounterSample(t, line)
		if !ok {
			continue
		}
		if got := labels["surface"]; got != "direct_parse" {
			t.Errorf("%s carries surface=%q, want the constant %q", directParseDispositionMetric, got, "direct_parse")
		}
		out[directParseDisposition{engine: labels["engine"], reason: labels["reason"]}] += value
	}
	return out
}

// parseCounterSample splits one Prometheus text-format counter sample into its
// label set and value. It reports ok=false for a line it cannot read rather than
// guessing, and fails the test rather than silently returning a zero that would
// read as "the counter did not move".
func parseCounterSample(t *testing.T, line string) (map[string]string, float64, bool) {
	t.Helper()

	open := strings.Index(line, "{")
	closeIdx := strings.LastIndex(line, "}")
	if open < 0 || closeIdx < open {
		t.Errorf("unreadable counter sample (no label set): %q", line)
		return nil, 0, false
	}
	labels := map[string]string{}
	for _, pair := range strings.Split(line[open+1:closeIdx], ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, found := strings.Cut(pair, "=")
		if !found {
			t.Errorf("unreadable label pair %q in %q", pair, line)
			return nil, 0, false
		}
		labels[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[closeIdx+1:]), 64)
	if err != nil {
		t.Errorf("unreadable counter value in %q: %v", line, err)
		return nil, 0, false
	}
	return labels, value, true
}

// dispositionDelta subtracts a previous scrape from a later one, yielding what the
// request between them recorded. Requests are issued one at a time, so the delta is
// attributable to exactly one parse.
func dispositionDelta(before, after map[directParseDisposition]float64) map[directParseDisposition]float64 {
	delta := map[directParseDisposition]float64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			delta[k] = d
		}
	}
	return delta
}

// parseRouteBody builds the `/parse/_dynamic` request body for one corpus case
// through the bamlutils types the endpoint decodes into, so the schema's
// declaration order survives to the server (the map-backed testutil schema cannot
// carry it) and preserve_schema_order is pinned explicitly rather than inherited
// from a server default.
func parseRouteBody(t *testing.T, c bamlfuzz.ParseRecoveryCase) []byte {
	t.Helper()

	lowered, err := bamlfuzz.LowerToDynamicSchema(c.Schema)
	if err != nil {
		t.Fatalf("lower case schema onto the dynamic path: %v", err)
	}
	preserve := c.PreserveSchemaOrder
	body, err := sonic.Marshal(bamlutils.DynamicParseInput{
		Raw:                 c.Raw,
		OutputSchema:        &lowered,
		PreserveSchemaOrder: &preserve,
	})
	if err != nil {
		t.Fatalf("marshal parse request body: %v", err)
	}
	return body
}

// TestDeBAMLDirectParseRouteNativeFirst is the deployed-route differential
// described at the top of this file.
func TestDeBAMLDirectParseRouteNativeFirst(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}

	corpus, err := bamlfuzz.LoadParseRecoveryCorpus(parseRecoveryCorpusDir)
	if err != nil {
		t.Fatalf("load parse recovery corpus from %s: %v", parseRecoveryCorpusDir, err)
	}
	if len(corpus) == 0 {
		t.Fatalf("parse recovery corpus at %s is empty", parseRecoveryCorpusDir)
	}

	onEnv := dedicatedDeBAMLEnv(t, "true")
	offEnv := dedicatedDeBAMLEnv(t, "false")
	on := testutil.NewBAMLRestClient(onEnv.BAMLRestURL)
	off := testutil.NewBAMLRestClient(offEnv.BAMLRestURL)

	var served, declined int
	declines := map[string]int{}
	// nativeServed records which cases the route served natively, so the named
	// fallback guard below reads the same evidence the per-case assertions did.
	nativeServed := map[string]bool{}
	// observed records every case the run actually routed. Without it the named
	// fallback guard would pass VACUOUSLY for a case that was renamed or removed
	// from the corpus: the nativeServed lookup would return false and the guard
	// would silently stop guarding.
	observed := map[string]bool{}

	prev := readDirectParseDispositions(t, on)
	for _, c := range corpus {
		if !c.HasFinal() {
			continue // streaming-only fixture: no final leg to route.
		}
		c := c
		t.Run(c.Name, func(t *testing.T) {
			observed[c.Name] = true
			body := parseRouteBody(t, c)

			ctx, cancel := context.WithTimeout(context.Background(), directParseRouteTimeout)
			defer cancel()
			onResp, err := on.DynamicParseJSON(ctx, body)
			if err != nil {
				t.Fatalf("flag-ON /parse/_dynamic: %v", err)
			}
			offResp, err := off.DynamicParseJSON(ctx, body)
			if err != nil {
				t.Fatalf("flag-OFF /parse/_dynamic: %v", err)
			}

			// THE invariant, in the only terms a caller can observe: turning the
			// umbrella flag on changed nothing about the response.
			if onResp.StatusCode != offResp.StatusCode {
				t.Errorf("status drift: flag-ON %d, flag-OFF %d", onResp.StatusCode, offResp.StatusCode)
			}
			if string(onResp.Data) != string(offResp.Data) {
				t.Errorf("body drift on %q:\n flag-ON : %s\n flag-OFF: %s", c.Raw, string(onResp.Data), string(offResp.Data))
			}
			if onResp.ErrorCode != offResp.ErrorCode {
				t.Errorf("error-code drift: flag-ON %q, flag-OFF %q", onResp.ErrorCode, offResp.ErrorCode)
			}
			if onResp.Error != offResp.Error {
				// BAML's own error TEXT is not stable across processes: its
				// match-ambiguity message enumerates the candidate values in hash
				// order, and the two containers are two processes with two hash
				// seeds. Re-running either leg reproduces that leg's own ordering,
				// so no cross-container comparison of this text can be exact.
				//
				// Rather than drop the check, compare the messages as CHARACTER
				// MULTISETS: a pure reordering of the same content passes (and is
				// logged), while any actual content change — a different value, a
				// different reason, a different scope — still fails. The bridge
				// never substitutes native's error anyway: an errored parse returns
				// BAML's own error object untouched, which the engine assertion
				// below independently confirms.
				if sortedRunes(onResp.Error) != sortedRunes(offResp.Error) {
					t.Errorf("error drift: flag-ON %q, flag-OFF %q", onResp.Error, offResp.Error)
				} else {
					t.Logf("BAML's error text differs only by ordering (its own per-process enumeration order):\n flag-ON : %s\n flag-OFF: %s", onResp.Error, offResp.Error)
				}
			}

			// Which engine served it, straight from the deployed worker's counter.
			cur := readDirectParseDispositions(t, on)
			delta := dispositionDelta(prev, cur)
			prev = cur

			total := 0.0
			for _, v := range delta {
				total += v
			}
			if total != 1 {
				t.Fatalf("the route recorded %v direct-parse dispositions for one request, want exactly 1 (delta=%v)", total, delta)
			}
			var d directParseDisposition
			for k := range delta {
				d = k
			}

			// An errored parse must never be served by native: the bridge returns
			// BAML's own error object, so the disposition has to say so.
			if onResp.StatusCode >= 400 && d.engine != "baml" {
				t.Errorf("an errored parse was attributed to engine %q; an error is always BAML's", d.engine)
			}

			switch d.engine {
			case "native":
				served++
				nativeServed[c.Name] = true
				if d.reason != "agreement" {
					t.Errorf("a natively-served parse carries reason %q, want %q — native may only win on proven agreement", d.reason, "agreement")
				}
				t.Logf("route served %q NATIVELY (%s)", c.Name, d.reason)
			case "baml":
				declined++
				declines[d.reason]++
				t.Logf("route declined %q to BAML (%s)", c.Name, d.reason)
			default:
				t.Fatalf("unknown engine label %q", d.engine)
			}
		})
	}

	t.Logf("deployed /parse/_dynamic dispositions: %d served natively, %d declined to BAML", served, declined)
	for reason, n := range declines {
		t.Logf("  decline reason %-24s %d", reason, n)
	}

	// The scoreboard, pinned as a FLOOR. Without it every assertion above would
	// still pass if native declined the entire corpus — the parity checks would be
	// trivially satisfied and the named-fallback guard only forbids over-claiming.
	// A floor (rather than an exact count) fails on regression while leaving room
	// for the burn-down this counter exists to drive.
	if served < routeNativeServeFloor {
		t.Errorf("the deployed route served %d cases natively, below the pinned floor of %d — native coverage regressed", served, routeNativeServeFloor)
	}

	// The corpus's named fallback families must still decline at the route. They are
	// the burn-down list: each one is a shape native is known not to reproduce, and
	// a case quietly flipping to native-served would mean the cut-line moved without
	// anyone deciding it should.
	for _, name := range parseRouteNamedFallbacks {
		if !observed[name] {
			t.Errorf("named fallback %q is not a final-parse case in the corpus; the guard for it is stale and guards nothing", name)
			continue
		}
		if nativeServed[name] {
			t.Errorf("named fallback %q was served NATIVELY at the route; it must decline to BAML", name)
		}
	}

	// Flag-off is zero native, not mostly-BAML: the OFF container must never have
	// recorded a single direct-parse disposition, because the bridge does not run
	// there at all.
	if offDispositions := readDirectParseDispositions(t, off); len(offDispositions) != 0 {
		t.Errorf("the flag-OFF container recorded direct-parse dispositions: %v", offDispositions)
	}
}

// routeNativeServeFloor is how many of the corpus's final-parse cases the deployed
// `/parse/_dynamic` route serves NATIVELY today: 169 of 195. The remaining 26
// decline — 22 outside the native parser's cut-line, 3 where BAML recovered a
// non-finite float that cannot be serialized at all, and 1 where both parsers
// errored and BAML's error text is the one served.
//
// The UNION burn-down moved this from 160 (of 186). Nine cases flipped, all in the
// union family: the three direct list<multi-arm-union> shapes plus the class union
// with a defaultable-collection arm, and five new fixtures that pin the mechanisms
// those needed — the array `union_variant_hint` made observable (2^53+1 through
// `list<int|float>`), its per-array reset, the map-field default, and the
// null-into-`string|map` class-field default-fill the cold review surfaced. Nothing
// was removed from the cut-line to get there: `internal/debaml` now reproduces
// BAML's cross-element hint (coerce_array.rs), its class try_cast scoring, and
// coerce_class's `default_value(Some(e))` fill for a provably-failing required
// field, so the byte comparison that gates every native serve simply started
// agreeing.
//
// Burn-down batch 1 had moved it from 151, emptying the `result_drift` bucket: the
// five cases that declined there were never semantic disagreements, only
// payload-shape ones the host's own normalization used to erase after the
// comparison — an absent optional BAML spells as `null`, and class field order.
// Both are closed at the source (internal/debaml emits the null itself;
// worker/direct_parse_schema_order.go declares the schema in the order BAML's
// TypeBuilder will be populated in), so native's worker-boundary bytes are BAML's
// bytes with nothing downstream assumed. The other four came from the lenient
// map-key family, where a key matching no enum value / literal arm is KEPT under
// its original string rather than skipped.
//
// Asserted as a floor, not an equality: raising it is the point of the burn-down,
// and a corpus that grows should not have to move this number to stay green.
const routeNativeServeFloor = 169

// parseRouteNamedFallbacks are the corpus families the native parser is known not
// to reproduce and that must therefore keep declining at the deployed route. It is
// a SUBSET of the corpus's full fallback set (parseRecoveryNativeClaim pins every
// case); these are the ones named as this slice's guarded burn-down list.
//
// This test deliberately does NOT key off parseRecoveryNativeClaim itself, because
// the two pin DIFFERENT facts. That map pins the native PARSER's cut-line — whether
// internal/debaml.Parse claims a case at all. This test observes the ROUTE's
// disposition — whether the transition oracle then found native's payload equal to
// BAML's. A case can be claimed by the parser and still decline at the route (five
// do today), so reusing the map would assert a fact this surface does not have.
// What DOES transfer is the one-directional guarantee: a case the parser declines
// can never be served natively, which is what this list checks.
//
// Batch 1 retired two of the original thirteen — `map_bad_enum_key` and
// `map_enum_key_nonmember_live_probe` — by reproducing the lenient map-key keep
// they were guarding. The UNION burn-down retires three more —
// `class_union_all_default_*`, `list_scalar_union_*` and `list_string_int_union_*`
// — by reproducing the array union_variant_hint and the defaultable-collection class
// arm; all five are served natively now and must NOT be listed (the guard would fail
// on them). Two NEW guards join in their place, both live-captured boundaries this
// slice deliberately did not cross: a JSON null against a non-nullable union with a
// COMPOSITE arm (BAML's list arm absorbs it as `[]`), and a class union arm with an
// OPTIONAL field (BAML's Class::try_cast succeeds at a non-zero score).
//
// `scalar_union_no_match_fallback` stays listed, and always will: its `want` is a
// BAML ERROR, and an errored parse is BY CONSTRUCTION never native-served — the
// bridge returns BAML's own error object (worker/direct_parse_native.go
// settleBAMLError), so no parser change can move it.
//
// The cold-review fix adds one more: `list_union_map_arm_rejects_null_stays_fallback`
// is the position where a `string|map` union's null error has nothing to default-fill
// it (a list ELEMENT), so BAML skips the item and native — unable to prove a failing
// UNION element is a BAML parse error — declines. Its class-field sibling
// (`class_field_union_map_arm_null_default_claimed`) is served NATIVELY and must NOT
// be listed. `class_union_arm_collection_class_field_stays_fallback` joins them: it is
// the union arm's optional-field boundary one level down, inside a class-valued
// collection field.
var parseRouteNamedFallbacks = []string{
	"truncated_final_error",
	"trailing_commas_nested_object_array",
	"map_partial_incomplete",
	"scalar_union_no_match_fallback",
	"literal_int_numeric_string_mismatch",
	"literal_int_hex_spelling_stays_fallback",
	"primitive_int_array_empty_stays_fallback",
	"union_null_composite_arm_stays_fallback",
	"list_union_map_arm_rejects_null_stays_fallback",
	"class_union_optional_field_arm_stays_fallback",
	"class_union_arm_collection_class_field_stays_fallback",
	"baml_error_native_fallback_guard",
}

// sortedRunes returns s's characters in sorted order, so two strings that differ
// only by ordering compare equal. Used to separate BAML's own per-process
// enumeration order from a real change in an error message's content.
func sortedRunes(s string) string {
	runes := []rune(s)
	slices.Sort(runes)
	return string(runes)
}
