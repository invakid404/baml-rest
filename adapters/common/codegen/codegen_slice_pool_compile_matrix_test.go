package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/fixtures"
	"github.com/invakid404/baml-rest/bamlutils"
)

// TestCompileMatrix is the "broad" complement to the targeted F1 / F2
// / F3 regression pins. The targeted tests assert specific textual
// fragments. This one drives the real generator path
// (ensureMirrorStruct + makePreambleWithArgs) for a matrix of
// legitimate BAML signature shapes, writes the rendered output to a
// temp Go module, and shells out to `go build` to type-check it.
// That catches every class of compile-level bug the rendered output
// can grow — including future variants we haven't pre-enumerated.
//
// Per the meta-analysis driving this test: the existing codegen
// tests verify rendered-source fragments but never type-check the
// output. That gap let five generator-contract bugs slip through
// review. This test directly closes it.
//
// The matrix crosses 7 top-level param shapes with 3 nested field
// shapes (21 cells). Each cell emits one synthetic `cell_<N>_*`
// dispatch function into a shared file; we run `go build` against
// the file once (per the brief's batching note) and rely on the
// type checker to surface mismatches anywhere in the matrix. A
// follow-up AST scan asserts no `__releaseConverted` is emitted in
// the single-non-pooled cell, which `go build` alone wouldn't catch
// (an unnecessary closure would still compile).
func TestCompileMatrix(t *testing.T) {
	t.Parallel()

	// Skip when `go` is not on PATH (the subprocess compile step
	// needs it). In CI Go is always present; in offline / sandbox
	// shells this avoids a noisy failure unrelated to the contract
	// under test.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go binary not on PATH: %v", err)
	}

	out, cells := emitMatrix(t)
	rendered := out.GoString()

	tmp := t.TempDir()
	writeTempModule(t, tmp, rendered)
	runGoBuild(t, tmp)

	parsed, fset := parseRendered(t, rendered)
	assertNoReleaseConvertedInNonPooledCell(t, parsed, fset, cells)
}

// matrixCell records the shape under test and the name of the
// dispatch function the matrix emitter wrote into the shared file.
// AST assertions look up cells by `dispatchFunc`; subprocess
// `go build` does not need the metadata.
type matrixCell struct {
	name                  string
	dispatchFunc          string
	expectReleaseConverted bool
}

