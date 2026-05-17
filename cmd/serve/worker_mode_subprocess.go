//go:build !inprocess

package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/invakid404/baml-rest/pool"
)

//go:embed worker
var workerBinary []byte

// extractWorker materialises the embedded worker binary into a
// cache file and returns its path. The path is keyed by the first 8
// bytes of the SHA-256 (short enough for a tidy filename, long
// enough to make collisions practically impossible across builds),
// but reuse of an existing cache entry is gated on a full-SHA-256
// match against the embedded bytes — a half-written file from an
// earlier interrupted extraction has the right name but the wrong
// content, and silently exec'ing it would crash the worker process.
//
// New writes go through a temp-file + fsync + rename so the
// hash-named path either does not exist or contains a fully-written,
// fsynced binary. Readers (the pool's exec.Command) cannot observe a
// torn intermediate state.
func extractWorker(logger zerolog.Logger) (string, error) {
	expectedHash := sha256.Sum256(workerBinary)
	hashStr := hex.EncodeToString(expectedHash[:8])

	// Use system cache directory
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		// Fallback to temp directory
		cacheDir = os.TempDir()
	}
	cacheDir = filepath.Join(cacheDir, "baml-rest")

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	workerFilename := fmt.Sprintf("worker-%s", hashStr)
	workerPath := filepath.Join(cacheDir, workerFilename)

	// Verify the cached binary's full SHA-256 before reusing it. A
	// stat-only short-circuit would happily return a corrupt file
	// left behind by an interrupted previous extraction.
	if existing, err := os.ReadFile(workerPath); err == nil {
		if sha256.Sum256(existing) == expectedHash {
			return workerPath, nil
		}
		logger.Warn().Str("path", workerPath).Msg("Cached worker binary failed integrity check, re-extracting")
	}

	if err := writeWorkerAtomic(workerPath, workerFilename, cacheDir); err != nil {
		return "", err
	}

	// Clean up old worker versions
	entries, err := os.ReadDir(cacheDir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "worker-") && name != workerFilename {
				oldPath := filepath.Join(cacheDir, name)
				if err := os.Remove(oldPath); err != nil {
					logger.Debug().Str("path", oldPath).Err(err).Msg("Failed to remove old worker binary")
				} else {
					logger.Debug().Str("path", oldPath).Msg("Removed old worker binary")
				}
			}
		}
	}

	return workerPath, nil
}

// writeWorkerAtomic writes workerBinary to dst via a temp file in
// the same directory followed by os.Rename. Rename is atomic within
// a filesystem on POSIX, so concurrent readers see either the old
// file or the fully-written new one — never a torn write. The temp
// file is removed on every error path; on success the deferred
// remove targets the original (now-renamed) name and is a harmless
// no-op.
func writeWorkerAtomic(dst, workerFilename, cacheDir string) error {
	tmp, err := os.CreateTemp(cacheDir, workerFilename+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp worker file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(workerBinary); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp worker file: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp worker file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp worker file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp worker file: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("rename worker into place: %w", err)
	}
	return nil
}

// configureWorkerMode prepares the pool config for subprocess worker
// execution: extracts the embedded worker binary to a cache path and
// wires it into Config.WorkerPath. The in-process WorkerFactory seam
// is unused in subprocess builds.
//
// runtimeCfg matches the inprocess signature for build-tag symmetry
// but is unused on this build: subprocess workers resolve env state
// themselves inside cmd/worker/main.go at process startup. There is
// no host→worker config RPC; programmatic config flows only through
// the inprocess WorkerFactory seam.
func configureWorkerMode(logger zerolog.Logger, cfg *pool.Config, runtimeCfg workerModeRuntimeConfig) error {
	_ = runtimeCfg
	workerPath, err := extractWorker(logger)
	if err != nil {
		return fmt.Errorf("failed to extract worker binary: %w", err)
	}
	logger.Info().Str("path", workerPath).Msg("Worker binary extracted")
	cfg.WorkerPath = workerPath
	return nil
}

// effectivePoolSizeForMemory is the pool size memlimit.CalculateLimits
// uses to split the total memory budget between server and workers.
// Subprocess builds run one process per slot, so the requested
// PoolSize is honest. The inprocess variant returns 1 because every
// slot collapses onto the same address space.
func effectivePoolSizeForMemory(poolSize int) int { return poolSize }

// warnPoolSizeOverride is a no-op in subprocess builds — multiple
// workers are the design center. The inprocess variant logs a
// warning when the operator explicitly requests >1, since the pool
// clamps to 1.
func warnPoolSizeOverride(_ zerolog.Logger, _ int, _ bool) {}
