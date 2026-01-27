package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/metrics"
	"github.com/go-chi/render"
	"github.com/goccy/go-json"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/httplogger"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/pool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/tmaxmax/go-sse"
	"google.golang.org/protobuf/proto"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed worker
var workerBinary []byte

//go:embed openapi.json
var openapiJSON []byte

type sseContextKey string

const (
	sseContextKeyTopic = sseContextKey("topic")
	sseContextKeyReady = sseContextKey("ready")
)

var (
	port             int
	poolSize         int
	firstByteTimeout time.Duration
	prettyLogs       bool
	memLimit         int64
)

var rootCmd = &cobra.Command{
	Use:   "baml-rest",
	Short: "BAML REST API server",
}

// extractWorker extracts the embedded worker binary to a cache location
// Returns the path to the extracted binary
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

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the BAML REST API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Initialize zerolog logger
		var output io.Writer = os.Stdout
		if prettyLogs {
			output = zerolog.ConsoleWriter{Out: os.Stdout}
		}
		logger := zerolog.New(output).With().Timestamp().Logger()

		// Configure memory limits
		totalMem := memLimit
		if totalMem == 0 {
			totalMem = memlimit.DetectAvailable()
		}

		var workerMemLimit int64
		if totalMem > 0 {
			serverMem, workerMem := memlimit.CalculateLimits(totalMem, poolSize)
			workerMemLimit = workerMem

			memlimit.SetGOMEMLIMIT(serverMem)

			// Start RSS monitor to trigger GC when native memory pressure is high.
			// Returns a stop function that's called during shutdown.
			rssThreshold := serverMem * 8 / 10
			stopRSSMonitor := memlimit.StartRSSMonitor(memlimit.RSSMonitorConfig{
				Threshold: rssThreshold,
				Interval:  5 * time.Second,
				OnGC: func(rssBefore, rssAfter int64, result memlimit.GCResult) {
					logger.Debug().
						Str("rss_before", memlimit.FormatBytes(rssBefore)).
						Str("rss_after", memlimit.FormatBytes(rssAfter)).
						Str("threshold", memlimit.FormatBytes(rssThreshold)).
						Str("result", result.String()).
						Msg("RSS-triggered GC completed")
				},
			})
			defer stopRSSMonitor()

			logger.Info().
				Str("total", memlimit.FormatBytes(totalMem)).
				Str("server", memlimit.FormatBytes(serverMem)).
				Str("per_worker", memlimit.FormatBytes(workerMem)).
				Int("workers", poolSize).
				Msg("Memory limits configured")
		} else {
			logger.Warn().Msg("Could not detect memory limit, GOMEMLIMIT not set")
		}

		// Load OpenAPI schema from embedded JSON
		var schema openapi3.T
		if err := json.Unmarshal(openapiJSON, &schema); err != nil {
			return fmt.Errorf("failed to parse embedded OpenAPI schema: %w", err)
		}

		// Extract method names from schema paths
		const callPrefix = "/call/"
		methodNames := make([]string, 0)
		for path := range schema.Paths.Map() {
			// Extract method name from /call/{methodName}
			if methodName := strings.TrimPrefix(path, callPrefix); methodName != path {
				methodNames = append(methodNames, methodName)
			}
		}

		// Extract worker binary
		workerPath, err := extractWorker(logger)
		if err != nil {
			return fmt.Errorf("failed to extract worker binary: %w", err)
		}
		logger.Info().Str("path", workerPath).Msg("Worker binary extracted")

		// Initialize worker pool with external worker binary
		poolConfig := pool.DefaultConfig()
		poolConfig.PoolSize = poolSize
		poolConfig.FirstByteTimeout = firstByteTimeout
		poolConfig.LogOutput = os.Stdout
		poolConfig.PrettyLogs = prettyLogs
		poolConfig.WorkerPath = workerPath
		poolConfig.WorkerMemLimit = workerMemLimit

		workerPool, err := pool.New(poolConfig)
		if err != nil {
			return fmt.Errorf("failed to create worker pool: %w", err)
		}

		logger.Info().Int("size", poolSize).Msg("Worker pool initialized")

		// Create Chi router
		r := chi.NewRouter()

		// Pre-allocate SSE event kinds
		sseErrorKind, err := sse.NewType("error")
		if err != nil {
			panic(err)
		}
		sseResetKind, err := sse.NewType("reset")
		if err != nil {
			panic(err)
		}
		sseFinalKind, err := sse.NewType("final")
		if err != nil {
			panic(err)
		}

		// Create SSE server
		s := &sse.Server{
			OnSession: func(_ http.ResponseWriter, req *http.Request) (topics []string, permitted bool) {
				topic, ok := req.Context().Value(sseContextKeyTopic).(string)
				if !ok {
					return nil, false
				}

				// Signal that SSE connection is ready for publishing
				if ready, ok := req.Context().Value(sseContextKeyReady).(chan struct{}); ok {
					close(ready)
				}

				return []string{topic}, true
			},
		}

		// Set global panic recovery handler to use structured logging
		recovery.ErrorHandler = func(err error) {
			logger.Error().Err(err).Msg("Unhandled panic recovered")
		}

		r.Use(middleware.Recoverer)

		// Create combined gatherer that includes main process and worker metrics
		combinedGatherer := &combinedMetricsGatherer{
			prefix:       "bamlrest_",
			mainGatherer: prometheus.DefaultGatherer,
			pool:         workerPool,
		}

		// Add /metrics endpoint for Prometheus scraping (no HTTP logging to reduce noise)
		r.Handle("/metrics", promhttp.HandlerFor(
			combinedGatherer,
			promhttp.HandlerOpts{},
		))

		// Register debug endpoints (no-op unless built with -tags=debug)
		registerDebugEndpoints(r, logger, workerPool)

		// Routes with HTTP request logging and metrics
		r.Group(func(r chi.Router) {
			r.Use(metrics.Collector(metrics.CollectorOpts{}))
			r.Use(middleware.RequestID)
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if reqID := middleware.GetReqID(r.Context()); reqID != "" {
						w.Header().Set("X-Request-Id", reqID)
					}
					next.ServeHTTP(w, r)
				})
			})

			r.Use(httplogger.RequestLogger(logger, &httplogger.Options{
				RecoverPanics: true,
			}))

			// Add /openapi.json endpoint
			r.Get("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
				render.JSON(w, r, &schema)
			})

			// Add /openapi.yaml endpoint
			r.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/yaml")
				yamlData, err := yaml.Marshal(&schema)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_, _ = w.Write(yamlData)
			})

			for _, methodName := range methodNames {
				methodName := methodName // capture for closure

				// Skip the internal dynamic method - it has dedicated endpoints
				if methodName == bamlutils.DynamicEndpointName {
					continue
				}

				logger.Info().Str("prompt", methodName).Msg("Registering prompt")

				makeCallHandler := func(streamMode bamlutils.StreamMode) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						ctx, cancel := context.WithCancel(r.Context())
						defer cancel()

						rawBody, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
							return
						}

						result, err := workerPool.Call(ctx, methodName, rawBody, streamMode)
						if err != nil {
							httplogger.SetError(r.Context(), err)
							http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
							return
						}

						w.Header().Set("Content-Type", "application/json")
						if streamMode.NeedsRaw() {
							render.JSON(w, r, CallWithRawResponse{
								Data: result.Data,
								Raw:  result.Raw,
							})
						} else {
							_, _ = w.Write(result.Data)
						}
					}
				}

				r.Post(fmt.Sprintf("/call/%s", methodName), makeCallHandler(bamlutils.StreamModeCall))
				r.Post(fmt.Sprintf("/call-with-raw/%s", methodName), makeCallHandler(bamlutils.StreamModeCallWithRaw))

				makeStreamHandler := func(pathPrefix string, streamMode bamlutils.StreamMode) http.HandlerFunc {
					config := &StreamHandlerConfig{
						SSEServer:    s,
						SSEErrorType: sseErrorKind,
						SSEResetType: sseResetKind,
						SSEFinalType: sseFinalKind,
						PathPrefix:   pathPrefix,
					}
					return func(w http.ResponseWriter, r *http.Request) {
						rawBody, err := io.ReadAll(r.Body)
						if err != nil {
							httplogger.SetError(r.Context(), err)
							http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
							return
						}

						HandleStream(w, r, methodName, rawBody, streamMode, workerPool, config, false)
					}
				}

				r.Post(fmt.Sprintf("/stream/%s", methodName), makeStreamHandler("stream", bamlutils.StreamModeStream))
				r.Post(fmt.Sprintf("/stream-with-raw/%s", methodName), makeStreamHandler("stream-with-raw", bamlutils.StreamModeStreamWithRaw))

				// Parse endpoint - parses raw LLM output using this method's schema
				r.Post(fmt.Sprintf("/parse/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()

					rawBody, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
						return
					}

					result, err := workerPool.Parse(ctx, methodName, rawBody)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Error parsing with %s: %v", methodName, err), http.StatusInternalServerError)
						return
					}

					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(result.Data)
				})
			}

			// Dynamic endpoint handlers (only if dynamic method exists - requires BAML >= 0.215.0)
			hasDynamicMethod := slices.Contains(methodNames, bamlutils.DynamicEndpointName)
			if !hasDynamicMethod {
				logger.Info().Msg("Dynamic endpoints not available (BAML < 0.215.0 or dynamic.baml not injected)")
			} else {
				logger.Info().Str("endpoint", bamlutils.DynamicEndpointName).Msg("Registering dynamic prompt")

			makeDynamicCallHandler := func(streamMode bamlutils.StreamMode) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()

					rawBody, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
						return
					}

					// Parse and validate dynamic input
					var input bamlutils.DynamicInput
					if err := json.Unmarshal(rawBody, &input); err != nil {
						http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
						return
					}

					if err := input.Validate(); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}

					// Convert to internal format
					workerInput, err := input.ToWorkerInput()
					if err != nil {
						http.Error(w, fmt.Sprintf("Failed to convert input: %v", err), http.StatusInternalServerError)
						return
					}

					result, err := workerPool.Call(ctx, bamlutils.DynamicMethodName, workerInput, streamMode)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Error calling dynamic prompt: %v", err), http.StatusInternalServerError)
						return
					}

					// Flatten DynamicProperties to root level for better UX
					flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Error flattening response: %v", err), http.StatusInternalServerError)
						return
					}

					w.Header().Set("Content-Type", "application/json")
					if streamMode.NeedsRaw() {
						render.JSON(w, r, CallWithRawResponse{
							Data: flattenedData,
							Raw:  result.Raw,
						})
					} else {
						_, _ = w.Write(flattenedData)
					}
				}
			}

			r.Post(fmt.Sprintf("/call/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCall))
			r.Post(fmt.Sprintf("/call-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCallWithRaw))

			makeDynamicStreamHandler := func(pathPrefix string, streamMode bamlutils.StreamMode) http.HandlerFunc {
				config := &StreamHandlerConfig{
					SSEServer:    s,
					SSEErrorType: sseErrorKind,
					SSEResetType: sseResetKind,
					SSEFinalType: sseFinalKind,
					PathPrefix:   pathPrefix,
				}
				return func(w http.ResponseWriter, r *http.Request) {
					rawBody, err := io.ReadAll(r.Body)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
						return
					}

					// Parse and validate dynamic input
					var input bamlutils.DynamicInput
					if err := json.Unmarshal(rawBody, &input); err != nil {
						http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
						return
					}

					if err := input.Validate(); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}

					// Convert to internal format
					workerInput, err := input.ToWorkerInput()
					if err != nil {
						http.Error(w, fmt.Sprintf("Failed to convert input: %v", err), http.StatusInternalServerError)
						return
					}

					HandleStream(w, r, bamlutils.DynamicMethodName, workerInput, streamMode, workerPool, config, true)
				}
			}

			r.Post(fmt.Sprintf("/stream/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler("stream", bamlutils.StreamModeStream))
			r.Post(fmt.Sprintf("/stream-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler("stream-with-raw", bamlutils.StreamModeStreamWithRaw))

			// Dynamic parse endpoint
			r.Post(fmt.Sprintf("/parse/%s", bamlutils.DynamicEndpointName), func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				// Parse and validate dynamic parse input
				var input bamlutils.DynamicParseInput
				if err := json.Unmarshal(rawBody, &input); err != nil {
					http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
					return
				}

				if err := input.Validate(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				// Convert to internal format
				workerInput, err := input.ToWorkerInput()
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to convert input: %v", err), http.StatusInternalServerError)
					return
				}

				result, err := workerPool.Parse(ctx, bamlutils.DynamicMethodName, workerInput)
				if err != nil {
					httplogger.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error parsing with dynamic prompt: %v", err), http.StatusInternalServerError)
					return
				}

				// Flatten DynamicProperties to root level for better UX
				flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
				if err != nil {
					httplogger.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error flattening response: %v", err), http.StatusInternalServerError)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(flattenedData)
			})
			}
		})

		// Create HTTP server
		srv := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: r,
		}

		// Start server in a goroutine
		serverErr := make(chan error, 1)
		go recovery.GoHandler(func(err error) {
			serverErr <- err
		}, func() error {
			logger.Info().Int("port", port).Msg("Starting server")
			logger.Info().Msgf("OpenAPI JSON: http://localhost:%d/openapi.json", port)
			logger.Info().Msgf("OpenAPI YAML: http://localhost:%d/openapi.yaml", port)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})

		// Set up signal handling for graceful shutdown
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		// Wait for interrupt signal or server error
		select {
		case err := <-serverErr:
			_ = workerPool.Close()
			return fmt.Errorf("server error: %w", err)
		case sig := <-sigChan:
			logger.Info().Str("signal", sig.String()).Msg("Received signal, initiating graceful shutdown")

			// Create shutdown context with timeout
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer shutdownCancel()

			// Shutdown HTTP server - this will wait for active connections to finish
			// Active connections are waiting on worker responses, so workers must stay alive
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error().Err(err).Msg("Error during HTTP server shutdown")
				_ = workerPool.Close()
				return fmt.Errorf("shutdown error: %w", err)
			}

			logger.Info().Msg("HTTP server stopped, shutting down worker pool")

			// Gracefully shutdown worker pool - waits for any remaining in-flight requests
			if err := workerPool.Shutdown(shutdownCtx); err != nil {
				logger.Error().Err(err).Msg("Error during worker pool shutdown")
				return fmt.Errorf("worker pool shutdown error: %w", err)
			}

			logger.Info().Msg("Server gracefully stopped")
			return nil
		}
	},
}

