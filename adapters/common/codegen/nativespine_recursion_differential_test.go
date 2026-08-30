package codegen

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// nativespine_recursion_differential_test.go is the M3c JSON differential: the
// emitted RECURSIVE carriers (recursive classes, TypeRecursiveAlias declarations,
// pure-container structural aliases) serialize byte-identically to BAML v0.223's
// generated carriers, compile in a hermetic module with NO baml_client/CFFI, and
// are deterministic. It also pins the two M3c negative controls: a direct
// by-value class SCC is DECLINED (never emitted), and a user-built pointer cycle
// returns a MARSHAL ERROR (never a stack overflow) on both the native carrier and
// the BAML-equivalent reference.

// --- small builders shared by the recursion fixtures ---

func recPrim(p sd.PrimitiveKind) sd.Type { return sd.Type{Kind: sd.TypePrimitive, Primitive: p} }
func recAlias(n string) sd.Type {
	return sd.Type{Kind: sd.TypeRecursiveAlias, Name: n, Mode: sd.NonStreaming}
}
func recClass(n string) sd.Type       { return sd.Type{Kind: sd.TypeClass, Name: n} }
func recList(e sd.Type) sd.Type       { c := e; return sd.Type{Kind: sd.TypeList, Elem: &c} }
func recStrMap(v sd.Type) sd.Type {
	k := recPrim(sd.PrimitiveString)
	vv := v
	return sd.Type{Kind: sd.TypeMap, Key: &k, Value: &vv}
}
func recOpt(inner sd.Type) sd.Type {
	return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: true, Variants: []sd.Type{inner}}}
}
func recUnion(nullable bool, arms ...sd.Type) sd.Type {
	return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: nullable, Variants: arms}}
}

// recClassMethod returns a method whose output class exercises the recursive
// CLASS shapes: self-optional (`Next *OutputNode`), self-list (`Children
// []OutputNode`), self-map (`ByName map[string]OutputNode`), and recursion
// through a multi-arm union (`Through.value string | Through`, whose recursive
// arm is `*OutputThrough`).
func recClassMethod() projectdescriptor.Method {
	return projectdescriptor.Method{
		Name:  "RecClass",
		Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version, Method: "RecClass", Target: recClass("Node"),
			Classes: []sd.ClassDef{
				{Name: sd.Name{Name: "Node"}, Fields: []sd.ClassField{
					{Name: sd.Name{Name: "value"}, Type: recPrim(sd.PrimitiveString)},
					{Name: sd.Name{Name: "next"}, Type: recOpt(recClass("Node"))},
					{Name: sd.Name{Name: "children"}, Type: recList(recClass("Node"))},
					{Name: sd.Name{Name: "byName"}, Type: recStrMap(recClass("Node"))},
					{Name: sd.Name{Name: "through"}, Type: recOpt(recClass("Through"))},
				}},
				{Name: sd.Name{Name: "Through"}, Fields: []sd.ClassField{
					{Name: sd.Name{Name: "value"}, Type: recUnion(false, recPrim(sd.PrimitiveString), recClass("Through"))},
				}},
			},
			RecursiveClasses: []string{"Node", "Through"},
		},
	}
}

// recAliasMethod returns a method whose output class exercises the structural
// recursive ALIAS shapes: Recursive1 (`int | Recursive1[]` -> a value union
// carrier), JSONValue (nullable `string|int|float|bool|JSONValue[]|map<string,
// JSONValue>` -> a pointer-to-union alias), and the two pure-container `any`
// fallbacks ListNode (`ListNode[]` -> []any) and StrMap (`map<string,StrMap>` ->
// map[string]any). Four disjoint single-alias cycles in one bundle prove ordered
// emission.
func recAliasMethod() projectdescriptor.Method {
	return projectdescriptor.Method{
		Name:  "RecAlias",
		Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version, Method: "RecAlias", Target: recClass("AW"),
			Classes: []sd.ClassDef{{Name: sd.Name{Name: "AW"}, Fields: []sd.ClassField{
				{Name: sd.Name{Name: "rec1"}, Type: recAlias("Recursive1")},
				{Name: sd.Name{Name: "doc"}, Type: recAlias("JSONValue")},
				{Name: sd.Name{Name: "nodes"}, Type: recAlias("ListNode")},
				{Name: sd.Name{Name: "dict"}, Type: recAlias("StrMap")},
			}}},
			StructuralRecursiveAliases: []sd.RecursiveAliasDef{
				{Name: "Recursive1", Target: recUnion(false, recPrim(sd.PrimitiveInt), recList(recAlias("Recursive1")))},
				{Name: "JSONValue", Target: recUnion(true,
					recPrim(sd.PrimitiveString), recPrim(sd.PrimitiveInt), recPrim(sd.PrimitiveFloat), recPrim(sd.PrimitiveBool),
					recList(recAlias("JSONValue")), recStrMap(recAlias("JSONValue")),
				)},
				{Name: "ListNode", Target: recList(recAlias("ListNode"))},
				{Name: "StrMap", Target: recStrMap(recAlias("StrMap"))},
			},
		},
	}
}

// TestNativeRecursiveClassDifferential emits the recursive-class carrier, checks
// determinism + key emission shapes, then compiles+runs the JSON behavior against
// a frozen CFFI-free reference in a hermetic module.
func TestNativeRecursiveClassDifferential(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping recursive-class differential")
	}
	m := recClassMethod()
	src := emitDeterministic(t, m, "recpkg")

	for _, want := range []string{
		"type OutputNode struct",
		"Next     *OutputNode",              // self-optional preserves the pointer
		"Children []OutputNode",             // self-list is a value slice, NOT pointerized
		"ByName   map[string]OutputNode",    // self-map is a value map
		"type OutputThrough struct",
		"Value OutputUnion1",                // recursion through a value union
		"variant1 *OutputThrough",           // the union's recursive arm carries the pointer
		"nativeSpineCheckAcyclic(v)",        // recursion-safe class marshal guard
		"nativeSpineCheckAcyclic(u)",        // recursion-safe union marshal guard
		`"reflect"`,                         // guard imports reflect
	} {
		if !strings.Contains(src, want) {
			t.Errorf("emitted recursive-class carrier missing %q", want)
		}
	}

	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, src, map[string]string{"recursion_class_test.go": recClassTestSource})
	assertNoCFFI(t, tmp)
	if out, err := testharness.RunGoTest(t, tmp, "TestRecClass|TestRecThroughUnion|TestRecCycleMarshalErrors"); err != nil {
		t.Fatalf("recursive-class differential failed: %v\n%s", err, out)
	}
}

