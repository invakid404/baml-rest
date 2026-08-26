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
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
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

	// TrustedClients is the deployment's APPROVED-CONFIGURATION declaration
	// (BAML_REST_DEBAML_TRUSTED_CLIENTS, de-BAML serving cutover S3a). It is the
	// only thing that can seal a request's client as deployment-owned, which is
	// the only thing that can give it a configuration identity at the native
	// admission seam.
	//
	// Nil (or empty) is the shipped default: nothing is sealed, no request is
	// altered, and no request obtains an identity. A MALFORMED declaration is a
	// boot failure at the entrypoint that loads it, never a silent empty set.
	TrustedClients *trustedclients.Set

	BaseURLRewrites []urlrewrite.Rule
	HTTPClient      *llmhttp.Client

	// SoftFinalParse is the per-handler opt-in (dynclient.WithSoftFinalParse)
	// that softens a final structured-parse miss on a raw-wanted STREAM
	// (/stream-with-raw, StreamModeStreamWithRaw) into a successful raw-only
	// final instead of a hard error. Zero value (false) keeps the strict
	// final parse. configureAdapter installs it on every adapter; the
	// orchestrator gates it on NeedsPartials && NeedsRaw (streaming only) and
	// never applies it to a cancellation/deadline. dynclient supplies it
	// explicitly; server/worker entrypoints leave it false today.
	SoftFinalParse bool

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

	// NativeServeComparator is the neutral native SERVE implementation (de-BAML
	// cutover Slice 6), injected as a public-typed callback for the same
	// module-boundary reason as NativeShadowComparator. Non-nil ONLY in the SERVE
	// deploy profile's worker with the umbrella flag on; the DEFAULT production
	// worker, the S2 native-capable worker, the SHADOW worker, and every flag-off
	// build leave it nil, so the generated dynamic call seam installs no native
	// child-attempt callback and every request stays byte-identical to today. It
	// is installed on every adapter via the narrow nativeServeSetter interface,
	// gated by DeBAMLConfig().Enabled at the seam. A worker MUST NOT supply BOTH a
	// serve and a shadow comparator — the entry point enforces that mutual
	// exclusion (workerboot fails startup if both factories are set), so the
	// generated installer only ever sees one.
	NativeServeComparator bamlutils.NativeServeFunc

	// NativeStreamServeComparator is the neutral native STREAM SERVE implementation
	// (de-BAML Phase 7D), the streaming twin of NativeServeComparator. Non-nil ONLY
	// in the SERVE deploy profile's worker with the umbrella flag on; every other
	// build leaves it nil, so the generated dynamic StreamRequest seam installs no
	// native stream callback and every streamed request stays byte-identical to
	// today. It is installed on every adapter via the narrow nativeStreamServeSetter
	// interface, gated by DeBAMLConfig().Enabled at the seam. It coexists with
	// NativeServeComparator (the serve profile installs both unary + stream); it is
	// NOT installed in the shadow profile, so it does not participate in the
	// serve/shadow mutual exclusion.
	NativeStreamServeComparator bamlutils.NativeStreamServeFunc

	// NativeStaticObserver is the neutral native STATIC no-send admission OBSERVER
	// (de-BAML Slice 8B). Non-nil ONLY in a native worker with the umbrella flag on;
	// the BAML-only worker and every flag-off build leave it nil, so the generated
	// static seam's observer callback stays nil/hard-off and every request stays
	// byte-identical to today. It is installed on every adapter via the narrow
	// nativeStaticObserverSetter interface, gated by DeBAMLConfig().Enabled at the
	// seam. It is OBSERVE-ONLY (always declines), so it does NOT participate in the
	// serve/shadow mutual exclusion — a native worker may install it alongside the
	// serve or shadow callback, or on its own.
	NativeStaticObserver bamlutils.NativeStaticObserveFunc

	// NativeStaticServeComparator is the neutral native STATIC SERVE implementation
	// (de-BAML Slice 8C). Non-nil ONLY in a SERVE-profile worker with the umbrella
	// flag on; every default/shadow/flag-off build leaves it nil, so the generated
	// static /call seam's serve callback stays nil/hard-off and the request stays
	// byte-identical to today. It is installed on every adapter via the narrow
	// nativeStaticServeSetter interface, gated by DeBAMLConfig().Enabled at the seam.
	// It SERVES admitted static unary /call (one send, tri-state pre-claim decline to
	// BAML), coexisting with the dynamic serve/stream callbacks; it is the static
	// twin of NativeServeComparator.
	NativeStaticServeComparator bamlutils.NativeStaticServeFunc

	// NativeStaticStreamServeComparator is the neutral native STATIC STREAM SERVE
	// implementation (de-BAML Phase 3b). Non-nil ONLY in a SERVE-profile worker with the
	// umbrella flag on; every default/shadow/flag-off build leaves it nil, so the
	// generated static /stream{,-with-raw} seam's serve callback stays nil/hard-off and
	// the request stays byte-identical to today. Installed on every adapter via the
	// narrow nativeStaticStreamServeSetter interface, gated by DeBAMLConfig().Enabled at
	// the seam. It SERVES admitted static streams (one DoStream RoundTrip, tri-state
	// pre-transport decline to BAML); it is the streaming twin of
	// NativeStaticServeComparator.
	NativeStaticStreamServeComparator bamlutils.NativeStaticStreamServeFunc

	// NativeDirectParseObserver is the neutral DIRECT-PARSE observation sink (de-BAML
	// serving cutover S1). Non-nil ONLY in a native-capable worker with the umbrella
	// flag on; every default/flag-off build leaves it nil and Parse calls nothing.
	//
	// It is a telemetry sink, not a serving seam: Parse reports each `/parse/{method}`
	// request to it and then runs BAML exactly as before, ignoring anything the
	// observer does. It exists because `direct_parse` is one of the cutover's five
	// declared surfaces and is the only one that never reaches native admission, so
	// without it that endpoint class would have no per-request evidence of who owns
	// it. See bamlutils.NativeDirectParseObserveFunc.
	NativeDirectParseObserver bamlutils.NativeDirectParseObserveFunc

	// NativeStaticShadowComparator is the neutral native STATIC Stage-1 SHADOW
	// comparator (de-BAML Slice 8C). Non-nil ONLY in a SHADOW-profile worker with the
	// umbrella flag on; every default/serve/flag-off build leaves it nil. It runs the
	// no-send admission + plan compare and, on a match, compares native's parse of
	// BAML's captured bytes against BAML's parse — with ZERO native sends — then
	// declines so BAML serves. Installed on every adapter via the narrow
	// nativeStaticShadowSetter interface. It is the static twin of
	// NativeShadowComparator; the generated /call seam PREFERS serve over shadow over
	// observe.
	NativeStaticShadowComparator bamlutils.NativeStaticShadowFunc
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

