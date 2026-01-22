package introspected

// Typed is a minimal interface for types that can return a Type
type Typed interface {
	Type() (Type, error)
}

// DynamicClassBuilder is the interface for dynamic classes (minimal: what generated types implement)
type DynamicClassBuilder interface {
	Type() (Type, error)
	AddProperty(name string, fieldType Type) (ClassPropertyBuilder, error)
}

// DynamicEnumBuilder is the interface for dynamic enums (minimal: what generated types implement)
type DynamicEnumBuilder interface {
	Type() (Type, error)
	AddValue(value string) (EnumValueBuilder, error)
}

// DynamicClassAccessor is a function that returns a DynamicClassBuilder for a dynamic class
type DynamicClassAccessor func(*TypeBuilder) (DynamicClassBuilder, error)

// DynamicEnumAccessor is a function that returns a DynamicEnumBuilder for a dynamic enum
type DynamicEnumAccessor func(*TypeBuilder) (DynamicEnumBuilder, error)

// StaticClassAccessor is a function that returns a Typed for a static class
type StaticClassAccessor func(*TypeBuilder) (Typed, error)

// StaticEnumAccessor is a function that returns a Typed for a static enum
type StaticEnumAccessor func(*TypeBuilder) (Typed, error)
