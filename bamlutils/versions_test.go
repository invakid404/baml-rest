package bamlutils

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// openString turns an in-memory .baml source into an fs.File so the
// ExtractVersions tests don't need to materialise on disk. Uses fstest.MapFS
// because that's what the ParseVersions tests use too.
func openString(t *testing.T, name, content string) fs.File {
	t.Helper()
	fsys := fstest.MapFS{name: {Data: []byte(content)}}
	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}
	return f
}

func extract(t *testing.T, name, content string) ([]string, error) {
	t.Helper()
	f := openString(t, name, content)
	defer f.Close()
	return ExtractVersions(name, f)
}

func TestExtractVersions_SingleGenerator(t *testing.T) {
	got, err := extract(t, "a.baml", `generator go { version "0.219.0" }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.219.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_MultipleGenerators(t *testing.T) {
	src := `
generator a { version "0.219.0" }
generator b { version "0.220.0" }
`
	got, err := extract(t, "a.baml", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.219.0", "0.220.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_MultipleVersionFieldsInOneGenerator(t *testing.T) {
	// Two version fields in a single generator: both are appended in
	// source order. Matches the prior loop's behavior.
	src := `generator g { version "0.1.0" version "0.2.0" }`
	got, err := extract(t, "a.baml", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.1.0", "0.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_BareIdentValue(t *testing.T) {
	// Bare-ident value: preserves the prior grammar's @Ident arm.
	got, err := extract(t, "a.baml", `generator g { version dev-branch }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"dev-branch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_EmptyQuotedValue(t *testing.T) {
	// `version ""` returns "" — not skipped. Matches the prior parser,
	// which appended strings.Trim's empty result without a non-empty guard.
	got, err := extract(t, "a.baml", `generator g { version "" }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_BareNumericRejected(t *testing.T) {
	// `version 0.219.0` (bare numeric) — the bamlparser tokenises this as
	// Number/Punct/Number which the prior @String|@Ident grammar would have
	// failed to parse. Surface that as an error here rather than silently
	// widening via Value.String().
	_, err := extract(t, "a.baml", `generator g { version 0.219.0 }`)
	if err == nil {
		t.Fatalf("expected error for bare-numeric version value")
	}
}

func TestExtractVersions_EnvRefRejected(t *testing.T) {
	// `version env.MY_VAR` — Value.EnvRef. The prior grammar's narrow
	// String|Ident whitelist would have failed to parse this.
	_, err := extract(t, "a.baml", `generator g { version env.MY_VAR }`)
	if err == nil {
		t.Fatalf("expected error for env-ref version value")
	}
}

func TestExtractVersions_RawStringRejected(t *testing.T) {
	// `version #"raw"#` — Value.Raw. Same narrow-whitelist rationale.
	_, err := extract(t, "a.baml", `generator g { version #"raw"# }`)
	if err == nil {
		t.Fatalf("expected error for raw-string version value")
	}
}

func TestExtractVersions_ListRejected(t *testing.T) {
	// `version ["a", "b"]` — Value.List. Same narrow-whitelist rationale.
	_, err := extract(t, "a.baml", `generator g { version ["a", "b"] }`)
	if err == nil {
		t.Fatalf("expected error for list version value")
	}
}

func TestExtractVersions_UppercaseGeneratorIgnored(t *testing.T) {
	// Intentional tightening (#265 PR 4): the shared bamlparser matches the
	// literal lowercase `generator` token, matching upstream BAML's
	// case-sensitive grammar. The prior versions-only parser had `(?i)generator`
	// in its lexer and accepted `Generator`, `GENERATOR`, etc. With the shared
	// parser, an uppercase `Generator` block lands in Item.Other (or further
	// metadata-only nodes) and ExtractVersions ignores it cleanly. The block
	// body's `version "..."` is consumed by the Other catch-all's balanced
	// brace skip, so no error is raised either.
	got, err := extract(t, "a.baml", `Generator g { version "0.219.0" }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no versions for uppercase Generator, got %#v", got)
	}
}

func TestExtractVersions_UppercaseVersionFieldIgnored(t *testing.T) {
	// Intentional tightening (#265 PR 4): strict `field.Key != "version"`
	// replaces the prior strings.EqualFold. `Version`, `VERSION`, etc. inside
	// a lowercase generator block are no longer treated as version fields.
	got, err := extract(t, "a.baml", `generator g { Version "0.219.0" }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no versions for uppercase Version field, got %#v", got)
	}
}

func TestExtractVersions_NoVersionField(t *testing.T) {
	got, err := extract(t, "a.baml", `generator g { output_type "go" }`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no versions, got %#v", got)
	}
}

func TestExtractVersions_NonGeneratorBlocksIgnored(t *testing.T) {
	// A file with several non-generator top-level constructs plus one real
	// generator. Only the generator's version is extracted; the other
	// constructs land in TypeBlock / ClientBlock / FunctionBlock /
	// RetryPolicyBlock / TypeAlias / Other and are ignored cleanly.
	src := `
class Foo { name string }
enum E { A B }
client<llm> C { provider openai }
function F(x: string) -> string { client C prompt #"hi"# }
retry_policy R { max_retries 3 }
type Alias = string
generator g { version "0.219.0" }
`
	got, err := extract(t, "a.baml", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.219.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtractVersions_CommentsAndWhitespaceTolerated(t *testing.T) {
	src := `
// leading comment
generator g {
    // inline comment
    version "0.219.0"  // trailing comment

}
`
	got, err := extract(t, "a.baml", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.219.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseVersions_FlatFS(t *testing.T) {
	fsys := fstest.MapFS{
		"a.baml": {Data: []byte(`generator g1 { version "0.219.0" }`)},
		"b.baml": {Data: []byte(`generator g2 { version "0.220.0" }`)},
	}
	got, err := ParseVersions(fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 versions, got %#v", got)
	}
	// Order across files is dictated by fs.WalkDir's lexical ordering; both
	// values must be present.
	want := map[string]bool{"0.219.0": true, "0.220.0": true}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected version %q in %#v", v, got)
		}
	}
}

func TestParseVersions_NestedDirectories(t *testing.T) {
	fsys := fstest.MapFS{
		"top.baml":             {Data: []byte(`generator g0 { version "0.1.0" }`)},
		"sub/nested.baml":      {Data: []byte(`generator g1 { version "0.2.0" }`)},
		"sub/deeper/leaf.baml": {Data: []byte(`generator g2 { version "0.3.0" }`)},
		"sub/deeper/notes.txt": {Data: []byte(`generator x { version "ignored" }`)},
	}
	got, err := ParseVersions(fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 versions across nested dirs, got %#v", got)
	}
	want := map[string]bool{"0.1.0": true, "0.2.0": true, "0.3.0": true}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected version %q in %#v", v, got)
		}
	}
}

func TestParseVersions_NonBamlExtensionIgnored(t *testing.T) {
	fsys := fstest.MapFS{
		"a.txt":  {Data: []byte(`generator g { version "ignored" }`)},
		"b.baml": {Data: []byte(`generator g { version "0.219.0" }`)},
	}
	got, err := ParseVersions(fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0.219.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseVersions_NoBamlFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"a.txt": {Data: []byte(`generator g { version "0.219.0" }`)},
	}
	got, err := ParseVersions(fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for no .baml files, got %#v", got)
	}
}

func TestParseVersions_PropagatesExtractError(t *testing.T) {
	// A .baml whose generator's `version` value is not a String|Ident — make
	// sure the error from ExtractVersions bubbles up through ParseVersions
	// (rather than being silently swallowed by the WalkDir caller).
	fsys := fstest.MapFS{
		"a.baml": {Data: []byte(`generator g { version env.X }`)},
	}
	_, err := ParseVersions(fsys)
	if err == nil {
		t.Fatalf("expected error from ParseVersions when an inner .baml has an invalid version value")
	}
	if !strings.Contains(err.Error(), "version field value") {
		t.Fatalf("error %q did not mention version field value", err)
	}
}
