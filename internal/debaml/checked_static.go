package debaml

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2b-3 — the PRODUCTION coercion-state → `Checked[T]` carrier mapper,
// and the ONE fingerprint native now SERVES.
//
// # Why this file exists at all
//
// Slice 7.2a-2's coercion-state collector (constraint_state_collect_test.go) is
// TEST-ONLY and cannot become the serving implementation: it is guarded structurally
// against ever appearing in production code
// (TestConstraintStateCollectorIsTestOnly), and its job is to WITNESS what a
// coercion decided, not to produce a value a caller receives. This file is the
// minimum PRODUCTION seam that turns the same coercion decisions into the two
// public outcomes stock v0.223.0 can produce for the admitted fingerprint: a
// [bamlutils.Checked] carrier, or an assertion error. The collector stays beside it
// as an INDEPENDENT oracle.
//
// # What it admits, and what still admits nothing
//
// 7.2b-2 built everything below behind a non-admitting seam. 7.2b-3 opens it — for
// EXACTLY the fingerprint [staticCheckedProfileOf] classifies, on EXACTLY the static
// unary /call route, and for nothing else. Admission still needs BOTH halves:
// [staticCheckedAdmitsConstraints] (now true — the cutover) AND a route CAPABILITY
// only the static unary /call route constructs. Every constraint-bearing bundle
// OUTSIDE the fingerprint still declines through checkSupported / checkSupportedFields
// / checkSupportedType / [SupportsNativeFinalBundle] / [Parse] / [ParseStaticBundle]
// and through nativeserve's admission gate, and every bundle INSIDE it still declines
// on every route but one.
//
// Splitting it that way is what keeps the cutover inside the scope's boundary
// ("static unary /call final parsing only", direct parse endpoints left declined),
// and it is why the boundary is a MEASURED fact rather than a comment: the same
// production gates that serve the companion rows on the /call route are driven over
// the direct routes and over one-property siblings, and keep declining.
//
// SLICE 7.2c-3 widened WHAT is admitted and nothing else. The predicate went from
// `this > I` to `this OP I` over the six direct comparisons — one edit, in
// [staticCheckedManifestTokens] — so the companion set went from 4 rows to 24
// (6 operators x check pass/fail x assert pass/fail). The route set, the structural
// rules, the one-constraint rule and the one-assert renderer are all unchanged.
//
// # ONE fingerprint, shared by every named schema gate
//
// [staticCheckedProfileOf] is the ONLY place in the repository that decides a
// constraint-bearing bundle may be claimed, and EVERY named schema gate invokes it:
// checkSupported, checkSupportedFields and checkSupportedType (through
// [staticCheckedAdmittedConstraintNode]), [SupportsNativeFinalBundle], and — through
// [IsAdmittedStaticCheckedFamily] — nativeserve's admission return-shape gate. They give
// the SAME answer for the SAME schema, in both directions, which
// TestStaticCheckedGatesShareOneFingerprint asserts symmetrically over the 24 admitted
// rows plus every one-property sibling, and TestStaticCheckedGateAgreementIsProvenToBite
// proves by mutating each gate in turn, both ways. Since Slice 7.2c-3 the manifest is
// pinned from BOTH sides as well: widening it to a seventh operator form must produce a
// disagreement (TestStaticCheckedSeventhFormWideningIsProvenToBite) and narrowing it back
// must lose exactly the rows of the operators it dropped
// (TestStaticCheckedManifestNarrowingIsProvenToBite).
//
// There is no codegen-side twin, and none can exist: adapters/common does not depend on
// the root module and codegen's Introspection carries no static descriptors, so codegen
// cannot see a return SCHEMA at emission time. It emits the seam unconditionally and
// admission decides — see nativeserve/admission/static.go and
// adapters/common/codegen.TestCodegenMakesNoStaticReturnShapeClaim.
//
// Keeping the DYNAMIC and STREAM lanes on BAML is therefore a ROUTE decision rather than
// a shape one, made at the route by [staticCheckedRouteBoundary]. That split is what lets
// the shape gates agree with each other while the lanes the scope leaves declined still
// refuse the fingerprint — and it declines by naming the route, so a fallback is
// attributable to the boundary rather than to the shape.
//
// # The six requirements (scope §"Coercion-state to output mapper")
//
//  1. the PRODUCTION evaluator: [EvaluateConstraint] in this package, NEVER
//     internal/bamlprofile's test façade (the #649 seam);
//  2. a STATICALLY PROVEN expression profile only, with the source expression text
//     preserved VERBATIM in [bamlutils.Check.Expression];
//  3. each true/false `@check` mapped into the carrier with stock's
//     `succeeded`/`failed` status;
//  4. a false `@assert` rendered as the STOCK ERROR in the correct nested coercion
//     scope, emitting NO value;
//  5. canonical class field order and normal JSON behaviour preserved WITHOUT
//     reparsing the raw assistant text; and
//  6. the existing unsupported sentinel returned BEFORE admission whenever
//     extraction, expression support, or byte-parity proof is missing.
//
// The mapper consumes the native canonical coercion output — the same [coerce] call
// [ParseStaticBundle] makes. It never serializes native JSON and asks the CFFI to
// decode it as a validation step: the stock byte authority is captured
// independently (internal/debaml/checkedwire) and compared against, never consulted
// at runtime.

// staticCheckedAdmitsConstraints is THE cutover switch of Slice 7.2b-3, and it is the
// ONLY one at this layer.
//
// While it was false (7.2b-2), [staticCheckedAdmittedFamily] answered no for EVERY
// bundle, so every named schema gate fell through to its ordinary constraint decline and
// [staticCheckedParse] reported "not my case". It is now true, which is deliberately the whole
// cutover here: there is no second, more permissive switch to forget, which is the
// failure mode the scope calls out ("do not lift one blanket 'has constraints'
// rejection and leave another gate more permissive").
//
// It is a compile-time CONSTANT rather than a variable so no production code path can
// widen the claim at runtime; TestStaticCheckedSeamIsTheOnlySwitch pins that
// structurally. Flipping it back to false restores 7.2b-2's total decline without any
// other edit — which is what makes it the rollback as well as the cutover.
const staticCheckedAdmitsConstraints = true

// staticCheckedClaim is the CAPABILITY to claim the checked-static fingerprint, and it
// is the second half of the cutover.
//
// The flip of [staticCheckedAdmitsConstraints] alone admits NOTHING: a route must also
// hand in a granting capability, and only the static unary /call route may construct
// one. That is deliberate and is what keeps the cutover inside the boundary the scope
// draws — "static unary /call final parsing only", with direct parse endpoints not
// behind the static unary seam left DECLINED.
//
// It matters because [ParseStaticBundle] is not reached from one place. Today it is
// called by the static unary serve seam (the admitted route), by its shadow
// comparator, by the stream-final completion lane, and by root [Parse] for an ordinary
// static-descriptor parse — the direct-parse endpoint. A cutover that lived only in a
// package-level constant would admit the fingerprint on ALL of them at once.
//
// The zero value is the DIRECT route: no capability, never admits. That is what every
// exported entry point passes today, so a route added without thinking about this
// declines by construction rather than by remembering to.
type staticCheckedClaim struct {
	// staticUnaryCall is granted only by the static unary /call serve route.
	staticUnaryCall bool
}

