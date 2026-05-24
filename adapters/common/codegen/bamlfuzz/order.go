package bamlfuzz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/invakid404/baml-rest/bamlutils"
)

// ErrSchemaOrderUnsupported is returned by SchemaOrderDiff when the
// schema shape carries semantics the walker does not yet model. Today
// this is union-bearing schemas (FuzzSchema.HasUnion); the walker has
// no canonical "expected order" for a union value without a union-aware
// dispatch. Callers detect with errors.Is and typically skip the order
// assertion while still running the semantic-equality checks.
var ErrSchemaOrderUnsupported = errors.New("bamlfuzz: schema order check unsupported")

// SchemaOrderDiffEntry names one schema-order disagreement at a JSON
// path. `Side` is copied verbatim from the caller's `label` (e.g.
// "expected_vs_dynclient") so failure envelopes can identify which
// oracle pair the entry belongs to. `Expected` and `Actual` are the
// wire-order key slices at `Path` so the failure log shows the swap
// explicitly.
type SchemaOrderDiffEntry struct {
	Side     string   `json:"side"`
	Path     string   `json:"path"`
	Expected []string `json:"expected"`
	Actual   []string `json:"actual"`
}

// SchemaOrderDiff walks `expected` and `actual` in parallel with
// `schema`, asserting wire key order only at JSON nodes the schema
// types as a class instance. Map nodes participate in recursion but
// their key order is not asserted; map values are walked through the
// inner type.
//
// `label` is opaque and is stored on every emitted entry's Side field.
//
// Pure: takes no *testing.T. Callers decide whether a non-empty diff
// fails, logs, or feeds the replay envelope. Returns
// ErrSchemaOrderUnsupported when schema.HasUnion is true so callers can
// skip the order assertion without abandoning semantic diagnostics.
//
// An empty diff slice with a nil error means the inputs agreed on key
// order at every class node reachable from schema.RootClass.
func SchemaOrderDiff(label string, schema FuzzSchema, expected, actual json.RawMessage) ([]SchemaOrderDiffEntry, error) {
	if schema.HasUnion {
		return nil, ErrSchemaOrderUnsupported
	}
	if schema.RootClass == "" {
		return nil, fmt.Errorf("bamlfuzz: schema missing RootClass")
	}
	if _, ok := schema.FindClass(schema.RootClass); !ok {
		return nil, fmt.Errorf("bamlfuzz: root class %q not present in schema", schema.RootClass)
	}
	exp, err := decodeOrderedJSON(expected)
	if err != nil {
		return nil, fmt.Errorf("decode expected: %w", err)
	}
	got, err := decodeOrderedJSON(actual)
	if err != nil {
		return nil, fmt.Errorf("decode actual: %w", err)
	}
	w := &orderWalker{schema: schema, side: label}
	w.walkClass("$", schema.RootClass, exp, got)
	return w.diffs, w.err
}

// FormatSchemaOrderDiffs renders order diagnostics into the
// human-readable line shape that travels through the failure
// envelope's OrderWarning []string field. Empty input returns nil so a
// nil append leaves the envelope's OrderWarning unchanged.
func FormatSchemaOrderDiffs(diffs []SchemaOrderDiffEntry) []string {
	if len(diffs) == 0 {
		return nil
	}
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, fmt.Sprintf("%s at %s: expected %v, got %v", d.Side, d.Path, d.Expected, d.Actual))
	}
	return out
}

// orderedJSONKind discriminates the shapes the order walker
// differentiates. Scalars collapse to orderedScalar because the walker
// never inspects scalar content.
type orderedJSONKind int

const (
	orderedNull orderedJSONKind = iota
	orderedObject
	orderedArray
	orderedScalar
)

// orderedJSON is a parsed JSON tree that preserves object key order.
// Objects are decoded recursively through bamlutils.OrderedMap so
// duplicate object keys are rejected at the boundary: semantic diff
// cannot represent duplicates and class-instance order assertions
// would otherwise be ambiguous.
type orderedJSON struct {
	kind  orderedJSONKind
	keys  []string
	byKey map[string]orderedJSON
	array []orderedJSON
}

