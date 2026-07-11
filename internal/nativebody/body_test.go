package nativebody

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

func sp(s string) *string { return &s }

func textPart(s string) nativeprompt.Part { return nativeprompt.Part{Text: sp(s)} }

func chat(msgs ...nativeprompt.RenderedMessage) *nativeprompt.RenderedPrompt {
	return &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindChat, Messages: msgs}
}

func msg(role string, parts ...nativeprompt.Part) nativeprompt.RenderedMessage {
	return nativeprompt.RenderedMessage{Role: role, Parts: parts}
}

func openaiClient(model string) ClientIntent {
	return ClientIntent{Provider: ProviderOpenAI, TargetModel: model}
}

// TestInvalidUTF8Declines proves every body-bound string (target model,
// completion, text part) fails closed with FeatureInvalidUTF8 when not valid
// UTF-8, so native never emits raw bytes BAML rejects at its protobuf/CFFI
// boundary. (See TestNativeBodyDynamicOracleInvalidUTF8 for the BAML anchor.)
func TestInvalidUTF8Declines(t *testing.T) {
	const badByte = "x\xffy" // invalid UTF-8
	cases := []struct {
		name     string
		rendered *nativeprompt.RenderedPrompt
		client   ClientIntent
	}{
		{"target_model", chat(msg("user", textPart("hi"))), openaiClient(badByte)},
		{"completion", &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: badByte}, openaiClient("m")},
		{"text_part", chat(msg("user", textPart(badByte))), openaiClient("m")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SupportsOpenAIChat(tc.rendered, tc.client)
			var d *Decline
			if !errors.As(err, &d) || d.Feature != FeatureInvalidUTF8 {
				t.Fatalf("want FeatureInvalidUTF8, got %v", err)
			}
			if body, berr := BuildOpenAIChat(tc.rendered, tc.client); body != nil || berr == nil {
				t.Fatalf("BuildOpenAIChat must decline invalid UTF-8, got body=%v err=%v", body, berr)
			}
		})
	}
}

// TestDuplicateDynamicClientDeclines proves the dynamic normalizer rejects a
// registry with duplicate/empty client names (BAML's AddLlmClient is last-wins,
// a forward scan is first-wins) rather than silently selecting a client that may
// differ from BAML's.
func TestDuplicateDynamicClientDeclines(t *testing.T) {
	prim := "Dup"
	dup := &bamlutils.ClientRegistry{
		Primary: &prim,
		Clients: []*bamlutils.ClientProperty{
			{Name: "Dup", Provider: "openai", Options: map[string]any{"model": "first"}},
			{Name: "Dup", Provider: "openai", Options: map[string]any{"model": "second"}},
		},
	}
	var d *Decline
	if _, err := NormalizeDynamicClient(dup, "a", false); !errors.As(err, &d) || d.Feature != FeatureClientSelection {
		t.Fatalf("duplicate client name must decline FeatureClientSelection, got %v", err)
	}

	empty := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{Name: "", Provider: "openai", Options: map[string]any{"model": "m"}}},
	}
	if _, err := NormalizeDynamicClient(empty, "a", false); !errors.As(err, &d) || d.Feature != FeatureClientSelection {
		t.Fatalf("empty client name must decline FeatureClientSelection, got %v", err)
	}
}

// TestEscapedModelLiteralDeclines proves a REGULAR string model literal bearing
// an escape sequence declines FeatureModelEscape (BAML decodes it; native cannot
// prove the decoded bytes), while a RAW-string literal (verbatim in BAML) and an
// escape-free regular literal are admitted.
func TestEscapedModelLiteralDeclines(t *testing.T) {
	base := promptdescriptor.ClientConfig{Present: true, Name: "C", Provider: "openai"}

	// Regular literal with a backslash escape -> decline.
	regEsc := base
	regEsc.Model = promptdescriptor.ClientModel{Value: `gpt\t4`, Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: false}
	var d *Decline
	if _, err := NormalizeStaticClient(regEsc, "a", false); !errors.As(err, &d) || d.Feature != FeatureModelEscape {
		t.Fatalf("escaped regular model literal must decline FeatureModelEscape, got %v", err)
	}

	// Raw-string literal with a backslash -> verbatim in BAML -> admitted.
	rawEsc := base
	rawEsc.Model = promptdescriptor.ClientModel{Value: `gpt\t4`, Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: true}
	intent, err := NormalizeStaticClient(rawEsc, "a", false)
	if err != nil {
		t.Fatalf("raw-string model literal must be admitted, got %v", err)
	}
	if intent.TargetModel != `gpt\t4` {
		t.Fatalf("raw-string model must be verbatim, got %q", intent.TargetModel)
	}

	// Escape-free regular literal -> decoded == raw -> admitted.
	plain := base
	plain.Model = promptdescriptor.ClientModel{Value: "gpt-4o", Provenance: promptdescriptor.ModelProvenanceLiteral, RawString: false}
	if _, err := NormalizeStaticClient(plain, "a", false); err != nil {
		t.Fatalf("escape-free regular literal must be admitted, got %v", err)
	}
}

