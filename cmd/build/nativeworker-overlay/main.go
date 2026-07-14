// Command nativeworker-overlay applies the build-time go.mod overlay to the
// extracted isolated nanollm worker module so it can resolve the builder's
// generated baml_client and use the build's selected/custom BAML under
// GOWORK=off (de-BAML cutover Slice 2). build.sh invokes it in the
// NATIVE_WORKER=true branch, after client generation and module extraction.
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
	flag.Parse()

	if *moduleDir == "" || *bamlClient == "" {
		fmt.Fprintln(os.Stderr, "nativeworker-overlay: --module-dir and --baml-client are required")
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
