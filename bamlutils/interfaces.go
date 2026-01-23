package bamlutils

import (
	"context"
	"fmt"
)

type StreamResult interface {
	Kind() StreamResultKind
	Stream() any
	Final() any
	Error() error
	// Raw returns the raw LLM response text at this streaming point
	Raw() string
	// Reset returns true if the client should discard accumulated state (retry occurred)
	Reset() bool
	// Release returns the StreamResult to a pool for reuse.
	// After calling Release, the StreamResult should not be accessed.
	Release()
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
	StreamResultKindHeartbeat
)

// StreamMode controls how streaming results are processed and what data is collected.
type StreamMode int

const (
	// StreamModeCall - final only, no raw, no partials (for /call endpoint)
	StreamModeCall StreamMode = iota
	// StreamModeStream - partials + final, no raw (for /stream endpoint)
	StreamModeStream
	// StreamModeCallWithRaw - final + raw, no partials (for /call-with-raw endpoint)
	StreamModeCallWithRaw
	// StreamModeStreamWithRaw - partials + final + raw (for /stream-with-raw endpoint)
	StreamModeStreamWithRaw
)

// NeedsRaw returns true if this mode requires raw LLM response collection.
func (m StreamMode) NeedsRaw() bool {
	return m == StreamModeCallWithRaw || m == StreamModeStreamWithRaw
}

// NeedsPartials returns true if this mode requires forwarding partial/intermediate results.
func (m StreamMode) NeedsPartials() bool {
	return m == StreamModeStream || m == StreamModeStreamWithRaw
}

type StreamingPrompt func(adapter Adapter, input any) (<-chan StreamResult, error)

type StreamingMethod struct {
	MakeInput        func() any
	MakeOutput       func() any
	MakeStreamOutput func() any // Stream/partial type (may differ from final output type)
	Impl             StreamingPrompt
}

type ParsePrompt func(adapter Adapter, raw string) (any, error)

type ParseMethod struct {
	MakeOutput func() any
	Impl       ParsePrompt
}

type ClientRegistry struct {
	Primary *string           `json:"primary"`
	Clients []*ClientProperty `json:"clients"`
}

type ClientProperty struct {
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	RetryPolicy *string        `json:"retry_policy"`
	Options     map[string]any `json:"options,omitempty"`
}

type TypeBuilder struct {
	BamlSnippets []string      `json:"baml_snippets,omitempty"`
	DynamicTypes *DynamicTypes `json:"dynamic_types,omitempty"`
}

func (b *TypeBuilder) Add(input string) {
	b.BamlSnippets = append(b.BamlSnippets, input)
}

// DynamicTypes defines classes and enums to be created via the imperative TypeBuilder API.
// This provides a JSON schema-like structure for defining types programmatically.
type DynamicTypes struct {
	Classes map[string]*DynamicClass `json:"classes,omitempty"`
	Enums   map[string]*DynamicEnum  `json:"enums,omitempty"`
}

