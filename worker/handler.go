package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/workerplugin"
)

// Config carries the construction-time dependencies for a Handler.
// Runtime is the only required field; the rest have nil-tolerant
// contracts described per-field below.
//
//   - Runtime is required. New returns an error when it is nil so a
//     misconfiguration surfaces at construction rather than at first
//     request.
//   - Logger nil: bridge/drain paths and the round-robin warnings become
//     silent for that handler instance.
//   - Metrics nil: New constructs a default registry with the same Go +
//     process collectors NewMetricsRegistry would have produced.
//   - ClientDefaults nil: BAML_REST_CLIENT_DEFAULTS overrides aren't
//     applied. clientdefaults.Config.Apply is nil-safe.
//   - SharedState nil: the handler logs the existing once-per-process
//     warning the first time a request tries to use round-robin shared
//     state, then falls back to the in-process Coordinator.
//   - BaseURLRewrites nil: no per-handler URL rewrites — the worker
//     skips the rewrite pass before SetClientRegistry; the per-handler
//     HTTPClient still owns outbound rewrites if it was constructed
//     with rules.
//   - HTTPClient nil: generated BuildRequest code uses
//     llmhttp.DefaultClient as the fallback (the codegen-emitted gate
//     reads adapter.HTTPClient() at dispatch time).
type Config struct {
	Runtime Runtime

	Logger         bamlutils.Logger
	Metrics        *prometheus.Registry
	ClientDefaults *clientdefaults.Config
	SharedState    SharedStateHook

	BaseURLRewrites []urlrewrite.Rule
	HTTPClient      *llmhttp.Client

	// DeBAML mirrors BAML_REST_USE_DEBAML — the umbrella switch for
	// native de-BAML behaviour (the native ctx.output_format renderer on
	// the dynamic BuildRequest route today). Zero value (disabled) keeps
	// the dynamic path BAML-as-today. Server and worker entrypoints
	// resolve it once at startup; dynclient supplies it explicitly, and
	// configureAdapter installs it on every adapter.
	DeBAML bamlutils.DeBAMLConfig

	// DeBAMLRender injects the native ctx.output_format renderer as a
	// public-typed callback. The worker module cannot import baml-rest's
	// root internal/schema + outputformat packages (a root↔worker module
	// cycle), so the root module (cmd/serve, cmd/worker, or a dynclient
	// caller via dynclient.WithDeBAMLRenderer) supplies the concrete
	// implementation. nil means the dynamic BuildRequest seam has no
	// renderer and falls back to BAML-as-today even when DeBAML.Enabled.
	DeBAMLRender bamlutils.DeBAMLRenderFunc

	// DeBAMLParse injects the native response parser as a public-typed
	// callback, the parser-side twin of DeBAMLRender. Same module-boundary
	// reason: the root module (or a dynclient caller via
	// dynclient.WithDeBAMLParser) supplies internal/debaml.Parse. nil means
	// the dynamic final-parse seam has no native parser and stays
	// BAML-as-today even when DeBAML.Enabled.
	DeBAMLParse bamlutils.DeBAMLParseFunc

	// NativeCapability is the neutral, opaque handle to this worker binary's
	// linked native-send engine (de-BAML cutover Slice 2). Non-nil ONLY in the
	// isolated BAML+nanollm worker built from the out-of-go.work nanollmprepare
	// module; the BAML-only worker and every in-process host leave it nil. It is
	// STORED (see Handler.NativeCapability) and reported at startup as a build
	// capability, but is NOT routed: the orchestrator's native child-attempt
	// callback stays nil/hard-off in this slice, so its presence changes no
	// serving behaviour. A later slice turns a present capability into an
	// installed, enabled native attempt.
	NativeCapability NativeCapability

	// NativeShadowComparator is the neutral native one-send SHADOW comparator
	// (de-BAML cutover Slice 4), injected as a public-typed callback for the same
	// module-boundary reason as DeBAMLRender/DeBAMLParse: the worker package
	// cannot import the out-of-go.work nanollm bridge, so the shadow worker's
	// entry point supplies the concrete nanollm-backed comparator. Non-nil ONLY
	// in the SHADOW deploy profile's worker; the DEFAULT production worker (and
	// the S2 native-capable worker) leave it nil, so the generated dynamic call
	// seam installs no native child-attempt callback and every request stays
	// byte-identical to today. It is installed on every adapter via the narrow
	// nativeShadowSetter interface, gated by DeBAMLConfig().Enabled at the seam.
	NativeShadowComparator bamlutils.NativeShadowFunc
}

