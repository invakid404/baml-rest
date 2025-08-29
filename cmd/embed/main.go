package main

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/stoewer/go-strcase"
	"golang.org/x/mod/modfile"
)

//go:embed embed.go.tmpl
var embedTemplateInput string

const outputName = "embed.go"

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

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			panic(err)
		}

		var dirs []string
		var imports []*ImportEntry

		for _, entry := range entries {
			name := entry.Name()
			if name[0] == '.' {
				continue
			}

			absolutePath := filepath.Join(root, name)
			if slices.Contains(roots, absolutePath) {
				continue
			}

			dirs = append(dirs, name)
		}

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
			"Dirs":    strings.Join(dirs, " "),
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
