//go:build nanollm_integration

package execute

// De-BAML Slice 2a harness: launch a go-mocklm v0.4.0 subprocess bound to a
// loopback port, and drive it entirely over admin HTTP. go-mocklm is a
// `package main` (not an embeddable library), so it is pinned as a Go TOOL in
// this module's go.mod and launched as a child process here — never linked into
// the test binary.
//
// This file is the subprocess/admin/loopback plumbing ONLY: config + spawn +
// health wait + kill/wait cleanup, a loopback-only http transport with a dial
// guard, and scenario register/capture/counter helpers over the admin API. It
// imports NO nanollm and NO BAML runtime; the functional matrix lives in
// send_integration_test.go. Tests are SERIAL (no t.Parallel): each top-level
// test starts its own isolated mock process, so the global /admin/requests log
// and per-scenario counters are never shared across tests.

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const (
	// loopbackHost is the ONLY host the mock binds to and the ONLY host the
	// test transport is allowed to dial. A literal 127.0.0.1 (not "localhost")
	// keeps the dial guard a simple, resolution-free check.
	loopbackHost = "127.0.0.1"

	// startupDeadline bounds how long we wait for GET /health to report 200
	// after spawning the subprocess (the close-then-spawn port handoff plus
	// process start).
	startupDeadline = 20 * time.Second

	// healthPollInterval is the gap between /health probes during startup.
	healthPollInterval = 25 * time.Millisecond

	// adminTimeout bounds admin/health HTTP calls so a wedged child cannot hang
	// the suite. The functional Do/DoStream clients deliberately use no client
	// timeout (they rely on the context deadline instead).
	adminTimeout = 10 * time.Second
)

// mockServer is a running go-mocklm subprocess plus the loopback-guarded admin
// client used to register scenarios and read back capture/counter state.
type mockServer struct {
	t       *testing.T
	cmd     *exec.Cmd
	baseURL string // http://127.0.0.1:<port>
	port    int

	admin *http.Client

	logs        *lockedBuffer // combined child stdout+stderr
	logDumpOnce sync.Once

	done chan struct{} // closed when the child's Wait returns
}

// startMock launches one isolated go-mocklm subprocess on a fresh loopback port
// and blocks until it is healthy. Cleanup (kill + wait, and a child-log dump on
// failure) is registered on t.
func startMock(t *testing.T) *mockServer {
	t.Helper()

	bin := mocklmBinary(t)
	port := freeLoopbackPort(t)

	cfgPath := filepath.Join(t.TempDir(), "mocklm.toml")
	// Minimal config: bind the clear-text lane to a loopback literal on the
	// reserved port. Provider defaults (tokens, error_status, ...) are fine;
	// scenarios override content per test. Response validation is forced on via
	// MOCKLM_VALIDATE_RESPONSES below.
	cfg := fmt.Sprintf("[server]\nhost = %q\nport = %d\n", loopbackHost, port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing mock config: %v", err)
	}

	logs := &lockedBuffer{}
	cmd := exec.Command(bin)
	// go-mocklm reads only these four variables; pass a scoped env (NOT the test
	// process's full os.Environ()) so no ambient/inherited variable can influence
	// the loopback mock's behavior.
	cmd.Env = []string{
		"CONFIG_PATH=" + cfgPath,
		"MOCKLM_HTTP_ENABLED=1",
		"MOCKLM_TLS_ENABLED=0",
		"MOCKLM_VALIDATE_RESPONSES=1",
	}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting go-mocklm (%s): %v", bin, err)
	}

	m := &mockServer{
		t:       t,
		cmd:     cmd,
		baseURL: fmt.Sprintf("http://%s:%d", loopbackHost, port),
		port:    port,
		admin:   &http.Client{Transport: loopbackTransport(), Timeout: adminTimeout},
		logs:    logs,
		done:    make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()
	t.Cleanup(m.stop)

	m.waitHealthy()
	return m
}

// stop kills the child, waits for it to reap, and dumps its captured output when
// the test has failed (only on failure, to keep green runs quiet).
func (m *mockServer) stop() {
	if m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
	<-m.done
	if m.t.Failed() {
		m.dumpLogs()
	}
}

