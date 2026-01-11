package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
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
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/render"
	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/gregwebs/go-recovery"
	"github.com/invakid404/baml-rest/internal/unsafeutil"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/tmaxmax/go-sse"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed worker
var workerBinary []byte

//go:embed openapi.json
var openapiJSON []byte

type sseContextKey string

const sseContextKeyTopic = sseContextKey("topic")

var (
	port             int
	poolSize         int
	firstByteTimeout time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "baml-rest",
	Short: "BAML REST API server",
}

// extractWorker extracts the embedded worker binary to a cache location
// Returns the path to the extracted binary
func extractWorker() (string, error) {
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
					slog.Debug("Failed to remove old worker binary", "path", oldPath, "error", err)
				} else {
					slog.Debug("Removed old worker binary", "path", oldPath)
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
		workerPath, err := extractWorker()
		if err != nil {
			return fmt.Errorf("failed to extract worker binary: %w", err)
		}
		slog.Info("Worker binary extracted", "path", workerPath)

		// Initialize worker pool with external worker binary
		poolConfig := pool.DefaultConfig()
		poolConfig.PoolSize = poolSize
		poolConfig.FirstByteTimeout = firstByteTimeout
		poolConfig.Logger = slog.Default()
		poolConfig.WorkerPath = workerPath

		workerPool, err := pool.New(poolConfig)
		if err != nil {
			return fmt.Errorf("failed to create worker pool: %w", err)
		}

		slog.Info("Worker pool initialized", "size", poolSize)

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

				return []string{topic}, true
			},
		}

		// Add middleware
		logger := slog.Default()

		// Set global panic recovery handler to use structured logging
		recovery.ErrorHandler = func(err error) {
			logger.Error("Unhandled panic recovered", "error", err)
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
		r.Use(httplog.RequestLogger(logger, &httplog.Options{
			Level:         slog.LevelInfo,
			RecoverPanics: true,
			LogExtraAttrs: func(req *http.Request, reqBody string, respStatus int) []slog.Attr {
				if reqID := middleware.GetReqID(req.Context()); reqID != "" {
					return []slog.Attr{slog.String("request_id", reqID)}
				}
				return nil
			},
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

			logger.Info("Registering prompt: " + methodName)

			r.Post(fmt.Sprintf("/call/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				result, err := workerPool.Call(ctx, methodName, rawBody, false)
				if err != nil {
					_ = httplog.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(result.Data)
			})

			r.Post(fmt.Sprintf("/call-with-raw/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				result, err := workerPool.Call(ctx, methodName, rawBody, true)
				if err != nil {
					_ = httplog.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				// Use json.RawMessage to embed pre-serialized data without re-parsing
				render.JSON(w, r, CallWithRawResponse{
					Data: result.Data,
					Raw:  result.Raw,
				})
			})

			r.Post(fmt.Sprintf("/stream/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				topicUUID, err := uuid.NewRandom()
				if err != nil {
					_ = httplog.SetError(r.Context(), err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				topic := fmt.Sprintf("stream/%s/%s", methodName, topicUUID.String())

				ctx, cancel := context.WithCancel(
					context.WithValue(r.Context(), sseContextKeyTopic, topic),
				)
				defer cancel()

				r = r.WithContext(ctx)

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				results, err := workerPool.CallStream(ctx, methodName, rawBody, false)
				if err != nil {
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				go recovery.Go(func() error {
					s.ServeHTTP(w, r)
					return nil
				})

				for result := range results {
					message := &sse.Message{}

					switch result.Kind {
					case workerplugin.StreamResultKindError:
						message.Type = sseErrorKind
						if result.Error != nil {
							message.AppendData(result.Error.Error())
						}
					case workerplugin.StreamResultKindStream, workerplugin.StreamResultKindFinal:
						// SAFETY: result.Data is owned by this goroutine, used only for this
						// AppendData call, and never modified afterward. The string is consumed
						// immediately by the SSE library.
						message.AppendData(unsafeutil.BytesToString(result.Data))
					}

					if err := s.Publish(message, topic); err != nil {
						break
					}
				}
			})
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
			logger.Info(fmt.Sprintf("Starting server on :%d...", port))
			logger.Info("OpenAPI schema available at:")
			logger.Info(fmt.Sprintf("  JSON: http://localhost:%d/openapi.json", port))
			logger.Info(fmt.Sprintf("  YAML: http://localhost:%d/openapi.yaml", port))
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
			logger.Info(fmt.Sprintf("Received signal: %v. Initiating graceful shutdown...", sig))

			// Create shutdown context with timeout
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer shutdownCancel()

			// Shutdown HTTP server - this will wait for active connections to finish
			// Active connections are waiting on worker responses, so workers must stay alive
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error(fmt.Sprintf("Error during HTTP server shutdown: %v", err))
				_ = workerPool.Close()
				return fmt.Errorf("shutdown error: %w", err)
			}

			logger.Info("HTTP server stopped, shutting down worker pool...")

			// Gracefully shutdown worker pool - waits for any remaining in-flight requests
			if err := workerPool.Shutdown(shutdownCtx); err != nil {
				logger.Error(fmt.Sprintf("Error during worker pool shutdown: %v", err))
				return fmt.Errorf("worker pool shutdown error: %w", err)
			}

			logger.Info("Server gracefully stopped")
			return nil
		}
	},
}

func init() {
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
	serveCmd.Flags().IntVar(&poolSize, "pool-size", 4, "Number of workers in the pool")
	serveCmd.Flags().DurationVar(&firstByteTimeout, "first-byte-timeout", 120*time.Second, "Timeout for first byte from worker (deadlock detection)")

	rootCmd.AddCommand(serveCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}

// CallWithRawResponse is the response format for the /call-with-raw endpoint
type CallWithRawResponse struct {
	Data json.RawMessage `json:"data"`
	Raw  string          `json:"raw"`
}
