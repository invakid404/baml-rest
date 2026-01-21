package adapter

import (
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/invakid404/baml-rest/bamlutils"
)

// typeBuilderWrapper wraps a native BAML TypeBuilder to implement bamlutils.BamlTypeBuilder.
// The inner field is stored as any because the generated baml_client.TypeBuilder type
// is different from baml.TypeBuilder (the interface from the pkg).
type typeBuilderWrapper struct {
	inner    any             // The native TypeBuilder (*baml_client.TypeBuilder)
	bamlInner baml.TypeBuilder // The underlying baml.TypeBuilder for our operations
}

// WrapTypeBuilder creates a wrapper around a native BAML TypeBuilder.
// It accepts any because the generated baml_client.TypeBuilder is not assignable to baml.TypeBuilder.
// The native parameter is the *baml_client.TypeBuilder returned by NewTypeBuilder().
// The bamlTb parameter is the underlying baml.TypeBuilder for internal operations.
func WrapTypeBuilder(native any, bamlTb baml.TypeBuilder) bamlutils.BamlTypeBuilder {
	return &typeBuilderWrapper{inner: native, bamlInner: bamlTb}
}

// Native returns the underlying native BAML TypeBuilder (*baml_client.TypeBuilder).
// This is used by generated code to extract the native type for BAML calls.
func (w *typeBuilderWrapper) Native() any {
	return w.inner
}

func (w *typeBuilderWrapper) AddBaml(baml string) error {
	return w.bamlInner.AddBaml(baml)
}

func (w *typeBuilderWrapper) String() (bamlutils.BamlType, error) {
	return w.bamlInner.String()
}

func (w *typeBuilderWrapper) Int() (bamlutils.BamlType, error) {
	return w.bamlInner.Int()
}

func (w *typeBuilderWrapper) Float() (bamlutils.BamlType, error) {
	return w.bamlInner.Float()
}

func (w *typeBuilderWrapper) Bool() (bamlutils.BamlType, error) {
	return w.bamlInner.Bool()
}

func (w *typeBuilderWrapper) Null() (bamlutils.BamlType, error) {
	return w.bamlInner.Null()
}

func (w *typeBuilderWrapper) LiteralString(value string) (bamlutils.BamlType, error) {
	return w.bamlInner.LiteralString(value)
}

func (w *typeBuilderWrapper) LiteralInt(value int64) (bamlutils.BamlType, error) {
	return w.bamlInner.LiteralInt(value)
}

func (w *typeBuilderWrapper) LiteralBool(value bool) (bamlutils.BamlType, error) {
	return w.bamlInner.LiteralBool(value)
}

func (w *typeBuilderWrapper) List(inner bamlutils.BamlType) (bamlutils.BamlType, error) {
	t, ok := inner.(baml.Type)
	if !ok {
		// If it's not a baml.Type directly, it might be wrapped
		return nil, errInvalidType("List inner", inner)
	}
	return w.bamlInner.List(t)
}

func (w *typeBuilderWrapper) Optional(inner bamlutils.BamlType) (bamlutils.BamlType, error) {
	t, ok := inner.(baml.Type)
	if !ok {
		return nil, errInvalidType("Optional inner", inner)
	}
	return w.bamlInner.Optional(t)
}

func (w *typeBuilderWrapper) Union(types []bamlutils.BamlType) (bamlutils.BamlType, error) {
	bamlTypes := make([]baml.Type, len(types))
	for i, t := range types {
		bt, ok := t.(baml.Type)
		if !ok {
			return nil, errInvalidType("Union element", t)
		}
		bamlTypes[i] = bt
	}
	return w.bamlInner.Union(bamlTypes)
}

func (w *typeBuilderWrapper) Map(key, value bamlutils.BamlType) (bamlutils.BamlType, error) {
	k, ok := key.(baml.Type)
	if !ok {
		return nil, errInvalidType("Map key", key)
	}
	v, ok := value.(baml.Type)
	if !ok {
		return nil, errInvalidType("Map value", value)
	}
	return w.bamlInner.Map(k, v)
}

