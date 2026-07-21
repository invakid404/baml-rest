package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	"github.com/invakid404/baml-rest/internal/schema"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// dynamicMethod is the ONE internal method the native unary surface admits.
const dynamicMethod = "Baml_Rest_Dynamic"

// chatCompletionsPath is the fixed path the admitted OpenAI chat surface appends
// to the client base_url to form the effective request target. It is the target
// the prepared-plan URL is checked against and the target the send-path
// rewrite/proxy parity is evaluated against.
const chatCompletionsPath = "/chat/completions"

// Input is the whole set of request-wide + payload facts the native unary
// callback would receive from the orchestrator. In this cutover the callback is
// nil/hard-off and Input is supplied by the gated tests; nothing in production
// routing constructs it. Every field is a neutral fact — no nanollm type crosses
// this boundary.
type Input struct {
	// Build/flag/route (layer 1). WorkerCapable/RequestAPIPresent/OnBuildRequestRoute
	// are the native-worker capability, the preferred non-streaming Request API,
	// and this child being on the BuildRequest (not legacy) route. FlagEnabled is
	// BAML_REST_USE_DEBAML resolved enabled. Method is the internal method name;
	// Mode is the request mode.
	WorkerCapable       bool
	RequestAPIPresent   bool
	OnBuildRequestRoute bool
	FlagEnabled         bool
	// RouteKind is the closed dynamic|static distinction (Slice 8B). The zero value
	// is RouteKindDynamic, so an existing dynamic caller that leaves it unset admits
	// through admitCore exactly as before; a static invocation uses AdmitStatic and
	// never reaches this struct.
	RouteKind RouteKind
	Method    string
	Mode      Mode

	// Whole orchestration plan (layer 2). SingleLeaf is "exactly one resolved
	// client/leaf"; the Has* flags mark a fallback chain, round-robin strategy,
	// legacy child, or request-retry override the exact lane would bypass.
	// ResolvedProvider is the orchestrator-resolved leaf provider.
	SingleLeaf              bool
	HasFallbackChain        bool
	HasRoundRobin           bool
	IsLegacyChild           bool
	HasRequestRetryOverride bool
	ResolvedProvider        string

	// WouldRewriteOrProxy, when non-nil, reports whether the effective send client
	// would rewrite the outbound URL or route the EFFECTIVE TARGET through an HTTP
	// proxy — the send-path transforms the exact lane would bypass. It is evaluated
	// against the target Admit itself resolves (base_url + /chat/completions), AFTER
	// the effective client is mapped, so the proxy decision is the transport's own
	// resolver against the real target rather than an env re-read; a true result
	// declines at the strategy stage before the native plan is prepared, BAML's plan
	// is obtained, or a plan_compare is recorded. Nil disables the check (lightweight
	// tests that carry no send client).
	WouldRewriteOrProxy func(effectiveURL string) bool

	// Effective dynamic client + payload (layers 3-4). Registry is the effective
	// one-client registry; Alias is the SEPARATE internal nanollm alias (never the
	// target); Messages are the generated dynamic messages; OutputSchema is the
	// dynamic output schema.
	Registry     *bamlutils.ClientRegistry
	Alias        string
	Messages     []bamlutils.DynamicMessage
	OutputSchema *bamlutils.DynamicOutputSchema
}

// Admitted is a native plan proven up to — but NOT including — the exact
// RoundTrip. It carries the prepared plan, the neutral exact-attempt request
// built (and header-preflighted) but never sent, and the alias/target/provider.
// The nanollm engine that produced the plan has already been Closed inside Admit
// (the no-send path never needs it to outlive the call), so an Admitted holds no
// engine and no live client.
//
// SENSITIVE — treat an Admitted like the API key it embeds. Both Prepared
// (Headers) and ExactRequest (Headers/Body) retain the real bearer Authorization
// value and the exact request body. An Admitted (or its Prepared/ExactRequest)
// MUST NEVER be logged, serialized, marshaled, or otherwise emitted; surface only
// the redacted, secret-free views (llmhttp's RedactedSummary/RedactedURL, header
// NAMES, body length/hash) instead. Alias/Target/Provider are secret-free.
type Admitted struct {
	Prepared     *nanollm.PreparedRequest
	ExactRequest *llmhttp.ExactAttemptRequest
	Alias        string
	Target       string
	Provider     string
	// Verification is the post-Prepare verification policy the mapper assigned
	// (§6): PolicyStrictOpenAI runs the byte-exact OpenAI oracles + BAML plan/
	// response comparisons; PolicyTrustedProvider runs only provider-neutral
	// self-consistency and NEVER a BAML comparison. The serve path branches on it
	// once. In S1 only strict OpenAI is ever produced in production (every
	// non-openai provider mapping-declines before nanollm.New).
	Verification VerificationPolicy
}

