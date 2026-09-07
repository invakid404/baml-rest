package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// The COMPOSED cross-product differential for the required scalar-LIST input
// widening (scope §3.A proof 1).
//
// ONE small .baml project is built ONCE and drives BOTH input paths the serving
// cohort depends on:
//
//	standard path — cmd/introspect's generated argument projector
//	                (emitStaticPromptArgumentProjectors -> StaticPromptArgumentValues)
//	spine path    — the BAML-free emitted module's ProjectInput
//	                (adapters/common/codegen.EmitNativeStaticStream -> Binding().ProjectInput)
//
// Both are EMITTED Go source, so the only honest way to compare them is to compile
// and run them: an AST or string assertion cannot show that `in.Tags` and
// `args[4].([]string)` produce the same ordered vector. The temp module is fully
// offline (GOPROXY=off, GOWORK=off, CGO_ENABLED=0) and resolves bamlutils through a
// directory replace, matching the established harness in this package.
//
// What the compiled comparison covers, per the scope: the three admitted list
// primitives, scalar/list mixtures, the empty list, nil-vs-empty slices, repeated
// and ordered items, unicode / quotes / backslashes / newlines / HTML characters,
// the int64 limits, finite float formatting INCLUDING negative zero (on the scalar
// float, which the cohort admits), and the public-boundary distinction between a
// MISSING argument and an untyped nil.
//
// The signature carries no `float[]`: it is excluded from the serving cohort by a
// measured stock-BAML list-render residual (see
// internal/nativebody/nanollmprepare/listserve/float_residual_test.go), so a
// differential over it would be comparing two projectors for a shape the population
// never admits.
//
// The comparator is sign-aware on floats on purpose: reflect.DeepEqual and `==`
// both report -0.0 == 0.0, so a differential that used either would pass even if
// one projector silently normalised the sign away.

// scalarListDifferentialSources is the composed corpus. The return is the UNCHANGED
// exact five-arm JSON alias — this slice widens inputs only — and the prompt renders
// every argument directly, so the whole vector is load-bearing rather than declared
// and ignored.
var scalarListDifferentialSources = map[string]string{
	"clients.baml": `client<llm> C {
  provider openai
  options { model "gpt-4o-mini" api_key "sk-x" base_url "http://127.0.0.1:0/v1" }
}
`,
	"types.baml": "type JSON = int | string | bool | JSON[] | map<string, JSON>\n",
	"functions.baml": `function ListMix(topic: string, n: int, r: float, b: bool, tags: string[], counts: int[], flags: bool[]) -> JSON {
  client C
  prompt #"{{ topic }} {{ n }} {{ r }} {{ b }} {{ tags }} {{ counts }} {{ flags }} {{ ctx.output_format }}"#
}
`,
}

const scalarListMethod = "ListMix"

