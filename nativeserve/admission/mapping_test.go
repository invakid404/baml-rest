package admission

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// S2 pure-mapping coverage (default suite, NO FFI, NO socket): it exercises
// mapClientConfig, the pure newTrustedConfig (the Env-placeholder / Params builder,
// which never calls nanollm.New), and applyBodyOptions, for the common bearer and
// Bedrock mappers plus every stable decline row. It proves OUR registry->nanollm
// plumbing — not nanollm-vs-BAML.

const trustedAlias = "__s2_trusted_alias__"

// mapReg builds a one-client dynamic registry for provider p with options opts.
func mapReg(name, p string, opts map[string]any) *bamlutils.ClientRegistry {
	return &bamlutils.ClientRegistry{
		Primary: s1sp(name),
		Clients: []*bamlutils.ClientProperty{{Name: name, Provider: p, Options: opts}},
	}
}

// mustMap resolves a registry to a *mappedClient, failing on a decline/error.
func mustMap(t *testing.T, reg *bamlutils.ClientRegistry, provider string) *mappedClient {
	t.Helper()
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: reg, alias: trustedAlias, resolvedProvider: provider})
	if err != nil {
		t.Fatalf("planner error: %v", err)
	}
	if dec != nil {
		t.Fatalf("unexpected decline: %v", dec)
	}
	if m == nil {
		t.Fatal("mappedClient is nil")
	}
	return m
}

// wantMapDecline resolves a registry and asserts it declines at (stage, reason)
// without leaking any secret substring.
func wantMapDecline(t *testing.T, reg *bamlutils.ClientRegistry, provider string, stage Stage, reason Reason) *Decline {
	t.Helper()
	m, dec, err := mapClientConfig(context.Background(), mappingInput{registry: reg, alias: trustedAlias, resolvedProvider: provider})
	if err != nil {
		t.Fatalf("planner error: %v", err)
	}
	if m != nil {
		// NEVER format the mappedClient — it can embed an api key / Bedrock credential
		// literals, which would leak into CI failure output. Report only the fact.
		t.Fatal("mappedClient must be nil on a decline (a non-nil mapped client may embed secrets; not printed)")
	}
	if dec == nil || dec.Stage != stage || dec.Reason != reason {
		t.Fatalf("decline = %v, want %s/%s", dec, stage, reason)
	}
	for _, secret := range []string{"SECRET", "sk-", "AKIA", "topsecret"} {
		if strings.Contains(dec.Detail, secret) {
			// NEVER print dec.Detail — it contains the leaked value. Report only the
			// forbidden token that matched (a fixed needle from this list, not a secret).
			t.Fatalf("decline detail contained the forbidden token %q (detail redacted)", secret)
		}
	}
	return dec
}

// --- Anthropic base-URL adapter (§4.5) ---

func TestMapAnthropic_BaseURLAdapter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://api.anthropic.com", "https://api.anthropic.com/v1"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1"},
		{"http://127.0.0.1:9099", "http://127.0.0.1:9099/v1"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			m := mustMap(t, mapReg("A", "anthropic", map[string]any{
				"model":    "claude-x",
				"api_key":  "sk-ant-fake",
				"base_url": tc.in,
			}), "anthropic")
			if m.verification != PolicyTrustedProvider {
				t.Errorf("policy = %v, want trusted_provider", m.verification)
			}
			if m.baseURL != tc.want {
				// URL class: report the redacted view (host+path; query/userinfo stripped).
				t.Errorf("adapted base = %q, want %q", llmhttp.RedactedURL(m.baseURL), llmhttp.RedactedURL(tc.want))
			}
			// A wrong /v1 is OUR bug — assert the placeholder plumbing carries it.
			cfg := newTrustedConfig(m)
			if got := cfg.Env[envKeyBaseURL]; got != tc.want {
				t.Errorf("Env base = %q, want %q", llmhttp.RedactedURL(got), llmhttp.RedactedURL(tc.want))
			}
			if cfg.Models[0].BaseURL != envPlaceholder(envKeyBaseURL) {
				t.Error("ModelConfig.BaseURL is not the expected private ${...} placeholder")
			}
		})
	}
}

// A non-anthropic bearer provider (cerebras) does NOT get the /v1 adapter — its
// base_url is the nanollm API base verbatim.
func TestMapCerebras_BaseURLNotAdapted(t *testing.T) {
	m := mustMap(t, mapReg("C", "cerebras", map[string]any{
		"model":    "llama-x",
		"api_key":  "sk-cb-fake",
		"base_url": "http://127.0.0.1:9099/v1",
	}), "cerebras")
	if m.baseURL != "http://127.0.0.1:9099/v1" {
		t.Errorf("cerebras base = %q, want the verbatim nanollm API base", llmhttp.RedactedURL(m.baseURL))
	}
	if m.nanollmProvider != "cerebras" {
		t.Errorf("nanollm provider = %q, want cerebras", m.nanollmProvider)
	}
}

// --- api-key resolution: explicit, <PROVIDER>_API_KEY env, and missing ---

func TestMapBearer_APIKeyEnvFallback(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "sk-env-fallback")
	m := mustMap(t, mapReg("C", "cerebras", map[string]any{"model": "m"}), "cerebras")
	if m.apiKey != "sk-env-fallback" {
		// Never print the resolved key (secret hygiene, even for a fake).
		t.Error("api key was not resolved from the CEREBRAS_API_KEY env fallback")
	}
}

func TestMapBearer_MissingKeyDeclines(t *testing.T) {
	// Ensure the documented env var is unset for this provider.
	t.Setenv("ANTHROPIC_API_KEY", "")
	wantMapDecline(t, mapReg("A", "anthropic", map[string]any{"model": "m"}), "anthropic",
		StageCredentialSource, ReasonMissingCredential)
}