// Admitter evaluates the native admission predicate and records the bounded
// de-BAML metrics. Construct one with NewAdmitter; a nil *Metrics disables
// recording (valid for lightweight tests).
//
// It holds the exact-transport executor it would send an admitted plan through.
// In this cutover the executor is used ONLY for the no-send Preflight at the
// exact-transport boundary — Admit never calls Execute — so it opens no socket;
// holding the very executor a later send would use makes a stray RoundTrip
// observable on that executor's transport (the zero-socket tests inject a
// counting transport and assert it stays at zero).
type Admitter struct {
	m    *Metrics
	exec *llmhttp.ExactExecutor
}

// NewAdmitter returns an Admitter recording on m and preflighting through exec.
// m may be nil (recording disabled). A nil exec defaults to a hardened
// single-attempt exact executor; tests pass an executor over a counting/poison
// transport to observe that Admit never dials.
func NewAdmitter(m *Metrics, exec *llmhttp.ExactExecutor) *Admitter {
	if exec == nil {
		exec = llmhttp.NewExactExecutor(nil)
	}
	return &Admitter{m: m, exec: exec}
}

// Claim is an Admitted native plan whose request-scoped nanollm client is kept
// ALIVE so a serving path can call TranslateResponse on the SAME client that
// Prepare produced the plan on — avoiding a second nanollm.New/Close cycle per
// served request. The caller OWNS the client's lifecycle and MUST Close the Claim
// on EVERY return path (success, error, panic, cancel). The no-send shadow path
// uses Admit (which closes immediately); only the serve path retains a Claim.
//
// SENSITIVE — a Claim embeds an Admitted (see its secret contract) plus a live
// engine; it MUST NEVER be logged, serialized, or emitted.
type Claim struct {
	Admitted
	client *nanollm.Client
}

// Client returns the request-scoped nanollm engine the admitted plan was Prepared
// on, still open so the serve path can TranslateResponse on it.
func (c *Claim) Client() *nanollm.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// PlanExpired reports whether the admitted prepared plan's signature window has
// passed. Admission already proved the plan non-expiring at claim time
// (validatePlanExpiry), but the serve path re-checks it immediately before the
// socket so that a plan which expired DURING the (BAML-plan-build + compare)
// window is caught as a provably PRE-SOCKET condition and declined to BAML rather
// than claimed. Always false for the admitted never-expiring OpenAI surface;
// guards the seam for the signed-plan providers a later phase adds.
func (c *Claim) PlanExpired() bool {
	if c == nil || c.Prepared == nil {
		return false
	}
	return c.Prepared.Expired()
}

// Close releases the request-scoped nanollm engine. Idempotent-safe against a nil
// receiver / nil client; the caller must call it on every path.
func (c *Claim) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
	c.client = nil
}

// Admit runs the FULL pre/post-Prepare no-send native admission predicate for
// one unary OpenAI `_dynamic` call and returns the admitted plan with its
// request-scoped engine ALREADY Closed (the no-send shadow path never needs the
// engine to outlive the call). It returns:
//
//   - (*Admitted, nil) when every layer is proven up to the exact RoundTrip — the
//     plan is READY to send but is deliberately NOT sent (no serving change);
//   - (nil, *Decline) — unwrapping to ErrDeclined — for a stable, secret-free
//     parity-decline to BAML;
//   - (nil, non-decline error) for an unexpected native planner/FFI error before
//     any socket (counted as OutcomePlannerError so it can alert, not read as
//     ordinary unsupported traffic).
//
// It opens ZERO sockets and performs ZERO RoundTrips on every path.
func (a *Admitter) Admit(ctx context.Context, in Input) (*Admitted, error) {
	// The no-send path records the terminal OutcomeAdmitted and closes the engine
	// immediately — it is the whole disposition for shadow.
	claim, err := a.admitClaim(ctx, in, true)
	if err != nil {
		return nil, err
	}
	defer claim.Close()
	adm := claim.Admitted
	return &adm, nil
}