// TestBuildOpenAIChatExactBytes pins BuildOpenAIChat against the exact canonical
// bytes BAML v0.223 emits (captured from the live BAML oracle): top-level model,
// then messages, content as an array of text blocks, compact, in that key order.
func TestBuildOpenAIChatExactBytes(t *testing.T) {
	cases := []struct {
		name     string
		rendered *nativeprompt.RenderedPrompt
		model    string
		want     string
	}{
		{
			name:     "completion_realized_as_system_message",
			rendered: &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: "Write a short, plain note about cats and dogs."},
			model:    "fake-static-oracle-model",
			want:     `{"model":"fake-static-oracle-model","messages":[{"role":"system","content":[{"type":"text","text":"Write a short, plain note about cats and dogs."}]}]}`,
		},
		{
			name:     "single_user_message",
			rendered: chat(msg("user", textPart("What is 2+2?"))),
			model:    "gpt-4o-mini",
			want:     `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"What is 2+2?"}]}]}`,
		},
		{
			name: "ordered_system_user_assistant",
			rendered: chat(
				msg("system", textPart("You are a concise assistant.")),
				msg("user", textPart("Tell me about weather.")),
				msg("assistant", textPart("Here are 3 facts.")),
			),
			model: "gpt-4o-mini",
			want:  `{"model":"gpt-4o-mini","messages":[{"role":"system","content":[{"type":"text","text":"You are a concise assistant."}]},{"role":"user","content":[{"type":"text","text":"Tell me about weather."}]},{"role":"assistant","content":[{"type":"text","text":"Here are 3 facts."}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := BuildOpenAIChat(tc.rendered, openaiClient(tc.model))
			if err != nil {
				t.Fatalf("BuildOpenAIChat: %v", err)
			}
			if got := string(body.Bytes()); got != tc.want {
				t.Errorf("bytes mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestWriteJSONStringMatchesSerdeJSON pins the escaper against the exact bytes
// BAML/serde_json produces (captured from the live oracle) for the escaping cases
// where Go's encoding/json defaults diverge.
func TestWriteJSONStringMatchesSerdeJSON(t *testing.T) {
	esc := func(b byte) string { const h = "0123456789abcdef"; return "\\u00" + string(h[b>>4]) + string(h[b&0xf]) }
	cases := []struct{ in, want string }{
		{"a<b>c&d", `"a<b>c&d"`},                                           // HTML chars NOT escaped (Go escapes them)
		{"x\bx\fx", `"x\bx\fx"`},                                           // short \b \f escapes
		{"tab\tval\x01end", `"tab\tval` + esc(0x01) + `end"`},              // \t short; other control -> \u00XX
		{"a\u2028b\u2029c", "\"a\u2028b\u2029c\""},                         // line/para sep NOT escaped
		{"a\x7fb", "\"a\x7fb\""},                                           // DEL (0x7f) not escaped
		{"caf\u00e9 \u2615 \U0001F600", "\"caf\u00e9 \u2615 \U0001F600\""}, // raw UTF-8 passthrough
		{"q\"q\\q", `"q\"q\\q"`},                                           // quote + backslash
		{"nl\ncr\r", `"nl\ncr\r"`},                                         // \n \r short escapes
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		writeJSONString(&buf, tc.in)
		if got := buf.String(); got != tc.want {
			t.Errorf("writeJSONString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestModelAliasSeparation proves the nanollm alias is carried as metadata only
// and NEVER substituted into the JSON model field.
func TestModelAliasSeparation(t *testing.T) {
	client := ClientIntent{Provider: ProviderOpenAI, TargetModel: "gpt-4o", ModelAlias: "openai/gpt-4o"}
	body, err := BuildOpenAIChat(chat(msg("user", textPart("hi"))), client)
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	got := string(body.Bytes())
	if !strings.Contains(got, `"model":"gpt-4o"`) {
		t.Errorf("JSON model must be the target model; got %s", got)
	}
	if strings.Contains(got, "openai/gpt-4o") {
		t.Errorf("nanollm alias leaked into JSON body: %s", got)
	}
	if body.TargetModel() != "gpt-4o" || body.ModelAlias() != "openai/gpt-4o" || body.Provider() != "openai" {
		t.Errorf("metadata mismatch: target=%q alias=%q provider=%q", body.TargetModel(), body.ModelAlias(), body.Provider())
	}
}

// TestBytesImmutable proves Bytes returns a copy that cannot mutate the body.
func TestBytesImmutable(t *testing.T) {
	body, err := BuildOpenAIChat(chat(msg("user", textPart("hi"))), openaiClient("m"))
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	first := body.Bytes()
	for i := range first {
		first[i] = 'X'
	}
	if bytes.Equal(body.Bytes(), first) {
		t.Fatal("mutating the returned slice mutated the canonical body")
	}
}

// TestDeclines proves every declared decline class fails closed and unwraps to
// ErrUnsupported, and that BuildOpenAIChat refuses to serialize a declined input.
func TestDeclines(t *testing.T) {
	mediaPart := func(kind string) nativeprompt.Part {
		return nativeprompt.Part{Media: &nativeprompt.MediaPart{Kind: kind, URL: "https://x/y"}}
	}
	bodyOpt := func(key, feature string) ClientIntent {
		return ClientIntent{Provider: ProviderOpenAI, TargetModel: "m",
			BodyAffecting: []BodyAffectingOption{{Key: key, Feature: feature}}}
	}
	cases := []struct {
		name     string
		rendered *nativeprompt.RenderedPrompt
		client   ClientIntent
		feature  string
	}{
		{"non_openai_provider", chat(msg("user", textPart("hi"))), ClientIntent{Provider: "anthropic", TargetModel: "m"}, FeatureProvider},
		{"empty_provider", chat(msg("user", textPart("hi"))), ClientIntent{TargetModel: "m"}, FeatureProvider},
		{"absent_model", chat(msg("user", textPart("hi"))), ClientIntent{Provider: ProviderOpenAI}, FeatureModelSelection},
		{"streaming", chat(msg("user", textPart("hi"))), ClientIntent{Provider: ProviderOpenAI, TargetModel: "m", Stream: true}, FeatureStreaming},
		{"request_body_option", chat(msg("user", textPart("hi"))), bodyOpt("request_body.temperature", FeatureRequestBody), FeatureRequestBody},
		{"tools_option", chat(msg("user", textPart("hi"))), bodyOpt("tools", FeatureTools), FeatureTools},
		{"response_format_option", chat(msg("user", textPart("hi"))), bodyOpt("response_format", FeatureResponseFormat), FeatureResponseFormat},
		{"unordered_generic_option", chat(msg("user", textPart("hi"))), bodyOpt("temperature", FeatureClientOption), FeatureClientOption},
		{"empty_messages", chat(), openaiClient("m"), FeatureEmptyMessages},
		{"custom_role", chat(msg("tool", textPart("x"))), openaiClient("m"), FeatureRole},
		{"allow_duplicate_role", chat(nativeprompt.RenderedMessage{Role: "user", AllowDuplicateRole: true, Parts: []nativeprompt.Part{textPart("x")}}), openaiClient("m"), FeatureAllowDuplicateRole},
		{"message_meta", chat(nativeprompt.RenderedMessage{Role: "user", Meta: map[string]any{"cache_control": "x"}, Parts: []nativeprompt.Part{textPart("x")}}), openaiClient("m"), FeatureMessageMeta},
		{"empty_message", chat(msg("user")), openaiClient("m"), FeatureEmptyMessage},
		{"media_image", chat(msg("user", mediaPart("image"))), openaiClient("m"), FeatureMediaPart},
		{"media_audio", chat(msg("user", mediaPart("audio"))), openaiClient("m"), FeatureMediaPart},
		{"media_pdf", chat(msg("user", mediaPart("pdf"))), openaiClient("m"), FeatureMediaPart},
		{"media_video", chat(msg("user", mediaPart("video"))), openaiClient("m"), FeatureMediaPart},
		{"nil_prompt", nil, openaiClient("m"), FeatureRenderedPrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SupportsOpenAIChat(tc.rendered, tc.client)
			if err == nil {
				t.Fatalf("expected decline %q, got nil", tc.feature)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("decline does not unwrap to ErrUnsupported: %v", err)
			}
			var d *Decline
			if !errors.As(err, &d) || d.Feature != tc.feature {
				t.Fatalf("want feature %q, got %v", tc.feature, err)
			}
			if body, berr := BuildOpenAIChat(tc.rendered, tc.client); berr == nil || body != nil {
				t.Fatalf("BuildOpenAIChat must decline too, got body=%v err=%v", body, berr)
			}
		})
	}
}

// TestNormalizeDynamicClient proves the dynamic registry normalizer selects the
// client, extracts the model, ignores transport-only options, and records
// body-affecting options for the gate.
func TestNormalizeDynamicClient(t *testing.T) {
	prim := "TestClient"
	reg := &bamlutils.ClientRegistry{
		Primary: &prim,
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestClient", Provider: "openai", Options: map[string]any{
				"model": "gpt-4o-mini", "base_url": "http://x/v1", "api_key": "k",
			}},
		},
	}
	intent, err := NormalizeDynamicClient(reg, "alias-x", false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	if intent.Provider != "openai" || intent.TargetModel != "gpt-4o-mini" || intent.ModelAlias != "alias-x" {
		t.Fatalf("unexpected intent: %+v", intent)
	}
	if intent.Stream {
		t.Fatalf("non-streaming attempt must not set Stream")
	}
	if len(intent.BodyAffecting) != 0 {
		t.Fatalf("transport-only options must not be body-affecting: %v", intent.BodyAffecting)
	}
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), intent); err != nil {
		t.Fatalf("normalized transport-only client should be admitted: %v", err)
	}

	// A non-transport option (temperature) is body-affecting (FeatureClientOption).
	reg.Clients[0].Options["temperature"] = 0.7
	intent2, err := NormalizeDynamicClient(reg, "alias-x", false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	if len(intent2.BodyAffecting) != 1 || intent2.BodyAffecting[0].Key != "temperature" || intent2.BodyAffecting[0].Feature != FeatureClientOption {
		t.Fatalf("want [{temperature client_option}], got %v", intent2.BodyAffecting)
	}
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), intent2); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("body-affecting option must decline, got %v", err)
	}

	// A streaming attempt declines FeatureStreaming.
	streamIntent, err := NormalizeDynamicClient(oracleReg(), "alias-x", true)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient(stream): %v", err)
	}
	var d *Decline
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), streamIntent); !errors.As(err, &d) || d.Feature != FeatureStreaming {
		t.Fatalf("streaming attempt must decline FeatureStreaming, got %v", err)
	}

	// tools / response_format options map to their concrete features.
	for _, tc := range []struct{ key, feature string }{
		{"tools", FeatureTools},
		{"response_format", FeatureResponseFormat},
	} {
		r := oracleReg()
		r.Clients[0].Options[tc.key] = "x"
		in, err := NormalizeDynamicClient(r, "a", false)
		if err != nil {
			t.Fatalf("NormalizeDynamicClient(%s): %v", tc.key, err)
		}
		if len(in.BodyAffecting) != 1 || in.BodyAffecting[0].Feature != tc.feature {
			t.Fatalf("option %q: want feature %q, got %v", tc.key, tc.feature, in.BodyAffecting)
		}
	}
}

// oracleReg builds a minimal single-client openai registry for normalizer tests.
func oracleReg() *bamlutils.ClientRegistry {
	prim := "TestClient"
	return &bamlutils.ClientRegistry{
		Primary: &prim,
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestClient", Provider: "openai", Options: map[string]any{"model": "gpt-4o-mini"}},
		},
	}
}

// TestNormalizeDynamicClientSelectionDeclines proves ambiguous/absent selection
// declines.
func TestNormalizeDynamicClientSelectionDeclines(t *testing.T) {
	if _, err := NormalizeDynamicClient(nil, "a", false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("nil registry must decline, got %v", err)
	}
	two := &bamlutils.ClientRegistry{Clients: []*bamlutils.ClientProperty{
		{Name: "A", Provider: "openai", Options: map[string]any{"model": "m"}},
		{Name: "B", Provider: "openai", Options: map[string]any{"model": "m"}},
	}}
	if _, err := NormalizeDynamicClient(two, "a", false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("multiple clients without primary must decline, got %v", err)
	}
}

// TestNormalizeStaticClient proves the static descriptor normalizer admits a
// literal model, declines env/dynamic models, and records request_body presence.
func TestNormalizeStaticClient(t *testing.T) {
	lit := promptdescriptor.ClientConfig{
		Present:  true,
		Name:     "StaticOracleClient",
		Provider: "openai",
		Model:    promptdescriptor.ClientModel{Value: "fake-static-oracle-model", Provenance: promptdescriptor.ModelProvenanceLiteral},
		TransportOptions: []promptdescriptor.ClientOption{
			{Key: "api_key", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: "k"}},
			{Key: "base_url", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: "u"}},
		},
	}
	intent, err := NormalizeStaticClient(lit, "alias", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	if intent.TargetModel != "fake-static-oracle-model" || len(intent.BodyAffecting) != 0 {
		t.Fatalf("unexpected intent: %+v", intent)
	}

	// env-derived and dynamic (ident) models both decline FeatureModelSelection.
	for _, prov := range []promptdescriptor.ModelProvenance{
		promptdescriptor.ModelProvenanceEnv,
		promptdescriptor.ModelProvenanceDynamic,
		promptdescriptor.ModelProvenanceAbsent,
	} {
		cfg := lit
		cfg.Model = promptdescriptor.ClientModel{Provenance: prov}
		var d *Decline
		if _, err := NormalizeStaticClient(cfg, "a", false); !errors.As(err, &d) || d.Feature != FeatureModelSelection {
			t.Errorf("provenance %q must decline FeatureModelSelection, got %v", prov, err)
		}
	}

	// A streaming attempt declines FeatureStreaming even for a literal model.
	streamIntent, err := NormalizeStaticClient(lit, "a", true)
	if err != nil {
		t.Fatalf("NormalizeStaticClient(stream): %v", err)
	}
	var sd *Decline
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), streamIntent); !errors.As(err, &sd) || sd.Feature != FeatureStreaming {
		t.Fatalf("streaming attempt must decline FeatureStreaming, got %v", err)
	}

	// a request_body entry is body-affecting (FeatureRequestBody).
	rbCfg := lit
	rbCfg.RequestBody = []promptdescriptor.RequestBodyEntry{{Key: "temperature", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionNumber, Number: "0.5"}}}
	intent3, err := NormalizeStaticClient(rbCfg, "a", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	if len(intent3.BodyAffecting) != 1 || intent3.BodyAffecting[0].Key != "request_body.temperature" || intent3.BodyAffecting[0].Feature != FeatureRequestBody {
		t.Fatalf("want [{request_body.temperature request_body}], got %v", intent3.BodyAffecting)
	}
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), intent3); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("request_body must decline, got %v", err)
	}

	// a top-level tools option keeps FeatureTools.
	toolsCfg := lit
	toolsCfg.BodyAffectingOptions = []promptdescriptor.ClientOption{{Key: "tools", Value: promptdescriptor.OptionValue{Kind: promptdescriptor.OptionIdent, String: "x"}}}
	intent4, err := NormalizeStaticClient(toolsCfg, "a", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	if len(intent4.BodyAffecting) != 1 || intent4.BodyAffecting[0].Feature != FeatureTools {
		t.Fatalf("want tools feature, got %v", intent4.BodyAffecting)
	}

	// An explicit empty `request_body {}` (RequestBodyPresent, zero entries) is
	// NOT equivalent to an absent one: BAML emits "request_body":{}, so it declines
	// FeatureRequestBody under the bare "request_body" key. The absent case (lit,
	// RequestBodyPresent=false) is admitted.
	emptyRB := lit
	emptyRB.RequestBodyPresent = true // RequestBody stays nil (empty block)
	intent5, err := NormalizeStaticClient(emptyRB, "a", false)
	if err != nil {
		t.Fatalf("NormalizeStaticClient: %v", err)
	}
	if len(intent5.BodyAffecting) != 1 || intent5.BodyAffecting[0].Key != "request_body" || intent5.BodyAffecting[0].Feature != FeatureRequestBody {
		t.Fatalf("want [{request_body request_body}] for explicit empty block, got %v", intent5.BodyAffecting)
	}
	if err := SupportsOpenAIChat(chat(msg("user", textPart("hi"))), intent5); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("explicit empty request_body must decline, got %v", err)
	}
	// Sanity: the absent case (lit) is admitted (no request_body key emitted).
	if len(intent.BodyAffecting) != 0 {
		t.Fatalf("absent request_body must be admitted, got body-affecting %v", intent.BodyAffecting)
	}
}
