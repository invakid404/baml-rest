package nativeprompt

import (
	"strings"
	"testing"

	"github.com/mitsuhiko/minijinja/minijinja-go/v2/value"
)

// TestRoleHelperAllowDupeMustBeBool pins the CodeRabbit fix: a non-bool
// __baml_allow_dupe_role__ kwarg is rejected (fail closed) rather than silently
// coerced to false, consistent with the sibling role positional/kwarg
// validation. A valid bool is accepted and carried into the role marker.
func TestRoleHelperAllowDupeMustBeBool(t *testing.T) {
	h := roleHelper{}

	// Non-bool allow_dupe -> error.
	if _, err := h.CallMethod(nil, "role",
		[]value.Value{value.FromString("user")},
		map[string]value.Value{allowDupeRoleKey: value.FromString("true")},
	); err == nil {
		t.Fatalf("expected error for non-bool %s, got nil", allowDupeRoleKey)
	}

	// A numeric value must also be rejected (not coerced).
	if _, err := h.CallMethod(nil, "role",
		[]value.Value{value.FromString("user")},
		map[string]value.Value{allowDupeRoleKey: value.FromInt(1)},
	); err == nil {
		t.Fatalf("expected error for numeric %s, got nil", allowDupeRoleKey)
	}

	// Valid bool allow_dupe -> accepted, reflected in the marker.
	out, err := h.CallMethod(nil, "role",
		[]value.Value{value.FromString("user")},
		map[string]value.Value{allowDupeRoleKey: value.FromBool(true)},
	)
	if err != nil {
		t.Fatalf("valid bool allow_dupe should succeed, got %v", err)
	}
	marker, ok := out.AsString()
	if !ok || !strings.Contains(marker, `"`+allowDupeRoleKey+`":true`) {
		t.Errorf("marker should carry allow_dupe=true, got %q", marker)
	}
}
