package artifactprofile

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// noEnv is the environment of a process with no expectation configured.
func noEnv(string) (string, bool) { return "", false }

// envWith returns a lookup that reports exactly one variable.
func envWith(key, value string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == key {
			return value, true
		}
		return "", false
	}
}

// sampleInputs is a realistic stamped build for the given profile.
func sampleInputs(profile Profile) Inputs {
	in := Inputs{
		Profile:               profile,
		WorkerPackage:         "nanollmprepare:./cmd/worker/",
		BuildTags:             "subprocess,nativestreamserve,nativeworkerartifact",
		Subprocess:            true,
		BAMLVersion:           "0.223.0",
		AdapterVersion:        "v0.219.0",
		SourceRevision:        "27af8af5ae04",
		SourceBundleDigest:    "1122334455667788",
		NativeWorkerTarDigest: "99aabbccddeeff00",
	}
	if profile == ProfileBAMLOnly {
		in.WorkerPackage = "root:./cmd/worker/"
		in.BuildTags = "subprocess"
	}
	return in
}

// stampFor returns the three -X stamp values a real build would emit for in.
func stampFor(in Inputs) (profile, id, blob string) {
	return string(in.Profile), ComputeArtifactID(in), in.Marshal()
}

// TestAttestUnstampedReportsDerivedProfile pins the "no claim" case: a binary
// the builder never stamped is NOT an error. S2 must not hard-break an
// entrypoint it cannot see, and the repository is explicitly not assumed to
// reveal every production deploy path, so a plain `go build` still attests —
// it just attests an unstamped identity.
func TestAttestUnstampedReportsDerivedProfile(t *testing.T) {
	for _, tc := range []struct {
		name          string
		nativeCapable bool
		want          Profile
	}{
		{"baml only", false, ProfileBAMLOnly},
		{"native capable", true, ProfileNativeCapable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att, err := attest(DeriveProfile(tc.nativeCapable), "", "", "", noEnv)
			if err != nil {
				t.Fatalf("attest: %v", err)
			}
			if att.Profile != tc.want {
				t.Errorf("Profile = %q, want %q", att.Profile, tc.want)
			}
			if att.Stamped {
				t.Errorf("Stamped = true for an unstamped binary")
			}
			if att.ArtifactID != UnstampedArtifactID {
				t.Errorf("ArtifactID = %q, want %q", att.ArtifactID, UnstampedArtifactID)
			}
			if att.AlertReason != "" {
				t.Errorf("AlertReason = %q, want none without an expectation", att.AlertReason)
			}
		})
	}
}

// TestAttestAcceptsAgreeingStamp pins the happy path a real build produces.
func TestAttestAcceptsAgreeingStamp(t *testing.T) {
	in := sampleInputs(ProfileNativeCapable)
	profile, id, blob := stampFor(in)

	att, err := attest(ProfileNativeCapable, profile, id, blob, noEnv)
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if !att.Stamped {
		t.Errorf("Stamped = false for a stamped binary")
	}
	if att.ArtifactID != id {
		t.Errorf("ArtifactID = %q, want %q", att.ArtifactID, id)
	}
	// The provenance the ID was derived from is recovered for the startup log.
	if att.SourceRevision() != in.SourceRevision {
		t.Errorf("SourceRevision() = %q, want %q", att.SourceRevision(), in.SourceRevision)
	}
	if att.SourceBundleDigest() != in.SourceBundleDigest {
		t.Errorf("SourceBundleDigest() = %q, want %q", att.SourceBundleDigest(), in.SourceBundleDigest)
	}
	if att.NativeWorkerTarDigest() != in.NativeWorkerTarDigest {
		t.Errorf("NativeWorkerTarDigest() = %q, want %q", att.NativeWorkerTarDigest(), in.NativeWorkerTarDigest)
	}
}

