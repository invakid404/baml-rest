// Command gen-native-spine-worker emits the DEPLOYMENT-SPECIFIC native spine registry
// for the ExecBridge packaged workers — BOTH the native-only artifact (via the emitted
// NewRuntime) AND the ExecBridge-U1c standard native-capable serve worker (via the
// emitted NewExecutor). It consumes the JSON emitted by
// `cmd/introspect --native-spine-descriptors` (a projectdescriptor.Project) and
// generates, into the extracted nanollmprepare build tree:
//
//   - one deterministic collision-proof subpackage per codegen-admitted static
//     method, produced by adapters/common/codegen.EmitNativeStaticUnary for a
//     ClassStaticUnary method and EmitNativeStaticStream for a ClassStaticStream one
//     (a subpackage boundary is required because every emitted file exports generic
//     names such as MethodName, BuildMethod, and Binding);
//
//   - nativegenerated/project.json, embedded by the aggregate registry package;
//
//   - a deterministic aggregate with TWO candidate projections (nativegenerated/
//     generated.go, under the debamlnativespinegenerated build tag) that imports every
//     emitted subpackage:
//
//     descriptor methods
//     ├─ unaryCandidates: BOTH classes' Binding()       -> NewExecutor  -> spine.NewPopulationExecutor (/call only, empty allowed)
//     └─ streamCandidates: ClassStaticStream's          -> NewRuntime   -> spine.NewWorkerRuntime      (call + streams, empty is a boot error)
//     StreamBinding() only
//
// The split is deliberate: accepting a stream-class method's UNARY projection in the
// standard composite preserves the existing /call behaviour across the v3 descriptor
// bump; it is NOT standard stream enrollment, which is a later slice.
//
// It emits CANDIDATES, not a rollout/cohort manifest: runtime registry membership
// is decided downstream by spine.NewWorkerRuntime's single U1 classifier, so later
// slices grow the cohort by widening that classifier while this generator's output
// format and the bootstrap stay unchanged.
//
// Generation CLEANS its output directory first (every prior generated subpackage,
// generated.go, and project.json) so a removed method cannot survive as a stale
// registration; the committed generated_off.go stub is left in place.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
)

// aggregateFileName is the generated aggregate registry file (tag-gated ON).
const aggregateFileName = "generated.go"

// projectJSONFileName is the embedded deployment descriptor.
const projectJSONFileName = "project.json"

// stubFileName is the committed fail-loud stub (tag-gated OFF). Generation never
// touches it.
const stubFileName = "generated_off.go"

// defaultRegistryPackagePath is the canonical import path of the aggregate
// registry package in the isolated nanollmprepare module. It is the base for the
// emitted per-method subpackage import paths. It is overridable (--package-path)
// only so the generator's own tests can emit a self-describing golden without
// pretending to live at this path.
const defaultRegistryPackagePath = "github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated"

// registryPackageName is the Go package name of the aggregate registry.
const registryPackageName = "nativegenerated"

// buildTag is the PROFILE-NEUTRAL tag under which the generated registry is the real
// implementation; without it the committed stub compiles and fails loud. Both the
// native-only artifact (via NewRuntime) AND the standard native-capable serve worker
// (via NewExecutor) are built with it, so its name says "native spine", not "native
// only".
const buildTag = "debamlnativespinegenerated"

// emittedMethod is one codegen-admitted static method plus its resolved,
// collision-proof subpackage identity.
type emittedMethod struct {
	method     projectdescriptor.Method
	dirName    string // subpackage directory name (== package name)
	importPath string // full import path of the subpackage
	fileName   string // the emitted source file name inside the subpackage
	source     []byte // the class emitter's output
}

// stream reports whether this method was emitted through the STREAM emitter and
// therefore exports StreamBinding().
func (e emittedMethod) stream() bool {
	return e.method.Class == projectdescriptor.ClassStaticStream
}

