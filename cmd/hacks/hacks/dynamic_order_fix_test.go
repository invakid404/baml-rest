package hacks

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
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

			// Postcondition (Option A IsSinglePattern dispatch): the
			// optional / single-pattern branch — the path BAML drives
			// for `T | null` shapes — dispatches on the inner CFFI
			// holder kind. Only dynamic class values and dynamic-value
			// maps flow through DecodeToOrderedValue; lists, scalars,
			// nested unions, and statically-typed maps stay on plain
			// Decode so the optional wrapper can `Set(value)` into a
			// `reflect.New(pointerToConcrete)` without a type mismatch.
			// A prior pass routed every IsSinglePattern shape through
			// DecodeToOrderedValue and panicked on `optional(list<T>)`
			// static fields with `*[]any` vs `*[]T`.
			//
			// The dispatch line `if isOrderableSinglePattern(...)` is
			// unique to the new branch body and the helper definition
			// is appended once per file; together they pin both halves
			// of the after-image across v0.204 (isOptionalPattern) and
			// v0.215+ (IsSinglePattern).
			if !strings.Contains(body, "if isOrderableSinglePattern(valueUnion.Value, typeMap)") {
				t.Fatalf("decode.go in %s did not dispatch the IsSinglePattern / isOptionalPattern branch through isOrderableSinglePattern", v)
			}
			if !strings.Contains(body, "func isOrderableSinglePattern(") {
				t.Fatalf("decode.go in %s missing isOrderableSinglePattern helper definition", v)
			}
			// Postcondition (cold-v6 finding 1 / B2): the _MapValue arm
			// of isOrderableSinglePattern delegates to
			// isOrderableMapValueType — a ValueType-discriminated
			// helper that does not consult the runtime's
			// `INTERNAL.nil` sentinel. That sentinel is absent from
			// the external TypeMap a generated client populates, so
			// the pre-B2 probe returned false universally and
			// `optional(map<string, dynamic>)` lost key order. B2
			// replaces the broken probe with direct CFFI oneof
			// inspection.
			if !strings.Contains(body, "func isOrderableMapValueType(") {
				t.Fatalf("decode.go in %s missing isOrderableMapValueType helper definition", v)
			}
			if !strings.Contains(body, "isOrderableMapValueType(v.MapValue.") {
				t.Fatalf("decode.go in %s did not delegate the _MapValue arm of isOrderableSinglePattern to isOrderableMapValueType", v)
			}
			// The previous helper (commit-era de3bbd5c3) read
			// `typeMap["INTERNAL.nil"]` (or `typeMap.typeMap["INTERNAL.nil"]`
			// on familyC) inside the _MapValue arm; B2 must not bring
			// that probe back. Other call sites in the file (e.g.
			// convertFieldTypeToGoType's NullType branch) still
			// reference INTERNAL.nil legitimately, so the assertion
			// is scoped to the helper-local probe shapes only.
			for _, banned := range []string{
				`nilType, ok := typeMap["INTERNAL.nil"]`,
				`nilType, ok := typeMap.typeMap["INTERNAL.nil"]`,
			} {
				if strings.Contains(body, banned) {
					t.Fatalf("decode.go in %s still carries the pre-B2 INTERNAL.nil sentinel probe %q", v, banned)
				}
			}
			if !strings.Contains(body, "decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)") {
				t.Fatalf("decode.go in %s did not route the orderable IsSinglePattern / isOptionalPattern branch through DecodeToOrderedValue", v)
			}
			// The pre-patch family A action line `return Decode(value, typeMap)`
			// must be gone — the new template references valueUnion.Value
			// directly in its fallback, so the bare `value` form would
			// only appear if the patch failed to replace the branch.
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

// TestStaticMapClientRewrite_RewritesTypesAndAsserts pins the issue
// #366 static-map pass: every concrete `map[string]T` field/return is
// rewritten to `baml.OrderedMap[T]`, every `Decode().Interface().(map[string]T)`
// cast routes through a generated typed conversion helper, and the
// per-package helper file lands alongside the patched code. The
// dynamic-surface `map[string]any` and `DynamicProperties` shapes are
// left untouched so the dynamic-only pipeline still works.
func TestStaticMapClientRewrite_RewritesTypesAndAsserts(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
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
	Headers      map[string]string  ` + "`json:\"headers\"`" + `
	Counts       map[string]int     ` + "`json:\"counts\"`" + `
	NestedMap    map[string]map[string]string ` + "`json:\"nested_map\"`" + `
	OptionalMap  *map[string]string ` + "`json:\"opt_map,omitempty\"`" + `
	DynamicAny   map[string]any
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "headers":
			s.Headers = baml.Decode(valueHolder).Interface().(map[string]string)
		case "counts":
			s.Counts = baml.Decode(valueHolder).Interface().(map[string]int)
		case "nested_map":
			s.NestedMap = baml.Decode(valueHolder).Interface().(map[string]map[string]string)
		case "opt_map":
			s.OptionalMap = baml.Decode(valueHolder).Interface().(*map[string]string)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}

func (s Sample) Encode() (*cffi.HostValue, error) {
	fields := map[string]any{}
	fields["headers"] = s.Headers
	return baml.EncodeClass("Sample", fields, nil)
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)
	// Concrete types rewritten.
	for _, marker := range []string{
		"baml.OrderedMap[string]",
		"baml.OrderedMap[int]",
		"baml.OrderedMap[baml.OrderedMap[string]]",
		"*baml.OrderedMap[string]",
	} {
		if !strings.Contains(after, marker) {
			t.Fatalf("missing rewritten type %q in patched file:\n%s", marker, after)
		}
	}
	// Dynamic surfaces untouched.
	if !strings.Contains(after, "map[string]any") {
		t.Fatalf("dynamic surface map[string]any was rewritten unexpectedly:\n%s", after)
	}
	// Decode asserts now route through helpers.
	for _, helper := range []string{
		"bamlOrderedAs_OM_string",
		"bamlOrderedAs_OM_int",
		"baml.DecodeToOrderedValue(valueHolder)",
	} {
		if !strings.Contains(after, helper) {
			t.Fatalf("expected helper marker %q in patched file:\n%s", helper, after)
		}
	}

	// Helper file emitted alongside.
	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	for _, marker := range []string{
		"package types",
		"func bamlOrderedAs_OM_string(",
		"func bamlOrderedAs_OM_int(",
		"RangeAny(func(string, any) bool)",
	} {
		if !strings.Contains(helperBody, marker) {
			t.Fatalf("helper file missing %q:\n%s", marker, helperBody)
		}
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

// TestStaticMapHelperFile_AdoptsSiblingBAMLImportPath pins the
// per-context import path used by the emitted helper file. The two
// codepaths the static-map pass runs through (cmd/regenerate-dynclient
// and cmd/build/build.sh) target different BAML module paths; the
// helper has to match whichever path the sibling generated files
// declare or it will fail to resolve at compile time.
//
// upstreamImport mirrors the BAML-emitted preamble in the integration
// build pipeline (cmd/build/build.sh): `baml` is aliased to the
// upstream module path and no post-pass import rewrite runs, so the
// helper must adopt the same path.
//
// patchedImport mirrors the committed dynclient tree after
// RewriteGeneratedClientBAMLImports lands on it: `baml` is aliased to
// the patched-fork path. The helper must follow.
func TestStaticMapHelperFile_AdoptsSiblingBAMLImportPath(t *testing.T) {
	cases := []struct {
		name       string
		importPath string
	}{
		{name: "upstream", importPath: "github.com/boundaryml/baml/engine/language_client_go/pkg"},
		{name: "patched_fork", importPath: "github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/pkg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()
			clientDir := filepath.Join(srcDir, "baml_client")
			typesDir := filepath.Join(clientDir, "types")
			if err := os.MkdirAll(typesDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			classesSrc := fmt.Sprintf(`package types

import (
	"fmt"

	baml %q
	%q
)

type Sample struct {
	Headers map[string]string `+"`json:\"headers\"`"+`
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "headers":
			s.Headers = baml.Decode(valueHolder).Interface().(map[string]string)
		default:
			panic(fmt.Sprintf("unexpected field: %%s", key))
		}
	}
}
`, tc.importPath, tc.importPath+"/cffi")
			classesPath := filepath.Join(typesDir, "classes.go")
			if err := os.WriteFile(classesPath, []byte(classesSrc), 0o644); err != nil {
				t.Fatalf("write classes.go: %v", err)
			}

			hack := &DynamicOrderClientHack{}
			if err := hack.Apply(clientDir); err != nil {
				t.Fatalf("apply: %v", err)
			}

			helperPath := filepath.Join(typesDir, "ordered_map_static.go")
			helperBody := readFileT(t, helperPath)
			wantImport := fmt.Sprintf(`baml %q`, tc.importPath)
			if !strings.Contains(helperBody, wantImport) {
				t.Fatalf("helper file does not adopt sibling import path.\nwant import line containing: %s\nhelper file:\n%s", wantImport, helperBody)
			}
			// The classes.go path is the only acceptable BAML module
			// reference — the helper must not stamp a different path
			// alongside the sibling-derived one.
			other := "github.com/boundaryml/baml/engine/language_client_go/pkg"
			if tc.importPath == other {
				other = "github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/pkg"
			}
			if strings.Contains(helperBody, other) {
				t.Fatalf("helper file references unrelated import %q in addition to sibling %q:\n%s", other, tc.importPath, helperBody)
			}
		})
	}
}

