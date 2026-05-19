package bamlutils

import (
	"bytes"
	"testing"

	"github.com/goccy/go-json"
)

// TestDynamicInput_ToWorkerInput_CacheControlBridge pins the bridge from
// the public CacheControl{Type: ...} shape to the generated dynamic BAML
// input's `cache_control.cache_type` shape. The previous wire reused the
// public MessageMetadata struct directly, so the generated input dropped
// the `type` value and forwarded `cache_control: {}` to Anthropic
// once `allowed_role_metadata: ["cache_control"]` enabled the field
// (issue #304).
//
// Asserts:
//   - the tagged message's metadata contains `cache_control.cache_type:"ephemeral"`,
//   - no message carries the public `cache_control.type` JSON key,
//   - no message carries an empty `cache_control:{}` object,
//   - untagged messages emit no `metadata` key at all.
func TestDynamicInput_ToWorkerInput_CacheControlBridge(t *testing.T) {
	t.Parallel()

	systemText := "you are a helpful assistant"
	userText := "tell me a joke"
	assistantText := "sure: why did the chicken cross the road?"
	followupText := "i don't know, why?"

	primary := "TestClient"
	provider := "anthropic"

	input := &DynamicInput{
		Messages: []DynamicMessage{
			{Role: "system", TextContent: &systemText},
			{
				Role: "user",
				PartsContent: []DynamicContentPart{
					{Type: "text", Text: &userText},
				},
				Metadata: &MessageMetadata{
					CacheControl: &CacheControl{Type: "ephemeral"},
				},
			},
			{Role: "assistant", TextContent: &assistantText},
			{Role: "user", TextContent: &followupText},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{
					Name:     primary,
					Provider: provider,
					Options: map[string]any{
						"model":                "test-model",
						"api_key":              "test-key",
						"allowed_role_metadata": []any{"cache_control"},
					},
				},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: map[string]*DynamicProperty{
				"answer": {Type: "string"},
			},
		},
	}

	if err := input.Validate(); err != nil {
		t.Fatalf("input.Validate: %v", err)
	}
	data, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	// The public `type` JSON key must never leak into the BAML-facing
	// payload; the generated input names the field `cache_type`.
	if bytes.Contains(data, []byte(`"cache_control":{"type":`)) {
		t.Errorf("worker payload still carries public cache_control.type shape:\n%s", data)
	}
	// An empty cache_control object is the exact bug shape from #304;
	// the bridge must never emit it.
	if bytes.Contains(data, []byte(`"cache_control":{}`)) {
		t.Errorf("worker payload carries empty cache_control object:\n%s", data)
	}
	// Spot the empty cache_type case too — the generated decoder treats
	// missing and empty alike, and both are invalid.
	if bytes.Contains(data, []byte(`"cache_type":""`)) {
		t.Errorf("worker payload carries empty cache_type string:\n%s", data)
	}

	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	if len(decoded.Messages) != 4 {
		t.Fatalf("expected 4 messages in worker payload, got %d\n%s", len(decoded.Messages), data)
	}

	// Index 0: system text — no metadata.
	if md, ok := decoded.Messages[0]["metadata"]; ok && md != nil {
		t.Errorf("system message must not carry metadata; got %v", md)
	}

	// Index 1: user parts with cache_control.
	msg := decoded.Messages[1]
	mdRaw, ok := msg["metadata"]
	if !ok {
		t.Fatalf("tagged user message missing metadata key: %v", msg)
	}
	md, ok := mdRaw.(map[string]any)
	if !ok {
		t.Fatalf("tagged metadata is not an object: %T %v", mdRaw, mdRaw)
	}
	ccRaw, ok := md["cache_control"]
	if !ok {
		t.Fatalf("tagged metadata missing cache_control: %v", md)
	}
	cc, ok := ccRaw.(map[string]any)
	if !ok {
		t.Fatalf("cache_control is not an object: %T %v", ccRaw, ccRaw)
	}
	if got, want := cc["cache_type"], "ephemeral"; got != want {
		t.Errorf("cache_type: got %v, want %q", got, want)
	}
	if _, present := cc["type"]; present {
		t.Errorf("cache_control still carries public `type` key: %v", cc)
	}

	// Index 2: assistant text — no metadata.
	if md, ok := decoded.Messages[2]["metadata"]; ok && md != nil {
		t.Errorf("assistant message must not carry metadata; got %v", md)
	}
	// Index 3: second user text — no metadata.
	if md, ok := decoded.Messages[3]["metadata"]; ok && md != nil {
		t.Errorf("follow-up user message must not carry metadata; got %v", md)
	}
}

// TestDynamicInput_ToWorkerInput_NoCacheControl_NoMetadata pins the
// omitempty contract: when no message carries metadata, the worker
// payload must not contain any `metadata` key at all.
func TestDynamicInput_ToWorkerInput_NoCacheControl_NoMetadata(t *testing.T) {
	t.Parallel()

	prompt := "hello"
	primary := "TestClient"
	provider := "anthropic"
	input := &DynamicInput{
		Messages: []DynamicMessage{
			{Role: "user", TextContent: &prompt},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{Name: primary, Provider: provider, Options: map[string]any{"model": "test-model", "api_key": "test-key"}},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: map[string]*DynamicProperty{"answer": {Type: "string"}},
		},
	}

	data, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}
	if bytes.Contains(data, []byte(`"metadata"`)) {
		t.Errorf("worker payload should omit metadata when no message carries it; got:\n%s", data)
	}
}

// TestDynamicMessage_PublicJSON_StillUsesType locks the public API in.
// Callers of the dynamic endpoint pass `cache_control.type`; the bridge
// rewrites it internally. A regression that flipped the public shape
// would break every existing caller, so the public marshalled form is
// asserted here alongside the internal rewrite test above.
func TestDynamicMessage_PublicJSON_StillUsesType(t *testing.T) {
	t.Parallel()
	body := []byte(`{"role":"user","content":"hi","metadata":{"cache_control":{"type":"ephemeral"}}}`)
	var msg DynamicMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal public DynamicMessage: %v", err)
	}
	if msg.Metadata == nil || msg.Metadata.CacheControl == nil {
		t.Fatalf("metadata.CacheControl not populated from public JSON: %+v", msg)
	}
	if msg.Metadata.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type: got %q, want %q", msg.Metadata.CacheControl.Type, "ephemeral")
	}
	out, err := json.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Errorf("public DynamicMessage marshal lost the type key; got:\n%s", out)
	}
}
