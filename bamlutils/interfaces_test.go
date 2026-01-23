package bamlutils

import (
	"strings"
	"testing"
)

func TestDynamicTypes_Validate(t *testing.T) {
	tests := []struct {
		name    string
		dt      *DynamicTypes
		wantErr string // empty means no error expected
	}{
		{
			name:    "nil DynamicTypes is valid",
			dt:      nil,
			wantErr: "",
		},
		{
			name:    "empty DynamicTypes is valid",
			dt:      &DynamicTypes{},
			wantErr: "",
		},
		{
			name: "empty class is valid",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"EmptyClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "empty enum is valid",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"EmptyEnum": {},
				},
			},
			wantErr: "",
		},
		{
			name: "nil class definition",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"NilClass": nil,
				},
			},
			wantErr: `class "NilClass": definition is nil`,
		},
		{
			name: "nil enum definition",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"NilEnum": nil,
				},
			},
			wantErr: `enum "NilEnum": definition is nil`,
		},
		{
			name: "nil property",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"nilProp": nil,
						},
					},
				},
			},
			wantErr: `class "TestClass": property "nilProp" is nil`,
		},
		{
			name: "nil enum value",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"TestEnum": {
						Values: []*DynamicEnumValue{nil},
					},
				},
			},
			wantErr: `enum "TestEnum": value at index 0 is nil`,
		},
		{
			name: "enum value with empty name",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"TestEnum": {
						Values: []*DynamicEnumValue{
							{Name: ""},
						},
					},
				},
			},
			wantErr: `enum "TestEnum": value at index 0 has empty name`,
		},
		{
			name: "property missing type and ref",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {},
						},
					},
				},
			},
			wantErr: `must have 'type' or '$ref'`,
		},
		{
			name: "property with both type and ref",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {
								Type: "string",
								Ref:  "OtherClass",
							},
						},
					},
				},
			},
			wantErr: `cannot have both 'type' and '$ref'`,
		},
		{
			name: "valid primitive types",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"strProp":   {Type: "string"},
							"intProp":   {Type: "int"},
							"floatProp": {Type: "float"},
							"boolProp":  {Type: "bool"},
							"nullProp":  {Type: "null"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "unknown type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {Type: "unknown_type"},
						},
					},
				},
			},
			wantErr: `unknown type "unknown_type"`,
		},
		{
			name: "list without items",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"listProp": {Type: "list"},
						},
					},
				},
			},
			wantErr: `'list' type requires 'items'`,
		},
		{
			name: "valid list",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"listProp": {
								Type:  "list",
								Items: &DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "optional without inner",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"optProp": {Type: "optional"},
						},
					},
				},
			},
			wantErr: `'optional' type requires 'inner'`,
		},
		{
			name: "valid optional",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"optProp": {
								Type:  "optional",
								Inner: &DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "map without keys",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type:   "map",
								Values: &DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: `'map' type requires 'keys' and 'values'`,
		},
		{
			name: "map without values",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type: "map",
								Keys: &DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: `'map' type requires 'keys' and 'values'`,
		},
		{
			name: "valid map",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type:   "map",
								Keys:   &DynamicTypeRef{Type: "string"},
								Values: &DynamicTypeRef{Type: "int"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "union without oneOf",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {Type: "union"},
						},
					},
				},
			},
			wantErr: `'union' type requires 'oneOf' with at least one type`,
		},
		{
			name: "union with nil variant",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {
								Type:  "union",
								OneOf: []*DynamicTypeRef{nil},
							},
						},
					},
				},
			},
			wantErr: `'oneOf[0]' is nil`,
		},
		{
			name: "valid union",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {
								Type: "union",
								OneOf: []*DynamicTypeRef{
									{Type: "string"},
									{Type: "int"},
								},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_string without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string"},
						},
					},
				},
			},
			wantErr: `'literal_string' type requires 'value'`,
		},
		{
			name: "literal_string with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string", Value: 123},
						},
					},
				},
			},
			wantErr: `'literal_string' value must be a string`,
		},
		{
			name: "valid literal_string",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string", Value: "hello"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_int without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int"},
						},
					},
				},
			},
			wantErr: `'literal_int' type requires 'value'`,
		},
		{
			name: "literal_int with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: "not an int"},
						},
					},
				},
			},
			wantErr: `'literal_int' value must be an integer`,
		},
		{
			name: "valid literal_int with int",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: 42},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "valid literal_int with float64 (JSON unmarshaling)",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: float64(42)},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_bool without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool"},
						},
					},
				},
			},
			wantErr: `'literal_bool' type requires 'value'`,
		},
		{
			name: "literal_bool with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool", Value: "true"},
						},
					},
				},
			},
			wantErr: `'literal_bool' value must be a boolean`,
		},
		{
			name: "valid literal_bool",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool", Value: true},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "valid $ref to class",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "OtherClass"},
						},
					},
					"OtherClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "valid $ref to enum",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "Status"},
						},
					},
				},
				Enums: map[string]*DynamicEnum{
					"Status": {
						Values: []*DynamicEnumValue{
							{Name: "ACTIVE"},
							{Name: "INACTIVE"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "$ref to undefined type is allowed (may exist in baml_src)",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "ExternalType"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "deeply nested valid types",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"deepProp": {
								Type: "optional",
								Inner: &DynamicTypeRef{
									Type: "list",
									Items: &DynamicTypeRef{
										Type: "map",
										Keys: &DynamicTypeRef{Type: "string"},
										Values: &DynamicTypeRef{
											Type: "union",
											OneOf: []*DynamicTypeRef{
												{Type: "string"},
												{Type: "int"},
												{Ref: "OtherClass"},
											},
										},
									},
								},
							},
						},
					},
					"OtherClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "nested type with error",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"deepProp": {
								Type: "list",
								Items: &DynamicTypeRef{
									Type: "optional",
									// Missing Inner
								},
							},
						},
					},
				},
			},
			wantErr: `'optional' type requires 'inner'`,
		},
		{
			name: "enum with all features",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"Status": {
						Description: "Status enum",
						Alias:       "UserStatus",
						Values: []*DynamicEnumValue{
							{Name: "ACTIVE", Description: "Active user", Alias: "active"},
							{Name: "INACTIVE", Skip: true},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "class with all features",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"User": {
						Description: "User class",
						Alias:       "UserModel",
						Properties: map[string]*DynamicProperty{
							"name": {Type: "string", Description: "User name", Alias: "userName"},
							"age":  {Type: "int"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "self-referential class",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TreeNode": {
						Properties: map[string]*DynamicProperty{
							"value": {Type: "string"},
							"children": {
								Type: "list",
								Items: &DynamicTypeRef{
									Ref: "TreeNode",
								},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "mutually referential classes",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"ClassA": {
						Properties: map[string]*DynamicProperty{
							"b": {Ref: "ClassB"},
						},
					},
					"ClassB": {
						Properties: map[string]*DynamicProperty{
							"a": {Ref: "ClassA"},
						},
					},
				},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dt.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Validate() error = %q, want error containing %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestDynamicTypes_Validate_MaxDepth(t *testing.T) {
	// Build a deeply nested type that exceeds maxTypeDepth
	var buildDeepType func(depth int) *DynamicTypeRef
	buildDeepType = func(depth int) *DynamicTypeRef {
		if depth <= 0 {
			return &DynamicTypeRef{Type: "string"}
		}
		return &DynamicTypeRef{
			Type:  "optional",
			Inner: buildDeepType(depth - 1),
		}
	}

	t.Run("type at max depth is valid", func(t *testing.T) {
		dt := &DynamicTypes{
			Classes: map[string]*DynamicClass{
				"TestClass": {
					Properties: map[string]*DynamicProperty{
						"deepProp": {
							Type:  "optional",
							Inner: buildDeepType(maxTypeDepth - 2), // -2 because: property itself is depth 0, outer optional is depth 1
						},
					},
				},
			},
		}
		if err := dt.Validate(); err != nil {
			t.Errorf("Validate() at max depth should not error, got: %v", err)
		}
	})

	t.Run("type exceeding max depth returns error", func(t *testing.T) {
		dt := &DynamicTypes{
			Classes: map[string]*DynamicClass{
				"TestClass": {
					Properties: map[string]*DynamicProperty{
						"tooDeepProp": {
							Type:  "optional",
							Inner: buildDeepType(maxTypeDepth + 10),
						},
					},
				},
			},
		}
		err := dt.Validate()
		if err == nil {
			t.Error("Validate() expected error for exceeding max depth, got nil")
		} else if !strings.Contains(err.Error(), "exceeds maximum depth") {
			t.Errorf("Validate() error = %q, want error about exceeding max depth", err.Error())
		}
	})
}
