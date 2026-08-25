package codegen

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

func strptr(s string) *string { return &s }

// sampleMethod is a minimal admitted static-unary method (scalar input, class
// output) built directly, so this test needs no introspect pipeline (which lives
// in the root module and cannot be imported here).
func sampleMethod() projectdescriptor.Method {
	return projectdescriptor.Method{
		Name:   "Greet",
		Class:  projectdescriptor.ClassStaticUnary,
		Prompt: "Greet {{ name }}",
		Args: []projectdescriptor.Argument{
			{Name: "name", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueString}},
			{Name: "formal", Type: promptdescriptor.ResolvedValueType{Kind: promptdescriptor.ValueBool}},
		},
		Client:   "GPT4",
		Provider: "openai",
		Model:    projectdescriptor.Model{Value: "gpt-4o", Provenance: promptdescriptor.ModelProvenanceLiteral},
		Return: sd.Bundle{
			Version: sd.Version,
			Method:  "Greet",
			Target:  sd.Type{Kind: sd.TypeClass, Name: "Greeting"},
			Classes: []sd.ClassDef{{
				Name: sd.Name{Name: "Greeting"},
				Fields: []sd.ClassField{
					{Name: sd.Name{Name: "text"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}},
					{Name: sd.Name{Name: "formal"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveBool}},
				},
			}},
		},
	}
}

func TestEmitNativeStaticUnary(t *testing.T) {
	m := sampleMethod()
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "greetpkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := string(src)

	// The emitted import graph is bamlutils-only — no baml_client, no BAML runtime.
	for _, forbidden := range []string{"boundaryml/baml", "baml_client", "baml-patched", "language_client_go"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("emitted code references forbidden import %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, `"github.com/invakid404/baml-rest/bamlutils"`) {
		t.Error("emitted code does not import bamlutils")
	}
	// Carriers, method name, registration.
	for _, want := range []string{
		"package greetpkg",
		`const MethodName = "Greet"`,
		"type GreetInput struct",
		`json:"name"`, // input args keep struct tags (identifiers, not aliases)
		`json:"formal"`,
		"type OutputGreeting struct", // output types are namespaced (P1-5)
		"func (v OutputGreeting) MarshalJSON",
		"nativeSpineMarshalObject", // pure-Go alias-faithful codec (P1-4)
		`{"text", v.Text}`,         // exact wire key
		"type Executor interface",
		"func BuildMethod(exec Executor)",
		"bamlutils.StreamModeCall",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted code missing %q", want)
		}
	}

	// Determinism.
	src2, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "greetpkg"})
	if err != nil {
		t.Fatal(err)
	}
	if got != string(src2) {
		t.Fatal("emitter is not deterministic")
	}
}

func TestEmitNativeStaticUnaryRejectsNonUnary(t *testing.T) {
	m := sampleMethod()
	m.Class = "something_else"
	if _, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "x"}); err == nil {
		t.Fatal("want error for non-static-unary class, got nil")
	}
}

// aliasMethod is sampleMethod with the first output field aliased.
func aliasMethod(alias string) projectdescriptor.Method {
	m := sampleMethod()
	m.Return.Classes[0].Fields[0].Name.Alias = strptr(alias)
	return m
}

// TestEmitNativeStaticUnaryAliasCodec proves output fields serialize under their
// EXACT BAML wire key via the custom codec — even keys a Go struct tag cannot
// express ("-" would drop the field; "" would fall back to the Go name) (P1-4).
func TestEmitNativeStaticUnaryAliasCodec(t *testing.T) {
	dash, err := EmitNativeStaticUnary(aliasMethod("-"), NativeSpineOptions{PackageName: "p"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(dash)
	if strings.Contains(got, "`json:\"-\"`") {
		t.Error("emitted a json:\"-\" struct tag on an output field — encoding/json would drop it")
	}
	// Marshal side carries the exact key; unmarshal side keys on it too (gofmt may
	// pad the map value, so match the key alone).
	if !strings.Contains(got, `{"-", v.Text}`) || !strings.Contains(got, `"-":`) {
		t.Errorf("codec does not carry the exact wire key %q:\n%s", "-", got)
	}

	empty, err := EmitNativeStaticUnary(aliasMethod(""), NativeSpineOptions{PackageName: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `{"", v.Text}`) {
		t.Error("codec does not carry an empty wire key")
	}
}

// TestEmitNativeStaticUnaryCompiles is the REAL type-check backstop (go/format
// does not catch duplicate declarations): an output class named "Executor" —
// which would collide with the fixed Executor interface without the namespace
// prefix (P1-5) — and a field aliased "-" (exercising the codec, P1-4) must
// compile.
func TestEmitNativeStaticUnaryCompiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping compile backstop")
	}
	m := sampleMethod()
	m.Name = "Collide"
	m.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Executor"}
	m.Return.Classes = []sd.ClassDef{{
		Name: sd.Name{Name: "Executor"},
		Fields: []sd.ClassField{
			{Name: sd.Name{Name: "value", Alias: strptr("-")}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}},
		},
	}}
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "collidepkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if strings.Contains(string(src), "type Executor struct") {
		t.Fatal("output class emitted as unprefixed 'Executor', would collide with the Executor interface")
	}
	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, string(src), nil)
	testharness.RunGoBuild(t, tmp) // fails the test on any duplicate-decl / codec compile error
}