// TestMapBearer_CustomBaseURLRequiresExplicitKey is the CR-A security regression: a
// dynamic registry can point base_url at an arbitrary (attacker-controlled) host, so
// the documented <PROVIDER>_API_KEY environment fallback must NEVER be resolved when
// a custom base_url is configured. With BOTH env fallbacks set but a custom base_url
// and NO explicit api_key, the mapper declines PRE-socket (the env credential is
// never read, no client is constructed, no socket opens). The env values embed
// recognizable secret tokens to prove none reaches the (secret-scanned) decline.
func TestMapBearer_CustomBaseURLRequiresExplicitKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-SECRET")
	t.Setenv("CEREBRAS_API_KEY", "sk-cb-env-SECRET")

	const attacker = "https://attacker.example.com/v1"

	// Negative: custom base_url + NO explicit api_key => decline, never send the env key.
	for _, provider := range []string{"anthropic", "cerebras"} {
		t.Run("decline/"+provider, func(t *testing.T) {
			wantMapDecline(t, mapReg("X", provider, map[string]any{
				"model":    "m",
				"base_url": attacker,
			}), provider, StageCredentialSource, ReasonExplicitAPIKeyRequiredForCustomBase)
		})
	}

	// Positive 1: an EXPLICIT api_key alongside a custom base_url still maps — the
	// operator-chosen key is used, never the ambient env credential.
	t.Run("explicit-key+custom-base maps", func(t *testing.T) {
		m := mustMap(t, mapReg("C", "cerebras", map[string]any{
			"model":    "m",
			"api_key":  "sk-cb-explicit",
			"base_url": attacker,
		}), "cerebras")
		if m.verification != PolicyTrustedProvider {
			t.Errorf("policy = %v, want trusted_provider", m.verification)
		}
	})

	// Positive 2: the documented env fallback IS used when NO base_url is configured
	// (the provider's default service root) — the safe, unchanged case.
	t.Run("env-key+no-base maps", func(t *testing.T) {
		m := mustMap(t, mapReg("A", "anthropic", map[string]any{"model": "m"}), "anthropic")
		if m.apiKey == "" {
			// Never print the resolved key (secret hygiene, even for a fake).
			t.Error("env api_key fallback was not resolved for the default (no base_url) case")
		}
	})
}

// --- common body-option projection: typed fields, Extra, declines ---

// TestMapBearer_BodyOptionProjection covers the documented COMMON subset (accepted
// for every common-bearer provider incl. cerebras) — NOT the provider-specific
// extensions top_k / anthropic_beta (those are covered separately).
func TestMapBearer_BodyOptionProjection(t *testing.T) {
	m := mustMap(t, mapReg("C", "cerebras", map[string]any{
		"model":             "m",
		"api_key":           "k",
		"temperature":       0.7,
		"max_tokens":        256,
		"top_p":             0.95,
		"n":                 1,
		"seed":              42,
		"frequency_penalty": 0.1,
		"presence_penalty":  0.2,
		"user":              "u-1",
		"logprobs":          true,
		"stop":              []any{"STOP", "END"},
		// max_completion_tokens is common (every bearer provider) -> Extra
		"max_completion_tokens": 512,
	}), "cerebras")

	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.Temperature == nil || *r.Temperature != 0.7 {
		t.Errorf("temperature = %v, want 0.7", r.Temperature)
	}
	if r.MaxTokens == nil || *r.MaxTokens != 256 {
		t.Errorf("max_tokens = %v, want 256", r.MaxTokens)
	}
	if r.TopP == nil || *r.TopP != 0.95 {
		t.Errorf("top_p = %v", r.TopP)
	}
	if r.Seed == nil || *r.Seed != 42 {
		t.Errorf("seed = %v", r.Seed)
	}
	if r.Logprobs == nil || !*r.Logprobs {
		t.Errorf("logprobs = %v", r.Logprobs)
	}
	if r.User != "u-1" {
		t.Errorf("user = %q", r.User)
	}
	if len(r.Stop) != 2 || r.Stop[0] != "STOP" || r.Stop[1] != "END" {
		t.Errorf("stop = %v", r.Stop)
	}
	if r.Extra["max_completion_tokens"] != 512 {
		t.Errorf("extra max_completion_tokens = %v", r.Extra["max_completion_tokens"])
	}
	// A common-subset provider must NOT carry the anthropic extensions.
	if _, ok := r.Extra["top_k"]; ok {
		t.Error("cerebras must not carry top_k")
	}
	// A modeled key never lands in Extra (would collide at Build).
	if _, ok := r.Extra["temperature"]; ok {
		t.Error("temperature must be a typed field, not an Extra entry")
	}
	// The typed ChatRequest.Build must accept the projection (no Extra collision).
	if _, err := r.Build(canonicalSonicMarshaler); err != nil {
		t.Fatalf("ChatRequest.Build rejected the projection: %v", err)
	}
}

// TestMapAnthropic_ProviderExtensions proves the anthropic-specific extensions
// top_k / anthropic_beta ARE admitted through the common-bearer path for anthropic
// and land in ChatRequest.Extra.
func TestMapAnthropic_ProviderExtensions(t *testing.T) {
	m := mustMap(t, mapReg("A", "anthropic", map[string]any{
		"model":          "claude-x",
		"api_key":        "sk-ant-fake",
		"top_k":          40,
		"anthropic_beta": []any{"beta-1", "beta-2"},
	}), "anthropic")
	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.Extra["top_k"] != 40 {
		t.Errorf("anthropic top_k = %v, want 40", r.Extra["top_k"])
	}
	beta, ok := r.Extra["anthropic_beta"].([]string)
	if !ok || len(beta) != 2 || beta[0] != "beta-1" {
		t.Errorf("anthropic_beta = %v", r.Extra["anthropic_beta"])
	}
}

// TestMapBearer_ProviderExtensionDeclinedForNonAnthropic proves top_k /
// anthropic_beta DECLINE for a non-anthropic common-bearer provider (cerebras and
// an arbitrary unknown provider) — they never bypass the confident mapping
// boundary into ChatRequest.Extra without provider support.
func TestMapBearer_ProviderExtensionDeclinedForNonAnthropic(t *testing.T) {
	for _, tc := range []struct {
		provider string
		key      string
		val      any
	}{
		{"cerebras", "top_k", 40},
		{"cerebras", "anthropic_beta", "beta-1"},
		{"some-future-provider", "top_k", 40},
		{"cohere", "anthropic_beta", []any{"beta-1"}},
	} {
		t.Run(tc.provider+"_"+tc.key, func(t *testing.T) {
			wantMapDecline(t, mapReg("X", tc.provider, map[string]any{
				"model": "m", "api_key": "k", tc.key: tc.val,
			}), tc.provider, StageClientOption, ReasonUnprovenClientOption)
		})
	}
}

