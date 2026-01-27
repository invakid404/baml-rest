package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/dave/jennifer/jen"
)

type parsedFile struct {
	file *ast.File
	path string
}

// TypeBuilderMethod represents a method on TypeBuilder that returns a class/enum accessor
type TypeBuilderMethod struct {
	Name       string // Method name on TypeBuilder (e.g., "Resume", "DynamicTestOutput")
	TypeName   string // The BAML type name (same as method name)
	ReturnType string // The return type name (e.g., "ResumeClassView", "DynamicTestOutputClassBuilder")
	IsDynamic  bool   // true if returns Builder, false if returns View
	IsClass    bool   // true for class, false for enum
}

func main() {
	stubMode := flag.Bool("stub", false, "Generate stub file (no baml_client required)")
	flag.Parse()

	if *stubMode {
		generateStub()
		return
	}

	generateFull()
}

func generateStub() {
	out := jen.NewFile("introspected")

	// Stream stubs
	out.Comment("Stream is the BAML streaming client instance")
	out.Var().Id("Stream").Op("=").Op("&").Struct().Values()

	out.Comment("StreamMethods is a map from method name to argument names")
	out.Var().Id("StreamMethods").Op("=").Map(jen.String()).Index().String().Values()

	out.Comment("SyncMethods maps sync function names to their argument names")
	out.Var().Id("SyncMethods").Op("=").Map(jen.String()).Index().String().Values()

	out.Comment("SyncFuncs maps sync function names to their function values (for reflection)")
	out.Var().Id("SyncFuncs").Op("=").Map(jen.String()).Any().Values()

	out.Comment("Parse is the parse API for parsing raw LLM responses into final types")
	out.Var().Id("Parse").Op("=").Op("&").Struct().Values()

	out.Comment("ParseMethods is a set of method names available on Parse")
	out.Var().Id("ParseMethods").Op("=").Map(jen.String()).Struct().Values()

	out.Comment("ParseStream is the parse_stream API for parsing raw LLM responses into partial/stream types")
	out.Var().Id("ParseStream").Op("=").Op("&").Struct().Values()

	out.Comment("ParseStreamMethods is a set of method names available on ParseStream")
	out.Var().Id("ParseStreamMethods").Op("=").Map(jen.String()).Struct().Values()

	out.Comment("ParseStreamFuncs maps ParseStream method names to their function values (for reflection)")
	out.Var().Id("ParseStreamFuncs").Op("=").Map(jen.String()).Any().Values()

	// TypeBuilder stubs
	generateTypeBuilderTypes(out, true)
	generateTypeBuilderInterfaces(out)
	generateTypeBuilderMaps(out, nil, nil, nil, nil)
	generateTypeBuilderHelpers(out, true)

	if err := out.Save("introspected/introspected.go"); err != nil {
		panic(err)
	}
}

