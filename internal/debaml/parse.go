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
//   - Schemas that cannot be lowered/validated, or that use maps, general
//     (multi-variant) unions, constraints, or recursive aliases.
//   - Raw text whose JSON-looking candidate needs a repair outside the
//     conservative M2a fixing subset — comments, escapes, missing commas,
//     unterminated structures, multiple top-level values, … — which stays
//     BAML's job (see fix.go for the exact claimed subset).
//
// A non-sentinel error is a CLAIMED native parse failure and propagates:
// the response carries no JSON candidate at all, or a decoded value fails
// to coerce against the schema. The native-vs-BAML differential compares
// these claims against BAML, so drift surfaces rather than being masked
// behind a silent fallback.
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

	parsed, outcome := extractCandidate(req.Raw)
	switch outcome {
	case extractNeedsFixing:
		// A JSON-looking candidate exists but needs a repair outside the
		// conservative M2a fixing subset; recovering it stays BAML's job.
		return bamlutils.DeBAMLParseResult{}, unsupported("non-strict JSON syntax outside fixing subset")
	case extractNotFound:
		// No JSON candidate in the response (e.g. truncated mid-value).
		// This is a CLAIMED parse failure — BAML errors here too, so the
		// differential checks error parity rather than masking it.
		return bamlutils.DeBAMLParseResult{}, fmt.Errorf("debaml: no parseable JSON value found in response")
	}

	out, err := coerce(bundle, bundle.Target, parsed)
	if err != nil {
		// Decoded but un-coercible against the schema: a claimed parse
		// failure (BAML's jsonish also rejects it), propagated.
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
		return unsupported("map")
	case schema.TypeRecursiveAlias:
		return unsupported("recursive alias")
	default:
		return unsupported(fmt.Sprintf("type kind %q", t.Kind))
	}
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