func strField(name, alias string) sd.ClassField {
	f := sd.ClassField{Name: sd.Name{Name: name}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}}
	if alias != "\x00" {
		f.Name.Alias = strptr(alias)
	}
	return f
}

// TestEmitNativeStaticUnaryRejectsNameCollision is the emitter backstop for the
// classifier's name-collision gate (P1-5, fix #2): lossy strcase normalization
// makes distinct BAML names collide into the same Go identifier, which go/format
// accepts but go build rejects. The emitter must fail closed rather than emit
// uncompilable duplicate declarations/fields.
func TestEmitNativeStaticUnaryRejectsNameCollision(t *testing.T) {
	// Field collision: foo_bar and fooBar both normalize to FooBar.
	mf := sampleMethod()
	mf.Name = "FieldCollide"
	mf.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Reply"}
	mf.Return.Classes = []sd.ClassDef{{
		Name:   sd.Name{Name: "Reply"},
		Fields: []sd.ClassField{strField("foo_bar", "\x00"), strField("fooBar", "\x00")},
	}}
	if _, err := EmitNativeStaticUnary(mf, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for field-name normalization collision, got nil")
	}

	// Type collision: Foo_Bar and FooBar both normalize to OutputFooBar.
	mt := sampleMethod()
	mt.Name = "TypeCollide"
	mt.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Wrap"}
	mt.Return.Classes = []sd.ClassDef{
		{Name: sd.Name{Name: "Foo_Bar"}, Fields: []sd.ClassField{strField("x", "\x00")}},
		{Name: sd.Name{Name: "FooBar"}, Fields: []sd.ClassField{strField("y", "\x00")}},
		{Name: sd.Name{Name: "Wrap"}, Fields: []sd.ClassField{
			{Name: sd.Name{Name: "a"}, Type: sd.Type{Kind: sd.TypeClass, Name: "Foo_Bar"}},
			{Name: sd.Name{Name: "b"}, Type: sd.Type{Kind: sd.TypeClass, Name: "FooBar"}},
		}},
	}
	if _, err := EmitNativeStaticUnary(mt, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for type-name normalization collision, got nil")
	}
}

// TestEmitNativeStaticUnaryRejectsCarrierAndEnumCollisions covers the two
// collision shapes the earlier preflight missed (P1-5, fix #3): the generated
// input carrier name vs an output type name, and an enum CONSTANT vs a class type.
func TestEmitNativeStaticUnaryRejectsCarrierAndEnumCollisions(t *testing.T) {
	// Input carrier vs output type: method OutputFoo + class FooInput both -> OutputFooInput.
	ic := sampleMethod()
	ic.Name = "OutputFoo"
	ic.Args = nil
	ic.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "FooInput"}
	ic.Return.Classes = []sd.ClassDef{{Name: sd.Name{Name: "FooInput"}, Fields: []sd.ClassField{strField("value", "\x00")}}}
	if _, err := EmitNativeStaticUnary(ic, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for input-carrier vs output-type collision, got nil")
	}

	// Enum constant vs class type: enum Color{RED} const + class ColorRed type both -> OutputColorRed.
	ec := sampleMethod()
	ec.Name = "W"
	ec.Args = nil
	ec.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Wrap"}
	ec.Return.Enums = []sd.EnumDef{{Name: sd.Name{Name: "Color"}, Values: []sd.EnumValue{{Name: sd.Name{Name: "RED"}}}}}
	ec.Return.Classes = []sd.ClassDef{
		{Name: sd.Name{Name: "ColorRed"}, Fields: []sd.ClassField{strField("value", "\x00")}},
		{Name: sd.Name{Name: "Wrap"}, Fields: []sd.ClassField{strField("a", "\x00")}},
	}
	if _, err := EmitNativeStaticUnary(ec, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for enum-constant vs class-type collision, got nil")
	}
}

// TestEmitNativeStaticUnaryRejectsCodecMethodCollision proves the emitter backstop
// rejects an output field whose Go identifier collides with a generated codec
// METHOD (fix #4, P1): emitClassCodec always declares MarshalJSON/UnmarshalJSON,
// and a field and method cannot share a name.
func TestEmitNativeStaticUnaryRejectsCodecMethodCollision(t *testing.T) {
	for _, field := range []string{"marshal_J_S_O_N", "unmarshal_J_S_O_N"} {
		m := sampleMethod()
		m.Name = "Codec"
		m.Args = nil
		m.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Reply"}
		m.Return.Classes = []sd.ClassDef{{Name: sd.Name{Name: "Reply"}, Fields: []sd.ClassField{strField(field, "\x00")}}}
		if _, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "p"}); err == nil {
			t.Errorf("field %q: want error (collides with generated codec method), got nil", field)
		}
	}
}

