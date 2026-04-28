package adapter

import (
	"reflect"
	"unsafe"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// upstreamClientNamesSnapshot returns a defensive copy of the names
// AddLlmClient was called for on the BuildRequest-safe registry view
// during the most recent SetClientRegistry call. Same-package test-
// only helper — production code must not consume this state. See
// CodeRabbit verdict-22 finding A; previously these were exported
// methods on *BamlAdapter, which expanded the production API surface
// for test-only observability.
func upstreamClientNamesSnapshot(b *BamlAdapter) []string {
	return append([]string(nil), b.upstreamClientNames...)
}

// legacyUpstreamClientNamesSnapshot is the legacy-view companion to
// upstreamClientNamesSnapshot — see that doc for rationale.
func legacyUpstreamClientNamesSnapshot(b *BamlAdapter) []string {
	return append([]string(nil), b.legacyUpstreamClientNames...)
}

// legacyClientEntrySnapshot reads the (provider, options) tuple BAML
// stored under name in the LegacyClientRegistry's internal map.
// CodeRabbit verdict-38 finding F1 wanted assertions on what we
// actually forwarded to BAML, but BAML's ClientRegistry exposes only
// AddLlmClient / SetPrimaryClient — `clients` is unexported. This
// helper uses reflection + unsafe.Pointer to peek at the field shape
// pinned in language_client_go's rawobjects_client_registry.go
// (`clients clientRegistryMap = map[string]clientProperty`, where
// clientProperty has `provider string` and `options map[string]any`).
//
// Returns ok=false when the registry is nil, the key is absent, or
// the BAML registry shape changes. Adapter-version-pinned tests run
// against pinned BAML versions, so the shape is stable for each
// adapter package; a future BAML rev that renames fields would surface
// here as ok=false (the test would fail loudly rather than silently
// pass).
func legacyClientEntrySnapshot(b *BamlAdapter, name string) (provider string, options map[string]any, ok bool) {
	reg := b.LegacyClientRegistry
	if reg == nil {
		return "", nil, false
	}
	regVal := reflect.ValueOf(reg).Elem()
	clientsField := regVal.FieldByName("clients")
	if !clientsField.IsValid() || clientsField.Kind() != reflect.Map {
		return "", nil, false
	}
	clientsField = reflect.NewAt(clientsField.Type(), unsafe.Pointer(clientsField.UnsafeAddr())).Elem()
	entry := clientsField.MapIndex(reflect.ValueOf(name))
	if !entry.IsValid() {
		return "", nil, false
	}
	// Map values are returned as unaddressable Values (the map's
	// internal hashing means there's no stable address). Copy into an
	// addressable temporary so unsafe.Pointer + UnsafeAddr can read
	// the unexported provider/options fields.
	addr := reflect.New(entry.Type()).Elem()
	addr.Set(entry)
	providerField := addr.FieldByName("provider")
	optionsField := addr.FieldByName("options")
	if !providerField.IsValid() || !optionsField.IsValid() {
		return "", nil, false
	}
	providerField = reflect.NewAt(providerField.Type(), unsafe.Pointer(providerField.UnsafeAddr())).Elem()
	optionsField = reflect.NewAt(optionsField.Type(), unsafe.Pointer(optionsField.UnsafeAddr())).Elem()
	provider = providerField.String()
	if optionsField.IsNil() {
		options = nil
	} else {
		options = make(map[string]any, optionsField.Len())
		for _, k := range optionsField.MapKeys() {
			options[k.String()] = optionsField.MapIndex(k).Interface()
		}
	}
	return provider, options, true
}

var _ = baml.NewClientRegistry // pin baml import for the BamlAdapter type used in reflection
