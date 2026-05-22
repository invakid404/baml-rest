package testharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// WriteReplayArtifact writes a pretty-printed JSON replay artifact
// at `dir/name`. `name` must be a basename — no path separators, no
// `.` or `..` segments, no absolute paths. Callers wanting to nest
// the artifact in a subdirectory should create the directory and
// pass its absolute path as `dir`. See CheckReplayName for the
// validation contract exposed to tests.
//
// The content is re-indented with two-space indent + a trailing
// newline so successive runs produce byte-identical files — making
// git diffs across fuzz failure refreshes readable and review-
// friendly.
//
// The fuzz framework uses this for the per-failure replay envelope:
// when an invariant fails, the (schema, value, expected, mock_output)
// quadruple is serialized via this helper into a stable JSON file
// the developer can rerun deterministically.
func WriteReplayArtifact(t *testing.T, dir, name string, content json.RawMessage) {
	t.Helper()
	if err := CheckReplayName(name); err != nil {
		t.Fatalf("WriteReplayArtifact: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, content, "", "  "); err != nil {
		t.Fatalf("indent replay artifact %s: %v\nraw: %s", name, err, string(content))
	}
	buf.WriteByte('\n')
	WriteFile(t, filepath.Join(dir, filepath.Clean(name)), buf.String())
}

// CheckReplayName reports whether `name` is a safe replay-artifact
// basename. Returns a non-nil error when `name` contains a path
// separator, is absolute, or is `.` / `..`. Exposed so unit tests
// can drive the rejection rules directly.
func CheckReplayName(name string) error {
	if filepath.IsAbs(name) {
		return fmt.Errorf("absolute name %q not allowed", name)
	}
	clean := filepath.Clean(name)
	if clean != filepath.Base(clean) || clean == "." || clean == ".." {
		return fmt.Errorf("name must be a basename, got %q", name)
	}
	return nil
}
