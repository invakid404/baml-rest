// Package dynclient is the in-process Go client for the baml-rest
// dynamic endpoints (/call, /call-with-raw, /stream, /stream-with-raw,
// /parse). It wraps the same worker handler the HTTP server uses, so
// dynamic requests issued through Client follow the same dispatch,
// retry, and metadata contract as their HTTP counterparts — without
// crossing a network boundary.
//
// Configuration is explicit-only. New never reads process environment
// variables. Callers that want the server's env defaults must load them
// explicitly and pass the values through options:
//
//	defaults, err := clientdefaults.Load()
//	rewrites := urlrewrite.LoadDefaultRules()
//	client, err := dynclient.New(
//	    dynclient.WithClientDefaults(defaults),
//	    dynclient.WithBaseURLRewrites(rewrites),
//	)
//
// client_registry is supplied per-request on Request — there is no
// client-wide WithClientRegistry option, matching the HTTP endpoint
// contract.
package dynclient

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/goccy/go-json"
	"github.com/google/uuid"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient/internal/generated"
	"github.com/invakid404/baml-rest/internal/worker"
	"github.com/invakid404/baml-rest/workerplugin"
)

// runtimeInitOnce gates the process-wide BAML runtime initialization.
// generated.Runtime{}.InitRuntime() loads the shared library via the
// upstream sync.Once wired in by cmd/hacks; the additional Once here
// keeps multiple New() calls in the same process from racing against
// each other before that inner Once is reached.
var runtimeInitOnce sync.Once

// Client is a public in-process dynamic BAML client. The zero value is
// not usable — construct via New.
type Client struct {
	handler *worker.Handler
	store   *workerplugin.SharedStateStore
	logger  bamlutils.Logger
}

// errNilClient is returned by Client methods when called on a nil
// receiver. Exported as a sentinel so callers using errors.Is can tell
// the failure apart from a wrapped runtime error.
var errNilClient = errors.New("dynclient: nil *Client")

// New constructs a Client from the supplied options. The default
// runtime is the generated dynamic BAML runtime; initialization is
// guarded by a package-level sync.Once so concurrent New calls share a
// single InitRuntime invocation.
func New(opts ...Option) (*Client, error) {
	return newWithRuntime(generated.Runtime{}, func() {
		runtimeInitOnce.Do(func() {
			generated.Runtime{}.InitRuntime()
		})
	}, opts...)
}

// newWithRuntime is the unexported constructor seam used by tests. The
// init callback is invoked exactly once at construction time (the
// caller is responsible for any de-duplication semantics). Tests pass a
// no-op init alongside a fake runtime so the native BAML CFFI is never
// loaded.
func newWithRuntime(rt worker.Runtime, init func(), opts ...Option) (*Client, error) {
	cfg := &config{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("dynclient: new client: %w", err)
		}
	}

	if init != nil {
		init()
	}

	httpClient := newLLMHTTPClient(cfg)

	workerCfg := worker.Config{
		Runtime:         rt,
		Logger:          cfg.logger,
		Metrics:         cfg.metrics,
		ClientDefaults:  cfg.clientDefaults,
		BuildRequest:    cfg.buildRequest,
		BaseURLRewrites: cfg.baseURLRewrites,
		HTTPClient:      httpClient,
	}
	if cfg.sharedState != nil {
		workerCfg.SharedState = worker.NewStoreSharedStateHook(cfg.sharedState)
	}

	handler, err := worker.New(workerCfg)
	if err != nil {
		return nil, fmt.Errorf("dynclient: new client: %w", err)
	}

	return &Client{
		handler: handler,
		store:   cfg.sharedState,
		logger:  cfg.logger,
	}, nil
}

// newLLMHTTPClient builds the per-handler llmhttp.Client. Caller-supplied
// HTTP clients are wrapped explicitly; otherwise the tuned default LLM
// transport is used. Either path passes the configured rewrite rules so
// outbound requests are rewritten consistently with the registry pass.
func newLLMHTTPClient(cfg *config) *llmhttp.Client {
	opts := llmhttp.ClientOptions{RewriteRules: cfg.baseURLRewrites}
	if cfg.httpClient != nil {
		opts.HTTPClient = cfg.httpClient
		return llmhttp.NewClientWithOptions(opts)
	}
	return llmhttp.NewDefaultClientWithOptions(opts)
}

// DynamicCall executes a non-streaming dynamic call. Returns the final
// flattened JSON payload along with any planned/outcome metadata events
// the orchestrator emitted.
func (c *Client) DynamicCall(ctx context.Context, req Request) (*CallResult, error) {
	if c == nil {
		return nil, errNilClient
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}
	input, err := req.ToWorkerInput()
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}

	ctx, dropScope := c.beginScope(ctx)
	defer dropScope()

	results, err := c.handler.CallStream(ctx, bamlutils.DynamicMethodName, input, bamlutils.StreamModeCall)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}

	data, _, _, metadata, err := drainCall(results, false)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}
	flattened, err := bamlutils.FlattenDynamicOutput(data)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}
	return &CallResult{Data: flattened, Metadata: metadata}, nil
}