// staticCheckedDirect is the capability every DIRECT route carries: none.
//
// Root [Parse]'s static-descriptor lane, the static-stream lanes, the shadow
// comparator and any future caller of [ParseStaticBundle] get this, so the checked
// fingerprint stays declined on all of them no matter what the seam constant says.
func staticCheckedDirect() staticCheckedClaim { return staticCheckedClaim{} }

// staticCheckedGrantStaticUnaryCall is the ONE granting constructor: the capability the
// static unary /call serve route carries.
//
// It reads the cutover constant directly and nothing else. 7.2b-2 additionally
// consulted a test-only atomic seam so the closed state could be shown to be the only
// thing holding the fingerprint back; that stand-in is GONE with the cutover, because a
// mutable test switch that can only re-open an already-open gate proves nothing and is
// a way for a test to widen production's claim. The attribution it used to provide is
// now carried by the one-property siblings, which decline against the same production
// gates the admitted rows pass — four of them under 7.2b-3, and since Slice 7.2c-3 the
// whole six-operator manifest.
func staticCheckedGrantStaticUnaryCall() staticCheckedClaim {
	return staticCheckedClaim{staticUnaryCall: staticCheckedAdmitsConstraints}
}

// staticCheckedAdmittedFamily is THE admission answer for a whole bundle, and the ONLY
// place the shape side of the cutover is decided.
//
// It is the fingerprint AND the cutover constant, together. Both halves matter:
//
//   - without the fingerprint it would admit any constraint, which is the whole point of
//     the slice;
//   - without the CONSTANT, flipping [staticCheckedAdmitsConstraints] back to false would
//     leave the SHAPE gates still admitting (they consult this, not the route capability)
//     while the parse routes declined — a gate disagreement in the rollback state, and a
//     silent falsification of "flipping it back restores the total decline". That is not
//     hypothetical: it was measured before this function existed.
//
// Every named schema gate reaches the fingerprint through here — checkSupported /
// checkSupportedFields / checkSupportedType via [staticCheckedAdmittedConstraintNode],
// nativeserve's admission return-shape gate via [IsAdmittedStaticCheckedFamily] — so the
// cutover is one constant with exactly two production readers: this, and the route
// capability [staticCheckedGrantStaticUnaryCall].
func staticCheckedAdmittedFamily(b *schema.Bundle) bool {
	if !staticCheckedAdmitsConstraints {
		return false
	}
	_, ok := staticCheckedProfileOf(b)
	return ok
}

// staticCheckedAdmittedConstraintNode reports whether t is THE constrained type node of
// the one admitted fingerprint carried by b.
//
// It is the single-node form of [staticCheckedProfileOf], and it is what lets the three
// GENERIC shape gates (checkSupported / checkSupportedFields / checkSupportedType) give
// the same answer as [SupportsNativeFinalBundle] for the same schema instead of refusing
// every constraint outright. The bundle is required, not just the node: `confidence int
// @check(positive, {{ this > 0 }})` is admissible ONLY inside the exact two-field class
// the byte captures were taken from, so a node that looks identical in a DIFFERENT bundle
// is not this node.
//
// The node is matched by VALUE (kind, primitive, dynamic/stream metadata and the single
// constraint's level, label and expression) rather than by index: checkSupportedType is
// handed a type, not a path, and it is also reached from coerce's single-non-null-optional
// re-check — so "is this the fingerprint's constrained node" has to be answerable from the
// node itself.
func staticCheckedAdmittedConstraintNode(b *schema.Bundle, t schema.Type) bool {
	if !staticCheckedAdmittedFamily(b) {
		return false
	}
	// staticCheckedProfileOf has already proven the bundle has exactly one class with
	// exactly two fields and one direct constraint on the second.
	node := b.Classes[0].Fields[1].Type
	if len(t.Meta.Constraints) != 1 {
		return false
	}
	got, want := t.Meta.Constraints[0], node.Meta.Constraints[0]
	if got.Level != want.Level || got.Expression != want.Expression {
		return false
	}
	if (got.Label == nil) != (want.Label == nil) {
		return false
	}
	if got.Label != nil && *got.Label != *want.Label {
		return false
	}
	return t.Kind == schema.TypePrimitive && t.Primitive == schema.PrimitiveInt &&
		!t.Dynamic && t.Meta.Stream.IsZero()
}

// staticCheckedRouteBoundary is the DECLINE a route that may not claim the checked-static
// fingerprint must return for it, and nil for every other bundle.
//
// It exists because the shape gates now AGREE with each other about the fingerprint (the
// single-fingerprint contract), which means "this schema is claimable" no longer implies
// "this route may claim it". The scope admits the fingerprint on the static unary /call
// route ONLY, so the DYNAMIC final lane, the DYNAMIC/static STREAM lanes and every direct
// parse endpoint have to say so themselves — as a route decision, at the route.
//
// `route` names the caller and is a fixed, secret-free token: it goes into the decline
// message so a fallback is attributable to the route rather than to the shape.
func staticCheckedRouteBoundary(b *schema.Bundle, route string) error {
	// [staticCheckedAdmittedFamily], not the bare fingerprint: this boundary exists only
	// because the SHAPE gates admit, and they admit only while the cutover is on. Reading
	// the same choke point makes the rollback EXACT — with the constant off this is a
	// no-op and every lane falls through to the ordinary constraint decline it had in
	// 7.2b-2, message included, instead of declining here for a route reason.
	if !staticCheckedAdmittedFamily(b) {
		return nil
	}
	return unsupported("checked-static fingerprint on " + route + ", which may not claim it")
}

// IsAdmittedStaticCheckedFamily reports whether b is the ONE checked-static fingerprint
// the cutover admits — the exported form of the SAME decision
// [SupportsNativeFinalBundle] makes.
//
// It exists for nativeserve/admission's return-shape gate — the SOLE pre-claim
// return-shape decision, since codegen makes none (it is schema-blind and emits the seam
// unconditionally; see nativeserve/admission/static.go and
// adapters/common/codegen.TestCodegenMakesNoStaticReturnShapeClaim). That gate lives in
// an isolated, out-of-go.work module, and spelling the fingerprint there is precisely how
// two gates drift apart — so the predicate lives ONCE here and is delegated to, exactly
// as [IsProvenServedRecursiveAliasStaticFamily] and [IsProvenRecursiveStaticFamily]
// already are for their families.
//
// It is deliberately NOT a general "constraints supported" switch: it answers only
// "is this one of the two concrete generated fixture return types the byte differential
// covers, carrying one of the manifest's evidence-gated predicates, with the cutover
// on".
func IsAdmittedStaticCheckedFamily(b *schema.Bundle) bool {
	return staticCheckedAdmittedFamily(b)
}

// admits reports whether this capability may claim the checked-static fingerprint.
func (c staticCheckedClaim) admits() bool { return c.staticUnaryCall }

