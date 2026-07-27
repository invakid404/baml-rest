package admission

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/schema"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// De-BAML Phase 3b — static STREAM serve admission (the streaming twin of the unary
// static AdmitStaticClaim in static.go).
//
// AdmitStaticStreamClaim runs the FULL no-send static predicate for one generated
// static `/stream{,-with-raw}` request and, on a full would-admit, returns a
// *StaticStreamClaim whose request-scoped nanollm engine is kept ALIVE so the serve
// core can drive DoStream on the SAME client Prepare produced the plan on. Every
// pre-claim decline guarantees NO provider socket occurred, so the caller runs BAML
// for the same request — the exact tri-state pre-claim boundary.
//
// It differs from the unary AdmitStaticClaim in exactly four ways: the mode gate
// admits the two STREAMING modes (not final), a stream-with-raw request is ALLOWED
// (unary call-with-raw declines), Prepare runs with nanollm Stream:true (so the engine
// injects BAML's stream suffix) while the request body is still the UNARY BuildOpenAIChat
// body from NormalizeStaticClient(..., false) — NormalizeStaticClient(..., true) is used
// ONLY to validate the BuildOpenAIChatStream parity anchor + resolve the target model —
// and the plan compare is against BAML's **StreamRequest** plan (in.BuildBAMLRequest
// returns the StreamRequest closure). Everything else — descriptor envelope, arg binder,
// Return-Bundle lower, the single-leaf/no-strategy/no-retry gates, the strict OpenAI
// byte-exact plan compare — is shared with the unary path.
//
// admittedStaticStreamReturnShape admits the PROVEN recursive-alias family (the exact
// five-arm JSON alias) and returns false for everything else; the gate is placed EARLY
// (right after the Return Bundle is lowered, BEFORE any render / client normalize / nanollm
// New / Prepare), so a request whose Return Bundle is NOT the admitted shape declines pre-claim
// having opened NO socket and done NO nanollm work — BAML serves it exactly as today. A request
// whose Return Bundle DOES match this shape is only ELIGIBLE to proceed: the render→Prepare→plan-
// compare tail below is REACHED, but it claims the JSON alias ONLY on a full would-admit —
// RenderStatic, the client/provider normalize, nanollm New/Prepare, and the strict StreamRequest
// plan compare can each still decline pre-claim.

// StaticStreamInput is the neutral static-stream admission input, the streaming twin of
// StaticInput. It carries the same descriptor/strategy/plan facts plus the streaming
// mode + NeedsRaw. No nanollm type crosses it.
type StaticStreamInput struct {
	// Build/flag/route (layer 1). Same neutral facts as StaticInput.
	WorkerCapable       bool
	RequestAPIPresent   bool
	OnBuildRequestRoute bool
	FlagEnabled         bool
	// RouteKind must be RouteKindStatic; AdmitStaticStreamClaim rejects anything else.
	RouteKind RouteKind

	// Method / Descriptor / Args / ArgOrder / Alias — identical to StaticInput.
	Method     string
	Descriptor promptdescriptor.Function
	Args       map[string]any
	ArgOrder   []string
	Alias      string
	// Mode is the streaming public mode (stream / stream_with_raw). The mode gate
	// admits exactly those two; a unary/unknown mode declines.
	Mode bamlutils.NativeStreamMode
	// NeedsRaw reports a /stream-with-raw request. Unlike the unary call-with-raw
	// envelope (which declines), stream-with-raw's raw/reasoning channels are owned by
	// the native-only parser in the orchestrator, so it is admitted at the transport
	// layer here.
	NeedsRaw bool

	// Whole orchestration plan + selected-child facts (layer 2).
	SingleLeaf              bool
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool
	ClientOverride          string
	Provider                string

	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's **StreamRequest** plan WITHOUT sending, for the
	// strict plan compare. A nil closure records a decline (no plan to compare).
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)
}