// TestNativeRecursiveAliasDifferential emits the recursive-alias carrier and runs
// its JSON behavior (scalar/list/map arms, nullable-alias null, first-success
// unmarshal, and the pure-container `any` fallback).
func TestNativeRecursiveAliasDifferential(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping recursive-alias differential")
	}
	m := recAliasMethod()
	src := emitDeterministic(t, m, "recaliaspkg")

	for _, want := range []string{
		"type OutputRecursive1 = OutputUnion1",     // alias to a value union carrier
		"variant1 *[]OutputRecursive1",             // Recursive1's recursive list arm
		"type OutputJsonValue = *OutputUnion2",     // nullable alias -> pointer to union
		"variant4 *[]OutputJsonValue",              // JSONValue's recursive list arm
		"variant5 *map[string]OutputJsonValue",     // JSONValue's recursive map arm
		"type OutputListNode = []any",              // pure-container fallback (list)
		"type OutputStrMap = map[string]any",       // pure-container fallback (map)
	} {
		if !strings.Contains(src, want) {
			t.Errorf("emitted recursive-alias carrier missing %q", want)
		}
	}
	// No invalid self-referential Go alias must ever be emitted.
	for _, bad := range []string{"type OutputListNode = []OutputListNode", "type OutputStrMap = map[string]OutputStrMap"} {
		if strings.Contains(src, bad) {
			t.Errorf("emitted an uncompilable self-referential Go alias %q", bad)
		}
	}

	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, src, map[string]string{"recursion_alias_test.go": recAliasTestSource})
	assertNoCFFI(t, tmp)
	if out, err := testharness.RunGoTest(t, tmp, "TestRecAlias"); err != nil {
		t.Fatalf("recursive-alias differential failed: %v\n%s", err, out)
	}
}

// emitDeterministic emits m twice and requires byte-identical source, returning it.
func emitDeterministic(t *testing.T, m projectdescriptor.Method, pkg string) string {
	t.Helper()
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: pkg})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	src2, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != string(src2) {
		t.Fatal("emitter is not deterministic for the recursive carrier")
	}
	return string(src)
}

// TestNativeRecursiveDirectCycleDeclined pins negative control (2): a direct
// by-value class SCC (self or mutual) is what stock v0.223 rejects as a class
// dependency cycle, so the emitter (and the shared carrier-shape gate) DECLINE it
// rather than emit an uncompilable `type A struct { ... A ... }`. M2's permissive
// descriptor builder can synthesize such a bundle, so the direct-entry emitter
// must catch it.
func TestNativeRecursiveDirectCycleDeclined(t *testing.T) {
	selfCycle := sd.Bundle{
		Version: sd.Version, Method: "SelfCycle", Target: recClass("C"),
		Classes: []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: []sd.ClassField{
			{Name: sd.Name{Name: "self"}, Type: recClass("C")},
		}}},
		RecursiveClasses: []string{"C"},
	}
	mutualCycle := sd.Bundle{
		Version: sd.Version, Method: "Mutual", Target: recClass("A"),
		Classes: []sd.ClassDef{
			{Name: sd.Name{Name: "A"}, Fields: []sd.ClassField{{Name: sd.Name{Name: "b"}, Type: recClass("B")}}},
			{Name: sd.Name{Name: "B"}, Fields: []sd.ClassField{{Name: sd.Name{Name: "a"}, Type: recClass("A")}}},
		},
		RecursiveClasses: []string{"A", "B"},
	}
	for _, tc := range []struct {
		name   string
		bundle sd.Bundle
	}{
		{"self", selfCycle},
		{"mutual", mutualCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Shared gate rejects it.
			if err := CheckNativeCarrierShape(tc.bundle); err == nil {
				t.Fatal("CheckNativeCarrierShape admitted a direct by-value class cycle")
			}
			// The direct emitter's backstop rejects it (never emits uncompilable Go).
			_, err := EmitNativeStaticUnary(projectdescriptor.Method{
				Name: tc.bundle.Method, Class: projectdescriptor.ClassStaticUnary, Return: tc.bundle,
			}, NativeSpineOptions{PackageName: "cyclepkg"})
			if err == nil {
				t.Fatal("EmitNativeStaticUnary emitted a direct by-value class cycle")
			}
			if !strings.Contains(err.Error(), "dependency cycle") {
				t.Fatalf("decline reason %q does not mention a dependency cycle", err.Error())
			}
		})
	}
}