// staticCheckedAnswerField / staticCheckedConfidenceField are the two canonical
// field names of the admitted fingerprint, in declaration order. They are part of
// the fingerprint rather than incidental: the scope admits two CONCRETE generated
// fixture return types, not a general "class with one constrained int" feature.
const (
	staticCheckedAnswerField     = "answer"
	staticCheckedConfidenceField = "confidence"
)

// staticCheckedCheckClass / staticCheckedAssertClass are the names of those two
// concrete generated fixture return types, as the staticserve fixture project declares
// them.
//
// Pinning the NAME is not decoration. The scope admits "two concrete generated fixture
// return types, not a generic constraint feature", and the byte proof is per-fixture:
// the stock captures in internal/debaml/checkedwire were taken from these exact
// declarations. A different class with the same two fields has no capture behind it, so
// it is outside what has been measured and must decline.
//
// The pairing with the LEVEL is part of it too: a `@check` on the assert fixture's
// class (or the reverse) is a shape neither capture describes.
const (
	staticCheckedCheckClass  = "StaticCheckedAnswer"
	staticCheckedAssertClass = "StaticAssertAnswer"
)

// staticCheckedManifestTokens is the PRODUCTION allowed-operator manifest: the
// operators the static expression profile will classify, and therefore the only
// ones any schema gate can admit.
//
// SLICE 7.2c-3 — THE CUTOVER. It is the SIX direct comparisons, and it is the
// first schema-admission widening since #668. Until this slice it was `>` alone;
// widening it here is the ONLY semantic edit of the cutover, and every one of the
// five named schema surfaces follows through delegation rather than through a
// second grammar.
//
// EVERY TOKEN IS EVIDENCE-GATED, PER OPERATOR. The capability and the manifest
// stay separate for the reason Slice 7.2c-2 wrote them apart:
//
//   - the CAPABILITY is a statement about the EVALUATOR ("native can decide this
//     form exactly, for every i64"), proven by arithmetic in
//     constraint_direct_i64.go — it recognised all six from the day it landed and
//     that admitted nothing;
//   - the MANIFEST is a statement about STOCK ("native reproduces BAML's bytes for
//     this schema"), provable only by that operator's own CFFI wire AND error
//     capture on these two name-pinned classes, plus a live pre-socket admission
//     and one-socket serve proof.
//
// Each of the six tokens below carries the second kind of evidence, and it is
// paired to it by NAME rather than by assertion: [directCompareOp.ID] is the same
// key internal/debaml/predicatewire files its per-operator stock captures under
// (`gt`, `ge`, `lt`, `le`, `eq`, `ne`), so a token and the capture that authorises
// it can be joined mechanically. TestStaticCheckedManifestIsEvidenceGated does
// exactly that join — over the untagged copies of the captures — and
// TestPredicateWireManifestIsBackedByCaptures repeats it in the integration lane
// against the tagged originals. A token whose capture set is missing or
// disagreeing therefore fails a test rather than being admitted by a grammar that
// happens to parse it: the operator stays DECLINED, which is the shape the 7.2c
// scope requires ("a missing/absent/disagreeing row leaves that operator DECLINED
// — it must NOT be masked by a broad grammar").
//
// It stays a FUNCTION returning a fresh slice, not a package variable, so nothing —
// production or test — can widen the claim of a running binary by assigning to it.
// The mutation proofs build their own manifests and drive a TWIN classifier:
// TestStaticCheckedManifestMutationBites now widens to a SEVENTH form and requires
// the decline assertions to fail, and TestStaticCheckedManifestNarrowingBites
// removes one token at a time and requires the SERVED assertions to fail — so the
// manifest is pinned from both sides.
func staticCheckedManifestTokens() []string {
	return []string{">", ">=", "<", "<=", "==", "!="}
}

// staticCheckedManifest is the production manifest resolved against the exact-i64
// capability table.
//
// Resolving it through [directCompareManifest] rather than restating the operator
// records is what keeps the classifier and the evaluator from drifting: a token
// here that the capability cannot decide yields NO manifest, so the profile
// admits nothing at all rather than admitting a form the evaluator would refuse
// after the claim. That is the same failure direction Slice 7.2c-2 exists to
// close, expressed as a construction rule instead of a comment.
func staticCheckedManifest() []directCompareOp {
	ops, ok := directCompareManifest(staticCheckedManifestTokens())
	if !ok {
		// FAIL CLOSED. An unresolvable manifest is a programming error, and the
		// safe answer to one is an empty classifier: every expression declines and
		// nothing is served, rather than a partial manifest quietly claiming a
		// subset nobody chose.
		return nil
	}
	return ops
}

// staticCheckedProfile is the classification of a bundle that matches the one
// admitted fingerprint:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(<label>, {{ this OP <int> }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(<label?>, {{ this OP <int> }}) }
//
// where OP is one of the six direct comparisons in [staticCheckedManifestTokens].
//
// Exactly two fields in that order, no aliases and no other metadata, `confidence`
// required / direct / non-nullable `int`, and EXACTLY ONE direct constraint on it —
// one `@check` or one `@assert`, never both and never more.
type staticCheckedProfile struct {
	// className is the single class the bundle declares and the target names.
	className string
	// level is `check` or `assert`; the two produce different public outcomes.
	level schema.ConstraintLevel
	// label is the constraint label. A `@check` requires a non-empty one (BAML's
	// grammar does too, and it is the carrier's map key); a `@assert` may omit it,
	// which changes only the rendered cause text.
	label string
	// expression is the predicate text stock puts in [bamlutils.Check.Expression] and
	// quotes in the rendered assertion cause.
	//
	// It is the source text with the `{{ }}` block's PADDING removed and NOTHING else
	// changed — never re-spaced inside, never re-serialized. That one normalisation is
	// measured rather than assumed: a generated static method's descriptor carries the
	// attribute text with the delimiters stripped and the padding KEPT (the staticserve
	// fixture's descriptor for `{{ this > 0 }}` is literally " this > 0 "), while stock
	// emits `this > 0`. internal/debaml/checkedwire's ExprPadNone / ExprPadOne /
	// ExprPadTwo rows drive all three paddings through the real CFFI and pin that stock
	// emits the SAME unpadded string for each. See [staticCheckedCanonicalExpression]
	// for which paddings are admitted.
	expression string
}

