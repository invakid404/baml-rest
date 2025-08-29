package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest"
	"github.com/stoewer/go-strcase"
)

var (
	rootPkg       = "github.com/invakid404/baml-rest"
	interfacesPkg = fmt.Sprintf("%s/baml", rootPkg)
)

type methodOut struct {
	name             string
	inputStructName  string
	outputStructQual jen.Code
}

func main() {
	out := jen.NewFilePathName(rootPkg, "baml_rest")

	streamReflect := reflect.ValueOf(baml_rest.Stream)
	var methods []methodOut

	for methodIdx := 0; methodIdx < streamReflect.NumMethod(); methodIdx++ {
		methodType := streamReflect.Type().Method(methodIdx)
		methodName := methodType.Name

		firstParam := methodType.Type.In(1)
		if firstParam.String() != "context.Context" {
			continue
		}

		args, ok := baml_rest.StreamMethods[methodType.Name]
		if !ok {
			continue
		}

		// Generate the input struct
		var structFields []jen.Code

		for paramIdx := 2; paramIdx < methodType.Type.NumIn()-1; paramIdx++ {
			paramName := args[paramIdx-2]
			paramType := methodType.Type.In(paramIdx)

			parsedType := parseReflectType(jen.Id(strcase.UpperCamelCase(paramName)), paramType)

			structFields = append(structFields,
				parsedType.statement.
					Tag(map[string]string{
						"json": paramName,
					}))
		}

		inputStructName := strcase.UpperCamelCase(fmt.Sprintf("%sInput", methodName))
		out.Type().Id(inputStructName).Struct(structFields...)

		// Generate the output struct
		outputStructName := strcase.UpperCamelCase(fmt.Sprintf("%sOutput", methodName))

		streamResultType := methodType.Type.Out(0).Elem()

		outputInnerFieldName := "Value"

		parsedStreamResultType := parseReflectType(jen.Id(outputInnerFieldName), streamResultType)
		out.Type().Id(outputStructName).Struct(parsedStreamResultType.statement)

		finalResultType := parsedStreamResultType.generics[1]

		// Implement `StreamResult` interface for the output struct
		selfName := jen.Id("v")
		selfValueName := selfName.Clone().Dot(outputInnerFieldName)

		selfParam := selfName.Clone().Op("*").Id(outputStructName)

		out.Func().
			Params(selfParam.Clone()).
			Id("Kind").Params().
			Qual(interfacesPkg, "StreamResultKind").
			Block(
				jen.If(selfValueName.Clone().Dot("IsError")).
					Block(jen.Return(jen.Qual(interfacesPkg, "StreamResultKindError"))),
				jen.If(selfValueName.Clone().Dot("IsFinal")).
					Block(jen.Return(jen.Qual(interfacesPkg, "StreamResultKindFinal"))),
				jen.Return(jen.Qual(interfacesPkg, "StreamResultKindStream")),
			)

		out.Func().
			Params(selfParam.Clone()).
			Id("Stream").Params().
			Any().
			Block(
				jen.Return(selfValueName.Clone().Dot("Stream").Call()),
			)

		out.Func().
			Params(selfParam.Clone()).
			Id("Final").Params().
			Any().
			Block(
				jen.Return(selfValueName.Clone().Dot("Final").Call()),
			)

		out.Func().
			Params(selfParam.Clone()).
			Id("Error").Params().
			Error().
			Block(
				jen.Return(selfValueName.Clone().Dot("Error")),
			)

		// Generate the method
		var methodBody []jen.Code

		var callParams []jen.Code
		callParams = append(callParams, jen.Id("ctx"))

		for _, arg := range args {
			argName := strcase.UpperCamelCase(arg)
			callParams = append(callParams, jen.Id("input").Dot(argName))
		}

		// Call the streaming method and propagate errors
		methodBody = append(methodBody,
			jen.Id("input").Op(":=").Id("rawInput").Assert(jen.Op("*").Id(inputStructName)),

			jen.List(jen.Id("result"), jen.Id("err")).Op(":=").
				Qual(rootPkg, "Stream").Dot(methodName).Call(callParams...),

			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
		)

		// Initialize the channel
		streamResultInterface := jen.Qual(interfacesPkg, "StreamResult")

		methodBody = append(methodBody,
			jen.Id("out").Op(":=").Make(jen.Chan().Add(streamResultInterface.Clone())),
		)

		// Translate the stream into the channel
		methodBody = append(methodBody,
			jen.Go().Func().Params().
				Block(
					jen.Defer().Close(jen.Id("out")),
					jen.For().Block(
						jen.Select().Block(
							jen.Case(jen.List(jen.Id("entry"), jen.Id("ok")).Op(":=").Op("<-").Id("result")).
								Block(
									jen.If(jen.Op("!").Id("ok")).Block(
										jen.Return(),
									),
									jen.Id("out").Op("<-").Op("&").Id(outputStructName).Values(jen.Dict{
										jen.Id("Value"): jen.Id("entry"),
									}),
								),
							jen.Case(jen.Op("<-").Id("ctx").Dot("Done").Call()).
								Block(jen.Return()),
						),
					),
				).Call(),
		)

		// Return the channel
		methodBody = append(methodBody,
			jen.Return(jen.Id("out"), jen.Nil()),
		)

		out.Func().
			Id(methodName).
			Params(
				jen.Id("ctx").Qual("context", "Context"),
				jen.Id("rawInput").Any(),
			).
			Call(
				jen.List(
					jen.Op("<-").Chan().Add(streamResultInterface.Clone()),
					jen.Error(),
				)).
			Block(methodBody...)

		methods = append(methods, methodOut{
			name:             methodName,
			inputStructName:  inputStructName,
			outputStructQual: finalResultType,
		})
	}

	// Generate the map of methods
	streamingFunctionInterface := jen.Qual(interfacesPkg, "StreamingMethod")

	mapElements := make(jen.Dict)
	for _, method := range methods {
		mapElements[jen.Lit(method.name)] = jen.Values(jen.Dict{
			jen.Id("MakeInput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(jen.Id(method.inputStructName))),
				),
			jen.Id("MakeOutput"): jen.Func().Params().Any().
				Block(
					jen.Return(jen.New(method.outputStructQual)),
				),
			jen.Id("Impl"): jen.Id(method.name),
		})
	}

	out.Var().Id("Methods").Op("=").
		Map(jen.String()).Add(streamingFunctionInterface).
		Values(mapElements)

	if err := out.Save("adapter.go"); err != nil {
		panic(err)
	}
}

