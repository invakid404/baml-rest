package main

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/protobuf/proto"
	"github.com/docker/buildx/util/progress"
	"github.com/goccy/go-json"
	controlapi "github.com/moby/buildkit/api/services/control"
	buildkitclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/opencontainers/go-digest"

	bamlrest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/registry"
	"golang.org/x/mod/semver"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//go:embed Dockerfile.tmpl
var dockerfileDockerTemplateInput string

//go:embed build.sh
var buildScript string

type fileWriter interface {
	WriteFile(name string, data io.Reader, size int64, mode int64) error
}

type tarWriter struct {
	tw *tar.Writer
}

func (t *tarWriter) WriteFile(name string, data io.Reader, size int64, mode int64) error {
	header := tar.Header{
		Name: name,
		Mode: mode,
		Size: size,
	}
	if err := t.tw.WriteHeader(&header); err != nil {
		return fmt.Errorf("failed to write file header for %s: %w", name, err)
	}
	if _, err := io.Copy(t.tw, data); err != nil {
		return fmt.Errorf("failed to copy %s: %w", name, err)
	}
	return nil
}

type diskWriter struct {
	baseDir string
}

func (d *diskWriter) WriteFile(name string, data io.Reader, _ int64, _ int64) error {
	targetPath := filepath.Join(d.baseDir, name)

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for %s: %w", targetPath, err)
	}

	outFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", targetPath, err)
	}
	defer func(outFile *os.File) {
		_ = outFile.Close()
	}(outFile)

	if _, err = io.Copy(outFile, data); err != nil {
		return fmt.Errorf("failed to copy %s: %w", targetPath, err)
	}

	return nil
}

type copyFSMapper func(path string, dirEntry fs.DirEntry, fileInfo fs.FileInfo) *string

func copyFS(dir fs.FS, writer fileWriter, mapper copyFSMapper) error {
	return fs.WalkDir(dir, ".", func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if dirEntry.IsDir() {
			return nil
		}

		fileInfo, err := dirEntry.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info for %s: %w", path, err)
		}

		name := mapper(path, dirEntry, fileInfo)
		if name == nil {
			return nil
		}

		file, err := dir.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		defer func(file fs.File) {
			_ = file.Close()
		}(file)

		return writer.WriteFile(*name, file, fileInfo.Size(), int64(fileInfo.Mode()))
	})
}

func copyDirToTar(path string, target *tar.Writer, mapper copyFSMapper) error {
	return copyFS(os.DirFS(path), &tarWriter{tw: target}, mapper)
}

func copyFSToTar(dir fs.FS, target *tar.Writer, mapper copyFSMapper) error {
	return copyFS(dir, &tarWriter{tw: target}, mapper)
}

func copyDirToDisk(path string, targetDir string, mapper copyFSMapper) error {
	return copyFS(os.DirFS(path), &diskWriter{baseDir: targetDir}, mapper)
}

func copyFSToDisk(dir fs.FS, targetDir string, mapper copyFSMapper) error {
	return copyFS(dir, &diskWriter{baseDir: targetDir}, mapper)
}

const (
	adapterPrefix = "adapters/adapter_v"
	bamlRestDir   = "baml_rest"
	bamlSrcDir    = "baml_src"
	bamlFileExt   = ".baml"
	bamlRestName  = "baml-rest"
)

var (
	targetImage string
	buildMode   string
	outputPath  string
	bamlVersion string
	keepSource  string
)

