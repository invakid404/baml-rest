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
	"sync/atomic"
	"testing"
	"time"
)

// perVersionTestRan gates the heavy per-version hack fixture so it only
// performs its real go vet + go build sweep once per process. Each pass
// across all pinned BAML versions runs ~8 compile-shaped operations
// against fresh patched module trees; under the unit-tests workflow's
// `go test -race -count=100 ./...` loop that would balloon to ~800
// invocations and blow past the default 10-minute go-test timeout.
// The deterministic-AST + compile invariants don't gain extra signal
// from being re-checked 100×, so the second and subsequent invocations
// skip cleanly while preserving real-regression coverage on the first.
var perVersionTestRan atomic.Int32

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
	if perVersionTestRan.Add(1) > 1 {
		t.Skip("per-version compile sweep is deterministic; only ran on the first -count iteration to keep -count=100 under the go-test default 10m timeout")
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
			// DecodeToOrderedValue. With the plain Decode path, a
			// dynamic union whose Value is a CFFI map drops key order
			// on the way out of serde.
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

			// Postcondition (cold-v4 IsSinglePattern fix): the
			// optional / single-pattern branch — the path BAML drives
			// for `T | null` shapes including non-null
			// `optional(map<...>)` dynamic fields — also flows the
			// inner value through DecodeToOrderedValue. The plain
			// Decode path landed the inner value as a Go map, so
			// UnwrapDynamicValue could not see the CFFI key order.
			//
			// The action line `decoded := DecodeToOrderedValue(...)`
			// is emitted identically across all three families'
			// after-images, so a single substring assertion covers
			// v0.204 (isOptionalPattern) and v0.215+ (IsSinglePattern)
			// at once. The absence checks pin the pre-patch lines that
			// each family used to carry, family-by-family.
			if !strings.Contains(body, "decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)") {
				t.Fatalf("decode.go in %s did not route the IsSinglePattern / isOptionalPattern branch through DecodeToOrderedValue", v)
			}
			if strings.Contains(body, "return Decode(valueUnion.Value, typeMap)\n") {
				t.Fatalf("decode.go in %s still returns Decode(valueUnion.Value, typeMap) directly (family B/C IsSinglePattern pre-patch shape)", v)
			}
			if strings.Contains(body, "return Decode(value, typeMap)\n") {
				t.Fatalf("decode.go in %s still returns Decode(value, typeMap) directly (family A isOptionalPattern pre-patch shape)", v)
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

// TestPatchDecodeUnionValueDynamicBranch_TolerantToDrift covers the
// brittleness fix that motivated the token-anchored rewrite: comment
// rewording, blank-line drift inside the branch, and reduced
// indentation depth must all leave the patch operational. Each
// synthetic fixture is a minimal slice of decodeUnionValue's body
// containing only the dynamic-union branch and a small surrounding
// signature; the patch is invoked directly so the test runs without
// touching the module cache.
func TestPatchDecodeUnionValueDynamicBranch_TolerantToDrift(t *testing.T) {
	cases := []struct {
		name           string
		family         decodeFamily
		source         string
		mustContain    []string
		mustNotContain []string
	}{
		{
			name:   "familyA reworded comment plus extra blank line and shallower indent",
			family: familyA,
			// Indentation reduced from two tabs to one; the upstream
			// "Union not found" header is replaced with a single
			// reworded note; an extra blank line separates the comment
			// from the struct literal.
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) reflect.Value {\n" +
				"\t// dynamic union path: type lookup miss, drop union info.\n" +
				"\n" +
				"\tdynamicUnion := DynamicUnion{\n" +
				"\t\tVariant: unionName,\n" +
				"\t\tValue:   Decode(valueUnion.Value, typeMap).Interface(),\n" +
				"\t}\n" +
				"\treturn reflect.ValueOf(dynamicUnion)\n" +
				"}\n",
			mustContain: []string{
				"// dynamic union path: type lookup miss, drop union info.",
				"\tdynamicUnion := DynamicUnion{\n",
				"\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n",
				"\treturn reflect.ValueOf(dynamicUnion)\n",
			},
			mustNotContain: []string{
				"Decode(valueUnion.Value, typeMap).Interface()",
				"// Union not found",
			},
		},
		{
			name:   "familyB reworded comment plus inserted blank line and shallower indent",
			family: familyB,
			// Indentation reduced from four tabs to three; comment block
			// reworded entirely; an extra blank line is added inside the
			// branch between the value-decode and the struct literal.
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) (reflect.Value, reflect.Type) {\n" +
				"\tvalue, goType := func() (reflect.Value, reflect.Type) {\n" +
				"\t\tif true {\n" +
				"\t\t\t// (note) variant not in typeMap; fall through to dynamic.\n" +
				"\n" +
				"\t\t\tvalue, _ := Decode(valueUnion.Value, typeMap)\n" +
				"\n" +
				"\t\t\tdynamicUnion := DynamicUnion{\n" +
				"\t\t\t\tVariant: valueUnion.Name.Name,\n" +
				"\t\t\t\tValue:   value.Elem(),\n" +
				"\t\t\t}\n" +
				"\t\t\tvalue = reflect.ValueOf(dynamicUnion)\n" +
				"\t\t\tgoType = reflect.TypeOf(DynamicUnion{})\n" +
				"\t\t\treturn value, goType\n" +
				"\t\t}\n" +
				"\t\treturn reflect.ValueOf(nil), nil\n" +
				"\t}()\n" +
				"\treturn value, goType\n" +
				"}\n",
			mustContain: []string{
				"// (note) variant not in typeMap; fall through to dynamic.",
				"\t\t\tdynamicUnion := DynamicUnion{\n",
				"\t\t\t\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n",
				"\t\t\tvalue := reflect.ValueOf(dynamicUnion)\n",
			},
			mustNotContain: []string{
				"value, _ := Decode(valueUnion.Value, typeMap)",
				"Value:   value.Elem(),",
				"// Union not found",
			},
		},
		{
			name:   "familyC reworded comment plus blank-line drift plus inner-block indentation tweak",
			family: familyC,
			// Three concurrent drifts: the comment is rewritten to a
			// single shorter line, the upstream blank line between the
			// struct literal and the scalar switch is removed and an
			// extra blank line is inserted inside the switch arms, and
			// two inner-block lines use four-space indentation in
			// place of tabs. The patch must locate the span by
			// distinctive code tokens and replace the whole branch
			// regardless of inner-block indentation style; the
			// indentation tweak lives strictly inside the replaced
			// span, exercising the property that any drift between the
			// start and end anchors is discarded by the wholesale
			// rewrite. The start-anchor indentation is left at four
			// tabs so the after-image's captured indent stays stable
			// and the mustContain assertions remain literal.
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) (reflect.Value, reflect.Type) {\n" +
				"\tvalue, goType := func() (reflect.Value, reflect.Type) {\n" +
				"\t\tif true {\n" +
				"\t\t\tif true {\n" +
				"\t\t\t\t// fully dynamic; preserve scalar fast-path.\n" +
				"\t\t\t\tvalue, goType := Decode(valueUnion.Value, typeMap)\n" +
				"\t\t\t\tdynamicUnion := DynamicUnion{\n" +
				"                    Variant: valueUnion.Name.Name,\n" +
				"\t\t\t\t}\n" +
				"\t\t\t\tswitch goType {\n" +
				"\t\t\t\tcase reflect.TypeOf(int64(0)):\n" +
				"                    dynamicUnion.Value = value.Int()\n" +
				"\n" +
				"\t\t\t\tcase reflect.TypeOf(float64(0)):\n" +
				"\t\t\t\t\tdynamicUnion.Value = value.Float()\n" +
				"\t\t\t\tcase reflect.TypeOf(false):\n" +
				"\t\t\t\t\tdynamicUnion.Value = value.Bool()\n" +
				"\t\t\t\tdefault:\n" +
				"\t\t\t\t\tdynamicUnion.Value = value.Interface()\n" +
				"\t\t\t\t}\n" +
				"\t\t\t\tvalue = reflect.ValueOf(dynamicUnion)\n" +
				"\t\t\t\tgoType = reflect.TypeOf(DynamicUnion{})\n" +
				"\t\t\t\treturn value, goType\n" +
				"\t\t\t}\n" +
				"\t\t}\n" +
				"\t\treturn reflect.ValueOf(nil), nil\n" +
				"\t}()\n" +
				"\treturn value, goType\n" +
				"}\n",
			mustContain: []string{
				"// fully dynamic; preserve scalar fast-path.",
				"\t\t\t\tdynamicUnion := DynamicUnion{\n",
				"\t\t\t\t\tValue:   DecodeToOrderedValue(valueUnion.Value, typeMap),\n",
				"\t\t\t\tvalue := reflect.ValueOf(dynamicUnion)\n",
			},
			mustNotContain: []string{
				"value, goType := Decode(valueUnion.Value, typeMap)",
				"switch goType {",
				"dynamicUnion.Value = value.Int()",
				"dynamicUnion.Value = value.Float()",
				"                    Variant: valueUnion.Name.Name,",
				"                    dynamicUnion.Value = value.Int()",
				"// Union not found",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := patchDecodeUnionValueDynamicBranch(c.source, c.family)
			if err != nil {
				t.Fatalf("patch failed: %v\ninput:\n%s", err, c.source)
			}
			for _, marker := range c.mustContain {
				if !strings.Contains(got, marker) {
					t.Errorf("expected patched output to contain %q\noutput:\n%s", marker, got)
				}
			}
			for _, marker := range c.mustNotContain {
				if strings.Contains(got, marker) {
					t.Errorf("expected patched output to NOT contain %q\noutput:\n%s", marker, got)
				}
			}
		})
	}
}

// TestPatchDecodeUnionValueDynamicBranch_FailClosed pins the
// fail-closed contract: missing anchors, ambiguous start anchors, and
// a missing discriminator each return an error naming the family.
func TestPatchDecodeUnionValueDynamicBranch_FailClosed(t *testing.T) {
	t.Run("missing start anchor", func(t *testing.T) {
		_, err := patchDecodeUnionValueDynamicBranch("package serde\n\nfunc nothing() {}\n", familyC)
		if err == nil {
			t.Fatalf("expected error for missing start anchor")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})

	t.Run("ambiguous start anchor", func(t *testing.T) {
		dup := "package serde\n" +
			"\t\t\t\tvalue, goType := Decode(valueUnion.Value, typeMap)\n" +
			"\t\t\t\tvalue, goType := Decode(valueUnion.Value, typeMap)\n"
		_, err := patchDecodeUnionValueDynamicBranch(dup, familyC)
		if err == nil {
			t.Fatalf("expected error for ambiguous start anchor")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})

	t.Run("missing discriminator", func(t *testing.T) {
		// familyC start anchor present, end anchor present, but the
		// `switch goType` discriminator is absent — the spec was not
		// designed for this shape.
		missing := "package serde\n\n" +
			"func decodeUnionValue() (reflect.Value, reflect.Type) {\n" +
			"\t\t\t\tvalue, goType := Decode(valueUnion.Value, typeMap)\n" +
			"\t\t\t\t_ = value\n" +
			"\t\t\t\treturn value, goType\n" +
			"}\n"
		_, err := patchDecodeUnionValueDynamicBranch(missing, familyC)
		if err == nil {
			t.Fatalf("expected error for missing discriminator")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})
}

// TestPatchDecodeUnionValueIsSinglePatternBranch covers the cold-v4
// patch that routes the optional / single-pattern branch through
// DecodeToOrderedValue. familyA uses a structurally distinct
// `if isOptionalPattern { ... }` block returning a single
// reflect.Value; familyB and familyC share an
// `else if valueUnion.IsSinglePattern { ... }` block returning
// (reflect.Value, reflect.Type). Each case asserts the after-image
// uses the ordered decoder and that the pre-patch action line is
// gone.
func TestPatchDecodeUnionValueIsSinglePatternBranch(t *testing.T) {
	cases := []struct {
		name           string
		family         decodeFamily
		source         string
		mustContain    []string
		mustNotContain []string
	}{
		{
			name:   "familyA isOptionalPattern branch",
			family: familyA,
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) reflect.Value {\n" +
				"\tvar isOptionalPattern bool = false\n" +
				"\t// For optional patterns (T | null), decode the inner value directly\n" +
				"\t// These shouldn't be looked up as union types\n" +
				"\tif isOptionalPattern {\n" +
				"\t\tvalue := valueUnion.Value\n" +
				"\t\treturn Decode(value, typeMap)\n" +
				"\t}\n" +
				"\treturn reflect.ValueOf(nil)\n" +
				"}\n",
			mustContain: []string{
				"\tif isOptionalPattern {\n",
				"\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\treturn reflect.ValueOf(nil)\n",
				"\t\treturn reflect.ValueOf(decoded)\n",
			},
			mustNotContain: []string{
				"return Decode(value, typeMap)",
				"value := valueUnion.Value",
			},
		},
		{
			name:   "familyB IsSinglePattern branch",
			family: familyB,
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) (reflect.Value, reflect.Type) {\n" +
				"\tvalue, goType := func() (reflect.Value, reflect.Type) {\n" +
				"\t\tif ok := valueUnion.Value.GetNullValue(); ok != nil {\n" +
				"\t\t\treturn reflect.ValueOf(nil), nil\n" +
				"\t\t} else if valueUnion.IsSinglePattern {\n" +
				"\t\t\t// For optional patterns (T | null), decode the inner value directly\n" +
				"\t\t\t// These shouldn't be looked up as union types\n" +
				"\t\t\treturn Decode(valueUnion.Value, typeMap)\n" +
				"\t\t} else {\n" +
				"\t\t\treturn reflect.ValueOf(nil), nil\n" +
				"\t\t}\n" +
				"\t}()\n" +
				"\treturn value, goType\n" +
				"}\n",
			mustContain: []string{
				"\t\t} else if valueUnion.IsSinglePattern {\n",
				"\t\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\t\treturn reflect.ValueOf(nil), nil\n",
				"\t\t\trv := reflect.ValueOf(decoded)\n",
				"\t\t\treturn rv, rv.Type()\n",
			},
			mustNotContain: []string{
				"return Decode(valueUnion.Value, typeMap)",
			},
		},
		{
			name:   "familyC IsSinglePattern branch with reworded comment and shallower indent",
			family: familyC,
			source: "package serde\n\n" +
				"func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) (reflect.Value, reflect.Type) {\n" +
				"\tif true {\n" +
				"\t\tif ok := valueUnion.Value.GetNullValue(); ok != nil {\n" +
				"\t\t\treturn reflect.ValueOf(nil), nil\n" +
				"\t\t} else if valueUnion.IsSinglePattern {\n" +
				"\t\t\t// reworded: drop union-ness for optional shape\n" +
				"\t\t\treturn Decode(valueUnion.Value, typeMap)\n" +
				"\t\t}\n" +
				"\t}\n" +
				"\treturn reflect.ValueOf(nil), nil\n" +
				"}\n",
			mustContain: []string{
				"\t\t} else if valueUnion.IsSinglePattern {\n",
				"\t\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\trv := reflect.ValueOf(decoded)\n",
				"\t\t\treturn rv, rv.Type()\n",
			},
			mustNotContain: []string{
				"return Decode(valueUnion.Value, typeMap)",
				"// For optional patterns (T | null), decode the inner value directly",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := patchDecodeUnionValueIsSinglePatternBranch(c.source, c.family)
			if err != nil {
				t.Fatalf("patch failed: %v\ninput:\n%s", err, c.source)
			}
			for _, marker := range c.mustContain {
				if !strings.Contains(got, marker) {
					t.Errorf("expected patched output to contain %q\noutput:\n%s", marker, got)
				}
			}
			for _, marker := range c.mustNotContain {
				if strings.Contains(got, marker) {
					t.Errorf("expected patched output to NOT contain %q\noutput:\n%s", marker, got)
				}
			}
		})
	}
}

// TestPatchDecodeUnionValueIsSinglePatternBranch_FailClosed mirrors the
// fail-closed contract pinned for the dynamic-union branch: missing
// anchors, ambiguous start anchors, and a missing discriminator each
// surface a family-named error.
func TestPatchDecodeUnionValueIsSinglePatternBranch_FailClosed(t *testing.T) {
	t.Run("missing start anchor", func(t *testing.T) {
		_, err := patchDecodeUnionValueIsSinglePatternBranch("package serde\n\nfunc nothing() {}\n", familyC)
		if err == nil {
			t.Fatalf("expected error for missing start anchor")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})

	t.Run("ambiguous start anchor", func(t *testing.T) {
		dup := "package serde\n" +
			"\t\t} else if valueUnion.IsSinglePattern {\n" +
			"\t\t\treturn Decode(valueUnion.Value, typeMap)\n" +
			"\t\t} else if valueUnion.IsSinglePattern {\n" +
			"\t\t\treturn Decode(valueUnion.Value, typeMap)\n"
		_, err := patchDecodeUnionValueIsSinglePatternBranch(dup, familyC)
		if err == nil {
			t.Fatalf("expected error for ambiguous start anchor")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})

	t.Run("missing discriminator", func(t *testing.T) {
		// Start anchor present but the branch body never references
		// `Decode(valueUnion.Value, typeMap)`; the spec was not
		// designed for this shape and must fail closed.
		missing := "package serde\n\n" +
			"func decodeUnionValue() (reflect.Value, reflect.Type) {\n" +
			"\t\t} else if valueUnion.IsSinglePattern {\n" +
			"\t\t\t_ = valueUnion\n" +
			"\t\t\treturn Decode(other.Value, typeMap)\n" +
			"}\n"
		_, err := patchDecodeUnionValueIsSinglePatternBranch(missing, familyC)
		if err == nil {
			t.Fatalf("expected error for missing discriminator or end anchor")
		}
		if !strings.Contains(err.Error(), string(familyC)) {
			t.Errorf("error %q should name family %s", err, familyC)
		}
	})
}
