package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"

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
//   - a synthetic Baml_Rest_DynamicOutput class is the target, with the
//     top-level properties as its fields (matching buildWorkerClassMap);
//   - Bundle.Enums and Bundle.Classes are ordered by BAML's target-
//     reachability traversal, NOT declared order: definitions are
//     discovered from the target via a LIFO stack and appended on first
//     pop, so the order is reverse-of-reference (DFS), matching BAML's
//     render_output_format relevant_data_models exactly (see
//     [orderByReachability]). The synthetic class is the first class popped,
//     so it stays first in Bundle.Classes;
//   - definitions the target cannot reach are pruned (BAML returns only the
//     relevant data models), so a declared-but-unreferenced dynamic enum or
//     class is absent from the bundle;
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
// Streaming behavior and constraints have no representation in the
// current dynamic input, so they are absent (non-streaming, no
// constraints) in the produced bundle. Skipped enum values are dropped,
// mirroring BAML's OutputFormatContent (see [buildEnum]).
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

	// Lower every definition body into local lookup tables keyed the way
	// BAML's OutputFormatContent indexes them (enum by canonical name,
	// class by (name, mode)). The bundle's ordered slices are NOT filled
	// here: the reachability traversal below decides which definitions are
	// reachable from the target and in what order they appear, matching
	// BAML's relevant_data_models. Lowering is unchanged from the declared-
	// order path — only the storage differs.
	enumDefs := make(map[string]EnumDef, s.Enums.Len())
	for _, e := range s.Enums.Entries() {
		enumDef, err := buildEnum(e.Key, e.Value)
		if err != nil {
			return nil, err
		}
		enumDefs[e.Key] = enumDef
	}

	classDefs := make(map[ClassKey]ClassDef, s.Classes.Len()+1)
	// Synthetic top-level class: its fields are the top-level properties.
	syntheticFields, err := b.buildFields(dynamicOutputClassName, s.Properties)
	if err != nil {
		return nil, err
	}
	classDefs[ClassKey{Name: dynamicOutputClassName, Mode: NonStreaming}] = ClassDef{
		Name:   Name{Name: dynamicOutputClassName},
		Mode:   NonStreaming,
		Fields: syntheticFields,
	}
	for _, c := range s.Classes.Entries() {
		cls := c.Value
		if cls == nil {
			return nil, fmt.Errorf("schema: class %q: nil definition", c.Key)
		}
		fields, err := b.buildFields(c.Key, cls.Properties)
		if err != nil {
			return nil, err
		}
		classDefs[ClassKey{Name: c.Key, Mode: NonStreaming}] = ClassDef{
			Name:        Name{Name: c.Key, Alias: strPtr(cls.Alias)},
			Description: strPtr(cls.Description),
			Mode:        NonStreaming,
			Fields:      fields,
		}
	}

	// Walk from the target with BAML's LIFO reachability traversal, which
	// yields the enum and class definitions in BAML's hoist order
	// (reverse-of-reference DFS) and prunes anything the target cannot
	// reach. Assign the ordered slices, then rebuild the indexes ONCE on
	// the final source-of-truth slices. The dynamic input surface produces no
	// structural recursive aliases, so aliasTargets is nil and the returned
	// alias order is empty.
	bundle.Enums, bundle.Classes, _ = orderByReachability(bundle.Target, enumDefs, classDefs, nil)

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

