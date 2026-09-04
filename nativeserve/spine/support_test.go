package spine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
)

// Shared, pure-Go (no nanollm) test support: build projectdescriptor.Projects from
// .baml corpora and pair them with the emitted JSON-alias binding. The production
// constructor takes the whole validated Project (Codex review finding 2), so the
// tests build a Project and pass it directly.

const jsonAliasMethod = "StaticRecursiveAliasJSON"

// clientBlock is the shared literal-openai client for the decline-table corpora.
const clientBlock = `client<llm> C {
  provider openai
  options { model "gpt-4o-mini" api_key "sk-x" base_url "http://127.0.0.1:0/v1" }
}
`

func corpus(types, fn string) map[string]string {
	return map[string]string{
		"clients.baml":   clientBlock,
		"types.baml":     types,
		"functions.baml": fn,
	}
}

// projectFromCorpus builds the whole-project descriptor from a .baml corpus.
func projectFromCorpus(t *testing.T, sources map[string]string) projectdescriptor.Project {
	t.Helper()
	proj, err := nativespine.BuildFromSource(sources)
	if err != nil {
		t.Fatalf("BuildFromSource: %v", err)
	}
	return proj
}

// jsonAliasProject is the admitted five-arm JSON alias project (the positive).
func jsonAliasProject(t *testing.T) projectdescriptor.Project {
	t.Helper()
	return projectFromCorpus(t, nativespine.JSONAliasFixtureSources)
}

// injectBaseURL returns a copy of proj whose client base_url transport option is set
// to baseURL (the loopback), mirroring how the static-serve integration test injects
// its loopback URL. It fails if no client carries a base_url option to override.
func injectBaseURL(t *testing.T, proj projectdescriptor.Project, baseURL string) projectdescriptor.Project {
	t.Helper()
	clients := make([]projectdescriptor.Client, len(proj.Clients))
	copy(clients, proj.Clients)
	found := false
	for ci := range clients {
		opts := make([]promptdescriptor.ClientOption, len(clients[ci].Config.TransportOptions))
		copy(opts, clients[ci].Config.TransportOptions)
		for oi := range opts {
			if opts[oi].Key == "base_url" {
				opts[oi].Value = promptdescriptor.OptionValue{Kind: promptdescriptor.OptionString, String: baseURL}
				found = true
			}
		}
		clients[ci].Config.TransportOptions = opts
	}
	if !found {
		t.Fatalf("no client carries a base_url transport option to override")
	}
	proj.Clients = clients
	return proj
}

// jsonAliasBinding returns the emitted JSON-alias binding, optionally renamed to
// match a corpus method (the decline-table corpora name their function "F").
func jsonAliasBinding(name ...string) bamlutils.NativeSpineUnaryBinding {
	b := nativespinejsonfixture.Binding()
	if len(name) > 0 {
		b.Method = name[0]
	}
	return b
}

func renameBinding(b bamlutils.NativeSpineUnaryBinding, name string) bamlutils.NativeSpineUnaryBinding {
	b.Method = name
	return b
}

// jsonAliasValues projects the exact-U1 fixture input into the argument vector the admitted
// StaticRecursiveAliasJSON descriptor binds. A near-miss invocation carrying these Values is
// a VALID exact-U1 request with exactly one fact flipped, so a gate PAST the arg binder (the
// provider gate) is reachable — an empty-Values base would decline earlier at the binder.
func jsonAliasValues(t *testing.T) []promptdescriptor.ArgumentValue {
	t.Helper()
	vals, err := nativespinejsonfixture.Binding().ProjectInput(&nativespinejsonfixture.StaticRecursiveAliasJsonInput{Topic: "weather"})
	if err != nil {
		t.Fatalf("ProjectInput: %v", err)
	}
	return vals
}

// declinesOn asserts NewUnaryExecutor over proj+binding is rejected with wantSub.
func declinesOn(t *testing.T, err error, wantSub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("registration admitted, want rejection containing %q", wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("registration error %q, want substring %q", err, wantSub)
	}
}