// emitMatrix walks the 7-top × 3-nested matrix, drives the real
// generator path for each cell, and returns the shared rendered
// file plus the per-cell metadata the AST checks consume. The file
// uses the `matrix` package name; `runGoBuild` writes it to a temp
// module that imports bamlutils + fixtures via replace directives.
func emitMatrix(t *testing.T) (*jen.File, []matrixCell) {
	t.Helper()

	pkgs := DefaultPackageConfig()
	// Point BamlPkg at the fixtures package so `isMediaReflectType`
	// resolves `Image` as a BAML media type. Without this the
	// fixture structs would not be detected as media-bearing and
	// the matrix would not exercise ensureMirrorStruct at all.
	pkgs.BamlPkg = reflect.TypeOf(fixtures.Image{}).PkgPath()
	pkgs.OutputPkg = "github.com/invakid404/baml-rest/adapters/common/codegen/matrixtest"
	pkgs.OutputPkgName = "matrix"

	out := jen.NewFilePathName(pkgs.OutputPkg, pkgs.OutputPkgName)
	tracker := newMirrorStructTracker()
	pools := newSlicePoolTracker(pkgs)

	// nestedShapes enumerates the 3 nested-field configurations the
	// matrix crosses. The fixture struct families (MessageA /
	// MessageB / MessageC + Class<X> / Other<X> siblings) capture
	// the three shapes:
	//
	//   a — `Parts *[]ContentPart` (value-element pooled, the
	//       closure-context happy path)
	//   b — `Parts *[]*ContentPart` (pointer-element fallback to
	//       the legacy `make` path; converter doesn't take
	//       ownedNested)
	//   c — `Parts *[]Content; Tools *[]Tool` (two distinct pooled
	//       types per converter — the F2 multi-pool shape)
	nestedShapes := []struct {
		name      string
		messageTy reflect.Type
		classTy   reflect.Type
		otherTy   reflect.Type
	}{
		{name: "a_value_elem", messageTy: reflect.TypeOf(fixtures.MessageA{}), classTy: reflect.TypeOf(fixtures.ClassA{}), otherTy: reflect.TypeOf(fixtures.OtherA{})},
		{name: "b_ptr_elem", messageTy: reflect.TypeOf(fixtures.MessageB{}), classTy: reflect.TypeOf(fixtures.ClassB{}), otherTy: reflect.TypeOf(fixtures.OtherB{})},
		{name: "c_two_pooled_types", messageTy: reflect.TypeOf(fixtures.MessageC{}), classTy: reflect.TypeOf(fixtures.ClassC{}), otherTy: reflect.TypeOf(fixtures.OtherC{})},
	}

	// topShapes enumerates the 7 top-level signature shapes per the
	// brief. Each shape is described by a builder that, given the
	// per-nested-shape fixture types, returns the structMediaParam
	// list to feed into the synthetic methodEmitter.
	type paramSpec struct {
		name string
		ty   reflect.Type
	}
	topShapes := []struct {
		name           string
		expectRelease  bool
		buildParams    func(messageTy, classTy, otherTy reflect.Type) []paramSpec
	}{
		{
			name:          "1_pooled_baseline",
			expectRelease: true,
			buildParams: func(messageTy, _, _ reflect.Type) []paramSpec {
				return []paramSpec{{name: "messages", ty: reflect.SliceOf(messageTy)}}
			},
		},
		{
			name:          "2_two_pooled_params",
			expectRelease: true,
			buildParams: func(messageTy, classTy, _ reflect.Type) []paramSpec {
				return []paramSpec{
					{name: "messages", ty: reflect.SliceOf(messageTy)},
					{name: "classes", ty: reflect.SliceOf(classTy)},
				}
			},
		},
		{
			name:          "3_pooled_plus_slice_of_ptr",
			expectRelease: true,
			buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
				return []paramSpec{
					{name: "messages", ty: reflect.SliceOf(messageTy)},
					{name: "extra", ty: reflect.SliceOf(reflect.PointerTo(otherTy))},
				}
			},
		},
		{
			name:          "4_pooled_plus_ptr_to_slice",
			expectRelease: true,
			buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
				return []paramSpec{
					{name: "messages", ty: reflect.SliceOf(messageTy)},
					{name: "extra", ty: reflect.PointerTo(reflect.SliceOf(otherTy))},
				}
			},
		},
		{
			name:          "5_pooled_plus_ptr",
			expectRelease: true,
			buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
				return []paramSpec{
					{name: "messages", ty: reflect.SliceOf(messageTy)},
					{name: "extra", ty: reflect.PointerTo(otherTy)},
				}
			},
		},
		{
			name:          "6_pooled_plus_direct",
			expectRelease: true,
			buildParams: func(messageTy, _, otherTy reflect.Type) []paramSpec {
				return []paramSpec{
					{name: "messages", ty: reflect.SliceOf(messageTy)},
					{name: "extra", ty: otherTy},
				}
			},
		},
		{
			name:          "7_single_non_pooled_ptr",
			// Whether the cell emits __releaseConverted depends on
			// whether `*Other<Shape>`'s converter takes ownedNested.
			// For nested shapes a and c the converter pools, so it
			// takes ownedNested → the dispatch hoists
			// __releaseConverted (to drain the closures).
			// For nested shape b the converter does NOT pool
			// (pointer-element fallback) → no ownedNested → no
			// __releaseConverted. The per-cell expectation is
			// computed below.
			expectRelease: false,
			buildParams: func(_, _, otherTy reflect.Type) []paramSpec {
				return []paramSpec{{name: "extra", ty: reflect.PointerTo(otherTy)}}
			},
		},
	}

	var cells []matrixCell
	for topIdx, top := range topShapes {
		for nestedIdx, nested := range nestedShapes {
			cellIdx := topIdx*len(nestedShapes) + nestedIdx
			cellName := fmt.Sprintf("cell_%02d_%s_%s", cellIdx, top.name, nested.name)

			params := top.buildParams(nested.messageTy, nested.classTy, nested.otherTy)

			// Ensure mirror structs for every struct-media param.
			// This populates `tracker.convertNeedsOwnedNested` so
			// the makePreambleWithArgs call below sees the
			// up-to-date contract.
			smps := make([]structMediaParam, 0, len(params))
			paramTypes := make([]reflect.Type, 0, len(params))
			for _, p := range params {
				mirrorName := tracker.ensureMirrorStruct(out, p.ty, pkgs, pools)
				smps = append(smps, structMediaParam{
					paramName:   p.name,
					mirrorName:  mirrorName,
					convertFunc: "convert" + mirrorName,
					paramType:   p.ty,
				})
				paramTypes = append(paramTypes, p.ty)
			}

			me := newSyntheticMethodEmitter(out, tracker, pools, pkgs, cellName, smps, paramTypes)
			preamble := me.makePreambleWithArgs("makeOptionsFromAdapter")
			// Silence `declared and not used` for the preamble's
			// locals (options, __struct_<name>, etc.). In the real
			// codegen these flow into legacy-stream / BuildRequest
			// emission downstream; the matrix only exercises the
			// preamble, so we tack on a `_ = options` plus a
			// `_ = __struct_<name>` per pooled param.
			tail := []jen.Code{jen.Id("_").Op("=").Id("options")}
			for _, smp := range smps {
				tail = append(tail, jen.Id("_").Op("=").Id("__struct_"+smp.paramName))
			}
			tail = append(tail, jen.Return(jen.Nil()))
			body := append([]jen.Code{}, preamble...)
			body = append(body, tail...)
			// Wrap the preamble in a dispatch function so the
			// rendered file declares a callable function `go build`
			// can type-check. The signature mirrors what the real
			// codegen emits at dispatch sites (adapter, rawInput,
			// error return).
			dispatchName := cellName + "_dispatch"
			out.Func().Id(dispatchName).
				Params(
					jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter"),
					jen.Id("rawInput").Any(),
				).
				Error().
				Block(body...)

			// Compute per-cell expectReleaseConverted. For the
			// single-non-pooled top shape, it's true only when the
			// `*Other` converter itself takes ownedNested (nested
			// shapes a and c).
			expect := top.expectRelease
			if top.name == "7_single_non_pooled_ptr" {
				expect = tracker.convertNeedsOwnedNestedFor(nested.otherTy)
			}

			cells = append(cells, matrixCell{
				name:                   cellName,
				dispatchFunc:           dispatchName,
				expectReleaseConverted: expect,
			})
		}
	}

	// The rendered file's preamble references `makeOptionsFromAdapter`
	// (which the real generator emits elsewhere). Provide a stub so
	// `go build` resolves it.
	out.Func().Id("makeOptionsFromAdapter").
		Params(jen.Id("adapter").Qual(pkgs.InterfacesPkg, "Adapter")).
		Params(jen.Index().Any(), jen.Error()).
		Block(jen.Return(jen.Nil(), jen.Nil()))

	return out, cells
}