// StaticStreamClaim is the static-stream serve token AdmitStaticStreamClaim returns on a
// full would-admit. It is the fusion of StaticClaim (kept-alive engine + prepared plan +
// Return Bundle + exact-attempt request + alias) and StreamClaim (the nanollm.Request the
// executor hands DoStream). The serve core owns Close-ing it on EVERY path.
type StaticStreamClaim struct {
	client *nanollm.Client

	// Prepared is the nanollm prepared streaming plan (Stream:true).
	Prepared *nanollm.PreparedRequest
	// Bundle is the lowered, validated FINAL Return Bundle the native static-stream SAP
	// parses each partial + the final against (the streaming profile is derived
	// internally inside internal/debaml).
	Bundle *schema.Bundle
	// ExactRequest is the neutral exact-attempt stream carrier derived from Prepared —
	// the plan the 7A one-shot exact stream client compares DoStream's re-prepared
	// request against before the single RoundTrip.
	ExactRequest *llmhttp.ExactAttemptRequest
	// Alias is the fixed internal nanollm alias the plan was prepared under.
	Alias string
	// request is the OpenAI-format streaming nanollm.Request the executor hands DoStream.
	request nanollm.Request
}

// Client returns the request-scoped nanollm engine (nil after Close).
func (c *StaticStreamClaim) Client() *nanollm.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Request returns the streaming nanollm.Request the executor hands DoStream.
func (c *StaticStreamClaim) Request() nanollm.Request {
	if c == nil {
		return nanollm.Request{}
	}
	return c.request
}

// PlanExpired reports whether the prepared plan's signature window passed before the
// socket. OpenAI plans are unsigned and never expire; this guards the seam for the
// signed-plan providers a later phase adds, mirroring StaticClaim / StreamClaim.
func (c *StaticStreamClaim) PlanExpired() bool {
	if c == nil || c.Prepared == nil {
		return false
	}
	return c.Prepared.Expired()
}

// Close releases the request-scoped nanollm engine. Idempotent and nil-safe.
func (c *StaticStreamClaim) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
	c.client = nil
}

// admittedStaticStreamReturnShape reports whether the lowered Return Bundle is inside the
// admitted static-STREAM return-shape set. It is the STREAM twin of
// admittedStaticReturnShape and delegates to debaml.IsProvenRecursiveAliasStaticStreamFamily
// so the isolated serve gate and the root-owned static-stream parser profile stay in EXACT
// lockstep. It DELIBERATELY does NOT reuse admittedStaticReturnShape (a final decoder gate
// that admits unrelated final-only shapes): stream admission is NOT inherited from final.
//
// True ONLY for the STREAM-served recursive-alias family (the exact five-arm `JSON`
// alias); false for every other bundle, which declines pre-claim and is served by BAML.
//
// It is deliberately NARROWER than admittedStaticReturnShape, which also admits the
// nullable six-stored-variant `JsonValue` family for the FINAL lane. The stream gate
// admits by descriptor SHAPE pre-socket and a claimed stream has no route back to BAML,
// so a family whose parse can decline on a VALUE must not claim a stream socket; the
// unary lane repairs the same response through BAML parse-only, so it can. The root
// package owns that distinction (internal/debaml/static_stream_serve.go); this gate just
// asks the STREAM predicate, never the wider FINAL one.
func admittedStaticStreamReturnShape(b *schema.Bundle) bool {
	return debaml.IsProvenRecursiveAliasStaticStreamFamily(b)
}

