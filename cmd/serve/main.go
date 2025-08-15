package main

import (
	"fmt"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
	"github.com/invakid404/baml-rest"
	"github.com/spf13/cobra"
	"github.com/stoewer/go-strcase"
	"gopkg.in/yaml.v3"
	"log"
	"net/http"
	"reflect"
	"strings"
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

	streamReflect := reflect.ValueOf(baml_rest.Stream)
	for methodIdx := 0; methodIdx < streamReflect.NumMethod(); methodIdx++ {
		methodType := streamReflect.Type().Method(methodIdx)

		firstParam := methodType.Type.In(1)
		if firstParam.String() != "context.Context" {
			continue
		}

		args, ok := baml_rest.StreamMethods[methodType.Name]
		if !ok {
			continue
		}

		var structFields []reflect.StructField

		for paramIdx := 2; paramIdx < methodType.Type.NumIn()-1; paramIdx++ {
			paramName := args[paramIdx-2]
			paramType := methodType.Type.In(paramIdx)

			structField := reflect.StructField{
				Name: strcase.UpperCamelCase(paramName),
				Type: paramType,
				Tag:  reflect.StructTag(fmt.Sprintf(`json:%q`, paramName)),
			}
			structFields = append(structFields, structField)
		}

		inputStruct := reflect.StructOf(structFields)
		inputStructInstance := reflect.New(inputStruct)

		inputSchema, err := generator.NewSchemaRefForValue(inputStructInstance.Interface(), schemas)
		if err != nil {
			panic(err)
		}

		inputSchemaName := fmt.Sprintf("%sInput", methodType.Name)
		schemas[inputSchemaName] = inputSchema

		streamResultType := methodType.Type.Out(0).Elem()
		streamResultTypeInstance := reflect.New(streamResultType)

		finalType := streamResultTypeInstance.MethodByName("Final").Type().Out(0).Elem()

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
		description := fmt.Sprintf("Successful response for %s", methodType.Name)
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

		path := fmt.Sprintf("/call/%s", methodType.Name)
		paths.Set(path, &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: methodType.Name,
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

	finalSchema := openapi3.T{
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

	return &finalSchema
}

var dryRunFormat string

var rootCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the BAML REST API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		schema := generateOpenAPISchema()

		if dryRunFormat != "" {
			// Just generate and print the schema, then exit
			switch dryRunFormat {
			case "json":
				schemaJSON, err := schema.MarshalJSON()
				if err != nil {
					return fmt.Errorf("failed to marshal schema to JSON: %w", err)
				}
				fmt.Println(string(schemaJSON))
			case "yaml":
				yamlData, err := yaml.Marshal(schema)
				if err != nil {
					return fmt.Errorf("failed to marshal schema to YAML: %w", err)
				}
				fmt.Print(string(yamlData))
			default:
				return fmt.Errorf("unsupported format: %s (supported: json, yaml)", dryRunFormat)
			}
			return nil
		}

		// Create Chi router
		r := chi.NewRouter()

		// Add middleware
		r.Use(middleware.Logger)
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

		// Start server
		log.Println("Starting server on :8080...")
		log.Println("OpenAPI schema available at:")
		log.Println("  JSON: http://localhost:8080/openapi.json")
		log.Println("  YAML: http://localhost:8080/openapi.yaml")
		return http.ListenAndServe(":8080", r)
	},
}

func init() {
	rootCmd.Flags().StringVar(&dryRunFormat, "dry-run", "", "Generate OpenAPI schema and exit without starting the server (format: json, yaml; defaults to json)")
	rootCmd.Flags().Lookup("dry-run").NoOptDefVal = "json"
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}
