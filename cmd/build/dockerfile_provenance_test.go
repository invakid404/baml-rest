package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/invakid404/baml-rest/internal/artifactprofile"
)

// De-BAML serving cutover S2 — the CONTAINER-BUILD provenance gate.
//
// This gate exists because of a regression it would have caught. The S2
// artifact-ID provenance keys were added to cmd/build's renderer of
// cmd/build/Dockerfile.tmpl and NOT to the integration harness's renderer of the
// same template. Go's text/template renders a missing map key as the literal
// string `<no value>`, silently, so every integration image was built with:
//
//	ENV ARTIFACT_SOURCE_BUNDLE_DIGEST="<no value>"
//
// build.sh handed that to artifactattest, whose strict digest check correctly
// rejected it, the image build exited 1, and all ~25 integration jobs failed with
// "Failed to setup test environment". Nothing in the cold review, the bot triage
// or the local gates ran a container build, so the only signal was the full
// integration matrix.
//
// So the class is closed HERE, in the ordinary `go test ./...` lane, with no
// Docker required:
//
//	RENDER    the real cmd/build args map must render the template with no
//	          `<no value>` anywhere and a valid 16-hex digest in the ENV line;
//	REJECT    build.sh must fail LOUDLY on a `<no value>` provenance value rather
//	          than passing it down to artifactattest;
//	ACCEPT    a real digest must survive build.sh into a valid artifact ID.
//
// The integration harness's own renderer is covered by the sibling gate in
// integration/testutil (build-tagged, since that harness is).

// dockerfileTemplatePath is the template both renderers share.
const dockerfileTemplatePath = "Dockerfile.tmpl"

// missingKeyRendering is what text/template emits for an absent map key. It is
// the exact string that broke the matrix.
const missingKeyRendering = "<no value>"

var envDigestRe = regexp.MustCompile(`ENV ARTIFACT_SOURCE_BUNDLE_DIGEST="([^"]*)"`)
var envRevisionRe = regexp.MustCompile(`ENV ARTIFACT_SOURCE_REVISION="([^"]*)"`)

// renderDockerfile renders the shared template with args.
func renderDockerfile(t *testing.T, args map[string]interface{}) string {
	t.Helper()
	raw, err := os.ReadFile(dockerfileTemplatePath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfileTemplatePath, err)
	}
	tmpl, err := template.New("dockerfile").Parse(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", dockerfileTemplatePath, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, args); err != nil {
		t.Fatalf("execute %s: %v", dockerfileTemplatePath, err)
	}
	return out.String()
}

// realProvenance is a provenance record shaped like one cmd/build resolves.
func realProvenance(t *testing.T) artifactProvenanceRecord {
	t.Helper()
	rec, err := resolveArtifactProvenance("27af8af5ae04")
	if err != nil {
		t.Fatalf("resolveArtifactProvenance: %v", err)
	}
	if err := artifactprofile.ValidateArtifactID(rec.BundleDigest); err != nil {
		t.Fatalf("cmd/build computed a bundle digest its own validator rejects: %v", err)
	}
	return rec
}

// TestDockerfileRendersRealProvenance is the RENDER half: the map cmd/build
// actually uses must leave no unrendered key anywhere in the Dockerfile, and must
// put a real digest in the ENV line build.sh reads.
func TestDockerfileRendersRealProvenance(t *testing.T) {
	prov := realProvenance(t)

	// Every build shape whose template branch differs. The BAML-SOURCE cases
	// matter because that branch carries a bare `{{ .protocGenGoVersion }}` the
	// default branch never renders: a review found that dropping that key still
	// passed every gate here, because no case set bamlSource.
	for _, tc := range []struct {
		name         string
		nativeWorker bool
		bamlSource   bool
		protoc       string
	}{
		{name: "standard artifact", nativeWorker: true},
		{name: "rollback artifact", nativeWorker: false},
		{name: "standard artifact from BAML source", nativeWorker: true, bamlSource: true, protoc: "v1.36.12"},
		{name: "rollback artifact from BAML source", nativeWorker: false, bamlSource: true, protoc: "v1.36.12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := renderDockerfile(t, dockerfileTemplateArgsFor(
				"0.223.0", "v0.219.0", "", "", tc.protoc,
				false, false, false, tc.nativeWorker, false, tc.bamlSource, prov))

			// No key anywhere may be unrendered — not just the provenance pair.
			// A future key added to the template and forgotten in this map fails
			// here rather than in a container build.
			if strings.Contains(rendered, missingKeyRendering) {
				for _, line := range strings.Split(rendered, "\n") {
					if strings.Contains(line, missingKeyRendering) {
						t.Errorf("Dockerfile line renders an unsupplied template key: %s", line)
					}
				}
				t.FailNow()
			}

			m := envDigestRe.FindStringSubmatch(rendered)
			if m == nil {
				t.Fatalf("rendered Dockerfile has no ARTIFACT_SOURCE_BUNDLE_DIGEST ENV line")
			}
			if err := artifactprofile.ValidateArtifactID(m[1]); err != nil {
				t.Errorf("ENV ARTIFACT_SOURCE_BUNDLE_DIGEST=%q is not a valid artifact ID: %v", m[1], err)
			}
			if r := envRevisionRe.FindStringSubmatch(rendered); r == nil || r[1] == "" {
				t.Errorf("rendered Dockerfile has no usable ARTIFACT_SOURCE_REVISION ENV line: %v", r)
			}

			// Non-vacuity for the BAML-source cases: prove the branch that
			// carries the bare protoc interpolation was actually rendered, so
			// "no <no value>" is a statement about a branch that exists here.
			if tc.bamlSource {
				if !strings.Contains(rendered, "protoc-gen-go@"+tc.protoc) {
					t.Errorf("the BAML-source branch did not render the protoc-gen-go interpolation; this case is not exercising it")
				}
			}
		})
	}
}

