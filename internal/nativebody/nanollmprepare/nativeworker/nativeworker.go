// Package nativeworker is the nanollm-backed native-send capability for the
// isolated production worker (de-BAML cutover Slice 2). It lives in the
// out-of-go.work nanollmprepare module and is UNTAGGED production code: it is
// compiled into the BAML+nanollm worker binary (built with GOWORK=off + CGO),
// and by importing github.com/viktordanov/nanollm-ffi/go it is what actually
// links the nanollm static archive into that binary. Because the module is
// outside go.work and absent from the root go.mod/go.sum, nothing here can ever
// enter the host/root link graph — the host stays zero-nanollm and CGO-free.
//
// The capability it produces is neutral (worker.NativeCapability): it reports
// the engine identity/version for the startup diagnostic. In this slice it is
// stored on the handler but NOT wired to the orchestrator's native
// child-attempt callback (which stays nil/hard-off), so a native-capable worker
// still serves 100% BAML. The Prepare/translate/exact-attempt pipeline the
// capability will eventually drive already lives, untagged, in the sibling
// execute package (execute.RunAttempt).
package nativeworker

import (
	"fmt"

	nanollm "github.com/viktordanov/nanollm-ffi/go"

	"github.com/invakid404/baml-rest/worker"
)

// EngineName is the stable, secret-free identifier of the linked native engine.
const EngineName = "nanollm"

// capability is the nanollm-backed worker.NativeCapability. It carries only the
// engine version string resolved at construction; it holds no client, secret,
// or per-request state (a present capability is a build fact, not a session).
type capability struct {
	version string
}

// Compile-time assertion that the nanollm-backed capability satisfies the
// neutral host-side interface. Catches drift between worker.NativeCapability
// and this implementation at build time.
var _ worker.NativeCapability = capability{}

func (c capability) NativeEngine() string        { return EngineName }
func (c capability) NativeEngineVersion() string { return c.version }

// NewCapability returns the nanollm-backed native-send capability. It resolves
// the linked engine's version via the FFI (which forces the archive to link and
// surfaces a gross link failure here), and reports it for the startup
// diagnostic. It performs no network I/O and constructs no engine — that is
// ProbeRuntime's job at startup.
func NewCapability() worker.NativeCapability {
	return capability{version: nanollm.Version()}
}

// ProbeRuntime initializes the nanollm runtime once, proving it comes up
// alongside BAML at worker startup before any request is served. It builds a
// throwaway engine from a fully-offline, structurally-valid config (a single
// fake OpenAI model — no reachable host, no real credential) and immediately
// frees it. nanollm.New validates the whole config and constructs the engine
// WITHOUT opening any socket, so this exercises the native allocator/ABI path
// with zero network I/O; a non-nil error means the native runtime failed to
// initialize and the caller should fail worker startup loudly.
func ProbeRuntime() error {
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			// A structurally-valid but deliberately unreachable OpenAI model.
			// The ".invalid" TLD (RFC 6761) can never resolve, and the key is
			// an obvious placeholder — no probe request is ever sent, so
			// neither value leaves the process.
			Name:    "native-capability-startup-probe",
			Model:   "openai/gpt-4o-mini",
			APIKey:  "sk-native-capability-startup-probe",
			BaseURL: "http://native-capability-startup-probe.invalid/v1",
		}},
	})
	if err != nil {
		return fmt.Errorf("nativeworker: nanollm startup probe failed: %w", err)
	}
	return c.Close()
}
