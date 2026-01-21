package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
)

func main() {
	// Initialize BAML runtime for type introspection
	baml_rest.InitBamlRuntime()

	schema := generateOpenAPISchema()

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling schema: %v\n", err)
		os.Exit(1)
	}

	// Output to stdout or file
	if len(os.Args) > 1 {
		if err := os.WriteFile(os.Args[1], data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing schema file: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Schema written to %s\n", os.Args[1])
	} else {
		fmt.Println(string(data))
	}
}

// streamSchemaSuffix is appended to component schema names for their nullable stream variants.
const streamSchemaSuffix = "__Stream"

// makeStreamEventSchema creates a streaming event schema with the given type and data schema.
// If includeRaw is true, adds a "raw" field for LLM output.
func makeStreamEventSchema(eventType, description string, dataSchema *openapi3.SchemaRef, includeRaw bool, rawDescription string) *openapi3.SchemaRef {
	props := openapi3.Schemas{
		"type": &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type: &openapi3.Types{openapi3.TypeString},
				Enum: []any{eventType},
			},
		},
		"data": dataSchema,
	}
	required := []string{"type", "data"}

	if includeRaw {
		props["raw"] = &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: rawDescription,
			},
		}
		required = append(required, "raw")
	}

	return &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type:        &openapi3.Types{openapi3.TypeObject},
			Description: description,
			Properties:  props,
			Required:    required,
		},
	}
}

