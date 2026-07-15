package buildrequest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// CallConfig holds configuration for a non-streaming call request.
// This is simpler than StreamConfig because non-streaming calls don't need
// partial result handling or parse throttling.
type CallConfig struct {
	// Provider is the LLM provider name (e.g., "openai", "anthropic").
	// Used for response content extraction. For single-provider clients,
	// this is the only provider used. For fallback chains, this is
	// ignored in favor of per-attempt provider resolution.
	Provider string

	// RetryPolicy is the retry policy for this request. Nil means no retries.
	RetryPolicy *retry.Policy

	// NeedsRaw is true if the caller wants raw LLM response text
	// (for /call-with-raw endpoint).
	NeedsRaw bool

	// IncludeReasoning is the per-request opt-in for surfacing provider-
	// specific reasoning/thinking text on the structured reasoning
	// channel of /with-raw endpoints, distinct from raw. When true, the
	// response extractor populates the reasoning channel with Anthropic
	// thinking blocks / Gemini thought parts / OpenAI Chat Completions
	// reasoning_content / OpenAI Responses summary entries. When false
	// (default), the reasoning channel stays empty. The flag never
	// affects raw (always text-only) or the parseable text passed to
	// Parse.
	IncludeReasoning bool

	// FallbackChain is the ordered list of child client names for fallback
	// strategies. When non-empty, each retry attempt walks the entire chain
	// in order; if any child succeeds, the attempt returns immediately.
	// When empty, the single Provider is used for all attempts.
	FallbackChain []string

	// ClientOverride mirrors StreamConfig.ClientOverride — the
	// clientOverride value forwarded to buildRequest on single-provider
	// attempts, used by the round-robin path to target the RR-selected
	// leaf instead of the function's default client.
	ClientOverride string

	// ClientProviders maps child client names to their provider strings.
	// Used with FallbackChain to resolve the provider for each attempt.
	// In mixed-mode chains this map includes both BuildRequest-supported
	// children and legacy (unsupported-provider) children — it is a pure
	// lookup table, not a "supported set." Callers must consult
	// LegacyChildren to decide which path runs a given child.
	ClientProviders map[string]string

	// LegacyChildren marks the entries in FallbackChain that must be driven
	// via the legacy BAML Stream API rather than the BuildRequest path.
	// Mirrors StreamConfig.LegacyChildren — see that field for details.
	LegacyChildren map[string]bool

	// FallbackTargets mirrors StreamConfig.FallbackTargets — see that
	// field (and Metadata.FallbackTargets) for the contract. The call-
	// side BuildRequest dispatch loop honours FallbackTargets[child]
	// when computing the WithClient target for centrally-unwrapped RR
	// fallback children.
	FallbackTargets map[string]string

	// FallbackRoundRobin mirrors StreamConfig.FallbackRoundRobin —
	// per-child RR decisions for fallback children resolved through
	// BuildRequest centralization. Populated from resolver output so
	// outgoing planned metadata describes the selected leaf alongside
	// the RR decision behind it.
	FallbackRoundRobin map[string]*bamlutils.RoundRobinInfo

	// MetadataPlan is the pre-computed planned metadata for this request.
	// See StreamConfig.MetadataPlan for the contract; the call orchestrator
	// behaves identically — planned metadata is emitted upfront from the
	// orchestrator before any HTTP work, then heartbeat fires on upstream
	// 2xx, and outcome metadata is emitted on success right before final.
	MetadataPlan *bamlutils.Metadata

	// NewMetadataResult constructs a pooled StreamResult wrapping a metadata
	// payload. Required when MetadataPlan is non-nil.
	NewMetadataResult NewMetadataResultFunc

	// LegacyCallChild runs a single child via BAML's Stream API for the
	// non-streaming call path. The orchestrator invokes it for children
	// marked in LegacyChildren. Contract mirrors
	// StreamConfig.LegacyStreamChild: callback fires sendHeartbeat on the
	// first FunctionLog tick (matching the BuildRequest call path's
	// post-HTTP liveness semantics) and returns (final, raw, err).
	//
	// Even though this is the non-streaming path, the BAML runtime exposes
	// streaming as the primitive, so the legacy helper goes through
	// Stream.Method + FunctionLog capture identically to the streaming
	// callback — raw comes from FunctionLog.RawLLMResponse().
	LegacyCallChild LegacyCallChildFunc

	// --- Native child-attempt seam (de-BAML cutover Slice 1) ---
	//
	// These three fields are the NEUTRAL, HARD-OFF plumbing for injecting a
	// native (non-BAML) child-attempt implementation into tryOneChild. They
	// are zero (nil / false) in every production constructor, so the seam is
	// off and every request path is byte-identical to today. A LATER slice
	// installs the real nanollm-backed callback via the worker. See
	// native_callback.go for the full tri-state ownership contract.

	// NativeAttempt is the optional native child-attempt callback. When both
	// this is non-nil AND NativeAttemptEnabled is true, the orchestrator
	// invokes it as the FIRST operation for each selected non-legacy child,
	// before any BAML build/send. Legacy children never reach it. Nil in
	// production (hard off).
	NativeAttempt NativeCallAttemptFunc

	// NativeAttemptEnabled is the neutral "enabled" predicate gating the
	// native seam — the resolved umbrella-flag decision, passed in by the
	// caller. The orchestrator itself resolves no flag; it only honours this
	// boolean alongside a non-nil NativeAttempt. False in production.
	NativeAttemptEnabled bool

	// NativeOutputSchema is an opaque, engine-specific handle to the method's
	// output schema, forwarded verbatim into NativeCallAttempt.OutputSchema.
	// The generic orchestrator never inspects it. Nil in production.
	NativeOutputSchema any
}

