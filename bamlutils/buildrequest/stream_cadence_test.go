package buildrequest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
)

// stream_cadence_test.go pins the extracted cadence's behaviour — the accumulation
// rules, the throttle, the raw-decoupling, and the TWO parse-error policies. The
// legacy policy's rows are the behaviour RunStreamOrchestration's inline closure had,
// so this file is also the regression guard for the extraction being behaviour
// preserving.

// cadenceSink records every event the cadence produced, in order.
type cadenceSink struct {
	events []StreamCadenceEvent
	err    error
}

func (s *cadenceSink) emit(ev StreamCadenceEvent) error {
	s.events = append(s.events, ev)
	return s.err
}

// fakeClock is an injectable monotonic clock for throttle tests.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// TestCadenceSkipsEmptyChunks proves an empty / role-only / finish-only / usage-only
// chunk (all three channels empty) produces NO event on any mode.
func TestCadenceSkipsEmptyChunks(t *testing.T) {
	for _, needsRaw := range []bool{false, true} {
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true, NeedsRaw: needsRaw,
			ParsePartial: func(context.Context, string) (any, bool, error) { return "p", true, nil },
			Emit:         sink.emit,
		})
		if err := c.Delta(context.Background(), "", "", ""); err != nil {
			t.Fatalf("empty delta: %v", err)
		}
		if len(sink.events) != 0 {
			t.Fatalf("needsRaw=%v: empty chunk produced %d event(s)", needsRaw, len(sink.events))
		}
		if c.Parseable() != "" || c.Raw() != "" || c.Reasoning() != "" {
			t.Fatalf("needsRaw=%v: empty chunk mutated the accumulators", needsRaw)
		}
	}
}

// TestCadenceAccumulatesAllThreeChannels proves the accumulators are the FINAL parse's
// input and the raw/reasoning channels, and that a reasoning-only chunk is not skipped.
func TestCadenceAccumulatesAllThreeChannels(t *testing.T) {
	sink := &cadenceSink{}
	c := NewStreamCadence(StreamCadenceConfig{
		NeedsPartials: true, NeedsRaw: true,
		ParsePartial: func(context.Context, string) (any, bool, error) { return nil, false, nil },
		Emit:         sink.emit,
	})
	ctx := context.Background()
	mustDelta := func(p, r, rs string) {
		t.Helper()
		if err := c.Delta(ctx, p, r, rs); err != nil {
			t.Fatalf("Delta(%q,%q,%q): %v", p, r, rs, err)
		}
	}
	mustDelta(`{"k"`, `{"k"`, "")
	mustDelta("", "", "thinking")
	mustDelta(`:1}`, `:1}`, "")

	if got, want := c.Parseable(), `{"k":1}`; got != want {
		t.Errorf("Parseable() = %q, want %q", got, want)
	}
	if got, want := c.Raw(), `{"k":1}`; got != want {
		t.Errorf("Raw() = %q, want %q", got, want)
	}
	if got, want := c.Reasoning(), "thinking"; got != want {
		t.Errorf("Reasoning() = %q, want %q", got, want)
	}
	if len(sink.events) != 3 {
		t.Fatalf("got %d events, want 3 raw-only deltas (including the reasoning-only chunk)", len(sink.events))
	}
	if sink.events[1].Raw != "" || sink.events[1].Reasoning != "thinking" {
		t.Errorf("reasoning-only event = (%q, %q), want an empty raw with the reasoning", sink.events[1].Raw, sink.events[1].Reasoning)
	}
}

// TestCadenceRawIsDecoupledFromParseSuccess is the customer-visible rule: on a
// raw-wanted stream, raw/reasoning are delivered whether the structured parse
// succeeded, failed, returned nil, or was throttle-skipped. On a NON-raw stream none of
// those ticks produces an event.
func TestCadenceRawIsDecoupledFromParseSuccess(t *testing.T) {
	parsers := map[string]StreamCadenceParseFunc{
		"parse_error": func(context.Context, string) (any, bool, error) { return nil, false, errors.New("boom") },
		"nil_result":  func(context.Context, string) (any, bool, error) { return nil, false, nil },
	}
	for name, parse := range parsers {
		t.Run(name+"_with_raw", func(t *testing.T) {
			sink := &cadenceSink{}
			c := NewStreamCadence(StreamCadenceConfig{
				NeedsPartials: true, NeedsRaw: true, ParsePartial: parse, Emit: sink.emit,
			})
			if err := c.Delta(context.Background(), "x", "x", ""); err != nil {
				t.Fatalf("Delta: %v", err)
			}
			if len(sink.events) != 1 || sink.events[0].HasPartial || sink.events[0].Raw != "x" {
				t.Fatalf("events = %+v, want one raw-only delta", sink.events)
			}
		})
		t.Run(name+"_without_raw", func(t *testing.T) {
			sink := &cadenceSink{}
			c := NewStreamCadence(StreamCadenceConfig{
				NeedsPartials: true, ParsePartial: parse, Emit: sink.emit,
			})
			if err := c.Delta(context.Background(), "x", "x", ""); err != nil {
				t.Fatalf("Delta: %v", err)
			}
			if len(sink.events) != 0 {
				t.Fatalf("events = %+v, want none (a non-raw stream keeps a strict cadence)", sink.events)
			}
		})
	}
}

