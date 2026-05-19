package codegen

import (
	"github.com/dave/jennifer/jen"
)

// enumValueAttrsCode returns jen.Code statements for setting Description, Alias, and Skip
// on an enum value builder (vb) from a value struct (v).
func enumValueAttrsCode() []jen.Code {
	return []jen.Code{
		jen.If(jen.Id("v").Dot("Description").Op("!=").Lit("")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetDescription").Call(jen.Id("v").Dot("Description")),
		),
		jen.If(jen.Id("v").Dot("Alias").Op("!=").Lit("")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetAlias").Call(jen.Id("v").Dot("Alias")),
		),
		jen.If(jen.Id("v").Dot("Skip")).Block(
			jen.Id("_").Op("=").Id("vb").Dot("SetSkip").Call(jen.True()),
		),
	}
}

// generateApplyDynamicTypes generates the applyDynamicTypes function that translates
// DynamicTypes JSON schema to imperative TypeBuilder calls.
// Uses the introspected package for type lookups instead of reflection.
func generateApplyDynamicTypes(out *jen.File, pkgs PackageConfig) {
	// Use the configured introspected.Type to be consistent with
	// the introspected TypeBuilder (both come from the generated
	// client, not the runtime library directly).
	introspectedPkg := pkgs.IntrospectedPkg
	typeAlias := jen.Qual(introspectedPkg, "Type")
	// Generate applyDynamicTypes function
	out.Func().Id("applyDynamicTypes").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("dt").Op("*").Qual(pkgs.InterfacesPkg, "DynamicTypes"),
		).
		Error().
		Block(
			jen.If(jen.Id("dt").Op("==").Nil()).Block(
				jen.Return(jen.Nil()),
			),
			// Validate the schema before processing
			jen.If(jen.Id("err").Op(":=").Id("dt").Dot("Validate").Call(), jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("invalid dynamic_types schema: %w"), jen.Id("err"))),
			),
			// Create caches
			jen.Id("typeCache").Op(":=").Make(jen.Map(jen.String()).Add(typeAlias)),
			jen.Id("classBuilderCache").Op(":=").Make(jen.Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder")),
			jen.Line(),
			// Phase 1: Create all enum shells (for NEW enums only, with values since we have builder)
			// Iterate sorted keys so TypeBuilder population order is deterministic: Go map
			// iteration is intentionally randomised, and BAML preserves the order it receives,
			// so unsorted ranging produces non-deterministic rendered output_format text.
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("dt").Dot("Enums"))).Block(
				jen.Id("enum").Op(":=").Id("dt").Dot("Enums").Index(jen.Id("name")),
				jen.If(jen.Id("err").Op(":=").Id("createEnumShell").Call(jen.Id("tb"), jen.Id("name"), jen.Id("enum"), jen.Id("typeCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("enum %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 2: Add values to EXISTING enums
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("dt").Dot("Enums"))).Block(
				jen.Id("enum").Op(":=").Id("dt").Dot("Enums").Index(jen.Id("name")),
				jen.If(jen.Id("err").Op(":=").Id("addEnumValues").Call(jen.Id("tb"), jen.Id("name"), jen.Id("enum"), jen.Id("typeCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("enum %q values: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 3: Create all NEW class shells (no properties yet - just register the type)
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("dt").Dot("Classes"))).Block(
				jen.If(jen.Id("err").Op(":=").Id("createClassShell").Call(jen.Id("tb"), jen.Id("name"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 4a: Add properties to NEW classes only (from classBuilderCache)
			// These classes only have primitive types or reference other new classes
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("dt").Dot("Classes"))).Block(
				jen.Id("class").Op(":=").Id("dt").Dot("Classes").Index(jen.Id("name")),
				jen.If(jen.Id("err").Op(":=").Id("addNewClassProperties").Call(jen.Id("tb"), jen.Id("name"), jen.Id("class"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q properties: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Line(),
			// Phase 4b: Cache types for all NEW classes (now that properties are added)
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("classBuilderCache"))).Block(
				jen.Id("cb").Op(":=").Id("classBuilderCache").Index(jen.Id("name")),
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("cb").Dot("Type").Call(),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q type: %w"), jen.Id("name"), jen.Id("err"))),
				),
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
			),
			jen.Line(),
			// Phase 4c: Add properties to EXISTING dynamic classes (refs can now be resolved)
			jen.For(jen.List(jen.Id("_"), jen.Id("name")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("dt").Dot("Classes"))).Block(
				jen.Id("class").Op(":=").Id("dt").Dot("Classes").Index(jen.Id("name")),
				jen.If(jen.Id("err").Op(":=").Id("addExistingClassProperties").Call(jen.Id("tb"), jen.Id("name"), jen.Id("class"), jen.Id("typeCache"), jen.Id("classBuilderCache")), jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("class %q properties: %w"), jen.Id("name"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate sortedMapKeys helper - returns map keys sorted alphabetically.
	// Used by applyDynamicTypes to make TypeBuilder population deterministic.
	out.Func().Id("sortedMapKeys").
		Types(jen.Id("V").Any()).
		Params(jen.Id("m").Map(jen.String()).Id("V")).
		Index().String().
		Block(
			jen.Id("keys").Op(":=").Make(jen.Index().String(), jen.Lit(0), jen.Len(jen.Id("m"))),
			jen.For(jen.Id("k").Op(":=").Range().Id("m")).Block(
				jen.Id("keys").Op("=").Append(jen.Id("keys"), jen.Id("k")),
			),
			jen.Qual("slices", "Sort").Call(jen.Id("keys")),
			jen.Return(jen.Id("keys")),
		)

	// Generate createEnumShell helper - creates enum with values (new) or skips existing
	// For NEW enums: creates enum AND adds values (since we have the builder)
	// For EXISTING enums: does nothing (values added in Phase 2 via introspected accessors)
	// IMPORTANT: We must check if the enum already exists BEFORE calling AddEnum, because
	// calling AddEnum on an existing enum may have side effects in BAML's internal state.
	out.Func().Id("createEnumShell").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("enum").Op("*").Qual(pkgs.InterfacesPkg, "DynamicEnum"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
		).
		Error().
		Block(
			// Check if enum already exists (either dynamic or static from baml_src)
			// If so, skip - values will be added in Phase 2 for dynamic enums
			jen.If(jen.Qual(introspectedPkg, "EnumExists").Call(jen.Id("name"))).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Create new enum
			jen.List(jen.Id("eb"), jen.Id("err")).Op(":=").Id("tb").Dot("AddEnum").Call(jen.Id("name")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to create enum: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// NEW enum - add values now since we have the builder
			jen.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id("enum").Dot("Values")).Block(
				append([]jen.Code{
					jen.List(jen.Id("vb"), jen.Id("err")).Op(":=").Id("eb").Dot("AddValue").Call(jen.Id("v").Dot("Name")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Continue(), // Value might already exist
					),
				}, enumValueAttrsCode()...)...,
			),
			jen.Line(),
			// Cache the new enum's type
			jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("eb").Dot("Type").Call(),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get type: %w"), jen.Id("err"))),
			),
			jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
			jen.Return(jen.Nil()),
		)

	// Generate addEnumValues helper - adds values to EXISTING dynamic enums (Phase 2)
	// Only for enums that already existed in baml_src (marked @@dynamic)
	// New enums already had values added in createEnumShell
	out.Func().Id("addEnumValues").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("enum").Op("*").Qual(pkgs.InterfacesPkg, "DynamicEnum"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
		).
		Error().
		Block(
			jen.Id("_").Op("=").Id("typeCache"), // unused
			// Check if this is an existing dynamic enum via introspected accessor
			jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Qual(introspectedPkg, "DynamicEnums").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Not a dynamic enum OR it's a new enum (values already added in Phase 1)
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Get the enum builder using the typed accessor
			jen.List(jen.Id("eb"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get enum builder: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Add values using the typed EnumBuilder interface
			jen.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id("enum").Dot("Values")).Block(
				append([]jen.Code{
					jen.List(jen.Id("vb"), jen.Id("err")).Op(":=").Id("eb").Dot("AddValue").Call(jen.Id("v").Dot("Name")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						// Value might already exist, skip silently
						jen.Continue(),
					),
				}, enumValueAttrsCode()...)...,
			),
			jen.Return(jen.Nil()),
		)

	// Generate createClassShell helper - creates class shell only (no properties, no type caching)
	// For NEW classes: creates class, caches builder for Phase 4a
	// For EXISTING classes: does nothing (properties added in Phase 4c via introspected accessors)
	// IMPORTANT: We must check if the class already exists BEFORE calling AddClass, because
	// calling AddClass on an existing class may have side effects in BAML's internal state.
	// IMPORTANT: We do NOT call Type() here - that happens in Phase 4b AFTER properties are added
	out.Func().Id("createClassShell").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			jen.Id("_").Op("=").Id("typeCache"), // unused in this function
			// Check if class already exists (either dynamic or static from baml_src)
			// If so, skip - properties will be added in Phase 4c for dynamic classes
			jen.If(jen.Qual(introspectedPkg, "ClassExists").Call(jen.Id("name"))).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Create new class shell (no properties yet, no Type() call)
			jen.List(jen.Id("cb"), jen.Id("err")).Op(":=").Id("tb").Dot("AddClass").Call(jen.Id("name")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("failed to create class: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Cache the builder for Phase 4a property additions
			// Type will be cached in Phase 4b AFTER properties are added
			jen.Id("classBuilderCache").Index(jen.Id("name")).Op("=").Id("cb"),
			jen.Return(jen.Nil()),
		)

	// Generate addNewClassProperties helper - adds properties to NEW classes only (Phase 4a)
	// Only processes classes that are in classBuilderCache (created in Phase 3)
	// Properties should only reference primitive types or other new classes
	out.Func().Id("addNewClassProperties").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("class").Op("*").Qual(pkgs.InterfacesPkg, "DynamicClass"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			// Only process NEW classes (in classBuilderCache)
			jen.List(jen.Id("cb"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Not a new class - skip (will be processed in Phase 4c)
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Add properties to the new class builder
			jen.For(jen.List(jen.Id("_"), jen.Id("propName")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("class").Dot("Properties"))).Block(
				jen.Id("prop").Op(":=").Id("class").Dot("Properties").Index(jen.Id("propName")),
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolvePropertyType").Call(jen.Id("tb"), jen.Id("prop"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					// Skip unresolved refs - they may reference existing types in baml_src
					// which will be resolved when the type is used
					jen.If(jen.Qual("strings", "Contains").Call(jen.Id("err").Dot("Error").Call(), jen.Lit("unresolved reference"))).Block(
						jen.Continue(),
					),
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q type: %w"), jen.Id("propName"), jen.Id("err"))),
				),
				jen.Line(),
				// Call AddProperty using the typed ClassBuilder interface
				jen.List(jen.Id("_"), jen.Id("err")).Op("=").Id("cb").Dot("AddProperty").Call(jen.Id("propName"), jen.Id("typ")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q: %w"), jen.Id("propName"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate addExistingClassProperties helper - adds properties to EXISTING dynamic classes (Phase 4c)
	// Only processes classes that are in introspected.DynamicClasses (from baml_src with @@dynamic)
	// At this point, all new class types are cached in typeCache (from Phase 4b)
	out.Func().Id("addExistingClassProperties").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("class").Op("*").Qual(pkgs.InterfacesPkg, "DynamicClass"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Error().
		Block(
			// Skip if this is a NEW class (already processed in Phase 4a)
			jen.If(jen.List(jen.Id("_"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Check if it's an EXISTING dynamic class
			jen.List(jen.Id("accessor"), jen.Id("ok")).Op(":=").Qual(introspectedPkg, "DynamicClasses").Index(jen.Id("name")),
			jen.If(jen.Op("!").Id("ok")).Block(
				// Class is read-only (static) - cannot add properties
				jen.Return(jen.Nil()),
			),
			jen.Line(),
			// Get the class builder using the typed accessor
			jen.List(jen.Id("cb"), jen.Id("err")).Op(":=").Id("accessor").Call(jen.Id("tb")),
			jen.If(jen.Id("err").Op("!=").Nil()).Block(
				jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("get class builder: %w"), jen.Id("err"))),
			),
			jen.Line(),
			// Add properties to the existing class builder
			jen.For(jen.List(jen.Id("_"), jen.Id("propName")).Op(":=").Range().Id("sortedMapKeys").Call(jen.Id("class").Dot("Properties"))).Block(
				jen.Id("prop").Op(":=").Id("class").Dot("Properties").Index(jen.Id("propName")),
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolvePropertyType").Call(jen.Id("tb"), jen.Id("prop"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					// Skip unresolved refs - they may reference types in baml_src
					jen.If(jen.Qual("strings", "Contains").Call(jen.Id("err").Dot("Error").Call(), jen.Lit("unresolved reference"))).Block(
						jen.Continue(),
					),
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q type: %w"), jen.Id("propName"), jen.Id("err"))),
				),
				jen.Line(),
				// Call AddProperty using the typed ClassBuilder interface
				jen.List(jen.Id("_"), jen.Id("err")).Op("=").Id("cb").Dot("AddProperty").Call(jen.Id("propName"), jen.Id("typ")),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("property %q: %w"), jen.Id("propName"), jen.Id("err"))),
				),
			),
			jen.Return(jen.Nil()),
		)

	// Generate resolvePropertyType helper
	out.Func().Id("resolvePropertyType").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("prop").Op("*").Qual(pkgs.InterfacesPkg, "DynamicProperty"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			// Handle $ref
			jen.If(jen.Id("prop").Dot("Ref").Op("!=").Lit("")).Block(
				jen.Return(jen.Id("resolveRef").Call(jen.Id("tb"), jen.Id("prop").Dot("Ref"), jen.Id("typeCache"), jen.Id("classBuilderCache"))),
			),
			jen.Line(),
			// Convert to DynamicTypeSpec and resolve
			jen.Return(jen.Id("resolveTypeRef").Call(
				jen.Id("tb"),
				jen.Op("&").Qual(pkgs.InterfacesPkg, "DynamicTypeSpec").Values(jen.Dict{
					jen.Id("Type"):   jen.Id("prop").Dot("Type"),
					jen.Id("Items"):  jen.Id("prop").Dot("Items"),
					jen.Id("Inner"):  jen.Id("prop").Dot("Inner"),
					jen.Id("OneOf"):  jen.Id("prop").Dot("OneOf"),
					jen.Id("Keys"):   jen.Id("prop").Dot("Keys"),
					jen.Id("Values"): jen.Id("prop").Dot("Values"),
					jen.Id("Value"):  jen.Id("prop").Dot("Value"),
				}),
				jen.Id("typeCache"),
				jen.Id("classBuilderCache"),
			)),
		)

	// Generate resolveTypeRef helper
	out.Func().Id("resolveTypeRef").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("ref").Op("*").Qual(pkgs.InterfacesPkg, "DynamicTypeSpec"),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			jen.If(jen.Id("ref").Op("==").Nil()).Block(
				jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("nil type reference"))),
			),
			jen.Line(),
			// Handle $ref
			jen.If(jen.Id("ref").Dot("Ref").Op("!=").Lit("")).Block(
				jen.Return(jen.Id("resolveRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Ref"), jen.Id("typeCache"), jen.Id("classBuilderCache"))),
			),
			jen.Line(),
			jen.Switch(jen.Id("ref").Dot("Type")).Block(
				jen.Case(jen.Lit("string")).Block(
					jen.Return(jen.Id("tb").Dot("String").Call()),
				),
				jen.Case(jen.Lit("int")).Block(
					jen.Return(jen.Id("tb").Dot("Int").Call()),
				),
				jen.Case(jen.Lit("float")).Block(
					jen.Return(jen.Id("tb").Dot("Float").Call()),
				),
				jen.Case(jen.Lit("bool")).Block(
					jen.Return(jen.Id("tb").Dot("Bool").Call()),
				),
				jen.Case(jen.Lit("null")).Block(
					jen.Return(jen.Id("tb").Dot("Null").Call()),
				),
				jen.Case(jen.Lit("literal_string")).Block(
					jen.List(jen.Id("str"), jen.Id("ok")).Op(":=").Id("ref").Dot("Value").Assert(jen.String()),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_string value must be a string, got %T"), jen.Id("ref").Dot("Value"))),
					),
					jen.Return(jen.Id("tb").Dot("LiteralString").Call(jen.Id("str"))),
				),
				jen.Case(jen.Lit("literal_int")).Block(
					jen.Switch(jen.Id("v").Op(":=").Id("ref").Dot("Value").Assert(jen.Type())).Block(
						jen.Case(jen.Float64()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Int64().Call(jen.Id("v")))),
						),
						jen.Case(jen.Int64()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Id("v"))),
						),
						jen.Case(jen.Int()).Block(
							jen.Return(jen.Id("tb").Dot("LiteralInt").Call(jen.Int64().Call(jen.Id("v")))),
						),
						jen.Default().Block(
							jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_int value must be a number, got %T"), jen.Id("ref").Dot("Value"))),
						),
					),
				),
				jen.Case(jen.Lit("literal_bool")).Block(
					jen.List(jen.Id("b"), jen.Id("ok")).Op(":=").Id("ref").Dot("Value").Assert(jen.Bool()),
					jen.If(jen.Op("!").Id("ok")).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("literal_bool value must be a boolean, got %T"), jen.Id("ref").Dot("Value"))),
					),
					jen.Return(jen.Id("tb").Dot("LiteralBool").Call(jen.Id("b"))),
				),
				jen.Case(jen.Lit("list")).Block(
					jen.If(jen.Id("ref").Dot("Items").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("list type requires 'items' field"))),
					),
					jen.List(jen.Id("inner"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Items"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("list items: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("List").Call(jen.Id("inner"))),
				),
				jen.Case(jen.Lit("optional")).Block(
					jen.If(jen.Id("ref").Dot("Inner").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("optional type requires 'inner' field"))),
					),
					jen.List(jen.Id("inner"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Inner"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("optional inner: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("Optional").Call(jen.Id("inner"))),
				),
				jen.Case(jen.Lit("map")).Block(
					jen.If(jen.Id("ref").Dot("Keys").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map type requires 'keys' field"))),
					),
					jen.If(jen.Id("ref").Dot("Values").Op("==").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map type requires 'values' field"))),
					),
					jen.List(jen.Id("keys"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Keys"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map keys: %w"), jen.Id("err"))),
					),
					jen.List(jen.Id("values"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("ref").Dot("Values"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
					jen.If(jen.Id("err").Op("!=").Nil()).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("map values: %w"), jen.Id("err"))),
					),
					jen.Return(jen.Id("tb").Dot("Map").Call(jen.Id("keys"), jen.Id("values"))),
				),
				jen.Case(jen.Lit("union")).Block(
					jen.If(jen.Len(jen.Id("ref").Dot("OneOf")).Op("==").Lit(0)).Block(
						jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("union type requires 'oneOf' field with at least one type"))),
					),
					jen.Id("types").Op(":=").Make(jen.Index().Add(typeAlias), jen.Lit(0), jen.Len(jen.Id("ref").Dot("OneOf"))),
					jen.For(jen.List(jen.Id("i"), jen.Id("item")).Op(":=").Range().Id("ref").Dot("OneOf")).Block(
						jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("resolveTypeRef").Call(jen.Id("tb"), jen.Id("item"), jen.Id("typeCache"), jen.Id("classBuilderCache")),
						jen.If(jen.Id("err").Op("!=").Nil()).Block(
							jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("union oneOf[%d]: %w"), jen.Id("i"), jen.Id("err"))),
						),
						jen.Id("types").Op("=").Append(jen.Id("types"), jen.Id("typ")),
					),
					jen.Return(jen.Id("tb").Dot("Union").Call(jen.Id("types"))),
				),
				jen.Case(jen.Lit("")).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("type field is required"))),
				),
				jen.Default().Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("unknown type: %q"), jen.Id("ref").Dot("Type"))),
				),
			),
		)

	// Generate resolveRef helper
	// Updated to also accept classBuilderCache for resolving newly created classes
	out.Func().Id("resolveRef").
		Params(
			jen.Id("tb").Op("*").Qual(introspectedPkg, "TypeBuilder"),
			jen.Id("name").String(),
			jen.Id("typeCache").Map(jen.String()).Add(typeAlias),
			jen.Id("classBuilderCache").Map(jen.String()).Qual(introspectedPkg, "DynamicClassBuilder"),
		).
		Params(typeAlias, jen.Error()).
		Block(
			// Check cache first (from dynamic_types)
			jen.If(jen.List(jen.Id("typ"), jen.Id("ok")).Op(":=").Id("typeCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Check if this is a newly created class - get type directly from builder
			jen.If(jen.List(jen.Id("cb"), jen.Id("ok")).Op(":=").Id("classBuilderCache").Index(jen.Id("name")), jen.Id("ok")).Block(
				jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Id("cb").Dot("Type").Call(),
				jen.If(jen.Id("err").Op("!=").Nil()).Block(
					jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("get type for class %q: %w"), jen.Id("name"), jen.Id("err"))),
				),
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Try to get existing class via introspected accessor
			jen.List(jen.Id("typ"), jen.Id("err")).Op(":=").Qual(introspectedPkg, "GetClassType").Call(jen.Id("tb"), jen.Id("name")),
			jen.If(jen.Id("err").Op("==").Nil().Op("&&").Id("typ").Op("!=").Nil()).Block(
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			// Try to get existing enum via introspected accessor
			jen.List(jen.Id("typ"), jen.Id("err")).Op("=").Qual(introspectedPkg, "GetEnumType").Call(jen.Id("tb"), jen.Id("name")),
			jen.If(jen.Id("err").Op("==").Nil().Op("&&").Id("typ").Op("!=").Nil()).Block(
				jen.Id("typeCache").Index(jen.Id("name")).Op("=").Id("typ"),
				jen.Return(jen.Id("typ"), jen.Nil()),
			),
			jen.Line(),
			jen.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("unresolved reference: %q"), jen.Id("name"))),
		)
}
