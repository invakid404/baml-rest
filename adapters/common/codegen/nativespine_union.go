package codegen

// nativespine_union.go is the M3b union-planning and union-carrier emission
// layer (codegen-spine slice M3b). It extends the merged M3a output-carrier core
// (nativespine.go) with the native discriminated value carrier for a
// multi-arm TypeUnion, plus the literal-arm lowering that rides on it.
//
// The carrier reproduces BAML v0.223's generated types/unions.go JSON behavior
// (the authoritative reference is in the module cache under
// engine/generators/languages/go/.../types/unions.go), MINUS the CFFI
// Decode/Encode/BamlTypeName methods and imports. Two properties are PINNED, not
// improved:
//
//   - MarshalJSON switches on the discriminator and marshals ONLY the selected
//     arm pointer via encoding/json; an unset/unknown discriminator errors.
//   - UnmarshalJSON tries arms SEQUENTIALLY in descriptor order, clearing a
//     failed arm and selecting the FIRST successful standard JSON decode. This
//     is intentionally not a semantic BAML parser: it reproduces the generated
//     Go carrier, including same-base literal ambiguity (for `true | false`,
//     generic JSON `false` decodes into the first bool arm; for `"a" | "b"`, any
//     JSON string decodes into the first string arm). No value checks are added
//     — that would make native stricter than BAML's generated carrier. Literal
//     correctness is established upstream by BAML parsing/materialization and by
//     the no-argument literal constructors.
//
// Native Go identifier spelling (OutputUnionN / VariantN) is not a public
// compatibility surface (docs/codegen-spine/05 D8: JSON projection is
// authoritative, source text is not); this layer does NOT reimplement BAML's
// private union-name mangling. It only preserves DESCRIPTOR ARM ORDER, which
// controls the first-success selection above.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// carrierPlan holds the deterministic native names assigned to every distinct
// multi-arm union reachable from a method's return graph. It is the single
// source of truth shared by name-collision preflight, schemaGoType, and
// emission, so admission can never outrun what emission can name (M3b open risk
// 4). Building the plan is the mandatory step before any Go declaration is
// written.
type carrierPlan struct {
	// byFingerprint maps a union's structural fingerprint (over its arms, NOT
	// its nullability) to the native name assigned to that carrier, so identical
	// union occurrences deduplicate to one declaration and `A|B` shares its
	// carrier with the nullable `(A|B)?` (which is emitted as `*OutputUnionN`).
	byFingerprint map[string]string
	// unions is the ordered list of distinct union carriers in first-reach order
	// of the deterministic return-graph walk (target, then class fields in
	// descriptor order, descending into lists/maps/nested unions).
	unions []plannedUnion
}

// plannedUnion is one emitted union carrier: its native name and the arms in
// descriptor order (the arms are re-resolved to Go types at emit time so a
// nested union arm can reference a later-planned carrier).
type plannedUnion struct {
	name     string
	variants []schemadescriptor.Type
}

