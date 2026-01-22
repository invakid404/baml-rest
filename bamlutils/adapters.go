package bamlutils

import (
	"embed"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

const AdapterPrefix = "adapters/adapter_v"

// AdapterInfo contains information about a selected adapter.
type AdapterInfo struct {
	// Version is the semver version string (e.g., "v0.215.0")
	Version string
	// Path is the full path in the Sources map (e.g., "adapters/adapter_v0_215_0")
	Path string
}

// GetAdapterForBAMLVersion returns the appropriate adapter for a given BAML version.
// It finds the highest adapter version that is <= the target BAML version.
func GetAdapterForBAMLVersion(sources map[string]embed.FS, bamlVersion string) (*AdapterInfo, error) {
	var availableVersions []string
	versionToPath := make(map[string]string)

	for key := range sources {
		if !strings.HasPrefix(key, AdapterPrefix) {
			continue
		}

		// Convert "adapters/adapter_v0_204_0" to "v0.204.0"
		version := "v" + strings.ReplaceAll(strings.TrimPrefix(key, AdapterPrefix), "_", ".")
		availableVersions = append(availableVersions, version)
		versionToPath[version] = key
	}

	if len(availableVersions) == 0 {
		return nil, fmt.Errorf("no adapter versions found in sources")
	}

	semver.Sort(availableVersions)

	// Normalize BAML version to semver format
	targetVersion := bamlVersion
	if !strings.HasPrefix(targetVersion, "v") {
		targetVersion = "v" + targetVersion
	}

	// Find the highest adapter version that's <= the target BAML version
	var selectedVersion string
	for _, version := range slices.Backward(availableVersions) {
		if semver.Compare(targetVersion, version) >= 0 {
			selectedVersion = version
			break
		}
	}

	if selectedVersion == "" {
		return nil, fmt.Errorf(
			"BAML version %q is unsupported, the minimum supported version is %q",
			bamlVersion, strings.TrimPrefix(availableVersions[0], "v"),
		)
	}

	return &AdapterInfo{
		Version: selectedVersion,
		Path:    versionToPath[selectedVersion],
	}, nil
}