// DynamicCallRaw is like DynamicCall but also returns the raw upstream
// LLM response and any reasoning/thinking text the provider produced.
func (c *Client) DynamicCallRaw(ctx context.Context, req Request) (*CallRawResult, error) {
	if c == nil {
		return nil, errNilClient
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}
	input, err := req.ToWorkerInput()
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}

	ctx, dropScope := c.beginScope(ctx)
	defer dropScope()

	results, err := c.handler.CallStream(ctx, bamlutils.DynamicMethodName, input, bamlutils.StreamModeCallWithRaw)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}

	data, raw, reasoning, metadata, err := drainCall(results, true)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}
	flattened, err := bamlutils.FlattenDynamicOutput(data)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}
	return &CallRawResult{
		Data:      flattened,
		Raw:       raw,
		Reasoning: reasoning,
		Metadata:  metadata,
	}, nil
}

// DynamicParse parses a raw LLM response against the request's output
// schema and returns the flattened JSON payload.
func (c *Client) DynamicParse(ctx context.Context, req ParseRequest) (*ParseResult, error) {
	if c == nil {
		return nil, errNilClient
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}
	input, err := req.ToWorkerInput()
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}

	result, err := c.handler.Parse(ctx, bamlutils.DynamicMethodName, input)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}
	data := append(json.RawMessage(nil), result.Data...)
	flattened, err := bamlutils.FlattenDynamicOutput(data)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}
	return &ParseResult{Data: flattened}, nil
}

// DynamicStream starts a partial+final streaming dynamic call. Callers
// must invoke Stream.Close (or drain to io.EOF) so the request scope
// and any buffered worker results are released.
func (c *Client) DynamicStream(ctx context.Context, req Request) (*Stream, error) {
	return c.openStream(ctx, req, bamlutils.StreamModeStream, "dynamic stream")
}

// DynamicStreamRaw starts a partial+final streaming call that also
// surfaces raw and reasoning channels.
func (c *Client) DynamicStreamRaw(ctx context.Context, req Request) (*Stream, error) {
	return c.openStream(ctx, req, bamlutils.StreamModeStreamWithRaw, "dynamic stream raw")
}

// openStream is the common setup path for DynamicStream and
// DynamicStreamRaw. The errLabel customizes the wrapper string so
// callers can tell the two paths apart.
func (c *Client) openStream(ctx context.Context, req Request, mode bamlutils.StreamMode, errLabel string) (*Stream, error) {
	if c == nil {
		return nil, errNilClient
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("dynclient: %s: %w", errLabel, err)
	}
	input, err := req.ToWorkerInput()
	if err != nil {
		return nil, fmt.Errorf("dynclient: %s: %w", errLabel, err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	scopedCtx, dropScope := c.beginScope(streamCtx)

	results, err := c.handler.CallStream(scopedCtx, bamlutils.DynamicMethodName, input, mode)
	if err != nil {
		dropScope()
		cancel()
		return nil, fmt.Errorf("dynclient: %s: %w", errLabel, err)
	}

	return newStream(streamCtx, cancel, dropScope, results, mode.NeedsRaw(), errLabel), nil
}

// beginScope generates a request id, threads it through the context for
// the worker's shared-state hook, and returns a cleanup that drops the
// scope from the store exactly once. When no shared-state store is
// configured the cleanup is a no-op and the context is returned
// unchanged.
func (c *Client) beginScope(ctx context.Context) (context.Context, func()) {
	if c.store == nil {
		return ctx, func() {}
	}
	requestID := uuid.NewString()
	ctx = workerplugin.WithRequestID(ctx, requestID)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			c.store.DropScope(requestID)
		})
	}
}

// drainCall consumes worker stream results until it observes a final or
// error frame, copying every payload byte slice before releasing the
// underlying result. needRaw controls whether the raw and reasoning
// channels are propagated to the caller.
func drainCall(results <-chan *workerplugin.StreamResult, needRaw bool) (json.RawMessage, string, string, []Metadata, error) {
	var (
		data      json.RawMessage
		raw       string
		reasoning string
		metadata  []Metadata
	)
	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			err := workerplugin.NewErrorWithMetadata(result.Error, result.Stacktrace, result.ErrorCode, result.ErrorDetails)
			workerplugin.ReleaseStreamResult(result)
			drainAndRelease(results)
			return nil, "", "", nil, err
		case workerplugin.StreamResultKindFinal:
			data = append(json.RawMessage(nil), result.Data...)
			if needRaw {
				raw = result.Raw
				reasoning = result.Reasoning
			}
			workerplugin.ReleaseStreamResult(result)
			drainAndRelease(results)
			return data, raw, reasoning, metadata, nil
		case workerplugin.StreamResultKindMetadata:
			md, err := decodeMetadata(result.Data)
			workerplugin.ReleaseStreamResult(result)
			if err != nil {
				drainAndRelease(results)
				return nil, "", "", nil, err
			}
			if md != nil {
				metadata = append(metadata, *md)
			}
		default:
			workerplugin.ReleaseStreamResult(result)
		}
	}
	return nil, "", "", nil, errors.New("no final result received")
}

// decodeMetadata unmarshals a metadata frame payload into Metadata.
// Empty payloads are silently dropped — the worker may emit
// metadata-kind frames with an empty Data slice on retry paths.
func decodeMetadata(data []byte) (*Metadata, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var md Metadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	return &md, nil
}

// drainAndRelease consumes any remaining worker results and returns
// them to their pool. Used after a final or error frame so the bridge
// goroutine can close its output channel without leaking pooled
// allocations.
func drainAndRelease(results <-chan *workerplugin.StreamResult) {
	for r := range results {
		workerplugin.ReleaseStreamResult(r)
	}
}
