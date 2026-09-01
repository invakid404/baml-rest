// Command nativeworker-overlay applies the build-time go.mod overlay to the
// extracted isolated nanollm worker module under GOWORK=off. It has TWO mutually
// exclusive modes:
//
//   - BAML (default): the full-worker overlay (de-BAML cutover Slice 2). It does
//     the generic missing-replace cleanup AND wires in the builder's generated
//     baml_client + the build's selected/custom BAML. build.sh invokes it in the
//     NATIVE_WORKER=true branch, after client generation and module extraction.
//   - native-only (--native-only): the ExecBridge-U1b overlay. It does ONLY the
//     generic missing-replace cleanup and SKIPS both BAML operations, so the
//     native-only worker's module graph never gains a baml_client or BAML require.
//
// The two modes are validated mutually exclusive here so a future build-script
// typo cannot silently inject BAML into the native-only artifact.
// See package nativeworkersrc (overlay.go) for the rationale.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/cmd/build/nativeworkersrc"
)

func main() {
	moduleDir := flag.String("module-dir", "", "path to the extracted isolated nanollm worker module")
	bamlClient := flag.String("baml-client", "", "generated baml_client dir (replace target; relative to --module-dir or absolute)")
	bamlVersion := flag.String("baml-version", "", "selected stock BAML version (e.g. v0.223.0)")
	customBAMLLib := flag.String("custom-baml-lib", "", "custom BAML Go library path replacing github.com/boundaryml/baml (relative to --module-dir or absolute)")
	nativeOnly := flag.Bool("native-only", false, "ExecBridge-U1b native-only overlay: only the missing-replace cleanup, NO baml_client/BAML wiring (mutually exclusive with --baml-client/--baml-version/--custom-baml-lib)")
	flag.Parse()

	if *moduleDir == "" {
		fmt.Fprintln(os.Stderr, "nativeworker-overlay: --module-dir is required")
		os.Exit(2)
	}

	if *nativeOnly {
		// The two modes are mutually exclusive: a native-only overlay must never be
		// handed BAML wiring flags, because the whole point of the artifact is a
		// BAML-free graph.
		if *bamlClient != "" || *bamlVersion != "" || *customBAMLLib != "" {
			fmt.Fprintln(os.Stderr, "nativeworker-overlay: --native-only is mutually exclusive with --baml-client/--baml-version/--custom-baml-lib (the native-only artifact links no BAML)")
			os.Exit(2)
		}
		if err := nativeworkersrc.ApplyNativeOnlyOverlay(*moduleDir); err != nil {
			fmt.Fprintf(os.Stderr, "nativeworker-overlay: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *bamlClient == "" {
		fmt.Fprintln(os.Stderr, "nativeworker-overlay: --baml-client is required in BAML overlay mode (or pass --native-only)")
		os.Exit(2)
	}

	if err := nativeworkersrc.ApplyOverlay(*moduleDir, nativeworkersrc.OverlayOptions{
		BAMLClientPath:    *bamlClient,
		BAMLVersion:       *bamlVersion,
		CustomBAMLLibPath: *customBAMLLib,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "nativeworker-overlay: %v\n", err)
		os.Exit(1)
	}
}
