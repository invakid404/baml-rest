// Package artifactprofile is the de-BAML serving-cutover S2 artifact identity:
// which WORKER PROFILE a shipped baml-rest artifact carries, the release
// artifact ID that names the exact build, and the startup attestation that
// proves the two agree.
//
// WHY THIS EXISTS. S2 makes the nanollmprepare-based NATIVE-CAPABLE worker the
// STANDARD deployable artifact and demotes the zero-options BAML-only
// cmd/worker to an explicit ROLLBACK artifact. That is a change of artifact
// OWNERSHIP, not of serving behaviour: the S1 cohort policy is still empty, so
// every request declines pre-socket and BAML serves 100% of traffic on either
// artifact. The one thing an operator therefore cannot see from traffic alone
// is WHICH artifact is actually running — so it has to be attested.
//
// TWO INDEPENDENT FACTS, CROSS-CHECKED. The profile is never a free-text label
// a build can assert about itself:
//
//   - the DERIVED profile is a property of the running binary. The worker
//     derives it from whether a native engine is actually linked in (a non-nil
//     NativeCapability, or the static NativeBuildCapable advertisement the
//     flag-off native artifact carries); the host derives it from the
//     `nativeworkerartifact` build tag that cmd/build/build.sh sets exactly when
//     it embeds an isolated-module worker.
//   - the STAMPED profile is what the BUILD claims, injected by build.sh via
//     `-ldflags -X` into stampedProfile/stampedArtifactID.
//
// Attest fails CLOSED when a present stamp contradicts the derived fact. That
// is what makes a mislabelled artifact — a BAML-only binary stamped
// native_capable, or the reverse — a startup failure rather than a silent lie
// on a dashboard, and it is the property the mutation tests bite on.
//
// An UNSTAMPED binary (a plain `go build ./cmd/worker`, or any deploy path this
// repository does not know about) is NOT an error: it attests its derived
// profile with ArtifactID "unstamped". S2 must not hard-break an entrypoint it
// cannot see, so absence of a stamp is absence of a claim, never a conflict.
//
// SEPARATELY, the operator EXPECTATION. BAML_REST_EXPECTED_ARTIFACT_PROFILE
// declares which profile the deployment slot is supposed to run. A mismatch is
// an ALERT (a bounded reason on the startup log plus a 1-valued expectation
// metric), NEVER a refusal to boot: the whole point of keeping the BAML-only
// artifact is that rolling back to it must always work, including into a slot
// still configured to expect the standard profile. Refusing to boot there would
// break the rollback this slice exists to preserve.
//
// WHAT THE RELEASE ARTIFACT ID IS. It is a digest over the artifact's PROVENANCE,
// not merely over the flags that selected it. A cold review pointed out that an ID
// derived only from build axes (profile, worker package, tags, versions) collides
// across different source releases that happen to select the same axes — which is
// most releases — so it could not identify a release at all. The digest therefore
// also covers:
//
//   - SourceRevision: the release revision the builder was told it is building;
//   - SourceBundleDigest: a content digest over the ENTIRE embedded source bundle
//     cmd/build lays into the build context — the actual root-module bytes that
//     become this artifact;
//   - NativeWorkerTarDigest: the content digest of cmd/build/nativeworker_module.tar,
//     the opaque packaged nativeserve + nanollmprepare source the native-capable
//     worker binary is compiled from.
//
// AND THE ID IS VERIFIED, not merely shaped. The build stamps the canonical INPUTS
// alongside the ID; at startup Attest re-derives the ID from those inputs and
// rejects any mismatch. A well-formed but wrong 16-hex ID therefore fails to
// serve, instead of being accepted because it looked like a digest.
//
// LABEL POLICY (consistent with the S1 telemetry contract). Everything this
// package exposes as a metric label is a predeclared bounded token: profile is
// one of two values, expectation one of three, stamped one of two, and the
// artifact ID is a fixed-width opaque digest of BUILD SELECTION AXES — never a
// URL, client name, model, alias, method, prompt, header or secret, and never
// per-request. There is exactly one artifact-ID value per running process, so
// it carries no request cardinality at all.
package artifactprofile

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Profile is the bounded artifact-profile enum. It has exactly two values, and
// every metric label / log field that carries a profile carries one of them.
type Profile string

