// Package testhelpers provides version-agnostic reflection-based snapshot
// helpers that read the unexported fields of BAML's ClientRegistry struct.
//
// Each per-adapter module pins its own BAML version and therefore exposes a
// distinct *baml.ClientRegistry type identity, which cannot be named in a
// shared package. The helpers here accept the registry pointer as `any` and
// rely on reflection to read the unexported `clients` map and `primary`
// pointer. Every reflect.Value is verified for kind/type before any
// unsafe.Pointer access, so a future BAML rev that renames a field or
// changes the registry shape surfaces loudly via ok=false (or a nil result)
// rather than panicking on UnsafeAddr against an unexpected type.
//
// Type guards use exact-type comparison (e.g. `reflect.TypeOf("")` rather
// than `Kind() == reflect.String`) on every code path that subsequently
// constructs a typed reflect.Value to interact with the field — most
// notably `MapIndex` against the `clients` map, which would panic on a
// defined-string-alias key that passes a Kind-only check. Kind-only checks
// would suffice while the helpers took a typed `*baml.ClientRegistry`
// parameter (BAML's pinned shape made the alias case unreachable), but the
// shared `any` boundary makes arbitrary synthetic shapes reachable, so the
// guards are tightened accordingly.
//
// These helpers underpin the per-adapter `clientEntrySnapshot` and
// `clientRegistryPrimarySnapshot` test wrappers; production code must not
// consume them.
package testhelpers

import (
	"reflect"
	"unsafe"
)

var (
	stringType       = reflect.TypeOf("")
	stringPtrType    = reflect.TypeOf((*string)(nil))
	stringAnyMapType = reflect.TypeOf(map[string]any(nil))
)

// ClientEntrySnapshot reads the (provider, options) tuple BAML stored under
// name in the supplied ClientRegistry's internal map. BAML's ClientRegistry
// exposes only AddLlmClient / SetPrimaryClient — `clients` is unexported.
// This helper uses reflection + unsafe.Pointer to peek at the field shape
// pinned in language_client_go's rawobjects_client_registry.go
// (`clients clientRegistryMap = map[string]clientProperty`, where
// clientProperty has `provider string` and `options map[string]any`).
//
// The reg parameter is typed as `any` so a single implementation works
// across all per-adapter *baml.ClientRegistry type identities. Callers are
// expected to pass a non-nil *baml.ClientRegistry; any other shape (or a
// nil pointer, or a registry whose internal layout has drifted) returns
// ok=false.
func ClientEntrySnapshot(reg any, name string) (provider string, options map[string]any, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	regVal := reflect.ValueOf(reg)
	if regVal.Kind() != reflect.Ptr || regVal.IsNil() {
		return "", nil, false
	}
	regVal = regVal.Elem()
	if regVal.Kind() != reflect.Struct {
		return "", nil, false
	}
	clientsField := regVal.FieldByName("clients")
	if !clientsField.IsValid() || clientsField.Kind() != reflect.Map {
		return "", nil, false
	}
	// Exact-type guard on the map key: a defined-string alias
	// (`type myString string`) would pass a Kind-only check and then
	// panic at MapIndex below when keyed with a plain `string`.
	if clientsField.Type().Key() != stringType {
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
	// Map values are returned as unaddressable Values (the map's internal
	// hashing means there's no stable address). Copy into an addressable
	// temporary so unsafe.Pointer + UnsafeAddr can read the unexported
	// provider/options fields.
	addr := reflect.New(entry.Type()).Elem()
	addr.Set(entry)
	providerField := addr.FieldByName("provider")
	optionsField := addr.FieldByName("options")
	if !providerField.IsValid() || !optionsField.IsValid() {
		return "", nil, false
	}
	// Exact-type guards: `provider` must be `string` and `options` must be
	// `map[string]any`. Kind-only checks would let a defined-type alias
	// through; the read paths below would silently coerce or — for the
	// options-key case — be one MapIndex(plainString) away from panicking
	// if the iteration strategy ever changes.
	if providerField.Type() != stringType {
		return "", nil, false
	}
	if optionsField.Type() != stringAnyMapType {
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

// ClientRegistryPrimarySnapshot reads BAML's unexported `primary *string`
// field from the supplied *baml.ClientRegistry. Returns nil when primary is
// unset (BAML's representation of "no primary forwarded"), or when the
// field shape drifts from the pinned `primary *string` type. Otherwise
// returns a copy of the dereferenced string. Same reflect+unsafe pattern
// as ClientEntrySnapshot, and the same `any` boundary for the same reason.
func ClientRegistryPrimarySnapshot(reg any) *string {
	if reg == nil {
		return nil
	}
	regVal := reflect.ValueOf(reg)
	if regVal.Kind() != reflect.Ptr || regVal.IsNil() {
		return nil
	}
	regVal = regVal.Elem()
	if regVal.Kind() != reflect.Struct {
		return nil
	}
	primaryField := regVal.FieldByName("primary")
	if !primaryField.IsValid() {
		return nil
	}
	// Exact-type guard: `primary` must be `*string`. A `*StringAlias`
	// would pass Kind=Ptr + Elem().Kind()=String but break callers that
	// expect the value to round-trip as a Go string.
	if primaryField.Type() != stringPtrType {
		return nil
	}
	primaryField = reflect.NewAt(primaryField.Type(), unsafe.Pointer(primaryField.UnsafeAddr())).Elem()
	if primaryField.IsNil() {
		return nil
	}
	s := primaryField.Elem().String()
	return &s
}
