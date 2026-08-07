package debaml

// TEST-ONLY canonical coercion-state collector (de-BAML Slice 7.2a-2).
//
// WHAT IT IS FOR. internal/debaml/coerce.go returns json.RawMessage. That is
// the right production contract — the serving path emits bytes — but it is
// deliberately insufficient for constraints: decoding those bytes again loses
// the named class/enum identity, the declaration/schema order boundary, the
// canonical alias decision, the skipped child/union paths, and the exact node a
// constraint is attached to. `{"suit":"Hearts"}` cannot tell you that `suit` is
// the enum `Suit`, that the model wrote the alias `hearts_alias`, or that the
// class is `Hand` rather than a bare map.
//
// This file rebuilds exactly that lost information as a [constraintCoercionState]
// tree, DURING a canonical coercion, and runs the attached @check/@assert
// predicates over it through the ONE production evaluator seam
// ([ConstraintValue] + [EvaluateConstraint]).
//
// IT IS TEST-ONLY, AND THAT IS ENFORCED STRUCTURALLY. Every identifier here
// lives in a _test.go file, so it cannot be linked into any production binary.
// constraint_state_seam_test.go proves that with a go/ast walk over the whole
// repo rather than a source-text grep: it re-derives this file's declared names
// from the AST and fails if any non-test .go file so much as mentions one.
// Nothing in this collector changes coerce's return, checkSupported, Parse,
// ParseStaticBundle, admission, or any public surface. Constraint-bearing bundles
// decline — [constraintCoercionRun.ProductionSupport] records that verdict on
// every run so a fixture cannot quietly become admitted — and that now holds with
// NO exception. It briefly did not: a constraint declared on `b.Target` itself
// was admitted, because checkSupported did not walk the target type. That
// pre-existing over-claim was carried as a documented temporary exception and an
// asserted known-gap tripwire while this TEST-ONLY collector landed; the
// decline-more fix has since walked b.Target in checkSupportedFields, and
// constraint_state_test.go asserts the whole invariant with nothing carved out.
//
// HOW IT STAYS HONEST ABOUT "THE SAME COERCION". The collector never
// re-implements a canonicalization decision it could delegate. At every node it
//
//  1. calls PRODUCTION [coerce] for that (type, value) and keeps the bytes,
//  2. builds the node's [ConstraintValue] from the SCHEMA TRAVERSAL — schema
//     field order, canonical field/variant names, production's own
//     [matchString] / [matchesStringToString] / [coerceListChild] /
//     [coerceMapValueChild] / [tryCastUnion] / [selectUnionArms] decisions — and
//  3. requires the two to agree ([constraintStateJSONEquivalent]).
//
// Step 3 is the load-bearing one: a traversal that drifts from production —
// wrong field order, a dropped element kept, the wrong union arm — produces a
// different document and the collector FAILS instead of reporting a state
// nothing produced. Where the collector cannot mirror production exactly it
// returns [errConstraintStateUnmodelled] rather than guessing; the unmodelled
// shapes are named in [constraintStateCollector.canonicalValue].
//
// IDENTITY AND ORDER NEVER COME FROM JSON. Enum variants come from
// [matchString] over [enumMatchCandidates], class names and field order come
// from the [schema.ClassDef], map order comes from the input value's ordered
// entries, and the winning union arm comes from production's own selector. Only
// a SCALAR LEAF is read back from its own one-token coercion result (an int, a
// float, a bool, a string — a JSON scalar carries no identity and no order to
// lose), through a strict decoder that rejects trailing data. The CanonicalJSON
// field is an ORACLE ARTIFACT for Slice 7.2a-3 and is never an input to the
// state.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/internal/schema"
)

// ---------------------------------------------------------------------------
// Path
// ---------------------------------------------------------------------------

// constraintStatePathKind tags one step of the path from the return type to a
// coerced node.
type constraintStatePathKind uint8

const (
	// constraintPathRoot is the return-type node itself.
	constraintPathRoot constraintStatePathKind = iota
	// constraintPathField is a class field, named by its CANONICAL name.
	constraintPathField
	// constraintPathIndex is a list element, indexed by its INPUT position (so a
	// dropped element keeps the index it arrived at, not the one it would have
	// had in the emitted list).
	constraintPathIndex
	// constraintPathMapEntry is a map entry, named by its ORIGINAL input key.
	constraintPathMapEntry
	// constraintPathMapKeyType is the map's KEY TYPE node — the position map-key
	// constraints are declared at. It carries no value.
	constraintPathMapKeyType
	// constraintPathUnionArm is the WINNING union arm, indexed into
	// schema.UnionType.Variants.
	constraintPathUnionArm
)

// constraintStatePathSegment is one step of a [constraintStatePath].
type constraintStatePathSegment struct {
	Kind  constraintStatePathKind
	Name  string // field canonical name / map input key
	Index int    // list input index / union arm index
}

// constraintStatePath is the exact node a state describes. It is a value type
// and is copied on descent, so a child never aliases its parent's backing array.
type constraintStatePath []constraintStatePathSegment

// String renders the path in a stable, fully discriminating form:
//
//	$                      the return type
//	$.suit                 class field `suit` (canonical name)
//	$.hand[2]              list element at INPUT index 2
//	$.scores["b"]          map entry under the ORIGINAL input key "b"
//	$.scores.<key>         the map's key-type node
//	$|arm1                 the winning union arm at variant index 1
func (p constraintStatePath) String() string {
	var b strings.Builder
	for _, seg := range p {
		switch seg.Kind {
		case constraintPathRoot:
			b.WriteByte('$')
		case constraintPathField:
			b.WriteByte('.')
			b.WriteString(seg.Name)
		case constraintPathIndex:
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(seg.Index))
			b.WriteByte(']')
		case constraintPathMapEntry:
			b.WriteByte('[')
			b.WriteString(strconv.Quote(seg.Name))
			b.WriteByte(']')
		case constraintPathMapKeyType:
			b.WriteString(".<key>")
		case constraintPathUnionArm:
			b.WriteString("|arm")
			b.WriteString(strconv.Itoa(seg.Index))
		default:
			b.WriteString("?")
		}
	}
	return b.String()
}

// descend returns p plus one segment, copying the backing array so sibling
// descents cannot overwrite each other.
func (p constraintStatePath) descend(seg constraintStatePathSegment) constraintStatePath {
	out := make(constraintStatePath, len(p), len(p)+1)
	copy(out, p)
	return append(out, seg)
}

// ---------------------------------------------------------------------------
// Events and dispositions
// ---------------------------------------------------------------------------

// constraintStateOutcome is what the evaluator did with one predicate. It is an
// explicit three-way value rather than a bool: "could not decide" is a distinct
// outcome from "decided false", and collapsing them is exactly the false-green a
// constraint witness must not have.
type constraintStateOutcome string

const (
	constraintOutcomeTrue  constraintStateOutcome = "true"
	constraintOutcomeFalse constraintStateOutcome = "false"
	// constraintOutcomeUnsupported is ErrConstraintUnsupported — native could not
	// reproduce stock's answer, so there is NO boolean. The event keeps the error.
	constraintOutcomeUnsupported constraintStateOutcome = "unsupported"
)

// constraintStateOrigin records WHERE a constraint was declared, so the two
// sources stay distinguishable in the witness.
type constraintStateOrigin string

const (
	// constraintOriginDeclaration is a constraint on the class/enum DECLARATION
	// (schema.ClassDef.Constraints / schema.EnumDef.Constraints).
	constraintOriginDeclaration constraintStateOrigin = "declaration"
	// constraintOriginTypeMeta is a constraint on the TYPE NODE — the field,
	// element, map value, union arm or return type it is written on
	// (schema.TypeMeta.Constraints).
	constraintOriginTypeMeta constraintStateOrigin = "type_meta"
)