// Generate reads a projectdescriptor.Project from descriptorsJSON, validates it,
// and writes the deployment-specific native spine registry into outDir. packagePath is
// the import path outDir is built at (defaultRegistryPackagePath in production).
//
// allowEmpty selects the profile's empty-population semantics. With allowEmpty FALSE
// (the native-only artifact) a project that yields NO codegen-admitted STREAM-capable
// candidate is REFUSED — the native-only worker serves through NewWorkerRuntime, which
// requires the full stream surface, so a project with only unary-class methods could not
// boot it. With allowEmpty TRUE (the ExecBridge-U1c standard composite) an empty
// population is permitted: NewExecutor yields an all-decline executor whose every /call
// falls back to BAML, so an ordinary BAML project does not become unbuildable merely
// because its standard artifact has nothing in U1. It fails on an invalid descriptor and
// any emitter error, and cleans the output directory first.
func Generate(descriptorsJSON []byte, outDir, packagePath string, allowEmpty bool) error {
	var proj projectdescriptor.Project
	if err := json.Unmarshal(descriptorsJSON, &proj); err != nil {
		return fmt.Errorf("gen-native-spine-worker: decode descriptor JSON: %w", err)
	}
	if err := proj.Validate(); err != nil {
		return fmt.Errorf("gen-native-spine-worker: invalid project descriptor: %w", err)
	}

	emitted, err := emitMethods(proj, packagePath)
	if err != nil {
		return err
	}
	if !allowEmpty && countStream(emitted) == 0 {
		return fmt.Errorf("gen-native-spine-worker: project has no codegen-admitted %s method; a native-only worker serves through NewWorkerRuntime and needs at least one stream-capable candidate", projectdescriptor.ClassStaticStream)
	}

	if err := cleanOutputDir(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("gen-native-spine-worker: create output dir: %w", err)
	}

	// Per-method subpackages.
	for _, em := range emitted {
		dir := filepath.Join(outDir, em.dirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("gen-native-spine-worker: create subpackage dir %q: %w", em.dirName, err)
		}
		if err := os.WriteFile(filepath.Join(dir, em.fileName), em.source, 0o644); err != nil {
			return fmt.Errorf("gen-native-spine-worker: write %q: %w", em.fileName, err)
		}
	}

	// The embedded deployment descriptor, canonically rendered.
	projJSON, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return fmt.Errorf("gen-native-spine-worker: marshal project descriptor: %w", err)
	}
	projJSON = append(projJSON, '\n')
	if err := os.WriteFile(filepath.Join(outDir, projectJSONFileName), projJSON, 0o644); err != nil {
		return fmt.Errorf("gen-native-spine-worker: write %s: %w", projectJSONFileName, err)
	}

	// The aggregate registry.
	agg, err := renderAggregate(emitted)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, aggregateFileName), agg, 0o644); err != nil {
		return fmt.Errorf("gen-native-spine-worker: write %s: %w", aggregateFileName, err)
	}
	return nil
}

// countStream returns how many emitted methods carry the stream class.
func countStream(emitted []emittedMethod) int {
	n := 0
	for _, em := range emitted {
		if em.stream() {
			n++
		}
	}
	return n
}

// emitMethods produces the deterministic, method-name-sorted set of emitted
// per-method candidates, DISPATCHING each method to its class emitter. Every
// codegen-admitted static method becomes a candidate; membership in the served runtime
// is the runtime classifier's call.
func emitMethods(proj projectdescriptor.Project, packagePath string) ([]emittedMethod, error) {
	// Sort by method name so the emitted set and the aggregate import order are
	// deterministic regardless of descriptor method order.
	methods := make([]projectdescriptor.Method, len(proj.Methods))
	copy(methods, proj.Methods)
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })

	out := make([]emittedMethod, 0, len(methods))
	seenDir := make(map[string]string, len(methods))
	for _, m := range methods {
		if m.Class != projectdescriptor.ClassStaticUnary && m.Class != projectdescriptor.ClassStaticStream {
			// Only the two known native classes are emittable; anything else is not a
			// candidate. A class this build does not know never becomes one silently.
			continue
		}
		dir := subpackageDirName(m.Name)
		if prev, dup := seenDir[dir]; dup {
			// Two method names collided to the same collision-proof suffix — a hash
			// collision, which must never happen but must fail loud if it ever does.
			return nil, fmt.Errorf("gen-native-spine-worker: subpackage name collision %q for methods %q and %q", dir, prev, m.Name)
		}
		seenDir[dir] = m.Name

		var src []byte
		var err error
		if m.Class == projectdescriptor.ClassStaticStream {
			src, err = codegen.EmitNativeStaticStream(m, codegen.NativeSpineOptions{PackageName: dir})
		} else {
			src, err = codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: dir})
		}
		if err != nil {
			return nil, fmt.Errorf("gen-native-spine-worker: emit %q: %w", m.Name, err)
		}
		out = append(out, emittedMethod{
			method:     m,
			dirName:    dir,
			importPath: packagePath + "/" + dir,
			fileName:   "generated_" + sanitizeLower(m.Name) + ".go",
			source:     src,
		})
	}
	return out, nil
}

