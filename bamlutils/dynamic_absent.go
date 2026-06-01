package bamlutils

import (
	"bytes"
	"fmt"
)

// InjectAbsentOptionals walks the schema and inserts JSON null for any
// declared optional property absent from data. Applied after
// FlattenDynamicOutput and before reorder/sort so key presence is
// consistent regardless of preserve_schema_order.
func InjectAbsentOptionals(data []byte, schema *DynamicOutputSchema) ([]byte, error) {
	if schema == nil || schema.Properties.Len() == 0 {
		return data, nil
	}
	node, err := decodeOrderedDynamicJSON(data)
	if err != nil {
		return nil, fmt.Errorf("inject absent optionals: %w", err)
	}
	w := &absentOptionalWalker{schema: schema}
	injected, err := w.injectClass(dynamicOutputClassName, node)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := injected.appendTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type absentOptionalWalker struct {
	schema *DynamicOutputSchema
}

func (w *absentOptionalWalker) resolveClass(name string) (*DynamicClass, error) {
	if name == dynamicOutputClassName {
		return &DynamicClass{Properties: w.schema.Properties}, nil
	}
	cls, ok := w.schema.Classes.Get(name)
	if !ok {
		return nil, fmt.Errorf("inject absent optionals: unknown class %q", name)
	}
	return cls, nil
}

func (w *absentOptionalWalker) isClassRef(name string) bool {
	if name == dynamicOutputClassName {
		return true
	}
	_, ok := w.schema.Classes.Get(name)
	return ok
}

// injectClass walks a class node, inserting null for absent optional
// properties and recursing into present properties that may contain
// nested classes with their own absent optionals.
func (w *absentOptionalWalker) injectClass(className string, node orderedNode) (orderedNode, error) {
	cls, err := w.resolveClass(className)
	if err != nil {
		return orderedNode{}, err
	}
	if node.kind != orderedObject {
		return node, nil
	}
	out := orderedNode{
		kind:  orderedObject,
		keys:  make([]string, 0, len(node.keys)+cls.Properties.Len()),
		byKey: make(map[string]orderedNode, len(node.keys)+cls.Properties.Len()),
	}
	declared := make(map[string]struct{}, cls.Properties.Len())
	var propErr error
	cls.Properties.Range(func(propName string, prop *DynamicProperty) bool {
		declared[propName] = struct{}{}
		child, present := node.byKey[propName]
		if !present {
			if prop != nil && prop.Type == "optional" {
				out.keys = append(out.keys, propName)
				out.byKey[propName] = orderedNode{kind: orderedNull, raw: []byte("null")}
			}
			return true
		}
		if prop == nil {
			out.keys = append(out.keys, propName)
			out.byKey[propName] = child
			return true
		}
		spec := propertyAsTypeSpec(prop)
		injected, err := w.injectType(spec, child)
		if err != nil {
			propErr = err
			return false
		}
		out.keys = append(out.keys, propName)
		out.byKey[propName] = injected
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

// injectType recurses into composite types to reach nested class
// instances that may have their own absent optionals.
func (w *absentOptionalWalker) injectType(spec DynamicTypeSpec, node orderedNode) (orderedNode, error) {
	if spec.Ref != "" {
		if w.isClassRef(spec.Ref) {
			return w.injectClass(spec.Ref, node)
		}
		return node, nil
	}
	switch spec.Type {
	case "optional":
		if node.kind == orderedNull {
			return node, nil
		}
		if spec.Inner == nil {
			return node, nil
		}
		return w.injectType(*spec.Inner, node)
	case "list":
		if node.kind != orderedArray || spec.Items == nil {
			return node, nil
		}
		out := orderedNode{kind: orderedArray, array: make([]orderedNode, len(node.array))}
		for i, item := range node.array {
			injected, err := w.injectType(*spec.Items, item)
			if err != nil {
				return orderedNode{}, err
			}
			out.array[i] = injected
		}
		return out, nil
	case "map":
		if node.kind != orderedObject || spec.Values == nil {
			return node, nil
		}
		out := orderedNode{
			kind:  orderedObject,
			keys:  append([]string(nil), node.keys...),
			byKey: make(map[string]orderedNode, len(node.keys)),
		}
		for _, k := range node.keys {
			injected, err := w.injectType(*spec.Values, node.byKey[k])
			if err != nil {
				return orderedNode{}, err
			}
			out.byKey[k] = injected
		}
		return out, nil
	case "union":
		for _, variant := range spec.OneOf {
			if variant == nil {
				continue
			}
			if w.matchesType(*variant, node) {
				return w.injectType(*variant, node)
			}
		}
		return node, nil
	default:
		return node, nil
	}
}

// matchesType delegates to the dynamic order walker's matching logic.
func (w *absentOptionalWalker) matchesType(spec DynamicTypeSpec, node orderedNode) bool {
	orderWalker := &dynamicOrderWalker{schema: w.schema}
	return orderWalker.matchesType(spec, node)
}
