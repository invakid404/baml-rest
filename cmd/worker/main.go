package main

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
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

func main() {
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

	// Resolve env-driven config once at startup. The resulting values
	// are passed into worker.New so the handler never reads env state
	// at request time. Subprocess workers stay env-based by design —
	// the host does not ship programmatic config across the go-plugin
	// boundary; library-mode callers wire their own config inside the
	// host process instead.
	buildRequestConfig := buildrequest.EnvConfig()
	// Note: the retired-BAML_REST_USE_BUILD_REQUEST deprecation warning is
	// emitted once by the host (cmd/serve), which runs at startup in both
	// subprocess and in-process modes. Emitting it here too would duplicate
	// the message once per pooled worker subprocess, so the worker stays
	// silent on it.
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
	// when a BuildRequest serializer is actually in the request path.
	// StreamRequest is always BuildRequest (streaming, plus the /call
	// bridge, which ignores DisableCallBuildRequest); the non-streaming
	// Request path is taken only when Request exists AND
	// DisableCallBuildRequest is off. When neither holds, all traffic is
	// legacy and the serializer caveat is irrelevant. Note this is NOT
	// `(Request|StreamRequest) && !DisableCallBuildRequest`: that would
	// wrongly suppress the advisory when StreamRequest still routes
	// streaming + bridged-/call traffic through BuildRequest.
	buildRequestInUse := introspected.StreamRequest != nil ||
		(introspected.Request != nil && !buildRequestConfig.DisableCallBuildRequest)
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
		BuildRequest:    buildRequestConfig,
		BaseURLRewrites: baseURLRewrites,
		HTTPClient:      httpClient,
		DeBAML:          deBAMLConfig,
		DeBAMLRender:    debaml.Render,
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
// worker package's SharedStateHook seam. Lives in cmd/worker rather
// than the worker package so the protobuf/gRPC client type does not
// leak into the handler package; the wire layer continues to use
// workerplugin.NewRemoteAdvancer so request handling stays
// byte-identical to the pre-extraction subprocess build.
type grpcSharedStateHook struct {
	client pb.SharedStateClient
}

func (h grpcSharedStateHook) NewRoundRobinAdvancer(ctx context.Context, requestID string) bamlutils.RoundRobinAdvancer {
	return workerplugin.NewRemoteAdvancer(ctx, h.client, requestID)
}
