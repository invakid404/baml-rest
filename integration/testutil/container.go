//go:build integration

package testutil

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"text/template"
	"time"

	bamlrest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// MockLLMContainerName is the hostname for the mock LLM server on the Docker network
	MockLLMContainerName = "mockllm"

	// BAMLRestContainerName is the hostname for the baml-rest server on the Docker network
	BAMLRestContainerName = "bamlrest"

	// MockLLMInternalPort is the port the mock server listens on inside Docker
	MockLLMInternalPort = "8080/tcp"

	// BAMLRestInternalPort is the port baml-rest listens on inside Docker
	BAMLRestInternalPort = "8080/tcp"

	// BAMLRestUnaryPort is the port for the unary server inside Docker
	BAMLRestUnaryPort = "8081/tcp"
)

// TestEnvironment holds the running test containers and network.
type TestEnvironment struct {
	Network          *testcontainers.DockerNetwork
	MockLLM          testcontainers.Container
	BAMLRest         testcontainers.Container
	MockLLMURL       string // URL to reach mock server from host (http://localhost:xxxxx)
	BAMLRestURL      string // URL to reach baml-rest from host (http://localhost:xxxxx)
	BAMLRestUnaryURL string // URL to reach unary server from host (http://localhost:xxxxx)
	MockLLMInternal  string // URL to reach mock server from baml-rest (http://mockllm:8080)
}