// constraintStateEvent is one evaluated predicate.
//
// NOT A MAP, ON PURPOSE. Labels REPEAT: `@check(len, ...) @check(len, ...)` is
// legal and stock records both. Folding events into map[label]result would
// silently drop one and pre-decide the wire question Slice 7.2b owns, so this is
// an ordered slice and [TestConstraintStateEventsAreOrderedNotFolded] pins the
// field's kind so a future refactor cannot quietly change it.
type constraintStateEvent struct {
	Origin     constraintStateOrigin
	Level      schema.ConstraintLevel
	Labeled    bool   // the constraint carried a label at all (nil vs "" differ)
	Label      string // the label as declared; may repeat within one node
	Expression string // the EXACT source bytes of the predicate
	Outcome    constraintStateOutcome
	// Err is non-nil exactly when Outcome is constraintOutcomeUnsupported. It is
	// the ErrConstraintUnsupported chain, retained rather than discarded so the
	// witness can say WHY native declined.
	Err error
}

// constraintStateSkipped is a constraint that did NOT run at this node.
//
// It exists so a skip is positive evidence rather than an absence. A
// SkipBareStringReturn node records, for each skipped predicate, the
// COUNTERFACTUAL outcome the evaluator produces for the very same canonical
// value — so a test can prove the predicate was reached, evaluated to false, and
// still did not reject, instead of proving only that nothing happened.
type constraintStateSkipped struct {
	Origin     constraintStateOrigin
	Level      schema.ConstraintLevel
	Labeled    bool
	Label      string
	Expression string
	// Counterfactual is what the evaluator returns for this predicate over this
	// node's canonical value. Empty only when the node has no canonical value
	// (a skipped child path or a policy-declined key node), where there is
	// nothing to evaluate against and inventing one would be a fabrication.
	Counterfactual constraintStateOutcome
	// CounterfactualErr is non-nil exactly when Counterfactual is
	// constraintOutcomeUnsupported.
	CounterfactualErr error
}

// constraintStateDisposition is what happened to the constraints at one node.
type constraintStateDisposition string

const (
	// constraintDispositionUnconstrained: no predicate is attached here. A node
	// with zero constraints is NOT "evaluated with no events" — that conflation
	// is what makes an empty-event assertion vacuous.
	constraintDispositionUnconstrained constraintStateDisposition = "unconstrained"
	// constraintDispositionEvaluated: every attached predicate ran and produced a
	// boolean.
	constraintDispositionEvaluated constraintStateDisposition = "evaluated"
	// constraintDispositionSkipBareStringReturn: the return type is a BARE
	// STRING, and stock skips constraint evaluation on that route — checks come
	// back empty and a FALSE assertion does not reject. Slice 7.2a records the
	// asymmetry; it does not "fix" it.
	constraintDispositionSkipBareStringReturn constraintStateDisposition = "skip_bare_string_return"
	// constraintDispositionUnsupportedExpression: at least one predicate returned
	// ErrConstraintUnsupported. Every predicate still has an event.
	constraintDispositionUnsupportedExpression constraintStateDisposition = "unsupported_evaluator_expression"
	// constraintDispositionSkippedPath: this child path did not contribute to the
	// parent's canonical value — a list element dropped by ArrayItemParseError, a
	// map entry dropped by MapValueParseError, or an absent optional field. Its
	// predicates did not run and NO value is synthesized for it.
	constraintDispositionSkippedPath constraintStateDisposition = "skipped_child_or_union_path"
	// constraintDispositionPolicyDeclined: a constrained shape the profile
	// refuses to evaluate at all — today the MAP KEY node, which stays a
	// negative-admission fixture.
	constraintDispositionPolicyDeclined constraintStateDisposition = "policy_declined_constrained_shape"
)

// ---------------------------------------------------------------------------
// Origin metadata
// ---------------------------------------------------------------------------

// constraintStateAliasOrigin records ONE canonicalization decision: what the
// input spelled, what the schema renders, and what the canonical identity is.
//
// It is DIAGNOSTIC ONLY. The predicate never sees it — the evaluator is handed
// the canonical [ConstraintValue] — which is precisely the asymmetry this slice
// has to represent and which #583 keeps declining in production.
type constraintStateAliasOrigin struct {
	Canonical string // the canonical field / enum-variant name
	Rendered  string // schema.Name.RenderedName() — the alias when there is one
	Observed  string // the exact input spelling that routed here
}

// constraintStateUnionOrigin records which arm of a union won.
type constraintStateUnionOrigin struct {
	// Index is the index into schema.UnionType.Variants, or len(Variants) for
	// BAML's appended null arm.
	Index int
	// NullArm is true when the winner is the null arm (iter_include_null's last
	// entry, or the JSON-null fast path).
	NullArm bool
	// Variants is len(schema.UnionType.Variants) — recorded so a test can prove a
	// state exists for exactly ONE arm out of many, not merely for "an arm".
	Variants int
}

// constraintStateDefaultOrigin records that a node's value came from a DEFAULT
// rather than from input, and which BAML rule filled it.
//
// A defaulted field is a SUCCESSFUL coercion with a real canonical value, so it
// gets a full state and its predicates run against that value — the provenance
// lives here rather than in a special disposition, because nothing about the
// evaluation is special: the default IS the field's canonical value.
type constraintStateDefaultOrigin struct {
	// Rule is BAML's flag name for the fill: DefaultFromNoValue (the field was
	// absent and its type is defaultable — TypeIR::default_value) or
	// DefaultButHadUnparseableValue (a PRESENT map field whose value coerce_map
	// refuses, which BAML fills with {}).
	Rule string
	// ObservedKind is the jsonish kind of the present-but-unusable value, and
	// empty when the field was absent entirely.
	ObservedKind string
}

// constraintStateImpliedOrigin records coerce_class's single-field absorption:
// the WHOLE input became the lone field's value.
type constraintStateImpliedOrigin struct {
	// Field is the canonical name of the lone field that absorbed the input.
	Field string
	// Inferred distinguishes the two forms: false is the OBJECT implied-key
	// (coerce_class.rs:224 — an object whose keys matched nothing), true is the
	// SCALAR/null inferred-object (coerce_class.rs:295).
	Inferred bool
}

// constraintStateArrayOrigin records coerce_class's ARRAY input branch: the
// class value is coerce_array_to_singular's winner.
type constraintStateArrayOrigin struct {
	// Index is the winning item's index in the input array.
	Index int
	// Items is how many items competed, so a test can prove the winner was
	// SELECTED rather than that there was only ever one candidate.
	Items int
}

// constraintStateRawMetadata is the `Original` half of the model: alias/origin/
// route diagnostics. It is never predicate input.
type constraintStateRawMetadata struct {
	// InputKind is the jsonish kind of the value that fed this node.
	InputKind string
	// EnumAlias is set on an enum node whose canonical variant differs from what
	// the input spelled, or whose schema renders an alias.
	EnumAlias *constraintStateAliasOrigin
	// FieldAliases holds, in SCHEMA order, one entry per class field whose input
	// key or rendered name differs from the canonical field name.
	FieldAliases []constraintStateAliasOrigin
	// Union is set on a union node.
	Union *constraintStateUnionOrigin
	// SingleToArray records BAML's non-array-into-list wrap.
	SingleToArray bool
	// DefaultFill is set when this node's value came from a default.
	DefaultFill *constraintStateDefaultOrigin
	// Implied is set on a class that absorbed the whole input into its lone field.
	Implied *constraintStateImpliedOrigin
	// ArrayToSingular is set on a class coerced from an ARRAY input.
	ArrayToSingular *constraintStateArrayOrigin
}

// ---------------------------------------------------------------------------
// The state
// ---------------------------------------------------------------------------