// TestAttestRejectsAWellFormedButWrongArtifactID is the mutation a cold review
// asked for. Before this, Attest checked only that the stamped ID LOOKED like a
// digest, so any 16-hex string was accepted and the "release artifact ID" was an
// unfalsifiable assertion. The ID is now re-derived from the stamped inputs, so an
// ID that those inputs do not produce cannot serve — whatever it looks like.
func TestAttestRejectsAWellFormedButWrongArtifactID(t *testing.T) {
	in := sampleInputs(ProfileNativeCapable)
	profile, id, blob := stampFor(in)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"one flipped nibble", flipFirstHexDigit(id)},
		{"another release's ID", ComputeArtifactID(sampleInputs(ProfileBAMLOnly))},
		{"a plausible hand-written ID", "0123456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id == id {
				t.Fatalf("test setup produced the correct ID; it would prove nothing")
			}
			if err := ValidateArtifactID(tc.id); err != nil {
				t.Fatalf("test setup produced a malformed ID (%v); the point is that a WELL-FORMED wrong ID fails", err)
			}
			_, err := attest(ProfileNativeCapable, profile, tc.id, blob, noEnv)
			if err == nil {
				t.Fatalf("attest accepted artifact ID %q, which its own stamped inputs do not produce", tc.id)
			}
			if !errors.Is(err, ErrArtifactIDMismatch) {
				t.Fatalf("error %v does not wrap ErrArtifactIDMismatch", err)
			}
		})
	}
}

// flipFirstHexDigit returns id with its first hex digit changed.
func flipFirstHexDigit(id string) string {
	if id == "" {
		return id
	}
	next := byte('0')
	if id[0] == '0' {
		next = '1'
	}
	return string(next) + id[1:]
}

// TestAttestRejectsInputsThatContradictTheBinary pins the SECOND, independent
// path to a profile contradiction: the stamped inputs carry the profile too, so
// patching only the profile stamp to agree with the binary still fails.
func TestAttestRejectsInputsThatContradictTheBinary(t *testing.T) {
	// Inputs describing a native_capable build, but stamped onto a BAML-only
	// binary with a matching profile stamp and a matching ID.
	in := sampleInputs(ProfileNativeCapable)
	blob := in.Marshal()
	id := ComputeArtifactID(in)

	_, err := attest(ProfileBAMLOnly, string(ProfileBAMLOnly), id, blob, noEnv)
	if err == nil {
		t.Fatal("attest accepted stamped inputs describing a different profile than the binary")
	}
	if !errors.Is(err, ErrProfileStampMismatch) {
		t.Fatalf("error %v does not wrap ErrProfileStampMismatch", err)
	}
}

// TestProvenanceParticipatesInTheArtifactID is the collision proof: two builds
// that select the SAME axes but come from different source must not share an ID.
// An axes-only digest — what this used to be — collides for almost every pair of
// consecutive releases, which is the defect this closes.
func TestProvenanceParticipatesInTheArtifactID(t *testing.T) {
	base := sampleInputs(ProfileNativeCapable)
	id := ComputeArtifactID(base)

	for name, mutate := range map[string]func(Inputs) Inputs{
		"source revision": func(in Inputs) Inputs { in.SourceRevision = "de1eefa68ed8"; return in },
		"source bundle digest": func(in Inputs) Inputs {
			in.SourceBundleDigest = "8877665544332211"
			return in
		},
		"native worker tar digest": func(in Inputs) Inputs {
			in.NativeWorkerTarDigest = "00ffeeddccbbaa99"
			return in
		},
	} {
		if got := ComputeArtifactID(mutate(base)); got == id {
			t.Errorf("changing the %s did not change the artifact ID; two different releases would share one identity", name)
		}
	}
}

