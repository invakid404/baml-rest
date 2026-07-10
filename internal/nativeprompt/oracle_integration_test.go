//go:build integration

package nativeprompt

// Build-only differential oracle harness for the native dynamic prompt
// renderer. It drives BAML's real v0.223 runtime through the in-process
// dynclient to render the Baml_Rest_Dynamic template, captures the provider
// request BAML builds via a short-circuiting net/http RoundTripper (no network,
// no provider send, canned response), extracts BAML's RenderedPrompt from the
// captured OpenAI body, and compares it byte-for-byte against the native
// renderer's RenderedPrompt across the seeded corpus.
//
// de-BAML is disabled on the client so BAML renders everything itself
// (including ctx.output_format), making this a pure BAML oracle: a pass proves
// both the template mechanics AND (re-confirms) output_format parity together.
//
// Run: go test -tags integration ./internal/nativeprompt/
// Requires: CGO + the cached BAML v0.223 CFFI library (auto-located under the
// user BAML cache dir; downloaded on first use).

import (
	"context"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/dynclient"
)

// captureRoundTripper records the last outbound request body and returns a
// canned OpenAI chat-completion response without touching the network.
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

	const canned = `{"id":"chatcmpl-native-oracle","object":"chat.completion","created":0,` +
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

// reset clears the captured body. Called at the start of each sub-test so that
// a sub-test whose BAML render fails before send yields a nil body (caught by
// the empty-body guard) instead of surfacing the PREVIOUS sub-test's body,
// which would mask a parity regression as a false pass.
func (c *captureRoundTripper) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = nil
}

// TestCaptureRoundTripperReset pins Fix #2 in isolation: reset() clears the
// captured body so a subsequent pre-send failure cannot surface stale data.
func TestCaptureRoundTripperReset(t *testing.T) {
	c := &captureRoundTripper{}
	c.last = []byte(`{"messages":[]}`)
	if len(c.lastBody()) == 0 {
		t.Fatal("precondition: body should be non-empty")
	}
	c.reset()
	if len(c.lastBody()) != 0 {
		t.Fatalf("reset() did not clear captured body: %q", c.lastBody())
	}
}

