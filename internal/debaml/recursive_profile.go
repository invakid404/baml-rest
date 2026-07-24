package debaml

import "github.com/invakid404/baml-rest/internal/schema"

// De-BAML Phase 2 (recursive classes) — the FINAL-only recursive-class support
// profile.
//
// The native static-final lane serves a NARROW recursive-class family: a
// self-recursive class (Node{value string; next Node?}) and a mutually-recursive
// SCC (A{value string; b B?} <-> B{value string; a A?}). The ONLY recursive edge
// shape admitted is a DIRECT nullable single-class reference into the SCC; every
// other union, container recursion, required class edge, structural recursive
// alias, and one-field implied-recursion shape (Loop{next Loop?}) stays a #583
// residual decline.
//
// This profile is the factored predicate the three final consumers share
// (SupportsNativeFinalBundle, ParseStaticBundle, and nativeserve admission's
// SupportsNativeFinalBundle call): admittedRecursiveClassProfile classifies the
// bundle once, and checkStreamRootSupported / checkSupportedRecursive exempt EXACTLY
// the admitted edges (and only those) from the blanket cycle/union stream declines.
// SupportsNativeStreamBundle passes a nil profile, so the stream lane keeps its
// blanket cycle/union declines unchanged. A non-recursive bundle (RecursiveClasses
// empty) also yields a nil profile → the UNCHANGED non-recursive cut-line runs, so
// the dynamic 289 pin and the 8C static pin stay byte-for-byte unchanged.

// recProfile describes an admitted recursive-class family: the set of ClassKeys
// that participate in the admitted recursive SCC(s). By construction of
// [admittedRecursiveClassProfile], every recursive edge in the graph is a direct
// optional single-class reference into this set (a TypeUnion with Nullable true,
// exactly one non-null Variant, that Variant a TypeClass whose ClassKey is in
// sccKeys). Identity is ClassKey, never name alone, so a future name/mode split can
// never merge two distinct definitions into one false SCC member.
type recProfile struct {
	sccKeys map[schema.ClassKey]bool
}

// classKeyOf builds the (name, mode) key for a class/recursive-alias type node.
func classKeyOf(t schema.Type) schema.ClassKey {
	return schema.ClassKey{Name: t.Name, Mode: t.Mode}
}

// admittedRecursiveClassProfile classifies bundle b against the EXACT ratified
// recursive-class family and returns (profile, true) ONLY for one of the three
// fingerprint-proven shapes: the self-recursive `Node{value string; next Node?}` or
// the mutual `A{value string; b B?}` <-> `B{value string; a A?}` SCC rooted at A or B.
// It returns (‗, false) for a non-recursive bundle, any structural-recursive-alias or
// enum bundle, and any recursive shape OUTSIDE the fingerprint — the caller then keeps
// the blanket recursion decline.
//
// The match is GENUINELY EXACT (not a name-free {bare-string, optional-self-ref}
// shape): canonical class names (Node/A/B), canonical field names (value/next/b/a),
// EXACTLY two fields / one direct nullable-class edge per class, non-streaming
// ClassKeys, and NO class-name @alias, field @alias, class/field/union/variant
// constraints, dynamic flag, OR @stream.* annotation anywhere (class-level
// ClassDef.Stream, field-level ClassField.StreamingNeeded, and TypeMeta.Stream on the
// target / value field / nullable-union edge / class variant — static lowering
// PRESERVES @stream.* on a final non-streaming descriptor, so a shape like
// `Node{value string @stream.done; next Node?}` must be rejected here, not silently
// admitted). A same-shaped `Other{payload string; child Other?}`, an extra edge/scalar,
// a constrained/streamed variant, or a class-name alias all DECLINE. This is the
// pre-claim rule for every non-Node/A/B / aliased / constrained / annotated shape;
// [IsProvenRecursiveStaticFamily] exports it so the nativeserve serve gate stays in
// exact lockstep.
func admittedRecursiveClassProfile(b *schema.Bundle) (recProfile, bool) {
	if b == nil || len(b.RecursiveClasses) == 0 {
		return recProfile{}, false
	}
	// Structural recursive aliases and enums stay concrete #583 residuals (declined).
	if len(b.StructuralRecursiveAliases) > 0 || len(b.Enums) > 0 {
		return recProfile{}, false
	}
	// The target must be a BARE, unconstrained, non-dynamic, non-streaming class with
	// no class-name @alias and NO @stream.* annotation — a constrained/dynamic/aliased/
	// stream-annotated target may not slip the gate. TypeMeta.IsZero() rejects BOTH type
	// constraints AND the {needed,done,state} @stream.* triple in one check.
	t := b.Target
	if t.Kind != schema.TypeClass || t.Mode != schema.NonStreaming || t.Dynamic || !t.Meta.IsZero() {
		return recProfile{}, false
	}

	switch len(b.Classes) {
	case 1:
		// SELF-recursive: EXACTLY Node{value string; next Node?}.
		cd := &b.Classes[0]
		if !isExactSelfRecursiveClass(cd, recNodeClass) {
			return recProfile{}, false
		}
		if t.Name != recNodeClass || !recNamesExactly(b.RecursiveClasses, recNodeClass) {
			return recProfile{}, false
		}
		return recProfile{sccKeys: map[schema.ClassKey]bool{
			{Name: recNodeClass, Mode: schema.NonStreaming}: true,
		}}, true

	case 2:
		// MUTUAL SCC: EXACTLY A{value string; b B?} <-> B{value string; a A?}, rooted at
		// A or B. Look the two classes up BY CANONICAL NAME so a bundle whose two classes
		// are not exactly {A, B} declines.
		a, aok := b.FindClass(recAClass, schema.NonStreaming)
		bb, bok := b.FindClass(recBClass, schema.NonStreaming)
		if !aok || !bok {
			return recProfile{}, false
		}
		if t.Name != recAClass && t.Name != recBClass {
			return recProfile{}, false
		}
		if !recNamesExactly(b.RecursiveClasses, recAClass, recBClass) {
			return recProfile{}, false
		}
		if !isExactMutualClass(a, recAClass, "b", recBClass) || !isExactMutualClass(bb, recBClass, "a", recAClass) {
			return recProfile{}, false
		}
		return recProfile{sccKeys: map[schema.ClassKey]bool{
			{Name: recAClass, Mode: schema.NonStreaming}: true,
			{Name: recBClass, Mode: schema.NonStreaming}: true,
		}}, true

	default:
		return recProfile{}, false
	}
}

