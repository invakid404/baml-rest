package schema

import "fmt"

// RebuildIndexes deterministically reconstructs every lookup index from
// the ordered slices (the source of truth), and enforces the uniqueness
// invariants the indexes assume:
//
//   - enum canonical names are unique;
//   - class (canonical name, mode) keys are unique;
//   - structural recursive alias names are unique;
//   - rendered field names are unique within each class;
//   - rendered enum value names are unique within each enum.
//
// It is idempotent: calling it twice yields identical indexes. A Bundle
// produced by a constructor in this package already has indexes built;
// call this after decoding a Bundle from JSON (which cannot populate the
// unexported maps) or after mutating any slice.
//
// On the first violation it returns an error and leaves the indexes in a
// partially built but self-consistent-per-definition state; callers
// should treat a non-nil return as "do not use the indexes".
func (b *Bundle) RebuildIndexes() error {
	b.enumByName = make(map[string]int, len(b.Enums))
	for i := range b.Enums {
		e := &b.Enums[i]
		if _, dup := b.enumByName[e.Name.Name]; dup {
			return fmt.Errorf("schema: duplicate enum name %q", e.Name.Name)
		}
		b.enumByName[e.Name.Name] = i
		if err := e.rebuildIndexes(); err != nil {
			return err
		}
	}

	b.classByKey = make(map[ClassKey]int, len(b.Classes))
	for i := range b.Classes {
		c := &b.Classes[i]
		key := ClassKey{Name: c.Name.Name, Mode: c.Mode}
		if _, dup := b.classByKey[key]; dup {
			return fmt.Errorf("schema: duplicate class %q (mode %q)", key.Name, key.Mode)
		}
		b.classByKey[key] = i
		if err := c.rebuildIndexes(); err != nil {
			return err
		}
	}

	b.aliasByName = make(map[string]int, len(b.StructuralRecursiveAliases))
	for i := range b.StructuralRecursiveAliases {
		a := &b.StructuralRecursiveAliases[i]
		if _, dup := b.aliasByName[a.Name]; dup {
			return fmt.Errorf("schema: duplicate structural recursive alias %q", a.Name)
		}
		b.aliasByName[a.Name] = i
	}

	return nil
}

func (e *EnumDef) rebuildIndexes() error {
	e.valueByName = make(map[string]int, len(e.Values))
	e.valueByRenderedName = make(map[string]int, len(e.Values))
	for i := range e.Values {
		v := &e.Values[i]
		if _, dup := e.valueByName[v.Name.Name]; dup {
			return fmt.Errorf("schema: enum %q: duplicate value name %q", e.Name.Name, v.Name.Name)
		}
		e.valueByName[v.Name.Name] = i
		rendered := v.Name.RenderedName()
		if _, dup := e.valueByRenderedName[rendered]; dup {
			return fmt.Errorf("schema: enum %q: duplicate rendered value name %q", e.Name.Name, rendered)
		}
		e.valueByRenderedName[rendered] = i
	}
	return nil
}

func (c *ClassDef) rebuildIndexes() error {
	c.fieldByName = make(map[string]int, len(c.Fields))
	c.fieldByRenderedName = make(map[string]int, len(c.Fields))
	for i := range c.Fields {
		f := &c.Fields[i]
		if _, dup := c.fieldByName[f.Name.Name]; dup {
			return fmt.Errorf("schema: class %q: duplicate field name %q", c.Name.Name, f.Name.Name)
		}
		c.fieldByName[f.Name.Name] = i
		rendered := f.Name.RenderedName()
		if _, dup := c.fieldByRenderedName[rendered]; dup {
			return fmt.Errorf("schema: class %q: duplicate rendered field name %q", c.Name.Name, rendered)
		}
		c.fieldByRenderedName[rendered] = i
	}
	return nil
}

// FindEnum returns the enum with the given canonical name, mirroring
// BAML's OutputFormatContent::find_enum (a plain map lookup by real
// name). The returned pointer aliases the slice element. Requires
// indexes to be built.
func (b *Bundle) FindEnum(name string) (*EnumDef, bool) {
	i, ok := b.enumByName[name]
	if !ok {
		return nil, false
	}
	return &b.Enums[i], true
}

// FindClass returns the class for the given (canonical name, mode) key,
// mirroring BAML's OutputFormatContent::find_class. The returned pointer
// aliases the slice element. Requires indexes to be built.
func (b *Bundle) FindClass(name string, mode StreamingMode) (*ClassDef, bool) {
	i, ok := b.classByKey[ClassKey{Name: name, Mode: mode}]
	if !ok {
		return nil, false
	}
	return &b.Classes[i], true
}

// FindRecursiveAlias returns the structural recursive alias with the
// given canonical name, mirroring BAML's find_recursive_alias_target. The
// returned pointer aliases the slice element. Requires indexes to be
// built.
func (b *Bundle) FindRecursiveAlias(name string) (*RecursiveAliasDef, bool) {
	i, ok := b.aliasByName[name]
	if !ok {
		return nil, false
	}
	return &b.StructuralRecursiveAliases[i], true
}

// Field returns the field with the given canonical name. Requires the
// owning bundle's indexes to be built.
func (c *ClassDef) Field(name string) (*ClassField, bool) {
	i, ok := c.fieldByName[name]
	if !ok {
		return nil, false
	}
	return &c.Fields[i], true
}

// FieldByRenderedName returns the field whose rendered (alias-or-name)
// matches, the lookup jsonish class coercion uses to match model output.
// Requires the owning bundle's indexes to be built.
func (c *ClassDef) FieldByRenderedName(rendered string) (*ClassField, bool) {
	i, ok := c.fieldByRenderedName[rendered]
	if !ok {
		return nil, false
	}
	return &c.Fields[i], true
}

// Value returns the enum value with the given canonical name. Requires
// the owning bundle's indexes to be built.
func (e *EnumDef) Value(name string) (*EnumValue, bool) {
	i, ok := e.valueByName[name]
	if !ok {
		return nil, false
	}
	return &e.Values[i], true
}

// ValueByRenderedName returns the enum value whose rendered
// (alias-or-name) matches, the lookup jsonish enum coercion builds
// candidates from. Requires the owning bundle's indexes to be built.
func (e *EnumDef) ValueByRenderedName(rendered string) (*EnumValue, bool) {
	i, ok := e.valueByRenderedName[rendered]
	if !ok {
		return nil, false
	}
	return &e.Values[i], true
}
