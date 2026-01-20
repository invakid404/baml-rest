//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestEnv holds the shared test environment
var TestEnv *testutil.TestEnvironment

// MockClient is the client for registering scenarios
var MockClient *mockllm.Client

// BAMLClient is the client for calling baml-rest
var BAMLClient *testutil.BAMLRestClient

// BAMLVersion is the version being tested
const BAMLVersion = "0.214.0"

// GoroutineLeakFilter contains comma-separated patterns for detecting goroutine leaks
// in our code. Used by leak detection tests to filter pprof data. Case-insensitive.
// Covers: baml-rest (github.com/invakid404/baml-rest) and BAML (github.com/boundaryml/baml)
const GoroutineLeakFilter = "invakid404/baml-rest,boundaryml/baml"

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Find the testdata directory
	bamlSrcPath, err := findTestdataPath()
	if err != nil {
		println("Failed to find testdata:", err.Error())
		os.Exit(1)
	}

	// Get the appropriate adapter version
	adapterVersion, err := testutil.GetAdapterVersionForBAML(BAMLVersion)
	if err != nil {
		println("Failed to get adapter version:", err.Error())
		os.Exit(1)
	}

	println("Setting up test environment...")
	println("  BAML Version:", BAMLVersion)
	println("  Adapter Version:", adapterVersion)
	println("  BAML Src Path:", bamlSrcPath)

	// Setup test environment
	TestEnv, err = testutil.Setup(ctx, testutil.SetupOptions{
		BAMLSrcPath:    bamlSrcPath,
		BAMLVersion:    BAMLVersion,
		AdapterVersion: adapterVersion,
	})
	if err != nil {
		println("Failed to setup test environment:", err.Error())
		os.Exit(1)
	}

	println("Test environment ready:")
	println("  Mock LLM URL:", TestEnv.MockLLMURL)
	println("  Mock LLM Internal URL:", TestEnv.MockLLMInternal)
	println("  BAML REST URL:", TestEnv.BAMLRestURL)

	// Create clients
	MockClient = mockllm.NewClient(TestEnv.MockLLMURL)
	BAMLClient = testutil.NewBAMLRestClient(TestEnv.BAMLRestURL)

	// Run tests
	code := m.Run()

	// Cleanup
	println("Tearing down test environment...")
	if err := TestEnv.Terminate(context.Background()); err != nil {
		println("Failed to terminate test environment:", err.Error())
	}

	os.Exit(code)
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

// Helper to register a scenario and create client options
func setupScenario(t *testing.T, scenarioID, content string) *testutil.BAMLOptions {
	t.Helper()

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

// Helper to register a non-streaming scenario
func setupNonStreamingScenario(t *testing.T, scenarioID, content string) *testutil.BAMLOptions {
	t.Helper()

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
