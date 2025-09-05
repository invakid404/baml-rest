package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/goccy/go-json"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/render"
	"github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"

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

var rootCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the BAML REST API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		schema := generateOpenAPISchema()

		// Create Chi router
		r := chi.NewRouter()

		// Add middleware
		logger := slog.Default()
		r.Use(httplog.RequestLogger(logger, &httplog.Options{
			Level:         slog.LevelInfo,
			RecoverPanics: true,
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
				rawInput, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				var options BamlOptionsInput
				if err := json.Unmarshal(rawInput, &options); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
				}

				input := method.MakeInput()
				if err := json.Unmarshal(rawInput, input); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				adapter := baml_rest.MakeAdapter(ctx)
				if options.Options != nil && options.Options.ClientRegistry != nil {
					adapter.SetClientRegistry(options.Options.ClientRegistry)
				}

				result, err := method.Impl(adapter, input)

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
		}

		// Start server
		logger.Info("Starting server on :8080...")
		logger.Info("OpenAPI schema available at:")
		logger.Info("  JSON: http://localhost:8080/openapi.json")
		logger.Info("  YAML: http://localhost:8080/openapi.yaml")
		return http.ListenAndServe(":8080", r)
	},
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}

type BamlOptionsInput struct {
	Options *BamlOptions `json:"__baml_options__,omitempty"`
}

type BamlOptions struct {
	ClientRegistry *bamlutils.ClientRegistry `json:"client_registry"`
}
