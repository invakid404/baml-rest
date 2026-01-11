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
	"strings"
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
	"github.com/goccy/go-json"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/internal/httplogger"
	"github.com/invakid404/baml-rest/internal/unsafeutil"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/rs/zerolog"
	"github.com/tmaxmax/go-sse"

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

		r.Use(middleware.RequestID)
		// Custom middleware to set request ID as response header
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
		r.Use(middleware.Recoverer)

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

			logger.Info().Str("prompt", methodName).Msg("Registering prompt")

			makeCallHandler := func(enableRawCollection bool) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()

					rawBody, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
						return
					}

					result, err := workerPool.Call(ctx, methodName, rawBody, enableRawCollection)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
						return
					}

					w.Header().Set("Content-Type", "application/json")
					if enableRawCollection {
						render.JSON(w, r, CallWithRawResponse{
							Data: result.Data,
							Raw:  result.Raw,
						})
					} else {
						_, _ = w.Write(result.Data)
					}
				}
			}

			r.Post(fmt.Sprintf("/call/%s", methodName), makeCallHandler(false))
			r.Post(fmt.Sprintf("/call-with-raw/%s", methodName), makeCallHandler(true))

			makeStreamHandler := func(pathPrefix string, enableRawCollection bool) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					topic := fmt.Sprintf("%s/%s/%p", pathPrefix, methodName, r)
					ready := make(chan struct{})

					ctx, cancel := context.WithCancel(
						context.WithValue(
							context.WithValue(r.Context(), sseContextKeyTopic, topic),
							sseContextKeyReady, ready,
						),
					)
					defer cancel()

					r = r.WithContext(ctx)

					rawBody, err := io.ReadAll(r.Body)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
						return
					}

					results, err := workerPool.CallStream(ctx, methodName, rawBody, enableRawCollection)
					if err != nil {
						httplogger.SetError(r.Context(), err)
						http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
						return
					}

					sseDone := make(chan struct{})
					go recovery.Go(func() error {
						defer close(sseDone)
						s.ServeHTTP(w, r)
						return nil
					})

					// Wait for SSE connection to be established before publishing
					select {
					case <-ready:
						// SSE is ready, proceed
					case <-ctx.Done():
						// Context cancelled (client disconnected)
						<-sseDone
						return
					}

					for result := range results {
						message := &sse.Message{}

						switch result.Kind {
						case workerplugin.StreamResultKindError:
							message.Type = sseErrorKind
							if result.Error != nil {
								message.AppendData(result.Error.Error())
							}
						case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
							data := result.Data
							if enableRawCollection {
								var err error
								data, err = json.Marshal(CallWithRawResponse{
									Data: result.Data,
									Raw:  result.Raw,
								})
								if err != nil {
									message.Type = sseErrorKind
									message.AppendData(fmt.Sprintf("Failed to marshal response: %v", err))
									_ = s.Publish(message, topic)
									continue
								}
							}
							// SAFETY: data is owned by this goroutine, used only for this
							// AppendData call, and never modified afterward. The string is consumed
							// immediately by the SSE library.
							message.AppendData(unsafeutil.BytesToString(data))
						}

						if err := s.Publish(message, topic); err != nil {
							break
						}
					}

					// Cancel context to signal SSE server to stop, then wait for it
					cancel()
					<-sseDone
				}
			}

			r.Post(fmt.Sprintf("/stream/%s", methodName), makeStreamHandler("stream", false))
			r.Post(fmt.Sprintf("/stream-with-raw/%s", methodName), makeStreamHandler("stream-with-raw", true))
		}

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
