package admission

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// De-BAML serving cutover S1 — DEFAULT-DENY cohort admission + the privacy-safe
// configuration inventory.
//
// # What this is, and what it deliberately is NOT
//
// This file adds the closed SURFACE + COHORT identity check the serving-cutover
// scope requires at the admission boundary, evaluated BEFORE any native work
// (before nanollm New/render/Prepare and before any claim), IN ADDITION to every
// admission predicate that already exists. It relaxes NOTHING: the gate can only
// turn an otherwise-admissible request into a pre-socket decline to BAML.
//
// It is ADMISSION EVIDENCE, not a second kill switch. The one global revert stays
// BAML_REST_USE_DEBAML (StageFlag / ReasonFlagDisabled, layer 1): with the umbrella
// flag off a native-capable worker installs no capability, no runtime, no factory,
// runs no Prepare and opens no socket, so the flag remains the complete reversal.
// The cohort policy answers a different question — "has THIS configuration class
// been proven and approved to claim a native request on THIS surface?" — and its
// initial answer is NO for everything.
//
// # S1 ships the policy EMPTY
//
// [ProductionCohortGate] returns a versioned policy with ZERO enrollments and an
// EMPTY inventory, so every request resolves to cohort `none` and declines with
// (StageCohort, ReasonCohortNotEnrolled) before native work. S1 therefore flips
// NOTHING into native serving: a native-capable artifact running this code is
// externally equivalent to BAML transport. Enrollment is S3's slice, and it is a
// change to [ProductionCohortGate]'s two manifests — not to any gate below.
//
// # Where the gate is evaluated (one rule, no exceptions)
//
// Every exported admission entry point evaluates the gate IMMEDIATELY after the
// layer-1 build/flag/route/mode facts — which is exactly the point at which the
// SURFACE is known — and before anything else. Layer 1 is deliberately first so
// the kill switch and the route/mode facts keep reporting their own bounded
// reasons (a flag-off request must read as flag_disabled, never as a cohort
// decline). Everything after layer 1 is config/payload-shaped work the gate
// precedes. TestEveryAdmissionEntryPointIsCohortGated enumerates the entry points
// from the package source and fails if a new one appears without gate coverage.
//
// # Privacy contract (binding, and tested)
//
// Nothing in this file — and nothing derived from it that can reach a log line, a
// metric label, or a Decline detail — may carry a client name, an alias, a model
// name, a target URL, a prompt, a request/response body, a header, an API key, an
// Authorization value, a method name, or an arbitrary per-request schema
// fingerprint. The only identifiers here are small PREDECLARED buckets: a bounded
// opaque configuration fingerprint, a bounded cohort ID, a closed surface enum, a
// closed provider CLASS, and a bounded approval reference. The constructors
// enforce that structurally (strict decoders, closed charsets, hard count caps),
// so a manifest that tried to carry a secret fails to build rather than shipping
// one into a label.

// Surface is the CLOSED set of public serving surfaces the cutover distinguishes.
// It separates endpoint ownership without ever labelling a URI, a route template,
// or a method name.
//
// It is an integer enum, not a string, for two reasons: the zero value is
// deliberately NOT a valid surface (an unset surface can never be enrolled), and a
// value outside the set cannot be spelled by a caller — every Surface reaching the
// gate is DERIVED by the admission lane it runs in, never taken from the request.
type Surface uint8

const (
	// surfaceInvalid is the zero value: not a surface. It exists so an unset field
	// fails closed instead of aliasing onto a real surface. It is never enrollable
	// and never a metric label (see [Surface.Label]).
	surfaceInvalid Surface = iota
	// SurfaceDynamicCall is the dynamic unary `/call` surface (the generated
	// Baml_Rest_Dynamic route, ModeCall / ModeCallWithRaw). It is the fe-v1 target.
	SurfaceDynamicCall
	// SurfaceDynamicStream is the dynamic `/stream{,-with-raw}` surface.
	SurfaceDynamicStream
	// SurfaceStaticCall is the generated static `Request.<Method>` unary `/call`
	// surface (including its parse-only leg, which belongs to the same call).
	SurfaceStaticCall
	// SurfaceStaticStream is the generated static `/stream{,-with-raw}` surface.
	SurfaceStaticStream
	// SurfaceDirectParse is the direct `/parse/{method}` surface. No native
	// admission lane produces it today — worker/parse.go invokes BAML's method.Impl
	// / method.StreamImpl directly and there is no native seam there — so it is
	// declared, enrollable-in-principle, and proven unreachable by
	// TestNoAdmissionLaneReportsDirectParse. It exists so the enum is the scope's
	// full closed set rather than "the surfaces that happen to be wired".
	SurfaceDirectParse
)

// surfaceLabelInvalid is the out-of-band label for a Surface outside the closed
// set. It is UNREACHABLE by construction (every Surface is lane-derived from a
// constant), and it exists only so a hypothetical out-of-range value folds onto
// ONE bounded label instead of widening cardinality or being silently dropped.
const surfaceLabelInvalid = "invalid"