// TestDockerfileBAMLSourceBranchNeedsItsOwnKeys pins the branch-conditional half
// of the contract: a key the template interpolates only on the BAML-source branch
// is just as load-bearing as one on the default branch, and just as invisible
// when it is missing. Dropping protocGenGoVersion there renders `<no value>` into
// a `go install …@<no value>` line that fails deep inside the image build.
func TestDockerfileBAMLSourceBranchNeedsItsOwnKeys(t *testing.T) {
	prov := realProvenance(t)
	args := dockerfileTemplateArgsFor("0.223.0", "v0.219.0", "", "", "v1.36.12",
		false, false, false, true, false, true, prov)
	delete(args, "protocGenGoVersion")

	rendered := renderDockerfile(t, args)
	if !strings.Contains(rendered, missingKeyRendering) {
		t.Fatalf("dropping protocGenGoVersion on the BAML-source branch did not render %q; this gate's premise no longer holds", missingKeyRendering)
	}
}

// TestDockerfileMissingProvenanceKeyRendersNoValue documents the hazard this gate
// exists for, so the next reader does not have to rediscover why the checks below
// are shaped the way they are: an absent key is NOT an error at render time, it is
// a plausible-looking string that only fails much later, inside a container build.
func TestDockerfileMissingProvenanceKeyRendersNoValue(t *testing.T) {
	prov := realProvenance(t)
	args := dockerfileTemplateArgsFor("0.223.0", "v0.219.0", "", "", "", false, false, false, true, false, false, prov)
	delete(args, "artifactSourceBundleDigest")

	rendered := renderDockerfile(t, args)
	m := envDigestRe.FindStringSubmatch(rendered)
	if m == nil {
		t.Fatal("expected the ENV line to still be emitted with an unrendered value")
	}
	if m[1] != missingKeyRendering {
		t.Fatalf("a missing template key rendered as %q, not %q; this gate's premise no longer holds and its assertions need rethinking", m[1], missingKeyRendering)
	}
}

