//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
	"github.com/testcontainers/testcontainers-go"
)

// TestEnv holds the shared test environment
var TestEnv *testutil.TestEnvironment

// MockClient is the client for registering scenarios
var MockClient *mockllm.Client

// BAMLClient is the client for calling baml-rest (Fiber server)
var BAMLClient *testutil.BAMLRestClient

// UnaryClient is the client for calling the unary server (chi/net-http).
// nil when the unary server is not enabled.
var UnaryClient *testutil.BAMLRestClient

// BAMLVersion is the version being tested (set at init time)
var BAMLVersion string

// BAMLSourcePath is the path to a local BAML source repo (set via BAML_SOURCE env var)
var BAMLSourcePath string

// PrebuiltCffiDir is the path to a directory holding a prebuilt libbaml_cffi.so
// + baml-cli (set via BAML_PREBUILT_CFFI_DIR). When set (CI only), the Docker
// build injects these artifacts and skips the in-image cargo build. Unset for
// local runs, which keep building the cffi lib from source.
var PrebuiltCffiDir string

// UseBuildRequest is true when the test container runs the BuildRequest path.
// Set in TestMain from the BAML_REST_USE_BUILD_REQUEST env var.
var UseBuildRequest bool

// Matrix-derived setup inputs populated by TestMain before m.Run. Dedicated
// tests read these via matrixSetupOptions so a new axis added to TestMain
// propagates without each callsite needing to be updated.
var (
	bamlSrcPath    string
	adapterVersion string
	unaryServer    bool
)

// matrixSetupOptions returns SetupOptions pre-populated from the matrix
// axes (build mode, unary server, build-request, BAML version) so dedicated
// envs inherit them by default; tests override only what they pin.
func matrixSetupOptions() testutil.SetupOptions {
	opts := testutil.SetupOptions{
		BAMLSrcPath:     bamlSrcPath,
		BAMLVersion:     BAMLVersion,
		AdapterVersion:  adapterVersion,
		BAMLSource:      BAMLSourcePath,
		PrebuiltCffiDir: PrebuiltCffiDir,
		UnaryServer:     unaryServer,
		UseBuildRequest: UseBuildRequest,
		InProcess:       inProcessBuild,
	}
	// Forward the host BAML_REST_HTTP_CLIENT selector (the CI `http-client`
	// matrix axis) into the container env so it actually reaches cmd/serve and
	// the subprocess cmd/worker. Read here — the shared setup path for both the
	// versioned and baml-source jobs — so the axis exercises net/http vs
	// fasthttp instead of silently running every arm as auto. No-op when unset.
	opts.ForwardHostHTTPClientSelector()
	return opts
}

// ActuallyBuildRequest reports whether a request is genuinely routed through
// the BuildRequest orchestrator given both the runtime env gate and the BAML
// runtime version. BuildRequest requires the Request / StreamRequest APIs
// which only exist from BAML 0.219.0; older runtimes fall through to the
// legacy path even when BAML_REST_USE_BUILD_REQUEST=true.
func ActuallyBuildRequest() bool {
	return UseBuildRequest && bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0")
}

// skipIfInProcess short-circuits a test that depends on subprocess-only
// semantics — chiefly OS process death, signal delivery, and gRPC
// Unavailable on worker kill — when the binary was built without the
// `subprocess` tag. Reason is included in the t.Skipf message so logs
// make the skip cause obvious.
func skipIfInProcess(t *testing.T, reason string) {
	t.Helper()
	if inProcessBuild {
		t.Skipf("in-process build: %s", reason)
	}
}