// constraintCoercionState is the semantic result of coercing ONE node.
type constraintCoercionState struct {
	// Path is the exact node, including field/list/map/union-arm steps.
	Path constraintStatePath
	// Type is the resolved schema node the value was coerced against.
	Type schema.Type
	// HasCanonical is false exactly for a node with NO value: a skipped child
	// path and the policy-declined map-key node. The zero ConstraintValue is a
	// valid NULL, so presence cannot be inferred from Canonical alone.
	HasCanonical bool
	// Canonical is the final canonical value — ordered, typed, and carrying the
	// class/enum identity the JSON drops.
	Canonical ConstraintValue
	// CanonicalJSON is PRODUCTION coerce's bytes for this node. ORACLE ARTIFACT
	// ONLY (Slice 7.2a-3 compares it against stock readback); it is never decoded
	// back into a ConstraintValue.
	CanonicalJSON json.RawMessage
	// Original is alias/route diagnostics — never predicate input.
	Original    constraintStateRawMetadata
	Disposition constraintStateDisposition
	// Events are the predicates that RAN, in declaration order, duplicates and all.
	Events []constraintStateEvent
	// Skipped are the predicates that did NOT run at this node.
	Skipped []constraintStateSkipped
	// SkipReason is non-empty exactly for the skipped-path and policy-declined
	// dispositions.
	SkipReason string
	// AssertFailed is true when an @assert-level event evaluated FALSE.
	AssertFailed bool
	// Children are the sub-node states in traversal order. A union contributes at
	// most ONE child (the winner); losing candidates and defaults get no state.
	Children []*constraintCoercionState
}

// walk yields this state and every descendant, parents before children, in
// traversal order.
func (s *constraintCoercionState) walk(fn func(*constraintCoercionState)) {
	if s == nil {
		return
	}
	fn(s)
	for _, c := range s.Children {
		c.walk(fn)
	}
}

// find returns the single state at the given rendered path, or nil.
func (s *constraintCoercionState) find(path string) *constraintCoercionState {
	var hit *constraintCoercionState
	s.walk(func(n *constraintCoercionState) {
		if n.Path.String() == path {
			hit = n
		}
	})
	return hit
}

// constraintCoercionRun is one collection: the state tree plus the production
// verdict recorded alongside it.
type constraintCoercionRun struct {
	Root *constraintCoercionState
	// ProductionSupport is checkSupported(bundle) VERBATIM. It is recorded, never
	// acted on: Slice 7.2a's only passing result is "test-only native state
	// exists AND production still declines", so every fixture carries the proof
	// that admission did not move.
	ProductionSupport error
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

// errConstraintStateUnmodelled is returned for a coercion shape the collector
// does not mirror EXACTLY. It is a refusal, not a fallback: a partial state for
// a shape whose canonicalization the collector guessed at would be worse than no
// state at all.
var errConstraintStateUnmodelled = errors.New("debaml: coercion shape outside the test-only constraint-state collector")

// errConstraintStateDiverged is returned when the traversal-built state does not
// serialize to the document production coerce produced for the same node. It can
// only fire if the collector stopped following production, which is the one
// failure the collector must never report as a result.
var errConstraintStateDiverged = errors.New("debaml: constraint-state traversal diverged from production coercion")

type constraintStateCollector struct {
	bundle *schema.Bundle
}

// collectConstraintCoercionState coerces raw against the bundle's target type
// and returns the constraint-aware state of that coercion.
//
// raw is decoded with production's own ordered strict decoder ([strictDecode]),
// so the collector sees the same `value` model coerce sees. Candidate
// EXTRACTION (the markdown/fixing/candidate-selection front half of Parse) is a
// separate stage that Slice 7.2a-3's serving-shaped oracle drives end to end;
// this collector is about what coercion itself knows.
func collectConstraintCoercionState(b *schema.Bundle, raw string) (*constraintCoercionRun, error) {
	if b == nil {
		return nil, fmt.Errorf("debaml: constraint-state collector: nil bundle")
	}
	in, err := strictDecode(raw)
	if err != nil {
		return nil, fmt.Errorf("debaml: constraint-state collector: decode %q: %w", raw, err)
	}
	c := &constraintStateCollector{bundle: b}
	root, err := c.node(b.Target, in, nil, constraintStatePath{{Kind: constraintPathRoot}}, false, true)
	if err != nil {
		return nil, err
	}
	return &constraintCoercionRun{Root: root, ProductionSupport: checkSupported(b)}, nil
}

// node collects one coerced node.
//
// inUnionArm reproduces production's arm accumulator (coerceFlags.targetIsUnion),
// which participates in array-to-singular and pick_best ordering, so a re-coerce
// of the winning arm scores exactly as it did inside the union.
//
// isReturnRoot is true only for the return-type node, because the bare-string
// skip is a property of the RETURN ROUTE and not of every string in the tree.
func (c *constraintStateCollector) node(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, inUnionArm, isReturnRoot bool) (*constraintCoercionState, error) {
	canonical, err := coerce(c.bundle, t, in, &coerceFlags{targetIsUnion: inUnionArm}, cctx)
	if err != nil {
		return nil, fmt.Errorf("debaml: constraint-state collector: %s: production coercion did not succeed (the collector models a SUCCESSFUL canonical coercion only): %w", path, err)
	}
	st := &constraintCoercionState{
		Path:          path,
		Type:          t,
		CanonicalJSON: append(json.RawMessage(nil), canonical...),
		Original:      constraintStateRawMetadata{InputKind: in.kind.String()},
	}
	cv, children, err := c.canonicalValue(t, in, cctx, path, canonical, inUnionArm, st)
	if err != nil {
		return nil, err
	}
	st.HasCanonical = true
	st.Canonical = cv
	st.Children = children

	// The divergence check. The state is built from the traversal; these bytes
	// come from production. If they disagree the traversal stopped following
	// production and the state describes a coercion nothing performed.
	got, err := cv.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("debaml: constraint-state collector: %s: serialize canonical value: %w", path, err)
	}
	if diff, ok := constraintStateJSONEquivalent(got, canonical); !ok {
		return nil, fmt.Errorf("%w at %s: %s (state %s vs production %s)", errConstraintStateDiverged, path, diff, got, canonical)
	}

	if err := c.evaluate(st, isReturnRoot); err != nil {
		return nil, err
	}
	return st, nil
}

// canonicalValue builds the node's ConstraintValue from the SCHEMA TRAVERSAL and
// returns the child states it produced.
//
// EVERY SUCCESSFUL PRODUCTION ROUTE THROUGH THESE KINDS IS MODELLED, including
// the ones that produce a value from something other than matched input: the
// class single-field implied-key and inferred-object absorptions, a required
// field filled from TypeIR::default_value, the present-map-non-object {} fill,
// the list SingleToArray wrap, and coerce_array_to_singular for an ARRAY into a
// class. Those are successful coercions with real canonical values, so leaving
// them stateless would make the witness blind exactly where a value came from
// somewhere other than the obvious place.
//
// The refusal ([errConstraintStateUnmodelled]) is reserved for TYPE KINDS
// outside §2's node list — recursive aliases, tuples, arrows, the top type and
// media primitives. Those are not "a route this skipped": schema.ValidateOutput
// rejects tuple/arrow/top/media before parsing, and a recursive alias is a
// separate admitted family with its own scored coercer (alias_coerce.go) whose
// canonicalization the collector would have to re-derive rather than delegate.
func (c *constraintStateCollector) canonicalValue(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, canonical json.RawMessage, inUnionArm bool, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	switch t.Kind {
	case schema.TypePrimitive:
		v, err := c.primitiveValue(t, canonical, path)
		return v, nil, err
	case schema.TypeLiteral:
		v, err := c.literalValue(t, path)
		return v, nil, err
	case schema.TypeEnum:
		return c.enumValue(t, in, path, st)
	case schema.TypeClass:
		return c.classValue(t, in, cctx, path, inUnionArm, st)
	case schema.TypeList:
		return c.listValue(t, in, cctx, path, st)
	case schema.TypeMap:
		return c.mapValue(t, in, cctx, path, st)
	case schema.TypeUnion:
		return c.unionValue(t, in, cctx, path, st)
	default:
		return ConstraintValue{}, nil, fmt.Errorf("%w: %s: type kind %q", errConstraintStateUnmodelled, path, t.Kind)
	}
}