// AdmitClaim runs the SAME full no-send admission predicate as Admit but returns
// a Claim whose request-scoped nanollm engine is kept ALIVE, so the serve path
// can call TranslateResponse on the identical client Prepare ran on. The caller
// MUST Close the returned Claim on every path. It does NOT record the terminal
// OutcomeAdmitted — the serve path records exactly one terminal serving outcome
// (success/transport_error/…) instead, so admission is never double-counted as a
// served success. Declines / planner errors are recorded exactly as Admit does.
//
// It opens ZERO sockets and performs ZERO RoundTrips on every path.
func (a *Admitter) AdmitClaim(ctx context.Context, in Input) (*Claim, error) {
	return a.admitClaim(ctx, in, false)
}

// admitCore is the shared admission core for BOTH the unary and streaming lanes.
// recordAdmitted controls whether the terminal OutcomeAdmitted is recorded (true
// for the no-send Admit path, false for the serving AdmitClaim / AdmitStreamClaim
// paths). stream selects the streaming variant at exactly the four points the two
// lanes diverge — the admitted mode, the canonical body builder, the prepared
// Request's Stream flag, and the plan-meta validator (stream=true wants an SSE
// response format, stream=false a JSON one); every other layer is identical, so
// the unary path (stream=false) is byte-for-byte the pre-7B behavior.
//
// On success it returns the proven plan (Admitted), the OPEN request-scoped
// engine (ownership passes to the caller, which wraps it in a *Claim /
// *StreamClaim and MUST Close it), and the nanollm streaming Request the stream
// executor hands DoStream (unused on the unary path). On every decline/error/
// panic AFTER the engine is created it Closes the engine before returning (via
// the closeClient guard) and returns a nil client.
func (a *Admitter) admitCore(ctx context.Context, in Input, recordAdmitted, stream bool) (Admitted, *nanollm.Client, nanollm.Request, error) {
	provider := providerFromResolved(in.ResolvedProvider)

	decline := func(d *Decline) (Admitted, *nanollm.Client, nanollm.Request, error) {
		a.m.recordDecline(in.Mode, provider, d)
		return Admitted{}, nil, nanollm.Request{}, d
	}
	plannerErr := func(err error) (Admitted, *nanollm.Client, nanollm.Request, error) {
		a.m.recordAttempt(in.Mode, provider, OutcomePlannerError)
		return Admitted{}, nil, nanollm.Request{}, err
	}
	// ctxDecline is the PRE-SOCKET decline for a request cancelled/expired around a
	// non-context FFI boundary (New / render / Prepare). It declines to BAML — the
	// ordinary BAML attempt then surfaces the same context error to the caller —
	// rather than counting a planner error or opening a socket.
	ctxDecline := func() (Admitted, *nanollm.Client, nanollm.Request, error) {
		return decline(declinef(StageContext, ReasonContextCancelled, "request context cancelled during admission"))
	}

	// --- Layer 1: build / flag / route ------------------------------------
	if !in.WorkerCapable {
		return decline(declinef(StageCapability, ReasonWorkerNotCapable, "native worker capability is absent"))
	}
	if !in.RequestAPIPresent {
		return decline(declinef(StageCapability, ReasonRequestAPIAbsent, "the preferred non-streaming Request API is absent"))
	}
	if !in.OnBuildRequestRoute {
		return decline(declinef(StageCapability, ReasonNotBuildReqRoute, "child is not on the BuildRequest route"))
	}
	if !in.FlagEnabled {
		return decline(declinef(StageFlag, ReasonFlagDisabled, "BAML_REST_USE_DEBAML is not resolved enabled"))
	}
	// Closed ROUTE-KIND distinction (Slice 8B): admitCore admits ONLY the dynamic
	// route. A static invocation flows through AdmitStatic (static.go) instead; the
	// zero-value RouteKind is RouteKindDynamic, so every existing dynamic caller
	// (which leaves the field unset) keeps its exact behaviour. This replaces the
	// previous implicit "the one internal method is Baml_Rest_Dynamic" gate with an
	// explicit route kind so the two predicates never cross.
	if in.RouteKind != RouteKindDynamic {
		return decline(declinef(StageMethod, ReasonNotDynamicMethod, "dynamic admission requires the dynamic route kind"))
	}
	if in.Method != dynamicMethod {
		return decline(declinef(StageMethod, ReasonNotDynamicMethod, "internal method is not Baml_Rest_Dynamic"))
	}
	if stream {
		if d := admitStreamMode(in.Mode); d != nil {
			return decline(d)
		}
	} else {
		if d := admitMode(in.Mode); d != nil {
			return decline(d)
		}
	}
	// Output schema present (layer 1). The bounds check follows once the message
	// surface is validated, but an absent schema declines up front.
	if in.OutputSchema == nil {
		return decline(declinef(StagePrompt, ReasonOutputSchemaAbsent, "no output schema supplied"))
	}

	// --- Layer 2: whole orchestration plan --------------------------------
	if d := admitStrategy(in); d != nil {
		return decline(d)
	}
	// The resolved-provider openai gate is REMOVED (§7): the provider-neutral
	// foundation resolves + maps the provider inside mapClientConfig, which
	// declines a non-openai provider with mapping_unavailable BEFORE nanollm.New
	// (S1 admits no non-openai socket) and enforces §4.2 provider provenance.

	// --- Layer 3: effective dynamic client (+ request-scoped engine) ------
	// mapDynamicClient resolves the effective target (base_url + /chat/completions)
	// and evaluates in.WouldRewriteOrProxy against it BEFORE constructing the
	// request-scoped engine — a rewrite or a proxied effective target declines at
	// the strategy stage there, before the engine, the plan, BAML's plan, or a
	// plan_compare.
	// Cancellation gate BEFORE nanollm.New (mapDynamicClient constructs the engine).
	if err := ctx.Err(); err != nil {
		return ctxDecline()
	}
	client, facts, policy, dec, err := mapDynamicClient(ctx, in.Registry, in.Alias, in.ResolvedProvider, in.WouldRewriteOrProxy)
	// Cancellation gate IMMEDIATELY AFTER mapping: the mapping may perform bounded
	// credential I/O (the Bedrock resolver), and a cancellation during it surfaces
	// as an ordinary credential decline / planner error from the resolver rather than
	// the established pre-socket context_cancelled path. Re-check the ORIGINAL request
	// context (not the resolver's bounded derivation) first so a caller cancellation
	// classifies as context_cancelled. A successfully-constructed engine is closed
	// here (the closeClient defer is not yet armed); an err/decline path returns a nil
	// client, so this never double-closes.
	if ctx.Err() != nil {
		if client != nil {
			client.Close()
		}
		return ctxDecline()
	}
	if err != nil {
		return plannerErr(err)
	}
	if dec != nil {
		return decline(dec)
	}
	// Request-scoped engine: closed on EVERY decline/error/panic path below via the
	// closeClient guard; on SUCCESS ownership passes to the returned Claim (the
	// serve path Closes it after TranslateResponse; the no-send Admit path Closes
	// it immediately). A defer (rather than an inline close before each return)
	// preserves the "close on panic/cancel" contract.
	closeClient := true
	defer func() {
		if closeClient {
			client.Close()
		}
	}()
	// Bounded observability: a successfully-mapped aws-bedrock admission records
	// WHICH documented credential source it resolved through (§9) — a secret-free
	// enum only. Recorded once here (creds resolved, engine constructed) even if a
	// later stage declines, so credential-path rollout stays visible.
	if facts.bedrockSource != "" {
		a.m.recordBedrockCredentialSource(facts.bedrockSource)
	}
	// Streaming stays out of scope for a TRUSTED provider in this slice: the S2
	// mappers activate the unary surface only, so a trusted provider that reached
	// the streaming lane declines (the strict OpenAI stream anchor is unchanged).
	if stream && policy != PolicyStrictOpenAI {
		return decline(declinef(StageMode, ReasonStreamingUnproven,
			"streaming is out of scope for a trusted-provider native attempt in this slice"))
	}

	// Cancellation gate AFTER nanollm.New (the engine is now open; the closeClient
	// guard above closes it) and BEFORE render.
	if err := ctx.Err(); err != nil {
		return ctxDecline()
	}

	// --- Layer 4: prompt + canonical body ---------------------------------
	if d := validateMessages(in.Messages); d != nil {
		return decline(d)
	}
	if _, serr := schema.FromDynamicOutputSchema(in.OutputSchema, schema.BuildOptions{}); serr != nil {
		return decline(declinef(StagePrompt, ReasonOutputSchemaUnbounded, "output schema is outside the native schema/SAP bounds"))
	}
	if stream {
		// Phase 7C native-stream SAP schema row (§5.5). The claimed native stream
		// lane owns EVERY partial and the final for this schema with NO per-prefix
		// BAML fallback (I6), so decline pre-transport (I2) any schema whose
		// native-only stream/final parser cannot own the whole type graph.
		// SupportsNativeStream subsumes the final-support gate — both run the shared
		// checkSupported cut-line; the stream preflight additionally rejects stream
		// annotations — so it is the single schema-support row for the stream claim.
		// Zero sockets, zero FFI: a pure schema-graph walk. The unary lane
		// (stream=false) skips this block and stays byte-for-byte unchanged.
		if serr := debaml.SupportsNativeStream(in.OutputSchema); serr != nil {
			return decline(declinef(StagePrompt, ReasonStreamSchemaUnsupported, "output schema is outside the native stream SAP bounds"))
		}
	}

	rendered, rerr := nativeprompt.Render(toNativeMessages(in.Messages), in.OutputSchema)
	if rerr != nil {
		var pd *nativeprompt.Decline
		if errors.As(rerr, &pd) {
			return decline(classifyPromptDecline(pd.Feature))
		}
		// A non-decline render error on the proved template is unexpected.
		return plannerErr(rerr)
	}

	// --- Layer 4-5: canonical body + prepared plan — FORK on verification policy.
	// The STRICT OpenAI anchor is UNCHANGED: it builds the byte-exact canonical body
	// (BuildOpenAIChat[Stream]) as the runtime parity anchor and re-asserts it
	// byte-for-byte on the prepared plan. A TRUSTED provider builds the SAME
	// OpenAI-shaped ChatRequest PLUS the supported mapped body-field subset, but
	// claims NO byte-exact anchor — nanollm owns the transformed provider body /
	// signed plan / member order (§6), so there is nothing to byte-compare.
	var canonicalBytes []byte // strict anchor bytes; nil for a trusted provider
	var nreq nanollm.Request

	if policy == PolicyStrictOpenAI {
		intent := nativebody.ClientIntent{
			Provider:    facts.provider,
			TargetModel: facts.target,
			ModelAlias:  in.Alias,
			Stream:      stream,
		}
		var canonical *nativebody.CanonicalBody
		var berr error
		if stream {
			canonical, berr = nativebody.BuildOpenAIChatStream(rendered, intent)
		} else {
			canonical, berr = nativebody.BuildOpenAIChat(rendered, intent)
		}
		if berr != nil {
			var bd *nativebody.Decline
			if errors.As(berr, &bd) {
				return decline(classifyBodyDecline(bd.Feature))
			}
			return plannerErr(berr)
		}
		canonicalBytes = canonical.Bytes()
		if d := validateCanonicalBody(canonicalBytes, facts.target); d != nil {
			return decline(d)
		}

		// Cancellation gate AFTER render / BEFORE the Prepare FFI.
		if err := ctx.Err(); err != nil {
			return ctxDecline()
		}

		// Assemble the request through nanollm v0.4.x's typed ChatRequest.Build seam
		// (the FFI package's canonical OpenAI request type). The typed model is mapped
		// from the admitted rendered prompt; the body is serialized by the SHIPPED
		// canonicalSonicMarshaler (configured sonic + the backslash-parity-aware short-
		// escape fixup), which the gated canonreq oracle proves byte-exact vs the
		// zero-nanollm root writer on every admitted input. The root writer's bytes
		// (canonicalBytes) stay the runtime parity anchor via validatePreparedBody
		// below, so any mismatch fails closed to BAML rather than emitting a non-parity
		// body. Build copies the target model into Request.Model — override it with the
		// separate nanollm alias, exactly as the previous hand-built nanollm.Request did.
		var brerr error
		nreq, brerr = chatRequestFromRendered(rendered, facts.target).Build(canonicalSonicMarshaler)
		if brerr != nil {
			// An unexpected serializer error on the SHIPPED canonical marshaler (§5.1):
			// availability-first BAML fallback with no socket, but recorded as a planner
			// error (not an ordinary unsupported decline) so it alerts rather than reading
			// as expected unsupported traffic.
			return plannerErr(fmt.Errorf("nativeserve/admission: nanollm ChatRequest.Build: %w", brerr))
		}
		nreq.Model = in.Alias
		// Stream is set on the nanollm Request (NOT baked into the body): the engine
		// injects BAML's `"stream":true,"stream_options":{"include_usage":true}` suffix
		// into the prepared body when stream is true, which validatePreparedBody then
		// checks byte-for-byte against the canonical stream oracle. On the unary path
		// this is the zero value (false), so the prepared body stays the unary body.
		nreq.Stream = stream
	} else {
		// TRUSTED provider (unary only in this slice): the OpenAI-shaped ChatRequest
		// from the admitted rendered prompt PLUS the supported mapped body-field
		// subset (temperature/max_tokens/… and the anthropic/bedrock extensions).
		// Bedrock's inference_configuration is projected onto the SAME body fields; its
		// AWS creds / additional_model_request_fields live on the engine config, not
		// here. There is no byte-exact anchor — validateGenericPlan proves the plan.
		chatReq := chatRequestFromRendered(rendered, facts.target)
		applyBodyOptions(&chatReq, facts.body)

		// Cancellation gate AFTER render / BEFORE the Prepare FFI.
		if err := ctx.Err(); err != nil {
			return ctxDecline()
		}

		var brerr error
		nreq, brerr = chatReq.Build(canonicalSonicMarshaler)
		if brerr != nil {
			// A ChatRequest.Build error (e.g. an Extra key colliding with a typed field
			// — a fail-closed guard) is a planner error: safe BAML fallback that alerts.
			return plannerErr(fmt.Errorf("nativeserve/admission: nanollm ChatRequest.Build: %w", brerr))
		}
		nreq.Model = in.Alias
		// Sanity gate on the pre-Prepare body: present, valid JSON, carrying the
		// literal target model (the alias override touches Request.Model, not the body).
		if d := validateCanonicalBody(nreq.Body, facts.target); d != nil {
			return decline(d)
		}
	}

	prep, perr := client.Prepare(nreq)
	// Cancellation gate AFTER the Prepare FFI, before the (fast, local) plan
	// validations that finish admission.
	if err := ctx.Err(); err != nil {
		return ctxDecline()
	}
	if perr != nil {
		// Typed New/Prepare classifier (§5.1), splitting today's too-broad
		// prepare_error: a nanollm *Error whose Code is unsupported_request /
		// invalid_provider is an ORDINARY pre-socket unsupported decline to BAML; any
		// OTHER Prepare failure is a planner error (safe BAML fallback that alerts),
		// never counted as ordinary unsupported traffic. Keyed on the typed CODE via
		// errors.As, never a string match.
		if classifyEngineError(perr) == engineUnsupported {
			return decline(declinef(StagePrepare, prepareUnsupportedReason(perr),
				"nanollm Prepare reported a typed unsupported request"))
		}
		return plannerErr(fmt.Errorf("nativeserve/admission: nanollm Prepare: %w", perr))
	}
	// --- Layer 5: prepared plan, revalidated immediately after Prepare --------
	// The exact-attempt carrier is a direct field-for-field projection of the
	// SAME prepared plan, built once and header-preflighted for BOTH policies (the
	// transport admissibility scan is provider-neutral). The plan validation then
	// forks on the verification policy: the STRICT OpenAI anchor runs every
	// existing byte-exact oracle in its existing order (UNCHANGED — §6), while the
	// TRUSTED provider runs only the provider-neutral post-Prepare self-consistency
	// gate (§5.2) — nanollm owns the transformed body / signed plan / provider
	// endpoint, so no BAML byte/URL/header parity is claimed for it.
	exactReq := exactRequestFromPlan(prep)
	if policy == PolicyStrictOpenAI {
		if d := validatePreparedBody(prep, canonicalBytes); d != nil {
			return decline(d)
		}
		// 7B streaming lane: a stream plan validates its SSE plan-meta, the unary
		// plan its JSON plan-meta. In S1 only the strict-OpenAI anchor reaches the
		// streaming lane (every non-openai provider mapping-declines before nanollm.New).
		if stream {
			if d := validateStreamPlanMeta(prep, in.Alias, facts.target); d != nil {
				return decline(d)
			}
		} else {
			if d := validatePlanMeta(prep, in.Alias, facts.target); d != nil {
				return decline(d)
			}
		}
		if d := validatePlanExpiry(prep); d != nil {
			return decline(d)
		}
		if d := validatePlanHeaders(prep, facts.baseURL); d != nil {
			return decline(d)
		}
	} else {
		if d := validateGenericPlan(prep, in.Alias, facts.target, facts.provider); d != nil {
			return decline(d)
		}
		// Trusted-provider rewrite/proxy parity (§5.2): the effective provider URL is
		// only known AFTER Prepare (nanollm owns the endpoint), so the send-path
		// rewrite/proxy check moves here — against the prepared URL — rather than into
		// the mapper. A rewrite or a proxied target declines pre-claim; a nil predicate
		// (lightweight tests) skips it.
		if in.WouldRewriteOrProxy != nil && in.WouldRewriteOrProxy(prep.URL) {
			return decline(declinef(StageStrategy, ReasonURLRewriteOrProxy,
				"the effective send path would rewrite or proxy the prepared provider URL"))
		}
	}
	if d := validateExactTransport(a.exec, exactReq); d != nil {
		return decline(d)
	}

	// Proven up to — but NOT including — the RoundTrip. Do NOT send here.
	if recordAdmitted {
		a.m.recordAttempt(in.Mode, provider, OutcomeAdmitted)
	}
	// Hand engine ownership to the caller (the wrapping *Claim / *StreamClaim): the
	// deferred guard no longer closes it.
	closeClient = false
	return Admitted{
		Prepared:     prep,
		ExactRequest: exactReq,
		Alias:        in.Alias,
		Target:       facts.target,
		Provider:     facts.provider,
		Verification: policy,
	}, client, nreq, nil
}

