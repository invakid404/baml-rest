// Package testdriver runs the consolidated SetClientRegistry test suite
// against any per-adapter BamlAdapter via the Adapter interface. Each
// per-version adapter (v0.204 / v0.215 / v0.219) exposes the same
// SetClientRegistry behaviour around BAML's ClientRegistry — dual-view
// materialisation, strategy-parent drop predicates, primary-cache
// synthesis, validator-driven duplicate / empty-name rejection — but
// the underlying *baml.ClientRegistry pointer is a per-version distinct
// Go type because each adapter module pins its own BAML version. The
// Adapter interface is the seam that lets the suite drive all three
// modules from one place: the per-adapter test file constructs a thin
// harness around its concrete *BamlAdapter and passes a Factory that
// produces fresh harnesses to RunSetClientRegistrySuite.
//
// The 14 tests previously triplicated across each adapter_test.go plus
// the 6 cases that historically lived only in v0.219 (covering the
// same SetClientRegistry behaviour the older adapters also implement)
// run uniformly here. The per-adapter test file shrinks to a single
// TestSetClientRegistry function that wires the harness and calls
// RunSetClientRegistrySuite.
package testdriver

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Adapter is the per-version handle the suite drives. Each per-adapter
// test file defines a harness type that satisfies this interface around
// its concrete *BamlAdapter, then the suite calls into it without
// naming the per-version *baml.ClientRegistry type.
//
// The interface composes bamlutils.Adapter (which every per-adapter
// *BamlAdapter satisfies via the var _ bamlutils.Adapter compile-time
// check at the bottom of each adapter.go) with a small set of test-only
// hooks: registry-pointer accessors that return the typed pointer
// wrapped in any (so suite code can do identity comparisons across
// calls without importing the per-version baml package), the snapshot
// helpers that delegate into the per-adapter shims backed by
// adapters/common/testhelpers, and the per-version ExpectedOptions
// hook that translates an operator-supplied options map into the form
// BAML's internal client map stores it in (v0.204 wraps every value
// through DynamicValue for the older CFFI shape; v0.215+ leave the
// map untouched).
type Adapter interface {
	bamlutils.Adapter

	// BuildRequestRegistry returns the BuildRequest-safe *baml.ClientRegistry
	// pointer wrapped in any. Two calls return == when the underlying
	// typed pointer is unchanged, so suite code uses this for identity
	// comparisons (rejected SetClientRegistry calls must not swap the
	// registry pointer underneath).
	BuildRequestRegistry() any
	LegacyRegistry() any

	// BuildRequestRegistryNil reports whether the BuildRequest-safe
	// registry pointer is currently nil. Wrapping a typed-nil pointer
	// in any does not equal a literal nil any, so callers cannot use
	// `BuildRequestRegistry() == nil` directly — this method does the
	// per-adapter `b.ClientRegistry == nil` check on the typed field.
	BuildRequestRegistryNil() bool
	LegacyRegistryNil() bool

	// UpstreamClientNamesSnapshot returns a defensive copy of the
	// AddLlmClient name order recorded on the BuildRequest-safe view
	// during the most recent SetClientRegistry call.
	UpstreamClientNamesSnapshot() []string
	LegacyUpstreamClientNamesSnapshot() []string

	// BuildRequestClientEntrySnapshot reads BAML's stored
	// (provider, options) pair for name in the BuildRequest-safe view.
	// ok=false when the entry is absent, the registry pointer is nil,
	// or BAML's internal field shape drifts (the underlying reflection
	// helper guards every UnsafeAddr access with kind/type checks).
	BuildRequestClientEntrySnapshot(name string) (provider string, options map[string]any, ok bool)
	LegacyClientEntrySnapshot(name string) (provider string, options map[string]any, ok bool)

	// BuildRequestPrimarySnapshot reads BAML's stored unexported
	// `primary *string` from the BuildRequest-safe registry; nil for
	// "primary unset" (or when the registry pointer is nil / shape
	// drifts).
	BuildRequestPrimarySnapshot() *string
	LegacyPrimarySnapshot() *string

	// ExpectedOptions returns the form an operator-supplied options
	// map takes once stored in BAML's internal client map. v0.204 wraps
	// every value through DynamicValue (the older CFFI shape requires
	// it for nested-map serialisation); v0.215+ pass the map through
	// untouched. Tests that compare snapshot options against an
	// expected oracle compute the oracle via this hook so the suite
	// does not need to know which adapter wraps and which doesn't.
	ExpectedOptions(map[string]any) map[string]any
}

// Factory constructs a fresh per-version Adapter wired with the
// supplied IntrospectedClientProvider map. The suite calls this once
// per top-level subtest (each subtest constructs its own adapter to
// keep state isolated). The returned Adapter must be ready to receive
// SetClientRegistry calls.
type Factory func(introspected map[string]string) Adapter

// RunSetClientRegistrySuite drives the consolidated SetClientRegistry
// test set against a single per-version adapter. Each per-adapter
// test file calls this from a single TestSetClientRegistry function:
//
//	func TestSetClientRegistry(t *testing.T) {
//	    testdriver.RunSetClientRegistrySuite(t, newAdapterUnderTest)
//	}
//
// where newAdapterUnderTest is the per-adapter harness factory.
func RunSetClientRegistrySuite(t *testing.T, factory Factory) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"NilRegistryClearsState", runNilRegistryClearsState},
		{"SkipsNilClientEntries", runSkipsNilClientEntries},
		{"ClearsStaleProviderCache", runClearsStaleProviderCache},
		{"StaticPrimary_PopulatesProviderFromIntrospected", runStaticPrimaryPopulatesProviderFromIntrospected},
		{"PresentEmptyMatchingPrimary_StaysEmpty", runPresentEmptyMatchingPrimaryStaysEmpty},
		{"PresentNonEmptyMatchingPrimary_UsesOverride", runPresentNonEmptyMatchingPrimaryUsesOverride},
		{"KeepsExplicitStrategyParentForLegacyOnly", runKeepsExplicitStrategyParentForLegacyOnly},
		{"DropsInertPresenceOnlyParentInBothBamlViews", runDropsInertPresenceOnlyParentInBothBamlViews},
		{"DualViewPrimaryCacheUnderInertParent", runDualViewPrimaryCacheUnderInertParent},
		{"DualViewPrimaryCacheUnderExplicitParent", runDualViewPrimaryCacheUnderExplicitParent},
		{"PresentEmptyPrimaryIsNoOp", runPresentEmptyPrimaryIsNoOp},
		{"RejectsDuplicateClientName", runRejectsDuplicateClientName},
		{"AcceptsDistinctClientNames", runAcceptsDistinctClientNames},
		{"RejectsEmptyClientName", runRejectsEmptyClientName},
		{"DropsResolvedStrategyParents", runDropsResolvedStrategyParents},
		{"PresenceOnlyRRRoundTrip", runPresenceOnlyRRRoundTrip},
		{"NonStrategyEntriesForwarded", runNonStrategyEntriesForwarded},
		{"TranslatesCanonicalRoundRobinSpelling", runTranslatesCanonicalRoundRobinSpelling},
		{"PresentEmptyProviderStaysEmpty", runPresentEmptyProviderStaysEmpty},
		{"ExplicitProviderPassesThrough", runExplicitProviderPassesThrough},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, factory)
		})
	}
}

// emptyMap is a convenience for "construct a fresh adapter without
// any introspected mappings". Several tests want explicit isolation
// between the runtime override and the introspected baseline.
func emptyMap() map[string]string { return map[string]string{} }
