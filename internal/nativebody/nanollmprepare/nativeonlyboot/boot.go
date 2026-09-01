// Package nativeonlyboot is the BAML-free startup bootstrap for the ExecBridge-U1b
// native-only packaged worker. It is the deletion-substrate twin of
// internal/workerboot: it assembles a worker subprocess from the ALREADY-BAML-free
// blocks — the neutral worker.Handler, the workerplugin transport, and a pure-Go
// native runtime (nativeserve/spine backed by the emitted registry) — and installs
// NO BAML runtime, oracle, dynclient, render/parse callback, or serve/shadow
// factory.
//
// It deliberately does NOT import internal/workerboot, internal/rootruntime,
// introspected, the root baml_rest package, dynclient, the generated baml_client,
// language_client_go, or BoundaryML. That is the whole point: parameterizing
// workerboot.Run cannot remove those package-level imports, so the native-only
// artifact needs its own entrypoint whose compiled dependency graph is BAML-free.
// The whole-command dependency gate (TestNativeOnlyWorkerHasNoBAML) proves it.
//
// There is NO BAML fallback in this artifact. A method/mode outside the admitted
// cohort is rejected before a socket by the pure runtime + executor and becomes a
// terminal caller-visible error — never a fall-through to an absent BAML child.
package nativeonlyboot

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/artifactprofile"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/worker"
	"github.com/invakid404/baml-rest/workerplugin"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// methodNamer is the optional structural interface the pure native runtime
// satisfies so the bootstrap can report a BOUNDED admitted-method COUNT at startup
// (§4, "deletion frontier is observable structurally"). It never reports method
// names or request data to production logs.
type methodNamer interface {
	MethodNames() []string
}

