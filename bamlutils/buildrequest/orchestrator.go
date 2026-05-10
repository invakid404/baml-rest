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
	"fmt"
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

// supportedProviders is the set of providers whose SSE format is handled by
// ExtractDeltaFromText. This must match the switch cases in that function
// exactly — any provider not in this set falls back to the legacy path.
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
	// "aws-bedrock" is intentionally absent: BAML's StreamRequest errors for it,
	// and its SSE format requires special handling not suited to BuildRequest.
}

// IsProviderSupported returns true if the provider's SSE format is handled
// by ExtractDeltaFromText and the provider supports BAML StreamRequest.
// Unknown providers fall back to the legacy CallStream+OnTick path.
func IsProviderSupported(provider string) bool {
	return supportedProviders[provider]
}

// callSupportedProviders is the set of providers whose non-streaming JSON
// response format is handled by ExtractResponseContent. This is separate
// from supportedProviders because the streaming path requires SSE format
// compatibility while the non-streaming path requires JSON response format
// compatibility — different constraints that may evolve independently.
//
// "aws-bedrock" is excluded pending verification that BAML's Request API
// supports Bedrock (see design doc Section 5.3).
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

	// IncludeThinkingInRaw is the per-request opt-in for surfacing
	// provider-specific reasoning/thinking content in raw. When true,
	// the streaming extractor accumulates Anthropic thinking_delta
	// events into raw alongside text deltas. When false (default),
	// raw matches BAML's RawLLMResponse() text-only semantics. The
	// flag never affects the parseable text passed to ParseStream.
	IncludeThinkingInRaw bool

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
	// doc string for the wire-shape contract. PR 1 adds the field; PR 2
	// (issue #237) wires the BuildRequest dispatch loop to honour
	// FallbackTargets[child] when computing the WithClient target for
	// an immediate RR fallback child that was centrally unwrapped to a
	// leaf. The orchestrator is the consumer; resolver/planning
	// upstream is the producer.
	FallbackTargets map[string]string

	// FallbackRoundRobin mirrors Metadata.FallbackRoundRobin — the
	// per-child RR decision for fallback children resolved through
	// BuildRequest centralization. PR 1 adds the field; PR 2 wires
	// resolver output to populate it so outgoing planned metadata
	// describes both the selected leaf (FallbackTargets[child]) and
	// the RR decision behind that selection.
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
	// Raw note: legacy raw comes from BAML's FunctionLog.RawLLMResponse(),
	// which is text-only across providers (BAML's runtime drops Anthropic-
	// thinking_delta and equivalents). Under the default
	// IncludeThinkingInRaw=false this exactly matches the BuildRequest
	// path's behavior. Under IncludeThinkingInRaw=true, this niche path's
	// raw stays text-only — the flag has no effect here because BAML's
	// runtime never surfaces thinking content for this codepath to
	// accumulate. Documented as a known limitation in the
	// include-thinking-in-raw tracking issue; revisit only if reported in
	// a real workflow.
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
) (finalResult any, raw string, err error)

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
type NewResultFunc func(kind bamlutils.StreamResultKind, stream, final any, raw string, err error, reset bool) bamlutils.StreamResult

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
	} else {
		if len(config.LegacyChildren) > 0 && config.LegacyStreamChild == nil {
			return fmt.Errorf("buildrequest: LegacyChildren set but LegacyStreamChild is nil")
		}
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
			r := newResult(bamlutils.StreamResultKindHeartbeat, nil, nil, "", nil, false)
			select {
			case out <- r:
			default:
				r.Release()
			}
		}
		emitPlannedMetadata()
	}

	// tryOneStreamChild runs a single child's streaming attempt against the
	// BuildRequest path. Returns the parsed final plus the accumulated raw
	// text; the caller (attemptFull) is responsible for emitting the final
	// result on the output channel so supported and legacy children share a
	// single emission path.
	tryOneStreamChild := func(provider, clientOverride string) (any, string, error) {
		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, "", fmt.Errorf("buildrequest: failed to build request: %w", err)
		}

		resp, err := httpClient.ExecuteStream(ctx, req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Close()

		// Send heartbeat on connection success
		sendHeartbeat()

		var parseableAccumulated strings.Builder
		var rawAccumulated strings.Builder
		var lastParseTime time.Time

		trySendPartial := func(parsed any, rawDelta string) error {
			r := newResult(bamlutils.StreamResultKindStream, parsed, nil, rawDelta, nil, false)
			select {
			case out <- r:
				// Mark only on successful send: ctx.Done and the
				// drop-on-buffer-full branches did not actually
				// deliver a frame to the client, so the reset-marker
				// gate must stay armed.
				sawStreamFrame.Store(true)
			case <-ctx.Done():
				r.Release()
				return ctx.Err()
			default:
				r.Release() // Drop partial/raw delta — buffer full
			}
			return nil
		}

		for ev := range resp.Events {
			// Extract parseable/raw delta content from the SSE event using this
			// attempt's provider. When IncludeThinkingInRaw is true, Anthropic
			// thinking_delta events contribute to Raw only; Parseable always
			// excludes thinking so the BAML parser cannot be influenced by
			// reasoning text regardless of the flag.
			delta, extractErr := sse.ExtractDeltaPartsFromText(provider, ev.Data, config.IncludeThinkingInRaw)
			if extractErr != nil {
				// Extraction error — fail the attempt so retry logic can handle it
				// rather than silently accumulating incomplete text.
				return nil, "", fmt.Errorf("buildrequest: delta extraction failed: %w", extractErr)
			}
			if delta.Raw == "" {
				continue
			}

			rawAccumulated.WriteString(delta.Raw)
			if delta.Parseable != "" {
				parseableAccumulated.WriteString(delta.Parseable)
			}

			if config.NeedsPartials && delta.Parseable == "" {
				if config.NeedsRaw {
					if err := trySendPartial(nil, delta.Raw); err != nil {
						return nil, "", err
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
						if config.NeedsRaw {
							rawForResult = delta.Raw
						}
						if err := trySendPartial(parsed, rawForResult); err != nil {
							return nil, "", err
						}
					}
				}
			} else if config.NeedsRaw {
				// Raw-only mode: accumulate silently; full raw text is
				// included in the final result via rawForFinal.
			}
		}

		// Check for stream errors
		if streamErr := <-resp.Errc; streamErr != nil {
			return nil, "", fmt.Errorf("buildrequest: stream error: %w", streamErr)
		}

		// Parse the final result — let parseFinal decide whether an empty
		// completion is valid. The legacy path does not reject empty strings.
		fullRaw := rawAccumulated.String()

		finalResult, parseErr := parseFinal(ctx, parseableAccumulated.String())
		if parseErr != nil {
			return nil, "", fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)
		}

		return finalResult, fullRaw, nil
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
		// decision picked (issue #237). The realised winner is
		// encoded in WinnerClient on outcome, so retaining these
		// planned-only fields duplicates information and inflates
		// the outcome payload. Clear them alongside Chain /
		// LegacyChildren for the same reason.
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
	emitFinal := func(finalResult any, raw string) error {
		emitOutcomeMetadata()
		rawForFinal := ""
		if config.NeedsRaw {
			rawForFinal = raw
		}
		r := newResult(bamlutils.StreamResultKindFinal, nil, finalResult, rawForFinal, nil, false)
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
			finalResult, raw, err := tryOneStreamChild(config.Provider, config.ClientOverride)
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
			if emitErr := emitFinal(finalResult, raw); emitErr != nil {
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
					r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
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
				finalResult any
				raw         string
				err         error
				path        string
			)
			if config.LegacyChildren[child] {
				// Legacy children run via BAML's Stream API. The callback
				// owns heartbeat timing (fires sendHeartbeat on the first
				// FunctionLog tick) so pool hung-detection stays correct.
				// Partials are never emitted by the callback — it reports
				// only (final, raw, err).
				finalResult, raw, err = config.LegacyStreamChild(
					ctx,
					child,
					config.ClientProviders[child],
					config.NeedsRaw,
					sendHeartbeat,
				)
				path = "legacy"
			} else {
				provider := config.ClientProviders[child]
				finalResult, raw, err = tryOneStreamChild(provider, child)
				path = "buildrequest"
			}
			if err == nil {
				winnerClient = child
				winnerProvider = config.ClientProviders[child]
				winnerPath = path
				finalRetryCount = attempt
				if emitErr := emitFinal(finalResult, raw); emitErr != nil {
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
			r := newResult(bamlutils.StreamResultKindStream, nil, nil, "", nil, true)
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
		r := newResult(bamlutils.StreamResultKindError, nil, nil, "", err, false)
		select {
		case out <- r:
		case <-ctx.Done():
			r.Release()
		}
	}

	return nil // errors are communicated via the channel, not the return value
}
