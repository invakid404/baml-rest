//go:build debug

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

func registerDebugEndpoints(r chi.Router, logger zerolog.Logger, workerPool *pool.Pool) {
	// Kill a worker that has in-flight requests (for testing mid-stream worker death)
	r.Post("/_debug/kill-worker", func(w http.ResponseWriter, r *http.Request) {
		result, err := workerPool.KillWorkerWithInFlight()
		if err != nil {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, map[string]interface{}{
				"status": "no_worker_found",
				"error":  err.Error(),
			})
			return
		}

		render.JSON(w, r, map[string]interface{}{
			"status":          "killed",
			"worker_id":       result.WorkerID,
			"in_flight_count": result.InFlightCount,
			"got_first_byte":  result.GotFirstByte,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/kill-worker")

	// Configure pool settings (for testing)
	r.Post("/_debug/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FirstByteTimeoutMs *int64 `json:"first_byte_timeout_ms,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, map[string]interface{}{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}

		if req.FirstByteTimeoutMs != nil {
			workerPool.SetFirstByteTimeout(time.Duration(*req.FirstByteTimeoutMs) * time.Millisecond)
			logger.Info().Int64("first_byte_timeout_ms", *req.FirstByteTimeoutMs).Msg("Updated FirstByteTimeout")
		}

		render.JSON(w, r, map[string]interface{}{
			"status":                "ok",
			"first_byte_timeout_ms": workerPool.GetFirstByteTimeout().Milliseconds(),
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/config")

	// Get goroutine stacks for leak detection
	// Returns pprof goroutine data with optional filtering (case-insensitive)
	r.Get("/_debug/goroutines", func(w http.ResponseWriter, r *http.Request) {
		// Get debug level (1 = counts, 2 = full stacks)
		debugLevel := 2
		if lvl := r.URL.Query().Get("debug"); lvl != "" {
			if parsed, err := strconv.Atoi(lvl); err == nil {
				debugLevel = parsed
			}
		}

		// Optional filter patterns (comma-separated, case-insensitive matching)
		filterPatterns := r.URL.Query().Get("filter")

		// Capture goroutine profile from main process
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, debugLevel); err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]interface{}{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}

		stacks := buf.String()
		totalCount := runtime.NumGoroutine()

		// If filter patterns provided, count matching goroutines (case-insensitive)
		var matchCount int
		var matchedStacks []string
		if filterPatterns != "" {
			patterns := strings.Split(filterPatterns, ",")
			goroutineStacks := strings.Split(stacks, "goroutine ")
			for _, stack := range goroutineStacks {
				if stack == "" {
					continue
				}
				stackLower := strings.ToLower(stack)
				for _, pattern := range patterns {
					pattern = strings.TrimSpace(pattern)
					patternLower := strings.ToLower(pattern)
					if patternLower != "" && strings.Contains(stackLower, patternLower) {
						matchCount++
						// Truncate for readability
						if len(stack) > 1000 {
							stack = stack[:1000] + "..."
						}
						matchedStacks = append(matchedStacks, "goroutine "+stack)
						break
					}
				}
			}
		}

		// Get worker goroutine data
		workerResults := workerPool.GetAllWorkersGoroutines(r.Context(), filterPatterns)

		// Build worker results for response
		workers := make([]map[string]interface{}, 0, len(workerResults))
		for _, wr := range workerResults {
			workerData := map[string]interface{}{
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

		response := map[string]interface{}{
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

		render.JSON(w, r, response)
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/goroutines")

	r.Get("/_debug/gc", func(w http.ResponseWriter, r *http.Request) {
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
		workerResults := workerPool.TriggerAllWorkersGC(r.Context())

		// Build worker results for response
		workers := make([]map[string]interface{}, 0, len(workerResults))
		for _, wr := range workerResults {
			workerData := map[string]interface{}{
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

		render.JSON(w, r, map[string]interface{}{
			"status": "ok",
			"main_process": map[string]interface{}{
				"before": map[string]interface{}{
					"alloc":         memBefore.Alloc,
					"total_alloc":   memBefore.TotalAlloc,
					"sys":           memBefore.Sys,
					"heap_alloc":    memBefore.HeapAlloc,
					"heap_sys":      memBefore.HeapSys,
					"heap_idle":     memBefore.HeapIdle,
					"heap_inuse":    memBefore.HeapInuse,
					"heap_released": memBefore.HeapReleased,
				},
				"after": map[string]interface{}{
					"alloc":         memAfter.Alloc,
					"total_alloc":   memAfter.TotalAlloc,
					"sys":           memAfter.Sys,
					"heap_alloc":    memAfter.HeapAlloc,
					"heap_sys":      memAfter.HeapSys,
					"heap_idle":     memAfter.HeapIdle,
					"heap_inuse":    memAfter.HeapInuse,
					"heap_released": memAfter.HeapReleased,
				},
				"freed": map[string]interface{}{
					"heap_alloc":    memBefore.HeapAlloc - memAfter.HeapAlloc,
					"heap_inuse":    memBefore.HeapInuse - memAfter.HeapInuse,
					"heap_released": memAfter.HeapReleased - memBefore.HeapReleased,
				},
			},
			"workers": workers,
		})
	})
	logger.Info().Msg("Debug endpoints enabled: /_debug/gc")
}
