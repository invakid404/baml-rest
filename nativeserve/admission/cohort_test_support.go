//go:build nanollm_integration

package admission

import "fmt"

// De-BAML serving cutover S1 — the PROOF cohort identity.
//
// # Why this file carries a build tag
//
// The default-deny gate is only as strong as the set of things that can select a
// DIFFERENT gate. A cold review of the first draft found the answer was "anything":
// the proof gate and an exported `CohortInput.Gate` field shipped untagged in the
// public module, so an external consumer could enroll itself and reach native
// Prepare/claim without touching the production policy. That is a second admission
// path, and it defeats the sole-revert contract at the API boundary.
//
// So: the gate override field is UNEXPORTED (only this package can set it), and
// everything in this file — the proof inventory, policy, gate and identity — sits
// behind the `nanollm_integration` opt-in tag, exactly like the package's other
// synthetic-claim helpers. A released consumer building the module normally cannot
// link any of it, and TestNoUntaggedExportedGateInjection proves the untagged
// surface offers no way in.
//
// # Why a proof identity is needed at all
//
// The gated suites still have to exercise the layers BEHIND the gate — the whole
// map/render/body/Prepare/plan predicate, the claim paths, and the post-claim
// serving pipeline. They present this identity rather than weakening the gate. The
// suites live in three modules (nativeserve/admission, nativeserve/execute, and the
// out-of-go.work nanollmprepare suites), which is why it is exported rather than a
// package-local test helper.

const (
	// ProofCohort is the cohort ID the proof suites present. It is a normal
	// (non-reserved) cohort ID, so it exercises the SAME code path a real
	// enrollment will.
	ProofCohort CohortID = "proof"
	// ProofPolicyVersion names the proof policy, so an accidental production scrape
	// of a test binary is unmistakable.
	ProofPolicyVersion = "proof-suite-all-surfaces"
	// proofApproval is the offline approval reference for the proof record.
	proofApproval ApprovalRef = "DEBAML-602"
)

var proofGate = mustProofGate()

func mustProofGate() *CohortGate {
	inv, err := newConfigInventory([]ConfigRecord{{
		Fingerprint: proofConfigFingerprint,
		Cohort:      ProofCohort,
		Surfaces:    AllSurfaces(),
		Provider:    ConfigProviderOpenAI,
		Approval:    proofApproval,
	}})
	if err != nil {
		panic(fmt.Sprintf("nativeserve/admission: proof inventory is malformed: %v", err))
	}
	entries := make([]CohortEnrollment, 0, len(AllSurfaces()))
	for _, s := range AllSurfaces() {
		entries = append(entries, CohortEnrollment{Surface: s, Cohort: ProofCohort})
	}
	pol, err := newCohortPolicy(ProofPolicyVersion, entries...)
	if err != nil {
		panic(fmt.Sprintf("nativeserve/admission: proof policy is malformed: %v", err))
	}
	g, err := newCohortGate(pol, inv)
	if err != nil {
		panic(fmt.Sprintf("nativeserve/admission: proof gate is malformed: %v", err))
	}
	return g
}

// ProofCohortGateForTest returns the proof gate: ProofCohort enrolled on all five
// surfaces, and nothing else. Production never references it, and an untagged build
// does not contain it.
func ProofCohortGateForTest() *CohortGate { return proofGate }

// ProofCohortInputForTest is the [CohortInput] a proof suite presents so its request
// reaches the layers behind the default-deny gate. It is the ONLY way to build a
// gate-bearing CohortInput, and it exists only under the opt-in tag.
func ProofCohortInputForTest() CohortInput {
	// Provider is part of the identity since serving cutover S3a: the gate binds a
	// fingerprint to its INVENTORY RECORD, and the proof record declares openai on
	// every surface. An identity that omitted the class would resolve
	// CohortUnrecognized and the gated suites would stop reaching the layers behind
	// the gate — which is the correct failure, and the reason this is stated here
	// rather than special-cased in the gate.
	return CohortInput{
		Fingerprint: proofConfigFingerprint,
		Provider:    ConfigProviderOpenAI,
		gate:        ProofCohortGateForTest(),
	}
}