// LegacyCallChildFunc runs one child of a mixed-mode fallback chain via
// BAML's Stream API for the non-streaming call path. See
// CallConfig.LegacyCallChild for the full contract.
type LegacyCallChildFunc func(
	ctx context.Context,
	clientOverride string,
	provider string,
	needsRaw bool,
	sendHeartbeat func(),
) (finalResult any, raw, reasoning string, err error)

// BuildCallRequestFunc builds an HTTP request for a non-streaming call by
// calling Request.Method(ctx, args, opts...) and converting the result
// to an llmhttp.Request. The clientOverride parameter, when non-empty,
// forces the request to use a specific child client (for fallback chain
// iteration). This is provided by the generated adapter code.
type BuildCallRequestFunc func(ctx context.Context, clientOverride string) (*llmhttp.Request, error)

// ExtractResponseFunc extracts the LLM output text from a non-streaming
// JSON response body. Returns three strings: parseable (for Parse.Method),
// raw (for /call-with-raw's Raw()), and reasoning (for /call-with-raw's
// Reasoning()).
//
// Parseable and raw are always text-only — reasoning/thinking content is
// never accumulated into either, so neither the BAML parser nor the wire's
// raw channel can be influenced by reasoning text. When includeReasoning
// is true the function populates the reasoning return with provider-
// specific reasoning text (e.g. Anthropic thinking blocks); otherwise
// reasoning is empty.
type ExtractResponseFunc func(provider string, responseBody string, includeReasoning bool) (parseable, raw, reasoning string, err error)

// ExtractResponseBytesFunc is the optional byte-oriented twin of
// ExtractResponseFunc. When supplied it is used in place of
// ExtractResponseFunc on transports that hand back caller-owned response
// bytes (the net/http unary lane, where llmhttp.Response.BodyBytes() is
// non-nil), letting the extractor parse the body without forcing a
// whole-body []byte->string copy (e.g. via gjson.GetBytes).
//
// It is an OPTIONAL seam: when nil, RunCallOrchestration falls back to the
// injected ExtractResponseFunc over the body on every lane, so a custom
// string-only extractor is always honored — the byte path only changes
// allocations, never which extractor runs. It must return results identical
// to the injected ExtractResponseFunc for the same JSON.
//
// RunCallOrchestration takes two values of this type: extractResponseBytes for
// the net/http OWNED-bytes lane (typically ExtractResponseContentBytes, which
// copies via gjson.GetBytes) and extractResponseBorrowed for the fasthttp
// BORROWED lane (typically ExtractResponseContentBorrowed, which aliases
// unescaped scalar/single-block content straight out of the borrowed buffer).
// Both are optional and both must agree with the string extractor's output;
// they differ only in allocation behavior and which transport lane they serve.
//
// Body lifetime — differs by lane and matters most for CUSTOM implementations:
//   - As extractResponseBytes, body is caller-owned (the net/http io.ReadAll
//     buffer) and stays valid for the life of the response.
//   - As extractResponseBorrowed, body ALIASES pooled transport storage that
//     the orchestrator recycles via resp.Release the instant the attempt is
//     done. An implementation MUST treat body as READ-ONLY and MUST NOT retain
//     body — or any string/[]byte that aliases it — after it returns. Anything
//     that must outlive the call has to be copied (the default extractors alias
//     for zero copy and rely on the orchestrator to clone every escaping value
//     before Release; a custom extractor that returns owned strings is always
//     safe).
type ExtractResponseBytesFunc func(provider string, body []byte, includeReasoning bool) (parseable, raw, reasoning string, err error)