// newSyntheticMethodEmitter constructs a minimal methodEmitter that
// makePreambleWithArgs can run against — only the fields the preamble
// path touches need to be set. structMediaParamSet is rebuilt here
// because the public newMethodEmitter wouldn't accept synthetic
// inputs cleanly.
func newSyntheticMethodEmitter(
	out *jen.File,
	tracker *mirrorStructTracker,
	pools *slicePoolTracker,
	pkgs PackageConfig,
	cellName string,
	smps []structMediaParam,
	paramTypes []reflect.Type,
) *methodEmitter {
	g := &generator{
		opts:                 Options{SupportsWithClient: true, Packages: pkgs, Introspection: RootIntrospection()},
		pkgs:                 pkgs,
		intro:                RootIntrospection(),
		out:                  out,
		supportsWithClient:   true,
		mirrors:              tracker,
		emittedUnwrapHelpers: map[string]bool{},
		slicePools:           pools,
	}
	args := make([]string, 0, len(smps))
	for _, smp := range smps {
		args = append(args, smp.paramName)
	}
	me := &methodEmitter{
		g:                 g,
		methodName:        cellName,
		args:              args,
		syncFuncType:      synthSyncFuncType(paramTypes),
		methodMediaParams: map[string]bamlutils.MediaKind{},
		structMediaParams: smps,
		inputStructName:   cellName + "Input",
	}
	me.structMediaParamSet = make(map[string]bool, len(smps))
	for _, smp := range smps {
		me.structMediaParamSet[smp.paramName] = true
	}
	// Declare the synthetic input struct that
	// `rawInput.(*<InputStruct>)` type-asserts to. Fields use the
	// MIRROR type (not the original BAML type) because the real
	// codegen emits the JSON-decoded shape there — the convert
	// function takes `*<Mirror>`, so the loop's `&__v` must produce
	// a `*<Mirror>` pointer too. Using the BAML type here was the
	// source of "cannot use &__v ... as *<Mirror>" errors during
	// matrix bring-up.
	var fields []jen.Code
	for _, smp := range smps {
		fields = append(fields, jen.Id(upperCamelForMatrix(smp.paramName)).Add(mirrorFieldType(smp.paramType, smp.mirrorName)))
	}
	out.Type().Id(me.inputStructName).Struct(fields...)
	return me
}