func generateFull() {
	var (
		streamFile       *parsedFile
		parseFile        *parsedFile
		parseStreamFile  *parsedFile
		syncFuncsFile    *parsedFile
		typeBuilderFiles []*parsedFile
	)

	err := filepath.WalkDir("baml_client", func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if dirEntry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		// Look for Stream variable (functions_stream.go)
		if streamFile == nil {
			if streamVar := file.Scope.Lookup("Stream"); streamVar != nil {
				streamFile = &parsedFile{file: file, path: path}
			}
		}

		// Look for Parse variable (functions_parse.go)
		if parseFile == nil {
			if parseVar := file.Scope.Lookup("Parse"); parseVar != nil {
				parseFile = &parsedFile{file: file, path: path}
			}
		}

		// Look for ParseStream variable (functions_parse_stream.go)
		if parseStreamFile == nil {
			if parseStreamVar := file.Scope.Lookup("ParseStream"); parseStreamVar != nil {
				parseStreamFile = &parsedFile{file: file, path: path}
			}
		}

		// Collect type_builder files
		if strings.Contains(path, "type_builder") && strings.HasSuffix(path, ".go") {
			typeBuilderFiles = append(typeBuilderFiles, &parsedFile{file: file, path: path})
		}

		// Look for sync functions in functions.go
		if syncFuncsFile == nil {
			for _, decl := range file.Decls {
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if funcDecl.Recv != nil {
					continue
				}
				if funcDecl.Type.Params == nil || len(funcDecl.Type.Params.List) == 0 {
					continue
				}
				firstParam := funcDecl.Type.Params.List[0]
				if selectorExpr, ok := firstParam.Type.(*ast.SelectorExpr); ok {
					if ident, ok := selectorExpr.X.(*ast.Ident); ok {
						if ident.Name == "context" && selectorExpr.Sel.Name == "Context" {
							syncFuncsFile = &parsedFile{file: file, path: path}
							break
						}
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		panic(err)
	}

	if streamFile == nil {
		panic("stream file not found")
	}
	if parseFile == nil {
		panic("parse file not found")
	}
	if parseStreamFile == nil {
		panic("parse_stream file not found")
	}
	if syncFuncsFile == nil {
		panic("sync functions file not found")
	}

	packageName := streamFile.file.Name.Name
	streamPkg := fmt.Sprintf("github.com/invakid404/baml-rest/%s", packageName)
	typeBuilderPkg := fmt.Sprintf("github.com/invakid404/baml-rest/%s/type_builder", packageName)

	// Extract methods
	streamMethods := extractStreamMethods(streamFile)
	syncMethods := extractSyncMethods(syncFuncsFile)
	parseMethods := extractParseMethods(parseFile)
	parseStreamMethods := extractParseStreamMethods(parseStreamFile)

	// Extract TypeBuilder methods
	var dynamicClasses, staticClasses, dynamicEnums, staticEnums []TypeBuilderMethod
	for _, tbFile := range typeBuilderFiles {
		methods := extractTypeBuilderMethods(tbFile)
		for _, m := range methods {
			if m.IsClass {
				if m.IsDynamic {
					dynamicClasses = append(dynamicClasses, m)
				} else {
					staticClasses = append(staticClasses, m)
				}
			} else {
				if m.IsDynamic {
					dynamicEnums = append(dynamicEnums, m)
				} else {
					staticEnums = append(staticEnums, m)
				}
			}
		}
	}

	hasTypeBuilder := len(typeBuilderFiles) > 0

	// Generate file
	out := jen.NewFile("introspected")

	// Imports are handled automatically by jennifer

	// Stream
	out.Comment("Stream is the BAML streaming client instance")
	out.Var().Id("Stream").Op("=").Qual(streamPkg, "Stream")

	// StreamMethods
	out.Comment("StreamMethods is a map from method name to argument names")
	out.Var().Id("StreamMethods").Op("=").Map(jen.String()).Index().String().Values(
		methodsDict(streamMethods)...,
	)

	// SyncMethods
	out.Comment("SyncMethods maps sync function names to their argument names")
	out.Var().Id("SyncMethods").Op("=").Map(jen.String()).Index().String().Values(
		methodsDict(syncMethods)...,
	)

	// SyncFuncs
	out.Comment("SyncFuncs maps sync function names to their function values (for reflection)")
	out.Var().Id("SyncFuncs").Op("=").Map(jen.String()).Any().Values(
		syncFuncsDict(syncMethods, streamPkg)...,
	)

	// Parse
	out.Comment("Parse is the parse API for parsing raw LLM responses into final types")
	out.Var().Id("Parse").Op("=").Qual(streamPkg, "Parse")

	// ParseMethods
	out.Comment("ParseMethods is a set of method names available on Parse")
	out.Var().Id("ParseMethods").Op("=").Map(jen.String()).Struct().Values(
		parseMethodsDict(parseMethods)...,
	)

	// ParseStream
	out.Comment("ParseStream is the parse_stream API for parsing raw LLM responses into partial/stream types")
	out.Var().Id("ParseStream").Op("=").Qual(streamPkg, "ParseStream")

	// ParseStreamMethods
	out.Comment("ParseStreamMethods is a set of method names available on ParseStream")
	out.Var().Id("ParseStreamMethods").Op("=").Map(jen.String()).Struct().Values(
		parseMethodsDict(parseStreamMethods)...,
	)

	// ParseStreamFuncs
	out.Comment("ParseStreamFuncs maps ParseStream method names to their function values (for reflection)")
	out.Var().Id("ParseStreamFuncs").Op("=").Map(jen.String()).Any().Values(
		parseStreamFuncsDict(parseStreamMethods, streamPkg)...,
	)

	// TypeBuilder types and helpers
	if hasTypeBuilder {
		generateTypeBuilderTypesWithAliases(out, typeBuilderPkg, streamPkg)
		generateTypeBuilderInterfaces(out)
		generateTypeBuilderMaps(out, dynamicClasses, staticClasses, dynamicEnums, staticEnums)
		generateTypeBuilderHelpers(out, false)
	}

	if err := out.Save("introspected/introspected.go"); err != nil {
		panic(err)
	}
}

func generateTypeBuilderTypes(out *jen.File, isStub bool) {
	out.Comment("TypeBuilder type")
	out.Type().Id("TypeBuilder").Struct()

	out.Comment("Type is the BAML type")
	out.Type().Id("Type").Interface()

	out.Comment("ClassPropertyBuilder is the interface for class property builders")
	out.Type().Id("ClassPropertyBuilder").Interface()

	out.Comment("EnumValueBuilder is the interface for enum value builders")
	out.Type().Id("EnumValueBuilder").Interface(
		jen.Id("SetDescription").Params(jen.Id("description").String()).Error(),
		jen.Id("SetAlias").Params(jen.Id("alias").String()).Error(),
		jen.Id("SetSkip").Params(jen.Id("skip").Bool()).Error(),
	)

	out.Comment("NewTypeBuilder creates a new TypeBuilder")
	out.Var().Id("NewTypeBuilder").Op("=").Func().Params().Params(
		jen.Op("*").Id("TypeBuilder"),
		jen.Error(),
	).Block(
		jen.Return(jen.Nil(), jen.Nil()),
	)
}

func generateTypeBuilderTypesWithAliases(out *jen.File, typeBuilderPkg, streamPkg string) {
	out.Comment("TypeBuilder is the generated TypeBuilder type")
	out.Type().Id("TypeBuilder").Op("=").Qual(typeBuilderPkg, "TypeBuilder")

	out.Comment("Type is the BAML type")
	out.Type().Id("Type").Op("=").Qual(typeBuilderPkg, "Type")

	out.Comment("ClassPropertyBuilder is the interface for class property builders")
	out.Type().Id("ClassPropertyBuilder").Op("=").Qual(typeBuilderPkg, "ClassPropertyBuilder")

	out.Comment("EnumValueBuilder is the interface for enum value builders")
	out.Type().Id("EnumValueBuilder").Op("=").Qual(typeBuilderPkg, "EnumValueBuilder")

	out.Comment("NewTypeBuilder creates a new TypeBuilder using the BAML runtime's factory")
	out.Var().Id("NewTypeBuilder").Op("=").Qual(streamPkg, "NewTypeBuilder")
}

func generateTypeBuilderInterfaces(out *jen.File) {
	out.Comment("Typed is a minimal interface for types that can return a Type")
	out.Type().Id("Typed").Interface(
		jen.Id("Type").Params().Params(jen.Id("Type"), jen.Error()),
	)

	out.Comment("DynamicClassBuilder is the interface for dynamic classes")
	out.Type().Id("DynamicClassBuilder").Interface(
		jen.Id("Type").Params().Params(jen.Id("Type"), jen.Error()),
		jen.Id("AddProperty").Params(
			jen.Id("name").String(),
			jen.Id("fieldType").Id("Type"),
		).Params(jen.Id("ClassPropertyBuilder"), jen.Error()),
	)

	out.Comment("DynamicEnumBuilder is the interface for dynamic enums")
	out.Type().Id("DynamicEnumBuilder").Interface(
		jen.Id("Type").Params().Params(jen.Id("Type"), jen.Error()),
		jen.Id("AddValue").Params(jen.Id("value").String()).Params(jen.Id("EnumValueBuilder"), jen.Error()),
	)

	out.Comment("DynamicClassAccessor is a function that returns a DynamicClassBuilder for a dynamic class")
	out.Type().Id("DynamicClassAccessor").Func().Params(
		jen.Op("*").Id("TypeBuilder"),
	).Params(jen.Id("DynamicClassBuilder"), jen.Error())

	out.Comment("DynamicEnumAccessor is a function that returns a DynamicEnumBuilder for a dynamic enum")
	out.Type().Id("DynamicEnumAccessor").Func().Params(
		jen.Op("*").Id("TypeBuilder"),
	).Params(jen.Id("DynamicEnumBuilder"), jen.Error())

	out.Comment("StaticClassAccessor is a function that returns a Typed for a static class")
	out.Type().Id("StaticClassAccessor").Func().Params(
		jen.Op("*").Id("TypeBuilder"),
	).Params(jen.Id("Typed"), jen.Error())

	out.Comment("StaticEnumAccessor is a function that returns a Typed for a static enum")
	out.Type().Id("StaticEnumAccessor").Func().Params(
		jen.Op("*").Id("TypeBuilder"),
	).Params(jen.Id("Typed"), jen.Error())
}

func generateTypeBuilderMaps(out *jen.File, dynamicClasses, staticClasses, dynamicEnums, staticEnums []TypeBuilderMethod) {
	// DynamicClasses
	out.Comment("DynamicClasses maps dynamic class names to their accessor functions")
	entries := make([]jen.Code, 0, len(dynamicClasses))
	for _, m := range dynamicClasses {
		entries = append(entries, jen.Lit(m.TypeName).Op(":").Func().Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
		).Params(jen.Id("DynamicClassBuilder"), jen.Error()).Block(
			jen.Return(jen.Id("tb").Dot(m.Name).Call()),
		))
	}
	out.Var().Id("DynamicClasses").Op("=").Map(jen.String()).Id("DynamicClassAccessor").Values(entries...)

	// StaticClasses
	out.Comment("StaticClasses maps static class names to their accessor functions")
	entries = make([]jen.Code, 0, len(staticClasses))
	for _, m := range staticClasses {
		entries = append(entries, jen.Lit(m.TypeName).Op(":").Func().Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
		).Params(jen.Id("Typed"), jen.Error()).Block(
			jen.Return(jen.Id("tb").Dot(m.Name).Call()),
		))
	}
	out.Var().Id("StaticClasses").Op("=").Map(jen.String()).Id("StaticClassAccessor").Values(entries...)

	// DynamicEnums
	out.Comment("DynamicEnums maps dynamic enum names to their accessor functions")
	entries = make([]jen.Code, 0, len(dynamicEnums))
	for _, m := range dynamicEnums {
		entries = append(entries, jen.Lit(m.TypeName).Op(":").Func().Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
		).Params(jen.Id("DynamicEnumBuilder"), jen.Error()).Block(
			jen.Return(jen.Id("tb").Dot(m.Name).Call()),
		))
	}
	out.Var().Id("DynamicEnums").Op("=").Map(jen.String()).Id("DynamicEnumAccessor").Values(entries...)

	// StaticEnums
	out.Comment("StaticEnums maps static enum names to their accessor functions")
	entries = make([]jen.Code, 0, len(staticEnums))
	for _, m := range staticEnums {
		entries = append(entries, jen.Lit(m.TypeName).Op(":").Func().Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
		).Params(jen.Id("Typed"), jen.Error()).Block(
			jen.Return(jen.Id("tb").Dot(m.Name).Call()),
		))
	}
	out.Var().Id("StaticEnums").Op("=").Map(jen.String()).Id("StaticEnumAccessor").Values(entries...)
}

