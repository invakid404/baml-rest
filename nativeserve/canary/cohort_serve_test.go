package canary

// De-BAML serving cutover S1 — the SERVE-boundary proof: with no cohort enrolled,
// every serving surface declines PRE-SOCKET, so a native-capable artifact is
// externally equivalent to BAML transport.
//
// This is the external-equivalence proof stated in the terms the seam actually has:
// a native serve callback that returns Declined means the orchestrator runs its
// ordinary BAML path for that same request, unchanged. So "externally equivalent to
// BAML" is exactly "every surface declines, and no socket was opened doing it" —
// both of which are checked here, on every serving lane, with a transport that
// counts and refuses dials.
//
// It also carries the operational invariants the scope requires S1 to assert:
//
//	I1  a pre-claim decline has ZERO native sockets and BAML owns the request;
//	I3  a BAML same-response parse is labelled SEPARATELY from a transport fallback;
//	I4  a non-enrolled surface reporting a native CLAIM is a rollout-stop — the
//	    series exists, is pre-initialized, and stays at zero;
//	I5  flag-off produces zero native runtime/Prepare/socket observations.
//
// I2 (a native claim = exactly one native provider attempt and zero BAML provider
// attempts after it) needs a real claim over a real socket, so it lives in the
// gated cohort_claim_integration_test.go next door.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// dialCountingTransport counts RoundTrips and refuses them. Every assertion below
// that a lane opened no socket reads this counter; TestDialCounterObservesARoundTrip
// is the positive control proving a zero here is meaningful.
type dialCountingTransport struct{ n atomic.Int64 }

func (c *dialCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errNoDialPermitted
}

var errNoDialPermitted = errors.New("cohort proof: no dial is permitted before a claim")

// TestDialCounterObservesARoundTrip is the positive control: the counter DOES move
// when a RoundTrip actually happens, so "the counter stayed at zero" is evidence
// rather than a tautology.
func TestDialCounterObservesARoundTrip(t *testing.T) {
	ct := &dialCountingTransport{}
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := ct.RoundTrip(req); !errors.Is(err, errNoDialPermitted) {
		t.Fatalf("control RoundTrip err = %v", err)
	}
	if got := ct.n.Load(); got != 1 {
		t.Fatalf("control counter = %d, want 1", got)
	}
}

// proofSchema is a minimal non-nil dynamic output schema: the dynamic serve lane
// declines a nil schema BEFORE admission (a different, pre-existing gate), so the
// cohort gate would never be reached without one.
func proofSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: bamlutils.MustOrderedMap(
		bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
	)}
}

// counterValue sums a de-BAML counter family's series matching the given labels
// (a subset match: unnamed labels are wildcards). It returns -1 when the family is
// absent so a missing family is distinguishable from a zero one.
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
			if !labelsMatch(m, want) {
				continue
			}
			total += m.GetCounter().GetValue()
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

// --- I1 + the external-equivalence proof -------------------------------------

