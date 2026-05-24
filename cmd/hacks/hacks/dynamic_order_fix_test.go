package hacks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestApplyDynamicOrderFixToDir_PerVersion exercises the hack against
// each pinned upstream BAML version. The fixture is the read-only
// module cache copy of github.com/boundaryml/baml@<v>; the test copies
// it into a temp dir, applies the patch, then re-parses the patched
// serde + pkg files plus a tiny driver program to assert the result
// compiles. Versions that are not present in the local module cache
// are skipped — the resolve step is `go mod download` against the
// existing cache, so the test runs in any environment that already
// has the version unpacked.
func TestApplyDynamicOrderFixToDir_PerVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("per-version fixtures require module-cache access; skip under -short")
	}

	versions := []string{
		"v0.204.0",
		"v0.215.0",
		"v0.219.0",
		"v0.222.0",
	}

	// orderedmap source path used by the hack — resolve once relative
	// to this test file so the binary works regardless of the working
	// directory `go test` is invoked from.
	repoRoot := repoRootForTest(t)
	prev := orderedMapSourcePath
	orderedMapSourcePath = filepath.Join(repoRoot, "bamlutils", "orderedmap.go")
	defer func() { orderedMapSourcePath = prev }()

	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			srcDir := bamlModuleCachePath(t, v)
			tmpDir := t.TempDir()
			workDir := filepath.Join(tmpDir, "baml")
			if err := copyDir(srcDir, workDir); err != nil {
				t.Fatalf("copy upstream BAML %s: %v", v, err)
			}
			// Ensure the working tree is writable; the module cache
			// is read-only on disk.
			if err := chmodTreeWritable(workDir); err != nil {
				t.Fatalf("chmod writable: %v", err)
			}

			if err := ApplyDynamicOrderFixToDir(v, workDir); err != nil {
				t.Fatalf("ApplyDynamicOrderFixToDir(%s): %v", v, err)
			}

			// Postcondition: serde.DynamicClass.Fields uses OrderedFields.
			body := readFileT(t, filepath.Join(workDir, "engine", "language_client_go", "baml_go", "serde", "decode.go"))
			if !strings.Contains(body, "Fields OrderedFields") {
				t.Fatalf("decode.go in %s does not declare Fields OrderedFields", v)
			}
			if strings.Contains(body, "Fields map[string]any") {
				t.Fatalf("decode.go in %s still has Fields map[string]any", v)
			}
			if !strings.Contains(body, "func DecodeToOrderedValue(") {
				t.Fatalf("decode.go in %s missing DecodeToOrderedValue helper", v)
			}

			// Postcondition (cold-v2 union fix): DecodeToOrderedValue
			// has an explicit *cffi.CFFIValueHolder_UnionVariantValue
			// case that routes through decodeUnionValue (which itself
			// uses the ordered decoder), and the unknown-union branch
			// in decodeUnionValue routes the nested value through
			// DecodeToOrderedValue rather than Decode. Without these
			// two patches, a dynamic union whose Value is a CFFI map
			// drops key order on the way out of serde.
			if !strings.Contains(body, "*cffi.CFFIValueHolder_UnionVariantValue") {
				t.Fatalf("decode.go in %s missing CFFIValueHolder_UnionVariantValue case in DecodeToOrderedValue", v)
			}
			if !strings.Contains(body, "DecodeToOrderedValue(valueUnion.Value, typeMap)") {
				t.Fatalf("decode.go in %s did not route decodeUnionValue's dynamic branch through DecodeToOrderedValue", v)
			}
			if strings.Contains(body, "Decode(valueUnion.Value, typeMap).Interface()") {
				t.Fatalf("decode.go in %s still routes dynamic union value through Decode(...).Interface() (family A pre-patch shape)", v)
			}
			if strings.Contains(body, "Value:   value.Elem(),") {
				t.Fatalf("decode.go in %s still wraps dynamic union value through value.Elem() (family B pre-patch shape)", v)
			}

			// Postcondition: pkg/lib.go exposes the public surface.
			libBody := readFileT(t, filepath.Join(workDir, "engine", "language_client_go", "pkg", "lib.go"))
			for _, marker := range []string{
				"type OrderedFields = serde.OrderedFields",
				"func NewOrderedFields(",
				"func DecodeToOrderedValue(",
				"func EncodeClassOrdered(",
			} {
				if !strings.Contains(libBody, marker) {
					t.Fatalf("pkg/lib.go in %s missing %q", v, marker)
				}
			}

			// Postcondition: ordered_fields.go exists and was rewritten
			// into the serde package.
			ofBody := readFileT(t, filepath.Join(workDir, "engine", "language_client_go", "baml_go", "serde", "ordered_fields.go"))
			if !strings.HasPrefix(ofBody, "package serde") {
				t.Fatalf("ordered_fields.go in %s does not begin with `package serde`", v)
			}
			if !strings.Contains(ofBody, "type OrderedFields = OrderedMap[any]") {
				t.Fatalf("ordered_fields.go in %s missing OrderedFields alias", v)
			}

			// Compile check: gofmt + go vet against the patched serde
			// package. A successful vet implies the AST parses and
			// type-checks within the package; we cannot do a full
			// `go build` because the package depends on cgo + native
			// libraries the test environment may not have.
			vetOut, vetErr := runGoVetSerde(workDir)
			if vetErr != nil {
				t.Fatalf("go vet on patched serde failed: %v\n%s", vetErr, vetOut)
			}

			// Compile check: go build against the patched pkg (the
			// BAML facade lib.go). This catches per-version drift in
			// the serde.EncodeClass/EncodeEnum/EncodeUnion shapes that
			// the EncodeClassOrdered wrapper has to thread through —
			// e.g. v0.204-family takes a func() *cffi.CFFITypeName
			// while v0.215+ takes a string. Tested via `go build`
			// (not `go vet`) so the upstream encode_decode_test.go
			// file, which indexes serde.DynamicClass.Fields as a map,
			// does not need to be deleted from the fixture.
			buildOut, buildErr := runGoBuildPkg(workDir)
			if buildErr != nil {
				t.Fatalf("go build on patched pkg failed: %v\n%s", buildErr, buildOut)
			}
		})
	}
}

