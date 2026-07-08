package main

import (
	"reflect"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
)

// schemaWireJSON is the #313 dynamic output schema in deliberately
// non-alphabetical wire order, so a stable-vs-sorted assertion actually
// distinguishes the two policies. Decoded through DynamicOutputSchema's
// OrderedMap unmarshal, the property order is preserved exactly.
const schemaWireJSON = `{
  "properties": {
    "delta": {"type": "string"},
    "alpha": {"type": "int"},
    "hotel": {"type": "list", "items": {"type": "string"}},
    "charlie": {"ref": "Mailbox"},
    "bravo": {"type": "string"},
    "foxtrot": {"ref": "Status"},
    "echo": {"type": "bool"},
    "golf": {"ref": "Profile"}
  },
  "classes": {
    "Profile": {
      "properties": {
        "zulu": {"type": "string"},
        "alpha": {"type": "int"},
        "status": {"ref": "Status"},
        "preference": {"ref": "Preference"}
      }
    },
    "Mailbox": {
      "properties": {
        "zip": {"type": "string"},
        "city": {"type": "string"},
        "street": {"type": "string"}
      }
    }
  },
  "enums": {
    "Status": {"values": [{"name": "PENDING"}, {"name": "ACTIVE"}, {"name": "ARCHIVED"}]},
    "Preference": {"values": [{"name": "LOW"}, {"name": "MEDIUM"}, {"name": "HIGH"}]}
  }
}`

var (
	rootWireOrder = []string{"delta", "alpha", "hotel", "charlie", "bravo", "foxtrot", "echo", "golf"}
	rootSortOrder = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	profWireOrder = []string{"zulu", "alpha", "status", "preference"}
	profSortOrder = []string{"alpha", "preference", "status", "zulu"}
)

// renderSchemaOrderFor resolves a request's preserve_schema_order against a
// server default exactly as the dynamic handlers do (applyPreserveSchemaOrderDefault
// then ToWorkerInput), then returns the field order the native
// ctx.output_format renderer would see: the schema carried in
// __baml_options__.output_schema.
func renderSchemaOrderFor(t *testing.T, serverDefault bool, requestField *bool) (root, profile []string) {
	t.Helper()

	var schema bamlutils.DynamicOutputSchema
	if err := sonic.Unmarshal([]byte(schemaWireJSON), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	input := bamlutils.DynamicInput{
		OutputSchema:        &schema,
		PreserveSchemaOrder: requestField,
	}
	applyPreserveSchemaOrderDefault(&input, serverDefault)

	payload, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	var decoded struct {
		Options struct {
			OutputSchema bamlutils.DynamicOutputSchema `json:"output_schema"`
		} `json:"__baml_options__"`
	}
	if err := sonic.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode worker payload: %v", err)
	}

	prof, ok := decoded.Options.OutputSchema.Classes.Get("Profile")
	if !ok {
		t.Fatal("Profile class missing from carried schema")
	}
	return decoded.Options.OutputSchema.Properties.Keys(), prof.Properties.Keys()
}

func boolPtr(b bool) *bool { return &b }

// TestRenderSchemaOrder_PreservePolicyTruthTable pins the native
// output_format render order policy across the server-default × per-request
// tri-state matrix (the fix for TestDynamicOutputFormatPreserveSchemaOrder_
// ServerDefault and TestDynamicOutputFormatOrderStable under
// BAML_REST_USE_DEBAML): a resolved preserve=true keeps the JSON wire
// order, preserve=false alphabetizes each class's fields.
func TestRenderSchemaOrder_PreservePolicyTruthTable(t *testing.T) {
	cases := []struct {
		name          string
		serverDefault bool
		requestField  *bool
		wantRoot      []string
		wantProfile   []string
	}{
		{
			name:          "default_true_omitted_preserves_wire_order",
			serverDefault: true,
			requestField:  nil,
			wantRoot:      rootWireOrder,
			wantProfile:   profWireOrder,
		},
		{
			name:          "default_true_explicit_false_sorts_alphabetically",
			serverDefault: true,
			requestField:  boolPtr(false),
			wantRoot:      rootSortOrder,
			wantProfile:   profSortOrder,
		},
		{
			name:          "default_false_explicit_true_preserves_wire_order",
			serverDefault: false,
			requestField:  boolPtr(true),
			wantRoot:      rootWireOrder,
			wantProfile:   profWireOrder,
		},
		{
			name:          "default_false_omitted_sorts_alphabetically",
			serverDefault: false,
			requestField:  nil,
			wantRoot:      rootSortOrder,
			wantProfile:   profSortOrder,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, profile := renderSchemaOrderFor(t, tc.serverDefault, tc.requestField)
			if !reflect.DeepEqual(root, tc.wantRoot) {
				t.Errorf("root order: got %v want %v", root, tc.wantRoot)
			}
			if !reflect.DeepEqual(profile, tc.wantProfile) {
				t.Errorf("Profile order: got %v want %v", profile, tc.wantProfile)
			}
		})
	}
}