// TestCadenceThrottleUsesTheInjectedClock proves the throttle skips a parse inside the
// interval, updates the timestamp REGARDLESS of parse outcome (so repeated failures
// cannot bypass it), and that a zero interval parses every tick.
func TestCadenceThrottleUsesTheInjectedClock(t *testing.T) {
	clk := newFakeClock()
	var parses int
	sink := &cadenceSink{}
	c := NewStreamCadence(StreamCadenceConfig{
		NeedsPartials:         true,
		ParseThrottleInterval: 100 * time.Millisecond,
		ParsePartial: func(context.Context, string) (any, bool, error) {
			parses++
			return nil, false, errors.New("always fails")
		},
		Emit: sink.emit,
		Now:  clk.Now,
	})
	ctx := context.Background()
	// First tick parses (lastParseTime is the zero time, far in the past).
	_ = c.Delta(ctx, "a", "a", "")
	// Two more ticks well inside the interval must NOT parse — the failing parse still
	// updated the timestamp.
	clk.advance(10 * time.Millisecond)
	_ = c.Delta(ctx, "b", "b", "")
	clk.advance(10 * time.Millisecond)
	_ = c.Delta(ctx, "c", "c", "")
	if parses != 1 {
		t.Fatalf("parses = %d inside the throttle window, want 1 (a failing parse must still arm the throttle)", parses)
	}
	clk.advance(100 * time.Millisecond)
	_ = c.Delta(ctx, "d", "d", "")
	if parses != 2 {
		t.Fatalf("parses = %d after the interval elapsed, want 2", parses)
	}

	// A zero interval parses on every tick.
	parses = 0
	c2 := NewStreamCadence(StreamCadenceConfig{
		NeedsPartials: true,
		ParsePartial: func(context.Context, string) (any, bool, error) {
			parses++
			return nil, false, nil
		},
		Emit: sink.emit,
		Now:  clk.Now,
	})
	for i := 0; i < 3; i++ {
		_ = c2.Delta(ctx, "x", "x", "")
	}
	if parses != 3 {
		t.Fatalf("parses = %d with a zero throttle interval, want 3", parses)
	}
}

// TestCadenceFirstTickAlwaysParsesRegardlessOfTheClockEpoch pins that the FIRST delta in
// a window parses even under a positive throttle, whatever epoch the injected clock
// starts at. Deriving "have we parsed yet" from lastParseTime's ZERO value made that
// depend on the clock: a clock starting at (or near) the zero time reads as "inside the
// interval" and suppresses every structured partial until it advances past the throttle.
// Real time is far from the zero time, so the production clock never showed it.
func TestCadenceFirstTickAlwaysParsesRegardlessOfTheClockEpoch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Time
	}{
		{"zero_time_clock", time.Time{}},
		{"just_after_the_zero_time", time.Time{}.Add(time.Millisecond)},
		{"ordinary_wall_clock", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: tc.start}
			var parses int
			sink := &cadenceSink{}
			c := NewStreamCadence(StreamCadenceConfig{
				NeedsPartials:         true,
				ParseThrottleInterval: time.Hour,
				ParsePartial: func(context.Context, string) (any, bool, error) {
					parses++
					return "p", true, nil
				},
				Emit: sink.emit,
				Now:  clk.Now,
			})
			if err := c.Delta(context.Background(), "x", "x", ""); err != nil {
				t.Fatalf("Delta: %v", err)
			}
			if parses != 1 {
				t.Fatalf("parses = %d on the first tick, want 1 (the throttle must not suppress it)", parses)
			}
			if len(sink.events) != 1 || !sink.events[0].HasPartial {
				t.Fatalf("events = %+v, want one structured partial", sink.events)
			}
			// And the throttle is genuinely armed afterwards: a second tick inside the
			// interval does NOT parse again.
			clk.advance(time.Second)
			_ = c.Delta(context.Background(), "y", "y", "")
			if parses != 1 {
				t.Fatalf("parses = %d inside the throttle window, want 1", parses)
			}
		})
	}
}

