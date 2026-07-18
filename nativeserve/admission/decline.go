package admission

import (
	"errors"
	"fmt"
)

// Stage is the FIXED enum of native-admission decline stages. The set mirrors
// the scope's "Exact initial predicate" layers, evaluated in order; a decline
// records the FIRST stage that fails. The values are stable, secret-free label
// values for the bounded-cardinality declines metric — never free-form, never a
// method/client/model/URL/alias/request-id.
type Stage string

const (
	// StageCapability: native-worker capability, the preferred non-streaming
	// Request API, and the BuildRequest route (layer 1).
	StageCapability Stage = "capability"
	// StageFlag: BAML_REST_USE_DEBAML resolved enabled (layer 1).
	StageFlag Stage = "flag"
	// StageMethod: internal method is exactly Baml_Rest_Dynamic (layer 1).
	StageMethod Stage = "method"
	// StageMode: mode is unary `call` — not call-with-raw or streaming (layer 1).
	StageMode Stage = "mode"
	// StageStrategy: exactly one leaf; no fallback chain, round-robin, legacy
	// child, request-retry override, client-retry policy, or URL rewrite/proxy
	// the exact lane would bypass (layer 2).
	StageStrategy Stage = "strategy"
	// StageProvider: the resolved leaf / effective client provider is exactly
	// openai (layers 2-3).
	StageProvider Stage = "provider"
	// StageMapping: the pure registry->nanollm config mapper could not
	// CONFIDENTLY map the selected client to a request-scoped native intent
	// (layer 3). In S1 the only complete production mapping is strict OpenAI, so
	// every non-openai provider mapping-declines here (mapping_unavailable) BEFORE
	// nanollm.New — no non-openai socket is admitted. S2 fills in the generic
	// bearer + Bedrock mappers; a future provider whose config cannot be expressed
	// keeps declining here rather than inventing credentials/options.
	StageMapping Stage = "mapping"
	// StageClientSelection: the registry validates and resolves exactly one
	// unambiguous non-nil, non-empty client with a resolved literal target model,
	// named by primary when a primary is present (layer 3).
	StageClientSelection Stage = "client_selection"
	// StageClientOption: no client option beyond the proved transport trio
	// (model/base_url/api_key) — no headers option, tools, response_format,
	// request_body, or any other body-affecting option (layers 3-4).
	StageClientOption Stage = "client_option"
	// StageCredentialSource: base_url and api_key are present, non-empty strings
	// supplied by the registry — no default credential chain and no ambient/
	// process-env resolution (layer 3).
	StageCredentialSource Stage = "credential_source"
	// StagePrompt: nativeprompt claims the exact generated dynamic prompt, and
	// the output schema is present and passes the dynamic schema / native SAP
	// bounds (layers 1, 4).
	StagePrompt Stage = "prompt"
	// StageMessage: non-empty ordered messages; roles only system/user/assistant;
	// text-only, valid UTF-8, at least one part per message; no media, metadata,
	// custom roles, or unknown parts (layer 4).
	StageMessage Stage = "message"
	// StageBody: nativebody claims the selected client + rendered prompt, and the
	// canonical body is present, non-empty, valid JSON, and carries the literal
	// target model (layer 4).
	StageBody Stage = "body"
	// StagePrepare: nanollm Prepare proved a plan whose body bytes are non-empty,
	// valid JSON, and byte-equal to the admitted canonical body (layer 5).
	StagePrepare Stage = "prepare"
	// StagePlanMeta: the prepared plan's meta matches the preflight intent —
	// alias/target/provider/request-type/stream/transform/max-retries and a JSON
	// response format (layer 5).
	StagePlanMeta Stage = "plan_meta"
	// StagePlanExpiry: the plan is unsigned and non-expiring (SignedAt nil,
	// ExpiresAt nil, !Expired) (layer 5).
	StagePlanExpiry Stage = "plan_expiry"
	// StagePlanHeaders: method POST; URL exactly base + /chat/completions; headers
	// only the proved unique OpenAI semantics (one application/json Content-Type,
	// one bearer Authorization) with no custom/duplicate/Host/framing field
	// (layer 5).
	StagePlanHeaders Stage = "plan_headers"
	// StageExactTransport: the exact-transport header validation succeeds before
	// any RoundTrip (layer 5).
	StageExactTransport Stage = "exact_transport"
	// StageContext: the request context was cancelled/expired mid-admission,
	// around a non-context FFI boundary (New / render / Prepare). It is a
	// PRE-SOCKET decline to BAML — no native plan is sent — so the ordinary BAML
	// attempt surfaces the same context error to the caller.
	StageContext Stage = "context"
)

