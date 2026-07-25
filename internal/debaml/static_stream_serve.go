package debaml

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3b/3c (recursive-alias STREAMING) — the STATIC-STREAM admission
// predicates + the neutral static-stream parse entrypoints, the streaming twins of
// the Phase-3a/3c final-alias surface (recursive_alias_profile.go / parse_static.go).
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
//   - Phase 3c adds the `JsonValue` family to the FINAL lane only. Its streaming
//     PARSER is built and proven here (stream_alias_coerce.go, and the strict
//     gate-free per-prefix differential), but the family is DELIBERATELY NOT admitted
//     to the streaming gate — see [staticStreamAliasProfile] for the exact reason.
//
// This mirrors the final-alias lockstep — [IsProvenRecursiveAliasStaticStreamFamily] keeps
// the isolated nativeserve STREAM admission gate in EXACT step with the root-owned stream
// parser — but note the predicate it builds on. The stream fingerprint is the `JSON`-ONLY
// final predicate ([admittedRecursiveAliasProfile]) plus the stream-only admission fact (a
// static stream parse DERIVED FROM THE FINAL Bundle, with NO stream annotations). It is
// deliberately NOT the either-family FINAL classifier
// ([IsProvenServedRecursiveAliasStaticFamily]), which is strictly wider; conflating the two
// is exactly how the FINAL-served `JsonValue` would wrongly claim a stream socket.

