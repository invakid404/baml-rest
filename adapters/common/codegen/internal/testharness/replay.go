package testharness

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

// WriteReplayArtifact writes a pretty-printed JSON replay artifact
// under `dir`. `name` is the file basename (relative to `dir`). The
// content is re-indented with two-space indent + a trailing newline
// so successive runs produce byte-identical files — making git diffs
// across fuzz failure refreshes readable and review-friendly.
//
// The fuzz framework uses this for the per-failure replay envelope:
// when an invariant fails, the (schema, value, expected, mock_output)
// quadruple is serialized via this helper into a stable JSON file
// the developer can rerun deterministically.
func WriteReplayArtifact(t *testing.T, dir, name string, content json.RawMessage) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, content, "", "  "); err != nil {
		t.Fatalf("indent replay artifact %s: %v\nraw: %s", name, err, string(content))
	}
	buf.WriteByte('\n')
	WriteFile(t, filepath.Join(dir, name), buf.String())
}