// exited reports whether the child has already terminated (e.g. a bad config or
// a lost port race makes go-mocklm log.Fatalf and exit before serving).
func (m *mockServer) exited() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// dumpLogs surfaces a bounded FACT about the child's combined stdout+stderr
// exactly once — NEVER the raw text. go-mocklm forces MOCKLM_VALIDATE_RESPONSES=1,
// and its response validator logs `body: <raw>` on a validation failure, so the
// captured child log can contain a provider response body verbatim. Printing only
// a line count + a SHA-256 digest keeps the failure log secret-clean while still
// signalling that output exists (set MOCKLM_BINARY and run the mock manually to
// inspect the raw logs when debugging).
func (m *mockServer) dumpLogs() {
	m.logDumpOnce.Do(func() {
		s := m.logs.String()
		trimmed := strings.TrimRight(s, "\n")
		lines := 0
		if trimmed != "" {
			lines = strings.Count(trimmed, "\n") + 1
		}
		m.t.Logf("go-mocklm child output suppressed for secret-hygiene (may contain a response body): %d line(s), %s", lines, bodyDigest([]byte(s)))
	})
}

// waitHealthy polls GET /health until 200 within startupDeadline, failing fast
// if the child dies first.
func (m *mockServer) waitHealthy() {
	m.t.Helper()
	deadline := time.Now().Add(startupDeadline)
	var lastErr error
	for {
		if m.exited() {
			m.dumpLogs()
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
			m.dumpLogs()
			m.t.Fatalf("go-mocklm not healthy on loopback port %d within %s (last: %v)", m.port, startupDeadline, lastErr)
		}
		time.Sleep(healthPollInterval)
	}
}

// --- loopback-only transport ---

// loopbackTransport builds an http.Transport with proxying disabled and a dial
// guard that refuses any non-loopback destination. A mis-typed BaseURL — or any
// accidental attempt to reach a public provider — fails here instead of leaving
// the machine. Both the functional Do/DoStream clients and the admin client use
// this transport.
func loopbackTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("loopback guard: unparsable dial address %q: %w", addr, err)
			}
			if !isLoopbackHost(host) {
				return nil, fmt.Errorf("loopback guard: refusing non-loopback dial to %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
	}
}

// loopbackClient returns a fresh http.Client on the loopback-guarded transport
// with NO client timeout — the functional cases bound their calls with a context
// deadline instead. Each Do/DoStream call receives one via nanollm.WithHTTPClient.
func loopbackClient() *http.Client {
	return &http.Client{Transport: loopbackTransport()}
}

// isLoopbackHost reports whether host is a loopback literal (or "localhost").
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- admin / scenario helpers ---

// chunking mirrors go-mocklm's scenario Output.Chunking (mode: whole|runes|words).
type chunking struct {
	Mode string `json:"mode,omitempty"`
	Size int    `json:"size,omitempty"`
}

