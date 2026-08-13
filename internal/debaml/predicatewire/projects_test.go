//go:build integration

// Package predicatewire is the de-BAML Slice 7.2c-1 STOCK v0.223.0 CFFI AUTHORITY for
// the six DIRECT COMPARISON predicates on the two name-pinned static families.
//
// # What it establishes, and what it does not
//
// Slice 7.2c proposes widening the admitted predicate of `StaticCheckedAnswer` /
// `StaticAssertAnswer` from `this > I` to the six direct comparisons
// `this OP I`, OP in {>, >=, <, <=, ==, !=}. That widening is EVIDENCE-GATED: an
// operator may be admitted (in 7.2c-3) only after fresh stock captures cover it. This
// package is that evidence, and NOTHING ELSE. It captures each operator in BOTH positions
// the scope requires — NESTED on the two pinned families (operators_test.go) and
// TOP-LEVEL on a bare `int` target (toplevel_test.go) — because a top-level check emits
// an unenclosed carrier and a top-level failing assert carries no required-field wrapper
// at all, neither of which is derivable from the nested rows.
//
// It flips no gate, widens no fingerprint and serves no new shape: the only admitted
// predicate is still `this > I`, which [TestPredicateWireAdmissionIsUnchanged] and
// [TestTopLevelOperatorFormsAreDeclined] re-assert through the production entry points
// for every operator and position captured here.
//
// # Why a separate package from checkedwire
//
// internal/debaml/checkedwire is the 7.2b byte authority. It builds ONE in-memory
// project, so its declarations must all coexist — and it namespaces every class with a
// `CW_` prefix for exactly that reason. The 7.2c question cannot be asked that way: one
// BAML project cannot declare six predicate variants of the SAME name-pinned class, and
// renaming the class to make them coexist would answer a question about
// `StaticGtePredicateAnswer` rather than about the admitted family. So this package
// builds ISOLATED PROJECTS — one runtime each, each declaring the two pinned names once
// — and keeps checkedwire's project, its golden and its SHA untouched.
//
// The names here are therefore the PRODUCTION names, unprefixed and unpinned-from:
// `StaticCheckedAnswer` and `StaticAssertAnswer`. That is the point of the isolation.
// The live `>=` sibling in internal/nativeprompt/testdata/staticserve_fixture is a
// DIFFERENT class (`StaticGtePredicateAnswer`) and stays a decline; nothing here
// repurposes it.
//
// # Stock is the authority; native output is never fed back in
//
// The only bytes handed to the CFFI are a fixture's raw assistant text. What comes back
// is compared against a pinned literal. The native leg — the evaluator in
// [TestDirectIntBoundaryMatrix], the gates in [TestPredicateWireAdmissionIsUnchanged] —
// is driven INDEPENDENTLY over the same captured source and value, and its output is
// never submitted to BAML as a validation step.
//
// # Running
//
//	CGO_ENABLED=1 go test -tags integration ./internal/debaml/predicatewire
//
// Requires CGO and the stock BAML v0.223.0 CFFI library (auto-located under the user
// BAML cache dir), exactly like the sibling oracles. .github/workflows/
// constraint-oracle.yml runs it on every change under internal/debaml/**.
//
// # Regenerating the project goldens
//
//	PREDICATE_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/debaml/predicatewire -run TestPredicateWireProjectDrift
package predicatewire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
	"github.com/boundaryml/baml/engine/language_client_go/pkg/cffi"
	"golang.org/x/mod/modfile"
)

const (
	// pwFuncPrefix namespaces every generated function so a probe name cannot collide
	// with a BAML keyword. CLASS names are deliberately NOT namespaced — see the
	// package doc.
	pwFuncPrefix = "PW_"
	// pwClient is the client every function names. It is never called — this drives
	// the PARSE entry point, not the LLM — but a BAML function must declare one.
	pwClient = "PredicateWireClient"

	// pwProjectFile is the single .baml file inside each isolated project, and
	// pwGoldenDir the directory of checked-in copies the drift test byte-compares
	// against.
	pwProjectFile = "predicate_wire.baml"
	pwGoldenDir   = "testdata/projects"

	// pwWriteEnv rewrites the goldens instead of comparing.
	pwWriteEnv = "PREDICATE_WIRE_FIXTURE_WRITE"

	// pwBAMLRuntimeVersion is the CFFI version every capture in this package is only
	// valid against.
	pwBAMLRuntimeVersion = "0.223.0"
	// rootGoModPath is the root module's go.mod, from this package's CWD.
	rootGoModPath         = "../../../go.mod"
	bamlModulePath        = "github.com/boundaryml/baml"
	wantBAMLModuleVersion = "v0.223.0"
)

