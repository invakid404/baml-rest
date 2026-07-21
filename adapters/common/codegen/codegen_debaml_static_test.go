package codegen

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
)

// renderCode renders a bare jen.Code fragment to its Go source string.
func renderCode(c jen.Code) string { return jen.Null().Add(c).GoString() }

// TestEmitParseMethods_StaticObserveWiring pins the de-BAML Slice 8B parse-only
// observer emission: for a STATIC (non-dynamic) method, with DeBAMLStaticObserve on,
// parse_<Method> looks up the FRESH descriptor via introspected.StaticPromptDescriptor
// and calls maybeObserveDeBAMLStaticParse before falling through to BAML's Parse. It
// is NOT emitted when the opt-in is off (BAML-as-today), and it records that a static
// call was emitted so generate() writes the helper.
func TestEmitParseMethods_StaticObserveWiring(t *testing.T) {
	staticFinal := func() (widgetPlainShape, error) { return widgetPlainShape{}, nil }

	newGen := func(observe bool) *generator {
		return &generator{
			opts: Options{DeBAMLStaticObserve: observe},
			pkgs: PackageConfig{
				OutputPkgName:      "generated",
				GeneratedClientPkg: "example.com/x/baml_client",
				IntrospectedPkg:    "example.com/x/introspected",
				InterfacesPkg:      "github.com/invakid404/baml-rest/bamlutils",
			},
			intro: Introspection{
				ParseMethods: map[string]struct{}{"StaticGreet": {}},
				SyncFuncs:    map[string]any{"StaticGreet": staticFinal},
			},
			out:                  common.MakeFile(),
			selfUtilsPkg:         "example.com/x/generated/utils",
			emittedUnwrapHelpers: map[string]bool{},
		}
	}

	// Opt-in ON: the parse-only observer is wired to the descriptor lookup, and the
	// observer is resolved (deBAMLStaticObserver) BEFORE the lookup so flag-off does
	// no lookup (P1a flag-off identity).
	on := newGen(true)
	on.emitParseMethods()
	rendered := on.out.GoString()
	for _, want := range []string{
		`introspected.StaticPromptDescriptor("StaticGreet")`,
		"deBAMLStaticObserver(adapter)",
		"maybeObserveDeBAMLStaticParse(__staticObserve, __staticDescriptor)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("opt-in ON: emitted parse method missing %q\n%s", want, rendered)
		}
	}
	// The observer non-nil gate must appear BEFORE the StaticPromptDescriptor lookup
	// (no lookup when the observer is nil / flag off).
	if gi, li := strings.Index(rendered, "deBAMLStaticObserver(adapter)"), strings.Index(rendered, "StaticPromptDescriptor("); gi < 0 || li < 0 || gi > li {
		t.Errorf("opt-in ON: deBAMLStaticObserver gate (%d) must precede StaticPromptDescriptor lookup (%d)", gi, li)
	}
	if !on.emittedDeBAMLStaticCall {
		t.Error("opt-in ON: emittedDeBAMLStaticCall must be set so generate() writes the static helper")
	}

	// Opt-in OFF: no static observe wiring at all — BAML-as-today.
	off := newGen(false)
	off.emitParseMethods()
	if got := off.out.GoString(); strings.Contains(got, "maybeObserveDeBAMLStaticParse") || strings.Contains(got, "StaticPromptDescriptor") {
		t.Errorf("opt-in OFF must emit no static observe wiring\n%s", got)
	}
	if off.emittedDeBAMLStaticCall {
		t.Error("opt-in OFF: emittedDeBAMLStaticCall must stay false")
	}
}

