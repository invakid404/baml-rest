package buildrequest

import (
	"context"
	"strings"
	"time"
)

// stream_cadence.go is the SHARED, BAML-FREE per-delta accumulation + throttled-partial
// cadence: the single behavioral source of truth for
//
//   - parseable / raw / reasoning accumulation over one child/attempt window,
//   - the ParseThrottleInterval decision (and the "update the timestamp regardless of
//     parse success" rule that stops repeated failures bypassing the throttle),
//   - "no event for an empty, role-only, finish-only, or usage-only chunk",
//   - raw/reasoning delivery even when a structured partial is absent or throttled,
//   - non-blocking partial delivery with drop-on-full and cancellation semantics.
//
// It is deliberately BAML-agnostic: it knows nothing about any parser sentinel, because
// "no structured partial for this prefix" is an explicit RESULT of the parse callback
// rather than an error the cadence has to recognize.
//
// It was extracted VERBATIM from RunStreamOrchestration's processDelta closure so the
// three engines that drive it — the BAML SSE transport, the claimed native-stream seam
// in the standard orchestrator, and the M3e-A BAML-free spine stream executor — cannot
// drift in partial cadence. The orchestrator's own behavior is unchanged BYTE-FOR-BYTE:
// it keeps [CadenceParseErrorsAreNoEvent], the policy its inline closure encoded.
//
// It imports no BAML, no generated client, and no transport: a caller supplies the
// parser and the event sink, and the cadence owns only the accumulation, the throttle,
// and the event SHAPE.

// StreamCadenceParsePolicy selects how a PARTIAL parser/decoder error is treated. It is
// the one deliberate difference between the legacy and spine lanes, and it is a policy
// on the cadence — never a per-call flag a caller can flip mid-stream.
type StreamCadenceParsePolicy uint8

const (
	// CadenceParseErrorsAreNoEvent is the LEGACY/standard policy: ANY partial parse
	// error, and a nil parse result, mean "no structured event for this prefix" and the
	// stream continues (for a raw-wanted stream, raw/reasoning still flow). It is what
	// the orchestrator's inline closure did, and the streaming behaviour of every
	// generated BAML/hybrid method depends on it: prose streamed against a class schema
	// fails ParseStream for every prefix and must not terminate the stream.
	CadenceParseErrorsAreNoEvent StreamCadenceParsePolicy = iota

	// CadenceParseErrorsAreTerminal is the STRICT policy the BAML-free spine lane uses:
	// EVERY error from the parse callback propagates to the caller and becomes a
	// post-claim TERMINAL outcome. Swallowing one (as the legacy policy does) would hide
	// a genuine failure of a bundle admission already proved supported.
	//
	// The cadence deliberately knows NOTHING about any parser sentinel. "No structured
	// partial for this prefix" is a first-class RESULT of the callback
	// ([StreamCadenceParseFunc]'s hasPartial=false with a nil error), not an error the
	// cadence has to recognize. That separation is what stops a DECODER error whose
	// chain happens to contain the parser's no-partial sentinel from being read as a
	// benign no-event: the callback resolves the parser's sentinel BEFORE it invokes the
	// decoder, so by the time an error reaches here it is unambiguously terminal.
	CadenceParseErrorsAreTerminal
)

// StreamCadenceEvent is one cadence-decided public event: a structured partial, a
// raw-only delta, or both. Raw/Reasoning are ALREADY mode-gated — they are empty unless
// the cadence was configured NeedsRaw — so a sink forwards them verbatim.
type StreamCadenceEvent struct {
	// HasPartial reports that a structured partial is present. It is authoritative:
	// Partial may legitimately be a TYPED nil (a present-but-null partial for a nullable
	// family), which must be forwarded as an event, not collapsed into "no event".
	HasPartial bool
	Partial    any
	Raw        string
	Reasoning  string
}

