package main

import (
	"context"
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
	"github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
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

var port int

var rootCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the BAML REST API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		schema := generateOpenAPISchema()

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

		for methodName, method := range baml_rest.Methods {
			logger.Info("Registering prompt: " + methodName)

			r.Post(fmt.Sprintf("/call/%s", methodName), func(w http.ResponseWriter, r *http.Request) {
				input, err := readBody(r, method.MakeInput)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				adapter := baml_rest.MakeAdapter(ctx)
				if err := input.options.Apply(adapter); err != nil {
					http.Error(w, fmt.Sprintf("Error applying __baml_options__: %v", err), http.StatusBadRequest)
				}

				result, err := method.Impl(adapter, input.input)

				if err != nil {
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)
					return
				}

				for result := range result {
					kind := result.Kind()
					if kind == bamlutils.StreamResultKindError {
						_ = httplog.SetError(r.Context(), result.Error())
						w.WriteHeader(http.StatusInternalServerError)

						return
					}

					if kind == bamlutils.StreamResultKindFinal {
						render.JSON(w, r, result.Final())
						return
					}
				}
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

				input, err := readBody(r, method.MakeInput)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)

					return
				}

				adapter := baml_rest.MakeAdapter(ctx)
				if err := input.options.Apply(adapter); err != nil {
					http.Error(w, fmt.Sprintf("Error applying __baml_options__: %v", err), http.StatusBadRequest)
				}

				result, err := method.Impl(adapter, input.input)
				if err != nil {
					http.Error(w, fmt.Sprintf("Error calling prompt %s: %v", methodName, err), http.StatusInternalServerError)

					return
				}

				go s.ServeHTTP(w, r)

				for result := range result {
					message := &sse.Message{}

					var value any
					var err error

					switch result.Kind() {
					case bamlutils.StreamResultKindError:
						err = result.Error()
					case bamlutils.StreamResultKindStream:
						value = result.Stream()
					case bamlutils.StreamResultKindFinal:
						value = result.Final()
					}

					var data []byte
					if value != nil {
						data, err = json.Marshal(value)
					}

					if err != nil {
						message.Type = sseErrorKind
						message.AppendData(err.Error())
					} else {
						message.AppendData(string(data))
					}

					err = s.Publish(message, topic)
					if err != nil {
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
		go func() {
			logger.Info(fmt.Sprintf("Starting server on :%d...", port))
			logger.Info("OpenAPI schema available at:")
			logger.Info(fmt.Sprintf("  JSON: http://localhost:%d/openapi.json", port))
			logger.Info(fmt.Sprintf("  YAML: http://localhost:%d/openapi.yaml", port))
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErr <- err
			}
		}()

		// Set up signal handling for graceful shutdown
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		// Wait for interrupt signal or server error
		select {
		case err := <-serverErr:
			return fmt.Errorf("server error: %w", err)
		case sig := <-sigChan:
			logger.Info(fmt.Sprintf("Received signal: %v. Initiating graceful shutdown...", sig))

			// Create shutdown context with timeout
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer shutdownCancel()

			// Shutdown server - this will wait for active connections to finish
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error(fmt.Sprintf("Error during server shutdown: %v", err))
				return fmt.Errorf("shutdown error: %w", err)
			}

			logger.Info("Server gracefully stopped")
			return nil
		}
	},
}

func init() {
	rootCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the server on")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}

type readBodyResult struct {
	input   any
	options *BamlOptionsInput
}

func readBody(r *http.Request, makeInput func() any) (*readBodyResult, error) {
	rawInput, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	var options BamlOptionsInput
	if err := json.Unmarshal(rawInput, &options); err != nil {
		return nil, fmt.Errorf("failed to unmarshal baml options: %w", err)
	}

	input := makeInput()
	if err := json.Unmarshal(rawInput, input); err != nil {
		return nil, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	return &readBodyResult{
		input:   input,
		options: &options,
	}, nil
}

type BamlOptionsInput struct {
	Options *BamlOptions `json:"__baml_options__,omitempty"`
}

func (i *BamlOptionsInput) Apply(adapter bamlutils.Adapter) error {
	if i.Options == nil {
		return nil
	}

	if i.Options.ClientRegistry != nil {
		if err := adapter.SetClientRegistry(i.Options.ClientRegistry); err != nil {
			return fmt.Errorf("failed to set client registry: %w", err)
		}
	}

	if i.Options.TypeBuilder != nil {
		if err := adapter.SetTypeBuilder(i.Options.TypeBuilder); err != nil {
			return fmt.Errorf("failed to set type builder: %w", err)
		}
	}

	return nil
}

type BamlOptions struct {
	ClientRegistry *bamlutils.ClientRegistry `json:"client_registry"`
	TypeBuilder    *bamlutils.TypeBuilder    `json:"type_builder"`
}
