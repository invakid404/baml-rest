package debaml

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3b (recursive-alias STREAMING) — the STATIC-STREAM admission
// predicates + the neutral static-stream parse entrypoints, the streaming twins of
// the Phase-3a final-alias surface (recursive_alias_profile.go / parse_static.go).
//
// TWO INTERNAL PHASES, BOTH LANDED IN THIS PR:
//
//   - Phase A built the whole transport seam DARK. The single lockstep gate is
//     [staticStreamAliasProfile]; the three public predicates and both parse
//     entrypoints delegate to it, so they admit/decline in lockstep.
//
//   - Phase B (ENABLED here) flipped [staticStreamAliasProfile] to the REAL structural
//     fingerprint + the stream-only fact and gave [ParseStaticStreamPartial] the
//     completion-state-aware coerceStreamAlias, so the exact five-arm JSON alias now
//     CLAIMS a socket — proven byte/event-exact vs stock BAML v0.223 by the strict
//     per-prefix + SSE-replay differentials. Every OTHER static stream still DECLINES
//     pre-claim, so BAML serves it exactly as today.
//
// This mirrors the final-alias lockstep: [IsProvenRecursiveAliasStaticFamily] keeps
// the isolated nativeserve admission gate in EXACT step with the root-owned parser.
// The stream predicates add the stream-only admission fact (a static stream parse
// DERIVED FROM THE FINAL Bundle, with NO stream annotations) on top of the shared
// final fingerprint, so the served static-stream family can never drift from either
// the final family or the parser.

// staticStreamAliasProfile is the SINGLE lockstep gate for the served static-stream
// recursive-alias family. It returns (profile, true) ONLY for a bundle that is BOTH
// the exact final five-arm JSON alias family AND carries the stream-only admission
// fact (no @stream.* annotations anywhere reachable). Every public static-stream
// predicate and both parse entrypoints delegate here, so they can never diverge.
//
// This gate is ENABLED (Phase B): it returns (profile, true) for the admitted five-arm
// JSON alias — guarded behind the proven Phase-B partial parser + the strict per-prefix
// / SSE-replay differentials — which is the ENTIRE admission enablement; the transport
// plumbing does not change. Every static stream OUTSIDE the admitted family stays a #583
// pre-claim decline that BAML serves exactly as today.
func staticStreamAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	// PHASE B (ENABLED): the direct five-arm JSON alias is proven byte/event-exact vs stock
	// BAML v0.223 (the strict per-prefix parser differential + the SSE-replay differential),
	// so it now claims the static stream socket. The fingerprint is the SAME exact structural
	// predicate the final family uses (admittedRecursiveAliasProfile), PLUS the stream-only
	// fact: the streaming parse is DERIVED FROM THE FINAL Bundle, so any reachable
	// @stream.done / @stream.not_null / @stream.with_state annotation is outside the
	// no-annotation profile and declines. Every OTHER alias (the wider JsonValue, a
	// renamed/wrapped/multi alias, a float/null arm) still declines pre-claim.
	if b == nil {
		return recAliasProfile{}, false
	}
	prof, ok := admittedRecursiveAliasProfile(b)
	if !ok {
		return recAliasProfile{}, false
	}
	if checkNoStreamAnnotations(b) != nil {
		return recAliasProfile{}, false
	}
	return prof, true
}

// IsProvenRecursiveAliasStaticStreamFamily is the exported STREAM lockstep predicate
// the isolated nativeserve stream admission gate uses so the served stream fingerprint
// and the root-owned stream parser can NEVER diverge. It is the STREAM twin of
// [IsProvenRecursiveAliasStaticFamily]; it delegates to [staticStreamAliasProfile] so
// the codegen emit gate, the admission lowered-Return gate, the Parse*Stream*
// defensive gates, and the fixture/oracle preconditions all admit the identical shape.
//
// True ONLY for the admitted five-arm JSON alias family; false (decline) for every
// other bundle.
func IsProvenRecursiveAliasStaticStreamFamily(b *schema.Bundle) bool {
	_, ok := staticStreamAliasProfile(b)
	return ok
}

// SupportsNativeStaticStreamBundle is the stream-support half of static-stream
// admission: it proves the native static STREAM parser can own EVERY partial for the
// bundle. It requires BOTH final support (for the terminal Parse — every stream ends
// in a final) AND the static-stream alias family (for every partial). It is the
// STREAM twin of [SupportsNativeFinalBundle] and is DELIBERATELY NOT
// [SupportsNativeStreamBundle], which is the DYNAMIC contract (the pinned 289/157/132
// universe) that must stay byte-for-byte unchanged.
//
// Returns nil (supported) ONLY for the admitted five-arm JSON alias family; every other
// bundle declines and BAML serves it exactly as today.
func SupportsNativeStaticStreamBundle(b *schema.Bundle) error {
	if b == nil {
		return unsupported("nil bundle")
	}
	// Every stream ends in a FINAL parse, so the terminal Parse must be supported too.
	// The final surface is the proven Phase-3a alias family (SupportsNativeFinalBundle),
	// so this is a genuine precondition, not a formality.
	if err := SupportsNativeFinalBundle(b); err != nil {
		return err
	}
	if _, ok := staticStreamAliasProfile(b); !ok {
		return unsupported("static stream: not the proven recursive-alias family (#583 pre-claim decline)")
	}
	return nil
}

