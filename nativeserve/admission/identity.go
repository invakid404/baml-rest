package admission

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
)

// De-BAML serving cutover S3a — the TRUSTED effective-configuration identity
// resolver.
//
// # The wiring gap this closes
//
// S1 built the default-deny cohort gate but left its request-side half unwired:
// the serve seam handed admission a PROCESS-LEVEL [CohortInput] that production
// always left zero. That is safe only while the policy enrolls nothing. The moment
// one cohort is enrolled, a process-level identity stamps that cohort onto EVERY
// request the worker hosts — every registry, every client, every model — which is
// an out-claim of BAML by construction rather than by accident. Closing the gap is
// a PREREQUISITE for enrollment, and it landed on its own, with the policy still
// empty, so the highest-risk piece was reviewable while it still served nothing.
// Serving cutover S3b then enrolled ONE tuple on top of it; every guarantee below is
// what makes that enrollment safe rather than a worker-wide stamp.
//
// # Why the request's own registry can never be the identity
//
// The dynamic `/call` surface takes its `client_registry` from the PUBLIC REQUEST
// BODY: the caller names the clients, their provider, and every option including
// `model`, `base_url` and `api_key`. Which leaf BAML then selects, what its
// effective target is, and what its options are, are all consequences of that
// caller-supplied document.
//
// So no amount of checking the registry against itself — not the resolved leaf
// provider, not the named selected leaf, not the orchestration shape, and not even
// a byte-exact match against an approved shape — can establish that a request is
// running the deployment's approved configuration. A caller who sends the approved
// client name, provider, model and base_url with THEIR OWN credential has matched
// the mask, not the configuration. Treating that as identity would let an enrolled
// cohort be claimed by traffic the deployment never approved, which is precisely
// the out-claim the cutover's hard invariants forbid.
//
// # The trusted fact
//
// Identity therefore comes from a fact the request cannot manufacture: the
// TRUSTED-CONFIGURATION SEAL (bamlutils/trustedconfig.go). A sealed client is one
// the DEPLOYMENT configured — the request did no more than NAME it, and the
// worker's config-load path installed the provider and every option from the
// deployment's own approved-configuration declaration
// (bamlutils/trustedclients). The seal is an UNEXPORTED field: no JSON decoder can
// reach it, decoding clears it, and marshalling never emits it, so there is no
// wire representation to forge.
//
// This resolver reads that seal and NOTHING ELSE for identity. The registry facts
// it does check are all NARROWING conditions on top of it, never a substitute for
// it.
//
// # Prove-or-decline
//
// [ResolveConfigIdentity] returns the zero identity — which the gate resolves to
// CohortNone and declines pre-claim — unless every one of these holds:
//
//  1. The orchestration resolved exactly one leaf, with no fallback chain, no
//     round robin and no request-retry override.
//  2. The seam carried BAML's no-send plan builder. Without it the strict plan
//     equality fe-v1 requires cannot run at all, so the legacy native-first probe
//     route (which passes none) can never carry an identity.
//  3. The effective registry resolves to exactly ONE unambiguous client — primary
//     by name, else the sole client — under [selectOneClient].
//  4. That client is the leaf BAML named. A request whose named selected leaf is
//     not the client the registry selector picks is ambiguous, and ambiguity is
//     declined, never guessed.
//  5. The selected client carries no client retry policy.
//  6. THE SELECTED CLIENT IS SEALED. This is the provenance boundary; everything
//     else on this list only narrows what a sealed client may still be.
//  7. The resolved leaf provider is present, agrees (under canonical spelling)
//     with the provider on the selected client, and is one of the bounded declared
//     provider CLASSES.
//  8. The canonical digest of the effective configuration, recomputed here, equals
//     the digest recorded when it was sealed — so a pass that mutated a sealed
//     client after sealing cannot inherit its identity.
//  9. The sealed fingerprint is in THIS build's declared opaque vocabulary. The
//     deployment chooses which bucket it assigned; it does not get to invent one.
//
// # Privacy contract
//
// Only the opaque `cfgNNN` fingerprint and the bounded provider CLASS leave this
// file, into [CohortInput], where the gate folds them onto a bounded cohort bucket
// before anything records anything. The digest is a comparison value, never an
// observation: it is not a metric label, not a log field and not a decline detail.
// No configuration name, URL, model, credential or request content is read into
// any of them.
//
// # Sealing is not enrolling
//
// A seal assigns an opaque bucket to a configuration the deployment owns. It does
// NOT permit a claim: that remains the compile-time enrollment manifest's answer,
// and that manifest names exactly ONE slot (serving cutover S3b). A deployment may
// seal every class it likes; every one sealed under any OTHER slot still declines —
// which TestSealedButUnenrolledIdentityStillDeclines drives, and which
// TestAnUnenrolledSlotNeverInheritsTheEnrolledCohort drives across the whole
// declared vocabulary.