func init() {
	viper.SetConfigName(bamlRestName)
	viper.SetConfigType("toml")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("BAML_REST")

	replacer := strings.NewReplacer("-", "_", ".", "_")
	viper.SetEnvKeyReplacer(replacer)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			// Config file was found but another error was produced
			log.Printf("Error reading config file: %v", err)
		}
	}

	rootCmd.Flags().StringVarP(&buildMode, "mode", "m", "docker", "Build mode: 'docker' (container build) or 'native' (direct execution)")
	rootCmd.Flags().StringVarP(&targetImage, "target-image", "t", "", "Target image name and tag for the built Docker image (required for docker mode)")
	rootCmd.Flags().StringVarP(&outputPath, "output", "o", "", "Output path for the binary (native mode only, defaults to ./baml-rest)")
	rootCmd.Flags().StringVarP(&bamlVersion, "baml-version", "b", "", "Specific BAML version to use (bypasses automatic version detection)")
	rootCmd.Flags().StringVarP(&keepSource, "keep-source", "k", "", "Keep generated source files at specified path (default: /baml-rest-generated-src). Use --keep-source or --keep-source=<path>")
	rootCmd.Flags().Lookup("keep-source").NoOptDefVal = "/baml-rest-generated-src"

	_ = viper.BindPFlag("mode", rootCmd.Flags().Lookup("mode"))
	_ = viper.BindPFlag("target-image", rootCmd.Flags().Lookup("target-image"))
	_ = viper.BindPFlag("output", rootCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("baml-version", rootCmd.Flags().Lookup("baml-version"))
	_ = viper.BindPFlag("keep-source", rootCmd.Flags().Lookup("keep-source"))
}

var rootCmd = &cobra.Command{
	Use:   "baml-rest [directory]",
	Short: "Build a REST API server for your BAML project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Get configuration from Viper
		buildMode = viper.GetString("mode")
		targetImage = viper.GetString("target-image")
		outputPath = viper.GetString("output")
		bamlVersion = viper.GetString("baml-version")
		keepSource = viper.GetString("keep-source")

		// Validate mode
		if buildMode != "docker" && buildMode != "native" {
			return fmt.Errorf("invalid mode %q, must be 'docker' or 'native'", buildMode)
		}

		// Validate required flags based on mode
		if buildMode == "docker" && targetImage == "" {
			return fmt.Errorf("--target-image is required for docker mode")
		}

		// Set default output path for native mode
		if buildMode == "native" && outputPath == "" {
			outputPath = "./baml-rest"
		}

		targetDir := args[0]

		// Common setup: validate directory structure
		info, err := os.Stat(targetDir)
		if err != nil {
			return fmt.Errorf("failed to access directory %s: %w", targetDir, err)
		}

		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", targetDir)
		}

		bamlSrcPath := filepath.Join(targetDir, bamlSrcDir)
		bamlSrcInfo, err := os.Stat(bamlSrcPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("No baml_src folder found in %s\n", targetDir)
				return nil
			}
			return fmt.Errorf("failed to check for baml_src folder: %w", err)
		}

		if !bamlSrcInfo.IsDir() {
			fmt.Printf("Found baml_src in %s, but it's not a directory\n", targetDir)
			return nil
		}

		fmt.Printf("Found baml_src folder in %s\n", targetDir)

		// Common setup: detect BAML and adapter versions
		var availableAdapterVersions []string
		adapterVersionToPath := make(map[string]string)

		for key := range bamlrest.Sources {
			if !strings.HasPrefix(key, adapterPrefix) {
				continue
			}

			version := "v" + strings.ReplaceAll(strings.TrimPrefix(key, adapterPrefix), "_", ".")
			availableAdapterVersions = append(
				availableAdapterVersions,
				version,
			)
			adapterVersionToPath[version] = key
		}

		semver.Sort(availableAdapterVersions)

		var detectedVersion string
		var detectedVersionForComparison string

		if bamlVersion != "" {
			// Use the manually specified BAML version
			detectedVersion = bamlVersion
			detectedVersionForComparison = "v" + detectedVersion
			fmt.Printf("Using manually specified BAML version: %s\n", detectedVersion)
		} else {
			// Auto-detect BAML version
			detectedVersions, err := bamlutils.ParseVersions(os.DirFS(bamlSrcPath))
			if err != nil {
				return fmt.Errorf("failed to parse versions: %w", err)
			}

			if len(detectedVersions) == 0 {
				return fmt.Errorf("no BAML generators found in %q, cannot infer version", bamlSrcPath)
			}

			if len(detectedVersions) > 1 {
				return fmt.Errorf(
					"detected multiple BAML versions in %q: %v, cannot infer which one to use",
					bamlSrcPath, detectedVersions,
				)
			}

			detectedVersion = detectedVersions[0]
			detectedVersionForComparison = "v" + detectedVersion
		}

		adapterVersionToUse := ""

		for _, version := range slices.Backward(availableAdapterVersions) {
			if semver.Compare(detectedVersionForComparison, version) >= 0 {
				adapterVersionToUse = version
				break
			}
		}

		if adapterVersionToUse == "" {
			return fmt.Errorf(
				"BAML version %q is unsupported, the minimum supported version is %q",
				detectedVersion, availableAdapterVersions[0],
			)
		}

		fmt.Printf("BAML version: %s\n", detectedVersion)
		fmt.Printf("Adapter version: %s\n", adapterVersionToUse)
		fmt.Printf("Build mode: %s\n", buildMode)

		// Dispatch to appropriate build function
		if buildMode == "docker" {
			return buildDocker(bamlSrcPath, detectedVersion, adapterVersionToPath[adapterVersionToUse], keepSource)
		} else {
			return buildNative(bamlSrcPath, detectedVersion, adapterVersionToPath[adapterVersionToUse], keepSource)
		}
	},
}

