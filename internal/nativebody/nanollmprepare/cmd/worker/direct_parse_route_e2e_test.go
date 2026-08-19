//go:build nanollm_integration

package main

// De-BAML serving cutover S1 — the FIFTH surface's factory-to-route proof.
//
// # The gap this closes
//
// The direct-parse observation seam was proven in halves: nativeserve's test called an
// observer built by NewDirectParseObserve but never went through the parse handler,
// and worker's test called Handler.Parse but injected a fake observer. Neither held
// the JOIN — the options literal in this binary that supplies the real factory to
// workerboot. A cold review demonstrated the consequence by deleting
// `NativeDirectParseObserveFactory: nativeserve.NewDirectParseObserve,` from that
// literal: every committed test stayed green while the fifth surface's production
// telemetry silently disappeared.
//
// So this test holds the join. It takes the REAL flag-on options this binary installs,
// builds the observer through the REAL factory those options name, injects it exactly
// as workerboot does, drives a REAL BAML parse request through the REAL
// worker.Handler.Parse route, and gathers the same registry the factory registered on.
// Removing that options field makes it fail at the first assertion.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/worker"
)

// parseOnlyRuntime is the smallest worker.Runtime that can service a /parse request:
// one parse method whose Impl echoes the raw input back. The direct-parse observation
// is about the ROUTE, not about what BAML parses, so the parser is deliberately
// trivial — and asserting its output is what proves the observation left BAML alone.
type parseOnlyRuntime struct {
	parsed *bool
}

func (r *parseOnlyRuntime) InitRuntime() {}

func (r *parseOnlyRuntime) Method(string) (bamlutils.StreamingMethod, bool) {
	return bamlutils.StreamingMethod{}, false
}

func (r *parseOnlyRuntime) ParseMethod(name string) (bamlutils.ParseMethod, bool) {
	if name != "parse-ok" {
		return bamlutils.ParseMethod{}, false
	}
	return bamlutils.ParseMethod{
		MakeOutput: func() any { return &map[string]any{} },
		Impl: func(_ bamlutils.Adapter, raw string) (any, error) {
			*r.parsed = true
			return map[string]any{"echo": raw}, nil
		},
		StreamImpl: func(_ bamlutils.Adapter, raw string) (any, error) {
			*r.parsed = true
			return map[string]any{"partial": raw}, nil
		},
	}, true
}

func (r *parseOnlyRuntime) MakeAdapter(context.Context) bamlutils.Adapter {
	return &routeAdapter{}
}

// routeAdapter is the smallest bamlutils.Adapter the parse route touches. It embeds
// the interface so the type is satisfied, and implements exactly the three setters
// Handler.Parse and configureAdapter call unconditionally; anything else the route
// might start calling panics loudly rather than being silently absorbed, which is the
// behaviour a route test wants.
//
// The root module's real adapter is generated at build time (adapter.go is a stub in
// the source tree), so a source-tree test cannot borrow it — and does not need to:
// this test is about the OBSERVATION the route emits, and the parse implementation it
// drives ignores the adapter entirely.
type routeAdapter struct {
	bamlutils.Adapter
}

func (*routeAdapter) SetLogger(bamlutils.Logger)             {}
func (*routeAdapter) SetHTTPClient(*llmhttp.Client)          {}
func (*routeAdapter) SetDeBAMLConfig(bamlutils.DeBAMLConfig) {}

// counterValue sums a de-BAML counter family's series matching the given labels.
// Returns -1 when the family is absent, so "missing" is distinguishable from "zero".
func counterValue(t *testing.T, reg *prometheus.Registry, family string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		total := 0.0
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, want) {
				total += m.GetCounter().GetValue()
			}
		}
		return total
	}
	return -1
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// streamPrefixes is the ordered accumulated-prefix sequence the parse-STREAM path is
// driven with. Each prefix produces a distinct partial, so the ORDER of the results is
// observable: dropping, duplicating, reordering or truncating any element changes the
// sequence and fails the comparison below.
var streamPrefixes = []string{"{", `{"a`, `{"a":1`, `{"a":1,"b`, `{"a":1,"b":2}`}

