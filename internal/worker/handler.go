package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/goccy/go-json"
	"github.com/prometheus/client_golang/prometheus"

	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/workerplugin"
)

// Config carries the construction-time dependencies for a Handler. Any
// field may be left at its zero value:
//
//   - Logger nil: bridge/drain paths and the round-robin warnings become
//     silent for that handler instance.
//   - Metrics nil: New constructs a default registry with the same Go +
//     process collectors NewMetricsRegistry would have produced.
//   - ClientDefaults nil: BAML_REST_CLIENT_DEFAULTS overrides aren't
//     applied. clientdefaults.Config.Apply is nil-safe.
//   - SharedState nil: the handler logs the existing once-per-process
//     warning the first time a request tries to use round-robin shared
//     state, then falls back to the in-process Coordinator.
type Config struct {
	Logger         bamlutils.Logger
	Metrics        *prometheus.Registry
	ClientDefaults *clientdefaults.Config
	SharedState    SharedStateHook
}

// Handler is the worker-side request handler extracted from
// cmd/worker/main.go. It satisfies workerplugin.Worker so the subprocess
// binary can hand it to goplugin.Serve without wrapping. Process-global
// state from the previous package-main layout (client defaults,
// shared-state client, logger, warning sync.Once values) now lives on
// the Handler so the type is constructible without hidden initialization.
type Handler struct {
	logger         bamlutils.Logger
	metricsReg     *prometheus.Registry
	clientDefaults *clientdefaults.Config

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
	metricsReg := cfg.Metrics
	if metricsReg == nil {
		metricsReg = NewMetricsRegistry()
	}
	h := &Handler{
		logger:         cfg.Logger,
		metricsReg:     metricsReg,
		clientDefaults: cfg.ClientDefaults,
	}
	if cfg.SharedState != nil {
		h.SetSharedStateHook(cfg.SharedState)
	}
	return h, nil
}

// CallStream executes a streaming BAML method and bridges its results
// onto the worker plugin's stream channel.
func (h *Handler) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	method, ok := baml_rest.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input — the typed input struct ignores unknown fields like __baml_options__
	input := method.MakeInput()
	if err := json.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options separately — only extracts __baml_options__ field.
	// This is a second pass over the same JSON. A single-pass approach would
	// require a combined struct, but the input type is generated per-method
	// and not known at compile time. The cost is minor for typical payloads.
	var options workerBamlOptions
	if err := json.Unmarshal(inputJSON, &options); err != nil {
		return nil, fmt.Errorf("failed to unmarshal options: %w", err)
	}

	// Create adapter and apply options
	adapter := baml_rest.MakeAdapter(ctx)
	adapter.SetLogger(h.logger)
	adapter.SetStreamMode(streamMode)
	// Install a per-request round-robin Advancer that delegates to the
	// host-side SharedState store. Safe to call unconditionally: returns
	// nil when no shared-state hook is attached, and the adapter treats
	// nil as "fall back to the introspected default Coordinator".
	adapter.SetRoundRobinAdvancer(h.roundRobinAdvancerFor(ctx))
	if err := options.apply(adapter, h.clientDefaults); err != nil {
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