// TestNativeRecursiveNegativeControls pins the decline boundary the SHARED gate
// must own. Each case is a bundle stock v0.223 rejects or M3c cannot faithfully
// emit; before the shared gate owned the whole boundary each passed admission and
// either compile-failed or emitted a wrong/metadata-dropped carrier. Both the
// gate AND the direct emitter backstop must decline every one.
func TestNativeRecursiveNegativeControls(t *testing.T) {
	classC := func(fields ...sd.ClassField) []sd.ClassDef {
		return []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: fields}}
	}
	cases := []struct {
		name   string
		bundle sd.Bundle
		reason string // substring expected in the decline error
	}{
		{
			// Multi-alias SCC A=[]B, B=[]A -> emits `type OutputA=[]OutputB` /
			// `type OutputB=[]OutputA`, an invalid recursive Go alias cycle.
			name: "multi-alias SCC",
			bundle: sd.Bundle{
				Version: sd.Version, Target: recAlias("A"),
				StructuralRecursiveAliases: []sd.RecursiveAliasDef{
					{Name: "A", Target: recList(recAlias("B"))},
					{Name: "B", Target: recList(recAlias("A"))},
				},
			},
			reason: "single-alias structural cycle",
		},
		{
			// Non-cyclic entry wrongly in the structural-alias table.
			name: "non-cyclic structural alias entry",
			bundle: sd.Bundle{
				Version: sd.Version, Target: recAlias("A"),
				StructuralRecursiveAliases: []sd.RecursiveAliasDef{
					{Name: "A", Target: recList(recPrim(sd.PrimitiveInt))},
				},
			},
			reason: "does not reference itself",
		},
		{
			// TypeClass reference to a name declared only as an ENUM -> would emit a
			// carrier binding the enum's Go type instead of declining.
			name: "wrong-kind ref (class names an enum)",
			bundle: sd.Bundle{
				Version: sd.Version, Target: sd.Type{Kind: sd.TypeClass, Name: "E"},
				Enums: []sd.EnumDef{{Name: sd.Name{Name: "E"}, Values: []sd.EnumValue{{Name: sd.Name{Name: "X"}}}}},
			},
			reason: "undeclared class",
		},
		{
			// Streaming-mode class reference against a non-streaming declaration.
			name: "streaming-mode ref",
			bundle: sd.Bundle{
				Version: sd.Version, Target: sd.Type{Kind: sd.TypeClass, Name: "C", Mode: sd.Streaming},
				Classes: classC(sd.ClassField{Name: sd.Name{Name: "x"}, Type: recPrim(sd.PrimitiveString)}),
			},
			reason: "streaming",
		},
		{
			// @stream.* streaming behavior on a field type (silently dropped pre-fix).
			name: "field streaming behavior",
			bundle: sd.Bundle{
				Version: sd.Version, Target: recClass("C"),
				Classes: classC(sd.ClassField{Name: sd.Name{Name: "x"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveString, Meta: sd.TypeMeta{Stream: sd.StreamingBehavior{Done: true}}}}),
			},
			reason: "streaming",
		},
		{
			// ClassField.StreamingNeeded (silently dropped pre-fix).
			name: "field StreamingNeeded",
			bundle: sd.Bundle{
				Version: sd.Version, Target: recClass("C"),
				Classes: classC(sd.ClassField{Name: sd.Name{Name: "x"}, Type: recPrim(sd.PrimitiveString), StreamingNeeded: true}),
			},
			reason: "stream",
		},
		{
			// @check under a class (the EMITTER backstop must decline it, not emit a
			// carrier that drops the constraint).
			name: "check constraint on a field type",
			bundle: sd.Bundle{
				Version: sd.Version, Target: recClass("C"),
				Classes: classC(sd.ClassField{Name: sd.Name{Name: "x"}, Type: sd.Type{Kind: sd.TypePrimitive, Primitive: sd.PrimitiveInt, Meta: sd.TypeMeta{Constraints: []sd.Constraint{{Level: sd.ConstraintCheck, Expression: "this > 0"}}}}}),
			},
			reason: "check",
		},
		{
			// @@dynamic type (the EMITTER backstop must decline it).
			name: "dynamic type",
			bundle: sd.Bundle{
				Version: sd.Version, Target: sd.Type{Kind: sd.TypeClass, Name: "C", Dynamic: true},
				Classes: classC(sd.ClassField{Name: sd.Name{Name: "x"}, Type: recPrim(sd.PrimitiveString)}),
			},
			reason: "dynamic",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNativeCarrierShape(tc.bundle)
			if err == nil {
				t.Fatalf("CheckNativeCarrierShape admitted %s (must decline)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("decline reason %q does not mention %q", err.Error(), tc.reason)
			}
			// The direct emitter backstop must decline the same shape.
			if _, emitErr := EmitNativeStaticUnary(projectdescriptor.Method{
				Name: "Neg", Class: projectdescriptor.ClassStaticUnary, Return: tc.bundle,
			}, NativeSpineOptions{PackageName: "negpkg"}); emitErr == nil {
				t.Fatalf("EmitNativeStaticUnary emitted %s (must decline)", tc.name)
			}
		})
	}
}

// TestNativeRecursiveGuardDerivedFromGraph pins finding 4: cycle-guard gating is
// derived STRUCTURALLY from the lowered graph, never trusted from the descriptor's
// recursion metadata. Missing metadata on a truly-recursive class must STILL emit
// the guard (no stack overflow); stray metadata on an acyclic bundle must NOT
// inject the guard (byte-unchanged non-recursive invariant).
func TestNativeRecursiveGuardDerivedFromGraph(t *testing.T) {
	// Truly recursive class with EMPTY RecursiveClasses metadata -> guard required.
	missing := recClassMethod()
	missing.Return.RecursiveClasses = nil
	src, err := EmitNativeStaticUnary(missing, NativeSpineOptions{PackageName: "gpkg"})
	if err != nil {
		t.Fatalf("emit (missing metadata): %v", err)
	}
	if !strings.Contains(string(src), "nativeSpineCheckAcyclic") {
		t.Fatal("a truly-recursive class with missing RecursiveClasses metadata emitted NO cycle guard (would stack-overflow on a user pointer cycle)")
	}

	// Acyclic bundle with STRAY RecursiveClasses metadata -> no guard, no reflect.
	stray := projectdescriptor.Method{
		Name: "Acyclic", Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version, Target: recClass("C"),
			Classes:          []sd.ClassDef{{Name: sd.Name{Name: "C"}, Fields: []sd.ClassField{{Name: sd.Name{Name: "x"}, Type: recPrim(sd.PrimitiveString)}}}},
			RecursiveClasses: []string{"C"}, // stray/inconsistent metadata
		},
	}
	src2, err := EmitNativeStaticUnary(stray, NativeSpineOptions{PackageName: "gpkg"})
	if err != nil {
		t.Fatalf("emit (stray metadata): %v", err)
	}
	if strings.Contains(string(src2), "nativeSpineCheckAcyclic") {
		t.Fatal("an acyclic bundle with stray RecursiveClasses metadata injected the cycle guard (breaks the byte-unchanged non-recursive invariant)")
	}
	if strings.Contains(string(src2), `"reflect"`) {
		t.Fatal("an acyclic bundle imported reflect for an unneeded cycle guard")
	}
}

