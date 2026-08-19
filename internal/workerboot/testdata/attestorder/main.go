// Command attestorder is a TEST FIXTURE for the de-BAML serving-cutover S2
// attestation-order rehearsal. It is not shipped and not reachable from any
// production build: it lives under testdata precisely so the go tool's wildcards
// never pick it up.
//
// It is the smallest possible native-capable worker: it advertises the static
// build capability (so workerboot derives the native_capable profile, exactly
// like a real flag-off native artifact) and supplies a NativeInit that does
// nothing but record THAT IT RAN, by creating the file named in
// WORKERBOOT_NATIVE_INIT_SENTINEL.
//
// That sentinel is the whole point. workerboot attests the artifact profile
// BEFORE it initializes the native runtime, so a binary whose build stamp
// contradicts what it is must refuse to serve without ever having initialized
// anything native. Both outcomes exit non-zero (a correctly-stamped fixture still
// has no go-plugin handshake to complete), so the exit code cannot tell them
// apart — the presence or absence of this file can.
package main

import (
	"os"

	"github.com/invakid404/baml-rest/internal/workerboot"
)

func main() {
	sentinel := os.Getenv("WORKERBOOT_NATIVE_INIT_SENTINEL")
	workerboot.Run(workerboot.Options{
		// A static build-capability advertisement: no FFI, but enough for
		// workerboot to derive the native_capable profile.
		NativeBuildCapable: true,
		NativeEngineName:   "attestorder-sentinel",
		NativeInit: func() error {
			if sentinel == "" {
				return nil
			}
			return os.WriteFile(sentinel, []byte("native init ran\n"), 0o644)
		},
	})
}