// staticCheckedProfileOf classifies a bundle against the one admitted fingerprint,
// reporting false for everything else.
//
// It is deliberately a WHOLE-BUNDLE fingerprint rather than a per-node predicate:
// the scope admits two concrete shapes, so a bundle carrying an extra class, an
// enum, a third field, a differently-ordered pair, a second constraint, an alias
// anywhere, a description, any streaming metadata, or a recursive marker is NOT this
// family and must keep declining. Every rejection below is one of those.
func staticCheckedProfileOf(b *schema.Bundle) (staticCheckedProfile, bool) {
	if b == nil {
		return staticCheckedProfile{}, false
	}
	// Recursion markers: not this family, and they would change the coercion the
	// mapper consumes.
	//
	// PRESENCE, not emptiness, for every collection the fingerprint requires to be
	// ABSENT — the same rule [staticCheckedCanonicalType] applies to Type.Items, and for
	// the same reason: a slice's zero value is nil, so a non-nil empty one is a
	// populated field that ordinary lowering never produces. Every one of these is nil
	// on the real descriptor-lowered path (TestStaticCheckedLoweringProducesNilAbsences
	// measures it), so requiring nil costs no admitted shape.
	if b.RecursiveClasses != nil || b.StructuralRecursiveAliases != nil {
		return staticCheckedProfile{}, false
	}
	// EXACTLY one class and no enums. A second definition is a different schema even
	// if the root class matches.
	if len(b.Classes) != 1 || b.Enums != nil {
		return staticCheckedProfile{}, false
	}
	cls := &b.Classes[0]
	// The target is the class itself, non-streaming, and carries NO metadata of its
	// own — a target-level constraint is the #664 over-claim and stays declined — and
	// NO payload its kind does not select ([staticCheckedCanonicalType]).
	//
	// The target's Meta is tested FIELD BY FIELD rather than through
	// [schema.TypeMeta.IsZero], and that is the point rather than verbosity: IsZero's
	// body is `len(m.Constraints) == 0 && m.Stream.IsZero()`, a LENGTH test, so a
	// non-nil EMPTY `Constraints` slice on the target satisfied it and the whole
	// fingerprint admitted a populated metadata payload. A helper that hides a length
	// check behind a name that reads like "is the zero value" is exactly how the
	// nil-versus-empty class kept reappearing, so the absence question is asked here
	// where it can be seen. (`Stream` is three BOOLs, so its IsZero carries no
	// collection and is safe to delegate to.)
	if !staticCheckedCanonicalType(b.Target, schema.TypeClass) ||
		b.Target.Name != cls.Name.Name || b.Target.Mode != schema.NonStreaming ||
		b.Target.Meta.Constraints != nil || !b.Target.Meta.Stream.IsZero() {
		return staticCheckedProfile{}, false
	}
	if cls.Mode != schema.NonStreaming || cls.Name.Alias != nil || cls.Description != nil ||
		cls.Constraints != nil || !cls.Stream.IsZero() || len(cls.Fields) != 2 {
		return staticCheckedProfile{}, false
	}
	answer, confidence := &cls.Fields[0], &cls.Fields[1]
	if !staticCheckedPlainField(answer, staticCheckedAnswerField, schema.PrimitiveString) ||
		answer.Type.Meta.Constraints != nil {
		return staticCheckedProfile{}, false
	}
	if !staticCheckedPlainField(confidence, staticCheckedConfidenceField, schema.PrimitiveInt) {
		return staticCheckedProfile{}, false
	}
	// EXACTLY ONE direct constraint. Two @checks under one label are the duplicate
	// asymmetry stock folds last-write-wins; a check PLUS an assert is a third
	// outcome shape; neither is proven.
	if len(confidence.Type.Meta.Constraints) != 1 {
		return staticCheckedProfile{}, false
	}
	c := confidence.Type.Meta.Constraints[0]
	label := ""
	if c.Label != nil {
		label = *c.Label
	}
	switch c.Level {
	case schema.ConstraintCheck:
		// FIXTURE IDENTITY: the check fixture's own class, and no other.
		if cls.Name.Name != staticCheckedCheckClass {
			return staticCheckedProfile{}, false
		}
		// A check's label is the carrier's map key AND the value of `name`, so an
		// empty one has no byte-proven rendering.
		if !staticCheckedASCIILabel(label) {
			return staticCheckedProfile{}, false
		}
	case schema.ConstraintAssert:
		if cls.Name.Name != staticCheckedAssertClass {
			return staticCheckedProfile{}, false
		}
		// An assert's label is OPTIONAL — stock renders `Failed: <expr>` without one —
		// but a PRESENT one must be inside the proven ASCII set.
		//
		// PRESENCE is tested on the POINTER, never on the normalised string. A
		// pointer-to-empty-string is a label that is THERE and is empty, which is a
		// different schema from an absent one: internal/bamlprofile rejects it as an
		// invalid BAML identifier, schema.lowerConstraints deliberately preserves the
		// nil-versus-present distinction, and nothing has captured what stock does with
		// it. Collapsing the two — `label != "" && …`, which is what this used to say —
		// silently admitted the present-empty form through EVERY gate and rendered its
		// false assert as the UNLABELLED error, a byte shape no capture establishes for
		// that source.
		if c.Label != nil && !staticCheckedASCIILabel(*c.Label) {
			return staticCheckedProfile{}, false
		}
	default:
		return staticCheckedProfile{}, false
	}
	expression, ok := staticCheckedCanonicalExpression(c.Expression)
	if !ok {
		return staticCheckedProfile{}, false
	}
	return staticCheckedProfile{
		className:  cls.Name.Name,
		level:      c.Level,
		label:      label,
		expression: expression,
	}, true
}

// staticCheckedCanonicalType reports whether t carries ONLY the payloads its Kind
// selects, with every other [schema.Type] field at its zero value.
//
// It exists because [schema.Bundle.ValidateOutput] validates the SELECTED kind and
// IGNORES irrelevant populated payloads, while [SupportsNativeFinalBundle] and
// [ParseStaticBundleUnaryCall] deliberately accept a PRE-LOWERED Bundle. So a
// hand-constructed, ValidateOutput-valid bundle could set `Media`, `Name`, `Mode`,
// `Literal`, `Elem`, `Key`, `Value`, `Items`, `Union` or `Arrow` on a primitive field —
// or the primitive/collection payloads on the class target — and still look like the
// fingerprint to a predicate that only read Kind/Primitive. That is a representation the
// byte captures say nothing about, so it must FAIL CLOSED, exactly as the `dynamic`
// primitive already does.
//
// Ordinary static-descriptor lowering rejects a stray payload today, so this is not a
// generated-BAML route; it is the same class of hand-built Bundle ingress the `dynamic`
// guard is about, and "no other metadata" has to mean the whole struct.
//
// TestStaticCheckedCanonicalTypeCoversEveryField reflects over schema.Type and fails if a
// field appears that this function neither SELECTS nor requires to be zero, so a future
// payload cannot slip in behind it.
func staticCheckedCanonicalType(t schema.Type, kind schema.TypeKind) bool {
	if t.Kind != kind || t.Dynamic {
		return false
	}
	// Payloads NO admitted kind selects. Meta is governed separately (it carries the one
	// admitted constraint and the stream flags the callers check).
	//
	// PRESENCE, not emptiness. `Items` is a SLICE, whose zero value is nil, so a
	// non-nil EMPTY slice — `Items: []schema.Type{}` — is a populated payload even
	// though it has length zero. Testing `len(t.Items) != 0` admitted exactly that
	// representation: ValidateOutput does not traverse Items on a primitive or class
	// node, so it reached the fingerprint and was claimed. The pointer payloads have
	// always used `!= nil` for the same reason; this is the slice getting the same rule.
	// (The descriptor lowerer is already stricter here, so ordinary generated BAML never
	// produced it — but SupportsNativeFinalBundle and ParseStaticBundleUnaryCall accept
	// PRE-LOWERED bundles, which is the ingress this whole predicate exists for.)
	if t.Media != "" || t.Literal != nil || t.Elem != nil || t.Key != nil || t.Value != nil ||
		t.Items != nil || t.Union != nil || t.Arrow != nil {
		return false
	}
	switch kind {
	case schema.TypePrimitive:
		// Primitive selects Primitive alone; Name/Mode belong to a class/enum reference.
		return t.Name == "" && t.Mode == ""
	case schema.TypeClass:
		// Class selects Name + Mode, which the caller pins to the fixture identity.
		return t.Primitive == ""
	default:
		// The fingerprint uses no other kind, and a kind this function has not been
		// taught is not something to guess about.
		return false
	}
}

