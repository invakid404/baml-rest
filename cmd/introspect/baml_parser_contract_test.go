package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
)

// TestStrategyParserContract_RuntimeAndIntrospectOverlap pins that the
// runtime BAML strategy parser (buildrequest.ParseRuntimeStrategyStringForTest)
// and the introspect production walker agree on the parsed chain for the
// same logical strategy list across the whitespace / separator variants
// the existing #265 corpus exercises.
//
// The introspect side feeds the strategy list into a wrapped
// client<llm> ... options { ... } block parsed through bamlparser +
// processBAMLFile (the live production pipeline) and reads the captured
// chain from cfg.fallbackChains. Pre-#265 PR 3 this test compared the
// deleted parseStrategyList helper directly; the production-walker
// comparison preserves the runtime/introspect overlap contract without
// reaching into helper internals that no longer exist.
func TestStrategyParserContract_RuntimeAndIntrospectOverlap(t *testing.T) {
	tests := []struct {
		name string
		// introspectInput is the strategy expression as it appears
		// inside an options { } block. The test prepends `strategy `
		// when missing so list-only variants ("[A B]") still feed the
		// walker as `strategy [A B]`.
		introspectInput string
		runtimeInput    string
		want            []string
	}{
		{
			name:            "comma separated with prefix",
			introspectInput: "strategy [ClientA, ClientB, ClientC]",
			runtimeInput:    "strategy [ClientA, ClientB, ClientC]",
			want:            []string{"ClientA", "ClientB", "ClientC"},
		},
		{
			name:            "space separated without runtime prefix",
			introspectInput: "[ClientA ClientB]",
			runtimeInput:    "[ClientA ClientB]",
			want:            []string{"ClientA", "ClientB"},
		},
		{
			name:            "newline and comma variants",
			introspectInput: "strategy [\nClientA,\nClientB,\nClientC\n]",
			runtimeInput:    "strategy [\nClientA,\nClientB,\nClientC\n]",
			want:            []string{"ClientA", "ClientB", "ClientC"},
		},
		{
			name:            "mixed whitespace and optional runtime prefix",
			introspectInput: "strategy [ ClientA,\n\tClientB ClientC ]",
			runtimeInput:    "[ ClientA,\n\tClientB ClientC ]",
			want:            []string{"ClientA", "ClientB", "ClientC"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := tt.introspectInput
			if !strings.HasPrefix(strings.TrimSpace(expr), "strategy") {
				expr = "strategy " + expr
			}
			src := fmt.Sprintf(`client<llm> ContractFB {
    provider baml-fallback
    options {
        %s
    }
}
`, expr)
			cfg := runProductionParser(t, src)
			gotIntrospect := cfg.fallbackChains["ContractFB"]
			if !reflect.DeepEqual(gotIntrospect, tt.want) {
				t.Fatalf("introspect chain = %v, want %v", gotIntrospect, tt.want)
			}

			gotRuntime := buildrequest.ParseRuntimeStrategyStringForTest(tt.runtimeInput)
			if !reflect.DeepEqual(gotRuntime, tt.want) {
				t.Fatalf("ParseRuntimeStrategyStringForTest(%q) = %v, want %v", tt.runtimeInput, gotRuntime, tt.want)
			}

			if !reflect.DeepEqual(gotIntrospect, gotRuntime) {
				t.Fatalf("parser mismatch: introspect=%v runtime=%v", gotIntrospect, gotRuntime)
			}
		})
	}
}