func init() {
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
	serveCmd.Flags().IntVar(&poolSize, "pool-size", 4, "Number of workers in the pool")
	serveCmd.Flags().DurationVar(&firstByteTimeout, "first-byte-timeout", 120*time.Second, "Timeout for first byte from worker (deadlock detection)")
	serveCmd.Flags().BoolVar(&prettyLogs, "pretty", false, "Use pretty console logging instead of structured JSON")
	serveCmd.Flags().Int64Var(&memLimit, "mem-limit", 0, "Total memory limit in bytes (0 = auto-detect from cgroups/system)")

	rootCmd.AddCommand(serveCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
		logger.Fatal().Err(err).Msg("Command failed")
	}
}

// CallWithRawResponse is the response format for the /call-with-raw endpoint
type CallWithRawResponse struct {
	Data json.RawMessage `json:"data"`
	Raw  string          `json:"raw"`
}

// combinedMetricsGatherer gathers metrics from the main process and all workers
type combinedMetricsGatherer struct {
	prefix       string
	mainGatherer prometheus.Gatherer
	pool         *pool.Pool
}

func (g *combinedMetricsGatherer) Gather() ([]*dto.MetricFamily, error) {
	// Gather main process metrics
	mainMfs, err := g.mainGatherer.Gather()
	if err != nil {
		return nil, err
	}

	// Add prefix to main process metrics and mark with process="main"
	mainLabel := "main"
	processLabelName := "process"
	for _, mf := range mainMfs {
		name := g.prefix + mf.GetName()
		mf.Name = &name
		// Add process="main" label to each metric
		for _, m := range mf.Metric {
			m.Label = append(m.Label, &dto.LabelPair{
				Name:  &processLabelName,
				Value: &mainLabel,
			})
		}
	}

	// Gather worker metrics
	workerMetrics := g.pool.GatherWorkerMetrics(context.Background())

	// Deserialize and merge worker metrics
	for _, wm := range workerMetrics {
		workerLabel := fmt.Sprintf("worker_%d", wm.WorkerID)

		for _, mfBytes := range wm.MetricFamilies {
			var mf dto.MetricFamily
			if err := proto.Unmarshal(mfBytes, &mf); err != nil {
				continue // Skip malformed metrics
			}

			// Add prefix and process label
			name := g.prefix + mf.GetName()
			mf.Name = &name
			for _, m := range mf.Metric {
				m.Label = append(m.Label, &dto.LabelPair{
					Name:  &processLabelName,
					Value: &workerLabel,
				})
			}

			// Merge into main metrics (find existing or append)
			merged := false
			for _, existingMf := range mainMfs {
				if existingMf.GetName() == mf.GetName() {
					existingMf.Metric = append(existingMf.Metric, mf.Metric...)
					merged = true
					break
				}
			}
			if !merged {
				mainMfs = append(mainMfs, &mf)
			}
		}
	}

	return mainMfs, nil
}