func TestMapBearer_ControlAndUnknownOptionsDecline(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		o := map[string]any{"model": "m", "api_key": "k"}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	cases := []struct {
		key    string
		val    any
		reason Reason
	}{
		{"tools", []any{}, ReasonToolsOption},
		{"tool_choice", "auto", ReasonToolsOption},
		{"response_format", map[string]any{"type": "json_object"}, ReasonResponseFormatOption},
		{"request_body", map[string]any{"x": 1}, ReasonRequestBodyOption},
		{"reasoning_effort", "high", ReasonUnprovenClientOption},
		{"temperature", "hot", ReasonInvalidBodyOption},
		{"max_tokens", 1.5, ReasonInvalidBodyOption},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			wantMapDecline(t, mapReg("C", "cerebras", base(map[string]any{tc.key: tc.val})), "cerebras",
				StageClientOption, tc.reason)
		})
	}
}

func TestMapBearer_MalformedHeadersDecline(t *testing.T) {
	wantMapDecline(t, mapReg("A", "anthropic", map[string]any{
		"model":   "m",
		"api_key": "k",
		"headers": map[string]any{"X-Trace": 123}, // non-string value
	}), "anthropic", StageClientOption, ReasonInvalidHeadersOption)
}

// TestMapBearer_InvalidHeaderGrammarDecline proves custom headers are validated
// against the HTTP field grammar (the same exact-transport checks) BEFORE nanollm —
// an invalid field name (separator/control) or a value carrying CR/LF (a
// header-injection vector) declines invalid_headers_option pre-FFI.
func TestMapBearer_InvalidHeaderGrammarDecline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]any
	}{
		{"name_with_space", map[string]any{"Bad Name": "v"}},
		{"name_with_colon", map[string]any{"X:Trace": "v"}},
		{"name_with_control", map[string]any{"X\x01Trace": "v"}},
		{"value_with_crlf", map[string]any{"X-Trace": "line1\r\nInjected: evil"}},
		{"value_with_nul", map[string]any{"X-Trace": "va\x00lue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("A", "anthropic", map[string]any{
				"model": "m", "api_key": "k", "headers": tc.headers,
			}), "anthropic", StageClientOption, ReasonInvalidHeadersOption)
		})
	}
}