// Terminate shuts down all containers and network.
func (e *TestEnvironment) Terminate(ctx context.Context) error {
	var errs []error

	if e.BAMLRest != nil {
		if err := e.BAMLRest.Terminate(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to terminate baml-rest: %w", err))
		}
	}

	if e.MockLLM != nil {
		if err := e.MockLLM.Terminate(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to terminate mock LLM: %w", err))
		}
	}

	if e.Network != nil {
		if err := e.Network.Remove(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove network: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("terminate errors: %v", errs)
	}
	return nil
}

// SetupOptions configures the test environment.
type SetupOptions struct {
	// BAMLSrcPath is the path to the baml_src directory for the test fixtures
	BAMLSrcPath string

	// BAMLVersion is the BAML version to use (e.g., "0.214.0")
	BAMLVersion string

	// AdapterVersion is the adapter version path (e.g., "adapters/adapter_v0_204_0")
	AdapterVersion string

	// KeepSource keeps generated sources at the specified path (optional)
	KeepSource string

	// BAMLSource is the path to a local BAML source repository for building from unreleased versions
	BAMLSource string

	// PrebuiltCffiDir, when set, points at a directory holding a prebuilt
	// libbaml_cffi.so + baml-cli (built once in CI inside rust:bookworm for
	// glibc parity). When set, the in-Docker cargo build (cffi-builder stage)
	// is skipped and these artifacts are injected into the build context
	// instead. Requires BAMLSource to be set (for language_client_go source).
	// When unset, behavior is unchanged: the cffi lib is built from source
	// inside the Docker image (the local-dev default).
	PrebuiltCffiDir string

	// UnaryServer enables the opt-in chi/net-http unary server on port 8081.
	UnaryServer bool

	// UseBuildRequest enables the BuildRequest/StreamRequest path inside
	// the container. When false the container uses the legacy
	// CallStream+OnTick path. This must be forwarded from the host env
	// so that the CI matrix leg actually toggles the code path under test.
	UseBuildRequest bool

	// InProcess builds the baml-rest binary without the `subprocess`
	// build tag so the server and worker handler share one OS process.
	// The Dockerfile template forwards this to build.sh which drops
	// SUBPROCESS from the go build invocation.
	InProcess bool

	// RuntimeEnv adds arbitrary environment variables to the baml-rest
	// container. Used by tests that need to configure runtime-only features
	// (e.g. BAML_REST_CLIENT_DEFAULTS) that the shared test env does not set.
	// Keys collide in favor of RuntimeEnv, so tests can override defaults.
	RuntimeEnv map[string]string
}

// setupRetryBackoff is the delay applied before each retry of the container
// build+start sequence. Its length determines how many retries Setup performs:
// the first entry is the wait before the 2nd attempt, the second before the
// 3rd, and so on. Three retries (plus the initial attempt) gives four total
// tries with escalating backoff.
var setupRetryBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

// Setup-budget constants — the single source of truth for every setup-ctx
// construction across the integration suite (#424). Every callsite that does
// context.WithTimeout(..., testutil.Setup) routes its budget through
// SetupBudget so the per-attempt/overall relationship is enforced in one
// place rather than scattered as magic numbers.
//
// The #424 failure was a slow-but-healthy Docker image build (mockllm +
// baml-rest, sequential, on a contended GitHub runner) exceeding the old 10m
// main-setup ctx (integration_test.go) and tripping the
// "context done before retrying" bail before the retry path could engage. The
// load-bearing fix is growing the OVERALL (parent) budget so one generous
// single attempt can ride out that transient slowness; the per-attempt budget
// (below) is the structural cleanup that stops a slow attempt from silently
// consuming the whole parent.
//
// Budgets are sized against MEASURED green-CI timings and the #422 suite
// watchdog, which bounds the window Go's `go test -timeout` does not cover
// (setup + m.Run + teardown). The governing inequality, per job class, is:
//
//	overall_setup + healthy_m.Run + teardown(<= defaultTeardownTimeout=5m) <= watchdog
//
// Measured on a green master run (build = "Setting up..." -> "ready",
// m.Run = "ready" -> "Tearing down"):
//
//	light (versioned/in-process, watchdog 42m): build ~4.5m, m.Run ~12m worst,
//	    teardown <1s. Ceiling check: 20 + 12 + 5 = 37m < 42m (>=2m margin) and
//	    < the 45m step timeout.
//	baml-source (watchdog 57m): build ~18.5m cold (Rust cargo build --release
//	    cffi stage dominates), m.Run ~26m worst, teardown ~1s. Real:
//	    18.5 + 26 + ~0 = ~44.5m < 57m. The 30m ceiling intentionally exceeds the
//	    strict ceiling-sum (30 + 26 + 5 > 57) because real cold setup is ~18.5m,
//	    not 30m; 30m is a pathology ceiling the watchdog still backstops, and it
//	    is left unchanged so baml-source's single-attempt budget never shrinks.
const (
	// OverallSetupBudgetLight is the overall (parent) ctx budget for a full
	// Setup call in the light job classes (versioned + in-process matrices,
	// BAML_SOURCE unset). Grown from the historical 10m to ride out a
	// slow-but-healthy build while staying under the 42m watchdog (#424).
	OverallSetupBudgetLight = 20 * time.Minute

	// OverallSetupBudgetBAMLSource is the overall budget for baml-source
	// builds (BAML_SOURCE set), whose Rust cargo build --release cffi stage
	// dominates setup. Unchanged from the historical 30m so the single-attempt
	// budget never shrinks (scope G1); the 57m watchdog backstops it.
	OverallSetupBudgetBAMLSource = 30 * time.Minute

	// setupRetryReserve is the slice of the overall budget held back from a
	// single attempt so a FAST-failing attempt (build.sh non-zero exit, BAML
	// library 404, init panic, registry blip — the transient classes #415's
	// retry targets) leaves real headroom for the retry to actually run. A
	// slow-success build that exhausts its per-attempt budget deliberately
	// does NOT get a meaningful retry: retrying a build that is slow due to
	// runner/daemon contention just hits the same wall (scope G4/G5), so the
	// design favors one big single attempt over guaranteeing a second.
	setupRetryReserve = 2 * time.Minute
)

// setupCleanupTimeout bounds the partial-teardown of a failed attempt
// (terminating whatever network/containers it had already created before the
// next attempt or before the error propagates). This budget is deliberately
// INDEPENDENT of the per-attempt work context: testcontainers Terminate needs
// a LIVE context to issue Docker stop/remove, but the most common cleanup
// trigger is the per-attempt deadline firing — so cleanup is run on a fresh
// context.Background()-derived ctx (see runSetupCleanup), not the expired work
// ctx. Sized smaller than the 5m defaultTeardownTimeout because a
// partially-started attempt has at most a half-built container and a network to
// remove, not a full running environment. A var (not const) so tests can shrink
// it to assert the budget is actually ENFORCED.
var setupCleanupTimeout = 2 * time.Minute

// SetupBudget returns the overall (parent) context budget a Setup call should
// be given for the supplied options. baml-source builds compile the Rust cffi
// stage and need the larger budget; every other build uses the light budget.
// This is the one knob every setup-ctx callsite reads (#424).
func SetupBudget(opts SetupOptions) time.Duration {
	if opts.BAMLSource != "" {
		return OverallSetupBudgetBAMLSource
	}
	return OverallSetupBudgetLight
}

// perAttemptBudget derives the per-attempt deadline from the parent ctx at
// loop entry: the whole remaining parent budget minus setupRetryReserve, so
// the first attempt gets a generous single build window while leaving room for
// a fast-fail retry. This is mode-aware for free — a 20m parent yields ~18m,
// a 30m parent ~28m (scope G1). Returns 0 ("no per-attempt bound; use the
// parent ctx as-is") when the parent has no deadline, or when too little
// budget remains to both run an attempt and reserve retry headroom — in the
// latter case a sub-context would only shrink the attempt further, and the
// parent deadline still caps it.
func perAttemptBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	budget := time.Until(deadline) - setupRetryReserve
	if budget <= 0 {
		return 0
	}
	return budget
}

