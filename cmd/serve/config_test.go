package main

import (
	"strings"
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

// TestPresentRetiredEnvWarnings pins the warn-and-ignore contract for the
// retired BuildRequest env vars (BAML_REST_USE_BUILD_REQUEST from #537,
// BAML_REST_DISABLE_CALL_BUILD_REQUEST from #539): presence — not
// truthiness — gates the warning, any value warns, and absent vars stay
// silent. The helper is pure (lookup is injected) so the contract is
// testable without capturing log output or mutating the process env.
func TestPresentRetiredEnvWarnings(t *testing.T) {
	t.Parallel()

	lookupFrom := func(present map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			v, ok := present[name]
			return v, ok
		}
	}

	t.Run("no retired vars set -> no warnings", func(t *testing.T) {
		got := presentRetiredEnvWarnings(lookupFrom(map[string]string{
			"BAML_REST_HTTP_CLIENT": "auto",
		}))
		if len(got) != 0 {
			t.Errorf("expected no warnings, got %v", got)
		}
	})

	t.Run("disable-call var present (truthy) warns", func(t *testing.T) {
		got := presentRetiredEnvWarnings(lookupFrom(map[string]string{
			"BAML_REST_DISABLE_CALL_BUILD_REQUEST": "true",
		}))
		if len(got) != 1 {
			t.Fatalf("expected exactly 1 warning, got %d: %v", len(got), got)
		}
		if !strings.Contains(got[0], "BAML_REST_DISABLE_CALL_BUILD_REQUEST") ||
			!strings.Contains(got[0], "retired and ignored") {
			t.Errorf("warning text unexpected: %q", got[0])
		}
	})

	t.Run("presence not truthiness: empty value still warns", func(t *testing.T) {
		got := presentRetiredEnvWarnings(lookupFrom(map[string]string{
			"BAML_REST_DISABLE_CALL_BUILD_REQUEST": "",
		}))
		if len(got) != 1 {
			t.Fatalf("expected empty-value var to warn (presence-based), got %d: %v", len(got), got)
		}
	})

	t.Run("both retired vars present -> both warn in declaration order", func(t *testing.T) {
		got := presentRetiredEnvWarnings(lookupFrom(map[string]string{
			"BAML_REST_USE_BUILD_REQUEST":          "1",
			"BAML_REST_DISABLE_CALL_BUILD_REQUEST": "no",
		}))
		if len(got) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %v", len(got), got)
		}
		if !strings.Contains(got[0], "BAML_REST_USE_BUILD_REQUEST") {
			t.Errorf("first warning should be for BAML_REST_USE_BUILD_REQUEST, got %q", got[0])
		}
		if !strings.Contains(got[1], "BAML_REST_DISABLE_CALL_BUILD_REQUEST") {
			t.Errorf("second warning should be for BAML_REST_DISABLE_CALL_BUILD_REQUEST, got %q", got[1])
		}
	})
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
