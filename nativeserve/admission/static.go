package admission

import (
	"context"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/testutil"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// De-BAML — the STATIC native admission route, alongside the existing dynamic one.
//
// TWO ENTRY POINTS, ONE PREDICATE. This file's history describes an observe-only
// slice (8B), which is no longer the whole picture: Slice 8C added the SERVING
// entry point next to the observing one, and both run the SAME predicate through
// admitStaticThroughPrepare + staticPlanCompareObservation:
//
//   - [AdmitStatic] is the OBSERVER. It runs the full pre-socket predicate
//     (envelope validation, arg-binder match, Return-Bundle lower/support,
//     RenderStatic, canonical body, nanollm New/Prepare, and the strict BAML
//     `Request.<Method>` no-send plan compare), records the tri-state observation,
//     and ALWAYS forces a decline. It never claims, never opens a socket, never
//     RoundTrips.
//   - [AdmitStaticClaim] is the SERVE path. On a full would-admit — every gate
//     passed AND the native plan byte-matched BAML's — it additionally applies the
//     serve-only proven-return-shape gate and CLAIMS the attempt, keeping the
//     request-scoped engine alive so the serve core can perform exactly ONE
//     RoundTrip. Every pre-claim decline still guarantees no socket occurred.
//
// So "this route never serves" is FALSE as a property of the file; what is true —
// and what the executable gates enforce — is that a static request is served
// natively only when it is admitted through AdmitStaticClaim by a serve-profile
// worker. The serving-cutover S1 slice narrowed that further: admission now also
// requires an ENROLLED surface/cohort pair (layer 1b below), and the shipped policy
// enrolls NO static surface — S3b enrolls the dynamic unary call surface only — so
// every static request declines pre-socket today and BAML serves it.
//
// SENSITIVE: every value the predicate touches (descriptor, args, rendered prompt,
// canonical body, prepared plan, BAML plan) carries secret material — never log,
// serialize, %v-format, error-wrap, or metric-label any of it; only the bounded
// stage/reason/family/observation tokens leave this function.

// RouteKind is the closed distinction between the dynamic and static native
// admission routes (Slice 8B). It replaces the previous implicit "the one internal
// method is Baml_Rest_Dynamic" gate with an explicit route kind so a static
// invocation can never be mis-admitted through the dynamic predicate and vice
// versa. The zero value is RouteKindDynamic, so every existing dynamic caller (which
// leaves the field unset) keeps its exact behaviour.
type RouteKind uint8

const (
	// RouteKindDynamic is the existing dynamic `Baml_Rest_Dynamic` route (zero
	// value): admitCore admits it, gated on Method == dynamicMethod.
	RouteKindDynamic RouteKind = iota
	// RouteKindStatic is the Slice 8B static route: AdmitStatic observes it and
	// always declines. admitCore rejects it defensively (the two predicates never
	// cross).
	RouteKindStatic
)

// String returns a bounded, secret-free token for the route kind.
func (k RouteKind) String() string {
	switch k {
	case RouteKindStatic:
		return "static"
	case RouteKindDynamic:
		return "dynamic"
	default:
		return "unknown"
	}
}

// StagePlanCompare is the bounded stage token for the strict BAML plan compare.
const StagePlanCompare Stage = "plan_compare"

// Static-route bounded reason tokens. They are secret-free enum-like constants that
// name WHY the static observer stepped aside (or what it observed); they never carry
// a value. Grouped by pipeline stage for legibility.
const (
	reasonRouteKindNotStatic Reason = "route_kind_not_static"
	reasonModeNotFinal       Reason = "mode_not_final"
	reasonModeNotParseOnly   Reason = "mode_not_parse_only"
	reasonModeUnsupported    Reason = "mode_unsupported"
	reasonCallWithRawUnprove Reason = "call_with_raw_unproven"

	reasonDescriptorAbsent          Reason = "descriptor_absent"
	reasonDescriptorMethodMismatch  Reason = "descriptor_method_mismatch"
	reasonDescriptorVersionMismatch Reason = "descriptor_version_mismatch"
	reasonReturnVersionMismatch     Reason = "return_version_mismatch"
	reasonReturnMethodMismatch      Reason = "return_method_mismatch"
	reasonReturnIsStream            Reason = "return_is_stream"

	reasonArgBinderArity     Reason = "arg_binder_arity_mismatch"
	reasonArgBinderCount     Reason = "arg_binder_value_count_mismatch"
	reasonArgBinderOrder     Reason = "arg_binder_order_mismatch"
	reasonArgBinderMissing   Reason = "arg_binder_missing_value"
	reasonArgBinderDuplicate Reason = "arg_binder_duplicate_name"

	reasonReturnBundleInvalid          Reason = "return_bundle_invalid"
	reasonReturnBundleNotOutputUsable  Reason = "return_bundle_not_output_usable"
	reasonReturnBundleFinalUnsupported Reason = "return_bundle_native_final_unsupported"
	reasonReturnShapeUnproven          Reason = "return_shape_decoder_unproven"
	reasonStaticDescriptorStricter     Reason = "static_descriptor_stricter"
	reasonSpineNotExactAlias           Reason = "spine_not_exact_json_alias"
	reasonSpineRewriteProxyUnverified  Reason = "spine_rewrite_proxy_unverified"

	reasonStaticPromptUnsupported Reason = "static_prompt_unsupported"
	reasonStaticRenderFailed      Reason = "static_render_failed"

	reasonProviderNotOpenAI     Reason = "provider_not_openai"
	reasonStaticClientUnsupport Reason = "static_client_unsupported"
	reasonCanonicalBodyUnsuppor Reason = "canonical_body_unsupported"
	reasonRoundRobinStrategy    Reason = "round_robin_strategy"
	reasonRequestRetryOverride  Reason = "request_retry_override"
	reasonClientOverride        Reason = "client_override_unproven"
	reasonClientBlockAbsent     Reason = "client_block_absent"
	reasonBaseURLNotLiteral     Reason = "base_url_not_literal"
	reasonBaseURLAbsent         Reason = "base_url_absent"
	reasonAPIKeyNotLiteral      Reason = "api_key_not_literal"
	reasonAPIKeyAbsent          Reason = "api_key_absent"

	reasonNanollmNewFailed     Reason = "nanollm_new_failed"
	reasonNanollmPrepareFailed Reason = "nanollm_prepare_failed"
	reasonNoBAMLPlanClosure    Reason = "no_baml_plan_closure"
	reasonBAMLPlanBuildFailed  Reason = "baml_plan_build_failed"
	reasonBAMLPlanHeaders      Reason = "baml_plan_headers_invalid"
	reasonNativePlanHeaders    Reason = "native_plan_headers_invalid"

	reasonNativePlanMismatch Reason = "native_plan_mismatch"
	reasonPlanMatch          Reason = "plan_match"
)

// StaticInput is the whole set of request-wide + descriptor facts the static
// observe callback receives. Every field is neutral — no nanollm type crosses this
// boundary. The caller (the generated static seam, or the gated manifest test) has
// already resolved the FRESH descriptor via introspected.StaticPromptDescriptor and
// bound the exact typed arguments; AdmitStatic validates and observes them.
type StaticInput struct {
	// Build/flag/route (layer 1). Same neutral facts as the dynamic Input.
	WorkerCapable       bool
	RequestAPIPresent   bool
	OnBuildRequestRoute bool
	FlagEnabled         bool
	// RouteKind must be RouteKindStatic; AdmitStatic rejects anything else so a
	// mis-routed dynamic invocation can never reach the static predicate.
	RouteKind RouteKind

	// Method is the BAML function name. Descriptor is the FRESH per-call descriptor
	// the caller selected for Method; Args is the exact generated argument map;
	// ArgOrder is the declared descriptor-argument order the generated binder
	// emitted. Alias is the SEPARATE internal nanollm alias (never the target).
	Method     string
	Descriptor promptdescriptor.Function
	Args       map[string]any
	ArgOrder   []string
	// Values is the de-BAML Slice 7.1b PROJECTED argument vector the generated
	// projector produced (see bamlutils.NativeStaticInvocation.Values). It is the
	// ONLY host-value input the native static binder accepts; Args/ArgOrder stay
	// the BAML request facts the binder-match gate proves.
	Values []promptdescriptor.ArgumentValue
	Alias  string
	Mode   bamlutils.NativeStaticMode

	// Whole orchestration plan + selected-child facts (layer 2). Any shape outside
	// the narrow 8B matrix declines at the strategy/client stage before nanollm.New.
	SingleLeaf              bool
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool
	// Raw reports a call-with-raw request; the native static raw envelope is not
	// proven in 8B, so it declines at the mode gate before any render/Prepare.
	Raw            bool
	ClientOverride string
	Provider       string

	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's `Request.<Method>` plan WITHOUT sending, for
	// the strict plan compare. A nil closure records a decline (no plan to compare).
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)

	// Cohort is the serving-cutover S1 configuration identity + the default-deny
	// cohort gate it is evaluated against (layer 1b), exactly as on the dynamic
	// [Input]. Production leaves both halves zero, so every static request resolves
	// to CohortNone and declines with cohort_not_enrolled before any native work.
	Cohort CohortInput
}