// Setup creates the test environment with mock LLM server and baml-rest
// container, retrying the whole build+start sequence on transient failures.
//
// Container creation is the flaky part of the integration suite: Docker image
// builds (build.sh) occasionally exit non-zero, the BAML library download can
// fail ("could not find BAML library v0.XXX.0 for linux/amd64"), the BAML
// runtime can panic on init inside the container, the registry can time out,
// and runners hit disk pressure. These are transient infra failures, not test
// logic bugs, so we retry the entire setup with exponential backoff. Only the
// build/start sequence is retried here; actual test execution is untouched.
//
// Each failed attempt fully tears down whatever was partially created (network
// and any started containers) via setupOnce before the next attempt, so retries
// always start from a clean slate.
func Setup(ctx context.Context, opts SetupOptions) (*TestEnvironment, error) {
	return setupWithRetry(ctx, perAttemptBudget(ctx), setupRetryBackoff,
		func(attemptCtx context.Context) (*TestEnvironment, error) {
			return runSetupAttempt(attemptCtx, opts)
		})
}

// setupWithRetry is the retry loop extracted from Setup so it can be
// unit-tested without real Docker (#424). It runs attempt under its own
// per-attempt deadline (perAttempt, capped by the parent ctx) so a slow
// attempt cannot silently consume the whole parent budget, then decides
// retry-vs-bail on the PARENT ctx:
//
//   - attempt timed out on its per-attempt budget but the parent still has
//     budget left -> ctx.Err() == nil -> fall through and retry (the
//     fast-fail transient path #415 targets); and
//   - parent exhausted -> ctx.Err() != nil -> bail, since every further
//     attempt would fail the same way.
//
// perAttempt <= 0 means "no per-attempt bound": the attempt runs on the
// parent ctx directly. attempt receives the per-attempt context and MUST
// honor it (runSetupAttempt -> setupOnce threads it through both the mockllm
// and baml-rest builds plus their startup waits, so one attempt's deadline
// covers the whole sequence — scope G9).
func setupWithRetry(
	ctx context.Context,
	perAttempt time.Duration,
	backoff []time.Duration,
	attempt func(context.Context) (*TestEnvironment, error),
) (*TestEnvironment, error) {
	totalAttempts := len(backoff) + 1

	var lastErr error
	for n := 1; n <= totalAttempts; n++ {
		if n > 1 {
			wait := backoff[n-2]
			log.Printf("[testutil] container setup attempt %d/%d failed: %v; "+
				"cleaning up and retrying in %s", n-1, totalAttempts, lastErr, wait)

			// Respect cancellation/deadline while backing off so we don't
			// burn the remaining context budget sleeping.
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("container setup aborted after %d attempt(s) while "+
					"waiting to retry: %w (last error: %v)", n-1, ctx.Err(), lastErr)
			case <-timer.C:
			}
		}

		attemptCtx, cancel := attemptContext(ctx, perAttempt)
		env, err := attempt(attemptCtx)
		cancel()
		if err == nil {
			if n > 1 {
				log.Printf("[testutil] container setup succeeded on attempt %d/%d", n, totalAttempts)
			}
			return env, nil
		}
		lastErr = err

		// If the PARENT context is already done there's no point retrying:
		// every further attempt would fail the same way. A per-attempt
		// timeout that leaves parent budget intact does NOT take this branch,
		// so it proceeds to the next attempt. Report the number of attempts
		// actually made (not the cap).
		if ctx.Err() != nil {
			return nil, fmt.Errorf("container setup failed after %d attempt(s); "+
				"context done before retrying: %w (last error: %v)", n, ctx.Err(), lastErr)
		}
	}

	return nil, fmt.Errorf("container setup failed after %d attempt(s): %w", totalAttempts, lastErr)
}