// staticStreamAliasProfile is the SINGLE lockstep gate for the served static-stream
// recursive-alias family. It returns (profile, true) ONLY for a bundle that is BOTH the
// exact five-arm `JSON` final family AND carries the stream-only admission fact (no
// @stream.* annotations anywhere reachable). Every public static-stream predicate and
// both parse entrypoints delegate here, so they can never diverge — and because it
// returns the PROFILE (not a bool), the partial parser receives the SAME family
// classification admission used.
//
// # Why `JsonValue` is FINAL-served but STREAM-declined
//
// The static-stream gate admits by DESCRIPTOR SHAPE, pre-socket. That makes it a
// one-way door: once a native stream claims the socket there is NO route back to BAML.
// [ParseStaticStreamFinal] delegates to [ParseAliasStreamFinal] (and thence to
// ParseStaticBundle) and propagates the unsupported sentinel when EOF completion cannot
// repair the text; the generated stream seam maps a
// partial-parser error to NO EVENT and returns the final-parser error unchanged
// (adapters/common/codegen/codegen_debaml_static.go); and the orchestrator treats that
// final error as TERMINAL, explicitly forbidding a BAML fallback
// (bamlutils/buildrequest/orchestrator.go). So on the claimed stream lane, ANY
// value-scoped decline is a lost partial or a terminal error where BAML would have
// produced a result.
//
// The UNARY lane has no such hazard: native owns the single provider request and BAML
// parse-only produces the final over the SAME response (the `native_baml_parse` winner
// token), so a value-scoped decline there is a repair, not a loss. That asymmetry — not
// any property of the family itself — is why `JsonValue` is admitted to the final gate
// (SupportsNativeFinalBundle / IsProvenServedRecursiveAliasStaticFamily) and NOT here.
//
// The residual that blocks stream admission is measured, not guessed: it is the shared
// jsonish-recovery debt of native's conservative extractor (#583) — bare root scalars
// that are not strict JSON, unquoted tokens containing whitespace, prose with no
// container, multiple top-level values, triple-quoted/backtick strings, deferred escapes,
// and the greedy object-value cascade — plus this family's own negative-zero value
// decline (alias_coerce.go errNegativeZeroFloat). It is enumerated and pinned as an exact
// set by TestJsonValueStreamResidualLedger; flipping this gate requires that ledger to be
// EMPTY, not merely for the differential to exclude the offending values.
//
// The streaming PARSER itself is complete and proven for the family (stream_alias_coerce.go
// plus the strict gate-free per-prefix differential, 100% agreement over the corpus it can
// own), so this is an admission decision that a later slice flips by closing the extractor
// debt — the same "built dark, enabled later" shape Phase 3b used.
func staticStreamAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	// The direct five-arm `JSON` alias is proven byte/event-exact vs stock BAML v0.223
	// (the strict per-prefix parser differential + the SSE-replay differential), so it
	// claims the static stream socket. The fingerprint is the SAME exact structural
	// predicate the final family uses (admittedRecursiveAliasProfile — deliberately NOT
	// the either-family classifier, see the doc above), PLUS the stream-only fact: the
	// streaming parse is DERIVED FROM THE FINAL Bundle, so any reachable @stream.done /
	// @stream.not_null / @stream.with_state annotation is outside the no-annotation
	// profile and declines. Every OTHER alias — including the FINAL-served `JsonValue`,
	// a renamed/wrapped/multi alias, an extra or reordered arm — declines pre-claim.
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
// True ONLY for the five-arm `JSON` alias family. It is deliberately NARROWER than the
// FINAL predicate [IsProvenServedRecursiveAliasStaticFamily]: the FINAL-served
// `JsonValue` family is NOT stream-admitted, because a value-scoped decline is a repair
// on the unary lane but a lost partial / terminal error on a claimed stream. See
// [staticStreamAliasProfile] for the full reasoning and the residual that blocks it.
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
// Returns nil (supported) ONLY for the stream-admitted `JSON` alias family; every other
// bundle — including the FINAL-served `JsonValue` — declines and BAML serves it exactly
// as today.
func SupportsNativeStaticStreamBundle(b *schema.Bundle) error {
	if b == nil {
		return unsupported("nil bundle")
	}
	// Every stream ends in a FINAL parse, so the terminal Parse must be supported too.
	// The final surface is the proven Phase-3a/3c alias families
	// (SupportsNativeFinalBundle), so this is a genuine precondition, not a formality. It
	// is NOT sufficient on its own: the stream gate below is strictly narrower.
	if err := SupportsNativeFinalBundle(b); err != nil {
		return err
	}
	if _, ok := staticStreamAliasProfile(b); !ok {
		return unsupported("static stream: not the stream-admitted recursive-alias family (#583 pre-claim decline)")
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
// byte-identical to the unary final for the same completed text, so for the
// stream-admitted alias family it REUSES the proven Phase-3a final alias coercer (plus
// BAML's EOF object-completion) via [ParseAliasStreamFinal]. A claimed parse FAILURE
// propagates unchanged for parity (never a silent BAML fallback on the claimed lane).
//
// This entry is exactly the support GATE plus a delegation to the gate-free body, mirroring
// the partial pair ([ParseStaticStreamPartial] / [ParseAliasStreamPartial]).
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
	return ParseAliasStreamFinal(ctx, bundle, accumulated)
}

// ParseAliasStreamFinal is the GATE-FREE stream-FINAL parse the per-prefix differential
// and the stream residual ledger drive — the FINAL twin of [ParseAliasStreamPartial], and
// the body [ParseStaticStreamFinal] delegates to once its support gate has passed.
//
// It is gate-free BY DESIGN so a differential can measure what the alias parser can own
// for a family INDEPENDENTLY of whether that family is admitted to the streaming gate.
// That separation is load-bearing for Phase 3c: `JsonValue` is FINAL-served but
// STREAM-declined, and the evidence for flipping its stream gate later is exactly a
// measurement taken through this entry (see TestJsonValueStreamResidualLedger).
//
// The FINAL of a stream is byte-identical to the unary final for the same completed text,
// so it REUSES the proven final alias coercer via [ParseStaticBundle]; when the accumulated
// text is complete-but-UNCLOSED it applies BAML's EOF object-completion ITSELF before
// re-parsing (ParseStaticBundle declines the unclosed candidate). A claimed parse FAILURE
// propagates unchanged for parity.
func ParseAliasStreamFinal(ctx context.Context, bundle *schema.Bundle, accumulated string) (bamlutils.DeBAMLParseResult, error) {
	if bundle == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil static stream bundle")
	}
	// FINAL == the non-stream Parse for the completed accumulated text (a proven
	// alias family), reproduced by ParseStaticBundle.
	res, err := ParseStaticBundle(ctx, bundle, accumulated)
	if err == nil {
		return res, nil
	}
	// EOF completion: an ordinary completed stream ([DONE]/finish_reason:stop) whose
	// accumulated model text is a complete-but-UNCLOSED structure is a SUCCESS for BAML (its
	// stream final closes the structure at EOF). But the native non-stream parse
	// (ParseStaticBundle, above) DECLINES an unclosed candidate — so this reproduces BAML's
	// EOF close HERE via completeUnclosedFinal (the SAME pass the dynamic native-only final
	// uses, ParseNativeStreamFinal), then re-parses the equivalent CLOSED text through
	// ParseStaticBundle. It does NOT change the streaming PARTIAL cadence.
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
