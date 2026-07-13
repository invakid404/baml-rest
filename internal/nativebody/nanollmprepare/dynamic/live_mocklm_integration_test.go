//go:build integration && nanollm_integration

package dynamic

// De-BAML Slice 6c complementary realism proof: the SAME two live legs, but run
// against the real go-mocklm v0.4.0 subprocess (the second of the scope's two
// loopback responders) instead of the baml-rest capture server.
//
// Where the capture server proves byte-exact wire fidelity (repeated headers /
// per-name order, exact body bytes) and final-output parity, go-mocklm proves
// PROVIDER-NATIVE VALIDITY: both the BAML-as-served request and the native
// PreparedRequest are real OpenAI Chat Completions requests that go-mocklm (with
// MOCKLM_VALIDATE_RESPONSES) accepts on its /v1/chat/completions route and answers
// deterministically, and BOTH legs reach final structured output — one request
// each, no retry/fallback. go-mocklm records headers as a map[string]string, so
// it cannot prove repeated headers/order; that stays the capture server's job.
//
// This is a compact, self-contained subprocess harness modeled on the Slice-2
// harness in ../execute (which is package-local test code and so cannot be
// imported across the package boundary). It imports NO BAML and NO nanollm — only
// os/exec + admin HTTP — and its loopback dial guard + credential/proxy scrub keep
// every byte on 127.0.0.1.

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare/execute"
)

const (
	mockLoopbackHost    = "127.0.0.1"
	mockStartupDeadline = 20 * time.Second
	mockHealthPollEvery = 25 * time.Millisecond
	mockAdminTimeout    = 10 * time.Second
)

// liveMock is a running go-mocklm subprocess plus a loopback-guarded admin client.
type liveMock struct {
	t       *testing.T
	cmd     *exec.Cmd
	baseURL string
	admin   *http.Client
	logs    *lockedBuf
	dumped  sync.Once
	done    chan struct{}
}

// lockedBuf is a concurrency-safe buffer for the child's combined stdout+stderr.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startLiveMock launches one isolated go-mocklm on a fresh loopback port with
// provider/proxy credentials scrubbed from the child environment, and blocks
// until it is healthy.
func startLiveMock(t *testing.T) *liveMock {
	t.Helper()
	bin := liveMockBinary(t)
	port := freeLoopbackPortLive(t)

	cfgPath := filepath.Join(t.TempDir(), "mocklm.toml")
	cfg := fmt.Sprintf("[server]\nhost = %q\nport = %d\n", mockLoopbackHost, port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing mock config: %v", err)
	}

	logs := &lockedBuf{}
	cmd := exec.Command(bin)
	cmd.Env = append(scrubbedEnv(),
		"CONFIG_PATH="+cfgPath,
		"MOCKLM_HTTP_ENABLED=1",
		"MOCKLM_TLS_ENABLED=0",
		"MOCKLM_VALIDATE_RESPONSES=1",
	)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting go-mocklm (%s): %v", bin, err)
	}

	m := &liveMock{
		t:       t,
		cmd:     cmd,
		baseURL: fmt.Sprintf("http://%s:%d", mockLoopbackHost, port),
		admin:   &http.Client{Transport: mockLoopbackTransport(), Timeout: mockAdminTimeout},
		logs:    logs,
		done:    make(chan struct{}),
	}
	go func() { _ = cmd.Wait(); close(m.done) }()
	t.Cleanup(m.stop)
	m.waitHealthy()
	return m
}

// scrubbedEnv drops provider/proxy credential variables from the inherited
// environment so a helper subprocess can never resolve or forward a real secret.
func scrubbedEnv() []string {
	drop := func(k string) bool {
		up := strings.ToUpper(k)
		return strings.Contains(up, "API_KEY") || strings.Contains(up, "APIKEY") ||
			strings.Contains(up, "TOKEN") || strings.Contains(up, "SECRET") ||
			strings.HasSuffix(up, "_PROXY") || up == "HTTP_PROXY" || up == "HTTPS_PROXY" ||
			up == "ALL_PROXY" || strings.HasPrefix(up, "OPENAI") || strings.HasPrefix(up, "ANTHROPIC") ||
			strings.HasPrefix(up, "AWS_")
	}
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 && drop(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (m *liveMock) stop() {
	if m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
	<-m.done
	if m.t.Failed() {
		m.dumped.Do(func() {
			out := strings.TrimRight(m.logs.String(), "\n")
			if out == "" {
				out = "(no child output captured)"
			}
			m.t.Logf("go-mocklm child output:\n%s", out)
		})
	}
}

func (m *liveMock) exited() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

func (m *liveMock) waitHealthy() {
	m.t.Helper()
	deadline := time.Now().Add(mockStartupDeadline)
	var lastErr error
	for {
		if m.exited() {
			m.t.Fatalf("go-mocklm exited before becoming healthy (config/port failure?)")
		}
		req, _ := http.NewRequest(http.MethodGet, m.baseURL+"/health", nil)
		resp, err := m.admin.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("go-mocklm not healthy at %s within %s (last: %v)", m.baseURL, mockStartupDeadline, lastErr)
		}
		time.Sleep(mockHealthPollEvery)
	}
}

// mockLoopbackTransport disables proxying and refuses any non-loopback dial.
func mockLoopbackTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: loopbackDial}
}

func liveMockBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("MOCKLM_BINARY"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("MOCKLM_BINARY=%q is not usable: %v", bin, err)
		}
		return bin
	}
	out := filepath.Join(t.TempDir(), "go-mocklm")
	cmd := exec.Command("go", "build", "-o", out, "github.com/viktordanov/go-mocklm")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building pinned go-mocklm tool (set MOCKLM_BINARY to skip): %v\n%s", err, b)
	}
	return out
}

func freeLoopbackPortLive(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(mockLoopbackHost, "0"))
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// --- admin helpers ---

type mockExactOutput struct {
	Text         string `json:"text"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

type mockScenario struct {
	ID       string           `json:"id"`
	Provider string           `json:"provider"`
	Model    string           `json:"model,omitempty"`
	Output   *mockExactOutput `json:"output,omitempty"`
}

func (m *liveMock) adminDo(method, path string, body []byte) *http.Response {
	m.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, m.baseURL+path, r)
	if err != nil {
		m.t.Fatalf("building admin request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.admin.Do(req)
	if err != nil {
		m.t.Fatalf("admin request %s %s: %v", method, path, err)
	}
	return resp
}

func (m *liveMock) registerScenario(spec mockScenario) {
	m.t.Helper()
	body, err := stdjson.Marshal(spec)
	if err != nil {
		m.t.Fatalf("marshaling scenario %q: %v", spec.ID, err)
	}
	resp := m.adminDo(http.MethodPost, "/admin/scenarios", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("registering scenario %q: status %d: %s", spec.ID, resp.StatusCode, b)
	}
	io.Copy(io.Discard, resp.Body)
	m.t.Cleanup(func() {
		r := m.adminDo(http.MethodDelete, "/admin/scenarios/"+spec.ID, nil)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	})
}

func (m *liveMock) scenarioRequestCount(id string) int {
	m.t.Helper()
	resp := m.adminDo(http.MethodGet, "/admin/scenarios/"+id+"/request-count", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("request-count for scenario %q: status %d: %s", id, resp.StatusCode, b)
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := stdjson.NewDecoder(resp.Body).Decode(&out); err != nil {
		m.t.Fatalf("decoding request-count for %q: %v", id, err)
	}
	return out.Count
}

// TestLiveMockLMBothLegs runs both live legs against the go-mocklm subprocess and
// asserts each produces a provider-native request go-mocklm accepts and answers,
// each leg hits it exactly once, and both reach the same final structured output.
func TestLiveMockLMBothLegs(t *testing.T) {
	t.Setenv(bamlutils.EnvUseDeBAML, "1")

	m := startLiveMock(t)
	const id = "p6c-live-openai-structured"
	const content = `{"answer":"ok"}`
	m.registerScenario(mockScenario{
		ID:       id,
		Provider: "openai",
		Model:    fenceModel,
		Output:   &mockExactOutput{Text: content, OutputTokens: 7},
	})

	// The parser spy is installed only to satisfy WithDeBAMLParser; de-BAML is
	// false so it can never run.
	oracleSpy := &liveParseSpy{fn: passthroughParse}
	oracle := newLiveOracleClient(t, loopbackOracleHTTPClient(), oracleSpy)
	nano := newLiveNativeClient(t, m.baseURL)
	tc := dynFixtureByName(t, "single_user_message")

	// --- oracle leg: BAML-as-served, provider-native, exactly one request. ---
	oracleData, oerr := runOracleLeg(t, oracle, m.baseURL, tc)
	if oerr != nil {
		t.Fatalf("oracle leg failed against go-mocklm: %v", oerr)
	}
	if got := m.scenarioRequestCount(id); got != 1 {
		t.Fatalf("oracle leg produced %d go-mocklm requests, want 1", got)
	}

	// --- native leg: 6a+6b, provider-native, exactly one more request. ---
	res, spy, ran, nerr := runNativeLeg(t, nano, m.baseURL, tc)
	if !ran || nerr != nil {
		t.Fatalf("native leg failed against go-mocklm: ran=%v err=%v", ran, nerr)
	}
	if got := m.scenarioRequestCount(id); got != 2 {
		t.Fatalf("native leg brought the go-mocklm count to %d, want 2 (one per leg)", got)
	}

	// Both reach structured output; the attempted alias translated the response.
	if res.Outcome != execute.OutcomeStructured {
		t.Fatalf("native outcome = %s, want structured", res.Outcome)
	}
	if res.AttemptedAlias != fenceAlias {
		t.Errorf("native attempted alias = %q, want %q", res.AttemptedAlias, fenceAlias)
	}
	if !res.SAPInvoked || spy.calls != 1 {
		t.Errorf("native SAP: SAPInvoked=%v calls=%d, want invoked once", res.SAPInvoked, spy.calls)
	}
	assertStructuredParity(t, tc.schema, oracleData, res.Structured)

	// BAML-as-served never touched the native parser.
	if oracleSpy.calls != 0 {
		t.Errorf("oracle native-parser spy fired %d times, want 0", oracleSpy.calls)
	}
}

// passthroughParse is a never-invoked parser installed on the oracle client only
// to satisfy WithDeBAMLParser; de-BAML is false so it can never run.
func passthroughParse(ctx context.Context, req bamlutils.DeBAMLParseRequest) (bamlutils.DeBAMLParseResult, error) {
	return bamlutils.DeBAMLParseResult{}, fmt.Errorf("passthroughParse must never be called (de-BAML is false)")
}
