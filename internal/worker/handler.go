package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/goccy/go-json"
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
//   - BuildRequest: zero value disables the BuildRequest path on this
//     handler and propagates to the generated router via the adapter's
//     BuildRequestConfig() accessor.
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

	BuildRequest    bamlutils.BuildRequestConfig
	BaseURLRewrites []urlrewrite.Rule
	HTTPClient      *llmhttp.Client
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

	buildRequest    bamlutils.BuildRequestConfig
	baseURLRewrites []urlrewrite.Rule
	httpClient      *llmhttp.Client

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
		runtime:         cfg.Runtime,
		logger:          cfg.Logger,
		metricsReg:      metricsReg,
		clientDefaults:  cfg.ClientDefaults,
		buildRequest:    cfg.BuildRequest,
		baseURLRewrites: cfg.BaseURLRewrites,
		httpClient:      cfg.HTTPClient,
	}
	if cfg.SharedState != nil {
		h.SetSharedStateHook(cfg.SharedState)
	}
	return h, nil
}

// configureAdapter installs the per-handler BuildRequest config and
// HTTP client on a freshly-minted adapter. Both setters are part of
// the bamlutils.Adapter interface and are no-ops on adapter versions
// that don't honour them (HasHTTPClient=false in codegen options
// emits a no-op SetHTTPClient).
func (h *Handler) configureAdapter(adapter bamlutils.Adapter) {
	adapter.SetBuildRequestConfig(h.buildRequest)
	adapter.SetHTTPClient(h.httpClient)
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
