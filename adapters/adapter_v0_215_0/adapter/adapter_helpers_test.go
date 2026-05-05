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
// under name in the supplied ClientRegistry's internal map — see the
// v0.204 adapter helper for the full rationale and the kind/type
// guards that protect against BAML registry shape drift.
func clientEntrySnapshot(reg *baml.ClientRegistry, name string) (provider string, options map[string]any, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	regVal := reflect.ValueOf(reg).Elem()
	clientsField := regVal.FieldByName("clients")
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

func legacyClientEntrySnapshot(b *BamlAdapter, name string) (provider string, options map[string]any, ok bool) {
	return clientEntrySnapshot(b.LegacyClientRegistry, name)
}

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
