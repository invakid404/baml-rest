//go:build integration

package testutil

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	bamlrest "github.com/invakid404/baml-rest"
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
)

// TestEnvironment holds the running test containers and network.
type TestEnvironment struct {
	Network         *testcontainers.DockerNetwork
	MockLLM         testcontainers.Container
	BAMLRest        testcontainers.Container
	MockLLMURL      string // URL to reach mock server from host (http://localhost:xxxxx)
	BAMLRestURL     string // URL to reach baml-rest from host (http://localhost:xxxxx)
	MockLLMInternal string // URL to reach mock server from baml-rest (http://mockllm:8080)
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
}

// Setup creates the test environment with mock LLM server and baml-rest container.
func Setup(ctx context.Context, opts SetupOptions) (*TestEnvironment, error) {
	env := &TestEnvironment{}

	// Create Docker network
	net, err := network.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker network: %w", err)
	}
	env.Network = net

	// Start mock LLM server
	mockLLM, err := startMockLLMContainer(ctx, net.Name)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to start mock LLM container: %w", err)
	}
	env.MockLLM = mockLLM

	// Get mock LLM mapped port
	mockPort, err := mockLLM.MappedPort(ctx, MockLLMInternalPort)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to get mock LLM port: %w", err)
	}
	mockHost, err := mockLLM.Host(ctx)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to get mock LLM host: %w", err)
	}
	env.MockLLMURL = fmt.Sprintf("http://%s:%s", mockHost, mockPort.Port())
	env.MockLLMInternal = fmt.Sprintf("http://%s:8080", MockLLMContainerName)

	// Build and start baml-rest container
	bamlRest, err := startBAMLRestContainer(ctx, net.Name, opts)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to start baml-rest container: %w", err)
	}
	env.BAMLRest = bamlRest

	// Get baml-rest mapped port
	restPort, err := bamlRest.MappedPort(ctx, BAMLRestInternalPort)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to get baml-rest port: %w", err)
	}
	restHost, err := bamlRest.Host(ctx)
	if err != nil {
		_ = env.Terminate(ctx)
		return nil, fmt.Errorf("failed to get baml-rest host: %w", err)
	}
	env.BAMLRestURL = fmt.Sprintf("http://%s:%s", restHost, restPort.Port())

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
		},
		ExposedPorts: []string{MockLLMInternalPort},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {MockLLMContainerName},
		},
		WaitingFor: wait.ForHTTP("/_admin/health").WithPort(MockLLMInternalPort).WithStartupTimeout(30 * time.Second),
	}

	return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
}

func createMockLLMBuildContext() (io.ReadSeeker, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Get the project root (parent of integration directory)
	projectRoot, err := findProjectRoot()
	if err != nil {
		return nil, err
	}

	// Add integration/mockllm directory (contains its own go.mod)
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

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			ContextArchive: buildCtx,
			Dockerfile:     "Dockerfile",
		},
		ExposedPorts: []string{BAMLRestInternalPort},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {BAMLRestContainerName},
		},
		Env: map[string]string{
			"BAML_LOG": "off",
		},
		WaitingFor: wait.ForHTTP("/openapi.json").WithPort(BAMLRestInternalPort).WithStartupTimeout(180 * time.Second),
	}

	return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
}

// dockerfileTemplateData maps SetupOptions to the template fields used by cmd/build/Dockerfile.tmpl
type dockerfileTemplateData struct {
	// Standard fields (lowercase to match template)
	bamlVersion    string
	adapterVersion string
	keepSource     string
	debugBuild     bool

	// Integration test specific flags
	defaultTargetArch string // Provide default for TARGETARCH (testcontainers may not set it)
	noCacheMount      bool   // Disable BuildKit cache mount (not supported in testcontainers)
	noCustomBamlLib   bool   // Don't copy custom_baml_lib.so (not used in tests)
}

// MarshalMap converts the template data to a map for template execution
func (d dockerfileTemplateData) toMap() map[string]any {
	return map[string]any{
		"bamlVersion":       d.bamlVersion,
		"adapterVersion":    d.adapterVersion,
		"keepSource":        d.keepSource,
		"debugBuild":        d.debugBuild,
		"defaultTargetArch": d.defaultTargetArch,
		"noCacheMount":      d.noCacheMount,
		"noCustomBamlLib":   d.noCustomBamlLib,
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
		debugBuild:        false,
		defaultTargetArch: "amd64", // Provide default since testcontainers may not set TARGETARCH
		noCacheMount:      true,    // testcontainers doesn't reliably support BuildKit
		noCustomBamlLib:   true,    // Integration tests don't use custom BAML lib
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
	const adapterPrefix = "adapters/adapter_v"

	// Collect available adapter versions
	var versions []string
	for key := range bamlrest.Sources {
		if strings.HasPrefix(key, adapterPrefix) {
			versions = append(versions, key)
		}
	}

	if len(versions) == 0 {
		return "", fmt.Errorf("no adapter versions found")
	}

	// For simplicity, use a basic version comparison
	// In production, you'd use semver comparison
	targetVersion := "v" + bamlVersion

	// Find the highest adapter version that's <= the target BAML version
	var selected string
	for _, v := range versions {
		adapterVersion := "v" + strings.ReplaceAll(strings.TrimPrefix(v, adapterPrefix), "_", ".")
		if adapterVersion <= targetVersion {
			if selected == "" || adapterVersion > strings.ReplaceAll(strings.TrimPrefix(selected, adapterPrefix), "_", ".") {
				selected = v
			}
		}
	}

	if selected == "" {
		return "", fmt.Errorf("no compatible adapter found for BAML version %s", bamlVersion)
	}

	return selected, nil
}
