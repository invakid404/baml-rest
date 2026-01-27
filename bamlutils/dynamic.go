package bamlutils

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

// Dynamic endpoint constants
const (
	// DynamicMethodName is the internal BAML method name for dynamic prompts
	DynamicMethodName = "Baml_Rest_Dynamic"
	// DynamicEndpointName is the URL path segment for dynamic endpoints
	DynamicEndpointName = "_dynamic"
)

// CacheControl represents Anthropic prompt caching metadata
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// MessageMetadata contains optional metadata for a message.
// Currently only supports cache_control, but structured for future extensibility.
type MessageMetadata struct {
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	// Future metadata fields go here
}

// DynamicMessage represents a chat message with role, content, and optional metadata
type DynamicMessage struct {
	Role     string           `json:"role"`
	Content  string           `json:"content"`
	Metadata *MessageMetadata `json:"metadata,omitempty"`
}

// DynamicOutputSchema defines the output structure for dynamic endpoints.
// It supports both simple flat schemas and complex nested structures.
//
// Simple flat schema:
//
//	{
//	  "properties": {
//	    "name": {"type": "string"},
//	    "age": {"type": "int"}
//	  }
//	}
//
// Nested structures with classes and enums:
//
//	{
//	  "classes": {
//	    "Address": {
//	      "properties": {
//	        "street": {"type": "string"},
//	        "city": {"type": "string"}
//	      }
//	    }
//	  },
//	  "enums": {
//	    "Status": {
//	      "values": [{"name": "ACTIVE"}, {"name": "INACTIVE"}]
//	    }
//	  },
//	  "properties": {
//	    "name": {"type": "string"},
//	    "address": {"$ref": "Address"},
//	    "status": {"$ref": "Status"}
//	  }
//	}
type DynamicOutputSchema struct {
	// Properties defines the fields of the output object (required)
	Properties map[string]*DynamicProperty `json:"properties"`
	// Classes defines additional class types that can be referenced via $ref (optional)
	Classes map[string]*DynamicClass `json:"classes,omitempty"`
	// Enums defines enum types that can be referenced via $ref (optional)
	Enums map[string]*DynamicEnum `json:"enums,omitempty"`
}

// DynamicInput is the request body for dynamic endpoints
type DynamicInput struct {
	Messages       []DynamicMessage     `json:"messages"`
	ClientRegistry *ClientRegistry      `json:"client_registry"`
	OutputSchema   *DynamicOutputSchema `json:"output_schema"`
}

// Validate checks that required fields are present
func (d *DynamicInput) Validate() error {
	if len(d.Messages) == 0 {
		return fmt.Errorf("messages is required and cannot be empty")
	}
	if d.ClientRegistry == nil || d.ClientRegistry.Primary == nil {
		return fmt.Errorf("client_registry with primary is required")
	}
	if d.OutputSchema == nil || len(d.OutputSchema.Properties) == 0 {
		return fmt.Errorf("output_schema with at least one property is required")
	}
	for i, m := range d.Messages {
		if m.Role == "" {
			return fmt.Errorf("message[%d] role is required", i)
		}
		if m.Content == "" {
			return fmt.Errorf("message[%d] content is required", i)
		}
	}
	return nil
}

// ToWorkerInput converts to the internal format for worker processing
func (d *DynamicInput) ToWorkerInput() ([]byte, error) {
	// Build classes map: start with user-defined classes, then add the output class
	classes := make(map[string]*DynamicClass)
	for name, class := range d.OutputSchema.Classes {
		classes[name] = class
	}
	// Add the output class with the top-level properties
	classes["Baml_Rest_DynamicOutput"] = &DynamicClass{
		Properties: d.OutputSchema.Properties,
	}

	internal := map[string]any{
		"messages": d.Messages,
		"__baml_options__": &BamlOptions{
			ClientRegistry: d.ClientRegistry,
			TypeBuilder: &TypeBuilder{
				DynamicTypes: &DynamicTypes{
					Classes: classes,
					Enums:   d.OutputSchema.Enums,
				},
			},
		},
	}
	return json.Marshal(internal)
}

// DynamicParseInput is the request body for dynamic parse endpoint
type DynamicParseInput struct {
	Raw          string               `json:"raw"`
	OutputSchema *DynamicOutputSchema `json:"output_schema"`
}

// Validate checks that required fields are present for parse
func (d *DynamicParseInput) Validate() error {
	if d.Raw == "" {
		return fmt.Errorf("raw is required and cannot be empty")
	}
	if d.OutputSchema == nil || len(d.OutputSchema.Properties) == 0 {
		return fmt.Errorf("output_schema with at least one property is required")
	}
	return nil
}

// ToWorkerInput converts to the internal format for worker processing
func (d *DynamicParseInput) ToWorkerInput() ([]byte, error) {
	// Build classes map: start with user-defined classes, then add the output class
	classes := make(map[string]*DynamicClass)
	for name, class := range d.OutputSchema.Classes {
		classes[name] = class
	}
	// Add the output class with the top-level properties
	classes["Baml_Rest_DynamicOutput"] = &DynamicClass{
		Properties: d.OutputSchema.Properties,
	}

	internal := map[string]any{
		"raw": d.Raw,
		"__baml_options__": &BamlOptions{
			TypeBuilder: &TypeBuilder{
				DynamicTypes: &DynamicTypes{
					Classes: classes,
					Enums:   d.OutputSchema.Enums,
				},
			},
		},
	}
	return json.Marshal(internal)
}

// FlattenDynamicOutput extracts the DynamicProperties field from a dynamic endpoint response.
//
// Input:  {"DynamicProperties": {"name": "John", "age": 30}}
// Output: {"name": "John", "age": 30}
//
// If the input doesn't have a DynamicProperties field, it's returned as-is.
func FlattenDynamicOutput(data []byte) ([]byte, error) {
	dynProps := gjson.GetBytes(data, "DynamicProperties")
	if !dynProps.Exists() {
		return data, nil
	}
	return []byte(dynProps.Raw), nil
}