// The two PINNED class names. They are the production fingerprint's names, written once
// here so a fixture cannot drift onto a renamed class and quietly answer a different
// question.
const (
	pwCheckedClass = "StaticCheckedAnswer"
	pwAssertClass  = "StaticAssertAnswer"
)

// pwPrelude is the fixed head of every isolated project.
const pwPrelude = `// GENERATED at test time from the Slice 7.2c-1 predicate-wire project table
// (projects_test.go) — do not edit.
//
// Regenerate with:
//   PREDICATE_WIRE_FIXTURE_WRITE=1 CGO_ENABLED=1 go test -tags integration \
//     ./internal/debaml/predicatewire -run TestPredicateWireProjectDrift

client<llm> ` + pwClient + ` {
  provider openai
  options {
    model "fake"
    api_key "fake"
    base_url "https://predicate-wire.invalid/v1"
  }
}
`

// ---------------------------------------------------------------------------
// The project model.
// ---------------------------------------------------------------------------

// pwFunc is one BAML function inside a project.
type pwFunc struct {
	// Name is the function suffix; the BAML function is PW_<Name>. It is unique
	// WITHIN a project, not across the table — two isolated projects deliberately
	// carry the same function name for the same role under a different predicate.
	Name string
	// Doc is rendered into the .baml as a comment, so each checked-in golden explains
	// what its declarations are for.
	Doc string
	// Target is the return-type expression, attributes included.
	Target string
}

// pwProject is ONE isolated in-memory BAML project with its OWN stock runtime.
//
// Isolation is the mechanism this package exists for: each project may declare the two
// pinned class names once, so six predicate variants of the same name-pinned class can
// be captured without ever renaming a class.
type pwProject struct {
	// Key names the project and its golden (testdata/projects/<Key>.baml).
	Key string
	// Doc is rendered into the golden's head.
	Doc string
	// Decls are whole class declarations in source form, emitted in order.
	Decls []string
	// Funcs are the functions, emitted in order.
	Funcs []pwFunc
	// WantCompile is whether stock's parser must ACCEPT this project.
	//
	// It is false for the literal-discriminator projects, whose whole point is that
	// stock may reject the attribute text outright. A rejection is a RECORDED FACT
	// with pinned error bytes, not a test failure — and a project that was expected
	// to be rejected and compiled anyway fails just as loudly as the reverse.
	WantCompile bool
}

func (f pwFunc) method() string { return pwFuncPrefix + f.Name }

// pwFuncDeclaration is the exact function declaration the renderer emits. Sharing the
// formatter lets a guard tie a probe to its own declaration bytes rather than finding a
// fragment somewhere else in the project.
func pwFuncDeclaration(f pwFunc) string {
	return fmt.Sprintf("function %s(topic: string) -> %s {\n  client %s\n  prompt #\"{{ topic }} {{ ctx.output_format }}\"#\n}\n",
		f.method(), f.Target, pwClient)
}

// pwRenderProject renders one isolated project. Declarations first in table order, then
// the functions, so the output is a pure function of the table and the drift test can
// byte-compare it.
func pwRenderProject(p pwProject) string {
	var b strings.Builder
	b.WriteString(pwPrelude)
	if p.Doc != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(p.Doc, "\n") {
			fmt.Fprintf(&b, "// %s\n", line)
		}
	}
	for _, d := range p.Decls {
		b.WriteString("\n")
		b.WriteString(d)
	}
	for _, f := range p.Funcs {
		b.WriteString("\n")
		if f.Doc != "" {
			for _, line := range strings.Split(f.Doc, "\n") {
				fmt.Fprintf(&b, "// %s\n", line)
			}
		}
		b.WriteString(pwFuncDeclaration(f))
	}
	return b.String()
}

// pwGoldenPath is the checked-in copy of a project's rendered source.
func pwGoldenPath(key string) string { return filepath.Join(pwGoldenDir, key+".baml") }

