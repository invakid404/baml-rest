package hacks

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Hack defines the interface for a code transformation hack.
// Hacks are applied to generated BAML client code to fix issues
// or work around bugs in specific BAML versions.
type Hack interface {
	// Name returns a human-readable name for the hack.
	Name() string

	// MinVersion returns the minimum BAML version this hack applies to.
	// Empty string means no minimum (applies to all versions from the start).
	MinVersion() string

	// MaxVersion returns the maximum BAML version this hack applies to.
	// Empty string means no maximum (applies to all future versions).
	MaxVersion() string

	// Apply applies the hack to the generated BAML client code.
	// bamlClientDir is the path to the generated baml_client directory.
	Apply(bamlClientDir string) error
}

// registry holds all registered hacks.
var registry []Hack

// Register adds a hack to the registry.
func Register(h Hack) {
	registry = append(registry, h)
}

// ApplyAll applies all applicable hacks for the given BAML version.
func ApplyAll(bamlVersion, bamlClientDir string) error {
	// Normalize version to semver format (prepend 'v' if needed)
	version := bamlutils.NormalizeVersion(bamlVersion)

	for _, hack := range registry {
		if !isApplicable(hack, version) {
			fmt.Printf("Skipping hack %q (not applicable for version %s)\n", hack.Name(), bamlVersion)
			continue
		}

		fmt.Printf("Applying hack %q...\n", hack.Name())
		if err := hack.Apply(bamlClientDir); err != nil {
			return fmt.Errorf("hack %q failed: %w", hack.Name(), err)
		}
		fmt.Printf("Hack %q applied successfully\n", hack.Name())
	}

	return nil
}

// isApplicable checks if a hack should be applied for the given version.
func isApplicable(h Hack, version string) bool {
	minVersion := h.MinVersion()
	maxVersion := h.MaxVersion()

	// Check minimum version constraint
	if minVersion != "" {
		if bamlutils.CompareVersions(version, minVersion) < 0 {
			return false
		}
	}

	// Check maximum version constraint
	if maxVersion != "" {
		if bamlutils.CompareVersions(version, maxVersion) > 0 {
			return false
		}
	}

	return true
}