// attemptContext derives the per-attempt context from the parent. A
// non-positive perAttempt means "no per-attempt bound" (the attempt runs on
// the parent ctx directly); otherwise the attempt deadline is
// min(perAttempt, parent-remaining) since the child is derived from ctx.
func attemptContext(ctx context.Context, perAttempt time.Duration) (context.Context, context.CancelFunc) {
	if perAttempt <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, perAttempt)
}

// runSetupCleanup runs a partial-teardown function under a fresh, bounded
// context derived from context.Background() — independent of BOTH the
// (possibly expired) per-attempt work ctx AND a (possibly cancelled) parent
// ctx. testcontainers Terminate needs a LIVE context to issue Docker
// stop/remove; the dominant cleanup trigger is the per-attempt deadline
// firing, and the overall budget may also be exhausted, so reusing either
// would leave half-started containers and networks leaked before the retry.
//
// The budget is ENFORCED preemptively (same goroutine+select shape as
// integration.boundedTerminate, the #422 teardown bound): a Docker stop/remove
// that ignores context cancellation must not block the retry/bail loop past the
// budget. teardown runs in a goroutine reporting on a buffered channel so it
// never blocks on send even after we've returned via the deadline; a leaked
// goroutine on a truly-wedged Terminate is acceptable for best-effort
// inter-attempt cleanup. Errors are ignored (best-effort), matching the
// existing setup-path cleanup contract; cancel is always invoked so the cleanup
// ctx itself never leaks.
func runSetupCleanup(teardown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupCleanupTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// teardown runs on its own goroutine, outside runSetupAttempt's recover
		// wrapper, so contain a panic from Terminate/network removal here — a
		// panicking best-effort cleanup must not crash the test binary. teardown
		// either returns OR panics (never both), so at most one send happens;
		// errCh is buffered (cap 1) so neither send blocks.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[testutil] recovered panic during setup cleanup: %v", r)
				errCh <- fmt.Errorf("setup cleanup panicked: %v\n%s", r, debug.Stack())
			}
		}()
		errCh <- teardown(ctx)
	}()
	select {
	case <-errCh:
	case <-ctx.Done():
	}
}

// runSetupAttempt performs a single setupOnce and converts a panic into an
// error so a true Go panic from deep inside testcontainers still triggers a
// retry instead of tearing down the whole test binary. setupOnce tears down
// its own partial resources (including on panic, see its deferred recover)
// before the panic propagates here, so no network or container is leaked.
func runSetupAttempt(ctx context.Context, opts SetupOptions) (env *TestEnvironment, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("container setup panicked: %v\n%s", r, debug.Stack())
			log.Printf("[testutil] recovered panic during container setup: %v", r)
		}
	}()

	return setupOnce(ctx, opts)
}

// setupOnce performs a single build+start of the mock LLM server and baml-rest
// container. On any failure it terminates whatever it had already created
// (network and/or containers) before returning, so callers can safely retry.
func setupOnce(ctx context.Context, opts SetupOptions) (*TestEnvironment, error) {
	env := &TestEnvironment{}

	// If anything below panics (e.g. deep inside testcontainers), tear down
	// whatever was already created before the panic propagates to the retry
	// wrapper, so a retry doesn't leak a network or half-started container.
	defer func() {
		if r := recover(); r != nil {
			runSetupCleanup(env.Terminate)
			panic(r)
		}
	}()

	// Create Docker network
	net, err := network.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker network: %w", err)
	}
	env.Network = net

	// Start mock LLM server
	mockLLM, err := startMockLLMContainer(ctx, net.Name)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to start mock LLM container: %w", err)
	}
	env.MockLLM = mockLLM

	// Get mock LLM mapped port
	mockPort, err := mockLLM.MappedPort(ctx, MockLLMInternalPort)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to get mock LLM port: %w", err)
	}
	mockHost, err := mockLLM.Host(ctx)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to get mock LLM host: %w", err)
	}
	env.MockLLMURL = fmt.Sprintf("http://%s:%s", mockHost, mockPort.Port())
	env.MockLLMInternal = fmt.Sprintf("http://%s:8080", MockLLMContainerName)

	// Build and start baml-rest container
	bamlRest, err := startBAMLRestContainer(ctx, net.Name, opts)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to start baml-rest container: %w", err)
	}
	env.BAMLRest = bamlRest

	// Get baml-rest mapped ports
	restPort, err := bamlRest.MappedPort(ctx, BAMLRestInternalPort)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to get baml-rest port: %w", err)
	}
	restHost, err := bamlRest.Host(ctx)
	if err != nil {
		runSetupCleanup(env.Terminate)
		return nil, fmt.Errorf("failed to get baml-rest host: %w", err)
	}
	env.BAMLRestURL = fmt.Sprintf("http://%s:%s", restHost, restPort.Port())

	if opts.UnaryServer {
		unaryPort, err := bamlRest.MappedPort(ctx, BAMLRestUnaryPort)
		if err != nil {
			runSetupCleanup(env.Terminate)
			return nil, fmt.Errorf("failed to get baml-rest unary port: %w", err)
		}
		env.BAMLRestUnaryURL = fmt.Sprintf("http://%s:%s", restHost, unaryPort.Port())
	}

	return env, nil
}

