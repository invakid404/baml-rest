package debaml

import (
	"context"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// Parse is the bamlutils.DeBAMLParseFunc implementation: the bounded
// native response parser. It lowers the dynamic output schema, validates
// it, then extracts and decodes a JSON candidate from the raw model text —
// strict first, then a conservative fixing pass (M2a) — and coerces it
// against the schema, returning the flattened dynamic output JSON (no
// DynamicProperties envelope).
//
// The cut-line (see package and method docs below) is deliberately narrow.
// Anything outside it returns bamlutils.ErrDeBAMLParseUnsupported so the
// caller falls back to BAML for that final parse:
//
//   - Stream parses (req.Stream==true) — native stream semantics are M4.
//   - Schemas that cannot be lowered/validated, or that use general
//     (multi-variant) unions, constraints, recursive aliases, or a map
//     whose key/value falls outside the clean M2b map subset (see
//     checkSupportedMapKey and coerceMap). Clean maps — object input,
//     exact key match, in-scope value — are claimed in input key order.
//   - Raw text whose JSON-looking candidate needs a repair outside the
//     conservative M2a fixing subset — comments, escapes, missing commas,
//     unterminated structures, multiple top-level values, … — which stays
//     BAML's job (see fix.go for the exact claimed subset).
//   - Raw text with no cleanly-claimable JSON candidate at all — no
//     JSON-looking content, or only an unterminated/incomplete structure —
//     which BAML may still recover, so native declines rather than claiming
//     a parse error (extraction never claims; only coercion does).
//
// A non-sentinel error is a CLAIMED native parse failure and propagates: a
// decoded value fails to coerce against the schema in a way BAML also
// rejects (e.g. a non-object where a multi-field class is required). The
// native-vs-BAML differential compares these claims against BAML, so drift
// surfaces rather than being masked behind a silent fallback.
func Parse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx // M1 parsing is a local CPU operation; no cancellation points.

	if req.Stream {
		return bamlutils.DeBAMLParseResult{}, unsupported("stream parse")
	}
	if req.OutputSchema == nil {
		return bamlutils.DeBAMLParseResult{}, unsupported("nil output schema")
	}

	bundle, err := schema.FromDynamicOutputSchema(req.OutputSchema, schema.BuildOptions{})
	if err != nil {
		// A schema we cannot lower self-containedly (e.g. references to
		// static baml_src types the dynamic bundle does not carry) is out
		// of M1 scope — fall back to BAML rather than claim a parse error.
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("lower schema", err)
	}
	if err := bundle.ValidateOutput(); err != nil {
		return bamlutils.DeBAMLParseResult{}, unsupportedErr("validate schema", err)
	}
	if err := checkSupported(bundle); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}

	parsed, ok := extractCandidate(req.Raw)
	if !ok {
		// No cleanly-claimable JSON candidate: no JSON-looking content, a
		// repair outside the conservative M2a fixing subset, an unterminated
		// structure, or multiple top-level values. BAML may still recover any
		// of these, so DECLINE (fall back) rather than claim a parse error —
		// "could not find / complete a candidate" is never a claim.
		return bamlutils.DeBAMLParseResult{}, unsupported("no cleanly-claimable JSON candidate")
	}

	out, err := coerce(bundle, bundle.Target, parsed)
	if err != nil {
		// A candidate was decoded but does not coerce against the schema.
		// coerce returns ErrDeBAMLParseUnsupported where the failure is only
		// native being stricter than BAML's lenient coercers (so the caller
		// falls back); any other error is a CLAIMED parse failure BAML would
		// also hit (e.g. missing required field), propagated for parity.
		return bamlutils.DeBAMLParseResult{}, err
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}

// Compile-time assertion that Parse satisfies the public callback type.
var _ bamlutils.DeBAMLParseFunc = Parse