// TestEmitNativeStaticUnaryRejectsDuplicateWireKey proves the emitter backstop
// rejects two fields of one output class that share a WIRE key (alias or canonical
// name). Their Go fields differ, so the field-normalization guard passes, but the
// codec would emit a duplicate map-literal key (a Go compile error) and a doubled
// JSON key. Here `body @alias("text")` collides with the canonical `text`.
func TestEmitNativeStaticUnaryRejectsDuplicateWireKey(t *testing.T) {
	m := sampleMethod()
	m.Name = "WireDup"
	m.Args = nil
	m.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Reply"}
	m.Return.Classes = []sd.ClassDef{{
		Name: sd.Name{Name: "Reply"},
		Fields: []sd.ClassField{
			{Name: sd.Name{Name: "body", Alias: strptr("text")}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}},
			{Name: sd.Name{Name: "text"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString}},
		},
	}}
	if _, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for duplicate wire key across two fields, got nil")
	}
}

// TestEmitNativeStaticUnaryRejectsUndeclaredRef proves the emitter backstop rejects
// a bundle that references an output type it does not declare — which would emit
// source naming an undefined Go type. Covers a Target ref and a nested field ref.
func TestEmitNativeStaticUnaryRejectsUndeclaredRef(t *testing.T) {
	// Target references an undeclared class.
	mt := sampleMethod()
	mt.Name = "BadTarget"
	mt.Args = nil
	mt.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Missing"}
	mt.Return.Classes = nil
	if _, err := EmitNativeStaticUnary(mt, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for undeclared target type, got nil")
	}

	// A declared class field references an undeclared enum.
	mf := sampleMethod()
	mf.Name = "BadField"
	mf.Args = nil
	mf.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Wrap"}
	mf.Return.Classes = []sd.ClassDef{{
		Name:   sd.Name{Name: "Wrap"},
		Fields: []sd.ClassField{{Name: sd.Name{Name: "color"}, Type: sd.Type{Kind: sd.TypeEnum, Name: "Color"}}},
	}}
	if _, err := EmitNativeStaticUnary(mf, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for undeclared field type, got nil")
	}
}

// TestEmitNativeStaticUnaryAliasRoundTrip is the required BEHAVIORAL proof (P1-4,
// fix #2): it COMPILES and EXECUTES the emitted MarshalJSON/UnmarshalJSON and
// asserts exact wire bytes both directions for a canonical (nil-alias) key, a
// present-empty-alias key, a "-" key, and a comma key — guarding against a
// marshal/unmarshal key mismatch that would still compile.
func TestEmitNativeStaticUnaryAliasRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping behavioral round-trip")
	}
	m := sampleMethod()
	m.Name = "RT"
	m.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Reply"}
	m.Return.Classes = []sd.ClassDef{{
		Name: sd.Name{Name: "Reply"},
		Fields: []sd.ClassField{
			strField("canon", "\x00"), // nil alias -> canonical "canon"
			strField("empty", ""),     // present empty alias -> ""
			strField("dash", "-"),     // "-" (a json:"-" tag would drop the field)
			strField("comma", "a,b"),  // comma (a struct tag would read it as options)
		},
	}}
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "rtpkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	rt := "package rtpkg\n\n" +
		"import (\n\t\"encoding/json\"\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
		"func TestRoundTrip(t *testing.T) {\n" +
		"\tv := OutputReply{Canon: \"c\", Empty: \"e\", Dash: \"d\", Comma: \"m\"}\n" +
		"\tgot, err := json.Marshal(v)\n" +
		"\tif err != nil { t.Fatal(err) }\n" +
		"\tconst want = `{\"canon\":\"c\",\"\":\"e\",\"-\":\"d\",\"a,b\":\"m\"}`\n" +
		"\tif string(got) != want { t.Fatalf(\"marshal = %s, want %s\", got, want) }\n" +
		"\tvar back OutputReply\n" +
		"\tif err := json.Unmarshal([]byte(want), &back); err != nil { t.Fatal(err) }\n" +
		"\tif !reflect.DeepEqual(v, back) { t.Fatalf(\"round-trip mismatch: %+v != %+v\", back, v) }\n" +
		"}\n"
	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, string(src), map[string]string{"roundtrip_test.go": rt})
	if out, err := testharness.RunGoTest(t, tmp, "TestRoundTrip"); err != nil {
		t.Fatalf("emitted codec round-trip failed: %v\n%s", err, out)
	}
}
