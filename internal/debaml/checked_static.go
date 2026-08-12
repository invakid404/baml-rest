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
	"sync/atomic"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2b-2 — the PRODUCTION coercion-state → `Checked[T]` carrier mapper,
// behind a NON-ADMITTING seam.
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
// # It admits nothing yet
//
// Admission needs BOTH halves of the seam, and both are DENY in 7.2b-2:
// [staticCheckedAdmitsConstraints] (the cutover constant, or the
// descriptor-specific test seam [staticCheckedSeamOpen] standing in for it), and a
// route CAPABILITY that only the static unary /call route carries. Every
// constraint-bearing bundle therefore still declines through checkSupported /
// checkSupportedFields / checkSupportedType / [SupportsNativeFinalBundle] / [Parse] /
// [ParseStaticBundle] and through nativeserve's admission gate — the four #665
// companion rows for this exact fingerprint included.
//
// Splitting it that way is what keeps the cutover inside the scope's boundary
// ("static unary /call final parsing only", direct parse endpoints left declined) and
// what lets the closed seam be PROVEN to be the only thing holding the fingerprint
// back: the tests open it and watch these same production gates move.
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

// staticCheckedAdmitsConstraints is THE non-admitting seam of Slice 7.2b-2.
//
// While it is false, [staticCheckedFinalSupport] and [staticCheckedParse] report
// "not my case" for EVERY bundle, so both entry points fall through to the
// unchanged constraint decline and nothing below this line is reachable from a
// request. The mapper is exercised only by this package's own tests, which call it
// directly.
//
// 7.2b-3 flips it to true. That is deliberately the whole cutover at this layer:
// there is no second, more permissive switch to forget, which is the failure mode
// the scope calls out ("do not lift one blanket 'has constraints' rejection and
// leave another gate more permissive").
const staticCheckedAdmitsConstraints = false

// staticCheckedClaim is the CAPABILITY to claim the checked-static fingerprint, and it
// is the second half of the seam.
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

// staticCheckedSeamOpen is the DESCRIPTOR-SPECIFIC test seam.
//
// It exists so the closed seam can be shown to be the ONLY thing holding the
// fingerprint back — by opening it and watching the REAL production gates admit the
// same four rows, rather than by driving stand-ins that never execute production code.
//
// It is DENY by default and no production code writes it: the only writer is
// [OpenStaticCheckedSeamForTest], and [TestStaticCheckedSeamHasNoProductionWriter]
// proves that structurally. Opening it does NOT lift the constraint cut-line generally
// — every gate still runs [staticCheckedProfileOf] first, so exactly the two concrete
// generated fixture return types are affected and every other constraint-bearing bundle
// keeps declining.
//
// atomic.Bool rather than a plain bool: a test that opens it while another goroutine
// reads a gate would otherwise be a data race under `-race -count=100`.
var staticCheckedSeamOpen atomic.Bool

// OpenStaticCheckedSeamForTest opens the checked-static seam and returns the closer.
//
// It is EXPORTED only because the admission gate that consults this package lives in
// another module (nativeserve), which must be able to drive the same open state; the
// package is `internal/`, so the reach is bounded to this repository. It is deliberately
// NOT a general "constraints supported" switch: it gates the two concrete fixture
// fingerprints and nothing else.
//
// SENSITIVE TO MISUSE, so it is loud rather than convenient: it returns a closer the
// caller must defer, and opening an already-open seam panics rather than silently
// nesting (which would let one test's closer re-close another's open state).
func OpenStaticCheckedSeamForTest() func() {
	if !staticCheckedSeamOpen.CompareAndSwap(false, true) {
		panic("debaml: the checked-static test seam is already open; nested opens would make one " +
			"test's closer end another test's open state")
	}
	return func() { staticCheckedSeamOpen.Store(false) }
}

