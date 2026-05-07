// Package adapterversions is the canonical inventory of pinned BAML
// adapter versions baml-rest supports, plus the codegen.Options each
// version requires. Two consumers share this list:
//
//   - cmd/verify-framework-adapter (the PR 4a CI verifier) iterates
//     FrameworkAdapters to run the structural-equivalence /
//     deterministic-emission / behavioural-test-parity checks against
//     every adapter.
//   - Each adapters/adapter_v0_<X>_0/cmd/main.go calls
//     MustOptionsForSelfPkg to fetch its own options without
//     re-declaring the matrix.
//
// Keeping the list in one place avoids the (verifier, 3 cmd/main.go)
// duplication and means new adapter versions or new feature flags get
// added in exactly one place.
package adapterversions

import (
	"fmt"

	"github.com/invakid404/baml-rest/adapters/common/codegen"
)

// FrameworkAdapter declares the codegen.Options for one pinned
// adapter, plus the on-disk directory name (relative to adapters/)
// the verifier needs to locate the hand-written adapter.go for the
// structural-equivalence comparison.
type FrameworkAdapter struct {
	// DirName is the per-adapter directory under adapters/, e.g.
	// "adapter_v0_204_0". Consumed by the verifier to build paths
	// like adapters/<DirName>/adapter/adapter.go.
	DirName string
	// Options is the per-version codegen feature matrix the adapter's
	// cmd/main.go would otherwise inline. Every field codegen.Options
	// carries today is captured here so consumers don't accidentally
	// drop a flag at the call site.
	Options codegen.Options
}

// FrameworkAdapters is the canonical inventory. Order is meaningful:
// the verifier emits "=== <DirName> ===" headers in this order, and
// per-version flag values are paired with their SelfPkg here.
var FrameworkAdapters = []FrameworkAdapter{
	{
		DirName: "adapter_v0_204_0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0",
			SupportsWithClient: false,
			HasWrapMapValues:   true,
			HasHTTPClient:      false,
		},
	},
	{
		DirName: "adapter_v0_215_0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_215_0",
			SupportsWithClient: false,
			HasWrapMapValues:   false,
			HasHTTPClient:      false,
		},
	},
	{
		DirName: "adapter_v0_219_0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0",
			SupportsWithClient: true,
			HasWrapMapValues:   false,
			HasHTTPClient:      true,
		},
	},
}

// MustOptionsForSelfPkg returns the codegen.Options registered for
// selfPkg. Panics if selfPkg is not in the inventory; the caller is
// always a per-adapter cmd/main.go that knows its own SelfPkg as a
// compile-time constant, so a missing entry indicates the inventory
// has drifted from the on-disk adapter set rather than a runtime
// error worth recovering from.
func MustOptionsForSelfPkg(selfPkg string) codegen.Options {
	for _, fa := range FrameworkAdapters {
		if fa.Options.SelfPkg == selfPkg {
			return fa.Options
		}
	}
	known := make([]string, 0, len(FrameworkAdapters))
	for _, fa := range FrameworkAdapters {
		known = append(known, fa.Options.SelfPkg)
	}
	panic(fmt.Sprintf("adapterversions: SelfPkg %q not in FrameworkAdapters; known: %v", selfPkg, known))
}