// admitClaim wraps admitCore for the UNARY lane, folding the core's (Admitted,
// engine, streaming-Request) tuple into a *Claim. The streaming Request the core
// also returns is unused on the unary path. Behavior is identical to the pre-7B
// admitClaim: admitCore with stream=false runs the exact prior sequence.
func (a *Admitter) admitClaim(ctx context.Context, in Input, recordAdmitted bool) (*Claim, error) {
	adm, client, _, err := a.admitCore(ctx, in, recordAdmitted, false)
	if err != nil {
		return nil, err
	}
	return &Claim{Admitted: adm, client: client}, nil
}

// admitMode declines every mode outside the admitted unary `call`.
func admitMode(mode Mode) *Decline {
	switch mode {
	case ModeCall:
		return nil
	case ModeCallWithRaw:
		return declinef(StageMode, ReasonWithRawUnproven, "call-with-raw is not proven for the native unary attempt")
	case ModeStream, ModeStreamWithRaw:
		return declinef(StageMode, ReasonStreamingUnproven, "streaming is out of scope for the native unary attempt")
	default:
		return declinef(StageMode, ReasonModeUnknown, "unrecognized request mode")
	}
}

// admitStrategy declines any whole-orchestration-plan shape the initial matrix
// did not prove: more than one leaf, a fallback chain, round-robin, a legacy
// child, a request-retry override, or a URL rewrite/proxy the exact lane bypasses.
func admitStrategy(in Input) *Decline {
	switch {
	case !in.SingleLeaf:
		return declinef(StageStrategy, ReasonNotSingleLeaf, "orchestration plan does not resolve exactly one leaf")
	case in.HasFallbackChain:
		return declinef(StageStrategy, ReasonFallbackChain, "orchestration plan has a fallback chain")
	case in.HasRoundRobin:
		return declinef(StageStrategy, ReasonRoundRobin, "orchestration plan has a round-robin strategy")
	case in.IsLegacyChild:
		return declinef(StageStrategy, ReasonLegacyChild, "selected child is a legacy child")
	case in.HasRequestRetryOverride:
		return declinef(StageStrategy, ReasonRequestRetryOverride, "request carries a retry override")
	}
	return nil
}