// upperCamelForMatrix mirrors the strcase.UpperCamelCase the codegen
// uses for input-struct field names. Inlined to avoid an extra dep
// here.
func upperCamelForMatrix(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// writeTempModule lays out a self-contained Go module under `dir`
// that compiles the rendered file against bamlutils + the fixtures
// package via replace directives. The temp module reuses
// adapters/common's go.mod + go.sum verbatim — that module already
// pins every transitive dep bamlutils needs — and rewrites the
// module path / replace directives to point at the local sources.
// Without inheriting the transitive-dep pins `go build` would
// complain "updates to go.mod needed" and refuse to proceed without
// network access.
func writeTempModule(t *testing.T, dir, rendered string) {
	t.Helper()

	bamlutilsAbs := absRepoPath(t, "bamlutils")
	introspectedAbs := absRepoPath(t, "introspected")
	fixturesAbs := absRepoPath(t, "adapters/common/codegen/internal/fixtures")
	commonAbs := absRepoPath(t, "adapters/common")

	_ = fixturesAbs // fixtures is a sub-package of adapters/common; the adapters/common replace below covers it

	commonModBytes, err := os.ReadFile(filepath.Join(commonAbs, "go.mod"))
	if err != nil {
		t.Fatalf("read adapters/common go.mod: %v", err)
	}
	commonMod := string(commonModBytes)

	// Rewrite the module path so the temp module is a standalone
	// build target (`matrixtest`). Replace the original
	// first-party replaces (which used `../../...` relative paths)
	// with absolute paths so the temp dir's CWD doesn't matter.
	// Declare the temp module under the adapters/common/codegen
	// import-path prefix so the matrix file can import
	// `.../codegen/internal/fixtures` — Go's `internal/`
	// visibility rule allows that from any package whose path is
	// rooted at the same parent (`adapters/common/codegen/`).
	// Any module path outside that prefix would be rejected as a
	// cross-tree internal import.
	rewritten := strings.Replace(
		commonMod,
		"module github.com/invakid404/baml-rest/adapters/common",
		"module github.com/invakid404/baml-rest/adapters/common/codegen/matrixtest",
		1,
	)
	rewritten = strings.Replace(
		rewritten,
		"github.com/invakid404/baml-rest/bamlutils => ../../bamlutils",
		"github.com/invakid404/baml-rest/bamlutils => "+bamlutilsAbs,
		1,
	)
	rewritten = strings.Replace(
		rewritten,
		"github.com/invakid404/baml-rest/introspected => ../../introspected",
		"github.com/invakid404/baml-rest/introspected => "+introspectedAbs,
		1,
	)
	// Add a require + replace for the adapters/common module so the
	// rendered file's fixtures import (a sub-package of
	// adapters/common) resolves to the local source tree.
	rewritten += "\nrequire github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000\n"
	rewritten += "\nreplace github.com/invakid404/baml-rest/adapters/common => " + commonAbs + "\n"
	writeFile(t, filepath.Join(dir, "go.mod"), rewritten)

	// Reuse adapters/common's go.sum verbatim so transitive
	// dep hashes don't trip the "module verification" gate.
	commonSum, err := os.ReadFile(filepath.Join(commonAbs, "go.sum"))
	if err != nil {
		t.Fatalf("read adapters/common go.sum: %v", err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(commonSum))

	writeFile(t, filepath.Join(dir, "matrix.go"), rendered)
}

// runGoBuild shells out to `go build ./...` in `dir`. The matrix is
// rendered into a single file, so a single build invocation
// type-checks every cell in one shot. Any failure (undeclared
// identifier, arg-count mismatch, type mismatch) surfaces as a
// build error.
func runGoBuild(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("matrix temp module failed to compile (the rendered codegen output type-checks against bamlutils + fixtures):\n%s\nerror: %v", out, err)
	}
}

// parseRendered re-parses the rendered file in-process so the AST
// scan in `assertNoReleaseConvertedInNonPooledCell` can look at the
// emitter's exact textual output rather than reading the temp file
// from disk.
func parseRendered(t *testing.T, rendered string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "matrix.go", rendered, parser.AllErrors)
	if err != nil {
		t.Fatalf("rendered matrix file fails to parse: %v\n--- rendered ---\n%s", err, rendered)
	}
	return f, fset
}

