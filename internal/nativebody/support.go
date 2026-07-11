// Package nativebody is the de-BAML Phase 4a native canonical OpenAI
// chat-completions body builder. Given a native
// [nativeprompt.RenderedPrompt] and a normalized selected-client record, it
// produces the EXACT canonical request bytes BAML v0.223 would emit — proven
// byte-for-byte against BAML itself by the differential oracles — or a
// fail-closed decline.
//
// It is pure Go and TEST-ONLY in Phase 4a: no serving path calls it, it sends
// nothing, and it has no BAML or nanollm production dependency. It intentionally
// claims one deliberately narrow surface (openai provider, literal target model,
// non-streaming chat completions, ordered standard system/user/assistant
// text-only messages, and completions realized as a single system message) and
// DECLINES everything else, mirroring the fail-closed ErrUnsupported/Decline
// convention of internal/nativeprompt. Native never out-claims BAML: when in
// doubt it declines, and a decline means BAML remains authoritative.
//
// The load-bearing model separation: the BAML/OpenAI TARGET model goes in the
// top-level JSON `model`; the separate baml-rest/nanollm ALIAS is carried as
// metadata ONLY (for a later nanollm Request.Model) and is NEVER substituted
// into the JSON model field.
package nativebody

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// ErrUnsupported is the sentinel wrapped by every decline. A caller uses
// errors.Is(err, ErrUnsupported) to route the request to the authoritative BAML
// path instead of treating the decline as a hard failure — the same contract as
// [nativeprompt.ErrUnsupported].
var ErrUnsupported = errors.New("nativebody: unsupported OpenAI chat body shape")

// Decline is the concrete decline error. Feature names the unsupported feature
// class; Detail is a specific human-readable reason. It unwraps to
// [ErrUnsupported].
type Decline struct {
	Feature string
	Detail  string
}

func (d *Decline) Error() string {
	return fmt.Sprintf("%s: %s (%s)", ErrUnsupported.Error(), d.Detail, d.Feature)
}

func (d *Decline) Unwrap() error { return ErrUnsupported }

func decline(feature, detail string) *Decline { return &Decline{Feature: feature, Detail: detail} }

// Feature keys for declines. They enumerate the parity boundary of Phase 4a: any
// input that trips one is outside the proven byte-exact claim and must fall back
// to BAML.
const (
	// FeatureProvider: a non-openai (or empty) provider. openai is the only
	// proven body/oracle provider.
	FeatureProvider = "provider"
	// FeatureModelSelection: absent / non-literal / dynamic / ambiguous target
	// model. 4a admits only a resolved, literal, nonempty target model.
	FeatureModelSelection = "model_selection"
	// FeatureModelEscape: a REGULAR (non-raw) string model literal that carries an
	// escape sequence. BAML decodes regular string literals (e.g. `"gpt\t4"` ->
	// a real tab) before building the request, while the passive descriptor retains
	// the raw escape text; native cannot prove the decoded form byte-for-byte, so
	// it declines (over-decline). Raw-string literals (`#"..."#`) are verbatim in
	// BAML and are NOT declined.
	FeatureModelEscape = "model_escape"
	// FeatureClientSelection: no unambiguous selected client in a dynamic
	// registry (no primary, none/multiple/duplicate/empty-name clients, missing
	// primary). BAML's map-backed AddLlmClient is last-wins on a duplicate name,
	// while a forward scan is first-wins — an ambiguity native declines rather than
	// risk selecting a different client than BAML.
	FeatureClientSelection = "client_selection"
	// FeatureInvalidUTF8: a body-bound string (target model, completion, or a
	// text part) is not valid UTF-8. BAML rejects invalid UTF-8 at its CFFI/
	// protobuf boundary ("string field contains invalid UTF-8") and builds NO
	// request, so native must fail closed rather than emit raw invalid bytes.
	FeatureInvalidUTF8 = "invalid_utf8"
	// FeatureRequestBody: an explicit `request_body` block/option — the special
	// passthrough BAML flattens into the JSON body. Not proven yet, so it declines.
	FeatureRequestBody = "request_body"
	// FeatureTools: a tools / tool_choice / functions / function_call option
	// (tool calling). Outside the proven surface.
	FeatureTools = "tools"
	// FeatureResponseFormat: a response_format / response schema option.
	FeatureResponseFormat = "response_format"
	// FeatureClientOption: any OTHER body-affecting client option that cannot be
	// represented in a source-ordered form (e.g. a bare `temperature`, or an entry
	// in the unordered dynamic Options map that is neither the model nor a
	// recognized transport-only/named-body key). BAML flattens such options into
	// the JSON body (proven for temperature), so the unordered dynamic map cannot
	// reproduce their order and they decline.
	FeatureClientOption = "client_option"
	// FeatureStreaming: a streaming request/attempt. BAML's stream request emits
	// `"stream":true,"stream_options":{...}` after messages; 4a proves only the
	// non-streaming chat-completions body, so every stream mode declines.
	FeatureStreaming = "streaming"
	// FeatureRenderedPrompt: a nil / unknown-kind rendered prompt.
	FeatureRenderedPrompt = "rendered_prompt"
	// FeatureEmptyMessages: a chat prompt with no messages.
	FeatureEmptyMessages = "empty_messages"
	// FeatureRole: a role outside the proven system/user/assistant set (custom /
	// default / remapped roles).
	FeatureRole = "role"
	// FeatureAllowDuplicateRole: a message with allow_duplicate_role set.
	FeatureAllowDuplicateRole = "allow_duplicate_role"
	// FeatureMessageMeta: message-level metadata (e.g. cache_control) — role
	// metadata is outside the proven text-only claim.
	FeatureMessageMeta = "message_meta"
	// FeatureEmptyMessage: a message with no content parts.
	FeatureEmptyMessage = "empty_message"
	// FeatureMediaPart: an image/audio/pdf/video/media content part.
	FeatureMediaPart = "media_part"
	// FeatureUnknownPart: a content part that is neither text nor media (an
	// unreconstructable rendered part).
	FeatureUnknownPart = "unknown_part"
)

