package admission

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"time"
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// bedrockCredentialResolveTimeout bounds the pre-claim AWS credential I/O (shared
// config / profile load / ECS / IMDS / STS) when the admission context carries no
// deadline of its own — so a request arriving with an unbounded context can never
// block admission on an unavailable metadata service. An earlier caller deadline
// is always preserved (this only adds one when none exists).
const bedrockCredentialResolveTimeout = 5 * time.Second

// ambientBedrockCredentialSources names the AWS SDK credential providers that
// represent AMBIENT (host-environment) credentials — the process env, the EC2
// instance metadata service (IMDS), and the ECS/container credential endpoint. A
// DECLARED profile that resolves through one of these has fallen through to
// ambient creds instead of the profile's own credential mechanism, which the hard
// "declared-but-broken never falls to ambient" rule forbids. The strings are the
// pinned aws-sdk-go-v2 ProviderName / Source constants (credentials v1.19.x:
// ec2rolecreds.ProviderName, endpointcreds.ProviderName; config v1.32.x:
// CredentialsSourceName) — RE-VERIFY on an AWS SDK bump.
var ambientBedrockCredentialSources = map[string]struct{}{
	"EnvConfigCredentials":        {}, // config.CredentialsSourceName (process env)
	"EC2RoleProvider":             {}, // ec2rolecreds (IMDS)
	"CredentialsEndpointProvider": {}, // endpointcreds (ECS / container endpoint)
}

// resolvedAWSCredential is the flattened output of one credential retrieval: the
// literal values plus the AWS SDK Source name (the provider that actually sourced
// them), used to prove a declared profile did not fall through to ambient creds.
type resolvedAWSCredential struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	source          string
}

// awsCredentialRetriever resolves a Bedrock credential selector to concrete
// credential literals + the SDK Source name, via the existing baml-rest/AWS
// selector. It is a package var ONLY so tests can inject deterministic credentials
// (and observe the resolution context's deadline) without real AWS I/O; production
// never rebinds it. It NEVER falls from a declared-but-broken source to ambient
// creds — a declared static/profile that cannot resolve returns an error; only an
// empty selector uses the AWS default chain.
var awsCredentialRetriever = retrieveAWSCredentialViaSelector

func retrieveAWSCredentialViaSelector(ctx context.Context, clientName string, sel llmhttp.BedrockCredentialSelector) (resolvedAWSCredential, error) {
	provider, err := llmhttp.ResolveBedrockCredentials(ctx, clientName, sel)
	if err != nil {
		return resolvedAWSCredential{}, err
	}
	if provider == nil {
		// Nothing declared: the AWS default chain (env / shared config / ECS-IMDS / SSO).
		provider, err = llmhttp.DefaultAWSCredentialProvider(ctx)
		if err != nil {
			return resolvedAWSCredential{}, err
		}
	}
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return resolvedAWSCredential{}, err
	}
	return resolvedAWSCredential{
		accessKeyID:     creds.AccessKeyID,
		secretAccessKey: creds.SecretAccessKey,
		sessionToken:    creds.SessionToken,
		source:          creds.Source,
	}, nil
}

// This file holds the S2 aws-bedrock special mapper (§4.5 + §13 DECISION A — the
// FULL AWS credential chain). nanollm owns SigV4 signing; the mapper only resolves
// the registry intent into a request-scoped nanollm Params input:
//
//   - model `bedrock/<model-id>` from EXACTLY ONE of `model`/`model_id`;
//   - a nonempty region (explicit `region`, else AWS_REGION, then AWS_DEFAULT_REGION);
//   - resolved credential LITERALS via the existing baml-rest/AWS selector
//     supporting explicit-static / env / profile / ECS-IMDS / default-chain,
//     retrieved PER-ADMISSION immediately before nanollm.New and passed to nanollm
//     with UseProcessEnv=false — it NEVER falls from a declared-but-broken source
//     to ambient credentials (a broken declared source declines to BAML);
//   - inference_configuration -> canonical OpenAI body fields (max_tokens /
//     temperature / top_p / stop);
//   - additional_model_request_fields deep-copied to Params.
//
// It declines endpoint_url (no override in v0.4.3 — never rewrite a signed URL),
// custom headers, both/no model keys, missing region, and partial/failed creds.

