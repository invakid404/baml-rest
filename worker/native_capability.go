package worker

// NativeCapability is a neutral, opaque handle to a worker binary's linked
// native-send engine (de-BAML cutover Slice 2). Its PRESENCE is a BUILD
// capability — the worker was compiled with a native engine linked — not a
// runtime flag. The concrete implementation lives OUTSIDE the host/root link
// graph, in the isolated out-of-go.work nanollmprepare worker module; the
// worker package holds it only through this neutral interface so neither the
// host nor the worker package ever imports the native engine. This mirrors
// SharedStateHook: a worker-owned neutral interface whose implementation is
// injected at the binary entry point.
//
// In this slice the capability is STORED and REPORTED at startup but is NOT
// wired to the orchestrator's native child-attempt seam
// (buildrequest.CallConfig.NativeAttempt stays nil/hard-off). A later slice
// turns a present capability into an installed, enabled NativeCallAttemptFunc.
// There is deliberately no adapter setter here: unlike the render/parser
// callbacks the capability never touches the generated adapter, so it lives on
// the Handler alone (storage + getter).
type NativeCapability interface {
	// NativeEngine is a stable, secret-free identifier of the linked native
	// engine (e.g. "nanollm"). Used for the startup capability diagnostic and,
	// in a later slice, to tag the winner engine on metrics. Never a secret.
	NativeEngine() string

	// NativeEngineVersion is the linked engine's version string, secret-free.
	// Empty when the engine exposes no version. Reported at startup so a
	// deployed worker's native-engine build provenance is observable.
	NativeEngineVersion() string
}
