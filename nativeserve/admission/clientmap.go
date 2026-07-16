package admission

import (
	"fmt"
	"sort"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// clientFacts are the secret-free, body-relevant facts the mapper resolves from
// the selected client. baseURL is retained ONLY for the prepared-plan URL check
// (base + /chat/completions); the api key is written straight into the engine
// config and is NEVER retained, returned, or logged. target is the resolved
// literal OpenAI model — never the internal nanollm alias.
type clientFacts struct {
	provider string
	target   string
	baseURL  string
}

// transportTrio is the ONLY option set the request-scoped mapper accepts: the
// proved transport trio. Every other option — headers, tools, response_format,
// request_body, temperature, or anything unrecognized — declines at
// StageClientOption. There is deliberately no headers passthrough, no default
// credential chain, and no option beyond these three.
var transportTrio = map[string]struct{}{
	"model":    {},
	"base_url": {},
	"api_key":  {},
}

// mapDynamicClient resolves the effective one-client dynamic registry into a
// request-scoped nanollm engine plus the facts the predicate needs, or a stable
// Decline. It enforces, else declines:
//
//   - the registry validates and resolves EXACTLY ONE unambiguous non-nil client,
//     named by primary when a primary is present;
//   - no client-retry policy on the selected client;
//   - provider is exactly openai;
//   - options are exactly the transport trio — model (a resolved literal target),
//     base_url, and api_key — each a present, non-empty string, and nothing else.
//
// It configures nanollm with a SEPARATE internal alias, Model "openai/"+target,
// MaxRetries 0, no fallbacks, Env nil, and UseProcessEnv false — so no ambient/
// process-env value and no default credential chain can mask a difference. The
// values may be real in production; they are never logged (the proof is
// structural, with fake values in tests).
//
// The returned client is OPEN; the caller owns Close. An unexpected engine
// construction failure returns a non-decline error (a planner error), not a
// parity-decline, so it can be alerted instead of counted as ordinary
// unsupported traffic.
func mapDynamicClient(reg *bamlutils.ClientRegistry, alias string, wouldRewriteOrProxy func(effectiveURL string) bool) (*nanollm.Client, clientFacts, *Decline, error) {
	cp, dec := selectOneClient(reg)
	if dec != nil {
		return nil, clientFacts{}, dec, nil
	}

	// A per-client retry policy is a strategy the initial matrix does not prove
	// (baml-rest owns the retry budget); decline at the strategy stage.
	if cp.RetryPolicy != nil {
		return nil, clientFacts{}, declinef(StageStrategy, ReasonClientRetryPolicy,
			"selected client carries a retry_policy; the initial matrix proves no client-retry policy"), nil
	}

	if cp.Provider != nativebody.ProviderOpenAI {
		return nil, clientFacts{}, declinef(StageProvider, ReasonProviderNotOpenAI,
			"selected client provider is not the admitted openai surface"), nil
	}

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
		return nil, clientFacts{}, declinef(StageClientOption, classifyClientOption(extra[0]),
			"selected client carries an unproven option beyond the transport trio"), nil
	}

	// The transport trio: model is a resolved literal target (StageClientSelection);
	// base_url and api_key are the credential source (StageCredentialSource). Each
	// must be a present, non-empty string.
	target, dec := trioString(cp, "model", StageClientSelection, ReasonModelAbsent, ReasonModelNotLiteral)
	if dec != nil {
		return nil, clientFacts{}, dec, nil
	}
	baseURL, dec := trioString(cp, "base_url", StageCredentialSource, ReasonBaseURLAbsent, ReasonBaseURLAbsent)
	if dec != nil {
		return nil, clientFacts{}, dec, nil
	}
	apiKey, dec := trioString(cp, "api_key", StageCredentialSource, ReasonAPIKeyAbsent, ReasonAPIKeyAbsent)
	if dec != nil {
		return nil, clientFacts{}, dec, nil
	}

	// The required SEPARATE internal alias: non-empty and distinct from BOTH the
	// resolved target model and the selected client name. Enforced BEFORE
	// nanollm.New so a colliding alias never configures the engine — an
	// alias == target would also make the later plan alias/target equality checks
	// tautological, and alias == client name would blur the internal alias with
	// the operator-visible client identity.
	if alias == "" || alias == target || alias == cp.Name {
		return nil, clientFacts{}, declinef(StageClientSelection, ReasonInvalidAlias,
			"internal alias is empty or collides with the resolved target model or the selected client name"), nil
	}

	// Effective send-path rewrite/proxy parity — evaluated NOW that the effective
	// target is known (base_url + /chat/completions) and BEFORE the engine is
	// constructed. BAML's llmhttp client applies URL rewrites and HTTP proxying at
	// EXECUTION time, on the built request — AFTER the plan the comparator captures.
	// wouldRewriteOrProxy is the send client's own rewrite config plus its
	// transport's own Proxy resolver evaluated against THAT exact target — the same
	// function (and cached env snapshot) the transport uses at send — so a rewrite
	// or a proxied target is an unproven shape that declines here, before the engine,
	// the prepared plan, BAML's plan, or a plan_compare. A nil predicate (lightweight
	// tests that carry no send client) skips the check.
	if wouldRewriteOrProxy != nil && wouldRewriteOrProxy(baseURL+chatCompletionsPath) {
		return nil, clientFacts{}, declinef(StageStrategy, ReasonURLRewriteOrProxy,
			"the effective send path would rewrite or proxy the request target"), nil
	}

	// Request-scoped engine: ONE openai model under the internal alias, the
	// resolved target, a zero retry budget, and no fallbacks. Env nil +
	// UseProcessEnv false forbid every ambient/process-env resolution. No
	// headers option, no transforms, no option beyond the proved transport trio.
	client, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       alias,
			Model:      "openai/" + target,
			APIKey:     apiKey,
			BaseURL:    baseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		// Unexpected native construction failure — a planner error, not a
		// parity-decline. Secret-free: nanollm.New's error never echoes the key.
		return nil, clientFacts{}, nil, fmt.Errorf("nativeserve/admission: nanollm.New: %w", err)
	}

	return client, clientFacts{provider: cp.Provider, target: target, baseURL: baseURL}, nil, nil
}

// NewResponseClient rebuilds the request-scoped nanollm engine for the SAME
// effective client the plan phase admitted, so the de-BAML same-response shadow
// oracle can call TranslateResponse(alias, …) on BAML's already-fetched response.
// It reuses the EXACT mapping mapDynamicClient uses (identical alias -> openai/
// target config, zero retries, no ambient env), so the translation is faithful to
// what the admitted plan would have translated with — but it performs NO
// rewrite/proxy check (that gate already decided routing in the plan phase and is
// irrelevant to a pure response translation) and NO Prepare (no plan is built or
// sent). The returned client is OPEN; the caller owns Close. It never opens a
// socket. It is only invoked AFTER the plan phase already admitted this exact
// client, so a decline/error here is unexpected and surfaces as an error.
func NewResponseClient(reg *bamlutils.ClientRegistry, alias string) (*nanollm.Client, string, error) {
	client, facts, dec, err := mapDynamicClient(reg, alias, nil)
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