// Label returns the bounded, secret-free metric label for the surface.
func (s Surface) Label() string {
	switch s {
	case SurfaceDynamicCall:
		return "dynamic_call"
	case SurfaceDynamicStream:
		return "dynamic_stream"
	case SurfaceStaticCall:
		return "static_call"
	case SurfaceStaticStream:
		return "static_stream"
	case SurfaceDirectParse:
		return "direct_parse"
	default:
		return surfaceLabelInvalid
	}
}

// Valid reports whether s is one of the five declared surfaces.
func (s Surface) Valid() bool { return s.Label() != surfaceLabelInvalid }

// AllSurfaces returns the five declared surfaces in declaration order. It is the
// single source of truth the policy/inventory validators and the cardinality tests
// share, so a new surface cannot be added to the enum without appearing here.
func AllSurfaces() []Surface {
	return []Surface{
		SurfaceDynamicCall,
		SurfaceDynamicStream,
		SurfaceStaticCall,
		SurfaceStaticStream,
		SurfaceDirectParse,
	}
}

// CohortID is a small, PREDECLARED opaque bucket naming an approved configuration
// class. It is the only configuration identity that is ever a metric label or a
// policy value — never a client name, a model, a URL, or a per-request hash.
//
// Two values are reserved and can NEVER be enrolled:
//
//   - CohortNone: the request presented no configuration identity at all. This is
//     what every request resolves to in S1, because nothing assigns a fingerprint
//     yet (see [CohortInput.Fingerprint]).
//   - CohortUnrecognized: the request presented a syntactically valid fingerprint
//     that is not in the inventory. It folds every unknown identity onto ONE label
//     so a wire-influenced value can never widen cardinality.
type CohortID string

const (
	CohortNone         CohortID = "none"
	CohortUnrecognized CohortID = "unrecognized"
)

// reservedCohortIDs are the two non-enrollable resolution outcomes.
func reservedCohortIDs() []CohortID { return []CohortID{CohortNone, CohortUnrecognized} }

// maxCohortIDLen bounds a declared cohort ID. Cohort IDs are hand-written manifest
// entries, so the bound is a sanity fence, not a truncation policy — an over-long
// ID fails the constructor rather than being cut down.
const maxCohortIDLen = 32

// ParseCohortID strictly decodes a declared cohort ID: lowercase ASCII letters,
// digits and underscore, starting with a letter, at most maxCohortIDLen bytes, and
// not one of the reserved resolution outcomes. It never normalizes, lowercases, or
// trims — a value that is not already exactly right is REJECTED, so a manifest
// cannot smuggle in a label that differs from what was reviewed.
func parseCohortID(s string) (CohortID, error) {
	if s == "" {
		return "", fmt.Errorf("nativeserve/admission: cohort ID is empty")
	}
	if len(s) > maxCohortIDLen {
		return "", fmt.Errorf("nativeserve/admission: cohort ID is longer than %d bytes", maxCohortIDLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_'):
		default:
			return "", fmt.Errorf("nativeserve/admission: cohort ID has an unaccepted byte at offset %d", i)
		}
	}
	for _, r := range reservedCohortIDs() {
		if CohortID(s) == r {
			return "", fmt.Errorf("nativeserve/admission: cohort ID %q is reserved and can never be enrolled", s)
		}
	}
	if err := rejectCredentialShaped("cohort ID", s); err != nil {
		return "", err
	}
	return CohortID(s), nil
}

// credentialShapedPrefixes are the well-known prefixes of provider credentials and
// tokens. They are rejected in the identifier decoders as DEFENCE IN DEPTH: the
// charsets below already exclude URLs, headers and whitespace, but they cannot tell
// an opaque bucket name from a lowercase API key, and a fingerprint IS a metric
// label. Nothing legitimate needs to start with one of these, so refusing them
// costs nothing and closes the one shape that could otherwise slip a secret into a
// label through a hand-written manifest.
//
// The configuration-ID decoder no longer needs it (its form admits no letters at
// all), but the cohort-ID decoder does: cohort IDs are lowercase words, and
// `secret_key` is a lowercase word. This is a redaction rule with a bite test
// (TestCredentialShapedIdentifiersAreRejected mutates it away and requires the
// decoder suite to fail), not a comment.
var credentialShapedPrefixes = []string{
	"sk-", "sk_", "pk-", "pk_", "rk-", "ghp_", "gho_", "ghs_", "akia", "asia",
	"bearer", "token", "secret", "apikey", "api-key", "api_key", "password", "passwd",
}

// rejectCredentialShaped reports an error when s begins like a credential.
func rejectCredentialShaped(kind, s string) error {
	lower := strings.ToLower(s)
	for _, p := range credentialShapedPrefixes {
		if strings.HasPrefix(lower, p) {
			return fmt.Errorf("nativeserve/admission: %s starts with the credential-shaped prefix %q and is refused as a label value", kind, p)
		}
	}
	return nil
}

