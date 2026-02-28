package main

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/goccy/go-json"
	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/gofiber/fiber/v3"
	fiberadaptor "github.com/gofiber/fiber/v3/middleware/adaptor"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/pool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
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
	sseContextKeyTopic  = sseContextKey("topic")
	sseContextKeyReady  = sseContextKey("ready")
	maxRequestBodyBytes = 4 * 1024 * 1024
)

var errRequestBodyTooLarge = errors.New("request body too large")

var (
	port                 int
	poolSize             int
	firstByteTimeout     time.Duration
	sseKeepaliveInterval time.Duration
	prettyLogs           bool
	memLimit             int64
)

var rootCmd = &cobra.Command{
	Use:   "baml-rest",
	Short: "BAML REST API server",
}

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests processed by the server",
		},
		[]string{"method", "path", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "status"},
	)
	registerHTTPMetricsOnce sync.Once
	registerHTTPMetricsErr  error
)

func registerHTTPMetrics() error {
	registerHTTPMetricsOnce.Do(func() {
		for _, collector := range []prometheus.Collector{httpRequestsTotal, httpRequestDuration} {
			if err := prometheus.Register(collector); err != nil {
				if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
					continue
				}
				registerHTTPMetricsErr = err
				return
			}
		}
	})

	return registerHTTPMetricsErr
}

