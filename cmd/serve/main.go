package main

import (
	"encoding/json"
	"fmt"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/invakid404/baml-rest"
	"github.com/stoewer/go-strcase"
	"reflect"
	"strings"
)

func main() {
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

		resultSchemaName := fmt.Sprintf("%sResult", methodType.Name)
		schemas[resultSchemaName] = resultTypeSchema.Value.Properties["x"]

		responses := openapi3.NewResponses()
		responses.Set("200", &openapi3.ResponseRef{
			Ref: fmt.Sprintf("#/components/schemas/%s", resultSchemaName),
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

	data, _ := json.MarshalIndent(finalSchema, "", "  ")
	fmt.Println(string(data))
}
