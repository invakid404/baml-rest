package clientdefaults

import (
	"encoding/json"
	"testing"
)

func TestAllowedRoleMetadataHandler_ParseAccepts(t *testing.T) {
	cases := []string{
		`"all"`,
		`"none"`,
		`[]`,
		`["cache_control"]`,
		`["cache_control","tenant_id"]`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := allowedRoleMetadataHandler.Parse(json.RawMessage(c)); err != nil {
				t.Fatalf("expected Parse(%s) to succeed, got %v", c, err)
			}
		})
	}
}

func TestAllowedRoleMetadataHandler_ParseRejects(t *testing.T) {
	cases := []string{
		`42`,
		`true`,
		`{"foo":"bar"}`,
		`[1,2,3]`,
		`[null]`,
		// Arbitrary strings: BAML only accepts "all" / "none" for the
		// string form, so anything else must fail at startup rather than
		// survive into BAML and blow up on the first request.
		`"cache_control"`,
		`"ALL"`,
		`""`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := allowedRoleMetadataHandler.Parse(json.RawMessage(c)); err == nil {
				t.Fatalf("expected Parse(%s) to fail", c)
			}
		})
	}
}

func TestAllowedRoleMetadataHandler_ApplyPresentFalse(t *testing.T) {
	parsed := []any{"cache_control"}
	out, set := allowedRoleMetadataHandler.Apply(parsed, nil, false)
	if !set {
		t.Fatalf("expected set=true when key absent")
	}
	outSlice := out.([]any)
	if len(outSlice) != 1 || outSlice[0] != "cache_control" {
		t.Fatalf("expected cloned [cache_control], got %v", outSlice)
	}

	// Mutating the parsed value after Apply must not affect what was returned.
	parsed[0] = "hijacked"
	if outSlice[0] != "cache_control" {
		t.Fatalf("Apply aliased parsed slice: %v", outSlice)
	}
}

func TestAllowedRoleMetadataHandler_ApplyPresentTrue(t *testing.T) {
	parsed := []any{"cache_control"}
	cases := []any{
		[]any{"tenant_id"},
		[]any{},
		"none",
		"all",
		nil,
	}
	for i, existing := range cases {
		out, set := allowedRoleMetadataHandler.Apply(parsed, existing, true)
		if set {
			t.Fatalf("case %d: expected set=false when caller value present", i)
		}
		if out != nil {
			t.Fatalf("case %d: expected out=nil when set=false, got %v", i, out)
		}
	}
}

func TestCloneAsJSONValue(t *testing.T) {
	original := map[string]any{
		"scalar": "s",
		"list":   []any{"a", "b"},
		"nested": map[string]any{"inner": []any{"x"}},
		"num":    float64(1.5),
		"bool":   true,
		"null":   nil,
	}
	clone := cloneAsJSONValue(original).(map[string]any)

	clone["scalar"] = "hijacked"
	if original["scalar"] != "s" {
		t.Fatalf("cloneAsJSONValue did not clone top-level map")
	}

	clone["list"].([]any)[0] = "hijacked"
	if original["list"].([]any)[0] != "a" {
		t.Fatalf("cloneAsJSONValue did not clone list")
	}

	clone["nested"].(map[string]any)["inner"].([]any)[0] = "hijacked"
	if original["nested"].(map[string]any)["inner"].([]any)[0] != "x" {
		t.Fatalf("cloneAsJSONValue did not clone nested list")
	}

	// Primitive scalars are returned unchanged — they're immutable in Go.
	if cloneAsJSONValue("s").(string) != "s" {
		t.Fatalf("scalar clone changed value")
	}
}