// parseBoolEnv parses a boolean environment variable using the same accepted
// literals as the server's UseBuildRequest parser: 1/true/yes/on → true,
// everything else (including empty) → false.
func parseBoolEnv(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func init() {
	BAMLSourcePath = os.Getenv("BAML_SOURCE")
	PrebuiltCffiDir = os.Getenv("BAML_PREBUILT_CFFI_DIR")
	BAMLVersion = getBAMLVersion()
}

// getBAMLVersion returns the BAML version to test.
// Priority: BAML_VERSION env var > BAML source Cargo.toml > baml_versions.json "latest" field
func getBAMLVersion() string {
	if v := os.Getenv("BAML_VERSION"); v != "" {
		return v
	}

	if BAMLSourcePath != "" {
		v, err := testutil.DetectBamlSourceVersion(BAMLSourcePath)
		if err != nil {
			panic(fmt.Sprintf("BAML_SOURCE set but failed to detect version: %v", err))
		}
		return v
	}

	// Fall back to reading from baml_versions.json
	paths := []string{
		"integration/baml_versions.json",
		"baml_versions.json",
		"../baml_versions.json",
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		var versions struct {
			Latest string `json:"latest"`
		}
		if err := sonic.Unmarshal(data, &versions); err != nil {
			continue
		}
		if versions.Latest != "" {
			return versions.Latest
		}
	}

	panic("BAML version not found: set BAML_VERSION env var or ensure integration/baml_versions.json exists")
}

// GoroutineLeakFilter contains comma-separated patterns for detecting goroutine leaks
// in our code. Used by leak detection tests to filter pprof data. Case-insensitive.
// Covers: baml-rest (github.com/invakid404/baml-rest) and BAML (github.com/boundaryml/baml)
// Excludes known background goroutines:
//   - StartRSSMonitor: RSS memory monitoring goroutine (runs for process lifetime)
//   - healthChecker: Pool health check goroutine (runs for pool lifetime)
//   - GetGoroutines: The goroutine running the pprof capture itself (self-capture)
//   - acceptandserve: go-plugin broker goroutines that can outlive canceled requests
const GoroutineLeakFilter = "invakid404/baml-rest,boundaryml/baml,-StartRSSMonitor,-healthChecker,-GetGoroutines,-acceptandserve"

// defaultSuiteWatchdogTimeout returns the watchdog budget to use when
// BAML_REST_SUITE_WATCHDOG is unset. The watchdog guards the window Go's
// `go test -timeout` provably does NOT cover — everything before/after
// m.Run (setup, which now retries with backoff per #415, and teardown,
// the actual #420 stall). The testing package arms its alarm inside
// M.Run and stops it before Run returns, so a stall in TestMain teardown
// has no `-timeout` guard at all; this watchdog is that guard.
//
// In CI the budget is set explicitly per job (see the BAML_REST_SUITE_WATCHDOG
// env in .github/workflows/integration-tests.yml), anchored to each job's
// step-level `timeout-minutes` minus a 3m margin. This default only
// applies to local / unset runs, and is biased by mode so a healthy
// local run never false-trips: baml-source mode (BAML_SOURCE set) builds
// BAML from source and has a 38-45m healthy m.Run window, so it needs the
// larger 57m budget; the lighter modes finish well under 45m and use 42m.
func defaultSuiteWatchdogTimeout() time.Duration {
	if os.Getenv("BAML_SOURCE") != "" {
		return 57 * time.Minute
	}
	return 42 * time.Minute
}

// suiteWatchdogTimeout resolves the watchdog budget from the
// BAML_REST_SUITE_WATCHDOG env var (any time.ParseDuration value),
// defaulting to defaultSuiteWatchdogTimeout(). "0"/"off" (or a parsed 0)
// disables the watchdog entirely (returns 0).
func suiteWatchdogTimeout() time.Duration {
	def := defaultSuiteWatchdogTimeout()
	raw := strings.TrimSpace(os.Getenv("BAML_REST_SUITE_WATCHDOG"))
	switch strings.ToLower(raw) {
	case "":
		return def
	case "0", "off", "disable", "disabled":
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid BAML_REST_SUITE_WATCHDOG %q: %v; using %s\n", raw, err, def)
		return def
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "negative BAML_REST_SUITE_WATCHDOG %q (parsed %s); using %s\n", raw, d, def)
		return def
	}
	// A parsed 0 (e.g. "0s") passes through and disables the watchdog,
	// consistent with the "0"/"off" literals above.
	return d
}

