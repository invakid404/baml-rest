package admission

import (
	"context"
	"errors"

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

// De-BAML Slice 8B — static unary no-send admission (OBSERVE-ONLY).
//
// This file adds the STATIC native admission route alongside the existing dynamic
// one. It is deliberately OBSERVE-ONLY: AdmitStatic runs the full pre-socket
// predicate (envelope validation, arg-binder match, Return-Bundle lower/support,
// RenderStatic, canonical body, nanollm New/Prepare, and the strict BAML
// `Request.<Method>` no-send plan compare) and then ALWAYS records an observation
// and forces a decline. It NEVER claims a native attempt, NEVER opens a socket, and
// NEVER RoundTrips — the generated `Request.<Method>` / `Parse.<Method>` (BAML)
// serve every request exactly as today. It proves ATTACHMENT with zero behavior
// change.
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
	reasonStaticDescriptorStricter     Reason = "static_descriptor_stricter"

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
	Alias      string
	Mode       bamlutils.NativeStaticMode

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

// AdmitStatic runs the static unary no-send admission predicate and returns the
// recorded observation. It ALWAYS declines: it opens no socket, RoundTrips nothing,
// and returns whether the static route WOULD have admitted (byte-matched BAML's
// plan), MISMATCHED, or DECLINED at a specific gate. The caller forces the decline
// regardless of the observation.
//
// The predicate order mirrors the dynamic admitCore where the two overlap and the
// proven no-send static chain (SupportsStatic -> RenderStatic -> NormalizeStaticClient
// -> BuildOpenAIChat -> nanollm Prepare -> BAML Request.<Method> compare) elsewhere.
func AdmitStatic(ctx context.Context, in StaticInput) StaticObservation {
	// --- Layer 1: build / flag / route --------------------------------------
	if err := ctx.Err(); err != nil {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
	}
	if in.RouteKind != RouteKindStatic {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageMethod, reasonRouteKindNotStatic)
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

	// --- Mode gate: FAIL CLOSED before any render/Prepare -------------------
	// AdmitStatic proves ONLY the unary FINAL surface. A stream mode (or any
	// unrecognized mode) declines here — before render, body build, nanollm New, or
	// Prepare — so a NativeStaticModeStream invocation can NEVER reach would_admit.
	// The parse-only mode is served by AdmitStaticParse, so it must not reach here.
	if in.Mode != bamlutils.NativeStaticModeFinal {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageMode, reasonModeNotFinal)
	}
	// Call-with-raw's native static envelope is not proven in 8B; decline it at the
	// mode gate (truthful — the generated seam forwards adapter.StreamMode().NeedsRaw()).
	if in.Raw {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageMode, reasonCallWithRawUnprove)
	}

	// --- Layer 2: whole orchestration plan + selected-child facts -----------
	// The narrow 8B matrix is unary final/parse-only, a single resolved leaf, the
	// descriptor default client, no override/strategy/retry/proxy. Any broader shape
	// is a MEASURED client decline BEFORE the descriptor is even lowered.
	if !in.SingleLeaf {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonNotSingleLeaf)
	}
	if in.HasFallbackChain {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonFallbackChain)
	}
	if in.HasRoundRobin {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonRoundRobinStrategy)
	}
	if in.HasRequestRetryOverride {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonRequestRetryOverride)
	}
	// A non-empty client override is only admissible if it is PROVABLY byte-identical
	// to the descriptor's default ClientConfig. 8B has no such proof, so any override
	// is a conservative client decline (scope §3 static-client gate).
	if in.ClientOverride != "" {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonClientOverride)
	}

	// --- Layer 3: descriptor envelope --------------------------------------
	fn := in.Descriptor
	if d := checkStaticEnvelope(fn, in.Method); d != nil {
		return *d
	}
	// Arg binder EXACTLY matches the descriptor's declared arguments (final only —
	// a parse has no arguments).
	if d := checkArgBinder(fn.Args, in.ArgOrder, in.Args); d != "" {
		return declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StageMethod, d)
	}

	// --- Layer 3b: Return Bundle lower / validate / native final support ----
	if _, d := checkStaticReturnBundle(fn); d != nil {
		return *d
	}

	// --- Layer 4: static prompt render support ------------------------------
	if serr := nativeprompt.SupportsStatic(fn, in.Args); serr != nil {
		var pd *nativeprompt.Decline
		if errors.As(serr, &pd) && pd.Feature == nativeprompt.FeatureStaticDescriptor {
			// A stricter envelope decline than the explicit checks above.
			return declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonStaticDescriptorStricter)
		}
		return declineStatic(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticPromptUnsupported)
	}
	rendered, rerr := nativeprompt.RenderStatic(fn, in.Args)
	if rerr != nil {
		// SupportsStatic shares the preparer, so a render error here is unexpected —
		// still a pre-socket decline (BAML serves), classified as a prompt decline.
		return declineStatic(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticRenderFailed)
	}

	// --- Layer 5: static client normalize + canonical body ------------------
	if in.Provider != "openai" {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonProviderNotOpenAI)
	}
	alias := staticAliasOr(in.Alias)
	intent, cerr := nativebody.NormalizeStaticClient(fn.ClientConfig, alias, false)
	if cerr != nil {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonStaticClientUnsupport)
	}
	baseURL, apiKey, terr := staticTransport(fn.ClientConfig)
	if terr != "" {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, terr)
	}
	native, borr := nativebody.BuildOpenAIChat(rendered, intent)
	if borr != nil {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StagePrompt, reasonCanonicalBodyUnsuppor)
	}

	// --- Layer 6: nanollm New/Prepare (NO SEND) -----------------------------
	if err := ctx.Err(); err != nil {
		return declineStatic(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
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
		return declineStatic(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNanollmNewFailed)
	}
	defer client.Close()

	prep, perr := client.Prepare(nanollm.Request{
		Model:  alias,
		Body:   native.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if perr != nil {
		return declineStatic(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNanollmPrepareFailed)
	}

	// --- Layer 7: strict BAML Request.<Method> no-send plan compare ---------
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
	if in.WouldRewriteOrProxy != nil && in.WouldRewriteOrProxy(prep.URL) {
		return declineStatic(bamlutils.NativeStaticFamilyClient, StageStrategy, ReasonURLRewriteOrProxy)
	}

	match, cmpReason := staticPlanMatches(bamlReq, prep)
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

	// Proven up to — but NOT including — the RoundTrip. 8B records the would-admit
	// and forces the decline; a later serving slice could CLAIM here.
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
