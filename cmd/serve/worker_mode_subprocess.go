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

// extractWorker extracts the embedded worker binary to a cache location.
// Returns the path to the extracted binary.
func extractWorker(logger zerolog.Logger) (string, error) {
	// Use a hash-based filename to detect changes
	hash := sha256.Sum256(workerBinary)
	hashStr := hex.EncodeToString(hash[:8]) // First 8 bytes is enough

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

	// Check if worker already exists with correct hash
	if _, err := os.Stat(workerPath); err == nil {
		return workerPath, nil
	}

	// Extract worker binary
	if err := os.WriteFile(workerPath, workerBinary, 0755); err != nil {
		return "", fmt.Errorf("failed to write worker binary: %w", err)
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

// configureWorkerMode prepares the pool config for subprocess worker
// execution: extracts the embedded worker binary to a cache path and
// wires it into Config.WorkerPath. The in-process WorkerFactory seam
// is unused in subprocess builds.
func configureWorkerMode(logger zerolog.Logger, cfg *pool.Config) error {
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
// warning when the operator requests >1, since the pool clamps to 1.
func warnPoolSizeOverride(_ zerolog.Logger, _ int) {}
