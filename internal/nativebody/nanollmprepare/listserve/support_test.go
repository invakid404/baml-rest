//go:build integration && nanollm_integration

// Package listserve holds the LIVE-ORACLE differentials for the required
// scalar-LIST input widening: the standard worker's /call, /stream and
// /stream-with-raw composites driven over a method whose inputs mix required
// scalars with required single-level lists of non-nullable primitives, against a
// REAL stock BAML v0.223 oracle.
//
// WHY A HERMETIC BAML RUNTIME RATHER THAN A GENERATED CLIENT. The sibling
// staticserve suite drives the checked-in generated fixture client, which has no
// list-input method whose return is the exact five-arm `JSON` alias; adding one
// means regenerating that fixture (npx BAML codegen + the ctx-first hacks + a CGO
// adapter emit), which this slice's storage-safety constraint does not permit. So
// the BAML leg here is built the way the generated wrappers themselves are built:
// `baml.CreateRuntime` over the .baml source, then the SAME CFFI entry points the
// generated `Request` / `StreamRequest` / `Parse` / `ParseStream` methods call —
// `BuildRequest` and `CallFunctionParse`, with the identical Kwargs map and the
// identical `"stream"` flag. The generated wrapper adds nothing else, so this is
// the same oracle with one less layer of codegen, not a weaker stand-in.
//
// The boundary that is NOT crossed, stated plainly: no generated adapter seam runs
// here, so the argument-map/ArgOrder plumbing a generated `/call` performs is
// supplied by the harness. That plumbing is proven separately and for exactly this
// cohort by the composed cross-product differential
// (cmd/introspect/projector_scalarlist_differential_test.go), which compiles and
// runs BOTH emitted projectors, and by the sibling staticserve suite, which crosses
// the generated seam for the scalar cohort.
package listserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/standardspineoracle"
	"github.com/invakid404/baml-rest/internal/nativespine"
	"github.com/invakid404/baml-rest/internal/nativespinelistfixture"
	"github.com/invakid404/baml-rest/nativeserve/spine"

	streamtypes "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/stream_types"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle/baml_client/types"
)

// bamlRuntimeVersion is the ONLY CFFI this differential is valid against. The
// widening's whole claim is byte parity with stock v0.223, so a differently
// versioned library would compare native against something the claim never named.
const bamlRuntimeVersion = "0.223.0"

func requireStockBAML(t *testing.T) {
	t.Helper()
	if got := baml_go.BamlVersion(); got != bamlRuntimeVersion {
		t.Fatalf("loaded BAML CFFI is %q, want stock %q — this differential is only valid against stock v0.223", got, bamlRuntimeVersion)
	}
}

// ---------------------------------------------------------------------------
// The typed cross-product
// ---------------------------------------------------------------------------

// argRow is ONE point of the cross-product, expressed exactly once and projected
// into both legs. Writing the BAML Kwargs map and the native input carrier from the
// SAME struct is what makes the plan comparison meaningful: two independently typed
// literals could drift and the differential would compare two different requests.
type argRow struct {
	name   string
	topic  string
	ratio  float64
	tags   []string
	counts []int64
	flags  []bool
}

// kwargs is the BAML argument map, in the shape the generated wrappers build.
// stream selects `StreamRequest` (true) or `Request` / final `Parse` (false).
func (r argRow) kwargs(stream bool) map[string]any {
	return map[string]any{
		"topic":  r.topic,
		"ratio":  r.ratio,
		"tags":   r.tags,
		"counts": r.counts,
		"flags":  r.flags,
		"stream": stream,
	}
}

// argOrder is the declared argument order the descriptor carries.
func argOrder() []string { return []string{"topic", "ratio", "tags", "counts", "flags"} }

// args is the generated-adapter argument map fact (what BAML was asked to bind).
func (r argRow) args() map[string]any {
	return map[string]any{
		"topic":  r.topic,
		"ratio":  r.ratio,
		"tags":   r.tags,
		"counts": r.counts,
		"flags":  r.flags,
	}
}

// input is the emitted native input carrier.
func (r argRow) input() *nativespinelistfixture.StaticListArgsJsonInput {
	return &nativespinelistfixture.StaticListArgsJsonInput{
		Topic: r.topic, Ratio: r.ratio, Tags: r.tags, Counts: r.counts, Flags: r.flags,
	}
}

