package introspected

// NOTE: this file will be overwritten during build

// Stream is the BAML streaming client instance
var Stream = &struct{}{}

// StreamMethods is a map from method name to argument names
var StreamMethods = map[string][]string{}

// SyncMethods maps sync function names to their argument names
var SyncMethods = map[string][]string{}

// SyncFuncs maps sync function names to their function values (for reflection)
var SyncFuncs = map[string]any{}

// Parse is the parse API for parsing raw LLM responses into final types
var Parse = &struct{}{}

// ParseMethods is a set of method names available on Parse
var ParseMethods = map[string]struct{}{}

// ParseStream is the parse_stream API for parsing raw LLM responses into partial/stream types
var ParseStream = &struct{}{}

// ParseStreamMethods is a set of method names available on ParseStream
var ParseStreamMethods = map[string]struct{}{}

// ParseStreamFuncs maps ParseStream method names to their function values (for reflection)
var ParseStreamFuncs = map[string]any{}

// TypeBuilder stubs - these are replaced during build when type_builder exists
type TypeBuilder struct{}
type Type interface{}
type ClassPropertyBuilder interface{}
type EnumValueBuilder interface {
	SetDescription(description string) error
	SetAlias(alias string) error
	SetSkip(skip bool) error
}

// NewTypeBuilder stub
var NewTypeBuilder = func() (*TypeBuilder, error) { return nil, nil }

// DynamicClasses maps dynamic class names to their accessor functions
var DynamicClasses = map[string]DynamicClassAccessor{}

// StaticClasses maps static class names to their accessor functions
var StaticClasses = map[string]StaticClassAccessor{}

// DynamicEnums maps dynamic enum names to their accessor functions
var DynamicEnums = map[string]DynamicEnumAccessor{}

// StaticEnums maps static enum names to their accessor functions
var StaticEnums = map[string]StaticEnumAccessor{}

// AllClasses returns all known class names (both dynamic and static)
func AllClasses() []string { return nil }

// AllEnums returns all known enum names (both dynamic and static)
func AllEnums() []string { return nil }

// IsDynamicClass returns true if the class exists and is dynamic
func IsDynamicClass(name string) bool { return false }

// IsDynamicEnum returns true if the enum exists and is dynamic
func IsDynamicEnum(name string) bool { return false }

// ClassExists returns true if a class with this name exists (dynamic or static)
func ClassExists(name string) bool { return false }

// EnumExists returns true if an enum with this name exists (dynamic or static)
func EnumExists(name string) bool { return false }

// GetClassType returns the Type for a class by name
func GetClassType(tb *TypeBuilder, name string) (Type, error) { return nil, nil }

// GetEnumType returns the Type for an enum by name
func GetEnumType(tb *TypeBuilder, name string) (Type, error) { return nil, nil }
