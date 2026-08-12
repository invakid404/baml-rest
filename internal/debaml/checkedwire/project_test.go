//go:build integration

// Package checkedwire is the de-BAML Slice 7.2b-1 STOCK BYTE AUTHORITY for the
// `Checked[T]` carrier and for BAML's assertion-error text.
//
// # What it establishes, and why it comes first
//
// Slice 7.2b will let native serve a `@check`-bearing node. Before any of that is
// written, the BYTES stock v0.223.0 produces have to exist as a fixture, captured
// from the real CFFI rather than inferred from the Rust source: the wire form of a
// checked value under the worker's own serializer, and the UNMODIFIED `err.Error()`
// of a failed `@assert`. This package is that capture. It claims nothing about
// native behaviour — no admission gate changes in this slice, and every
// constraint-bearing bundle still declines to BAML (which the three asymmetry rows
// here re-assert through the exported entry points, and which
// internal/debaml's own TestServingOracleBoundaryLock pins in full).
//
// # Stock is the authority; native output is never fed back in
//
// The only bytes handed to the CFFI are a fixture's raw assistant text. What comes
// back is compared against a pinned literal, and — for the wire fixtures —
// against [bamlutils.Checked] built from stock's OWN decoded check results. Native
// JSON is never re-parsed by BAML as a validation step.
//
// # Why a separate package from internal/debaml
//
// The #665 serving oracle (in package debaml) pins a byte-compared project golden and
// a 49-row boundary lock over its corpus. Adding fixtures there would move both. This
// package builds its own in-memory project and its own runtime, so the #665 corpus,
// its golden and its lock are untouched by this slice.
//
// # Running
//
//	CGO_ENABLED=1 go test -tags integration ./internal/debaml/checkedwire
//
// Requires CGO and the stock BAML v0.223.0 CFFI library (auto-located under the user
// BAML cache dir), exactly like the #649/#665 oracles. .github/workflows/
// constraint-oracle.yml runs it on every change under internal/debaml/**.
//
// # Regenerating the project golden
//
//	CHECKED_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/debaml/checkedwire -run TestCheckedWireProjectDrift
package checkedwire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/boundaryml/baml/engine/language_client_go/pkg/cffi"
)

const (
	// cwPrefix namespaces every declaration so a fixture name cannot collide with a
	// BAML keyword.
	cwPrefix = "CW_"
	// cwClient is the client every function names. It is never called — this drives
	// the PARSE entry point, not the LLM — but a BAML function must declare one.
	cwClient = "CheckedWireClient"

	// cwProjectFile is the single .baml file of the in-memory project, and
	// cwProjectGolden the checked-in copy the drift test byte-compares it against.
	cwProjectFile   = "checked_wire.baml"
	cwProjectGolden = "testdata/project.baml"

	// cwWriteEnv rewrites the golden instead of comparing.
	cwWriteEnv = "CHECKED_WIRE_FIXTURE_WRITE"

	// cwBAMLRuntimeVersion is the CFFI version every capture in this package is only
	// valid against.
	cwBAMLRuntimeVersion = "0.223.0"
	// rootGoModPath is the root module's go.mod, from this package's CWD.
	rootGoModPath         = "../../../go.mod"
	bamlModulePath        = "github.com/boundaryml/baml"
	wantBAMLModuleVersion = "v0.223.0"
)

// The three rewrites encoding/json applies to the output of ANY json.Marshaler, and
// which sonic's default config does not. Stock's own canonical expression `this > 0`
// carries a `>`, so this reaches every check fixture here; sonic is the wire route and
// therefore the acceptance authority.
const (
	cwEscapedLT  = "\\u003c"
	cwEscapedGT  = "\\u003e"
	cwEscapedAmp = "\\u0026"
)

// cwPrelude is the fixed head of the project.
const cwPrelude = `// GENERATED at test time from the Slice 7.2b-1 checked-wire fixture table
// (fixtures_test.go) — do not edit.
//
// Regenerate with:
//   CHECKED_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml/checkedwire -run TestCheckedWireProjectDrift

client<llm> ` + cwClient + ` {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://checked-wire.invalid/v1"
  }
}
`