// AdmitStaticStreamClaim runs the static-stream no-send admission predicate and returns a
// live *StaticStreamClaim on a full would-admit, else a *StaticDecline guaranteeing no
// socket occurred. The caller MUST Close the returned claim on every path.
func AdmitStaticStreamClaim(ctx context.Context, in StaticStreamInput) (*StaticStreamClaim, error) {
	// --- Layers 1-3b: build/flag/route, mode, strategy, descriptor, Return bundle ---
	bundle, fn, dec := admitStaticStreamThroughBundle(ctx, in)
	if dec != nil {
		return nil, staticDeclineFromObs(*dec)
	}

	// --- Return-shape gate (EARLY): the SERVE-only static-stream shape gate. Placed
	// before any render / client normalize / nanollm New / Prepare so a stream OUTSIDE the
	// STREAM-admitted five-arm JSON alias family declines here doing ZERO nanollm work and
	// opening NO socket. ---
	if !admittedStaticStreamReturnShape(bundle) {
		return nil, staticDeclineFromObs(declineStatic(bamlutils.NativeStaticFamilyDescriptorEnvelope, StagePrompt, reasonReturnShapeUnproven))
	}

	// --- Layers 4-6: render + streaming client normalize + streaming body + Prepare ---
	prep, req, pdec := admitStaticStreamPrepare(ctx, in, fn, bundle)
	if pdec != nil {
		return nil, staticDeclineFromObs(*pdec)
	}

	// --- Layer 7: strict BAML StreamRequest plan compare -----------------------------
	obs := staticPlanCompareObservation(ctx, StaticInput{
		BuildBAMLRequest:    in.BuildBAMLRequest,
		WouldRewriteOrProxy: in.WouldRewriteOrProxy,
	}, prep)
	if obs.Observation != bamlutils.NativeStaticObserveWouldAdmit {
		prep.close()
		return nil, staticDeclineFromObs(obs)
	}

	// Would-admit: transfer ownership of the kept-alive engine to the claim.
	return &StaticStreamClaim{
		client:       prep.client,
		Prepared:     prep.prepared,
		Bundle:       prep.bundle,
		ExactRequest: prep.exactRequest,
		Alias:        prep.alias,
		request:      req,
	}, nil
}

// admitStaticStreamThroughBundle runs the static-stream layers 1-3b (build/flag/route,
// the STREAM mode gate, the orchestration-plan gate, descriptor envelope + arg binder,
// and Return-Bundle lower/support) and returns the lowered Return Bundle + descriptor on
// success, or a bounded decline. It opens NO socket and does NO nanollm work.
func admitStaticStreamThroughBundle(ctx context.Context, in StaticStreamInput) (*schema.Bundle, promptdescriptor.Function, *StaticObservation) {
	decline := func(family bamlutils.NativeStaticObserveFamily, stage Stage, reason Reason) (*schema.Bundle, promptdescriptor.Function, *StaticObservation) {
		o := declineStatic(family, stage, reason)
		return nil, promptdescriptor.Function{}, &o
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

	// --- Mode gate: admit exactly the two STREAMING modes -------------------
	if in.Mode != bamlutils.NativeStreamModeStream && in.Mode != bamlutils.NativeStreamModeStreamWithRaw {
		return decline(bamlutils.NativeStaticFamilyClient, StageMode, reasonModeUnsupported)
	}

	// --- Layer 2: whole orchestration plan + selected-child facts -----------
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
	if in.ClientOverride != "" && in.ClientOverride != in.Descriptor.Client {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonClientOverride)
	}

	// --- Layer 3: descriptor envelope + arg binder --------------------------
	fn := in.Descriptor
	if d := checkStaticEnvelope(fn, in.Method); d != nil {
		return nil, promptdescriptor.Function{}, d
	}
	if d := checkArgBinder(fn.Args, in.ArgOrder, in.Args); d != "" {
		return decline(bamlutils.NativeStaticFamilyDescriptorEnvelope, StageMethod, d)
	}

	// --- Layer 3b: Return Bundle lower / validate / native final support ----
	bundle, d := checkStaticReturnBundle(fn)
	if d != nil {
		return nil, promptdescriptor.Function{}, d
	}
	return bundle, fn, nil
}

