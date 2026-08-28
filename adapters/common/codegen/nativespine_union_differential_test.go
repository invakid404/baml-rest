package codegen

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// unionLiteralMethod is an admitted static-unary method whose RETURN class
// exercises the M3b carrier additions in one shape:
//
//   - cross: a CROSS-KIND union `string | int`  -> OutputUnion1
//   - choice: a SAME-BASE string-literal union `"a" | "b"` -> OutputUnion2 (both *string)
//   - flag: a SAME-BASE bool-literal union `true | false` -> OutputUnion3 (both *bool)
//   - maybe: a NULLABLE multi-arm union `(string | int)?` -> *OutputUnion1 (DEDUPES to OutputUnion1)
//   - status: a STANDALONE string literal `"active"` -> plain string
//   - code: a STANDALONE int literal `200` -> plain int64
//   - items: a LIST of the `string | int` union -> []OutputUnion1 (dedupe again)
//
// Every field uses its canonical name (M3b aliases are covered separately). This
// is the descriptor the emitter lowers to the differential carriers.
func unionLiteralMethod() projectdescriptor.Method {
	prim := func(p sd.PrimitiveKind) sd.Type { return sd.Type{Kind: sd.TypePrimitive, Primitive: p} }
	litStr := func(s string) sd.Type {
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralString, String: s}}
	}
	litBool := func(v bool) sd.Type {
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralBool, Bool: v}}
	}
	litInt := func(v int64) sd.Type {
		return sd.Type{Kind: sd.TypeLiteral, Literal: &sd.LiteralValue{Kind: sd.LiteralInt, Int: v}}
	}
	union := func(nullable bool, arms ...sd.Type) sd.Type {
		return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: nullable, Variants: arms}}
	}
	list := func(elem sd.Type) sd.Type { e := elem; return sd.Type{Kind: sd.TypeList, Elem: &e} }

	stringOrInt := []sd.Type{prim(sd.PrimitiveString), prim(sd.PrimitiveInt)}

	return projectdescriptor.Method{
		Name:  "UnionLit",
		Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version,
			Method:  "UnionLit",
			Target:  sd.Type{Kind: sd.TypeClass, Name: "UL"},
			Classes: []sd.ClassDef{{
				Name: sd.Name{Name: "UL"},
				Fields: []sd.ClassField{
					{Name: sd.Name{Name: "cross"}, Type: union(false, stringOrInt...)},
					{Name: sd.Name{Name: "choice"}, Type: union(false, litStr("a"), litStr("b"))},
					{Name: sd.Name{Name: "flag"}, Type: union(false, litBool(true), litBool(false))},
					{Name: sd.Name{Name: "maybe"}, Type: union(true, stringOrInt...)},
					{Name: sd.Name{Name: "status"}, Type: litStr("active")},
					{Name: sd.Name{Name: "code"}, Type: litInt(200)},
					{Name: sd.Name{Name: "items"}, Type: list(union(false, stringOrInt...))},
				},
			}},
		},
	}
}