// cwFixture is one BAML function driven through the stock CFFI.
type cwFixture struct {
	// Name is the function suffix; the BAML function is CW_<Name>.
	Name string
	// Doc is rendered into the .baml as a comment, so the checked-in golden explains
	// what each declaration is for.
	Doc string
	// Classes are whole class declarations this fixture needs, in source form.
	// Identical sources contributed by two fixtures are emitted once.
	Classes []string
	// Target is the return-type expression, attributes included.
	Target string
	// Raw is the assistant text handed to Parse. It is the ONLY thing this package
	// ever gives the CFFI.
	Raw string
}

func (f cwFixture) method() string { return cwPrefix + f.Name }

// cwFixtureDeclaration is the exact function declaration cwRenderProject puts in the
// source it hands to the stock runtime. Keeping this formatter shared lets guards tie
// a row to its own declaration bytes rather than finding a fragment somewhere else in
// the whole project.
func cwFixtureDeclaration(f cwFixture) string {
	return fmt.Sprintf("function %s(topic: string) -> %s {\n  client %s\n  prompt #\"{{ topic }} {{ ctx.output_format }}\"#\n}\n",
		f.method(), f.Target, cwClient)
}

// cwRenderProject renders the whole in-memory project from the fixture table.
//
// Declarations come first in first-contribution order, then the functions in table
// order, so the output is a pure function of the table and the drift test can
// byte-compare it.
func cwRenderProject(fixtures []cwFixture) (string, error) {
	var b strings.Builder
	b.WriteString(cwPrelude)

	seen := map[string]string{}
	var order []string
	for _, f := range fixtures {
		for _, src := range f.Classes {
			name, err := cwClassName(src)
			if err != nil {
				return "", fmt.Errorf("fixture %s: %w", f.Name, err)
			}
			prev, ok := seen[name]
			if ok {
				if prev != src {
					return "", fmt.Errorf("class %q is declared differently by two fixtures:\n%s\n%s", name, prev, src)
				}
				continue
			}
			seen[name] = src
			order = append(order, name)
		}
	}
	for _, name := range order {
		b.WriteString("\n")
		b.WriteString(seen[name])
	}
	for _, f := range fixtures {
		b.WriteString("\n")
		if f.Doc != "" {
			for _, line := range strings.Split(f.Doc, "\n") {
				fmt.Fprintf(&b, "// %s\n", line)
			}
		}
		b.WriteString(cwFixtureDeclaration(f))
	}
	return b.String(), nil
}

// cwClassName extracts the declared name from a class source, so the dedupe above is
// keyed on the declaration rather than on a name the table would have to repeat.
func cwClassName(src string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(src), "class ")
	if !ok {
		return "", fmt.Errorf("class source does not begin with `class `: %q", src)
	}
	name, _, ok := strings.Cut(rest, " ")
	if !ok || name == "" {
		return "", fmt.Errorf("class source has no name: %q", src)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// The Go side of the generated shapes.
// ---------------------------------------------------------------------------

// cwUnexpectedFields records class fields the decoders below did not expect.
//
// Stock's GENERATED decoders panic on an unexpected field. A panic on the CFFI
// callback thread is not recoverable by the caller — it kills the process, which is
// the property that makes `divisibleby(0)` process-fatal — so these record instead,
// and [TestCheckedWireDecodersSawWhatWasDeclared] fails on a non-empty record. Every
// call in this package is sequential, so no lock is needed.
var cwUnexpectedFields []string

func cwUnexpected(class, key string) {
	cwUnexpectedFields = append(cwUnexpectedFields, class+"."+key)
}

// cwStaticCheckedAnswer mirrors what BAML's Go generator emits for
// `class CW_StaticCheckedAnswer { answer string; confidence int @check(...) }`.
//
// The mirror is not taken on trust: [TestGeneratedCarrierShapeMatchesStockCodegen]
// compares this declaration against the checked-in STOCK-GENERATED client under
// internal/debaml/testdata/constraint_oracle, which BAML v0.223.0 itself produced for
// a class with a checked int field.
type cwStaticCheckedAnswer struct {
	Answer     string                `json:"answer"`
	Confidence shared.Checked[int64] `json:"confidence"`
}

func (c *cwStaticCheckedAnswer) Decode(holder *cffi.CFFIValueClass, _ baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "answer":
			c.Answer, _ = baml.Decode(field.Value).Interface().(string)
		case "confidence":
			c.Confidence, _ = baml.Decode(field.Value).Interface().(shared.Checked[int64])
		default:
			cwUnexpected("CW_StaticCheckedAnswer", field.Key)
		}
	}
}

