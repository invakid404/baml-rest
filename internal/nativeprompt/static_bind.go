package nativeprompt

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/invakid404/minijinja-go/v2/value"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/bamlprofile"
)

// static_bind.go is the de-BAML Slice 7.1b V3 BINDER: the single seam that turns
// a descriptor's source-resolved input universe plus one call's projected
// argument vector into bound bamlprofile host values.
//
// It replaces the primitive-only binder. There is deliberately NO map-based
// overload left: a call site that has only the raw `map[string]any` invocation
// args cannot state nested order, canonical names, or enum identity, so it is
// ineligible by construction rather than by discipline. The raw map survives at
// the generated invocation boundary because it proves BAML REQUEST facts; it is
// never a source of host-value semantics.
//
// # Two independent authorities, both required
//
//   - the DESCRIPTOR's V3 universe is the only authority for enum namespaces,
//     canonical enum identity, display aliases, class field order/aliases, and
//     list element types. It comes from .baml source.
//   - the PROJECTED vector is the only authority for the per-call values. It
//     comes from generated code with exact type assertions.
//
// The binder validates one against the other and declines on ANY disagreement.
// Neither half is trusted alone: a projector that proposed an unknown enum
// member, a wrong class field order, or a shifted argument vector is rejected
// here rather than rendered.
//
// # What it refuses
//
// There is no coercion anywhere. A wrong kind, a wrong named type, a wrong field
// count/name/order, an unknown enum canonical value, a malformed list item, a
// nullable edge, a null value, or a reachable recursive class graph is a
// decline. Those are #583 ledger items, not gaps to paper over: the parity rule
// is that a shape without a stock differential falls back to BAML.

// v3Universe is the validated view of a descriptor's InputValueUniverse. It is
// built ONCE per prepareStatic run, before any template is considered, so a
// malformed descriptor can never become a support claim.
//
// The lookup maps are an internal index over the descriptor's ORDERED slices;
// they answer "does this name exist" only. No rendered order, member order, or
// field order is ever taken from them — those always come from the slices.
type v3Universe struct {
	enums   map[string]*promptdescriptor.ResolvedEnum
	classes map[string]*promptdescriptor.ResolvedClass
	// enumMembers indexes each enum's members by canonical name for O(1) identity
	// checks; the ordered Members slice remains the authority for everything else.
	enumMembers map[string]map[string]*promptdescriptor.ResolvedEnumMember
	// defs is the descriptor's enum slice in source order, forwarded verbatim to
	// bamlprofile.Config so the installed namespace globals are the stock-like
	// complete set.
	defs []bamlprofile.EnumDef
}