// TestScalarListProjectorsAgreeAcrossBothInputPaths is the composed differential.
func TestScalarListProjectorsAgreeAcrossBothInputPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess compile/behaviour harness in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}

	proj, descriptors, err := nativespine.ProjectAndDescriptorsFromSource(scalarListDifferentialSources)
	if err != nil {
		t.Fatalf("ProjectAndDescriptorsFromSource: %v", err)
	}

	// The source classifier must admit the method and stamp it stream-capable, or
	// the differential below would be comparing two projectors for a method the
	// serving cohort never reaches.
	m := admittedMethodByName(t, proj, scalarListMethod)
	if m.Class != projectdescriptor.ClassStaticStream {
		t.Fatalf("%s class = %q, want %q", scalarListMethod, m.Class, projectdescriptor.ClassStaticStream)
	}
	if _, ok := descriptors[scalarListMethod]; !ok {
		t.Fatalf("no prompt descriptor for %s", scalarListMethod)
	}

	dir := t.TempDir()
	repoRoot := gateARepoRoot(t)

	// --- standard path: the generated argument projector -----------------------
	// idx is nil: a scalar/scalar-list signature needs no generated types package,
	// which is itself part of the claim (this cohort adds no class/enum dependency).
	out := jen.NewFile("introspected")
	emitStaticPromptArgumentProjectors(out, &config{InterfacesPkg: gateAInterfacesPkg}, descriptors, nil)
	if err := out.Save(filepath.Join(dir, "introspected.go")); err != nil {
		t.Fatalf("save emitted standard projector: %v", err)
	}
	assertStandardProjectorAdmitted(t, filepath.Join(dir, "introspected.go"))

	// --- spine path: the BAML-free emitted module ------------------------------
	// EmitNativeStaticStream, not the unary emitter: the JSON-returning method is
	// stamped ClassStaticStream, and the stream emitter is what the standard worker
	// and the native-only worker both consume. Its ProjectInput is the SAME emitted
	// projector the unary Binding() exposes (StreamBinding embeds Binding), so this
	// differential covers both surfaces at once.
	carrier, err := codegen.EmitNativeStaticStream(m, codegen.NativeSpineOptions{PackageName: "carrier"})
	if err != nil {
		t.Fatalf("EmitNativeStaticStream: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "carrier"), 0o755); err != nil {
		t.Fatalf("mkdir carrier: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "carrier", "carrier.go"), carrier, 0o644); err != nil {
		t.Fatalf("write carrier.go: %v", err)
	}

	const modPath = "scalarlistdiff"
	if err := os.WriteFile(filepath.Join(dir, "differential_test.go"), []byte(scalarListHarness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	bamlutilsAbs := filepath.Join(repoRoot, "bamlutils")
	goMod := fmt.Sprintf("module %s\n\ngo %s\n\nrequire %s v0.0.0\n\nreplace %s => %s\n",
		modPath, gateAGoVersion(t, repoRoot), gateAInterfacesPkg, gateAInterfacesPkg, filepath.ToSlash(bamlutilsAbs))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("write go.sum: %v", err)
		}
	}

	// The emitted spine module must still link no BAML/CFFI after the widening.
	assertEmittedCarrierNoCFFI(t, filepath.Join(dir, "carrier"))

	cmd := exec.Command("go", "test", "-count=1", "-v", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local",
	)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("composed scalar-list projector differential failed: %v\n%s", err, outBytes)
	}
	// A subprocess `go test` that ran NOTHING also exits 0, so the exit code alone
	// is not evidence. Every leg of the differential is named here and must appear
	// as a PASS, including each cross-product row.
	transcript := string(outBytes)
	for _, name := range []string{
		"TestBothInputPathsProduceTheSameVector/ordinary",
		"TestBothInputPathsProduceTheSameVector/empty_lists",
		"TestBothInputPathsProduceTheSameVector/nil_lists",
		"TestBothInputPathsProduceTheSameVector/repeated_and_ordered",
		"TestBothInputPathsProduceTheSameVector/escaping",
		"TestBothInputPathsProduceTheSameVector/int64_limits",
		"TestBothInputPathsProduceTheSameVector/scalar_float_negative_zero",
		"TestBothInputPathsProduceTheSameVector/scalar_float_extremes",
		"TestNilAndEmptySlicesBothProjectAnEmptyList",
		"TestNegativeZeroKeepsItsSign",
		"TestPublicInputBoundaryDeclines",
	} {
		if !strings.Contains(transcript, "--- PASS: "+name) {
			t.Fatalf("the differential harness did not report a PASS for %s:\n%s", name, transcript)
		}
	}
}

