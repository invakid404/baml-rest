package codegen

// nativespine.go is the M1 native codegen entry point (codegen-spine scope §4,
// deliverable 3). It consumes a neutral projectdescriptor.Method — the artifact
// cmd/introspect --native-spine-descriptors emits — and emits pure-Go source for
// one static unary method: a typed input carrier and output carrier(s) (D9:
// types-compatible names, NO CFFI codecs), a neutral injected Executor interface,
// one bamlutils.StreamingMethod + ParseMethod registration whose closures admit
// only unary final-call (StreamModeCall) and decline every other mode, and a
// finalResult/errorResult carrier. It NEVER references BamlPkg/GeneratedClientPkg,
// so the emitted code's import graph is bamlutils-only — no baml_client, no BAML
// runtime, no CFFI.
//
// Two faithfulness properties the M1 review required:
//
//   - Output carriers serialize each field under its CANONICAL BAML key via a
//     pure-Go custom JSON codec (nativeSpineMarshalObject/UnmarshalObject).
//     BAML serves — and a generated Go struct tags — the canonical field name
//     and canonical enum value, never an @alias (empirically confirmed, M3b
//     scope §2); the alias is ingress-only metadata and is never an output
//     token. The custom codec is retained over a struct tag because a canonical
//     name can still be a string a struct tag cannot express ("-", "", a comma),
//     and to keep HTML escaping off to match the serving path.
//   - Emitted output type names are namespaced with an "Output" prefix so they
//     can never collide with the fixed generated declarations (Executor,
//     MethodName, BuildMethod, ErrUnsupportedStreamMode, <Method>Input).
//
// M3b extends the carrier vocabulary with multi-arm unions (a discriminated
// value carrier per union — see nativespine_union.go), string/int/bool literals
// (a standalone literal lowers to its plain Go base; a literal union arm carries
// its identity via a no-argument constructor), and @alias metadata (admitted and
// ignored for output token selection).
//
// It is separate from the reflection-driven adapter codegen in this package: it
// learns the method from the descriptor artifact, never from a reflected
// generated client (D10). Emission is deterministic (fixed slice order +
// go/format), so its output is golden-testable.

import (
	"fmt"
	"go/format"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/stoewer/go-strcase"
)

// NativeSpineOptions parameterizes a native-spine emission.
type NativeSpineOptions struct {
	// PackageName is the Go package the emitted file declares.
	PackageName string
	// BamlutilsPkg is the import path of the bamlutils package the emitted code
	// references. Defaults to the canonical path when empty.
	BamlutilsPkg string
}

const defaultBamlutilsPkg = "github.com/invakid404/baml-rest/bamlutils"

// outputTypeNamePrefix namespaces every emitted output class/enum type so it
// cannot collide with the fixed generated declarations. See package doc.
const outputTypeNamePrefix = "Output"

// outputTypeName is the collision-safe Go type name for an emitted output
// class/enum.
func outputTypeName(raw string) string {
	return outputTypeNamePrefix + strcase.UpperCamelCase(raw)
}

// reservedPackageIdents are the fixed package-level Go identifiers every emitted
// file declares (types/vars/funcs). An emitted output type, enum constant, or the
// input carrier that normalizes onto one of these would redeclare it. The codec
// helpers are included because they are emitted whenever an output class exists.
var reservedPackageIdents = []string{
	"MethodName", "Executor", "BuildMethod", "ErrUnsupportedStreamMode",
	"finalResult", "errorResult",
	"nativeSpineField", "nativeSpineMarshalObject", "nativeSpineUnmarshalObject",
}

