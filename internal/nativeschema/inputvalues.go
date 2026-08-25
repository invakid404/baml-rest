package nativeschema

// inputvalues.go is the de-BAML Slice 7.1b V3 SOURCE-RESOLVED INPUT VALUE
// resolver. It runs inside BuildPromptDescriptors, over the SAME parsed AST and
// the SAME schemaTypeIndex the prompt eligibility scan already uses, and turns a
// function's argument types into the passive promptdescriptor V3 universe:
//
//	promptdescriptor.InputValueUniverse{ProjectEnums, Classes}
//	promptdescriptor.Argument.ValueType
//
// It is a SOURCE/TYPE-GRAPH operation and nothing else. It does not import
// bamlprofile, generated client types, MiniJinja, or BAML CFFI; it never reads a
// Go struct, a struct tag, or a generated identifier; and it never falls back to
// a name-based guess. Everything it emits is a fact the .baml source states.
//
// # What V3 claims, and what it refuses to claim
//
// The claimed value graph is scalar/null, enum, class, and list, with nullable
// edges recorded explicitly. Every other BAML node — map, tuple, union (other
// than the single-variant nullable union `T?` lowers to), literal type, media,
// @@dynamic, @skip, an ambiguous/unresolved name, a parameterized or
// unsupported class body, a multi-dimensional list, an attributed type node —
// produces NO V3 descriptor for the function. That is a build-time DECLINE with
// a stable reason recorded in StaticPromptDeclines (a #583 ledger entry), not a
// lossy approximation: a descriptor that under-describes a value is exactly how
// a native renderer would out-do BAML.
//
// A reachable RECURSIVE input class graph also declines here. ResolvedValueType
// is a name reference, so a recursive universe would be representable — but this
// slice has no stock differential for rendering one, and the generated projector
// walks the closure structurally, so the honest boundary is a build-time decline
// (the binder declines a recursive universe independently; see nativeprompt).
//
// # Enums are project-wide on purpose
//
// ProjectEnums is EVERY enum declared in the project, in deterministic parsed
// file order then within-file declaration order — deliberately broader than
// argument reachability. That is stock v0.223's model: render_prompt walks the
// IR enums and installs one namespace global per enum, so `Color.RED` resolves
// in a function that never takes a Color argument. Because the set is installed
// whole, ONE unresolvable project enum poisons V3 for EVERY function rather than
// producing a partial environment in which a namespace silently goes missing.
//
// # Alias policy
//
// A display alias is only ever taken from a SINGLE plain/raw string literal
// (attributeStringArg — the same conservative D5 policy the output-schema
// builder uses). A dynamic, multi-valued, or otherwise non-literal @alias in a
// project enum or a reachable input class DECLINES; it is never guessed at, and
// never defaulted to the canonical name, because the alias is rendered verbatim.

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// resolveProjectEnums collects EVERY project enum into deterministic source
// order (parsed file order, then within-file declaration order) with each
// member's canonical name and resolved display alias.
//
// It returns a non-empty decline reason — which poisons V3 for every function in
// the project — the moment any enum cannot be described exactly. See the file
// doc for why the failure is global rather than per-function.
func resolveProjectEnums(files []SourceFile, idx *schemaTypeIndex) ([]promptdescriptor.ResolvedEnum, string, PreDeclineFeature) {
	var out []promptdescriptor.ResolvedEnum
	seen := make(map[string]bool)

	for _, sf := range files {
		f := sf.File
		if f == nil {
			continue
		}
		for _, it := range f.Items {
			tb := it.TypeBlock
			if tb == nil || tb.Keyword != "enum" || tb.Name == "" {
				continue
			}
			if seen[tb.Name] {
				// Duplicate declaration: idx.ambiguous already records it, but a
				// second pass would also emit a duplicate namespace global.
				return nil, enumDecline(tb.Name, "declared more than once"), FeatureInputShape
			}
			seen[tb.Name] = true
			def, err := resolveEnumDef(tb, idx)
			if err != nil {
				// An @@dynamic enum is a schema_dynamic_class cause; any other
				// unresolvable project enum is a bare input-universe shape defect.
				return nil, enumDecline(tb.Name, err.Error()), enumDeclineFeatureOf(err)
			}
			out = append(out, def)
		}
	}
	return out, "", FeatureNone
}