// TestEmitBuildCallRequest_StaticObserveWiring pins the de-BAML Slice 8B FINAL
// observe emission on the generated /call BuildRequest path (driven through
// emitMethods): the observer is resolved (deBAMLStaticObserver) BEFORE the
// StaticPromptDescriptor lookup (P1a flag-off identity — no lookup when the observer
// is nil), and the helper is called with the ACTUAL selected-route facts (provider,
// clientOverride, single-leaf / fallback / round-robin shape, and the call-with-raw
// flag) plus the selected-child BAML closure (P1b truthful narrow surface).
// newCallSeamGen builds a generator whose /call BuildRequest path is switched on
// (a non-nil Request singleton) with a single static method, DeBAMLStaticObserve
// toggled by observe.
func newCallSeamGen(observe bool) *generator {
	// newMethodEmitter requires a CTX-FIRST sync func + a ParseStream counterpart.
	staticFinal := func(_ context.Context, _ string) (widgetPlainShape, error) {
		return widgetPlainShape{}, nil
	}
	staticStream := func(_ context.Context, _ string) (widgetPlainShape, error) {
		return widgetPlainShape{}, nil
	}
	return &generator{
		opts: Options{DeBAMLStaticObserve: observe, SupportsWithClient: true},
		pkgs: PackageConfig{
			OutputPkgName:      "generated",
			GeneratedClientPkg: "example.com/x/baml_client",
			IntrospectedPkg:    "example.com/x/introspected",
			InterfacesPkg:      "github.com/invakid404/baml-rest/bamlutils",
			BuildRequestPkg:    "github.com/invakid404/baml-rest/bamlutils/buildrequest",
			LLMHTTPPkg:         "github.com/invakid404/baml-rest/bamlutils/llmhttp",
			RetryPkg:           "github.com/invakid404/baml-rest/bamlutils/retry",
			SSEPkg:             "github.com/invakid404/baml-rest/bamlutils/sse",
			BamlPkg:            "example.com/x/baml_client",
			QueuePkg:           "github.com/enriquebris/goconcurrentqueue",
		},
		intro: Introspection{
			SupportsWithClient: true,
			// A non-nil Request singleton switches on the BuildRequest /call path
			// (emitBuildCallRequest); its methods are referenced by name, not reflected.
			Request:            struct{}{},
			SyncMethods:        map[string][]string{"StaticGreet": {"topic"}},
			SyncFuncs:          map[string]any{"StaticGreet": staticFinal},
			ParseMethods:       map[string]struct{}{"StaticGreet": {}},
			ParseStreamMethods: map[string]struct{}{"StaticGreet": {}},
			ParseStreamFuncs:   map[string]any{"StaticGreet": staticStream},
		},
		out:                  common.MakeFile(),
		supportsWithClient:   true,
		selfUtilsPkg:         "example.com/x/generated/utils",
		emittedUnwrapHelpers: map[string]bool{},
	}
}

func TestEmitBuildCallRequest_StaticObserveWiring(t *testing.T) {
	g := newCallSeamGen(true)
	g.emitMethods()
	rendered := g.out.GoString()

	// The observer gate must be emitted and precede the descriptor lookup.
	gate := strings.Index(rendered, "deBAMLStaticObserver(adapter)")
	lookup := strings.Index(rendered, `StaticPromptDescriptor("StaticGreet")`)
	if gate < 0 || lookup < 0 {
		t.Fatalf("final observe seam missing: gate=%d lookup=%d\n%s", gate, lookup, rendered)
	}
	if gate > lookup {
		t.Errorf("P1a: deBAMLStaticObserver gate (%d) must precede StaticPromptDescriptor lookup (%d)", gate, lookup)
	}
	// The helper call must forward the actual route facts + raw + selected-child closure.
	for _, want := range []string{
		"maybeObserveDeBAMLStaticFinal(__staticObserve, adapter, __staticDescriptor",
		`map[string]any{"topic": input.Topic}`,
		`[]string{"topic"}`,
		"provider,",
		"clientOverride,",
		"len(fallbackChain) == 0",
		"len(fallbackChain) > 0",
		"plannedMetadata != nil && plannedMetadata.RoundRobin != nil",
		"retryPolicy != nil",
		"adapter.StreamMode().NeedsRaw()",
		`buildRequestFn(ctx, clientOverride)`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("P1b: final observe emission missing %q", want)
		}
	}
}

// TestEmitBuildCallRequest_StaticObserveOptInOff pins the FINAL /call BuildRequest
// path's flag-off identity at the GENERATOR level: with DeBAMLStaticObserve off, the
// generated /call method emits NONE of the static observe wiring — no observer gate,
// no StaticPromptDescriptor lookup, no observe helper call — so a generic / customer /
// pre-0.219 adapter is byte-identical BAML-as-today (the parse-only twin is covered by
// TestEmitParseMethods_StaticObserveWiring).
func TestEmitBuildCallRequest_StaticObserveOptInOff(t *testing.T) {
	g := newCallSeamGen(false)
	g.emitMethods()
	rendered := g.out.GoString()

	for _, forbidden := range []string{
		"deBAMLStaticObserver",
		"StaticPromptDescriptor",
		"maybeObserveDeBAMLStaticFinal",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("opt-in OFF: /call emission must contain no static observe wiring, found %q", forbidden)
		}
	}
	if g.emittedDeBAMLStaticCall {
		t.Error("opt-in OFF: emittedDeBAMLStaticCall must stay false (no static helper written)")
	}
}

// TestStaticArgBinder_DeclaredOrder pins the static arg binder emission: a
// map[string]any keyed by the descriptor argument NAMES bound to the exact generated
// input-field expressions, plus the ordered []string of names in declared order.
func TestStaticArgBinder_DeclaredOrder(t *testing.T) {
	me := &methodEmitter{args: []string{"topic", "count", "flag"}}

	binder := renderCode(me.staticArgBinderMap())
	for _, want := range []string{
		"map[string]any{",
		`"topic": input.Topic`,
		`"count": input.Count`,
		`"flag":  input.Flag`,
	} {
		if !strings.Contains(binder, want) {
			t.Errorf("static arg binder missing %q\n%s", want, binder)
		}
	}

	order := renderCode(me.staticArgOrderSlice())
	if want := `[]string{"topic", "count", "flag"}`; !strings.Contains(order, want) {
		t.Errorf("static arg order slice = %q, want it to contain %q", order, want)
	}
}

