package admission

import (
	"testing"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 3c — the ISOLATED module's own lockstep proof.
//
// nativeserve/admission holds no Bundle semantics: both of its alias gates delegate to the
// root-owned predicates so the final gate, the stream gate, and the parser can never
// drift. This test pins that delegation from THIS side of the module boundary, INCLUDING
// the Phase-3c asymmetry: the FINAL gate admits both alias families, while the STREAM gate
// admits only `JSON`.
//
// That asymmetry is the load-bearing part. The stream gate admits by descriptor SHAPE
// pre-socket and a claimed native stream has no route back to BAML, so a family whose
// parse can decline on a VALUE must not claim a stream socket; the unary lane repairs the
// same response through BAML parse-only, so it can. Pinning both gates here means neither
// can be widened in this module without the other being considered.

// aliasBundleFor builds one of the two served alias shapes, or a perturbation of it.
func aliasBundleFor(t *testing.T, name string, nullable bool, variants []schema.Type) *schema.Bundle {
	t.Helper()
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeRecursiveAlias, Name: name, Mode: schema.NonStreaming},
		StructuralRecursiveAliases: []schema.RecursiveAliasDef{{
			Name:   name,
			Target: schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Nullable: nullable, Variants: variants}},
		}},
	}
	_ = b.RebuildIndexes()
	return b
}

func aliasRef(name string) *schema.Type {
	return &schema.Type{Kind: schema.TypeRecursiveAlias, Name: name, Mode: schema.NonStreaming}
}

func prim(k schema.PrimitiveKind) schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: k}
}

// jsonVariants is the exact five-arm `JSON` variant list.
func jsonVariants() []schema.Type {
	return []schema.Type{
		prim(schema.PrimitiveInt), prim(schema.PrimitiveString), prim(schema.PrimitiveBool),
		{Kind: schema.TypeList, Elem: aliasRef("JSON")},
		{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: aliasRef("JSON")},
	}
}

// jsonValueVariants is the exact six ordered stored variants of `JsonValue`.
func jsonValueVariants() []schema.Type {
	return []schema.Type{
		prim(schema.PrimitiveInt), prim(schema.PrimitiveFloat), prim(schema.PrimitiveBool), prim(schema.PrimitiveString),
		{Kind: schema.TypeList, Elem: aliasRef("JsonValue")},
		{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: aliasRef("JsonValue")},
	}
}

func TestIsolatedAliasAdmission_BothServedFamilies(t *testing.T) {
	cases := []struct {
		name        string
		bundle      *schema.Bundle
		admit       bool // the FINAL / serve-shape gates
		admitStream bool // the STREAM gate (narrower since Phase 3c)
		why         string
	}{
		{
			name:        "JSON (five-arm, non-nullable)",
			bundle:      aliasBundleFor(t, "JSON", false, jsonVariants()),
			admit:       true,
			admitStream: true,
			why:         "the frozen Phase-3a served family, served on BOTH lanes",
		},
		{
			name:        "JsonValue (six stored variants, nullable)",
			bundle:      aliasBundleFor(t, "JsonValue", true, jsonValueVariants()),
			admit:       true,
			admitStream: false,
			why:         "the Phase-3c family: FINAL-served, STREAM-declined (a value-scoped decline behind a shape-scoped gate has no route back to BAML)",
		},
		{
			name: "JsonValue arms REORDERED (float before int)",
			bundle: func() *schema.Bundle {
				vs := jsonValueVariants()
				vs[0], vs[1] = vs[1], vs[0]
				return aliasBundleFor(t, "JsonValue", true, vs)
			}(),
			admit: false,
			why:   "the ordered variant list is pinned",
		},
		{
			name:   "JsonValue shape under a DIFFERENT name",
			bundle: aliasBundleFor(t, "Blob", true, jsonValueVariants()),
			admit:  false,
			why:    "the canonical alias name is pinned (and the self-references no longer resolve)",
		},
		{
			name:   "JsonValue arms but NON-nullable",
			bundle: aliasBundleFor(t, "JsonValue", false, jsonValueVariants()),
			admit:  false,
			why:    "the null arm (Union.Nullable) is part of the fingerprint",
		},
		{
			name:   "JSON arms but NULLABLE",
			bundle: aliasBundleFor(t, "JSON", true, jsonVariants()),
			admit:  false,
			why:    "the frozen JSON predicate requires a non-nullable union",
		},
		{
			name: "JsonValue plus a seventh arm",
			bundle: func() *schema.Bundle {
				vs := append(jsonValueVariants(), schema.Type{
					Kind:  schema.TypeMap,
					Key:   &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
					Value: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
				})
				return aliasBundleFor(t, "JsonValue", true, vs)
			}(),
			admit: false,
			why:   "the stored-variant COUNT is pinned",
		},
		{
			name: "JsonValue with a constrained float arm",
			bundle: func() *schema.Bundle {
				vs := jsonValueVariants()
				vs[1].Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this > 0"}}
				return aliasBundleFor(t, "JsonValue", true, vs)
			}(),
			admit: false,
			why:   "Meta.IsZero() is required everywhere; constraints stay a decline",
		},
		{
			name: "JsonValue with a @stream.*-annotated target reference",
			bundle: func() *schema.Bundle {
				b := aliasBundleFor(t, "JsonValue", true, jsonValueVariants())
				b.Target.Mode = schema.Streaming
				return b
			}(),
			admit: false,
			why:   "stream annotations stay a decline for both families",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotFinal := isProvenRecursiveAliasStaticReturn(tc.bundle)
			if gotFinal != tc.admit {
				t.Errorf("isProvenRecursiveAliasStaticReturn = %v, want %v (%s)", gotFinal, tc.admit, tc.why)
			}
			// The STREAM gate is NARROWER than the final one: it admits `JSON` only.
			gotStream := admittedStaticStreamReturnShape(tc.bundle)
			if gotStream != tc.admitStream {
				t.Errorf("admittedStaticStreamReturnShape = %v, want %v (%s)", gotStream, tc.admitStream, tc.why)
			}
			// A stream admit without a final admit would be incoherent in either direction.
			if gotStream && !gotFinal {
				t.Errorf("stream-admitted but not final-admitted (%s) — every stream ends in a final", tc.why)
			}
			// And the SERVE-shape gate (which checks the alias families BEFORE its generic
			// alias reject) must reach the same verdict.
			if got := admittedStaticReturnShape(tc.bundle); got != tc.admit {
				t.Errorf("admittedStaticReturnShape = %v, want %v (%s)", got, tc.admit, tc.why)
			}
		})
	}
}