// deBAMLRendererSetter is the narrow optional interface the adapter
// implements to receive the native render callback. Kept off the
// bamlutils.Adapter interface so test doubles and non-dynamic adapters
// need not implement it; the generated dynclient adapter does.
type deBAMLRendererSetter interface {
	SetDeBAMLRenderer(bamlutils.DeBAMLRenderFunc)
}

// deBAMLParserSetter is the parser-side twin of deBAMLRendererSetter: the
// narrow optional interface the adapter implements to receive the native
// response-parser callback. Same rationale for keeping it off
// bamlutils.Adapter.
type deBAMLParserSetter interface {
	SetDeBAMLParser(bamlutils.DeBAMLParseFunc)
}

// nativeShadowSetter is the narrow optional interface the adapter implements to
// receive the native one-send SHADOW comparator (de-BAML cutover Slice 4). Kept
// off the bamlutils.Adapter interface like the renderer/parser setters so test
// doubles and non-dynamic adapters need not implement it; the generated dynclient
// adapter does. nil comparator ⇒ nothing installed ⇒ callback hard-off.
type nativeShadowSetter interface {
	SetNativeShadowComparator(bamlutils.NativeShadowFunc)
}

// ErrRuntimeRequired is returned by New when Config.Runtime is nil.
// Surfaced as a sentinel so callers (subprocess startup, in-process
// WorkerFactory) can distinguish the misconfiguration from runtime
// errors raised later.
var ErrRuntimeRequired = errors.New("worker: Config.Runtime is required")

// Handler is the worker-side request handler extracted from
// cmd/worker/main.go. It satisfies workerplugin.Worker so the subprocess
// binary can hand it to goplugin.Serve without wrapping. Process-global
// state from the previous package-main layout (client defaults,
// shared-state client, logger, warning sync.Once values) now lives on
// the Handler so the type is constructible without hidden initialization.
type Handler struct {
	runtime        Runtime
	logger         bamlutils.Logger
	metricsReg     *prometheus.Registry
	clientDefaults *clientdefaults.Config

	baseURLRewrites []urlrewrite.Rule
	httpClient      *llmhttp.Client
	deBAML          bamlutils.DeBAMLConfig
	deBAMLRender    bamlutils.DeBAMLRenderFunc
	deBAMLParse     bamlutils.DeBAMLParseFunc

	// nativeCapability is the neutral native-send capability linked into this
	// worker binary, or nil for the BAML-only worker. Stored at construction
	// and read back via NativeCapability; never wired to the orchestrator in
	// this slice (the native child-attempt callback stays nil/hard-off).
	nativeCapability NativeCapability

	// nativeShadow is the neutral native one-send SHADOW comparator, injected only
	// in the shadow deploy profile (nil in every default build). Installed on
	// every adapter in configureAdapter; the generated dynamic call seam gates it
	// on DeBAMLConfig().Enabled and otherwise leaves the callback nil/hard-off.
	nativeShadow bamlutils.NativeShadowFunc

	sharedStateHook hookStorage

	noSharedStateWarnOnce    sync.Once
	missingRequestIDWarnOnce sync.Once
}

// Compile-time assertion that Handler satisfies the wire interface.
// Catches signature drift between workerplugin.Worker and Handler at
// build time rather than at first plugin handshake.
var _ workerplugin.Worker = (*Handler)(nil)

// New constructs a Handler from the supplied configuration. See Config
// for the nil-tolerance contract.
func New(cfg Config) (*Handler, error) {
	if cfg.Runtime == nil {
		return nil, ErrRuntimeRequired
	}
	metricsReg := cfg.Metrics
	if metricsReg == nil {
		metricsReg = NewMetricsRegistry()
	}
	h := &Handler{
		runtime:          cfg.Runtime,
		logger:           cfg.Logger,
		metricsReg:       metricsReg,
		clientDefaults:   cfg.ClientDefaults,
		baseURLRewrites:  cfg.BaseURLRewrites,
		httpClient:       cfg.HTTPClient,
		deBAML:           cfg.DeBAML,
		deBAMLRender:     cfg.DeBAMLRender,
		deBAMLParse:      cfg.DeBAMLParse,
		nativeCapability: cfg.NativeCapability,
		nativeShadow:     cfg.NativeShadowComparator,
	}
	if cfg.SharedState != nil {
		h.SetSharedStateHook(cfg.SharedState)
	}
	return h, nil
}