func generateTypeBuilderHelpers(out *jen.File, isStub bool) {
	// AllClasses
	out.Comment("AllClasses returns all known class names (both dynamic and static)")
	if isStub {
		out.Func().Id("AllClasses").Params().Index().String().Block(
			jen.Return(jen.Nil()),
		)
	} else {
		out.Func().Id("AllClasses").Params().Index().String().Block(
			jen.Id("result").Op(":=").Make(jen.Index().String(), jen.Lit(0), jen.Len(jen.Id("DynamicClasses")).Op("+").Len(jen.Id("StaticClasses"))),
			jen.For(jen.Id("name").Op(":=").Range().Id("DynamicClasses")).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"), jen.Id("name")),
			),
			jen.For(jen.Id("name").Op(":=").Range().Id("StaticClasses")).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"), jen.Id("name")),
			),
			jen.Return(jen.Id("result")),
		)
	}

	// AllEnums
	out.Comment("AllEnums returns all known enum names (both dynamic and static)")
	if isStub {
		out.Func().Id("AllEnums").Params().Index().String().Block(
			jen.Return(jen.Nil()),
		)
	} else {
		out.Func().Id("AllEnums").Params().Index().String().Block(
			jen.Id("result").Op(":=").Make(jen.Index().String(), jen.Lit(0), jen.Len(jen.Id("DynamicEnums")).Op("+").Len(jen.Id("StaticEnums"))),
			jen.For(jen.Id("name").Op(":=").Range().Id("DynamicEnums")).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"), jen.Id("name")),
			),
			jen.For(jen.Id("name").Op(":=").Range().Id("StaticEnums")).Block(
				jen.Id("result").Op("=").Append(jen.Id("result"), jen.Id("name")),
			),
			jen.Return(jen.Id("result")),
		)
	}

	// IsDynamicClass
	out.Comment("IsDynamicClass returns true if the class exists and is dynamic")
	if isStub {
		out.Func().Id("IsDynamicClass").Params(jen.Id("name").String()).Bool().Block(
			jen.Return(jen.False()),
		)
	} else {
		out.Func().Id("IsDynamicClass").Params(jen.Id("name").String()).Bool().Block(
			jen.List(jen.Id("_"), jen.Id("ok")).Op(":=").Id("DynamicClasses").Index(jen.Id("name")),
			jen.Return(jen.Id("ok")),
		)
	}

	// IsDynamicEnum
	out.Comment("IsDynamicEnum returns true if the enum exists and is dynamic")
	if isStub {
		out.Func().Id("IsDynamicEnum").Params(jen.Id("name").String()).Bool().Block(
			jen.Return(jen.False()),
		)
	} else {
		out.Func().Id("IsDynamicEnum").Params(jen.Id("name").String()).Bool().Block(
			jen.List(jen.Id("_"), jen.Id("ok")).Op(":=").Id("DynamicEnums").Index(jen.Id("name")),
			jen.Return(jen.Id("ok")),
		)
	}

	// ClassExists
	out.Comment("ClassExists returns true if a class with this name exists (dynamic or static)")
	if isStub {
		out.Func().Id("ClassExists").Params(jen.Id("name").String()).Bool().Block(
			jen.Return(jen.False()),
		)
	} else {
		out.Func().Id("ClassExists").Params(jen.Id("name").String()).Bool().Block(
			jen.List(jen.Id("_"), jen.Id("dynamic")).Op(":=").Id("DynamicClasses").Index(jen.Id("name")),
			jen.List(jen.Id("_"), jen.Id("static")).Op(":=").Id("StaticClasses").Index(jen.Id("name")),
			jen.Return(jen.Id("dynamic").Op("||").Id("static")),
		)
	}

	// EnumExists
	out.Comment("EnumExists returns true if an enum with this name exists (dynamic or static)")
	if isStub {
		out.Func().Id("EnumExists").Params(jen.Id("name").String()).Bool().Block(
			jen.Return(jen.False()),
		)
	} else {
		out.Func().Id("EnumExists").Params(jen.Id("name").String()).Bool().Block(
			jen.List(jen.Id("_"), jen.Id("dynamic")).Op(":=").Id("DynamicEnums").Index(jen.Id("name")),
			jen.List(jen.Id("_"), jen.Id("static")).Op(":=").Id("StaticEnums").Index(jen.Id("name")),
			jen.Return(jen.Id("dynamic").Op("||").Id("static")),
		)
	}

	// GetClassType
	out.Comment("GetClassType returns the Type for a class by name")
	if isStub {
		out.Func().Id("GetClassType").Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
			jen.Id("name").String(),
		).Params(jen.Id("Type"), jen.Error()).Block(
			jen.Return(jen.Nil(), jen.Nil()),
		)
	} else {
		out.Func().Id("GetClassType").Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
			jen.Id("name").String(),
		).Params(jen.Id("Type"), jen.Error()).Block(
			jen.If(
				jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Id("DynamicClasses").Index(jen.Id("name")),
				jen.Id("ok"),
			).Block(
				jen.List(jen.Id("builder"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				),
				jen.Return(jen.Id("builder").Dot("Type").Call()),
			),
			jen.If(
				jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Id("StaticClasses").Index(jen.Id("name")),
				jen.Id("ok"),
			).Block(
				jen.List(jen.Id("view"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				),
				jen.Return(jen.Id("view").Dot("Type").Call()),
			),
			jen.Return(jen.Nil(), jen.Nil()),
		)
	}

	// GetEnumType
	out.Comment("GetEnumType returns the Type for an enum by name")
	if isStub {
		out.Func().Id("GetEnumType").Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
			jen.Id("name").String(),
		).Params(jen.Id("Type"), jen.Error()).Block(
			jen.Return(jen.Nil(), jen.Nil()),
		)
	} else {
		out.Func().Id("GetEnumType").Params(
			jen.Id("tb").Op("*").Id("TypeBuilder"),
			jen.Id("name").String(),
		).Params(jen.Id("Type"), jen.Error()).Block(
			jen.If(
				jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Id("DynamicEnums").Index(jen.Id("name")),
				jen.Id("ok"),
			).Block(
				jen.List(jen.Id("builder"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				),
				jen.Return(jen.Id("builder").Dot("Type").Call()),
			),
			jen.If(
				jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Id("StaticEnums").Index(jen.Id("name")),
				jen.Id("ok"),
			).Block(
				jen.List(jen.Id("view"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Id("err")),
				),
				jen.Return(jen.Id("view").Dot("Type").Call()),
			),
			jen.Return(jen.Nil(), jen.Nil()),
		)
	}
}