// pwProjectHash is the SHA-256 of a rendered project, in hex.
func pwProjectHash(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// pwProjects is every isolated project in the table, assembled from the per-concern
// tables in the files beside this one.
//
// The order is the order the goldens are reviewed in; nothing depends on it, because
// each project is rendered and hashed independently.
func pwProjects() []pwProject {
	var out []pwProject
	out = append(out, pwOperatorProjects()...)
	out = append(out, pwTopLevelProject())
	out = append(out, pwExprTextProject())
	out = append(out, pwLiteralProjects()...)
	out = append(out, pwBoundaryProjects()...)
	out = append(out, pwResidualProjects()...)
	return out
}

// pwProjectNamed looks a project up by key, failing loudly on an unknown one so a
// renamed project cannot silently stop being driven.
func pwProjectNamed(t *testing.T, key string) pwProject {
	t.Helper()
	for _, p := range pwProjects() {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("no project keyed %q", key)
	return pwProject{}
}

// ---------------------------------------------------------------------------
// The Go side of the generated shapes.
// ---------------------------------------------------------------------------

// pwUnexpectedFields records class fields the decoders below did not expect.
//
// Stock's GENERATED decoders panic on an unexpected field, and a panic on the CFFI
// callback thread is not recoverable by the caller — it kills the process. These record
// instead, and [TestPredicateWireDecodersSawWhatWasDeclared] fails on a non-empty
// record, so the substitution is not a silent weakening.
var pwUnexpectedFields []string

func pwUnexpected(class, key string) {
	pwUnexpectedFields = append(pwUnexpectedFields, class+"."+key)
}

// pwCheckedAnswer mirrors what BAML's Go generator emits for the CHECK family:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(...) }
//
// The mirror is not taken on trust — internal/debaml/checkedwire's
// TestGeneratedCarrierShapeMatchesStockCodegen grounds the identical declaration against
// the checked-in STOCK-GENERATED client under internal/debaml/testdata/constraint_oracle,
// and [TestPredicateWireMirrorsTheCheckedWireShape] here requires this package's mirror
// to stay field-for-field identical to that one.
type pwCheckedAnswer struct {
	Answer     string                `json:"answer"`
	Confidence shared.Checked[int64] `json:"confidence"`
}

func (c *pwCheckedAnswer) Decode(holder *cffi.CFFIValueClass, _ baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "answer":
			c.Answer, _ = baml.Decode(field.Value).Interface().(string)
		case "confidence":
			c.Confidence, _ = baml.Decode(field.Value).Interface().(shared.Checked[int64])
		default:
			pwUnexpected(pwCheckedClass, field.Key)
		}
	}
}

// pwAssertAnswer mirrors the ASSERT family:
//
//	class StaticAssertAnswer { answer string; confidence int @assert(...) }
//
// `confidence` is a BARE int64, not a wrapper: `as_check()` excludes an assert from the
// CFFI check list, so a PASSING assert leaves the generated field its ordinary Go type.
type pwAssertAnswer struct {
	Answer     string `json:"answer"`
	Confidence int64  `json:"confidence"`
}

