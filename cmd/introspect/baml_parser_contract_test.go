package main

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
)

func TestStrategyParserContract_RuntimeAndIntrospectOverlap(t *testing.T) {
	tests := []struct {
		name            string
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
			gotIntrospect := parseStrategyList(tt.introspectInput)
			if !reflect.DeepEqual(gotIntrospect, tt.want) {
				t.Fatalf("parseStrategyList(%q) = %v, want %v", tt.introspectInput, gotIntrospect, tt.want)
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