// startSuiteWatchdog launches a background timer that, if the whole
// TestMain (setup + m.Run + teardown) has not finished within timeout,
// dumps all Go goroutine stacks (this test process) plus best-effort
// worker goroutine and native (OS-thread) stacks pulled from inside the
// container via the /_debug endpoints, then force-exits non-zero.
//
// It is intentionally fire-and-forget and is NEVER cancelled: the
// process exits (via os.Exit at the end of TestMain) on a clean run
// before the timer fires, while on a hang the timer must still be live
// during teardown — the post-m.Run window Go's `-timeout` does not cover
// (#420). Cancelling it when m.Run returns (an earlier mistake) would
// blind exactly the window we need to watch.
func startSuiteWatchdog(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	go func() {
		<-time.After(timeout)
		fmt.Fprintf(os.Stderr, "\n=== SUITE WATCHDOG: TestMain exceeded %s without finishing (baml-rest #420) ===\n", timeout)
		fmt.Fprintf(os.Stderr, "=== dumping ALL goroutine stacks (test process) ===\n")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		dumpWorkerDiagnostics()
		fmt.Fprintf(os.Stderr, "=== SUITE WATCHDOG: forcing exit(1) after dump ===\n")
		os.Exit(1)
	}()
}

// dumpWorkerDiagnostics fetches goroutine and native-thread stacks from
// the worker subprocess(es) behind each running server via the debug
// endpoints, printing them to stderr. Best-effort: each failure is
// logged and skipped so the watchdog always reaches its force-exit. The
// native-stacks output is the key signal for #420 — a worker blocked in
// an unpreemptable cgo call into the BAML native lib shows a native
// frame here while its Go goroutine sits in cgocall.
func dumpWorkerDiagnostics() {
	clients := []struct {
		name   string
		client *testutil.BAMLRestClient
	}{
		{"baml-rest", BAMLClient},
		{"unary", UnaryClient},
	}
	// The worker subprocess only fills per-worker matched_stacks when a
	// filter is supplied (an empty filter returns just the main process's
	// full dump, which the watchdog already captured via pprof). Match
	// the BAML and baml-rest frames so a worker wedged in a BAML
	// streaming/cgo call shows up here.
	const workerStackFilter = "invakid404/baml-rest,boundaryml/baml"
	for _, c := range clients {
		if c.client == nil {
			continue
		}
		// Each endpoint gets its own 30s budget so a slow goroutine dump
		// can't starve the native-stack dump — we need both, and the
		// native stacks are the key cgo-vs-Go signal.
		gctx, gcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if g, err := c.client.GetGoroutines(gctx, workerStackFilter); err == nil {
			for _, w := range g.Workers {
				if w.Error != "" {
					fmt.Fprintf(os.Stderr, "=== %s worker %d goroutines: error: %s ===\n", c.name, w.WorkerID, w.Error)
					continue
				}
				fmt.Fprintf(os.Stderr, "=== %s worker %d goroutines (total=%d, matched=%d) ===\n%s\n", c.name, w.WorkerID, w.TotalCount, w.MatchCount, strings.Join(w.MatchedStacks, "\n"))
			}
		} else {
			fmt.Fprintf(os.Stderr, "=== %s worker goroutines: error: %v ===\n", c.name, err)
		}
		gcancel()

		nctx, ncancel := context.WithTimeout(context.Background(), 30*time.Second)
		if n, err := c.client.GetNativeStacks(nctx); err == nil {
			for _, w := range n.Workers {
				if w.Error != "" {
					fmt.Fprintf(os.Stderr, "=== %s worker %d native stacks (pid=%d): error: %s ===\n", c.name, w.WorkerID, w.Pid, w.Error)
					continue
				}
				fmt.Fprintf(os.Stderr, "=== %s worker %d native stacks (pid=%d) ===\n%s\n", c.name, w.WorkerID, w.Pid, w.Output)
			}
		} else {
			fmt.Fprintf(os.Stderr, "=== %s worker native stacks: error: %v ===\n", c.name, err)
		}
		ncancel()
	}
}

// defaultTeardownTimeout bounds TestMain's post-m.Run teardown. The
// observed #420 failure is not a hang inside m.Run (every in-test client
// read is already bounded by a per-test context and the 2m
// http.Client.Timeout) but a stall in teardown: TestEnv.Terminate was
// called with an unbounded context.Background(), so a wedged container
// that won't stop dragged Docker stop/remove out to the 60m job cap with
// no diagnostics. Bounding teardown converts that into a fast, named
// teardown failure (and a container-side stack dump) instead.
const defaultTeardownTimeout = 5 * time.Minute