// primitiveValue reads a scalar leaf back from its OWN one-token coercion
// result. The BAML value domain the primitive maps onto is chosen by the SCHEMA
// (int -> BamlValue::Int, float -> Float), never guessed from the token, so an
// integral float stays a Float and an int stays an Int.
func (c *constraintStateCollector) primitiveValue(t schema.Type, canonical json.RawMessage, path constraintStatePath) (ConstraintValue, error) {
	switch t.Primitive {
	case schema.PrimitiveNull:
		if !bytes.Equal(bytes.TrimSpace(canonical), []byte("null")) {
			return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: null primitive coerced to %s", path, canonical)
		}
		return NullValue(), nil
	case schema.PrimitiveBool:
		b, err := constraintStateReadBool(canonical)
		if err != nil {
			return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		return BoolValue(b), nil
	case schema.PrimitiveInt:
		n, err := constraintStateReadInt(canonical)
		if err != nil {
			return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		return IntValue(n), nil
	case schema.PrimitiveFloat:
		f, err := constraintStateReadFloat(canonical)
		if err != nil {
			return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		return FloatValue(f), nil
	case schema.PrimitiveString:
		s, err := constraintStateReadString(canonical)
		if err != nil {
			return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		return StringValue(s), nil
	default:
		// Media: schema.Bundle.ValidateOutput rejects it before parsing, so no
		// media value can reach a predicate on the native path.
		return ConstraintValue{}, fmt.Errorf("%w: %s: primitive %q", errConstraintStateUnmodelled, path, t.Primitive)
	}
}

// literalValue takes the value from the LITERAL TYPE itself. A literal that
// coerced successfully IS its declared value — there is nothing to read back.
func (c *constraintStateCollector) literalValue(t schema.Type, path constraintStatePath) (ConstraintValue, error) {
	if t.Literal == nil {
		return ConstraintValue{}, fmt.Errorf("debaml: constraint-state collector: %s: literal type missing payload", path)
	}
	switch t.Literal.Kind {
	case schema.LiteralString:
		return StringValue(t.Literal.String), nil
	case schema.LiteralInt:
		return IntValue(t.Literal.Int), nil
	case schema.LiteralBool:
		return BoolValue(t.Literal.Bool), nil
	default:
		return ConstraintValue{}, fmt.Errorf("%w: %s: literal kind %q", errConstraintStateUnmodelled, path, t.Literal.Kind)
	}
}

// enumValue reproduces coerceEnum's decision with production's own helpers and
// builds BamlValue::Enum(enum name, CANONICAL variant).
//
// The predicate therefore sees the canonical variant, never the alias the model
// wrote — BAML serializes BamlValue::Enum(_, v) as v, and v is the canonical
// name. The alias that routed here is retained in Original for the witness.
func (c *constraintStateCollector) enumValue(t schema.Type, in value, path constraintStatePath, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	e, ok := c.bundle.FindEnum(t.Name)
	if !ok {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: unknown enum %q", path, t.Name)
	}
	observed, err := stringForMatch(in, &coerceFlags{})
	if err != nil {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: enum match input: %w", path, err)
	}
	variant, outcome, _ := matchString(observed, enumMatchCandidates(e), true)
	if outcome != matchOne {
		// coerce already SUCCEEDED for this node, so match_string must have had
		// exactly one winner. Anything else means the collector is not running the
		// same matcher production ran.
		return ConstraintValue{}, nil, fmt.Errorf("%w at %s: enum %q: match_string outcome %v for %q after a successful coercion", errConstraintStateDiverged, path, t.Name, outcome, observed)
	}
	rendered := variant
	for i := range e.Values {
		if e.Values[i].Name.Name == variant {
			rendered = e.Values[i].Name.RenderedName()
			break
		}
	}
	if rendered != variant || observed != variant {
		st.Original.EnumAlias = &constraintStateAliasOrigin{Canonical: variant, Rendered: rendered, Observed: observed}
	}
	return EnumValue(e.Name.Name, variant), nil, nil
}

// classValue reproduces coerceClass and builds
// BamlValue::Class(name, SCHEMA-ORDER canonical fields) after the child
// coercions succeed.
//
// It covers every route coerce_class can SUCCEED through, because each of them
// produces a real canonical value that a constraint then runs against:
//
//   - ARRAY input -> coerce_array_to_singular ([classArrayValue]);
//   - the single-field OBJECT implied-key and SCALAR/null inferred-object
//     absorptions, where the WHOLE input becomes the lone field's value;
//   - matched fields, assigned by production's own [matchesStringToString] in
//     INPUT order (first match wins a field, first key keeps it, the rest are
//     extras);
//   - an ABSENT OPTIONAL — a skipped child path with NO synthesized value,
//     matching production's omission of the key;
//   - an absent required field filled from TypeIR::default_value, and a PRESENT
//     map field whose non-object value coerce_map refuses and coerce_class fills
//     with {} — both recorded as full states carrying [constraintStateDefaultOrigin].
func (c *constraintStateCollector) classValue(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, inUnionArm bool, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	cls, ok := c.bundle.FindClass(t.Name, t.Mode)
	if !ok {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: unknown class %q", path, t.Name)
	}
	// coerce_class derives the circular-reference child context BEFORE the array
	// branch, so both branches descend under the same guard production uses.
	child := cctx.enterCoerce(schema.ClassKey{Name: t.Name, Mode: t.Mode}, in)
	if in.kind == valArray {
		return c.classArrayValue(t, cls, in.arrV, child, path, inUnionArm, st)
	}

	nF := len(cls.Fields)
	matched := make([]bool, nF)
	assigned := make([]value, nF)
	observed := make([]string, nF)
	extraCount := 0
	if in.kind == valObject {
		// coerce_class's assignment pass, in INPUT order: each key to the FIRST
		// field whose rendered name it fuzzily matches; a key for an already-filled
		// field is a duplicate (keep first); a key matching nothing is an extra.
		for i := range in.objV {
			key := in.objV[i].key
			mf := -1
			for j := range cls.Fields {
				if matchesStringToString(key, cls.Fields[j].Name.RenderedName()) {
					mf = j
					break
				}
			}
			switch {
			case mf < 0:
				extraCount++
			case matched[mf]:
				// Duplicate match for an already-filled field: keep first, ignore.
			default:
				assigned[mf] = in.objV[i].val
				observed[mf] = key
				matched[mf] = true
			}
		}
	}

	// The two single-field absorptions (coerce_class.rs:224 and :295). For a
	// single-field class `!matched[0]` IS coerce_class's `!found_any`. A
	// multi-field class fed a scalar assigns nothing and falls entirely to the
	// default pass below, exactly as BAML's Some(x) arm does.
	if nF == 1 && !matched[0] {
		inferred := in.kind != valObject
		if inferred || extraCount > 0 {
			matched[0] = true
			assigned[0] = in
			observed[0] = cls.Fields[0].Name.Name // absorbed, not key-routed: no alias
			st.Original.Implied = &constraintStateImpliedOrigin{Field: cls.Fields[0].Name.Name, Inferred: inferred}
		}
	}

	entries := make([]ConstraintEntry, 0, nF)
	seen := make(map[string]struct{}, nF)
	children := make([]*constraintCoercionState, 0, nF)
	for j := range cls.Fields {
		f := &cls.Fields[j]
		fieldPath := path.descend(constraintStatePathSegment{Kind: constraintPathField, Name: f.Name.Name})
		if _, dup := seen[f.Name.Name]; dup {
			// A schema with two identically-named fields would silently lose one
			// through any map-shaped builder. Refuse BEFORE descending: coercing and
			// EVALUATING the duplicate first would report the child's own error (or
			// waste the evaluation) instead of the refusal this guard exists for.
			return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: class %q declares field %q twice", path, t.Name, f.Name.Name)
		}
		seen[f.Name.Name] = struct{}{}
		var cs *constraintCoercionState
		switch {
		case !matched[j] && isOptional(f.Type):
			// Absent optional: production OMITS the key (InjectAbsentOptionals adds
			// the null downstream), so the canonical class value omits it too. The
			// path is recorded as a skipped child with NO value.
			children = append(children, c.skippedPath(f.Type, fieldPath,
				fmt.Sprintf("absent optional field %q (OptionalDefaultFromNoValue; production omits the key)", f.Name.Name)))
			continue
		case !matched[j]:
			// Absent required field -> TypeIR::default_value (DefaultFromNoValue).
			// A non-defaultable one is error_missing_required_field, which makes the
			// whole class coercion fail, so node()'s step 1 already returned.
			d, ok := defaultValue(f.Type)
			if !ok {
				return ConstraintValue{}, nil, fmt.Errorf("%w at %s: class %q required field %q is absent and non-defaultable after a successful coercion", errConstraintStateDiverged, path, t.Name, f.Name.Name)
			}
			n, err := c.defaultFilledNode(f.Type, fieldPath, "DefaultFromNoValue", "", d)
			if err != nil {
				return ConstraintValue{}, nil, err
			}
			cs = n
		case f.Type.Kind == schema.TypeMap && assigned[j].kind != valObject:
			// coerce_map is error_unexpected_type on a non-object, so coerce_class
			// fills the map default {} (DefaultButHadUnparseableValue). The field has
			// a real canonical value; only its provenance differs.
			n, err := c.defaultFilledNode(f.Type, fieldPath, "DefaultButHadUnparseableValue", assigned[j].kind.String(), json.RawMessage("{}"))
			if err != nil {
				return ConstraintValue{}, nil, err
			}
			cs = n
		default:
			n, err := c.node(f.Type, assigned[j], child, fieldPath, false, false)
			if err != nil {
				return ConstraintValue{}, nil, err
			}
			cs = n
		}
		children = append(children, cs)
		entries = append(entries, ConstraintEntry{Key: f.Name.Name, Value: cs.Canonical})
		if matched[j] && (observed[j] != f.Name.Name || f.Name.RenderedName() != f.Name.Name) {
			st.Original.FieldAliases = append(st.Original.FieldAliases, constraintStateAliasOrigin{
				Canonical: f.Name.Name,
				Rendered:  f.Name.RenderedName(),
				Observed:  observed[j],
			})
		}
	}
	return ClassValue(cls.Name.Name, entries), children, nil
}

// classArrayValue reproduces coerce_class's ARRAY branch through production's
// own [coerceArrayToSingular], so the winning item is the item PRODUCTION
// picked, and collects state for that item only.
//
// Every other item lost the pick_best ranking (or was excluded as a proven BAML
// error) and gets no state, for the same reason a losing union arm gets none:
// there is no coercion of it in the result.
func (c *constraintStateCollector) classArrayValue(t schema.Type, cls *schema.ClassDef, items []value, cctx *coerceCtx, path constraintStatePath, inUnionArm bool, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	if !classAllRequiredFlatLeaf(cls) {
		// coerceClassArray itself declines this, so node()'s step 1 already
		// returned; the guard is here so the reason is stated at the traversal too.
		return ConstraintValue{}, nil, fmt.Errorf("%w: %s: class %q array-to-singular is claimed only for a multi-field all-required-flat-leaf class", errConstraintStateUnmodelled, path, t.Name)
	}
	classType := schema.Type{Kind: schema.TypeClass, Name: t.Name, Mode: t.Mode}
	w, err := coerceArrayToSingular(items, inUnionArm, func(item value) (json.RawMessage, *coerceFlags, error) {
		itemCf := &coerceFlags{targetIsUnion: inUnionArm}
		out, e := coerce(c.bundle, classType, item, itemCf, cctx)
		return out, itemCf, e
	}, nil)
	if err != nil {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
	}
	if w.originIndex < 0 || w.originIndex >= len(items) {
		return ConstraintValue{}, nil, fmt.Errorf("%w at %s: array-to-singular winner index %d outside [0,%d)", errConstraintStateDiverged, path, w.originIndex, len(items))
	}
	st.Original.ArrayToSingular = &constraintStateArrayOrigin{Index: w.originIndex, Items: len(items)}
	itemPath := path.descend(constraintStatePathSegment{Kind: constraintPathIndex, Index: w.originIndex})
	cs, err := c.node(classType, items[w.originIndex], cctx, itemPath, inUnionArm, false)
	if err != nil {
		return ConstraintValue{}, nil, err
	}
	return cs.Canonical, []*constraintCoercionState{cs}, nil
}

// defaultConstraintValue mirrors production [defaultValue]'s VALUE domain: the
// BAML value BAML fills for a field with no usable input.
//
// It is a mirror of the DOMAIN, not a decode of the bytes — list is an empty
// BamlValue::List, map an empty BamlValue::Map, and a union resolves to its
// first defaultable arm exactly as TypeIR::default_value does. The caller still
// checks the result against production's own default bytes, so a divergence
// between the two mirrors fails rather than passes.
func (c *constraintStateCollector) defaultConstraintValue(t schema.Type, path constraintStatePath) (ConstraintValue, error) {
	switch t.Kind {
	case schema.TypeList:
		return ListValue(nil), nil
	case schema.TypeMap:
		return MapValue(nil), nil
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveNull {
			return NullValue(), nil
		}
	case schema.TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if _, ok := defaultValue(t.Union.Variants[i]); ok {
					return c.defaultConstraintValue(t.Union.Variants[i], path)
				}
			}
			if t.Union.Nullable {
				return NullValue(), nil
			}
		}
	}
	return ConstraintValue{}, fmt.Errorf("%w: %s: type kind %q has no modelled default", errConstraintStateUnmodelled, path, t.Kind)
}

// defaultFilledNode builds the full state of a field whose value came from a
// default.
//
// It is a complete node, not a marker: the default IS the field's canonical
// value, so its predicates run against it exactly as any other node's do. The
// provenance lives in [constraintStateRawMetadata.DefaultFill] rather than in a
// special disposition, because nothing about the EVALUATION is special —
// inventing a fourth "skipped" arm here would claim an asymmetry no stock
// witness supports.
//
// canonical is production's own default bytes, and the same divergence check
// every other node runs applies.
func (c *constraintStateCollector) defaultFilledNode(t schema.Type, path constraintStatePath, rule, observedKind string, canonical json.RawMessage) (*constraintCoercionState, error) {
	cv, err := c.defaultConstraintValue(t, path)
	if err != nil {
		return nil, err
	}
	st := &constraintCoercionState{
		Path:          path,
		Type:          t,
		HasCanonical:  true,
		Canonical:     cv,
		CanonicalJSON: append(json.RawMessage(nil), canonical...),
		Original: constraintStateRawMetadata{
			InputKind:   observedKind,
			DefaultFill: &constraintStateDefaultOrigin{Rule: rule, ObservedKind: observedKind},
		},
	}
	if t.Kind == schema.TypeUnion && t.Union != nil {
		// Record WHICH arm TypeIR::default_value resolved to, so a defaulted union
		// field is as legible as a coerced one.
		origin := &constraintStateUnionOrigin{Index: len(t.Union.Variants), NullArm: true, Variants: len(t.Union.Variants)}
		for i := range t.Union.Variants {
			if _, ok := defaultValue(t.Union.Variants[i]); ok {
				origin = &constraintStateUnionOrigin{Index: i, NullArm: false, Variants: len(t.Union.Variants)}
				break
			}
		}
		st.Original.Union = origin
	}
	got, err := cv.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("debaml: constraint-state collector: %s: serialize default value: %w", path, err)
	}
	if diff, ok := constraintStateJSONEquivalent(got, canonical); !ok {
		return nil, fmt.Errorf("%w at %s: %s (default state %s vs production %s)", errConstraintStateDiverged, path, diff, got, canonical)
	}
	if err := c.evaluate(st, false); err != nil {
		return nil, err
	}
	return st, nil
}

// listValue builds BamlValue::List after production's skip/drop rules settle:
// [coerceListChild] decides keep-vs-skip, so a dropped element is dropped for
// exactly the reason production dropped it.
func (c *constraintStateCollector) listValue(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	if t.Elem == nil {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: list type missing element", path)
	}
	items := in.arrV
	if in.kind != valArray {
		// BAML wraps a non-array as one implied element (SingleToArray).
		st.Original.SingleToArray = true
		items = []value{in}
	}
	values := make([]ConstraintValue, 0, len(items))
	children := make([]*constraintCoercionState, 0, len(items))
	for i := range items {
		itemPath := path.descend(constraintStatePathSegment{Kind: constraintPathIndex, Index: i})
		_, keep, err := coerceListChild(c.bundle, *t.Elem, items[i], &coerceFlags{}, cctx)
		if err != nil {
			return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: %w", itemPath, err)
		}
		if !keep {
			children = append(children, c.skippedPath(*t.Elem, itemPath,
				"element dropped by BAML's ArrayItemParseError partial-array skip"))
			continue
		}
		cs, err := c.node(*t.Elem, items[i], cctx, itemPath, false, false)
		if err != nil {
			return ConstraintValue{}, nil, err
		}
		children = append(children, cs)
		values = append(values, cs.Canonical)
	}
	return ListValue(values), children, nil
}

// mapValue builds BamlValue::Map after key coercion and canonical order settle:
// entries keep the ORIGINAL input key string in INPUT order, which is what
// coerce_map emits and what BAML's IndexMap preserves.
//
// The map's KEY TYPE gets its own node. Map-key constraints stay a
// negative-admission fixture, so if any is declared the key node records
// constraintDispositionPolicyDeclined with the declined predicates listed —
// present in the witness, never evaluated.
func (c *constraintStateCollector) mapValue(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	if t.Key == nil || t.Value == nil {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: map type missing key or value", path)
	}
	if in.kind != valObject {
		return ConstraintValue{}, nil, fmt.Errorf("%w: %s: map from a %s input", errConstraintStateUnmodelled, path, in.kind)
	}
	entries := make([]ConstraintEntry, 0, len(in.objV))
	children := make([]*constraintCoercionState, 0, len(in.objV)+1)
	if len(c.attached(*t.Key)) > 0 {
		children = append(children, c.policyDeclined(*t.Key,
			path.descend(constraintStatePathSegment{Kind: constraintPathMapKeyType}),
			"map-key constraints stay a negative-admission fixture in Slice 7.2a"))
	}
	seen := make(map[string]struct{}, len(in.objV))
	for i := range in.objV {
		f := &in.objV[i]
		entryPath := path.descend(constraintStatePathSegment{Kind: constraintPathMapEntry, Name: f.key})
		if _, dup := seen[f.key]; dup {
			// coerce_map declines a duplicate input key outright, so a successful
			// coercion cannot have one. Refuse rather than overwrite.
			return ConstraintValue{}, nil, fmt.Errorf("%w at %s: duplicate input key %q survived coerce_map", errConstraintStateDiverged, entryPath, f.key)
		}
		seen[f.key] = struct{}{}
		_, keep, err := coerceMapValueChild(c.bundle, *t.Value, f.val, &coerceFlags{}, cctx)
		if err != nil {
			return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: %w", entryPath, err)
		}
		if !keep {
			children = append(children, c.skippedPath(*t.Value, entryPath,
				"entry dropped by BAML's MapValueParseError partial-map skip"))
			continue
		}
		cs, err := c.node(*t.Value, f.val, cctx, entryPath, false, false)
		if err != nil {
			return ConstraintValue{}, nil, err
		}
		children = append(children, cs)
		entries = append(entries, ConstraintEntry{Key: f.key, Value: cs.Canonical})
	}
	return MapValue(entries), children, nil
}

// unionValue resolves the union through PRODUCTION's own selector
// ([tryCastUnion] / [selectUnionArms], in coerceUnionSafe's three-case order)
// and collects state for the WINNING ARM ONLY. Losing candidates and the
// score-110 null candidate never get a state — synthesizing one would describe a
// coercion that did not happen.
func (c *constraintStateCollector) unionValue(t schema.Type, in value, cctx *coerceCtx, path constraintStatePath, st *constraintCoercionState) (ConstraintValue, []*constraintCoercionState, error) {
	u := t.Union
	if u == nil {
		return ConstraintValue{}, nil, fmt.Errorf("debaml: constraint-state collector: %s: union type missing payload", path)
	}
	idx, isNull, err := c.unionWinner(u, in, cctx, path)
	if err != nil {
		return ConstraintValue{}, nil, err
	}
	st.Original.Union = &constraintStateUnionOrigin{Index: idx, NullArm: isNull, Variants: len(u.Variants)}
	if isNull {
		// The null arm carries no variant to descend into: BAML's null is the value.
		return NullValue(), nil, nil
	}
	armPath := path.descend(constraintStatePathSegment{Kind: constraintPathUnionArm, Index: idx})
	cs, err := c.node(u.Variants[idx], in, cctx, armPath, true, false)
	if err != nil {
		return ConstraintValue{}, nil, err
	}
	return cs.Canonical, []*constraintCoercionState{cs}, nil
}

// unionWinner mirrors coerceUnionSafe's case split exactly, calling the same
// selectors, and reports which arm production picked.
func (c *constraintStateCollector) unionWinner(u *schema.UnionType, in value, cctx *coerceCtx, path constraintStatePath) (int, bool, error) {
	// Case 1: the JSON-null fast path.
	if in.kind == valNull {
		if !u.Nullable {
			return 0, false, fmt.Errorf("%w at %s: null input to a non-nullable union after a successful coercion", errConstraintStateDiverged, path)
		}
		return len(u.Variants), true, nil
	}
	// Case 2: the optional shape — a single non-null variant scored against the
	// null arm.
	if len(u.Variants) == 1 {
		if err := checkSupportedType(c.bundle, u.Variants[0]); err != nil {
			return 0, false, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		w, err := selectUnionArms(1, true, func(int) (json.RawMessage, *coerceFlags, error) {
			armF := &coerceFlags{targetIsUnion: true}
			out, e := coerce(c.bundle, u.Variants[0], in, armF, cctx)
			return out, armF, e
		})
		if err != nil {
			return 0, false, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
		}
		return w.originIndex, w.originIndex >= 1, nil
	}
	// Case 3: a multi-variant union — try_cast pass, then the lenient scored pass.
	if err := checkSupportedUnionShape(c.bundle, u); err != nil {
		return 0, false, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
	}
	if w, hit, err := tryCastUnion(c.bundle, u.Variants, in, cctx); err != nil {
		return 0, false, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
	} else if hit {
		return w.originIndex, false, nil
	}
	w, err := selectUnionArms(len(u.Variants), u.Nullable, func(i int) (json.RawMessage, *coerceFlags, error) {
		armF := &coerceFlags{targetIsUnion: true}
		out, e := coerce(c.bundle, u.Variants[i], in, armF, cctx)
		return out, armF, e
	})
	if err != nil {
		return 0, false, fmt.Errorf("debaml: constraint-state collector: %s: %w", path, err)
	}
	return w.originIndex, w.originIndex >= len(u.Variants), nil
}

// skippedPath builds the marker state for a path that did NOT contribute a
// value: no ConstraintValue is synthesized, and the predicates declared there
// are listed as not-run rather than silently omitted.
func (c *constraintStateCollector) skippedPath(t schema.Type, path constraintStatePath, reason string) *constraintCoercionState {
	return &constraintCoercionState{
		Path:        path,
		Type:        t,
		Disposition: constraintDispositionSkippedPath,
		SkipReason:  reason,
		Skipped:     c.attachedAsSkipped(t),
	}
}

// policyDeclined builds the marker state for a constrained shape the profile
// refuses to evaluate.
func (c *constraintStateCollector) policyDeclined(t schema.Type, path constraintStatePath, reason string) *constraintCoercionState {
	return &constraintCoercionState{
		Path:        path,
		Type:        t,
		Disposition: constraintDispositionPolicyDeclined,
		SkipReason:  reason,
		Skipped:     c.attachedAsSkipped(t),
	}
}

// constraintStateAttached is one declared predicate plus where it was declared.
type constraintStateAttached struct {
	Origin     constraintStateOrigin
	Level      schema.ConstraintLevel
	Labeled    bool
	Label      string
	Expression string
}

// attached lists the predicates declared at a node, in DECLARATION ORDER:
// the class/enum declaration's own constraints first, then the type node's.
//
// The two sources are kept distinguishable by [constraintStateEvent.Origin]
// rather than merged, because their RELATIVE order is a wire/declaration
// question Slice 7.2a-3's stock oracle answers; 7.2a-2 records both, in a fixed
// and stated order, and folds neither away.
func (c *constraintStateCollector) attached(t schema.Type) []constraintStateAttached {
	var out []constraintStateAttached
	add := func(origin constraintStateOrigin, cs []schema.Constraint) {
		for i := range cs {
			a := constraintStateAttached{
				Origin:     origin,
				Level:      cs[i].Level,
				Expression: cs[i].Expression,
			}
			if cs[i].Label != nil {
				a.Labeled = true
				a.Label = *cs[i].Label
			}
			out = append(out, a)
		}
	}
	switch t.Kind {
	case schema.TypeClass:
		if cls, ok := c.bundle.FindClass(t.Name, t.Mode); ok {
			add(constraintOriginDeclaration, cls.Constraints)
		}
	case schema.TypeEnum:
		if e, ok := c.bundle.FindEnum(t.Name); ok {
			add(constraintOriginDeclaration, e.Constraints)
		}
	}
	add(constraintOriginTypeMeta, t.Meta.Constraints)
	return out
}

// attachedAsSkipped lists a node's predicates as not-run, with no
// counterfactual: a skipped/declined node has no canonical value, and
// evaluating against a fabricated one would be worse than recording nothing.
func (c *constraintStateCollector) attachedAsSkipped(t schema.Type) []constraintStateSkipped {
	att := c.attached(t)
	if len(att) == 0 {
		return nil
	}
	out := make([]constraintStateSkipped, 0, len(att))
	for _, a := range att {
		out = append(out, constraintStateSkipped{
			Origin: a.Origin, Level: a.Level, Labeled: a.Labeled,
			Label: a.Label, Expression: a.Expression,
		})
	}
	return out
}

// constraintStateIsBareStringReturn reports the route condition for asymmetry 1:
// the function's RETURN TYPE is the bare `string` primitive.
//
// It is deliberately narrow. `string?` is a union and is NOT this route, and a
// string nested anywhere inside a class/list/map is not either — which is what
// makes the asymmetry observable at all: the same constrained string type
// evaluates normally one level down.
func constraintStateIsBareStringReturn(t schema.Type) bool {
	return t.Kind == schema.TypePrimitive && t.Primitive == schema.PrimitiveString
}

// evaluate runs the node's predicates and sets its disposition.
func (c *constraintStateCollector) evaluate(st *constraintCoercionState, isReturnRoot bool) error {
	att := c.attached(st.Type)
	if len(att) == 0 {
		st.Disposition = constraintDispositionUnconstrained
		return nil
	}
	if isReturnRoot && constraintStateIsBareStringReturn(st.Type) {
		// ASYMMETRY 1, retained BEFORE normal node evaluation. Stock skips
		// constraints on a bare string return: the check collection comes back
		// empty and a FALSE assertion does not reject the value. The counterfactual
		// is recorded so the skip is evidence of a reached-and-not-applied
		// decision, not an absence.
		st.Disposition = constraintDispositionSkipBareStringReturn
		st.SkipReason = "bare string return: stock skips constraint evaluation on this route (checks empty; a false assertion does not reject)"
		for _, a := range att {
			outcome, recorded, err := c.evaluateOne(st, a)
			if err != nil {
				return err
			}
			st.Skipped = append(st.Skipped, constraintStateSkipped{
				Origin: a.Origin, Level: a.Level, Labeled: a.Labeled,
				Label: a.Label, Expression: a.Expression,
				Counterfactual: outcome, CounterfactualErr: recorded,
			})
		}
		return nil
	}
	unsupported := false
	for _, a := range att {
		outcome, recorded, err := c.evaluateOne(st, a)
		if err != nil {
			return err
		}
		// ASYMMETRY 2: one event per DECLARED predicate, appended in order.
		// Duplicate labels produce two events and are never folded by label.
		st.Events = append(st.Events, constraintStateEvent{
			Origin: a.Origin, Level: a.Level, Labeled: a.Labeled,
			Label: a.Label, Expression: a.Expression,
			Outcome: outcome, Err: recorded,
		})
		if outcome == constraintOutcomeUnsupported {
			unsupported = true
		}
		if a.Level == schema.ConstraintAssert && outcome == constraintOutcomeFalse {
			st.AssertFailed = true
		}
	}
	if unsupported {
		st.Disposition = constraintDispositionUnsupportedExpression
	} else {
		st.Disposition = constraintDispositionEvaluated
	}
	return nil
}

// evaluateOne runs ONE predicate through the production evaluator seam.
//
// It returns (outcome, recordedErr, hardErr). EvaluateConstraint's fail-closed
// contract is that EVERY error wraps ErrConstraintUnsupported, so an error that
// does not is a contract violation and becomes a HARD failure — never an event
// with a swallowed cause.
func (c *constraintStateCollector) evaluateOne(st *constraintCoercionState, a constraintStateAttached) (constraintStateOutcome, error, error) {
	ok, err := EvaluateConstraint(st.Canonical, a.Expression)
	switch {
	case err == nil && ok:
		return constraintOutcomeTrue, nil, nil
	case err == nil:
		return constraintOutcomeFalse, nil, nil
	case errors.Is(err, ErrConstraintUnsupported):
		return constraintOutcomeUnsupported, err, nil
	default:
		return "", nil, fmt.Errorf("debaml: constraint-state collector: %s: evaluator returned a non-sentinel error for %q: %w", st.Path, a.Expression, err)
	}
}

// ---------------------------------------------------------------------------
// Strict scalar readback
// ---------------------------------------------------------------------------

// constraintStateExpectEOF fails unless the decoder is exhausted, so a leaf
// readback cannot silently accept "value + trailing junk".
func constraintStateExpectEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing data after the scalar leaf")
		}
		return fmt.Errorf("unexpected trailing data after the scalar leaf: %w", err)
	}
	return nil
}