// bedrockParams is the resolved, request-scoped aws-bedrock input carried on a
// mappedClient. The credential fields are SENSITIVE literals resolved immediately
// before nanollm.New; they are written into a private Config.Env placeholder and
// NEVER logged, returned in clientFacts, or retained beyond the request-scoped
// client. additionalModelRequestFields is an owned deep copy of the registry's
// nested JSON. source is the bounded credential-source label (metric observation
// only — never a client/profile/region name).
type bedrockParams struct {
	region          string
	accessKeyID     string // SENSITIVE
	secretAccessKey string // SENSITIVE
	sessionToken    string // SENSITIVE (empty when the source has none)

	additionalModelRequestFields map[string]any
	source                       BedrockCredentialSource
}

// mapBedrock is the aws-bedrock special mapper. It assigns PolicyTrustedProvider
// (never a BAML comparison — §6). The credential resolution is the one documented
// impurity of the pure mapper, bounded by ctx.
func mapBedrock(ctx context.Context, cp *bamlutils.ClientProperty, in mappingInput, registryProvider, nanollmProvider string) (*mappedClient, *Decline, error) {
	var (
		modelVal, modelIDVal string
		modelSet, modelIDSet bool
		regionOpt            string
		sel                  llmhttp.BedrockCredentialSelector
		infConfig            bodyOptions
		additionalFields     map[string]any
	)

	for _, key := range sortedOptionKeys(cp.Options) {
		raw := cp.Options[key]
		switch key {
		case "model":
			s, ok := raw.(string)
			if !ok || s == "" {
				return nil, declinef(StageClientSelection, ReasonBedrockModelKeys,
					"aws-bedrock model option is empty or not a resolved literal string"), nil
			}
			modelVal, modelSet = s, true
		case "model_id":
			s, ok := raw.(string)
			if !ok || s == "" {
				return nil, declinef(StageClientSelection, ReasonBedrockModelKeys,
					"aws-bedrock model_id option is empty or not a resolved literal string"), nil
			}
			modelIDVal, modelIDSet = s, true
		case "region":
			s, ok := raw.(string)
			if !ok || s == "" {
				return nil, declinef(StageCredentialSource, ReasonRegionMissing,
					"aws-bedrock region option is empty or not a resolved literal string"), nil
			}
			regionOpt = s
		case "access_key_id":
			s, dec := bedrockCredString(raw)
			if dec != nil {
				return nil, dec, nil
			}
			sel.AccessKeyID, sel.AccessKeyIDPresent = s, true
		case "secret_access_key":
			s, dec := bedrockCredString(raw)
			if dec != nil {
				return nil, dec, nil
			}
			sel.SecretAccessKey, sel.SecretAccessKeyPresent = s, true
		case "session_token":
			s, dec := bedrockCredString(raw)
			if dec != nil {
				return nil, dec, nil
			}
			sel.SessionToken, sel.SessionTokenPresent = s, true
		case "profile":
			s, dec := bedrockCredString(raw)
			if dec != nil {
				return nil, dec, nil
			}
			sel.Profile, sel.ProfilePresent = s, true
		case "inference_configuration":
			if dec := parseInferenceConfig(&infConfig, raw); dec != nil {
				return nil, dec, nil
			}
		case "additional_model_request_fields":
			obj, ok := deepCopyJSONObject(raw)
			if !ok {
				return nil, declinef(StageClientOption, ReasonInvalidBodyOption,
					"aws-bedrock additional_model_request_fields is not a JSON object"), nil
			}
			additionalFields = obj
		case "endpoint_url":
			// nanollm v0.4.3 has no endpoint override for its signed planner; never
			// rewrite a signed URL. Decline (a future nanollm feature can remove this).
			return nil, declinef(StageClientOption, ReasonEndpointOverride,
				"aws-bedrock endpoint_url override is unproven (nanollm signs the default host)"), nil
		case "headers":
			return nil, declinef(StageClientOption, ReasonHeadersOption,
				"aws-bedrock custom headers are unproven (they would break the SigV4 signature)"), nil
		default:
			return nil, declinef(StageClientOption, ReasonUnprovenClientOption,
				"aws-bedrock client carries an unproven option"), nil
		}
	}

	// Exactly one of model / model_id.
	if modelSet == modelIDSet {
		return nil, declinef(StageClientSelection, ReasonBedrockModelKeys,
			"aws-bedrock requires exactly one of model / model_id (both or neither is ambiguous)"), nil
	}
	target := modelVal
	if modelIDSet {
		target = modelIDVal
	}
	// Require a valid UTF-8 target model, matching the strict OpenAI path.
	if !utf8.ValidString(target) {
		return nil, declinef(StageClientSelection, ReasonModelNotLiteral,
			"aws-bedrock target model is not valid UTF-8"), nil
	}

	// Region precedence: explicit -> AWS_REGION -> AWS_DEFAULT_REGION; nonempty.
	region := regionOpt
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		return nil, declinef(StageCredentialSource, ReasonRegionMissing,
			"aws-bedrock region is required (explicit region option, else AWS_REGION / AWS_DEFAULT_REGION)"), nil
	}
	if !utf8.ValidString(region) {
		return nil, declinef(StageCredentialSource, ReasonRegionMissing,
			"aws-bedrock region is not valid UTF-8"), nil
	}

	if dec := validateAlias(in.alias, target, cp.Name); dec != nil {
		return nil, dec, nil
	}

	bp, dec := resolveBedrockCreds(ctx, cp.Name, region, sel, additionalFields)
	if dec != nil {
		return nil, dec, nil
	}

	return &mappedClient{
		registryProvider: registryProvider,
		nanollmProvider:  nanollmProvider,
		target:           target,
		alias:            in.alias,
		bedrock:          bp,
		body:             infConfig,
		verification:     PolicyTrustedProvider,
	}, nil, nil
}