// CheckNativeNameCollision reports an error if the FULL set of package-level Go
// identifiers EmitNativeStaticUnary would emit for this method is not unique after
// the lossy strcase.UpperCamelCase normalization. strcase is not injective, and
// go/format accepts duplicate declarations, so this fail-closed check is the only
// backstop against an uncompilable carrier. The set covers: the input carrier
// type (<Method>Input), every output class/enum type, every enum CONSTANT, the
// fixed package-level declarations, per-struct field identifiers (input args and
// each class's fields). It is called by both the emitter (backstop) and the
// classifier (so it declines exactly what the emitter cannot emit) — one source
// of truth, so the two can never drift.
func CheckNativeNameCollision(methodName string, argNames []string, ret schemadescriptor.Bundle) error {
	owner := map[string]string{}
	claim := func(id, what string) error {
		if prev, ok := owner[id]; ok {
			return fmt.Errorf("emitted Go identifier %q collides: %s and %s", id, prev, what)
		}
		owner[id] = what
		return nil
	}
	for _, r := range reservedPackageIdents {
		owner[r] = "fixed declaration " + r
	}
	if err := claim(strcase.UpperCamelCase(methodName+"Input"), "input carrier"); err != nil {
		return err
	}
	for _, c := range ret.Classes {
		if err := claim(outputTypeName(c.Name.Name), "output class "+c.Name.Name); err != nil {
			return err
		}
		// emitClassCodec declares MarshalJSON and UnmarshalJSON methods on every
		// output class, and Go forbids a field and method with the same name — so
		// reserve those identifiers in the per-class set alongside the fields.
		fields := map[string]string{"MarshalJSON": "generated codec method", "UnmarshalJSON": "generated codec method"}
		// The codec keys its map[string]any / marshals each field by its CANONICAL
		// name (field.Name.Name), never an @alias (aliases are ignored on output).
		// BAML forbids two fields of a class sharing a canonical name, but the
		// direct emitter accepts synthetic bundles, so reject a duplicate canonical
		// key fail-closed (it would emit a duplicate map-literal key — a Go compile
		// error — and a doubled JSON key).
		keys := map[string]bool{}
		for _, f := range c.Fields {
			fg := strcase.UpperCamelCase(f.Name.Name)
			if what, ok := fields[fg]; ok {
				return fmt.Errorf("field %q of class %q normalizes to Go identifier %q, colliding with %s", f.Name.Name, c.Name.Name, fg, what)
			}
			fields[fg] = "field " + f.Name.Name
			k := f.Name.Name
			if keys[k] {
				return fmt.Errorf("fields of class %q share the canonical key %q", c.Name.Name, k)
			}
			keys[k] = true
		}
	}
	for _, e := range ret.Enums {
		enumType := outputTypeName(e.Name.Name)
		if err := claim(enumType, "output enum "+e.Name.Name); err != nil {
			return err
		}
		for _, v := range e.Values {
			if err := claim(enumType+strcase.UpperCamelCase(v.Name.Name), "enum constant of "+e.Name.Name); err != nil {
				return err
			}
		}
	}
	// Planned multi-arm union carriers add package-level identifiers: the carrier
	// type (OutputUnionN) and its per-arm constructors (OutputUnionNNewVariantK).
	// Claiming them here keeps admission and emission on ONE plan, so a class
	// named e.g. "Union1" (→ OutputUnion1) is declined as a collision rather than
	// emitted as a duplicate declaration. A malformed union (unplannable) is left
	// to classifyOutputSchema / the emitter's own buildCarrierPlan to reject with
	// its shape code, so a plan-build error here is not turned into a collision.
	if plan, perr := buildCarrierPlan(ret); perr == nil {
		for _, u := range plan.unions {
			if err := claim(u.name, "output union carrier"); err != nil {
				return err
			}
			for i := range u.variants {
				if err := claim(fmt.Sprintf("%sNewVariant%d", u.name, i), "union constructor of "+u.name); err != nil {
					return err
				}
			}
		}
	}
	// M3c: structural recursive alias types (`type Output<Name> = ...`) join the
	// package identifier universe. They normalize with the SAME outputTypeName
	// formula as classes/enums, so an alias and a class sharing a source name (which
	// BAML forbids but the direct emitter accepts) collide here rather than emitting
	// a duplicate Go declaration.
	for i := range ret.StructuralRecursiveAliases {
		name := ret.StructuralRecursiveAliases[i].Name
		if err := claim(outputTypeName(name), "output recursive alias "+name); err != nil {
			return err
		}
	}
	inputFields := map[string]bool{}
	for _, n := range argNames {
		g := strcase.UpperCamelCase(n)
		if inputFields[g] {
			return fmt.Errorf("input arguments normalize to the same Go field %q", g)
		}
		inputFields[g] = true
	}
	return nil
}

// argNamesOf projects the argument names for the collision check.
func argNamesOf(args []projectdescriptor.Argument) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a.Name
	}
	return out
}

