package llmhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
)

// TestClientPerInstanceRewriteRules pins that two Clients with
// different RewriteRules slices resolve the same request URL
// differently. This is the load-bearing per-instance assertion:
// each options-constructed Client carries its own rewrite ruleset
// rather than reaching into urlrewrite.GlobalRules.
func TestClientPerInstanceRewriteRules(t *testing.T) {
	t.Parallel()

	clientA := NewClientWithOptions(ClientOptions{
		Mode:         ClientModeNetHTTP,
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://a.local:8080/"}},
	})
	clientB := NewClientWithOptions(ClientOptions{
		Mode:         ClientModeNetHTTP,
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://b.local:9090/"}},
	})

	req := &Request{URL: "https://upstream.example/v1/chat"}

	gotA := clientA.resolveRequestURL(req)
	gotB := clientB.resolveRequestURL(req)
	if gotA == gotB {
		t.Fatalf("expected different rewrites between clients; both got %q", gotA)
	}
	if want := "http://a.local:8080/v1/chat"; gotA != want {
		t.Errorf("client A: got %q, want %q", gotA, want)
	}
	if want := "http://b.local:9090/v1/chat"; gotB != want {
		t.Errorf("client B: got %q, want %q", gotB, want)
	}
}

// TestClientLegacyNewClientHonorsGlobalRewrites pins that the
// legacy NewClient(nil) path still consults urlrewrite.GlobalRules
// when the per-Client rules are nil. This is the compatibility
// shape that env-driven setups (cmd/serve at startup, the existing
// integration tests) depend on.
func TestClientLegacyNewClientHonorsGlobalRewrites(t *testing.T) {
	// Not parallel: mutates the urlrewrite global cache via env.

	t.Setenv("BAML_REST_BASE_URL_REWRITES", "https://upstream.example/=http://legacy.local:7070/")
	urlrewrite.ResetGlobalRules()
	t.Cleanup(func() {
		urlrewrite.ResetGlobalRules()
	})

	client := NewClient(nil)
	got := client.resolveRequestURL(&Request{URL: "https://upstream.example/v1/chat"})
	if want := "http://legacy.local:7070/v1/chat"; got != want {
		t.Errorf("legacy client rewrite: got %q, want %q", got, want)
	}
}

// TestNewDefaultClientWithOptionsUsesTunedTransport pins that the
// startup constructor uses defaultLLMTransport — not http.DefaultTransport
// — so the explicit per-handler client preserves the connection-pool
// sizing the legacy DefaultClient relied on. A regression that forgot
// to wire the tuned transport would silently revert to Go's default
// MaxIdleConnsPerHost=2 and slow every concurrent LLM request.
func TestNewDefaultClientWithOptionsUsesTunedTransport(t *testing.T) {
	t.Parallel()

	client := NewDefaultClientWithOptions(ClientOptions{Mode: ClientModeNetHTTP})
	if client.httpClient == nil {
		t.Fatal("expected non-nil underlying http.Client")
	}
	if client.httpClient.Transport != defaultLLMTransport {
		t.Errorf("expected NewDefaultClientWithOptions to install defaultLLMTransport; got %T", client.httpClient.Transport)
	}
}

// TestNewClientWithOptionsForcesModeWithoutEnv pins that Mode is
// honoured from the options struct regardless of the env. Without
// the explicit-mode path, two Clients in the same process would
// share the env-driven mode and a library caller could not dictate
// its own backend.
func TestNewClientWithOptionsForcesModeWithoutEnv(t *testing.T) {
	// Not parallel: sets env to prove the override.

	t.Setenv(EnvVarClientMode, "")
	client := NewClientWithOptions(ClientOptions{Mode: ClientModeFastHTTP})
	if client.cache == nil {
		t.Fatal("expected protocol cache to be initialised")
	}
	if client.cache.mode != ClientModeFastHTTP {
		t.Errorf("expected cache mode to be FastHTTP, got %v", client.cache.mode)
	}
}