func decodeOrderedJSON(data json.RawMessage) (orderedJSON, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return orderedJSON{}, fmt.Errorf("empty JSON payload")
	}
	switch trimmed[0] {
	case '{':
		var om bamlutils.OrderedMap[json.RawMessage]
		if err := om.UnmarshalJSON(data); err != nil {
			return orderedJSON{}, err
		}
		node := orderedJSON{
			kind:  orderedObject,
			keys:  om.Keys(),
			byKey: make(map[string]orderedJSON, om.Len()),
		}
		var decodeErr error
		om.Range(func(k string, raw json.RawMessage) bool {
			child, err := decodeOrderedJSON(raw)
			if err != nil {
				decodeErr = fmt.Errorf("%s: %w", k, err)
				return false
			}
			node.byKey[k] = child
			return true
		})
		if decodeErr != nil {
			return orderedJSON{}, decodeErr
		}
		return node, nil
	case '[':
		var raws []json.RawMessage
		if err := json.Unmarshal(data, &raws); err != nil {
			return orderedJSON{}, err
		}
		node := orderedJSON{kind: orderedArray, array: make([]orderedJSON, 0, len(raws))}
		for i, r := range raws {
			child, err := decodeOrderedJSON(r)
			if err != nil {
				return orderedJSON{}, fmt.Errorf("[%d]: %w", i, err)
			}
			node.array = append(node.array, child)
		}
		return node, nil
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			return orderedJSON{kind: orderedNull}, nil
		}
		return orderedJSON{}, fmt.Errorf("invalid JSON token")
	default:
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return orderedJSON{}, err
		}
		if v == nil {
			return orderedJSON{kind: orderedNull}, nil
		}
		return orderedJSON{kind: orderedScalar}, nil
	}
}

type orderWalker struct {
	schema FuzzSchema
	side   string
	diffs  []SchemaOrderDiffEntry
	err    error
}

func (w *orderWalker) setErr(err error) {
	if w.err == nil {
		w.err = err
	}
}

func (w *orderWalker) walkClass(path, className string, exp, got orderedJSON) {
	cls, ok := w.schema.FindClass(className)
	if !ok {
		w.setErr(fmt.Errorf("unknown class %q at %s", className, path))
		return
	}
	// Structural mismatches are SemanticDiff's contract; the order
	// walker silently bails so the failure envelope shows the
	// semantic diff without piling on a confusing order entry.
	if exp.kind != orderedObject || got.kind != orderedObject {
		return
	}
	if !slices.Equal(exp.keys, got.keys) {
		w.diffs = append(w.diffs, SchemaOrderDiffEntry{
			Side:     w.side,
			Path:     path,
			Expected: append([]string{}, exp.keys...),
			Actual:   append([]string{}, got.keys...),
		})
	}
	for _, prop := range cls.Properties {
		ev, eok := exp.byKey[prop.Name]
		av, aok := got.byKey[prop.Name]
		if !eok || !aok {
			continue
		}
		w.walkType(path+"."+prop.Name, prop.Type, ev, av)
	}
}

func (w *orderWalker) walkType(path string, typ FuzzType, exp, got orderedJSON) {
	switch typ.Kind {
	case KindClassRef:
		w.walkClass(path, typ.Ref, exp, got)
	case KindOptional:
		if typ.Inner == nil {
			return
		}
		if exp.kind == orderedNull || got.kind == orderedNull {
			return
		}
		w.walkType(path, *typ.Inner, exp, got)
	case KindList:
		if typ.Inner == nil {
			return
		}
		if exp.kind != orderedArray || got.kind != orderedArray {
			return
		}
		n := min(len(exp.array), len(got.array))
		for i := 0; i < n; i++ {
			w.walkType(fmt.Sprintf("%s[%d]", path, i), *typ.Inner, exp.array[i], got.array[i])
		}
	case KindMap:
		if typ.Inner == nil {
			return
		}
		if exp.kind != orderedObject || got.kind != orderedObject {
			return
		}
		for _, key := range sortedIntersection(exp.byKey, got.byKey) {
			w.walkType(fmt.Sprintf("%s[%q]", path, key), *typ.Inner, exp.byKey[key], got.byKey[key])
		}
	}
}

func sortedIntersection(a, b map[string]orderedJSON) []string {
	out := make([]string, 0, len(a))
	for k := range a {
		if _, ok := b[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