// TestInputsRoundTripAndParseStrictly pins the stamped-inputs codec. It is the
// evidence the ID is checked against, so a decoder that tolerated drift would
// quietly make the check meaningless.
func TestInputsRoundTripAndParseStrictly(t *testing.T) {
	in := sampleInputs(ProfileNativeCapable)
	back, err := ParseInputs(in.Marshal())
	if err != nil {
		t.Fatalf("ParseInputs: %v", err)
	}
	if back != in.normalized() {
		t.Fatalf("round trip changed the inputs:\n got %+v\nwant %+v", back, in.normalized())
	}

	valid := in.canonical()
	for _, tc := range []struct {
		name string
		blob string
	}{
		{"not base64url", "!!!not base64!!!"},
		{"empty", base64.RawURLEncoding.EncodeToString([]byte(""))},
		{"missing trailing newline", base64.RawURLEncoding.EncodeToString([]byte(strings.TrimSuffix(valid, "\n")))},
		{"a dropped field", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "build_tags=subprocess,nativestreamserve,nativeworkerartifact\n", "", 1)))},
		{"a renamed key", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "profile=", "prof1le=", 1)))},
		{"a wrong schema", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, inputSchema, "baml-rest/artifact-profile/v1", 1)))},
		{"a non-boolean subprocess", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "subprocess=true", "subprocess=yes", 1)))},
		{"an unknown profile", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "profile=native_capable", "profile=native", 1)))},
		{"a URL smuggled into the revision", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "source_revision=27af8af5ae04", "source_revision=https://example.invalid/secret?k=v", 1)))},
		{"a path smuggled into a digest", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(valid, "source_bundle_digest=1122334455667788", "source_bundle_digest=/home/build/tree", 1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseInputs(tc.blob); err == nil {
				t.Fatalf("ParseInputs accepted %s", tc.name)
			}
		})
	}
}

// TestComputeBundleDigestIsDeterministicAndContentSensitive pins the source
// provenance component: the same tree always digests the same, and any content or
// path change moves it.
func TestComputeBundleDigestIsDeterministicAndContentSensitive(t *testing.T) {
	base := fstest.MapFS{
		"a/one.go": &fstest.MapFile{Data: []byte("package a\n")},
		"b/two.go": &fstest.MapFile{Data: []byte("package b\n")},
	}
	digest, err := ComputeBundleDigest(map[string]fs.FS{".": base})
	if err != nil {
		t.Fatalf("ComputeBundleDigest: %v", err)
	}
	if err := ValidateArtifactID(digest); err != nil {
		t.Fatalf("bundle digest is not a bounded token: %v", err)
	}
	again, err := ComputeBundleDigest(map[string]fs.FS{".": base})
	if err != nil {
		t.Fatalf("ComputeBundleDigest: %v", err)
	}
	if again != digest {
		t.Fatalf("ComputeBundleDigest is not deterministic: %q then %q", digest, again)
	}

	changedContent := fstest.MapFS{
		"a/one.go": &fstest.MapFile{Data: []byte("package a // edited\n")},
		"b/two.go": &fstest.MapFile{Data: []byte("package b\n")},
	}
	renamed := fstest.MapFS{
		"a/uno.go": &fstest.MapFile{Data: []byte("package a\n")},
		"b/two.go": &fstest.MapFile{Data: []byte("package b\n")},
	}
	for name, tree := range map[string]fstest.MapFS{"content": changedContent, "path": renamed} {
		got, err := ComputeBundleDigest(map[string]fs.FS{".": tree})
		if err != nil {
			t.Fatalf("ComputeBundleDigest(%s): %v", name, err)
		}
		if got == digest {
			t.Errorf("changing a %s did not change the bundle digest", name)
		}
	}

	// The mount prefix is part of the identity too: the same tree mounted
	// elsewhere is a different bundle.
	moved, err := ComputeBundleDigest(map[string]fs.FS{"elsewhere": base})
	if err != nil {
		t.Fatalf("ComputeBundleDigest(moved): %v", err)
	}
	if moved == digest {
		t.Error("changing a bundle mount prefix did not change the digest")
	}
}