// admitStaticStreamPrepare runs the static-stream layers 4-6 (static prompt render, the
// STREAMING client normalize + canonical body, and nanollm New/Prepare with Stream:true).
// It returns a kept-alive *staticPrepared + the streaming nanollm.Request the executor
// hands DoStream, or a bounded decline (leaving NO engine open). It opens NO socket.
//
// REACHED for an admitted stream (the early return-shape gate lets the proven recursive-alias
// family through to here); a non-admitted bundle never reaches this because it declines at
// that earlier gate first.
func admitStaticStreamPrepare(ctx context.Context, in StaticStreamInput, fn promptdescriptor.Function, bundle *schema.Bundle) (*staticPrepared, nanollm.Request, *StaticObservation) {
	decline := func(family bamlutils.NativeStaticObserveFamily, stage Stage, reason Reason) (*staticPrepared, nanollm.Request, *StaticObservation) {
		o := declineStatic(family, stage, reason)
		return nil, nanollm.Request{}, &o
	}

	// --- Layer 4: static prompt render support ------------------------------
	if serr := nativeprompt.SupportsStatic(fn, in.Args); serr != nil {
		return decline(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticPromptUnsupported)
	}
	rendered, rerr := nativeprompt.RenderStatic(fn, in.Args)
	if rerr != nil {
		return decline(bamlutils.NativeStaticFamilyPrompt, StagePrompt, reasonStaticRenderFailed)
	}

	// --- Layer 5: streaming client normalize + canonical body ---------------
	if in.Provider != "openai" {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonProviderNotOpenAI)
	}
	alias := staticAliasOr(in.Alias)
	// The STREAM intent (Stream:true) proves BuildOpenAIChatStream reproduces BAML's
	// StreamRequest bytes; the anchor is the runtime parity target. The request body is
	// the UNARY body (BuildOpenAIChat) + nanollm Stream:true, so the engine injects
	// BAML's `,"stream":true,"stream_options":{"include_usage":true}` suffix — exactly as
	// the dynamic stream lane assembles it (admit.go).
	streamIntent, cerr := nativebody.NormalizeStaticClient(fn.ClientConfig, alias, true)
	if cerr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonStaticClientUnsupport)
	}
	unaryIntent, ucerr := nativebody.NormalizeStaticClient(fn.ClientConfig, alias, false)
	if ucerr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, reasonStaticClientUnsupport)
	}
	baseURL, apiKey, terr := staticTransport(fn.ClientConfig)
	if terr != "" {
		return decline(bamlutils.NativeStaticFamilyClient, StageStrategy, terr)
	}
	// Prove the stream canonical body is a supported shape (the parity anchor); the
	// engine-injected suffix is validated against it in a later slice's plan oracle.
	if _, sberr := nativebody.BuildOpenAIChatStream(rendered, streamIntent); sberr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StagePrompt, reasonCanonicalBodyUnsuppor)
	}
	unaryBody, borr := nativebody.BuildOpenAIChat(rendered, unaryIntent)
	if borr != nil {
		return decline(bamlutils.NativeStaticFamilyClient, StagePrompt, reasonCanonicalBodyUnsuppor)
	}

	// --- Layer 6: nanollm New/Prepare (NO SEND, Stream:true) ----------------
	if err := ctx.Err(); err != nil {
		return decline(bamlutils.NativeStaticFamilyCapability, StageContext, ReasonContextCancelled)
	}
	client, nerr := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       alias,
			Model:      "openai/" + streamIntent.TargetModel,
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
	req := nanollm.Request{
		Model:  alias,
		Body:   unaryBody.Bytes(),
		Type:   nanollm.ChatCompletion,
		Stream: true,
	}
	prep, perr := client.Prepare(req)
	if perr != nil {
		client.Close()
		return decline(bamlutils.NativeStaticFamilyPrepare, StagePrepare, reasonNanollmPrepareFailed)
	}
	return &staticPrepared{
		client:       client,
		prepared:     prep,
		bundle:       bundle,
		exactRequest: exactRequestFromPlan(prep),
		alias:        alias,
	}, req, nil
}