// EmitNativeStaticUnary emits the Go source for one ClassStaticUnary method.
// It returns gofmt'd bytes; the caller writes or golden-compares them. It errors
// if the method is not ClassStaticUnary or carries a type outside the M1 profile
// (the classifier should have declined such methods upstream — this is a
// fail-closed backstop, never a silent weakening).
func EmitNativeStaticUnary(m projectdescriptor.Method, opts NativeSpineOptions) ([]byte, error) {
	if m.Class != projectdescriptor.ClassStaticUnary {
		return nil, fmt.Errorf("codegen: EmitNativeStaticUnary: method %q has class %q, want %q", m.Name, m.Class, projectdescriptor.ClassStaticUnary)
	}
	if opts.PackageName == "" {
		return nil, fmt.Errorf("codegen: EmitNativeStaticUnary: PackageName is required")
	}
	bamlutilsPkg := opts.BamlutilsPkg
	if bamlutilsPkg == "" {
		bamlutilsPkg = defaultBamlutilsPkg
	}

	if err := CheckNativeNameCollision(m.Name, argNamesOf(m.Args), m.Return); err != nil {
		return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
	}
	// The SHARED emitter-feasibility gate: reference resolution, union planning,
	// every-type-lowers, and the direct-by-value class-SCC decline. The classifier
	// calls the SAME impl (mapping it to unsupported_output_shape), so a shape that
	// reaches here uncaught is a bypassed preflight, not a silent weakening — fail
	// closed. Name collisions are handled separately above (name_collision).
	if err := CheckNativeCarrierShape(m.Return); err != nil {
		return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
	}

	// Plan the reachable multi-arm unions before writing any declaration (the same
	// plan CheckNativeCarrierShape validated).
	plan, err := buildCarrierPlan(m.Return)
	if err != nil {
		return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
	}

	inputStruct := strcase.UpperCamelCase(m.Name + "Input")

	outputBase, err := schemaGoType(m.Return.Target, plan)
	if err != nil {
		return nil, fmt.Errorf("codegen: method %q: output %w", m.Name, err)
	}

	hasClasses := len(m.Return.Classes) > 0
	hasEnums := len(m.Return.Enums) > 0
	hasUnions := len(plan.unions) > 0
	// guardCycles gates the recursion-safe marshal guard. It is DERIVED
	// structurally from the validated lowered graph (carrierGraphIsRecursive) — NOT
	// from the descriptor's RecursiveClasses/StructuralRecursiveAliases metadata,
	// which the emitter must not trust: a truly-recursive carrier whose metadata was
	// missing would otherwise emit a custom codec with no guard (a user pointer
	// cycle → stack overflow), and stray metadata on an acyclic bundle would inject
	// the guard and break the byte-unchanged M3a/M3b invariant. A cycle in the Go
	// carrier graph implies a class or union carrier, so this subsumes hasClasses/
	// hasUnions; a pure-container `any` alias is correctly NOT recursive.
	guardCycles := carrierGraphIsRecursive(m.Return, plan)

	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by adapters/common/codegen EmitNativeStaticUnary; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", opts.PackageName)

	// Imports: fmt + bamlutils + bamlutils/promptdescriptor always. bamlutils carries
	// the neutral ExecBridge-U1 executor/binding contract + the strict static-final
	// decoder; promptdescriptor carries the projected ArgumentValue vector the emitted
	// input projector produces. bytes when an output class is emitted (its codec
	// assembles bytes by hand). encoding/json when a class, an enum, OR a union carrier
	// is emitted: the class codec encodes each field with it (HTML escaping off, to
	// match the serving serializer), the validating enum codec marshals/unmarshals its
	// string value with it, and the union carrier codec marshals/unmarshals the
	// selected arm with it (BAML generated-carrier parity). reflect when the
	// recursion-safe marshal guard is emitted. The emitted graph names ONLY
	// context/bamlutils/bamlutils/promptdescriptor/stdlib — never nativeserve or
	// internal/*, and never generated BAML or CFFI (ExecBridge-U1 §3).
	b.WriteString("import (\n")
	if hasClasses {
		fmt.Fprintf(&b, "\t%q\n", "bytes")
	}
	if hasClasses || hasEnums || hasUnions {
		fmt.Fprintf(&b, "\t%q\n", "encoding/json")
	}
	fmt.Fprintf(&b, "\t%q\n", "fmt")
	if guardCycles {
		fmt.Fprintf(&b, "\t%q\n", "reflect")
	}
	fmt.Fprintf(&b, "\n\t%q\n\t%q\n)\n\n", bamlutilsPkg, bamlutilsPkg+"/promptdescriptor")

	fmt.Fprintf(&b, "// MethodName is the canonical BAML method this file serves.\n")
	fmt.Fprintf(&b, "const MethodName = %q\n\n", m.Name)

	// Input carrier. Argument names are BAML identifiers, so a struct json tag is
	// faithful here (unlike arbitrary output field aliases).
	fmt.Fprintf(&b, "// %s is the typed input carrier (no CFFI codecs).\n", inputStruct)
	fmt.Fprintf(&b, "type %s struct {\n", inputStruct)
	for _, a := range m.Args {
		gt, err := valueGoType(a.Type)
		if err != nil {
			return nil, fmt.Errorf("codegen: method %q: input arg %q %w", m.Name, a.Name, err)
		}
		fmt.Fprintf(&b, "\t%s %s `json:%q`\n", strcase.UpperCamelCase(a.Name), gt, a.Name)
	}
	fmt.Fprintf(&b, "}\n\n")

	// Output enums, then classes (deterministic descriptor order).
	for _, e := range m.Return.Enums {
		name := outputTypeName(e.Name.Name)
		fmt.Fprintf(&b, "// %s is a generated output enum carrier.\n", name)
		fmt.Fprintf(&b, "type %s string\n\n", name)
		if len(e.Values) > 0 {
			fmt.Fprintf(&b, "const (\n")
			for _, v := range e.Values {
				// CANONICAL enum value (v.Name.Name), never the alias: BAML serves
				// canonical enum members and a generated enum validates against them
				// (empirically confirmed, scope §2). An @alias is metadata only.
				fmt.Fprintf(&b, "\t%s%s %s = %q\n", name, strcase.UpperCamelCase(v.Name.Name), name, v.Name.Name)
			}
			fmt.Fprintf(&b, ")\n\n")
		}
		emitEnumCodec(&b, name, e.Values)
	}
	for _, c := range m.Return.Classes {
		name := outputTypeName(c.Name.Name)
		fmt.Fprintf(&b, "// %s is a generated output carrier.\n", name)
		fmt.Fprintf(&b, "type %s struct {\n", name)
		fields := make([]string, 0, len(c.Fields))
		for _, f := range c.Fields {
			gt, err := schemaGoType(f.Type, plan)
			if err != nil {
				return nil, fmt.Errorf("codegen: method %q: class %s field %q %w", m.Name, c.Name.Name, f.Name.Name, err)
			}
			goField := strcase.UpperCamelCase(f.Name.Name)
			fields = append(fields, goField)
			// No json struct tag: the custom codec below emits the canonical key.
			fmt.Fprintf(&b, "\t%s %s\n", goField, gt)
		}
		fmt.Fprintf(&b, "}\n\n")
		emitClassCodec(&b, name, c.Fields, fields, guardCycles)
	}

	// M3c: structural recursive alias declarations, in descriptor order. Each is a
	// Go ALIAS (`type Output<Name> = <lowered target>`), reproducing BAML v0.223's
	// generated type_aliases.go — including the pure-container `any` fallback. The
	// alias may forward-reference a union carrier emitted below; Go resolves
	// package-level type declarations regardless of order.
	for i := range m.Return.StructuralRecursiveAliases {
		a := m.Return.StructuralRecursiveAliases[i]
		lowered, err := lowerRecursiveAliasDecl(a, plan)
		if err != nil {
			return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
		}
		name := outputTypeName(a.Name)
		fmt.Fprintf(&b, "// %s is a generated structural recursive alias carrier.\n", name)
		fmt.Fprintf(&b, "type %s = %s\n\n", name, lowered)
	}

	// Output union carriers, in first-reach plan order (OutputUnion1, ...). A
	// class field / list element / map value / nested-union arm / recursive-alias
	// target above binds to these by name; Go resolves the forward reference at
	// package level.
	for _, u := range plan.unions {
		if err := emitUnionCarrier(&b, u, plan, guardCycles); err != nil {
			return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
		}
	}

	// Neutral executor seam + boilerplate + registration (+ object-codec helpers
	// when any output class was emitted, + the recursion-safe marshal guard when a
	// recursive carrier was emitted).
	b.WriteString(nativeSpineBoilerplate(inputStruct, outputBase, hasClasses, guardCycles))

	// ExecBridge-U1: the neutral per-method registration — the reflection-free scalar
	// input projector, the strict static-final decoder, and the Binding() that pairs
	// them with MethodName for the production runtime. Emitted alongside the carriers
	// so the module names ONLY context/bamlutils/bamlutils/promptdescriptor/stdlib.
	if err := emitBinding(&b, m, inputStruct, outputBase, staticFinalDecoderName(m.Return)); err != nil {
		return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("codegen: method %q: gofmt emitted source: %w\n---\n%s", m.Name, err, b.String())
	}
	return src, nil
}

