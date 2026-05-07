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

// definedStringKey is the regression case for the exact-type guard on the
// `clients` map key: a defined-string alias passes a Kind-only check and
// then panics at MapIndex when keyed with a plain `string`. The exact-type
// guard must reject this with ok=false.
type definedStringKey string

type driftRegistryDefinedStringKey struct {
	primary *string
	clients map[definedStringKey]struct {
		provider string
		options  map[string]any
	}
}

// driftRegistryProviderDefinedString has the right `clients` shape but
// the entry's `provider` field is a defined-string alias rather than a
// plain `string`. The exact-type guard on `provider` must reject this.
type driftRegistryProviderDefinedString struct {
	primary *string
	clients map[string]struct {
		provider definedStringKey
		options  map[string]any
	}
}

// driftRegistryOptionsDefinedKey has the right `clients` shape but the
// entry's `options` map key is a defined-string alias rather than a plain
// `string`. The exact-type guard on `options` must reject this.
type driftRegistryOptionsDefinedKey struct {
	primary *string
	clients map[string]struct {
		provider string
		options  map[definedStringKey]any
	}
}

// driftRegistryOptionsWrongValue has the right `clients` shape but the
// entry's `options` map value is `string` instead of `any`. The
// exact-type guard on `options` must reject this — silently iterating
// such a map and copying values would coerce them to `any` and surprise
// downstream callers.
type driftRegistryOptionsWrongValue struct {
	primary *string
	clients map[string]struct {
		provider string
		options  map[string]string
	}
}

func TestClientEntrySnapshot_ShapeDrift(t *testing.T) {
	withEntry := func(reg any) any { return reg }
	cases := []struct {
		name string
		reg  any
	}{
		{"non-pointer", baml.ClientRegistry{}},
		{"pointer-to-non-struct", func() any { s := "x"; return &s }()},
		{"missing-clients-field", &driftRegistryNoClientsField{}},
		{"wrong-key-kind", &driftRegistryWrongKeyKind{}},
		// Regression cases for the exact-type guards added per Codex's
		// NO-GO finding on PR 2: each previously-Kind-only guard would
		// have either panicked (clients-key) or silently misbehaved
		// (provider/options) when crossed by a synthetic input via the
		// `any` boundary that wasn't reachable through *baml.ClientRegistry.
		{
			name: "clients-defined-string-key",
			reg: withEntry(&driftRegistryDefinedStringKey{
				clients: map[definedStringKey]struct {
					provider string
					options  map[string]any
				}{"x": {provider: "openai", options: nil}},
			}),
		},
		{
			name: "provider-defined-string",
			reg: withEntry(&driftRegistryProviderDefinedString{
				clients: map[string]struct {
					provider definedStringKey
					options  map[string]any
				}{"x": {provider: "openai", options: nil}},
			}),
		},
		{
			name: "options-defined-string-key",
			reg: withEntry(&driftRegistryOptionsDefinedKey{
				clients: map[string]struct {
					provider string
					options  map[definedStringKey]any
				}{"x": {provider: "openai", options: nil}},
			}),
		},
		{
			name: "options-wrong-value-type",
			reg: withEntry(&driftRegistryOptionsWrongValue{
				clients: map[string]struct {
					provider string
					options  map[string]string
				}{"x": {provider: "openai", options: nil}},
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ClientEntrySnapshot panicked on shape drift: %v", r)
				}
			}()
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

// driftRegistryPrimaryDefinedStringPtr is the regression case for the
// exact-`*string` guard: `*StringAlias` would pass Kind=Ptr +
// Elem().Kind()=String but break the round-trip-as-Go-string contract.
type driftRegistryPrimaryDefinedStringPtr struct {
	primary *definedStringKey
	clients map[string]any
}

func TestClientRegistryPrimarySnapshot_ShapeDrift(t *testing.T) {
	aliasVal := definedStringKey("Primary")
	cases := []struct {
		name string
		reg  any
	}{
		{"non-pointer", baml.ClientRegistry{}},
		{"pointer-to-non-struct", func() any { s := "x"; return &s }()},
		{"missing-primary-field", &driftRegistryNoPrimaryField{}},
		{"primary-wrong-elem", &driftRegistryPrimaryWrongElem{}},
		{"primary-not-pointer", &driftRegistryPrimaryNotPointer{}},
		{"primary-defined-string-ptr", &driftRegistryPrimaryDefinedStringPtr{primary: &aliasVal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ClientRegistryPrimarySnapshot panicked on shape drift: %v", r)
				}
			}()
			if got := testhelpers.ClientRegistryPrimarySnapshot(tc.reg); got != nil {
				t.Errorf("got %q, want nil on shape drift", *got)
			}
		})
	}
}