// TestCadenceLegacyPolicySwallowsParseErrors is the byte-for-byte guard on the standard
// orchestrator's behaviour: under CadenceParseErrorsAreNoEvent, ANY partial parse error
// — sentinel or not — is a benign no-event and the stream continues.
func TestCadenceLegacyPolicySwallowsParseErrors(t *testing.T) {
	for _, parseErr := range []error{
		errors.New("arbitrary parser failure"),
		fmt.Errorf("declined: %w", bamlutils.ErrDeBAMLParseUnsupported),
	} {
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true, NeedsRaw: true,
			ParsePartial: func(context.Context, string) (any, bool, error) { return nil, false, parseErr },
			ParsePolicy:  CadenceParseErrorsAreNoEvent,
			Emit:         sink.emit,
		})
		if err := c.Delta(context.Background(), "x", "x", ""); err != nil {
			t.Fatalf("legacy policy propagated a partial parse error: %v", err)
		}
		if len(sink.events) != 1 || sink.events[0].HasPartial {
			t.Fatalf("events = %+v, want one raw-only delta", sink.events)
		}
	}
}

// TestCadenceStrictPolicySplitsSentinelFromFailure is the M3e-A rule, stated as the
// cadence now enforces it: "no structured partial for this prefix" is an explicit
// RESULT of the callback and is benign; every ERROR that reaches the cadence propagates,
// so the claimed spine stream terminates on it.
func TestCadenceStrictPolicySplitsSentinelFromFailure(t *testing.T) {
	t.Run("explicit_no_partial_is_benign", func(t *testing.T) {
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true,
			ParsePartial: func(context.Context, string) (any, bool, error) {
				// The callback resolved the parser's sentinel itself: the cadence sees
				// the explicit no-partial result, never an error.
				return nil, false, nil
			},
			ParsePolicy: CadenceParseErrorsAreTerminal,
			Emit:        sink.emit,
		})
		if err := c.Delta(context.Background(), "{", "{", ""); err != nil {
			t.Fatalf("the explicit no-partial result terminated the stream: %v", err)
		}
		if len(sink.events) != 0 {
			t.Fatalf("events = %+v, want none", sink.events)
		}
	})

	// The P1 regression at the cadence level: an error whose CHAIN contains the parser's
	// no-partial sentinel is STILL terminal. Only the explicit no-partial RESULT is
	// benign, so a decoder that returned or wrapped that sentinel can never be read as
	// "no event". A cadence that re-introduced an errors.Is(ErrDeBAMLParseUnsupported)
	// check would swallow this row.
	t.Run("an_error_wrapping_the_sentinel_is_terminal", func(t *testing.T) {
		wrapped := fmt.Errorf("decode static alias stream: %w", bamlutils.ErrDeBAMLParseUnsupported)
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true, NeedsRaw: true,
			ParsePartial: func(context.Context, string) (any, bool, error) { return nil, false, wrapped },
			ParsePolicy:  CadenceParseErrorsAreTerminal,
			Emit:         sink.emit,
		})
		err := c.Delta(context.Background(), "x", "x", "")
		if !errors.Is(err, wrapped) {
			t.Fatalf("Delta = %v, want the sentinel-wrapping error propagated as terminal", err)
		}
		if len(sink.events) != 0 {
			t.Fatalf("events = %+v, want none after a terminal parse failure", sink.events)
		}
	})

	t.Run("other_errors_are_terminal", func(t *testing.T) {
		boom := errors.New("carrier decode failed")
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true, NeedsRaw: true,
			ParsePartial: func(context.Context, string) (any, bool, error) { return nil, false, boom },
			ParsePolicy:  CadenceParseErrorsAreTerminal,
			Emit:         sink.emit,
		})
		err := c.Delta(context.Background(), "x", "x", "")
		if !errors.Is(err, boom) {
			t.Fatalf("Delta = %v, want the parser error propagated", err)
		}
		// It must terminate BEFORE emitting a raw-only consolation frame: the stream is
		// over, and a post-error event would be a public emission after the fault.
		if len(sink.events) != 0 {
			t.Fatalf("events = %+v, want none after a terminal parse failure", sink.events)
		}
	})
}

