package hacks

import (
	"go/ast"
	"go/token"
	"strings"
)

// ExprReplacer is a function that examines an expression and returns a
// replacement expression, or nil to keep the original unchanged.
type ExprReplacer func(ast.Expr) ast.Expr

// ReplaceExprsInNode walks the AST node and replaces expressions using the replacer.
func ReplaceExprsInNode(node ast.Node, replacer ExprReplacer) {
	switch n := node.(type) {
	case *ast.BlockStmt:
		for i, stmt := range n.List {
			n.List[i] = ReplaceExprsInStmt(stmt, replacer)
		}
	case *ast.FuncDecl:
		if n.Body != nil {
			ReplaceExprsInNode(n.Body, replacer)
		}
	}
}

// ReplaceExprsInStmt replaces expressions within a statement using the replacer.
func ReplaceExprsInStmt(stmt ast.Stmt, replacer ExprReplacer) ast.Stmt {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		s.X = ReplaceExprsInExpr(s.X, replacer)
	case *ast.AssignStmt:
		for i, expr := range s.Rhs {
			s.Rhs[i] = ReplaceExprsInExpr(expr, replacer)
		}
		for i, expr := range s.Lhs {
			s.Lhs[i] = ReplaceExprsInExpr(expr, replacer)
		}
	case *ast.ReturnStmt:
		for i, expr := range s.Results {
			s.Results[i] = ReplaceExprsInExpr(expr, replacer)
		}
	case *ast.IfStmt:
		if s.Init != nil {
			s.Init = ReplaceExprsInStmt(s.Init, replacer)
		}
		s.Cond = ReplaceExprsInExpr(s.Cond, replacer)
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
		if s.Else != nil {
			s.Else = ReplaceExprsInStmt(s.Else, replacer)
		}
	case *ast.ForStmt:
		if s.Init != nil {
			s.Init = ReplaceExprsInStmt(s.Init, replacer)
		}
		if s.Cond != nil {
			s.Cond = ReplaceExprsInExpr(s.Cond, replacer)
		}
		if s.Post != nil {
			s.Post = ReplaceExprsInStmt(s.Post, replacer)
		}
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
	case *ast.RangeStmt:
		s.X = ReplaceExprsInExpr(s.X, replacer)
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
	case *ast.BlockStmt:
		ReplaceExprsInNode(s, replacer)
	case *ast.DeclStmt:
		if genDecl, ok := s.Decl.(*ast.GenDecl); ok {
			for _, spec := range genDecl.Specs {
				if valueSpec, ok := spec.(*ast.ValueSpec); ok {
					for i, val := range valueSpec.Values {
						valueSpec.Values[i] = ReplaceExprsInExpr(val, replacer)
					}
				}
			}
		}
	case *ast.DeferStmt:
		if replaced := ReplaceExprsInExpr(s.Call, replacer); replaced != nil {
			if call, ok := replaced.(*ast.CallExpr); ok {
				s.Call = call
			}
		}
	case *ast.GoStmt:
		if replaced := ReplaceExprsInExpr(s.Call, replacer); replaced != nil {
			if call, ok := replaced.(*ast.CallExpr); ok {
				s.Call = call
			}
		}
	case *ast.SwitchStmt:
		if s.Init != nil {
			s.Init = ReplaceExprsInStmt(s.Init, replacer)
		}
		if s.Tag != nil {
			s.Tag = ReplaceExprsInExpr(s.Tag, replacer)
		}
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
	case *ast.TypeSwitchStmt:
		if s.Init != nil {
			s.Init = ReplaceExprsInStmt(s.Init, replacer)
		}
		if s.Assign != nil {
			s.Assign = ReplaceExprsInStmt(s.Assign, replacer)
		}
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
	case *ast.SelectStmt:
		if s.Body != nil {
			ReplaceExprsInNode(s.Body, replacer)
		}
	case *ast.CaseClause:
		for i, expr := range s.List {
			s.List[i] = ReplaceExprsInExpr(expr, replacer)
		}
		for i, stmt := range s.Body {
			s.Body[i] = ReplaceExprsInStmt(stmt, replacer)
		}
	case *ast.CommClause:
		if s.Comm != nil {
			s.Comm = ReplaceExprsInStmt(s.Comm, replacer)
		}
		for i, stmt := range s.Body {
			s.Body[i] = ReplaceExprsInStmt(stmt, replacer)
		}
	case *ast.SendStmt:
		s.Chan = ReplaceExprsInExpr(s.Chan, replacer)
		s.Value = ReplaceExprsInExpr(s.Value, replacer)
	}
	return stmt
}

