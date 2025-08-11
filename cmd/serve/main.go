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

		schema, err := generator.NewSchemaRefForValue(inputStructInstance.Interface(), schemas)
		if err != nil {
			panic(err)
		}

		schemaName := fmt.Sprintf("%sInput", methodType.Name)
		schemas[schemaName] = schema
	}

	data, _ := json.MarshalIndent(schemas, "", "  ")
	fmt.Println(string(data))
}