// StaticObservation is the tri-state observation AdmitStatic records before forcing
// its decline. It ALWAYS implies a forced decline in 8B (the observer never claims).
// Every field is a bounded, secret-free token.
type StaticObservation struct {
	Observation bamlutils.NativeStaticObservation
	Family      bamlutils.NativeStaticObserveFamily
	Stage       string
	Reason      string
}

// staticAlias is the fixed internal nanollm alias the static observer uses when a
// caller supplies none. It is never the target model — nanollm maps it to the
// descriptor's literal model — and never appears in the request body.
const staticAlias = "baml_rest_static"

// staticPrepared is the request-scoped output of the static predicate through
// nanollm Prepare: the KEPT-ALIVE nanollm engine, the prepared plan, the lowered
// Return Bundle (the native static SAP surface), the exact-attempt request derived
// from the plan, and the alias. The observe path (AdmitStatic) closes the engine
// immediately; the serve path (AdmitStaticClaim) hands it to the serve core, which
// owns closing it. The client is NEVER left open on a decline.
type staticPrepared struct {
	client       *nanollm.Client
	prepared     *nanollm.PreparedRequest
	bundle       *schema.Bundle
	exactRequest *llmhttp.ExactAttemptRequest
	alias        string
	// cohort is the bounded configuration bucket the layer-1b gate resolved for this
	// request. It is carried out to the claim so the serve boundary attributes its
	// phase/winner telemetry to the identity admission actually gated on.
	cohort CohortID
}

func (p *staticPrepared) close() {
	if p != nil && p.client != nil {
		p.client.Close()
		p.client = nil
	}
}

// AdmitStatic runs the static unary no-send admission predicate and returns the
// recorded observation. It ALWAYS declines: it opens no socket, RoundTrips nothing,
// and returns whether the static route WOULD have admitted (byte-matched BAML's
// plan), MISMATCHED, or DECLINED at a specific gate. The caller forces the decline
// regardless of the observation. It is the OBSERVE-ONLY twin of [AdmitStaticClaim]:
// both share admitStaticThroughPrepare + the plan compare, but AdmitStatic closes
// the request-scoped engine immediately (no serve) while AdmitStaticClaim keeps it
// alive as a claim.
//
// The predicate order mirrors the dynamic admitCore where the two overlap and the
// proven no-send static chain (SupportsStatic -> RenderStatic -> NormalizeStaticClient
// -> BuildOpenAIChat -> nanollm Prepare -> BAML Request.<Method> compare) elsewhere.
func AdmitStatic(ctx context.Context, in StaticInput) StaticObservation {
	prep, dec := admitStaticThroughPrepare(ctx, in, false)
	if dec != nil {
		return *dec
	}
	// OBSERVE-ONLY: close the request-scoped engine immediately — no serve, no socket.
	defer prep.close()
	return staticPlanCompareObservation(ctx, in, prep)
}

// AdmitStaticClaim runs the SAME static unary predicate as [AdmitStatic] but, on a
// full would-admit (every gate passed AND the native plan byte-matched BAML's
// `Request.<Method>` plan), returns a *StaticClaim that KEEPS the request-scoped
// nanollm engine alive so the serve core can RoundTrip and TranslateResponse on the
// exact client Prepare produced the plan on (de-BAML Slice 8C). Every pre-claim
// decline/mismatch returns a *StaticDecline error and guarantees NO socket occurred
// (the engine is closed before returning), so the caller runs BAML for the same
// call in the same request — the exact tri-state pre-claim boundary.
func AdmitStaticClaim(ctx context.Context, in StaticInput) (*StaticClaim, error) {
	prep, dec := admitStaticThroughPrepare(ctx, in, false)
	if dec != nil {
		return nil, staticDeclineFromObs(*dec)
	}
	// Return-shape gate (SERVE-only): the native serve path can only OWN a return
	// shape whose per-method generated DecodeNativeStaticFinal mapper is
	// BAML-v0.223-differential-proven. SupportsNativeFinalBundle admits more than the
	// proven-decoder set (optionals, enums, nested classes, lists, maps), so a shape
	// the parser supports but the decoder has not proven declines PRE-CLAIM here so
	// BAML serves.
	//
	// This is the SOLE return-shape decision — there is no codegen-side twin to be in
	// lockstep with. Codegen emits the serve seam for EVERY serve-enabled static method
	// and makes no return-shape claim, because it cannot: see admittedStaticReturnShape
	// below for why (adapters/common has no root-module dependency and codegen's
	// Introspection carries no static descriptors), and
	// adapters/common/codegen.TestCodegenMakesNoStaticReturnShapeClaim for the proof
	// from the other side. The observe path (AdmitStatic) does NOT apply this gate — it
	// measures parser attachment, not decoder support.
	if !admittedStaticReturnShape(prep.bundle) {
		prep.close()
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonReturnShapeUnproven))
	}
	obs := staticPlanCompareObservation(ctx, in, prep)
	if obs.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		// A plan mismatch or a plan-compare decline: close the engine (no socket) and
		// hand a decline to the serve core so BAML serves.
		prep.close()
		return nil, staticDeclineFromObs(obs)
	}
	// Would-admit: transfer ownership of the kept-alive engine to the claim.
	return &StaticClaim{
		client:       prep.client,
		Prepared:     prep.prepared,
		Bundle:       prep.bundle,
		ExactRequest: prep.exactRequest,
		Alias:        prep.alias,
		Surface:      SurfaceStaticCall,
		Cohort:       prep.cohort,
	}, nil
}