// StreamCadenceParseFunc parses the ACCUMULATED parseable text into a structured
// partial. Its three results are a CLOSED set:
//
//   - (partial, true, nil): a structured partial is PRESENT and must be emitted.
//     Presence is the callback's own verdict, not a nil check by the cadence, so a
//     TYPED-NIL carrier (a present-but-null partial for a nullable family) is forwarded
//     as an event rather than collapsed into "no event".
//   - (nil, false, nil): there is NO structured partial for this prefix. This is the
//     explicit, benign no-event result under BOTH policies — never an error.
//   - (_, _, err): the parse FAILED. Its treatment is the cadence's
//     [StreamCadenceParsePolicy], and under the strict policy every such error is
//     terminal regardless of its error chain.
//
// A callback that composes a parser with a decoder MUST resolve the parser's own
// "no partial yet" outcome into the second form BEFORE invoking the decoder, so a
// decoder failure can never be mistaken for a benign no-partial.
//
// The accumulated string is BORROWED for the duration of the call: an implementation
// must copy anything it retains.
type StreamCadenceParseFunc func(ctx context.Context, accumulated string) (partial any, hasPartial bool, err error)

// StreamCadenceConfig configures one cadence. NeedsPartials/NeedsRaw are the public
// mode's two predicates; ParseThrottleInterval, ParsePartial, and Emit are the
// orchestration seams; Now is injectable so throttle behaviour is testable without
// sleeping.
type StreamCadenceConfig struct {
	NeedsPartials         bool
	NeedsRaw              bool
	ParseThrottleInterval time.Duration

	// ParsePartial may be nil, which means "never attempt a structured partial" — the
	// raw channels still flow for a raw-wanted stream.
	ParsePartial StreamCadenceParseFunc

	// Emit is the event sink. Returning an error stops the caller reading; the cadence
	// propagates it verbatim.
	Emit func(StreamCadenceEvent) error

	ParsePolicy StreamCadenceParsePolicy

	// Now defaults to time.Now.
	Now func() time.Time
}

// streamAccumulator holds one child/attempt window's parseable/raw/reasoning
// accumulation plus the throttle clock. It is the per-window state the cadence mutates,
// letting the SSE transport path and the native EmitDelta paths drive byte-identical
// partial cadence. A fresh accumulator is created per child/attempt window.
type streamAccumulator struct {
	parseable     strings.Builder
	raw           strings.Builder
	reasoning     strings.Builder
	lastParseTime time.Time
}

// StreamCadence is one child/attempt window's cadence driver. It is NOT safe for
// concurrent use: a stream window has exactly one reader.
type StreamCadence struct {
	cfg StreamCadenceConfig
	acc streamAccumulator
}

// NewStreamCadence returns a cadence over a FRESH per-window accumulator.
func NewStreamCadence(cfg StreamCadenceConfig) *StreamCadence {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &StreamCadence{cfg: cfg}
}

// Parseable, Raw, and Reasoning return the window's accumulated channels. The FINAL
// parse runs over Parseable; Raw/Reasoning are the accumulated /stream-with-raw
// channels (and the raw diagnostic carried on a terminal failure).
func (c *StreamCadence) Parseable() string { return c.acc.parseable.String() }
func (c *StreamCadence) Raw() string       { return c.acc.raw.String() }
func (c *StreamCadence) Reasoning() string { return c.acc.reasoning.String() }