// The canonical ratified recursive-class family names. The family IS these exact
// schemas (recursive-classes-scope §2/§5), so the fingerprint pins the canonical
// class AND field names, not merely a {bare-string, optional-self-ref} SHAPE — a
// same-shaped `Other{payload string; child Other?}` is NOT the served family and
// declines pre-claim.
const (
	recNodeClass = "Node"
	recAClass    = "A"
	recBClass    = "B"
)

// IsProvenRecursiveStaticFamily is the exported lockstep predicate the isolated
// nativeserve admission gate uses so the served fingerprint and the parser profile can
// NEVER diverge (both admit EXACTLY Node / mutual A<->B).
func IsProvenRecursiveStaticFamily(b *schema.Bundle) bool {
	_, ok := admittedRecursiveClassProfile(b)
	return ok
}

// isExactSelfRecursiveClass reports whether cd is EXACTLY `name`{value string; next name?}.
func isExactSelfRecursiveClass(cd *schema.ClassDef, name string) bool {
	return isExactRecursiveClassHeader(cd, name) &&
		isExactValueField(cd.Fields[0]) &&
		isExactNullableClassEdge(cd.Fields[1], "next", name)
}

// isExactMutualClass reports whether cd is EXACTLY `name`{value string; edgeField edgeTarget?}.
func isExactMutualClass(cd *schema.ClassDef, name, edgeField, edgeTarget string) bool {
	return isExactRecursiveClassHeader(cd, name) &&
		isExactValueField(cd.Fields[0]) &&
		isExactNullableClassEdge(cd.Fields[1], edgeField, edgeTarget)
}

// isExactRecursiveClassHeader requires the class canonical name (no @alias),
// non-streaming mode, no class constraints, NO class-level @stream.* behavior, and
// EXACTLY two fields.
func isExactRecursiveClassHeader(cd *schema.ClassDef, name string) bool {
	return cd.Name.Name == name && cd.Name.Alias == nil &&
		cd.Mode == schema.NonStreaming && len(cd.Constraints) == 0 && cd.Stream.IsZero() &&
		len(cd.Fields) == 2
}

// isExactValueField requires a field named exactly "value" (no @alias, no
// @stream.needed) whose type is a bare, unconstrained, non-dynamic, ANNOTATION-FREE
// primitive STRING (Meta.IsZero() rejects both constraints and @stream.*).
func isExactValueField(f schema.ClassField) bool {
	if f.Name.Name != "value" || f.Name.Alias != nil || f.StreamingNeeded {
		return false
	}
	t := f.Type
	return t.Kind == schema.TypePrimitive && t.Primitive == schema.PrimitiveString &&
		!t.Dynamic && t.Meta.IsZero()
}