// ConfigFingerprint is the OPAQUE, BOUNDED configuration identity assigned at
// CONFIG LOAD by the control plane — a small predeclared bucket, deliberately NOT
// a per-request hash of anything. It is the join key an operator uses to look the
// configuration up in their own approved-configuration record, offline; it carries
// no information about the configuration itself.
//
// S1 assigns none: no config-load path populates [CohortInput.Fingerprint], so
// every production request resolves to CohortNone. Populating it is part of the
// slice that enrolls a cohort.
type ConfigFingerprint string

// proofConfigFingerprint is the opaque ID reserved for the gated proof suite. The ID
// itself is declared here, in untagged source, because the vocabulary below must be
// a single reviewed list; the GATE that enrolls it lives behind the
// `nanollm_integration` tag (cohort_test_support.go) and does not exist in a
// released build. A declared ID with no enrollment admits nothing.
const proofConfigFingerprint ConfigFingerprint = "cfg900"

// declaredConfigFingerprints is the FINITE, REVIEWED vocabulary of opaque
// configuration IDs this build knows. It is not a grammar and not a runtime string
// space: an ID that is not on this list cannot enter an inventory, cannot resolve to
// a cohort, and therefore can never become a metric label — whatever a manifest, a
// config-load spec, or a caller supplies.
//
// The IDs are deliberately opaque and deliberately meaningless HERE. `cfg001`…`cfg016`
// are unassigned production slots: the code attaches no configuration, no provider and
// no approval to any of them, because doing so would mean guessing a production
// identity the cutover scope explicitly forbids guessing. A DEPLOYMENT assigns meaning
// by declaring one through the config-load path (see LoadDeclaredInventory), pairing
// the slot with the cohort, surfaces, provider class and offline approval reference its
// operators actually approved. That is what keeps the mapping privacy-safe: the binary
// knows only the opaque slot, and the operator holds the slot -> real-configuration
// record in their own approved-configuration document.
//
// Sixteen slots is a cap, not a target. It bounds the label space a config reload can
// reach, on top of the per-load maxInventoryRecords cap.
func declaredConfigFingerprints() []ConfigFingerprint {
	return []ConfigFingerprint{
		"cfg001", "cfg002", "cfg003", "cfg004", "cfg005", "cfg006", "cfg007", "cfg008",
		"cfg009", "cfg010", "cfg011", "cfg012", "cfg013", "cfg014", "cfg015", "cfg016",
		// Reserved for the gated proof suite (the proof gate exists only under the
		// `nanollm_integration` build tag). A declared ID with no production
		// enrollment admits nothing.
		proofConfigFingerprint,
	}
}

// configFingerprintForm is the OPAQUE form every declared ID must take: the literal
// `cfg` followed by three to six decimal digits, e.g. `cfg001`.
//
// Digits-only after the prefix is the whole point. An earlier draft accepted any
// lowercase/digit/dash string, which — as a cold review demonstrated by publishing
// `fingerprint="gpt-4o-acme-tuned-2026"` from a live registry — happily spells a
// model name, and would just as happily spell a client name or a host label. A form
// that cannot carry letters cannot carry a name, a URL, a header value or a
// credential, so the privacy property is structural rather than a review promise.
var configFingerprintForm = regexp.MustCompile(`^cfg[0-9]{3,6}$`)

// parseConfigFingerprint strictly decodes an opaque configuration ID: it must take
// the opaque form AND be a member of the declared vocabulary. Both checks are
// required — the form alone would still be a (large) runtime string space, and
// membership alone would not stop a future entry from being a name.
//
// It is a strict decoder in the proof-integrity sense: no trimming, no case folding,
// no truncation, no "best effort" — anything else is an error.
func parseConfigFingerprint(s string) (ConfigFingerprint, error) {
	if s == "" {
		return "", fmt.Errorf("nativeserve/admission: configuration fingerprint is empty")
	}
	if !configFingerprintForm.MatchString(s) {
		return "", fmt.Errorf("nativeserve/admission: configuration fingerprint is not of the opaque form cfg<3-6 digits>")
	}
	for _, declared := range declaredConfigFingerprints() {
		if ConfigFingerprint(s) == declared {
			return declared, nil
		}
	}
	return "", fmt.Errorf("nativeserve/admission: configuration fingerprint is not in the declared vocabulary")
}

// ConfigProviderClass is the bounded provider CLASS an inventory record declares.
// It is a CLASS, never a client name, a base URL or a credential — the same closed
// set the attempts metric's provider label uses, minus its `other`/`unknown`
// folding buckets (a record must name a real class or fail to build).
// TestConfigProviderClassMatchesMetricProviderEnum keeps the two sets in lockstep.
type ConfigProviderClass string

const (
	ConfigProviderOpenAI    ConfigProviderClass = "openai"
	ConfigProviderAnthropic ConfigProviderClass = "anthropic"
	ConfigProviderBedrock   ConfigProviderClass = "bedrock"
	ConfigProviderCerebras  ConfigProviderClass = "cerebras"
	ConfigProviderCohere    ConfigProviderClass = "cohere"
)

