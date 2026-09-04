//go:build nanollm_integration

package main

// Shared harness for the ExecBridge-U1b native-only worker booted-command e2e.
//
// TestMain generates the deployment-specific native registry from the exact U1
// fixture (through the PRODUCTION `cmd/introspect --native-spine-descriptors` +
// `cmd/gen-native-spine-worker` commands) and builds the REAL ./cmd/worker-nativeonly
// with the same GOWORK=off, CGO_ENABLED=1, subprocess + generated-runtime tags as
// build.sh. Every test boots that binary through the real HashiCorp go-plugin
// handshake (workerplugin.Handshake) and drives it over RPC — NOT in-process.
//
// The loopback OpenAI provider's base_url is baked into the descriptor BEFORE
// generation (its port is dynamic, so it is started first), and its per-request
// behaviour is swappable so one shared provider serves the success path, the
// decline table (zero hits), and the post-claim fault (one hit).
//
// Gated by nanollm_integration so a default (no-tag) build never needs nanollm or
// a C toolchain.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/workerplugin"
)

const nativeOnlyMethod = "StaticRecursiveAliasJSON"

// generated tags mirror build.sh's native-only worker build exactly.
const nativeOnlyBuildTags = "subprocess,debamlnativespinegenerated"

var (
	// nativeOnlyBin is the built native-only worker binary (set by TestMain).
	nativeOnlyBin string
	// generatedProjectJSON is the path to the generated deployment descriptor
	// (nativegenerated/project.json) the generator emitted; its Methods are the
	// generated candidate set before the runtime classifier runs (set by TestMain).
	generatedProjectJSON string
	// providerURL is the loopback provider base_url baked into the descriptor.
	providerURL string
	// providerHits counts every request the loopback provider receives.
	providerHits atomic.Int64
	// providerConns counts every NEW TCP connection the loopback provider accepts, so a
	// stream row can prove exactly one socket as well as exactly one request.
	providerConns atomic.Int64
	// providerHandler is the swappable per-test provider behaviour.
	providerHandler atomic.Pointer[func(w http.ResponseWriter, r *http.Request)]
)

// setProviderHandler installs the per-test provider behaviour and resets the hit and
// connection counters. Returns a function reading the current hit count.
func setProviderHandler(h func(w http.ResponseWriter, r *http.Request)) func() int64 {
	providerHits.Store(0)
	providerConns.Store(0)
	providerHandler.Store(&h)
	return providerHits.Load
}

func okChatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": content}},
		},
	})
}

