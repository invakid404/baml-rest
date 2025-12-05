package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/dave/jennifer/jen"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
	"github.com/stoewer/go-strcase"
)

type methodOut struct {
	name             string
	inputStructName  string
	outputStructQual jen.Code
}

const (
	selfPkg        = "github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
	selfAdapterPkg = selfPkg + "/adapter"
)

func main() {
	out := common.MakeFile()

	streamReflect := reflect.ValueOf(introspected.Stream)
	var methods []methodOut

	for methodIdx := 0; methodIdx < streamReflect.NumMethod(); methodIdx++ {
		methodType := streamReflect.Type().Method(methodIdx)
		methodName := methodType.Name

		firstParam := methodType.Type.In(1)
		if firstParam.String() != "context.Context" {
			continue
		}

		args, ok := introspected.StreamMethods[methodType.Name]
		if !ok {
			continue
		}

		// Generate the input struct
		var structFields []jen.Code

		for paramIdx := 2; paramIdx < methodType.Type.NumIn()-1; paramIdx++ {
			paramName := args[paramIdx-2]
			paramType := methodType.Type.In(paramIdx)

			parsedType := jen.Id(strcase.UpperCamelCase(paramName)).Add(parseReflectType(paramType).statement)

			structFields = append(structFields,
				parsedType.
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

		parsedStreamResultType := parseReflectType(streamResultType)
		out.Type().Id(outputStructName).Struct(
			jen.Id(outputInnerFieldName).Add(parsedStreamResultType.statement),
		)

		finalResultType := parsedStreamResultType.generics[1]

		// Implement `StreamResult` interface for the output struct
		selfName := jen.Id("v")
		selfValueName := selfName.Clone().Dot(outputInnerFieldName)

		selfParam := selfName.Clone().Op("*").Id(outputStructName)

		out.Func().
			Params(selfParam.Clone()).
			Id("Kind").Params().
			Qual(common.InterfacesPkg, "StreamResultKind").
			Block(
				jen.If(selfValueName.Clone().Dot("IsError")).
					Block(jen.Return(jen.Qual(common.InterfacesPkg, "StreamResultKindError"))),
				jen.If(selfValueName.Clone().Dot("IsFinal")).
					Block(jen.Return(jen.Qual(common.InterfacesPkg, "StreamResultKindFinal"))),
				jen.Return(jen.Qual(common.InterfacesPkg, "StreamResultKindStream")),
			)

		isDynamic := hasDynamicProperties(streamResultType)

		makeValueGetter := func(method string) []jen.Code {
			if !isDynamic {
				return []jen.Code{jen.Return(selfValueName.Clone().Dot(method).Call())}
			}

			return []jen.Code{
				jen.Id("result").Op(":=").Add(selfValueName.Clone().Dot(method).Call()),
				jen.If(jen.Id("result").Dot("DynamicProperties").Op("!=").Nil()).Block(
					jen.For(jen.List(jen.Id("key"), jen.Id("value")).Op(":=").Range().Id("result").Dot("DynamicProperties")).
						Block(
							jen.If(
								jen.List(jen.Id("reflectValue"), jen.Id("ok")).Op(":=").Id("value").Assert(jen.Qual("reflect", "Value")),
								jen.Id("ok"),
							).Block(
								jen.Id("result").Dot("DynamicProperties").Index(jen.Id("key")).Op("=").Id("reflectValue").Dot("Interface").Call(),
							),
						),
				),
				jen.Return(jen.Id("result")),
			}
		}

		out.Func().
			Params(selfParam.Clone()).
			Id("Stream").Params().
			Any().
			Block(
				makeValueGetter("Stream")...,
			)

		out.Func().
			Params(selfParam.Clone()).
			Id("Final").Params().
			Any().
			Block(
				makeValueGetter("Final")...,
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
		callParams = append(callParams, jen.Id("adapter"))

		for _, arg := range args {
			argName := strcase.UpperCamelCase(arg)
			callParams = append(callParams, jen.Id("input").Dot(argName))
		}

		callParams = append(callParams, jen.Id("makeOptionsFromAdapter").Call(jen.Id("adapter")).Op("..."))

		// Call the streaming method and propagate errors
		methodBody = append(methodBody,
			jen.Id("input").Op(":=").Id("rawInput").Assert(jen.Op("*").Id(inputStructName)),

			jen.List(jen.Id("result"), jen.Id("err")).Op(":=").
				Qual(common.IntrospectedPkg, "Stream").Dot(methodName).Call(callParams...),

			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Nil(), jen.Id("err")),
			),
		)

		// Initialize the channel
		streamResultInterface := jen.Qual(common.InterfacesPkg, "StreamResult")

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
							jen.Case(jen.Op("<-").Id("adapter").Dot("Done").Call()).
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
				jen.Id("adapter").Qual(common.InterfacesPkg, "Adapter"),
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
	streamingFunctionInterface := jen.Qual(common.InterfacesPkg, "StreamingMethod")

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

	// Generate `MakeAdapter`
	out.Func().Id("MakeAdapter").
		Params(jen.Id("ctx").Qual("context", "Context")).
		Qual(common.InterfacesPkg, "Adapter").
		Block(
			jen.Return(
				jen.Op("&").Qual(selfAdapterPkg, "BamlAdapter").
					Values(jen.Dict{
						jen.Id("Context"): jen.Id("ctx"),
						jen.Id("TypeBuilderFactory"): jen.Func().Params().
							Call(
								jen.Qual(selfAdapterPkg, "BamlTypeBuilder"),
								jen.Error(),
							).
							Block(
								jen.Return(jen.Qual(common.GeneratedClientPkg, "NewTypeBuilder").Call()),
							),
					}),
			),
		)

	// Generate `makeOptionsFromAdapter`
	out.Func().Id("makeOptionsFromAdapter").
		Params(jen.Id("adapterIn").Qual(common.InterfacesPkg, "Adapter")).
		Call(
			jen.Id("result").Op("[]").Qual(common.GeneratedClientPkg, "CallOptionFunc"),
		).
		Block(
			jen.Id("adapter").Op(":=").Id("adapterIn").Assert(jen.Op("*").Qual(selfAdapterPkg, "BamlAdapter")),
			jen.If(jen.Id("adapter").Dot("ClientRegistry").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithClientRegistry").
						Call(jen.Id("adapter").Dot("ClientRegistry"))),
			),
			jen.If(jen.Id("adapter").Dot("TypeBuilder").Op("!=").Nil()).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"),
					jen.Qual(common.GeneratedClientPkg, "WithTypeBuilder").
						Call(jen.Id("adapter").Dot("TypeBuilder").Assert(jen.Op("*").Qual(common.GeneratedClientPkg, "TypeBuilder"))))),
			jen.Return(),
		)

	if err := common.Commit(out); err != nil {
		panic(err)
	}
}

