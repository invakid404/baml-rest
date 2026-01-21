package bamlutils

import (
	"context"
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

// BamlType is a marker interface for BAML types returned by the TypeBuilder.
type BamlType interface{}

// BamlTypeBuilder interface for the native BAML TypeBuilder.
type BamlTypeBuilder interface {
	// AddBaml adds raw BAML schema definitions.
	AddBaml(baml string) error

	// Basic types
	String() (BamlType, error)
	Int() (BamlType, error)
	Float() (BamlType, error)
	Bool() (BamlType, error)
	Null() (BamlType, error)

	// Literal types
	LiteralString(value string) (BamlType, error)
	LiteralInt(value int64) (BamlType, error)
	LiteralBool(value bool) (BamlType, error)

	// Composite types
	List(inner BamlType) (BamlType, error)
	Optional(inner BamlType) (BamlType, error)
	Union(types []BamlType) (BamlType, error)
	Map(key, value BamlType) (BamlType, error)

	// Class operations
	AddClass(name string) (BamlClassBuilder, error)
	Class(name string) (BamlClassBuilder, error)

	// Enum operations
	AddEnum(name string) (BamlEnumBuilder, error)
	Enum(name string) (BamlEnumBuilder, error)
}

// BamlClassBuilder for building classes imperatively.
type BamlClassBuilder interface {
	SetDescription(description string) error
	SetAlias(alias string) error
	AddProperty(name string, fieldType BamlType) (BamlPropertyBuilder, error)
	Type() (BamlType, error)
}

// BamlPropertyBuilder for building class properties.
type BamlPropertyBuilder interface {
	SetDescription(description string) error
	SetAlias(alias string) error
}

// BamlEnumBuilder for building enums imperatively.
type BamlEnumBuilder interface {
	SetDescription(description string) error
	SetAlias(alias string) error
	AddValue(name string) (BamlEnumValueBuilder, error)
	Type() (BamlType, error)
}

// BamlEnumValueBuilder for building enum values.
type BamlEnumValueBuilder interface {
	SetDescription(description string) error
	SetAlias(alias string) error
	SetSkip(skip bool) error
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