// staticCheckedPlainField reports whether f is the named, unaliased, undescribed,
// non-streaming, non-dynamic, directly-typed primitive the fingerprint requires. It
// does NOT look at constraints — the caller wants opposite answers for the two fields.
//
// The TYPE payload rule is [staticCheckedCanonicalType], and it covers the WHOLE
// schema.Type rather than the kind-selected part. `Dynamic` is the case that made the
// rule necessary — it is documented as meaningful only for enums and classes, but it is
// a field of EVERY Type and [schema.Bundle.ValidateOutput] does not reject it on a
// primitive — and Media/Name/Mode/Literal/Elem/Key/Value/Items/Union/Arrow are the same
// hazard. Nothing has measured what stock does for any of those variants, and the whole
// point of a fingerprint is that everything inside it has a byte capture behind it, so
// each declines rather than being admitted on the grounds that ordinary descriptor
// lowering happens not to produce it today.
func staticCheckedPlainField(f *schema.ClassField, name string, prim schema.PrimitiveKind) bool {
	return f.Name.Name == name && f.Name.Alias == nil && f.Description == nil && !f.StreamingNeeded &&
		staticCheckedCanonicalType(f.Type, schema.TypePrimitive) && f.Type.Primitive == prim &&
		f.Type.Meta.Stream.IsZero()
}

// staticCheckedMaxCauseLen is the length at which stock v0.223.0 TRUNCATES a
// `ParsingError` cause: `validate_asserts` measures `Failed: <label> <expr>` with
// Rust's `String::len()` (bytes) and, when it exceeds this, cuts it here and appends
// `...`. internal/debaml/checkedwire measures the boundary directly — its
// AssertFailCause100 row is NOT truncated and AssertFailCause101 IS.
const staticCheckedMaxCauseLen = 100

// staticCheckedCausePrefix is the fixed head of every rendered assertion cause.
// It is named so the length bound below is arithmetic over the SAME string
// [staticCheckedAssertFailure] writes, not a number that agrees with it by luck.
const staticCheckedCausePrefix = "Failed: "

// staticCheckedMaxLabelLen bounds the admitted label so the rendered cause can never
// reach [staticCheckedMaxCauseLen].
//
// The cause is [staticCheckedCausePrefix] + label + one separator space + the
// expression. Bounding the label in the FINGERPRINT — rather than teaching the
// renderer to truncate — is the conservative choice the scope requires: truncation
// interacts with Rust's UTF-8-boundary panic, which checkedwire records as an
// UNMEASURED hazard, so a renderer that reproduced it would be claiming bytes
// nothing has measured. Declining a longer label is safe over-decline;
// [staticCheckedAssertFailure] re-checks the assembled cause as a backstop.
//
// SLICE 7.2c-2: RECOMPUTED, AND FROM NOW ON DERIVED. The expression term used to be
// the LITERAL string `this > -9223372036854775808` (27 bytes), which is correct only
// while the manifest is `>`-shaped. `>=` and `<=` are one byte longer, so the moment
// 7.2c-3 adds either token the same 64-byte label would render a 101-byte cause —
// one byte INTO stock's truncation regime, producing an untruncated string where
// stock truncates and appends `...`. The 7.2c scope names that hazard directly
// ("A one-byte-longer expression can cross stock's 100-byte assert-cause boundary;
// labels must be re-bounded, not inherited").
//
// So the bound is no longer a restatement: [directI64LongestExpression] derives the
// longest canonical expression the CURRENT manifest can produce, and the bound
// follows it automatically. SLICE 7.2c-3 IS WHERE THAT PAYS: admitting `>=` and
// `<=` moved the longest canonical expression from 27 bytes to 28, so this bound
// went 64 → 63 with no edit here at all. A 64-byte label is now DECLINED, which is
// the safe direction — under the widened manifest it could have rendered a 101-byte
// cause, one byte inside stock's truncation regime.
//
// It is a var only because Go const arithmetic cannot range over a table; it is
// assigned once, at init, from a pure function of the manifest.
// TestStaticCheckedCauseBoundIsDerivedFromTheManifest pins both the present value
// and the value the six-operator manifest would produce, and proves the inherited
// bound would have overrun.
var staticCheckedMaxLabelLen = staticCheckedMaxCauseLen - len(staticCheckedCausePrefix) - 1 -
	len(directI64LongestExpression(staticCheckedManifest()))

