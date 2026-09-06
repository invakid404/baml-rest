//go:build integration && nanollm_integration

package staticserve

// ExecBridge-U1s — the GENERATED BAML per-prefix oracle closure's error contract.
//
// The U1s rule is that EVERY error from `ParseStream.<Method>` is terminal for the claimed
// stream. That is not a policy choice between plausible options; it follows from how BAML
// actually signals "nothing yet", and the first test here is what pins that fact so a BAML
// upgrade cannot quietly invalidate the rule. The rest drive the REAL generated closure —
// not an injected stand-in — through the real seam.

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"

	bamlclient "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client"
	fixture "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/generated"
)

// degeneratePrefixes are the shapes a streaming prefix passes through on its way to being
// complete, plus text that never becomes valid at all. If BAML errored on ANY of them, the
// all-errors-terminal rule would turn ordinary streaming into a terminal.
var degeneratePrefixes = []string{
	``, `[`, `[1,`, `[1,"x`, `[1,"x",`,
	`{`, `{"a"`, `{"a":`, `{"a":1,`,
	`tru`, `"unterminated`, `not json at all`, `   `,
}

// TestBAMLParseStreamNeverErrorsOnAnIncompletePrefix is the EVIDENCE the U1s per-prefix rule
// rests on, kept as a maintained invariant rather than a one-time measurement.
//
// Generated BAML v0.223 reports "no partial for this prefix yet" by RETURNING a value with a
// nil error — never by erroring. It is measured here across three different return shapes
// (the exact JSON alias' pointer union, a top-level string, and a class) so the property is
// not read off one lucky carrier.
//
// WHY IT MATTERS: because BAML has no benign ERROR class, there is nothing for the generated
// oracle closure to carve out, and any error it does return is a genuine failure that has
// removed the post-claim authority. If a future BAML starts rejecting incomplete prefixes
// with an error, THIS test goes red first — and whoever sees it must give the closure a
// positive discriminator for that one benign case instead of letting ordinary streaming
// terminate.
func TestBAMLParseStreamNeverErrorsOnAnIncompletePrefix(t *testing.T) {
	fixtureInitRuntime()
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		parse func(context.Context, string) (any, error)
	}{
		{"exact JSON alias (pointer union carrier)", func(c context.Context, p string) (any, error) {
			return bamlclient.ParseStream.StaticRecursiveAliasJSON(c, p)
		}},
		{"top-level string", func(c context.Context, p string) (any, error) {
			return bamlclient.ParseStream.StaticCompletion(c, p)
		}},
		{"class return", func(c context.Context, p string) (any, error) {
			return bamlclient.ParseStream.StaticCheckedAliasedField(c, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range degeneratePrefixes {
				v, err := tc.parse(ctx, p)
				if err != nil {
					t.Errorf("ParseStream(%q) returned an error (%v); BAML signals an incomplete prefix by RETURNING a value, "+
						"so the U1s all-errors-terminal rule needs a positive discriminator for this case before it is safe", p, err)
					continue
				}
				if bamlutils.IsBAMLStreamNoValue(v) {
					t.Errorf("ParseStream(%q) returned no value; the neutral contract reads that as an authoritative BAML no-value, which SUPPRESSES the tick", p)
				}
			}
		})
	}

	// The one error shape that DOES occur, and the reason it must stay terminal: a cancelled
	// context. Note it comes back WITH a typed-nil carrier, which is exactly why the
	// no-value rule has to look past a nil interface.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := bamlclient.ParseStream.StaticRecursiveAliasJSON(cancelled, `[1,"x",true]`)
	if err == nil {
		t.Fatal("a cancelled context produced no error; the terminal classification has nothing to key on")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled-context error = %v, want context.Canceled", err)
	}
	if rv := reflect.ValueOf(v); !(rv.Kind() == reflect.Ptr && rv.IsNil()) {
		t.Logf("note: the cancelled call returned %T (not a typed-nil pointer)", v)
	}
}

// captureStreamOracleInvocation installs a serve func that CAPTURES the invocation the real
// generated seam built — including its real BAML closures — and then declines, so BAML
// serves the request and no socket is claimed on the native lane.
func captureStreamOracleInvocation(t *testing.T, corpus []string) bamlutils.NativeStaticStreamOracleInvocation {
	t.Helper()
	var captured bamlutils.NativeStaticStreamOracleInvocation
	var got atomic.Bool
	serveFn := func(_ context.Context, inv bamlutils.NativeStaticStreamOracleInvocation, _ bamlutils.NativeSpineStreamEmit) bamlutils.NativeStaticStreamOracleServeResult {
		captured = inv
		got.Store(true)
		return bamlutils.NativeStaticStreamOracleServeResult{
			Disposition: bamlutils.NativeStaticStreamOracleDeclined,
			Stage:       "registry", Reason: "method_not_registered",
		}
	}
	server := newFixtureStreamServer(t, corpus)
	a := buildFixtureStreamOracleAdapter(t, serveFn, true, bamlutils.StreamModeStream)
	ch, err := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, err)
	server.close()
	if tr.drainer != nil {
		t.Fatalf("the declined leg failed: %v", tr.drainer)
	}
	if !got.Load() {
		t.Fatal("the generated seam never invoked the oracle serve callback; nothing was captured")
	}
	if captured.BAMLStreamParse == nil || captured.BAMLFinalParse == nil {
		t.Fatal("the generated seam supplied no BAML oracle closures")
	}
	return captured
}

