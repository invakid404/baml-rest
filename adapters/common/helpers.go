package common

import (
	"github.com/dave/jennifer/jen"
)

const (
	RootPkg     = "github.com/invakid404/baml-rest"
	RootPkgName = "baml_rest"

	IntrospectedPkg = RootPkg + "/introspected"
	InterfacesPkg   = RootPkg + "/bamlutils"

	OutputPath = "adapter.go"
)

func MakeFile() *jen.File {
	return jen.NewFilePathName(RootPkg, RootPkgName)
}

func Commit(file *jen.File) error {
	return file.Save(OutputPath)
}
