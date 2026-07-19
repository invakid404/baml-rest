// Command worker is the BAML+nanollm subprocess worker and, as of de-BAML cutover
// Slice 6, the native SERVE deploy profile: on a native-capable worker with the
// umbrella flag ON it actually SERVES an admitted unary OpenAI `_dynamic` call
// natively — one exact provider RoundTrip, native translate/extract/parse, the S4
// plan-compare pre-socket precondition + the S5 same-response BAML-parse safety
// compare, and the native final returned through the merged Slice-1 tryOneChild
// seam. Unsupported traffic (streaming, non-openai, fallback/RR, call-with-raw,
// static, …) declines PRE-SOCKET and BAML serves it on the same instance.
//
// It is built FROM the out-of-go.work nanollmprepare module with GOWORK=off + CGO
// so the nanollm static archive links into it (via the nativeworker/canary
// imports) while the root/host module graph stays zero-nanollm and CGO-free.
// cmd/build/build.sh builds it and drops it at cmd/serve/worker under the opt-in
// NATIVE_WORKER variant; the default build keeps the BAML-only worker, preserving
// "BAML-only worker = 100% BAML" as an immediate build-level reversal.
//
// FLAG-FIRST, ZERO-NATIVE WHEN OFF: the umbrella flag is resolved BEFORE any
// native wiring is evaluated. With BAML_REST_USE_DEBAML falsy this binary executes
// NO nanollm FFI at boot (no capability Version probe, no runtime init, no serve
// factory), installs no serve callback, opens no socket, and serves 100% BAML —
// identical to the BAML-only worker, so a flag flip is an immediate, total
// reversal (the kill switch). It still advertises a STATIC build capability
// (native_build_capable=true, engine name from a compile-time constant) so the
// startup diagnostic is unambiguous without touching nanollm.
package main

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeworker"
	"github.com/invakid404/baml-rest/internal/workerboot"
	"github.com/invakid404/baml-rest/nativeserve"
)

func main() {
	// Resolve the umbrella flag FIRST. NewCapability() (nanollm.Version) and
	// ProbeRuntime (nanollm.New) are the two boot-time FFI touch points, so they
	// are constructed ONLY inside the enabled branch. With the flag off we hand
	// workerboot a static build-capability advertisement and NOTHING else — no
	// FFI, no serve factory, no native runtime init — so the serve-capable binary
	// behaves exactly like cmd/worker (BAML-only) even though the archive is linked.
	if !bamlutils.DeBAMLConfigFromEnv().Enabled {
		workerboot.Run(workerboot.Options{
			// Static build fact (no FFI): report the linked engine so the startup
			// diagnostic shows native_build_capable=true, runtime uninitialized,
			// rollout_mode=off, native_serving=off.
			NativeBuildCapable: true,
			NativeEngineName:   nativeworker.EngineName,
		})
		return
	}

	workerboot.Run(workerboot.Options{
		// Native capability + startup init: a present capability is reported at
		// startup and the nanollm runtime is proven to come up alongside BAML
		// before the handler serves.
		NativeCapability: nativeworker.NewCapability(),
		NativeInit:       nativeworker.ProbeRuntime,
		// The serve factory: registers the bounded de-BAML collectors on the
		// worker's private registry and returns the neutral bamlutils.NativeServeFunc
		// the generated dynamic call seam installs as the Slice-1 native
		// child-attempt callback — which actually serves admitted unary `_dynamic`
		// calls natively. This is the SAME public constructor an in-process dynclient
		// consumer passes to dynclient.WithNativeServeComparator (#624), so the
		// subprocess serve worker and the in-process path are at transport parity by
		// construction.
		NativeServeFactory: nativeserve.New,
		// The STREAM serve factory (de-BAML Phase 7D): returns the neutral
		// bamlutils.NativeStreamServeFunc the generated dynamic StreamRequest seam
		// installs as StreamConfig.NativeAttempt — which serves admitted dynamic OpenAI
		// `/stream{,-with-raw}/_dynamic` requests natively (one exact RoundTrip driving
		// nanollm DoStream) or declines pre-transport to BAML. Installed ALONGSIDE the
		// unary serve factory (both live in the serve profile); with the flag off this
		// whole branch is skipped, so the stream lane is hard-off and 100% BAML.
		NativeStreamServeFactory: nativeserve.NewStream,
	})
}