// AllConfigProviderClasses returns the declared provider classes in a stable order.
func AllConfigProviderClasses() []ConfigProviderClass {
	return []ConfigProviderClass{
		ConfigProviderOpenAI,
		ConfigProviderAnthropic,
		ConfigProviderBedrock,
		ConfigProviderCerebras,
		ConfigProviderCohere,
	}
}

// Valid reports whether p is one of the declared provider classes.
func (p ConfigProviderClass) Valid() bool {
	for _, c := range AllConfigProviderClasses() {
		if p == c {
			return true
		}
	}
	return false
}

// ApprovalRef is the bounded reference to the OFFLINE approval record for a
// configuration class — the second half of the operator's join (fingerprint ->
// record -> their own approval document). Its grammar is deliberately tiny:
// `<UPPERCASE-TAG>-<digits>`, e.g. `DEBAML-673`. Nothing else fits, so an approval
// reference cannot become a smuggling channel for a client name, a URL, a note, or
// a credential.
type ApprovalRef string

// maxApprovalTagLen bounds the uppercase tag half of an approval reference.
const maxApprovalTagLen = 16

// ParseApprovalRef strictly decodes an approval reference. Like the other decoders
// in this file it rejects rather than repairs.
func parseApprovalRef(s string) (ApprovalRef, error) {
	dash := strings.IndexByte(s, '-')
	if dash <= 0 || dash == len(s)-1 {
		return "", fmt.Errorf("nativeserve/admission: approval reference must be <TAG>-<number>")
	}
	tag, num := s[:dash], s[dash+1:]
	if len(tag) > maxApprovalTagLen {
		return "", fmt.Errorf("nativeserve/admission: approval reference tag is longer than %d bytes", maxApprovalTagLen)
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] < 'A' || tag[i] > 'Z' {
			return "", fmt.Errorf("nativeserve/admission: approval reference tag has an unaccepted byte at offset %d", i)
		}
	}
	if len(num) > 9 {
		return "", fmt.Errorf("nativeserve/admission: approval reference number is longer than 9 digits")
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return "", fmt.Errorf("nativeserve/admission: approval reference number has an unaccepted byte at offset %d", i)
		}
	}
	return ApprovalRef(s), nil
}

// ConfigRecord is the OPERATOR-VISIBLE configuration record an opaque fingerprint
// maps to. It is the whole privacy-safe inventory row: every field is a bounded
// predeclared bucket, so the complete record can be published to a control-plane
// dashboard or a metric without any redaction step.
//
// What it deliberately does NOT contain, and must never gain: a client/alias name,
// a model name, a base URL or any URL, a prompt, a request/response body, a header
// name or value, an API key, an Authorization value, a BAML method name, or a
// schema fingerprint. An operator joins those in THEIR OWN approved-configuration
// record, keyed by Fingerprint and Approval.
type ConfigRecord struct {
	// Fingerprint is the opaque bounded configuration ID (the join key).
	Fingerprint ConfigFingerprint
	// Cohort is the bounded cohort bucket this configuration class belongs to. It,
	// not the fingerprint, is the per-request metric label.
	Cohort CohortID
	// Surfaces is the closed, non-empty set of surfaces this configuration class is
	// declared for. It is what makes a WRONG-surface request a decline: a class
	// declared for dynamic_call is not enrollable on dynamic_stream.
	Surfaces []Surface
	// Provider is the bounded provider CLASS (never a client name or endpoint).
	Provider ConfigProviderClass
	// Approval is the bounded reference to the offline approval record.
	Approval ApprovalRef
}

// maxInventoryRecords is the HARD cap on declared inventory rows. The inventory is
// the only thing that can grow the cohort/fingerprint label sets, so capping it
// caps de-BAML's whole configuration-label cardinality at a number that is checked
// in a test rather than asserted in a comment.
const maxInventoryRecords = 32

// ConfigInventory is the privacy-safe control-plane record: an immutable map from
// opaque configuration fingerprint to its operator-visible [ConfigRecord]. It is
// built once at config load, validated strictly, and never mutated afterwards.
//
// A nil *ConfigInventory is a valid EMPTY inventory (every lookup misses), which is
// the fail-closed default.
type ConfigInventory struct {
	byFingerprint map[ConfigFingerprint]ConfigRecord
	// ordered is the declaration-ordered fingerprint list, so Records() and the
	// published inventory metric are deterministic.
	ordered []ConfigFingerprint
}