// validateCanonicalBody is the StageBody sanity gate on the native canonical
// body: non-empty, valid JSON, and carrying the literal target model as its
// top-level "model" value. Body bytes are never surfaced in a diagnostic.
func validateCanonicalBody(raw []byte, target string) *Decline {
	if len(raw) == 0 {
		return declinef(StageBody, ReasonBodyEmpty, "canonical body is empty")
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return declinef(StageBody, ReasonBodyNotJSON, "canonical body is not valid JSON")
	}
	if probe.Model != target {
		return declinef(StageBody, ReasonBodyMissingTarget, "canonical body does not carry the literal target model")
	}
	return nil
}

// providerFromResolved folds an arbitrary resolved provider into the bounded
// provider label used by the attempts metric (§9). The five known nanollm
// provider classes each get their own label; the BAML `aws-bedrock` spelling
// folds onto `bedrock`; an empty provider is "unknown"; everything else is
// "other". The label OBSERVES; it never decides admission.
func providerFromResolved(p string) providerLabel {
	switch p {
	case nativebody.ProviderOpenAI:
		return providerOpenAI
	case "anthropic":
		return providerAnthropic
	case providerNanollmBedrock, providerBAMLBedrock:
		return providerBedrock
	case "cerebras":
		return providerCerebras
	case "cohere":
		return providerCohere
	case "":
		return providerUnknown
	default:
		return providerOther
	}
}

