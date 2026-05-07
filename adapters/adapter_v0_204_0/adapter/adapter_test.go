package adapter

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/adapters/common/testdriver"
)

// adapterUnderTest wraps the per-version *BamlAdapter so the shared
// SetClientRegistry suite can drive it without naming the per-version
// *baml.ClientRegistry type. The snapshot accessors delegate into the
// per-adapter shims in adapter_helpers_test.go, which in turn read
// BAML's unexported ClientRegistry fields via reflection through
// adapters/common/testhelpers.
type adapterUnderTest struct {
	*BamlAdapter
}

func newAdapterUnderTest(introspected map[string]string) testdriver.Adapter {
	return &adapterUnderTest{
		BamlAdapter: &BamlAdapter{
			Context:                    context.Background(),
			IntrospectedClientProvider: introspected,
		},
	}
}

func (a *adapterUnderTest) BuildRequestRegistry() any {
	return a.BamlAdapter.ClientRegistry
}

func (a *adapterUnderTest) LegacyRegistry() any {
	return a.BamlAdapter.LegacyClientRegistry
}

func (a *adapterUnderTest) BuildRequestRegistryNil() bool {
	return a.BamlAdapter.ClientRegistry == nil
}

func (a *adapterUnderTest) LegacyRegistryNil() bool {
	return a.BamlAdapter.LegacyClientRegistry == nil
}

func (a *adapterUnderTest) UpstreamClientNamesSnapshot() []string {
	return upstreamClientNamesSnapshot(a.BamlAdapter)
}

func (a *adapterUnderTest) LegacyUpstreamClientNamesSnapshot() []string {
	return legacyUpstreamClientNamesSnapshot(a.BamlAdapter)
}

func (a *adapterUnderTest) BuildRequestClientEntrySnapshot(name string) (string, map[string]any, bool) {
	return buildRequestClientEntrySnapshot(a.BamlAdapter, name)
}

func (a *adapterUnderTest) LegacyClientEntrySnapshot(name string) (string, map[string]any, bool) {
	return legacyClientEntrySnapshot(a.BamlAdapter, name)
}

func (a *adapterUnderTest) BuildRequestPrimarySnapshot() *string {
	return clientRegistryPrimarySnapshot(a.BamlAdapter.ClientRegistry)
}

func (a *adapterUnderTest) LegacyPrimarySnapshot() *string {
	return clientRegistryPrimarySnapshot(a.BamlAdapter.LegacyClientRegistry)
}

// ExpectedOptions wraps every value through DynamicValue. v0.204
// recursively wraps client.Options before forwarding to BAML's older
// CFFI shape (dynamic_value.go: WrapMapValues), so BAML's internal
// client map stores wrapped values; comparisons against an oracle
// must run the same wrap on the input. v0.215+ pass options through
// unwrapped and override this hook with the identity transform.
func (a *adapterUnderTest) ExpectedOptions(in map[string]any) map[string]any {
	return WrapMapValues(in)
}

func TestSetClientRegistry(t *testing.T) {
	testdriver.RunSetClientRegistrySuite(t, newAdapterUnderTest)
}
