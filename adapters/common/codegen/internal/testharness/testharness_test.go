package testharness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTempBamlSrcWritesFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.baml":         "// hello\n",
		"shared/types.baml": "class Foo { x int }\n",
		"shared/utils.baml": "// utils\n",
	}
	WriteTempBamlSrc(t, dir, files)
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, "baml_src", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("content mismatch at %s\nwant: %q\ngot:  %q", rel, want, string(got))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "baml_src")); err != nil {
		t.Fatalf("baml_src root missing: %v", err)
	}
}

func TestWriteTempBamlSrcEmptyMap(t *testing.T) {
	dir := t.TempDir()
	WriteTempBamlSrc(t, dir, nil)
	info, err := os.Stat(filepath.Join(dir, "baml_src"))
	if err != nil {
		t.Fatalf("baml_src not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("baml_src is not a directory")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "baml_src"))
	if err != nil {
		t.Fatalf("read baml_src: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty map should produce empty baml_src, got %d entries", len(entries))
	}
}

func TestCheckBamlSrcName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "baml_src")
	good := []string{
		"main.baml",
		"shared/types.baml",
		"a/b/c/d.baml",
	}
	for _, name := range good {
		if err := CheckBamlSrcName(root, name); err != nil {
			t.Errorf("legit name %q rejected: %v", name, err)
		}
	}
	type rejection struct {
		name string
		want string
	}
	bad := []rejection{
		{"/etc/passwd", "absolute file name"},
		{"../escaped.baml", "escapes baml_src root"},
		{"shared/../../escape.baml", "escapes baml_src root"},
		{".", "invalid file name"},
		{"..", "invalid file name"},
	}
	for _, c := range bad {
		err := CheckBamlSrcName(root, c.name)
		if err == nil {
			t.Errorf("bad name %q accepted; want rejection containing %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("bad name %q error %q does not contain %q", c.name, err.Error(), c.want)
		}
	}
}

func TestWriteReplayArtifactStableOutput(t *testing.T) {
	dir := t.TempDir()
	// json.Indent preserves source byte order; callers pass a
	// canonical form, the helper pretty-prints it byte-stably.
	content := json.RawMessage(`{"b":2,"a":1,"nested":{"y":[1,2,3],"x":"hi"}}`)
	WriteReplayArtifact(t, dir, "case.json", content)
	got, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "{\n  \"b\": 2,\n  \"a\": 1,\n  \"nested\": {\n    \"y\": [\n      1,\n      2,\n      3\n    ],\n    \"x\": \"hi\"\n  }\n}\n"
	if string(got) != want {
		t.Errorf("indent output mismatch\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestCheckReplayName(t *testing.T) {
	if err := CheckReplayName("case.json"); err != nil {
		t.Errorf("good basename rejected: %v", err)
	}
	if err := CheckReplayName("seed-123.json"); err != nil {
		t.Errorf("good basename rejected: %v", err)
	}
	bad := []string{
		"sub/file.json",
		"/abs/file.json",
		"../escape.json",
		".",
		"..",
	}
	for _, name := range bad {
		if err := CheckReplayName(name); err == nil {
			t.Errorf("bad name %q accepted; should reject", name)
		}
	}
}