// checkSupported reports whether every type reachable from the lowered
// bundle is inside the M1 coercion cut-line. It walks the synthetic
// target's class and every other reachable class (TypeClass/TypeEnum
// references are leaves here because the bundle lists each reachable
// definition as its own entry), returning a wrapped
// bamlutils.ErrDeBAMLParseUnsupported for the first out-of-scope feature.
//
// Cycles in the schema (a class that references itself) need no special
// handling: coercion is data-driven, so it descends only as deep as the
// finite JSON input and always terminates. Structural recursive aliases
// and explicitly-marked recursive classes are rejected anyway — the
// dynamic lowering never produces them, so their presence signals an
// unexpected shape.
func checkSupported(b *schema.Bundle) error {
	if len(b.StructuralRecursiveAliases) > 0 {
		return unsupported("structural recursive alias")
	}
	if len(b.RecursiveClasses) > 0 {
		return unsupported("recursive class")
	}
	for i := range b.Enums {
		if len(b.Enums[i].Constraints) > 0 {
			return unsupported("enum constraints")
		}
	}
	for i := range b.Classes {
		c := &b.Classes[i]
		if len(c.Constraints) > 0 {
			return unsupported("class constraints")
		}
		for j := range c.Fields {
			if err := checkSupportedType(c.Fields[j].Type); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSupportedType walks one type tree, rejecting the kinds M1 does not
// coerce. Named class/enum references are leaves (their definitions are
// validated via the bundle's class/enum slices).
func checkSupportedType(t schema.Type) error {
	if len(t.Meta.Constraints) > 0 {
		return unsupported("type constraints")
	}
	switch t.Kind {
	case schema.TypePrimitive, schema.TypeLiteral, schema.TypeEnum, schema.TypeClass:
		return nil
	case schema.TypeList:
		if t.Elem == nil {
			return unsupported("list without element")
		}
		return checkSupportedType(*t.Elem)
	case schema.TypeUnion:
		if t.Union == nil {
			return unsupported("union without payload")
		}
		// Only an optional — a nullable union with exactly one non-null
		// variant — is in scope. Any other union needs variant scoring,
		// which is out of M1.
		if !t.Union.Nullable || len(t.Union.Variants) != 1 {
			return unsupported("general union")
		}
		return checkSupportedType(t.Union.Variants[0])
	case schema.TypeMap:
		// M2b CLAIMS clean maps: a JSON-object input coerced under a
		// map-key-safe key type and a value type that itself passes the
		// cut-line. The key must be checked SPECIALLY — not via the general
		// checkSupportedType, which rejects every union — because a
		// non-nullable union of string literals is a legal map key while
		// being an out-of-scope union everywhere else.
		if t.Key == nil || t.Value == nil {
			return unsupported("map without key or value")
		}
		if err := checkSupportedMapKey(*t.Key); err != nil {
			return err
		}
		return checkSupportedType(*t.Value)
	case schema.TypeRecursiveAlias:
		return unsupported("recursive alias")
	default:
		return unsupported(fmt.Sprintf("type kind %q", t.Kind))
	}
}

// checkSupportedMapKey reports whether t is a map key shape M2b coerces by
// EXACT match: a string primitive, an enum, a string literal, or a
// non-nullable union whose recursively-flattened members are all string
// literals. This mirrors BAML's allowed map-key set (coerce_map.rs) and the
// repo's own isValidMapKey schema gate, kept separate from
// checkSupportedType so the legal union-of-string-literals key is not
// caught by the general-union rejection. A constrained key is out of scope.
func checkSupportedMapKey(t schema.Type) error {
	if len(t.Meta.Constraints) > 0 {
		return unsupported("map key constraints")
	}
	switch t.Kind {
	case schema.TypePrimitive:
		if t.Primitive == schema.PrimitiveString {
			return nil
		}
		return unsupported(fmt.Sprintf("map key primitive %q (only string)", t.Primitive))
	case schema.TypeEnum:
		return nil
	case schema.TypeLiteral:
		if t.Literal != nil && t.Literal.Kind == schema.LiteralString {
			return nil
		}
		return unsupported("map key literal must be a string literal")
	case schema.TypeUnion:
		if isStringLiteralUnionType(t) {
			return nil
		}
		return unsupported("map key union must be a non-nullable union of string literals")
	default:
		return unsupported(fmt.Sprintf("map key kind %q", t.Kind))
	}
}

// isStringLiteralUnionType reports whether t is a non-nullable union every
// member of which is a string literal or a nested non-nullable union of
// string literals — the only union shape BAML (and M2b) accept as a map
// key. It mirrors schema.isStringLiteralUnion: the non-nullable requirement
// reproduces jsonish rejecting the null iter_include_null() appends for an
// optional union.
func isStringLiteralUnionType(t schema.Type) bool {
	if t.Union == nil || t.Union.Nullable || len(t.Union.Variants) == 0 {
		return false
	}
	for i := range t.Union.Variants {
		v := &t.Union.Variants[i]
		switch v.Kind {
		case schema.TypeLiteral:
			if v.Literal == nil || v.Literal.Kind != schema.LiteralString {
				return false
			}
		case schema.TypeUnion:
			if !isStringLiteralUnionType(*v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// unsupported wraps bamlutils.ErrDeBAMLParseUnsupported with a reason so
// the caller falls back to BAML while logs/metrics still record why.
func unsupported(reason string) error {
	return fmt.Errorf("%w: %s", bamlutils.ErrDeBAMLParseUnsupported, reason)
}

// unsupportedErr is unsupported with an underlying cause attached.
func unsupportedErr(stage string, cause error) error {
	return fmt.Errorf("%w: %s: %v", bamlutils.ErrDeBAMLParseUnsupported, stage, cause)
}