// recMutualMethod is a mutual-class graph broken by a NULLABLE edge (A.b B?, B.a
// A): B->A is a value edge, A->B is a pointer (nullable), so the Go size is finite
// and it is ADMITTED. `A.b` is `*OutputB`; `B.a` is a value `OutputA`.
func recMutualMethod() projectdescriptor.Method {
	return projectdescriptor.Method{
		Name: "Mutual", Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version, Target: recClass("A"),
			Classes: []sd.ClassDef{
				{Name: sd.Name{Name: "A"}, Fields: []sd.ClassField{
					{Name: sd.Name{Name: "id"}, Type: recPrim(sd.PrimitiveString)},
					{Name: sd.Name{Name: "b"}, Type: recOpt(recClass("B"))},
				}},
				{Name: sd.Name{Name: "B"}, Fields: []sd.ClassField{
					{Name: sd.Name{Name: "a"}, Type: recClass("A")},
				}},
			},
			RecursiveClasses: []string{"A", "B"},
		},
	}
}

// TestNativeRecursiveMutualClassAdmitted pins the positive pointer-broken mutual
// class: admitted, `A.b` is `*OutputB` (pointer breaks the size cycle), `B.a` is a
// VALUE `OutputA`, the guard is emitted (it is genuinely recursive), and it
// compiles + round-trips.
func TestNativeRecursiveMutualClassAdmitted(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping mutual-class differential")
	}
	m := recMutualMethod()
	if err := CheckNativeCarrierShape(m.Return); err != nil {
		t.Fatalf("pointer-broken mutual class wrongly declined: %v", err)
	}
	src := emitDeterministic(t, m, "mutualpkg")
	// Alignment-insensitive (gofmt pads struct fields): `A.b` is a pointer to
	// OutputB (nullable edge breaks the size cycle), `B.a` is a value OutputA.
	for _, want := range []string{"*OutputB", "A OutputA", "nativeSpineCheckAcyclic(v)"} {
		if !strings.Contains(src, want) {
			t.Errorf("mutual carrier missing %q", want)
		}
	}
	if strings.Contains(src, "A *OutputA") {
		t.Error("B.a was wrongly pointerized (should be a value OutputA)")
	}
	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, src, map[string]string{"mutual_test.go": recMutualTestSource})
	assertNoCFFI(t, tmp)
	if out, err := testharness.RunGoTest(t, tmp, "TestRecMutual"); err != nil {
		t.Fatalf("mutual-class differential failed: %v\n%s", err, out)
	}
}

const recMutualTestSource = `package mutualpkg

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
)

func TestRecMutual(t *testing.T) {
	v := OutputA{Id: "a0", B: &OutputB{A: OutputA{Id: "a1"}}}
	b, err := sonic.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "{\"id\":\"a0\",\"b\":{\"a\":{\"id\":\"a1\",\"b\":null}}}"
	if string(b) != golden {
		t.Fatalf("mutual JSON != golden\n got:    %s\n golden: %s", b, golden)
	}
	var back OutputA
	if err := json.Unmarshal([]byte(golden), &back); err != nil {
		t.Fatal(err)
	}
	if back.B == nil || back.B.A.Id != "a1" {
		t.Fatalf("mutual unmarshal lost structure: %+v", back)
	}
	again, err := sonic.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != golden {
		t.Fatalf("mutual round-trip not byte-identical: %s", again)
	}
}
`

// TestNativeRecursiveBareAliasReturnsCompile pins that a method returning a bare
// structural recursive alias (no wrapping class/enum) emits a compiling carrier —
// the import gating differs from the class case (no bytes/no object codec; a
// union-only alias emits encoding/json + the reflect guard; a pure-container
// alias emits neither). A regression here is an unused-import compile error.
func TestNativeRecursiveBareAliasReturnsCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping bare-alias compile check")
	}
	mk := func(name string, target sd.Type, aliases []sd.RecursiveAliasDef) projectdescriptor.Method {
		return projectdescriptor.Method{Name: name, Class: projectdescriptor.ClassStaticUnary,
			Return: sd.Bundle{Version: sd.Version, Method: name, Target: target, StructuralRecursiveAliases: aliases}}
	}
	cases := []struct {
		name   string
		method projectdescriptor.Method
	}{
		{"pure-container", mk("ListNodeR", recAlias("ListNode"),
			[]sd.RecursiveAliasDef{{Name: "ListNode", Target: recList(recAlias("ListNode"))}})},
		{"union-value", mk("Rec1R", recAlias("Recursive1"),
			[]sd.RecursiveAliasDef{{Name: "Recursive1", Target: recUnion(false, recPrim(sd.PrimitiveInt), recList(recAlias("Recursive1")))}})},
		{"nullable-union", mk("JsonR", recAlias("JSONValue"),
			[]sd.RecursiveAliasDef{{Name: "JSONValue", Target: recUnion(true, recPrim(sd.PrimitiveString), recList(recAlias("JSONValue")))}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := EmitNativeStaticUnary(tc.method, NativeSpineOptions{PackageName: "barepkg"})
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			tmp := t.TempDir()
			testharness.WriteTempModule(t, tmp, string(src), nil)
			assertNoCFFI(t, tmp)
			testharness.RunGoBuild(t, tmp)
		})
	}
}