// emitClassCodec emits MarshalJSON/UnmarshalJSON for one output class that map
// each Go field to its CANONICAL BAML key (field.Name.Name). BAML serves — and a
// generated Go struct tags — the canonical field name, never an @alias
// (empirically confirmed, scope §2); the alias is ingress-only metadata and is
// never an output token. The custom codec is retained (over a struct tag)
// because a canonical name can still be a string a struct tag cannot express
// ("-", "", a comma), and to keep HTML escaping off to match the serving path.
func emitClassCodec(b *strings.Builder, typeName string, schemaFields []schemadescriptor.ClassField, goFields []string, guardCycles bool) {
	fmt.Fprintf(b, "// MarshalJSON emits %s with each field under its canonical BAML key.\n", typeName)
	fmt.Fprintf(b, "func (v %s) MarshalJSON() ([]byte, error) {\n", typeName)
	if guardCycles {
		// Recursion-safe: a user-built pointer cycle errors here instead of
		// recursing until the stack overflows (the custom codec bypasses the
		// ordinary encoder's cycle tracking). Finite values pass untouched.
		fmt.Fprintf(b, "\tif err := nativeSpineCheckAcyclic(v); err != nil {\n\t\treturn nil, err\n\t}\n")
	}
	fmt.Fprintf(b, "\treturn nativeSpineMarshalObject([]nativeSpineField{\n")
	for i, f := range schemaFields {
		fmt.Fprintf(b, "\t\t{%q, v.%s},\n", f.Name.Name, goFields[i])
	}
	fmt.Fprintf(b, "\t})\n}\n\n")

	fmt.Fprintf(b, "// UnmarshalJSON reads %s from each field's canonical BAML key.\n", typeName)
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error {\n", typeName)
	fmt.Fprintf(b, "\treturn nativeSpineUnmarshalObject(data, map[string]any{\n")
	for i, f := range schemaFields {
		fmt.Fprintf(b, "\t\t%q: &v.%s,\n", f.Name.Name, goFields[i])
	}
	fmt.Fprintf(b, "\t})\n}\n\n")
}

// emitEnumCodec emits IsValid/MarshalJSON/UnmarshalJSON for one output enum,
// mirroring a baml_client-generated enum carrier: both directions REJECT any
// value outside the declared members (see the checked-in generated enum at
// internal/nativeprompt/testdata/staticserve_fixture/baml_client/types/enums.go).
// A plain `type E string` without these methods would silently accept an
// out-of-range value that BAML's served carrier rejects; the differential pins
// this parity with negative cases.
func emitEnumCodec(b *strings.Builder, typeName string, values []schemadescriptor.EnumValue) {
	fmt.Fprintf(b, "// IsValid reports whether e is one of %s's declared members.\n", typeName)
	if len(values) == 0 {
		fmt.Fprintf(b, "func (e %s) IsValid() bool { return false }\n\n", typeName)
	} else {
		fmt.Fprintf(b, "func (e %s) IsValid() bool {\n\tswitch e {\n\tcase ", typeName)
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%s%s", typeName, strcase.UpperCamelCase(v.Name.Name))
		}
		b.WriteString(":\n\t\treturn true\n\t}\n\treturn false\n}\n\n")
	}

	// MarshalJSON mirrors the generated enum exactly: validate, then json.Marshal
	// the raw string. Enum members are HTML-free identifiers, so encoding/json's
	// escaping is a no-op here and the bytes match the sonic-served carrier.
	fmt.Fprintf(b, "// MarshalJSON emits %s as its string value, rejecting out-of-range values (BAML parity).\n", typeName)
	fmt.Fprintf(b, "func (e %s) MarshalJSON() ([]byte, error) {\n", typeName)
	fmt.Fprintf(b, "\tif !e.IsValid() {\n\t\treturn nil, fmt.Errorf(%q, string(e))\n\t}\n", "invalid "+typeName+": %q")
	fmt.Fprintf(b, "\treturn json.Marshal(string(e))\n}\n\n")

	fmt.Fprintf(b, "// UnmarshalJSON reads %s, rejecting values outside its declared members (BAML parity).\n", typeName)
	fmt.Fprintf(b, "func (e *%s) UnmarshalJSON(data []byte) error {\n", typeName)
	fmt.Fprintf(b, "\tvar s string\n\tif err := json.Unmarshal(data, &s); err != nil {\n\t\treturn err\n\t}\n")
	fmt.Fprintf(b, "\t*e = %s(s)\n\tif !e.IsValid() {\n\t\treturn fmt.Errorf(%q, s)\n\t}\n\treturn nil\n}\n\n", typeName, "invalid "+typeName+": %q")
}