// enumDeclineFeatureOf maps a project-enum resolution error to the structural
// feature it poisons the V3 input universe with: FeatureDynamic when the enum
// carries @@dynamic (stamped by resolveEnumDef), FeatureInputShape otherwise.
func enumDeclineFeatureOf(err error) PreDeclineFeature {
	if featureOf(err) == FeatureDynamic {
		return FeatureDynamic
	}
	return FeatureInputShape
}

// resolveEnumDef describes one enum exactly, or reports why it cannot be.
func resolveEnumDef(tb *bamlparser.TypeBlock, idx *schemaTypeIndex) (promptdescriptor.ResolvedEnum, error) {
	if idx.isAmbiguous(tb.Name) {
		return promptdescriptor.ResolvedEnum{}, fmt.Errorf("name is declared more than once (duplicate class/enum/alias)")
	}
	if tb.HasUnsupportedContent {
		return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum has unsupported body content")
	}
	if len(tb.Args) > 0 {
		return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum has a named-argument list (parameterized enums are not supported)")
	}
	// A block attribute could rename the namespace global (@@alias), attach a
	// constraint (@@check/@@assert — Slice 7.2), or mark the enum dynamic. None
	// of those has a stock differential for the PROMPT namespace, so any block
	// attribute declines rather than being assumed inert. @@dynamic is called out
	// separately because it is not merely unproven: BAML builds the IR (and hence
	// the namespace global) with the request's type_builder overlay applied, so
	// the member set genuinely is not a build-time fact.
	for _, a := range tb.Attributes {
		if a.Block && a.Name == "dynamic" {
			return promptdescriptor.ResolvedEnum{}, declineFeature(FeatureDynamic,
				"enum is @@dynamic; its member set is extended at request time by type_builder, so it is not a build-time fact")
		}
	}
	if len(tb.Attributes) > 0 {
		return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum carries block attribute %s; enum-level block attributes are not proven for the prompt namespace", attrSigil(tb.Attributes[0]))
	}
	if len(tb.Fields) == 0 {
		return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum declares no members")
	}

	members := make([]promptdescriptor.ResolvedEnumMember, 0, len(tb.Fields))
	seen := make(map[string]bool, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Name == "" {
			return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum has a member with an empty name")
		}
		if seen[m.Name] {
			return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum has duplicate member %q", m.Name)
		}
		seen[m.Name] = true
		alias, err := memberDisplayAlias(m.Attributes)
		if err != nil {
			return promptdescriptor.ResolvedEnum{}, fmt.Errorf("enum member %q: %w", m.Name, err)
		}
		members = append(members, promptdescriptor.ResolvedEnumMember{Canonical: m.Name, Alias: alias})
	}
	return promptdescriptor.ResolvedEnum{Name: tb.Name, Members: members}, nil
}

// memberDisplayAlias resolves an enum member's display alias from its
// attributes. Only @alias and @description are recognized (@description is
// prompt-inert — it renders in ctx.output_format, never in a value), and only a
// SINGLE plain/raw string literal @alias argument is a known display string.
// @skip, a constraint/stream attribute, an unknown attribute, a duplicate
// @alias, and any block attribute all decline.
func memberDisplayAlias(attrs []*bamlparser.Attribute) (*string, error) {
	var alias *string
	for _, a := range attrs {
		if a.Block {
			return nil, fmt.Errorf("stray block attribute %s", attrSigil(a))
		}
		switch a.Name {
		case "alias":
			if alias != nil {
				return nil, fmt.Errorf("duplicate @alias attribute")
			}
			v, err := attributeStringArg(a)
			if err != nil {
				// D5 / the conservative source-literal policy: a dynamic or
				// multi-valued alias is NOT a known display string.
				return nil, fmt.Errorf("@alias is not a single plain string literal: %w", err)
			}
			alias = &v
		case "description":
			if _, err := attributeStringArg(a); err != nil {
				return nil, fmt.Errorf("@description is not a single plain string literal: %w", err)
			}
		default:
			return nil, fmt.Errorf("unsupported attribute %s", attrSigil(a))
		}
	}
	return alias, nil
}

