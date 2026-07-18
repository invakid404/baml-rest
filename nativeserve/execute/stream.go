package execute

// De-BAML Phase 7B native stream execution: drive nanollm DoStream THROUGH the
// Phase-7A one-shot exact stream client into normalized, OWNED answer/raw/
// reasoning deltas plus usage and typed TERMINAL errors.
//
// This is the streaming twin of attempt.go's unary RunAttempt. The unary lane
// owns its single RoundTrip through the exact EXECUTOR (bytes buffered, then
// TranslateResponse). Streaming cannot: nanollm's DoStream owns the socket and
// the SSE decoder, so the exactness guarantee is delegated to the *http.Client we
// hand it — 7A's NewExactStreamClient, whose one-shot RoundTripper compares the
// request DoStream actually produces against the already-admitted plan BEFORE any
// socket opens (scope §5.7, invariants I3/I4). A nanollm re-prepare drift or a
// retry/fallback regression is therefore a ZERO-SOCKET failure or a refused
// second call, never a duplicate physical request.
//
// Deliberate non-goals (matching the unary lane and the 7B scope):
//
//   - Exactly ONE physical native request. WithoutFallback disables the
//     cross-model chain and the admitted plan's Meta.MaxRetries==0 disables the
//     same-model retry, so DoStream performs one Prepare + one connect + one
//     RoundTrip; the exact client's one-shot gate is the defense in depth.
//   - No BAML anything. This never sends a BAML request, never parses through
//     BAML, and (Phase 7B) does NOT own partial/final SEMANTIC parsing — it emits
//     normalized transport deltas only. Native-only partial/final parsing is 7C.
//   - UNROUTED. Nothing in RunStreamOrchestration, the generated adapter, workers,
//     or any public route calls RunStream; it is a test-harness API composed with
//     admission.AdmitStreamClaim (which builds the Expected plan + the Request).
//   - Provider non-2xx (D11) is PRESERVED, never normalized into the uniform
//     envelope: DoStream's wrapped *nanollm.ProviderStatusError stays reachable
//     via errors.As, and ProviderStatusHTTPError maps it to the public
//     *llmhttp.HTTPError the serving path (7D) will surface.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/sse"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// StreamDelta is one normalized, OWNED increment from the native stream. Its
// three channels mirror the BAML transport path's sse.DeltaParts exactly:
// ParseableDelta is the text-only answer channel (what a native ParseStream would
// consume in 7C), RawDelta is the text-only /stream-with-raw channel (equal to
// ParseableDelta for OpenAI), and ReasoningDelta carries provider reasoning text
// only when the caller opted in. Every string is a self-owned copy — the SSE
// extractor copies out of gjson results and nanollm's decoder buffers — so a
// delta never aliases an FFI/decoder buffer and stays valid after EmitDelta
// returns.
type StreamDelta struct {
	ParseableDelta string
	RawDelta       string
	ReasoningDelta string
}

// StreamResult is the terminal, secret-free outcome of one native stream. Usage
// is nanollm's accumulated token usage, sampled ONLY after a valid terminal
// completion ([DONE] / clean upstream EOF); it is nil when the provider sent
// none. PartialCount is the number of non-empty normalized deltas successfully
// emitted — a bounded metric/test aid carrying no content.
type StreamResult struct {
	Usage        *nanollm.Usage
	PartialCount int
}

// StreamPhase is the bounded terminal-phase label for a post-claim native stream
// failure (scope §8 "terminal failures by phase"). It never carries content — it
// is a stable enum safe for a metric label.
type StreamPhase string