func startMockLLMContainer(ctx context.Context, networkName string) (testcontainers.Container, error) {
	// Build context from the mockllm directory
	buildCtx, err := createMockLLMBuildContext()
	if err != nil {
		return nil, fmt.Errorf("failed to create mock LLM build context: %w", err)
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			ContextArchive: buildCtx,
			Dockerfile:     "integration/mockllm/Dockerfile",
			PrintBuildLog:  true, // Enable build log output to see compilation errors
		},
		ExposedPorts: []string{MockLLMInternalPort},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {MockLLMContainerName},
		},
		WaitingFor: wait.ForHTTP("/_admin/health").WithPort(MockLLMInternalPort).WithStartupTimeout(30 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		// On a start/wait failure (e.g. the health check never passes)
		// testcontainers may still return a built container. Tear it down so a
		// retry doesn't leak it.
		if c != nil {
			runSetupCleanup(func(cleanupCtx context.Context) error { return c.Terminate(cleanupCtx) })
		}
		return nil, err
	}
	return c, nil
}

func createMockLLMBuildContext() (io.ReadSeeker, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Get the project root (parent of integration directory)
	projectRoot, err := findProjectRoot()
	if err != nil {
		return nil, err
	}

	// Add go.mod and go.sum so the mockllm build uses the project's dependencies
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := addFileToTar(tw, filepath.Join(projectRoot, name), name); err != nil {
			return nil, fmt.Errorf("failed to add %s to build context: %w", name, err)
		}
	}

	// Add integration/mockllm directory
	mockLLMDir := filepath.Join(projectRoot, "integration", "mockllm")
	if err := addDirToTar(tw, mockLLMDir, "integration/mockllm"); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf.Bytes()), nil
}

func startBAMLRestContainer(ctx context.Context, networkName string, opts SetupOptions) (testcontainers.Container, error) {
	buildCtx, err := createBAMLRestBuildContext(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create baml-rest build context: %w", err)
	}

	exposedPorts := []string{BAMLRestInternalPort}
	cmd := []string{"--sse-keepalive-interval=100ms"}
	waitStrategy := wait.ForHTTP("/openapi.json").WithPort(BAMLRestInternalPort).WithStartupTimeout(180 * time.Second)

	var waitFor wait.Strategy = waitStrategy
	if opts.UnaryServer {
		// When built with the unaryserver tag, the default port is 8081.
		// No need to pass --unary-port explicitly.
		exposedPorts = append(exposedPorts, BAMLRestUnaryPort)
		waitFor = wait.ForAll(
			waitStrategy,
			wait.ForListeningPort(BAMLRestUnaryPort).WithStartupTimeout(180*time.Second),
		)
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			ContextArchive: buildCtx,
			Dockerfile:     "Dockerfile",
			PrintBuildLog:  true, // Enable build log output to see compilation errors
		},
		ExposedPorts: exposedPorts,
		Cmd:          cmd,
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {BAMLRestContainerName},
		},
		Env: buildContainerEnv(opts),
		HostConfigModifier: func(hc *container.HostConfig) {
			if hc.Sysctls == nil {
				hc.Sysctls = make(map[string]string)
			}
			hc.Sysctls["net.ipv4.tcp_tw_reuse"] = "1"

			// SYS_PTRACE is required for gdb to attach to worker processes
			// for native thread backtraces (/_debug/native-stacks endpoint).
			hc.CapAdd = append(hc.CapAdd, "SYS_PTRACE")
		},
		WaitingFor: waitFor,
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		// A build.sh non-zero exit, a BAML library init panic, or a startup
		// timeout all surface here. In the start/wait cases testcontainers may
		// still hand back a built container; terminate it so a retry starts
		// clean instead of leaking a half-started container.
		if c != nil {
			runSetupCleanup(func(cleanupCtx context.Context) error { return c.Terminate(cleanupCtx) })
		}
		return nil, err
	}
	return c, nil
}

