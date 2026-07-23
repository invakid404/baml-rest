// Command worker-shadow is the de-BAML cutover Slice 4 SHADOW deploy profile: a
// BAML+nanollm subprocess worker that ALSO installs the native one-send shadow
// comparator. It is the shadow twin of the sibling cmd/worker (the S2
// native-capable-but-unrouted worker): both build from the out-of-go.work
// nanollmprepare module with GOWORK=off + CGO so the nanollm static archive links
// into them, and both delegate startup to internal/workerboot; this binary
// additionally supplies the NativeShadowFactory.
//
// What the shadow profile does (only while BAML_REST_USE_DEBAML is enabled):
// for each admitted unary `_dynamic` call the orchestrator's native child-attempt
// callback runs the S3 admission, builds the native request plan, obtains BAML's
// built plan for the SAME child WITHOUT sending, compares method/target/host/
// body/header-semantics, records baml_rest_debaml_plan_compare_total{result,field}
// on the worker's private registry (NO values), and then DECLINES so BAML serves
// the request and returns its own envelope, byte-identical. Native NEVER opens a
// socket / RoundTrips. Flag-off is zero native: no plan build, no FFI, no socket.
//
// It is a SEPARATE deployment revision/cohort, NOT a second application flag: the
// single umbrella flag still decides all-BAML vs run-the-comparator, and this
// build simply installs the comparator that a default build omits. The DEFAULT
// production worker (root cmd/worker, BAML-only) and the S2 native worker both
// leave the comparator nil, so they serve 100% BAML with the callback hard-off.
package main

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/nativeworker"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/shadow"
	"github.com/invakid404/baml-rest/internal/workerboot"
	"github.com/invakid404/baml-rest/nativeserve"
)

func main() {
	// Resolve the umbrella flag FIRST, BEFORE any native wiring is evaluated. The
	// flag-off shadow build must be ZERO native: no nanollm FFI at boot (no
	// capability probe, no runtime init), no plan build, no socket — identical to
	// the BAML-only worker, so a flag flip is an immediate, total reversal.
	//
	// NewCapability() (nanollm.Version) and ProbeRuntime (nanollm.New) are the two
	// boot-time FFI touch points, so they are constructed ONLY inside the enabled
	// branch. With the flag off we hand workerboot a zero Options — the shadow
	// binary then behaves exactly like cmd/worker (BAML-only), executing no nanollm
	// FFI even though the archive is linked.
	if !bamlutils.DeBAMLConfigFromEnv().Enabled {
		workerboot.Run(workerboot.Options{})
		return
	}

	workerboot.Run(workerboot.Options{
		// Native capability + startup init, exactly like the S2 native worker: a
		// present capability is reported at startup and the nanollm runtime is
		// proven to come up alongside BAML before the handler serves.
		NativeCapability: nativeworker.NewCapability(),
		NativeInit:       nativeworker.ProbeRuntime,
		// The shadow comparator factory: registers the bounded de-BAML collectors
		// on the worker's private registry and returns the neutral
		// bamlutils.NativeShadowFunc the generated dynamic call seam installs as
		// the Slice-1 native child-attempt callback.
		NativeShadowFactory: shadow.NewShadowFunc,
		// The STATIC Stage-1 SHADOW factory (de-BAML Slice 8C): returns the neutral
		// bamlutils.NativeStaticShadowFunc the generated static /call seam installs when
		// no serve callback is present. For an admitted static /call it runs the no-send
		// admission + plan compare and, on a match, compares native's parse of BAML's
		// captured bytes against BAML's parse — BAML stays the SOLE sender, ZERO native
		// sends — then declines so BAML serves. Installed ALONGSIDE the dynamic shadow in
		// the shadow profile; skipped entirely when the umbrella flag is off.
		NativeStaticShadowFactory: nativeserve.NewStaticShadow,
	})
}