const (
	// ProfileNativeCapable is the STANDARD S2 artifact: the worker built from
	// the isolated internal/nativebody/nanollmprepare module, which links the
	// native engine and CAN serve natively once a cohort is enrolled. With the
	// S1 policy empty it serves nothing natively; with BAML_REST_USE_DEBAML
	// falsy it performs no native work at all.
	ProfileNativeCapable Profile = "native_capable"

	// ProfileBAMLOnly is the explicit ROLLBACK artifact: the zero-options root
	// cmd/worker, which links no native engine and can never serve natively
	// under any flag or policy.
	ProfileBAMLOnly Profile = "baml_only"
)

// ExpectedProfileEnv names the operator expectation for the deployment slot.
// Unset means "no expectation declared" — the common case for a developer build
// and for any deploy path that has not been taught about S2 — and produces no
// alert. Set, it must be exactly one of the two Profile values; a typo is a
// configuration error, not a silently ignored string.
const ExpectedProfileEnv = "BAML_REST_EXPECTED_ARTIFACT_PROFILE"

// UnstampedArtifactID is the ArtifactID of a binary the builder did not stamp.
// It is a bounded sentinel rather than an empty label so the info metric always
// carries a value and "nobody stamped this" is visibly different from "the
// stamp is empty".
const UnstampedArtifactID = "unstamped"

// ExpectationNone is the bounded label used for the expectation dimension when
// no expectation is configured.
const ExpectationNone = "none"

// Provenance sentinels. Each names a specific, bounded "this input was genuinely
// not available" state, so an absent provenance component is a DECLARED value that
// still participates in the digest — never an empty string that silently collapses
// two different builds onto one ID.
const (
	// ProvenanceUnset is the source revision / source bundle digest of a build the
	// cmd/build front end did not drive (a hand-run cmd/build/build.sh). It is
	// honest rather than fatal: build.sh must stay runnable on its own.
	ProvenanceUnset = "unset"
	// TarDigestAbsent is the native-worker tar digest of a build context that
	// carries no packaged tar (a trimmed BAML-only bundle).
	TarDigestAbsent = "absent"
)

// Alert reasons. Bounded, and the ONLY non-empty values AlertReason can take.
const (
	// AlertRollbackArtifactInStandardSlot fires when the running artifact is the
	// BAML-only rollback artifact but the slot expects the standard
	// native-capable one. This is the alert §S2 of the cutover scope requires:
	// "alert if a BAML-only ordinary artifact is selected where the standard
	// profile is expected". It is safe (BAML serves everything) but it means the
	// slot is NOT running the standard artifact.
	AlertRollbackArtifactInStandardSlot = "baml_only_where_native_capable_expected"

	// AlertStandardArtifactInRollbackSlot fires for the reverse: a slot pinned to
	// the rollback artifact is running the native-capable one. Also safe with an
	// empty cohort policy, but it means a deliberate rollback did not take.
	AlertStandardArtifactInRollbackSlot = "native_capable_where_baml_only_expected"
)

// artifactIDLen is the hex width of a release artifact ID: 8 bytes of SHA-256.
// Long enough that two distinct build-selection tuples do not collide in
// practice, short enough to read in a log line and to keep the label a
// fixed-width opaque token.
const artifactIDLen = 16

// ErrProfileStampMismatch is returned when a binary carries a build stamp that
// contradicts what the binary demonstrably IS. It is deliberately a distinct
// error: callers fail closed on it.
var ErrProfileStampMismatch = errors.New("artifactprofile: build profile stamp contradicts the running artifact")

