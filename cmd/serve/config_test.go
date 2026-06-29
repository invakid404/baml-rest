package main

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestParseTruthyEnvBool pins the env-parser contract: 1/true/yes/on
// (case-insensitive) are the only truthy tokens. Mirrors the shared
// bamlutils.IsTruthyEnvValue accept list so baml-rest's server env flags
// share a single truthiness convention — diverging would surprise
// operators who set multiple vars together.
func TestParseTruthyEnvBool(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{"Yes", true},
		{"On", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{" true ", false}, // no whitespace trimming — mirrors IsTruthyEnvValue
		{"truthy", false},
		{"2", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseTruthyEnvBool(tc.in); got != tc.want {
				t.Errorf("parseTruthyEnvBool(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestApplyPreserveSchemaOrderDefault_PreservesExplicitPerRequestChoice
// pins the contract that the server default only fills in an absent
// (nil) PreserveSchemaOrder. Per-request *true and *false must survive
// the helper unchanged so callers retain the ability to override the
// server default on either side of the boolean.
func TestApplyPreserveSchemaOrderDefault_PreservesExplicitPerRequestChoice(t *testing.T) {
	t.Parallel()

	t.Run("nil inherits true default", func(t *testing.T) {
		in := &bamlutils.DynamicInput{}
		applyPreserveSchemaOrderDefault(in, true)
		if in.PreserveSchemaOrder == nil || !*in.PreserveSchemaOrder {
			t.Errorf("nil + default=true should resolve to *true, got %v", in.PreserveSchemaOrder)
		}
	})

	t.Run("nil inherits false default", func(t *testing.T) {
		in := &bamlutils.DynamicInput{}
		applyPreserveSchemaOrderDefault(in, false)
		if in.PreserveSchemaOrder == nil || *in.PreserveSchemaOrder {
			t.Errorf("nil + default=false should resolve to *false, got %v", in.PreserveSchemaOrder)
		}
	})

	t.Run("explicit true wins over false default", func(t *testing.T) {
		explicit := true
		in := &bamlutils.DynamicInput{PreserveSchemaOrder: &explicit}
		applyPreserveSchemaOrderDefault(in, false)
		if in.PreserveSchemaOrder == nil || !*in.PreserveSchemaOrder {
			t.Errorf("*true must survive default=false, got %v", in.PreserveSchemaOrder)
		}
	})

	t.Run("explicit false wins over true default", func(t *testing.T) {
		explicit := false
		in := &bamlutils.DynamicInput{PreserveSchemaOrder: &explicit}
		applyPreserveSchemaOrderDefault(in, true)
		if in.PreserveSchemaOrder == nil || *in.PreserveSchemaOrder {
			t.Errorf("*false must survive default=true, got %v", in.PreserveSchemaOrder)
		}
	})
}

// TestApplyParsePreserveSchemaOrderDefault mirrors the call-side
// contract on the parse-input type.
func TestApplyParsePreserveSchemaOrderDefault(t *testing.T) {
	t.Parallel()

	t.Run("nil inherits true default", func(t *testing.T) {
		in := &bamlutils.DynamicParseInput{}
		applyParsePreserveSchemaOrderDefault(in, true)
		if in.PreserveSchemaOrder == nil || !*in.PreserveSchemaOrder {
			t.Errorf("nil + default=true should resolve to *true, got %v", in.PreserveSchemaOrder)
		}
	})

	t.Run("explicit false wins over true default", func(t *testing.T) {
		explicit := false
		in := &bamlutils.DynamicParseInput{PreserveSchemaOrder: &explicit}
		applyParsePreserveSchemaOrderDefault(in, true)
		if in.PreserveSchemaOrder == nil || *in.PreserveSchemaOrder {
			t.Errorf("*false must survive default=true, got %v", in.PreserveSchemaOrder)
		}
	})
}
