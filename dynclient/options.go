package dynclient

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/workerplugin"
)

// Option configures a Client constructed via New. Options never read
// process env vars — every value is supplied by the caller.
type Option func(*config) error

// config is the internal, mutable view of a client's configuration. It
// is populated from Options and read once by New when wiring the
// underlying worker handler.
//
// HTTP backend selection and tuning are split across three orthogonal
// fields. clientMode (WithClientMode) selects which backend handles a
// request. netHTTPClient (WithNetHTTPClient) tunes net/http only.
// fastHTTPOptions (WithFastHTTPClient) tunes fasthttp only. The *Set
// companion booleans record whether the caller invoked the
// corresponding option at all — required so validate() can reject
// configurations like "forced net/http with fasthttp tuning" even when
// the tuning value would otherwise look like a zero value.
type config struct {
	deBAML               bamlutils.DeBAMLConfig
	deBAMLRender         bamlutils.DeBAMLRenderFunc
	deBAMLParse          bamlutils.DeBAMLParseFunc
	nativeShadow         bamlutils.NativeShadowFunc
	nativeServe          bamlutils.NativeServeFunc
	nativeStreamServe    bamlutils.NativeStreamServeFunc
	requestRetryOverride *bamlutils.RetryConfig
	baseURLRewrites      []urlrewrite.Rule
	logger               bamlutils.Logger
	metrics              *prometheus.Registry
	clientDefaults       *clientdefaults.Config
	sharedState          *workerplugin.SharedStateStore

	clientMode    llmhttp.ClientMode
	clientModeSet bool

	netHTTPClient    *http.Client
	netHTTPClientSet bool

	fastHTTPOptions llmhttp.FastHTTPClientOptions
	fastHTTPSet     bool

	// preserveSchemaOrderDefault is the default value used to resolve
	// a Request / ParseRequest with a nil PreserveSchemaOrder. dynclient
	// defaults this to true at New() time so direct Go callers get the
	// ordered output_format render they almost always want; the option
	// flips it back to false for callers who prefer alphabetical sort.
	// Per-request non-nil pointers always win over this default.
	preserveSchemaOrderDefault bool
}

// WithDeBAML mirrors the BAML_REST_USE_DEBAML umbrella switch for a
// single client. When enabled, native de-BAML behaviour runs on routes
// that expose a native seam — today, the native ctx.output_format
// renderer on the dynamic BuildRequest route. dynclient never reads the
// env var; callers opt in explicitly here. The native output_format seam
// lives on the BuildRequest route, which is unconditional as of #537, so
// de-BAML reaches the native seam on capable BAML versions without any
// route opt-in.
func WithDeBAML(enabled bool) Option {
	return func(c *config) error {
		c.deBAML.Enabled = enabled
		return nil
	}
}

// WithDeBAMLRenderer injects the native ctx.output_format render callback.
// dynclient lives in a separate Go module and cannot import baml-rest's
// root internal/schema + outputformat packages, so a caller in the root
// module supplies the renderer (e.g. internal/debaml.Render). Without it,
// WithDeBAML has no renderer wired and the dynamic path stays
// BAML-as-today. Pairs with WithDeBAML (the enable switch); the native
// seam lives on the BuildRequest route, which is unconditional as of #537.
func WithDeBAMLRenderer(render bamlutils.DeBAMLRenderFunc) Option {
	return func(c *config) error {
		c.deBAMLRender = render
		return nil
	}
}

// WithDeBAMLParser injects the native response-parser callback, the
// parser-side twin of WithDeBAMLRenderer. dynclient lives in a separate Go
// module and cannot import baml-rest's root internal/schema package, so a
// caller in the root module supplies the parser (e.g. internal/debaml.Parse).
// Without it, the dynamic final-parse path has no native parser wired and
// stays BAML-as-today even with WithDeBAML enabled. Pairs with WithDeBAML
// (the enable switch).
func WithDeBAMLParser(parse bamlutils.DeBAMLParseFunc) Option {
	return func(c *config) error {
		c.deBAMLParse = parse
		return nil
	}
}