// validateUniverse checks the descriptor's V3 universe for internal consistency
// BEFORE a template or a value is considered (binder step 1). A malformed
// universe is a descriptor contract violation, so it declines rather than
// rendering through a half-valid environment.
func validateUniverse(u promptdescriptor.InputValueUniverse) (*v3Universe, error) {
	out := &v3Universe{
		enums:       make(map[string]*promptdescriptor.ResolvedEnum, len(u.ProjectEnums)),
		classes:     make(map[string]*promptdescriptor.ResolvedClass, len(u.Classes)),
		enumMembers: make(map[string]map[string]*promptdescriptor.ResolvedEnumMember, len(u.ProjectEnums)),
		defs:        make([]bamlprofile.EnumDef, 0, len(u.ProjectEnums)),
	}

	for i := range u.ProjectEnums {
		e := &u.ProjectEnums[i]
		if e.Name == "" {
			return nil, decline(FeatureStaticDescriptor, "V3 universe has an enum with an empty name")
		}
		if reservedGlobalNames[e.Name] {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("V3 enum %q collides with a reserved render global", e.Name))
		}
		if _, dup := out.enums[e.Name]; dup {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("V3 universe declares enum %q more than once", e.Name))
		}
		if len(e.Members) == 0 {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("V3 enum %q declares no members", e.Name))
		}
		members := make(map[string]*promptdescriptor.ResolvedEnumMember, len(e.Members))
		def := bamlprofile.EnumDef{Name: e.Name, Values: make([]bamlprofile.EnumValue, 0, len(e.Members))}
		for j := range e.Members {
			m := &e.Members[j]
			if m.Canonical == "" {
				return nil, decline(FeatureStaticDescriptor,
					fmt.Sprintf("V3 enum %q has a member with an empty canonical name", e.Name))
			}
			if _, dup := members[m.Canonical]; dup {
				return nil, decline(FeatureStaticDescriptor,
					fmt.Sprintf("V3 enum %q declares member %q more than once", e.Name, m.Canonical))
			}
			members[m.Canonical] = m
			def.Values = append(def.Values, bamlprofile.EnumValue{Canonical: m.Canonical, Alias: m.Alias})
		}
		out.enums[e.Name] = e
		out.enumMembers[e.Name] = members
		out.defs = append(out.defs, def)
	}

	for i := range u.Classes {
		c := &u.Classes[i]
		if c.Name == "" {
			return nil, decline(FeatureStaticDescriptor, "V3 universe has a class with an empty name")
		}
		if _, dup := out.classes[c.Name]; dup {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("V3 universe declares class %q more than once", c.Name))
		}
		if _, clash := out.enums[c.Name]; clash {
			return nil, decline(FeatureStaticDescriptor,
				fmt.Sprintf("V3 universe declares %q as BOTH an enum and a class", c.Name))
		}
		seen := make(map[string]bool, len(c.Fields))
		for j := range c.Fields {
			f := &c.Fields[j]
			if f.Canonical == "" {
				return nil, decline(FeatureStaticDescriptor,
					fmt.Sprintf("V3 class %q has a field with an empty canonical name", c.Name))
			}
			if seen[f.Canonical] {
				return nil, decline(FeatureStaticDescriptor,
					fmt.Sprintf("V3 class %q declares field %q more than once", c.Name, f.Canonical))
			}
			seen[f.Canonical] = true
		}
		out.classes[c.Name] = c
	}

	// Every named edge must resolve, and no edge may be malformed. This runs
	// after both name tables exist so a forward reference is fine.
	for i := range u.Classes {
		c := &u.Classes[i]
		for j := range c.Fields {
			if err := out.checkEdge(&c.Fields[j].Type); err != nil {
				return nil, decline(FeatureStaticDescriptor,
					fmt.Sprintf("V3 class %q field %q: %v", c.Name, c.Fields[j].Canonical, err))
			}
		}
	}
	return out, nil
}

// checkEdge validates one value-type node's shape and named reference. It does
// NOT reject a nullable edge — the descriptor is allowed to describe one
// truthfully; the BINDER refuses to bind it (see bindValue).
func (u *v3Universe) checkEdge(t *promptdescriptor.ResolvedValueType) error {
	if t == nil {
		return fmt.Errorf("missing value type")
	}
	switch t.Kind {
	case promptdescriptor.ValueString, promptdescriptor.ValueInt,
		promptdescriptor.ValueFloat, promptdescriptor.ValueBool, promptdescriptor.ValueNull:
		if t.EnumName != "" || t.ClassName != "" || t.Elem != nil {
			return fmt.Errorf("scalar value type carries an enum/class/element edge")
		}
		return nil
	case promptdescriptor.ValueEnum:
		if t.ClassName != "" || t.Elem != nil {
			return fmt.Errorf("enum value type carries a class/element edge")
		}
		if _, ok := u.enums[t.EnumName]; !ok {
			return fmt.Errorf("enum %q is not in the V3 universe", t.EnumName)
		}
		return nil
	case promptdescriptor.ValueClass:
		if t.EnumName != "" || t.Elem != nil {
			return fmt.Errorf("class value type carries an enum/element edge")
		}
		if _, ok := u.classes[t.ClassName]; !ok {
			return fmt.Errorf("class %q is not in the V3 universe", t.ClassName)
		}
		return nil
	case promptdescriptor.ValueList:
		if t.EnumName != "" || t.ClassName != "" {
			return fmt.Errorf("list value type carries an enum/class edge")
		}
		if t.Elem == nil {
			return fmt.Errorf("list value type has no element type")
		}
		// A NESTED list is refused here independently of the source resolver
		// (which also declines it, including the alias-hidden `type L = string[];
		// F(x: L[])` spelling). Two things ride on this rejection:
		//
		//   - it is the same unproven shape `T[][]` declines for, so a hand-built
		//     or future descriptor cannot smuggle it past the binder;
		//   - it BOUNDS this walk. A malformed descriptor whose Elem points back
		//     at itself is a list-of-list by construction, so it is rejected here
		//     rather than recursed into — which is what keeps checkEdge (and
		//     assertAcyclic, which walks the same Elem edges) from overflowing the
		//     stack on a cyclic value-type graph.
		if t.Elem.Kind == promptdescriptor.ValueList {
			return fmt.Errorf("list element is itself a list; nested lists are not supported")
		}
		return u.checkEdge(t.Elem)
	default:
		return fmt.Errorf("unknown value kind %q", string(t.Kind))
	}
}

