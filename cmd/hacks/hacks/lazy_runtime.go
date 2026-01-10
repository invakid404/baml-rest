package hacks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
)

func init() {
	Register(&LazyRuntimeHack{})
}

// LazyRuntimeHack converts BAML's eager runtime initialization to lazy initialization.
//
// The issue: The generated client's init() function calls baml.CreateRuntime()
// which loads the native shared library immediately when the package is imported.
// This is problematic for applications that want to defer loading to worker processes.
//
// The fix: Rename init() to InitRuntime() and wrap with sync.Once, so the runtime
// is only loaded when explicitly requested.
type LazyRuntimeHack struct{}

func (h *LazyRuntimeHack) Name() string {
	return "lazy-runtime"
}

func (h *LazyRuntimeHack) MinVersion() string {
	return "" // Applies to all versions
}

func (h *LazyRuntimeHack) MaxVersion() string {
	return "" // Until BAML provides native lazy loading
}

func (h *LazyRuntimeHack) Apply(bamlClientDir string) error {
	runtimePath := filepath.Join(bamlClientDir, "runtime.go")

	if _, err := os.Stat(runtimePath); os.IsNotExist(err) {
		return fmt.Errorf("runtime.go not found in %s", bamlClientDir)
	}

	modified, err := h.processRuntimeFile(runtimePath)
	if err != nil {
		return fmt.Errorf("processing runtime.go: %w", err)
	}

	if modified {
		fmt.Printf("  Modified: %s\n", runtimePath)
	}

	return nil
}

func (h *LazyRuntimeHack) processRuntimeFile(path string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("parsing file: %w", err)
	}

	// Find the init function that contains CreateRuntime
	var initFuncDecl *ast.FuncDecl
	var initFuncIndex int = -1

	for i, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv != nil {
			continue
		}
		if funcDecl.Name.Name == "init" && h.initContainsCreateRuntime(funcDecl) {
			initFuncDecl = funcDecl
			initFuncIndex = i
			break
		}
	}

	if initFuncDecl == nil {
		// No init with CreateRuntime found, nothing to do
		return false, nil
	}

	// Add sync import
	EnsureImport(file, "sync")

	// Add the sync.Once variable declaration after imports
	onceVarDecl := &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{
				Names: []*ast.Ident{{Name: "initRuntimeOnce"}},
				Type: &ast.SelectorExpr{
					X:   &ast.Ident{Name: "sync"},
					Sel: &ast.Ident{Name: "Once"},
				},
			},
		},
	}

	// Rename init to initRuntime (unexported, called by InitRuntime)
	initFuncDecl.Name.Name = "initRuntime"

	// Create the exported InitRuntime function that uses sync.Once
	initRuntimeFunc := &ast.FuncDecl{
		Name: &ast.Ident{Name: "InitRuntime"},
		Type: &ast.FuncType{
			Params:  &ast.FieldList{},
			Results: nil,
		},
		Body: &ast.BlockStmt{
			List: []ast.Stmt{
				// initRuntimeOnce.Do(initRuntime)
				&ast.ExprStmt{
					X: &ast.CallExpr{
						Fun: &ast.SelectorExpr{
							X:   &ast.Ident{Name: "initRuntimeOnce"},
							Sel: &ast.Ident{Name: "Do"},
						},
						Args: []ast.Expr{
							&ast.Ident{Name: "initRuntime"},
						},
					},
				},
			},
		},
	}

	// Insert the once variable and InitRuntime function
	// Insert onceVarDecl right after imports, and InitRuntime after the renamed init
	newDecls := make([]ast.Decl, 0, len(file.Decls)+2)

	// Find where imports end
	importEndIdx := 0
	for i, decl := range file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			importEndIdx = i + 1
		}
	}

	// Build new declarations list
	for i, decl := range file.Decls {
		newDecls = append(newDecls, decl)
		// Insert onceVarDecl after imports
		if i == importEndIdx-1 {
			newDecls = append(newDecls, onceVarDecl)
		}
		// Insert InitRuntime after the renamed initRuntime
		if i == initFuncIndex {
			newDecls = append(newDecls, initRuntimeFunc)
		}
	}

	// Handle edge case where there are no imports
	if importEndIdx == 0 {
		newDecls = append([]ast.Decl{onceVarDecl}, newDecls...)
	}

	file.Decls = newDecls

	// Write modified file back
	out, err := os.Create(path)
	if err != nil {
		return false, fmt.Errorf("creating output file: %w", err)
	}
	defer out.Close()

	if err := printer.Fprint(out, fset, file); err != nil {
		return false, fmt.Errorf("writing file: %w", err)
	}

	return true, nil
}

func (h *LazyRuntimeHack) initContainsCreateRuntime(funcDecl *ast.FuncDecl) bool {
	if funcDecl.Body == nil {
		return false
	}

	found := false
	ast.Inspect(funcDecl.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if IsCallToAny(call, "baml", "CreateRuntime") {
				found = true
				return false
			}
		}
		return true
	})

	return found
}

// IsCallToAny checks if a call expression is a call to pkg.name() (with any number of arguments).
func IsCallToAny(call *ast.CallExpr, pkg, name string) bool {
	selectorExpr, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	pkgIdent, ok := selectorExpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	return pkgIdent.Name == pkg && selectorExpr.Sel.Name == name
}
