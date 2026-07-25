package debaml

import "github.com/invakid404/baml-rest/internal/schema"

// De-BAML Phase 3a (recursive ALIASES) — the recursive-alias support profile, a
// sibling of the Phase-2 recursive-CLASS profile (recursive_profile.go).
//
// The native static lane serves EXACTLY TWO structural-recursive-alias families, each
// pinned by its OWN canonical name-exact fingerprint:
//
//	Phase 3a/3b  type JSON      = int | string | bool | JSON[] | map<string, JSON>
//	Phase 3c     type JsonValue = int | float | bool | string | null
//	                            | JsonValue[] | map<string, JsonValue>
//
// each returned directly (StaticRecursiveAliasJSON(...) -> JSON,
// StaticRecursiveAliasJsonValue(...) -> JsonValue).
//
// The two families are served on DIFFERENT sets of lanes, and the predicates here are the
// FINAL-lane ones. `JSON` is served on both the final and the streaming lane; `JsonValue`
// is served on the FINAL lane only. The streaming gate is deliberately narrower — see
// [staticStreamAliasProfile] in static_stream_serve.go — because a claimed stream has no
// route back to BAML, so a family whose parse can decline on a VALUE must not claim a
// stream socket. Callers therefore must NOT treat
// [IsProvenServedRecursiveAliasStaticFamily] as a streaming predicate; the streaming one is
// [IsProvenRecursiveAliasStaticStreamFamily].
//
// Every OTHER alias — a
// renamed/multi/wrapped alias, an extra or reordered arm, a non-direct target, or any
// constraint/dynamic/stream metadata — stays a concrete #583 residual DECLINE. Both
// fingerprints are GENUINELY EXACT (canonical name, the ordered non-null arm list,
// list-elem = map-value = the same bare self-reference, a bare string map key), so a
// same-shaped alias under a different name, or with an extra/missing arm, declines
// pre-claim.
//
// The two families are deliberately kept as TWO exact predicates rather than one
// widened "recursive alias with primitive arms" rule: `JSON` keeps its ORIGINAL
// byte-for-byte predicate (and therefore its original proof), and the JsonValue arms
// (float scoring + first-class null) can never leak into it. The internal classifier
// returns a [recAliasProfile] discriminator so the coercers branch on the PROFILE, not
// on scattered `aliasName == "JsonValue"` string tests.
//
// [IsProvenRecursiveAliasStaticFamily] /
// [IsProvenJsonValueRecursiveAliasStaticFamily] export the two predicates, and
// [IsProvenServedRecursiveAliasStaticFamily] their exact OR, so the isolated
// nativeserve admission gate stays in EXACT lockstep with the root-owned parser: both
// admit the identical fingerprints, and neither can drift from the other.

// The canonical served alias names. A family IS its exact alias, so each fingerprint
// pins the canonical name, not merely an arm SHAPE — a same-shaped
// `type Blob = int | string | bool | Blob[] | map<string, Blob>` is NOT a served
// family and declines pre-claim.
const (
	recAliasJSONName      = "JSON"
	recAliasJsonValueName = "JsonValue"
)

// aliasFamily discriminates WHICH proven recursive-alias family a bundle matched. It
// is the single switch the alias coercers / stream coercers branch on, so the two
// families' semantics (notably `JSON`'s null -> [] trap versus `JsonValue`'s
// first-class typed null) can never be selected by an ad-hoc name comparison.
type aliasFamily uint8

const (
	// aliasFamilyJSON is the Phase-3a/3b five-arm NON-nullable `JSON` alias.
	aliasFamilyJSON aliasFamily = iota
	// aliasFamilyJsonValue is the Phase-3c six-stored-variant NULLABLE `JsonValue`
	// alias (adds the `float` arm and the first-class `null` arm).
	aliasFamilyJsonValue
)

// recAliasProfile describes an admitted recursive-alias family: its canonical name
// (for the coercer to resolve via Bundle.FindRecursiveAlias), its family
// discriminator, and whether the resolved union is NULLABLE — the single fact that
// separates `JSON`'s null -> [] list fallback from `JsonValue`'s typed-null fast path.
type recAliasProfile struct {
	aliasName string
	family    aliasFamily
	nullable  bool
}

// isJsonValue reports whether this profile is the nullable float+null `JsonValue`
// family. Coercers branch on THIS, never on a name string, so the proven `JSON` path
// is mechanically incapable of picking up a JsonValue-only behaviour.
func (p recAliasProfile) isJsonValue() bool { return p.family == aliasFamilyJsonValue }

