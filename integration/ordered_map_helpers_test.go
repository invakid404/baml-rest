//go:build integration

package integration

import (
	"sort"

	"github.com/invakid404/baml-rest/dynclient"
)

// dynclientFromMap converts a plain Go map literal into a dynclient
// OrderedMap. Keys are inserted in lexical order, which keeps ad-hoc
// test fixtures (where wire order does not matter for the assertion
// under test) terse without mixing in the per-entry MustOrderedMap
// boilerplate.
//
// Tests that depend on a specific insertion order construct the
// OrderedMap explicitly via dynclient.MustOrderedMap / OrderedKV
// instead of going through this helper.
func dynclientFromMap[V any](m map[string]V) dynclient.OrderedMap[V] {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var om dynclient.OrderedMap[V]
	for _, k := range keys {
		if err := om.Set(k, m[k]); err != nil {
			panic(err)
		}
	}
	return om
}
