package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// TestRunRejectsIncoherentSelections pins the builder-side validation. The
// artifact ID is what a reviewer recomputes from a build log to check that the
// shipped binary is the binary that was reviewed, so a mis-invocation must fail
// the BUILD rather than stamp an identity that does not describe the artifact.
func TestRunRejectsIncoherentSelections(t *testing.T) {
	const (
		baml    = "0.223.0"
		adapter = "v0.219.0"
	)
	const (
		rev    = "27af8af5ae04"
		bundle = "1122334455667788"
	)
	tar := writeTar(t)

	for _, tc := range []struct {
		name    string
		profile string
		pkg     string
		tags    string
		sub     string
		baml    string
		adapter string
		rev     string
		bundle  string
		tarPath string
	}{
		{"unknown profile", "native", "root:./cmd/worker/", "subprocess", "true", baml, adapter, rev, bundle, tar},
		{"empty profile", "", "root:./cmd/worker/", "subprocess", "true", baml, adapter, rev, bundle, tar},
		{"non-boolean subprocess", "baml_only", "root:./cmd/worker/", "subprocess", "yes", baml, adapter, rev, bundle, tar},
		{"subprocess build without a worker package", "baml_only", "", "subprocess", "true", baml, adapter, rev, bundle, tar},
		{"in-process build with a worker package", "baml_only", "root:./cmd/worker/", "", "false", baml, adapter, rev, bundle, tar},
		{"in-process build claiming native capability", "native_capable", "", "", "false", baml, adapter, rev, bundle, tar},
		{"missing baml version", "baml_only", "root:./cmd/worker/", "subprocess", "true", "", adapter, rev, bundle, tar},
		{"missing adapter version", "baml_only", "root:./cmd/worker/", "subprocess", "true", baml, "", rev, bundle, tar},
		// Provenance is REQUIRED, not defaulted: a builder that forgot to thread it
		// must fail rather than stamp an identity that cannot tell two releases
		// apart. "unset" is the one accepted absence and it has to be explicit.
		{"missing source revision", "baml_only", "root:./cmd/worker/", "subprocess", "true", baml, adapter, "", bundle, tar},
		{"missing source bundle digest", "baml_only", "root:./cmd/worker/", "subprocess", "true", baml, adapter, rev, "", tar},
		{"a path smuggled in as the bundle digest", "baml_only", "root:./cmd/worker/", "subprocess", "true", baml, adapter, rev, "/home/build/tree", tar},
		{"a URL smuggled in as the revision", "baml_only", "root:./cmd/worker/", "subprocess", "true", baml, adapter, "https://example.invalid/x", bundle, tar},
		// A native-capable artifact IS the packaged worker; a context without the
		// tar cannot produce one, and stamping "absent" would attest a provenance
		// the artifact does not have.
		{"native_capable without the packaged tar", "native_capable", "nanollmprepare:./cmd/worker/", "subprocess", "true", baml, adapter, rev, bundle, filepath.Join(t.TempDir(), "missing.tar")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.profile, tc.pkg, tc.tags, tc.sub, tc.baml, tc.adapter, tc.rev, tc.bundle, tc.tarPath); err == nil {
				t.Fatalf("run accepted an incoherent selection")
			}
		})
	}
}

// writeTar materialises a stand-in packaged-module tar so the digest step has
// real bytes to read.
func writeTar(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nativeworker_module.tar")
	if err := os.WriteFile(path, []byte("packaged nativeserve + nanollmprepare source"), 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	return path
}

// TestRunAcceptsTheRealBuildSelections pins that the selections build.sh
// actually emits are accepted, so the validation above cannot be satisfied by
// simply rejecting everything.
func TestRunAcceptsTheRealBuildSelections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		pkg     string
		tags    string
		sub     string
	}{
		// ExecBridge-U1c: the standard native-capable serve worker now also compiles the
		// generated native spine registry (the oracle composite drives NewExecutor), so
		// its build tags carry the profile-neutral debamlnativespinegenerated too.
		{"standard artifact", "native_capable", "nanollmprepare:./cmd/worker/", "subprocess,nativestreamserve,nativeworkerartifact,debamlnativespinegenerated", "true"},
		{"shadow artifact", "native_capable", "nanollmprepare:./cmd/worker-shadow/", "subprocess,nativeworkerartifact", "true"},
		// ExecBridge-U1b/U1c: the native-only worker is native_capable (a true derived
		// fact — it links the native engine) and is distinguished from the standard
		// native artifact only by its WorkerPackage, since both now carry the
		// debamlnativespinegenerated build tag.
		{"native-only artifact", "native_capable", "nanollmprepare:./cmd/worker-nativeonly/", "subprocess,nativeworkerartifact,debamlnativespinegenerated", "true"},
		{"rollback artifact", "baml_only", "root:./cmd/worker/", "subprocess", "true"},
		{"in-process artifact", "baml_only", "", "", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.profile, tc.pkg, tc.tags, tc.sub, "0.223.0", "v0.219.0",
				"27af8af5ae04", "1122334455667788", writeTar(t)); err != nil {
				t.Fatalf("run rejected a real build selection: %v", err)
			}
		})
	}

	// A hand-run build.sh has no front end to supply provenance; the explicit
	// "unset" sentinel must stay accepted so build.sh remains runnable on its own.
	if err := run("baml_only", "root:./cmd/worker/", "subprocess", "true", "0.223.0", "v0.219.0",
		artifactprofile.ProvenanceUnset, artifactprofile.ProvenanceUnset, writeTar(t)); err != nil {
		t.Fatalf("run rejected an explicitly unset provenance: %v", err)
	}
}

// TestPrintedIDMatchesTheLibrary proves the CLI stamps exactly what the startup
// validator recomputes — the two must not be able to disagree, since the whole
// attestation rests on them being one implementation.
func TestPrintedIDMatchesTheLibrary(t *testing.T) {
	in := artifactprofile.Inputs{
		Profile:               artifactprofile.ProfileNativeCapable,
		WorkerPackage:         "nanollmprepare:./cmd/worker/",
		BuildTags:             "subprocess,nativestreamserve,nativeworkerartifact",
		Subprocess:            true,
		BAMLVersion:           "0.223.0",
		AdapterVersion:        "v0.219.0",
		SourceRevision:        "27af8af5ae04",
		SourceBundleDigest:    "1122334455667788",
		NativeWorkerTarDigest: "99aabbccddeeff00",
	}
	id := artifactprofile.ComputeArtifactID(in)
	if err := artifactprofile.ValidateArtifactID(id); err != nil {
		t.Fatalf("library produced an ID its own validator rejects: %v", err)
	}
	blob := in.Marshal()
	for name, value := range map[string]string{"artifact ID": id, "inputs blob": blob} {
		if strings.TrimSpace(value) != value {
			t.Fatalf("%s %q carries surrounding whitespace; build.sh substitutes it into -ldflags verbatim", name, value)
		}
		if strings.ContainsAny(value, " \t\n'\"") {
			t.Fatalf("%s %q contains a character that would break -ldflags quoting", name, value)
		}
	}
	back, err := artifactprofile.ParseInputs(blob)
	if err != nil {
		t.Fatalf("ParseInputs: %v", err)
	}
	if artifactprofile.ComputeArtifactID(back) != id {
		t.Fatal("the stamped inputs do not re-derive the stamped ID; startup attestation would reject every build")
	}
}