func buildDocker(bamlSrcPath, bamlVersion, adapterVersion string, keepSource string) error {
	fmt.Printf("\n=== Docker Build Mode ===\n\n")

	fmt.Printf("Making docker client...\n")
	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("failed to connect to docker daemon: %w", err)
	}

	dockerVersion, err := dockerClient.ServerVersion(context.TODO(), client.ServerVersionOptions{})
	if err != nil {
		return fmt.Errorf("failed to get docker version: %w", err)
	}
	fmt.Printf("Connected to docker daemon version %s\n", dockerVersion.Version)

	var buf bytes.Buffer
	tarWriter := tar.NewWriter(&buf)

	// Use the new Docker template with build.sh
	dockerfileTemplate := template.Must(template.New("dockerfile").Parse(dockerfileDockerTemplateInput))
	var dockerfileOut bytes.Buffer

	dockerfileTemplateArgs := map[string]interface{}{
		"bamlVersion":    bamlVersion,
		"adapterVersion": adapterVersion,
		"keepSource":     keepSource,
	}
	if err = dockerfileTemplate.Execute(&dockerfileOut, dockerfileTemplateArgs); err != nil {
		return fmt.Errorf("failed to render Dockerfile template: %w", err)
	}
	dockerfile := dockerfileOut.Bytes()

	images, err := extractFromImages(&dockerfileOut)
	if err != nil {
		return fmt.Errorf("failed to extract images from Dockerfile: %w", err)
	}

	if err = pullImagesIfNeeded(dockerClient, images); err != nil {
		return fmt.Errorf("failed to pull images: %w", err)
	}

	dockerfileHeader := tar.Header{
		Name: "Dockerfile",
		Mode: 0644,
		Size: int64(len(dockerfile)),
	}
	if err := tarWriter.WriteHeader(&dockerfileHeader); err != nil {
		return fmt.Errorf("failed to write Dockerfile header to build context: %w", err)
	}

	if _, err := tarWriter.Write(dockerfile); err != nil {
		return fmt.Errorf("failed to write Dockerfile to build context: %w", err)
	}

	buildScriptHeader := tar.Header{
		Name: "build.sh",
		Mode: 0755,
		Size: int64(len(buildScript)),
	}
	if err := tarWriter.WriteHeader(&buildScriptHeader); err != nil {
		return fmt.Errorf("failed to write build.sh header to build context: %w", err)
	}

	if _, err := tarWriter.Write([]byte(buildScript)); err != nil {
		return fmt.Errorf("failed to write build.sh to build context: %w", err)
	}

	for path, source := range bamlrest.Sources {
		err = copyFSToTar(source, tarWriter, func(filePath string, dirEntry fs.DirEntry, _ fs.FileInfo) *string {
			result := filepath.Join(bamlRestDir, path, filePath)
			return &result
		})
		if err != nil {
			return fmt.Errorf("failed to copy baml_rest sources: %w", err)
		}
	}

	err = copyDirToTar(bamlSrcPath, tarWriter, func(path string, _ fs.DirEntry, fileInfo fs.FileInfo) *string {
		baseName := fileInfo.Name()
		if !strings.HasSuffix(baseName, bamlFileExt) {
			return nil
		}

		result := fmt.Sprintf("baml_src/%s", path)
		return &result
	})
	if err != nil {
		return fmt.Errorf("failed to copy target directory to build context: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to close build context writer: %w", err)
	}

	fmt.Printf("Building image...\n")
	response, err := dockerClient.ImageBuild(context.TODO(), &buf, client.ImageBuildOptions{
		Dockerfile: "Dockerfile",
		Tags:       []string{targetImage},
		Remove:     true,
		Version:    build.BuilderBuildKit,
		AuthConfigs: map[string]registry.AuthConfig{
			"docker.io": {},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}

	defer func(body io.ReadCloser) {
		_ = body.Close()
	}(response.Body)

	decoder := json.NewDecoder(response.Body)

	progressWriter, err := progress.NewPrinter(context.TODO(), os.Stdout, progressui.AutoMode)
	if err != nil {
		return fmt.Errorf("failed to create progress writer: %w", err)
	}

	// Track build errors and failed vertices
	var buildErrors []string
	var failedVertices []string
	vertexLogs := make(map[string][]string) // Map vertex digest to its logs

	for decoder.More() {
		var message dockerBuildMessage
		if err := decoder.Decode(&message); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "failed to parse build output: %v\n", err)
			continue
		}

		// Check for standard Docker error messages
		if message.Error != "" {
			buildErrors = append(buildErrors, message.Error)
		}
		if message.ErrorDetail.Message != "" {
			buildErrors = append(buildErrors, message.ErrorDetail.Message)
		}

		if message.ID == "moby.buildkit.trace" {
			aux, err := base64.StdEncoding.DecodeString(message.Aux.(string))
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "failed to decode aux output: %v\n", err)
				continue
			}

			var statusResponse controlapi.StatusResponse
			if err = proto.Unmarshal(aux, &statusResponse); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "failed to parse aux output: %v\n", err)
				continue
			}

			// Check for vertex errors
			for _, v := range statusResponse.GetVertexes() {
				if v != nil && v.GetError() != "" {
					vertexName := v.GetName()
					vertexError := v.GetError()
					if vertexName != "" {
						failedVertices = append(failedVertices, vertexName)
						buildErrors = append(buildErrors, fmt.Sprintf("%s: %s", vertexName, vertexError))
					} else {
						buildErrors = append(buildErrors, vertexError)
					}
				}
			}

			// Capture logs for each vertex
			for _, l := range statusResponse.GetLogs() {
				if l != nil {
					vertexDigest := l.GetVertex()
					logData := string(l.GetMsg())
					vertexLogs[vertexDigest] = append(vertexLogs[vertexDigest], logData)
				}
			}

			solveStatus := mapStatusResponseToSolveStatus(&statusResponse)

			progressWriter.Write(&solveStatus)
		}
	}

	// If there were any errors, report them
	if len(buildErrors) > 0 {
		fmt.Printf("\n✗ Docker build failed!\n\n")

		// Print failed vertices and their logs
		if len(failedVertices) > 0 {
			fmt.Printf("Failed steps:\n")
			for _, vertexName := range failedVertices {
				fmt.Printf("  - %s\n", vertexName)
			}
			fmt.Printf("\n")
		}

		// Print errors
		fmt.Printf("Errors:\n")
		for _, buildError := range buildErrors {
			fmt.Printf("  %s\n", buildError)
		}
		fmt.Printf("\n")

		// Print relevant logs if available
		if len(vertexLogs) > 0 {
			fmt.Printf("Build output:\n")
			for _, logs := range vertexLogs {
				for _, logEntry := range logs {
					fmt.Printf("%s", logEntry)
				}
			}
		}

		return errors.New("docker build failed")
	}

	fmt.Printf("\n✓ Docker build completed successfully!\n")
	fmt.Printf("Image: %s\n", targetImage)

	return nil
}