// TestComputeFileDigestDistinguishesAbsentFromUnreadable pins that "not there"
// and "we could not read it" stay different facts: only the first is a valid
// attestation, and folding an I/O error into "absent" would stamp a provenance
// the artifact does not have.
func TestComputeFileDigestDistinguishesAbsentFromUnreadable(t *testing.T) {
	dir := t.TempDir()

	got, err := ComputeFileDigest(filepath.Join(dir, "no-such-file"))
	if err != nil {
		t.Fatalf("ComputeFileDigest(absent): %v", err)
	}
	if got != TarDigestAbsent {
		t.Errorf("absent file digest = %q, want %q", got, TarDigestAbsent)
	}

	present := filepath.Join(dir, "tar")
	if err := os.WriteFile(present, []byte("packaged module bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	digest, err := ComputeFileDigest(present)
	if err != nil {
		t.Fatalf("ComputeFileDigest(present): %v", err)
	}
	if err := ValidateArtifactID(digest); err != nil {
		t.Fatalf("file digest is not a bounded token: %v", err)
	}

	// A directory is readable-as-an-entry but not as a file: it must surface as an
	// error, not as "absent".
	if _, err := ComputeFileDigest(dir); err == nil {
		t.Error("ComputeFileDigest reported a directory as a digest or as absent")
	}
}

// TestAttestRejectsMislabelledProfile is the MUTATION BITE the slice requires:
// a stamp that contradicts what the binary demonstrably is must be a hard
// failure, in BOTH directions. A BAML-only binary stamped native_capable would
// otherwise report itself as the standard artifact on every dashboard while
// being incapable of ever serving natively; a native-capable binary stamped
// baml_only would hide a native-capable artifact from the rollout view.
func TestAttestRejectsMislabelledProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		derived Profile
		stamp   Profile
	}{
		{"baml-only binary claiming native", ProfileBAMLOnly, ProfileNativeCapable},
		{"native binary claiming baml-only", ProfileNativeCapable, ProfileBAMLOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fully self-consistent stamp for the CLAIMED profile: the ID matches
			// its inputs and the inputs match the profile stamp. The only thing
			// wrong is the binary it is stamped onto.
			profile, id, blob := stampFor(sampleInputs(tc.stamp))
			_, err := attest(tc.derived, profile, id, blob, noEnv)
			if err == nil {
				t.Fatalf("attest accepted a stamp of %q on a %q binary", tc.stamp, tc.derived)
			}
			if !errors.Is(err, ErrProfileStampMismatch) {
				t.Fatalf("error %v does not wrap ErrProfileStampMismatch", err)
			}
		})
	}
}

// TestAttestRejectsMalformedStamps pins the strict decoders: a builder that
// emits an unknown profile token, a non-hex / wrong-width artifact ID, or only
// half of the stamp pair fails the process rather than attesting a degenerate
// identity or leaking an unbounded token into a metric label.
func TestAttestRejectsMalformedStamps(t *testing.T) {
	good := sampleInputs(ProfileBAMLOnly)
	goodProfile, goodID, goodBlob := stampFor(good)

	for _, tc := range []struct {
		name    string
		profile string
		id      string
		blob    string
	}{
		{"unknown profile token", "native", goodID, goodBlob},
		{"empty-ish profile token", " ", goodID, goodBlob},
		{"id too short", goodProfile, "0123456789abcde", goodBlob},
		{"id too long", goodProfile, "0123456789abcdef0", goodBlob},
		{"id uppercase hex", goodProfile, strings.ToUpper(goodID), goodBlob},
		{"id not hex", goodProfile, "0123456789abcdeg", goodBlob},
		{"id is a branch name", goodProfile, "feat/debaml-s2x", goodBlob},
		{"profile without id or inputs", goodProfile, "", ""},
		{"id without profile or inputs", "", goodID, ""},
		{"inputs without profile or id", "", "", goodBlob},
		{"profile and id without inputs", goodProfile, goodID, ""},
		{"malformed inputs blob", goodProfile, goodID, "not-base64url!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := attest(ProfileBAMLOnly, tc.profile, tc.id, tc.blob, noEnv); err == nil {
				t.Fatalf("attest accepted profile=%q id=%q inputs=%q", tc.profile, tc.id, tc.blob)
			}
		})
	}
}

