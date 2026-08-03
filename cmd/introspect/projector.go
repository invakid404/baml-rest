package main

// projector.go is the de-BAML Slice 7.1b GENERATED ARGUMENT PROJECTOR emitter.
//
// A V3 descriptor says what a value MEANS; it cannot read a concrete generated
// `types.Palette`. This file emits the other half: per-method Go code that turns
// the generated call's ALREADY-TYPED arguments into the neutral, ordered
// promptdescriptor.ArgumentValue vector the native binder consumes.
//
//	var StaticPromptArgumentProjectors = map[string]func([]any) ([]promptdescriptor.ArgumentValue, bool){...}
//	var StaticPromptProjectorDeclines  = map[string]string{...}
//	func StaticPromptArgumentValues(method string, args []any) ([]promptdescriptor.ArgumentValue, bool)
//
// # Why generated code and not reflection
//
// Every emitted projector uses EXACT type assertions and DIRECT field selectors,
// and writes the canonical BAML field/member names as string LITERALS taken from
// the V3 source graph. It never calls Encode(), marshals JSON, reads a struct
// tag at runtime, iterates a map, or derives a canonical name from a display
// alias. Those are precisely the Go-convention-for-BAML-semantics substitutions
// the slice exists to remove.
//
// # The build-time audit is what makes the selectors safe
//
// A selector is only emitted after auditing the generated client's `types`
// package AST:
//
//   - a V3 enum must be a generated `type <Name> string` (so `string(v)` is the
//     CANDIDATE canonical member — which the binder still validates against V3);
//   - a V3 class must be a generated struct in which EACH canonical BAML field
//     name matches EXACTLY ONE Go field by its `json:"<canonical>"` tag, whose Go
//     type is recursively compatible with that field's V3 type, and which has no
//     extra tagged field (an extra field is drift, not a value we may ignore).
//
// The V3 SOURCE graph supplies the canonical names and their order; the AST
// check only proves which Go selector carries each one. A missing, ambiguous, or
// mismatched mapping emits NO projector for that method and records a reason —
// which makes the method a pre-render static decline (#583), never a guess.
//
// SECURITY: a projector reads real request arguments. The emitted code never
// logs, formats, or stores them; it only rebuilds them into the neutral carrier.

import (
	"fmt"
	"go/ast"
	"sort"
	"strconv"
	"strings"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// typesField is one audited field of a generated client struct.
type typesField struct {
	goName string // the Go selector, e.g. "Primary"
	json   string // the `json:"..."` name (before any comma); "" when untagged
	typ    ast.Expr
}

// typesIndex is the AST audit's view of the generated client's `types`
// subpackage: the struct types (BAML classes) and the string-underlying named
// types (BAML enums). It is built from the SAME WalkDir that already parses the
// generated client, so no second parse of the tree happens.
type typesIndex struct {
	// pkgPath is the import path of the generated `types` package.
	pkgPath string
	// structs maps a generated struct type name to its fields in declaration
	// order. A name declared twice is recorded in ambiguous instead.
	structs map[string][]typesField
	// stringEnums holds every generated `type <Name> string` declaration.
	stringEnums map[string]bool
	// ambiguous holds every type name declared more than once in the package. A
	// duplicated name is never a usable selector target.
	ambiguous map[string]bool
}

func newTypesIndex(pkgPath string) *typesIndex {
	return &typesIndex{
		pkgPath:     pkgPath,
		structs:     map[string][]typesField{},
		stringEnums: map[string]bool{},
		ambiguous:   map[string]bool{},
	}
}

// addFile records every top-level type declaration of one parsed file from the
// generated `types` package.
func (idx *typesIndex) addFile(f *ast.File) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok.String() != "type" {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil || ts.Name.Name == "" {
				continue
			}
			name := ts.Name.Name
			if idx.known(name) {
				idx.ambiguous[name] = true
				continue
			}
			switch t := ts.Type.(type) {
			case *ast.StructType:
				idx.structs[name] = structFields(t)
			case *ast.Ident:
				if t.Name == "string" {
					idx.stringEnums[name] = true
				}
			}
		}
	}
}

