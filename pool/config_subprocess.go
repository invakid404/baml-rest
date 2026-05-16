//go:build !inprocess

package pool

import "fmt"

// normalizeConfig validates and applies defaults for subprocess pool
// builds. WorkerPath identifies the worker binary that exec.Command
// will spawn; without it the pool cannot start a worker. PoolSize
// defaults to 4 to match DefaultConfig when callers omit it.
func normalizeConfig(config *Config) error {
	if config.WorkerPath == "" {
		return fmt.Errorf("WorkerPath is required")
	}
	if config.PoolSize <= 0 {
		config.PoolSize = 4
	}
	return nil
}
