// Package shadow is the de-BAML cutover Slice 4 one-send SHADOW comparator: for
// an admitted unary `_dynamic` call it builds the native request plan (via the
// S3 admission predicate), obtains BAML's built request plan for the SAME
// selected child WITHOUT sending, compares the two field by field, records a
// bounded plan_compare result (NO values), and then DECLINES so the orchestrator
// serves BAML unchanged. Native NEVER opens a socket / RoundTrips here.
//
// It lives in the out-of-go.work nanollmprepare module and links nanollm (through
// the admission package), so nothing here can enter the host/root link graph —
// the host stays zero-nanollm and CGO-free. A SHADOW deploy profile's worker
// injects it as a neutral bamlutils.NativeShadowFunc; every default build leaves
// it nil, so the orchestrator's native child-attempt callback stays hard-off.
//
// The pure plan/response comparison primitives live in the shared parity package
// so the Slice-6 native SERVE implementation reuses the SAME policy — these thin
// wrappers keep the shadow package's internal names/behaviour unchanged.
package shadow

import (
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/parity"
)

// planComparison is the shadow-local view of parity.PlanComparison. It keeps the
// package-private allMatch() the shadow flow and tests use; the fields mirror
// parity.PlanComparison exactly so comparePlans can convert directly.
type planComparison struct {
	Method       bool
	Target       bool
	Host         bool
	Headers      bool
	Body         bool
	MetaMismatch bool
	Diffs        []string
}

// allMatch reports whether every compared field matched.
func (c planComparison) allMatch() bool {
	return c.Method && c.Target && c.Host && c.Headers && c.Body
}

// comparePlans delegates to the shared parity.ComparePlans so the shadow oracle
// and the native serve path apply the IDENTICAL request-plan comparison policy.
func comparePlans(bamlReq *llmhttp.Request, native *llmhttp.ExactAttemptRequest) planComparison {
	return planComparison(parity.ComparePlans(bamlReq, native))
}
