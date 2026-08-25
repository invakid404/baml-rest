package codegen

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/testharness"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	sd "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// kitchenSinkMethod is an admitted static-unary method whose RETURN class
// exercises the whole M3a final-carrier BASE vocabulary in one shape: the four
// primitives, a NESTED class, an ENUM, a LIST of primitives, a LIST of classes, an
// OPTIONAL (nullable) primitive, and a STRING-keyed MAP. Every field/enum uses its
// CANONICAL name: an aliased output field/enum member is now DECLINED by the
// classifier (its served bytes go under the alias in the native codec but under the
// canonical json tag in a baml_client-generated carrier — they diverge), so the
// admitted carrier vocabulary is canonical-only until M3b pins the output-key
// policy. Inputs stay scalar (M1 input profile).
func kitchenSinkMethod() projectdescriptor.Method {
	prim := func(p sd.PrimitiveKind) sd.Type { return sd.Type{Kind: sd.TypePrimitive, Primitive: p} }
	optional := func(inner sd.Type) sd.Type {
		return sd.Type{Kind: sd.TypeUnion, Union: &sd.UnionType{Nullable: true, Variants: []sd.Type{inner}}}
	}
	list := func(elem sd.Type) sd.Type { e := elem; return sd.Type{Kind: sd.TypeList, Elem: &e} }
	strMap := func(v sd.Type) sd.Type {
		k := prim(sd.PrimitiveString)
		vv := v
		return sd.Type{Kind: sd.TypeMap, Key: &k, Value: &vv}
	}

	return projectdescriptor.Method{
		Name:  "KitchenSink",
		Class: projectdescriptor.ClassStaticUnary,
		Return: sd.Bundle{
			Version: sd.Version,
			Method:  "KitchenSink",
			Target:  sd.Type{Kind: sd.TypeClass, Name: "KS"},
			Enums: []sd.EnumDef{{
				Name:   sd.Name{Name: "Color"},
				Values: []sd.EnumValue{{Name: sd.Name{Name: "RED"}}, {Name: sd.Name{Name: "GREEN"}}},
			}},
			// Declaration order below is the JSON key order both the native codec and a
			// BAML-generated struct emit — keep it stable.
			Classes: []sd.ClassDef{
				{
					Name: sd.Name{Name: "KS"},
					Fields: []sd.ClassField{
						{Name: sd.Name{Name: "name"}, Type: prim(sd.PrimitiveString)},
						{Name: sd.Name{Name: "count"}, Type: prim(sd.PrimitiveInt)},
						{Name: sd.Name{Name: "ratio"}, Type: prim(sd.PrimitiveFloat)},
						{Name: sd.Name{Name: "active"}, Type: prim(sd.PrimitiveBool)},
						{Name: sd.Name{Name: "inner"}, Type: sd.Type{Kind: sd.TypeClass, Name: "Inner"}},
						{Name: sd.Name{Name: "color"}, Type: sd.Type{Kind: sd.TypeEnum, Name: "Color"}},
						{Name: sd.Name{Name: "tags"}, Type: list(prim(sd.PrimitiveString))},
						{Name: sd.Name{Name: "nested"}, Type: list(sd.Type{Kind: sd.TypeClass, Name: "Inner"})},
						{Name: sd.Name{Name: "nick"}, Type: optional(prim(sd.PrimitiveString))},
						{Name: sd.Name{Name: "scores"}, Type: strMap(prim(sd.PrimitiveInt))},
						// A string-VALUED map: exercises the map-value encoding path with an
						// HTML-bearing value ("<x>"), which must stay unescaped (EscapeHTML=false),
						// same as top-level string fields.
						{Name: sd.Name{Name: "labels"}, Type: strMap(prim(sd.PrimitiveString))},
					},
				},
				{
					Name:   sd.Name{Name: "Inner"},
					Fields: []sd.ClassField{{Name: sd.Name{Name: "label"}, Type: prim(sd.PrimitiveString)}},
				},
			},
		},
	}
}

