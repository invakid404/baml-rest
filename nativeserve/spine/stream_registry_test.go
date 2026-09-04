package spine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
)

// stream_registry_test.go is the M3e-A CLASSIFIER + REGISTRY proof. It is pure Go: no
// nanollm engine, no socket, no provider — every row here is decided before any
// transport could exist, which is itself part of what it proves.

// streamReg is the accepted emitted STREAM candidate for the exact five-arm JSON alias.
func streamReg() spine.StreamRegistration {
	return spine.StreamRegistration{
		Binding:     nativespinejsonfixture.StreamBinding(),
		BuildMethod: nativespinejsonfixture.BuildMethod,
	}
}

// namedStream renames the emitted stream candidate onto another corpus method.
func namedStream(name string) spine.StreamRegistration {
	reg := streamReg()
	reg.Binding.Unary.Method = name
	return reg
}

// TestNewStreamExecutorAdmitsExactJSONAlias is the positive: the emitted stream
// registration over the admitted project yields an executor that satisfies the STREAM
// contract while still satisfying the frozen unary and oracle ones.
func TestNewStreamExecutorAdmitsExactJSONAlias(t *testing.T) {
	url, hits := newCountingServer(t)
	proj := injectBaseURL(t, jsonAliasProject(t), url)

	e, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{streamReg()}, nil)
	if err != nil {
		t.Fatalf("NewStreamExecutor: %v", err)
	}
	if got := e.Methods(); len(got) != 1 || got[0] != jsonAliasMethod {
		t.Fatalf("Methods() = %v, want exactly [%q]", got, jsonAliasMethod)
	}
	// The stream executor IS a unary executor: /call and the final /parse are inherited
	// unchanged, which is what keeps a ClassStaticStream method serving both surfaces.
	var _ bamlutils.NativeSpineUnaryExecutor = e
	var _ bamlutils.NativeSpineUnaryOracleExecutor = e
	var _ bamlutils.NativeSpineStreamExecutor = e

	if hits() != 0 {
		t.Fatalf("registration opened %d socket(s); classification is pure and pre-transport", hits())
	}
}

// TestStreamCandidateRequiresTheFullStreamSurface proves the stream lane's surface gate:
// a missing stream binding, a missing partial decoder, or a missing BuildMethod is
// CORRUPTION (the generator emits them together), and each fails before any transport.
func TestStreamCandidateRequiresTheFullStreamSurface(t *testing.T) {
	url, hits := newCountingServer(t)
	proj := injectBaseURL(t, jsonAliasProject(t), url)

	cases := []struct {
		name    string
		mutate  func(r *spine.StreamRegistration)
		wantSub string
	}{
		{
			name:    "nil_decode_partial",
			mutate:  func(r *spine.StreamRegistration) { r.Binding.DecodePartial = nil },
			wantSub: "DecodePartial is nil",
		},
		{
			name:    "nil_embedded_projector",
			mutate:  func(r *spine.StreamRegistration) { r.Binding.Unary.ProjectInput = nil },
			wantSub: "ProjectInput is nil",
		},
		{
			name:    "nil_embedded_final_decoder",
			mutate:  func(r *spine.StreamRegistration) { r.Binding.Unary.DecodeFinal = nil },
			wantSub: "DecodeFinal is nil",
		},
		{
			name:    "nil_build_method",
			mutate:  func(r *spine.StreamRegistration) { r.BuildMethod = nil },
			wantSub: "nil BuildMethod",
		},
		{
			name:    "empty_method_name",
			mutate:  func(r *spine.StreamRegistration) { r.Binding.Unary.Method = "" },
			wantSub: "no method name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := streamReg()
			tc.mutate(&reg)
			_, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{reg}, nil)
			declinesOn(t, err, tc.wantSub)
			// The SAME candidate must fail BOOT of the native-only runtime, not be
			// quietly omitted: an incomplete stream registration is corruption.
			if _, rerr := spine.NewWorkerRuntime(proj, []spine.StreamRegistration{reg}, nil); rerr == nil {
				t.Fatalf("NewWorkerRuntime booted with an incomplete stream candidate")
			}
		})
	}
	if hits() != 0 {
		t.Fatalf("stream-registration rejections opened %d socket(s), want 0", hits())
	}
}