// parseOutcome is everything a /parse caller observes: the marshalled result bytes and
// the error. Both are compared; a route that started returning the right bytes with a
// spurious error, or vice versa, is not "unchanged".
type parseOutcome struct {
	data string
	err  string
}

func runParseSequence(t *testing.T, h *worker.Handler) (final parseOutcome, stream []parseOutcome) {
	t.Helper()
	call := func(input string) parseOutcome {
		res, err := h.Parse(context.Background(), "parse-ok", []byte(input))
		out := parseOutcome{}
		if err != nil {
			out.err = err.Error()
		}
		if res != nil {
			out.data = string(res.Data)
		}
		return out
	}
	final = call(`{"raw":"hello"}`)
	for _, prefix := range streamPrefixes {
		body, err := json.Marshal(map[string]any{"raw": prefix, "stream": true})
		if err != nil {
			t.Fatalf("marshal stream input: %v", err)
		}
		stream = append(stream, call(string(body)))
	}
	return final, stream
}

// The PINNED BAML outputs for the sequence above.
//
// These exist because a baseline-vs-candidate comparison alone is not sufficient: both
// handlers run the same worker.Parse, so a route-level corruption that mutates EVERY
// result changes both arms equally and cancels out. A cold review demonstrated exactly
// that, prefixing every parse result with `tampered:` and watching the test stay green.
// The goldens are what catch it — they are what BAML actually produced, recorded, and
// independent of the route that produces them.
const goldenFinalParse = `{"echo":"hello"}`

func goldenStreamParses() []string {
	return []string{
		`{"partial":"{"}`,
		`{"partial":"{\"a"}`,
		`{"partial":"{\"a\":1"}`,
		`{"partial":"{\"a\":1,\"b"}`,
		`{"partial":"{\"a\":1,\"b\":2}"}`,
	}
}

