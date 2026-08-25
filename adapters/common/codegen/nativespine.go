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
//   - Output carriers serialize the EXACT BAML wire key of every field via a
//     pure-Go custom JSON codec (nativeSpineMarshalObject/UnmarshalObject), so
//     arbitrary aliases — "-", "", names with commas, unicode — round-trip
//     losslessly instead of being mangled by an encoding/json struct tag.
//   - Emitted output type names are namespaced with an "Output" prefix so they
//     can never collide with the fixed generated declarations (Executor,
//     MethodName, BuildMethod, ErrUnsupportedStreamMode, <Method>Input).
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
		// The codec keys its map[string]any / marshals each field by its exact WIRE
		// key (wireName: alias when present, else canonical name), which is a
		// different space than the normalized Go field. Two fields can share a wire
		// key (e.g. a field aliased onto another's name, or two empty aliases) while
		// having distinct Go fields; that emits a duplicate map-literal key (a Go
		// compile error) and a doubled JSON key. Reject it here, fail-closed.
		keys := map[string]bool{}
		for _, f := range c.Fields {
			fg := strcase.UpperCamelCase(f.Name.Name)
			if what, ok := fields[fg]; ok {
				return fmt.Errorf("field %q of class %q normalizes to Go identifier %q, colliding with %s", f.Name.Name, c.Name.Name, fg, what)
			}
			fields[fg] = "field " + f.Name.Name
			k := wireName(f.Name)
			if keys[k] {
				return fmt.Errorf("fields of class %q share the wire key %q", c.Name.Name, k)
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
	if err := validateOutputRefs(m.Return); err != nil {
		return nil, fmt.Errorf("codegen: method %q: %w", m.Name, err)
	}

	inputStruct := strcase.UpperCamelCase(m.Name + "Input")

	outputBase, err := schemaGoType(m.Return.Target)
	if err != nil {
		return nil, fmt.Errorf("codegen: method %q: output %w", m.Name, err)
	}

	hasClasses := len(m.Return.Classes) > 0
	hasEnums := len(m.Return.Enums) > 0

	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by adapters/common/codegen EmitNativeStaticUnary; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", opts.PackageName)

	// Imports: fmt + bamlutils always. bytes when an output class is emitted (its
	// codec assembles bytes by hand). encoding/json when a class OR an enum is
	// emitted: the class codec encodes each field with it (HTML escaping off, to
	// match the serving serializer), and the validating enum codec marshals/
	// unmarshals its string value with it.
	b.WriteString("import (\n")
	if hasClasses {
		fmt.Fprintf(&b, "\t%q\n", "bytes")
	}
	if hasClasses || hasEnums {
		fmt.Fprintf(&b, "\t%q\n", "encoding/json")
	}
	fmt.Fprintf(&b, "\t%q\n\n\t%q\n)\n\n", "fmt", bamlutilsPkg)

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
				fmt.Fprintf(&b, "\t%s%s %s = %q\n", name, strcase.UpperCamelCase(v.Name.Name), name, wireName(v.Name))
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
			gt, err := schemaGoType(f.Type)
			if err != nil {
				return nil, fmt.Errorf("codegen: method %q: class %s field %q %w", m.Name, c.Name.Name, f.Name.Name, err)
			}
			goField := strcase.UpperCamelCase(f.Name.Name)
			fields = append(fields, goField)
			// No json struct tag: the custom codec below emits the exact wire key.
			fmt.Fprintf(&b, "\t%s %s\n", goField, gt)
		}
		fmt.Fprintf(&b, "}\n\n")
		emitClassCodec(&b, name, c.Fields, fields)
	}

	// Neutral executor seam + boilerplate + registration (+ object-codec helpers
	// when any output class was emitted).
	b.WriteString(nativeSpineBoilerplate(inputStruct, outputBase, hasClasses))

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("codegen: method %q: gofmt emitted source: %w\n---\n%s", m.Name, err, b.String())
	}
	return src, nil
}