// ProviderOpenAI is the one provider Phase 4a proves for the chat body.
const ProviderOpenAI = "openai"

// admittedRoles is the proven standard OpenAI chat role set. Any other role
// (tool/function/developer/custom, or a remapped/default role) declines: only
// these three are proven byte-exact against BAML, and a completion is realized
// as a single "system" message.
var admittedRoles = map[string]bool{
	"system":    true,
	"user":      true,
	"assistant": true,
}

// BodyAffectingOption names one client option that would affect the JSON body
// but is not proven, together with the concrete decline [Feature] it maps to
// (FeatureRequestBody / FeatureTools / FeatureResponseFormat / FeatureClientOption).
// Carrying the feature per option lets the admission gate return the specific
// named decline rather than a generic one.
type BodyAffectingOption struct {
	Key     string
	Feature string
}

// ClientIntent is the normalized, body-relevant view of the selected BAML client
// for one attempt. It is the ONLY client input [BuildOpenAIChat] /
// [SupportsOpenAIChat] read; the two normalizers ([NormalizeDynamicClient] and
// [NormalizeStaticClient]) produce it from the dynamic registry or the static
// descriptor respectively. Transport-only options (base_url/api_key/headers) are
// intentionally absent: 4a neither serializes nor needs them.
type ClientIntent struct {
	// Provider is the normalized BAML provider (e.g. "openai").
	Provider string
	// TargetModel is the resolved literal model string BAML puts in the top-level
	// JSON "model" field.
	TargetModel string
	// ModelAlias is the SEPARATE baml-rest/nanollm alias. It is carried as
	// metadata ONLY and is NEVER substituted into the JSON "model" field.
	ModelAlias string
	// Stream marks a streaming request/attempt. BAML's stream request emits an
	// extra `"stream":true,"stream_options":{...}` suffix, so 4a — which proves
	// only the non-streaming body — declines it (FeatureStreaming). The normalizers
	// set this from the caller's request mode; a non-streaming attempt leaves it
	// false (the admitted mode).
	Stream bool
	// BodyAffecting is the ordered list of body-affecting client options (the
	// special request_body, tools, response_format, or an unrecognized flattened
	// option), each tagged with the concrete decline feature it maps to. 4a proves
	// none of them, so a non-empty list declines. The full ordered typed tree lives
	// in [promptdescriptor.ClientConfig] for future phases; here the key + feature
	// matter.
	BodyAffecting []BodyAffectingOption
}