// inputValueResolver walks a function's argument type graph, producing each
// argument's ResolvedValueType and accumulating the transitive input-class
// closure. It resolves named references through the SHARED schemaTypeIndex
// (never a second resolver, never a Go type).
//
// visiting guards the ACTIVE DFS path so a reachable recursive class graph is
// detected and declined; resolved memoizes finished classes so a DAG (the same
// class reached twice) resolves once and does not falsely look recursive.
type inputValueResolver struct {
	idx      *schemaTypeIndex
	resolved map[string]promptdescriptor.ResolvedClass
	visiting map[string]bool
	// aliasPath guards alias expansion against an alias cycle. A cyclic alias is
	// already an output-schema decline, but the input resolver must not loop on
	// one before that verdict is reached.
	aliasPath map[string]bool
}

func newInputValueResolver(idx *schemaTypeIndex) *inputValueResolver {
	return &inputValueResolver{
		idx:       idx,
		resolved:  make(map[string]promptdescriptor.ResolvedClass),
		visiting:  make(map[string]bool),
		aliasPath: make(map[string]bool),
	}
}

// resolveType lowers one source type expression into a V3 value type, adding any
// reachable class to the closure. It returns the first decline it encounters.
//
// An attribute on ANY node of an input type graph declines: a @check/@assert
// reassociated onto a type node is Slice 7.2 work, and an unknown attribute is
// by definition unproven. The caller strips a field's OWN metadata attributes
// (@alias/@description) before calling, exactly as build.go's lowerFieldType
// does, so this sees only genuine type-node attributes.
func (r *inputValueResolver) resolveType(t *bamlparser.TypeExpr) (promptdescriptor.ResolvedValueType, error) {
	if t == nil {
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("missing type expression")
	}
	if len(t.Attributes) > 0 {
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("type node carries attribute %s; attributed input types are not supported", attrSigil(t.Attributes[0]))
	}

	switch t.Kind {
	case bamlparser.KindPrimitive:
		switch t.Primitive {
		case "string":
			return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}, nil
		case "int":
			return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueInt}, nil
		case "float":
			return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueFloat}, nil
		case "bool":
			return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueBool}, nil
		case "null":
			return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueNull}, nil
		default:
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("unsupported primitive %q", t.Primitive)
		}

	case bamlparser.KindNameRef:
		return r.resolveNameRef(t)

	case bamlparser.KindList:
		// A multi-dimensional list (`T[][]`) is representable as nested lists but
		// has no stock differential in this slice, so it declines rather than
		// being synthesized.
		if t.Dims != 1 {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("list has %d dimensions; only a single-dimension list is supported", t.Dims)
		}
		elem, err := r.resolveType(t.Elem)
		if err != nil {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("list element: %w", err)
		}
		// The Dims check above only sees the SPELLING at this node. A type ALIAS
		// can hide the same nested shape behind a single-dimension spelling —
		// `type L = string[]` makes `L[]` parse as Dims==1 while resolving to
		// ValueList(ValueList(string)) — so the RESOLVED element is what decides.
		// A nested list is exactly the shape `T[][]` is declined for, and it must
		// decline by the same rule however it is spelled.
		if elem.Kind == promptdescriptor.ValueList {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf(
				"list element resolves to another list (a nested list, possibly via a type alias); only a single-dimension list is supported")
		}
		return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueList, Elem: &elem}, nil

	case bamlparser.KindUnion:
		// The ONLY admitted union is the single-variant nullable union `T?`
		// lowers to. A real union is a genuine multi-shape value with no proven
		// host lowering here.
		if !t.Nullable || len(t.Variants) != 1 {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("union types are not supported (only the optional `T?` form is)")
		}
		inner, err := r.resolveType(t.Variants[0])
		if err != nil {
			return promptdescriptor.ResolvedValueType{}, err
		}
		inner.Nullable = true
		return inner, nil

	case bamlparser.KindGroup:
		return r.resolveType(t.Inner)

	case bamlparser.KindMap:
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("map types are not supported")
	case bamlparser.KindTuple:
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("tuple types are not supported")
	case bamlparser.KindLiteral:
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("literal types are not supported")
	case bamlparser.KindMedia:
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("media types are not supported")
	case bamlparser.KindUnsupported:
		reason := t.Reason
		if reason == "" {
			reason = "unsupported type"
		}
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("%s", reason)
	default:
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("unhandled type kind %d", t.Kind)
	}
}

