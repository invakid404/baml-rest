package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"github.com/invakid404/baml-rest"
	"github.com/moby/moby/api/types/build"
	"html/template"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

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
			// TODO: unhardcode
			"bamlVersion": "0.204.0",
		}
		if err = dockerfileTemplate.Execute(&dockerfileOut, dockerfileTemplateArgs); err != nil {
			return fmt.Errorf("failed to render Dockerfile template: %w", err)
		}
		dockerfile := dockerfileOut.Bytes()

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

		err = copyFSToTar(baml_rest.Source, tarWriter, func(filePath string, dirEntry fs.DirEntry, _ fs.FileInfo) *string {
			result := path.Join("baml_rest", filePath)
			return &result
		})
		if err != nil {
			return fmt.Errorf("failed to copy source code to build context: %w", err)
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
		})
		if err != nil {
			return fmt.Errorf("failed to build image: %w", err)
		}

		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(response.Body)

		// Use a scanner to read the response stream line by line
		scanner := bufio.NewScanner(response.Body)

		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println(line)
		}
		if err := scanner.Err(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "reading build output failed: %v\n", err)
		}

		return nil
	},
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}
