// Command gen-native-spine-worker emits the DEPLOYMENT-SPECIFIC native-only
// worker registry for the ExecBridge-U1b packaged worker. It consumes the JSON
// emitted by `cmd/introspect --native-spine-descriptors` (a projectdescriptor.Project)
// and generates, into the extracted nanollmprepare build tree:
//
//   - one deterministic collision-proof subpackage per codegen-admitted static
//     unary method, each produced by adapters/common/codegen.EmitNativeStaticUnary
//     (a subpackage boundary is required because every emitted file exports generic
//     names such as MethodName, BuildMethod, and Binding);
//   - nativegenerated/project.json, embedded by the aggregate registry package;
//   - a deterministic aggregate candidate list (nativegenerated/generated.go, under
//     the debamlnativeonlygenerated build tag) that imports every emitted subpackage
//     and pairs its Binding() with its BuildMethod, then exposes NewRuntime() which
//     decodes the embedded descriptor and calls spine.NewWorkerRuntime.
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

// buildTag is the tag under which the generated registry is the real
// implementation; without it the committed stub compiles and fails loud.
const buildTag = "debamlnativeonlygenerated"

// emittedMethod is one codegen-admitted static-unary method plus its resolved,
// collision-proof subpackage identity.
type emittedMethod struct {
	method     projectdescriptor.Method
	dirName    string // subpackage directory name (== package name)
	importPath string // full import path of the subpackage
	fileName   string // the emitted source file name inside the subpackage
	source     []byte // the EmitNativeStaticUnary output
}

// Generate reads a projectdescriptor.Project from descriptorsJSON, validates it,
// and writes the deployment-specific native registry into outDir. packagePath is
// the import path outDir is built at (defaultRegistryPackagePath in production).
//
// It fails on an invalid descriptor, on a project that yields NO codegen-admitted
// static-unary candidate (a native-only worker needs at least one), and on any
// emitter error. It cleans the output directory first.
func Generate(descriptorsJSON []byte, outDir, packagePath string) error {
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
	if len(emitted) == 0 {
		return fmt.Errorf("gen-native-spine-worker: project has no codegen-admitted static-unary method; a native-only worker needs at least one candidate")
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

// emitMethods produces the deterministic, method-name-sorted set of emitted
// per-method candidates. Every codegen-admitted static-unary method becomes a
// candidate; membership in the served runtime is the runtime classifier's call.
func emitMethods(proj projectdescriptor.Project, packagePath string) ([]emittedMethod, error) {
	// Sort by method name so the emitted set and the aggregate import order are
	// deterministic regardless of descriptor method order.
	methods := make([]projectdescriptor.Method, len(proj.Methods))
	copy(methods, proj.Methods)
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })

	out := make([]emittedMethod, 0, len(methods))
	seenDir := make(map[string]string, len(methods))
	for _, m := range methods {
		if m.Class != projectdescriptor.ClassStaticUnary {
			// Only static-unary methods are emittable; anything else is not a candidate.
			continue
		}
		dir := subpackageDirName(m.Name)
		if prev, dup := seenDir[dir]; dup {
			// Two method names collided to the same collision-proof suffix — a hash
			// collision, which must never happen but must fail loud if it ever does.
			return nil, fmt.Errorf("gen-native-spine-worker: subpackage name collision %q for methods %q and %q", dir, prev, m.Name)
		}
		seenDir[dir] = m.Name

		src, err := codegen.EmitNativeStaticUnary(m, codegen.NativeSpineOptions{PackageName: dir})
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
	b.WriteString("\t\"github.com/invakid404/baml-rest/bamlutils/projectdescriptor\"\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/nativeserve/spine\"\n")
	b.WriteString("\t\"github.com/invakid404/baml-rest/worker\"\n\n")
	for i, em := range emitted {
		fmt.Fprintf(&b, "\tp%d %q\n", i, em.importPath)
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "//go:embed %s\n", projectJSONFileName)
	b.WriteString("var projectDescriptorJSON []byte\n\n")

	b.WriteString("// candidates is the deterministic aggregate candidate list: one emitted\n")
	b.WriteString("// per-method registration in method-name order. Membership in the SERVED\n")
	b.WriteString("// runtime is NOT decided here — spine.NewWorkerRuntime's single U1 classifier\n")
	b.WriteString("// owns that. These are CANDIDATES only; there is no fallback slot.\n")
	b.WriteString("func candidates() []spine.UnaryRegistration {\n")
	b.WriteString("\treturn []spine.UnaryRegistration{\n")
	for i := range emitted {
		fmt.Fprintf(&b, "\t\t{Binding: p%d.Binding(), BuildMethod: p%d.BuildMethod},\n", i, i)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n\n")

	b.WriteString("// NewRuntime decodes the embedded deployment descriptor and builds the\n")
	b.WriteString("// immutable admitted native runtime via spine.NewWorkerRuntime. It is the\n")
	b.WriteString("// native-only command's root-runtime selection; there is no BAML fallback and\n")
	b.WriteString("// no nil/default path to a generated BAML runtime.\n")
	b.WriteString("func NewRuntime() (worker.Runtime, error) {\n")
	b.WriteString("\tvar proj projectdescriptor.Project\n")
	b.WriteString("\tif err := json.Unmarshal(projectDescriptorJSON, &proj); err != nil {\n")
	b.WriteString("\t\treturn nil, fmt.Errorf(\"nativegenerated: decode embedded project descriptor: %w\", err)\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn spine.NewWorkerRuntime(proj, candidates(), nil)\n")
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
// or a non-generated file (README, go.mod, .gitignore, ...) is left untouched. A
// non-existent outDir is not an error.
func cleanOutputDir(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
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
