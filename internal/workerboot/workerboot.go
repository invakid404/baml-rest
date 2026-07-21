// Package workerboot holds the subprocess worker's startup bootstrap, factored
// out of cmd/worker/main.go so more than one worker entry point can share it
// (de-BAML cutover Slice 2). Two binaries call Run:
//
//   - cmd/worker (root module) — the BAML-only worker, the immediate-reversal
//     build, passes a zero Options (nil native capability);
//   - internal/nativebody/nanollmprepare/cmd/worker (the isolated, out-of-go.work
//     module built with GOWORK=off + CGO) — the BAML+nanollm worker, passes a
//     nanollm-backed NativeCapability and a NativeInit that initializes the
//     native runtime at startup.
//
// This package is neutral: it imports no native engine (no nanollm, no CGO) and
// stays in the host/root module's zero-nanollm link graph. The native engine is
// injected only through the plain func/interface fields on Options, so the
// BAML-only worker and the in-process host never link nanollm.
package workerboot

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/internal/rootruntime"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/invakid404/baml-rest/worker"
	"github.com/invakid404/baml-rest/workerplugin"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// Options carries the entry-point-specific dependencies a particular worker
// binary injects into the shared bootstrap. The zero value is the BAML-only
// worker (no native engine); the nanollm worker supplies the two native fields.
type Options struct {
	// NativeCapability, when non-nil, is the neutral native-send capability
	// linked into this worker binary. It is stored on the handler and reported
	// at startup as a build capability. It is NOT routed in this slice: the
	// orchestrator's native child-attempt callback stays nil/hard-off, so a
	// present capability changes no serving behaviour. nil in the BAML-only
	// worker.
	NativeCapability worker.NativeCapability

	// NativeInit, when non-nil, initializes the native runtime once at startup
	// (after the BAML runtime is up, before the handler serves) so a
	// link/ABI/init failure surfaces loudly at boot rather than at first
	// request. A returned error exits the process non-zero, which fails
	// go-plugin's handshake and surfaces to the host's pool startup — exactly
	// like a fatal BAML/config error below. nil in the BAML-only worker.
	NativeInit func() error

	// NativeShadowFactory, when non-nil, builds the native one-send SHADOW
	// comparator (de-BAML cutover Slice 4) the SHADOW deploy profile's worker
	// injects. It is called ONCE at startup with the worker's private Prometheus
	// registry so the comparator registers its bounded de-BAML collectors
	// (declines / attempts / plan_compare) on the same registry the host gathers;
	// the returned NativeShadowFunc is installed on every adapter. The generated
	// dynamic call seam turns it into the Slice-1 native child-attempt callback
	// only when it is non-nil AND the umbrella flag is enabled, and that callback
	// ALWAYS declines to BAML after a no-socket plan comparison. A returned error
	// exits the process non-zero (fails the go-plugin handshake). nil in the
	// BAML-only worker and the S2 native-capable worker, so those serve 100% BAML
	// with the callback hard-off.
	NativeShadowFactory func(reg prometheus.Registerer) (bamlutils.NativeShadowFunc, error)

	// NativeServeFactory, when non-nil, builds the native SERVE implementation
	// (de-BAML cutover Slice 6) the SERVE deploy profile's worker injects while
	// the umbrella flag is on. Like NativeShadowFactory it is called ONCE at
	// startup with the worker's private Prometheus registry so the serve
	// implementation registers its bounded de-BAML collectors on the same registry
	// the host gathers; the returned NativeServeFunc is installed on every adapter.
	// The generated dynamic call seam turns it into the Slice-1 native
	// child-attempt callback only when it is non-nil AND the umbrella flag is
	// enabled, and that callback actually SERVES an admitted unary `_dynamic` call
	// natively (one exact RoundTrip) or declines pre-socket to BAML. A returned
	// error exits the process non-zero (fails the go-plugin handshake). nil in
	// every non-serve build. It is MUTUALLY EXCLUSIVE with NativeShadowFactory —
	// Run fails startup if BOTH are supplied, so a worker installs at most one
	// native callback.
	NativeServeFactory func(reg prometheus.Registerer) (bamlutils.NativeServeFunc, error)

	// NativeStreamServeFactory, when non-nil, builds the native STREAM SERVE
	// implementation (de-BAML Phase 7D) the SERVE deploy profile's worker injects
	// alongside NativeServeFactory while the umbrella flag is on. It is called ONCE
	// at startup with the worker's private Prometheus registry (for signature
	// symmetry with NativeServeFactory; the stream lane records no de-BAML counters
	// this phase — the rollout observability is owner-trimmed); the returned
	// NativeStreamServeFunc is installed on every adapter. The generated dynamic
	// StreamRequest seam turns it into StreamConfig.NativeAttempt only when it is
	// non-nil AND the umbrella flag is enabled, and that callback actually SERVES an
	// admitted dynamic OpenAI `_dynamic` StreamRequest natively (one exact RoundTrip
	// driving nanollm DoStream) or declines pre-transport to BAML. A returned error
	// exits the process non-zero (fails the go-plugin handshake). nil in every
	// non-serve build. It is the streaming twin of NativeServeFactory and is NOT
	// installed in the shadow profile, so it does not participate in the serve/shadow
	// mutual exclusion — a serve worker supplies BOTH NativeServeFactory and
	// NativeStreamServeFactory.
	NativeStreamServeFactory func(reg prometheus.Registerer) (bamlutils.NativeStreamServeFunc, error)

	// NativeStaticObserveFactory, when non-nil, builds the native STATIC no-send
	// admission OBSERVER (de-BAML Slice 8B) a native worker injects while the umbrella
	// flag is on. Like the other native factories it is called ONCE at startup with
	// the worker's private Prometheus registry (retained for signature symmetry; 8B
	// records no de-BAML counters — it is observe-only); the returned
	// NativeStaticObserveFunc is installed on every adapter. The generated static
	// observe seam turns it into an observer callback only when it is non-nil AND the
	// umbrella flag is enabled, and that callback ALWAYS declines to BAML after a
	// no-socket admission observation. A returned error exits the process non-zero
	// (fails the go-plugin handshake). nil in every non-native build. It is
	// OBSERVE-ONLY, so it is NOT part of the serve/shadow mutual exclusion — a native
	// worker may supply it alongside the serve/shadow factory or on its own.
	NativeStaticObserveFactory func(reg prometheus.Registerer) (bamlutils.NativeStaticObserveFunc, error)

	// NativeStaticServeFactory, when non-nil, builds the native STATIC SERVE
	// implementation (de-BAML Slice 8C) a SERVE-profile worker injects while the
	// umbrella flag is on. Like the other native factories it is called ONCE at
	// startup with the worker's private Prometheus registry; the returned
	// NativeStaticServeFunc is installed on every adapter. The generated static /call
	// seam turns it into a serving callback only when it is non-nil AND the umbrella
	// flag is enabled: it SERVES an admitted static unary /call natively (one exact
	// RoundTrip), and every unsupported shape declines PRE-SOCKET so BAML serves it. A
	// returned error exits the process non-zero (fails the go-plugin handshake). nil
	// in every non-serve build. It is the static twin of NativeServeFactory and, like
	// the observer, is NOT part of the dynamic serve/shadow mutual exclusion.
	NativeStaticServeFactory func(reg prometheus.Registerer) (bamlutils.NativeStaticServeFunc, error)

	// NativeStaticShadowFactory, when non-nil, builds the native STATIC Stage-1 SHADOW
	// comparator (de-BAML Slice 8C) a SHADOW-profile worker injects while the umbrella
	// flag is on. Called ONCE at startup with the worker's private registry; the
	// returned NativeStaticShadowFunc is installed on every adapter. The generated
	// static /call seam turns it into a shadow callback only when it is non-nil AND the
	// umbrella flag is enabled AND no serve callback is installed: BAML remains the SOLE
	// sender and native parses BAML's captured bytes (ZERO native sends) to compare. A
	// returned error exits the process non-zero. nil in every non-shadow build. It is
	// the static twin of NativeShadowFactory.
	NativeStaticShadowFactory func(reg prometheus.Registerer) (bamlutils.NativeStaticShadowFunc, error)

	// NativeBuildCapable and NativeEngineName advertise a STATIC build capability
	// (no FFI) for the startup diagnostic even while the umbrella flag is off, so a
	// flag-off serve/shadow profile can report native_build_capable=true + the
	// engine name WITHOUT calling nanollm (resolving NativeCapability would call
	// the engine's Version FFI). A non-nil NativeCapability supersedes both (it
	// carries the live engine identity/version). The BAML-only worker leaves them
	// zero, so it reports native_build_capable=false.
	NativeBuildCapable bool
	NativeEngineName   string
}

