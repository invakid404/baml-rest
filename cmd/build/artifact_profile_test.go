package main

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// De-BAML serving cutover S2 — the STANDARD-ARTIFACT SELECTION contract.
//
// S2's central claim is a build decision: for a subprocess build, the
// native-capable worker built from the isolated nanollmprepare module is now the
// DEFAULT deployable artifact, and the zero-options BAML-only worker is reachable
// only by asking for it. That decision lives in build.sh, so these tests drive
// build.sh ITSELF through its dry-run hook rather than restating the rules in Go
// — a second copy of the rules would be the copy that drifts, and it would go on
// passing after someone flipped the default back.
//
// The hook exits before the toolchain, the network and the generated sources are
// touched, so this is a fast, hermetic test of the real script.

// artifactSelection is the parsed dry-run output.
type artifactSelection struct {
	profile          string
	nativeWorker     string
	shadowWorker     string
	nativeOnlyWorker string
	subprocess       string
	buildTags        []string
	// de-BAML S2 artifact-ID provenance, as build.sh resolved and validated it.
	sourceRevision     string
	sourceBundleDigest string
}

func (s artifactSelection) hasTag(tag string) bool {
	for _, t := range s.buildTags {
		if t == tag {
			return true
		}
	}
	return false
}

// runBuildScript executes build.sh's artifact-profile dry run with env and
// returns the parsed selection. It fails the test when the script exits
// non-zero; wantFailure cases use runBuildScriptExpectingFailure instead.
func runBuildScript(t *testing.T, env map[string]string) artifactSelection {
	t.Helper()
	out, err := execBuildScript(t, env)
	if err != nil {
		t.Fatalf("build.sh dry run failed: %v\n%s", err, out)
	}
	sel := artifactSelection{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "artifact_profile":
			sel.profile = value
		case "native_worker":
			sel.nativeWorker = value
		case "shadow_worker":
			sel.shadowWorker = value
		case "native_only_worker":
			sel.nativeOnlyWorker = value
		case "subprocess":
			sel.subprocess = value
		case "artifact_source_revision":
			sel.sourceRevision = value
		case "artifact_source_bundle_digest":
			sel.sourceBundleDigest = value
		case "build_tags":
			if value != "" {
				sel.buildTags = strings.Split(value, ",")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading build.sh output: %v", err)
	}
	if sel.profile == "" {
		t.Fatalf("build.sh dry run printed no artifact_profile:\n%s", out)
	}
	return sel
}

// execBuildScript runs the dry run and returns its combined output.
//
// The env is fully controlled: the test never inherits NATIVE_WORKER /
// SHADOW_WORKER / SUBPROCESS from the developer's shell, because inheriting the
// very variable under test is how a selection test quietly becomes vacuous.
func execBuildScript(t *testing.T, env map[string]string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("build.sh")
	if err != nil {
		t.Fatalf("resolve build.sh: %v", err)
	}
	tmp := t.TempDir()
	base := map[string]string{
		"PATH":                     "/usr/bin:/bin:/usr/sbin:/sbin",
		"ARTIFACT_PROFILE_DRY_RUN": "true",
		// build.sh's required inputs; irrelevant to the selection but validated
		// before the dry-run hook is reached.
		"BAML_VERSION":      "0.223.0",
		"ADAPTER_VERSION":   "v0.219.0",
		"USER_CONTEXT_PATH": tmp,
		// Keep every directory the pre-hook prologue creates inside the temp dir.
		"CACHE_DIR":      filepath.Join(tmp, "cache"),
		"BAML_CACHE_DIR": filepath.Join(tmp, "baml-cache"),
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = tmp
	cmd.Env = nil
	for k, v := range base {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestNativeCapableWorkerIsTheStandardArtifact is the S2 headline: a plain
// subprocess build — nothing set, exactly what an unconfigured release build
// does — selects the native-capable worker and tags the host as carrying it.
// Flipping the default back to the BAML-only worker fails here.
func TestNativeCapableWorkerIsTheStandardArtifact(t *testing.T) {
	sel := runBuildScript(t, nil)
	if sel.profile != "native_capable" {
		t.Errorf("default artifact_profile = %q, want native_capable (the S2 standard artifact)", sel.profile)
	}
	if sel.nativeWorker != "true" {
		t.Errorf("default native_worker = %q, want true", sel.nativeWorker)
	}
	if !sel.hasTag("nativeworkerartifact") {
		t.Errorf("default build tags %v lack nativeworkerartifact; the host would attest itself BAML-only", sel.buildTags)
	}
	if !sel.hasTag("nativestreamserve") {
		t.Errorf("default build tags %v lack nativestreamserve; the standard artifact IS the stream-serve profile", sel.buildTags)
	}
	// ExecBridge-U1c: the standard native-capable serve worker now compiles the
	// generated native spine registry (its serveProfileOptions installs the oracle
	// composite, which drives NewExecutor), so the profile-neutral registry tag is
	// present. Without it standardspineoracle.NewExecutor would see the fail-loud stub
	// and the worker would refuse to serve.
	if !sel.hasTag("debamlnativespinegenerated") {
		t.Errorf("default build tags %v lack debamlnativespinegenerated; the standard oracle composite would see the fail-loud registry stub", sel.buildTags)
	}
}

// TestBAMLOnlyRollbackArtifactStaysSelectable pins the other half of the
// decision: the BAML-only artifact remains buildable, on purpose, as the
// rollback lane. BAML_REST_USE_DEBAML=false only promises a total BAML revert
// for as long as a BAML-capable artifact exists to revert to.
func TestBAMLOnlyRollbackArtifactStaysSelectable(t *testing.T) {
	sel := runBuildScript(t, map[string]string{"NATIVE_WORKER": "false"})
	if sel.profile != "baml_only" {
		t.Errorf("NATIVE_WORKER=false artifact_profile = %q, want baml_only", sel.profile)
	}
	if sel.hasTag("nativeworkerartifact") {
		t.Errorf("rollback build tags %v include nativeworkerartifact; the host would attest a native-capable artifact it does not carry", sel.buildTags)
	}
	if sel.hasTag("nativestreamserve") {
		t.Errorf("rollback build tags %v include nativestreamserve; the pool would suppress stream retries against a worker that cannot serve streams", sel.buildTags)
	}
}

// TestInProcessBuildDowngradesWithoutBreaking is the do-not-hard-break rule.
// Flipping the default made "in-process" and "native worker" collide by DEFAULT
// rather than only on request. An in-process build that never asked for a native
// worker must still succeed (as the BAML-only profile — it has no worker
// subprocess to make native-capable), while one that explicitly asks is still a
// contradiction and must fail.
func TestInProcessBuildDowngradesWithoutBreaking(t *testing.T) {
	sel := runBuildScript(t, map[string]string{"SUBPROCESS": "false"})
	if sel.profile != "baml_only" {
		t.Errorf("in-process artifact_profile = %q, want baml_only", sel.profile)
	}
	if sel.hasTag("nativeworkerartifact") {
		t.Errorf("in-process build tags %v include nativeworkerartifact", sel.buildTags)
	}

	for _, requested := range []map[string]string{
		{"SUBPROCESS": "false", "NATIVE_WORKER": "true"},
		{"SUBPROCESS": "false", "SHADOW_WORKER": "true"},
	} {
		out, err := execBuildScript(t, requested)
		if err == nil {
			t.Errorf("build.sh accepted %v; an explicit native worker with SUBPROCESS=false must fail:\n%s", requested, out)
		}
	}
}

// TestShadowProfileIsNativeCapableButNotStreamServe pins the distinction that
// made the artifact profile a SEPARATE build tag from nativestreamserve. The
// shadow worker is built from the isolated module (so it links the native engine
// and IS a native-capable artifact) but installs no stream serve factory. Before
// S2 the "not a stream-serve profile" half held only because a shadow build left
// NATIVE_WORKER unset and unset meant false — a coincidence the flipped default
// would have silently broken.
func TestShadowProfileIsNativeCapableButNotStreamServe(t *testing.T) {
	sel := runBuildScript(t, map[string]string{"SHADOW_WORKER": "true"})
	if sel.profile != "native_capable" {
		t.Errorf("shadow artifact_profile = %q, want native_capable", sel.profile)
	}
	if !sel.hasTag("nativeworkerartifact") {
		t.Errorf("shadow build tags %v lack nativeworkerartifact", sel.buildTags)
	}
	if sel.hasTag("nativestreamserve") {
		t.Errorf("shadow build tags %v include nativestreamserve; the shadow worker installs no stream serve factory", sel.buildTags)
	}
	// ExecBridge-U1c: the shadow worker does NOT use the generated native spine registry
	// (it installs the no-send static shadow comparator, not the oracle composite), so
	// the profile-neutral registry tag must be absent — the same GO_BUILD_TAGS would
	// otherwise force the shadow build to carry (and require generation of) a registry it
	// never imports.
	if sel.hasTag("debamlnativespinegenerated") {
		t.Errorf("shadow build tags %v include debamlnativespinegenerated; the shadow worker uses no generated registry", sel.buildTags)
	}
}

// TestArtifactSelectorsAreStrictlyDecoded pins that the two variables deciding
// what ships are decoded strictly. A typo must fail the build rather than fall
// through to a falsy default and silently produce the other artifact — the exact
// failure mode that would make a "rollback" build ship the standard artifact.
func TestArtifactSelectorsAreStrictlyDecoded(t *testing.T) {
	for _, env := range []map[string]string{
		{"NATIVE_WORKER": "yes"},
		{"NATIVE_WORKER": "1"},
		{"NATIVE_WORKER": "TRUE"},
		{"NATIVE_WORKER": "off"},
		{"SHADOW_WORKER": "1"},
		{"SHADOW_WORKER": "TRUE"},
	} {
		out, err := execBuildScript(t, env)
		if err == nil {
			t.Errorf("build.sh accepted a non-boolean selector %v:\n%s", env, out)
		}
	}

	// EMPTY is the one non-boolean spelling that is deliberately accepted, and it
	// means UNSET — the ${VAR:-default} convention build.sh uses for every other
	// variable, and the shape a deploy system that unconditionally exports a
	// variable produces. Pinned explicitly so the meaning is a decision on record
	// rather than a gap in the strict decoder: empty selects the STANDARD
	// artifact, exactly like an absent variable.
	for _, env := range []map[string]string{
		{"NATIVE_WORKER": ""},
		{"SHADOW_WORKER": ""},
	} {
		sel := runBuildScript(t, env)
		if sel.profile != "native_capable" {
			t.Errorf("empty selector %v gave artifact_profile = %q, want the standard native_capable", env, sel.profile)
		}
	}
}

// TestNativeOnlyWorkerIsSelectableAndTaggedGenerated is the ExecBridge-U1b
// selection contract: NATIVE_ONLY_WORKER=true selects the BAML-free native-only
// worker. It is native_capable (it links the native engine), it carries the
// profile-neutral debamlnativespinegenerated tag (so the generated registry, not the
// fail-loud stub, compiles), and it is NOT a stream-serve profile (its cohort is unary
// final-call only), so the host must not arm stream-retry suppression for it.
func TestNativeOnlyWorkerIsSelectableAndTaggedGenerated(t *testing.T) {
	sel := runBuildScript(t, map[string]string{"NATIVE_ONLY_WORKER": "true"})
	if sel.profile != "native_capable" {
		t.Errorf("native-only artifact_profile = %q, want native_capable", sel.profile)
	}
	if sel.nativeOnlyWorker != "true" {
		t.Errorf("native_only_worker = %q, want true", sel.nativeOnlyWorker)
	}
	if !sel.hasTag("debamlnativespinegenerated") {
		t.Errorf("native-only build tags %v lack debamlnativespinegenerated; the fail-loud stub would compile instead of the generated registry", sel.buildTags)
	}
	if !sel.hasTag("nativeworkerartifact") {
		t.Errorf("native-only build tags %v lack nativeworkerartifact", sel.buildTags)
	}
	if sel.hasTag("nativestreamserve") {
		t.Errorf("native-only build tags %v include nativestreamserve; the native-only cohort serves no streams, so the host must not suppress stream retries", sel.buildTags)
	}
}

// TestNativeOnlyWorkerFlagOffKeepsTheStandardArtifact proves --native-only-worker=false
// selects the STANDARD native-capable serve artifact. Since ExecBridge-U1c that standard
// artifact ALSO carries the profile-neutral debamlnativespinegenerated tag (its
// serveProfileOptions installs the oracle composite over the generated registry), so the
// two native artifacts are distinguished by native_only_worker + WorkerPackage, NOT by
// the tag. The stream-serve profile is retained.
func TestNativeOnlyWorkerFlagOffKeepsTheStandardArtifact(t *testing.T) {
	for _, env := range []map[string]string{
		nil,
		{"NATIVE_ONLY_WORKER": "false"},
		{"NATIVE_ONLY_WORKER": ""}, // empty == unset == false
	} {
		sel := runBuildScript(t, env)
		if sel.nativeOnlyWorker != "false" {
			t.Errorf("env %v: native_only_worker = %q, want false", env, sel.nativeOnlyWorker)
		}
		// The standard artifact carries the generic registry tag (the oracle composite),
		// but is NOT the native-only worker.
		if !sel.hasTag("debamlnativespinegenerated") {
			t.Errorf("env %v: standard build tags %v lack debamlnativespinegenerated; the oracle composite would see the fail-loud stub", env, sel.buildTags)
		}
		// The standard artifact is otherwise unchanged: native_capable + stream serve.
		if sel.profile != "native_capable" || !sel.hasTag("nativestreamserve") {
			t.Errorf("env %v: flag-off build changed the standard artifact (profile=%q tags=%v)", env, sel.profile, sel.buildTags)
		}
	}
}

// TestNativeOnlyWorkerRejectsContradictions pins the mutual exclusions: the
// native-only worker is a native_capable subprocess artifact, so it cannot be
// combined with the shadow profile, an in-process build, or NATIVE_WORKER=false —
// all rejected BEFORE any build work.
func TestNativeOnlyWorkerRejectsContradictions(t *testing.T) {
	for _, env := range []map[string]string{
		{"NATIVE_ONLY_WORKER": "true", "SHADOW_WORKER": "true"},
		{"NATIVE_ONLY_WORKER": "true", "SUBPROCESS": "false"},
		{"NATIVE_ONLY_WORKER": "true", "NATIVE_WORKER": "false"},
	} {
		out, err := execBuildScript(t, env)
		if err == nil {
			t.Errorf("build.sh accepted a contradictory native-only selection %v:\n%s", env, out)
		}
	}
}

// TestNativeOnlyWorkerIsStrictlyDecoded pins that NATIVE_ONLY_WORKER is decoded
// strictly like the other artifact selectors: a typo fails the build.
func TestNativeOnlyWorkerIsStrictlyDecoded(t *testing.T) {
	for _, env := range []map[string]string{
		{"NATIVE_ONLY_WORKER": "yes"},
		{"NATIVE_ONLY_WORKER": "1"},
		{"NATIVE_ONLY_WORKER": "TRUE"},
		{"NATIVE_ONLY_WORKER": "off"},
	} {
		out, err := execBuildScript(t, env)
		if err == nil {
			t.Errorf("build.sh accepted a non-boolean NATIVE_ONLY_WORKER %v:\n%s", env, out)
		}
	}
}
