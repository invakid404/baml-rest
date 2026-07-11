//go:build integration

package nativebody

// Dynamic differential raw-body oracle for the native canonical OpenAI
// chat-completions body builder (de-BAML Phase 4a). It drives BAML's real
// v0.223 runtime through the in-process dynclient with de-BAML DISABLED, so BAML
// itself renders the prompt AND builds the provider request body; a
// short-circuiting net/http RoundTripper captures that body with no network and
// no send. For every admitted fixture it renders the SAME data through
// nativeprompt.Render, normalizes the selected client, builds the native
// canonical body, and asserts EXACT byte equality against BAML's captured body.
//
// Raw bytes are primary: the comparison is exact byte equality; neither side is
// normalized before comparison, because key order, content-array layout,
// escaping, and the flattened request_body order are exactly what must be
// proven. A parsed-JSON diff is reported only to explain a failure.
//
// Decline fixtures (media, role metadata) assert the native support gate FAILS
// CLOSED before serialization; mutation controls prove a single-byte divergence
// is caught.
//
// Run:
//
//	CGO_ENABLED=1 go test -tags integration ./internal/nativebody
//
// Requires CGO + the cached BAML v0.223 CFFI library, exactly as the
// internal/nativeprompt dynamic oracle.

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// captureRoundTripper records the last outbound request body and returns a
// canned OpenAI chat-completion response without touching the network. Mirrors
// the internal/nativeprompt oracle's helper (kept local — test helpers are not
// importable across packages).
type captureRoundTripper struct {
	mu   sync.Mutex
	last []byte
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	c.mu.Lock()
	c.last = body
	c.mu.Unlock()

	const canned = `{"id":"chatcmpl-native-body-oracle","object":"chat.completion","created":0,` +
		`"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(canned)),
		Request:    req,
	}, nil
}

func (c *captureRoundTripper) lastBody() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *captureRoundTripper) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = nil
}

// oracleModel / oracleAlias are the fixed dynamic-oracle target model and the
// SEPARATE nanollm alias. The alias deliberately differs from the target so the
// model-separation assertion is meaningful: the alias must never appear in the
// JSON body.
const (
	oracleModel = "gpt-4o-mini"
	oracleAlias = "nanollm://openai/gpt-4o-mini"
)

func strptr(s string) *string { return &s }

func simpleSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
		),
	}
}

func richSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("answer", &bamlutils.DynamicProperty{Type: "string"}),
			bamlutils.OrderedKV("count", &bamlutils.DynamicProperty{Type: "int"}),
		),
	}
}

// bodyCase is one differential fixture. admit==true cases are compared
// byte-for-byte; admit==false cases assert the native gate declines before
// serialization with the concrete named declineFeature.
type bodyCase struct {
	name           string
	messages       []nativeprompt.Message
	schema         *bamlutils.DynamicOutputSchema
	clientOptions  map[string]any
	admit          bool
	declineFeature string
}

func bodyCases() []bodyCase {
	return []bodyCase{
		{
			name:     "single_user_message",
			messages: []nativeprompt.Message{{Role: "user", Content: strptr("What is 2+2?")}},
			schema:   simpleSchema(),
			admit:    true,
		},
		{
			name: "system_user_ordered",
			messages: []nativeprompt.Message{
				{Role: "system", Content: strptr("You are a helpful assistant.")},
				{Role: "user", Content: strptr("What is 2+2?")},
			},
			schema: simpleSchema(),
			admit:  true,
		},
		{
			name: "system_user_assistant_ordered",
			messages: []nativeprompt.Message{
				{Role: "system", Content: strptr("You are concise.")},
				{Role: "user", Content: strptr("Tell me about weather.")},
				{Role: "assistant", Content: strptr("Here are 3 facts.")},
			},
			schema: simpleSchema(),
			admit:  true,
		},
		{
			name: "output_format_rendered_into_text",
			messages: []nativeprompt.Message{
				{Role: "system", Parts: []nativeprompt.ContentPart{{OutputFormat: true}}},
				{Role: "user", Content: strptr("Produce the structured output.")},
			},
			schema: richSchema(),
			admit:  true,
		},
		{
			name: "special_characters_escaping",
			messages: []nativeprompt.Message{
				{Role: "user", Content: strptr("html <b>&</b>, quote \"q\", slash a/b, unicode café ☕")},
			},
			schema: simpleSchema(),
			admit:  true,
		},
		// Declines (fail-closed, named). Prompt-side: role metadata + media. Client-
		// side: body-affecting options that BAML flattens into the JSON body but 4a
		// cannot reproduce (each mapped to its concrete decline feature).
		{
			// cache_control on the message → native Render carries Meta → the
			// prompt-side gate declines FeatureMessageMeta. (No allowed_role_metadata
			// client option here so the message-meta gate — not the client-option gate
			// — is the one under test; this is a decline case, so no byte comparison.)
			name: "role_metadata_cache_control_declines",
			messages: []nativeprompt.Message{
				{Role: "user", Parts: []nativeprompt.ContentPart{{Text: strptr("Say ok.")}},
					Metadata: &nativeprompt.Metadata{CacheControl: &nativeprompt.CacheControl{Type: "ephemeral"}}},
			},
			schema:         simpleSchema(),
			admit:          false,
			declineFeature: FeatureMessageMeta,
		},
		{
			name: "image_media_declines",
			messages: []nativeprompt.Message{
				{Role: "user", Parts: []nativeprompt.ContentPart{
					{Text: strptr("Describe this image:")},
					{Image: &nativeprompt.Media{Kind: nativeprompt.MediaImage, URL: "https://example.com/cat.png"}},
				}},
			},
			schema:         simpleSchema(),
			admit:          false,
			declineFeature: FeatureMediaPart,
		},
		{
			name:           "tools_option_declines",
			messages:       []nativeprompt.Message{{Role: "user", Content: strptr("hi")}},
			schema:         simpleSchema(),
			clientOptions:  map[string]any{"tools": []any{map[string]any{"type": "function"}}},
			admit:          false,
			declineFeature: FeatureTools,
		},
		{
			name:           "response_format_option_declines",
			messages:       []nativeprompt.Message{{Role: "user", Content: strptr("hi")}},
			schema:         simpleSchema(),
			clientOptions:  map[string]any{"response_format": map[string]any{"type": "json_object"}},
			admit:          false,
			declineFeature: FeatureResponseFormat,
		},
		{
			name:           "unordered_generic_option_declines",
			messages:       []nativeprompt.Message{{Role: "user", Content: strptr("hi")}},
			schema:         simpleSchema(),
			clientOptions:  map[string]any{"temperature": 0.7},
			admit:          false,
			declineFeature: FeatureClientOption,
		},
		{
			// An EXPLICIT empty request_body — NOT equivalent to absent: BAML emits
			// "request_body":{} (proven by TestNativeBodyDynamicOracleEmptyRequestBody),
			// which native does not reproduce, so it declines FeatureRequestBody.
			name:           "explicit_empty_request_body_declines",
			messages:       []nativeprompt.Message{{Role: "user", Content: strptr("hi")}},
			schema:         simpleSchema(),
			clientOptions:  map[string]any{"request_body": map[string]any{}},
			admit:          false,
			declineFeature: FeatureRequestBody,
		},
	}
}

// newOracleClient builds a dynclient with de-BAML disabled so BAML renders the
// prompt and builds the request body itself, captured by the transport.
func newOracleClient(t *testing.T, capture *captureRoundTripper) *dynclient.Client {
	t.Helper()
	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(&http.Client{Transport: capture}),
		dynclient.WithDeBAML(false),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}
	return client
}

// oracleRegistry builds a client_registry pointing at a fake OpenAI base URL
// (intercepted by the capture transport), with the fixed target model plus any
// extra options.
func oracleRegistry(extra map[string]any) *dynclient.ClientRegistry {
	opts := map[string]any{
		"model":    oracleModel,
		"base_url": "http://native-body-oracle.invalid/v1",
		"api_key":  "test-key",
	}
	for k, v := range extra {
		opts[k] = v
	}
	return &dynclient.ClientRegistry{
		Primary: strptr("TestClient"),
		Clients: []*dynclient.ClientProperty{
			{Name: "TestClient", Provider: "openai", Options: opts},
		},
	}
}

// captureBAMLBody drives BAML to build (and "send", i.e. capture) the provider
// request body for one fixture, returning the exact captured bytes.
func captureBAMLBody(t *testing.T, client *dynclient.Client, capture *captureRoundTripper, tc bodyCase) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	capture.reset()
	req := dynclient.Request{
		Messages:       toDynMessages(tc.messages),
		ClientRegistry: oracleRegistry(tc.clientOptions),
		OutputSchema:   tc.schema,
	}
	if _, err := client.DynamicCall(ctx, req); err != nil {
		t.Logf("DynamicCall returned (response parse may fail on canned body; request still captured): %v", err)
	}
	body := capture.lastBody()
	if len(body) == 0 {
		t.Fatalf("no provider request captured — BAML render/build failed before send")
	}
	return body
}

// TestNativeBodyDynamicOracleParity is the core proof: for every admitted
// fixture the native canonical body equals BAML's captured body byte-for-byte.
func TestNativeBodyDynamicOracleParity(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	for _, tc := range bodyCases() {
		if !tc.admit {
			continue
		}
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bamlBody := captureBAMLBody(t, client, capture, tc)

			rendered, err := nativeprompt.Render(tc.messages, tc.schema)
			if err != nil {
				t.Fatalf("nativeprompt.Render: %v", err)
			}
			intent, err := NormalizeDynamicClient(oracleRegistry(tc.clientOptions), oracleAlias, false)
			if err != nil {
				t.Fatalf("NormalizeDynamicClient: %v", err)
			}
			native, err := BuildOpenAIChat(rendered, intent)
			if err != nil {
				t.Fatalf("BuildOpenAIChat declined an admitted fixture: %v", err)
			}

			if !bytesEqual(native.Bytes(), bamlBody) {
				t.Errorf("native body diverged from BAML\n--- native ---\n%s\n--- BAML ---\n%s\n%s",
					native.Bytes(), bamlBody, jsonDiff(native.Bytes(), bamlBody))
			}

			// Model separation: JSON model is the target, never the alias.
			if native.TargetModel() != oracleModel {
				t.Errorf("target model = %q, want %q", native.TargetModel(), oracleModel)
			}
			if strings.Contains(string(native.Bytes()), oracleAlias) {
				t.Errorf("nanollm alias leaked into JSON body: %s", native.Bytes())
			}
		})
	}
}

// TestNativeBodyDynamicOracleDeclines proves the unsupported fixtures fail closed
// before serialization with the concrete named decline feature (not a generic
// one), and BuildOpenAIChat returns a wrapped ErrUnsupported.
func TestNativeBodyDynamicOracleDeclines(t *testing.T) {
	for _, tc := range bodyCases() {
		if tc.admit {
			continue
		}
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := nativeprompt.Render(tc.messages, tc.schema)
			if err != nil {
				t.Fatalf("nativeprompt.Render: %v", err)
			}
			intent, err := NormalizeDynamicClient(oracleRegistry(tc.clientOptions), oracleAlias, false)
			if err == nil {
				_, err = BuildOpenAIChat(rendered, intent)
			}
			if err == nil {
				t.Fatalf("expected decline %q, got a serialized body", tc.declineFeature)
			}
			var d *Decline
			if !errors.As(err, &d) || d.Feature != tc.declineFeature {
				t.Fatalf("want concrete decline feature %q, got %v", tc.declineFeature, err)
			}
		})
	}
}

// TestNativeBodyDynamicOracleStreamControl is the streaming decline CONTROL: it
// captures BAML's actual streaming request body (which appends
// "stream":true,"stream_options":{...}) and proves the native builder DECLINES a
// streaming attempt (FeatureStreaming) — the stream request never reaches
// serialization, so a 4b caller cannot mistake the non-streaming body for it.
func TestNativeBodyDynamicOracleStreamControl(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	tc := bodyCases()[0] // single_user_message
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	capture.reset()
	stream, err := client.DynamicStream(ctx, dynclient.Request{
		Messages:       toDynMessages(tc.messages),
		ClientRegistry: oracleRegistry(tc.clientOptions),
		OutputSchema:   tc.schema,
	})
	// Fail-closed on BOTH halves of the contract: a DynamicStream error (the
	// stream request could not be opened/built) OR a nil stream is a control
	// failure, not a skip. DynamicStream opens the stream and builds+issues the
	// request; the canned non-SSE response only fails LATER, during draining, so a
	// healthy run returns a nil error and a non-nil stream here.
	if err != nil {
		t.Fatalf("DynamicStream errored — stream request construction failed: %v", err)
	}
	if stream == nil {
		t.Fatalf("DynamicStream returned a nil stream with no error")
	}
	// Drain/close so the request is issued and captured; the canned non-SSE
	// response makes decoding fail during draining, but the request body is
	// captured first.
	for {
		ev, nerr := stream.Next()
		if ev == nil || nerr != nil {
			break
		}
	}
	_ = stream.Close()
	// FAIL (not log-and-pass) if BAML built no request: a harness regression that
	// prevents stream request construction must break this control.
	bamlStream := capture.lastBody()
	if len(bamlStream) == 0 {
		t.Fatalf("no BAML stream request captured — stream request construction failed before send")
	}
	// The v0.223 stream body carries BOTH stream:true AND the stream_options
	// object; a regression that drops either must fail here.
	if !strings.Contains(string(bamlStream), `"stream":true`) {
		t.Errorf("BAML stream body missing \"stream\":true: %s", bamlStream)
	}
	if !strings.Contains(string(bamlStream), `"stream_options":{"include_usage":true}`) {
		t.Errorf("BAML stream body missing \"stream_options\":{\"include_usage\":true}: %s", bamlStream)
	}

	// Native must DECLINE the streaming attempt with FeatureStreaming.
	rendered, err := nativeprompt.Render(tc.messages, tc.schema)
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	intent, err := NormalizeDynamicClient(oracleRegistry(tc.clientOptions), oracleAlias, true /* stream */)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	body, err := BuildOpenAIChat(rendered, intent)
	if body != nil || err == nil {
		t.Fatalf("streaming attempt must not serialize; got body=%v err=%v", body, err)
	}
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureStreaming {
		t.Fatalf("streaming attempt must decline FeatureStreaming, got %v", err)
	}
}

// TestNativeBodyDynamicOracleEmptyRequestBody is the empty-vs-absent boundary
// CONTROL: it captures BAML's body for a client whose request_body is an EXPLICIT
// empty object and proves BAML EMITS "request_body":{} (i.e. an explicit empty
// request_body is NOT equivalent to an absent one, whose body is just
// model+messages). Because native does not reproduce that key, it must DECLINE
// (FeatureRequestBody) — never serialize a model+messages body that silently
// drops BAML's request_body object.
func TestNativeBodyDynamicOracleEmptyRequestBody(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	msgs := []nativeprompt.Message{{Role: "user", Content: strptr("hi")}}
	emptyRBOpts := map[string]any{"request_body": map[string]any{}}

	// BAML leg: capture the body for an explicit empty request_body.
	tc := bodyCase{messages: msgs, schema: simpleSchema(), clientOptions: emptyRBOpts}
	bamlBody := captureBAMLBody(t, client, capture, tc)
	if !strings.Contains(string(bamlBody), `"request_body":{}`) {
		t.Fatalf("expected BAML to emit \"request_body\":{} for an explicit empty request_body, got %s", bamlBody)
	}

	// Absent request_body (baseline) is just model+messages — proving the two
	// inputs diverge, so native cannot treat empty as absent.
	baseline := captureBAMLBody(t, client, capture, bodyCase{messages: msgs, schema: simpleSchema()})
	if strings.Contains(string(baseline), "request_body") {
		t.Fatalf("baseline (absent request_body) must NOT contain request_body, got %s", baseline)
	}

	// Native leg: the explicit empty request_body must DECLINE, not serialize.
	rendered, err := nativeprompt.Render(msgs, simpleSchema())
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	intent, err := NormalizeDynamicClient(oracleRegistry(emptyRBOpts), oracleAlias, false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	body, err := BuildOpenAIChat(rendered, intent)
	if body != nil || err == nil {
		t.Fatalf("explicit empty request_body must not serialize; got body=%v err=%v", body, err)
	}
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureRequestBody {
		t.Fatalf("explicit empty request_body must decline FeatureRequestBody, got %v", err)
	}
}

// TestNativeBodyDynamicOracleInvalidUTF8 is the BAML-anchored control for the
// UTF-8 safety boundary: BAML REJECTS invalid UTF-8 at its protobuf/CFFI boundary
// ("string field contains invalid UTF-8") and builds NO request, so the raw
// writer must never emit those bytes. It proves (a) BAML captures nothing for an
// invalid-UTF-8 message, and (b) native declines FeatureInvalidUTF8 before
// serialization.
func TestNativeBodyDynamicOracleInvalidUTF8(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	bad := "hi \xff\xfe there" // invalid UTF-8 bytes
	msgs := []nativeprompt.Message{{Role: "user", Content: &bad}}

	// BAML leg: rejects invalid UTF-8, so no request body is captured.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	capture.reset()
	if _, err := client.DynamicCall(ctx, dynclient.Request{
		Messages:       toDynMessages(msgs),
		ClientRegistry: oracleRegistry(nil),
		OutputSchema:   simpleSchema(),
	}); err == nil {
		t.Fatalf("expected BAML to reject invalid UTF-8, got nil error")
	}
	if body := capture.lastBody(); len(body) != 0 {
		t.Fatalf("BAML must build NO request for invalid UTF-8, captured %d bytes: %q", len(body), body)
	}

	// Native leg: Render succeeds but BuildOpenAIChat must DECLINE (never emit the
	// invalid bytes BAML rejected).
	rendered, err := nativeprompt.Render(msgs, simpleSchema())
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	intent, err := NormalizeDynamicClient(oracleRegistry(nil), oracleAlias, false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	body, err := BuildOpenAIChat(rendered, intent)
	if body != nil || err == nil {
		t.Fatalf("native must decline invalid UTF-8, got body=%v err=%v", body, err)
	}
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureInvalidUTF8 {
		t.Fatalf("native must decline FeatureInvalidUTF8, got %v", err)
	}
}

// TestNativeBodyDynamicOracleMutation is the negative control: a single-byte
// mutation of the native body must break byte equality with BAML's body, proving
// the comparison is sensitive.
func TestNativeBodyDynamicOracleMutation(t *testing.T) {
	capture := &captureRoundTripper{}
	client := newOracleClient(t, capture)

	tc := bodyCases()[1] // system_user_ordered
	bamlBody := captureBAMLBody(t, client, capture, tc)

	rendered, err := nativeprompt.Render(tc.messages, tc.schema)
	if err != nil {
		t.Fatalf("nativeprompt.Render: %v", err)
	}
	intent, err := NormalizeDynamicClient(oracleRegistry(tc.clientOptions), oracleAlias, false)
	if err != nil {
		t.Fatalf("NormalizeDynamicClient: %v", err)
	}
	native, err := BuildOpenAIChat(rendered, intent)
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	if !bytesEqual(native.Bytes(), bamlBody) {
		t.Fatalf("precondition: native must equal BAML before mutation")
	}
	mutated := native.Bytes()
	mutated[len(mutated)/2] ^= 0x20
	if bytesEqual(mutated, bamlBody) {
		t.Fatal("a one-byte mutation was NOT detected — the oracle is not byte-sensitive")
	}
}

// toDynMessages converts native corpus messages into dynclient request messages
// so the same fixture drives both legs.
func toDynMessages(msgs []nativeprompt.Message) []dynclient.Message {
	out := make([]dynclient.Message, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		dm := dynclient.Message{Role: m.Role}
		if m.Metadata != nil && m.Metadata.CacheControl != nil {
			dm.Metadata = &dynclient.MessageMetadata{
				CacheControl: &dynclient.CacheControl{Type: m.Metadata.CacheControl.Type},
			}
		}
		if len(m.Parts) > 0 {
			for j := range m.Parts {
				dm.PartsContent = append(dm.PartsContent, toDynPart(&m.Parts[j]))
			}
		} else if m.Content != nil {
			dm.TextContent = m.Content
		}
		out = append(out, dm)
	}
	return out
}

func toDynPart(p *nativeprompt.ContentPart) dynclient.ContentPart {
	switch {
	case p.Text != nil:
		return dynclient.ContentPart{Type: "text", Text: p.Text}
	case p.Image != nil:
		return dynclient.ContentPart{Type: "image", Image: toMediaInput(p.Image)}
	case p.OutputFormat:
		return dynclient.ContentPart{Type: "output_format"}
	}
	return dynclient.ContentPart{}
}

func toMediaInput(m *nativeprompt.Media) *bamlutils.MediaInput {
	mi := &bamlutils.MediaInput{}
	if m.URL != "" {
		mi.URL = strptr(m.URL)
	}
	if m.Base64 != "" {
		mi.Base64 = strptr(m.Base64)
	}
	if m.Mime != "" {
		mi.MediaType = strptr(m.Mime)
	}
	return mi
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// jsonDiff renders both bodies as indented JSON for a diagnostic-only view on
// failure. It is NEVER used for the comparison itself.
func jsonDiff(native, baml []byte) string {
	var nb, bb any
	_ = stdjson.Unmarshal(native, &nb)
	_ = stdjson.Unmarshal(baml, &bb)
	np, _ := stdjson.MarshalIndent(nb, "", "  ")
	bp, _ := stdjson.MarshalIndent(bb, "", "  ")
	return "--- native (parsed) ---\n" + string(np) + "\n--- BAML (parsed) ---\n" + string(bp)
}