// values is the PROJECTED neutral vector, produced by the emitted spine projector —
// the only host-value input the native binder accepts.
func (r argRow) values(t *testing.T) []promptdescriptor.ArgumentValue {
	t.Helper()
	v, err := nativespinelistfixture.Binding().ProjectInput(r.input())
	if err != nil {
		t.Fatalf("%s: the emitted spine projector declined the row: %v", r.name, err)
	}
	return v
}

// argRows is the cross-product the scope requires, narrowed to the ADMITTED cohort:
// the three admitted list primitives (string/int/bool) mixed with the pre-existing
// required scalars — including the scalar `float`, which is still in the cohort and
// still carries the negative-zero and extreme-magnitude cases; the empty list AND
// the nil slice (distinct Go values that must both render as an empty BAML list);
// repeated and deliberately unsorted items; unicode, quotes, backslashes, newlines,
// tabs and HTML characters; and the int64 limits.
//
// `float[]` is NOT here because it is not admitted: see the measured
// Debug-vs-Display residual in float_residual_test.go, which is the reason and the
// reopening condition. NaN/Inf are absent too — the binder declines them PRE-SEND,
// which is a serving decision covered by the decline controls, not a rendering one.
func argRows() []argRow {
	return []argRow{
		{
			name: "ordinary", topic: "weather", ratio: 2.5,
			tags: []string{"b", "a", "c"}, counts: []int64{1, -2, 3}, flags: []bool{true, false, true},
		},
		{
			name: "empty_lists", topic: "empty", ratio: 0,
			tags: []string{}, counts: []int64{}, flags: []bool{},
		},
		{
			// A required nil slice is NOT a null: both projectors turn it into an
			// empty StaticList, so stock BAML must render the same empty list it
			// renders for the explicit empty slice above.
			name: "nil_lists", topic: "nil", ratio: 0,
			tags: nil, counts: nil, flags: nil,
		},
		{
			name: "repeated_and_ordered", topic: "order", ratio: 1,
			tags: []string{"b", "a", "b", "a"}, counts: []int64{3, 1, 3}, flags: []bool{false, true, false},
		},
		{
			name:  "escaping",
			topic: "quote \" backslash \\ newline \n tab \t", ratio: 0.1,
			tags: []string{"café ☕", "<b>&amp;</b>", "line\nbreak", "back\\slash",
				"quote\"inside", " leading and trailing ", ""},
			counts: []int64{0}, flags: []bool{true},
		},
		{
			name: "int64_limits", topic: "ints", ratio: -1.25,
			tags: []string{"x"}, counts: []int64{math.MaxInt64, math.MinInt64, 0, -1}, flags: []bool{true},
		},
		{
			// The SCALAR float, which the cohort still admits: negative zero and the
			// magnitudes whose LIST rendering is the excluded residual. They belong
			// here precisely because the scalar position has no such divergence.
			name: "scalar_float_negative_zero", topic: "floats", ratio: math.Copysign(0, -1),
			tags: []string{"x"}, counts: []int64{0}, flags: []bool{false},
		},
		{
			name: "scalar_float_extremes", topic: "floats", ratio: math.MaxFloat64,
			tags: []string{"x"}, counts: []int64{0}, flags: []bool{false},
		},
	}
}

// ---------------------------------------------------------------------------
// The stock BAML leg
// ---------------------------------------------------------------------------

// bamlLeg is a stock v0.223 runtime compiled from the SAME .baml text the native
// spine project is built from, plus the environment snapshot the generated
// wrappers pass.
type bamlLeg struct {
	rt  baml.BamlRuntime
	env map[string]string
}

// newBAMLLeg compiles the scalar-list corpus (with baseURL substituted) into a stock
// runtime.
func newBAMLLeg(t *testing.T, baseURL string) *bamlLeg {
	t.Helper()
	requireStockBAML(t)
	env := envSnapshot()
	sources := nativespine.ListArgsFixtureSourcesAt(baseURL)
	rt, err := baml.CreateRuntime("./baml_src", sources, env)
	if err != nil {
		t.Fatalf("stock BAML could not compile the scalar-list corpus: %v\n%s", err, joinSources(sources))
	}
	return &bamlLeg{rt: rt, env: env}
}

func (b *bamlLeg) encode(t *testing.T, kwargs map[string]any) []byte {
	t.Helper()
	args := baml.BamlFunctionArguments{Kwargs: kwargs, Env: b.env}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatalf("encode BAML arguments: %v", err)
	}
	return encoded
}