// resolveBedrockCreds resolves the aws-bedrock credentials to concrete literals
// via the existing baml-rest/AWS selector (static > profile > default chain),
// retrieving a fresh credential value PER-ADMISSION. A declared-but-broken source
// (partial static, empty/failed profile) declines to BAML — it NEVER silently
// falls to the default chain / ambient credentials. Nothing declared uses the AWS
// default chain (env / shared config / ECS-IMDS / SSO — owner decision A). A
// resolved-but-empty credential declines rather than sending an unsigned request.
func resolveBedrockCreds(ctx context.Context, clientName, region string, sel llmhttp.BedrockCredentialSelector, additional map[string]any) (*bedrockParams, *Decline) {
	staticDeclared := sel.AccessKeyIDPresent || sel.SecretAccessKeyPresent || sel.SessionTokenPresent

	// A partial static pair is a confident configuration error: never fall through
	// to profile / default chain (a typo could otherwise escalate to whatever creds
	// happen to be on the host).
	if staticDeclared && !(sel.AccessKeyIDPresent && sel.SecretAccessKeyPresent) {
		return nil, declinef(StageCredentialSource, ReasonPartialCredentials,
			"aws-bedrock static credentials are incomplete (need both access_key_id and secret_access_key)")
	}

	var source BedrockCredentialSource
	switch {
	case staticDeclared:
		source = BedrockCredentialExplicit
	case sel.ProfilePresent:
		source = BedrockCredentialProfile
	default:
		source = BedrockCredentialDefaultChain
	}

	// Bound the pre-claim AWS credential I/O when the admission context carries no
	// deadline (an earlier caller deadline is preserved), so a request arriving with
	// an unbounded context can never block admission on an unavailable ECS/IMDS.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bedrockCredentialResolveTimeout)
		defer cancel()
	}

	// Retrieve the resolved credential value immediately before nanollm.New. A
	// declared-but-broken source (partial static already handled; empty/failed
	// profile) returns an error here — never a silent fall to ambient. The SDK owns
	// any safe caching; we never cache a flattened secret beyond this call.
	rc, rerr := awsCredentialRetriever(ctx, clientName, sel)
	if rerr != nil {
		return nil, declinef(StageCredentialSource, ReasonCredentialResolution,
			"aws-bedrock credential source could not be resolved")
	}
	if rc.accessKeyID == "" || rc.secretAccessKey == "" {
		return nil, declinef(StageCredentialSource, ReasonMissingCredential,
			"aws-bedrock resolved credentials are empty")
	}
	// Resolved credential literals are written into the config JSON (via private Env
	// placeholders); a non-UTF-8 value would break the marshal, so take the stable
	// pre-FFI decline. The value is never surfaced.
	if !utf8.ValidString(rc.accessKeyID) || !utf8.ValidString(rc.secretAccessKey) || !utf8.ValidString(rc.sessionToken) {
		return nil, declinef(StageCredentialSource, ReasonCredentialResolution,
			"aws-bedrock resolved credentials are not valid UTF-8")
	}

	// A DECLARED profile must be sourced FROM that profile's own credential
	// mechanism. The AWS SDK's shared-config resolution falls through to ambient
	// env / ECS / IMDS credentials when a named profile has no configured credential
	// source, so a resolved credential whose SDK Source is an ambient provider means
	// the declared profile silently used host creds — decline to BAML rather than
	// admit ambient credentials under a "profile" label. (The no-declared-source
	// default-chain branch is allowed to use ambient creds per owner decision A.)
	if sel.ProfilePresent {
		if _, ambient := ambientBedrockCredentialSources[rc.source]; ambient {
			return nil, declinef(StageCredentialSource, ReasonUnsupportedCredentialSource,
				"aws-bedrock declared profile fell through to ambient credentials")
		}
	}

	return &bedrockParams{
		region:                       region,
		accessKeyID:                  rc.accessKeyID,
		secretAccessKey:              rc.secretAccessKey,
		sessionToken:                 rc.sessionToken,
		additionalModelRequestFields: additional,
		source:                       source,
	}, nil
}