// NewConfigInventory validates and freezes the declared configuration records. It
// FAILS (rather than dropping a row) on: an unparseable fingerprint, cohort or
// approval reference; a reserved cohort ID; an unknown provider class; an empty,
// duplicated or invalid surface set; a duplicate fingerprint; two records claiming
// the same (cohort, surface) pair; or more than maxInventoryRecords rows.
//
// Rejecting an entire malformed manifest is deliberate: a partially-loaded
// inventory would silently change which traffic is enrolled, which is exactly the
// class of silent failure this arc's proof rules forbid.
func newConfigInventory(records []ConfigRecord) (*ConfigInventory, error) {
	if len(records) > maxInventoryRecords {
		return nil, fmt.Errorf("nativeserve/admission: configuration inventory has %d records, cap is %d", len(records), maxInventoryRecords)
	}
	inv := &ConfigInventory{byFingerprint: make(map[ConfigFingerprint]ConfigRecord, len(records))}
	type cohortSurface struct {
		cohort  CohortID
		surface Surface
	}
	claimed := make(map[cohortSurface]ConfigFingerprint, len(records))
	for i, r := range records {
		if _, err := parseConfigFingerprint(string(r.Fingerprint)); err != nil {
			return nil, fmt.Errorf("nativeserve/admission: inventory record %d: %w", i, err)
		}
		if _, err := parseCohortID(string(r.Cohort)); err != nil {
			return nil, fmt.Errorf("nativeserve/admission: inventory record %d: %w", i, err)
		}
		if !r.Provider.Valid() {
			return nil, fmt.Errorf("nativeserve/admission: inventory record %d: provider class is not one of the declared classes", i)
		}
		if _, err := parseApprovalRef(string(r.Approval)); err != nil {
			return nil, fmt.Errorf("nativeserve/admission: inventory record %d: %w", i, err)
		}
		if len(r.Surfaces) == 0 {
			return nil, fmt.Errorf("nativeserve/admission: inventory record %d: declares no surface", i)
		}
		seen := make(map[Surface]struct{}, len(r.Surfaces))
		for _, s := range r.Surfaces {
			if !s.Valid() {
				return nil, fmt.Errorf("nativeserve/admission: inventory record %d: declares a surface outside the closed set", i)
			}
			if _, dup := seen[s]; dup {
				return nil, fmt.Errorf("nativeserve/admission: inventory record %d: declares surface %s twice", i, s.Label())
			}
			seen[s] = struct{}{}
			key := cohortSurface{cohort: r.Cohort, surface: s}
			if other, dup := claimed[key]; dup {
				return nil, fmt.Errorf("nativeserve/admission: inventory records %q and %q both claim cohort %q on surface %s",
					other, r.Fingerprint, r.Cohort, s.Label())
			}
			claimed[key] = r.Fingerprint
		}
		if _, dup := inv.byFingerprint[r.Fingerprint]; dup {
			return nil, fmt.Errorf("nativeserve/admission: inventory declares fingerprint %q twice", r.Fingerprint)
		}
		inv.byFingerprint[r.Fingerprint] = cloneRecord(r)
		inv.ordered = append(inv.ordered, r.Fingerprint)
	}
	return inv, nil
}

// cloneRecord copies a record and its surface slice so a caller retaining the input
// slice cannot mutate the frozen inventory afterwards.
func cloneRecord(r ConfigRecord) ConfigRecord {
	out := r
	out.Surfaces = append([]Surface(nil), r.Surfaces...)
	return out
}

// Lookup returns the operator-visible record for an opaque fingerprint. A nil
// inventory (the fail-closed default) misses every lookup.
func (inv *ConfigInventory) Lookup(fp ConfigFingerprint) (ConfigRecord, bool) {
	if inv == nil {
		return ConfigRecord{}, false
	}
	r, ok := inv.byFingerprint[fp]
	if !ok {
		return ConfigRecord{}, false
	}
	return cloneRecord(r), true
}

// Records returns the declared records in declaration order. The copies are deep
// (surface slices included), so an operator-facing caller cannot mutate the
// inventory through the returned rows.
func (inv *ConfigInventory) Records() []ConfigRecord {
	if inv == nil {
		return nil
	}
	out := make([]ConfigRecord, 0, len(inv.ordered))
	for _, fp := range inv.ordered {
		out = append(out, cloneRecord(inv.byFingerprint[fp]))
	}
	return out
}

// Len returns the number of declared records (0 for a nil inventory).
func (inv *ConfigInventory) Len() int {
	if inv == nil {
		return 0
	}
	return len(inv.ordered)
}

// CohortEnrollment is ONE (surface, cohort) permission in the cohort policy: this
// cohort may claim a native attempt on this surface. Enrollment is per PAIR, which
// is what makes "right cohort, wrong surface" a decline rather than an admission.
type CohortEnrollment struct {
	Surface Surface
	Cohort  CohortID
}

// maxPolicyVersionLen bounds the policy version string.
const maxPolicyVersionLen = 40

// CohortPolicy is the VERSIONED, DEFAULT-DENY set of (surface, cohort)
// enrollments. It is immutable once built; enrollment changes are code changes to
// the production manifest, reviewed as such.
//
// A nil *CohortPolicy enrolls nothing — the fail-closed default.
type CohortPolicy struct {
	version string
	entries map[CohortEnrollment]struct{}
}