// buildRequest is the no-send `Request.<Method>` (stream=false) or
// `StreamRequest.<Method>` (stream=true) plan, converted exactly as the generated
// adapter converts it.
func (b *bamlLeg) buildRequest(t *testing.T, r argRow, stream bool) *llmhttp.Request {
	t.Helper()
	req, err := b.rt.BuildRequest(context.Background(), nativespine.ListArgsFixtureMethod, b.encode(t, r.kwargs(stream)))
	if err != nil {
		t.Fatalf("%s: stock BAML BuildRequest(stream=%v): %v", r.name, stream, err)
	}
	return httpRequestToLLMHTTP(t, req)
}

// buildRequestFn is buildRequest as the closure the invocation carries. It rebuilds
// on every call, exactly as the generated closure does.
func (b *bamlLeg) buildRequestFn(t *testing.T, r argRow, stream bool) func(context.Context) (*llmhttp.Request, error) {
	return func(ctx context.Context) (*llmhttp.Request, error) {
		req, err := b.rt.BuildRequest(ctx, nativespine.ListArgsFixtureMethod, b.encode(t, r.kwargs(stream)))
		if err != nil {
			return nil, err
		}
		return httpRequestToLLMHTTP(t, req), nil
	}
}

// parse is BAML's FINAL `Parse.<Method>` over the complete text.
func (b *bamlLeg) parse(ctx context.Context, t *testing.T, r argRow, text string) (any, error) {
	kw := r.kwargs(false)
	kw["text"] = text
	return b.rt.CallFunctionParse(ctx, nativespine.ListArgsFixtureMethod, b.encode(t, kw))
}

// parseStream is BAML's `ParseStream.<Method>` over one accumulated prefix.
func (b *bamlLeg) parseStream(ctx context.Context, t *testing.T, r argRow, prefix string) (any, error) {
	kw := r.kwargs(true)
	kw["text"] = prefix
	return b.rt.CallFunctionParse(ctx, nativespine.ListArgsFixtureMethod, b.encode(t, kw))
}

// httpRequestToLLMHTTP mirrors, field for field, the conversion the generated
// adapter performs on BAML's HTTPRequest.
func httpRequestToLLMHTTP(t *testing.T, req baml.HTTPRequest) *llmhttp.Request {
	t.Helper()
	url, err := req.Url()
	if err != nil {
		t.Fatalf("BAML plan Url(): %v", err)
	}
	method, err := req.Method()
	if err != nil {
		t.Fatalf("BAML plan Method(): %v", err)
	}
	headers, err := req.Headers()
	if err != nil {
		t.Fatalf("BAML plan Headers(): %v", err)
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("BAML plan Body(): %v", err)
	}
	text, err := body.Text()
	if err != nil {
		t.Fatalf("BAML plan Body().Text(): %v", err)
	}
	return &llmhttp.Request{URL: url, Method: method, Headers: headers, Body: text}
}

// readBody reads the whole request body so the captured bytes can be compared with
// BAML's plan body.
//
// It RETURNS the error rather than failing: it runs on the httptest HANDLER
// goroutine, and t.Fatalf there calls FailNow, which the testing package requires to
// run on the goroutine running the test function. From the handler it would only kill
// that goroutine, leaving the response unwritten and the test failing later for an
// unrelated reason with no attribution. captured() reports the stored error from the
// test goroutine instead — a read failure still cannot be mistaken for a plan
// divergence, which is what the fatal was there for.
func readBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("read captured request body: %w", err)
	}
	return string(b), nil
}

func envSnapshot() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		env[k] = v
	}
	return env
}