// TestMapBearer_NegativeCountOptionsDecline proves the count-like body options
// (max_tokens / n / max_completion_tokens / anthropic top_k) fail closed for a
// NEGATIVE value in the mapper (invalid_body_option) — they must never reach
// Prepare. top_k stays anthropic-only.
func TestMapBearer_NegativeCountOptionsDecline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		key      string
		val      any
	}{
		{"max_tokens_negative", "cerebras", "max_tokens", -1},
		{"n_negative", "cerebras", "n", -3},
		{"max_completion_tokens_negative", "cerebras", "max_completion_tokens", -5},
		{"top_k_negative", "anthropic", "top_k", -2},
		{"max_tokens_negative_jsonnumber", "cerebras", "max_tokens", json.Number("-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("X", tc.provider, map[string]any{"model": "m", "api_key": "k", tc.key: tc.val}),
				tc.provider, StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

// TestMapBearer_ZeroCountOptionsAccepted proves the count-like options accept a
// CONTRACT-VALID zero rather than falsely declining it as malformed: the pinned
// Anthropic OpenAPI fixture declares max_tokens minimum 0 (prompt-cache pre-warming)
// and top_k minimum 0, and the common subset (max_tokens / n / max_completion_tokens)
// maps a non-negative value for every bearer provider (a value a specific provider
// forbids is that provider's runtime contract, not a mapping malformation).
func TestMapBearer_ZeroCountOptionsAccepted(t *testing.T) {
	// Anthropic zero max_tokens + zero top_k both map (both minimum 0 per fixture).
	ma := mustMap(t, mapReg("A", "anthropic", map[string]any{
		"model": "claude-x", "api_key": "k", "max_tokens": 0, "top_k": 0,
	}), "anthropic")
	var ra nanollm.ChatRequest
	applyBodyOptions(&ra, ma.body)
	if ra.MaxTokens == nil || *ra.MaxTokens != 0 {
		t.Errorf("anthropic max_tokens = %v, want 0 (contract-valid)", ra.MaxTokens)
	}
	if ra.Extra["top_k"] != 0 {
		t.Errorf("anthropic top_k = %v, want 0 (contract-valid)", ra.Extra["top_k"])
	}

	// Common-subset zeros map for a non-anthropic bearer provider too.
	mc := mustMap(t, mapReg("C", "cerebras", map[string]any{
		"model": "m", "api_key": "k", "max_tokens": 0, "n": 0, "max_completion_tokens": 0,
	}), "cerebras")
	var rc nanollm.ChatRequest
	applyBodyOptions(&rc, mc.body)
	if rc.MaxTokens == nil || *rc.MaxTokens != 0 || rc.N == nil || *rc.N != 0 {
		t.Errorf("cerebras count zeros not projected: max_tokens=%v n=%v", rc.MaxTokens, rc.N)
	}
	if rc.Extra["max_completion_tokens"] != 0 {
		t.Errorf("cerebras max_completion_tokens = %v, want 0", rc.Extra["max_completion_tokens"])
	}
}

func TestMapBearer_HeadersProjectedToParams(t *testing.T) {
	m := mustMap(t, mapReg("A", "anthropic", map[string]any{
		"model":   "m",
		"api_key": "k",
		"headers": map[string]any{"X-Trace": "abc", "X-Env": "prod"},
	}), "anthropic")
	if len(m.headers) != 2 || m.headers["X-Trace"] != "abc" || m.headers["X-Env"] != "prod" {
		// Header values may carry auth material — report only the count, never the map.
		t.Fatalf("headers projection mismatch: got %d entries", len(m.headers))
	}
	cfg := newTrustedConfig(m)
	hdr, ok := cfg.Models[0].Params["headers"].(map[string]string)
	if !ok || hdr["X-Trace"] != "abc" || hdr["X-Env"] != "prod" {
		// Header values may carry auth material — report only presence, never the map.
		t.Fatalf("Params[headers] not projected as the expected string map (isStringMap=%v)", ok)
	}
}

// --- Env placeholder plumbing (§4.3) ---

func TestNewTrustedConfig_EnvPlaceholders(t *testing.T) {
	m := mustMap(t, mapReg("A", "anthropic", map[string]any{
		"model":    "claude-x",
		"api_key":  "sk-ant-topsecret",
		"base_url": "https://api.anthropic.com",
	}), "anthropic")
	cfg := newTrustedConfig(m)
	if cfg.UseProcessEnv {
		t.Error("UseProcessEnv must be false (nanollm never reads ambient env)")
	}
	mc := cfg.Models[0]
	if mc.Model != "anthropic/claude-x" {
		t.Errorf("model = %q, want anthropic/claude-x", mc.Model)
	}
	if mc.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0", mc.MaxRetries)
	}
	if mc.APIKey != envPlaceholder(envKeyAPIKey) {
		// Never print mc.APIKey — on a regression it could be the literal secret.
		t.Error("ModelConfig.APIKey is not the expected private ${...} placeholder (value not printed)")
	}
	if cfg.Env[envKeyAPIKey] != "sk-ant-topsecret" {
		t.Errorf("Env api key not carried")
	}
	// The private key holds the LITERAL — a resolved value with a ${...} must not be
	// recursively interpreted (single-pass interpolation is nanollm's contract).
	m2 := mustMap(t, mapReg("A", "anthropic", map[string]any{
		"model":   "m",
		"api_key": "sk-${NOT_A_VAR}-literal",
	}), "anthropic")
	if newTrustedConfig(m2).Env[envKeyAPIKey] != "sk-${NOT_A_VAR}-literal" {
		// Never print the resolved key (even a fake) — report only the fact.
		t.Error("Env api key was not carried verbatim (a ${...} literal must not be re-interpreted)")
	}
}

// --- alias / provenance ---

func TestMapBearer_AliasCollisionDeclines(t *testing.T) {
	// alias == target
	m, dec, err := mapClientConfig(context.Background(), mappingInput{
		registry: mapReg("C", "cerebras", map[string]any{"model": "collide", "api_key": "k"}), alias: "collide", resolvedProvider: "cerebras",
	})
	if err != nil || m != nil || dec == nil || dec.Reason != ReasonInvalidAlias {
		// Never print the mappedClient (embeds secrets); dec/err are secret-free.
		t.Fatalf("alias==target: mappedClientNil=%v dec=%v err=%v, want a nil client + invalid_alias decline", m == nil, dec, err)
	}
}

// --- Bedrock mapper (§4.5 + §13) ---

// bedrockStaticReg is a fully-specified static-credential aws-bedrock registry (no
// network I/O: static creds resolve via the SDK's static provider).
func bedrockStaticReg(extra map[string]any) *bamlutils.ClientRegistry {
	opts := map[string]any{
		"model_id":          "anthropic.claude-v2",
		"region":            "us-east-1",
		"access_key_id":     "AKIAFAKE",
		"secret_access_key": "secretfake",
	}
	for k, v := range extra {
		opts[k] = v
	}
	return mapReg("B", "aws-bedrock", opts)
}

func TestMapBedrock_StaticCredsAndInferenceConfig(t *testing.T) {
	orig := map[string]any{"anthropic_version": "bedrock-2023"}
	m := mustMap(t, bedrockStaticReg(map[string]any{
		"session_token": "sessfake",
		"inference_configuration": map[string]any{
			"max_tokens":     1024,
			"temperature":    0.3,
			"top_p":          0.9,
			"stop_sequences": []any{"\n\nHuman:"},
		},
		"additional_model_request_fields": orig,
	}), "aws-bedrock")

	if m.verification != PolicyTrustedProvider {
		t.Errorf("policy = %v, want trusted_provider", m.verification)
	}
	if m.nanollmProvider != "bedrock" || m.target != "anthropic.claude-v2" {
		t.Errorf("mapping = (%q,%q), want (bedrock, anthropic.claude-v2)", m.nanollmProvider, m.target)
	}
	if m.bedrock == nil {
		t.Fatal("bedrock params missing")
	}
	if m.bedrock.region != "us-east-1" {
		t.Errorf("bedrock region = %q, want us-east-1", m.bedrock.region)
	}
	// Never print the resolved credential literals (secret hygiene).
	if m.bedrock.accessKeyID != "AKIAFAKE" || m.bedrock.secretAccessKey != "secretfake" || m.bedrock.sessionToken != "sessfake" {
		t.Error("bedrock resolved credential literals did not match the injected static values (not printed)")
	}
	if m.bedrock.source != BedrockCredentialExplicit {
		t.Errorf("cred source = %q, want explicit", m.bedrock.source)
	}

	// inference_configuration -> canonical OpenAI body fields.
	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.MaxTokens == nil || *r.MaxTokens != 1024 || r.Temperature == nil || *r.Temperature != 0.3 || r.TopP == nil || *r.TopP != 0.9 {
		t.Error("inference config projection wrong (max_tokens/temperature/top_p did not match)")
	}
	if len(r.Stop) != 1 || r.Stop[0] != "\n\nHuman:" {
		t.Errorf("stop_sequences -> stop wrong: %v", r.Stop)
	}

	// Config: creds via top-level Params placeholders (interpolated); additional
	// fields as a nested literal (NOT interpolated); no api_key/base_url.
	cfg := newTrustedConfig(m)
	if cfg.UseProcessEnv {
		t.Error("UseProcessEnv must be false")
	}
	p := cfg.Models[0].Params
	if p["region"] != envPlaceholder(envKeyBedrockRegion) || p["access_key_id"] != envPlaceholder(envKeyBedrockAccessKeyID) || p["secret_access_key"] != envPlaceholder(envKeyBedrockSecretKey) || p["session_token"] != envPlaceholder(envKeyBedrockSessionToken) {
		// Never print the Params map (credential placeholders + arbitrary nested fields).
		t.Error("bedrock Params credential placeholders are not the expected private ${...} references")
	}
	if cfg.Env[envKeyBedrockAccessKeyID] != "AKIAFAKE" || cfg.Env[envKeyBedrockSecretKey] != "secretfake" {
		t.Errorf("bedrock Env literals not carried")
	}
	amrf, ok := p["additional_model_request_fields"].(map[string]any)
	if !ok || amrf["anthropic_version"] != "bedrock-2023" {
		// Never print the additional-model-fields map.
		t.Errorf("additional_model_request_fields not projected as the expected object (isObject=%v)", ok)
	}
	if cfg.Models[0].APIKey != "" {
		t.Errorf("bedrock APIKey must be empty (creds go via Params)")
	}
	// Deep copy: mutating the original registry map must not affect the mapping.
	orig["anthropic_version"] = "MUTATED"
	if amrf["anthropic_version"] != "bedrock-2023" {
		t.Error("additional_model_request_fields was not deep-copied")
	}
}

func TestMapBedrock_ModelKeyXOR(t *testing.T) {
	// both model and model_id
	wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
		"model": "a", "model_id": "b", "region": "us-east-1",
		"access_key_id": "AKIAFAKE", "secret_access_key": "s",
	}), "aws-bedrock", StageClientSelection, ReasonBedrockModelKeys)
	// neither
	wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
		"region": "us-east-1", "access_key_id": "AKIAFAKE", "secret_access_key": "s",
	}), "aws-bedrock", StageClientSelection, ReasonBedrockModelKeys)
}