// TestExtractOracleRenderedFailsClosed pins the CodeRabbit fix: the oracle
// extractor must FAIL CLOSED on any malformed / unexpected OpenAI-body shape
// (missing/invalid role, missing content, non-object block, text block without
// a string text field, unsupported block type, image_url without a usable url)
// rather than normalizing or discarding it — otherwise provider-shape drift
// could produce a misleading parity PASS. Valid bodies must still extract.
func TestExtractOracleRenderedFailsClosed(t *testing.T) {
	valid := `{"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}` +
		`]}`
	if _, err := extractOracleRendered([]byte(valid)); err != nil {
		t.Fatalf("valid body should extract, got: %v", err)
	}

	bad := map[string]string{
		"missing_role":       `{"messages":[{"content":"hi"}]}`,
		"empty_role":         `{"messages":[{"role":"","content":"hi"}]}`,
		"non_string_role":    `{"messages":[{"role":123,"content":"hi"}]}`,
		"missing_content":    `{"messages":[{"role":"user"}]}`,
		"null_content":       `{"messages":[{"role":"user","content":null}]}`,
		"non_object_block":   `{"messages":[{"role":"user","content":["oops"]}]}`,
		"text_missing_field": `{"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"text_non_string":    `{"messages":[{"role":"user","content":[{"type":"text","text":5}]}]}`,
		"unsupported_block":  `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
		"image_url_missing":  `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		"image_url_no_url":   `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`,
		"image_url_empty":    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := extractOracleRendered([]byte(body)); err == nil {
				t.Fatalf("extractor must fail closed on %s, got nil error", name)
			}
		})
	}
}

func TestNativeDynamicPromptOracleParity(t *testing.T) {
	capture := &captureRoundTripper{}
	client, err := dynclient.New(
		dynclient.WithClientMode(llmhttp.ClientModeNetHTTP),
		dynclient.WithNetHTTPClient(&http.Client{Transport: capture}),
		// Pure BAML oracle: BAML renders ctx.output_format itself.
		dynclient.WithDeBAML(false),
	)
	if err != nil {
		t.Fatalf("dynclient.New: %v", err)
	}

	for _, tc := range corpusCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// Clear any prior sub-test's captured body so a pre-send render
			// failure here surfaces as an empty body, not stale data.
			capture.reset()

			req := dynclient.Request{
				Messages:       toDynMessages(tc.messages),
				ClientRegistry: oracleRegistry(tc.clientOptions),
				OutputSchema:   tc.schema,
			}

			// The request is built and "sent" (captured) before the response is
			// parsed, so it is captured even if response parsing against the
			// schema fails on the canned body.
			if _, err := client.DynamicCall(ctx, req); err != nil {
				t.Logf("DynamicCall returned (response parsing may fail; request still captured): %v", err)
			}

			body := capture.lastBody()
			if len(body) == 0 {
				t.Fatalf("no provider request captured — BAML render/build failed before send")
			}

			oracle, err := extractOracleRendered(body)
			if err != nil {
				t.Fatalf("extractOracleRendered: %v\ncaptured body: %s", err, string(body))
			}

			native, err := Render(tc.messages, tc.schema)
			if err != nil {
				t.Fatalf("native Render: %v", err)
			}

			equal, nativeJSON, oracleJSON, err := Equal(native, oracle)
			if err != nil {
				t.Fatalf("Equal: %v", err)
			}
			if !equal {
				t.Errorf("native RenderedPrompt diverged from BAML oracle\n--- native ---\n%s\n--- BAML oracle ---\n%s\n--- captured body ---\n%s",
					nativeJSON, oracleJSON, string(body))
			}
		})
	}
}

// oracleRegistry builds a client_registry pointing at a fake OpenAI base URL
// (intercepted by the capture transport). extra options (e.g.
// allowed_role_metadata) are merged in.
func oracleRegistry(extra map[string]any) *dynclient.ClientRegistry {
	opts := map[string]any{
		"model":    "gpt-4o-mini",
		"base_url": "http://native-prompt-oracle.invalid/v1",
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

// toDynMessages converts the native corpus messages into dynclient request
// messages so the same fixture drives both sides.
func toDynMessages(msgs []Message) []dynclient.Message {
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

func toDynPart(p *ContentPart) dynclient.ContentPart {
	switch {
	case p.Text != nil:
		return dynclient.ContentPart{Type: "text", Text: p.Text}
	case p.Image != nil:
		return dynclient.ContentPart{Type: "image", Image: toMediaInput(p.Image)}
	case p.Audio != nil:
		return dynclient.ContentPart{Type: "audio", Audio: toMediaInput(p.Audio)}
	case p.PDF != nil:
		return dynclient.ContentPart{Type: "pdf", PDF: toMediaInput(p.PDF)}
	case p.Video != nil:
		return dynclient.ContentPart{Type: "video", Video: toMediaInput(p.Video)}
	case p.OutputFormat:
		return dynclient.ContentPart{Type: "output_format"}
	}
	return dynclient.ContentPart{}
}

func toMediaInput(m *Media) *bamlutils.MediaInput {
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

// extractOracleRendered reconstructs a canonical RenderedPrompt from BAML's
// captured OpenAI chat-completions body. Roles, ordered content parts (text +
// media), and message-level cache_control are recovered; allow_duplicate_role
// is not serialized to the wire, so it is set to false (the dynamic template
// never emits __baml_allow_dupe_role__, so false is provably correct here).
func extractOracleRendered(body []byte) (*RenderedPrompt, error) {
	var root map[string]any
	if err := stdjson.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	msgsAny, ok := root["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("body missing messages array (got %T)", root["messages"])
	}

	rp := &RenderedPrompt{Kind: KindChat}
	for i, ma := range msgsAny {
		msg, ok := ma.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] not an object: %T", i, ma)
		}
		// Fail closed on a missing/empty/non-string role rather than
		// normalizing it to "" (which would hide provider-shape drift).
		role, ok := msg["role"].(string)
		if !ok || role == "" {
			return nil, fmt.Errorf("messages[%d] missing or non-string role (got %T %v)", i, msg["role"], msg["role"])
		}

		parts, err := extractParts(msg["content"])
		if err != nil {
			return nil, fmt.Errorf("messages[%d] content: %w", i, err)
		}

		rm := RenderedMessage{Role: role, AllowDuplicateRole: false, Parts: parts}
		if cc := findCacheControl(msg); cc != nil {
			rm.Meta = map[string]any{"cache_control": cc}
		}
		rp.Messages = append(rp.Messages, rm)
	}
	return rp, nil
}

// extractParts canonicalizes an OpenAI message `content` (string or array of
// typed blocks) into ordered Parts, FAILING CLOSED on any shape it does not
// recognise: a missing/null content, a non-object block, a text block without a
// string text field, an image_url block without a usable url, or an unsupported
// block type all return an error. Silently normalizing or discarding these
// would let unexpected BAML provider-shape drift produce a misleading parity
// PASS; instead it surfaces as a test failure.
func extractParts(content any) ([]Part, error) {
	switch c := content.(type) {
	case string:
		return []Part{textPart(c)}, nil
	case []any:
		parts := make([]Part, 0, len(c))
		for j, ba := range c {
			block, ok := ba.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content block[%d] is not an object: %T", j, ba)
			}
			btype, _ := block["type"].(string)
			switch btype {
			case "text":
				s, ok := block["text"].(string)
				if !ok {
					return nil, fmt.Errorf("content block[%d] (text): missing or non-string text field", j)
				}
				parts = append(parts, textPart(s))
			case "image_url":
				iu, ok := block["image_url"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("content block[%d] (image_url): missing or non-object image_url", j)
				}
				mp, err := mediaFromImageURL(iu)
				if err != nil {
					return nil, fmt.Errorf("content block[%d]: %w", j, err)
				}
				parts = append(parts, Part{Media: mp})
			default:
				return nil, fmt.Errorf("content block[%d]: unsupported block type %q", j, btype)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("missing or unexpected content shape %T", content)
	}
}

// mediaFromImageURL reconstructs a MediaPart from an OpenAI image_url object,
// failing closed on a missing/empty url or a malformed data URI.
func mediaFromImageURL(iu map[string]any) (*MediaPart, error) {
	url, ok := iu["url"].(string)
	if !ok || url == "" {
		return nil, fmt.Errorf("image_url: missing or non-string url")
	}
	if strings.HasPrefix(url, "data:") {
		// data:{mime};base64,{b64}
		rest := strings.TrimPrefix(url, "data:")
		idx := strings.Index(rest, ";base64,")
		if idx < 0 {
			return nil, fmt.Errorf("image_url: data URI without ;base64, segment: %q", url)
		}
		mime := rest[:idx]
		b64 := rest[idx+len(";base64,"):]
		if mime == "" || b64 == "" {
			return nil, fmt.Errorf("image_url: data URI with empty mime or base64: %q", url)
		}
		return &MediaPart{Kind: "image", Mime: mime, Base64: b64}, nil
	}
	return &MediaPart{Kind: "image", URL: url}, nil
}

// findCacheControl returns the message's cache_control value, whether BAML
// placed it at the message level or on a content block. Returns nil if absent.
func findCacheControl(msg map[string]any) any {
	if cc, ok := msg["cache_control"]; ok {
		return cc
	}
	if blocks, ok := msg["content"].([]any); ok {
		for _, ba := range blocks {
			if block, ok := ba.(map[string]any); ok {
				if cc, ok := block["cache_control"]; ok {
					return cc
				}
			}
		}
	}
	return nil
}