// staticCheckedASCIILabel reports whether a constraint label is a non-empty ASCII
// identifier — letters, digits and underscore, not starting with a digit — short
// enough that the rendered assertion cause cannot be truncated.
//
// It is deliberately narrower than "any non-empty string": the label is emitted
// unescaped into the assertion cause and as a JSON object KEY, and the byte
// authority covers ASCII only (checkedwire records the non-ASCII truncation
// boundary as an UNMEASURED hazard rather than guessing at it).
func staticCheckedASCIILabel(s string) bool {
	if s == "" || len(s) > staticCheckedMaxLabelLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// staticCheckedExprMaxPad is the most ASCII-space padding an admitted SOURCE expression
// may carry on each side: one, which is exactly what BAML's `{{ expr }}` block produces.
//
// internal/debaml/checkedwire measured zero, one and TWO spaces and found stock emits
// the same unpadded string for all three, so the rule generalises — but only the two
// paddings that actually occur are admitted. A `{{ expr }}` attribute gives one space
// each side (the production descriptor form) and a hand-built Bundle gives none (the
// unit corpus form); nothing produces two, so admitting it would widen the fingerprint
// for no route. The ExprPadTwo capture stays as the non-admission oracle row that makes
// this a bounded over-decline rather than an unmeasured edge.
const staticCheckedExprMaxPad = 1

// staticCheckedStripExprPadding removes the admitted `{{ }}` padding from a source
// predicate, returning the canonical inner text.
//
// Padding is ASCII SPACE only: a tab, newline or non-breaking space has no capture
// behind it, and `strings.TrimSpace` would have silently accepted all three. More
// than [staticCheckedExprMaxPad] on either side is refused rather than trimmed, and
// an all-padding source is refused outright.
//
// It is a NAMED helper rather than an inline loop because the operator-manifest
// mutation proof (TestStaticCheckedManifestMutationBites) has to strip padding
// exactly the way production does: that twin's ONLY intended semantic difference
// from the production classifier is its operator manifest, so a second copy of this
// scan could drift and make a mutation result ambiguous — a disagreement that came
// from the padding rule would read as one that came from the manifest. One scan,
// one rule, both callers.
func staticCheckedStripExprPadding(src string) (string, bool) {
	lead, trail := 0, 0
	for lead < len(src) && src[lead] == ' ' {
		lead++
	}
	if lead == len(src) {
		return "", false // all padding, no expression
	}
	for trail < len(src)-lead && src[len(src)-1-trail] == ' ' {
		trail++
	}
	if lead > staticCheckedExprMaxPad || trail > staticCheckedExprMaxPad {
		return "", false
	}
	return src[lead : len(src)-trail], true
}

// staticCheckedCanonicalExpression strips the admitted `{{ }}` padding from a source
// predicate and proves what remains is the statically proven profile.
//
// It returns the string stock puts in [bamlutils.Check.Expression] and quotes in the
// assertion cause.
func staticCheckedCanonicalExpression(src string) (string, bool) {
	canonical, ok := staticCheckedStripExprPadding(src)
	if !ok {
		return "", false
	}
	if _, ok := staticCheckedThreshold(canonical); !ok {
		return "", false
	}
	return canonical, true
}

// staticCheckedThreshold is the STATICALLY PROVEN expression profile: exactly
// `this OP <ASCII decimal integer literal>` for an OP in the production manifest
// ([staticCheckedManifestTokens], which since Slice 7.2c-3 is the six direct
// comparisons), and nothing else.
//
// Slice 7.2c-2 turned it from a hand-written prefix match into a DATA-DRIVEN
// classifier over [parseDirectI64Comparison] — the same parser the exact-i64
// evaluator path uses. That is the point of the refactor rather than a tidy-up:
// while the classifier had its own copy of the grammar, "the profile admits it"
// and "the evaluator can decide it" were two separate claims that could disagree,
// and they DID (7.2c-1 measured 31 of 37 admitted (literal, value) pairs reaching
// a post-claim refusal). One parser, one grammar, one answer.
//
// The literal must round-trip through [strconv.FormatInt], which rejects `+5`,
// `007`, `1_000`, a float, and anything that would not survive to an i64 — so the
// admitted text is precisely the form the differential covers. The returned
// threshold is NOT used to decide the predicate (that is the evaluator's job,
// requirement 1); it exists so the classification is a proof about the EXPRESSION
// rather than a guess that the evaluator will cope.
func staticCheckedThreshold(expr string) (int64, bool) {
	cmp, ok := parseDirectI64Comparison(expr, staticCheckedManifest())
	if !ok {
		return 0, false
	}
	return cmp.literal, true
}

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// staticCheckedParse is the narrow checked-static case of [ParseStaticBundle]: it CLAIMS
// the decision for the fingerprint (the second return) instead of letting it fall through
// to the constraint-blind ordinary path.
func staticCheckedParse(b *schema.Bundle, raw string, claim staticCheckedClaim) (bamlutils.DeBAMLParseResult, error, bool) {
	prof, ok := staticCheckedProfileOf(b)
	if !ok {
		return bamlutils.DeBAMLParseResult{}, nil, false
	}
	if !claim.admits() {
		// THE FINGERPRINT MATCHED BUT THIS ROUTE MAY NOT CLAIM IT. Declining HERE is
		// load-bearing rather than tidy: the support predicate is route-agnostic, so once
		// the seam is open it answers "supported" for this bundle on EVERY route. Falling
		// through would then run the ordinary extract → coerce path, which knows nothing
		// about constraints, and serve `{"answer":…,"confidence":9}` with no carrier and
		// no assertion — an over-claim on exactly the shape this slice exists to be
		// careful about.
		return bamlutils.DeBAMLParseResult{}, unsupported(
			"checked-static fingerprint on a route that may not claim it"), true
	}
	res, err := staticCheckedMap(b, prof, raw)
	return res, err, true
}

// ---------------------------------------------------------------------------
// The mapper
// ---------------------------------------------------------------------------

// staticCheckedMap is the coercion-state → carrier/error mapper itself.
//
// It runs the SAME extract → coerce pipeline [ParseStaticBundle] runs, so the
// canonical class field order and the normal JSON behaviour of every unconstrained
// field are INHERITED rather than re-derived, and the raw assistant text is read
// exactly once (requirement 5). What it adds is the constraint layer: evaluate the
// one proven predicate over the canonical `confidence` value, then either wrap that
// field in the carrier (`@check`) or reject the node with stock's assertion error
// (`@assert`).
//
// Every failure that is not a real parse failure returns the unsupported sentinel
// (requirement 6), including a byte-parity failure of the splice: if the object the
// mapper rebuilds is not byte-identical to the coercion's own output outside the one
// wrapped field, the mapper has no proof it preserved normal JSON behaviour and must
// decline rather than serve.
//
// SENSITIVE: raw is provider output and the returned JSON is parsed provider output;
// the caller treats both like the response body and never logs them.
func staticCheckedMap(b *schema.Bundle, prof staticCheckedProfile, raw string) (bamlutils.DeBAMLParseResult, error) {
	// The same extraction the static path performs: JSONish comments stripped, then
	// the single cleanly-claimable candidate. No candidate DECLINES (BAML may still
	// recover it) — never claims.
	parsed, ok := extractCandidateMode(stripJSONComments(raw), bundleNumMode(b))
	if !ok {
		return bamlutils.DeBAMLParseResult{}, unsupported("no cleanly-claimable JSON candidate")
	}
	// The canonical coercion output. A sentinel error is a decline; anything else is
	// a CLAIMED parse failure BAML would also hit, propagated unchanged.
	out, err := coerce(b, b.Target, parsed, nil, &coerceCtx{})
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}

	// Consume the coercion's OWN canonical output: split it into ordered members
	// without touching the raw assistant text again, and require the member list to
	// be exactly the class's canonical field order.
	members, err := staticCheckedSplit(out)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if len(members) != 2 ||
		members[0].key != staticCheckedAnswerField || members[1].key != staticCheckedConfidenceField {
		return bamlutils.DeBAMLParseResult{}, unsupported(fmt.Sprintf(
			"canonical coercion output for class %q is not the admitted field order [%s %s]",
			prof.className, staticCheckedAnswerField, staticCheckedConfidenceField))
	}
	// BYTE-PARITY PROOF. Rebuilding the object from the split members must reproduce
	// the coercion's bytes EXACTLY. Without it the splice below would be an
	// unproven re-serialization of the whole object rather than a substitution of one
	// member, and a whitespace or escaping difference in an untouched field would
	// ride out to the wire unnoticed.
	rebuilt, err := staticCheckedJoin(members)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if !bytes.Equal(rebuilt, out) {
		return bamlutils.DeBAMLParseResult{}, unsupported(
			"canonical coercion output does not survive an unmodified member-by-member rebuild; " +
				"the mapper cannot prove it preserves normal JSON behaviour")
	}

	confidence, err := staticCheckedInt(members[1].raw)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}

	// REQUIREMENT 1: the production evaluator, over the canonical coerced value.
	//
	// An evaluator failure is NOT a failed check and NOT an assertion error — stock
	// formats it as its own `Failed to evaluate constraints:` rejection. Native has
	// no byte proof for that form, and the expression profile was proven statically,
	// so reaching here means native disagrees with its own proof: DECLINE.
	held, err := EvaluateConstraint(IntValue(confidence), prof.expression)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr(
			"evaluate the statically proven constraint expression", err)
	}

	if prof.level == schema.ConstraintAssert {
		if held {
			// A PASSING assert leaves no trace in the value: no check entry, no wrapper,
			// the canonical bytes unchanged. (checkedwire's AssertPassNoCheck row is the
			// stock capture of exactly that.)
			return bamlutils.DeBAMLParseResult{JSON: out}, nil
		}
		// REQUIREMENT 4: a FALSE assert emits NO value. The error is stock's, in the
		// required-field coercion scope the constrained field sits in.
		rendered, rerr := staticCheckedAssertFailure(staticCheckedConfidenceField, prof.label, prof.expression)
		if rerr != nil {
			return bamlutils.DeBAMLParseResult{}, rerr
		}
		return bamlutils.DeBAMLParseResult{}, rendered
	}

	// REQUIREMENT 3: a true/false @check is DATA. Both statuses still emit the value.
	status := bamlutils.CheckFailed
	if held {
		status = bamlutils.CheckSucceeded
	}
	// REQUIREMENT 2: the source expression text, VERBATIM.
	carrier, cerr := bamlutils.NewChecked(confidence, []bamlutils.Check{{
		Name:       prof.label,
		Expression: prof.expression,
		Status:     status,
	}})
	if cerr != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("build the Checked carrier", cerr)
	}
	// sonic is the WIRE serializer (worker/parse.go and the final stream path), so the
	// carrier bytes spliced in here are the bytes the wire acceptance test pins.
	wrapped, merr := sonic.Marshal(carrier)
	if merr != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("serialize the Checked carrier", merr)
	}
	members[1].raw = wrapped
	spliced, jerr := staticCheckedJoin(members)
	if jerr != nil {
		return bamlutils.DeBAMLParseResult{}, jerr
	}
	return bamlutils.DeBAMLParseResult{JSON: spliced}, nil
}

