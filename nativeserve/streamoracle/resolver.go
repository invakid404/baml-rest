// Package streamoracle is the PURE, transport-free ExecBridge-U1s per-prefix and final
// oracle: the closed decision matrix that resolves ONE accumulated prefix (or the completed
// text) from a native leg and a BAML leg into exactly one public action.
//
// It is the streaming twin of nativeserve/staticoracle and is deliberately a separate
// package from nativeserve/spine for the same reason: the matrix is where the safety of
// the default-serve flip lives, so it must be exercisable as a pure table — no admission,
// no nanollm engine, no socket, no cadence, no CFFI. Every input is a function value the
// caller supplies; this package opens nothing and knows nothing about how either leg is
// built.
//
// THREE PROPERTIES ARE LOAD-BEARING.
//
//   - BOTH legs run on the SAME string. The caller hands one prefix in and both closures
//     receive that identical value, which is what makes "native and BAML saw the same
//     bytes" a provable statement rather than an assumption.
//   - The comparison is on PUBLIC MARSHALED BYTES, produced by the same serializer the
//     worker uses at release (sonic.Marshal). Not reflect.DeepEqual, not canonical parser
//     JSON, not semantic map equality: two values that marshal to different public bytes
//     ARE a public difference, whatever an in-process comparison would say about them.
//   - BAML is the SAFETY AUTHORITY after the claim, and native is not. A native failure
//     whose prefix BAML has already answered is DRIFT (serve BAML's answer, latch the
//     attribution) — never a reason to kill a claimed stream. A BAML leg that cannot be
//     ESTABLISHED is terminal even when native produced a value, because the request has
//     then lost its oracle.
package streamoracle

import (
	"context"
	"errors"
	"fmt"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
)

// NativePrefixParse is the NATIVE leg for one accumulated prefix, already composed with the
// standard method's carrier decoder by the caller. Its three results are a CLOSED set,
// matching [bamlutils.StreamCadenceParseFunc]'s discipline:
//
//   - (value, true, nil): native established a structured partial for this prefix;
//   - (nil, false, nil): native's documented "no parseable partial for this prefix yet"
//     sentinel — an ORDINARY outcome for an incomplete prefix, never a failure;
//   - (_, _, err): native's parse or decode FAILED. That is drift, not a stream-killer:
//     the BAML leg still gets its chance to establish the authoritative answer.
//
// The caller MUST resolve the parser's own no-partial sentinel into the second form BEFORE
// invoking the decoder, so a decoder failure can never be read as a benign no-value.
type NativePrefixParse func(ctx context.Context, prefix string) (any, bool, error)

// NativeFinalParse is the NATIVE leg for the COMPLETED accumulated text, already composed
// with the standard method's final carrier decoder. A non-nil error (including native's
// support decline) is drift, resolved against BAML below.
type NativeFinalParse func(ctx context.Context, full string) (any, error)

// Marshal is the PUBLIC serializer the comparison runs on. [Legs.marshal] defaults it to
// sonic.Marshal — the exact function worker/stream.go uses to put a partial and a final on
// the wire — so a byte comparison here is a comparison of what the client would receive.
// It is a field ONLY so a test can prove a marshal failure is classified correctly; every
// production construction leaves it nil.
type Marshal func(v any) ([]byte, error)

// Legs is the pair of engines plus the public serializer. All four closures are REQUIRED:
// a nil one means the oracle cannot be established, which after the claim is terminal, so
// [Legs.Validate] is called by the caller BEFORE admission returns a claim.
type Legs struct {
	NativePrefix NativePrefixParse
	NativeFinal  NativeFinalParse
	BAMLPrefix   bamlutils.BAMLStreamParse
	BAMLFinal    bamlutils.BAMLStreamFinalParse
	// Serializer overrides sonic.Marshal. Nil in production.
	Serializer Marshal
}

// Errors the resolver returns. Every one of them is TERMINAL for a claimed stream and every
// one is BOUNDED: no prefix, value, or engine error string is interpolated into the
// sentinel itself (a wrapped cause carries the engine's own typed error, which the caller
// treats as sensitive and never logs).
var (
	// ErrLegsIncomplete: a required leg is missing. Detected pre-claim.
	ErrLegsIncomplete = errors.New("streamoracle: the per-prefix/final oracle requires a native parser, a BAML parser, a native final parser and a BAML final parser")
	// ErrBAMLPrefixUnavailable: the BAML leg could not be ESTABLISHED for this prefix
	// (cancellation, option construction, an invariant failure). It is NOT an ordinary
	// "no partial yet", which the closure reports as a value-less success.
	ErrBAMLPrefixUnavailable = errors.New("streamoracle: the BAML per-prefix oracle could not be established for this prefix")
	// ErrBAMLPrefixUnmarshalable: BAML produced a partial whose public bytes could not be
	// produced, so the comparison cannot be established.
	ErrBAMLPrefixUnmarshalable = errors.New("streamoracle: the BAML per-prefix oracle value could not be marshaled to public bytes")
	// ErrBAMLFinalUnavailable: the BAML final parse failed.
	ErrBAMLFinalUnavailable = errors.New("streamoracle: the BAML final oracle could not parse the accumulated response")
	// ErrBAMLFinalNoValue: the BAML final parse reported success with no value. There is no
	// valid final "no value" state on a claimed stream.
	ErrBAMLFinalNoValue = errors.New("streamoracle: the BAML final oracle produced no value; a claimed stream has no valid final no-value state")
	// ErrBAMLFinalUnmarshalable: the BAML final value could not be marshaled to public bytes.
	ErrBAMLFinalUnmarshalable = errors.New("streamoracle: the BAML final oracle value could not be marshaled to public bytes")
)

