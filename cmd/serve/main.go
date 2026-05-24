package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	stdjson "encoding/json"

	"github.com/bytedance/sonic"
	"github.com/getkin/kin-openapi/openapi3"
	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
	"github.com/invakid404/baml-rest/internal/apierror"
	"github.com/invakid404/baml-rest/internal/memlimit"
	"github.com/invakid404/baml-rest/internal/rootruntime"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/invakid404/baml-rest/pool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed openapi.json
var openapiJSON []byte

const (
	maxRequestBodyBytes = 4 * 1024 * 1024
)

var (
	port                    int
	unaryPort               int
	poolSize                int
	firstByteTimeout        time.Duration
	streamKeepaliveInterval time.Duration
	prettyLogs              bool
	memLimit                int64
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
		[]string{"method", "path", "code"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "code"},
	)
	httpRequestsInflight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_inflight",
			Help: "Number of HTTP requests currently being processed",
		},
	)
	registerHTTPMetricsOnce sync.Once
	registerHTTPMetricsErr  error
)

func registerHTTPMetrics() error {
	registerHTTPMetricsOnce.Do(func() {
		for _, collector := range []prometheus.Collector{httpRequestsTotal, httpRequestDuration, httpRequestsInflight} {
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
	httpRequestsInflight.Inc()
	defer httpRequestsInflight.Dec()
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

	code := strconv.Itoa(statusCode)
	path := c.FullPath()
	if path == "" {
		path = "_unmatched"
	}
	httpRequestsTotal.WithLabelValues(c.Method(), path, code).Inc()
	httpRequestDuration.WithLabelValues(c.Method(), path, code).Observe(time.Since(start).Seconds())

	return err
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

		// Mirror the SharedStateSeeds gate's availability predicate
		// below: the BuildRequest path is exercisable as long as
		// EITHER non-streaming Request or streaming StreamRequest is
		// exposed by the generated codegen. Warn only when neither
		// surface exists (streaming-only BAML still has a usable
		// BuildRequest path via StreamRequest, even when Request is
		// nil — the call bridge falls back). Without this widening
		// the warning misleadingly fires on streaming-only BAML
		// installations that are perfectly capable of using the
		// BuildRequest path.
		if introspected.Request == nil && introspected.StreamRequest == nil {
			logger.Warn().Msg("No BuildRequest surface available (neither Request nor StreamRequest exposed): all requests will use the CallStream+OnTick path. Upgrade BAML or check the generated baml_client to enable the BuildRequest code path.")
		}

		// Configure memory limits. effectivePoolSizeForMemory is
		// tag-split so the budget split and the per_worker log
		// reflect the actual layout (in-process collapses to 1
		// regardless of the requested pool-size).
		warnPoolSizeOverride(logger, poolSize, cmd.Flags().Changed("pool-size"))
		effectivePoolSize := effectivePoolSizeForMemory(poolSize)

		totalMem := memLimit
		if totalMem == 0 {
			totalMem = memlimit.DetectAvailable()
		}

		var workerMemLimit int64
		if totalMem > 0 {
			serverMem, workerMem := memlimit.CalculateLimits(totalMem, effectivePoolSize)
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
				Int("workers", effectivePoolSize).
				Msg("Memory limits configured")
		} else {
			logger.Warn().Msg("Could not detect memory limit, GOMEMLIMIT not set")
		}

		// Load OpenAPI schema from embedded JSON
		var schema openapi3.T
		if err := sonic.Unmarshal(openapiJSON, &schema); err != nil {
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

		// Resolve env-driven runtime config once at startup. The
		// resulting values are threaded into the in-process worker
		// factory so every handler in the pool observes the same
		// configuration the subprocess build's cmd/worker would
		// produce on its own at process startup.
		buildRequestConfig := buildrequest.EnvConfig()
		preserveSchemaOrderDefault := preserveSchemaOrderDefaultFromEnv()
		baseURLRewrites := urlrewrite.LoadDefaultRules()
		httpClient := llmhttp.NewDefaultClientWithOptions(llmhttp.ClientOptions{
			Mode:         llmhttp.ClientModeFromEnv(),
			RewriteRules: baseURLRewrites,
		})
		runtimeCfg := workerModeRuntimeConfig{
			Runtime:         rootruntime.Runtime{},
			BuildRequest:    buildRequestConfig,
			BaseURLRewrites: baseURLRewrites,
			HTTPClient:      httpClient,
		}

		// Initialize worker pool. configureWorkerMode is tag-split:
		// subprocess extracts the embedded worker binary and sets
		// WorkerPath; in-process initialises the BAML runtime in
		// this process and installs a WorkerFactory.
		poolConfig := pool.DefaultConfig()
		poolConfig.PoolSize = poolSize
		poolConfig.FirstByteTimeout = firstByteTimeout
		poolConfig.LogOutput = os.Stdout
		poolConfig.PrettyLogs = prettyLogs
		poolConfig.WorkerMemLimit = workerMemLimit
		if err := configureWorkerMode(logger, poolConfig, runtimeCfg); err != nil {
			return err
		}
		// Host the shared-state round-robin counters. Passing a non-nil
		// seeds map enables the reverse broker channel between host and
		// each worker — without it every worker rotates off an in-process
		// coordinator and baml-roundrobin degrades to independent per-
		// worker draws at PoolSize > 1. The map is consulted for BAML
		// `start N` values; unseeded clients get a random offset on first
		// touch, matching the legacy single-process Coordinator behaviour.
		//
		// Gated on UseBuildRequest: with the kill-switch off, baml-rest's
		// RR resolver isn't engaged on the request path (the codegen
		// upgrade gate at adapters/common/codegen/codegen.go skips
		// ResolveEffectiveClient and the worker's per-request
		// SetRoundRobinAdvancer feeds an unread advancer), so the broker
		// channel and the AttachSharedState handshake would be pure dead
		// weight. Skipping seeds collapses the kill-switch contract
		// end-to-end: no host-side store, no SharedStateImpl in the
		// plugin map, no reverse-broker dial, and BAML's per-worker
		// runtime owns rotation exactly as it did pre-PR.
		// Gate on BuildRequest enabled AND at least one BuildRequest
		// surface (Request or StreamRequest) actually exposed by the
		// generated codegen. Streaming-only BAML emits
		// `var Request any` / `var StreamRequest any` for the absent
		// surface (cmd/introspect/main.go); the codegen distinguishes
		// the two surfaces (codegen.go non-stream vs stream emission)
		// and the call bridge can fall back to StreamRequest when
		// Request is nil. Only when neither surface exists does the
		// BuildRequest path become unexercisable — wiring SharedState
		// in that case would be pure dead weight.
		if buildRequestConfig.UseBuildRequest && (introspected.Request != nil || introspected.StreamRequest != nil) {
			poolConfig.SharedStateSeeds = introspected.RoundRobinStart
		}

		workerPool, err := pool.New(poolConfig)
		if err != nil {
			return fmt.Errorf("failed to create worker pool: %w", err)
		}

		logger.Info().Int("size", workerPool.Stats().TotalWorkers).Msg("Worker pool initialized")

		// Create Fiber app
		//
		// ErrorHandler maps framework-emitted *fiber.Error values into
		// the apierror response shape so framework-level failures
		// (BodyLimit/413 in particular, but also 404 / 405 / etc.)
		// surface as the same coded JSON envelope our handlers emit
		// instead of Fiber's default text/plain body. Without this,
		// a 413 from BodyLimit bypassed writeFiberJSONErrorWithCode
		// and clients lost the request_too_large code that the chi
		// path already produces via writeChiBodyReadError. Non-
		// *fiber.Error errors (uncaught from a handler) default to
		// 500 internal_error.
		app := fiber.New(fiber.Config{
			JSONEncoder:  sonic.Marshal,
			JSONDecoder:  sonic.Unmarshal,
			BodyLimit:    maxRequestBodyBytes,
			ErrorHandler: fiberErrorHandler,
		})

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
					// Use c.Context() rather than c.RequestCtx(): fasthttp's
					// RequestCtx.Done() is server-shutdown-scoped (shared s.done
					// channel), so deriving from it cancels the worker call the
					// instant Fiber.Shutdown starts — before the pool can drain.
					ctx, cancel := context.WithCancel(c.Context())
					defer cancel()

					result, err := workerPool.Call(ctx, methodName, c.Body(), streamMode)
					// Surface routing observability headers even on the
					// error path. Pool.Call now preserves accumulated
					// planned/outcome metadata in the result on error
					// tails so X-BAML-Path / X-BAML-Path-Reason still
					// reach the client when the request fails after the
					// orchestrator emitted planned metadata. Pre-fix the
					// 500 path returned with no headers, which made
					// invalid-runtime-options diagnoses invisible.
					if result != nil {
						setBAMLHeaders(fiberHeaderSetter(c), decodeMetadataJSON(result.Planned), decodeMetadataJSON(result.Outcome))
					}
					if err != nil {
						logger.Error().Err(err).Str("method", methodName).Msg("worker call failed")
						return writeFiberWorkerError(c, err)
					}
					c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
					if streamMode.NeedsRaw() {
						return c.Status(fiber.StatusOK).JSON(CallWithRawResponse{
							Data:      result.Data,
							Raw:       result.Raw,
							Reasoning: result.Reasoning,
						})
					}

					return c.Status(fiber.StatusOK).Send(result.Data)
				}
			}

			app.Post(fmt.Sprintf("/call/%s", methodName), makeCallHandler(bamlutils.StreamModeCall))
			app.Post(fmt.Sprintf("/call-with-raw/%s", methodName), makeCallHandler(bamlutils.StreamModeCallWithRaw))

			makeStreamHandler := func(streamMode bamlutils.StreamMode) fiber.Handler {
				return func(c fiber.Ctx) error {
					switch c.Accepts(contentTypeSSE, ContentTypeNDJSON) {
					case ContentTypeNDJSON:
						return HandleNDJSONStreamFiber(c, methodName, c.Body(), streamMode, workerPool, false, nil)
					case contentTypeSSE:
						return HandleSSEStreamFiber(c, methodName, c.Body(), streamMode, workerPool, false, nil)
					default:
						return writeFiberJSONErrorWithCode(c, "Not Acceptable: server can produce text/event-stream or application/x-ndjson", apierror.CodeNotAcceptable, nil, fiber.StatusNotAcceptable)
					}
				}
			}

			app.Post(fmt.Sprintf("/stream/%s", methodName), makeStreamHandler(bamlutils.StreamModeStream))
			app.Post(fmt.Sprintf("/stream-with-raw/%s", methodName), makeStreamHandler(bamlutils.StreamModeStreamWithRaw))

			// Parse endpoint - parses raw LLM output using this method's schema
			app.Post(fmt.Sprintf("/parse/%s", methodName), func(c fiber.Ctx) error {
				ctx, cancel := context.WithCancel(c.Context())
				defer cancel()

				result, err := workerPool.Parse(ctx, methodName, c.Body())
				if err != nil {
					logger.Error().Err(err).Str("method", methodName).Msg("worker parse failed")
					return writeFiberParseWorkerError(c, err)
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
					ctx, cancel := context.WithCancel(c.Context())
					defer cancel()

					parsed, statusCode, code, err := parseDynamicCallBody(c.Body(), preserveSchemaOrderDefault)
					if err != nil {
						if statusCode >= fiber.StatusInternalServerError {
							logger.Error().Err(err).Msg("dynamic input conversion failed")
							return writeFiberInternalError(c, err)
						}
						return writeFiberJSONErrorWithCode(c, err.Error(), code, nil, statusCode)
					}

					result, err := workerPool.Call(ctx, bamlutils.DynamicMethodName, parsed.WorkerInput, streamMode)
					// Surface routing headers on the error tail too — see
					// the per-method handler above for rationale.
					if result != nil {
						setBAMLHeaders(fiberHeaderSetter(c), decodeMetadataJSON(result.Planned), decodeMetadataJSON(result.Outcome))
					}
					if err != nil {
						logger.Error().Err(err).Msg("dynamic worker call failed")
						return writeFiberWorkerError(c, err)
					}

					// Flatten DynamicProperties to root level for better UX
					flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
					if err != nil {
						logger.Error().Err(err).Msg("dynamic response flatten failed")
						return writeFiberInternalError(c, err)
					}
					if parsed.PreserveSchemaOrder {
						reordered, rerr := bamlutils.ReorderDynamicOutputBySchema(flattenedData, parsed.OutputSchema)
						if rerr != nil {
							logger.Error().Err(rerr).Msg("dynamic response reorder failed")
							return writeFiberInternalError(c, rerr)
						}
						flattenedData = reordered
					}
					c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
					if streamMode.NeedsRaw() {
						return c.Status(fiber.StatusOK).JSON(CallWithRawResponse{
							Data:      flattenedData,
							Raw:       result.Raw,
							Reasoning: result.Reasoning,
						})
					}

					return c.Status(fiber.StatusOK).Send(flattenedData)
				}
			}

			app.Post(fmt.Sprintf("/call/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCall))
			app.Post(fmt.Sprintf("/call-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicCallHandler(bamlutils.StreamModeCallWithRaw))

			makeDynamicStreamHandler := func(streamMode bamlutils.StreamMode) fiber.Handler {
				return func(c fiber.Ctx) error {
					parsed, statusCode, code, err := parseDynamicCallBody(c.Body(), preserveSchemaOrderDefault)
					if err != nil {
						if statusCode >= fiber.StatusInternalServerError {
							logger.Error().Err(err).Msg("dynamic stream input parsing failed")
							// 5xx tail: route through writeFiberInternalError so
							// httplogger.SetError attaches the error to the request-
							// scoped structured log alongside the discrete logger
							// line above. Wire shape is unchanged —
							// parseDynamicCallBody only returns statusCode>=500
							// from the ToWorkerInput path, which already classifies
							// as CodeInternalError, so the helper produces an
							// identical envelope.
							return writeFiberInternalError(c, err)
						}
						return writeFiberJSONErrorWithCode(c, err.Error(), code, nil, statusCode)
					}

					reorder := &dynamicFinalReorder{
						Schema:  parsed.OutputSchema,
						Enabled: parsed.PreserveSchemaOrder,
					}
					switch c.Accepts(contentTypeSSE, ContentTypeNDJSON) {
					case ContentTypeNDJSON:
						return HandleNDJSONStreamFiber(c, bamlutils.DynamicMethodName, parsed.WorkerInput, streamMode, workerPool, true, reorder)
					case contentTypeSSE:
						return HandleSSEStreamFiber(c, bamlutils.DynamicMethodName, parsed.WorkerInput, streamMode, workerPool, true, reorder)
					default:
						return writeFiberJSONErrorWithCode(c, "Not Acceptable: server can produce text/event-stream or application/x-ndjson", apierror.CodeNotAcceptable, nil, fiber.StatusNotAcceptable)
					}
				}
			}

			app.Post(fmt.Sprintf("/stream/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler(bamlutils.StreamModeStream))
			app.Post(fmt.Sprintf("/stream-with-raw/%s", bamlutils.DynamicEndpointName), makeDynamicStreamHandler(bamlutils.StreamModeStreamWithRaw))

			// Dynamic parse endpoint
			app.Post(fmt.Sprintf("/parse/%s", bamlutils.DynamicEndpointName), func(c fiber.Ctx) error {
				ctx, cancel := context.WithCancel(c.Context())
				defer cancel()

				parsed, statusCode, code, err := parseDynamicParseBody(c.Body(), preserveSchemaOrderDefault)
				if err != nil {
					if statusCode >= fiber.StatusInternalServerError {
						logger.Error().Err(err).Msg("dynamic parse input conversion failed")
						return writeFiberInternalError(c, err)
					}
					return writeFiberJSONErrorWithCode(c, err.Error(), code, nil, statusCode)
				}

				result, err := workerPool.Parse(ctx, bamlutils.DynamicMethodName, parsed.WorkerInput)
				if err != nil {
					logger.Error().Err(err).Msg("dynamic worker parse failed")
					return writeFiberParseWorkerError(c, err)
				}

				// Flatten DynamicProperties to root level for better UX
				flattenedData, err := bamlutils.FlattenDynamicOutput(result.Data)
				if err != nil {
					logger.Error().Err(err).Msg("dynamic parse response flatten failed")
					return writeFiberInternalError(c, err)
				}
				if parsed.PreserveSchemaOrder {
					reordered, rerr := bamlutils.ReorderDynamicOutputBySchema(flattenedData, parsed.OutputSchema)
					if rerr != nil {
						logger.Error().Err(rerr).Msg("dynamic parse response reorder failed")
						return writeFiberInternalError(c, rerr)
					}
					flattenedData = reordered
				}

				c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
				return c.Status(fiber.StatusOK).Send(flattenedData)
			})
		}

		// Optionally start the chi-based unary server on a separate port.
		// newUnaryServer returns nil when the unaryserver build tag is absent
		// or when --unary-port is set to 0.
		unaryServer := newUnaryServer(logger, workerPool, methodNames, hasDynamicMethod, preserveSchemaOrderDefault)

		// Start Fiber server in a goroutine.
		serverErr := make(chan error, 1)
		go recovery.GoHandler(func(err error) {
			serverErr <- err
		}, func() error {
			logger.Info().Int("port", port).Msg("Starting server")
			logger.Info().Msgf("OpenAPI JSON: http://localhost:%d/openapi.json", port)
			logger.Info().Msgf("OpenAPI YAML: http://localhost:%d/openapi.yaml", port)
			return app.Listen(fmt.Sprintf(":%d", port), fiber.ListenConfig{DisableStartupMessage: true})
		})

		// Start unary server in a goroutine (if enabled).
		unaryServerErr := make(chan error, 1)
		if unaryServer != nil {
			go recovery.GoHandler(func(err error) {
				unaryServerErr <- err
			}, func() error {
				logger.Info().Int("port", unaryPort).Msg("Starting unary server (chi/net-http)")
				err := unaryServer.ListenAndServe()
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			})
		}

		// Set up signal handling for graceful shutdown
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		// Wait for interrupt signal or server error
		select {
		case err := <-serverErr:
			_ = workerPool.Close()
			return fmt.Errorf("server error: %w", err)
		case err := <-unaryServerErr:
			_ = workerPool.Close()
			return fmt.Errorf("unary server error: %w", err)
		case sig := <-sigChan:
			logger.Info().Str("signal", sig.String()).Msg("Received signal, initiating graceful shutdown")

			// Create shutdown context with timeout
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer shutdownCancel()

			// Shutdown both HTTP servers — active connections wait on worker responses,
			// so workers must stay alive until all servers are drained.
			if err := app.ShutdownWithContext(shutdownCtx); err != nil {
				logger.Error().Err(err).Msg("Error during Fiber server shutdown")
				_ = workerPool.Close()
				return fmt.Errorf("shutdown error: %w", err)
			}

			if unaryServer != nil {
				if err := unaryServer.Shutdown(shutdownCtx); err != nil {
					logger.Error().Err(err).Msg("Error during unary server shutdown")
					_ = workerPool.Close()
					return fmt.Errorf("unary shutdown error: %w", err)
				}
			}

			logger.Info().Msg("HTTP servers stopped, shutting down worker pool")

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

// parsedDynamicCall bundles the post-validation context the dynamic
// call/stream Fiber handlers need: the worker-shaped input bytes, the
// resolved output schema (pointer back into the parsed DynamicInput so
// the schema is not deep-copied), and the resolved preserve_schema_order
// boolean. Streaming handlers carry the same struct so final-frame
// reorder can run with the same context the unary path uses.
type parsedDynamicCall struct {
	WorkerInput         []byte
	OutputSchema        *bamlutils.DynamicOutputSchema
	PreserveSchemaOrder bool
}

// parsedDynamicParse mirrors parsedDynamicCall for the /parse/_dynamic
// endpoint.
type parsedDynamicParse struct {
	WorkerInput         []byte
	OutputSchema        *bamlutils.DynamicOutputSchema
	PreserveSchemaOrder bool
}

// parseDynamicCallBody parses + validates the JSON body for any
// dynamic call/stream endpoint (/call/_dynamic, /call-with-raw/_dynamic,
// /stream/_dynamic, /stream-with-raw/_dynamic) and returns the
// post-validation context alongside the apierror.Code that classifies
// any failure (so callers preserve machine-readable codes without
// collapsing every 4xx into invalid_request). On success: code is ""
// and statusCode is 0.
//
// preserveSchemaOrderDefault is the server-level default for the
// dynamic preserve_schema_order opt-in. It is applied only when the
// request omits / nulls the field; per-request true/false wins.
func parseDynamicCallBody(rawBody []byte, preserveSchemaOrderDefault bool) (parsed *parsedDynamicCall, statusCode int, code apierror.Code, err error) {
	var input bamlutils.DynamicInput
	if err := sonic.Unmarshal(rawBody, &input); err != nil {
		return nil, fiber.StatusBadRequest, apierror.CodeInvalidJSON, fmt.Errorf("invalid JSON: %w", err)
	}

	applyPreserveSchemaOrderDefault(&input, preserveSchemaOrderDefault)

	if err := input.Validate(); err != nil {
		return nil, fiber.StatusBadRequest, apierror.CodeInvalidRequest, err
	}

	workerInput, err := input.ToWorkerInput()
	if err != nil {
		return nil, fiber.StatusInternalServerError, apierror.CodeInternalError, fmt.Errorf("failed to convert input: %w", err)
	}

	return &parsedDynamicCall{
		WorkerInput:         workerInput,
		OutputSchema:        input.OutputSchema,
		PreserveSchemaOrder: input.PreserveSchemaOrder != nil && *input.PreserveSchemaOrder,
	}, 0, "", nil
}

// parseDynamicParseBody mirrors parseDynamicCallBody for the
// /parse/_dynamic endpoint, decoding into the DynamicParseInput shape.
func parseDynamicParseBody(rawBody []byte, preserveSchemaOrderDefault bool) (parsed *parsedDynamicParse, statusCode int, code apierror.Code, err error) {
	var input bamlutils.DynamicParseInput
	if err := sonic.Unmarshal(rawBody, &input); err != nil {
		return nil, fiber.StatusBadRequest, apierror.CodeInvalidJSON, fmt.Errorf("invalid JSON: %w", err)
	}

	applyParsePreserveSchemaOrderDefault(&input, preserveSchemaOrderDefault)

	if err := input.Validate(); err != nil {
		return nil, fiber.StatusBadRequest, apierror.CodeInvalidRequest, err
	}

	workerInput, err := input.ToWorkerInput()
	if err != nil {
		return nil, fiber.StatusInternalServerError, apierror.CodeInternalError, fmt.Errorf("failed to convert input: %w", err)
	}

	return &parsedDynamicParse{
		WorkerInput:         workerInput,
		OutputSchema:        input.OutputSchema,
		PreserveSchemaOrder: input.PreserveSchemaOrder != nil && *input.PreserveSchemaOrder,
	}, 0, "", nil
}

func init() {
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
	serveCmd.Flags().IntVar(&poolSize, "pool-size", 4, "Number of workers in the pool")
	serveCmd.Flags().DurationVar(&firstByteTimeout, "first-byte-timeout", 120*time.Second, "Timeout for first byte from worker (deadlock detection)")
	serveCmd.Flags().DurationVar(&streamKeepaliveInterval, "sse-keepalive-interval", defaultStreamKeepaliveInterval, "Interval between stream keepalive signals for SSE/NDJSON (minimum 100ms)")
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
	Data      stdjson.RawMessage `json:"data"`
	Raw       string          `json:"raw"`
	Reasoning string          `json:"reasoning,omitempty"`
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