// constraintStateDecoder builds the strict decoder every readback uses:
// unknown struct fields are rejected and numbers keep their source spelling.
func constraintStateDecoder(raw []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	return dec
}

func constraintStateReadString(raw []byte) (string, error) {
	dec := constraintStateDecoder(raw)
	var s string
	if err := dec.Decode(&s); err != nil {
		return "", fmt.Errorf("read string leaf %s: %w", raw, err)
	}
	return s, constraintStateExpectEOF(dec)
}

func constraintStateReadBool(raw []byte) (bool, error) {
	dec := constraintStateDecoder(raw)
	var b bool
	if err := dec.Decode(&b); err != nil {
		return false, fmt.Errorf("read bool leaf %s: %w", raw, err)
	}
	return b, constraintStateExpectEOF(dec)
}

func constraintStateReadNumber(raw []byte) (json.Number, error) {
	dec := constraintStateDecoder(raw)
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return "", fmt.Errorf("read number leaf %s: %w", raw, err)
	}
	return n, constraintStateExpectEOF(dec)
}

func constraintStateReadInt(raw []byte) (int64, error) {
	n, err := constraintStateReadNumber(raw)
	if err != nil {
		return 0, err
	}
	// BAML's Int is an i64 and coercePrimitiveInt emits strconv.FormatInt, so a
	// token that is not an exact i64 means the leaf was not an int after all.
	v, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("int leaf %s is not an exact i64: %w", raw, err)
	}
	return v, nil
}