// ErrArtifactIDMismatch is returned when the stamped release artifact ID is not
// the ID its own stamped inputs produce. It is what turns the ID from an
// unverifiable label into a checked claim: a well-formed but wrong ID fails here.
var ErrArtifactIDMismatch = errors.New("artifactprofile: stamped artifact ID does not match the stamped build inputs")

// stampedProfile and stampedArtifactID are the BUILD's claim about this binary,
// injected by cmd/build/build.sh with
//
//	-ldflags "-X github.com/invakid404/baml-rest/internal/artifactprofile.stampedProfile=... \
//	          -X github.com/invakid404/baml-rest/internal/artifactprofile.stampedArtifactID=..."
//
// They are unexported so nothing in the program can set them at runtime: the
// only writer is the linker, and the only reader is Attest. An unstamped build
// leaves them empty, which is a valid (claim-free) state — see the package doc.
var (
	stampedProfile    string
	stampedArtifactID string
	// stampedArtifactInputs is the base64url rendering of the canonical Inputs the
	// artifact ID was computed from. It is what makes the ID VERIFIABLE at startup:
	// Attest re-derives the ID from it and refuses a mismatch. Without it the ID
	// would be an unfalsifiable assertion about the build.
	stampedArtifactInputs string
)

// Attestation is the resolved artifact identity of the running process.
type Attestation struct {
	// Profile is the DERIVED profile — what this binary actually is. When a
	// stamp is present it has already been proven equal to the stamp.
	Profile Profile

	// ArtifactID is the release artifact ID from the build stamp, or
	// UnstampedArtifactID.
	ArtifactID string

	// Stamped reports whether the builder stamped this binary at all.
	Stamped bool

	// Expected is the operator expectation from ExpectedProfileEnv, or "" when
	// none is configured.
	Expected Profile

	// AlertReason is "" when the artifact matches the expectation (or no
	// expectation is configured), and one of the bounded Alert* constants
	// otherwise. It is an ALERT, not a failure: see the package doc.
	AlertReason string

	// Inputs are the verified build inputs the artifact ID was derived from, when
	// the binary is stamped. They are the provenance an operator joins a running
	// process back to a release with. Reported on the startup LOG, never as metric
	// labels — the bounded artifact ID is the label.
	Inputs Inputs
}

// SourceRevision returns the release revision this artifact was built from, or
// ProvenanceUnset.
func (a Attestation) SourceRevision() string {
	if a.Inputs.SourceRevision == "" {
		return ProvenanceUnset
	}
	return a.Inputs.SourceRevision
}

// SourceBundleDigest returns the content digest of the embedded source bundle
// this artifact was built from, or ProvenanceUnset.
func (a Attestation) SourceBundleDigest() string {
	if a.Inputs.SourceBundleDigest == "" {
		return ProvenanceUnset
	}
	return a.Inputs.SourceBundleDigest
}

// NativeWorkerTarDigest returns the content digest of the packaged native-worker
// module tar this artifact was built from, or TarDigestAbsent.
func (a Attestation) NativeWorkerTarDigest() string {
	if a.Inputs.NativeWorkerTarDigest == "" {
		return TarDigestAbsent
	}
	return a.Inputs.NativeWorkerTarDigest
}

// ExpectationLabel returns the bounded label for the expectation dimension:
// the expected profile, or ExpectationNone when none is configured.
func (a Attestation) ExpectationLabel() string {
	if a.Expected == "" {
		return ExpectationNone
	}
	return string(a.Expected)
}

// StampedLabel returns the bounded "true"/"false" label for the stamp
// dimension.
func (a Attestation) StampedLabel() string {
	if a.Stamped {
		return "true"
	}
	return "false"
}

// ExpectationViolated reports whether the running artifact contradicts a
// configured expectation.
func (a Attestation) ExpectationViolated() bool { return a.AlertReason != "" }

