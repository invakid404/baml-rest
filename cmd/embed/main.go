package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/stoewer/go-strcase"
	"golang.org/x/mod/modfile"
)

//go:embed embed.go.tmpl
var embedTemplateInput string

const outputName = "embed.go"
const ignoreFileName = ".embedignore"

type ImportEntry struct {
	Module       string
	PkgName      string
	RelativePath string
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	var roots []string

	err = filepath.WalkDir(cwd, func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if filepath.Base(path) == "go.mod" {
			roots = append(roots, filepath.Dir(path))
		}

		return nil
	})
	if err != nil {
		panic(err)
	}

	tree := buildPathTree(roots)
	rootPatterns := loadIgnorePatterns(cwd)

	for _, root := range roots {
		patterns := rootPatterns
		if root != cwd {
			modulePatterns := loadIgnorePatterns(root)
			patterns = append(rootPatterns, modulePatterns...)
		}

		paths := collectEmbedPaths(root, patterns, roots)

		var imports []*ImportEntry

		for _, otherRoot := range tree[root] {
			currentImport, err := func() (*ImportEntry, error) {
				nestedModuleFile, err := os.Open(filepath.Join(otherRoot, "go.mod"))
				if err != nil {
					return nil, err
				}

				defer func(nestedModuleFile *os.File) {
					_ = nestedModuleFile.Close()
				}(nestedModuleFile)

				nestedModuleContent, err := io.ReadAll(nestedModuleFile)
				if err != nil {
					return nil, err
				}

				parsedModule, err := modfile.Parse("go.mod", nestedModuleContent, nil)
				if err != nil {
					return nil, err
				}

				relativePath, err := filepath.Rel(root, otherRoot)
				if err != nil {
					return nil, err
				}

				return &ImportEntry{
					Module: strconv.Quote(parsedModule.Module.Mod.Path),
					PkgName: strings.ReplaceAll(
						filepath.Base(parsedModule.Module.Mod.Path),
						".",
						"_",
					),
					RelativePath: strconv.Quote(relativePath),
				}, nil
			}()

			if err != nil {
				panic(err)
			}

			imports = append(imports, currentImport)
		}

		embedTemplate := template.Must(template.New("embed.go").Parse(embedTemplateInput))
		input := map[string]any{
			"Package": strings.ReplaceAll(strcase.SnakeCase(filepath.Base(root)), ".", "_"),
			"Dirs":    strings.Join(paths, " "),
			"Imports": imports,
		}

		var embedOutput bytes.Buffer
		if err = embedTemplate.Execute(&embedOutput, input); err != nil {
			panic(err)
		}

		if err = os.WriteFile(filepath.Join(root, outputName), embedOutput.Bytes(), 0644); err != nil {
			panic(err)
		}
	}
}

func buildPathTree(paths []string) map[string][]string {
	tree := make(map[string][]string)
	for _, path := range paths {
		tree[path] = nil
	}

	for _, path := range paths {
		current := path
		for current != "" && current != "." && current != "/" {
			current = filepath.Dir(current)
			if _, exists := tree[current]; exists {
				tree[current] = append(tree[current], path)
				break
			}
		}
	}

	return tree
}

func loadIgnorePatterns(root string) []string {
	var patterns []string

	file, err := os.Open(filepath.Join(root, ignoreFileName))
	if err != nil {
		return patterns
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}

	return patterns
}

func collectEmbedPaths(root string, patterns []string, moduleRoots []string) []string {
	var paths []string

	entries, err := os.ReadDir(root)
	if err != nil {
		panic(err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if name[0] == '.' {
			continue
		}

		absolutePath := filepath.Join(root, name)
		if slices.Contains(moduleRoots, absolutePath) {
			continue
		}

		if isIgnored(name, patterns) {
			continue
		}

		if !entry.IsDir() {
			paths = append(paths, name)
			continue
		}

		// For directories, recursively check if any descendant file
		// matches an ignore pattern. Only expand the specific subtrees
		// that contain ignored files; keep clean subtrees as directory
		// names so the go:embed directive stays minimal.
		resolved, hasIgnored := resolveDir(root, absolutePath, patterns, moduleRoots)
		if hasIgnored {
			paths = append(paths, resolved...)
		} else {
			paths = append(paths, name)
		}
	}

	return paths
}

// resolveDir recursively resolves a directory into embed paths. If the
// directory contains no ignored files at any depth, it returns hasIgnored=false
// and the caller can use the directory name as-is. When ignored files exist,
// it returns the minimal set of paths: subdirectories without ignored files
// are kept as directory names, while subdirectories with ignored files are
// expanded further. Directories that are Go module roots are skipped entirely,
// as go:embed automatically excludes directories containing go.mod files.
func resolveDir(root string, dir string, patterns []string, moduleRoots []string) (paths []string, hasIgnored bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip hidden and underscore-prefixed entries
		// (consistent with go:embed directory behavior)
		if name[0] == '.' || name[0] == '_' {
			continue
		}

		absolutePath := filepath.Join(dir, name)

		// Skip directories that are Go module roots; go:embed
		// automatically excludes directories containing go.mod.
		if entry.IsDir() && slices.Contains(moduleRoots, absolutePath) {
			continue
		}

		relPath, err := filepath.Rel(root, absolutePath)
		if err != nil {
			panic(err)
		}
		relPath = filepath.ToSlash(relPath)

		if isIgnored(relPath, patterns) {
			hasIgnored = true
			continue
		}

		if !entry.IsDir() {
			paths = append(paths, relPath)
			continue
		}

		subPaths, subHasIgnored := resolveDir(root, absolutePath, patterns, moduleRoots)
		if subHasIgnored {
			hasIgnored = true
			paths = append(paths, subPaths...)
		} else {
			paths = append(paths, relPath)
		}
	}

	return
}

func isIgnored(path string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := doublestar.Match(pattern, path)
		if err == nil && matched {
			return true
		}
	}
	return false
}
