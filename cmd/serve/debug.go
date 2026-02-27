//go:build debug

package main

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v3"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

func registerDebugEndpoints(r fiber.Router, logger zerolog.Logger, workerPool *pool.Pool) {
	// Kill a worker that has in-flight requests (for testing mid-stream worker death)
	r.Post("/_debug/kill-worker", func(c fiber.Ctx) error {
		result, err := workerPool.KillWorkerWithInFlight()
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"status": "no_worker_found",
				"error":  err.Error(),
			})
		}

		return c.JSON(fiber.Map{
			"status":          "killed",
			"worker_id":       result.WorkerID,
			"in_flight_count": result.InFlightCount,
			"got_first_byte":  result.GotFirstByte,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/kill-worker")

	// Get in-flight status of all workers (for debugging)
	r.Get("/_debug/in-flight", func(c fiber.Ctx) error {
		status := workerPool.GetInFlightStatus()
		workers := make([]map[string]any, 0, len(status))
		for _, s := range status {
			workers = append(workers, map[string]any{
				"worker_id":      s.WorkerID,
				"healthy":        s.Healthy,
				"in_flight":      s.InFlight,
				"got_first_byte": s.GotFirstByte,
			})
		}

		return c.JSON(fiber.Map{
			"status":  "ok",
			"workers": workers,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/in-flight")

	// Configure pool settings (for testing)
	r.Post("/_debug/config", func(c fiber.Ctx) error {
		var req struct {
			FirstByteTimeoutMs *int64 `json:"first_byte_timeout_ms,omitempty"`
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"status": "error",
				"error":  err.Error(),
			})
		}

		if req.FirstByteTimeoutMs != nil {
			if *req.FirstByteTimeoutMs < 0 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"status": "error",
					"error":  "first_byte_timeout_ms must be >= 0",
				})
			}
			workerPool.SetFirstByteTimeout(time.Duration(*req.FirstByteTimeoutMs) * time.Millisecond)
			logger.Info().Int64("first_byte_timeout_ms", *req.FirstByteTimeoutMs).Msg("Updated FirstByteTimeout")
		}

		return c.JSON(fiber.Map{
			"status":                "ok",
			"first_byte_timeout_ms": workerPool.GetFirstByteTimeout().Milliseconds(),
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/config")

	// Get goroutine stacks for leak detection
	// Returns pprof goroutine data with optional filtering (case-insensitive)
	// Supports both include patterns and exclude patterns (prefixed with -)
	// Example: filter=invakid404/baml-rest,-healthChecker,-StartRSSMonitor
	r.Get("/_debug/goroutines", func(c fiber.Ctx) error {
		// Get debug level (1 = counts, 2 = full stacks)
		debugLevel := 2
		if lvl := c.Query("debug"); lvl != "" {
			if parsed, err := strconv.Atoi(lvl); err == nil {
				debugLevel = parsed
			}
		}

		// Optional filter patterns (comma-separated, case-insensitive matching)
		// Patterns prefixed with - are exclusions
		filterPatterns := c.Query("filter")

		// Capture goroutine profile from main process
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, debugLevel); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"status": "error",
				"error":  err.Error(),
			})
		}

		stacks := buf.String()
		totalCount := runtime.NumGoroutine()

		// Parse include and exclude patterns
		var includePatterns, excludePatterns []string
		if filterPatterns != "" {
			for _, pattern := range strings.Split(filterPatterns, ",") {
				pattern = strings.TrimSpace(pattern)
				if pattern == "" {
					continue
				}
				if strings.HasPrefix(pattern, "-") {
					excludePatterns = append(excludePatterns, strings.ToLower(strings.TrimPrefix(pattern, "-")))
				} else {
					includePatterns = append(includePatterns, strings.ToLower(pattern))
				}
			}
		}

		// If filter patterns provided, count matching goroutines (case-insensitive)
		var matchCount int
		var matchedStacks []string
		if len(includePatterns) > 0 {
			goroutineStacks := strings.Split(stacks, "goroutine ")
			for _, stack := range goroutineStacks {
				if stack == "" {
					continue
				}
				stackLower := strings.ToLower(stack)

				// Check if stack matches any include pattern
				matched := false
				for _, pattern := range includePatterns {
					if strings.Contains(stackLower, pattern) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}

				// Check if stack matches any exclude pattern
				excluded := false
				for _, pattern := range excludePatterns {
					if strings.Contains(stackLower, pattern) {
						excluded = true
						break
					}
				}
				if excluded {
					continue
				}

				matchCount++
				// Truncate for readability
				if len(stack) > 1000 {
					stack = stack[:1000] + "..."
				}
				matchedStacks = append(matchedStacks, "goroutine "+stack)
			}
		}

		// Get worker goroutine data
		workerResults := workerPool.GetAllWorkersGoroutines(c.Context(), filterPatterns)

		// Build worker results for response
		workers := make([]map[string]any, 0, len(workerResults))
		for _, wr := range workerResults {
			workerData := map[string]any{
				"worker_id": wr.WorkerID,
			}
			if wr.Error != nil {
				workerData["error"] = wr.Error.Error()
			} else {
				workerData["total_count"] = wr.TotalCount
				workerData["match_count"] = wr.MatchCount
				workerData["matched_stacks"] = wr.MatchedStacks
			}
			workers = append(workers, workerData)
		}

		response := map[string]any{
			"status":      "ok",
			"total_count": totalCount,
			"workers":     workers,
		}

		if filterPatterns != "" {
			response["filter"] = filterPatterns
			response["match_count"] = matchCount
			response["matched_stacks"] = matchedStacks
		} else {
			response["stacks"] = stacks
		}

		return c.JSON(response)
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/goroutines")

	r.Get("/_debug/gc", func(c fiber.Ctx) error {
		// Capture memory stats before GC (main process)
		var memBefore runtime.MemStats
		runtime.ReadMemStats(&memBefore)

		// Force garbage collection (main process)
		runtime.GC()

		// Aggressively release memory to OS (main process)
		debug.FreeOSMemory()

		// Capture memory stats after GC (main process)
		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)

		// Trigger GC on all workers
		workerResults := workerPool.TriggerAllWorkersGC(c.Context())

		// Build worker results for response
		workers := make([]map[string]any, 0, len(workerResults))
		for _, wr := range workerResults {
			workerData := map[string]any{
				"worker_id": wr.WorkerID,
			}
			if wr.Error != nil {
				workerData["error"] = wr.Error.Error()
			} else {
				workerData["heap_alloc_before"] = wr.HeapAllocBefore
				workerData["heap_alloc_after"] = wr.HeapAllocAfter
				workerData["heap_released"] = wr.HeapReleased
				workerData["heap_freed"] = wr.HeapAllocBefore - wr.HeapAllocAfter
			}
			workers = append(workers, workerData)
		}

		return c.JSON(map[string]any{
			"status": "ok",
			"main_process": map[string]any{
				"before": map[string]any{
					"alloc":         memBefore.Alloc,
					"total_alloc":   memBefore.TotalAlloc,
					"sys":           memBefore.Sys,
					"heap_alloc":    memBefore.HeapAlloc,
					"heap_sys":      memBefore.HeapSys,
					"heap_idle":     memBefore.HeapIdle,
					"heap_inuse":    memBefore.HeapInuse,
					"heap_released": memBefore.HeapReleased,
				},
				"after": map[string]any{
					"alloc":         memAfter.Alloc,
					"total_alloc":   memAfter.TotalAlloc,
					"sys":           memAfter.Sys,
					"heap_alloc":    memAfter.HeapAlloc,
					"heap_sys":      memAfter.HeapSys,
					"heap_idle":     memAfter.HeapIdle,
					"heap_inuse":    memAfter.HeapInuse,
					"heap_released": memAfter.HeapReleased,
				},
				"freed": map[string]any{
					"heap_alloc":    memBefore.HeapAlloc - memAfter.HeapAlloc,
					"heap_inuse":    memBefore.HeapInuse - memAfter.HeapInuse,
					"heap_released": memAfter.HeapReleased - memBefore.HeapReleased,
				},
			},
			"workers": workers,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/gc")

	// Native thread backtraces via gdb — shows where Rust/C threads are blocked.
	// Requires gdb installed in the container and SYS_PTRACE capability.
	r.Get("/_debug/native-stacks", func(c fiber.Ctx) error {
		results := workerPool.GetAllWorkersNativeStacks(c.Context())

		workers := make([]map[string]any, 0, len(results))
		for _, wr := range results {
			workerData := map[string]any{
				"worker_id": wr.WorkerID,
				"pid":       wr.Pid,
			}
			if wr.Error != nil {
				workerData["error"] = wr.Error.Error()
			} else {
				workerData["output"] = wr.Output
			}
			workers = append(workers, workerData)
		}

		return c.JSON(map[string]any{
			"status":  "ok",
			"workers": workers,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/native-stacks")
}