// NewCohortPolicy validates and freezes a versioned enrollment set. It FAILS on an
// unparseable version, an invalid surface, a reserved or unparseable cohort ID, or
// a duplicate enrollment. There is no "skip the bad entry" path: a policy either
// means exactly what it says or does not load.
func newCohortPolicy(version string, entries ...CohortEnrollment) (*CohortPolicy, error) {
	if version == "" {
		return nil, fmt.Errorf("nativeserve/admission: cohort policy version is empty")
	}
	if len(version) > maxPolicyVersionLen {
		return nil, fmt.Errorf("nativeserve/admission: cohort policy version is longer than %d bytes", maxPolicyVersionLen)
	}
	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return nil, fmt.Errorf("nativeserve/admission: cohort policy version has an unaccepted byte at offset %d", i)
		}
	}
	p := &CohortPolicy{version: version, entries: make(map[CohortEnrollment]struct{}, len(entries))}
	for i, e := range entries {
		if !e.Surface.Valid() {
			return nil, fmt.Errorf("nativeserve/admission: enrollment %d: surface is outside the closed set", i)
		}
		if _, err := parseCohortID(string(e.Cohort)); err != nil {
			return nil, fmt.Errorf("nativeserve/admission: enrollment %d: %w", i, err)
		}
		if _, dup := p.entries[e]; dup {
			return nil, fmt.Errorf("nativeserve/admission: enrollment %d: cohort %q is enrolled twice on surface %s", i, e.Cohort, e.Surface.Label())
		}
		p.entries[e] = struct{}{}
	}
	return p, nil
}

// Version returns the policy version ("" for a nil policy).
func (p *CohortPolicy) Version() string {
	if p == nil {
		return ""
	}
	return p.version
}

// Len returns the number of enrollments (0 for a nil policy).
func (p *CohortPolicy) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Enrolled reports whether cohort may claim a native attempt on surface. It is the
// whole policy decision, and it is DEFAULT-DENY at every layer: a nil policy, an
// empty policy, an invalid surface, a reserved cohort (none/unrecognized), or an
// absent (surface, cohort) pair all return false. The reserved-cohort check is
// re-asserted here rather than relying on the constructor, so the property survives
// a future constructor change.
func (p *CohortPolicy) Enrolled(surface Surface, cohort CohortID) bool {
	if p == nil || len(p.entries) == 0 || !surface.Valid() {
		return false
	}
	for _, r := range reservedCohortIDs() {
		if cohort == r {
			return false
		}
	}
	_, ok := p.entries[CohortEnrollment{Surface: surface, Cohort: cohort}]
	return ok
}

// Enrollments returns the enrollments sorted by (surface, cohort) for a
// deterministic operator-facing view.
func (p *CohortPolicy) Enrollments() []CohortEnrollment {
	if p == nil {
		return nil
	}
	out := make([]CohortEnrollment, 0, len(p.entries))
	for e := range p.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		return out[i].Cohort < out[j].Cohort
	})
	return out
}

// CohortGate binds a versioned policy to the configuration inventory it is written
// against. It is the single object every admission lane consults.
//
// A nil *CohortGate is a valid DEFAULT-DENY gate: it resolves every request to
// CohortNone and enrolls nothing. That is the semantic a forgotten wiring gets.
type CohortGate struct {
	policy    *CohortPolicy
	inventory *ConfigInventory
}

// NewCohortGate binds policy to inventory. It FAILS if the policy enrolls a
// (surface, cohort) pair the inventory cannot substantiate — an enrollment whose
// cohort has no declared configuration record, or whose record does not declare
// that surface. That cross-check is what makes the policy "admission EVIDENCE":
// you cannot enroll a cohort nobody has inventoried, and you cannot enroll it on a
// surface it was not approved for.
func newCohortGate(policy *CohortPolicy, inventory *ConfigInventory) (*CohortGate, error) {
	for _, e := range policy.Enrollments() {
		substantiated := false
		for _, r := range inventory.Records() {
			if r.Cohort != e.Cohort {
				continue
			}
			for _, s := range r.Surfaces {
				if s == e.Surface {
					substantiated = true
					break
				}
			}
			if substantiated {
				break
			}
		}
		if !substantiated {
			return nil, fmt.Errorf("nativeserve/admission: cohort policy %q enrolls cohort %q on surface %s with no inventory record declaring it",
				policy.Version(), e.Cohort, e.Surface.Label())
		}
	}
	return &CohortGate{policy: policy, inventory: inventory}, nil
}

// Policy returns the bound policy (nil for a nil gate).
func (g *CohortGate) Policy() *CohortPolicy {
	if g == nil {
		return nil
	}
	return g.policy
}

// Inventory returns the bound inventory (nil for a nil gate).
func (g *CohortGate) Inventory() *ConfigInventory {
	if g == nil {
		return nil
	}
	return g.inventory
}

// Resolve maps an opaque configuration fingerprint onto its bounded cohort bucket.
// It NEVER returns an unbounded value: an absent fingerprint is CohortNone, and any
// fingerprint the inventory does not declare — including a syntactically invalid
// one — is CohortUnrecognized. That is what keeps the cohort label bounded by the
// declared inventory no matter what reaches the seam.
func (g *CohortGate) Resolve(fp ConfigFingerprint) CohortID {
	if fp == "" {
		return CohortNone
	}
	r, ok := g.Inventory().Lookup(fp)
	if !ok {
		return CohortUnrecognized
	}
	return r.Cohort
}