// TestEverySurfaceDeclinesPreSocketWithNoEnrollment drives all four native SERVING
// lanes THROUGH THE FACTORIES A WORKER INSTALLS and proves each one declines with
// the cohort reason, opens no socket, and records the pre-claim decline +
// BAML-transport winner on the shared registry.
//
// Driving the production factories is the point. An earlier revision constructed the
// servers directly, which a cold review caught papering over a real gap: the shipped
// dynamic-stream factory discarded its registry, so that lane emitted nothing in
// production while the test — holding a hand-built server — happily asserted series.
// Everything below now comes from NewServeFunc / NewStreamServeFunc /
// NewStaticServeFunc / NewStaticStreamServeFunc on ONE registry, exactly as
// workerboot wires them.
func TestEverySurfaceDeclinesPreSocketWithNoEnrollment(t *testing.T) {
	ctx := context.Background()
	// TWO INDEPENDENT LOCKS hold this property, and both are asserted here so a
	// mutation to EITHER one fails this test rather than only the unit test next to
	// the thing mutated:
	//
	//  1. the shipped policy enrolls nothing, so no (surface, cohort) pair is
	//     permitted; and
	//  2. the serving lanes present NO configuration identity, which resolves to the
	//     reserved cohort `none` — and a reserved cohort is non-enrollable by
	//     construction, so it stays refused even against a policy that enrolled
	//     everything.
	if got := admission.ProductionCohortGate().Policy().Len(); got != 0 {
		t.Fatalf("the shipped serving policy enrolls %d cohort(s), want 0: S1 must flip nothing native", got)
	}
	if got := admission.ProductionCohortGate().Inventory().Len(); got != 0 {
		t.Fatalf("the shipped configuration inventory declares %d record(s), want 0", got)
	}
	for surface, in := range productionServeIdentities() {
		if got := admission.ResolveCohort(surface, in); got != admission.CohortNone {
			t.Fatalf("%s presents an identity resolving to %q, want none", surface.Label(), got)
		}
	}

	// ONE registry, the four production factories, in the order workerboot installs
	// them (the unary serve registers the collectors; the rest reuse them).
	reg := prometheus.NewRegistry()
	serve, err := NewServeFunc(reg)
	if err != nil {
		t.Fatalf("NewServeFunc: %v", err)
	}
	streamServe, err := NewStreamServeFunc(reg)
	if err != nil {
		t.Fatalf("NewStreamServeFunc: %v", err)
	}
	staticServe, err := NewStaticServeFunc(reg)
	if err != nil {
		t.Fatalf("NewStaticServeFunc: %v", err)
	}
	staticStreamServe, err := NewStaticStreamServeFunc(reg)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFunc: %v", err)
	}

	// dynamic_call
	out := serve(ctx, bamlutils.NativeServeRequest{
		Provider: "openai", Mode: bamlutils.NativeServeModeCall, SingleLeaf: true,
		OutputSchema: proofSchema(),
	})
	if out.Disposition != bamlutils.NativeServeDeclined ||
		out.Stage != string(admission.StageCohort) || out.Reason != string(admission.ReasonCohortNotEnrolled) {
		t.Fatalf("dynamic_call: (%v, %s, %s), want (Declined, cohort, cohort_not_enrolled)", out.Disposition, out.Stage, out.Reason)
	}

	// dynamic_stream
	sout := streamServe(ctx, bamlutils.NativeStreamServeRequest{
		Provider: "openai", Mode: bamlutils.NativeStreamModeStream, SingleLeaf: true,
		OutputSchema: proofSchema(),
		EmitDelta:    func(bamlutils.NativeStreamDelta) error { return nil },
	})
	if sout.Disposition != bamlutils.NativeStreamDeclined ||
		sout.Stage != string(admission.StageCohort) || sout.Reason != string(admission.ReasonCohortNotEnrolled) {
		t.Fatalf("dynamic_stream: (%v, %s, %s), want (Declined, cohort, cohort_not_enrolled)", sout.Disposition, sout.Stage, sout.Reason)
	}

	// static_call
	statOut := staticServe(ctx, bamlutils.NativeStaticInvocation{
		Method: "Request.Proof", Mode: bamlutils.NativeStaticModeFinal, SingleLeaf: true, Provider: "openai",
	})
	if statOut.Disposition != bamlutils.NativeStaticServeDeclined ||
		statOut.Stage != string(admission.StageCohort) || statOut.Reason != string(admission.ReasonCohortNotEnrolled) {
		t.Fatalf("static_call: (%v, %s, %s), want (Declined, cohort, cohort_not_enrolled)", statOut.Disposition, statOut.Stage, statOut.Reason)
	}

	// static_stream
	ssout := staticStreamServe(ctx, bamlutils.NativeStaticStreamInvocation{
		Method: "Request.Proof", Mode: bamlutils.NativeStreamModeStream, SingleLeaf: true, Provider: "openai",
		EmitDelta: func(bamlutils.NativeStreamDelta) error { return nil },
	})
	if ssout.Disposition != bamlutils.NativeStreamDeclined ||
		ssout.Stage != string(admission.StageCohort) || ssout.Reason != string(admission.ReasonCohortNotEnrolled) {
		t.Fatalf("static_stream: (%v, %s, %s), want (Declined, cohort, cohort_not_enrolled)", ssout.Disposition, ssout.Stage, ssout.Reason)
	}

	// I1: no socket on any lane. The production factories own their executors, so the
	// evidence here is the socket COUNTER (zero) rather than an injected transport;
	// TestNoSocketOnAnyDeclineWithACountingTransport next door supplies the
	// independent transport-level proof over the same four lanes.
	if got := counterValue(t, reg, "baml_rest_debaml_native_sockets_total", nil); got != 0 {
		t.Fatalf("native_sockets_total = %v, want 0", got)
	}

	// ALL FOUR native serving lanes recorded a real, per-request pre-claim decline +
	// baml_transport winner through the factories a worker installs.
	for _, surface := range []admission.Surface{
		admission.SurfaceDynamicCall,
		admission.SurfaceDynamicStream,
		admission.SurfaceStaticCall,
		admission.SurfaceStaticStream,
	} {
		declineLabels := map[string]string{"surface": surface.Label(), "cohort": string(admission.CohortNone), "phase": string(admission.PhasePreclaimDecline)}
		if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", declineLabels); got != 1 {
			t.Errorf("%s: preclaim_decline phase = %v, want 1 (a REAL per-request series from the shipped factory)", surface.Label(), got)
		}
		winnerLabels := map[string]string{"surface": surface.Label(), "cohort": string(admission.CohortNone), "winner": string(admission.WinnerBAMLTransport)}
		if got := counterValue(t, reg, "baml_rest_debaml_winner_total", winnerLabels); got != 1 {
			t.Errorf("%s: baml_transport winner = %v, want 1", surface.Label(), got)
		}
		// I4: the rollout-stop series exist (pre-initialized) and stayed at zero.
		claimed := map[string]string{"surface": surface.Label(), "phase": string(admission.PhaseClaimed)}
		if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed); got != 0 {
			t.Errorf("%s: a non-enrolled surface reported %v native claim(s) — rollout-stop", surface.Label(), got)
		}
		native := map[string]string{"surface": surface.Label(), "winner": string(admission.WinnerNative)}
		if got := counterValue(t, reg, "baml_rest_debaml_winner_total", native); got != 0 {
			t.Errorf("%s: a non-enrolled surface reported %v native win(s) — rollout-stop", surface.Label(), got)
		}
	}

	// The ADMISSION collectors are live too, not just the cutover families — this is
	// the check that catches the regression the last review found empirically: a probe
	// saw the stream lane emitting phase/winner while
	// declines_total{stage=cohort,reason=cohort_not_enrolled} stayed ABSENT, because
	// the factory built its server with a nil admitter metric and patched the serving
	// recorder afterwards.
	//
	// TWO, not four, and the difference is structural rather than a gap: the DYNAMIC
	// lanes decline through *Admitter, which writes this family, while the STATIC
	// lanes decline through the package-level AdmitStatic*Claim functions, which have
	// no metrics receiver and report through their own bounded observation family
	// instead. Both static lanes' cutover phase/winner series are asserted above, so
	// no lane is unaccounted for; this counter just does not span all four.
	cohortDeclines := map[string]string{
		"stage":  string(admission.StageCohort),
		"reason": string(admission.ReasonCohortNotEnrolled),
	}
	if got := counterValue(t, reg, "baml_rest_debaml_declines_total", cohortDeclines); got != 2 {
		t.Errorf("declines_total{stage=cohort,reason=cohort_not_enrolled} = %v, want 2 (the two dynamic lanes)", got)
	}
	// Attributed per lane through the attempts family's mode label, which is what
	// actually pins the STREAM admitter as live rather than merely the pair summing to
	// two. A nil admitter on either lane makes its row absent.
	for _, mode := range []admission.Mode{admission.ModeCall, admission.ModeStream} {
		labels := map[string]string{"mode": string(mode), "outcome": string(admission.OutcomeDecline)}
		if got := counterValue(t, reg, "baml_rest_debaml_attempts_total", labels); got != 1 {
			t.Errorf("attempts_total{mode=%s,outcome=decline} = %v, want 1 — that lane's admitter has no collectors", mode, got)
		}
	}

	// The rollout-stop series stayed flat on every surface, including direct_parse,
	// which is driven through its own production route in
	// TestDirectParseRouteEmitsRealPerRequestTelemetry.
	for _, surface := range admission.AllSurfaces() {
		claimed := map[string]string{"surface": surface.Label(), "phase": string(admission.PhaseClaimed)}
		if got := counterValue(t, reg, "baml_rest_debaml_admission_phase_total", claimed); got != 0 {
			t.Errorf("%s: %v native claim(s) with nothing enrolled — rollout-stop", surface.Label(), got)
		}
	}
}

