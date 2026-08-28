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

// TestEmitNativeStaticUnaryAliasIgnoredCanonicalOutput proves M3b's canonical
// output-key policy (scope §2): an @alias on an output field is IGNORED — the
// codec keys the field by its CANONICAL name (field.Name.Name), never the alias.
// This holds for exotic aliases a struct tag could not even express ("-", "", a
// comma, unicode), which must NOT become output tokens.
func TestEmitNativeStaticUnaryAliasIgnoredCanonicalOutput(t *testing.T) {
	for _, alias := range []string{"-", "", "a,b", "naïve", "score"} {
		src, err := EmitNativeStaticUnary(aliasMethod(alias), NativeSpineOptions{PackageName: "p"})
		if err != nil {
			t.Fatalf("alias %q: emit: %v", alias, err)
		}
		got := string(src)
		// The canonical field name "text" is the marshal AND unmarshal key.
		if !strings.Contains(got, `{"text", v.Text}`) || !strings.Contains(got, `"text":`) {
			t.Errorf("alias %q: codec does not key on the canonical name %q:\n%s", alias, "text", got)
		}
		// The alias string is never emitted as a codec key (skip "" which would
		// match spuriously, and skip "score" which contains no chars that would
		// only appear as a key — assert the codec key line specifically).
		if alias != "" && strings.Contains(got, `{"`+alias+`", v.Text}`) {
			t.Errorf("alias %q leaked into the output codec as a wire key:\n%s", alias, got)
		}
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

// unionField returns a class field of a bare 2-arm union type, used to force a
// planned OutputUnion1 in the synthetic collision tests.
func unionField(name string, arms ...sd.Type) sd.ClassField {
	return sd.ClassField{Name: sd.Name{Name: name}, Type: sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: arms}}}
}

// TestEmitNativeStaticUnaryRejectsUnionNameCollisions is the emitter backstop for
// the M3b plan (open risk 4): a planned union carrier's type name or constructor
// name colliding with a declared output type must fail closed, so the classifier
// (which delegates to CheckNativeNameCollision) declines EXACTLY what the emitter
// cannot emit. A synthetic descriptor is used because the BAML parser cannot
// produce these collisions directly.
func TestEmitNativeStaticUnaryRejectsUnionNameCollisions(t *testing.T) {
	prim := func(p sd.PrimitiveKind) sd.Type { return sd.Type{Kind: sd.TypePrimitive, Primitive: p} }

	// Union TYPE-name collision: class "Union1" -> OutputUnion1, colliding with the
	// planned OutputUnion1 for the string|int field.
	tc := sampleMethod()
	tc.Name = "UnionTypeCollide"
	tc.Args = nil
	tc.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Wrap"}
	tc.Return.Classes = []sd.ClassDef{
		{Name: sd.Name{Name: "Union1"}, Fields: []sd.ClassField{strField("x", "\x00")}},
		{Name: sd.Name{Name: "Wrap"}, Fields: []sd.ClassField{
			unionField("u", prim(sd.PrimitiveString), prim(sd.PrimitiveInt)),
			{Name: sd.Name{Name: "w"}, Type: sd.Type{Kind: sd.TypeClass, Name: "Union1"}},
		}},
	}
	if _, err := EmitNativeStaticUnary(tc, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for class Union1 colliding with planned OutputUnion1, got nil")
	}
	if err := CheckNativeNameCollision(tc.Name, nil, tc.Return); err == nil {
		t.Error("classifier preflight must also reject the union type-name collision")
	}

	// Union CONSTRUCTOR-name collision: class "Union1NewVariant0" ->
	// OutputUnion1NewVariant0, colliding with the generated constructor.
	cc := sampleMethod()
	cc.Name = "UnionCtorCollide"
	cc.Args = nil
	cc.Return.Target = sd.Type{Kind: sd.TypeClass, Name: "Wrap"}
	cc.Return.Classes = []sd.ClassDef{
		{Name: sd.Name{Name: "Union1NewVariant0"}, Fields: []sd.ClassField{strField("x", "\x00")}},
		{Name: sd.Name{Name: "Wrap"}, Fields: []sd.ClassField{
			unionField("u", prim(sd.PrimitiveString), prim(sd.PrimitiveInt)),
			{Name: sd.Name{Name: "w"}, Type: sd.Type{Kind: sd.TypeClass, Name: "Union1NewVariant0"}},
		}},
	}
	if _, err := EmitNativeStaticUnary(cc, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for class Union1NewVariant0 colliding with planned constructor, got nil")
	}
}

