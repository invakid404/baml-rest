package bamlutils

import "context"

// De-BAML serving cutover S1 — the DIRECT-PARSE observation seam.
//
// # Why this exists
//
// The cutover's telemetry contract enumerates five serving surfaces, and
// `direct_parse` — the `/parse/{method}` endpoint — is one of them. The other four
// run through native admission, so they emit their own surface/cohort/phase/winner
// accounting. This one does not: worker/parse.go invokes BAML's `method.Impl` /
// `method.StreamImpl` directly, and there is no native parse implementation to
// decline. Without a seam, the surface would be the one endpoint class with no
// per-request evidence of who owns it — which is exactly what a rollout dashboard
// must not have.
//
// So the parse route reports each request to an OPTIONAL observer. The observer is
// neutral by construction: this package is in the zero-nanollm host graph and must
// stay that way, so the seam is a plain func type here and the nanollm-linked
// implementation is injected by a native-capable worker (nativeserve.NewDirectParseObserve).
//
// # What it is NOT
//
// It is NOT a native parse path, and it cannot become one by accident. The observer
// returns nothing the parse route acts on: `Parse` calls it and then parses exactly
// as it would have, whatever the observer does. It cannot claim, cannot decline,
// cannot substitute a result, and cannot fail the request — a panic inside it is
// contained by the caller.
//
// A native direct-parse CLAIM does now exist, but not here and not through this
// seam: worker/parse.go carries a native-first bridge for the DYNAMIC final parse
// that runs the injected DeBAMLParseFunc and then serves its bytes only when a
// same-input BAML parse produced the identical bytes. That bridge has its own
// gate, its own counter and its own transition oracle; this observer neither feeds
// it nor learns anything from it, and remains a pure telemetry sink for the
// surface as a whole.
//
// With BAML_REST_USE_DEBAML off, a worker installs no observer at all, so the parse
// route calls nothing and the surface reports nothing — the same zero-native-observation
// property the other four lanes have when the flag is off.

// NativeDirectParseObservation is the whole, bounded fact the parse route reports.
// It carries NO method name, NO raw input, NO parsed output and NO options — the
// method name in particular is prohibited as a metric label, and the observer is a
// telemetry sink, so there is nothing here for it to leak.
type NativeDirectParseObservation struct {
	// Stream reports whether this was a parse-STREAM request (BAML ParseStream over
	// an accumulated prefix) rather than a final parse. It is a bounded boolean and
	// the only shape distinction the surface makes.
	Stream bool
}

// NativeDirectParseObserveFunc observes ONE direct `/parse/{method}` request. It is
// installed only by a native-capable worker with the umbrella flag on, and it is
// advisory in the strictest sense: its return value is nothing, and the parse route
// ignores everything about it except that it was called.
type NativeDirectParseObserveFunc func(ctx context.Context, obs NativeDirectParseObservation)