// Validate reports whether every required leg is present. The caller runs it BEFORE
// admission so missing wiring is a PRE-SOCKET decline rather than a post-claim discovery.
func (l Legs) Validate() error {
	if l.NativePrefix == nil || l.NativeFinal == nil || l.BAMLPrefix == nil || l.BAMLFinal == nil {
		return ErrLegsIncomplete
	}
	return nil
}

func (l Legs) marshal(v any) ([]byte, error) {
	if l.Serializer != nil {
		return l.Serializer(v)
	}
	return sonic.Marshal(v)
}

// Action is the CLOSED set of public actions one prefix tick can produce. There is no
// fourth "retry"/"reset"/"fall back" action: the stream is already claimed.
type Action uint8

const (
	// ActionNoEvent: no structured partial is released for this prefix. Raw/reasoning on a
	// /stream-with-raw tick still flow through the cadence's own raw-only branch.
	ActionNoEvent Action = iota
	// ActionEmitNative: release native's decoded standard carrier.
	ActionEmitNative
	// ActionEmitBAML: release BAML's typed value for the SAME prefix. No second provider
	// request is made — the value comes from the response already in hand.
	ActionEmitBAML
)

// PrefixOutcome is the resolution of ONE structured cadence tick.
type PrefixOutcome struct {
	Action Action
	// Value is the value to release when Action is an emit. SENSITIVE.
	Value any
	// Compare is the bounded classification recorded on the observations.
	Compare bamlutils.NativeStreamOracleCompare
	// Drift latches the sticky baml_parse_same_response attribution. It is set whenever
	// the native engine did not, on its own, produce exactly what was released:
	// substitution, suppression, a byte mismatch, or a native failure.
	Drift bool
	// Substituted / Suppressed are the two bounded shapes of Drift the composite records
	// separately, because "BAML answered instead" and "nothing was released" are different
	// operational facts.
	Substituted bool
	Suppressed  bool
}

// FinalOutcome is the resolution of the completed accumulated text.
type FinalOutcome struct {
	// Value is the final to return. Always present on a nil error. SENSITIVE.
	Value       any
	Compare     bamlutils.NativeStreamOracleCompare
	Drift       bool
	Substituted bool
}