// Reason is the FIXED enum of secret-free decline reason codes. Every Decline
// carries one Reason drawn from this set; the (Stage, Reason) pair is the only
// thing the declines metric labels, so the cardinality stays bounded and no
// free-form text — let alone a secret — ever reaches a label.
type Reason string

const (
	// capability
	ReasonWorkerNotCapable Reason = "worker_not_capable"
	ReasonRequestAPIAbsent Reason = "request_api_absent"
	ReasonNotBuildReqRoute Reason = "not_buildrequest_route"
	// flag
	ReasonFlagDisabled Reason = "flag_disabled"
	// method
	ReasonNotDynamicMethod Reason = "not_dynamic_method"
	// mode
	ReasonWithRawUnproven   Reason = "with_raw_unproven"
	ReasonStreamingUnproven Reason = "streaming_unproven"
	ReasonModeUnknown       Reason = "mode_unknown"
	// ReasonNotStreamMode is the STREAM-claim mirror of ReasonStreamingUnproven:
	// the stream admission declines a non-streaming mode (unary call /
	// call-with-raw) that reached the stream claim by mistake.
	ReasonNotStreamMode Reason = "not_stream_mode"
	// strategy
	ReasonNotSingleLeaf        Reason = "not_single_leaf"
	ReasonFallbackChain        Reason = "fallback_chain"
	ReasonRoundRobin           Reason = "round_robin"
	ReasonLegacyChild          Reason = "legacy_child"
	ReasonRequestRetryOverride Reason = "request_retry_override"
	ReasonClientRetryPolicy    Reason = "client_retry_policy"
	ReasonURLRewriteOrProxy    Reason = "url_rewrite_or_proxy"
	// provider
	ReasonProviderNotOpenAI Reason = "provider_not_openai"
	// mapping — the pure registry->nanollm mapper's stable declines (layer 3).
	// provider_mismatch is the §4.2 guard when the selected client's explicit
	// provider disagrees with the resolved leaf provider. mapping_unavailable was
	// S1's "no complete non-openai mapping yet" decline; S2 replaced it with the
	// common bearer + Bedrock mappers, so it is no longer emitted — it is retained
	// as a reserved bounded-enum value (a future provider whose config truly cannot
	// be expressed confidently may decline here again rather than inventing
	// credentials/options).
	ReasonMappingUnavailable Reason = "mapping_unavailable"
	ReasonProviderMismatch   Reason = "provider_mismatch"
	// client_selection
	ReasonNoRegistry         Reason = "no_registry"
	ReasonRegistryInvalid    Reason = "registry_invalid"
	ReasonNoClients          Reason = "no_clients"
	ReasonAmbiguousSelection Reason = "ambiguous_selection"
	ReasonPrimaryMissing     Reason = "primary_missing"
	ReasonModelAbsent        Reason = "model_absent"
	ReasonModelNotLiteral    Reason = "model_not_literal"
	ReasonInvalidAlias       Reason = "invalid_alias"
	// client_option
	ReasonHeadersOption        Reason = "headers_option"
	ReasonToolsOption          Reason = "tools_option"
	ReasonResponseFormatOption Reason = "response_format_option"
	ReasonRequestBodyOption    Reason = "request_body_option"
	ReasonUnprovenClientOption Reason = "unproven_client_option"
	// client_option — S2 trusted-provider option projection. A supported common
	// body option carrying a malformed/wrong-typed value, a malformed headers map,
	// or an aws-bedrock endpoint_url override (never rewrite a signed URL) each
	// decline with a stable reason; the request-controlled option VALUE is never
	// surfaced.
	ReasonInvalidHeadersOption Reason = "invalid_headers_option"
	ReasonInvalidBodyOption    Reason = "invalid_body_option"
	ReasonEndpointOverride     Reason = "endpoint_override"
	// credential_source
	ReasonBaseURLAbsent Reason = "base_url_absent"
	ReasonAPIKeyAbsent  Reason = "api_key_absent"
	// credential_source — S2 trusted-provider credential resolution. A trusted
	// bearer provider missing its key (explicit or <PROVIDER>_API_KEY env), a
	// present-but-non-string base_url, an aws-bedrock client missing its region, a
	// partial static credential pair, a resolved-but-empty credential, or a
	// declared-but-broken credential source that must NEVER fall to ambient creds
	// each decline with a stable, secret-free reason.
	//
	// explicit_api_key_required_for_custom_base_url is the SECURITY guard for the
	// bearer env fallback: a dynamic registry can point base_url anywhere, so the
	// documented <PROVIDER>_API_KEY environment credential is resolved ONLY when no
	// custom base_url is configured (the provider's default service root). A
	// non-empty base_url with no explicit api_key declines here rather than sending
	// the host credential to a wire-controlled destination.
	ReasonInvalidBaseURL                      Reason = "invalid_base_url"
	ReasonRegionMissing                       Reason = "region_missing"
	ReasonMissingCredential                   Reason = "missing_credential"
	ReasonPartialCredentials                  Reason = "partial_credentials"
	ReasonUnsupportedCredentialSource         Reason = "unsupported_credential_source"
	ReasonCredentialResolution                Reason = "credential_resolution"
	ReasonExplicitAPIKeyRequiredForCustomBase Reason = "explicit_api_key_required_for_custom_base_url"
	// client_selection — S2 aws-bedrock model-key selection: the registry must
	// carry EXACTLY ONE of `model`/`model_id` (both, or neither, declines).
	ReasonBedrockModelKeys Reason = "bedrock_model_keys"
	// prompt
	ReasonOutputSchemaAbsent    Reason = "output_schema_absent"
	ReasonOutputSchemaUnbounded Reason = "output_schema_unbounded"
	ReasonPromptUnclaimed       Reason = "prompt_unclaimed"
	// message
	ReasonEmptyMessages    Reason = "empty_messages"
	ReasonEmptyMessage     Reason = "empty_message"
	ReasonAmbiguousContent Reason = "ambiguous_content"
	ReasonRoleUnsupported  Reason = "role_unsupported"
	ReasonMediaPart        Reason = "media_part"
	ReasonUnknownPart      Reason = "unknown_part"
	ReasonMixedPayload     Reason = "mixed_payload"
	ReasonMessageMetadata  Reason = "message_metadata"
	ReasonInvalidUTF8      Reason = "invalid_utf8"
	// body
	ReasonBodyEmpty         Reason = "body_empty"
	ReasonBodyNotJSON       Reason = "body_not_json"
	ReasonBodyMissingTarget Reason = "body_missing_target_model"
	ReasonBodyUnclaimed     Reason = "body_unclaimed"
	// prepare — the typed New/Prepare classifier (§5.1) splits today's too-broad
	// prepare_error: a nanollm *Error whose Code is unsupported_request /
	// invalid_provider is an ORDINARY pre-socket unsupported decline to BAML; any
	// OTHER New/Prepare/Build failure is NOT a decline — it is recorded as
	// OutcomePlannerError (safe BAML fallback that alerts) so it never reads as
	// expected unsupported traffic. invalid_provider is keyed by CODE (via
	// errors.As, never a string match); nanollm v0.4.3 does not emit it yet, so the
	// classifier is forward-ready for Viktor's P0.
	ReasonPrepareUnsupportedRequest Reason = "unsupported_request"
	ReasonPrepareInvalidProvider    Reason = "invalid_provider"
	ReasonBodyNotByteEqual          Reason = "body_not_byte_equal"
	// plan_meta
	ReasonAliasMismatch         Reason = "alias_mismatch"
	ReasonTargetMismatch        Reason = "target_mismatch"
	ReasonPlanProviderMismatch  Reason = "plan_provider_mismatch"
	ReasonRequestTypeMismatch   Reason = "request_type_mismatch"
	ReasonPlanStreamTrue        Reason = "plan_stream_true"
	ReasonTransformPresent      Reason = "transform_present"
	ReasonMaxRetriesNonzero     Reason = "max_retries_nonzero"
	ReasonResponseFormatNotJSON Reason = "response_format_not_json"
	// ReasonPlanStreamFalse / ReasonResponseFormatNotSSE are the STREAM-claim
	// mirrors of ReasonPlanStreamTrue / ReasonResponseFormatNotJSON: a stream plan
	// meta MUST carry stream=true and an SSE response format, so the opposite of
	// each declines.
	ReasonPlanStreamFalse      Reason = "plan_stream_false"
	ReasonResponseFormatNotSSE Reason = "response_format_not_sse"
	// ReasonEmbeddingPlan is the S2 trusted-provider self-consistency guard: a
	// chat-completion Prepare that produced an EMBEDDINGS endpoint plan is NOT a
	// usable unary chat plan and declines PRE-socket. It is provider-neutral (it
	// enumerates a wrong OPERATION shape, never a provider), and it is the fail-
	// closed backstop for a DEFERRED provider whose spec plans an embedding for a
	// chat request in the pinned nanollm (cohere in v0.4.3 plans /v2/embed and does
	// not reject RequestType::ChatCompletion — the §1.3 gap P0 closes). See
	// classifyEngineError's KNOWN-LIMITATION note (#546/#583). It never fires for a
	// real chat provider (openai/anthropic/cerebras/bedrock chat endpoints are not
	// embeddings) and auto-retires when a provider gains a real chat endpoint.
	ReasonEmbeddingPlan Reason = "embedding_plan"
	// plan_expiry
	ReasonSignedPlan   Reason = "signed_plan"
	ReasonExpiringPlan Reason = "expiring_plan"
	ReasonExpiredPlan  Reason = "expired_plan"
	// plan_headers
	ReasonMethodNotPost         Reason = "method_not_post"
	ReasonURLMismatch           Reason = "url_mismatch"
	ReasonUnexpectedHeader      Reason = "unexpected_header"
	ReasonDuplicateHeader       Reason = "duplicate_header"
	ReasonMissingContentType    Reason = "missing_content_type"
	ReasonWrongContentType      Reason = "wrong_content_type"
	ReasonMissingAuthorization  Reason = "missing_authorization"
	ReasonMultipleAuthorization Reason = "multiple_authorization"
	ReasonHostHeader            Reason = "host_header"
	// exact_transport
	ReasonTransportControlledHeader Reason = "transport_controlled_header"
	ReasonDuplicateHost             Reason = "duplicate_host"
	ReasonInvalidHeaderName         Reason = "invalid_header_name"
	ReasonInvalidHeaderValue        Reason = "invalid_header_value"
	// context
	ReasonContextCancelled Reason = "context_cancelled"
)

// ErrDeclined is the umbrella sentinel for a native admission parity-decline:
// the predicate refused a request BEFORE any socket, so native never out-claims
// BAML on a shape it has not proven. Callers gate on errors.Is(err, ErrDeclined)
// and recover the stable stage/reason via errors.As(&Decline{}).
var ErrDeclined = errors.New("nativeserve/admission: native admission declined to BAML")

// Decline is a stable, secret-free native-admission decline: the fixed-enum
// Stage + Reason that name WHICH proof failed, plus a short Detail for redacted
// diagnostics. Detail describes structural facts only — never body bytes, header
// values, api keys, Authorization, prompt text, or schema contents. It unwraps
// to ErrDeclined.
type Decline struct {
	Stage  Stage
	Reason Reason
	Detail string
}

func (d *Decline) Error() string {
	return fmt.Sprintf("nativeserve/admission: declined at %s (%s): %s", d.Stage, d.Reason, d.Detail)
}

func (d *Decline) Unwrap() error { return ErrDeclined }

// declinef builds a *Decline with a formatted, secret-free detail. Callers must
// never pass a secret-bearing value into the format arguments.
func declinef(stage Stage, reason Reason, format string, args ...any) *Decline {
	return &Decline{Stage: stage, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