// TestBuildScriptRejectsUnrenderedProvenance is the REJECT half, and the
// mutation-biting one: the exact value the broken template produced must fail
// build.sh loudly, with a diagnostic naming the template key — not be defaulted,
// coerced, or passed down to artifactattest where the cause is no longer visible.
func TestBuildScriptRejectsUnrenderedProvenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"unrendered digest", map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": missingKeyRendering}},
		{"unrendered revision", map[string]string{
			"ARTIFACT_SOURCE_REVISION":      missingKeyRendering,
			"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789abcdef",
		}},
		{"short digest", map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": "abc123"}},
		{"uppercase digest", map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789ABCDEF"}},
		{"digest with a path in it", map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": "/home/build/tree"}},

		// SET BUT EMPTY. `${VAR:-unset}` substitutes for an empty variable as well
		// as an unset one, so the colon form silently turned `ENV
		// ARTIFACT_SOURCE_BUNDLE_DIGEST=""` into the accepted `unset` sentinel and
		// minted a valid artifact ID with provenance that is absent by ACCIDENT
		// rather than by declaration. build.sh uses the unset-only form, so these
		// reach the validator and are refused.
		{"empty digest", map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": ""}},
		{"empty revision", map[string]string{
			"ARTIFACT_SOURCE_REVISION":      "",
			"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789abcdef",
		}},

		// A FORBIDDEN CHARACTER anywhere in the revision. The preflight used to be
		// an allowed-class glob of the form `[allowed]*[allowed]`, whose middle `*`
		// matches arbitrary characters — so a compliant first and last character
		// let `abc<def` through, and only artifactattest caught it, after the whole
		// Node/BAML codegen had run.
		{"revision with a forbidden character", map[string]string{
			"ARTIFACT_SOURCE_REVISION":      "abc<def",
			"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789abcdef",
		}},
		{"revision with a space", map[string]string{
			"ARTIFACT_SOURCE_REVISION":      "abc def",
			"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789abcdef",
		}},
		{"revision with a shell metacharacter", map[string]string{
			"ARTIFACT_SOURCE_REVISION":      "abc;def",
			"ARTIFACT_SOURCE_BUNDLE_DIGEST": "0123456789abcdef",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execBuildScript(t, tc.env)
			if err == nil {
				t.Fatalf("build.sh accepted provenance %v; a value it cannot use must fail the build:\n%s", tc.env, out)
			}
			if !strings.Contains(out, "is not usable artifact provenance") {
				t.Errorf("build.sh rejected %v without the provenance diagnostic:\n%s", tc.env, out)
			}
		})
	}

	// An EMPTY value must be diagnosed as an empty value, not as a generic
	// malformed one: the whole point is that a reader can tell "nobody supplied
	// this" from "somebody wrote the sentinel on purpose".
	emptyOut, emptyErr := execBuildScript(t, map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": ""})
	if emptyErr == nil {
		t.Fatalf("build.sh accepted an explicitly empty digest:\n%s", emptyOut)
	}
	if !strings.Contains(emptyOut, "SET but EMPTY") {
		t.Errorf("the empty-provenance diagnostic does not distinguish empty from the sentinel:\n%s", emptyOut)
	}

	// The diagnostic must name the TEMPLATE KEY, because the fix belongs in
	// whichever renderer omitted it — not in build.sh and not in artifactattest.
	out, err := execBuildScript(t, map[string]string{"ARTIFACT_SOURCE_BUNDLE_DIGEST": missingKeyRendering})
	if err == nil {
		t.Fatal("build.sh accepted the unrendered digest")
	}
	for _, want := range []string{"artifactSourceBundleDigest", "Dockerfile.tmpl", "text/template"} {
		if !strings.Contains(out, want) {
			t.Errorf("the provenance diagnostic does not mention %q; the reader cannot tell which renderer to fix:\n%s", want, out)
		}
	}
}

// TestBuildScriptAcceptsTheUnsetSentinel keeps the empty-value rejection above
// from over-reaching: `unset` is a DELIBERATE sentinel and must still be accepted,
// so a hand-run build.sh outside cmd/build stays runnable. Together with the
// empty-value cases this pins the distinction the `:-` expansion had erased.
func TestBuildScriptAcceptsTheUnsetSentinel(t *testing.T) {
	// Genuinely unset: execBuildScript's base environment does not define either.
	sel := runBuildScript(t, nil)
	if sel.sourceRevision != "unset" || sel.sourceBundleDigest != "unset" {
		t.Fatalf("an unset environment resolved to revision=%q digest=%q, want both %q",
			sel.sourceRevision, sel.sourceBundleDigest, "unset")
	}

	// And written out explicitly, which is what a caller does on purpose.
	sel = runBuildScript(t, map[string]string{
		"ARTIFACT_SOURCE_REVISION":      "unset",
		"ARTIFACT_SOURCE_BUNDLE_DIGEST": "unset",
	})
	if sel.sourceRevision != "unset" || sel.sourceBundleDigest != "unset" {
		t.Fatalf("the explicit sentinel resolved to revision=%q digest=%q", sel.sourceRevision, sel.sourceBundleDigest)
	}
}

// TestBuildScriptAcceptsRenderedProvenance is the ACCEPT half, and it keeps the
// REJECT half honest: a gate that only proved rejection would be satisfied by a
// build.sh that refused everything.
func TestBuildScriptAcceptsRenderedProvenance(t *testing.T) {
	prov := realProvenance(t)
	rendered := renderDockerfile(t, dockerfileTemplateArgsFor(
		"0.223.0", "v0.219.0", "", "", "", false, false, false, true, false, false, prov))
	digest := envDigestRe.FindStringSubmatch(rendered)[1]
	revision := envRevisionRe.FindStringSubmatch(rendered)[1]

	sel := runBuildScript(t, map[string]string{
		"ARTIFACT_SOURCE_BUNDLE_DIGEST": digest,
		"ARTIFACT_SOURCE_REVISION":      revision,
	})
	if sel.sourceBundleDigest != digest {
		t.Errorf("build.sh resolved the bundle digest to %q, want the rendered %q", sel.sourceBundleDigest, digest)
	}
	if sel.sourceRevision != revision {
		t.Errorf("build.sh resolved the revision to %q, want the rendered %q", sel.sourceRevision, revision)
	}

	// And the value survives into a real artifact ID: this is the step that
	// aborted the container build.
	attest := exec.Command("go", "run", "./cmd/build/artifactattest",
		"--profile", "native_capable",
		"--worker-package", "nanollmprepare:./cmd/worker/",
		"--build-tags", "subprocess,nativestreamserve,nativeworkerartifact",
		"--subprocess", "true",
		"--baml-version", "0.223.0",
		"--adapter-version", "v0.219.0",
		"--source-revision", revision,
		"--source-bundle-digest", digest,
		"--native-worker-tar", filepath.Join("cmd", "build", "nativeworker_module.tar"),
	)
	// Run from the repo root, exactly as build.sh does, so the tar digest is read
	// out of a real build context.
	attest.Dir = filepath.Join("..", "..")
	out, err := attest.CombinedOutput()
	if err != nil {
		t.Fatalf("artifactattest rejected the rendered provenance: %v\n%s", err, out)
	}
	var artifactID string
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "artifact_id="); ok {
			artifactID = v
		}
	}
	if err := artifactprofile.ValidateArtifactID(artifactID); err != nil {
		t.Fatalf("artifactattest emitted %q, which is not a valid artifact ID: %v", artifactID, err)
	}
}