// differentialTestSource is the test rendered into the hermetic module (which has
// NO baml_client/CFFI in its go.mod — only bamlutils and its deps, incl. sonic).
//
// It proves the emitted native carrier is byte-identical to what BAML ACTUALLY
// SERVES. The worker marshals final results with sonic.Marshal (worker/stream.go),
// i.e. sonic ConfigDefault: EscapeHTML=false, SortMapKeys=false. So this test
// marshals BOTH carriers with sonic (not encoding/json, whose EscapeHTML=true would
// hide the '<' -> < divergence), and compares:
//   - the native carrier (custom codec) vs a frozen golden literal, AND
//   - the native carrier vs a BAML-EQUIVALENT reference (plain struct with the same
//     json tags + a VALIDATING enum type mirroring a baml_client-generated enum,
//     which has no other MarshalJSON — confirmed at classes.go / enums.go in the
//     staticserve_fixture baml_client).
//
// Discriminating rows the earlier fixture lacked:
//   - name = "<x>": pins EscapeHTML=false (encoding/json would emit <x>).
//   - labels = {"k":"<x>"}: an HTML-bearing MAP VALUE, pinning that the map-value
//     encoding path also disables HTML escaping (single-key -> byte-identical).
//   - ratio = 1.23456789012345: a non-representable float that pins float64 (a
//     float32 regression would round it differently).
//   - a MULTI-KEY map (TestMultiKeyMapCanonicalAndSemantic): per the 2026-08-25
//     OWNER DECISION (Path 2), the parity relation for a >1-key map is canonical-
//     JSON-equality, NOT raw bytes — BAML's served map order is non-deterministic
//     (sonic SortMapKeys=false), so there is no single byte-string to reproduce.
//     Native emits the deterministic SORTED representative (asserted against a
//     frozen golden), which is a valid member of BAML's output set. Byte-identity
//     still holds exactly for 0- and 1-key maps and all non-map vocabulary.
//   - an out-of-range enum (TestEnumValidation): native and reference BOTH reject
//     "PURPLE" in both directions.
const differentialTestSource = `package kspkg

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// refColor mirrors a baml_client-generated enum carrier EXACTLY: a named string
// whose MarshalJSON/UnmarshalJSON reject values outside the declared members.
type refColor string

const (
	refColorRED   refColor = "RED"
	refColorGREEN refColor = "GREEN"
)

func (e refColor) IsValid() bool {
	switch e {
	case refColorRED, refColorGREEN:
		return true
	}
	return false
}
func (e refColor) MarshalJSON() ([]byte, error) {
	if !e.IsValid() {
		return nil, fmt.Errorf("invalid refColor: %q", string(e))
	}
	return json.Marshal(string(e))
}
func (e *refColor) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*e = refColor(s)
	if !e.IsValid() {
		return fmt.Errorf("invalid refColor: %q", s)
	}
	return nil
}

// refInner / refKS mirror the shape a BAML-generated Go type has: a plain struct
// with json tags in declaration order, optional as a pointer with no omitempty.
type refInner struct {
	Label string ` + "`json:\"label\"`" + `
}
type refKS struct {
	Name   string           ` + "`json:\"name\"`" + `
	Count  int64            ` + "`json:\"count\"`" + `
	Ratio  float64          ` + "`json:\"ratio\"`" + `
	Active bool             ` + "`json:\"active\"`" + `
	Inner  refInner         ` + "`json:\"inner\"`" + `
	Color  refColor         ` + "`json:\"color\"`" + `
	Tags   []string         ` + "`json:\"tags\"`" + `
	Nested []refInner       ` + "`json:\"nested\"`" + `
	Nick   *string           ` + "`json:\"nick\"`" + `
	Scores map[string]int64  ` + "`json:\"scores\"`" + `
	Labels map[string]string ` + "`json:\"labels\"`" + `
}

func nativeValue(nick *string) OutputKs {
	return OutputKs{
		Name:   "<x>",
		Count:  7,
		Ratio:  1.23456789012345,
		Active: true,
		Inner:  OutputInner{Label: "L"},
		Color:  OutputColorGreen,
		Tags:   []string{"a", "b"},
		Nested: []OutputInner{{Label: "A"}, {Label: "B"}},
		Nick:   nick,
		Scores: map[string]int64{"k": 1},
		Labels: map[string]string{"k": "<x>"},
	}
}
func refValue(nick *string) refKS {
	return refKS{
		Name:   "<x>",
		Count:  7,
		Ratio:  1.23456789012345,
		Active: true,
		Inner:  refInner{Label: "L"},
		Color:  refColorGREEN,
		Tags:   []string{"a", "b"},
		Nested: []refInner{{Label: "A"}, {Label: "B"}},
		Nick:   nick,
		Scores: map[string]int64{"k": 1},
		Labels: map[string]string{"k": "<x>"},
	}
}

const goldenNilNick = ` + "`{\"name\":\"<x>\",\"count\":7,\"ratio\":1.23456789012345,\"active\":true,\"inner\":{\"label\":\"L\"},\"color\":\"GREEN\",\"tags\":[\"a\",\"b\"],\"nested\":[{\"label\":\"A\"},{\"label\":\"B\"}],\"nick\":null,\"scores\":{\"k\":1},\"labels\":{\"k\":\"<x>\"}}`" + `
const goldenPresentNick = ` + "`{\"name\":\"<x>\",\"count\":7,\"ratio\":1.23456789012345,\"active\":true,\"inner\":{\"label\":\"L\"},\"color\":\"GREEN\",\"tags\":[\"a\",\"b\"],\"nested\":[{\"label\":\"A\"},{\"label\":\"B\"}],\"nick\":\"nn\",\"scores\":{\"k\":1},\"labels\":{\"k\":\"<x>\"}}`" + `

// goldenMultiKey is the FROZEN, deterministic native representative for a 3-key
// "scores" map: keys sorted (a,m,z). It is the canonical member of BAML's output
// SET that the native lane always emits (see TestMultiKeyMapCanonicalAndSemantic).
const goldenMultiKey = ` + "`{\"name\":\"<x>\",\"count\":7,\"ratio\":1.23456789012345,\"active\":true,\"inner\":{\"label\":\"L\"},\"color\":\"GREEN\",\"tags\":[\"a\",\"b\"],\"nested\":[{\"label\":\"A\"},{\"label\":\"B\"}],\"nick\":null,\"scores\":{\"a\":1,\"m\":13,\"z\":26},\"labels\":{\"k\":\"<x>\"}}`" + `

// TestDifferential marshals both carriers with the SERVING serializer (sonic
// ConfigDefault) and asserts native == golden == BAML-equivalent, then round-trips.
func TestDifferential(t *testing.T) {
	nn := "nn"
	for _, tc := range []struct {
		name   string
		nick   *string
		golden string
	}{
		{"nil optional", nil, goldenNilNick},
		{"present optional", &nn, goldenPresentNick},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, err := sonic.Marshal(nativeValue(tc.nick))
			if err != nil {
				t.Fatal(err)
			}
			if string(native) != tc.golden {
				t.Fatalf("native carrier JSON != golden\n native: %s\n golden: %s", native, tc.golden)
			}
			ref, err := sonic.Marshal(refValue(tc.nick))
			if err != nil {
				t.Fatal(err)
			}
			if string(ref) != string(native) {
				t.Fatalf("native carrier JSON != BAML-equivalent struct JSON (sonic)\n native: %s\n ref:    %s", native, ref)
			}
			// Round-trip: golden -> native carrier -> re-marshal -> byte-identical.
			var back OutputKs
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

// TestMultiKeyMapCanonicalAndSemantic pins the OWNER-SANCTIONED map contract for
// a >1-key map. This is NOT a workaround for a failing byte assertion; it asserts
// the parity relation the owner chose.
//
// OWNER DECISION (2026-08-25) — Path 2: for a map with more than one key the parity
// relation is canonical-JSON-equality (order-normalized), NOT raw-byte identity.
// Rationale: exact byte reproduction is UNDEFINED for a non-deterministic reference.
// BAML serves map keys in randomized Go iteration order (sonic ConfigDefault leaves
// SortMapKeys=false; worker/stream.go:141), so its output for a multi-key map is a
// SET of key-permutations, not one byte-string. The native lane emits the SORTED
// representative, which is a valid member of that set — this is not a parity
// relaxation and does not out-strict BAML (it accepts/rejects nothing differently;
// it merely picks a deterministic representative). Deterministic-sorted is also the
// correct native end-state once BAML is deleted and native becomes the reference.
// Byte-identity continues to hold EXACTLY for 0- and 1-key maps (TestDifferential
// asserts a 1-key map; the empty-map case below asserts a 0-key map) and for all
// non-map vocabulary.
//
// So this test asserts: (1) native emits a FROZEN, deterministic, sorted byte golden
// (determinism + canonical form), and (2) native equals the sonic-served reference
// under canonical (order-normalized) comparison. Raw-byte equality is INTENTIONALLY
// not asserted between native and the served multi-key map.
func TestMultiKeyMapCanonicalAndSemantic(t *testing.T) {
	scores := map[string]int64{"z": 26, "a": 1, "m": 13}
	native := nativeValue(nil)
	native.Scores = scores
	ref := refValue(nil)
	ref.Scores = scores

	nb, err := sonic.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	// (1a) Frozen canonical golden: whole-object byte-exact, keys sorted a<m<z.
	if string(nb) != goldenMultiKey {
		t.Fatalf("native multi-key map JSON != frozen sorted golden\n native: %s\n golden: %s", nb, goldenMultiKey)
	}
	// (1b) Determinism: a second marshal of the same value is byte-identical (Go map
	// iteration is randomized, so an unsorted codec would diverge here).
	nb2, err := sonic.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	if string(nb2) != string(nb) {
		t.Fatalf("native multi-key map is not deterministic across marshals\n a: %s\n b: %s", nb, nb2)
	}
	// (2) Canonical-JSON-equality with the sonic-served reference: decode both
	// (recursively, into any) and compare. This is the owner-sanctioned parity
	// relation for multi-key maps, not raw bytes (see the decision above).
	rb, err := sonic.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	var nAny, rAny map[string]any
	if err := json.Unmarshal(nb, &nAny); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rb, &rAny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nAny, rAny) {
		t.Fatalf("native and served differ under canonical equality\n native: %s\n served: %s", nb, rb)
	}

	// 0-key (empty) map: deterministic, so byte-identity DOES hold and is asserted.
	empty := nativeValue(nil)
	empty.Scores = map[string]int64{}
	eb, err := sonic.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eb), ` + "`\"scores\":{}`" + `) {
		t.Fatalf("empty map not emitted as {}: %s", eb)
	}
	er, err := sonic.Marshal(func() refKS { r := refValue(nil); r.Scores = map[string]int64{}; return r }())
	if err != nil {
		t.Fatal(err)
	}
	if string(er) != string(eb) {
		t.Fatalf("empty-map native != served (byte-identity must hold for 0 keys)\n native: %s\n served: %s", eb, er)
	}
}

// TestEnumValidation proves the native enum carrier matches the generated enum's
// validating behavior: in-range marshals/unmarshals, out-of-range is rejected in
// both directions, for BOTH the native carrier and the BAML-equivalent reference.
func TestEnumValidation(t *testing.T) {
	// In-range marshals identically on both carriers.
	nb, err := sonic.Marshal(OutputColorGreen)
	if err != nil || string(nb) != ` + "`\"GREEN\"`" + ` {
		t.Fatalf("native marshal GREEN = %s, %v", nb, err)
	}
	if rb, err := sonic.Marshal(refColorGREEN); err != nil || string(rb) != ` + "`\"GREEN\"`" + ` {
		t.Fatalf("ref marshal GREEN = %s, %v", rb, err)
	}
	// Out-of-range is rejected on marshal by both.
	if _, err := sonic.Marshal(OutputColor("PURPLE")); err == nil {
		t.Fatal("native carrier marshaled out-of-range enum PURPLE")
	}
	if _, err := sonic.Marshal(refColor("PURPLE")); err == nil {
		t.Fatal("reference carrier marshaled out-of-range enum PURPLE")
	}
	// Out-of-range is rejected on unmarshal by both.
	var nc OutputColor
	if err := json.Unmarshal([]byte(` + "`\"PURPLE\"`" + `), &nc); err == nil {
		t.Fatal("native carrier unmarshaled out-of-range enum PURPLE")
	}
	var rc refColor
	if err := json.Unmarshal([]byte(` + "`\"PURPLE\"`" + `), &rc); err == nil {
		t.Fatal("reference carrier unmarshaled out-of-range enum PURPLE")
	}
	// In-range unmarshals cleanly on the native carrier.
	var ok OutputColor
	if err := json.Unmarshal([]byte(` + "`\"RED\"`" + `), &ok); err != nil {
		t.Fatalf("native carrier rejected in-range enum RED: %v", err)
	}
}
`

