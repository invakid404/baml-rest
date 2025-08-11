package main

import (
	"fmt"
	"github.com/invakid404/baml-rest"
	"reflect"
)

func main() {
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

		fmt.Println(methodType.Name)
		for paramIdx := 2; paramIdx < methodType.Type.NumIn()-1; paramIdx++ {
			paramName := args[paramIdx-2]
			paramType := methodType.Type.In(paramIdx)

			fmt.Println(" ", paramName, paramType)
		}
	}
}