// softFinalParseSetter is the narrow optional interface the adapter
// implements to receive the per-handler soft-final opt-in
// (dynclient.WithSoftFinalParse). Kept OFF bamlutils.Adapter — only the
// getter SoftFinalParse() is on the interface (the generated stream router
// reads it) — so minimal adapter doubles that only exercise non-streaming
// routes (e.g. the direct-parse route double, which embeds bamlutils.Adapter
// and implements only the setters it uses) need not implement a setter
// configureAdapter would otherwise dispatch on their nil embed. The generated
// dynclient/static adapters implement it.
type softFinalParseSetter interface {
	SetSoftFinalParse(bool)
}

// nativeShadowSetter is the narrow optional interface the adapter implements to
// receive the native one-send SHADOW comparator (de-BAML cutover Slice 4). Kept
// off the bamlutils.Adapter interface like the renderer/parser setters so test
// doubles and non-dynamic adapters need not implement it; the generated dynclient
// adapter does. nil comparator ⇒ nothing installed ⇒ callback hard-off.
type nativeShadowSetter interface {
	SetNativeShadowComparator(bamlutils.NativeShadowFunc)
}

// nativeServeSetter is the serve-side twin of nativeShadowSetter: the narrow
// optional interface the adapter implements to receive the native SERVE
// implementation (de-BAML cutover Slice 6). Same rationale for keeping it off
// bamlutils.Adapter. nil implementation ⇒ nothing installed ⇒ callback hard-off.
type nativeServeSetter interface {
	SetNativeServeComparator(bamlutils.NativeServeFunc)
}

// nativeStreamServeSetter is the streaming twin of nativeServeSetter: the narrow
// optional interface the adapter implements to receive the native STREAM SERVE
// implementation (de-BAML Phase 7D). Kept off bamlutils.Adapter for the same
// reason. nil implementation ⇒ nothing installed ⇒ stream callback hard-off.
type nativeStreamServeSetter interface {
	SetNativeStreamServeComparator(bamlutils.NativeStreamServeFunc)
}

