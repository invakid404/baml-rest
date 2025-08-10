package main

import (
	"bytes"
	_ "embed"
	"os"
	"strings"
	"text/template"
)

//go:embed embed.go.tmpl
var embedTemplateInput string

const outputName = "embed.go"

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		panic(err)
	}

	var dirs []string
	for _, entry := range entries {
		name := entry.Name()
		if name[0] != '.' {
			dirs = append(dirs, name)
		}
	}

	embedTemplate := template.Must(template.New("embed.go").Parse(embedTemplateInput))
	input := map[string]any{
		"dirs": strings.Join(dirs, " "),
	}

	var embedOutput bytes.Buffer
	if err = embedTemplate.Execute(&embedOutput, input); err != nil {
		panic(err)
	}

	if err = os.WriteFile(outputName, embedOutput.Bytes(), 0644); err != nil {
		panic(err)
	}
}