// TestEmitNativeStaticUnaryRejectsMalformedUnion is the emitter backstop for a
// union shape the classifier declines and the BAML parser cannot produce: a
// zero-arm union and a nil-payload literal arm must fail closed at emit time even
// when the classifier preflight is bypassed (direct emitter call).
func TestEmitNativeStaticUnaryRejectsMalformedUnion(t *testing.T) {
	// Nil union payload (distinct from a non-nil empty Variants slice).
	np := sampleMethod()
	np.Name = "NilPayload"
	np.Args = nil
	np.Return.Target = sd.Type{Kind: sd.TypeUnion, Union: nil}
	np.Return.Classes = nil
	if _, err := EmitNativeStaticUnary(np, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for a nil-payload union target, got nil")
	}

	// Zero-arm (non-nullable) union target.
	z := sampleMethod()
	z.Name = "ZeroArm"
	z.Args = nil
	z.Return.Target = sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: nil}}
	z.Return.Classes = nil
	if _, err := EmitNativeStaticUnary(z, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for a zero-arm union target, got nil")
	}

	// A multi-arm union with a nil-payload literal arm.
	n := sampleMethod()
	n.Name = "NilLit"
	n.Args = nil
	n.Return.Target = sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Variants: []sd.Type{
		{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString},
		{Kind: sd.TypeLiteral, Literal: nil},
	}}}
	n.Return.Classes = nil
	if _, err := EmitNativeStaticUnary(n, NativeSpineOptions{PackageName: "p"}); err == nil {
		t.Error("want error for a nil-payload literal union arm, got nil")
	}
}

// TestEmitNativeStaticUnaryAliasOntoAnotherCanonicalAdmitted proves the M3b
// output policy resolves the former M3a "duplicate wire key" case: `body
// @alias("text")` alongside a canonical `text` field no longer collides, because
// the alias is IGNORED — the two output keys are the distinct canonical names
// `body` and `text`. The emit succeeds and emits both canonical keys once.
func TestEmitNativeStaticUnaryAliasOntoAnotherCanonicalAdmitted(t *testing.T) {
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
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "p"})
	if err != nil {
		t.Fatalf("alias-onto-another-canonical must be admitted under M3b canonical output, got: %v", err)
	}
	got := string(src)
	if !strings.Contains(got, `{"body", v.Body}`) || !strings.Contains(got, `{"text", v.Text}`) {
		t.Errorf("expected distinct canonical keys body+text, got:\n%s", got)
	}
	// The alias "text" must NOT double the "text" key: exactly one marshal entry
	// keyed "text" (v.Text), and none keyed "text" for v.Body.
	if strings.Contains(got, `{"text", v.Body}`) {
		t.Errorf("alias leaked: body field emitted under alias key \"text\":\n%s", got)
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

// TestEmitNativeStaticUnaryAliasCanonicalRoundTrip is the required BEHAVIORAL
// proof of the M3b canonical output-key policy (scope §2): it COMPILES and
// EXECUTES the emitted MarshalJSON/UnmarshalJSON and asserts the wire bytes use
// the CANONICAL field names in both directions, even when the fields carry
// exotic aliases ("", "-", a comma) that a struct tag could not express — the
// aliases are ignored and never appear in the bytes.
func TestEmitNativeStaticUnaryAliasCanonicalRoundTrip(t *testing.T) {
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
			strField("empty", ""),     // present empty alias -> IGNORED, canonical "empty"
			strField("dash", "-"),     // "-" alias -> IGNORED, canonical "dash"
			strField("comma", "a,b"),  // comma alias -> IGNORED, canonical "comma"
		},
	}}
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "rtpkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	rt := "package rtpkg\n\n" +
		"import (\n\t\"encoding/json\"\n\t\"reflect\"\n\t\"strings\"\n\t\"testing\"\n)\n\n" +
		"func TestRoundTrip(t *testing.T) {\n" +
		"\tv := OutputReply{Canon: \"c\", Empty: \"e\", Dash: \"d\", Comma: \"m\"}\n" +
		"\tgot, err := json.Marshal(v)\n" +
		"\tif err != nil { t.Fatal(err) }\n" +
		"\tconst want = `{\"canon\":\"c\",\"empty\":\"e\",\"dash\":\"d\",\"comma\":\"m\"}`\n" +
		"\tif string(got) != want { t.Fatalf(\"marshal = %s, want %s\", got, want) }\n" +
		"\tfor _, alias := range []string{`\"-\"`, `\"a,b\"`} {\n" +
		"\t\tif strings.Contains(string(got), alias) { t.Fatalf(\"alias %s leaked into output bytes %s\", alias, got) }\n" +
		"\t}\n" +
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