// TestAttestExpectationAlertsButNeverBlocks is the other half of the contract:
// an artifact/expectation mismatch ALERTS and still boots. The BAML-only
// artifact is the rollback lane, and a rollback into a slot still configured to
// expect the standard profile has to work — refusing to boot there would break
// the very reversal this slice exists to preserve.
func TestAttestExpectationAlertsButNeverBlocks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		derived   Profile
		expected  string
		wantAlert string
	}{
		{"rollback artifact in a standard slot", ProfileBAMLOnly, string(ProfileNativeCapable), AlertRollbackArtifactInStandardSlot},
		{"standard artifact in a rollback slot", ProfileNativeCapable, string(ProfileBAMLOnly), AlertStandardArtifactInRollbackSlot},
		{"standard artifact in a standard slot", ProfileNativeCapable, string(ProfileNativeCapable), ""},
		{"rollback artifact in a rollback slot", ProfileBAMLOnly, string(ProfileBAMLOnly), ""},
		{"explicitly cleared expectation", ProfileBAMLOnly, "", ""},
		{"whitespace-padded expectation", ProfileBAMLOnly, "  native_capable  ", AlertRollbackArtifactInStandardSlot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att, err := attest(tc.derived, "", "", "", envWith(ExpectedProfileEnv, tc.expected))
			if err != nil {
				t.Fatalf("attest returned an error for an expectation mismatch: %v", err)
			}
			if att.AlertReason != tc.wantAlert {
				t.Errorf("AlertReason = %q, want %q", att.AlertReason, tc.wantAlert)
			}
			if att.ExpectationViolated() != (tc.wantAlert != "") {
				t.Errorf("ExpectationViolated() = %v, want %v", att.ExpectationViolated(), tc.wantAlert != "")
			}
		})
	}
}

// TestAttestRejectsUnknownExpectation pins the strict decode of the operator
// setting. A typo must not read as "no expectation": that would silently
// disable the alert this slice adds.
func TestAttestRejectsUnknownExpectation(t *testing.T) {
	if _, err := attest(ProfileBAMLOnly, "", "", "", envWith(ExpectedProfileEnv, "native")); err == nil {
		t.Fatal("attest accepted an unknown expectation value")
	}
}

// TestAttestReadsTheLinkerStamp proves the exported Attest is actually wired to
// the -X targets, not just to the injectable helper the other tests drive.
// Without this, every stamp test above could pass while the linker stamp was
// read from nowhere.
func TestAttestReadsTheLinkerStamp(t *testing.T) {
	origProfile, origID, origInputs := stampedProfile, stampedArtifactID, stampedArtifactInputs
	t.Cleanup(func() {
		stampedProfile, stampedArtifactID, stampedArtifactInputs = origProfile, origID, origInputs
	})

	in := sampleInputs(ProfileNativeCapable)
	stampedProfile, stampedArtifactID, stampedArtifactInputs = stampFor(in)
	att, err := Attest(ProfileNativeCapable, noEnv)
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if !att.Stamped || att.ArtifactID != ComputeArtifactID(in) {
		t.Fatalf("Attest did not read the linker stamp: %+v", att)
	}
	if _, err := Attest(ProfileBAMLOnly, noEnv); !errors.Is(err, ErrProfileStampMismatch) {
		t.Fatalf("Attest did not cross-check the linker profile stamp: err = %v", err)
	}

	// The ID stamp is read from the linker too, not only the profile: corrupt it
	// and the verification must bite through the exported entry point.
	stampedArtifactID = flipFirstHexDigit(stampedArtifactID)
	if _, err := Attest(ProfileNativeCapable, noEnv); !errors.Is(err, ErrArtifactIDMismatch) {
		t.Fatalf("Attest did not verify the linker artifact-ID stamp: err = %v", err)
	}
}