// buildCarrierPlan walks the return graph in deterministic order and assigns a
// native name to every distinct multi-arm union. It returns a fail-closed error
// for a union shape it cannot lower to a carrier (a non-nullable/zero-arm union
// with fewer than two arms) so a direct emitter call rejects it even when the
// classifier preflight is bypassed. Structurally identical unions dedupe by
// fingerprint; the single-nullable-variant `T?` case is NOT a union carrier
// (M3a emits it as `*T`) and is walked through, not registered.
func buildCarrierPlan(ret schemadescriptor.Bundle) (*carrierPlan, error) {
	p := &carrierPlan{byFingerprint: map[string]string{}}

	var walk func(t *schemadescriptor.Type) error
	walk = func(t *schemadescriptor.Type) error {
		if t == nil {
			return nil
		}
		switch t.Kind {
		case schemadescriptor.TypeList:
			return walk(t.Elem)
		case schemadescriptor.TypeMap:
			if err := walk(t.Key); err != nil {
				return err
			}
			return walk(t.Value)
		case schemadescriptor.TypeUnion:
			if t.Union == nil {
				// A nil union payload is malformed — fail closed here rather than
				// silently skip it and defer to schemaGoType (keeps the emitter's plan
				// and type resolution symmetric, and admission == emission).
				return fmt.Errorf("union node has a nil payload")
			}
			// M3a optional-of-one (`T?`) stays `*T`; it is not a union carrier.
			if t.Union.Nullable && len(t.Union.Variants) == 1 {
				return walk(&t.Union.Variants[0])
			}
			if len(t.Union.Variants) < 2 {
				return fmt.Errorf("union with %d variant(s) cannot be lowered to a carrier", len(t.Union.Variants))
			}
			fp := unionFingerprint(t.Union.Variants)
			if _, ok := p.byFingerprint[fp]; !ok {
				name := fmt.Sprintf("%sUnion%d", outputTypeNamePrefix, len(p.unions)+1)
				p.byFingerprint[fp] = name
				p.unions = append(p.unions, plannedUnion{name: name, variants: t.Union.Variants})
			}
			// Descend into arms to discover nested acyclic unions (assigned later
			// ordinals; recursive edges are declined by the classifier, so this
			// terminates).
			for i := range t.Union.Variants {
				if err := walk(&t.Union.Variants[i]); err != nil {
					return err
				}
			}
			return nil
		default:
			// primitive, literal, enum, class: leaves for planning purposes (a
			// class/enum reference is a name; its definition is walked at the
			// bundle level).
			return nil
		}
	}

	if err := walk(&ret.Target); err != nil {
		return nil, err
	}
	for ci := range ret.Classes {
		for fi := range ret.Classes[ci].Fields {
			if err := walk(&ret.Classes[ci].Fields[fi].Type); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

// unionName resolves a multi-arm union's arms to its planned native name. A nil
// plan (or an unplanned union) reports not-found so callers fail closed.
func (p *carrierPlan) unionName(variants []schemadescriptor.Type) (string, bool) {
	if p == nil {
		return "", false
	}
	name, ok := p.byFingerprint[unionFingerprint(variants)]
	return name, ok
}

// unionFingerprint is a canonical structural key over a union's arms. The M3b
// vocabulary carries no maps inside a Type node and D1 declines any
// attribute-bearing union, so encoding/json is deterministic here and equals
// structural equality of the arm trees. Nullability is deliberately excluded so
// `A|B` and `(A|B)?` share one carrier.
func unionFingerprint(variants []schemadescriptor.Type) string {
	b, err := json.Marshal(variants)
	if err != nil {
		// A Type tree cannot fail to marshal; fall back to a spelling that still
		// distinguishes arm counts if the impossible ever happens.
		return fmt.Sprintf("!fpfail:%d", len(variants))
	}
	return string(b)
}

// literalGoBase maps a literal's kind to the ordinary Go base BAML v0.223
// generates for it (string/int64/bool). A nil payload, a float literal, or an
// unknown kind is a fail-closed error (unsupported_output_shape upstream); BAML
// v0.223 has no float literal and the descriptor model no float literal kind.
func literalGoBase(l *schemadescriptor.LiteralValue) (string, error) {
	if l == nil {
		return "", fmt.Errorf("literal has no value")
	}
	switch l.Kind {
	case schemadescriptor.LiteralString:
		return "string", nil
	case schemadescriptor.LiteralInt:
		return "int64", nil
	case schemadescriptor.LiteralBool:
		return "bool", nil
	default:
		return "", fmt.Errorf("literal kind %q is outside the carrier profile", l.Kind)
	}
}

// literalGoConst renders the Go constant expression for a literal arm's
// no-argument constructor/setter (the reference carrier installs the constant
// with `var v <base> = <const>`).
func literalGoConst(l *schemadescriptor.LiteralValue) (string, error) {
	if l == nil {
		return "", fmt.Errorf("literal has no value")
	}
	switch l.Kind {
	case schemadescriptor.LiteralString:
		return fmt.Sprintf("%q", l.String), nil
	case schemadescriptor.LiteralInt:
		return fmt.Sprintf("%d", l.Int), nil
	case schemadescriptor.LiteralBool:
		if l.Bool {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("literal kind %q is outside the carrier profile", l.Kind)
	}
}

// unionArm is one resolved arm of a planned union: its discriminator string, the
// ordinal method suffix, its Go value type, and — for a literal arm — the
// constant its no-argument constructor/setter installs.
type unionArm struct {
	field  string // struct field name: variant0, variant1, ...
	disc   string // discriminator value stored in `variant`: v0, v1, ...
	suffix string // method suffix: Variant0, Variant1, ...
	goType string // arm value Go type (int64, OutputInner, []string, *string, OutputUnion2, ...)
	isLit  bool
	litVal string // Go constant expression when isLit
}

// resolveUnionArms resolves every arm of a planned union to its Go type and
// literal metadata via schemaGoType (so a nested-union arm binds to its own
// planned carrier). It fails closed on any arm outside the carrier vocabulary.
func resolveUnionArms(u plannedUnion, plan *carrierPlan) ([]unionArm, error) {
	arms := make([]unionArm, 0, len(u.variants))
	for i := range u.variants {
		v := u.variants[i]
		gt, err := schemaGoType(v, plan)
		if err != nil {
			return nil, fmt.Errorf("union %s arm %d: %w", u.name, i, err)
		}
		a := unionArm{
			field:  fmt.Sprintf("variant%d", i),
			disc:   fmt.Sprintf("v%d", i),
			suffix: fmt.Sprintf("Variant%d", i),
			goType: gt,
		}
		if v.Kind == schemadescriptor.TypeLiteral {
			lit, err := literalGoConst(v.Literal)
			if err != nil {
				return nil, fmt.Errorf("union %s arm %d: %w", u.name, i, err)
			}
			a.isLit = true
			a.litVal = lit
		}
		arms = append(arms, a)
	}
	return arms, nil
}

// emitUnionCarrier writes the pure-Go discriminated value carrier for one
// planned union: the struct, MarshalJSON/UnmarshalJSON, and per-arm
// constructor/setter/predicate/accessor. It mirrors BAML v0.223's generated
// carrier JSON behavior without any CFFI method/import. It returns an error for
// an arm it cannot resolve (fail-closed backstop for a direct emitter call).
func emitUnionCarrier(b *strings.Builder, u plannedUnion, plan *carrierPlan) error {
	arms, err := resolveUnionArms(u, plan)
	if err != nil {
		return err
	}

	// Struct: a discriminator string plus one pointer per arm.
	fmt.Fprintf(b, "// %s is a generated output union carrier: a discriminated value\n", u.name)
	fmt.Fprintf(b, "// holding exactly one arm, reproducing BAML v0.223 generated-carrier JSON behavior.\n")
	fmt.Fprintf(b, "type %s struct {\n", u.name)
	fmt.Fprintf(b, "\tvariant string\n")
	for _, a := range arms {
		fmt.Fprintf(b, "\t%s *%s\n", a.field, a.goType)
	}
	fmt.Fprintf(b, "}\n\n")

	// MarshalJSON: switch on the discriminator, marshal only the selected arm
	// pointer; unset/unknown errors (BAML parity: an unset union does not
	// serialize).
	fmt.Fprintf(b, "// MarshalJSON emits the selected arm; an unset/unknown discriminator errors.\n")
	fmt.Fprintf(b, "func (u %s) MarshalJSON() ([]byte, error) {\n", u.name)
	fmt.Fprintf(b, "\tswitch u.variant {\n")
	for _, a := range arms {
		fmt.Fprintf(b, "\tcase %q:\n\t\treturn json.Marshal(u.%s)\n", a.disc, a.field)
	}
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\treturn nil, fmt.Errorf(%q, u.variant)\n", "nativespine: "+u.name+": invalid union variant %q")
	fmt.Fprintf(b, "}\n\n")

	// UnmarshalJSON: try arms sequentially in descriptor order, clearing a failed
	// arm, first-success wins. No value checks (BAML parity, incl. same-base
	// literal ambiguity).
	fmt.Fprintf(b, "// UnmarshalJSON tries arms sequentially in descriptor order, first-success\n")
	fmt.Fprintf(b, "// (BAML v0.223 generated-carrier parity: no value checks; same-base arms are ambiguous).\n")
	fmt.Fprintf(b, "func (u *%s) UnmarshalJSON(data []byte) error {\n", u.name)
	for _, a := range arms {
		fmt.Fprintf(b, "\tif err := json.Unmarshal(data, &u.%s); err == nil {\n", a.field)
		fmt.Fprintf(b, "\t\tu.variant = %q\n\t\treturn nil\n\t}\n", a.disc)
		fmt.Fprintf(b, "\tu.%s = nil\n", a.field)
	}
	fmt.Fprintf(b, "\treturn fmt.Errorf(%q, string(data))\n", "nativespine: "+u.name+": no union variant matched: %s")
	fmt.Fprintf(b, "}\n\n")

	// Per-arm constructor / setter / predicate / accessor. Every setter clears
	// all OTHER arm pointers.
	for i, a := range arms {
		if a.isLit {
			// Literal arm: no-argument constructor/setter install the constant.
			fmt.Fprintf(b, "// %s%s constructs %s holding literal arm %d.\n", u.name, "New"+a.suffix, u.name, i)
			fmt.Fprintf(b, "func %sNew%s() %s {\n", u.name, a.suffix, u.name)
			fmt.Fprintf(b, "\tv := %s(%s)\n", a.goType, a.litVal)
			fmt.Fprintf(b, "\treturn %s{variant: %q, %s: &v}\n}\n\n", u.name, a.disc, a.field)

			fmt.Fprintf(b, "// Set%s selects literal arm %d, clearing the others.\n", a.suffix, i)
			fmt.Fprintf(b, "func (u *%s) Set%s() {\n", u.name, a.suffix)
			fmt.Fprintf(b, "\tv := %s(%s)\n", a.goType, a.litVal)
			fmt.Fprintf(b, "\tu.variant = %q\n\tu.%s = &v\n", a.disc, a.field)
			emitClearOtherArms(b, arms, i)
			fmt.Fprintf(b, "}\n\n")
		} else {
			fmt.Fprintf(b, "// %sNew%s constructs %s holding arm %d.\n", u.name, a.suffix, u.name, i)
			fmt.Fprintf(b, "func %sNew%s(v %s) %s {\n", u.name, a.suffix, a.goType, u.name)
			fmt.Fprintf(b, "\treturn %s{variant: %q, %s: &v}\n}\n\n", u.name, a.disc, a.field)

			fmt.Fprintf(b, "// Set%s selects arm %d, clearing the others.\n", a.suffix, i)
			fmt.Fprintf(b, "func (u *%s) Set%s(v %s) {\n", u.name, a.suffix, a.goType)
			fmt.Fprintf(b, "\tu.variant = %q\n\tu.%s = &v\n", a.disc, a.field)
			emitClearOtherArms(b, arms, i)
			fmt.Fprintf(b, "}\n\n")
		}

		fmt.Fprintf(b, "// Is%s reports whether arm %d is selected.\n", a.suffix, i)
		fmt.Fprintf(b, "func (u *%s) Is%s() bool { return u.variant == %q }\n\n", u.name, a.suffix, a.disc)

		fmt.Fprintf(b, "// As%s returns the arm-%d pointer, or nil when another arm is selected.\n", a.suffix, i)
		fmt.Fprintf(b, "func (u *%s) As%s() *%s {\n", u.name, a.suffix, a.goType)
		fmt.Fprintf(b, "\tif u.variant != %q {\n\t\treturn nil\n\t}\n\treturn u.%s\n}\n\n", a.disc, a.field)
	}
	return nil
}

// emitClearOtherArms writes `u.variantK = nil` for every arm K != selected.
func emitClearOtherArms(b *strings.Builder, arms []unionArm, selected int) {
	for k, a := range arms {
		if k == selected {
			continue
		}
		fmt.Fprintf(b, "\tu.%s = nil\n", a.field)
	}
}