// buildContainerEnv returns the env map passed to the baml-rest container.
// Entries in opts.RuntimeEnv take precedence over the shared defaults, so a
// test can override BAML_LOG or BAML_REST_USE_BUILD_REQUEST if it needs to.
func buildContainerEnv(opts SetupOptions) map[string]string {
	env := map[string]string{
		"BAML_LOG":                    "debug",
		"BAML_REST_USE_BUILD_REQUEST": strconv.FormatBool(opts.UseBuildRequest),
	}
	for k, v := range opts.RuntimeEnv {
		env[k] = v
	}
	return env
}

// dockerfileTemplateData maps SetupOptions to the template fields used by cmd/build/Dockerfile.tmpl
type dockerfileTemplateData struct {
	// Standard fields (lowercase to match template)
	bamlVersion    string
	adapterVersion string
	keepSource     string
	debugBuild     bool

	// Integration test specific flags
	defaultTargetArch  string // Provide default for TARGETARCH (testcontainers may not set it)
	noCacheMount       bool   // Disable BuildKit cache mount (not supported in testcontainers)
	noCustomBamlLib    bool   // Don't copy custom_baml_lib.so (not used in tests)
	bamlSource         bool   // Build from BAML source (enables cffi-builder stage)
	prebuiltCffi       bool   // Inject prebuilt cffi artifacts instead of running the cffi-builder cargo stage
	protocGenGoVersion string // protoc-gen-go version for BAML source builds
	unaryServer        bool   // Build with unaryserver tag for chi unary server
	inProcess          bool   // Build single-process server+worker (drops subprocess tag)
}

// MarshalMap converts the template data to a map for template execution
func (d dockerfileTemplateData) toMap() map[string]any {
	return map[string]any{
		"bamlVersion":        d.bamlVersion,
		"adapterVersion":     d.adapterVersion,
		"keepSource":         d.keepSource,
		"debugBuild":         d.debugBuild,
		"defaultTargetArch":  d.defaultTargetArch,
		"noCacheMount":       d.noCacheMount,
		"noCustomBamlLib":    d.noCustomBamlLib,
		"bamlSource":         d.bamlSource,
		"prebuiltCffi":       d.prebuiltCffi,
		"protocGenGoVersion": d.protocGenGoVersion,
		"unaryServer":        d.unaryServer,
		"inProcess":          d.inProcess,
	}
}