// TestComputeArtifactIDIsReproducibleAndSelective pins the reproducible-build
// property: the ID depends on every build selection axis and on nothing else,
// so the same selection recomputes to the same ID from a build log while any
// change of what actually ships changes it.
func TestComputeArtifactIDIsReproducibleAndSelective(t *testing.T) {
	base := sampleInputs(ProfileNativeCapable)
	id := ComputeArtifactID(base)
	if err := ValidateArtifactID(id); err != nil {
		t.Fatalf("ComputeArtifactID produced an ID the validator rejects: %v", err)
	}
	if again := ComputeArtifactID(base); again != id {
		t.Fatalf("ComputeArtifactID is not deterministic: %q then %q", id, again)
	}

	mutations := map[string]Inputs{}
	m := base
	m.Profile = ProfileBAMLOnly
	mutations["profile"] = m
	m = base
	m.WorkerPackage = "root:./cmd/worker/"
	mutations["worker package"] = m
	m = base
	m.BuildTags = "subprocess"
	mutations["build tags"] = m
	m = base
	m.Subprocess = false
	mutations["subprocess"] = m
	m = base
	m.BAMLVersion = "0.224.0"
	mutations["baml version"] = m
	m = base
	m.AdapterVersion = "v0.215.0"
	mutations["adapter version"] = m

	seen := map[string]string{id: "base"}
	for name, in := range mutations {
		got := ComputeArtifactID(in)
		if prev, dup := seen[got]; dup {
			t.Errorf("mutating %s did not change the artifact ID (collides with %s)", name, prev)
		}
		seen[got] = name
	}
}

// TestCanonicalIsUnambiguous pins the field framing: two different tuples must
// not be able to render to the same canonical bytes by shifting content across
// a field boundary.
func TestCanonicalIsUnambiguous(t *testing.T) {
	a := Inputs{Profile: ProfileBAMLOnly, WorkerPackage: "root:./cmd/worker/", BuildTags: "x", Subprocess: true, BAMLVersion: "1", AdapterVersion: "2", SourceRevision: "r", SourceBundleDigest: "1122334455667788", NativeWorkerTarDigest: "99aabbccddeeff00"}
	b := a
	b.WorkerPackage = "root:./cmd/worker/\nbuild_tags=x"
	b.BuildTags = ""
	if ComputeArtifactID(a) == ComputeArtifactID(b) {
		t.Fatal("canonical rendering allows a field-boundary collision")
	}
}

// TestCollectorLabelsAreBoundedAndRedacted enforces the S1 telemetry contract on
// the S2 signal: every label name is predeclared, every label VALUE comes from a
// closed set (or is the fixed-width opaque artifact ID), and nothing that could
// carry a URL, client/model name, method, prompt, header or secret can reach a
// label. It walks the full attestation cross-product, so a future label added
// with an open value set fails here.
func TestCollectorLabelsAreBoundedAndRedacted(t *testing.T) {
	allowedLabelValues := map[string]map[string]bool{
		"profile":      {string(ProfileNativeCapable): true, string(ProfileBAMLOnly): true},
		"actual":       {string(ProfileNativeCapable): true, string(ProfileBAMLOnly): true},
		"expected":     {string(ProfileNativeCapable): true, string(ProfileBAMLOnly): true, ExpectationNone: true},
		"stamped":      {"true": true, "false": true},
		"alert_reason": {AlertRollbackArtifactInStandardSlot: true, AlertStandardArtifactInRollbackSlot: true, "none": true},
		// artifact_id is checked structurally below rather than by enumeration.
		"artifact_id": nil,
	}

	var attestations []Attestation
	for _, derived := range []Profile{ProfileNativeCapable, ProfileBAMLOnly} {
		for _, stamped := range []bool{false, true} {
			var stamp, id, blob string
			if stamped {
				stamp, id, blob = stampFor(sampleInputs(derived))
			}
			for _, expected := range []string{"", string(ProfileNativeCapable), string(ProfileBAMLOnly)} {
				lookup := noEnv
				if expected != "" {
					lookup = envWith(ExpectedProfileEnv, expected)
				}
				att, err := attest(derived, stamp, id, blob, lookup)
				if err != nil {
					t.Fatalf("attest(%q,stamped=%t,%q): %v", derived, stamped, expected, err)
				}
				attestations = append(attestations, att)
			}
		}
	}

	seenMetrics := map[string]bool{}
	for _, att := range attestations {
		reg := prometheus.NewRegistry()
		if err := Register(reg, att); err != nil {
			t.Fatalf("Register(%+v): %v", att, err)
		}
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		for _, mf := range families {
			seenMetrics[mf.GetName()] = true
			if !strings.HasPrefix(mf.GetName(), "baml_rest_debaml_") {
				t.Errorf("metric %q does not use the de-BAML metric prefix", mf.GetName())
			}
			for _, m := range mf.Metric {
				if len(m.Label) == 0 {
					t.Errorf("metric %q has no labels", mf.GetName())
				}
				for _, lp := range m.Label {
					allowed, known := allowedLabelValues[lp.GetName()]
					if !known {
						t.Errorf("metric %q carries undeclared label %q (bounded-label contract)", mf.GetName(), lp.GetName())
						continue
					}
					if lp.GetName() == "artifact_id" {
						if lp.GetValue() != UnstampedArtifactID {
							if err := ValidateArtifactID(lp.GetValue()); err != nil {
								t.Errorf("artifact_id label %q is not a bounded opaque token: %v", lp.GetValue(), err)
							}
						}
						continue
					}
					if !allowed[lp.GetValue()] {
						t.Errorf("metric %q label %s=%q is outside its declared value set", mf.GetName(), lp.GetName(), lp.GetValue())
					}
				}
			}
		}
	}

	want := []string{ArtifactInfoMetric, ExpectationMetric}
	sort.Strings(want)
	var got []string
	for name := range seenMetrics {
		got = append(got, name)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("gathered metric families = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gathered metric families = %v, want %v", got, want)
		}
	}
}