// exactOutput mirrors go-mocklm's scenario ExactOutput: verbatim assistant text,
// its stream chunking, and pinned output-token usage.
type exactOutput struct {
	Text         string    `json:"text"`
	Chunking     *chunking `json:"chunking,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
}

// scenarioSpec is the subset of go-mocklm's POST /admin/scenarios body this
// suite registers: an id, the provider + model match keys, the exact output,
// and an optional raw provider-config fragment (e.g. {"strict":true}).
type scenarioSpec struct {
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	Model    string          `json:"model,omitempty"`
	Output   *exactOutput    `json:"output,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// capturedRequest is the per-scenario last-matched request: the byte-exact
// request body plus the method/path go-mocklm reports via response headers.
type capturedRequest struct {
	Method string
	Path   string
	Body   []byte
}

// recordedRequest mirrors one entry of GET /admin/requests — the global request
// log carrying provider/path and the (canonicalized) request headers, used for
// the auth-header assertions.
type recordedRequest struct {
	Provider string            `json:"provider"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
	Body     json.RawMessage   `json:"body"`
}

// registerScenario publishes a scenario and schedules its deletion at test end
// so serial tests never collide on a (provider, model) key.
func (m *mockServer) registerScenario(spec scenarioSpec) {
	m.t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		m.t.Fatalf("marshaling scenario %q: %v", spec.ID, err)
	}
	resp := m.adminDo(http.MethodPost, "/admin/scenarios", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("registering scenario %q: status %d: %s", spec.ID, resp.StatusCode, bodyDigest(b))
	}
	io.Copy(io.Discard, resp.Body)
	m.t.Cleanup(func() {
		r := m.adminDo(http.MethodDelete, "/admin/scenarios/"+spec.ID, nil)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	})
}

// lastRequest returns the byte-exact body and method/path of the last request
// that matched the scenario (GET /admin/scenarios/{id}/last-request).
func (m *mockServer) lastRequest(id string) capturedRequest {
	m.t.Helper()
	resp := m.adminDo(http.MethodGet, "/admin/scenarios/"+id+"/last-request", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("last-request for scenario %q: status %d: %s", id, resp.StatusCode, bodyDigest(b))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		m.t.Fatalf("reading last-request body for %q: %v", id, err)
	}
	return capturedRequest{
		Method: resp.Header.Get("X-MockLM-Captured-Method"),
		Path:   resp.Header.Get("X-MockLM-Captured-Path"),
		Body:   body,
	}
}

// scenarioRequestCount is the scenario's matched-request count
// (GET /admin/scenarios/{id}/request-count).
func (m *mockServer) scenarioRequestCount(id string) int {
	return m.scenarioCounter(id, "request-count")
}

// scenarioAttemptCount is the scenario's fault-attempt sequence position
// (GET /admin/scenarios/{id}/attempt-count) — the per-scenario retry oracle.
func (m *mockServer) scenarioAttemptCount(id string) int {
	return m.scenarioCounter(id, "attempt-count")
}

func (m *mockServer) scenarioCounter(id, kind string) int {
	m.t.Helper()
	resp := m.adminDo(http.MethodGet, "/admin/scenarios/"+id+"/"+kind, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("%s for scenario %q: status %d: %s", kind, id, resp.StatusCode, bodyDigest(b))
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		m.t.Fatalf("decoding %s for scenario %q: %v", kind, id, err)
	}
	return out.Count
}

// recordedRequests returns the global request log (GET /admin/requests).
func (m *mockServer) recordedRequests() []recordedRequest {
	m.t.Helper()
	resp := m.adminDo(http.MethodGet, "/admin/requests", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("admin/requests: status %d: %s", resp.StatusCode, bodyDigest(b))
	}
	var out struct {
		Requests []recordedRequest `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		m.t.Fatalf("decoding admin/requests: %v", err)
	}
	return out.Requests
}

// recordedFor returns the single recorded request for the given provider+path,
// failing if there is not exactly one (the auth assertions want an unambiguous
// hit; serial per-test isolation guarantees a clean log).
func (m *mockServer) recordedFor(provider, path string) recordedRequest {
	m.t.Helper()
	var hits []recordedRequest
	for _, rec := range m.recordedRequests() {
		if rec.Provider == provider && rec.Path == path {
			hits = append(hits, rec)
		}
	}
	if len(hits) != 1 {
		m.t.Fatalf("want exactly 1 recorded %s request to %s, got %d", provider, path, len(hits))
	}
	return hits[0]
}

func (m *mockServer) adminDo(method, path string, body []byte) *http.Response {
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

// headerValue does a case-insensitive lookup over a recorded request's headers;
// go-mocklm canonicalizes header names, but the assertion must not depend on it.
func headerValue(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// --- binary resolution + port reservation ---

// mocklmBinary resolves the go-mocklm executable. CI provides the tool-built
// binary via MOCKLM_BINARY (authoritative); locally, if unset, build the pinned
// tool on demand from this module's go.mod (GOWORK=off).
func mocklmBinary(t *testing.T) string {
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

// freeLoopbackPort reserves a free loopback TCP port and releases it for the
// child to bind. There is a small close-then-spawn window; serial tests plus the
// health wait absorb it (per the scope's subprocess note).
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// lockedBuffer is a concurrency-safe io.Writer for capturing child stdout+stderr
// (exec copies each stream from its own goroutine).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
