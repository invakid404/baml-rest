package debaml

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 8C — static Bundle parse entrypoint (the neutral SAP callback).
//
// ParseStaticBundle is the STATIC twin of [Parse]: it runs the SAME bounded
// native response parser (extract → conservative M2a fix → coerce → flatten) but
// over an ALREADY-LOWERED, validated *schema.Bundle carried by a generated static
// method's promptdescriptor.Function.Return, instead of lowering a runtime
// *bamlutils.DynamicOutputSchema. This is the load-bearing tar boundary the
// foundation scope (§7) mandates: static Bundle PARSING stays owned by
// internal/debaml so a later recursion slice can lift the recursion/alias
// declines HERE — inside this root-owned package — without touching the isolated
// nativeserve module. The isolated serve core consumes it only as a schema-neutral
// `func(ctx, raw string) ([]byte, error)` closure that captures the Bundle; it
// never re-implements Bundle semantics.
//
// The cut-line is IDENTICAL to [Parse]'s final branch, so a static return whose
// Bundle is inside the proven native-final surface parses to the SAME flattened
// canonical JSON native SAP produces for the equivalent dynamic schema, while
// every out-of-bounds shape (recursive class/alias, general union, unproven map,
// constraints, …) returns [bamlutils.ErrDeBAMLParseUnsupported] via the shared
// checkSupported gate so the caller falls back to BAML `Parse.<Method>`. A
// non-sentinel error is a CLAIMED native parse failure that propagates unchanged,
// exactly as in the dynamic path, so the native-vs-BAML static differential
// catches drift rather than masking it behind a silent fallback.
//
// It performs no I/O and opens no socket: the M1/M2a parser is a local CPU
// operation over the already-fetched assistant text (ctx is accepted for
// signature parity with [Parse] and honoured for a future cancellation point, but
// has no cancellation points today). It is SENSITIVE only in its inputs/outputs —
// raw is provider output and the returned JSON is parsed provider output; the
// caller treats both like the response body and never logs them.
func ParseStaticBundle(ctx context.Context, bundle *schema.Bundle, raw string) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx // M1 parsing is a local CPU operation; no cancellation points.

	if bundle == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil static bundle")
	}
	// Validate the Bundle is output-usable (rejects tuple/arrow/top/media) and prove
	// the whole type graph is inside the native FINAL parser surface — the SAME gate
	// SupportsNativeFinalBundle applies at admission, so a Bundle that admitted
	// cannot later decline here for a support reason, and a recursion/alias Bundle
	// (which admission already declined) also declines here as a defensive backstop.
	if err := bundle.ValidateOutput(); err != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("validate static bundle", err)
	}
	// Use the SAME final-profile support predicate admission applies
	// (SupportsNativeFinalBundle), NOT a bare checkSupported. This keeps the direct
	// static parse from drifting from the route: the recursion-aware profile admits the
	// narrow recursive-class family (self Node, mutual A<->B) exactly as admission does,
	// and declines every out-of-family recursive/union/cycle shape the marker gate would.
	// For the non-recursive 8C corpus (top-level string, StaticAnswer) this is
	// behaviour-identical — both pass the final cut-line — so the static pin is unchanged.
	if err := SupportsNativeFinalBundle(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}

	// Strip JSONish comments (string-aware) exactly as BAML does before extraction,
	// then extract the single cleanly-claimable JSON candidate (strict → markdown
	// fence → balanced span, each strict-then-M2a-fixed). No cleanly-claimable
	// candidate DECLINES (never claims) — BAML may still recover it.
	parsed, ok := extractCandidate(stripJSONComments(raw))
	if !ok {
		return bamlutils.DeBAMLParseResult{}, unsupported("no cleanly-claimable JSON candidate")
	}

	// Coerce the candidate against the Bundle target. A top-level coercion needs no
	// cleanliness accumulator (nil flags): a nullable target's own null/clean
	// decision is made inside coerceUnionSafe, exactly as the dynamic final path. A
	// FRESH pair-guard context (empty coerce/try_cast active sets) threads BAML's
	// path-local (ClassKey, value) circular-reference guard through every recursive
	// descent — no depth cap — so the admitted recursive-class family coerces safely.
	out, err := coerce(bundle, bundle.Target, parsed, nil, &coerceCtx{})
	if err != nil {
		// coerce returns ErrDeBAMLParseUnsupported where native is merely stricter
		// than BAML's lenient coercers (DECLINE → fall back); any other error is a
		// CLAIMED parse failure BAML would also hit, propagated unchanged for parity.
		return bamlutils.DeBAMLParseResult{}, err
	}
	// Static absent-optional normalizer (admitted recursive-class family ONLY): coerce
	// OMITS an absent optional from its object output, but BAML marshals the optional's
	// nil pointer as an explicit null. Insert null for the absent admitted nullable-class
	// edge, in schema field order, so an OMITTED terminal produces the SAME canonical
	// bytes as BAML's pointer→nil→null marshal ({"value":"x","next":null}) — a byte-exact
	// FinalJSON, not merely a semantically-equal decode. Non-recursive bundles (the 8C
	// corpus carries no optionals) skip this entirely, so their bytes are unchanged.
	if prof, ok := admittedRecursiveClassProfile(bundle); ok {
		out, err = normalizeRecursiveFinal(out, bundle.Target, bundle, prof)
		if err != nil {
			return bamlutils.DeBAMLParseResult{}, err
		}
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}
