// Command artifactattest computes the reproducible de-BAML serving-cutover S2
// RELEASE ARTIFACT ID for a build selection and prints it on stdout.
//
// cmd/build/build.sh calls it once, then stamps the printed ID (and the
// artifact profile) into the worker and serve binaries with
// `-ldflags -X`. The single-purpose CLI exists so the ID is computed by the SAME
// Go code that validates it at startup (internal/artifactprofile), rather than
// by a second implementation in shell that could drift from the validator.
//
// The ID is a digest of the build SELECTION AXES (profile, worker package, build
// tags, subprocess mode, BAML version, adapter version) AND the artifact's
// PROVENANCE: the release revision, a content digest over the embedded source
// bundle this build was laid down from, and the content digest of the packaged
// native-worker tar — which this command computes itself from the build context,
// so the digest describes the tar that is actually about to be extracted and
// compiled rather than one the caller asserted.
//
// Provenance is what makes the ID identify a RELEASE. An axes-only digest
// collides across every release that selects the same axes, which is most of
// them; a cold review flagged exactly that.
//
// No timestamp, hostname, local path or working-tree state participates, so the
// same source built the same way always yields the same ID: a reviewer can
// recompute it from a build log and get a byte-identical answer.
//
// It prints TWO values, `artifact_id=` and `artifact_inputs=`, because the ID is
// VERIFIED at startup rather than trusted: build.sh stamps both, and
// artifactprofile.Attest re-derives the ID from the inputs and refuses a
// mismatch.
//
// Every flag is REQUIRED (except the deliberately-empty worker package of an
// in-process build) and strictly validated, so a mis-invocation fails the build
// instead of silently stamping a degenerate identity.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

func main() {
	profile := flag.String("profile", "", "artifact profile: native_capable or baml_only (required)")
	workerPackage := flag.String("worker-package", "", "the worker package this build compiles, empty for an in-process build")
	buildTags := flag.String("build-tags", "", "comma-separated Go build tags of this build")
	subprocess := flag.String("subprocess", "", "true for a subprocess (worker) build, false for in-process (required)")
	bamlVersion := flag.String("baml-version", "", "selected BAML version (required)")
	adapterVersion := flag.String("adapter-version", "", "selected framework adapter version (required)")
	sourceRevision := flag.String("source-revision", "", "release revision this build is of, or \"unset\" (required)")
	sourceBundleDigest := flag.String("source-bundle-digest", "", "digest of the embedded source bundle this build was laid down from, or \"unset\" (required)")
	tarPath := flag.String("native-worker-tar", "cmd/build/nativeworker_module.tar", "path to the packaged native-worker module tar, digested into the artifact ID")
	flag.Parse()

	if err := run(*profile, *workerPackage, *buildTags, *subprocess, *bamlVersion, *adapterVersion,
		*sourceRevision, *sourceBundleDigest, *tarPath); err != nil {
		fmt.Fprintf(os.Stderr, "artifactattest: %v\n", err)
		os.Exit(1)
	}
}

// run validates the selection and prints the artifact ID. Separated from main so
// the validation contract is unit-testable without a process boundary.
func run(profile, workerPackage, buildTags, subprocess, bamlVersion, adapterVersion,
	sourceRevision, sourceBundleDigest, tarPath string) error {
	parsedProfile, err := artifactprofile.ParseProfile(profile)
	if err != nil {
		return fmt.Errorf("--profile: %w", err)
	}

	var isSubprocess bool
	switch subprocess {
	case "true":
		isSubprocess = true
	case "false":
		isSubprocess = false
	default:
		return fmt.Errorf("--subprocess must be exactly \"true\" or \"false\", got %q", subprocess)
	}

	// A subprocess build always compiles a worker binary; an in-process build
	// never does. A selection that claims otherwise is a build-script bug, and
	// stamping it would produce an ID that does not describe the artifact.
	if isSubprocess && workerPackage == "" {
		return fmt.Errorf("--worker-package is required for a subprocess build")
	}
	if !isSubprocess && workerPackage != "" {
		return fmt.Errorf("--worker-package must be empty for an in-process build, got %q", workerPackage)
	}
	// An in-process build has no worker subprocess, so it can never be the
	// native-capable artifact: nanollm links only into the worker.
	if !isSubprocess && parsedProfile == artifactprofile.ProfileNativeCapable {
		return fmt.Errorf("--profile %q is impossible for an in-process build: the native engine links only into the worker subprocess", parsedProfile)
	}

	if bamlVersion == "" {
		return fmt.Errorf("--baml-version is required")
	}
	if adapterVersion == "" {
		return fmt.Errorf("--adapter-version is required")
	}

	// Provenance is required, not defaulted: a builder that forgot to thread it
	// must fail loudly rather than silently stamp an identity that cannot tell two
	// releases apart. "unset" is the one accepted absence, and it is EXPLICIT.
	if sourceRevision == "" {
		return fmt.Errorf("--source-revision is required (pass %q when the build front end supplied none)", artifactprofile.ProvenanceUnset)
	}
	if sourceBundleDigest == "" {
		return fmt.Errorf("--source-bundle-digest is required (pass %q when the build front end supplied none)", artifactprofile.ProvenanceUnset)
	}

	// The tar digest is COMPUTED here rather than accepted from the caller, so it
	// describes the tar this build context will actually extract and compile.
	tarDigest, err := artifactprofile.ComputeFileDigest(tarPath)
	if err != nil {
		return err
	}
	// A native-capable artifact IS the packaged worker; building one from a
	// context with no tar cannot work, and stamping "absent" for it would attest a
	// provenance the artifact does not have.
	if parsedProfile == artifactprofile.ProfileNativeCapable && tarDigest == artifactprofile.TarDigestAbsent {
		return fmt.Errorf("--profile native_capable but %s is missing from the build context", tarPath)
	}

	inputs := artifactprofile.Inputs{
		Profile:               parsedProfile,
		WorkerPackage:         workerPackage,
		BuildTags:             buildTags,
		Subprocess:            isSubprocess,
		BAMLVersion:           bamlVersion,
		AdapterVersion:        adapterVersion,
		SourceRevision:        sourceRevision,
		SourceBundleDigest:    sourceBundleDigest,
		NativeWorkerTarDigest: tarDigest,
	}
	if err := inputs.Validate(); err != nil {
		return err
	}

	id := artifactprofile.ComputeArtifactID(inputs)
	// Validate what we are about to print: the startup attestation rejects an
	// ID that is not a fixed-width lowercase-hex token, so the builder must not
	// be able to emit one that would fail there.
	if err := artifactprofile.ValidateArtifactID(id); err != nil {
		return err
	}
	blob := inputs.Marshal()
	// Round-trip before printing. The stamped blob is the evidence the startup
	// check re-derives the ID from; a blob this build could not read back is a
	// binary that would refuse to boot, and finding that out at deploy time
	// instead of build time is the wrong end.
	roundTripped, err := artifactprofile.ParseInputs(blob)
	if err != nil {
		return fmt.Errorf("inputs blob does not round-trip: %w", err)
	}
	if got := artifactprofile.ComputeArtifactID(roundTripped); got != id {
		return fmt.Errorf("inputs blob round-trips to artifact ID %q, want %q", got, id)
	}

	fmt.Printf("artifact_id=%s\n", id)
	fmt.Printf("artifact_inputs=%s\n", blob)
	return nil
}
