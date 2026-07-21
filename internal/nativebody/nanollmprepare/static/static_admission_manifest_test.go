//go:build integration && nanollm_integration

package static

// De-BAML Slice 8B — STATIC unary no-send ADMISSION manifest differential.
//
// This is the OBSERVE-ONLY admission twin of prepared_request_integration_test.go.
// It drives nativeserve/admission.AdmitStatic (the full pre-socket static predicate:
// descriptor envelope -> arg binder -> Return-Bundle lower/support -> RenderStatic ->
// canonical body -> nanollm New/Prepare -> strict BAML Request.<Method> no-send plan
// compare) over the SAME Phase-4a-admitted static corpus (5 functions / 14 argument
// rows) plus one negative mutation per decline family, and pins the exact admission
// manifest:
//
//	total / would-admit / descriptor-envelope-decline / prompt-decline /
//	client-decline / plan-match  (with plan-mismatch = total - the rest)
//
// It proves ATTACHMENT with ZERO behavior change: AdmitStatic ALWAYS declines (the
// caller forces a pre-socket decline), opens NO socket, and RoundTrips NOTHING — the
// nanollm client only Prepares (no send) and the BAML plan closure only builds
// Request.<Method> (no send). Zero RoundTrips is structural AND empirical: the corpus
// client's base_url is the unroutable fence host (…​.invalid), so a would-admit — which
// requires a SUCCESSFUL Prepare AND a successful BAML Request.<Method> build — could
// never happen if EITHER opened a real socket (it would DNS-fail against .invalid).
//
// A would-admit row is EXACTLY a row whose native prepared plan matched BAML's
// Request.<Method> no-send plan under the scope-ratified Slice 5.1 static comparison:
// method/URL/body BYTE-EXACT plus the SEMANTIC header set (name+value) exact. Header
// ORDER and CASING are NOT compared and BAML's internal baml-original-url transport
// header is exempted — because BAML's Go HTTPRequest.Headers() surface is an UNORDERED
// map[string]string (no raw order exists to compare) and casing is HTTP-insignificant;
// see staticPlanMatches. AdmitStatic returns would-admit iff testutil.Diff is empty, so
// would-admit == 14 is the raw plan-equality proof at the fidelity the BAML Go surface
// admits. Anti-omission: every decline family and the plan-mismatch outcome occur >= 1,
// and each negative mutation FLIPS the outcome of its otherwise-admitted base row.

import (
	"context"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/nativeserve/admission"
)

// bamlPlanClosure adapts a corpus row's generated Request.<Method> build (a no-send
// BAML HTTPRequest) into the neutral *llmhttp.Request plan closure AdmitStatic
// compares against. It opens no socket — Request.<Method> only builds.
func bamlPlanClosure(build func() (baml.HTTPRequest, error)) func(context.Context) (*llmhttp.Request, error) {
	return func(context.Context) (*llmhttp.Request, error) {
		req, err := build()
		if err != nil {
			return nil, err
		}
		method, err := req.Method()
		if err != nil {
			return nil, err
		}
		url, err := req.Url()
		if err != nil {
			return nil, err
		}
		hdrs, err := req.Headers()
		if err != nil {
			return nil, err
		}
		body, err := req.Body()
		if err != nil {
			return nil, err
		}
		text, err := body.Text()
		if err != nil {
			return nil, err
		}
		return &llmhttp.Request{Method: method, URL: url, Headers: hdrs, Body: text}, nil
	}
}

// argOrderOf returns the descriptor's declared argument names in signature order —
// the order AdmitStatic checks the generated binder against.
func argOrderOf(fn promptdescriptor.Function) []string {
	out := make([]string, len(fn.Args))
	for i, a := range fn.Args {
		out[i] = a.Name
	}
	return out
}

// staticInputFor builds the neutral StaticInput for one admitted corpus row: the
// hardcoded layer-1 facts a native worker supplies, the fresh descriptor, the exact
// generated args + declared order, and the BAML Request.<Method> no-send plan closure.
func staticInputFor(fn promptdescriptor.Function, c staticCase) admission.StaticInput {
	return admission.StaticInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           admission.RouteKindStatic,
		Method:              fn.Method,
		Descriptor:          fn,
		Args:                c.args,
		ArgOrder:            argOrderOf(fn),
		Mode:                bamlutils.NativeStaticModeFinal,
		SingleLeaf:          true,
		Provider:            fn.Provider,
		BuildBAMLRequest:    bamlPlanClosure(c.build),
	}
}

