package bamlutils

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

var configLexer = lexer.MustSimple([]lexer.SimpleRule{
	{"Comment", `//.*`},
	{"Whitespace", `\s+`},
	{"Keyword", `(?i)generator`},
	{"String", `"[^"]*"`},
	{"Ident", `[a-zA-Z_][a-zA-Z0-9_/-]*`},
	{"LBrace", `\{`},
	{"RBrace", `\}`},
	{"LAngle", `<`},
	{"RAngle", `>`},
	{"Other", `.`},
})

type Config struct {
	Items []Item `@@*`
}

type Item struct {
	Generator *Generator `@@`
	Other     *Other     `| @@`
}

type Generator struct {
	Keyword string   `@Keyword`
	Name    string   `@Ident`
	Fields  []*Field `{ LBrace @@* RBrace }`
}

type Field struct {
	Key   string `@Ident`
	Value string `( @String | @Ident )`
}

type Other struct {
	Token string `@( Ident | String | LBrace | RBrace | LAngle | RAngle | Other )`
}

func ExtractVersions(filePath string, file fs.File) ([]string, error) {
	// Build the parser.
	parser, err := participle.Build[Config](
		participle.Lexer(configLexer),
		participle.Elide("Comment", "Whitespace"),
		participle.CaseInsensitive("Ident"),
	)
	if err != nil {
		return nil, err
	}

	// Parse the entire file content.
	config, err := parser.Parse(filePath, file)
	if err != nil {
		return nil, err
	}

	// Collect versions from all generators.
	var versions []string
	for _, item := range config.Items {
		if item.Generator != nil {
			generator := item.Generator
			for _, field := range generator.Fields {
				if strings.EqualFold(field.Key, "version") {
					value := strings.Trim(field.Value, `"`)
					versions = append(versions, value)
				}
			}
		}
	}
	return versions, nil
}

func ParseVersions(target fs.FS) (versions []string, err error) {
	err = fs.WalkDir(target, ".", func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if dirEntry.IsDir() || filepath.Ext(path) != ".baml" {
			return nil
		}

		file, err := target.Open(path)
		if err != nil {
			return err
		}
		defer func(file fs.File) {
			_ = file.Close()
		}(file)

		currentVersions, err := ExtractVersions(path, file)

		if err != nil {
			return err
		}

		if len(currentVersions) > 0 {
			versions = append(versions, currentVersions...)
		}

		return nil
	})

	return
}
