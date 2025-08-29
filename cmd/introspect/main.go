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
	"strconv"
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

	var streamFunctions []map[string]any

	for _, decl := range streamFile.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		receiver, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}

		receiverIdentifier, ok := receiver.X.(*ast.Ident)
		if !ok {
			continue
		}

		if receiverIdentifier.Name != "stream" {
			continue
		}

		output := make(map[string]any)
		output["name"] = funcDecl.Name.Name

		var args []string
		for _, param := range funcDecl.Type.Params.List {
			args = append(args, param.Names[0].Name)
		}
		// Remove context and variadic args
		output["args"] = args[1 : len(args)-1]

		streamFunctions = append(streamFunctions, output)
	}

	funcMap := template.FuncMap{
		"quoteAndJoin": func(input []string) string {
			quoted := make([]string, len(input))
			for i, s := range input {
				quoted[i] = strconv.Quote(s)
			}
			return strings.Join(quoted, ",")
		},
	}

	introspectedTemplate := template.Must(template.New("introspected.go").Funcs(funcMap).Parse(introspectedTemplateInput))
	var introspectedTemplateOut bytes.Buffer

	introspectedTemplateArgs := map[string]any{
		"streamPackageName": packageBaseName,
		"streamPackagePath": fmt.Sprintf("github.com/invakid404/baml-rest/%s", packageName),
		"streamMethods":     streamFunctions,
	}
	if err := introspectedTemplate.Execute(&introspectedTemplateOut, introspectedTemplateArgs); err != nil {
		panic(err)
	}

	if err := os.WriteFile("introspected/introspected.go", introspectedTemplateOut.Bytes(), 0644); err != nil {
		panic(err)
	}
}