// buildEnum lowers a dynamic enum into an EnumDef. A nil definition is a
// malformed input (DynamicTypes.Validate rejects it too), so construction
// fails closed rather than inventing an empty enum.
//
// Skipped values are dropped: BAML's jsonish helper and runtime
// output-format builder both return None for @skip values, so they are
// absent from OutputFormatContent — keeping them would render hidden
// categories (P3) and let the parser accept values BAML never resolves
// (P4/P5). OutputFormatContent.Enum has no skip field for the same
// reason; this model stores only final parse/render values.
func buildEnum(name string, e *bamlutils.DynamicEnum) (EnumDef, error) {
	def := EnumDef{Name: Name{Name: name}}
	if e == nil {
		return EnumDef{}, fmt.Errorf("schema: enum %q: nil definition", name)
	}
	def.Name.Alias = strPtr(e.Alias)
	for i, v := range e.Values {
		// Fail closed on a malformed value rather than silently shrinking
		// the enum, matching the nil-property handling in buildFields and
		// bamlutils.DynamicTypes.Validate.
		if v == nil {
			return EnumDef{}, fmt.Errorf("schema: enum %q: value at index %d is nil", name, i)
		}
		if v.Skip {
			continue
		}
		def.Values = append(def.Values, EnumValue{
			Name:        Name{Name: v.Name, Alias: strPtr(v.Alias)},
			Description: strPtr(v.Description),
		})
	}
	return def, nil
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
		return makeOptional(innerTy), nil
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
		choices := make([]Type, 0, len(oneOf))
		for i, spec := range oneOf {
			v, err := b.convertSpec(spec, fmt.Sprintf("%s[oneOf.%d]", path, i))
			if err != nil {
				return Type{}, err
			}
			choices = append(choices, v)
		}
		return makeUnion(choices), nil
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

// nullType is the canonical null primitive, the value BAML represents as
// TypeGeneric::null().
func nullType() Type {
	return Type{Kind: TypePrimitive, Primitive: PrimitiveNull}
}

// isNull reports whether t is the null primitive (BAML TypeGeneric::is_null,
// which matches only Primitive(Null), not optional unions).
func isNull(t Type) bool {
	return t.Kind == TypePrimitive && t.Primitive == PrimitiveNull
}

// makeOptional mirrors BAML's TypeIR::optional: optional<null> is null,
// otherwise it is union([inner, null]) run through the same simplifier.
func makeOptional(inner Type) Type {
	if isNull(inner) {
		return inner
	}
	return makeUnion([]Type{inner, nullType()})
}

// makeUnion mirrors BAML's TypeIR::union constructor: a single choice
// collapses to that choice, otherwise the union is simplified. This is
// the only way unions are built so every union is normalised.
func makeUnion(choices []Type) Type {
	if len(choices) == 1 {
		return choices[0]
	}
	return simplifyUnion(choices)
}

// simplifyUnion reproduces BAML's UnionConstructor + simplify() for the
// dynamic-construction path (engine/baml-lib/baml-types/src/ir_type/
// simplify/ir.rs and union_type.rs):
//
//   - flatten nested unions (an optional nested union contributes a
//     trailing null);
//   - deduplicate variants, preserving first-seen order;
//   - hoist null out of the variant list into the Nullable marker;
//   - collapse an all-null union to the null primitive;
//   - collapse a single non-null variant to that variant, unless the
//     union is also nullable (then it stays an optional-of-one).
//
// The dynamic input carries no constraints or streaming metadata, so the
// metadata-distribution and subtype-absorption steps BAML performs for
// constraint-wrapped unions cannot trigger here and are intentionally
// omitted; flattening already fully unwraps every (constraint-free)
// nested union, leaving no union-typed variant for absorption to act on.
func simplifyUnion(choices []Type) Type {
	flattened := make([]Type, 0, len(choices))
	for _, c := range choices {
		flattened = append(flattened, flattenForUnion(c)...)
	}

	hasNull := false
	variants := make([]Type, 0, len(flattened))
	seen := make([]Type, 0, len(flattened))
	for _, t := range flattened {
		if isNull(t) {
			hasNull = true
			continue
		}
		if containsType(seen, t) {
			continue
		}
		seen = append(seen, t)
		variants = append(variants, t)
	}

	switch len(variants) {
	case 0:
		return nullType()
	case 1:
		if hasNull {
			return Type{Kind: TypeUnion, Union: &UnionType{Variants: []Type{variants[0]}, Nullable: true}}
		}
		return variants[0]
	default:
		return Type{Kind: TypeUnion, Union: &UnionType{Variants: variants, Nullable: hasNull}}
	}
}

// flattenForUnion mirrors BAML's TypeGeneric::flatten + the union view's
// flatten: a constraint-free union is replaced by its non-null variants
// (recursively flattened) with a trailing null when it is nullable; every
// other type, and any constraint-carrying union (preserved as a unit),
// flattens to itself.
func flattenForUnion(t Type) []Type {
	if t.Kind == TypeUnion && t.Union != nil && len(t.Meta.Constraints) == 0 {
		out := make([]Type, 0, len(t.Union.Variants)+1)
		for _, v := range t.Union.Variants {
			out = append(out, flattenForUnion(v)...)
		}
		if t.Union.Nullable {
			out = append(out, nullType())
		}
		return out
	}
	return []Type{t}
}

// containsType reports whether ts already holds a type structurally equal
// to t. BAML's simplify() deduplicates with full structural equality
// (TypeGeneric derives Eq), which for the metadata-free dynamic path
// reduces to a deep comparison of the type trees.
func containsType(ts []Type, t Type) bool {
	for i := range ts {
		if reflect.DeepEqual(ts[i], t) {
			return true
		}
	}
	return false
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
// integer types are accepted too. A float is rejected when it is
// non-integral, out of int64 range, or at or beyond ±2^53 (where float64
// can no longer represent every integer, so the decoded value may already
// be a rounded approximation of the caller's literal). The integrality,
// range, and exactness checks all run BEFORE the int64 cast, because converting
// an out-of-range float64 to int64 is undefined in Go (float64(2^63)
// would otherwise round-trip through MaxInt64 and pass a naive
// v == float64(int64(v)) test).
func toInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("value %v is not an integer", v)
		}
		// math.MinInt64 is exactly representable as float64; math.MaxInt64
		// is not (it rounds up to 2^63), so the upper bound is exclusive at
		// 2^63 to exclude everything that would overflow int64.
		const minInt64f = -9223372036854775808.0 // math.MinInt64
		const twoPow63 = 9223372036854775808.0   // math.MaxInt64 + 1
		if v < minInt64f || v >= twoPow63 {
			return 0, fmt.Errorf("value %v is out of int64 range", v)
		}
		// float64 represents every integer exactly only BELOW 2^53. At and
		// above 2^53 it skips integers, so the upstream JSON decode (which
		// already produced this float64) may have silently rounded the
		// caller's literal — e.g. 2^53+1 rounds to exactly float64(2^53).
		// The boundary is therefore rejected too: a value that arrives as
		// float64(2^53) is indistinguishable from a rounded 2^53+1, and the
		// original digits are unrecoverable, so we fail closed rather than
		// store a value that may differ from the input.
		const maxExactInt = 9007199254740992.0 // 2^53
		if v >= maxExactInt || v <= -maxExactInt {
			return 0, fmt.Errorf("value %v cannot be represented exactly (magnitude at or beyond 2^53; it may have been rounded during JSON decoding)", v)
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