// assertStandardProjectorAdmitted fails if the emitted standard projector put the
// differential's method in its DECLINE ledger instead of its registry. It is a
// source read rather than a compiled call because a declined method has no
// projector to call at all, so the compiled harness below could not tell the
// difference between "declined" and "absent".
func assertStandardProjectorAdmitted(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read emitted projector: %v", err)
	}
	src := string(b)
	// Guarded rather than sliced directly: a missing ledger would make
	// src[-1:] panic, and the panic would replace the specific failure message this
	// assertion exists to produce.
	start := strings.Index(src, "StaticPromptProjectorDeclines")
	if start < 0 {
		t.Fatal("the emitted projector declared no StaticPromptProjectorDeclines ledger; the emitter's shape has changed")
	}
	declines := src[start:]
	if end := strings.Index(declines, "\n}"); end >= 0 {
		declines = declines[:end]
	}
	if strings.Contains(declines, fmt.Sprintf("%q:", scalarListMethod)) {
		t.Fatalf("the standard projector DECLINED %s; the standard input path must project the scalar-list cohort.\n%s",
			scalarListMethod, declines)
	}
	if !strings.Contains(src, fmt.Sprintf("%q: func(args []any)", scalarListMethod)) {
		t.Fatalf("the standard projector emitted no registry entry for %s", scalarListMethod)
	}
}

