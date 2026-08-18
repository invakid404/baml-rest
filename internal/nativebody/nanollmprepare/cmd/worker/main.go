// Command worker is the BAML+nanollm subprocess worker and the native SERVE deploy
// profile: with the umbrella flag ON it installs the native serve factories the
// generated seams use and can actually SERVE an admitted request natively — one
// exact provider RoundTrip, native translate/extract/parse, the pre-socket BAML
// plan-compare precondition + (on the unary lanes) the same-response BAML-parse
// safety compare, and the native final returned through the merged Slice-1
// tryOneChild seam. Unsupported traffic declines PRE-SOCKET and BAML serves it on
// the same instance.
//
// WHICH SURFACES IT WIRES. This header once described a unary-only profile, and the
// Options literal below has since grown the dynamic STREAM, static unary SERVE and
// static STREAM factories alongside the original dynamic unary one (plus the static
// no-send observer). The factory wiring below — not this paragraph — is the
// authority; it now installs, under the flag: dynamic unary serve, dynamic stream
// serve, static observe, static unary serve, static stream serve, and the
// direct-parse observation sink (a telemetry sink, not a serving callback — /parse
// stays BAML-served).
//
// WHAT IT SERVES TODAY. Installing a factory is CAPABILITY, not enrollment. Since
// the serving-cutover S1 slice, admission additionally requires an ENROLLED
// surface/cohort pair before any native work, and the shipped policy
// (admission.ProductionCohortGate) enrolls NOTHING — so every one of the surfaces
// above declines pre-socket with `cohort_not_enrolled` and BAML serves 100% of
// traffic on this binary. Enrollment is a separate, reviewed change to that policy;
// the umbrella flag below remains the one global revert.
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
		workerboot.Run(flagOffProfileOptions())
		return
	}

	workerboot.Run(serveProfileOptions())
}

// flagOffProfileOptions is the FLAG-OFF options literal: a static build-capability
// advertisement and NOTHING else — no FFI, no serve factory, no observer, no native
// runtime init. Extracted alongside serveProfileOptions so a test can assert the
// zero-native property by inspecting the options themselves rather than inferring it
// from a boot log.
func flagOffProfileOptions() workerboot.Options {
	return workerboot.Options{
		// Static build fact (no FFI): report the linked engine so the startup
		// diagnostic shows native_build_capable=true, runtime uninitialized,
		// rollout_mode=off, native_serving=off.
		NativeBuildCapable: true,
		NativeEngineName:   nativeworker.EngineName,
	}
}

// serveProfileOptions is the FLAG-ON options literal: every native factory this
// binary installs, in one place.
//
// It is a function rather than an inline literal so the wiring is reachable from a
// test. A cold review demonstrated why that matters: it deleted the direct-parse
// factory line from this literal and every committed test stayed green, because the
// only proofs were of the factory and of the parse route SEPARATELY. The literal is
// the join between them, so the join needs to be something a test can hold —
// direct_parse_route_e2e_test.go asserts this function supplies the factory and then
// drives the resulting observer through the real parse handler.
func serveProfileOptions() workerboot.Options {
	return workerboot.Options{
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
		// The STATIC no-send admission OBSERVER factory (de-BAML Slice 8B): returns the
		// neutral bamlutils.NativeStaticObserveFunc the generated static observe seam
		// installs. For an eligible static method it runs the full pre-socket admission
		// predicate (descriptor -> args -> native render/Prepare -> BAML plan compare)
		// and ALWAYS declines pre-socket to BAML — observe-only, zero serving behaviour
		// change. Installed ONLY under the umbrella flag (this whole branch is skipped
		// when the flag is off), so the static observer is hard-off and 100% BAML then.
		NativeStaticObserveFactory: nativeserve.NewStaticObserve,
		// The STATIC SERVE factory (de-BAML Slice 8C): returns the neutral
		// bamlutils.NativeStaticServeFunc the generated static /call seam installs — it
		// actually SERVES an admitted static unary /call natively (one exact RoundTrip,
		// native static SAP over the selected Return Bundle, the S5 same-response BAML
		// parse safety compare) or declines PRE-SOCKET to BAML. It reuses the SAME serve
		// core as nativeserve.New. Installed ALONGSIDE the observer in the serve profile;
		// the generated static /call seam PREFERS the serve callback when present
		// (mirroring the dynamic serve-over-shadow precedence), so the observer is the
		// no-send fallback for a shadow/observe-only build. Skipped when the flag is off.
		NativeStaticServeFactory: nativeserve.NewStaticServe,
		// The STATIC STREAM SERVE factory (de-BAML Phase 3b): returns the neutral
		// bamlutils.NativeStaticStreamServeFunc the generated static /stream{,-with-raw}
		// seam installs as StreamConfig.NativeAttempt — it actually SERVES an admitted
		// static stream natively (one exact RoundTrip driving nanollm DoStream over the
		// selected Return Bundle, the native-only partial/final parsers owned by the
		// orchestrator) or declines PRE-TRANSPORT to BAML. Installed ALONGSIDE the unary
		// static serve factory (both live in the serve profile) and mirrors the
		// dynamic serve/stream factory pair. Skipped when the flag is off.
		NativeStaticStreamServeFactory: nativeserve.NewStaticStream,
		// The DIRECT-PARSE observation sink (de-BAML serving cutover S1): returns the
		// neutral bamlutils.NativeDirectParseObserveFunc the worker's /parse route
		// reports each request to. It is NOT a serving callback — it observes and
		// records, and BAML parses the request exactly as before — but it is what
		// makes `direct_parse`, the one surface that never reaches native admission,
		// emit the same per-request "BAML owns this" evidence the other four do.
		// Skipped when the flag is off, like everything else in this branch.
		NativeDirectParseObserveFactory: nativeserve.NewDirectParseObserve,
	}
}