func (c *pwAssertAnswer) Decode(holder *cffi.CFFIValueClass, _ baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "answer":
			c.Answer, _ = baml.Decode(field.Value).Interface().(string)
		case "confidence":
			c.Confidence, _ = baml.Decode(field.Value).Interface().(int64)
		default:
			pwUnexpected(pwAssertClass, field.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// The runtimes — one per isolated project.
// ---------------------------------------------------------------------------

// pwRuntimeEntry is one project's created runtime, or the creation error stock reported.
//
// The error is RETAINED rather than fatal because a project's compile disposition is
// part of what this package pins: the literal-discriminator projects exist to find out
// whether stock's parser accepts `{{ this > +5 }}` at all.
type pwRuntimeEntry struct {
	rt  baml.BamlRuntime
	err error
	src string
}

var (
	pwOnce      sync.Once
	pwRuntimes  map[string]*pwRuntimeEntry
	pwEnv       map[string]string
	pwSetupErr  error
	pwTypeMapMu sync.Once
)

// pwBuildTypeMap registers every name baml_go can ask for while decoding these
// projects' values.
//
// A MISSING key is a panic on the CFFI callback thread — a dead process with no
// attribution — so the registration is deliberately generous. The names this package
// depends on are typed precisely, because the captures compare against the concrete
// shape BAML's generator emits.
//
// The type map is PROCESS-GLOBAL while the runtimes are not, and that is exactly why
// every isolated project declares the two pinned classes with the SAME field types: one
// entry per class name serves all of them. A project whose `confidence` had a different
// Go shape could not share the name, which is the structural reason the type/shape
// residuals in residuals.md are characterised as ledger rows rather than as new CFFI
// projects here.
func pwBuildTypeMap() map[string]reflect.Type {
	anyChecked := reflect.TypeOf(shared.Checked[any]{})
	tm := map[string]reflect.Type{
		"CHECKED_TYPES.int":    reflect.TypeOf(shared.Checked[int64]{}),
		"CHECKED_TYPES.string": reflect.TypeOf(shared.Checked[string]{}),
	}
	for _, n := range []string{"float", "bool", "null", "class", "list", "map", "optional", "union", "checked", "stream_state"} {
		tm["CHECKED_TYPES."+n] = anyChecked
	}
	tm["TYPES."+pwCheckedClass] = reflect.TypeOf(pwCheckedAnswer{})
	tm["TYPES."+pwAssertClass] = reflect.TypeOf(pwAssertAnswer{})
	tm["CHECKED_TYPES."+pwCheckedClass] = anyChecked
	tm["CHECKED_TYPES."+pwAssertClass] = anyChecked
	return tm
}

// pwEnsureRuntimes renders every project and creates its runtime once per process.
func pwEnsureRuntimes(t *testing.T) {
	t.Helper()
	pwOnce.Do(func() {
		pwTypeMapMu.Do(func() { baml.SetTypeMap(pwBuildTypeMap()) })
		pwEnv = pwEnvSnapshot()
		pwRuntimes = map[string]*pwRuntimeEntry{}
		seen := map[string]bool{}
		for _, p := range pwProjects() {
			if seen[p.Key] {
				pwSetupErr = fmt.Errorf("two projects share the key %q", p.Key)
				return
			}
			seen[p.Key] = true
			src := pwRenderProject(p)
			rt, err := baml.CreateRuntime("./baml_src", map[string]string{pwProjectFile: src}, pwEnv)
			pwRuntimes[p.Key] = &pwRuntimeEntry{rt: rt, err: err, src: src}
		}
	})
	if pwSetupErr != nil {
		t.Fatalf("the predicate-wire project table is malformed: %v", pwSetupErr)
	}
}

// pwEnvSnapshot mirrors the generated client's getEnvVars. The client is unroutable and
// no LLM call is ever made; this is passed for parity with the real serving path.
func pwEnvSnapshot() map[string]string {
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

// pwRuntimeOf returns the runtime for a project that MUST have compiled.
func pwRuntimeOf(t *testing.T, key string) baml.BamlRuntime {
	t.Helper()
	pwEnsureRuntimes(t)
	e, ok := pwRuntimes[key]
	if !ok {
		t.Fatalf("no project keyed %q", key)
	}
	if e.err != nil {
		t.Fatalf("the stock runtime for project %q could not be created: %v\n"+
			"the project it renders is not a valid BAML v0.223.0 project:\n%s", key, e.err, e.src)
	}
	return e.rt
}

// pwCompileError returns the creation error for a project that must have been REJECTED.
func pwCompileError(t *testing.T, key string) error {
	t.Helper()
	pwEnsureRuntimes(t)
	e, ok := pwRuntimes[key]
	if !ok {
		t.Fatalf("no project keyed %q", key)
	}
	if e.err == nil {
		t.Fatalf("project %q COMPILED, but it is pinned as a project stock rejects:\n%s", key, e.src)
	}
	return e.err
}

// ---------------------------------------------------------------------------
// Guards.
// ---------------------------------------------------------------------------

// pwStockModuleFacts is what the toolchain ACTUALLY resolves the stock BAML module to,
// as opposed to what any one manifest asks for.
type pwStockModuleFacts struct {
	Version string
	// ReplacedBy is empty when the module is unreplaced, and otherwise names the path
	// (and version, for a module replacement) it resolves through.
	ReplacedBy string
}

// pwResolveStockModule asks the go command what [bamlModulePath] resolves to from this
// package's directory.
//
// It shells out on purpose: reimplementing module resolution would reproduce exactly the
// blind spot this check exists to close, and the go command is the same one that built
// this test binary. Adapted from internal/debaml/guardledger's resolveStockModule, which
// carries the self-test proving a go.work replacement really does surface here.
func pwResolveStockModule() (pwStockModuleFacts, error) {
	cmd := exec.Command("go", "list", "-m", "-f",
		"{{.Version}}\t{{with .Replace}}{{.Path}}@{{.Version}}{{end}}", bamlModulePath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return pwStockModuleFacts{}, fmt.Errorf("go list -m %s: %w (%s)",
			bamlModulePath, err, strings.TrimSpace(stderr.String()))
	}
	// Trim only the line terminator: an UNREPLACED module leaves the field after the tab
	// empty, and a general whitespace trim would eat the separator with it.
	version, replaced, found := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\t")
	if !found {
		return pwStockModuleFacts{}, fmt.Errorf("go list -m %s produced an unreadable line %q",
			bamlModulePath, string(out))
	}
	// A DIRECTORY replacement carries no version, so the raw form is `path@`; normalise
	// that to the bare path rather than reporting a stray separator.
	return pwStockModuleFacts{Version: version, ReplacedBy: strings.TrimSuffix(replaced, "@")}, nil
}

// pwActiveWorkspaceFile is the go.work in force for this build, or "" when the build is
// not in a workspace.
func pwActiveWorkspaceFile(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOWORK").Output()
	if err != nil {
		t.Fatalf("go env GOWORK: %v", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" || path == "off" {
		return ""
	}
	return path
}

// pwBAMLReplacements reports every replace directive in a manifest that names the stock
// module on EITHER side.
//
// Both positions matter and both are PARSED rather than pattern-matched, so the block
// form is caught as readily as the single-line one — the substring scan this replaced
// missed both. The module as the SOURCE (`replace github.com/boundaryml/baml => ...`)
// swaps the stock runtime out while leaving every version string intact; the module as
// the TARGET makes some other path resolve to it.
func pwBAMLReplacements(path string, content []byte, work bool) ([]string, error) {
	var replaces []*modfile.Replace
	if work {
		f, err := modfile.ParseWork(path, content, nil)
		if err != nil {
			return nil, err
		}
		replaces = f.Replace
	} else {
		f, err := modfile.Parse(path, content, nil)
		if err != nil {
			return nil, err
		}
		replaces = f.Replace
	}
	var out []string
	for _, r := range replaces {
		if r.Old.Path != bamlModulePath && r.New.Path != bamlModulePath {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s => %s %s", r.Old.Path, r.Old.Version, r.New.Path, r.New.Version))
	}
	return out, nil
}

// pwAssertNoBAMLReplacement fails if the named manifest replaces the stock module.
func pwAssertNoBAMLReplacement(t *testing.T, path string, work bool) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	found, err := pwBAMLReplacements(path, content, work)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(found) > 0 {
		t.Fatalf("%s replaces %s (%s); this package must link the STOCK module, and a same-version "+
			"fork would satisfy the CFFI version string and the require pin while invalidating every "+
			"capture in this package", path, bamlModulePath, strings.Join(found, "; "))
	}
}

// TestPredicateWireBAMLVersionPinned requires the CFFI runtime this binary loaded, AND
// the module the toolchain actually resolves, to be stock BAML v0.223.0 — so a captured
// byte string can never be attributed to a different BAML.
//
// THE EFFECTIVE RESOLUTION COMES FIRST, because it is the only check that cannot be
// routed around. A manifest scan answers "what does this file say"; `go list -m` answers
// "what will the toolchain actually link", after go.mod, the active go.work and any
// GOFLAGS have all had their say. A workspace replacement is invisible to the former and
// decisive for the latter — and a SAME-VERSION fork leaves `BamlVersion()` reading
// "0.223.0" and the root require line reading v0.223.0 while every byte in
// pwOperatorCaptures came from something else. That hostile configuration is precisely
// the one an authority package must reject itself rather than inherit from a tidy
// environment.
func TestPredicateWireBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != pwBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, pwBAMLRuntimeVersion)
	}

	res, err := pwResolveStockModule()
	if err != nil {
		t.Fatalf("resolve the effective %s module: %v\n"+
			"The pin cannot be verified, so no capture in this package can be attributed to stock BAML.",
			bamlModulePath, err)
	}
	if res.Version != wantBAMLModuleVersion {
		t.Fatalf("the toolchain resolves %s to %s, want exactly %s",
			bamlModulePath, res.Version, wantBAMLModuleVersion)
	}
	if res.ReplacedBy != "" {
		t.Fatalf("the toolchain resolves %s through a REPLACEMENT (%s); this package must link the "+
			"stock module, and a same-version fork would satisfy both the CFFI version string and the "+
			"manifest checks below while invalidating every captured byte string",
			bamlModulePath, res.ReplacedBy)
	}

	// The manifests are then checked directly, so a red run names the FILE that has to
	// change rather than only the effect. The active go.work is included because it is
	// the source a root-go.mod scan cannot see.
	pwAssertNoBAMLReplacement(t, rootGoModPath, false)
	if work := pwActiveWorkspaceFile(t); work != "" {
		pwAssertNoBAMLReplacement(t, work, true)
	}

	raw, err := os.ReadFile(rootGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", rootGoModPath, err)
	}
	f, err := modfile.Parse(rootGoModPath, raw, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", rootGoModPath, err)
	}
	required := false
	for _, r := range f.Require {
		if r.Mod.Path != bamlModulePath {
			continue
		}
		required = true
		if r.Mod.Version != wantBAMLModuleVersion {
			t.Fatalf("root go.mod requires %s %s, want exactly %s",
				bamlModulePath, r.Mod.Version, wantBAMLModuleVersion)
		}
	}
	if !required {
		t.Fatalf("root go.mod does not require %s %s", bamlModulePath, wantBAMLModuleVersion)
	}
}

// TestPredicateWireReplacementScanIsProvenToBite is the negative control for the scanner
// above: a hostile manifest must be REPORTED, in every form and on either side of the
// arrow.
//
// Without it the scan could be a no-op — which is exactly what the substring check it
// replaced was for the block form. The manifests are synthetic strings; nothing on disk
// is touched.
func TestPredicateWireReplacementScanIsProvenToBite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		work    bool
		content string
		want    bool
	}{{
		name: "go.mod replaces the stock module", content: "module x\n\ngo 1.21\n\nreplace " + bamlModulePath + " => ./fork\n", want: true,
	}, {
		name: "go.mod replaces it at the SAME version", content: "module x\n\ngo 1.21\n\nreplace " + bamlModulePath + " v0.223.0 => ./fork\n", want: true,
	}, {
		name: "go.mod block form", content: "module x\n\ngo 1.21\n\nreplace (\n\texample.com/a => ./a\n\t" + bamlModulePath + " => ./fork\n)\n", want: true,
	}, {
		name: "go.mod names it as the TARGET", content: "module x\n\ngo 1.21\n\nreplace example.com/other => " + bamlModulePath + " v0.223.0\n", want: true,
	}, {
		name: "clean go.mod", content: "module x\n\ngo 1.21\n\nrequire " + bamlModulePath + " v0.223.0\n", want: false,
	}, {
		name: "go.work replaces the stock module", work: true,
		content: "go 1.21\n\nuse .\n\nreplace " + bamlModulePath + " => ./fork\n", want: true,
	}, {
		name: "clean go.work", work: true, content: "go 1.21\n\nuse .\n", want: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			path := "go.mod"
			if tc.work {
				path = "go.work"
			}
			found, err := pwBAMLReplacements(path, []byte(tc.content), tc.work)
			if err != nil {
				t.Fatalf("parse the synthetic %s: %v", path, err)
			}
			if got := len(found) > 0; got != tc.want {
				t.Fatalf("replacement detected = %v, want %v (found %v)", got, tc.want, found)
			}
		})
	}
}