type parsedReflectType struct {
	statement *jen.Statement
	generics  []jen.Code
}

func parseReflectType(targetStatement *jen.Statement, typ reflect.Type) parsedReflectType {
	pkgPath := typ.PkgPath()
	typeName := typ.Name()

	genericsStartIdx := strings.Index(typeName, "[")
	genericsEndIdx := strings.LastIndex(typeName, "]")

	var genericsTypes []jen.Code
	if genericsStartIdx != -1 && genericsEndIdx != -1 {
		genericsStr := typeName[genericsStartIdx+1 : genericsEndIdx]
		typeName = typeName[:genericsStartIdx]

		genericsEntries := strings.Split(genericsStr, ",")

		for _, entry := range genericsEntries {
			operator := ""
			pkgName := ""
			ident := entry

			lastDot := strings.LastIndex(entry, ".")

			if lastDot != -1 {
				pkgName = entry[:lastDot]
				ident = entry[lastDot+1:]
			}

			firstChar := strings.IndexFunc(pkgName, unicode.IsLetter)
			if firstChar > 0 {
				operator = pkgName[:firstChar]
				pkgName = pkgName[firstChar:]
			}

			var current *jen.Statement
			if pkgName == "" {
				current = jen.Id(ident)
			} else {
				current = jen.Qual(pkgName, ident)
			}

			if operator != "" {
				current = jen.Op(operator).Add(current)
			}

			genericsTypes = append(genericsTypes, current)
		}
	}

	var statement *jen.Statement
	if pkgPath == "" {
		statement = targetStatement.Id(typeName)
	} else {
		statement = targetStatement.Qual(pkgPath, typeName)
	}

	return parsedReflectType{
		statement.Types(genericsTypes...),
		genericsTypes,
	}
}