// ConfigSelection is the per-request set of facts the identity resolver reads. All
// of them are produced by BAML's own resolution and threaded through the native
// serve seam; there is deliberately no fingerprint field, and nothing here is an
// identity a caller can assert.
type ConfigSelection struct {
	// Registry is the effective client registry BAML resolved the leaf from, AFTER
	// the worker merged its deployment-wide client defaults, applied its base-URL
	// rewrites, and applied the TRUSTED-CONFIGURATION SEAL — i.e. the configuration
	// that will actually be used, carrying the deployment's statement about which
	// of its clients the deployment itself configured.
	Registry *bamlutils.ClientRegistry
	// ResolvedProvider is the orchestrator-resolved leaf provider: the
	// authoritative routing view, the same one the strict mapper treats as
	// authoritative.
	ResolvedProvider string
	// SelectedLeaf is the concrete client name BAML selected for this attempt. It
	// must be the client the registry selector resolves, or the request is
	// ambiguous.
	SelectedLeaf string
	// SingleLeaf reports the orchestration plan resolved exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride are the
	// whole-orchestration-plan shapes that make "the effective selected leaf" not a
	// single answer. Any of them means no identity.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool
	// HasBAMLPlanOracle reports the seam carried BAML's no-send request-plan
	// builder for this attempt. The direct-legacy native-first probe route carries
	// none, and without one the strict plan equality fe-v1 requires cannot run — so
	// a request that could never be a valid claim is never given an identity.
	HasBAMLPlanOracle bool
}

// ConfigIdentity is what a request presents to the cohort gate: the opaque
// configuration bucket the DEPLOYMENT assigned to the configuration it sealed,
// plus the bounded provider CLASS that configuration resolved to.
//
// The zero value is "no identity", which is what every request on every deployment
// that sealed nothing resolves to.
type ConfigIdentity struct {
	Fingerprint ConfigFingerprint
	Provider    ConfigProviderClass
}

// ResolveConfigIdentity resolves the request's effective selected configuration to
// its declared opaque identity, or to the zero identity when it cannot be PROVEN
// to be a configuration the deployment sealed.
//
// It never returns a value derived from the request: the fingerprint always comes
// from a seal only the worker's config load can create, so what reaches the gate is
// bounded by the deployment's declaration rather than by the wire. Every
// uncertainty — an unsealed client, an ambiguous orchestration, an ambiguous
// registry, a leaf disagreement, a provider disagreement, a post-seal mutation, an
// undeclared fingerprint — returns no identity, and no identity declines at the
// cohort stage before any native work.
func ResolveConfigIdentity(sel ConfigSelection) ConfigIdentity {
	id, err := sel.resolve()
	if err != nil {
		return ConfigIdentity{}
	}
	return id
}