func joinSources(sources map[string]string) string {
	var b strings.Builder
	for name, src := range sources {
		fmt.Fprintf(&b, "--- %s\n%s\n", name, src)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The native leg
// ---------------------------------------------------------------------------

// listProject builds the native spine project from the SAME corpus text the BAML
// runtime compiled.
func listProject(t *testing.T, baseURL string) projectdescriptor.Project {
	t.Helper()
	p, err := nativespine.BuildFromSource(nativespine.ListArgsFixtureSourcesAt(baseURL))
	if err != nil {
		t.Fatalf("BuildFromSource(scalar-list corpus): %v", err)
	}
	return p
}

// unaryComposite is the REAL standard /call composite over a REAL spine executor
// admitting exactly the scalar-list method.
func unaryComposite(t *testing.T, baseURL string) (bamlutils.NativeStaticServeFunc, *spine.UnaryExecutor) {
	t.Helper()
	exec, err := spine.NewPopulationExecutor(listProject(t, baseURL), []spine.UnaryRegistration{{
		Binding:     nativespinelistfixture.Binding(),
		BuildMethod: nativespinelistfixture.BuildMethod,
	}}, nil)
	if err != nil {
		t.Fatalf("NewPopulationExecutor: %v", err)
	}
	if got := exec.Methods(); len(got) != 1 || got[0] != nativespine.ListArgsFixtureMethod {
		t.Fatalf("the standard /call population is %v, want exactly [%s] — a differential over an EMPTY population proves nothing",
			got, nativespine.ListArgsFixtureMethod)
	}
	serve, err := standardspineoracle.NewStaticServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticServeFromExecutor: %v", err)
	}
	return serve, exec
}

// streamComposite is the REAL standard /stream + /stream-with-raw composite over a
// REAL spine stream executor.
func streamComposite(t *testing.T, baseURL string) (bamlutils.NativeStaticStreamOracleServeFunc, *spine.StreamExecutor) {
	t.Helper()
	exec, err := spine.NewPopulationStreamExecutor(listProject(t, baseURL), []spine.StreamRegistration{{
		Binding:     nativespinelistfixture.StreamBinding(),
		BuildMethod: nativespinelistfixture.BuildMethod,
	}}, nil)
	if err != nil {
		t.Fatalf("NewPopulationStreamExecutor: %v", err)
	}
	if got := exec.Methods(); len(got) != 1 || got[0] != nativespine.ListArgsFixtureMethod {
		t.Fatalf("the standard stream population is %v, want exactly [%s]", got, nativespine.ListArgsFixtureMethod)
	}
	serve, err := standardspineoracle.NewStaticStreamServeFromExecutor(prometheus.NewRegistry(), exec)
	if err != nil {
		t.Fatalf("NewStaticStreamServeFromExecutor: %v", err)
	}
	return serve, exec
}

// ---------------------------------------------------------------------------
// Loopback providers
// ---------------------------------------------------------------------------

// captureServer records the exact bytes of every request it accepts.
type captureServer struct {
	srv   *httptest.Server
	hits  atomic.Int64
	mu    sync.Mutex
	last  capturedRequest
	first bool
	// readErr is the first body-read failure the handler goroutine saw. It is
	// checked on the TEST goroutine in two places, for two different caller sets:
	// the registered cleanup (every server, unconditionally) and captured() (the
	// callers that compare bytes, where the earlier failure attributes better).
	// readErrReported latches once either of them has reported it, so one failure
	// is not restated twice.
	readErr         error
	readErrReported bool
}

type capturedRequest struct {
	method  string
	url     string
	headers map[string]string
	body    string
}

// newJSONServer answers every request with an OpenAI chat completion whose
// assistant content is content.
func newJSONServer(t *testing.T, content string) *captureServer {
	t.Helper()
	return startCaptureServer(t, func(cs *captureServer, w http.ResponseWriter, r *http.Request) {
		cs.record(r)
		env, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(env)
	})
}

// newSSEServer replays events as an SSE stream.
func newSSEServer(t *testing.T, events []string) *captureServer {
	t.Helper()
	return startCaptureServer(t, func(cs *captureServer, w http.ResponseWriter, r *http.Request) {
		cs.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
			if fl != nil {
				fl.Flush()
			}
		}
	})
}

// startCaptureServer is the ONE construction site for a capture server, and the ONE
// place its cleanup is registered. Both matter.
//
// The cleanup closes the server — httptest.Server.Close blocks until every in-flight
// handler has returned, which is the happens-before that makes reading readErr here
// race-free — and then validates readErr on the TEST goroutine.
//
// That validation is UNCONDITIONAL, and that is the point. An earlier arrangement
// checked readErr only inside captured(); the streaming tests never call captured(),
// so a body-read failure there was silently ignored — strictly weaker than the
// t.Fatalf-from-the-handler it replaced. Every server now gets the check whether its
// test compares bytes or not.
//
// A zero-request negative control stays valid: readErr can only be set inside
// record(), so no request means no error to report.
//
// t.Errorf rather than t.Fatalf: a cleanup runs on the test goroutine, but FailNow
// there would Goexit mid-cleanup and skip the remaining cleanups. Marking the failure
// is enough — the test body has already finished.
func startCaptureServer(t *testing.T, handle func(*captureServer, http.ResponseWriter, *http.Request)) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(cs, w, r)
	}))
	t.Cleanup(func() {
		cs.srv.Close()
		cs.reportReadErr(t.Errorf)
	})
	return cs
}