// AdmitStaticSpineClaim is the ExecBridge-U1 codegen-spine unary admission entry. It
// reuses the SAME pre-socket predicate as [AdmitStaticClaim] (admitStaticThroughPrepare:
// mode gate, orchestration plan, descriptor envelope, arg binder, Return-Bundle
// lower/support, RenderStatic, canonical body, nanollm New/Prepare) with TWO
// substitutions for the hermetic, oracle-free spine cohort:
//
//   - the return-shape gate is the ONE root-owned totality predicate
//     debaml.SupportsNativeStaticStreamBundle — the exact five-arm `JSON` recursive
//     alias family — NOT admittedStaticReturnShape (which is the wider serve-decoder
//     set including JsonValue). The SAME predicate controls registration, call, and
//     direct parse in lockstep, and proves the admitted final parser cannot make a
//     support/value decline after the claim (see internal/debaml).
//   - the strict BAML `Request.<Method>` plan compare (staticPlanCompareObservation)
//     is OMITTED by design: the spine module carries no generated BAML plan closure,
//     and frozen v0.223 oracle evidence replaces it for this exact cohort.
//
// The spine lane's layer-1b cohort gate is skipped because this lane has its own
// registration-time admission. The lane is selected by an INTERNAL parameter to
// admitStaticThroughPrepare, NOT a caller-settable StaticInput field, so ONLY this entry
// point can skip the cohort gate — a caller of AdmitStatic/AdmitStaticParse/
// AdmitStaticClaim cannot opt into the skip (CodeRabbit #4; the same "the surface is the
// LANE's constant, never a caller field" rule the neighbouring gate states). Every
// pre-claim decline still guarantees NO socket occurred (the engine is closed before
// returning). On a full pass it returns a *StaticClaim that keeps the request-scoped
// nanollm engine alive for exactly one RoundTrip; every decline returns a typed
// *StaticDecline.
func AdmitStaticSpineClaim(ctx context.Context, in StaticInput) (*StaticClaim, error) {
	prep, dec := admitSpineThroughTotality(ctx, in)
	if dec != nil {
		return nil, dec
	}
	// No BAML plan compare — frozen v0.223 oracle evidence stands in for this exact
	// cohort. Transfer ownership of the kept-alive engine to the claim.
	return staticClaimFrom(prep), nil
}

// AdmitStaticSpineOracleClaim is the ExecBridge-U1c LIVE-oracle spine unary admission
// entry — the standard-worker twin of the frozen-evidence [AdmitStaticSpineClaim]. It
// runs the SAME shared spine prepare/rewrite/totality portion
// (admitSpineThroughTotality) and then, for the oracle entry ONLY, adds back the strict
// BAML `Request.<Method>` no-send plan compare (staticPlanCompareObservation) that the
// native-only lane deliberately omits: a standard SERVE worker CAN construct BAML's
// no-send plan (att.BuildBAMLRequest), so the live plan-match precondition is restored
// as the exact-cohort safety rail before any socket. Only a full
// NativeStaticObserveWouldAdmit (byte-match) transfers the kept-alive engine into a
// claim; every other observation (mismatch, nil/failed BAML builder, a send-path
// rewrite/proxy, a snapshot error) closes the engine and returns a typed *StaticDecline
// guaranteeing NO socket occurred, so the outer composite falls back to BAML.
//
// The fork from AdmitStaticSpineClaim is deliberately TWO named exported functions, not
// a boolean on StaticInput: a caller-controlled bit could accidentally bypass the
// compare, so the enrollment-gate bypass (spineLane) and the live-vs-frozen oracle
// choice are both internal to the lane, never caller-settable.
func AdmitStaticSpineOracleClaim(ctx context.Context, in StaticInput) (*StaticClaim, error) {
	prep, dec := admitSpineThroughTotality(ctx, in)
	if dec != nil {
		return nil, dec
	}
	// LIVE BAML plan compare (the standard-worker oracle rail): build BAML's `Request.<Method>`
	// plan WITHOUT sending and require a byte match before the claim. A nil/failed builder,
	// a send-path rewrite/proxy, a snapshot error, or a plan mismatch all close the kept-alive
	// engine (no socket) and decline so the outer composite runs BAML for the same call.
	obs := staticPlanCompareObservation(ctx, in, prep)
	if obs.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		prep.close()
		return nil, staticDeclineFromObs(obs)
	}
	// Would-admit: transfer ownership of the kept-alive engine to the claim.
	return staticClaimFrom(prep), nil
}

// admitSpineThroughTotality is the shared exact-spine pre-claim portion both spine
// admission entries run: layers 1-6 (admitStaticThroughPrepare with the spine-lane
// cohort-gate bypass), the MANDATORY fail-closed rewrite/proxy gate, and the ONE
// root-owned U1 totality cut. On success it returns the kept-alive *staticPrepared
// (the caller decides frozen-claim vs live-oracle-claim); on any decline it closes the
// engine (NO socket) and returns a typed *StaticDecline.
//
// The rewrite/proxy gate is MANDATORY and TOTAL at this boundary, not optional-on-nil:
// the spine lane is cohort-gate-EXEMPT, so a caller reaching a spine admission entry
// directly with a valid StaticInput but a NIL WouldRewriteOrProxy predicate must NOT be
// allowed to claim while the check is skipped (CodeRabbit #9 admission-boundary residual
// — the call.go default only covers UnaryExecutor.Call). FAIL CLOSED: a nil predicate
// means the effective target's rewrite/proxy status could not be verified, so decline
// PRE-CLAIM exactly as a positive rewrite/proxy verdict would. UnaryExecutor.Call always
// supplies a non-nil predicate (llmhttp.DefaultClient), so this only bites a direct
// caller that omitted it.
func admitSpineThroughTotality(ctx context.Context, in StaticInput) (*staticPrepared, *StaticDecline) {
	prep, dec := admitStaticThroughPrepare(ctx, in, true)
	if dec != nil {
		return nil, staticDeclineFromObs(*dec)
	}
	// A send-path rewrite/proxy on the EFFECTIVE target would make the exact-transport
	// evidence meaningless (the request would go elsewhere), so decline pre-claim against
	// the prepared URL. SingleLeaf / no-fallback / no-round-robin / no-retry-override were
	// already enforced by admitStaticThroughPrepare's layer-2 strategy gate.
	if in.WouldRewriteOrProxy == nil {
		prep.close()
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonSpineRewriteProxyUnverified))
	}
	if in.WouldRewriteOrProxy(prep.prepared.URL) {
		prep.close()
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonURLRewriteOrProxy))
	}
	// The ONE root-owned totality gate: the exact five-arm `JSON` alias family. It is
	// strictly narrower than admittedStaticReturnShape (it excludes the FINAL-served
	// JsonValue and every other alias/shape), so a non-exact-alias descriptor that
	// passed the shared final-support gate inside admitStaticThroughPrepare still
	// declines here PRE-CLAIM (no socket) and the outer policy serves it.
	if err := debaml.SupportsNativeStaticStreamBundle(prep.bundle); err != nil {
		prep.close()
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonSpineNotExactAlias))
	}
	return prep, nil
}

