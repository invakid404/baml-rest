package bamlutils

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// ExtractVersions scans a single .baml file for `generator NAME { version V }`
// declarations and returns the value of every `version` field, in source order.
//
// Value shapes accepted (matching the prior narrow grammar's `@String | @Ident`
// whitelist): quoted strings and bare identifiers. Number, env-ref, raw-string,
// and list values return an error — the prior participle grammar failed to
// parse them as a Field value, and we preserve that posture rather than widen
// via bamlparser.Value.String().
//
// Keyword matching is case-sensitive on both the `generator` block keyword
// and the `version` field key, matching upstream BAML's grammar. The old
// parser was case-insensitive on both via `(?i)generator` in its lexer and
// strings.EqualFold on the field name; that was an artifact of how participle's
// CaseInsensitive lexer interacted with EqualFold rather than a designed
// feature (versions.go had no tests). This refactor closes that divergence.
func ExtractVersions(filePath string, file fs.File) ([]string, error) {
	parsed, err := bamlparser.ParseReader(filePath, file)
	if err != nil {
		return nil, err
	}

	var versions []string
	for _, item := range parsed.Items {
		if item == nil || item.Generator == nil {
			continue
		}
		for _, field := range item.Generator.Fields {
			if field == nil || field.Key != "version" {
				continue
			}
			value, err := versionFieldValue(filePath, field.Value)
			if err != nil {
				return nil, err
			}
			versions = append(versions, value)
		}
	}
	return versions, nil
}

// versionFieldValue extracts the version string from a `version` field's
// Value, preserving the prior grammar's narrow `String | Ident` whitelist.
// Number / EnvRef / Raw / List values return an error — those shapes would
// not have parsed as a Field value under the old grammar at all, so callers
// see the same failure mode (an error instead of a silently widened value).
//
// An empty quoted value (`version ""`) returns the empty string and no error,
// matching the prior parser which appended strings.Trim's result regardless.
func versionFieldValue(filePath string, v *bamlparser.Value) (string, error) {
	if v == nil {
		return "", fmt.Errorf("%s: version field has no value", filePath)
	}
	if s, ok := v.LiteralValue(); ok {
		return s, nil
	}
	if s, ok := v.IdentValue(); ok {
		return s, nil
	}
	return "", fmt.Errorf("%s: version field value must be a string or identifier", filePath)
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
