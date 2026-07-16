// Command gen-nativeworker-src regenerates the committed opaque tar of the
// isolated internal/nativebody/nanollmprepare module (de-BAML cutover Slice 2).
//
// Run it from the repo root whenever the isolated module's production source
// changes:
//
//	go run ./cmd/build/gen-nativeworker-src
//
// The freshness test (cmd/build/nativeworker_src_test.go) fails the default CI
// build if the committed tar drifts from the live module tree, pointing back
// here. See package nativeworkersrc for why the module ships as an opaque asset
// rather than through the normal embed path.
package main

import (
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/cmd/build/nativeworkersrc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-nativeworker-src: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Resolve the repo root: this generator is meant to run from it, so CWD is
	// the root. Validate by checking every packaged module's go.mod is present.
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, modRel := range nativeworkersrc.ModuleRelPaths {
		if _, err := os.Stat(modRel + "/go.mod"); err != nil {
			return fmt.Errorf("run from the repo root: %s/go.mod not found (%w)", modRel, err)
		}
	}

	data, err := nativeworkersrc.BuildTar(repoRoot)
	if err != nil {
		return err
	}
	if err := os.WriteFile(nativeworkersrc.TarRelPath, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", nativeworkersrc.TarRelPath, len(data))
	return nil
}