// DeriveProfile maps the running binary's OBSERVED native capability to a
// profile. nativeCapable must come from a fact about the binary — a linked
// native engine, or the build tag that selects the native-capable embed — never
// from a flag, an env var or a stamp.
func DeriveProfile(nativeCapable bool) Profile {
	if nativeCapable {
		return ProfileNativeCapable
	}
	return ProfileBAMLOnly
}

// ParseProfile strictly decodes a profile token. Anything that is not exactly
// one of the two enum values is an error — there is no "unknown" fallback,
// because every producer of this token is a build or an operator setting that
// must be corrected rather than tolerated.
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case ProfileNativeCapable:
		return ProfileNativeCapable, nil
	case ProfileBAMLOnly:
		return ProfileBAMLOnly, nil
	default:
		return "", fmt.Errorf("artifactprofile: unknown profile %q (want %q or %q)",
			s, ProfileNativeCapable, ProfileBAMLOnly)
	}
}

// ValidateArtifactID strictly decodes a release artifact ID: exactly
// artifactIDLen lowercase hex characters. The strictness is what keeps the
// metric label a fixed-width opaque token — a builder that tried to stamp a
// branch name, a path or a timestamp fails here instead of leaking it into
// telemetry.
func ValidateArtifactID(s string) error {
	if len(s) != artifactIDLen {
		return fmt.Errorf("artifactprofile: artifact ID %q has length %d, want %d hex chars", s, len(s), artifactIDLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("artifactprofile: artifact ID %q is not lowercase hex", s)
	}
	return nil
}

// Attest resolves the running process's artifact identity from the linker
// stamp, the derived capability fact, and the operator expectation.
//
// lookupEnv is the environment reader (os.LookupEnv in production); it is a
// parameter so the whole contract is testable without mutating process state.
//
// It returns an error — and callers MUST fail closed on it — when:
//
//   - the stamped profile is present but not a known profile token;
//   - the stamped profile contradicts the derived profile (ErrProfileStampMismatch);
//   - the stamped artifact ID is present but malformed;
//   - a profile is stamped without an artifact ID, or the reverse (a half stamp
//     means the builder's ldflags are broken, and a partly-attested artifact is
//     not attested);
//   - ExpectedProfileEnv is set to something that is not a profile token.
//
// It does NOT return an error for an expectation MISMATCH: that is reported
// through AlertReason so a rollback into a standard-expecting slot still boots.
func Attest(derived Profile, lookupEnv func(string) (string, bool)) (Attestation, error) {
	return attest(derived, stampedProfile, stampedArtifactID, stampedArtifactInputs, lookupEnv)
}

// attest is Attest with the stamp injected, so tests can drive every stamp
// combination without relying on linker flags.
func attest(derived Profile, stampProfile, stampID, stampInputs string, lookupEnv func(string) (string, bool)) (Attestation, error) {
	if _, err := ParseProfile(string(derived)); err != nil {
		return Attestation{}, fmt.Errorf("artifactprofile: derived profile is invalid: %w", err)
	}

	att := Attestation{Profile: derived, ArtifactID: UnstampedArtifactID}

	// A stamp is all-or-nothing across all THREE parts. A partial stamp means the
	// builder emitted some -X flags and lost others, which would attest a profile
	// with no release identity, or an identity nothing can verify — none of which
	// is an attestation.
	switch {
	case stampProfile == "" && stampID == "" && stampInputs == "":
		// Unstamped: no claim to check. Not an error.
	case stampProfile == "" || stampID == "" || stampInputs == "":
		return Attestation{}, fmt.Errorf(
			"artifactprofile: incomplete build stamp (profile=%q, artifact_id=%q, inputs_present=%t); all three -X stamps must be set together",
			stampProfile, stampID, stampInputs != "")
	default:
		claimed, err := ParseProfile(stampProfile)
		if err != nil {
			return Attestation{}, fmt.Errorf("artifactprofile: malformed build profile stamp: %w", err)
		}
		if err := ValidateArtifactID(stampID); err != nil {
			return Attestation{}, fmt.Errorf("artifactprofile: malformed build artifact-ID stamp: %w", err)
		}
		if claimed != derived {
			return Attestation{}, fmt.Errorf(
				"%w: build stamped profile=%q but this binary is %q",
				ErrProfileStampMismatch, claimed, derived)
		}
		inputs, err := ParseInputs(stampInputs)
		if err != nil {
			return Attestation{}, fmt.Errorf("artifactprofile: malformed build inputs stamp: %w", err)
		}
		// The inputs carry the profile too, so they are a second, independent path
		// to the same contradiction: a build that patched the profile stamp alone
		// still fails here.
		if inputs.Profile != derived {
			return Attestation{}, fmt.Errorf(
				"%w: stamped build inputs describe profile=%q but this binary is %q",
				ErrProfileStampMismatch, inputs.Profile, derived)
		}
		// THE VERIFICATION. Re-derive the ID from the stamped inputs; a stamped ID
		// that is well-formed but is not the ID those inputs produce is rejected.
		if recomputed := ComputeArtifactID(inputs); recomputed != stampID {
			return Attestation{}, fmt.Errorf(
				"%w: stamped artifact_id=%q, but the stamped inputs produce %q",
				ErrArtifactIDMismatch, stampID, recomputed)
		}
		att.Stamped = true
		att.ArtifactID = stampID
		att.Inputs = inputs
	}

	if raw, ok := lookupEnv(ExpectedProfileEnv); ok {
		trimmed := strings.TrimSpace(raw)
		// An explicitly empty value is treated as "no expectation": a deploy
		// system that exports the variable unconditionally must be able to clear
		// it without tripping the strict decoder.
		if trimmed != "" {
			expected, err := ParseProfile(trimmed)
			if err != nil {
				return Attestation{}, fmt.Errorf("artifactprofile: %s is invalid: %w", ExpectedProfileEnv, err)
			}
			att.Expected = expected
			att.AlertReason = alertReason(expected, derived)
		}
	}

	return att, nil
}

// alertReason maps an (expected, actual) profile pair to the bounded alert
// reason. Pure, so the alert contract is unit-testable on its own.
func alertReason(expected, actual Profile) string {
	if expected == actual {
		return ""
	}
	if expected == ProfileNativeCapable {
		return AlertRollbackArtifactInStandardSlot
	}
	return AlertStandardArtifactInRollbackSlot
}

// Inputs are everything a release artifact ID is computed over: the BUILD
// SELECTION AXES (which worker binary ships and how it was compiled) plus the
// artifact's PROVENANCE (which source it was built from).
//
// The axes alone are not an identity — most releases select the same axes, so an
// axes-only digest collides across releases and identifies nothing. The three
// provenance fields below are what make the ID name a release.
//
// Nothing here is a timestamp, a hostname, a local path or a working-tree state,
// so the same source built the same way always produces the same ID: it is
// reproducible by construction, which is what lets a reviewer recompute it from a
// build log and compare.
type Inputs struct {
	// Profile is the artifact profile the build selected.
	Profile Profile
	// WorkerPackage is the package the worker binary was built from
	// (e.g. "./cmd/worker" in the isolated module, or the root "./cmd/worker/"),
	// or "" for an in-process build with no worker binary.
	WorkerPackage string
	// BuildTags is the comma-separated Go build tag list of the build.
	BuildTags string
	// Subprocess reports whether this is a subprocess (worker) build.
	Subprocess bool
	// BAMLVersion is the selected BAML version.
	BAMLVersion string
	// AdapterVersion is the selected framework adapter version.
	AdapterVersion string

	// SourceRevision is the release revision the builder was told it is building
	// (a commit SHA or tag), or ProvenanceUnset when the build front end did not
	// supply one. It is the human-joinable half of provenance.
	SourceRevision string
	// SourceBundleDigest is a content digest over the ENTIRE embedded source
	// bundle cmd/build lays into the build context — the actual root-module bytes
	// this artifact is compiled from — or ProvenanceUnset for a build.sh run that
	// cmd/build did not drive. This is what stops two different source releases
	// with the same selected axes from sharing an artifact ID.
	SourceBundleDigest string
	// NativeWorkerTarDigest is the content digest of
	// cmd/build/nativeworker_module.tar, the opaque packaged nativeserve +
	// nanollmprepare source the native-capable worker binary is compiled from, or
	// TarDigestAbsent when the build context carries no tar. It binds the artifact
	// ID to the out-of-go.work module content the root source digest cannot see.
	NativeWorkerTarDigest string
}

// inputFields is the FIXED field order of the canonical rendering and of the
// stamped inputs blob. It is declared once so canonical() and ParseInputs cannot
// drift apart: the writer emits these keys in this order, and the reader demands
// exactly these keys in exactly this order.
var inputFields = []string{
	"schema",
	"profile",
	"worker_package",
	"build_tags",
	"subprocess",
	"baml_version",
	"adapter_version",
	"source_revision",
	"source_bundle_digest",
	"native_worker_tar_digest",
}

// inputSchema versions the canonical rendering. A reader that meets a different
// schema refuses rather than guessing, so an artifact stamped by a builder with a
// different notion of "the inputs" cannot be silently half-verified.
const inputSchema = "baml-rest/artifact-profile/v2"

// normalized returns in with every optional provenance field replaced by its
// declared sentinel, so an unset field is a DECLARED value in the digest rather
// than an empty string two different builds could share.
func (in Inputs) normalized() Inputs {
	out := in
	if out.SourceRevision == "" {
		out.SourceRevision = ProvenanceUnset
	}
	if out.SourceBundleDigest == "" {
		out.SourceBundleDigest = ProvenanceUnset
	}
	if out.NativeWorkerTarDigest == "" {
		out.NativeWorkerTarDigest = TarDigestAbsent
	}
	return out
}

// values returns the canonical values in inputFields order.
func (in Inputs) values() []string {
	n := in.normalized()
	subprocess := "false"
	if n.Subprocess {
		subprocess = "true"
	}
	return []string{
		inputSchema,
		string(n.Profile),
		n.WorkerPackage,
		n.BuildTags,
		subprocess,
		n.BAMLVersion,
		n.AdapterVersion,
		n.SourceRevision,
		n.SourceBundleDigest,
		n.NativeWorkerTarDigest,
	}
}

// Marshal renders Inputs as the compact, ldflags-safe blob the build stamps
// alongside the artifact ID. base64url because the canonical rendering is
// newline-delimited and `-ldflags -X` cannot carry a newline.
func (in Inputs) Marshal() string {
	return base64.RawURLEncoding.EncodeToString([]byte(in.canonical()))
}

// ParseInputs strictly decodes a stamped inputs blob. Every deviation is an
// error: a wrong schema, a missing or extra field, a reordered field, a key that
// is not the expected one, a non-boolean subprocess value, or an unknown profile.
// Strictness is the point — this blob is the evidence the artifact ID is checked
// against, so a decoder that tolerated drift would make the check meaningless.
func ParseInputs(blob string) (Inputs, error) {
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return Inputs{}, fmt.Errorf("artifactprofile: inputs blob is not base64url: %w", err)
	}
	text := string(raw)
	if !strings.HasSuffix(text, "\n") {
		return Inputs{}, errors.New("artifactprofile: inputs blob is not newline-terminated")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != len(inputFields) {
		return Inputs{}, fmt.Errorf("artifactprofile: inputs blob has %d fields, want %d", len(lines), len(inputFields))
	}

	values := make([]string, len(lines))
	for i, line := range lines {
		key, escaped, ok := strings.Cut(line, "=")
		if !ok {
			return Inputs{}, fmt.Errorf("artifactprofile: inputs field %d is not key=value", i)
		}
		if key != inputFields[i] {
			return Inputs{}, fmt.Errorf("artifactprofile: inputs field %d is %q, want %q", i, key, inputFields[i])
		}
		value, err := unescapeFieldValue(escaped)
		if err != nil {
			return Inputs{}, fmt.Errorf("artifactprofile: inputs field %q: %w", key, err)
		}
		values[i] = value
	}
	if values[0] != inputSchema {
		return Inputs{}, fmt.Errorf("artifactprofile: inputs schema is %q, want %q", values[0], inputSchema)
	}
	profile, err := ParseProfile(values[1])
	if err != nil {
		return Inputs{}, err
	}
	var subprocess bool
	switch values[4] {
	case "true":
		subprocess = true
	case "false":
		subprocess = false
	default:
		return Inputs{}, fmt.Errorf("artifactprofile: inputs subprocess is %q, want \"true\" or \"false\"", values[4])
	}

	in := Inputs{
		Profile:               profile,
		WorkerPackage:         values[2],
		BuildTags:             values[3],
		Subprocess:            subprocess,
		BAMLVersion:           values[5],
		AdapterVersion:        values[6],
		SourceRevision:        values[7],
		SourceBundleDigest:    values[8],
		NativeWorkerTarDigest: values[9],
	}
	if err := in.Validate(); err != nil {
		return Inputs{}, err
	}
	return in, nil
}

// Validate checks the bounded shape of every provenance component, so a builder
// cannot stamp a path, a URL or a branch name where a digest belongs.
func (in Inputs) Validate() error {
	n := in.normalized()
	if _, err := ParseProfile(string(n.Profile)); err != nil {
		return err
	}
	for _, d := range []struct {
		name, value, absent string
	}{
		{"source_bundle_digest", n.SourceBundleDigest, ProvenanceUnset},
		{"native_worker_tar_digest", n.NativeWorkerTarDigest, TarDigestAbsent},
	} {
		if d.value == d.absent {
			continue
		}
		if err := ValidateArtifactID(d.value); err != nil {
			return fmt.Errorf("artifactprofile: %s: %w", d.name, err)
		}
	}
	if n.SourceRevision != ProvenanceUnset && !isBoundedRevision(n.SourceRevision) {
		return fmt.Errorf("artifactprofile: source_revision %q is not a bounded revision token", n.SourceRevision)
	}
	return nil
}

// isBoundedRevision reports whether s is a plausible, bounded revision token: a
// commit SHA, a tag or a short branch-ish name. It exists to keep an arbitrary
// string — a path, a URL, a message — out of the attestation.
func isBoundedRevision(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '.' || c == '_' || c == '-' || c == '/' || c == '+':
		default:
			return false
		}
	}
	return true
}