// bodyAffectingFeature classifies a body-affecting client-option key into its
// concrete decline feature. request_body is classified by the caller (it is a
// dedicated block); this handles the top-level flattened options.
func bodyAffectingFeature(key string) string {
	switch key {
	case "tools", "tool_choice", "functions", "function_call", "parallel_tool_calls":
		return FeatureTools
	case "response_format":
		return FeatureResponseFormat
	case "request_body":
		return FeatureRequestBody
	default:
		return FeatureClientOption
	}
}

// SupportsOpenAIChat is the fail-closed admission predicate. It returns nil when
// [BuildOpenAIChat] is proven to reproduce BAML's exact bytes for
// (rendered, client), and a *Decline (unwrapping to [ErrUnsupported]) otherwise.
// BuildOpenAIChat calls it first, so a nil result guarantees BuildOpenAIChat
// serializes without a new decline category.
func SupportsOpenAIChat(rendered *nativeprompt.RenderedPrompt, client ClientIntent) error {
	if err := supportsClient(client); err != nil {
		return err
	}
	return supportsRendered(rendered)
}

// supportsClient gates the normalized client record: openai only, a resolved
// literal nonempty target model, non-streaming only, and no body-affecting
// options (each reported with its concrete named feature).
func supportsClient(client ClientIntent) error {
	if client.Provider != ProviderOpenAI {
		return decline(FeatureProvider,
			fmt.Sprintf("provider %q is not the proven openai provider", client.Provider))
	}
	if client.TargetModel == "" {
		return decline(FeatureModelSelection,
			"selected client has no resolved literal target model")
	}
	if !utf8.ValidString(client.TargetModel) {
		return decline(FeatureInvalidUTF8,
			"target model is not valid UTF-8 (BAML rejects it at its protobuf/CFFI boundary)")
	}
	if client.Stream {
		return decline(FeatureStreaming,
			"streaming attempts emit an extra stream/stream_options suffix; 4a proves only the non-streaming chat-completions body")
	}
	if len(client.BodyAffecting) > 0 {
		first := client.BodyAffecting[0]
		return decline(first.Feature,
			fmt.Sprintf("client carries unproven body-affecting option %q; 4a admits only an absent request_body (an explicit request_body {} declines: BAML emits \"request_body\":{})", first.Key))
	}
	return nil
}

// supportsRendered gates the rendered prompt: a completion (realized as a single
// system message) is always admitted; a chat must be a nonempty ordered list of
// standard-role, metadata-free, text-only messages.
func supportsRendered(rp *nativeprompt.RenderedPrompt) error {
	if rp == nil {
		return decline(FeatureRenderedPrompt, "nil rendered prompt")
	}
	switch rp.Kind {
	case nativeprompt.KindCompletion:
		// A role-less completion is realized by BAML's stock openai provider as a
		// single "system" message carrying the completion text — proven by the
		// static oracle. The only remaining gate is UTF-8 validity (BAML rejects
		// invalid UTF-8 before building any request).
		if !utf8.ValidString(rp.Completion) {
			return decline(FeatureInvalidUTF8,
				"completion text is not valid UTF-8 (BAML rejects it at its protobuf/CFFI boundary)")
		}
		return nil
	case nativeprompt.KindChat:
		if len(rp.Messages) == 0 {
			return decline(FeatureEmptyMessages, "chat prompt has no messages")
		}
		for i := range rp.Messages {
			if err := supportsMessage(&rp.Messages[i]); err != nil {
				return err
			}
		}
		return nil
	default:
		return decline(FeatureRenderedPrompt, fmt.Sprintf("unknown rendered kind %q", rp.Kind))
	}
}