// assertNoReleaseConvertedInNonPooledCell complements `go build` by
// catching the failure mode where a cell that should NOT emit
// `__releaseConverted` does so anyway. The compiler would accept an
// unused closure; only an AST scan catches the over-emit.
//
// We iterate per-cell, find the cell's dispatch function in the
// parsed AST, and assert the presence or absence of `__releaseConverted`
// against the cell's expectReleaseConverted flag.
func assertNoReleaseConvertedInNonPooledCell(t *testing.T, parsed *ast.File, fset *token.FileSet, cells []matrixCell) {
	t.Helper()

	funcByName := map[string]*ast.FuncDecl{}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		funcByName[fd.Name.Name] = fd
	}

	// Sort cells for deterministic iteration order — test output
	// orders by cell index so failures point at a stable cell.
	sorted := append([]matrixCell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	for _, cell := range sorted {
		fd, ok := funcByName[cell.dispatchFunc]
		if !ok {
			t.Errorf("cell %q: dispatch function %q missing from rendered AST", cell.name, cell.dispatchFunc)
			continue
		}
		hasRelease := dispatchHasReleaseConverted(fd)
		switch {
		case cell.expectReleaseConverted && !hasRelease:
			t.Errorf("cell %q: expected __releaseConverted closure in %s (a pooled param or ownedNested-needing converter is present) but the dispatch did not emit one",
				cell.name, cell.dispatchFunc)
		case !cell.expectReleaseConverted && hasRelease:
			t.Errorf("cell %q: dispatch %s emitted __releaseConverted but the cell has no pooled resources and no converter that takes ownedNested",
				cell.name, cell.dispatchFunc)
		}
	}
}

// dispatchHasReleaseConverted returns true if the function body
// contains a `__releaseConverted := func() { ... }` assignment.
func dispatchHasReleaseConverted(fd *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fd, func(n ast.Node) bool {
		if found {
			return false
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if id.Name == "__releaseConverted" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// absRepoPath returns the absolute path to a repo-relative directory.
// Used to wire the temp module's go.mod replaces against the real
// bamlutils + fixtures source trees.
func absRepoPath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test file path via runtime.Caller")
	}
	// Walk up from adapters/common/codegen/<this file> to the repo
	// root, then descend into the requested rel path.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(repoRoot, rel)
}

// writeFile is a t.Helper wrapper that fails the test on write error
// rather than ignoring it.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