// cwRequiredAssert is the wrapper-chain fixture's class: one REQUIRED int field
// carrying a failing @assert, which is the shape that makes stock wrap the assertion
// error in its required-fields / field-coercion chain.
type cwRequiredAssert struct {
	V int64 `json:"v"`
}

func (c *cwRequiredAssert) Decode(holder *cffi.CFFIValueClass, _ baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "v":
			c.V, _ = baml.Decode(field.Value).Interface().(int64)
		default:
			cwUnexpected("CW_RequiredAssert", field.Key)
		}
	}
}

// cwAliasedChecked is the ALIAS-ingress asymmetry row: the field is named `qty` in
// BAML and ingested as `amount`.
type cwAliasedChecked struct {
	Qty shared.Checked[int64] `json:"qty"`
}

func (c *cwAliasedChecked) Decode(holder *cffi.CFFIValueClass, _ baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "qty":
			c.Qty, _ = baml.Decode(field.Value).Interface().(shared.Checked[int64])
		default:
			cwUnexpected("CW_AliasedChecked", field.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// The runtime.
// ---------------------------------------------------------------------------

var (
	cwOnce   sync.Once
	cwRt     baml.BamlRuntime
	cwErr    error
	cwSource string
	cwEnv    map[string]string
	cwTypes  map[string]reflect.Type
)

// cwBuildTypeMap registers every name baml_go can ask for while decoding this
// project's values.
//
// A MISSING key is a panic on the CFFI callback thread — a dead process with no
// attribution — so the registration is deliberately generous. The two names this
// package actually depends on are typed precisely, because the wire fixtures compare
// against the concrete shape BAML's generator emits:
//
//	CHECKED_TYPES.int    -> shared.Checked[int64]
//	CHECKED_TYPES.string -> shared.Checked[string]
//
// Everything else maps to shared.Checked[any], whose Value field accepts any decoded
// value; that is a panic guard, not a claim about a shape.
func cwBuildTypeMap() map[string]reflect.Type {
	anyChecked := reflect.TypeOf(shared.Checked[any]{})
	tm := map[string]reflect.Type{
		"CHECKED_TYPES.int":    reflect.TypeOf(shared.Checked[int64]{}),
		"CHECKED_TYPES.string": reflect.TypeOf(shared.Checked[string]{}),
	}
	for _, n := range []string{"float", "bool", "null", "class", "list", "map", "optional", "union", "checked", "stream_state"} {
		tm["CHECKED_TYPES."+n] = anyChecked
	}
	tm["TYPES.CW_StaticCheckedAnswer"] = reflect.TypeOf(cwStaticCheckedAnswer{})
	tm["TYPES.CW_RequiredAssert"] = reflect.TypeOf(cwRequiredAssert{})
	tm["TYPES.CW_AliasedChecked"] = reflect.TypeOf(cwAliasedChecked{})
	for _, n := range []string{"CW_StaticCheckedAnswer", "CW_RequiredAssert", "CW_AliasedChecked"} {
		tm["CHECKED_TYPES."+n] = anyChecked
	}
	return tm
}

// cwEnsureRuntime renders the project and creates the stock runtime once per process.
func cwEnsureRuntime(t *testing.T) {
	t.Helper()
	cwOnce.Do(func() {
		cwSource, cwErr = cwRenderProject(cwFixtures)
		if cwErr != nil {
			return
		}
		cwTypes = cwBuildTypeMap()
		baml.SetTypeMap(cwTypes)
		cwEnv = cwEnvSnapshot()
		cwRt, cwErr = baml.CreateRuntime("./baml_src", map[string]string{cwProjectFile: cwSource}, cwEnv)
	})
	if cwErr != nil {
		t.Fatalf("the stock runtime for the checked-wire project could not be created: %v\n"+
			"the project the fixture table renders is not a valid BAML v0.223.0 project:\n%s", cwErr, cwSource)
	}
}

// cwEnvSnapshot mirrors the generated client's getEnvVars. The client is unroutable
// and no LLM call is ever made; this is passed for parity with the real serving path.
func cwEnvSnapshot() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		env[k] = v
	}
	return env
}