func constraintStateReadFloat(raw []byte) (float64, error) {
	n, err := constraintStateReadNumber(raw)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return 0, fmt.Errorf("float leaf %s is not an f64: %w", raw, err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Renderings used by the assertions
// ---------------------------------------------------------------------------

// constraintStateDescribe renders a ConstraintValue as a single string carrying
// everything the JSON drops: the BAML variant, the class/enum NAME, and the
// ordered entries.
//
//	class:Probe{b=int:1,a=string:"x"}
//	enum:Suit=Hearts
//	list[int:1,int:3]
//	map{b=int:1,a=int:2}
//
// One equality against this string pins kind, identity, order and every leaf at
// once, which is what makes an assertion over a coercion state discriminating
// rather than a shape check.
func constraintStateDescribe(v ConstraintValue) string {
	switch v.kind {
	case ConstraintKindNull:
		return "null"
	case ConstraintKindBool:
		return "bool:" + strconv.FormatBool(v.b)
	case ConstraintKindInt:
		return "int:" + strconv.FormatInt(v.i, 10)
	case ConstraintKindFloat:
		return "float:" + strconv.FormatFloat(v.f, 'g', -1, 64)
	case ConstraintKindString:
		return "string:" + strconv.Quote(v.s)
	case ConstraintKindEnum:
		return "enum:" + v.name + "=" + v.s
	case ConstraintKindList:
		parts := make([]string, len(v.list))
		for i := range v.list {
			parts[i] = constraintStateDescribe(v.list[i])
		}
		return "list[" + strings.Join(parts, ",") + "]"
	case ConstraintKindMap, ConstraintKindClass:
		parts := make([]string, len(v.entries))
		for i := range v.entries {
			parts[i] = v.entries[i].Key + "=" + constraintStateDescribe(v.entries[i].Value)
		}
		prefix := "map{"
		if v.kind == ConstraintKindClass {
			prefix = "class:" + v.name + "{"
		}
		return prefix + strings.Join(parts, ",") + "}"
	default:
		return "kind:" + v.kind.String()
	}
}

// describe renders one event as `origin/level/label/expression=outcome`, with an
// unlabelled constraint rendered as `-`. Labels are NOT deduplicated, so two
// events sharing a label render as two distinct strings in order.
func (e constraintStateEvent) describe() string {
	label := "-"
	if e.Labeled {
		label = strconv.Quote(e.Label)
	}
	return fmt.Sprintf("%s/%s/%s/%s=%s", e.Origin, e.Level, label, e.Expression, e.Outcome)
}

// describe renders one skipped constraint, including the COUNTERFACTUAL outcome
// — the positive evidence that the predicate was reached and would have decided,
// rather than merely being absent.
func (s constraintStateSkipped) describe() string {
	label := "-"
	if s.Labeled {
		label = strconv.Quote(s.Label)
	}
	cf := string(s.Counterfactual)
	if cf == "" {
		cf = "not-evaluated"
	}
	return fmt.Sprintf("%s/%s/%s/%s~would-be-%s", s.Origin, s.Level, label, s.Expression, cf)
}

// ---------------------------------------------------------------------------
// Divergence check
// ---------------------------------------------------------------------------

// constraintStateJSONEquivalent compares the state's serialization against
// production coerce's bytes, ORDER-SENSITIVELY, and describes the first
// difference.
//
// WHY NOT bytes.Equal. Two encoder conventions differ without any semantic
// disagreement: production's [marshalJSON] disables HTML escaping while
// [ConstraintValue.MarshalJSON] goes through encoding/json's default (so `<`
// becomes `<` on one side only), and a float's shortest round-trip spelling
// differs between strconv 'g' and encoding/json's exponent form (`1e-07` vs
// `1e-7`). A byte comparison would fail on those and prove nothing about the
// state.
//
// WHAT IT STILL CATCHES — everything the divergence check exists for. Both
// documents are decoded with production's ORDERED [strictDecode], so object key
// ORDER, key SPELLING, entry COUNT, element ORDER and every scalar VALUE are
// compared exactly. A class emitted in the wrong field order, a dropped element
// kept, a map reordered, an alias emitted instead of a canonical name, or the
// wrong union arm all fail here. Only the two encoding conventions above are
// normalized away, and numbers are compared EXACTLY (big.Rat, not float64 — see
// the valNumber arm of [constraintStateValueDiff]: a float64 fallback would fuse
// adjacent i64 values above 2^53).
func constraintStateJSONEquivalent(state, production []byte) (string, bool) {
	a, err := strictDecode(string(state))
	if err != nil {
		return fmt.Sprintf("state serialization is not decodable: %v", err), false
	}
	b, err := strictDecode(string(production))
	if err != nil {
		return fmt.Sprintf("production output is not decodable: %v", err), false
	}
	return constraintStateValueDiff(a, b, "$")
}

// constraintStateValueDiff reports the first ordered difference between two
// decoded documents, or ("", true) when they agree.
func constraintStateValueDiff(a, b value, at string) (string, bool) {
	if a.kind != b.kind {
		return fmt.Sprintf("%s: kind %s vs %s", at, a.kind, b.kind), false
	}
	switch a.kind {
	case valNull:
		return "", true
	case valBool:
		if a.boolV != b.boolV {
			return fmt.Sprintf("%s: bool %v vs %v", at, a.boolV, b.boolV), false
		}
		return "", true
	case valString:
		if a.strV != b.strV {
			return fmt.Sprintf("%s: string %q vs %q", at, a.strV, b.strV), false
		}
		return "", true
	case valNumber:
		if a.numV == b.numV {
			return "", true
		}
		// EXACT decimal comparison, via big.Rat. A float64 round-trip would be
		// wrong here in the one direction that matters: BAML's Int is an i64, and
		// two ADJACENT exact integers above 2^53 (9007199254740992 vs
		// 9007199254740993) collapse to the same float64 — so the divergence check
		// that guards every node would go green on precisely the large-number
		// surface the guard ledger treats as parity-sensitive. big.Rat compares the
		// values the tokens DENOTE, so it normalizes the intended spelling
		// difference (encoding/json's `1e-7` vs strconv 'g' `1e-07`) and nothing
		// else.
		ar, aok := new(big.Rat).SetString(string(a.numV))
		br, bok := new(big.Rat).SetString(string(b.numV))
		if !aok || !bok || ar.Cmp(br) != 0 {
			return fmt.Sprintf("%s: number %s vs %s", at, a.numV, b.numV), false
		}
		return "", true
	case valArray:
		if len(a.arrV) != len(b.arrV) {
			return fmt.Sprintf("%s: %d elements vs %d", at, len(a.arrV), len(b.arrV)), false
		}
		for i := range a.arrV {
			if d, ok := constraintStateValueDiff(a.arrV[i], b.arrV[i], fmt.Sprintf("%s[%d]", at, i)); !ok {
				return d, false
			}
		}
		return "", true
	case valObject:
		if len(a.objV) != len(b.objV) {
			return fmt.Sprintf("%s: %d entries vs %d", at, len(a.objV), len(b.objV)), false
		}
		for i := range a.objV {
			if a.objV[i].key != b.objV[i].key {
				return fmt.Sprintf("%s: entry %d key %q vs %q (order or spelling)", at, i, a.objV[i].key, b.objV[i].key), false
			}
			if d, ok := constraintStateValueDiff(a.objV[i].val, b.objV[i].val, fmt.Sprintf("%s.%s", at, a.objV[i].key)); !ok {
				return d, false
			}
		}
		return "", true
	default:
		return fmt.Sprintf("%s: unknown kind %s", at, a.kind), false
	}
}