// TestNoSocketOnAnyDeclineWithACountingTransport is I1's transport-level half: the
// same four lanes, this time over an executor whose transport COUNTS and refuses
// dials, so "no socket" is evidence from the transport rather than from the counter
// the code under test writes. The production-factory sweep above owns the telemetry
// half.
func TestNoSocketOnAnyDeclineWithACountingTransport(t *testing.T) {
	ctx := context.Background()
	ct := &dialCountingTransport{}
	exec := llmhttp.NewExactExecutor(ct)
	reg := prometheus.NewRegistry()
	m, err := admission.NewMetrics(reg)
	if err != nil {
		t.Fatalf("admission.NewMetrics: %v", err)
	}

	unary := NewServer(m, exec)
	unary.Serve(ctx, bamlutils.NativeServeRequest{
		Provider: "openai", Mode: bamlutils.NativeServeModeCall, SingleLeaf: true, OutputSchema: proofSchema(),
	})
	unary.ServeStatic(ctx, bamlutils.NativeStaticInvocation{
		Method: "Request.Proof", Mode: bamlutils.NativeStaticModeFinal, SingleLeaf: true, Provider: "openai",
	})
	NewStreamServer(m, exec, time.Second, time.Second).Serve(ctx, bamlutils.NativeStreamServeRequest{
		Provider: "openai", Mode: bamlutils.NativeStreamModeStream, SingleLeaf: true,
		OutputSchema: proofSchema(), EmitDelta: func(bamlutils.NativeStreamDelta) error { return nil },
	})
	NewStaticStreamServer(time.Second, time.Second).ServeStaticStream(ctx, bamlutils.NativeStaticStreamInvocation{
		Method: "Request.Proof", Mode: bamlutils.NativeStreamModeStream, SingleLeaf: true, Provider: "openai",
		EmitDelta: func(bamlutils.NativeStreamDelta) error { return nil },
	})

	if got := ct.n.Load(); got != 0 {
		t.Fatalf("the declining lanes opened %d socket(s); a pre-claim decline must open zero", got)
	}
}

