package main

import (
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"
)

func TestMethodsDict(t *testing.T) {
	tests := []struct {
		name     string
		methods  []map[string]any
		wantKeys []string
		wantArgs map[string][]string
	}{
		{
			name:     "empty methods",
			methods:  []map[string]any{},
			wantKeys: []string{},
			wantArgs: map[string][]string{},
		},
		{
			name: "single method no args",
			methods: []map[string]any{
				{"name": "GetUser", "args": []string{}},
			},
			wantKeys: []string{"GetUser"},
			wantArgs: map[string][]string{"GetUser": {}},
		},
		{
			name: "single method with args",
			methods: []map[string]any{
				{"name": "CreateUser", "args": []string{"name", "email"}},
			},
			wantKeys: []string{"CreateUser"},
			wantArgs: map[string][]string{"CreateUser": {"name", "email"}},
		},
		{
			name: "multiple methods",
			methods: []map[string]any{
				{"name": "GetUser", "args": []string{"id"}},
				{"name": "CreateUser", "args": []string{"name", "email"}},
				{"name": "DeleteUser", "args": []string{"id"}},
			},
			wantKeys: []string{"GetUser", "CreateUser", "DeleteUser"},
			wantArgs: map[string][]string{
				"GetUser":    {"id"},
				"CreateUser": {"name", "email"},
				"DeleteUser": {"id"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := methodsDict(tt.methods)

			// Check count
			if len(code) != len(tt.wantKeys) {
				t.Errorf("methodsDict() returned %d entries, want %d", len(code), len(tt.wantKeys))
			}

			// Render and verify output
			f := jen.NewFile("test")
			f.Var().Id("Methods").Op("=").Map(jen.String()).Index().String().Values(code...)
			output := f.GoString()

			// Check each expected key and args
			for _, key := range tt.wantKeys {
				if !strings.Contains(output, `"`+key+`"`) {
					t.Errorf("methodsDict() missing key %q", key)
				}
			}

			for key, args := range tt.wantArgs {
				for _, arg := range args {
					// The arg should appear as a string literal
					if !strings.Contains(output, `"`+arg+`"`) {
						t.Errorf("methodsDict() missing arg %q for key %q", arg, key)
					}
				}
			}
		})
	}
}

func TestMethodsDict_OutputFormat(t *testing.T) {
	methods := []map[string]any{
		{"name": "TestMethod", "args": []string{"arg1", "arg2"}},
	}

	code := methodsDict(methods)
	f := jen.NewFile("test")
	f.Var().Id("Methods").Op("=").Map(jen.String()).Index().String().Values(code...)
	output := f.GoString()

	// Should produce valid Go map syntax
	// Expected pattern: "TestMethod": []string{"arg1", "arg2"}
	if !strings.Contains(output, `"TestMethod": []string{"arg1", "arg2"}`) {
		t.Errorf("methodsDict() output format incorrect.\nGot:\n%s", output)
	}
}
