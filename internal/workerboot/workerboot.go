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
	if opts.NativeInit != nil {
		if err := opts.NativeInit(); err != nil {
			logger.Error("native runtime failed to initialize at startup", "err", err.Error())
			os.Exit(1)
		}
	}
	if nc := opts.NativeCapability; nc != nil {
		logger.Info("native send capability linked (build capability; routing hard-off in this build)",
			"native_engine", nc.NativeEngine(),
			"native_engine_version", nc.NativeEngineVersion(),
			"native_routing", "off")
	} else {
		logger.Info("native send capability not linked (BAML-only worker)",
			"native_routing", "off")
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
	deBAMLConfig := bamlutils.DeBAMLConfigFromEnv()
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

	handler, err := worker.New(worker.Config{
		Runtime:         rt,
		Logger:          logger,
		Metrics:         worker.NewMetricsRegistry(),
		ClientDefaults:  clientDefaults,
		BaseURLRewrites: baseURLRewrites,
		HTTPClient:      httpClient,
		DeBAML:          deBAMLConfig,
		DeBAMLRender:    debaml.Render,
		DeBAMLParse:     debaml.Parse,
		// Store the neutral native-send capability (nil for the BAML-only
		// worker). Storage + getter only in this slice — not wired to routing.
		NativeCapability: opts.NativeCapability,
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