func TestMain(m *testing.M) {
	// One loopback provider for the whole suite; its handler is swapped per test. It is
	// started UNSTARTED so ConnState can be installed before the listener accepts: the
	// stream rows assert exactly one provider CONNECTION as well as exactly one request.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits.Add(1)
		if hp := providerHandler.Load(); hp != nil {
			(*hp)(w, r)
			return
		}
		// Default: a well-formed success, so a stray request never hangs.
		okChatCompletion(w, `{"k":1}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			providerConns.Add(1)
		}
	}
	srv.Start()
	providerURL = srv.URL + "/v1"

	code, err := func() (int, error) {
		cleanup, err := generateAndBuild()
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return 1, err
		}
		return m.Run(), nil
	}()
	srv.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "native-only e2e setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// repoPaths resolves the repo root and the nanollmprepare module root from the
// test file location.
func repoPaths() (repoRoot, moduleRoot string) {
	_, file, _, _ := runtime.Caller(0)
	// .../internal/nativebody/nanollmprepare/cmd/worker-nativeonly/e2e_support_test.go
	moduleRoot = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	repoRoot = filepath.Clean(filepath.Join(moduleRoot, "..", "..", ".."))
	return repoRoot, moduleRoot
}

// generateAndBuild runs the production introspect + generator against the U1
// fixture (base_url = the loopback), builds the native-only worker, and returns a
// cleanup that removes the generated registry (keeping the committed stub) and the
// binary.
func generateAndBuild() (func(), error) {
	repoRoot, moduleRoot := repoPaths()
	tmp, err := os.MkdirTemp("", "u1b-e2e-*")
	if err != nil {
		return nil, err
	}
	cleanupTmp := func() { _ = os.RemoveAll(tmp) }

	// Write the U1 fixture .baml corpus with the loopback base_url baked in.
	bamlSrc := filepath.Join(tmp, "baml_src")
	if err := os.MkdirAll(bamlSrc, 0o755); err != nil {
		cleanupTmp()
		return nil, err
	}
	// Two OpenAI clients: the admitted strict oracle, and one carrying a retry_policy —
	// a client shape codegen/M1 ADMITS into Project.Methods (a single OpenAI leaf) but
	// the U1 executor DECLINES at admission (the exact cohort forbids retries). This is
	// the point of the retry candidate: a non-OpenAI client would be rejected before
	// Project.Methods and never generated, so it could not prove a generate-then-decline.
	clients := fmt.Sprintf(`client<llm> JSONOracle {
  provider openai
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1-not-a-real-secret"
    base_url %q
  }
}

retry_policy U1bRetry {
  max_retries 2
  strategy { type constant_delay delay_ms 100 }
}

client<llm> RetryingOracle {
  provider openai
  retry_policy U1bRetry
  options {
    model "gpt-4o-mini"
    api_key "sk-execbridge-u1b-not-a-real-secret"
    base_url %q
  }
}
`, providerURL, providerURL)
	// Three static methods are introspected + emitted as candidates, but only
	// StaticRecursiveAliasJSON is IN the exact U1 cohort. The other two are generated
	// candidates OUTSIDE it that the codegen classifier admits into Project.Methods and
	// the RUNTIME classifier then DECLINES, so NewWorkerRuntime OMITS them at boot.
	//
	// The two axes are independent, and this corpus separates them deliberately. The
	// descriptor CLASS comes from the RETURN SHAPE alone, so StaticRecursiveAliasJSON
	// and RetryPolicyMethod are both static_stream (each returns the exact five-arm JSON
	// alias) while NonCohortStringReturn is static_unary (a plain string return).
	// RetryPolicyMethod is then declined by the CLIENT gate — its retry_policy client is
	// a cohort miss — which is why a stream-CLASS method can still be omitted at boot.
	// The in-process mirror of this classification is
	// internal/nativespine.TestBootedE2ECorpusClasses.
	functions := `function StaticRecursiveAliasJSON(topic: string) -> JSON {
  client JSONOracle
  prompt #"Return a JSON document describing {{ topic }}."#
}

function NonCohortStringReturn(topic: string) -> string {
  client JSONOracle
  prompt #"Return a string describing {{ topic }}."#
}

function RetryPolicyMethod(topic: string) -> JSON {
  client RetryingOracle
  prompt #"Return a JSON document describing {{ topic }}."#
}
`
	files := map[string]string{
		"clients.baml":   clients,
		"types.baml":     "type JSON = int | string | bool | JSON[] | map<string, JSON>\n",
		"functions.baml": functions,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(bamlSrc, name), []byte(body), 0o644); err != nil {
			cleanupTmp()
			return nil, err
		}
	}

	// Production descriptor emission.
	descPath := filepath.Join(tmp, "descriptors.json")
	if out, err := runGo(repoRoot, nil, "run", "./cmd/introspect",
		"--native-spine-descriptors", descPath, "--baml-src-dir", bamlSrc); err != nil {
		cleanupTmp()
		return nil, fmt.Errorf("introspect: %w\n%s", err, out)
	}

	// Production registry generation into the real nativegenerated tree.
	nativeGenDir := filepath.Join(moduleRoot, "nativegenerated")
	if out, err := runGo(repoRoot, nil, "run", "./cmd/gen-native-spine-worker",
		"--descriptors", descPath, "--out-dir", nativeGenDir); err != nil {
		cleanupTmp()
		return nil, fmt.Errorf("gen-native-spine-worker: %w\n%s", err, out)
	}
	cleanupGen := func() { removeGeneratedRegistry(nativeGenDir) }
	// The emitted deployment descriptor: its Methods are the generated candidate set
	// (every codegen-admitted static-unary method), before the runtime classifier runs.
	generatedProjectJSON = filepath.Join(nativeGenDir, "project.json")

	// Build the real command with the same GOWORK=off + CGO + tags as build.sh.
	// Use -mod=readonly (not build.sh's -mod=mod) because this builds IN PLACE in the
	// real module tree, and -mod=mod would rewrite the committed go.mod; build.sh
	// builds in a throwaway extracted tree where mutation is harmless.
	bin := filepath.Join(tmp, "worker-nativeonly")
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "CGO_ENABLED=1")
	if out, err := runGo(moduleRoot, env, "build", "-tags="+nativeOnlyBuildTags, "-o", bin, "./cmd/worker-nativeonly/"); err != nil {
		cleanupGen()
		cleanupTmp()
		return nil, fmt.Errorf("build worker-nativeonly: %w\n%s", err, out)
	}
	nativeOnlyBin = bin

	return func() {
		cleanupGen()
		cleanupTmp()
	}, nil
}

// removeGeneratedRegistry deletes every generated artifact, leaving only the
// committed stub — mirroring the generator's own clean-first invariant, so the
// working tree is restored byte-for-byte after the test.
func removeGeneratedRegistry(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "generated_off.go" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func runGo(dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bootedWorker is a live native-only worker subprocess dispensed over go-plugin.
type bootedWorker struct {
	worker workerplugin.Worker
	kill   func()
	stderr *syncBuffer
}

// syncBuffer is a concurrency-safe sink for the worker subprocess's stderr.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bootWorker launches the built native-only binary through the real go-plugin
// handshake — the same handshake config, protocol, and dial options the pool uses
// — and dispenses the worker. It is a REAL booted command, not an in-process
// handler.
//
// It ALWAYS hosts a real brokered shared-state store (SharedStateImpl), so the
// go-plugin dispense performs the full AttachSharedState handshake exactly as the
// pool does. A dispense that returns without error therefore
// proves the shared-state attach succeeded; the round-robin decline row further
// proves the attached advancer is live (it is non-nil only when the attach
// completed AND the request carries a request id). extraEnv is passed to the
// worker subprocess (e.g. BAML_REST_BASE_URL_REWRITES for the rewrite/proxy row).
func bootWorker(t *testing.T, extraEnv ...string) *bootedWorker {
	t.Helper()
	if nativeOnlyBin == "" {
		t.Fatal("native-only binary was not built (TestMain setup failed)")
	}
	cmd := exec.Command(nativeOnlyBin)
	cmd.Env = append(os.Environ(),
		workerplugin.Handshake.MagicCookieKey+"="+workerplugin.Handshake.MagicCookieValue,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	stderr := &syncBuffer{}
	// Host a real shared-state store so the AttachSharedState handshake runs.
	wp := &workerplugin.WorkerPlugin{
		SharedStateImpl: workerplugin.NewSharedStateServer(
			workerplugin.NewSharedStateStore(func(string) uint64 { return 0 })),
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  workerplugin.Handshake,
		Plugins:          map[string]goplugin.Plugin{"worker": wp},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		GRPCDialOptions:  workerplugin.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
		Stderr:           stderr,
		SyncStderr:       stderr,
		StartTimeout:     60 * time.Second,
	})
	var once sync.Once
	kill := func() { once.Do(client.Kill) }
	t.Cleanup(kill)

	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("go-plugin handshake: %v", err)
	}
	raw, err := rpc.Dispense("worker")
	if err != nil {
		t.Fatalf("dispense worker (AttachSharedState handshake): %v\nstderr:\n%s", err, stderr.String())
	}
	w, ok := raw.(workerplugin.Worker)
	if !ok {
		t.Fatalf("dispensed %T, not a workerplugin.Worker", raw)
	}
	return &bootedWorker{worker: w, kill: kill, stderr: stderr}
}

// admittedMethodCount waits (bounded) for the worker's startup diagnostic and
// returns the bounded admitted-method count it reports — the structurally
// observable deletion frontier. It proves candidates OUTSIDE the U1 cohort were
// omitted at boot (default-deny), not merely declined at call time.
func admittedMethodCount(t *testing.T, b *bootedWorker) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	re := regexp.MustCompile(`"admitted_method_count":(\d+)`)
	for {
		if m := re.FindStringSubmatch(b.stderr.String()); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("bad admitted_method_count %q: %v", m[1], err)
			}
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("no admitted_method_count in worker stderr:\n%s", b.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// generatedCandidateNames reads the emitted deployment descriptor and returns the names
// of every generated (codegen-admitted) candidate — the set BEFORE the runtime
// classifier runs. Paired with admittedMethodCount it distinguishes a candidate that was
// GENERATED then declined at admission from one that was never generated at all (an
// unknown-method lookup).
//
// It accepts BOTH admitted classes. Since M3e-A the exact five-arm JSON alias is stamped
// ClassStaticStream, and a static_stream method is a SUPERSET of a static_unary one — it
// is a generated candidate in both projections (Binding() for the standard composite's
// /call, StreamBinding() for the native-only runtime). Filtering to static_unary alone
// would silently drop the very method the acceptance rows serve, and the suite's own
// non-vacuity assertion would fail before it ever exercised streaming.
func generatedCandidateNames(t *testing.T) []string {
	t.Helper()
	if generatedProjectJSON == "" {
		t.Fatal("generated project.json path not set (TestMain setup failed)")
	}
	raw, err := os.ReadFile(generatedProjectJSON)
	if err != nil {
		t.Fatalf("read generated project.json: %v", err)
	}
	var proj projectdescriptor.Project
	if err := json.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("unmarshal generated project.json: %v", err)
	}
	var names []string
	for _, m := range proj.Methods {
		switch m.Class {
		case projectdescriptor.ClassStaticUnary, projectdescriptor.ClassStaticStream:
			names = append(names, m.Name)
		default:
			// A class this build does not know is not a candidate: the generator skips
			// it, so listing it here would make the generate-then-decline proof lie.
		}
	}
	return names
}

// generatedCandidateClass returns the descriptor class the generator stamped on one
// method, or "" when the method is not in the emitted descriptor.
func generatedCandidateClass(t *testing.T, method string) projectdescriptor.MethodClass {
	t.Helper()
	raw, err := os.ReadFile(generatedProjectJSON)
	if err != nil {
		t.Fatalf("read generated project.json: %v", err)
	}
	var proj projectdescriptor.Project
	if err := json.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("unmarshal generated project.json: %v", err)
	}
	for _, m := range proj.Methods {
		if m.Name == method {
			return m.Class
		}
	}
	return ""
}

// drainFinal collects stream results until the channel closes (bounded).
func drainFinal(t *testing.T, ch <-chan *workerplugin.StreamResult) []*workerplugin.StreamResult {
	t.Helper()
	var out []*workerplugin.StreamResult
	deadline := time.After(30 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("stream channel did not close within 30s after %d result(s)", len(out))
			return out
		}
	}
}

func callInput(topic string) []byte { return []byte(fmt.Sprintf(`{"topic":%q}`, topic)) }
func parseInput(raw string) []byte  { b, _ := json.Marshal(map[string]string{"raw": raw}); return b }

// --- M3e-A stream helpers ---------------------------------------------------------

// transcriptDelta is one provider content delta and the public event it must produce.
type transcriptDelta struct {
	Content string `json:"content"`
	Emit    bool   `json:"emit"`
	Partial string `json:"partial,omitempty"`
}

// transcriptFixture is the SHARED expected public stream transcript. It is OWNED and
// regenerated by internal/nativespinejsonfixture (which computes it from the root-owned
// native static-stream parsers plus the emitted decoders); this module cannot import
// root-internal packages, so it reads the committed file from the repo root. Nothing
// here computes an expectation at runtime, and nothing here imports BAML to do it.
type transcriptFixture struct {
	Deltas      []transcriptDelta `json:"deltas"`
	Final       string            `json:"final"`
	Accumulated string            `json:"accumulated"`
}

// structuredPartials returns the ordered expected partial bytes.
func (tr transcriptFixture) structuredPartials() []string {
	var out []string
	for _, d := range tr.Deltas {
		if d.Emit {
			out = append(out, d.Partial)
		}
	}
	return out
}

// loadTranscript reads the committed shared transcript fixture.
func loadTranscript(t *testing.T) transcriptFixture {
	t.Helper()
	repoRoot, _ := repoPaths()
	path := filepath.Join(repoRoot, "internal", "nativespinejsonfixture", "testdata", "stream_transcript.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared stream transcript: %v", err)
	}
	var tr transcriptFixture
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode shared stream transcript: %v", err)
	}
	if len(tr.Deltas) == 0 || tr.Final == "" {
		t.Fatal("shared stream transcript is empty")
	}
	return tr
}

// sseChunk renders one OpenAI streaming chunk carrying an assistant content delta.
func sseChunk(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	})
	return "data: " + string(b) + "\n\n"
}

// sseReasoningChunk renders a chunk carrying ONLY provider reasoning text.
func sseReasoningChunk(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": text}}},
	})
	return "data: " + string(b) + "\n\n"
}

// The noise frames every real provider interleaves. None may produce a public event.
const (
	sseRoleOnly   = `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
	sseEmptyDelta = `data: {"choices":[{"delta":{"content":""}}]}` + "\n\n"
	sseFinishOnly = `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	sseUsageOnly  = `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n"
	sseDone       = "data: [DONE]\n\n"
)