// TestExpectationViolationMetricValue pins the alert semantics of the metric an
// operator would page on: 1 exactly when the running artifact contradicts the
// expectation, 0 in every other case (including no expectation at all).
func TestExpectationViolationMetricValue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		derived  Profile
		expected string
		want     float64
	}{
		{"rollback artifact where standard expected", ProfileBAMLOnly, string(ProfileNativeCapable), 1},
		{"standard artifact where standard expected", ProfileNativeCapable, string(ProfileNativeCapable), 0},
		{"no expectation configured", ProfileBAMLOnly, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := noEnv
			if tc.expected != "" {
				lookup = envWith(ExpectedProfileEnv, tc.expected)
			}
			att, err := attest(tc.derived, "", "", "", lookup)
			if err != nil {
				t.Fatalf("attest: %v", err)
			}
			reg := prometheus.NewRegistry()
			if err := Register(reg, att); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if got := gaugeValue(t, reg, ExpectationMetric); got != tc.want {
				t.Errorf("%s = %v, want %v", ExpectationMetric, got, tc.want)
			}
			if got := gaugeValue(t, reg, ArtifactInfoMetric); got != 1 {
				t.Errorf("%s = %v, want 1", ArtifactInfoMetric, got)
			}
		})
	}
}

// gaugeValue returns the single sample of the named gauge family.
func gaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if len(mf.Metric) != 1 {
			t.Fatalf("metric %q has %d samples, want exactly 1 per process", name, len(mf.Metric))
		}
		var m *dto.Metric = mf.Metric[0]
		if m.Gauge == nil {
			t.Fatalf("metric %q is not a gauge", name)
		}
		return m.Gauge.GetValue()
	}
	t.Fatalf("metric %q was not gathered", name)
	return 0
}

