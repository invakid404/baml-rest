package hacks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(&UnusedDecodeVarHack{})
}

// UnusedDecodeVarHack neutralises a BAML codegen bug that emits a
// short variable declaration the generated body never reads, which Go
// rejects at compile time with "declared and not used".
//
// The observed shape (every BAML version checked, 0.214 through 0.222)
// is a class `Decode` method whose field loop always declares
// `valueHolder := field.Value`:
//
//	for _, field := range holder.Fields {
//		key := field.Key
//		valueHolder := field.Value
//		switch key {
//		case "a":
//			c.A = (*interface{})(nil)
//		}
//	}
//
// When every field of the class is a BAML `null` (so each case decodes
// to `(*interface{})(nil)` without touching `valueHolder`), the
// declaration is unused and `go build` fails. The fuzzer reaches this
// shape whenever it draws a class all of whose fields are `null`.
//
// The fix scans generated `Decode` methods and, for any `:=`-declared
// local that is never referenced within its own scope, splices a
// `_ = <name>` blank assignment immediately after the declaration. The
// rewrite is a no-op when the variable is used (the reference count
// guard skips it), so it is safe to run against every version and is
// idempotent: the blank assignment itself counts as a use on re-runs.
type UnusedDecodeVarHack struct{}

func (h *UnusedDecodeVarHack) Name() string {
	return "unused-decode-var-fix"
}

func (h *UnusedDecodeVarHack) MinVersion() string {
	return ""
}

func (h *UnusedDecodeVarHack) MaxVersion() string {
	return ""
}

func (h *UnusedDecodeVarHack) Apply(bamlClientDir string) error {
	return filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		modified, applyErr := h.processFile(path)
		if applyErr != nil {
			return fmt.Errorf("processing %s: %w", path, applyErr)
		}
		if modified {
			fmt.Printf("  Modified (unused-decode-var): %s\n", path)
		}
		return nil
	})
}

func (h *UnusedDecodeVarHack) processFile(path string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("parsing file: %w", err)
	}

	modified := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		// Restrict to generated decode entry points so the blank-
		// assignment rewrite never touches an unrelated method whose
		// "unused" local is really a future-use the generator left for
		// a later pass. Class, union, and stream decoders are all named
		// Decode with a pointer receiver.
		if fn.Name == nil || fn.Name.Name != "Decode" {
			continue
		}
		if blankUnusedShortVarDecls(fn.Body) {
			modified = true
		}
	}

	if !modified {
		return false, nil
	}

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

// blankUnusedShortVarDecls walks every block in body and, for each
// `:=` short variable declaration whose declared name is never
// referenced within the remainder of the block (its scope), inserts a
// `_ = <name>` statement right after the declaration. Returns true when
// any block was rewritten.
//
// Scope handling: Go scopes a short-var-decl from its declaration to
// the end of the enclosing block, so usage is counted over the
// statements following the declaration within the same block (nested
// blocks included). References in earlier statements cannot belong to
// this declaration (use-before-declare is invalid Go), so they are
// ignored.
func blankUnusedShortVarDecls(body *ast.BlockStmt) bool {
	changed := false
	ast.Inspect(body, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		var rebuilt []ast.Stmt
		blockChanged := false
		for i, stmt := range block.List {
			rebuilt = append(rebuilt, stmt)
			assign, ok := stmt.(*ast.AssignStmt)
			if !ok || assign.Tok != token.DEFINE {
				continue
			}
			rest := block.List[i+1:]
			for _, lhs := range assign.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				if identUsedInStmts(rest, ident.Name) {
					continue
				}
				rebuilt = append(rebuilt, blankAssign(ident.Name))
				blockChanged = true
			}
		}
		if blockChanged {
			block.List = rebuilt
			changed = true
		}
		return true
	})
	return changed
}

// identUsedInStmts reports whether name appears as an identifier
// anywhere in stmts (including nested nodes).
func identUsedInStmts(stmts []ast.Stmt, name string) bool {
	used := false
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				used = true
				return false
			}
			return !used
		})
		if used {
			return true
		}
	}
	return false
}

// blankAssign builds the statement `_ = <name>`.
func blankAssign(name string) ast.Stmt {
	return &ast.AssignStmt{
		Lhs: []ast.Expr{&ast.Ident{Name: "_"}},
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{&ast.Ident{Name: name}},
	}
}
