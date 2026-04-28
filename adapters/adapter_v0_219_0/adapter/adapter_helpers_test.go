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
// CodeRabbit verdict-22 v0.219 audit; previously these were exported
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

// clientEntrySnapshot reads the (provider, options) tuple BAML stored
// under name in the supplied ClientRegistry's internal map. CodeRabbit
// verdict-38 F1 + verdict-39 F1-F3 — see the v0.204 adapter helper
// for the full rationale and the kind/type guards that protect
// against BAML registry shape drift.
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
