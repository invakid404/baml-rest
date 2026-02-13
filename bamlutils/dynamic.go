package bamlutils

import (
	"fmt"

	"github.com/goccy/go-json"
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

// DynamicContentPart represents a single part within a multi-part message.
// Used in the user-facing API where content is an array of typed parts.
//
// Each part has a "type" discriminator and a corresponding payload field:
//   - {"type": "text", "text": "..."}
//   - {"type": "image", "image": {"url": "...", "media_type": "..."}}
//   - {"type": "audio", "audio": {"base64": "...", "media_type": "..."}}
//   - {"type": "pdf", "pdf": {"base64": "...", "media_type": "..."}}
//   - {"type": "video", "video": {"url": "...", "media_type": "..."}}
//   - {"type": "output_format"}
type DynamicContentPart struct {
	Type  string      `json:"type"`
	Text  *string     `json:"text,omitempty"`
	Image *MediaInput `json:"image,omitempty"`
	Audio *MediaInput `json:"audio,omitempty"`
	PDF   *MediaInput `json:"pdf,omitempty"`
	Video *MediaInput `json:"video,omitempty"`
}

// Validate checks that the content part has a valid type and the required payload.
func (p *DynamicContentPart) Validate(msgIdx, partIdx int) error {
	switch p.Type {
	case "text":
		if p.Text == nil {
			return fmt.Errorf("message[%d].content[%d]: text part requires 'text' field", msgIdx, partIdx)
		}
	case "image":
		if p.Image == nil {
			return fmt.Errorf("message[%d].content[%d]: image part requires 'image' field", msgIdx, partIdx)
		}
		if err := p.Image.Validate(); err != nil {
			return fmt.Errorf("message[%d].content[%d]: image %w", msgIdx, partIdx, err)
		}
	case "audio":
		if p.Audio == nil {
			return fmt.Errorf("message[%d].content[%d]: audio part requires 'audio' field", msgIdx, partIdx)
		}
		if err := p.Audio.Validate(); err != nil {
			return fmt.Errorf("message[%d].content[%d]: audio %w", msgIdx, partIdx, err)
		}
	case "pdf":
		if p.PDF == nil {
			return fmt.Errorf("message[%d].content[%d]: pdf part requires 'pdf' field", msgIdx, partIdx)
		}
		if err := p.PDF.Validate(); err != nil {
			return fmt.Errorf("message[%d].content[%d]: pdf %w", msgIdx, partIdx, err)
		}
	case "video":
		if p.Video == nil {
			return fmt.Errorf("message[%d].content[%d]: video part requires 'video' field", msgIdx, partIdx)
		}
		if err := p.Video.Validate(); err != nil {
			return fmt.Errorf("message[%d].content[%d]: video %w", msgIdx, partIdx, err)
		}
	case "output_format":
		// No payload needed
	default:
		return fmt.Errorf("message[%d].content[%d]: unknown content part type %q", msgIdx, partIdx, p.Type)
	}
	return nil
}

// DynamicMessage represents a chat message with role, content, and optional metadata.
//
// Content can be either:
//   - A plain string (backward compatible): {"role": "user", "content": "hello"}
//   - An array of content parts (multi-part with media): {"role": "user", "content": [...parts]}
//
// When content is a string, the legacy {output_format} placeholder replacement is supported.
// When content is a parts array, use {"type": "output_format"} as a part instead.
type DynamicMessage struct {
	Role     string           `json:"role"`
	Metadata *MessageMetadata `json:"metadata,omitempty"`

	// Parsed content - exactly one of these will be set after unmarshaling
	TextContent  *string              // Set when content is a plain string
	PartsContent []DynamicContentPart // Set when content is an array of parts
}

// UnmarshalJSON implements custom JSON unmarshaling to handle the content union type.
func (m *DynamicMessage) UnmarshalJSON(data []byte) error {
	// Use a raw type to avoid infinite recursion
	var raw struct {
		Role     string           `json:"role"`
		Content  json.RawMessage  `json:"content"`
		Metadata *MessageMetadata `json:"metadata,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	m.Role = raw.Role
	m.Metadata = raw.Metadata

	if len(raw.Content) == 0 {
		return nil
	}

	// Determine if content is a string or array
	firstByte := raw.Content[0]
	switch firstByte {
	case '"':
		// String content
		var s string
		if err := json.Unmarshal(raw.Content, &s); err != nil {
			return fmt.Errorf("invalid string content: %w", err)
		}
		m.TextContent = &s
	case '[':
		// Array of content parts
		var parts []DynamicContentPart
		if err := json.Unmarshal(raw.Content, &parts); err != nil {
			return fmt.Errorf("invalid content parts array: %w", err)
		}
		m.PartsContent = parts
	default:
		return fmt.Errorf("content must be a string or array, got %q", string(raw.Content))
	}

	return nil
}

// MarshalJSON implements custom JSON marshaling.
func (m *DynamicMessage) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role     string           `json:"role"`
		Content  any              `json:"content,omitempty"`
		Parts    any              `json:"parts,omitempty"`
		Metadata *MessageMetadata `json:"metadata,omitempty"`
	}

	a := alias{
		Role:     m.Role,
		Metadata: m.Metadata,
	}

	if m.PartsContent != nil {
		a.Content = m.PartsContent
	} else if m.TextContent != nil {
		a.Content = *m.TextContent
	}

	return json.Marshal(a)
}

// internalContentPart is the BAML-facing format for content parts.
// This maps to the Baml_Rest_ContentPart class in dynamic.baml.
// Field names use shortened forms (img, aud, doc, vid) to avoid
// clashing with BAML type keywords (image, audio, pdf, video).
// Media fields are serialized as MediaInput objects which the codegen
// mirror struct mechanism will convert to BAML media types.
type internalContentPart struct {
	Text         *string     `json:"text,omitempty"`
	Img          *MediaInput `json:"img,omitempty"`
	Aud          *MediaInput `json:"aud,omitempty"`
	Doc          *MediaInput `json:"doc,omitempty"`
	Vid          *MediaInput `json:"vid,omitempty"`
	OutputFormat *bool       `json:"output_format,omitempty"`
}

// internalMessage is the BAML-facing format for messages.
// This maps to the Baml_Rest_Message class in dynamic.baml.
type internalMessage struct {
	Role     string                `json:"role"`
	Content  *string               `json:"content,omitempty"`
	Parts    []internalContentPart `json:"parts,omitempty"`
	Metadata *MessageMetadata      `json:"metadata,omitempty"`
}

// toInternalMessage converts a user-facing DynamicMessage to the internal format.
func (m *DynamicMessage) toInternalMessage() internalMessage {
	msg := internalMessage{
		Role:     m.Role,
		Metadata: m.Metadata,
	}

	if m.PartsContent != nil {
		parts := make([]internalContentPart, 0, len(m.PartsContent))
		for _, p := range m.PartsContent {
			part := internalContentPart{}
			switch p.Type {
			case "text":
				part.Text = p.Text
			case "image":
				part.Img = p.Image
			case "audio":
				part.Aud = p.Audio
			case "pdf":
				part.Doc = p.PDF
			case "video":
				part.Vid = p.Video
			case "output_format":
				t := true
				part.OutputFormat = &t
			}
			parts = append(parts, part)
		}
		msg.Parts = parts
	} else if m.TextContent != nil {
		msg.Content = m.TextContent
	}

	return msg
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
//	    "address": {"ref": "Address"},
//	    "status": {"ref": "Status"}
//	  }
//	}
type DynamicOutputSchema struct {
	// Properties defines the fields of the output object (required)
	Properties map[string]*DynamicProperty `json:"properties"`
	// Classes defines additional class types that can be referenced via ref (optional)
	Classes map[string]*DynamicClass `json:"classes,omitempty"`
	// Enums defines enum types that can be referenced via ref (optional)
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
		if m.TextContent == nil && m.PartsContent == nil {
			return fmt.Errorf("message[%d] content is required", i)
		}
		if m.TextContent != nil && *m.TextContent == "" {
			return fmt.Errorf("message[%d] content is required", i)
		}
		if m.PartsContent != nil {
			if len(m.PartsContent) == 0 {
				return fmt.Errorf("message[%d] content parts array cannot be empty", i)
			}
			for j, p := range m.PartsContent {
				if err := p.Validate(i, j); err != nil {
					return err
				}
			}
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

	// Convert messages to internal format
	internalMessages := make([]internalMessage, 0, len(d.Messages))
	for _, m := range d.Messages {
		internalMessages = append(internalMessages, m.toInternalMessage())
	}

	internal := map[string]any{
		"messages": internalMessages,
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
