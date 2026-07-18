package admission

import (
	"context"
	"fmt"
	"sort"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// VerificationPolicy selects the post-Prepare verification regime for an admitted
// plan (§6). It is carried on the mapped client and the Claim so the serve path
// branches ONCE on a typed enum rather than re-deriving intent from free-form
// provider strings. The policy assignment is the ONE intentional OpenAI special
// case — it is NOT admission membership: a non-openai plan still succeeds/fails on
// mapping + nanollm, it just never runs a BAML plan/response comparison.
type VerificationPolicy uint8

const (
	// PolicyStrictOpenAI runs every existing byte-exact OpenAI oracle: canonical
	// body byte-equality, the exact base + /chat/completions URL and unique
	// bearer/Content-Type headers, the unsigned/never-expiring plan, the BAML
	// request-plan comparison, and the same-response comparison. It is the strict
	// anchor and is UNCHANGED in S1.
	PolicyStrictOpenAI VerificationPolicy = iota
	// PolicyTrustedProvider runs only the provider-neutral post-Prepare
	// self-consistency checks (§5.2) and NEVER a BAML plan/response comparison:
	// nanollm owns the provider's transport contract, so the transformed provider
	// bytes / signed plan / member order are not a BAML parity claim. S1 ships no
	// production trusted mapping (every non-openai provider mapping-declines
	// before nanollm.New); the policy plumbing is exercised by synthetic tests and
	// activated in S2.
	PolicyTrustedProvider
)

func (p VerificationPolicy) String() string {
	switch p {
	case PolicyStrictOpenAI:
		return "strict_openai"
	case PolicyTrustedProvider:
		return "trusted_provider"
	default:
		return "unknown"
	}
}

// Provider spellings the mapper normalizes on. BAML's registry uses the
// `aws-bedrock` spelling; nanollm's model prefix is `bedrock`. Every other
// provider string is used as the nanollm prefix verbatim — there is deliberately
// no `supportedProviders` membership map in admission.
const (
	providerBAMLBedrock      = "aws-bedrock"
	providerNanollmBedrock   = "bedrock"
	providerNanollmAnthropic = "anthropic"
)

// clientFacts are the secret-free, body-relevant facts the mapper resolves from
// the selected client. baseURL is retained ONLY for the OpenAI prepared-plan URL
// check (base + /chat/completions); the api key is written straight into the
// engine config and is NEVER retained, returned, or logged. target is the
// resolved literal model — never the internal nanollm alias. provider is the
// nanollm provider spelling (aws-bedrock normalized to bedrock).
//
// bedrockSource is the bounded credential-source label for a mapped aws-bedrock
// client (empty for every other provider); it is a metric observation only and
// carries no credential value. body is the resolved supported body-field subset a
// TRUSTED provider projects onto the ChatRequest (zero-valued for the strict
// OpenAI anchor, which never widens beyond the transport trio) — it is body-
// relevant and secret-free (temperature/max_tokens/etc., never a credential).
type clientFacts struct {
	provider      string
	target        string
	baseURL       string
	bedrockSource BedrockCredentialSource
	body          bodyOptions
}

// mappedClient is the PURE mapping of the effective registry client onto a
// request-scoped native intent plus its verification policy — resolved WITHOUT
// calling nanollm (§4.1). newMappedClient is the SOLE nanollm.New site. In S1 the
// only complete mapping is strict OpenAI; every non-openai provider
// mapping-declines (mapping_unavailable) BEFORE newMappedClient, so no non-openai
// socket is admitted. S2 fills in the generic bearer + Bedrock mappers.
//
// SENSITIVE — apiKey and bedrock creds are credentials. A mappedClient MUST NEVER
// be logged, serialized, or emitted; surface only clientFacts (secret-free) for
// metrics/tests.
type mappedClient struct {
	registryProvider string // BAML spelling, e.g. "aws-bedrock"
	nanollmProvider  string // nanollm spelling, e.g. "bedrock"
	target           string
	alias            string
	baseURL          string
	apiKey           string // SENSITIVE (bearer credential; empty for bedrock)
	verification     VerificationPolicy

	// --- S2 trusted-provider material (nil/zero for the strict OpenAI anchor) ---

	// headers is the custom string header map a trusted provider projects onto
	// nanollm ModelConfig.Params["headers"] (a NESTED map — nanollm does not
	// interpolate nested JSON, so header values pass through literally).
	headers map[string]string
	// bedrock carries the resolved aws-bedrock params (region + credential
	// literals + additional model request fields) for a trusted bedrock client.
	bedrock *bedrockParams
	// body is the supported common body-field subset (§4.4) a trusted provider
	// projects onto the typed nanollm ChatRequest / its Extra map.
	body bodyOptions
}

// mappingInput is the pure input to mapClientConfig: the effective registry, the
// SEPARATE internal nanollm alias, the orchestrator-resolved leaf provider (the
// attempted provider), and the effective send client's rewrite/proxy predicate.
type mappingInput struct {
	registry            *bamlutils.ClientRegistry
	alias               string
	resolvedProvider    string
	wouldRewriteOrProxy func(effectiveURL string) bool
}

// transportTrio is the ONLY option set the strict OpenAI mapper accepts: the
// proved transport trio. Every other option — headers, tools, response_format,
// request_body, temperature, or anything unrecognized — declines at
// StageClientOption. There is deliberately no headers passthrough, no default
// credential chain, and no option beyond these three (S2 widens the trusted body
// options; OpenAI stays the strict trio).
var transportTrio = map[string]struct{}{
	"model":    {},
	"base_url": {},
	"api_key":  {},
}

// normalizeNanollmProvider folds the ONE BAML spelling nanollm spells
// differently: `aws-bedrock` -> `bedrock`. Every other nonempty provider string
// is returned verbatim to be used as the nanollm prefix — there is no membership
// check, so a future common-config provider starts mapping without a baml-rest
// change, and an unknown prefix reaches nanollm.New (which types it as
// invalid_provider once Viktor's P0 lands).
func normalizeNanollmProvider(p string) string {
	if p == providerBAMLBedrock {
		return providerNanollmBedrock
	}
	return p
}

// mapClientConfig is the PURE registry->native-intent mapper (§4.1): it resolves
// the effective one-client dynamic registry into a *mappedClient plus its
// verification policy, or a stable *Decline. It is pure EXCEPT for the explicit
// aws-bedrock credential resolver (which may touch shared config / ECS / IMDS,
// bounded by ctx); it never calls nanollm or opens a provider socket. It
// enforces, else declines:
//
//   - the registry validates and resolves EXACTLY ONE unambiguous non-nil client,
//     named by primary when a primary is present;
//   - no client-retry policy on the selected client;
//   - the selected client's explicit provider AGREES with the resolved leaf
//     provider (§4.2; an absent client provider uses the resolved one);
//   - it dispatches on the canonical provider class: openai -> the strict
//     byte-exact anchor (UNCHANGED); aws-bedrock/bedrock -> the Bedrock mapper;
//     any other nonempty provider -> the common bearer mapper. There is NO
//     provider membership allowlist — an unknown prefix maps through the common
//     bearer path and is typed as invalid_provider by nanollm.New/Prepare.
func mapClientConfig(ctx context.Context, in mappingInput) (*mappedClient, *Decline, error) {
	cp, dec := selectOneClient(in.registry)
	if dec != nil {
		return nil, dec, nil
	}

	// A per-client retry policy is a strategy the initial matrix does not prove
	// (baml-rest owns the retry budget); decline at the strategy stage.
	if cp.RetryPolicy != nil {
		return nil, declinef(StageStrategy, ReasonClientRetryPolicy,
			"selected client carries a retry_policy; the initial matrix proves no client-retry policy"), nil
	}

	// §4.2 provider provenance: the RESOLVED leaf provider is the authoritative
	// routing view and stays the mapped provider. It MUST be present — native never
	// guesses the authoritative provider from an absent resolved leaf (an absent
	// leaf letting the selected client's own provider stand in would admit a
	// mapping without an authoritative resolved leaf), so an empty resolved leaf
	// declines here BEFORE any nanollm construction. A selected client that
	// explicitly carries a provider must AGREE with the resolved leaf under CANONICAL
	// spelling (so BAML's aws-bedrock and nanollm's bedrock compare equal, while two
	// genuinely distinct canonical providers still decline). The provider strings are
	// structural routing facts (not secrets), but are not interpolated into the
	// Detail — the bounded provider metric label already records which declined.
	if in.resolvedProvider == "" {
		return nil, declinef(StageClientSelection, ReasonProviderMismatch,
			"the resolved leaf provider is absent; native never guesses the authoritative provider"), nil
	}
	registryProvider := in.resolvedProvider
	nanollmProvider := normalizeNanollmProvider(registryProvider)
	if cp.Provider != "" && normalizeNanollmProvider(cp.Provider) != nanollmProvider {
		return nil, declinef(StageClientSelection, ReasonProviderMismatch,
			"selected client provider disagrees with the resolved leaf provider"), nil
	}

	// Provider-class dispatch (§4.5). No membership allowlist: the default arm maps
	// EVERY other nonempty provider through the common bearer path.
	switch nanollmProvider {
	case nativebody.ProviderOpenAI:
		return mapOpenAIStrict(cp, in, registryProvider, nanollmProvider)
	case providerNanollmBedrock:
		return mapBedrock(ctx, cp, in, registryProvider, nanollmProvider)
	default:
		return mapCommonBearer(cp, in, registryProvider, nanollmProvider)
	}
}

// mapOpenAIStrict is the strict OpenAI mapping (byte-exact, UNCHANGED from the S6
// anchor): options are exactly the transport trio, each a present non-empty
// string; a SEPARATE internal alias distinct from target + client name; and the
// effective send target (base_url + /chat/completions) is neither rewritten nor
// proxied. It is the ONE strict_openai policy assignment.
func mapOpenAIStrict(cp *bamlutils.ClientProperty, in mappingInput, registryProvider, nanollmProvider string) (*mappedClient, *Decline, error) {
	// Reject any option beyond the transport trio BEFORE reading the trio values,
	// scanning in a stable sorted order so a client carrying several unproven
	// options always declines on the same (first) one — no map-iteration
	// nondeterminism in the recorded reason.
	var extra []string
	for key := range cp.Options {
		if _, ok := transportTrio[key]; !ok {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		// The first (sorted) unproven key drives the fixed reason enum via
		// classifyClientOption, but the request-controlled key text is NEVER
		// interpolated into the secret-free Detail.
		return nil, declinef(StageClientOption, classifyClientOption(extra[0]),
			"selected client carries an unproven option beyond the transport trio"), nil
	}

	target, dec := trioString(cp, "model", StageClientSelection, ReasonModelAbsent, ReasonModelNotLiteral)
	if dec != nil {
		return nil, dec, nil
	}
	baseURL, dec := trioString(cp, "base_url", StageCredentialSource, ReasonBaseURLAbsent, ReasonBaseURLAbsent)
	if dec != nil {
		return nil, dec, nil
	}
	apiKey, dec := trioString(cp, "api_key", StageCredentialSource, ReasonAPIKeyAbsent, ReasonAPIKeyAbsent)
	if dec != nil {
		return nil, dec, nil
	}

	if dec := validateAlias(in.alias, target, cp.Name); dec != nil {
		return nil, dec, nil
	}

	// Effective send-path rewrite/proxy parity — evaluated now that the effective
	// target is known (base_url + /chat/completions) and BEFORE any engine is
	// constructed. A rewrite or a proxied target is an unproven shape that declines
	// here. A nil predicate (lightweight tests that carry no send client) skips it.
	if in.wouldRewriteOrProxy != nil && in.wouldRewriteOrProxy(baseURL+chatCompletionsPath) {
		return nil, declinef(StageStrategy, ReasonURLRewriteOrProxy,
			"the effective send path would rewrite or proxy the request target"), nil
	}

	return &mappedClient{
		registryProvider: registryProvider,
		nanollmProvider:  nanollmProvider,
		target:           target,
		alias:            in.alias,
		baseURL:          baseURL,
		apiKey:           apiKey,
		verification:     PolicyStrictOpenAI,
	}, nil, nil
}

// validateAlias enforces the required SEPARATE internal alias: non-empty and
// distinct from BOTH the resolved target model and the selected client name (an
// alias == target would make the later plan alias/target equality checks
// tautological; alias == client name would blur the internal alias with the
// operator-visible identity). Shared by every provider mapper.
func validateAlias(alias, target, clientName string) *Decline {
	if alias == "" || alias == target || alias == clientName {
		return declinef(StageClientSelection, ReasonInvalidAlias,
			"internal alias is empty or collides with the resolved target model or the selected client name")
	}
	return nil
}

// newMappedClient is the SOLE nanollm.New call (§4.1). It builds the
// request-scoped engine for an already-mapped client: ONE model under the
// internal alias, the nanollm-spelled provider + resolved target, a zero retry
// budget, no fallbacks, and UseProcessEnv false so nanollm never reads the
// ambient process environment and no default credential chain inside the engine
// can mask a difference. The returned client is OPEN; the caller owns Close. The
// typed New-error classification lives in mapDynamicClient (the sole caller),
// which keeps this function a pure construction step.
//
// The strict OpenAI anchor (PolicyStrictOpenAI) is UNCHANGED from S1: the literal
// trio (api_key/base_url written directly) with Env nil. A trusted provider
// (PolicyTrustedProvider) instead routes every resolved secret through a
// request-local private Config.Env placeholder (§4.3): nanollm's interpolation is
// single-pass, so a resolved value that itself contains a literal ${...} stays
// literal, and UseProcessEnv false forbids any ambient fallback. The values may be
// real in production and are never logged (the proof is structural, with fake
// values in tests).
func newMappedClient(m *mappedClient) (*nanollm.Client, error) {
	if m.verification == PolicyStrictOpenAI {
		// UNCHANGED strict anchor: literal trio, Env nil.
		return nanollm.New(nanollm.Config{
			Models: []nanollm.ModelConfig{{
				Name:       m.alias,
				Model:      m.nanollmProvider + "/" + m.target,
				APIKey:     m.apiKey,
				BaseURL:    m.baseURL,
				MaxRetries: 0,
			}},
			Env:           nil,
			UseProcessEnv: false,
		})
	}
	return nanollm.New(newTrustedConfig(m))
}

// mapDynamicClient resolves the effective one-client dynamic registry into a
// request-scoped nanollm engine, its secret-free clientFacts, and its
// verification policy — composing the pure mapClientConfig with the sole
// newMappedClient. A pure-mapping decline (unsupported/ambiguous shape,
// mapping_unavailable, provider mismatch) returns a *Decline; a typed nanollm
// New failure classifies via classifyEngineError — an ordinary unsupported code
// (unsupported_request / invalid_provider) is a pre-socket decline to BAML, and
// any other construction failure is a non-decline planner error the caller alerts
// on rather than counting as ordinary unsupported traffic.
//
// The returned client is OPEN; the caller owns Close. ctx bounds the aws-bedrock
// credential resolution (the one impurity inside mapClientConfig); it is unused
// for the strict OpenAI and common bearer paths.
func mapDynamicClient(ctx context.Context, reg *bamlutils.ClientRegistry, alias, resolvedProvider string, wouldRewriteOrProxy func(effectiveURL string) bool) (*nanollm.Client, clientFacts, VerificationPolicy, *Decline, error) {
	m, dec, err := mapClientConfig(ctx, mappingInput{
		registry:            reg,
		alias:               alias,
		resolvedProvider:    resolvedProvider,
		wouldRewriteOrProxy: wouldRewriteOrProxy,
	})
	if err != nil {
		return nil, clientFacts{}, 0, nil, err
	}
	if dec != nil {
		return nil, clientFacts{}, 0, dec, nil
	}

	client, cerr := newMappedClient(m)
	if cerr != nil {
		if classifyEngineError(cerr) == engineUnsupported {
			// A typed unsupported/invalid-provider construction: an ordinary
			// pre-socket decline to the same BAML child (no socket, secret-free —
			// nanollm.New's error never echoes the key).
			return nil, clientFacts{}, 0, declinef(StagePrepare, prepareUnsupportedReason(cerr),
				"nanollm.New reported a typed unsupported provider"), nil
		}
		// Unexpected native construction failure — a planner error, not a
		// parity-decline, so it can alert instead of reading as unsupported traffic.
		return nil, clientFacts{}, 0, nil, fmt.Errorf("nativeserve/admission: nanollm.New: %w", cerr)
	}

	facts := clientFacts{provider: m.nanollmProvider, target: m.target, baseURL: m.baseURL, body: m.body}
	if m.bedrock != nil {
		facts.bedrockSource = m.bedrock.source
	}
	return client, facts, m.verification, nil, nil
}

// NewResponseClient rebuilds the request-scoped nanollm engine for the SAME
// effective client the plan phase admitted, so the de-BAML same-response shadow
// oracle can call TranslateResponse(alias, …) on BAML's already-fetched response.
// It reuses the EXACT strict-OpenAI mapping mapDynamicClient uses (identical
// alias -> openai/target config, zero retries, no ambient env), so the
// translation is faithful to what the admitted plan would have translated with —
// but it performs NO rewrite/proxy check (that gate already decided routing in
// the plan phase) and NO Prepare (no plan is built or sent). It is only invoked
// on the OpenAI surface AFTER admission admitted this exact client, so it resolves
// the provider as openai; a decline/error here is unexpected and surfaces as an
// error. The returned client is OPEN; the caller owns Close. It never opens a
// socket.
func NewResponseClient(reg *bamlutils.ClientRegistry, alias string) (*nanollm.Client, string, error) {
	// OpenAI-only re-map (no bedrock resolution), so a background context suffices
	// for the ctx the bedrock resolver would otherwise bound.
	client, facts, _, dec, err := mapDynamicClient(context.Background(), reg, alias, nativebody.ProviderOpenAI, nil)
	if err != nil {
		return nil, "", err
	}
	if dec != nil {
		// *Decline is an error (unwraps to ErrDeclined); return it so a caller can
		// classify a surprising re-map failure without a second bespoke type.
		return nil, "", dec
	}
	return client, facts.target, nil
}

// selectOneClient resolves the effective client of a dynamic registry: the
// Primary by name when set, else the sole client when exactly one is present.
// It reuses ClientRegistry.Validate (the same first-wins/last-wins ambiguity
// guard the adapter entry points enforce) so native never selects a different
// client than BAML would. Anything ambiguous declines at StageClientSelection.
func selectOneClient(reg *bamlutils.ClientRegistry) (*bamlutils.ClientProperty, *Decline) {
	if reg == nil {
		return nil, declinef(StageClientSelection, ReasonNoRegistry, "no client registry supplied")
	}
	if err := reg.Validate(); err != nil {
		return nil, declinef(StageClientSelection, ReasonRegistryInvalid,
			"registry is ambiguous (duplicate/empty client name)")
	}
	clients := make([]*bamlutils.ClientProperty, 0, len(reg.Clients))
	for _, c := range reg.Clients {
		if c != nil {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		return nil, declinef(StageClientSelection, ReasonNoClients, "registry has no clients")
	}
	if reg.Primary != nil && *reg.Primary != "" {
		for _, c := range clients {
			if c.Name == *reg.Primary {
				return c, nil
			}
		}
		return nil, declinef(StageClientSelection, ReasonPrimaryMissing, "primary names no client in the registry")
	}
	if len(clients) == 1 {
		return clients[0], nil
	}
	return nil, declinef(StageClientSelection, ReasonAmbiguousSelection,
		"registry has multiple clients but no primary; selection is ambiguous")
}

// trioString reads one transport-trio option as a present, non-empty string, or
// declines. absent is the reason for a missing key; nonString is the reason for
// a present-but-non-string/empty value (the two coincide for base_url/api_key).
func trioString(cp *bamlutils.ClientProperty, key string, stage Stage, absent, nonString Reason) (string, *Decline) {
	raw, ok := cp.Options[key]
	if !ok {
		return "", declinef(stage, absent, "selected client has no %s option", key)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", declinef(stage, nonString, "selected client %s option is empty or not a resolved literal string", key)
	}
	return s, nil
}

// classifyClientOption maps an unproven client-option key to its fixed reason,
// mirroring the body-affecting classification the native body builder uses. The
// key NAME is safe to surface; option values are never read here.
func classifyClientOption(key string) Reason {
	switch key {
	case "headers":
		return ReasonHeadersOption
	case "tools", "tool_choice", "functions", "function_call", "parallel_tool_calls":
		return ReasonToolsOption
	case "response_format":
		return ReasonResponseFormatOption
	case "request_body":
		return ReasonRequestBodyOption
	default:
		return ReasonUnprovenClientOption
	}
}