// supportsMessage gates one chat message: a proven standard role, no
// allow_duplicate_role, no message metadata, and at least one part with every
// part being text (no media/unknown parts).
func supportsMessage(m *nativeprompt.RenderedMessage) error {
	if !admittedRoles[m.Role] {
		return decline(FeatureRole,
			fmt.Sprintf("role %q is outside the proven system/user/assistant set", m.Role))
	}
	if m.AllowDuplicateRole {
		return decline(FeatureAllowDuplicateRole,
			"allow_duplicate_role is not proven byte-exact")
	}
	if m.Meta != nil {
		return decline(FeatureMessageMeta,
			"message metadata (e.g. cache_control) is outside the proven text-only claim")
	}
	if len(m.Parts) == 0 {
		return decline(FeatureEmptyMessage, "message has no content parts")
	}
	for i := range m.Parts {
		p := &m.Parts[i]
		if p.Media != nil {
			return decline(FeatureMediaPart,
				"media content parts (image/audio/pdf/video) are outside the proven text-only claim")
		}
		if p.Text == nil {
			return decline(FeatureUnknownPart,
				"content part is neither text nor media")
		}
		if !utf8.ValidString(*p.Text) {
			return decline(FeatureInvalidUTF8,
				"text part is not valid UTF-8 (BAML rejects it at its protobuf/CFFI boundary)")
		}
	}
	return nil
}

// NormalizeDynamicClient normalizes the selected client of a dynamic
// [bamlutils.ClientRegistry] into a [ClientIntent], carrying the separate
// nanollm alias. It selects the effective client (the Primary, or the sole
// client when no primary is named), records its provider and literal model, and
// classifies every other option:
//
//   - a recognized transport-only key (base_url/api_key/headers) is ignored — it
//     is not a chat-body field;
//   - anything else is body-affecting. The dynamic Options map does NOT preserve
//     BAML request_body order, so generic map-to-JSON is a parity bug; such keys
//     are recorded (sorted, for a stable message) and the admission gate then
//     declines them. This is intentionally conservative: over-declining is safe.
//
// stream marks a streaming attempt (the caller knows whether this is a
// Request or a StreamRequest); it flows into [ClientIntent.Stream] and declines
// via [SupportsOpenAIChat].
//
// It fail-closes (returns a *Decline) on an absent/ambiguous client selection or
// a missing/non-string/empty model. The provider is recorded verbatim and gated
// later by [SupportsOpenAIChat].
func NormalizeDynamicClient(reg *bamlutils.ClientRegistry, alias string, stream bool) (ClientIntent, error) {
	cp, err := selectDynamicClient(reg)
	if err != nil {
		return ClientIntent{}, err
	}

	intent := ClientIntent{Provider: cp.Provider, ModelAlias: alias, Stream: stream}

	var bodyAffecting []BodyAffectingOption
	for key, raw := range cp.Options {
		switch {
		case key == "model":
			s, ok := raw.(string)
			if !ok || s == "" {
				return ClientIntent{}, decline(FeatureModelSelection,
					fmt.Sprintf("selected client model option is absent/empty/non-string (%T)", raw))
			}
			intent.TargetModel = s
		case transportOnlyDynamicOptions[key]:
			// transport-only: retained as future intent, not a body field.
		default:
			bodyAffecting = append(bodyAffecting, BodyAffectingOption{Key: key, Feature: bodyAffectingFeature(key)})
		}
	}
	if intent.TargetModel == "" {
		return ClientIntent{}, decline(FeatureModelSelection,
			"selected client has no model option")
	}
	// The dynamic Options map has no source order, so sort by key for a stable,
	// deterministic decline (the presence — not the order — is what declines).
	sort.Slice(bodyAffecting, func(i, j int) bool { return bodyAffecting[i].Key < bodyAffecting[j].Key })
	intent.BodyAffecting = bodyAffecting
	return intent, nil
}

// transportOnlyDynamicOptions is the recognized transport-only option set for
// the dynamic registry path, mirroring the static extractor's set. Kept minimal
// and explicit: every other option key is treated as body-affecting.
var transportOnlyDynamicOptions = map[string]bool{
	"api_key":  true,
	"base_url": true,
	"headers":  true,
}

