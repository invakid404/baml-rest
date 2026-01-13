package memlimit

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/procfs"
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

// FormatBytes formats bytes as a human-readable string using IEC binary units.
// Uses 1024-based multipliers with proper IEC labels (KiB, MiB, GiB).
//
// NOTE: This is for human-readable logs only. Do NOT use for setting GOMEMLIMIT
// or other machine-parsed values - use raw integer bytes instead (e.g., EnvVar()).
func FormatBytes(bytes int64) string {
	const (
		KiB = 1024
		MiB = KiB * 1024
		GiB = MiB * 1024
	)

	switch {
	case bytes >= GiB:
		return fmt.Sprintf("%.2fGiB", float64(bytes)/GiB)
	case bytes >= MiB:
		return fmt.Sprintf("%.2fMiB", float64(bytes)/MiB)
	case bytes >= KiB:
		return fmt.Sprintf("%.2fKiB", float64(bytes)/KiB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// EnvVar returns the environment variable string for setting GOMEMLIMIT on a subprocess.
func EnvVar(limit int64) string {
	return fmt.Sprintf("GOMEMLIMIT=%d", limit)
}

// ParseBytes parses a byte size string with optional suffix.
//
// Supported suffixes:
//   - Decimal (1000-based): KB, MB, GB, TB
//   - Binary (1024-based): KiB, MiB, GiB, TiB (also Ki, Mi, Gi, Ti and K, M, G, T)
//   - No suffix or "B": raw bytes
//   - "off": returns (0, nil) - special case for GOMEMLIMIT
//
// Only integer values are accepted. Decimal points (e.g., "1.5GiB") are rejected.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}

	// Handle special values
	if strings.EqualFold(s, "off") {
		return 0, nil
	}

	// Find where the number ends and suffix begins
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}

	if i == 0 {
		return 0, fmt.Errorf("no numeric value found in %q", s)
	}

	// Check for decimal point (not supported)
	if i < len(s) && s[i] == '.' {
		return 0, fmt.Errorf("decimal values not supported in %q; use integer bytes", s)
	}

	numStr := s[:i]
	suffix := strings.TrimSpace(s[i:])

	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in %q: %w", s, err)
	}

	var multiplier int64 = 1
	switch strings.ToUpper(suffix) {
	case "", "B":
		multiplier = 1
	// Decimal (1000-based)
	case "KB":
		multiplier = 1000
	case "MB":
		multiplier = 1000 * 1000
	case "GB":
		multiplier = 1000 * 1000 * 1000
	case "TB":
		multiplier = 1000 * 1000 * 1000 * 1000
	// Binary (1024-based) - IEC standard
	case "KIB", "KI", "K":
		multiplier = 1024
	case "MIB", "MI", "M":
		multiplier = 1024 * 1024
	case "GIB", "GI", "G":
		multiplier = 1024 * 1024 * 1024
	case "TIB", "TI", "T":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown suffix %q in %q", suffix, s)
	}

	// Check for overflow before multiplying
	if num > 0 && multiplier > 1 {
		maxSafe := (1<<63 - 1) / multiplier
		if num > maxSafe {
			return 0, fmt.Errorf("value %q too large: would overflow int64", s)
		}
	}

	return num * multiplier, nil
}

// GetRSS returns the current resident set size (RSS) of the process in bytes.
// Returns 0 if RSS cannot be determined (e.g., on non-Linux systems).
func GetRSS() int64 {
	p, err := procfs.Self()
	if err != nil {
		return 0
	}
	rss, _ := getRSSFromProc(p)
	return rss
}

// getRSSFromProc reads RSS from an already-opened procfs.Proc handle.
// This avoids repeated procfs.Self() calls in hot paths.
// Returns (rss, true) on success, (0, false) on error.
func getRSSFromProc(p procfs.Proc) (int64, bool) {
	stat, err := p.Stat()
	if err != nil {
		return 0, false
	}
	return int64(stat.RSS * os.Getpagesize()), true
}

// RSSMonitorConfig configures the RSS-based GC trigger.
type RSSMonitorConfig struct {
	// Threshold is the RSS threshold in bytes that triggers GC.
	Threshold int64
	// Interval is how often to check RSS.
	Interval time.Duration
	// MaxBackoff is the maximum backoff interval when GC doesn't help at all.
	// Defaults to 5 minutes.
	MaxBackoff time.Duration
	// OnGC is an optional callback invoked when GC is triggered.
	// It receives the RSS before GC, RSS after GC, and the result.
	OnGC func(rssBefore, rssAfter int64, result GCResult)
}

// GCResult indicates the outcome of a GC cycle.
type GCResult int

const (
	// GCResultRecovered means RSS dropped below threshold.
	GCResultRecovered GCResult = iota
	// GCResultPartial means RSS decreased but is still above threshold.
	GCResultPartial
	// GCResultIneffective means RSS didn't decrease (possible true leak).
	GCResultIneffective
)

