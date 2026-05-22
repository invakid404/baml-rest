package testharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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
	WriteFile(t, filepath.Join(dir, name), buf.String())
}

// CheckReplayName reports whether `name` is a safe replay-artifact
// basename. Validation runs on the RAW input — `filepath.Clean`
// would otherwise normalize away segments like `sub/../case.json`
// and quietly accept a name that violates the basename contract.
//
// Rejected inputs:
//   - empty, `.`, `..`
//   - absolute paths
//   - any path separator (`/` or `\` — both checked because Windows-
//     style names could appear in test inputs)
//   - any `..` segment after splitting on either separator
func CheckReplayName(name string) error {
	if filepath.IsAbs(name) {
		return fmt.Errorf("absolute name %q not allowed", name)
	}
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("name must be a non-empty basename, got %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name must be a basename, got %q (contains path separator)", name)
	}
	for _, seg := range strings.FieldsFunc(name, isPathSep) {
		if seg == ".." {
			return fmt.Errorf("name must be a basename, got %q (contains `..` segment)", name)
		}
	}
	return nil
}

func isPathSep(r rune) bool { return r == '/' || r == '\\' }