// scalarListHarness is the in-module differential the behaviour leg compiles and
// runs against BOTH emitted projectors.
const scalarListHarness = `package introspected

import (
	"math"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"

	"scalarlistdiff/carrier"
)

// row is one cross-product point: the SAME typed values presented to the standard
// projector as an ordered []any and to the spine projector as the emitted input
// carrier. Anything the two disagree on is a serving divergence, because the
// standard worker uses the first and the BAML-free worker uses the second.
type row struct {
	name  string
	args  []any
	input *carrier.ListMixInput
}

func rows() []row {
	mk := func(topic string, n int64, r float64, b bool, tags []string, counts []int64, flags []bool) row {
		return row{
			args:  []any{topic, n, r, b, tags, counts, flags},
			input: &carrier.ListMixInput{Topic: topic, N: n, R: r, B: b, Tags: tags, Counts: counts, Flags: flags},
		}
	}
	out := []row{}
	add := func(name string, r row) { r.name = name; out = append(out, r) }

	add("ordinary", mk("weather", 7, 2.5, true,
		[]string{"b", "a", "c"}, []int64{1, -2, 3}, []bool{true, false, true}))

	// Empty and nil slices are DISTINCT Go values that both project to an EMPTY
	// StaticList — the projectors turn a required nil slice into an empty list
	// rather than a null. Both are compared so the two paths cannot disagree
	// about which one they treat as absent.
	add("empty_lists", mk("", 0, 0, false,
		[]string{}, []int64{}, []bool{}))
	add("nil_lists", mk("", 0, 0, false, nil, nil, nil))

	// Order is INPUT order, and repeats are preserved verbatim (not deduplicated,
	// not sorted).
	add("repeated_and_ordered", mk("t", 1, 1, true,
		[]string{"b", "a", "b", "a"}, []int64{3, 1, 3}, []bool{false, true, false}))

	// Escaping-sensitive strings, both as the scalar and inside the list.
	add("escaping", mk("quote \" backslash \\ newline \n tab \t", 0, 0, false,
		[]string{"café ☕", "<b>&amp;</b>", "line\nbreak", "back\\slash", "quote\"inside", " lead", ""},
		[]int64{0}, []bool{true}))

	// int64 limits.
	add("int64_limits", mk("i", math.MaxInt64, 0, false,
		[]string{"x"}, []int64{math.MaxInt64, math.MinInt64, 0, -1}, []bool{true}))

	// Finite float formatting on the admitted SCALAR float, INCLUDING negative zero.
	// NaN/Inf are deliberately absent: the binder declines them pre-send, which is a
	// serving decision, not a projection one.
	add("scalar_float_negative_zero", mk("f", 0, math.Copysign(0, -1), false,
		[]string{"x"}, []int64{0}, []bool{false}))
	add("scalar_float_extremes", mk("f", 0, math.MaxFloat64, false,
		[]string{"x"}, []int64{0}, []bool{false}))

	return out
}

func TestBothInputPathsProduceTheSameVector(t *testing.T) {
	for _, r := range rows() {
		t.Run(r.name, func(t *testing.T) {
			std, ok := StaticPromptArgumentValues("ListMix", r.args)
			if !ok {
				t.Fatal("the STANDARD generated projector declined a scalar-list call it must project")
			}
			spineVec, err := carrier.Binding().ProjectInput(r.input)
			if err != nil {
				t.Fatalf("the SPINE ProjectInput declined a scalar-list call it must project: %v", err)
			}
			assertVectorsEqual(t, std, spineVec)

			// Both must be the full 7-argument vector in DECLARED order; a shared
			// truncation would otherwise compare equal to itself.
			wantNames := []string{"topic", "n", "r", "b", "tags", "counts", "flags"}
			if len(std) != len(wantNames) {
				t.Fatalf("vector length %d, want %d", len(std), len(wantNames))
			}
			for i, n := range wantNames {
				if std[i].Name != n {
					t.Fatalf("argument %d is %q, want %q (declared order)", i, std[i].Name, n)
				}
			}
		})
	}
}

// TestNilAndEmptySlicesBothProjectAnEmptyList pins the nil-vs-empty answer
// EXPLICITLY rather than only as an equality between the two paths: a shared bug
// that turned both into a null would satisfy the comparison above.
func TestNilAndEmptySlicesBothProjectAnEmptyList(t *testing.T) {
	seen := 0
	for _, r := range rows() {
		if r.name != "nil_lists" && r.name != "empty_lists" {
			continue
		}
		seen++
		std, ok := StaticPromptArgumentValues("ListMix", r.args)
		if !ok {
			t.Fatalf("%s: standard projector declined", r.name)
		}
		spineVec, err := carrier.Binding().ProjectInput(r.input)
		if err != nil {
			t.Fatalf("%s: spine projector declined: %v", r.name, err)
		}
		for _, vec := range [][]promptdescriptor.ArgumentValue{std, spineVec} {
			for _, i := range []int{4, 5, 6} {
				v := vec[i]
				if v.Value.Kind != promptdescriptor.StaticList {
					t.Fatalf("%s: %s projected as kind %q, want StaticList (never StaticNull)", r.name, v.Name, v.Value.Kind)
				}
				if len(v.Value.Items) != 0 {
					t.Fatalf("%s: %s projected %d items, want 0", r.name, v.Name, len(v.Value.Items))
				}
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected both the nil and the empty row, saw %d", seen)
	}
}

// TestNegativeZeroKeepsItsSign is the comparator's own proof. == and
// reflect.DeepEqual both report -0.0 == 0.0, so without a signbit check a
// projector that normalised the sign would pass every comparison above.
func TestNegativeZeroKeepsItsSign(t *testing.T) {
	args := []any{"f", int64(0), math.Copysign(0, -1), false, []string{}, []int64{}, []bool{}}
	std, ok := StaticPromptArgumentValues("ListMix", args)
	if !ok {
		t.Fatal("standard projector declined the negative-zero row")
	}
	spineVec, err := carrier.Binding().ProjectInput(&carrier.ListMixInput{
		Topic: "f", R: math.Copysign(0, -1), Tags: []string{}, Counts: []int64{}, Flags: []bool{}})
	if err != nil {
		t.Fatalf("spine projector declined the negative-zero row: %v", err)
	}
	for _, vec := range [][]promptdescriptor.ArgumentValue{std, spineVec} {
		if !math.Signbit(vec[2].Value.Float) {
			t.Error("the scalar negative zero lost its sign")
		}
		if vec[2].Value.Float != 0 {
			t.Errorf("the negative zero is no longer zero: %v", vec[2].Value.Float)
		}
	}
}

// TestPublicInputBoundaryDeclines pins the MISSING-argument and untyped-nil cases
// at the public boundary. Both must DECLINE (ok=false / an error) before anything
// native is sent — never be repaired into an empty list.
func TestPublicInputBoundaryDeclines(t *testing.T) {
	full := []any{"t", int64(1), 1.0, true, []string{"a"}, []int64{1}, []bool{true}}

	if _, ok := StaticPromptArgumentValues("ListMix", full[:6]); ok {
		t.Error("a SHORT argument vector (missing list argument) must decline")
	}
	if _, ok := StaticPromptArgumentValues("ListMix", append(append([]any{}, full...), "extra")); ok {
		t.Error("an OVER-LONG argument vector must decline")
	}

	untypedNil := append([]any{}, full...)
	untypedNil[4] = nil
	if _, ok := StaticPromptArgumentValues("ListMix", untypedNil); ok {
		t.Error("an untyped nil (a JSON null at the public boundary) must decline, not project an empty list")
	}

	// Malformed element types: the assertion is EXACT, so the near-miss Go types a
	// caller could plausibly supply all decline.
	for i, bad := range []any{[]int{1}, []float32{1}, []any{"a"}, "a"} {
		wrong := append([]any{}, full...)
		wrong[5] = bad
		if _, ok := StaticPromptArgumentValues("ListMix", wrong); ok {
			t.Errorf("case %d: a %T where []int64 is declared must decline", i, bad)
		}
	}

	// The spine projector's own boundary: a nil or wrong-typed input carrier.
	if _, err := carrier.Binding().ProjectInput(nil); err == nil {
		t.Error("the spine projector must decline a nil input carrier")
	}
	if _, err := carrier.Binding().ProjectInput(&struct{}{}); err == nil {
		t.Error("the spine projector must decline a foreign input carrier")
	}
}

// assertVectorsEqual compares two projected vectors exactly, with a SIGN-AWARE
// float comparison (see the file comment).
func assertVectorsEqual(t *testing.T, a, b []promptdescriptor.ArgumentValue) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("vector lengths differ: standard=%d spine=%d", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			t.Fatalf("argument %d name: standard=%q spine=%q", i, a[i].Name, b[i].Name)
		}
		assertValuesEqual(t, a[i].Name, a[i].Value, b[i].Value)
	}
}

func assertValuesEqual(t *testing.T, path string, a, b promptdescriptor.StaticValue) {
	t.Helper()
	if a.Kind != b.Kind {
		t.Fatalf("%s kind: standard=%q spine=%q", path, a.Kind, b.Kind)
	}
	switch a.Kind {
	case promptdescriptor.StaticString:
		if a.String != b.String {
			t.Fatalf("%s string: standard=%q spine=%q", path, a.String, b.String)
		}
	case promptdescriptor.StaticInt:
		if a.Int != b.Int {
			t.Fatalf("%s int: standard=%d spine=%d", path, a.Int, b.Int)
		}
	case promptdescriptor.StaticFloat:
		if a.Float != b.Float || math.Signbit(a.Float) != math.Signbit(b.Float) {
			t.Fatalf("%s float: standard=%v (signbit %v) spine=%v (signbit %v)",
				path, a.Float, math.Signbit(a.Float), b.Float, math.Signbit(b.Float))
		}
	case promptdescriptor.StaticBool:
		if a.Bool != b.Bool {
			t.Fatalf("%s bool: standard=%v spine=%v", path, a.Bool, b.Bool)
		}
	case promptdescriptor.StaticList:
		if len(a.Items) != len(b.Items) {
			t.Fatalf("%s list length: standard=%d spine=%d", path, len(a.Items), len(b.Items))
		}
		for i := range a.Items {
			assertValuesEqual(t, path+"["+itoa(i)+"]", a.Items[i], b.Items[i])
		}
	default:
		t.Fatalf("%s projected an unexpected kind %q for a scalar-list cohort argument", path, a.Kind)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
`