// NativeCapability returns the neutral native-send capability linked into this
// worker binary, or nil when the binary is BAML-only. It is the getter twin of
// Config.NativeCapability, following the render/parser storage pattern. In this
// slice callers use it only for the startup capability diagnostic; the
// orchestrator's native child-attempt callback stays nil/hard-off, so a
// non-nil capability changes no serving behaviour.
func (h *Handler) NativeCapability() NativeCapability {
	return h.nativeCapability
}

// configureAdapter installs the per-handler HTTP client and de-BAML
// config on a freshly-minted adapter. Both setters are part of the
// bamlutils.Adapter interface and are no-ops on adapter versions that
// don't honour them (HasHTTPClient=false in codegen options emits a
// no-op SetHTTPClient). The native render callback is installed through
// the narrow deBAMLRendererSetter optional interface so only adapters
// that implement it (the generated dynclient adapter) carry it; the
// callback may be nil, in which case the dynamic BuildRequest seam falls
// back to BAML-as-today. The native parser callback is installed the same
// way through the deBAMLParserSetter optional interface.
func (h *Handler) configureAdapter(adapter bamlutils.Adapter) {
	adapter.SetHTTPClient(h.httpClient)
	adapter.SetDeBAMLConfig(h.deBAML)
	if setter, ok := adapter.(deBAMLRendererSetter); ok {
		setter.SetDeBAMLRenderer(h.deBAMLRender)
	}
	if setter, ok := adapter.(deBAMLParserSetter); ok {
		setter.SetDeBAMLParser(h.deBAMLParse)
	}
	// Install the native one-send shadow comparator (nil in every default build,
	// so this is a no-op there). The generated dynamic call seam only builds a
	// native child-attempt callback when this is non-nil AND DeBAMLConfig().Enabled.
	if setter, ok := adapter.(nativeShadowSetter); ok {
		setter.SetNativeShadowComparator(h.nativeShadow)
	}
}

// CallStream executes a streaming BAML method and bridges its results
// onto the worker plugin's stream channel.
func (h *Handler) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	method, ok := h.runtime.Method(methodName)
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input — the typed input struct ignores unknown fields like __baml_options__
	input := method.MakeInput()
	if err := sonic.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options separately — only extracts __baml_options__ field.
	// This is a second pass over the same JSON. A single-pass approach would
	// require a combined struct, but the input type is generated per-method
	// and not known at compile time. The cost is minor for typical payloads.
	var options workerBamlOptions
	if err := sonic.Unmarshal(inputJSON, &options); err != nil {
		return nil, fmt.Errorf("failed to unmarshal options: %w", err)
	}

	// Create adapter and apply options
	adapter := h.runtime.MakeAdapter(ctx)
	h.configureAdapter(adapter)
	adapter.SetLogger(h.logger)
	adapter.SetStreamMode(streamMode)
	// Install a per-request round-robin Advancer that delegates to the
	// host-side SharedState store. Safe to call unconditionally: returns
	// nil when no shared-state hook is attached, and the adapter treats
	// nil as "fall back to the introspected default Coordinator".
	adapter.SetRoundRobinAdvancer(h.roundRobinAdvancerFor(ctx))
	if err := options.apply(adapter, h.clientDefaults, h.baseURLRewrites); err != nil {
		return nil, fmt.Errorf("failed to apply options: %w", err)
	}

	// Execute the method
	resultChan, err := method.Impl(adapter, input)
	if err != nil {
		return nil, fmt.Errorf("failed to call method: %w", err)
	}

	return bridgeStreamResults(ctx, resultChan, h.logger), nil
}

// Health is part of the workerplugin.Worker interface — the host calls
// it as a liveness probe. Always returns (true, nil) today; the
// subprocess process model treats a non-responsive handler the same as
// a crashed worker.
func (h *Handler) Health(ctx context.Context) (bool, error) {
	return true, nil
}
