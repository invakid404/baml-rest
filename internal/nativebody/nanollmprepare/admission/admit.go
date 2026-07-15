package admission

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
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
	Method              string
	Mode                Mode

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

// Admit runs the FULL pre/post-Prepare no-send native admission predicate for
// one unary OpenAI `_dynamic` call. It returns:
//
//   - (*Admitted, nil) when every layer is proven up to the exact RoundTrip — the
//     plan is READY to send but is deliberately NOT sent (no serving change);
//   - (nil, *Decline) — unwrapping to ErrDeclined — for a stable, secret-free
//     parity-decline to BAML;
//   - (nil, non-decline error) for an unexpected native planner/FFI error before
//     any socket (counted as OutcomePlannerError so it can alert, not read as
//     ordinary unsupported traffic).
//
// It opens ZERO sockets and performs ZERO RoundTrips on every path. The
// request-scoped nanollm engine is created and safely Closed within the call.
func (a *Admitter) Admit(ctx context.Context, in Input) (*Admitted, error) {
	provider := providerFromResolved(in.ResolvedProvider)

	decline := func(d *Decline) (*Admitted, error) {
		a.m.recordDecline(in.Mode, provider, d)
		return nil, d
	}
	plannerErr := func(err error) (*Admitted, error) {
		a.m.recordAttempt(in.Mode, provider, OutcomePlannerError)
		return nil, err
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
	if in.Method != dynamicMethod {
		return decline(declinef(StageMethod, ReasonNotDynamicMethod, "internal method is not Baml_Rest_Dynamic"))
	}
	if d := admitMode(in.Mode); d != nil {
		return decline(d)
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
	if in.ResolvedProvider != nativebody.ProviderOpenAI {
		return decline(declinef(StageProvider, ReasonProviderNotOpenAI, "resolved leaf provider is not openai"))
	}

	// --- Layer 3: effective dynamic client (+ request-scoped engine) ------
	// mapDynamicClient resolves the effective target (base_url + /chat/completions)
	// and evaluates in.WouldRewriteOrProxy against it BEFORE constructing the
	// request-scoped engine — a rewrite or a proxied effective target declines at
	// the strategy stage there, before the engine, the plan, BAML's plan, or a
	// plan_compare.
	client, facts, dec, err := mapDynamicClient(in.Registry, in.Alias, in.WouldRewriteOrProxy)
	if err != nil {
		return plannerErr(err)
	}
	if dec != nil {
		return decline(dec)
	}
	// Request-scoped: safely Close the engine on EVERY return path below. The
	// plan is bytes, so the no-send path never needs the engine to outlive Admit.
	defer client.Close()

	// --- Layer 4: prompt + canonical body ---------------------------------
	if d := validateMessages(in.Messages); d != nil {
		return decline(d)
	}
	if _, serr := schema.FromDynamicOutputSchema(in.OutputSchema, schema.BuildOptions{}); serr != nil {
		return decline(declinef(StagePrompt, ReasonOutputSchemaUnbounded, "output schema is outside the native schema/SAP bounds"))
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

	intent := nativebody.ClientIntent{
		Provider:    facts.provider,
		TargetModel: facts.target,
		ModelAlias:  in.Alias,
	}
	canonical, berr := nativebody.BuildOpenAIChat(rendered, intent)
	if berr != nil {
		var bd *nativebody.Decline
		if errors.As(berr, &bd) {
			return decline(classifyBodyDecline(bd.Feature))
		}
		return plannerErr(berr)
	}
	canonicalBytes := canonical.Bytes()
	if d := validateCanonicalBody(canonicalBytes, facts.target); d != nil {
		return decline(d)
	}

	// --- Layer 5: prepared plan, revalidated immediately after Prepare ----
	prep, perr := client.Prepare(nanollm.Request{
		Model:  in.Alias,
		Body:   canonicalBytes,
		Type:   nanollm.ChatCompletion,
		Stream: false,
	})
	if perr != nil {
		// Prepare could not prove a plan: a parity-decline to BAML (no socket).
		return decline(declinef(StagePrepare, ReasonPrepareError, "nanollm Prepare could not prove a plan"))
	}
	if d := validatePreparedBody(prep, canonicalBytes); d != nil {
		return decline(d)
	}
	if d := validatePlanMeta(prep, in.Alias, facts.target); d != nil {
		return decline(d)
	}
	if d := validatePlanExpiry(prep); d != nil {
		return decline(d)
	}
	if d := validatePlanHeaders(prep, facts.baseURL); d != nil {
		return decline(d)
	}
	exactReq := exactRequestFromPlan(prep)
	if d := validateExactTransport(a.exec, exactReq); d != nil {
		return decline(d)
	}

	// Proven up to — but NOT including — the RoundTrip. Do NOT send.
	a.m.recordAttempt(in.Mode, provider, OutcomeAdmitted)
	return &Admitted{
		Prepared:     prep,
		ExactRequest: exactReq,
		Alias:        in.Alias,
		Target:       facts.target,
		Provider:     facts.provider,
	}, nil
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
// provider label used by the attempts metric.
func providerFromResolved(p string) providerLabel {
	switch p {
	case nativebody.ProviderOpenAI:
		return providerOpenAI
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