// RunCallOrchestration executes the non-streaming BuildRequest path.
//
// Flow — single retry loop covering HTTP, extraction, and parsing:
//  1. buildRequest(ctx, clientOverride) → llmhttp.Request
//  2. httpClient.ExecuteBorrowed(ctx, req, onSuccess) → llmhttp.Response
//     (the orchestrator owns the borrow lifecycle and releases per-attempt;
//     callers never see a release obligation)
//  3. extract(provider, body, includeReasoning) → parseable + raw + reasoning
//     text — extractResponseBorrowed over resp.BorrowedBytes() on the fasthttp
//     borrow lane (aliasing, zero-copy on scalar/single-block content), else
//     extractResponseBytes over resp.BodyBytes() on the net/http owned-bytes
//     lane, else extractResponse over resp.BodyString()
//  4. parseFinal(ctx, parseable) → typed result
//  5. emit StreamResultKindFinal on channel
//
// All four steps run inside the retry loop so that extraction/parse failures
// in a fallback chain advance to the next child. This matches the streaming
// orchestrator, where parseFinal failures are retried from inside the attempt
// function.
//
// The request is rebuilt on each attempt because BuildRequest fallback routing
// may select a different child client/provider per retry. The generated
// adapter passes the chosen child via clientOverride, and Request may return a
// different HTTP request on each invocation.
//
// Top-level baml-roundrobin is resolved upstream by ResolveEffectiveClient
// (backed by SharedStateStore for cross-worker centralisation on modern
// adapters with SupportsWithClient); this orchestrator then dispatches to
// the resolved leaf like any other single-provider client. Older adapters
// without SupportsWithClient, and baml-roundrobin nested inside a fallback
// chain, still fall through to BAML's per-worker runtime rotation.
//
// The output channel is NOT closed by this function — the caller (generated
// code) is responsible for closing it.
//
// This function blocks until the call is complete or the context is cancelled.
func RunCallOrchestration(
	ctx context.Context,
	out chan<- bamlutils.StreamResult,
	config *CallConfig,
	httpClient *llmhttp.Client,
	buildRequest BuildCallRequestFunc,
	parseFinal ParseFinalFunc,
	extractResponse ExtractResponseFunc,
	extractResponseBytes ExtractResponseBytesFunc,
	extractResponseBorrowed ExtractResponseBytesFunc,
	newResult NewResultFunc,
) error {
	if httpClient == nil {
		httpClient = llmhttp.DefaultClient
	}

	// sendHeartbeat emits a heartbeat to the pool's hung detector via
	// Execute()'s onSuccess callback. It fires after the provider returns
	// a 2xx status but before the body is read, so the pool sees liveness
	// without waiting for the full body to buffer.
	//
	// Heartbeat sends are best-effort: if the output channel is full the
	// heartbeat is dropped (via the default branch) and the StreamResult
	// is released back to the pool. This is safe because a full channel
	// means the consumer already has buffered results and won't consider
	// the worker hung.
	var heartbeatSent atomic.Bool

	// plannedMetadataOnce gates the single planned-metadata emission
	// (see RunStreamOrchestration for the rationale — heartbeatSent resets
	// per inner retry, planned metadata must not).
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
			// Block on the send so planned metadata is never silently
			// dropped when the consumer is momentarily not ready —
			// callers (cmd/serve and pool) and tests assert the planned
			// frame is observable on every path. Only abandon the send
			// when the orchestrator's context is cancelled, in which case
			// the StreamResult must be released back to its pool.
			// Heartbeat keeps non-blocking semantics elsewhere because
			// a full channel implies the consumer already has buffered
			// liveness; planned metadata is the routing-decision frame
			// and has no analogue.
			select {
			case out <- r:
			case <-ctx.Done():
				r.Release()
			}
		})
	}

	// Emit planned metadata BEFORE validation so the routing decision
	// is observable on every path, including immediate validation
	// failures. See RunStreamOrchestration for the parallel
	// arrangement.
	emitPlannedMetadata()

	// Validate the configured provider(s) up front so invalid fallback
	// chains fail before any retry attempts are launched. The validation
	// walks every child — previously only the first child was checked,
	// which allowed a mixed-shaped chain with a valid first but broken
	// later child to start retrying before surfacing the error.
	if len(config.FallbackChain) == 0 {
		if config.Provider == "" || !IsCallProviderSupported(config.Provider) {
			return fmt.Errorf("buildrequest: unsupported or empty provider %q for non-streaming call", config.Provider)
		}
	} else {
		if len(config.LegacyChildren) > 0 && config.LegacyCallChild == nil {
			return fmt.Errorf("buildrequest: LegacyChildren set but LegacyCallChild is nil")
		}
		for _, child := range config.FallbackChain {
			if config.LegacyChildren[child] {
				// Legacy children are driven via BAML's Stream API, which
				// tolerates providers BuildRequest doesn't support; we
				// only require the legacy callback itself. Skipping the
				// IsCallProviderSupported check is safe because
				// ResolveFallbackChain rejects chains with any empty
				// provider (returns nil,nil,nil), so ClientProviders[child]
				// is guaranteed non-empty by the time we get here.
				continue
			}
			provider := config.ClientProviders[child]
			if provider == "" || !IsCallProviderSupported(provider) {
				return fmt.Errorf("buildrequest: unsupported or empty provider %q for child %q", provider, child)
			}
		}
	}

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

	// trySend attempts to send a StreamResult on the output channel,
	// prioritizing cancellation. Uses the priority-select idiom: check
	// ctx.Done() first with a non-blocking select, then fall through to
	// the blocking send only if the context is still active. This avoids
	// the race where ctx cancels between an if-guard and the select,
	// making both cases ready and allowing Go to nondeterministically
	// choose the send.
	trySend := func(r bamlutils.StreamResult) bool {
		select {
		case <-ctx.Done():
			r.Release()
			return false
		default:
		}
		select {
		case out <- r:
			return true
		case <-ctx.Done():
			r.Release()
			return false
		}
	}

	// callAttemptResult carries the final parsed value, raw text, and
	// reasoning text from the winning attempt, plus the winner's routing
	// identity for outcome metadata.
	type callAttemptResult struct {
		finalResult    any
		raw            string
		reasoning      string
		winnerClient   string
		winnerProvider string
		winnerPath     string
		// winnerEngine marks WHICH engine produced the result. Empty for the
		// ordinary BAML build/send and legacy paths (so outcome metadata is
		// byte-identical to today); set to the native marker only when the
		// native seam succeeds. See the native seam in tryOneChild.
		winnerEngine string
	}

	startTime := time.Now()

	// tryOneChild builds, executes, extracts, and parses a single child.
	tryOneChild := func(provider, clientOverride string) (*callAttemptResult, error) {
		// onResponseShadow is the optional SAME-response shadow continuation a
		// DECLINED native attempt may install (de-BAML cutover Slice 5). It stays
		// nil on every non-shadow path (every production constructor, and the
		// shadow profile when the request-plan comparison did not match), so the
		// BAML send below is byte-identical to today. When set, it is invoked ONCE
		// after a 2xx BAML send with BAML's already-fetched status+body for a
		// no-transport native-vs-BAML response-parity comparison — it never
		// RoundTrips and never changes the served result.
		var onResponseShadow func(ctx context.Context, status int, body []byte)

		// Native child-attempt seam (de-BAML cutover Slice 1), HARD-OFF unless
		// a caller both installs the callback AND flips the neutral enabled
		// gate. It runs as the FIRST operation for each selected non-legacy
		// child: legacy children are dispatched by attemptFull and never call
		// tryOneChild, so they never reach here. With the callback nil (every
		// production constructor) this block is skipped entirely and the
		// attempt is byte-identical to today.
		if config.NativeAttempt != nil && config.NativeAttemptEnabled {
			outcome := config.NativeAttempt(ctx, NativeCallAttempt{
				Provider:         provider,
				ClientOverride:   clientOverride,
				NeedsRaw:         config.NeedsRaw,
				IncludeReasoning: config.IncludeReasoning,
				OutputSchema:     config.NativeOutputSchema,
				// BuildBAMLRequest is the SAME per-attempt build closure the BAML
				// path uses below, pre-bound to this child's clientOverride. A
				// shadow comparator calls it to obtain BAML's built plan for the
				// same child WITHOUT sending; it opens no socket. On DECLINE the
				// orchestrator still runs buildRequest(ctx, clientOverride) itself
				// below (a second, cheap no-socket build), so the BAML send path is
				// byte-identical to today.
				BuildBAMLRequest: func(bctx context.Context) (*llmhttp.Request, error) {
					return buildRequest(bctx, clientOverride)
				},
				// Hand the native transport the SAME idempotent first-2xx
				// liveness signal the BAML path gets via ExecuteBorrowed's
				// onSuccess. A native impl must call this the instant it reads
				// 2xx response headers (before buffering the body) so the pool's
				// hung detector sees liveness on a slow body and does not
				// retry/restart a worker whose provider request is already in
				// flight. Idempotent + best-effort — see NativeCallAttempt.
				SendHeartbeat: sendHeartbeat,
			})
			switch outcome.Disposition {
			case NativeCallSucceeded:
				// Native produced the final value over a completed send. Return
				// it as an ordinary attempt result. raw/reasoning are gated on
				// NeedsRaw exactly like the BAML path; the callback owns them
				// (no borrowed transport buffer to clone — see the ownership
				// contract on NativeCallOutcome), so they cross the attempt
				// boundary as-is.
				var rawOut, reasoningOut string
				if config.NeedsRaw {
					rawOut = outcome.Raw
					reasoningOut = outcome.Reasoning
				}
				return &callAttemptResult{
					finalResult:    outcome.FinalResult,
					raw:            rawOut,
					reasoning:      reasoningOut,
					winnerProvider: provider,
					winnerPath:     "buildrequest",
					winnerEngine:   nativeEngineMarker,
				}, nil
			case NativeCallFailed:
				// A native socket may have opened; the error is terminal for
				// this child attempt. Hand it to the outer fallback/retry loop
				// exactly like a BAML attempt error — NEVER fall through to a
				// second BAML build/send for the same child.
				//
				// Normalize a nil error HERE, not just in FailNativeCall:
				// NativeCallOutcome is public and directly constructible, so a
				// literal NativeCallOutcome{Disposition: NativeCallFailed}
				// reaches this case with a nil Err. Returning (nil, nil) would
				// look like success to retry.Execute and then panic on the nil
				// *callAttemptResult. The sentinel keeps the failure terminal
				// and typed.
				if outcome.Err == nil {
					return nil, errNativeCallFailedNil
				}
				return nil, outcome.Err
			case NativeCallDeclined:
				// The callback guarantees no socket occurred. Fall through to
				// the existing BAML build/send for the SAME child in the SAME
				// retry iteration below — no extra retry consumed, no fallback
				// advance. Capture an optional SAME-response shadow continuation
				// to run against BAML's fetched response below; nil on every
				// non-shadow path, so the send stays byte-identical to today.
				onResponseShadow = outcome.OnResponseShadow
			default:
				// An out-of-contract disposition can't assert "no socket", so
				// treat it as terminal rather than risk a hidden second
				// same-child send. Unreachable for the closed NativeCall* set.
				return nil, fmt.Errorf("buildrequest: native call attempt returned unknown disposition %d", outcome.Disposition)
			}
		}

		req, err := buildRequest(ctx, clientOverride)
		if err != nil {
			return nil, fmt.Errorf("buildrequest: failed to build request: %w", err)
		}
		resp, httpErr := httpClient.ExecuteBorrowed(ctx, req, sendHeartbeat)
		if httpErr != nil {
			return nil, httpErr
		}
		// ExecuteBorrowed returns a response whose body may BORROW pooled
		// transport storage (the fasthttp lanes; the net/http lane is already
		// owned and Release is a no-op there). The orchestrator OWNS that
		// lifecycle — public BAML/dynclient callers never see a release
		// obligation. Release runs per-attempt: this defer fires when tryOneChild
		// returns, which is strictly before the next retry or fallback child runs,
		// so no borrowed buffer accumulates across attempts.
		//
		// Everything that crosses this attempt boundary must be an owned copy
		// taken BEFORE this Release fires: raw/reasoning escaping via
		// callAttemptResult (gated on NeedsRaw), raw on parse-failure, and the
		// diagnostic body on extraction-failure are each cloned below. Anything
		// consumed within the attempt — parseable feeding parseFinal — stays
		// borrowed.
		defer resp.Release()

		// SAME-response shadow oracle (de-BAML cutover Slice 5), declined native
		// attempt only. BAML has already fetched this 2xx response; hand its
		// status + raw body to the response-parity comparator WITHOUT any
		// transport or second provider request. It is strictly non-authoritative:
		// panic-guarded (a shadow FFI/parse panic must never fail an otherwise
		// BAML-served request) and does not touch the served result — BAML's
		// envelope is returned byte-identical regardless. nil on every non-shadow
		// path.
		//
		// Invoked SYNCHRONOUSLY on the attempt path by design (see
		// NativeCallOutcome.OnResponseShadow): the comparator's BAML-only parse
		// closure captures the per-request adapter + BAML CFFI runtime, which are
		// recycled once this serving goroutine returns, so detached/off-path
		// execution would risk a use-after-free / concurrent-CFFI race. The parity
		// CPU/latency is an accepted, shadow-profile-only cost removed by deployment
		// stage, not a latency-neutral production path. The body is cloned so the
		// comparator never aliases the borrowed
		// transport buffer this attempt's deferred Release recycles.
		if onResponseShadow != nil {
			runResponseShadow(ctx, onResponseShadow, resp.StatusCode, responseBodyClone(resp))
		}

		// Extractor routing — three lanes, gated on how the transport owns the
		// body, falling back to the always-present string extractor:
		//
		//  1. BORROWED (fasthttp unary success → resp.BorrowedBytes() != nil):
		//     the body aliases pooled storage that the deferred Release recycles.
		//     When a borrowed extractor is supplied we run it over those bytes;
		//     it aliases unescaped scalar / single-block content straight out of
		//     the borrowed buffer (gjson.Get over an unsafe string view), so our
		//     side copies nothing for that common shape. SAFE because the aliased
		//     parseable is consumed in-attempt by parseFinal — whose ParseFinalFunc
		//     contract requires it to copy (not retain a reference to) accumulated
		//     before returning, which the generated wrapper does by marshalling it
		//     into the protobuf parse args before Release — and every value that
		//     escapes the attempt is cloned below before the deferred Release fires.
		//  2. OWNED BYTES (net/http unary success → resp.BodyBytes() != nil): the
		//     io.ReadAll buffer is already caller-owned; the byte extractor parses
		//     it directly (gjson.GetBytes, which copies matched content out),
		//     skipping the whole-body string copy.
		//  3. STRING (no owned/borrowed bytes, or no byte extractor injected): run
		//     the injected extractResponse over resp.BodyString().
		//
		// This keeps the injection seam consistent: a custom string extractor is
		// always honored, and the byte/borrowed extractors only change
		// allocations, never which content is extracted. All three are pinned to
		// identical output for the same JSON by
		// TestExtractResponseContentBytesMatchesString /
		// TestExtractResponseContentBorrowedMatchesString.
		var parseable, raw, reasoning string
		var extractErr error
		if bb := resp.BorrowedBytes(); bb != nil && extractResponseBorrowed != nil {
			parseable, raw, reasoning, extractErr = extractResponseBorrowed(provider, bb, config.IncludeReasoning)
		} else if rb := resp.BodyBytes(); rb != nil && extractResponseBytes != nil {
			parseable, raw, reasoning, extractErr = extractResponseBytes(provider, rb, config.IncludeReasoning)
		} else {
			parseable, raw, reasoning, extractErr = extractResponse(provider, resp.BodyString(), config.IncludeReasoning)
		}
		if extractErr != nil {
			// Extraction failed before raw could be split out of the
			// provider's 2xx JSON body — but the body itself is the
			// diagnostic input the user wants to inspect (refusal text,
			// unexpected schema, etc.). Wrap with the raw body so the
			// outer emission forwards it as details.raw (per #256).
			// Pre-2xx provider errors don't reach this path — those
			// surface as *HTTPError from Execute above and map to
			// details.body via the existing provider_error classifier.
			// The body crosses the retry boundary inside the error, recovered by
			// rawFromError after retries; it aliases the borrowed buffer, so clone
			// it before the deferred Release returns that storage to the pool.
			return nil, newRawError(
				fmt.Errorf("buildrequest: failed to extract response content: %w", extractErr),
				strings.Clone(resp.BodyString()),
			)
		}

		finalResult, parseErr := parseFinal(ctx, parseable)
		if parseErr != nil {
			// Carry the extracted raw across the retry boundary so the
			// outer emission can forward it as details.raw on the
			// parse_error envelope (per #256). raw here is the
			// already-split text-only return from extractResponse —
			// not the full body — matching what success would have
			// surfaced on /call-with-raw.
			// raw crosses the retry boundary inside the error (recovered by
			// rawFromError); it may alias the borrowed buffer when extraction
			// returned a view into the body, so clone before the deferred Release.
			return nil, newRawError(
				wrapOutputParse(fmt.Errorf("buildrequest: failed to parse final result: %w", parseErr)),
				strings.Clone(raw),
			)
		}

		// raw/reasoning escape this attempt via callAttemptResult -> StreamResult,
		// which is BAML-facing — but only when NeedsRaw gates them through at the
		// final emission (see rawForFinal/reasoningForFinal below). They may alias
		// the borrowed buffer, so clone the pair out before the deferred Release
		// when needed; otherwise leave them empty so no borrowed view is retained
		// past Release.
		var rawOut, reasoningOut string
		if config.NeedsRaw {
			rawOut = strings.Clone(raw)
			reasoningOut = strings.Clone(reasoning)
		}
		return &callAttemptResult{
			finalResult:    finalResult,
			raw:            rawOut,
			reasoning:      reasoningOut,
			winnerProvider: provider,
			winnerPath:     "buildrequest",
		}, nil
	}

	// finalAttempt carries the 0-indexed attempt number of the winning call,
	// captured inside attemptFull because retry.Execute does not expose it
	// to the caller on success.
	var finalAttempt int

	// attemptFull tries either the single provider or the entire fallback
	// chain. For fallback chains, each retry attempt walks all children in
	// order — matching the BAML runtime's semantics where retries retry the
	// entire strategy after all children fail.
	attemptFull := func(attempt int) (any, error) {
		if len(config.FallbackChain) == 0 {
			result, err := tryOneChild(config.Provider, config.ClientOverride)
			if err == nil {
				finalAttempt = attempt
				if config.MetadataPlan != nil {
					result.winnerClient = config.MetadataPlan.Client
				}
			}
			return result, err
		}
		var lastErr error
		for _, child := range config.FallbackChain {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if config.LegacyChildren[child] {
				// Legacy children run via BAML's Stream API. The callback
				// owns heartbeat timing (fires sendHeartbeat on the first
				// FunctionLog tick) so pool hung-detection stays correct
				// for slow upstreams that eventually produce bytes. Note:
				// callback dispatch stays rooted at `child` (the
				// chain-position name) — FallbackTargets is honored only
				// for the BuildRequest path, since the legacy callback's
				// scoped registry depends on the wrapper identity.
				finalResult, raw, reasoning, err := config.LegacyCallChild(
					ctx,
					child,
					config.ClientProviders[child],
					config.NeedsRaw,
					sendHeartbeat,
				)
				if err == nil {
					finalAttempt = attempt
					return &callAttemptResult{
						finalResult:    finalResult,
						raw:            raw,
						reasoning:      reasoning,
						winnerClient:   child,
						winnerProvider: config.ClientProviders[child],
						winnerPath:     "legacy",
					}, nil
				}
				lastErr = err
				continue
			}
			provider := config.ClientProviders[child]
			// Wrapper-vs-target identity: when an immediate RR fallback
			// child was centrally unwrapped to a leaf,
			// FallbackTargets[child] names that leaf. Mirrors the
			// streaming orchestrator — see RunStreamOrchestration for
			// the full rationale.
			target := child
			if t, ok := config.FallbackTargets[child]; ok && t != "" {
				target = t
			}
			result, err := tryOneChild(provider, target)
			if err == nil {
				finalAttempt = attempt
				result.winnerClient = target
				return result, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	// Non-streaming retry callbacks do not emit heartbeats. In the pool,
	// any received result marks gotFirstByte=true before heartbeat results
	// are filtered, so an onRetry heartbeat would disable first-byte hung
	// detection even though no provider has produced a real 2xx response yet.
	result, err := retry.Execute(ctx, config.RetryPolicy, attemptFull, nil)

	if err != nil {
		// Per #256: failing attempts wrap their error with raw via
		// newRawError at the return site (extraction-failure carries
		// resp.Body, final-parse carries the extracted raw). rawFromError
		// walks the chain and returns "" when no wrapper is present —
		// the worker bridge's mergeRawDetail no-ops on empty raw,
		// matching the omitempty contract on details.raw.
		trySend(newResult(bamlutils.StreamResultKindError, nil, nil, rawFromError(err), "", err, false))
		return nil
	}

	winningResult := result.(*callAttemptResult)

	// Emit outcome metadata before the final result so clients observe the
	// routing outcome attached to (and ordered before) the terminal payload.
	if config.MetadataPlan != nil && config.NewMetadataResult != nil {
		outcome := *config.MetadataPlan
		outcome.Phase = bamlutils.MetadataPhaseOutcome
		outcome.Attempt = 0
		outcome.RetryMax = nil
		outcome.RetryPolicy = ""
		outcome.Chain = nil
		outcome.LegacyChildren = nil
		// See the streaming orchestrator's outcome builder for the
		// rationale on clearing these planned-only fallback-target
		// fields — the planned shape describes intent, the realised
		// winner already appears in WinnerClient.
		outcome.FallbackTargets = nil
		outcome.FallbackRoundRobin = nil
		outcome.Strategy = ""
		outcome.Provider = ""

		retryCount := finalAttempt
		outcome.RetryCount = &retryCount
		outcome.WinnerClient = winningResult.winnerClient
		outcome.WinnerProvider = winningResult.winnerProvider
		outcome.WinnerPath = winningResult.winnerPath
		// WinnerEngine is empty on the BAML build/send and legacy paths, so
		// this assignment leaves the outcome frame byte-identical to today for
		// every production (nil-callback) request; it is non-empty only when
		// the native seam won the attempt.
		outcome.WinnerEngine = winningResult.winnerEngine
		dur := time.Since(startTime).Milliseconds()
		outcome.UpstreamDurMs = &dur

		if !trySend(config.NewMetadataResult(&outcome)) {
			return nil
		}
	}

	rawForFinal := ""
	reasoningForFinal := ""
	if config.NeedsRaw {
		rawForFinal = winningResult.raw
		reasoningForFinal = winningResult.reasoning
	}
	trySend(newResult(bamlutils.StreamResultKindFinal, nil, winningResult.finalResult, rawForFinal, reasoningForFinal, nil, false))

	return nil
}

// runResponseShadow invokes a declined native attempt's SAME-response shadow
// continuation, guarding against a panic in the shadow oracle. The shadow work
// (native TranslateResponse / SAP / an independent BAML parse) is strictly
// non-authoritative: it must never fail an otherwise BAML-served request, so a
// panic is swallowed here (the comparator records its own error facet). It opens
// no socket and returns no value — BAML's envelope is served regardless.
func runResponseShadow(ctx context.Context, fn func(context.Context, int, []byte), status int, body []byte) {
	defer func() { _ = recover() }()
	fn(ctx, status, body)
}

// responseBodyClone returns an owned copy of the response body across all three
// transport lanes (borrowed / owned-bytes / string view). The SAME-response
// shadow oracle runs synchronously before this attempt's deferred Release, but
// the clone keeps it defensively independent of the pooled/borrowed buffer so no
// aliasing view can outlive Release.
func responseBodyClone(resp *llmhttp.Response) []byte {
	if bb := resp.BorrowedBytes(); bb != nil {
		return append([]byte(nil), bb...)
	}
	if rb := resp.BodyBytes(); rb != nil {
		return append([]byte(nil), rb...)
	}
	return append([]byte(nil), resp.BodyString()...)
}