// nativeStaticObserverSetter is the STATIC observe-only twin of the native
// setters (de-BAML Slice 8B): the narrow optional interface the adapter implements
// to receive the native static no-send admission OBSERVER. Kept off the
// bamlutils.Adapter interface for the same reason as the renderer/parser/native
// setters so test doubles and non-static adapters need not implement it; the
// generated adapter does. nil observer ⇒ nothing installed ⇒ static seam hard-off.
type nativeStaticObserverSetter interface {
	SetNativeStaticObserver(bamlutils.NativeStaticObserveFunc)
}

// nativeStaticServeSetter is the SERVE twin of nativeStaticObserverSetter (de-BAML
// Slice 8C): the narrow optional interface the adapter implements to receive the
// native static SERVE implementation. Kept off the bamlutils.Adapter interface for
// the same reason as the other native setters. nil implementation ⇒ nothing
// installed ⇒ static /call serve seam hard-off.
type nativeStaticServeSetter interface {
	SetNativeStaticServeComparator(bamlutils.NativeStaticServeFunc)
}

// nativeStaticStreamServeSetter is the STREAMING twin of nativeStaticServeSetter
// (de-BAML Phase 3b): the narrow optional interface the adapter implements to receive
// the native static STREAM SERVE implementation. Kept off the bamlutils.Adapter
// interface for the same reason as the other native setters. nil implementation ⇒
// nothing installed ⇒ static /stream serve seam hard-off.
type nativeStaticStreamServeSetter interface {
	SetNativeStaticStreamServeComparator(bamlutils.NativeStaticStreamServeFunc)
}

// nativeStaticShadowSetter is the SHADOW twin of nativeStaticServeSetter (de-BAML
// Slice 8C Stage-1): the narrow optional interface the adapter implements to receive
// the native static SHADOW comparator. Kept off the bamlutils.Adapter interface for
// the same reason as the other native setters. nil comparator ⇒ nothing installed ⇒
// static /call shadow seam hard-off.
type nativeStaticShadowSetter interface {
	SetNativeStaticShadowComparator(bamlutils.NativeStaticShadowFunc)
}

// ErrRuntimeRequired is returned by New when Config.Runtime is nil.
// Surfaced as a sentinel so callers (subprocess startup, in-process
// WorkerFactory) can distinguish the misconfiguration from runtime
// errors raised later.
var ErrRuntimeRequired = errors.New("worker: Config.Runtime is required")

