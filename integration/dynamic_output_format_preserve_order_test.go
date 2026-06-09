//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestDynamicOutputFormatPreserveSchemaOrder pins #313: when a caller
// opts into preserve_schema_order, identical raw-JSON requests must
// (1) render byte-identical upstream prompts across repeats (stability,
// to keep prompt caching warm) and (2) follow the JSON wire order of
// properties/classes/enums and per-class properties rather than the
// alphabetical fallback (order fidelity).
//
// The request body is built as a raw JSON string so the JSON object
// order capture path is exercised end-to-end. Go map literals would
// not carry order and would defeat the whole invariant.
//
// The nested class is named "Mailbox" rather than "Address" because
// integration/baml_src/types.baml already declares a static (non-@@dynamic)
// `class Address`. When a dynamic schema reuses that name, the generated
// applyDynamicTypes short-circuits via ClassExists("Address") and the
// static class's source order wins for that class — masking the
// order-fidelity invariant we're trying to pin here. Mailbox sidesteps
// the collision and exercises the user-defined-class path cleanly.
func TestDynamicOutputFormatPreserveSchemaOrder(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	scenarioID := "test-dynamic-output-format-preserve-order"
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

	registryJSON, err := sonic.Marshal(opts.ClientRegistry)
	if err != nil {
		t.Fatalf("marshal client_registry: %v", err)
	}

	rawReq := fmt.Sprintf(`{
  "preserve_schema_order": true,
  "messages": [
    {
      "role": "system",
      "content": [
        {"type": "text", "text": "Return only JSON matching the requested schema."},
        {"type": "output_format"}
      ]
    },
    {"role": "user", "content": "Build a sample record."}
  ],
  "client_registry": %s,
  "output_schema": {
    "properties": {
      "delta": {"type": "string"},
      "alpha": {"type": "int"},
      "hotel": {"type": "list", "items": {"type": "string"}},
      "charlie": {"ref": "Mailbox"},
      "bravo": {"type": "string"},
      "foxtrot": {"ref": "Status"},
      "echo": {"type": "bool"},
      "golf": {"ref": "Profile"}
    },
    "classes": {
      "Profile": {
        "properties": {
          "zulu": {"type": "string"},
          "alpha": {"type": "int"},
          "status": {"ref": "Status"},
          "preference": {"ref": "Preference"}
        }
      },
      "Mailbox": {
        "properties": {
          "zip": {"type": "string"},
          "city": {"type": "string"},
          "street": {"type": "string"}
        }
      }
    },
    "enums": {
      "Status": {"values": [{"name": "PENDING"}, {"name": "ACTIVE"}, {"name": "ARCHIVED"}]},
      "Preference": {"values": [{"name": "LOW"}, {"name": "MEDIUM"}, {"name": "HIGH"}]}
    }
  }
}`, string(registryJSON))

	const iterations = 50
	hashCounts := make(map[string]int, 2)
	samples := make(map[string]string, 2)

	url := fmt.Sprintf("%s/call/_dynamic", TestEnv.BAMLRestURL)

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		if err := postRaw(ctx, url, rawReq); err != nil {
			cancel()
			t.Fatalf("iteration %d: POST failed: %v", i, err)
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

	// Pick one sample for order-fidelity assertions. With stability
	// passing the choice is arbitrary; with stability failing we still
	// want to surface what the renderer produced.
	var prompt string
	for _, s := range samples {
		prompt = s
		break
	}

	assertOrderIn(t, "top-level", prompt, "", []string{"delta", "alpha", "hotel", "charlie", "bravo", "foxtrot", "echo", "golf"})
	// Nested classes share property names with the top level (alpha is in
	// both top-level and Profile), so scan within the block opened by the
	// referencing field rather than the full prompt. golf -> Profile,
	// charlie -> Mailbox.
	assertOrderIn(t, "Profile", prompt, "golf: {", []string{"zulu", "alpha", "status", "preference"})
	assertOrderIn(t, "Mailbox", prompt, "charlie: {", []string{"zip", "city", "street"})
}

// TestDynamicOutputFormatPreserveSchemaOrder_ServerDefault pins the
// server-default behavior: when the server is started with
// BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT truthy, dynamic requests that
// omit preserve_schema_order must inherit the default and render the
// output_format in JSON wire order. A per-request
// `preserve_schema_order: false` overrides the server default back to
// the alphabetical fallback. The test boots a dedicated env (the
// shared TestEnv container has no such env var) and exercises both
// legs against the same /call/_dynamic endpoint.
func TestDynamicOutputFormatPreserveSchemaOrder_ServerDefault(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	opts := matrixSetupOptions()
	opts.RuntimeEnv = map[string]string{
		"BAML_REST_PRESERVE_SCHEMA_ORDER_DEFAULT": "true",
	}

	// Centralized, mode-aware setup budget (#424).
	setupCtx, setupCancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer setupCancel()

	env, err := testutil.Setup(setupCtx, opts)
	if err != nil {
		t.Fatalf("Failed to setup dedicated env: %v", err)
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		if err := env.Terminate(termCtx); err != nil {
			t.Logf("dedicated env Terminate: %v", err)
		}
	}()

	mockClient := mockllm.NewClient(env.MockLLMURL)

	// Helper: register a non-streaming scenario on the dedicated mock and
	// return a ClientRegistry that targets it. The shared
	// setupNonStreamingScenario uses TestEnv / MockClient — we need the
	// dedicated env's clients instead.
	registerScenario := func(t *testing.T, scenarioID, content string) *testutil.ClientRegistry {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      0,
			InitialDelayMs: 50,
		}
		if err := mockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("RegisterScenario: %v", err)
		}
		return testutil.CreateTestClient(env.MockLLMInternal, scenarioID)
	}

	responseContent := `{
  "alpha": 1,
  "bravo": "b",
  "charlie": {"city": "Sofia", "street": "Main", "zip": "1000"},
  "delta": "d",
  "echo": true,
  "foxtrot": "ACTIVE",
  "golf": {"alpha": 7, "preference": "HIGH", "status": "PENDING", "zulu": "z"},
  "hotel": ["x", "y"]
}`

	// Build a raw JSON request body with the same schema as the #313
	// preserve-order test, but parameterized on the preserve_schema_order
	// field so we can exercise omitted vs. false in the same env.
	buildReq := func(t *testing.T, registry *testutil.ClientRegistry, preserveField string) string {
		t.Helper()
		registryJSON, err := sonic.Marshal(registry)
		if err != nil {
			t.Fatalf("marshal client_registry: %v", err)
		}
		return fmt.Sprintf(`{
  %s
  "messages": [
    {
      "role": "system",
      "content": [
        {"type": "text", "text": "Return only JSON matching the requested schema."},
        {"type": "output_format"}
      ]
    },
    {"role": "user", "content": "Build a sample record."}
  ],
  "client_registry": %s,
  "output_schema": {
    "properties": {
      "delta": {"type": "string"},
      "alpha": {"type": "int"},
      "hotel": {"type": "list", "items": {"type": "string"}},
      "charlie": {"ref": "Mailbox"},
      "bravo": {"type": "string"},
      "foxtrot": {"ref": "Status"},
      "echo": {"type": "bool"},
      "golf": {"ref": "Profile"}
    },
    "classes": {
      "Profile": {
        "properties": {
          "zulu": {"type": "string"},
          "alpha": {"type": "int"},
          "status": {"ref": "Status"},
          "preference": {"ref": "Preference"}
        }
      },
      "Mailbox": {
        "properties": {
          "zip": {"type": "string"},
          "city": {"type": "string"},
          "street": {"type": "string"}
        }
      }
    },
    "enums": {
      "Status": {"values": [{"name": "PENDING"}, {"name": "ACTIVE"}, {"name": "ARCHIVED"}]},
      "Preference": {"values": [{"name": "LOW"}, {"name": "MEDIUM"}, {"name": "HIGH"}]}
    }
  }
}`, preserveField, string(registryJSON))
	}

	url := fmt.Sprintf("%s/call/_dynamic", env.BAMLRestURL)

	t.Run("omitted_inherits_server_default_true", func(t *testing.T) {
		scenarioID := "test-server-default-omitted"
		registry := registerScenario(t, scenarioID, responseContent)
		req := buildReq(t, registry, "")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := postRaw(ctx, url, req); err != nil {
			t.Fatalf("POST failed: %v", err)
		}

		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer inspectCancel()
		capturedBody, err := mockClient.GetLastRequest(inspectCtx, scenarioID)
		if err != nil {
			t.Fatalf("GetLastRequest: %v", err)
		}
		prompt, err := extractFirstMessageText(capturedBody)
		if err != nil {
			t.Fatalf("extractFirstMessageText: %v\ncaptured: %s", err, string(capturedBody))
		}

		assertOrderIn(t, "top-level (wire order)", prompt, "", []string{"delta", "alpha", "hotel", "charlie", "bravo", "foxtrot", "echo", "golf"})
		assertOrderIn(t, "Profile (wire order)", prompt, "golf: {", []string{"zulu", "alpha", "status", "preference"})
		assertOrderIn(t, "Mailbox (wire order)", prompt, "charlie: {", []string{"zip", "city", "street"})
	})

	t.Run("explicit_false_overrides_server_default", func(t *testing.T) {
		scenarioID := "test-server-default-explicit-false"
		registry := registerScenario(t, scenarioID, responseContent)
		req := buildReq(t, registry, `"preserve_schema_order": false,`)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := postRaw(ctx, url, req); err != nil {
			t.Fatalf("POST failed: %v", err)
		}

		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer inspectCancel()
		capturedBody, err := mockClient.GetLastRequest(inspectCtx, scenarioID)
		if err != nil {
			t.Fatalf("GetLastRequest: %v", err)
		}
		prompt, err := extractFirstMessageText(capturedBody)
		if err != nil {
			t.Fatalf("extractFirstMessageText: %v\ncaptured: %s", err, string(capturedBody))
		}

		// Alphabetical fallback — same schema, but the renderer must sort
		// each block lexicographically.
		assertOrderIn(t, "top-level (alphabetical)", prompt, "", []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"})
		assertOrderIn(t, "Profile (alphabetical)", prompt, "golf: {", []string{"alpha", "preference", "status", "zulu"})
		assertOrderIn(t, "Mailbox (alphabetical)", prompt, "charlie: {", []string{"city", "street", "zip"})
	})
}