// subpackageDirName returns a deterministic, collision-proof Go-identifier package
// (and directory) name for a method: the sanitized lowercase method name plus a
// suffix of the method name's SHA-256, so two methods that sanitize to the same
// string still land in distinct packages. It is NOT only a sanitized method name.
func subpackageDirName(method string) string {
	sum := sha256.Sum256([]byte(method))
	suffix := hex.EncodeToString(sum[:])[:12]
	san := sanitizeLower(method)
	if san == "" {
		san = "method"
	}
	return "m" + san + "_" + suffix
}

// sanitizeLower reduces s to lowercase [a-z0-9], dropping every other rune. The
// hash suffix in subpackageDirName guarantees uniqueness, so a lossy sanitizer is
// safe here.
func sanitizeLower(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderAggregate produces the gofmt'd aggregate registry source: the embedded
// descriptor, the deterministic candidate list, and NewRuntime.
func renderAggregate(emitted []emittedMethod) ([]byte, error) {
	var b strings.Builder
	b.WriteString("//go:build " + buildTag + "\n\n")
	b.WriteString("// Code generated by cmd/gen-native-spine-worker; DO NOT EDIT.\n\n")
	b.WriteString("package " + registryPackageName + "\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t_ \"embed\"\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"fmt\"\n\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/bamlutils\"\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/bamlutils/projectdescriptor\"\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/nativeserve/spine\"\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/worker\"\n")
	if len(emitted) > 0 {
		b.WriteString("\n")
		for i, em := range emitted {
			fmt.Fprintf(&b, "\tp%d %q\n", i, em.importPath)
		}
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "//go:embed %s\n", projectJSONFileName)
	b.WriteString("var projectDescriptorJSON []byte\n\n")

	b.WriteString("// unaryCandidates is the deterministic aggregate UNARY candidate list: one emitted\n")
	b.WriteString("// per-method registration in method-name order, for BOTH admitted classes (a\n")
	b.WriteString("// static_stream method explicitly INCLUDES unary /call, so its Binding() belongs\n")
	b.WriteString("// here too). Empty when the project has no codegen-admitted static method.\n")
	b.WriteString("// Membership in the SERVED population is NOT decided here — the single classifier\n")
	b.WriteString("// in nativeserve/spine owns that. These are CANDIDATES only; there is no fallback\n")
	b.WriteString("// slot.\n")
	b.WriteString("func unaryCandidates() []spine.UnaryRegistration {\n")
	b.WriteString("\treturn []spine.UnaryRegistration{\n")
	for i := range emitted {
		fmt.Fprintf(&b, "\t\t{Binding: p%d.Binding(), BuildMethod: p%d.BuildMethod},\n", i, i)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	b.WriteString("// streamCandidates is the deterministic aggregate STREAM candidate list: ONLY the\n")
	b.WriteString("// static_stream methods, carrying the emitted StreamBinding() (whose Unary field is\n")
	b.WriteString("// the same Binding() above — one projector, one final decoder). It is what the\n")
	b.WriteString("// native-only runtime is built from; a unary-class method is deliberately absent,\n")
	b.WriteString("// because it has no partial decoder and could not serve a claimed stream.\n")
	b.WriteString("func streamCandidates() []spine.StreamRegistration {\n")
	b.WriteString("\treturn []spine.StreamRegistration{\n")
	for i := range emitted {
		if !emitted[i].stream() {
			continue
		}
		fmt.Fprintf(&b, "\t\t{Binding: p%d.StreamBinding(), BuildMethod: p%d.BuildMethod},\n", i, i)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	b.WriteString("// NewRuntime decodes the embedded deployment descriptor and builds the\n")
	b.WriteString("// immutable admitted native-only runtime via spine.NewWorkerRuntime over the\n")
	b.WriteString("// STREAM candidates (which REFUSES an empty population and verifies the full\n")
	b.WriteString("// stream method surface). It is the native-only command's root-runtime\n")
	b.WriteString("// selection; there is no BAML fallback and no nil/default path to a generated\n")
	b.WriteString("// BAML runtime.\n")
	b.WriteString("func NewRuntime() (worker.Runtime, error) {\n")
	b.WriteString("\tvar proj projectdescriptor.Project\n")
	b.WriteString("\tif err := json.Unmarshal(projectDescriptorJSON, &proj); err != nil {\n")
	b.WriteString("\t\treturn nil, fmt.Errorf(\"nativegenerated: decode embedded project descriptor: %w\", err)\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn spine.NewWorkerRuntime(proj, streamCandidates(), nil)\n")
	b.WriteString("}\n\n")

	b.WriteString("// NewExecutor decodes the embedded deployment descriptor and builds the\n")
	b.WriteString("// population-filtered oracle-capable executor via spine.NewPopulationExecutor over\n")
	b.WriteString("// the UNARY candidates (BOTH classes' Binding(): a static_stream method's unary\n")
	b.WriteString("// /call is retained, which is NOT standard stream enrollment). It ALLOWS an\n")
	b.WriteString("// empty population (an all-decline executor whose every /call\n")
	b.WriteString("// falls back to BAML). It is the ExecBridge-U1c standard composite's construction\n")
	b.WriteString("// path; this executor's own path carries no generated BAML/CFFI — the standard\n")
	b.WriteString("// worker links BAML elsewhere and injects only the neutral no-send plan +\n")
	b.WriteString("// same-bytes parse closures.\n")
	b.WriteString("func NewExecutor() (bamlutils.NativeSpineUnaryOracleExecutor, error) {\n")
	b.WriteString("\tvar proj projectdescriptor.Project\n")
	b.WriteString("\tif err := json.Unmarshal(projectDescriptorJSON, &proj); err != nil {\n")
	b.WriteString("\t\treturn nil, fmt.Errorf(\"nativegenerated: decode embedded project descriptor: %w\", err)\n")
	b.WriteString("\t}\n")
	b.WriteString("\texec, err := spine.NewPopulationExecutor(proj, unaryCandidates(), nil)\n")
	b.WriteString("\tif err != nil {\n")
	b.WriteString("\t\treturn nil, err\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn exec, nil\n")
	b.WriteString("}\n")

	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("gen-native-spine-worker: gofmt aggregate: %w\n%s", err, b.String())
	}
	return formatted, nil
}

// generatedSubpackageDir matches the exact shape subpackageDirName emits ("m" +
// sanitized-lowercase name + "_" + 12 hex chars of the name's SHA-256), so the cleaner
// removes only subpackage directories THIS generator created — never one an operator
// happened to place under the output path.
var generatedSubpackageDir = regexp.MustCompile(`^m[a-z0-9]*_[0-9a-f]{12}$`)

// cleanOutputDir removes the generator's OWN prior artifacts from outDir — the
// aggregate file, the embedded descriptor, every generated subpackage directory, and
// any stale root-level generated .go from an earlier layout — while leaving the
// committed stub (generated_off.go) and anything this generator never emits.
//
// It first requires the committed stub to be present. outDir is the dedicated
// native-only registry package, whose ONLY checked-in file is that stub; a missing
// stub means outDir is not that package (a mistyped --out-dir), so cleaning is REFUSED
// rather than deleting files this generator does not own. It then removes only entries
// whose shape this generator produces — a subpackage directory matching
// generatedSubpackageDir, or a .go source / project.json — so an unexpected directory
// or a non-generated file (README, go.mod, .gitignore, ...) is left untouched.
//
// A NON-EXISTENT outDir is refused for the same reason: it carries no committed stub,
// so generating into it would create a stubless registry package — the very state the
// stub guard exists to prevent. The registry package is committed (stub included), so
// a missing directory is a mistyped --out-dir, never a legitimate first-run.
func cleanOutputDir(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("gen-native-spine-worker: refusing to generate into %q: the directory does not exist, so the committed %s that marks the native-only registry package is absent (check --out-dir)", outDir, stubFileName)
		}
		return fmt.Errorf("gen-native-spine-worker: read output dir: %w", err)
	}
	stubPresent := false
	for _, e := range entries {
		if !e.IsDir() && e.Name() == stubFileName {
			stubPresent = true
			break
		}
	}
	if !stubPresent {
		return fmt.Errorf("gen-native-spine-worker: refusing to clean %q: the committed %s is absent, so this is not the native-only registry package (check --out-dir)", outDir, stubFileName)
	}
	for _, e := range entries {
		name := e.Name()
		if name == stubFileName {
			// The committed fail-loud stub (tag-gated OFF) is the ONE file the source
			// checkout owns.
			continue
		}
		if e.IsDir() {
			if !generatedSubpackageDir.MatchString(name) {
				continue // not a directory this generator emitted; leave it in place
			}
			if err := os.RemoveAll(filepath.Join(outDir, name)); err != nil {
				return fmt.Errorf("gen-native-spine-worker: remove stale subpackage %q: %w", name, err)
			}
			continue
		}
		// The registry package is generated-only, so any .go but the stub — including a
		// root-level one from a previous layout — is stale generated output; project.json
		// is the embedded descriptor. Any other file is left untouched.
		if strings.HasSuffix(name, ".go") || name == projectJSONFileName {
			if err := os.Remove(filepath.Join(outDir, name)); err != nil {
				return fmt.Errorf("gen-native-spine-worker: remove stale output %q: %w", name, err)
			}
		}
	}
	return nil
}
