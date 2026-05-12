// Package buildrequest provides the runtime orchestrators for the
// BuildRequest/StreamRequest call and streaming paths. This replaces the
// CallStream+OnTick pipeline for providers that support the modular API.
//
// The generated adapter code calls RunStreamOrchestration with
// provider-specific closures. This package handles SSE event consumption,
// delta extraction, ParseStream throttling, retry logic, and heartbeat
// emission — producing the same StreamResult channel shape as the legacy paths.
package buildrequest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
	"github.com/invakid404/baml-rest/bamlutils/sse"
)

// parseBuildRequestEnv reads BAML_REST_USE_BUILD_REQUEST and returns its
// boolean interpretation. Extracted so the test can verify parsing logic
// independently of the cache.
func parseBuildRequestEnv() bool {
	v := os.Getenv("BAML_REST_USE_BUILD_REQUEST")
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// useBuildRequestOnce caches the result of parseBuildRequestEnv. The env var
// is read once on first call and the result is reused for all subsequent
// calls. This avoids repeated os.Getenv (which acquires a lock) on every
// request dispatch — the generated router reads the cached value once at
// router entry and threads it into every gate (BuildRequest landing,
// legacy predicate, and the supportsWithClient ResolveEffectiveClient
// upgrade) so the same boolean drives every decision in a request.
var useBuildRequestOnce sync.Once
var useBuildRequestCached bool

// UseBuildRequest returns true if the BuildRequest/StreamRequest paths are enabled.
// Controlled by the BAML_REST_USE_BUILD_REQUEST environment variable.
//
// When false, the flag is a full rollback to the legacy CallStream+OnTick
// path: the BuildRequest transport is skipped, baml-rest's RR resolver
// and coordinator are NOT engaged on supportsWithClient adapters, and
// BAML's own runtime owns strategy rotation per-worker. This is the
// kill-switch contract — flipping the flag off should remove every new
// failure surface this PR added (RemoteAdvancer, SharedState broker,
// in-process coordinator) from the request path so operators can revert
// to legacy semantics during incident response.
//
// Scope: the flag is read by both the request-path gates (codegen
// upgrade in adapters/common/codegen, ResolveEffectiveClient consumers)
// and the host startup wiring in cmd/serve. cmd/serve only seeds
// SharedStateSeeds when the flag is on; with it off, the host doesn't
// host a SharedStateStore, the plugin map ships without
// SharedStateImpl, no reverse broker is accepted, and the worker's
// AttachSharedState handshake never runs. That keeps the kill-switch
// rollback genuinely surface-free: the broker, store, and remote
// advancer are all skipped end-to-end rather than constructed and
// merely unused.
//
// The environment variable is read once and cached for the process lifetime.
func UseBuildRequest() bool {
	useBuildRequestOnce.Do(func() {
		useBuildRequestCached = parseBuildRequestEnv()
	})
	return useBuildRequestCached
}

// parseDisableCallBuildRequestEnv reads BAML_REST_DISABLE_CALL_BUILD_REQUEST
// and returns its boolean interpretation. Extracted so the test can verify
// parsing logic independently of the cache.
func parseDisableCallBuildRequestEnv() bool {
	v := os.Getenv("BAML_REST_DISABLE_CALL_BUILD_REQUEST")
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// disableCallBuildRequestOnce caches the result of the env lookup so
// IsCallProviderSupported stays hot. Same rationale as useBuildRequestOnce.
var disableCallBuildRequestOnce sync.Once
var disableCallBuildRequestCached bool

// disableCallBuildRequest returns true when BAML_REST_DISABLE_CALL_BUILD_REQUEST
// is set, forcing the non-streaming Request API off for all providers. The
// call-mode router block then declines and /call{,-with-raw} fall through to
// the stream-accumulation bridge (when StreamRequest is available) or legacy.
//
// Primary uses:
//
//   - Exercising the bridge path in integration tests without relying on an
//     organic divergence between callSupportedProviders and supportedProviders.
//   - An operational escape hatch if the non-streaming Request API regresses
//     for a provider; flipping this forces bridging at a latency cost while a
//     fix ships.
func disableCallBuildRequest() bool {
	disableCallBuildRequestOnce.Do(func() {
		disableCallBuildRequestCached = parseDisableCallBuildRequestEnv()
	})
	return disableCallBuildRequestCached
}

// supportedProviders is the set of providers whose streaming transport
// is handled by the BuildRequest orchestrator. Providers absent from
// the set fall back to the legacy CallStream+OnTick path.
//
// Two distinct transports live under this gate:
//
//   - SSE (text/event-stream) — every non-aws-bedrock entry. The
//     orchestrator's default path consumes sse.ExtractDeltaPartsFromText
//     on llmhttp.ExecuteStream events; the provider names below must
//     match that function's switch cases exactly.
//   - AWS event-stream (application/vnd.amazon.eventstream) —
//     aws-bedrock only. The orchestrator dispatches on provider to
//     llmhttp.ExecuteAWSStream + extractBedrockStreamDelta. The
//     dispatch contract is encoded in StreamConfig.BuildBedrockStreamRequest
//     and validated up-front; the SSE extractor is never consulted on
//     this path.
var supportedProviders = map[string]bool{
	"openai":           true,
	"openai-generic":   true,
	"azure-openai":     true,
	"ollama":           true,
	"openrouter":       true,
	"openai-responses": true,
	"anthropic":        true,
	"google-ai":        true,
	"vertex-ai":        true,
	// aws-bedrock (v0.219+): streaming uses Request.<Method> as a
	// body assembler (BAML's StreamRequest errors for AWS), then
	// mutates URL /converse → /converse-stream, signs SigV4 over
	// the rewritten URL, executes via llmhttp.ExecuteAWSStream, and
	// pipes decoded awsstream events through extractBedrockStreamDelta.
	// PR3-bedrock-stream breadcrumb (issue #243).
	"aws-bedrock": true,
}

// IsProviderSupported returns true if the provider's streaming transport
// is handled by the BuildRequest orchestrator — SSE
// (sse.ExtractDeltaPartsFromText on llmhttp.ExecuteStream events) for
// every non-aws-bedrock entry, AWS event-stream
// (extractBedrockStreamDelta on llmhttp.ExecuteAWSStream events) for
// aws-bedrock. Unknown providers fall back to the legacy
// CallStream+OnTick path.
func IsProviderSupported(provider string) bool {
	return supportedProviders[provider]
}

// callSupportedProviders is the set of providers whose non-streaming JSON
// response format is handled by ExtractResponseContent. This is separate
// from supportedProviders because the streaming path requires per-
// provider streaming-transport compatibility (SSE for most, AWS
// event-stream for aws-bedrock) while the non-streaming path requires
// JSON response format compatibility — different constraints that may
// evolve independently.
var callSupportedProviders = map[string]bool{
	"openai":           true,
	"openai-generic":   true,
	"azure-openai":     true,
	"ollama":           true,
	"openrouter":       true,
	"openai-responses": true,
	"anthropic":        true,
	"google-ai":        true,
	"vertex-ai":        true,
	// aws-bedrock (v0.219+): both call and stream go through
	// BuildRequest. See supportedProviders above for the streaming
	// wire-path summary. Scope cuts: v0.204/v0.215 fall through to
	// legacy (codegen's _buildCallRequest is gated on
	// introspected.Request, which only resolves on v0.219); the
	// default credential chain is used (no static .baml creds); the
	// default Bedrock runtime endpoint is used (no endpoint_url
	// override); reasoning signature/redactedContent and tool-use
	// streaming deltas are silently skipped. PR1-bedrock /
	// PR3-bedrock-stream breadcrumb (issue #243).
	"aws-bedrock": true,
}

// IsCallProviderSupported returns true if the provider's non-streaming JSON
// response format is handled by ExtractResponseContent and the provider
// supports BAML's Request API. Unknown providers fall back to the legacy
// CallStream+OnTick path.
//
// Returns false for every provider when BAML_REST_DISABLE_CALL_BUILD_REQUEST
// is set, which routes /call{,-with-raw} through the stream-accumulation
// bridge (or legacy, if StreamRequest is unavailable).
//
// Debug builds (-tags debug) additionally honour
// BAML_REST_CALL_UNSUPPORTED_PROVIDERS, a comma-separated list that marks
// specific providers as call-unsupported while keeping them stream-
// supported. This exists so integration tests can force the mixed-chain
// fall-through gate to fire without waiting for callSupportedProviders and
// supportedProviders to diverge organically. See debugFilterCallSupported
// in call_support_debug.go / call_support_stub.go.
func IsCallProviderSupported(provider string) bool {
	if disableCallBuildRequest() {
		return false
	}
	return debugFilterCallSupported(provider, callSupportedProviders[provider])
}

// StreamConfig holds the configuration for a single streaming request.
type StreamConfig struct {
	// Provider is the LLM provider name (e.g., "openai", "anthropic").
	// Used for SSE delta extraction. For single-provider clients, this
	// is the only provider used. For fallback chains, this is ignored
	// in favor of per-attempt provider resolution via ClientProviders.
	Provider string

	// RetryPolicy is the retry policy for this request. Nil means no retries.
	RetryPolicy *retry.Policy

	// NeedsPartials is true if the caller wants intermediate parsed partials.
	NeedsPartials bool

	// NeedsRaw is true if the caller wants raw LLM response text.
	NeedsRaw bool

	// IncludeReasoning is the per-request opt-in for surfacing provider-
	// specific reasoning/thinking text on the structured reasoning
	// channel of /with-raw endpoints, distinct from raw. When true, the
	// streaming extractor accumulates Anthropic thinking_delta / Gemini
	// thought parts / OpenAI Chat Completions reasoning_content / OpenAI
	// Responses reasoning_summary_text deltas into the reasoning channel
	// alongside text deltas. When false (default), the reasoning channel
	// stays empty. The flag never affects raw (always text-only) or the
	// parseable text passed to ParseStream.
	IncludeReasoning bool

	// ParseThrottleInterval is the minimum time between ParseStream calls.
	// Zero means parse on every SSE event (no throttling).
	ParseThrottleInterval time.Duration

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientOverride, when non-empty, is passed as the clientOverride
	// argument to buildRequest for single-provider attempts (FallbackChain
	// empty). Used by the round-robin path to direct BAML to build the
	// request for the RR-selected leaf client rather than the function's
	// default. Ignored for fallback-chain attempts — those iterate child
	// names and pass each as the per-attempt override.
	ClientOverride string

	// ClientProviders maps child client names to their provider strings.
	// Used with FallbackChain to resolve the provider for each attempt.
	// In mixed-mode chains this map includes both BuildRequest-supported
	// children and legacy (unsupported-provider) children — it is a pure
	// lookup table, not a "supported set." Callers must consult
	// LegacyChildren to decide which path runs a given child.
	ClientProviders map[string]string

	// LegacyChildren marks the entries in FallbackChain that must be driven
	// via the legacy BAML Stream API (WithClient + WithOnTick) rather than
	// the BuildRequest path. Populated by ResolveFallbackChain when the
	// chain mixes supported and unsupported providers. Children absent
	// from this map use the BuildRequest path (buildRequest closure).
	// When non-empty, LegacyStreamChild must be non-nil — the orchestrator
	// fails up-front validation otherwise.
	LegacyChildren map[string]bool

	// FallbackTargets is the dispatch-target counterpart of the metadata
	// field of the same name on bamlutils.Metadata — see that field's
	// doc string for the wire-shape contract. The BuildRequest dispatch
	// loop consults FallbackTargets[child] when computing the WithClient
	// target for an immediate RR fallback child that was centrally
	// unwrapped to a leaf. The orchestrator is the consumer; resolver/
	// planning upstream is the producer.
	FallbackTargets map[string]string

	// FallbackRoundRobin mirrors Metadata.FallbackRoundRobin — the
	// per-child RR decision for fallback children resolved through
	// BuildRequest centralization. Populated from resolver output so
	// outgoing planned metadata describes both the selected leaf
	// (FallbackTargets[child]) and the RR decision behind that
	// selection.
	FallbackRoundRobin map[string]*bamlutils.RoundRobinInfo

	// MetadataPlan is the pre-computed planned metadata for this request.
	// When non-nil, the orchestrator emits a single planned metadata event
	// upfront — before any HTTP work — so the routing decision is observable
	// even when the upstream provider never produces a 2xx (e.g., immediate
	// validation failure or hung connection). Heartbeat fires later on
	// upstream 2xx, and the outcome metadata event is emitted on success
	// right before the final result. Nil disables metadata emission.
	//
	// The orchestrator populates the outcome event's winner fields (winner
	// client, provider, path, retry count, upstream duration) from runtime
	// observations — callers supply only the planned portion.
	MetadataPlan *bamlutils.Metadata

	// NewMetadataResult constructs a pooled StreamResult wrapping a metadata
	// payload. Required when MetadataPlan is non-nil.
	NewMetadataResult NewMetadataResultFunc

	// BuildBedrockStreamRequest, when non-nil, builds the
	// non-streaming BAML Request.<Method> body and rewrites the URL
	// from /converse to /converse-stream for aws-bedrock streaming.
	// Required when the orchestrator is asked to stream against an
	// aws-bedrock provider (single or fallback-chain child). The
	// codegen-emitted closure also attaches the AWS event-stream
	// Accept header and calls llmhttp.MaybeAttachBedrockAuth to wire
	// the SigV4 signing hook.
	//
	// The two distinct closures (BuildRequest for SSE providers,
	// BuildBedrockStreamRequest for bedrock) are emitted side-by-side
	// because BAML's StreamRequest API errors for aws-bedrock — there
	// is no upstream-streaming builder we can call. PR3-bedrock-stream
	// breadcrumb (issue #243): PR 4 will swap this to BAML's modular
	// streaming builder if/when it lands.
	BuildBedrockStreamRequest BuildRequestFunc

	// LegacyStreamChild runs a single child via BAML's Stream API. The
	// orchestrator invokes it for children marked in LegacyChildren.
	//
	// Contract:
	//   - Invoke BAML's Stream.Method with WithClient(clientOverride) and
	//     wire a WithOnTick callback that invokes sendHeartbeat on the
	//     first FunctionLog tick (this matches the BuildRequest path's
	//     post-HTTP heartbeat liveness semantics — don't pre-emit the
	//     heartbeat before upstream activity, or the pool's hung detector
	//     is disabled prematurely).
	//   - Drain the stream to completion; do NOT emit partials on the
	//     orchestrator's output channel. The orchestrator is responsible
	//     for the single final emission.
	//   - Return (final, raw, err). When needsRaw is false the helper may
	//     return an empty raw string.
	//
	// Retry note: this callback goes through BAML's runtime, so any
	// client-level retry_policy declared statically in the BAML file will
	// apply on top of the orchestrator's outer retry.Execute loop. Per-
	// request __baml_options__.retry is NOT forwarded into BAML because
	// makeOptionsFromAdapter does not translate it — outer retries handle
	// per-request retries, so only the inner BAML static retry compounds.
	//
	// Raw + reasoning note: legacy raw comes from BAML's
	// FunctionLog.RawLLMResponse(), which is text-only across providers
	// (BAML's runtime drops Anthropic thinking_delta and equivalents).
	// Reasoning for the mixed-mode legacy child path is currently always
	// empty: the legacy helper reads BAML's authoritative raw view and
	// does not run its own SSE extractor over the response, so it has no
	// source of reasoning text. The inline (non-mixed) legacy code path
	// does surface reasoning via the shared extractor — see
	// codegen_stream_helpers.go's runFullOrchestration.
	LegacyStreamChild LegacyStreamChildFunc
}

// LegacyStreamChildFunc runs one child of a mixed-mode fallback chain via
// BAML's Stream API. See StreamConfig.LegacyStreamChild for the full
// contract.
type LegacyStreamChildFunc func(
	ctx context.Context,
	clientOverride string,
	provider string,
	needsRaw bool,
	sendHeartbeat func(),
) (finalResult any, raw, reasoning string, err error)

// BuildRequestFunc builds an HTTP request for streaming by calling
// StreamRequest.Method(ctx, args, opts...) and converting the result
// to an llmhttp.Request. The clientOverride parameter, when non-empty,
// forces the request to use a specific child client (for fallback chain
// iteration). This is provided by the generated adapter code.
type BuildRequestFunc func(ctx context.Context, clientOverride string) (*llmhttp.Request, error)

// ParseStreamFunc parses accumulated raw text into a typed partial.
// This wraps ParseStream.Method(accumulated, opts...) from the generated code.
type ParseStreamFunc func(ctx context.Context, accumulated string) (any, error)

// ParseFinalFunc parses complete raw text into the typed final result.
// This wraps Parse.Method(accumulated, opts...) from the generated code.
type ParseFinalFunc func(ctx context.Context, accumulated string) (any, error)

// NewResultFunc creates a new pooled StreamResult with the given fields.
// This is provided by the generated adapter code (the per-method pool getter).
//
// reasoning carries provider-specific reasoning text for the structured
// /with-raw reasoning channel. Populated only when IncludeReasoning is on;
// callers pass "" otherwise (raw stays text-only by construction).
type NewResultFunc func(kind bamlutils.StreamResultKind, stream, final any, raw, reasoning string, err error, reset bool) bamlutils.StreamResult

// NewMetadataResultFunc creates a new pooled StreamResult wrapping a metadata
// payload. Provided by the generated adapter code via the per-method metadata
// constructor. The returned StreamResult's Kind() must be
// StreamResultKindMetadata and its Metadata() must return the supplied value.
type NewMetadataResultFunc func(md *bamlutils.Metadata) bamlutils.StreamResult

// RunStreamOrchestration executes the BuildRequest streaming path.
//
// It builds an HTTP request, executes it, parses SSE events, extracts deltas,
// optionally calls ParseStream for typed partials, and emits StreamResult
// values on the output channel. Retries are handled per the config's policy.
//
// The output channel is NOT closed by this function — the caller (generated
// code) is responsible for closing it.
//
// This function blocks until streaming is complete or the context is cancelled.
func RunStreamOrchestration(
	ctx context.Context,
	out chan<- bamlutils.StreamResult,
	config *StreamConfig,
	httpClient *llmhttp.Client,
	buildRequest BuildRequestFunc,
	parseStream ParseStreamFunc,
	parseFinal ParseFinalFunc,
	newResult NewResultFunc,
) error {
	if httpClient == nil {
		httpClient = llmhttp.DefaultClient
	}

	var heartbeatSent atomic.Bool

	// sawStreamFrame tracks whether the current child/attempt window
	// actually queued a non-reset StreamResultKindStream frame to
	// `out`. Set inside tryOneStreamChild's trySendPartial only on the
	// successful-send branch — drop-on-buffer-full and ctx.Done paths
	// must NOT count, because the client never received those frames
	// and there is no partial state to discard.
	//
	// Reset markers (StreamResultKindStream with reset=true) emitted
	// at fallback-child handoff and retry callbacks are gated on this
	// flag: a reset marker before any actual partial frame produces a
	// false first-byte signal in the pool's hung detector
	// (pool/pool.go:1415-1427 treats any non-planned-metadata kind as
	// progress), letting a stream that hadn't yet produced upstream
	// bytes look alive. Gating on sawStreamFrame ensures the reset
	// only fires when there's genuine downstream state the next
	// window needs to clear.
	//
	// heartbeatSent's reset behavior is independent of this flag.
	// Resetting heartbeatSent across windows still correctly re-arms
	// per-attempt liveness via the next window's organic heartbeat;
	// only the reset *marker* is harmful.
	var sawStreamFrame atomic.Bool

	// plannedMetadataOnce gates the single planned-metadata emission. It is
	// strictly once-per-orchestrator-invocation: the sync.Once is never
	// reset within RunStreamOrchestration. This is deliberately stronger
	// than heartbeatSent's contract: heartbeatSent IS reset on fallback-
	// child handoff (~line 1955) and retry callbacks (~line 2008) so the
	// next child/retry can re-arm pool first-byte detection, but the
	// planned metadata describes the whole orchestrator run and must fire
	// exactly once regardless of how many children/retries that run
	// consumes.
	//
	// Pool-level retries produce a fresh orchestrator invocation with a
	// fresh Once, so each pool attempt gets its own planned emission
	// (pool rewrites the Attempt field).
	var plannedMetadataOnce sync.Once
	emitPlannedMetadata := func() {
		if config.MetadataPlan == nil || config.NewMetadataResult == nil {
			return
		}
		plannedMetadataOnce.Do(func() {
			plan := *config.MetadataPlan
			plan.Phase = bamlutils.MetadataPhasePlanned
			plan.Attempt = 0
			r := config.NewMetadataResult(&plan)
			select {
			case out <- r:
			default:
				r.Release()
			}
		})
	}

	// Emit planned metadata BEFORE validation so the routing decision
	// is observable on every path, including immediate validation
	// failures. Order: emit planned → validate → return (with metadata
	// already out) on error, or proceed on success. The
	// plannedMetadataOnce gate keeps the contract idempotent for
	// sendHeartbeat below.
	emitPlannedMetadata()

	// Validate the configured provider(s) up front so invalid fallback chains
	// fail before any stream/reset events are emitted.
	if len(config.FallbackChain) == 0 {
		if config.Provider == "" || !IsProviderSupported(config.Provider) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q", config.Provider)
		}
		if config.Provider == "aws-bedrock" && config.BuildBedrockStreamRequest == nil {
			return errors.New("buildrequest: aws-bedrock streaming requires StreamConfig.BuildBedrockStreamRequest to be set")
		}
	} else {
		if len(config.LegacyChildren) > 0 && config.LegacyStreamChild == nil {
			return fmt.Errorf("buildrequest: LegacyChildren set but LegacyStreamChild is nil")
		}
		// Track whether the chain contains an aws-bedrock leaf so we
		// can require BuildBedrockStreamRequest up front rather than
		// failing the first time a bedrock child runs (which would
		// have already burned earlier non-bedrock retries).
		bedrockInChain := false
		for _, child := range config.FallbackChain {
			if config.LegacyChildren[child] {
				// Legacy children are driven via BAML's Stream API, which
				// tolerates providers BuildRequest doesn't support; we
				// only require the legacy callback itself. Skipping the
				// IsProviderSupported check is safe because
				// ResolveFallbackChain rejects chains with any empty
				// provider (returns nil,nil,nil), so ClientProviders[child]
				// is guaranteed non-empty by the time we get here.
				continue
			}
			provider := config.ClientProviders[child]
			if provider == "" || !IsProviderSupported(provider) {
				return fmt.Errorf("buildrequest: unsupported or empty provider %q for child %q", provider, child)
			}
			if provider == "aws-bedrock" {
				bedrockInChain = true
			}
		}
		if bedrockInChain && config.BuildBedrockStreamRequest == nil {
			return errors.New("buildrequest: aws-bedrock child in fallback chain requires StreamConfig.BuildBedrockStreamRequest to be set")
		}
	}

	// sendHeartbeat fires when the upstream provider returns 2xx and
	// also nudges the planned-metadata emit (a no-op after the upfront
	// emit above — the plannedMetadataOnce gate guarantees idempotency).
	//
	// Heartbeat is gated on heartbeatSent — idempotent until that flag
	// is reset. The flag IS reset deliberately at two seams within a
	// single orchestrator invocation, allowing one heartbeat per
	// upstream attempt/child window rather than once per whole
	// orchestrator:
	//
	//   - fallback-child handoff after a failed prior child (see the
	//     reset around line 1955): the next child must be able to
	//     emit a fresh heartbeat so the pool's first-byte hung
	//     detection re-arms for that attempt.
	//   - retry callback before the next retry attempt (see the reset
	//     around line 2008): same liveness-rearm rationale, applied
	//     to retry windows on the BuildRequest path.
	//
	// Pool-level retries also create a fresh orchestrator invocation
	// (and a fresh heartbeatSent), so the pool sees one heartbeat per
	// pool-attempt × inner-attempt × child window rather than once per
	// outer pool attempt.
	sendHeartbeat := func() {
		if heartbeatSent.CompareAndSwap(false, true) {
			r := newResult(bamlutils.StreamResultKindHeartbeat, nil, nil, "", "", nil, false)
			select {
			case out <- r:
			default:
				r.Release()
			}
		}
		emitPlannedMetadata()
	}

	// trySendPartialShared is the common partial-emission helper for
	// both the SSE-driven and AWS-event-stream-driven streaming
	// paths. Centralising the send + sawStreamFrame book-keeping
	// keeps the two paths' delta semantics identical (drop on
	// buffer-full, mark sawStreamFrame only on successful send, ctx
	// cancellation surfaces ctx.Err).
	trySendPartialShared := func(parsed any, rawDelta, reasoningDelta string) error {
		r := newResult(bamlutils.StreamResultKindStream, parsed, nil, rawDelta, reasoningDelta, nil, false)
		select {
		case out <- r:
			sawStreamFrame.Store(true)
		case <-ctx.Done():
			r.Release()
			return ctx.Err()
		default:
			r.Release()
		}
		return nil
	}

	// tryOneBedrockStreamChild runs a single aws-bedrock streaming
	// attempt. Structurally parallel to tryOneStreamChild but uses:
	//   - config.BuildBedrockStreamRequest (Request.<Method> +
	//     /converse-stream URL rewrite + AWS event-stream Accept
	//     header) instead of buildRequest (StreamRequest.<Method>).
	//   - httpClient.ExecuteAWSStream (Content-Type:
	//     application/vnd.amazon.eventstream) instead of
	//     ExecuteStream.
	//   - extractBedrockStreamDelta on awsstream.Decoder.Next()
	//     events instead of sse.ExtractDeltaPartsFromText on SSE
	//     events.
	//
	// Errors flow up the same way: transport drops (*HTTPError,
	// transport-flake umbrella, *awsstream.TransportError) go to the
	// orchestrator's retry/fallback loop, while modeled exceptions
	// from a contentBlockDelta extractor surface as
	// *BedrockStreamException — both routed through the worker
	// classifier's typed-before-legacy ordering.
	//
	// PR3-bedrock-stream breadcrumb (issue #243).
	tryOneBedrockStreamChild := func(clientOverride string) (any, string, string, error) {
		if config.BuildBedrockStreamRequest == nil {
			return nil, "", "", errors.New("buildrequest: aws-bedrock streaming requires StreamConfig.BuildBedrockStreamRequest to be set")
		}
		req, err := config.BuildBedrockStreamRequest(ctx, clientOverride)
		if err != nil {
			return nil, "", "", fmt.Errorf("buildrequest: failed to build request: %w", err)
		}

		resp, err := httpClient.ExecuteAWSStream(ctx, req)
		if err != nil {
			return nil, "", "", err
		}
		defer resp.Close()

		// Send heartbeat on connection success — matches the SSE
		// path's "first byte from upstream = pool first-byte
		// detection re-arms" contract.
		sendHeartbeat()

		var parseableAccumulated strings.Builder
		var rawAccumulated strings.Builder
		var reasoningAccumulated strings.Builder
		var lastParseTime time.Time

		for {
			evt, nextErr := resp.Events.Next()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				// *awsstream.TransportError flows up verbatim: the
				// worker classifier has a dedicated arm that maps it
				// to provider_error with details.error_code /
				// error_message. Generic framing errors (CRC,
				// truncation, ErrUnexpectedEOF) wrap the same way
				// the SSE path's stream-error wrap does so retry
				// gating keeps treating them as transport-class
				// failures.
				return nil, "", "", fmt.Errorf("buildrequest: stream error: %w", nextErr)
			}

			delta, extractErr := extractBedrockStreamDelta(evt, config.IncludeReasoning)
			if extractErr != nil {
				// Modeled exception (*BedrockStreamException) or
				// genuinely malformed payload — both fail the
				// attempt so retry/fallback can take over. Same
				// failure-fast contract as the SSE path's
				// extraction error arm.
				return nil, "", "", fmt.Errorf("buildrequest: delta extraction failed: %w", extractErr)
			}

			// Skip frames with no incremental content on any
			// channel — messageStart, contentBlockStart/Stop,
			// messageStop, metadata, tool-use silent-skip
			// variants. Reasoning-only frames under
			// IncludeReasoning=false also land here (extractor
			// returns a zero delta).
			if delta.Raw == "" && delta.Parseable == "" && delta.Reasoning == "" {
				continue
			}

			rawAccumulated.WriteString(delta.Raw)
			if delta.Parseable != "" {
				parseableAccumulated.WriteString(delta.Parseable)
			}
			if delta.Reasoning != "" {
				reasoningAccumulated.WriteString(delta.Reasoning)
			}

			// Reasoning-only frame (delta.Parseable == ""): under
			// NeedsRaw=true, push the reasoning/raw delta out as a
			// partial. Matches the SSE path's "delta.Parseable ==
			// '' && config.NeedsRaw" arm.
			if config.NeedsPartials && delta.Parseable == "" {
				if config.NeedsRaw {
					if err := trySendPartialShared(nil, delta.Raw, delta.Reasoning); err != nil {
						return nil, "", "", err
					}
				}
				continue
			}

			if config.NeedsPartials && parseStream != nil {
				shouldParse := config.ParseThrottleInterval == 0 ||
					time.Since(lastParseTime) >= config.ParseThrottleInterval
				if shouldParse {
					lastParseTime = time.Now()
					parsed, parseErr := parseStream(ctx, parseableAccumulated.String())
					if parseErr == nil && parsed != nil {
						rawForResult := ""
						reasoningForResult := ""
						if config.NeedsRaw {
							rawForResult = delta.Raw
							reasoningForResult = delta.Reasoning
						}
						if err := trySendPartialShared(parsed, rawForResult, reasoningForResult); err != nil {
							return nil, "", "", err
						}
					}
				}
			}
		}

		fullRaw := rawAccumulated.String()
		fullReasoning := reasoningAccumulated.String()

		finalResult, parseErr := parseFinal(ctx, parseableAccumulated.String())
		if parseErr != nil {
			return nil, "", "", wrapOutputParse(fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr))
		}

		return finalResult, fullRaw, fullReasoning, nil
	}

	// tryOneStreamChild runs a single child's streaming attempt against the
	// BuildRequest path. Returns the parsed final, the accumulated raw
	// (text-only) text, and the accumulated reasoning text; the caller
	// (attemptFull) is responsible for emitting the final result on the
	// output channel so supported and legacy children share a single
	// emission path.
	tryOneStreamChild := func(provider, clientOverride string) (any, string, string, error) {
		// aws-bedrock streaming uses a separate transport (AWS
		// event-stream) and request builder (Request.<Method>, not
		// StreamRequest — see BuildBedrockStreamRequest doc).
		// Dispatch to the dedicated bedrock closure. PR3-bedrock-
		// stream breadcrumb (issue #243).
		if provider == "aws-bedrock" {
			return tryOneBedrockStreamChild(clientOverride)
		}
		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, "", "", fmt.Errorf("buildrequest: failed to build request: %w", err)
		}

		resp, err := httpClient.ExecuteStream(ctx, req)
		if err != nil {
			return nil, "", "", err
		}
		defer resp.Close()

		// Send heartbeat on connection success
		sendHeartbeat()

		var parseableAccumulated strings.Builder
		var rawAccumulated strings.Builder
		var reasoningAccumulated strings.Builder
		var lastParseTime time.Time

		// Share the partial-emission semantics with
		// tryOneBedrockStreamChild — same drop-on-buffer-full /
		// ctx-cancellation / sawStreamFrame contract.
		trySendPartial := trySendPartialShared

		for ev := range resp.Events {
			// Extract parseable/raw/reasoning delta content from the SSE
			// event using this attempt's provider. Reasoning text never
			// enters Parseable or Raw; under IncludeReasoning=true it
			// populates DeltaParts.Reasoning instead.
			delta, extractErr := sse.ExtractDeltaPartsFromText(provider, ev.Data, config.IncludeReasoning)
			if extractErr != nil {
				// Extraction error — fail the attempt so retry logic can handle it
				// rather than silently accumulating incomplete text.
				return nil, "", "", fmt.Errorf("buildrequest: delta extraction failed: %w", extractErr)
			}
			// Skip when nothing meaningful arrived on any channel. Under
			// IncludeReasoning=true a reasoning-only event has empty Raw
			// but non-empty Reasoning — so the gate must consider
			// Reasoning too, otherwise reasoning-only frames would be
			// dropped without ever reaching the wire.
			if delta.Raw == "" && delta.Parseable == "" && delta.Reasoning == "" {
				continue
			}

			rawAccumulated.WriteString(delta.Raw)
			if delta.Parseable != "" {
				parseableAccumulated.WriteString(delta.Parseable)
			}
			if delta.Reasoning != "" {
				reasoningAccumulated.WriteString(delta.Reasoning)
			}

			if config.NeedsPartials && delta.Parseable == "" {
				if config.NeedsRaw {
					if err := trySendPartial(nil, delta.Raw, delta.Reasoning); err != nil {
						return nil, "", "", err
					}
				}
				continue
			}

			// Emit partial if needed.
			// Non-blocking sends for partials/deltas: drop when the output
			// buffer is full so the LLM stream keeps draining. This matches
			// the legacy path behavior which intentionally drops non-reset
			// partials rather than coupling upstream HTTP reads to downstream
			// consumer backpressure.
			if config.NeedsPartials && parseStream != nil {
				shouldParse := config.ParseThrottleInterval == 0 ||
					time.Since(lastParseTime) >= config.ParseThrottleInterval

				if shouldParse {
					// Update throttle timestamp regardless of parse success/failure
					// so that repeated failures don't bypass the throttle interval.
					lastParseTime = time.Now()

					parsed, parseErr := parseStream(ctx, parseableAccumulated.String())
					if parseErr == nil && parsed != nil {
						rawForResult := ""
						reasoningForResult := ""
						if config.NeedsRaw {
							rawForResult = delta.Raw
							reasoningForResult = delta.Reasoning
						}
						if err := trySendPartial(parsed, rawForResult, reasoningForResult); err != nil {
							return nil, "", "", err
						}
					}
				}
			} else if config.NeedsRaw {
				// Raw-only mode: accumulate silently; full raw + reasoning
				// text is included in the final result via rawForFinal /
				// reasoningForFinal.
			}
		}

		// Check for stream errors
		if streamErr := <-resp.Errc; streamErr != nil {
			return nil, "", "", fmt.Errorf("buildrequest: stream error: %w", streamErr)
		}

		// Parse the final result — let parseFinal decide whether an empty
		// completion is valid. The legacy path does not reject empty strings.
		fullRaw := rawAccumulated.String()
		fullReasoning := reasoningAccumulated.String()

		finalResult, parseErr := parseFinal(ctx, parseableAccumulated.String())
		if parseErr != nil {
			return nil, "", "", wrapOutputParse(fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr))
		}

		return finalResult, fullRaw, fullReasoning, nil
	}

	// Track the winning attempt for outcome metadata. Populated by
	// attemptFull right before emitFinal runs, so the outcome event
	// always reflects the child that actually produced the final result.
	var (
		winnerClient    string
		winnerProvider  string
		winnerPath      string
		finalRetryCount int
		startTime       = time.Now()
	)

	// emitOutcomeMetadata sends the outcome metadata event just before the
	// final result. Scheduled from attemptFull once a winning child has
	// been identified. Safe to call with a nil MetadataPlan (no-op).
	emitOutcomeMetadata := func() {
		if config.MetadataPlan == nil || config.NewMetadataResult == nil {
			return
		}
		outcome := *config.MetadataPlan
		outcome.Phase = bamlutils.MetadataPhaseOutcome
		outcome.Attempt = 0
		// Clear planned-only noise from the outcome payload. Clients key
		// behaviour off Phase, but keeping the event compact avoids a large
		// duplicated payload on every successful request.
		outcome.RetryMax = nil
		outcome.RetryPolicy = ""
		outcome.Chain = nil
		outcome.LegacyChildren = nil
		// FallbackTargets / FallbackRoundRobin describe PLANNED
		// fallback-chain intent — which RR-wrapper children were
		// centrally unwrapped to leaves and which leaves the RR
		// decision picked. The realised winner is encoded in
		// WinnerClient on outcome, so retaining these planned-only
		// fields duplicates information and inflates the outcome
		// payload. Clear them alongside Chain / LegacyChildren for
		// the same reason.
		outcome.FallbackTargets = nil
		outcome.FallbackRoundRobin = nil
		outcome.Strategy = ""
		outcome.Provider = ""

		retryCount := finalRetryCount
		outcome.RetryCount = &retryCount
		outcome.WinnerClient = winnerClient
		outcome.WinnerProvider = winnerProvider
		outcome.WinnerPath = winnerPath
		dur := time.Since(startTime).Milliseconds()
		outcome.UpstreamDurMs = &dur

		r := config.NewMetadataResult(&outcome)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}
	}

	// emitFinal sends the single StreamResultKindFinal for the winning
	// attempt, respecting context cancellation. Centralising the emission
	// here guarantees exactly one final event regardless of whether the
	// winning child was BuildRequest-driven or routed through the legacy
	// helper.
	emitFinal := func(finalResult any, raw, reasoning string) error {
		emitOutcomeMetadata()
		rawForFinal := ""
		reasoningForFinal := ""
		if config.NeedsRaw {
			rawForFinal = raw
			reasoningForFinal = reasoning
		}
		r := newResult(bamlutils.StreamResultKindFinal, nil, finalResult, rawForFinal, reasoningForFinal, nil, false)
		select {
		case out <- r:
			return nil
		case <-ctx.Done():
			r.Release()
			return ctx.Err()
		}
	}

	// attemptFull tries the single provider or the entire fallback chain.
	// For fallback chains, each retry walks all children in order —
	// matching the BAML runtime where retries retry the entire strategy.
	attemptFull := func(attempt int) (any, error) {
		if len(config.FallbackChain) == 0 {
			finalResult, raw, reasoning, err := tryOneStreamChild(config.Provider, config.ClientOverride)
			if err != nil {
				return nil, err
			}
			winnerClient = ""
			if config.MetadataPlan != nil {
				winnerClient = config.MetadataPlan.Client
			}
			winnerProvider = config.Provider
			winnerPath = "buildrequest"
			finalRetryCount = attempt
			if emitErr := emitFinal(finalResult, raw, reasoning); emitErr != nil {
				return nil, emitErr
			}
			return finalResult, nil
		}
		var lastErr error
		for i, child := range config.FallbackChain {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			// Emit a reset signal before trying the next child so the
			// downstream discards any partial/raw state leaked by the
			// previous child's failed stream. Not needed before the first
			// child in the chain. Gated on TWO conditions:
			//
			//   - config.NeedsPartials: for NeedsPartials=false (call
			//     modes bridged through this orchestrator) there is no
			//     partial state downstream, and the reset would falsely
			//     flip the pool's gotFirstByte flag, disabling hung
			//     detection for the next child.
			//   - sawStreamFrame: the previous child window must have
			//     actually queued a non-reset stream frame for there to
			//     be partial state worth clearing. Without this gate, a
			//     child that fails before producing any bytes would
			//     still emit a reset, and the pool would treat that
			//     reset (a StreamResultKindStream) as first-byte
			//     progress for the next child — papering over a genuine
			//     no-byte hang.
			//
			// The heartbeat reset is independent of the reset marker:
			// it still runs on every handoff so the next child re-arms
			// first-byte detection via its own organic heartbeat.
			if i > 0 && lastErr != nil {
				if config.NeedsPartials && sawStreamFrame.Load() {
					r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", "", nil, true)
					select {
					case out <- r:
					case <-ctx.Done():
						r.Release()
						return nil, ctx.Err()
					}
				}
				// Clear sawStreamFrame for the next child's window
				// regardless of whether we emitted a reset — the next
				// child starts fresh and any frames it queues (or
				// fails to queue) are what the next handoff/retry
				// boundary will see.
				sawStreamFrame.Store(false)
				heartbeatSent.Store(false)
			}
			var (
				finalResult  any
				raw          string
				reasoning    string
				err          error
				path         string
				winnerTarget = child
			)
			if config.LegacyChildren[child] {
				// Legacy children run via BAML's Stream API. The callback
				// owns heartbeat timing (fires sendHeartbeat on the first
				// FunctionLog tick) so pool hung-detection stays correct.
				// Partials are never emitted by the callback — it reports
				// only (final, raw, reasoning, err). Note: callback
				// dispatch stays rooted at `child` (the chain-position
				// name) so the scoped registry preserves runtime strategy
				// overrides for true legacy children — FallbackTargets is
				// honored for the BuildRequest path only.
				finalResult, raw, reasoning, err = config.LegacyStreamChild(
					ctx,
					child,
					config.ClientProviders[child],
					config.NeedsRaw,
					sendHeartbeat,
				)
				path = "legacy"
			} else {
				provider := config.ClientProviders[child]
				// Wrapper-vs-target identity: when an immediate RR
				// fallback child was centrally unwrapped to a leaf,
				// FallbackTargets[child] names that leaf. Dispatch
				// BuildRequest against the leaf so BAML's runtime sees
				// the resolved client identity directly, not the RR
				// wrapper (which would re-rotate per-worker inside
				// BAML and defeat the centralization). When the map
				// has no entry or the entry equals child, dispatch
				// passes the chain-position name verbatim — the
				// pass-through shape for chains with no centralization.
				if t, ok := config.FallbackTargets[child]; ok && t != "" {
					winnerTarget = t
				}
				finalResult, raw, reasoning, err = tryOneStreamChild(provider, winnerTarget)
				path = "buildrequest"
			}
			if err == nil {
				// WinnerClient names the client actually contacted:
				// for legacy children, the wrapper (chain-position
				// name); for BR children, the dispatch target (the
				// centralized leaf when FallbackTargets[child] is set,
				// otherwise the chain-position name). WinnerProvider
				// stays keyed by the chain position so it reflects
				// the resolver's classification (leaf provider for
				// centralized RR children, wrapper provider otherwise).
				winnerClient = winnerTarget
				winnerProvider = config.ClientProviders[child]
				winnerPath = path
				finalRetryCount = attempt
				if emitErr := emitFinal(finalResult, raw, reasoning); emitErr != nil {
					return nil, emitErr
				}
				return finalResult, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	// Execute with retries
	_, err := retry.Execute(ctx, config.RetryPolicy, attemptFull, func(attempt int) {
		// Emit reset signal so downstream discards accumulated partial
		// state. Same TWO-condition gate as the fallback handoff
		// above:
		//
		//   - config.NeedsPartials: NeedsPartials=false has no
		//     partial state downstream and the reset would flip the
		//     pool's gotFirstByte flag before the next attempt's
		//     upstream produces a byte.
		//   - sawStreamFrame: the just-failed attempt's last child
		//     window must have actually queued a frame to the
		//     client; otherwise the reset is a phantom that the pool
		//     would mistake for first-byte progress.
		if config.NeedsPartials && sawStreamFrame.Load() {
			r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", "", nil, true)
			select {
			case out <- r:
			case <-ctx.Done():
				r.Release()
			}
		}

		// Clear sawStreamFrame for the next attempt's window. Same
		// rationale as the fallback handoff: the next attempt starts
		// fresh, and the subsequent handoff/retry boundary should
		// see only what THAT attempt managed to queue.
		sawStreamFrame.Store(false)

		// Reset heartbeat for new attempt so first-byte detection re-arms
		// via the next attempt's heartbeat.
		heartbeatSent.Store(false)
	})

	if err != nil && ctx.Err() == nil {
		// Emit error result (skip if context was cancelled — cancellation is not an error)
		r := newResult(bamlutils.StreamResultKindError, nil, nil, "", "", err, false)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}
	}

	return nil // errors are communicated via the channel, not the return value
}
