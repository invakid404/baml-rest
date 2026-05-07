package testhelpers_test

import (
	"reflect"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/adapters/common/testhelpers"
)

func TestClientEntrySnapshot_HappyPath(t *testing.T) {
	reg := &baml.ClientRegistry{}
	opts := map[string]any{"model": "gpt-4o", "temperature": 0.7}
	reg.AddLlmClient("MyClient", "openai", opts)

	provider, options, ok := testhelpers.ClientEntrySnapshot(reg, "MyClient")
	if !ok {
		t.Fatalf("ClientEntrySnapshot: ok=false for present entry")
	}
	if provider != "openai" {
		t.Errorf("provider: got %q, want %q", provider, "openai")
	}
	if !reflect.DeepEqual(options, opts) {
		t.Errorf("options: got %v, want %v", options, opts)
	}
}

func TestClientEntrySnapshot_NilOptions(t *testing.T) {
	reg := &baml.ClientRegistry{}
	reg.AddLlmClient("NoOpts", "openai", nil)

	provider, options, ok := testhelpers.ClientEntrySnapshot(reg, "NoOpts")
	if !ok {
		t.Fatalf("ClientEntrySnapshot: ok=false for present entry with nil options")
	}
	if provider != "openai" {
		t.Errorf("provider: got %q, want %q", provider, "openai")
	}
	if options != nil {
		t.Errorf("options: got %v, want nil", options)
	}
}

func TestClientEntrySnapshot_MissingEntry(t *testing.T) {
	reg := &baml.ClientRegistry{}
	reg.AddLlmClient("Other", "openai", nil)

	if _, _, ok := testhelpers.ClientEntrySnapshot(reg, "Missing"); ok {
		t.Errorf("ClientEntrySnapshot: ok=true for missing entry")
	}
}

func TestClientEntrySnapshot_NilRegistry(t *testing.T) {
	if _, _, ok := testhelpers.ClientEntrySnapshot((*baml.ClientRegistry)(nil), "x"); ok {
		t.Errorf("typed nil pointer: ok=true, want false")
	}
	if _, _, ok := testhelpers.ClientEntrySnapshot(nil, "x"); ok {
		t.Errorf("untyped nil: ok=true, want false")
	}
}

func TestClientEntrySnapshot_EmptyRegistry(t *testing.T) {
	if _, _, ok := testhelpers.ClientEntrySnapshot(&baml.ClientRegistry{}, "x"); ok {
		t.Errorf("ClientEntrySnapshot on empty registry: ok=true, want false")
	}
}

// driftRegistryNoClientsField mimics a future BAML revision that renamed
// `clients`. The kind/type guards in ClientEntrySnapshot must surface this
// as ok=false rather than panicking.
type driftRegistryNoClientsField struct {
	primary *string
	other   map[string]any
}

// driftRegistryWrongKeyKind has a `clients` field whose key is not a
// string. ClientEntrySnapshot must return ok=false.
type driftRegistryWrongKeyKind struct {
	clients map[int]struct {
		provider string
		options  map[string]any
	}
}

func TestClientEntrySnapshot_ShapeDrift(t *testing.T) {
	cases := []struct {
		name string
		reg  any
	}{
		{"non-pointer", baml.ClientRegistry{}},
		{"pointer-to-non-struct", func() any { s := "x"; return &s }()},
		{"missing-clients-field", &driftRegistryNoClientsField{}},
		{"wrong-key-kind", &driftRegistryWrongKeyKind{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := testhelpers.ClientEntrySnapshot(tc.reg, "x"); ok {
				t.Errorf("ok=true on shape drift, want false")
			}
		})
	}
}

func TestClientRegistryPrimarySnapshot_HappyPath(t *testing.T) {
	reg := &baml.ClientRegistry{}
	reg.SetPrimaryClient("Primary")

	got := testhelpers.ClientRegistryPrimarySnapshot(reg)
	if got == nil {
		t.Fatalf("ClientRegistryPrimarySnapshot: nil for set primary")
	}
	if *got != "Primary" {
		t.Errorf("primary: got %q, want %q", *got, "Primary")
	}
}

func TestClientRegistryPrimarySnapshot_Unset(t *testing.T) {
	reg := &baml.ClientRegistry{}
	if got := testhelpers.ClientRegistryPrimarySnapshot(reg); got != nil {
		t.Errorf("ClientRegistryPrimarySnapshot on unset registry: got %q, want nil", *got)
	}
}

func TestClientRegistryPrimarySnapshot_Nil(t *testing.T) {
	if got := testhelpers.ClientRegistryPrimarySnapshot((*baml.ClientRegistry)(nil)); got != nil {
		t.Errorf("typed nil: got %q, want nil", *got)
	}
	if got := testhelpers.ClientRegistryPrimarySnapshot(nil); got != nil {
		t.Errorf("untyped nil: got %q, want nil", *got)
	}
}

// driftRegistryNoPrimaryField mimics a future BAML revision that renamed
// `primary`. ClientRegistryPrimarySnapshot must return nil rather than
// panicking.
type driftRegistryNoPrimaryField struct {
	clients map[string]any
}

// driftRegistryPrimaryWrongElem has a `primary` field that's a pointer to
// something other than string. ClientRegistryPrimarySnapshot must return
// nil.
type driftRegistryPrimaryWrongElem struct {
	primary *int
	clients map[string]any
}

// driftRegistryPrimaryNotPointer has a `primary` field that isn't a
// pointer. ClientRegistryPrimarySnapshot must return nil.
type driftRegistryPrimaryNotPointer struct {
	primary string
	clients map[string]any
}

func TestClientRegistryPrimarySnapshot_ShapeDrift(t *testing.T) {
	cases := []struct {
		name string
		reg  any
	}{
		{"non-pointer", baml.ClientRegistry{}},
		{"pointer-to-non-struct", func() any { s := "x"; return &s }()},
		{"missing-primary-field", &driftRegistryNoPrimaryField{}},
		{"primary-wrong-elem", &driftRegistryPrimaryWrongElem{}},
		{"primary-not-pointer", &driftRegistryPrimaryNotPointer{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := testhelpers.ClientRegistryPrimarySnapshot(tc.reg); got != nil {
				t.Errorf("got %q, want nil on shape drift", *got)
			}
		})
	}
}
