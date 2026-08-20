//go:build !nativeworkerartifact

package main

import (
	"testing"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// TestDefaultBuildIsTheBAMLOnlyArtifact pins the untagged half of the tag-split
// constant. A build with no `nativeworkerartifact` tag embeds no isolated-module
// worker, so it must attest BAML-only; if this constant were true by default,
// every hand-built serve binary — and every deploy path this repository does not
// know about — would claim to be the standard S2 artifact without carrying one.
func TestDefaultBuildIsTheBAMLOnlyArtifact(t *testing.T) {
	if hostEmbeddedWorkerNativeCapable {
		t.Fatal("hostEmbeddedWorkerNativeCapable is true in the untagged build")
	}
	if got := artifactprofile.DeriveProfile(hostEmbeddedWorkerNativeCapable); got != artifactprofile.ProfileBAMLOnly {
		t.Fatalf("untagged build derives profile %q, want %q", got, artifactprofile.ProfileBAMLOnly)
	}
}
