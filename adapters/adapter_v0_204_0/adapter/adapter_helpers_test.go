package adapter

import (
	"reflect"
	"unsafe"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// upstreamClientNamesSnapshot returns a defensive copy of the names
// AddLlmClient was called for on the BuildRequest-safe registry view
// during the most recent SetClientRegistry call. Same-package test-
// only helper — production code must not consume this state.
// Exposing this as an exported method on *BamlAdapter would expand
// the production API surface for test-only observability.
func upstreamClientNamesSnapshot(b *BamlAdapter) []string {
	return append([]string(nil), b.upstreamClientNames...)
}

// legacyUpstreamClientNamesSnapshot is the legacy-view companion to
// upstreamClientNamesSnapshot — see that doc for rationale.
func legacyUpstreamClientNamesSnapshot(b *BamlAdapter) []string {
	return append([]string(nil), b.legacyUpstreamClientNames...)
}

// clientEntrySnapshot reads the (provider, options) tuple BAML stored
// under name in the supplied ClientRegistry's internal map. BAML's
// ClientRegistry exposes only AddLlmClient / SetPrimaryClient —
// `clients` is unexported. This helper uses reflection +
// unsafe.Pointer to peek at the field shape pinned in
// language_client_go's rawobjects_client_registry.go
// (`clients clientRegistryMap = map[string]clientProperty`, where
// clientProperty has `provider string` and `options map[string]any`).
//
// Returns ok=false when the registry is nil, the key is absent, or
// the BAML registry shape drifts. Every reflect.Value is verified
// for kind/type before unsafe.Pointer access, so a future BAML rev
// that renamed a field or changed `clients` to a non-string-keyed
// map surfaces loudly as ok=false rather than panicking on
// UnsafeAddr against an unexpected type.
func clientEntrySnapshot(reg *baml.ClientRegistry, name string) (provider string, options map[string]any, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	regVal := reflect.ValueOf(reg).Elem()
	clientsField := regVal.FieldByName("clients")
	// Shape guards: confirm the field exists, is a map, and has
	// string keys before any reflection trickery.
	if !clientsField.IsValid() || clientsField.Kind() != reflect.Map {
		return "", nil, false
	}
	if clientsField.Type().Key().Kind() != reflect.String {
		return "", nil, false
	}
	clientsField = reflect.NewAt(clientsField.Type(), unsafe.Pointer(clientsField.UnsafeAddr())).Elem()
	entry := clientsField.MapIndex(reflect.ValueOf(name))
	if !entry.IsValid() {
		return "", nil, false
	}
	if entry.Kind() != reflect.Struct {
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
	if providerField.Kind() != reflect.String {
		return "", nil, false
	}
	if optionsField.Kind() != reflect.Map || optionsField.Type().Key().Kind() != reflect.String {
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

// legacyClientEntrySnapshot is the legacy-view wrapper around
// clientEntrySnapshot.
func legacyClientEntrySnapshot(b *BamlAdapter, name string) (provider string, options map[string]any, ok bool) {
	return clientEntrySnapshot(b.LegacyClientRegistry, name)
}

// buildRequestClientEntrySnapshot is the BuildRequest-safe-view
// wrapper around clientEntrySnapshot. Lets tests assert the
// materialised registry preserves operator-supplied entries even
// when Primary is present-empty.
func buildRequestClientEntrySnapshot(b *BamlAdapter, name string) (provider string, options map[string]any, ok bool) {
	return clientEntrySnapshot(b.ClientRegistry, name)
}

// clientRegistryPrimarySnapshot reads BAML's unexported `primary *string`
// field from the supplied *baml.ClientRegistry. Returns nil when primary
// is unset (BAML's representation of "no primary forwarded"), or when the
// field shape drifts from the pinned `primary *string` type. Otherwise
// returns a copy of the dereferenced string. Same reflect+unsafe pattern
// as clientEntrySnapshot.
func clientRegistryPrimarySnapshot(reg *baml.ClientRegistry) *string {
	if reg == nil {
		return nil
	}
	regVal := reflect.ValueOf(reg).Elem()
	primaryField := regVal.FieldByName("primary")
	if !primaryField.IsValid() || primaryField.Kind() != reflect.Ptr {
		return nil
	}
	if primaryField.Type().Elem().Kind() != reflect.String {
		return nil
	}
	primaryField = reflect.NewAt(primaryField.Type(), unsafe.Pointer(primaryField.UnsafeAddr())).Elem()
	if primaryField.IsNil() {
		return nil
	}
	s := primaryField.Elem().String()
	return &s
}
