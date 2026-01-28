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
//
// # Overview
//
// DynamicTypes allows you to define BAML types at runtime without writing .baml files.
// This is useful for:
//   - Adding fields to existing dynamic classes based on user input
//   - Creating entirely new types at runtime
//   - Adding values to dynamic enums
//
// # Example Usage
//
//	typeBuilder := &bamlutils.TypeBuilder{
//	    DynamicTypes: &bamlutils.DynamicTypes{
//	        Classes: map[string]*bamlutils.DynamicClass{
//	            "DynamicOutput": {
//	                Properties: map[string]*bamlutils.DynamicProperty{
//	                    "name":   {Type: "string"},
//	                    "age":    {Type: "int"},
//	                    "active": {Type: "bool"},
//	                },
//	            },
//	        },
//	        Enums: map[string]*bamlutils.DynamicEnum{
//	            "Priority": {
//	                Values: []*bamlutils.DynamicEnumValue{
//	                    {Name: "HIGH", Alias: "high priority"},
//	                    {Name: "MEDIUM"},
//	                    {Name: "LOW"},
//	                },
//	            },
//	        },
//	    },
//	}
//
// # Supported Types
//
// Primitive types: "string", "int", "float", "bool", "null"
//
// Composite types:
//   - "list": requires "items" field with element type
//   - "optional": requires "inner" field with wrapped type
//   - "map": requires "keys" and "values" fields
//   - "union": requires "oneOf" array with variant types
//
// Literal types:
//   - "literal_string": requires "value" (string)
//   - "literal_int": requires "value" (integer)
//   - "literal_bool": requires "value" (boolean)
//
// References: use "ref" to reference other classes/enums by name
//
// # JSON Schema Example
//
//	{
//	  "classes": {
//	    "Address": {
//	      "properties": {
//	        "street": {"type": "string"},
//	        "city": {"type": "string"},
//	        "zip": {"type": "optional", "inner": {"type": "string"}}
//	      }
//	    },
//	    "Person": {
//	      "description": "A person with an address",
//	      "properties": {
//	        "name": {"type": "string"},
//	        "age": {"type": "int"},
//	        "address": {"ref": "Address"},
//	        "tags": {"type": "list", "items": {"type": "string"}},
//	        "metadata": {"type": "map", "keys": {"type": "string"}, "values": {"type": "string"}}
//	      }
//	    }
//	  },
//	  "enums": {
//	    "Status": {
//	      "values": [
//	        {"name": "ACTIVE", "alias": "active", "description": "Active status"},
//	        {"name": "INACTIVE", "skip": true}
//	      ]
//	    }
//	  }
//	}
type DynamicTypes struct {
	Classes map[string]*DynamicClass `json:"classes,omitempty"`
	Enums   map[string]*DynamicEnum  `json:"enums,omitempty"`
}

// maxTypeDepth is the maximum nesting depth for type references.
// This prevents stack overflow from maliciously deep or circular type definitions.
const maxTypeDepth = 64

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
			if err := validateTypeRef(prop.Type, prop.Ref, prop.Items, prop.Inner, prop.OneOf, prop.Keys, prop.Values, prop.Value, definedTypes, fmt.Sprintf("class %q property %q", name, propName), 0); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateTypeRef validates a type reference structure