func createBAMLRestBuildContext(opts SetupOptions) (io.ReadSeeker, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Get Dockerfile template from embedded sources
	dockerfileTmplContent, err := getDockerfileTemplate()
	if err != nil {
		return nil, fmt.Errorf("failed to get Dockerfile template: %w", err)
	}

	// Generate Dockerfile from template
	tmpl, err := template.New("dockerfile").Parse(string(dockerfileTmplContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse Dockerfile template: %w", err)
	}

	// Create template data with integration test specific flags
	tmplData := dockerfileTemplateData{
		bamlVersion:       opts.BAMLVersion,
		adapterVersion:    opts.AdapterVersion,
		keepSource:        opts.KeepSource,
		debugBuild:        true,            // Enable debug endpoints for testing (/_debug/*)
		defaultTargetArch: getDockerArch(), // Use native architecture
		noCacheMount:      true,            // testcontainers doesn't reliably support BuildKit
		noCustomBamlLib:   true,            // Integration tests don't use custom BAML lib
		unaryServer:       opts.UnaryServer,
		inProcess:         opts.InProcess,
	}

	if opts.BAMLSource != "" {
		tmplData.bamlSource = true
		tmplData.prebuiltCffi = opts.PrebuiltCffiDir != ""
		protocGenGoVersion, err := detectProtocGenGoVersion(opts.BAMLSource)
		if err != nil {
			return nil, fmt.Errorf("failed to detect protoc-gen-go version: %w", err)
		}
		tmplData.protocGenGoVersion = protocGenGoVersion
	}

	var dockerfileBuf bytes.Buffer
	if err := tmpl.Execute(&dockerfileBuf, tmplData.toMap()); err != nil {
		return nil, fmt.Errorf("failed to execute Dockerfile template: %w", err)
	}

	// Add Dockerfile
	dockerfile := dockerfileBuf.Bytes()
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0644,
		Size: int64(len(dockerfile)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(dockerfile); err != nil {
		return nil, err
	}

	// Get build.sh from embedded sources
	buildScript, err := getBuildScript()
	if err != nil {
		return nil, fmt.Errorf("failed to get build script: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "build.sh",
		Mode: 0755,
		Size: int64(len(buildScript)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(buildScript); err != nil {
		return nil, err
	}

	// Add baml_rest sources from embedded FS
	for path, source := range bamlrest.Sources {
		if err := copyFSToTar(source, tw, "baml_rest/"+path); err != nil {
			return nil, fmt.Errorf("failed to copy baml_rest source %s: %w", path, err)
		}
	}

	// Add baml_src directory
	if err := addDirToTar(tw, opts.BAMLSrcPath, "baml_src"); err != nil {
		return nil, fmt.Errorf("failed to add baml_src: %w", err)
	}

	// Add empty custom_baml_go_lib directory (placeholder)
	if err := tw.WriteHeader(&tar.Header{
		Name: "custom_baml_go_lib/.placeholder",
		Mode: 0644,
		Size: 0,
	}); err != nil {
		return nil, err
	}

	// Add BAML source to build context for --baml-source builds
	if opts.BAMLSource != "" {
		if opts.PrebuiltCffiDir != "" {
			// Prebuilt path: inject the cffi artifacts built once in CI and
			// skip taring the full engine/ tree (the cffi-builder cargo stage
			// is gone). language_client_go is plain Go source still required
			// for the module replace directive, so tar just that subtree.
			soSrc := filepath.Join(opts.PrebuiltCffiDir, "libbaml_cffi.so")
			if err := addFileToTar(tw, soSrc, "prebuilt/libbaml_cffi.so"); err != nil {
				return nil, fmt.Errorf("failed to add prebuilt libbaml_cffi.so: %w", err)
			}
			cliSrc := filepath.Join(opts.PrebuiltCffiDir, "baml-cli")
			if err := addFileToTar(tw, cliSrc, "prebuilt/baml-cli"); err != nil {
				return nil, fmt.Errorf("failed to add prebuilt baml-cli: %w", err)
			}

			lcgDir := filepath.Join(opts.BAMLSource, "engine", "language_client_go")
			if err := addDirToTarExclude(tw, lcgDir, "baml_engine/language_client_go", map[string]bool{
				"target": true, ".git": true, "node_modules": true,
			}); err != nil {
				return nil, fmt.Errorf("failed to copy language_client_go source: %w", err)
			}
		} else {
			// Copy engine directory (for Rust CFFI/CLI build), excluding large/irrelevant dirs
			engineDir := filepath.Join(opts.BAMLSource, "engine")
			if err := addDirToTarExclude(tw, engineDir, "baml_engine", map[string]bool{
				"target": true, ".git": true, "node_modules": true,
			}); err != nil {
				return nil, fmt.Errorf("failed to copy BAML engine source: %w", err)
			}
		}

		// Copy go.mod and go.sum from repo root (needed for Go module replace directive)
		for _, fileName := range []string{"go.mod", "go.sum"} {
			filePath := filepath.Join(opts.BAMLSource, fileName)
			fileData, err := os.ReadFile(filePath)
			if err != nil {
				if os.IsNotExist(err) && fileName == "go.sum" {
					continue // go.sum might not exist
				}
				return nil, fmt.Errorf("failed to read BAML source %s: %w", fileName, err)
			}
			if err := tw.WriteHeader(&tar.Header{
				Name: "baml_" + fileName,
				Mode: 0644,
				Size: int64(len(fileData)),
			}); err != nil {
				return nil, err
			}
			if _, err := tw.Write(fileData); err != nil {
				return nil, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf.Bytes()), nil
}

func getBuildScript() ([]byte, error) {
	// The build script is in cmd/build/build.sh within the root embedded FS
	rootFS, ok := bamlrest.Sources["."]
	if !ok {
		return nil, fmt.Errorf("root source not found in embedded sources")
	}

	return fs.ReadFile(rootFS, "cmd/build/build.sh")
}

func getDockerfileTemplate() ([]byte, error) {
	// The Dockerfile template is in cmd/build/Dockerfile.tmpl within the root embedded FS
	rootFS, ok := bamlrest.Sources["."]
	if !ok {
		return nil, fmt.Errorf("root source not found in embedded sources")
	}

	return fs.ReadFile(rootFS, "cmd/build/Dockerfile.tmpl")
}

func copyFSToTar(fsys fs.FS, tw *tar.Writer, prefix string) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		file, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		targetPath := prefix
		if path != "." {
			targetPath = prefix + "/" + path
		}

		header := &tar.Header{
			Name: targetPath,
			Mode: int64(info.Mode()),
			Size: info.Size(),
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		_, err = io.Copy(tw, file)
		return err
	})
}

func addFileToTar(tw *tar.Writer, srcPath, dstPath string) error {
	file, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name: dstPath,
		Mode: int64(info.Mode()),
		Size: info.Size(),
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err = io.Copy(tw, file)
	return err
}

func addDirToTar(tw *tar.Writer, srcDir, dstPrefix string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		dstPath := dstPrefix + "/" + relPath

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		header := &tar.Header{
			Name: dstPath,
			Mode: int64(info.Mode()),
			Size: info.Size(),
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		_, err = io.Copy(tw, file)
		return err
	})
}

func addDirToTarExclude(tw *tar.Writer, srcDir, dstPrefix string, excludeDirs map[string]bool) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != srcDir && excludeDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		dstPath := dstPrefix + "/" + relPath

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		header := &tar.Header{
			Name: dstPath,
			Mode: int64(info.Mode()),
			Size: info.Size(),
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		_, err = io.Copy(tw, file)
		return err
	})
}

// DetectBamlSourceVersion reads the BAML version from the source repository's engine/Cargo.toml.
func DetectBamlSourceVersion(bamlSource string) (string, error) {
	cargoToml := filepath.Join(bamlSource, "engine", "Cargo.toml")
	content, err := os.ReadFile(cargoToml)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", cargoToml, err)
	}

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "version") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				version := strings.TrimSpace(parts[1])
				version = strings.Trim(version, "\"'")
				if version != "" {
					return version, nil
				}
			}
		}
	}

	return "", fmt.Errorf("version not found in %s", cargoToml)
}