// emitClassCodec emits MarshalJSON/UnmarshalJSON for one output class that map
// each Go field to the EXACT BAML wire key, so arbitrary aliases round-trip.
func emitClassCodec(b *strings.Builder, typeName string, schemaFields []schemadescriptor.ClassField, goFields []string) {
	fmt.Fprintf(b, "// MarshalJSON emits %s with each field under its exact BAML wire key.\n", typeName)
	fmt.Fprintf(b, "func (v %s) MarshalJSON() ([]byte, error) {\n", typeName)
	fmt.Fprintf(b, "\treturn nativeSpineMarshalObject([]nativeSpineField{\n")
	for i, f := range schemaFields {
		fmt.Fprintf(b, "\t\t{%q, v.%s},\n", wireName(f.Name), goFields[i])
	}
	fmt.Fprintf(b, "\t})\n}\n\n")

	fmt.Fprintf(b, "// UnmarshalJSON reads %s from each field's exact BAML wire key.\n", typeName)
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error {\n", typeName)
	fmt.Fprintf(b, "\treturn nativeSpineUnmarshalObject(data, map[string]any{\n")
	for i, f := range schemaFields {
		fmt.Fprintf(b, "\t\t%q: &v.%s,\n", wireName(f.Name), goFields[i])
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
// StreamingMethod/ParseMethod builder, and (when withCodec) the pure-Go
// object-codec helpers. inputStruct is the typed input carrier; outputBase is the
// Go type the output carriers construct.
func nativeSpineBoilerplate(inputStruct, outputBase string, withCodec bool) string {
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
	return `// Executor is the neutral injected execution seam. M1 injects a fake; the real
// native request/parse executor arrives in a later milestone. It is NOT a BAML
// request or parse call.
type Executor interface {
	// Call runs the unary final call and returns the final output Go value.
	Call(method string, input any) (any, error)
	// Parse coerces raw text into the method's output Go value.
	Parse(method string, raw string) (any, error)
}

// ErrUnsupportedStreamMode is the stable decline returned when a mode other than
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
// MethodName, driven by exec. The StreamingMethod admits unary call mode only
// and declines every other mode with ErrUnsupportedStreamMode; neither closure
// makes a BAML request or parse call. A successful executor result is delivered
// as one final frame; an executor error as one error frame — both flow through
// the existing worker stream bridge and envelope.
func BuildMethod(exec Executor) (bamlutils.StreamingMethod, bamlutils.ParseMethod) {
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
				if final, err := exec.Call(MethodName, input); err != nil {
					res = errorResult{err: err}
				} else {
					res = finalResult{final: final}
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
			return exec.Parse(MethodName, raw)
		},
		StreamImpl: nil,
	}
	return sm, pm
}
` + codec
}

// wireName returns the JSON wire name for a schemadescriptor.Name: its alias when
// present (even when empty), else its canonical name.
func wireName(n schemadescriptor.Name) string {
	if n.Alias != nil {
		return *n.Alias
	}
	return n.Name
}

// schemaGoType maps a schemadescriptor output type to a Go type expression,
// within the M3a final-carrier profile (primitive/enum/class/list/optional/
// string-keyed map). Class/enum references use the namespaced output type name.
// Anything else — union (other than the optional T? form), tuple, literal,
// recursion, media, dynamic — is a fail-closed error; those stay declined by the
// classifier until their own sub-slice lands.
func schemaGoType(t schemadescriptor.Type) (string, error) {
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
	case schemadescriptor.TypeEnum, schemadescriptor.TypeClass:
		return outputTypeName(t.Name), nil
	case schemadescriptor.TypeList:
		if t.Elem == nil {
			return "", fmt.Errorf("list has no element type")
		}
		elem, err := schemaGoType(*t.Elem)
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
		val, err := schemaGoType(*t.Value)
		if err != nil {
			return "", err
		}
		return "map[string]" + val, nil
	case schemadescriptor.TypeUnion:
		if t.Union != nil && t.Union.Nullable && len(t.Union.Variants) == 1 {
			inner, err := schemaGoType(t.Union.Variants[0])
			if err != nil {
				return "", err
			}
			return "*" + inner, nil
		}
		return "", fmt.Errorf("only optional (single nullable variant) unions are in the carrier profile")
	default:
		return "", fmt.Errorf("type kind %q is outside the carrier profile", t.Kind)
	}
}

// validateOutputRefs rejects any class/enum reference in the return bundle — the
// Target or a nested field/element/variant type — that names a type the bundle
// does not declare. schemaGoType maps such a reference to outputTypeName(t.Name)
// unconditionally, so an undeclared name would emit source referencing an
// undefined Go type (uncompilable). This is a fail-closed backstop for the
// exported entrypoint; the real classifier only ever produces closed bundles.
func validateOutputRefs(ret schemadescriptor.Bundle) error {
	declared := make(map[string]bool, len(ret.Classes)+len(ret.Enums))
	for _, c := range ret.Classes {
		declared[c.Name.Name] = true
	}
	for _, e := range ret.Enums {
		declared[e.Name.Name] = true
	}
	var walk func(t schemadescriptor.Type) error
	walk = func(t schemadescriptor.Type) error {
		switch t.Kind {
		case schemadescriptor.TypeEnum, schemadescriptor.TypeClass:
			if !declared[t.Name] {
				return fmt.Errorf("references undeclared output type %q", t.Name)
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
