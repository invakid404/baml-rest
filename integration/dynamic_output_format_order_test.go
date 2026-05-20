//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestDynamicOutputFormatOrderStable pins #309: identical /call/_dynamic
// requests must render byte-identical upstream prompts, so prompt caching
// stays warm across repeats of the same schema. The Go map literals below
// are intentionally non-alphabetical, so the codegen's sorted TypeBuilder
// population path has to do real work for the hash to converge.
func TestDynamicOutputFormatOrderStable(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	scenarioID := "test-dynamic-output-format-order"
	opts := setupNonStreamingScenario(t, scenarioID, `{
  "alpha": 1,
  "bravo": "b",
  "charlie": {"city": "Sofia", "street": "Main", "zip": "1000"},
  "delta": "d",
  "echo": true,
  "foxtrot": "ACTIVE",
  "golf": {"alpha": 7, "preference": "HIGH", "status": "PENDING", "zulu": "z"},
  "hotel": ["x", "y"]
}`)

	systemText := "Return only JSON matching the requested schema."
	userText := "Build a sample record."

	req := testutil.DynamicRequest{
		Messages: []testutil.DynamicMessage{
			{
				Role: "system",
				Content: []testutil.DynamicContentPart{
					{Type: "text", Text: strPtr(systemText)},
					{Type: "output_format"},
				},
			},
			{Role: "user", Content: userText},
		},
		ClientRegistry: opts.ClientRegistry,
		OutputSchema: &testutil.DynamicOutputSchema{
			Properties: map[string]*testutil.DynamicProperty{
				"delta":   {Type: "string"},
				"alpha":   {Type: "int"},
				"hotel":   {Type: "list", Items: &testutil.DynamicTypeSpec{Type: "string"}},
				"charlie": {Ref: "Address"},
				"bravo":   {Type: "string"},
				"foxtrot": {Ref: "Status"},
				"echo":    {Type: "bool"},
				"golf":    {Ref: "Profile"},
			},
			Classes: map[string]*testutil.DynamicClass{
				"Profile": {
					Properties: map[string]*testutil.DynamicProperty{
						"zulu":       {Type: "string"},
						"alpha":      {Type: "int"},
						"status":     {Ref: "Status"},
						"preference": {Ref: "Preference"},
					},
				},
				"Address": {
					Properties: map[string]*testutil.DynamicProperty{
						"zip":    {Type: "string"},
						"city":   {Type: "string"},
						"street": {Type: "string"},
					},
				},
			},
			Enums: map[string]*testutil.DynamicEnum{
				"Status": {
					Values: []*testutil.DynamicEnumValue{
						{Name: "PENDING"},
						{Name: "ACTIVE"},
						{Name: "ARCHIVED"},
					},
				},
				"Preference": {
					Values: []*testutil.DynamicEnumValue{
						{Name: "LOW"},
						{Name: "MEDIUM"},
						{Name: "HIGH"},
					},
				},
			},
		},
	}

	const iterations = 50
	hashCounts := make(map[string]int, 2)
	samples := make(map[string]string, 2)

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		resp, err := BAMLClient.DynamicCall(ctx, req)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: DynamicCall failed: %v", i, err)
		}
		if resp.StatusCode != 200 {
			cancel()
			t.Fatalf("iteration %d: expected status 200, got %d: %s", i, resp.StatusCode, resp.Error)
		}

		capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: failed to get last request: %v", i, err)
		}
		cancel()

		prompt, err := extractFirstMessageText(capturedBody)
		if err != nil {
			t.Fatalf("iteration %d: %v\ncaptured body: %s", i, err, string(capturedBody))
		}

		sum := sha256.Sum256([]byte(prompt))
		key := hex.EncodeToString(sum[:])
		hashCounts[key]++
		if _, ok := samples[key]; !ok {
			samples[key] = prompt
		}
	}

	if len(hashCounts) != 1 {
		t.Errorf("expected exactly 1 unique prompt hash across %d iterations, got %d", iterations, len(hashCounts))
		shown := 0
		for key, count := range hashCounts {
			t.Logf("hash %s: %d iterations", key, count)
			if shown < 2 {
				t.Logf("sample for %s:\n%s", key, samples[key])
				shown++
			}
		}
	}
}

// extractFirstMessageText pulls the first chat message's text content out of
// an OpenAI-compatible mock-LLM capture body. The captured body's first
// message is either a plain string or an array of content parts; either way
// we return the concatenated text, since that is what feeds the rendered
// output_format block we are pinning.
func extractFirstMessageText(body []byte) (string, error) {
	var captured map[string]any
	if err := sonic.Unmarshal(body, &captured); err != nil {
		return "", fmt.Errorf("unmarshal captured body: %w", err)
	}

	messagesAny, ok := captured["messages"]
	if !ok {
		return "", fmt.Errorf("captured body missing 'messages' field")
	}
	messages, ok := messagesAny.([]any)
	if !ok || len(messages) == 0 {
		return "", fmt.Errorf("captured body 'messages' is not a non-empty array: %T", messagesAny)
	}

	first, ok := messages[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("messages[0] is not an object: %T", messages[0])
	}
	content, ok := first["content"]
	if !ok {
		return "", fmt.Errorf("messages[0] missing 'content' field")
	}

	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var combined string
		for _, partAny := range c {
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				combined += text
			}
		}
		return combined, nil
	default:
		return "", fmt.Errorf("messages[0].content has unexpected shape: %T", content)
	}
}