// aliasBundleShapeOK is the family-INDEPENDENT bundle precondition both fingerprints
// require: a non-nil bundle carrying EXACTLY one structural recursive alias and NO
// classes, enums, or recursive classes (a bundle mixing an alias with a class/enum is
// outside this slice).
//
// It is shared deliberately. The per-family FINGERPRINTS — the canonical name and the
// exact ordered target matcher — stay separate, because keeping them independent is what
// lets `JSON` retain its original predicate and proof. But this bundle-level preamble is
// identical for both families by definition, and duplicating it is precisely the drift
// this file's rationale warns about: a future `schema.Bundle` field that must also be
// rejected would otherwise have to be added in two places, and only one would be.
func aliasBundleShapeOK(b *schema.Bundle) bool {
	return b != nil &&
		len(b.StructuralRecursiveAliases) == 1 &&
		len(b.Classes) == 0 && len(b.Enums) == 0 && len(b.RecursiveClasses) == 0
}

// admittedRecursiveAliasProfile classifies bundle b against the EXACT ratified
// recursive-alias family and returns (profile, true) ONLY for the direct five-arm JSON
// alias returned directly. It returns (‗, false) for a non-alias bundle, any bundle
// mixing classes/enums/recursive-classes, the wider `JsonValue`, and every alias shape
// outside the fingerprint — the caller then keeps the blanket recursive-alias decline
// (checkSupported / checkSupportedType).
func admittedRecursiveAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	if !aliasBundleShapeOK(b) {
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
	return recAliasProfile{aliasName: recAliasJSONName, family: aliasFamilyJSON}, true
}

// admittedJsonValueRecursiveAliasProfile is the Phase-3c twin of
// [admittedRecursiveAliasProfile]: it classifies bundle b against the EXACT ratified
// `JsonValue` family and returns (profile, true) ONLY for the direct
//
//	type JsonValue = int | float | bool | string | null
//	               | JsonValue[] | map<string, JsonValue>
//
// alias returned directly. The lowered form stores `null` as Union.Nullable (NOT a
// seventh variant), followed by EXACTLY the six ordered non-null variants
// int, float, bool, string, list, map — so the arm ORDER is pinned and a reordered /
// extra / missing arm declines. Every other bundle (a non-alias bundle, any bundle
// mixing classes/enums/recursive-classes, the narrower `JSON`, a renamed or wrapped
// alias, anything carrying metadata) returns (‗, false) and keeps the blanket
// recursive-alias decline.
func admittedJsonValueRecursiveAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	if !aliasBundleShapeOK(b) {
		return recAliasProfile{}, false
	}
	// Target: a bare `JsonValue` alias reference — non-dynamic, non-streaming, zero
	// metadata (Meta.IsZero() rejects both constraints AND the {needed,done,state}
	// @stream.* triple), so a constrained / dynamic / streamed direct return declines.
	if !isBareAliasRef(b.Target, recAliasJsonValueName) {
		return recAliasProfile{}, false
	}
	def := &b.StructuralRecursiveAliases[0]
	if def.Name != recAliasJsonValueName {
		return recAliasProfile{}, false
	}
	if !isExactJsonValueAliasTarget(def.Target) {
		return recAliasProfile{}, false
	}
	return recAliasProfile{aliasName: recAliasJsonValueName, family: aliasFamilyJsonValue, nullable: true}, true
}

// admittedServedRecursiveAliasProfile is the SINGLE internal classifier every served
// alias lane routes through: it tries the two exact family fingerprints in turn and
// returns the matching profile. Having one entry point (rather than an OR expression
// re-spelled per lane) is what keeps the root final gate, the root stream gate, the
// isolated nativeserve gate, and the coercers from drifting apart.
func admittedServedRecursiveAliasProfile(b *schema.Bundle) (recAliasProfile, bool) {
	if prof, ok := admittedRecursiveAliasProfile(b); ok {
		return prof, true
	}
	return admittedJsonValueRecursiveAliasProfile(b)
}

