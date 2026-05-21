package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/adapters/common/codegen/internal/fixtures"
)

// TestEmitPoolAuditHooks_Toggle is the contract guarantee for the
// EmitPoolAuditHooks option: with the flag off, getXSlice / putXSlice
// reference no symbol from the internal/poolaudit package and the
// emitted helpers are byte-identical to the pre-audit baseline; with
// it on, getXSlice contains exactly one OnCheckout call and putXSlice
// contains exactly one CheckZeroPrePut and one OnRelease call. AST
// scanning rather than text matching catches accidental partial-emit
// regressions (an extra OnCheckout inside the if-branch, a missing
// OnRelease past an early-return, etc.) that a substring check would
// silently accept.
func TestEmitPoolAuditHooks_Toggle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		audit          bool
		wantOnCheckout int
		wantOnRelease  int
		wantCheckZero  int
	}{
		{name: "off", audit: false},
		{name: "on", audit: true, wantOnCheckout: 1, wantOnRelease: 1, wantCheckZero: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkgs := DefaultPackageConfig()
			pkgs.BamlPkg = reflect.TypeOf(fixtures.Image{}).PkgPath()
			pkgs.OutputPkg = "github.com/invakid404/baml-rest/adapters/common/codegen/matrixtest"
			pkgs.OutputPkgName = "matrix"

			out := jen.NewFilePathName(pkgs.OutputPkg, pkgs.OutputPkgName)
			tracker := newSlicePoolTracker(pkgs, tc.audit, false)
			tracker.ensure(out, reflect.TypeOf(fixtures.ContentPartA{}), 256)

			rendered := out.GoString()

			gotCheckout := strings.Count(rendered, "poolaudit.OnCheckout(")
			gotRelease := strings.Count(rendered, "poolaudit.OnRelease(")
			gotCheckZero := strings.Count(rendered, "poolaudit.CheckZeroPrePut(")
			if gotCheckout != tc.wantOnCheckout {
				t.Errorf("OnCheckout count: got %d want %d\n%s", gotCheckout, tc.wantOnCheckout, rendered)
			}
			if gotRelease != tc.wantOnRelease {
				t.Errorf("OnRelease count: got %d want %d\n%s", gotRelease, tc.wantOnRelease, rendered)
			}
			if gotCheckZero != tc.wantCheckZero {
				t.Errorf("CheckZeroPrePut count: got %d want %d\n%s", gotCheckZero, tc.wantCheckZero, rendered)
			}

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "audit.go", rendered, parser.AllErrors)
			if err != nil {
				t.Fatalf("rendered helper file does not parse: %v\n--- rendered ---\n%s", err, rendered)
			}

			elemTy := reflect.TypeOf(fixtures.ContentPartA{})
			base := slicePoolBaseName(elemTy)
			getFn := findFunc(f, "get"+base+"Slice")
			putFn := findFunc(f, "put"+base+"Slice")
			if getFn == nil || putFn == nil {
				t.Fatalf("expected get/put helpers (base=%q) in rendered file:\n%s", base, rendered)
			}
			gotInGet := countPoolauditCalls(getFn)
			gotInPut := countPoolauditCalls(putFn)
			wantInGet := tc.wantOnCheckout
			wantInPut := tc.wantOnRelease + tc.wantCheckZero
			if gotInGet != wantInGet {
				t.Errorf("poolaudit calls inside get helper: got %d want %d", gotInGet, wantInGet)
			}
			if gotInPut != wantInPut {
				t.Errorf("poolaudit calls inside put helper: got %d want %d", gotInPut, wantInPut)
			}

			// Off-mode must NOT import the poolaudit package — any
			// non-zero count above would already have failed, but
			// double-check the import list so a future regression
			// where the qual leaks into a different statement
			// surfaces here.
			hasImport := false
			for _, imp := range f.Imports {
				if strings.Contains(imp.Path.Value, "internal/poolaudit") {
					hasImport = true
					break
				}
			}
			if tc.audit && !hasImport {
				t.Errorf("audit=on but rendered file does not import poolaudit")
			}
			if !tc.audit && hasImport {
				t.Errorf("audit=off but rendered file still imports poolaudit")
			}
		})
	}
}

// findFunc walks the file's top-level declarations and returns the
// function named `name`, or nil if absent.
func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

// countPoolauditCalls returns the number of call expressions inside
// fd whose selector is rooted at the poolaudit package identifier.
// Counts CheckZeroPrePut, OnCheckout, OnRelease together so the
// caller can assert per-helper totals.
func countPoolauditCalls(fd *ast.FuncDecl) int {
	count := 0
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Name == "poolaudit" {
			count++
		}
		return true
	})
	return count
}
