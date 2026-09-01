// Command worker-nativeonly is the ExecBridge-U1b NATIVE-ONLY packaged worker: a
// subprocess worker that boots and serves the ExecBridge-U1 exact-JSON cohort with
// ZERO BAML/CFFI in its runtime graph. It is the deletion substrate — as the
// admitted registry grows to cover every retained method, deleting the full-BAML
// build branch becomes a packaging deletion, not another bootstrap migration.
//
// It is built FROM the out-of-go.work nanollmprepare module with GOWORK=off + CGO
// (so the nanollm static archive links via the nativeworker import) and with the
// debamlnativeonlygenerated build tag (so the generated registry — emitted at build
// time by cmd/gen-native-spine-worker from the deployment's own introspected
// descriptor — is the real nativegenerated.NewRuntime). cmd/build/build.sh selects
// it under the single --native-only-worker / BAML_REST_NATIVE_ONLY_WORKER gate and
// drops it at cmd/serve/worker for the host to embed.
//
// nativegenerated.NewRuntime() is this command's ROOT-RUNTIME SELECTION: there is
// no nil/default path to a generated BAML runtime and no BAML fallback. It does NOT
// import internal/workerboot, internal/rootruntime, introspected, the root baml_rest
// package, dynclient, generated baml_client, language_client_go, or BoundaryML —
// proven by the whole-command dependency gate. It does NOT read BAML_REST_USE_DEBAML:
// artifact selection is the one all-on/all-off gate, and a native-only binary cannot
// safely turn itself into an all-decline worker at runtime with no BAML fallback.
package main

import (
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativegenerated"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeonlyboot"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeworker"
)

func main() {
	// The pure-Go native runtime built from the emitted registry (the classifier in
	// nativeserve/spine owns cohort membership). A generation/registry failure is a
	// fatal build error surfaced here — there is no BAML fallback to degrade to.
	rt, err := nativegenerated.NewRuntime()
	if err != nil {
		// Deferring to nativeonlyboot would log via hclog, but a nil runtime there is a
		// generic "nil runtime" message; surface the concrete generation error instead.
		panic(err)
	}
	nativeonlyboot.Run(rt, nativeworker.NewCapability(), nativeworker.ProbeRuntime)
}
