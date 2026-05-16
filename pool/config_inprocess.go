//go:build inprocess

package pool

import "fmt"

// normalizeConfig validates and applies defaults for inprocess pool
// builds. The factory is the only construction path; WorkerPath and
// WorkerMemLimit have no meaning because there is no worker process
// to spawn or memory-limit. PoolSize is force-clamped to 1: a single
// process cannot restore the FFI isolation that multiple worker
// processes provide, so running more than one handler in the same
// address space buys nothing while creating contention.
func normalizeConfig(config *Config) error {
	if config.WorkerFactory == nil {
		return fmt.Errorf("inprocess pool: WorkerFactory is required")
	}
	config.PoolSize = 1
	return nil
}