// --- Frozen CFFI-free reference carriers + behavioral tests (compiled in the
// --- hermetic module). JSON goldens are written as escaped Go string literals so
// --- they survive this outer raw-string wrapper unchanged.

const recClassTestSource = `package recpkg

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

// refNode/refThrough mirror the shape a BAML-generated Go type has: a plain
// struct with json tags (no custom MarshalJSON), optional/list/map with no
// omitempty; refUnion mirrors the generated union carrier JSON methods.
type refNode struct {
	Value    string             ` + "`json:\"value\"`" + `
	Next     *refNode           ` + "`json:\"next\"`" + `
	Children []refNode          ` + "`json:\"children\"`" + `
	ByName   map[string]refNode ` + "`json:\"byName\"`" + `
	Through  *refThrough        ` + "`json:\"through\"`" + `
}

type refThrough struct {
	Value refUnion ` + "`json:\"value\"`" + `
}

// refUnion mirrors a generated Union2StringOrThrough (arms: string, then Through).
type refUnion struct {
	variant string

	varString  *string
	varThrough *refThrough
}

func refUnionString(v string) refUnion       { return refUnion{variant: "String", varString: &v} }
func refUnionThrough(v refThrough) refUnion  { return refUnion{variant: "Through", varThrough: &v} }

func (u refUnion) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "String":
		return json.Marshal(u.varString)
	case "Through":
		return json.Marshal(u.varThrough)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnion) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &u.varString); err == nil {
		u.variant = "String"
		return nil
	}
	u.varString = nil
	if err := json.Unmarshal(data, &u.varThrough); err == nil {
		u.variant = "Through"
		return nil
	}
	u.varThrough = nil
	return fmt.Errorf("no union variant matched: %s", string(data))
}

// nativeNode / refNodeValue build the SAME recursive value on each carrier: a
// depth-2 graph exercising nil (next=null), empty ([]), populated list, and a
// single-key map (byte-deterministic).
func nativeNode() OutputNode {
	return OutputNode{
		Value:    "root",
		Next:     &OutputNode{Value: "n1", Children: []OutputNode{}},
		Children: []OutputNode{{Value: "c0"}},
		ByName:   map[string]OutputNode{"k": {Value: "kv"}},
	}
}
func refNodeValue() refNode {
	return refNode{
		Value:    "root",
		Next:     &refNode{Value: "n1", Children: []refNode{}},
		Children: []refNode{{Value: "c0"}},
		ByName:   map[string]refNode{"k": {Value: "kv"}},
	}
}

const goldenNode = "{\"value\":\"root\",\"next\":{\"value\":\"n1\",\"next\":null,\"children\":[],\"byName\":null,\"through\":null},\"children\":[{\"value\":\"c0\",\"next\":null,\"children\":null,\"byName\":null,\"through\":null}],\"byName\":{\"k\":{\"value\":\"kv\",\"next\":null,\"children\":null,\"byName\":null,\"through\":null}},\"through\":null}"

func TestRecClass(t *testing.T) {
	native, err := sonic.Marshal(nativeNode())
	if err != nil {
		t.Fatal(err)
	}
	if string(native) != goldenNode {
		t.Fatalf("native recursive-class JSON != golden\n native: %s\n golden: %s", native, goldenNode)
	}
	ref, err := sonic.Marshal(refNodeValue())
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != string(native) {
		t.Fatalf("native != BAML-equivalent reference\n native: %s\n ref:    %s", native, ref)
	}
	// Round-trip: golden -> native -> re-marshal -> byte-identical (recursive unmarshal).
	var back OutputNode
	if err := json.Unmarshal([]byte(goldenNode), &back); err != nil {
		t.Fatal(err)
	}
	again, err := sonic.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != goldenNode {
		t.Fatalf("recursive round-trip not byte-identical\n got:    %s\n golden: %s", again, goldenNode)
	}

	// Depth-0 (all recursive edges nil/absent): next=null, children=null, byName=null.
	d0, err := sonic.Marshal(OutputNode{Value: "leaf"})
	if err != nil {
		t.Fatal(err)
	}
	const goldenLeaf = "{\"value\":\"leaf\",\"next\":null,\"children\":null,\"byName\":null,\"through\":null}"
	if string(d0) != goldenLeaf {
		t.Fatalf("depth-0 node JSON != golden\n got:    %s\n golden: %s", d0, goldenLeaf)
	}

	// Multi-key recursive map: native emits the deterministic SORTED representative;
	// parity with the served (unsorted) reference is canonical-JSON-equality (the
	// owner-sanctioned relation for >1-key maps), asserted at every nesting depth.
	nm := OutputNode{Value: "r", ByName: map[string]OutputNode{"z": {Value: "vz"}, "a": {Value: "va"}, "m": {Value: "vm"}}}
	rm := refNode{Value: "r", ByName: map[string]refNode{"z": {Value: "vz"}, "a": {Value: "va"}, "m": {Value: "vm"}}}
	nb, err := sonic.Marshal(nm)
	if err != nil {
		t.Fatal(err)
	}
	nb2, err := sonic.Marshal(nm)
	if err != nil {
		t.Fatal(err)
	}
	if string(nb) != string(nb2) {
		t.Fatalf("recursive multi-key map not deterministic\n a: %s\n b: %s", nb, nb2)
	}
	if !strings.Contains(string(nb), "\"a\":") || strings.Index(string(nb), "\"a\":") > strings.Index(string(nb), "\"z\":") {
		t.Fatalf("native multi-key map is not sorted: %s", nb)
	}
	rb, err := sonic.Marshal(rm)
	if err != nil {
		t.Fatal(err)
	}
	var nAny, rAny any
	if err := json.Unmarshal(nb, &nAny); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rb, &rAny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nAny, rAny) {
		t.Fatalf("native and served recursive maps differ under canonical equality\n native: %s\n served: %s", nb, rb)
	}
}

func TestRecThroughUnion(t *testing.T) {
	// value: string -> Through wraps a string.
	sv := OutputThrough{Value: OutputUnion1NewVariant0("hi")}
	nb, err := sonic.Marshal(sv)
	if err != nil {
		t.Fatal(err)
	}
	const goldenStr = "{\"value\":\"hi\"}"
	if string(nb) != goldenStr {
		t.Fatalf("through(string) != golden\n got: %s\n want: %s", nb, goldenStr)
	}
	rs := refThrough{Value: refUnionString("hi")}
	rb, err := sonic.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if string(rb) != string(nb) {
		t.Fatalf("through(string) native != ref\n native: %s\n ref: %s", nb, rb)
	}

	// value: Through -> recursion through the union arm pointer.
	inner := OutputThrough{Value: OutputUnion1NewVariant0("deep")}
	nv := OutputThrough{Value: OutputUnion1NewVariant1(inner)}
	nb2, err := sonic.Marshal(nv)
	if err != nil {
		t.Fatal(err)
	}
	const goldenNested = "{\"value\":{\"value\":\"deep\"}}"
	if string(nb2) != goldenNested {
		t.Fatalf("through(Through) != golden\n got: %s\n want: %s", nb2, goldenNested)
	}
	// Round-trip the nested value.
	var back OutputThrough
	if err := json.Unmarshal([]byte(goldenNested), &back); err != nil {
		t.Fatal(err)
	}
	again, err := sonic.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != goldenNested {
		t.Fatalf("through round-trip not byte-identical\n got: %s\n want: %s", again, goldenNested)
	}
}

// TestRecCycleMarshalErrors pins negative control (5): a user-built pointer cycle
// returns a marshal ERROR on BOTH the native carrier (its custom codec's guard)
// and the BAML-equivalent reference (default json.Marshal cycle detection), and
// NEITHER hangs or overflows the stack (each marshal runs under a timeout).
func TestRecCycleMarshalErrors(t *testing.T) {
	marshalWithTimeout := func(name string, fn func() ([]byte, error)) error {
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("panic: %v", r)
				}
			}()
			_, err := fn()
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: cyclic marshal hung (stack-overflow guard failed)", name)
			return nil
		}
	}

	// Native pointer cycle: next points back to self.
	nc := &OutputNode{Value: "a"}
	nc.Next = nc
	if err := marshalWithTimeout("native", func() ([]byte, error) { return json.Marshal(nc) }); err == nil {
		t.Fatal("native cyclic marshal did not error")
	}

	// Reference pointer cycle: plain struct, encoding/json's built-in cycle detection.
	rc := &refNode{Value: "a"}
	rc.Next = rc
	if err := marshalWithTimeout("reference", func() ([]byte, error) { return json.Marshal(rc) }); err == nil {
		t.Fatal("reference cyclic marshal did not error")
	}

	// A finite (non-cyclic) node still marshals cleanly (the guard is not a false positive).
	if _, err := json.Marshal(nativeNode()); err != nil {
		t.Fatalf("finite recursive node failed the acyclic guard: %v", err)
	}
}
`

