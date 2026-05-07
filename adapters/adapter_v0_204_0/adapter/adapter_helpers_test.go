package adapter

import (
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/adapters/common/testhelpers"
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

// clientEntrySnapshot delegates to the shared reflection helper. The
// shared package operates on `any` because each adapter's
// *baml.ClientRegistry is a distinct per-version type identity.
func clientEntrySnapshot(reg *baml.ClientRegistry, name string) (provider string, options map[string]any, ok bool) {
	return testhelpers.ClientEntrySnapshot(reg, name)
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

// clientRegistryPrimarySnapshot delegates to the shared reflection
// helper — see clientEntrySnapshot's comment for rationale.
func clientRegistryPrimarySnapshot(reg *baml.ClientRegistry) *string {
	return testhelpers.ClientRegistryPrimarySnapshot(reg)
}