// ParseStaticStreamPartial is the neutral static-stream PARTIAL parse entrypoint the
// generated static stream installer wires as StreamConfig.NativeParseStream (via an
// injected Bundle-based closure — the generated adapter never imports internal/debaml).
// It is the streaming twin of [ParseStaticBundle]: for an admitted static-stream alias
// bundle it derives BAML's streaming alias profile internally, obtains a
// completion-bearing JSONish value from the accumulated text, runs coerceStreamAlias +
// BAML semantic-streaming, and returns the SORTED-public partial bytes; it NEVER calls
// BAML (I6).
//
// Return contract:
//   - (json, nil): native produced a partial for accumulated → the caller emits it.
//   - (zero, ErrDeBAMLParseUnsupported): either the defensive support gate declined OR
//     there is no parseable partial for this prefix yet. The value returned is the NON-NIL
//     unsupported(...) sentinel (which wraps ErrDeBAMLParseUnsupported), NOT (nil, nil); the
//     installer maps that sentinel to a benign no-emit. It is NEVER a BAML fallback on the
//     claimed lane.
func ParseStaticStreamPartial(ctx context.Context, bundle *schema.Bundle, accumulated string) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx // partial parsing is a local CPU operation; no cancellation points.
	if bundle == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil static stream bundle")
	}
	// Defensive support gate (lockstep with admission): a bundle that admitted cannot
	// decline here for a support reason; a non-admitted bundle declines as the backstop.
	if err := SupportsNativeStaticStreamBundle(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	// coerceStreamAlias over a completion-bearing value + BAML semantic-streaming, proven
	// byte-exact vs stock BAML v0.223's ParseStream by the strict per-prefix differential.
	out, emit, err := ParseAliasStreamPartial(bundle, accumulated)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if !emit {
		// No parseable partial yet for this prefix — a benign no-emit (never a BAML fallback).
		return bamlutils.DeBAMLParseResult{}, unsupported("static stream: no partial for this prefix yet")
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}

// ParseStaticStreamFinal is the neutral static-stream FINAL parse entrypoint the
// generated installer wires as StreamConfig.NativeParseFinal. The FINAL of a stream is
// byte-identical to the unary final for the same completed text, so for the admitted
// five-arm JSON alias it REUSES the proven Phase-3a final alias coercer via
// [ParseStaticBundle] and produces the final natively; when the accumulated text is
// complete-but-UNCLOSED, ParseStaticStreamFinal applies BAML's EOF object-completion ITSELF
// before re-parsing (see the EOF-completion block below — ParseStaticBundle declines the
// unclosed candidate). A claimed parse FAILURE propagates unchanged for parity (never a
// silent BAML fallback on the claimed lane).
//
// The support gate below (SupportsNativeStaticStreamBundle) can trip ONLY for a bundle that
// was NEVER admitted/claimed — that returns the fallback sentinel and BAML produces the
// final. On the CLAIMED lane the gate is UNREACHABLE: admission already proved support in
// lockstep, so a claimed stream never falls back to BAML for its final. The two are not in
// tension — the sentinel is a pre-claim outcome, the no-fallback invariant is a claimed-lane
// one.
func ParseStaticStreamFinal(ctx context.Context, bundle *schema.Bundle, accumulated string) (bamlutils.DeBAMLParseResult, error) {
	if bundle == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil static stream bundle")
	}
	if err := SupportsNativeStaticStreamBundle(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	// FINAL == the non-stream Parse for the completed accumulated text (proven Phase-3a
	// family), reproduced by ParseStaticBundle.
	res, err := ParseStaticBundle(ctx, bundle, accumulated)
	if err == nil {
		return res, nil
	}
	// EOF completion: an ordinary completed stream ([DONE]/finish_reason:stop) whose
	// accumulated model text is a complete-but-UNCLOSED structure is a SUCCESS for BAML (its
	// stream final closes the structure at EOF). But the native non-stream parse
	// (ParseStaticBundle, above) DECLINES an unclosed candidate — so ParseStaticStreamFinal
	// itself reproduces BAML's EOF close HERE via completeUnclosedFinal (the SAME pass the
	// dynamic native-only final uses, ParseNativeStreamFinal), then re-parses the equivalent
	// CLOSED text through ParseStaticBundle. This is scoped to admitted alias bundles (the
	// support gate above ran); it does NOT change the streaming PARTIAL cadence.
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		if completed, changed := completeUnclosedFinal(accumulated); changed {
			if res2, err2 := ParseStaticBundle(ctx, bundle, completed); err2 == nil {
				return res2, nil
			}
		}
	}
	// A claimed parse failure (never a BAML fallback): propagate for parity.
	return res, err
}
