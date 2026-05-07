// Package adapterversions is the canonical inventory of pinned BAML
// adapter versions baml-rest supports, plus the codegen.Options each
// version requires. Three consumers share this list:
//
//   - cmd/verify-framework-adapter (the PR 4a CI verifier) iterates
//     FrameworkAdapters to run the structural-equivalence /
//     deterministic-emission / behavioural-test-parity checks against
//     every adapter.
//   - Each adapters/adapter_v0_<X>_0/cmd/main.go calls
//     MustOptionsForSelfPkg to fetch its own options without
//     re-declaring the matrix.
//   - cmd/verify-adapter-pins iterates PinnedModules to assert each
//     pinned go.mod still resolves github.com/boundaryml/baml to its
//     canonical version after `scripts/sync.sh` runs.
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
	// BAMLVersion is the canonical github.com/boundaryml/baml pin for
	// this adapter's go.mod, e.g. "v0.204.0". The pin verifier reads
	// this to assert that `go list -m github.com/boundaryml/baml`
	// inside adapters/<DirName> still returns this exact version
	// after `scripts/sync.sh` runs.
	BAMLVersion string
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
		DirName:     "adapter_v0_204_0",
		BAMLVersion: "v0.204.0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0",
			SupportsWithClient: false,
			HasWrapMapValues:   true,
			HasHTTPClient:      false,
		},
	},
	{
		DirName:     "adapter_v0_215_0",
		BAMLVersion: "v0.215.0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_215_0",
			SupportsWithClient: false,
			HasWrapMapValues:   false,
			HasHTTPClient:      false,
		},
	},
	{
		DirName:     "adapter_v0_219_0",
		BAMLVersion: "v0.219.0",
		Options: codegen.Options{
			SelfPkg:            "github.com/invakid404/baml-rest/adapters/adapter_v0_219_0",
			SupportsWithClient: true,
			HasWrapMapValues:   false,
			HasHTTPClient:      true,
		},
	},
}

// PinnedModule is one (DirName, BAMLVersion) pair that the pin
// verifier asserts against. PinnedModules is the union of
// adapters/common (which is not a FrameworkAdapter — it has no
// codegen.Options matrix — but does pin a BAML version that must not
// drift) and the FrameworkAdapters set.
type PinnedModule struct {
	// DirName is the path relative to the repo root, e.g.
	// "adapters/common" or "adapters/adapter_v0_204_0". The verifier
	// resolves it under repoRoot to run `go list -m`.
	DirName string
	// BAMLVersion is the canonical github.com/boundaryml/baml pin
	// expected for the module at DirName.
	BAMLVersion string
}

// PinnedModules is the canonical pin matrix consumed by
// cmd/verify-adapter-pins. The common module pins the lowest adapter
// BAML version (v0.204.0) so that the local `replace ../common`
// directives in each adapter's go.mod can satisfy v0.204.0,
// v0.215.0, and v0.219.0 without pulling any adapter forward.
var PinnedModules = []PinnedModule{
	{DirName: "adapters/common", BAMLVersion: "v0.204.0"},
	{DirName: "adapters/adapter_v0_204_0", BAMLVersion: "v0.204.0"},
	{DirName: "adapters/adapter_v0_215_0", BAMLVersion: "v0.215.0"},
	{DirName: "adapters/adapter_v0_219_0", BAMLVersion: "v0.219.0"},
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
