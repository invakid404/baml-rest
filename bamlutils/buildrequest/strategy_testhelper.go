package buildrequest

// ParseRuntimeStrategyStringForTest exposes the runtime string strategy parser
// for cross-package contract tests. Keep this narrow: it only covers the
// string-tokenization path shared with introspection parsing.
func ParseRuntimeStrategyStringForTest(s string) []string {
	return parseRuntimeStrategyOption(s)
}