// nativeSpineBoilerplate is the method-independent tail: the Executor interface,
// the stable stream-mode decline, the finalResult/errorResult carriers, the
// StreamingMethod/ParseMethod builder, (when withCodec) the pure-Go object-codec
// helpers, and (when guardCycles) the recursion-safe marshal guard. inputStruct
// is the typed input carrier; outputBase is the Go type the output carriers
// construct.
func nativeSpineBoilerplate(inputStruct, outputBase string, withCodec, guardCycles bool) string {
	guard := ""
	if guardCycles {
		guard = nativeSpineCycleGuardSource
	}
	codec := ""
	if withCodec {
		codec = `
// nativeSpineField is one output field: its exact BAML wire key and value.
type nativeSpineField struct {
	key string
	val any
}

// nativeSpineEncode marshals v with HTML escaping DISABLED, matching the serving
// serializer (worker serves final results via sonic.Marshal == sonic ConfigDefault,
// whose EscapeHTML is false). encoding/json's package default escapes '<', '>', '&'
// to </>/&, which would make a string field like "<x>" diverge from
// the bytes BAML actually serves. Go maps still emit in sorted-key order here
// (encoding/json sorts), which is the native lane's documented canonical order.
func nativeSpineEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a trailing newline; the codec assembles the object
	// by hand, so strip it.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// nativeSpineMarshalObject renders fields as a JSON object in declaration order,
// writing each field under its exact wire key — including keys Go struct tags
// cannot express ("-", "", names with commas). Every key and value is encoded
// with nativeSpineEncode (HTML escaping off) so the bytes match the serving path.
func nativeSpineMarshalObject(fields []nativeSpineField) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := nativeSpineEncode(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := nativeSpineEncode(f.val)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// nativeSpineUnmarshalObject reads each destination pointer from its exact wire
// key. A missing key leaves the destination at its zero value.
func nativeSpineUnmarshalObject(data []byte, dst map[string]any) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key, ptr := range dst {
		rm, ok := raw[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(rm, ptr); err != nil {
			return err
		}
	}
	return nil
}
`
	}
	return `// ErrUnsupportedStreamMode is the stable decline returned when a mode other than
// unary final-call (StreamModeCall) is requested. M1 admits only unary call.
var ErrUnsupportedStreamMode = fmt.Errorf("nativespine: method %q supports only unary final-call (StreamModeCall)", MethodName)

// finalResult is a minimal bamlutils.StreamResult carrying one final value; the
// worker stream bridge marshals Final() into the output envelope.
type finalResult struct{ final any }

func (r finalResult) Kind() bamlutils.StreamResultKind { return bamlutils.StreamResultKindFinal }
func (r finalResult) Stream() any                      { return nil }
func (r finalResult) Final() any                       { return r.final }
func (r finalResult) Error() error                     { return nil }
func (r finalResult) Raw() string                      { return "" }
func (r finalResult) Reasoning() string                { return "" }
func (r finalResult) Reset() bool                      { return false }
func (r finalResult) Metadata() *bamlutils.Metadata    { return nil }
func (r finalResult) Release()                         {}

// errorResult is a minimal bamlutils.StreamResult carrying an executor error; the
// worker stream bridge classifies Error() into the error envelope.
type errorResult struct{ err error }

func (r errorResult) Kind() bamlutils.StreamResultKind { return bamlutils.StreamResultKindError }
func (r errorResult) Stream() any                      { return nil }
func (r errorResult) Final() any                       { return nil }
func (r errorResult) Error() error                     { return r.err }
func (r errorResult) Raw() string                      { return "" }
func (r errorResult) Reasoning() string                { return "" }
func (r errorResult) Reset() bool                      { return false }
func (r errorResult) Metadata() *bamlutils.Metadata    { return nil }
func (r errorResult) Release()                         {}

// BuildMethod returns the StreamingMethod and ParseMethod registrations for
// MethodName, driven by exec (a neutral bamlutils.NativeSpineUnaryExecutor). The
// StreamingMethod admits unary call mode only and declines every other mode with
// ErrUnsupportedStreamMode; neither closure makes a BAML request or parse call. The
// adapter (a bamlutils.Adapter embeds context.Context) is passed as the executor
// call context so a cancelled request is observed inside the executor. A succeeded
// result is delivered as one final frame; a pre-socket decline or a terminal
// failed-after-claim result as one error frame carrying its typed error — both flow
// through the existing worker stream bridge and envelope. The module never falls
// back: an outer composite executor (if any) intercepts a matched pre-socket decline
// BEFORE this closure sees it, so this module knows nothing about any oracle.
func BuildMethod(exec bamlutils.NativeSpineUnaryExecutor) (bamlutils.StreamingMethod, bamlutils.ParseMethod) {
	sm := bamlutils.StreamingMethod{
		MakeInput:        func() any { return new(` + inputStruct + `) },
		MakeOutput:       func() any { return new(` + outputBase + `) },
		MakeStreamOutput: func() any { return new(` + outputBase + `) },
		Impl: func(adapter bamlutils.Adapter, input any) (<-chan bamlutils.StreamResult, error) {
			if adapter.StreamMode() != bamlutils.StreamModeCall {
				return nil, fmt.Errorf("%w (got mode %d)", ErrUnsupportedStreamMode, adapter.StreamMode())
			}
			// Run the executor off the caller goroutine so a slow Call does not block
			// the stream bridge, and observe the adapter context (bamlutils.Adapter
			// embeds context.Context) so a cancelled request stops waiting to deliver.
			ch := make(chan bamlutils.StreamResult, 1)
			go func() {
				defer close(ch)
				var res bamlutils.StreamResult
				out := exec.Call(adapter, MethodName, input)
				if out.Disposition == bamlutils.NativeSpineSucceeded {
					res = finalResult{final: out.Final}
				} else {
					// A pre-socket decline (typed capability decline) or a terminal
					// failed-after-claim: both surface as one error frame. Never a resend.
					res = errorResult{err: out.Err}
				}
				select {
				case ch <- res:
				case <-adapter.Done():
				}
			}()
			return ch, nil
		},
	}
	pm := bamlutils.ParseMethod{
		MakeOutput: func() any { return new(` + outputBase + `) },
		Impl: func(adapter bamlutils.Adapter, raw string) (any, error) {
			return exec.Parse(adapter, MethodName, raw)
		},
		StreamImpl: nil,
	}
	return sm, pm
}
` + codec + guard
}

