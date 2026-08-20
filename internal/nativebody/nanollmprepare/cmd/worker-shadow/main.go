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
	// branch.
	if !bamlutils.DeBAMLConfigFromEnv().Enabled {
		workerboot.Run(flagOffProfileOptions())
		return
	}

	workerboot.Run(shadowProfileOptions())
}

// flagOffProfileOptions is the FLAG-OFF options literal: a STATIC build-capability
// advertisement and NOTHING else — no FFI, no comparator, no native runtime init.
//
// The static advertisement is not cosmetic. This binary IS a native-capable
// artifact: it links the nanollm archive, and the de-BAML serving-cutover S2 build
// stamps it `native_capable`. workerboot derives the running artifact's profile
// from exactly these two fields (plus a live NativeCapability), and refuses to
// serve when the derived profile contradicts the build stamp. Handing it a ZERO
// Options here — which is what this branch used to do — made it derive
// `baml_only`, contradict its own `native_capable` stamp, and exit BEFORE serving
// any BAML at all. That turned BAML_REST_USE_DEBAML=false, the one global kill
// switch, into a hard failure on this artifact. The advertisement is what keeps
// flag-off a total BAML revert rather than an outage.
//
// It still executes ZERO nanollm FFI: NativeBuildCapable/NativeEngineName are a
// compile-time bool and a compile-time constant string, whereas resolving a
// NativeCapability would call the engine's Version FFI. Extracted alongside
// shadowProfileOptions so the zero-native property is assertable from the options
// themselves rather than inferred from a boot log — the entrypoint guard in the
// root module reads BOTH functions.
func flagOffProfileOptions() workerboot.Options {
	return workerboot.Options{
		NativeBuildCapable: true,
		NativeEngineName:   nativeworker.EngineName,
	}
}

// shadowProfileOptions is the FLAG-ON options literal: every native factory this
// binary installs, in one place. A function rather than an inline literal for the
// same reason as the sibling serve worker's: the literal is the join between "a
// factory exists" and "this binary wires it", and a join needs to be something a
// test can hold.
func shadowProfileOptions() workerboot.Options {
	return workerboot.Options{
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
	}
}