// staticClaimFrom transfers ownership of a fully-admitted kept-alive *staticPrepared
// into the request-scoped *StaticClaim the serve core RoundTrips on. The claim owns
// closing the engine from here.
func staticClaimFrom(prep *staticPrepared) *StaticClaim {
	return &StaticClaim{
		client:       prep.client,
		Prepared:     prep.prepared,
		Bundle:       prep.bundle,
		ExactRequest: prep.exactRequest,
		Alias:        prep.alias,
		Surface:      SurfaceStaticCall,
		Cohort:       prep.cohort,
	}
}

// admitStaticThroughPrepare runs the static predicate layers 1-6 (build/flag/route,
// mode, orchestration plan, descriptor envelope + arg binder, Return-Bundle
// lower/support, static prompt render, client normalize + canonical body, nanollm
// New/Prepare). On success it returns a *staticPrepared whose engine is KEPT ALIVE
// (the caller decides whether to close or claim it); on any gate decline it returns
// a *StaticObservation and leaves NO engine open. It performs NO plan compare and
// opens NO socket.
func admitStaticThroughPrepare(ctx context.Context, in StaticInput, spineLane bool) (*staticPrepared, *StaticObservation) {
	decline := func(family bamlutils.NativeStaticObserveFamily, stage Stage, reason Reason) (*staticPrepared, *StaticObservation) {
		o := declineStatic(family, stage, reason)
		return nil, &o
	}

	// --- Layer 1: build / flag / route --------------------------------------
	if err := ctx.Err(); err != nil {
		return decline(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
	}
	if in.RouteKind != RouteKindStatic {
		return decline(bamlutils.NativeStaticFamilyCapability, StageMethod, reasonRouteKindNotStatic)
	}
	if !in.WorkerCapable {
		return decline(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonWorkerNotCapable)
	}
	if !in.RequestAPIPresent {
		return decline(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonRequestAPIAbsent)
	}
	if !in.OnBuildRequestRoute {
		return decline(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonNotBuildReqRoute)
	}
	if !in.FlagEnabled {
		return decline(bamlutils.NativeStaticFamilyCapability, StageFlag, ReasonFlagDisabled)
	}

	// --- Mode gate: FAIL CLOSED before any render/Prepare -------------------
	// The predicate proves ONLY the unary FINAL surface. A stream mode (or any
	// unrecognized mode) declines here — before render, body build, nanollm New, or
	// Prepare — so a NativeStaticModeStream invocation can NEVER reach would_admit
	// or a claim. The parse-only mode is served by AdmitStaticParse, so it must not
	// reach here.
	if in.Mode != bamlutils.NativeStaticModeFinal {
		return decline(bamlutils.NativeStaticFamilyClient, StageMode, reasonModeNotFinal)
	}
	// Call-with-raw's native static envelope is not proven; decline it at the mode
	// gate (truthful — the generated seam forwards adapter.StreamMode().NeedsRaw()).
	if in.Raw {
		return decline(bamlutils.NativeStaticFamilyClient, StageMode, reasonCallWithRawUnprove)
	}

	// --- Layer 1b: DEFAULT-DENY cohort admission (serving cutover S1) -------
	// The static twin of admitCore's layer-1b gate, in the SHARED static core so
	// both the observe path (AdmitStatic) and the serve path (AdmitStaticClaim) —
	// and the Stage-1 static shadow, which also enters through AdmitStaticClaim —
	// evaluate it. It runs before the orchestration-plan, descriptor, render,
	// client-normalize and nanollm New/Prepare work below, so a non-enrolled static
	// configuration costs ZERO native work and provably opens no socket. The surface
	// is the LANE's constant (the internal spineLane parameter), never a caller field.
	// It only NARROWS.
	//
	// The bounded observe FAMILY is `capability`: like the flag/route/build rows just
	// above, this is a deployment-level refusal, and the precision lives in the
	// (cohort, cohort_not_enrolled) stage/reason pair rather than in a new
	// cross-module enum value.
	// The ExecBridge-U1 spine lane skips the dynamic-rollout default-deny cohort gate:
	// it is a separate lane whose admission is its own root-owned totality gate over
	// the emitted exact-JSON cohort (resolved at registration), and it must NOT widen
	// the dynamic cohort manifest. cohort stays CohortNone, carried out for telemetry.
	// Every non-spine caller keeps the gate exactly.
	cohort := CohortNone
	if !spineLane {
		c, cd := admitCohort(SurfaceStaticCall, in.Cohort)
		if cd != nil {
			return decline(bamlutils.NativeStaticFamilyCapability, cd.Stage, cd.Reason)
		}
		cohort = c
	}

	// --- Layer 2: whole orchestration plan + selected-child facts -----------
	// The narrow matrix is unary final/parse-only, a single resolved leaf, the
	// descriptor default client, no override/strategy/retry/proxy. Any broader shape
	// is a MEASURED client decline BEFORE the descriptor is even lowered.
	if !in.SingleLeaf {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonNotSingleLeaf)
	}
	if in.HasFallbackChain {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonFallbackChain)
	}
	if in.HasRoundRobin {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonRoundRobinStrategy)
	}
	if in.HasRequestRetryOverride {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonRequestRetryOverride)
	}
	// A non-empty client override is admissible ONLY when it NAMES the descriptor's
	// own resolved default client (in.Descriptor.Client) — the generated /call
	// orchestrator always surfaces the resolved leaf client name as the override, even
	// for a single default-client request, so declining every non-empty override would
	// decline the entire generated default-client route. Naming the default client
	// selects the SAME client config the descriptor baked; a DIFFERENT name is an
	// unproven override and declines. The strict BAML `Request.<Method>` plan compare
	// (staticPlanMatches) is the byte-exact safety net: if a runtime registry redefined
	// that client name with different options, native (descriptor ClientConfig) and
	// BAML (runtime registry) would produce different plans and decline at plan compare.
	if in.ClientOverride != "" && in.ClientOverride != in.Descriptor.Client {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonClientOverride)
	}

	// --- Layer 3: descriptor envelope --------------------------------------
	fn := in.Descriptor
	if d := checkStaticEnvelope(fn, in.Method); d != nil {
		return nil, d
	}
	// Arg binder EXACTLY matches the descriptor's declared arguments (final only —
	// a parse has no arguments).
	if d := checkArgBinder(fn.Args, in.ArgOrder, in.Args); d != "" {
		return decline(bamlutils.NativeStaticFamilyDescriptorEnvelope, StageMethod, d)
	}

	// --- Layer 3b: Return Bundle lower / validate / native final support ----
	bundle, d := checkStaticReturnBundle(fn)
	if d != nil {
		return nil, d
	}

	// --- Layer 4: static prompt render support ------------------------------
	if serr := nativeprompt.SupportsStatic(fn, in.Values); serr != nil {
		var pd *nativeprompt.Decline
		if errors.As(serr, &pd) && pd.Feature == nativeprompt.FeatureStaticDescriptor {
			// A stricter envelope decline than the explicit checks above.
			return decline(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonStaticDescriptorStricter)
		}
		return decline(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticPromptUnsupported)
	}
	rendered, rerr := nativeprompt.RenderStatic(fn, in.Values)
	if rerr != nil {
		// SupportsStatic shares the preparer, so a render error here is unexpected —
		// still a pre-socket decline (BAML serves), classified as a prompt decline.
		return decline(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticRenderFailed)
	}

	// --- Layer 5: static client normalize + canonical body ------------------
	if in.Provider != "openai" {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonProviderNotOpenAI)
	}
	alias := staticAliasOr(in.Alias)
	intent, cerr := nativebody.NormalizeStaticClient(fn.ClientConfig, alias, false)
	if cerr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonStaticClientUnsupport)
	}
	baseURL, apiKey, terr := staticTransport(fn.ClientConfig)
	if terr != "" {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, terr)
	}
	native, borr := nativebody.BuildOpenAIChat(rendered, intent)
	if borr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StagePrompt, reasonCanonicalBodyUnsuppor)
	}

	// --- Layer 6: nanollm New/Prepare (NO SEND) -----------------------------
	if err := ctx.Err(); err != nil {
		return decline(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
	}
	client, nerr := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       alias,
			Model:      "openai/" + intent.TargetModel,
			APIKey:     apiKey,
			BaseURL:    baseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if nerr != nil {
		return decline(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNanollmNewFailed)
	}

	prep, perr := client.Prepare(nanollm.Request{
		Model:  alias,
		Body:   native.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if perr != nil {
		// Prepare failed AFTER New succeeded: close the engine so no decline path
		// leaks a live client.
		client.Close()
		return decline(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNanollmPrepareFailed)
	}

	return &staticPrepared{
		client:       client,
		prepared:     prep,
		bundle:       bundle,
		exactRequest: exactRequestFromPlan(prep),
		alias:        alias,
		cohort:       cohort,
	}, nil
}

// staticPlanCompareObservation runs the strict BAML `Request.<Method>` no-send plan
// compare (predicate layer 7) over a prepared static request and returns the
// tri-state observation: would-admit (byte match), plan-mismatch (Prepare OK but
// plan differs), or a decline (missing/failed BAML builder, a send-path
// rewrite/proxy, or a snapshot construction error). It opens NO socket.
func staticPlanCompareObservation(ctx context.Context, in StaticInput, prep *staticPrepared) StaticObservation {
	if in.BuildBAMLRequest == nil {
		return declineStatic(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNoBAMLPlanClosure)
	}
	bamlReq, buildErr := in.BuildBAMLRequest(ctx)
	if buildErr != nil || bamlReq == nil {
		return declineStatic(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonBAMLPlanBuildFailed)
	}
	// A send-path rewrite/proxy on the EFFECTIVE target would make a byte "match"
	// meaningless (BAML sends elsewhere); decline pre-plan-record like the dynamic
	// path does, against the prepared URL.
	if in.WouldRewriteOrProxy != nil && in.WouldRewriteOrProxy(prep.prepared.URL) {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonURLRewriteOrProxy)
	}

	match, cmpReason := staticPlanMatches(bamlReq, prep.prepared)
	if cmpReason != "" {
		return declineStatic(bamlutils.NativeStaticFamilyPrepare, StagePrepare, cmpReason)
	}
	if !match {
		// Every pre-plan gate passed and Prepare succeeded, but the native plan did
		// not byte-match BAML's plan. Observable routing signal; still declines.
		return StaticObservation{
			Observation: bamlutils.NativeStaticObservePlanMismatch,
			Family:      bamlutils.NativeStaticFamilyNone,
			Stage:       string(StagePlanCompare),
			Reason:      string(reasonNativePlanMismatch),
		}
	}

	// Proven up to — but NOT including — the RoundTrip. The observe path records this
	// and forces the decline; the claim path CLAIMS here.
	return StaticObservation{
		Observation: bamlutils.NativeStaticObserveWouldAdmit,
		Family:      bamlutils.NativeStaticFamilyNone,
		Stage:       string(StagePlanCompare),
		Reason:      string(reasonPlanMatch),
	}
}

// AdmitStaticParse runs the static PARSE-ONLY no-send admission observation and
// returns the recorded observation. It is the /parse twin of AdmitStatic: it
// validates the descriptor envelope and MEASURES the Return-Bundle native-final
// support (the parser surface a later serving slice would own), then ALWAYS
// declines so BAML's Parse.<Method> serves the same response exactly as today. It
// binds no arguments, renders no prompt, builds no body, and opens no socket —
// there is no BAML request plan to compare for a parse.
func AdmitStaticParse(ctx context.Context, in StaticInput) StaticObservation {
	if err := ctx.Err(); err != nil {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
	}
	if in.RouteKind != RouteKindStatic {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageMethod, reasonRouteKindNotStatic)
	}
	// Parse-only mode gate: FAIL CLOSED for any non-parse-only mode (a final/stream
	// invocation must not reach the parse-only surface).
	if in.Mode != bamlutils.NativeStaticModeParseOnly {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageMode, reasonModeNotParseOnly)
	}
	if !in.WorkerCapable {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonWorkerNotCapable)
	}
	if !in.RequestAPIPresent {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonRequestAPIAbsent)
	}
	if !in.OnBuildRequestRoute {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageCapability, ReasonNotBuildReqRoute)
	}
	if !in.FlagEnabled {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageFlag, ReasonFlagDisabled)
	}
	// Layer 1b: the DEFAULT-DENY cohort gate, on the parse-only leg too. This
	// predicate does no native work at all (no bind, no render, no body, no nanollm)
	// and ALWAYS declines, so gating it changes no serving behaviour — it is here so
	// the rule has NO exceptions: every exported admission entry point evaluates the
	// gate the moment its surface is established. The parse-only leg belongs to the
	// static CALL surface; the direct `/parse/{method}` endpoint is a different
	// surface with no native seam at all (see SurfaceDirectParse).
	if _, cd := admitCohort(SurfaceStaticCall, in.Cohort); cd != nil {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, cd.Stage, cd.Reason)
	}

	fn := in.Descriptor
	if d := checkStaticEnvelope(fn, in.Method); d != nil {
		return *d
	}
	if _, d := checkStaticReturnBundle(fn); d != nil {
		return *d
	}

	// The parse-only Return Bundle is a supported native-final surface. 8B records
	// the would-admit-parse observation and forces the decline (BAML parses).
	return StaticObservation{
		Observation: bamlutils.NativeStaticObserveWouldAdmit,
		Family:      bamlutils.NativeStaticFamilyNone,
		Stage:       string(StagePlanCompare),
		Reason:      string(reasonPlanMatch),
	}
}