const recAliasTestSource = `package recaliaspkg

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bytedance/sonic"
)

// --- Frozen v0.223 reference carriers: the generated recursive-alias/union JSON
// --- methods (type_aliases.go + unions.go), transcribed verbatim MINUS the CFFI
// --- Decode/Encode/BamlTypeName. Recursion is reproduced by the alias being a Go
// --- alias to a value union carrier (Recursive1) or a pointer-to-union (JSON).

// type Recursive1 = Union2IntOrListRecursive1
type refRecursive1 = refUnionR1
type refUnionR1 struct {
	variant string
	varInt  *int64
	varList *[]refRecursive1
}

func refR1Int(v int64) refUnionR1        { return refUnionR1{variant: "Int", varInt: &v} }
func refR1List(v []refRecursive1) refUnionR1 { return refUnionR1{variant: "List", varList: &v} }

func (u refUnionR1) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "Int":
		return json.Marshal(u.varInt)
	case "List":
		return json.Marshal(u.varList)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnionR1) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &u.varInt); err == nil {
		u.variant = "Int"
		return nil
	}
	u.varInt = nil
	if err := json.Unmarshal(data, &u.varList); err == nil {
		u.variant = "List"
		return nil
	}
	u.varList = nil
	return fmt.Errorf("invalid union variant: %s", string(data))
}

// type JSONValue = *Union...  (nullable alias -> pointer to union). Arm order
// follows the descriptor: String, Int, Float, Bool, List, Map.
type refJSON = *refUnionJ
type refUnionJ struct {
	variant   string
	varString *string
	varInt    *int64
	varFloat  *float64
	varBool   *bool
	varList   *[]refJSON
	varMap    *map[string]refJSON
}

func refJString(v string) refUnionJ         { return refUnionJ{variant: "String", varString: &v} }
func refJInt(v int64) refUnionJ             { return refUnionJ{variant: "Int", varInt: &v} }
func refJBool(v bool) refUnionJ             { return refUnionJ{variant: "Bool", varBool: &v} }
func refJList(v []refJSON) refUnionJ        { return refUnionJ{variant: "List", varList: &v} }
func refJMap(v map[string]refJSON) refUnionJ { return refUnionJ{variant: "Map", varMap: &v} }

func (u refUnionJ) MarshalJSON() ([]byte, error) {
	switch u.variant {
	case "String":
		return json.Marshal(u.varString)
	case "Int":
		return json.Marshal(u.varInt)
	case "Float":
		return json.Marshal(u.varFloat)
	case "Bool":
		return json.Marshal(u.varBool)
	case "List":
		return json.Marshal(u.varList)
	case "Map":
		return json.Marshal(u.varMap)
	}
	return nil, fmt.Errorf("invalid union variant: %s", u.variant)
}
func (u *refUnionJ) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &u.varString); err == nil {
		u.variant = "String"
		return nil
	}
	u.varString = nil
	if err := json.Unmarshal(data, &u.varInt); err == nil {
		u.variant = "Int"
		return nil
	}
	u.varInt = nil
	if err := json.Unmarshal(data, &u.varFloat); err == nil {
		u.variant = "Float"
		return nil
	}
	u.varFloat = nil
	if err := json.Unmarshal(data, &u.varBool); err == nil {
		u.variant = "Bool"
		return nil
	}
	u.varBool = nil
	if err := json.Unmarshal(data, &u.varList); err == nil {
		u.variant = "List"
		return nil
	}
	u.varList = nil
	if err := json.Unmarshal(data, &u.varMap); err == nil {
		u.variant = "Map"
		return nil
	}
	u.varMap = nil
	return fmt.Errorf("invalid union variant: %s", string(data))
}

// TestRecAlias exercises the emitted alias carriers' JSON behavior AND pins it
// against the frozen v0.223 reference carriers above (byte-equal marshal, nullable
// null, first-success unmarshal order, and the pure-container any fallback). The
// alias declarations themselves (OutputRecursive1 = OutputUnion1, OutputJsonValue =
// *OutputUnion2, OutputListNode = []any, OutputStrMap = map[string]any) are pinned
// by the emit-string assertions in the parent test.
func TestRecAlias(t *testing.T) {
	// Recursive1 int arm.
	rInt := OutputUnion1NewVariant0(5)
	if b, err := sonic.Marshal(rInt); err != nil || string(b) != "5" {
		t.Fatalf("Recursive1 int arm = %s, %v", b, err)
	}
	// Recursive1 recursive list arm: [5, [6]].
	nested := OutputUnion1NewVariant1([]OutputRecursive1{OutputUnion1NewVariant0(6)})
	rList := OutputUnion1NewVariant1([]OutputRecursive1{OutputUnion1NewVariant0(5), nested})
	b, err := sonic.Marshal(rList)
	if err != nil {
		t.Fatal(err)
	}
	const goldenRec = "[5,[6]]"
	if string(b) != goldenRec {
		t.Fatalf("Recursive1 list arm = %s, want %s", b, goldenRec)
	}
	// First-success unmarshal: "5" binds the int arm; "[6]" binds the list arm.
	var u OutputRecursive1
	if err := json.Unmarshal([]byte("5"), &u); err != nil || !u.IsVariant0() {
		t.Fatalf("unmarshal 5 -> int arm failed: variant0=%v err=%v", u.IsVariant0(), err)
	}
	var u2 OutputRecursive1
	if err := json.Unmarshal([]byte("[6]"), &u2); err != nil || !u2.IsVariant1() {
		t.Fatalf("unmarshal [6] -> list arm failed: variant1=%v err=%v", u2.IsVariant1(), err)
	}

	// JSONValue is a nullable alias: nil -> null (no arm selected).
	var doc OutputJsonValue // *OutputUnion2
	if b, err := sonic.Marshal(doc); err != nil || string(b) != "null" {
		t.Fatalf("nil JSONValue alias = %s, %v (want null)", b, err)
	}
	// JSONValue string arm.
	sv := OutputUnion2NewVariant0("hello")
	doc = &sv
	if b, err := sonic.Marshal(doc); err != nil || string(b) != "\"hello\"" {
		t.Fatalf("JSONValue string arm = %s, %v", b, err)
	}
	// JSONValue recursive map arm: {"k": 1}.
	mv := OutputUnion2NewVariant5(map[string]OutputJsonValue{"k": func() OutputJsonValue { u := OutputUnion2NewVariant1(1); return &u }()})
	doc = &mv
	if b, err := sonic.Marshal(doc); err != nil || string(b) != "{\"k\":1}" {
		t.Fatalf("JSONValue map arm = %s, %v", b, err)
	}
	// JSONValue recursive list arm: [1, "x"].
	lv := OutputUnion2NewVariant4([]OutputJsonValue{
		func() OutputJsonValue { u := OutputUnion2NewVariant1(1); return &u }(),
		func() OutputJsonValue { u := OutputUnion2NewVariant0("x"); return &u }(),
	})
	doc = &lv
	if b, err := sonic.Marshal(doc); err != nil || string(b) != "[1,\"x\"]" {
		t.Fatalf("JSONValue list arm = %s, %v", b, err)
	}

	// ListNode = []any: pure-container fallback marshals/unmarshals as a plain slice.
	var ln OutputListNode = []any{float64(1), "x", []any{float64(2)}}
	if b, err := sonic.Marshal(ln); err != nil || string(b) != "[1,\"x\",[2]]" {
		t.Fatalf("ListNode = %s, %v", b, err)
	}
	var lnBack OutputListNode
	if err := json.Unmarshal([]byte("[1,\"x\"]"), &lnBack); err != nil || len(lnBack) != 2 {
		t.Fatalf("ListNode unmarshal failed: %v (%v)", lnBack, err)
	}
	// nil ListNode -> null (no omitempty semantics for a bare slice alias).
	var lnNil OutputListNode
	if b, err := sonic.Marshal(lnNil); err != nil || string(b) != "null" {
		t.Fatalf("nil ListNode = %s, %v (want null)", b, err)
	}

	// StrMap = map[string]any: pure-container fallback.
	var sm OutputStrMap = map[string]any{"a": float64(1)}
	if b, err := sonic.Marshal(sm); err != nil || string(b) != "{\"a\":1}" {
		t.Fatalf("StrMap = %s, %v", b, err)
	}

	// The whole AW carrier round-trips with every alias field populated.
	aw := OutputAw{
		Rec1:  OutputUnion1NewVariant0(7),
		Doc:   func() OutputJsonValue { u := OutputUnion2NewVariant3(true); return &u }(),
		Nodes: []any{"a"},
		Dict:  map[string]any{"k": "v"},
	}
	awb, err := sonic.Marshal(aw)
	if err != nil {
		t.Fatal(err)
	}
	const goldenAW = "{\"rec1\":7,\"doc\":true,\"nodes\":[\"a\"],\"dict\":{\"k\":\"v\"}}"
	if string(awb) != goldenAW {
		t.Fatalf("AW carrier = %s, want %s", awb, goldenAW)
	}
	var awBack OutputAw
	if err := json.Unmarshal([]byte(goldenAW), &awBack); err != nil {
		t.Fatal(err)
	}
	again, err := sonic.Marshal(awBack)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != goldenAW {
		t.Fatalf("AW round-trip not byte-identical\n got: %s\n want: %s", again, goldenAW)
	}

	// --- native carrier == frozen v0.223 reference carrier (the real differential).
	eq := func(label string, nv, rv any) {
		nb, err := sonic.Marshal(nv)
		if err != nil {
			t.Fatalf("%s: native marshal: %v", label, err)
		}
		rb, err := sonic.Marshal(rv)
		if err != nil {
			t.Fatalf("%s: reference marshal: %v", label, err)
		}
		if string(nb) != string(rb) {
			t.Fatalf("%s: native != v0.223 reference\n native: %s\n ref:    %s", label, nb, rb)
		}
	}
	// Recursive1: nested list arm [5,[6]].
	eq("Recursive1 list",
		OutputUnion1NewVariant1([]OutputRecursive1{OutputUnion1NewVariant0(5), OutputUnion1NewVariant1([]OutputRecursive1{OutputUnion1NewVariant0(6)})}),
		refR1List([]refRecursive1{refR1Int(5), refR1List([]refRecursive1{refR1Int(6)})}))
	// JSONValue nullable null: nil alias on both marshals to null.
	var nNil OutputJsonValue
	var rNil refJSON
	eq("JSONValue null", nNil, rNil)
	// JSONValue string / bool / list / map arms.
	eq("JSONValue string", func() OutputJsonValue { u := OutputUnion2NewVariant0("hi"); return &u }(), func() refJSON { u := refJString("hi"); return &u }())
	eq("JSONValue bool", func() OutputJsonValue { u := OutputUnion2NewVariant3(true); return &u }(), func() refJSON { u := refJBool(true); return &u }())
	eq("JSONValue list", func() OutputJsonValue {
		u := OutputUnion2NewVariant4([]OutputJsonValue{func() OutputJsonValue { v := OutputUnion2NewVariant1(1); return &v }()})
		return &u
	}(), func() refJSON {
		u := refJList([]refJSON{func() refJSON { v := refJInt(1); return &v }()})
		return &u
	}())
	eq("JSONValue map", func() OutputJsonValue {
		u := OutputUnion2NewVariant5(map[string]OutputJsonValue{"k": func() OutputJsonValue { v := OutputUnion2NewVariant0("v"); return &v }()})
		return &u
	}(), func() refJSON {
		u := refJMap(map[string]refJSON{"k": func() refJSON { v := refJString("v"); return &v }()})
		return &u
	}())

	// First-success unmarshal PARITY: native and the reference select the SAME arm
	// for each input (ambiguous scalars resolve in descriptor arm order).
	firstSuccess := func(in string, wantNative func(*OutputUnion2) bool, wantRef func(*refUnionJ) bool) {
		var n OutputUnion2
		if err := json.Unmarshal([]byte(in), &n); err != nil {
			t.Fatalf("native unmarshal %s: %v", in, err)
		}
		var r refUnionJ
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Fatalf("reference unmarshal %s: %v", in, err)
		}
		if !wantNative(&n) || !wantRef(&r) {
			t.Fatalf("first-success arm mismatch for %s: reference selected variant %q", in, r.variant)
		}
	}
	// A JSON int binds Int (arm 1), before Float (arm 2): native variant1, ref "Int".
	firstSuccess("7", func(n *OutputUnion2) bool { return n.IsVariant1() }, func(r *refUnionJ) bool { return r.variant == "Int" })
	// A JSON string binds String (arm 0).
	firstSuccess(` + "`\"z\"`" + `, func(n *OutputUnion2) bool { return n.IsVariant0() }, func(r *refUnionJ) bool { return r.variant == "String" })
	// A JSON bool binds Bool (arm 3), after string/int/float fail.
	firstSuccess("true", func(n *OutputUnion2) bool { return n.IsVariant3() }, func(r *refUnionJ) bool { return r.variant == "Bool" })

	// Pure-container any fallback: native OutputListNode/OutputStrMap == plain
	// []any/map[string]any (BAML's own any fallback), the reference for these.
	var nList2 OutputListNode = []any{float64(1), []any{float64(2)}}
	nb, _ := sonic.Marshal(nList2)
	rb, _ := sonic.Marshal([]any{float64(1), []any{float64(2)}})
	if string(nb) != string(rb) {
		t.Fatalf("ListNode any-fallback native != reference []any\n native: %s\n ref: %s", nb, rb)
	}

	// Sub-slice aliasing is FINITE, not a cycle: nodes[1] shares nodes' backing
	// array at offset 0. The recursion-safe marshal guard keys slices on
	// (ptr, len) — like encoding/json — so this must NOT be reported as a cycle.
	nodes := []any{"x", nil}
	nodes[1] = nodes[0:1]
	subAliased := OutputAw{
		Rec1:  OutputUnion1NewVariant0(0),
		Nodes: nodes,
		Dict:  map[string]any{},
	}
	if _, err := sonic.Marshal(subAliased); err != nil {
		t.Fatalf("sub-slice-aliased (finite) []any field wrongly rejected as a cycle: %v", err)
	}
	// It matches encoding/json's own marshal of the same finite value.
	if b, err := json.Marshal(nodes); err != nil || string(b) != "[\"x\",[\"x\"]]" {
		t.Fatalf("sub-slice alias reference marshal = %s, %v", b, err)
	}
}
`
