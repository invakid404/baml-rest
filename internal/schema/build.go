package schema

import (
	"encoding/json"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// dynamicOutputClassName is the synthetic top-level class baml-rest
// assigns to a dynamic output. It MUST stay in lockstep with the
// unexported bamlutils.dynamicOutputClassName that buildWorkerClassMap
// injects (bamlutils/dynamic.go), so a bundle and the worker payload
// agree on where the synthetic class lives.
const dynamicOutputClassName = "Baml_Rest_DynamicOutput"

// RefKind classifies a name a static-ref resolver recognises, so
// construction can emit the correct reference node ([TypeClass] vs
// [TypeEnum]) for a name not defined inside the dynamic bundle.
type RefKind string

const (
	RefClass RefKind = "class"
	RefEnum  RefKind = "enum"
)

// RefResolver classifies a reference name that is not defined inside the
// dynamic bundle. It returns the kind and true when it recognises the
// name. A resolver lets construction succeed for references to static
// baml_src types; the resulting bundle is only self-contained (passes
// [Bundle.Validate]) if those definitions are also present, so a resolver
// is the seam for future static-definition injection, not a way to skip
// self-containment.
type RefResolver func(name string) (RefKind, bool)

// BuildOptions tunes [FromDynamicOutputSchema]. The zero value builds a
// strictly self-contained bundle: every reference must resolve to a class
// or enum defined in the same dynamic schema, else construction fails.
type BuildOptions struct {
	// Resolver, when set, classifies reference names not defined inside
	// the dynamic schema. See [RefResolver].
	Resolver RefResolver
}

// FromDynamicOutputSchema lowers the current baml-rest dynamic output
// schema (the DynamicOutputSchema / DynamicTypes input surface) into a
// self-contained [Bundle], mirroring how the worker payload is built:
//
//   - a synthetic Baml_Rest_DynamicOutput class is injected first, with
//     the top-level properties as its fields (matching buildWorkerClassMap);
//   - user classes and enums follow in their declared order;
//   - the optional type lowers to a nullable union (BAML TypeIR has no
//     optional kind);
//   - every reference resolves to a class/enum in the bundle, or via an
//     explicit resolver, else construction returns an error.
//
// All produced class/enum definitions and references are marked dynamic,
// since they originate from the dynamic TypeBuilder surface. The returned
// bundle has its indexes built; the caller runs [Bundle.Validate] or
// [Bundle.ValidateOutput] to enforce the remaining invariants.
//
// Streaming behavior, constraints, and the per-value enum skip flag have
// no representation in the current dynamic input, so they are absent
// (non-streaming, no constraints) in the produced bundle. Skipped enum
// values are carried as ordinary values: skip is a rendering concern (P3)
// the model does not yet express.
func FromDynamicOutputSchema(s *bamlutils.DynamicOutputSchema, opts BuildOptions) (*Bundle, error) {
	if s == nil {
		return nil, fmt.Errorf("schema: nil DynamicOutputSchema")
	}

	b := &builder{
		resolver:   opts.Resolver,
		classNames: make(map[string]struct{}),
		enumNames:  make(map[string]struct{}),
	}

	// Classification sets first, so references resolve regardless of
	// declaration order. The synthetic class participates so a top-level
	// property could in principle reference it (recursion).
	b.classNames[dynamicOutputClassName] = struct{}{}
	for _, name := range s.Classes.Keys() {
		if name == dynamicOutputClassName {
			return nil, fmt.Errorf("schema: class %q is reserved by baml-rest", dynamicOutputClassName)
		}
		b.classNames[name] = struct{}{}
	}
	for _, name := range s.Enums.Keys() {
		if _, dup := b.classNames[name]; dup {
			return nil, fmt.Errorf("schema: %q is declared as both a class and an enum", name)
		}
		b.enumNames[name] = struct{}{}
	}

	bundle := &Bundle{
		Target: Type{Kind: TypeClass, Name: dynamicOutputClassName, Mode: NonStreaming, Dynamic: true},
	}

	// Enums, declared order.
	for _, e := range s.Enums.Entries() {
		bundle.Enums = append(bundle.Enums, buildEnum(e.Key, e.Value))
	}

	// Synthetic top-level class first, then user classes in declared order.
	syntheticFields, err := b.buildFields(dynamicOutputClassName, s.Properties)
	if err != nil {
		return nil, err
	}
	bundle.Classes = append(bundle.Classes, ClassDef{
		Name:   Name{Name: dynamicOutputClassName},
		Mode:   NonStreaming,
		Fields: syntheticFields,
	})
	for _, c := range s.Classes.Entries() {
		cls := c.Value
		if cls == nil {
			return nil, fmt.Errorf("schema: class %q: nil definition", c.Key)
		}
		fields, err := b.buildFields(c.Key, cls.Properties)
		if err != nil {
			return nil, err
		}
		bundle.Classes = append(bundle.Classes, ClassDef{
			Name:        Name{Name: c.Key, Alias: strPtr(cls.Alias)},
			Description: strPtr(cls.Description),
			Mode:        NonStreaming,
			Fields:      fields,
		})
	}

	if err := bundle.RebuildIndexes(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// builder holds the per-construction reference-classification state.
type builder struct {
	resolver   RefResolver
	classNames map[string]struct{}
	enumNames  map[string]struct{}
}

func buildEnum(name string, e *bamlutils.DynamicEnum) EnumDef {
	def := EnumDef{Name: Name{Name: name}}
	if e == nil {
		return def
	}
	def.Name.Alias = strPtr(e.Alias)
	for _, v := range e.Values {
		if v == nil {
			continue
		}
		def.Values = append(def.Values, EnumValue{
			Name:        Name{Name: v.Name, Alias: strPtr(v.Alias)},
			Description: strPtr(v.Description),
		})
	}
	return def
}

func (b *builder) buildFields(className string, props bamlutils.OrderedMap[*bamlutils.DynamicProperty]) ([]ClassField, error) {
	var fields []ClassField
	var err error
	props.Range(func(propName string, prop *bamlutils.DynamicProperty) bool {
		if prop == nil {
			err = fmt.Errorf("schema: class %q: property %q is nil", className, propName)
			return false
		}
		path := fmt.Sprintf("class %q property %q", className, propName)
		var ty Type
		ty, err = b.convertProperty(prop, path)
		if err != nil {
			return false
		}
		fields = append(fields, ClassField{
			Name:        Name{Name: propName, Alias: strPtr(prop.Alias)},
			Type:        ty,
			Description: strPtr(prop.Description),
		})
		return true
	})
	return fields, err
}

// convertProperty lowers a class property (which carries its own
// description/alias on the field, not the type) into a [Type].
func (b *builder) convertProperty(p *bamlutils.DynamicProperty, path string) (Type, error) {
	return b.convertType(p.Type, p.Ref, p.Items, p.Inner, p.OneOf, p.Keys, p.Values, p.Value, path)
}

func (b *builder) convertSpec(s *bamlutils.DynamicTypeSpec, path string) (Type, error) {
	if s == nil {
		return Type{}, fmt.Errorf("schema: %s: nil type spec", path)
	}
	return b.convertType(s.Type, s.Ref, s.Items, s.Inner, s.OneOf, s.Keys, s.Values, s.Value, path)
}

// convertType is the single lowering point shared by properties and
// nested specs, mirroring the dispatch of bamlutils.validateTypeRef.
func (b *builder) convertType(
	typ, ref string,
	items, inner *bamlutils.DynamicTypeSpec,
	oneOf []*bamlutils.DynamicTypeSpec,
	keys, values *bamlutils.DynamicTypeSpec,
	value any,
	path string,
) (Type, error) {
	if ref != "" {
		if typ != "" {
			return Type{}, fmt.Errorf("schema: %s: cannot have both 'type' and 'ref'", path)
		}
		return b.resolveRef(ref, path)
	}
	if typ == "" {
		return Type{}, fmt.Errorf("schema: %s: must have 'type' or 'ref'", path)
	}

	switch typ {
	case "string":
		return Type{Kind: TypePrimitive, Primitive: PrimitiveString}, nil
	case "int":
		return Type{Kind: TypePrimitive, Primitive: PrimitiveInt}, nil
	case "float":
		return Type{Kind: TypePrimitive, Primitive: PrimitiveFloat}, nil
	case "bool":
		return Type{Kind: TypePrimitive, Primitive: PrimitiveBool}, nil
	case "null":
		return Type{Kind: TypePrimitive, Primitive: PrimitiveNull}, nil
	case "list":
		if items == nil {
			return Type{}, fmt.Errorf("schema: %s: 'list' requires 'items'", path)
		}
		elem, err := b.convertSpec(items, path+"[items]")
		if err != nil {
			return Type{}, err
		}
		return Type{Kind: TypeList, Elem: &elem}, nil
	case "optional":
		if inner == nil {
			return Type{}, fmt.Errorf("schema: %s: 'optional' requires 'inner'", path)
		}
		innerTy, err := b.convertSpec(inner, path+"[inner]")
		if err != nil {
			return Type{}, err
		}
		return makeNullable(innerTy), nil
	case "map":
		if keys == nil || values == nil {
			return Type{}, fmt.Errorf("schema: %s: 'map' requires 'keys' and 'values'", path)
		}
		key, err := b.convertSpec(keys, path+"[keys]")
		if err != nil {
			return Type{}, err
		}
		val, err := b.convertSpec(values, path+"[values]")
		if err != nil {
			return Type{}, err
		}
		return Type{Kind: TypeMap, Key: &key, Value: &val}, nil
	case "union":
		if len(oneOf) == 0 {
			return Type{}, fmt.Errorf("schema: %s: 'union' requires 'oneOf' with at least one type", path)
		}
		return b.buildUnion(oneOf, path)
	case "literal_string":
		str, ok := value.(string)
		if !ok {
			return Type{}, fmt.Errorf("schema: %s: 'literal_string' requires a string 'value'", path)
		}
		return Type{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralString, String: str}}, nil
	case "literal_int":
		n, err := toInt64(value)
		if err != nil {
			return Type{}, fmt.Errorf("schema: %s: 'literal_int' requires an integer 'value': %w", path, err)
		}
		return Type{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralInt, Int: n}}, nil
	case "literal_bool":
		bv, ok := value.(bool)
		if !ok {
			return Type{}, fmt.Errorf("schema: %s: 'literal_bool' requires a boolean 'value'", path)
		}
		return Type{Kind: TypeLiteral, Literal: &LiteralValue{Kind: LiteralBool, Bool: bv}}, nil
	default:
		return Type{}, fmt.Errorf("schema: %s: unknown type %q", path, typ)
	}
}

// buildUnion lowers a union, normalising any explicit null variant into
// the nullable marker so [UnionType.Variants] never contains a null
// primitive — matching BAML's union construction and iter_include_null
// ordering (non-null variants first, canonical null appended last).
func (b *builder) buildUnion(oneOf []*bamlutils.DynamicTypeSpec, path string) (Type, error) {
	var variants []Type
	nullable := false
	for i, spec := range oneOf {
		v, err := b.convertSpec(spec, fmt.Sprintf("%s[oneOf.%d]", path, i))
		if err != nil {
			return Type{}, err
		}
		if v.Kind == TypePrimitive && v.Primitive == PrimitiveNull {
			nullable = true
			continue
		}
		variants = append(variants, v)
	}
	return Type{Kind: TypeUnion, Union: &UnionType{Variants: variants, Nullable: nullable}}, nil
}

// makeNullable wraps inner in a nullable union, the TypeIR representation
// of optional<inner>. When inner is already a union, the existing
// variants are preserved and only the nullable marker is set, avoiding a
// redundant nested union.
func makeNullable(inner Type) Type {
	if inner.Kind == TypeUnion && inner.Union != nil {
		u := *inner.Union
		u.Nullable = true
		inner.Union = &u
		return inner
	}
	return Type{Kind: TypeUnion, Union: &UnionType{Variants: []Type{inner}, Nullable: true}}
}

// resolveRef turns a reference name into a class or enum reference node.
// Names defined in the dynamic bundle classify themselves; otherwise the
// optional resolver is consulted. A name that is neither is an
// unresolved-reference error, keeping the bundle self-contained.
func (b *builder) resolveRef(name, path string) (Type, error) {
	_, isClass := b.classNames[name]
	_, isEnum := b.enumNames[name]
	switch {
	case isClass && isEnum:
		return Type{}, fmt.Errorf("schema: %s: ambiguous reference %q (both a class and an enum)", path, name)
	case isClass:
		return Type{Kind: TypeClass, Name: name, Mode: NonStreaming, Dynamic: true}, nil
	case isEnum:
		return Type{Kind: TypeEnum, Name: name, Dynamic: true}, nil
	}
	if b.resolver != nil {
		if kind, ok := b.resolver(name); ok {
			switch kind {
			case RefClass:
				return Type{Kind: TypeClass, Name: name, Mode: NonStreaming}, nil
			case RefEnum:
				return Type{Kind: TypeEnum, Name: name}, nil
			default:
				return Type{}, fmt.Errorf("schema: %s: resolver returned unknown kind %q for %q", path, kind, name)
			}
		}
	}
	return Type{}, fmt.Errorf("schema: %s: unresolved reference %q", path, name)
}

// toInt64 coerces a JSON-decoded literal_int value to int64. JSON numbers
// decode to float64 through the standard decoders; json.Number and the
// integer types are accepted too. A non-integral float is rejected.
func toInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if v != float64(int64(v)) {
			return 0, fmt.Errorf("value %v is not an integer", v)
		}
		return int64(v), nil
	case json.Number:
		return v.Int64()
	case nil:
		return 0, fmt.Errorf("value is missing")
	default:
		return 0, fmt.Errorf("value has non-integer type %T", v)
	}
}