// ReplaceExprsInExpr recursively replaces expressions using the replacer.
func ReplaceExprsInExpr(expr ast.Expr, replacer ExprReplacer) ast.Expr {
	if expr == nil {
		return nil
	}

	// First check if this expression should be replaced
	if replacement := replacer(expr); replacement != nil {
		return replacement
	}

	// Otherwise recurse into sub-expressions
	switch e := expr.(type) {
	case *ast.CallExpr:
		for i, arg := range e.Args {
			e.Args[i] = ReplaceExprsInExpr(arg, replacer)
		}
		e.Fun = ReplaceExprsInExpr(e.Fun, replacer)
	case *ast.BinaryExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
		e.Y = ReplaceExprsInExpr(e.Y, replacer)
	case *ast.UnaryExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
	case *ast.ParenExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
	case *ast.SelectorExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
	case *ast.IndexExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
		e.Index = ReplaceExprsInExpr(e.Index, replacer)
	case *ast.SliceExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
		if e.Low != nil {
			e.Low = ReplaceExprsInExpr(e.Low, replacer)
		}
		if e.High != nil {
			e.High = ReplaceExprsInExpr(e.High, replacer)
		}
		if e.Max != nil {
			e.Max = ReplaceExprsInExpr(e.Max, replacer)
		}
	case *ast.TypeAssertExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
	case *ast.StarExpr:
		e.X = ReplaceExprsInExpr(e.X, replacer)
	case *ast.CompositeLit:
		for i, elt := range e.Elts {
			e.Elts[i] = ReplaceExprsInExpr(elt, replacer)
		}
	case *ast.KeyValueExpr:
		e.Key = ReplaceExprsInExpr(e.Key, replacer)
		e.Value = ReplaceExprsInExpr(e.Value, replacer)
	case *ast.FuncLit:
		if e.Body != nil {
			ReplaceExprsInNode(e.Body, replacer)
		}
	}

	return expr
}

// EnsureImport adds an import to the file if it's not already present.
func EnsureImport(file *ast.File, importPath string) {
	// Check if already imported
	for _, imp := range file.Imports {
		if imp.Path != nil && strings.Trim(imp.Path.Value, `"`) == importPath {
			return
		}
	}

	// Create import spec
	importSpec := &ast.ImportSpec{
		Path: &ast.BasicLit{
			Kind:  token.STRING,
			Value: `"` + importPath + `"`,
		},
	}

	// Find or create import declaration
	var importDecl *ast.GenDecl
	for _, decl := range file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			importDecl = genDecl
			break
		}
	}

	if importDecl == nil {
		// Create new import declaration
		importDecl = &ast.GenDecl{
			Tok:   token.IMPORT,
			Specs: []ast.Spec{importSpec},
		}
		// Prepend to declarations
		file.Decls = append([]ast.Decl{importDecl}, file.Decls...)
	} else {
		// Add to existing import declaration
		importDecl.Specs = append(importDecl.Specs, importSpec)
	}
}

// IsCallTo checks if a call expression is a call to pkg.name() with no arguments.
func IsCallTo(call *ast.CallExpr, pkg, name string) bool {
	selectorExpr, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	pkgIdent, ok := selectorExpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	return pkgIdent.Name == pkg && selectorExpr.Sel.Name == name && len(call.Args) == 0
}

// IsSelectorType checks if a type expression is pkg.Name (e.g., context.Context).
func IsSelectorType(expr ast.Expr, pkg, name string) bool {
	selectorExpr, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	pkgIdent, ok := selectorExpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	return pkgIdent.Name == pkg && selectorExpr.Sel.Name == name
}

// HasFirstParamOfType checks if the function's first parameter is of the given type (pkg.Name).
func HasFirstParamOfType(funcDecl *ast.FuncDecl, pkg, name string) bool {
	if funcDecl.Type.Params == nil || len(funcDecl.Type.Params.List) == 0 {
		return false
	}

	return IsSelectorType(funcDecl.Type.Params.List[0].Type, pkg, name)
}

// PrependParam adds a new parameter as the first parameter of a function.
func PrependParam(funcDecl *ast.FuncDecl, paramName, pkgName, typeName string) {
	param := &ast.Field{
		Names: []*ast.Ident{{Name: paramName}},
		Type: &ast.SelectorExpr{
			X:   &ast.Ident{Name: pkgName},
			Sel: &ast.Ident{Name: typeName},
		},
	}

	if funcDecl.Type.Params == nil {
		funcDecl.Type.Params = &ast.FieldList{}
	}

	// Prepend parameter
	funcDecl.Type.Params.List = append([]*ast.Field{param}, funcDecl.Type.Params.List...)
}

// IsPointerReceiverOfType checks if a function has a pointer receiver of the given type name.
func IsPointerReceiverOfType(recv *ast.FieldList, typeName string) bool {
	if recv == nil || len(recv.List) != 1 {
		return false
	}

	starExpr, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}

	ident, ok := starExpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	return ident.Name == typeName
}

// IsPointerReceiverOfTypes checks if a function has a pointer receiver of any of the given type names.
func IsPointerReceiverOfTypes(recv *ast.FieldList, typeNames ...string) bool {
	for _, name := range typeNames {
		if IsPointerReceiverOfType(recv, name) {
			return true
		}
	}
	return false
}