// CohortInput is the request-side half of the gate: the configuration identity the
// control plane assigned, plus the gate to evaluate it against. It is embedded in
// every admission input struct.
type CohortInput struct {
	// Fingerprint is the opaque configuration ID assigned at CONFIG LOAD. S1's
	// production wiring assigns NONE — no config-load path populates it yet — so
	// every production request resolves to CohortNone and declines. It is a
	// PREDECLARED bucket, never a per-request hash: nothing in admission computes
	// it from the request.
	Fingerprint ConfigFingerprint
	// gate is UNEXPORTED on purpose, and it is the whole answer to "can a released
	// consumer select its own admission policy?". It cannot: no exported field,
	// constructor or function outside this package can populate it, so every
	// CohortInput an external caller can build resolves against
	// [ProductionCohortGate] — the shipped EMPTY, default-deny gate. The gated proof
	// suite sets it from INSIDE this package, behind the `nanollm_integration` build
	// tag, so it cannot be linked into a released consumer's binary at all.
	//
	// TestNoUntaggedExportedGateInjection is the standing guard on that property.
	gate *CohortGate
}

// gate returns the gate this input is evaluated against: the explicit one, or the
// production default-deny gate.
func (c CohortInput) resolvedGate() *CohortGate {
	if c.gate != nil {
		return c.gate
	}
	return ProductionCohortGate()
}

// ProductionCohortPolicyVersion is the version of the shipped production policy. It
// is the operator-visible name of "S1: nothing is enrolled".
const ProductionCohortPolicyVersion = "s1-default-deny-empty"

// productionInventoryRecords is the CONFIG-LOAD MANIFEST: the declared,
// operator-visible configuration classes this deployment knows about. It is the one
// place a candidate class is declared, and it is what a control-plane dashboard
// renders (see Metrics.publishCohortGate).
//
// Declaring is NOT enrolling. A record here makes a class visible and joinable
// offline by its opaque ID; whether it may CLAIM a native request is a separate
// question answered by productionEnrollments below. That separation is the whole
// point of having two manifests, and TestDeclaredButUnenrolledRecordIsVisibleAndStillDeclines
// exercises it through this very builder.
//
// S1 declares NONE. Not because the mechanism is unfinished, but because the class
// to declare is operator input: the cutover scope is explicit that "the exact
// production client/configuration identity must not be guessed from repository
// code", and inventing an approval reference for a class nobody approved would be
// false evidence on an operator-facing dashboard. The enrolling slice supplies the
// real record — a fingerprint from the declared vocabulary plus its offline approval
// reference — by adding it here.
func productionInventoryRecords() []ConfigRecord { return nil }

// productionEnrollments is the (surface, cohort) permission manifest. S1 enrolls
// NOTHING; that is the slice's entire serving guarantee, expressed as data rather
// than as a code path. The enrolling slice adds exactly one entry here, and no gate
// logic changes.
func productionEnrollments() []CohortEnrollment { return nil }

// productionGate is the process-wide production gate, resolved ONCE on first use
// from the compile-time enrollment manifest plus the deployment's config-loaded
// inventory (see config_load.go). Once, not at init, because the config-load half
// reads the environment and a package-init read would fix the answer before a test —
// or a deployment that sets the variable late — could influence it.
//
// A config-load failure does NOT silently degrade. loadProductionGate keeps the
// error, ProductionCohortGate falls back to the EMPTY gate (fail closed: still
// default-deny, still nothing enrolled), and NewMetrics surfaces the error to its
// caller — which is a factory, which workerboot turns into a boot failure. So a
// malformed declaration is loud at startup and safe in the meantime.
var loadProductionGate = sync.OnceValues(func() (*CohortGate, error) {
	inv, err := loadProductionInventory()
	if err != nil {
		return emptyFallbackGate(), err
	}
	pol, err := newCohortPolicy(ProductionCohortPolicyVersion, productionEnrollments()...)
	if err != nil {
		return emptyFallbackGate(), fmt.Errorf("production cohort policy: %w", err)
	}
	g, err := newCohortGate(pol, inv)
	if err != nil {
		return emptyFallbackGate(), fmt.Errorf("production cohort gate: %w", err)
	}
	return g, nil
})

// emptyFallbackGate is the gate a failed config load falls back to: no records, no
// enrollments, the shipped policy version.
//
// Its two constructor errors are deliberately not propagated, and that is not a
// dropped error: the inputs are a nil record slice and a compile-time version
// constant, neither of which any validator can reject, so the only way to reach the
// discard is for this file's own constants to be malformed. Even then the fallback
// stays correct — a nil policy enrolls nothing and a nil inventory resolves nothing,
// so the gate is default-deny either way, which is exactly what a fallback on a
// config-load failure must be. The failure the operator needs to see is the ORIGINAL
// load error, and that one is preserved and surfaced through NewMetrics.
func emptyFallbackGate() *CohortGate {
	inv, invErr := newConfigInventory(nil)
	pol, polErr := newCohortPolicy(ProductionCohortPolicyVersion)
	if invErr != nil || polErr != nil {
		// Unreachable for compile-time-literal inputs; the zero-value gate is still
		// default-deny, so fail closed rather than panicking inside a fallback.
		return &CohortGate{}
	}
	return &CohortGate{policy: pol, inventory: inv}
}