func TestMapBedrock_RegionPrecedence(t *testing.T) {
	// explicit option wins
	m := mustMap(t, bedrockStaticReg(map[string]any{"region": "eu-west-1"}), "aws-bedrock")
	if m.bedrock.region != "eu-west-1" {
		t.Errorf("region = %q, want the explicit option", m.bedrock.region)
	}
	// AWS_REGION fallback
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("AWS_DEFAULT_REGION", "")
	reg := mapReg("B", "aws-bedrock", map[string]any{"model_id": "m", "access_key_id": "AKIAFAKE", "secret_access_key": "s"})
	m2 := mustMap(t, reg, "aws-bedrock")
	if m2.bedrock.region != "ap-south-1" {
		t.Errorf("region = %q, want the AWS_REGION fallback", m2.bedrock.region)
	}
	// AWS_DEFAULT_REGION fallback
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "sa-east-1")
	m3 := mustMap(t, reg, "aws-bedrock")
	if m3.bedrock.region != "sa-east-1" {
		t.Errorf("region = %q, want the AWS_DEFAULT_REGION fallback", m3.bedrock.region)
	}
}

func TestMapBedrock_MissingRegionDeclines(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
		"model_id": "m", "access_key_id": "AKIAFAKE", "secret_access_key": "s",
	}), "aws-bedrock", StageCredentialSource, ReasonRegionMissing)
}

func TestMapBedrock_PartialStaticCredsDecline(t *testing.T) {
	// access_key_id without secret_access_key: NEVER fall to ambient — decline.
	wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
		"model_id": "m", "region": "us-east-1", "access_key_id": "AKIAFAKE",
	}), "aws-bedrock", StageCredentialSource, ReasonPartialCredentials)
}

func TestMapBedrock_EndpointAndHeadersDecline(t *testing.T) {
	wantMapDecline(t, bedrockStaticReg(map[string]any{"endpoint_url": "http://localhost:9000"}), "aws-bedrock",
		StageClientOption, ReasonEndpointOverride)
	wantMapDecline(t, bedrockStaticReg(map[string]any{"headers": map[string]any{"X": "y"}}), "aws-bedrock",
		StageClientOption, ReasonHeadersOption)
}

func TestMapBedrock_UnknownInferenceFieldDeclines(t *testing.T) {
	wantMapDecline(t, bedrockStaticReg(map[string]any{
		"inference_configuration": map[string]any{"frequency_penalty": 0.1},
	}), "aws-bedrock", StageClientOption, ReasonInvalidBodyOption)
}

func TestMapBedrock_UnknownOptionDeclines(t *testing.T) {
	wantMapDecline(t, bedrockStaticReg(map[string]any{"guardrail_config": map[string]any{"id": "x"}}), "aws-bedrock",
		StageClientOption, ReasonUnprovenClientOption)
}

// --- P1: a declared Bedrock profile must not silently use ambient creds ---

// TestMapBedrock_DeclaredProfileFallsToAmbientDeclines injects a resolution that
// returns credentials sourced from the EC2 instance metadata service (the AWS SDK
// fallthrough for a credential-less named profile). A DECLARED profile resolving
// through an ambient source must decline PRE-socket — never admit host creds under
// a "profile" label.
func TestMapBedrock_DeclaredProfileFallsToAmbientDeclines(t *testing.T) {
	for _, ambient := range []string{"EC2RoleProvider", "CredentialsEndpointProvider", "EnvConfigCredentials"} {
		t.Run(ambient, func(t *testing.T) {
			orig := awsCredentialRetriever
			defer func() { awsCredentialRetriever = orig }()
			awsCredentialRetriever = func(ctx context.Context, clientName string, sel llmhttp.BedrockCredentialSelector) (resolvedAWSCredential, error) {
				return resolvedAWSCredential{accessKeyID: "AKIAAMBIENTFAKE", secretAccessKey: "ambientsecret", source: ambient}, nil
			}
			wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
				"model_id": "m", "region": "us-east-1", "profile": "region-only-profile",
			}), "aws-bedrock", StageCredentialSource, ReasonUnsupportedCredentialSource)
		})
	}
}

// TestMapBedrock_DeclaredProfileWithProfileSourceMaps proves a declared profile
// resolving through a genuine profile credential mechanism (non-ambient source) is
// accepted and labeled `profile`.
func TestMapBedrock_DeclaredProfileWithProfileSourceMaps(t *testing.T) {
	orig := awsCredentialRetriever
	defer func() { awsCredentialRetriever = orig }()
	awsCredentialRetriever = func(ctx context.Context, clientName string, sel llmhttp.BedrockCredentialSelector) (resolvedAWSCredential, error) {
		return resolvedAWSCredential{accessKeyID: "AKIAPROFILEFAKE", secretAccessKey: "profilesecret", source: "StaticCredentials"}, nil
	}
	m := mustMap(t, mapReg("B", "aws-bedrock", map[string]any{
		"model_id": "m", "region": "us-east-1", "profile": "prod",
	}), "aws-bedrock")
	if m.bedrock == nil {
		t.Fatal("bedrock params missing")
	}
	if m.bedrock.source != BedrockCredentialProfile {
		// source is a bounded enum (secret-free); the creds are never printed.
		t.Fatalf("bedrock source = %v, want profile", m.bedrock.source)
	}
	if m.bedrock.accessKeyID != "AKIAPROFILEFAKE" {
		t.Errorf("profile creds not carried")
	}
}

// TestMapBedrock_DefaultChainAmbientAllowed proves the NO-declared-source default
// chain MAY use ambient credentials (owner decision A) — an ambient source there
// is NOT declined (only a declared profile falling to ambient is).
func TestMapBedrock_DefaultChainAmbientAllowed(t *testing.T) {
	orig := awsCredentialRetriever
	defer func() { awsCredentialRetriever = orig }()
	awsCredentialRetriever = func(ctx context.Context, clientName string, sel llmhttp.BedrockCredentialSelector) (resolvedAWSCredential, error) {
		return resolvedAWSCredential{accessKeyID: "AKIAIMDSFAKE", secretAccessKey: "imdssecret", source: "EC2RoleProvider"}, nil
	}
	m := mustMap(t, mapReg("B", "aws-bedrock", map[string]any{
		"model_id": "m", "region": "us-east-1", // nothing declared -> default chain
	}), "aws-bedrock")
	if m.bedrock == nil {
		t.Fatal("bedrock params missing")
	}
	if m.bedrock.source != BedrockCredentialDefaultChain {
		t.Fatalf("bedrock source = %v, want default_chain", m.bedrock.source)
	}
}