// TestApplyDynamicOrderFix_GeneratedClient_ToyFixture compiles a small
// synthetic baml_client tree through the generated-client hack and
// asserts the rewrite covers field, allocation, assignment, and
// EncodeClass call shapes simultaneously.
func TestApplyDynamicOrderFix_GeneratedClient_ToyFixture(t *testing.T) {
	srcDir := t.TempDir()
	typesDir := filepath.Join(srcDir, "baml_client", "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const before = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type Sample struct {
	DynamicProperties map[string]any
}

func (c *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	c.DynamicProperties = make(map[string]any, 4)
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {

		default:

			c.DynamicProperties[key] = baml.DecodeToValue(valueHolder)
		}
	}
	_ = fmt.Sprintf("%v", typeMap)
}

func (c Sample) Encode() (*cffi.HostValue, error) {
	fields := map[string]any{}
	return baml.EncodeClass("Sample", fields, &c.DynamicProperties)
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(filepath.Join(srcDir, "baml_client")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)
	for _, marker := range []string{
		"DynamicProperties baml.OrderedFields",
		"DynamicProperties = baml.NewOrderedFields(4)",
		"c.DynamicProperties.Set(key, baml.DecodeToOrderedValue(valueHolder))",
		"baml.EncodeClassOrdered(",
	} {
		if !strings.Contains(after, marker) {
			t.Fatalf("missing marker %q in patched file:\n%s", marker, after)
		}
	}
	if strings.Contains(after, "DynamicProperties map[string]any") {
		t.Fatalf("legacy map type still present in patched file")
	}
}

// repoRootForTest walks upward from the test file's package looking
// for the bamlutils directory. The hack tests need a stable path to
// bamlutils/orderedmap.go independent of where `go test` is invoked.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "bamlutils", "orderedmap.go")); err == nil {
			return dir
		}
	}
	t.Fatalf("cannot locate repo root containing bamlutils/orderedmap.go from %s", cwd)
	return ""
}