// productionServeIdentities is the configuration identity each of the four SERVING
// lanes presents when built the way a worker builds it — through the factories
// workerboot can actually reach, which take only a registry. Every one must be the
// zero identity: S1 assigns no configuration fingerprint, so every lane resolves to
// admission.CohortNone and the default-deny gate refuses it.
//
// Building the servers through the production constructors (rather than reading the
// field) is deliberate: it is the constructor a deploy profile uses that has to be
// safe, not the struct.
func productionServeIdentities() map[admission.Surface]admission.CohortInput {
	unary := NewServer(nil, nil)
	stream := NewStreamServer(nil, nil, time.Second, time.Second)
	staticStream := NewStaticStreamServer(time.Second, time.Second)
	return map[admission.Surface]admission.CohortInput{
		admission.SurfaceDynamicCall:   unary.serveCohortInput(bamlutils.NativeServeRequest{}),
		admission.SurfaceStaticCall:    unary.staticCohortInput(bamlutils.NativeStaticInvocation{}),
		admission.SurfaceDynamicStream: stream.streamCohortInput(bamlutils.NativeStreamServeRequest{}),
		admission.SurfaceStaticStream:  staticStream.toStaticStreamAdmissionInput(bamlutils.NativeStaticStreamInvocation{}).Cohort,
	}
}