// resolveNameRef resolves a named reference through the schema type index into
// an enum edge, a class edge (adding the class to the closure), or the expansion
// of a type alias. A type alias is EXPANDED rather than represented: a BAML
// alias has no distinct Jinja host identity, so a node for it would invent a
// distinction the renderer does not have.
func (r *inputValueResolver) resolveNameRef(t *bamlparser.TypeExpr) (promptdescriptor.ResolvedValueType, error) {
	if t.Namespaced || t.Path {
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("path/namespaced identifier %q is not supported in a type position", t.Name)
	}
	name := t.Name
	if r.idx.isAmbiguous(name) {
		return promptdescriptor.ResolvedValueType{}, fmt.Errorf("type name %q is declared more than once (duplicate class/enum/alias)", name)
	}
	if _, ok := r.idx.enums[name]; ok {
		// The definition itself lives in ProjectEnums (resolved project-wide);
		// this edge only names it.
		return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueEnum, EnumName: name}, nil
	}
	if tb, ok := r.idx.classes[name]; ok {
		if err := r.resolveClass(name, tb); err != nil {
			return promptdescriptor.ResolvedValueType{}, err
		}
		return promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueClass, ClassName: name}, nil
	}
	if alias, ok := r.idx.aliases[name]; ok {
		if r.aliasPath[name] {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("type alias %q forms a cycle", name)
		}
		if len(alias.Attributes) > 0 {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("type alias %q carries attribute %s", name, attrSigil(alias.Attributes[0]))
		}
		if alias.Expr == nil {
			return promptdescriptor.ResolvedValueType{}, fmt.Errorf("type alias %q has an unparsed right-hand side", name)
		}
		r.aliasPath[name] = true
		defer delete(r.aliasPath, name)
		return r.resolveType(alias.Expr)
	}
	return promptdescriptor.ResolvedValueType{}, fmt.Errorf("unresolved type reference %q", name)
}

