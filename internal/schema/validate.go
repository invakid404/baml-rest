package schema

import "fmt"

// Validate enforces the compact-bundle invariants that jsonish
// parse-resolution and renderer lookups depend on. It is structural: it
// does not reject the output-only-illegal kinds (tuple/arrow/top/media) —
// use [Bundle.ValidateOutput] for a bundle destined for rendering or
// parse-resolution.
//
// Validate rebuilds the indexes first (which enforces the uniqueness
// invariants: duplicate enum names, duplicate (name, mode) classes,
// duplicate rendered field/value names), then walks every type reachable
// from the target, class fields, and alias targets to enforce:
//
//   - every class/enum/recursive-alias reference resolves to a definition
//     in this self-contained bundle;
//   - unions carry null only via [UnionType.Nullable], never as a null
//     primitive variant (BAML's iter_include_null ordering);
//   - map keys are jsonish-compatible (string, enum, string literal, or a
//     non-nullable union of those);
//   - every recursive-class name resolves to a class.
//
// The traversal follows only local type nesting and treats named
// references as leaves, so it terminates on recursive schemas.
func (b *Bundle) Validate() error {
	if err := b.RebuildIndexes(); err != nil {
		return err
	}

	if err := b.eachRootType(func(t *Type, path string) error {
		return b.validateType(t, path)
	}); err != nil {
		return err
	}

	for i, name := range b.RecursiveClasses {
		if !b.classExists(name) {
			return fmt.Errorf("schema: recursive_classes[%d]: unresolved class %q", i, name)
		}
	}

	return nil
}

// ValidateOutput runs [Bundle.Validate] and additionally rejects the
// TypeIR variants BAML's renderer and jsonish parser error or panic on:
// tuple, arrow, top, and media primitives. Use this for a bundle that
// will drive output-format rendering or parse-resolution.
func (b *Bundle) ValidateOutput() error {
	if err := b.Validate(); err != nil {
		return err
	}
	return b.eachRootType(func(t *Type, path string) error {
		return walkType(t, path, func(n *Type, p string) error {
			switch n.Kind {
			case TypeTuple:
				return fmt.Errorf("schema: %s: tuple is not usable as an output type", p)
			case TypeArrow:
				return fmt.Errorf("schema: %s: arrow is not usable as an output type", p)
			case TypeTop:
				return fmt.Errorf("schema: %s: top is not usable as an output type", p)
			case TypePrimitive:
				if n.Primitive == PrimitiveMedia {
					return fmt.Errorf("schema: %s: media is not usable as an output type", p)
				}
			}
			return nil
		})
	})
}

// eachRootType invokes fn on every top-level type the bundle owns: the
// target, every class field type, and every structural-recursive-alias
// target. These are the roots whose trees collectively contain every
// reachable type without re-descending into named definitions.
func (b *Bundle) eachRootType(fn func(t *Type, path string) error) error {
	if err := fn(&b.Target, "target"); err != nil {
		return err
	}
	for ci := range b.Classes {
		c := &b.Classes[ci]
		for fi := range c.Fields {
			f := &c.Fields[fi]
			path := fmt.Sprintf("class %q.%s", c.Name.Name, f.Name.Name)
			if err := fn(&f.Type, path); err != nil {
				return err
			}
		}
	}
	for ai := range b.StructuralRecursiveAliases {
		a := &b.StructuralRecursiveAliases[ai]
		if err := fn(&a.Target, fmt.Sprintf("alias %q", a.Name)); err != nil {
			return err
		}
	}
	return nil
}