// assertOrderIn checks that the listed field names appear in the
// declared order within the prompt window starting at anchor and
// ending at the matching closing brace. anchor="" scans the whole
// prompt. Order is checked via the first occurrence of each name
// inside the window; matching is good enough because each rendered
// class block lists each property exactly once.
func assertOrderIn(t *testing.T, label, prompt, anchor string, names []string) {
	t.Helper()
	window := prompt
	if anchor != "" {
		start := strings.Index(prompt, anchor)
		if start < 0 {
			t.Errorf("%s: anchor %q not found", label, anchor)
			t.Logf("prompt:\n%s", prompt)
			return
		}
		rest := prompt[start+len(anchor):]
		end := strings.Index(rest, "}")
		if end < 0 {
			t.Errorf("%s: closing brace not found after anchor %q", label, anchor)
			t.Logf("prompt:\n%s", prompt)
			return
		}
		window = rest[:end]
	}
	indices := make([]int, len(names))
	for i, name := range names {
		idx := fieldTokenIndex(window, name)
		if idx < 0 {
			t.Errorf("%s: name %q not found in window", label, name)
			t.Logf("window:\n%s\nfull prompt:\n%s", window, prompt)
			return
		}
		indices[i] = idx
	}
	for i := 1; i < len(indices); i++ {
		if indices[i] <= indices[i-1] {
			t.Errorf("%s: %q (at %d) must appear after %q (at %d) within window", label, names[i], indices[i], names[i-1], indices[i-1])
			t.Logf("window:\n%s\nfull prompt:\n%s", window, prompt)
			return
		}
	}
}

// fieldTokenIndex returns the lowest index at which name appears as a
// field-key token in window, i.e. either `name:` (unquoted, as the
// BAML output_format renderer emits) or `"name":` (quoted, as a JSON
// payload would). Returns -1 if neither form is present. Searching
// for the token rather than the bare name avoids matching incidental
// text in descriptions, sibling field names, or value renderings.
func fieldTokenIndex(window, name string) int {
	candidates := []string{
		fmt.Sprintf("%q:", name),
		fmt.Sprintf("%s:", name),
	}
	best := -1
	for _, token := range candidates {
		if idx := strings.Index(window, token); idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	return best
}

func postRaw(ctx context.Context, url, body string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		if len(respBody) > 0 {
			return fmt.Errorf("read response body: %w (partial body: %s)", readErr, string(respBody))
		}
		return fmt.Errorf("read response body: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