// TestStaticMapClientRewrite_TopLevelMapReturnCasts pins the
// direct-cast surfaces the static-map pass must cover beyond class
// Decode bodies: top-level, parse, and stream final/partial paths
// cast `result.Data` (and `result`, `result.StreamData`) directly to
// the asserted return type. For a `map<string, T>` return, pass 1
// rewrites the asserted type to `baml.OrderedMap[T]`, but the
// patched runtime still hands back an ordered carrier; an unrouted
// direct assertion panics at runtime. The static-map pass therefore
// recognises the direct-cast shapes the generator emits and routes
// each through the same typed helper used for the
// `baml.Decode(...).Interface()` sites in class Decode bodies.
//
// The fixture mirrors the four BAML-generator shapes:
//   - top-level call: `casted := (result.Data).(map[string]string)`
//   - parse:          `casted := (result).(map[string]string)`
//   - stream final:   `data := (result.Data).(map[string]string)`
//   - stream partial: `data := (result.StreamData).(stream_types.X)`
//     (kept as a non-map control to make sure non-map asserts are not
//     rewritten)
func TestStaticMapClientRewrite_TopLevelMapReturnCasts(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const before = `package baml_client

import (
	baml "github.com/example/fake-baml-patched/pkg"
)

func CallMap(result struct {
	Data any
}) map[string]string {
	casted := (result.Data).(map[string]string)
	return casted
}

func ParseMap(result any) map[string]string {
	casted := (result).(map[string]string)
	return casted
}

func StreamMap(result struct {
	Data       any
	StreamData any
}) (map[string]string, map[string]string) {
	final := (result.Data).(map[string]string)
	partial := result.StreamData.(map[string]string)
	return final, partial
}

func _bamlUse() { _ = baml.NewOrderedFields }
`
	path := filepath.Join(clientDir, "functions.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)
	// All four direct-cast sites must route through the helper. The
	// type expression after pass 1 is `baml.OrderedMap[string]`, so a
	// surviving `.(baml.OrderedMap[` substring would indicate the cast
	// was not rewritten.
	if strings.Contains(after, ".(baml.OrderedMap[") {
		t.Fatalf("direct-cast assertion to baml.OrderedMap[...] still present after rewrite:\n%s", after)
	}
	// The helper call must wrap each carrier expression.
	for _, marker := range []string{
		"bamlOrderedAs_OM_string(result.Data)",
		"bamlOrderedAs_OM_string(result)",
		"bamlOrderedAs_OM_string(result.StreamData)",
	} {
		if !strings.Contains(after, marker) {
			t.Fatalf("expected helper call %q in patched file:\n%s", marker, after)
		}
	}

	helperPath := filepath.Join(clientDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	if !strings.Contains(helperBody, "func bamlOrderedAs_OM_string(") {
		t.Fatalf("helper file missing bamlOrderedAs_OM_string declaration:\n%s", helperBody)
	}
}

// TestStaticMapClientRewrite_MapOfListElementConversion pins the
// element-wise conversion of `map<string, list<T>>` carriers. A
// direct `v.([]T)` assertion in the helper body would panic, because
// the patched DecodeToOrderedValue returns `[]any` for every list
// (`interface conversion: []interface {} is not []string`). The
// helper routes the carrier through a per-element-type list helper
// that converts the `[]any` to `[]T` element-by-element, and
// composes the same way for nested lists / qualified element types.
func TestStaticMapClientRewrite_MapOfListElementConversion(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
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
	Lists       map[string][]string ` + "`json:\"lists\"`" + `
	NestedLists map[string][][]string ` + "`json:\"nested_lists\"`" + `
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "lists":
			s.Lists = baml.Decode(valueHolder).Interface().(map[string][]string)
		case "nested_lists":
			s.NestedLists = baml.Decode(valueHolder).Interface().(map[string][][]string)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	// The helper body must NOT contain a direct `v.([]string)` /
	// `v.([][]string)` assertion — that's the pre-fix shape that panics.
	for _, banned := range []string{
		"v.([]string)",
		"v.([][]string)",
	} {
		if strings.Contains(helperBody, banned) {
			t.Fatalf("helper body still contains direct list assertion %q (panics on []any):\n%s", banned, helperBody)
		}
	}
	// The element-wise loop must produce `[]string` from `[]any`.
	// The exact shape is implementation-defined; assert on the structural
	// markers that the converter emits — a `value.([]any)` cast against
	// the runtime carrier and a `make([]string, ...)` allocation
	// scaled to the carrier length.
	mustContain := []string{
		"value.([]any)",
		"make([]string,",
		"make([][]string,",
	}
	for _, marker := range mustContain {
		if !strings.Contains(helperBody, marker) {
			t.Fatalf("helper body missing element-wise conversion marker %q:\n%s", marker, helperBody)
		}
	}
	// The helper file must compile under go/parser at least.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, helperPath, []byte(helperBody), parser.ParseComments); err != nil {
		t.Fatalf("helper file does not parse: %v\n%s", err, helperBody)
	}
}

// TestStaticMapClientRewrite_NestedOnlyPackageHasInnerHelper pins
// the transitive helper collection contract: when a package contains
// a nested map field but no flat-map sibling, the outer helper
// recursively calls an inner helper that never appears verbatim in
// any rewritten file. ensureStaticMapHelperFile must walk every
// emitted helper's body for nested `bamlOrderedAs_*` references and
// include them in the emitted set; otherwise the outer helper calls
// an undefined identifier and the package fails to compile.
func TestStaticMapClientRewrite_NestedOnlyPackageHasInnerHelper(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
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
	NestedMap map[string]map[string]string ` + "`json:\"nested_map\"`" + `
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "nested_map":
			s.NestedMap = baml.Decode(valueHolder).Interface().(map[string]map[string]string)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	// Both the outer helper (referenced by the rewritten code) AND
	// the inner helper it calls transitively MUST be declared in the
	// helper file. Pre-fix, only the outer landed and the file failed
	// to compile.
	for _, decl := range []string{
		"func bamlOrderedAs_OM_OM_string(",
		"func bamlOrderedAs_OM_string(",
	} {
		if !strings.Contains(helperBody, decl) {
			t.Fatalf("helper file missing required declaration %q:\n%s", decl, helperBody)
		}
	}
	// The helper file must parse.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, helperPath, []byte(helperBody), parser.ParseComments); err != nil {
		t.Fatalf("helper file does not parse: %v\n%s", err, helperBody)
	}
}

// TestStaticMapClientRewrite_QualifiedTypesRoundTripAndImports pins
// two coupled contracts the static-map pass needs for cross-package
// element types like `types.Foo` and `stream_types.Bar`:
//
//  1. The helper-name encoding round-trips so the rendered helper
//     declares the correct `baml.OrderedMap[<pkg>.<Type>]` signature.
//     A `.`-as-`_` encoding would let the decoder treat
//     `_<uppercase>` as a generic close and produce nonsense like
//     `baml.OrderedMap[types]Foo`.
//
//  2. The emitted helper file imports every sibling generated
//     package its helper bodies reference. Without that, helpers in
//     `baml_client/` (or `stream_types/`) can't compile when they
//     reach into a sibling `types` package.
//
// The fixture lives at baml_client/ (not types/), so a value type of
// `types.Foo` requires both a correct round-trip in the helper name
// AND a sibling `types` import in the emitted helper file.
func TestStaticMapClientRewrite_QualifiedTypesRoundTripAndImports(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	streamDir := filepath.Join(clientDir, "stream_types")
	for _, d := range []string{typesDir, streamDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// Sibling package decls so the helper file's import block can
	// pick up the actual import paths via the package-by-name lookup.
	if err := os.WriteFile(filepath.Join(typesDir, "classes.go"), []byte(`package types

type Foo struct{}
`), 0o644); err != nil {
		t.Fatalf("write types/classes.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(streamDir, "classes.go"), []byte(`package stream_types

type Bar struct{}
`), 0o644); err != nil {
		t.Fatalf("write stream_types/classes.go: %v", err)
	}

	const before = `package baml_client

import (
	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"

	"github.com/example/proj/baml_client/types"
	"github.com/example/proj/baml_client/stream_types"
)

type Sample struct {
	FooMap map[string]types.Foo        ` + "`json:\"foo_map\"`" + `
	BarMap map[string]stream_types.Bar ` + "`json:\"bar_map\"`" + `
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		valueHolder := field.Value
		switch field.Key {
		case "foo_map":
			s.FooMap = baml.Decode(valueHolder).Interface().(map[string]types.Foo)
		case "bar_map":
			s.BarMap = baml.Decode(valueHolder).Interface().(map[string]stream_types.Bar)
		}
	}
	_ = cffi.CFFIValueClass{}
}
`
	path := filepath.Join(clientDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(clientDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// The helper function signatures must declare the qualified
	// return type — the pre-fix encoder/decoder pair garbled this.
	for _, sig := range []string{
		"baml.OrderedMap[types.Foo]",
		"baml.OrderedMap[stream_types.Bar]",
	} {
		if !strings.Contains(helperBody, sig) {
			t.Fatalf("helper file missing helper signature with %q:\n%s", sig, helperBody)
		}
	}
	// The garbled pre-fix forms must NOT appear.
	for _, banned := range []string{
		"baml.OrderedMap[types]Foo",
		"baml.OrderedMap[stream]types",
		"baml.OrderedMap[stream]Bar",
	} {
		if strings.Contains(helperBody, banned) {
			t.Fatalf("helper file still contains garbled qualified-type encoding %q:\n%s", banned, helperBody)
		}
	}
	// The helper file must import the sibling packages it references.
	for _, importPath := range []string{
		"github.com/example/proj/baml_client/types",
		"github.com/example/proj/baml_client/stream_types",
	} {
		if !strings.Contains(helperBody, importPath) {
			t.Fatalf("helper file missing required sibling import %q:\n%s", importPath, helperBody)
		}
	}
	// The helper file must parse with go/parser; type-checking
	// happens via the broader compile sweep.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, helperPath, []byte(helperBody), parser.ParseComments); err != nil {
		t.Fatalf("helper file does not parse: %v\n%s", err, helperBody)
	}
}

// TestStaticMapClientRewrite_SliceWrappedOrderedMapHasHelper pins
// the slice-wrapped variant of the static-map helper contract.
// `isStaticMapAssertType` accepts `[]map[string]T` and
// `[]baml.OrderedMap[T]` assertions, so a generated `list<map<string, T>>`
// field reaches `staticMapHelperName` with an `Slice_OM_*` encoding.
// Before this fix, `staticMapHelperTypeFromName` only accepted
// `OM_*` and `Ptr_OM_*` encodings, so the helper for a
// `bamlOrderedAs_Slice_OM_*` call was never emitted and the
// rewritten package failed to compile with an undefined symbol.
//
// The fixture pairs a pre-rewrite `[]map[string]T` field with a
// post-rewrite-shape `[]baml.OrderedMap[T]` field to cover both
// the pass-1 promotion of native list-of-maps AND the case where a
// previous run already left an `[]baml.OrderedMap[T]` carrier in
// place.
func TestStaticMapClientRewrite_SliceWrappedOrderedMapHasHelper(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const before = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type ConcreteClass struct{}

type Sample struct {
	Headers []map[string]string             ` + "`json:\"headers\"`" + `
	Records []baml.OrderedMap[ConcreteClass] ` + "`json:\"records\"`" + `
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "headers":
			s.Headers = baml.Decode(valueHolder).Interface().([]map[string]string)
		case "records":
			s.Records = baml.Decode(valueHolder).Interface().([]baml.OrderedMap[ConcreteClass])
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)
	// Pass 1 must promote []map[string]string to []baml.OrderedMap[string].
	if !strings.Contains(after, "[]baml.OrderedMap[string]") {
		t.Fatalf("pass 1 did not promote []map[string]string:\n%s", after)
	}
	// Both call sites must route through the slice-wrapped helper.
	for _, marker := range []string{
		"bamlOrderedAs_Slice_OM_string(baml.DecodeToOrderedValue(valueHolder))",
		"bamlOrderedAs_Slice_OM_ConcreteClass(baml.DecodeToOrderedValue(valueHolder))",
	} {
		if !strings.Contains(after, marker) {
			t.Fatalf("expected slice-wrapped helper call %q in patched file:\n%s", marker, after)
		}
	}

	// The helper file must define both slice-wrapped helpers AND
	// the inner OM helpers they recurse through. Without all four
	// declarations the package has an undefined symbol.
	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	for _, decl := range []string{
		"func bamlOrderedAs_Slice_OM_string(",
		"func bamlOrderedAs_Slice_OM_ConcreteClass(",
		"func bamlOrderedAs_OM_string(",
		"func bamlOrderedAs_OM_ConcreteClass(",
	} {
		if !strings.Contains(helperBody, decl) {
			t.Fatalf("helper file missing required declaration %q:\n%s", decl, helperBody)
		}
	}
	// The slice-wrapped helper signature must declare the typed
	// list return — a `bamlOrderedAs_Slice_OM_string` returning
	// anything other than `[]baml.OrderedMap[string]` would point
	// to a regression in the name-to-type decoder.
	for _, sig := range []string{
		"func bamlOrderedAs_Slice_OM_string(value any) []baml.OrderedMap[string]",
		"func bamlOrderedAs_Slice_OM_ConcreteClass(value any) []baml.OrderedMap[ConcreteClass]",
	} {
		if !strings.Contains(helperBody, sig) {
			t.Fatalf("helper file missing slice-wrapped signature %q:\n%s", sig, helperBody)
		}
	}
	// The helper body must recurse through the inner ordered-map
	// helper; a direct `ev.(baml.OrderedMap[...])`
	// assertion would not work — the runtime carrier from
	// DecodeToOrderedValue is an OrderedFields, not a typed OrderedMap,
	// so a direct cast
	// panics.
	for _, banned := range []string{
		"ev.(baml.OrderedMap[string])",
		"ev.(baml.OrderedMap[ConcreteClass])",
	} {
		if strings.Contains(helperBody, banned) {
			t.Fatalf("helper body still has direct OrderedMap assertion %q (panics on OrderedFields):\n%s", banned, helperBody)
		}
	}
	// The helper file must parse cleanly.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, helperPath, []byte(helperBody), parser.ParseComments); err != nil {
		t.Fatalf("helper file does not parse: %v\n%s", err, helperBody)
	}
}

// TestStaticMapClientRewrite_AliasedSiblingImportPreserved pins
// the explicit-alias contract for sibling imports. When a
// surrounding file imports a sibling generated package under an
// explicit alias (e.g. `models "example.com/baml_client/types"`)
// and a static map field references that alias
// (`map[string]models.Foo`), the helper body emits the alias name.
// Before this fix, the helper file's import block stamped the
// default segment name from the path — binding `types`, not
// `models` — so `models.Foo` in the helper body resolved to an
// undefined identifier and the `types` import was unused.
func TestStaticMapClientRewrite_AliasedSiblingImportPreserved(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(typesDir, "classes.go"), []byte(`package types

type Foo struct{}
`), 0o644); err != nil {
		t.Fatalf("write types/classes.go: %v", err)
	}

	const before = `package baml_client

import (
	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"

	models "github.com/example/proj/baml_client/types"
)

type Sample struct {
	FooMap map[string]models.Foo ` + "`json:\"foo_map\"`" + `
}

func (s *Sample) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		valueHolder := field.Value
		if field.Key == "foo_map" {
			s.FooMap = baml.Decode(valueHolder).Interface().(map[string]models.Foo)
		}
	}
	_ = cffi.CFFIValueClass{}
}
`
	path := filepath.Join(clientDir, "classes.go")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(clientDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// The helper file's import block must carry the same explicit
	// alias `models` against the sibling path. The default-name
	// binding `types` would leave `models` undefined in the helper
	// body.
	wantAliased := `models "github.com/example/proj/baml_client/types"`
	if !strings.Contains(helperBody, wantAliased) {
		t.Fatalf("helper file does not preserve explicit alias.\nwant: %s\nhelper file:\n%s", wantAliased, helperBody)
	}
	// The unaliased default import would bind `types`; the helper
	// body never references `types.Foo` so the import would be
	// unused — guard against that drift explicitly.
	unaliased := `"github.com/example/proj/baml_client/types"` + "\n"
	if strings.Contains(helperBody, "\t"+unaliased) {
		t.Fatalf("helper file uses unaliased sibling import; would bind `types` not `models`:\n%s", helperBody)
	}
	// The helper body must reference the aliased name.
	if !strings.Contains(helperBody, "baml.OrderedMap[models.Foo]") {
		t.Fatalf("helper body does not use the alias `models`:\n%s", helperBody)
	}
	if strings.Contains(helperBody, "baml.OrderedMap[types.Foo]") {
		t.Fatalf("helper body rewrote alias `models` to default name `types`:\n%s", helperBody)
	}
	// The helper file must parse cleanly.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, helperPath, []byte(helperBody), parser.ParseComments); err != nil {
		t.Fatalf("helper file does not parse: %v\n%s", err, helperBody)
	}
}

// TestRecursiveMapSkip pins the issue #366 / cold-v3 BLOCK fix for
// the recursive-type-alias trap. The Go compiler ICEs on a
// `baml.OrderedMap[T]` instantiation when T is a type alias whose
// transitive closure references the same alias in another map element
// position (the integration `JsonValue = *Union6...` shape). The
// static-map pass must leave the recursive arm as native
// `map[string]T` and route the matching decode/assert site through a
// `bamlNativeMapAs_*` helper so the resulting tree compiles without
// the ICE — while still rewriting non-recursive map fields in the
// same package normally.
//
// The fixture mirrors the integration shape compactly: a `Self` alias
// to a union struct that has a `map[string]Self` arm, plus a plain
// `map[string]int` arm on the same struct that must still rewrite
// to `baml.OrderedMap[int]`. The rewrite is correct only when both
// surface types come out simultaneously consistent: the recursive
// arm stays native and the non-recursive arm flips to OrderedMap.
func TestRecursiveMapSkip(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const aliasSrc = `package types

type Self = *MyUnion
`
	if err := os.WriteFile(filepath.Join(typesDir, "type_aliases.go"), []byte(aliasSrc), 0o644); err != nil {
		t.Fatalf("write type_aliases.go: %v", err)
	}

	const unionSrc = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type MyUnion struct {
	variant                     string
	variant_Int                 *int64
	variant_String              *string
	variant_MapSelf             *map[string]Self
	variant_MapInt              *map[string]int
}

func (u *MyUnion) Decode(holder *cffi.CFFIValueUnionVariant, typeMap baml.TypeMap) {
	valueHolder := holder.Value
	variantName := holder.ValueOptionName
	switch variantName {
	case "int":
		u.variant = "Int"
		value := baml.Decode(valueHolder).Int()
		u.variant_Int = &value
	case "string":
		u.variant = "String"
		value := baml.Decode(valueHolder).Interface().(string)
		u.variant_String = &value
	case "Map__string_Self":
		u.variant = "MapSelf"
		value := baml.Decode(valueHolder).Interface().(map[string]Self)
		u.variant_MapSelf = &value
	case "Map__string_int":
		u.variant = "MapInt"
		value := baml.Decode(valueHolder).Interface().(map[string]int)
		u.variant_MapInt = &value
	default:
		panic(fmt.Sprintf("unexpected variant: %s", variantName))
	}
}

func (u *MyUnion) AsMapSelf() *map[string]Self {
	if u.variant != "MapSelf" {
		return nil
	}
	return u.variant_MapSelf
}

func (u *MyUnion) AsMapInt() *map[string]int {
	if u.variant != "MapInt" {
		return nil
	}
	return u.variant_MapInt
}

func MyUnion__NewMapSelf(v map[string]Self) MyUnion {
	return MyUnion{
		variant:         "MapSelf",
		variant_MapSelf: &v,
	}
}
`
	unionsPath := filepath.Join(typesDir, "unions.go")
	if err := os.WriteFile(unionsPath, []byte(unionSrc), 0o644); err != nil {
		t.Fatalf("write unions.go: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, unionsPath)

	// Recursive arm stays native (no OrderedMap[Self], no rewritten cast).
	if strings.Contains(after, "baml.OrderedMap[Self]") {
		t.Fatalf("recursive map[string]Self was rewritten to baml.OrderedMap[Self] — would ICE on Go compiler:\n%s", after)
	}
	for _, want := range []string{
		"variant_MapSelf *map[string]Self",
		"func (u *MyUnion) AsMapSelf() *map[string]Self",
		"func MyUnion__NewMapSelf(v map[string]Self)",
		"bamlNativeMapAs_Self(baml.DecodeToOrderedValue(valueHolder))",
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("missing required recursive-arm shape %q in patched file:\n%s", want, after)
		}
	}
	// The recursive Decode assertion must not route through the
	// OrderedMap helper — that would produce baml.OrderedMap[Self]
	// at the helper boundary.
	if strings.Contains(after, "bamlOrderedAs_OM_Self") {
		t.Fatalf("recursive arm routed through OrderedMap helper, native-map helper expected:\n%s", after)
	}

	// Non-recursive arm still flips to OrderedMap normally.
	for _, want := range []string{
		"variant_MapInt  *baml.OrderedMap[int]",
		"func (u *MyUnion) AsMapInt() *baml.OrderedMap[int]",
		"bamlOrderedAs_OM_int(baml.DecodeToOrderedValue(valueHolder))",
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("non-recursive map[string]int was not rewritten as expected; missing %q:\n%s", want, after)
		}
	}

	// Helper file must define the native-map helper for Self and the
	// ordered helper for int.
	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	for _, want := range []string{
		"func bamlNativeMapAs_Self(value any) map[string]Self",
		"func bamlOrderedAs_OM_int(",
	} {
		if !strings.Contains(helperBody, want) {
			t.Fatalf("helper file missing %q:\n%s", want, helperBody)
		}
	}
	// The native-map helper must not advertise order preservation —
	// the recursive arm's documented trade-off is that order is lost.
	if !strings.Contains(helperBody, "CFFI insertion order is lost on this arm") {
		t.Fatalf("native-map helper does not document the order-loss trade-off:\n%s", helperBody)
	}
}

// TestFuncReturnMapRewrite pins the issue #366 / cold-v3 Finding-2
// fix for the composite-literal guard. A function declaration whose
// return type is `*?map[string]T` is followed by ` {` opening the
// function body — the same trailing-`{` shape as a `map[string]T{...}`
// composite literal. The pre-fix guard skipped the rewrite for both
// shapes, leaving union `As*` accessors with a stale `*map[string]T`
// return type that no longer matched the rewritten storage field's
// `*baml.OrderedMap[T]` type.
//
// The fixture covers four shapes simultaneously:
//   - `func AsX() *map[string]T {` must rewrite the return type.
//   - `func F() map[string]T {`     same, without the pointer wrapper.
//   - `return map[string]T{}` (composite literal) must NOT rewrite —
//     OrderedMap's fields are unexported and a literal allocation
//     would fail to compile.
//   - `var v = map[string]T{}` (composite literal in expression
//     context) must NOT rewrite for the same reason.
func TestFuncReturnMapRewrite(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	baml "github.com/example/fake-baml-patched/pkg"
	_ "github.com/example/fake-baml-patched/pkg/cffi"
)

type Container struct {
	storage *map[string]string
}

func (c *Container) AsStringMap() *map[string]string {
	return c.storage
}

func PlainStringMap() map[string]string {
	return map[string]string{"k": "v"}
}

var seed = map[string]string{"a": "b"}

var _ = baml.TypeMap{}

var _ = seed
`
	path := filepath.Join(typesDir, "container.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, path)

	// Function return types must flip to OrderedMap.
	for _, want := range []string{
		"func (c *Container) AsStringMap() *baml.OrderedMap[string]",
		"func PlainStringMap() baml.OrderedMap[string]",
		"storage *baml.OrderedMap[string]",
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("missing rewritten return type %q in patched file:\n%s", want, after)
		}
	}

	// Composite literals must NOT flip — those would not compile
	// because baml.OrderedMap's fields are unexported.
	if strings.Contains(after, "baml.OrderedMap[string]{") {
		t.Fatalf("composite literal map[string]string{...} was rewritten to baml.OrderedMap[string]{...}, would fail to compile:\n%s", after)
	}
	for _, mustStay := range []string{
		`return map[string]string{"k": "v"}`,
		`var seed = map[string]string{"a": "b"}`,
	} {
		if !strings.Contains(after, mustStay) {
			t.Fatalf("composite literal lost expected form %q after rewrite:\n%s", mustStay, after)
		}
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
	// Fragments that on their own pin the failure to the test
	// environment's missing cgo / native BAML libraries. A syntax or
	// type error in the patched serde package surfaces under a
	// different shape and is re-raised as a real failure.
	allowedFragments := []string{
		"package cffi",
		"build constraints exclude all Go files",
		"cgo",
		"undefined: C.",
		"_cgo_export.h",
		"libbaml",
		"baml_cffi_wrapper.h",
		"language_client_go/baml_go/lib_",
		"no Go files",
		"no buildable Go source files",
	}
	pairedCgoTokens := []string{"cgo", "C source", "libbaml", "lib_baml", "_cgo_", "baml_cffi"}

	// The verdict is scoped per line so a benign `could not import
	// example.com/pkg (no Go files in /…)` is not blocked by an
	// unrelated `no Go files` further down the output, and conversely
	// a real diagnostic on a different line is not masked by an
	// otherwise-allowed import line. The function walks every
	// non-noise line and tracks two flags: hasCouldNotImport for a
	// validated import diagnostic, hasOtherDiagnostic for any line
	// that is neither an allowed import nor matches an
	// allowedFragments substring. Headers (`# pkg/path`) and blank
	// lines are skipped as noise. The function returns true only
	// when at least one line is allowed and no other diagnostic is
	// present; mixed shapes — a valid import diagnostic plus a real
	// `cannot use X as Y` elsewhere — return false.
	hasCouldNotImport := false
	hasOtherDiagnostic := false
	hasAllowedFragment := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "could not import") {
			lineAllowed := false
			for _, paired := range pairedCgoTokens {
				if strings.Contains(line, paired) {
					lineAllowed = true
					break
				}
			}
			if !lineAllowed {
				for _, f := range allowedFragments {
					if strings.Contains(line, f) {
						lineAllowed = true
						break
					}
				}
			}
			if !lineAllowed {
				return false
			}
			hasCouldNotImport = true
			continue
		}
		lineAllowed := false
		for _, f := range allowedFragments {
			if strings.Contains(line, f) {
				lineAllowed = true
				break
			}
		}
		if lineAllowed {
			hasAllowedFragment = true
		} else {
			hasOtherDiagnostic = true
		}
	}

	if hasOtherDiagnostic {
		return false
	}
	return hasCouldNotImport || hasAllowedFragment
}

func TestIsAllowedVetFailure(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "benign-same-line-no-go-files",
			out:  "app.go:3:8: could not import example.com/badpkg (no Go files in /home/x/example.com/badpkg)",
			want: true,
		},
		{
			name: "benign-same-line-no-buildable",
			out:  "app.go:3:8: could not import example.com/cgopkg (no buildable Go source files in /home/x/example.com/cgopkg)",
			want: true,
		},
		{
			name: "real-failure-no-go-files-elsewhere",
			out:  "app.go:3:8: could not import example.com/realfailure (transitive dep broken)\nother.go:1: no Go files in /unrelated/path",
			want: false,
		},
		{
			name: "cgo-stub-package",
			out:  "package cffi: build constraints exclude all Go files in /pkg/cffi",
			want: true,
		},
		{
			name: "could-not-import-with-cgo-token-on-same-line",
			out:  "could not import example.com/_cgo_pkg (reason)",
			want: true,
		},
		{
			name: "real-import-failure",
			out:  "could not import example.com/realfailure (some other reason)",
			want: false,
		},
		{
			name: "mixed-allowed-import-plus-real-diagnostic",
			out:  "app.go:3:8: could not import example.com/cgopkg (no Go files in /path)\ndecode.go:123:5: cannot use X (untyped string constant) as int",
			want: false,
		},
		{
			name: "mixed-cgo-stub-plus-real-diagnostic",
			out:  "# pkg/cffi\npkg/cffi/wrapper.go:5:8: undefined: C.foo\ndecode.go:123:5: cannot use X as Y",
			want: false,
		},
		{
			name: "cgo-stub-with-header-and-multiple-allowed-lines",
			out:  "# pkg/cffi\npkg/cffi/wrapper.go:5:8: undefined: C.foo\npkg/cffi/wrapper.go:6:8: undefined: C.bar",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAllowedVetFailure(tc.out)
			if got != tc.want {
				t.Fatalf("isAllowedVetFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
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
				"\t\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n",
				"\t\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\t\treturn reflect.ValueOf(nil)\n",
				"\t\t\treturn reflect.ValueOf(decoded)\n",
				"\t\treturn Decode(valueUnion.Value, typeMap)\n",
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
				"\t\t\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n",
				"\t\t\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\t\t\treturn reflect.ValueOf(nil), nil\n",
				"\t\t\t\trv := reflect.ValueOf(decoded)\n",
				"\t\t\t\treturn rv, rv.Type()\n",
				"\t\t\treturn Decode(valueUnion.Value, typeMap)\n",
			},
			mustNotContain: []string{
				// The pre-patch one-line shape — the new template wraps
				// the fallback Decode inside the dispatch's else arm, so
				// the unadorned discriminator line must be gone.
				"// For optional patterns (T | null), decode the inner value directly",
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
				"\t\t\tif isOrderableSinglePattern(valueUnion.Value, typeMap) {\n",
				"\t\t\t\tdecoded := DecodeToOrderedValue(valueUnion.Value, typeMap)\n",
				"\t\t\t\trv := reflect.ValueOf(decoded)\n",
				"\t\t\t\treturn rv, rv.Type()\n",
				"\t\t\treturn Decode(valueUnion.Value, typeMap)\n",
			},
			mustNotContain: []string{
				"// reworded: drop union-ness for optional shape",
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

// TestPatchDecodeGo_PartiallyPatchedTreeIsCompleted pins the
// idempotency contract patchDecodeGo must satisfy when
// preparePatchedBamlModuleDir hands back a cached
// .baml_patched_modules tree that an earlier hack revision left in a
// partially-patched state — specifically, the DynamicClass struct
// field has already been switched to OrderedFields but the
// IsSinglePattern dispatch never landed. The previous sentinel
// short-circuited on the first marker alone, so the dispatch step was
// silently skipped on cache reuse. The tightened sentinel keeps
// looking until both markers are present, and each step is robust
// enough to complete the patch on a partially-applied tree.
func TestPatchDecodeGo_PartiallyPatchedTreeIsCompleted(t *testing.T) {
	tmp := t.TempDir()
	moduleDir := filepath.Join(tmp, "baml")
	serdeDir := filepath.Join(moduleDir, "engine", "language_client_go", "baml_go", "serde")
	if err := os.MkdirAll(serdeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Synthetic v0.219+ shaped decode.go where the DynamicClass field
	// is already in its post-patch form and a stale (pre-dispatch)
	// IsSinglePattern after-image is in place. The DecodeToOrderedValue
	// helper is already appended; the new isOrderableSinglePattern
	// helper is missing. This mirrors what a cached patched-module
	// tree looks like after an earlier hack run on a previous commit.
	partial := `package serde

import (
	"reflect"

	"github.com/example/fake-baml/engine/language_client_go/pkg/cffi"
)

type DynamicClass struct {
	Name   string
	Fields OrderedFields
}

func (d *DynamicClass) Decode(holder *cffi.CFFIValueClass, typeMap TypeMap) {
	d.Name = string(holder.Name.Name)
	d.Fields = NewOrderedFields(len(holder.Fields))
	for _, field := range holder.Fields {
		_ = d.Fields.Set(field.Key, DecodeToOrderedValue(field.Value, typeMap))
	}
}

type DynamicUnion struct {
	Variant string
	Value   any
}

func (d *DynamicUnion) Decode(holder *cffi.CFFIValueUnionVariant, typeMap TypeMap) {
	d.Variant = string(holder.ValueOptionName)
	d.Value = DecodeToOrderedValue(holder.Value, typeMap)
}

func decodeUnionValue(valueUnion *cffi.CFFIValueUnionVariant, typeMap TypeMap) (reflect.Value, reflect.Type) {
	value, goType := func() (reflect.Value, reflect.Type) {
		if ok := valueUnion.Value.GetNullValue(); ok != nil {
			return reflect.ValueOf(nil), nil
		} else if valueUnion.IsSinglePattern {
			decoded := DecodeToOrderedValue(valueUnion.Value, typeMap)
			if decoded == nil {
				return reflect.ValueOf(nil), nil
			}
			rv := reflect.ValueOf(decoded)
			return rv, rv.Type()
		} else {
			dynamicUnion := DynamicUnion{
				Variant: valueUnion.Name.Name,
				Value:   DecodeToOrderedValue(valueUnion.Value, typeMap),
			}
			value := reflect.ValueOf(dynamicUnion)
			goType = reflect.TypeOf(DynamicUnion{})
			return value, goType
		}
	}()
	return value, goType
}

// Unpatched decodeMapValue body — a cached tree from the prior hack
// revision left this untouched. The new patch must rewrite it.
func decodeMapValue(valueMap *cffi.CFFIValueMap, typeMap TypeMap) (reflect.Value, reflect.Type) {
	if valueMap == nil {
		panic("decodeMapValue: valueMap is nil")
	}
	keyType := valueMap.KeyType
	valueType := valueMap.ValueType
	goKeyType := convertFieldTypeToGoType(keyType, typeMap)
	goValueType := convertFieldTypeToGoType(valueType, typeMap)
	debugLog("goValueType: %+v\n", goValueType)
	debugLog("typeMap.typeMap[\"INTERNAL.nil\"]: %+v\n", typeMap.typeMap["INTERNAL.nil"])
	if goValueType == typeMap.typeMap["INTERNAL.nil"] {
		values := map[string]any{}
		for _, entry := range valueMap.Entries {
			key := entry.Key
			value := entry.Value
			decodedValue, goType := Decode(value, typeMap)
			switch goType {
			case reflect.TypeOf(int64(0)):
				values[key] = decodedValue.Int()
			case reflect.TypeOf(float64(0)):
				values[key] = decodedValue.Float()
			case reflect.TypeOf(false):
				values[key] = decodedValue.Bool()
			default:
				values[key] = decodedValue.Interface()
			}
		}
		return reflect.ValueOf(values), reflect.TypeOf(values)
	} else {
		mapType := reflect.MapOf(goKeyType, goValueType)
		values := reflect.MakeMap(mapType)
		for _, entry := range valueMap.Entries {
			key := entry.Key
			value := entry.Value
			decodedValue, _ := Decode(value, typeMap)
			values.SetMapIndex(reflect.ValueOf(key), decodedValue)
		}
		return values, mapType
	}
}

// Unpatched convertFieldTypeToGoType — only the MapType arm is
// inspected by D5; the surrounding switches are stubbed so the function
// parses.
func convertFieldTypeToGoType(fieldType *cffi.CFFIFieldTypeHolder, typeMap TypeMap) reflect.Type {
	if map_, ok := fieldType.Type.(*cffi.CFFIFieldTypeHolder_MapType); ok {
		mapType := map_.MapType
		goKeyType := convertFieldTypeToGoType(mapType.KeyType, typeMap)
		goValueType := convertFieldTypeToGoType(mapType.ValueType, typeMap)
		if goValueType == typeMap.typeMap["INTERNAL.nil"] {
			return reflect.TypeOf(map[string]any{})
		}
		return reflect.MapOf(goKeyType, goValueType)
	}
	return nil
}

func DecodeToOrderedValue(holder *cffi.CFFIValueHolder, typeMap TypeMap) any {
	return nil
}

func Decode(holder *cffi.CFFIValueHolder, typeMap TypeMap) (reflect.Value, reflect.Type) {
	return reflect.ValueOf(nil), nil
}

func debugLog(format string, args ...any) {}
`
	if err := os.WriteFile(filepath.Join(serdeDir, "decode.go"), []byte(partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Run the focused step under test. patchDecodeGo is the function
	// whose sentinel determines whether to skip a cached tree.
	if err := patchDecodeGo(moduleDir, familyC); err != nil {
		t.Fatalf("patchDecodeGo on partially-patched tree: %v", err)
	}

	patched, err := os.ReadFile(filepath.Join(serdeDir, "decode.go"))
	if err != nil {
		t.Fatalf("read patched: %v", err)
	}
	body := string(patched)

	// Postcondition: the dispatch helper and its call site landed —
	// the partially-patched tree was completed, not skipped.
	if !strings.Contains(body, "if isOrderableSinglePattern(valueUnion.Value, typeMap)") {
		t.Fatalf("partially-patched tree was not completed: IsSinglePattern dispatch missing\n%s", body)
	}
	if !strings.Contains(body, "func isOrderableSinglePattern(") {
		t.Fatalf("partially-patched tree was not completed: isOrderableSinglePattern helper missing\n%s", body)
	}
	// cold-v6 finding 1 / B2: the ValueType-discriminated helper
	// must also land on a partially-patched tree. Cached patched-
	// module copies from the de3bbd5c3 commit era satisfy both the
	// OrderedFields struct field marker and the isOrderableSinglePattern
	// marker but lack isOrderableMapValueType; the tightened sentinel
	// forces a re-pass that adds it.
	if !strings.Contains(body, "func isOrderableMapValueType(") {
		t.Fatalf("partially-patched tree was not completed: isOrderableMapValueType helper missing\n%s", body)
	}
	if !strings.Contains(body, "isOrderableMapValueType(v.MapValue.ValueType, typeMap)") {
		t.Fatalf("partially-patched tree was not completed: _MapValue arm does not delegate to isOrderableMapValueType\n%s", body)
	}
	// issue #366: decodeMapValue body must have been rewritten on the
	// re-pass to emit OrderedFields. The marker is family-stable and
	// embedded in the after-image.
	if !strings.Contains(body, orderedMapDecodeMarker) {
		t.Fatalf("partially-patched tree was not completed: decodeMapValue did not land the ordered after-image\n%s", body)
	}
	// issue #366: the convertFieldTypeToGoType map arm advertises the
	// OrderedFields carrier type; the upstream reflect.MapOf(K, V)
	// return is replaced by reflect.TypeOf(OrderedFields{}).
	if !strings.Contains(body, orderedMapTypeMarker) {
		t.Fatalf("partially-patched tree was not completed: convertFieldTypeToGoType map arm did not land the ordered after-image\n%s", body)
	}

	// And the prior in-place markers (DynamicClass.Fields, the union
	// dynamic branch's ordered routing) remain — the re-pass converged
	// without unwinding them.
	if !strings.Contains(body, "Fields OrderedFields") {
		t.Fatalf("re-pass over partially-patched tree wiped the OrderedFields struct field\n%s", body)
	}
	if !strings.Contains(body, "Value:   DecodeToOrderedValue(valueUnion.Value, typeMap),") {
		t.Fatalf("re-pass over partially-patched tree wiped the dynamic-union ordered routing\n%s", body)
	}

	// No duplicate DecodeToOrderedValue declarations — the append
	// step skipped when the function name was already present.
	if got := strings.Count(body, "func DecodeToOrderedValue("); got != 1 {
		t.Fatalf("expected exactly 1 DecodeToOrderedValue declaration after re-pass; got %d\n%s", got, body)
	}
	if got := strings.Count(body, "func isOrderableSinglePattern("); got != 1 {
		t.Fatalf("expected exactly 1 isOrderableSinglePattern declaration after re-pass; got %d\n%s", got, body)
	}
	if got := strings.Count(body, "func isOrderableMapValueType("); got != 1 {
		t.Fatalf("expected exactly 1 isOrderableMapValueType declaration after re-pass; got %d\n%s", got, body)
	}

	// A second re-pass over the now-fully-patched tree must be a no-op:
	// the tightened sentinel sees both markers and skips before any
	// per-step rewrite runs.
	before, _ := os.ReadFile(filepath.Join(serdeDir, "decode.go"))
	if err := patchDecodeGo(moduleDir, familyC); err != nil {
		t.Fatalf("patchDecodeGo on fully-patched tree: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(serdeDir, "decode.go"))
	if string(before) != string(after) {
		t.Fatalf("second pass over fully-patched tree mutated decode.go")
	}
}

// TestIsOrderableMapValueType_PerFamily encodes the verdict table for
// the rendered isOrderableMapValueType helper as text-shape assertions
// against the emitted source. The helper is generated as Go source
// (not callable from the hacks package) so the per-family case arms
// and per-family lookup forms are validated structurally.
//
// The table mirrors the user-facing dispatch contract:
//
//   - AnyType / NullType → true (structurally dynamic)
//   - UnionVariantType{Name: nil} → true (no name to look up)
//   - UnionVariantType{Name: <typeMap miss>} → true (dynamic union)
//   - UnionVariantType{Name: <typeMap hit>} → false (concrete user union)
//   - OptionalType / CheckedType / StreamStateType → recurse into
//     the inner value type
//   - Concrete scalar / class / enum / list / map / type-alias / tuple →
//     false (default fall-through; tuple has an explicit arm on
//     familyA)
//
// The per-family difference is the UnionVariantType lookup form:
// familyA/familyB index TypeMap directly using
// `name.Namespace.Enum().String() + "." + name.Name`; familyC calls
// `typeMap.GetType(name)` (the only stable accessor on the struct-
// wrapped TypeMap).
//
// Regression coverage for cold-v6 finding 1 lives in the dispatch
// check: the helper does not consult `typeMap["INTERNAL.nil"]` at any
// point, so a generated client whose external TypeMap lacks that
// sentinel still routes AnyType / NullType ValueType maps through
// the ordered pipeline. Regression coverage for cold-v5 lives in the
// _MapValue accessor check on the caller side (a non-_MapValue holder
// falls into the `default: return false` arm and the optional wrapper
// stays on plain Decode, avoiding the `*[]any` vs `*[]string` panic
// for `optional(list<string>)`).
func TestIsOrderableMapValueType_PerFamily(t *testing.T) {
	type contract struct {
		mustContain    []string
		mustNotContain []string
	}
	commonTrueArms := []string{
		"case *cffi.CFFIFieldTypeHolder_AnyType:",
		"case *cffi.CFFIFieldTypeHolder_NullType:",
		"if t.UnionVariantType == nil || t.UnionVariantType.Name == nil {",
		"return true",
	}
	commonRecurse := []string{
		"case *cffi.CFFIFieldTypeHolder_OptionalType:",
		"return isOrderableMapValueType(t.OptionalType.Value, typeMap)",
		"case *cffi.CFFIFieldTypeHolder_CheckedType:",
		"return isOrderableMapValueType(t.CheckedType.Value, typeMap)",
		"case *cffi.CFFIFieldTypeHolder_StreamStateType:",
		"return isOrderableMapValueType(t.StreamStateType.Value, typeMap)",
	}
	commonStaticFallthrough := []string{
		"default:",
		"return false",
	}
	commonBanned := []string{
		// The whole point of B2 — the helper must not depend on the
		// generated client's TypeMap carrying an INTERNAL.nil entry.
		`typeMap["INTERNAL.nil"]`,
		`typeMap.typeMap["INTERNAL.nil"]`,
		// The previous helper called convertFieldTypeToGoType to
		// resolve the value type and compared against nilType.
		// B2 dispatches directly on the CFFI oneof variant.
		"convertFieldTypeToGoType(v.MapValue.ValueType, typeMap)",
		"convertFieldTypeToGoType(v.MapValue.Value, typeMap)",
	}

	cases := map[decodeFamily]contract{
		familyA: {
			mustContain: append(append(append(
				[]string{
					// familyA TypeMap is a plain map; lookup mirrors
					// convertFieldTypeToGoType's union-variant resolution.
					`key := name.Namespace.Enum().String() + "." + name.Name`,
					"_, ok := typeMap[key]",
					"return !ok",
					// TupleType is familyA-only and lands on an explicit
					// static-verdict arm.
					"case *cffi.CFFIFieldTypeHolder_TupleType:",
				},
				commonTrueArms...), commonRecurse...), commonStaticFallthrough...),
			mustNotContain: append(commonBanned,
				// familyC accessor must not leak into familyA.
				"typeMap.GetType(t.UnionVariantType.Name)",
			),
		},
		familyB: {
			mustContain: append(append(append(
				[]string{
					`key := name.Namespace.Enum().String() + "." + name.Name`,
					"_, ok := typeMap[key]",
					"return !ok",
				},
				commonTrueArms...), commonRecurse...), commonStaticFallthrough...),
			mustNotContain: append(commonBanned,
				// No TupleType variant exists on familyB.
				"case *cffi.CFFIFieldTypeHolder_TupleType:",
				// familyC accessor must not leak into familyB.
				"typeMap.GetType(t.UnionVariantType.Name)",
			),
		},
		familyC: {
			mustContain: append(append(append(
				[]string{
					// familyC TypeMap is a struct; GetType is the only
					// stable accessor.
					"_, ok := typeMap.GetType(t.UnionVariantType.Name)",
					"return !ok",
				},
				commonTrueArms...), commonRecurse...), commonStaticFallthrough...),
			mustNotContain: append(commonBanned,
				"case *cffi.CFFIFieldTypeHolder_TupleType:",
				// No direct-index map lookup; that is the familyA/B shape.
				`name.Namespace.Enum().String() + "." + name.Name`,
			),
		},
	}

	for family, want := range cases {
		t.Run(string(family), func(t *testing.T) {
			body := isOrderableMapValueTypeFunc(family)
			if body == "" {
				t.Fatalf("isOrderableMapValueTypeFunc(%s) returned empty body", family)
			}
			for _, marker := range want.mustContain {
				if !strings.Contains(body, marker) {
					t.Errorf("rendered helper for %s missing %q\n--- helper ---\n%s", family, marker, body)
				}
			}
			for _, marker := range want.mustNotContain {
				if strings.Contains(body, marker) {
					t.Errorf("rendered helper for %s unexpectedly contains %q\n--- helper ---\n%s", family, marker, body)
				}
			}
		})
	}
}

// TestNativeMapHelperPreservesNilValues pins the cold-v4 Finding 1
// regression for the recursive-type-alias path. BAML's `JsonValue`
// alias resolves to `*Union6...` and accepts `null` as a variant; the
// patched runtime decodes a null map value to an untyped nil in the
// ranger callback. Without explicit nil handling, the helper's typed
// assertion `v.(JsonValue)` rejects untyped nil and the key drops
// from the produced map — turning a documented order-loss arm into
// silent data loss. The fix emits a `v == nil` branch that writes
// `out[k] = nil`, preserving the key.
//
// The fixture mirrors the integration shape: a `Self` alias to a
// pointer-bearing union, plus a non-nilable element type on a
// sibling helper to confirm only the nilable arm emits the nil
// preservation branch.
func TestNativeMapHelperPreservesNilValues(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const aliasSrc = `package types

type Self = *MyUnion
`
	if err := os.WriteFile(filepath.Join(typesDir, "type_aliases.go"), []byte(aliasSrc), 0o644); err != nil {
		t.Fatalf("write type_aliases.go: %v", err)
	}

	const unionSrc = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type MyUnion struct {
	variant         string
	variant_Int     *int64
	variant_MapSelf *map[string]Self
}

func (u *MyUnion) Decode(holder *cffi.CFFIValueUnionVariant, typeMap baml.TypeMap) {
	valueHolder := holder.Value
	variantName := holder.ValueOptionName
	switch variantName {
	case "int":
		u.variant = "Int"
		value := baml.Decode(valueHolder).Int()
		u.variant_Int = &value
	case "Map__string_Self":
		u.variant = "MapSelf"
		value := baml.Decode(valueHolder).Interface().(map[string]Self)
		u.variant_MapSelf = &value
	default:
		panic(fmt.Sprintf("unexpected variant: %s", variantName))
	}
}

func (u *MyUnion) AsMapSelf() *map[string]Self {
	if u.variant != "MapSelf" {
		return nil
	}
	return u.variant_MapSelf
}
`
	unionsPath := filepath.Join(typesDir, "unions.go")
	if err := os.WriteFile(unionsPath, []byte(unionSrc), 0o644); err != nil {
		t.Fatalf("write unions.go: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// The nilable arm's helper must guard against untyped nil
	// BEFORE the typed assertion and preserve the key.
	const nilGuard = "if v == nil {\n\t\t\t\tout[k] = nil\n\t\t\t\treturn true\n\t\t\t}"
	if !strings.Contains(helperBody, nilGuard) {
		t.Fatalf("native-map helper missing nil-preservation branch:\n%s", helperBody)
	}
	// The typed-assertion path remains in place — the guard is
	// additive, not a replacement.
	if !strings.Contains(helperBody, "if cv, ok := v.(Self); ok {") {
		t.Fatalf("native-map helper lost typed-assertion path for non-nil values:\n%s", helperBody)
	}
	// The guard must precede the typed assertion textually so the
	// nil case wins over the assertion (which would reject untyped
	// nil and silently drop the key).
	idxGuard := strings.Index(helperBody, "if v == nil")
	idxAssert := strings.Index(helperBody, "if cv, ok := v.(Self); ok {")
	if idxGuard < 0 || idxAssert < 0 || idxGuard > idxAssert {
		t.Fatalf("nil guard does not precede typed assertion:\n%s", helperBody)
	}
}

// TestNativeMapHelperSkipsNilForNonNilableElement pins the other side
// of the nilability split: a recursive map whose element type is a
// non-nilable struct must not emit the `out[k] = nil` branch. The
// pkgTypeIndex resolves the alias chain to a struct definition and
// reports the element as non-nilable; the helper renderer then omits
// the nil-preservation arm because the BAML runtime never hands an
// untyped nil into the ranger callback for non-nilable types (and
// writing a zero struct would falsely advertise a present entry).
//
// This test guards against future drift where the nilability check
// is dropped or always-true: were it always-true, this fixture's
// helper would compile-fail because `out[k] = nil` is not assignable
// to a non-nilable struct value.
func TestNativeMapHelperSkipsNilForNonNilableElement(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// `Self` aliases a struct that has a `map[string]Self` field, so
	// the recursive-map guard fires (Self is an alias whose closure
	// contains a map<string,Self>) but the element type itself is a
	// non-nilable named struct.
	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type Self = StructNode

type StructNode struct {
	Children map[string]Self
}

type Wrapper struct {
	variant         string
	variant_MapSelf *map[string]Self
}

func (w *Wrapper) Decode(holder *cffi.CFFIValueUnionVariant, typeMap baml.TypeMap) {
	switch holder.ValueOptionName {
	case "Map__string_Self":
		value := baml.Decode(holder.Value).Interface().(map[string]Self)
		w.variant_MapSelf = &value
	default:
		panic(fmt.Sprintf("unexpected: %s", holder.ValueOptionName))
	}
}
`
	path := filepath.Join(typesDir, "node.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)
	// The native helper for the non-nilable `Self` (alias→struct)
	// must not emit `out[k] = nil` because nil is not assignable to
	// a struct value.
	if strings.Contains(helperBody, "out[k] = nil") {
		t.Fatalf("non-nilable element helper unexpectedly emitted nil preservation:\n%s", helperBody)
	}
	// The typed-assertion path remains.
	if !strings.Contains(helperBody, "if cv, ok := v.(Self); ok {") {
		t.Fatalf("non-nilable helper lost typed-assertion path:\n%s", helperBody)
	}
}

// TestStaticMapRewriteAddsBamlImport pins the cold-v4 Finding 2
// regression for files that gain a `baml.OrderedMap[T]` reference
// during the rewrite but did not import the BAML pkg beforehand. The
// integration build's `baml_client/embed.go`, written by cmd/embed
// before the hacks pass, only imports `embed`; a `make(map[string]X)`
// argument used to surface as `undefined: baml` after rewriting (a
// separate fix skips the make-call rewrite outright). Other shapes —
// a struct field with `map[string]string` and no `make` wrapper —
// still need a rewrite plus the missing import; this fixture pins
// that path: rewrite happens AND the file gains a `baml` aliased
// import.
func TestStaticMapRewriteAddsBamlImport(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Sibling file declares the BAML pkg import so the helper / import
	// resolver knows which path to bind in the rewritten file.
	const siblingSrc = `package types

import (
	baml "github.com/example/fake-baml-patched/pkg"
)

var _ = baml.TypeMap{}
`
	if err := os.WriteFile(filepath.Join(typesDir, "sibling.go"), []byte(siblingSrc), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	// Target file has no baml import; the rewrite must introduce one.
	const targetSrc = `package types

type Standalone struct {
	Headers map[string]string
}
`
	targetPath := filepath.Join(typesDir, "standalone.go")
	if err := os.WriteFile(targetPath, []byte(targetSrc), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, targetPath)
	if !strings.Contains(after, "Headers baml.OrderedMap[string]") {
		t.Fatalf("target field was not rewritten:\n%s", after)
	}
	if !strings.Contains(after, `baml "github.com/example/fake-baml-patched/pkg"`) {
		t.Fatalf("rewritten file did not gain the baml aliased import:\n%s", after)
	}
	// Validate the rewritten file parses cleanly — guards against
	// the AST surgery emitting an ill-formed import block.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, targetPath, []byte(after), parser.ParseComments); err != nil {
		t.Fatalf("rewritten file does not parse: %v\n%s", err, after)
	}
}

// TestStaticMapRewriteSkipsMakeMapArg pins the cold-v4 Finding 2
// regression for cmd/embed-shaped files. The generator-emitted
// `embed.go` has `var Sources = make(map[string]embed.FS)` plus
// `Sources["k"] = v`. Rewriting the `make` argument to
// `baml.OrderedMap[embed.FS]` produces a struct type that `make`
// rejects at compile time, and the index-assignment line fails for
// the same reason. The rewrite must leave runtime map allocations
// untouched; this fixture asserts the file is byte-identical after
// the static-map pass (no rewrite landed, no helper file emitted).
func TestStaticMapRewriteSkipsMakeMapArg(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package baml_client

import (
	"embed"
)

//go:embed sentinel.txt
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
}
`
	embedPath := filepath.Join(clientDir, "embed.go")
	if err := os.WriteFile(embedPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// `//go:embed` requires the named pattern to exist.
	if err := os.WriteFile(filepath.Join(clientDir, "sentinel.txt"), []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := readFileT(t, embedPath)
	if after != src {
		t.Fatalf("embed.go changed after static-map pass:\n--- before ---\n%s\n--- after ---\n%s", src, after)
	}
	// No helper file should have been emitted for a package with no
	// successful rewrites.
	helperPath := filepath.Join(clientDir, "ordered_map_static.go")
	if _, err := os.Stat(helperPath); err == nil {
		t.Fatalf("unexpected helper file emitted for embed-only package: %s", helperPath)
	}
}

// TestIsOrderableSinglePatternFunc_DelegatesToMapValueType pins the
// IsSinglePattern helper's _MapValue accessor and confirms it hands
// off to the value-type discriminator on every family. The pre-B2
// helper carried a typeMap-sentinel check inline (cold-v6 finding 1);
// the post-B2 helper must call isOrderableMapValueType and stop
// referencing INTERNAL.nil. The value-side `CFFIValueMap.ValueType`
// field is consistently named across all families — only the
// type-descriptor inner-field names differ, and that variance is
// absorbed inside isOrderableMapValueType.
func TestIsOrderableSinglePatternFunc_DelegatesToMapValueType(t *testing.T) {
	const delegateCall = "isOrderableMapValueType(v.MapValue.ValueType, typeMap)"
	for _, family := range []decodeFamily{familyA, familyB, familyC} {
		t.Run(string(family), func(t *testing.T) {
			body := isOrderableSinglePatternFunc(family)
			if !strings.Contains(body, delegateCall) {
				t.Errorf("%s helper missing delegate call %q\n--- helper ---\n%s", family, delegateCall, body)
			}
			// cold-v6 finding 1 regression — the helper no longer
			// short-circuits on the INTERNAL.nil sentinel that may be
			// absent from a generated client's external TypeMap.
			for _, banned := range []string{
				`typeMap["INTERNAL.nil"]`,
				`typeMap.typeMap["INTERNAL.nil"]`,
				"convertFieldTypeToGoType(v.MapValue.ValueType, typeMap)",
				"convertFieldTypeToGoType(v.MapValue.Value, typeMap)",
			} {
				if strings.Contains(body, banned) {
					t.Errorf("%s helper still references pre-B2 sentinel/conversion %q\n--- helper ---\n%s", family, banned, body)
				}
			}
		})
	}
}

// TestOrderedMapHelperPreservesNilPtrValue pins the cold-v5 regression
// for the typed ordered-map helper. The native-map helper (cold-v4)
// guards against untyped nil values from the BAML ranger callback,
// but the equivalent guard was missing on the
// `bamlOrderedAs_OM_<T>(value any) baml.OrderedMap[T]` emission. For
// a class field declared as `map[string]*ConcreteClass` the rewrite
// surfaces a `baml.OrderedMap[*ConcreteClass]`; an `OrderedFields`
// carrier with `Set("a", nil)` panics inside the helper at
// `v.(*ConcreteClass)` before the assignment can record the nil.
// The fix mirrors the native-map approach: emit
// `if v == nil { _ = out.Set(k, nil); return true }` ahead of the
// typed assertion so the carrier's CFFI insertion order survives a
// null entry.
//
// The fixture declares a pointer-element map field and asserts the
// generated helper contains the guard with the right structure (guard
// precedes assertion, sink writes `nil` to the same `out.Set` call).
func TestOrderedMapHelperPreservesNilPtrValue(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type ConcreteClass struct {
	Name string
}

type WithPtrMap struct {
	Items map[string]*ConcreteClass
}

func (w *WithPtrMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "items":
			w.Items = baml.Decode(valueHolder).Interface().(map[string]*ConcreteClass)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// The pointer-element helper must include the nil guard ahead of
	// the typed assertion. The guard writes the key with a nil value
	// to `out.Set` so the carrier's insertion order survives.
	const nilGuard = "if v == nil {\n\t\t\t\t_ = out.Set(k, nil)\n\t\t\t\treturn true\n\t\t\t}"
	if !strings.Contains(helperBody, nilGuard) {
		t.Fatalf("ordered-map helper missing nil-preservation branch:\n%s", helperBody)
	}
	// The typed-assertion `_ = out.Set(k, v.(*ConcreteClass))` line
	// must remain — the guard is additive, not a replacement.
	if !strings.Contains(helperBody, "_ = out.Set(k, v.(*ConcreteClass))") {
		t.Fatalf("ordered-map helper lost typed-assertion path:\n%s", helperBody)
	}
	// The guard must precede the typed assertion textually so the
	// nil case wins over the assertion (which would panic on
	// untyped nil and crash the decode).
	idxGuard := strings.Index(helperBody, "if v == nil {")
	idxAssert := strings.Index(helperBody, "_ = out.Set(k, v.(*ConcreteClass))")
	if idxGuard < 0 || idxAssert < 0 || idxGuard > idxAssert {
		t.Fatalf("nil guard does not precede typed assertion:\n%s", helperBody)
	}
}

// TestOrderedMapHelperPreservesNilAliasPtrValue pins the alias-chain
// arm of the ordered-map nil guard. A `type AliasPtr = *ConcreteClass`
// declaration is a bare identifier in the element-type slot but
// resolves through the package index to a pointer; the nilability
// check must follow the alias hop and emit the guard. Without the
// alias-walk the helper would skip the guard and the typed assertion
// `v.(AliasPtr)` would panic on untyped nil for the same reason as
// the direct-pointer case.
func TestOrderedMapHelperPreservesNilAliasPtrValue(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type ConcreteClass struct {
	Name string
}

type AliasPtr = *ConcreteClass

type WithAliasMap struct {
	Items map[string]AliasPtr
}

func (w *WithAliasMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "items":
			w.Items = baml.Decode(valueHolder).Interface().(map[string]AliasPtr)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// Same structural expectations as the direct-pointer case, but
	// the typed assertion is against the alias name. The nilability
	// walk must resolve `AliasPtr` → `*ConcreteClass` → nilable.
	const nilGuard = "if v == nil {\n\t\t\t\t_ = out.Set(k, nil)\n\t\t\t\treturn true\n\t\t\t}"
	if !strings.Contains(helperBody, nilGuard) {
		t.Fatalf("ordered-map helper missing nil-preservation branch for alias element:\n%s", helperBody)
	}
	if !strings.Contains(helperBody, "_ = out.Set(k, v.(AliasPtr))") {
		t.Fatalf("ordered-map helper lost typed-assertion path for alias element:\n%s", helperBody)
	}
	idxGuard := strings.Index(helperBody, "if v == nil {")
	idxAssert := strings.Index(helperBody, "_ = out.Set(k, v.(AliasPtr))")
	if idxGuard < 0 || idxAssert < 0 || idxGuard > idxAssert {
		t.Fatalf("nil guard does not precede typed assertion for alias element:\n%s", helperBody)
	}
}

// TestOrderedMapHelperSkipsNilGuardForNonNilableElement pins the
// other side of the nilability split for the ordered-map helper. A
// `map[string]string` field must not emit the `v == nil` branch
// because the BAML runtime never hands an untyped nil into the
// ranger callback for non-nilable element types — emitting the guard
// anyway would compile-fail at `_ = out.Set(k, nil)` since `nil` is
// not assignable to a `string` value parameter. The guard is gated on
// the type expression's nilability so non-nilable elements skip it
// cleanly.
func TestOrderedMapHelperSkipsNilGuardForNonNilableElement(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type WithStringMap struct {
	Items map[string]string
}

func (w *WithStringMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "items":
			w.Items = baml.Decode(valueHolder).Interface().(map[string]string)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// The non-nilable element helper must not emit the
	// `_ = out.Set(k, nil)` branch — `nil` is not assignable to a
	// `string` and would compile-fail if emitted.
	if strings.Contains(helperBody, "_ = out.Set(k, nil)") {
		t.Fatalf("non-nilable string-element helper unexpectedly emitted nil preservation:\n%s", helperBody)
	}
	// The typed-assertion path remains.
	if !strings.Contains(helperBody, "_ = out.Set(k, v.(string))") {
		t.Fatalf("non-nilable string-element helper lost typed-assertion path:\n%s", helperBody)
	}
}

// TestConvertListHelperPreservesNilPtrValue pins the list-helper arm
// of the cold-v5 nil-guard fix. The map-of-list rewrite produces a
// per-element-type `bamlConvertList_<T>` helper that walks the
// `[]any` carrier from `baml.DecodeToOrderedValue` and converts each
// slot to T. For T = `*ConcreteClass` an untyped nil slot would panic
// the raw `ev.(*ConcreteClass)` assertion; the fix mirrors the
// ordered-map guard with `if ev == nil { out[i] = nil; continue }`.
func TestConvertListHelperPreservesNilPtrValue(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type ConcreteClass struct {
	Name string
}

type WithPtrListMap struct {
	Items map[string][]*ConcreteClass
}

func (w *WithPtrListMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "items":
			w.Items = baml.Decode(field.Value).Interface().(map[string][]*ConcreteClass)
		default:
			panic(fmt.Sprintf("unexpected: %s", field.Key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	const nilGuard = "if ev == nil {\n\t\t\tout[i] = nil\n\t\t\tcontinue\n\t\t}"
	if !strings.Contains(helperBody, nilGuard) {
		t.Fatalf("convert-list helper missing nil-preservation branch for pointer element:\n%s", helperBody)
	}
	if !strings.Contains(helperBody, "out[i] = ev.(*ConcreteClass)") {
		t.Fatalf("convert-list helper lost typed-assertion path for pointer element:\n%s", helperBody)
	}
	idxGuard := strings.Index(helperBody, "if ev == nil {")
	idxAssert := strings.Index(helperBody, "out[i] = ev.(*ConcreteClass)")
	if idxGuard < 0 || idxAssert < 0 || idxGuard > idxAssert {
		t.Fatalf("nil guard does not precede typed assertion in convert-list helper:\n%s", helperBody)
	}
}

// TestConvertListHelperSkipsNilGuardForNonNilableElement is the
// non-nilable side of the convert-list nil guard: a
// `map[string][]string` field rewrites to a list helper for
// `[]string`; the helper must not emit `out[i] = nil` because the
// zero string would silently masquerade as a present nil entry (and
// the assignment would compile-fail anyway since `nil` is not
// assignable to a `string`).
func TestConvertListHelperSkipsNilGuardForNonNilableElement(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type WithStrListMap struct {
	Items map[string][]string
}

func (w *WithStrListMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		switch field.Key {
		case "items":
			w.Items = baml.Decode(field.Value).Interface().(map[string][]string)
		default:
			panic(fmt.Sprintf("unexpected: %s", field.Key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	if strings.Contains(helperBody, "out[i] = nil") {
		t.Fatalf("non-nilable string-element convert-list helper unexpectedly emitted nil preservation:\n%s", helperBody)
	}
	if !strings.Contains(helperBody, "out[i] = ev.(string)") {
		t.Fatalf("non-nilable string-element convert-list helper lost typed-assertion path:\n%s", helperBody)
	}
}

// TestOrderedMapHelperNilInNestedOrderedMap pins the nested case for
// the ordered-map nil guard. A nested `map[string]map[string]*ConcreteClass`
// surfaces as `baml.OrderedMap[baml.OrderedMap[*ConcreteClass]]`. The
// outer helper recurses through a sub-helper for the inner ordered
// map (which already handles a nil inner carrier by returning the
// zero value), and the inner ordered-map helper carries the pointer-
// element guard so a nil entry inside a present inner carrier is
// preserved as a key with a nil value; without the guard the inner
// `v.(*ConcreteClass)` assertion would panic.
//
// Verified textually: the inner helper for `*ConcreteClass` must
// emit the guard, and the outer helper for the nested ordered map
// must NOT emit the per-value guard (because its inner conversion
// routes through the inner helper, which handles nil itself).
func TestOrderedMapHelperNilInNestedOrderedMap(t *testing.T) {
	srcDir := t.TempDir()
	clientDir := filepath.Join(srcDir, "baml_client")
	typesDir := filepath.Join(clientDir, "types")
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const src = `package types

import (
	"fmt"

	baml "github.com/example/fake-baml-patched/pkg"
	"github.com/example/fake-baml-patched/pkg/cffi"
)

type ConcreteClass struct {
	Name string
}

type WithNestedMap struct {
	Items map[string]map[string]*ConcreteClass
}

func (w *WithNestedMap) Decode(holder *cffi.CFFIValueClass, typeMap baml.TypeMap) {
	for _, field := range holder.Fields {
		key := field.Key
		valueHolder := field.Value
		switch key {
		case "items":
			w.Items = baml.Decode(valueHolder).Interface().(map[string]map[string]*ConcreteClass)
		default:
			panic(fmt.Sprintf("unexpected field: %s", key))
		}
	}
}
`
	path := filepath.Join(typesDir, "classes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hack := &DynamicOrderClientHack{}
	if err := hack.Apply(clientDir); err != nil {
		t.Fatalf("apply: %v", err)
	}

	helperPath := filepath.Join(typesDir, "ordered_map_static.go")
	helperBody := readFileT(t, helperPath)

	// Inner helper (bamlOrderedAs_OM_Ptr_ConcreteClass) carries the
	// pointer-element guard ahead of the typed assertion.
	const nilGuard = "if v == nil {\n\t\t\t\t_ = out.Set(k, nil)\n\t\t\t\treturn true\n\t\t\t}"
	if !strings.Contains(helperBody, nilGuard) {
		t.Fatalf("nested-case inner helper missing nil-preservation branch:\n%s", helperBody)
	}
	if !strings.Contains(helperBody, "_ = out.Set(k, v.(*ConcreteClass))") {
		t.Fatalf("nested-case inner helper lost typed-assertion path:\n%s", helperBody)
	}
	// Outer helper recurses through `bamlOrderedAs_OM_Ptr_ConcreteClass`
	// for each value; the per-value branch in the outer helper is
	// the recursive helper call, not a raw assertion, so no guard is
	// needed (or correct) there. The outer helper's per-element line
	// must NOT have a raw assertion against the inner ordered-map
	// type — that would skip the per-key recursion entirely.
	if strings.Contains(helperBody, "v.(baml.OrderedMap[*ConcreteClass])") {
		t.Fatalf("outer helper unexpectedly bypassed the inner ordered-map helper with a raw assertion:\n%s", helperBody)
	}
	// And the recursive call must be present.
	if !strings.Contains(helperBody, "bamlOrderedAs_OM_Ptr_ConcreteClass(v)") {
		t.Fatalf("outer helper missing the recursive inner-ordered-map call:\n%s", helperBody)
	}
}