// ErrNativeCallbackConflict is returned by New when BOTH a serve and a shadow
// comparator are supplied. A worker installs AT MOST ONE native child-attempt
// callback; workerboot already enforces this at the factory level, but Config is
// public so New re-validates it so a direct or dynclient caller cannot bypass the
// invariant (which would otherwise silently rely on the generated installer's
// serve-precedence tie-break).
var ErrNativeCallbackConflict = errors.New("worker: NativeServeComparator and NativeShadowComparator are mutually exclusive")

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
	// trustedClients is the deployment's approved-configuration declaration
	// (de-BAML serving cutover S3a). Nil / empty on every deployment that
	// declared none, which seals nothing and alters no request.
	trustedClients *trustedclients.Set

	baseURLRewrites []urlrewrite.Rule
	httpClient      *llmhttp.Client
	deBAML          bamlutils.DeBAMLConfig
	deBAMLRender    bamlutils.DeBAMLRenderFunc
	deBAMLParse     bamlutils.DeBAMLParseFunc

	// softFinalParse mirrors Config.SoftFinalParse — the per-handler
	// soft-final opt-in installed on every adapter in configureAdapter.
	softFinalParse bool

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

	// nativeServe is the neutral native SERVE implementation, injected only in the
	// serve deploy profile with the flag on (nil in every default/shadow/flag-off
	// build). Installed on every adapter in configureAdapter; the generated dynamic
	// call seam gates it on DeBAMLConfig().Enabled and otherwise leaves the callback
	// nil/hard-off. Mutually exclusive with nativeShadow at the entry point.
	nativeServe bamlutils.NativeServeFunc

	// nativeStreamServe is the neutral native STREAM SERVE implementation (de-BAML
	// Phase 7D), injected only in the serve deploy profile with the flag on (nil in
	// every default/shadow/flag-off build). Installed on every adapter in
	// configureAdapter; the generated dynamic StreamRequest seam gates it on
	// DeBAMLConfig().Enabled and otherwise leaves the stream callback nil/hard-off.
	nativeStreamServe bamlutils.NativeStreamServeFunc

	// nativeStaticObserver is the neutral native STATIC no-send admission observer
	// (de-BAML Slice 8B), injected only in a native worker with the flag on (nil in
	// every default/flag-off build). Installed on every adapter in configureAdapter;
	// the generated static seam gates it on DeBAMLConfig().Enabled and otherwise
	// leaves the static observer callback nil/hard-off. OBSERVE-ONLY: it always
	// declines, so it never changes what BAML serves.
	nativeStaticObserver bamlutils.NativeStaticObserveFunc

	// nativeStaticServe is the neutral native STATIC SERVE implementation (de-BAML
	// Slice 8C), injected only in the SERVE deploy profile with the flag on (nil in
	// every default/shadow/flag-off build). Installed on every adapter in
	// configureAdapter; the generated static /call seam gates it on
	// DeBAMLConfig().Enabled and otherwise leaves the serve callback nil/hard-off.
	nativeStaticServe bamlutils.NativeStaticServeFunc

	// nativeStaticStreamServe is the neutral native STATIC STREAM SERVE implementation
	// (de-BAML Phase 3b), injected only in the SERVE deploy profile with the flag on
	// (nil in every default/shadow/flag-off build). Installed on every adapter in
	// configureAdapter; the generated static /stream{,-with-raw} seam gates it on
	// DeBAMLConfig().Enabled and otherwise leaves the serve callback nil/hard-off.
	nativeStaticStreamServe bamlutils.NativeStaticStreamServeFunc

	// nativeStaticShadow is the neutral native STATIC Stage-1 SHADOW comparator
	// (de-BAML Slice 8C), injected only in the SHADOW deploy profile with the flag on
	// (nil in every default/serve/flag-off build). Installed on every adapter in
	// configureAdapter; the generated static /call seam gates it on
	// DeBAMLConfig().Enabled + serve-absent, otherwise leaving it nil/hard-off.
	nativeStaticShadow bamlutils.NativeStaticShadowFunc

	// nativeDirectParseObserver is the neutral DIRECT-PARSE observation sink, injected
	// only in a native-capable worker with the umbrella flag on (nil in every
	// default/flag-off build). Parse reports to it and ignores the result; it can
	// never change what BAML parses. See bamlutils.NativeDirectParseObserveFunc.
	nativeDirectParseObserver bamlutils.NativeDirectParseObserveFunc

	// directParseMetrics is the native-first direct-parse burn-down counter,
	// registered on metricsReg at construction. It is recorded ONLY from inside the
	// native-first bridge, so a flag-off / BAML-only worker never touches it and the
	// series stays absent there. See direct_parse_metrics.go.
	directParseMetrics *directParseMetrics

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
	// A worker installs at most one native child-attempt callback. Reject both
	// BEFORE storing anything so an invalid config yields no handler.
	if cfg.NativeServeComparator != nil && cfg.NativeShadowComparator != nil {
		return nil, ErrNativeCallbackConflict
	}
	metricsReg := cfg.Metrics
	if metricsReg == nil {
		metricsReg = NewMetricsRegistry()
	}
	h := &Handler{
		runtime:                 cfg.Runtime,
		logger:                  cfg.Logger,
		metricsReg:              metricsReg,
		clientDefaults:          cfg.ClientDefaults,
		trustedClients:          cfg.TrustedClients,
		baseURLRewrites:         cfg.BaseURLRewrites,
		httpClient:              cfg.HTTPClient,
		deBAML:                  cfg.DeBAML,
		deBAMLRender:            cfg.DeBAMLRender,
		deBAMLParse:             cfg.DeBAMLParse,
		softFinalParse:          cfg.SoftFinalParse,
		nativeCapability:        cfg.NativeCapability,
		nativeShadow:            cfg.NativeShadowComparator,
		nativeServe:             cfg.NativeServeComparator,
		nativeStreamServe:       cfg.NativeStreamServeComparator,
		nativeStaticObserver:    cfg.NativeStaticObserver,
		nativeStaticServe:       cfg.NativeStaticServeComparator,
		nativeStaticStreamServe: cfg.NativeStaticStreamServeComparator,
		nativeStaticShadow:      cfg.NativeStaticShadowComparator,

		nativeDirectParseObserver: cfg.NativeDirectParseObserver,
	}
	// Register the direct-parse disposition counter up front. A registry that
	// rejects it is a real misconfiguration (a colliding metric of a different
	// type), so it fails construction here rather than at first parse; a registry
	// that already carries it — an in-process host sharing one registry across
	// handlers — re-uses it. See newDirectParseMetrics.
	dpm, err := newDirectParseMetrics(metricsReg)
	if err != nil {
		return nil, fmt.Errorf("worker: registering direct-parse metrics: %w", err)
	}
	h.directParseMetrics = dpm
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
	// Installed via the optional interface (not an unconditional call) so
	// minimal non-streaming adapter doubles that embed a nil bamlutils.Adapter
	// are skipped rather than dispatching SetSoftFinalParse on the nil embed.
	if setter, ok := adapter.(softFinalParseSetter); ok {
		setter.SetSoftFinalParse(h.softFinalParse)
	}
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
	// Install the native SERVE implementation (nil in every default/shadow/flag-off
	// build, so this is a no-op there). The generated dynamic call seam builds a
	// serving native child-attempt callback when this is non-nil AND
	// DeBAMLConfig().Enabled; it takes precedence over shadow, and the entry point
	// guarantees the two are never both non-nil.
	if setter, ok := adapter.(nativeServeSetter); ok {
		setter.SetNativeServeComparator(h.nativeServe)
	}
	// Install the native STREAM SERVE implementation (nil in every default/shadow/
	// flag-off build, so this is a no-op there). The generated dynamic StreamRequest
	// seam builds a serving native stream callback when this is non-nil AND
	// DeBAMLConfig().Enabled; it coexists with the unary serve callback.
	if setter, ok := adapter.(nativeStreamServeSetter); ok {
		setter.SetNativeStreamServeComparator(h.nativeStreamServe)
	}
	// Install the native STATIC no-send admission observer (de-BAML Slice 8B; nil in
	// every default/flag-off build, so this is a no-op there). The generated static
	// seam only builds an observer callback when this is non-nil AND
	// DeBAMLConfig().Enabled; it is OBSERVE-ONLY (always declines to BAML), so it
	// coexists with the unary/stream serve or shadow callbacks and changes no serving
	// behaviour.
	if setter, ok := adapter.(nativeStaticObserverSetter); ok {
		setter.SetNativeStaticObserver(h.nativeStaticObserver)
	}
	// Install the native STATIC SERVE implementation (de-BAML Slice 8C; nil in every
	// default/shadow/flag-off build, so this is a no-op there). The generated static
	// /call seam builds a serving native callback when this is non-nil AND
	// DeBAMLConfig().Enabled; on a pre-claim decline it runs BAML's Request.<Method> /
	// Parse.<Method> for the same call.
	if setter, ok := adapter.(nativeStaticServeSetter); ok {
		setter.SetNativeStaticServeComparator(h.nativeStaticServe)
	}
	// Install the native STATIC STREAM SERVE implementation (de-BAML Phase 3b; nil in
	// every default/shadow/flag-off build, so this is a no-op there). The generated
	// static /stream{,-with-raw} seam builds a serving native callback when this is
	// non-nil AND DeBAMLConfig().Enabled; on a pre-transport decline it runs BAML's
	// StreamRequest.<Method> / ParseStream.<Method> for the same request.
	if setter, ok := adapter.(nativeStaticStreamServeSetter); ok {
		setter.SetNativeStaticStreamServeComparator(h.nativeStaticStreamServe)
	}
	// Install the native STATIC Stage-1 SHADOW comparator (de-BAML Slice 8C; nil in
	// every default/serve/flag-off build, so this is a no-op there). The generated
	// static /call seam builds a shadow callback when this is non-nil AND
	// DeBAMLConfig().Enabled AND no serve callback is installed; BAML remains the sole
	// sender and native only parses the captured response bytes.
	if setter, ok := adapter.(nativeStaticShadowSetter); ok {
		setter.SetNativeStaticShadowComparator(h.nativeStaticShadow)
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
	if err := options.apply(adapter, h.clientDefaults, h.baseURLRewrites, h.trustedClients); err != nil {
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