// makeStreamSchemaFullyNullable recursively processes a schema to make all fields nullable.
// This is necessary for streaming partial events where any field may be null during parsing.
//
// For $ref schemas, instead of inlining, this creates a new component schema named X__Stream
// and returns a $ref to it. This keeps the schema smaller and allows reuse.
func makeStreamSchemaFullyNullable(schemaRef *openapi3.SchemaRef, schemas openapi3.Schemas, inProgress map[string]bool) *openapi3.SchemaRef {
	if schemaRef == nil {
		return nil
	}

	// Handle $ref schemas - create X__Stream component schemas
	if schemaRef.Ref != "" {
		refName := strings.TrimPrefix(schemaRef.Ref, "#/components/schemas/")
		streamName := refName + streamSchemaSuffix

		// If stream schema already exists, just reference it
		if _, exists := schemas[streamName]; exists {
			return &openapi3.SchemaRef{
				Ref: fmt.Sprintf("#/components/schemas/%s", streamName),
			}
		}

		// If we're currently creating this schema (cycle detection), return a $ref
		// The schema will be complete by the time it's needed
		if inProgress[refName] {
			return &openapi3.SchemaRef{
				Ref: fmt.Sprintf("#/components/schemas/%s", streamName),
			}
		}

		// Look up the original schema
		refSchema, ok := schemas[refName]
		if !ok || refSchema.Value == nil {
			// Can't resolve - just wrap with nullable allOf
			return &openapi3.SchemaRef{
				Value: &openapi3.Schema{
					Nullable: true,
					AllOf: openapi3.SchemaRefs{
						{Ref: schemaRef.Ref},
					},
				},
			}
		}

		// Mark as in-progress before recursing
		inProgress[refName] = true

		// Create the stream version of the schema
		streamSchema := makeStreamSchemaFullyNullable(refSchema, schemas, inProgress)

		// Register it as a component schema
		schemas[streamName] = streamSchema

		// Return a $ref to the new stream schema
		return &openapi3.SchemaRef{
			Ref: fmt.Sprintf("#/components/schemas/%s", streamName),
		}
	}

	if schemaRef.Value == nil {
		return schemaRef
	}

	schema := schemaRef.Value

	// Create a new schema to avoid mutating the original
	newSchema := &openapi3.Schema{
		Type:        schema.Type,
		Description: schema.Description,
		Enum:        schema.Enum,
		Default:     schema.Default,
		Nullable:    true, // All fields nullable in stream types
		// Don't copy Required - stream types have no required fields
	}

	// Handle different schema types
	if schema.Type != nil && len(*schema.Type) > 0 {
		schemaType := (*schema.Type)[0]

		switch schemaType {
		case openapi3.TypeArray:
			// In stream types, arrays can be null (not yet parsed)
			// Items should also be nullable
			if schema.Items != nil {
				newSchema.Items = makeStreamSchemaFullyNullable(schema.Items, schemas, inProgress)
			}
			return &openapi3.SchemaRef{Value: newSchema}

		case openapi3.TypeObject:
			// Make all properties nullable and process them recursively
			if schema.Properties != nil {
				newSchema.Properties = make(openapi3.Schemas)
				for propName, propSchema := range schema.Properties {
					newSchema.Properties[propName] = makeStreamSchemaFullyNullable(propSchema, schemas, inProgress)
				}
			}
			return &openapi3.SchemaRef{Value: newSchema}

		default:
			// Primitives (string, number, integer, boolean) - already set nullable above
			return &openapi3.SchemaRef{Value: newSchema}
		}
	}

	// Handle OneOf (union types)
	if len(schema.OneOf) > 0 {
		newSchema.OneOf = make(openapi3.SchemaRefs, len(schema.OneOf))
		for i, oneOf := range schema.OneOf {
			newSchema.OneOf[i] = makeStreamSchemaFullyNullable(oneOf, schemas, inProgress)
		}
		return &openapi3.SchemaRef{Value: newSchema}
	}

	// Handle AllOf
	if len(schema.AllOf) > 0 {
		newSchema.AllOf = make(openapi3.SchemaRefs, len(schema.AllOf))
		for i, allOf := range schema.AllOf {
			newSchema.AllOf[i] = makeStreamSchemaFullyNullable(allOf, schemas, inProgress)
		}
		return &openapi3.SchemaRef{Value: newSchema}
	}

	return &openapi3.SchemaRef{Value: newSchema}
}

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
						} else {
							// Pointer fields are nullable - mark them in the schema
							if propSchema, ok := schema.Properties[fieldName]; ok {
								if propSchema.Ref != "" && strings.HasPrefix(propSchema.Ref, "#/") {
									// For proper $ref schemas (like #/components/schemas/Foo),
									// wrap with allOf to add nullable without modifying the referenced schema
									schema.Properties[fieldName] = &openapi3.SchemaRef{
										Value: &openapi3.Schema{
											Nullable: true,
											AllOf: openapi3.SchemaRefs{
												{Ref: propSchema.Ref},
											},
										},
									}
								} else if propSchema.Value != nil {
									// For inline schemas, directly set nullable
									propSchema.Value.Nullable = true
								}
							}
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
	bamlOptionsSchema, err := generator.NewSchemaRefForValue(bamlutils.BamlOptions{}, schemas)
	if err != nil {
		panic(err)
	}
	schemas[bamlOptionsSchemaName] = bamlOptionsSchema

	// Global streaming event schemas (shared across all methods)
	// Use double underscore prefix/suffix to avoid collision with user-defined BAML types
	streamResetEventSchemaName := "__StreamResetEvent__"
	schemas[streamResetEventSchemaName] = &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type:        &openapi3.Types{openapi3.TypeObject},
			Description: "Reset event indicating client should discard accumulated state (sent when a retry occurs)",
			Properties: openapi3.Schemas{
				"type": &openapi3.SchemaRef{
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeString},
						Enum: []any{"reset"},
					},
				},
			},
			Required: []string{"type"},
		},
	}

	streamErrorEventSchemaName := "__StreamErrorEvent__"
	schemas[streamErrorEventSchemaName] = &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type:        &openapi3.Types{openapi3.TypeObject},
			Description: "Error event indicating the stream has failed",
			Properties: openapi3.Schemas{
				"type": &openapi3.SchemaRef{
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeString},
						Enum: []any{"error"},
					},
				},
				"error": &openapi3.SchemaRef{
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Error message describing what went wrong",
					},
				},
			},
			Required: []string{"type", "error"},
		},
	}

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

		// Generate schema for stream/partial type (may differ from final type)
		streamType := reflect.ValueOf(method.MakeStreamOutput()).Elem().Type()

		streamTypeStruct := reflect.StructOf([]reflect.StructField{
			{
				Name: "X",
				Type: streamType,
				Tag:  reflect.StructTag(fmt.Sprintf(`json:"x"`)),
			},
		})

		streamTypeStructInstance := reflect.New(streamTypeStruct)
		streamTypeSchema, err := generator.NewSchemaRefForValue(streamTypeStructInstance.Interface(), schemas)
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

		// References to global event schemas
		resetEventSchemaRef := &openapi3.SchemaRef{
			Ref: fmt.Sprintf("#/components/schemas/%s", streamResetEventSchemaName),
		}
		errorEventSchemaRef := &openapi3.SchemaRef{
			Ref: fmt.Sprintf("#/components/schemas/%s", streamErrorEventSchemaName),
		}

		// Response for /stream endpoint (NDJSON streaming without raw)
		// Partial data events contain intermediate results (may have null placeholders for unparsed fields)
		// Make all fields in the stream type nullable since any field may be null during streaming
		nullableStreamDataSchema := makeStreamSchemaFullyNullable(streamTypeSchema.Value.Properties["x"], schemas, make(map[string]bool))
		finalDataSchema := resultTypeSchema.Value.Properties["x"]

		streamPartialDataEventSchema := makeStreamEventSchema(
			"data",
			"Partial data event containing an intermediate parsed result. Fields not yet parsed may be null.",
			nullableStreamDataSchema, false, "",
		)
		streamFinalDataEventSchema := makeStreamEventSchema(
			"final",
			"Final data event containing the complete, validated result",
			finalDataSchema, false, "",
		)

		streamDescription := fmt.Sprintf("Stream of partial and final results for %s", methodName)
		sseStreamDescription := "Server-Sent Events stream. Default format if Accept header is not set. Data events contain JSON, error/reset events use SSE event types."
		streamResponses := openapi3.NewResponses()
		streamResponses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{
				Description: &streamDescription,
				Content: openapi3.Content{
					"application/x-ndjson": &openapi3.MediaType{
						Schema: &openapi3.SchemaRef{
							Value: &openapi3.Schema{
								OneOf: openapi3.SchemaRefs{
									streamPartialDataEventSchema,
									streamFinalDataEventSchema,
									resetEventSchemaRef,
									errorEventSchemaRef,
								},
								Discriminator: &openapi3.Discriminator{
									PropertyName: "type",
								},
							},
						},
					},
					"text/event-stream": &openapi3.MediaType{
						Schema: &openapi3.SchemaRef{
							Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Description: sseStreamDescription,
							},
						},
					},
				},
			},
		})

		streamPath := fmt.Sprintf("/stream/%s", methodName)
		paths.Set(streamPath, &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: fmt.Sprintf("%sStream", methodName),
				Summary:     fmt.Sprintf("Stream %s results", methodName),
				Description: "Returns a stream of events containing partial results as they become available, followed by the final result. " +
					"Use `Accept: application/x-ndjson` header for typed NDJSON responses (recommended for generated clients). " +
					"Without an Accept header, returns Server-Sent Events (text/event-stream) by default. " +
					"Events have type 'data' for partial results (fields may be null), 'final' for the complete validated result, " +
					"'reset' if the stream restarts due to a retry, or 'error' for failures.",
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
				Responses: streamResponses,
			},
		})

		// Response for /stream-with-raw endpoint (NDJSON streaming with raw LLM output)
		// Reuse the same nullable stream data schema from above
		streamWithRawPartialDataEventSchema := makeStreamEventSchema(
			"data",
			"Partial data event containing an intermediate parsed result with accumulated raw LLM output. Fields not yet parsed may be null.",
			nullableStreamDataSchema, true, "Accumulated raw LLM response text up to this point",
		)
		streamWithRawFinalDataEventSchema := makeStreamEventSchema(
			"final",
			"Final data event containing the complete, validated result with full raw LLM output",
			finalDataSchema, true, "Complete raw LLM response text",
		)

		streamWithRawDescription := fmt.Sprintf("Stream of partial and final results for %s with raw LLM output", methodName)
		sseStreamWithRawDescription := "Server-Sent Events stream. Default format if Accept header is not set. Data events contain JSON with 'data' and 'raw' fields, error/reset events use SSE event types."
		streamWithRawResponses := openapi3.NewResponses()
		streamWithRawResponses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{
				Description: &streamWithRawDescription,
				Content: openapi3.Content{
					"application/x-ndjson": &openapi3.MediaType{
						Schema: &openapi3.SchemaRef{
							Value: &openapi3.Schema{
								OneOf: openapi3.SchemaRefs{
									streamWithRawPartialDataEventSchema,
									streamWithRawFinalDataEventSchema,
									resetEventSchemaRef,
									errorEventSchemaRef,
								},
								Discriminator: &openapi3.Discriminator{
									PropertyName: "type",
								},
							},
						},
					},
					"text/event-stream": &openapi3.MediaType{
						Schema: &openapi3.SchemaRef{
							Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Description: sseStreamWithRawDescription,
							},
						},
					},
				},
			},
		})

		streamWithRawPath := fmt.Sprintf("/stream-with-raw/%s", methodName)
		paths.Set(streamWithRawPath, &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: fmt.Sprintf("%sStreamWithRaw", methodName),
				Summary:     fmt.Sprintf("Stream %s results with raw LLM output", methodName),
				Description: "Returns a stream of events containing partial results and the accumulated raw LLM output as they become available. " +
					"Use `Accept: application/x-ndjson` header for typed NDJSON responses (recommended for generated clients). " +
					"Without an Accept header, returns Server-Sent Events (text/event-stream) by default. " +
					"Events have type 'data' for partial results (fields may be null, includes 'raw' field), 'final' for the complete validated result, " +
					"'reset' if the stream restarts due to a retry, or 'error' for failures.",
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
				Responses: streamWithRawResponses,
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