// ProductionCohortGateError returns the config-load error, if the deployment's
// declared inventory failed to decode. NewMetrics propagates it so a malformed
// declaration fails worker boot instead of quietly publishing nothing.
func ProductionCohortGateError() error {
	_, err := loadProductionGate()
	return err
}

// buildCohortGate is the config-load path itself: validate the declared records,
// validate the enrollments, and bind them into a gate that refuses any enrollment
// the records do not substantiate. Production calls it with its two manifests; the
// declared-but-unenrolled proof calls it with a record and no enrollment, so that
// proof runs through the SAME code path production does rather than assembling a
// gate by hand.
func buildCohortGate(version string, records []ConfigRecord, enrollments []CohortEnrollment) (*CohortGate, error) {
	inv, err := newConfigInventory(records)
	if err != nil {
		return nil, fmt.Errorf("configuration inventory: %w", err)
	}
	pol, err := newCohortPolicy(version, enrollments...)
	if err != nil {
		return nil, fmt.Errorf("cohort policy: %w", err)
	}
	return newCohortGate(pol, inv)
}

// ProductionCohortGate returns the shipped default-deny gate: the deployment's
// DECLARED configuration inventory (empty unless it set ConfigInventoryEnv) bound to
// an EMPTY versioned enrollment policy. Declaring is not enrolling, so every request
// evaluated against it — whatever it declares — resolves to a bounded cohort and
// declines with (StageCohort, ReasonCohortNotEnrolled) before any native work, and a
// native-capable artifact running S1 serves 100% BAML.
//
// If the config load failed, this is the EMPTY gate and ProductionCohortGateError
// reports why; the failure is surfaced at boot through NewMetrics rather than being
// absorbed here.
func ProductionCohortGate() *CohortGate {
	g, _ := loadProductionGate()
	return g
}

// admitCohort is THE gate: it resolves the request's bounded cohort and evaluates
// the versioned default-deny policy for (surface, cohort). It returns the resolved
// cohort — ALWAYS a bounded label, decline or not, so telemetry can attribute the
// decline — and a *Decline when the pair is not enrolled.
//
// It runs BEFORE any native work on every lane: no nanollm New, no render, no
// Prepare, no claim, and therefore provably no socket. It ADDS a requirement and
// can only narrow: every predicate that already declined still declines.
//
// The Detail is structural and secret-free. It distinguishes the three refusal
// shapes the scope names (absent identity, unrecognized identity, known identity
// not enrolled for this surface) WITHOUT giving them separate reason labels — the
// scope pins one bounded reason, cohort_not_enrolled, so the metric stays a single
// actionable bucket while the (non-label) detail stays diagnosable.
func admitCohort(surface Surface, in CohortInput) (CohortID, *Decline) {
	g := in.resolvedGate()
	cohort := g.Resolve(in.Fingerprint)
	if g.Policy().Enrolled(surface, cohort) {
		return cohort, nil
	}
	switch cohort {
	case CohortNone:
		return cohort, declinef(StageCohort, ReasonCohortNotEnrolled,
			"request presented no configuration identity for surface %s (policy %q enrolls %d)",
			surface.Label(), g.Policy().Version(), g.Policy().Len())
	case CohortUnrecognized:
		return cohort, declinef(StageCohort, ReasonCohortNotEnrolled,
			"configuration identity is not in the inventory for surface %s (policy %q enrolls %d)",
			surface.Label(), g.Policy().Version(), g.Policy().Len())
	default:
		return cohort, declinef(StageCohort, ReasonCohortNotEnrolled,
			"cohort is not enrolled on surface %s (policy %q enrolls %d)",
			surface.Label(), g.Policy().Version(), g.Policy().Len())
	}
}

// dynamicSurface derives the closed surface for the dynamic admission core from
// the LANE it is running in — the same boolean that selects the streaming body
// builder, plan-meta validator and Prepare flags. It is derived, never read from
// the Input, so a caller cannot present a surface it is not on (which is what makes
// operational invariant 4 — "a non-enrolled surface reporting a native claim is a
// rollout-stop" — a statement about reality rather than about a request field).
func dynamicSurface(stream bool) Surface {
	if stream {
		return SurfaceDynamicStream
	}
	return SurfaceDynamicCall
}

// ResolveCohort maps a request's configuration identity onto its bounded cohort
// bucket, for telemetry attribution on the paths that decline OUTSIDE admission
// (the serve boundary's own pre-claim declines: no output schema, cancelled
// context, plan mismatch, expired plan). It is pure — no FFI, no socket, no
// recording — and it returns exactly what the gate itself resolved, so a decline
// recorded by the serve boundary carries the same cohort label admission would
// have used.
func ResolveCohort(c CohortInput) CohortID { return c.resolvedGate().Resolve(c.Fingerprint) }