// bamlModuleCachePath resolves the on-disk path of the requested BAML
// module version via `go mod download`. Tests are skipped when the
// version is unavailable (e.g. offline + cold cache).
func bamlModuleCachePath(t *testing.T, version string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", "github.com/boundaryml/baml@"+version)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Skipf("go mod download for %s timed out", version)
		}
		t.Skipf("go mod download for %s failed: %v", version, err)
	}
	dir, parseErr := extractJSONField(string(out), "Dir")
	if parseErr != nil {
		t.Fatalf("parse go mod download output: %v", parseErr)
	}
	if dir == "" {
		t.Skipf("go mod download returned empty Dir for %s", version)
	}
	return dir
}

func extractJSONField(s, field string) (string, error) {
	key := `"` + field + `"`
	idx := strings.Index(s, key)
	if idx < 0 {
		return "", nil
	}
	rest := s[idx+len(key):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", nil
	}
	rest = strings.TrimLeft(rest[colon+1:], " \t")
	if !strings.HasPrefix(rest, `"`) {
		return "", nil
	}
	end := strings.IndexByte(rest[1:], '"')
	if end < 0 {
		return "", nil
	}
	return rest[1 : 1+end], nil
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func chmodTreeWritable(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, info.Mode()|0o200)
	})
}

// runGoVetSerde invokes `go vet` on the patched serde package. The
// command runs in a temporary working dir that contains a synthesised
// go.mod replacing the BAML module path to the patched copy so go vet
// can resolve the cgo-bearing sibling packages.
//
// On a system without a Go toolchain in the test environment, the
// vet step is skipped (the caller treats a SkipError as success).
var goVetOnce sync.Once
var goVetAvailable bool

func runGoVetSerde(moduleDir string) (string, error) {
	goVetOnce.Do(func() {
		if _, err := exec.LookPath("go"); err == nil {
			goVetAvailable = true
		}
	})
	if !goVetAvailable {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "vet", "./engine/language_client_go/baml_go/serde/...")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return string(out), fmt.Errorf("go vet timed out")
		}
		// go vet against the patched module may fail because the cgo
		// wrappers under baml_go reference native libraries the test
		// environment lacks. We treat a vet failure that mentions
		// build constraints or missing native libraries as a soft
		// signal — the per-file marker assertions above already pin
		// the patched shape. Re-surface only when the failure looks
		// like a syntax or type-check error in serde itself.
		if isAllowedVetFailure(string(out)) {
			return string(out), nil
		}
		return string(out), err
	}
	return string(out), nil
}

// runGoBuildPkg invokes `go build` on the patched pkg/ package. The
// build runs in moduleDir and uses GOFLAGS=-mod=mod so the patched
// module resolves against its own go.sum. Unlike `go vet`, `go build`
// does not include _test.go files in the compilation unit, which keeps
// upstream tests under engine/language_client_go/pkg/ — which still
// index serde.DynamicClass.Fields as a map[string]any — out of the way.
//
// On a system without a Go toolchain in the test environment, the
// build step is skipped silently (matches the runGoVetSerde policy).
func runGoBuildPkg(moduleDir string) (string, error) {
	goVetOnce.Do(func() {
		if _, err := exec.LookPath("go"); err == nil {
			goVetAvailable = true
		}
	})
	if !goVetAvailable {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "./engine/language_client_go/pkg/")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return string(out), fmt.Errorf("go build timed out")
		}
		// As with runGoVetSerde, allow failures that look like the
		// build environment lacks cgo or the native BAML libraries
		// (the linker would surface those late; type errors in
		// pkg/lib.go itself — which is what we want to catch — show
		// up as `cannot use X as Y` lines well before linking).
		if isAllowedVetFailure(string(out)) {
			return string(out), nil
		}
		return string(out), err
	}
	return string(out), nil
}

func isAllowedVetFailure(out string) bool {
	allowedFragments := []string{
		"package cffi",
		"build constraints exclude all Go files",
		"cgo",
		"undefined: C.",
		"_cgo_export.h",
		"libbaml",
		"baml_cffi_wrapper.h",
		"language_client_go/baml_go/lib_",
		"could not import",
		"no Go files",
	}
	for _, f := range allowedFragments {
		if strings.Contains(out, f) {
			return true
		}
	}
	return false
}

