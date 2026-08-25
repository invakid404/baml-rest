package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/internal/nativespine"
)

// emitNativeSpineDescriptors implements the experimental
// --native-spine-descriptors mode (codegen-spine M1). It reuses the exact same
// .baml walk the normal path uses (parseBamlSourceDir), builds the neutral
// projectdescriptor.Project from the already-computed prompt descriptors +
// declines, and writes it as JSON. It does NOT emit introspected.go — the normal
// path is untouched.
// emitNativeSpineDescriptors returns an error on any failure rather than calling
// os.Exit, so tests can invoke it directly (a fatal exit would kill the test
// binary and skip the rest of the package). The main.go call site turns a
// returned error into the process exit.
func emitNativeSpineDescriptors(cfg *config) error {
	bc := parseBamlSourceDir(cfg.BAMLSourceDir)
	proj := nativespine.BuildProjectDescriptor(bc.staticPromptDescriptors, bc.staticPromptDeclines, bc.staticPromptDeclineFeatures)
	if err := proj.Validate(); err != nil {
		return fmt.Errorf("native-spine descriptor invalid: %w", err)
	}

	data, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal native-spine descriptor: %w", err)
	}
	data = append(data, '\n')

	if cfg.NativeSpineDescriptors == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			return fmt.Errorf("write native-spine descriptor: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(cfg.NativeSpineDescriptors, data, 0o644); err != nil {
		return fmt.Errorf("write native-spine descriptor: %w", err)
	}
	return nil
}