func (r GCResult) String() string {
	switch r {
	case GCResultRecovered:
		return "recovered"
	case GCResultPartial:
		return "partial"
	case GCResultIneffective:
		return "ineffective"
	default:
		return "unknown"
	}
}

// rssMonitorStarted ensures only one RSS monitor runs per process.
var rssMonitorStarted sync.Once

// rssMonitorStop holds the stop function for the singleton monitor.
var rssMonitorStop func()

// StartRSSMonitor starts a background goroutine that monitors RSS and triggers
// GC when it exceeds the configured threshold. This is useful for applications
// with significant native memory (e.g., CGO/FFI) where Go's GC doesn't see the
// full memory pressure.
//
// Singleton behavior: Only one monitor can run per process. The first successful
// call starts the monitor and sets the configuration. Subsequent calls return the
// same stop function without starting additional goroutines or changing the config.
//
// Backoff behavior:
//   - If GC brings RSS below threshold: reset to normal interval
//   - If GC reduces RSS but still above threshold: keep normal interval (still helping)
//   - If GC doesn't reduce RSS at all: back off exponentially (true leak, GC is wasteful)
//
// The monitor runs until the returned stop function is called.
// The stop function is safe to call multiple times.
//
// Returns a no-op stop function only if:
//   - Threshold is <= 0
//   - RSS cannot be read (non-Linux systems, procfs unavailable)
func StartRSSMonitor(cfg RSSMonitorConfig) (stop func()) {
	noop := func() {}

	if cfg.Threshold <= 0 {
		return noop
	}

	// Probe procfs before starting - if we can't read RSS, don't start
	proc, err := procfs.Self()
	if err != nil {
		return noop
	}
	if _, ok := getRSSFromProc(proc); !ok {
		return noop
	}

	rssMonitorStarted.Do(func() {
		if cfg.Interval <= 0 {
			cfg.Interval = 5 * time.Second
		}
		if cfg.MaxBackoff <= 0 {
			cfg.MaxBackoff = 5 * time.Minute
		}

		done := make(chan struct{})
		var stopOnce sync.Once

		go func() {
			// Cache procfs handle to avoid repeated Self() calls
			p, err := procfs.Self()
			if err != nil {
				return
			}

			interval := cfg.Interval
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			// Track consecutive ineffective GCs (from previous cycle) to reduce
			// FreeOSMemory frequency during true leak scenarios
			ineffectiveCount := 0

			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					rssBefore, ok := getRSSFromProc(p)
					if !ok {
						// Can't read RSS, skip this tick
						continue
					}
					if rssBefore <= cfg.Threshold {
						// RSS is fine, reset to normal interval if we were backed off
						if interval != cfg.Interval {
							interval = cfg.Interval
							ticker.Reset(interval)
						}
						ineffectiveCount = 0
						continue
					}

					// RSS exceeded threshold, trigger GC cycle:
					// 1. Run GC twice with a small gap to allow finalizers to run
					//    (finalizers are async, first GC queues them, second processes freed memory)
					// 2. Return memory to OS (skipped during long ineffective streaks)
					// 3. Measure RSS after the full cycle
					runtime.GC()
					time.Sleep(10 * time.Millisecond)
					runtime.GC()

					// Call FreeOSMemory to return pages to OS.
					// Based on previous cycle's ineffectiveCount: skip every 3rd attempt
					// during long ineffective streaks to reduce overhead when GC can't help.
					if ineffectiveCount < 3 || ineffectiveCount%3 == 0 {
						debug.FreeOSMemory()
					}

					// Measure RSS after the full GC+FreeOSMemory cycle
					rssAfter, ok := getRSSFromProc(p)

					var result GCResult
					if !ok {
						// Can't read RSS after GC, treat as ineffective.
						// Use rssBefore for callback so consumers don't see misleading 0.
						rssAfter = rssBefore
						result = GCResultIneffective
						ineffectiveCount++
					} else if rssAfter <= cfg.Threshold {
						result = GCResultRecovered
						ineffectiveCount = 0
					} else if rssAfter < rssBefore {
						result = GCResultPartial
						ineffectiveCount = 0
					} else {
						result = GCResultIneffective
						ineffectiveCount++
					}

					if cfg.OnGC != nil {
						cfg.OnGC(rssBefore, rssAfter, result)
					}

					switch result {
					case GCResultRecovered:
						// Fully recovered, reset to normal interval
						if interval != cfg.Interval {
							interval = cfg.Interval
							ticker.Reset(interval)
						}
					case GCResultPartial:
						// GC is helping but not enough yet, keep checking frequently
						if interval != cfg.Interval {
							interval = cfg.Interval
							ticker.Reset(interval)
						}
					case GCResultIneffective:
						// GC isn't helping at all, back off to avoid waste
						interval = min(interval*2, cfg.MaxBackoff)
						ticker.Reset(interval)
					}
				}
			}
		}()

		rssMonitorStop = func() {
			stopOnce.Do(func() { close(done) })
		}
	})

	if rssMonitorStop == nil {
		return noop
	}
	return rssMonitorStop
}