// TestCanonicalEncodingIsBijective is the collision + round-trip pair for the
// artifact-ID encoding.
//
// The writer used to replace a newline with the two characters `\n` and there was
// no matching decoder. Two different builds — one whose field held a LITERAL
// backslash-n, one whose field held a REAL newline — therefore rendered
// identically and shared one release artifact ID, in the function whose entire
// job is to tell builds apart. Escaping in the writer alone is not an encoding;
// only a decoder that inverts it makes the rendering injective.
func TestCanonicalEncodingIsBijective(t *testing.T) {
	// Every field that can carry arbitrary build text. The digest/revision fields
	// are shape-validated elsewhere, so the adversarial values go here.
	adversarial := []string{
		`a\nb`,  // literal backslash + n
		"a\nb",  // a real newline
		`a\\nb`, // literal backslash backslash n
		`a\`,    // a trailing backslash
		"a\rb",  // a carriage return
		`a\rb`,  // literal backslash + r
		`=`,     // the key/value separator
		`k=v`,   // something that looks like another field
		``,      // empty
		`plain`, // the ordinary case
		"\n",    // nothing but a newline
		`\`,     // nothing but a backslash
	}

	// COLLISION: distinct values must never share an artifact ID.
	seen := map[string]string{}
	for _, v := range adversarial {
		in := sampleInputs(ProfileNativeCapable)
		in.BuildTags = v
		id := ComputeArtifactID(in)
		if prev, dup := seen[id]; dup {
			t.Errorf("build tags %q and %q share artifact ID %s; two different builds would carry one release identity", prev, v, id)
		}
		seen[id] = v
	}

	// ROUND TRIP: Marshal -> ParseInputs must return exactly what went in, for
	// every one of those values, in every free-text field.
	for _, v := range adversarial {
		for name, mutate := range map[string]func(Inputs) Inputs{
			"build tags":     func(in Inputs) Inputs { in.BuildTags = v; return in },
			"worker package": func(in Inputs) Inputs { in.WorkerPackage = v; return in },
			"baml version":   func(in Inputs) Inputs { in.BAMLVersion = v; return in },
		} {
			in := mutate(sampleInputs(ProfileNativeCapable))
			back, err := ParseInputs(in.Marshal())
			if err != nil {
				t.Errorf("ParseInputs after Marshal with %s=%q: %v", name, v, err)
				continue
			}
			if back != in.normalized() {
				t.Errorf("round trip with %s=%q changed the inputs:\n got %+v\nwant %+v", name, v, back, in.normalized())
			}
			// And the ID the stamped inputs re-derive must be the ID that was
			// stamped — this is the attestation path, not just the codec.
			if got := ComputeArtifactID(back); got != ComputeArtifactID(in) {
				t.Errorf("round trip with %s=%q changed the artifact ID: %s -> %s", name, v, ComputeArtifactID(in), got)
			}
		}
	}
}

// TestAttestAcceptsAdversarialInputValues drives the same values through the
// FULL attestation path — stamp, re-derive, compare — so the codec is proven
// where it is actually used rather than only in isolation.
func TestAttestAcceptsAdversarialInputValues(t *testing.T) {
	for _, v := range []string{`a\nb`, "a\nb", `a\`, "\n", `\`} {
		in := sampleInputs(ProfileNativeCapable)
		in.BuildTags = v
		profile, id, blob := stampFor(in)

		att, err := attest(ProfileNativeCapable, profile, id, blob, noEnv)
		if err != nil {
			t.Errorf("attest with build tags %q: %v", v, err)
			continue
		}
		if att.ArtifactID != id {
			t.Errorf("attest with build tags %q reported ID %q, want %q", v, att.ArtifactID, id)
		}
		if att.Inputs.BuildTags != v {
			t.Errorf("attest with build tags %q recovered %q", v, att.Inputs.BuildTags)
		}
	}
}

// TestUnescapeFieldValueIsStrict pins that the decoder rejects anything the
// encoder cannot have produced. A decoder that passed an unknown escape through
// would stop being an inverse, which puts the collision straight back.
func TestUnescapeFieldValueIsStrict(t *testing.T) {
	for _, bad := range []string{
		`\`,    // dangling escape
		`a\`,   // dangling escape at the end
		`\x`,   // unknown escape
		`\N`,   // wrong case
		`a\zb`, // unknown escape mid-value
		"a\nb", // a raw newline can never appear in an encoded value
		"a\rb", // nor a raw carriage return
	} {
		if got, err := unescapeFieldValue(bad); err == nil {
			t.Errorf("unescapeFieldValue(%q) accepted it and returned %q", bad, got)
		}
	}
	// And the inverse property itself, on the raw helpers.
	for _, v := range []string{``, `plain`, `a\nb`, "a\nb", `\`, "\r", `\\`} {
		back, err := unescapeFieldValue(escapeFieldValue(v))
		if err != nil {
			t.Errorf("unescape(escape(%q)): %v", v, err)
			continue
		}
		if back != v {
			t.Errorf("unescape(escape(%q)) = %q; the encoder and decoder are not inverses", v, back)
		}
	}
}
