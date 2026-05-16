//go:build !inprocess

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rs/zerolog"
)

// TestExtractWorkerRewritesCorruptCache pins the integrity contract:
// when a cached worker file exists at the hash-derived path but its
// SHA-256 does not match the embedded bytes — the signature of an
// interrupted previous extraction — extractWorker overwrites it
// atomically instead of silently returning the corrupt path. Without
// this, the pool would exec a half-written binary on the next start.
func TestExtractWorkerRewritesCorruptCache(t *testing.T) {
	cacheRoot := redirectUserCacheDir(t)

	cacheDir := filepath.Join(cacheRoot, "baml-rest")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("setup cacheDir: %v", err)
	}

	expected := sha256.Sum256(workerBinary)
	workerFilename := fmt.Sprintf("worker-%s", hex.EncodeToString(expected[:8]))
	dst := filepath.Join(cacheDir, workerFilename)

	// Seed the cache with content that has the right filename but the
	// wrong bytes — exactly the shape an interrupted earlier
	// extraction would leave behind.
	corrupt := []byte("partially-written-garbage")
	if sha256.Sum256(corrupt) == expected {
		t.Fatalf("test setup picked corrupt bytes that hash to the expected value; pick different bytes")
	}
	if err := os.WriteFile(dst, corrupt, 0755); err != nil {
		t.Fatalf("seed corrupt cache: %v", err)
	}

	got, err := extractWorker(zerolog.Nop())
	if err != nil {
		t.Fatalf("extractWorker: %v", err)
	}
	if got != dst {
		t.Fatalf("extractWorker returned unexpected path: got %q, want %q", got, dst)
	}

	gotBytes, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if sha256.Sum256(gotBytes) != expected {
		t.Errorf("extracted file SHA-256 does not match embedded workerBinary; corrupt cache was reused")
	}
}

// TestExtractWorkerReusesValidCache verifies the happy path: when an
// existing cached file's full SHA-256 matches the embedded bytes,
// extractWorker returns it without rewriting. Pairs with the
// corrupt-cache test to confirm the integrity check is the gate, not
// an unconditional rewrite.
func TestExtractWorkerReusesValidCache(t *testing.T) {
	cacheRoot := redirectUserCacheDir(t)

	cacheDir := filepath.Join(cacheRoot, "baml-rest")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("setup cacheDir: %v", err)
	}

	expected := sha256.Sum256(workerBinary)
	workerFilename := fmt.Sprintf("worker-%s", hex.EncodeToString(expected[:8]))
	dst := filepath.Join(cacheDir, workerFilename)

	// Pre-populate the cache with the canonical bytes; record mtime
	// so we can detect a needless rewrite.
	if err := os.WriteFile(dst, workerBinary, 0755); err != nil {
		t.Fatalf("seed valid cache: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat seeded cache: %v", err)
	}
	preMtime := info.ModTime()

	got, err := extractWorker(zerolog.Nop())
	if err != nil {
		t.Fatalf("extractWorker: %v", err)
	}
	if got != dst {
		t.Fatalf("extractWorker returned unexpected path: got %q, want %q", got, dst)
	}

	postInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat post-extract: %v", err)
	}
	if !postInfo.ModTime().Equal(preMtime) {
		t.Errorf("extractWorker rewrote a valid cached file (mtime changed %v -> %v)", preMtime, postInfo.ModTime())
	}
}

// redirectUserCacheDir points os.UserCacheDir at a t.TempDir for the
// duration of the test. Both HOME (macOS) and XDG_CACHE_HOME (Linux)
// are set so the redirection works across platforms.
func redirectUserCacheDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch runtime.GOOS {
	case "darwin":
		t.Setenv("HOME", root)
		return filepath.Join(root, "Library", "Caches")
	default:
		t.Setenv("XDG_CACHE_HOME", root)
		t.Setenv("HOME", root)
		return root
	}
}