const (
	// StreamPhaseConnect: a transport failure before any usable response
	// (dial/TLS/reset), surfaced by DoStream's connect step.
	StreamPhaseConnect StreamPhase = "connect"
	// StreamPhaseStatus: a stream-start non-2xx — the nanollm ProviderStatusError
	// (D11) preserved, never normalized into the uniform envelope.
	StreamPhaseStatus StreamPhase = "status"
	// StreamPhaseProtocol: a protocol/invariant failure with zero or committed
	// socket — invalid 2xx content type, an exact plan MISMATCH, a refused SECOND
	// RoundTrip, or an outgoing body-read failure. All unwrap to llmhttp.ErrExactStream.
	StreamPhaseProtocol StreamPhase = "protocol"
	// StreamPhaseFirstBody: the exact client's first-body deadline fired (2xx
	// headers, then no body byte).
	StreamPhaseFirstBody StreamPhase = "first_body"
	// StreamPhaseIdle: the exact client's inter-byte idle deadline fired.
	StreamPhaseIdle StreamPhase = "idle"
	// StreamPhaseDecode: the SSE decoder failed or the stream truncated mid-frame
	// (a decoder Finish error / a non-EOF read error / a malformed chunk).
	StreamPhaseDecode StreamPhase = "decode"
	// StreamPhaseEmit: the synchronous EmitDelta callback returned an error, asking
	// execution to stop. Reading stops immediately; there is never a retry.
	StreamPhaseEmit StreamPhase = "emit"
	// StreamPhaseCancel: caller cancellation / deadline observed. It wins over any
	// timeout classification (scope §5.10 / I7).
	StreamPhaseCancel StreamPhase = "cancel"
)

// TerminalError is a post-claim native stream failure: a bounded Phase naming
// WHICH terminal condition occurred, wrapping the underlying cause. It unwraps to
// the cause, so errors.As reaches the preserved *nanollm.ProviderStatusError
// (D11), the typed llmhttp exact-stream errors, or a context error, while Phase
// stays a stable label for metrics/tests. Every one of these is TERMINAL: the
// point of no return was the transport claim, so no caller may retry, fall back,
// or resend after one is returned (invariant I4).
type TerminalError struct {
	Phase StreamPhase
	Err   error
}

func (e *TerminalError) Error() string {
	return fmt.Sprintf("nativeserve: native stream failed (%s): %v", e.Phase, e.Err)
}

func (e *TerminalError) Unwrap() error { return e.Err }

// StreamConfig is the input to RunStream: the request-scoped nanollm client and
// the OpenAI-format streaming Request (both from a StreamClaim), the admitted
// exact plan the outgoing request is compared against, the reasoning opt-in, the
// synchronous owned-delta callback, the two byte-progress deadlines, and the
// optional liveness callbacks forwarded to the exact client.
type StreamConfig struct {
	// Client is the request-scoped nanollm engine the admitted plan was Prepared
	// on. RunStream does NOT close it — the caller (the StreamClaim owner) does.
	Client *nanollm.Client
	// Request is the OpenAI-format streaming nanollm.Request. Its Body is the
	// UNARY canonical body; DoStream re-prepares it with Stream=true and the engine
	// injects the streaming suffix (which Expected already carries).
	Request nanollm.Request
	// Expected is the admitted exact streaming plan (StreamClaim.ExactRequest). The
	// exact client's RoundTripper compares the request DoStream produces against it
	// before any socket; any drift is a zero-socket protocol failure.
	Expected *llmhttp.ExactAttemptRequest

	// IncludeReasoning gates the reasoning channel exactly as the BAML transport
	// path does: off means ReasoningDelta is always empty.
	IncludeReasoning bool

	// EmitDelta is the SYNCHRONOUS owned-delta sink. Returning nil consumes the
	// delta; returning an error stops reading immediately (StreamPhaseEmit) with no
	// retry. Required.
	EmitDelta func(StreamDelta) error

	// FirstBodyTimeout / IdleTimeout are the two independent byte-progress
	// deadlines forwarded to the exact client (scope §5.10). <= 0 disables each.
	FirstBodyTimeout time.Duration
	IdleTimeout      time.Duration

	// OnClaim / OnResponseHeaders / OnFirstBody are the exact client's liveness
	// callbacks, fired in that fixed order. Any may be nil.
	OnClaim           func()
	OnResponseHeaders func()
	OnFirstBody       func()
}

