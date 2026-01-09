package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/render"
	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/gregwebs/go-recovery"
	goplugin "github.com/hashicorp/go-plugin"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/pool"
	"github.com/invakid404/baml-rest/workerplugin"
	"github.com/tmaxmax/go-sse"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func generateOpenAPISchema() *openapi3.T {
	schemas := make(openapi3.Schemas)

	var generator *openapi3gen.Generator

	isUnion := func(t reflect.Type) bool {
		return strings.HasPrefix(t.Name(), "Union")
	}

	handleUnion := func(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
		for idx := 0; idx < t.NumField(); idx++ {
			field := t.Field(idx)
			if field.Type.Kind() != reflect.Ptr {
				continue
			}

			fakeStruct := reflect.StructOf([]reflect.StructField{
				{
					Name: "X",
					Type: field.Type.Elem(),
					Tag:  reflect.StructTag(fmt.Sprintf(`json:"x"`)),
				},
			})

			fieldInstance := reflect.New(fakeStruct)
			fieldSchema, err := generator.NewSchemaRefForValue(fieldInstance.Interface(), schemas)
			if err != nil {
				return fmt.Errorf("failed to generate schema for field %q in type %q: %w", field.Name, t.Name(), err)
			}

			schema.OneOf = append(schema.OneOf, fieldSchema.Value.Properties["x"])
		}

		return nil
	}

	isEnum := func(t reflect.Type) bool {
		if t.Kind() != reflect.String || t.Name() == "string" {
			return false
		}

		_, ok := t.MethodByName("Values")

		return ok
	}

	handleEnum := func(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
		valuesMethod, ok := t.MethodByName("Values")
		if !ok {
			panic("enum type must have a Values method")
		}

		enumInstance := reflect.New(t).Elem()

		values := valuesMethod.Func.Call([]reflect.Value{enumInstance})[0]
		if values.Kind() != reflect.Slice {
			return fmt.Errorf("values method must return a slice")
		}

		length := values.Len()
		result := make([]any, length)
		for i := 0; i < length; i++ {
			result[i] = values.Index(i).String()
		}

		var schemaType openapi3.Types
		schemaType = append(schemaType, openapi3.TypeString)
		schema.Type = &schemaType

		schema.Enum = result

		return nil
	}

	generator = openapi3gen.NewGenerator(
		openapi3gen.UseAllExportedFields(),
		openapi3gen.CreateComponentSchemas(openapi3gen.ExportComponentSchemasOptions{
			ExportComponentSchemas: true,
		}),
		openapi3gen.SchemaCustomizer(func(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
			if isUnion(t) {
				if err := handleUnion(name, t, tag, schema); err != nil {
					return err
				}
			} else if isEnum(t) {
				if err := handleEnum(name, t, tag, schema); err != nil {
					return err
				}
			} else {
				// Handle required vs optional fields based on pointer types
				if t.Kind() == reflect.Struct {
					var requiredFields []string
					for i := 0; i < t.NumField(); i++ {
						field := t.Field(i)
						// Skip unexported fields
						if !field.IsExported() {
							continue
						}

						// Get the JSON tag name, default to field name if no tag
						jsonTag := field.Tag.Get("json")
						fieldName := field.Name
						if jsonTag != "" && jsonTag != "-" {
							// Handle "name,omitempty" format
							if parts := strings.Split(jsonTag, ","); len(parts) > 0 && parts[0] != "" {
								fieldName = parts[0]
							}
						}

						// If field is not a pointer, it's required
						if field.Type.Kind() != reflect.Ptr {
							requiredFields = append(requiredFields, fieldName)
						}
					}

					if len(requiredFields) > 0 {
						schema.Required = requiredFields
					}
				}

				return nil
			}

			schemas[t.Name()] = &openapi3.SchemaRef{
				Value: schema,
			}

			return nil
		}),
	)

	paths := openapi3.NewPaths()

	bamlOptionsSchemaName := "BamlOptions"
	bamlOptionsSchema, err := generator.NewSchemaRefForValue(BamlOptions{}, schemas)
	if err != nil {
		panic(err)
	}
	schemas[bamlOptionsSchemaName] = bamlOptionsSchema

	for methodName, method := range baml_rest.Methods {
		inputStruct := method.MakeInput()
		inputStructInstance := reflect.ValueOf(inputStruct)

		inputSchema, err := generator.NewSchemaRefForValue(inputStructInstance.Interface(), schemas)
		if err != nil {
			panic(err)
		}

		if inputSchema.Value.Properties == nil {
			inputSchema.Value.Properties = make(openapi3.Schemas)
		}

		inputSchema.Value.Properties["__baml_options__"] = &openapi3.SchemaRef{
			Ref: fmt.Sprintf("#/components/schemas/%s", bamlOptionsSchemaName),
		}

		inputSchemaName := inputStructInstance.Elem().Type().Name()
		schemas[inputSchemaName] = inputSchema

		finalType := reflect.ValueOf(method.MakeOutput()).Elem().Type()

		resultTypeStruct := reflect.StructOf([]reflect.StructField{
			{
				Name: "X",
				Type: finalType,
				Tag:  reflect.StructTag(fmt.Sprintf(`json:"x"`)),
			},
		})

		resultTypeStructInstance := reflect.New(resultTypeStruct)
		resultTypeSchema, err := generator.NewSchemaRefForValue(resultTypeStructInstance.Interface(), schemas)
		if err != nil {
			panic(err)
		}

		// Response for /call endpoint
		responses := openapi3.NewResponses()
		description := fmt.Sprintf("Successful response for %s", methodName)
		responses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{
				Description: &description,
				Content: openapi3.Content{
					"application/json": &openapi3.MediaType{
						Schema: resultTypeSchema.Value.Properties["x"],
					},
				},
			},
		})

		path := fmt.Sprintf("/call/%s", methodName)
		paths.Set(path, &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: methodName,
				RequestBody: &openapi3.RequestBodyRef{
					Value: &openapi3.RequestBody{
						Content: map[string]*openapi3.MediaType{
							"application/json": {
								Schema: &openapi3.SchemaRef{
									Ref: fmt.Sprintf("#/components/schemas/%s", inputSchemaName),
								},
							},
						},
					},
				},
				Responses: responses,
			},
		})

		// Response for /call-with-raw endpoint
		rawResponsesDescription := fmt.Sprintf("Successful response for %s with raw LLM output", methodName)
		rawResponses := openapi3.NewResponses()
		rawResponses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{
				Description: &rawResponsesDescription,
				Content: openapi3.Content{
					"application/json": &openapi3.MediaType{
						Schema: &openapi3.SchemaRef{
							Value: &openapi3.Schema{
								Type: &openapi3.Types{openapi3.TypeObject},
								Properties: openapi3.Schemas{
									"data": resultTypeSchema.Value.Properties["x"],
									"raw": &openapi3.SchemaRef{
										Value: &openapi3.Schema{
											Type:        &openapi3.Types{openapi3.TypeString},
											Description: "Raw LLM response text",
										},
									},
								},
								Required: []string{"data", "raw"},
							},
						},
					},
				},
			},
		})

		rawPath := fmt.Sprintf("/call-with-raw/%s", methodName)
		paths.Set(rawPath, &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: fmt.Sprintf("%sWithRaw", methodName),
				RequestBody: &openapi3.RequestBodyRef{
					Value: &openapi3.RequestBody{
						Content: map[string]*openapi3.MediaType{
							"application/json": {
								Schema: &openapi3.SchemaRef{
									Ref: fmt.Sprintf("#/components/schemas/%s", inputSchemaName),
								},
							},
						},
					},
				},
				Responses: rawResponses,
			},
		})
	}

	return &openapi3.T{
		OpenAPI: "3.0.0",
		Info: &openapi3.Info{
			Title:   "baml-rest",
			Version: "1.0.0",
		},
		Components: &openapi3.Components{
			Schemas: schemas,
		},
		Paths: paths,
	}
}

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

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the BAML REST API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		schema := generateOpenAPISchema()

		// Initialize worker pool - spawns self with "worker" subcommand
		poolConfig := pool.DefaultConfig()
		poolConfig.PoolSize = poolSize
		poolConfig.FirstByteTimeout = firstByteTimeout
		poolConfig.Logger = slog.Default()

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
			render.JSON(w, r, schema)
		})

		// Add /openapi.yaml endpoint
		r.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			yamlData, err := yaml.Marshal(schema)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(yamlData)
		})

		for methodName := range baml_rest.Methods {
			methodName := methodName // capture for closure

			logger.Info("Registering prompt: " + methodName)

			r.Post(fmt.Sprintf("/call/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				result, err := workerPool.Call(ctx, methodName, rawBody, rawBody)
				if err != nil {
					_ = httplog.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(result.Data)
			})

			r.Post(fmt.Sprintf("/call-with-raw/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				rawBody, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
					return
				}

				result, err := workerPool.Call(ctx, methodName, rawBody, rawBody)
				if err != nil {
					_ = httplog.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				// Unmarshal data and create response with raw
				var data any
				if err := json.Unmarshal(result.Data, &data); err != nil {
					_ = httplog.SetError(r.Context(), err)
					http.Error(w, fmt.Sprintf("Error unmarshaling result: %v", err), http.StatusInternalServerError)
					return
				}

				render.JSON(w, r, CallWithRawResponse{
					Data: data,
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

				results, err := workerPool.CallStream(ctx, methodName, rawBody, rawBody)
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
						message.AppendData(string(result.Data))
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

var workerCmd = &cobra.Command{
	Use:    "worker",
	Short:  "Run as a worker process (internal use)",
	Hidden: true,
	Run: func(cmd *cobra.Command, args []string) {
		runWorker()
	},
}

func init() {
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
	serveCmd.Flags().IntVar(&poolSize, "pool-size", 4, "Number of workers in the pool")
	serveCmd.Flags().DurationVar(&firstByteTimeout, "first-byte-timeout", 120*time.Second, "Timeout for first byte from worker (deadlock detection)")

	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(workerCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}

// CallWithRawResponse is the response format for the /call-with-raw endpoint
type CallWithRawResponse struct {
	Data any    `json:"data"`
	Raw  string `json:"raw"`
}

// BamlOptions is used for OpenAPI schema generation of the __baml_options__ field
type BamlOptions struct {
	ClientRegistry *bamlutils.ClientRegistry `json:"client_registry"`
	TypeBuilder    *bamlutils.TypeBuilder    `json:"type_builder"`
}

// Worker mode implementation

func runWorker() {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins: map[string]goplugin.Plugin{
			"worker": &workerplugin.WorkerPlugin{Impl: &workerImpl{}},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}

// workerImpl implements the workerplugin.Worker interface
type workerImpl struct{}

// workerBamlOptions mirrors the options structure for worker parsing
type workerBamlOptions struct {
	Options *BamlOptions `json:"__baml_options__,omitempty"`
}

func (o *workerBamlOptions) apply(adapter bamlutils.Adapter) error {
	if o.Options == nil {
		return nil
	}

	if o.Options.ClientRegistry != nil {
		if err := adapter.SetClientRegistry(o.Options.ClientRegistry); err != nil {
			return fmt.Errorf("failed to set client registry: %w", err)
		}
	}

	if o.Options.TypeBuilder != nil {
		if err := adapter.SetTypeBuilder(o.Options.TypeBuilder); err != nil {
			return fmt.Errorf("failed to set type builder: %w", err)
		}
	}

	return nil
}

func (w *workerImpl) Call(ctx context.Context, methodName string, inputJSON, optionsJSON []byte) (*workerplugin.CallResult, error) {
	method, ok := baml_rest.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input
	input := method.MakeInput()
	if err := json.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options
	var options workerBamlOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, fmt.Errorf("failed to unmarshal options: %w", err)
		}
	}

	// Create adapter and apply options
	adapter := baml_rest.MakeAdapter(ctx)
	if err := options.apply(adapter); err != nil {
		return nil, fmt.Errorf("failed to apply options: %w", err)
	}

	// Execute the method
	resultChan, err := method.Impl(adapter, input)
	if err != nil {
		return nil, fmt.Errorf("failed to call method: %w", err)
	}

	// Wait for final result
	for result := range resultChan {
		kind := result.Kind()
		if kind == bamlutils.StreamResultKindError {
			return nil, result.Error()
		}

		if kind == bamlutils.StreamResultKindFinal {
			data, err := json.Marshal(result.Final())
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %w", err)
			}
			return &workerplugin.CallResult{
				Data: data,
				Raw:  result.Raw(),
			}, nil
		}
	}

	return nil, fmt.Errorf("no final result received")
}

func (w *workerImpl) CallStream(ctx context.Context, methodName string, inputJSON, optionsJSON []byte) (<-chan *workerplugin.StreamResult, error) {
	method, ok := baml_rest.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %q not found", methodName)
	}

	// Parse input
	input := method.MakeInput()
	if err := json.Unmarshal(inputJSON, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Parse options
	var options workerBamlOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, fmt.Errorf("failed to unmarshal options: %w", err)
		}
	}

	// Create adapter and apply options
	adapter := baml_rest.MakeAdapter(ctx)
	if err := options.apply(adapter); err != nil {
		return nil, fmt.Errorf("failed to apply options: %w", err)
	}

	// Execute the method
	resultChan, err := method.Impl(adapter, input)
	if err != nil {
		return nil, fmt.Errorf("failed to call method: %w", err)
	}

	// Convert to plugin stream results
	out := make(chan *workerplugin.StreamResult)
	go func() {
		defer close(out)
		for result := range resultChan {
			var pluginResult *workerplugin.StreamResult

			switch result.Kind() {
			case bamlutils.StreamResultKindError:
				pluginResult = &workerplugin.StreamResult{
					Kind:  workerplugin.StreamResultKindError,
					Error: result.Error(),
				}
			case bamlutils.StreamResultKindStream:
				data, err := json.Marshal(result.Stream())
				if err != nil {
					pluginResult = &workerplugin.StreamResult{
						Kind:  workerplugin.StreamResultKindError,
						Error: fmt.Errorf("failed to marshal stream result: %w", err),
					}
				} else {
					pluginResult = &workerplugin.StreamResult{
						Kind: workerplugin.StreamResultKindStream,
						Data: data,
						Raw:  result.Raw(),
					}
				}
			case bamlutils.StreamResultKindFinal:
				data, err := json.Marshal(result.Final())
				if err != nil {
					pluginResult = &workerplugin.StreamResult{
						Kind:  workerplugin.StreamResultKindError,
						Error: fmt.Errorf("failed to marshal final result: %w", err),
					}
				} else {
					pluginResult = &workerplugin.StreamResult{
						Kind: workerplugin.StreamResultKindFinal,
						Data: data,
						Raw:  result.Raw(),
					}
				}
			}

			select {
			case out <- pluginResult:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func (w *workerImpl) Health(ctx context.Context) (bool, error) {
	return true, nil
}
