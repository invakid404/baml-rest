//go:build nanollm_integration

package planassert

// The SHARED nanollm-plan client + assertions for both Phase 5.1 legs. Gated by
// `nanollm_integration` (the opt-in tag): this file imports nanollm, so it is
// only compiled when a leg's gated test binary links the public nanollm-ffi
// module's automatically-selected embedded prebuilt FFI archive. It imports NO
// BAML runtime, so sharing it never links the two legs' distinct BAML runtimes
// into one binary.

import (
	"testing"

	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/testutil"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// NewPrepareClient builds a nanollm engine with ONE openai alias configured to
// the literal fence values, a zero retry budget, and no fallbacks; Env nil and
// UseProcessEnv false so no ambient credential/base can mask a difference. target
// overrides the configured target (FenceModel for parity rows; a different value
// only for the target-rewrite negative control).
func NewPrepareClient(t *testing.T, target string) *nanollm.Client {
	t.Helper()
	c, err := nanollm.New(nanollm.Config{
		Models: []nanollm.ModelConfig{{
			Name:       FenceAlias,
			Model:      "openai/" + target,
			APIKey:     FenceAPIKey,
			BaseURL:    FenceBaseURL,
			MaxRetries: 0,
		}},
		Env:           nil,
		UseProcessEnv: false,
	})
	if err != nil {
		t.Fatalf("nanollm.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// AssertFenceURL requires the URL to equal EXACTLY the fake base + /v1 +
// /chat/completions (WantURL) — the Phase 5.1 field policy is URL-exact, so a
// substring check (which would accept an appended path like
// /chat/completions/extra) is not enough. An ambient-env base cannot satisfy it.
func AssertFenceURL(t *testing.T, url string) {
	t.Helper()
	if url != WantURL {
		t.Errorf("url = %q, want %q", url, WantURL)
	}
}

// AssertAuth requires exactly one authorization header on each side (the unique
// map guarantees at most one) with the exact fake bearer value, redacting the
// value in any diagnostic.
func AssertAuth(t *testing.T, bamlHeaders, prepHeaders map[string]string) {
	t.Helper()
	wantAuth := "Bearer " + FenceAPIKey
	if bv, ok := testutil.Authorization(bamlHeaders); !ok {
		t.Error("BAML request carries no authorization header")
	} else if bv != wantAuth {
		t.Errorf("BAML authorization mismatch (redacted): got %s want %s",
			testutil.RedactValue("authorization", bv), testutil.RedactValue("authorization", wantAuth))
	}
	if pv, ok := testutil.Authorization(prepHeaders); !ok {
		t.Error("nanollm plan carries no authorization header")
	} else if pv != wantAuth {
		t.Errorf("nanollm authorization mismatch (redacted): got %s want %s",
			testutil.RedactValue("authorization", pv), testutil.RedactValue("authorization", wantAuth))
	}
}

// AssertNanollmHeaderOrder pins nanollm's OWN known no-override order —
// Content-Type: application/json then Authorization: Bearer <fake> — and is NEVER
// compared to BAML (that order/casing claim is explicitly declined).
func AssertNanollmHeaderOrder(t *testing.T, headers [][2]string) {
	t.Helper()
	want := [][2]string{
		{"Content-Type", "application/json"},
		{"Authorization", "Bearer " + FenceAPIKey},
	}
	if len(headers) != len(want) {
		t.Errorf("nanollm emitted %d headers, want %d", len(headers), len(want))
		return
	}
	for i := range want {
		if headers[i][0] != want[i][0] {
			t.Errorf("nanollm header[%d] name = %q, want %q", i, headers[i][0], want[i][0])
		}
		if headers[i][1] != want[i][1] {
			t.Errorf("nanollm header[%d] %q value mismatch (redacted): got %s want %s",
				i, headers[i][0], testutil.RedactValue(headers[i][0], headers[i][1]), testutil.RedactValue(want[i][0], want[i][1]))
		}
	}
}

// AssertResponseFormat requires the structural OpenAI JSON response format.
func AssertResponseFormat(t *testing.T, prep *nanollm.PreparedRequest) {
	t.Helper()
	if prep.ResponseFormat != nanollm.FormatJSON {
		t.Errorf("response format = %q, want json", prep.ResponseFormat)
	}
}

// AssertMeta requires the plan metadata to resolve the alias/target/openai
// provider/ChatCompletion/non-stream, with no jq transform and a zero nanollm
// retry budget (baml-rest owns retries/fallbacks/round-robin).
func AssertMeta(t *testing.T, prep *nanollm.PreparedRequest, wantTarget string) {
	t.Helper()
	m := prep.Meta
	if m.ModelAlias != FenceAlias {
		t.Errorf("meta.ModelAlias = %q, want %q", m.ModelAlias, FenceAlias)
	}
	if m.TargetModel != wantTarget {
		t.Errorf("meta.TargetModel = %q, want %q", m.TargetModel, wantTarget)
	}
	if m.Provider != nativebody.ProviderOpenAI {
		t.Errorf("meta.Provider = %q, want %q", m.Provider, nativebody.ProviderOpenAI)
	}
	if m.RequestType != nanollm.ChatCompletion {
		t.Errorf("meta.RequestType = %q, want %q", m.RequestType, nanollm.ChatCompletion)
	}
	if m.Stream {
		t.Error("meta.Stream = true, want false (non-streaming attempt)")
	}
	if m.TransformKey != "" {
		t.Errorf("meta.TransformKey = %q, want empty (openai has no jq transform)", m.TransformKey)
	}
	if m.MaxRetries != 0 {
		t.Errorf("meta.MaxRetries = %d, want 0 (baml-rest owns the retry budget)", m.MaxRetries)
	}
}

// AssertUnsigned requires the positive unsigned-OpenAI state: no SigV4 signing
// window and not expired.
func AssertUnsigned(t *testing.T, prep *nanollm.PreparedRequest) {
	t.Helper()
	if prep.Meta.SignedAt != nil {
		t.Errorf("unsigned OpenAI plan must have SignedAt nil, got %v", prep.Meta.SignedAt)
	}
	if prep.Meta.ExpiresAt != nil {
		t.Errorf("unsigned OpenAI plan must have ExpiresAt nil, got %v", prep.Meta.ExpiresAt)
	}
	if prep.Expired() {
		t.Error("unsigned OpenAI plan must not be Expired()")
	}
}