// TestMaybeWriteDeBAMLStaticHelper_WritesWhenEmitted pins that the static observe
// helper is written next to adapter.go (package + import paths parameterized) with a
// valid, gofmt-parseable body when a static observe call was emitted.
func TestMaybeWriteDeBAMLStaticHelper_WritesWhenEmitted(t *testing.T) {
	dir := t.TempDir()
	g := &generator{
		pkgs: PackageConfig{
			OutputPath:      filepath.Join(dir, "adapter.go"),
			OutputPkgName:   "genx",
			InterfacesPkg:   "github.com/invakid404/baml-rest/bamlutils",
			BuildRequestPkg: "github.com/invakid404/baml-rest/bamlutils/buildrequest",
			LLMHTTPPkg:      "github.com/invakid404/baml-rest/bamlutils/llmhttp",
		},
		emittedDeBAMLStaticCall: true,
	}
	g.maybeWriteDeBAMLStaticHelper()

	b, err := os.ReadFile(filepath.Join(dir, "debaml_static.go"))
	if err != nil {
		t.Fatalf("debaml_static.go not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"package genx",
		`bamlutils "github.com/invakid404/baml-rest/bamlutils"`,
		`buildrequest "github.com/invakid404/baml-rest/bamlutils/buildrequest"`,
		`llmhttp "github.com/invakid404/baml-rest/bamlutils/llmhttp"`,
		`promptdescriptor "github.com/invakid404/baml-rest/bamlutils/promptdescriptor"`,
		"type nativeStaticObserverGetter interface {",
		"func deBAMLStaticObserver(adapter bamlutils.Adapter) bamlutils.NativeStaticObserveFunc {",
		"func maybeObserveDeBAMLStaticFinal(",
		"func maybeObserveDeBAMLStaticParse(",
		// The observation runs FIRE-AND-FORGET via the LOCAL, payload-redacted
		// panic-contained dispatch (buildrequest.ObserveStaticFireAndForget) — an
		// observer panic / its pre-socket work never blocks or crashes the caller AND
		// no descriptor/plan/credential/body payload can reach logs. Emitted for BOTH
		// the final and the parse hooks.
		"buildrequest.ObserveStaticFireAndForget(observe, inv)",
		// The final helper takes the already-resolved observer + the forwarded raw +
		// hasRetry facts.
		"observe bamlutils.NativeStaticObserveFunc",
		"hasRetry bool",
		"raw bool",
		"Raw:                     raw",
		"HasRequestRetryOverride: hasRetry",
		"WouldRewriteOrProxy:     httpClient.WouldRewriteOrProxy",
		"bamlutils.NativeStaticModeFinal",
		"bamlutils.NativeStaticModeParseOnly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted debaml_static.go missing %q\n%s", want, got)
		}
	}
	// SECURITY + ANTI-ROT: the generated helper must NOT route recovered panics to
	// go-recovery's process-global handler (recovery.Go / recovery.GoHandler), must NOT
	// format a recovered value (%+v), and must mention "go-recovery" NOWHERE — including
	// in COMMENTS, so the stale prose describing the removed unsafe mechanism can't rot
	// back in. Panic containment + payload redaction live only in
	// buildrequest.ObserveStaticFireAndForget.
	for _, forbidden := range []string{
		"recovery.Go(",
		"gregwebs/go-recovery",
		"go-recovery",
		"%+v",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("emitted debaml_static.go must not contain %q (payload-leak / global-handler risk, or stale prose)", forbidden)
		}
	}
	// The helper must be valid Go (maybeWriteDeBAMLStaticHelper gofmt's it, but parse
	// again independently so a template edit that breaks syntax fails loudly here).
	if _, err := parser.ParseFile(token.NewFileSet(), "debaml_static.go", got, parser.AllErrors); err != nil {
		t.Fatalf("emitted debaml_static.go is not valid Go: %v", err)
	}
}

// TestMaybeWriteDeBAMLStaticHelper_RemovesStaleWhenNotEmitted pins that a generation
// which emits no static observe call removes any debaml_static.go a prior generation
// left in the same output directory.
func TestMaybeWriteDeBAMLStaticHelper_RemovesStaleWhenNotEmitted(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "debaml_static.go")
	if err := os.WriteFile(stale, []byte("package genx\n"), 0o644); err != nil {
		t.Fatalf("seed stale helper: %v", err)
	}
	g := &generator{
		pkgs:                    PackageConfig{OutputPath: filepath.Join(dir, "adapter.go")},
		emittedDeBAMLStaticCall: false,
	}
	g.maybeWriteDeBAMLStaticHelper()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale debaml_static.go not removed (stat err = %v)", err)
	}
}
