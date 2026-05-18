package dynclient

import (
	"net/http"
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/workerplugin"
)

// Option configures a Client constructed via New. Options never read
// process env vars — every value is supplied by the caller.
type Option func(*config) error

// config is the internal, mutable view of a client's configuration. It
// is populated from Options and read once by New when wiring the
// underlying worker handler.
type config struct {
	buildRequest    bamlutils.BuildRequestConfig
	baseURLRewrites []urlrewrite.Rule
	logger          bamlutils.Logger
	metrics         *prometheus.Registry
	clientDefaults  *clientdefaults.Config
	httpClient      *http.Client
	sharedState     *workerplugin.SharedStateStore
}

// WithUseBuildRequest mirrors the BAML_REST_USE_BUILD_REQUEST gate for a
// single client. When enabled, the BuildRequest path drives dispatch for
// supported providers.
func WithUseBuildRequest(enabled bool) Option {
	return func(c *config) error {
		c.buildRequest.UseBuildRequest = enabled
		return nil
	}
}

// WithDisableCallBuildRequest mirrors BAML_REST_DISABLE_CALL_BUILD_REQUEST.
// When true, the non-streaming Request API is treated as unsupported and
// /call{,-with-raw} fall through to the stream-accumulation bridge or
// legacy path.
func WithDisableCallBuildRequest(disabled bool) Option {
	return func(c *config) error {
		c.buildRequest.DisableCallBuildRequest = disabled
		return nil
	}
}

// WithBaseURLRewrites installs URL rewrite rules applied both to
// outbound HTTP requests and to per-request client_registry base_url
// overrides. Passing nil clears any previously installed rules. The
// slice is cloned defensively.
func WithBaseURLRewrites(rules []BaseURLRewriteRule) Option {
	return func(c *config) error {
		if rules == nil {
			c.baseURLRewrites = nil
			return nil
		}
		c.baseURLRewrites = slices.Clone(rules)
		return nil
	}
}

// WithLogger installs the logger used by the worker bridge and the
// dynamic adapter for diagnostic output. The interface is compatible
// with hclog.Logger.
func WithLogger(logger Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

// WithMetricsRegistry installs the Prometheus registry the worker will
// register its internal collectors against. When nil, the worker
// constructs a default registry.
func WithMetricsRegistry(reg *prometheus.Registry) Option {
	return func(c *config) error {
		c.metrics = reg
		return nil
	}
}

// WithClientDefaults installs deployment-wide client defaults that are
// merged into each request's client_registry before BAML sees it.
// Callers wanting env parity can pass clientdefaults.Load() explicitly.
func WithClientDefaults(cfg *clientdefaults.Config) Option {
	return func(c *config) error {
		c.clientDefaults = cfg
		return nil
	}
}

// WithHTTPClient installs the underlying *http.Client used for outbound
// LLM traffic on the BuildRequest path. Passing nil resets to the
// library default and does not panic.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) error {
		c.httpClient = client
		return nil
	}
}

// WithSharedStateStore installs a shared-state store so concurrent
// requests against this client coordinate baml-roundrobin counters
// pool-wide instead of per-call. Passing nil disables the hook.
func WithSharedStateStore(store *SharedStateStore) Option {
	return func(c *config) error {
		c.sharedState = store
		return nil
	}
}