// reportReadErr calls fail with the stored body-read failure, if any, AT MOST ONCE.
//
// fail is a parameter rather than a *testing.T so the cleanup can pass t.Errorf while
// TestCaptureServerReportsAReadFailureWithoutCaptured can pass a recorder — which is
// how this net is proven to fire without failing the test that proves it.
//
// The at-most-once latch is why captured() does not simply CLEAR readErr before its
// own t.Fatalf. Clearing would work — Fatalf's Goexit runs the cleanup, which would
// then find nothing — but it would make the cleanup's unconditional net depend on
// captured() having run first, which is precisely the conditional-check arrangement
// that left this package's streaming tests blind to a read failure. The error stays
// stored; only the reporting is deduplicated.
func (cs *captureServer) reportReadErr(fail func(format string, args ...any)) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.readErr != nil && !cs.readErrReported {
		cs.readErrReported = true
		fail("the capture server could not read a request body: %v — a truncated or empty "+
			"captured body would otherwise be compared against BAML's plan and reported as a "+
			"plan divergence that never happened", cs.readErr)
	}
}

func (cs *captureServer) record(r *http.Request) {
	cs.hits.Add(1)
	body, err := readBody(r)
	hdr := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			hdr[strings.ToLower(k)] = v[0]
		}
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err != nil && cs.readErr == nil {
		cs.readErr = err
	}
	cs.last = capturedRequest{method: r.Method, url: r.URL.String(), headers: hdr, body: body}
	cs.first = true
}

func (cs *captureServer) captured(t *testing.T) capturedRequest {
	t.Helper()
	// Report a read failure FIRST, and fatally: this caller goes on to compare these
	// bytes against BAML's plan, where an empty captured body would surface as a plan
	// divergence that never happened. The registered cleanup checks the same thing
	// unconditionally for every server; the latch inside reportReadErr keeps the one
	// failure from being stated twice when Fatalf's Goexit runs that cleanup.
	cs.reportReadErr(t.Fatalf)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.first {
		t.Fatal("no request reached the provider; there is nothing to compare")
	}
	return cs.last
}

func (cs *captureServer) baseURL() string { return cs.srv.URL + "/v1" }

// ---------------------------------------------------------------------------
// OpenAI SSE builders (the same shapes the sibling staticserve corpus uses)
// ---------------------------------------------------------------------------

func openAIChunk(delta, finish string) string {
	fin := "null"
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, fin)
}

func openAIUsageChunk() string {
	return `{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`
}

// contentSSE fragments content across several deltas so the per-prefix oracle sees
// several structured ticks — including prefixes that are not yet parseable. A
// single-frame corpus cannot observe per-tick behaviour at all.
func contentSSE(contentChunks, reasoningChunks []string) []string {
	events := []string{openAIChunk(`{"role":"assistant"}`, "")}
	for _, rc := range reasoningChunks {
		events = append(events, openAIChunk(`{"reasoning_content":`+jsonString(rc)+`}`, ""))
	}
	for _, cc := range contentChunks {
		events = append(events, openAIChunk(`{"content":`+jsonString(cc)+`}`, ""))
	}
	events = append(events, openAIUsageChunk(), openAIChunk(`{}`, "stop"), "[DONE]")
	return events
}

// listStreamCorpus is the shared fragmented answer. It is an ARRAY of scalars
// deliberately: the return family is UNCHANGED by this slice, so the corpus is the
// one the exact-JSON cohort already proves, and the variable under test stays the
// INPUT.
func listStreamCorpus() []string {
	return contentSSE([]string{"[1,", `"x",`, "true]"}, nil)
}

const listStreamFinal = `[1,"x",true]`

// listReasoningDeltas is the reasoning corpus, and listReasoningFull the complete
// text they concatenate to. Both are named so the stream tests assert the WHOLE
// accumulated channel rather than its non-emptiness.
var listReasoningDeltas = []string{"thinking ", "harder"}

const listReasoningFull = "thinking harder"

// jsonString quotes s as a JSON string.
//
// json.Marshal rather than %q on purpose: Go's %q is strconv.Quote, which emits
// \a, \v and \xNN escapes that no JSON parser accepts. Today's corpora carry no
// such byte, but a helper that silently produces invalid JSON for one would make the
// oracle misparse the frame rather than fail, so the escaping is done by the JSON
// encoder that owns it.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Unreachable: encoding a Go string cannot fail. Panicking beats returning
		// a malformed frame that the oracle would then misattribute.
		panic("listserve: marshal SSE chunk string: " + err.Error())
	}
	return string(b)
}