// TestGeneratedBAMLPrefixClosurePropagatesAGenuineError drives the REAL generated closure
// (the one production installs, not an injected stand-in) and pins BOTH halves of its
// contract: an incomplete prefix is a VALUE, and a genuine failure is an ERROR — never a
// silent no-value that would leave the claimed stream running without its authority.
func TestGeneratedBAMLPrefixClosurePropagatesAGenuineError(t *testing.T) {
	inv := captureStreamOracleInvocation(t, oracleStreamCorpus())

	// Incomplete prefixes: a value, no error.
	for _, p := range []string{`[`, `[1,`, `[1,"x`} {
		res, err := inv.BAMLStreamParse(context.Background(), p)
		if err != nil {
			t.Errorf("the generated closure errored on the incomplete prefix %q: %v", p, err)
		}
		if !res.HasValue {
			t.Errorf("the generated closure reported no value for %q; that would SUPPRESS the tick", p)
		}
	}

	// A genuine failure: an error, and NOT a value-less success.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := inv.BAMLStreamParse(cancelled, `[1,"x",true]`)
	if err == nil {
		t.Fatal("the generated closure SWALLOWED a genuine BAML failure as a no-value; the claimed stream would keep emitting with no post-claim authority")
	}
	if res.HasValue {
		t.Error("the generated closure returned both an error and a value")
	}
}

// TestGeneratedBAMLPrefixClosureErrorIsOnePostClaimTerminal joins the two halves: a genuine
// error out of the REAL generated closure, mid-stream on a CLAIMED stream, must produce
// exactly ONE terminal and stop the transcript dead — no further structured frame, no
// further raw, no final.
//
// The failure is injected at the BAML boundary (the closure is handed a cancelled context
// from the second structured tick) rather than by substituting a fake closure, so the
// generated normalization is what decides the outcome. The stream's own transport context
// stays alive, so the terminal is attributable to the oracle and not to the transport.
func TestGeneratedBAMLPrefixClosureErrorIsOnePostClaimTerminal(t *testing.T) {
	fixtureInitRuntime()
	dir := fixtureBamlSrcDir()
	sources := readBamlSources(t, dir)
	proj, err := nativespine.BuildFromSource(sources)
	if err != nil {
		t.Fatalf("BuildFromSource(fixture baml_src): %v", err)
	}
	exec, err := spine.NewPopulationStreamExecutor(proj, []spine.StreamRegistration{
		{Binding: nativespinejsonfixture.StreamBinding(), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor: %v", err)
	}
	inner, err := standardspineoracle.NewStaticStreamServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var ticks atomic.Int32
	// The REAL generated closure, handed a cancelled context from the SECOND structured
	// tick onward. What it does with BAML's resulting error is the thing under test.
	serveFn := func(ctx context.Context, invocation bamlutils.NativeStaticStreamOracleInvocation, emit bamlutils.NativeSpineStreamEmit) bamlutils.NativeStaticStreamOracleServeResult {
		generated := invocation.BAMLStreamParse
		invocation.BAMLStreamParse = func(pctx context.Context, prefix string) (bamlutils.BAMLStreamPrefixResult, error) {
			if ticks.Add(1) >= 2 {
				pctx = cancelled
			}
			return generated(pctx, prefix)
		}
		return inner(ctx, invocation, emit)
	}

	server := newFixtureStreamServer(t, oracleStreamCorpus())
	a := buildFixtureStreamOracleAdapter(t, serveFn, true, bamlutils.StreamModeStreamWithRaw)
	ch, cerr := fixture.StaticRecursiveAliasJSON(a, &fixture.StaticRecursiveAliasJsonInput{Topic: "arbitrary json"})
	tr := drainStreamTrace(t, ch, cerr)
	hits := server.count.Load()
	server.close()

	if tr.drainer == nil {
		t.Fatal("a genuine BAML per-prefix failure did not terminate the claimed stream")
	}
	if hits != 1 {
		t.Errorf("the provider saw %d request(s), want exactly 1 — a post-claim terminal never resends", hits)
	}
	if snap := exec.Metrics().Snapshot(); snap.Claims != 1 || snap.Failures != 1 || snap.Successes != 0 {
		t.Errorf("executor counters = %+v, want exactly one claim and one failure", snap)
	}

	// The transcript stops dead: no final, and nothing published after the terminal.
	terminalAt := -1
	for i, e := range tr.events {
		if e.kind == bamlutils.StreamResultKindFinal {
			t.Errorf("a terminated stream published a final at event %d", i)
		}
		if e.kind == bamlutils.StreamResultKindError {
			terminalAt = i
		}
	}
	if terminalAt < 0 {
		t.Fatal("no terminal error frame was published")
	}
	for i := terminalAt + 1; i < len(tr.events); i++ {
		e := tr.events[i]
		if e.kind == bamlutils.StreamResultKindStream {
			t.Errorf("event %d (%v, raw=%q) was published AFTER the terminal; losing the oracle must stop raw as well as structured output",
				i, e.kind, e.raw)
		}
	}
	// The oracle failed on the SECOND tick, so at most one structured frame may have been
	// released — a lane that kept streaming would show more.
	structured := 0
	for _, e := range tr.events {
		if e.kind == bamlutils.StreamResultKindStream && e.partial != "" {
			structured++
		}
	}
	if structured > 1 {
		t.Errorf("%d structured frame(s) were released though the oracle failed on the second tick; the comparison must precede every emit", structured)
	}
}

// fixtureStreamServerCorpusHTTP is a small marker so the file's http import is used by the
// adapter wiring below; the fixture's client transport is what needs it.
var _ = http.MethodPost
