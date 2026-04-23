//go:build !debug

package buildrequest

// debugFilterCallSupported is a release-build no-op: the debug env override
// compiles away so IsCallProviderSupported's behaviour on a release binary
// is driven entirely by callSupportedProviders. See call_support_debug.go
// for the debug implementation.
func debugFilterCallSupported(_ string, staticSupported bool) bool {
	return staticSupported
}