// validateType checks structural invariants for the type tree rooted at
// t, resolving every named reference against the bundle's indexes.
func (b *Bundle) validateType(t *Type, path string) error {
	switch t.Kind {
	case TypeTop:
		return nil
	case TypePrimitive:
		switch t.Primitive {
		case PrimitiveString, PrimitiveInt, PrimitiveFloat, PrimitiveBool, PrimitiveNull:
			return nil
		case PrimitiveMedia:
			switch t.Media {
			case MediaImage, MediaAudio, MediaPDF, MediaVideo:
				return nil
			default:
				return fmt.Errorf("schema: %s: media primitive has invalid subtype %q", path, t.Media)
			}
		default:
			return fmt.Errorf("schema: %s: unknown primitive %q", path, t.Primitive)
		}
	case TypeLiteral:
		if t.Literal == nil {
			return fmt.Errorf("schema: %s: literal type missing value", path)
		}
		switch t.Literal.Kind {
		case LiteralString, LiteralInt, LiteralBool:
			return nil
		default:
			return fmt.Errorf("schema: %s: unknown literal kind %q", path, t.Literal.Kind)
		}
	case TypeEnum:
		if _, ok := b.FindEnum(t.Name); !ok {
			return fmt.Errorf("schema: %s: unresolved enum reference %q", path, t.Name)
		}
		return nil
	case TypeClass:
		if err := validateStreamingMode(t.Mode); err != nil {
			return fmt.Errorf("schema: %s: class reference %q: %w", path, t.Name, err)
		}
		if _, ok := b.FindClass(t.Name, t.Mode); !ok {
			return fmt.Errorf("schema: %s: unresolved class reference %q (mode %q)", path, t.Name, t.Mode)
		}
		return nil
	case TypeRecursiveAlias:
		if err := validateStreamingMode(t.Mode); err != nil {
			return fmt.Errorf("schema: %s: recursive alias reference %q: %w", path, t.Name, err)
		}
		if _, ok := b.FindRecursiveAlias(t.Name); !ok {
			return fmt.Errorf("schema: %s: unresolved recursive alias reference %q", path, t.Name)
		}
		return nil
	case TypeList:
		if t.Elem == nil {
			return fmt.Errorf("schema: %s: list type missing element", path)
		}
		return b.validateType(t.Elem, path+"[]")
	case TypeMap:
		if t.Key == nil || t.Value == nil {
			return fmt.Errorf("schema: %s: map type requires key and value", path)
		}
		if !b.isValidMapKey(t.Key) {
			return fmt.Errorf("schema: %s: map key must be string, enum, string literal, or a non-nullable union of those", path)
		}
		if err := b.validateType(t.Key, path+"[key]"); err != nil {
			return err
		}
		return b.validateType(t.Value, path+"[value]")
	case TypeUnion:
		if t.Union == nil {
			return fmt.Errorf("schema: %s: union type missing payload", path)
		}
		if len(t.Union.Variants) == 0 && !t.Union.Nullable {
			return fmt.Errorf("schema: %s: union has no variants", path)
		}
		for i := range t.Union.Variants {
			v := &t.Union.Variants[i]
			if v.Kind == TypePrimitive && v.Primitive == PrimitiveNull {
				return fmt.Errorf("schema: %s: union variant %d is a null primitive; represent null via Nullable", path, i)
			}
			if err := b.validateType(v, fmt.Sprintf("%s|%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	case TypeTuple:
		for i := range t.Items {
			if err := b.validateType(&t.Items[i], fmt.Sprintf("%s.%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	case TypeArrow:
		if t.Arrow == nil {
			return fmt.Errorf("schema: %s: arrow type missing payload", path)
		}
		for i := range t.Arrow.Params {
			if err := b.validateType(&t.Arrow.Params[i], fmt.Sprintf("%s.param%d", path, i)); err != nil {
				return err
			}
		}
		return b.validateType(&t.Arrow.Return, path+".return")
	default:
		return fmt.Errorf("schema: %s: unknown type kind %q", path, t.Kind)
	}
}

// isValidMapKey reports whether t is a type jsonish accepts as a map key:
// a string primitive, an enum, a string literal, or a non-nullable union
// whose variants are all themselves valid keys.
func (b *Bundle) isValidMapKey(t *Type) bool {
	switch t.Kind {
	case TypePrimitive:
		return t.Primitive == PrimitiveString
	case TypeEnum:
		return true
	case TypeLiteral:
		return t.Literal != nil && t.Literal.Kind == LiteralString
	case TypeUnion:
		if t.Union == nil || t.Union.Nullable || len(t.Union.Variants) == 0 {
			return false
		}
		for i := range t.Union.Variants {
			if !b.isValidMapKey(&t.Union.Variants[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// classExists reports whether a class with the given canonical name
// exists under either streaming mode.
func (b *Bundle) classExists(name string) bool {
	if _, ok := b.FindClass(name, NonStreaming); ok {
		return true
	}
	_, ok := b.FindClass(name, Streaming)
	return ok
}

// walkType invokes fn on t and every type nested within it (local
// nesting only; named references are leaves). It is used for whole-tree
// predicates such as the output-profile rejection scan.
func walkType(t *Type, path string, fn func(n *Type, p string) error) error {
	if err := fn(t, path); err != nil {
		return err
	}
	switch t.Kind {
	case TypeList:
		if t.Elem != nil {
			return walkType(t.Elem, path+"[]", fn)
		}
	case TypeMap:
		if t.Key != nil {
			if err := walkType(t.Key, path+"[key]", fn); err != nil {
				return err
			}
		}
		if t.Value != nil {
			return walkType(t.Value, path+"[value]", fn)
		}
	case TypeUnion:
		if t.Union != nil {
			for i := range t.Union.Variants {
				if err := walkType(&t.Union.Variants[i], fmt.Sprintf("%s|%d", path, i), fn); err != nil {
					return err
				}
			}
		}
	case TypeTuple:
		for i := range t.Items {
			if err := walkType(&t.Items[i], fmt.Sprintf("%s.%d", path, i), fn); err != nil {
				return err
			}
		}
	case TypeArrow:
		if t.Arrow != nil {
			for i := range t.Arrow.Params {
				if err := walkType(&t.Arrow.Params[i], fmt.Sprintf("%s.param%d", path, i), fn); err != nil {
					return err
				}
			}
			return walkType(&t.Arrow.Return, path+".return", fn)
		}
	}
	return nil
}