// Delta feeds one normalized delta triple through the cadence: it accumulates the three
// channels, decides whether this tick produces a structured partial, a raw-only event,
// or no event at all, and hands any event to the sink.
//
// It returns a non-nil error ONLY when the sink asked to stop (a cancelled partial
// send, or a sink failure) or — under [CadenceParseErrorsAreTerminal] — when the
// partial parser failed for a non-sentinel reason. Every benign case (an empty chunk, a
// throttled tick, a declined prefix) returns nil, exactly as the orchestrator's inline
// cadence did.
func (c *StreamCadence) Delta(ctx context.Context, parseableDelta, rawDelta, reasoningDelta string) error {
	// Skip when nothing meaningful arrived on any channel. Under IncludeReasoning=true a
	// reasoning-only event has empty raw/parseable but non-empty reasoning — so the gate
	// must consider reasoning too.
	if rawDelta == "" && parseableDelta == "" && reasoningDelta == "" {
		return nil
	}

	c.acc.raw.WriteString(rawDelta)
	if parseableDelta != "" {
		c.acc.parseable.WriteString(parseableDelta)
	}
	if reasoningDelta != "" {
		c.acc.reasoning.WriteString(reasoningDelta)
	}

	if c.cfg.NeedsPartials && parseableDelta == "" {
		if c.cfg.NeedsRaw {
			return c.emitRawOnly(rawDelta, reasoningDelta)
		}
		return nil
	}

	// Emit a partial if needed. Non-blocking sends for partials/deltas are the sink's
	// concern: drop when the output buffer is full so the LLM stream keeps draining
	// (matching the legacy path, which intentionally drops non-reset partials rather
	// than coupling upstream reads to downstream consumer backpressure).
	if c.cfg.NeedsPartials {
		// Attempt a throttled structured parse, but DELIVER RAW REGARDLESS of whether
		// the parse succeeds, errors, returns nil, OR the tick is throttle-skipped: for a
		// NeedsRaw stream, raw partials must not depend on structured-parse success or
		// cadence (prose streamed against a class schema fails the parse for every
		// prefix, which would otherwise hide every live partial).
		var parsed any
		var hasPartial bool
		if c.cfg.ParsePartial != nil {
			shouldParse := c.cfg.ParseThrottleInterval == 0 ||
				c.cfg.Now().Sub(c.acc.lastParseTime) >= c.cfg.ParseThrottleInterval
			if shouldParse {
				// Update the throttle timestamp regardless of parse success/failure so
				// repeated failures don't bypass the throttle interval.
				c.acc.lastParseTime = c.cfg.Now()
				candidate, present, parseErr := c.cfg.ParsePartial(ctx, c.acc.parseable.String())
				if parseErr != nil {
					if err := c.classifyParseError(parseErr); err != nil {
						return err
					}
				} else if present {
					parsed, hasPartial = candidate, true
				}
			}
		}
		if hasPartial {
			rawForResult := ""
			reasoningForResult := ""
			if c.cfg.NeedsRaw {
				rawForResult = rawDelta
				reasoningForResult = reasoningDelta
			}
			return c.cfg.Emit(StreamCadenceEvent{
				HasPartial: true,
				Partial:    parsed,
				Raw:        rawForResult,
				Reasoning:  reasoningForResult,
			})
		}
		if c.cfg.NeedsRaw {
			// No structured partial this tick (a swallowed parse error under the legacy
			// policy, an explicit no-partial result, OR a throttle-skipped tick). Emit
			// the raw/reasoning delta on its own so live raw survives a parse miss and a
			// throttle skip alike. Non-raw streams never enter this branch, preserving
			// their strict cadence.
			return c.emitRawOnly(rawDelta, reasoningDelta)
		}
	}
	return nil
}

// emitRawOnly hands the sink a partial-less raw/reasoning delta.
func (c *StreamCadence) emitRawOnly(rawDelta, reasoningDelta string) error {
	return c.cfg.Emit(StreamCadenceEvent{Raw: rawDelta, Reasoning: reasoningDelta})
}

// classifyParseError applies the cadence's parse policy to a non-nil partial parse
// error: nil to continue with no structured event, or the error itself to stop.
//
// It inspects only the POLICY, never the error's chain. A parse callback that wants an
// outcome treated as benign must return it as the explicit no-partial result
// ([StreamCadenceParseFunc]'s hasPartial=false with a nil error); anything that arrives
// here as an error is a failure, and under the strict policy it is terminal — including
// a decoder error that happens to return or wrap a parser sentinel.
func (c *StreamCadence) classifyParseError(err error) error {
	if c.cfg.ParsePolicy != CadenceParseErrorsAreTerminal {
		// LEGACY: a partial parse error is a no-event. Preserved byte-for-byte.
		return nil
	}
	return err
}