// TestNoUntaggedIdentityTakingConstructor is the serve-side half of the API guard.
//
// admission's own guard proves a released consumer cannot build a gate-bearing
// CohortInput (the gate field is unexported). This proves the second door is shut
// too: no UNTAGGED exported constructor in the serve packages accepts a
// configuration identity, so a consumer cannot hand one in even if a future change
// made identities constructible. The …WithCohortIdentity constructors the gated
// proofs use live behind the `nanollm_integration` tag, which a released build does
// not set.
//
// A cold review found the first draft shipping those constructors untagged in the
// public module, next to an exported gate-override field — together a second
// admission path. This is the standing guard on both halves staying closed.
func TestNoUntaggedIdentityTakingConstructor(t *testing.T) {
	sources, scanned := untaggedServeSources(t)
	if scanned < 5 {
		t.Fatalf("scanned only %d untagged sources; the guard is not looking at the serve packages", scanned)
	}
	inspected, doors := exportedIdentityDoors(sources)
	for _, d := range doors {
		t.Errorf("%s: exported %s mentions %s in its signature in untagged source; "+
			"a released consumer could hand in a cohort identity or gate", d.file, d.name, d.mention)
	}
	// NON-VACUITY: these packages export a great deal of untagged API, so inspecting no
	// function at all means discovery broke rather than that the door is shut.
	if inspected == 0 {
		t.Fatal("no untagged exported functions were inspected; the identity-injection guard is vacuous")
	}
}

// serveSource is one parsed UNTAGGED source of the serve packages.
type serveSource struct {
	name string
	file *ast.File
}

// identityDoor is an untagged exported function that takes or returns a cohort identity
// or gate — i.e. a finding.
type identityDoor struct {
	file    string
	name    string
	mention string
}

// untaggedServeSources parses the non-test, non-tagged sources of `canary` and its parent
// `nativeserve`, returning them and the count scanned.
func untaggedServeSources(t *testing.T) (sources []serveSource, scanned int) {
	t.Helper()
	fset := token.NewFileSet()
	for _, dir := range []string{".", ".."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if isGatedByTheOptInTag(raw) {
				continue // opt-in tag: not linkable by a released consumer
			}
			scanned++
			f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			sources = append(sources, serveSource{name: path, file: f})
		}
	}
	return sources, scanned
}

// isGatedByTheOptInTag reports whether a source file is behind the `nanollm_integration`
// opt-in constraint — i.e. whether a released consumer CANNOT link it. Everything the
// guard above SKIPS rests on this.
//
// It requires the constraint to be exactly that tag rather than merely to start with it.
// A prefix test was permissive in the direction that matters: `//go:build
// nanollm_integration || something_else` starts with the same text but IS linkable without
// the tag, so treating it as gated would have excluded it from the scan. Anything this
// cannot positively recognise as gated is treated as untagged, which is the strict
// direction.
//
// admission's guard substrate carries the same helper. Go test helpers do not cross
// package boundaries, so the two copies are unavoidable — which is why each carries its
// own bite rather than trusting the other's.
func isGatedByTheOptInTag(raw []byte) bool {
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first) == "//go:build nanollm_integration"
}

// TestOptInTagDetectionIsExact is the bite for it.
func TestOptInTagDetectionIsExact(t *testing.T) {
	for _, src := range []string{
		"//go:build nanollm_integration\n\npackage canary\n",
		"//go:build nanollm_integration  \n\npackage canary\n",
	} {
		if !isGatedByTheOptInTag([]byte(src)) {
			t.Errorf("a genuinely gated file was read as untagged: %q", src)
		}
	}
	for _, src := range []string{
		"//go:build nanollm_integration || something_else\n\npackage canary\n",
		"//go:build nanollm_integration_extra\n\npackage canary\n",
		"// a comment first\n//go:build nanollm_integration\n\npackage canary\n",
		"package canary\n",
	} {
		if isGatedByTheOptInTag([]byte(src)) {
			t.Errorf("a file a released consumer can link was treated as gated (and would be "+
				"skipped by the identity-injection scan): %q", src)
		}
	}
}