func (idx *typesIndex) known(name string) bool {
	if idx.ambiguous[name] {
		return true
	}
	_, isStruct := idx.structs[name]
	return isStruct || idx.stringEnums[name]
}

// structFields extracts a struct's fields in declaration order with their
// resolved `json:"..."` names. An embedded field (no name) is retained with an
// empty goName so a class audit sees it and refuses the mapping rather than
// silently ignoring it.
func structFields(st *ast.StructType) []typesField {
	var out []typesField
	if st.Fields == nil {
		return out
	}
	for _, f := range st.Fields.List {
		tag := jsonTagName(f.Tag)
		if len(f.Names) == 0 {
			out = append(out, typesField{json: tag, typ: f.Type})
			continue
		}
		for _, n := range f.Names {
			out = append(out, typesField{goName: n.Name, json: tag, typ: f.Type})
		}
	}
	return out
}

// jsonTagName returns the field's json tag NAME (the part before the first
// comma), or "" when the field is untagged / the tag is unparsable / the tag is
// the explicit "-" skip marker.
func jsonTagName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	val, ok := structTag(raw)["json"]
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(val, ",")
	if name == "-" {
		return ""
	}
	return name
}

// structTag parses a Go struct tag into its key/value pairs. It follows the
// conventional `key:"value"` space-separated encoding; a malformed tail is
// ignored (the caller then simply sees no json key and refuses the mapping).
func structTag(tag string) map[string]string {
	out := map[string]string{}
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]
		value, err := strconv.Unquote(quoted)
		if err != nil {
			break
		}
		out[name] = value
	}
	return out
}

// ---------------------------------------------------------------------------
// The audit: does a V3 value type have an exact generated Go carrier?
// ---------------------------------------------------------------------------

// projectorAudit walks a method's V3 argument types against the generated types
// package and reports the first reason a projector cannot be emitted, or "" when
// every argument (and every class it reaches) has an exact selector mapping.
//
// It also collects the distinct value types that need an emitted helper, so the
// caller can emit each helper exactly once for the whole package.
type projectorAudit struct {
	idx      *typesIndex
	universe promptdescriptor.InputValueUniverse
	// need collects every value type reachable from an audited argument, keyed
	// by its mangled helper name so the set is deduplicated deterministically.
	need map[string]promptdescriptor.ResolvedValueType
	// visiting guards the class walk. The V3 resolver already declines a
	// recursive input class graph, so this only exists so a hand-broken
	// descriptor cannot make generation loop forever.
	visiting map[string]bool
}

func newProjectorAudit(idx *typesIndex, u promptdescriptor.InputValueUniverse) *projectorAudit {
	return &projectorAudit{
		idx:      idx,
		universe: u,
		need:     map[string]promptdescriptor.ResolvedValueType{},
		visiting: map[string]bool{},
	}
}

// auditValueType validates one V3 value node and records the helpers it needs.
func (a *projectorAudit) auditValueType(t *promptdescriptor.ResolvedValueType) error {
	if t == nil {
		return fmt.Errorf("argument has no resolved V3 value type")
	}
	// A nullable edge (and the bare `null` type) has no fixture-proven rendering
	// in this slice, and BAML's Go lowering for an optional is a pointer whose
	// nil case is exactly the unproven shape. Decline rather than project it.
	if t.Nullable {
		return fmt.Errorf("nullable value types are not projected in this slice")
	}
	switch t.Kind {
	case promptdescriptor.ValueString, promptdescriptor.ValueInt,
		promptdescriptor.ValueFloat, promptdescriptor.ValueBool:
		a.need[mangleValueType(*t)] = *t
		return nil
	case promptdescriptor.ValueNull:
		return fmt.Errorf("the null value type is not projected in this slice")
	case promptdescriptor.ValueEnum:
		if _, ok := findEnum(a.universe, t.EnumName); !ok {
			return fmt.Errorf("enum %q is not in the descriptor's V3 universe", t.EnumName)
		}
		if a.idx == nil || !a.idx.stringEnums[t.EnumName] || a.idx.ambiguous[t.EnumName] {
			return fmt.Errorf("generated types package has no unambiguous `type %s string` declaration", t.EnumName)
		}
		a.need[mangleValueType(*t)] = *t
		return nil
	case promptdescriptor.ValueClass:
		if err := a.auditClass(t.ClassName); err != nil {
			return err
		}
		a.need[mangleValueType(*t)] = *t
		return nil
	case promptdescriptor.ValueList:
		if t.Elem == nil {
			return fmt.Errorf("list value type has no element type")
		}
		if err := a.auditValueType(t.Elem); err != nil {
			return err
		}
		a.need[mangleValueType(*t)] = *t
		return nil
	default:
		return fmt.Errorf("unknown V3 value kind %q", string(t.Kind))
	}
}

