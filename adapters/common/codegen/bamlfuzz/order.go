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
// schema shape carries semantics the walker cannot drive without
// additional metadata. Today this fires when a union node is
// reached without a recorded UnionChoice entry — the walker has no
// canonical "expected order" for a union value without knowing
// which arm produced it. The integration oracles treat this as a
// hard failure because UnionChoices are propagated for every union
// path the walker visits; a missing or stale choice is an integrity
// bug, not a skippable shape.
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
// `schema`, asserting wire key order at JSON nodes the schema types
// as either a class instance or a map. The map-key contract is
// insertion order: FuzzValue.MapEntries carries the request's
// insertion order and the wire is expected to echo it back. Map
// values are then walked through the inner type. Union nodes resolve
// through `choices` (typically
// CaseMetadata.UnionChoices from the walker), advancing into the
// recorded variant arm; a missing or out-of-range entry returns
// ErrSchemaOrderUnsupported, which the integration oracles surface
// as a hard failure (see ErrSchemaOrderUnsupported's doc comment).
//
// `label` is opaque and is stored on every emitted entry's Side field.
//
// Pure: takes no *testing.T. Callers decide whether a non-empty diff
// fails, logs, or feeds the replay envelope.
//
// An empty diff slice with a nil error means the inputs agreed on key
// order at every class node reachable from schema's effective root.
func SchemaOrderDiff(label string, schema FuzzSchema, expected, actual json.RawMessage) ([]SchemaOrderDiffEntry, error) {
	return SchemaOrderDiffWithChoices(label, schema, expected, actual, nil)
}

// SchemaOrderDiffWithChoices is the union-aware variant of
// SchemaOrderDiff. `choices` mirrors CaseMetadata.UnionChoices. It is for
// expected-vs-actual comparisons: the boundaryml/baml#3690 null-key
// tolerance is applied only to the actual side. For actual-vs-actual
// parity order checks use SchemaOrderDiffParityWithChoices.
func SchemaOrderDiffWithChoices(label string, schema FuzzSchema, expected, actual json.RawMessage, choices map[string]UnionChoice) ([]SchemaOrderDiffEntry, error) {
	return schemaOrderDiff(label, schema, expected, actual, choices, false)
}

// SchemaOrderDiffParityWithChoices is the symmetric variant for
// actual-vs-actual parity order checks (e.g. dynclient_vs_rest), where
// both legs are BAML-generated and either may carry a boundaryml/baml#3690
// leaked null key. Leaked null keys on either side are tolerated before
// the key-order comparison.
func SchemaOrderDiffParityWithChoices(label string, schema FuzzSchema, a, b json.RawMessage, choices map[string]UnionChoice) ([]SchemaOrderDiffEntry, error) {
	return schemaOrderDiff(label, schema, a, b, choices, true)
}

func schemaOrderDiff(label string, schema FuzzSchema, expected, actual json.RawMessage, choices map[string]UnionChoice, parity bool) ([]SchemaOrderDiffEntry, error) {
	if schema.RootClass == "" && schema.RootType == nil {
		return nil, fmt.Errorf("bamlfuzz: schema missing both RootClass and RootType")
	}
	if schema.RootClass != "" {
		if _, ok := schema.FindClass(schema.RootClass); !ok {
			return nil, fmt.Errorf("bamlfuzz: root class %q not present in schema", schema.RootClass)
		}
	}
	exp, err := decodeOrderedJSON(expected)
	if err != nil {
		return nil, fmt.Errorf("decode expected: %w", err)
	}
	got, err := decodeOrderedJSON(actual)
	if err != nil {
		return nil, fmt.Errorf("decode actual: %w", err)
	}
	w := &orderWalker{schema: schema, side: label, choices: choices, parity: parity}
	w.walkType("$", schema.EffectiveRoot(), exp, got)
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
	schema  FuzzSchema
	side    string
	diffs   []SchemaOrderDiffEntry
	err     error
	choices map[string]UnionChoice
	// parity tolerates boundaryml/baml#3690 leaked null keys on either
	// side (actual-vs-actual). When false, only the actual side (got) is
	// stripped (expected-vs-actual).
	parity bool
}

// orderKeys applies the boundaryml/baml#3690 null-key tolerance to a
// class/map node and returns the (expectedKeys, actualKeys) to compare.
// The actual side is always stripped of leaked nulls; in parity mode the
// expected side is stripped too.
//
// stripAllNulls is set by map-arm callers. A null value is never a
// legitimate map entry, so in parity mode (two actual outputs that may
// each leak) every null key is dropped from both sides — including a
// null key both sides carry, whose position would otherwise produce a
// spurious order diff when the two leaks land in different spots. Class
// nodes pass false: a null there can be a real optional field whose
// declared position still matters.
func (w *orderWalker) orderKeys(exp, got orderedJSON, stripAllNulls bool) (expKeys, gotKeys []string) {
	if w.parity && stripAllNulls {
		return stripNullKeys(exp.keys, exp.byKey), stripNullKeys(got.keys, got.byKey)
	}
	gotKeys = stripExtraNullKeys(got.keys, got.byKey, exp.byKey)
	if w.parity {
		expKeys = stripExtraNullKeys(exp.keys, exp.byKey, got.byKey)
	} else {
		expKeys = append([]string{}, exp.keys...)
	}
	return expKeys, gotKeys
}

func (w *orderWalker) setErr(err error) {
	if w.err == nil {
		w.err = err
	}
}