// DeclineStaticMode returns a fail-closed decline observation for a static
// observation mode the predicates do not handle (a stream mode, or any
// unrecognized mode). It is the strict observer router's default so a
// non-final/non-parse-only invocation never reaches render or Prepare — 8B claims
// no stream route (no stream result/mapping contract exists). The mode is a bounded
// enum token, safe to carry.
func DeclineStaticMode(mode bamlutils.NativeStaticMode) StaticObservation {
	_ = mode
	return declineStatic(bamlutils.NativeStaticFamilyClient, StageMode, reasonModeUnsupported)
}

// declineStatic builds a decline observation with the bounded family/stage/reason.
func declineStatic(family bamlutils.NativeStaticObserveFamily, stage Stage, reason Reason) StaticObservation {
	return StaticObservation{
		Observation: bamlutils.NativeStaticObserveDecline,
		Family:      family,
		Stage:       string(stage),
		Reason:      string(reason),
	}
}

// checkStaticEnvelope validates the descriptor envelope shared by the final and
// parse-only predicates: the descriptor is present and names Method, the descriptor
// + Return schema versions match, the Return names the same method, and the Return
// is the non-streaming variant. It returns nil on success or a pointer to a bounded
// descriptor-envelope decline observation. It inspects no argument or return VALUE
// (secret-free).
func checkStaticEnvelope(fn promptdescriptor.Function, method string) *StaticObservation {
	envelope := func(r Reason) *StaticObservation {
		o := declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StageMethod, r)
		return &o
	}
	switch {
	case fn.Method == "":
		return envelope(reasonDescriptorAbsent)
	case fn.Method != method:
		return envelope(reasonDescriptorMethodMismatch)
	case fn.Version != promptdescriptor.Version:
		return envelope(reasonDescriptorVersionMismatch)
	case fn.Return.Version != schemadescriptor.Version:
		return envelope(reasonReturnVersionMismatch)
	case fn.Return.Method != fn.Method:
		return envelope(reasonReturnMethodMismatch)
	case fn.Return.Stream:
		return envelope(reasonReturnIsStream)
	}
	return nil
}