// staticCheckedGrantStaticUnaryCall is the ONE granting constructor: the capability the
// static unary /call serve route carries.
//
// It grants when the seam constant is flipped (the 7.2b-3 cutover) OR while the
// descriptor-specific test seam is open. Both are DENY today, so it grants nothing in a
// production process.
func staticCheckedGrantStaticUnaryCall() staticCheckedClaim {
	if !staticCheckedAdmitsConstraints && !staticCheckedSeamOpen.Load() {
		return staticCheckedClaim{}
	}
	return staticCheckedClaim{staticUnaryCall: true}
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

// staticCheckedExprPrefix is the ONLY predicate head the statically proven
// expression profile accepts. See [staticCheckedThreshold].
const staticCheckedExprPrefix = "this > "

// staticCheckedProfile is the classification of a bundle that matches the one
// admitted fingerprint:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(<label>, {{ this > <int> }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(<label?>, {{ this > <int> }}) }
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
	// expression is the source predicate text, VERBATIM. It is what
	// [bamlutils.Check.Expression] must carry and what the rendered assertion cause
	// must quote, so it is never normalised, re-spaced or re-serialized.
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
	if len(b.RecursiveClasses) > 0 || len(b.StructuralRecursiveAliases) > 0 {
		return staticCheckedProfile{}, false
	}
	// EXACTLY one class and no enums. A second definition is a different schema even
	// if the root class matches.
	if len(b.Classes) != 1 || len(b.Enums) != 0 {
		return staticCheckedProfile{}, false
	}
	cls := &b.Classes[0]
	// The target is the class itself, non-streaming, and carries NO metadata of its
	// own — a target-level constraint is the #664 over-claim and stays declined.
	if b.Target.Kind != schema.TypeClass || b.Target.Name != cls.Name.Name ||
		b.Target.Mode != schema.NonStreaming || !b.Target.Meta.IsZero() || b.Target.Dynamic {
		return staticCheckedProfile{}, false
	}
	if cls.Mode != schema.NonStreaming || cls.Name.Alias != nil || cls.Description != nil ||
		len(cls.Constraints) > 0 || !cls.Stream.IsZero() || len(cls.Fields) != 2 {
		return staticCheckedProfile{}, false
	}
	answer, confidence := &cls.Fields[0], &cls.Fields[1]
	if !staticCheckedPlainField(answer, staticCheckedAnswerField, schema.PrimitiveString) ||
		len(answer.Type.Meta.Constraints) > 0 {
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
		// An assert's label is optional — stock renders `Failed: <expr>` without one —
		// but a present one must still be inside the proven ASCII set.
		if label != "" && !staticCheckedASCIILabel(label) {
			return staticCheckedProfile{}, false
		}
	default:
		return staticCheckedProfile{}, false
	}
	if _, ok := staticCheckedThreshold(c.Expression); !ok {
		return staticCheckedProfile{}, false
	}
	return staticCheckedProfile{
		className:  cls.Name.Name,
		level:      c.Level,
		label:      label,
		expression: c.Expression,
	}, true
}

// staticCheckedPlainField reports whether f is the named, unaliased, undescribed,
// non-streaming, non-dynamic, directly-typed primitive the fingerprint requires. It
// does NOT look at constraints — the caller wants opposite answers for the two fields.
//
// [schema.Type.Dynamic] is checked even though it is documented as meaningful only for
// enums and classes: it is a field of EVERY Type, and [schema.Bundle.ValidateOutput]
// does not reject it on a primitive, so a hand-constructed Bundle could otherwise carry
// it into the fingerprint. Nothing has measured what stock does for that variant, and
// the whole point of a fingerprint is that everything inside it has a byte capture
// behind it — so it declines rather than being admitted on the grounds that ordinary
// descriptor lowering happens not to produce it today.
func staticCheckedPlainField(f *schema.ClassField, name string, prim schema.PrimitiveKind) bool {
	return f.Name.Name == name && f.Name.Alias == nil && f.Description == nil && !f.StreamingNeeded &&
		f.Type.Kind == schema.TypePrimitive && f.Type.Primitive == prim &&
		!f.Type.Dynamic && f.Type.Meta.Stream.IsZero()
}