// unionLiteralTestSource is the hermetic differential harness. It compares the
// emitted M3b carrier against a FROZEN, CFFI-free reference that mirrors BAML
// v0.223's generated types/unions.go JSON methods (module cache:
// engine/generators/languages/go/.../types/unions.go), MINUS CFFI. It asserts:
//
//   - native bytes == frozen golden == reference-carrier bytes (non-map rows);
//   - unmarshal -> remarshal byte-identity for each golden;
//   - constructors select/clear the right arm; an unset union marshal errors;
//   - sequential UnmarshalJSON arm precedence matches BAML, INCLUDING the
//     counterintuitive same-base literal controls (generic JSON `false` decodes
//     into the first bool arm; any JSON string into the first string arm; no
//     value checks);
//   - a standalone literal field is a plain base with NO extra rejection.
const unionLiteralTestSource = `package ulpkg

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bytedance/sonic"
)

// --- Frozen reference carriers: BAML v0.223 generated types/unions.go JSON
// --- methods, verbatim except for the stripped CFFI Decode/Encode/BamlTypeName.

// refUnion1 mirrors a generated Union2StringOrInt (arms in descriptor order:
// string, then int).
type refUnion1 struct {
	variant string

	variant_String *string
	variant_Int    *int64
}

func refUnion1NewString(v string) refUnion1 { return refUnion1{variant: "String", variant_String: &v} }
func refUnion1NewInt(v int64) refUnion1     { return refUnion1{variant: "Int", variant_Int: &v} }

func (u refUnion1) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "String":
		return json.Marshal(u.variant_String)
	case "Int":
		return json.Marshal(u.variant_Int)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnion1) UnmarshalJSON(data []byte) error {
	var err error
	err = json.Unmarshal(data, &u.variant_String)
	if err == nil {
		u.variant = "String"
		return nil
	} else {
		u.variant_String = nil
	}
	err = json.Unmarshal(data, &u.variant_Int)
	if err == nil {
		u.variant = "Int"
		return nil
	} else {
		u.variant_Int = nil
	}
	return fmt.Errorf("invalid union variant: %s", string(data))
}

// refUnion2 mirrors a generated Union2 of string literals "a" | "b" (both *string,
// arm order a, b).
type refUnion2 struct {
	variant string

	variant_A *string
	variant_B *string
}

func refUnion2NewA() refUnion2 { v := "a"; return refUnion2{variant: "A", variant_A: &v} }
func refUnion2NewB() refUnion2 { v := "b"; return refUnion2{variant: "B", variant_B: &v} }

func (u refUnion2) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "A":
		return json.Marshal(u.variant_A)
	case "B":
		return json.Marshal(u.variant_B)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnion2) UnmarshalJSON(data []byte) error {
	var err error
	err = json.Unmarshal(data, &u.variant_A)
	if err == nil {
		u.variant = "A"
		return nil
	} else {
		u.variant_A = nil
	}
	err = json.Unmarshal(data, &u.variant_B)
	if err == nil {
		u.variant = "B"
		return nil
	} else {
		u.variant_B = nil
	}
	return fmt.Errorf("invalid union variant: %s", string(data))
}

// refUnion3 mirrors a generated Union2 of bool literals true | false (both *bool,
// arm order true, false — matching the source declaration order).
type refUnion3 struct {
	variant string

	variant_True  *bool
	variant_False *bool
}

func refUnion3NewTrue() refUnion3  { v := true; return refUnion3{variant: "True", variant_True: &v} }
func refUnion3NewFalse() refUnion3 { v := false; return refUnion3{variant: "False", variant_False: &v} }

func (u refUnion3) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "True":
		return json.Marshal(u.variant_True)
	case "False":
		return json.Marshal(u.variant_False)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnion3) UnmarshalJSON(data []byte) error {
	var err error
	err = json.Unmarshal(data, &u.variant_True)
	if err == nil {
		u.variant = "True"
		return nil
	} else {
		u.variant_True = nil
	}
	err = json.Unmarshal(data, &u.variant_False)
	if err == nil {
		u.variant = "False"
		return nil
	} else {
		u.variant_False = nil
	}
	return fmt.Errorf("invalid union variant: %s", string(data))
}

// refUL mirrors the shape a BAML-generated Go class has: a plain struct with json
// tags in declaration order; the union fields are the reference carriers above.
type refUL struct {
	Cross  refUnion1   ` + "`json:\"cross\"`" + `
	Choice refUnion2   ` + "`json:\"choice\"`" + `
	Flag   refUnion3   ` + "`json:\"flag\"`" + `
	Maybe  *refUnion1  ` + "`json:\"maybe\"`" + `
	Status string      ` + "`json:\"status\"`" + `
	Code   int64       ` + "`json:\"code\"`" + `
	Items  []refUnion1 ` + "`json:\"items\"`" + `
}

func nativeValue(maybe *OutputUnion1) OutputUl {
	return OutputUl{
		Cross:  OutputUnion1NewVariant0("hi"),
		Choice: OutputUnion2NewVariant0(),
		Flag:   OutputUnion3NewVariant1(),
		Maybe:  maybe,
		Status: "active",
		Code:   200,
		Items:  []OutputUnion1{OutputUnion1NewVariant1(7), OutputUnion1NewVariant0("x")},
	}
}
func refValue(maybe *refUnion1) refUL {
	return refUL{
		Cross:  refUnion1NewString("hi"),
		Choice: refUnion2NewA(),
		Flag:   refUnion3NewFalse(),
		Maybe:  maybe,
		Status: "active",
		Code:   200,
		Items:  []refUnion1{refUnion1NewInt(7), refUnion1NewString("x")},
	}
}

const goldenNilMaybe = ` + "`{\"cross\":\"hi\",\"choice\":\"a\",\"flag\":false,\"maybe\":null,\"status\":\"active\",\"code\":200,\"items\":[7,\"x\"]}`" + `
const goldenPresentMaybe = ` + "`{\"cross\":\"hi\",\"choice\":\"a\",\"flag\":false,\"maybe\":42,\"status\":\"active\",\"code\":200,\"items\":[7,\"x\"]}`" + `

// TestUnionDifferential marshals both carriers with the SERVING serializer (sonic
// ConfigDefault) and asserts native == golden == reference, then round-trips.
func TestUnionDifferential(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nativeMaybe  *OutputUnion1
		refMaybe     *refUnion1
		golden       string
	}{
		{"nil maybe", nil, nil, goldenNilMaybe},
		{"present maybe", func() *OutputUnion1 { u := OutputUnion1NewVariant1(42); return &u }(),
			func() *refUnion1 { u := refUnion1NewInt(42); return &u }(), goldenPresentMaybe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, err := sonic.Marshal(nativeValue(tc.nativeMaybe))
			if err != nil {
				t.Fatal(err)
			}
			if string(native) != tc.golden {
				t.Fatalf("native carrier JSON != golden\n native: %s\n golden: %s", native, tc.golden)
			}
			ref, err := sonic.Marshal(refValue(tc.refMaybe))
			if err != nil {
				t.Fatal(err)
			}
			if string(ref) != string(native) {
				t.Fatalf("native carrier JSON != reference carrier JSON\n native: %s\n ref:    %s", native, ref)
			}
			// Round-trip: golden -> native carrier -> re-marshal -> byte-identical.
			var back OutputUl
			if err := json.Unmarshal([]byte(tc.golden), &back); err != nil {
				t.Fatal(err)
			}
			again, err := sonic.Marshal(back)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != tc.golden {
				t.Fatalf("round-trip not byte-identical\n got:    %s\n golden: %s", again, tc.golden)
			}
		})
	}
}

// TestUnionConstructorsSelectClear proves a constructor selects exactly its arm
// and a setter clears the others (accessors return nil for the non-selected arm).
func TestUnionConstructorsSelectClear(t *testing.T) {
	u := OutputUnion1NewVariant0("hi")
	if !u.IsVariant0() || u.IsVariant1() {
		t.Fatalf("NewVariant0: discriminator wrong")
	}
	if got := u.AsVariant0(); got == nil || *got != "hi" {
		t.Fatalf("AsVariant0 = %v", got)
	}
	if u.AsVariant1() != nil {
		t.Fatalf("AsVariant1 should be nil for a variant0 value")
	}
	u.SetVariant1(9)
	if !u.IsVariant1() || u.IsVariant0() {
		t.Fatalf("SetVariant1: discriminator wrong")
	}
	if u.AsVariant0() != nil {
		t.Fatalf("SetVariant1 must clear the variant0 pointer")
	}
	if got := u.AsVariant1(); got == nil || *got != 9 {
		t.Fatalf("AsVariant1 = %v", got)
	}
}

// TestUnsetUnionMarshalErrors proves an unset union errors on marshal, both bare
// and as a class field (BAML parity: an unset union does not serialize).
func TestUnsetUnionMarshalErrors(t *testing.T) {
	var bare OutputUnion1
	if _, err := json.Marshal(bare); err == nil {
		t.Fatal("bare unset union marshaled without error")
	}
	if _, err := sonic.Marshal(OutputUl{}); err == nil {
		t.Fatal("class with an unset union field marshaled without error")
	}
}

// TestUnionCrossKindPrecedence proves cross-kind arm selection: a JSON number
// falls through the string arm to the int arm; a JSON string binds the string arm.
func TestUnionCrossKindPrecedence(t *testing.T) {
	var n OutputUnion1
	if err := json.Unmarshal([]byte("7"), &n); err != nil {
		t.Fatal(err)
	}
	if !n.IsVariant1() || n.AsVariant1() == nil || *n.AsVariant1() != 7 {
		t.Fatalf("JSON 7 should bind the int arm; got variant0=%v variant1=%v", n.AsVariant0(), n.AsVariant1())
	}
	var s OutputUnion1
	if err := json.Unmarshal([]byte(` + "`\"z\"`" + `), &s); err != nil {
		t.Fatal(err)
	}
	if !s.IsVariant0() || s.AsVariant0() == nil || *s.AsVariant0() != "z" {
		t.Fatalf("JSON \"z\" should bind the string arm; got %v", s)
	}
	// Reference agrees.
	var r refUnion1
	if err := json.Unmarshal([]byte("7"), &r); err != nil || r.variant != "Int" {
		t.Fatalf("reference JSON 7 -> variant %q, err %v", r.variant, err)
	}
}

// TestSameBaseLiteralAmbiguity PINS the counterintuitive first-success selection
// for same-base literal arms (do NOT "improve" this): any JSON string decodes into
// the FIRST string arm regardless of value, and generic JSON false decodes into the
// FIRST bool arm. No value checks. Native and the BAML-equivalent reference agree.
func TestSameBaseLiteralAmbiguity(t *testing.T) {
	// "a" | "b": JSON "b" (and even a non-literal "zzz") lands in the FIRST arm.
	for _, in := range []string{` + "`\"b\"`" + `, ` + "`\"zzz\"`" + `} {
		var u OutputUnion2
		if err := json.Unmarshal([]byte(in), &u); err != nil {
			t.Fatal(err)
		}
		if !u.IsVariant0() || u.IsVariant1() {
			t.Fatalf("string %s should bind the FIRST string arm (v0), got v0=%v v1=%v", in, u.AsVariant0(), u.AsVariant1())
		}
	}
	var r2 refUnion2
	if err := json.Unmarshal([]byte(` + "`\"b\"`" + `), &r2); err != nil || r2.variant != "A" {
		t.Fatalf("reference \"b\" -> variant %q (want first arm A), err %v", r2.variant, err)
	}

	// true | false: JSON false decodes into the FIRST (true-declared) bool arm.
	var b OutputUnion3
	if err := json.Unmarshal([]byte("false"), &b); err != nil {
		t.Fatal(err)
	}
	if !b.IsVariant0() || b.AsVariant0() == nil || *b.AsVariant0() != false {
		t.Fatalf("JSON false should bind the FIRST bool arm (v0) holding false, got %v", b)
	}
	var r3 refUnion3
	if err := json.Unmarshal([]byte("false"), &r3); err != nil || r3.variant != "True" {
		t.Fatalf("reference false -> variant %q (want first arm True), err %v", r3.variant, err)
	}
}

// TestStandaloneLiteralNoValidation proves a standalone literal field is a plain
// Go base with NO value validation (BAML v0.223 generated classes do not enforce
// literal values in ordinary json.Unmarshal).
func TestStandaloneLiteralNoValidation(t *testing.T) {
	const in = ` + "`{\"cross\":\"hi\",\"choice\":\"a\",\"flag\":true,\"maybe\":null,\"status\":\"anything\",\"code\":999,\"items\":[]}`" + `
	var v OutputUl
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("standalone literal field rejected an out-of-literal value: %v", err)
	}
	if v.Status != "anything" || v.Code != 999 {
		t.Fatalf("standalone literals not plain bases: status=%q code=%d", v.Status, v.Code)
	}
}
`