// schemaGoType maps a schemadescriptor output type to a Go type expression,
// within the M3a+M3b final-carrier profile (primitive/enum/class/list/optional/
// string-keyed map/literal/multi-arm union). Class/enum references use the
// namespaced output type name; a multi-arm union resolves to its planned carrier
// name (see [carrierPlan]). Anything else — tuple, recursion, media, dynamic — is
// a fail-closed error; those stay declined by the classifier until their own
// sub-slice lands.
//
// plan supplies the native names for reachable multi-arm unions and MUST be the
// plan built for this bundle; a nil plan makes any multi-arm union a fail-closed
// error (the M3a optional `T?` and literal paths do not need it).
func schemaGoType(t schemadescriptor.Type, plan *carrierPlan) (string, error) {
	switch t.Kind {
	case schemadescriptor.TypePrimitive:
		switch t.Primitive {
		case schemadescriptor.PrimitiveString:
			return "string", nil
		case schemadescriptor.PrimitiveInt:
			return "int64", nil
		case schemadescriptor.PrimitiveFloat:
			return "float64", nil
		case schemadescriptor.PrimitiveBool:
			return "bool", nil
		default:
			return "", fmt.Errorf("primitive %q is outside the carrier profile", t.Primitive)
		}
	case schemadescriptor.TypeLiteral:
		// A standalone/arm literal lowers to its ordinary Go base, exactly like
		// BAML v0.223 generated fields (e.g. both `true` and `false` fields are
		// plain `bool`). Literal identity is carried only by union construction.
		return literalGoBase(t.Literal)
	case schemadescriptor.TypeEnum, schemadescriptor.TypeClass:
		return outputTypeName(t.Name), nil
	case schemadescriptor.TypeRecursiveAlias:
		// M3c: a reference to a structural recursive alias resolves to the emitted
		// Go alias name (`type Output<Name> = ...`), never a drop. The recursive-
		// occurrence -> `any` fallback is scoped to the alias's OWN declaration
		// (lowerAliasTarget); a reference from a class field / list / map / union
		// arm always names the alias, matching BAML's plain-lookup type_to_go.
		return outputTypeName(t.Name), nil
	case schemadescriptor.TypeList:
		if t.Elem == nil {
			return "", fmt.Errorf("list has no element type")
		}
		elem, err := schemaGoType(*t.Elem, plan)
		if err != nil {
			return "", err
		}
		return "[]" + elem, nil
	case schemadescriptor.TypeMap:
		if t.Key == nil || t.Value == nil {
			return "", fmt.Errorf("map has no key/value type")
		}
		// Only STRING-keyed maps are in the M3a profile — BAML maps a string-keyed
		// map to a JSON object, matching a Go map[string]V (json.Marshal sorts the
		// keys, as BAML's generated Go map type does).
		if t.Key.Kind != schemadescriptor.TypePrimitive || t.Key.Primitive != schemadescriptor.PrimitiveString {
			return "", fmt.Errorf("only string-keyed maps are in the carrier profile")
		}
		val, err := schemaGoType(*t.Value, plan)
		if err != nil {
			return "", err
		}
		return "map[string]" + val, nil
	case schemadescriptor.TypeUnion:
		if t.Union == nil {
			return "", fmt.Errorf("union has no payload")
		}
		// Optional-of-one (`T?`) stays M3a's `*T`.
		if t.Union.Nullable && len(t.Union.Variants) == 1 {
			inner, err := schemaGoType(t.Union.Variants[0], plan)
			if err != nil {
				return "", err
			}
			return "*" + inner, nil
		}
		// Multi-arm union → its planned carrier; nullable multi-arm → *carrier.
		if len(t.Union.Variants) >= 2 {
			name, ok := plan.unionName(t.Union.Variants)
			if !ok {
				return "", fmt.Errorf("multi-arm union was not planned (admission outran emission)")
			}
			if t.Union.Nullable {
				return "*" + name, nil
			}
			return name, nil
		}
		return "", fmt.Errorf("union with %d variant(s) is outside the carrier profile", len(t.Union.Variants))
	default:
		return "", fmt.Errorf("type kind %q is outside the carrier profile", t.Kind)
	}
}