// checkStaticReturnBundle lowers the descriptor's Return to an internal Bundle,
// validates it is output-usable, and proves the native FINAL parser can own the
// whole type graph (SupportsNativeFinalBundle) — the parser surface both the /call
// and /parse observations require. It also MEASURES stream support but never claims
// a stream route (no stream result/mapping contract exists in 8B). It returns the
// lowered bundle on success, or nil + a bounded descriptor-envelope decline
// (including the correct 8B recursion / out-of-bounds decline a later slice lifts).
func checkStaticReturnBundle(fn promptdescriptor.Function) (*schema.Bundle, *StaticObservation) {
	envelope := func(r Reason) *StaticObservation {
		o := declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, r)
		return &o
	}
	bundle, berr := schema.FromStaticDescriptor(fn.Return)
	if berr != nil {
		return nil, envelope(reasonReturnBundleInvalid)
	}
	if verr := bundle.ValidateOutput(); verr != nil {
		return nil, envelope(reasonReturnBundleNotOutputUsable)
	}
	if serr := debaml.SupportsNativeFinalBundle(bundle); serr != nil {
		return nil, envelope(reasonReturnBundleFinalUnsupported)
	}
	// MEASURE stream support but NEVER claim a stream route.
	_ = debaml.SupportsNativeStreamBundle(bundle)
	return bundle, nil
}

// admittedStaticReturnShape reports whether the lowered Return Bundle is inside the
// INITIAL admitted return-shape set — the EXACT shapes whose generated
// DecodeNativeStaticFinal mapper is BAML-v0.223-**differential-proven** (see
// internal/nativebody/nanollmprepare/static/mapper_differential_integration_test.go).
//
// IT IS THE SOLE RETURN-SHAPE GATE. There is no codegen-side twin to stay in lockstep
// with, and an earlier revision of this comment named one (`isAdmittedStaticServeReturn`
// in adapters/common/codegen) that has never existed. That was a false claim about where
// the decision lives, and the reason a codegen twin cannot exist is structural rather
// than an omission: `adapters/common` does not depend on the root module at all (its
// go.mod requires only bamlutils + introspected), and codegen's own [Introspection]
// carries no static prompt descriptors — so codegen cannot see a method's RETURN SCHEMA
// at emission time and cannot evaluate the fingerprint. What codegen does emit for every
// serve-enabled static method is unconditional: the seam plus a STRICT
// bamlutils.DecodeStaticFinal closure at the method's concrete return type. That is not
// an admission claim; admission is decided here, pre-claim and pre-socket, and
// TestCodegenMakesNoStaticReturnShapeClaim (adapters/common/codegen) pins the absence
// structurally so a future codegen-side claim cannot appear unnoticed.
//
// The set is deliberately NARROWED to EXACTLY the two shapes the v0.223 differential
// covers (review P1.3) — NOT every flat class of bare string/int fields (which would
// let an UNTESTED class such as `C{count int; title string; extra string}` claim a
// send with no BAML differential). A shape outside the precisely-proven set declines
// PRE-CLAIM so it is never served on an unproven bridge (scope §8.1):
//
//   - a top-level primitive STRING target — proven by StaticCompletion. No
//     enums/classes/aliases/recursion, no target constraints.
//   - a flat class target with EXACTLY the two fields, in order, `answer` (string)
//     then `confidence` (int), no @alias/constraints, single class, no nested/list/
//     map/union/enum/alias/recursion — the exact StaticAnswer{answer:string,
//     confidence:int} shape the mapper differential proves.
//   - de-BAML Slice 7.2b-3: the same two-field pair with EXACTLY ONE direct
//     `@check`/`@assert(<ascii label>, {{ this > <int> }})` on `confidence`, under the
//     two pinned class names — see isAdmittedStaticCheckedReturn, which delegates the
//     whole decision to the root-owned fingerprint.
//
// Every other shape — a top-level int/float/bool scalar, ANY other class (different
// field names/types/order/count, a float/bool field, optionals, enums, unions, maps,
// lists, nested/recursive classes, aliases, field aliases, and every constraint outside
// that one fingerprint) — declines until its OWN mapper + v0.223 differential fixture +
// explicit gate is added. This is the precise "every admitted shape has a mapper +
// fixture" boundary.
func admittedStaticReturnShape(b *schema.Bundle) bool {
	if b == nil {
		return false
	}
	// De-BAML Phase 3a/3c: the admitted structural-recursive-ALIAS families (the direct
	// five-arm `JSON` alias and the nullable six-stored-variant `JsonValue` alias) are
	// served through the NARROW per-method alias materializer/decoder, gated by the SAME
	// exact fingerprints the root-owned parser profile uses
	// (debaml.IsProvenServedRecursiveAliasStaticFamily — the exact OR of the two
	// name-pinned predicates, spelled ONCE in the root package so the two admission
	// lanes cannot drift). Checked BEFORE the generic alias reject below so the served
	// shapes and the parser stay in exact lockstep; every OTHER alias bundle still
	// declines at the reject.
	if isProvenRecursiveAliasStaticReturn(b) {
		return true
	}
	// De-BAML Slice 7.2b-3: the ONE constraint-bearing family native serves — the two
	// concrete generated fixture return types
	// `{answer string; confidence int @check|@assert(<ascii>, {{ this > <int> }})}`,
	// whose generated decoder is bamlutils.DecodeStaticFinal at Checked[int64] / int64
	// and whose bytes are pinned against the real BAML v0.223.0 CFFI
	// (internal/debaml/checkedwire). Checked BEFORE the generic per-field reject below,
	// which refuses every constrained field, and delegated to the ROOT-owned predicate so
	// this gate cannot drift from the parser that has to produce the bytes.
	//
	// Every OTHER constraint-bearing return still falls through to that reject.
	if isAdmittedStaticCheckedReturn(b) {
		return true
	}
	// Structural recursive aliases and enums stay declined (no proven decoder).
	if len(b.StructuralRecursiveAliases) > 0 || len(b.Enums) > 0 {
		return false
	}
	// De-BAML Phase 2: the admitted recursive-class family is served through the SAME
	// generic DecodeStaticFinal carrier (a legal *Node/*A/*B pointer graph), gated by a
	// descriptor FINGERPRINT so only the three v0.223-differential-proven shapes claim a
	// socket. A non-recursive bundle keeps the exact 8C string/StaticAnswer gate below
	// byte-for-byte unchanged (the 5/5/5/5/5/2/3/1 pin is untouched).
	if len(b.RecursiveClasses) > 0 {
		return isProvenRecursiveStaticReturn(b)
	}
	switch b.Target.Kind {
	case schema.TypePrimitive:
		// PROVEN scalar target: top-level string ONLY (StaticCompletion). int/float/bool
		// scalar returns have no v0.223 differential method, so decline.
		return len(b.Classes) == 0 && isProvenStaticStringScalar(b.Target)
	case schema.TypeClass:
		// PROVEN class target: EXACTLY the StaticAnswer{answer:string, confidence:int}
		// field shape (names + types + order), single class, no constraints. Reject a
		// CONSTRAINED (`-> C @check/@assert(...)`) or DYNAMIC (`@@dynamic`) class target
		// at the usage site too — schema.Type carries Meta.Constraints + Dynamic on the
		// target independently of the resolved ClassDef, so neither may slip the gate.
		if b.Target.Dynamic || len(b.Target.Meta.Constraints) > 0 {
			return false
		}
		if len(b.Classes) != 1 {
			return false
		}
		cd, ok := b.FindClass(b.Target.Name, b.Target.Mode)
		if !ok || len(cd.Constraints) > 0 || len(cd.Fields) != 2 {
			return false
		}
		return isProvenStaticField(cd.Fields[0], "answer", schema.PrimitiveString) &&
			isProvenStaticField(cd.Fields[1], "confidence", schema.PrimitiveInt)
	default:
		return false
	}
}