// teardownTimeout resolves the teardown budget from
// BAML_REST_TEARDOWN_TIMEOUT (any time.ParseDuration value), defaulting
// to defaultTeardownTimeout. "0"/"off" restores the old unbounded
// behavior (returns 0 → context.Background()).
func teardownTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("BAML_REST_TEARDOWN_TIMEOUT"))
	switch strings.ToLower(raw) {
	case "":
		return defaultTeardownTimeout
	case "0", "off", "disable", "disabled":
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid BAML_REST_TEARDOWN_TIMEOUT %q: %v; using %s\n", raw, err, defaultTeardownTimeout)
		return defaultTeardownTimeout
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "negative BAML_REST_TEARDOWN_TIMEOUT %q (parsed %s); using %s\n", raw, d, defaultTeardownTimeout)
		return defaultTeardownTimeout
	}
	// A parsed 0 (e.g. "0s") passes through and restores the unbounded
	// teardown, consistent with the "0"/"off" literals above.
	return d
}

// envTerminator is the subset of *TestEnvironment that boundedTerminate
// needs, so the bounding logic can be unit-tested with a fake that
// blocks until its context is cancelled.
type envTerminator interface {
	Terminate(context.Context) error
}

// classifyTeardownResult maps a completed teardown (its error and the
// bounded context's error) to the (err, timedOut) contract. timedOut is
// classified on ctxErr, NOT on the teardown error value:
// TestEnvironment.Terminate flattens its aggregate with %v (not %w), so a
// real deadline error from an inner Terminate/Remove would not satisfy
// errors.Is(err, context.DeadlineExceeded) and the caller would skip the
// #420 stack dump. ctxErr is wrapping- and error-shape-independent.
// Gated on err != nil so a boundary success (Terminate returned nil just
// as ctx expired) stays (nil, false) — no phantom timeout.
func classifyTeardownResult(err error, ctxErr error) (error, bool) {
	if err == nil {
		return nil, false
	}
	return err, ctxErr != nil
}

// boundedTerminate runs env teardown under a budget-bounded context so a
// wedged container can never stall teardown to the job cap (#420). A
// budget of 0 means "no bound" (legacy context.Background()). It returns
// the teardown error and whether the budget was exceeded (so the caller
// can capture a container-side dump while the container is likely still
// alive holding the stuck goroutine).
//
// The deadline is enforced INDEPENDENTLY of whether env.Terminate honors
// the context: Terminate runs in a goroutine and we select on its result
// vs ctx.Done(). The #420 stall is a Docker stop/remove that may not be
// abortable via ctx cancellation, so a synchronous call could block past
// the budget on the very failure this bounds. errCh is buffered so the
// goroutine never blocks even when we return via the timeout path; a
// wedged Terminate goroutine may then linger, but this runs in TestMain
// teardown immediately before diagnostics + os.Exit, so that leak is
// acceptable.
func boundedTerminate(env envTerminator, budget time.Duration) (err error, timedOut bool) {
	if budget <= 0 {
		return env.Terminate(context.Background()), false
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- env.Terminate(ctx)
	}()

	// finish maps a completed Terminate result to the (err, timedOut)
	// contract, classifying timedOut on the bounded ctx — see
	// classifyTeardownResult.
	finish := func(err error) (error, bool) {
		return classifyTeardownResult(err, ctx.Err())
	}

	select {
	case err := <-errCh:
		return finish(err)
	case <-ctx.Done():
		// Photo-finish tie-break: prefer a Terminate result that already
		// completed by the time the deadline fired, so a benign
		// slow-but-successful teardown resolves to its real result
		// deterministically rather than racing the select arms. Only a
		// genuine timeout with no completion reports (DeadlineExceeded,
		// true) — which still flips CI via exitCodeAfterTeardown.
		select {
		case err := <-errCh:
			return finish(err)
		default:
			return ctx.Err(), true
		}
	}
}

// exitCodeAfterTeardown decides the process exit code given the test
// result code and the teardown error. A teardown failure of any kind
// (timeout or otherwise) must fail the suite: otherwise a green test run
// followed by a wedged-teardown hang — the #420 shape this PR exists to
// surface — would still exit 0 and let CI pass. A non-zero test code is
// left as-is (the test failure dominates and is the more useful signal).
func exitCodeAfterTeardown(code int, teardownErr error) int {
	if code == 0 && teardownErr != nil {
		return 1
	}
	return code
}