// selectDynamicClient picks the effective client from a runtime registry: the
// Primary by name when set, else the sole client when exactly one is present.
// Anything else (nil registry, no clients, a named-but-missing primary, or
// multiple clients without a primary) is an ambiguous selection and declines.
func selectDynamicClient(reg *bamlutils.ClientRegistry) (*bamlutils.ClientProperty, error) {
	if reg == nil {
		return nil, decline(FeatureClientSelection, "no client registry supplied")
	}
	// Reject duplicate/empty client names BEFORE selecting. BAML's map-backed
	// AddLlmClient is LAST-WINS on a duplicate name, whereas the forward scan below
	// is FIRST-WINS — so a duplicate name could make native select a different
	// client than BAML actually runs. Reuse the same invariant the adapter entry
	// points enforce (bamlutils.ClientRegistry.Validate) and decline on violation.
	if err := reg.Validate(); err != nil {
		return nil, decline(FeatureClientSelection,
			fmt.Sprintf("ambiguous client registry: %v", err))
	}
	clients := make([]*bamlutils.ClientProperty, 0, len(reg.Clients))
	for _, c := range reg.Clients {
		if c != nil {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		return nil, decline(FeatureClientSelection, "client registry has no clients")
	}
	if reg.Primary != nil && *reg.Primary != "" {
		for _, c := range clients {
			if c.Name == *reg.Primary {
				return c, nil
			}
		}
		return nil, decline(FeatureClientSelection,
			fmt.Sprintf("primary client %q not found in registry", *reg.Primary))
	}
	if len(clients) == 1 {
		return clients[0], nil
	}
	return nil, decline(FeatureClientSelection,
		"registry has multiple clients but no primary; selection is ambiguous")
}

// NormalizeStaticClient normalizes a static [promptdescriptor.ClientConfig] into
// a [ClientIntent], carrying the separate nanollm alias and the stream mode. It
// admits the target model only when it is a resolved LITERAL (env-derived /
// dynamic / absent models decline), and records every request_body entry and
// body-affecting option (source order preserved, each tagged with its concrete
// decline feature) so the admission gate declines a client that carries any.
// Transport-only options are intentionally dropped (future intent only).
func NormalizeStaticClient(cfg promptdescriptor.ClientConfig, alias string, stream bool) (ClientIntent, error) {
	intent := ClientIntent{Provider: cfg.Provider, ModelAlias: alias, Stream: stream}

	if cfg.Model.Provenance != promptdescriptor.ModelProvenanceLiteral || cfg.Model.Value == "" {
		return intent, decline(FeatureModelSelection,
			fmt.Sprintf("static client model is not a resolved literal (provenance %q)", cfg.Model.Provenance))
	}
	// A REGULAR string literal bearing a backslash carries an escape sequence BAML
	// decodes (e.g. `"gpt\t4"` -> a real tab) but the descriptor retains verbatim;
	// native cannot prove the decoded bytes, so it declines. RAW-string literals
	// (`#"..."#`) are verbatim in BAML and admitted even with a backslash.
	if !cfg.Model.RawString && strings.ContainsRune(cfg.Model.Value, '\\') {
		return intent, decline(FeatureModelEscape,
			"regular string model literal contains an escape sequence; BAML decodes it and native cannot prove the decoded model byte-for-byte")
	}
	intent.TargetModel = cfg.Model.Value

	var bodyAffecting []BodyAffectingOption
	// A declared request_body block is body-affecting (FeatureRequestBody) — BAML
	// v0.223 emits a "request_body":{...} object whenever the block is present,
	// INCLUDING an explicit empty `request_body {}` (verified against the oracle:
	// an empty block yields "request_body":{} in the body, which native does not
	// reproduce). So the block's PRESENCE declines, empty or not. A present-empty
	// block declines under the bare "request_body" key; a nonempty one declines
	// per entry (preserving the descriptor's source order).
	if cfg.RequestBodyPresent && len(cfg.RequestBody) == 0 {
		bodyAffecting = append(bodyAffecting, BodyAffectingOption{Key: "request_body", Feature: FeatureRequestBody})
	}
	for _, e := range cfg.RequestBody {
		bodyAffecting = append(bodyAffecting, BodyAffectingOption{Key: "request_body." + e.Key, Feature: FeatureRequestBody})
	}
	// Other flattened options keep their concrete feature (tools/response_format/
	// generic) and source order.
	for _, o := range cfg.BodyAffectingOptions {
		bodyAffecting = append(bodyAffecting, BodyAffectingOption{Key: o.Key, Feature: bodyAffectingFeature(o.Key)})
	}
	intent.BodyAffecting = bodyAffecting
	return intent, nil
}
