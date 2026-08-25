package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/internal/nativeschema"
	"github.com/invakid404/baml-rest/internal/nativespine"
)

// nativeSourceFacts assembles the whole-project nativespine.SourceFacts from the
// already-parsed config: the per-function prompt descriptors/declines/features
// plus the client graph and template set read (from the same retained AST) by
// nativeschema. It is the M2 seam that turns cmd/introspect's parse into the
// whole-project descriptor input.
func (cfg *bamlConfig) nativeSourceFacts() nativespine.SourceFacts {
	clients, retries, strategies := nativeschema.BuildClientGraph(cfg.parsedFiles)
	return nativespine.SourceFacts{
		Funcs:              cfg.staticPromptDescriptors,
		PreDeclines:        cfg.staticPromptDeclines,
		PreDeclineFeatures: cfg.staticPromptDeclineFeatures,
		Clients:            clients,
		RetryPolicies:      retries,
		Strategies:         strategies,
		Templates:          nativeschema.BuildProjectTemplates(cfg.parsedFiles),
	}
}

// emitNativeSpineDescriptors implements the experimental
// --native-spine-descriptors mode (codegen-spine M1+M2). It reuses the same .baml
// walk the normal path uses (parseBamlSourceDir), then — UNLIKE the best-effort
// generated lane — applies STRICT diagnostics (M2, §1.3): it fails generation on
// any unreadable/invalid source or duplicate/ambiguous declaration / unresolved
// reference rather than silently skipping it. It then builds the whole-project
// neutral projectdescriptor.Project and writes it as JSON. It does NOT emit
// introspected.go — the normal path is untouched.
//
// It returns an error on any failure rather than calling os.Exit, so tests can
// invoke it directly (a fatal exit would kill the test binary and skip the rest
// of the package). The main.go call site turns a returned error into the process
// exit.
func emitNativeSpineDescriptors(cfg *config) error {
	bc := parseBamlSourceDir(cfg.BAMLSourceDir)

	// Strict diagnostics: a native build must fail on invalid retained source.
	if len(bc.parseDiagnostics) > 0 {
		return fmt.Errorf("native-spine strict source diagnostics: %w", errors.Join(bc.parseDiagnostics...))
	}
	if err := nativeschema.CheckProjectIntegrity(bc.parsedFiles); err != nil {
		return fmt.Errorf("native-spine strict source diagnostics: %w", err)
	}

	proj := nativespine.BuildProjectDescriptor(bc.nativeSourceFacts())
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
