package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed introspected.go.tmpl
var introspectedTemplateInput string

func main() {
	var streamFile *ast.File

	err := filepath.WalkDir("baml_client", func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if streamFile != nil {
			return nil
		}

		if dirEntry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		streamVar := file.Scope.Lookup("Stream")
		if streamVar != nil {
			streamFile = file
		}

		return nil
	})

	if err != nil {
		panic(err)
	}

	if streamFile == nil {
		panic("stream file not found")
	}

	packageName := streamFile.Name.Name
	packageBaseName := packageName[strings.LastIndex(packageName, "/")+1:]

	introspectedTemplate := template.Must(template.New("introspected.go").Parse(introspectedTemplateInput))
	var introspectedTemplateOut bytes.Buffer

	introspectedTemplateArgs := map[string]any{
		"streamPackageName": packageBaseName,
		"streamPackagePath": fmt.Sprintf("github.com/invakid404/baml-rest/%s", packageName),
	}
	if err := introspectedTemplate.Execute(&introspectedTemplateOut, introspectedTemplateArgs); err != nil {
		panic(err)
	}

	if err := os.WriteFile("introspected.go", introspectedTemplateOut.Bytes(), 0644); err != nil {
		panic(err)
	}
}
