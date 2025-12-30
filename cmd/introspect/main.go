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

type parsedFile struct {
	file *ast.File
	path string
}

func main() {
	var (
		streamFile      *parsedFile
		parseFile       *parsedFile
		parseStreamFile *parsedFile
		syncFuncsFile   *parsedFile
	)

	err := filepath.WalkDir("baml_client", func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if dirEntry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		// Look for Stream variable (functions_stream.go)
		if streamFile == nil {
			if streamVar := file.Scope.Lookup("Stream"); streamVar != nil {
				streamFile = &parsedFile{file: file, path: path}
			}
		}

		// Look for Parse variable (functions_parse.go)
		if parseFile == nil {
			if parseVar := file.Scope.Lookup("Parse"); parseVar != nil {
				parseFile = &parsedFile{file: file, path: path}
			}
		}

		// Look for ParseStream variable (functions_parse_stream.go)
		if parseStreamFile == nil {
			if parseStreamVar := file.Scope.Lookup("ParseStream"); parseStreamVar != nil {
				parseStreamFile = &parsedFile{file: file, path: path}
			}
		}

		// Look for sync functions in functions.go (package-level functions with context.Context first param)
		// We identify this file by having package-level functions but no Stream/Parse/ParseStream vars
		// Actually, functions.go doesn't have these vars, but we need a better heuristic
		// Let's look for package-level functions that take context.Context as first param
		if syncFuncsFile == nil {
			for _, decl := range file.Decls {
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				// Skip methods (functions with receivers)
				if funcDecl.Recv != nil {
					continue
				}
				// Check if first param is context.Context
				if funcDecl.Type.Params == nil || len(funcDecl.Type.Params.List) == 0 {
					continue
				}
				firstParam := funcDecl.Type.Params.List[0]
				if selectorExpr, ok := firstParam.Type.(*ast.SelectorExpr); ok {
					if ident, ok := selectorExpr.X.(*ast.Ident); ok {
						if ident.Name == "context" && selectorExpr.Sel.Name == "Context" {
							syncFuncsFile = &parsedFile{file: file, path: path}
							break
						}
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		panic(err)
	}

	if streamFile == nil {
		panic("stream file not found")
	}

	if parseFile == nil {
		panic("parse file not found")
	}

	if parseStreamFile == nil {
		panic("parse_stream file not found")
	}

	if syncFuncsFile == nil {
		panic("sync functions file not found")
	}

	packageName := streamFile.file.Name.Name
	packageBaseName := packageName[strings.LastIndex(packageName, "/")+1:]

	// Extract stream methods (keeping for backwards compatibility / reference)
	var streamFunctions []map[string]any
	for _, decl := range streamFile.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
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

	// Extract sync functions (package-level functions with context.Context first param)
	var syncFunctions []map[string]any
	for _, decl := range syncFuncsFile.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		// Skip methods (functions with receivers)
		if funcDecl.Recv != nil {
			continue
		}

		// Check if first param is context.Context
		if funcDecl.Type.Params == nil || len(funcDecl.Type.Params.List) == 0 {
			continue
		}

		firstParam := funcDecl.Type.Params.List[0]
		selectorExpr, ok := firstParam.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}

		ident, ok := selectorExpr.X.(*ast.Ident)
		if !ok {
			continue
		}

		if ident.Name != "context" || selectorExpr.Sel.Name != "Context" {
			continue
		}

		output := make(map[string]any)
		output["name"] = funcDecl.Name.Name

		var args []string
		for _, param := range funcDecl.Type.Params.List {
			for _, name := range param.Names {
				args = append(args, name.Name)
			}
		}
		// Remove context (first) and variadic opts (last) args
		if len(args) >= 2 {
			output["args"] = args[1 : len(args)-1]
		} else {
			output["args"] = []string{}
		}

		syncFunctions = append(syncFunctions, output)
	}

	// Extract parse methods (methods on *parse receiver)
	var parseMethods []string
	for _, decl := range parseFile.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
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

		if receiverIdentifier.Name != "parse" {
			continue
		}

		parseMethods = append(parseMethods, funcDecl.Name.Name)
	}

	// Extract parse_stream methods (methods on *parse_stream receiver)
	var parseStreamMethods []string
	for _, decl := range parseStreamFile.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
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

		if receiverIdentifier.Name != "parse_stream" {
			continue
		}

		parseStreamMethods = append(parseStreamMethods, funcDecl.Name.Name)
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
		"streamPackageName":  packageBaseName,
		"streamPackagePath":  fmt.Sprintf("github.com/invakid404/baml-rest/%s", packageName),
		"streamMethods":      streamFunctions,
		"syncMethods":        syncFunctions,
		"parseMethods":       parseMethods,
		"parseStreamMethods": parseStreamMethods,
	}
	if err := introspectedTemplate.Execute(&introspectedTemplateOut, introspectedTemplateArgs); err != nil {
		panic(err)
	}

	if err := os.WriteFile("introspected/introspected.go", introspectedTemplateOut.Bytes(), 0644); err != nil {
		panic(err)
	}
}