// TestFlagOnParseRouteEmitsDirectParseTelemetryThroughTheRealFactory is the fifth
// surface's factory-to-route proof, and the proof that observing it changes nothing.
//
// It compares THREE things, because two of them are individually insufficient:
//
//  1. native-capable-with-no-enrollment vs a BAML-ONLY control handler — catches any
//     divergence the observation itself introduces;
//  2. both of those against the PINNED BAML outputs — catches a route-level corruption
//     that would change both arms equally and cancel out of (1);
//  3. the telemetry — the fifth surface's real per-request series, and zero on the
//     control handler that installs no observer.
//
// The output comparison covers the FINAL path and the ordered STREAM sequence. An
// earlier revision asserted only `Contains(final, "hello")` and discarded the stream
// result entirely, which a review showed stayed green under both a stream-only and an
// every-result corruption.
//
// MUTATION PROOF — each of these was applied to worker/parse.go, run, and reverted;
// every one turns this test RED:
//
//  1. prefix ONLY the streaming result bytes with "tampered:"  (the review's mutation)
//  2. prefix EVERY parse result with "tampered:"               (the review's mutation)
//  3. truncate the streamed output by two bytes                (truncation)
//  4. serve a stream request from method.Impl instead of StreamImpl (reordering: the
//     observed sequence stops matching the prefixes that produced it)
//
// (1) and (3) and (4) are caught by both the control comparison and the goldens; (2) is
// caught by the goldens alone, which is precisely why the goldens are there.
func TestFlagOnParseRouteEmitsDirectParseTelemetryThroughTheRealFactory(t *testing.T) {
	// (1) The REAL flag-on options this binary hands workerboot.
	opts := serveProfileOptions()
	if opts.NativeDirectParseObserveFactory == nil {
		t.Fatal("the flag-on options install no direct-parse observer factory: the fifth surface would emit nothing in production")
	}

	// (2) The REAL factory, on a registry standing in for the worker's private one —
	// built exactly as workerboot builds it, including the fail-loud contract.
	reg := prometheus.NewRegistry()
	observe, err := opts.NativeDirectParseObserveFactory(reg)
	if err != nil {
		t.Fatalf("the direct-parse observer factory failed: %v", err)
	}
	if observe == nil {
		t.Fatal("the direct-parse observer factory returned nil without an error")
	}

	// (3) Two REAL handlers over identical runtimes: the native-capable one wired the
	// way workerboot wires it, and a BAML-ONLY control with no observer at all — the
	// same handler a flag-off or default build produces.
	nativeParsed, controlParsed := false, false
	nativeCapable, err := worker.New(worker.Config{
		Runtime:                   &parseOnlyRuntime{parsed: &nativeParsed},
		NativeDirectParseObserver: observe,
	})
	if err != nil {
		t.Fatalf("worker.New (native-capable): %v", err)
	}
	controlReg := prometheus.NewRegistry()
	if _, err := opts.NativeDirectParseObserveFactory(controlReg); err != nil {
		t.Fatalf("control registry factory: %v", err)
	}
	control, err := worker.New(worker.Config{Runtime: &parseOnlyRuntime{parsed: &controlParsed}})
	if err != nil {
		t.Fatalf("worker.New (BAML-only control): %v", err)
	}

	// (4) The REAL route, same requests through both.
	nativeFinal, nativeStream := runParseSequence(t, nativeCapable)
	controlFinal, controlStream := runParseSequence(t, control)

	if !nativeParsed || !controlParsed {
		t.Fatalf("BAML's parse implementation did not run (native=%v control=%v)", nativeParsed, controlParsed)
	}

	// (5a) EQUALITY with the BAML-only control — the observation changes nothing.
	if nativeFinal != controlFinal {
		t.Errorf("final parse differs with the observer installed:\n  native-capable %+v\n  BAML-only      %+v", nativeFinal, controlFinal)
	}
	if len(nativeStream) != len(controlStream) {
		t.Fatalf("stream sequence length differs: native-capable %d, BAML-only %d", len(nativeStream), len(controlStream))
	}
	for i := range nativeStream {
		if nativeStream[i] != controlStream[i] {
			t.Errorf("stream chunk %d differs with the observer installed:\n  native-capable %+v\n  BAML-only      %+v",
				i, nativeStream[i], controlStream[i])
		}
	}

	// (5b) EQUALITY with the pinned BAML outputs — catches a corruption that moved both
	// arms together. Byte-exact on the final result and on every element of the ordered
	// stream sequence, so a dropped, duplicated, reordered, truncated or rewritten chunk
	// all fail here.
	if nativeFinal.err != "" {
		t.Errorf("final parse errored: %s", nativeFinal.err)
	}
	if nativeFinal.data != goldenFinalParse {
		t.Errorf("final parse bytes = %q, want the pinned BAML output %q", nativeFinal.data, goldenFinalParse)
	}
	golden := goldenStreamParses()
	if len(nativeStream) != len(golden) {
		t.Fatalf("stream produced %d results, want %d — the sequence was truncated or padded", len(nativeStream), len(golden))
	}
	for i, want := range golden {
		if nativeStream[i].err != "" {
			t.Errorf("stream chunk %d errored: %s", i, nativeStream[i].err)
		}
		if nativeStream[i].data != want {
			t.Errorf("stream chunk %d bytes = %q, want the pinned BAML output %q", i, nativeStream[i].data, want)
		}
	}

	// (6) The fifth surface emitted REAL per-request series on the factory's registry:
	// one observation per request, final and stream alike.
	wantObservations := float64(1 + len(streamPrefixes))
	decline := map[string]string{
		"surface": admission.SurfaceDirectParse.Label(),
		"cohort":  string(admission.CohortNone),
		"phase":   string(admission.PhasePreclaimDecline),
	}
	if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", decline); got != wantObservations {
		t.Errorf("direct_parse preclaim_decline = %v, want %v (one per parse request, through the real factory and route)", got, wantObservations)
	}
	winner := map[string]string{
		"surface": admission.SurfaceDirectParse.Label(),
		"cohort":  string(admission.CohortNone),
		"winner":  string(admission.WinnerBAMLTransport),
	}
	if got := counterValue(t, reg, "baml_rest_debaml_winner_total", winner); got != wantObservations {
		t.Errorf("direct_parse baml_transport winner = %v, want %v", got, wantObservations)
	}
	// The BAML-only control installed no observer, so its registry saw nothing — the
	// zero-observation property a flag-off or default build has.
	if got := counterValue(t, controlReg, "baml_rest_debaml_admission_phase_total", decline); got != 0 {
		t.Errorf("the BAML-only control emitted %v direct_parse observations, want 0", got)
	}
	// And it never claims: /parse has no native path, so a claim or a socket here
	// would be a rollout-stop.
	for _, rollout := range []struct {
		family string
		labels map[string]string
	}{
		{"baml_rest_debaml_admission_phase_total", map[string]string{"surface": "direct_parse", "phase": string(admission.PhaseClaimed)}},
		{"baml_rest_debaml_winner_total", map[string]string{"surface": "direct_parse", "winner": string(admission.WinnerNative)}},
		{"baml_rest_debaml_native_sockets_total", nil},
	} {
		if got := counterValue(t, reg, rollout.family, rollout.labels); got > 0 {
			t.Errorf("direct_parse %s%v = %v, want 0", rollout.family, rollout.labels, got)
		}
	}
}