// ComputeBundleDigest returns a deterministic content digest over a set of
// embedded source trees, keyed by their mount prefix. It is the artifact's ROOT
// SOURCE provenance: the same bytes that are laid into the build context and
// compiled, rather than a version string that says nothing about them.
//
// Determinism comes from sorting both the prefixes and each tree's paths, and
// from length-delimiting every path and every file body, so no two distinct trees
// can render to the same byte stream.
func ComputeBundleDigest(sources map[string]fs.FS) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", "baml-rest/source-bundle/v1")

	prefixes := make([]string, 0, len(sources))
	for prefix := range sources {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	for _, prefix := range prefixes {
		fmt.Fprintf(h, "tree %d %s\n", len(prefix), prefix)
		var paths []string
		err := fs.WalkDir(sources[prefix], ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("artifactprofile: walking bundle %q: %w", prefix, err)
		}
		sort.Strings(paths)
		for _, p := range paths {
			f, err := sources[prefix].Open(p)
			if err != nil {
				return "", fmt.Errorf("artifactprofile: opening %q in bundle %q: %w", p, prefix, err)
			}
			body, err := io.ReadAll(f)
			closeErr := f.Close()
			if err != nil {
				return "", fmt.Errorf("artifactprofile: reading %q in bundle %q: %w", p, prefix, err)
			}
			if closeErr != nil {
				return "", fmt.Errorf("artifactprofile: closing %q in bundle %q: %w", p, prefix, closeErr)
			}
			fmt.Fprintf(h, "file %d %s %d\n", len(p), path.Clean(p), len(body))
			h.Write(body)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:artifactIDLen], nil
}

// ComputeFileDigest returns the bounded content digest of one file, or
// (TarDigestAbsent, nil) when the file does not exist. A genuine I/O error is
// returned rather than being folded into "absent": "we could not read it" and
// "it is not there" are different facts, and only the second is a valid
// attestation.
func ComputeFileDigest(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return TarDigestAbsent, nil
		}
		return "", fmt.Errorf("artifactprofile: opening %s: %w", filePath, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("artifactprofile: reading %s: %w", filePath, err)
	}
	return hex.EncodeToString(h.Sum(nil))[:artifactIDLen], nil
}