// writeSSE writes body as an event-stream, flushing after each fragment. fragment <= 0
// writes it in one piece; otherwise it is split into fragment-byte writes, which
// deliberately splits multi-byte UTF-8 runes across the wire.
func writeSSE(w http.ResponseWriter, body string, fragment int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	b := []byte(body)
	step := len(b)
	if fragment > 0 {
		step = fragment
	}
	for i := 0; i < len(b); i += step {
		end := i + step
		if end > len(b) {
			end = len(b)
		}
		_, _ = w.Write(b[i:end])
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// transcriptBody renders the shared transcript's content deltas interleaved with the
// noise frames a real provider sends, terminated with [DONE]. reasoning, when non-empty,
// is emitted as reasoning-only chunks between the content deltas.
func transcriptBody(tr transcriptFixture, reasoning []string) string {
	var b strings.Builder
	b.WriteString(sseRoleOnly)
	for i, d := range tr.Deltas {
		if i < len(reasoning) {
			b.WriteString(sseReasoningChunk(reasoning[i]))
		}
		b.WriteString(sseChunk(d.Content))
		b.WriteString(sseEmptyDelta)
	}
	b.WriteString(sseFinishOnly)
	b.WriteString(sseUsageOnly)
	b.WriteString(sseDone)
	return b.String()
}

// streamCallInput is the JSON envelope for a stream request that opts into reasoning.
func streamCallInputWithReasoning(topic string) []byte {
	b, _ := json.Marshal(map[string]any{
		"topic":            topic,
		"__baml_options__": map[string]any{"include_reasoning": true},
	})
	return b
}

// streamParseInput is the JSON envelope for a stream `/parse` of raw model text.
func streamParseInput(raw string) []byte {
	b, _ := json.Marshal(map[string]any{"raw": raw, "stream": true})
	return b
}
