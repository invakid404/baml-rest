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
	Register(&ContextFixHack{})
}

// ContextFixHack fixes the goroutine leak in BAML's generated client.
//
// The issue: parse, parse_stream, build_request, and build_request_stream
// methods pass context.Background() to BAML runtime functions (CallFunctionParse,
// BuildRequest, etc.), which spawn goroutines that wait for context cancellation.
// Since context.Background() never cancels, goroutines leak.
//
// The fix: Add a context.Context parameter to these methods and pass it
// instead of context.Background().
type ContextFixHack struct{}

func (h *ContextFixHack) Name() string {
	return "context-fix"
}

func (h *ContextFixHack) MinVersion() string {
	return "" // Applies to all versions
}

func (h *ContextFixHack) MaxVersion() string {
	return "" // Until BAML fixes this upstream
}

func (h *ContextFixHack) Apply(bamlClientDir string) error {
	return filepath.WalkDir(bamlClientDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		modified, err := h.processFile(path)
		if err != nil {
			return fmt.Errorf("processing %s: %w", path, err)
		}

		if modified {
			fmt.Printf("  Modified: %s\n", path)
		}

		return nil
	})
}

func (h *ContextFixHack) processFile(path string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("parsing file: %w", err)
	}

	modified := false

	// Find all methods with receivers that use context.Background()
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv == nil {
			continue
		}

		// Check receiver type
		if !IsPointerReceiverOfTypes(funcDecl.Recv, "parse", "parse_stream", "build_request", "build_request_stream") {
			continue
		}

		// Check if context.Context is already the first parameter
		if HasFirstParamOfType(funcDecl, "context", "Context") {
			continue
		}

		// Add context.Context as first parameter
		PrependParam(funcDecl, "ctx", "context", "Context")

		// Replace all context.Background() calls with ctx
		replacer := func(expr ast.Expr) ast.Expr {
			call, ok := expr.(*ast.CallExpr)
			if !ok {
				return nil
			}
			if IsCallTo(call, "context", "Background") {
				return &ast.Ident{Name: "ctx"}
			}
			return nil
		}
		ReplaceExprsInNode(funcDecl.Body, replacer)

		modified = true
	}

	if !modified {
		return false, nil
	}

	// Ensure context import exists
	EnsureImport(file, "context")

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