// Helper functions for extracting AST info

func extractStreamMethods(f *parsedFile) []map[string]any {
	var methods []map[string]any
	for _, decl := range f.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			continue
		}
		receiver, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		receiverIdent, ok := receiver.X.(*ast.Ident)
		if !ok || receiverIdent.Name != "stream" {
			continue
		}
		var args []string
		for _, param := range funcDecl.Type.Params.List {
			args = append(args, param.Names[0].Name)
		}
		methods = append(methods, map[string]any{
			"name": funcDecl.Name.Name,
			"args": args[1 : len(args)-1], // Remove context and variadic
		})
	}
	return methods
}

func extractSyncMethods(f *parsedFile) []map[string]any {
	var methods []map[string]any
	for _, decl := range f.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv != nil {
			continue
		}
		if funcDecl.Type.Params == nil || len(funcDecl.Type.Params.List) == 0 {
			continue
		}
		firstParam := funcDecl.Type.Params.List[0]
		selectorExpr, ok := firstParam.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		ident, ok := selectorExpr.X.(*ast.Ident)
		if !ok || ident.Name != "context" || selectorExpr.Sel.Name != "Context" {
			continue
		}
		var args []string
		for _, param := range funcDecl.Type.Params.List {
			for _, name := range param.Names {
				args = append(args, name.Name)
			}
		}
		if len(args) >= 2 {
			methods = append(methods, map[string]any{
				"name": funcDecl.Name.Name,
				"args": args[1 : len(args)-1],
			})
		}
	}
	return methods
}

