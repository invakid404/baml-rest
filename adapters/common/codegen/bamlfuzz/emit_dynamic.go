package bamlfuzz

import (
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// ErrDynamicSelfRefUnsupported is returned by LowerToDynamicSchema when
// the supplied FuzzSchema reaches a class through its own type tree.
// Dynamic TypeBuilder cannot currently express self-referential classes;
// TODO(upstream-self-ref) tracks removal of this guard once upstream
// BAML lifts the limitation. Callers detect via errors.Is.
var ErrDynamicSelfRefUnsupported = errors.New("bamlfuzz: dynamic emitter rejects self-referential schema")

// LowerToDynamicSchema lowers a FuzzSchema into a bamlutils.DynamicOutputSchema
// suitable for posting to /call/_dynamic or driving dynclient.Client.DynamicCall.
//
// The lowering preserves declaration order for both top-level properties and
// each class's properties via bamlutils.OrderedMap, so preserve_schema_order=true
// callers observe the same order the FuzzSchema generator emitted.
//
// Self-referential schemas are rejected with ErrDynamicSelfRefUnsupported.
// Mutual recursion through distinct classes is permitted — the value generator
// terminates such cycles via per-class recursion caps.
func LowerToDynamicSchema(schema FuzzSchema) (bamlutils.DynamicOutputSchema, error) {
	if schema.HasSelfRef || schema.RequiresDynamicSkip {
		return bamlutils.DynamicOutputSchema{}, fmt.Errorf("%w: root=%q", ErrDynamicSelfRefUnsupported, schema.RootClass)
	}
	rootCls, ok := schema.FindClass(schema.RootClass)
	if !ok {
		return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: root class %q not present in schema", schema.RootClass)
	}

	out := bamlutils.DynamicOutputSchema{}

	for _, prop := range rootCls.Properties {
		dprop, err := lowerProperty(prop.Type)
		if err != nil {
			return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: root.%s: %w", prop.Name, err)
		}
		if err := out.Properties.Set(prop.Name, dprop); err != nil {
			return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: root.%s: %w", prop.Name, err)
		}
	}

	for _, cls := range schema.Classes {
		dcls := &bamlutils.DynamicClass{}
		for _, prop := range cls.Properties {
			dprop, err := lowerProperty(prop.Type)
			if err != nil {
				return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: %s.%s: %w", cls.Name, prop.Name, err)
			}
			if err := dcls.Properties.Set(prop.Name, dprop); err != nil {
				return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: %s.%s: %w", cls.Name, prop.Name, err)
			}
		}
		if err := out.Classes.Set(cls.Name, dcls); err != nil {
			return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: class %s: %w", cls.Name, err)
		}
	}

	for _, enum := range schema.Enums {
		values := make([]*bamlutils.DynamicEnumValue, len(enum.Values))
		for i, v := range enum.Values {
			values[i] = &bamlutils.DynamicEnumValue{Name: v}
		}
		if err := out.Enums.Set(enum.Name, &bamlutils.DynamicEnum{Values: values}); err != nil {
			return bamlutils.DynamicOutputSchema{}, fmt.Errorf("bamlfuzz: enum %s: %w", enum.Name, err)
		}
	}

	return out, nil
}

// lowerProperty produces the DynamicProperty for a top-level class field.
// Top-level properties carry the full set of fields (Items, Inner, Keys,
// Values, Value, Ref) directly on DynamicProperty; nested composites use the
// recursive DynamicTypeSpec via lowerTypeSpec.
func lowerProperty(ft FuzzType) (*bamlutils.DynamicProperty, error) {
	prop := &bamlutils.DynamicProperty{}
	switch ft.Kind {
	case KindString, KindInt, KindFloat, KindBool, KindNull:
		prop.Type = string(ft.Kind)
	case KindLiteral:
		if ft.Literal == nil {
			return nil, fmt.Errorf("literal property missing payload")
		}
		t, v, err := literalTypeAndValue(ft.Literal)
		if err != nil {
			return nil, err
		}
		prop.Type = t
		prop.Value = v
	case KindOptional:
		if ft.Inner == nil {
			return nil, fmt.Errorf("optional missing inner")
		}
		inner, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, err
		}
		prop.Type = "optional"
		prop.Inner = inner
	case KindList:
		if ft.Inner == nil {
			return nil, fmt.Errorf("list missing inner")
		}
		items, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, err
		}
		prop.Type = "list"
		prop.Items = items
	case KindMap:
		if ft.Key == nil || ft.Inner == nil {
			return nil, fmt.Errorf("map missing key or inner")
		}
		keys, err := lowerTypeSpec(*ft.Key)
		if err != nil {
			return nil, fmt.Errorf("map keys: %w", err)
		}
		values, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, fmt.Errorf("map values: %w", err)
		}
		prop.Type = "map"
		prop.Keys = keys
		prop.Values = values
	case KindClassRef, KindEnumRef:
		if ft.Ref == "" {
			return nil, fmt.Errorf("ref kind %q missing target", ft.Kind)
		}
		prop.Ref = ft.Ref
	default:
		return nil, fmt.Errorf("unsupported kind %q", ft.Kind)
	}
	return prop, nil
}

// lowerTypeSpec produces the recursive DynamicTypeSpec used inside composite
// types (list items, optional inner, map keys/values).
func lowerTypeSpec(ft FuzzType) (*bamlutils.DynamicTypeSpec, error) {
	spec := &bamlutils.DynamicTypeSpec{}
	switch ft.Kind {
	case KindString, KindInt, KindFloat, KindBool, KindNull:
		spec.Type = string(ft.Kind)
	case KindLiteral:
		if ft.Literal == nil {
			return nil, fmt.Errorf("literal spec missing payload")
		}
		t, v, err := literalTypeAndValue(ft.Literal)
		if err != nil {
			return nil, err
		}
		spec.Type = t
		spec.Value = v
	case KindOptional:
		if ft.Inner == nil {
			return nil, fmt.Errorf("optional missing inner")
		}
		inner, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, err
		}
		spec.Type = "optional"
		spec.Inner = inner
	case KindList:
		if ft.Inner == nil {
			return nil, fmt.Errorf("list missing inner")
		}
		items, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, err
		}
		spec.Type = "list"
		spec.Items = items
	case KindMap:
		if ft.Key == nil || ft.Inner == nil {
			return nil, fmt.Errorf("map missing key or inner")
		}
		keys, err := lowerTypeSpec(*ft.Key)
		if err != nil {
			return nil, fmt.Errorf("map keys: %w", err)
		}
		values, err := lowerTypeSpec(*ft.Inner)
		if err != nil {
			return nil, fmt.Errorf("map values: %w", err)
		}
		spec.Type = "map"
		spec.Keys = keys
		spec.Values = values
	case KindClassRef, KindEnumRef:
		if ft.Ref == "" {
			return nil, fmt.Errorf("ref kind %q missing target", ft.Kind)
		}
		spec.Ref = ft.Ref
	default:
		return nil, fmt.Errorf("unsupported kind %q", ft.Kind)
	}
	return spec, nil
}

func literalTypeAndValue(lit *FuzzLiteral) (string, any, error) {
	switch lit.Kind {
	case LiteralString:
		return "literal_string", lit.String, nil
	case LiteralInt:
		return "literal_int", lit.Int, nil
	case LiteralBool:
		return "literal_bool", lit.Bool, nil
	default:
		return "", nil, fmt.Errorf("unknown literal kind %q", lit.Kind)
	}
}