func httpMetricsMiddleware(c fiber.Ctx) error {
	start := time.Now()
	err := c.Next()

	statusCode := c.Response().StatusCode()
	if err != nil {
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			statusCode = fiberErr.Code
			if statusCode == 0 {
				statusCode = fiber.StatusInternalServerError
			}
		} else if statusCode < 400 {
			statusCode = fiber.StatusInternalServerError
		}
	}

	status := strconv.Itoa(statusCode)
	path := c.FullPath()
	if path == "" {
		path = "_unmatched"
	}
	httpRequestsTotal.WithLabelValues(c.Method(), path, status).Inc()
	httpRequestDuration.WithLabelValues(c.Method(), path, status).Observe(time.Since(start).Seconds())

	return err
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
		if sseKeepaliveInterval < minSSEKeepaliveInterval {
			return fmt.Errorf("--sse-keepalive-interval must be >= %s", minSSEKeepaliveInterval)
		}

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

		openapiYAML, err := yaml.Marshal(&schema)
		if err != nil {
			return fmt.Errorf("failed to generate OpenAPI YAML: %w", err)
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
		slices.Sort(methodNames)

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

		// Create Fiber app
		app := fiber.New(fiber.Config{
			JSONEncoder: json.Marshal,
			JSONDecoder: json.Unmarshal,
			BodyLimit:   maxRequestBodyBytes,
		})

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

		app.Use(fiberrecover.New())

		// Create combined gatherer that includes main process and worker metrics
		combinedGatherer := &combinedMetricsGatherer{
			prefix:       "bamlrest_",
			mainGatherer: prometheus.DefaultGatherer,
			pool:         workerPool,
		}

		// Add /metrics endpoint for Prometheus scraping (no HTTP logging to reduce noise)
		app.Get("/metrics", func(c fiber.Ctx) error {
			metricFamilies, err := combinedGatherer.Gather()
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).SendString("failed to gather metrics\n")
			}

			var buf bytes.Buffer
			encoder := expfmt.NewEncoder(&buf, expfmt.FmtText)
			for _, mf := range metricFamilies {
				if err := encoder.Encode(mf); err != nil {
					return c.Status(fiber.StatusInternalServerError).SendString("failed to encode metrics\n")
				}
			}

			c.Set(fiber.HeaderContentType, string(expfmt.FmtText))
			return c.Send(buf.Bytes())
		})

		// Register debug endpoints (no-op unless built with -tags=debug)
		registerDebugEndpoints(app, logger, workerPool)

		// Routes with HTTP request logging and metrics
		if err := registerHTTPMetrics(); err != nil {
			_ = workerPool.Close()
			return fmt.Errorf("failed to register HTTP metrics: %w", err)
		}
		app.Use(httpMetricsMiddleware)
		app.Use(fiberrequestid.New(fiberrequestid.Config{
			Header: "X-Request-Id",
		}))
		app.Use(fiberzerolog.New(fiberzerolog.Config{
			Logger: &logger,
			Next: func(c fiber.Ctx) bool {
				path := c.Path()
				return path == "/metrics" || strings.HasPrefix(path, "/_debug")
			},
			Fields: []string{
				fiberzerolog.FieldLatency,
				fiberzerolog.FieldStatus,
				fiberzerolog.FieldMethod,
				fiberzerolog.FieldURL,
				fiberzerolog.FieldRequestID,
				fiberzerolog.FieldError,
			},
		}))

		// Add /openapi.json endpoint
		app.Get("/openapi.json", func(c fiber.Ctx) error {
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			return c.Status(fiber.StatusOK).Send(openapiJSON)
		})

		// Add /openapi.yaml endpoint
		app.Get("/openapi.yaml", func(c fiber.Ctx) error {
			c.Set(fiber.HeaderContentType, "application/yaml")
			return c.Status(fiber.StatusOK).Send(openapiYAML)
		})

		for _, methodName := range methodNames {
			methodName := methodName // capture for closure

			// Skip the internal dynamic method - it has dedicated endpoints
			if methodName == bamlutils.DynamicEndpointName {
				continue
			}

			logger.Info().Str("prompt", methodName).Msg("Registering prompt")

			makeCallHandler := func(streamMode bamlutils.StreamMode) fiber.Handler {
				return func(c fiber.Ctx) error {
					ctx, cancel := context.WithCancel(c.RequestCtx())
					defer cancel()

					result, err := workerPool.Call(ctx, methodName, c.Body(), streamMode)
					if err != nil {
						logger.Error().Err(err).Str("method", methodName).Msg("worker call failed")
						return writeFiberJSONError(c, "failed to process request", fiber.StatusInternalServerError)
					}

					c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
					if streamMode.NeedsRaw() {
						return c.Status(fiber.StatusOK).JSON(CallWithRawResponse{
							Data: result.Data,
							Raw:  result.Raw,
						})
					}

					return c.Status(fiber.StatusOK).Send(result.Data)
				}
			}

			app.Post(fmt.Sprintf("/call/%s", methodName), makeCallHandler(bamlutils.StreamModeCall))
			app.Post(fmt.Sprintf("/call-with-raw/%s", methodName), makeCallHandler(bamlutils.StreamModeCallWithRaw))

			makeStreamHandler := func(pathPrefix string, streamMode bamlutils.StreamMode) fiber.Handler {
				config := &StreamHandlerConfig{
					SSEServer:            s,
					SSEErrorType:         sseErrorKind,
					SSEResetType:         sseResetKind,
					SSEFinalType:         sseFinalKind,
					SSEKeepaliveInterval: sseKeepaliveInterval,
					PathPrefix:           pathPrefix,
				}

				sseHandler := fiberadaptor.HTTPHandlerWithContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rawBody, err := readRequestBodyLimited(r)
					if err != nil {
						statusCode := http.StatusBadRequest
						message := "failed to read request body"
						if errors.Is(err, errRequestBodyTooLarge) {
							statusCode = http.StatusRequestEntityTooLarge
							message = "request body too large"
						}
						logger.Error().Err(err).Str("method", methodName).Msg("failed to read stream request body")
						writeJSONError(w, r, message, statusCode)
						return
					}

					HandleStream(w, r, methodName, rawBody, streamMode, workerPool, config, false)
				}))

				return func(c fiber.Ctx) error {
					if NegotiateStreamFormatFromAccept(c.Get(fiber.HeaderAccept)) == StreamFormatNDJSON {
						return HandleNDJSONStreamFiber(c, methodName, c.Body(), streamMode, workerPool, false)
					}

					return sseHandler(c)
				}
			}

			app.Post(fmt.Sprintf("/stream/%s", methodName), makeStreamHandler("stream", bamlutils.StreamModeStream))
			app.Post(fmt.Sprintf("/stream-with-raw/%s", methodName), makeStreamHandler("stream-with-raw", bamlutils.StreamModeStreamWithRaw))

			// Parse endpoint - parses raw LLM output using this method's schema
			app.Post(fmt.Sprintf("/parse/%s", methodName), func(c fiber.Ctx) error {
				ctx, cancel := context.WithCancel(c.RequestCtx())
				defer cancel()

				result, err := workerPool.Parse(ctx, methodName, c.Body())
				if err != nil {
					logger.Error().Err(err).Str("method", methodName).Msg("worker parse failed")
					return writeFiberJSONError(c, "failed to parse response", fiber.StatusInternalServerError)
				}

				c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
				return c.Status(fiber.StatusOK).Send(result.Data)
			})
		}

		// Dynamic endpoint handlers (only if dynamic method exists - requires BAML >= 0.215.0)
		hasDynamicMethod := slices.Contains(methodNames, bamlutils.DynamicEndpointName)
		if !hasDynamicMethod {
			logger.Info().Msg("Dynamic endpoints not available (BAML < 0.215.0 or dynamic.baml not injected)")
		} else {
			logger.Info().Str("endpoint", bamlutils.DynamicEndpointName).Msg("Registering dynamic prompt")

			makeDynamicCallHandler := func(streamMode bamlutils.StreamMode) fiber.Handler {
				return func(c fiber.Ctx) error {
					ctx, cancel := context.WithCancel(c.RequestCtx())
					defer cancel()

					rawBody := c.Body()

					// Parse and validate dynamic input
					var input bamlutils.DynamicInput
					if err := json.Unmarshal(rawBody, &input); err != nil {
						return writeFiberJSONError(c, "invalid JSON payload", fiber.StatusBadRequest)
					}

					if err := input.Validate(); err != nil {
						return writeFiberJSONError(c, err.Error(), fiber.StatusBadRequest)
					}

					// Convert to internal format
					workerInput, err := input.ToWorkerInput()
					if err != nil {
						logger.Error().Err(err).Msg("dynamic input conversion failed")
						return writeFiberJSONError(c, "failed to process dynamic input", fiber.StatusInternalServerError)
					}

					result, err := workerPool.Call(ctx, bamlutils.DynamicMethodName, workerInput, streamMode)
					if err != nil {
						logger.Error().Err(err).Msg("dynamic worker call failed")
						return writeFiberJSONError(c, "failed to process request", fiber.StatusInternalServerError)
					}

					// Flatten DynamicProperties to root level for better UX
					flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
					if err != nil {
						logger.Error().Err(err).Msg("dynamic response flatten failed")
						return writeFiberJSONError(c, "failed to process response", fiber.StatusInternalServerError)
					}

					c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
					if streamMode.NeedsRaw() {
						return c.Status(fiber.StatusOK).JSON(CallWithRawResponse{
							Data: flattenedData,
							Raw:  result.Raw,
						})
					}

					return c.Status(fiber.StatusOK).Send(flattenedData)
				}
			}

			app.Post(fmt.Sprintf("/call/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCall))
			app.Post(fmt.Sprintf("/call-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCallWithRaw))

			makeDynamicStreamHandler := func(pathPrefix string, streamMode bamlutils.StreamMode) fiber.Handler {
				config := &StreamHandlerConfig{
					SSEServer:            s,
					SSEErrorType:         sseErrorKind,
					SSEResetType:         sseResetKind,
					SSEFinalType:         sseFinalKind,
					SSEKeepaliveInterval: sseKeepaliveInterval,
					PathPrefix:           pathPrefix,
				}

				sseHandler := fiberadaptor.HTTPHandlerWithContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rawBody, err := readRequestBodyLimited(r)
					if err != nil {
						statusCode := http.StatusBadRequest
						message := "failed to read request body"
						if errors.Is(err, errRequestBodyTooLarge) {
							statusCode = http.StatusRequestEntityTooLarge
							message = "request body too large"
						}
						logger.Error().Err(err).Str("method", bamlutils.DynamicMethodName).Msg("failed to read dynamic stream request body")
						writeJSONError(w, r, message, statusCode)
						return
					}

					workerInput, statusCode, err := parseDynamicStreamInput(rawBody)
					if err != nil {
						message := err.Error()
						if statusCode >= http.StatusInternalServerError {
							logger.Error().Err(err).Msg("dynamic stream input parsing failed")
							message = "failed to process dynamic stream input"
						}
						writeJSONError(w, r, message, statusCode)
						return
					}

					HandleStream(w, r, bamlutils.DynamicMethodName, workerInput, streamMode, workerPool, config, true)
				}))

				return func(c fiber.Ctx) error {
					if NegotiateStreamFormatFromAccept(c.Get(fiber.HeaderAccept)) == StreamFormatNDJSON {
						workerInput, statusCode, err := parseDynamicStreamInput(c.Body())
						if err != nil {
							message := err.Error()
							if statusCode >= fiber.StatusInternalServerError {
								logger.Error().Err(err).Msg("dynamic NDJSON input parsing failed")
								message = "failed to process dynamic stream input"
							}
							return writeFiberJSONError(c, message, statusCode)
						}

						return HandleNDJSONStreamFiber(c, bamlutils.DynamicMethodName, workerInput, streamMode, workerPool, true)
					}

					return sseHandler(c)
				}
			}

			app.Post(fmt.Sprintf("/stream/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler("stream", bamlutils.StreamModeStream))
			app.Post(fmt.Sprintf("/stream-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler("stream-with-raw", bamlutils.StreamModeStreamWithRaw))

			// Dynamic parse endpoint
			app.Post(fmt.Sprintf("/parse/%s", bamlutils.DynamicEndpointName), func(c fiber.Ctx) error {
				ctx, cancel := context.WithCancel(c.RequestCtx())
				defer cancel()

				rawBody := c.Body()

				// Parse and validate dynamic parse input
				var input bamlutils.DynamicParseInput
				if err := json.Unmarshal(rawBody, &input); err != nil {
					return writeFiberJSONError(c, "invalid JSON payload", fiber.StatusBadRequest)
				}
				if err := input.Validate(); err != nil {
					return writeFiberJSONError(c, err.Error(), fiber.StatusBadRequest)
				}

				// Convert to internal format
				workerInput, err := input.ToWorkerInput()
				if err != nil {
					logger.Error().Err(err).Msg("dynamic parse input conversion failed")
					return writeFiberJSONError(c, "failed to process dynamic input", fiber.StatusInternalServerError)
				}

				result, err := workerPool.Parse(ctx, bamlutils.DynamicMethodName, workerInput)
				if err != nil {
					logger.Error().Err(err).Msg("dynamic worker parse failed")
					return writeFiberJSONError(c, "failed to parse response", fiber.StatusInternalServerError)
				}

				// Flatten DynamicProperties to root level for better UX
				flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
				if err != nil {
					logger.Error().Err(err).Msg("dynamic parse response flatten failed")
					return writeFiberJSONError(c, "failed to process response", fiber.StatusInternalServerError)
				}

				c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
				return c.Status(fiber.StatusOK).Send(flattenedData)
			})
		}

		// Start server in a goroutine
		serverErr := make(chan error, 1)
		go recovery.GoHandler(func(err error) {
			serverErr <- err
		}, func() error {
			logger.Info().Int("port", port).Msg("Starting server")
			logger.Info().Msgf("OpenAPI JSON: http://localhost:%d/openapi.json", port)
			logger.Info().Msgf("OpenAPI YAML: http://localhost:%d/openapi.yaml", port)
			return app.Listen(fmt.Sprintf(":%d", port), fiber.ListenConfig{DisableStartupMessage: true})
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

			// Shutdown Fiber server - this will wait for active connections to finish
			// Active connections are waiting on worker responses, so workers must stay alive
			if err := app.ShutdownWithContext(shutdownCtx); err != nil {
				logger.Error().Err(err).Msg("Error during Fiber server shutdown")
				_ = workerPool.Close()
				return fmt.Errorf("shutdown error: %w", err)
			}

			logger.Info().Msg("Fiber server stopped, shutting down worker pool")

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

func readRequestBodyLimited(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxRequestBodyBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBodyBytes {
		return nil, errRequestBodyTooLarge
	}
	return body, nil
}

func parseDynamicStreamInput(rawBody []byte) (workerInput []byte, statusCode int, err error) {
	var input bamlutils.DynamicInput
	if err := json.Unmarshal(rawBody, &input); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err)
	}

	if err := input.Validate(); err != nil {
		return nil, http.StatusBadRequest, err
	}

	workerInput, err = input.ToWorkerInput()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to convert input: %w", err)
	}

	return workerInput, 0, nil
}

func init() {
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
	serveCmd.Flags().IntVar(&poolSize, "pool-size", 1, "Number of workers in the pool")
	serveCmd.Flags().DurationVar(&firstByteTimeout, "first-byte-timeout", 120*time.Second, "Timeout for first byte from worker (deadlock detection)")
	serveCmd.Flags().DurationVar(&sseKeepaliveInterval, "sse-keepalive-interval", defaultSSEKeepaliveInterval, "Interval between SSE keepalive comments (minimum 1s)")
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
