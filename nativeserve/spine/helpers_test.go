//go:build nanollm_integration

package spine_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
	"github.com/invakid404/baml-rest/worker"
)

const jsonAliasMethod = "StaticRecursiveAliasJSON"

// loopback is a loopback OpenAI-compatible provider with a request-count spy. Its
// handler is fully caller-controlled so a test can serve a canned success, a fault,
// or block until released.
type loopback struct {
	srv     *httptest.Server
	hits    atomic.Int64
	handler func(w http.ResponseWriter, r *http.Request)
}

func newLoopback(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *loopback {
	t.Helper()
	lb := &loopback{handler: handler}
	lb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lb.hits.Add(1)
		lb.handler(w, r)
	}))
	t.Cleanup(lb.srv.Close)
	return lb
}

// count returns the number of requests the loopback has received.
func (lb *loopback) count() int64 { return lb.hits.Load() }

// baseURL is the OpenAI-style base URL (with /v1) a client points at.
func (lb *loopback) baseURL() string { return lb.srv.URL + "/v1" }

// okChatCompletion writes a 200 OpenAI chat-completion whose assistant content is
// content (the model's returned text).
func okChatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	payload := map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": content}},
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// reconstructJSONAlias builds the ExecBridge-U1 JSON-alias project from source,
// reconstructs the StaticRecursiveAliasJSON function, and points its client's
// base_url at baseURL (the descriptor otherwise carries the corpus placeholder URL).
func reconstructJSONAlias(t *testing.T, baseURL string) promptdescriptor.Function {
	t.Helper()
	proj, err := nativespine.BuildFromSource(nativespine.JSONAliasFixtureSources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	var m projectdescriptor.Method
	for _, mm := range proj.Methods {
		if mm.Name == jsonAliasMethod {
			m = mm
		}
	}
	if m.Name == "" {
		t.Fatalf("%s not admitted (methods=%d diagnostics=%d)", jsonAliasMethod, len(proj.Methods), len(proj.Diagnostics))
	}
	fn, err := nativespine.ReconstructFunction(proj, m)
	if err != nil {
		t.Fatalf("ReconstructFunction: %v", err)
	}
	return withBaseURL(t, fn, baseURL)
}

// withBaseURL returns a copy of fn whose client base_url transport option is set to
// baseURL (the loopback), mirroring how the static-serve integration test injects its
// loopback URL. It fails if the descriptor carries no base_url option to override.
func withBaseURL(t *testing.T, fn promptdescriptor.Function, baseURL string) promptdescriptor.Function {
	t.Helper()
	opts := make([]promptdescriptor.ClientOption, len(fn.ClientConfig.TransportOptions))
	copy(opts, fn.ClientConfig.TransportOptions)
	found := false
	for i := range opts {
		if opts[i].Key == "base_url" {
			opts[i].Value = promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: baseURL}
			found = true
		}
	}
	if !found {
		t.Fatalf("descriptor client carries no base_url transport option to override")
	}
	fn.ClientConfig.TransportOptions = opts
	return fn
}

// jsonAliasMethods pairs the reconstructed descriptor with the emitted binding.
func jsonAliasMethods(fn promptdescriptor.Function) []spine.SpineMethod {
	return []spine.SpineMethod{{Function: fn, Binding: nativespinejsonfixture.Binding()}}
}

// newHandler builds the production spine executor over the given methods and wraps it
// in the emitted JSON-alias runtime + a worker.Handler.
func newHandler(t *testing.T, exec *spine.UnaryExecutor) *worker.Handler {
	t.Helper()
	rt := nativespinejsonfixture.NewNativeRuntime(exec)
	rt.InitRuntime()
	h, err := worker.New(worker.Config{Runtime: rt})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return h
}

// callInput is the JSON envelope for a StaticRecursiveAliasJSON call.
func callInput(topic string) []byte {
	return []byte(fmt.Sprintf(`{"topic":%q}`, topic))
}

// parseInput is the JSON envelope for a /parse of raw model text.
func parseInput(raw string) []byte {
	b, _ := json.Marshal(map[string]string{"raw": raw})
	return b
}