func TestMain(m *testing.M) {
	// Arm the suite watchdog FIRST, before container setup, so its budget
	// is measured from process start and covers setup + m.Run + teardown.
	// Setup now does container-create retries with backoff (#415), so a
	// slow/retrying setup must not eat into the watchdog's headroom — and
	// Go's `go test -timeout` covers none of setup or teardown anyway. It
	// is deliberately never stopped: the process exits (os.Exit at the end
	// of TestMain) on a clean run before it fires, while on a hang it must
	// still be live during teardown (#420).
	startSuiteWatchdog(suiteWatchdogTimeout())

	// Find the testdata directory
	var err error
	bamlSrcPath, err = findTestdataPath()
	if err != nil {
		println("Failed to find testdata:", err.Error())
		os.Exit(1)
	}

	// Get the appropriate adapter version
	adapterVersion, err = testutil.GetAdapterVersionForBAML(BAMLVersion)
	if err != nil {
		println("Failed to get adapter version:", err.Error())
		os.Exit(1)
	}

	println("Setting up test environment...")
	println("  BAML Version:", BAMLVersion)
	println("  Adapter Version:", adapterVersion)
	println("  BAML Src Path:", bamlSrcPath)
	if BAMLSourcePath != "" {
		println("  BAML Source:", BAMLSourcePath)
	}

	// Setup test environment
	if v := os.Getenv("UNARY_SERVER"); v != "" {
		unaryServer, err = strconv.ParseBool(v)
		if err != nil {
			println("Invalid UNARY_SERVER value:", v, "(expected true/false)")
			os.Exit(1)
		}
	}
	UseBuildRequest = parseBoolEnv(os.Getenv("BAML_REST_USE_BUILD_REQUEST"))

	// The overall setup budget is mode-aware and centralized in testutil
	// (#424): baml-source builds the Rust cffi stage and need the larger
	// budget, the light jobs a smaller one. This ctx is NOT covered by
	// `go test -timeout` (it runs before m.Run); the suite watchdog and step
	// timeout are its outer guards.
	opts := matrixSetupOptions()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.SetupBudget(opts))
	defer cancel()

	TestEnv, err = testutil.Setup(ctx, opts)
	if err != nil {
		println("Failed to setup test environment:", err.Error())
		os.Exit(1)
	}

	println("Test environment ready:")
	println("  Mock LLM URL:", TestEnv.MockLLMURL)
	println("  Mock LLM Internal URL:", TestEnv.MockLLMInternal)
	println("  BAML REST URL:", TestEnv.BAMLRestURL)
	println("  Unary URL:", TestEnv.BAMLRestUnaryURL)
	println("  UseBuildRequest:", strconv.FormatBool(UseBuildRequest))
	println("  InProcess:", strconv.FormatBool(inProcessBuild))

	// Create clients
	MockClient = mockllm.NewClient(TestEnv.MockLLMURL)
	BAMLClient = testutil.NewBAMLRestClient(TestEnv.BAMLRestURL)
	if TestEnv.BAMLRestUnaryURL != "" {
		UnaryClient = testutil.NewBAMLRestClient(TestEnv.BAMLRestUnaryURL)
	}

	// Run tests. The suite watchdog armed at the top of TestMain stays
	// live across m.Run AND teardown — the post-m.Run window Go's
	// `-timeout` does not cover (#420).
	code := m.Run()

	// Dump container logs on failure to surface errors from inside
	// the Docker containers (e.g. BAML runtime panics, worker crashes)
	if code != 0 {
		dumpContainerLogs("BAML REST", TestEnv.BAMLRest)
		dumpContainerLogs("Mock LLM", TestEnv.MockLLM)
	}

	// Cleanup. Bound teardown so a wedged container fails fast with a
	// named error (and a container-side stack dump) instead of stalling
	// Docker stop/remove to the 60m job cap — the actual #420 shape, in
	// the post-m.Run window Go's `-timeout` does not cover.
	println("Tearing down test environment...")
	if err, timedOut := boundedTerminate(TestEnv, teardownTimeout()); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to terminate test environment: %v\n", err)
		if timedOut {
			fmt.Fprintf(os.Stderr, "=== teardown exceeded %s — capturing diagnostics before exit (#420) ===\n", teardownTimeout())
			fmt.Fprintf(os.Stderr, "=== dumping ALL goroutine stacks (test process) ===\n")
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			dumpWorkerDiagnostics()
		}
		// A teardown failure must not pass CI on an otherwise-green run —
		// that is exactly the wedged-teardown hang this PR surfaces.
		code = exitCodeAfterTeardown(code, err)
	}

	os.Exit(code)
}