// WithNativeShadowComparator injects the native one-send SHADOW comparator
// (de-BAML cutover Slice 4). dynclient lives in a separate Go module and cannot
// import the out-of-go.work nanollm bridge, so a caller supplies the concrete
// nanollm-backed comparator (e.g. from the nanollmprepare shadow package). It is
// installed on every adapter; the generated dynamic call seam turns it into the
// Slice-1 native child-attempt callback only when it is non-nil AND WithDeBAML is
// enabled, and that callback ALWAYS declines to BAML after a no-socket plan
// comparison. Without it (or with WithDeBAML off) the dynamic call path stays
// BAML-as-today with zero native FFI / socket / plan build. It is the shadow
// deploy profile's equivalent for direct dynclient callers (tests, tooling).
func WithNativeShadowComparator(shadow bamlutils.NativeShadowFunc) Option {
	return func(c *config) error {
		c.nativeShadow = shadow
		return nil
	}
}

// WithNativeServeComparator injects the native SERVE implementation (de-BAML
// cutover Slice 6). dynclient lives in a separate Go module and cannot import the
// out-of-go.work nanollm bridge, so a caller supplies the concrete nanollm-backed
// implementation (e.g. from the nanollmprepare canary package). It is installed on
// every adapter; the generated dynamic call seam turns it into the Slice-1 native
// child-attempt callback only when it is non-nil AND WithDeBAML is enabled, and
// that callback actually SERVES an admitted unary `_dynamic` call natively (one
// exact RoundTrip) or declines pre-socket to BAML. Without it (or with WithDeBAML
// off) the dynamic call path stays BAML-as-today with zero native FFI / socket /
// plan build. It is the serve deploy profile's equivalent for direct dynclient
// callers (tests, tooling) and MUST NOT be combined with WithNativeShadowComparator
// — a caller installs at most one native callback.
func WithNativeServeComparator(serve bamlutils.NativeServeFunc) Option {
	return func(c *config) error {
		c.nativeServe = serve
		return nil
	}
}

// WithNativeStreamServeComparator injects the native STREAM SERVE implementation
// (de-BAML Phase 7D), the streaming twin of WithNativeServeComparator. A caller in
// the root module supplies the concrete nanollm-backed implementation (e.g.
// nativeserve.NewStream). It is installed on every adapter; the generated dynamic
// StreamRequest seam turns it into StreamConfig.NativeAttempt only when it is
// non-nil AND WithDeBAML is enabled, and that callback actually SERVES an admitted
// dynamic OpenAI `/stream{,-with-raw}/_dynamic` request natively (one exact
// RoundTrip driving DoStream) or declines pre-transport to BAML. Without it (or with
// WithDeBAML off) the dynamic stream path stays BAML-as-today with zero native FFI /
// socket / plan build. It coexists with WithNativeServeComparator (a serve caller
// installs both unary + stream).
func WithNativeStreamServeComparator(serve bamlutils.NativeStreamServeFunc) Option {
	return func(c *config) error {
		c.nativeStreamServe = serve
		return nil
	}
}

