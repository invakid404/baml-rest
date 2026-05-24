package bamlutils

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/bytedance/sonic"
)

// ReorderDynamicOutputBySchema reorders JSON object keys in a dynamic
// output payload so every class-instance node carries its keys in the
// declared schema order.
//
// `data` is the post-FlattenDynamicOutput JSON, i.e. the user-facing
// payload with the DynamicProperties envelope already stripped.
// `schema` carries the declared top-level properties (synthetic root
// class Baml_Rest_DynamicOutput) plus any user-defined Classes and
// Enums referenced by ref.
//
// The walker descends the JSON tree in parallel with the schema:
//
//   - Class nodes: declared properties are emitted in schema order;
//     declared properties missing from the JSON are skipped; extra
//     input keys are appended after the declared ones in their
//     original input order.
//   - Optional: null passes through; non-null recurses into Inner.
//   - List: each element recurses against Items.
//   - Map: input key order is preserved; values recurse against Values.
//   - Union: variants are tried in declared oneOf order; the first
//     shape-matching variant wins; no match passes through unmodified.
//   - Scalars, literals, and enum refs pass through unchanged.
//
// Errors are returned for malformed JSON, duplicate object keys, or a
// class ref that names a class missing from the schema.
func ReorderDynamicOutputBySchema(data []byte, schema *DynamicOutputSchema) ([]byte, error) {
	if schema == nil || schema.Properties.Len() == 0 {
		return nil, fmt.Errorf("dynamic output order: missing root schema")
	}
	node, err := decodeOrderedDynamicJSON(data)
	if err != nil {
		return nil, fmt.Errorf("dynamic output order: %w", err)
	}
	w := &dynamicOrderWalker{schema: schema}
	out, err := w.reorderClass(dynamicOutputClassName, node)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := out.appendTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// orderedJSONKind discriminates the JSON value kinds the dynamic order
// walker tracks. Booleans, numbers, strings, and null collapse into
// scalar-like leaf kinds carrying their source bytes verbatim so the
// re-emit step round-trips the literal content.
type orderedJSONKind int

const (
	orderedNull orderedJSONKind = iota
	orderedBool
	orderedNumber
	orderedString
	orderedArray
	orderedObject
)

// orderedNode is the parsed JSON tree the walker mutates. Object key
// order is captured in `keys`; values live in `byKey`. Arrays carry
// their elements in `array`. Leaf scalars carry their raw source bytes
// in `raw` so re-emit produces the same literal value the input
// supplied (modulo whitespace).
type orderedNode struct {
	kind  orderedJSONKind
	raw   []byte
	keys  []string
	byKey map[string]orderedNode
	array []orderedNode
}

// decodeOrderedDynamicJSON parses data into an orderedNode. Object
// decoding goes through OrderedMap[stdjson.RawMessage], which rejects
// duplicate keys and preserves wire order. Arrays decode through sonic
// into a raw-message slice for per-element recursion. Scalars stash
// their source bytes for fidelity on re-emit.
func decodeOrderedDynamicJSON(data []byte) (orderedNode, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return orderedNode{}, fmt.Errorf("empty JSON payload")
	}
	switch trimmed[0] {
	case '{':
		var om OrderedMap[stdjson.RawMessage]
		if err := om.UnmarshalJSON(data); err != nil {
			return orderedNode{}, err
		}
		node := orderedNode{
			kind:  orderedObject,
			keys:  om.Keys(),
			byKey: make(map[string]orderedNode, om.Len()),
		}
		var decodeErr error
		om.Range(func(k string, raw stdjson.RawMessage) bool {
			child, err := decodeOrderedDynamicJSON(raw)
			if err != nil {
				decodeErr = fmt.Errorf("%s: %w", k, err)
				return false
			}
			node.byKey[k] = child
			return true
		})
		if decodeErr != nil {
			return orderedNode{}, decodeErr
		}
		return node, nil
	case '[':
		var raws []stdjson.RawMessage
		if err := sonic.Unmarshal(data, &raws); err != nil {
			return orderedNode{}, err
		}
		node := orderedNode{kind: orderedArray, array: make([]orderedNode, 0, len(raws))}
		for i, r := range raws {
			child, err := decodeOrderedDynamicJSON(r)
			if err != nil {
				return orderedNode{}, fmt.Errorf("[%d]: %w", i, err)
			}
			node.array = append(node.array, child)
		}
		return node, nil
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			return orderedNode{kind: orderedNull, raw: cloneBytes(trimmed)}, nil
		}
		return orderedNode{}, fmt.Errorf("invalid JSON token")
	case 't':
		if bytes.Equal(trimmed, []byte("true")) {
			return orderedNode{kind: orderedBool, raw: cloneBytes(trimmed)}, nil
		}
		return orderedNode{}, fmt.Errorf("invalid JSON token")
	case 'f':
		if bytes.Equal(trimmed, []byte("false")) {
			return orderedNode{kind: orderedBool, raw: cloneBytes(trimmed)}, nil
		}
		return orderedNode{}, fmt.Errorf("invalid JSON token")
	case '"':
		if !sonic.Valid(trimmed) {
			return orderedNode{}, fmt.Errorf("invalid JSON string")
		}
		return orderedNode{kind: orderedString, raw: cloneBytes(trimmed)}, nil
	default:
		if !sonic.Valid(trimmed) {
			return orderedNode{}, fmt.Errorf("invalid JSON number")
		}
		return orderedNode{kind: orderedNumber, raw: cloneBytes(trimmed)}, nil
	}
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// appendTo writes the node's JSON representation into buf. Object keys
// are emitted from the node's keys slice, so the reorder applied by
// the walker is what reaches the wire.
func (n orderedNode) appendTo(buf *bytes.Buffer) error {
	switch n.kind {
	case orderedNull, orderedBool, orderedNumber, orderedString:
		buf.Write(n.raw)
		return nil
	case orderedArray:
		buf.WriteByte('[')
		for i, item := range n.array {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := item.appendTo(buf); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case orderedObject:
		buf.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := sonic.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			child := n.byKey[k]
			if err := child.appendTo(buf); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	}
	return fmt.Errorf("dynamic output order: unknown node kind %d", n.kind)
}

// dynamicOrderWalker drives the schema-guided traversal. The walker is
// stateless except for the schema pointer; recursion through
// self-referential or mutually-recursive class refs terminates because
// the JSON payload itself is finite.
type dynamicOrderWalker struct {
	schema *DynamicOutputSchema
}

// resolveClass returns the schema's class definition for name. The
// synthetic root class is materialised on demand from
// schema.Properties so callers can refer to it via the constant
// dynamicOutputClassName without populating Classes.
func (w *dynamicOrderWalker) resolveClass(name string) (*DynamicClass, error) {
	if name == dynamicOutputClassName {
		return &DynamicClass{Properties: w.schema.Properties}, nil
	}
	cls, ok := w.schema.Classes.Get(name)
	if !ok {
		return nil, fmt.Errorf("dynamic output order: unknown class %q", name)
	}
	return cls, nil
}

func (w *dynamicOrderWalker) isClassRef(name string) bool {
	if name == dynamicOutputClassName {
		return true
	}
	_, ok := w.schema.Classes.Get(name)
	return ok
}

func (w *dynamicOrderWalker) isEnumRef(name string) bool {
	_, ok := w.schema.Enums.Get(name)
	return ok
}

// reorderClass emits the class node's declared properties in schema
// order followed by any extra/unknown input keys in their input order.
// A non-object input at a class position passes through unchanged —
// shape mismatches are FlattenDynamicOutput-domain failures the
// reorder helper does not pretend to authoritatively diagnose.
func (w *dynamicOrderWalker) reorderClass(className string, node orderedNode) (orderedNode, error) {
	cls, err := w.resolveClass(className)
	if err != nil {
		return orderedNode{}, err
	}
	if node.kind != orderedObject {
		return node, nil
	}
	out := orderedNode{
		kind:  orderedObject,
		keys:  make([]string, 0, len(node.keys)),
		byKey: make(map[string]orderedNode, len(node.keys)),
	}
	declared := make(map[string]struct{}, cls.Properties.Len())
	var propErr error
	cls.Properties.Range(func(propName string, prop *DynamicProperty) bool {
		declared[propName] = struct{}{}
		child, present := node.byKey[propName]
		if !present {
			return true
		}
		if prop == nil {
			out.keys = append(out.keys, propName)
			out.byKey[propName] = child
			return true
		}
		spec := propertyAsTypeSpec(prop)
		reordered, err := w.reorderType(spec, child)
		if err != nil {
			propErr = err
			return false
		}
		out.keys = append(out.keys, propName)
		out.byKey[propName] = reordered
		return true
	})
	if propErr != nil {
		return orderedNode{}, propErr
	}
	for _, k := range node.keys {
		if _, isDeclared := declared[k]; isDeclared {
			continue
		}
		out.keys = append(out.keys, k)
		out.byKey[k] = node.byKey[k]
	}
	return out, nil
}

// reorderType dispatches on the type spec's kind. Class refs descend
// into reorderClass; composites recurse through their element types;
// scalars/literals/enums pass through unchanged.
func (w *dynamicOrderWalker) reorderType(spec DynamicTypeSpec, node orderedNode) (orderedNode, error) {
	if spec.Ref != "" {
		if w.isClassRef(spec.Ref) {
			return w.reorderClass(spec.Ref, node)
		}
		if w.isEnumRef(spec.Ref) {
			return node, nil
		}
		return orderedNode{}, fmt.Errorf("dynamic output order: unknown ref %q", spec.Ref)
	}
	switch spec.Type {
	case "optional":
		if node.kind == orderedNull {
			return node, nil
		}
		if spec.Inner == nil {
			return node, nil
		}
		return w.reorderType(*spec.Inner, node)
	case "list":
		if node.kind != orderedArray {
			return node, nil
		}
		if spec.Items == nil {
			return node, nil
		}
		out := orderedNode{kind: orderedArray, array: make([]orderedNode, len(node.array))}
		for i, item := range node.array {
			reordered, err := w.reorderType(*spec.Items, item)
			if err != nil {
				return orderedNode{}, err
			}
			out.array[i] = reordered
		}
		return out, nil
	case "map":
		if node.kind != orderedObject {
			return node, nil
		}
		out := orderedNode{
			kind:  orderedObject,
			keys:  append([]string(nil), node.keys...),
			byKey: make(map[string]orderedNode, len(node.keys)),
		}
		if spec.Values == nil {
			for _, k := range node.keys {
				out.byKey[k] = node.byKey[k]
			}
			return out, nil
		}
		for _, k := range node.keys {
			reordered, err := w.reorderType(*spec.Values, node.byKey[k])
			if err != nil {
				return orderedNode{}, err
			}
			out.byKey[k] = reordered
		}
		return out, nil
	case "union":
		for _, variant := range spec.OneOf {
			if variant == nil {
				continue
			}
			if w.matchesType(*variant, node) {
				return w.reorderType(*variant, node)
			}
		}
		return node, nil
	default:
		return node, nil
	}
}

// matchesType reports whether the runtime JSON node could be a value of
// the given type spec. Used by union dispatch to pick the first
// shape-compatible declared variant.
func (w *dynamicOrderWalker) matchesType(spec DynamicTypeSpec, node orderedNode) bool {
	if spec.Ref != "" {
		if w.isClassRef(spec.Ref) {
			return w.matchesClass(spec.Ref, node)
		}
		if w.isEnumRef(spec.Ref) {
			return w.matchesEnum(spec.Ref, node)
		}
		return false
	}
	switch spec.Type {
	case "string":
		return node.kind == orderedString
	case "int":
		return node.kind == orderedNumber && isIntNumberLiteral(node.raw)
	case "float":
		return node.kind == orderedNumber
	case "bool":
		return node.kind == orderedBool
	case "null":
		return node.kind == orderedNull
	case "optional":
		if node.kind == orderedNull {
			return true
		}
		if spec.Inner == nil {
			return false
		}
		return w.matchesType(*spec.Inner, node)
	case "list":
		if node.kind != orderedArray {
			return false
		}
		if spec.Items == nil || len(node.array) == 0 {
			return true
		}
		return w.matchesType(*spec.Items, node.array[0])
	case "map":
		if node.kind != orderedObject {
			return false
		}
		if spec.Values == nil {
			return true
		}
		for _, k := range node.keys {
			if !w.matchesType(*spec.Values, node.byKey[k]) {
				return false
			}
		}
		return true
	case "union":
		for _, variant := range spec.OneOf {
			if variant == nil {
				continue
			}
			if w.matchesType(*variant, node) {
				return true
			}
		}
		return false
	case "literal_string":
		if node.kind != orderedString {
			return false
		}
		want, ok := spec.Value.(string)
		if !ok {
			return false
		}
		got, ok := decodeJSONString(node.raw)
		if !ok {
			return false
		}
		return got == want
	case "literal_int":
		if node.kind != orderedNumber || !isIntNumberLiteral(node.raw) {
			return false
		}
		return compareLiteralInt(spec.Value, node.raw)
	case "literal_bool":
		if node.kind != orderedBool {
			return false
		}
		want, ok := spec.Value.(bool)
		if !ok {
			return false
		}
		if want {
			return bytes.Equal(node.raw, []byte("true"))
		}
		return bytes.Equal(node.raw, []byte("false"))
	}
	return false
}

// matchesClass reports whether node is an object whose keys are a
// superset of the named class's declared properties. Extra/unknown
// keys are tolerated so union dispatch can pick a class variant even
// when the value carries fields the schema does not enumerate.
func (w *dynamicOrderWalker) matchesClass(name string, node orderedNode) bool {
	if node.kind != orderedObject {
		return false
	}
	cls, err := w.resolveClass(name)
	if err != nil {
		return false
	}
	missing := false
	cls.Properties.Range(func(propName string, _ *DynamicProperty) bool {
		if _, present := node.byKey[propName]; !present {
			missing = true
			return false
		}
		return true
	})
	return !missing
}

func (w *dynamicOrderWalker) matchesEnum(name string, node orderedNode) bool {
	if node.kind != orderedString {
		return false
	}
	enum, ok := w.schema.Enums.Get(name)
	if !ok || enum == nil {
		return false
	}
	val, ok := decodeJSONString(node.raw)
	if !ok {
		return false
	}
	for _, v := range enum.Values {
		if v == nil {
			continue
		}
		if v.Name == val {
			return true
		}
	}
	return false
}

func propertyAsTypeSpec(p *DynamicProperty) DynamicTypeSpec {
	return DynamicTypeSpec{
		Type:   p.Type,
		Ref:    p.Ref,
		Items:  p.Items,
		Inner:  p.Inner,
		OneOf:  p.OneOf,
		Keys:   p.Keys,
		Values: p.Values,
		Value:  p.Value,
	}
}

// isIntNumberLiteral reports whether raw is a JSON number without a
// fractional or exponent component. Used by int / literal_int union
// matching where a number with a `.` or `e` would otherwise be
// classified as float even when written as `1.0`.
func isIntNumberLiteral(raw []byte) bool {
	for _, b := range raw {
		if b == '.' || b == 'e' || b == 'E' {
			return false
		}
	}
	return true
}

func decodeJSONString(raw []byte) (string, bool) {
	var s string
	if err := sonic.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func compareLiteralInt(value any, raw []byte) bool {
	nodeInt, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return false
	}
	switch v := value.(type) {
	case int:
		return int64(v) == nodeInt
	case int32:
		return int64(v) == nodeInt
	case int64:
		return v == nodeInt
	case float64:
		return float64(nodeInt) == v
	}
	return false
}

// SortDynamicOutput re-emits `data` with every JSON object's keys in
// alphabetical order. Used in the PreserveSchemaOrder=false path so
// the wire shape is deterministic and matches the codegen-emitted
// schemaKeys fallback (which alpha-sorts schema iteration when
// preserve is false).
//
// Lists and scalars pass through. Nested objects are recursively
// alpha-sorted. Duplicate object keys are rejected with the same
// error shape ReorderDynamicOutputBySchema surfaces, so the two
// helpers share invariants on malformed inputs. One JSON parse + one
// re-emit; linear in payload size.
func SortDynamicOutput(data []byte) ([]byte, error) {
	node, err := decodeOrderedDynamicJSON(data)
	if err != nil {
		return nil, fmt.Errorf("dynamic output sort: %w", err)
	}
	sorted := alphaSortNode(node)
	var buf bytes.Buffer
	if err := sorted.appendTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// alphaSortNode returns a copy of node with every nested object's
// keys reordered alphabetically. Leaf scalars and arrays pass through
// (array elements are recursively sorted to catch nested objects).
func alphaSortNode(node orderedNode) orderedNode {
	switch node.kind {
	case orderedObject:
		out := orderedNode{
			kind:  orderedObject,
			keys:  append([]string(nil), node.keys...),
			byKey: make(map[string]orderedNode, len(node.keys)),
		}
		sort.Strings(out.keys)
		for _, k := range out.keys {
			out.byKey[k] = alphaSortNode(node.byKey[k])
		}
		return out
	case orderedArray:
		out := orderedNode{kind: orderedArray, array: make([]orderedNode, len(node.array))}
		for i, item := range node.array {
			out.array[i] = alphaSortNode(item)
		}
		return out
	default:
		return node
	}
}
