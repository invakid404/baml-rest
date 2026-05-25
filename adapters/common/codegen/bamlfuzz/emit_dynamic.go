package bamlfuzz

import (
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// ErrDynamicSchemaUnsupported is the wider sentinel both dynamic-skip
// reasons wrap. Callers test errors.Is(err, ErrDynamicSchemaUnsupported)
// to decide whether a schema should be skipped by the dynamic oracle as
// a whole, without enumerating each upstream limitation by hand.
var ErrDynamicSchemaUnsupported = errors.New("bamlfuzz: dynamic emitter does not support this schema")

// ErrDynamicSelfRefUnsupported is returned by LowerToDynamicSchema when
// the supplied FuzzSchema reaches a class through its own type tree.
// Dynamic TypeBuilder cannot currently express self-referential classes;
// TODO(upstream-self-ref) tracks removal of this guard once upstream
// BAML lifts the limitation. Wraps ErrDynamicSchemaUnsupported.
var ErrDynamicSelfRefUnsupported = fmt.Errorf("%w: self-referential schema", ErrDynamicSchemaUnsupported)

// ErrDynamicMutualCycleUnsupported is returned by LowerToDynamicSchema
// when the schema contains a class-ref cycle between two or more
// distinct classes. The BAML cgo TypeBuilder aborts the process with a
// signal-level fault (SIGBUS / SIGILL) when fed such schemas; gating
// here keeps the integration oracle from killing its host process when
// a corpus or rapid case happens to walk into the failure shape.
// TODO(upstream-mutual-rec-dynamic-crash) tracks removal once upstream
// BAML stops aborting on mutual-cycle dynamic schemas. Wraps
// ErrDynamicSchemaUnsupported.
var ErrDynamicMutualCycleUnsupported = fmt.Errorf("%w: mutual-cycle schema", ErrDynamicSchemaUnsupported)

// ErrDynamicRootTypeUnsupported is returned by LowerToDynamicSchema
// when the schema declares a non-class effective root type (a raw
// top-level union / list / map / primitive). The production
// /call/_dynamic endpoint accepts object-shaped output schemas
// only — the contract is "fill in a class" — so the dynamic emitter
// rejects raw roots. Static lowering handles the same shapes
// natively via the synthesized function's return type. Wraps
// ErrDynamicSchemaUnsupported.
var ErrDynamicRootTypeUnsupported = fmt.Errorf("%w: raw non-class root type", ErrDynamicSchemaUnsupported)

// LowerToDynamicSchema lowers a FuzzSchema into a bamlutils.DynamicOutputSchema
// suitable for posting to /call/_dynamic or driving dynclient.Client.DynamicCall.
//
// The lowering preserves declaration order for both top-level properties and
// each class's properties via bamlutils.OrderedMap, so preserve_schema_order=true
// callers observe the same order the FuzzSchema generator emitted.
//
// Self-referential schemas are rejected with ErrDynamicSelfRefUnsupported.
// Mutual-cycle schemas (A→B→A through distinct classes) are rejected with
// ErrDynamicMutualCycleUnsupported pending upstream BAML fix; both errors
// wrap ErrDynamicSchemaUnsupported.
//
// Graph metadata is recomputed via AnalyzeGraph regardless of the stamped
// Has*/Requires* booleans on the input — those fields are documented as
// non-authoritative and a hand-edited corpus or replay artifact may carry
// stale flags. Trusting them here would let a self-ref or mutual-cycle
// schema slip through the guard.
func LowerToDynamicSchema(schema FuzzSchema) (bamlutils.DynamicOutputSchema, error) {
	flags := AnalyzeGraph(schema)
	if flags.HasSelfRef {
		return bamlutils.DynamicOutputSchema{}, fmt.Errorf("%w: root=%q", ErrDynamicSelfRefUnsupported, schema.RootClass)
	}
	if flags.HasMutualCycle {
		return bamlutils.DynamicOutputSchema{}, fmt.Errorf("%w: root=%q", ErrDynamicMutualCycleUnsupported, schema.RootClass)
	}
	if schema.RootType != nil && schema.RootType.Kind != KindClassRef {
		return bamlutils.DynamicOutputSchema{}, fmt.Errorf("%w: root kind=%q", ErrDynamicRootTypeUnsupported, schema.RootType.Kind)
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
	case KindUnion:
		variants, err := lowerVariants(ft.Variants)
		if err != nil {
			return nil, err
		}
		prop.Type = "union"
		prop.OneOf = variants
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
	case KindUnion:
		variants, err := lowerVariants(ft.Variants)
		if err != nil {
			return nil, err
		}
		spec.Type = "union"
		spec.OneOf = variants
	default:
		return nil, fmt.Errorf("unsupported kind %q", ft.Kind)
	}
	return spec, nil
}

// lowerVariants renders each union variant as a DynamicTypeSpec.
// Empty / single-arm variants are rejected: an empty union is
// invalid BAML, and a single-arm union should have been collapsed
// to the bare variant before reaching the dynamic emitter
// (LowerToBamlSource enforces the static-side equivalent).
func lowerVariants(variants []FuzzType) ([]*bamlutils.DynamicTypeSpec, error) {
	if len(variants) == 0 {
		return nil, fmt.Errorf("union has no variants")
	}
	out := make([]*bamlutils.DynamicTypeSpec, len(variants))
	for i, v := range variants {
		spec, err := lowerTypeSpec(v)
		if err != nil {
			return nil, fmt.Errorf("union variant %d: %w", i, err)
		}
		out[i] = spec
	}
	return out, nil
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