// RunStream performs exactly one native stream attempt: it builds the 7A one-shot
// exact stream client from cfg.Expected, drives nanollm DoStream through it
// (WithoutFallback), normalizes every nonempty OpenAI chunk through the shared SSE
// extractor, synchronously emits owned deltas, samples usage after a valid
// terminal completion, and closes the decoder/body exactly once.
//
// Error-vs-terminal contract:
//
//   - A nil-config guard failure (nil client / nil Expected / nil EmitDelta /
//     empty Request body) returns (nil, error) BEFORE any socket — a caller/wiring
//     bug, not a claimed failure.
//   - A DoStream start failure (transport, stream-start non-2xx / D11, invalid
//     content type, plan mismatch, refused second call, or an already-observed
//     cancellation) returns (nil, *TerminalError). At most one socket was opened.
//   - A mid-stream failure (decoder/truncation, first-body/idle timeout,
//     cancellation, or an EmitDelta error) returns (partialResult, *TerminalError),
//     carrying the deltas emitted so far.
//   - A clean end returns (result, nil) with usage sampled and PartialCount set.
func RunStream(ctx context.Context, cfg StreamConfig) (*StreamResult, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("nativeserve: nil nanollm client")
	}
	if cfg.Expected == nil {
		return nil, fmt.Errorf("nativeserve: nil expected stream plan")
	}
	if cfg.EmitDelta == nil {
		return nil, fmt.Errorf("nativeserve: nil emit-delta callback")
	}
	if len(cfg.Request.Body) == 0 {
		return nil, fmt.Errorf("nativeserve: empty streaming request body")
	}

	// Build the 7A one-shot exact stream client. Its RoundTripper compares the
	// request DoStream actually produces against cfg.Expected before any socket,
	// fires OnClaim at the point of no return, validates 2xx text/event-stream, and
	// arms the first-body/idle deadlines on the streamed body.
	httpClient, err := llmhttp.NewExactStreamClient(llmhttp.ExactStreamClientConfig{
		Expected:          cfg.Expected,
		FirstBodyTimeout:  cfg.FirstBodyTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		OnClaim:           cfg.OnClaim,
		OnResponseHeaders: cfg.OnResponseHeaders,
		OnFirstBody:       cfg.OnFirstBody,
	})
	if err != nil {
		return nil, fmt.Errorf("nativeserve: building exact stream client: %w", err)
	}

	// The ONE physical native request. WithoutFallback + the admitted plan's
	// Meta.MaxRetries==0 make it a single Prepare + single connect + single
	// RoundTrip. DoStream re-prepares the request internally; the exact client
	// verifies that re-prepared request against cfg.Expected.
	stream, err := cfg.Client.DoStream(ctx, cfg.Request,
		nanollm.WithHTTPClient(httpClient),
		nanollm.WithoutFallback())
	if err != nil {
		return nil, classifyStreamError(ctx, err, false)
	}
	// Close the response body + decoder EXACTLY once on every exit (I7).
	defer stream.Close()

	res := &StreamResult{}
	for {
		chunk, nerr := stream.Next()
		if nerr == io.EOF {
			// A valid terminal completion: [DONE] or a clean upstream close.
			break
		}
		if nerr != nil {
			return res, classifyStreamError(ctx, nerr, true)
		}

		// Normalize the OpenAI chunk JSON through the SAME extractor the BAML
		// transport path uses, so answer/raw/reasoning split identically.
		//
		// OWNED-COPY INVARIANT (I: deltas must not alias FFI/decoder buffers):
		// chunk.Raw is a fresh per-event allocation from nanollm's decoder (not a
		// view into its reused scratch), and ExtractDeltaPartsFromBytes returns
		// owned strings because gjson.GetBytes copies each matched value out. The
		// emitted StreamDelta strings therefore stay valid after EmitDelta returns
		// and after the next Next(). A future no-copy JSON reader here would break
		// that — keep the copy guarantee (or strings.Clone the parts) if swapped.
		parts, xerr := sse.ExtractDeltaPartsFromBytes(nativebody.ProviderOpenAI, chunk.Raw, cfg.IncludeReasoning)
		if xerr != nil {
			return res, &TerminalError{Phase: StreamPhaseDecode, Err: fmt.Errorf("nativeserve: extracting stream delta: %w", xerr)}
		}
		// A usage-only / finish / role-only / empty-content chunk yields no
		// channel text — emit no spurious partial (matches the BAML path).
		if parts.Parseable == "" && parts.Raw == "" && parts.Reasoning == "" {
			continue
		}
		if eerr := cfg.EmitDelta(StreamDelta{
			ParseableDelta: parts.Parseable,
			RawDelta:       parts.Raw,
			ReasoningDelta: parts.Reasoning,
		}); eerr != nil {
			// The orchestrator asked execution to stop. Terminal, never retried.
			return res, &TerminalError{Phase: StreamPhaseEmit, Err: eerr}
		}
		res.PartialCount++
	}

	// Valid completion: sample usage BEFORE the deferred Close closes the decoder.
	if u, ok := stream.Usage(); ok {
		res.Usage = u
	}
	return res, nil
}