// argBinding is one bound argument: its declared V3 value type (the grammar
// gate's type input), the constructed host value, and whether that value renders
// a non-whitespace string (the value-aware chat-layout input).
type argBinding struct {
	name  string
	vtype *promptdescriptor.ResolvedValueType
	value value.Value
	nonWS bool
}

// checkV3ArgDeclarations runs the argument-DECLARATION gate over a V3
// descriptor. Every argument must have a unique, non-empty name that shadows
// neither a reserved render global nor a PROJECT ENUM NAMESPACE, and must carry
// a resolved V3 value type.
//
// The enum-namespace collision check is load-bearing: bamlprofile installs one
// global per project enum, and a bound variable of the same name would shadow
// it, so `Color.RED` in the template would silently mean "attribute RED of the
// argument Color". Declining is the only honest answer — BAML resolves that
// spelling against its own IR, and this slice has no differential for the
// shadowed reading.
func checkV3ArgDeclarations(args []promptdescriptor.Argument, u *v3Universe) ([]promptdescriptor.Argument, error) {
	seen := make(map[string]bool, len(args))
	for _, a := range args {
		if a.Name == "" {
			return nil, decline(FeatureStaticArgType, "argument has an empty name")
		}
		if reservedGlobalNames[a.Name] {
			return nil, decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q shadows a reserved global", a.Name))
		}
		if _, clash := u.enums[a.Name]; clash {
			return nil, decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q shadows the project enum namespace global of the same name", a.Name))
		}
		if seen[a.Name] {
			return nil, decline(FeatureStaticArgValue,
				fmt.Sprintf("duplicate argument declaration %q", a.Name))
		}
		seen[a.Name] = true
		if a.ValueType == nil {
			return nil, decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q has no V3 resolved value type", a.Name))
		}
		if err := u.checkEdge(a.ValueType); err != nil {
			return nil, decline(FeatureStaticArgType,
				fmt.Sprintf("argument %q value type is malformed: %v", a.Name, err))
		}
		// An acyclic value closure is the ONLY class/list surface this slice
		// claims. A recursive graph is representable in V3 (edges are name
		// references) and the source resolver already declines one at build time;
		// this is the independent binder-side fence, so a hand-built or future
		// descriptor cannot reach a shape with no stock differential.
		if err := u.assertAcyclic(a.ValueType, map[string]bool{}); err != nil {
			return nil, decline(FeatureEnumClassValue,
				fmt.Sprintf("argument %q: %v", a.Name, err))
		}
	}
	return args, nil
}

