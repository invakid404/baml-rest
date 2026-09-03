//go:build !debamlnativespinegenerated

// Package nativegenerated is the DEPLOYMENT-SPECIFIC native spine worker registry for
// the ExecBridge packaged workers. Its real implementation is emitted at container-build
// time by cmd/gen-native-spine-worker (from the project's own introspected descriptor)
// and compiled under the debamlnativespinegenerated build tag; cmd/build/build.sh builds
// BOTH the native-only artifact AND the standard native-capable serve worker with that
// tag, so the tag name is profile-NEUTRAL (a native spine registry, not a native-only
// one).
//
// This file is the committed stub that keeps a PLAIN source checkout compiling (no
// generated registry present) while making a worker that was NOT generated fail LOUD
// rather than silently serving a fixture method or degrading to all-BAML. It uses NO
// dynclient, baml_client, staticworkerruntime, or any BAML package: there is no BAML
// fallback in the native-only artifact, and the standard composite must fail if
// generation was expected but only this stub is linked.
package nativegenerated

import (
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/worker"
)

// ErrRuntimeNotGenerated is the fail-loud sentinel returned when a worker was built
// without generating its deployment-specific spine registry. The build pipeline
// generates the registry and builds with -tags=debamlnativespinegenerated, so a returned
// error here means the artifact is misbuilt and must fail startup — never a silent
// degrade to all-BAML on the standard composite.
var ErrRuntimeNotGenerated = errors.New("nativegenerated: native spine registry was not generated (run cmd/gen-native-spine-worker and build with -tags=debamlnativespinegenerated)")

// NewRuntime always fails on a plain source build. The generated implementation
// (debamlnativespinegenerated) decodes the embedded descriptor and builds the immutable
// admitted native-only runtime via nativeserve/spine.NewWorkerRuntime.
func NewRuntime() (worker.Runtime, error) {
	return nil, ErrRuntimeNotGenerated
}

// NewExecutor always fails on a plain source build. The generated implementation
// (debamlnativespinegenerated) decodes the embedded descriptor and builds the
// population-filtered oracle-capable executor via nativeserve/spine.NewPopulationExecutor
// (empty allowed) that the ExecBridge-U1c standard composite drives. A returned error is
// the fail-loud production-registry guard: only a successfully generated (possibly empty)
// population may yield an all-decline executor.
func NewExecutor() (bamlutils.NativeSpineUnaryOracleExecutor, error) {
	return nil, ErrRuntimeNotGenerated
}