// WithRequestRetryOverride sets a per-request retry override applied to every
// dynamic call this client makes, threaded through __baml_options__.retry — the
// same seam a REST caller uses. The worker's option decoder installs it via
// adapter.SetRetryConfig, so the strategy-aware resolver returns a non-nil request
// retry policy (RetryConfigToPolicy). Passing nil (the default) leaves the worker
// input byte-identical and retry resolution unchanged. Direct Go callers use this
// to exercise the resolved-retry-policy request shape without a REST round-trip;
// under the shadow comparator (WithNativeShadowComparator + WithDeBAML) such a
// request is a truthful retry-override the single-attempt exact lane bypasses, so
// the comparator declines at the S3 strategy gate before building any plan.
func WithRequestRetryOverride(retry *RetryConfig) Option {
	return func(c *config) error {
		c.requestRetryOverride = retry
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

// WithClientMode selects which HTTP backend handles outbound LLM
// traffic. Backend selection and backend tuning are orthogonal axes:
// this option is the only one that picks a backend. WithNetHTTPClient
// and WithFastHTTPClient configure their respective backends without
// implying a mode.
//
// The default mode (zero value) is llmhttp.ClientModeAuto, which routes
// each origin to net/http or fasthttp based on a per-origin ALPN probe.
// Under Auto, both backends can be tuned simultaneously and each
// applies to the origins it actually serves.
//
// Forced modes reject tuning aimed at the unused backend: combining
// ClientModeNetHTTP with WithFastHTTPClient (or ClientModeFastHTTP with
// WithNetHTTPClient) is a hard error from New so the misconfiguration
// surfaces at startup rather than silently no-op'ing at request time.
// Invalid llmhttp.ClientMode values are also rejected.
//
// Repeated calls are last-wins.
func WithClientMode(mode llmhttp.ClientMode) Option {
	return func(c *config) error {
		c.clientMode = mode
		c.clientModeSet = true
		return nil
	}
}

// WithNetHTTPClient supplies a custom *http.Client for the net/http
// backend. Passing nil records that net/http tuning was requested but
// supplies no replacement: the default tuned net/http transport (via
// llmhttp.NewDefaultClientWithOptions) still applies. The nil case
// still counts as "net/http tuning supplied" for validation purposes,
// so combining WithClientMode(ClientModeFastHTTP) with
// WithNetHTTPClient(nil) is a hard error.
//
// This option tunes only the net/http backend. It does not select a
// mode; under the default Auto mode the supplied client governs
// origins routed to net/http while fasthttp-routed origins are
// unaffected. Use WithClientMode to force a specific backend.
//
// Repeated calls are last-wins.
func WithNetHTTPClient(client *http.Client) Option {
	return func(c *config) error {
		c.netHTTPClient = client
		c.netHTTPClientSet = true
		return nil
	}
}

// WithFastHTTPClient tunes the fasthttp backend (MaxConns,
// MaxConnWaitTimeout, MaxIdleConnDuration, TLSConfig). The options
// apply only to origins that route through fasthttp.
//
// This option tunes only the fasthttp backend. It does not select a
// mode; under the default Auto mode the values apply to fasthttp-routed
// origins while net/http-routed origins are unaffected. Use
// WithClientMode to force a specific backend.
//
// Repeated calls are last-wins.
func WithFastHTTPClient(opts llmhttp.FastHTTPClientOptions) Option {
	return func(c *config) error {
		c.fastHTTPOptions = opts
		c.fastHTTPSet = true
		return nil
	}
}

// WithPreserveSchemaOrderDefault overrides the dynclient default for
// per-request preserve_schema_order resolution. dynclient defaults to
// true at New() — direct Go callers using OrderedMap literals get the
// rendered output_format following their construction order without
// extra opt-in. Pass false to flip the default to alphabetical sort.
//
// Per-request non-nil PreserveSchemaOrder values always win over this
// default: a request explicitly setting *true preserves order even
// under WithPreserveSchemaOrderDefault(false), and a request setting
// *false sorts even under the default true. Only nil/absent requests
// inherit.
func WithPreserveSchemaOrderDefault(enabled bool) Option {
	return func(c *config) error {
		c.preserveSchemaOrderDefault = enabled
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

// validate enforces the interaction rules between WithClientMode,
// WithNetHTTPClient, and WithFastHTTPClient. Selection and tuning are
// orthogonal under Auto mode, but forced modes reject tuning for the
// unused backend so misconfiguration surfaces at New() rather than as
// silent no-ops at request time.
//
// Runs on the final config so option order doesn't matter — repeated
// WithClientMode calls are last-wins.
func (c *config) validate() error {
	if c == nil {
		return nil
	}
	if c.clientModeSet && !validClientMode(c.clientMode) {
		return fmt.Errorf("WithClientMode: invalid llmhttp.ClientMode %d", c.clientMode)
	}
	if !c.clientModeSet || c.clientMode == llmhttp.ClientModeAuto {
		return nil
	}

	switch c.clientMode {
	case llmhttp.ClientModeNetHTTP:
		if c.fastHTTPSet {
			return fmt.Errorf("WithClientMode(llmhttp.ClientModeNetHTTP) conflicts with WithFastHTTPClient: fasthttp tuning has no effect when the backend is forced to net/http")
		}
	case llmhttp.ClientModeFastHTTP:
		if c.netHTTPClientSet {
			return fmt.Errorf("WithClientMode(llmhttp.ClientModeFastHTTP) conflicts with WithNetHTTPClient: net/http tuning has no effect when the backend is forced to fasthttp")
		}
	}
	return nil
}

// validClientMode reports whether the supplied ClientMode is a known
// enum value. The llmhttp package treats unknown raw values from env as
// Auto, but for an explicit programmatic option a typo is a bug, not a
// silent fallback.
func validClientMode(mode llmhttp.ClientMode) bool {
	switch mode {
	case llmhttp.ClientModeAuto, llmhttp.ClientModeFastHTTP, llmhttp.ClientModeNetHTTP:
		return true
	default:
		return false
	}
}
