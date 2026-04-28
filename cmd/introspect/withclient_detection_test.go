package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDetectsWithClientFunction pins the AST-walk detection that
// generateFull uses to populate introspected.SupportsWithClient.
// Hardcoding SupportsWithClient=true would silently emit
// WithClient(clientOverride) against pre-0.219 baml_client packages
// that lack the symbol; the detection lives in the same AST walk
// that finds Request / StreamRequest. This test pins that
// file.Scope.Lookup("WithClient") returns a *ast.Object with
// Kind==ast.Fun for runtime.go shapes matching BAML v0.219.0+, and
// nil for runtime.go shapes from older runtimes.
func TestDetectsWithClientFunction(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// supportsWithClient is the expected detector outcome —
		// matches the supportsWithClient bool that generateFull's
		// AST walk computes.
		supportsWithClient bool
	}{
		{
			name: "BAML v0.219.0+: top-level WithClient(string) CallOption present",
			src: `package baml_client

type CallOptionFunc func(*callOption)
type callOption struct{ client *string }

// WithClient is the exported override added in BAML v0.219.0.
func WithClient(client string) CallOptionFunc {
	return func(o *callOption) { o.client = &client }
}
`,
			supportsWithClient: true,
		},
		{
			name: "BAML pre-0.219: no WithClient in baml_client",
			src: `package baml_client

type CallOptionFunc func(*callOption)
type callOption struct{}

// Other CallOption funcs exist, but no WithClient yet.
func WithTypeBuilder(tb any) CallOptionFunc {
	return func(o *callOption) {}
}
`,
			supportsWithClient: false,
		},
		{
			name: "method with WithClient name on a receiver does NOT match — only top-level free funcs",
			src: `package baml_client

type Builder struct{}

// Method receiver disqualifies; the AST walk wants the free
// function form the codegen actually calls.
func (b Builder) WithClient(name string) {}
`,
			// The detector uses Scope.Lookup which only finds top-
			// level identifiers; receiver methods aren't in package
			// scope so this stays false.
			supportsWithClient: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "runtime.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			obj := file.Scope.Lookup("WithClient")
			detected := obj != nil && obj.Kind == ast.Fun
			if detected != tc.supportsWithClient {
				t.Errorf("WithClient detection: got %v, want %v (lookup obj=%v)", detected, tc.supportsWithClient, obj)
			}
		})
	}
}