// jsonOf marshals a public value the way a client would receive it.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal public value: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// The stock decode registration
// ---------------------------------------------------------------------------

// TestMain registers the stock BAML type map for the exact five-arm `JSON` alias
// before any parse runs.
//
// It is REQUIRED, not decoration: baml_go's CFFI decode callback resolves a named
// alias through the global type map and PANICS on the callback thread with
// "type alias not found: TYPES.JSON" when it is absent — a dead process with no
// attribution rather than a test failure. A generated client registers this map in
// its own init; this package deliberately links no generated client, so it registers
// the same four entries itself.
//
// The carriers are the REAL generated ones from the static_oracle fixture, whose
// `JSON` alias is the identical five-arm shape, so the value BAML hands the oracle
// is the same carrier a generated standard method would hand it — including its
// MarshalJSON, which is what the public-byte comparison reads. Substituting a
// hand-written stand-in here would compare native against a carrier production never
// uses.
func TestMain(m *testing.M) {
	baml.SetTypeMap(map[string]reflect.Type{
		"TYPES.JSON":        reflect.TypeOf(types.Union5BoolOrIntOrListJSONOrMapStringKeyJSONValueOrString{}),
		"STREAM_TYPES.JSON": reflect.TypeOf((*streamtypes.Union5BoolOrIntOrListJSONOrMapStringKeyJSONValueOrString)(nil)),
		// The union-VARIANT name the CFFI uses for the same alias (arms sorted), which
		// decodeUnionValue resolves separately from the alias name.
		"TYPES.List__JSON__Map__string_JSON__bool__int__string": reflect.TypeOf(
			types.Union5BoolOrIntOrListJSONOrMapStringKeyJSONValueOrString{}),
		"STREAM_TYPES.List__JSON__Map__string_JSON__bool__int__string": reflect.TypeOf(
			streamtypes.Union5BoolOrIntOrListJSONOrMapStringKeyJSONValueOrString{}),
	})
	os.Exit(m.Run())
}

// TestCaptureServerReportsAReadFailureWithoutCaptured covers the caller set the
// earlier arrangement lost: a test that drives the provider but never calls
// captured() — which is every streaming test in this package.
//
// It deliberately does NOT call captured(). It stores a read failure the way record()
// would, then drives the same reportReadErr the registered cleanup drives, through a
// recorder rather than t.Errorf, and requires it to fire. Passing the failure function
// in is what lets the net be exercised without the exercise itself failing.
func TestCaptureServerReportsAReadFailureWithoutCaptured(t *testing.T) {
	cs := newJSONServer(t, `[1]`)

	// CONTROL: a server that read every body cleanly reports nothing, so the
	// assertion below is about the stored error and not about reportReadErr always
	// firing. This also covers the zero-request negative controls, which have no
	// request and therefore no readErr.
	var clean []string
	cs.reportReadErr(func(format string, args ...any) { clean = append(clean, fmt.Sprintf(format, args...)) })
	if len(clean) != 0 {
		t.Fatalf("a clean capture server reported %d read failure(s): %v", len(clean), clean)
	}

	cs.mu.Lock()
	cs.readErr = errors.New("synthetic body-read failure")
	cs.mu.Unlock()

	var reported []string
	record := func(format string, args ...any) { reported = append(reported, fmt.Sprintf(format, args...)) }
	cs.reportReadErr(record)
	if len(reported) != 1 {
		t.Fatalf("a stored body-read failure was reported %d time(s), want exactly 1 — a caller that never "+
			"invokes captured() would otherwise ignore it, which is the regression this test exists for", len(reported))
	}
	if !strings.Contains(reported[0], "synthetic body-read failure") {
		t.Errorf("the report does not carry the underlying error: %q", reported[0])
	}

	// AT MOST ONCE. captured() reports fatally and Fatalf's Goexit then runs the
	// registered cleanup, which reports again through the same function — so without
	// the latch one read failure would be stated twice. Driving the second call here
	// is also what leaves the planted error unreported by this test's own cleanup.
	cs.reportReadErr(record)
	if len(reported) != 1 {
		t.Fatalf("the same body-read failure was reported %d time(s), want exactly 1", len(reported))
	}

	// The error itself is still STORED: deduplicating the report must not swallow it,
	// or the cleanup's unconditional net would depend on captured() having run.
	cs.mu.Lock()
	stored := cs.readErr
	cs.mu.Unlock()
	if stored == nil {
		t.Error("reporting the failure cleared it; the stored error must survive so nothing depends on report order")
	}
}