// ResolvePrefix runs BOTH legs over the SAME prefix and resolves the closed matrix.
//
//	native                     | BAML     | action
//	---------------------------|----------|----------------------------------------------
//	value, same public bytes   | value    | emit native
//	value, different bytes     | value    | emit BAML's same-prefix value; drift
//	no-value (sentinel)        | value    | emit BAML's same-prefix value; drift
//	error (parse/decode/marshal)| value   | emit BAML's same-prefix value; drift
//	value                      | no-value | SUPPRESS the structured partial; drift
//	no-value (sentinel)        | no-value | no event; NO drift
//	error                      | no-value | no event; drift
//	any                        | error    | terminal
//
// ORDER IS DELIBERATE: the native leg runs FIRST and its failure is RECORDED rather than
// returned, because the BAML leg must still get the chance to establish the authoritative
// answer for this prefix. Terminating on native drift alone would kill a claimed stream
// whose correct answer was already available.
//
// A BAML PANIC is not caught here: it unwinds to the claimed executor's recover guard,
// which classifies it as post-claim terminal. Swallowing it as a no-value would be the one
// way a broken oracle could silently stop being an oracle.
func ResolvePrefix(ctx context.Context, legs Legs, prefix string) (PrefixOutcome, error) {
	if err := legs.Validate(); err != nil {
		return PrefixOutcome{}, err
	}

	// --- Native leg. A failure here is DRIFT, held until BAML has answered. ---
	nativeValue, nativeHas, nativeErr := legs.NativePrefix(ctx, prefix)
	var nativeBytes []byte
	if nativeErr == nil && nativeHas {
		b, merr := legs.marshal(nativeValue)
		if merr != nil {
			// A native value whose PUBLIC bytes cannot be produced could never have been
			// released to the client, so it is a native failure — not a comparison the
			// oracle failed to establish.
			nativeErr, nativeHas = merr, false
		} else {
			nativeBytes = b
		}
	}

	// --- BAML leg, on the SAME string value. ---
	bamlRes, bamlErr := legs.BAMLPrefix(ctx, prefix)
	if bamlErr != nil {
		// The oracle could not be established. Terminal even if native produced a value:
		// releasing an unverified partial is exactly what this lane exists to prevent.
		return PrefixOutcome{}, fmt.Errorf("%w: %w", ErrBAMLPrefixUnavailable, bamlErr)
	}

	if !bamlRes.HasValue {
		if nativeErr != nil {
			// Native failed and BAML has nothing to substitute: release nothing, but the
			// stream is no longer purely native.
			return PrefixOutcome{Compare: bamlutils.NativeStreamCompareNativeError, Drift: true}, nil
		}
		if nativeHas {
			// SUPPRESSION. BAML — the authority — established no partial for this prefix,
			// so native's must not reach the client.
			return PrefixOutcome{
				Compare:    bamlutils.NativeStreamCompareBAMLNoValue,
				Drift:      true,
				Suppressed: true,
			}, nil
		}
		// Both engines agree there is nothing yet: the ordinary incomplete-prefix tick.
		return PrefixOutcome{Compare: bamlutils.NativeStreamCompareNativeNoValue}, nil
	}

	// BAML established a value for this prefix. Marshal it FIRST, before any branch:
	// the authority's value is what may be released, so a value with no public form is a
	// comparison that cannot be established — terminal — rather than something to release
	// unchecked on the rows where native has nothing to compare against.
	bamlBytes, merr := legs.marshal(bamlRes.Value)
	if merr != nil {
		return PrefixOutcome{}, fmt.Errorf("%w: %w", ErrBAMLPrefixUnmarshalable, merr)
	}
	if nativeErr != nil {
		return PrefixOutcome{
			Action:      ActionEmitBAML,
			Value:       bamlRes.Value,
			Compare:     bamlutils.NativeStreamCompareNativeError,
			Drift:       true,
			Substituted: true,
		}, nil
	}
	if !nativeHas {
		return PrefixOutcome{
			Action:      ActionEmitBAML,
			Value:       bamlRes.Value,
			Compare:     bamlutils.NativeStreamCompareNativeNoValue,
			Drift:       true,
			Substituted: true,
		}, nil
	}
	if string(nativeBytes) == string(bamlBytes) {
		return PrefixOutcome{
			Action:  ActionEmitNative,
			Value:   nativeValue,
			Compare: bamlutils.NativeStreamCompareMatch,
		}, nil
	}
	return PrefixOutcome{
		Action:      ActionEmitBAML,
		Value:       bamlRes.Value,
		Compare:     bamlutils.NativeStreamCompareBytesMismatch,
		Drift:       true,
		Substituted: true,
	}, nil
}

// ResolveFinal runs BOTH legs over the COMPLETE accumulated text and resolves the final.
//
// It differs from [ResolvePrefix] in exactly one way, and the difference is the point:
// there is no valid final "BAML has nothing yet" state. A stream that reached a clean
// transport completion must produce a final, so a BAML final that errors, panics, marshals
// unrepresentably, or reports success with no value is TERMINAL — even when native
// succeeded. Native declining, failing, or drifting is not: BAML's same-response final is
// served instead.
func ResolveFinal(ctx context.Context, legs Legs, full string) (FinalOutcome, error) {
	if err := legs.Validate(); err != nil {
		return FinalOutcome{}, err
	}

	nativeValue, nativeErr := legs.NativeFinal(ctx, full)
	var nativeBytes []byte
	if nativeErr == nil {
		b, merr := legs.marshal(nativeValue)
		if merr != nil {
			nativeErr = merr
		} else {
			nativeBytes = b
		}
	}

	bamlValue, bamlErr := legs.BAMLFinal(ctx, full)
	if bamlErr != nil {
		return FinalOutcome{}, fmt.Errorf("%w: %w", ErrBAMLFinalUnavailable, bamlErr)
	}
	if bamlValue == nil {
		return FinalOutcome{}, ErrBAMLFinalNoValue
	}
	bamlBytes, merr := legs.marshal(bamlValue)
	if merr != nil {
		return FinalOutcome{}, fmt.Errorf("%w: %w", ErrBAMLFinalUnmarshalable, merr)
	}

	if nativeErr != nil {
		return FinalOutcome{
			Value:       bamlValue,
			Compare:     bamlutils.NativeStreamCompareNativeError,
			Drift:       true,
			Substituted: true,
		}, nil
	}
	if string(nativeBytes) == string(bamlBytes) {
		return FinalOutcome{Value: nativeValue, Compare: bamlutils.NativeStreamCompareMatch}, nil
	}
	return FinalOutcome{
		Value:       bamlValue,
		Compare:     bamlutils.NativeStreamCompareBytesMismatch,
		Drift:       true,
		Substituted: true,
	}, nil
}