// --- P2: pre-claim credential I/O is bounded ---

// TestMapBedrock_UnboundedContextGetsDeadline proves an admission context with no
// deadline gets a bounded one ~5s out for the AWS credential I/O (a precise window,
// so a drift of bedrockCredentialResolveTimeout from 5s is caught), while an earlier
// caller deadline is preserved unchanged.
func TestMapBedrock_UnboundedContextGetsDeadline(t *testing.T) {
	orig := awsCredentialRetriever
	defer func() { awsCredentialRetriever = orig }()
	var sawDeadline bool
	var seen time.Time
	awsCredentialRetriever = func(ctx context.Context, clientName string, sel llmhttp.BedrockCredentialSelector) (resolvedAWSCredential, error) {
		seen, sawDeadline = ctx.Deadline()
		return resolvedAWSCredential{accessKeyID: "AKIAFAKE", secretAccessKey: "s", source: "StaticCredentials"}, nil
	}
	reg := bedrockStaticReg(nil)

	// Unbounded input context -> a deadline ~5s from now is applied. Capture a start
	// bound just before the call and assert the resolver deadline lands in a tolerant
	// window around the LITERAL 5s (not the const), so a drift to any other value fails.
	start := time.Now()
	if _, dec, err := mapClientConfig(context.Background(), mappingInput{registry: reg, alias: trustedAlias, resolvedProvider: "aws-bedrock"}); err != nil || dec != nil {
		t.Fatalf("map failed: dec=%v err=%v", dec, err)
	}
	if !sawDeadline {
		t.Fatal("unbounded admission context: credential resolution ran without a deadline")
	}
	if d := seen.Sub(start); d < 4*time.Second || d > 6*time.Second {
		t.Errorf("applied credential deadline = %v after start, want ~5s (drift from the intended timeout)", d)
	}

	// A caller deadline far in the future is preserved (not shortened to the 5s default).
	want := time.Now().Add(2 * time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if _, dec, err := mapClientConfig(ctx, mappingInput{registry: reg, alias: trustedAlias, resolvedProvider: "aws-bedrock"}); err != nil || dec != nil {
		t.Fatalf("map failed: dec=%v err=%v", dec, err)
	}
	if !sawDeadline || seen.Before(want.Add(-time.Minute)) {
		t.Errorf("caller deadline not preserved: resolver deadline is %v before the caller's ~2h deadline", want.Sub(seen))
	}
}

// --- P3: non-JSON / non-UTF8 values decline pre-FFI (no engine construction) ---

func TestMapBearer_NonFiniteAndOutOfRangeDecline(t *testing.T) {
	base := func(k string, v any) map[string]any {
		return map[string]any{"model": "m", "api_key": "k", k: v}
	}
	cases := []struct {
		name string
		opts map[string]any
	}{
		{"temperature_nan", base("temperature", math.NaN())},
		{"temperature_posinf", base("temperature", math.Inf(1))},
		{"temperature_neginf", base("temperature", math.Inf(-1))},
		{"max_tokens_out_of_range", base("max_tokens", 1e300)},
		{"top_p_nan", base("top_p", math.NaN())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("C", "cerebras", tc.opts), "cerebras", StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

func TestMapBearer_NonUTF8TargetDeclines(t *testing.T) {
	bad := string([]byte{'g', 'p', 't', 0xff, 0xfe})
	wantMapDecline(t, mapReg("C", "cerebras", map[string]any{"model": bad, "api_key": "k"}),
		"cerebras", StageClientSelection, ReasonModelNotLiteral)
}

func TestMapBedrock_NonUTF8TargetDeclines(t *testing.T) {
	bad := string([]byte{'m', 0xff})
	wantMapDecline(t, mapReg("B", "aws-bedrock", map[string]any{
		"model_id": bad, "region": "us-east-1", "access_key_id": "AKIAFAKE", "secret_access_key": "s",
	}), "aws-bedrock", StageClientSelection, ReasonModelNotLiteral)
}

func TestMapBedrock_AdditionalFieldsNonJSONDecline(t *testing.T) {
	cases := []struct {
		name string
		amrf map[string]any
	}{
		{"nan_leaf", map[string]any{"x": math.NaN()}},
		{"inf_leaf", map[string]any{"x": math.Inf(1)}},
		{"nested_nan", map[string]any{"cfg": map[string]any{"y": math.Inf(-1)}}},
		{"non_utf8_string", map[string]any{"s": string([]byte{0xff, 0xfe})}},
		{"array_nan", map[string]any{"arr": []any{1.0, math.NaN()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, bedrockStaticReg(map[string]any{"additional_model_request_fields": tc.amrf}),
				"aws-bedrock", StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

// --- P3 (round 2): json.Number JSON-grammar validation + UTF-8 keys/strings ---

// TestValidJSONNumber unit-tests the single number-validity source of truth: it
// enforces the JSON-number grammar AND finiteness, rejecting the non-JSON spellings
// json.Number.Float64()/Int64() would otherwise accept (NaN/Inf/"01"/"+1"/".5"/"1.").
func TestValidJSONNumber(t *testing.T) {
	valid := []string{"0", "-1", "1", "1.5", "-2.5", "1e3", "1E3", "1.2e-3", "123456789", "0.5", "-0"}
	invalid := []string{"", "NaN", "Inf", "-Inf", "+1", "01", "-01", ".5", "1.", "1e", "1e400", "0x1", "abc", "true", "null", "1 ", " 1", "1,2", "\"5\""}
	for _, s := range valid {
		if !validJSONNumber(json.Number(s)) {
			t.Errorf("validJSONNumber(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if validJSONNumber(json.Number(s)) {
			t.Errorf("validJSONNumber(%q) = true, want false", s)
		}
	}
}

// TestMapBearer_MalformedJSONNumberDecline proves a malformed json.Number body
// option takes the stable pre-FFI invalid_body_option decline (no engine
// construction) — the round-1 finite check missed these because Float64()/Int64()
// accept them.
func TestMapBearer_MalformedJSONNumberDecline(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		val  any
	}{
		{"nan_temperature", "temperature", json.Number("NaN")},
		{"inf_top_p", "top_p", json.Number("Inf")},
		{"leading_zero_max_tokens", "max_tokens", json.Number("01")},
		{"plus_max_tokens", "max_tokens", json.Number("+1")},
		{"overflow_temperature", "temperature", json.Number("1e400")},
		{"bare_dot_seed", "seed", json.Number(".5")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("C", "cerebras", map[string]any{"model": "m", "api_key": "k", tc.key: tc.val}),
				"cerebras", StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

// TestAsInt64_ExactBoundaries unit-tests exact int64 range validation for
// json.Number: a Float64() round-trip must not be used for range (a negative
// out-of-range literal rounds to exactly float64(MinInt64)). The exact
// MinInt64/MaxInt64 literals are accepted; one-below-min / one-above-max and
// non-integral spellings decline.
func TestAsInt64_ExactBoundaries(t *testing.T) {
	accept := map[string]int64{
		"-9223372036854775808": math.MinInt64, // exact min
		"9223372036854775807":  math.MaxInt64, // exact max
		"0":                    0,
		"-1":                   -1,
		"256":                  256,
	}
	for s, want := range accept {
		got, ok := asInt64(json.Number(s))
		if !ok || got != want {
			t.Errorf("asInt64(json.Number(%q)) = (%d,%v), want (%d,true)", s, got, ok, want)
		}
	}
	reject := []string{
		"-9223372036854775809", // one below min (the round-to-boundary bug)
		"9223372036854775808",  // one above max
		"-9223372036854775808.5",
		"9223372036854775807.4",
		"100.0", // non-integral spelling for an int field
		"1e2",   // exponent spelling for an int field
		"1.5",
		"99999999999999999999999999999999", // far out of range
	}
	for _, s := range reject {
		if got, ok := asInt64(json.Number(s)); ok {
			t.Errorf("asInt64(json.Number(%q)) = (%d,true), want reject", s, got)
		}
	}
}

// TestMapBearer_Int64BoundaryJSONNumberDecline proves the boundary bug is closed at
// the mapping layer for an int64-typed option (seed): a one-below-min literal takes
// the pre-FFI invalid_body_option decline (no engine construction), while the exact
// MinInt64 literal is still accepted.
func TestMapBearer_Int64BoundaryJSONNumberDecline(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		val  any
	}{
		{"seed_one_below_min", "seed", json.Number("-9223372036854775809")},
		{"seed_one_above_max", "seed", json.Number("9223372036854775808")},
		{"max_tokens_one_below_min", "max_tokens", json.Number("-9223372036854775809")},
		{"seed_near_boundary_fractional", "seed", json.Number("-9223372036854775808.5")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("C", "cerebras", map[string]any{"model": "m", "api_key": "k", tc.key: tc.val}),
				"cerebras", StageClientOption, ReasonInvalidBodyOption)
		})
	}
	// The exact MinInt64 literal is still accepted and projected.
	m := mustMap(t, mapReg("C", "cerebras", map[string]any{
		"model": "m", "api_key": "k", "seed": json.Number("-9223372036854775808"),
	}), "cerebras")
	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.Seed == nil || *r.Seed != math.MinInt64 {
		t.Errorf("seed = %v, want MinInt64", r.Seed)
	}
}

// TestMapBedrock_InferenceMaxTokensBoundaryDecline proves the same exactness applies
// to Bedrock inference_configuration.max_tokens.
func TestMapBedrock_InferenceMaxTokensBoundaryDecline(t *testing.T) {
	wantMapDecline(t, bedrockStaticReg(map[string]any{
		"inference_configuration": map[string]any{"max_tokens": json.Number("-9223372036854775809")},
	}), "aws-bedrock", StageClientOption, ReasonInvalidBodyOption)
}

// TestMapBedrock_InferenceMaxTokensNegativeDecline proves a NEGATIVE Bedrock
// inference_configuration.max_tokens declines pre-FFI (matching the common bearer
// mapper) — nanollm ChatRequest.Build does no semantic validation, so the guard
// must live in the mapper. int and json.Number forms both decline.
func TestMapBedrock_InferenceMaxTokensNegativeDecline(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"int_negative", -1},
		{"jsonnumber_negative", json.Number("-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, bedrockStaticReg(map[string]any{
				"inference_configuration": map[string]any{"max_tokens": tc.val},
			}), "aws-bedrock", StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

// TestMapBedrock_InferenceMaxTokensZeroAccepted proves a CONTRACT-VALID zero
// Bedrock inference_configuration.max_tokens maps (Bedrock/Anthropic allow 0), not
// falsely declined as malformed.
func TestMapBedrock_InferenceMaxTokensZeroAccepted(t *testing.T) {
	m := mustMap(t, bedrockStaticReg(map[string]any{
		"inference_configuration": map[string]any{"max_tokens": 0},
	}), "aws-bedrock")
	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.MaxTokens == nil || *r.MaxTokens != 0 {
		t.Errorf("bedrock inference max_tokens = %v, want 0 (contract-valid)", r.MaxTokens)
	}
}

// TestMapBearer_ValidJSONNumberAccepted proves a well-formed json.Number (the shape
// a JSON decoder produces with UseNumber) is still accepted and projected.
func TestMapBearer_ValidJSONNumberAccepted(t *testing.T) {
	m := mustMap(t, mapReg("C", "cerebras", map[string]any{
		"model": "m", "api_key": "k",
		"temperature": json.Number("1.5"),
		"max_tokens":  json.Number("256"),
	}), "cerebras")
	var r nanollm.ChatRequest
	applyBodyOptions(&r, m.body)
	if r.Temperature == nil || *r.Temperature != 1.5 {
		t.Errorf("temperature = %v, want 1.5", r.Temperature)
	}
	if r.MaxTokens == nil || *r.MaxTokens != 256 {
		t.Errorf("max_tokens = %v, want 256", r.MaxTokens)
	}
}

// TestMapBedrock_AdditionalFieldsMalformedNumberAndKeyDecline proves the recursive
// additional_model_request_fields validator rejects a malformed json.Number leaf
// AND an invalid-UTF-8 object key (round-1 validated string VALUES but not keys or
// json.Number grammar) — all pre-FFI, no engine construction.
func TestMapBedrock_AdditionalFieldsMalformedNumberAndKeyDecline(t *testing.T) {
	for _, tc := range []struct {
		name string
		amrf map[string]any
	}{
		{"nan_number", map[string]any{"x": json.Number("NaN")}},
		{"leading_zero_number", map[string]any{"x": json.Number("01")}},
		{"nested_inf_number", map[string]any{"cfg": map[string]any{"y": json.Number("Inf")}}},
		{"array_malformed_number", map[string]any{"arr": []any{json.Number("1"), json.Number("01")}}},
		{"invalid_utf8_key", map[string]any{string([]byte{0xff, 0xfe}): "v"}},
		{"nested_invalid_utf8_key", map[string]any{"cfg": map[string]any{string([]byte{0xff}): 1.0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, bedrockStaticReg(map[string]any{"additional_model_request_fields": tc.amrf}),
				"aws-bedrock", StageClientOption, ReasonInvalidBodyOption)
		})
	}
}

// TestMapBedrock_AdditionalFieldsValidJSONNumberAccepted proves a well-formed
// json.Number leaf still maps (a JSON decoder produces json.Number for numbers).
func TestMapBedrock_AdditionalFieldsValidJSONNumberAccepted(t *testing.T) {
	m := mustMap(t, bedrockStaticReg(map[string]any{
		"additional_model_request_fields": map[string]any{"k": json.Number("1.5"), "n": json.Number("-3")},
	}), "aws-bedrock")
	amrf := m.bedrock.additionalModelRequestFields
	if amrf["k"] != json.Number("1.5") || amrf["n"] != json.Number("-3") {
		// Never print the additional-model-fields map.
		t.Error("additional fields json.Number leaves not carried verbatim")
	}
}

// TestMapBearer_NonUTF8StringOptionsDecline proves every string option carried into
// a trusted request is UTF-8-validated pre-FFI (header name/value, user, base_url,
// stop element).
func TestMapBearer_NonUTF8StringOptionsDecline(t *testing.T) {
	badKey := string([]byte{0xff})
	badVal := string([]byte{0xff, 0xfe})
	for _, tc := range []struct {
		name     string
		provider string
		opts     map[string]any
		stage    Stage
		reason   Reason
	}{
		{"header_key", "anthropic", map[string]any{"model": "m", "api_key": "k", "headers": map[string]any{badKey: "v"}}, StageClientOption, ReasonInvalidHeadersOption},
		{"header_value", "anthropic", map[string]any{"model": "m", "api_key": "k", "headers": map[string]any{"X": badVal}}, StageClientOption, ReasonInvalidHeadersOption},
		{"user", "cerebras", map[string]any{"model": "m", "api_key": "k", "user": badVal}, StageClientOption, ReasonInvalidBodyOption},
		{"base_url", "cerebras", map[string]any{"model": "m", "api_key": "k", "base_url": badVal}, StageCredentialSource, ReasonInvalidBaseURL},
		{"stop_element", "cerebras", map[string]any{"model": "m", "api_key": "k", "stop": []any{badVal}}, StageClientOption, ReasonInvalidBodyOption},
		{"api_key", "cerebras", map[string]any{"model": "m", "api_key": badVal}, StageCredentialSource, ReasonAPIKeyAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMapDecline(t, mapReg("X", tc.provider, tc.opts), tc.provider, tc.stage, tc.reason)
		})
	}
}

// --- generic post-Prepare self-consistency gate (§5.2), synthetic plans, no FFI ---

// TestValidateGenericPlan_Rejects proves the trusted-provider self-consistency gate
// rejects a non-chat / embedding / expired / non-JSON / streaming / wrong-provider
// plan WITHOUT a BAML differential. The request-type check is the load-bearing
// backstop that keeps a deferred provider's embedding plan (e.g. cohere in v0.4.3)
// from ever being admitted as chat.
func TestValidateGenericPlan_Rejects(t *testing.T) {
	good := func() *nanollm.PreparedRequest {
		return &nanollm.PreparedRequest{
			Method:         "POST",
			URL:            "https://provider.example/endpoint",
			Body:           []byte(`{"model":"m"}`),
			ResponseFormat: nanollm.FormatJSON,
			Meta: nanollm.PreparedMeta{
				ModelAlias:  "a",
				TargetModel: "m",
				Provider:    "anthropic",
				RequestType: nanollm.ChatCompletion,
			},
		}
	}
	if d := validateGenericPlan(good(), "a", "m", "anthropic"); d != nil {
		t.Fatalf("baseline valid plan declined: %v", d)
	}

	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name   string
		mutate func(*nanollm.PreparedRequest)
		reason Reason
	}{
		{"embedding_request_type", func(p *nanollm.PreparedRequest) { p.Meta.RequestType = nanollm.Embedding }, ReasonRequestTypeMismatch},
		{"embed_endpoint", func(p *nanollm.PreparedRequest) { p.URL = "https://api.cohere.com/v2/embed" }, ReasonEmbeddingPlan},
		{"embeddings_endpoint", func(p *nanollm.PreparedRequest) { p.URL = "https://api.openai.com/v1/embeddings" }, ReasonEmbeddingPlan},
		{"completion_request_type", func(p *nanollm.PreparedRequest) { p.Meta.RequestType = nanollm.Completion }, ReasonRequestTypeMismatch},
		{"expired", func(p *nanollm.PreparedRequest) { p.Meta.ExpiresAt = &past }, ReasonExpiredPlan},
		{"stream_true", func(p *nanollm.PreparedRequest) { p.Meta.Stream = true }, ReasonPlanStreamTrue},
		{"nonzero_retries", func(p *nanollm.PreparedRequest) { p.Meta.MaxRetries = 2 }, ReasonMaxRetriesNonzero},
		{"not_json", func(p *nanollm.PreparedRequest) { p.ResponseFormat = nanollm.FormatSSE }, ReasonResponseFormatNotJSON},
		{"not_post", func(p *nanollm.PreparedRequest) { p.Method = "GET" }, ReasonMethodNotPost},
		{"provider_mismatch", func(p *nanollm.PreparedRequest) { p.Meta.Provider = "cohere" }, ReasonPlanProviderMismatch},
		{"empty_body", func(p *nanollm.PreparedRequest) { p.Body = nil }, ReasonBodyEmpty},
		{"bad_json", func(p *nanollm.PreparedRequest) { p.Body = []byte(`{`) }, ReasonBodyNotJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.mutate(p)
			d := validateGenericPlan(p, "a", "m", "anthropic")
			if d == nil || d.Reason != tc.reason {
				t.Fatalf("decline = %v, want reason %s", d, tc.reason)
			}
		})
	}
}
