//go:build inprocess

package main

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/internal/worker"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
)

// configureWorkerMode prepares the pool config for inprocess worker
// execution: initialises the BAML runtime in this process (the
// subprocess build does it inside cmd/worker; without a worker
// process we have to do it here), parses deployment-wide client
// defaults with the same fail-loud contract cmd/worker enforces, and
// installs a WorkerFactory that constructs internal/worker.Handler
// directly.
//
// The factory runs at initial pool fill AND on restart, so it must
// not call InitRuntime — re-initialising BAML on every handler
// rebuild would be wrong. InitRuntime fires exactly once here at
// server startup.
func configureWorkerMode(logger zerolog.Logger, cfg *pool.Config) error {
	worker.InitRuntime()

	clientDefaults, err := worker.LoadClientDefaults()
	if err != nil {
		return fmt.Errorf("invalid BAML_REST_CLIENT_DEFAULTS: %w", err)
	}
	if clientDefaults.HasKey("allowed_role_metadata") && buildrequest.UseBuildRequest() {
		logger.Warn().Msg(
			"BAML_REST_CLIENT_DEFAULTS sets allowed_role_metadata but " +
				"BAML_REST_USE_BUILD_REQUEST=true; message-level metadata " +
				"(e.g. cache_control) is dropped by the BuildRequest serializer " +
				"until the upstream TODOs are resolved: " +
				"baml_language/crates/sys_llm/src/build_request/openai.rs:100 and " +
				"baml_language/crates/sys_llm/src/build_request/anthropic.rs:91")
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
			Logger:         wcfg.Logger,
			Metrics:        worker.NewMetricsRegistry(),
			ClientDefaults: clientDefaults,
			SharedState:    hook,
		})
	}
	return nil
}

// effectivePoolSizeForMemory clamps to 1 because the pool itself
// will normalise PoolSize to 1 — memlimit.CalculateLimits and the
// per_worker startup log should report the actual layout, not the
// requested-but-discarded value.
func effectivePoolSizeForMemory(_ int) int { return 1 }

// warnPoolSizeOverride logs a one-shot warning when the operator
// asked for a multi-worker pool but the inprocess build will collapse
// it to a single handler. Hits the same warning surface a future
// --pool-size deprecation would use, so operators get the same
// message regardless of how the value was supplied (CLI flag,
// BAML_REST_POOL_SIZE, etc.).
func warnPoolSizeOverride(logger zerolog.Logger, requested int) {
	if requested <= 1 {
		return
	}
	logger.Warn().
		Int("requested", requested).
		Msg("--pool-size > 1 has no effect in inprocess builds; running a single in-process handler")
}
