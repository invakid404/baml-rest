package bamlutils

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

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

// payloadFieldNames returns the names of all payload fields that are set (non-nil).
func (p *DynamicContentPart) payloadFieldNames() []string {
	var names []string
	if p.Text != nil {
		names = append(names, "text")
	}
	if p.Image != nil {
		names = append(names, "image")
	}
	if p.Audio != nil {
		names = append(names, "audio")
	}
	if p.PDF != nil {
		names = append(names, "pdf")
	}
	if p.Video != nil {
		names = append(names, "video")
	}
	return names
}

// Validate checks that the content part has a valid type, the required payload,
// and no extraneous payload fields. Exactly one payload field must be set and it
// must match the declared type (output_format must have zero payload fields).
func (p *DynamicContentPart) Validate(msgIdx, partIdx int) error {
	prefix := fmt.Sprintf("message[%d].content[%d]", msgIdx, partIdx)
	set := p.payloadFieldNames()

	switch p.Type {
	case "text":
		if p.Text == nil {
			return fmt.Errorf("%s: text part requires 'text' field", prefix)
		}
	case "image":
		if p.Image == nil {
			return fmt.Errorf("%s: image part requires 'image' field", prefix)
		}
		if err := p.Image.Validate(); err != nil {
			return fmt.Errorf("%s: image %w", prefix, err)
		}
	case "audio":
		if p.Audio == nil {
			return fmt.Errorf("%s: audio part requires 'audio' field", prefix)
		}
		if err := p.Audio.Validate(); err != nil {
			return fmt.Errorf("%s: audio %w", prefix, err)
		}
	case "pdf":
		if p.PDF == nil {
			return fmt.Errorf("%s: pdf part requires 'pdf' field", prefix)
		}
		if err := p.PDF.Validate(); err != nil {
			return fmt.Errorf("%s: pdf %w", prefix, err)
		}
	case "video":
		if p.Video == nil {
			return fmt.Errorf("%s: video part requires 'video' field", prefix)
		}
		if err := p.Video.Validate(); err != nil {
			return fmt.Errorf("%s: video %w", prefix, err)
		}
	case "output_format":
		if len(set) > 0 {
			return fmt.Errorf("%s: output_format part must not have payload fields, got %v", prefix, set)
		}
	default:
		return fmt.Errorf("%s: unknown content part type %q", prefix, p.Type)
	}

	// For non-output_format types: exactly one payload field must be set and match the type
	if p.Type != "output_format" {
		if len(set) > 1 {
			return fmt.Errorf("%s: %s part must have only its own payload field, got %v", prefix, p.Type, set)
		}
		// len(set)==1 is guaranteed here since the switch above verified the matching field is non-nil,
		// but verify the single field matches the type (e.g. reject type:"text" with only "image" set —
		// impossible via JSON since the switch already checked, but guards against future refactors).
		if set[0] != p.Type {
			return fmt.Errorf("%s: %s part has mismatched payload field %q", prefix, p.Type, set[0])
		}
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
	tb, err := buildDynamicTypeBuilder(d.OutputSchema)
	if err != nil {
		return nil, err
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
			TypeBuilder:    tb,
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
	tb, err := buildDynamicTypeBuilder(d.OutputSchema)
	if err != nil {
		return nil, err
	}

	internal := map[string]any{
		"raw": d.Raw,
		"__baml_options__": &BamlOptions{
			TypeBuilder: tb,
		},
	}
	return json.Marshal(internal)
}

// buildDynamicTypeBuilder converts a DynamicOutputSchema into a TypeBuilder for
// the worker. When the user's classes contain a self-referential cycle, the
// classes (and the dynamic output entry point) are emitted as BAML source via
// BamlSnippets instead of the imperative DynamicTypes API. This works around a
// BAML runtime segfault that occurs when ctx.output_format renders a dynamic
// class with cycles built through the imperative TypeBuilder. Statically
// defined recursive classes — and the same shape declared through tb.AddBaml —
// render correctly, so we feed the cyclic case through that path. Enums always
// stay in DynamicTypes; they're applied first by the codegen so snippets can
// reference them by name.
func buildDynamicTypeBuilder(schema *DynamicOutputSchema) (*TypeBuilder, error) {
	if hasClassCycles(schema.Classes) {
		snippet, err := dynamicSchemaToBamlSource(schema)
		if err != nil {
			return nil, fmt.Errorf("failed to convert recursive dynamic schema to BAML source: %w", err)
		}
		return &TypeBuilder{
			BamlSnippets: []string{snippet},
			DynamicTypes: &DynamicTypes{
				Enums: schema.Enums,
			},
		}, nil
	}

	classes := make(map[string]*DynamicClass, len(schema.Classes)+1)
	for name, class := range schema.Classes {
		classes[name] = class
	}
	classes["Baml_Rest_DynamicOutput"] = &DynamicClass{
		Properties: schema.Properties,
	}
	return &TypeBuilder{
		DynamicTypes: &DynamicTypes{
			Classes: classes,
			Enums:   schema.Enums,
		},
	}, nil
}

// hasClassCycles returns true if any user-defined dynamic class transitively
// references itself through its property types.
func hasClassCycles(classes map[string]*DynamicClass) bool {
	for start, cls := range classes {
		if cls == nil {
			continue
		}
		toVisit := classDirectRefs(cls)
		visited := make(map[string]bool, len(classes))
		for len(toVisit) > 0 {
			cur := toVisit[len(toVisit)-1]
			toVisit = toVisit[:len(toVisit)-1]
			if cur == start {
				return true
			}
			if visited[cur] {
				continue
			}
			visited[cur] = true
			if c, ok := classes[cur]; ok {
				toVisit = append(toVisit, classDirectRefs(c)...)
			}
		}
	}
	return false
}

func classDirectRefs(cls *DynamicClass) []string {
	if cls == nil {
		return nil
	}
	var refs []string
	for _, prop := range cls.Properties {
		if prop == nil {
			continue
		}
		if prop.Ref != "" {
			refs = append(refs, prop.Ref)
		}
		refs = appendTypeSpecRefs(refs, prop.Items)
		refs = appendTypeSpecRefs(refs, prop.Inner)
		refs = appendTypeSpecRefs(refs, prop.Keys)
		refs = appendTypeSpecRefs(refs, prop.Values)
		for _, t := range prop.OneOf {
			refs = appendTypeSpecRefs(refs, t)
		}
	}
	return refs
}

func appendTypeSpecRefs(refs []string, t *DynamicTypeSpec) []string {
	if t == nil {
		return refs
	}
	if t.Ref != "" {
		refs = append(refs, t.Ref)
	}
	refs = appendTypeSpecRefs(refs, t.Items)
	refs = appendTypeSpecRefs(refs, t.Inner)
	refs = appendTypeSpecRefs(refs, t.Keys)
	refs = appendTypeSpecRefs(refs, t.Values)
	for _, sub := range t.OneOf {
		refs = appendTypeSpecRefs(refs, sub)
	}
	return refs
}

// dynamicSchemaToBamlSource emits a BAML source snippet that defines all
// user-supplied classes plus a `dynamic class Baml_Rest_DynamicOutput { ... }`
// block adding the schema's top-level properties.
func dynamicSchemaToBamlSource(schema *DynamicOutputSchema) (string, error) {
	var sb strings.Builder

	classNames := make([]string, 0, len(schema.Classes))
	for name := range schema.Classes {
		classNames = append(classNames, name)
	}
	sort.Strings(classNames)
	for _, name := range classNames {
		cls := schema.Classes[name]
		if cls == nil {
			continue
		}
		if err := writeBamlClass(&sb, "class "+name, cls.Properties); err != nil {
			return "", fmt.Errorf("class %q: %w", name, err)
		}
	}

	if err := writeBamlClass(&sb, "dynamic class Baml_Rest_DynamicOutput", schema.Properties); err != nil {
		return "", fmt.Errorf("dynamic output: %w", err)
	}

	return sb.String(), nil
}

func writeBamlClass(sb *strings.Builder, header string, props map[string]*DynamicProperty) error {
	sb.WriteString(header)
	sb.WriteString(" {\n")
	propNames := make([]string, 0, len(props))
	for n := range props {
		propNames = append(propNames, n)
	}
	sort.Strings(propNames)
	for _, n := range propNames {
		prop := props[n]
		if prop == nil {
			continue
		}
		typStr, err := propertyToBamlType(prop)
		if err != nil {
			return fmt.Errorf("property %q: %w", n, err)
		}
		sb.WriteString("  ")
		sb.WriteString(n)
		sb.WriteString(" ")
		sb.WriteString(typStr)
		sb.WriteString("\n")
	}
	sb.WriteString("}\n\n")
	return nil
}

func propertyToBamlType(prop *DynamicProperty) (string, error) {
	if prop.Ref != "" {
		return prop.Ref, nil
	}
	return typeSpecToBamlType(&DynamicTypeSpec{
		Type:   prop.Type,
		Items:  prop.Items,
		Inner:  prop.Inner,
		OneOf:  prop.OneOf,
		Keys:   prop.Keys,
		Values: prop.Values,
		Value:  prop.Value,
	})
}

func typeSpecToBamlType(t *DynamicTypeSpec) (string, error) {
	if t == nil {
		return "", fmt.Errorf("nil type spec")
	}
	if t.Ref != "" {
		return t.Ref, nil
	}
	switch t.Type {
	case "string", "int", "float", "bool", "null":
		return t.Type, nil
	case "list":
		if t.Items == nil {
			return "", fmt.Errorf("list requires 'items'")
		}
		inner, err := typeSpecToBamlType(t.Items)
		if err != nil {
			return "", fmt.Errorf("list items: %w", err)
		}
		if needsParens(t.Items) {
			inner = "(" + inner + ")"
		}
		return inner + "[]", nil
	case "optional":
		if t.Inner == nil {
			return "", fmt.Errorf("optional requires 'inner'")
		}
		inner, err := typeSpecToBamlType(t.Inner)
		if err != nil {
			return "", fmt.Errorf("optional inner: %w", err)
		}
		if needsParens(t.Inner) {
			inner = "(" + inner + ")"
		}
		return inner + "?", nil
	case "map":
		if t.Keys == nil || t.Values == nil {
			return "", fmt.Errorf("map requires 'keys' and 'values'")
		}
		k, err := typeSpecToBamlType(t.Keys)
		if err != nil {
			return "", fmt.Errorf("map keys: %w", err)
		}
		v, err := typeSpecToBamlType(t.Values)
		if err != nil {
			return "", fmt.Errorf("map values: %w", err)
		}
		return "map<" + k + ", " + v + ">", nil
	case "union":
		if len(t.OneOf) == 0 {
			return "", fmt.Errorf("union requires 'oneOf' with at least one type")
		}
		parts := make([]string, len(t.OneOf))
		for i, v := range t.OneOf {
			s, err := typeSpecToBamlType(v)
			if err != nil {
				return "", fmt.Errorf("union[%d]: %w", i, err)
			}
			parts[i] = s
		}
		return strings.Join(parts, " | "), nil
	case "literal_string":
		s, ok := t.Value.(string)
		if !ok {
			return "", fmt.Errorf("literal_string value must be a string, got %T", t.Value)
		}
		return strconv.Quote(s), nil
	case "literal_int":
		switch v := t.Value.(type) {
		case float64:
			return strconv.FormatInt(int64(v), 10), nil
		case int:
			return strconv.Itoa(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		default:
			return "", fmt.Errorf("literal_int value must be a number, got %T", v)
		}
	case "literal_bool":
		b, ok := t.Value.(bool)
		if !ok {
			return "", fmt.Errorf("literal_bool value must be a bool, got %T", t.Value)
		}
		if b {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("unknown type %q", t.Type)
	}
}

// needsParens reports whether a sub-type must be parenthesized when used as
// the inner of a list or optional, to disambiguate against BAML's parse rules
// (e.g. `string | int[]` would otherwise mean `string | (int[])`).
func needsParens(t *DynamicTypeSpec) bool {
	if t == nil {
		return false
	}
	switch t.Type {
	case "union", "optional":
		return true
	}
	return false
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