// resolveClass adds one class to the transitive closure, preserving its SOURCE
// field order and canonical names and recursively lowering each field type. A
// re-entry on the ACTIVE path is a recursive input class graph and declines; a
// class already fully resolved is reused (a DAG is fine).
func (r *inputValueResolver) resolveClass(name string, tb *bamlparser.TypeBlock) error {
	if _, done := r.resolved[name]; done {
		return nil
	}
	if r.visiting[name] {
		return fmt.Errorf("class %q is part of a recursive input class graph; recursive input classes are not supported", name)
	}
	if tb.HasUnsupportedContent {
		return fmt.Errorf("class %q has unsupported body content (methods or nested blocks)", name)
	}
	if len(tb.Args) > 0 {
		return fmt.Errorf("class %q has a named-argument list (parameterized classes are not supported)", name)
	}
	// As for enums: no block attribute on an INPUT class has a stock differential
	// for value rendering (@@dynamic is a hard no; @@check/@@assert are 7.2), so
	// any of them declines rather than being assumed inert.
	if len(tb.Attributes) > 0 {
		return fmt.Errorf("class %q carries block attribute %s; class-level block attributes are not proven for input values", name, attrSigil(tb.Attributes[0]))
	}

	r.visiting[name] = true
	defer delete(r.visiting, name)

	fields := make([]promptdescriptor.ResolvedClassField, 0, len(tb.Fields))
	seen := make(map[string]bool, len(tb.Fields))
	for _, m := range tb.Fields {
		if m.Name == "" {
			return fmt.Errorf("class %q has a field with an empty name", name)
		}
		if seen[m.Name] {
			return fmt.Errorf("class %q has duplicate field %q", name, m.Name)
		}
		seen[m.Name] = true
		if m.Type == nil {
			return fmt.Errorf("class %q field %q has no type", name, m.Name)
		}
		// memberAttributes reassociates a field's trailing attributes exactly as
		// build.go does, so @alias/@description are read here and any constraint /
		// stream / unknown attribute declines.
		alias, err := memberDisplayAlias(memberAttributes(m))
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		ft, err := r.resolveType(stripOuterAttributes(m.Type))
		if err != nil {
			return fmt.Errorf("class %q field %q: %w", name, m.Name, err)
		}
		fields = append(fields, promptdescriptor.ResolvedClassField{
			Canonical: m.Name,
			Alias:     alias,
			Type:      ft,
		})
	}
	r.resolved[name] = promptdescriptor.ResolvedClass{Name: name, Fields: fields}
	return nil
}

// stripOuterAttributes returns t with its OUTERMOST attributes removed (they
// have already been partitioned by memberAttributes/memberDisplayAlias), leaving
// nested-node attributes for resolveType to decline. It never mutates t.
func stripOuterAttributes(t *bamlparser.TypeExpr) *bamlparser.TypeExpr {
	if t == nil || len(t.Attributes) == 0 {
		return t
	}
	cp := *t
	cp.Attributes = nil
	return &cp
}

// closureInDeclarationOrder returns the resolved classes in canonical project
// declaration order (schemaTypeIndex.classDeclOrder: parsed file order, then
// within-file declaration order) — NEVER Go map iteration order and never
// discovery order, so the emitted universe is byte-stable across builds.
//
// It orders by INTERSECTING the resolved set with the declaration list, so it
// asserts that the two agree before returning. They are built from the same
// parsed files and should never diverge; if they ever did, the intersection
// would silently DROP a resolved class and the descriptor would carry a
// ValueClass edge naming a class absent from InputValueUniverse.Classes. That is
// a malformed descriptor: the binder would reject it at REQUEST time, per
// request, instead of the build declining once. Fail loudly here so the
// divergence is a build-time decline with a reason.
func (r *inputValueResolver) closureInDeclarationOrder() ([]promptdescriptor.ResolvedClass, error) {
	if len(r.resolved) == 0 {
		return nil, nil
	}
	out := make([]promptdescriptor.ResolvedClass, 0, len(r.resolved))
	for _, name := range r.idx.classDeclOrder {
		if c, ok := r.resolved[name]; ok {
			out = append(out, c)
		}
	}
	if len(out) != len(r.resolved) {
		return nil, fmt.Errorf(
			"resolved input class closure has %d classes but only %d appear in the project declaration order; "+
				"the descriptor would name a class absent from its own universe", len(r.resolved), len(out))
	}
	return out, nil
}

// enumDecline frames a project-enum failure as the stable global decline reason.
func enumDecline(name, reason string) string {
	return fmt.Sprintf("input value graph cannot be resolved faithfully: project enum %q: %s", name, reason)
}

// argValueDecline frames a per-argument failure as the stable per-function
// decline reason.
func argValueDecline(arg string, err error) error {
	// %w, not %v: the message text is identical either way, but wrapping keeps the
	// underlying cause inspectable with errors.Is/errors.As instead of leaving
	// callers to substring-match a decline reason.
	return fmt.Errorf("input value graph cannot be resolved faithfully: argument %q: %w", arg, err)
}