// detectProtocGenGoVersion finds the protoc-gen-go version from generated .pb.go files in the BAML source.
func detectProtocGenGoVersion(bamlSource string) (string, error) {
	pbDir := filepath.Join(bamlSource, "engine", "language_client_go", "pkg", "cffi")

	entries, err := os.ReadDir(pbDir)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", pbDir, err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".pb.go") {
			continue
		}

		content, err := os.ReadFile(filepath.Join(pbDir, entry.Name()))
		if err != nil {
			continue
		}

		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//") && strings.Contains(line, "protoc-gen-go v") {
				idx := strings.Index(line, "protoc-gen-go v")
				if idx >= 0 {
					return strings.TrimSpace(line[idx+len("protoc-gen-go "):]), nil
				}
			}
		}
	}

	return "", fmt.Errorf("no .pb.go files with protoc-gen-go version found in %s", pbDir)
}

func findProjectRoot() (string, error) {
	// Start from current working directory and look for go.mod
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find project root (no go.mod found)")
		}
		dir = parent
	}
}

// GetAdapterVersionForBAML returns the appropriate adapter version path for a BAML version.
func GetAdapterVersionForBAML(bamlVersion string) (string, error) {
	adapterInfo, err := bamlutils.GetAdapterForBAMLVersion(bamlrest.Sources, bamlVersion)
	if err != nil {
		return "", err
	}
	return adapterInfo.Path, nil
}

// getDockerArch returns the Docker-style architecture name for the current platform.
func getDockerArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "amd64":
		return "amd64"
	default:
		return "amd64" // fallback
	}
}