// classifyPromptDecline maps a native prompt-renderer decline feature to a
// stable (stage, reason). Media features attribute to the message stage; every
// other prompt feature is an unclaimed prompt.
func classifyPromptDecline(feature string) *Decline {
	switch feature {
	case nativeprompt.FeatureNilOutputSchema:
		return declinef(StagePrompt, ReasonOutputSchemaAbsent, "input can reach ctx.output_format but no schema was supplied")
	case nativeprompt.FeatureInvalidMedia, nativeprompt.FeatureUnsupportedMediaKind:
		return declinef(StageMessage, ReasonMediaPart, "input carries an unproven media part")
	default:
		return declinef(StagePrompt, ReasonPromptUnclaimed, "native renderer does not claim this prompt shape")
	}
}

// classifyBodyDecline maps a native body-builder decline feature to a stable
// (stage, reason), so a residual client/message decline surfaced by the body
// builder is attributed to the same stage the earlier explicit gates use.
func classifyBodyDecline(feature string) *Decline {
	switch feature {
	case nativebody.FeatureProvider:
		return declinef(StageProvider, ReasonProviderNotOpenAI, "body builder rejected a non-openai provider")
	case nativebody.FeatureModelSelection:
		return declinef(StageClientSelection, ReasonModelAbsent, "body builder found no resolved literal target model")
	case nativebody.FeatureModelEscape:
		return declinef(StageClientSelection, ReasonModelNotLiteral, "target model literal carries an undecodable escape")
	case nativebody.FeatureClientSelection:
		return declinef(StageClientSelection, ReasonAmbiguousSelection, "body builder found an ambiguous client selection")
	case nativebody.FeatureInvalidUTF8:
		return declinef(StageMessage, ReasonInvalidUTF8, "a body-bound string is not valid UTF-8")
	case nativebody.FeatureRequestBody:
		return declinef(StageClientOption, ReasonRequestBodyOption, "client carries a request_body passthrough")
	case nativebody.FeatureTools:
		return declinef(StageClientOption, ReasonToolsOption, "client carries a tools/functions option")
	case nativebody.FeatureResponseFormat:
		return declinef(StageClientOption, ReasonResponseFormatOption, "client carries a response_format option")
	case nativebody.FeatureClientOption:
		return declinef(StageClientOption, ReasonUnprovenClientOption, "client carries an unproven body-affecting option")
	case nativebody.FeatureStreaming:
		return declinef(StageMode, ReasonStreamingUnproven, "body builder rejected a streaming attempt")
	case nativebody.FeatureEmptyMessages:
		return declinef(StageMessage, ReasonEmptyMessages, "rendered prompt has no messages")
	case nativebody.FeatureRole:
		return declinef(StageMessage, ReasonRoleUnsupported, "rendered message role is unsupported")
	case nativebody.FeatureAllowDuplicateRole:
		return declinef(StageMessage, ReasonRoleUnsupported, "rendered message sets allow_duplicate_role")
	case nativebody.FeatureMessageMeta:
		return declinef(StageMessage, ReasonMessageMetadata, "rendered message carries metadata")
	case nativebody.FeatureEmptyMessage:
		return declinef(StageMessage, ReasonEmptyMessage, "rendered message has no content parts")
	case nativebody.FeatureMediaPart:
		return declinef(StageMessage, ReasonMediaPart, "rendered message carries a media part")
	case nativebody.FeatureUnknownPart:
		return declinef(StageMessage, ReasonUnknownPart, "rendered message carries an unknown part")
	default:
		return declinef(StageBody, ReasonBodyUnclaimed, "native body builder does not claim this shape")
	}
}