// auditClass proves the generated struct for one V3 class carries EXACTLY the
// V3 field set: one Go field per canonical name (matched by its json tag), each
// with a recursively compatible Go type, and no extra tagged field.
func (a *projectorAudit) auditClass(name string) error {
	def, ok := findClass(a.universe, name)
	if !ok {
		return fmt.Errorf("class %q is not in the descriptor's V3 universe", name)
	}
	if a.visiting[name] {
		return fmt.Errorf("class %q is part of a recursive graph", name)
	}
	a.visiting[name] = true
	defer delete(a.visiting, name)

	if a.idx == nil || a.idx.ambiguous[name] {
		return fmt.Errorf("generated types package has no unambiguous struct %q", name)
	}
	fields, ok := a.idx.structs[name]
	if !ok {
		return fmt.Errorf("generated types package has no struct %q", name)
	}

	tagged := 0
	for _, f := range fields {
		if f.json != "" {
			tagged++
		}
	}
	if tagged != len(def.Fields) {
		return fmt.Errorf("generated struct %q has %d json-tagged fields but the source class has %d",
			name, tagged, len(def.Fields))
	}

	for i := range def.Fields {
		src := def.Fields[i]
		var match *typesField
		for j := range fields {
			if fields[j].json != src.Canonical {
				continue
			}
			if match != nil {
				return fmt.Errorf("generated struct %q has more than one field tagged json:%q", name, src.Canonical)
			}
			match = &fields[j]
		}
		if match == nil {
			return fmt.Errorf("generated struct %q has no field tagged json:%q", name, src.Canonical)
		}
		if match.goName == "" || !ast.IsExported(match.goName) {
			return fmt.Errorf("generated struct %q field tagged json:%q is embedded or unexported", name, src.Canonical)
		}
		if err := a.auditValueType(&src.Type); err != nil {
			return fmt.Errorf("class %q field %q: %w", name, src.Canonical, err)
		}
		if !goTypeMatches(match.typ, src.Type, a.idx) {
			return fmt.Errorf("generated struct %q field %s (json:%q) does not carry the source field's type",
				name, match.goName, src.Canonical)
		}
	}
	return nil
}

// goTypeMatches reports whether a generated field's Go type expression is EXACTLY
// the carrier BAML v0.223's Go generator emits for a V3 value type. It is
// deliberately strict and syntactic: a named type must be the bare in-package
// identifier, a list must be an unbounded slice, and a scalar must be the exact
// Go scalar (int -> int64, float -> float64). Anything else is a mismatch, which
// omits the projector rather than assuming a conversion.
func goTypeMatches(expr ast.Expr, want promptdescriptor.ResolvedValueType, idx *typesIndex) bool {
	if want.Nullable {
		return false
	}
	switch want.Kind {
	case promptdescriptor.ValueString:
		return isIdentExpr(expr, "string")
	case promptdescriptor.ValueInt:
		return isIdentExpr(expr, "int64")
	case promptdescriptor.ValueFloat:
		return isIdentExpr(expr, "float64")
	case promptdescriptor.ValueBool:
		return isIdentExpr(expr, "bool")
	case promptdescriptor.ValueEnum:
		return isIdentExpr(expr, want.EnumName) && idx != nil && idx.stringEnums[want.EnumName]
	case promptdescriptor.ValueClass:
		if !isIdentExpr(expr, want.ClassName) || idx == nil {
			return false
		}
		_, ok := idx.structs[want.ClassName]
		return ok
	case promptdescriptor.ValueList:
		arr, ok := expr.(*ast.ArrayType)
		if !ok || arr.Len != nil || want.Elem == nil {
			return false
		}
		return goTypeMatches(arr.Elt, *want.Elem, idx)
	default:
		return false
	}
}