// isProvenRecursiveAliasStaticReturn reports whether the lowered Return Bundle is
// EXACTLY one of the two served recursive-alias families:
//
//	type JSON      = int | string | bool | JSON[] | map<string, JSON>
//	type JsonValue = int | float | bool | string | null
//	               | JsonValue[] | map<string, JsonValue>
//
// It delegates to debaml.IsProvenServedRecursiveAliasStaticFamily so the isolated
// nativeserve serve gate and the root-owned parser profile stay in EXACT lockstep — a
// newly-added or wider alias (a renamed/wrapped alias, an extra or reordered arm) can
// never claim a socket until it brings its own oracle rows and is intentionally
// admitted. The OR lives in the root package, NOT here: spelling it in this isolated
// module is exactly how the final gate and the stream gate would drift apart.
func isProvenRecursiveAliasStaticReturn(b *schema.Bundle) bool {
	return debaml.IsProvenServedRecursiveAliasStaticFamily(b)
}

// isAdmittedStaticCheckedReturn reports whether the lowered Return Bundle is the ONE
// checked-static fingerprint of de-BAML Slice 7.2b-3:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(<ascii>, {{ this > <int> }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(<ascii?>, {{ this > <int> }}) }
//
// It DELEGATES to debaml.IsAdmittedStaticCheckedFamily, which is the exported form of
// the very predicate debaml.SupportsNativeFinalBundle consults, so this serve gate and
// the parser that must produce the bytes can never disagree about which schema is
// claimed. Restating the fingerprint in this isolated module is exactly how the two
// would drift: a second constraint, a duplicate label, an alias, a reordered field pair,
// a non-ASCII label, a different predicate or a differently-named class has no stock
// byte capture behind it and must decline PRE-CLAIM.
func isAdmittedStaticCheckedReturn(b *schema.Bundle) bool {
	return debaml.IsAdmittedStaticCheckedFamily(b)
}

// isProvenStaticStringScalar reports whether t is a bare, constraint-free primitive
// STRING — the only proven top-level scalar return (StaticCompletion).
func isProvenStaticStringScalar(t schema.Type) bool {
	return t.Kind == schema.TypePrimitive && len(t.Meta.Constraints) == 0 && t.Primitive == schema.PrimitiveString
}

// isProvenStaticField reports whether f is EXACTLY the named, bare (no @alias, no
// constraints) primitive field of kind prim — a field of the differential-proven
// StaticAnswer shape. Requiring the field name + type + no-alias is what keeps the
// admitted class shape to precisely the shape the mapper differential covers, so no
// structurally-different (untested) class can claim a send.
func isProvenStaticField(f schema.ClassField, name string, prim schema.PrimitiveKind) bool {
	if f.Name.Alias != nil || f.Name.Name != name {
		return false
	}
	if f.Type.Dynamic || len(f.Type.Meta.Constraints) > 0 {
		return false
	}
	return f.Type.Kind == schema.TypePrimitive && f.Type.Primitive == prim
}

// isProvenRecursiveStaticReturn reports whether the lowered Return Bundle is EXACTLY
// one of the THREE fingerprint-proven recursive-class shapes whose generated
// DecodeStaticFinal pointer carrier + BAML v0.223 differential are proven (the
// static-recursion manifest): the SELF-recursive Node{value string; next Node?}, and
// the MUTUAL A{value string; b B?} <-> B{value string; a A?} SCC rooted at A OR at B.
//
// It DELEGATES to the root-owned debaml.IsProvenRecursiveStaticFamily so the serve
// fingerprint and the parser's admittedRecursiveClassProfile can NEVER diverge — a
// single EXACT predicate (canonical class AND field names, exactly two fields / one
// direct nullable-class edge, non-streaming ClassKeys, no @alias / constraints /
// dynamic / enum / variant-metadata anywhere). A same-shaped but non-canonical
// `Other{payload string; child Other?}`, a required/list/map edge, a one-field Loop,
// an extra field, a reordered field, a bool/float/int value, a class-name alias, or a
// constrained variant all decline PRE-CLAIM.
func isProvenRecursiveStaticReturn(b *schema.Bundle) bool {
	return debaml.IsProvenRecursiveStaticFamily(b)
}

// NewStaticResponseClient builds a FRESH request-scoped nanollm engine for the SAME
// static descriptor client (base_url/api_key/target model from the descriptor's
// ClientConfig), purely to TranslateResponse over BAML's already-fetched bytes in the
// Stage-1 SHADOW comparison. It opens NO socket — the returned engine is used only by
// execute.ConsumeResponse's non-transport translate/extract/SAP. It mirrors the
// admitStaticThroughPrepare client build (layers 5-6) and is the static twin of the
// dynamic shadow's NewResponseClient. The caller MUST Close the returned engine.
func NewStaticResponseClient(fn promptdescriptor.Function, alias string) (*nanollm.Client, string, error) {
	alias = staticAliasOr(alias)
	intent, cerr := nativebody.NormalizeStaticClient(fn.ClientConfig, alias, false)
	if cerr != nil {
		return nil, "", cerr
	}
	baseURL, apiKey, terr := staticTransport(fn.ClientConfig)
	if terr != "" {
		return nil, "", fmt.Errorf("nativeserve: static response client: %s", terr)
	}
	client, nerr := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       alias,
			Model:      "openai/" + intent.TargetModel,
			APIKey:     apiKey,
			BaseURL:    baseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if nerr != nil {
		return nil, "", nerr
	}
	return client, intent.TargetModel, nil
}