func TestStaticAdmissionManifest(t *testing.T) {
	descriptors := buildDescriptors(t)
	cases := staticCases()

	var (
		total         int
		wouldAdmit    int
		planMatch     int
		envDecline    int
		promptDecline int
		clientDecline int
		planMismatch  int
	)
	ctx := context.Background()

	// --- Clean corpus: every admitted row would-admit AND plan-match. -------
	for _, c := range cases {
		fn, ok := descriptors[c.fn]
		if !ok {
			t.Fatalf("fixture has no descriptor for %q", c.fn)
		}
		obs := admission.AdmitStatic(ctx, staticInputFor(fn, c))
		total++
		if obs.Observation != bamlutils.NativeStaticObserveWouldAdmit {
			t.Errorf("clean row %s: observation=%q family=%q stage=%q reason=%q, want would_admit",
				c.name, obs.Observation, obs.Family, obs.Stage, obs.Reason)
			continue
		}
		wouldAdmit++
		planMatch++
	}

	// A shared admitted base row + its descriptor for the negative mutations. Each
	// mutation FLIPS this otherwise would-admit row to a specific decline / mismatch.
	base := cases[0] // StaticCompletion/ascii — a clean would-admit row above.
	baseFn := descriptors[base.fn]

	// --- Mutation 1: descriptor-envelope decline (bad Return version). -------
	{
		fn := baseFn // value copy; Return.Version is a scalar
		fn.Return.Version = 999
		obs := admission.AdmitStatic(ctx, staticInputFor(fn, base))
		total++
		if obs.Observation == bamlutils.NativeStaticObserveDecline &&
			obs.Family == bamlutils.NativeStaticFamilyDescriptorEnvelope {
			envDecline++
		} else {
			t.Errorf("envelope mutation: observation=%q family=%q reason=%q, want descriptor_envelope decline",
				obs.Observation, obs.Family, obs.Reason)
		}
	}

	// --- Mutation 2: prompt decline (a non-primitive argument value). --------
	{
		c := base
		c.args = map[string]any{"topic": []int{1, 2, 3}}
		obs := admission.AdmitStatic(ctx, staticInputFor(baseFn, c))
		total++
		if obs.Observation == bamlutils.NativeStaticObserveDecline &&
			obs.Family == bamlutils.NativeStaticFamilyPrompt {
			promptDecline++
		} else {
			t.Errorf("prompt mutation: observation=%q family=%q reason=%q, want prompt decline",
				obs.Observation, obs.Family, obs.Reason)
		}
	}

	// --- Mutation 3: client decline (a non-openai selected provider). --------
	{
		fn := baseFn
		fn.Provider = "anthropic"
		obs := admission.AdmitStatic(ctx, staticInputFor(fn, base))
		total++
		if obs.Observation == bamlutils.NativeStaticObserveDecline &&
			obs.Family == bamlutils.NativeStaticFamilyClient {
			clientDecline++
		} else {
			t.Errorf("client mutation: observation=%q family=%q reason=%q, want client decline",
				obs.Observation, obs.Family, obs.Reason)
		}
	}

	// --- Mutation 4: plan mismatch (mutate BAML's plan body post-build). -----
	// Every gate + Prepare passes; only the strict plan compare fails.
	{
		si := staticInputFor(baseFn, base)
		real := si.BuildBAMLRequest
		si.BuildBAMLRequest = func(ctx context.Context) (*llmhttp.Request, error) {
			r, err := real(ctx)
			if err != nil {
				return nil, err
			}
			mutated := *r
			mutated.Body = r.Body + " " // one-byte drift -> mismatch
			return &mutated, nil
		}
		obs := admission.AdmitStatic(ctx, si)
		total++
		if obs.Observation == bamlutils.NativeStaticObservePlanMismatch {
			planMismatch++
		} else {
			t.Errorf("plan-mismatch mutation: observation=%q family=%q reason=%q, want plan_mismatch",
				obs.Observation, obs.Family, obs.Reason)
		}
	}

	// --- Exact manifest pin -------------------------------------------------
	type pin struct {
		name string
		got  int
		want int
	}
	for _, p := range []pin{
		{"total", total, 18},
		{"would-admit", wouldAdmit, 14},
		{"plan-match", planMatch, 14},
		{"descriptor-envelope-decline", envDecline, 1},
		{"prompt-decline", promptDecline, 1},
		{"client-decline", clientDecline, 1},
		{"plan-mismatch", planMismatch, 1},
	} {
		if p.got != p.want {
			t.Errorf("static admission manifest: %s = %d, want %d", p.name, p.got, p.want)
		}
	}

	// Anti-omission: every decline family + the would-admit + plan-mismatch outcome
	// must each occur at least once, so the manifest can never silently drop a family.
	if wouldAdmit < 1 || envDecline < 1 || promptDecline < 1 || clientDecline < 1 || planMismatch < 1 {
		t.Errorf("anti-omission: each outcome must occur >= 1 (would_admit=%d env=%d prompt=%d client=%d mismatch=%d)",
			wouldAdmit, envDecline, promptDecline, clientDecline, planMismatch)
	}
}