// canonical renders Inputs as the exact bytes hashed into an artifact ID. The
// field order is FIXED here (not map iteration) so the digest is stable across
// Go versions and platforms, and every field sits on its own `key=` line so no
// two distinct tuples can render to the same string by shifting content across
// field boundaries.
//
// Values are escaped with escapeFieldValue, which unescapeFieldValue INVERTS.
// That pairing is what makes this an encoding rather than a sanitiser: an
// earlier version replaced a newline with the two characters `\n` in the writer
// and had no matching decoder, so a value containing a literal backslash-n and a
// value containing a real newline rendered identically — two different builds
// with one artifact ID, in the function whose entire job is to tell builds apart.
func (in Inputs) canonical() string {
	values := in.values()
	var b strings.Builder
	for i, key := range inputFields {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(escapeFieldValue(values[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

// escapeFieldValue renders one field value into the line-oriented canonical
// form. It is INJECTIVE: `\` becomes `\\` and a newline becomes `\n`, so the
// two-character sequence `\n` in the input and a real newline in the input have
// distinct renderings (`\\n` and `\n`), and no rendered value can contain a raw
// newline to break the framing.
func escapeFieldValue(v string) string {
	if !strings.ContainsAny(v, "\\\n\r") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 8)
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// unescapeFieldValue is the exact inverse of escapeFieldValue.
//
// It is STRICT: a backslash must introduce one of the three escapes this encoder
// produces, and a trailing backslash is an error. A decoder that passed an
// unknown escape through would stop being an inverse, which would put the
// collision back exactly where it was.
func unescapeFieldValue(v string) (string, error) {
	if !strings.Contains(v, `\`) {
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("artifactprofile: field value contains a raw newline; the canonical form escapes them")
		}
		return v, nil
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '\\' {
			if c == '\n' || c == '\r' {
				return "", fmt.Errorf("artifactprofile: field value contains a raw newline; the canonical form escapes them")
			}
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(v) {
			return "", fmt.Errorf("artifactprofile: field value ends with a dangling escape")
		}
		switch v[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			return "", fmt.Errorf("artifactprofile: unknown escape %q in a field value", `\`+string(v[i]))
		}
	}
	return b.String(), nil
}

// ComputeArtifactID returns the reproducible release artifact ID for a build:
// the first artifactIDLen hex characters of the SHA-256 of the canonical
// rendering of its selection axes AND provenance.
//
// The build stamps both this ID and the Inputs it came from, and Attest
// re-derives the ID at startup — so this function is also the verifier, and a
// stamped ID that these inputs do not produce cannot serve.
func ComputeArtifactID(in Inputs) string {
	sum := sha256.Sum256([]byte(in.canonical()))
	return hex.EncodeToString(sum[:])[:artifactIDLen]
}
