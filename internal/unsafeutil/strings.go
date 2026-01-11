// Package unsafeutil provides unsafe operations for performance-critical code paths.
package unsafeutil

import "unsafe"

// BytesToString converts a byte slice to a string without copying.
//
// SAFETY: This function is safe to use when:
//   - The byte slice is not modified after the string is created
//   - The byte slice remains valid for the lifetime of the string
//   - The string is used ephemerally (e.g., passed to a function and not stored)
//
// Violating these invariants causes undefined behavior due to Go's immutable
// string semantics being broken.
func BytesToString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