// resolve is the predicate itself. Its error is diagnostic only: no caller records
// it, because "no identity" is not a decline reason of its own — the cohort gate
// owns the bounded reason.
func (sel ConfigSelection) resolve() (ConfigIdentity, error) {
	var none ConfigIdentity
	if !sel.SingleLeaf {
		return none, fmt.Errorf("nativeserve/admission: the orchestration did not resolve exactly one leaf")
	}
	if sel.HasFallbackChain || sel.HasRoundRobin {
		return none, fmt.Errorf("nativeserve/admission: the orchestration carries a fallback chain or round robin")
	}
	if sel.HasRequestRetryOverride {
		return none, fmt.Errorf("nativeserve/admission: the request carries a retry override")
	}
	if !sel.HasBAMLPlanOracle {
		return none, fmt.Errorf("nativeserve/admission: the seam carried no BAML no-send plan builder")
	}
	cp, dec := selectOneClient(sel.Registry)
	if dec != nil {
		return none, dec
	}
	// The leaf BAML named and the leaf the registry selector resolves must be the
	// SAME leaf. An empty name proves nothing (there is no leaf to compare
	// against), and a different name means the identity would be read off a client
	// the request is not going to use.
	if sel.SelectedLeaf == "" || sel.SelectedLeaf != cp.Name {
		return none, fmt.Errorf("nativeserve/admission: the named selected leaf is absent or is not the client the registry resolves")
	}
	if cp.RetryPolicy != nil {
		return none, fmt.Errorf("nativeserve/admission: the selected client carries a retry policy")
	}

	// THE PROVENANCE BOUNDARY. Everything above narrows; this is the only thing
	// that establishes the configuration is the deployment's rather than the
	// caller's, and there is no path around it: an unsealed client has no identity
	// however closely it resembles an approved one.
	fingerprint, sealedDigest, sealed := cp.TrustedConfigSeal()
	if !sealed {
		return none, fmt.Errorf("nativeserve/admission: the selected client is not a sealed deployment configuration")
	}

	// The resolved leaf provider is authoritative — the same rule the strict mapper
	// applies — and an absent one is never guessed from the client. A sealed client
	// always carries the declared provider, so this is an agreement check between
	// the seal and BAML's routing view rather than a tolerance for absence.
	if sel.ResolvedProvider == "" {
		return none, fmt.Errorf("nativeserve/admission: the resolved leaf provider is absent")
	}
	canonical := normalizeNanollmProvider(sel.ResolvedProvider)
	if cp.Provider == "" || normalizeNanollmProvider(cp.Provider) != canonical {
		return none, fmt.Errorf("nativeserve/admission: the selected client provider disagrees with the resolved leaf provider")
	}
	// Identity is carried by the bounded provider CLASS, not by a free provider
	// string: a provider outside the declared classes has no class to be identified
	// as, so it gets no identity rather than an improvised one.
	class := ConfigProviderClass(canonical)
	if !class.Valid() {
		return none, fmt.Errorf("nativeserve/admission: the resolved leaf provider is not a declared provider class")
	}

	// Anti-drift: the seal describes a configuration, and this is the configuration
	// in front of us. If a later pass mutated the sealed client, the recomputed
	// digest moves and the seal stops applying, rather than the mutated
	// configuration inheriting the approved identity.
	digest, err := bamlutils.TrustedConfigDigest(cp, sel.ResolvedProvider)
	if err != nil {
		return none, fmt.Errorf("nativeserve/admission: the selected client cannot be canonicalized: %w", err)
	}
	if digest != sealedDigest {
		return none, fmt.Errorf("nativeserve/admission: the selected client no longer matches the configuration that was sealed")
	}

	// The deployment chooses WHICH declared bucket it assigned; it does not get to
	// invent one. An out-of-vocabulary fingerprint is refused here rather than
	// folded onto a label downstream.
	fp, err := parseConfigFingerprint(fingerprint)
	if err != nil {
		return none, err
	}
	return ConfigIdentity{Fingerprint: fp, Provider: class}, nil
}