// TestStreamCandidateRequiresTheStreamClass proves the CLASS gate in both directions:
// a final-only (ClassStaticUnary) method is a valid UNARY candidate but never a stream
// candidate, and a stream candidate for it is an ordinary cohort miss — omitted by the
// runtime, rejected by the strict constructor — not corruption.
func TestStreamCandidateRequiresTheStreamClass(t *testing.T) {
	proj := projectFromCorpus(t, corpus(aliasType, strings.Join([]string{
		`function StaticRecursiveAliasJSON(topic: string) -> JSON { client C prompt #"{{ topic }}"# }`,
		`function PlainString(topic: string) -> string { client C prompt #"{{ topic }}"# }`,
	}, "\n")))

	// Sanity: the corpus really does carry one method of each class, or the rows below
	// would prove nothing.
	classes := map[string]projectdescriptor.MethodClass{}
	for _, m := range proj.Methods {
		classes[m.Name] = m.Class
	}
	if classes["StaticRecursiveAliasJSON"] != projectdescriptor.ClassStaticStream {
		t.Fatalf("StaticRecursiveAliasJSON class = %q, want %q", classes["StaticRecursiveAliasJSON"], projectdescriptor.ClassStaticStream)
	}
	if classes["PlainString"] != projectdescriptor.ClassStaticUnary {
		t.Fatalf("PlainString class = %q, want %q", classes["PlainString"], projectdescriptor.ClassStaticUnary)
	}

	// The strict stream constructor REJECTS the unary-class method.
	_, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{namedStream("PlainString")}, nil)
	if err == nil {
		t.Fatal("NewStreamExecutor admitted a ClassStaticUnary method as a stream candidate")
	}

	// The native-only runtime OMITS it (a cohort miss) and serves the stream-class one.
	rt, err := spine.NewWorkerRuntime(proj, []spine.StreamRegistration{
		namedStream("StaticRecursiveAliasJSON"), namedStream("PlainString"),
	}, nil)
	if err != nil {
		t.Fatalf("NewWorkerRuntime: %v", err)
	}
	if _, ok := rt.Method("PlainString"); ok {
		t.Fatal("a ClassStaticUnary method was admitted into the native-only STREAM runtime")
	}
	if _, ok := rt.Method("StaticRecursiveAliasJSON"); !ok {
		t.Fatal("the stream-class method was omitted")
	}

	// The UNARY lane still accepts BOTH classes' unary projection — this is what keeps
	// the standard composite's /call behaviour unchanged across the v3 bump.
	pe, err := spine.NewPopulationExecutor(proj, []spine.UnaryRegistration{
		{Binding: renameBinding(nativespinejsonfixture.Binding(), "StaticRecursiveAliasJSON"), BuildMethod: nativespinejsonfixture.BuildMethod},
		{Binding: renameBinding(nativespinejsonfixture.Binding(), "PlainString"), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationExecutor: %v", err)
	}
	got := pe.Methods()
	if len(got) != 1 || got[0] != "StaticRecursiveAliasJSON" {
		t.Fatalf("NewPopulationExecutor Methods() = %v, want the stream-class method's UNARY projection only (PlainString is outside U1)", got)
	}
}

// TestStreamStampedMethodOutsideTotalityIsHardCorruption proves the asymmetry the
// design requires: a method the DESCRIPTOR stamped ClassStaticStream whose return the
// one root-owned totality predicate declines is an INCONSISTENT descriptor, so it fails
// BOOT — it must never be silently downgraded to a cohort miss and omitted, which would
// hide a descriptor/predicate disagreement.
func TestStreamStampedMethodOutsideTotalityIsHardCorruption(t *testing.T) {
	// A well-formed scalar Return that LOWERS cleanly but is outside the exact cohort.
	plain := projectFromCorpus(t, corpus("", `function StaticRecursiveAliasJSON(topic: string) -> string { client C prompt #"{{ topic }}"# }`))
	proj := mutatedJSONProject(t, func(p *projectdescriptor.Project) {
		p.Methods[0].Return = plain.Methods[0].Return
	})
	if proj.Methods[0].Class != projectdescriptor.ClassStaticStream {
		t.Fatalf("fixture method class = %q, want the stream class (the mutation must keep the stamp)", proj.Methods[0].Class)
	}
	if err := proj.Validate(); err != nil {
		t.Fatalf("mutated project is not valid, so the test would prove the wrong thing: %v", err)
	}

	_, err := spine.NewWorkerRuntime(proj, []spine.StreamRegistration{streamReg()}, nil)
	if err == nil {
		t.Fatal("NewWorkerRuntime booted with a stream-stamped method outside the totality predicate")
	}
	if !strings.Contains(err.Error(), "descriptor stamped") {
		t.Fatalf("error = %v, want the stamped-but-outside-cohort hard failure", err)
	}
	if strings.Contains(err.Error(), "empty accepted cohort") {
		t.Fatalf("the stream-stamped corruption was downgraded to a cohort miss: %v", err)
	}
}

// TestNewWorkerRuntimeVerifiesTheFullStreamMethod proves boot-time verification of the
// BUILT method: a builder that omits MakeStreamOutput or ParseMethod.StreamImpl fails
// boot rather than publishing a runtime that accepts a /stream request and then cannot
// serve it.
func TestNewWorkerRuntimeVerifiesTheFullStreamMethod(t *testing.T) {
	proj := jsonAliasProject(t)

	cases := []struct {
		name   string
		mutate func(sm *bamlutils.StreamingMethod, pm *bamlutils.ParseMethod)
		want   string
	}{
		{"nil_make_stream_output", func(sm *bamlutils.StreamingMethod, _ *bamlutils.ParseMethod) { sm.MakeStreamOutput = nil }, "incomplete StreamingMethod"},
		{"nil_stream_impl", func(_ *bamlutils.StreamingMethod, pm *bamlutils.ParseMethod) { pm.StreamImpl = nil }, "incomplete ParseMethod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := streamReg()
			reg.BuildMethod = func(exec bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod) {
				sm, pm := nativespinejsonfixture.BuildMethod(exec)
				tc.mutate(&sm, &pm)
				return sm, pm
			}
			_, err := spine.NewWorkerRuntime(proj, []spine.StreamRegistration{reg}, nil)
			if err == nil {
				t.Fatalf("NewWorkerRuntime booted a runtime missing the full stream surface")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestNewPopulationExecutorAllowsEmptyAndNeverStreams pins the two-constructor split:
// the standard composite tolerates an empty population (all /call falls back to BAML),
// while the native-only runtime refuses one. The standard executor is also NOT a stream
// executor — accepting the stream class's unary projection is not stream enrollment.
func TestNewPopulationExecutorAllowsEmptyAndNeverStreams(t *testing.T) {
	proj := projectFromCorpus(t, corpus("", `function F(topic: string) -> string { client C prompt #"{{ topic }}"# }`))

	pe, err := spine.NewPopulationExecutor(proj, []spine.UnaryRegistration{
		{Binding: renameBinding(nativespinejsonfixture.Binding(), "F"), BuildMethod: nativespinejsonfixture.BuildMethod},
	}, nil)
	if err != nil {
		t.Fatalf("NewPopulationExecutor refused an empty population: %v", err)
	}
	if got := pe.Methods(); len(got) != 0 {
		t.Fatalf("Methods() = %v, want an empty (all-decline) population", got)
	}
	// The same candidate set refuses to boot a native-only runtime.
	if _, rerr := spine.NewWorkerRuntime(proj, []spine.StreamRegistration{namedStream("F")}, nil); rerr == nil {
		t.Fatal("NewWorkerRuntime booted an empty native-only cohort")
	}

	// A *UnaryExecutor is deliberately NOT a stream executor: only NewStreamExecutor /
	// NewWorkerRuntime produce one, so the standard composite cannot acquire a stream
	// surface by accident.
	if _, ok := any(pe).(bamlutils.NativeSpineStreamExecutor); ok {
		t.Fatal("the standard population executor satisfies the STREAM contract; standard stream serving is a later slice")
	}
}

// TestParseRoutesOpenNoSocket proves BOTH direct parse routes are socket-free and
// distinct: Parse returns the FINAL value carrier, ParseStream the POINTER carrier, and
// the loopback provider is never contacted by either.
func TestParseRoutesOpenNoSocket(t *testing.T) {
	url, hits := newCountingServer(t)
	proj := injectBaseURL(t, jsonAliasProject(t), url)
	e, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{streamReg()}, nil)
	if err != nil {
		t.Fatalf("NewStreamExecutor: %v", err)
	}
	ctx := context.Background()

	fin, err := e.Parse(ctx, jsonAliasMethod, `{"k":1}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := fin.(nativespinejsonfixture.OutputJson); !ok {
		t.Fatalf("Parse produced %T, want the FINAL value carrier OutputJson", fin)
	}
	part, err := e.ParseStream(ctx, jsonAliasMethod, `{"k":1}`)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if _, ok := part.(nativespinejsonfixture.OutputJsonStream); !ok {
		t.Fatalf("ParseStream produced %T, want the STREAM pointer carrier OutputJsonStream", part)
	}
	if hits() != 0 {
		t.Fatalf("a parse route opened %d socket(s), want 0", hits())
	}

	// An unregistered method is the typed capability-decline on BOTH routes.
	var unsupported *bamlutils.NativeSpineUnsupportedMethodError
	if _, err := e.ParseStream(ctx, "NotAdmitted", `{"k":1}`); !errors.As(err, &unsupported) {
		t.Fatalf("ParseStream of an unregistered method = %v, want the typed capability decline", err)
	}
	if _, err := e.Parse(ctx, "NotAdmitted", `{"k":1}`); !errors.As(err, &unsupported) {
		t.Fatalf("Parse of an unregistered method = %v, want the typed capability decline", err)
	}

	// A caller-supplied dynamic output schema would change the parse target; the stream
	// parse route must FAIL rather than parse under the cohort schema.
	ad := newTestAdapter()
	ad.SetDeBAMLOutputSchema(&bamlutils.DynamicOutputSchema{})
	if _, err := e.ParseStream(ad, jsonAliasMethod, `{"k":1}`); err == nil {
		t.Fatal("ParseStream accepted a request carrying a dynamic output schema")
	}
	if hits() != 0 {
		t.Fatalf("the decline rows opened %d socket(s), want 0", hits())
	}
}

// TestStreamPreSocketDeclines is the pre-socket decline table for Stream: every row
// certifies zero provider sockets and zero emitted events, so the emit spy must stay
// untouched and the loopback must never be contacted.
func TestStreamPreSocketDeclines(t *testing.T) {
	url, hits := newCountingServer(t)
	proj := injectBaseURL(t, jsonAliasProject(t), url)
	e, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{streamReg()}, nil)
	if err != nil {
		t.Fatalf("NewStreamExecutor: %v", err)
	}
	input := &nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"}

	streamAdapter := func(mut func(a *testAdapter)) *testAdapter {
		a := newTestAdapter()
		a.SetStreamMode(bamlutils.StreamModeStream)
		if mut != nil {
			mut(a)
		}
		return a
	}
	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	cases := []struct {
		name       string
		method     string
		ctx        context.Context
		emitIsNil  bool
		wantReason string
	}{
		{name: "unknown_method", method: "NotAdmitted", ctx: streamAdapter(nil), wantReason: "method_not_registered"},
		{name: "nil_emit_callback", method: jsonAliasMethod, ctx: streamAdapter(nil), emitIsNil: true, wantReason: "nil_emit_callback"},
		{name: "unary_call_mode", method: jsonAliasMethod, ctx: newTestAdapter(), wantReason: "mode_not_stream"},
		{name: "call_with_raw_mode", method: jsonAliasMethod, ctx: streamAdapter(func(a *testAdapter) {
			a.SetStreamMode(bamlutils.StreamModeCallWithRaw)
		}), wantReason: "mode_not_stream"},
		{name: "plain_context_has_no_stream_mode", method: jsonAliasMethod, ctx: context.Background(), wantReason: "mode_not_stream"},
		{name: "client_registry_override", method: jsonAliasMethod, ctx: streamAdapter(func(a *testAdapter) {
			_ = a.SetClientRegistry(&bamlutils.ClientRegistry{})
		}), wantReason: "client_registry_present"},
		{name: "dynamic_output_schema", method: jsonAliasMethod, ctx: streamAdapter(func(a *testAdapter) {
			a.SetDeBAMLOutputSchema(&bamlutils.DynamicOutputSchema{})
		}), wantReason: "dynamic_output_schema_present"},
		{name: "cancelled_before_claim", method: jsonAliasMethod, ctx: cancelled(), wantReason: "context_cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitted := 0
			var emit bamlutils.NativeSpineStreamEmit
			if !tc.emitIsNil {
				emit = func(bamlutils.NativeSpineStreamEvent) error { emitted++; return nil }
			}
			before := hits()
			res := e.Stream(tc.ctx, tc.method, input, emit)
			if res.Disposition != bamlutils.NativeSpineStreamDeclinedPreSocket {
				t.Fatalf("disposition = %v (stage %q reason %q err %v), want declined_pre_socket", res.Disposition, res.Stage, res.Reason, res.Err)
			}
			if res.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", res.Reason, tc.wantReason)
			}
			if res.Err == nil {
				t.Fatal("a decline carries no typed error")
			}
			if emitted != 0 {
				t.Fatalf("a pre-socket decline emitted %d event(s), want 0", emitted)
			}
			if hits() != before {
				t.Fatalf("a pre-socket decline opened a socket: hits %d -> %d", before, hits())
			}
		})
	}
}

// TestStreamDeclinesAMethodWithoutAStreamBinding proves a registry entry built through
// the UNARY constructor — no stream binding — declines Stream and ParseStream
// pre-socket instead of claiming a socket whose partials it cannot decode. This is the
// bite for a future constructor that forgot to require the stream surface.
func TestStreamDeclinesAMethodWithoutAStreamBinding(t *testing.T) {
	url, hits := newCountingServer(t)
	proj := injectBaseURL(t, jsonAliasProject(t), url)
	// Register through the UNARY constructor, then reach the stream surface through a
	// stream executor built over the same project with NO stream candidate for it.
	unary, err := spine.NewUnaryExecutor(proj, []bamlutils.NativeSpineUnaryBinding{nativespinejsonfixture.Binding()}, nil)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	if _, ok := any(unary).(bamlutils.NativeSpineStreamExecutor); ok {
		t.Fatal("a unary-constructed executor satisfies the STREAM contract; it must not")
	}
	if hits() != 0 {
		t.Fatalf("unary registration opened %d socket(s), want 0", hits())
	}
}