// TestPredicateWireProjectsAreIsolatedAndPinned is the structural invariant this whole
// package rests on: each project declares the PINNED class names, at most once each, and
// no project was renamed to make two predicate variants coexist.
//
// Without it, a future row could quietly reintroduce the `CW_`-style namespacing (or the
// live `StaticGtePredicateAnswer` name) and the captures would stop being about the
// admitted family.
func TestPredicateWireProjectsAreIsolatedAndPinned(t *testing.T) {
	projects := pwProjects()
	if len(projects) < 20 {
		t.Fatalf("the project table has %d entries; the operator, expression-text, literal, "+
			"boundary and residual groups together are substantially more", len(projects))
	}
	classDecl := 0
	for _, p := range projects {
		t.Run(p.Key, func(t *testing.T) {
			src := pwRenderProject(p)
			counts := map[string]int{}
			for _, line := range strings.Split(src, "\n") {
				rest, ok := strings.CutPrefix(line, "class ")
				if !ok {
					continue
				}
				name, _, ok := strings.Cut(rest, " ")
				if !ok || name == "" {
					t.Fatalf("unparseable class declaration: %q", line)
				}
				counts[name]++
			}
			for name, n := range counts {
				classDecl += n
				if name != pwCheckedClass && name != pwAssertClass {
					t.Errorf("declares class %q; only the two PINNED names %q and %q may be declared, "+
						"because a renamed class answers a question about a different family",
						name, pwCheckedClass, pwAssertClass)
				}
				if n != 1 {
					t.Errorf("declares class %q %d times in one project; a name-pinned class may be "+
						"declared at most once per isolated project", name, n)
				}
			}
			if len(p.Funcs) == 0 {
				t.Error("declares no function, so nothing about it can ever be driven")
			}
		})
	}
	if classDecl == 0 {
		t.Fatal("no project declares either pinned class; the nested-family captures would be vacuous")
	}
	t.Logf("%d isolated projects, %d pinned-class declarations across them", len(projects), classDecl)
}