type parsedReflectType struct {
	statement *jen.Statement
	generics  []jen.Code
}

func parseReflectType(typ reflect.Type) parsedReflectType {
	var ops []string
	for {
		if typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
			ops = append(ops, "*")
		} else if typ.Kind() == reflect.Slice {
			typ = typ.Elem()
			ops = append(ops, "[]")
		} else {
			break
		}
	}

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
		statement = jen.Id(typeName)
	} else {
		statement = jen.Qual(pkgPath, typeName)
	}

	for _, op := range slices.Backward(ops) {
		statement = jen.Op(op).Add(statement)
	}

	return parsedReflectType{
		statement.Types(genericsTypes...),
		genericsTypes,
	}
}

func hasDynamicProperties(typ reflect.Type) bool {
	for typ.Kind() != reflect.Ptr {
		typ = reflect.PointerTo(typ)
	}

	finalMethod, hasFinalMethod := typ.MethodByName("Final")
	if !hasFinalMethod {
		return false
	}

	finalMethodType := finalMethod.Type
	if finalMethodType.NumOut() != 1 {
		return false
	}

	returnType := finalMethodType.Out(0).Elem()
	if returnType.Kind() == reflect.Ptr {
		returnType = returnType.Elem()
	}

	if returnType.Kind() != reflect.Struct {
		return false
	}

	_, hasDynamicFields := returnType.FieldByName("DynamicProperties")
	return hasDynamicFields
}