// classifyStreamError folds a DoStream start error (readPhase=false) or a
// stream-read error (readPhase=true) into a bounded *TerminalError, preserving
// the underlying cause for errors.As. Classification order encodes the scope's
// precedence:
//
//  1. caller cancellation / deadline wins over any timeout classification (§5.10);
//  2. a stream-start non-2xx is the preserved ProviderStatusError (D11);
//  3. the two exact-stream byte-progress deadlines are distinct phases;
//  4. every other exact-stream error (content type / plan mismatch / second call /
//     body read) is a protocol/invariant failure;
//  5. anything else is a decoder failure mid-read, or a connect failure at start.
func classifyStreamError(ctx context.Context, err error, readPhase bool) *TerminalError {
	// (1) Cancellation precedence: prefer the request context's own error so a
	// caller cancel that raced a provider stall is never mislabelled a timeout
	// (scope §5.10). This DELIBERATELY subordinates a racing stream-start non-2xx
	// too: once the caller has observed a cancel, the outcome is a cancel terminal,
	// not a D11 status — the request is already going away. (If a future serving
	// phase needs the real upstream status even under a racing cancel, it must
	// reorder this ahead of the ProviderStatusError check below.)
	if ce := ctx.Err(); ce != nil {
		return &TerminalError{Phase: StreamPhaseCancel, Err: ce}
	}
	if isContextErr(err) {
		return &TerminalError{Phase: StreamPhaseCancel, Err: err}
	}

	// (2) Stream-start non-2xx (D11): keep the ProviderStatusError intact.
	var pse *nanollm.ProviderStatusError
	if errors.As(err, &pse) {
		return &TerminalError{Phase: StreamPhaseStatus, Err: err}
	}

	// (3) The two distinct byte-progress deadlines (checked before the generic
	// ErrExactStream umbrella, which they also unwrap to).
	if errors.Is(err, llmhttp.ErrExactStreamFirstBodyTimeout) {
		return &TerminalError{Phase: StreamPhaseFirstBody, Err: err}
	}
	if errors.Is(err, llmhttp.ErrExactStreamIdleTimeout) {
		return &TerminalError{Phase: StreamPhaseIdle, Err: err}
	}

	// (4) Any other exact-stream failure (content type, plan mismatch, refused
	// second call, outgoing body read) is a protocol/invariant terminal.
	if errors.Is(err, llmhttp.ErrExactStream) {
		return &TerminalError{Phase: StreamPhaseProtocol, Err: err}
	}

	// (5) Fall through: a mid-read failure is a decoder/truncation error; a
	// start-time failure is a connect error.
	if readPhase {
		return &TerminalError{Phase: StreamPhaseDecode, Err: err}
	}
	return &TerminalError{Phase: StreamPhaseConnect, Err: err}
}

// isContextErr reports whether err is a caller cancellation or deadline.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ProviderStatusHTTPError maps a nanollm *ProviderStatusError found anywhere in
// err's chain to the public *llmhttp.HTTPError the serving path (Phase 7D) will
// surface: the REAL provider status and the provider-native error body capped to
// the 4 KiB PUBLIC diagnostic limit (llmhttp.MaxErrorBodyBytes), NOT the 64 KiB
// internal cap nanollm retains. It returns (nil, false) when err carries no
// ProviderStatusError. It NEVER normalizes into the uniform envelope — the D11
// divergence (the raw provider-native body) is preserved, only bounded.
func ProviderStatusHTTPError(err error) (*llmhttp.HTTPError, bool) {
	var pse *nanollm.ProviderStatusError
	if !errors.As(err, &pse) {
		return nil, false
	}
	body := pse.Body
	if len(body) > llmhttp.MaxErrorBodyBytes {
		body = body[:llmhttp.MaxErrorBodyBytes]
	}
	return &llmhttp.HTTPError{StatusCode: pse.Status, Body: string(body)}, true
}
