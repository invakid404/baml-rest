package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/moby/moby/api/types/build"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

//go:embed Dockerfile
var dockerfile []byte

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

		err = filepath.WalkDir(bamlSrcPath, func(path string, dirEntry os.DirEntry, err error) error {
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

			baseName := fileInfo.Name()
			if !strings.HasSuffix(baseName, ".baml") {
				return nil
			}

			fmt.Printf("Writing %s\n", baseName)

			fileHeader := tar.Header{
				Name: fmt.Sprintf("baml_src/%s", path[len(bamlSrcPath)+1:]),
				Mode: 0644,
				Size: fileInfo.Size(),
			}
			if err := tarWriter.WriteHeader(&fileHeader); err != nil {
				return fmt.Errorf("failed to write file header for %s: %w", baseName, err)
			}

			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open %s: %w", path, err)
			}
			defer func(file *os.File) {
				_ = file.Close()
			}(file)

			if _, err = io.Copy(tarWriter, file); err != nil {
				return fmt.Errorf("failed to copy %s to build context: %w", baseName, err)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to walk baml_src: %w", err)
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

		var data map[string]any
		for scanner.Scan() {
			line := scanner.Text()
			err := json.Unmarshal([]byte(line), &data)
			if err != nil {
				continue
			}

			if stream, ok := data["stream"].(string); ok {
				fmt.Print(stream)
			}
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