// isExactNullableClassEdge requires a field named exactly fieldName (no @alias, no
// @stream.needed) whose type is a direct nullable single-class edge (`targetClass?`):
// a nullable union with EXACTLY one non-null variant that is a non-dynamic,
// non-streaming class reference to targetClass — and NO metadata (constraints OR
// @stream.*) on EITHER the union type or the class variant.
func isExactNullableClassEdge(f schema.ClassField, fieldName, targetClass string) bool {
	if f.Name.Name != fieldName || f.Name.Alias != nil || f.StreamingNeeded {
		return false
	}
	t := f.Type
	if t.Kind != schema.TypeUnion || t.Union == nil || !t.Union.Nullable || len(t.Union.Variants) != 1 {
		return false
	}
	if !t.Meta.IsZero() {
		return false
	}
	v := t.Union.Variants[0]
	return v.Kind == schema.TypeClass && v.Name == targetClass && v.Mode == schema.NonStreaming &&
		!v.Dynamic && v.Meta.IsZero()
}

// recNamesExactly reports whether the RecursiveClasses marker SET equals exactly the
// given names (dedup-aware, no extras, no missing).
func recNamesExactly(rec []string, names ...string) bool {
	got := make(map[string]bool, len(rec))
	for _, r := range rec {
		got[r] = true
	}
	if len(got) != len(names) {
		return false
	}
	for _, n := range names {
		if !got[n] {
			return false
		}
	}
	return true
}

// isAdmittedRecursiveEdge reports whether t is the ONLY admitted recursive edge
// shape: a nullable union (`T?`) with exactly one non-null variant, that variant a
// direct class reference whose ClassKey is in the admitted SCC. A multi-arm nullable
// union, a nullable non-class arm, a nullable list/map, or a required (non-nullable)
// class edge all return false. It also rejects metadata on the union type or the class
// variant, matching the exact fingerprint.
func isAdmittedRecursiveEdge(t schema.Type, prof recProfile) bool {
	if t.Kind != schema.TypeUnion || t.Union == nil || !t.Union.Nullable {
		return false
	}
	if len(t.Union.Variants) != 1 || !t.Meta.IsZero() {
		return false
	}
	v := t.Union.Variants[0]
	return v.Kind == schema.TypeClass && !v.Dynamic && v.Meta.IsZero() && prof.sccKeys[classKeyOf(v)]
}

// typeGraphHasNonAdmittedUnion reports whether any union reachable from the bundle
// target is NOT an admitted recursive edge. It is the recursion-aware replacement for
// typeGraphHasUnion on the final recursive path: an admitted direct nullable-class
// edge is allowed (its lone class variant is a leaf reference validated separately),
// while EVERY other union — a multi-arm union, a nullable non-class union, a
// nullable list/map — still declines. This is what catches checkSupportedType's
// null fast path: no non-admitted nullable union can be claimed on the recursive
// final lane.
func typeGraphHasNonAdmittedUnion(b *schema.Bundle, prof recProfile) bool {
	if typeHasNonAdmittedUnion(b.Target, prof) {
		return true
	}
	for i := range b.Classes {
		for j := range b.Classes[i].Fields {
			if typeHasNonAdmittedUnion(b.Classes[i].Fields[j].Type, prof) {
				return true
			}
		}
	}
	return false
}

func typeHasNonAdmittedUnion(t schema.Type, prof recProfile) bool {
	switch t.Kind {
	case schema.TypeUnion:
		if isAdmittedRecursiveEdge(t, prof) {
			// Allowed edge: do NOT descend into its lone class variant (a leaf ref).
			return false
		}
		return true
	case schema.TypeList:
		return t.Elem != nil && typeHasNonAdmittedUnion(*t.Elem, prof)
	case schema.TypeMap:
		return (t.Key != nil && typeHasNonAdmittedUnion(*t.Key, prof)) || (t.Value != nil && typeHasNonAdmittedUnion(*t.Value, prof))
	case schema.TypeTuple:
		for i := range t.Items {
			if typeHasNonAdmittedUnion(t.Items[i], prof) {
				return true
			}
		}
	}
	return false
}

// checkSupportedRecursive is the recursion-aware twin of [checkSupported] for an
// admitted recursive-class family: it keeps the structural-recursive-alias reject
// but SKIPS the blanket recursive-class reject (the classes are admitted by prof),
// then runs the IDENTICAL per-field cut-line via checkSupportedFields.
func checkSupportedRecursive(b *schema.Bundle, prof recProfile) error {
	if len(b.StructuralRecursiveAliases) > 0 {
		return unsupported("structural recursive alias")
	}
	_ = prof // membership already validated by admittedRecursiveClassProfile
	return checkSupportedFields(b)
}