// TestClientModeFromEnv pins the env-parser used at cmd/serve and
// cmd/worker startup so a regression to the parsing rules would
// surface here rather than as a server behaviour drift.
func TestClientModeFromEnv(t *testing.T) {
	// Not parallel: sets the env var.

	cases := []struct {
		name string
		raw  string
		want ClientMode
	}{
		{"empty defaults to auto", "", ClientModeAuto},
		{"unknown defaults to auto", "garbage", ClientModeAuto},
		{"explicit fasthttp", "fasthttp", ClientModeFastHTTP},
		{"explicit nethttp", "nethttp", ClientModeNetHTTP},
		{"trimmed and case-folded", "  FastHTTP  ", ClientModeFastHTTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv(EnvVarClientMode, tc.raw)
			defer os.Unsetenv(EnvVarClientMode)
			if got := ClientModeFromEnv(); got != tc.want {
				t.Errorf("ClientModeFromEnv with %q: got %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestExecuteStreamUsesPerClientRewrites is the end-to-end pin that
// rewrites apply at the executor seam, not just the resolver helper.
// Two clients pointed at the same backend URL but with different
// rewrite rules MUST land on different test servers.
func TestExecuteStreamUsesPerClientRewrites(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var hits []string

	mkServer := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, label)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
		}))
	}

	servA := mkServer("A")
	defer servA.Close()
	servB := mkServer("B")
	defer servB.Close()

	clientA := NewClientWithOptions(ClientOptions{
		Mode:         ClientModeNetHTTP,
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: servA.URL + "/"}},
	})
	clientB := NewClientWithOptions(ClientOptions{
		Mode:         ClientModeNetHTTP,
		RewriteRules: []urlrewrite.Rule{{From: "https://upstream.example/", To: servB.URL + "/"}},
	})

	req := &Request{
		URL:     "https://upstream.example/v1/chat",
		Method:  "POST",
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    `{}`,
	}

	for label, client := range map[string]*Client{"A": clientA, "B": clientB} {
		resp, err := client.ExecuteStream(context.Background(), req)
		if err != nil {
			t.Fatalf("client %s: ExecuteStream: %v", label, err)
		}
		// Drain so the SSE goroutine exits cleanly.
		for range resp.Events {
		}
		_ = <-resp.Errc
		resp.Close()
	}

	mu.Lock()
	got := append([]string(nil), hits...)
	mu.Unlock()
	want := []string{"A", "B"}
	// Map iteration order isn't deterministic; sort by checking both
	// labels appear exactly once.
	if !reflect.DeepEqual(sortedCopy(got), sortedCopy(want)) {
		t.Errorf("expected hits = %v (any order), got %v", want, got)
	}
}

// TestClientWithOptionsIgnoresGlobalRewritesWhenRulesNil is the
// regression pin for the bug where NewClientWithOptions /
// NewDefaultClientWithOptions silently fell back to
// urlrewrite.GlobalRules when RewriteRules was nil. The contract
// documented on ClientOptions.RewriteRules is "nil = no rewrites"
// for the options constructors; the runtime now matches the doc.
//
// Without the useGlobalRewriteRules gate, this test would fail —
// an explicit options Client would inherit the process-wide
// BAML_REST_BASE_URL_REWRITES value and surprise programmatic
// callers who passed no rules on purpose.
func TestClientWithOptionsIgnoresGlobalRewritesWhenRulesNil(t *testing.T) {
	// Not parallel: mutates the urlrewrite global cache via env.
	t.Setenv("BAML_REST_BASE_URL_REWRITES", "https://upstream.example/=http://global-leak.local:9999/")
	urlrewrite.ResetGlobalRules()
	t.Cleanup(func() {
		urlrewrite.ResetGlobalRules()
	})

	client := NewClientWithOptions(ClientOptions{
		Mode: ClientModeNetHTTP,
		// RewriteRules deliberately omitted (nil) — must NOT pick
		// up the env value above.
	})

	const orig = "https://upstream.example/v1/chat"
	got := client.resolveRequestURL(&Request{URL: orig})
	if got != orig {
		t.Errorf("expected options client with nil rules to skip global rewrites; got %q, want %q",
			got, orig)
	}

	// NewDefaultClientWithOptions shares the constructor — same
	// guarantee. Pinning it independently catches a future
	// refactor that diverges the two paths.
	def := NewDefaultClientWithOptions(ClientOptions{Mode: ClientModeNetHTTP})
	if gotDef := def.resolveRequestURL(&Request{URL: orig}); gotDef != orig {
		t.Errorf("expected NewDefaultClientWithOptions with nil rules to skip global rewrites; got %q, want %q",
			gotDef, orig)
	}
}

// TestClientWithOptionsDefensiveCopiesRewriteRules pins that the
// constructor takes a snapshot of opts.RewriteRules. A library
// caller mutating the slice after Client construction must not
// affect rewrites applied at request time — otherwise concurrent
// Execute / ExecuteStream calls reading c.rewriteRules race with
// caller-side mutation.
func TestClientWithOptionsDefensiveCopiesRewriteRules(t *testing.T) {
	t.Parallel()

	rules := []urlrewrite.Rule{{From: "https://upstream.example/", To: "http://snapshot.local:8080/"}}
	client := NewClientWithOptions(ClientOptions{
		Mode:         ClientModeNetHTTP,
		RewriteRules: rules,
	})

	// Mutate the caller-side slice after construction. The Client
	// must keep dispatching against the snapshot it took at
	// construction time.
	rules[0] = urlrewrite.Rule{From: "https://upstream.example/", To: "http://post-mutation.local:1111/"}

	got := client.resolveRequestURL(&Request{URL: "https://upstream.example/v1/chat"})
	if want := "http://snapshot.local:8080/v1/chat"; got != want {
		t.Errorf("expected snapshot rule to survive caller-side mutation; got %q, want %q", got, want)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	// tiny stable insertion sort — slice is at most 2 elements
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
