package debaml

import (
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// The schema.Bundle -> schemadescriptor.Bundle mirror, shared by every test that has
// to hand a bundle to root [Parse] through the STATIC DESCRIPTOR lane — the only
// request shape that can carry a constraint at all (a DynamicOutputSchema has no
// constraint channel).
//
// It lives in an UNTAGGED file, and deliberately so: the serving oracle
// (build-tagged `integration`) and the Slice 7.2b-3 gate battery (untagged) both
// drive root Parse over the same bundles, and two copies of this conversion would be
// two chances for one of them to drive Parse over a schema nobody else saw.

// ---------------------------------------------------------------------------
// schema.Bundle -> schemadescriptor.Bundle
// ---------------------------------------------------------------------------

// bundleDescriptorFor converts a fixture bundle into the PUBLIC descriptor shape, so
// the same schema can be handed to [Parse] through the static-descriptor lane —
// the only request shape that can carry a constraint at all.
//
// It is a mechanical field-for-field mirror; it is NOT trusted. Every caller
// lowers the result back with schema.FromStaticDescriptor and requires the round
// trip to reproduce the original bundle exactly (soParseRequestFor), so a
// conversion mistake fails loudly instead of quietly driving Parse over a
// different schema.
func bundleDescriptorFor(b *schema.Bundle) schemadescriptor.Bundle {
	out := schemadescriptor.Bundle{
		Version:          schemadescriptor.Version,
		Method:           "SO",
		Target:           bundleDescriptorType(b.Target),
		RecursiveClasses: append([]string(nil), b.RecursiveClasses...),
	}
	for _, e := range b.Enums {
		de := schemadescriptor.EnumDef{
			Name:        schemadescriptor.Name{Name: e.Name.Name, Alias: e.Name.Alias},
			Constraints: bundleDescriptorConstraints(e.Constraints),
		}
		for _, v := range e.Values {
			de.Values = append(de.Values, schemadescriptor.EnumValue{
				Name:        schemadescriptor.Name{Name: v.Name.Name, Alias: v.Name.Alias},
				Description: v.Description,
			})
		}
		out.Enums = append(out.Enums, de)
	}
	for _, c := range b.Classes {
		dc := schemadescriptor.ClassDef{
			Name:        schemadescriptor.Name{Name: c.Name.Name, Alias: c.Name.Alias},
			Description: c.Description,
			Mode:        schemadescriptor.StreamingMode(c.Mode),
			Constraints: bundleDescriptorConstraints(c.Constraints),
			Stream: schemadescriptor.StreamingBehavior{
				Needed: c.Stream.Needed, Done: c.Stream.Done, State: c.Stream.State,
			},
		}
		for _, f := range c.Fields {
			dc.Fields = append(dc.Fields, schemadescriptor.ClassField{
				Name:            schemadescriptor.Name{Name: f.Name.Name, Alias: f.Name.Alias},
				Type:            bundleDescriptorType(f.Type),
				Description:     f.Description,
				StreamingNeeded: f.StreamingNeeded,
			})
		}
		out.Classes = append(out.Classes, dc)
	}
	for _, a := range b.StructuralRecursiveAliases {
		out.StructuralRecursiveAliases = append(out.StructuralRecursiveAliases,
			schemadescriptor.RecursiveAliasDef{Name: a.Name, Target: bundleDescriptorType(a.Target)})
	}
	return out
}

func bundleDescriptorConstraints(cs []schema.Constraint) []schemadescriptor.Constraint {
	var out []schemadescriptor.Constraint
	for _, c := range cs {
		out = append(out, schemadescriptor.Constraint{
			Level:      schemadescriptor.ConstraintLevel(c.Level),
			Expression: c.Expression,
			Label:      c.Label,
		})
	}
	return out
}

func bundleDescriptorType(t schema.Type) schemadescriptor.Type {
	out := schemadescriptor.Type{
		Kind: schemadescriptor.TypeKind(t.Kind),
		Meta: schemadescriptor.TypeMeta{
			Constraints: bundleDescriptorConstraints(t.Meta.Constraints),
			Stream: schemadescriptor.StreamingBehavior{
				Needed: t.Meta.Stream.Needed, Done: t.Meta.Stream.Done, State: t.Meta.Stream.State,
			},
		},
		Primitive: schemadescriptor.PrimitiveKind(t.Primitive),
		Media:     schemadescriptor.MediaKind(t.Media),
		Name:      t.Name,
		Mode:      schemadescriptor.StreamingMode(t.Mode),
		Dynamic:   t.Dynamic,
	}
	if t.Literal != nil {
		out.Literal = &schemadescriptor.LiteralValue{
			Kind:   schemadescriptor.LiteralKind(t.Literal.Kind),
			String: t.Literal.String, Int: t.Literal.Int, Bool: t.Literal.Bool,
		}
	}
	if t.Elem != nil {
		e := bundleDescriptorType(*t.Elem)
		out.Elem = &e
	}
	if t.Key != nil {
		k := bundleDescriptorType(*t.Key)
		out.Key = &k
	}
	if t.Value != nil {
		v := bundleDescriptorType(*t.Value)
		out.Value = &v
	}
	for _, it := range t.Items {
		out.Items = append(out.Items, bundleDescriptorType(it))
	}
	if t.Union != nil {
		u := schemadescriptor.UnionType{Nullable: t.Union.Nullable}
		for _, v := range t.Union.Variants {
			u.Variants = append(u.Variants, bundleDescriptorType(v))
		}
		out.Union = &u
	}
	if t.Arrow != nil {
		a := schemadescriptor.ArrowType{Return: bundleDescriptorType(t.Arrow.Return)}
		for _, p := range t.Arrow.Params {
			a.Params = append(a.Params, bundleDescriptorType(p))
		}
		out.Arrow = &a
	}
	return out
}

// bundleWalkTypes yields t and every type nested inside it.
func bundleWalkTypes(t schema.Type, fn func(schema.Type)) {
	fn(t)
	if t.Elem != nil {
		bundleWalkTypes(*t.Elem, fn)
	}
	if t.Key != nil {
		bundleWalkTypes(*t.Key, fn)
	}
	if t.Value != nil {
		bundleWalkTypes(*t.Value, fn)
	}
	for _, it := range t.Items {
		bundleWalkTypes(it, fn)
	}
	if t.Union != nil {
		for _, v := range t.Union.Variants {
			bundleWalkTypes(v, fn)
		}
	}
	if t.Arrow != nil {
		for _, p := range t.Arrow.Params {
			bundleWalkTypes(p, fn)
		}
		bundleWalkTypes(t.Arrow.Return, fn)
	}
}

// bundleConstrainedTypeNodes returns every TYPE node of a bundle that carries a
// constraint — the target, its nested nodes, and every class field's type tree.
//
// These are exactly the nodes checkSupportedType is responsible for. A class-level
// or enum-level constraint is NOT one of them (it lives on the definition, which
// checkSupportedType never sees), so claiming a decline for it would be asserting
// something that function does not do.
func bundleConstrainedTypeNodes(b *schema.Bundle) []schema.Type {
	var out []schema.Type
	collect := func(t schema.Type) {
		bundleWalkTypes(t, func(n schema.Type) {
			if len(n.Meta.Constraints) > 0 {
				out = append(out, n)
			}
		})
	}
	collect(b.Target)
	for _, c := range b.Classes {
		for _, f := range c.Fields {
			collect(f.Type)
		}
	}
	return out
}