func extractParseMethods(f *parsedFile) []string {
	var methods []string
	for _, decl := range f.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			continue
		}
		receiver, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		receiverIdent, ok := receiver.X.(*ast.Ident)
		if !ok || receiverIdent.Name != "parse" {
			continue
		}
		methods = append(methods, funcDecl.Name.Name)
	}
	return methods
}

func extractParseStreamMethods(f *parsedFile) []string {
	var methods []string
	for _, decl := range f.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			continue
		}
		receiver, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		receiverIdent, ok := receiver.X.(*ast.Ident)
		if !ok || receiverIdent.Name != "parse_stream" {
			continue
		}
		methods = append(methods, funcDecl.Name.Name)
	}
	return methods
}

func extractTypeBuilderMethods(f *parsedFile) []TypeBuilderMethod {
	var methods []TypeBuilderMethod
	for _, decl := range f.file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			continue
		}
		receiver, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		receiverIdent, ok := receiver.X.(*ast.Ident)
		if !ok || receiverIdent.Name != "TypeBuilder" {
			continue
		}
		if funcDecl.Type.Results == nil || len(funcDecl.Type.Results.List) != 2 {
			continue
		}
		firstResult, ok := funcDecl.Type.Results.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		returnTypeIdent, ok := firstResult.X.(*ast.Ident)
		if !ok {
			continue
		}
		returnTypeName := returnTypeIdent.Name

		var isClass, isDynamic bool
		var typeName string

		if strings.HasSuffix(returnTypeName, "ClassBuilder") {
			isClass, isDynamic = true, true
			typeName = strings.TrimSuffix(returnTypeName, "ClassBuilder")
		} else if strings.HasSuffix(returnTypeName, "ClassView") {
			isClass, isDynamic = true, false
			typeName = strings.TrimSuffix(returnTypeName, "ClassView")
		} else if strings.HasSuffix(returnTypeName, "EnumBuilder") {
			isClass, isDynamic = false, true
			typeName = strings.TrimSuffix(returnTypeName, "EnumBuilder")
		} else if strings.HasSuffix(returnTypeName, "EnumView") {
			isClass, isDynamic = false, false
			typeName = strings.TrimSuffix(returnTypeName, "EnumView")
		} else {
			continue
		}

		methods = append(methods, TypeBuilderMethod{
			Name:       funcDecl.Name.Name,
			TypeName:   typeName,
			ReturnType: returnTypeName,
			IsDynamic:  isDynamic,
			IsClass:    isClass,
		})
	}
	return methods
}