// assertAcyclic walks the class/list edges reachable from t and reports the
// first class re-entered on the ACTIVE path. A DAG (the same class reached
// twice through different fields) is fine; only a cycle declines.
func (u *v3Universe) assertAcyclic(t *promptdescriptor.ResolvedValueType, onPath map[string]bool) error {
	if t == nil {
		return nil // checkEdge already rejected a nil edge; nothing to walk.
	}
	switch t.Kind {
	case promptdescriptor.ValueList:
		return u.assertAcyclic(t.Elem, onPath)
	case promptdescriptor.ValueClass:
		if onPath[t.ClassName] {
			return fmt.Errorf("class %q is part of a recursive input class graph, which is not supported", t.ClassName)
		}
		onPath[t.ClassName] = true
		defer delete(onPath, t.ClassName)
		def := u.classes[t.ClassName]
		for i := range def.Fields {
			if err := u.assertAcyclic(&def.Fields[i].Type, onPath); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// bindV3Args validates the ordered PROJECTED argument vector against the
// descriptor's arguments and binds each value (binder steps 2 and 3).
//
// The vector must match the declaration by COUNT, ORDER, and NAME. Order is
// checked (not just membership) because it is the only evidence the generated
// projector and the descriptor agree about which value is which; a permuted
// vector with the right names would otherwise bind silently wrong values.
func bindV3Args(args []promptdescriptor.Argument, values []promptdescriptor.ArgumentValue, u *v3Universe) ([]argBinding, error) {
	if len(values) != len(args) {
		return nil, decline(FeatureStaticArgValue,
			fmt.Sprintf("projected argument vector has %d values for %d declared arguments", len(values), len(args)))
	}
	out := make([]argBinding, 0, len(args))
	for i, a := range args {
		v := values[i]
		if v.Name != a.Name {
			return nil, decline(FeatureStaticArgValue,
				fmt.Sprintf("projected argument %d is %q but the signature declares %q", i, v.Name, a.Name))
		}
		bound, err := bindValue(a.Name, a.ValueType, v.Value, u)
		if err != nil {
			return nil, err
		}
		out = append(out, argBinding{
			name:  a.Name,
			vtype: a.ValueType,
			value: bound,
			nonWS: renderedNonWhitespaceV3(a.ValueType, v.Value, u),
		})
	}
	return out, nil
}

// bindValue recursively binds one projected value against its declared V3 type,
// constructing bamlprofile host values for enum/class/list and the explicit fork
// scalar constructors for scalars. Every mismatch declines; nothing is coerced.
//
// path names the value being bound for the decline detail (e.g.
// `palette.swatch.color`); it carries argument names and canonical field names
// only, never a value.
func bindValue(path string, want *promptdescriptor.ResolvedValueType, got promptdescriptor.StaticValue, u *v3Universe) (value.Value, error) {
	// Defensive: validateUniverse / checkV3ArgDeclarations already rejected a nil
	// or malformed edge, so this is unreachable — but a nil deref on a serving
	// path would be a panic where a decline belongs.
	if want == nil {
		return value.Value{}, decline(FeatureStaticArgType,
			fmt.Sprintf("%s has no resolved value type", path))
	}
	// A NULLABLE edge is describable but not bindable in this slice: BAML lowers
	// an optional to a pointer whose nil case has no stock differential here, and
	// a non-nil value on a nullable edge is equally unproven. Both decline (#583).
	if want.Nullable {
		return value.Value{}, decline(FeatureStaticArgType,
			fmt.Sprintf("%s is a nullable value type; nullable inputs are not supported", path))
	}
	// The DECLARED-type refusal is checked before the projected value, so a null
	// declaration is a type decline (what the source says) rather than a value
	// decline (what one call happened to pass).
	if want.Kind == promptdescriptor.ValueNull {
		return value.Value{}, decline(FeatureStaticArgType,
			fmt.Sprintf("%s is declared null; the null value type is not supported", path))
	}
	if got.Kind == promptdescriptor.StaticNull {
		return value.Value{}, decline(FeatureStaticArgValue,
			fmt.Sprintf("%s is null; null inputs are not supported", path))
	}

	switch want.Kind {
	case promptdescriptor.ValueString:
		if got.Kind != promptdescriptor.StaticString {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		if !utf8.ValidString(got.String) {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s string is not valid UTF-8", path))
		}
		// A reserved marker inside a value is fenced byte-faithfully on the
		// RENDERED output (validateRenderedMarkers), not per-piece here.
		return value.FromString(got.String), nil

	case promptdescriptor.ValueInt:
		if got.Kind != promptdescriptor.StaticInt {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		return value.FromInt(got.Int), nil

	case promptdescriptor.ValueFloat:
		if got.Kind != promptdescriptor.StaticFloat {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		if math.IsNaN(got.Float) || math.IsInf(got.Float, 0) {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s float is non-finite (NaN/Inf)", path))
		}
		return value.FromFloat(got.Float), nil

	case promptdescriptor.ValueBool:
		if got.Kind != promptdescriptor.StaticBool {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		return value.FromBool(got.Bool), nil

	case promptdescriptor.ValueEnum:
		if got.Kind != promptdescriptor.StaticEnum {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		if got.TypeName != want.EnumName {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s is enum %q but the projected value names enum %q", path, want.EnumName, got.TypeName))
		}
		member, ok := u.enumMembers[want.EnumName][got.Canonical]
		if !ok {
			// The projector proposes a CANDIDATE canonical name read from the
			// generated Go value; V3 is what decides whether it is a real member.
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s: %q is not a canonical member of enum %q", path, got.Canonical, want.EnumName))
		}
		return bamlprofile.EnumMember(want.EnumName, member.Canonical, member.Alias)

	case promptdescriptor.ValueClass:
		if got.Kind != promptdescriptor.StaticClass {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		if got.TypeName != want.ClassName {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s is class %q but the projected value names class %q", path, want.ClassName, got.TypeName))
		}
		def := u.classes[want.ClassName]
		if len(got.Fields) != len(def.Fields) {
			return value.Value{}, decline(FeatureStaticArgValue,
				fmt.Sprintf("%s (class %q) has %d projected fields, want %d", path, want.ClassName, len(got.Fields), len(def.Fields)))
		}
		fields := make([]bamlprofile.ClassField, 0, len(def.Fields))
		for i := range def.Fields {
			d := &def.Fields[i]
			g := got.Fields[i]
			// Field ORDER is the descriptor's source order, and the projected
			// vector must already be in it: BAML renders a class in source field
			// order, so a reordered vector is a divergence, not a detail.
			if g.Canonical != d.Canonical {
				return value.Value{}, decline(FeatureStaticArgValue,
					fmt.Sprintf("%s (class %q) field %d is %q but the source declares %q at that position",
						path, want.ClassName, i, g.Canonical, d.Canonical))
			}
			bound, err := bindValue(path+"."+d.Canonical, &d.Type, g.Value, u)
			if err != nil {
				return value.Value{}, err
			}
			fields = append(fields, bamlprofile.ClassField{
				Canonical: d.Canonical,
				Alias:     d.Alias,
				Value:     bound,
			})
		}
		return bamlprofile.ClassValue(fields)

	case promptdescriptor.ValueList:
		if got.Kind != promptdescriptor.StaticList {
			return value.Value{}, kindMismatch(path, want.Kind, got.Kind)
		}
		if want.Elem == nil {
			return value.Value{}, decline(FeatureStaticArgType,
				fmt.Sprintf("%s is a list with no element type", path))
		}
		items := make([]value.Value, 0, len(got.Items))
		for i := range got.Items {
			bound, err := bindValue(fmt.Sprintf("%s[%d]", path, i), want.Elem, got.Items[i], u)
			if err != nil {
				return value.Value{}, err
			}
			items = append(items, bound)
		}
		return bamlprofile.ListValue(items)

	default:
		return value.Value{}, decline(FeatureStaticArgType,
			fmt.Sprintf("%s has unknown value kind %q", path, string(want.Kind)))
	}
}

// kindMismatch builds the no-coercion decline for a projected value whose kind
// disagrees with its declared V3 type.
func kindMismatch(path string, want promptdescriptor.ValueKind, got promptdescriptor.StaticValueKind) error {
	return decline(FeatureStaticArgValue,
		fmt.Sprintf("%s is declared %s but the projected value is %s (no coercion)", path, string(want), string(got)))
}

// renderedNonWhitespaceV3 reports whether a bound value renders to a string with
// at least one non-whitespace rune, feeding the value-aware chat-layout check.
//
//   - a string does iff it is not whitespace-only; int/float/bool always do;
//   - an ENUM renders its alias-or-canonical DISPLAY, so a member carrying a
//     whitespace-only @alias("  ") really does render nothing. That is computed
//     from the universe, never assumed — guessing "true" here would let a chat
//     message whose only content is such an enum pass the layout gate and then
//     drop to zero parts at render, the exact divergence the check prevents;
//   - a CLASS renders Rust's alternate debug-map and a LIST its debug-list; both
//     are `{}`/`{...}` / `[]`/`[...]`, always non-whitespace even when empty.
//
// The value has already been bound, so the shapes here are exactly the ones
// bindValue accepted.
func renderedNonWhitespaceV3(want *promptdescriptor.ResolvedValueType, got promptdescriptor.StaticValue, u *v3Universe) bool {
	switch want.Kind {
	case promptdescriptor.ValueString:
		return strings.TrimSpace(got.String) != ""
	case promptdescriptor.ValueEnum:
		m, ok := u.enumMembers[want.EnumName][got.Canonical]
		if !ok {
			// Unreachable: bindValue already resolved the member.
			return false
		}
		if m.Alias != nil {
			return strings.TrimSpace(*m.Alias) != ""
		}
		return strings.TrimSpace(m.Canonical) != ""
	default:
		return true
	}
}