func buildNative(bamlSrcPath, bamlVersion, adapterVersion string, keepSource string) error {
	fmt.Printf("\n=== Native Build Mode ===\n\n")

	// Check prerequisites
	requiredCommands := []string{"node", "npx", "go", "gawk"}
	for _, cmd := range requiredCommands {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("required command %q not found in PATH. Please install it before using native mode", cmd)
		}
	}

	fmt.Printf("✓ All required tools are available\n")

	// Set up cache directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}

	cacheDir := filepath.Join(homeDir, ".cache", bamlRestName)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	fmt.Printf("Cache directory: %s\n", cacheDir)

	// Create temporary build context directory
	tmpDir, err := os.MkdirTemp("", "baml-rest-build-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func(path string) {
		_ = os.RemoveAll(path)
	}(tmpDir)

	// Write build.sh to temporary location
	buildScriptPath := filepath.Join(tmpDir, "build.sh")
	if err := os.WriteFile(buildScriptPath, []byte(buildScript), 0755); err != nil {
		return fmt.Errorf("failed to write build.sh: %w", err)
	}

	// Create build context directory structure
	buildContextDir := filepath.Join(tmpDir, "context")
	if err := os.MkdirAll(buildContextDir, 0755); err != nil {
		return fmt.Errorf("failed to create build context directory: %w", err)
	}

	// Write baml_rest sources to build context
	fmt.Printf("Writing baml_rest sources to build context...\n")
	for path, source := range bamlrest.Sources {
		if err := copyFSToDisk(source, buildContextDir, func(filePath string, _ fs.DirEntry, _ fs.FileInfo) *string {
			result := filepath.Join(bamlRestDir, path, filePath)
			return &result
		}); err != nil {
			return fmt.Errorf("failed to copy baml_rest sources: %w", err)
		}
	}

	// Copy user's baml_src to build context
	fmt.Printf("Copying baml_src to build context...\n")
	if err := copyDirToDisk(bamlSrcPath, buildContextDir, func(path string, _ fs.DirEntry, fileInfo fs.FileInfo) *string {
		baseName := fileInfo.Name()
		if !strings.HasSuffix(baseName, bamlFileExt) {
			return nil
		}
		result := filepath.Join(bamlSrcDir, path)
		return &result
	}); err != nil {
		return fmt.Errorf("failed to copy baml_src to build context: %w", err)
	}

	// Convert output path to absolute path to avoid ambiguity
	absOutputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for output: %w", err)
	}

	// Prepare environment variables
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("BAML_VERSION=%s", bamlVersion),
		fmt.Sprintf("ADAPTER_VERSION=%s", adapterVersion),
		fmt.Sprintf("USER_CONTEXT_PATH=%s", buildContextDir),
		fmt.Sprintf("OUTPUT_PATH=%s", absOutputPath),
		fmt.Sprintf("CACHE_DIR=%s", cacheDir),
	)
	if keepSource != "" {
		env = append(env, "KEEP_SOURCE=true", fmt.Sprintf("KEEP_SOURCE_DIR=%s", keepSource))
	}

	// Execute build.sh
	fmt.Printf("\nExecuting build script...\n\n")

	cmd := exec.Command("/bin/bash", buildScriptPath)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build script failed: %w", err)
	}

	fmt.Printf("\n✓ Native build completed successfully!\n")
	fmt.Printf("Binary: %s\n", outputPath)

	return nil
}

