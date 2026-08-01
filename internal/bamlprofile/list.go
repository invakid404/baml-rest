package bamlprofile

import (
	"fmt"

	"github.com/invakid404/minijinja-go/v2/value"
)

// listObject is BAML's MinijinjaBamlList: the host value a BamlValue::List lowers
// to (baml_value_to_jinja_value.rs:34-40,337-388). It is a sequence — normal
// indexing, length, iteration and membership — whose rendering is Rust's
// debug_list with nested none rendered as null: COMPACT when rendered directly
// (the ambient formatter is non-alternate there) and multi-line when nested
// inside a class, which forces alternate.
//
// Authority: BAML v0.223 engine/baml-lib/jinja-runtime/src/
// baml_value_to_jinja_value.rs:337-388 (Seq repr, debug_list Display with the
// none->null replacement, seq serialization).
//
// A host list is used, rather than a plain fork []Value, because the fork's
// native slice renders compactly via each item's Repr, whereas BAML renders a
// lowered list as a multi-line pretty debug-list. The distinction is only
// observable in DIRECT rendering; indexing/length/iteration/membership match a
// plain sequence, so those go through the fork's ordinary Seq handling.
type listObject struct {
	items []value.Value
}

// ListValue constructs a host list value (BAML's BamlValue::List lowering) for
// use as a render argument or a nested class/list element. Like ClassValue it
// fails loud when an item is not a scalar, none, or a bamlprofile host value —
// a native fork container would render compactly via the fork's Repr instead of
// BAML's pretty host rendering, a silent divergence.
//
// The validated items are stored as an OWNED shallow copy, not the caller's
// slice. Without the copy a caller could invalidate the construction-time proof
// after the fact (`items[0] = value.FromSlice(...)`), and the renderer's
// "unreachable" panic in debugValue would become reachable from ordinary API
// use. ClassValue already builds its own []classField; this makes the list's
// ingress contract identical. The copy is shallow because each element is an
// immutable fork value.Value — only the slice header/backing array is aliased,
// and only that aliasing is what assertHostRenderable cannot re-check later.
func ListValue(items []value.Value) (value.Value, error) {
	for i, it := range items {
		if err := assertHostRenderable(it); err != nil {
			return value.Undefined(), fmt.Errorf("bamlprofile: list item %d: %w", i, err)
		}
	}
	owned := make([]value.Value, len(items))
	copy(owned, items)
	return value.FromObject(&listObject{items: owned}), nil
}

// ObjectRepr is Seq (rs:369-371): the list iterates, indexes and measures like a
// sequence.
func (l *listObject) ObjectRepr() value.ObjectRepr { return value.ObjectReprSeq }

// GetAttr has no attributes; a list is indexed, not attribute-accessed. The fork
// routes index access through SeqItem, not GetAttr.
func (l *listObject) GetAttr(string) value.Value { return value.Undefined() }

// SeqLen / SeqItem give the fork ordinary sequence length, indexing, iteration
// and membership. Items are returned verbatim (a none stays none, so `is none`
// and comparisons see the real value); the none->null substitution is
// display-only and lives in the pretty renderer (rs:373-388: only Display
// swaps).
func (l *listObject) SeqLen() int { return len(l.items) }

func (l *listObject) SeqItem(i int) value.Value {
	if i < 0 || i >= len(l.items) {
		return value.Undefined()
	}
	return l.items[i]
}

// ObjectString renders the list at TOP LEVEL, where it is COMPACT
// (`[a, b, c]`). BAML's MinijinjaBamlList Display calls the ambient formatter's
// debug_list (rs:373-388), which is non-alternate at the top level, so a
// directly-rendered list is single-line — unlike a class, whose Display forces
// `{map:#?}` and is always multi-line. A list NESTED inside a class inherits the
// alternate flag and renders multi-line; that path goes through the walker's
// alternate mode, not here. The fork's Value.String/Value.Repr dispatch this
// (PATCHES #104), so it composes through containers and coercions too.
func (l *listObject) ObjectString() string { return debugList(l, debugCompact, 0) }
