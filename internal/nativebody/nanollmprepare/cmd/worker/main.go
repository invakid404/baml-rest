// Command worker is the BAML+nanollm subprocess worker (de-BAML cutover Slice
// 2). It is the isolated twin of the root cmd/worker (the BAML-only worker):
// both delegate startup to the shared internal/workerboot bootstrap, so their
// behaviour is identical apart from the native capability injected here.
//
// This binary is built FROM the out-of-go.work nanollmprepare module with
// GOWORK=off + CGO so the nanollm static archive links into it (via the
// nativeworker import) while the root/host module graph stays zero-nanollm and
// CGO-free. cmd/build/build.sh builds it and drops it at cmd/serve/worker (the
// host embed location) only under the opt-in NATIVE_WORKER variant; the default
// build keeps the BAML-only worker, preserving "BAML-only worker = 100% BAML"
// as an immediate build-level reversal.
//
// It links nanollm but does NOT route to it: the injected capability is stored
// and reported at startup, while the orchestrator's native child-attempt
// callback stays nil/hard-off. Serving behaviour is byte-identical to the
// BAML-only worker in this slice.
package main

import (
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeworker"
	"github.com/invakid404/baml-rest/internal/workerboot"
)

func main() {
	workerboot.Run(workerboot.Options{
		// Presence of a non-nil capability is the build capability the startup
		// diagnostic reports. NativeInit proves the nanollm runtime initializes
		// alongside BAML before the handler serves; a failure exits non-zero and
		// fails the go-plugin handshake.
		NativeCapability: nativeworker.NewCapability(),
		NativeInit:       nativeworker.ProbeRuntime,
	})
}