// Run boots the subprocess worker and serves it over go-plugin. It never
// returns under normal operation: goplugin.Serve blocks until the parent
// terminates the process. The body is the former cmd/worker/main.go main(),
// with the native-capability wiring and startup diagnostic added.
func Run(opts Options) {
	// Initialize BAML runtime via the rootruntime wrapper — the worker
	// package no longer imports the root generated package directly, so
	// the runtime touch point lives here at the binary entry point.
	rt := rootruntime.Runtime{}
	rt.InitRuntime()

	// Create hclog logger for go-plugin communication.
	// Logs are routed back to the main process via go-plugin's protocol.
	logger := hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Debug,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	// Native-send capability is a BUILD capability, not a runtime flag: its
	// presence means this binary was linked with a native engine (nanollm in
	// the isolated worker). Initialize the native runtime here — after BAML is
	// up, before serving — so both native runtimes are proven to coexist at
	// startup and any link/ABI/init failure fails the handshake loudly. Then
	// emit a clear diagnostic of whether this worker is native-capable. In this
	// slice the capability is reported only; it is never wired to the
	// orchestrator (the native child-attempt callback stays nil/hard-off), so a
	// native-capable worker still serves 100% BAML.
	// A worker installs AT MOST ONE native child-attempt callback. Supplying both
	// a shadow and a serve factory is a build-wiring bug that would silently pick
	// one (serve) and drop the other; fail the handshake loudly instead so a
	// mis-wired cohort never serves under an ambiguous rollout mode.
	if opts.NativeShadowFactory != nil && opts.NativeServeFactory != nil {
		logger.Error("both NativeShadowFactory and NativeServeFactory supplied; a worker installs at most one native callback")
		os.Exit(1)
	}

	nativeRuntimeInitialized := false
	if opts.NativeInit != nil {
		if err := opts.NativeInit(); err != nil {
			logger.Error("native runtime failed to initialize at startup", "err", err.Error())
			os.Exit(1)
		}
		nativeRuntimeInitialized = true
	}

	// Resolve the umbrella flag HERE (each worker resolves its OWN environment —
	// the subprocess host's in-process runtimeCfg is deliberately ignored), so the
	// startup diagnostic below reports the flag ALONGSIDE the build capability and
	// the rollout mode. Any host/worker configuration disagreement is therefore
	// visible in this per-worker line. deBAMLConfig flows unchanged into worker.New.
	deBAMLConfig := bamlutils.DeBAMLConfigFromEnv()

	// Rollout mode is a deployment PROFILE, not a second application flag. It is
	// derived from which native child-attempt LANES this build wired — and a lane
	// counts whether it is DYNAMIC (`_dynamic` call) or STATIC (generated static
	// `/call`), so a static-only serve/shadow cohort is reported correctly rather
	// than appearing BAML-only to rollout monitoring:
	//   - "serve" when a serve implementation was injected (the de-BAML cutover
	//     Slice 6 dynamic SERVE and/or the Slice 8C static SERVE deploy profile).
	//     It serves an admitted call natively (one exact RoundTrip) while the
	//     umbrella flag is enabled; unsupported traffic declines pre-socket to BAML.
	//   - "shadow" when a shadow comparator was injected (the dynamic and/or the
	//     Slice 8C static shadow deploy profile). It runs a no-socket native-vs-BAML
	//     comparison per admitted call ONLY while the flag is enabled, then declines
	//     — BAML still serves every request.
	//   - "off" otherwise (BAML-only worker, the S2 native-capable worker, and
	//     every flag-off build): no native child-attempt callback is installed,
	//     100% BAML.
	// serve takes precedence over shadow (matching worker install order); the two
	// profiles are not wired together in practice.
	rolloutMode, nativeServing := nativeLaneStatus(
		opts.NativeServeFactory != nil,
		opts.NativeStaticServeFactory != nil,
		opts.NativeShadowFactory != nil,
		opts.NativeStaticShadowFactory != nil,
		deBAMLConfig.Enabled,
	)
	// native_build_capable is a STATIC build fact: true when the binary linked a
	// native engine, reported even while the flag is off. A live NativeCapability
	// carries the engine identity/version (resolved via FFI); a flag-off
	// serve/shadow profile supplies the static NativeBuildCapable/NativeEngineName
	// instead so the diagnostic never calls nanollm on the flag-off path.
	nativeBuildCapable := opts.NativeCapability != nil || opts.NativeBuildCapable
	nativeEngine := opts.NativeEngineName
	nativeEngineVersion := ""
	if nc := opts.NativeCapability; nc != nil {
		nativeEngine = nc.NativeEngine()
		nativeEngineVersion = nc.NativeEngineVersion()
	}
	// Two message forms keyed on the static build capability (NOT on a live
	// NativeCapability): a flag-off serve/shadow-capable profile advertises the
	// build capability WITHOUT any FFI. native_build_capable + native_runtime_
	// initialized disambiguate "linked but not initialized" from "not linked".
	if nativeBuildCapable {
		logger.Info("de-BAML worker startup: native capability + resolved flag + rollout mode",
			"native_engine", nativeEngine,
			"native_engine_version", nativeEngineVersion,
			"debaml_flag_enabled", deBAMLConfig.Enabled,
			"native_build_capable", true,
			"native_runtime_initialized", nativeRuntimeInitialized,
			"rollout_mode", rolloutMode,
			"native_serving", nativeServing,
			"config_source", "worker_env")
	} else {
		logger.Info("de-BAML worker startup: no native capability (BAML-only worker)",
			"debaml_flag_enabled", deBAMLConfig.Enabled,
			"native_build_capable", false,
			"native_runtime_initialized", false,
			"rollout_mode", rolloutMode,
			"native_serving", nativeServing,
			"config_source", "worker_env")
	}

	// Resolve env-driven config once at startup. The resulting values
	// are passed into worker.New so the handler never reads env state
	// at request time. Subprocess workers stay env-based by design —
	// the host does not ship programmatic config across the go-plugin
	// boundary; library-mode callers wire their own config inside the
	// host process instead.
	//
	// Note: the retired-env deprecation warnings (BAML_REST_USE_BUILD_REQUEST
	// from #537 and BAML_REST_DISABLE_CALL_BUILD_REQUEST from #539) are
	// emitted once by the host (cmd/serve), which runs at startup in both
	// subprocess and in-process modes. Emitting them here too would
	// duplicate the messages once per pooled worker subprocess, so the
	// worker stays silent on both. Neither var affects routing, so the
	// worker has nothing to resolve from them.
	// (deBAMLConfig was resolved above with the startup diagnostic.)
	baseURLRewrites := urlrewrite.LoadDefaultRules()
	streamIdleTimeout := llmhttp.StreamIdleTimeoutFromEnv()
	clientMode := llmhttp.ClientModeFromEnv()
	logger.Info("llmhttp client backend configured", "mode", clientMode.String())
	httpClient := llmhttp.NewDefaultClientWithOptions(llmhttp.ClientOptions{
		Mode:              clientMode,
		RewriteRules:      baseURLRewrites,
		StreamIdleTimeout: &streamIdleTimeout,
	})

	// Load deployment-wide ClientRegistry defaults. A parse failure here is
	// fatal by design: the worker exits non-zero, go-plugin's handshake
	// fails, and pool.New surfaces the error to serve's startup. This keeps
	// misconfiguration loud instead of silently ignoring the env var.
	//
	// logger.Error (hclog-structured JSON on stderr) is the only channel
	// go-plugin preserves; fmt.Fprintln / log.Fatal output would be demoted
	// to debug or dropped entirely by the plugin host.
	clientDefaults, err := worker.LoadClientDefaults()
	if err != nil {
		logger.Error("invalid BAML_REST_CLIENT_DEFAULTS", "err", err.Error())
		os.Exit(1)
	}
	// Gate on the effective BuildRequest route: the advisory only applies
	// when a BuildRequest serializer is actually in the request path. A
	// BuildRequest surface — StreamRequest (streaming, plus the /call
	// bridge) or the non-streaming Request path for /call — puts a
	// BuildRequest serializer in the path. When neither surface exists,
	// all traffic is legacy and the serializer caveat is irrelevant.
	buildRequestInUse := introspected.StreamRequest != nil || introspected.Request != nil
	if clientDefaults.HasKey("allowed_role_metadata") && buildRequestInUse {
		logger.Warn(
			"BAML_REST_CLIENT_DEFAULTS sets allowed_role_metadata and the " +
				"BuildRequest route is on by default; older BAML BuildRequest " +
				"serializers may drop message-level metadata (e.g. cache_control). " +
				"Keep this covered by integration tests when changing supported " +
				"BAML versions.")
	}

	// Start RSS monitor to trigger GC when native memory pressure is high.
	// BAML's native (Rust) memory isn't visible to Go's GC, so we monitor RSS
	// and force GC to run finalizers that clean up native resources.
	//
	// Note: We discard the stop function because workers run until the parent
	// go-plugin process terminates them. There's no graceful shutdown path.
	if memLimitStr := os.Getenv("GOMEMLIMIT"); memLimitStr != "" {
		if memLimit, err := memlimit.ParseBytes(memLimitStr); err == nil && memLimit > 0 {
			// Trigger GC when RSS exceeds 80% of memory limit
			threshold := memLimit * 8 / 10
			_ = memlimit.StartRSSMonitor(memlimit.RSSMonitorConfig{
				Threshold: threshold,
				Interval:  5 * time.Second,
				OnGC: func(rssBefore, rssAfter int64, result memlimit.GCResult) {
					logger.Debug("RSS-triggered GC completed",
						"rss_before", memlimit.FormatBytes(rssBefore),
						"rss_after", memlimit.FormatBytes(rssAfter),
						"threshold", memlimit.FormatBytes(threshold),
						"result", result.String(),
					)
				},
			})
		}
	}

	// Build the worker's private metrics registry once so a shadow comparator can
	// register its de-BAML collectors on the SAME registry the host gathers.
	metricsReg := worker.NewMetricsRegistry()

	// Build the native one-send SHADOW comparator (nil except in the shadow deploy
	// profile). The factory registers the bounded de-BAML collectors on the
	// worker registry; a failure is fatal (fails the go-plugin handshake) so a
	// misconfigured shadow build never silently serves with the comparator off.
	// A factory that is PRESENT (rollout_mode already logged "shadow" above) but
	// returns a nil comparator without an error is equally fatal: continuing would
	// serve all-BAML while reporting shadow mode — a silent shadow-cohort
	// misconfiguration, not a benign hard-off (that path leaves the factory nil).
	var nativeShadow bamlutils.NativeShadowFunc
	if opts.NativeShadowFactory != nil {
		fn, err := opts.NativeShadowFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native shadow comparator", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native shadow comparator factory returned a nil comparator without an error; a shadow-profile worker must not run all-BAML while reporting rollout_mode=shadow")
			os.Exit(1)
		}
		nativeShadow = fn
	}

	// Build the native SERVE implementation (nil except in the serve deploy
	// profile). Same fail-loud contract as the shadow factory: a build error or a
	// PRESENT factory that returns a nil implementation without an error is fatal,
	// so a mislabeled serve cohort never serves all-BAML while reporting
	// rollout_mode=serve/native_serving=eligible.
	var nativeServe bamlutils.NativeServeFunc
	if opts.NativeServeFactory != nil {
		fn, err := opts.NativeServeFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native serve implementation", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native serve factory returned a nil implementation without an error; a serve-profile worker must not run all-BAML while reporting rollout_mode=serve")
			os.Exit(1)
		}
		nativeServe = fn
	}

	// Build the native STREAM SERVE implementation (de-BAML Phase 7D; nil except in
	// the serve deploy profile). Same fail-loud contract as the unary serve factory:
	// a build error or a PRESENT factory that returns a nil implementation without an
	// error is fatal, so a serve cohort never silently streams all-BAML while
	// reporting rollout_mode=serve.
	var nativeStreamServe bamlutils.NativeStreamServeFunc
	if opts.NativeStreamServeFactory != nil {
		fn, err := opts.NativeStreamServeFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native stream serve implementation", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native stream serve factory returned a nil implementation without an error; a serve-profile worker must not stream all-BAML while reporting rollout_mode=serve")
			os.Exit(1)
		}
		nativeStreamServe = fn
	}

	// Build the native STATIC no-send admission OBSERVER (de-BAML Slice 8B; nil except
	// in a native worker with the flag on). Same fail-loud contract as the serve
	// factories: a build error or a PRESENT factory that returns a nil observer
	// without an error is fatal, so a native cohort never silently runs with the
	// observer off while reporting a native build. It is OBSERVE-ONLY (always
	// declines), so it does not participate in the serve/shadow mutual exclusion.
	var nativeStaticObserver bamlutils.NativeStaticObserveFunc
	if opts.NativeStaticObserveFactory != nil {
		fn, err := opts.NativeStaticObserveFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native static observer", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native static observer factory returned a nil observer without an error; a native worker must not run with the static observer off while reporting a native build")
			os.Exit(1)
		}
		nativeStaticObserver = fn
	}

	// Build the native STATIC SERVE implementation (de-BAML Slice 8C; nil except in a
	// SERVE-profile worker with the flag on). A non-nil factory returning an error or
	// a nil func without an error is fatal, so a serve cohort never silently runs with
	// the static serve callback off while reporting a serve build.
	var nativeStaticServe bamlutils.NativeStaticServeFunc
	if opts.NativeStaticServeFactory != nil {
		fn, err := opts.NativeStaticServeFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native static serve implementation", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native static serve factory returned a nil implementation without an error; a serve worker must not run with the static serve callback off while reporting a serve build")
			os.Exit(1)
		}
		nativeStaticServe = fn
	}

	// Build the native STATIC Stage-1 SHADOW comparator (de-BAML Slice 8C; nil except
	// in a SHADOW-profile worker with the flag on). A non-nil factory returning an
	// error or a nil func without an error is fatal.
	var nativeStaticShadow bamlutils.NativeStaticShadowFunc
	if opts.NativeStaticShadowFactory != nil {
		fn, err := opts.NativeStaticShadowFactory(metricsReg)
		if err != nil {
			logger.Error("failed to build native static shadow comparator", "err", err.Error())
			os.Exit(1)
		}
		if fn == nil {
			logger.Error("native static shadow factory returned a nil comparator without an error; a shadow worker must not run with the static shadow callback off while reporting a shadow build")
			os.Exit(1)
		}
		nativeStaticShadow = fn
	}

	handler, err := worker.New(worker.Config{
		Runtime:         rt,
		Logger:          logger,
		Metrics:         metricsReg,
		ClientDefaults:  clientDefaults,
		BaseURLRewrites: baseURLRewrites,
		HTTPClient:      httpClient,
		DeBAML:          deBAMLConfig,
		DeBAMLRender:    debaml.Render,
		DeBAMLParse:     debaml.Parse,
		// Store the neutral native-send capability (nil for the BAML-only
		// worker). Storage + getter only — not wired to native serving.
		NativeCapability: opts.NativeCapability,
		// Install the native one-send SHADOW comparator (nil except in the
		// shadow deploy profile's worker). When non-nil AND the umbrella flag is
		// enabled it drives a no-socket native-vs-BAML plan comparison per
		// admitted dynamic call, then declines so BAML still serves.
		NativeShadowComparator: nativeShadow,
		// Install the native SERVE implementation (nil except in the serve deploy
		// profile's worker with the flag on). When non-nil AND the umbrella flag
		// is enabled it actually serves an admitted unary `_dynamic` call natively
		// (one exact RoundTrip); unsupported traffic declines pre-socket to BAML.
		NativeServeComparator: nativeServe,
		// Install the native STREAM SERVE implementation (de-BAML Phase 7D; nil except
		// in the serve deploy profile's worker with the flag on). When non-nil AND the
		// umbrella flag is enabled it serves an admitted dynamic OpenAI `_dynamic`
		// StreamRequest natively (one exact RoundTrip driving DoStream); unsupported
		// traffic declines pre-transport to BAML.
		NativeStreamServeComparator: nativeStreamServe,
		// Install the native STATIC no-send admission observer (de-BAML Slice 8B; nil
		// except in a native worker with the flag on). When non-nil AND the umbrella
		// flag is enabled it runs the full static admission predicate per eligible
		// static method, then ALWAYS declines pre-socket to BAML — observe-only.
		NativeStaticObserver: nativeStaticObserver,
		// Install the native STATIC SERVE implementation (de-BAML Slice 8C; nil except
		// in a SERVE-profile worker with the flag on). When non-nil AND the umbrella
		// flag is enabled it serves an admitted static unary /call natively (one exact
		// RoundTrip); unsupported traffic declines pre-socket to BAML.
		NativeStaticServeComparator: nativeStaticServe,
		// Install the native STATIC Stage-1 SHADOW comparator (de-BAML Slice 8C; nil
		// except in a SHADOW-profile worker with the flag on). When non-nil AND the flag
		// is enabled AND no serve callback is installed, the generated static /call seam
		// runs a no-send shadow comparison per admitted static call — BAML sole sender.
		NativeStaticShadowComparator: nativeStaticShadow,
	})
	if err != nil {
		logger.Error("failed to construct worker handler", "err", err.Error())
		os.Exit(1)
	}

	workerPlugin := &workerplugin.WorkerPlugin{Impl: handler}
	// Install the AttachSharedState callback *before* handing the plugin
	// to go-plugin. The host calls AttachSharedState once during
	// handshake; without a handler the RPC fails fast (see
	// workerplugin/grpc.go) and worker startup fails — there is no
	// silent fallback to the in-process Coordinator on this path.
	workerPlugin.SetAttachSharedStateHandler(func(_ context.Context, client pb.SharedStateClient) error {
		handler.SetSharedStateHook(grpcSharedStateHook{client: client})
		return nil
	})

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins: map[string]goplugin.Plugin{
			"worker": workerPlugin,
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts, workerplugin.GRPCServerOptions()...)
			return grpc.NewServer(opts...)
		},
		Logger: logger,
	})
}