// dumpContainerLogs fetches and prints logs from a Docker container.
// Used on test failure to surface errors from inside containers
// (e.g. BAML runtime Rust panics, worker crashes) that are otherwise invisible.
func dumpContainerLogs(name string, container testcontainers.Container) {
	if container == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logs, err := container.Logs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get %s container logs: %v\n", name, err)
		return
	}
	defer logs.Close()

	data, err := io.ReadAll(logs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s container logs: %v\n", name, err)
		return
	}

	fmt.Fprintf(os.Stderr, "\n=== %s container logs ===\n%s=== end %s logs ===\n\n", name, data, name)
}

func findTestdataPath() (string, error) {
	// Try relative to current directory first
	paths := []string{
		"integration/testdata/baml_src",
		"testdata/baml_src",
		"../testdata/baml_src",
	}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
	}

	// Try relative to GOMOD file
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Walk up to find project root
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			bamlSrcPath := filepath.Join(dir, "integration", "testdata", "baml_src")
			if _, err := os.Stat(bamlSrcPath); err == nil {
				return bamlSrcPath, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", os.ErrNotExist
}

// waitForHealthy polls the health endpoint until the server reports healthy or
// the timeout expires. Useful after worker death tests where the pool may still
// be recovering.
func waitForHealthy(t *testing.T, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := BAMLClient.Health(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Server did not become healthy within %s", timeout)
		case <-ticker.C:
		}
	}
}

// Helper to register a scenario and create client options
func setupScenario(t *testing.T, scenarioID, content string) *testutil.BAMLOptions {
	t.Helper()

	// Ensure the server is healthy before setting up. Tests may run after
	// destructive operations (worker kills, etc.) in any order.
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        content,
		ChunkSize:      20, // 20 chars per chunk for streaming
		InitialDelayMs: 50,
		ChunkDelayMs:   10,
		ChunkJitterMs:  5,
	}

	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	return &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}
}

// namedClient pairs a display name with a BAMLRestClient for parameterized testing.
type namedClient struct {
	Name   string
	Client *testutil.BAMLRestClient
}

// unaryTestClients returns the set of clients to test unary endpoints against.
// When the unary server is enabled, both Fiber and chi are tested;
// otherwise only Fiber is tested.
func unaryTestClients() []namedClient {
	clients := []namedClient{
		{"fiber", BAMLClient},
	}
	if UnaryClient != nil {
		clients = append(clients, namedClient{"chi", UnaryClient})
	}
	return clients
}

// forEachUnaryClient runs fn as a subtest for each unary backend (Fiber and chi).
// Use this wrapper for tests that exercise only unary endpoints (/call, /call-with-raw, /parse).
func forEachUnaryClient(t *testing.T, fn func(t *testing.T, client *testutil.BAMLRestClient)) {
	t.Helper()
	for _, nc := range unaryTestClients() {
		t.Run(nc.Name, func(t *testing.T) {
			fn(t, nc.Client)
		})
	}
}

// callAndDecode calls client.Call, asserts status 200, and unmarshals the
// response body into T. Reduces boilerplate in tests that follow the common
// call → assert-200 → unmarshal pattern.
func callAndDecode[T any](t *testing.T, client *testutil.BAMLRestClient, ctx context.Context, req testutil.CallRequest) T {
	t.Helper()
	resp, err := client.Call(ctx, req)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
	}
	var result T
	if err := sonic.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	return result
}

// Helper to register a non-streaming scenario
func setupNonStreamingScenario(t *testing.T, scenarioID, content string) *testutil.BAMLOptions {
	t.Helper()

	// Ensure the server is healthy before setting up. Tests may run after
	// destructive operations (worker kills, etc.) in any order.
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        content,
		ChunkSize:      0, // 0 = non-streaming
		InitialDelayMs: 50,
	}

	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	return &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}
}
