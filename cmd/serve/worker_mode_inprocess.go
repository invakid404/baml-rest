//go:build !subprocess

package main

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/introspected"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/worker"
	"github.com/invakid404/baml-rest/workerplugin"
)

// configureWorkerMode prepares the pool config for inprocess worker
// execution: initialises the BAML runtime in this process (the
// subprocess build does it inside cmd/worker; without a worker
// process we have to do it here), parses deployment-wide client
// defaults with the same fail-loud contract cmd/worker enforces, and
// installs a WorkerFactory that constructs worker.Handler
// directly.
//
// The factory runs at initial pool fill AND on restart, so it must
// not call InitRuntime — re-initialising BAML on every handler
// rebuild would be wrong. InitRuntime fires exactly once here at
// server startup.
//
// runtimeCfg carries the env-resolved runtime values cmd/serve's main
// resolves once at startup: the rootruntime wrapper, BuildRequest
// config, base-URL rewrites, and the per-handler llmhttp.Client. Every
// in-process handler is constructed with the same values so behaviour
// across the pool is identical to the subprocess build.
func configureWorkerMode(logger zerolog.Logger, cfg *pool.Config, runtimeCfg workerModeRuntimeConfig) error {
	runtimeCfg.Runtime.InitRuntime()

	clientDefaults, err := worker.LoadClientDefaults()
	if err != nil {
		return fmt.Errorf("invalid BAML_REST_CLIENT_DEFAULTS: %w", err)
	}
	// Gate on BuildRequest surface availability: the advisory only applies
	// when traffic actually takes the BuildRequest route. On BAML versions
	// that expose neither Request nor StreamRequest, all traffic is legacy
	// and the BuildRequest serializer caveat is irrelevant.
	if clientDefaults.HasKey("allowed_role_metadata") && (introspected.Request != nil || introspected.StreamRequest != nil) {
		logger.Warn().Msg(
			"BAML_REST_CLIENT_DEFAULTS sets allowed_role_metadata and the " +
				"BuildRequest route is on by default; older BAML BuildRequest " +
				"serializers may drop message-level metadata (e.g. cache_control). " +
				"Keep this covered by integration tests when changing supported " +
				"BAML versions.")
	}

	cfg.WorkerFactory = func(wcfg pool.WorkerFactoryConfig) (workerplugin.Worker, error) {
		var hook worker.SharedStateHook
		if wcfg.SharedStateStore != nil {
			hook = worker.NewStoreSharedStateHook(wcfg.SharedStateStore)
		}
		// Each handler gets its own metrics registry; a restart
		// rebuilds the registry cleanly rather than reusing the
		// previous instance's counters.
		return worker.New(worker.Config{
			Runtime:         runtimeCfg.Runtime,
			Logger:          wcfg.Logger,
			Metrics:         worker.NewMetricsRegistry(),
			ClientDefaults:  clientDefaults,
			SharedState:     hook,
			BuildRequest:    runtimeCfg.BuildRequest,
			DeBAML:          runtimeCfg.DeBAML,
			DeBAMLRender:    runtimeCfg.DeBAMLRender,
			BaseURLRewrites: runtimeCfg.BaseURLRewrites,
			HTTPClient:      runtimeCfg.HTTPClient,
		})
	}
	return nil
}

// effectivePoolSizeForMemory clamps to 1 because the pool itself
// will normalise PoolSize to 1 — memlimit.CalculateLimits and the
// per_worker startup log should report the actual layout, not the
// requested-but-discarded value.
func effectivePoolSizeForMemory(_ int) int { return 1 }

// warnPoolSizeOverride logs a warning when the operator explicitly
// set --pool-size to a value > 1 but this build collapses it to a
// single in-process handler. The explicit gate matters because
// --pool-size has a non-zero default (4) inherited from the
// subprocess design — warning on every default startup would be
// noise. The cobra Changed() bit lets the helper distinguish "the
// operator typed this number" from "this is just the flag default".
func warnPoolSizeOverride(logger zerolog.Logger, requested int, explicit bool) {
	if !explicit || requested <= 1 {
		return
	}
	logger.Warn().
		Int("requested", requested).
		Msg("--pool-size > 1 has no effect in in-process builds; running a single in-process handler")
}