// bedrockCredString validates a bedrock credential/profile option as a resolved
// literal string (empty is allowed — the AWS selector rejects an empty resolved
// static/profile value). A non-string value is a malformed credential option and
// declines partial_credentials. The value is NEVER surfaced.
func bedrockCredString(raw any) (string, *Decline) {
	s, ok := raw.(string)
	if !ok {
		return "", declinef(StageCredentialSource, ReasonPartialCredentials,
			"aws-bedrock credential option is not a resolved literal string")
	}
	return s, nil
}

// parseInferenceConfig projects a Bedrock inference_configuration object onto the
// canonical OpenAI body fields (§4.5): max_tokens/temperature/top_p/stop_sequences
// -> ChatRequest max_tokens/temperature/top_p/stop. Any other field, or a
// malformed value, declines invalid_body_option (fail-closed, never silently dropped).
func parseInferenceConfig(b *bodyOptions, raw any) *Decline {
	obj, ok := raw.(map[string]any)
	if !ok {
		return declinef(StageClientOption, ReasonInvalidBodyOption,
			"aws-bedrock inference_configuration is not a JSON object")
	}
	invalid := func() *Decline {
		return declinef(StageClientOption, ReasonInvalidBodyOption,
			"aws-bedrock inference_configuration carries a malformed or unproven field")
	}
	for _, key := range sortedOptionKeys(obj) {
		v := obj[key]
		switch key {
		case "max_tokens":
			// Count-like: reject NEGATIVE before engine construction (matching the
			// common bearer mapper's guard — nanollm ChatRequest.Build does no semantic
			// validation). Zero is retained (Anthropic/Bedrock allow 0). max_tokens is
			// the only count-like INTEGER in inference_configuration (temperature/top_p
			// are floats, stop_sequences is strings), so no other integer needs a guard.
			n, ok := asInt(v)
			if !ok || n < 0 {
				return invalid()
			}
			b.maxTokens = &n
		case "temperature":
			f, ok := asFloat64(v)
			if !ok {
				return invalid()
			}
			b.temperature = &f
		case "top_p":
			f, ok := asFloat64(v)
			if !ok {
				return invalid()
			}
			b.topP = &f
		case "stop_sequences":
			ss, ok := asStringSlice(v)
			if !ok || len(ss) == 0 {
				return invalid()
			}
			b.stop = ss
		default:
			return invalid()
		}
	}
	return nil
}

// deepCopyJSONObject returns an owned deep copy of a JSON object option, rejecting
// a non-object or a value carrying a non-JSON leaf (so a caller cannot mutate a
// reused registry concurrently with admission, and no non-JSON value reaches FFI).
func deepCopyJSONObject(raw any) (map[string]any, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	cp, ok := deepCopyJSONValue(m)
	if !ok {
		return nil, false
	}
	return cp.(map[string]any), true
}

// deepCopyJSONValue deep-copies a JSON-compatible value tree, returning false for
// any non-JSON leaf: a non-UTF-8 string, a non-finite float (JSON has no NaN/Inf),
// or a malformed json.Number. These would otherwise cross the mapping boundary and
// fail later in config/body serialization instead of taking a stable
// invalid_body_option pre-FFI decline. Validation is recursive over the whole tree.
func deepCopyJSONValue(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			// A JSON object key must be a valid UTF-8 string (a non-UTF-8 key would
			// break serialization / cross into Params unvalidated).
			if !utf8.ValidString(k) {
				return nil, false
			}
			cv, ok := deepCopyJSONValue(val)
			if !ok {
				return nil, false
			}
			out[k] = cv
		}
		return out, true
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			cv, ok := deepCopyJSONValue(val)
			if !ok {
				return nil, false
			}
			out[i] = cv
		}
		return out, true
	case string:
		if !utf8.ValidString(t) {
			return nil, false
		}
		return t, true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, false
		}
		return t, true
	case float32:
		f := float64(t)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return t, true
	case json.Number:
		// A json.Number is valid ONLY if it satisfies the JSON-number grammar and is
		// finite — Float64() alone accepts NaN/Inf and non-JSON spellings like "01".
		if !validJSONNumber(t) {
			return nil, false
		}
		return t, true
	case bool, int, int32, int64, nil:
		return t, true
	default:
		return nil, false
	}
}