// Run boots the native-only subprocess worker and serves it over go-plugin. It
// never returns under normal operation: goplugin.Serve blocks until the parent
// terminates the process.
//
// rt is the pure-Go native runtime (the emitted registry via
// nativeserve/spine.NewWorkerRuntime). capability is the REQUIRED concrete native
// engine capability — a native-only artifact is native_capable by construction, so
// a nil capability is a build error. nativeInit initializes the native engine once
// (proving it comes up before the handshake) and is likewise required.
//
// Every fatal condition logs via hclog (the only channel go-plugin preserves) and
// exits non-zero, which fails the handshake and surfaces to the host's pool.
func Run(rt worker.Runtime, capability worker.NativeCapability, nativeInit func() error) {
	logger := hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Debug,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	// A native-only artifact has no BAML fallback and no default runtime: nil
	// runtime/capability/init is a build-wiring bug, not a degraded mode. Fail loud
	// before any serving work.
	if rt == nil {
		logger.Error("native-only worker startup: nil runtime; a native-only artifact has no BAML fallback runtime")
		os.Exit(1)
	}
	if capability == nil {
		logger.Error("native-only worker startup: nil native capability; a native-only artifact is native_capable by construction")
		os.Exit(1)
	}
	if nativeInit == nil {
		logger.Error("native-only worker startup: nil native init; the native engine must be proven to initialize before serving")
		os.Exit(1)
	}

	// Initialize the pure-Go native runtime (validates the immutable registry is
	// non-empty). No shared library is loaded here — the emitted runtime is pure Go.
	rt.InitRuntime()

	// ARTIFACT ATTESTATION. The derived profile is a FACT about this binary's link
	// graph — it links a native engine — not a label it chose. A native-only artifact
	// is native_capable; DeriveProfile(capability != nil) is native_capable here by
	// construction. WorkerPackage (attested by the builder) distinguishes the
	// native-only artifact ID from the standard native-capable one, so no third
	// profile is needed. Attest fails closed on a stamp that contradicts the running
	// artifact. It runs BEFORE nativeInit, deliberately: a mislabeled artifact must
	// refuse to serve before it initializes the native runtime.
	attestation, err := artifactprofile.Attest(
		artifactprofile.DeriveProfile(capability != nil), os.LookupEnv)
	if err != nil {
		logger.Error("native-only worker startup: artifact profile attestation failed; refusing to serve under an unproven artifact identity",
			"err", err.Error())
		os.Exit(1)
	}

	// Prove the native engine comes up before the handshake so a link/ABI/init
	// failure fails the handshake loudly rather than at first request.
	if err := nativeInit(); err != nil {
		logger.Error("native-only worker startup: native runtime failed to initialize", "err", err.Error())
		os.Exit(1)
	}

	admittedMethodCount := -1
	if namer, ok := rt.(methodNamer); ok {
		admittedMethodCount = len(namer.MethodNames())
	}

	logger.Info("native-only worker startup: BAML-free native runtime serving the exact-JSON cohort",
		"native_engine", capability.NativeEngine(),
		"native_engine_version", capability.NativeEngineVersion(),
		"native_build_capable", true,
		"native_runtime_initialized", true,
		"admitted_method_count", admittedMethodCount,
		"artifact_profile", string(attestation.Profile),
		"artifact_id", attestation.ArtifactID,
		"artifact_stamped", attestation.Stamped,
		"artifact_source_revision", attestation.SourceRevision(),
		"artifact_source_bundle_digest", attestation.SourceBundleDigest(),
		"artifact_native_worker_tar_digest", attestation.NativeWorkerTarDigest(),
		"expected_artifact_profile", attestation.ExpectationLabel())

	// The expectation ALERT (a native-only artifact deployed where a different
	// profile is expected). Logged at Error so it pages; NOT fatal, mirroring the
	// full worker.
	if attestation.ExpectationViolated() {
		logger.Error("native-only worker startup: artifact profile does not match the expected deployment profile",
			"artifact_profile", string(attestation.Profile),
			"expected_artifact_profile", attestation.ExpectationLabel(),
			"artifact_id", attestation.ArtifactID,
			"alert_reason", attestation.AlertReason)
	}

	// Pure HTTP client + rewrite configuration. The exact cohort DECLINES a request
	// whose effective send target would be rewritten/proxied (pre-socket), so the
	// rewrite rules here never divert an admitted native send; they exist so the
	// admission gate has the real rule set to check against.
	baseURLRewrites := urlrewrite.LoadDefaultRules()
	streamIdleTimeout := llmhttp.StreamIdleTimeoutFromEnv()
	clientMode := llmhttp.ClientModeFromEnv()
	logger.Info("llmhttp client backend configured", "mode", clientMode.String())
	httpClient := llmhttp.NewDefaultClientWithOptions(llmhttp.ClientOptions{
		Mode:              clientMode,
		RewriteRules:      baseURLRewrites,
		StreamIdleTimeout: &streamIdleTimeout,
	})

	// RSS-triggered GC for native (nanollm) memory pressure, matching the full
	// worker: the native allocator's memory is invisible to Go's GC.
	if memLimitStr := os.Getenv("GOMEMLIMIT"); memLimitStr != "" {
		if memLimit, err := memlimit.ParseBytes(memLimitStr); err == nil && memLimit > 0 {
			threshold := memLimit * 8 / 10
			_ = memlimit.StartRSSMonitor(memlimit.RSSMonitorConfig{
				Threshold: threshold,
				Interval:  5 * time.Second,
				OnGC: func(rssBefore, rssAfter int64, result memlimit.GCResult) {
					logger.Debug("RSS-triggered GC completed",
						"rss_before", memlimit.FormatBytes(rssBefore),
						"rss_after", memlimit.FormatBytes(rssAfter),
						"threshold", memlimit.FormatBytes(threshold),
						"result", result.String())
				},
			})
		}
	}

	metricsReg := worker.NewMetricsRegistry()
	if err := artifactprofile.Register(metricsReg, attestation); err != nil {
		logger.Error("native-only worker startup: failed to register artifact profile collectors", "err", err.Error())
		os.Exit(1)
	}

	// Build the neutral worker.Handler. Runtime + the neutral HTTP/config fields
	// ONLY. Every de-BAML render/parse callback, native-serve/shadow factory,
	// trusted-client rollout policy, and BAML fallback is DELIBERATELY nil/absent —
	// a native-only worker has nowhere to fall back to.
	handler, err := worker.New(worker.Config{
		Runtime:          rt,
		Logger:           logger,
		Metrics:          metricsReg,
		BaseURLRewrites:  baseURLRewrites,
		HTTPClient:       httpClient,
		NativeCapability: capability,
	})
	if err != nil {
		logger.Error("native-only worker startup: failed to construct worker handler", "err", err.Error())
		os.Exit(1)
	}

	workerPlugin := &workerplugin.WorkerPlugin{Impl: handler}
	// Install the AttachSharedState callback BEFORE handing the plugin to go-plugin.
	// The host calls AttachSharedState once during handshake; pool expects the
	// callback during startup even though the admitted cohort itself declines
	// round-robin strategies (so the advancer is never exercised).
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

// ErrNilRuntime is retained for callers that want to assert the nil-runtime
// contract without booting; Run itself exits the process rather than returning.
var ErrNilRuntime = errors.New("nativeonlyboot: nil runtime")

// grpcSharedStateHook adapts the brokered pb.SharedStateClient to the worker
// package's SharedStateHook seam. It lives here (with the bootstrap) rather than in
// the worker package so the protobuf/gRPC client type does not leak into the
// handler package, mirroring internal/workerboot's own hook. workerplugin owns the
// wire layer via NewRemoteAdvancer.
type grpcSharedStateHook struct {
	client pb.SharedStateClient
}

func (h grpcSharedStateHook) NewRoundRobinAdvancer(ctx context.Context, requestID string) bamlutils.RoundRobinAdvancer {
	return workerplugin.NewRemoteAdvancer(ctx, h.client, requestID)
}