// TestCadenceStructuredEventShape proves a structured partial carries the parsed value
// with mode-gated raw/reasoning, and that a TYPED NIL partial is a PRESENT event.
func TestCadenceStructuredEventShape(t *testing.T) {
	type carrier struct{ V int }
	parsed := &carrier{V: 1}

	t.Run("with_raw", func(t *testing.T) {
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true, NeedsRaw: true,
			ParsePartial: func(context.Context, string) (any, bool, error) { return parsed, true, nil },
			Emit:         sink.emit,
		})
		_ = c.Delta(context.Background(), "x", "x", "why")
		want := StreamCadenceEvent{HasPartial: true, Partial: parsed, Raw: "x", Reasoning: "why"}
		if !reflect.DeepEqual(sink.events, []StreamCadenceEvent{want}) {
			t.Fatalf("events = %+v, want %+v", sink.events, want)
		}
	})

	t.Run("without_raw_strips_channels", func(t *testing.T) {
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true,
			ParsePartial:  func(context.Context, string) (any, bool, error) { return parsed, true, nil },
			Emit:          sink.emit,
		})
		_ = c.Delta(context.Background(), "x", "x", "why")
		want := StreamCadenceEvent{HasPartial: true, Partial: parsed}
		if !reflect.DeepEqual(sink.events, []StreamCadenceEvent{want}) {
			t.Fatalf("events = %+v, want %+v (a plain /stream carries no raw/reasoning)", sink.events, want)
		}
	})

	t.Run("presence_is_the_callbacks_verdict_not_a_nil_check", func(t *testing.T) {
		// A callback that reports a PRESENT partial whose value is an untyped nil is
		// still an event: the cadence must not re-derive presence from the value, or a
		// decoder's successful nil result would be silently collapsed into "no event".
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true,
			ParsePartial:  func(context.Context, string) (any, bool, error) { return nil, true, nil },
			Emit:          sink.emit,
		})
		_ = c.Delta(context.Background(), "null", "null", "")
		if len(sink.events) != 1 || !sink.events[0].HasPartial {
			t.Fatalf("events = %+v, want one PRESENT partial", sink.events)
		}
	})

	t.Run("typed_nil_partial_is_present", func(t *testing.T) {
		var typedNil *carrier
		sink := &cadenceSink{}
		c := NewStreamCadence(StreamCadenceConfig{
			NeedsPartials: true,
			ParsePartial:  func(context.Context, string) (any, bool, error) { return typedNil, true, nil },
			Emit:          sink.emit,
		})
		_ = c.Delta(context.Background(), "null", "null", "")
		if len(sink.events) != 1 || !sink.events[0].HasPartial {
			t.Fatalf("events = %+v, want one PRESENT (typed-nil) partial", sink.events)
		}
	})
}

// TestCadenceNoPartialsModeEmitsNothing proves a unary-shaped cadence (NeedsPartials
// false) accumulates but never emits — the whole stream is the final.
func TestCadenceNoPartialsModeEmitsNothing(t *testing.T) {
	sink := &cadenceSink{}
	var parses int
	c := NewStreamCadence(StreamCadenceConfig{
		ParsePartial: func(context.Context, string) (any, bool, error) { parses++; return "p", true, nil },
		Emit:         sink.emit,
	})
	_ = c.Delta(context.Background(), "x", "x", "r")
	if len(sink.events) != 0 {
		t.Fatalf("events = %+v, want none", sink.events)
	}
	if parses != 0 {
		t.Fatalf("parses = %d, want 0 (no partial parse without NeedsPartials)", parses)
	}
	if c.Parseable() != "x" || c.Raw() != "x" || c.Reasoning() != "r" {
		t.Fatal("a partial-less cadence must still accumulate for the final")
	}
}

// TestCadenceSinkErrorPropagates proves a sink error (the caller's cancellation signal)
// stops the caller reading, on both the structured and the raw-only path.
func TestCadenceSinkErrorPropagates(t *testing.T) {
	stop := errors.New("context cancelled")
	for _, tc := range []struct {
		name  string
		parse StreamCadenceParseFunc
	}{
		{"structured", func(context.Context, string) (any, bool, error) { return "p", true, nil }},
		{"raw_only", func(context.Context, string) (any, bool, error) { return nil, false, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &cadenceSink{err: stop}
			c := NewStreamCadence(StreamCadenceConfig{
				NeedsPartials: true, NeedsRaw: true, ParsePartial: tc.parse, Emit: sink.emit,
			})
			if err := c.Delta(context.Background(), "x", "x", ""); !errors.Is(err, stop) {
				t.Fatalf("Delta = %v, want the sink error propagated", err)
			}
		})
	}
}

// TestCadenceNilParserStillDeliversRaw proves a cadence with no parser is legal: a
// raw-wanted stream keeps flowing, a plain stream emits nothing.
func TestCadenceNilParserStillDeliversRaw(t *testing.T) {
	sink := &cadenceSink{}
	c := NewStreamCadence(StreamCadenceConfig{NeedsPartials: true, NeedsRaw: true, Emit: sink.emit})
	if err := c.Delta(context.Background(), "x", "x", ""); err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].HasPartial || sink.events[0].Raw != "x" {
		t.Fatalf("events = %+v, want one raw-only delta", sink.events)
	}
}
