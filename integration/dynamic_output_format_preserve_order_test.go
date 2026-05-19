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

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
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

	registryJSON, err := json.Marshal(opts.ClientRegistry)
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
		idx := strings.Index(window, name)
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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