// staticCheckedMember is one member of the canonical coercion output: its key and
// its RAW value bytes, exactly as the coercion emitted them.
type staticCheckedMember struct {
	key string
	raw json.RawMessage
}

// staticCheckedSplit decomposes the canonical coercion output into ordered members.
//
// It is a token walk over NATIVE's own output, not a re-parse of the assistant text:
// the bytes it reads were produced by [coerce] a few lines earlier. The decode is
// strict (a single JSON value, then EOF) and the value of each member is captured as
// a [json.RawMessage] so nothing is re-encoded — which is what makes the byte-parity
// rebuild in [staticCheckedMap] a real proof rather than a re-serialization that
// would agree with itself.
func staticCheckedSplit(canonical json.RawMessage) ([]staticCheckedMember, error) {
	dec := json.NewDecoder(bytes.NewReader(canonical))
	tok, err := dec.Token()
	if err != nil {
		return nil, unsupportedErr("read the canonical coercion output", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, unsupported("canonical coercion output is not a JSON object")
	}
	var members []staticCheckedMember
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return nil, unsupportedErr("read a canonical member key", kerr)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, unsupported("canonical member key is not a string")
		}
		var raw json.RawMessage
		if verr := dec.Decode(&raw); verr != nil {
			return nil, unsupportedErr("read a canonical member value", verr)
		}
		members = append(members, staticCheckedMember{key: key, raw: raw})
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, unsupportedErr("close the canonical coercion output", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, unsupported("canonical coercion output carries a trailing JSON value")
		}
		return nil, unsupportedErr("read past the canonical coercion output", err)
	}
	return members, nil
}

