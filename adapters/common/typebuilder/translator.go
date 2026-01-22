// Package typebuilder provides a translator that converts DynamicTypes JSON schema
// definitions to imperative BAML TypeBuilder calls.
package typebuilder

import (
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// ErrUnresolvedRef is returned when a $ref cannot be resolved to a known type.
// This is expected when referencing types defined in baml_src that we can't access.
var ErrUnresolvedRef = errors.New("unresolved reference")

// Translator converts DynamicTypes to imperative TypeBuilder calls.
type Translator struct {
	tb        bamlutils.BamlTypeBuilder
	typeCache map[string]bamlutils.BamlType
}

// NewTranslator creates a new Translator for the given BamlTypeBuilder.
func NewTranslator(tb bamlutils.BamlTypeBuilder) *Translator {
	return &Translator{
		tb:        tb,
		typeCache: make(map[string]bamlutils.BamlType),
	}
}

// Apply processes DynamicTypes and makes imperative TypeBuilder calls.
// Processing order:
// 1. Create all enums (no dependencies)
// 2. Create all class shells (for forward references)
// 3. Add properties to classes (all refs now resolvable)
func (t *Translator) Apply(dt *bamlutils.DynamicTypes) error {
	if dt == nil {
		return nil
	}

	// Phase 1: Create all enums (they have no dependencies)
	for name, enum := range dt.Enums {
		if err := t.createEnum(name, enum); err != nil {
			return fmt.Errorf("enum %q: %w", name, err)
		}
	}

	// Phase 2: Create all class shells (for forward references)
	for name, class := range dt.Classes {
		if err := t.createClassShell(name, class); err != nil {
			return fmt.Errorf("class %q: %w", name, err)
		}
	}

	// Phase 3: Add properties to classes (all refs now resolvable)
	for name, class := range dt.Classes {
		if err := t.addClassProperties(name, class); err != nil {
			return fmt.Errorf("class %q properties: %w", name, err)
		}
	}

	return nil
}

func (t *Translator) createEnum(name string, enum *bamlutils.DynamicEnum) error {
	// Try to get existing enum first, create if it doesn't exist
	eb, err := t.tb.Enum(name)
	if err != nil {
		// Enum doesn't exist, create it
		eb, err = t.tb.AddEnum(name)
		if err != nil {
			return err
		}
	}

	// Note: SetDescription and SetAlias are not exported in the native BAML Go library.
	// The description/alias fields in DynamicEnum are preserved for potential future use
	// or for documentation purposes, but they are not applied at runtime.

	for _, v := range enum.Values {
		vb, err := eb.AddValue(v.Name)
		if err != nil {
			// Value might already exist, skip silently
			continue
		}
		// Note: SetDescription and SetAlias are not exported for enum values either.
		if v.Skip {
			if err := vb.SetSkip(true); err != nil {
				return fmt.Errorf("value %q set skip: %w", v.Name, err)
			}
		}
	}

	// Cache the enum type for references
	typ, err := eb.Type()
	if err != nil {
		return fmt.Errorf("get type: %w", err)
	}
	t.typeCache[name] = typ

	return nil
}

func (t *Translator) createClassShell(name string, class *bamlutils.DynamicClass) error {
	// Try to get existing class first, create if it doesn't exist
	cb, err := t.tb.Class(name)
	if err != nil {
		// Class doesn't exist, create it
		cb, err = t.tb.AddClass(name)
		if err != nil {
			return err
		}
	}

	// Note: SetDescription and SetAlias are not exported in the native BAML Go library.
	// The description/alias fields in DynamicClass are preserved for potential future use
	// or for documentation purposes, but they are not applied at runtime.

	// Cache the class type for references
	typ, err := cb.Type()
	if err != nil {
		return fmt.Errorf("get type: %w", err)
	}
	t.typeCache[name] = typ

	return nil
}

func (t *Translator) addClassProperties(name string, class *bamlutils.DynamicClass) error {
	cb, err := t.tb.Class(name)
	if err != nil {
		return err
	}

	for propName, prop := range class.Properties {
		typ, err := t.resolvePropertyType(prop)
		if err != nil {
			// Only skip unresolved refs - they may reference types in baml_src
			// Other errors (e.g., invalid type specs) should be reported
			if errors.Is(err, ErrUnresolvedRef) {
				continue
			}
			return fmt.Errorf("property %q type: %w", propName, err)
		}

		_, err = cb.AddProperty(propName, typ)
		if err != nil {
			return fmt.Errorf("property %q: %w", propName, err)
		}

		// Note: SetDescription and SetAlias are not exported in the native BAML Go library
		// for property builders. The description/alias fields in DynamicProperty are preserved
		// for potential future use or for documentation purposes.
	}

	return nil
}

func (t *Translator) resolvePropertyType(prop *bamlutils.DynamicProperty) (bamlutils.BamlType, error) {
	// Handle $ref
	if prop.Ref != "" {
		return t.resolveRef(prop.Ref)
	}

	// Convert to DynamicTypeRef and resolve
	return t.resolveTypeRef(&bamlutils.DynamicTypeRef{
		Type:   prop.Type,
		Items:  prop.Items,
		Inner:  prop.Inner,
		OneOf:  prop.OneOf,
		Keys:   prop.Keys,
		Values: prop.Values,
		Value:  prop.Value,
	})
}

func (t *Translator) resolveTypeRef(ref *bamlutils.DynamicTypeRef) (bamlutils.BamlType, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil type reference")
	}

	// Handle $ref
	if ref.Ref != "" {
		return t.resolveRef(ref.Ref)
	}

	switch ref.Type {
	case "string":
		return t.tb.String()
	case "int":
		return t.tb.Int()
	case "float":
		return t.tb.Float()
	case "bool":
		return t.tb.Bool()
	case "null":
		return t.tb.Null()

	case "literal_string":
		str, ok := ref.Value.(string)
		if !ok {
			return nil, fmt.Errorf("literal_string value must be a string, got %T", ref.Value)
		}
		return t.tb.LiteralString(str)

	case "literal_int":
		// JSON numbers are float64
		switch v := ref.Value.(type) {
		case float64:
			return t.tb.LiteralInt(int64(v))
		case int64:
			return t.tb.LiteralInt(v)
		case int:
			return t.tb.LiteralInt(int64(v))
		default:
			return nil, fmt.Errorf("literal_int value must be a number, got %T", ref.Value)
		}

	case "literal_bool":
		b, ok := ref.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("literal_bool value must be a boolean, got %T", ref.Value)
		}
		return t.tb.LiteralBool(b)

	case "list":
		if ref.Items == nil {
			return nil, fmt.Errorf("list type requires 'items' field")
		}
		inner, err := t.resolveTypeRef(ref.Items)
		if err != nil {
			return nil, fmt.Errorf("list items: %w", err)
		}
		return t.tb.List(inner)

	case "optional":
		if ref.Inner == nil {
			return nil, fmt.Errorf("optional type requires 'inner' field")
		}
		inner, err := t.resolveTypeRef(ref.Inner)
		if err != nil {
			return nil, fmt.Errorf("optional inner: %w", err)
		}
		return t.tb.Optional(inner)

	case "map":
		if ref.Keys == nil {
			return nil, fmt.Errorf("map type requires 'keys' field")
		}
		if ref.Values == nil {
			return nil, fmt.Errorf("map type requires 'values' field")
		}
		keys, err := t.resolveTypeRef(ref.Keys)
		if err != nil {
			return nil, fmt.Errorf("map keys: %w", err)
		}
		values, err := t.resolveTypeRef(ref.Values)
		if err != nil {
			return nil, fmt.Errorf("map values: %w", err)
		}
		return t.tb.Map(keys, values)

	case "union":
		if len(ref.OneOf) == 0 {
			return nil, fmt.Errorf("union type requires 'oneOf' field with at least one type")
		}
		types := make([]bamlutils.BamlType, 0, len(ref.OneOf))
		for i, item := range ref.OneOf {
			typ, err := t.resolveTypeRef(item)
			if err != nil {
				return nil, fmt.Errorf("union oneOf[%d]: %w", i, err)
			}
			types = append(types, typ)
		}
		return t.tb.Union(types)

	case "":
		return nil, fmt.Errorf("type field is required")

	default:
		return nil, fmt.Errorf("unknown type: %q", ref.Type)
	}
}

func (t *Translator) resolveRef(name string) (bamlutils.BamlType, error) {
	// Check our cache first (from dynamic_types)
	if typ, ok := t.typeCache[name]; ok {
		return typ, nil
	}

	// Try existing class in BAML runtime
	if cb, err := t.tb.Class(name); err == nil {
		if typ, err := cb.Type(); err == nil {
			t.typeCache[name] = typ
			return typ, nil
		}
	}

	// Try existing enum in BAML runtime
	if eb, err := t.tb.Enum(name); err == nil {
		if typ, err := eb.Type(); err == nil {
			t.typeCache[name] = typ
			return typ, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrUnresolvedRef, name)
}
