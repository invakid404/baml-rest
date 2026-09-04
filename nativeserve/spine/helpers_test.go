//go:build nanollm_integration

package spine_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"
	"github.com/invakid404/baml-rest/worker"
)

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

// newJSONExec builds the production spine executor over the admitted JSON-alias
// project with base_url pointed at baseURL and the given emitted binding (default =
// nativespinejsonfixture.Binding). exec is the exact executor (nil = default).
func newJSONExec(t *testing.T, baseURL string, exec *llmhttp.ExactExecutor, binding ...bamlutils.NativeSpineUnaryBinding) *spine.UnaryExecutor {
	t.Helper()
	proj := injectBaseURL(t, jsonAliasProject(t), baseURL)
	b := nativespinejsonfixture.Binding()
	if len(binding) > 0 {
		b = binding[0]
	}
	e, err := spine.NewUnaryExecutor(proj, []bamlutils.NativeSpineUnaryBinding{b}, exec)
	if err != nil {
		t.Fatalf("NewUnaryExecutor: %v", err)
	}
	return e
}

// newJSONStreamExec builds the production spine STREAM executor over the admitted
// JSON-alias project with base_url pointed at baseURL and the given emitted stream
// binding (default = nativespinejsonfixture.StreamBinding). It is what the native-only
// worker runtime is driven by, so a handler-level test exercises the real serving path.
func newJSONStreamExec(t *testing.T, baseURL string, exec *llmhttp.ExactExecutor, binding ...bamlutils.NativeSpineStreamBinding) *spine.StreamExecutor {
	t.Helper()
	proj := injectBaseURL(t, jsonAliasProject(t), baseURL)
	b := nativespinejsonfixture.StreamBinding()
	if len(binding) > 0 {
		b = binding[0]
	}
	e, err := spine.NewStreamExecutor(proj, []spine.StreamRegistration{{Binding: b, BuildMethod: nativespinejsonfixture.BuildMethod}}, exec)
	if err != nil {
		t.Fatalf("NewStreamExecutor: %v", err)
	}
	return e
}

// newHandler wraps the spine STREAM executor in the emitted JSON-alias runtime + a
// worker.Handler (the production dispatch path that passes the adapter into Call and
// Stream). The runtime requires the stream contract, so the handler serves /call,
// /stream, /stream-with-raw, and both parse routes off one executor.
func newHandler(t *testing.T, exec *spine.StreamExecutor) *worker.Handler {
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