// nativeLaneStatus derives the bounded (rollout_mode, native_serving) startup
// diagnostics from which native child-attempt lanes a build wired. A serve/shadow
// lane counts whether it is DYNAMIC or STATIC, so a static-only cohort (e.g. only
// NativeStaticServeFactory) is reported as serving rather than BAML-only — the gap
// that let an actively static-serving worker read as rollout_mode=off. serve takes
// precedence over shadow (matching the worker install order). Pure so the mapping
// is unit-testable without booting a worker.
func nativeLaneStatus(dynServe, staticServe, dynShadow, staticShadow, flagEnabled bool) (rolloutMode, nativeServing string) {
	serveLane := dynServe || staticServe
	shadowLane := dynShadow || staticShadow
	switch {
	case serveLane:
		rolloutMode = "serve"
	case shadowLane:
		rolloutMode = "shadow"
	default:
		rolloutMode = "off"
	}
	return rolloutMode, nativeServingLabel(serveLane, flagEnabled)
}

// nativeServingLabel returns the bounded native_serving startup-diagnostic label.
// It is "eligible" ONLY when a serve LANE is active with the umbrella flag on (a
// serve factory — dynamic OR static — is present AND the flag is enabled). It is
// "off" otherwise — crucially including the flag-OFF + factory-present case: the
// generated seam gates every native child-attempt callback on DeBAMLConfig().Enabled,
// so with the flag off no serve callback is installed and nothing serves natively
// regardless of a present factory. Extracted as a pure function so this eligibility
// contract is unit-testable without booting a worker.
func nativeServingLabel(serveFactoryPresent, flagEnabled bool) string {
	if serveFactoryPresent && flagEnabled {
		return "eligible"
	}
	return "off"
}

// grpcSharedStateHook adapts the brokered pb.SharedStateClient to the
// worker package's SharedStateHook seam. Lives here (with the bootstrap)
// rather than the worker package so the protobuf/gRPC client type does not
// leak into the handler package; the wire layer continues to use
// workerplugin.NewRemoteAdvancer so request handling stays byte-identical to
// the pre-extraction subprocess build.
type grpcSharedStateHook struct {
	client pb.SharedStateClient
}

func (h grpcSharedStateHook) NewRoundRobinAdvancer(ctx context.Context, requestID string) bamlutils.RoundRobinAdvancer {
	return workerplugin.NewRemoteAdvancer(ctx, h.client, requestID)
}
