//go:build !debamlnativeonlygenerated

// Package nativegenerated is the DEPLOYMENT-SPECIFIC native-only worker registry
// for the ExecBridge-U1b packaged worker. Its real implementation is emitted at
// container-build time by cmd/gen-native-spine-worker (from the project's own
// introspected descriptor) and compiled under the debamlnativeonlygenerated build
// tag; cmd/build/build.sh always builds the native-only artifact with that tag.
//
// This file is the committed stub that keeps a PLAIN source checkout compiling
// (no generated registry present) while making a native-only worker that was NOT
// generated fail LOUD rather than silently serving a fixture method. It uses NO
// dynclient, baml_client, staticworkerruntime, or any BAML package: there is no
// BAML fallback in the native-only artifact.
package nativegenerated

import (
	"errors"

	"github.com/invakid404/baml-rest/worker"
)

// ErrRuntimeNotGenerated is the fail-loud sentinel returned when the native-only
// worker was built without generating its deployment-specific registry. The build
// pipeline generates the registry and builds with -tags=debamlnativeonlygenerated,
// so a returned error here means the artifact is misbuilt and must fail startup.
var ErrRuntimeNotGenerated = errors.New("nativegenerated: native runtime was not generated (run cmd/gen-native-spine-worker and build with -tags=debamlnativeonlygenerated)")

// NewRuntime always fails on a plain source build. The generated implementation
// (debamlnativeonlygenerated) decodes the embedded descriptor and builds the
// immutable admitted native runtime via nativeserve/spine.NewWorkerRuntime.
func NewRuntime() (worker.Runtime, error) {
	return nil, ErrRuntimeNotGenerated
}
