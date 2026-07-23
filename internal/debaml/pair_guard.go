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

// pairFrame is one (ClassKey, value) entry on a path-local active set. Frames are
// immutable and chained parent-first.
type pairFrame struct {
	key  schema.ClassKey
	val  *value
	prev *pairFrame
}

// coerceCtx carries the SEPARATE Class::coerce and Class::try_cast active pair sets
// threaded through every final recursive descent. The two sets are independent: a
// class visited on the coerce path does not block the same class on a try_cast path
// and vice-versa, matching BAML's two ParsingContext sets.
type coerceCtx struct {
	coerceChain  *pairFrame
	tryCastChain *pairFrame
}

func (c *coerceCtx) coerceHas(key schema.ClassKey, v value) bool {
	if c == nil {
		return false
	}
	for f := c.coerceChain; f != nil; f = f.prev {
		if f.key == key && valueEqual(*f.val, v) {
			return true
		}
	}
	return false
}

func (c *coerceCtx) tryCastHas(key schema.ClassKey, v value) bool {
	if c == nil {
		return false
	}
	for f := c.tryCastChain; f != nil; f = f.prev {
		if f.key == key && valueEqual(*f.val, v) {
			return true
		}
	}
	return false
}

// enterCoerce derives a CHILD context that adds (key, v) to the coerce set and
// inherits the try_cast set unchanged. The parent is not mutated (path-local).
func (c *coerceCtx) enterCoerce(key schema.ClassKey, v value) *coerceCtx {
	var cc, tc *pairFrame
	if c != nil {
		cc, tc = c.coerceChain, c.tryCastChain
	}
	vv := v
	return &coerceCtx{coerceChain: &pairFrame{key: key, val: &vv, prev: cc}, tryCastChain: tc}
}

// enterTryCast derives a CHILD context that adds (key, v) to the try_cast set and
// inherits the coerce set unchanged.
func (c *coerceCtx) enterTryCast(key schema.ClassKey, v value) *coerceCtx {
	var cc, tc *pairFrame
	if c != nil {
		cc, tc = c.coerceChain, c.tryCastChain
	}
	vv := v
	return &coerceCtx{coerceChain: cc, tryCastChain: &pairFrame{key: key, val: &vv, prev: tc}}
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
