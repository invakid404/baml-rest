//go:build nanollm_integration

package shadow

// De-BAML serving cutover S1 — the gated cohort-identity constructor for the shadow
// comparator.
//
// The shipped shadow profile presents the production (zero) configuration identity
// and therefore declines at the default-deny gate: S1 gives the no-send paths no
// exception, because an enrolled "observe" cohort would be a second non-empty
// admission policy inside the slice. The gated proofs still have to exercise the
// plan-compare mechanics behind the gate, so they build their comparator here.
//
// Behind the `nanollm_integration` tag, like every other cohort-identity seam: a
// released consumer cannot link it, and admission's CohortInput cannot carry a gate
// outside that tag anyway.

import (
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// NewComparatorWithCohortIdentity is [NewComparator] with an explicit serving-cutover
// configuration identity.
func NewComparatorWithCohortIdentity(m *admission.Metrics, exec *llmhttp.ExactExecutor, identity admission.CohortInput) *Comparator {
	return NewComparator(m, exec).withCohortIdentity(identity)
}
