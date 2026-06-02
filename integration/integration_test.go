//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	return testutil.SetupOptions{
		BAMLSrcPath:     bamlSrcPath,
		BAMLVersion:     BAMLVersion,
		AdapterVersion:  adapterVersion,
		BAMLSource:      BAMLSourcePath,
		UnaryServer:     unaryServer,
		UseBuildRequest: UseBuildRequest,
		InProcess:       inProcessBuild,
	}
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

func TestMain(m *testing.M) {
	timeout := 10 * time.Minute
	if BAMLSourcePath != "" {
		timeout = 30 * time.Minute // Rust compilation is slow
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

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

	// FuzzBamlfuzzStatic worker subprocesses re-run TestMain but don't need
	// the shared Docker environment — that target builds its own per-iteration
	// env via setupStaticEnv and never touches TestEnv. Skip the expensive
	// Setup for those workers to avoid resource conflicts and unrecovered
	// goroutine panics from testcontainers health-check goroutines. The other
	// fuzz targets (Dynamic, Streaming, InvalidJSONCoercion) use the shared
	// TestEnv/MockClient/BAMLClient, so their workers must still build it.
	if isStaticFuzzWorker() {
		os.Exit(m.Run())
	}

	TestEnv, err = testutil.Setup(ctx, matrixSetupOptions())
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

	// Run tests
	code := m.Run()

	// Dump container logs on failure to surface errors from inside
	// the Docker containers (e.g. BAML runtime panics, worker crashes)
	if code != 0 {
		dumpContainerLogs("BAML REST", TestEnv.BAMLRest)
		dumpContainerLogs("Mock LLM", TestEnv.MockLLM)
	}

	// Cleanup
	println("Tearing down test environment...")
	if err := TestEnv.Terminate(context.Background()); err != nil {
		println("Failed to terminate test environment:", err.Error())
	}

	os.Exit(code)
}

// isStaticFuzzWorker reports whether this process is a Go fuzzing worker
// subprocess (-test.fuzzworker) that is fuzzing the FuzzBamlfuzzStatic
// target. Only that target builds its own per-iteration environment via
// setupStaticEnv; the others (Dynamic, Streaming, InvalidJSONCoercion) rely
// on the shared TestEnv, so their workers must still run the Docker setup.
//
// The fuzz framework launches workers with -test.fuzzworker prepended to a
// copy of the coordinator's args (internal/fuzz.startWorkers), so the worker
// inherits the same -test.fuzz=<pattern> regexp the coordinator was invoked
// with — the nightly uses -fuzz='^FuzzBamlfuzzStatic$'.
func isStaticFuzzWorker() bool {
	var fuzzWorker bool
	var fuzzPattern string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-test.fuzzworker" || arg == "--test.fuzzworker",
			strings.HasPrefix(arg, "-test.fuzzworker="), strings.HasPrefix(arg, "--test.fuzzworker="):
			fuzzWorker = true
		case strings.HasPrefix(arg, "-test.fuzz="):
			fuzzPattern = strings.TrimPrefix(arg, "-test.fuzz=")
		case strings.HasPrefix(arg, "--test.fuzz="):
			fuzzPattern = strings.TrimPrefix(arg, "--test.fuzz=")
		case arg == "-test.fuzz" || arg == "--test.fuzz":
			if i+1 < len(args) {
				fuzzPattern = args[i+1]
				i++
			}
		}
	}
	return fuzzWorker && strings.Contains(fuzzPattern, "FuzzBamlfuzzStatic")
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