// staticCheckedJoin re-emits ordered members as a compact JSON object. Keys go
// through [json.Marshal] (the encoder [marshalOrderedEntries] uses for the same job)
// and values are written UNTOUCHED, so an unmodified member set rebuilds the input
// byte for byte.
func staticCheckedJoin(members []staticCheckedMember) (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			// Not expected — the keys come from a decoded JSON document — but a key
			// that cannot be re-emitted is a DECLINE, never a substituted placeholder:
			// emitting anything else would claim a field name the coercion did not
			// produce.
			return nil, unsupportedErr("re-emit a canonical member key", err)
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(m.raw)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// staticCheckedInt reads the canonical `confidence` member as an i64, STRICTLY.
//
// BAML's value domain for an `int` field is i64 (BamlValue::Int), and the predicate
// must see the same number the wire carries. A float, an exponent form, a value
// outside i64, or a non-number therefore DECLINES rather than being rounded or
// widened into something the evaluator would decide differently from stock.
func staticCheckedInt(raw json.RawMessage) (int64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	// Decoded as `any`, NOT straight into a json.Number: json.Number is a string type,
	// and encoding/json will happily store a QUOTED "9" in one. Type-asserting the
	// decoded value is what makes a JSON string, bool or null a decline rather than a
	// silently accepted number.
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return 0, unsupportedErr("read the canonical int member", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return 0, unsupported("canonical int member carries a trailing JSON value")
		}
		return 0, unsupportedErr("read past the canonical int member", err)
	}
	num, ok := decoded.(json.Number)
	if !ok {
		return 0, unsupported(fmt.Sprintf("canonical int member is a %T, not a JSON number", decoded))
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return 0, unsupportedErr("the canonical int member is not an i64", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// The assertion error
// ---------------------------------------------------------------------------

// staticCheckedAssertError is the native rendering of stock v0.223.0's failed-assert
// rejection. It is deliberately a NARROW unexported type rather than a new public
// error API (the scope forbids one crossing the module boundary): the only contract
// is that [error.Error] returns the stock bytes, which
// internal/debaml/checkedwire's captures pin against the real CFFI.
//
// It does NOT wrap [bamlutils.ErrDeBAMLParseUnsupported]: a false assert is a
// CLAIMED parse failure that BAML would also produce, not a decline.
type staticCheckedAssertError struct{ msg string }

func (e *staticCheckedAssertError) Error() string { return e.msg }

// staticCheckedAssertFailure renders the stock v0.223.0 error for ONE failing
// `@assert` on a REQUIRED class field.
//
// The shape is measured, not inferred — internal/debaml/checkedwire drives the same
// declaration through the real CFFI and pins the unmodified `err.Error()`:
//
//	Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing
//	required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [],
//	reason: "Failed to parse field <field>: <root>: Assertions failed.\n  - <root>:
//	Failed: <label> <expr>", causes: [ParsingError { scope: [], reason: "Assertions
//	failed.", causes: [ParsingError { scope: [], reason: "Failed: <label> <expr>",
//	causes: [] }] }] }] }
//
// Three nested facts are load-bearing and none of them is cosmetic: the direct-parse
// wrapper is Rust's `{:?}` DEBUG rendering (so the embedded newline is the two bytes
// `\` `n`, never a real one), the field-level reason embeds the DISPLAY rendering of
// the inner error (`<root>: …\n  - <root>: …`), and the inner tree is retained
// ALONGSIDE that flattened text rather than replaced by it.
//
// A label-free assert renders `Failed: <expr>`; the separator space belongs to the
// label, not to the prefix.
func staticCheckedAssertFailure(field, label, expression string) (error, error) {
	cause := staticCheckedCausePrefix
	if label != "" {
		cause += label + " "
	}
	cause += expression
	// BACKSTOP for the truncation boundary the fingerprint already bounds
	// ([staticCheckedMaxLabelLen]). A cause stock would truncate has no byte proof
	// here, so it DECLINES rather than being emitted untruncated — which would be a
	// silent divergence in the one string this renderer exists to reproduce.
	if len(cause) > staticCheckedMaxCauseLen {
		return nil, unsupported(fmt.Sprintf(
			"the assertion cause is %d bytes; stock truncates above %d and that boundary is not byte-proven here",
			len(cause), staticCheckedMaxCauseLen))
	}

	inner := staticCheckedParsingError{
		reason: "Assertions failed.",
		causes: []staticCheckedParsingError{{reason: cause}},
	}
	innerDisplay, err := inner.display()
	if err != nil {
		return nil, err
	}
	outer := staticCheckedParsingError{
		reason: "Failed while parsing required fields: missing=0, unparsed=1",
		causes: []staticCheckedParsingError{{
			reason: "Failed to parse field " + field + ": " + innerDisplay,
			causes: []staticCheckedParsingError{inner},
		}},
	}
	debug, err := outer.debug()
	if err != nil {
		return nil, err
	}
	return &staticCheckedAssertError{msg: "Failed to coerce value: " + debug}, nil
}

// staticCheckedParsingError mirrors BAML v0.223.0's `ParsingError` for the ONE
// nesting the admitted fingerprint can produce. `scope` is always empty here — the
// captured stock bytes show `scope: []` at every level of this chain — so it is not
// modelled as a field that could silently acquire a value the renderer cannot prove.
type staticCheckedParsingError struct {
	reason string
	causes []staticCheckedParsingError
}

// display renders the Rust `Display` form: `<root>: <reason>`, then each cause on
// its own line prefixed with two spaces and `- `, recursively.
//
// `<root>` is what an EMPTY scope renders as; a non-empty scope is not modelled
// because this renderer never produces one.
func (e staticCheckedParsingError) display() (string, error) {
	reason, err := staticCheckedPlain(e.reason)
	if err != nil {
		return "", err
	}
	out := "<root>: " + reason
	for _, c := range e.causes {
		sub, err := c.display()
		if err != nil {
			return "", err
		}
		// Each nested line is indented by two spaces, so a cause of a cause lines up
		// under its parent exactly as stock's own recursive formatter produces.
		out += "\n  - " + strings.ReplaceAll(sub, "\n", "\n  ")
	}
	return out, nil
}

// debug renders the Rust `{:?}` form of the same value.
func (e staticCheckedParsingError) debug() (string, error) {
	reason, err := staticCheckedDebugString(e.reason)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("ParsingError { scope: [], reason: ")
	b.WriteString(reason)
	b.WriteString(", causes: [")
	for i, c := range e.causes {
		if i > 0 {
			b.WriteString(", ")
		}
		sub, err := c.debug()
		if err != nil {
			return "", err
		}
		b.WriteString(sub)
	}
	b.WriteString("] }")
	return b.String(), nil
}

// staticCheckedDebugString renders s as Rust's `{:?}` for a `String`: double quotes,
// with `"`, `\`, newline, carriage return and tab escaped.
//
// It FAILS CLOSED on anything outside printable ASCII. Rust's Debug escapes many
// more characters (`\u{…}` for non-printables, and its own rules for Unicode), and
// none of that is byte-proven here — the admitted fingerprint's labels and
// expressions are ASCII by construction ([staticCheckedASCIILabel],
// [staticCheckedThreshold]), so a character that reaches this function is evidence
// the fingerprint let something through, not something to guess an escape for.
func staticCheckedDebugString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 || c > 0x7e {
				return "", unsupported(fmt.Sprintf(
					"assertion text carries the byte %#02x, whose Rust Debug escape is not byte-proven", c))
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// staticCheckedPlain returns s unchanged after proving it is printable ASCII plus
// newline — the Display form applies no escaping, so an unprovable byte must be
// caught here rather than emitted raw into the flattened field reason.
func staticCheckedPlain(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\n' && (c < 0x20 || c > 0x7e) {
			return "", unsupported(fmt.Sprintf(
				"assertion text carries the byte %#02x, which the Display renderer cannot prove", c))
		}
	}
	return s, nil
}

// staticCheckedIsAssertFailure reports whether err is the rendered stock assertion
// failure. It exists so the seam's own tests can distinguish a CLAIMED assertion
// rejection from a decline without matching on message text.
func staticCheckedIsAssertFailure(err error) bool {
	var target *staticCheckedAssertError
	return errors.As(err, &target)
}

// ParseStaticBundleUnaryCall is [ParseStaticBundle] for the STATIC UNARY /call serve
// route — the ONE route the 7.2b scope admits the checked-static fingerprint on.
//
// It is byte-for-byte [ParseStaticBundle] for every bundle except the two concrete
// checked-static fixtures. The difference is the capability it carries: this route may
// claim the fingerprint, and the direct routes ([ParseStaticBundle] itself, root
// [Parse]'s ordinary static-descriptor lane, the shadow comparator, the stream-final
// completion lane) may not. That is the scope's boundary — "static unary /call final
// parsing only", with direct parse endpoints not behind the static unary seam left
// declined — expressed as a route rather than as a comment.
//
// Slice 7.2b-3 wires the isolated serve core's SAP closure
// (nativeserve/canary.ServeStatic) to this function; it is the only production caller,
// and every other consumer of the static parser keeps calling [ParseStaticBundle].
//
// SENSITIVE: raw is provider output and the returned JSON is parsed provider output.
func ParseStaticBundleUnaryCall(ctx context.Context, bundle *schema.Bundle, raw string) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx // M1 parsing is a local CPU operation; no cancellation points.

	if bundle == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil static bundle")
	}
	if err := bundle.ValidateOutput(); err != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("validate static bundle", err)
	}
	if err := SupportsNativeFinalBundle(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if res, err, claimed := staticCheckedParse(bundle, raw, staticCheckedGrantStaticUnaryCall()); claimed {
		return res, err
	}
	// Not the checked-static fingerprint: the ordinary static parse, unchanged. Routing
	// through [ParseStaticBundle] rather than duplicating its body is what keeps the two
	// routes from drifting for every shape this one does not specialise.
	return ParseStaticBundle(ctx, bundle, raw)
}