func isIdentExpr(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

func findEnum(u promptdescriptor.InputValueUniverse, name string) (promptdescriptor.ResolvedEnum, bool) {
	for _, e := range u.ProjectEnums {
		if e.Name == name {
			return e, true
		}
	}
	return promptdescriptor.ResolvedEnum{}, false
}

func findClass(u promptdescriptor.InputValueUniverse, name string) (promptdescriptor.ResolvedClass, bool) {
	for _, c := range u.Classes {
		if c.Name == name {
			return c, true
		}
	}
	return promptdescriptor.ResolvedClass{}, false
}

// ---------------------------------------------------------------------------
// Helper naming
// ---------------------------------------------------------------------------

// mangleValueType is the deterministic, collision-free key/suffix for one value
// type's emitted helper. BAML type names are Go identifiers, so concatenating
// them after a kind tag cannot collide across kinds ("EnumColor" vs
// "ClassColor") or across nesting depths ("ListOfEnumColor").
func mangleValueType(t promptdescriptor.ResolvedValueType) string {
	switch t.Kind {
	case promptdescriptor.ValueString:
		return "String"
	case promptdescriptor.ValueInt:
		return "Int"
	case promptdescriptor.ValueFloat:
		return "Float"
	case promptdescriptor.ValueBool:
		return "Bool"
	case promptdescriptor.ValueEnum:
		return "Enum" + t.EnumName
	case promptdescriptor.ValueClass:
		return "Class" + t.ClassName
	case promptdescriptor.ValueList:
		if t.Elem == nil {
			return "ListOfInvalid"
		}
		return "ListOf" + mangleValueType(*t.Elem)
	default:
		return "Unknown"
	}
}

// projectorHelperName is the emitted helper function name for a value type. The
// prefix is deliberately long and package-private so it cannot collide with a
// generated-client symbol re-exported by the introspected package.
func projectorHelperName(t promptdescriptor.ResolvedValueType) string {
	return "staticPromptValue" + mangleValueType(t)
}

// ---------------------------------------------------------------------------
// Emission
// ---------------------------------------------------------------------------

// projectorEmitter renders the projector registry, the accessor, and the value
// helpers into the generated introspected package.
type projectorEmitter struct {
	pd    string // promptdescriptor import path
	types string // generated client types package import path ("" when absent)
	idx   *typesIndex
}

// goTypeCode renders the generated Go carrier type for a value type.
func (p *projectorEmitter) goTypeCode(t promptdescriptor.ResolvedValueType) *jen.Statement {
	switch t.Kind {
	case promptdescriptor.ValueString:
		return jen.String()
	case promptdescriptor.ValueInt:
		return jen.Int64()
	case promptdescriptor.ValueFloat:
		return jen.Float64()
	case promptdescriptor.ValueBool:
		return jen.Bool()
	case promptdescriptor.ValueEnum:
		return jen.Qual(p.types, t.EnumName)
	case promptdescriptor.ValueClass:
		return jen.Qual(p.types, t.ClassName)
	case promptdescriptor.ValueList:
		return jen.Index().Add(p.goTypeCode(*t.Elem))
	default:
		// Unreachable: the audit rejects every other kind before emission.
		return jen.Any()
	}
}

// staticValueKind renders the promptdescriptor.StaticValueKind constant matching
// a value type.
func (p *projectorEmitter) staticValueKind(t promptdescriptor.ResolvedValueType) *jen.Statement {
	switch t.Kind {
	case promptdescriptor.ValueString:
		return jen.Qual(p.pd, "StaticString")
	case promptdescriptor.ValueInt:
		return jen.Qual(p.pd, "StaticInt")
	case promptdescriptor.ValueFloat:
		return jen.Qual(p.pd, "StaticFloat")
	case promptdescriptor.ValueBool:
		return jen.Qual(p.pd, "StaticBool")
	case promptdescriptor.ValueEnum:
		return jen.Qual(p.pd, "StaticEnum")
	case promptdescriptor.ValueClass:
		return jen.Qual(p.pd, "StaticClass")
	case promptdescriptor.ValueList:
		return jen.Qual(p.pd, "StaticList")
	default:
		return jen.Qual(p.pd, "StaticNull")
	}
}

// helperFunc emits one value-type helper: a pure function from the generated Go
// carrier to the neutral StaticValue. Every canonical BAML name it writes is a
// literal taken from the V3 source graph.
func (p *projectorEmitter) helperFunc(t promptdescriptor.ResolvedValueType, u promptdescriptor.InputValueUniverse) jen.Code {
	name := projectorHelperName(t)
	sig := jen.Func().Id(name).Params(jen.Id("v").Add(p.goTypeCode(t))).Qual(p.pd, "StaticValue")

	switch t.Kind {
	case promptdescriptor.ValueString:
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"):   p.staticValueKind(t),
			jen.Id("String"): jen.Id("v"),
		})))
	case promptdescriptor.ValueInt:
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"): p.staticValueKind(t),
			jen.Id("Int"):  jen.Id("v"),
		})))
	case promptdescriptor.ValueFloat:
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"):  p.staticValueKind(t),
			jen.Id("Float"): jen.Id("v"),
		})))
	case promptdescriptor.ValueBool:
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"): p.staticValueKind(t),
			jen.Id("Bool"): jen.Id("v"),
		})))
	case promptdescriptor.ValueEnum:
		// `string(v)` is the CANDIDATE canonical member. The generated enum is a
		// `type X string` whose constants are the canonical variant names, so this
		// reads the source-canonical identity — but the binder still checks it
		// against the V3 member list, so an out-of-range value declines.
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"):      p.staticValueKind(t),
			jen.Id("TypeName"):  jen.Lit(t.EnumName),
			jen.Id("Canonical"): jen.String().Call(jen.Id("v")),
		})))
	case promptdescriptor.ValueClass:
		def, _ := findClass(u, t.ClassName)
		fields := make([]jen.Code, 0, len(def.Fields))
		for _, f := range def.Fields {
			sel := p.selectorFor(t.ClassName, f.Canonical)
			fields = append(fields, jen.Qual(p.pd, "StaticFieldValue").Values(jen.Dict{
				jen.Id("Canonical"): jen.Lit(f.Canonical),
				jen.Id("Value"):     jen.Id(projectorHelperName(f.Type)).Call(jen.Id("v").Dot(sel)),
			}))
		}
		return sig.Block(jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
			jen.Id("Kind"):     p.staticValueKind(t),
			jen.Id("TypeName"): jen.Lit(t.ClassName),
			jen.Id("Fields"):   jen.Index().Qual(p.pd, "StaticFieldValue").Values(fields...),
		})))
	case promptdescriptor.ValueList:
		// Ranged by INDEX so input order is preserved verbatim.
		return sig.Block(
			jen.Id("items").Op(":=").Make(jen.Index().Qual(p.pd, "StaticValue"), jen.Lit(0), jen.Len(jen.Id("v"))),
			jen.For(jen.Id("i").Op(":=").Range().Id("v")).Block(
				jen.Id("items").Op("=").Append(jen.Id("items"),
					jen.Id(projectorHelperName(*t.Elem)).Call(jen.Id("v").Index(jen.Id("i")))),
			),
			jen.Return(jen.Qual(p.pd, "StaticValue").Values(jen.Dict{
				jen.Id("Kind"):  p.staticValueKind(t),
				jen.Id("Items"): jen.Id("items"),
			})),
		)
	default:
		// Unreachable: the audit rejects every other kind before emission.
		return jen.Null()
	}
}

