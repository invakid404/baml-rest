//go:build nativeworkerartifact

package main

import (
	"testing"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// TestNativeArtifactBuildIsNativeCapable pins the tagged half of the tag-split
// constant, and it is the reason build.sh's `nativeworkerartifact` tag and its
// `-ldflags` profile stamp cannot drift apart unnoticed: build.sh sets both from
// the same ARTIFACT_PROFILE variable, this test proves the tag maps to
// native_capable, and artifactprofile.Attest proves at startup that the stamp
// agrees with what the tag selected.
//
// CI builds this file via the -tags=nativeworkerartifact sanity step in
// .github/workflows/unit-tests.yml; without that step the tagged variant would
// never be compiled and this assertion would be dead code.
func TestNativeArtifactBuildIsNativeCapable(t *testing.T) {
	if !hostEmbeddedWorkerNativeCapable {
		t.Fatal("hostEmbeddedWorkerNativeCapable is false under -tags=nativeworkerartifact")
	}
	if got := artifactprofile.DeriveProfile(hostEmbeddedWorkerNativeCapable); got != artifactprofile.ProfileNativeCapable {
		t.Fatalf("tagged build derives profile %q, want %q", got, artifactprofile.ProfileNativeCapable)
	}
}
