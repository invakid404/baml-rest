package main

import (
	"fmt"
	"github.com/invakid404/baml-rest"
	"reflect"
)

func main() {
	streamReflect := reflect.ValueOf(baml_rest.Stream)
	for idx := 0; idx < streamReflect.NumMethod(); idx++ {
		fmt.Println(streamReflect.Type().Method(idx).Name)
	}
}