// selectorFor returns the audited Go field selector carrying one canonical BAML
// field. The audit already proved it exists and is unique, so a miss here would
// be an internal inconsistency; returning the canonical name would emit
// non-compiling code, which is the correct loud failure.
func (p *projectorEmitter) selectorFor(class, canonical string) string {
	if p.idx == nil {
		return canonical
	}
	for _, f := range p.idx.structs[class] {
		if f.json == canonical {
			return f.goName
		}
	}
	return canonical
}

// emitStaticPromptArgumentProjectors writes the per-method projector registry,
// its decline ledger, the shared value helpers, and the accessor.
//
// descriptors is the V3 descriptor set (a method with no descriptor gets no
// projector by construction). idx is the audited generated `types` package, or
// nil when the layout has none (the stub, or a client with no class/enum types)
// — in which case only scalar-argument methods can be projected.
func emitStaticPromptArgumentProjectors(
	out *jen.File,
	cliCfg *config,
	descriptors map[string]promptdescriptor.Function,
	idx *typesIndex,
) {
	pd := cliCfg.promptDescriptorPkg()
	typesPkg := ""
	if idx != nil {
		typesPkg = idx.pkgPath
	}
	p := &projectorEmitter{pd: pd, types: typesPkg, idx: idx}

	methods := make([]string, 0, len(descriptors))
	for m := range descriptors {
		methods = append(methods, m)
	}
	sort.Strings(methods)

	type projected struct {
		method string
		fn     promptdescriptor.Function
		audit  *projectorAudit
	}
	var ok []projected
	declines := map[string]string{}
	// helpers accumulates every value type needing an emitted helper across ALL
	// admitted methods, deduplicated by mangled name so it is emitted once.
	helpers := map[string]promptdescriptor.ResolvedValueType{}
	// helperUniverse remembers, per class helper, the universe that defined it.
	// Every descriptor in one project shares the same source classes, so the
	// first definition wins and a disagreement is impossible by construction.
	helperUniverse := map[string]promptdescriptor.InputValueUniverse{}

	for _, m := range methods {
		fn := descriptors[m]
		a := newProjectorAudit(idx, fn.InputValues)
		var reason string
		for _, arg := range fn.Args {
			if err := a.auditValueType(arg.ValueType); err != nil {
				reason = fmt.Sprintf("argument %q: %v", arg.Name, err)
				break
			}
		}
		if reason != "" {
			declines[m] = reason
			continue
		}
		ok = append(ok, projected{method: m, fn: fn, audit: a})
		for key, vt := range a.need {
			if _, dup := helpers[key]; !dup {
				helpers[key] = vt
				helperUniverse[key] = fn.InputValues
			}
		}
	}

	// --- value helpers (deterministic order) ---
	helperKeys := make([]string, 0, len(helpers))
	for k := range helpers {
		helperKeys = append(helperKeys, k)
	}
	sort.Strings(helperKeys)
	if len(helperKeys) > 0 {
		out.Comment("staticPromptValue* are the de-BAML Slice 7.1b generated value projectors: pure,")
		out.Comment("reflection-free functions from a generated client Go carrier to the neutral")
		out.Comment("promptdescriptor.StaticValue tree. Every canonical BAML enum member / class field")
		out.Comment("name they write is a LITERAL taken from the V3 source graph, and every Go field")
		out.Comment("selector was proven by a build-time AST audit of the generated types package.")
		out.Comment("SENSITIVE: they carry real request arguments; never log or format their input.")
	}
	for _, k := range helperKeys {
		out.Add(p.helperFunc(helpers[k], helperUniverse[k]))
	}

	// --- registry ---
	projectorType := jen.Func().Params(jen.Index().Any()).Params(jen.Index().Qual(pd, "ArgumentValue"), jen.Bool())
	entries := make([]jen.Code, 0, len(ok))
	for _, pr := range ok {
		entries = append(entries, jen.Lit(pr.method).Op(":").Add(p.projectorFunc(pr.fn)))
	}
	out.Comment("StaticPromptArgumentProjectors maps a BAML method name to its generated argument")
	out.Comment("projector: an exact-type-assertion, direct-field-selector function from the ordered")
	out.Comment("[]any the generated adapter emits to the neutral ordered promptdescriptor.ArgumentValue")
	out.Comment("vector the native static binder consumes (de-BAML Slice 7.1b). It performs NO runtime")
	out.Comment("reflection, JSON marshalling, Encode() call, struct-tag read, or map iteration, and it")
	out.Comment("returns ok=false — never a partial vector — on any arity or type mismatch.")
	out.Var().Id("StaticPromptArgumentProjectors").Op("=").Map(jen.String()).Add(projectorType).Values(entries...)

	out.Comment("StaticPromptProjectorDeclines maps a BAML method name that HAS a V3 descriptor to the")
	out.Comment("stable build-time reason no argument projector could be generated for it (a missing,")
	out.Comment("ambiguous, or mismatched generated Go selector, or a value shape this slice does not")
	out.Comment("project). Such a method has no native static claim: the seam declines pre-render and")
	out.Comment("BAML serves it. Every entry is a #583 teardown blocker, not an accepted fallback.")
	declKeys := make([]string, 0, len(declines))
	for m := range declines {
		declKeys = append(declKeys, m)
	}
	sort.Strings(declKeys)
	declEntries := make([]jen.Code, 0, len(declKeys))
	for _, m := range declKeys {
		declEntries = append(declEntries, jen.Lit(m).Op(":").Lit(declines[m]))
	}
	out.Var().Id("StaticPromptProjectorDeclines").Op("=").Map(jen.String()).String().Values(declEntries...)

	out.Comment("StaticPromptArgumentValues projects one call's ordered, already-typed arguments into")
	out.Comment("the neutral ArgumentValue vector for method, returning ok=false when the method has no")
	out.Comment("generated projector or the supplied arguments do not match its exact Go types. A false")
	// A plain interpreted string: the text carries double quotes, and a RAW literal
	// would write the backslashes verbatim into every generated file.
	out.Comment("result means \"no native static value binding; stay on the BAML path\" — it NEVER falls")
	out.Comment("back to reflection or to the raw argument map.")
	out.Func().Id("StaticPromptArgumentValues").
		Params(jen.Id("method").String(), jen.Id("args").Index().Any()).
		Params(jen.Index().Qual(pd, "ArgumentValue"), jen.Bool()).
		Block(
			jen.List(jen.Id("project"), jen.Id("ok")).Op(":=").Id("StaticPromptArgumentProjectors").Index(jen.Id("method")),
			jen.If(jen.Op("!").Id("ok")).Block(jen.Return(jen.Nil(), jen.False())),
			jen.Return(jen.Id("project").Call(jen.Id("args"))),
		)
}