// TestFlagOffProfileInstallsNoNativeFactories is the zero-native control, asserted on
// the options themselves rather than inferred from a boot log: with the umbrella flag
// off this binary hands workerboot a build-capability advertisement and nothing else,
// so no serve callback, no observer and no runtime init exist to be invoked.
func TestFlagOffProfileInstallsNoNativeFactories(t *testing.T) {
	opts := flagOffProfileOptions()
	if !opts.NativeBuildCapable || opts.NativeEngineName == "" {
		t.Error("the flag-off profile should still advertise the STATIC build capability")
	}
	if opts.NativeCapability != nil || opts.NativeInit != nil {
		t.Error("the flag-off profile constructs native capability/runtime; it must touch no FFI")
	}
	for name, installed := range map[string]bool{
		"NativeServeFactory":              opts.NativeServeFactory != nil,
		"NativeStreamServeFactory":        opts.NativeStreamServeFactory != nil,
		"NativeStaticObserveFactory":      opts.NativeStaticObserveFactory != nil,
		"NativeStaticServeFactory":        opts.NativeStaticServeFactory != nil,
		"NativeStaticStreamServeFactory":  opts.NativeStaticStreamServeFactory != nil,
		"NativeDirectParseObserveFactory": opts.NativeDirectParseObserveFactory != nil,
		"NativeShadowFactory":             opts.NativeShadowFactory != nil,
		"NativeStaticShadowFactory":       opts.NativeStaticShadowFactory != nil,
	} {
		if installed {
			t.Errorf("the flag-off profile installs %s; flag-off must be zero native", name)
		}
	}
}

// TestServeProfileInstallsEveryDeclaredSurface pins the join the other direction: the
// flag-on options must carry a factory for every surface the cutover claims to
// observe, so a future surface cannot be declared in the telemetry contract while its
// wiring is quietly absent from the binary that ships.
func TestServeProfileInstallsEveryDeclaredSurface(t *testing.T) {
	opts := serveProfileOptions()
	for name, installed := range map[string]bool{
		"dynamic_call (NativeServeFactory)":              opts.NativeServeFactory != nil,
		"dynamic_stream (NativeStreamServeFactory)":      opts.NativeStreamServeFactory != nil,
		"static_call (NativeStaticServeFactory)":         opts.NativeStaticServeFactory != nil,
		"static_stream (NativeStaticStreamServeFactory)": opts.NativeStaticStreamServeFactory != nil,
		"direct_parse (NativeDirectParseObserveFactory)": opts.NativeDirectParseObserveFactory != nil,
	} {
		if !installed {
			t.Errorf("the flag-on serve profile is missing %s", name)
		}
	}
	if len(admission.AllSurfaces()) != 5 {
		t.Fatalf("the cutover declares %d surfaces; this test enumerates 5", len(admission.AllSurfaces()))
	}
}
