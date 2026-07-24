package debaml

import "github.com/invakid404/baml-rest/internal/schema"

// De-BAML Phase 3a (recursive ALIASES) — the FINAL-only recursive-alias support
// profile, a sibling of the Phase-2 recursive-CLASS profile (recursive_profile.go).
//
// The native static-final lane serves EXACTLY ONE structural-recursive-alias family:
// the direct five-arm JSON alias
//
//	type JSON = int | string | bool | JSON[] | map<string, JSON>
//
// returned directly (function StaticRecursiveAliasJSON(...) -> JSON). Every other
// alias — a renamed/multi/wrapped alias, a float or explicit-null arm (the wider
// `JsonValue`), a nullable top-level union, a non-direct target, or any
// constraint/dynamic/stream metadata — stays a concrete #583 residual DECLINE. The
// admission fingerprint is GENUINELY EXACT (canonical `JSON` name, the ordered
// non-null arm list, list-elem = map-value = the same bare `JSON`, a bare string map
// key), so a same-shaped alias under a different name, or with an extra/missing arm,
// declines pre-claim.
//
// [IsProvenRecursiveAliasStaticFamily] exports the predicate so the isolated
// nativeserve admission gate stays in EXACT lockstep with the root-owned parser: both
// admit the identical fingerprint, and neither can drift from the other.

// The canonical served alias name. The family IS this exact alias, so the fingerprint
// pins the canonical name, not merely a five-arm SHAPE — a same-shaped
// `type Blob = int | string | bool | Blob[] | map<string, Blob>` is NOT the served
// family and declines pre-claim.
const recAliasJSONName = "JSON"

// recAliasProfile marks an admitted recursive-alias family. The only admitted family
// is the direct five-arm JSON alias, so the profile just carries its canonical name
// (for the coercer to resolve via Bundle.FindRecursiveAlias).
type recAliasProfile struct {
	aliasName string
}

// admittedRecursiveAliasProfile classifies bundle b against the EXACT ratified
// recursive-alias family and returns (profile, true) ONLY for the direct five-arm JSON
// alias returned directly. It returns (‗, false) for a non-alias bundle, any bundle
// mixing classes/enums/recursive-classes, the wider `JsonValue`, and every alias shape
// outside the fingerprint — the caller then keeps the blanket recursive-alias decline
// (checkSupported / checkSupportedType).
func admittedRecursiveAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	if b == nil || len(b.StructuralRecursiveAliases) != 1 {
		return recAliasProfile{}, false
	}
	// EXACTLY one structural recursive alias, and NO classes / enums / recursive
	// classes: a bundle mixing an alias with a class/enum is outside this slice.
	if len(b.Classes) != 0 || len(b.Enums) != 0 || len(b.RecursiveClasses) != 0 {
		return recAliasProfile{}, false
	}
	// Target: a bare `JSON` alias reference — non-dynamic, non-streaming, zero
	// metadata (Meta.IsZero() rejects both constraints AND the {needed,done,state}
	// @stream.* triple), so a constrained / dynamic / streamed direct return declines.
	if !isBareJSONAliasRef(b.Target) {
		return recAliasProfile{}, false
	}
	def := &b.StructuralRecursiveAliases[0]
	if def.Name != recAliasJSONName {
		return recAliasProfile{}, false
	}
	if !isExactJSONAliasTarget(def.Target) {
		return recAliasProfile{}, false
	}
	return recAliasProfile{aliasName: recAliasJSONName}, true
}

// IsProvenRecursiveAliasStaticFamily is the exported lockstep predicate the isolated
// nativeserve admission gate uses so the served fingerprint and the parser profile can
// NEVER diverge (both admit EXACTLY the direct five-arm JSON alias).
func IsProvenRecursiveAliasStaticFamily(b *schema.Bundle) bool {
	_, ok := admittedRecursiveAliasProfile(b)
	return ok
}

// isBareJSONAliasRef reports whether t is exactly a reference to the canonical `JSON`
// alias — a bare, unconstrained, non-dynamic, non-streaming recursive-alias node with
// NO metadata.
func isBareJSONAliasRef(t schema.Type) bool {
	return t.Kind == schema.TypeRecursiveAlias && t.Name == recAliasJSONName &&
		t.Mode == schema.NonStreaming && !t.Dynamic && t.Meta.IsZero()
}

// isExactJSONAliasTarget reports whether t is EXACTLY the ordered five-arm union
//
//	int | string | bool | JSON[] | map<string, JSON>
//
// with zero metadata anywhere, NON-nullable (no `null` arm — that is the wider
// JsonValue), list element = map value = the bare `JSON` alias, and a bare string map
// key. The arm ORDER is pinned (int, string, bool, list, map) so a reordered or
// extra/missing arm declines.
func isExactJSONAliasTarget(t schema.Type) bool {
	if t.Kind != schema.TypeUnion || t.Union == nil || t.Union.Nullable || !t.Meta.IsZero() {
		return false
	}
	vs := t.Union.Variants
	if len(vs) != 5 {
		return false
	}
	// Arms 0-2: bare int / string / bool primitives (no meta, no dynamic).
	if !isBarePrimitive(vs[0], schema.PrimitiveInt) ||
		!isBarePrimitive(vs[1], schema.PrimitiveString) ||
		!isBarePrimitive(vs[2], schema.PrimitiveBool) {
		return false
	}
	// Arm 3: JSON[] — a bare list whose element is the bare `JSON` alias.
	if vs[3].Kind != schema.TypeList || !vs[3].Meta.IsZero() || vs[3].Elem == nil ||
		!isBareJSONAliasRef(*vs[3].Elem) {
		return false
	}
	// Arm 4: map<string, JSON> — a bare map, a bare string key, a bare `JSON` value.
	m := vs[4]
	if m.Kind != schema.TypeMap || !m.Meta.IsZero() || m.Key == nil || m.Value == nil {
		return false
	}
	return isBarePrimitive(*m.Key, schema.PrimitiveString) && isBareJSONAliasRef(*m.Value)
}

// isBarePrimitive reports whether t is a bare primitive of the given kind — no
// metadata (constraints / @stream.*), not dynamic.
func isBarePrimitive(t schema.Type, p schema.PrimitiveKind) bool {
	return t.Kind == schema.TypePrimitive && t.Primitive == p && !t.Dynamic && t.Meta.IsZero()
}