// validateOutputRefs rejects any class/enum/recursive-alias reference in the
// return bundle — the Target or a nested field/element/variant/alias-target type —
// that does not resolve KIND- and MODE-exactly to a declaration. Resolution is
// kind-exact (a TypeClass must name a CLASS, never an enum or alias, and vice
// versa — the schemadescriptor keeps classes/enums/aliases in separate namespaces
// but they share the outputTypeName() Go identifier, so a merged name set would
// let a TypeClass{Name:"E"} silently bind an enum E and emit the wrong carrier)
// and mode-exact (a TypeClass/TypeRecursiveAlias is identified by (name, mode) —
// a streaming reference must not resolve against a non-streaming declaration).
// schemaGoType maps a reference to outputTypeName(t.Name) unconditionally, so an
// unresolved (or wrong-kind/wrong-mode) name would emit source binding an
// undefined or wrong Go type; this fail-closed backstop keeps admission == what
// the emitter can faithfully render.
func validateOutputRefs(ret schemadescriptor.Bundle) error {
	type classKey struct {
		name string
		mode schemadescriptor.StreamingMode
	}
	classDecl := make(map[classKey]bool, len(ret.Classes))
	for _, c := range ret.Classes {
		classDecl[classKey{c.Name.Name, c.Mode}] = true
	}
	enumDecl := make(map[string]bool, len(ret.Enums))
	for _, e := range ret.Enums {
		enumDecl[e.Name.Name] = true
	}
	// Structural recursive aliases are their own namespace (the table has no mode;
	// M3c alias references are always non-streaming — a streaming-mode alias ref is
	// rejected upstream by checkNoUnsupportedMetadata).
	aliasDecl := make(map[string]bool, len(ret.StructuralRecursiveAliases))
	for i := range ret.StructuralRecursiveAliases {
		aliasDecl[ret.StructuralRecursiveAliases[i].Name] = true
	}
	var walk func(t schemadescriptor.Type) error
	walk = func(t schemadescriptor.Type) error {
		switch t.Kind {
		case schemadescriptor.TypeClass:
			if !classDecl[classKey{t.Name, t.Mode}] {
				return fmt.Errorf("references undeclared class %q (mode %q)", t.Name, t.Mode)
			}
		case schemadescriptor.TypeEnum:
			if !enumDecl[t.Name] {
				return fmt.Errorf("references undeclared enum %q", t.Name)
			}
		case schemadescriptor.TypeRecursiveAlias:
			if !aliasDecl[t.Name] {
				return fmt.Errorf("references undeclared recursive alias %q", t.Name)
			}
		case schemadescriptor.TypeList:
			if t.Elem != nil {
				return walk(*t.Elem)
			}
		case schemadescriptor.TypeMap:
			if t.Key != nil {
				if err := walk(*t.Key); err != nil {
					return err
				}
			}
			if t.Value != nil {
				return walk(*t.Value)
			}
		case schemadescriptor.TypeUnion:
			if t.Union != nil {
				for _, v := range t.Union.Variants {
					if err := walk(v); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(ret.Target); err != nil {
		return fmt.Errorf("return target %w", err)
	}
	for _, c := range ret.Classes {
		for _, f := range c.Fields {
			if err := walk(f.Type); err != nil {
				return fmt.Errorf("class %q field %q %w", c.Name.Name, f.Name.Name, err)
			}
		}
	}
	// M3c: validate each structural recursive alias TARGET too. A TypeRecursiveAlias
	// node is a leaf here (checked against aliasDeclared above), so the walk stays
	// finite even when the target references the alias itself.
	for i := range ret.StructuralRecursiveAliases {
		a := &ret.StructuralRecursiveAliases[i]
		if err := walk(a.Target); err != nil {
			return fmt.Errorf("recursive alias %q target %w", a.Name, err)
		}
	}
	return nil
}

// valueGoType maps a promptdescriptor resolved input value type to a Go type
// expression, within the M1 profile (primitive/list/optional scalars — class and
// enum inputs are declined by the classifier).
func valueGoType(vt promptdescriptor.ResolvedValueType) (string, error) {
	base, err := valueGoBase(vt)
	if err != nil {
		return "", err
	}
	if vt.Nullable && vt.Kind != promptdescriptor.ValueList && vt.Kind != promptdescriptor.ValueNull {
		return "*" + base, nil
	}
	return base, nil
}

func valueGoBase(vt promptdescriptor.ResolvedValueType) (string, error) {
	switch vt.Kind {
	case promptdescriptor.ValueString:
		return "string", nil
	case promptdescriptor.ValueInt:
		return "int64", nil
	case promptdescriptor.ValueFloat:
		return "float64", nil
	case promptdescriptor.ValueBool:
		return "bool", nil
	case promptdescriptor.ValueList:
		if vt.Elem == nil {
			return "", fmt.Errorf("list has no element type")
		}
		elem, err := valueGoType(*vt.Elem)
		if err != nil {
			return "", err
		}
		return "[]" + elem, nil
	default:
		return "", fmt.Errorf("value kind %q is outside the M1 profile", vt.Kind)
	}
}

// staticFinalDecoderName selects the bamlutils static-final decoder for the emitted
// output carrier: the NARROW alias/union decoder (DecodeStaticAliasFinal) for a
// tagged-union output — a structural recursive alias target or a multi-arm union
// target, whose carrier dispatches its own UnmarshalJSON — else the generic strict
// decoder (DecodeStaticFinal). It is SCHEMA-driven (the nativespine emitter has no
// reflected sync func type to probe, unlike codegen_methods.finalResultDecoderName),
// and mirrors that router's intent: the recursive-alias / value-union return is kept
// on the separately-proven alias decoder so the generic decoder's proof set
// (scalars / flat classes / recursive-class pointer carriers) is never silently
// widened. For every nativespine carrier the two decoders differ only by
// DisallowUnknownFields, a no-op for a custom-UnmarshalJSON carrier.
func staticFinalDecoderName(ret schemadescriptor.Bundle) string {
	switch ret.Target.Kind {
	case schemadescriptor.TypeRecursiveAlias:
		return "DecodeStaticAliasFinal"
	case schemadescriptor.TypeUnion:
		if ret.Target.Union != nil && len(ret.Target.Union.Variants) >= 2 {
			return "DecodeStaticAliasFinal"
		}
	}
	return "DecodeStaticFinal"
}

// emitBinding writes the ExecBridge-U1 neutral per-method registration: the
// reflection-free scalar input projector, the strict static-final decoder, and the
// Binding() that pairs them with MethodName for the production runtime. The emitted
// closures name ONLY bamlutils / bamlutils/promptdescriptor / stdlib — no reflection,
// JSON round-trip, map iteration, generated BAML method, or CFFI decode.
func emitBinding(b *strings.Builder, m projectdescriptor.Method, inputStruct, outputBase, decoderName string) error {
	fmt.Fprintf(b, "// projectInput lowers the typed input carrier into the ordered projected\n")
	fmt.Fprintf(b, "// argument vector (exact type assertions, direct fields, canonical BAML names as\n")
	fmt.Fprintf(b, "// literals — no reflection, JSON round-trip, or map iteration).\n")
	fmt.Fprintf(b, "func projectInput(input any) ([]promptdescriptor.ArgumentValue, error) {\n")
	fmt.Fprintf(b, "\tin, ok := input.(*%s)\n", inputStruct)
	fmt.Fprintf(b, "\tif !ok {\n\t\treturn nil, fmt.Errorf(%q, MethodName, input)\n\t}\n", "nativespine: %s: input has Go type %T, want *"+inputStruct)
	fmt.Fprintf(b, "\t_ = in\n")
	fmt.Fprintf(b, "\tvalues := make([]promptdescriptor.ArgumentValue, 0, %d)\n", len(m.Args))
	for i, a := range m.Args {
		expr, err := buildStaticValueExpr(b, "in."+strcase.UpperCamelCase(a.Name), a.Type, fmt.Sprintf("arg%d", i))
		if err != nil {
			return fmt.Errorf("input arg %q: %w", a.Name, err)
		}
		fmt.Fprintf(b, "\tvalues = append(values, promptdescriptor.ArgumentValue{Name: %q, Value: %s})\n", a.Name, expr)
	}
	fmt.Fprintf(b, "\treturn values, nil\n}\n\n")

	fmt.Fprintf(b, "// decodeFinal strictly decodes the native canonical JSON into the emitted output\n")
	fmt.Fprintf(b, "// carrier via the proven bamlutils core (NO generated BAML, NO CFFI).\n")
	fmt.Fprintf(b, "func decodeFinal(canonicalJSON []byte) (any, error) {\n")
	fmt.Fprintf(b, "\treturn bamlutils.%s[%s](canonicalJSON)\n}\n\n", decoderName, outputBase)

	fmt.Fprintf(b, "// Binding returns the neutral per-method registration the production runtime\n")
	fmt.Fprintf(b, "// consumes: MethodName plus the reflection-free projector and strict decoder,\n")
	fmt.Fprintf(b, "// both resolved here at registration time (non-nil).\n")
	fmt.Fprintf(b, "func Binding() bamlutils.NativeSpineUnaryBinding {\n")
	fmt.Fprintf(b, "\treturn bamlutils.NativeSpineUnaryBinding{\n")
	fmt.Fprintf(b, "\t\tMethod:       MethodName,\n")
	fmt.Fprintf(b, "\t\tProjectInput: projectInput,\n")
	fmt.Fprintf(b, "\t\tDecodeFinal:  decodeFinal,\n")
	fmt.Fprintf(b, "\t}\n}\n")
	return nil
}

// buildStaticValueExpr emits any prelude statements needed to project the value at
// fieldExpr (of resolved type vt) into a promptdescriptor.StaticValue, and returns
// the Go expression for that value. A non-nullable scalar is a single inline
// expression (no prelude); a nullable scalar and a (possibly nested) list emit
// prelude statements building a uniquely-named local. It covers exactly the M1 input
// profile the input carrier (valueGoType) emits — string/int/float/bool scalars,
// optionally nullable, and lists of them — so admission never outruns what the
// projector can lower. seed makes emitted locals unique per argument/nesting depth.
func buildStaticValueExpr(b *strings.Builder, fieldExpr string, vt promptdescriptor.ResolvedValueType, seed string) (string, error) {
	switch vt.Kind {
	case promptdescriptor.ValueString, promptdescriptor.ValueInt, promptdescriptor.ValueFloat, promptdescriptor.ValueBool:
		kind, field := scalarStaticKindField(vt.Kind)
		if vt.Nullable {
			// A nullable scalar is a `*T` carrier field: nil projects to StaticNull, a
			// present value to the scalar of the dereferenced value.
			fmt.Fprintf(b, "\tvar %s promptdescriptor.StaticValue\n", seed)
			fmt.Fprintf(b, "\tif %s == nil {\n", fieldExpr)
			fmt.Fprintf(b, "\t\t%s = promptdescriptor.StaticValue{Kind: promptdescriptor.StaticNull}\n", seed)
			fmt.Fprintf(b, "\t} else {\n")
			fmt.Fprintf(b, "\t\t%s = promptdescriptor.StaticValue{Kind: promptdescriptor.%s, %s: *%s}\n", seed, kind, field, fieldExpr)
			fmt.Fprintf(b, "\t}\n")
			return seed, nil
		}
		return fmt.Sprintf("promptdescriptor.StaticValue{Kind: promptdescriptor.%s, %s: %s}", kind, field, fieldExpr), nil
	case promptdescriptor.ValueList:
		if vt.Elem == nil {
			return "", fmt.Errorf("list has no element type")
		}
		itemsVar := seed + "Items"
		fmt.Fprintf(b, "\t%s := make([]promptdescriptor.StaticValue, 0, len(%s))\n", itemsVar, fieldExpr)
		fmt.Fprintf(b, "\tfor _, %sElem := range %s {\n", seed, fieldExpr)
		elemExpr, err := buildStaticValueExpr(b, seed+"Elem", *vt.Elem, seed+"e")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(b, "\t\t%s = append(%s, %s)\n", itemsVar, itemsVar, elemExpr)
		fmt.Fprintf(b, "\t}\n")
		listVar := seed + "List"
		if vt.Nullable {
			// A NULLABLE list's carrier field is a slice (valueGoType keeps ValueList as
			// []T, never *[]T), so a NIL slice is the source `null` and a non-nil slice —
			// INCLUDING a non-nil empty one — is `[]`. Project them distinctly: nil to
			// StaticNull, non-nil to the StaticList built above. Without this, a nil slice
			// would fall through to StaticList{Items: []} and silently rewrite null -> [].
			fmt.Fprintf(b, "\tvar %s promptdescriptor.StaticValue\n", listVar)
			fmt.Fprintf(b, "\tif %s == nil {\n", fieldExpr)
			fmt.Fprintf(b, "\t\t%s = promptdescriptor.StaticValue{Kind: promptdescriptor.StaticNull}\n", listVar)
			fmt.Fprintf(b, "\t} else {\n")
			fmt.Fprintf(b, "\t\t%s = promptdescriptor.StaticValue{Kind: promptdescriptor.StaticList, Items: %s}\n", listVar, itemsVar)
			fmt.Fprintf(b, "\t}\n")
			return listVar, nil
		}
		fmt.Fprintf(b, "\t%s := promptdescriptor.StaticValue{Kind: promptdescriptor.StaticList, Items: %s}\n", listVar, itemsVar)
		return listVar, nil
	default:
		// class / enum / null inputs are declined by the classifier; fail closed so the
		// projector never silently drops one.
		return "", fmt.Errorf("input value kind %q is outside the projectable M1 profile", vt.Kind)
	}
}

func scalarStaticKindField(k promptdescriptor.ValueKind) (kind, field string) {
	switch k {
	case promptdescriptor.ValueString:
		return "StaticString", "String"
	case promptdescriptor.ValueInt:
		return "StaticInt", "Int"
	case promptdescriptor.ValueFloat:
		return "StaticFloat", "Float"
	case promptdescriptor.ValueBool:
		return "StaticBool", "Bool"
	default:
		return "", ""
	}
}