// IsProvenRecursiveAliasStaticFamily is the exported lockstep predicate the isolated
// nativeserve admission gate uses so the served fingerprint and the parser profile can
// NEVER diverge (both admit EXACTLY the direct five-arm JSON alias).
//
// It is DELIBERATELY still the narrow `JSON`-only predicate: Phase 3c adds a SECOND
// name-pinned predicate rather than widening this one, so every existing caller,
// regression row, and decline proof that names this function keeps its ORIGINAL
// meaning byte-for-byte. Callers that mean "either served family" use
// [IsProvenServedRecursiveAliasStaticFamily].
func IsProvenRecursiveAliasStaticFamily(b *schema.Bundle) bool {
	_, ok := admittedRecursiveAliasProfile(b)
	return ok
}

// IsProvenJsonValueRecursiveAliasStaticFamily is the Phase-3c exported lockstep
// predicate for the EXACT nullable six-stored-variant `JsonValue` family. It is the
// SECOND canonical name-pinned fingerprint; it never admits `JSON` (which stays on
// [IsProvenRecursiveAliasStaticFamily]) and never admits a wider/renamed alias.
func IsProvenJsonValueRecursiveAliasStaticFamily(b *schema.Bundle) bool {
	_, ok := admittedJsonValueRecursiveAliasProfile(b)
	return ok
}

// IsProvenServedRecursiveAliasStaticFamily is the exported "either served family"
// helper. The isolated nativeserve admission package holds no Bundle semantics of its
// own, so it asks THIS one predicate rather than spelling the OR itself — the two
// admission lanes therefore cannot drift from each other or from the parser.
func IsProvenServedRecursiveAliasStaticFamily(b *schema.Bundle) bool {
	_, ok := admittedServedRecursiveAliasProfile(b)
	return ok
}

// isBareJSONAliasRef reports whether t is exactly a reference to the canonical `JSON`
// alias — a bare, unconstrained, non-dynamic, non-streaming recursive-alias node with
// NO metadata.
func isBareJSONAliasRef(t schema.Type) bool {
	return isBareAliasRef(t, recAliasJSONName)
}

// isBareAliasRef reports whether t is exactly a reference to the recursive alias
// named name — a bare, unconstrained, non-dynamic, non-streaming recursive-alias node
// with NO metadata. It is the name-parameterized core both family fingerprints share;
// pinning the NAME here is what keeps each family to its own canonical alias.
func isBareAliasRef(t schema.Type, name string) bool {
	return t.Kind == schema.TypeRecursiveAlias && t.Name == name &&
		t.Mode == schema.NonStreaming && !t.Dynamic && t.Meta.IsZero()
}

// isExactJsonValueAliasTarget reports whether t is EXACTLY the lowered `JsonValue`
// target: a NULLABLE (the `null` arm) zero-metadata union whose SIX ordered stored
// variants are
//
//	int | float | bool | string | JsonValue[] | map<string, JsonValue>
//
// with zero metadata anywhere, list element = map value = the bare `JsonValue` alias,
// and a bare string map key. The arm ORDER is pinned (int, float, bool, string, list,
// map — note float BEFORE bool/string, matching the lowered descriptor) so a
// reordered or extra/missing arm declines, and Nullable MUST be true (a non-nullable
// six-arm sibling is a different shape and declines).
func isExactJsonValueAliasTarget(t schema.Type) bool {
	if t.Kind != schema.TypeUnion || t.Union == nil || !t.Union.Nullable || !t.Meta.IsZero() {
		return false
	}
	vs := t.Union.Variants
	if len(vs) != 6 {
		return false
	}
	// Arms 0-3: bare int / float / bool / string primitives (no meta, no dynamic).
	if !isBarePrimitive(vs[0], schema.PrimitiveInt) ||
		!isBarePrimitive(vs[1], schema.PrimitiveFloat) ||
		!isBarePrimitive(vs[2], schema.PrimitiveBool) ||
		!isBarePrimitive(vs[3], schema.PrimitiveString) {
		return false
	}
	// Arm 4: JsonValue[] — a bare list whose element is the bare `JsonValue` alias.
	if vs[4].Kind != schema.TypeList || !vs[4].Meta.IsZero() || vs[4].Elem == nil ||
		!isBareAliasRef(*vs[4].Elem, recAliasJsonValueName) {
		return false
	}
	// Arm 5: map<string, JsonValue> — a bare map, a bare string key, a bare value.
	m := vs[5]
	if m.Kind != schema.TypeMap || !m.Meta.IsZero() || m.Key == nil || m.Value == nil {
		return false
	}
	return isBarePrimitive(*m.Key, schema.PrimitiveString) &&
		isBareAliasRef(*m.Value, recAliasJsonValueName)
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
