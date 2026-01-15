//go:build debug

package main

import (
	"net/http"
	"runtime"
	"runtime/debug"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

func registerDebugEndpoints(r chi.Router, logger zerolog.Logger, workerPool *pool.Pool) {
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