// TestNativeUnionLiteralDifferential is the M3b JSON-roundtrip differential: the
// emitted union/literal carrier serializes byte-identically to BAML v0.223's
// generated-carrier behavior, compiles in a hermetic module with NO
// baml_client/CFFI import, and is deterministic.
func TestNativeUnionLiteralDifferential(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping union/literal differential")
	}
	m := unionLiteralMethod()
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "ulpkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Determinism: a second emit is byte-identical.
	src2, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "ulpkg"})
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != string(src2) {
		t.Fatal("emitter is not deterministic for the union/literal carrier")
	}
	// The emitted carrier must carry the planned union types and the standalone
	// literal bases (dedupe: exactly OutputUnion1/2/3, no OutputUnion4).
	// Alignment-insensitive checks (gofmt pads struct fields): assert standalone
	// type expressions and constructor signatures rather than field-name/type pairs.
	for _, want := range []string{
		"type OutputUnion1 struct",
		"type OutputUnion2 struct",
		"type OutputUnion3 struct",
		"*OutputUnion1",                               // nullable multi-arm union -> pointer to the DEDUPED carrier
		"[]OutputUnion1",                              // list of the deduped carrier
		"func OutputUnion2NewVariant0() OutputUnion2", // literal-arm constructor takes NO value
		`v := string("a")`,                            // and installs the literal constant
		`v := bool(false)`,                            // bool literal arm constant
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("emitted carrier missing %q", want)
		}
	}
	if strings.Contains(string(src), "type OutputUnion4 struct") {
		t.Error("union dedupe failed: a 4th carrier was emitted for a repeated union shape")
	}

	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, string(src), map[string]string{"union_differential_test.go": unionLiteralTestSource})

	// No-CFFI proof on the emitted carrier's non-test dependency graph.
	assertNoCFFI(t, tmp)

	if out, err := testharness.RunGoTest(t, tmp, "TestUnionDifferential|TestUnionConstructorsSelectClear|TestUnsetUnionMarshalErrors|TestUnionCrossKindPrecedence|TestSameBaseLiteralAmbiguity|TestStandaloneLiteralNoValidation"); err != nil {
		t.Fatalf("union/literal differential failed: %v\n%s", err, out)
	}
}