// Workaround for boundaryml/baml#3690: BAML's Go codegen serializes
// null-valued optional fields from a class union arm even when the
// active arm is a map or a different class, leaking extra null-valued
// keys into the wire object. The order comparisons below drop those
// leaked keys before checking key-order equality. The bug only ever
// ADDS keys to a BAML-generated output, so the direction depends on what
// is being compared (orderWalker.parity):
//
//   - expected-vs-actual (parity == false): only `got` (the actual side)
//     is stripped; `exp` is compared as-is, so a null key the expected
//     side carries that `got` dropped still registers as a mismatch.
//   - actual-vs-actual (parity == true): both sides are BAML-generated,
//     so leaked null keys on either side are stripped. In a map arm
//     whose value type cannot be null, a null is never a real entry, so
//     parity mode drops every null key from both sides — even one both
//     carry — so two leaks landing in different positions do not produce
//     a spurious order diff. When the map's value type CAN be null (e.g.
//     map<string, null> or map<string, optional<T>>) the nulls are real
//     entries and only the reference-aware strip applies.
//
// This is a comparison-only relaxation. Remove when the upstream fix lands.

// stripExtraNullKeys returns a filtered copy of `keys`, removing entries
// that are null-valued in `src` and absent from the reference set `ref`.
// Null keys that `ref` also carries are preserved so genuine null-valued
// fields still participate. The caller chooses which side(s) to strip,
// encoding the asymmetric / parity tolerance described above.
func stripExtraNullKeys(keys []string, src, ref map[string]orderedJSON) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if src[k].kind == orderedNull {
			if _, ok := ref[k]; !ok {
				continue
			}
		}
		out = append(out, k)
	}
	return out
}

// stripNullKeys returns a copy of `keys` with every null-valued entry
// removed, unconditional of any reference set. Used for parity map-arm
// contexts where a null is never a real entry, so its position carries
// no order signal even when both actual sides carry it.
func stripNullKeys(keys []string, src map[string]orderedJSON) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if src[k].kind == orderedNull {
			continue
		}
		out = append(out, k)
	}
	return out
}

// CanBeNull reports whether a value of `typ` can legitimately serialize
// as JSON null. It gates the boundaryml/baml#3690 null-leak tolerance:
// only when a type CANNOT be null is an observed null necessarily a leak.
// For the parity map-arm strip here, and for union-arm derivation in the
// integration oracle, a map whose value type can be null (e.g.
// map<string, null> or map<string, optional<T>>) has real null entries
// that must still be compared / matched.
func CanBeNull(typ FuzzType) bool {
	switch typ.Kind {
	case KindNull, KindOptional:
		return true
	case KindUnion:
		for _, v := range typ.Variants {
			if CanBeNull(v) {
				return true
			}
		}
		return false
	default:
		return false
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
	expKeys, gotKeys := w.orderKeys(exp, got, false)
	if !slices.Equal(expKeys, gotKeys) {
		w.diffs = append(w.diffs, SchemaOrderDiffEntry{
			Side:     w.side,
			Path:     path,
			Expected: expKeys,
			Actual:   gotKeys,
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
		// A null map entry is necessarily a #3690 leak only when the
		// value type cannot itself be null; for map<string, null> or
		// map<string, optional<T>> the nulls are real entries whose order
		// still matters, so fall back to the reference-aware strip.
		stripLeakedNulls := !CanBeNull(*typ.Inner)
		expKeys, gotKeys := w.orderKeys(exp, got, stripLeakedNulls)
		if !slices.Equal(expKeys, gotKeys) {
			w.diffs = append(w.diffs, SchemaOrderDiffEntry{
				Side:     w.side,
				Path:     path,
				Expected: expKeys,
				Actual:   gotKeys,
			})
		}
		for _, key := range sortedIntersection(exp.byKey, got.byKey) {
			// In parity mode a null-valued map key whose value type
			// cannot be null is a leaked field, not a real entry. Its
			// value carries no order signal, and a nested union beneath
			// it has no recorded choice — recursing would surface a
			// spurious ErrSchemaOrderUnsupported. Skip it.
			if w.parity && stripLeakedNulls && (exp.byKey[key].kind == orderedNull || got.byKey[key].kind == orderedNull) {
				continue
			}
			w.walkType(fmt.Sprintf("%s[%q]", path, key), *typ.Inner, exp.byKey[key], got.byKey[key])
		}
	case KindUnion:
		// CaseMetadata.UnionChoices uses paths rooted at "" (the
		// renderer convention); SchemaOrderDiff paths are rooted at
		// "$". Strip the leading "$" so the two conventions match
		// without leaking the path format into the metadata.
		key := path
		if len(key) > 0 && key[0] == '$' {
			key = key[1:]
		}
		choice, ok := w.choices[key]
		if !ok {
			w.setErr(ErrSchemaOrderUnsupported)
			return
		}
		if choice.Index < 0 || choice.Index >= len(typ.Variants) {
			w.setErr(ErrSchemaOrderUnsupported)
			return
		}
		// Fail closed on stale or schema-incompatible metadata. The
		// scope's D11 contract requires the order checker to bail
		// before traversing the wrong arm when a corpus / replay
		// envelope was recorded against a different shape. Treat any
		// of variant-count, arm-kind, or arm-ref drift as
		// unsupported.
		selected := typ.Variants[choice.Index]
		if choice.VariantCount != len(typ.Variants) {
			w.setErr(ErrSchemaOrderUnsupported)
			return
		}
		if choice.Kind != selected.Kind {
			w.setErr(ErrSchemaOrderUnsupported)
			return
		}
		if (selected.Kind == KindClassRef || selected.Kind == KindEnumRef) && choice.Ref != selected.Ref {
			w.setErr(ErrSchemaOrderUnsupported)
			return
		}
		w.walkType(path+":v", typ.Variants[choice.Index], exp, got)
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