func (w *typeBuilderWrapper) AddClass(name string) (bamlutils.BamlClassBuilder, error) {
	cb, err := w.bamlInner.AddClass(name)
	if err != nil {
		return nil, err
	}
	return &classBuilderWrapper{inner: cb}, nil
}

func (w *typeBuilderWrapper) Class(name string) (bamlutils.BamlClassBuilder, error) {
	cb, err := w.bamlInner.Class(name)
	if err != nil {
		return nil, err
	}
	return &classBuilderWrapper{inner: cb}, nil
}

func (w *typeBuilderWrapper) AddEnum(name string) (bamlutils.BamlEnumBuilder, error) {
	eb, err := w.bamlInner.AddEnum(name)
	if err != nil {
		return nil, err
	}
	return &enumBuilderWrapper{inner: eb}, nil
}

func (w *typeBuilderWrapper) Enum(name string) (bamlutils.BamlEnumBuilder, error) {
	eb, err := w.bamlInner.Enum(name)
	if err != nil {
		return nil, err
	}
	return &enumBuilderWrapper{inner: eb}, nil
}

// classBuilderWrapper wraps a native BAML ClassBuilder.
type classBuilderWrapper struct {
	inner baml.ClassBuilder
}

func (w *classBuilderWrapper) AddProperty(name string, fieldType bamlutils.BamlType) (bamlutils.BamlPropertyBuilder, error) {
	t, ok := fieldType.(baml.Type)
	if !ok {
		return nil, errInvalidType("property type", fieldType)
	}
	pb, err := w.inner.AddProperty(name, t)
	if err != nil {
		return nil, err
	}
	return &propertyBuilderWrapper{inner: pb}, nil
}

func (w *classBuilderWrapper) Type() (bamlutils.BamlType, error) {
	return w.inner.Type()
}

// propertyBuilderWrapper wraps a native BAML ClassPropertyBuilder.
// Note: The native BAML library doesn't expose SetDescription/SetAlias on properties.
type propertyBuilderWrapper struct {
	inner baml.ClassPropertyBuilder
}

// enumBuilderWrapper wraps a native BAML EnumBuilder.
type enumBuilderWrapper struct {
	inner baml.EnumBuilder
}

func (w *enumBuilderWrapper) AddValue(name string) (bamlutils.BamlEnumValueBuilder, error) {
	vb, err := w.inner.AddValue(name)
	if err != nil {
		return nil, err
	}
	return &enumValueBuilderWrapper{inner: vb}, nil
}

func (w *enumBuilderWrapper) Type() (bamlutils.BamlType, error) {
	return w.inner.Type()
}

// enumValueBuilderWrapper wraps a native BAML EnumValueBuilder.
type enumValueBuilderWrapper struct {
	inner baml.EnumValueBuilder
}

func (w *enumValueBuilderWrapper) SetSkip(skip bool) error {
	return w.inner.SetSkip(skip)
}

// errInvalidType creates an error for invalid type assertions.
func errInvalidType(context string, value any) error {
	return &invalidTypeError{context: context, value: value}
}

type invalidTypeError struct {
	context string
	value   any
}

func (e *invalidTypeError) Error() string {
	return "invalid type for " + e.context + ": expected baml.Type"
}

// Verify interfaces are implemented
var (
	_ bamlutils.BamlTypeBuilder      = (*typeBuilderWrapper)(nil)
	_ bamlutils.BamlClassBuilder     = (*classBuilderWrapper)(nil)
	_ bamlutils.BamlPropertyBuilder  = (*propertyBuilderWrapper)(nil)
	_ bamlutils.BamlEnumBuilder      = (*enumBuilderWrapper)(nil)
	_ bamlutils.BamlEnumValueBuilder = (*enumValueBuilderWrapper)(nil)
)