// Validate checks the DynamicTypes schema for errors before processing.
// Returns nil if valid, or an error describing the first issue found.
func (dt *DynamicTypes) Validate() error {
	if dt == nil {
		return nil
	}

	// Collect all defined type names for reference validation
	definedTypes := make(map[string]bool)
	for name := range dt.Classes {
		definedTypes[name] = true
	}
	for name := range dt.Enums {
		definedTypes[name] = true
	}

	// Validate enums
	for name, enum := range dt.Enums {
		if enum == nil {
			return fmt.Errorf("enum %q: definition is nil", name)
		}
		for i, v := range enum.Values {
			if v == nil {
				return fmt.Errorf("enum %q: value at index %d is nil", name, i)
			}
			if v.Name == "" {
				return fmt.Errorf("enum %q: value at index %d has empty name", name, i)
			}
		}
	}

	// Validate classes
	for name, class := range dt.Classes {
		if class == nil {
			return fmt.Errorf("class %q: definition is nil", name)
		}
		for propName, prop := range class.Properties {
			if prop == nil {
				return fmt.Errorf("class %q: property %q is nil", name, propName)
			}
			if err := validateTypeRef(prop.Type, prop.Ref, prop.Items, prop.Inner, prop.OneOf, prop.Keys, prop.Values, prop.Value, definedTypes, fmt.Sprintf("class %q property %q", name, propName)); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateTypeRef validates a type reference structure
func validateTypeRef(typ, ref string, items, inner *DynamicTypeRef, oneOf []*DynamicTypeRef, keys, values *DynamicTypeRef, value any, definedTypes map[string]bool, path string) error {
	// Must have either Type or Ref (or be a literal with Value)
	hasType := typ != ""
	hasRef := ref != ""

	if hasRef {
		if hasType {
			return fmt.Errorf("%s: cannot have both 'type' and '$ref'", path)
		}
		// Reference validation - check if the referenced type is defined
		// Note: we allow references to types not in DynamicTypes as they may exist in baml_src
		return nil
	}

	if !hasType {
		return fmt.Errorf("%s: must have 'type' or '$ref'", path)
	}

	// Validate based on type
	switch typ {
	case "string", "int", "float", "bool", "null":
		// Primitives - no additional fields needed
	case "list":
		if items == nil {
			return fmt.Errorf("%s: 'list' type requires 'items'", path)
		}
		if err := validateDynamicTypeRef(items, definedTypes, path+"[items]"); err != nil {
			return err
		}
	case "optional":
		if inner == nil {
			return fmt.Errorf("%s: 'optional' type requires 'inner'", path)
		}
		if err := validateDynamicTypeRef(inner, definedTypes, path+"[inner]"); err != nil {
			return err
		}
	case "map":
		if keys == nil || values == nil {
			return fmt.Errorf("%s: 'map' type requires 'keys' and 'values'", path)
		}
		if err := validateDynamicTypeRef(keys, definedTypes, path+"[keys]"); err != nil {
			return err
		}
		if err := validateDynamicTypeRef(values, definedTypes, path+"[values]"); err != nil {
			return err
		}
	case "union":
		if len(oneOf) == 0 {
			return fmt.Errorf("%s: 'union' type requires 'oneOf' with at least one type", path)
		}
		for i, variant := range oneOf {
			if variant == nil {
				return fmt.Errorf("%s: 'oneOf[%d]' is nil", path, i)
			}
			if err := validateDynamicTypeRef(variant, definedTypes, fmt.Sprintf("%s[oneOf.%d]", path, i)); err != nil {
				return err
			}
		}
	case "literal_string":
		if value == nil {
			return fmt.Errorf("%s: 'literal_string' type requires 'value'", path)
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: 'literal_string' value must be a string", path)
		}
	case "literal_int":
		if value == nil {
			return fmt.Errorf("%s: 'literal_int' type requires 'value'", path)
		}
		// JSON numbers unmarshal as float64
		switch v := value.(type) {
		case int, int64, float64:
			// ok
		default:
			return fmt.Errorf("%s: 'literal_int' value must be an integer, got %T", path, v)
		}
	case "literal_bool":
		if value == nil {
			return fmt.Errorf("%s: 'literal_bool' type requires 'value'", path)
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: 'literal_bool' value must be a boolean", path)
		}
	default:
		return fmt.Errorf("%s: unknown type %q", path, typ)
	}

	return nil
}

// validateDynamicTypeRef validates a nested DynamicTypeRef
func validateDynamicTypeRef(ref *DynamicTypeRef, definedTypes map[string]bool, path string) error {
	if ref == nil {
		return fmt.Errorf("%s: type reference is nil", path)
	}
	return validateTypeRef(ref.Type, ref.Ref, ref.Items, ref.Inner, ref.OneOf, ref.Keys, ref.Values, ref.Value, definedTypes, path)
}

// DynamicClass defines a class with properties.
type DynamicClass struct {
	Description string                      `json:"description,omitempty"`
	Alias       string                      `json:"alias,omitempty"`
	Properties  map[string]*DynamicProperty `json:"properties,omitempty"`
}

// DynamicProperty defines a property on a class.
type DynamicProperty struct {
	// Type specification - use Type for primitives/composites, Ref for references
	Type string `json:"type,omitempty"` // "string", "int", "float", "bool", "null", "list", "optional", "map", "union", "literal_string", "literal_int", "literal_bool"
	Ref  string `json:"$ref,omitempty"` // Reference to another class/enum by name

	// Metadata
	Description string `json:"description,omitempty"`
	Alias       string `json:"alias,omitempty"`

	// For composite types
	Items  *DynamicTypeRef   `json:"items,omitempty"`  // For "list" type
	Inner  *DynamicTypeRef   `json:"inner,omitempty"`  // For "optional" type
	OneOf  []*DynamicTypeRef `json:"oneOf,omitempty"`  // For "union" type
	Keys   *DynamicTypeRef   `json:"keys,omitempty"`   // For "map" type
	Values *DynamicTypeRef   `json:"values,omitempty"` // For "map" type

	// For literal types
	Value any `json:"value,omitempty"` // For literal_string, literal_int, literal_bool
}

// DynamicTypeRef is a recursive type reference used in composite types.
type DynamicTypeRef struct {
	Type string `json:"type,omitempty"` // Primitive or composite type name
	Ref  string `json:"$ref,omitempty"` // Reference to class/enum

	// For nested composite types
	Items  *DynamicTypeRef   `json:"items,omitempty"`
	Inner  *DynamicTypeRef   `json:"inner,omitempty"`
	OneOf  []*DynamicTypeRef `json:"oneOf,omitempty"`
	Keys   *DynamicTypeRef   `json:"keys,omitempty"`
	Values *DynamicTypeRef   `json:"values,omitempty"`
	Value  any               `json:"value,omitempty"`
}

// DynamicEnum defines an enum with values.
type DynamicEnum struct {
	Description string              `json:"description,omitempty"`
	Alias       string              `json:"alias,omitempty"`
	Values      []*DynamicEnumValue `json:"values,omitempty"`
}

// DynamicEnumValue defines a single enum value.
type DynamicEnumValue struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Skip        bool   `json:"skip,omitempty"`
}

type Adapter interface {
	context.Context
	SetClientRegistry(clientRegistry *ClientRegistry) error
	SetTypeBuilder(typeBuilder *TypeBuilder) error
	// SetStreamMode sets the streaming mode which controls partial forwarding and raw collection.
	SetStreamMode(mode StreamMode)
	// StreamMode returns the current streaming mode.
	StreamMode() StreamMode
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
}