func validateTypeRef(typ, ref string, items, inner *DynamicTypeSpec, oneOf []*DynamicTypeSpec, keys, values *DynamicTypeSpec, value any, definedTypes map[string]bool, path string, depth int) error {
	if depth > maxTypeDepth {
		return fmt.Errorf("%s: type nesting exceeds maximum depth of %d", path, maxTypeDepth)
	}

	// Must have either Type or Ref (or be a literal with Value)
	hasType := typ != ""
	hasRef := ref != ""

	if hasRef {
		if hasType {
			return fmt.Errorf("%s: cannot have both 'type' and 'ref'", path)
		}
		// Reference validation - check if the referenced type is defined
		// Note: we allow references to types not in DynamicTypes as they may exist in baml_src
		return nil
	}

	if !hasType {
		return fmt.Errorf("%s: must have 'type' or 'ref'", path)
	}

	// Validate based on type
	switch typ {
	case "string", "int", "float", "bool", "null":
		// Primitives - no additional fields needed
	case "list":
		if items == nil {
			return fmt.Errorf("%s: 'list' type requires 'items'", path)
		}
		if err := validateDynamicTypeSpec(items, definedTypes, path+"[items]", depth+1); err != nil {
			return err
		}
	case "optional":
		if inner == nil {
			return fmt.Errorf("%s: 'optional' type requires 'inner'", path)
		}
		if err := validateDynamicTypeSpec(inner, definedTypes, path+"[inner]", depth+1); err != nil {
			return err
		}
	case "map":
		if keys == nil || values == nil {
			return fmt.Errorf("%s: 'map' type requires 'keys' and 'values'", path)
		}
		if err := validateDynamicTypeSpec(keys, definedTypes, path+"[keys]", depth+1); err != nil {
			return err
		}
		if err := validateDynamicTypeSpec(values, definedTypes, path+"[values]", depth+1); err != nil {
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
			if err := validateDynamicTypeSpec(variant, definedTypes, fmt.Sprintf("%s[oneOf.%d]", path, i), depth+1); err != nil {
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

// validateDynamicTypeSpec validates a nested DynamicTypeSpec
func validateDynamicTypeSpec(ref *DynamicTypeSpec, definedTypes map[string]bool, path string, depth int) error {
	if ref == nil {
		return fmt.Errorf("%s: type reference is nil", path)
	}
	return validateTypeRef(ref.Type, ref.Ref, ref.Items, ref.Inner, ref.OneOf, ref.Keys, ref.Values, ref.Value, definedTypes, path, depth)
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
	Ref  string `json:"ref,omitempty"`  // Reference to another class/enum by name

	// Metadata
	Description string `json:"description,omitempty"`
	Alias       string `json:"alias,omitempty"`

	// For composite types
	Items  *DynamicTypeSpec   `json:"items,omitempty"`  // For "list" type
	Inner  *DynamicTypeSpec   `json:"inner,omitempty"`  // For "optional" type
	OneOf  []*DynamicTypeSpec `json:"oneOf,omitempty"`  // For "union" type
	Keys   *DynamicTypeSpec   `json:"keys,omitempty"`   // For "map" type
	Values *DynamicTypeSpec   `json:"values,omitempty"` // For "map" type

	// For literal types
	Value any `json:"value,omitempty"` // For literal_string, literal_int, literal_bool
}

// DynamicTypeSpec is a recursive type specification used in composite types.
// Note: This type was renamed from DynamicTypeRef to avoid a heuristic in
// kin-openapi's openapi3gen that treats types ending in "Ref" with both
// "Ref" and "Value" fields specially, generating incorrect oneOf schemas.
type DynamicTypeSpec struct {
	Type string `json:"type,omitempty"` // Primitive or composite type name
	Ref  string `json:"ref,omitempty"`  // Reference to class/enum

	// For nested composite types
	Items  *DynamicTypeSpec   `json:"items,omitempty"`
	Inner  *DynamicTypeSpec   `json:"inner,omitempty"`
	OneOf  []*DynamicTypeSpec `json:"oneOf,omitempty"`
	Keys   *DynamicTypeSpec   `json:"keys,omitempty"`
	Values *DynamicTypeSpec   `json:"values,omitempty"`
	Value  any               `json:"value,omitempty"`
}

// DynamicEnum defines an enum with values.
// Use this to add new values to existing dynamic enums or create new enums entirely.
type DynamicEnum struct {
	Description string              `json:"description,omitempty"` // Description shown to the LLM
	Alias       string              `json:"alias,omitempty"`       // Alternative name the LLM can use
	Values      []*DynamicEnumValue `json:"values,omitempty"`      // List of enum values
}

// DynamicEnumValue defines a single enum value.
//
// Example with all fields:
//
//	{
//	    "name": "HIGH",
//	    "description": "High priority items that need immediate attention",
//	    "alias": "high priority",
//	    "skip": false
//	}
//
// The Alias field is particularly useful for allowing the LLM to output
// natural language values (like "high priority") that get mapped to
// the canonical enum name ("HIGH").
type DynamicEnumValue struct {
	Name        string `json:"name"`                    // Required: the canonical enum value name
	Description string `json:"description,omitempty"`   // Optional: description shown to the LLM
	Alias       string `json:"alias,omitempty"`         // Optional: alternative name the LLM can output
	Skip        bool   `json:"skip,omitempty"`          // Optional: if true, this value is hidden from the LLM
}

// Logger is a minimal logging interface compatible with hclog.Logger.
type Logger interface {
	Debug(msg string, args ...interface{})
	Info(msg string, args ...interface{})
	Warn(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}

type Adapter interface {
	context.Context
	SetClientRegistry(clientRegistry *ClientRegistry) error
	SetTypeBuilder(typeBuilder *TypeBuilder) error
	// SetStreamMode sets the streaming mode which controls partial forwarding and raw collection.
	SetStreamMode(mode StreamMode)
	// StreamMode returns the current streaming mode.
	StreamMode() StreamMode
	// SetLogger sets the logger for debug output.
	SetLogger(logger Logger)
	// Logger returns the current logger, or nil if not set.
	Logger() Logger
}

// BamlOptions contains optional configuration for BAML method calls
type BamlOptions struct {
	ClientRegistry *ClientRegistry `json:"client_registry"`
	TypeBuilder    *TypeBuilder    `json:"type_builder"`
}
