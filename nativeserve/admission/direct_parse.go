package admission

import "context"

// De-BAML serving cutover S1 — DIRECT-PARSE admission.
//
// `/parse/{method}` is the fifth declared serving surface and the only one with no
// native implementation behind it: worker/parse.go invokes BAML's `method.Impl` /
// `method.StreamImpl` directly, and this module offers no native parse to claim.
//
// That does NOT make it exempt from the cutover's accounting. The scope's telemetry
// contract enumerates five surfaces precisely so no endpoint class sits outside the
// operational invariants, and "BAML owns 100% of this surface" is a claim that
// deserves per-request evidence rather than a comment. So the surface gets a real
// admission entry point, evaluated exactly like the other four:
//
//   - it runs the SAME default-deny cohort gate, on SurfaceDirectParse;
//   - it then declines unconditionally, because no native parse exists to admit.
//
// Both refusals are recorded, and the ORDER matters for what the numbers mean. While
// nothing is enrolled ON THIS SURFACE — which the shipped policy still is, since S3b
// enrolls the dynamic unary call surface only — the decline reads
// `cohort_not_enrolled`, exactly like the other unenrolled surfaces.
// If a cohort were ever enrolled for this surface, the second gate would take over
// and report `direct_parse_unproven` — a distinct, honest reason meaning "the policy
// would permit this class, but there is nothing here to permit it into". That is the
// fail-closed shape the scope wants: enrollment alone can never conjure a native
// parse path, and the metric says so.
//
// Introducing an actual native direct-parse claim is the scope's S9 ("introduce a
// separate optional native direct-parse seam at worker/parse.go rather than silently
// replacing method.Impl"). This file deliberately contains no such seam; it observes
// and refuses.

// DirectParseInput is the whole set of facts the direct-parse admission needs. Like
// every other admission input it is neutral — no nanollm type, no method name, no raw
// input, no parsed output.
type DirectParseInput struct {
	// Stream reports whether this is a parse-STREAM request rather than a final parse.
	// It is observed, not decided on: both shapes decline.
	Stream bool

	// Cohort is the serving-cutover configuration identity + the default-deny gate it
	// is evaluated against, exactly as on the other four surfaces. Production leaves
	// it zero.
	Cohort CohortInput
}

// AdmitDirectParse evaluates the direct-parse surface and ALWAYS declines, returning
// the stable, secret-free reason. It performs no native work of any kind — no
// nanollm, no render, no Prepare, no socket — because there is no native parse
// implementation to perform it with.
//
// It is the admission half of the direct_parse telemetry bridge; the recording half
// lives in the observer nativeserve.NewDirectParseObserve builds.
func AdmitDirectParse(ctx context.Context, in DirectParseInput) (CohortID, *Decline) {
	if err := ctx.Err(); err != nil {
		return CohortNone, declinef(StageContext, ReasonContextCancelled,
			"request context cancelled before direct-parse admission")
	}
	cohort, d := admitCohort(SurfaceDirectParse, in.Cohort)
	if d != nil {
		return cohort, d
	}
	// Enrolled — and still refused. There is no native parse path to claim, so an
	// enrolled cohort changes nothing here except which reason the dashboard shows.
	return cohort, declinef(StageMode, ReasonDirectParseUnproven,
		"direct /parse has no native implementation; BAML owns this surface")
}