// TestPredicateWireProjectDrift byte-compares every rendered project against its
// checked-in golden and pins its SHA-256, so a table change is visible in review and a
// golden cannot be regenerated without acknowledging it.
func TestPredicateWireProjectDrift(t *testing.T) {
	projects := pwProjects()
	if os.Getenv(pwWriteEnv) != "" {
		if err := os.MkdirAll(pwGoldenDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", pwGoldenDir, err)
		}
		keys := map[string]bool{}
		var lines []string
		for _, p := range projects {
			src := pwRenderProject(p)
			if err := os.WriteFile(pwGoldenPath(p.Key), []byte(src), 0o644); err != nil {
				t.Fatalf("write %s: %v", pwGoldenPath(p.Key), err)
			}
			keys[p.Key] = true
			lines = append(lines, fmt.Sprintf("\t%q: %q,", p.Key, pwProjectHash(src)))
		}
		// A golden whose project was DELETED would otherwise linger and keep being
		// reviewed as if it were live.
		stale, err := filepath.Glob(filepath.Join(pwGoldenDir, "*.baml"))
		if err != nil {
			t.Fatalf("glob %s: %v", pwGoldenDir, err)
		}
		for _, path := range stale {
			if !keys[strings.TrimSuffix(filepath.Base(path), ".baml")] {
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove stale golden %s: %v", path, err)
				}
			}
		}
		sort.Strings(lines)
		t.Logf("%s rewritten; update pwProjectSHA256 to:\n%s", pwGoldenDir, strings.Join(lines, "\n"))
		return
	}

	if len(pwProjectSHA256) != len(projects) {
		t.Fatalf("pwProjectSHA256 pins %d projects but the table has %d; every project's golden "+
			"must be pinned or a new one could arrive unreviewed", len(pwProjectSHA256), len(projects))
	}
	for _, p := range projects {
		t.Run(p.Key, func(t *testing.T) {
			src := pwRenderProject(p)
			golden, err := os.ReadFile(pwGoldenPath(p.Key))
			if err != nil {
				t.Fatalf("read %s: %v", pwGoldenPath(p.Key), err)
			}
			if string(golden) != src {
				t.Fatalf("%s is stale: it no longer matches the project table. Regenerate with %s=1.",
					pwGoldenPath(p.Key), pwWriteEnv)
			}
			want, ok := pwProjectSHA256[p.Key]
			if !ok {
				t.Fatalf("project %q has no pinned SHA-256", p.Key)
			}
			if got := pwProjectHash(src); got != want {
				t.Fatalf("project SHA-256 = %s, want %s; the golden changed without the pin being updated",
					got, want)
			}
		})
	}
	// A golden with no project behind it is dead review surface.
	files, err := filepath.Glob(filepath.Join(pwGoldenDir, "*.baml"))
	if err != nil {
		t.Fatalf("glob %s: %v", pwGoldenDir, err)
	}
	if len(files) != len(projects) {
		t.Fatalf("%s holds %d goldens for %d projects; a stale golden is dead review surface. "+
			"Regenerate with %s=1.", pwGoldenDir, len(files), len(projects), pwWriteEnv)
	}
}

