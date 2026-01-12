package memlimit

import (
	"bufio"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// cgroupV2MemMax is the cgroup v2 memory limit file
	cgroupV2MemMax = "/sys/fs/cgroup/memory.max"
	// cgroupV1MemLimit is the cgroup v1 memory limit file
	cgroupV1MemLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	// procMeminfo is the system memory info file
	procMeminfo = "/proc/meminfo"
)

// DetectAvailable detects the available memory limit from the environment.
// It checks in order: cgroups v2, cgroups v1, /proc/meminfo.
// Returns 0 if detection fails.
func DetectAvailable() int64 {
	// Try cgroups v2 first
	if limit := readCgroupV2(); limit > 0 {
		return limit
	}

	// Try cgroups v1
	if limit := readCgroupV1(); limit > 0 {
		return limit
	}

	// Fall back to /proc/meminfo (for VMs like Fly.io)
	if limit := readProcMeminfo(); limit > 0 {
		return limit
	}

	return 0
}

// readCgroupV2 reads the memory limit from cgroup v2.
// Returns 0 if not available or unlimited ("max").
func readCgroupV2() int64 {
	data, err := os.ReadFile(cgroupV2MemMax)
	if err != nil {
		return 0
	}

	content := strings.TrimSpace(string(data))
	if content == "max" {
		return 0 // unlimited
	}

	limit, err := strconv.ParseInt(content, 10, 64)
	if err != nil {
		return 0
	}

	return limit
}

// readCgroupV1 reads the memory limit from cgroup v1.
// Returns 0 if not available or effectively unlimited.
func readCgroupV1() int64 {
	data, err := os.ReadFile(cgroupV1MemLimit)
	if err != nil {
		return 0
	}

	limit, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}

	// cgroup v1 uses a very large number to indicate unlimited
	// (typically 9223372036854771712 or similar)
	// Consider anything over 1 exabyte as unlimited
	if limit > 1<<60 {
		return 0
	}

	return limit
}

// readProcMeminfo reads MemTotal from /proc/meminfo.
// This is the fallback for VMs (like Fly.io) where cgroups don't reflect the limit.
func readProcMeminfo() int64 {
	file, err := os.Open(procMeminfo)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}

			// Value is in kB
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}

			return kb * 1024 // Convert to bytes
		}
	}

	return 0
}

// CalculateLimits calculates memory limits for server and workers.
// totalLimit is the total available memory in bytes.
// poolSize is the number of worker processes.
// Returns (serverLimit, workerLimit) in bytes.
//
// The allocation strategy:
// - 10% reserved for non-heap memory (stack, mmap, etc.)
// - Server gets 20% of usable memory (it's mostly routing HTTP<->gRPC)
// - Workers split the remaining 80% (they hold BAML runtime, buffer LLM responses)
func CalculateLimits(totalLimit int64, poolSize int) (serverLimit, workerLimit int64) {
	if totalLimit <= 0 || poolSize <= 0 {
		return 0, 0
	}

	// Reserve 10% for non-heap memory (stack, mmap, etc.)
	usableMemory := int64(float64(totalLimit) * 0.9)

	// Server is lighter - give it 20%
	serverLimit = int64(float64(usableMemory) * 0.2)

	// Workers split the remaining 80%
	workerPool := int64(float64(usableMemory) * 0.8)
	workerLimit = workerPool / int64(poolSize)

	return serverLimit, workerLimit
}

// SetGOMEMLIMIT sets the GOMEMLIMIT for the current process.
// Returns the previous value (0 if not set).
func SetGOMEMLIMIT(limit int64) int64 {
	if limit <= 0 {
		return 0
	}

	prev := debug.SetMemoryLimit(limit)
	return prev
}

// FormatBytes formats bytes as a human-readable string.
func FormatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2fGB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2fMB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2fKB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// EnvVar returns the environment variable string for setting GOMEMLIMIT on a subprocess.
func EnvVar(limit int64) string {
	return fmt.Sprintf("GOMEMLIMIT=%d", limit)
}