// TestNativeCarrierDifferential is the M3a JSON-roundtrip differential: the emitted
// final carrier for the base type vocabulary serializes byte-identically to BAML's
// served-type contract (sonic ConfigDefault), compiles in a hermetic module with NO
// baml_client/CFFI import, and is deterministic.
func TestNativeCarrierDifferential(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping carrier differential")
	}
	m := kitchenSinkMethod()
	src, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "kspkg"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Determinism: a second emit is byte-identical.
	src2, err := EmitNativeStaticUnary(m, NativeSpineOptions{PackageName: "kspkg"})
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != string(src2) {
		t.Fatal("emitter is not deterministic for the kitchen-sink carrier")
	}
	// The emitted carrier must carry each base-vocabulary Go type and the
	// serving-parity machinery (EscapeHTML-off encoder, validating enum codec).
	// Assertions are alignment-insensitive (gofmt pads struct fields), so they
	// check standalone type expressions rather than field-name/type pairs.
	for _, want := range []string{
		"type OutputKs struct", "type OutputInner struct", "type OutputColor string",
		"[]OutputInner",                          // list of class
		"map[string]int64",                       // string-keyed map (int value)
		"map[string]string",                      // string-keyed map (string value)
		"*string",                                // optional
		"[]string",                               // list of primitive
		"float64",                                // the ratio field is a float64 (guards a float32 regression)
		`OutputColorGreen OutputColor = "GREEN"`, // enum carrier + canonical value
		"func (e OutputColor) IsValid() bool",    // validating enum codec
		"func (e OutputColor) MarshalJSON",       // "
		"func (e *OutputColor) UnmarshalJSON",    // "
		"enc.SetEscapeHTML(false)",               // serving-parity (sonic EscapeHTML=false)
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("emitted carrier missing %q", want)
		}
	}
	tmp := t.TempDir()
	testharness.WriteTempModule(t, tmp, string(src), map[string]string{"differential_test.go": differentialTestSource})

	// No-CFFI proof: the emitted carrier package's transitive (NON-test) dependency
	// graph links no baml_client, BAML runtime, patched dynclient, or CFFI symbol
	// (mirrors the M1 import-graph assertion, run here on the hermetic module). The
	// scan is package-only by design — `go list -deps` without -test walks the
	// emitted CARRIER's imports, not the differential harness's own test imports
	// (e.g. sonic); the carrier is the production artifact whose graph must be clean.
	assertNoCFFI(t, tmp)

	if out, err := testharness.RunGoTest(t, tmp, "TestDifferential|TestMultiKeyMapCanonicalAndSemantic|TestEnumValidation"); err != nil {
		t.Fatalf("carrier differential failed: %v\n%s", err, out)
	}
}

// assertNoCFFI runs `go list -deps ./...` in the hermetic module and fails if any
// dependency of the emitted carrier package is a forbidden BAML/CFFI path. It scans
// the NON-test dependency graph (no -test flag): the emitted carrier is non-test
// code, so its imports are exactly what must stay CFFI-free; the harness's own test
// imports are intentionally out of scope. stderr is captured and surfaced on error
// (cmd.Output alone discards it, hiding the real cause of a go-list failure).
func assertNoCFFI(t *testing.T, dir string) {
	t.Helper()
	forbidden := []string{"baml_client", "github.com/boundaryml/baml", "dynclient/baml-patched", "language_client_go"}
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v\nstderr:\n%s", err, stderr.String())
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Errorf("emitted carrier depends on %q (matches forbidden %q) — must link no baml_client/BAML/CFFI", dep, bad)
			}
		}
	}
}
