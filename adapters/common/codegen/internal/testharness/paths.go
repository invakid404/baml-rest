package testharness

// hasWindowsDrivePrefix reports whether `name` starts with a
// Windows drive letter (e.g. "C:foo", "d:bar"). The check is
// OS-independent so regression tests for the basename validators
// run the same way on Linux/macOS CI and on Windows developer
// machines — `filepath.VolumeName` is a no-op on non-Windows and
// would silently skip the regression we want to keep locked.
//
// Only a TOP-LEVEL drive prefix is recognized; nested colon-in-
// segment cases like "shared/C:foo.baml" are out of scope here.
func hasWindowsDrivePrefix(name string) bool {
	if len(name) < 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// isPathSep recognises the two path separators that can appear in
// test-authored names. Shared by the basename validators so
// rejecting `/` and `\` is uniform across the helpers.
func isPathSep(r rune) bool { return r == '/' || r == '\\' }
