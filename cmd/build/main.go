package main

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"os"
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
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/registry"
	"golang.org/x/mod/semver"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

//go:embed Dockerfile.tmpl
var dockerfileTemplateInput string

//go:embed clients.baml.template
var clientsBamlTemplate []byte

func copyDirToTar(path string, target *tar.Writer, mapper copyFSToTarMapper) error {
	return copyFSToTar(os.DirFS(path), target, mapper)
}

func copyFSToTar(dir fs.FS, target *tar.Writer, mapper copyFSToTarMapper) error {
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

		fileHeader := tar.Header{
			Name: *name,
			Mode: 0644,
			Size: fileInfo.Size(),
		}
		if err := target.WriteHeader(&fileHeader); err != nil {
			return fmt.Errorf("failed to write file header for %s: %w", *name, err)
		}

		file, err := dir.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		defer func(file fs.File) {
			_ = file.Close()
		}(file)

		if _, err = io.Copy(target, file); err != nil {
			return fmt.Errorf("failed to copy %s: %w", *name, err)
		}

		return nil
	})
}

type copyFSToTarMapper func(path string, dirEntry fs.DirEntry, fileInfo fs.FileInfo) *string

const (
	adapterPrefix = "adapters/v"
)

var rootCmd = &cobra.Command{
	Use:   "baml-rest [directory]",
	Short: "Build a REST API server for your BAML project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		targetDir := args[0]

		info, err := os.Stat(targetDir)
		if err != nil {
			return fmt.Errorf("failed to access directory %s: %w", targetDir, err)
		}

		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", targetDir)
		}

		bamlSrcPath := filepath.Join(targetDir, "baml_src")
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

		var availableAdapterVersions []string
		for key := range bamlrest.Sources {
			if !strings.HasPrefix(key, adapterPrefix) {
				continue
			}

			availableAdapterVersions = append(
				availableAdapterVersions,
				"v"+strings.TrimPrefix(key, adapterPrefix),
			)
		}

		semver.Sort(availableAdapterVersions)

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

		detectedVersion := detectedVersions[0]
		detectedVersionForComparison := "v" + detectedVersion

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

		fmt.Println("Adapter version to use:", adapterVersionToUse)

		fmt.Println("Making docker client...")
		dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("failed to connect to docker daemon: %w", err)
		}

		dockerVersion, err := dockerClient.ServerVersion(context.TODO())
		if err != nil {
			return fmt.Errorf("failed to get docker version: %w", err)
		}
		fmt.Printf("Connected to docker daemon version %s\n", dockerVersion.Version)

		var buf bytes.Buffer
		tarWriter := tar.NewWriter(&buf)

		dockerfileTemplate := template.Must(template.New("dockerfile").Parse(dockerfileTemplateInput))
		var dockerfileOut bytes.Buffer

		dockerfileTemplateArgs := map[string]string{
			"bamlVersion":    detectedVersion,
			"adapterVersion": adapterVersionToUse,
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

		clientsBamlTemplateHeader := tar.Header{
			Name: "clients.baml.template",
			Mode: 0644,
			Size: int64(len(clientsBamlTemplate)),
		}
		if err := tarWriter.WriteHeader(&clientsBamlTemplateHeader); err != nil {
			return fmt.Errorf("failed to write clients.baml template header to build context: %w", err)
		}

		if _, err := tarWriter.Write(clientsBamlTemplate); err != nil {
			return fmt.Errorf("failed to write clients.baml template to build context: %w", err)
		}

		for path, source := range bamlrest.Sources {
			err = copyFSToTar(source, tarWriter, func(filePath string, dirEntry fs.DirEntry, _ fs.FileInfo) *string {
				result := filepath.Join("baml_rest", path, filePath)
				return &result
			})
		}

		err = copyDirToTar(bamlSrcPath, tarWriter, func(path string, _ fs.DirEntry, fileInfo fs.FileInfo) *string {
			baseName := fileInfo.Name()
			if !strings.HasSuffix(baseName, ".baml") {
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

		fmt.Println("Building image")
		response, err := dockerClient.ImageBuild(context.TODO(), &buf, build.ImageBuildOptions{
			Dockerfile: "Dockerfile",
			Tags:       []string{"testis:latest"},
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

		// Use a JSON decoder to read the response stream
		decoder := json.NewDecoder(response.Body)

		progressWriter, err := progress.NewPrinter(context.TODO(), os.Stdout, progressui.AutoMode)
		if err != nil {
			return fmt.Errorf("failed to create progress writer: %w", err)
		}

		for decoder.More() {
			var message dockerBuildMessage
			if err := decoder.Decode(&message); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "failed to parse build output: %v\n", err)
				continue
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

				solveStatus := mapStatusResponseToSolveStatus(&statusResponse)

				progressWriter.Write(&solveStatus)
			}
		}

		return nil
	},
}

type dockerBuildMessage struct {
	ID  string `json:"id,omitempty"`
	Aux any    `json:"aux,omitempty"`
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
	images, err := cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return false, err
	}

	for _, img := range images {
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
	reader, err := cli.ImagePull(ctx, imageName, image.PullOptions{})
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
			if progress, ok := message["progress"].(string); ok {
				fmt.Printf("  %s %s\n", status, progress)
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

		log := &buildkitclient.VertexLog{
			Vertex: digest.Digest(l.GetVertex()),
			Stream: int(l.GetStream()),
			Data:   l.GetMsg(),
		}

		// Convert timestamp
		if timestamp := l.GetTimestamp(); timestamp != nil {
			log.Timestamp = time.Unix(timestamp.GetSeconds(), int64(timestamp.GetNanos())).UTC()
		}

		logs = append(logs, log)
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