// TestPredicateWireProjectsAreTheOnesStockDrives proves the bytes each runtime was
// created from are the bytes its golden pins, that a project's compile disposition is
// the pinned one, and that every declared function is present in the compiled source.
func TestPredicateWireProjectsAreTheOnesStockDrives(t *testing.T) {
	pwEnsureRuntimes(t)
	for _, p := range pwProjects() {
		t.Run(p.Key, func(t *testing.T) {
			e := pwRuntimes[p.Key]
			golden, err := os.ReadFile(pwGoldenPath(p.Key))
			if err != nil {
				t.Fatalf("read %s: %v", pwGoldenPath(p.Key), err)
			}
			if string(golden) != e.src {
				t.Fatal("the runtime was created from source that differs from the checked-in golden")
			}
			switch {
			case p.WantCompile && e.err != nil:
				t.Fatalf("stock REJECTED a project pinned as valid: %v", e.err)
			case !p.WantCompile && e.err == nil:
				t.Fatal("stock COMPILED a project pinned as one it rejects; the recorded rejection " +
					"fact is stale")
			}
			for _, f := range p.Funcs {
				if !strings.Contains(e.src, "function "+f.method()+"(") {
					t.Errorf("declares no function %s in the compiled project", f.method())
				}
			}
		})
	}
}