// exportedIdentityDoors is THE identity-injection predicate, run by both the production
// guard above and the nested-signature bite below. It returns how many untagged exported
// functions it inspected (the non-vacuity number) and the doors it found.
//
// The signature is read from the parsed AST, not matched with a regex: a bot review
// pointed out that the `[^)]*` parameter list the first version used stops at the FIRST
// `)`, so an exported constructor taking `func(admission.CohortInput) error` — or one whose
// parameters wrap across lines — was simply invisible to it. A second bot finding was that
// the bite carried its own copy of this loop, so narrowing the guard would have left both
// green; there is one copy now.
func exportedIdentityDoors(sources []serveSource) (inspected int, doors []identityDoor) {
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			inspected++
			if hit := identityTypeInSignature(fn); hit != "" {
				doors = append(doors, identityDoor{file: src.name, name: fn.Name.Name, mention: hit})
			}
		}
	}
	return inspected, doors
}

// identityTypeInSignature returns the name of the first cohort identity/gate type
// mentioned ANYWHERE in a function's parameters or results — including inside a
// nested function type, a slice, a map or a variadic — or "" if there is none.
//
// Both the bare name and the qualified `admission.CohortInput` form count: these two
// packages sit on either side of that import boundary.
func identityTypeInSignature(fn *ast.FuncDecl) string {
	want := map[string]bool{"CohortInput": true, "CohortGate": true}
	found := ""
	inspect := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			ast.Inspect(f.Type, func(n ast.Node) bool {
				if found != "" {
					return false
				}
				switch v := n.(type) {
				case *ast.SelectorExpr: // admission.CohortInput
					if want[v.Sel.Name] {
						found = v.Sel.Name
						return false
					}
				case *ast.Ident:
					if want[v.Name] {
						found = v.Name
						return false
					}
				}
				return true
			})
		}
	}
	inspect(fn.Type.Params)
	inspect(fn.Type.Results)
	return found
}

// TestIdentityGuardSeesNestedSignatures is the BITE for the predicate above: an identity
// buried in a nested parameter type, a slice, a map, a variadic, a return position or a
// wrapped multi-line parameter list must be caught, qualified or not.
//
// It drives the REAL exportedIdentityDoors over a synthetic source, so narrowing that
// function turns this red as well — it cannot agree with the guard by construction.
func TestIdentityGuardSeesNestedSignatures(t *testing.T) {
	const synthetic = `package canary

func NestedParam(cb func(admission.CohortInput) error) {}
func SliceParam(in []admission.CohortInput)            {}
func MapParam(in map[string]*admission.CohortGate)     {}
func VariadicParam(in ...admission.CohortInput)        {}
func BareParam(in CohortInput)                         {}
func NestedResult() func() *admission.CohortGate       { return nil }
func WrappedResult() (admission.CohortInput, error)    { return admission.CohortInput{}, nil }
func MultilineParam(
	name string,
	cb func(in admission.CohortInput),
) {
}
func Clean(name string) error       { return nil }
func unexportedTakesIt(in CohortInput) {}
`
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", synthetic, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	inspected, doors := exportedIdentityDoors([]serveSource{{name: "synthetic.go", file: f}})
	if inspected == 0 {
		t.Fatal("the real predicate inspected nothing in the synthetic source; the bite is vacuous")
	}
	flagged := map[string]bool{}
	for _, d := range doors {
		flagged[d.name] = true
	}
	for _, want := range []string{
		"NestedParam", "SliceParam", "MapParam", "VariadicParam", "BareParam",
		"NestedResult", "WrappedResult", "MultilineParam",
	} {
		if !flagged[want] {
			t.Errorf("%s hands a cohort identity/gate across the package boundary and was NOT flagged", want)
		}
	}
	for _, unwanted := range []string{"Clean", "unexportedTakesIt"} {
		if flagged[unwanted] {
			t.Errorf("%s was flagged; the guard must not fire on a clean signature or an unexported function", unwanted)
		}
	}
}