// staticAliasOr returns the caller alias or the fixed internal default.
func staticAliasOr(alias string) string {
	if alias == "" {
		return staticAlias
	}
	return alias
}

// checkArgBinder proves the generated binder's argument names match the
// descriptor's declared argument order EXACTLY (name + order) and that the bound
// value map carries exactly those keys. It returns "" on an exact match or a
// bounded reason token otherwise. It never inspects a value (secret-free).
func checkArgBinder(declared []promptdescriptor.Argument, binderOrder []string, values map[string]any) Reason {
	if len(binderOrder) != len(declared) {
		return reasonArgBinderArity
	}
	if len(values) != len(declared) {
		return reasonArgBinderCount
	}
	seen := make(map[string]struct{}, len(declared))
	for i, arg := range declared {
		if binderOrder[i] != arg.Name {
			return reasonArgBinderOrder
		}
		if _, dup := seen[arg.Name]; dup {
			return reasonArgBinderDuplicate
		}
		seen[arg.Name] = struct{}{}
		if _, ok := values[arg.Name]; !ok {
			return reasonArgBinderMissing
		}
	}
	return ""
}

// CheckStaticClientCohort runs the SAME static-client cohort checks the call-time
// admission applies (admitStaticThroughPrepare layer 5), WITHOUT a render or socket — so
// a REGISTRATION-time gate declines exactly what Call would, keeping
// registration/call/direct-parse in lockstep for a cohort-forbidden client descriptor
// (Codex review findings 2 + 3-#1). The three checks mirror the three layer-5 gates:
//
//  1. the method/request provider gate (Call's `in.Provider != "openai"`);
//  2. the ACTUAL call-time client predicate on the NORMALIZED INTENT — the identical
//     nativebody.SupportsOpenAIChat / BuildOpenAIChat gate — covering the intent's OWN
//     provider, a resolved literal VALID-UTF-8 target model, and no body-affecting
//     option; and
//  3. the literal base_url + api_key transport (Call's staticTransport, consumed by
//     nanollm.New).
//
// Gate 2 is the fix for review-3 finding 1: the earlier version checked only the passed
// `provider` argument (the METHOD provider) and intent.BodyAffecting, never the
// NORMALIZED intent's own provider or model validity. A validated Project may pair
// Method.Provider=="openai" with a SELECTED ClientConfig whose Provider!="openai" (the
// manifest validator does not force them to agree), or a literal model that is not valid
// UTF-8 — NormalizeStaticClient passes both through, and only BuildOpenAIChat declines
// them at Call. Running SupportsOpenAIChat here on the intent closes that
// register-admits / call-declines divergence. A canned valid completion prompt satisfies
// supportsRendered, so the only decline SupportsOpenAIChat can surface here is the client
// gate. Returns nil when the client is inside the exact cohort.
func CheckStaticClientCohort(provider string, cfg promptdescriptor.ClientConfig) error {
	if provider != "openai" {
		return fmt.Errorf("client cohort: provider is %q, only openai is proven", provider)
	}
	intent, cerr := nativebody.NormalizeStaticClient(cfg, staticAlias, false)
	if cerr != nil {
		return fmt.Errorf("client cohort: static client unsupported: %w", cerr)
	}
	probe := &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: staticAlias}
	if err := nativebody.SupportsOpenAIChat(probe, intent); err != nil {
		return fmt.Errorf("client cohort: selected client is outside the exact cohort: %w", err)
	}
	if _, _, terr := staticTransport(cfg); terr != "" {
		return fmt.Errorf("client cohort: transport option %s", terr)
	}
	return nil
}

// staticTransport extracts the descriptor client's base_url + api_key as build-time
// LITERALS for the no-send nanollm client. Both must be string literals (an env.X
// reference is a runtime value native cannot reproduce build-time), and base_url
// must be present (8B does not synthesize a provider default). It returns a bounded
// reason token on any decline. The returned values are SENSITIVE (api_key) — the
// caller passes them straight into nanollm.New and never logs them.
func staticTransport(cfg promptdescriptor.ClientConfig) (baseURL, apiKey string, reason Reason) {
	if !cfg.Present {
		return "", "", reasonClientBlockAbsent
	}
	for _, o := range cfg.TransportOptions {
		switch o.Key {
		case "base_url":
			if o.Value.Kind != promptdescriptor.OptionString {
				return "", "", reasonBaseURLNotLiteral
			}
			baseURL = o.Value.String
		case "api_key":
			if o.Value.Kind != promptdescriptor.OptionString {
				return "", "", reasonAPIKeyNotLiteral
			}
			apiKey = o.Value.String
		}
	}
	if baseURL == "" {
		return "", "", reasonBaseURLAbsent
	}
	if apiKey == "" {
		return "", "", reasonAPIKeyAbsent
	}
	return baseURL, apiKey, ""
}

// staticPlanMatches reports whether the nanollm prepared plan matches BAML's
// `Request.<Method>` no-send plan under the EXACT comparison the scope-ratified
// Slice 5.1 static prepared-request differential uses
// (internal/nativebody/nanollmprepare/static/prepared_request_integration_test.go):
// method byte-exact, URL byte-exact, body byte-exact, and the SEMANTIC header set
// (name+value) exact.
//
// Header ORDER and CASING are NOT compared, and BAML's internal `baml-original-url`
// transport header is exempted (testutil.Diff). This is not a relaxation the
// comparator chose to hide a difference: BAML's Go `HTTPRequest.Headers()` surface
// (the only header representation the neutral BuildBAMLRequest closure can obtain)
// is an UNORDERED `map[string]string`, so no raw BAML header order exists at this
// boundary to compare against nanollm's ordered plan; casing is HTTP-insignificant;
// and `baml-original-url` is BAML's own transport/rewrite header nanollm never
// emits. nanollm's OWN emitted header order is asserted independently by the Slice
// 5.1 differential, never against BAML. This is therefore the strictest raw
// comparison the BAML Go surface admits, and is the same ratified oracle the static
// arc uses; a would-admit means byte-exact method/URL/body + exact semantic headers.
//
// It returns a bounded reason token only on a snapshot construction error (a
// duplicate/multi-value header), never a value. It NEVER logs the plans.
func staticPlanMatches(bamlReq *llmhttp.Request, prep *nanollm.PreparedRequest) (bool, Reason) {
	bamlSnap, err := testutil.NewSnapshot(bamlReq.Method, bamlReq.URL, testutil.PairsFromStringMap(bamlReq.Headers), []byte(bamlReq.Body))
	if err != nil {
		return false, reasonBAMLPlanHeaders
	}
	prepSnap, err := testutil.NewSnapshot(prep.Method, prep.URL, prep.Headers, prep.Body)
	if err != nil {
		return false, reasonNativePlanHeaders
	}
	return len(testutil.Diff(bamlSnap, prepSnap)) == 0, ""
}