// testAdapter is a pure-Go bamlutils.Adapter for the request-scoped-fact decline
// tests (Codex review finding 1). Only the getters the executor reads carry state;
// every other method is an inert no-op. It embeds context.Background so a nil-field
// adapter behaves like a plain default call.
type testAdapter struct {
	context.Context
	registry     *bamlutils.ClientRegistry
	retry        *bamlutils.RetryConfig
	rrAdvancer   bamlutils.RoundRobinAdvancer
	httpClient   *llmhttp.Client
	outputSchema *bamlutils.DynamicOutputSchema
	streamMode   bamlutils.StreamMode
	// includeReasoning is the request's reasoning opt-in. It is REAL state (not an
	// inert no-op) because the stream lane forwards it to the transport's reasoning
	// channel, so a test that could not set it could not prove the channel is gated.
	includeReasoning bool
}

func newTestAdapter() *testAdapter { return &testAdapter{Context: context.Background()} }

func (a *testAdapter) SetClientRegistry(r *bamlutils.ClientRegistry) error {
	a.registry = r
	return nil
}
func (a *testAdapter) SetTypeBuilder(*bamlutils.TypeBuilder) error { return nil }
func (a *testAdapter) SetStreamMode(m bamlutils.StreamMode)        { a.streamMode = m }
func (a *testAdapter) StreamMode() bamlutils.StreamMode            { return a.streamMode }
func (a *testAdapter) SetLogger(bamlutils.Logger)                  {}
func (a *testAdapter) Logger() bamlutils.Logger                    { return nil }
func (a *testAdapter) NewMediaFromURL(bamlutils.MediaKind, string, *string) (any, error) {
	return nil, nil
}
func (a *testAdapter) NewMediaFromBase64(bamlutils.MediaKind, string, *string) (any, error) {
	return nil, nil
}
func (a *testAdapter) SetRetryConfig(c *bamlutils.RetryConfig)                { a.retry = c }
func (a *testAdapter) RetryConfig() *bamlutils.RetryConfig                    { return a.retry }
func (a *testAdapter) SetIncludeReasoning(v bool)                             { a.includeReasoning = v }
func (a *testAdapter) IncludeReasoning() bool                                 { return a.includeReasoning }
func (a *testAdapter) SoftFinalParse() bool                                   { return false }
func (a *testAdapter) ClientRegistryProvider() string                         { return "" }
func (a *testAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry      { return a.registry }
func (a *testAdapter) HTTPClient() *llmhttp.Client                            { return a.httpClient }
func (a *testAdapter) SetHTTPClient(c *llmhttp.Client)                        { a.httpClient = c }
func (a *testAdapter) SetDeBAMLConfig(bamlutils.DeBAMLConfig)                 {}
func (a *testAdapter) DeBAMLConfig() bamlutils.DeBAMLConfig                   { return bamlutils.DeBAMLConfig{} }
func (a *testAdapter) SetDeBAMLOutputSchema(s *bamlutils.DynamicOutputSchema) { a.outputSchema = s }
func (a *testAdapter) DeBAMLOutputSchema() *bamlutils.DynamicOutputSchema     { return a.outputSchema }
func (a *testAdapter) SetRoundRobinAdvancer(adv bamlutils.RoundRobinAdvancer) { a.rrAdvancer = adv }
func (a *testAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer       { return a.rrAdvancer }

var _ bamlutils.Adapter = (*testAdapter)(nil)

// newCountingServer stands up a loopback HTTP server with a request counter. It is
// used to prove REGISTRATION declines open ZERO sockets — the constructor is pure
// (no nanollm New/Prepare), so a project pointed at this server never reaches it.
func newCountingServer(t *testing.T) (url string, count func() int) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", func() int { return int(n.Load()) }
}

// mutatedJSONProject returns a FRESH admitted JSON-alias project with mut applied — for
// registration negatives whose mutation must survive to the constructor (the source
// classifier cannot express it), e.g. a body-affecting client option, a corrupted
// envelope/version/capability record.
func mutatedJSONProject(t *testing.T, mut func(p *projectdescriptor.Project)) projectdescriptor.Project {
	t.Helper()
	p := jsonAliasProject(t)
	mut(&p)
	return p
}
