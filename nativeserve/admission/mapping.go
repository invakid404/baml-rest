package admission

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// validJSONNumber reports whether n is a well-formed, FINITE JSON number. It is the
// single source of truth for number validity across asFloat64 / asInt64 /
// deepCopyJSONValue — json.Number.Float64() / Int64() alone are NOT JSON validation
// (the strconv parsers they wrap accept NaN, Inf, and non-JSON spellings like "01",
// "+1", ".5", "1."). It rejects those by requiring the token to (a) satisfy the
// JSON grammar via encoding/json, (b) actually BE a number (start with '-' or a
// digit, so a valid non-number JSON token like `true` is rejected), and (c) parse
// to a finite float64 (so a grammatically-valid overflow like 1e400 -> +Inf declines).
func validJSONNumber(n json.Number) bool {
	s := string(n)
	if s == "" {
		return false
	}
	if !json.Valid([]byte(s)) {
		return false
	}
	if c := s[0]; c != '-' && (c < '0' || c > '9') {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	return true
}

// This file holds the S2 TRUSTED-provider mapping: the common bearer mapper
// (anthropic / cerebras / cohere / any other nonempty provider), the supported
// common body-field projection (§4.4), and the request-local private Env
// placeholder plumbing (§4.3) that carries every resolved secret to nanollm
// WITHOUT letting the engine read the ambient process environment. The Bedrock
// special mapper lives in mapping_bedrock.go; both produce a *mappedClient carrying
// PolicyTrustedProvider (never a BAML comparison — §6).

// Private, FIXED Env placeholder keys (§4.3). Operator-controlled names are NEVER
// used as a placeholder key, and there is exactly ONE model per request-scoped
// config, so fixed keys never collide. Each resolved secret is stored in
// Config.Env under one of these keys and referenced from the config via
// envPlaceholder(); nanollm interpolation is single-pass, so a resolved value that
// itself contains a literal ${...} stays literal.
const (
	envKeyAPIKey              = "__BAML_REST_NATIVE_API_KEY"
	envKeyBaseURL             = "__BAML_REST_NATIVE_BASE_URL"
	envKeyBedrockRegion       = "__BAML_REST_NATIVE_BEDROCK_REGION"
	envKeyBedrockAccessKeyID  = "__BAML_REST_NATIVE_BEDROCK_ACCESS_KEY_ID"
	envKeyBedrockSecretKey    = "__BAML_REST_NATIVE_BEDROCK_SECRET_ACCESS_KEY"
	envKeyBedrockSessionToken = "__BAML_REST_NATIVE_BEDROCK_SESSION_TOKEN"
)

// envPlaceholder wraps a private Env key as the ${...} reference nanollm resolves
// against Config.Env.
func envPlaceholder(key string) string { return "${" + key + "}" }

// bodyOptions is the resolved, type-validated supported common body-field subset
// (§4.4) a trusted provider projects onto the typed nanollm ChatRequest. Every
// modeled field is a pointer (nil == absent, so the field is omitted); extra holds
// the small set of common-but-unmodeled fields (max_completion_tokens / top_k /
// anthropic_beta) placed on ChatRequest.Extra. NEVER carries a secret.
type bodyOptions struct {
	temperature      *float64
	maxTokens        *int
	topP             *float64
	n                *int
	seed             *int64
	frequencyPenalty *float64
	presencePenalty  *float64
	user             *string
	logprobs         *bool
	stop             []string
	extra            map[string]any
}

// setExtra records one unmodeled body field for ChatRequest.Extra.
func (b *bodyOptions) setExtra(key string, v any) {
	if b.extra == nil {
		b.extra = make(map[string]any, 4)
	}
	b.extra[key] = v
}

// applyBodyOptions projects the resolved options onto a nanollm ChatRequest. It
// assigns modeled keys through the typed fields and unmodeled keys through Extra —
// never colliding with model/messages/stream (ChatRequest.Build errors on an Extra
// key that collides with a typed field, an additional fail-closed guard).
func applyBodyOptions(r *nanollm.ChatRequest, b bodyOptions) {
	r.Temperature = b.temperature
	r.MaxTokens = b.maxTokens
	r.TopP = b.topP
	r.N = b.n
	r.Seed = b.seed
	r.FrequencyPenalty = b.frequencyPenalty
	r.PresencePenalty = b.presencePenalty
	r.Logprobs = b.logprobs
	if b.user != nil {
		r.User = *b.user
	}
	if len(b.stop) > 0 {
		r.Stop = b.stop
	}
	if len(b.extra) > 0 {
		if r.Extra == nil {
			r.Extra = make(map[string]any, len(b.extra))
		}
		for k, v := range b.extra {
			r.Extra[k] = v
		}
	}
}

// mapCommonBearer is the COMMON BEARER mapper (§4.5): anthropic / cerebras /
// cohere / any other nonempty provider that authenticates with a bearer/api-key
// header and speaks an OpenAI-shaped chat body. It resolves the nanollm model
// `<provider>/<model>`, the api key (explicit option or the documented
// <PROVIDER>_API_KEY env var), an optional nanollm-style base_url (anthropic's is
// service-root->API-root adapted), an optional string header map, and the
// supported common body-field subset. Every unknown/control/malformed option
// declines with a stable, secret-free reason — this is option mapping, NOT a
// provider allowlist. It assigns PolicyTrustedProvider (never a BAML comparison).
//
// Cohere is DEFERRED in nanollm v0.4.3 (embeddings-only): it flows through here
// unchanged and auto-declines PRE-socket at nanollm.New/Prepare (a typed
// unsupported/invalid-provider code) — there is deliberately NO cohere-specific
// mapping. A later upstream P0 adds cohere v2 chat and it then auto-admits.
func mapCommonBearer(cp *bamlutils.ClientProperty, in mappingInput, registryProvider, nanollmProvider string) (*mappedClient, *Decline, error) {
	var (
		target  string
		apiKey  string
		baseURL string
		headers map[string]string
		body    bodyOptions
	)
	// Scan options in a stable sorted order so a client carrying several unproven
	// options always declines on the same (first) one.
	for _, key := range sortedOptionKeys(cp.Options) {
		raw := cp.Options[key]
		switch key {
		case "model":
			s, ok := raw.(string)
			if !ok || s == "" {
				return nil, declinef(StageClientSelection, ReasonModelNotLiteral,
					"selected client model option is empty or not a resolved literal string"), nil
			}
			target = s
		case "api_key":
			s, ok := raw.(string)
			if !ok || s == "" {
				return nil, declinef(StageCredentialSource, ReasonAPIKeyAbsent,
					"selected client api_key option is empty or not a resolved literal string"), nil
			}
			apiKey = s
		case "base_url":
			s, ok := raw.(string)
			if !ok || s == "" || !utf8.ValidString(s) {
				return nil, declinef(StageCredentialSource, ReasonInvalidBaseURL,
					"selected client base_url option is empty, non-string, or not valid UTF-8"), nil
			}
			baseURL = s
		case "headers":
			h, dec := parseHeaderMap(raw)
			if dec != nil {
				return nil, dec, nil
			}
			headers = h
		default:
			if dec := applyOneBodyOption(&body, key, raw, nanollmProvider); dec != nil {
				return nil, dec, nil
			}
		}
	}

	if target == "" {
		return nil, declinef(StageClientSelection, ReasonModelAbsent,
			"selected client has no resolved literal target model"), nil
	}
	// Require a valid UTF-8 target model, matching the strict OpenAI path (BAML
	// rejects invalid UTF-8 at its protobuf/CFFI boundary and builds no request).
	if !utf8.ValidString(target) {
		return nil, declinef(StageClientSelection, ReasonModelNotLiteral,
			"target model is not valid UTF-8"), nil
	}

	// API key: explicit option, else the documented <PROVIDER>_API_KEY env var
	// (baml-rest resolves the documented source; nanollm never reads ambient env —
	// the resolved value is passed via a private Env placeholder with
	// UseProcessEnv=false). A missing key is a confident credential decline.
	//
	// SECURITY: the <PROVIDER>_API_KEY environment fallback is resolved ONLY when NO
	// base_url is configured — i.e. the provider's default service root. A dynamic
	// registry can choose an ARBITRARY base_url, so filling an absent api_key from
	// the host environment when a custom base_url is set would send the host's
	// credential to a wire-controlled destination. When a non-empty base_url is
	// configured an EXPLICIT api_key is REQUIRED; otherwise decline pre-socket — the
	// env credential is never read and no client/socket is constructed.
	if apiKey == "" && baseURL != "" {
		return nil, declinef(StageCredentialSource, ReasonExplicitAPIKeyRequiredForCustomBase,
			"a custom base_url requires an explicit api_key; the documented <PROVIDER>_API_KEY environment fallback is never sent to a wire-configured base_url"), nil
	}
	if apiKey == "" {
		if v := os.Getenv(providerAPIKeyEnv(registryProvider)); v != "" {
			apiKey = v
		}
	}
	if apiKey == "" {
		return nil, declinef(StageCredentialSource, ReasonMissingCredential,
			"no api key: neither an explicit api_key option nor the documented <PROVIDER>_API_KEY env var is set"), nil
	}
	// A non-UTF-8 resolved key (option or env) would break the config JSON marshal;
	// take the stable pre-FFI decline (the value is never surfaced).
	if !utf8.ValidString(apiKey) {
		return nil, declinef(StageCredentialSource, ReasonAPIKeyAbsent,
			"resolved api key is not valid UTF-8"), nil
	}

	// Anthropic base-URL adapter (§4.5): BAML stores the service root and appends
	// /v1/messages; nanollm's base is the API root and appends /messages, so a BAML
	// registry base_url is normalized (one trailing slash) and gets /v1 appended.
	if baseURL != "" && nanollmProvider == providerNanollmAnthropic {
		baseURL = anthropicNanollmBase(baseURL)
	}

	if dec := validateAlias(in.alias, target, cp.Name); dec != nil {
		return nil, dec, nil
	}

	return &mappedClient{
		registryProvider: registryProvider,
		nanollmProvider:  nanollmProvider,
		target:           target,
		alias:            in.alias,
		baseURL:          baseURL,
		apiKey:           apiKey,
		headers:          headers,
		body:             body,
		verification:     PolicyTrustedProvider,
	}, nil, nil
}

// applyOneBodyOption validates one supported common body option and records it on
// b, or declines. It is PROVIDER-AWARE (§4.4): the documented common subset is
// accepted for every common-bearer provider, but the provider-specific extensions
// top_k / anthropic_beta are gated to their supported provider (anthropic) — a
// cerebras/cohere/unknown registry option that is not in the proven common subset
// declines rather than being emitted unproven into ChatRequest.Extra (trusted
// providers have NO BAML comparison to catch a resulting body mismatch). A
// supported key with a malformed value declines invalid_body_option; a known
// control option (tools/response_format/request_body) declines with its specific
// reason; anything else declines unproven_client_option. The request-controlled
// option value is NEVER surfaced in the decline detail.
func applyOneBodyOption(b *bodyOptions, key string, raw any, provider string) *Decline {
	invalid := func() *Decline {
		return declinef(StageClientOption, ReasonInvalidBodyOption,
			"selected client carries a supported body option with a malformed or wrong-typed value")
	}
	// providerExtension gates a provider-specific extension to the providers that
	// support it; on any other provider the option is unproven and declines.
	providerExtension := func(supported ...string) *Decline {
		for _, p := range supported {
			if provider == p {
				return nil
			}
		}
		return declinef(StageClientOption, ReasonUnprovenClientOption,
			"selected client carries a provider-specific extension not proven for this provider")
	}
	switch key {
	case "temperature":
		f, ok := asFloat64(raw)
		if !ok {
			return invalid()
		}
		b.temperature = &f
	case "top_p":
		f, ok := asFloat64(raw)
		if !ok {
			return invalid()
		}
		b.topP = &f
	case "frequency_penalty":
		f, ok := asFloat64(raw)
		if !ok {
			return invalid()
		}
		b.frequencyPenalty = &f
	case "presence_penalty":
		f, ok := asFloat64(raw)
		if !ok {
			return invalid()
		}
		b.presencePenalty = &f
	case "max_tokens":
		// Count-like: reject only NEGATIVE values (unambiguously malformed — no
		// provider schema accepts a negative count). Zero is CONTRACT-VALID for an
		// admitted provider (the pinned Anthropic OpenAPI fixture declares max_tokens
		// minimum 0 and documents 0 for prompt-cache pre-warming; BAML's Anthropic
		// builder has no positive guard), so pre-declining 0 would over-decline a valid
		// config. A value a specific provider forbids at runtime is that provider's
		// transport contract to reject — trusted providers own it and get no BAML compare.
		n, ok := asInt(raw)
		if !ok || n < 0 {
			return invalid()
		}
		b.maxTokens = &n
	case "n":
		n, ok := asInt(raw)
		if !ok || n < 0 {
			return invalid()
		}
		b.n = &n
	case "seed":
		i, ok := asInt64(raw)
		if !ok {
			return invalid()
		}
		b.seed = &i
	case "logprobs":
		v, ok := raw.(bool)
		if !ok {
			return invalid()
		}
		b.logprobs = &v
	case "user":
		s, ok := raw.(string)
		if !ok || s == "" || !utf8.ValidString(s) {
			return invalid()
		}
		b.user = &s
	case "stop":
		ss, ok := asStringSlice(raw)
		if !ok || len(ss) == 0 {
			return invalid()
		}
		b.stop = ss
	// max_completion_tokens is part of the documented common subset (every
	// common-bearer provider) — carried through ChatRequest.Extra (unmodeled).
	// Count-like: reject only negative (see max_tokens rationale).
	case "max_completion_tokens":
		n, ok := asInt(raw)
		if !ok || n < 0 {
			return invalid()
		}
		b.setExtra(key, n)
	// top_k / anthropic_beta are PROVIDER-SPECIFIC extensions (§4.4): admitted only
	// for anthropic through the common-bearer path, declined for every other provider.
	case "top_k":
		if dec := providerExtension(providerNanollmAnthropic); dec != nil {
			return dec
		}
		// Count-like: reject only negative. The pinned Anthropic top_k schema declares
		// minimum 0, so top_k: 0 is contract-valid and must not decline as malformed.
		n, ok := asInt(raw)
		if !ok || n < 0 {
			return invalid()
		}
		b.setExtra(key, n)
	case "anthropic_beta":
		if dec := providerExtension(providerNanollmAnthropic); dec != nil {
			return dec
		}
		ss, ok := asStringSlice(raw)
		if !ok || len(ss) == 0 {
			return invalid()
		}
		b.setExtra(key, ss)
	// Known control options are DECLINED with their specific reason (until the whole
	// request/response/SAP behavior is explicitly represented) — never silently dropped.
	case "tools", "tool_choice", "functions", "function_call", "parallel_tool_calls":
		return declinef(StageClientOption, ReasonToolsOption,
			"selected client carries an unproven tools/functions option")
	case "response_format":
		return declinef(StageClientOption, ReasonResponseFormatOption,
			"selected client carries an unproven response_format option")
	case "request_body":
		return declinef(StageClientOption, ReasonRequestBodyOption,
			"selected client carries a request_body passthrough")
	default:
		return declinef(StageClientOption, ReasonUnprovenClientOption,
			"selected client carries an unproven option beyond the supported common body subset")
	}
	return nil
}

// parseHeaderMap validates a client `headers` option as a string->string map and
// returns an owned copy (nil for an absent/empty map). It declines
// invalid_headers_option for a non-map option, a non-string value, an empty name,
// a non-UTF-8 name/value, OR a name/value that fails the HTTP field grammar — the
// SAME exact-transport grammar checks (llmhttp.ValidHeaderName / ValidHeaderValue)
// the prepared-plan header scan applies — so a header name with a separator/control
// byte or a value with an embedded CR/LF (a header-injection vector) fails closed
// BEFORE the custom header map ever reaches nanollm.New/FFI. Header names and
// VALUES are never surfaced in the decline detail.
func parseHeaderMap(raw any) (map[string]string, *Decline) {
	invalid := func() *Decline {
		return declinef(StageClientOption, ReasonInvalidHeadersOption,
			"selected client headers option is not a valid string->string HTTP header map")
	}
	admit := func(k, val string) bool {
		return k != "" && utf8.ValidString(k) && utf8.ValidString(val) &&
			llmhttp.ValidHeaderName(k) && llmhttp.ValidHeaderValue(val)
	}
	out := map[string]string{}
	switch v := raw.(type) {
	case map[string]string:
		for k, val := range v {
			if !admit(k, val) {
				return nil, invalid()
			}
			out[k] = val
		}
	case map[string]any:
		for k, val := range v {
			s, ok := val.(string)
			if !ok || !admit(k, s) {
				return nil, invalid()
			}
			out[k] = s
		}
	default:
		return nil, invalid()
	}
	if len(out) == 0 {
		// An explicit empty headers map carries no custom header — treat as absent.
		return nil, nil
	}
	return out, nil
}

// newTrustedConfig builds the nanollm Config for a TRUSTED mapped client (§4.3):
// ONE model under the internal alias, the nanollm-spelled provider + resolved
// target, a zero retry budget, and every resolved secret routed through a private
// Config.Env placeholder with UseProcessEnv=false so nanollm never reads the
// ambient process environment. Custom headers and Bedrock additional-model-request
// fields are NESTED params (nanollm does not interpolate nested JSON), so they pass
// through literally; the Bedrock region + credential literals are top-level params
// string values, which nanollm DOES interpolate, so they use placeholders too.
func newTrustedConfig(m *mappedClient) nanollm.Config {
	env := make(map[string]string, 6)
	mc := nanollm.ModelConfig{
		Name:       m.alias,
		Model:      m.nanollmProvider + "/" + m.target,
		MaxRetries: 0,
	}
	if m.apiKey != "" {
		env[envKeyAPIKey] = m.apiKey
		mc.APIKey = envPlaceholder(envKeyAPIKey)
	}
	if m.baseURL != "" {
		env[envKeyBaseURL] = m.baseURL
		mc.BaseURL = envPlaceholder(envKeyBaseURL)
	}

	params := map[string]any{}
	if len(m.headers) > 0 {
		// A nested string map: nanollm injects these header values verbatim (no
		// interpolation of nested JSON), so a literal ${...} in a header value stays literal.
		hdr := make(map[string]string, len(m.headers))
		for k, v := range m.headers {
			hdr[k] = v
		}
		params["headers"] = hdr
	}
	if m.bedrock != nil {
		b := m.bedrock
		env[envKeyBedrockRegion] = b.region
		params["region"] = envPlaceholder(envKeyBedrockRegion)
		env[envKeyBedrockAccessKeyID] = b.accessKeyID
		params["access_key_id"] = envPlaceholder(envKeyBedrockAccessKeyID)
		env[envKeyBedrockSecretKey] = b.secretAccessKey
		params["secret_access_key"] = envPlaceholder(envKeyBedrockSecretKey)
		if b.sessionToken != "" {
			env[envKeyBedrockSessionToken] = b.sessionToken
			params["session_token"] = envPlaceholder(envKeyBedrockSessionToken)
		}
		if len(b.additionalModelRequestFields) > 0 {
			params["additional_model_request_fields"] = b.additionalModelRequestFields
		}
	}
	if len(params) > 0 {
		mc.Params = params
	}

	return nanollm.Config{
		Models:        []nanollm.ModelConfig{mc},
		Env:           env,
		UseProcessEnv: false,
	}
}

// providerAPIKeyEnv is the documented default api-key environment variable for a
// bearer provider: the uppercased provider spelling (hyphens folded to
// underscores) with a _API_KEY suffix, e.g. anthropic -> ANTHROPIC_API_KEY,
// cerebras -> CEREBRAS_API_KEY, cohere -> COHERE_API_KEY.
func providerAPIKeyEnv(provider string) string {
	up := strings.ToUpper(provider)
	up = strings.ReplaceAll(up, "-", "_")
	return up + "_API_KEY"
}

// anthropicNanollmBase adapts a BAML anthropic registry base_url (the SERVICE root,
// which BAML appends /v1/messages to) into nanollm's base (the API root, which
// nanollm appends /messages to): normalize one trailing slash as BAML does, then
// append /v1. A wrong /v1 here is OUR bug, so it is pinned by mapping tests.
func anthropicNanollmBase(bamlBase string) string {
	return strings.TrimSuffix(bamlBase, "/") + "/v1"
}

// sortedOptionKeys returns the option keys in a stable sorted order so a decline is
// deterministic regardless of map iteration order.
func sortedOptionKeys(opts map[string]any) []string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- JSON-value coercion helpers (registry Options are map[string]any; values
// arrive as native Go types in tests or float64/json.Number when JSON-decoded) ---

// asFloat64 coerces a registry option value to a FINITE float64 (JSON has no
// NaN/Inf; a non-finite value is not valid JSON and would fail serialization, so
// it declines pre-FFI rather than crossing the mapping boundary).
func asFloat64(raw any) (float64, bool) {
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case float32:
		f = float64(v)
	case int:
		f = float64(v)
	case int32:
		f = float64(v)
	case int64:
		f = float64(v)
	case json.Number:
		if !validJSONNumber(v) {
			return 0, false
		}
		ff, err := v.Float64()
		if err != nil {
			return 0, false
		}
		f = ff
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func asInt(raw any) (int, bool) {
	i, ok := asInt64(raw)
	return int(i), ok
}

func asInt64(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		// Finite, integral, AND in int64 range — a huge integral float (e.g. 1e300)
		// is not representable as int64 and would be an undefined conversion, so it
		// declines rather than silently wrapping.
		if math.IsInf(v, 0) || math.IsNaN(v) || v != math.Trunc(v) {
			return 0, false
		}
		if v >= float64(math.MaxInt64) || v < float64(math.MinInt64) {
			return 0, false
		}
		return int64(v), true
	case float32:
		// Widen and reuse the float64 guard so ±Inf / NaN / out-of-range reject consistently.
		return asInt64(float64(v))
	case json.Number:
		// Reject non-JSON spellings (NaN/Inf/"01"/"+1") FIRST — Int64() alone accepts
		// e.g. "01".
		if !validJSONNumber(v) {
			return 0, false
		}
		// EXACT int64 range validation via arbitrary-precision integer parsing — a
		// Float64() round-trip must NOT be used for range: a negative out-of-range
		// literal like "-9223372036854775809" rounds to exactly float64(MinInt64) and
		// would slip past a float lower-bound guard. big.Int.SetString(base 10) parses
		// a plain integer literal exactly (validJSONNumber already rejected the
		// non-integer/exponent grammar cases it does not accept), and IsInt64 is an
		// exact bounds check — so the exact MinInt64/MaxInt64 literals are accepted and
		// one-below-min / one-above-max (and non-integral spellings) decline.
		bi, ok := new(big.Int).SetString(string(v), 10)
		if !ok || !bi.IsInt64() {
			return 0, false
		}
		return bi.Int64(), true
	default:
		return 0, false
	}
}

// asStringSlice coerces a stop/anthropic_beta option to a string slice: a single
// string becomes a one-element slice; a []string / []any of strings is copied. Any
// non-string or non-UTF-8 element declines (a non-UTF-8 string is not a valid JSON
// string and would break serialization downstream).
func asStringSlice(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case string:
		if !utf8.ValidString(v) {
			return nil, false
		}
		return []string{v}, true
	case []string:
		for _, e := range v {
			if !utf8.ValidString(e) {
				return nil, false
			}
		}
		return append([]string(nil), v...), true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok || !utf8.ValidString(s) {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}