// TestPredicateWireDecodersSawWhatWasDeclared fails if any class decoder met a field it
// did not expect. Stock's generated decoders panic there; these record instead (a panic
// on the callback thread kills the process), so the record has to be asserted or the
// substitution would be a silent weakening.
func TestPredicateWireDecodersSawWhatWasDeclared(t *testing.T) {
	pwDriveEveryRow(t)
	if len(pwUnexpectedFields) != 0 {
		t.Fatalf("class decoders met unexpected field(s): %v", pwUnexpectedFields)
	}
}

// TestPredicateWireMirrorsTheCheckedWireShape requires this package's hand-written
// generated-shape mirrors to stay field-for-field identical to internal/debaml/
// checkedwire's, which are themselves grounded against the checked-in STOCK-GENERATED
// client (checkedwire's TestGeneratedCarrierShapeMatchesStockCodegen).
//
// Two copies of a generated shape can drift apart, and a drifted copy would still
// produce self-consistent bytes here — which is exactly how a capture stops being
// stock's. Comparing the SOURCE of the sibling declarations makes the grounding
// transitive instead of assumed.
func TestPredicateWireMirrorsTheCheckedWireShape(t *testing.T) {
	raw, err := os.ReadFile("../checkedwire/project_test.go")
	if err != nil {
		t.Fatalf("read the checkedwire mirror: %v", err)
	}
	src := string(raw)
	for _, want := range []struct{ what, decl string }{
		{"checked family", "Answer     string                `json:\"answer\"`\n\tConfidence shared.Checked[int64] `json:\"confidence\"`"},
		{"assert family", "Answer     string `json:\"answer\"`\n\tConfidence int64  `json:\"confidence\"`"},
	} {
		if !strings.Contains(src, want.decl) {
			t.Errorf("checkedwire no longer declares the %s mirror as this package does; the two "+
				"generated-shape copies have drifted and only checkedwire's is grounded against the "+
				"stock-generated client", want.what)
		}
	}
	// DISCRIMINATING: the comparison must be able to fail. A field type this package
	// does NOT use must be absent from what was matched, or "contains" would be
	// satisfied by any struct at all.
	if strings.Contains(src, "Confidence shared.Checked[float64]") {
		t.Fatal("checkedwire declares a float64 carrier mirror; this package's shape claim no longer " +
			"identifies a unique declaration")
	}
}