// projectorFunc emits one method's projector literal.
func (p *projectorEmitter) projectorFunc(fn promptdescriptor.Function) jen.Code {
	body := []jen.Code{
		// Exact arity. A mismatch is a generated-adapter/descriptor disagreement,
		// which must decline rather than bind a shifted vector.
		jen.If(jen.Len(jen.Id("args")).Op("!=").Lit(len(fn.Args))).Block(
			jen.Return(jen.Nil(), jen.False()),
		),
	}
	if len(fn.Args) == 0 {
		// A no-argument function still has a projector: its neutral vector is
		// empty, which is a real (and admissible) binding, not an absence.
		body = append(body, jen.Return(jen.Nil(), jen.True()))
		return jen.Func().Params(jen.Id("args").Index().Any()).
			Params(jen.Index().Qual(p.pd, "ArgumentValue"), jen.Bool()).Block(body...)
	}

	values := make([]jen.Code, 0, len(fn.Args))
	for i, arg := range fn.Args {
		local := fmt.Sprintf("a%d", i)
		okID := fmt.Sprintf("ok%d", i)
		body = append(body,
			jen.List(jen.Id(local), jen.Id(okID)).Op(":=").
				Id("args").Index(jen.Lit(i)).Assert(p.goTypeCode(*arg.ValueType)),
			jen.If(jen.Op("!").Id(okID)).Block(jen.Return(jen.Nil(), jen.False())),
		)
		values = append(values, jen.Qual(p.pd, "ArgumentValue").Values(jen.Dict{
			jen.Id("Name"):  jen.Lit(arg.Name),
			jen.Id("Value"): jen.Id(projectorHelperName(*arg.ValueType)).Call(jen.Id(local)),
		}))
	}
	body = append(body, jen.Return(
		jen.Index().Qual(p.pd, "ArgumentValue").Values(values...),
		jen.True(),
	))
	return jen.Func().Params(jen.Id("args").Index().Any()).
		Params(jen.Index().Qual(p.pd, "ArgumentValue"), jen.Bool()).Block(body...)
}