type dockerBuildMessage struct {
	ID          string `json:"id,omitempty"`
	Aux         any    `json:"aux,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorDetail struct {
		Message string `json:"message,omitempty"`
	} `json:"errorDetail,omitempty"`
	Stream string `json:"stream,omitempty"`
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}

func extractFromImages(dockerfileContent io.Reader) ([]string, error) {
	result, err := parser.Parse(dockerfileContent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Dockerfile: %w", err)
	}

	var images []string

	for _, child := range result.AST.Children {
		if strings.ToUpper(child.Value) == "FROM" {
			if child.Next != nil {
				image := extractImageFromNode(child.Next)
				if image != "" && !isStageReference(image) {
					images = append(images, normalizeImageName(image))
				}
			}
		}
	}

	return images, nil
}

func extractImageFromNode(node *parser.Node) string {
	var parts []string

	for n := node; n != nil; n = n.Next {
		if strings.ToUpper(n.Value) == "AS" {
			break
		}
		parts = append(parts, n.Value)
	}

	return strings.Join(parts, "")
}

func normalizeImageName(image string) string {
	// Handle scratch image (special case)
	if image == "scratch" {
		return image
	}

	// Add default tag if missing
	if !strings.Contains(image, ":") {
		image = image + ":latest"
	}

	// Add docker.io registry if no registry specified
	if !strings.Contains(strings.Split(image, "/")[0], ".") &&
		!strings.Contains(strings.Split(image, "/")[0], ":") &&
		!strings.HasPrefix(image, "localhost/") {

		// Check if it's an official image (no namespace)
		parts := strings.Split(image, "/")
		if len(parts) == 1 {
			image = "docker.io/library/" + image
		} else if len(parts) == 2 {
			image = "docker.io/" + image
		}
	}

	return image
}

func isStageReference(image string) bool {
	if image == "scratch" {
		return false
	}

	hasColon := strings.Contains(image, ":")
	hasSlash := strings.Contains(image, "/")

	return !hasColon && !hasSlash
}

func pullImagesIfNeeded(cli *client.Client, images []string) error {
	ctx := context.Background()

	for _, img := range images {
		// Skip scratch image
		if img == "scratch" {
			fmt.Printf("Skipping special image: %s\n", img)
			continue
		}

		// Check if image exists locally
		exists, err := imageExists(ctx, cli, img)
		if err != nil {
			return fmt.Errorf("failed to check image %s: %w", img, err)
		}

		if exists {
			fmt.Printf("Image already exists: %s\n", img)
		} else {
			fmt.Printf("Pulling image: %s\n", img)
			err = pullImage(ctx, cli, img)
			if err != nil {
				return fmt.Errorf("failed to pull image %s: %w", img, err)
			}
			fmt.Printf("Successfully pulled: %s\n", img)
		}
	}

	return nil
}

func imageExists(ctx context.Context, cli *client.Client, imageName string) (bool, error) {
	images, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return false, err
	}

	for _, img := range images.Items {
		for _, tag := range img.RepoTags {
			if tag == imageName ||
				strings.TrimPrefix(tag, "docker.io/") == strings.TrimPrefix(imageName, "docker.io/") ||
				"docker.io/library/"+tag == imageName {
				return true, nil
			}
		}
	}

	return false, nil
}

func pullImage(ctx context.Context, cli *client.Client, imageName string) error {
	// Pull the image
	reader, err := cli.ImagePull(ctx, imageName, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer func(reader io.ReadCloser) {
		_ = reader.Close()
	}(reader)

	// Process the output to show progress
	decoder := json.NewDecoder(reader)
	for {
		var message map[string]interface{}
		if err := decoder.Decode(&message); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// Print progress messages
		if status, ok := message["status"].(string); ok {
			if progressMessage, ok := message["progress"].(string); ok {
				fmt.Printf("  %s %s\n", status, progressMessage)
			} else {
				fmt.Printf("  %s\n", status)
			}
		}
	}

	return nil
}

// mapStatusResponseToSolveStatus converts a controlapi.StatusResponse to buildkitclient.SolveStatus
// This avoids the protobuf JSON serialization issue where int64 fields get serialized as strings
func mapStatusResponseToSolveStatus(statusResponse *controlapi.StatusResponse) buildkitclient.SolveStatus {
	if statusResponse == nil {
		return buildkitclient.SolveStatus{}
	}

	// Preallocate slices with necessary capacity
	vertexes := make([]*buildkitclient.Vertex, 0, len(statusResponse.GetVertexes()))
	statuses := make([]*buildkitclient.VertexStatus, 0, len(statusResponse.GetStatuses()))
	logs := make([]*buildkitclient.VertexLog, 0, len(statusResponse.GetLogs()))
	warnings := make([]*buildkitclient.VertexWarning, 0, len(statusResponse.GetWarnings()))

	// Map vertexes
	for _, v := range statusResponse.GetVertexes() {
		if v == nil {
			continue
		}

		vertex := &buildkitclient.Vertex{
			Digest:        digest.Digest(v.GetDigest()),
			Name:          v.GetName(),
			Cached:        v.GetCached(),
			Error:         v.GetError(),
			ProgressGroup: v.GetProgressGroup(),
		}

		// Convert inputs slice
		if inputs := v.GetInputs(); len(inputs) > 0 {
			vertex.Inputs = make([]digest.Digest, 0, len(inputs))
			for _, input := range inputs {
				vertex.Inputs = append(vertex.Inputs, digest.Digest(input))
			}
		}

		// Convert timestamps
		if started := v.GetStarted(); started != nil {
			t := time.Unix(started.GetSeconds(), int64(started.GetNanos())).UTC()
			vertex.Started = &t
		}
		if completed := v.GetCompleted(); completed != nil {
			t := time.Unix(completed.GetSeconds(), int64(completed.GetNanos())).UTC()
			vertex.Completed = &t
		}

		vertexes = append(vertexes, vertex)
	}

	// Map statuses
	for _, s := range statusResponse.GetStatuses() {
		if s == nil {
			continue
		}

		status := &buildkitclient.VertexStatus{
			ID:      s.GetID(),
			Vertex:  digest.Digest(s.GetVertex()),
			Name:    s.GetName(),
			Total:   s.GetTotal(),
			Current: s.GetCurrent(),
		}

		// Convert timestamps
		if timestamp := s.GetTimestamp(); timestamp != nil {
			status.Timestamp = time.Unix(timestamp.GetSeconds(), int64(timestamp.GetNanos())).UTC()
		}
		if started := s.GetStarted(); started != nil {
			t := time.Unix(started.GetSeconds(), int64(started.GetNanos())).UTC()
			status.Started = &t
		}
		if completed := s.GetCompleted(); completed != nil {
			t := time.Unix(completed.GetSeconds(), int64(completed.GetNanos())).UTC()
			status.Completed = &t
		}

		statuses = append(statuses, status)
	}

	// Map logs
	for _, l := range statusResponse.GetLogs() {
		if l == nil {
			continue
		}

		vertexLog := &buildkitclient.VertexLog{
			Vertex: digest.Digest(l.GetVertex()),
			Stream: int(l.GetStream()),
			Data:   l.GetMsg(),
		}

		// Convert timestamp
		if timestamp := l.GetTimestamp(); timestamp != nil {
			vertexLog.Timestamp = time.Unix(timestamp.GetSeconds(), int64(timestamp.GetNanos())).UTC()
		}

		logs = append(logs, vertexLog)
	}

	// Map warnings
	for _, w := range statusResponse.GetWarnings() {
		if w == nil {
			continue
		}

		warning := &buildkitclient.VertexWarning{
			Vertex:     digest.Digest(w.GetVertex()),
			Level:      int(w.GetLevel()),
			Short:      w.GetShort(),
			URL:        w.GetUrl(),
			SourceInfo: w.GetInfo(),
			Range:      w.GetRanges(),
		}

		// Convert detail slice
		if detail := w.GetDetail(); len(detail) > 0 {
			warning.Detail = make([][]byte, 0, len(detail))
			for _, d := range detail {
				warning.Detail = append(warning.Detail, d)
			}
		}

		warnings = append(warnings, warning)
	}

	return buildkitclient.SolveStatus{
		Vertexes: vertexes,
		Statuses: statuses,
		Logs:     logs,
		Warnings: warnings,
	}
}