// cwProjectHash is the SHA-256 of the rendered project, in hex, pinned beside the
// golden so it cannot be regenerated silently.
func cwProjectHash(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Guards.
// ---------------------------------------------------------------------------

// TestCheckedWireBAMLVersionPinned requires the LOADED CFFI runtime and the root
// go.mod pin to be exactly v0.223.0, so a captured byte string can never be
// attributed to a different BAML.
func TestCheckedWireBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != cwBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, cwBAMLRuntimeVersion)
	}
	raw, err := os.ReadFile(rootGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", rootGoModPath, err)
	}
	if !strings.Contains(string(raw), bamlModulePath+" "+wantBAMLModuleVersion) {
		t.Fatalf("root go.mod does not require %s %s", bamlModulePath, wantBAMLModuleVersion)
	}
	if strings.Contains(string(raw), "=> "+bamlModulePath) {
		t.Fatalf("root go.mod replaces %s; this package must link the stock module", bamlModulePath)
	}
}

// TestCheckedWireProjectDrift byte-compares the rendered project against the
// checked-in golden and pins its SHA-256, so a fixture-table change is visible in
// review and the golden cannot be regenerated without acknowledging it.
func TestCheckedWireProjectDrift(t *testing.T) {
	src, err := cwRenderProject(cwFixtures)
	if err != nil {
		t.Fatalf("render project: %v", err)
	}
	if os.Getenv(cwWriteEnv) != "" {
		if err := os.WriteFile(cwProjectGolden, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", cwProjectGolden, err)
		}
		t.Logf("%s rewritten; its SHA-256 is now %s — update cwProjectSHA256", cwProjectGolden, cwProjectHash(src))
		return
	}
	golden, err := os.ReadFile(cwProjectGolden)
	if err != nil {
		t.Fatalf("read %s: %v", cwProjectGolden, err)
	}
	if string(golden) != src {
		t.Fatalf("%s is stale: it no longer matches the fixture table. Regenerate with %s=1.", cwProjectGolden, cwWriteEnv)
	}
	if got := cwProjectHash(src); got != cwProjectSHA256 {
		t.Fatalf("project SHA-256 = %s, want %s; the golden changed without the pin being updated", got, cwProjectSHA256)
	}
}

// TestCheckedWireProjectIsTheOneStockDrives proves the bytes the runtime was created
// from are the bytes the golden pins, and that every fixture's function is present in
// them — so a fixture cannot silently drive a function the golden does not describe.
func TestCheckedWireProjectIsTheOneStockDrives(t *testing.T) {
	cwEnsureRuntime(t)
	golden, err := os.ReadFile(cwProjectGolden)
	if err != nil {
		t.Fatalf("read %s: %v", cwProjectGolden, err)
	}
	if string(golden) != cwSource {
		t.Fatal("the runtime was created from source that differs from the checked-in golden")
	}
	if len(cwFixtures) == 0 {
		t.Fatal("the fixture table is empty; every claim in this package would be vacuous")
	}
	for _, f := range cwFixtures {
		if !strings.Contains(cwSource, "function "+f.method()+"(") {
			t.Errorf("fixture %s declares no function %s in the compiled project", f.Name, f.method())
		}
	}
}

// TestCheckedWireDecodersSawWhatWasDeclared fails if any class decoder met a field it
// did not expect. Stock's generated decoders panic there; these record instead (a
// panic on the callback thread kills the process), so the record has to be asserted
// or the substitution would be a silent weakening.
func TestCheckedWireDecodersSawWhatWasDeclared(t *testing.T) {
	cwDriveAll(t)
	if len(cwUnexpectedFields) != 0 {
		t.Fatalf("class decoders met unexpected field(s): %v", cwUnexpectedFields)
	}
}