// staticCheckedMaxCauseLen is the length at which stock v0.223.0 TRUNCATES a
// `ParsingError` cause: `validate_asserts` measures `Failed: <label> <expr>` with
// Rust's `String::len()` (bytes) and, when it exceeds this, cuts it here and appends
// `...`. internal/debaml/checkedwire measures the boundary directly — its
// AssertFailCause100 row is NOT truncated and AssertFailCause101 IS.
const staticCheckedMaxCauseLen = 100

// staticCheckedMaxLabelLen bounds the admitted label so the rendered cause can never
// reach [staticCheckedMaxCauseLen].
//
// The cause is `Failed: ` (8 bytes) + label + one separator space + the expression,
// whose longest admitted form is `this > -9223372036854775808` (26 bytes). Bounding
// the label in the FINGERPRINT — rather than teaching the renderer to truncate — is
// the conservative choice the scope requires: truncation interacts with Rust's
// UTF-8-boundary panic, which checkedwire records as an UNMEASURED hazard, so a
// renderer that reproduced it would be claiming bytes nothing has measured. Declining
// a longer label is safe over-decline; [staticCheckedAssertFailure] re-checks the
// assembled cause as a backstop.
const staticCheckedMaxLabelLen = staticCheckedMaxCauseLen - len("Failed: ") - 1 - len("this > -9223372036854775808")

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

// staticCheckedThreshold is the STATICALLY PROVEN expression profile: exactly
// `this > <ASCII decimal integer literal>`, and nothing else.
//
// The literal must round-trip through [strconv.FormatInt], which rejects `+5`,
// `007`, `1_000`, a float, and anything that would not survive to an i64 — so the
// admitted text is precisely the form the differential covers. The returned
// threshold is NOT used to decide the predicate (that is the evaluator's job,
// requirement 1); it exists so the classification is a proof about the EXPRESSION
// rather than a guess that the evaluator will cope.
func staticCheckedThreshold(expr string) (int64, bool) {
	digits, ok := strings.CutPrefix(expr, staticCheckedExprPrefix)
	if !ok || digits == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != digits {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// staticCheckedFinalSupport is the narrow checked-static case of
// [SupportsNativeFinalBundle].
//
// The SECOND return is the seam: false means "not this case, keep going", which is
// what every caller gets while [staticCheckedAdmitsConstraints] is false. Wiring it
// as a claim-the-decision hook rather than as an early `return nil` is what keeps
// the flip to a single constant: with the seam open, an admitted bundle answers
// `nil` HERE and never reaches the constraint decline below it, while every
// non-matching bundle still falls through to the identical gate it has today.
// It carries no route capability, and deliberately so: "can the native final parser own
// this shape" is a property of the SHAPE, not of who is asking. The route decision is
// [staticCheckedParse]'s, which is why that function declines a matched fingerprint on a
// route that may not claim it rather than falling through.
func staticCheckedFinalSupport(b *schema.Bundle) (error, bool) {
	if !staticCheckedGrantStaticUnaryCall().admits() {
		return nil, false
	}
	if _, ok := staticCheckedProfileOf(b); !ok {
		return nil, false
	}
	return nil, true
}

// staticCheckedParse is the narrow checked-static case of [ParseStaticBundle], with
// the same claim-the-decision seam as [staticCheckedFinalSupport].
//
// With the seam closed it never runs, because [SupportsNativeFinalBundle] has
// already declined the bundle before ParseStaticBundle reaches this point.
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
	cause := "Failed: "
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
// checked-static fixtures, and identical even for those while the seam is closed. The
// difference is the capability it carries: this route may claim the fingerprint, and
// the direct routes ([ParseStaticBundle] itself, root [Parse]'s ordinary
// static-descriptor lane, the shadow comparator, the stream-final completion lane) may
// not. That is the scope's boundary — "static unary /call final parsing only", with
// direct parse endpoints not behind the static unary seam left declined — expressed as
// a route rather than as a comment.
//
// It exists NOW, closed, so the boundary can be exercised: opening the
// descriptor-specific test seam makes this route serve the fingerprint while every
// direct route still declines, which is what makes the boundary a measured fact.
// Wiring the isolated serve core to call it is the 7.2b-3 cutover's own step.
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