// Dict builders for jennifer

func methodsDict(methods []map[string]any) []jen.Code {
	entries := make([]jen.Code, 0, len(methods))
	for _, m := range methods {
		args := m["args"].([]string)
		argLits := make([]jen.Code, len(args))
		for i, a := range args {
			argLits[i] = jen.Lit(a)
		}
		entries = append(entries, jen.Lit(m["name"].(string)).Op(":").Index().String().Values(argLits...))
	}
	return entries
}

func syncFuncsDict(methods []map[string]any, streamPkg string) []jen.Code {
	entries := make([]jen.Code, 0, len(methods))
	for _, m := range methods {
		name := m["name"].(string)
		entries = append(entries, jen.Lit(name).Op(":").Qual(streamPkg, name))
	}
	return entries
}

func parseMethodsDict(methods []string) []jen.Code {
	entries := make([]jen.Code, 0, len(methods))
	for _, m := range methods {
		entries = append(entries, jen.Lit(m).Op(":").Values())
	}
	return entries
}

func parseStreamFuncsDict(methods []string, streamPkg string) []jen.Code {
	entries := make([]jen.Code, 0, len(methods))
	for _, m := range methods {
		entries = append(entries, jen.Lit(m).Op(":").Qual(streamPkg, "ParseStream").Dot(m))
	}
	return entries
}
