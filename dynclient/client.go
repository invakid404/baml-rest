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

	stdjson "encoding/json"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient/internal/generated"
	"github.com/invakid404/baml-rest/worker"
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
	// preserveSchemaOrderDefault is the resolved default for
	// per-request preserve_schema_order — captured from config at
	// construction time so request paths do not need to consult the
	// config again. Defaults to true; WithPreserveSchemaOrderDefault
	// flips it.
	preserveSchemaOrderDefault bool
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
//
// Validation runs after all options have been applied but BEFORE the
// init callback so a misconfigured client never reaches runtime
// initialization (which loads the native BAML library). Tests rely on
// this ordering to assert that error paths leave the init counter at
// zero.
func newWithRuntime(rt worker.Runtime, init func(), opts ...Option) (*Client, error) {
	cfg := &config{
		// dynclient defaults preserve_schema_order to true so direct
		// Go callers using OrderedMap literals get their construction
		// order rendered into the output_format prompt without needing
		// to set PreserveSchemaOrder on every request.
		preserveSchemaOrderDefault: true,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("dynclient: new client: %w", err)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("dynclient: new client: %w", err)
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
		DeBAML:          cfg.deBAML,
		DeBAMLRender:    cfg.deBAMLRender,
		DeBAMLParse:     cfg.deBAMLParse,
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
		handler:                    handler,
		store:                      cfg.sharedState,
		logger:                     cfg.logger,
		preserveSchemaOrderDefault: cfg.preserveSchemaOrderDefault,
	}, nil
}

// applyPreserveSchemaOrderDefault resolves a request's nil
// PreserveSchemaOrder to the client default. Request / ParseRequest
// are passed by value into the public Dynamic* methods, so mutating
// the local copy here cannot leak back to the caller.
func (c *Client) applyPreserveSchemaOrderDefault(p **bool) {
	if *p == nil {
		v := c.preserveSchemaOrderDefault
		*p = &v
	}
}

// newLLMHTTPClient builds the per-handler llmhttp.Client. The backend
// mode (zero is Auto) and fasthttp tuning always flow through; a
// caller-supplied *http.Client wraps an existing instance, otherwise
// the tuned default LLM transport is used. Either path passes the
// configured rewrite rules so outbound requests are rewritten
// consistently with the registry pass.
//
// netHTTPClientSet without a non-nil client (i.e. WithNetHTTPClient(nil))
// preserves the default tuned transport — validation has already
// guaranteed this combination is allowed (Auto or ClientModeNetHTTP).
func newLLMHTTPClient(cfg *config) *llmhttp.Client {
	opts := llmhttp.ClientOptions{
		Mode:         cfg.clientMode,
		RewriteRules: cfg.baseURLRewrites,
		FastHTTP:     cfg.fastHTTPOptions,
	}
	if cfg.netHTTPClientSet && cfg.netHTTPClient != nil {
		opts.NetHTTPClient = cfg.netHTTPClient
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
	c.applyPreserveSchemaOrderDefault(&req.PreserveSchemaOrder)
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
	flattened, err = bamlutils.InjectAbsentOptionals(flattened, req.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call: %w", err)
	}
	if req.PreserveSchemaOrder != nil && *req.PreserveSchemaOrder {
		flattened, err = bamlutils.ReorderDynamicOutputBySchema(flattened, req.OutputSchema)
	} else {
		flattened, err = bamlutils.SortDynamicOutput(flattened)
	}
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
	c.applyPreserveSchemaOrderDefault(&req.PreserveSchemaOrder)
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
	flattened, err = bamlutils.InjectAbsentOptionals(flattened, req.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic call raw: %w", err)
	}
	if req.PreserveSchemaOrder != nil && *req.PreserveSchemaOrder {
		flattened, err = bamlutils.ReorderDynamicOutputBySchema(flattened, req.OutputSchema)
	} else {
		flattened, err = bamlutils.SortDynamicOutput(flattened)
	}
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
//
// When req.Stream is true it drives BAML's parse-stream (partial) path over
// the raw text instead of the final parse, exposing a direct parse-stream
// oracle. The partial output is normalized through the identical
// flatten / absent-optional-injection / ordering pipeline, so a streaming
// prefix's result is directly comparable to a final parse's — this is what
// the bamlfuzz parse-recovery streaming differential consumes. BAML may
// reject an early prefix; that surfaces as a normal parse error here.
func (c *Client) DynamicParse(ctx context.Context, req ParseRequest) (*ParseResult, error) {
	if c == nil {
		return nil, errNilClient
	}
	c.applyPreserveSchemaOrderDefault(&req.PreserveSchemaOrder)
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
	data := append(stdjson.RawMessage(nil), result.Data...)
	flattened, err := bamlutils.FlattenDynamicOutput(data)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}
	flattened, err = bamlutils.InjectAbsentOptionals(flattened, req.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("dynclient: dynamic parse: %w", err)
	}
	if req.PreserveSchemaOrder != nil && *req.PreserveSchemaOrder {
		flattened, err = bamlutils.ReorderDynamicOutputBySchema(flattened, req.OutputSchema)
	} else {
		flattened, err = bamlutils.SortDynamicOutput(flattened)
	}
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
	c.applyPreserveSchemaOrderDefault(&req.PreserveSchemaOrder)
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

	preserveOrder := req.PreserveSchemaOrder != nil && *req.PreserveSchemaOrder
	return newStream(streamCtx, cancel, dropScope, results, mode.NeedsRaw(), errLabel, req.OutputSchema, preserveOrder), nil
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
func drainCall(results <-chan *workerplugin.StreamResult, needRaw bool) (stdjson.RawMessage, string, string, []Metadata, error) {
	var (
		data      stdjson.RawMessage
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
			data = append(stdjson.RawMessage(nil), result.Data...)
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
	if err := sonic.Unmarshal(data, &md); err != nil {
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
