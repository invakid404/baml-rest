package debaml

import (
	"fmt"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Phase 2 (recursive classes) — BAML's path-local (ClassKey, value)
// pair-guard.
//
// BAML does NOT impose a recursion-depth cap. Its ParsingContext carries two
// INDEPENDENT active sets — one for Class::coerce, one for Class::try_cast
// (baml-lib/jsonish deserializer/coercer/mod.rs) — and enter_scope CLONES both on
// every scope entry, while visit_class_value_pair adds the (type, value) pair to
// only the SELECTED set on the cloned context. That makes the state PATH-LOCAL:
// siblings inherit ancestor pairs but never poison each other. Class::coerce checks
// the coerce set and returns a circular-reference PARSING ERROR on a repeat;
// Class::try_cast checks the separate try_cast set and returns None (a normal strict
// no-match) on a repeat. The key's jsonish::Value has STRUCTURAL equality INCLUDING
// completion state (Value derives Eq; its Hash omits completion, but HashSet still
// applies Eq after a collision — so completion is part of membership).
//
// This native port models each active set as an IMMUTABLE frame chain rather than a
// cloned map: a child context SHARES the parent's frames (a pointer to the parent
// chain) and prepends its own frame, so deriving a child is O(1), siblings each
// derive from the same parent chain independently (never seeing each other's frame),
// and a membership check walks the chain applying full structural [valueEqual]. This
// is exactly BAML's clone-on-enter set semantics without a per-entry map clone, and
// it needs NO depth cap. A nil *coerceCtx is an empty context (no active pairs), so a
// leaf / stream / dynamic-final caller may pass nil.

// De-BAML Phase 3a (recursive aliases) extends the Phase-2 pair-guard to a TAGGED
// identity so a recursive CLASS and a recursive ALIAS with the same spelling can
// never collide on one active set: BAML keys the coerce/try_cast visited sets by the
// pair (class-or-alias NAME string, value), and a class name and an alias name live
// in the same string namespace only because BAML's IR guarantees they are distinct.
// Native keeps them distinct STRUCTURALLY via pairKind so a future name overlap can
// never merge a class frame with an alias frame.
type pairKind uint8

const (
	pairKindClass pairKind = iota
	pairKindAlias
)

// pairKey is the tagged (kind, identity) half of a pair-guard frame key. For a class
// it carries the (name, mode) ClassKey; for a structural recursive alias it carries
// the alias name (aliases have no streaming mode). The two never compare equal across
// kinds because kind is part of the struct.
type pairKey struct {
	kind  pairKind
	class schema.ClassKey // meaningful iff kind == pairKindClass
	alias string          // meaningful iff kind == pairKindAlias
}

func classPairKey(k schema.ClassKey) pairKey { return pairKey{kind: pairKindClass, class: k} }
func aliasPairKey(name string) pairKey       { return pairKey{kind: pairKindAlias, alias: name} }

// pairFrame is one (pairKey, value) entry on a path-local active set. Frames are
// immutable and chained parent-first.
type pairFrame struct {
	key  pairKey
	val  *value
	prev *pairFrame
}

// coerceCtx carries the SEPARATE Class::coerce and Class::try_cast active pair sets
// threaded through every final recursive descent. The two sets are independent: a
// class/alias visited on the coerce path does not block the same key on a try_cast
// path and vice-versa, matching BAML's two ParsingContext sets.
//
// hint is BAML's ctx.union_variant_hint: the winning union-variant index carried from
// the PREVIOUS array element to the next SIBLING (coerce_array's enter_scope_with_hint),
// tried first by the union coercer. It is nil (no hint) everywhere EXCEPT an array's
// sibling scope, and is RESET to nil by every ordinary enter_scope (map values, class
// fields) AND by the class/alias pair entry (visit_class_value_pair), matching stock
// BAML. The class family never sets it, so the 289/8C/Phase-2 paths are unaffected.
type coerceCtx struct {
	coerceChain  *pairFrame
	tryCastChain *pairFrame
	hint         *int
}

func (c *coerceCtx) has(chain func(*coerceCtx) *pairFrame, key pairKey, v value) bool {
	if c == nil {
		return false
	}
	for f := chain(c); f != nil; f = f.prev {
		if f.key == key && valueEqual(*f.val, v) {
			return true
		}
	}
	return false
}

func coerceChainOf(c *coerceCtx) *pairFrame  { return c.coerceChain }
func tryCastChainOf(c *coerceCtx) *pairFrame { return c.tryCastChain }

func (c *coerceCtx) coerceHas(key schema.ClassKey, v value) bool {
	return c.has(coerceChainOf, classPairKey(key), v)
}

func (c *coerceCtx) tryCastHas(key schema.ClassKey, v value) bool {
	return c.has(tryCastChainOf, classPairKey(key), v)
}

// coerceHasAlias / tryCastHasAlias are the recursive-ALIAS twins of coerceHas /
// tryCastHas: they check the tagged alias key (alias name, value) on the respective
// active set.
func (c *coerceCtx) coerceHasAlias(name string, v value) bool {
	return c.has(coerceChainOf, aliasPairKey(name), v)
}

func (c *coerceCtx) tryCastHasAlias(name string, v value) bool {
	return c.has(tryCastChainOf, aliasPairKey(name), v)
}

// chains returns the parent's two frame chains (nil-safe).
func (c *coerceCtx) chains() (*pairFrame, *pairFrame) {
	if c == nil {
		return nil, nil
	}
	return c.coerceChain, c.tryCastChain
}

// enterCoerceKey derives a CHILD context that adds (key, v) to the coerce set,
// inherits the try_cast set unchanged, and RESETS the hint (visit_class_value_pair
// sets union_variant_hint to None). The parent is not mutated (path-local).
func (c *coerceCtx) enterCoerceKey(key pairKey, v value) *coerceCtx {
	cc, tc := c.chains()
	vv := v
	return &coerceCtx{coerceChain: &pairFrame{key: key, val: &vv, prev: cc}, tryCastChain: tc}
}

// enterTryCastKey derives a CHILD context that adds (key, v) to the try_cast set,
// inherits the coerce set unchanged, and RESETS the hint.
func (c *coerceCtx) enterTryCastKey(key pairKey, v value) *coerceCtx {
	cc, tc := c.chains()
	vv := v
	return &coerceCtx{coerceChain: cc, tryCastChain: &pairFrame{key: key, val: &vv, prev: tc}}
}

func (c *coerceCtx) enterCoerce(key schema.ClassKey, v value) *coerceCtx {
	return c.enterCoerceKey(classPairKey(key), v)
}

func (c *coerceCtx) enterTryCast(key schema.ClassKey, v value) *coerceCtx {
	return c.enterTryCastKey(classPairKey(key), v)
}

// enterCoerceAlias / enterTryCastAlias are the recursive-ALIAS twins: they add the
// tagged alias pair to the respective active set (and reset the hint), matching
// coerce_alias / try_cast_alias's visit_class_value_pair.
func (c *coerceCtx) enterCoerceAlias(name string, v value) *coerceCtx {
	return c.enterCoerceKey(aliasPairKey(name), v)
}

func (c *coerceCtx) enterTryCastAlias(name string, v value) *coerceCtx {
	return c.enterTryCastKey(aliasPairKey(name), v)
}

// enterScope derives a CHILD context that inherits BOTH active sets unchanged and
// RESETS the hint to nil — BAML's ParsingContext::enter_scope (map values, class
// fields, the SingleToArray implied element). The two frame chains are shared by
// pointer, so the child sees ancestor pairs but adds none.
func (c *coerceCtx) enterScope() *coerceCtx {
	cc, tc := c.chains()
	return &coerceCtx{coerceChain: cc, tryCastChain: tc}
}

// enterScopeWithHint derives a CHILD context that inherits both active sets and
// CARRIES the given union-variant hint — BAML's enter_scope_with_hint, used only for
// the next array sibling. A nil hint is equivalent to enterScope.
func (c *coerceCtx) enterScopeWithHint(hint *int) *coerceCtx {
	cc, tc := c.chains()
	return &coerceCtx{coerceChain: cc, tryCastChain: tc, hint: hint}
}

// valueEqual reports STRUCTURAL equality of two native jsonish values, INCLUDING
// completion state (incomplete) and preserving ORDERED object entries with
// duplicates — the exact key equality BAML's jsonish::Value Eq uses for its
// (type, value) pair membership. Pointer identity is deliberately NOT used: two
// independently-parsed identical subtrees must compare equal, and a proper finite
// subtree must NOT compare equal to a differently-shaped ancestor.
func valueEqual(a, b value) bool {
	if a.kind != b.kind || a.incomplete != b.incomplete {
		return false
	}
	switch a.kind {
	case valNull:
		return true
	case valBool:
		return a.boolV == b.boolV
	case valNumber:
		return a.numV == b.numV // json.Number is the raw token string: exact equality
	case valString:
		return a.strV == b.strV
	case valArray:
		if len(a.arrV) != len(b.arrV) {
			return false
		}
		for i := range a.arrV {
			if !valueEqual(a.arrV[i], b.arrV[i]) {
				return false
			}
		}
		return true
	case valObject:
		if len(a.objV) != len(b.objV) {
			return false
		}
		for i := range a.objV {
			if a.objV[i].key != b.objV[i].key || !valueEqual(a.objV[i].val, b.objV[i].val) {
				return false
			}
		}
		return true
	}
	return false
}

// errCircularReference is native's analogue of BAML's Class::coerce
// circular-reference PARSING ERROR (ir_ref/coerce_class.rs) on a repeated
// (ClassKey, value) on the active COERCE path. It is a CLAIMED parse failure — NOT
// the bamlutils.ErrDeBAMLParseUnsupported fallback sentinel — so it propagates
// unchanged, matching BAML erroring (not declining) on the cycle; the differential
// asserts status parity. The admitted served family never reaches it (each recursive
// edge receives a proper finite subtree that cannot equal its non-cyclic parent); it
// guards a future widening / an out-of-profile one-field implied-recursion shape.
func errCircularReference(name string) error {
	return fmt.Errorf("debaml: circular reference detected while coercing class %q", name)
}
